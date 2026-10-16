import { describe, expect, it } from 'vitest';
import type { TransmitAnswer, VoiceSessionResult } from '../api';
import { VoiceClient, reconnectLimit, type Room, type RoomEvents } from './connection';
import { FakeClock, settle } from './fakes';

const token = 'join-token-secret-value';
const roomName = 'conch-room-secret-name';

class StubRoom implements Room {
  events: RoomEvents | null = null;
  sending: boolean[] = [];
  deafened: boolean[] = [];
  closed = false;
  connectError = false;
  connectArgs: { url: string; token: string; roomName: string; publish: boolean } | null = null;
  levelNow = 0;
  connect = (url: string, tok: string, name: string, publish: boolean, events: RoomEvents): Promise<void> => {
    this.connectArgs = { url, token: tok, roomName: name, publish };
    this.events = events;
    return this.connectError ? Promise.reject(new Error('unreachable')) : Promise.resolve();
  };
  setSending = (on: boolean): Promise<void> => {
    this.sending.push(on);
    return Promise.resolve();
  };
  setDeafened = (on: boolean): void => {
    this.deafened.push(on);
  };
  level = (): number => this.levelNow;
  close = (): Promise<void> => {
    this.closed = true;
    return Promise.resolve();
  };
}

const okSession = (canPublish = true): VoiceSessionResult => ({
  ok: true,
  value: {
    livekit_url: 'wss://lk.example',
    identity: 'p7',
    rooms: [{ room: roomName, token, can_publish: canPublish, expires_at: '2026-10-16T00:00:15.000Z' }],
  },
});

function setup(sessions: VoiceSessionResult[] = [okSession()], reportAnswers: TransmitAnswer[] = [], failFirst = 0) {
  const clock = new FakeClock();
  const rooms: StubRoom[] = [];
  const asked: string[] = [];
  const reports: string[] = [];
  const client = new VoiceClient(
    {
      clock,
      random: () => 0.999999,
      createSession: (channel) => {
        asked.push(channel);
        return Promise.resolve(sessions.shift() ?? okSession());
      },
      report: (_channel, state) => {
        reports.push(state);
        return Promise.resolve(reportAnswers.shift() ?? 'ok');
      },
      newRoom: () => {
        const room = new StubRoom();
        room.connectError = rooms.length < failFirst;
        rooms.push(room);
        return room;
      },
    },
    () => clock.now,
  );
  return { client, clock, rooms, asked, reports };
}

async function joined(t = setup()) {
  t.client.join('general');
  await settle();
  return t;
}

describe('joining', () => {
  it('asks conchd for a session, connects with the grant that has no audience, and is connected only then', async () => {
    const t = setup();
    t.client.join('general');
    expect(t.client.snapshot().phase).toBe('connecting');
    await settle();
    expect(t.asked).toEqual(['general']);
    expect(t.rooms[0]?.connectArgs).toEqual({ url: 'wss://lk.example', token, roomName, publish: true });
    expect(t.client.snapshot().phase).toBe('connected');
  });

  it('never keeps the token or the room name in its state', async () => {
    const t = await joined();
    expect(JSON.stringify({ ...t.client.snapshot(), speaking: [...t.client.snapshot().speaking] })).not.toMatch(/secret/);
  });

  it('a listen-only grant takes no press', async () => {
    const t = await joined(setup([okSession(false)]));
    t.client.press();
    await settle();
    expect(t.client.snapshot().listenOnly).toBe(true);
    expect(t.rooms[0]?.sending).toEqual([]);
    expect(t.reports).toEqual([]);
  });

  it('a press before the connection is up does nothing', () => {
    const t = setup();
    t.client.join('general');
    t.client.press();
    expect(t.reports).toEqual([]);
  });
});

describe('talking', () => {
  it('a press opens the track and reports started; a release closes it after the tail and reports stopped', async () => {
    const t = await joined();
    t.client.press();
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true]);
    expect(t.reports).toEqual(['started']);
    expect(t.client.snapshot().transmitting).toBe(true);
    t.client.release();
    t.clock.advance(100);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true, false]);
    expect(t.reports).toEqual(['started', 'stopped']);
  });

  it('losing focus is a release', async () => {
    const t = await joined();
    t.client.press();
    t.client.lost();
    t.clock.advance(100);
    expect(t.client.snapshot().transmitting).toBe(false);
  });

  it('muting shuts the gate and refuses a press; so does deafening, which also mutes what is heard', async () => {
    const t = await joined();
    t.client.press();
    t.client.setMuted(true);
    expect(t.client.snapshot().transmitting).toBe(false);
    t.client.press();
    expect(t.client.snapshot().transmitting).toBe(false);
    t.client.setMuted(false);
    t.client.setDeafened(true);
    t.client.press();
    expect(t.client.snapshot().transmitting).toBe(false);
    expect(t.rooms[0]?.deafened.at(-1)).toBe(true);
    t.client.setDeafened(false);
    t.client.press();
    expect(t.client.snapshot().transmitting).toBe(true);
  });

  it('the open microphone opens on speech and closes after the hang time', async () => {
    const t = await joined();
    t.client.setMode('open');
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.levelNow = 0.5;
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(true);
    room.levelNow = 0;
    t.clock.advance(50);
    t.clock.advance(400);
    expect(t.client.snapshot().transmitting).toBe(false);
    await settle();
    expect(t.reports).toEqual(['started', 'stopped']);
  });

  it('shows who is speaking', async () => {
    const t = await joined();
    t.rooms[0]?.events?.onSpeakers([3, 9]);
    expect([...t.client.snapshot().speaking]).toEqual([3, 9]);
  });

  it('audio does not wait for a report that never answers', async () => {
    const t = setup();
    t.client.join('general');
    await settle();
    t.client.press();
    expect(t.rooms[0]?.sending).toEqual([true]);
  });
});

describe('disconnects', () => {
  it('a deleted room or a removed participant asks conchd again at once', async () => {
    for (const cause of ['room_deleted', 'participant_removed'] as const) {
      const t = await joined();
      t.rooms[0]?.events?.onDisconnected(cause);
      await settle();
      expect(t.asked).toEqual(['general', 'general']);
      expect(t.rooms[0]?.closed).toBe(true);
      expect(t.client.snapshot().phase).toBe('connected');
    }
  });

  it('a duplicate identity stops, saying another device took over', async () => {
    const t = await joined();
    t.rooms[0]?.events?.onDisconnected('duplicate_identity');
    await settle();
    expect(t.client.snapshot().phase).toBe('stopped');
    expect(t.client.snapshot().why).toMatch(/Another device/);
    t.clock.advance(60_000);
    await settle();
    expect(t.asked).toHaveLength(1);
  });

  it('a refused session stops with the reason', async () => {
    const t = setup([{ ok: false, failure: 'invalid', status: 404 }]);
    t.client.join('general');
    await settle();
    expect(t.client.snapshot().phase).toBe('stopped');
    expect(t.client.snapshot().why).toMatch(/not a member/);
  });

  it('a lost connection retries with backoff, asking conchd each time', async () => {
    const t = await joined();
    t.rooms[0]?.events?.onDisconnected('lost');
    expect(t.client.snapshot().phase).toBe('unavailable');
    t.clock.advance(999);
    await settle();
    expect(t.asked).toHaveLength(1);
    t.clock.advance(1);
    await settle();
    expect(t.asked).toHaveLength(2);
    expect(t.client.snapshot().phase).toBe('connected');
  });

  it('an unreachable LiveKit is unavailable, and retried with a fresh session', async () => {
    const t = setup([okSession(), okSession()], [], 1);
    t.client.join('general');
    await settle();
    expect(t.client.snapshot().phase).toBe('unavailable');
    expect(t.rooms[0]?.closed).toBe(true);
    t.clock.advance(1000);
    await settle();
    expect(t.asked).toHaveLength(2);
    expect(t.client.snapshot().phase).toBe('connected');
  });

  it('the SDK may reconnect for 20 s, then the client goes back to conchd', async () => {
    const t = await joined();
    t.rooms[0]?.events?.onReconnecting();
    expect(t.client.snapshot().phase).toBe('reconnecting');
    t.client.press();
    expect(t.client.snapshot().transmitting).toBe(false);
    t.clock.advance(reconnectLimit - 1);
    await settle();
    expect(t.asked).toHaveLength(1);
    t.clock.advance(1);
    await settle();
    expect(t.asked).toHaveLength(2);
    expect(t.rooms[0]?.closed).toBe(true);
  });

  it('a reconnect inside the limit keeps the connection', async () => {
    const t = await joined();
    t.rooms[0]?.events?.onReconnecting();
    t.clock.advance(5000);
    t.rooms[0]?.events?.onReconnected();
    t.clock.advance(60_000);
    await settle();
    expect(t.asked).toHaveLength(1);
    expect(t.client.snapshot().phase).toBe('connected');
  });

  it('a report answered voice_no_session goes back to conchd', async () => {
    const t = setup([okSession(), okSession()], ['no_session']);
    await joined(t);
    t.client.press();
    await settle();
    await settle();
    expect(t.asked).toHaveLength(2);
  });

  it('a report given up on drops the connection', async () => {
    const t = setup([okSession(), okSession()], ['failed', 'failed', 'failed']);
    await joined(t);
    t.client.press();
    await settle();
    t.clock.advance(250);
    await settle();
    t.clock.advance(750);
    await settle();
    expect(t.asked).toHaveLength(2);
    expect(t.rooms[0]?.closed).toBe(true);
  });
});

describe('leaving', () => {
  it('shuts the gate, sends the last report, and disconnects', async () => {
    const t = await joined();
    t.client.press();
    await settle();
    await t.client.leave();
    expect(t.reports).toEqual(['started', 'stopped']);
    expect(t.rooms[0]?.closed).toBe(true);
    expect(t.client.snapshot().phase).toBe('idle');
  });
});
