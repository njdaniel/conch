import { describe, expect, it } from 'vitest';
import type { ApiResult } from '../api';
import { Feed, maxDelay, type Clock, type FeedSocket, type FeedStatus, type OpenSocket, type SocketEvents } from './feed';

// A clock that only moves when the test says so.
class FakeClock implements Clock {
  now = 0;
  private next = 1;
  private timers = new Map<number, { at: number; run: () => void }>();
  setTimeout(run: () => void, ms: number): unknown {
    const id = this.next++;
    this.timers.set(id, { at: this.now + ms, run });
    return id;
  }
  clearTimeout(handle: unknown): void {
    this.timers.delete(handle as number);
  }
  // pending is the delays still to run, from now.
  get pending(): number[] {
    return [...this.timers.values()].map((t) => t.at - this.now);
  }
  advance(ms: number): void {
    this.now += ms;
    for (const [id, t] of [...this.timers]) {
      if (t.at <= this.now) {
        this.timers.delete(id);
        t.run();
      }
    }
  }
}

interface Attempt {
  after: number;
  events: SocketEvents;
  closed: boolean;
}

// harness: each open() is one ticket + socket; the test decides how it ends.
function harness(after: () => number = () => 0) {
  const clock = new FakeClock();
  const attempts: Attempt[] = [];
  const resolvers: ((r: ApiResult<FeedSocket>) => void)[] = [];
  const statuses: FeedStatus[] = [];
  const frames: unknown[] = [];
  const open: OpenSocket = (_channel, a, events) =>
    new Promise((resolve) => {
      const attempt: Attempt = { after: a, events, closed: false };
      attempts.push(attempt);
      resolvers.push(resolve);
    });
  const feed = new Feed({ channel: 'ops', open, clock, after, onFrame: (d) => frames.push(d), onStatus: (s) => statuses.push(s) });
  const socketFor = (i: number): FeedSocket => ({
    close: () => {
      const attempt = attempts[i];
      if (attempt) attempt.closed = true;
    },
  });
  const settle = async (i: number, result: ApiResult<FeedSocket> = { ok: true, value: socketFor(i) }) => {
    resolvers[i]?.(result);
    await Promise.resolve();
    await Promise.resolve();
  };
  return { clock, attempts, statuses, frames, feed, settle };
}

describe('the reconnect machine', () => {
  it('opens, reports open, and passes frames on', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.attempts[0]?.events.onOpen();
    h.attempts[0]?.events.onFrame('a');
    expect(h.statuses).toEqual(['connecting', 'open']);
    expect(h.frames).toEqual(['a']);
  });

  it('waits 1 s, then doubling to 30 s, a fresh attempt each time', async () => {
    const h = harness();
    h.feed.start();
    const delays: number[] = [];
    for (let i = 0; i < 8; i++) {
      await h.settle(i);
      h.attempts[i]?.events.onClose(1006);
      delays.push(...h.clock.pending);
      h.clock.advance(h.clock.pending[0] ?? 0);
    }
    expect(delays).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]);
    expect(delays.every((d) => d <= maxDelay)).toBe(true);
    expect(h.attempts).toHaveLength(9);
  });

  it('starts again from one second after a socket has opened', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.attempts[0]?.events.onClose(1006);
    h.clock.advance(1000);
    await h.settle(1);
    h.attempts[1]?.events.onClose(1006);
    expect(h.clock.pending).toEqual([2000]);
    h.clock.advance(2000);
    await h.settle(2);
    h.attempts[2]?.events.onOpen();
    h.attempts[2]?.events.onClose(1006);
    expect(h.clock.pending).toEqual([1000]);
  });

  it('resumes from the newest id held at the time of each attempt', async () => {
    let held = 5;
    const h = harness(() => held);
    h.feed.start();
    await h.settle(0);
    held = 12;
    h.attempts[0]?.events.onClose(1006);
    h.clock.advance(1000);
    expect(h.attempts.map((a) => a.after)).toEqual([5, 12]);
  });

  it('retries when the ticket cannot be had', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0, { ok: false, failure: 'network' });
    expect(h.statuses).toEqual(['connecting', 'waiting']);
    h.clock.advance(1000);
    expect(h.attempts).toHaveLength(2);
  });

  it('does not retry a 1008 close', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.attempts[0]?.events.onOpen();
    h.attempts[0]?.events.onClose(1008);
    expect(h.statuses.at(-1)).toBe('denied');
    expect(h.clock.pending).toEqual([]);
    h.clock.advance(60_000);
    expect(h.attempts).toHaveLength(1);
  });

  it('retries later, through the backoff, and not at once', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.attempts[0]?.events.onClose(1008);
    h.feed.retryLater();
    expect(h.statuses.at(-1)).toBe('waiting');
    expect(h.attempts).toHaveLength(1);
    h.clock.advance(1000);
    expect(h.attempts).toHaveLength(2);
  });

  it('does not retry later once stopped', () => {
    const h = harness();
    h.feed.start();
    h.feed.stop();
    h.feed.retryLater();
    expect(h.clock.pending).toEqual([]);
  });

  it('can be started again after a 1008, at once', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.attempts[0]?.events.onClose(1008);
    h.feed.start();
    expect(h.attempts).toHaveLength(2);
  });

  it('reports a refused ticket as unauthorized and stops', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0, { ok: false, failure: 'unauthorized' });
    expect(h.statuses.at(-1)).toBe('unauthorized');
    expect(h.clock.pending).toEqual([]);
  });

  it('closes its socket and ignores it afterwards when stopped', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.feed.stop();
    expect(h.attempts[0]?.closed).toBe(true);
    h.attempts[0]?.events.onFrame('late');
    h.attempts[0]?.events.onClose(1006);
    expect(h.frames).toEqual([]);
    expect(h.clock.pending).toEqual([]);
  });

  it('closes a socket that arrives after it was stopped', async () => {
    const h = harness();
    h.feed.start();
    h.feed.stop();
    await h.settle(0);
    expect(h.attempts[0]?.closed).toBe(true);
  });

  it('cancels a pending wait when stopped', async () => {
    const h = harness();
    h.feed.start();
    await h.settle(0);
    h.attempts[0]?.events.onClose(1006);
    h.feed.stop();
    h.clock.advance(60_000);
    expect(h.attempts).toHaveLength(1);
  });
});
