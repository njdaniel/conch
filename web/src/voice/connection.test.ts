import { describe, expect, it } from 'vitest';
import type { ShareClaimResult, TransmitAnswer, VoiceSessionResult } from '../api';
import type { VoicePresenceV1 } from '../schema/voice';
import { VoiceClient, chosenGone, outputMoved, outputRefused, reconnectLimit, reportWait, type Microphone, type Output, type Phase, type Processing, type ProcessingStore, type Room, type RoomEvents, type ShareSink } from './connection';
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
  // Issue #661: each volume handed to the room, and whether the room was
  // connected by then.
  volumes: { readonly principal: number; readonly volume: number; readonly connected: boolean }[] = [];
  setVolume = (principal: number, volume: number): void => {
    this.volumes.push({ principal, volume, connected: this.connectArgs !== null });
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
  // Issue #658: the microphone chosen before connecting, each switch, the
  // list the browser gives and the device in use. A switch can be held for
  // the test to let finish, or fail.
  choices: (string | null)[] = [];
  switches: (string | null)[] = [];
  switchFailure: Error | null = null;
  holdSwitch = false;
  heldSwitch: (() => void) | null = null;
  list: readonly Microphone[] | null = [
    { id: 'default', label: 'Default - Headset' },
    { id: 'mic-1', label: 'Headset' },
    { id: 'mic-2', label: 'Webcam' },
  ];
  listed = 0;
  inUseNow: string | null = 'default';
  setMicrophone = (choice: string | null): void => {
    this.choices.push(choice);
  };
  switchMicrophone = (choice: string | null): Promise<void> => {
    this.switches.push(choice);
    const finish = (): Promise<void> => {
      if (this.switchFailure !== null) {
        this.inUseNow = null;
        return Promise.reject(this.switchFailure);
      }
      this.inUseNow = choice ?? 'default';
      return Promise.resolve();
    };
    if (!this.holdSwitch) return finish();
    return new Promise<void>((resolve) => {
      this.heldSwitch = resolve;
    }).then(finish);
  };
  microphones = (): Promise<readonly Microphone[] | null> => {
    this.listed++;
    return Promise.resolve(this.list);
  };
  microphoneInUse = (): string | null => this.inUseNow;
  // Issue #659: the voice processing handed to the room, and what each
  // capture says of it (all three as asked, unless a test keeps one on).
  processing: boolean[] = [];
  asked = true;
  kept: Partial<Processing> = {};
  setProcessing = (on: boolean): void => {
    this.processing.push(on);
    this.asked = on;
  };
  microphoneProcessing = (): Processing | null =>
    this.inUseNow === null ? null : { echoCancellation: this.asked, noiseSuppression: this.asked, autoGainControl: this.asked, ...this.kept };
  // Issue #660: each output handed to the room, and whether the room was
  // connected by then; the outputs the browser lists; and, when set, a
  // refusal of every output but the system default.
  outputCalls: { readonly id: string; readonly connected: boolean }[] = [];
  outputList: readonly Output[] = [
    { id: 'default', label: 'Default - Headset' },
    { id: 'out-1', label: 'Desk speakers' },
    { id: 'out-2', label: 'Headset' },
  ];
  outputsRead = 0;
  outputFailure: Error | null = null;
  switchOutput = (id: string): Promise<void> => {
    this.outputCalls.push({ id, connected: this.connectArgs !== null });
    return this.outputFailure !== null && id !== '' ? Promise.reject(this.outputFailure) : Promise.resolve();
  };
  outputs = (): Promise<readonly Output[]> => {
    this.outputsRead++;
    return Promise.resolve(this.outputList);
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
// microphone is the choice remembered from earlier in the tab, and
// processing the voice processing kept for each microphone.
function setup(
  sessions: VoiceSessionResult[] = [okSession()],
  reportAnswers: TransmitAnswer[] = [],
  failFirst = 0,
  held = false,
  prepare?: (room: StubRoom, index: number) => void,
  microphone?: Microphone | null,
  processing?: ProcessingStore,
  // Issue #660: whether the speakers may be chosen (they may not unless a
  // test says), and the output remembered from earlier in the tab.
  speakers?: { readonly selection: boolean; readonly output?: Output | null },
) {
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
      ...(microphone === undefined ? {} : { microphone }),
      ...(processing === undefined ? {} : { processing }),
      ...(speakers === undefined ? {} : { outputSelection: speakers.selection, output: speakers.output ?? null }),
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

// Issue #658: the microphone chosen, the switch, and the microphone missing.
describe('the microphone chosen', () => {
  const headset: Microphone = { id: 'mic-1', label: 'Headset' };
  const webcam: Microphone = { id: 'mic-2', label: 'Webcam' };
  const unplugged: Microphone = { id: 'unplugged', label: 'Old headset' };
  const room = (t: { rooms: StubRoom[] }, i: number): StubRoom => {
    const r = t.rooms[i];
    if (r === undefined) throw new Error(`no room ${String(i)}`);
    return r;
  };

  it('starts from the choice remembered in the tab, and the system default without one', () => {
    expect(setup([okSession()], [], 0, false, undefined, headset).client.snapshot().microphone).toEqual(headset);
    expect(setup().client.snapshot().microphone).toBeNull();
  });

  it('is handed to every new room before it connects: a new session, a renewed room, and Leave then Join', async () => {
    const t = setup([okSession(), okSession(), okSession(), okSession()], [], 0, false, undefined, headset);
    await joined(t);
    expect(room(t, 0).choices).toEqual(['mic-1']);
    expect(room(t, 0).connectArgs?.publish).toBe(true);
    // Chosen in voice: the capture moves now, and every later room has it.
    await t.client.chooseMicrophone(webcam);
    expect(room(t, 0).switches).toEqual(['mic-2']);
    room(t, 0).events?.onDisconnected('participant_removed');
    await settle();
    expect(room(t, 1).choices).toEqual(['mic-2']);
    room(t, 1).events?.onDisconnected('room_deleted');
    await settle();
    expect(room(t, 2).choices).toEqual(['mic-2']);
    await t.client.leave();
    t.client.join('general');
    await settle();
    expect(room(t, 3).choices).toEqual(['mic-2']);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', microphone: webcam, missing: false });
  });

  it('is only remembered while not in voice, and in watch only', async () => {
    const t = setup();
    await t.client.chooseMicrophone(headset);
    expect(t.client.snapshot().microphone).toEqual(headset);
    t.client.join('general', { watchOnly: true });
    await settle();
    await t.client.chooseMicrophone(webcam);
    expect(room(t, 0).switches).toEqual([]);
    expect(room(t, 0).choices).toEqual(['mic-1']);
    expect(t.client.snapshot().microphone).toEqual(webcam);
  });

  it('lists the microphones and the device in use while the microphone is used, and again when devices change', async () => {
    const t = await joined();
    expect(t.client.snapshot().microphones).toEqual(room(t, 0).list);
    expect(t.client.snapshot().inUse).toBe('default');
    room(t, 0).list = [{ id: 'mic-1', label: 'Headset' }];
    room(t, 0).inUseNow = 'mic-1';
    room(t, 0).events?.onDevicesChanged();
    await settle();
    expect(t.client.snapshot().microphones).toEqual([{ id: 'mic-1', label: 'Headset' }]);
    expect(t.client.snapshot().inUse).toBe('mic-1');
  });

  it('reads no list in watch only, and does once the microphone is used', async () => {
    const t = setup();
    t.client.join('general', { watchOnly: true });
    await settle();
    room(t, 0).events?.onDevicesChanged();
    await settle();
    expect(room(t, 0).listed).toBe(0);
    expect(t.client.snapshot().microphones).toBeNull();
    await t.client.useMicrophone();
    await settle();
    expect(room(t, 0).listed).toBe(1);
    expect(t.client.snapshot().microphones).toEqual(room(t, 0).list);
  });

  it('a switch blocks the gate: a press held across it reports stopped, and nothing is sent until a new press', async () => {
    const t = await joined();
    const r = room(t, 0);
    t.client.press();
    await settle();
    expect(r.sending).toEqual([true]);
    expect(t.reports).toEqual(['started']);
    r.holdSwitch = true;
    const switching = t.client.chooseMicrophone(webcam);
    expect(t.client.snapshot().transmitting).toBe(false);
    await settle();
    expect(r.sending).toEqual([true, false]);
    expect(t.reports).toEqual(['started', 'stopped']);
    // A press during the switch is refused.
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(false);
    r.heldSwitch?.();
    await switching;
    await settle();
    // The press still held does not open the gate again.
    expect(t.client.snapshot().transmitting).toBe(false);
    expect(t.client.snapshot().inUse).toBe('mic-2');
    expect(t.reports).toEqual(['started', 'stopped']);
    t.client.release();
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(true);
    expect(r.sending).toEqual([true, false, true]);
    expect(t.reports).toEqual(['started', 'stopped', 'started']);
  });

  it('a switch shuts the open microphone too; new speech after it opens it again', async () => {
    const t = await joined();
    const r = room(t, 0);
    t.client.setMode('open');
    r.levelNow = 0.5;
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(true);
    r.holdSwitch = true;
    const switching = t.client.chooseMicrophone(headset);
    expect(t.client.snapshot().transmitting).toBe(false);
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(false);
    r.heldSwitch?.();
    await switching;
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(true);
    await settle();
    expect(t.reports).toEqual(['started', 'stopped', 'started']);
  });

  it('a microphone lost is missing: the gate stays blocked through presses, and choosing again ends it', async () => {
    const t = await joined();
    const r = room(t, 0);
    t.client.press();
    await settle();
    r.inUseNow = null;
    r.events?.onMicrophoneLost();
    await settle();
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', missing: true, inUse: null, transmitting: false, why: null });
    expect(t.reports).toEqual(['started', 'stopped']);
    t.client.release();
    for (let i = 0; i < 3; i++) {
      t.client.press();
      t.client.release();
      t.clock.advance(100);
    }
    await settle();
    expect(t.reports).toEqual(['started', 'stopped']);
    expect(r.switches).toEqual([]);
    // System default counts as a choice.
    await t.client.chooseMicrophone(null);
    expect(r.switches).toEqual([null]);
    expect(t.client.snapshot()).toMatchObject({ missing: false, inUse: 'default', microphone: null });
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(true);
  });

  it('a microphone missing is not captured by the next connection either, until one is chosen', async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    room(t, 0).events?.onMicrophoneLost();
    room(t, 0).events?.onDisconnected('room_deleted');
    await settle();
    const next = room(t, 1);
    expect(next.connectArgs?.publish).toBe(false);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', missing: true, watchOnly: false });
    t.client.press();
    await settle();
    expect(t.reports).toEqual([]);
    await t.client.chooseMicrophone(headset);
    expect(next.switches).toEqual(['mic-1']);
    expect(t.client.snapshot().missing).toBe(false);
  });

  it.each<[string, RegExp]>([
    ['OverconstrainedError', /No microphone was found/],
    ['NotAllowedError', /Your browser blocked the microphone/],
    ['NotReadableError', /in use by another program/],
  ])('a switch that fails (%s) stays in voice, missing, with the reason', async (name, why) => {
    const t = await joined();
    const r = room(t, 0);
    r.switchFailure = deviceError(name);
    await t.client.chooseMicrophone(webcam);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', missing: true, inUse: null });
    expect(t.client.snapshot().why).toMatch(why);
    expect(r.closed).toBe(false);
    t.client.press();
    await settle();
    expect(t.reports).toEqual([]);
    // Choosing again, and succeeding, clears it.
    r.switchFailure = null;
    await t.client.chooseMicrophone(headset);
    expect(t.client.snapshot()).toMatchObject({ missing: false, why: null, inUse: 'mic-1' });
  });

  it('a remembered microphone missing at Join voice stops with the reason and lets the choice go', async () => {
    const t = setup([okSession(), okSession()], [], 0, false, (r, i) => {
      if (i === 0) r.connectFailure = deviceError('OverconstrainedError');
    }, unplugged);
    t.client.join('general');
    await settle();
    expect(t.client.snapshot()).toMatchObject({ phase: 'stopped', why: chosenGone, microphone: null });
    expect(t.clock.pending).toBe(0);
    // The next Join voice uses the system default, because the person pressed it.
    t.client.join('general');
    await settle();
    expect(room(t, 1).choices).toEqual([null]);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', why: null });
  });

  it('the system default, refused, is the device error it always was', async () => {
    const t = setup([okSession()], [], 0, false, (r) => {
      r.connectFailure = deviceError('OverconstrainedError');
    });
    t.client.join('general');
    await settle();
    expect(t.client.snapshot().why).toMatch(/No microphone was found/);
  });

  it('a remembered microphone missing at Use microphone keeps watch only, says so, and lets the choice go', async () => {
    const t = setup([okSession()], [], 0, false, (r) => {
      r.microphoneFailure = deviceError('OverconstrainedError');
    }, unplugged);
    t.client.join('general', { watchOnly: true });
    await settle();
    await t.client.useMicrophone();
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', watchOnly: true, why: chosenGone, microphone: null });
    // Handed to the room at connecting and again at Use microphone, then let go.
    expect(room(t, 0).choices).toEqual(['unplugged', 'unplugged', null]);
  });

  it('Leave ends the missing state; the choice is kept', async () => {
    const t = setup([okSession(), okSession()], [], 0, false, undefined, headset);
    await joined(t);
    room(t, 0).events?.onMicrophoneLost();
    await t.client.leave();
    expect(t.client.snapshot()).toMatchObject({ phase: 'idle', missing: false, microphones: null, inUse: null, microphone: headset });
  });

  // A browser may give a device a new id on a reload: the choice is found
  // again by its name, before anything is captured.
  it('finds a remembered microphone under a new id by its name, and keeps the new id', async () => {
    const t = setup([okSession()], [], 0, false, (r) => {
      r.list = [{ id: 'default', label: 'Default - Headset' }, { id: 'renamed-1', label: 'Headset' }, webcam];
    }, headset);
    await joined(t);
    expect(room(t, 0).choices).toEqual(['renamed-1']);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', microphone: { id: 'renamed-1', label: 'Headset' } });
  });

  it.each<[string, readonly Microphone[] | null]>([
    ['no device by that name', [{ id: 'default', label: 'Default' }, webcam]],
    ['two devices by that name', [{ id: 'a', label: 'Headset' }, { id: 'b', label: 'Headset' }]],
    ['a browser that names no device', null],
  ])('captures a remembered microphone by its own id when there is %s', async (_what, list) => {
    const t = setup([okSession()], [], 0, false, (r) => {
      r.list = list;
    }, headset);
    await joined(t);
    expect(room(t, 0).choices).toEqual(['mic-1']);
    expect(t.client.snapshot().microphone).toEqual(headset);
  });
});

// Issue #659: the browser's voice processing, one switch per microphone,
// kept per device with System default a device of its own.
describe('voice processing', () => {
  const headset: Microphone = { id: 'mic-1', label: 'Headset' };
  const webcam: Microphone = { id: 'mic-2', label: 'Webcam' };
  const room = (t: { rooms: StubRoom[] }, i: number): StubRoom => {
    const r = t.rooms[i];
    if (r === undefined) throw new Error(`no room ${String(i)}`);
    return r;
  };
  // A store of the choices, as the page keeps them: a device never set is on.
  const store = (entries: [string | null, boolean][] = []): ProcessingStore & { readonly kept: Map<string | null, boolean> } => {
    const kept = new Map(entries);
    return {
      kept,
      get: (device) => kept.get(device) ?? true,
      set: (device, on) => {
        kept.set(device, on);
      },
    };
  };
  const off: Processing = { echoCancellation: false, noiseSuppression: false, autoGainControl: false };
  const on: Processing = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };

  it('is on for a device never set, and System default is a device of its own', () => {
    expect(setup().client.snapshot().processing).toBe(true);
    expect(setup([okSession()], [], 0, false, undefined, headset, store()).client.snapshot().processing).toBe(true);
    expect(setup([okSession()], [], 0, false, undefined, headset, store([['mic-1', false]])).client.snapshot().processing).toBe(false);
    expect(setup([okSession()], [], 0, false, undefined, null, store([['mic-1', false]])).client.snapshot().processing).toBe(true);
  });

  it("reaches every new room before it connects, as the device's own: a new session, a renewed room, and Leave then Join", async () => {
    const t = setup([okSession(), okSession(), okSession(), okSession()], [], 0, false, undefined, headset, store([['mic-1', false]]));
    await joined(t);
    expect(room(t, 0).processing).toEqual([false]);
    room(t, 0).events?.onDisconnected('participant_removed');
    await settle();
    expect(room(t, 1).processing).toEqual([false]);
    room(t, 1).events?.onDisconnected('room_deleted');
    await settle();
    expect(room(t, 2).processing).toEqual([false]);
    await t.client.leave();
    t.client.join('general');
    await settle();
    expect(room(t, 3).processing).toEqual([false]);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', processing: false, reported: off });
  });

  it('turned off in voice, captures the same device again with all three off, and says what the capture reports; on again, all three on', async () => {
    const kept = store();
    const t = await joined(setup([okSession()], [], 0, false, undefined, headset, kept));
    const r = room(t, 0);
    r.inUseNow = 'mic-1';
    await t.client.setProcessing(false);
    expect(r.switches).toEqual(['mic-1']);
    expect(r.processing).toEqual([true, false]);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', processing: false, reported: off, inUse: 'mic-1', missing: false });
    expect(kept.kept.get('mic-1')).toBe(false);
    await t.client.setProcessing(true);
    expect(r.switches).toEqual(['mic-1', 'mic-1']);
    expect(r.processing).toEqual([true, false, true]);
    expect(t.client.snapshot()).toMatchObject({ processing: true, reported: on });
    // Never another connection, never a close.
    expect(t.rooms).toHaveLength(1);
    expect(r.closed).toBe(false);
  });

  it('System default keeps its own setting, apart from every device', async () => {
    const kept = store([['mic-1', false]]);
    const t = await joined(setup([okSession()], [], 0, false, undefined, null, kept));
    await t.client.setProcessing(false);
    expect(room(t, 0).switches).toEqual([null]);
    expect(kept.kept.get(null)).toBe(false);
    expect(kept.kept.get('mic-2')).toBeUndefined();
    await t.client.chooseMicrophone(webcam);
    expect(t.client.snapshot().processing).toBe(true);
    expect(room(t, 0).processing.at(-1)).toBe(true);
  });

  it('reports what the capture says: a filter the browser kept on is on', async () => {
    const t = await joined();
    room(t, 0).kept = { noiseSuppression: true };
    await t.client.setProcessing(false);
    expect(t.client.snapshot().reported).toEqual({ echoCancellation: false, noiseSuppression: true, autoGainControl: false });
  });

  it('blocks the gate during the restart: a press held across it reports stopped, and nothing is sent until a new press', async () => {
    const t = await joined();
    const r = room(t, 0);
    t.client.press();
    await settle();
    expect(t.reports).toEqual(['started']);
    r.holdSwitch = true;
    const changing = t.client.setProcessing(false);
    expect(t.client.snapshot().transmitting).toBe(false);
    await settle();
    expect(r.sending).toEqual([true, false]);
    expect(t.reports).toEqual(['started', 'stopped']);
    // Nothing is said of the processing until the new capture says it.
    expect(t.client.snapshot().reported).toBeNull();
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(false);
    r.heldSwitch?.();
    await changing;
    await settle();
    // The press still held does not open the gate again.
    expect(t.client.snapshot().transmitting).toBe(false);
    expect(t.reports).toEqual(['started', 'stopped']);
    t.client.release();
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(true);
    expect(t.reports).toEqual(['started', 'stopped', 'started']);
  });

  it('shuts the open microphone for the restart; new speech after it opens it again, at the same threshold', async () => {
    const t = await joined();
    const r = room(t, 0);
    t.client.setMode('open');
    t.client.setThreshold(0.3);
    r.levelNow = 0.5;
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(true);
    r.holdSwitch = true;
    const changing = t.client.setProcessing(false);
    expect(t.client.snapshot().transmitting).toBe(false);
    r.heldSwitch?.();
    await changing;
    expect(t.client.snapshot().threshold).toBe(0.3);
    // Below the threshold it stays shut; above it, it opens.
    r.levelNow = 0.2;
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(false);
    r.levelNow = 0.5;
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(true);
    await settle();
    expect(t.reports).toEqual(['started', 'stopped', 'started']);
  });

  it("choosing a device captures it with that device's own setting", async () => {
    const t = await joined(setup([okSession()], [], 0, false, undefined, null, store([['mic-2', false]])));
    const r = room(t, 0);
    await t.client.chooseMicrophone(webcam);
    expect(r.processing.at(-1)).toBe(false);
    expect(t.client.snapshot()).toMatchObject({ microphone: webcam, processing: false, reported: off });
    await t.client.chooseMicrophone(headset);
    expect(r.processing.at(-1)).toBe(true);
    expect(t.client.snapshot()).toMatchObject({ microphone: headset, processing: true, reported: on });
  });

  it('is only remembered while not in voice, in watch only, or while the microphone is missing; the next capture uses it', async () => {
    const kept = store();
    const t = setup([okSession()], [], 0, false, undefined, null, kept);
    await t.client.setProcessing(false);
    expect(kept.kept.get(null)).toBe(false);
    t.client.join('general', { watchOnly: true });
    await settle();
    await t.client.setProcessing(true);
    await t.client.setProcessing(false);
    expect(room(t, 0).switches).toEqual([]);
    await t.client.useMicrophone();
    expect(room(t, 0).processing.at(-1)).toBe(false);
    expect(t.client.snapshot().reported).toEqual(off);
    // Missing: nothing is captured until a microphone is chosen.
    room(t, 0).inUseNow = null;
    room(t, 0).events?.onMicrophoneLost();
    await settle();
    expect(t.client.snapshot().reported).toBeNull();
    await t.client.setProcessing(true);
    expect(room(t, 0).switches).toEqual([]);
    await t.client.chooseMicrophone(null);
    expect(room(t, 0).switches).toEqual([null]);
    expect(room(t, 0).processing.at(-1)).toBe(true);
    expect(t.client.snapshot()).toMatchObject({ missing: false, processing: true, reported: on });
  });

  it.each<[string, RegExp]>([
    ['OverconstrainedError', /No microphone was found/],
    ['NotReadableError', /in use by another program/],
  ])('a restart that fails (%s) is the missing state, with the reason, and captures no other device', async (name, why) => {
    const t = await joined(setup([okSession()], [], 0, false, undefined, headset));
    const r = room(t, 0);
    r.switchFailure = deviceError(name);
    await t.client.setProcessing(false);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', missing: true, inUse: null, reported: null, microphone: headset });
    expect(t.client.snapshot().why).toMatch(why);
    expect(r.switches).toEqual(['mic-1']);
    t.client.press();
    await settle();
    expect(t.reports).toEqual([]);
  });

  it('a remembered microphone found under a new id keeps its setting', async () => {
    const kept = store([['mic-1', false]]);
    const t = setup([okSession()], [], 0, false, (r) => {
      r.list = [{ id: 'default', label: 'Default - Headset' }, { id: 'renamed-1', label: 'Headset' }, webcam];
    }, headset, kept);
    await joined(t);
    expect(room(t, 0).processing).toEqual([false]);
    expect(t.client.snapshot()).toMatchObject({ microphone: { id: 'renamed-1', label: 'Headset' }, processing: false });
    expect(kept.kept.get('renamed-1')).toBe(false);
  });

  it('changed while connecting, the capture is made again once connected', async () => {
    // The connection waits for the test, as a slow one does.
    let connected = (): void => undefined;
    const t = setup([okSession()], [], 0, false, (r) => {
      const connect = r.connect;
      r.connect = (...args) =>
        new Promise<void>((resolve) => {
          connected = () => {
            resolve(connect(...args));
          };
        });
    });
    t.client.join('general');
    await settle();
    const r = room(t, 0);
    expect(r.processing).toEqual([true]);
    await t.client.setProcessing(false);
    expect(r.switches).toEqual([]);
    connected();
    await settle();
    expect(r.processing).toEqual([true, false]);
    expect(r.switches).toEqual([null]);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', processing: false, reported: off });
  });
});

// Issue #669: a microphone chosen while connecting is the one captured once
// connected, as #659 does for voice processing.
describe('a microphone chosen while connecting', () => {
  const webcam: Microphone = { id: 'mic-2', label: 'Webcam' };

  // A client whose first connection waits for the test, as a slow one does.
  function slow() {
    let connected = (): void => undefined;
    const t = setup([okSession()], [], 0, false, (r) => {
      const connect = r.connect;
      r.connect = (...args) =>
        new Promise<void>((resolve) => {
          connected = () => {
            resolve(connect(...args));
          };
        });
    });
    return {
      t,
      connect: () => {
        connected();
      },
    };
  }

  it('is captured once connected, and is the one in use', async () => {
    const { t, connect } = slow();
    t.client.join('general');
    await settle();
    const r = t.rooms[0];
    if (r === undefined) throw new Error('no room');
    expect(r.choices).toEqual([null]);
    await t.client.chooseMicrophone(webcam);
    expect(t.client.snapshot().phase).toBe('connecting');
    expect(r.switches).toEqual([]);
    connect();
    await settle();
    expect(r.switches).toEqual(['mic-2']);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', microphone: webcam, inUse: 'mic-2', missing: false });
    // The same connection moved its capture: no other room, none closed.
    expect(t.rooms).toHaveLength(1);
    expect(r.closed).toBe(false);
  });

  it('chosen back to what the room was given, nothing is captured again', async () => {
    const { t, connect } = slow();
    t.client.join('general');
    await settle();
    await t.client.chooseMicrophone(webcam);
    await t.client.chooseMicrophone(null);
    connect();
    await settle();
    expect(t.rooms[0]?.switches).toEqual([]);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', microphone: null, inUse: 'default' });
  });
});

// Issue #660: the speakers chosen, handed to every new room before it
// connects, found again by name, let go when refused or no longer listed,
// and followed when the SDK moves sound away from an output that went.
describe('the speakers chosen', () => {
  const desk: Output = { id: 'out-1', label: 'Desk speakers' };
  const headset: Output = { id: 'out-2', label: 'Headset' };
  const room = (t: { rooms: StubRoom[] }, i: number): StubRoom => {
    const r = t.rooms[i];
    if (r === undefined) throw new Error(`no room ${String(i)}`);
    return r;
  };
  const speakers = (output: Output | null = null) => ({ selection: true, output });
  const sessions = () => [okSession(), okSession(), okSession(), okSession()];

  it('is handed to every new room before it connects: a new session, a renewed room, and Leave then Join', async () => {
    const t = setup(sessions(), [], 0, false, undefined, undefined, undefined, speakers(desk));
    expect(t.client.snapshot()).toMatchObject({ outputSelection: true, output: desk });
    await joined(t);
    expect(room(t, 0).outputCalls).toEqual([{ id: 'out-1', connected: false }]);
    // Chosen in voice: every voice moves now, and every later room has it.
    await t.client.chooseOutput(headset);
    expect(room(t, 0).outputCalls.at(-1)).toEqual({ id: 'out-2', connected: true });
    room(t, 0).events?.onDisconnected('participant_removed');
    await settle();
    expect(room(t, 1).outputCalls).toEqual([{ id: 'out-2', connected: false }]);
    room(t, 1).events?.onDisconnected('room_deleted');
    await settle();
    expect(room(t, 2).outputCalls).toEqual([{ id: 'out-2', connected: false }]);
    await t.client.leave();
    t.client.join('general');
    await settle();
    expect(room(t, 3).outputCalls).toEqual([{ id: 'out-2', connected: false }]);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', output: headset, outputNote: null });
  });

  it('System default is the empty id, and a new room starts on it without being told', async () => {
    const t = await joined(setup(sessions(), [], 0, false, undefined, undefined, undefined, speakers(desk)));
    await t.client.chooseOutput(null);
    expect(room(t, 0).outputCalls.at(-1)).toEqual({ id: '', connected: true });
    room(t, 0).events?.onDisconnected('room_deleted');
    await settle();
    expect(room(t, 1).outputCalls).toEqual([]);
    expect(t.client.snapshot().output).toBeNull();
  });

  it('a choice the browser refuses keeps the output in use, and says so', async () => {
    const t = await joined(setup(sessions(), [], 0, false, undefined, undefined, undefined, speakers(desk)));
    room(t, 0).outputFailure = new DOMException('not allowed', 'NotAllowedError');
    await t.client.chooseOutput(headset);
    expect(t.client.snapshot()).toMatchObject({ output: desk, outputNote: outputRefused(headset, desk) });
    expect(t.client.snapshot().outputNote).toBe('Could not play sound on Headset; it stays on Desk speakers.');
    // A later choice the browser takes clears the note.
    room(t, 0).outputFailure = null;
    await t.client.chooseOutput(null);
    expect(t.client.snapshot()).toMatchObject({ output: null, outputNote: null });
  });

  it('a choice the browser refuses at a new room is let go for the system default, and said', async () => {
    const t = setup(sessions(), [], 0, false, (r) => {
      r.outputFailure = new DOMException('not allowed', 'NotAllowedError');
    }, undefined, undefined, speakers(desk));
    await joined(t);
    expect(t.client.snapshot()).toMatchObject({ phase: 'connected', output: null, outputNote: 'Could not play sound on Desk speakers; it stays on System default.' });
  });

  it('is found by its name under a new id, and let go when it is no longer listed', async () => {
    const renamed = { id: 'out-9', label: 'Desk speakers' };
    const t = setup(sessions(), [], 0, false, (r, i) => {
      r.outputList = i === 0 ? [renamed, headset] : [headset];
    }, undefined, undefined, speakers(desk));
    await joined(t);
    expect(room(t, 0).outputCalls[0]).toEqual({ id: 'out-9', connected: false });
    expect(t.client.snapshot()).toMatchObject({ output: renamed, outputNote: null });
    room(t, 0).events?.onDisconnected('room_deleted');
    await settle();
    expect(room(t, 1).outputCalls).toEqual([]);
    expect(t.client.snapshot()).toMatchObject({ output: null, outputNote: outputRefused(renamed, null) });
  });

  it('is found again once the browser names its outputs: under a new id, or let go', async () => {
    const t = setup(sessions(), [], 0, false, (r) => {
      // Nothing named before the microphone is allowed.
      r.outputList = [];
    }, undefined, undefined, speakers(desk));
    await joined(t);
    const r = room(t, 0);
    // Not to be told: handed to the room as it is.
    expect(r.outputCalls).toEqual([{ id: 'out-1', connected: false }]);
    expect(t.client.snapshot()).toMatchObject({ outputs: [], output: desk });
    r.outputList = [{ id: 'out-7', label: 'Desk speakers' }, headset];
    r.events?.onDevicesChanged();
    await settle();
    expect(r.outputCalls.at(-1)).toEqual({ id: 'out-7', connected: true });
    expect(t.client.snapshot()).toMatchObject({ outputs: r.outputList, output: { id: 'out-7', label: 'Desk speakers' }, outputNote: null });
    r.outputList = [headset];
    r.events?.onDevicesChanged();
    await settle();
    expect(r.outputCalls.at(-1)).toEqual({ id: '', connected: true });
    expect(t.client.snapshot()).toMatchObject({ output: null, outputNote: 'Could not play sound on Desk speakers; it stays on System default.' });
  });

  it('lists the outputs in voice, in watch only too, and forgets them on leaving', async () => {
    const t = setup(sessions(), [], 0, false, undefined, undefined, undefined, speakers());
    t.client.join('general', { watchOnly: true });
    await settle();
    expect(t.client.snapshot().outputs).toEqual(room(t, 0).outputList);
    await t.client.leave();
    expect(t.client.snapshot()).toMatchObject({ outputs: null, outputNote: null });
  });

  it('follows the SDK when the output in use went: the one now in use, and the note', async () => {
    const t = await joined(setup(sessions(), [], 0, false, undefined, undefined, undefined, speakers(headset)));
    room(t, 0).events?.onOutputMoved('out-1');
    expect(t.client.snapshot()).toMatchObject({ output: desk, outputNote: outputMoved(desk) });
    expect(t.client.snapshot().outputNote).toBe('Your speakers were disconnected; sound now plays on Desk speakers.');
  });

  it('where the speakers cannot be chosen, nothing is asked of the room', async () => {
    const t = setup(sessions(), [], 0, false, undefined, undefined, undefined, { selection: false, output: desk });
    await joined(t);
    await t.client.chooseOutput(headset);
    expect(room(t, 0).outputCalls).toEqual([]);
    expect(room(t, 0).outputsRead).toBe(0);
    expect(t.client.snapshot()).toMatchObject({ outputSelection: false, outputs: null });
  });
});

// Issue #661: each other person's volume, on this page only. Held by the
// client until the page is reloaded, never stored, and handed to every room
// before it connects, so it holds through a reconnect, a renewed room, and a
// Leave and a Join.
describe('the volume of each person', () => {
  it('is handed to the room at once, and is in the state; 100% leaves the person out', async () => {
    const t = await joined();
    t.client.setVolume(8, 0.4);
    expect(t.rooms[0]?.volumes).toEqual([{ principal: 8, volume: 0.4, connected: true }]);
    expect(t.client.snapshot().volumes).toEqual(new Map([[8, 0.4]]));
    t.client.setVolume(8, 1);
    expect(t.rooms[0]?.volumes.at(-1)).toEqual({ principal: 8, volume: 1, connected: true });
    expect(t.client.snapshot().volumes.size).toBe(0);
  });

  it('is kept between 0 and 1, in steps of 5%', async () => {
    const t = await joined();
    t.client.setVolume(8, -1);
    t.client.setVolume(9, 7);
    t.client.setVolume(10, 0.42);
    t.client.setVolume(11, Number.NaN);
    expect(t.rooms[0]?.volumes.map((v) => v.volume)).toEqual([0, 1, 0.4, 1]);
    expect(t.client.snapshot().volumes).toEqual(new Map([[8, 0], [10, 0.4]]));
  });

  it('set before joining is handed to the first room before it connects', async () => {
    const t = setup();
    t.client.setVolume(8, 0);
    t.client.join('general');
    await settle();
    expect(t.rooms[0]?.volumes).toEqual([{ principal: 8, volume: 0, connected: false }]);
  });

  it('is handed to every new room before it connects: a reconnect, a renewed room, and a Leave and a Join', async () => {
    const t = await joined();
    t.client.setVolume(8, 0);
    t.client.setVolume(9, 0.25);
    const kept = [
      { principal: 8, volume: 0, connected: false },
      { principal: 9, volume: 0.25, connected: false },
    ];
    // A connection lost, and a room renewed.
    t.rooms[0]?.events?.onDisconnected('lost');
    t.clock.advance(1000);
    await settle();
    t.rooms[1]?.events?.onDisconnected('room_deleted');
    await settle();
    // Left, and joined again.
    await t.client.leave();
    expect(t.client.snapshot().volumes).toEqual(new Map([[8, 0], [9, 0.25]]));
    t.client.join('general');
    await settle();
    expect(t.rooms).toHaveLength(4);
    for (const room of t.rooms.slice(1)) expect(room.volumes).toEqual(kept);
    expect(t.client.snapshot().phase).toBe('connected');
  });

  it('deafen is apart from it: undeafen leaves every volume as it was', async () => {
    const t = await joined();
    t.client.setVolume(8, 0.4);
    t.client.setDeafened(true);
    t.client.setDeafened(false);
    expect(t.rooms[0]?.volumes).toHaveLength(1);
    expect(t.client.snapshot().volumes).toEqual(new Map([[8, 0.4]]));
  });
});
