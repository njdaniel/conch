// The transmit gate: the one place that decides whether the microphone track
// is sending. Microphone audio leaves the page only while the gate is open.
// It opens on a press (push-to-talk) or on speech (open microphone) and shuts
// on a release after a short tail, on loss of focus, when blocked (muted,
// deafened, not connected), or when a single press has lasted too long.
//
// It owns no timer of its own: the host gives it the clock, so the tests drive
// it by hand.
import type { Clock } from '../state/feed';

export type GateMode = 'ptt' | 'open';

export interface GateOptions {
  // How long the gate stays open after a release, so the last syllable is not
  // cut (conch-voice.md section 3).
  readonly tail?: number;
  // How long the open microphone stays open after the level falls below the
  // threshold.
  readonly hang?: number;
  // The longest a single press may last; it catches a key that is stuck.
  readonly max?: number;
}

export const defaultTail = 100;
export const defaultHang = 400;
export const defaultMax = 120_000;

export class TransmitGate {
  private readonly clock: Clock;
  private readonly onChange: (open: boolean) => void;
  private readonly tail: number;
  private readonly hang: number;
  private readonly max: number;
  private isOpen = false;
  private mode: GateMode = 'ptt';
  private blocked = true;
  private threshold = 0.1;
  private tailTimer: unknown = null;
  private hangTimer: unknown = null;
  private maxTimer: unknown = null;

  constructor(clock: Clock, onChange: (open: boolean) => void, options: GateOptions = {}) {
    this.clock = clock;
    this.onChange = onChange;
    this.tail = options.tail ?? defaultTail;
    this.hang = options.hang ?? defaultHang;
    this.max = options.max ?? defaultMax;
  }

  get open(): boolean {
    return this.isOpen;
  }

  // A new gate is blocked until the host says the connection is ready.
  setBlocked(blocked: boolean): void {
    this.blocked = blocked;
    if (blocked) this.shut();
  }

  // Changing the mode shuts the gate: a press or speech is judged afresh.
  setMode(mode: GateMode): void {
    if (mode === this.mode) return;
    this.mode = mode;
    this.shut();
  }

  setThreshold(level: number): void {
    this.threshold = level;
  }

  press(): void {
    if (this.mode !== 'ptt' || this.blocked) return;
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

  // level is the microphone level, 0 to 1, in the open-microphone mode.
  level(value: number): void {
    if (this.mode !== 'open' || this.blocked) return;
    if (value >= this.threshold) {
      this.clear('hang');
      this.openNow();
    } else if (this.isOpen && this.hangTimer === null) {
      this.hangTimer = this.clock.setTimeout(() => {
        this.hangTimer = null;
        this.shut();
      }, this.hang);
    }
  }

  // shut closes the gate at once, without a tail.
  shut(): void {
    this.clear('tail');
    this.clear('hang');
    this.clear('max');
    if (!this.isOpen) return;
    this.isOpen = false;
    this.onChange(false);
  }

  private openNow(): void {
    if (this.isOpen) return;
    this.isOpen = true;
    this.onChange(true);
  }

  private clear(which: 'tail' | 'hang' | 'max'): void {
    const handle = which === 'tail' ? this.tailTimer : which === 'hang' ? this.hangTimer : this.maxTimer;
    if (handle !== null) this.clock.clearTimeout(handle);
    if (which === 'tail') this.tailTimer = null;
    else if (which === 'hang') this.hangTimer = null;
    else this.maxTimer = null;
  }
}
