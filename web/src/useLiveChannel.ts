// Keeps the open channel live: reads its history, then follows the socket from
// the newest id held, and tells the page how the connection is. The history
// comes first so the socket's `after` is known; a message posted in between is
// replayed by the socket and, being the same id, is shown once.
import { useCallback, useEffect, useMemo, useRef, useState, type Dispatch } from 'react';
import * as api from './api';
import { memberName } from './guestName';
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
  // The channel whose nets `nets` is the answer for, or null before it comes:
  // an empty list is only "no nets" once this is the open channel's id.
  readonly netsFor: number | null;
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
  nameOf,
  me,
  members,
  getAfter,
  getOldest,
  dispatchChat,
  dispatchConnection,
  onUnauthorized,
  onLost,
  onRenamed,
  onMissing,
}: {
  readonly channel: string | null;
  readonly channelId: number | null;
  // The channel's name now, by id, as the page holds it: what each request,
  // reconnect and dispatch names, so a channel renamed while open (issue #728)
  // keeps its socket and its history and the next call uses the new name.
  readonly nameOf: (channelId: number) => string | null;
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
  // The list read after a 1008 holds the channel under another name.
  readonly onRenamed: (channelId: number, name: string) => void;
  // A call for the channel answered channel_not_found: it may have been
  // renamed under this page, which the page finds out by reading the list.
  readonly onMissing: (channelId: number) => void;
}): Live {
  const [status, setStatus] = useState<LiveStatus>('loading');
  const [nets, setNets] = useState<ReadonlyMap<number, string>>(new Map());
  const [monitored, setMonitored] = useState<ReadonlySet<number>>(new Set());
  const [netsFor, setNetsFor] = useState<number | null>(null);
  const [attempt, setAttempt] = useState(0);
  // Moved on when the effect below is cleaned up (another channel, a retry,
  // sign-out): an older page asked for under an earlier one is then dropped.
  const generation = useRef(0);
  const meId = me.id;
  const meName = me.name;
  const meKind = me.kind;

  // Keyed on the channel's id, not its name: a rename of the open channel
  // neither reloads its history nor reopens its socket.
  const open = channel !== null;
  useEffect(() => {
    if (!open || channelId === null) return;
    const id = channelId;
    // The name as it was when the channel was opened, for a page whose list
    // no longer holds the id (it is lost, and this effect about to end).
    const opened = nameOf(id);
    if (opened === null) return;
    const name = () => nameOf(id) ?? opened;
    let live = true;
    let feed: Feed | null = null;
    let reached = false;
    setStatus('loading');
    setNets(new Map());
    setMonitored(new Set());
    setNetsFor(null);

    void api.listNets(name()).then((result) => {
      if (!live) return;
      if (!result.ok) {
        if (result.code === api.channelNotFound) onMissing(id);
        return;
      }
      setNets(new Map(result.value.map((n) => [n.id, n.name])));
      setNetsFor(id);
      setMonitored(new Set(result.value.filter((n) => n.members.some((m) => m.principal_id === meId && m.role === 'monitor')).map((n) => n.id)));
    });

    function onStatus(next: FeedStatus) {
      if (!live) return;
      switch (next) {
        case 'open':
          reached = true;
          setStatus('open');
          dispatchConnection({ type: 'reached' });
          break;
        case 'connecting':
          if (reached) setStatus('reconnecting');
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
              const found = list.ok ? list.value.find((c) => c.id === id) : undefined;
              if (!list.ok) {
                if (list.failure === 'unauthorized') onUnauthorized();
                else feed?.retryLater();
              } else if (found !== undefined) {
                // Still ours, by id: under a new name, if it was renamed.
                if (found.name !== name()) onRenamed(id, found.name);
                feed?.retryLater();
              } else onLost(name());
            });
          });
          break;
      }
    }

    dispatchChat({ type: 'historyLoading', channel: name() });
    void api.loadMessages(name()).then((result) => {
      if (!live) return;
      if (!result.ok) {
        if (result.failure === 'unauthorized') {
          onUnauthorized();
          return;
        }
        if (result.failure === 'network') dispatchConnection({ type: 'lost' });
        if (result.code === api.channelNotFound) onMissing(id);
        dispatchChat({ type: 'historyFailed', channel: name() });
        setStatus('failed');
        return;
      }
      dispatchChat({ type: 'historyLoaded', channel: name(), messages: result.value, more: result.value.length >= api.historyLimit });
      feed = new Feed({
        channel: opened,
        // Each attempt names the channel as it is called then.
        open: (_channel, after, events) => api.openSocket(name(), after, events),
        clock: realClock,
        after: () => getAfter(name()),
        onFrame: (data) => {
          const message = parseFrame(data, id);
          if (message === null) dispatchChat({ type: 'dropped', channel: name() });
          else dispatchChat({ type: 'received', channel: name(), message, own: message.author_id === meId });
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
  }, [open, channelId, nameOf, attempt, meId, getAfter, dispatchChat, dispatchConnection, onUnauthorized, onLost, onRenamed, onMissing]);

  // Names are a convenience: without them an author reads as #<id>. The
  // viewer's own name is the one they are signed in with, which a rename of
  // their own changes before the member list does.
  const people = useMemo(() => {
    const named = new Map<number, Person>();
    for (const m of members) {
      const name = memberName(m);
      if (name !== undefined) named.set(m.principal_id, { name, kind: m.kind ?? null });
    }
    named.set(meId, { name: meName, kind: meKind });
    return named;
  }, [members, meId, meName, meKind]);

  const retry = useCallback(() => {
    setAttempt((n) => n + 1);
  }, []);

  const loadOlder = useCallback(() => {
    if (channelId === null) return;
    const id = channelId;
    const name = () => nameOf(id);
    const asking = name();
    if (asking === null) return;
    const before = getOldest(asking);
    if (before === null) return;
    const asked = generation.current;
    dispatchChat({ type: 'olderLoading', channel: asking });
    void api.loadMessages(asking, before).then((result) => {
      // Answered after a rename: what it holds goes under the name now.
      const now = name();
      if (generation.current !== asked || now === null) return;
      if (!result.ok) {
        if (result.failure === 'unauthorized') {
          onUnauthorized();
          return;
        }
        if (result.failure === 'network') dispatchConnection({ type: 'lost' });
        if (result.code === api.channelNotFound) onMissing(id);
        dispatchChat({ type: 'olderFailed', channel: now });
        return;
      }
      dispatchChat({ type: 'olderLoaded', channel: now, messages: result.value, more: result.value.length >= api.historyLimit });
    });
  }, [channelId, nameOf, getOldest, dispatchChat, dispatchConnection, onUnauthorized, onMissing]);

  return { status, people, nets, monitored, netsFor, retry, loadOlder };
}
