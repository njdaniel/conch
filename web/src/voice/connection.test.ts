import { describe, expect, it } from 'vitest';
import type { ShareClaimResult, TransmitAnswer, VoiceSessionResult } from '../api';
import type { VoicePresenceV1 } from '../schema/voice';
import type { CameraClaimResult } from '../api';
import type { Camera, CameraProfile, KeptCamera } from './camera';
import { VoiceClient, chosenGone, earlierGone, levelEvery, meterEvery, outputMoved, outputRefused, reconnectLimit, reportWait, type CameraTrack, type Microphone, type ModeStore, type Output, type Phase, type Processing, type ProcessingStore, type Room, type RoomEvents, type ShareSink } from './connection';
import { resumeLimit, type Captured, type KeptScreen } from './share';
import { defaultTail } from './gate';
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
  // When set, a mute waits for the test to let it finish; and so, apart, an
  // unmute (issue #965).
  holdMute = false;
  heldMute: (() => void) | null = null;
  holdUnmute = false;
  heldUnmute: (() => void) | null = null;
  connect = (url: string, tok: string, name: string, publish: boolean, events: RoomEvents): Promise<void> => {
    this.connectArgs = { url, token: tok, roomName: name, publish };
    this.events = events;
    if (this.connectFailure !== null) return Promise.reject(this.connectFailure);
    return this.connectError ? Promise.reject(new Error('unreachable')) : Promise.resolve();
  };
  setSending = (on: boolean): Promise<void> => {
    this.sending.push(on);
    this.log?.push(on ? 'unmute' : 'mute');
    if (on && this.holdUnmute) {
      return new Promise((resolve) => {
        this.heldUnmute = resolve;
      });
    }
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
    this.deafenedWhen.push(this.connectArgs !== null);
  };
  // Issue #683: for each deafen handed to the room, whether it was connected
  // by then.
  deafenedWhen: boolean[] = [];
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
    this.camLog.push('close');
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
  // The camera (ADR-015): each step recorded in camLog, which the test shares
  // with conchd's camera calls, so that their order is seen; the local
  // track's state; a capture that fails with a browser's error; and the
  // cameras listed.
  camLog: string[] = [];
  camState: CameraTrack = 'none';
  captureError: Error | null = null;
  grantError: Error | null = null;
  cameraList: readonly Camera[] | null = [
    { id: 'cam-1', label: 'Desk camera' },
    { id: 'cam-2', label: 'Laptop camera' },
  ];
  keptLiveCamera = true;
  viewings: boolean[] = [];
  claimedSets: number[][] = [];
  // The profile each capture asked for, and the step a test holds: the next
  // capture, grant wait or publish waits until the test lets it go. As in
  // sdk.ts, a stop ends a grant wait or a publish under way; a capture asked
  // of the browser still comes, and its owner lets it go.
  profiles: (CameraProfile | undefined)[] = [];
  hold: 'capture' | 'grant' | 'publish' | null = null;
  letGo: () => void = () => undefined;
  private held: { readonly step: string; readonly fail: () => void } | null = null;
  private waitHeld(step: 'capture' | 'grant' | 'publish'): Promise<void> {
    return new Promise<void>((resolve, reject) => {
      this.letGo = resolve;
      this.held = { step, fail: () => { reject(new Error('stopped')); } };
    });
  }
  // Resolves at once but for the step held, so that an unheld test takes the
  // same turns it always did.
  private holdThen(step: 'capture' | 'grant' | 'publish', then: () => void): Promise<void> {
    if (this.hold !== step) {
      then();
      return Promise.resolve();
    }
    this.hold = null;
    return this.waitHeld(step).then(then);
  }
  captureCamera = (choice: string | null, profile?: CameraProfile): Promise<void> => {
    this.profiles.push(profile);
    this.camLog.push(`capture ${choice ?? 'browser'}`);
    if (this.captureError !== null) return Promise.reject(this.captureError);
    return this.holdThen('capture', () => {
      this.camState = 'captured';
    });
  };
  awaitCameraGrant = (): Promise<void> => {
    this.camLog.push('grant');
    if (this.grantError !== null) return Promise.reject(this.grantError);
    return this.holdThen('grant', () => undefined);
  };
  publishCamera = (): Promise<void> => {
    this.camLog.push('publish camera');
    return this.holdThen('publish', () => {
      this.camState = 'published';
      this.events?.onCameraChanged();
    });
  };
  stopCamera = (): Promise<void> => {
    if (this.camState !== 'none') this.camLog.push('stop camera');
    this.camState = 'none';
    const held = this.held;
    if (held !== null && held.step !== 'capture') {
      this.held = null;
      held.fail();
    }
    this.events?.onCameraChanged();
    return Promise.resolve();
  };
  keepCamera = (): KeptCamera | null => {
    if (this.camState === 'none') return null;
    this.camLog.push('keep camera');
    this.camState = 'none';
    // As sdk.ts: the room says its own camera changed, at once.
    this.events?.onCameraChanged();
    const log = this.camLog;
    return {
      live: () => this.keptLiveCamera,
      onEnded: null,
      stop: () => {
        log.push('stop kept camera');
      },
    };
  };
  adoptCamera = (): void => {
    this.camLog.push('adopt camera');
    this.camState = 'captured';
  };
  cameraTrack = (): CameraTrack => this.camState;
  camDevice = 'cam-1';
  cameraInUse = (): string | null => (this.camState === 'none' ? null : this.camDevice);
  cameras = (): Promise<readonly Camera[] | null> => Promise.resolve(this.cameraList);
  switchError: Error | null = null;
  // When set, the next switch waits, and the test fails it.
  holdSwitchCamera = false;
  failHeldSwitch: ((err: Error) => void) | null = null;
  switchCamera = (choice: string): Promise<void> => {
    this.camLog.push(`switch ${choice}`);
    if (this.holdSwitchCamera) {
      this.holdSwitchCamera = false;
      return new Promise((_resolve, reject) => {
        this.failHeldSwitch = reject;
      });
    }
    if (this.switchError === null) {
      this.camDevice = choice;
      return Promise.resolve();
    }
    this.camState = 'none';
    return Promise.reject(this.switchError);
  };
  attachSelf = (): void => undefined;
  setViewing = (on: boolean): void => {
    this.viewings.push(on);
  };
  shownSets: boolean[] = [];
  setShown = (on: boolean): void => {
    this.shownSets.push(on);
  };
  setCameras = (claimed: ReadonlySet<number>): void => {
    this.claimedSets.push([...claimed]);
  };
  attachCamera = (): void => undefined;
}

// The mode the harness starts in: push to talk, which most of these tests are of (issue #1244 made the open microphone the default).
const pushToTalk: ModeStore = { get: () => 'ptt', set: () => undefined };

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
  // ADR-015: conchd's answers to camera claims, in order (a granted claim by
  // default), and the camera remembered from earlier in the tab.
  camera?: { readonly answers?: CameraClaimResult[]; readonly choice?: Camera | null; readonly holdClaims?: boolean },
) {
  const clock = new FakeClock();
  const rooms: StubRoom[] = [];
  const asked: string[] = [];
  const reports: string[] = [];
  // The channel each report named (issue #728).
  const reportedIn: string[] = [];
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
  const trace: string[] = [];
  // What the camera asked of the rooms and of conchd, in order.
  const camLog: string[] = [];
  const cameraAnswers = camera?.answers ?? [];
  // With holdClaims, each claim waits for the test to answer it.
  const claimsWaiting: (() => void)[] = [];
  const client = new VoiceClient(
    {
      clock,
      mode: pushToTalk,
      trace: (line) => trace.push(line),
      random: () => 0.999999,
      createSession: (channel) => {
        asked.push(channel);
        camLog.push(`session ${channel}`);
        return Promise.resolve(sessions.shift() ?? okSession());
      },
      report: (channel, state) => {
        reports.push(state);
        reportedIn.push(channel);
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
        room.camLog = camLog;
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
      claimCamera: (channel, startedAt) => {
        camLog.push(startedAt === undefined ? `POST ${channel}` : `POST ${channel} ?started_at=${startedAt}`);
        const answer = cameraAnswers.shift() ?? cameraGranted;
        if (camera?.holdClaims !== true) return Promise.resolve(answer);
        return new Promise((resolve) => {
          claimsWaiting.push(() => { resolve(answer); });
        });
      },
      stopCamera: (channel, startedAt) => {
        camLog.push(`DELETE ${channel} ?started_at=${startedAt}`);
        return Promise.resolve();
      },
      ...(camera?.choice === undefined ? {} : { camera: camera.choice }),
      ...(microphone === undefined ? {} : { microphone }),
      ...(processing === undefined ? {} : { processing }),
      ...(speakers === undefined ? {} : { outputSelection: speakers.selection, output: speakers.output ?? null }),
    },
    () => clock.now,
  );
  return { client, clock, rooms, asked, reports, reportedIn, shares, log, answer, trace, camLog, claimsWaiting };
}

const cameraGranted: CameraClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:01.000Z' } };

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
  // Issue #1041: the trace names each step of a press, in order, with the time
  // in ms, and carries nothing of the room.
  it('traces a press and its release, in order, without the room or the token', async () => {
    const t = await joined();
    t.client.press();
    await settle();
    t.client.release();
    t.clock.advance(defaultTail);
    await settle();
    expect(t.trace.map((l) => l.replace(/^\d+ /, ''))).toEqual([
      'gate open',
      'started answered or waited out',
      'track unmuted',
      'gate shut',
      'track muted',
      'stopped reported',
    ]);
    for (const line of t.trace) {
      expect(line).toMatch(/^\d+ [a-z ]+$/);
      expect(line).not.toContain(roomName);
      expect(line).not.toContain(token);
    }
  });

  // A trace that throws (here, the array it writes to is frozen) changes
  // nothing: the release still mutes the track and reports stopped.
  it('a trace that throws does not stop a release from muting and reporting', async () => {
    const t = await joined();
    Object.freeze(t.trace);
    t.client.press();
    await settle();
    t.client.release();
    t.clock.advance(defaultTail);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true, false]);
    expect(t.reports).toEqual(['started', 'stopped']);
  });

  it('a press opens the track and reports started; a release closes it after the tail and reports stopped', async () => {
    const t = await joined();
    t.client.press();
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true]);
    expect(t.reports).toEqual(['started']);
    expect(t.client.snapshot().transmitting).toBe(true);
    t.client.release();
    t.clock.advance(defaultTail);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true, false]);
    expect(t.reports).toEqual(['started', 'stopped']);
  });

  // Issue #965: losing focus shuts at once, with no release delay, which can
  // be 2 s; and a press waiting for its answer never opens the track after.
  it('losing focus mutes at once, without the release delay, and reports stopped', async () => {
    const t = await joined();
    t.client.setReleaseDelay(2000);
    t.client.press();
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true]);
    t.client.lost();
    expect(t.rooms[0]?.sending).toEqual([true, false]);
    expect(t.client.snapshot()).toMatchObject({ talking: false, transmitting: false });
    await settle();
    expect(t.reports).toEqual(['started', 'stopped']);
    t.clock.advance(2000);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true, false]);
  });

  it('losing focus while a press waits for its answer: the track never opens', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.setReleaseDelay(2000);
    t.client.press();
    await settle();
    t.client.lost();
    t.clock.advance(100);
    await t.answer('ok');
    await settle();
    t.clock.advance(reportWait + 2000);
    await settle();
    expect(t.rooms[0]?.sending).not.toContain(true);
    expect(t.client.snapshot().transmitting).toBe(false);
    await t.answer('ok');
    expect(t.reports).toEqual(['started', 'stopped']);
  });

  it('losing focus leaves the open microphone open', async () => {
    const t = await joined();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    t.client.setMode('open');
    room.levelNow = 0.5;
    t.clock.advance(levelEvery);
    await settle();
    t.client.lost();
    t.clock.advance(levelEvery);
    await settle();
    expect(room.sending).toEqual([true]);
    expect(t.client.snapshot().transmitting).toBe(true);
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
    expect(t.client.snapshot().talking).toBe(true);
  });

  it('the open microphone opens on speech and closes after the hang time', async () => {
    const t = await joined();
    t.client.setMode('open');
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.levelNow = 0.5;
    t.clock.advance(50);
    expect(t.client.snapshot().talking).toBe(true);
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
    expect(t.client.snapshot().talking).toBe(true);
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
    t.clock.advance(defaultTail);
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
    t.clock.advance(defaultTail);
    t.client.press();
    t.clock.advance(50);
    t.client.release();
    t.clock.advance(defaultTail);
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

// Issue #965: what the page says is what is sent, the release delay is the
// person's, and the open microphone hears an onset sooner.
describe('what is sent, and when', () => {
  it('says transmitting only once the track is unmuted: not while the press waits for its answer', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.press();
    await settle();
    t.clock.advance(300);
    await settle();
    // The key is down and the gate open, but nothing is sent yet.
    expect(t.rooms[0]?.sending).toEqual([]);
    expect(t.client.snapshot()).toMatchObject({ talking: true, transmitting: false });
    await t.answer('ok');
    expect(t.rooms[0]?.sending).toEqual([true]);
    expect(t.client.snapshot()).toMatchObject({ talking: true, transmitting: true });
    // The gate shuts and the mute goes at once: so does the indicator.
    t.client.release();
    t.clock.advance(defaultTail);
    expect(t.rooms[0]?.sending).toEqual([true, false]);
    expect(t.client.snapshot()).toMatchObject({ talking: false, transmitting: false });
  });

  it('a press released while its report waits is never said to transmit', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    const said: boolean[] = [];
    t.client.subscribe(() => {
      said.push(t.client.snapshot().transmitting);
    });
    t.client.press();
    await settle();
    t.client.release();
    t.clock.advance(defaultTail);
    await settle();
    await t.answer('ok');
    await t.answer('ok');
    expect(t.rooms[0]?.sending).toEqual([false]);
    expect(said).not.toContain(true);
  });

  it('a report that never answers: transmitting from reportWait, when the track opens anyway', async () => {
    const t = await joined(setup([okSession()], [], 0, true));
    t.client.press();
    await settle();
    t.clock.advance(reportWait - 1);
    await settle();
    expect(t.client.snapshot().transmitting).toBe(false);
    t.clock.advance(1);
    await settle();
    expect(t.rooms[0]?.sending).toEqual([true]);
    expect(t.client.snapshot().transmitting).toBe(true);
  });

  it('an unmute that lands after reportWait is still said, once it lands', async () => {
    const t = await joined();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.holdUnmute = true;
    t.client.press();
    await settle();
    t.clock.advance(reportWait + 500);
    await settle();
    expect(t.client.snapshot()).toMatchObject({ talking: true, transmitting: false });
    room.heldUnmute?.();
    await settle();
    expect(t.client.snapshot()).toMatchObject({ talking: true, transmitting: true });
  });

  it.each([
    ['a release', (t: Awaited<ReturnType<typeof joined>>) => {
      t.client.release();
      t.clock.advance(defaultTail);
    }],
    ['a mute', (t: Awaited<ReturnType<typeof joined>>) => {
      t.client.setMuted(true);
    }],
    ['a loss of focus', (t: Awaited<ReturnType<typeof joined>>) => {
      t.client.lost();
    }],
    ['a drop', (t: Awaited<ReturnType<typeof joined>>) => {
      t.rooms[0]?.events?.onDisconnected('room_deleted');
    }],
  ])('a late unmute after %s is never said to transmit', async (_what, end) => {
    const t = await joined(setup([okSession(), okSession()]));
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.holdUnmute = true;
    const said: boolean[] = [];
    t.client.subscribe(() => {
      said.push(t.client.snapshot().transmitting);
    });
    t.client.press();
    await settle();
    t.clock.advance(reportWait + 500);
    await settle();
    end(t);
    await settle();
    room.heldUnmute?.();
    await settle();
    expect(said).not.toContain(true);
    expect(t.client.snapshot().transmitting).toBe(false);
  });

  it('a drop after the unmute ends transmitting with the connection', async () => {
    const t = await joined(setup([okSession(), okSession()]));
    t.client.press();
    await settle();
    expect(t.client.snapshot().transmitting).toBe(true);
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    expect(t.client.snapshot()).toMatchObject({ talking: false, transmitting: false });
  });

  it('the release delay is the default until set; set, it holds from the next release, between 0 and 2 s', async () => {
    const t = await joined();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    expect(t.client.snapshot().releaseDelay).toBe(defaultTail);
    t.client.setReleaseDelay(600);
    expect(t.client.snapshot().releaseDelay).toBe(600);
    t.client.press();
    await settle();
    t.client.release();
    t.clock.advance(599);
    expect(room.sending).toEqual([true]);
    t.clock.advance(1);
    expect(room.sending).toEqual([true, false]);
    await settle();
    expect(t.reports).toEqual(['started', 'stopped']);
    for (const [asked, kept] of [[-5, 0], [5000, 2000], [Number.NaN, defaultTail], [123.4, 123]] as const) {
      t.client.setReleaseDelay(asked);
      expect(t.client.snapshot().releaseDelay).toBe(kept);
    }
  });

  it('a release delay handed in at the start holds from the first release', async () => {
    const clock = new FakeClock();
    const rooms: StubRoom[] = [];
    const client = new VoiceClient({
      clock,
      mode: pushToTalk,
      random: () => 0.5,
      createSession: () => Promise.resolve(okSession()),
      report: () => Promise.resolve('ok'),
      newRoom: () => {
        const r = new StubRoom();
        rooms.push(r);
        return r;
      },
      claimShare: () => Promise.resolve(claim),
      stopShare: () => Promise.resolve(),
      gate: { tail: 40 },
    });
    expect(client.snapshot().releaseDelay).toBe(40);
    client.join('general');
    await settle();
    client.press();
    await settle();
    client.release();
    clock.advance(40);
    expect(rooms[0]?.sending).toEqual([true, false]);
  });

  it('the open microphone opens within one 10 ms read of an onset', async () => {
    const t = await joined();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    t.client.setMode('open');
    // The level is read every 10 ms from the connection; the onset lands
    // just after a read, and is heard by the next. Until issue #965 it was
    // read every 50 ms, and this onset waited 49 ms.
    t.clock.advance(1);
    room.levelNow = 0.5;
    t.clock.advance(9);
    await settle();
    expect(room.sending).toEqual([true]);
    expect(t.reports).toEqual(['started']);
  });

  it('the meter is told at most every meterEvery ms, however often the level is read', async () => {
    const t = await joined();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    let renders = 0;
    t.client.subscribe(() => {
      renders++;
    });
    for (let ms = 0; ms < 1000; ms += levelEvery) {
      room.levelNow = (ms / levelEvery) % 2 === 0 ? 0.3 : 0.6;
      t.clock.advance(levelEvery);
    }
    expect(renders).toBeLessThanOrEqual(1000 / meterEvery);
    expect(t.client.snapshot().level).toBeGreaterThan(0);
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
    expect(t.client.snapshot().talking).toBe(true);
    r.holdSwitch = true;
    const switching = t.client.chooseMicrophone(headset);
    expect(t.client.snapshot().transmitting).toBe(false);
    t.clock.advance(50);
    expect(t.client.snapshot().transmitting).toBe(false);
    r.heldSwitch?.();
    await switching;
    t.clock.advance(50);
    expect(t.client.snapshot().talking).toBe(true);
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
    expect(t.client.snapshot().talking).toBe(true);
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
    expect(t.client.snapshot().talking).toBe(true);
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
  function slow(remembered?: Microphone) {
    let connected = (): void => undefined;
    const t = setup([okSession(), okSession()], [], 0, false, (r, i) => {
      if (i > 0) return;
      const connect = r.connect;
      r.connect = (...args) =>
        new Promise<void>((resolve) => {
          connected = () => {
            resolve(connect(...args));
          };
        });
    }, remembered);
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

  it.each([
    ['the system default', undefined],
    ['a remembered device', { id: 'unplugged', label: 'Old headset' }],
  ])('keeps the newer choice when %s, which the room was given, is gone', async (_name, given) => {
    const { t, connect } = slow(given);
    t.client.join('general');
    await settle();
    const r = t.rooms[0];
    if (r === undefined) throw new Error('no room');
    await t.client.chooseMicrophone(webcam);
    r.connectFailure = deviceError('NotFoundError');
    connect();
    await settle();
    expect(t.client.snapshot()).toMatchObject({ phase: 'stopped', why: earlierGone, microphone: webcam });
    expect(t.clock.pending).toBe(0);
    // The next Join voice captures the newer choice.
    t.client.join('general');
    await settle();
    expect(t.rooms[1]?.choices).toEqual(['mic-2']);
  });

  it('lets the device go when it is the one given and still the choice', async () => {
    const { t, connect } = slow({ id: 'unplugged', label: 'Old headset' });
    t.client.join('general');
    await settle();
    const r = t.rooms[0];
    if (r === undefined) throw new Error('no room');
    r.connectFailure = deviceError('NotFoundError');
    connect();
    await settle();
    expect(t.client.snapshot()).toMatchObject({ phase: 'stopped', why: chosenGone, microphone: null });
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

// Issue #728: the channel voice is in is renamed. The room and the presence
// socket are the channel's by id, so nothing is left or reconnected; what is
// asked of conchd afterwards names the new name.
describe('a rename of the channel joined', () => {
  it('keeps the room and the connection, and names the new name in the next report, session request and share', async () => {
    const t = await joined();
    t.client.renamed('general', 'lounge');
    expect(t.client.snapshot().phase).toBe('connected');
    expect(t.client.snapshot().channel).toBe('lounge');
    expect(t.rooms).toHaveLength(1);
    expect(t.rooms[0]?.closed).toBe(false);
    expect(t.asked).toEqual(['general']);

    t.client.press();
    await settle();
    t.client.release();
    t.clock.advance(defaultTail);
    await settle();
    expect(t.reports).toEqual(['started', 'stopped']);
    expect(t.reportedIn).toEqual(['lounge', 'lounge']);

    t.client.startShare();
    await settle();
    expect(t.shares[0]).toBe('claim lounge');
    t.client.stopShare();
    await settle();
    expect(t.shares).toContain('stop lounge');
    expect(t.shares.filter((s) => s.endsWith('general'))).toEqual([]);

    // The room renewed: the client goes back to conchd, by the new name.
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.asked).toEqual(['general', 'lounge']);
    expect(t.client.snapshot().phase).toBe('connected');
    expect(t.client.snapshot().channel).toBe('lounge');
  });

  it('is told by the name joined: another channel renamed, or the same name again, changes nothing', async () => {
    const t = await joined();
    const before = t.client.snapshot();
    t.client.renamed('ops', 'ops-old');
    t.client.renamed('general', 'general');
    expect(t.client.snapshot()).toBe(before);
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.asked).toEqual(['general', 'general']);
  });

  it('while not in voice changes nothing, and the next join names what it is given', async () => {
    const t = setup();
    t.client.renamed('general', 'lounge');
    expect(t.client.snapshot().channel).toBeNull();
    t.client.join('lounge');
    await settle();
    expect(t.asked).toEqual(['lounge']);
  });
});

// Issue #683: a deafened page deafens each new room before it connects, so a
// microphone attached while the room is connecting is muted from the first.
describe('deafen on a new room', () => {
  it('is handed to the room before it connects: the first room, a reconnect, a renewed room, and Leave then Join', async () => {
    const t = setup();
    t.client.setDeafened(true);
    t.client.join('general');
    await settle();
    t.rooms[0]?.events?.onDisconnected('lost');
    t.clock.advance(1000);
    await settle();
    t.rooms[1]?.events?.onDisconnected('room_deleted');
    await settle();
    await t.client.leave();
    t.client.join('general');
    await settle();
    expect(t.rooms).toHaveLength(4);
    for (const room of t.rooms) {
      expect(room.deafened[0]).toBe(true);
      expect(room.deafenedWhen[0]).toBe(false);
    }
    expect(t.client.snapshot().phase).toBe('connected');
  });

  it('undeafen still reaches the connected room', async () => {
    const t = setup();
    t.client.setDeafened(true);
    t.client.join('general');
    await settle();
    t.client.setDeafened(false);
    expect(t.rooms[0]?.deafened.at(-1)).toBe(false);
  });
});

// ---- the camera through the client (ADR-015, design §6.2, §7, §11.5)

const cameraStarted = '2026-10-16T00:00:01.000Z';
const named = `?started_at=${cameraStarted}`;

// Presence with this person (7) and another (8) in the room, whoever holds a
// camera claim, and why this person's last one ended.
const cameraPresence = (cameras: readonly number[], ended?: VoicePresenceV1['camera_ended']): VoicePresenceV1 => ({
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
        ...(cameras.includes(id) ? { camera: true } : {}),
      })),
    },
  ],
  ...(ended === undefined ? {} : { camera_ended: ended }),
});

async function cameraOn(t = setup()) {
  await joined(t);
  await t.client.startCamera();
  await settle();
  return t;
}

describe('the camera through the client', () => {
  it('joining, unmuting, deafening and sharing never call the camera', async () => {
    const t = await joined();
    t.client.setMuted(true);
    t.client.setMuted(false);
    t.client.setDeafened(true);
    t.client.setDeafened(false);
    t.client.startShare();
    await settle();
    t.client.stopShare();
    await settle();
    expect(t.camLog.filter((l) => !l.startsWith('session'))).toEqual([]);
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: null });
  });

  it('the button captures, claims un-named, waits for the grant and publishes; the call view opens', async () => {
    const t = await cameraOn();
    expect(t.camLog).toEqual(['session general', 'capture browser', 'POST general', 'grant', 'publish camera']);
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'sending' }, cameraTrack: 'published', viewing: true });
    expect(t.rooms[0]?.viewings).toEqual([false, true]);
  });

  it('is not offered in Watch only or on a grant that cannot publish (O14)', async () => {
    const w = setup();
    w.client.join('general', { watchOnly: true });
    await settle();
    await w.client.startCamera();
    const l = await joined(setup([okSession(false)]));
    await l.client.startCamera();
    for (const t of [w, l]) expect(t.camLog.filter((x) => !x.startsWith('session'))).toEqual([]);
  });

  it('"Sending video" and "Camera in use" come from the local track, never from presence (S5)', async () => {
    const t = await joined();
    // Presence says this person holds a camera claim (another device's, or one
    // displaced): this page sends nothing, and says so.
    t.client.presence(cameraPresence([7, 8]));
    expect(t.client.snapshot().cameraTrack).toBe('none');
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.camState = 'captured';
    room.events?.onCameraChanged();
    expect(t.client.snapshot().cameraTrack).toBe('captured');
    room.camState = 'none';
    room.events?.onCameraChanged();
    t.client.presence(cameraPresence([7]));
    expect(t.client.snapshot().cameraTrack).toBe('none');
  });

  it('receives only the cameras of others presence shows holding a claim', async () => {
    const t = await joined();
    t.client.presence(cameraPresence([7, 8]));
    expect(t.rooms[0]?.claimedSets.at(-1)).toEqual([8]);
    t.client.presence(cameraPresence([]));
    expect(t.rooms[0]?.claimedSets.at(-1)).toEqual([]);
  });

  it('Show video and Hide video subscribe and let go; nothing opens the view by itself (O8)', async () => {
    const t = await joined();
    t.client.presence(cameraPresence([8]));
    expect(t.client.snapshot().viewing).toBe(false);
    t.client.viewVideo(true);
    t.client.viewVideo(false);
    expect(t.rooms[0]?.viewings).toEqual([false, true, false]);
  });

  it('mute, deafen and stopping a share leave the camera as it is (§7.6)', async () => {
    const t = await cameraOn();
    t.client.setMuted(true);
    t.client.setDeafened(true);
    t.client.startShare();
    await settle();
    t.client.stopShare();
    await settle();
    expect(t.camLog).not.toContain('stop camera');
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'sending' }, cameraTrack: 'published' });
  });

  it('the button, off: the capture stops, then DELETE names the claim', async () => {
    const t = await cameraOn();
    t.camLog.length = 0;
    await t.client.stopCamera();
    expect(t.camLog).toEqual(['stop camera', `DELETE general ${named}`]);
    expect(t.client.snapshot().cameraTrack).toBe('none');
  });

  it('leaving (and so signing out) stops the capture and releases the claim before the room is closed', async () => {
    const t = await cameraOn();
    t.camLog.length = 0;
    await t.client.leave();
    expect(t.camLog).toEqual(['stop camera', `DELETE general ${named}`, 'close']);
    expect(t.client.snapshot()).toMatchObject({ viewing: false, cameraTrack: 'none' });
  });

  it('a change of channel releases the camera before the next session is asked for', async () => {
    const t = await cameraOn();
    t.camLog.length = 0;
    await t.client.leave();
    t.client.join('random');
    await settle();
    expect(t.camLog).toEqual(['stop camera', `DELETE general ${named}`, 'close', 'session random']);
    // Off on joining: nothing is captured or claimed in the new channel.
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: null });
  });

  it('a signed-out report stops and releases the camera with the connection', async () => {
    const t = setup([okSession()], ['unauthorized']);
    await cameraOn(t);
    t.camLog.length = 0;
    t.client.press();
    await settle();
    t.clock.advance(reportWait);
    await settle();
    expect(t.camLog.slice(0, 2)).toEqual(['stop camera', 'close']);
    expect(t.camLog).toContain(`DELETE general ${named}`);
    expect(t.client.snapshot().phase).toBe('stopped');
  });

  it.each([
    ['duplicate_identity', false],
    ['participant_removed', true],
  ] as const)('%s stops the capture before anything else (S5); released by name: %s', async (cause, released) => {
    const t = setup([okSession(), okSession()]);
    await cameraOn(t);
    t.camLog.length = 0;
    t.rooms[0]?.events?.onDisconnected(cause);
    await settle();
    t.clock.advance(1000);
    await settle();
    expect(t.camLog.slice(0, 2)).toEqual(['stop camera', 'close']);
    // Displaced, the claim is the other device's now, and ends idle there.
    expect(t.camLog.includes(`DELETE general ${named}`)).toBe(released);
    // The capture is not carried to the next connection, if there is one.
    expect(t.camLog.filter((l) => l.startsWith('POST') || l.startsWith('adopt'))).toEqual([]);
    expect(t.client.snapshot().cameraTrack).toBe('none');
  });

  it('a deleted room is the renewal once presence says rotated: the next connection adopts the capture with a POST naming the ended claim', async () => {
    const adopted: CameraClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:40.000Z' } };
    const t = setup([okSession(), okSession()], [], 0, false, undefined, undefined, undefined, undefined, { answers: [cameraGranted, adopted] });
    await cameraOn(t);
    t.camLog.length = 0;
    t.client.presence(cameraPresence([], { reason: 'rotated', started_at: cameraStarted, ended_at: '2026-10-16T00:00:30.000Z' }));
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.camLog).toEqual(['keep camera', 'close', 'session general', `POST general ${named}`, 'adopt camera', 'grant', 'publish camera']);
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'sending' }, cameraTrack: 'published' });
  });

  it('a deleted room presence never says was renewed is stopped and released (S5)', async () => {
    const t = setup([okSession(), okSession()]);
    await cameraOn(t);
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    t.clock.advance(5000);
    await settle();
    expect(t.camLog).toContain('stop kept camera');
    expect(t.camLog).toContain(`DELETE general ${named}`);
    expect(t.camLog.filter((l) => l.startsWith('POST'))).toEqual(['POST general']);
    expect(t.client.snapshot().cameraTrack).toBe('none');
  });

  it('a refused next session stops and releases a capture held for it (S5)', async () => {
    const t = setup([okSession(), { ok: false, failure: 'invalid', status: 404, code: 'channel_not_found' }]);
    await cameraOn(t);
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    t.client.presence(cameraPresence([], { reason: 'rotated', started_at: cameraStarted, ended_at: '2026-10-16T00:00:30.000Z' }));
    await settle();
    expect(t.camLog).toContain('stop kept camera');
    expect(t.camLog.filter((l) => l.startsWith('POST'))).toEqual(['POST general']);
    expect(t.client.snapshot()).toMatchObject({ phase: 'stopped', cameraTrack: 'none' });
  });

  it('a full reconnect while holding the capture names the claim; stale stops it and says so', async () => {
    const stale: CameraClaimResult = { ok: false, failure: 'invalid', status: 409, code: 'voice_camera_stale' };
    const t = setup([okSession(), okSession()], [], 0, false, undefined, undefined, undefined, undefined, { answers: [cameraGranted, stale] });
    await cameraOn(t);
    t.rooms[0]?.events?.onDisconnected('lost');
    await settle();
    t.clock.advance(2000);
    await settle();
    expect(t.camLog.filter((l) => l.startsWith('POST'))).toEqual(['POST general', `POST general ${named}`]);
    expect(t.camLog).toContain('stop kept camera');
    expect(t.client.snapshot().camera).toEqual({ kind: 'failed', reason: 'stale' });
  });

  it("the SDK's reconnect names the claim", async () => {
    const t = await cameraOn();
    t.rooms[0]?.events?.onReconnecting();
    t.rooms[0]?.events?.onReconnected();
    await settle();
    expect(t.camLog.filter((l) => l.startsWith('POST'))).toEqual(['POST general', `POST general ${named}`]);
  });

  it('camera_ended for any reason but rotated stops the capture and claims nothing again', async () => {
    const t = setup([okSession(), okSession()]);
    await cameraOn(t);
    t.client.presence(cameraPresence([], { reason: 'operator', started_at: cameraStarted, ended_at: '2026-10-16T00:00:30.000Z' }));
    await settle();
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    await settle();
    expect(t.camLog.filter((l) => l.startsWith('POST'))).toEqual(['POST general']);
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: 'operator' });
  });

  it('the room renewed while sending: "Camera in use" for as long as the capture is held, never "none" (S5)', async () => {
    const t = setup([okSession(), okSession()]);
    await cameraOn(t);
    // Every state the page is told of, not only the last.
    const seen: string[] = [];
    t.client.subscribe(() => {
      seen.push(t.client.snapshot().cameraTrack);
    });
    t.client.presence(cameraPresence([], { reason: 'rotated', started_at: cameraStarted, ended_at: '2026-10-16T00:00:30.000Z' }));
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'resuming' }, cameraTrack: 'captured' });
    t.rooms[0]?.events?.onDisconnected('room_deleted');
    expect(t.client.snapshot().cameraTrack).toBe('captured');
    expect(seen.length).toBeGreaterThan(0);
    expect(seen.filter((s) => s === 'none')).toEqual([]);
  });

  it('the call view opens once the capture is given, and not for a camera refused (O8)', async () => {
    const t = setup([okSession()], [], 0, false, (room) => {
      room.captureError = new DOMException('no', 'NotAllowedError');
    });
    await cameraOn(t);
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'failed', reason: 'blocked' }, viewing: false });
    expect(t.rooms[0]?.viewings).toEqual([false]);
  });

  it('the microphone and the camera are independent: a microphone lost leaves the camera, and the camera lost leaves the microphone', async () => {
    const t = await cameraOn();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.events?.onMicrophoneLost();
    await settle();
    expect(t.client.snapshot()).toMatchObject({ missing: true, camera: { kind: 'sending' } });
    room.events?.onCameraEnded('device');
    await settle();
    expect(t.client.snapshot().camera).toEqual({ kind: 'failed', reason: 'disconnected' });
    expect(t.client.snapshot().phase).toBe('connected');
  });
});

describe('the camera chosen (N7)', () => {
  const desk: Camera = { id: 'old-id', label: 'Desk camera' };
  it('is found again by its name when exactly one camera carries it, and captured exactly', async () => {
    const t = setup([okSession()], [], 0, false, (room) => {
      room.cameraList = [{ id: 'new-id', label: 'Desk camera' }, { id: 'cam-2', label: 'Laptop camera' }];
    }, undefined, undefined, undefined, { choice: desk });
    await cameraOn(t);
    expect(t.camLog[1]).toBe('capture new-id');
    expect(t.client.snapshot().cameraChoice).toEqual({ id: 'new-id', label: 'Desk camera' });
  });

  it.each([
    ['two cameras of its name', [{ id: 'a', label: 'Desk camera' }, { id: 'b', label: 'Desk camera' }]],
    ['none', [{ id: 'default', label: 'Default camera' }, { id: 'cam-2', label: 'Laptop camera' }]],
  ])('with %s, is not connected: nothing is captured, never the default', async (_what, list) => {
    const t = setup([okSession()], [], 0, false, (room) => {
      room.cameraList = list;
    }, undefined, undefined, undefined, { choice: desk });
    await cameraOn(t);
    expect(t.camLog.filter((l) => !l.startsWith('session'))).toEqual([]);
    expect(t.client.snapshot().camera).toEqual({ kind: 'failed', reason: 'not_connected' });
  });

  it('a camera chosen while on that cannot be had stops and releases the camera, and takes no other', async () => {
    const t = await cameraOn();
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.switchError = new DOMException('gone', 'OverconstrainedError');
    await t.client.chooseCamera({ id: 'cam-2', label: 'Laptop camera' });
    await settle();
    expect(t.camLog.slice(-2)).toEqual(['switch cam-2', `DELETE general ${named}`]);
    expect(t.client.snapshot().camera).toEqual({ kind: 'failed', reason: 'not_connected' });
    expect(t.camLog.filter((l) => l.startsWith('capture'))).toEqual(['capture browser']);
  });

  it('a switch that fails after the camera was turned off and on again leaves the new camera alone', async () => {
    const t = setup([okSession()], [], 0, false, undefined, undefined, undefined, undefined, { answers: [cameraGranted, { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:09.000Z' } }] });
    await cameraOn(t);
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    room.holdSwitchCamera = true;
    const choosing = t.client.chooseCamera({ id: 'cam-2', label: 'Laptop camera' });
    await settle();
    await t.client.stopCamera();
    await t.client.startCamera();
    await settle();
    expect(t.client.snapshot().camera).toEqual({ kind: 'sending' });
    t.camLog.length = 0;
    room.failHeldSwitch?.(new DOMException('superseded', 'OverconstrainedError'));
    await choosing;
    await settle();
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'sending' }, cameraTrack: 'published' });
    expect(t.camLog).toEqual([]);
  });

  it('leaving forgets who holds a camera claim: the next connection receives nobody until presence says', async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    t.client.presence(cameraPresence([8]));
    await t.client.leave();
    t.client.join('general');
    await settle();
    expect(t.rooms[1]?.claimedSets[0]).toEqual([]);
  });

  it('a choice not connected is said through the camera, and joining again forgets it', async () => {
    const t = setup([okSession(), okSession()], [], 0, false, (room) => {
      room.cameraList = [{ id: 'cam-2', label: 'Laptop camera' }];
    }, undefined, undefined, undefined, { choice: desk });
    await cameraOn(t);
    expect(t.client.snapshot().camera).toEqual({ kind: 'failed', reason: 'not_connected' });
    await t.client.leave();
    t.client.join('random');
    await settle();
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: null });
  });

  it('is found again under a new id when the cameras are read: at a connection, and when devices change', async () => {
    let list: readonly Camera[] = [{ id: 'cam-2', label: 'Laptop camera' }];
    const t = setup([okSession()], [], 0, false, (room) => {
      room.cameras = () => Promise.resolve(list);
    }, undefined, undefined, undefined, { choice: desk });
    await joined(t);
    expect(t.client.snapshot().cameraChoice).toEqual(desk);
    expect(t.client.snapshot().cameras).toEqual(list);
    list = [...list, { id: 'plugged-in', label: 'Desk camera' }];
    t.rooms[0]?.events?.onDevicesChanged();
    await settle();
    expect(t.client.snapshot().cameras).toEqual(list);
    expect(t.client.snapshot().cameraChoice).toEqual({ id: 'plugged-in', label: 'Desk camera' });
  });

  it('cameraToUse finds the choice again before a test capture, and says false when it is not connected', async () => {
    const t = setup([okSession()], [], 0, false, (room) => {
      room.cameraList = [{ id: 'renamed', label: 'Desk camera' }];
    }, undefined, undefined, undefined, { choice: desk });
    await joined(t);
    expect(await t.client.cameraToUse()).toBe('renamed');
    const u = setup([okSession()], [], 0, false, (room) => {
      room.cameraList = [];
    }, undefined, undefined, undefined, { choice: desk });
    await joined(u);
    expect(await u.client.cameraToUse()).toBe(false);
  });

  it('a camera chosen before the camera is published is switched to once, by the start itself, before the publish', async () => {
    const t = setup([okSession(), okSession()]);
    await cameraOn(t);
    t.rooms[0]?.events?.onDisconnected('lost');
    await settle();
    expect(t.client.snapshot().camera).toEqual({ kind: 'resuming' });
    await t.client.chooseCamera({ id: 'cam-2', label: 'Laptop camera' });
    // Not switched here: nothing is captured on any room now.
    expect(t.camLog.filter((l) => l.startsWith('switch'))).toEqual([]);
    t.clock.advance(2000);
    await settle();
    const after = t.camLog.slice(t.camLog.indexOf('adopt camera'));
    expect(after).toEqual(['adopt camera', 'switch cam-2', 'grant', 'publish camera']);
  });

  it('a camera chosen while on moves the capture to it, exactly', async () => {
    const t = await cameraOn();
    await t.client.chooseCamera({ id: 'cam-2', label: 'Laptop camera' });
    expect(t.camLog.at(-1)).toBe('switch cam-2');
    expect(t.client.snapshot().cameraChoice).toEqual({ id: 'cam-2', label: 'Laptop camera' });
  });
});

// A hidden page on a phone (design §8, ADR-015 O9): the page calls pageHidden
// (useVoice.ts decides when), and the camera ends as the button ends it, from
// every step of its start.
describe('the page hidden on a phone', () => {
  type Step = 'capturing' | 'claiming' | 'granted-waiting' | 'publishing' | 'sending';
  const holds = { capturing: 'capture', 'granted-waiting': 'grant', publishing: 'publish' } as const;

  // Starts the camera and stops it at the step, with that step under way.
  async function reach(step: Step) {
    const t = setup(undefined, undefined, 0, false, undefined, undefined, undefined, undefined, { holdClaims: step === 'claiming' });
    await joined(t);
    const room = t.rooms[0];
    if (room === undefined) throw new Error('no room');
    if (step !== 'claiming' && step !== 'sending') room.hold = holds[step];
    void t.client.startCamera('phone');
    await settle();
    expect(t.client.snapshot().camera.kind).toBe(step);
    return { t, room };
  }

  it.each(['capturing', 'claiming', 'granted-waiting', 'publishing', 'sending'] as const)('ends the start or the camera from %s: stopped, nothing published, the claim released by name', async (step) => {
    const { t, room } = await reach(step);
    const published = t.camLog.filter((l) => l === 'publish camera').length;
    t.client.pageHidden();
    // What was under way still comes, and finds the start over.
    room.letGo();
    t.claimsWaiting.shift()?.();
    await settle();
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'off', ended: 'self' }, cameraTrack: 'none' });
    expect(room.cameraTrack()).toBe('none');
    expect(t.camLog.filter((l) => l === 'publish camera')).toHaveLength(published);
    const released = t.camLog.filter((l) => l.startsWith('DELETE'));
    // A start still asking the browser has claimed nothing: nothing to name.
    expect(released).toEqual(step === 'capturing' ? [] : [`DELETE general ${named}`]);
    expect(t.camLog.filter((l) => l.startsWith('POST'))).toHaveLength(step === 'capturing' ? 0 : 1);
    // Once the stop is over there is nothing left to stop or restart.
    t.camLog.length = 0;
    await settle();
    expect(t.camLog).toEqual([]);
  });

  it('a stop from sending is the button\'s: the capture stops, then DELETE names the claim', async () => {
    const { t } = await reach('sending');
    t.camLog.length = 0;
    t.client.pageHidden();
    await settle();
    expect(t.camLog).toEqual(['stop camera', `DELETE general ${named}`]);
  });

  it('showing the page again starts nothing: only the button does', async () => {
    const { t } = await reach('sending');
    t.client.pageHidden();
    await settle();
    t.camLog.length = 0;
    t.client.viewVideo(true);
    await settle();
    t.clock.advance(60_000);
    await settle();
    expect(t.camLog).toEqual([]);
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: 'self' });
  });

  it('with the camera off, does nothing', async () => {
    const t = await joined();
    t.client.pageHidden();
    await settle();
    expect(t.camLog.filter((l) => !l.startsWith('session'))).toEqual([]);
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: null });
  });

  it('a start that was still choosing its camera when the page was hidden captures nothing', async () => {
    const t = setup(undefined, undefined, 0, false, undefined, undefined, undefined, undefined, { choice: { id: 'cam-2', label: 'Laptop camera' } });
    await joined(t);
    // The camera chosen is looked for first (cameraToUse); hidden meanwhile.
    const starting = t.client.startCamera('phone');
    t.client.pageHidden();
    await starting;
    await settle();
    expect(t.camLog.filter((l) => !l.startsWith('session'))).toEqual([]);
    expect(t.client.snapshot().camera).toEqual({ kind: 'off', ended: null });
    // The button, pressed again, starts.
    await t.client.startCamera('phone');
    await settle();
    expect(t.client.snapshot().camera.kind).toBe('sending');
  });

  it('the camera starts with the profile the button gave, and the desktop\'s without one', async () => {
    const t = await joined();
    await t.client.startCamera('phone');
    await settle();
    expect(t.rooms[0]?.profiles).toEqual(['phone']);
    await t.client.stopCamera();
    await t.client.startCamera();
    await settle();
    expect(t.rooms[0]?.profiles).toEqual(['phone', 'desktop']);
  });
});

describe('the video shown here', () => {
  it('is told to every room, and to the next connection as it was left', async () => {
    const t = setup([okSession(), okSession()]);
    await joined(t);
    expect(t.rooms[0]?.shownSets).toEqual([true]);
    t.client.showVideo(false);
    expect(t.rooms[0]?.shownSets).toEqual([true, false]);
    // The person's choice to view is untouched by it.
    t.client.viewVideo(true);
    expect(t.client.snapshot().viewing).toBe(true);
    t.rooms[0]?.events?.onDisconnected('lost');
    await settle();
    t.clock.advance(60_000);
    await settle();
    expect(t.rooms[1]?.shownSets[0]).toBe(false);
    t.client.showVideo(true);
    expect(t.rooms[1]?.shownSets.at(-1)).toBe(true);
    expect(t.client.snapshot().viewing).toBe(true);
  });

  it('leaves the person\'s own camera sending', async () => {
    const t = await cameraOn();
    t.camLog.length = 0;
    t.client.showVideo(false);
    await settle();
    expect(t.camLog).toEqual([]);
    expect(t.client.snapshot()).toMatchObject({ camera: { kind: 'sending' }, cameraTrack: 'published' });
  });
});
