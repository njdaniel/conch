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
    presence: () => {
      log.push('presence');
      const value = presence(options.ids ?? []);
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
    requestFocus: () => (log.push('focus'), Promise.resolve(true)),
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
  };
  deliver(sink, { type: 'focus', state: 'lost', detail: '' });
  deliver(sink, { type: 'focus', state: 'duck', detail: '' });
  deliver(sink, { type: 'focus', state: 'lost_transient', detail: '' });
  deliver(sink, { type: 'focus', state: 'gained', detail: '' });
  deliver(sink, { type: 'lock', state: 'locked', detail: '' });
  deliver(sink, { type: 'lock', state: 'unlocked', detail: '' });
  deliver(sink, { type: 'route', state: 'changed', detail: 'bluetooth' });
  deliver(sink, { type: 'unknown', state: 'x', detail: '' });
  assert.deepEqual(calls, ['interrupted true', 'interrupted true', 'interrupted false', 'lock true', 'lock false', 'route bluetooth']);
});


// ---- the camera (M4-D)

const afterJoin = (log: string[]) => steps(log).slice(steps(log).findIndex((l) => l.startsWith('room connect')) + 1).filter((l) => l !== 'presence' && !l.startsWith('deafen'));

async function cameraOn(r: Rig): Promise<void> {
  await r.c.cameraTap();
  await settle();
  assert.equal(r.c.state.camera.kind, 'sending');
}

test('joining captures nothing, claims nothing and asks no camera permission (M-C2, M-C7)', async () => {
  const r = rig();
  await joined(r);
  assert.ok(!r.log.includes('camera permission'));
  assert.deepEqual(afterJoin(r.log), []);
  assert.equal(r.c.state.camera.kind, 'off');
  assert.equal(r.c.state.cameraOn, false);
});

test('the button: permission at the first tap, then capture, claim, grant, publish in that order; a second tap stops, then releases (M-C1, M-C7)', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  assert.deepEqual(afterJoin(r.log), ['camera permission', 'capture phone', 'claim general new', 'camera grant', 'camera publish']);
  assert.equal(r.c.state.cameraOn, true);
  await r.c.cameraTap();
  await settle();
  assert.deepEqual(afterJoin(r.log).slice(5), ['camera stop', `release general ${startedAt}`]);
  assert.equal(r.c.state.camera.kind, 'off');
  assert.equal(r.c.state.cameraOn, false);
});

test('the camera permission refused: nothing is captured or claimed, and the person is told', async () => {
  const r = rig({ camera: false });
  await joined(r);
  await r.c.cameraTap();
  await settle();
  assert.deepEqual(afterJoin(r.log), ['camera permission']);
  assert.deepEqual(r.c.state.camera, { kind: 'failed', reason: 'blocked' });
  assert.equal(r.c.state.cameraOn, false);
});

test('Listen only cannot send a camera: nothing is asked of the OS or the server', async () => {
  const r = rig({ canPublish: false });
  await joined(r);
  await r.c.cameraTap();
  await settle();
  assert.deepEqual(afterJoin(r.log), []);
  assert.match(r.c.state.cameraExtra ?? '', /Listen only/);
});

test('the camera button does nothing when not in voice', async () => {
  const r = rig();
  await r.c.cameraTap();
  assert.deepEqual(steps(r.log), []);
});

test('a claim refused (the room is full) stops the capture and says so', async () => {
  const r = rig();
  await joined(r);
  r.claims.push({ ok: false, failure: 'invalid', status: 409, code: 'voice_camera_full' });
  await r.c.cameraTap();
  await settle();
  assert.deepEqual(r.c.state.camera, { kind: 'failed', reason: 'full' });
  assert.equal(r.rooms[0]?.camera.captured, false);
  assert.ok(!r.log.some((l) => l.startsWith('release')), 'a claim never granted is not released');
});

for (const name of ['background', 'inactive'] as const) {
  test(`${name} with the camera on: the capture stops before anything is awaited, the claim is released naming it, and return turns nothing on (M-C3, M-C6)`, async () => {
    const r = rig();
    await joined(r);
    await cameraOn(r);
    const mark = r.log.length;
    r.c.appState(name);
    // Synchronously, in the same tick as the event: the 1 s bar.
    assert.equal(r.rooms[0]?.camera.captured, false);
    assert.equal(r.log[mark], 'camera stop');
    await settle();
    assert.deepEqual(r.log.slice(mark), ['camera stop', `release general ${startedAt}`]);
    assert.deepEqual(r.c.state.camera, { kind: 'off', ended: 'lifecycle' });
    assert.match(r.c.state.cameraExtra ?? r.c.state.camera.kind, /off/);
    const after = r.log.length;
    r.c.appState('active');
    await settle();
    r.clock.advance(60_000);
    await settle();
    assert.deepEqual(r.log.slice(after).filter((l) => /capture|claim|publish|permission/.test(l)), [], 'nothing turns the camera back on');
    assert.equal(r.c.state.camera.kind, 'off');
  });
}

test('lock and interruption each end the camera too; the camera is off after the last lifts (M-C4, M-C5)', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  r.c.screenLocked(true);
  assert.equal(r.rooms[0]?.camera.captured, false);
  await settle();
  r.c.screenLocked(false);
  assert.equal(r.c.state.camera.kind, 'off');
  await cameraOn(r);
  r.c.interrupted(true);
  assert.equal(r.rooms[0]?.camera.captured, false);
  await settle();
  r.c.interrupted(false);
  await settle();
  assert.equal(r.c.state.camera.kind, 'off');
  assert.equal(r.log.filter((l) => l.startsWith('claim')).length, 2, 'two taps, two claims, no more');
});

test('a tap while a lifecycle reason holds does nothing, and the button works again once it lifts', async () => {
  const r = rig();
  await joined(r);
  r.c.appState('background');
  await r.c.cameraTap();
  assert.deepEqual(afterJoin(r.log), []);
  r.c.appState('active');
  await cameraOn(r);
});

test('the permission prompt takes the app out of the foreground: the capture starts when it is back (M-C7)', async () => {
  const r = rig({
    prompt: (clock, c) => {
      c().appState('background');
      return new Promise((resolve) => {
        clock.setTimeout(() => {
          c().appState('active');
          resolve(true);
        }, 0);
      });
    },
  });
  await joined(r);
  const tap = r.c.cameraTap();
  r.clock.advance(0);
  await settle();
  r.clock.advance(100);
  await tap;
  await settle();
  assert.equal(r.c.state.camera.kind, 'sending');
  assert.equal(r.log.filter((l) => l.startsWith('capture')).length, 1);
});

test('the foreground never comes back after the prompt: nothing starts and the person is asked to tap again', async () => {
  const r = rig({
    prompt: (_clock, c) => {
      c().appState('background');
      return Promise.resolve(true);
    },
  });
  await joined(r);
  const tap = r.c.cameraTap();
  for (let i = 0; i < 30; i++) {
    r.clock.advance(100);
    await settle();
  }
  await tap;
  assert.ok(!r.log.some((l) => l.startsWith('capture')));
  assert.match(r.c.state.cameraExtra ?? '', /Tap the camera button again/);
});

test('the SDK reconnect names the claim it continues; a stale answer stops the camera (S2)', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  r.rooms[0]?.events?.onReconnecting();
  r.rooms[0]?.events?.onReconnected();
  await settle();
  assert.ok(r.log.includes(`claim general ${startedAt}`));
  assert.equal(r.c.state.camera.kind, 'sending');
  r.claims.push({ ok: false, failure: 'invalid', status: 409, code: 'voice_camera_stale' });
  r.rooms[0]?.events?.onReconnected();
  await settle();
  assert.deepEqual(r.c.state.camera, { kind: 'failed', reason: 'stale' });
  assert.equal(r.rooms[0]?.camera.captured, false);
  assert.equal(r.log.filter((l) => l.startsWith('claim general new')).length, 1, 'no new claim is started by itself');
});

test('the device ending the capture stops and releases; nothing captures again', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  r.rooms[0]?.events?.onCameraEnded('device');
  await settle();
  assert.deepEqual(r.c.state.camera, { kind: 'failed', reason: 'disconnected' });
  assert.ok(r.log.includes(`release general ${startedAt}`));
  r.clock.advance(60_000);
  await settle();
  assert.equal(r.log.filter((l) => l.startsWith('capture')).length, 1);
});

test('the server taking the source away stops the capture (ended)', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  r.rooms[0]?.events?.onCameraEnded('server');
  await settle();
  assert.equal(r.c.state.camera.kind, 'off');
  assert.equal(r.rooms[0]?.camera.captured, false);
});

test('an operator ending the claim, said in presence, stops the capture with the reason', async () => {
  const r = rig({ cameraEnded: { reason: 'operator', started_at: startedAt, ended_at: '2026-11-05T10:05:00Z' } });
  await joined(r);
  // The claim is the one presence names: the next poll tells the camera.
  await cameraOn(r);
  r.clock.advance(3000);
  await settle();
  assert.deepEqual(r.c.state, { ...r.c.state, camera: { kind: 'off', ended: 'operator' } });
  assert.equal(r.rooms[0]?.camera.captured, false);
});

test('leaving voice stops the capture and releases the claim before the room goes (S5)', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  await r.c.leave();
  const log = steps(r.log);
  assert.ok(log.indexOf('camera stop') < log.indexOf(`release general ${startedAt}`));
  assert.ok(log.indexOf(`release general ${startedAt}`) < log.indexOf('room disconnect'));
  assert.equal(r.c.state.camera.kind, 'off');
});

test('a removal stops and releases; another device taking over stops and leaves the claim to it', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  r.rooms[0]?.events?.onDisconnected('participant_removed');
  await settle();
  assert.ok(r.log.includes(`release general ${startedAt}`));
  assert.equal(r.c.state.camera.kind, 'off');

  const q = rig();
  await joined(q);
  await cameraOn(q);
  q.rooms[0]?.events?.onDisconnected('duplicate_identity');
  await settle();
  assert.equal(q.rooms[0]?.camera.captured, false);
  assert.ok(!q.log.some((l) => l.startsWith('release')), 'the claim is the other device\'s');
});

test('signing out (tearDown) stops the capture first', async () => {
  const r = rig();
  await joined(r);
  await cameraOn(r);
  await r.c.tearDown();
  assert.equal(r.rooms[0]?.camera.captured, false);
  assert.equal(r.c.state.phase, 'idle');
});

test('cameras are received only while the Voice view is open and shown, and only from claim holders (M-C8)', async () => {
  const r = rig({ ids: [3, 4], cameraPeers: [3] });
  await joined(r);
  const last = () => r.rooms[0]?.wants[(r.rooms[0]?.wants.length ?? 1) - 1];
  assert.equal(last()?.viewing, false, 'not open: nothing received');
  r.c.setViewing(true);
  await settle();
  assert.equal(last()?.viewing, true);
  assert.equal(last()?.shown, true);
  assert.deepEqual([...(last()?.claimed ?? [])], [3], 'only the claim holder');
  r.c.appState('background');
  assert.equal(last()?.shown, false, 'the app away: every camera is let go');
  r.c.appState('active');
  assert.equal(last()?.shown, true);
  r.c.setViewing(false);
  assert.equal(last()?.viewing, false, 'the view closed: let go');
});
