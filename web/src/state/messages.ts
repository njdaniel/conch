// The messages of each channel the tab has opened: what the server has told us,
// and what this tab has sent that the server has not yet confirmed. Pure
// reducer; src/api.ts and the socket feed it through App.
import { messageV2, type MessageV2 } from '../schema/message';

// The most messages held per channel. Messages arriving live push the oldest
// out (a pending send of our own is kept apart and never dropped). Loading
// older history never does: it stops instead, once another page would not fit
// (canLoadOlder), so the newest are never the ones lost.
export const maxHeld = 2000;

// The size of a page of history; see api.historyLimit.
const pageSize = 100;

// The state of one message this tab is sending. A message becomes `Sent` by
// leaving this list: the server's copy is in `byId` and its id in `sentIds`.
export type SendStatus = 'sending' | 'failed' | 'unconfirmed';

export interface PendingSend {
  // A local key; the server's id is not known until the POST returns.
  readonly localId: number;
  readonly body: string;
  // When it was typed (ms since the epoch), for the time shown beside it.
  readonly at: number;
  readonly status: SendStatus;
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
  | { readonly type: 'received'; readonly channel: string; readonly message: MessageV2 }
  // A frame that was not a message of this schema or channel.
  | { readonly type: 'dropped'; readonly channel: string }
  | { readonly type: 'sending'; readonly channel: string; readonly body: string; readonly at: number }
  // The POST returned: the server's copy replaces the local one, by id.
  | { readonly type: 'sent'; readonly channel: string; readonly localId: number; readonly message: MessageV2 }
  | { readonly type: 'sendFailed'; readonly channel: string; readonly localId: number; readonly outcome: 'failed' | 'unconfirmed' }
  // A click on Retry: the same text goes out again, in the same place.
  | { readonly type: 'retrying'; readonly channel: string; readonly localId: number }
  | { readonly type: 'discard'; readonly channel: string; readonly localId: number }
  | { readonly type: 'olderLoading'; readonly channel: string }
  | { readonly type: 'olderLoaded'; readonly channel: string; readonly messages: readonly MessageV2[]; readonly more: boolean }
  | { readonly type: 'olderFailed'; readonly channel: string }
  | { readonly type: 'reset' };

// mergeMessages is the TUI's: a map by id, so a message held already is
// replaced by itself and never doubled.
function merge(existing: ReadonlyMap<number, MessageV2>, incoming: readonly MessageV2[]): ReadonlyMap<number, MessageV2> {
  if (incoming.length === 0) return existing;
  const merged = new Map(existing);
  for (const message of incoming) merged.set(message.id, message);
  return merged;
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

export function chatReducer(state: ChatState, action: ChatAction): ChatState {
  switch (action.type) {
    case 'historyLoading':
      return update(state, action.channel, (c) => ({ ...c, history: 'loading' }));
    case 'historyLoaded':
      // The page replaces what was held: held messages from an earlier open may
      // be far below it, and keeping them would hide a gap. Older history is
      // reached again with Load older. Pending sends and sentIds stay.
      return update(state, action.channel, (c) => ({
        ...hold({ ...c, byId: new Map() }, action.messages),
        history: 'ready',
        older: 'idle',
        trimmed: false,
        exhausted: !action.more,
      }));
    case 'historyFailed':
      return update(state, action.channel, (c) => ({ ...c, history: 'error' }));
    case 'received':
      return update(state, action.channel, (c) => hold(c, [action.message]));
    case 'dropped':
      return update(state, action.channel, (c) => ({ ...c, dropped: c.dropped + 1 }));
    case 'sending':
      return update(state, action.channel, (c) => ({
        ...c,
        pending: [...c.pending, { localId: c.nextLocalId, body: action.body, at: action.at, status: 'sending' }],
        nextLocalId: c.nextLocalId + 1,
      }));
    case 'sent':
      return known(state, action.channel, (c) => {
        if (!c.pending.some((p) => p.localId === action.localId)) return c;
        return {
          ...hold(c, [action.message]),
          pending: c.pending.filter((p) => p.localId !== action.localId),
          sentIds: new Set(c.sentIds).add(action.message.id),
        };
      });
    case 'sendFailed':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.map((p) => (p.localId === action.localId ? { ...p, status: action.outcome } : p)),
      }));
    case 'retrying':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.map((p) =>
          p.localId === action.localId && p.status !== 'sending' ? { ...p, status: 'sending' } : p,
        ),
      }));
    case 'discard':
      return known(state, action.channel, (c) => ({
        ...c,
        pending: c.pending.filter((p) => p.localId !== action.localId),
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
    case 'reset':
      return initialChat;
  }
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
