// iOS-specific cases for the spike (#1394, M5-B): inactive, interruption with "should resume", route change.
/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { TransmitGate, defaultHang, phoneMax, type GateMode } from '../src/logic/gate';
import { IosAudioEvents } from '../src/logic/ios';
import { Lifecycle } from '../src/logic/lifecycle';
import { FakeClock } from './fakes';

// A gate and a lifecycle with a recording camera, wired the way the app will.
function rig(mode: GateMode) {
  const clock = new FakeClock();
  const changes: boolean[] = [];
  const gate = new TransmitGate(clock, (open) => changes.push(open), { max: phoneMax });
  gate.setMode(mode);
  gate.setBlocked(false);
  const camera: string[] = [];
  const life = new Lifecycle(gate, {
    lifecycleBlock: () => (camera.push('block'), Promise.resolve()),
    lifecycleUnblock: () => camera.push('unblock'),
  });
  const ios = new IosAudioEvents(life, gate);
  return { clock, changes, gate, camera, life, ios };
}

// settleOpen drives the open microphone through a settled latch to an open gate.
function openUp(r: ReturnType<typeof rig>): void {
  r.gate.level(0);
  r.clock.advance(defaultHang);
  r.gate.level(0.9);
  assert.equal(r.gate.open, true);
}

for (const mode of ['ptt', 'open'] as const) {
  test(`inactive shuts the ${mode} gate with no tail and ends the camera claim`, () => {
    const r = rig(mode);
    if (mode === 'ptt') r.gate.press();
    else openUp(r);
    assert.equal(r.gate.open, true);
    r.life.appState('inactive');
    assert.equal(r.gate.open, false, 'shut at once');
    assert.equal(r.clock.pending, 0, 'no tail left');
    assert.deepEqual(r.camera, ['block']);
    r.gate.press();
    r.gate.level(0.9);
    assert.equal(r.gate.open, false, 'nothing opens while inactive');
  });

  test(`${mode}: leaving inactive opens nothing`, () => {
    const r = rig(mode);
    if (mode === 'ptt') r.gate.press();
    else openUp(r);
    r.life.appState('inactive');
    r.life.appState('active');
    assert.equal(r.gate.open, false);
    r.gate.level(0.9);
    assert.equal(r.gate.open, false, 'loud sound straight after is latched');
    assert.deepEqual(r.camera, ['block', 'unblock']);
  });

  test(`${mode}: an interruption ended with shouldResume opens nothing`, () => {
    const r = rig(mode);
    if (mode === 'ptt') r.gate.press();
    else openUp(r);
    r.ios.interruption({ type: 'began' });
    assert.equal(r.gate.open, false);
    r.ios.interruption({ type: 'ended', shouldResume: true });
    assert.equal(r.life.blocked, false);
    assert.equal(r.gate.open, false);
    r.gate.level(0.9);
    assert.equal(r.gate.open, false, 'no onset without quiet first');
    assert.deepEqual(r.camera, ['block', 'unblock'], 'the camera is stopped and not restarted by the hint');
    assert.equal(r.ios.delivered[1], 'interruption ended shouldResume=true');
  });
}

test('shouldResume true or false behaves the same', () => {
  for (const shouldResume of [true, false, undefined]) {
    const r = rig('ptt');
    r.gate.press();
    r.ios.interruption({ type: 'began' });
    r.ios.interruption({ type: 'ended', shouldResume });
    assert.equal(r.gate.open, false);
    assert.deepEqual(r.changes, [true, false]);
  }
});

test('open mode: after an interruption, quiet and then an onset opens again', () => {
  const r = rig('open');
  openUp(r);
  r.ios.interruption({ type: 'began' });
  r.ios.interruption({ type: 'ended', shouldResume: true });
  r.gate.level(0);
  r.clock.advance(defaultHang);
  r.gate.level(0.9);
  assert.equal(r.gate.open, true);
});

test('a route change while held shuts the gate at once, and a new press opens', () => {
  const r = rig('ptt');
  r.gate.press();
  r.ios.routeChange({ reason: 'oldDeviceUnavailable' });
  assert.equal(r.gate.open, false);
  assert.equal(r.clock.pending, 0, 'no tail');
  assert.equal(r.life.blocked, false, 'it is not a lifecycle reason');
  assert.deepEqual(r.camera, [], 'the camera is left alone');
  r.gate.release();
  assert.equal(r.gate.open, false, 'the old release opens nothing');
  r.gate.press();
  assert.equal(r.gate.open, true, 'a new press opens');
});

test('a route change in open mode shuts the gate and the latch waits for quiet', () => {
  const r = rig('open');
  openUp(r);
  r.ios.routeChange({ reason: 'oldDeviceUnavailable' });
  assert.equal(r.gate.open, false);
  r.gate.level(0.9);
  assert.equal(r.gate.open, false, 'sound already there opens nothing');
  r.gate.level(0);
  r.clock.advance(defaultHang);
  r.gate.level(0.9);
  assert.equal(r.gate.open, true);
});

test('a route change during a lifecycle block does not lift it', () => {
  const r = rig('ptt');
  r.life.appState('inactive');
  r.ios.routeChange({ reason: 'oldDeviceUnavailable' });
  assert.equal(r.gate.lifecycleBlocked, true);
  r.gate.press();
  assert.equal(r.gate.open, false);
});

test('the events iOS delivered are recorded for the receipt', () => {
  const r = rig('ptt');
  r.ios.interruption({ type: 'began' });
  r.ios.interruption({ type: 'ended', shouldResume: false });
  r.ios.routeChange({ reason: 'newDeviceAvailable' });
  assert.deepEqual(r.ios.delivered, ['interruption began', 'interruption ended shouldResume=false', 'route change newDeviceAvailable']);
});

