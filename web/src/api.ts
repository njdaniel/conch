// The only module that reads or writes the credential, and the only one that
// calls conchd. The token lives in sessionStorage, which is per tab (decision
// D2): two tabs are two sign-ins, and closing the tab drops it. It is sent only
// as the Authorization header, and never put in a URL, a log call, the DOM or
// an error: a failure is reported as a kind, never with the request in it.
import { listActivityResponseV1, type AgentActivityV1 } from './schema/activity';
import { errorV0 } from './schema/error';
import { interventionV1, listInterventionsResponseV1, type CreateInterventionRequestV1, type InterventionV1 } from './schema/intervention';
import { listRunsResponseV1, runV1, type RunV1 } from './schema/run';
import { sessionV1, type CreateSessionRequestV1, type SessionV1 } from './schema/session';
import { listChannelMembersResponseV1, listChannelsResponse, type ChannelMemberV1, type ChannelV0 } from './schema/channel';
import { listMessagesResponseV2, postMessageResponseV2, type Audience, type MessageV2, type PostMessageRequestV2 } from './schema/message';
import { listNetsResponseV1, type NetV1 } from './schema/net';
import { whoAmIResponseV1, type WhoAmIResponseV1 } from './schema/principal';
import { wsTicketV1, type WSTicketV1 } from './schema/ticket';
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

// authedSend is the writing sibling of authed: the stored credential, a JSON
// body (or none), and on a 401 the token dropped. It hands back the response
// itself, because a write must tell "no answer" from "answered no". keepalive
// lets the request outlive the page, for the one sent as the page goes away.
type PostReply =
  | { readonly kind: 'reply'; readonly res: Response }
  | { readonly kind: 'none' }
  | { readonly kind: 'unauthorized' };

async function authedSend(method: 'POST' | 'DELETE', path: string, body?: unknown, keepalive = false): Promise<PostReply> {
  const token = readToken();
  if (token === null) return { kind: 'unauthorized' };
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: 'application/json',
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
      credentials: 'omit',
      keepalive,
    });
  } catch {
    return { kind: 'none' };
  }
  if (res.status === 401) {
    dropToken();
    return { kind: 'unauthorized' };
  }
  return { kind: 'reply', res };
}

const authedPost = (path: string, body?: unknown) => authedSend('POST', path, body);

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
export const openSocket: OpenSocket = (channel, after, events) =>
  connect((ticket) => socketUrl('/v2/ws', { channel, after: String(after), ticket }), events);

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
