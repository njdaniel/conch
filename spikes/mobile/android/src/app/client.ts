// The app's API client: the one place a request is made. Every request is
// built from the list in logic/requests.ts, every answer is checked by the
// generated validators in logic/schema, and the bearer is held in this object's
// memory and nowhere else (#1109: Credential and API use; ADR-016 Decision 3).
//
// - The bearer is sent only in the Authorization header (logic/requests.ts
//   authorization). It is never in a URL, a log line or the diagnostics.
// - A 401 from an authenticated call is the credential being dead: onSignal is
//   told 'whoami_401' or 'ticket_401' so the app tears media down before it
//   shows signed out (logic/auth.ts).
// - Nothing here persists: no storage, no file. A relaunch is signed out.
import type { ApiFailure, CameraClaimResult, TransmitAnswer, VoiceSessionResult } from '../logic/api';
import { afterAuthSignal } from '../logic/auth';
import type { AuthSignal, AuthStep } from '../logic/auth';
import { authorization, build, type Built, type RequestName } from '../logic/requests';
import type {
  ListChannelsResponse,
  LoginResponseV1,
  RedeemSignInLinkResponseV1,
  VoicePresenceV1,
  VoiceTransmitState,
  WhoAmIResponseV1,
} from '../logic/schema/generated';
import {
  errorV0,
  listChannelsResponse,
  loginResponseV1,
  redeemSignInLinkResponseV1,
  voiceCameraV1,
  voicePresenceV1,
  voiceSessionResponseV1,
  whoAmIResponseV1,
} from '../logic/schema/generated';
import type { Validator } from '../logic/schema/validate';

export interface HttpResponse {
  readonly status: number;
  json(): Promise<unknown>;
}

export interface HttpInit {
  readonly method: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly body?: string;
  readonly signal?: AbortSignal;
}

// fetch, as the client needs it. The app passes the global one.
export type Fetch = (url: string, init: HttpInit) => Promise<HttpResponse>;

export type Result<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false; readonly failure: ApiFailure; readonly status?: number; readonly code?: string };

export interface ClientOptions {
  readonly fetch: Fetch;
  // Told when the credential is refused; the app runs the steps in order.
  readonly onSignal: (signal: AuthSignal, steps: readonly AuthStep[]) => void;
  // A line for the diagnostics: route name and status only, never a URL.
  readonly note?: (message: string) => void;
}

export class ApiClient {
  private readonly options: ClientOptions;
  private origin = '';
  private bearer: string | null = null;

  constructor(options: ClientOptions) {
    this.options = options;
  }

  // The server the person typed. Plain HTTP is the development profile (the
  // debug build allows cleartext; the release build does not).
  setServer(origin: string): void {
    this.origin = origin.trim().replace(/\/+$/, '');
  }

  get server(): string {
    return this.origin;
  }

  get signedIn(): boolean {
    return this.bearer !== null;
  }

  // The bearer, for the diagnostics' redactor to hold and for nothing else.
  get secret(): string | null {
    return this.bearer;
  }

  // signOut forgets the bearer. The caller has already torn media down.
  signOut(): void {
    this.bearer = null;
  }

  async login(name: string, password: string): Promise<Result<LoginResponseV1>> {
    const reply = await this.send('login', {}, {}, { name, password }, false);
    if (!reply.ok) return reply;
    const out = await this.read(reply.value, loginResponseV1);
    if (out.ok && out.value.next === 'signed_in') this.bearer = out.value.token;
    return out;
  }

  // redeemSignInLink spends a one-use code. The link may be pasted whole
  // (<origin>/app/#/signin/<code>) or as the bare code.
  async redeemSignInLink(codeOrLink: string): Promise<Result<RedeemSignInLinkResponseV1>> {
    const reply = await this.send('redeemSignInLink', {}, {}, { code: signInCode(codeOrLink) }, false);
    if (!reply.ok) return reply;
    const out = await this.read(reply.value, redeemSignInLinkResponseV1);
    if (out.ok) this.bearer = out.value.token;
    return out;
  }

  async whoami(): Promise<Result<WhoAmIResponseV1>> {
    return this.getJson('whoami', {}, whoAmIResponseV1, 'whoami_401');
  }

  async channels(): Promise<Result<ListChannelsResponse>> {
    return this.getJson('listChannels', {}, listChannelsResponse, 'whoami_401');
  }

  async presence(channel: string): Promise<Result<VoicePresenceV1>> {
    return this.getJson('voicePresence', { channel }, voicePresenceV1, 'whoami_401');
  }

  async voiceSession(channel: string): Promise<VoiceSessionResult> {
    const reply = await this.send('voiceSession', { channel }, {}, undefined, true);
    if (!reply.ok) return reply;
    return this.read(reply.value, voiceSessionResponseV1);
  }

  // reportTransmit is one transmit report; the ReportQueue calls it.
  async reportTransmit(channel: string, state: VoiceTransmitState, signal: AbortSignal): Promise<TransmitAnswer> {
    const reply = await this.send('voiceTransmit', { channel }, {}, { state }, true, signal);
    if (!reply.ok) {
      if (reply.failure === 'unauthorized') return 'unauthorized';
      return reply.failure === 'network' ? 'failed' : reply.status !== undefined && reply.status >= 500 ? 'failed' : reply.status === 429 ? 'rate_limited' : 'no_session';
    }
    return 'ok';
  }

  // claimCamera claims the camera: without startedAt it starts a claim (the
  // camera button alone); with it, it names the claim it continues. Only
  // `started_at` is ever sent as a query: never `participant=`.
  async claimCamera(channel: string, startedAt?: string): Promise<CameraClaimResult> {
    const reply = await this.send('claimCamera', { channel }, { started_at: startedAt }, undefined, true, undefined, 'whoami_401');
    if (!reply.ok) return reply;
    return this.read(reply.value, voiceCameraV1);
  }

  // releaseCamera ends the claim whose started_at it names, and no other.
  async releaseCamera(channel: string, startedAt: string): Promise<Result<null>> {
    const reply = await this.send('stopCamera', { channel }, { started_at: startedAt }, undefined, true, undefined, 'whoami_401');
    return reply.ok ? { ok: true, value: null } : reply;
  }

  // ---- internals

  private async getJson<T>(name: RequestName, params: Record<string, string>, check: Validator<T>, signal: AuthSignal): Promise<Result<T>> {
    const reply = await this.send(name, params, {}, undefined, true, undefined, signal);
    if (!reply.ok) return reply;
    return this.read(reply.value, check);
  }

  private async read<T>(res: HttpResponse, check: Validator<T>): Promise<Result<T>> {
    try {
      const parsed: unknown = await res.json();
      if (check(parsed).length === 0) return { ok: true, value: parsed as T };
    } catch {
      // Fall through: not a document of the shape.
    }
    return { ok: false, failure: 'invalid' };
  }

  // send makes one request from the list. It answers the response on a 2xx and
  // a failure otherwise; a 401 on an authenticated call raises the auth signal.
  private async send(
    name: RequestName,
    params: Record<string, string>,
    query: Record<string, string | undefined>,
    body: unknown,
    authed: boolean,
    abort?: AbortSignal,
    signal: AuthSignal = 'ticket_401',
  ): Promise<{ ok: true; value: HttpResponse } | { ok: false; failure: ApiFailure; status?: number; code?: string }> {
    const built: Built = build(name, params, query);
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (authed) {
      if (this.bearer === null) return { ok: false, failure: 'unauthorized' };
      Object.assign(headers, authorization(this.bearer));
    }
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    let res: HttpResponse;
    try {
      res = await this.options.fetch(this.origin + built.url, {
        method: built.method,
        headers,
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
        ...(abort === undefined ? {} : { signal: abort }),
      });
    } catch {
      this.options.note?.(`${name}: no answer`);
      return { ok: false, failure: 'network' };
    }
    this.options.note?.(`${name}: ${res.status}`);
    if (res.status >= 200 && res.status < 300) return { ok: true, value: res };
    if (res.status === 401 && authed) {
      // The credential is dead: forget it, then let the app tear media down and
      // show signed out (auth.ts gives the order).
      this.bearer = null;
      this.options.onSignal(signal, afterAuthSignal(signal));
      return { ok: false, failure: 'unauthorized', status: 401 };
    }
    const code = await refusalCode(res);
    return code === undefined ? { ok: false, failure: 'invalid', status: res.status } : { ok: false, failure: 'invalid', status: res.status, code };
  }
}

async function refusalCode(res: HttpResponse): Promise<string | undefined> {
  try {
    const body: unknown = await res.json();
    if (errorV0(body).length === 0) return (body as { code: string }).code;
  } catch {
    // No document, or not one.
  }
  return undefined;
}

// signInCode is the code of a pasted sign-in link, or the text as typed.
export function signInCode(text: string): string {
  const t = text.trim();
  const match = /#\/signin\/([^/?\s]+)/.exec(t);
  return match?.[1] === undefined ? t : decodeURIComponent(match[1]);
}
