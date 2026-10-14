// The only module that reads or writes the credential, and the only one that
// calls conchd. The token lives in sessionStorage, which is per tab (decision
// D2): two tabs are two sign-ins, and closing the tab drops it. It is sent only
// as the Authorization header, and never put in a URL, a log call, the DOM or
// an error: a failure is reported as a kind, never with the request in it.
import { listChannelMembersResponseV1, listChannelsResponse, type ChannelMemberV1, type ChannelV0 } from './schema/channel';
import { listMessagesResponseV2, postMessageResponseV2, type MessageV2, type PostMessageRequestV2 } from './schema/message';
import { listNetsResponseV1, type NetV1 } from './schema/net';
import { whoAmIResponseV1, type WhoAmIResponseV1 } from './schema/principal';
import { wsTicketV1, type WSTicketV1 } from './schema/ticket';
import type { Validator } from './schema/validate';
import type { FeedSocket, OpenSocket } from './state/feed';

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

// authedPost is the POST sibling of authed: the stored credential, a JSON body
// (or none), and on a 401 the token dropped. It hands back the response
// itself, because a post must tell "no answer" from "answered no".
type PostReply =
  | { readonly kind: 'reply'; readonly res: Response }
  | { readonly kind: 'none' }
  | { readonly kind: 'unauthorized' };

async function authedPost(path: string, body?: unknown): Promise<PostReply> {
  const token = readToken();
  if (token === null) return { kind: 'unauthorized' };
  let res: Response;
  try {
    res = await fetch(path, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: 'application/json',
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
      credentials: 'omit',
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

// A page of history is the most the server returns for one call.
const historyLimit = 100;

// At most this many pages are walked (2,000 messages). The socket, opened after
// the newest id read, replays the rest.
export const maxHistoryPages = 20;

// loadMessages reads a channel's messages in id order. The server lists
// forwards only (`after`, no `before`; #334), so this walks its pages from the
// start, up to the cap, and the page draws the newest of them.
export async function loadMessages(channel: string): Promise<ApiResult<MessageV2[]>> {
  const all: MessageV2[] = [];
  let after = 0;
  for (let page = 0; page < maxHistoryPages; page++) {
    const path = `/v2/channels/${encodeURIComponent(channel)}/messages?after=${String(after)}&limit=${String(historyLimit)}`;
    const result = await authed(path, listMessagesResponseV2);
    if (!result.ok) return result;
    all.push(...result.value.messages);
    const next = result.value.next_after ?? 0;
    // A page that does not move forward would loop for ever.
    if (next <= after) return { ok: true, value: all };
    after = next;
  }
  return { ok: true, value: all };
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
  | { readonly ok: false; readonly outcome: 'unauthorized' | 'refused' | 'unconfirmed' };

// postMessage sends one message to the whole channel. It is never repeated
// here: sending again is the caller's, on a click.
export async function postMessage(channel: string, body: string): Promise<PostResult> {
  const request: PostMessageRequestV2 = { body };
  const reply = await authedPost(`/v2/channels/${encodeURIComponent(channel)}/messages`, request);
  if (reply.kind === 'none') return { ok: false, outcome: 'unconfirmed' };
  if (reply.kind === 'unauthorized') return { ok: false, outcome: 'unauthorized' };
  const res = reply.res;
  // A 5xx may come from something in front of conchd after it stored the message.
  if (res.status >= 500) return { ok: false, outcome: 'unconfirmed' };
  if (!res.ok) return { ok: false, outcome: 'refused' };
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
function socketUrl(channel: string, after: number, ticket: string): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const query = new URLSearchParams({ channel, after: String(after), ticket }).toString();
  return `${scheme}//${window.location.host}/v2/ws?${query}`;
}

// openSocket asks for a ticket with the credential and opens the message
// stream with it, replaying everything after `after`. The ticket is single
// use, so a reconnect calls this again.
export const openSocket: OpenSocket = async (channel, after, events) => {
  const ticket = await requestTicket();
  if (!ticket.ok) return ticket;
  let socket: WebSocket;
  try {
    socket = new WebSocket(socketUrl(channel, after, ticket.value.ticket));
  } catch {
    return { ok: false, failure: 'network' };
  }
  socket.onopen = events.onOpen;
  socket.onmessage = (event: MessageEvent) => {
    events.onFrame(event.data);
  };
  // An error is always followed by a close, which is what the feed acts on.
  socket.onclose = (event: CloseEvent) => {
    events.onClose(event.code);
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
};
