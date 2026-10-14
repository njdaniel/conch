// The only module that reads or writes the credential, and the only one that
// calls conchd. The token lives in sessionStorage, which is per tab (decision
// D2): two tabs are two sign-ins, and closing the tab drops it. It is sent only
// as the Authorization header, and never put in a URL, a log call, the DOM or
// an error: a failure is reported as a kind, never with the request in it.
import { listChannelsResponse, type ChannelV0 } from './schema/channel';
import { whoAmIResponseV1, type WhoAmIResponseV1 } from './schema/principal';
import type { Validator } from './schema/validate';

// Why a call failed. unauthorized is a 401: the server does not accept the
// credential. network means no answer at all. invalid is any other reply
// (another status, or a body that is not the schema's shape).
export type ApiFailure = 'unauthorized' | 'network' | 'invalid';

export type ApiResult<T> = { readonly ok: true; readonly value: T } | { readonly ok: false; readonly failure: ApiFailure };

const tokenKey = 'conch.web.token';

// An operator credential that checked out but that the person has not yet
// agreed to use in this browser (ADR-007). It is kept in memory only, so a
// reload during the warning starts again at sign-in.
let pending: string | null = null;

function readToken(): string | null {
  try {
    return window.sessionStorage.getItem(tokenKey);
  } catch {
    return null;
  }
}

function writeToken(token: string): boolean {
  try {
    window.sessionStorage.setItem(tokenKey, token);
    return true;
  } catch {
    return false;
  }
}

function dropToken(): void {
  try {
    window.sessionStorage.removeItem(tokenKey);
  } catch {
    // Nothing stored that could be removed.
  }
}

async function get<T>(path: string, token: string, check: Validator<T>): Promise<ApiResult<T>> {
  let res: Response;
  try {
    res = await fetch(path, {
      headers: { Authorization: `Bearer ${token}`, Accept: 'application/json' },
      cache: 'no-store',
      credentials: 'omit',
    });
  } catch {
    return { ok: false, failure: 'network' };
  }
  if (res.status === 401) return { ok: false, failure: 'unauthorized' };
  if (!res.ok) return { ok: false, failure: 'invalid' };
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    return { ok: false, failure: 'invalid' };
  }
  if (check(body).length > 0) return { ok: false, failure: 'invalid' };
  return { ok: true, value: body as T };
}

// authed is every call made with the stored credential: it reads the token,
// makes the request and, on a 401, drops the token, so "a 401 anywhere signs
// out" holds for every call added here without each one remembering it.
// signIn is the one call that does not use it: it checks a candidate.
async function authed<T>(path: string, check: Validator<T>): Promise<ApiResult<T>> {
  const token = readToken();
  if (token === null) return { ok: false, failure: 'unauthorized' };
  const result = await get(path, token, check);
  if (!result.ok && result.failure === 'unauthorized') dropToken();
  return result;
}

// hasToken says whether this tab holds a credential from an earlier load.
export function hasToken(): boolean {
  return readToken() !== null;
}

// signIn checks a credential with GET /v1/whoami. A member's is stored at
// once. An operator's is held back until continueAsOperator, because a browser
// is not the place for it. A rejected credential is stored nowhere.
export async function signIn(candidate: string): Promise<ApiResult<WhoAmIResponseV1>> {
  pending = null;
  const result = await get('/v1/whoami', candidate, whoAmIResponseV1);
  if (!result.ok) return result;
  if (result.value.role === 'operator') {
    pending = candidate;
  } else if (!writeToken(candidate)) {
    // sessionStorage is refused (blocked site data): signing in would not last
    // a reload, and the lobby could not call the API either.
    return { ok: false, failure: 'invalid' };
  }
  return result;
}

// continueAsOperator keeps the operator credential held by signIn.
export function continueAsOperator(): boolean {
  const token = pending;
  pending = null;
  return token !== null && writeToken(token);
}

// restore checks the credential left by an earlier load of this tab. A 401
// drops it.
export async function restore(): Promise<ApiResult<WhoAmIResponseV1>> {
  return authed('/v1/whoami', whoAmIResponseV1);
}

// signOut forgets the credential in this tab. It revokes nothing on the server.
export function signOut(): void {
  pending = null;
  dropToken();
}

// listChannels reads the channels the caller can use. A 401 drops the
// credential, so the caller only has to go back to sign-in.
export async function listChannels(): Promise<ApiResult<ChannelV0[]>> {
  const result = await authed('/v1/channels', listChannelsResponse);
  if (!result.ok) return result;
  return { ok: true, value: result.value.channels };
}
