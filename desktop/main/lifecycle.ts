// The lock and suspend rules of the Linux desktop app (issue #1588, D5-02 of
// #773; linux-desktop.md §9, the Lock and suspend rows). Platform-neutral:
// input is events (the screen locked or unlocked, the machine is about to
// sleep or has woken, whether the lock can be observed at all), output is
// actions on voice, the screen companion and notifications. It imports
// nothing from Electron and nothing from Node, so every rule is testable.
//
// The rules fail closed:
//   - lock: notification text is hidden, the companion ends ('lock'), and the
//     microphone is muted (not left: the call keeps playing). A mute the
//     helper does not acknowledge within the contract's bound is followed by
//     a leave.
//   - unlock: notification text may show again. Nothing else: still muted, no
//     share, no rejoin. Unmuting is the person's act.
//   - suspend: leave voice and end the companion ('suspend'), once however
//     many signals say so. Resume does nothing to either.
//   - when the lock cannot be observed, notification text is treated as
//     locked, and #1593 says so. Every action is idempotent.
//
// Owns: the locked and suspended flags, and nothing that persists. A signal
// source (lifecycle-linux.ts) reports under its own name: the screen is
// locked if any source says so.

// A lock-state source: logind's LockedHint and Lock/Unlock signals, or the
// idle-state poll (a second signal).
export type LockSource = 'logind' | 'idle';

// Whether main can tell when the screen locks. 'unavailable' is exposed for
// #1593 (D5-07), which tells the person that lock won't mute them.
export type LockObservation = 'available' | 'unavailable';

// The ways the companion can end because of this module.
export type LifecycleEnd = 'lock' | 'suspend';

// How long a mute may take to be acknowledged before main leaves instead
// (the contract's VOICE_CONTROL_ACK_TIMEOUT_MS).
export const MUTE_BOUND_MS = 5000;

// Voice statuses with no helper to mute or leave.
const NO_SESSION: ReadonlySet<string> = new Set(['idle', 'unavailable', 'ended', 'failed', 'lost']);

export interface LifecycleDeps {
  readonly voice: {
    state(): { readonly status: string };
    setMute(muted: boolean): Promise<{ readonly ok: boolean }>;
    leave(): Promise<unknown>;
    // Records that main muted the person at a lock, so the strip can say so
    // until they unmute or leave.
    setLockMuted(on: boolean): void;
  };
  readonly companion: { end(reason: LifecycleEnd): Promise<unknown> };
  readonly notifications: { setLocked(locked: boolean): void };
  readonly log: (line: string) => void;
  readonly muteBoundMs?: number;
}

export class Lifecycle {
  readonly #deps: LifecycleDeps;
  readonly #bound: number;
  readonly #sources = new Map<LockSource, boolean>();
  // Nothing observes the lock until a source says it can: fail closed.
  #observation: LockObservation = 'unavailable';
  #locked = false;
  #suspended = false;
  // What notifications were last told, so a repeat is not sent again.
  #redacted: boolean | null = null;

  constructor(deps: LifecycleDeps) {
    this.#deps = deps;
    this.#bound = deps.muteBoundMs ?? MUTE_BOUND_MS;
    this.#syncNotifications();
  }

  get lockObservation(): LockObservation {
    return this.#observation;
  }

  // Whether the screen is locked, by any source.
  get locked(): boolean {
    return this.#locked;
  }

  get suspended(): boolean {
    return this.#suspended;
  }

  // observe says whether the lock can be observed now. While it cannot,
  // notification text stays hidden, as if locked.
  observe(state: LockObservation): void {
    if (state === this.#observation) return;
    this.#observation = state;
    if (state === 'unavailable') this.#deps.log('lifecycle: the screen lock cannot be observed; notification text is hidden and lock will not mute voice');
    this.#syncNotifications();
  }

  // lockSignal is one source's report of whether the screen is locked. The
  // screen is locked if any source says so; only the change to locked acts.
  lockSignal(source: LockSource, locked: boolean): Promise<void> {
    if (this.#sources.get(source) === locked) return Promise.resolve();
    this.#sources.set(source, locked);
    const now = [...this.#sources.values()].some((v) => v);
    if (now === this.#locked) return Promise.resolve();
    this.#locked = now;
    this.#syncNotifications();
    return now ? this.#lock() : Promise.resolve();
  }

  // suspend: the machine is going to sleep (PrepareForSleep(true) or
  // powerMonitor's suspend, whichever comes first). Acts once until resume.
  suspend(): Promise<void> {
    if (this.#suspended) return Promise.resolve();
    this.#suspended = true;
    return Promise.all([this.#deps.voice.leave(), this.#deps.companion.end('suspend')]).then(
      () => undefined,
      () => {
        this.#deps.log('lifecycle: leaving voice or ending the share at suspend failed');
      },
    );
  }

  // resume: nothing rejoins, unmutes or reshares by itself.
  resume(): void {
    this.#suspended = false;
  }

  #syncNotifications(): void {
    const redact = this.#locked || this.#observation === 'unavailable';
    if (this.#redacted === redact) return;
    this.#redacted = redact;
    this.#deps.notifications.setLocked(redact);
  }

  async #lock(): Promise<void> {
    const results = await Promise.allSettled([this.#deps.companion.end('lock'), this.#mute()]);
    if (results.some((r) => r.status === 'rejected')) this.#deps.log('lifecycle: an action at lock failed');
  }

  // #mute mutes a running session, and leaves when the helper does not
  // acknowledge the mute within the bound (fail closed).
  async #mute(): Promise<void> {
    const voice = this.#deps.voice;
    if (NO_SESSION.has(voice.state().status)) return;
    let timer: NodeJS.Timeout | undefined;
    const late = new Promise<false>((resolve) => {
      timer = setTimeout(() => {
        resolve(false);
      }, this.#bound);
    });
    let muted = false;
    try {
      muted = await Promise.race([voice.setMute(true).then((r) => r.ok, () => false), late]);
    } finally {
      clearTimeout(timer);
    }
    if (muted) {
      voice.setLockMuted(true);
      return;
    }
    this.#deps.log('lifecycle: the mute at lock was not confirmed; leaving voice');
    await voice.leave();
  }
}
