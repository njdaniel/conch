// Keeps the open channel live: reads its history, then follows the socket from
// the newest id held, and tells the page how the connection is. The history
// comes first so the socket's `after` is known; a message posted in between is
// replayed by the socket and, being the same id, is shown once.
import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch } from 'react';
import * as api from './api';
import { Feed, type FeedStatus } from './state/feed';
import type { ConnectionAction } from './state/connection';
import { parseFrame, type ChatAction } from './state/messages';
import type { ChannelMemberV1 } from './schema/channel';
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
  // The nets the viewer only monitors: they cannot transmit to them.
  readonly monitored: ReadonlySet<number>;
  readonly retry: () => void;
  // Asks for the page below the oldest message held; a 401 signs out.
  readonly loadOlder: () => void;
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
  members,
  getAfter,
  getOldest,
  dispatchChat,
  dispatchConnection,
  onUnauthorized,
  onLost,
}: {
  readonly channel: string | null;
  readonly channelId: number | null;
  readonly me: { readonly id: number; readonly name: string; readonly kind: PrincipalKind };
  // The channel's members, as the people panel holds them (useAgents): read
  // when the channel opens and kept current by the events socket, so a name
  // that changes or leaves (a hidden account, #515) changes here too.
  readonly members: readonly ChannelMemberV1[];
  readonly getAfter: (channel: string) => number;
  // The id to read below, or null when no older page is to be asked for now.
  readonly getOldest: (channel: string) => number | null;
  readonly dispatchChat: Dispatch<ChatAction>;
  readonly dispatchConnection: Dispatch<ConnectionAction>;
  readonly onUnauthorized: () => void;
  readonly onLost: (channel: string) => void;
}): Live {
  const [status, setStatus] = useState<LiveStatus>('loading');
  const [nets, setNets] = useState<ReadonlyMap<number, string>>(new Map());
  const [monitored, setMonitored] = useState<ReadonlySet<number>>(new Set());
  const [attempt, setAttempt] = useState(0);
  // Moved on when the effect below is cleaned up (another channel, a retry,
  // sign-out): an older page asked for under an earlier one is then dropped.
  const generation = useRef(0);
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
    setNets(new Map());
    setMonitored(new Set());

    void api.listNets(channel).then((result) => {
      if (!live || !result.ok) return;
      setNets(new Map(result.value.map((n) => [n.id, n.name])));
      setMonitored(new Set(result.value.filter((n) => n.members.some((m) => m.principal_id === meId && m.role === 'monitor')).map((n) => n.id)));
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
          // 1008 is lost access, a revoked credential, or a reader too slow;
          // the close reason is not read. Ask who we are, then whether the
          // channel is still ours. Anything unsettled waits out the backoff.
          setStatus('reconnecting');
          void api.whoAmI().then((who) => {
            if (!live) return;
            if (!who.ok) {
              if (who.failure === 'unauthorized') onUnauthorized();
              else feed?.retryLater();
              return;
            }
            void api.listChannels().then((list) => {
              if (!live) return;
              if (!list.ok) {
                if (list.failure === 'unauthorized') onUnauthorized();
                else feed?.retryLater();
              } else if (list.value.some((c) => c.name === name)) feed?.retryLater();
              else onLost(name);
            });
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
      dispatchChat({ type: 'historyLoaded', channel, messages: result.value, more: result.value.length >= api.historyLimit });
      feed = new Feed({
        channel,
        open: api.openSocket,
        clock: realClock,
        after: () => getAfter(channel),
        onFrame: (data) => {
          const message = parseFrame(data, id);
          if (message === null) dispatchChat({ type: 'dropped', channel });
          else dispatchChat({ type: 'received', channel, message, own: message.author_id === me.id });
        },
        onStatus,
      });
      feed.start();
    });

    return () => {
      generation.current++;
      live = false;
      feed?.stop();
    };
  }, [channel, channelId, attempt, meId, getAfter, dispatchChat, dispatchConnection, onUnauthorized, onLost]);

  // Names are a convenience: without them an author reads as #<id>. The
  // viewer's own name is the one they are signed in with, which a rename of
  // their own changes before the member list does.
  const people = useMemo(() => {
    const named = new Map<number, Person>();
    for (const m of members) {
      if (m.name !== undefined) named.set(m.principal_id, { name: m.name, kind: m.kind ?? null });
    }
    named.set(meId, { name: meName, kind: meKind });
    return named;
  }, [members, meId, meName, meKind]);

  const retry = useCallback(() => {
    setAttempt((n) => n + 1);
  }, []);

  const loadOlder = useCallback(() => {
    if (channel === null) return;
    const before = getOldest(channel);
    if (before === null) return;
    const asked = generation.current;
    dispatchChat({ type: 'olderLoading', channel });
    void api.loadMessages(channel, before).then((result) => {
      if (generation.current !== asked) return;
      if (!result.ok) {
        if (result.failure === 'unauthorized') {
          onUnauthorized();
          return;
        }
        if (result.failure === 'network') dispatchConnection({ type: 'lost' });
        dispatchChat({ type: 'olderFailed', channel });
        return;
      }
      dispatchChat({ type: 'olderLoaded', channel, messages: result.value, more: result.value.length >= api.historyLimit });
    });
  }, [channel, getOldest, dispatchChat, dispatchConnection, onUnauthorized]);

  return { status, people, nets, monitored, retry, loadOlder };
}
