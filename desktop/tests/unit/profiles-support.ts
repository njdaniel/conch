// Fakes for the profile tests: files in memory, a vault in memory, and
// activations that record what was done to them.
import type { ProfileFiles, ReadText } from '../../main/profiles/files.ts';
import type { Account, Activation, OpenSpec } from '../../main/profiles/switch.ts';
import type { Classification, GetResult, PutResult, RemoveResult, Vault } from '../../main/vault/vault.ts';

export interface MemoryFiles extends ProfileFiles {
  text: string | null;
  readonly writes: number;
  readonly dirs: Set<string>;
  readonly quarantined: number;
  // What the last quarantine moved aside.
  readonly aside: string[];
  failQuarantine: boolean;
  failWrite: boolean;
}

export function memoryFiles(initial?: string): MemoryFiles {
  const state = { text: initial ?? null, writes: 0, dirs: new Set<string>(), quarantined: 0, aside: [] as string[], failQuarantine: false, failWrite: false };
  return {
    get text() {
      return state.text;
    },
    get writes() {
      return state.writes;
    },
    get dirs() {
      return state.dirs;
    },
    get quarantined() {
      return state.quarantined;
    },
    get aside() {
      return state.aside;
    },
    get failQuarantine() {
      return state.failQuarantine;
    },
    set failQuarantine(v: boolean) {
      state.failQuarantine = v;
    },
    get failWrite() {
      return state.failWrite;
    },
    set failWrite(v: boolean) {
      state.failWrite = v;
    },
    read: (): Promise<ReadText> => Promise.resolve(state.text === null ? { kind: 'absent' } : { kind: 'text', text: state.text }),
    write: (text) => {
      if (state.failWrite) return Promise.reject(new Error('disk full'));
      state.writes++;
      state.text = text;
      return Promise.resolve();
    },
    quarantine: () => {
      if (state.failQuarantine) return Promise.reject(new Error('EXDEV'));
      state.quarantined++;
      if (state.text !== null) state.aside.push(state.text);
      state.text = null;
      return Promise.resolve();
    },
    ensureProfileDir: (id) => {
      state.dirs.add(id);
      return Promise.resolve();
    },
    removeProfileDir: (id) => {
      state.dirs.delete(id);
      return Promise.resolve();
    },
  };
}

export interface FakeVault extends Vault {
  classification: Classification;
  readonly secrets: Map<string, string>;
  removeFails: boolean;
  // remove never answers (a keyring prompt that is never closed).
  removeHangs: boolean;
  // put waits for this before it writes, when set.
  putGate: Promise<void> | null;
  unusable: Set<string>;
  readonly calls: string[];
}

export function fakeVault(classification: Classification = { kind: 'protected' }): FakeVault {
  const secrets = new Map<string, string>();
  const calls: string[] = [];
  // One operation at a time per profile id, in the order they were asked, as
  // the real vault does.
  const tails = new Map<string, Promise<unknown>>();
  const serial = <T>(id: string, op: () => Promise<T>): Promise<T> => {
    const run = (tails.get(id) ?? Promise.resolve()).then(op);
    tails.set(id, run.catch(() => undefined));
    return run;
  };
  const vault: FakeVault = {
    classification,
    secrets,
    removeFails: false,
    removeHangs: false,
    putGate: null,
    unusable: new Set(),
    calls,
    classify: () => Promise.resolve(vault.classification),
    put(id, secret): Promise<PutResult> {
      calls.push(`put ${id}`);
      return serial(id, async () => {
        if (vault.classification.kind !== 'protected') return vault.classification;
        if (vault.putGate !== null) await vault.putGate;
        secrets.set(id, secret);
        return { kind: 'stored' } as const;
      });
    },
    get(id): Promise<GetResult> {
      calls.push(`get ${id}`);
      return serial<GetResult>(id, () => {
        if (vault.classification.kind !== 'protected') return Promise.resolve(vault.classification);
        if (vault.unusable.has(id)) return Promise.resolve({ kind: 'unusable' } as const);
        const secret = secrets.get(id);
        return Promise.resolve(secret === undefined ? ({ kind: 'absent' } as const) : ({ kind: 'found', secret } as const));
      });
    },
    remove(id): Promise<RemoveResult> {
      calls.push(`remove ${id}`);
      return serial(id, (): Promise<RemoveResult> => {
        if (vault.removeHangs) return new Promise<RemoveResult>(() => undefined);
        if (vault.removeFails) return Promise.resolve({ kind: 'failed' });
        secrets.delete(id);
        return Promise.resolve({ kind: 'removed' });
      });
    },
  };
  return vault;
}

export interface FakeActivation extends Activation {
  readonly spec: OpenSpec & { readonly token: string | null };
  held: string | null;
  account: Account | null;
  readonly done: string[];
  signedOut: boolean;
}

// A factory that records every activation it makes, and everything done to
// each, in one shared log with a counter standing in for the clock.
export function fakeFactory(log: string[] = []) {
  const made: FakeActivation[] = [];
  const create = (spec: OpenSpec & { readonly token: string | null }): Promise<Activation> => {
    const n = made.length;
    const activation: FakeActivation = {
      spec,
      held: spec.token,
      account: null,
      done: [],
      signedOut: false,
      profileId: spec.profile?.id ?? null,
      token: () => activation.held,
      whoami: () => Promise.resolve(activation.account),
      signOut: () => {
        activation.held = null;
        activation.signedOut = true;
        log.push(`${String(n)}:sign-out`);
        return Promise.resolve('ended');
      },
      endSockets: () => {
        activation.done.push('sockets');
        log.push(`${String(n)}:sockets`);
      },
      endRequests: () => {
        activation.held = null;
        activation.done.push('requests');
        log.push(`${String(n)}:requests`);
      },
      destroy: () => {
        activation.done.push('renderer');
        log.push(`${String(n)}:renderer`);
        return Promise.resolve();
      },
    };
    made.push(activation);
    log.push(`${String(n)}:open ${spec.profile?.id.slice(0, 4) ?? 'chooser'}`);
    return Promise.resolve(activation);
  };
  return { made, create, log };
}

export const ORIGIN_A = 'https://a.example';
export const ORIGIN_B = 'https://b.example';
