// What the bridge's functions do, in main (linux-desktop.md §4). Input: a
// request the IPC layer has already checked (caller, shape, size, rate).
// Output: the sanitised answer the renderer may see: a server's answer already
// validated, an account, or a refusal kind; never a bearer, a challenge, a
// ticket or a proof.
//
// It owns the sign-in sequence: send the step with no credential, consume the
// token the answer holds, check it with whoami under a new identity, and hand
// back the account. A credential that cannot be checked is revoked (best
// effort) and forgotten.
import type {
  BridgeReply,
  BridgeRequest,
  ServerAddress,
  SetServerAddressResult,
  SignInOutcome,
  SignInRefusal,
  SignOutOutcome,
} from '../../../web/src/platform/desktop/bridge.ts';
import { operations, type OperationId } from '../../../web/src/platform/operations.ts';
import type { WhoAmIResponseV1 } from '../../../web/src/schema/principal.ts';
import { whoAmIResponseV1 } from '../../../web/src/schema/principal.ts';
import { Client, type Exchange } from './client.ts';
import { parseServerAddress } from './origin.ts';
import type { Session } from './session.ts';

export const REVOKE_TIMEOUT_MS = 5_000;

// How long a checked, pasted credential waits to be kept: long enough for a
// person to read the operator warning.
export const PASTED_TTL_MS = 10 * 60_000;

export interface ServiceDeps {
  readonly client: Client;
  readonly session: Session;
  readonly now: () => number;
  // One line; never a body, a header or a credential.
  readonly log: (line: string) => void;
}

// The operations that give a credential, by the bridge function that sends
// them.
export type SignInStep =
  | 'login'
  | 'loginTotp'
  | 'redeemInvite'
  | 'redeemInviteGuest'
  | 'redeemSignInLink'
  | 'setupRedeem'
  | 'resetPassword'
  | 'joinGuest'
  | 'joinAccount';

// Operations the renderer may invoke that still have their own bridge
// function, so the generic request refuses them.
const DEDICATED: ReadonlySet<OperationId> = new Set<OperationId>(['logout']);

// Operations whose answer is a stream of bytes or whose request is one: the
// bridge's generic request carries JSON only. (Attachments have their own
// functions, main/files.ts.)
const NOT_JSON: ReadonlySet<OperationId> = new Set<OperationId>(['uploadFile', 'fileBytes']);

export function invocable(id: OperationId): boolean {
  const op = operations[id];
  return op.renderer && op.socket !== true && !DEDICATED.has(id);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// strip removes every credential field of the operation from an answer, and
// a field called token whatever the table says.
function strip(step: SignInStep, body: unknown): Record<string, unknown> {
  if (!isRecord(body)) return {};
  const removed = new Set<string>(['token', ...(operations[step].credentials ?? [])]);
  return Object.fromEntries(Object.entries(body).filter(([key]) => !removed.has(key)));
}

function refusalOf(exchange: Extract<Exchange, { kind: 'answer' }>): SignInRefusal {
  const doc = isRecord(exchange.body) ? exchange.body : {};
  const code = typeof doc['code'] === 'string' ? doc['code'] : undefined;
  const message = typeof doc['message'] === 'string' ? doc['message'] : undefined;
  return {
    status: exchange.status,
    ...(code === undefined ? {} : { code }),
    ...(message === undefined ? {} : { message }),
    ...(exchange.retryAfter === undefined ? {} : { retryAfter: exchange.retryAfter }),
    ...(exchange.requestId === undefined ? {} : { requestId: exchange.requestId }),
  };
}

function failed(exchange: Exchange): SignInOutcome {
  switch (exchange.kind) {
    case 'none':
      return exchange.reason === undefined ? { ok: false, kind: 'network' } : { ok: false, kind: 'network', reason: exchange.reason };
    case 'unauthorized':
      return { ok: false, kind: 'unauthorized', status: 401 };
    case 'answer':
      return { ok: false, kind: exchange.status === 401 ? 'unauthorized' : 'invalid', ...refusalOf(exchange) };
  }
}

// replyOf is what the renderer is told of an exchange: the server's answer
// already validated, or the reason there was none. Bytes never go this way.
export function replyOf(exchange: Exchange): BridgeReply {
  switch (exchange.kind) {
    case 'none':
      return exchange.reason === undefined ? { kind: 'none' } : { kind: 'none', reason: exchange.reason };
    case 'unauthorized':
      return { kind: 'unauthorized' };
    case 'answer':
      return {
        kind: 'reply',
        status: exchange.status,
        ...(exchange.body === undefined ? {} : { body: exchange.body }),
        ...(exchange.retryAfter === undefined ? {} : { retryAfter: exchange.retryAfter }),
        ...(exchange.requestId === undefined ? {} : { requestId: exchange.requestId }),
      };
  }
}

export class Service {
  readonly #deps: ServiceDeps;

  constructor(deps: ServiceDeps) {
    this.#deps = deps;
  }

  serverAddress(): ServerAddress {
    return { origin: this.#deps.session.origin };
  }

  setServerAddress(input: string): SetServerAddressResult {
    const parsed = parseServerAddress(input);
    if (!parsed.ok) return parsed;
    const { session } = this.#deps;
    const before = session.capture();
    session.setOrigin(parsed.origin);
    // A sign-in the change ended is revoked at the server that issued it.
    if (before.token !== null && !session.signedIn) void this.#revoke(before.origin, before.token);
    return { ok: true, origin: parsed.origin };
  }

  async request(request: BridgeRequest): Promise<BridgeReply> {
    const id = request.operation;
    // The IPC layer has refused these already; a second check costs nothing.
    if (!invocable(id)) return { kind: 'none', reason: 'unsupported' };
    if (NOT_JSON.has(id)) return { kind: 'none', reason: 'unsupported' };
    const exchange = await this.#deps.client.send({
      op: id,
      ...(request.params === undefined ? {} : { params: request.params }),
      ...(request.query === undefined ? {} : { query: request.query }),
      ...(request.body === undefined ? {} : { body: request.body }),
    });
    if (exchange.kind === 'none') this.#note(id, exchange.reason);
    return replyOf(exchange);
  }

  #note(id: OperationId, reason: string | undefined): void {
    if (reason === 'certificate' || reason === 'redirect' || reason === 'oversize' || reason === 'invalid') {
      this.#deps.log(`conch-desktop: ${id} had no answer: ${reason}`);
    }
  }

  // whoami with the credential now in force, or with a candidate that is not
  // yet kept.
  async #whoami(candidate?: string): Promise<{ readonly who: WhoAmIResponseV1 } | { readonly outcome: SignInOutcome }> {
    const exchange = await this.#deps.client.send({ op: 'whoami', ...(candidate === undefined ? {} : { token: candidate }) });
    if (exchange.kind === 'answer' && exchange.ok && whoAmIResponseV1(exchange.body).length === 0) {
      return { who: exchange.body as WhoAmIResponseV1 };
    }
    return { outcome: failed(exchange) };
  }

  async restore(): Promise<SignInOutcome> {
    if (!this.#deps.session.signedIn) return { ok: false, kind: 'unauthorized', status: 401 };
    const checked = await this.#whoami();
    return 'who' in checked ? { ok: true, who: checked.who, answer: {} } : checked.outcome;
  }

  // A pasted credential: checked with whoami, then set aside in main, not in
  // force. It is put in force only by keepCredential, which the page calls
  // when it keeps the credential: at once for a member's, and for an
  // operator's only once the person has passed the operator warning. One the
  // person declines is never in force, so a reload does not find it.
  async signInWithCredential(credential: string): Promise<SignInOutcome> {
    const { session } = this.#deps;
    const capture = session.capture();
    const exchange = await this.#deps.client.send({ op: 'whoami', token: credential, capture });
    if (exchange.kind !== 'answer' || !exchange.ok || whoAmIResponseV1(exchange.body).length > 0) return failed(exchange);
    if (!session.current(capture)) return { ok: false, kind: 'network', reason: 'stale' };
    session.holdPasted(credential, this.#deps.now() + PASTED_TTL_MS);
    return { ok: true, who: exchange.body as WhoAmIResponseV1, answer: {} };
  }

  // keepCredential puts in force the credential signInWithCredential set
  // aside. The page must present it again: one that was not just pasted and
  // checked, or has lapsed, is refused and nothing changes.
  keepCredential(credential: string): boolean {
    const { session } = this.#deps;
    if (!session.takePasted(credential, this.#deps.now())) return false;
    this.#adopt(credential);
    return true;
  }

  // adopt puts token in force. A credential it replaces is revoked (best
  // effort), not left valid at the server and held nowhere.
  #adopt(token: string): void {
    const { session } = this.#deps;
    const before = session.capture();
    session.adopt(token);
    if (before.token !== null && before.token !== token) void this.#revoke(before.origin, before.token);
  }

  // The steps that give a credential. request is already validated against the
  // operation's request validator.
  async signIn(step: SignInStep, request: Readonly<Record<string, unknown>>): Promise<SignInOutcome> {
    const { session, client } = this.#deps;
    const capture = session.capture();
    // The challenge of a code's step is a handle here; the server's own never
    // reached the renderer.
    let body: Readonly<Record<string, unknown>> = request;
    let handle: string | null = null;
    if (step === 'loginTotp') {
      handle = typeof request['challenge'] === 'string' ? request['challenge'] : null;
      const real = handle === null ? null : session.challenge(handle, this.#deps.now());
      if (real === null) return { ok: false, kind: 'invalid', status: 400, code: 'challenge_expired' };
      body = { ...request, challenge: real };
    }

    const exchange = await client.send({ op: step, body, capture });
    if (exchange.kind !== 'answer') return failed(exchange);
    if (!exchange.ok) {
      // The challenge stays for the next try while the server lets it.
      const keep = exchange.status === 400 || exchange.status === 401 || exchange.status === 429;
      if (handle !== null && !keep) session.dropChallenge(handle);
      return failed(exchange);
    }
    if (handle !== null) session.dropChallenge(handle);
    const answer = exchange.body;
    if (!isRecord(answer)) return { ok: false, kind: 'invalid', status: exchange.status };

    if (step === 'login' && answer['next'] === 'totp') {
      const until = typeof answer['challenge_expires_at'] === 'string' ? Date.parse(answer['challenge_expires_at']) : Number.NaN;
      if (typeof answer['challenge'] !== 'string' || Number.isNaN(until)) return { ok: false, kind: 'invalid', status: exchange.status };
      if (!session.current(capture)) return { ok: false, kind: 'network', reason: 'stale' };
      return {
        ok: false,
        kind: 'challenge',
        challenge: session.keepChallenge(answer['challenge'], until),
        expiresAt: answer['challenge_expires_at'] as string,
      };
    }

    // A reset signs nobody in, whatever the answer holds.
    if (step === 'resetPassword') return { ok: true, who: null, answer: strip(step, answer) };

    const token = answer['token'];
    if (typeof token !== 'string' || token === '') return { ok: false, kind: 'invalid', status: exchange.status };
    if (!session.current(capture)) {
      await this.#revoke(capture.origin, token);
      return { ok: false, kind: 'network', reason: 'stale' };
    }
    this.#adopt(token);
    const checked = await this.#whoami();
    if ('outcome' in checked) {
      // The credential works or it does not; either way it is not kept.
      if (session.signedIn) {
        const dropped = session.forget();
        if (dropped !== null) await this.#revoke(capture.origin, dropped);
      }
      return checked.outcome;
    }
    return { ok: true, who: checked.who, answer: strip(step, answer) };
  }

  // Ends the sign-in: forgets the credential at once, aborting whatever was in
  // flight, then tells the server if it can.
  async signOut(): Promise<SignOutOutcome> {
    const { session } = this.#deps;
    const origin = session.origin;
    const token = session.forget();
    if (token === null || origin === null) return 'ended';
    return this.#revoke(origin, token);
  }

  async #revoke(origin: string | null, token: string): Promise<SignOutOutcome> {
    if (origin === null) return 'unsent';
    const exchange = await this.#deps.client.send({
      op: 'logout',
      token,
      detached: true,
      capture: { generation: this.#deps.session.generation, origin, token, signal: AbortSignal.timeout(REVOKE_TIMEOUT_MS) },
    });
    if (exchange.kind === 'none') return 'unsent';
    // 401: the credential was no longer valid anyway.
    if (exchange.kind === 'answer' && (exchange.ok || exchange.status === 401)) return 'ended';
    return 'unconfirmed';
  }
}
