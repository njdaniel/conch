// The only module that reads or writes the credential, and the only one that
// calls conchd. Where the token lives depends on whose it is (issue #439,
// decision O1). A member's lasts: it is kept in localStorage, so it survives
// closing the browser and every tab of the origin shares it, until it is
// signed out or stops working. An operator's stays with the one tab that
// agreed to use it, in sessionStorage (decision D2, kept for operators). The
// page has no third-party script (script-src 'self'), which is what makes
// local storage acceptable. The token is sent only as the Authorization
// header, and never put in a URL, a log call, the DOM or an error: a failure
// is reported as a kind, never with the request in it.
import { listActivityResponseV1, type AgentActivityV1 } from './schema/activity';
import { revokeAllCredentialsResponseV1 } from './schema/credential';
import { errorV0 } from './schema/error';
import { health } from './schema/health';
import { interventionV1, listInterventionsResponseV1, type CreateInterventionRequestV1, type InterventionV1 } from './schema/intervention';
import { listRunsResponseV1, runV1, type RunV1 } from './schema/run';
import { sessionV1, type CreateSessionRequestV1, type SessionV1 } from './schema/session';
import { listChannelMembersResponseV1, listChannelsResponse, type ChannelMemberV1, type ChannelV0 } from './schema/channel';
import { listMessagesResponseV2, postMessageResponseV2, type Audience, type MessageV2, type PostMessageRequestV2 } from './schema/message';
import { listNetsResponseV1, type NetV1 } from './schema/net';
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
import { voicePresenceV1, voiceSessionResponseV1, type VoicePresenceV1, type VoiceSessionResponseV1, type VoiceTransmitReportV1, type VoiceTransmitState } from './schema/voice';
import type { Validator } from './schema/validate';
import type { FeedSocket, OpenSocket, SocketEvents } from './state/feed';

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
// operator's; 'lasting' is every tab of the origin until it is signed out
// (localStorage), for a member's.
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

// forget drops the credential held, from memory and from its storage.
function forget(held: Held): void {
  if (pinned?.token === held.token && pinned.place === held.place) pinned = null;
  dropToken(held);
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
    return { ok: false, failure: 'network' };
  }
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
    return { kind: 'none' };
  }
  if (res.status === 401) {
    forget(held);
    return { kind: 'unauthorized' };
  }
  return { kind: 'reply', res };
}

const authedPost = (path: string, body?: unknown) => authedSend('POST', path, body);

// serverVersion reads the server's build version from GET /healthz, which
// needs no credential. It is null when the server cannot be reached or does not
// answer with a health document. A degraded server (503) still names its
// version, so the status is not looked at.
export async function serverVersion(): Promise<string | null> {
  try {
    const res = await fetch('/healthz', { headers: { Accept: 'application/json' }, cache: 'no-store', credentials: 'omit' });
    const body: unknown = await res.json();
    return health(body).length === 0 ? (body as { version: string }).version : null;
  } catch {
    return null;
  }
}

// hasToken says whether this tab has a credential from an earlier load: its
// own, or a member's lasting one.
export function hasToken(): boolean {
  return readToken() !== null;
}

// signIn checks a credential with GET /v1/whoami. A member's is stored at
// once, to last. An operator's is held back until continueAsOperator, because
// a browser is not the place for it. A rejected credential is stored nowhere.
export async function signIn(candidate: string): Promise<ApiResult<WhoAmIResponseV1>> {
  pending = null;
  pinned = null;
  const result = await get('/v1/whoami', candidate, whoAmIResponseV1);
  if (!result.ok) return result;
  if (result.value.role === 'operator') {
    pending = candidate;
  } else if (writeToken(candidate, 'lasting')) {
    pinned = { token: candidate, place: 'lasting' };
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
  const held = readToken();
  if (held === null) return { ok: false, failure: 'unauthorized' };
  const result = await get('/v1/whoami', held.token, whoAmIResponseV1);
  if (!result.ok) {
    if (result.failure === 'unauthorized') dropToken(held);
    return result;
  }
  if (held.place === 'lasting' && result.value.role === 'operator') {
    dropToken(held);
    pending = held.token;
    return { ok: true, value: { principal: result.value, warn: true } };
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
  if (held === null) return true;
  const reply = await sendAs(held, 'DELETE', '/v1/credentials/current');
  dropToken(held);
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
  // one of ours when the answer carried none.
  | { readonly ok: false; readonly outcome: 'refused'; readonly reason: string };

// refusalLine is the message of the error document a refusal carries, on one
// line, or a fallback.
async function refusalLine(res: Response): Promise<string> {
  try {
    const body: unknown = await res.json();
    if (errorV0(body).length === 0) {
      const line = (body as { message: string }).message.replace(/\s+/g, ' ').trim();
      if (line !== '') return line.length > 300 ? `${line.slice(0, 299)}…` : line;
    }
  } catch {
    // No document, or not one.
  }
  return 'The server refused the message';
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
  if (!res.ok) return { ok: false, outcome: 'refused', reason: await refusalLine(res) };
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

// connect asks for a ticket with the credential and opens the socket that
// `address` names, given the ticket. The ticket is single use, so a reconnect
// calls this again.
async function connect(address: (ticket: string) => string, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  const ticket = await requestTicket();
  if (!ticket.ok) return ticket;
  let socket: WebSocket;
  try {
    socket = new WebSocket(address(ticket.value.ticket));
  } catch {
    return { ok: false, failure: 'network' };
  }
  socket.onopen = events.onOpen;
  socket.onmessage = (event: MessageEvent) => {
    events.onFrame(event.data);
  };
  // An error is always followed by a close, which is what the feed acts on.
  socket.onclose = (event: CloseEvent) => {
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
  connect((ticket) => socketUrl('/v2/ws', { channel, after: String(after), heartbeat: '1', ticket }), events);

// openEventsSocket opens the events stream of a channel (roster, runs,
// activity, interventions) with the browser's own session, which the server
// then keeps online while the socket is open.
export function openEventsSocket(channel: string, session: number, events: SocketEvents): Promise<ApiResult<FeedSocket>> {
  return connect((ticket) => socketUrl('/v1/events/ws', { channel, session: String(session), ticket }), events);
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
// It must be called while the credential is still held, so sign-out calls it
// first.
export function endSession(id: number): void {
  void authedSend('DELETE', `/v1/sessions/${String(id)}`, undefined, true);
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

// getVoicePresence reads who is in a channel's voice, once. The presence
// socket takes over while the caller is in voice.
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
  return connect((ticket) => socketUrl('/v1/voice/ws', { channel, ticket }), events);
}
