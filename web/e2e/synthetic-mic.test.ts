// The tone analysis of 65-who-hears-whom (issue #1610) on levels made by hand:
// what counts as heard, the floor, and the table a failure attaches. A unit
// test: no browser, no network.
import { describe, expect, it } from 'vitest';
import { floorOf, heardBy, table, tracksAt, type TrackLevel } from './synthetic-mic';

const track = (db: number[], live = true, streamId = 'PA_x|TR_y'): TrackLevel => ({ streamId, live, db });

describe('floorOf', () => {
  it('is the highest level of any live track at any frequency', () => {
    expect(floorOf([track([-90, -70, -100]), track([-60, -95, -95]), track([-10, -10, -10], false)])).toBe(-60);
  });
  it('is never below the lowest it is allowed, and is -120 for nothing at all', () => {
    expect(floorOf([track([-120, -120, -120])], -70)).toBe(-70);
    expect(floorOf([], -70)).toBe(-70);
    expect(floorOf([])).toBe(-120);
  });
});

describe('heardBy', () => {
  const tracks = [track([-18, -91, -94]), track([-120, -120, -120]), track([-18, -18, -18], false)];
  it('lists, for each live track, the frequencies more than the margin above the floor', () => {
    expect(heardBy(tracks, -70, 20)).toEqual([[0], []]);
  });
  it('does not count a level exactly at floor plus margin', () => {
    expect(heardBy([track([-50, -49.9, -51])], -70, 20)).toEqual([[1]]);
  });
  it('hears two tones on one track', () => {
    expect(heardBy([track([-18, -20, -100])], -70, 20)).toEqual([[0, 1]]);
  });
});

describe('tracksAt', () => {
  it('counts the live tracks heard at one frequency', () => {
    const tracks = [track([-18, -100, -100]), track([-18, -100, -100]), track([-100, -18, -100]), track([-18, -18, -18], false)];
    expect(tracksAt(tracks, -70, 20, 0)).toBe(2);
    expect(tracksAt(tracks, -70, 20, 1)).toBe(1);
    expect(tracksAt(tracks, -70, 20, 2)).toBe(0);
  });
});

describe('table', () => {
  it('names levels, floor and margin, and nothing else', () => {
    const out = table('alice', [track([-18.4, -91, -120]), track([-120, -120, -120], false)], [450, 650, 950], -70, 20);
    expect(out).toBe(
      ['alice: floor -70 dB, margin 20 dB', '  track 0 live 450 Hz -18 dB, 650 Hz -91 dB, 950 Hz -120 dB', '  track 1 ended 450 Hz -120 dB, 650 Hz -120 dB, 950 Hz -120 dB'].join('\n'),
    );
    expect(out).not.toContain('PA_');
  });
});
