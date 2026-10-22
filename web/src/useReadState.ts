// The person's read state (UX-02): the unread count of each channel in the list,
// and the mark that says how far they have read. The server keeps one mark per
// person, shared by all their clients; the counts here are its last answer plus
// a local +1 for each message that arrives unseen in the open channel. The rules
// are the terminal client's.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import * as api from './api';
import type { ChannelV0 } from './schema/channel';
import type { LiveStatus } from './useLiveChannel';

// The least time between two mark requests for one channel; the newest id wins
// when it is over.
export const markInterval = 2000;

// The server's cap on a count (schema.MaxUnreadCount): a count at it means
// "at least this many".
const cap = 1000;

// badgeText is what a count looks like: nothing at 0, 999+ at the cap.
export function badgeText(count: number): string | null {
  if (count <= 0) return null;
  return count >= cap ? `${String(cap - 1)}+` : String(count);
}

// The newest message of the open channel that the page holds.
export interface Newest {
  readonly id: number;
  readonly author: number;
}

// Everything here is kept by channel id, not name, so a channel renamed while
// open (issue #728) keeps its mark, its count and the timer of a mark on its
// way. The name is looked up only to ask the server, at the moment of asking.
interface Marks {
  // The newest id a mark was asked for, per channel. A request that fails is
  // not repeated until a newer message is shown.
  marked: Map<number, number>;
  // The mark the server last confirmed. A read it served before that is older
  // than the person's own reading and must not bring a count back.
  acked: Map<number, number>;
  markedAt: Map<number, number>;
  pending: Map<number, number>;
  timers: Map<number, number>;
  // When counts were last read, and the timer that reads once the interval
  // is over, for opening channels quickly one after another.
  readAt: number;
  readTimer: number | null;
}

const newMarks = (): Marks => ({ marked: new Map(), acked: new Map(), markedAt: new Map(), pending: new Map(), timers: new Map(), readAt: 0, readTimer: null });

const none: ReadonlyMap<string, number> = new Map();

const idOf = (channels: readonly ChannelV0[], name: string | null): number | null =>
  name === null ? null : (channels.find((c) => c.name === name)?.id ?? null);

export function useReadState({
  signedIn,
  channels,
  loaded,
  open,
  ready,
  newest,
  atBottom,
  status,
  me,
  onUnauthorized,
  onMissing,
}: {
  readonly signedIn: boolean;
  readonly channels: readonly ChannelV0[];
  // Whether the channel list has been read.
  readonly loaded: boolean;
  readonly open: string | null;
  // Whether the open channel's history is held, so `newest` is its newest.
  readonly ready: boolean;
  readonly newest: Newest | null;
  // Whether the reader is at the newest message of the open channel.
  readonly atBottom: boolean;
  readonly status: LiveStatus;
  readonly me: number;
  readonly onUnauthorized: () => void;
  // A mark refused with channel_not_found (issue #728): the channel, by id,
  // may have been renamed since the list was read.
  readonly onMissing?: (channel: number) => void;
}): ReadonlyMap<string, number> {
  const [counts, setCounts] = useState<ReadonlyMap<number, number>>(new Map());
  const openId = idOf(channels, open);
  const [visible, setVisible] = useState(() => document.visibilityState !== 'hidden');
  // Reading means the newest message is on screen: at the bottom, in a tab that
  // can be seen.
  const reading = atBottom && visible;

  const marks = useRef(newMarks());
  // Set when the server says it holds no read state for this credential (an
  // agent, or authentication off): from then on nothing is asked or shown.
  const off = useRef(false);
  // Moved on at sign-out: an answer to a question asked before is dropped.
  const epoch = useRef(0);
  // What the callbacks below read, as of the last render.
  const latest = useRef({ channels, open: openId, ready, reading, newest, connected: status === 'open', onUnauthorized, onMissing });
  latest.current = { channels, open: openId, ready, reading, newest, connected: status === 'open', onUnauthorized, onMissing };

  const setCount = useCallback((channel: number, count: number) => {
    setCounts((prev) => {
      if ((prev.get(channel) ?? 0) === count) return prev;
      const next = new Map(prev);
      if (count > 0) next.set(channel, Math.min(count, cap));
      else next.delete(channel);
      return next;
    });
  }, []);

  // A failed call says nothing, except these: a 401 signs out, and a refusal of
  // this credential turns read state off.
  const failed = useCallback((result: { readonly failure: api.ApiFailure; readonly code?: string }) => {
    if (result.failure === 'unauthorized') latest.current.onUnauthorized();
    else if (result.code !== undefined && api.readStateRefusals.includes(result.code)) {
      off.current = true;
      setCounts(new Map());
    }
  }, []);

  const read = useCallback(() => {
    if (off.current) return;
    marks.current.readAt = Date.now();
    const asked = epoch.current;
    void api.listReadState().then((result) => {
      if (epoch.current !== asked || off.current) return;
      if (!result.ok) {
        failed(result);
        return;
      }
      const { channels: known, open: current, ready: held, reading: now } = latest.current;
      const ids = new Set(known.map((c) => c.id));
      const next = new Map<number, number>();
      for (const state of result.value) {
        const id = state.channel_id;
        if (ids.has(id) && state.unread > 0 && state.last_read_id >= (marks.current.acked.get(id) ?? 0)) {
          next.set(id, Math.min(state.unread, cap));
        }
      }
      // The server counted before it had our mark: what is on screen and
      // already marked is not unread.
      if (current !== null && now && (marks.current.marked.get(current) ?? 0) > 0) next.delete(current);
      // A channel just opened, whose newest message is not yet known, is about
      // to be read: its count is dropped while its history loads, so that it
      // does not flicker. The trade-off: if it turns out not to be read (the
      // reader is scrolled up, say), the count comes back only with the next
      // read.
      if (current !== null && !held) next.delete(current);
      setCounts(next);
    });
  }, [failed]);

  // readSoon is read for a channel just opened: at most once per interval, the
  // last ask winning.
  const readSoon = useCallback(() => {
    const m = marks.current;
    const wait = m.readAt + markInterval - Date.now();
    if (wait <= 0) {
      read();
      return;
    }
    if (m.readTimer !== null) return;
    m.readTimer = window.setTimeout(() => {
      m.readTimer = null;
      read();
    }, wait);
  }, [read]);

  const send = useCallback(
    (channel: number, id: number) => {
      // The channel's name as the list holds it now: a mark asked for after a
      // rename goes to the new one.
      const name = latest.current.channels.find((c) => c.id === channel)?.name;
      if (name === undefined) return;
      const m = marks.current;
      m.marked.set(channel, id);
      m.markedAt.set(channel, Date.now());
      m.pending.delete(channel);
      const asked = epoch.current;
      void api.markRead(name, id).then((result) => {
        if (epoch.current !== asked || off.current) return;
        if (!result.ok) {
          // No answer, or the channel's name gone (renamed): the mark is not
          // made. Taken back, and kept to be sent when the socket is.
          const missing = result.code === api.channelNotFound;
          if (result.failure === 'network' || missing) {
            m.marked.set(channel, m.acked.get(channel) ?? 0);
            m.pending.set(channel, Math.max(m.pending.get(channel) ?? 0, id));
          }
          if (missing) latest.current.onMissing?.(channel);
          failed(result);
          return;
        }
        m.acked.set(channel, Math.max(m.acked.get(channel) ?? 0, result.value.last_read_id));
        const { open: current, reading: now, newest: head } = latest.current;
        // A newer message is on screen and its mark is on the way: this count is
        // already out of date.
        if (channel === current && now && (head?.id ?? 0) > result.value.last_read_id) return;
        setCount(channel, result.value.unread);
      });
    },
    [failed, setCount],
  );

  // Whether the tab can be seen.
  useEffect(() => {
    const onChange = () => {
      setVisible(document.visibilityState !== 'hidden');
    };
    document.addEventListener('visibilitychange', onChange);
    return () => {
      document.removeEventListener('visibilitychange', onChange);
    };
  }, []);

  // Signing out forgets everything, and signing in starts clean.
  useEffect(() => {
    if (!signedIn) return;
    return () => {
      epoch.current++;
      for (const timer of marks.current.timers.values()) window.clearTimeout(timer);
      if (marks.current.readTimer !== null) window.clearTimeout(marks.current.readTimer);
      marks.current = newMarks();
      off.current = false;
      setCounts(new Map());
    };
  }, [signedIn]);

  // The counts are read once the channels are known, again after a reconnect,
  // and when another channel is opened (the page has no socket for the others,
  // so this is when it can learn what came in there).
  useEffect(() => {
    if (signedIn && loaded) read();
  }, [signedIn, loaded, read]);
  // By id: the open channel renamed is not another channel opened.
  const was = useRef<{ status: LiveStatus; open: number | null }>({ status, open: openId });
  useEffect(() => {
    const before = was.current;
    was.current = { status, open: openId };
    if (!signedIn || !loaded) return;
    if (status === 'open' && before.status === 'reconnecting') {
      read();
    } else if (openId !== null && before.open !== null && openId !== before.open) readSoon();
  }, [signedIn, loaded, status, openId, read, readSoon]);

  // A message that arrives while it is not being read is a local +1. Your own
  // messages are not unread.
  const seen = useRef<{ readonly channel: number; readonly id: number } | null>(null);
  const newestId = newest?.id ?? 0;
  const newestAuthor = newest?.author ?? 0;
  useEffect(() => {
    if (!signedIn || openId === null || !ready) return;
    const before = seen.current;
    seen.current = { channel: openId, id: newestId };
    if (before === null || before.channel !== openId || newestId <= before.id) return;
    if (reading || newestAuthor === me || off.current) return;
    setCounts((prev) => new Map(prev).set(openId, Math.min((prev.get(openId) ?? 0) + 1, cap)));
  }, [signedIn, openId, ready, newestId, newestAuthor, reading, me]);

  // The mark moves to the newest message shown: when a channel is opened, when
  // a message arrives at the bottom, and when the newest comes back into view.
  useEffect(() => {
    if (!signedIn || openId === null || !ready || !reading || off.current) return;
    setCount(openId, 0);
    const m = marks.current;
    if (newestId <= (m.marked.get(openId) ?? 0)) return;
    // Not while the socket is down: the server may be out of reach, and a mark
    // lost then would be taken for made. It is held, and sent on reconnect.
    if (status !== 'open') {
      m.pending.set(openId, newestId);
      return;
    }
    const now = Date.now();
    const last = m.markedAt.get(openId);
    if (last === undefined || now - last >= markInterval) {
      send(openId, newestId);
      return;
    }
    // Too soon: the newest id wins when the interval is over.
    m.pending.set(openId, newestId);
    if (m.timers.has(openId)) return;
    const channel = openId;
    m.timers.set(
      channel,
      window.setTimeout(() => {
        m.timers.delete(channel);
        const id = m.pending.get(channel) ?? 0;
        if (!off.current && latest.current.connected && id > (m.marked.get(channel) ?? 0)) send(channel, id);
      }, last + markInterval - now),
    );
  }, [signedIn, openId, ready, reading, newestId, status, send, setCount]);

  // The counts by name, as the list shows them, from the names it holds now.
  const byName = useMemo(() => {
    const named = new Map<string, number>();
    for (const c of channels) {
      const count = counts.get(c.id);
      if (count !== undefined) named.set(c.name, count);
    }
    return named;
  }, [channels, counts]);

  return off.current ? none : byName;
}
