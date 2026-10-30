// The onset spec's analysis (issue #965) on levels made by hand: a reading
// every 5 ms, the tone loud where the listener would hear it. A stall of the
// page (no readings for a while) is not quiet: it must not split what was
// heard, and an edge it hides is refused rather than guessed. A unit test:
// no browser, no network.
import { describe, expect, it } from 'vitest';
import { burstMeasure, pressMeasure, spans, type Sample } from './onset';

// readings from t0 to t1, every 5 ms, loud while heard(t), and none inside
// any of the stalls.
function readings(t0: number, t1: number, heard: (t: number) => boolean, stalls: readonly [number, number][] = []): Sample[] {
  const out: Sample[] = [];
  for (let t = t0; t <= t1; t += 5) {
    if (stalls.some(([a, b]) => t > a && t < b)) continue;
    out.push({ t, rms: heard(t) ? 0.4 : 0.001 });
  }
  return out;
}

const within = (lo: number, hi: number) => (t: number) => t >= lo && t <= hi;
const either = (...fs: ((t: number) => boolean)[]) => (t: number) => fs.some((f) => f(t));

// One press as the spec makes it: the key down at 1000, the signal stopped
// at 1600 for 100 ms, the key up at 2000; heard 95 ms later, with nothing
// lost at the start and a 200 ms delay heard in full after the key.
const path = 95;
const down = 1000;
const gap = 1600;
const up = 2000;
const press = either(within(down + path, gap + path), within(gap + 100 + path, up + 200 + path));

const near = (got: number | undefined, want: number) => {
  expect(got).toBeDefined();
  expect(Math.abs((got ?? Number.NaN) - want)).toBeLessThanOrEqual(5);
};

describe('a press', () => {
  it('gives the path latency, what was lost and what was kept', () => {
    const m = pressMeasure(readings(800, 2800, press), down, up, gap);
    near(m?.path, path);
    near(m?.lost, 0);
    near(m?.kept, 200);
  });

  it('a 30 ms stall of the page while the tone is heard changes nothing', () => {
    const m = pressMeasure(readings(800, 2800, press, [[1300, 1330]]), down, up, gap);
    near(m?.path, path);
    near(m?.lost, 0);
    near(m?.kept, 200);
  });

  it('a stall over the onset, the gap or the end is refused, not guessed', () => {
    for (const stall of [[down + path - 15, down + path + 15], [gap + path - 15, gap + path + 15], [up + 200 + path - 15, up + 200 + path + 15]] as [number, number][]) {
      expect(pressMeasure(readings(800, 2800, press, [stall]), down, up, gap)).toBeNull();
    }
  });

  it('a dropout heard mid-press (three runs) is refused; a single quiet reading is a dip, not an end', () => {
    const dropout = (t: number) => press(t) && !within(1300, 1340)(t);
    expect(pressMeasure(readings(800, 2800, dropout), down, up, gap)).toBeNull();
    const dip = (t: number) => press(t) && t !== 1300;
    near(pressMeasure(readings(800, 2800, dip), down, up, gap)?.path, path);
  });

  it('a latency that cannot be one is refused', () => {
    // Heard before the signal stopped: the gap's edge is not the signal's.
    const early = either(within(down + path, gap - 300), within(gap + 100 + path, up + 200 + path));
    expect(pressMeasure(readings(800, 2800, early), down, up, gap)).toBeNull();
  });
});

describe('an open microphone burst', () => {
  const on = 1000;
  const off = 1800;
  // 30 ms of the onset lost, the rest heard 110 ms later, to the end.
  const burst = within(on + 30 + 110, off + 110);

  it('gives the path latency and what was lost at the onset, stall or not', () => {
    for (const stalls of [[], [[1400, 1440]]] as [number, number][][]) {
      const m = burstMeasure(readings(800, 3000, burst, stalls), on, off);
      near(m?.path, 110);
      near(m?.lost, 30);
    }
  });

  it('a stall over the onset or the end is refused', () => {
    expect(burstMeasure(readings(800, 3000, burst, [[on + 125, on + 160]]), on, off)).toBeNull();
    expect(burstMeasure(readings(800, 3000, burst, [[off + 95, off + 130]]), on, off)).toBeNull();
  });
});

describe('spans', () => {
  it('ends a run only at quiet that was read', () => {
    const s = spans(readings(0, 400, within(100, 300), [[150, 250]]));
    expect(s).toHaveLength(1);
    expect(s[0]).toMatchObject({ from: 100, to: 300, fromSlack: 5, toSlack: 5 });
  });
});
