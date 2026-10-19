// The occupancy hook (issue #596): one socket for the signed-in page, whose
// documents become a count per channel, never opened for someone without
// voice, honest when the server cannot be reached, and joining nothing. The
// socket and the read are fakes the test drives, on a fake clock. No SDK, no
// network.
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ApiResult } from '../api';
import type { VoiceOccupancyV1 } from '../schema/voice';
import type { FeedSocket, SocketEvents } from '../state/feed';

// One occupancy socket the page asked for: the events it was given and
// whether the page has closed it.
interface Opened {
  readonly events: SocketEvents;
  closed: boolean;
}

const fake = vi.hoisted(() => ({
  opened: [] as Opened[],
  reads: 0,
  // While true the ticket gets no answer, as with conchd stopped.
  down: false,
  // While true the ticket is refused with a 401.
  unauthorized: false,
  // What an occupancy read answers; never, unless a test says.
  read: (): Promise<ApiResult<VoiceOccupancyV1>> => new Promise(() => undefined),
}));

vi.mock('../api', () => ({
  getVoiceOccupancy: () => {
    fake.reads++;
    return fake.read();
  },
  openOccupancySocket: (events: SocketEvents): Promise<ApiResult<FeedSocket>> => {
    const o: Opened = { events, closed: fake.down || fake.unauthorized };
    fake.opened.push(o);
    if (fake.unauthorized) return Promise.resolve({ ok: false, failure: 'unauthorized' });
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
}));

import { countsOf, parseOccupancy, settleDelay, useOccupancy } from './useOccupancy';

const doc = (channels: [number, number][], up: Partial<Pick<VoiceOccupancyV1, 'configured' | 'available'>> = {}): VoiceOccupancyV1 => ({
  schema: 'conch.voice_occupancy.v1',
  configured: true,
  available: true,
  ...up,
  channels: channels.map(([channel_id, in_voice]) => ({ channel_id, in_voice })),
});

interface Props {
  readonly signedIn: boolean;
  readonly voiceAllowed: boolean;
  readonly reachable: boolean;
}

function hook(initial: Partial<Props> = {}) {
  const unauthorized = vi.fn();
  const props: Props = { signedIn: true, voiceAllowed: true, reachable: true, ...initial };
  const view = renderHook((p: Props) => useOccupancy({ onUnauthorized: unauthorized, ...p }), { initialProps: props });
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
  if (o === undefined) throw new Error('no occupancy socket was opened');
  return o;
}

// send opens the newest socket if it is not yet and sends it a frame.
function send(frame: unknown): void {
  const o = last();
  act(() => {
    o.events.onOpen();
    o.events.onFrame(typeof frame === 'string' ? frame : JSON.stringify(frame));
  });
}

function drop(code: number): void {
  const o = last();
  act(() => {
    o.events.onClose(code);
  });
}

const live = () => fake.opened.filter((o) => !o.closed);
const entries = (m: ReadonlyMap<number, number>) => [...m.entries()];

beforeEach(() => {
  vi.useFakeTimers();
  fake.opened = [];
  fake.reads = 0;
  fake.down = false;
  fake.unauthorized = false;
  fake.read = () => new Promise(() => undefined);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('a document', () => {
  it('is read as the channels with someone in voice, zeros left out', () => {
    expect(entries(countsOf(doc([[1, 3], [2, 0], [7, 1]])))).toEqual([[1, 3], [7, 1]]);
  });

  it('says nobody anywhere while voice is not set up, or LiveKit is out of reach', () => {
    expect(countsOf(doc([], { configured: false, available: false })).size).toBe(0);
    expect(countsOf(doc([], { available: false })).size).toBe(0);
  });

  it.each([
    ['not text', 7],
    ['not JSON', '{'],
    ['presence', JSON.stringify({ schema: 'conch.voice_presence.v1', channel_id: 1, configured: true, available: true, rooms: [] })],
    ['a count that is not a number', JSON.stringify({ schema: 'conch.voice_occupancy.v1', configured: true, available: true, channels: [{ channel_id: 1, in_voice: 'x' }] })],
  ])('is not one when it is %s', (_what, frame) => {
    expect(parseOccupancy(frame)).toBeNull();
  });
});

describe('the socket', () => {
  it('is one for the page, and its documents are the counts, live', async () => {
    const { result, rerender } = hook();
    await flush();
    expect(fake.opened).toHaveLength(1);
    expect(result.current.size).toBe(0);
    send(doc([[1, 2], [3, 0]]));
    expect(entries(result.current)).toEqual([[1, 2]]);
    send(doc([[1, 2], [3, 1]]));
    expect(entries(result.current)).toEqual([[1, 2], [3, 1]]);
    send(doc([[1, 0], [3, 1]]));
    expect(entries(result.current)).toEqual([[3, 1]]);
    // The page drawing again opens nothing more.
    rerender({ signedIn: true, voiceAllowed: true, reachable: true });
    await flush();
    expect(fake.opened).toHaveLength(1);
    expect(live()).toHaveLength(1);
  });

  it('keeps the same counts when a frame changes nothing', async () => {
    const { result } = hook();
    await flush();
    send(doc([[1, 2]]));
    const before = result.current;
    send(doc([[1, 2], [2, 0]]));
    expect(result.current).toBe(before);
  });

  it('ignores a frame that is not an occupancy document', async () => {
    const { result } = hook();
    await flush();
    send(doc([[1, 2]]));
    send('{"schema":"conch.voice_occupancy.v2"}');
    send('not json');
    expect(entries(result.current)).toEqual([[1, 2]]);
  });

  it('shows nobody while voice is not set up', async () => {
    const { result } = hook();
    await flush();
    send(doc([], { configured: false, available: false }));
    expect(result.current.size).toBe(0);
  });

  it.each([
    ['a person whose access has no voice (or an agent)', { voiceAllowed: false }],
    ['a page signed out', { signedIn: false }],
  ])('is never opened, and nothing is read, for %s', async (_who, props) => {
    const { result } = hook(props);
    await flush();
    await pass(5000);
    expect(fake.opened).toEqual([]);
    expect(fake.reads).toBe(0);
    expect(result.current.size).toBe(0);
  });

  it('is closed, and the counts go, on signing out', async () => {
    const { result, rerender } = hook();
    await flush();
    send(doc([[1, 2]]));
    rerender({ signedIn: false, voiceAllowed: true, reachable: true });
    await flush();
    expect(live()).toEqual([]);
    expect(result.current.size).toBe(0);
  });

  it('shows nobody while the server cannot be reached, and comes back with a fresh document', async () => {
    const { result } = hook();
    await flush();
    send(doc([[1, 2]]));
    fake.down = true;
    drop(1006);
    expect(result.current.size).toBe(0);
    await pass(1000);
    expect(fake.opened.length).toBe(2);
    fake.down = false;
    await pass(2000);
    expect(fake.opened.length).toBe(3);
    send(doc([[1, 1]]));
    expect(entries(result.current)).toEqual([[1, 1]]);
  });

  it('tries again at once when the server is back in reach', async () => {
    const { rerender } = hook();
    await flush();
    send(doc([[1, 2]]));
    rerender({ signedIn: true, voiceAllowed: true, reachable: false });
    drop(1006);
    await flush();
    expect(fake.opened).toHaveLength(1);
    rerender({ signedIn: true, voiceAllowed: true, reachable: true });
    await flush();
    expect(fake.opened).toHaveLength(2);
  });

  it('a ticket refused with 401 signs the page out', async () => {
    fake.unauthorized = true;
    const { unauthorized } = hook();
    await flush();
    expect(unauthorized).toHaveBeenCalledTimes(1);
  });

  it('after a 1008, reads once, a second later: a document is shown and the socket tried again later', async () => {
    fake.read = () => Promise.resolve({ ok: true, value: doc([[4, 1]]) });
    const { result } = hook();
    await flush();
    send(doc([[1, 2]]));
    drop(1008);
    await flush();
    expect(result.current.size).toBe(0);
    await pass(settleDelay - 250);
    expect(fake.reads).toBe(0);
    await pass(250);
    expect(fake.reads).toBe(1);
    expect(entries(result.current)).toEqual([[4, 1]]);
    expect(fake.opened).toHaveLength(1);
    await pass(1000);
    expect(fake.opened).toHaveLength(2);
  });

  it('after a 1008 read refused (voice withdrawn), asks nothing more', async () => {
    fake.read = () => Promise.resolve({ ok: false, failure: 'invalid', code: 'voice_not_allowed' });
    const { result } = hook();
    await flush();
    send(doc([[1, 2]]));
    drop(1008);
    await pass(60_000);
    expect(fake.reads).toBe(1);
    expect(fake.opened).toHaveLength(1);
    expect(result.current.size).toBe(0);
  });

  it('after a 1008 read answered 401, signs the page out', async () => {
    fake.read = () => Promise.resolve({ ok: false, failure: 'unauthorized' });
    const { unauthorized } = hook();
    await flush();
    drop(1008);
    await pass(settleDelay);
    expect(unauthorized).toHaveBeenCalledTimes(1);
  });

  it('after a 1008, reads nothing once the page is signed out meanwhile (by another tab, say)', async () => {
    const { rerender, unauthorized } = hook();
    await flush();
    drop(1008);
    rerender({ signedIn: false, voiceAllowed: true, reachable: true });
    await pass(5000);
    expect(fake.reads).toBe(0);
    expect(unauthorized).not.toHaveBeenCalled();
  });
});

describe('seeing who is in voice', () => {
  it('joins nothing: no device is asked for when a document arrives', async () => {
    const getUserMedia = vi.fn(() => Promise.reject(new Error('device requested')));
    Object.defineProperty(navigator, 'mediaDevices', { value: { getUserMedia }, configurable: true });
    try {
      const { result } = hook();
      await flush();
      send(doc([[1, 3], [2, 1]]));
      await pass(1000);
      expect(result.current.size).toBe(2);
      expect(getUserMedia).not.toHaveBeenCalled();
    } finally {
      Reflect.deleteProperty(navigator, 'mediaDevices');
    }
  });

  it('reaches neither the SDK nor the voice client', () => {
    const source = Object.values(import.meta.glob<string>('./useOccupancy.ts', { eager: true, query: '?raw', import: 'default' }))[0] ?? '';
    expect(source.length).toBeGreaterThan(0);
    expect(source).not.toMatch(/from\s+['"](\.\/sdk|\.\/connection|livekit-client)['"]/);
    expect(source).not.toMatch(/VoiceClient/);
  });
});
