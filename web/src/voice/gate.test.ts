import { describe, expect, it } from 'vitest';
import { FakeClock } from './fakes';
import { defaultTail, maxTail, TransmitGate } from './gate';

function setup(options = {}) {
  const clock = new FakeClock();
  const changes: boolean[] = [];
  const gate = new TransmitGate(clock, (open) => changes.push(open), options);
  gate.setBlocked(false);
  return { clock, gate, changes };
}

describe('push to talk', () => {
  it('opens on a press and shuts the release delay after the release', () => {
    const { clock, gate, changes } = setup();
    gate.press();
    expect(changes).toEqual([true]);
    gate.release();
    clock.advance(defaultTail - 1);
    expect(gate.open).toBe(true);
    clock.advance(1);
    expect(changes).toEqual([true, false]);
  });

  // Issue #965: the browser's mute also silences what is captured and not
  // yet sent, so its default delay is longer than conch-voice's 100 ms.
  it('by default stays open 200 ms after the release', () => {
    const { clock, gate, changes } = setup();
    gate.press();
    gate.release();
    clock.advance(199);
    expect(changes).toEqual([true]);
    clock.advance(1);
    expect(changes).toEqual([true, false]);
  });

  it('takes a new release delay from the next release, kept between 0 and the maximum', () => {
    const { clock, gate, changes } = setup();
    gate.press();
    gate.release();
    // A release already in its delay keeps the one it started with.
    gate.setTail(1000);
    clock.advance(defaultTail);
    expect(changes).toEqual([true, false]);
    gate.press();
    gate.release();
    clock.advance(999);
    expect(gate.open).toBe(true);
    clock.advance(1);
    expect(changes).toEqual([true, false, true, false]);
    gate.setTail(0);
    gate.press();
    gate.release();
    clock.advance(0);
    expect(changes).toEqual([true, false, true, false, true, false]);
    for (const [asked, kept] of [[-1, 0], [maxTail + 1, maxTail], [Number.POSITIVE_INFINITY, defaultTail]] as const) {
      gate.setTail(asked);
      expect(gate.releaseDelay).toBe(kept);
    }
  });

  it('a press during the tail keeps one transmission', () => {
    const { clock, gate, changes } = setup();
    gate.press();
    gate.release();
    clock.advance(50);
    gate.press();
    clock.advance(500);
    expect(changes).toEqual([true]);
    gate.release();
    clock.advance(defaultTail);
    expect(changes).toEqual([true, false]);
  });

  it('shuts at once when blocked, with no tail, and a held key does not reopen it', () => {
    const { gate, changes } = setup();
    gate.press();
    gate.setBlocked(true);
    expect(changes).toEqual([true, false]);
    gate.press();
    expect(changes).toEqual([true, false]);
  });

  it('a gate that was never unblocked takes no press', () => {
    const clock = new FakeClock();
    const changes: boolean[] = [];
    const gate = new TransmitGate(clock, (o) => changes.push(o));
    gate.press();
    expect(changes).toEqual([]);
  });

  it('shuts a press that lasts past the maximum, and a new press reopens it', () => {
    const { clock, gate, changes } = setup({ max: 1000 });
    gate.press();
    clock.advance(1000);
    expect(changes).toEqual([true, false]);
    gate.release();
    gate.press();
    expect(changes).toEqual([true, false, true]);
  });

  // Issue #965: with a release delay of up to 2 s, a release would leave the
  // microphone open that long after the person switched window.
  it('loss of focus shuts at once, with no tail, in the delay too; a held key needs a new press', () => {
    const { clock, gate, changes } = setup({ tail: maxTail });
    gate.press();
    gate.lost(); // what blur and hidden call
    expect(changes).toEqual([true, false]);
    gate.press();
    gate.release();
    clock.advance(10);
    gate.lost();
    expect(changes).toEqual([true, false, true, false]);
    clock.advance(maxTail);
    expect(changes).toEqual([true, false, true, false]);
    expect(clock.pending).toBe(0);
  });

  it('loss of focus leaves the open microphone open', () => {
    const { gate, changes } = setup();
    gate.setMode('open');
    gate.level(0.5);
    gate.lost();
    expect(changes).toEqual([true]);
  });

  it('a change of mode shuts the gate', () => {
    const { gate, changes } = setup();
    gate.press();
    gate.setMode('open');
    expect(changes).toEqual([true, false]);
    gate.press();
    expect(changes).toEqual([true, false]);
  });
});

describe('open microphone', () => {
  const levels: [string, number[], boolean[]][] = [
    ['quiet never opens', [0.01, 0.02, 0.0], []],
    ['speech opens', [0.01, 0.3], [true]],
    ['a dip shorter than the hang does not close', [0.3, 0.01, 0.3], [true]],
  ];
  it.each(levels)('%s', (_name, input, want) => {
    const { clock, gate, changes } = setup({ hang: 400 });
    gate.setMode('open');
    gate.setThreshold(0.1);
    for (const v of input) {
      gate.level(v);
      clock.advance(100);
    }
    expect(changes).toEqual(want);
  });

  it('closes after the hang time below the threshold', () => {
    const { clock, gate, changes } = setup({ hang: 400 });
    gate.setMode('open');
    gate.level(0.5);
    gate.level(0.0);
    clock.advance(399);
    expect(gate.open).toBe(true);
    clock.advance(1);
    expect(changes).toEqual([true, false]);
  });

  it('ignores levels when muted (blocked) and in push-to-talk', () => {
    const { gate, changes } = setup();
    gate.level(0.9);
    expect(changes).toEqual([]);
    gate.setMode('open');
    gate.setBlocked(true);
    gate.level(0.9);
    expect(changes).toEqual([]);
  });
});
