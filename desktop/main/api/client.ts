// Main's request executor: the one place a request reaches the network
// (linux-desktop.md §3: "Main is a native public-API client"). Input: an
// operation of the table, its target and body, and a Capture of the identity
// that asked. Output: what the server answered, validated, or the typed reason
// there was no answer.
//
// What it does, and the only things it does:
//  - the URL is the origin plus the target of targetOf; no caller supplies a
//    URL, a header or a method;
//  - Authorization: Bearer is sent for a bearer operation and for no other; no
//    Origin and no Cookie is ever sent; no proxy is used (Node's fetch does
//    not read one);
//  - a redirect is never followed, on any operation: it is a typed failure, and
//    the credential is not sent to the target;
//  - TLS is never relaxed: a certificate this machine does not trust is the
//    typed failure 'certificate';
//  - the answer is read up to a cap, parsed, and validated by the operation's
//    validator (a refusal by the error document's) before anything returns;
//  - the answer is dropped when the identity that asked has ended.
import { errorV0 } from '../../../web/src/schema/error.ts';
import { operations, type Operation, type OperationId } from '../../../web/src/platform/operations.ts';
import type { NoAnswerReason } from '../../../web/src/platform/desktop/bridge.ts';
import type { Capture, Session } from './session.ts';
import { targetOf } from './target.ts';
import { isCertificateFailure } from './trust.ts';

export const MAX_RESPONSE_BYTES = 4 * 1024 * 1024;
export const REQUEST_TIMEOUT_MS = 30_000;

export interface ClientDeps {
  readonly fetch: typeof globalThis.fetch;
  readonly session: Session;
  readonly maxResponseBytes?: number;
  readonly timeoutMs?: number;
}

export type Exchange =
  | {
      readonly kind: 'answer';
      readonly status: number;
      readonly ok: boolean;
      readonly body?: unknown;
      // A download's bytes (Send.download), on a 2xx answer only.
      readonly bytes?: Uint8Array;
      readonly retryAfter?: number;
      readonly requestId?: string;
    }
  | { readonly kind: 'none'; readonly reason?: NoAnswerReason }
  | { readonly kind: 'unauthorized' };

export interface Send {
  readonly op: OperationId;
  readonly params?: Readonly<Record<string, unknown>>;
  readonly query?: string;
  readonly body?: unknown;
  // Overrides the captured credential (the one that signs in sends none, and
  // the check of a new credential sends its own, before it is kept).
  readonly token?: string;
  // The capture to use; by default the identity in force now.
  readonly capture?: Capture;
  // True for the one request that must outlive its identity: the revocation
  // that sign-out sends for the credential it has just forgotten.
  readonly detached?: boolean;
  // The request body is these bytes with this declared media type, not JSON
  // (an upload). The type is untrusted data the caller has already shaped.
  readonly upload?: { readonly mediaType: string; readonly bytes: Uint8Array };
  // The answer is bytes, up to maxBytes, not a JSON document (a download).
  readonly download?: { readonly maxBytes: number };
  readonly timeoutMs?: number;
}

const none = (reason?: NoAnswerReason): Exchange => (reason === undefined ? { kind: 'none' } : { kind: 'none', reason });

// readCapped reads a response body up to the cap, or gives null when it is
// larger.
async function readCapped(response: Response, cap: number): Promise<Uint8Array | null> {
  const declared = Number(response.headers.get('content-length'));
  if (Number.isFinite(declared) && declared > cap) {
    await response.body?.cancel();
    return null;
  }
  if (response.body === null) return new Uint8Array();
  const reader: ReadableStreamDefaultReader<Uint8Array> = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > cap) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const bytes = new Uint8Array(total);
  let at = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, at);
    at += chunk.byteLength;
  }
  return bytes;
}

function retryAfterOf(response: Response): number | undefined {
  const seconds = Number.parseInt(response.headers.get('retry-after') ?? '', 10);
  return Number.isFinite(seconds) && seconds >= 0 && seconds < 86_400 ? seconds : undefined;
}

function requestIdOf(response: Response): string | undefined {
  const id = response.headers.get('x-request-id');
  return id !== null && /^[\w.-]{1,128}$/u.test(id) ? id : undefined;
}

function parseJson(bytes: Uint8Array): { readonly ok: true; readonly value: unknown } | { readonly ok: false } {
  try {
    return { ok: true, value: JSON.parse(new TextDecoder('utf-8', { fatal: true }).decode(bytes)) as unknown };
  } catch {
    return { ok: false };
  }
}

export class Client {
  readonly #deps: ClientDeps;

  constructor(deps: ClientDeps) {
    this.#deps = deps;
  }

  async send(request: Send): Promise<Exchange> {
    const { session } = this.#deps;
    const capture = request.capture ?? session.capture();
    const op: Operation = operations[request.op];
    if (op.socket === true || capture.origin === null) return none(capture.origin === null ? 'no-server' : 'unsupported');
    const target = targetOf(request.op, request.params, request.query);
    if (target === null) return none('unsupported');

    const headers = new Headers({ Accept: 'application/json' });
    if (op.auth === 'bearer') {
      const token = request.token ?? capture.token;
      // No credential is a 401 without asking the server.
      if (token === null) return { kind: 'unauthorized' };
      headers.set('Authorization', `Bearer ${token}`);
    }
    let body: string | Uint8Array | undefined;
    if (request.upload !== undefined) {
      body = request.upload.bytes;
      headers.set('Content-Type', request.upload.mediaType);
    } else if (request.body !== undefined) {
      body = JSON.stringify(request.body);
      headers.set('Content-Type', 'application/json');
    }

    const stale = (): boolean => request.detached !== true && !session.current(capture);
    let response: Response;
    try {
      response = await this.#deps.fetch(`${capture.origin}${target}`, {
        method: op.method,
        headers,
        ...(body === undefined ? {} : { body: body as RequestInit['body'] }),
        redirect: 'manual',
        cache: 'no-store',
        signal: AbortSignal.any([capture.signal, AbortSignal.timeout(request.timeoutMs ?? this.#deps.timeoutMs ?? REQUEST_TIMEOUT_MS)]),
      });
    } catch (error) {
      if (stale()) return none('stale');
      return none(isCertificateFailure(error) ? 'certificate' : undefined);
    }

    // A redirect is a failure on every operation: the target it names is
    // never asked, with or without a credential.
    if (response.type === 'opaqueredirect' || (response.status >= 300 && response.status < 400)) {
      await response.body?.cancel().catch(() => undefined);
      return none('redirect');
    }

    let bytes: Uint8Array | null;
    try {
      bytes = await readCapped(response, request.download?.maxBytes ?? this.#deps.maxResponseBytes ?? MAX_RESPONSE_BYTES);
    } catch {
      return none(stale() ? 'stale' : undefined);
    }
    // The identity that asked has ended: whatever came back is not for the
    // identity in force.
    if (stale()) return none('stale');
    if (bytes === null) return none('oversize');

    const retryAfter = retryAfterOf(response);
    const requestId = requestIdOf(response);
    const extras = { ...(retryAfter === undefined ? {} : { retryAfter }), ...(requestId === undefined ? {} : { requestId }) };

    // A 401 on a bearer operation ends the sign-in, as the client has always
    // done, unless the operation takes a secret with a working credential
    // (a wrong password there is auth_failed and the person stays in).
    if (response.status === 401 && op.auth === 'bearer' && op.secret !== true && request.token === undefined) {
      session.forget();
      return { kind: 'unauthorized' };
    }

    const ok = response.status >= 200 && response.status < 300;
    if (ok && request.download !== undefined) return { kind: 'answer', status: response.status, ok, bytes, ...extras };
    if (bytes.byteLength === 0) {
      // No document: fine for an answer that is its status alone, not for one
      // that promises a body.
      if (ok && op.response !== undefined && response.status !== 204) return none('invalid');
      return { kind: 'answer', status: response.status, ok, ...extras };
    }
    const parsed = parseJson(bytes);
    if (ok) {
      if (op.response === undefined) return { kind: 'answer', status: response.status, ok, ...extras };
      if (!parsed.ok || op.response(parsed.value).length > 0) return none('invalid');
      return { kind: 'answer', status: response.status, ok, body: parsed.value, ...extras };
    }
    // A refusal: its error document if it is one, else only its status.
    if (parsed.ok && errorV0(parsed.value).length === 0) return { kind: 'answer', status: response.status, ok, body: parsed.value, ...extras };
    return { kind: 'answer', status: response.status, ok, ...extras };
  }
}
