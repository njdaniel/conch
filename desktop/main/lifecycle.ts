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
//     helper does not acknowledge, or whose effect the view does not show,
//     within the contract's bound is followed by a leave. With no helper to
//     mute (idle, or a join or retry still on its way to starting one) the
//     lock leaves: leave is safe with nothing running and overtakes a join.
//     While locked, VoiceService refuses to unmute, starts joins muted and
//     the companion refuses grants (they read Lifecycle.locked).
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

// How often the view is read to see a mute take effect.
const MUTE_POLL_MS = 25;

export interface LifecycleDeps {
  readonly voice: {
    state(): { readonly status: string; readonly helper?: { readonly muted: boolean } };
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

  // dropSource forgets what one source last said, because the source is gone
  // (its watcher gave up), not because the screen changed. A stale "locked"
  // from a source that can no longer say "unlocked" would hold the app locked
  // for good. Nothing acts: no lock action runs, and an unlock only means the
  // flag is clear (unmute and join work again; still muted, no share, no
  // rejoin). Notification text stays hidden while observation is
  // 'unavailable', which is the state a gone source leaves behind.
  dropSource(source: LockSource): void {
    if (!this.#sources.delete(source)) return;
    const now = [...this.#sources.values()].some((v) => v);
    if (now === this.#locked) return;
    this.#locked = now;
    this.#syncNotifications();
  }

  // suspend: the machine is going to sleep (PrepareForSleep(true) or
  // powerMonitor's suspend, whichever comes first). Signals that arrive while
  // it acts are one. The latch clears when the leave and the end settle (both
  // are idempotent) and not only on resume, so a missed resume event cannot
  // leave the next suspend ignored.
  suspend(): Promise<void> {
    if (this.#suspended) return Promise.resolve();
    this.#suspended = true;
    return Promise.all([this.#deps.voice.leave(), this.#deps.companion.end('suspend')]).then(
      () => {
        this.#suspended = false;
      },
      () => {
        this.#suspended = false;
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
  // acknowledge the mute, or the view does not show it, within the bound
  // (fail closed). With no helper to mute (idle, ended, failed, lost: a join
  // or retry may be in flight with the status still idle) it leaves: leave is
  // safe with nothing running, and it overtakes a join that has not started.
  async #mute(): Promise<void> {
    const voice = this.#deps.voice;
    const status = voice.state().status;
    if (status !== 'running' && status !== 'starting' && status !== 'stopping') {
      await voice.leave();
      return;
    }
    let timer: NodeJS.Timeout | undefined;
    const bound = { over: false };
    const late = new Promise<false>((resolve) => {
      timer = setTimeout(() => {
        bound.over = true;
        resolve(false);
      }, this.#bound);
    });
    // The ack is the helper's word; the view is what the page and the next
    // unmute rely on, so both must say muted inside the one bound.
    const confirmed = (async (): Promise<boolean> => {
      const acked = await voice.setMute(true).then((r) => r.ok, () => false);
      if (!acked) return false;
      while (voice.state().helper?.muted !== true) {
        if (bound.over) return false;
        await new Promise<void>((resolve) => {
          setTimeout(resolve, MUTE_POLL_MS);
        });
      }
      return true;
    })();
    let muted = false;
    try {
      muted = await Promise.race([confirmed, late]);
    } finally {
      clearTimeout(timer);
      bound.over = true;
    }
    if (muted) {
      voice.setLockMuted(true);
      return;
    }
    this.#deps.log('lifecycle: the mute at lock was not confirmed; leaving voice');
    await voice.leave();
  }
}
