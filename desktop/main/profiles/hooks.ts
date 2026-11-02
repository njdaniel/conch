// Teardown hooks (issue #945): the places #771 (native voice) and #772 (share)
// register what must stop before one identity gives way to the next. Input: a
// name and a function from its owner. Output: the registered hooks, in order,
// for the switch sequence to run first (media) and last (the helper's release).
//
// A hook must stop what it owns and resolve; one that does not resolve within
// the bound is reported and the sequence goes on without it.

export type TeardownReason = 'switch' | 'sign-out' | 'quit';

export type TeardownHook = (context: { readonly reason: TeardownReason }) => void | Promise<void>;

export type HookPhase = 'media' | 'helper';

export interface NamedHook {
  readonly name: string;
  readonly run: TeardownHook;
}

export class TeardownHooks {
  readonly #hooks: Record<HookPhase, NamedHook[]> = { media: [], helper: [] };

  // register adds a hook to a phase and returns the function that removes it.
  register(phase: HookPhase, name: string, run: TeardownHook): () => void {
    const hook: NamedHook = { name, run };
    this.#hooks[phase].push(hook);
    return () => {
      const at = this.#hooks[phase].indexOf(hook);
      if (at >= 0) this.#hooks[phase].splice(at, 1);
    };
  }

  // The media owners' hooks (voice, share): run first.
  media(name: string, run: TeardownHook): () => void {
    return this.register('media', name, run);
  }

  // The helper's release: runs after the old renderer is gone.
  helper(name: string, run: TeardownHook): () => void {
    return this.register('helper', name, run);
  }

  list(phase: HookPhase): readonly NamedHook[] {
    return [...this.#hooks[phase]];
  }
}
