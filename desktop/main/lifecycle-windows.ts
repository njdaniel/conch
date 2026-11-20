// Windows lock and sleep observation (#1935; windows-desktop.md §9.2).
// Input: Electron powerMonitor events and its initial idle state. Output:
// calls on the shared Lifecycle rules. Owns only its event listeners; no
// helper, persistent state or Windows-specific product rule lives here.
import type { Lifecycle } from './lifecycle.ts';

export type WindowsPowerEvent = 'lock-screen' | 'unlock-screen' | 'suspend' | 'resume';

export interface WindowsPowerMonitor {
  on(event: WindowsPowerEvent, listener: () => void): unknown;
  removeListener(event: WindowsPowerEvent, listener: () => void): unknown;
  getSystemIdleState(idleThreshold: number): string;
}

export class WindowsLifecycle {
  readonly #deps: { readonly rules: Lifecycle; readonly powerMonitor: WindowsPowerMonitor };
  #started = false;
  readonly #listeners: Readonly<Record<WindowsPowerEvent, () => void>> = {
    'lock-screen': () => {
      void this.#deps.rules.lockSignal('powerMonitor', true);
      this.#deps.rules.observe('available');
    },
    'unlock-screen': () => {
      void this.#deps.rules.lockSignal('powerMonitor', false);
      this.#deps.rules.observe('available');
    },
    suspend: () => { void this.#deps.rules.suspend(); },
    resume: () => {
      this.#readLockState();
      this.#deps.rules.resume();
    },
  };

  constructor(deps: { readonly rules: Lifecycle; readonly powerMonitor: WindowsPowerMonitor }) {
    this.#deps = deps;
  }

  start(): void {
    if (this.#started) return;
    this.#started = true;
    const { powerMonitor } = this.#deps;
    for (const event of Object.keys(this.#listeners) as WindowsPowerEvent[]) powerMonitor.on(event, this.#listeners[event]);
    this.#readLockState();
  }

  #readLockState(): void {
    const { rules, powerMonitor } = this.#deps;
    // The listeners are installed before this check. An unknown read at
    // startup or resume keeps notifications and the hotkey fail-closed
    // until the next lock or unlock event establishes observation.
    let state = 'unknown';
    try {
      state = powerMonitor.getSystemIdleState(1);
    } catch {
      // Event observation remains installed even if this read fails.
    }
    if (state === 'locked' || state === 'active' || state === 'idle') {
      void rules.lockSignal('powerMonitor', state === 'locked');
      rules.observe('available');
    } else {
      rules.observe('unavailable');
    }
  }

  stop(): void {
    if (!this.#started) return;
    this.#started = false;
    for (const event of Object.keys(this.#listeners) as WindowsPowerEvent[]) this.#deps.powerMonitor.removeListener(event, this.#listeners[event]);
    this.#deps.rules.observe('unavailable');
    this.#deps.rules.dropSource('powerMonitor');
  }
}
