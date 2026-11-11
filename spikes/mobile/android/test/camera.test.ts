/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import type { CameraClaimResult } from '../src/logic/api';
import { CameraClient, cameraOff, endNote, type CameraApi, type CameraRoom, type CameraState, type KeptCamera } from '../src/logic/camera';
import { Lifecycle, type LifecycleReason } from '../src/logic/lifecycle';
import { TransmitGate } from '../src/logic/gate';
import { wanted, type TrackKind, type TrackSource } from '../src/logic/subscribe';
import type { VoiceCameraEndReason } from '../src/logic/schema/generated';
import { FakeClock, settle } from './fakes';

const startedAt = '2026-11-05T10:00:00Z';
const ok: CameraClaimResult = { ok: true, value: { principal_id: 7, started_at: startedAt } };

// A fake camera and room: every call is logged in order, with the fake time.
class Rig {
  readonly clock = new FakeClock();
  readonly log: string[] = [];
  readonly states: CameraState[] = [];
  claims: CameraClaimResult[] = [];
  captureError: unknown = null;
  captureImpl: (() => Promise<void>) | null = null;
  grant: Promise<void> | null = null;
  captured = false;
  stoppedAt: number[] = [];
  releaseAt: number[] = [];

  readonly api: CameraApi = {
    claim: (channel, named) => {
      this.log.push(`claim ${channel} ${named ?? 'new'}`);
      return Promise.resolve(this.claims.shift() ?? ok);
    },
    release: (channel, named) => {
      this.log.push(`release ${channel} ${named}`);
      this.releaseAt.push(this.clock.now);
      return Promise.resolve();
    },
  };

  readonly room: CameraRoom = {
    captureCamera: (choice, profile) => {
      this.log.push(`capture ${profile ?? 'none'}`);
      if (this.captureImpl !== null) return this.captureImpl();
      if (this.captureError !== null) return Promise.reject(this.captureError);
      this.captured = true;
      return Promise.resolve();
    },
    awaitCameraGrant: () => {
      this.log.push('grant');
      return this.grant ?? Promise.resolve();
    },
    publishCamera: () => {
      this.log.push('publish');
      return Promise.resolve();
    },
    stopCamera: () => {
      this.log.push('stop capture');
      this.captured = false;
      this.stoppedAt.push(this.clock.now);
      return Promise.resolve();
    },
    keepCamera: (): KeptCamera | null => {
      this.log.push('keep');
      return null;
    },
    adoptCamera: () => this.log.push('adopt'),
    cameraInUse: () => (this.captured ? 'front' : null),
    switchCamera: () => Promise.resolve(),
  };

  readonly client = new CameraClient(this.api, (s) => this.states.push(s), this.clock);

  get captures(): number {
    return this.log.filter((l) => l.startsWith('capture')).length;
  }
  get claimsSent(): string[] {
    return this.log.filter((l) => l.startsWith('claim'));
  }
}

async function sending(r: Rig): Promise<void> {
  await r.client.start('general', r.room, null);
  assert.equal(r.client.current.kind, 'sending');
}

test('the button sends one un-named claim, in the order capture, claim, grant, publish', async () => {
  const r = new Rig();
  await sending(r);
  assert.deepEqual(r.log, ['capture phone', 'claim general new', 'grant', 'publish']);
});

test('start does nothing unless the camera is off', async () => {
  const r = new Rig();
  await sending(r);
  await r.client.start('general', r.room, null);
  assert.equal(r.captures, 1);
  assert.equal(r.claimsSent.length, 1);
});

const reasons: readonly LifecycleReason[] = ['background', 'inactive', 'lock', 'interruption', 'stopped'];

for (const reason of reasons) {
  test(`${reason} with the camera sending: capture stops at once, then DELETE names the claim`, async () => {
    const r = new Rig();
    await sending(r);
    const gate = new TransmitGate(r.clock, () => undefined);
    const life = new Lifecycle(gate, r.client);
    r.log.length = 0;
    const t0 = r.clock.now;
    if (reason === 'background' || reason === 'inactive') life.appState(reason);
    else if (reason === 'lock') life.screenLocked(true);
    else if (reason === 'stopped') life.activityStopped(true);
    else life.interrupted(true);
    // Synchronously, before anything is awaited: the capture is stopped.
    assert.deepEqual(r.log, ['stop capture']);
    await life.settled();
    assert.deepEqual(r.log, ['stop capture', `release general ${startedAt}`]);
    assert.ok(r.stoppedAt[0] !== undefined && r.stoppedAt[0] - t0 < 1000, 'within the 1 s bar');
    assert.deepEqual(r.client.current, { kind: 'off', ended: 'lifecycle' });
    assert.equal(r.clock.pending, 0, 'no timer left');
  });
}

test('every state stops on a lifecycle event: capturing, claiming, waiting for the grant', async () => {
  // capturing: the OS is still being asked.
  {
    const r = new Rig();
    let finish: () => void = () => undefined;
    r.captureImpl = () =>
      new Promise((resolve) => {
        finish = () => {
          r.captured = true;
          resolve();
        };
      });
    const started = r.client.start('general', r.room, null);
    await settle();
    assert.equal(r.client.current.kind, 'capturing');
    const down = r.client.lifecycleBlock();
    finish();
    await started;
    await down;
    assert.equal(r.captured, false, 'what the OS gave is let go');
    assert.equal(r.claimsSent.length, 0, 'no claim after the event');
  }
  // waiting for the grant.
  {
    const r = new Rig();
    r.grant = new Promise(() => undefined);
    void r.client.start('general', r.room, null);
    await settle();
    assert.equal(r.client.current.kind, 'granted-waiting');
    await r.client.lifecycleBlock();
    assert.equal(r.captured, false);
    assert.ok(r.log.includes(`release general ${startedAt}`));
    assert.ok(!r.log.includes('publish'));
  }
});

test('while the block holds, the button starts nothing; after it, only the button does', async () => {
  const r = new Rig();
  await r.client.lifecycleBlock();
  await r.client.start('general', r.room, null);
  assert.equal(r.captures, 0);
  assert.deepEqual(r.claimsSent, []);
  r.client.lifecycleUnblock();
  assert.equal(r.captures, 0, 'unblocking captures nothing');
  assert.deepEqual(r.claimsSent, []);
  await r.client.start('general', r.room, null);
  assert.equal(r.captures, 1);
  assert.deepEqual(r.claimsSent, ['claim general new']);
});

test('resume, reconnect and the end of an interruption never capture or claim', async () => {
  const r = new Rig();
  await sending(r);
  const life = new Lifecycle(new TransmitGate(r.clock, () => undefined), r.client);
  life.appState('background');
  life.interrupted(true);
  await life.settled();
  const before = r.log.length;
  life.appState('active');
  life.interrupted(false);
  life.screenLocked(false);
  r.client.connected('general', r.room);
  await r.client.reconnected();
  r.client.presence(undefined);
  r.client.reset();
  await settle();
  r.clock.advance(120_000);
  await settle();
  assert.deepEqual(r.log.slice(before), [], 'nothing was asked of the camera or conchd');
  assert.equal(r.captures, 1);
  assert.deepEqual(r.client.current, cameraOff);
});

test('a second lifecycle event while stopping joins the stop and releases once', async () => {
  const r = new Rig();
  await sending(r);
  const a = r.client.lifecycleBlock();
  const b = r.client.lifecycleBlock();
  await Promise.all([a, b]);
  assert.equal(r.log.filter((l) => l.startsWith('release')).length, 1);
});

test('409 voice_camera_stale on a named continue stops the capture', async () => {
  const r = new Rig();
  await sending(r);
  r.claims.push({ ok: false, failure: 'invalid', status: 409, code: 'voice_camera_stale' });
  r.log.length = 0;
  await r.client.reconnected();
  assert.equal(r.log[0], `claim general ${startedAt}`, 'a continue names the claim');
  assert.ok(r.log.includes('stop capture'));
  assert.deepEqual(r.client.current, { kind: 'failed', reason: 'stale' });
  assert.equal(r.captured, false);
});

const endReasons: readonly VoiceCameraEndReason[] = ['self', 'operator', 'left', 'access_lost', 'idle', 'rotated'];

for (const reason of endReasons) {
  test(`camera_ended ${reason}`, async () => {
    const r = new Rig();
    await sending(r);
    r.client.presence({ reason, started_at: startedAt, ended_at: '2026-11-05T10:01:00Z' });
    await settle();
    if (reason === 'rotated') {
      // The room's renewal alone is resumed (no connection kept it here, so
      // the capture is given up, never restarted).
      assert.equal(r.captures, 1, 'nothing captures again');
    } else {
      assert.equal(r.captured, false, 'the capture stops');
      assert.deepEqual(r.client.current, { kind: 'off', ended: reason });
      assert.equal(r.log.filter((l) => l.startsWith('release')).length, 0, 'the server ended it: nothing to release');
    }
  });
}

test('a denied capture sends no claim', async () => {
  const r = new Rig();
  r.captureError = { name: 'NotAllowedError' };
  await r.client.start('general', r.room, null);
  assert.deepEqual(r.claimsSent, []);
  assert.deepEqual(r.client.current, { kind: 'failed', reason: 'blocked' });
});

test('permission_denied on the claim: no capture is left, "Camera is off for you"', async () => {
  const r = new Rig();
  r.claims.push({ ok: false, failure: 'invalid', status: 403, code: 'permission_denied' });
  await r.client.start('general', r.room, null);
  assert.deepEqual(r.client.current, { kind: 'failed', reason: 'not_allowed' });
  assert.equal(r.captured, false);
  assert.ok(!r.log.includes('publish'));
});

test('device loss falls back to nothing: no other camera is captured', async () => {
  const r = new Rig();
  await sending(r);
  r.log.length = 0;
  r.client.ended('device');
  await settle();
  assert.deepEqual(r.client.current, { kind: 'failed', reason: 'disconnected' });
  assert.ok(!r.log.some((l) => l.startsWith('capture')));
});

test('the lifecycle end has a note that says why', () => {
  assert.match(endNote('lifecycle') ?? '', /left the screen/);
});

const sources: readonly TrackSource[] = ['microphone', 'camera', 'screen_share', 'screen_share_audio', 'unknown'];
const kinds: readonly (TrackKind | undefined)[] = ['audio', 'video', undefined];
const all = { viewing: true, shown: true, claimed: true } as const;

for (const source of sources) {
  for (const kind of kinds) {
    const want = (source === 'microphone' && kind === 'audio') || (source === 'camera' && kind === 'video');
    test(`wanted(${source}, ${String(kind)}) is ${String(want)}`, () => {
      assert.equal(wanted(source, kind, all), want);
    });
  }
}

test('a camera is wanted only while viewed, shown and claimed', () => {
  assert.equal(wanted('camera', 'video', { viewing: false, shown: true, claimed: true }), false);
  assert.equal(wanted('camera', 'video', { viewing: true, shown: false, claimed: true }), false);
  assert.equal(wanted('camera', 'video', { viewing: true, shown: true, claimed: false }), false);
});
