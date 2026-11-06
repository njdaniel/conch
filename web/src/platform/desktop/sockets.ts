// The desktop's SocketTransport (issue #943): a live feed is a subscription of
// window.conchDesktop (./bridge), which main opens, holds and validates. The
// renderer names the feed and its parameters; it never holds the socket, its
// address, a ticket or the bearer. ticket is false, so the client asks for no
// ticket (main sends the bearer header itself).
//
// What comes back is a FeedConnection like a WebSocket's: an open, the text
// frames, and one close with its code and reason, so the client's reconnect
// and silence watchdog (../../state/feed.ts) run here unchanged. close() ends
// the subscription and, as for a socket whose handlers the client has
// removed, nothing more is reported.
import type { OperationId } from '../operations';
import type { FeedConnection, SocketTransport } from '../types';
import type { FeedKind, FeedRequest } from './bridge';
import { bridge } from './access';

const kinds: Partial<Record<OperationId, FeedKind>> = {
  messagesSocket: 'messages',
  eventsSocket: 'events',
  voiceSocket: 'voice',
  occupancySocket: 'occupancy',
};

function count(value: string | undefined): number | undefined {
  if (value === undefined || !/^\d{1,15}$/.test(value)) return undefined;
  return Number(value);
}

// feedOf is the subscription the client's query names: the feed of the
// operation, and its channel, session and cursor. The rest of the query (the
// messages feed's heartbeat) is main's to add.
export function feedOf(id: OperationId, query: Readonly<Record<string, string>>): FeedRequest | null {
  const kind = kinds[id];
  if (kind === undefined) return null;
  const channel = query['channel'];
  const session = count(query['session']);
  const after = count(query['after']);
  return {
    kind,
    ...(channel === undefined ? {} : { channel }),
    ...(session === undefined ? {} : { session }),
    ...(after === undefined ? {} : { after }),
  };
}

export const sockets: SocketTransport = {
  ticket: false,
  open(id, query) {
    const b = bridge();
    if (b === undefined) throw new TypeError('the desktop bridge is missing');
    const feed = feedOf(id, query);
    if (feed === null) throw new TypeError(`${id} is not a live feed`);
    let ended = false;
    const connection: FeedConnection = {
      onopen: null,
      onmessage: null,
      onclose: null,
      close: () => {
        if (ended) return;
        ended = true;
        unsubscribe();
      },
    };
    const unsubscribe = b.subscribe(feed, {
      onOpen: () => {
        if (!ended) connection.onopen?.(new Event('open'));
      },
      onFrame: (frame) => {
        if (!ended) connection.onmessage?.(new MessageEvent('message', { data: frame }));
      },
      onClose: (code, reason) => {
        if (ended) return;
        ended = true;
        connection.onclose?.(new CloseEvent('close', { code, reason }));
      },
    });
    return connection;
  },
};
