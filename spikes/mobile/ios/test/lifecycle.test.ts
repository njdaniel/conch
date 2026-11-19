// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { Lifecycle } from '../src/logic/lifecycle';

function rig() {
  const log: string[] = [];
  const life = new Lifecycle(
    { lifecycleBlock: () => log.push('gate block'), lifecycleUnblock: () => log.push('gate unblock') },
    { lifecycleBlock: () => (log.push('camera block'), Promise.resolve()), lifecycleUnblock: () => log.push('camera unblock') },
  );
  return { log, life };
}

test('each reason blocks the gate first and the camera second', () => {
  const r = rig();
  r.life.screenLocked(true);
  assert.deepEqual(r.log, ['gate block', 'camera block']);
});

test('the blocks lift only when the last reason ends', () => {
  const r = rig();
  r.life.appState('background');
  r.life.interrupted(true);
  r.life.appState('active');
  assert.equal(r.life.blocked, true);
  assert.ok(!r.log.includes('gate unblock'));
  r.life.interrupted(false);
  assert.equal(r.life.blocked, false);
  assert.deepEqual(r.log.slice(-2), ['gate unblock', 'camera unblock']);
});

test('iOS inactive is a reason of its own, and background replaces it', () => {
  const r = rig();
  r.life.appState('inactive');
  assert.deepEqual(r.life.reasons, ['inactive']);
  r.life.appState('background');
  assert.deepEqual(r.life.reasons, ['background']);
  r.life.appState('active');
  assert.deepEqual(r.life.reasons, []);
});

test('a repeated event changes nothing', () => {
  const r = rig();
  r.life.interrupted(true);
  r.life.interrupted(true);
  r.life.interrupted(false);
  r.life.interrupted(false);
  assert.deepEqual(r.log, ['gate block', 'camera block', 'gate unblock', 'camera unblock']);
});
