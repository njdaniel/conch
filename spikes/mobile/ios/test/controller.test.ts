/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import type { CameraClaimResult } from '../src/logic/api';
import type { CameraRoom, KeptCamera } from '../src/logic/camera';
import type { CameraWants } from '../src/app/camera/policy';
import { VoiceController, type NativePort, type RoomEvents, type RoomPort, type VoiceApi } from '../src/app/controller';
import { appStateOf, deliver } from '../src/app/bridge';
import type { InterruptionEvent, RouteChangeEvent } from '../src/logic/ios';
import { defaultTail, phoneMax } from '../src/logic/gate';
import { reportWait } from '../src/logic/mic';
import type { VoicePresenceV1, VoiceSessionResponseV1, VoiceTransmitState } from '../src/logic/schema/generated';
import { tapAgain } from '../src/app/camera/controls';
import { FakeClock, settle } from './fakes';

const me = 7;

function presence(ids: number[], options: { available?: boolean } = {}): VoicePresenceV1 {
  return {
    schema: 'conch.voice_presence.v1',
    channel_id: 1,
    configured: true,
    available: options.available ?? true,
    rooms: [{ participants: ids.map((id) => ({ principal_id: id, can_publish: true, transmitting: false, joined_at: '2026-11-05T10:00:00Z' })) }],
  };
}

function session(canPublish = true, url = 'x'): VoiceSessionResponseV1 {
  return { livekit_url: url, identity: `p${me}`, rooms: [{ room: 'r', token: 't', can_publish: canPublish, expires_at: '2026-11-05T10:00:15Z' }] };
}

const startedAt = '2026-11-05T10:00:00Z';

class FakeCamera implements CameraRoom {
  captured = false;
  grantError: Error | null = null;
  constructor(private readonly log: string[]) {}
  captureCamera(_choice: string | null, profile?: string): Promise<void> {
    this.log.push(`capture ${profile ?? ''}`);
    this.captured = true;
    return Promise.resolve();
  }
  awaitCameraGrant(): Promise<void> {
    this.log.push('camera grant');
    return this.grantError === null ? Promise.resolve() : Promise.reject(this.grantError);
  }
  publishCamera(): Promise<void> {
    this.log.push('camera publish');
    return Promise.resolve();
  }
  stopCamera(): Promise<void> {
    this.log.push('camera stop');
    this.captured = false;
    return Promise.resolve();
  }
  keepCamera(): KeptCamera | null {
    this.log.push('camera keep');
    return null;
  }
  adoptCamera(): void {
    this.log.push('camera adopt');
  }
  cameraInUse(): string | null {
    return this.captured ? 'front' : null;
  }
  switchCamera(): Promise<void> {
    return Promise.resolve();
  }
}

class FakeRoom implements RoomPort {
  events: RoomEvents | null = null;
  readonly camera: FakeCamera;
  wants: CameraWants[] = [];
  constructor(private readonly log: string[]) {
    this.camera = new FakeCamera(log);
  }
  watchCameras(wants: CameraWants): void {
    this.wants.push(wants);
  }
  connect(_url: string, _token: string, publish: boolean, events: RoomEvents): Promise<void> {
    this.events = events;
    this.log.push(`room connect publish=${publish}`);
    return Promise.resolve();
  }
  setSending(on: boolean): Promise<void> {
    this.log.push(on ? 'unmute' : 'mute');
    return Promise.resolve();
  }
  setDeafened(on: boolean): void {
    this.log.push(`deafen ${on}`);
  }
  disconnect(): Promise<void> {
    this.log.push('room disconnect');
    return Promise.resolve();
  }
}

interface Rig {
  c: VoiceController;
  clock: FakeClock;
  log: string[];
  rooms: FakeRoom[];
  reports: VoiceTransmitState[];
  claims: CameraClaimResult[];
}

function rig(
  options: {
    ids?: number[];
    mic?: 'granted' | 'microphone_denied';
    session?: boolean;
    canPublish?: boolean;
    // Who is in each channel's room, and the channels presence was read for.
    idsByChannel?: Record<string, number[]>;
    asked?: string[];
    livekitUrl?: string;
    held?: string[];
    camera?: boolean;
    cameraPeers?: number[];
    cameraEnded?: { reason: 'operator'; started_at: string; ended_at: string };
    prompt?: (clock: FakeClock, c: () => VoiceController) => Promise<boolean>;
  } = {},
): Rig {
  const clock = new FakeClock();
  const log: string[] = [];
  const rooms: FakeRoom[] = [];
  const reports: VoiceTransmitState[] = [];
  const claims: CameraClaimResult[] = [];
  let self: VoiceController | null = null;
  const api: VoiceApi = {
    presence: (channel) => {
      log.push('presence');
      options.asked?.push(channel);
      const value = presence(options.idsByChannel?.[channel] ?? options.ids ?? []);
      const rooms = value.rooms.map((r) => ({ ...r, participants: r.participants.map((p) => ({ ...p, ...(options.cameraPeers?.includes(p.principal_id) === true ? { camera: true } : {}) })) }));
      return Promise.resolve({ ok: true, value: { ...value, rooms, ...(options.cameraEnded === undefined ? {} : { camera_ended: options.cameraEnded }) } } as const);
    },
    claimCamera: (channel, named) => {
      log.push(`claim ${channel} ${named ?? 'new'}`);
      return Promise.resolve(claims.shift() ?? ({ ok: true, value: { principal_id: me, started_at: startedAt } } as const));
    },
    releaseCamera: (channel, named) => {
      log.push(`release ${channel} ${named}`);
      return Promise.resolve(null);
    },
    voiceSession: () => (log.push('session'), Promise.resolve({ ok: true, value: session(options.canPublish ?? true, options.livekitUrl) } as const)),
    reportTransmit: (_c, state) => {
      log.push(`report ${state}`);
      reports.push(state);
      return Promise.resolve('ok');
    },
  };
  const native: NativePort = {
    ensurePermissions: () => (log.push('permissions'), Promise.resolve(options.mic ?? 'granted')),
    ensureCameraPermission: () => {
      log.push('camera permission');
      if (options.prompt !== undefined) return options.prompt(clock, () => self as VoiceController);
      return Promise.resolve(options.camera !== false);
    },
    startSession: () => (log.push('observers start'), options.session === false ? Promise.reject(new Error('AVAudioSessionErrorCodeCannotStartPlaying')) : Promise.resolve()),
    stopSession: () => (log.push('observers stop'), Promise.resolve()),
  };
  const c = new VoiceController({
    api,
    native,
    newRoom: () => {
      const r = new FakeRoom(log);
      rooms.push(r);
      return r;
    },
    clock,
    random: () => 0.5,
    me: () => me,
    log: (m) => log.push(`log ${m}`),
    now: () => clock.now,
    hold: (...secrets) => options.held?.push(...secrets),
  });
  self = c;
  return { c, clock, log, rooms, reports, claims };
}

const steps = (log: string[]) => log.filter((l) => !l.startsWith('log '));

async function joined(r: Rig): Promise<void> {
  await r.c.join('general');
  await settle();
  assert.equal(r.c.state.phase, 'connected');
}

test('join: presence, permission, observers, voice session, room, in that order; nothing is sent', async () => {
  const r = rig();
  await joined(r);
  // (Then the deafen state is applied and presence is polled.)
  assert.deepEqual(steps(r.log).slice(0, 5), ['presence', 'permissions', 'observers start', 'session', 'room connect publish=true']);
  assert.equal(r.c.gate.open, false);
  assert.equal(r.c.state.transmitting, false);
  assert.ok(!r.log.includes('unmute'), 'joining unmutes nothing');
  assert.deepEqual(r.reports, [], 'and reports nothing');
});

test('hand-off: the same principal in the room warns first, and joins only on confirm', async () => {
  const r = rig({ ids: [3, me] });
  await r.c.join('general');
  await settle();
  assert.equal(r.c.state.phase, 'confirm');
  assert.match(r.c.state.warning ?? '', /another device/);
  assert.deepEqual(steps(r.log), ['presence'], 'no observers, no voice session before the person agrees');
  await r.c.confirmJoin();
  await settle();
  assert.equal(r.c.state.phase, 'connected');
  assert.ok(r.log.includes('observers start'));
});

test('hand-off: cancelling the warning joins nothing', async () => {
  const r = rig({ ids: [me] });
  await r.c.join('general');
  r.c.cancelJoin();
  assert.equal(r.c.state.phase, 'idle');
  assert.ok(!r.log.includes('observers start'));
});

test('no warning when only other people are in the room', async () => {
  const r = rig({ ids: [3, 4] });
  await joined(r);
  assert.equal(r.c.state.warning, null);
});

test('microphone permission denied fails closed: no observers, no voice session (M-P1)', async () => {
  const r = rig({ mic: 'microphone_denied' });
  await r.c.join('general');
  await settle();
  assert.equal(r.c.state.phase, 'idle');
  assert.match(r.c.state.why ?? '', /microphone/i);
  assert.deepEqual(steps(r.log), ['presence', 'permissions']);
});

test('a refused observer start fails the join by name, with no voice session', async () => {
  const r = rig({ session: false });
  await r.c.join('general');
  await settle();
  assert.equal(r.c.state.phase, 'idle');
  assert.match(r.c.state.why ?? '', /AVAudioSessionErrorCodeCannotStartPlaying/);
  assert.ok(!r.log.includes('session'));
});

test('hold to talk: started is reported before the unmute; release mutes after the tail, then stopped', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  await settle();
  assert.equal(r.c.state.gateOpen, true);
  assert.deepEqual(r.reports, ['started']);
  assert.ok(r.log.includes('unmute'));
  assert.ok(r.log.indexOf('report started') < r.log.indexOf('unmute'));
  r.c.release();
  r.clock.advance(defaultTail);
  await settle();
  assert.ok(r.log.indexOf('mute') > r.log.indexOf('unmute'));
  assert.deepEqual(r.reports, ['started', 'stopped']);
  assert.ok(r.log.lastIndexOf('mute') < r.log.indexOf('report stopped'), 'stopped follows the mute');
});

test('a press past the bound shuts the gate and says so (M-F6)', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  await settle();
  r.clock.advance(phoneMax);
  await settle();
  assert.equal(r.c.state.gateOpen, false);
  assert.match(r.c.state.note ?? '', /30 s/);
  assert.deepEqual(r.reports, ['started', 'stopped']);
});

test('mute, deafen and listen-only keep the gate shut', async () => {
  const r = rig();
  await joined(r);
  r.c.setMuted(true);
  r.c.press();
  assert.equal(r.c.gate.open, false);
  r.c.setMuted(false);
  r.c.setDeafened(true);
  r.c.press();
  assert.equal(r.c.gate.open, false);
  assert.ok(r.log.includes('deafen true'));

  const listen = rig({ canPublish: false });
  await joined(listen);
  assert.equal(listen.c.state.listenOnly, true);
  listen.c.press();
  assert.equal(listen.c.gate.open, false);
  assert.ok(listen.log.includes('room connect publish=false'));
});

test('lifecycle: background, inactive and interruption each shut a held press at once, and resume opens nothing', async () => {
  const cases: { name: string; start: (c: VoiceController) => void; end: (c: VoiceController) => void }[] = [
    { name: 'background', start: (c) => c.appState('background'), end: (c) => c.appState('active') },
    { name: 'inactive', start: (c) => c.appState('inactive'), end: (c) => c.appState('active') },
    { name: 'interruption', start: (c) => c.interruption({ type: 'began' }), end: (c) => c.interruption({ type: 'ended', shouldResume: true }) },
  ];
  for (const t of cases) {
    const r = rig();
    await joined(r);
    r.c.press();
    await settle();
    assert.equal(r.c.gate.open, true, t.name);
    t.start(r.c);
    assert.equal(r.c.gate.open, false, `${t.name}: shut at once, with no tail`);
    await settle();
    assert.deepEqual(r.reports, ['started', 'stopped'], t.name);
    r.c.press();
    assert.equal(r.c.gate.open, false, `${t.name}: a press while blocked is ignored`);
    t.end(r.c);
    assert.equal(r.c.gate.open, false, `${t.name}: resume opens nothing`);
    assert.deepEqual(r.c.state.blocked, [], t.name);
    r.c.press();
    assert.equal(r.c.gate.open, true, `${t.name}: a new press opens it`);
  }
});

test('open mode: a sustained sound across a resume opens nothing until quiet and a fresh onset', async () => {
  const r = rig();
  await joined(r);
  r.c.setMode('open');
  r.c.level(0.9); // sound while latched
  r.clock.advance(1000);
  assert.equal(r.c.gate.open, false);
  r.c.appState('background');
  r.c.appState('active');
  for (let i = 0; i < 10; i++) {
    r.c.level(0.9);
    r.clock.advance(100);
  }
  assert.equal(r.c.gate.open, false, 'sound continues: still shut');
  r.c.level(0);
  r.clock.advance(500);
  r.c.level(0);
  assert.equal(r.c.gate.open, false, 'the quiet only released the latch');
  r.c.level(0.9);
  assert.equal(r.c.gate.open, true, 'a fresh onset opens it');
});

test('touch cancel shuts at once with no tail', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  await settle();
  r.c.cancel();
  assert.equal(r.c.gate.open, false);
});

test('a route change shuts a held press at once; a new press opens (M-B2)', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  await settle();
  assert.equal(r.c.gate.open, true);
  r.c.routeChange({ reason: 'oldDeviceUnavailable' }, 'Speaker');
  assert.equal(r.c.gate.open, false);
  assert.equal(r.c.state.route, 'Speaker');
  assert.equal(r.c.state.note, null, 'not the 30 s bound');
  assert.deepEqual(r.c.state.blocked, [], 'a route change settles no lifecycle reason');
  r.c.press();
  assert.equal(r.c.gate.open, true, 'a new press opens');
});

test('a route change while an interruption blocks does not lift the block', async () => {
  const r = rig();
  await joined(r);
  r.c.interruption({ type: 'began' });
  r.c.routeChange({ reason: 'categoryChange' }, 'Receiver');
  assert.deepEqual(r.c.state.blocked, ['interruption']);
  r.c.press();
  assert.equal(r.c.gate.open, false);
  r.c.interruption({ type: 'ended', shouldResume: false });
  assert.deepEqual(r.c.state.blocked, []);
  assert.equal(r.c.gate.open, false, 'shouldResume is not read, and nothing opens');
  assert.deepEqual(r.c.audio.delivered, ['interruption began', 'route change categoryChange', 'interruption ended shouldResume=false']);
});

test('Leave: the gate shuts, stopped is reported, then the room goes and the observers stops', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  await settle();
  const left = r.c.leave();
  await settle();
  r.clock.advance(reportWait * 2);
  await left;
  assert.equal(r.c.state.phase, 'idle');
  const order = steps(r.log);
  assert.ok(order.indexOf('report stopped') < order.indexOf('room disconnect'));
  assert.ok(order.indexOf('room disconnect') < order.indexOf('observers stop'));
});

test('duplicate_identity stops, says so and does not rejoin (M-S2)', async () => {
  const r = rig();
  await joined(r);
  r.rooms[0]?.events?.onDisconnected('duplicate_identity');
  await settle();
  r.clock.advance(60_000);
  await settle();
  assert.equal(r.c.state.phase, 'idle');
  assert.match(r.c.state.why ?? '', /Another device/);
  assert.equal(r.log.filter((l) => l === 'session').length, 1, 'no new session');
  assert.ok(r.log.includes('observers stop'));
});

test('a connection lost in the foreground is retried through conchd with the gate shut; the app away leaves and says so (M-L7)', async () => {
  const fg = rig();
  await joined(fg);
  fg.rooms[0]?.events?.onDisconnected('lost');
  await settle();
  assert.equal(fg.c.state.phase, 'reconnecting');
  fg.clock.advance(1000);
  await settle();
  assert.equal(fg.c.state.phase, 'connected');
  assert.equal(fg.log.filter((l) => l === 'session').length, 2);
  assert.equal(fg.c.gate.open, false, 'the gate is shut after a reconnect');
  assert.ok(!fg.log.includes('unmute'));

  const bg = rig();
  await joined(bg);
  bg.c.appState('background');
  bg.rooms[0]?.events?.onDisconnected('lost');
  await settle();
  assert.equal(bg.c.state.phase, 'idle');
  assert.match(bg.c.state.why ?? '', /app was away/);
  assert.equal(bg.log.filter((l) => l === 'session').length, 1, 'no rejoin from the background');
});

test('opening the app never joins voice', () => {
  const r = rig();
  r.c.appState('active');
  assert.equal(r.c.state.phase, 'idle');
  assert.deepEqual(steps(r.log), []);
});

test('tearDown (a dead credential) stops media without a report', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  await settle();
  await r.c.tearDown();
  assert.equal(r.c.state.phase, 'idle');
  assert.ok(r.log.includes('room disconnect'));
  assert.ok(r.log.includes('observers stop'));
});

test('the bridge maps the OS signals; an interruption ending only lifts the block', () => {
  assert.equal(appStateOf('active'), 'active');
  assert.equal(appStateOf('inactive'), 'inactive');
  assert.equal(appStateOf('background'), 'background');
  assert.equal(appStateOf('something new'), 'background');
  const calls: string[] = [];
  const sink = {
    appState: (s: string) => calls.push(`app ${s}`),
    interruption: (e: InterruptionEvent) => calls.push(`interruption ${e.type} ${String(e.shouldResume)}`),
    routeChange: (e: RouteChangeEvent, route: string) => calls.push(`route ${e.reason} ${route}`),
  };
  deliver(sink, { type: 'interruption', state: 'began', detail: '' });
  deliver(sink, { type: 'interruption', state: 'ended', detail: 'shouldResume' });
  deliver(sink, { type: 'interruption', state: 'ended', detail: '' });
  deliver(sink, { type: 'route', state: 'oldDeviceUnavailable', detail: 'Speaker' });
  deliver(sink, { type: 'route', state: 'a reason iOS adds later', detail: 'Speaker' });
  deliver(sink, { type: 'unknown', state: 'x', detail: '' });
  assert.deepEqual(calls, [
    'interruption began undefined',
    'interruption ended true',
    'interruption ended false',
    'route oldDeviceUnavailable Speaker',
    'route unknown Speaker',
  ]);
});

test('open mode: a route change under an open gate arms the resume latch', async () => {
  const r = rig();
  await joined(r);
  r.c.setMode('open');
  r.c.level(0.9);
  r.clock.advance(1000);
  r.c.level(0);
  r.clock.advance(500);
  r.c.level(0);
  r.c.level(0.9);
  assert.equal(r.c.gate.open, true);
  r.c.routeChange({ reason: 'oldDeviceUnavailable' }, 'Speaker');
  assert.equal(r.c.gate.open, false);
  for (let i = 0; i < 5; i++) {
    r.c.level(0.9);
    r.clock.advance(100);
  }
  assert.equal(r.c.gate.open, false, 'the same sound does not reopen it');
  r.c.level(0);
  r.clock.advance(500);
  r.c.level(0);
  r.c.level(0.9);
  assert.equal(r.c.gate.open, true, 'quiet, then a fresh onset');
});

test('presence follows the joined channel, not the one on screen', async () => {
  const asked: string[] = [];
  const r = rig({ idsByChannel: { a: [3], b: [9] }, asked });
  r.c.watch('a');
  await r.c.join('a');
  await settle();
  assert.deepEqual(r.c.state.participants.map((p) => p.principal_id), [3]);
  asked.length = 0;
  r.c.watch('b'); // the Voice view of another channel opens
  await settle();
  assert.deepEqual(r.c.state.participants.map((p) => p.principal_id), [3], 'A is still the room shown');
  r.clock.advance(3000);
  await settle();
  assert.ok(asked.length > 0 && asked.every((ch) => ch === 'a'), 'only A is polled');
  r.c.watch(null); // B's view closes: A keeps polling
  asked.length = 0;
  r.clock.advance(3000);
  await settle();
  assert.ok(asked.includes('a'));
});

test('Leave starts presence for the channel on screen again', async () => {
  const asked: string[] = [];
  const r = rig({ idsByChannel: { a: [3], b: [9] }, asked });
  r.c.watch('b');
  await r.c.join('a');
  await settle();
  await r.c.leave();
  asked.length = 0;
  r.clock.advance(3000);
  await settle();
  assert.ok(asked.includes('b'), 'B, the view on screen, is read again');
});

test('open mode: leaving the joined Voice view shuts the open gate, and it needs quiet and a fresh onset to reopen', async () => {
  const r = rig();
  await joined(r);
  r.c.watch('general');
  r.c.setMode('open');
  r.c.level(0);
  r.clock.advance(500);
  r.c.level(0.9);
  assert.equal(r.c.gate.open, true);
  r.c.watch(null); // Back to Channels: the probe is gone, no more samples
  assert.equal(r.c.gate.open, false, 'shut at once');
  // The view comes back, with the same sound still there.
  r.c.watch('general');
  for (let i = 0; i < 5; i++) {
    r.c.level(0.9);
    r.clock.advance(100);
  }
  assert.equal(r.c.gate.open, false, 'no reopen on the same sound');
  r.c.level(0);
  r.clock.advance(500);
  r.c.level(0);
  r.c.level(0.9);
  assert.equal(r.c.gate.open, true, 'quiet, then a fresh onset');
});

test('open mode: with no view and no samples at all the watchdog shuts the gate', async () => {
  const r = rig();
  await joined(r);
  r.c.setMode('open');
  r.c.level(0);
  r.clock.advance(500);
  r.c.level(0.9);
  assert.equal(r.c.gate.open, true);
  r.clock.advance(1600);
  assert.equal(r.c.gate.open, false);
});

test('a press held while the joined Voice view unmounts shuts at once', async () => {
  const r = rig();
  await joined(r);
  r.c.watch('general');
  r.c.press();
  assert.equal(r.c.gate.open, true);
  r.c.watch(null);
  assert.equal(r.c.gate.open, false);
  await settle();
  assert.deepEqual(r.reports, ['started', 'stopped']);
});

test("another channel's view closing does not shut the joined channel's gate", async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  r.c.watch('elsewhere');
  r.c.watch(null);
  assert.equal(r.c.gate.open, true);
});

test("the LiveKit grant's host and host:port are registered with the diagnostics", async () => {
  const held: string[] = [];
  const r = rig({ livekitUrl: 'wss://livekit.example.me:7880', held });
  await joined(r);
  assert.deepEqual(held, ['wss://livekit.example.me:7880', 'livekit.example.me:7880', 'livekit.example.me']);
});

// iOS's counterpart of Android's permanent focus loss: an interruption that
// began and whose end the app never hears (the observers stopped at Leave, or
// the app was suspended). It must not outlive the room.
test('an interruption that never ended does not outlive Leave: began, leave, join, and a press opens', async () => {
  const r = rig();
  await joined(r);
  r.c.interruption({ type: 'began' });
  assert.deepEqual(r.c.state.blocked, ['interruption']);
  r.c.press();
  assert.equal(r.c.gate.open, false, 'blocked by the interruption');
  await r.c.leave();
  assert.deepEqual(r.c.state.blocked, [], 'stopping the observers clears it: no end will ever come');
  await joined(r);
  assert.deepEqual(r.c.state.blocked, []);
  r.c.press();
  assert.equal(r.c.gate.open, true, 'a fresh press opens');
});

test('the same holds for tear-down (a dead credential)', async () => {
  const r = rig();
  await joined(r);
  r.c.interruption({ type: 'began' });
  await r.c.tearDown();
  assert.deepEqual(r.c.state.blocked, []);
  await joined(r);
  r.c.press();
  assert.equal(r.c.gate.open, true);
});

test('back in front with an interruption that never ended: the gate stays shut and the person is told; its end clears the note', async () => {
  const r = rig();
  await joined(r);
  r.c.interruption({ type: 'began' });
  r.c.appState('background');
  r.c.appState('active');
  assert.deepEqual(r.c.state.blocked, ['interruption'], 'nothing lifted it');
  assert.match(r.c.state.note ?? '', /has not said it is over/);
  r.c.press();
  assert.equal(r.c.gate.open, false);
  r.c.interruption({ type: 'ended', shouldResume: false });
  assert.deepEqual(r.c.state.blocked, []);
  assert.equal(r.c.state.note, null, 'the end arrived after all');
  assert.equal(r.c.gate.open, false, 'and it opened nothing');
});

test('returning with no interruption pending adds no note', async () => {
  const r = rig();
  await joined(r);
  r.c.appState('background');
  r.c.appState('active');
  assert.equal(r.c.state.note, null);
});

// ---- the camera (M5-D), ported from Android's M4-D controller tests with its
// review fixes. iOS shows the camera permission as a system alert over the
// app, which the lifecycle sees as AppState 'inactive'; so the prompt takes the
// app out of the foreground before it answers.
const promptAway = (answer: boolean) => (_clock: FakeClock, self: () => VoiceController): Promise<boolean> => {
  self().appState('inactive');
  return Promise.resolve(answer);
};

const captures = (log: string[]) => log.filter((l) => l.startsWith('capture')).length;

test('camera: off on joining; no permission asked, no claim, no capture (M-C2)', async () => {
  const r = rig();
  await joined(r);
  assert.equal(r.c.state.cameraOn, false);
  assert.ok(!r.log.includes('camera permission'));
  assert.equal(captures(r.log), 0);
  assert.ok(!r.log.some((l) => l.startsWith('claim ')));
});

test('camera tap: the permission is asked at the tap, then the claim, the capture and the publish (M-C1, M-C7)', async () => {
  const r = rig();
  await joined(r);
  await r.c.cameraTap();
  await settle();
  const order = steps(r.log);
  assert.ok(order.indexOf('camera permission') < order.findIndex((l) => l.startsWith('capture')));
  assert.equal(r.c.state.camera.kind, 'sending');
  assert.equal(r.c.state.cameraOn, true);
  await r.c.cameraTap(); // off
  await settle();
  assert.ok(r.log.includes(`release general ${startedAt}`));
  assert.equal(r.c.state.cameraOn, false);
});

test('camera tap: permission denied leaves the camera off with a note and no claim (M-C7)', async () => {
  const r = rig({ camera: false });
  await joined(r);
  await r.c.cameraTap();
  await settle();
  assert.equal(captures(r.log), 0);
  assert.ok(!r.log.some((l) => l.startsWith('claim ')));
  assert.equal(r.c.state.cameraOn, false);
  assert.equal(r.c.state.camera.kind, 'failed');
});

test('camera tap: Listen only says why and does nothing', async () => {
  const r = rig({ canPublish: false });
  await joined(r);
  await r.c.cameraTap();
  assert.equal(captures(r.log), 0);
  assert.ok(!r.log.includes('camera permission'));
  assert.notEqual(r.c.state.cameraExtra, null);
});

test('camera: every lifecycle reason ends it, and nothing turns it back on (M-C3 to M-C6)', async () => {
  const cases: { name: string; start: (c: VoiceController) => void; end: (c: VoiceController) => void }[] = [
    { name: 'background', start: (c) => c.appState('background'), end: (c) => c.appState('active') },
    { name: 'inactive', start: (c) => c.appState('inactive'), end: (c) => c.appState('active') },
    { name: 'interruption', start: (c) => c.interruption({ type: 'began' }), end: (c) => c.interruption({ type: 'ended', shouldResume: true }) },
  ];
  for (const t of cases) {
    const r = rig();
    await joined(r);
    await r.c.cameraTap();
    await settle();
    assert.equal(r.c.state.camera.kind, 'sending', t.name);
    t.start(r.c);
    await settle();
    assert.equal(r.c.state.cameraOn, false, `${t.name}: stopped`);
    assert.ok(r.log.includes('camera stop'), t.name);
    assert.ok(r.log.includes(`release general ${startedAt}`), `${t.name}: the claim is released`);
    const before = captures(r.log);
    t.end(r.c);
    await settle();
    r.clock.advance(3000);
    await settle();
    assert.equal(captures(r.log), before, `${t.name}: nothing restarts on resume`);
    assert.equal(r.c.state.cameraOn, false, t.name);
  }
});

test('camera tap: a grant just before the app goes away starts nothing when timers were frozen across a long gap', async () => {
  const r = rig({ prompt: promptAway(true) });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  // Suspended: React Native's timers are paused. A minute passes on the wall
  // clock and no tick runs; then the app returns.
  r.clock.now += 60_000;
  r.c.appState('active');
  r.clock.advance(100);
  await tap;
  await settle();
  assert.equal(captures(r.log), 0, 'no capture on resume, with no new tap');
  assert.equal(r.c.state.cameraExtra, tapAgain);
  assert.equal(r.c.state.cameraOn, false);
});

test('camera tap: the foreground back within the wait starts the camera', async () => {
  const r = rig({ prompt: promptAway(true) });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  r.clock.advance(300);
  r.c.appState('active');
  r.clock.advance(100);
  await tap;
  await settle();
  assert.equal(captures(r.log), 1);
});

test('camera tap: a block that begins after the permission answer abandons the tap', async () => {
  const r = rig({ prompt: promptAway(true) });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  r.c.interruption({ type: 'began' }); // a new reason, after the answer
  r.c.appState('active');
  r.c.interruption({ type: 'ended', shouldResume: true }); // every block has lifted again
  r.clock.advance(100);
  await tap;
  await settle();
  assert.equal(captures(r.log), 0);
  assert.equal(r.c.state.cameraExtra, tapAgain);
});

test('camera tap: a tap in flight does not survive Leave', async () => {
  const r = rig({ prompt: promptAway(true) });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  const leaving = r.c.leave();
  r.clock.advance(2000);
  await leaving;
  r.c.appState('active');
  r.clock.advance(300);
  await tap;
  await settle();
  assert.equal(captures(r.log), 0);
  assert.equal(r.c.state.cameraOn, false);
  assert.equal(r.c.state.phase, 'idle');
});

test('Leave stops the camera and releases the claim before the room goes', async () => {
  const r = rig();
  await joined(r);
  await r.c.cameraTap();
  await settle();
  await r.c.leave();
  const order = r.log;
  assert.ok(order.indexOf('camera stop') >= 0 && order.indexOf(`release general ${startedAt}`) >= 0);
  assert.ok(order.indexOf(`release general ${startedAt}`) < order.indexOf('room disconnect'));
});

test('tear-down stops the capture first, then drops the room, then waits for the release', async () => {
  const r = rig();
  await joined(r);
  await r.c.cameraTap();
  await settle();
  assert.equal(r.c.state.camera.kind, 'sending');
  await r.c.tearDown();
  const stop = r.log.indexOf('camera stop');
  const drop = r.log.indexOf('room disconnect');
  assert.ok(stop >= 0 && drop > stop, 'capture stopped before the room went');
});

test('a reconnect tells the room what to receive again', async () => {
  const r = rig();
  await joined(r);
  const room = r.rooms[0];
  assert.ok(room !== undefined);
  const before = room.wants.length;
  room.events?.onReconnecting();
  room.events?.onReconnected();
  assert.ok(room.wants.length >= before + 2);
});

test('receive: a camera is wanted only from a claim holder, while the Voice view is open and nothing blocks', async () => {
  const r = rig({ ids: [3, 4], cameraPeers: [3] });
  r.c.watch('general');
  await r.c.join('general');
  await settle();
  const room = r.rooms[0];
  assert.ok(room !== undefined);
  const last = (): CameraWants => room.wants[room.wants.length - 1] ?? { viewing: false, shown: false, claimed: new Set() };
  assert.equal(last().viewing, false, 'the view has not said it is open');
  r.c.setViewing(true);
  assert.deepEqual([...last().claimed], [3]);
  assert.equal(last().viewing && last().shown, true);
  r.c.appState('inactive');
  assert.equal(last().shown, false, 'the app is away: nothing is received');
  r.c.appState('active');
  r.c.setViewing(false);
  assert.equal(last().viewing, false, 'the view closed: every camera is let go');
});

test('the server ending the claim, told by presence, stops the camera through the controller', async () => {
  const r = rig({ cameraEnded: { reason: 'operator', started_at: startedAt, ended_at: '2026-11-05T10:05:00Z' } });
  r.c.watch('general');
  await joined(r);
  await r.c.cameraTap();
  await settle();
  assert.equal(r.c.state.camera.kind, 'sending');
  r.clock.advance(3000); // the next presence read carries camera_ended
  await settle();
  assert.equal(r.c.state.cameraOn, false);
  assert.ok(r.log.includes('camera stop'));
});

// ---- a tap waiting on the permission answer is abandoned (review of #1736).
// The answer is held back by the test, so the tap is still inside permission()
// when each of these happens; the alert's own 'inactive' has already come.
function deferred(): { promise: Promise<boolean>; answer: (allowed: boolean) => void } {
  let answer: (allowed: boolean) => void = () => undefined;
  const promise = new Promise<boolean>((resolve) => {
    answer = resolve;
  });
  return { promise, answer };
}

async function pendingTap() {
  const d = deferred();
  const r = rig({ prompt: () => d.promise });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  r.c.appState('inactive'); // the permission alert
  return { r, d, tap };
}

test('camera tap: before the answer, each of these abandons it: no capture, no claim, even if Allow comes later', async () => {
  const cases: { name: string; happen: (r: Rig) => Promise<void> | void }[] = [
    { name: 'background', happen: (r) => r.c.appState('background') },
    { name: 'a second inactive', happen: (r) => (r.c.appState('active'), r.c.appState('inactive')) },
    { name: 'an interruption', happen: (r) => r.c.interruption({ type: 'began' }) },
    { name: 'tear-down', happen: (r) => r.c.tearDown() },
    { name: 'Leave', happen: async (r) => { const leaving = r.c.leave(); r.clock.advance(2000); await leaving; } },
    { name: 'a lost connection', happen: (r) => r.rooms[0]?.events?.onDisconnected('lost') },
    { name: 'reconnecting', happen: (r) => r.rooms[0]?.events?.onReconnecting() },
    { name: 'a tap on the button, which reads Turn camera off', happen: (r) => r.c.cameraTap() },
  ];
  for (const c of cases) {
    const { r, d, tap } = await pendingTap();
    await c.happen(r);
    await settle();
    r.clock.now += 120_000; // the Allow comes minutes later
    d.answer(true);
    r.c.appState('active');
    r.clock.advance(3000);
    await tap;
    await settle();
    assert.equal(captures(r.log), 0, `${c.name}: no capture`);
    assert.ok(!r.log.some((l) => l.startsWith('claim ')), `${c.name}: no claim`);
    assert.equal(r.c.state.cameraOn, false, c.name);
  }
});

test('camera tap: the alert alone (one inactive, then the answer) still starts the camera', async () => {
  const { r, d, tap } = await pendingTap();
  d.answer(true);
  r.c.appState('active');
  r.clock.advance(100);
  await tap;
  await settle();
  assert.equal(captures(r.log), 1);
});

test('camera tap: the alert reaching JavaScript only after the answer fails shut: whatever started is stopped at once and the camera is off', async () => {
  const d = deferred();
  const r = rig({ prompt: () => d.promise });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  d.answer(true);
  await settle(); // nothing blocks yet: the capture may begin
  r.c.appState('inactive'); // the late alert event: the lifecycle stops it
  await settle();
  r.c.appState('active');
  r.clock.advance(3000);
  await tap;
  await settle();
  assert.equal(r.c.state.cameraOn, false);
  assert.ok(captures(r.log) === 0 || r.log.includes('camera stop'), 'any capture that began was stopped');
  assert.equal(r.c.state.camera.kind === 'sending', false);
});

test('camera tap: Leave then join clears a stale note (reset tells the screen)', async () => {
  const r = rig({ canPublish: false });
  await joined(r);
  await r.c.cameraTap();
  assert.notEqual(r.c.state.cameraExtra, null, 'Listen only says why');
  await r.c.leave();
  await r.c.join('general');
  assert.equal(r.c.state.cameraExtra, null, 'the old note does not carry into the next join');
});
