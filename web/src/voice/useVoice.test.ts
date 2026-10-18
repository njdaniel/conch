// The hook: its talk key, and the presence of the channel on screen (issue
// #585), from one socket whether or not the page is in voice, with an honest
// state when it cannot be read. The presence socket and read are fakes the
// test drives, on a fake clock. No SDK, no network.
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ApiResult } from '../api';
import type { VoicePresenceV1 } from '../schema/voice';
import type { FeedSocket, SocketEvents } from '../state/feed';

// One presence socket the page asked for: its channel, the events it was
// given and whether the page has closed it.
interface Opened {
  readonly channel: string;
  readonly events: SocketEvents;
  closed: boolean;
}

const fake = vi.hoisted(() => ({
  opened: [] as Opened[],
  reads: [] as string[],
  // While true the ticket gets no answer, as with conchd stopped.
  down: false,
  // What a presence read answers; never, unless a test says.
  read: (): Promise<ApiResult<VoicePresenceV1>> => new Promise(() => undefined),
}));

vi.mock('../api', () => ({
  createVoiceSession: () => new Promise(() => undefined),
  reportVoiceTransmit: () => Promise.resolve('ok'),
  getVoicePresence: (channel: string) => {
    fake.reads.push(channel);
    return fake.read();
  },
  openVoiceSocket: (channel: string, events: SocketEvents): Promise<ApiResult<FeedSocket>> => {
    const o: Opened = { channel, events, closed: fake.down };
    fake.opened.push(o);
    if (fake.down) return Promise.resolve({ ok: false, failure: 'network' });
    return Promise.resolve({
      ok: true,
      value: {
        close: () => {
          o.closed = true;
        },
      },
    });
  },
  claimVoiceShare: () => new Promise(() => undefined),
  stopVoiceShare: () => new Promise(() => undefined),
}));
vi.mock('./sdk', () => ({ newLiveKitRoom: () => ({}) }));

import { defaultTalkKey } from './keys';
import { useVoice } from './useVoice';

const presence = (people: number[]): VoicePresenceV1 => ({
  schema: 'conch.voice_presence.v1',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [{ participants: people.map((principal_id) => ({ principal_id, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' })) }],
});

interface Props {
  readonly channel: string | null;
  readonly voiceAllowed: boolean;
  readonly reachable: boolean;
}

function hook(initial: Partial<Props> = {}) {
  const unauthorized = vi.fn();
  const props: Props = { channel: 'c', voiceAllowed: true, reachable: true, ...initial };
  const view = renderHook((p: Props) => useVoice({ signedIn: true, onUnauthorized: unauthorized, ...p }), { initialProps: props });
  return { ...view, unauthorized };
}

// Lets the ticket and read promises settle.
async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

// pass moves the clock on a little at a time, letting what each timer starts
// settle before the next is due.
async function pass(ms: number): Promise<void> {
  for (let t = 0; t < ms; t += 250) {
    await act(async () => {
      vi.advanceTimersByTime(Math.min(250, ms - t));
      for (let i = 0; i < 10; i++) await Promise.resolve();
    });
  }
}

function last(): Opened {
  const o = fake.opened[fake.opened.length - 1];
  if (o === undefined) throw new Error('no presence socket was opened');
  return o;
}

// open opens the newest socket and sends it a snapshot.
function open(people: number[]): void {
  const o = last();
  act(() => {
    o.events.onOpen();
    o.events.onFrame(JSON.stringify(presence(people)));
  });
}

function drop(code: number): void {
  const o = last();
  act(() => {
    o.events.onClose(code);
  });
}

const live = () => fake.opened.filter((o) => !o.closed);

beforeEach(() => {
  vi.useFakeTimers();
  fake.opened = [];
  fake.reads = [];
  fake.down = false;
  fake.read = () => new Promise(() => undefined);
});
afterEach(() => {
  vi.useRealTimers();
});

describe('the talk key', () => {
  it('is let go when it changes while held', () => {
    const { result } = hook({ channel: null });
    const client = result.current.client;
    const press = vi.spyOn(client, 'press');
    const release = vi.spyOn(client, 'release');
    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { code: defaultTalkKey }));
    });
    expect(press).toHaveBeenCalledTimes(1);
    expect(release).not.toHaveBeenCalled();
    act(() => {
      result.current.setTalkKey('KeyV');
    });
    expect(release).toHaveBeenCalled();
    // The old key's keyup is no longer the talk key's; the new key works.
    release.mockClear();
    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { code: 'KeyV' }));
      window.dispatchEvent(new KeyboardEvent('keyup', { code: 'KeyV' }));
    });
    expect(press).toHaveBeenCalledTimes(2);
    expect(release).toHaveBeenCalledTimes(1);
  });
});

describe('presence of the channel on screen', () => {
  it('comes from its socket while not in voice, and each frame is the roster', async () => {
    const { result } = hook();
    await flush();
    expect(fake.opened.map((o) => o.channel)).toEqual(['c']);
    expect(result.current.state.phase).toBe('idle');
    // Nothing yet before the first frame.
    expect(result.current.snapshot.kind).toBe('loading');
    open([3]);
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([3]) });
    // Someone arrives, then leaves: each change is a frame.
    act(() => {
      last().events.onFrame(JSON.stringify(presence([3, 4])));
    });
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([3, 4]) });
    act(() => {
      last().events.onFrame(JSON.stringify(presence([4])));
    });
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([4]) });
    // No read is needed for any of it.
    expect(fake.reads).toEqual([]);
  });

  it('is unreachable when the socket is lost, with no roster kept, and live again when it reopens', async () => {
    const { result } = hook();
    await flush();
    open([3]);
    drop(1006);
    expect(result.current.snapshot).toEqual({ kind: 'unreachable' });
    // The next attempt, after the backoff's first second, is still out of reach.
    await pass(1000);
    expect(fake.opened).toHaveLength(2);
    expect(result.current.snapshot).toEqual({ kind: 'unreachable' });
    open([3, 4]);
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([3, 4]) });
  });

  it('is unreachable when the first attempt fails, as when the server was down from the start', async () => {
    const { result } = hook();
    await flush();
    drop(1006);
    expect(result.current.snapshot).toEqual({ kind: 'unreachable' });
  });

  it('tries again at once when the server is reachable again, not after the backoff', async () => {
    const { result, rerender } = hook();
    await flush();
    open([3]);
    rerender({ channel: 'c', voiceAllowed: true, reachable: false });
    // Lost, and lost again, so the backoff is at 2 s and growing.
    drop(1006);
    await pass(1000);
    drop(1006);
    expect(fake.opened).toHaveLength(2);
    expect(result.current.snapshot.kind).toBe('unreachable');
    rerender({ channel: 'c', voiceAllowed: true, reachable: true });
    await flush();
    // No time has passed: the third attempt is the restart's.
    expect(fake.opened).toHaveLength(3);
    open([3]);
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([3]) });
  });

  it('does not restart a socket that is open when the server is reachable again', async () => {
    const { rerender } = hook({ reachable: false });
    await flush();
    open([3]);
    rerender({ channel: 'c', voiceAllowed: true, reachable: true });
    await flush();
    expect(fake.opened).toHaveLength(1);
    expect(live()).toHaveLength(1);
  });

  it('after leaving voice while the server is down, is live again once it is back (F06)', async () => {
    const { result, rerender } = hook();
    await flush();
    open([7]);
    act(() => {
      result.current.join();
    });
    expect(result.current.state.phase).toBe('connecting');
    // conchd stops: the status line says Reconnecting, the socket is lost.
    rerender({ channel: 'c', voiceAllowed: true, reachable: false });
    fake.down = true;
    drop(1006);
    await pass(8000);
    // In voice, the strip keeps the roster it had, as before: the room may
    // still be connected, and a share watched stays.
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([7]) });
    act(() => {
      result.current.leave();
    });
    await flush();
    expect(result.current.state.phase).toBe('idle');
    expect(result.current.snapshot).toEqual({ kind: 'unreachable' });
    // Attempts at 1 s, then 3 s and 7 s after the loss, each refused.
    const before = fake.opened.length;
    expect(before).toBe(4);
    // conchd is back.
    fake.down = false;
    rerender({ channel: 'c', voiceAllowed: true, reachable: true });
    await flush();
    expect(fake.opened.length).toBe(before + 1);
    open([]);
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([]) });
  });

  it('is not allowed after a 1008 close when the read says voice is withdrawn, and nothing is tried again', async () => {
    fake.read = () => Promise.resolve({ ok: false, failure: 'invalid', code: 'voice_not_allowed' });
    const { result, rerender, unauthorized } = hook();
    await flush();
    open([3]);
    drop(1008);
    await flush();
    expect(fake.reads).toEqual(['c']);
    expect(result.current.snapshot).toEqual({ kind: 'not_allowed' });
    // No loop: no more sockets or reads, through the longest backoff and a
    // return to reach.
    rerender({ channel: 'c', voiceAllowed: true, reachable: false });
    rerender({ channel: 'c', voiceAllowed: true, reachable: true });
    await pass(120_000);
    expect(fake.opened).toHaveLength(1);
    expect(fake.reads).toEqual(['c']);
    expect(unauthorized).not.toHaveBeenCalled();
  });

  it('is gone after a 1008 close for any other reason', async () => {
    fake.read = () => Promise.resolve({ ok: false, failure: 'invalid', code: 'channel_not_found' });
    const { result } = hook();
    await flush();
    open([3]);
    drop(1008);
    await flush();
    expect(result.current.snapshot).toEqual({ kind: 'gone' });
    await pass(120_000);
    expect(fake.opened).toHaveLength(1);
    expect(fake.reads).toEqual(['c']);
  });

  it('signs out when the read after a 1008 close is refused with 401', async () => {
    fake.read = () => Promise.resolve({ ok: false, failure: 'unauthorized' });
    const { unauthorized } = hook();
    await flush();
    open([3]);
    drop(1008);
    await flush();
    expect(unauthorized).toHaveBeenCalledTimes(1);
  });

  it('shows what the read says after a 1008 close it answers, and tries the socket again through the backoff', async () => {
    fake.read = () => Promise.resolve({ ok: true, value: presence([5]) });
    const { result } = hook();
    await flush();
    open([3]);
    drop(1008);
    await flush();
    expect(fake.opened).toHaveLength(1);
    await pass(1000);
    expect(fake.opened).toHaveLength(2);
    open([5]);
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([5]) });
  });

  it('opens nothing and reads nothing for a person without voice', async () => {
    const { result, rerender } = hook({ voiceAllowed: false });
    await flush();
    expect(result.current.snapshot).toEqual({ kind: 'not_allowed' });
    // Not on another channel, and not when the server comes back in reach.
    rerender({ channel: 'd', voiceAllowed: false, reachable: false });
    rerender({ channel: 'd', voiceAllowed: false, reachable: true });
    await pass(120_000);
    expect(fake.opened).toEqual([]);
    expect(fake.reads).toEqual([]);
    expect(result.current.snapshot).toEqual({ kind: 'not_allowed' });
  });

  it('keeps one socket through joining and leaving in the channel on screen, and gives the client what it said', async () => {
    const { result } = hook();
    await flush();
    open([3]);
    const client = result.current.client;
    const told = vi.spyOn(client, 'presence');
    act(() => {
      result.current.join();
    });
    await flush();
    expect(result.current.state.phase).toBe('connecting');
    expect(fake.opened).toHaveLength(1);
    expect(live()).toHaveLength(1);
    // On joining, the client has the last snapshot; and every frame after.
    expect(told).toHaveBeenLastCalledWith(presence([3]));
    act(() => {
      last().events.onFrame(JSON.stringify(presence([2, 3])));
    });
    expect(told).toHaveBeenLastCalledWith(presence([2, 3]));
    act(() => {
      result.current.leave();
    });
    await flush();
    expect(result.current.state.phase).toBe('idle');
    expect(fake.opened).toHaveLength(1);
    expect(live()).toHaveLength(1);
    // Not in voice, the client is told nothing.
    told.mockClear();
    act(() => {
      last().events.onFrame(JSON.stringify(presence([3])));
    });
    expect(told).not.toHaveBeenCalled();
    expect(result.current.snapshot).toEqual({ kind: 'presence', presence: presence([3]) });
  });

  it('holds one socket, for the channel on screen: another channel closes it and opens its own', async () => {
    const { rerender } = hook();
    await flush();
    open([]);
    rerender({ channel: 'd', voiceAllowed: true, reachable: true });
    await flush();
    expect(fake.opened.map((o) => [o.channel, o.closed])).toEqual([
      ['c', true],
      ['d', false],
    ]);
  });
});
