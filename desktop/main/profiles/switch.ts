// The switch sequence (linux-desktop.md §4: "On switch: stop media, close
// sockets, abort requests, clear renderer state and release the helper before
// activating the next profile"). Input: the activation in force, the next
// profile, and the hooks. Output: the next activation, and a report of what
// the sequence did.
//
// In order, each awaited with a bound: the registered media hooks; the
// sockets (the document ends, and its feeds close); the requests (the
// identity ends, and what is in flight is aborted); the renderer (its web
// contents destroyed, its in-memory partition cleared); the helper hooks.
// Only then does the next profile activate, in a new partition. A step that
// does not finish within its bound is reported and the sequence goes on.
//
// Every switch is one at a time; a generation counts them, so that an async
// completion of an older one can tell it has been superseded.
import type { TeardownHooks, TeardownReason } from './hooks.ts';
import type { Profile } from './store.ts';

export const STEP_BOUND_MS = 3_000;

// One activation: a profile (or none: the chooser), its identity in main and
// its window. app.ts makes them; the unit tests make fakes.
export interface Activation {
  readonly profileId: string | null;
  // The credential in force, held in main's memory; null when signed out. It
  // is for the vault and for nothing that leaves main.
  token(): string | null;
  // Asks the server who the credential in force is. lasting: a member's, which
  // may be kept; an operator's or a guest's belongs to the one window.
  whoami(): Promise<Account | null>;
  // Forgets the credential at once, then tells the server (best effort).
  signOut(): Promise<'ended' | 'unconfirmed' | 'unsent'>;
  // Ends the document: its feeds close.
  endSockets(): void;
  // Ends the identity: the credential is dropped from memory and every request
  // in flight is aborted.
  endRequests(): void;
  // Destroys the web contents and clears its partition.
  destroy(): Promise<void>;
}

export interface Account {
  readonly id: number;
  readonly name: string;
  readonly lasting: boolean;
  readonly guest: boolean;
}

export interface OpenSpec {
  readonly profile: Profile | null;
  readonly generation: number;
}

export type Step = 'media' | 'sockets' | 'requests' | 'renderer' | 'helper' | 'activate';

export interface TraceEntry {
  readonly step: Step;
  readonly name: string;
  readonly at: number;
  readonly outcome: 'done' | 'hung' | 'failed';
}

export interface SwitchReport {
  readonly trace: readonly TraceEntry[];
  // Names of the steps and hooks that did not finish within their bound.
  readonly hung: readonly string[];
}

export interface SwitcherDeps {
  readonly hooks: TeardownHooks;
  readonly open: (spec: OpenSpec) => Promise<Activation>;
  readonly now: () => number;
  readonly log: (line: string) => void;
  readonly boundMs?: number;
}

export interface SwitchOptions {
  readonly reason: TeardownReason;
  // Called after the sockets are closed and before the requests are aborted.
  // Sign-out forgets and revokes the credential here.
  readonly afterSockets?: (old: Activation) => void;
}

type Bounded = 'done' | 'hung' | 'failed';

export class Switcher {
  readonly #deps: SwitcherDeps;
  #current: Activation | null = null;
  #generation = 0;
  #tail: Promise<unknown> = Promise.resolve();

  constructor(deps: SwitcherDeps) {
    this.#deps = deps;
  }

  get current(): Activation | null {
    return this.#current;
  }

  // The number of activations begun; it increases at the start of each switch.
  get generation(): number {
    return this.#generation;
  }

  // isCurrent says whether a generation is still the one in force: false for
  // an async completion that belongs to an identity that has been left.
  isCurrent(generation: number): boolean {
    return generation === this.#generation;
  }

  // close runs the sequence and activates nothing: the app is ending.
  close(options: SwitchOptions): Promise<SwitchReport> {
    return this.#enqueue('close', options);
  }

  switchTo(profile: Profile | null, options: SwitchOptions): Promise<SwitchReport> {
    return this.#enqueue(profile, options);
  }

  #enqueue(next: Profile | null | 'close', options: SwitchOptions): Promise<SwitchReport> {
    // The generation moves at once, so completions of the activation being
    // left see that it has ended even while the sequence waits its turn.
    this.#generation++;
    const generation = this.#generation;
    const run = this.#tail.then(() => this.#run(next, generation, options));
    this.#tail = run.catch(() => undefined);
    return run;
  }

  async #bounded(promise: Promise<unknown>, ms: number): Promise<Bounded> {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const timeout = new Promise<'hung'>((resolve) => {
      timer = setTimeout(() => {
        resolve('hung');
      }, ms);
      timer.unref();
    });
    try {
      return await Promise.race([promise.then(() => 'done' as const), timeout]);
    } catch {
      return 'failed';
    } finally {
      clearTimeout(timer);
    }
  }

  async #run(next: Profile | null | 'close', generation: number, options: SwitchOptions): Promise<SwitchReport> {
    const { hooks, now, log } = this.#deps;
    const bound = this.#deps.boundMs ?? STEP_BOUND_MS;
    const trace: TraceEntry[] = [];
    const hung: string[] = [];
    const step = async (name: Step, label: string, run: () => unknown): Promise<void> => {
      const outcome = await this.#bounded(Promise.resolve().then(run), bound);
      trace.push({ step: name, name: label, at: now(), outcome });
      if (outcome !== 'done') {
        // The name of the step or hook, never anything it held.
        log(`conch-desktop: teardown ${name} "${label}" ${outcome === 'hung' ? 'did not finish in time' : 'failed'}; going on without it`);
        hung.push(label);
      }
    };

    const old = this.#current;
    const context = { reason: options.reason };
    for (const hook of hooks.list('media')) await step('media', hook.name, () => hook.run(context));
    if (old !== null) {
      await step('sockets', 'feeds', () => {
        old.endSockets();
      });
      if (options.afterSockets !== undefined) options.afterSockets(old);
      await step('requests', 'requests', () => {
        old.endRequests();
      });
      await step('renderer', 'web contents', () => old.destroy());
      this.#current = null;
    }
    for (const hook of hooks.list('helper')) await step('helper', hook.name, () => hook.run(context));

    if (next === 'close') return { trace, hung };
    this.#current = await this.#deps.open({ profile: next, generation });
    trace.push({ step: 'activate', name: next?.id ?? 'chooser', at: now(), outcome: 'done' });
    return { trace, hung };
  }
}
