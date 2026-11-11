/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import type { CameraClaimResult } from '../src/logic/api';
import type { CameraRoom, KeptCamera } from '../src/logic/camera';
import type { CameraWants } from '../src/app/camera/policy';
import { VoiceController, type NativePort, type RoomEvents, type RoomPort, type VoiceApi } from '../src/app/controller';
import { appStateOf, deliver } from '../src/app/bridge';
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

function session(canPublish = true): VoiceSessionResponseV1 {
  return { livekit_url: 'x', identity: `p${me}`, rooms: [{ room: 'r', token: 't', can_publish: canPublish, expires_at: '2026-11-05T10:00:15Z' }] };
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
    service?: boolean;
    canPublish?: boolean;
    focus?: boolean[];
    // Who is in each channel's room, and the channels presence was read for.
    idsByChannel?: Record<string, number[]>;
    asked?: string[];
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
    voiceSession: () => (log.push('session'), Promise.resolve({ ok: true, value: session(options.canPublish ?? true) } as const)),
    reportTransmit: (_c, state) => {
      log.push(`report ${state}`);
      reports.push(state);
      return Promise.resolve('ok');
    },
  };
  const native: NativePort = {
    ensureCameraPermission: () => {
      log.push('camera permission');
      if (options.prompt !== undefined) return options.prompt(clock, () => self as VoiceController);
      return Promise.resolve(options.camera !== false);
    },
    ensurePermissions: () => (log.push('permissions'), Promise.resolve(options.mic ?? 'granted')),
    startService: () => (log.push('service start'), options.service === false ? Promise.reject(new Error('ForegroundServiceStartNotAllowedException')) : Promise.resolve()),
    stopService: () => (log.push('service stop'), Promise.resolve()),
    // Each request takes the next answer of options.focus; granted by default.
    requestFocus: () => (log.push('focus'), Promise.resolve(options.focus?.shift() ?? true)),
    abandonFocus: () => (log.push('focus abandoned'), Promise.resolve()),
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

test('join: presence, permission, service, focus, session, room, in that order; nothing is sent', async () => {
  const r = rig();
  await joined(r);
  // (Then the deafen state is applied and presence is polled.)
  assert.deepEqual(steps(r.log).slice(0, 6), ['presence', 'permissions', 'service start', 'focus', 'session', 'room connect publish=true']);
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
  assert.deepEqual(steps(r.log), ['presence'], 'no service, no session before the person agrees');
  await r.c.confirmJoin();
  await settle();
  assert.equal(r.c.state.phase, 'connected');
  assert.ok(r.log.includes('service start'));
});

test('hand-off: cancelling the warning joins nothing', async () => {
  const r = rig({ ids: [me] });
  await r.c.join('general');
  r.c.cancelJoin();
  assert.equal(r.c.state.phase, 'idle');
  assert.ok(!r.log.includes('service start'));
});

test('no warning when only other people are in the room', async () => {
  const r = rig({ ids: [3, 4] });
  await joined(r);
  assert.equal(r.c.state.warning, null);
});

test('microphone permission denied fails closed: no service, no session (M-P1)', async () => {
  const r = rig({ mic: 'microphone_denied' });
  await r.c.join('general');
  await settle();
  assert.equal(r.c.state.phase, 'idle');
  assert.match(r.c.state.why ?? '', /microphone/i);
  assert.deepEqual(steps(r.log), ['presence', 'permissions']);
});

test('a refused service start fails the join by name, with no session (M-P3)', async () => {
  const r = rig({ service: false });
  await r.c.join('general');
  await settle();
  assert.equal(r.c.state.phase, 'idle');
  assert.match(r.c.state.why ?? '', /ForegroundServiceStartNotAllowedException/);
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

test('lifecycle: background, lock and interruption each shut a held press at once, and resume opens nothing', async () => {
  const cases: { name: string; start: (c: VoiceController) => void; end: (c: VoiceController) => void }[] = [
    { name: 'background', start: (c) => c.appState('background'), end: (c) => c.appState('active') },
    { name: 'inactive', start: (c) => c.appState('inactive'), end: (c) => c.appState('active') },
    { name: 'lock', start: (c) => c.screenLocked(true), end: (c) => c.screenLocked(false) },
    { name: 'interruption', start: (c) => c.interrupted(true), end: (c) => c.interrupted(false) },
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

test('a route change shuts an open gate (M-B2)', async () => {
  const r = rig();
  await joined(r);
  r.c.press();
  r.c.routeChanged('bluetooth');
  assert.equal(r.c.gate.open, false);
});

test('Leave: the gate shuts, stopped is reported, then the room goes, focus is abandoned and the service stops', async () => {
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
  assert.ok(order.indexOf('room disconnect') < order.indexOf('service stop'));
  assert.ok(order.includes('focus abandoned'));
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
  assert.ok(r.log.includes('service stop'));
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
  assert.ok(r.log.includes('service stop'));
});

test('the bridge maps the OS signals; a focus gain only lifts the block', () => {
  assert.equal(appStateOf('active'), 'active');
  assert.equal(appStateOf('inactive'), 'inactive');
  assert.equal(appStateOf('background'), 'background');
  assert.equal(appStateOf('something new'), 'background');
  const calls: string[] = [];
  const sink = {
    appState: (s: string) => calls.push(`app ${s}`),
    screenLocked: (b: boolean) => calls.push(`lock ${b}`),
    interrupted: (b: boolean) => calls.push(`interrupted ${b}`),
    routeChanged: (r: string) => calls.push(`route ${r}`),
    taskRemoved: () => calls.push('task removed'),
  };
  deliver(sink, { type: 'focus', state: 'lost', detail: '' });
  deliver(sink, { type: 'focus', state: 'duck', detail: '' });
  deliver(sink, { type: 'focus', state: 'lost_transient', detail: '' });
  deliver(sink, { type: 'focus', state: 'gained', detail: '' });
  deliver(sink, { type: 'lock', state: 'locked', detail: '' });
  deliver(sink, { type: 'lock', state: 'unlocked', detail: '' });
  deliver(sink, { type: 'route', state: 'changed', detail: 'bluetooth' });
  deliver(sink, { type: 'service', state: 'task_removed', detail: '' });
  deliver(sink, { type: 'service', state: 'started', detail: '' });
  deliver(sink, { type: 'unknown', state: 'x', detail: '' });
  assert.deepEqual(calls, ['interrupted true', 'interrupted true', 'interrupted false', 'lock true', 'lock false', 'route bluetooth', 'task removed']);
});

test('a permanent focus loss does not outlive Leave: lost, leave, join, and a press opens', async () => {
  const r = rig();
  await joined(r);
  deliver(r.c, { type: 'focus', state: 'lost', detail: '' });
  assert.deepEqual(r.c.state.blocked, ['interruption']);
  r.c.press();
  assert.equal(r.c.gate.open, false, 'blocked by the loss');
  await r.c.leave();
  assert.deepEqual(r.c.state.blocked, [], 'abandoning the focus clears the reason: no gain will ever come');
  await joined(r);
  assert.deepEqual(r.c.state.blocked, []);
  r.c.press();
  assert.equal(r.c.gate.open, true, 'a fresh press opens');
});

test('the same holds for tear-down (a dead credential)', async () => {
  const r = rig();
  await joined(r);
  deliver(r.c, { type: 'focus', state: 'lost', detail: '' });
  await r.c.tearDown();
  assert.deepEqual(r.c.state.blocked, []);
  await joined(r);
  r.c.press();
  assert.equal(r.c.gate.open, true);
});

test('a denied focus request holds the block and says so; a later grant lifts it only, and talking needs a fresh press', async () => {
  const r = rig({ focus: [false, true] });
  await joined(r);
  assert.deepEqual(r.c.state.blocked, ['interruption']);
  assert.match(r.c.state.note ?? '', /Another app has the audio/);
  r.c.press();
  assert.equal(r.c.gate.open, false);
  r.c.appState('background');
  r.c.appState('active');
  await settle();
  assert.equal(r.log.filter((l) => l === 'focus').length, 2, 'the focus was asked for again on return');
  assert.deepEqual(r.c.state.blocked, []);
  assert.equal(r.c.gate.open, false, 'the grant opened nothing');
  r.c.press();
  assert.equal(r.c.gate.open, true);
});

test('after a permanent loss the focus is asked for again when the app is next active, and not for a transient one', async () => {
  const r = rig();
  await joined(r);
  deliver(r.c, { type: 'focus', state: 'lost_transient', detail: '' });
  r.c.appState('active');
  await settle();
  assert.equal(r.log.filter((l) => l === 'focus').length, 1, 'a transient loss waits for its gain');
  deliver(r.c, { type: 'focus', state: 'gained', detail: '' });
  deliver(r.c, { type: 'focus', state: 'lost', detail: '' });
  r.c.appState('active');
  await settle();
  assert.equal(r.log.filter((l) => l === 'focus').length, 2);
  assert.deepEqual(r.c.state.blocked, []);
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
  r.c.routeChanged('bluetooth');
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

test('swiping the app away ends voice', async () => {
  const r = rig();
  await joined(r);
  deliver(r.c, { type: 'service', state: 'task_removed', detail: '' });
  await settle();
  assert.equal(r.c.state.phase, 'idle');
  assert.ok(r.log.includes('room disconnect'));
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

// The permission prompt takes the activity out of the foreground (Android shows
// it in front), so the app reports 'background' before it answers.
const promptAway = (answer: boolean) => (_clock: FakeClock, self: () => VoiceController): Promise<boolean> => {
  self().appState('background');
  return Promise.resolve(answer);
};

const captures = (log: string[]) => log.filter((l) => l.startsWith('capture')).length;

test('camera tap: a grant just before the app goes away starts nothing when timers were frozen across a long gap', async () => {
  const r = rig({ prompt: promptAway(true) });
  await joined(r);
  const tap = r.c.cameraTap();
  await settle();
  // Backgrounded: React Native's timers are paused. A minute passes on the
  // wall clock and no tick runs; then the app returns.
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
  r.c.screenLocked(true); // a new reason, after the answer
  r.c.appState('active');
  r.c.screenLocked(false); // every block has lifted again
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
