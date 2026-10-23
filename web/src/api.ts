// The only module that reads or writes the credential, and the only one that
// calls conchd. Where the token lives depends on whose it is (issue #439,
// decision O1). A member's lasts: it is kept in localStorage, so it survives
// closing the browser and every tab of the origin shares it, until it is
// signed out or stops working. An operator's stays with the one tab that
// agreed to use it, in sessionStorage (decision D2, kept for operators). A
// temporary guest's is kept for its one tab too (issue #713, ADR-009 D2): a
// refresh keeps the visit, another tab or another browser does not have it,
// and it is never kept to last. Closing a tab is no reliable end of a visit
// (a browser may restore its tabs, storage and all): End visit is, and the
// server's expiry bounds the rest. The
// page has no third-party script (script-src 'self'), which is what makes
// local storage acceptable. The token is sent only as the Authorization
// header, and never put in a URL, a log call, the DOM or an error: a failure
// is reported as a kind, never with the request in it.
import { listActivityResponseV1, type AgentActivityV1 } from './schema/activity';
import { listOwnCredentialsResponseV1, revokeAllCredentialsResponseV1, type OwnCredentialV1 } from './schema/credential';
import { errorV0, type ErrorV0 } from './schema/error';
import { loginResponseV1, type LoginRequestV1 } from './schema/auth';
import {
  convertGuestResponseV1,
  joinAccountResponseV1,
  joinGuestResponseV1,
  joinInfoV1,
  type ConvertGuestRequestV1,
  type ConvertGuestResponseV1,
  type JoinAccountRequestV1,
  type JoinGuestRequestV1,
  type JoinInfoV1,
} from './schema/guest';
import { health } from './schema/health';
import {
  createInviteResponseV1,
  invitePreviewV1,
  listInvitesResponseV1,
  redeemInviteResponseV1,
  type CreateInviteRequestV1,
  type CreateInviteResponseV1,
  type InvitePreviewRequestV1,
  type InvitePreviewV1,
  type InviteV1,
  type RedeemInviteGuestRequestV1,
  type RedeemInviteRequestV2,
} from './schema/invite';
import { interventionV1, listInterventionsResponseV1, type CreateInterventionRequestV1, type InterventionV1 } from './schema/intervention';
import { listRunsResponseV1, runV1, type RunV1 } from './schema/run';
import { sessionV1, type CreateSessionRequestV1, type SessionV1 } from './schema/session';
import {
  createChannelResponse,
  listChannelMembersResponseV1,
  listChannelsResponse,
  renameChannelResponseV1,
  type ChannelMemberV1,
  type ChannelV0,
  type CreateChannelRequest,
  type RenameChannelRequestV1,
} from './schema/channel';
import { listMessagesResponseV2, postMessageResponseV2, type Audience, type MessageV2, type PostMessageRequestV2 } from './schema/message';
import { listNetsResponseV1, type NetV1 } from './schema/net';
import { redeemSignInLinkResponseV1, signInLinkPreviewV1, type RedeemSignInLinkRequestV1, type SignInLinkPreviewRequestV1, type SignInLinkPreviewV1 } from './schema/signin';
import { listReadStateResponseV1, readStateV1, type MarkReadRequestV1, type ReadStateV1 } from './schema/readstate';
import {
  renamePrincipalResponseV1,
  whoAmIResponseV1,
  type PrincipalV0,
  type RenamePrincipalRequestV1,
  type RenamePrincipalResponseV1,
  type WhoAmIResponseV1,
} from './schema/principal';
import { wsTicketV1, type WSTicketV1 } from './schema/ticket';
import {
  voiceOccupancyV1,
  voicePresenceV1,
  voiceSessionResponseV1,
  voiceShareV1,
  type VoiceOccupancyV1,
  type VoicePresenceV1,
  type VoiceSessionResponseV1,
  type VoiceShareV1,
  type VoiceTransmitReportV1,
  type VoiceTransmitState,
} from './schema/voice';
import { createProblemReportResponseV1, type CreateProblemReportRequestV1, type CreateProblemReportResponseV1, type DiagnosticSocket } from './schema/report';
import type { Validator } from './schema/validate';
import type { FeedSocket, OpenSocket, SocketEvents } from './state/feed';
import { forget as forgetDiagnostics, record } from './diagnostics';
import { visitOf } from './guestName';

// Why a call failed. unauthorized is a 401: the server does not accept the
// credential. network means no answer at all. invalid is any other reply
// (another status, or a body that is not the schema's shape); when the server
// refused with an error document, code is its code (run_not_found, say).
export type ApiFailure = 'unauthorized' | 'network' | 'invalid';

export type ApiResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false; readonly failure: ApiFailure; readonly code?: string };

const tokenKey = 'conch.web.token';

// Where a credential is kept: 'tab' is this tab alone (sessionStorage), for an
// operator's and a guest's; 'lasting' is every tab of the origin until it is
// signed out (localStorage), for a member's.
type Place = 'tab' | 'lasting';

// The credential this tab uses, and where it was found, so that what drops
// it drops it from there.
interface Held {
  readonly token: string;
  readonly place: Place;
}

// The tab's own credential comes first: an operator who continued in this tab
// is that operator here, whatever member has signed in elsewhere.
const places: readonly Place[] = ['tab', 'lasting'];

// storageAt may throw (blocked site data); every caller is inside a try.
function storageAt(place: Place): Storage {
  return place === 'tab' ? window.sessionStorage : window.localStorage;
}

// An operator credential that checked out but that the person has not yet
// agreed to use in this browser (ADR-007). It is kept in memory only, so a
// reload during the warning starts again at sign-in.
let pending: string | null = null;

// The credential this page signed in with: set by signIn (a member's),
// continueAsOperator and restore, and cleared by the sign-outs and by any 401.
// Every call sends this one and no other. Storage is only where it is kept: if
// another tab replaces or removes it there, the page is no longer signed in,
// and a call never falls through to whatever else is stored, which could be
// another person's credential under this page's name.
let pinned: Held | null = null;

// unchecked is the credential restore found stored but could not check (the
// server did not answer): neither pinned nor dropped. Only it may be forgotten
// by forgetStored, and only while it is still the one stored (issue #469).
let unchecked: Held | null = null;

// stillStored says whether held is still the credential stored at its place.
function stillStored(held: Held): boolean {
  try {
    return storageAt(held.place).getItem(tokenKey) === held.token;
  } catch {
    return false;
  }
}

// current is the pinned credential while it is still the one stored at its
// place, else nothing (and the pin is cleared).
function current(): Held | null {
  const held = pinned;
  if (held === null) return null;
  let stored: string | null = null;
  try {
    stored = storageAt(held.place).getItem(tokenKey);
  } catch {
    // Refused: as good as gone.
  }
  if (stored === held.token) return held;
  pinned = null;
  return null;
}

// forget drops the credential held, from memory and from its storage. The
// diagnostic ring goes with it: the session it describes is over, and the next
// person to sign in on this browser must not report the last one's events.
function forget(held: Held): void {
  if (pinned?.token === held.token && pinned.place === held.place) pinned = null;
  dropToken(held);
  forgetDiagnostics();
}

// readToken finds a credential left by an earlier load, for restore to check:
// this tab's own first, then the lasting one. Nothing else reads storage for a
// credential.
function readToken(): Held | null {
  for (const place of places) {
    try {
      const token = storageAt(place).getItem(tokenKey);
      if (token !== null) return { token, place };
    } catch {
      // This storage is refused; the other may still hold one.
    }
  }
  return null;
}

function writeToken(token: string, place: Place): boolean {
  try {
    storageAt(place).setItem(tokenKey, token);
    return true;
  } catch {
    return false;
  }
}

// dropToken forgets the credential held, if it is still the one stored there:
// another tab may have signed in again since this one read it, and a late 401
// for the old credential must not sign the new one out.
function dropToken(held: Held): void {
  try {
    const storage = storageAt(held.place);
    if (storage.getItem(tokenKey) === held.token) storage.removeItem(tokenKey);
  } catch {
    // Nothing stored that could be removed.
  }
}

// refusalCode is the code of the error document a refusal carries, or none.
async function refusalCode(res: Response): Promise<string | undefined> {
  try {
    const body: unknown = await res.json();
    if (errorV0(body).length === 0) return (body as { code: string }).code;
  } catch {
    // No document, or not one.
  }
  return undefined;
}

// Diagnostics (issue #446). Every call that fails is noted in the page's
// diagnostic ring (diagnostics.ts) by its status, the server's error code and
// the X-Request-Id of the answer, which the operator can find in conchd's log.
// Never the path, which can name a channel, nor anything the call sent.

// noteNoAnswer notes a call that got no answer at all.
function noteNoAnswer(): void {
  record({ kind: 'http_failure', status: 0, error: 'network' });
}

// noteRefusal notes an answer that was not a success. The body is read from a
// copy, so the caller can still read it.
async function noteRefusal(res: Response): Promise<void> {
  let code: string | undefined;
  try {
    const body: unknown = await res.clone().json();
    if (errorV0(body).length === 0) code = (body as { code: string }).code;
  } catch {
    // No document, or not one.
  }
  record({ kind: 'http_failure', status: res.status, requestId: res.headers.get('X-Request-Id'), ...(code === undefined ? {} : { error: code }) });
}

// discard reads an answer's body to its end and drops it, for a call whose
// answer is its status alone. Chromium hands even a 204 a body, and logs a
// request whose body is left unread, or is cancelled, as failed
// (net::ERR_ABORTED) once the response is let go; read to the end, it is a
// request finished (issues #595, #622). sendAs does this for every 204, so a
// call added later cannot forget. A body that cannot be read changes nothing:
// the status was the answer.
async function discard(res: Response): Promise<void> {
  try {
    await res.arrayBuffer();
  } catch {
    // Read or not, the status stands.
  }
}

async function refused(res: Response): Promise<{ readonly ok: false; readonly failure: 'invalid'; readonly code?: string }> {
  const code = await refusalCode(res);
  return code === undefined ? { ok: false, failure: 'invalid' } : { ok: false, failure: 'invalid', code };
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
    noteNoAnswer();
    return { ok: false, failure: 'network' };
  }
  if (!res.ok) await noteRefusal(res);
  if (res.status === 401) return { ok: false, failure: 'unauthorized' };
  if (!res.ok) return refused(res);
  let body: unknown;
  try {
    body = await res.json();
  } catch {
    return { ok: false, failure: 'invalid' };
  }
  if (check(body).length > 0) return { ok: false, failure: 'invalid' };
  return { ok: true, value: body as T };
}

// authed is every call made with the signed-in credential: it takes the pinned
// token, makes the request and, on a 401, drops the token, so "a 401 anywhere
// signs out" holds for every call added here without each one remembering it.
// signIn and restore are the calls that do not use it: they check a candidate.
async function authed<T>(path: string, check: Validator<T>): Promise<ApiResult<T>> {
  const held = current();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const result = await get(path, held.token, check);
  if (!result.ok && result.failure === 'unauthorized') forget(held);
  return result;
}

// authedSend is the writing sibling of authed: the stored credential, a JSON
// body (or none), and on a 401 the token dropped. It hands back the response
// itself, because a write must tell "no answer" from "answered no". keepalive
// lets the request outlive the page, for the one sent as the page goes away.
type PostReply =
  | { readonly kind: 'reply'; readonly res: Response }
  | { readonly kind: 'none' }
  | { readonly kind: 'unauthorized' };

async function authedSend(method: 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown, keepalive = false, signal?: AbortSignal): Promise<PostReply> {
  const held = current();
  if (held === null) return { kind: 'unauthorized' };
  return sendAs(held, method, path, body, keepalive, signal);
}

// sendAs is authedSend with the credential already taken, for the sign-outs,
// which must drop the very credential they revoked.
async function sendAs(held: Held, method: 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown, keepalive = false, signal?: AbortSignal): Promise<PostReply> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers: {
        Authorization: `Bearer ${held.token}`,
        Accept: 'application/json',
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
      credentials: 'omit',
      keepalive,
      ...(signal === undefined ? {} : { signal }),
    });
  } catch {
    // A request its caller gave up on (a transmit report past its use) is
    // not a failure of the server's.
    if (signal?.aborted !== true) noteNoAnswer();
    return { kind: 'none' };
  }
  if (!res.ok) await noteRefusal(res);
  if (res.status === 401) {
    forget(held);
    return { kind: 'unauthorized' };
  }
  // A 204 is a status alone by definition (sign-outs, closes, a transmit
  // report): read it out here so no caller has to remember to (issue #622).
  if (res.status === 204) await discard(res);
  return { kind: 'reply', res };
}

const authedPost = (path: string, body?: unknown) => authedSend('POST', path, body);

// The version of the server this page was first served by; see clientVersion.
let pageVersion: string | null = null;

// serverVersion reads the server's build version from GET /healthz, which
// needs no credential. It is null when the server cannot be reached or does not
// answer with a health document. A degraded server (503) still names its
// version, so the status is not looked at.
export async function serverVersion(): Promise<string | null> {
  try {
    const res = await fetch('/healthz', { headers: { Accept: 'application/json' }, cache: 'no-store', credentials: 'omit' });
    const body: unknown = await res.json();
    if (health(body).length > 0) return null;
    const version = (body as { version: string }).version;
    pageVersion ??= version;
    return version;
  } catch {
    return null;
  }
}

// clientVersion is the page's own build: the version the first serverVersion
// read. A report sends it as the client's version, and the server adds its
// own, which differs when the server was upgraded under an open page.
export function clientVersion(): string | null {
  return pageVersion;
}

// hasToken says whether this tab has a credential from an earlier load: its
// own, or a member's lasting one.
export function hasToken(): boolean {
  return readToken() !== null;
}

// signIn checks a credential with GET /v1/whoami. A member's is stored at
// once, to last. An operator's is held back until continueAsOperator, because
// a browser is not the place for it. A rejected credential is stored nowhere.
//
// It is refused ('held') while this browser holds the sign-in restore could
// not check (the server was not reachable when the page loaded): signing in
// over it would overwrite a member's credential without revoking it, or sit
// under an operator's tab credential that the next load would prefer (issue
// #469); forgetStored ends that one first. A credential stored since this page
// looked (another tab signed in) is not this page's to replace or forget: the
// answer is 'stored', and the page checks it as on load.
export async function signIn(candidate: string): Promise<ApiResult<WhoAmIResponseV1> | { readonly ok: false; readonly failure: 'held' | 'stored' }> {
  if (unchecked !== null && !stillStored(unchecked)) unchecked = null;
  if (pending !== null || unchecked !== null) return { ok: false, failure: 'held' };
  if (hasToken()) return { ok: false, failure: 'stored' };
  pending = null;
  pinned = null;
  const result = await get('/v1/whoami', candidate, whoAmIResponseV1);
  if (!result.ok) return result;
  // A guest's credential pasted here stays with this tab, as a visit's does.
  const place: Place = visitOf(result.value) === null ? 'lasting' : 'tab';
  if (result.value.role === 'operator') {
    pending = candidate;
  } else if (writeToken(candidate, place)) {
    pinned = { token: candidate, place };
  } else {
    // localStorage is refused (blocked site data): signing in would not last
    // a reload, and the lobby could not call the API either.
    return { ok: false, failure: 'invalid' };
  }
  return result;
}

// continueAsOperator keeps the operator credential held by signIn, in this tab
// only.
export function continueAsOperator(): boolean {
  const token = pending;
  pending = null;
  if (token === null || !writeToken(token, 'tab')) return false;
  pinned = { token, place: 'tab' };
  return true;
}

// What restore found: who the stored credential is, and whether it is an
// operator's that was kept to last, which must pass the warning first.
export interface Restored {
  readonly principal: WhoAmIResponseV1;
  readonly warn: boolean;
}

// restore checks the credential left by an earlier load: this tab's own, else
// the lasting one, and signs in with it. A 401 drops it. An operator's found
// in the lasting place is never kept there: it is taken out and held back for
// the operator warning, as signIn holds one.
export async function restore(): Promise<ApiResult<Restored>> {
  pending = null;
  pinned = null;
  unchecked = null;
  let held = readToken();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const result = await get('/v1/whoami', held.token, whoAmIResponseV1);
  if (!result.ok) {
    if (result.failure === 'unauthorized') dropToken(held);
    else unchecked = held;
    return result;
  }
  if (held.place === 'lasting' && result.value.role === 'operator') {
    dropToken(held);
    pending = held.token;
    return { ok: true, value: { principal: result.value, warn: true } };
  }
  if (held.place === 'lasting' && visitOf(result.value) !== null) {
    // A visit is never kept to last: found there, it is moved to this tab,
    // and the other tabs of the origin no longer have it.
    dropToken(held);
    held = { token: held.token, place: 'tab' };
    if (!writeToken(held.token, held.place)) {
      // This tab cannot keep it either: it is revoked (if the server
      // answers) rather than left alive and kept nowhere.
      await sendAs(held, 'DELETE', '/v1/credentials/current');
      return { ok: false, failure: 'unauthorized' };
    }
  }
  pinned = held;
  return { ok: true, value: { principal: result.value, warn: false } };
}

// whoAmI asks who the signed-in credential is, mid-session (after a socket was
// refused, say). Unlike restore it never picks a credential: it asks with the
// one this page signed in with, and a 401 drops it.
export function whoAmI(): Promise<ApiResult<WhoAmIResponseV1>> {
  return authed('/v1/whoami', whoAmIResponseV1);
}

// signOut revokes the credential this tab uses (DELETE /v1/credentials/current)
// and then forgets it here, whatever the answer. It resolves to whether the
// server was told: true when it revoked the credential, when the credential
// was no longer valid anyway (401), or when none was stored; false when there
// was no answer or a refusal, and the credential may still work elsewhere.
export async function signOut(): Promise<boolean> {
  pending = null;
  const held = current();
  pinned = null;
  forgetDiagnostics();
  if (held === null) return true;
  const reply = await sendAs(held, 'DELETE', '/v1/credentials/current');
  dropToken(held);
  // Again: the revoke itself may have been noted, and is the last session's.
  forgetDiagnostics();
  return reply.kind === 'unauthorized' || (reply.kind === 'reply' && reply.res.ok);
}

// signOutEverywhere revokes every credential of the signed-in principal
// (POST /v1/principals/{id}/credentials/revoke-all), so every browser and
// client signed in as them is refused on its next request, and then forgets
// the credential here. The value is how many were revoked, or null when the
// answer was a success without a readable count. On any failure nothing is
// forgotten: the person is still signed in here, and can try again.
export async function signOutEverywhere(principal: number): Promise<ApiResult<number | null>> {
  pending = null;
  const held = current();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const reply = await sendAs(held, 'POST', `/v1/principals/${String(principal)}/credentials/revoke-all`);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  // A success: the credentials are revoked, this one with them, whatever the
  // body turns out to be.
  forget(held);
  try {
    const parsed: unknown = await reply.res.json();
    if (revokeAllCredentialsResponseV1(parsed).length === 0) return { ok: true, value: (parsed as { revoked: number }).revoked };
  } catch {
    // Fall through.
  }
  return { ok: true, value: null };
}

// listSignIns reads the caller's own sign-ins (issue #496): the credentials of
// theirs that still work, newest first, with the one this page uses marked
// current. The server never sends a token in it.
export async function listSignIns(): Promise<ApiResult<OwnCredentialV1[]>> {
  const result = await authed('/v1/credentials/mine', listOwnCredentialsResponseV1);
  return result.ok ? { ok: true, value: result.value.credentials } : result;
}

// endSignIn ends another of the caller's sign-ins by its id, so whatever
// browser or client holds it is refused on its next request. This page's own
// is ended by signOut instead, which also forgets it here. A refusal carries
// the server's code: credential_not_found for an id that is not the caller's.
export async function endSignIn(id: number): Promise<ApiResult<null>> {
  const reply = await authedSend('DELETE', `/v1/credentials/mine/${String(id)}`);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  return { ok: true, value: null };
}

// Invites (issue #440). Preview and redeem need no credential: the code is
// the proof. It travels in a POST body only, never in a URL, so no address,
// access log or performance entry holds it; nothing here stores it, logs it or
// keeps it once the call returns.

// A refused invite call says why: the server's code; its own line, for a
// request it could not take (invalid_request names the rule broken, never the
// value sent); and, for rate_limited, the seconds its Retry-After asks for.
export type InviteResult<T> =
  | { readonly ok: true; readonly value: T }
  | {
      readonly ok: false;
      readonly failure: ApiFailure;
      readonly code?: string;
      readonly message?: string;
      readonly retryAfter?: number;
    };

export type InviteRefusal = Extract<InviteResult<never>, { ok: false }>;

async function inviteRefused(res: Response): Promise<InviteRefusal> {
  let doc: ErrorV0 | undefined;
  try {
    const body: unknown = await res.json();
    if (errorV0(body).length === 0) doc = body as ErrorV0;
  } catch {
    // No document, or not one.
  }
  const message = doc?.message.replace(/\s+/g, ' ').trim().slice(0, 300) ?? '';
  const retryAfter = Number.parseInt(res.headers.get('Retry-After') ?? '', 10);
  return {
    ok: false,
    failure: 'invalid',
    ...(doc === undefined ? {} : { code: doc.code }),
    ...(message === '' ? {} : { message }),
    ...(Number.isFinite(retryAfter) && retryAfter >= 0 ? { retryAfter } : {}),
  };
}

async function readBody<T>(res: Response, check: Validator<T>): Promise<InviteResult<T>> {
  try {
    const parsed: unknown = await res.json();
    if (check(parsed).length === 0) return { ok: true, value: parsed as T };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// sendOpen posts a JSON body with no credential at all, for the invite,
// sign-in link, joining and password routes that take none; with no body it
// is a GET (the front door). It is null when there was no answer.
async function sendOpen(path: string, body?: unknown): Promise<Response | null> {
  let res: Response;
  try {
    res = await fetch(path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: body === undefined ? { Accept: 'application/json' } : { Accept: 'application/json', 'Content-Type': 'application/json' },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      cache: 'no-store',
      credentials: 'omit',
    });
  } catch {
    noteNoAnswer();
    return null;
  }
  if (!res.ok) await noteRefusal(res);
  return res;
}

// previewInvite asks what an invite would grant, without using it.
export async function previewInvite(inviteCode: string): Promise<InviteResult<InvitePreviewV1>> {
  const request: InvitePreviewRequestV1 = { code: inviteCode };
  const res = await sendOpen('/v1/invites/preview', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  return readBody(res, invitePreviewV1);
}

// What redeeming gave besides an answer of the server's: held is a refusal
// before anything was sent, because this browser still holds another
// sign-in (see redeemInvite); unkept is an account made whose credential this
// browser would not keep (site data blocked), so it cannot sign in with it;
// unchecked is one made and kept whose whoami did not answer, which a reload
// finishes.
export type RedeemResult =
  | InviteResult<WhoAmIResponseV1>
  | { readonly ok: false; readonly failure: 'held' }
  | { readonly ok: false; readonly failure: 'unkept' }
  | { readonly ok: false; readonly failure: 'unchecked' };

// heldElsewhere says whether this browser holds a sign-in other than one
// redeeming could make: a credential pinned, one waiting behind the operator
// warning, or one stored that could not be checked (restore had no answer, so
// it was neither pinned nor dropped).
export function heldElsewhere(): boolean {
  return pending !== null || hasToken();
}

// forgetStored ends the sign-in restore could not check, and only that one:
// it is revoked on the server if the server answers (best effort), then
// dropped here, while it is still the credential stored. A credential another
// tab stored since is never touched. It resolves to whether the server was
// told (true when there was nothing to forget).
export async function forgetStored(): Promise<boolean> {
  const held = unchecked;
  unchecked = null;
  if (held === null || !stillStored(held)) return true;
  const reply = await sendAs(held, 'DELETE', '/v1/credentials/current');
  dropToken(held);
  return reply.kind === 'unauthorized' || (reply.kind === 'reply' && reply.res.ok);
}

// redeemInvite uses an invite to make a new member called name, with the
// password it chose (POST /v2/invites/redeem, issue #713: the v1 route makes
// no account, design §7.6 B1). The token in the answer is kept as signIn
// keeps a member's, to last, and pinned; then whoami says who the page now is.
//
// Redeeming makes a new person, so it is refused while this browser holds any
// other sign-in, checked or not: the page signs it out first (signOut), which
// revokes it rather than leaving it behind. Writing over one that could not
// be checked would leave a member's credential alive and unrevoked, and an
// operator's tab credential would win over the joiner's at the next load.
// The password goes in the body alone and is kept nowhere.
export async function redeemInvite(inviteCode: string, name: string, password: string): Promise<RedeemResult> {
  if (heldElsewhere()) return { ok: false, failure: 'held' };
  pinned = null;
  const request: RedeemInviteRequestV2 = { code: inviteCode, name, password };
  const res = await sendOpen('/v2/invites/redeem', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const redeemed = await readBody(res, redeemInviteResponseV1);
  if (!redeemed.ok) return redeemed;
  return keepRedeemed(redeemed.value.token, redeemed.value.principal.id, 'lasting');
}

// redeemInviteAsGuest uses an invite that admits guests to start a visit
// (POST /v1/invites/redeem-guest), with the display label typed, if any. The
// visit's credential is kept for this tab alone. It is refused while this
// browser holds another sign-in, as redeemInvite is.
export async function redeemInviteAsGuest(inviteCode: string, label: string): Promise<RedeemResult> {
  if (heldElsewhere()) return { ok: false, failure: 'held' };
  pinned = null;
  const request: RedeemInviteGuestRequestV1 = label === '' ? { code: inviteCode } : { code: inviteCode, label };
  const res = await sendOpen('/v1/invites/redeem-guest', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const joined = await readBody(res, joinGuestResponseV1);
  if (!joined.ok) return joined;
  return keepRedeemed(joined.value.token, joined.value.principal.id, 'tab');
}

// Joining from the bare address (issue #713). The front door says what the
// address offers now; the server decides again when someone joins.

// joinInfo reads the front door (GET /v1/join), with no credential.
export async function joinInfo(): Promise<InviteResult<JoinInfoV1>> {
  const res = await sendOpen('/v1/join');
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  return readBody(res, joinInfoV1);
}

// joinAsGuest starts a visit from the bare address (POST /v1/join/guest).
// Its credential is kept for this tab alone, as an invite's guest's is.
export async function joinAsGuest(label: string): Promise<RedeemResult> {
  if (heldElsewhere()) return { ok: false, failure: 'held' };
  pinned = null;
  const request: JoinGuestRequestV1 = label === '' ? {} : { label };
  const res = await sendOpen('/v1/join/guest', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const joined = await readBody(res, joinGuestResponseV1);
  if (!joined.ok) return joined;
  return keepRedeemed(joined.value.token, joined.value.principal.id, 'tab');
}

// createAccount makes a member from the bare address, name and password
// together (POST /v1/join/account). Its credential is kept to last, as an
// invite's member's is.
export async function createAccount(name: string, password: string): Promise<RedeemResult> {
  if (heldElsewhere()) return { ok: false, failure: 'held' };
  pinned = null;
  const request: JoinAccountRequestV1 = { name, password };
  const res = await sendOpen('/v1/join/account', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const made = await readBody(res, joinAccountResponseV1);
  if (!made.ok) return made;
  return keepRedeemed(made.value.token, made.value.principal.id, 'lasting');
}

// keepRedeemed keeps the credential an invite, a sign-in link, a join or a
// sign-in gave at place: a member's to last, as signIn keeps one, and a
// guest's for this tab; and pins it. Then whoami says who the page now is,
// which must be the principal the answer named.
async function keepRedeemed(token: string, principal: number, place: Place): Promise<RedeemResult> {
  const held: Held = { token, place };
  if (!writeToken(held.token, held.place)) {
    // The credential works, but cannot be kept here: it is revoked (if the
    // server answers) rather than left alive.
    await sendAs(held, 'DELETE', '/v1/credentials/current');
    return { ok: false, failure: 'unkept' };
  }
  pinned = { token: held.token, place: held.place };
  const who = await whoAmI();
  if (!who.ok) return { ok: false, failure: 'unchecked' };
  if (who.value.role === 'operator' || who.value.id !== principal || (place === 'lasting' && visitOf(who.value) !== null)) {
    // None of these is for an operator, and a guest's is never kept to
    // last. Should the server ever say otherwise, or name someone else, the
    // credential is revoked on the server (best effort, as when it cannot
    // be kept) and not left in this browser.
    await sendAs(held, 'DELETE', '/v1/credentials/current');
    forget(held);
    return { ok: false, failure: 'invalid' };
  }
  return who;
}

// What a password sign-in gave besides an answer: held and stored as signIn's
// (another sign-in is held here); totp is a right password for an account
// that also needs a code, which this page does not take yet (#714).
export type PasswordSignInResult = RedeemResult | { readonly ok: false; readonly failure: 'stored' } | { readonly ok: false; readonly failure: 'totp' };

// signInWithPassword signs a returning member in with a name and a password
// (POST /v1/login, design §3.2). The credential is kept to last, as one an
// invite gave. It is refused, before anything is sent, while this browser
// holds another sign-in, for the reasons signIn is. Every wrong name or
// password is the server's one answer, auth_failed; the page says no more.
export async function signInWithPassword(name: string, password: string): Promise<PasswordSignInResult> {
  if (unchecked !== null && !stillStored(unchecked)) unchecked = null;
  if (pending !== null || unchecked !== null) return { ok: false, failure: 'held' };
  if (hasToken()) return { ok: false, failure: 'stored' };
  pinned = null;
  const request: LoginRequestV1 = { name, password };
  const res = await sendOpen('/v1/login', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const answer = await readBody(res, loginResponseV1);
  if (!answer.ok) return answer;
  if (answer.value.next === 'totp') return { ok: false, failure: 'totp' };
  return keepRedeemed(answer.value.token, answer.value.principal.id, 'lasting');
}

// Ending a visit and making it an account (issue #713, design §3.3, §3.4).

// endVisit ends this tab's guest visit (POST /v1/guest/end). It is ended
// when the server says so: the visit as it now is, or guest_ended (it had
// ended already), or a 401 (its credential no longer works, so neither does
// the visit); then the credential is forgotten here. Anything else is
// unconfirmed, and the credential is kept, so the person can try again:
// forgetting it here would prove nothing about the server, where the visit
// would go on until its expiry. unsent is a tab that no longer holds the
// credential (another tab or the browser removed it): nothing was sent, so
// nothing is claimed.
export async function endVisit(): Promise<'ended' | 'unconfirmed' | 'unsent'> {
  const held = current();
  if (held === null) return 'unsent';
  const reply = await sendAs(held, 'POST', '/v1/guest/end');
  // sendAs has forgotten it on a 401.
  if (reply.kind === 'unauthorized') return 'ended';
  if (reply.kind === 'none') return 'unconfirmed';
  // A success is the server's word that the visit ended, whatever its body
  // turns out to be (EndVisitResponseV1); it is read to its end all the same.
  const ended = reply.res.ok || (await refusalCode(reply.res)) === 'guest_ended';
  if (reply.res.ok) await discard(reply.res);
  if (!ended) return 'unconfirmed';
  forget(held);
  return 'ended';
}

// What Create an account during a visit gave. ok is an account made, its
// credential kept and this tab's guest credential dropped: the page starts
// over as the member. A refusal of the server's leaves the guest as it was.
// gone is a visit that is over: the account was made and its answer lost, or
// the visit ended, so the guest credential no longer works and is dropped;
// either way the person signs in again with the new name and password, if
// the account was made. unconfirmed is no answer whose outcome the page could
// learn: the guest is kept, and the person can try again. unkept is an
// account made whose credential this browser would not keep.
export type ConvertResult =
  | { readonly ok: true }
  | InviteRefusal
  | { readonly ok: false; readonly failure: 'gone' }
  | { readonly ok: false; readonly failure: 'unconfirmed' }
  | { readonly ok: false; readonly failure: 'unkept' };

// convertGuest makes this tab's visit an account, with a permanent name and
// a password (POST /v1/guest/convert). The guest stays what it was until the
// server's answer says the account is made: nothing here changes on a
// refusal or a cancel. When the answer is lost, the guest credential is asked
// about: the conversion revokes it in the transaction that makes the account,
// so a credential that no longer works means the account may be there, and
// password sign-in is the way back in (never the old guest credential).
export async function convertGuest(name: string, password: string): Promise<ConvertResult> {
  const held = current();
  if (held === null) return { ok: false, failure: 'gone' };
  const request: ConvertGuestRequestV1 = { name, password };
  const reply = await sendAs(held, 'POST', '/v1/guest/convert', request);
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'gone' };
  // A 5xx may come from something in front of conchd after it committed.
  if (reply.kind === 'none' || reply.res.status >= 500) return lostConversion(held);
  if (!reply.res.ok) {
    const refusal = await inviteRefused(reply.res);
    // A refusal with no error document of conchd's is from something in
    // front of it (a proxy's 404 or 408), which says nothing of whether the
    // conversion happened: it is asked about as a lost answer is.
    if (refusal.code === undefined) return lostConversion(held);
    if (refusal.code === 'guest_ended') {
      forget(held);
      return { ok: false, failure: 'gone' };
    }
    return refusal;
  }
  let body: unknown;
  try {
    body = await reply.res.json();
  } catch {
    body = undefined;
  }
  if (convertGuestResponseV1(body).length > 0) {
    // A success whose body is not the schema's shape may still carry the
    // member's new credential: it is revoked (best effort) rather than left
    // alive, and the person signs in with the password.
    const stray = tokenIn(body);
    if (stray !== null) await sendAs({ token: stray, place: 'tab' }, 'DELETE', '/v1/credentials/current');
    return lostConversion(held);
  }
  const made = { value: body as ConvertGuestResponseV1 };
  // The guest credential is revoked already, with the visit.
  dropToken(held);
  pinned = null;
  forgetDiagnostics();
  // The member's credential is kept to last, as any member's is, unless
  // another tab keeps a sign-in there (with this tab's gone, hasToken finds
  // only that one): writing over it would leave it alive and unrevoked, so
  // this tab keeps the member's instead.
  const member: Held = { token: made.value.token, place: hasToken() ? 'tab' : 'lasting' };
  if (!writeToken(member.token, member.place)) {
    await sendAs(member, 'DELETE', '/v1/credentials/current');
    return { ok: false, failure: 'unkept' };
  }
  pinned = { token: member.token, place: member.place };
  return { ok: true };
}

// tokenIn is the token field of an answer that is not the shape it should
// be, when it has a string there, else null.
function tokenIn(body: unknown): string | null {
  if (typeof body !== 'object' || body === null || !('token' in body)) return null;
  const token: unknown = body.token;
  return typeof token === 'string' && token !== '' ? token : null;
}

// lostConversion asks whether the guest credential still works, after a
// conversion whose answer was lost.
async function lostConversion(held: Held): Promise<ConvertResult> {
  const who = await get('/v1/whoami', held.token, whoAmIResponseV1);
  if (!who.ok && who.failure === 'unauthorized') {
    forget(held);
    return { ok: false, failure: 'gone' };
  }
  return { ok: false, failure: 'unconfirmed' };
}

// Sign-in links (issue #493). An operator makes one for a person who signed
// out, so they can come back into the same account. Like an invite's, its
// code is the proof: preview and redeem need no credential, and the code
// travels in a POST body only and is kept nowhere.

// previewSignInLink asks whose account a sign-in link opens, without using
// it.
export async function previewSignInLink(signInCode: string): Promise<InviteResult<SignInLinkPreviewV1>> {
  const request: SignInLinkPreviewRequestV1 = { code: signInCode };
  const res = await sendOpen('/v1/sign-in-links/preview', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  return readBody(res, signInLinkPreviewV1);
}

// redeemSignInLink uses a sign-in link: the new credential is kept to last
// and pinned, as an invite's is. It is refused, before anything is sent,
// while this browser holds any other sign-in (heldElsewhere), for the reasons
// redeemInvite is: the page signs that one out first, revoking it.
export async function redeemSignInLink(signInCode: string): Promise<RedeemResult> {
  if (heldElsewhere()) return { ok: false, failure: 'held' };
  pinned = null;
  const request: RedeemSignInLinkRequestV1 = { code: signInCode };
  const res = await sendOpen('/v1/sign-in-links/redeem', request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const redeemed = await readBody(res, redeemSignInLinkResponseV1);
  if (!redeemed.ok) return redeemed;
  return keepRedeemed(redeemed.value.token, redeemed.value.principal.id, 'lasting');
}

// createInvite makes an invite. The answer carries its code, shown once to
// whoever made it and nowhere else.
export async function createInvite(request: CreateInviteRequestV1): Promise<InviteResult<CreateInviteResponseV1>> {
  const reply = await authedPost('/v1/invites', request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  return readBody(reply.res, createInviteResponseV1);
}

// listInvites reads the invites the caller may see, newest first. For an
// operator that may be everyone's (issue #438): the panel shows only the
// caller's own.
export async function listInvites(): Promise<ApiResult<InviteV1[]>> {
  const result = await authed('/v1/invites', listInvitesResponseV1);
  return result.ok ? { ok: true, value: result.value.invites } : result;
}

// closeInvite closes an invite, so its link stops working. Uses already made
// stand.
export async function closeInvite(id: number): Promise<InviteResult<null>> {
  const reply = await authedSend('DELETE', `/v1/invites/${String(id)}`);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  return { ok: true, value: null };
}

// onSignedOutElsewhere calls back when another tab of this origin removes the
// lasting credential this page signed in with (it signed out, or was refused)
// or replaces it with another person's. A tab on its own operator credential
// is not affected. The browser fires the storage event only in the other tabs,
// never in the one that made the change. It returns the function that stops
// listening.
export function onSignedOutElsewhere(callback: () => void): () => void {
  const listener = (event: StorageEvent) => {
    // key null is the whole storage cleared.
    if (event.key !== null && event.key !== tokenKey) return;
    let lasting: boolean;
    try {
      lasting = event.storageArea === window.localStorage;
    } catch {
      return;
    }
    if (!lasting || pinned?.place !== 'lasting') return;
    // Still the credential this page signed in with: nothing changed for it.
    if (current() !== null) return;
    forgetDiagnostics();
    callback();
  };
  window.addEventListener('storage', listener);
  return () => {
    window.removeEventListener('storage', listener);
  };
}

// renamePrincipal changes a principal's name (issue #441): the caller's own,
// or anyone's for an operator. A refusal carries the server's code:
// principal_exists (the name, or one that looks the same, is taken),
// rate_limited (too many renames today) or invalid_request (not a valid name).
export async function renamePrincipal(id: number, name: string): Promise<ApiResult<PrincipalV0>> {
  const request: RenamePrincipalRequestV1 = { name };
  const reply = await authedSend('PUT', `/v1/principals/${String(id)}/name`, request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  try {
    const parsed: unknown = await reply.res.json();
    if (renamePrincipalResponseV1(parsed).length === 0) return { ok: true, value: (parsed as RenamePrincipalResponseV1).principal };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// listChannels reads the channels the caller can use. A 401 drops the
// credential, so the caller only has to go back to sign-in.
export async function listChannels(): Promise<ApiResult<ChannelV0[]>> {
  const result = await authed('/v1/channels', listChannelsResponse);
  if (!result.ok) return result;
  return { ok: true, value: result.value.channels };
}

// createChannel makes a channel (issue #695). The server allows operators
// alone (403 forbidden for anyone else) and makes the caller its only member,
// so it is one of theirs from then on. A refusal carries the server's code and
// line as an invite call's does: channel_exists for a name already used,
// invalid_request with the rule a name broke. It is never repeated here.
export async function createChannel(name: string): Promise<InviteResult<ChannelV0>> {
  const request: CreateChannelRequest = { name };
  const reply = await authedPost('/v0/channels', request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  const created = await readBody(reply.res, createChannelResponse);
  return created.ok ? { ok: true, value: created.value.channel } : created;
}

// renameChannel renames a channel by its id (issue #728): operators alone
// (403 forbidden for anyone else). from is the name the caller's list shows
// now; the server refuses the rename when that is no longer the channel's name
// (channel_name_stale), so two operators cannot rename past each other. Other
// refusals carry their code and line as createChannel's do: channel_exists for
// a name taken (or one that reads the same), invalid_request with the rule a
// name broke. The answer is the channel as it now is. It is never repeated
// here.
export async function renameChannel(id: number, from: string, name: string): Promise<InviteResult<ChannelV0>> {
  const request: RenameChannelRequestV1 = { name, from };
  const reply = await authedSend('PUT', `/v1/channels/${String(id)}/name`, request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  const renamed = await readBody(reply.res, renameChannelResponseV1);
  return renamed.ok ? { ok: true, value: renamed.value.channel } : renamed;
}

// How every channel-addressed route answers a name that is not a channel of
// the caller's: unknown, not theirs, or renamed since the page read the list
// (issue #728). The page then reads the list again to tell which.
export const channelNotFound = 'channel_not_found';

// A channel name goes in the path as one segment.
const channelPath = (channel: string) => `/v1/channels/${encodeURIComponent(channel)}`;

// A page of history is the most the server returns for one call, and the
// size the client asks for. A page that comes back shorter is the last one.
export const historyLimit = 100;

// loadMessages reads one page of a channel's history: the newest page, or with
// `before` the page just below that id. The server answers newest first
// (`order=desc`; #334) and this hands it back in id order, so opening a channel
// is one request however long its history is.
export async function loadMessages(channel: string, before?: number): Promise<ApiResult<MessageV2[]>> {
  const cursor = before === undefined ? '' : `&before=${String(before)}`;
  const path = `/v2/channels/${encodeURIComponent(channel)}/messages?order=desc&limit=${String(historyLimit)}${cursor}`;
  const result = await authed(path, listMessagesResponseV2);
  if (!result.ok) return result;
  return { ok: true, value: [...result.value.messages].reverse() };
}

export async function listMembers(channel: string): Promise<ApiResult<ChannelMemberV1[]>> {
  const result = await authed(`${channelPath(channel)}/members`, listChannelMembersResponseV1);
  return result.ok ? { ok: true, value: result.value.members } : result;
}

export async function listNets(channel: string): Promise<ApiResult<NetV1[]>> {
  const result = await authed(`${channelPath(channel)}/nets`, listNetsResponseV1);
  return result.ok ? { ok: true, value: result.value.nets } : result;
}

// What happened to a post. refused is an answer saying no (nothing was
// stored); unconfirmed is no answer, or one that cannot be trusted, so the
// message may or may not be there.
export type PostResult =
  | { readonly ok: true; readonly message: MessageV2 }
  | { readonly ok: false; readonly outcome: 'unauthorized' | 'unconfirmed' }
  // reason is the server's own line for the refusal, one line, or a plain
  // one of ours when the answer carried none; code is its code, when it had
  // one.
  | { readonly ok: false; readonly outcome: 'refused'; readonly reason: string; readonly code?: string };

// postRefusal is the line and the code of the error document a refusal
// carries, the line on one line, or a fallback.
async function postRefusal(res: Response): Promise<{ readonly reason: string; readonly code?: string }> {
  try {
    const body: unknown = await res.json();
    if (errorV0(body).length === 0) {
      const doc = body as ErrorV0;
      const line = doc.message.replace(/\s+/g, ' ').trim();
      const reason = line === '' ? 'The server refused the message' : line.length > 300 ? `${line.slice(0, 299)}…` : line;
      return { reason, code: doc.code };
    }
  } catch {
    // No document, or not one.
  }
  return { reason: 'The server refused the message' };
}

// postMessage sends one message, to the whole channel or, with an audience, to
// a net or a list of principals. It is never repeated here: sending again is
// the caller's, on a click.
export async function postMessage(channel: string, body: string, audience?: Audience): Promise<PostResult> {
  const request: PostMessageRequestV2 = audience === undefined ? { body } : { body, audience };
  const reply = await authedPost(`/v2/channels/${encodeURIComponent(channel)}/messages`, request);
  if (reply.kind === 'none') return { ok: false, outcome: 'unconfirmed' };
  if (reply.kind === 'unauthorized') return { ok: false, outcome: 'unauthorized' };
  const res = reply.res;
  // A 5xx may come from something in front of conchd after it stored the message.
  if (res.status >= 500) return { ok: false, outcome: 'unconfirmed' };
  if (!res.ok) return { ok: false, outcome: 'refused', ...(await postRefusal(res)) };
  try {
    const parsed: unknown = await res.json();
    if (postMessageResponseV2(parsed).length === 0) return { ok: true, message: (parsed as { message: MessageV2 }).message };
  } catch {
    // Fall through: the body was cut off.
  }
  return { ok: false, outcome: 'unconfirmed' };
}

// Read state (UX-02). Both calls are for people: an agent credential, or
// conchd with authentication off, is refused with one of the codes below and
// the caller then asks for nothing more.
export const readStateRefusals: readonly string[] = ['read_state_require_auth', 'human_required'];

// listReadState reads the caller's mark and unread count in every channel they
// are in, by channel id.
export async function listReadState(): Promise<ApiResult<ReadStateV1[]>> {
  const result = await authed('/v1/read-state', listReadStateResponseV1);
  return result.ok ? { ok: true, value: result.value.read_state } : result;
}

// markRead asks for the caller's mark in a channel to move to message `id`. The
// answer is the state as stored: the mark only moves forward, so it may be
// ahead of `id`.
export async function markRead(channel: string, id: number): Promise<ApiResult<ReadStateV1>> {
  const request: MarkReadRequestV1 = { last_read_id: id };
  const reply = await authedSend('PUT', `${channelPath(channel)}/read-state`, request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  try {
    const parsed: unknown = await reply.res.json();
    if (readStateV1(parsed).length === 0) return { ok: true, value: parsed as ReadStateV1 };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// requestTicket asks for a single-use ticket to open a socket with.
async function requestTicket(): Promise<ApiResult<WSTicketV1>> {
  const reply = await authedPost('/v1/ws-tickets');
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return { ok: false, failure: 'invalid' };
  try {
    const parsed: unknown = await reply.res.json();
    if (wsTicketV1(parsed).length === 0) return { ok: true, value: parsed as WSTicketV1 };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// socketUrl is built here from the page's own origin, so the socket is
// same-origin (connect-src 'self'), and it is handed to the WebSocket and to
// nothing else: never to location, history or the DOM, because it carries the
// ticket.
function socketUrl(path: string, query: Record<string, string>): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${window.location.host}${path}?${new URLSearchParams(query).toString()}`;
}

// The page's sockets. The occupancy socket (issue #596) is a voice socket to
// the diagnostic ring, whose vocabulary has no word of its own for it, but it
// keeps its own count of tries, so a presence socket's losses are not added
// to it.
type SocketName = DiagnosticSocket | 'occupancy';
const diagnosticOf: Record<SocketName, DiagnosticSocket> = { messages: 'messages', events: 'events', voice: 'voice', occupancy: 'voice' };

// How many times each socket has been tried since it was last open, for the
// diagnostic ring: 0 while it is open or was never lost.
const retries: Record<SocketName, number> = { messages: 0, events: 0, voice: 0, occupancy: 0 };
const lost: Record<SocketName, boolean> = { messages: false, events: false, voice: false, occupancy: false };

// connect asks for a ticket with the credential and opens the socket that
// `address` names, given the ticket. The ticket is single use, so a reconnect
// calls this again. The diagnostic ring is told of each open, each close with
// its code and each try after a loss, by kind, never by address: the
// address holds the channel and the ticket.
async function connect(name: SocketName, address: (ticket: string) => string, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  const kind = diagnosticOf[name];
  if (lost[name]) record({ kind: 'reconnect', socket: kind, attempt: ++retries[name] });
  // Lost until it opens: a ticket refused or a socket that never opens is
  // followed by another try.
  lost[name] = true;
  const ticket = await requestTicket();
  if (!ticket.ok) return ticket;
  let socket: WebSocket;
  try {
    socket = new WebSocket(address(ticket.value.ticket));
  } catch {
    return { ok: false, failure: 'network' };
  }
  socket.onopen = () => {
    lost[name] = false;
    retries[name] = 0;
    record({ kind: 'socket_open', socket: kind });
    events.onOpen();
  };
  socket.onmessage = (event: MessageEvent) => {
    events.onFrame(event.data);
  };
  // An error is always followed by a close, which is what the feed acts on.
  socket.onclose = (event: CloseEvent) => {
    lost[name] = true;
    record({ kind: 'socket_close', socket: kind, code: event.code });
    events.onClose(event.code, event.reason);
  };
  const handle: FeedSocket = {
    close: () => {
      socket.onopen = null;
      socket.onmessage = null;
      socket.onclose = null;
      socket.close();
    },
  };
  return { ok: true, value: handle };
}

// openSocket opens the message stream, replaying everything after `after`.
// heartbeat=1 asks for the server's heartbeat frames, which the feed's silence
// watchdog lives on; without it /v2/ws sends messages and nothing else.
export const openSocket: OpenSocket = (channel, after, events) =>
  connect('messages', (ticket) => socketUrl('/v2/ws', { channel, after: String(after), heartbeat: '1', ticket }), events);

// openEventsSocket opens the events stream of a channel (roster, runs,
// activity, interventions) with the browser's own session, which the server
// then keeps online while the socket is open.
export function openEventsSocket(channel: string, session: number, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  return connect('events', (ticket) => socketUrl('/v1/events/ws', { channel, session: String(session), ticket }), events);
}

// The browser's own session (UX-18): created after sign-in, ended on sign-out
// and when the page goes away.

// createSession registers the browser as a client of the signed-in principal.
export async function createSession(client: string): Promise<ApiResult<SessionV1>> {
  const request: CreateSessionRequestV1 = { client };
  const reply = await authedPost('/v1/sessions', request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  try {
    const parsed: unknown = await reply.res.json();
    if (sessionV1(parsed).length === 0) return { ok: true, value: parsed as SessionV1 };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// endSession ends the browser's own session. It says nothing about how it
// went: if the request is lost, the server marks the session offline itself.
// It must be called while the credential is still held, and sign-out waits for
// the promise before it revokes: revoking ends the credential's sessions, so a
// request still in flight would find its session gone and be answered 404.
export function endSession(id: number): Promise<void> {
  return authedSend('DELETE', `/v1/sessions/${String(id)}`, undefined, true).then(() => undefined);
}

// Runs, their activity and their interventions.

// listSessionRuns reads the newest runs of one session in a channel, newest
// first, with one request (the server's order=desc, issue #317).
export async function listSessionRuns(channel: string, session: number, limit: number): Promise<ApiResult<RunV1[]>> {
  const query = new URLSearchParams({ channel, session: String(session), order: 'desc', limit: String(limit) });
  const result = await authed(`/v1/runs?${query.toString()}`, listRunsResponseV1);
  return result.ok ? { ok: true, value: result.value.runs } : result;
}

// listChannelRuns reads one page of a channel's runs, newest first: without
// `before` the newest, with it the page just below that id (the server's
// order=desc and before, issue #317). Every run the caller may see is listed.
export async function listChannelRuns(channel: string, limit: number, before?: number): Promise<ApiResult<RunV1[]>> {
  const query = new URLSearchParams({ channel, order: 'desc', limit: String(limit) });
  if (before !== undefined && before > 0) query.set('before', String(before));
  const result = await authed(`/v1/runs?${query.toString()}`, listRunsResponseV1);
  return result.ok ? { ok: true, value: result.value.runs } : result;
}

export function getRun(id: number): Promise<ApiResult<RunV1>> {
  return authed(`/v1/runs/${String(id)}`, runV1);
}

// The most pages one read follows; past it the newest entries are what is
// missing from the end, and the live frames bring them.
const maxPages = 100;

// listActivity reads a run's activity after the entry `after`, oldest first.
export async function listActivity(run: number, after: number): Promise<ApiResult<AgentActivityV1[]>> {
  const all: AgentActivityV1[] = [];
  let from = after;
  for (let page = 0; page < maxPages; page++) {
    const path = `/v1/runs/${String(run)}/activity?after=${String(from)}&limit=${String(historyLimit)}`;
    const result = await authed(path, listActivityResponseV1);
    if (!result.ok) return result;
    all.push(...result.value.activity);
    const next = result.value.next_after ?? 0;
    if (next <= from || result.value.activity.length === 0) break;
    from = next;
  }
  return { ok: true, value: all };
}

// listInterventions reads every intervention of a run, oldest first. The
// response has no cursor: a page as long as the limit may have a successor.
export async function listInterventions(run: number): Promise<ApiResult<InterventionV1[]>> {
  const all: InterventionV1[] = [];
  let after = 0;
  for (let page = 0; page < maxPages; page++) {
    const path = `/v1/runs/${String(run)}/interventions?after=${String(after)}&limit=${String(historyLimit)}`;
    const result = await authed(path, listInterventionsResponseV1);
    if (!result.ok) return result;
    all.push(...result.value.interventions);
    const last = result.value.interventions[result.value.interventions.length - 1];
    if (result.value.interventions.length < historyLimit || last === undefined) break;
    after = last.id;
  }
  return { ok: true, value: all };
}

// createIntervention asks a run to take a message or to stop. The answer is
// the intervention as stored, "requested"; it is never repeated here.
export async function createIntervention(run: number, request: CreateInterventionRequestV1): Promise<ApiResult<InterventionV1>> {
  const reply = await authedPost(`/v1/runs/${String(run)}/interventions`, request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  try {
    const parsed: unknown = await reply.res.json();
    if (interventionV1(parsed).length === 0) return { ok: true, value: parsed as InterventionV1 };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// Voice (issue #442). The join token in a session is handed to the caller and
// kept nowhere here: not stored, not logged, not put in the DOM.

// A refused session carries the status and the server's code, so the voice
// client can tell "stop" from "try again" (conch-voice.md section 5).
export type VoiceSessionResult =
  | { readonly ok: true; readonly value: VoiceSessionResponseV1 }
  | { readonly ok: false; readonly failure: ApiFailure; readonly status?: number; readonly code?: string };

// createVoiceSession asks for the session that admits the caller to a
// channel's voice room. It is asked for before every connection attempt.
export async function createVoiceSession(channel: string): Promise<VoiceSessionResult> {
  const reply = await authedPost(`${channelPath(channel)}/voice/session`);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  const res = reply.res;
  if (!res.ok) {
    const code = await refusalCode(res);
    return code === undefined ? { ok: false, failure: 'invalid', status: res.status } : { ok: false, failure: 'invalid', status: res.status, code };
  }
  try {
    const parsed: unknown = await res.json();
    if (voiceSessionResponseV1(parsed).length === 0) return { ok: true, value: parsed as VoiceSessionResponseV1 };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// getVoicePresence reads who is in a channel's voice, once. The page keeps
// presence on its socket, joined or not, and reads this only to learn why
// the server closed that socket (1008).
export function getVoicePresence(channel: string): Promise<ApiResult<VoicePresenceV1>> {
  return authed(`${channelPath(channel)}/voice`, voicePresenceV1);
}

// How a transmit report was answered. no_session is "go back to conchd" (409
// voice_no_session, or a refusal that says the caller is not entitled any
// more); rate_limited is 429; failed is no answer or a 5xx, which may be
// retried.
export type TransmitAnswer = 'ok' | 'no_session' | 'rate_limited' | 'unauthorized' | 'failed';

// reportVoiceTransmit sends one press or release. It never repeats itself; the
// caller sends reports one at a time and decides about a retry. The signal
// ends a request that has outlived its usefulness, so it cannot be delivered
// late.
export async function reportVoiceTransmit(channel: string, state: VoiceTransmitState, signal: AbortSignal): Promise<TransmitAnswer> {
  const report: VoiceTransmitReportV1 = { state };
  const reply = await authedSend('POST', `${channelPath(channel)}/voice/transmit`, report, false, signal);
  if (reply.kind === 'none') return 'failed';
  if (reply.kind === 'unauthorized') return 'unauthorized';
  const status = reply.res.status;
  if (status >= 200 && status < 300) return 'ok';
  if (status === 429) return 'rate_limited';
  if (status >= 500) return 'failed';
  return 'no_session';
}

// openVoiceSocket opens the presence stream of a channel's voice with a ticket:
// the roster, and who is transmitting.
export function openVoiceSocket(channel: string, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  return connect('voice', (ticket) => socketUrl('/v1/voice/ws', { channel, ticket }), events);
}

// Voice occupancy (issue #596): how many are in each channel's voice, for
// every channel the caller may see voice in. Counts only, never names; the
// page holds one socket of it, not one per channel.

// getVoiceOccupancy reads it once. The page keeps it on its socket, and reads
// this only to learn why the server closed that socket (1008).
export function getVoiceOccupancy(): Promise<ApiResult<VoiceOccupancyV1>> {
  return authed('/v1/voice/occupancy', voiceOccupancyV1);
}

// openOccupancySocket opens its stream with a ticket: the whole document on
// connect, and again whenever a count changes.
export function openOccupancySocket(events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  return connect('occupancy', (ticket) => socketUrl('/v1/voice/occupancy/ws', { ticket }), events);
}

// sendReport sends a problem report (issue #446) to the server's operator.
// The answer is its id. A refusal says why as an invite call's does: the
// server's code and line, and for rate_limited the seconds to wait.
export async function sendReport(request: CreateProblemReportRequestV1): Promise<InviteResult<CreateProblemReportResponseV1>> {
  const reply = await authedPost('/v1/reports', request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  return readBody(reply.res, createProblemReportResponseV1);
}

// Screen share (issue #444, voice-control-plane.md section 12). The page
// connects to the room first, then claims the room's share, then publishes:
// the join token allows the microphone alone, and the claim is what lets that
// connection publish a screen and its sound.

// A claim refused carries the status and the server's code, as a session
// does: 409 voice_share_busy is someone else's share, 409 voice_no_session is
// no connection to grant it to.
export type ShareClaimResult =
  | { readonly ok: true; readonly value: VoiceShareV1 }
  | { readonly ok: false; readonly failure: ApiFailure; readonly status?: number; readonly code?: string };

// claimVoiceShare claims the room's share for the caller, or claims it again
// (after a reconnect), which keeps the share as it was.
export async function claimVoiceShare(channel: string): Promise<ShareClaimResult> {
  const reply = await authedPost(`${channelPath(channel)}/voice/share`);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  const res = reply.res;
  if (!res.ok) {
    const code = await refusalCode(res);
    return code === undefined ? { ok: false, failure: 'invalid', status: res.status } : { ok: false, failure: 'invalid', status: res.status, code };
  }
  try {
    const parsed: unknown = await res.json();
    if (voiceShareV1(parsed).length === 0) return { ok: true, value: parsed as VoiceShareV1 };
  } catch {
    // Fall through.
  }
  return { ok: false, failure: 'invalid' };
}

// stopVoiceShare ends the caller's share. A share nobody holds is 204 too, so
// stopping twice is harmless.
//
// startedAt names the share meant (issue #487): the started_at of the
// caller's own claim. conchd then ends it only while it is still the room's
// share and the caller's, and otherwise answers 409 voice_share_stale and
// changes nothing, so a release sent late never ends somebody's newer share.
// The parameter is schema.VoiceShareClaimParam.
export async function stopVoiceShare(channel: string, startedAt?: string): Promise<ApiResult<null>> {
  const query = startedAt === undefined ? '' : `?${new URLSearchParams({ started_at: startedAt }).toString()}`;
  const reply = await authedSend('DELETE', `${channelPath(channel)}/voice/share${query}`);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  return { ok: true, value: null };
}
