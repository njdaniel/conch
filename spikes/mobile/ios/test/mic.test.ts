// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { Transmitter, publishMicrophone, reportWait, type Mutable } from '../src/logic/mic';
import { TransmitGate, defaultTail } from '../src/logic/gate';
import { FakeClock, settle } from './fakes';
import type { VoiceTransmitState } from '../src/logic/schema/generated';

test('publish order: muted before publishing, again after, and again after a republish', async () => {
  const log: string[] = [];
  const pub: Mutable = { mute: () => (log.push('mute publication'), Promise.resolve()) };
  const track: Mutable = { mute: () => (log.push('mute track'), Promise.resolve()) };
  const publisher = { publish: () => (log.push('publish'), Promise.resolve(pub)) };
  await publishMicrophone(track, publisher);
  await publishMicrophone(track, publisher); // a forced republish
  assert.deepEqual(log, ['mute track', 'publish', 'mute publication', 'mute track', 'publish', 'mute publication']);
});

function rig() {
  const clock = new FakeClock();
  const log: string[] = [];
  const dones: (() => void)[] = [];
  const t = new Transmitter({
    clock,
    reports: {
      push: (state: VoiceTransmitState, done?: () => void) => {
        log.push(`report ${state}`);
        if (done !== undefined) dones.push(done);
      },
    },
    setSending: (on) => {
      log.push(on ? 'unmute' : 'mute');
      return Promise.resolve();
    },
  });
  return { clock, log, dones, t };
}

test('started is answered before the unmute; stopped is sent after the mute', async () => {
  const r = rig();
  r.t.onGate(true);
  await settle();
  assert.deepEqual(r.log, ['report started'], 'the unmute waits for the answer');
  r.dones.forEach((d) => d());
  await settle();
  assert.deepEqual(r.log, ['report started', 'unmute']);
  r.t.onGate(false);
  await r.t.idle();
  assert.deepEqual(r.log, ['report started', 'unmute', 'mute', 'report stopped']);
});

test('a started report that is never answered is waited out after reportWait', async () => {
  const r = rig();
  r.t.onGate(true);
  await settle();
  r.clock.advance(reportWait);
  await settle();
  assert.deepEqual(r.log, ['report started', 'unmute']);
});

test('a gate that shut while started was awaited never unmutes', async () => {
  const r = rig();
  r.t.onGate(true);
  await settle();
  r.t.onGate(false);
  r.dones.forEach((d) => d());
  await r.t.idle();
  assert.ok(!r.log.includes('unmute'));
  assert.equal(r.log[r.log.length - 1], 'report stopped');
});

test('a lifecycle block sends stopped after the mute, with no tail', async () => {
  const clock = new FakeClock();
  const log: string[] = [];
  const t = new Transmitter({
    clock,
    reports: { push: (s, done) => (log.push(`report ${s}`), done?.()) },
    setSending: (on) => (log.push(on ? 'unmute' : 'mute'), Promise.resolve()),
  });
  const gate = new TransmitGate(clock, (open) => t.onGate(open));
  gate.setBlocked(false);
  gate.press();
  await settle();
  assert.deepEqual(log, ['report started', 'unmute']);
  gate.lifecycleBlock();
  await t.idle();
  assert.deepEqual(log, ['report started', 'unmute', 'mute', 'report stopped']);
  assert.ok(clock.now < defaultTail);
});

test('reset abandons a connection that has gone', async () => {
  const r = rig();
  r.t.onGate(true);
  r.t.reset();
  await settle();
  r.dones.forEach((d) => d());
  await settle();
  assert.ok(!r.log.includes('unmute'));
});
