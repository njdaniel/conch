// The messages of each channel the tab has opened: what the server has told us,
// and what this tab has sent that the server has not yet confirmed. Pure
// reducer; src/api.ts and the socket feed it through App.
import { messageV2, type Audience, type MessageV2, type Payload } from '../schema/message';
import { attachmentPayload, type DraftAttachment } from './attachments';

// The most messages held per channel. Messages arriving live push the oldest
// out (a pending send of our own is kept apart and never dropped). Loading
// older history never does: it stops instead, once another page would not fit
// (canLoadOlder), so the newest are never the ones lost.
export const maxHeld = 2000;

// The size of a page of history; see api.historyLimit.
export const pageSize = 100;

// The state of one message this tab is sending. A message becomes `Sent` by
// leaving this list: the server's copy is in `byId` and its id in `sentIds`.
// Status remains the small set the conversation draws; phase also distinguishes
// checking an uncertain send from making its first POST.
export type SendStatus = 'sending' | 'failed' | 'unconfirmed';
export type SendPhase = 'posting' | 'failed' | 'unconfirmed' | 'reconciling';

export interface PendingSend {
  // A local key; the server's id is not known until the POST returns.
  readonly localId: number;
  // The channel identity is frozen so a restored send cannot follow a reused name.
  readonly channelId: number;
  // The principal who started the send; only their server copy can reconcile it.
  readonly authorId: number;
  readonly body: string;
  // When it was typed (ms since the epoch), for the time shown beside it.
  readonly at: number;
  readonly status: SendStatus;
  readonly phase: SendPhase;
  // Who it goes to when not the whole channel; kept so a Retry goes to the same.
  readonly audience?: Audience;
  readonly threadRootId?: number;
  // A frozen composer-order snapshot. Ready fileIds and payload survive a
  // failed POST and are reused by Retry; local bytes remain available too.
  readonly attachments: readonly DraftAttachment[];
  readonly payload?: Payload;
  // Only a server message after this id may reconcile the send. In particular,
  // an older identical message must not resolve an unconfirmed retry.
  readonly reconcileAfter: number;
  // A caller-owned unique requestId guards async answers when reset reuses localId.
  // It is optional only for compatibility with existing text-only callers.
  readonly requestId?: string;
}

export type HistoryStatus = 'idle' | 'loading' | 'ready' | 'error';

// The state of a request for the page below the oldest message held.
export type OlderStatus = 'idle' | 'loading' | 'error';

export interface ChannelChat {
  // The id is the identity of a message: one that arrives twice is one.
  readonly byId: ReadonlyMap<number, MessageV2>;
  readonly pending: readonly PendingSend[];
  // Messages this tab sent and the POST confirmed, shown as `Sent`.
  readonly sentIds: ReadonlySet<number>;
  readonly history: HistoryStatus;
  readonly older: OlderStatus;
  // Whether the server has nothing below the oldest message read: a page came
  // back shorter than asked for.
  readonly exhausted: boolean;
  // Whether older messages were dropped to stay within maxHeld.
  readonly trimmed: boolean;
  // Frames that failed the schema and were not shown.
  readonly dropped: number;
  readonly nextLocalId: number;
}

export interface ChatState {
  readonly channels: Readonly<Record<string, ChannelChat>>;
}

export const initialChat: ChatState = { channels: {} };

const emptyChannel: ChannelChat = {
  byId: new Map(),
  pending: [],
  sentIds: new Set(),
  history: 'idle',
  older: 'idle',
  exhausted: false,
  trimmed: false,
  dropped: 0,
  nextLocalId: 1,
};

export type ChatAction =
  | { readonly type: 'historyLoading'; readonly channel: string }
  // `more` is whether the page was full, so there may be older history.
  | { readonly type: 'historyLoaded'; readonly channel: string; readonly messages: readonly MessageV2[]; readonly more: boolean }
  | { readonly type: 'historyFailed'; readonly channel: string }
  // A message from the socket (already validated).
  // own: the message is the caller's, so it may stand for a pending send of
  // theirs whose POST has not answered yet (issue #490).
  | { readonly type: 'received'; readonly channel: string; readonly message: MessageV2; readonly own?: boolean }
  // An edited copy of a message the tab may hold (issue #845): the edit's own
  // answer, or the events socket's conch.message_edited.v1. Applied only to a
  // message held, under the client rule (isLater); never appended.
  | { readonly type: 'edited'; readonly channel: string; readonly message: MessageV2 }
  // A frame that was not a message of this schema or channel.
  | { readonly type: 'dropped'; readonly channel: string }
  | {
      readonly type: 'sending';
      readonly channel: string;
      readonly channelId: number;
      readonly authorId: number;
      readonly body: string;
      readonly at: number;
      readonly audience?: Audience;
      readonly threadRootId?: number;
      readonly attachments?: readonly DraftAttachment[];
      readonly payload?: Payload;
      readonly requestId?: string;
    }
  // The POST returned: the server's copy replaces the local one, by id.
  | { readonly type: 'sent'; readonly channel: string; readonly localId: number; readonly message: MessageV2; readonly requestId?: string }
  | { readonly type: 'sendFailed'; readonly channel: string; readonly localId: number; readonly outcome: 'failed' | 'unconfirmed'; readonly requestId?: string }
  // An unconfirmed send is checked against history/live frames before a retry.
  | { readonly type: 'reconciling'; readonly channel: string; readonly localId: number; readonly requestId?: string }
  | { readonly type: 'reconciliationFailed'; readonly channel: string; readonly localId: number; readonly requestId?: string }
  // A click on Retry: the same text, files and target go out again, in the same place.
  | { readonly type: 'retrying'; readonly channel: string; readonly localId: number; readonly requestId?: string }
  | { readonly type: 'discard'; readonly channel: string; readonly localId: number; readonly requestId?: string }
  | { readonly type: 'olderLoading'; readonly channel: string }
  | { readonly type: 'olderLoaded'; readonly channel: string; readonly messages: readonly MessageV2[]; readonly more: boolean }
  | { readonly type: 'olderFailed'; readonly channel: string }
  // The channel held under `from` is now called `to` (issue #728): what is
  // held moves with it, history, pending sends and all.
  | { readonly type: 'renamed'; readonly from: string; readonly to: string }
  // The channel held under this name was lost (issue #1027): nothing of it is
  // kept, so a later channel given the same name starts empty.
  | { readonly type: 'forget'; readonly channel: string }
  // Pending sends kept across an expired session return for the same principal.
  | { readonly type: 'restored'; readonly pending: Readonly<Record<string, readonly PendingSend[]>> }
  | { readonly type: 'reset' };

// editedMs is when a copy of a message was last edited, in ms; a copy never
// edited is the earliest of all.
function editedMs(message: MessageV2): number {
  return message.edited_at === undefined ? -Infinity : Date.parse(message.edited_at);
}

// isLater is the client rule of message editing (docs/design/message-editing.md
// §7.1): a copy replaces the one held only if its edited_at is the same or
// later, absent counting as earliest. edited_at strictly increases per message,
// so a history page read before an edit but answered after its frame, two
// edit frames out of order and a replay overlap all leave the newest text.
export function isLater(incoming: MessageV2, held: MessageV2): boolean {
  return editedMs(incoming) >= editedMs(held);
}

// mergeMessages is the TUI's: a map by id, so a message held already is
// replaced by itself and never doubled, and never by an older copy of itself.
function merge(existing: ReadonlyMap<number, MessageV2>, incoming: readonly MessageV2[]): ReadonlyMap<number, MessageV2> {
  if (incoming.length === 0) return existing;
  const merged = new Map(existing);
  for (const message of incoming) {
    const held = merged.get(message.id);
    if (held === undefined || isLater(message, held)) merged.set(message.id, message);
  }
  return merged;
}

// newest keeps, of each message on a page, the copy held already when that one
// is the later: the page replaces what is held, but never with older text.
function newest(held: ReadonlyMap<number, MessageV2>, page: readonly MessageV2[]): readonly MessageV2[] {
  return page.map((message) => {
    const was = held.get(message.id);
    return was === undefined || isLater(message, was) ? message : was;
  });
}

// hold merges and keeps only the newest maxHeld.
function hold(c: ChannelChat, incoming: readonly MessageV2[]): ChannelChat {
  const merged = merge(c.byId, incoming);
  if (merged.size <= maxHeld) return merged === c.byId ? c : { ...c, byId: merged };
  const newest = [...merged.keys()].sort((a, b) => a - b).slice(merged.size - maxHeld);
  const keep = new Set(newest);
  return { ...c, byId: new Map([...merged].filter(([id]) => keep.has(id))), trimmed: true };
}

function update(state: ChatState, channel: string, change: (c: ChannelChat) => ChannelChat): ChatState {
  const current = state.channels[channel] ?? emptyChannel;
  return { channels: { ...state.channels, [channel]: change(current) } };
}

// known runs a change only on a channel that has an entry: a late answer for a
// channel the tab has since reset is dropped.
function known(state: ChatState, channel: string, change: (c: ChannelChat) => ChannelChat): ChatState {
  return state.channels[channel] === undefined ? state : update(state, channel, change);
}

function ownsSend(pending: PendingSend, action: { readonly localId: number; readonly requestId?: string }): boolean {
  return pending.localId === action.localId && (pending.requestId === undefined || pending.requestId === action.requestId);
}

function canReconcile(pending: PendingSend, message: MessageV2, sentIds: ReadonlySet<number>): boolean {
  return !sentIds.has(message.id) && pending.phase !== 'failed' && pending.channelId === message.channel_id && pending.authorId === message.author_id &&
    message.id > pending.reconcileAfter && sameSend(pending, message);
}

// Reconcile a page or live frame against each pending send at most once. This
// covers both the live-frame-before-POST race and a later history read used to
// settle an unconfirmed send. A server id already used for a send is never
// consumed again, including when history repeats it.
function reconcile(c: ChannelChat, incoming: readonly MessageV2[]): ChannelChat {
  let pending = c.pending;
  let sentIds = c.sentIds;
  for (const message of incoming) {
    const twin = pending.find((p) => canReconcile(p, message, sentIds));
    if (twin === undefined) continue;
    pending = pending.filter((p) => p.localId !== twin.localId);
    sentIds = new Set(sentIds).add(message.id);
  }
  return pending === c.pending && sentIds === c.sentIds ? c : { ...c, pending, sentIds };
}

export function chatReducer(state: ChatState, action: ChatAction): ChatState {
  switch (action.type) {
    case 'historyLoading':
      return update(state, action.channel, (c) => ({ ...c, history: 'loading' }));
    case 'historyLoaded':
      // The page replaces what was held: held messages from an earlier open may
      // be far below it, and keeping them would hide a gap. Older history is
      // reached again with Load older. A matching page can settle an uncertain
      // send; all other pending sends and sentIds stay.
      return update(state, action.channel, (c) =>
        reconcile(
          {
            ...hold({ ...c, byId: new Map() }, newest(c.byId, action.messages)),
            history: 'ready',
            older: 'idle',
            trimmed: false,
            exhausted: !action.more,
          },
          action.messages,
        ),
      );
    case 'historyFailed':
      return update(state, action.channel, (c) => ({ ...c, history: 'error' }));
    case 'received':
      return update(state, action.channel, (c) => {
        const message = action.message;
        if (message.edited_at !== undefined) {
          // An edit frame (issue #845) of a message not held is not a new
          // message: it is dropped, never appended. One newer than anything
          // held is a message the socket is sending for the first time that
          // was edited before it was sent (a replay after a reconnect), and
          // is shown. An edit never stands for a pending send.
          if (!c.byId.has(message.id) && message.id <= lastId(c)) return c;
          return hold(c, [message]);
        }
        // Only one's own frames may stand for a pending send. The oldest match
        // wins, so identical sends never show twice (issue #490).
        const held = hold(c, [message]);
        return action.own === true ? reconcile(held, [message]) : held;
      });
    case 'edited':
      return known(state, action.channel, (c) => (c.byId.has(action.message.id) ? hold(c, [action.message]) : c));
    case 'dropped':
      return update(state, action.channel, (c) => ({ ...c, dropped: c.dropped + 1 }));
    case 'sending':
      return update(state, action.channel, (c) => {
        const attachments = action.attachments ?? [];
        const payload = action.attachments === undefined ? action.payload : attachmentPayload(attachments);
        return {
          ...c,
          pending: [
            ...c.pending,
            {
              localId: c.nextLocalId,
              channelId: action.channelId,
              authorId: action.authorId,
              body: action.body,
              at: action.at,
              status: 'sending',
              phase: 'posting',
              attachments,
              reconcileAfter: lastId(c),
              ...(payload === undefined ? {} : { payload }),
              ...(action.audience === undefined ? {} : { audience: action.audience }),
              ...(action.threadRootId === undefined ? {} : { threadRootId: action.threadRootId }),
              ...(action.requestId === undefined ? {} : { requestId: action.requestId }),
            },
          ],
          nextLocalId: c.nextLocalId + 1,
        };
      });
    case 'sent':
      return known(state, action.channel, (c) => {
        const pending = c.pending.find((p) => ownsSend(p, action));
        if (pending === undefined || pending.channelId !== action.message.channel_id || c.sentIds.has(action.message.id)) return c;
        return {
          ...hold(c, [action.message]),
          pending: c.pending.filter((p) => !ownsSend(p, action)),
          sentIds: new Set(c.sentIds).add(action.message.id),
        };
      });
    case 'sendFailed':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.map((p) => (ownsSend(p, action) ? { ...p, status: action.outcome, phase: action.outcome } : p)),
      }));
    case 'reconciling':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.map((p) =>
          ownsSend(p, action) && p.phase === 'unconfirmed' ? { ...p, status: 'sending', phase: 'reconciling' } : p,
        ),
      }));
    case 'reconciliationFailed':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.map((p) =>
          ownsSend(p, action) && p.phase === 'reconciling' ? { ...p, status: 'unconfirmed', phase: 'unconfirmed' } : p,
        ),
      }));
    case 'retrying':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.map((p) =>
          ownsSend(p, action) && p.status !== 'sending'
            ? { ...p, status: 'sending', phase: 'posting', reconcileAfter: p.phase === 'unconfirmed' ? p.reconcileAfter : lastId(c) }
            : p,
        ),
      }));
    case 'discard':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.filter((p) => !ownsSend(p, action)),
      }));
    case 'olderLoading':
      return known(state, action.channel, (c) => ({ ...c, older: 'loading' }));
    case 'olderLoaded':
      // Merged without hold's trim: canLoadOlder kept this page within maxHeld.
      return known(state, action.channel, (c) => ({
        ...c,
        byId: merge(c.byId, action.messages),
        older: 'idle',
        exhausted: !action.more,
      }));
    case 'olderFailed':
      return known(state, action.channel, (c) => ({ ...c, older: 'error' }));
    case 'renamed':
      return { channels: rekey(state.channels, action.from, action.to) };
    case 'forget':
      return Object.hasOwn(state.channels, action.channel) ? { channels: without(state.channels, action.channel) } : state;
    case 'restored':
      return Object.entries(action.pending).reduce(
        (restored, [channel, pending]) =>
          update(restored, channel, (c) => ({
            ...c,
            pending,
            nextLocalId: Math.max(c.nextLocalId, ...pending.map((p) => p.localId + 1)),
          })),
        state,
      );
    case 'reset':
      return initialChat;
  }
}

// without is held less what is kept under one channel name (issue #1027).
export function without<T>(held: Readonly<Record<string, T>>, name: string): Readonly<Record<string, T>> {
  if (!Object.hasOwn(held, name)) return held;
  const next: Record<string, T> = {};
  for (const [key, value] of Object.entries(held)) if (key !== name) next[key] = value;
  return next;
}

// rekey moves what is held under one channel name to another (issue #728).
// Names are unique, so nothing else is held under the new one; should
// anything be, the renamed channel's own replaces it. Nothing held under the
// old name, nothing changes.
export function rekey<T>(held: Readonly<Record<string, T>>, from: string, to: string): Readonly<Record<string, T>> {
  const moved = Object.hasOwn(held, from) ? held[from] : undefined;
  if (from === to || moved === undefined) return held;
  const next: Record<string, T> = {};
  for (const [name, value] of Object.entries(held)) if (name !== from) next[name] = value;
  next[to] = moved;
  return next;
}

export function chatFor(state: ChatState, channel: string | null): ChannelChat {
  return (channel === null ? undefined : state.channels[channel]) ?? emptyChannel;
}

// ordered is every message held, in id order.
export function ordered(chat: ChannelChat): MessageV2[] {
  return [...chat.byId.values()].sort((a, b) => a.id - b.id);
}

// oldestId is the smallest id held, 0 for none: where the next older page ends.
export function oldestId(chat: ChannelChat): number {
  let oldest = 0;
  for (const id of chat.byId.keys()) if (oldest === 0 || id < oldest) oldest = id;
  return oldest;
}

// canLoadOlder says whether another page of older history fits within maxHeld.
export function canLoadOlder(chat: ChannelChat): boolean {
  return !chat.exhausted && chat.byId.size > 0 && chat.byId.size + pageSize <= maxHeld;
}

// lastId is the newest id held, 0 for none: where the socket resumes from.
export function lastId(chat: ChannelChat): number {
  let last = 0;
  for (const id of chat.byId.keys()) if (id > last) last = id;
  return last;
}

// parseFrame reads one socket frame. A frame that is not JSON, or not a v2
// message, or is for another channel, is null: it is counted, never drawn.
export function parseFrame(data: unknown, channelId: number): MessageV2 | null {
  if (typeof data !== 'string') return null;
  let body: unknown;
  try {
    body = JSON.parse(data);
  } catch {
    return null;
  }
  if (messageV2(body).length > 0) return null;
  const message = body as MessageV2;
  return message.channel_id === channelId ? message : null;
}

// sameSend says whether message is what pending sent: the same body and payload
// to the same audience (the whole channel, a net, or the same people in any
// order). Payload array order is significant, including attachment file_ids.
export function sameSend(pending: PendingSend, message: MessageV2): boolean {
  if ((pending.threadRootId ?? 0) !== (message.thread_root_id ?? 0)) return false;
  if (pending.body !== message.body || !samePayload(pending.payload, message.payload)) return false;
  // A reply's audience is the root's, narrowed by the server to whoever is still
  // entitled (a whisper recipient who left, a net member taken off): it may be
  // smaller than what the pending send held, so it is not compared.
  if (pending.threadRootId !== undefined) return true;
  const a = pending.audience;
  const b = message.audience;
  if (a === undefined || b === undefined) return a === b;
  if (a.kind === 'net' && b.kind === 'net') return a.net_id === b.net_id;
  if (a.kind === 'principals' && b.kind === 'principals') {
    const x = [...a.principal_ids].sort((m, n) => m - n);
    const y = [...b.principal_ids].sort((m, n) => m - n);
    return x.length === y.length && x.every((v, i) => v === y[i]);
  }
  return false;
}

function samePayload(a: Payload | undefined, b: Payload | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  return a.schema === b.schema && sameJson(a.data, b.data);
}

function sameJson(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true;
  if (Array.isArray(a) || Array.isArray(b)) {
    return Array.isArray(a) && Array.isArray(b) && a.length === b.length && a.every((value, index) => sameJson(value, b[index]));
  }
  if (typeof a !== 'object' || a === null || typeof b !== 'object' || b === null) return false;
  const x = a as Readonly<Record<string, unknown>>;
  const y = b as Readonly<Record<string, unknown>>;
  const xKeys = Object.keys(x);
  const yKeys = Object.keys(y);
  return xKeys.length === yKeys.length && xKeys.every((key) => Object.hasOwn(y, key) && sameJson(x[key], y[key]));
}
