// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
/// <reference types="node" />
import test from 'node:test';
import assert from 'node:assert/strict';
import { TransmitGate, defaultHang, defaultTail, phoneMax, type GateMode } from '../src/logic/gate';
import { FakeClock } from './fakes';

function rig(mode: GateMode = 'ptt') {
  const clock = new FakeClock();
  const changes: boolean[] = [];
  const gate = new TransmitGate(clock, (open) => changes.push(open), { max: phoneMax });
  gate.setMode(mode);
  gate.setBlocked(false);
  return { clock, changes, gate };
}

// quietThenOnset drives the open microphone through a settled latch and an
// onset: quiet for the hang time, then one loud sample.
function quietThenOnset(r: ReturnType<typeof rig>): void {
  r.gate.level(0);
  r.clock.advance(defaultHang);
  r.gate.level(0.9);
}

test('press, release and tail', () => {
  const r = rig();
  r.gate.press();
  assert.equal(r.gate.open, true);
  r.gate.release();
  assert.equal(r.gate.open, true, 'open during the tail');
  r.clock.advance(defaultTail - 1);
  assert.equal(r.gate.open, true);
  r.clock.advance(1);
  assert.equal(r.gate.open, false);
  assert.deepEqual(r.changes, [true, false]);
});

test('touch cancel shuts at once, with no tail and no timer left', () => {
  const r = rig();
  r.gate.press();
  r.gate.cancel();
  assert.equal(r.gate.open, false);
  assert.equal(r.clock.pending, 0);
  assert.deepEqual(r.changes, [true, false]);
});

test('the 30 s bound shuts a press whose release never came', () => {
  assert.equal(phoneMax, 30_000);
  const r = rig();
  r.gate.press();
  r.clock.advance(29_999);
  assert.equal(r.gate.open, true);
  r.clock.advance(1);
  assert.equal(r.gate.open, false);
  r.gate.release();
  assert.equal(r.gate.open, false, 'a late release opens nothing');
  r.gate.press();
  assert.equal(r.gate.open, true, 'a new press opens');
});

const blockers: readonly { name: string; mode: GateMode }[] = [
  { name: 'background', mode: 'ptt' },
  { name: 'background', mode: 'open' },
  { name: 'inactive', mode: 'ptt' },
  { name: 'inactive', mode: 'open' },
  { name: 'lock', mode: 'ptt' },
  { name: 'lock', mode: 'open' },
  { name: 'interruption', mode: 'ptt' },
  { name: 'interruption', mode: 'open' },
];

for (const b of blockers) {
  test(`lifecycle block (${b.name}) shuts the gate in ${b.mode} mode at once, with no tail`, () => {
    const r = rig(b.mode);
    if (b.mode === 'ptt') r.gate.press();
    else quietThenOnset(r);
    assert.equal(r.gate.open, true);
    r.gate.lifecycleBlock();
    assert.equal(r.gate.open, false);
    assert.equal(r.clock.pending, 0);
    // Held down or still loud: nothing opens while the block holds.
    r.gate.press();
    r.gate.level(0.9);
    assert.equal(r.gate.open, false);
  });
}

test('resume: unblocking opens nothing, and a press is new', () => {
  const r = rig();
  r.gate.press();
  r.gate.lifecycleBlock();
  r.gate.lifecycleUnblock();
  assert.equal(r.gate.open, false);
  r.gate.release();
  assert.equal(r.gate.open, false);
  r.gate.press();
  assert.equal(r.gate.open, true);
});

test('resume latch: sustained sound across an unblock opens nothing until quiet for the hang time and then an onset', () => {
  const r = rig('open');
  quietThenOnset(r);
  assert.equal(r.gate.open, true);
  r.gate.lifecycleBlock();
  r.gate.lifecycleUnblock();
  // Sound goes on throughout.
  for (let i = 0; i < 200; i++) {
    r.gate.level(0.9);
    r.clock.advance(10);
  }
  assert.equal(r.gate.open, false, 'sustained sound opens nothing');
  // Quiet, but not long enough: a sound restarts the wait.
  r.gate.level(0);
  r.clock.advance(defaultHang - 1);
  r.gate.level(0.9);
  assert.equal(r.gate.open, false);
  r.clock.advance(100);
  r.gate.level(0);
  r.clock.advance(defaultHang - 1);
  r.gate.level(0.9);
  assert.equal(r.gate.open, false, 'one ms short of the hang time');
  r.clock.advance(1);
  r.gate.level(0);
  r.clock.advance(defaultHang);
  assert.equal(r.gate.open, false, 'quiet alone opens nothing');
  r.gate.level(0.9);
  assert.equal(r.gate.open, true, 'a fresh onset after quiet opens');
});

test('resume latch: PTT to open mode under sound behaves the same', () => {
  const r = rig('ptt');
  r.gate.setMode('open');
  r.gate.level(0.9);
  r.clock.advance(5000);
  r.gate.level(0.9);
  assert.equal(r.gate.open, false);
  quietThenOnset(r);
  assert.equal(r.gate.open, true);
});

test('resume latch: a block and an unblock of the connection (muted, then unmuted) arms it too', () => {
  const r = rig('open');
  quietThenOnset(r);
  r.gate.setBlocked(true);
  assert.equal(r.gate.open, false);
  r.gate.setBlocked(false);
  r.gate.level(0.9);
  assert.equal(r.gate.open, false, 'sound at the unmute opens nothing');
  quietThenOnset(r);
  assert.equal(r.gate.open, true);
});

test('a new gate in open mode does not open on the first loud sample', () => {
  const r = rig('open');
  r.gate.level(0.9);
  assert.equal(r.gate.open, false);
});

test('the open microphone shuts after the hang time once the level falls', () => {
  const r = rig('open');
  quietThenOnset(r);
  r.gate.level(0);
  r.clock.advance(defaultHang);
  assert.equal(r.gate.open, false);
});

test('the OS "should resume" hint opens nothing (lifecycle end, either answer)', () => {
  for (const mode of ['ptt', 'open'] as const) {
    const r = rig(mode);
    r.gate.lifecycleBlock();
    // The OS says shouldResume: the app lifts the block and nothing else.
    r.gate.lifecycleUnblock();
    r.gate.level(0.9);
    assert.equal(r.gate.open, false, mode);
  }
});
