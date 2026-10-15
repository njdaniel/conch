// The person's read state (UX-02): the unread count of each channel in the list,
// and the mark that says how far they have read. The server keeps one mark per
// person, shared by all their clients; the counts here are its last answer plus
// a local +1 for each message that arrives unseen in the open channel. The rules
// are the terminal client's.
import { useCallback, useEffect, useRef, useState } from 'react';
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

interface Marks {
  // The newest id a mark was asked for, per channel. A request that fails is
  // not repeated until a newer message is shown.
  marked: Map<string, number>;
  // The mark the server last confirmed. A read it served before that is older
  // than the person's own reading and must not bring a count back.
  acked: Map<string, number>;
  markedAt: Map<string, number>;
  pending: Map<string, number>;
  timers: Map<string, number>;
  // When counts were last read, and the timer that reads once the interval
  // is over, for opening channels quickly one after another.
  readAt: number;
  readTimer: number | null;
}

const newMarks = (): Marks => ({ marked: new Map(), acked: new Map(), markedAt: new Map(), pending: new Map(), timers: new Map(), readAt: 0, readTimer: null });

const none: ReadonlyMap<string, number> = new Map();

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
}): ReadonlyMap<string, number> {
  const [counts, setCounts] = useState<ReadonlyMap<string, number>>(new Map());
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
  const latest = useRef({ channels, open, ready, reading, newest, connected: status === 'open', onUnauthorized });
  latest.current = { channels, open, ready, reading, newest, connected: status === 'open', onUnauthorized };

  const setCount = useCallback((name: string, count: number) => {
    setCounts((prev) => {
      if ((prev.get(name) ?? 0) === count) return prev;
      const next = new Map(prev);
      if (count > 0) next.set(name, Math.min(count, cap));
      else next.delete(name);
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
      const names = new Map(known.map((c) => [c.id, c.name]));
      const next = new Map<string, number>();
      for (const state of result.value) {
        const name = names.get(state.channel_id);
        if (name !== undefined && state.unread > 0 && state.last_read_id >= (marks.current.acked.get(name) ?? 0)) {
          next.set(name, Math.min(state.unread, cap));
        }
      }
      // The server counted before it had our mark: what is on screen and
      // already marked is not unread.
      if (current !== null && now && (marks.current.marked.get(current) ?? 0) > 0) next.delete(current);
      // A channel just opened, whose newest message is not yet known, is about
      // to be read: its count is not shown in the meantime.
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
    (name: string, id: number) => {
      const m = marks.current;
      m.marked.set(name, id);
      m.markedAt.set(name, Date.now());
      m.pending.delete(name);
      const asked = epoch.current;
      void api.markRead(name, id).then((result) => {
        if (epoch.current !== asked || off.current) return;
        if (!result.ok) {
          // No answer: the mark is not made. Taken back, and kept to be sent
          // when the socket is.
          if (result.failure === 'network') {
            m.marked.set(name, m.acked.get(name) ?? 0);
            m.pending.set(name, Math.max(m.pending.get(name) ?? 0, id));
          }
          failed(result);
          return;
        }
        m.acked.set(name, Math.max(m.acked.get(name) ?? 0, result.value.last_read_id));
        const { open: current, reading: now, newest: head } = latest.current;
        // A newer message is on screen and its mark is on the way: this count is
        // already out of date.
        if (name === current && now && (head?.id ?? 0) > result.value.last_read_id) return;
        setCount(name, result.value.unread);
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
  const was = useRef<{ status: LiveStatus; open: string | null }>({ status, open });
  useEffect(() => {
    const before = was.current;
    was.current = { status, open };
    if (!signedIn || !loaded) return;
    if (status === 'open' && before.status === 'reconnecting') {
      read();
      // The mark held back while the server was out of reach goes now.
      const m = marks.current;
      const id = open === null ? 0 : (m.pending.get(open) ?? 0);
      if (open !== null && id > (m.marked.get(open) ?? 0) && latest.current.reading) send(open, id);
    } else if (open !== null && before.open !== null && open !== before.open) readSoon();
  }, [signedIn, loaded, status, open, read, readSoon, send]);

  // A message that arrives while it is not being read is a local +1. Your own
  // messages are not unread.
  const seen = useRef<{ readonly name: string; readonly id: number } | null>(null);
  const newestId = newest?.id ?? 0;
  const newestAuthor = newest?.author ?? 0;
  useEffect(() => {
    if (!signedIn || open === null || !ready) return;
    const before = seen.current;
    seen.current = { name: open, id: newestId };
    if (before === null || before.name !== open || newestId <= before.id) return;
    if (reading || newestAuthor === me || off.current) return;
    setCounts((prev) => new Map(prev).set(open, Math.min((prev.get(open) ?? 0) + 1, cap)));
  }, [signedIn, open, ready, newestId, newestAuthor, reading, me]);

  // The mark moves to the newest message shown: when a channel is opened, when
  // a message arrives at the bottom, and when the newest comes back into view.
  useEffect(() => {
    if (!signedIn || open === null || !ready || !reading || off.current) return;
    setCount(open, 0);
    const m = marks.current;
    if (newestId <= (m.marked.get(open) ?? 0)) return;
    // Not while the socket is down: the server may be out of reach, and a mark
    // lost then would be taken for made. It is held, and sent on reconnect.
    if (status !== 'open') {
      m.pending.set(open, newestId);
      return;
    }
    const now = Date.now();
    const last = m.markedAt.get(open);
    if (last === undefined || now - last >= markInterval) {
      send(open, newestId);
      return;
    }
    // Too soon: the newest id wins when the interval is over.
    m.pending.set(open, newestId);
    if (m.timers.has(open)) return;
    const name = open;
    m.timers.set(
      name,
      window.setTimeout(() => {
        m.timers.delete(name);
        const id = m.pending.get(name) ?? 0;
        if (!off.current && latest.current.connected && id > (m.marked.get(name) ?? 0)) send(name, id);
      }, last + markInterval - now),
    );
  }, [signedIn, open, ready, reading, newestId, status, send, setCount]);

  return off.current ? none : counts;
}
