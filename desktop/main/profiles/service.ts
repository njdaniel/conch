// The profiles in main (issue #945): what the bridge's profile family does.
// Input: the store, the vault, the switch sequence and a way to open an
// activation. Output: the view the page may see (display metadata and the
// storage status, never a credential) and the changes the person asks for.
//
// It owns: which profile is active; where a credential lives (the vault when
// storage is protected, main's memory when it is not); the binding of a
// profile to the account that first signed in; and the order of sign-out.
//
// The compatibility gate of #946 (the allowlist of server releases, which
// decides whether a profile may sign in automatically) belongs in
// tokenFor(): it is the one place a stored credential is turned into a
// session at activation.
import type { ProfileAddResult, ProfileNotice, ProfilesView, StorageStatus } from '../../../web/src/platform/desktop/bridge.ts';
import { parseServerAddress } from '../api/origin.ts';
import type { Classification, PutResult, Vault } from '../vault/vault.ts';
import type { TeardownHooks } from './hooks.ts';
import { MAX_PROFILES, type Principal, type Profile, type ProfileStore } from './store.ts';
import { Switcher, type Account, type Activation, type OpenSpec, type SwitchReport } from './switch.ts';

// How long the vault may take to delete an entry, or the server to confirm a
// revocation, before the person is told and the work goes on without it.
export const VAULT_BOUND_MS = 5_000;
export const REVOKE_BOUND_MS = 6_000;
export const VERIFY_BOUND_MS = 10_000;

export type ProbeResult = 'ok' | 'certificate' | 'unreachable' | 'not-conch';

// What a stored credential turned out to be when the server was asked (with no
// window yet): an account, a credential the server no longer accepts, or no
// answer at all.
export type StoredCheck = { readonly kind: 'account'; readonly account: Account } | { readonly kind: 'rejected' } | { readonly kind: 'unreachable' };

export interface ProfilesDeps {
  readonly store: ProfileStore;
  readonly vault: Vault;
  readonly hooks: TeardownHooks;
  readonly createActivation: (spec: OpenSpec & { readonly token: string | null }) => Promise<Activation>;
  // Asks an address whether it is a Conch server, with no credential.
  readonly probe: (origin: string) => Promise<ProbeResult>;
  // Tells a server that a credential main holds for a profile that is not
  // active is over (best effort, bounded by the caller).
  readonly revokeStored: (origin: string, token: string) => Promise<'ended' | 'unconfirmed' | 'unsent'>;
  // Asks the server who a stored credential is, before any window holds it.
  readonly verifyStored: (origin: string, token: string) => Promise<StoredCheck>;
  readonly platform: string;
  readonly now: () => number;
  // A fixed event and a profile id. Never a credential or a path.
  readonly log: (line: string) => void;
  readonly stepBoundMs?: number;
  readonly vaultBoundMs?: number;
  readonly revokeBoundMs?: number;
  readonly verifyBoundMs?: number;
}

export const MISMATCH = 'profile_account_mismatch';

function storageOf(c: Classification): StorageStatus {
  switch (c.kind) {
    case 'protected':
      return { kind: 'protected' };
    case 'sessionOnly':
      return { kind: 'session-only', reason: c.reason };
    case 'temporarilyUnavailable':
      return { kind: 'unavailable' };
  }
}

async function within<T>(promise: Promise<T>, ms: number): Promise<{ readonly done: true; readonly value: T } | { readonly done: false }> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<{ readonly done: false }>((resolve) => {
    timer = setTimeout(() => {
      resolve({ done: false });
    }, ms);
    timer.unref();
  });
  try {
    return await Promise.race([promise.then((value) => ({ done: true as const, value })), late]);
  } catch {
    return { done: false };
  } finally {
    clearTimeout(timer);
  }
}

export class ProfilesService {
  readonly #deps: ProfilesDeps;
  readonly #switcher: Switcher;
  // Credentials that could not be kept: in main's memory only, by profile.
  readonly #held = new Map<string, string>();
  // Profiles the person signed out of whose vault entry could not (yet) be
  // deleted. A stored credential of one of these is never turned back into a
  // session; removing it is tried again each time. A later kept sign-in clears it.
  readonly #signedOut = new Set<string>();
  #storage: Classification = { kind: 'temporarilyUnavailable' };
  #notice: ProfileNotice | null = null;

  constructor(deps: ProfilesDeps) {
    this.#deps = deps;
    this.#switcher = new Switcher({
      hooks: deps.hooks,
      now: deps.now,
      log: deps.log,
      ...(deps.stepBoundMs === undefined ? {} : { boundMs: deps.stepBoundMs }),
      open: async (spec) => deps.createActivation({ ...spec, token: await this.tokenFor(spec.profile, spec.generation) }),
    });
  }

  get switcher(): Switcher {
    return this.#switcher;
  }

  get active(): Activation | null {
    return this.#switcher.current;
  }

  // start loads the list, asks the vault what storage is, and activates the
  // profile that was active last time (or the server list).
  async start(): Promise<void> {
    const { store } = this.#deps;
    await store.load();
    this.#storage = await this.#classify();
    const last = store.corrupt || store.active === null ? undefined : store.get(store.active);
    await this.#switcher.switchTo(last ?? null, { reason: 'switch' });
  }

  // tokenFor is the credential a profile starts with: main's memory first (a
  // session-only credential of this run), then the vault. A stored credential
  // that cannot be used is "sign in again" and nothing else.
  async tokenFor(profile: Profile | null, generation: number): Promise<string | null> {
    if (profile === null) return null;
    const kept = this.#held.get(profile.id);
    if (kept !== undefined) return kept;
    if (this.#signedOut.has(profile.id)) {
      // Signed out: it starts signed out, without asking the vault anything
      // (a delete that hangs would hold a read behind it). Deleting is tried
      // again in the background.
      void this.#purge(profile.id);
      return null;
    }
    const read = await within(
      this.#deps.vault.get(profile.id).catch(() => ({ kind: 'unusable' as const })),
      this.#deps.vaultBoundMs ?? VAULT_BOUND_MS,
    );
    // The identity this was asked for has been left: it gets nothing.
    if (!this.#switcher.isCurrent(generation)) return null;
    if (!read.done) {
      this.#storage = { kind: 'temporarilyUnavailable' };
      return null;
    }
    const got = read.value;
    switch (got.kind) {
      case 'found':
        this.#storage = { kind: 'protected' };
        return this.#verified(profile, got.secret, generation);
      case 'absent':
        return null;
      case 'unusable':
        this.#notice = 'sign-in-again';
        return null;
      case 'sessionOnly':
      case 'temporarilyUnavailable':
        this.#storage = got;
        return null;
    }
  }

  // verified checks, before any window holds a stored credential, that the
  // server still says it is the account the profile is bound to and that it is
  // a member's (only those are kept). Anything else is refused, forgotten and
  // revoked, as a sign-in as another account is. A server that cannot be asked
  // is not trusted with it: it is not adopted, and not deleted.
  async #verified(profile: Profile, token: string, generation: number): Promise<string | null> {
    const checked = await within(this.#deps.verifyStored(profile.origin, token), this.#deps.verifyBoundMs ?? VERIFY_BOUND_MS);
    if (!this.#switcher.isCurrent(generation)) return null;
    if (!checked.done || checked.value.kind === 'unreachable') {
      this.#notice = 'could-not-verify';
      return null;
    }
    if (checked.value.kind === 'rejected') {
      this.#notice = 'sign-in-again';
      this.#signedOut.add(profile.id);
      void this.#purge(profile.id);
      return null;
    }
    const { account } = checked.value;
    const bound = profile.principal;
    if (bound !== null && account.id === bound.id && account.lasting && !account.guest) return token;
    this.#deps.log(`conch-desktop: the stored sign-in of profile ${profile.id} is not the account it is bound to; refused`);
    this.#notice = 'account-mismatch';
    this.#signedOut.add(profile.id);
    void within(this.#deps.revokeStored(profile.origin, token), this.#deps.revokeBoundMs ?? REVOKE_BOUND_MS);
    void this.#purge(profile.id);
    return null;
  }

  async #classify(): Promise<Classification> {
    const r = await within(this.#deps.vault.classify(), this.#deps.vaultBoundMs ?? VAULT_BOUND_MS);
    return r.done ? r.value : { kind: 'temporarilyUnavailable' };
  }

  // purge deletes a profile's vault entry, bounded. It is the only thing that
  // takes a profile off the signed-out list.
  async #purge(id: string): Promise<boolean> {
    const r = await within(this.#deps.vault.remove(id), this.#deps.vaultBoundMs ?? VAULT_BOUND_MS);
    if (r.done && r.value.kind === 'removed') {
      this.#signedOut.delete(id);
      return true;
    }
    this.#deps.log(`conch-desktop: could not delete the vault entry of profile ${id}`);
    return false;
  }

  view(): ProfilesView {
    const { store, platform } = this.#deps;
    const active = this.#switcher.current?.profileId ?? null;
    return {
      profiles: store.list().map((p) => ({
        id: p.id,
        label: p.label,
        host: new URL(p.origin).host,
        origin: p.origin,
        account: p.principal === null ? null : p.principal.name,
        active: p.id === active,
      })),
      active,
      storage: storageOf(this.#storage),
      recovery: store.corrupt,
      notice: this.#notice,
      platform,
    };
  }

  async add(address: string): Promise<ProfileAddResult> {
    const { store } = this.#deps;
    if (store.corrupt) return { ok: false, refusal: 'unsupported' };
    if (store.list().length >= MAX_PROFILES) return { ok: false, refusal: 'full' };
    const parsed = parseServerAddress(address);
    if (!parsed.ok) return { ok: false, refusal: parsed.refusal };
    const probed = await this.#deps.probe(parsed.origin);
    if (probed === 'certificate') return { ok: false, refusal: 'certificate' };
    if (probed === 'not-conch') return { ok: false, refusal: 'not-conch' };
    if (probed === 'unreachable') return { ok: false, refusal: 'unreachable' };
    const profile = await store.add(parsed.origin).catch(() => null);
    return profile === null ? { ok: false, refusal: 'unsupported' } : { ok: true, id: profile.id };
  }

  async rename(id: string, label: string): Promise<boolean> {
    return this.#deps.store.rename(id, label).catch(() => false);
  }

  // remove deletes the profile: its credential is revoked at the server first
  // (best effort, bounded), the sign-in is ended if it is the active one, then
  // the vault entry goes (a failure there keeps the profile, so that it can be
  // tried again), then the profile and its directory.
  async remove(id: string): Promise<boolean> {
    const { store, vault } = this.#deps;
    const profile = store.get(id);
    if (profile === undefined) return false;
    let revoke: Promise<'ended' | 'unconfirmed' | 'unsent'> = Promise.resolve('unsent');
    // The activation this was asked of, if the profile is active now. The run
    // may wait behind another switch; it ends this activation only if it is
    // still the one it finds, and revokes the credential as an inactive
    // profile's otherwise.
    const target = this.#switcher.current?.profileId === id ? this.#switcher.current : null;
    const known = target?.token() ?? null;
    const run = { ended: false };
    if (target !== null) {
      this.#notice = null;
      await this.#switcher.switchTo(null, {
        reason: 'switch',
        afterSockets: (old) => {
          if (old === target) {
            run.ended = true;
            revoke = old.signOut();
          }
        },
      });
    }
    if (!run.ended) {
      const bound = this.#deps.vaultBoundMs ?? VAULT_BOUND_MS;
      let token = known ?? this.#held.get(id) ?? null;
      if (token === null) {
        const got = await within(vault.get(id), bound);
        if (got.done && got.value.kind === 'found') token = got.value.secret;
      }
      if (token !== null) revoke = this.#deps.revokeStored(profile.origin, token);
    }
    this.#held.delete(id);
    await within(revoke, this.#deps.revokeBoundMs ?? REVOKE_BOUND_MS);
    this.#signedOut.add(id);
    if (!(await this.#purge(id))) return false;
    this.#signedOut.delete(id);
    return store.remove(id).catch(() => false);
  }

  async switchTo(id: string | null): Promise<boolean> {
    const { store } = this.#deps;
    if (store.corrupt) return false;
    const profile = id === null ? null : store.get(id);
    if (profile === undefined) return false;
    this.#notice = null;
    const report = await this.#switcher.switchTo(profile, { reason: 'switch' });
    this.#noteReport(report);
    await store.setActive(id).catch(() => undefined);
    return true;
  }

  #noteReport(report: SwitchReport): void {
    if (report.hung.length > 0 && this.#notice === null) this.#notice = 'teardown-slow';
  }

  // signOut ends the active profile's sign-in: media first, then the sockets;
  // then the credential leaves memory and the vault entry is deleted whatever
  // storage is (bounded, and not waited for by the page); the server is told if
  // it can be; the same profile then starts again in a fresh partition.
  async signOut(): Promise<'ended' | 'unconfirmed' | 'unsent'> {
    const { store } = this.#deps;
    const current = this.#switcher.current;
    if (current === null || current.profileId === null) return 'ended';
    const id = current.profileId;
    const profile = store.get(id) ?? null;
    let revoke: Promise<'ended' | 'unconfirmed' | 'unsent'> = Promise.resolve('unsent');
    this.#notice = null;
    // The activation this was asked of and its credential now: the run may
    // wait its turn behind another switch, and then the activation it finds
    // current is not this one.
    const token = current.token();
    const report = await this.#switcher.switchTo(profile, {
      reason: 'sign-out',
      afterSockets: (old) => {
        // The credential and the session go first; then the entry, whatever
        // the storage says. Neither is waited for here.
        if (old === current) revoke = old.signOut();
        else if (token !== null && profile !== null) revoke = this.#deps.revokeStored(profile.origin, token);
        this.#held.delete(id);
        // Until the entry is really gone, this profile does not start from it.
        this.#signedOut.add(id);
        void this.#purge(id);
      },
    });
    this.#noteReport(report);
    const outcome = await within(revoke, this.#deps.revokeBoundMs ?? REVOKE_BOUND_MS);
    const result = outcome.done ? outcome.value : 'unconfirmed';
    if (result === 'unconfirmed') this.#notice = 'revocation-unconfirmed';
    else if (result === 'unsent') this.#notice = 'revocation-unsent';
    return result;
  }

  // signedIn is called after a sign-in the active window just completed. It
  // applies the binding rule and then keeps the credential: in the vault when
  // storage is protected, in memory otherwise. false when the profile belongs
  // to another account: the credential is then forgotten and revoked.
  async signedIn(account: Account): Promise<boolean> {
    const { store } = this.#deps;
    const current = this.#switcher.current;
    if (current === null || current.profileId === null) return false;
    const id = current.profileId;
    // A guest is not an account of the server's people: nothing binds to it,
    // and nothing is kept.
    if (account.guest) return true;
    const bound = await store.bind(id, { id: account.id, name: account.name } satisfies Principal).catch(() => null);
    if (bound === null || !bound.ok) {
      this.#deps.log(`conch-desktop: a sign-in on profile ${id} was refused: ${bound === null ? 'not saved' : bound.reason}`);
      if (this.#switcher.current === current) {
        await current.signOut();
        // The client only sees a refusal it does not know; this says why.
        this.#notice = 'account-mismatch';
      }
      return false;
    }
    if (!account.lasting) return true;
    const token = current.token();
    // The window may have moved on while the store was written.
    if (token === null || this.#switcher.current !== current) return true;
    const put = await this.#put(id, token);
    if (this.#switcher.current !== current) return true;
    if (put.kind === 'stored') {
      this.#storage = { kind: 'protected' };
      this.#held.delete(id);
      this.#signedOut.delete(id);
    } else {
      // Not kept: this run only, in memory, and said so.
      this.#held.set(id, token);
      if (put.kind === 'sessionOnly' || put.kind === 'temporarilyUnavailable') this.#storage = put;
    }
    return true;
  }

  // keptCredential is signedIn for a pasted credential that has just been put
  // in force: who it is has to be asked.
  async keptCredential(): Promise<boolean> {
    const current = this.#switcher.current;
    const account = current === null ? null : await current.whoami();
    if (account !== null) return this.signedIn(account);
    // Who it is could not be checked (a timeout, a 5xx): it is not left in
    // force unbound. As a refused sign-in, it is forgotten and revoked.
    if (current !== null && this.#switcher.current === current) await current.signOut();
    return false;
  }

  async #put(id: string, token: string): Promise<PutResult | { readonly kind: 'failed' }> {
    const r = await within(this.#deps.vault.put(id, token).catch((): { readonly kind: 'failed' } => ({ kind: 'failed' })), this.#deps.vaultBoundMs ?? VAULT_BOUND_MS);
    return r.done ? r.value : { kind: 'failed' };
  }

  // retry asks the vault again. If storage is now protected, the credentials
  // held in memory are put (not trusted to a get). Each is written only if it
  // is still the credential held for that profile at the moment of its turn: a
  // profile signed out or removed while an earlier write was pending is not
  // written back.
  async retry(): Promise<ProfilesView> {
    this.#storage = await this.#classify();
    if (this.#storage.kind === 'protected') {
      for (const [id, token] of [...this.#held]) {
        if (this.#held.get(id) !== token || this.#signedOut.has(id) || this.#deps.store.get(id) === undefined) continue;
        const put = await this.#put(id, token);
        if (put.kind === 'stored') {
          // Only if nothing has changed it meanwhile; a sign-out in the
          // meantime has queued its delete behind this write.
          if (this.#held.get(id) === token) this.#held.delete(id);
        } else if (put.kind === 'sessionOnly' || put.kind === 'temporarilyUnavailable') this.#storage = put;
      }
    }
    return this.view();
  }

  async recover(): Promise<ProfilesView> {
    try {
      await this.#deps.store.recover();
    } catch {
      this.#deps.log('conch-desktop: could not set the damaged profiles.json aside; nothing was changed');
    }
    return this.view();
  }

  // close runs the teardown for the app's end.
  async close(): Promise<void> {
    await this.#switcher.close({ reason: 'quit' });
  }
}
