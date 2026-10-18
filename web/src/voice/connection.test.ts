import { describe, expect, it } from 'vitest';
import type { ShareClaimResult, TransmitAnswer, VoiceSessionResult } from '../api';
import type { VoicePresenceV1 } from '../schema/voice';
import { VoiceClient, reconnectLimit, reportWait, type Phase, type Room, type RoomEvents, type ShareSink } from './connection';
import { resumeLimit, type Captured, type KeptScreen } from './share';
import { FakeClock, settle } from './fakes';

const token = 'join-token-secret-value';
const roomName = 'conch-room-secret-name';

class StubRoom implements Room {
  events: RoomEvents | null = null;
  sending: boolean[] = [];
  deafened: boolean[] = [];
  closed = false;
  connectError = false;
  // When set, connect or publishMicrophone rejects with this, as a browser
  // refusing the microphone does (issue #586).
  connectFailure: Error | null = null;
  microphoneFailure: Error | null = null;
  connectArgs: { url: string; token: string; roomName: string; publish: boolean } | null = null;
  levelNow = 0;
  // What happened, in order, across the room and the reports (issue #495).
  log: string[] | null = null;
  // When set, a mute waits for the test to let it finish.
  holdMute = false;
  heldMute: (() => void) | null = null;
  connect = (url: string, tok: string, name: string, publish: boolean, events: RoomEvents): Promise<void> => {
    this.connectArgs = { url, token: tok, roomName: name, publish };
    this.events = events;
    if (this.connectFailure !== null) return Promise.reject(this.connectFailure);
    return this.connectError ? Promise.reject(new Error('unreachable')) : Promise.resolve();
  };
  setSending = (on: boolean): Promise<void> => {
    this.sending.push(on);
    this.log?.push(on ? 'unmute' : 'mute');
    if (!on && this.holdMute) {
      return new Promise((resolve) => {
        this.heldMute = () => {
          this.log?.push('muted');
          resolve();
        };
      });
    }
    return Promise.resolve();
  };
  setDeafened = (on: boolean): void => {
    this.deafened.push(on);
  };
  level = (): number => this.levelNow;
  // When set, closing the room reports it disconnected, as the SDK does for
  // a room that had joined.
  disconnectOnClose = false;
  close = (): Promise<void> => {
    this.closed = true;
    if (this.disconnectOnClose) this.events?.onDisconnected('lost');
    return Promise.resolve();
  };
  // The share and watch half of a room (issue #444), recorded.
  calls: string[] = [];
  watching: boolean[] = [];
  sinks: (ShareSink | null)[] = [];
  pickAudio = true;
  pickFails = false;
  publishMicrophone = (): Promise<void> => {
    this.calls.push('microphone');
    return this.microphoneFailure === null ? Promise.resolve() : Promise.reject(this.microphoneFailure);
  };
  setWatching = (on: boolean): void => {
    this.watching.push(on);
  };
  attachShare = (sink: ShareSink | null): void => {
    this.sinks.push(sink);
  };
  pickScreen = (): Promise<Captured> => {
    this.calls.push('pick');
    return this.pickFails ? Promise.reject(new Error('picker closed')) : Promise.resolve({ audio: this.pickAudio });
  };
  publishScreen = (): Promise<void> => {
    this.calls.push('publish');
    return Promise.resolve();
  };
  stopScreen = (): Promise<void> => {
    this.calls.push('stop');
    return Promise.resolve();
  };
  // Issue #516: a capture handed from one connection to the next.
  keptLive = true;
  keepScreen = (): KeptScreen | null => {
    this.calls.push('keep');
    const calls = this.calls;
    return {
      audio: this.pickAudio,
      live: () => this.keptLive,
      onEnded: null,
      stop: () => {
        calls.push('stop kept');
      },
    };
  };
  adoptScreen = (): void => {
    this.calls.push('adopt');
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

const claim: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:00.000Z', audio: true } };

// held: each report waits for the test to answer it (answer below), as a
// slow conchd would.
// prepare sets up each room as it is made, before it is connected.
function setup(sessions: VoiceSessionResult[] = [okSession()], reportAnswers: TransmitAnswer[] = [], failFirst = 0, held = false, prepare?: (room: StubRoom, index: number) => void) {
  const clock = new FakeClock();
  const rooms: StubRoom[] = [];
  const asked: string[] = [];
  const reports: string[] = [];
  const log: string[] = [];
  const waiting: ((a: TransmitAnswer) => void)[] = [];
  // answer answers the oldest report still waiting.
  const answer = async (a: TransmitAnswer = 'ok') => {
    const next = waiting.shift();
    if (next === undefined) throw new Error('no report is waiting');
    log.push(`answered ${a}`);
    next(a);
    await settle();
  };
  // What the share asked conchd, in order.
  const shares: string[] = [];
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
        log.push(`report ${state}`);
        if (held) {
          return new Promise((resolve) => {
            waiting.push(resolve);
          });
        }
        return Promise.resolve(reportAnswers.shift() ?? 'ok');
      },
      newRoom: () => {
        const room = new StubRoom();
        room.log = log;
        room.connectError = rooms.length < failFirst;
        prepare?.(room, rooms.length);
        rooms.push(room);
        return room;
      },
      claimShare: (channel) => {
        shares.push(`claim ${channel}`);
        return Promise.resolve(claim);
      },
      stopShare: (channel) => {
        shares.push(`stop ${channel}`);
        return Promise.resolve();
      },
    },
    () => clock.now,
  );
  return { client, clock, rooms, asked, reports, shares, log, answer };
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

  it('audio does not depend on a report that never answers: the track opens after reportWait', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.press();
    await settle();
    t.clock.advance(reportWait - 1);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([]);
    t.clock.advance(1);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true]);
  });
});

// Issue #495: conchd's poller compares what it sees of the microphone with the
// reports, so a press is reported before the track opens, and a release mutes
// the track before it is reported.
describe('the order of the track and the reports', () => {
  it('a press opens the track only once conchd has answered its report, however slow', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(true);
    t.clock.advance(reportWait - 100);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([]);
    await t.answer('ok');
    expect(t.rooms[0]?.sending).toEqual([true]);
    expect(t.log).toEqual(['report started', 'answered ok', 'unmute']);
  });

  it('a release mutes the track, and reports stopped only once it is muted', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    t.client.press();
    await settle();
    await t.answer('ok');
    room.holdMute = true;
    t.client.release();
    t.clock.advance(100);
    await settle();
    expect(t.reports).toEqual(['started']);
    room.heldMute?.();
    await settle();
    expect(t.reports).toEqual(['started', 'stopped']);
    await t.answer('ok');
    expect(t.log).toEqual(['report started', 'answered ok', 'unmute', 'mute', 'muted', 'report stopped', 'answered ok']);
  });

  it('a press released while its report waits never opens the track, and both reports go in order', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.press();
    await settle();
    t.client.release();
    t.clock.advance(100);
    t.client.press();
    t.clock.advance(50);
    t.client.release();
    t.clock.advance(100);
    await settle();
    await t.answer('ok');
    await t.answer('ok');
    await t.answer('ok');
    await t.answer('ok');
    expect(t.rooms[0]?.sending).toEqual([false, false]);
    expect(t.reports).toEqual(['started', 'stopped', 'started', 'stopped']);
  });

  it('a rate-limited report still opens the track: the limit is shown, and audio does not depend on it', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.press();
    await settle();
    await t.answer('rate_limited');
    expect(t.rooms[0]?.sending).toEqual([true]);
    expect(t.client.snapshot().reportNote).toBe('rate_limited');
  });

  it('a report answered voice_no_session drops the connection before the track would open', async () => {
    const t = await joined(setup([okSession(), okSession()], [], 0, true));
    t.client.press();
    await settle();
    await t.answer('no_session');
    await settle();
    expect(t.asked).toHaveLength(2);
    expect(t.rooms[0]?.closed).toBe(true);
    expect(t.rooms[0]?.sending).toEqual([false]);
    expect(t.rooms[1]?.sending).toEqual([]);
  });

  it('a Leave while a press waits mutes, sends both reports in order, and does not come back', async () => {
    const t = await joined(setup([okSession(), okSession()], [], 0, true));
    t.client.press();
    await settle();
    const leaving = t.client.leave();
    await settle();
    await t.answer('ok');
    await t.answer('ok');
    await leaving;
    expect(t.rooms[0]?.sending).toEqual([false]);
    expect(t.reports).toEqual(['started', 'stopped']);
    expect(t.rooms[0]?.closed).toBe(true);
    expect(t.client.snapshot().phase).toBe('idle');
    t.clock.advance(60_000);
    await settle();
    expect(t.asked).toHaveLength(1);
  });

  it('a drop while a press waits abandons it: nothing opens or is reported on the next connection', async () => {
    const t = await joined(setup([okSession(), okSession()], [], 0, true));
    t.client.press();
    await settle();
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.client.snapshot().phase).toBe('connected');
    t.clock.advance(reportWait);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([false]);
    expect(t.rooms[1]?.sending).toEqual([]);
    expect(t.reports).toEqual(['started']);
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

// Issue #586: a microphone the browser refuses, or that is not there, stops
// with the reason by name; nothing tries again until the person does.
const deviceError = (name: string): Error => new DOMException('device detail that is not shown', name);

describe('a refused or missing microphone', () => {
  it.each<[string, Phase, RegExp]>([
    ['NotAllowedError', 'stopped', /Your browser blocked the microphone/],
    ['SecurityError', 'stopped', /Your browser blocked the microphone/],
    ['NotFoundError', 'stopped', /No microphone was found/],
    ['OverconstrainedError', 'stopped', /No microphone was found/],
    ['NotReadableError', 'stopped', /in use by another program/],
    ['AbortError', 'unavailable', /trying again/],
    ['Error', 'unavailable', /trying again/],
  ])('%s on Join voice -> %s', async (name, phase, why) => {
    const t = setup([okSession(), okSession()], [], 0, false, (room, i) => {
      if (i === 0) room.connectFailure = deviceError(name);
    });
    t.client.join('general');
    await settle();
    expect(t.client.snapshot().phase).toBe(phase);
    expect(t.client.snapshot().why).toMatch(why);
    expect(t.rooms[0]?.closed).toBe(true);
    if (phase === 'stopped') {
      // No retry timer is set: nothing asks conchd again by itself.
      expect(t.clock.pending).toBe(0);
      t.clock.advance(60_000);
      await settle();
      expect(t.asked).toHaveLength(1);
    } else {
      expect(t.clock.pending).toBe(1);
      t.clock.advance(1000);
      await settle();
      expect(t.asked).toHaveLength(2);
      expect(t.client.snapshot().phase).toBe('connected');
    }
  });

  // The SDK joins the room before it asks for the microphone, so the room
  // closed after the refusal says it disconnected: that is not a loss to
  // retry.
  it('a room that joined before the microphone was refused stays stopped when it is closed', async () => {
    const t = setup([okSession(), okSession()], [], 0, false, (room) => {
      room.connectFailure = deviceError('NotAllowedError');
      room.disconnectOnClose = true;
    });
    t.client.join('general');
    await settle();
    expect(t.rooms[0]?.closed).toBe(true);
    expect(t.client.snapshot().phase).toBe('stopped');
    expect(t.clock.pending).toBe(0);
    t.clock.advance(60_000);
    await settle();
    expect(t.asked).toHaveLength(1);
  });

  it('the reason names nothing of the device, the room or the token', async () => {
    for (const name of ['NotAllowedError', 'NotFoundError', 'NotReadableError']) {
      const t = setup([okSession()], [], 0, false, (room) => {
        room.connectFailure = deviceError(name);
      });
      t.client.join('general');
      await settle();
      expect(t.client.snapshot().why).not.toMatch(/device detail|secret|general/);
    }
  });

  it('after a blocked microphone, Watch only connects without it', async () => {
    const t = setup([okSession(), okSession()], [], 0, false, (room, i) => {
      if (i === 0) room.connectFailure = deviceError('NotAllowedError');
    });
    t.client.join('general');
    await settle();
    expect(t.client.snapshot().phase).toBe('stopped');
    t.client.join('general', { watchOnly: true });
    await settle();
    expect(t.rooms[1]?.connectArgs?.publish).toBe(false);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', watchOnly: true, why: null });
  });

  it('a refused Use microphone keeps the connection and watch only, and says why by name', async () => {
    const t = setup([okSession()], [], 0, false, (room) => {
      room.microphoneFailure = deviceError('NotAllowedError');
    });
    t.client.join('general', { watchOnly: true });
    await settle();
    await t.client.useMicrophone();
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', watchOnly: true });
    expect(t.client.snapshot().why).toMatch(/Your browser blocked the microphone/);
    // Already watching only: not told to watch only.
    expect(t.client.snapshot().why).not.toMatch(/Watch only/);
    expect(t.rooms[0]?.closed).toBe(false);
    t.client.press();
    await settle();
    expect(t.reports).toEqual([]);
  });

  it('a Use microphone that fails for another reason says so plainly', async () => {
    const t = setup([okSession()], [], 0, false, (room) => {
      room.microphoneFailure = new Error('something else');
    });
    t.client.join('general', { watchOnly: true });
    await settle();
    await t.client.useMicrophone();
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', watchOnly: true, why: 'The microphone could not be used' });
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

// Issue #444: the share, watching and watch only, through the client.
const presenceWith = (sharing: number | null): VoicePresenceV1 => ({
  schema: 'conch.voice_presence.v1',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [
    {
      participants: [7, 8].map((id) => ({
        principal_id: id,
        can_publish: true,
        transmitting: false,
        joined_at: '2026-10-16T00:00:00.000Z',
        ...(id === sharing ? { sharing: true } : {}),
      })),
    },
  ],
});

describe('watch only', () => {
  it('connects without publishing the microphone, takes no press, and publishes it when asked', async () => {
    const t = setup();
    t.client.join('general', { watchOnly: true });
    await settle();
    expect(t.rooms[0]?.connectArgs?.publish).toBe(false);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', watchOnly: true });
    t.client.press();
    await settle();
    expect(t.reports).toEqual([]);
    expect(t.rooms[0]?.calls).toEqual([]);
    await t.client.useMicrophone();
    expect(t.rooms[0]?.calls).toEqual(['microphone']);
    expect(t.client.snapshot().watchOnly).toBe(false);
    t.client.press();
    await settle();
    expect(t.reports).toEqual(['started']);
  });

  it('stays watch only across a reconnect through conchd', async () => {
    const t = setup([okSession(), okSession()]);
    t.client.join('general', { watchOnly: true });
    await settle();
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.rooms[1]?.connectArgs?.publish).toBe(false);
  });
});

describe('sharing through the client', () => {
  it('claims, picks, claims again and publishes on the connected room; leaving stops it and tells conchd', async () => {
    const t = await joined();
    t.client.startShare();
    await settle();
    expect(t.shares).toEqual(['claim general', 'claim general']);
    expect(t.rooms[0]?.calls).toEqual(['pick', 'publish']);
    expect(t.client.snapshot().share).toEqual({ kind: 'sharing', audio: true });
    await t.client.leave();
    expect(t.shares).toEqual(['claim general', 'claim general', 'stop general']);
    expect(t.rooms[0]?.calls).toEqual(['pick', 'publish', 'stop']);
    expect(t.client.snapshot().share).toEqual({ kind: 'idle', ended: 'left' });
  });

  it('a share does not outlive its connection: a reconnect through conchd ends it and tells conchd', async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    t.client.startShare();
    await settle();
    // Removed from the room, which goes on: not a renewal (issue #516).
    t.rooms[0]?.events?.onDisconnected('participant_removed');
    await settle();
    expect(t.shares).toEqual(['claim general', 'claim general', 'stop general']);
    expect(t.client.snapshot().share).toEqual({ kind: 'idle', ended: 'lost' });
  });

  it('an SDK reconnect claims the share again', async () => {
    const t = await joined();
    t.client.startShare();
    await settle();
    t.rooms[0]?.events?.onReconnecting();
    t.rooms[0]?.events?.onReconnected();
    await settle();
    expect(t.shares).toEqual(['claim general', 'claim general', 'claim general']);
    expect(t.client.snapshot().share.kind).toBe('sharing');
  });

  it('presence that no longer shows this person sharing is the server ending it', async () => {
    const t = await joined();
    t.client.startShare();
    await settle();
    t.client.presence(presenceWith(7));
    t.client.presence(presenceWith(null));
    await settle();
    expect(t.client.snapshot().share).toEqual({ kind: 'idle', ended: 'server' });
    // Not asked to stop: by now the room's share may be someone else's.
    expect(t.shares).toEqual(['claim general', 'claim general']);
  });

  it('the room ending the published share is the browser or the server, by cause', async () => {
    const t = await joined();
    t.client.startShare();
    await settle();
    t.rooms[0]?.events?.onScreenEnded('browser');
    await settle();
    expect(t.client.snapshot().share).toEqual({ kind: 'idle', ended: 'browser' });
    expect(t.shares).toEqual(['claim general', 'claim general', 'stop general']);
  });

  it("does not ask conchd to stop a share presence shows as someone else's", async () => {
    const t = await joined();
    t.client.startShare();
    await settle();
    t.client.presence(presenceWith(7));
    // The server ended this share and principal 8 claimed the room's.
    t.client.presence(presenceWith(8));
    await settle();
    await t.client.leave();
    expect(t.shares).toEqual(['claim general', 'claim general']);
  });

  it('is not offered before the connection is up', () => {
    const t = setup();
    t.client.join('general');
    t.client.startShare();
    expect(t.shares).toEqual([]);
  });
});

// Issue #516: the room renewed under a share.
const renewedPresence = (endedFor7 = false): VoicePresenceV1 => ({
  ...presenceWith(null),
  // Everyone left the old room; nobody is in the new one yet.
  rooms: [{ participants: [] }],
  ...(endedFor7 ? { share_ended: { reason: 'rotated' as const, started_at: '2026-10-16T00:00:00.000Z', ended_at: '2026-10-16T00:01:00.000Z' } } : {}),
});

describe('a share across a renewed room', () => {
  it('is resumed on the new connection: claimed again and the same capture published, with no picker', async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    t.client.startShare();
    await settle();
    t.client.presence(presenceWith(7));
    t.client.presence(renewedPresence(true));
    expect(t.client.snapshot().share).toEqual({ kind: 'resuming' });
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.rooms[0]?.calls).toEqual(['pick', 'publish', 'keep']);
    expect(t.rooms[0]?.closed).toBe(true);
    expect(t.rooms[1]?.calls).toEqual(['adopt', 'publish']);
    expect(t.shares).toEqual(['claim general', 'claim general', 'claim general']);
    expect(t.client.snapshot().share).toEqual({ kind: 'sharing', audio: true, resumed: true });
  });

  it('a sharer who was the one removed gets no session: nothing is claimed again and the capture stops', async () => {
    const t = setup([okSession(), { ok: false, failure: 'unauthorized' }]);
    await joined(t);
    t.client.startShare();
    await settle();
    t.client.presence(presenceWith(7));
    t.client.presence(renewedPresence(true));
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.client.snapshot().phase).toBe('stopped');
    expect(t.rooms).toHaveLength(1);
    expect(t.rooms[0]?.calls).toEqual(['pick', 'publish', 'keep', 'stop kept']);
    expect(t.shares).toEqual(['claim general', 'claim general']);
    expect(t.client.snapshot().share).toEqual({ kind: 'idle', ended: 'rotated' });
  });

  it("a page watching it keeps watching while the same person's share comes back", async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    t.client.presence(presenceWith(8));
    t.client.watch(true);
    t.client.presence(renewedPresence());
    expect(t.client.snapshot().watching).toBe(true);
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    // In the new room, before the share is back.
    t.client.presence(presenceWith(null));
    expect(t.client.snapshot().watching).toBe(true);
    expect(t.rooms[1]?.watching).toEqual([true]);
    t.client.presence(presenceWith(8));
    expect(t.client.snapshot().watching).toBe(true);
    // Back for good: a share that ends now has ended.
    t.clock.advance(resumeLimit);
    expect(t.client.snapshot().watching).toBe(true);
    t.client.presence(presenceWith(null));
    expect(t.client.snapshot().watching).toBe(false);
  });

  it("a watching page lets go if the share does not come back in time, or someone else's comes instead", async () => {
    const late = setup([okSession(), okSession()]);
    await joined(late);
    late.client.presence(presenceWith(8));
    late.client.watch(true);
    late.client.presence(renewedPresence());
    late.clock.advance(resumeLimit - 1);
    expect(late.client.snapshot().watching).toBe(true);
    late.clock.advance(1);
    expect(late.client.snapshot().watching).toBe(false);

    const other = setup([okSession(), okSession()]);
    await joined(other);
    other.client.presence(presenceWith(8));
    other.client.watch(true);
    // The room deleted before presence said anything.
    other.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    other.client.presence({
      ...presenceWith(null),
      rooms: [{ participants: [{ principal_id: 9, can_publish: true, transmitting: false, joined_at: '2026-10-16T00:00:00.000Z', sharing: true }] }],
    });
    expect(other.client.snapshot().watching).toBe(false);
  });
});

describe('watching', () => {
  it('subscribes when asked, keeps the choice and the view on a new connection, and lets go when the share ends', async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    expect(t.rooms[0]?.watching).toEqual([false]);
    const sink = { video: document.createElement('video'), audio: document.createElement('audio') };
    t.client.attachShare(sink);
    t.client.watch(true);
    expect(t.rooms[0]?.watching).toEqual([false, true]);
    expect(t.rooms[0]?.sinks.at(-1)).toBe(sink);
    t.rooms[0]?.events?.onShareTracks({ video: true, audio: true });
    expect(t.client.snapshot().shareTracks).toEqual({ video: true, audio: true });
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    // The next connection is told before it connects.
    expect(t.rooms[1]?.watching).toEqual([true]);
    expect(t.rooms[1]?.sinks).toEqual([sink]);
    expect(t.client.snapshot().shareTracks).toEqual({ video: false, audio: false });
    t.client.presence(presenceWith(8));
    expect(t.client.snapshot().watching).toBe(true);
    t.client.presence(presenceWith(null));
    expect(t.client.snapshot().watching).toBe(false);
    expect(t.rooms[1]?.watching).toEqual([true, false]);
  });
});
