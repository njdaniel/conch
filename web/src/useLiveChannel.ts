// Keeps the open channel live: reads its history, then follows the socket from
// the newest id held, and tells the page how the connection is. The history
// comes first so the socket's `after` is known; a message posted in between is
// replayed by the socket and, being the same id, is shown once.
import { useCallback, useEffect, useState, type Dispatch } from 'react';
import * as api from './api';
import { Feed, type FeedStatus } from './state/feed';
import type { ConnectionAction } from './state/connection';
import { parseFrame, type ChatAction } from './state/messages';
import type { PrincipalKind } from './schema/principal';

// loading: reading history or connecting for the first time. reconnecting: the
// socket closed and the client is retrying, so what is shown may be out of
// date. failed: history could not be read; nothing is retried until asked.
export type LiveStatus = 'loading' | 'open' | 'reconnecting' | 'failed';

export interface Person {
  readonly name: string;
  readonly kind: PrincipalKind | null;
}

export interface Live {
  readonly status: LiveStatus;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly retry: () => void;
}

const realClock = {
  setTimeout: (run: () => void, ms: number) => window.setTimeout(run, ms),
  clearTimeout: (handle: unknown) => {
    window.clearTimeout(handle as number);
  },
};

export function useLiveChannel({
  channel,
  channelId,
  me,
  getAfter,
  dispatchChat,
  dispatchConnection,
  onUnauthorized,
  onLost,
}: {
  readonly channel: string | null;
  readonly channelId: number | null;
  readonly me: { readonly id: number; readonly name: string; readonly kind: PrincipalKind };
  readonly getAfter: (channel: string) => number;
  readonly dispatchChat: Dispatch<ChatAction>;
  readonly dispatchConnection: Dispatch<ConnectionAction>;
  readonly onUnauthorized: () => void;
  readonly onLost: (channel: string) => void;
}): Live {
  const [status, setStatus] = useState<LiveStatus>('loading');
  const [people, setPeople] = useState<ReadonlyMap<number, Person>>(new Map());
  const [nets, setNets] = useState<ReadonlyMap<number, string>>(new Map());
  const [attempt, setAttempt] = useState(0);
  const meId = me.id;
  const meName = me.name;
  const meKind = me.kind;

  useEffect(() => {
    if (channel === null || channelId === null) return;
    const id = channelId;
    const name = channel;
    let live = true;
    let feed: Feed | null = null;
    let opened = false;
    setStatus('loading');
    setPeople(new Map([[meId, { name: meName, kind: meKind }]]));
    setNets(new Map());

    // Names are a convenience: without them an author reads as #<id>.
    void api.listMembers(channel).then((result) => {
      if (!live || !result.ok) return;
      const named = new Map<number, Person>([[meId, { name: meName, kind: meKind }]]);
      for (const m of result.value) {
        if (m.name !== undefined) named.set(m.principal_id, { name: m.name, kind: m.kind ?? null });
      }
      setPeople(named);
    });
    void api.listNets(channel).then((result) => {
      if (!live || !result.ok) return;
      setNets(new Map(result.value.map((n) => [n.id, n.name])));
    });

    function onStatus(next: FeedStatus) {
      if (!live) return;
      switch (next) {
        case 'open':
          opened = true;
          setStatus('open');
          dispatchConnection({ type: 'reached' });
          break;
        case 'connecting':
          if (opened) setStatus('reconnecting');
          break;
        case 'waiting':
          setStatus('reconnecting');
          dispatchConnection({ type: 'lost' });
          break;
        case 'unauthorized':
          onUnauthorized();
          break;
        case 'denied':
          // 1008: lost access, or a revoked credential. Ask who we are now.
          setStatus('reconnecting');
          void api.restore().then((who) => {
            if (!live) return;
            if (who.ok) onLost(name);
            else if (who.failure === 'unauthorized') onUnauthorized();
            else feed?.start();
          });
          break;
      }
    }

    dispatchChat({ type: 'historyLoading', channel });
    void api.loadMessages(channel).then((result) => {
      if (!live) return;
      if (!result.ok) {
        if (result.failure === 'unauthorized') {
          onUnauthorized();
          return;
        }
        if (result.failure === 'network') dispatchConnection({ type: 'lost' });
        dispatchChat({ type: 'historyFailed', channel });
        setStatus('failed');
        return;
      }
      dispatchChat({ type: 'historyLoaded', channel, messages: result.value });
      feed = new Feed({
        channel,
        open: api.openSocket,
        clock: realClock,
        after: () => getAfter(channel),
        onFrame: (data) => {
          const message = parseFrame(data, id);
          if (message === null) dispatchChat({ type: 'dropped', channel });
          else dispatchChat({ type: 'received', channel, message });
        },
        onStatus,
      });
      feed.start();
    });

    return () => {
      live = false;
      feed?.stop();
    };
  }, [channel, channelId, attempt, meId, meName, meKind, getAfter, dispatchChat, dispatchConnection, onUnauthorized, onLost]);

  const retry = useCallback(() => {
    setAttempt((n) => n + 1);
  }, []);

  return { status, people, nets, retry };
}
