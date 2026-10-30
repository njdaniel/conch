// The analysis behind 32-voice-onset.spec.ts (issue #965): when the listener
// heard the talker's signal, from the levels its page read, and what that
// says of the speech lost at the edges of a transmission. Pure functions, so
// a unit test (onset.test.ts) can feed them levels made up by hand.
//
// The page reads every 5 ms, but a busy machine can stall it for tens of ms.
// A stall is a stretch with no reading: it is not quiet. So a run of loud
// readings ends only at quiet that was read (at least minQuiet ms of it),
// never at a gap; and an edge is only as precise as the readings on either
// side of it. A measurement whose edges are less precise than maxSlack, or
// whose spans are not the ones the signal makes, is refused (null), and the
// spec measures again.

export interface Sample {
  readonly t: number;
  readonly rms: number;
}

// A run of loud readings: the first and last loud reading, and how far each
// is from the reading before or after it (the edge lies somewhere in there).
export interface Span {
  readonly from: number;
  readonly to: number;
  readonly fromSlack: number;
  readonly toSlack: number;
}

// The level above which the tone is heard (it reads about 0.4), the quiet
// that ends a run, and the most an edge may be uncertain by.
export const loudLevel = 0.05;
export const minQuiet = 10;
export const maxSlack = 15;
// A path latency outside this is not one: the clocks or the readings are off.
const pathRange = { min: 0, max: 1000 };

// spans finds the runs of loud readings in samples, which are in time order.
export function spans(samples: readonly Sample[]): Span[] {
  const out: Span[] = [];
  // The run being read: its first loud reading and how precise that edge
  // is, and the index of its last loud reading.
  let from = 0;
  let fromSlack = 0;
  let last = -1;
  // Where the quiet read since the run's last loud reading began, if any.
  let quietFrom: number | null = null;
  const close = () => {
    const end = samples[last];
    const next = samples[last + 1];
    if (end !== undefined) out.push({ from, fromSlack, to: end.t, toSlack: next === undefined ? Number.POSITIVE_INFINITY : next.t - end.t });
    last = -1;
    quietFrom = null;
  };
  for (let i = 0; i < samples.length; i++) {
    const s = samples[i];
    if (s === undefined) continue;
    if (s.rms >= loudLevel) {
      if (last < 0) {
        const prev = samples[i - 1];
        from = s.t;
        fromSlack = prev === undefined ? Number.POSITIVE_INFINITY : s.t - prev.t;
      }
      last = i;
      // Quiet shorter than minQuiet inside a run is a dip, not its end.
      quietFrom = null;
      continue;
    }
    if (last < 0) continue;
    quietFrom ??= s.t;
    // At least two quiet readings, minQuiet ms apart: the run has ended.
    if (s.t - quietFrom >= minQuiet) close();
  }
  if (last >= 0) close();
  return out;
}

const precise = (s: Span, edges: readonly ('from' | 'to')[]) => edges.every((e) => (e === 'from' ? s.fromSlack : s.toSlack) <= maxSlack);
const pathOk = (p: number) => p >= pathRange.min && p <= pathRange.max;

// pressMeasure is one push-to-talk press: the key down at down and up at up,
// the signal on throughout but for a stop at gap (100 ms, while the key is
// held). The listener hears two runs: down to the gap, and the gap to the
// end. The end of the first, less gap, is the path latency; with it, what
// was lost after the key went down and what was heard after it came up.
export function pressMeasure(samples: readonly Sample[], down: number, up: number, gap: number): { path: number; lost: number; kept: number } | null {
  const ss = spans(samples).filter((s) => s.from >= down && s.from < up + 700);
  const first = ss[0];
  const last = ss[1];
  if (ss.length !== 2 || first === undefined || last === undefined) return null;
  if (!precise(first, ['from', 'to']) || !precise(last, ['to'])) return null;
  const path = first.to - gap;
  if (!pathOk(path)) return null;
  return { path, lost: first.from - down - path, kept: last.to - path - up };
}

// burstMeasure is one burst of the open microphone: the signal on at on and
// off at off, from quiet. The gate is still open (its hang) when the signal
// stops, so the end of what was heard, less off, is the path latency; with
// it, what was lost at the onset.
export function burstMeasure(samples: readonly Sample[], on: number, off: number): { path: number; lost: number } | null {
  const ss = spans(samples).filter((s) => s.from >= on && s.from < off + 1000);
  const heard = ss[0];
  if (ss.length !== 1 || heard === undefined || !precise(heard, ['from', 'to'])) return null;
  const path = heard.to - off;
  if (!pathOk(path)) return null;
  return { path, lost: heard.from - on - path };
}

export function median(xs: readonly number[]): number {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.floor(s.length / 2)] ?? Number.NaN;
}
