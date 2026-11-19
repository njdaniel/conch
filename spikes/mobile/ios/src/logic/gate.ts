// Copied from spikes/mobile/android at cc5fab02 (#1390, M4-B, with its review fixes) for the iOS spike (#1394, M5-B).
// Copied from web/src/voice/gate.ts at ed4a68f9 and adapted for the phone (#1109, M4-B).
// Differences are listed in the "Phone" notes below; the rest is the browser's.
//
// Phone (ADR-016 Decision 5, design section 4):
//  - the bound on one press is 30 s (phoneMax), not the browser's 120 s;
//  - the lifecycle block (lifecycleBlock / lifecycleUnblock) shuts the gate at
//    once, in BOTH modes and with no tail, on background, inactive, lock and
//    interruption, and a press is ignored while it holds;
//  - the resume latch: after any block, and after a change to open mode, the
//    open microphone needs quiet for the hang time and then a fresh onset
//    before it may open. Sound that is already there when the block lifts
//    opens nothing;
//  - cancel() is a touch cancel: it shuts at once, with no tail;
//  - nothing the OS says on resume (its "should resume" hint) opens the gate.
// The transmit gate: the one place that decides whether the microphone track
// is sending. Microphone audio leaves the page only while the gate is open.
// It opens on a press (push-to-talk) or on speech (open microphone) and shuts
// on a release after a short tail; at once on loss of focus, or when blocked
// (muted, deafened, not connected); or when a single press has lasted too
// long.
//
// It owns no timer of its own: the host gives it the clock, so the tests drive
// it by hand.
import type { Clock } from './clock';

export type GateMode = 'ptt' | 'open';

export interface GateOptions {
  // How long the gate stays open after a release, so the last syllable is not
  // cut (conch-voice.md section 3): the release delay, which the person can
  // change (setTail).
  readonly tail?: number;
  // How long the open microphone stays open after the level falls below the
  // threshold.
  readonly hang?: number;
  // The longest a single press may last; it catches a key that is stuck.
  readonly max?: number;
}

// The browser's release delay is longer than conch-voice's 100 ms (issue
// #965). conch-voice's gate drops frames at the microphone, so its tail is
// what a listener hears; the browser's is a mute of the track, which silences
// what the browser has captured and not yet sent as well. Measured against
// LiveKit (web/e2e/32-voice-onset.spec.ts), a 100 ms tail was heard as 36 to
// 51 ms after the key came up. 200 ms is heard as about 150.
export const defaultTail = 200;
// The longest release delay, as conch-voice's release_tail_ms.
export const maxTail = 2000;

// The release-delay slider moves by releaseStep ms (F22), and PageUp and
// PageDown by releasePage.
export const releaseStep = 50;
export const releasePage = 250;
export const defaultHang = 400;

// The open microphone's watchdog: while the gate is open, a level sample must
// arrive within this long, or the gate shuts and arms the resume latch. The
// samples come from a probe that lives in the Voice view; if the probe is ever
// gone (the view unmounted, a render fault) the gate fails shut. Longer than
// the hang time on purpose: a probe that reports only changes must not shut a
// steady, loud gate.
export const sampleWatchdog = 1500;
export const defaultMax = 120_000;
// The phone's bound on a single press: the desktop figure Nick set on
// 2026-10-28 (ADR-016 Decision 5). The phone's gate is built with it.
export const phoneMax = 30_000;

export class TransmitGate {
  private readonly clock: Clock;
  private readonly onChange: (open: boolean) => void;
  private tail: number;
  private readonly hang: number;
  private readonly max: number;
  private isOpen = false;
  private mode: GateMode = 'ptt';
  private blocked = true;
  // The lifecycle block: set while the app is not in the foreground, the
  // screen is locked or an audio interruption stands.
  private lifecycle = false;
  // The resume latch: the open microphone may not open until quiet has lasted
  // the hang time (quietTimer) and a sample at or above the threshold follows.
  private latched = true;
  private quietTimer: unknown = null;
  private threshold = 0.1;
  private tailTimer: unknown = null;
  private hangTimer: unknown = null;
  private maxTimer: unknown = null;
  private watchdogTimer: unknown = null;

  constructor(clock: Clock, onChange: (open: boolean) => void, options: GateOptions = {}) {
    this.clock = clock;
    this.onChange = onChange;
    this.tail = clampTail(options.tail ?? defaultTail);
    this.hang = options.hang ?? defaultHang;
    this.max = options.max ?? defaultMax;
  }

  get open(): boolean {
    return this.isOpen;
  }

  // A new gate is blocked until the host says the connection is ready.
  // Unblocking arms the resume latch, so sound that is there at that moment
  // opens nothing.
  setBlocked(blocked: boolean): void {
    if (!blocked && this.blocked) this.arm();
    this.blocked = blocked;
    if (blocked) this.shut();
  }

  // lifecycleBlock shuts the gate at once, in both modes and with no tail, and
  // holds it shut until lifecycleUnblock. It is called for background, the
  // iOS inactive state, screen lock and an audio interruption. It arms the
  // resume latch.
  lifecycleBlock(): void {
    this.lifecycle = true;
    this.arm();
    this.shut();
  }

  // lifecycleUnblock lifts the lifecycle block. It opens nothing: a press must
  // be new, and the open microphone needs quiet and then an onset.
  lifecycleUnblock(): void {
    this.lifecycle = false;
  }

  get lifecycleBlocked(): boolean {
    return this.lifecycle;
  }

  // shutAndArm shuts the gate at once and arms the resume latch, with no
  // block to lift: the route changed under an open gate. Push to talk needs a
  // new press; the open microphone needs quiet and then an onset.
  shutAndArm(): void {
    this.arm();
    this.shut();
  }

  // cancel is a touch cancel (the system took the touch): the gate shuts at
  // once, with no tail.
  cancel(): void {
    this.shut();
  }

  // Changing the mode shuts the gate: a press or speech is judged afresh.
  setMode(mode: GateMode): void {
    if (mode === this.mode) return;
    this.mode = mode;
    this.arm();
    this.shut();
  }

  setThreshold(level: number): void {
    this.threshold = level;
  }

  get releaseDelay(): number {
    return this.tail;
  }

  // setTail sets the release delay, from 0 to maxTail ms. A release already
  // in its tail keeps the delay it started with.
  setTail(ms: number): void {
    this.tail = clampTail(ms);
  }

  press(): void {
    if (this.mode !== 'ptt' || this.blocked || this.lifecycle) return;
    this.clear('tail');
    this.clear('max');
    this.maxTimer = this.clock.setTimeout(() => {
      this.maxTimer = null;
      this.shut();
    }, this.max);
    this.openNow();
  }

  release(): void {
    if (this.mode !== 'ptt' || !this.isOpen || this.tailTimer !== null) return;
    this.clear('max');
    this.tailTimer = this.clock.setTimeout(() => {
      this.tailTimer = null;
      this.shut();
    }, this.tail);
  }

  // Phone: the browser's lost() is not copied. It shuts push to talk only;
  // the phone's equivalent is lifecycleBlock, which shuts both modes.

  // level is the microphone level, 0 to 1, in the open-microphone mode.
  level(value: number): void {
    if (this.mode !== 'open' || this.blocked || this.lifecycle) return;
    if (this.latched) {
      this.settle(value);
      return;
    }
    if (value >= this.threshold) {
      this.clear('hang');
      this.openNow();
    } else if (this.isOpen && this.hangTimer === null) {
      this.hangTimer = this.clock.setTimeout(() => {
        this.hangTimer = null;
        this.shut();
      }, this.hang);
    }
    this.watch();
  }

  // watch re-arms the watchdog on each sample while the gate is open.
  private watch(): void {
    this.clear('watchdog');
    if (!this.isOpen) return;
    this.watchdogTimer = this.clock.setTimeout(() => {
      this.watchdogTimer = null;
      this.shutAndArm();
    }, sampleWatchdog);
  }

  // shut closes the gate at once, without a tail.
  shut(): void {
    this.clear('tail');
    this.clear('hang');
    this.clear('max');
    this.clear('watchdog');
    if (!this.isOpen) return;
    this.isOpen = false;
    this.onChange(false);
  }

  // arm sets the resume latch: quiet is waited for afresh.
  private arm(): void {
    this.latched = true;
    this.clear('quiet');
  }

  // settle is a sample while the latch holds: sound restarts the wait for
  // quiet, and quiet for the hang time releases the latch. The sample that
  // releases it opens nothing; the next onset does.
  private settle(value: number): void {
    if (value >= this.threshold) {
      this.clear('quiet');
      return;
    }
    if (this.quietTimer !== null) return;
    this.quietTimer = this.clock.setTimeout(() => {
      this.quietTimer = null;
      this.latched = false;
    }, this.hang);
  }

  private openNow(): void {
    if (this.isOpen) return;
    this.isOpen = true;
    this.onChange(true);
  }

  private clear(which: 'tail' | 'hang' | 'max' | 'quiet' | 'watchdog'): void {
    const handle = which === 'tail' ? this.tailTimer : which === 'hang' ? this.hangTimer : which === 'max' ? this.maxTimer : which === 'watchdog' ? this.watchdogTimer : this.quietTimer;
    if (handle !== null) this.clock.clearTimeout(handle);
    if (which === 'tail') this.tailTimer = null;
    else if (which === 'hang') this.hangTimer = null;
    else if (which === 'max') this.maxTimer = null;
    else if (which === 'watchdog') this.watchdogTimer = null;
    else this.quietTimer = null;
  }
}

// clampTail keeps a release delay between 0 and maxTail ms, in whole ms; a
// value that is not a number is the default.
export function clampTail(ms: number): number {
  if (!Number.isFinite(ms)) return defaultTail;
  return Math.round(Math.min(maxTail, Math.max(0, ms)));
}

// snapTail is clampTail, rounded to the nearest releaseStep: a stored value off
// the slider's grid loads on it.
export function snapTail(ms: number): number {
  return clampTail(Math.round(clampTail(ms) / releaseStep) * releaseStep);
}
