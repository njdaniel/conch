// The only module that holds the credential in use, and the only one that
// asks conchd anything. Where the token lives depends on whose it is (issue
// #439, decision O1). A member's lasts: it is kept so that it survives closing
// the browser and every tab of the origin shares it, until it is signed out or
// stops working. An operator's stays with the one tab that agreed to use it
// (decision D2, kept for operators). A temporary guest's is kept for its one
// tab too (issue #713, ADR-009 D2): a refresh keeps the visit, another tab or
// another browser does not have it, and it is never kept to last. Closing a
// tab is no reliable end of a visit (a browser may restore its tabs, storage
// and all): End visit is, and the server's expiry bounds the rest.
//
// Where it is kept, and how a request reaches conchd, belong to the platform
// (src/platform, issue #770): the browser's is src/platform/browser, which
// alone touches web storage, fetch and WebSocket; every request is a call of
// the operation table (src/platform/operations.ts). The token is sent only as
// the Authorization header, and never put in a URL, a log call, the DOM or an
// error: a failure is reported as a kind, never with the request in it.
import { listActivityResponseV1, type AgentActivityV1 } from './schema/activity';
import { listOwnCredentialsResponseV1, revokeAllCredentialsResponseV1, type OwnCredentialV1 } from './schema/credential';
import { errorV0, type ErrorV0 } from './schema/error';
import {
  loginResponseV1,
  passwordUpdatedResponseV1,
  reauthResponseV1,
  redeemSetupTokenResponseV1,
  totpEnrollmentV1,
  type AuthStateV1,
  type ChangePasswordRequestV1,
  type ConfirmTOTPRequestV1,
  type LoginRequestV1,
  type PasswordUpdatedResponseV1,
  type ReauthRequestV1,
  type ReauthResponseV1,
  type RedeemSetupTokenRequestV1,
  type ResetPasswordRequestV1,
  type SetPasswordRequestV1,
  type TOTPEnrollmentV1,
  type TOTPLoginRequestV1,
} from './schema/auth';
import {
  convertGuestResponseV1,
  endGuestsResponseV1,
  joinAccountResponseV1,
  joinInfoV1,
  joinSettingsResponseV1,
  listGuestsResponseV1,
  type ConvertGuestRequestV1,
  type ConvertGuestResponseV1,
  type EndGuestsRequestV1,
  type EndGuestsResponseV1,
  type GuestAdminV1,
  type JoinAccountRequestV1,
  type JoinInfoV1,
  type JoinSettingsResponseV1,
  type PutJoinSettingsRequestV1,
} from './schema/guest';
import { health } from './schema/health';
import {
  createInviteResponseV1,
  inviteSettingsV1,
  invitePreviewV1,
  listInvitesResponseV1,
  redeemInviteResponseV1,
  type CreateInviteRequestV1,
  type CreateInviteResponseV1,
  type InvitePreviewRequestV1,
  type InvitePreviewV1,
  type InviteSettingsV1,
  type InviteV1,
  type RedeemInviteRequestV2,
} from './schema/invite';
import { interventionV1, listInterventionsResponseV1, type CreateInterventionRequestV1, type InterventionV1 } from './schema/intervention';
import { listRunsResponseV1, runV1, type RunV1 } from './schema/run';
import { sessionV1, type CreateSessionRequestV1, type SessionV1 } from './schema/session';
import {
  channelMemberV1,
  createChannelResponse,
  listChannelMembersResponseV1,
  listChannelsResponse,
  renameChannelResponseV1,
  type AddChannelMemberRequestV1,
  type ChannelMemberV1,
  type ChannelV0,
  type CreateChannelRequest,
  type PutChannelMemberRequestV1,
  type RenameChannelRequestV1,
} from './schema/channel';
import { fileAttachmentPayloadSchema, fileV1, type FileAttachmentV1, type FileV1 } from './schema/file';
import {
  editMessageResponseV1,
  listMessageRevisionsResponseV1,
  listMessagesResponseV2,
  postMessageResponseV2,
  searchMessagesResponseV2,
  type Audience,
  type EditMessageRequestV1,
  type EditMessageResponseV1,
  type ListMessagesResponseV2,
  type MessageRevisionV1,
  type MessageV2,
  type Payload,
  type PostMessageRequestV2,
  type SearchMessagesResponseV2,
} from './schema/message';
import { listNetsResponseV1, type NetV1 } from './schema/net';
import {
  createSignInLinkResponseV1,
  listOwnSignInLinksResponseV1,
  redeemSignInLinkResponseV1,
  signInLinkPreviewV1,
  type CreateDeviceLinkRequestV1,
  type RedeemSignInLinkRequestV1,
  type SignInLinkPreviewRequestV1,
  type SignInLinkPreviewV1,
  type SignInLinkV1,
} from './schema/signin';
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
import { call, fileQuery, operations, queryOf, type Call, type OperationId } from './platform/operations';
import { platform } from './platform';
import type { FeedConnection, Held, Place } from './platform/types';
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

// Where a credential is kept is the platform's (platform.credentials): 'tab'
// is this tab alone, for an operator's and a guest's; 'lasting' is every tab
// of the origin until it is signed out, for a member's. A Held is the
// credential this tab uses, and where it was found, so that what drops it
// drops it from there.

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

// A write that replaces the credential it is sent with (a password write
// rotates it; a guest's conversion makes it a member's) races every other
// call sent with that credential: the server revokes it as it answers, so a
// call already out with it, or sent before the answer is read, comes back 401
// although the person is still signed in, with the new one (issue #1000).
//
// replacing holds, by the credential being replaced, when its write has been
// answered and the new credential kept (or not). replaced holds the
// credentials this page has replaced so, since it last signed in: a 401 for
// one of them is sent again with the credential pinned now, which is the
// same person's. Every other way the pin changes (a sign-in, a sign-out, a
// 401) empties it, so a call is never sent again as someone else.
const replacing = new Map<string, Promise<void>>();
const replaced = new Set<string>();

// pin sets the credential this page signs in with, as a new sign-in (or none).
function pin(held: Held | null): void {
  pinned = held;
  replaced.clear();
}

// rotate pins next in the place of from, which the server has revoked: the
// same sign-in, under a new credential.
function rotate(from: Held, next: Held): void {
  pinned = next;
  replaced.add(from.token);
}

// whileReplacing runs work, a write that may replace from, so that a 401 for
// from meanwhile waits for its answer (see settleRefusal). It is in force
// before work sends anything.
function whileReplacing<T>(from: Held | null, work: () => Promise<T>): Promise<T> {
  if (from === null) return work();
  const done = Promise.resolve().then(work);
  const settled = done.then(
    () => undefined,
    () => undefined,
  );
  replacing.set(from.token, settled);
  void settled.then(() => {
    if (replacing.get(from.token) === settled) replacing.delete(from.token);
  });
  return done;
}

// settleRefusal decides what a 401 for a call sent with held means. A write
// replacing held is waited for first, unless wait is false: the write's own
// requests, and the proof it may ask for, never wait for themselves. Then:
// held still the pinned credential (or none pinned) is a credential refused,
// so it is forgotten and the page signs out (null); held replaced by the
// credential pinned now is the same person, who is to be asked again with it;
// held from a sign-in that is over is 'stale': no answer for this page, and
// never a sign-out of the one now signed in.
async function settleRefusal(held: Held, wait: boolean): Promise<Held | 'stale' | null> {
  const change = wait ? replacing.get(held.token) : undefined;
  if (change !== undefined) await within(change, replacingWait);
  const now = current();
  if (now === null || now.token === held.token) {
    forget(held);
    return null;
  }
  return replaced.has(held.token) ? now : 'stale';
}

// How many times a call is sent again after a 401 for a credential since
// replaced: once per replacement, and never without end.
const resends = 3;

// How long a 401 waits for a replacement before it is settled as it stands:
// a change held up (an answer that never comes, a proof the person never
// gives) does not hold every refused call up with it.
const replacingWait = 30_000;

// within resolves when change does, or after ms, whichever is first.
function within(change: Promise<void>, ms: number): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, ms);
  });
  return Promise.race([change, late]).finally(() => {
    clearTimeout(timer);
  });
}

// unchecked is the credential restore found stored but could not check (the
// server did not answer): neither pinned nor dropped. Only it may be forgotten
// by forgetStored, and only while it is still the one stored (issue #469).
let unchecked: Held | null = null;

// stillStored says whether held is still the credential stored at its place.
function stillStored(held: Held): boolean {
  return platform.credentials.isStored(held);
}

// current is the pinned credential while it is still the one stored at its
// place, else nothing (and the pin is cleared).
function current(): Held | null {
  const held = pinned;
  if (held === null) return null;
  // A storage that refuses the read is as good as gone.
  if (stillStored(held)) return held;
  pin(null);
  return null;
}

// forget drops the credential held, from memory and from its storage. The
// diagnostic ring goes with it: the session it describes is over, and the next
// person to sign in on this browser must not report the last one's events.
function forget(held: Held): void {
  if (pinned?.token === held.token && pinned.place === held.place) pin(null);
  platform.credentials.drop(held);
  forgetDiagnostics();
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

async function get<T>(request: Call, token: string, check: Validator<T>): Promise<ApiResult<T>> {
  let res: Response;
  try {
    res = await platform.http.get(request, token);
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
// A 401 for a credential this page has since replaced is not a sign-out: the
// call is asked again with the new one (settleRefusal, issue #1000).
// signIn and restore are the calls that do not use it: they check a candidate.
async function authed<T>(request: Call, check: Validator<T>): Promise<ApiResult<T>> {
  let held = current();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  for (let sent = 0; sent < resends; sent++) {
    const result = await get(request, held.token, check);
    if (result.ok || result.failure !== 'unauthorized') return result;
    const next = await settleRefusal(held, true);
    if (next === null) return result;
    if (next === 'stale') break;
    held = next;
  }
  return { ok: false, failure: 'network' };
}

// authedSend is the writing sibling of authed: the stored credential, a JSON
// body (or none), and on a 401 the token dropped. It hands back the response
// itself, because a write must tell "no answer" from "answered no". keepalive
// lets the request outlive the page, for the one sent as the page goes away.
type PostReply =
  | { readonly kind: 'reply'; readonly res: Response }
  | { readonly kind: 'none' }
  | { readonly kind: 'unauthorized' };

// What a write sent with a credential gave, before it is a PostReply:
// replaced is a 401 for a credential this page has since replaced by `by`,
// the same person's (settleRefusal); the server did nothing, since a 401
// comes before a route acts.
type Sent = PostReply | { readonly kind: 'replaced'; readonly by: Held };

//
// A write the server answers 403 reauth_required (design §5.3: it needs a
// recent proof of the password, and an operator's code) is sent once more
// after the page has asked for that proof (onReauthRequired), with the same
// body: the refusal comes before the route does anything, so the second
// send is the first that acts. Once, never more: a second reauth_required
// is the caller's answer, so a server that keeps asking gets one question,
// not one after another (the CLI's rule, #715). Cancelled, the first answer
// stands.
//
// A 401 for a credential since replaced (a password changed meanwhile) is
// sent again with the new one, as authed's is.
async function authedSend(request: Call, body?: unknown, keepalive = false, signal?: AbortSignal): Promise<PostReply> {
  let held = current();
  if (held === null) return { kind: 'unauthorized' };
  for (let sent = 0; sent < resends; sent++) {
    const reply = await sendProving(held, request, body, keepalive, signal);
    if (reply.kind !== 'replaced') return reply;
    // A write that replaces the credential carries a proof made for the
    // one it was sent with (the current password, a one-use code): sent
    // again with another, it would be a wrong password counted against the
    // person. No answer for this one; they can try again.
    if (replacingOps.has(request.op)) return { kind: 'none' };
    held = reply.by;
  }
  return { kind: 'none' };
}

// sendProving is one send of authedSend's with held, and the one more after
// a proof the server asked for.
async function sendProving(held: Held, request: Call, body?: unknown, keepalive = false, signal?: AbortSignal): Promise<Sent> {
  const reply = await sendWith(held, request, body, keepalive, signal);
  if (reply.kind !== 'reply' || reply.res.status !== 403 || askReauth === null) return reply;
  if ((await refusalCode(reply.res.clone())) !== reauthRequired) return reply;
  if (!(await proveAgain())) return reply;
  const again = current();
  if (again === null) return { kind: 'unauthorized' };
  return sendWith(again, request, body, keepalive, signal);
}

// The server's codes the module acts on itself.
const authFailed = 'auth_failed';

// The routes that take a password or a code with a working credential, and
// answer a wrong one 401 auth_failed (see sendAs), are the table's secret ones.
const reauthRequired = 'reauth_required';

// How the page asks the person to prove who they are again (issue #714): it
// shows the question, sends the proof with reauth, and resolves to whether a
// proof now stands. Set by the signed-in page, cleared when it goes.
export type ReauthAsk = () => Promise<boolean>;
let askReauth: ReauthAsk | null = null;
// The question being asked: calls refused together share one.
let asking: Promise<boolean> | null = null;

// onReauthRequired sets how the page asks, or with null stops it asking: a
// call then answered reauth_required is handed back as it came.
export function onReauthRequired(ask: ReauthAsk | null): void {
  askReauth = ask;
}

function proveAgain(): Promise<boolean> {
  const ask = askReauth;
  if (ask === null) return Promise.resolve(false);
  asking ??= ask().finally(() => {
    asking = null;
  });
  return asking;
}

// sendAs is authedSend with the credential already taken, for the sign-outs,
// which must drop the very credential they revoked. It is never sent again
// with another credential: a 401 for one since replaced, or from a sign-in
// that is over, is no answer for this page (settleRefusal), not a sign-out.
async function sendAs(held: Held, request: Call, body?: unknown, keepalive = false, signal?: AbortSignal): Promise<PostReply> {
  const reply = await sendWith(held, request, body, keepalive, signal);
  return reply.kind === 'replaced' ? { kind: 'none' } : reply;
}

// The writes that replace the credential they are sent with, and the proof
// one may ask for on the way (reauth): a 401 for theirs never waits for the
// replacement, which is waiting for them (see whileReplacing), and is never
// sent again with another credential (see authedSend).
const replacingOps: ReadonlySet<OperationId> = new Set<OperationId>(['changePassword', 'setPassword', 'convertGuest', 'reauth']);

// sendWith sends a write with held and reads a 401 as settleRefusal does.
async function sendWith(held: Held, request: Call, body?: unknown, keepalive = false, signal?: AbortSignal): Promise<Sent> {
  let res: Response;
  try {
    res = await platform.http.send(request, held.token, { body, keepalive, ...(signal === undefined ? {} : { signal }) });
  } catch {
    // A request its caller gave up on (a transmit report past its use) is
    // not a failure of the server's.
    if (signal?.aborted !== true) noteNoAnswer();
    return { kind: 'none' };
  }
  if (!res.ok) await noteRefusal(res);
  // A 401 auth_failed is a password or a code that is not right, presented
  // to a route that takes one (a re-authentication, a password change, an
  // authenticator's confirmation; issue #714): the credential still works,
  // and the caller says what was wrong. Only those routes are believed when
  // they say so (secretRoutes); every other 401, auth_failed or not, is the
  // credential refused, which signs out.
  if (res.status === 401 && !(operations[request.op].secret === true && (await refusalCode(res.clone())) === authFailed)) {
    const next = await settleRefusal(held, !replacingOps.has(request.op));
    if (next === null) return { kind: 'unauthorized' };
    return next === 'stale' ? { kind: 'none' } : { kind: 'replaced', by: next };
  }
  // A 204 is a status alone by definition (sign-outs, closes, a transmit
  // report): read it out here so no caller has to remember to (issue #622).
  if (res.status === 204) await discard(res);
  return { kind: 'reply', res };
}

const authedPost = (request: Call, body?: unknown) => authedSend(request, body);

// The version of the server this page was first served by; see clientVersion.
let pageVersion: string | null = null;

// serverVersion reads the server's build version from GET /healthz, which
// needs no credential. It is null when the server cannot be reached or does not
// answer with a health document. A degraded server (503) still names its
// version, so the status is not looked at.
export async function serverVersion(): Promise<string | null> {
  return (await serverHealth())?.version ?? null;
}

// What /healthz says that the page shows: the version, and whether this is a
// temporary server (ADR-011). Only a temporary server carries server_mode.
export interface ServerHealth {
  readonly version: string;
  readonly temporary: boolean;
}

// serverHealth reads GET /healthz; null when the server cannot be reached or
// does not answer with a health document.
export async function serverHealth(): Promise<ServerHealth | null> {
  try {
    const res = await platform.http.health(call('health'));
    const body: unknown = await res.json();
    if (health(body).length > 0) return null;
    const { version, server_mode: mode } = body as { version: string; server_mode?: string };
    pageVersion ??= version;
    return { version, temporary: mode === 'temporary' };
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
  return platform.credentials.read() !== null;
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
  pin(null);
  const result = await get(call('whoami'), candidate, whoAmIResponseV1);
  if (!result.ok) return result;
  // A guest's credential pasted here stays with this tab, as a visit's does.
  const place: Place = visitOf(result.value) === null ? 'lasting' : 'tab';
  if (result.value.role === 'operator') {
    pending = candidate;
    return result;
  }
  const kept = platform.credentials.write(candidate, place);
  // localStorage is refused (blocked site data): signing in would not last
  // a reload, and the lobby could not call the API either.
  if (kept === null) return { ok: false, failure: 'invalid' };
  pin(kept);
  return result;
}

// continueAsOperator keeps the operator credential held by signIn, in this tab
// only.
export function continueAsOperator(): boolean {
  const token = pending;
  pending = null;
  const kept = token === null ? null : platform.credentials.write(token, 'tab');
  if (kept === null) return false;
  pin(kept);
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
  pin(null);
  unchecked = null;
  let held = platform.credentials.read();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const result = await get(call('whoami'), held.token, whoAmIResponseV1);
  if (!result.ok) {
    if (result.failure === 'unauthorized') platform.credentials.drop(held);
    else unchecked = held;
    return result;
  }
  if (held.place === 'lasting' && result.value.role === 'operator') {
    platform.credentials.drop(held);
    pending = held.token;
    return { ok: true, value: { principal: result.value, warn: true } };
  }
  if (held.place === 'lasting' && visitOf(result.value) !== null) {
    // A visit is never kept to last: found there, it is moved to this tab,
    // and the other tabs of the origin no longer have it.
    platform.credentials.drop(held);
    const moved: Held = { token: held.token, place: 'tab' };
    const kept = platform.credentials.write(moved.token, moved.place);
    if (kept === null) {
      // This tab cannot keep it either: it is revoked (if the server
      // answers) rather than left alive and kept nowhere.
      await sendAs(moved, call('logout'));
      return { ok: false, failure: 'unauthorized' };
    }
    held = kept;
  }
  pin(held);
  return { ok: true, value: { principal: result.value, warn: false } };
}

// whoAmI asks who the signed-in credential is, mid-session (after a socket was
// refused, say). Unlike restore it never picks a credential: it asks with the
// one this page signed in with, and a 401 drops it. A socket closed because
// a password change revoked the credential it was opened with asks this
// before the change's answer is read: the 401 for the old credential waits
// for it, and is asked again with the new one (issue #1000).
export function whoAmI(): Promise<ApiResult<WhoAmIResponseV1>> {
  return authed(call('whoami'), whoAmIResponseV1);
}

// signOut revokes the credential this tab uses (DELETE /v1/credentials/current)
// and then forgets it here, whatever the answer. It resolves to whether the
// server was told: true when it revoked the credential, when the credential
// was no longer valid anyway (401), or when none was stored; false when there
// was no answer or a refusal, and the credential may still work elsewhere.
export async function signOut(): Promise<boolean> {
  pending = null;
  const held = current();
  pin(null);
  forgetDiagnostics();
  if (held === null) return true;
  const reply = await sendAs(held, call('logout'));
  platform.credentials.drop(held);
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
  const reply = await sendAs(held, call('revokeAll', { principal }));
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
  const result = await authed(call('listOwnCredentials'), listOwnCredentialsResponseV1);
  return result.ok ? { ok: true, value: result.value.credentials } : result;
}

// endSignIn ends another of the caller's sign-ins by its id, so whatever
// browser or client holds it is refused on its next request. This page's own
// is ended by signOut instead, which also forgets it here. A refusal carries
// the server's code: credential_not_found for an id that is not the caller's.
export async function endSignIn(id: number): Promise<ApiResult<null>> {
  const reply = await authedSend(call('endOwnCredential', { id }));
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  return { ok: true, value: null };
}

// Passwords, the recent proof and Add a device (issue #714, design §5, §6).
// A password or a code goes in a request body alone and is kept nowhere
// here, as the sign-in's is.

// readSent is the answer to a write with the signed-in credential, read as
// an invite call's is: the server's code, its line and its Retry-After on a
// refusal, the schema's shape on a success.
async function readSent<T>(reply: PostReply, check: Validator<T>): Promise<InviteResult<T>> {
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  return readBody(reply.res, check);
}

// readDone is readSent for a write whose answer is its status alone.
async function readDone(reply: PostReply): Promise<InviteResult<null>> {
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  return { ok: true, value: null };
}

// reauth proves the signed-in person's password again (POST /v1/reauth), and
// an operator's code with it, so the session holds a proof for ten minutes.
// It never asks for a proof itself: it is sent once, as it is, and answered.
// A refusal carries the server's code: auth_failed (the password or code is
// not right, which keeps the person signed in), forbidden (a script's
// credential, which can hold no proof), password_not_set, rate_limited.
export async function reauth(password: string, code?: string): Promise<InviteResult<ReauthResponseV1>> {
  const held = current();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const request: ReauthRequestV1 = code === undefined ? { password } : { password, code };
  return readSent(await sendAs(held, call('reauth'), request), reauthResponseV1);
}

// What a password write gave: the answer, and whether this page's credential
// was replaced by the one it carried (a change rotates the session it came
// from). unkept is a new credential this browser would not keep: the old one
// is revoked already, so the person signs in again.
export type PasswordResult = InviteResult<PasswordUpdatedResponseV1> | { readonly ok: false; readonly failure: 'unkept' };

// changePassword replaces the signed-in person's password (PUT
// /v1/password/mine), proving the current one, and an operator's code with
// it. Every other sign-in of theirs ends; this one is rotated, and the new
// credential is kept where the old one was.
export async function changePassword(currentPassword: string, password: string, code?: string): Promise<PasswordResult> {
  const request: ChangePasswordRequestV1 = code === undefined ? { current: currentPassword, password } : { current: currentPassword, password, code };
  const before = current();
  return whileReplacing(before, async () => keepRotated(before, await authedSend(call('changePassword'), request)));
}

// setPassword sets a first password for a member from before passwords
// (POST /v1/password/mine), with the one-use enrollment code an operator
// gave them as the proof: a long-lived credential alone may not set one.
export async function setPassword(password: string, proof: string): Promise<PasswordResult> {
  const request: SetPasswordRequestV1 = { password, proof };
  const before = current();
  return whileReplacing(before, async () => keepRotated(before, await authedSend(call('setPassword'), request)));
}

// keepRotated reads a password write's answer and, when it carries a new
// credential for this page, keeps it in the place of the old one, which the
// server has revoked. A call of this page's that the server refused with the
// old one, meanwhile or after, is sent again with the new one (issue #1000).
//
// before is the credential the write was sent with. When it is no longer
// the one stored (another tab signed out or in meanwhile), the new
// credential has nobody here to hold it: it is revoked (best effort) rather
// than left alive, and this page is signed out, as the other tab chose.
async function keepRotated(before: Held | null, reply: PostReply): Promise<PasswordResult> {
  const result = await readSent(reply, passwordUpdatedResponseV1);
  if (!result.ok || result.value.token === undefined || before === null) return result;
  const next: Held = { token: result.value.token, place: before.place };
  const held = current();
  if (held === null || held.token !== before.token) {
    await sendAs(next, call('logout'));
    return { ok: false, failure: 'unkept' };
  }
  const kept = platform.credentials.write(next.token, next.place);
  if (kept === null) {
    await sendAs(next, call('logout'));
    forget(held);
    return { ok: false, failure: 'unkept' };
  }
  rotate(held, kept);
  return result;
}

// resetPassword replaces a forgotten password with the code of a reset link
// an operator made (POST /v1/password/reset). It needs no credential, and
// signs nobody in: every sign-in of the account ends, and the person signs
// in with the new password.
export async function resetPassword(signInCode: string, password: string): Promise<InviteResult<PasswordUpdatedResponseV1>> {
  const request: ResetPasswordRequestV1 = { code: signInCode, password };
  const res = await sendOpen(call('resetPassword'), request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  return readBody(res, passwordUpdatedResponseV1);
}

// An operator's setup (design §5.3): the host makes a one-use setup token
// (conchd operator bootstrap, setup or recover), and the page spends it for
// a fifteen-minute credential that reaches whoami, the password and the
// authenticator's enrolment, and nothing else. That credential is held here
// in memory only, never stored and never pinned: it signs nobody in, and a
// reload drops it (a new token is made on the host).
let setupHeld: Held | null = null;
// The setup run in force: a number, not a secret. Starting a setup (a token
// redeemed) and ending one (endSetup) each begin a new run, and a redemption
// that answers after its run has ended is discarded, and its credential
// revoked, so a late answer never replaces the credential of the run in force.
let setupRun = 0;

// What the setup credential says of its holder.
export interface SetupState {
  readonly name: string;
  readonly auth: AuthStateV1;
}

// redeemSetupToken spends a setup token (POST /v1/setup/redeem). The token
// goes in the body alone and is kept nowhere.
export async function redeemSetupToken(token: string): Promise<InviteResult<SetupState>> {
  setupHeld = null;
  const run = ++setupRun;
  const request: RedeemSetupTokenRequestV1 = { token };
  const res = await sendOpen(call('setupRedeem'), request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const redeemed = await readBody(res, redeemSetupTokenResponseV1);
  if (!redeemed.ok) return redeemed;
  const held: Held = { token: redeemed.value.token, place: 'tab' };
  if (run !== setupRun) {
    // Answered after its setup ended or another began: nothing here uses
    // this credential, so it is ended on the server (best effort) and
    // forgotten.
    await sendAs(held, call('logout'));
    return { ok: false, failure: 'unauthorized' };
  }
  const { principal, auth } = redeemed.value;
  setupHeld = held;
  return { ok: true, value: { name: principal.name, auth } };
}

// setupState reads the setup credential's whoami again: what is done.
export async function setupState(): Promise<InviteResult<SetupState>> {
  const held = setupHeld;
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const who = await get(call('whoami'), held.token, whoAmIResponseV1);
  if (!who.ok) {
    if (who.failure === 'unauthorized' && setupHeld === held) setupHeld = null;
    return who;
  }
  if (who.value.auth === undefined) return { ok: false, failure: 'invalid' };
  return { ok: true, value: { name: who.value.name, auth: who.value.auth } };
}

// sendSetup is a write with the setup credential. A 401 that is not a wrong
// code means it has ended (its fifteen minutes, or the setup completed).
async function sendSetup(request: Call, body?: unknown): Promise<PostReply> {
  const held = setupHeld;
  if (held === null) return { kind: 'unauthorized' };
  const reply = await sendAs(held, request, body);
  if (reply.kind === 'unauthorized' && setupHeld === held) setupHeld = null;
  return reply;
}

// setupPassword sets the operator's password on the setup credential (POST
// /v1/password/mine), replacing any it had: the host-made token is the proof.
export async function setupPassword(password: string): Promise<InviteResult<PasswordUpdatedResponseV1>> {
  const request: SetPasswordRequestV1 = { password };
  return readSent(await sendSetup(call('setPassword'), request), passwordUpdatedResponseV1);
}

// enrollAuthenticator starts the authenticator's enrolment (POST
// /v1/totp/enroll): a new secret, shown in this answer and never again. The
// caller shows it only when asked to, and never while the tab shares.
export async function enrollAuthenticator(): Promise<InviteResult<TOTPEnrollmentV1>> {
  return readSent(await sendSetup(call('totpEnroll')), totpEnrollmentV1);
}

// confirmAuthenticator proves the enrolment with a code from the app (POST
// /v1/totp/confirm). With the password set, setup is complete and the setup
// credential ends.
export async function confirmAuthenticator(code: string): Promise<InviteResult<null>> {
  const request: ConfirmTOTPRequestV1 = { code };
  return readDone(await sendSetup(call('totpConfirm'), request));
}

// skipAuthenticator ends the setup credential with the authenticator left
// unset (DELETE /v1/credentials/current, which a setup credential may call):
// ADR-012's Skip, offered only where whoami says setup is complete without
// one. The credential is forgotten here once the server has confirmed it
// ended (a 2xx) or says it has ended already (a 401); after a network error
// or any other refusal it is kept, so the person can try again.
export async function skipAuthenticator(): Promise<InviteResult<null>> {
  const held = setupHeld;
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const reply = await sendAs(held, call('logout'));
  if ((reply.kind === 'unauthorized' || (reply.kind === 'reply' && reply.res.ok)) && setupHeld === held) setupHeld = null;
  return readDone(reply);
}

// endSetup forgets the setup credential here. The server ends it at its
// fifteen minutes, or once setup is complete; it can revoke nothing itself.
export function endSetup(): void {
  setupRun++;
  setupHeld = null;
}

// A device link just made: the link as the server keeps it, and its code,
// shown once, to the person who made it, inside the link the page builds.
export interface DeviceLink {
  readonly link: SignInLinkV1;
  readonly linkCode: string;
}

// createDeviceLink makes a one-use link that signs this person's own account
// in on another device (POST /v1/sign-in-links/mine). The server asks for a
// recent proof of the password first (reauth_required), which authedSend
// asks for and then sends again.
export async function createDeviceLink(label: string): Promise<InviteResult<DeviceLink>> {
  const request: CreateDeviceLinkRequestV1 = label === '' ? {} : { label };
  const made = await readSent(await authedSend(call('createSignInLink'), request), createSignInLinkResponseV1);
  if (!made.ok) return made;
  const { sign_in_link: link, code } = made.value;
  return { ok: true, value: { link, linkCode: code } };
}

// listDeviceLinks reads the caller's open device links, newest first,
// without their codes.
export async function listDeviceLinks(): Promise<ApiResult<SignInLinkV1[]>> {
  const result = await authed(call('listSignInLinks'), listOwnSignInLinksResponseV1);
  return result.ok ? { ok: true, value: result.value.sign_in_links } : result;
}

// closeDeviceLink closes one of the caller's device links, so it signs
// nobody in. One used, closed or expired already is a success that changes
// nothing; another's id is sign_in_link_not_found.
export async function closeDeviceLink(id: number): Promise<InviteResult<null>> {
  return readDone(await authedSend(call('closeSignInLink', { id })));
}

// Guest entry, for an operator (issue #714, design §2): the joining
// settings, the live guests and End all guest visits. Changing the settings
// and ending visits need a recent proof (reauth_required), as authedSend
// asks for.

export function joinSettings(): Promise<ApiResult<JoinSettingsResponseV1>> {
  return authed(call('joinSettings'), joinSettingsResponseV1);
}

// putJoinSettings changes the settings it names and no others. A refusal
// carries the server's code: join_policy_conflict for settings that cannot
// stand together.
export async function putJoinSettings(request: PutJoinSettingsRequestV1): Promise<InviteResult<JoinSettingsResponseV1>> {
  return readSent(await authedSend(call('putJoinSettings'), request), joinSettingsResponseV1);
}

// inviteSettings reads whether every invite is paused (GET /v1/invites/settings).
export function inviteSettings(): Promise<ApiResult<InviteSettingsV1>> {
  return authed(call('inviteSettings'), inviteSettingsV1);
}

// putInviteSettings pauses or resumes every invite (PUT /v1/invites/settings),
// as `conch admin invite pause|resume` does, and answers with what the server
// now holds. It needs a recent proof (reauth_required), as authedSend asks
// for; a refusal carries the server's code.
export async function putInviteSettings(request: InviteSettingsV1): Promise<InviteResult<InviteSettingsV1>> {
  return readSent(await authedSend(call('putInviteSettings'), request), inviteSettingsV1);
}

// listGuests reads the live guest visits, newest first: one page.
export async function listGuests(): Promise<ApiResult<GuestAdminV1[]>> {
  const result = await authed(call('listGuests'), listGuestsResponseV1);
  return result.ok ? { ok: true, value: result.value.guests } : result;
}

// endAllGuests ends every live guest visit (POST /v1/guests/end); with dryRun
// it ends none and says which it would. A visit that became an account is
// never one of them.
export async function endAllGuests(dryRun: boolean): Promise<InviteResult<EndGuestsResponseV1>> {
  const request: EndGuestsRequestV1 = { dry_run: dryRun };
  return readSent(await authedSend(call('endGuests'), request), endGuestsResponseV1);
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
async function sendOpen(request: Call, body?: unknown): Promise<Response | null> {
  let res: Response;
  try {
    res = await platform.http.open(request, body);
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
  const res = await sendOpen(call('previewInvite'), request);
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
  const reply = await sendAs(held, call('logout'));
  platform.credentials.drop(held);
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
  pin(null);
  const request: RedeemInviteRequestV2 = { code: inviteCode, name, password };
  const res = await sendOpen(call('redeemInvite'), request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const redeemed = await readBody(res, redeemInviteResponseV1);
  if (!redeemed.ok) return redeemed;
  return keepRedeemed(redeemed.value.token, redeemed.value.principal.id, 'lasting');
}

// Joining from the bare address (issue #713). The front door says what the
// address offers now; the server decides again when someone joins.

// joinInfo reads the front door (GET /v1/join), with no credential.
export async function joinInfo(): Promise<InviteResult<JoinInfoV1>> {
  const res = await sendOpen(call('joinInfo'));
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  return readBody(res, joinInfoV1);
}

// createAccount makes a member from the bare address, name and password
// together (POST /v1/join/account). Its credential is kept to last, as an
// invite's member's is.
export async function createAccount(name: string, password: string): Promise<RedeemResult> {
  if (heldElsewhere()) return { ok: false, failure: 'held' };
  pin(null);
  const request: JoinAccountRequestV1 = { name, password };
  const res = await sendOpen(call('joinAccount'), request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const made = await readBody(res, joinAccountResponseV1);
  if (!made.ok) return made;
  return keepRedeemed(made.value.token, made.value.principal.id, 'lasting');
}

// keepRedeemed keeps the credential an invite, a sign-in link, a join or a
// sign-in gave at place: a member's to last, as signIn keeps one, and a
// guest's for this tab; and pins it. Then whoami says who the page now is,
// which must be the principal the answer named. operator is the one sign-in
// that is an operator's (issue #714): a password and a code, whose session
// is kept for this tab alone, as an operator's credential always is, and
// needs no warning, since it is the operator's sign-in rather than a script's
// credential.
async function keepRedeemed(token: string, principal: number, place: Place, operator = false): Promise<RedeemResult> {
  const made: Held = { token, place: operator ? 'tab' : place };
  const held = platform.credentials.write(made.token, made.place);
  if (held === null) {
    // The credential works, but cannot be kept here: it is revoked (if the
    // server answers) rather than left alive.
    await sendAs(made, call('logout'));
    return { ok: false, failure: 'unkept' };
  }
  pin(held);
  const who = await whoAmI();
  if (!who.ok) return { ok: false, failure: 'unchecked' };
  if ((who.value.role === 'operator') !== operator || who.value.id !== principal || (held.place === 'lasting' && visitOf(who.value) !== null)) {
    // None but the code's step is for an operator, and that one is for an
    // operator alone; a guest's is never kept to last. Should the server
    // ever say otherwise, or name someone else, the
    // credential is revoked on the server (best effort, as when it cannot
    // be kept) and not left in this browser.
    await sendAs(held, call('logout'));
    forget(held);
    return { ok: false, failure: 'invalid' };
  }
  return who;
}

// What a password sign-in gave besides an answer: held and stored as signIn's
// (another sign-in is held here); totp is a right password for an account
// that also needs a code (an operator's, issue #714): nobody is signed in
// yet, and signInWithCode is the second step, until expiresAt.
export type PasswordSignInResult =
  | RedeemResult
  | { readonly ok: false; readonly failure: 'stored' }
  | { readonly ok: false; readonly failure: 'totp'; readonly expiresAt: string };

// The operator's sign-in between its two steps (design §5.3): the challenge
// a right password earned, which signs nobody in, is good for five minutes
// and a few codes, and is bound to this address. It is held here, in memory
// only, like a credential: never stored, rendered or logged, and dropped
// once it is spent, out of time or given up.
interface PendingLogin {
  readonly challenge: string;
  readonly expiresAt: number;
  tries: number;
}
let pendingLogin: PendingLogin | null = null;

// The most codes one challenge takes (pkg/schema
// MaxTOTPAttemptsPerChallenge); the server spends it at the last.
export const codeTries = 5;
// How many digits an authenticator's code has (pkg/schema TOTPDigits).
export const codeDigits = 6;

// signInWithPassword signs a returning member in with a name and a password
// (POST /v1/login, design §3.2). The credential is kept to last, as one an
// invite gave. It is refused, before anything is sent, while this browser
// holds another sign-in, for the reasons signIn is. Every wrong name or
// password is the server's one answer, auth_failed; the page says no more.
export async function signInWithPassword(name: string, password: string): Promise<PasswordSignInResult> {
  if (unchecked !== null && !stillStored(unchecked)) unchecked = null;
  if (pending !== null || unchecked !== null) return { ok: false, failure: 'held' };
  if (hasToken()) return { ok: false, failure: 'stored' };
  pin(null);
  pendingLogin = null;
  const request: LoginRequestV1 = { name, password };
  const res = await sendOpen(call('login'), request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const answer = await readBody(res, loginResponseV1);
  if (!answer.ok) return answer;
  if (answer.value.next === 'totp') {
    const until = Date.parse(answer.value.challenge_expires_at);
    pendingLogin = { challenge: answer.value.challenge, expiresAt: until, tries: 0 };
    return { ok: false, failure: 'totp', expiresAt: answer.value.challenge_expires_at };
  }
  // A session straight from the password is a member's, or an operator's on
  // a server that lets an operator sign in with a password alone (ADR-012).
  // Which one decides where it is kept: an operator's is for this tab alone.
  // So whoami is asked with it before it is stored anywhere; nothing is kept
  // on a refusal, and the credential is then revoked, best effort.
  const token = answer.value.token;
  const who = await get(call('whoami'), token, whoAmIResponseV1);
  if (!who.ok) {
    await sendAs({ token, place: 'tab' }, call('logout'));
    return { ok: false, failure: who.failure === 'network' ? 'network' : 'invalid' };
  }
  return keepRedeemed(token, answer.value.principal.id, 'lasting', who.value.role === 'operator');
}

// What the code's step gave besides an answer: over is a sign-in that has
// to start again with the password, because its challenge ran out of time
// or of tries, or was never there (a reload drops it); nothing was sent for
// one out of time.
export type CodeSignInResult = RedeemResult | { readonly ok: false; readonly failure: 'over' };

// signInWithCode is an operator's second step (POST /v1/login/totp): the
// code from their authenticator app, with the challenge the password
// earned. The session it answers is the operator's and is kept for this tab
// alone (keepRedeemed). A code that is not right, a replayed one included,
// is the server's one answer, auth_failed, and the challenge stays for the
// next try until its tries are spent; any other refusal ends it.
export async function signInWithCode(code: string): Promise<CodeSignInResult> {
  const login = pendingLogin;
  if (login === null || Date.now() >= login.expiresAt) {
    pendingLogin = null;
    return { ok: false, failure: 'over' };
  }
  if (unchecked !== null || pending !== null) {
    pendingLogin = null;
    return { ok: false, failure: 'held' };
  }
  login.tries++;
  const request: TOTPLoginRequestV1 = { challenge: login.challenge, code };
  const res = await sendOpen(call('loginTotp'), request);
  if (res === null) {
    // No answer: the code may or may not have been looked at. The next try
    // is the person's.
    return { ok: false, failure: 'network' };
  }
  if (!res.ok) {
    const refusal = await inviteRefused(res);
    const again = (refusal.code === authFailed || refusal.code === 'rate_limited' || refusal.code === 'invalid_request') && login.tries < codeTries;
    if (!again) pendingLogin = null;
    return refusal.code === authFailed && !again ? { ok: false, failure: 'over' } : refusal;
  }
  pendingLogin = null;
  const answer = await readBody(res, loginResponseV1);
  if (!answer.ok) return answer;
  if (answer.value.next !== 'signed_in') return { ok: false, failure: 'invalid' };
  return keepRedeemed(answer.value.token, answer.value.principal.id, 'tab', true);
}

// cancelCodeSignIn gives up an operator's sign-in between its steps: the
// challenge is forgotten here, and the server lets it expire.
export function cancelCodeSignIn(): void {
  pendingLogin = null;
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
  const reply = await sendAs(held, call('endVisit'));
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
  return whileReplacing(held, () => convertAs(held, name, password));
}

// convertAs is convertGuest with the guest credential taken.
async function convertAs(held: Held, name: string, password: string): Promise<ConvertResult> {
  const request: ConvertGuestRequestV1 = { name, password };
  const reply = await sendAs(held, call('convertGuest'), request);
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
    if (stray !== null) await sendAs({ token: stray, place: 'tab' }, call('logout'));
    return lostConversion(held);
  }
  const made = { value: body as ConvertGuestResponseV1 };
  // The guest credential is revoked already, with the visit.
  platform.credentials.drop(held);
  pin(null);
  forgetDiagnostics();
  // The member's credential is kept to last, as any member's is, unless
  // another tab keeps a sign-in there (with this tab's gone, hasToken finds
  // only that one): writing over it would leave it alive and unrevoked, so
  // this tab keeps the member's instead.
  const member: Held = { token: made.value.token, place: hasToken() ? 'tab' : 'lasting' };
  const kept = platform.credentials.write(member.token, member.place);
  if (kept === null) {
    await sendAs(member, call('logout'));
    return { ok: false, failure: 'unkept' };
  }
  // The same person, as a member now: a call of the guest's that the
  // conversion refused is sent again with the member's credential.
  rotate(held, kept);
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
  const who = await get(call('whoami'), held.token, whoAmIResponseV1);
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
  const res = await sendOpen(call('previewSignInLink'), request);
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
  pin(null);
  const request: RedeemSignInLinkRequestV1 = { code: signInCode };
  const res = await sendOpen(call('redeemSignInLink'), request);
  if (res === null) return { ok: false, failure: 'network' };
  if (!res.ok) return inviteRefused(res);
  const redeemed = await readBody(res, redeemSignInLinkResponseV1);
  if (!redeemed.ok) return redeemed;
  return keepRedeemed(redeemed.value.token, redeemed.value.principal.id, 'lasting');
}

// createInvite makes an invite. The answer carries its code, shown once to
// whoever made it and nowhere else.
export async function createInvite(request: CreateInviteRequestV1): Promise<InviteResult<CreateInviteResponseV1>> {
  const reply = await authedPost(call('createInvite'), request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  return readBody(reply.res, createInviteResponseV1);
}

// listInvites reads the invites the caller may see, newest first. For an
// operator that may be everyone's (issue #438): the panel shows only the
// caller's own.
export async function listInvites(): Promise<ApiResult<InviteV1[]>> {
  const result = await authed(call('listInvites'), listInvitesResponseV1);
  return result.ok ? { ok: true, value: result.value.invites } : result;
}

// closeInvite closes an invite, so its link stops working. Uses already made
// stand.
export async function closeInvite(id: number): Promise<InviteResult<null>> {
  const reply = await authedSend(call('closeInvite', { id }));
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
  return platform.credentials.onLastingChange(() => {
    if (pinned?.place !== 'lasting') return;
    // Still the credential this page signed in with: nothing changed for it.
    if (current() !== null) return;
    forgetDiagnostics();
    callback();
  });
}

// renamePrincipal changes a principal's name (issue #441): the caller's own,
// or anyone's for an operator. A refusal carries the server's code:
// principal_exists (the name, or one that looks the same, is taken),
// rate_limited (too many renames today) or invalid_request (not a valid name).
export async function renamePrincipal(id: number, name: string): Promise<ApiResult<PrincipalV0>> {
  const request: RenamePrincipalRequestV1 = { name };
  const reply = await authedSend(call('renamePrincipal', { id }), request);
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
  const result = await authed(call('listChannels'), listChannelsResponse);
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
  const reply = await authedPost(call('createChannel'), request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return inviteRefused(reply.res);
  const created = await readBody(reply.res, createChannelResponse);
  return created.ok ? { ok: true, value: created.value.channel } : created;
}

// renameChannel renames a channel by its id (issue #728): operators and this
// channel's managers. from is the name the caller's list shows
// now; the server refuses the rename when that is no longer the channel's name
// (channel_name_stale), so two operators cannot rename past each other. Other
// refusals carry their code and line as createChannel's do: channel_exists for
// a name taken (or one that reads the same), invalid_request with the rule a
// name broke. The answer is the channel as it now is. It is never repeated
// here.
export async function renameChannel(id: number, from: string, name: string): Promise<InviteResult<ChannelV0>> {
  const request: RenameChannelRequestV1 = { name, from };
  const reply = await authedSend(call('renameChannel', { id }), request);
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

// A page of history is the most the server returns for one call, and the
// size the client asks for. A page that comes back shorter is the last one.
export const historyLimit = 100;
const maxHistoryPages = 100;

// loadMessages reads one page of a channel's history: the newest page, or with
// `before` the page just below that id. The server answers newest first
// (`order=desc`; #334) and this hands it back in id order, so opening a channel
// is one request however long its history is.
export async function loadMessages(channel: string, before?: number): Promise<ApiResult<MessageV2[]>> {
  const cursor = before === undefined ? '' : `&before=${String(before)}`;
  const request = call('listMessages', { channel }, `order=desc&limit=${String(historyLimit)}${cursor}`);
  const result = await authed(request, listMessagesResponseV2);
  if (!result.ok) return result;
  return { ok: true, value: [...result.value.messages].reverse() };
}

export async function loadMessagesAfter(channel: string, after: number): Promise<ApiResult<MessageV2[]>> {
  const result = await authed(call('listMessages', { channel }, queryOf({ order: 'asc', limit: String(historyLimit), after: String(after) })), listMessagesResponseV2);
  return result.ok ? { ok: true, value: result.value.messages } : result;
}

export async function loadThread(channel: string, root: number, after = 0): Promise<ApiResult<ListMessagesResponseV2>> {
  return authed(call('listMessages', { channel }, queryOf({ thread: String(root), after: String(after), limit: String(historyLimit) })), listMessagesResponseV2);
}

export async function searchMessages(channel: string, words: string, before?: number): Promise<ApiResult<SearchMessagesResponseV2>> {
  const query = new URLSearchParams({ q: words, limit: String(historyLimit) });
  if (before !== undefined) query.set('before', String(before));
  return authed(call('searchMessages', { channel }, query.toString()), searchMessagesResponseV2);
}

// What an edit of one's own message came to (issue #845,
// docs/design/message-editing.md §4). refused is the server's no, with its
// code (message_edit_window_closed, rate_limited, …) and, for a rate limit,
// how long to wait; network is no answer, or one that cannot be trusted, and
// is safe to send again: a repeated PUT of the same text changes nothing (§3.5).
export type EditResult =
  | { readonly ok: true; readonly message: MessageV2 }
  | { readonly ok: false; readonly failure: 'unauthorized' | 'network' }
  | { readonly ok: false; readonly failure: 'refused'; readonly code?: string; readonly retryAfter?: number };

// editMessage replaces the text of one's own message. It is never repeated
// here: Save again is the person's.
export async function editMessage(channel: string, id: number, body: string): Promise<EditResult> {
  const request: EditMessageRequestV1 = { body };
  const reply = await authedSend(call('editMessage', { channel, id }), request);
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  const res = reply.res;
  if (res.status >= 500) return { ok: false, failure: 'network' };
  if (!res.ok) {
    try {
      const doc: unknown = await res.json();
      if (errorV0(doc).length === 0) {
        const { code, retry_after_seconds: retryAfter } = doc as ErrorV0;
        return { ok: false, failure: 'refused', code, ...(retryAfter === undefined ? {} : { retryAfter }) };
      }
    } catch {
      // No document, or not one.
    }
    return { ok: false, failure: 'refused' };
  }
  try {
    const parsed: unknown = await res.json();
    if (editMessageResponseV1(parsed).length === 0) return { ok: true, message: (parsed as EditMessageResponseV1).message };
  } catch {
    // Fall through: the body was cut off.
  }
  return { ok: false, failure: 'network' };
}

// messageRevisions reads one's own message's earlier versions, oldest first.
// Only the author is answered; anyone else is message_not_found.
export async function messageRevisions(channel: string, id: number): Promise<ApiResult<MessageRevisionV1[]>> {
  const result = await authed(call('messageRevisions', { channel, id }), listMessageRevisionsResponseV1);
  return result.ok ? { ok: true, value: result.value.revisions } : result;
}

export type AscendingMessagesResult =
  | { readonly ok: true; readonly complete: boolean; readonly value: MessageV2[] }
  | { readonly ok: false; readonly failure: ApiFailure; readonly code?: string };

// loadMessagesAscending follows the server's after cursor from after. It is
// the reconciliation read after an uncertain post: match the frozen body,
// audience and attachment ids before deciding whether a retry is safe. An
// incomplete result is partial evidence only: absence from value is not proof
// that a retry is safe. The walk is complete only at a cursor-less final page.
export async function loadMessagesAscending(channel: string, after = 0): Promise<AscendingMessagesResult> {
  const all: MessageV2[] = [];
  let from = after;
  for (let page = 0; page < maxHistoryPages; page++) {
    const request = call('listMessages', { channel }, queryOf({ order: 'asc', limit: String(historyLimit), after: String(from) }));
    const result = await authed(request, listMessagesResponseV2);
    if (!result.ok) return result;
    all.push(...result.value.messages);
    const next = result.value.next_after;
    if (next === undefined) return { ok: true, complete: true, value: all };
    if (!Number.isSafeInteger(next) || next <= from || result.value.messages.length === 0) {
      return { ok: true, complete: false, value: all };
    }
    from = next;
  }
  return { ok: true, complete: false, value: all };
}

export async function listMembers(channel: string): Promise<ApiResult<ChannelMemberV1[]>> {
  const result = await authed(call('listMembers', { channel }), listChannelMembersResponseV1);
  return result.ok ? { ok: true, value: result.value.members } : result;
}

export async function addChannelMember(channel: string, name: string): Promise<InviteResult<ChannelMemberV1>> {
  const request: AddChannelMemberRequestV1 = { name };
  return readSent(await authedPost(call('addMember', { channel }), request), channelMemberV1);
}

export async function putChannelMember(channel: string, principal: number, request: PutChannelMemberRequestV1): Promise<InviteResult<null>> {
  return readDone(await authedSend(call('putMember', { channel, principal }), request));
}

export async function removeChannelMember(channel: string, principal: number): Promise<InviteResult<null>> {
  return readDone(await authedSend(call('removeMember', { channel, principal })));
}

export async function listNets(channel: string): Promise<ApiResult<NetV1[]>> {
  const result = await authed(call('listNets', { channel }), listNetsResponseV1);
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

// requestRefusal is the line and code of an error document, or a fallback
// specific to the request when there is no usable document.
async function requestRefusal(res: Response, fallback: string): Promise<{ readonly reason: string; readonly code?: string }> {
  try {
    const body: unknown = await res.json();
    if (errorV0(body).length === 0) {
      const doc = body as ErrorV0;
      const line = doc.message.replace(/\s+/g, ' ').trim();
      const reason = line === '' ? fallback : line.length > 300 ? `${line.slice(0, 299)}…` : line;
      return { reason, code: doc.code };
    }
  } catch {
    // No document, or not one.
  }
  return { reason: fallback };
}

const postRefusal = (res: Response) => requestRefusal(res, 'The server refused the message');

// postMessage sends one message, to the whole channel or, with an audience, to
// a net or a list of principals, with a payload (an attachment's file ids, say)
// when it carries one. It is never repeated here: sending again is the
// caller's, on a click.
export async function postMessage(channel: string, body: string, audience?: Audience, payload?: Payload, threadRootId?: number): Promise<PostResult> {
  const request: PostMessageRequestV2 = {
    body,
    ...(audience === undefined ? {} : { audience }),
    ...(payload === undefined ? {} : { payload }),
    ...(threadRootId === undefined ? {} : { thread_root_id: threadRootId }),
  };
  const reply = await authedPost(call('postMessage', { channel }), request);
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

// File attachments (issue #762): the browser face of the routes of issue #23.

// fileAttachmentPayload builds the registered payload from generated types.
// The server enforces ownership, channel, count and single-use attachment.
export function fileAttachmentPayload(fileIds: readonly number[]): Payload {
  const data: FileAttachmentV1 = { file_ids: [...fileIds] };
  return { schema: fileAttachmentPayloadSchema, data };
}

// What an upload came to. cancelled is the person's own Cancel; refused is the
// server's no, with its code (uploads_disabled, file_too_large, …) and its own
// line; network is no answer at all, which for an upload is safe to try again:
// the bytes are content-addressed and no message attached them. A retry makes
// another row. Completed rows and bytes have no automatic retention cleanup,
// whether or not the client learns their ids.
export type UploadResult =
  | { readonly ok: true; readonly file: FileV1 }
  | { readonly ok: false; readonly outcome: 'cancelled' | 'unauthorized' | 'network' }
  | { readonly ok: false; readonly outcome: 'refused'; readonly reason: string; readonly code?: string };

// uploadFile stores one file in a channel (POST /v2/channels/{channel}/files):
// the bytes are the request body itself (not JSON, not multipart), and the
// declared media type goes as the Content-Type, which the server reduces to
// type/subtype, records, and never serves back. Progress is an honest
// indeterminate state, with Cancel through signal: fetch cannot stream a
// request body portably (Chrome-only, and a streamed body would defeat the
// server's early 413, which answers before reading), so a byte counter would
// be a lie. A 401 signs out, as everywhere.
export async function uploadFile(channel: string, name: string, mediaType: string, bytes: Blob, signal: AbortSignal): Promise<UploadResult> {
  let held = current();
  if (held === null) return { ok: false, outcome: 'unauthorized' };
  let res: Response;
  for (let sent = 0; ; sent++) {
    try {
      res = await platform.http.upload(call('uploadFile', { channel }, fileQuery(name)), held.token, mediaType, bytes, signal);
    } catch {
      if (signal.aborted) return { ok: false, outcome: 'cancelled' };
      noteNoAnswer();
      return { ok: false, outcome: 'network' };
    }
    if (!res.ok) await noteRefusal(res);
    if (res.status !== 401) break;
    // A 401 is answered before the bytes are stored: sent again with a
    // credential since replaced, it is still one file (issue #1000).
    await discard(res);
    const next = await settleRefusal(held, true);
    if (next === null) return { ok: false, outcome: 'unauthorized' };
    if (next === 'stale' || sent + 1 >= resends) return { ok: false, outcome: 'network' };
    held = next;
  }
  if (!res.ok) return { ok: false, outcome: 'refused', ...(await requestRefusal(res, 'The server refused the upload')) };
  try {
    const parsed: unknown = await res.json();
    if (fileV1(parsed).length === 0) return { ok: true, file: parsed as FileV1 };
  } catch {
    // Fall through: the body was cut off.
  }
  // The created row and bytes persist even when their id is lost here.
  return { ok: false, outcome: 'refused', reason: 'The server’s answer was not readable' };
}

// What reading a file gave. unavailable is the server's one 404 (file_not_found),
// which covers "no such file", "not your channel" and "not a message you may
// see" alike: it is quiet, and never a sign-out.
export type FileResult<T> =
  | { readonly ok: true; readonly value: T }
  | { readonly ok: false; readonly failure: 'unavailable' | ApiFailure };

// getFileMeta reads a file's metadata (GET /v2/files/{id}/meta).
export async function getFileMeta(id: number): Promise<FileResult<FileV1>> {
  const result = await authed(call('fileMeta', { id }), fileV1);
  if (!result.ok && result.failure === 'invalid' && result.code === 'file_not_found') return { ok: false, failure: 'unavailable' };
  return result;
}

// fetchFileBytes reads a file's bytes (GET /v2/files/{id}). The download is
// always application/octet-stream with nosniff: the caller that renders them
// must validate the content itself, and never trust the stored media type.
export async function fetchFileBytes(id: number): Promise<FileResult<Blob>> {
  let held = current();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  let res: Response;
  for (let sent = 0; ; sent++) {
    try {
      res = await platform.http.download(call('fileBytes', { id }), held.token);
    } catch {
      noteNoAnswer();
      return { ok: false, failure: 'network' };
    }
    if (!res.ok) await noteRefusal(res);
    if (res.status !== 401) break;
    await discard(res);
    const next = await settleRefusal(held, true);
    if (next === null) return { ok: false, failure: 'unauthorized' };
    if (next === 'stale' || sent + 1 >= resends) return { ok: false, failure: 'network' };
    held = next;
  }
  if (res.status === 404) {
    await discard(res);
    return { ok: false, failure: 'unavailable' };
  }
  if (!res.ok) {
    await discard(res);
    return { ok: false, failure: 'invalid' };
  }
  try {
    return { ok: true, value: await res.blob() };
  } catch {
    return { ok: false, failure: 'network' };
  }
}

// Read state (UX-02). Both calls are for people: an agent credential, or
// conchd with authentication off, is refused with one of the codes below and
// the caller then asks for nothing more.
export const readStateRefusals: readonly string[] = ['read_state_require_auth', 'human_required'];

// listReadState reads the caller's mark and unread count in every channel they
// are in, by channel id.
export async function listReadState(): Promise<ApiResult<ReadStateV1[]>> {
  const result = await authed(call('listReadState'), listReadStateResponseV1);
  return result.ok ? { ok: true, value: result.value.read_state } : result;
}

// markRead asks for the caller's mark in a channel to move to message `id`. The
// answer is the state as stored: the mark only moves forward, so it may be
// ahead of `id`.
export async function markRead(channel: string, id: number): Promise<ApiResult<ReadStateV1>> {
  const request: MarkReadRequestV1 = { last_read_id: id };
  const reply = await authedSend(call('markRead', { channel }), request);
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

export type NotificationPolicy = 'off' | 'mentions' | 'all';
export interface NotificationPreferences {
  readonly policy: NotificationPolicy;
  readonly muted: readonly number[];
  readonly native: boolean;
}

const emptyNotificationPreferences: NotificationPreferences = { policy: 'off', muted: [], native: false };
const notificationPreferencesKey = (origin: string, account: number) => `conch.web.notifications.v1.${origin}.${String(account)}`;

export function readNotificationPreferences(origin: string, account: number): NotificationPreferences {
  try {
    const raw: unknown = JSON.parse(platform.credentials.readPreference(notificationPreferencesKey(origin, account)) ?? 'null');
    if (typeof raw !== 'object' || raw === null) return emptyNotificationPreferences;
    const value = raw as { policy?: unknown; muted?: unknown; native?: unknown };
    return {
      policy: value.policy === 'mentions' || value.policy === 'all' ? value.policy : 'off',
      muted: Array.isArray(value.muted) ? value.muted.filter((id): id is number => typeof id === 'number' && Number.isSafeInteger(id) && id > 0) : [],
      native: value.native === true,
    };
  } catch {
    return emptyNotificationPreferences;
  }
}

export function writeNotificationPreferences(origin: string, account: number, preferences: NotificationPreferences): void {
  try {
    platform.credentials.writePreference(notificationPreferencesKey(origin, account), JSON.stringify(preferences));
  } catch {
    // Storage can be unavailable; the in-memory preference still applies.
  }
}

// requestTicket asks for a single-use ticket to open a socket with.
async function requestTicket(): Promise<ApiResult<WSTicketV1>> {
  const reply = await authedPost(call('wsTicket'));
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
// `query` names, with the ticket added. The ticket is single use, so a
// reconnect calls this again. A platform that opens its sockets with the
// credential it holds (the desktop shell's main) is asked for none. The
// diagnostic ring is told of each open, each close with its code and each try
// after a loss, by kind, never by address: the address holds the channel and
// the ticket.
async function connect(name: SocketName, id: OperationId, query: Readonly<Record<string, string>>, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  const kind = diagnosticOf[name];
  if (lost[name]) record({ kind: 'reconnect', socket: kind, attempt: ++retries[name] });
  // Lost until it opens: a ticket refused or a socket that never opens is
  // followed by another try.
  lost[name] = true;
  let address = query;
  if (platform.sockets.ticket) {
    const ticket = await requestTicket();
    if (!ticket.ok) return ticket;
    address = { ...query, ticket: ticket.value.ticket };
  }
  let socket: FeedConnection;
  try {
    socket = platform.sockets.open(id, address);
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
  connect('messages', 'messagesSocket', { channel, after: String(after), heartbeat: '1' }, events);

// openEventsSocket opens the events stream of a channel (roster, runs,
// activity, interventions) with the browser's own session, which the server
// then keeps online while the socket is open.
export function openEventsSocket(channel: string, session: number, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  return connect('events', 'eventsSocket', { channel, session: String(session) }, events);
}

// The browser's own session (UX-18): created after sign-in, ended on sign-out
// and when the page goes away.

// createSession registers the browser as a client of the signed-in principal.
export async function createSession(client: string): Promise<ApiResult<SessionV1>> {
  const request: CreateSessionRequestV1 = { client };
  const reply = await authedPost(call('createSession'), request);
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
  return authedSend(call('endSession', { id }), undefined, true).then(() => undefined);
}

// Runs, their activity and their interventions.

// listSessionRuns reads the newest runs of one session in a channel, newest
// first, with one request (the server's order=desc, issue #317).
export async function listSessionRuns(channel: string, session: number, limit: number): Promise<ApiResult<RunV1[]>> {
  const query = new URLSearchParams({ channel, session: String(session), order: 'desc', limit: String(limit) });
  const result = await authed(call('listRuns', undefined, query.toString()), listRunsResponseV1);
  return result.ok ? { ok: true, value: result.value.runs } : result;
}

// listChannelRuns reads one page of a channel's runs, newest first: without
// `before` the newest, with it the page just below that id (the server's
// order=desc and before, issue #317). Every run the caller may see is listed.
export async function listChannelRuns(channel: string, limit: number, before?: number): Promise<ApiResult<RunV1[]>> {
  const query = new URLSearchParams({ channel, order: 'desc', limit: String(limit) });
  if (before !== undefined && before > 0) query.set('before', String(before));
  const result = await authed(call('listRuns', undefined, query.toString()), listRunsResponseV1);
  return result.ok ? { ok: true, value: result.value.runs } : result;
}

export function getRun(id: number): Promise<ApiResult<RunV1>> {
  return authed(call('getRun', { id }), runV1);
}

// The most pages one read follows; past it the newest entries are what is
// missing from the end, and the live frames bring them.
const maxPages = 100;

// listActivity reads a run's activity after the entry `after`, oldest first.
export async function listActivity(run: number, after: number): Promise<ApiResult<AgentActivityV1[]>> {
  const all: AgentActivityV1[] = [];
  let from = after;
  for (let page = 0; page < maxPages; page++) {
    const request = call('listActivity', { id: run }, queryOf({ after: String(from), limit: String(historyLimit) }));
    const result = await authed(request, listActivityResponseV1);
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
    const request = call('listInterventions', { id: run }, queryOf({ after: String(after), limit: String(historyLimit) }));
    const result = await authed(request, listInterventionsResponseV1);
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
  const reply = await authedPost(call('createIntervention', { id: run }), request);
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
  const reply = await authedPost(call('voiceSession', { channel }));
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
  return authed(call('voicePresence', { channel }), voicePresenceV1);
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
  const reply = await authedSend(call('voiceTransmit', { channel }), report, false, signal);
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
  return connect('voice', 'voiceSocket', { channel }, events);
}

// Voice occupancy (issue #596): how many are in each channel's voice, for
// every channel the caller may see voice in. Counts only, never names; the
// page holds one socket of it, not one per channel.

// getVoiceOccupancy reads it once. The page keeps it on its socket, and reads
// this only to learn why the server closed that socket (1008).
export function getVoiceOccupancy(): Promise<ApiResult<VoiceOccupancyV1>> {
  return authed(call('voiceOccupancy'), voiceOccupancyV1);
}

// openOccupancySocket opens its stream with a ticket: the whole document on
// connect, and again whenever a count changes.
export function openOccupancySocket(events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  return connect('occupancy', 'occupancySocket', {}, events);
}

// sendReport sends a problem report (issue #446) to the server's operator.
// The answer is its id. A refusal says why as an invite call's does: the
// server's code and line, and for rate_limited the seconds to wait.
export async function sendReport(request: CreateProblemReportRequestV1): Promise<InviteResult<CreateProblemReportResponseV1>> {
  const reply = await authedPost(call('sendReport'), request);
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
  const reply = await authedPost(call('claimShare', { channel }));
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
  const query = startedAt === undefined ? '' : queryOf({ started_at: startedAt });
  const reply = await authedSend(call('stopShare', { channel }, query));
  if (reply.kind === 'none') return { ok: false, failure: 'network' };
  if (reply.kind === 'unauthorized') return { ok: false, failure: 'unauthorized' };
  if (!reply.res.ok) return refused(reply.res);
  return { ok: true, value: null };
}
