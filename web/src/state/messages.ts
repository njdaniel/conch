// The messages of each channel the tab has opened: what the server has told us,
// and what this tab has sent that the server has not yet confirmed. Pure
// reducer; src/api.ts and the socket feed it through App.
import { messageV2, type MessageV2 } from '../schema/message';

// How many of the newest messages are drawn, and how many more each step back
// through older history draws.
export const pageSize = 100;

// The most messages held per channel: the newest. Older ones are dropped (a
// pending send of our own is kept apart and never dropped).
export const maxHeld = 2000;

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

export interface ChannelChat {
  // The id is the identity of a message: one that arrives twice is one.
  readonly byId: ReadonlyMap<number, MessageV2>;
  readonly pending: readonly PendingSend[];
  // Messages this tab sent and the POST confirmed, shown as `Sent`.
  readonly sentIds: ReadonlySet<number>;
  readonly history: HistoryStatus;
  // How many of the newest messages are drawn.
  readonly shown: number;
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
  shown: pageSize,
  trimmed: false,
  dropped: 0,
  nextLocalId: 1,
};

export type ChatAction =
  | { readonly type: 'historyLoading'; readonly channel: string }
  | { readonly type: 'historyLoaded'; readonly channel: string; readonly messages: readonly MessageV2[] }
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
  | { readonly type: 'showOlder'; readonly channel: string }
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
      return update(state, action.channel, (c) => ({ ...hold(c, action.messages), history: 'ready' }));
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
    case 'showOlder':
      return known(state, action.channel, (c) => ({ ...c, shown: c.shown + pageSize }));
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
