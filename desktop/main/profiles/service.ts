// The profiles in main (issue #945): what the bridge's profile family does.
// Input: the store, the vault, the switch sequence and a way to open an
// activation. Output: the view the page may see (display metadata and the
// storage status, never a credential) and the changes the person asks for.
//
// It owns: which profile is active; where a credential lives (the vault when
// storage is protected and the person kept the sign-in, main's memory when
// not); the binding of a profile to the account that first signed in; and the
// order of sign-out.
//
// "Keep me signed in on this computer" (issue #2077) is the person's choice
// for the activation in force, on by default; it is asked of the window, never
// stored. Unchecked, a member's sign-in lasts until the app closes, and any
// older entry of that profile is deleted. An operator's is never kept,
// whatever the choice (ADR-012).
//
// The compatibility gate of #946 is in tokenFor(), the one place a stored
// credential is turned into a session at activation: before the vault is read
// or the server is asked about a credential, /healthz must name a release on
// the allowlist (or the development flag must allow it). The same verdict
// decides whether a sign-in may happen in the window at all.
//
// The sweep of issue #2217 runs once per start, in the background, after the
// list loads and whatever any server's health: every vault entry an older
// build sealed with Chromium's built-in key (v10, as good as plain text) is
// ended, each in its profile's own vault turn, so that a remove, a sign-out or
// a kept sign-in asked meanwhile waits for it. If its server answers /healthz
// in time with the health document, the entry is opened only to be handed to
// revokeStored, as sign-out does, then deleted; if not, it is deleted unopened.
// Whenever an older build's sign-in is not confirmed ended at the server (and
// also when a read deleted one unread), that profile shows a notice saying it
// could not be ended there. Each outcome is one fixed log line.
import type { MainLog } from '../diagnostics.ts';
import type { CompatView, ProfileAddResult, ProfileNotice, ProfilesView, ProfileView, ShareSoundChoice, StorageStatus } from '../../../web/src/platform/desktop/bridge.ts';
import type { OperationId } from '../../../web/src/platform/operations.ts';
import { parseServerAddress } from '../api/origin.ts';
import { judge, permits, REQUIRED_FAMILIES, type CompatPolicy, type HealthResult, type Verdict } from '../compat.ts';
import type { Classification, PutResult, SessionOnlyReason, Vault } from '../vault/vault.ts';
import type { TeardownHooks } from './hooks.ts';
import { MAX_PROFILES, type Principal, type Profile, type ProfileStore } from './store.ts';
import { Switcher, type Account, type Activation, type OpenSpec, type SwitchReport } from './switch.ts';

// How long the vault may take to delete an entry, or the server to confirm a
// revocation, before the person is told and the work goes on without it.
export const VAULT_BOUND_MS = 5_000;
export const REVOKE_BOUND_MS = 6_000;
export const VERIFY_BOUND_MS = 10_000;

export const HEALTH_BOUND_MS = 8_000;

// What the sweep of #2217 did with one entry, or with the whole sweep: the
// closed vocabulary of its log lines. ended: revoked at the server and
// deleted; ended-not-deleted: revoked, but the file could not be deleted (a
// get deletes it unread later). deleted-*: deleted, and not shown ended at the
// server, because the server did not answer /healthz with the health document
// (unreachable), the revocation was not confirmed (unconfirmed), the file
// could not be opened (unopened), no saved server owns it (no-profile), or
// the file was open to group or others, so it was not read whatever its seal
// (loose-mode, issue #2217's review, R1).
// not-deleted: not ended at the server, and it could not be deleted.
// replaced: something replaced or deleted it before its turn. late: it did
// not finish within its bound. not-listed: the vault could not be searched (a
// get deletes such a file unread later, and says so). skipped: profiles.json
// could not be read, so no entry's server is known and nothing was done. Every
// outcome but ended and ended-not-deleted puts the profile's notice up.
export type LegacyOutcome =
  | 'ended'
  | 'ended-not-deleted'
  | 'deleted-unreachable'
  | 'deleted-unconfirmed'
  | 'deleted-unopened'
  | 'deleted-no-profile'
  | 'deleted-loose-mode'
  | 'not-deleted'
  | 'replaced'
  | 'late'
  | 'not-listed'
  | 'skipped';

// What an activation is opened with: the profile, the credential main found
// for it (null: signed out), and the way it reports an answer of a required
// family that was not the document this app was built for.
export interface ActivationSpec extends OpenSpec {
  readonly token: string | null;
  readonly onInvalid: (op: OperationId) => void;
}

export type ProbeResult = 'ok' | 'certificate' | 'unreachable' | 'not-conch';

// What a stored credential turned out to be when the server was asked (with no
// window yet): an account, a credential the server no longer accepts, or no
// answer at all.
export type StoredCheck = { readonly kind: 'account'; readonly account: Account } | { readonly kind: 'rejected' } | { readonly kind: 'unreachable' };

export interface ProfilesDeps {
  readonly store: ProfileStore;
  readonly vault: Vault;
  readonly hooks: TeardownHooks;
  readonly createActivation: (spec: ActivationSpec) => Promise<Activation>;
  // The allowlist and the development flag (issue #946), this app's own
  // version, and a way to ask an origin what /healthz says (no credential).
  readonly compat: CompatPolicy;
  readonly desktopVersion: string;
  readonly readHealth: (origin: string) => Promise<HealthResult>;
  // The same question, strictly (issue #2217's review): answered only when the
  // reply is the health document. The sweep opens an older build's entry
  // only on such an answer.
  readonly readSweepHealth: (origin: string) => Promise<HealthResult>;
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
  readonly log: MainLog;
  readonly stepBoundMs?: number;
  readonly vaultBoundMs?: number;
  readonly revokeBoundMs?: number;
  readonly verifyBoundMs?: number;
  readonly healthBoundMs?: number;
}

export const MISMATCH = 'profile_account_mismatch';

// The help for staying signed in, by main's platform (issue #2077): the page
// shows it as a link beside the keep choice, and a click on it leaves through
// main's link rules (links.ts) like any other. None where the platform's guide
// does not cover it yet. The renderer's own source names no outside address.
export function keepHelpFor(platform: string): string | null {
  return platform === 'linux' ? 'https://github.com/njdaniel/conch/blob/main/docs/desktop/linux.md#staying-signed-in' : null;
}

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
  // Profiles whose credential in memory the person chose not to keep: Retry
  // never puts one of these in the vault.
  readonly #unkept = new Set<string>();
  // The activation whose sign-in the person chose not to keep, if any. A new
  // activation (a switch, a sign-out, a restart) starts with the default: kept.
  #declined: Activation | null = null;
  #storage: Classification = { kind: 'temporarilyUnavailable' };
  #notice: ProfileNotice | null = null;
  // The verdict on the active profile's server, and the first required family
  // that answered with something this app does not understand.
  #verdict: Verdict | null = null;
  #invalid: string | null = null;
  // The sweep of #2217: whether its search of the vault has finished, the
  // whole sweep, the profiles whose old entry it is still ending (never read
  // as a sign-in meanwhile), and those whose entry it deleted without the
  // server ending it (the notice, while that profile is active).
  #listed: Promise<void> = Promise.resolve();
  #swept: Promise<void> = Promise.resolve();
  readonly #sweeping = new Map<string, Promise<void>>();
  readonly #unended = new Set<string>();
  // Profiles whose older build's sign-in the server confirmed ended this run:
  // a read that later deletes its file (one the sweep could not) needs no
  // notice.
  readonly #endedLegacy = new Set<string>();

  constructor(deps: ProfilesDeps) {
    this.#deps = deps;
    this.#switcher = new Switcher({
      hooks: deps.hooks,
      now: deps.now,
      log: deps.log,
      ...(deps.stepBoundMs === undefined ? {} : { boundMs: deps.stepBoundMs }),
      open: async (spec) =>
        deps.createActivation({
          ...spec,
          token: await this.tokenFor(spec.profile, spec.generation),
          onInvalid: (op) => {
            this.#reportInvalid(op, spec.generation);
          },
        }),
    });
  }

  get switcher(): Switcher {
    return this.#switcher;
  }

  get active(): Activation | null {
    return this.#switcher.current;
  }

  // swept settles when the sweep of #2217 this start began has finished (it
  // never rejects). Nothing waits for it but tests.
  get swept(): Promise<void> {
    return this.#swept;
  }

  // sweep starts the one sweep of this start, in the background (see the head
  // of this file). It never throws and never holds up start.
  #sweep(): void {
    let listed = (): void => undefined;
    this.#listed = new Promise<void>((resolve) => {
      listed = () => {
        resolve();
      };
    });
    this.#swept = this.#runSweep(listed).catch(() => undefined).finally(listed);
  }

  async #runSweep(listed: () => void): Promise<void> {
    const { store, vault } = this.#deps;
    if (store.corrupt) {
      // No entry's server is known: nothing is opened or deleted.
      this.#legacyLog('skipped');
      return;
    }
    const found = await within(vault.findLegacy().catch(() => null), this.#deps.vaultBoundMs ?? VAULT_BOUND_MS);
    if (!found.done || found.value === null) {
      this.#legacyLog('not-listed');
      return;
    }
    const ends = found.value.map((id) => {
      const end = (async () => {
        try {
          this.#legacyLog(await this.#endLegacy(id));
        } finally {
          this.#sweeping.delete(id);
        }
      })();
      // Set before anything can ask: the end above has not finished yet.
      this.#sweeping.set(id, end);
      return end;
    });
    listed();
    await Promise.all(ends);
  }

  // The longest one entry's end may take: its turn behind earlier work, the
  // health question, the decrypt, the revocation and the delete.
  #legacyBound(): number {
    const vault = this.#deps.vaultBoundMs ?? VAULT_BOUND_MS;
    return 2 * vault + (this.#deps.healthBoundMs ?? HEALTH_BOUND_MS) + (this.#deps.revokeBoundMs ?? REVOKE_BOUND_MS);
  }

  // endLegacy ends one entry sealed with the built-in key, bounded. Everything
  // happens in the profile's vault turn: the health question (bounded, so a
  // server that hangs holds the turn no longer than the health bound), then
  // the revocation or not. The credential exists only inside the use handed
  // to the vault: it goes to revokeStored and nowhere else.
  async #endLegacy(id: string): Promise<LegacyOutcome> {
    const profile = this.#deps.store.get(id);
    // Set by decide, in the turn.
    const asked = { answered: false };
    const revokeBound = this.#deps.revokeBoundMs ?? REVOKE_BOUND_MS;
    const decide = async (): Promise<((token: string) => Promise<'ended' | 'unconfirmed' | 'unsent'>) | null> => {
      // No saved server owns it, so there is no one to tell or ask.
      if (profile === undefined) return null;
      const health = await within(
        this.#deps.readSweepHealth(profile.origin).catch((): HealthResult => ({ kind: 'unreachable' })),
        this.#deps.healthBoundMs ?? HEALTH_BOUND_MS,
      );
      asked.answered = health.done && health.value.kind === 'answered';
      if (!asked.answered) return null;
      return async (token) => {
        const r = await within(this.#deps.revokeStored(profile.origin, token), revokeBound);
        return r.done ? r.value : 'unconfirmed';
      };
    };
    const r = await within(this.#deps.vault.endLegacy(id, decide), this.#legacyBound());
    const ended = r.done ? r.value : null;
    if (ended !== null && ended.kind === 'ended' && ended.opened && ended.used === 'ended') {
      this.#endedLegacy.add(id);
      return ended.removed ? 'ended' : 'ended-not-deleted';
    }
    // Not shown ended at the server, whatever the delete did: a copy of it may
    // still work there.
    this.#unended.add(id);
    if (ended === null) return 'late';
    if (ended.kind === 'not-legacy') return 'replaced';
    if (!ended.removed) return 'not-deleted';
    if (ended.kind === 'loose-mode') return 'deleted-loose-mode';
    if (profile === undefined) return 'deleted-no-profile';
    if (!asked.answered) return 'deleted-unreachable';
    return ended.opened ? 'deleted-unconfirmed' : 'deleted-unopened';
  }

  // One fixed line per outcome, from the closed LegacyOutcome vocabulary.
  #legacyLog(outcome: LegacyOutcome): void {
    this.#deps.log(`conch-desktop: a sign-in saved by an older build (v10): ${outcome}`, { ring: true });
  }

  // start loads the list, starts the sweep of #2217 without waiting for it,
  // asks the vault what storage is, and activates the profile that was active
  // last time (or the server list).
  async start(): Promise<void> {
    const { store } = this.#deps;
    await store.load();
    this.#sweep();
    this.#storage = await this.#classify();
    const last = store.corrupt || store.active === null ? undefined : store.get(store.active);
    await this.#switcher.switchTo(last ?? null, { reason: 'switch' });
  }

  // tokenFor is the credential a profile starts with: main's memory first (a
  // session-only credential of this run), then the vault. A stored credential
  // that cannot be used is "sign in again" and nothing else.
  async tokenFor(profile: Profile | null, generation: number): Promise<string | null> {
    // Whatever was said of the last server is not said of this one.
    this.#verdict = null;
    this.#invalid = null;
    if (profile === null) return null;
    // The server's release first. A server that is not on the allowlist (or
    // did not say) gets no automatic sign-in: its stored credential is not
    // read, not sent to it, and not removed; a credential held in memory is
    // not handed over either.
    const verdict = await this.#compatible(profile);
    if (!this.#switcher.isCurrent(generation)) return null;
    this.#verdict = verdict;
    if (!permits(verdict.state)) return null;
    const kept = this.#held.get(profile.id);
    if (kept !== undefined) return kept;
    if (this.#signedOut.has(profile.id)) {
      // Signed out: it starts signed out, without asking the vault anything
      // (a delete that hangs would hold a read behind it). Deleting is tried
      // again in the background.
      void this.#purge(profile.id);
      return null;
    }
    // The sweep of #2217 began before this activation and its search is local;
    // an entry it is ending is never a sign-in, and is not read here (its turn
    // in the vault may be waiting on the server).
    await within(this.#listed, this.#deps.vaultBoundMs ?? VAULT_BOUND_MS);
    if (!this.#switcher.isCurrent(generation)) return null;
    if (this.#sweeping.has(profile.id)) {
      this.#deps.log('conch-desktop: a sign-in saved by an older build is being removed, so it was not used', { ring: true });
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
      this.#deps.log('conch-desktop: the keyring did not answer in time, so no saved sign-in was read', { ring: true });
      return null;
    }
    const got = read.value;
    switch (got.kind) {
      case 'found':
        this.#storage = { kind: 'protected' };
        return this.#verified(profile, got.secret, generation);
      case 'absent':
        this.#deps.log('conch-desktop: no saved sign-in for this server', { ring: true });
        return null;
      case 'legacyRemoved':
        // An older build's entry the sweep did not end (it could not search
        // the vault, or delete the file): deleted unread, so not ended at the
        // server, unless the sweep had it ended there.
        if (!this.#endedLegacy.has(profile.id)) this.#unended.add(profile.id);
        else this.#notice = 'sign-in-again';
        this.#deps.log('conch-desktop: a sign-in saved by an older build was deleted unread; sign in again', { ring: true });
        return null;
      case 'unusable':
        this.#notice = 'sign-in-again';
        this.#deps.log('conch-desktop: a saved sign-in could not be read; sign in again', { ring: true });
        return null;
      case 'sessionOnly':
        this.#storage = got;
        this.#deps.log(`conch-desktop: saved sign-ins are not available here (${got.reason})`, { ring: true });
        return null;
      case 'temporarilyUnavailable':
        this.#storage = got;
        this.#deps.log('conch-desktop: the keyring did not answer, so no saved sign-in was read', { ring: true });
        return null;
    }
  }

  async #compatible(profile: Profile): Promise<Verdict> {
    const asked = await within(
      this.#deps.readHealth(profile.origin).catch((): HealthResult => ({ kind: 'unreachable' })),
      this.#deps.healthBoundMs ?? HEALTH_BOUND_MS,
    );
    return judge(this.#deps.compat, asked.done ? asked.value : { kind: 'unreachable' });
  }

  // reportInvalid records the first answer of a required family that was not
  // the document this app was built for, from the activation now current.
  #reportInvalid(op: OperationId, generation: number): void {
    const family = REQUIRED_FAMILIES[op];
    if (family === undefined || this.#invalid !== null || !this.#switcher.isCurrent(generation)) return;
    this.#invalid = family;
    this.#deps.log(`conch-desktop: a ${family} answer was not what this app version understands`);
  }

  // signInPermitted: whether a sign-in may happen in the active window, which
  // is only when its server is on the allowlist (or the flag lets it through).
  signInPermitted(): boolean {
    return this.#verdict !== null && permits(this.#verdict.state);
  }

  #compatView(): CompatView {
    return {
      state: this.#verdict?.state ?? 'none',
      server: this.#verdict?.server ?? null,
      desktop: this.#deps.desktopVersion,
      override: this.#deps.compat.override,
      invalid: this.#invalid,
    };
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
      this.#deps.log('conch-desktop: a saved sign-in could not be checked with the server, so it was not used', { ring: true });
      return null;
    }
    if (checked.value.kind === 'rejected') {
      // Signed out elsewhere, revoked or expired: the server's answer is the
      // last word, and the person is told so in plain words.
      this.#notice = 'sign-in-ended';
      this.#deps.log('conch-desktop: the server ended a saved sign-in; it was deleted', { ring: true });
      this.#signedOut.add(profile.id);
      void this.#purge(profile.id);
      return null;
    }
    const { account } = checked.value;
    const bound = profile.principal;
    if (bound !== null && account.id === bound.id && account.lasting && !account.guest) {
      this.#deps.log('conch-desktop: a saved sign-in was restored', { ring: true });
      return token;
    }
    this.#deps.log('conch-desktop: a stored sign-in did not match the account it is bound to; refused', { ring: true });
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
    this.#deps.log('conch-desktop: could not delete a profile vault entry', { ring: true });
    this.#deps.log(`conch-desktop: could not delete the vault entry of profile ${id}`);
    return false;
  }

  #profileView(p: Profile, active: string | null): ProfileView {
    return {
      id: p.id,
      label: p.label,
      host: new URL(p.origin).host,
      origin: p.origin,
      account: p.principal === null ? null : p.principal.name,
      active: p.id === active,
    };
  }

  // findByOrigin is every saved profile for a canonical origin, in list order:
  // there can be several (another account on the same server is another
  // entry). Display metadata only; the invite router (../invites.ts) offers
  // them by name and account.
  findByOrigin(origin: string): readonly ProfileView[] {
    const active = this.#switcher.current?.profileId ?? null;
    return this.#deps.store
      .list()
      .filter((p) => p.origin === origin)
      .map((p) => this.#profileView(p, active));
  }

  // Whether the window in force is one whose sign-in is not to be kept: the
  // person turned the choice off in it, or earlier for its profile this run.
  #declinedNow(): boolean {
    const current = this.#switcher.current;
    if (current === null) return false;
    return this.#declined === current || (current.profileId !== null && this.#unkept.has(current.profileId));
  }

  view(): ProfilesView {
    const { store, platform } = this.#deps;
    const active = this.#switcher.current?.profileId ?? null;
    return {
      profiles: store.list().map((p) => this.#profileView(p, active)),
      active,
      storage: storageOf(this.#storage),
      keep: !this.#declinedNow(),
      keepHelp: keepHelpFor(platform),
      recovery: store.corrupt,
      // The sweep's notice is the active profile's until the person removes
      // or signs out of it; any other notice of the moment comes first.
      notice: this.#notice ?? (active !== null && this.#unended.has(active) ? 'legacy-not-ended' : null),
      compat: this.#compatView(),
      platform,
    };
  }

  // The choice for the Linux screen share's sound, of the profile in force
  // (issue #1854): public, kept with the profile, the default when never set.
  // None when no profile is active: no choice is ever read from the page.
  shareSoundChoice(): ShareSoundChoice | null {
    const id = this.#switcher.current?.profileId ?? null;
    return id === null ? null : this.#deps.store.shareSound(id);
  }

  // Keeps a choice for the profile in force, after the person's act (the IPC
  // layer has checked the act). The profile and the switch generation are
  // those of this call, and are asked again when the write's turn comes: a
  // switch or sign-out that overtook it writes nothing and says false. A
  // choice the profile already has is true and writes nothing.
  async setShareSoundChoice(choice: ShareSoundChoice): Promise<boolean> {
    const activation = this.#switcher.current;
    const id = activation?.profileId ?? null;
    const generation = this.#switcher.generation;
    if (id === null) return false;
    try {
      return await this.#deps.store.setShareSound(id, choice, () => this.#switcher.current === activation && this.#switcher.generation === generation);
    } catch {
      this.#deps.log('conch-desktop: could not keep a profile share sound choice', { ring: true });
      this.#deps.log(`conch-desktop: could not keep the share sound choice of profile ${id}`);
      return false;
    }
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
    // An older build's entry the sweep is ending is ended first (it holds the
    // profile's vault turn; the reads and the delete below would wait behind
    // it anyway, and might give up at their bound).
    const sweeping = this.#sweeping.get(id);
    if (sweeping !== undefined) await within(sweeping, this.#legacyBound());
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
    this.#unkept.delete(id);
    this.#unended.delete(id);
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
        this.#unkept.delete(id);
        this.#unended.delete(id);
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
  // applies the binding rule and then keeps a member's credential: in the vault
  // when storage is protected, in memory otherwise. An operator's stays in the
  // window, with a notice explaining why. false when the profile belongs
  // to another account: the credential is then forgotten and revoked.
  //
  // expected is the credential that sign-in put in force, read before
  // anything was awaited (keptCredential passes the one it asked whoami
  // about); by default the one in force when this is called. Only that
  // credential is kept: if another is in force once the binding is written (a
  // second sign-in or a rotation overtook this one), this keeps nothing and
  // the later sign-in's own call decides.
  async signedIn(account: Account, expected?: string | null): Promise<boolean> {
    const { store } = this.#deps;
    const current = this.#switcher.current;
    if (current === null || current.profileId === null) return false;
    const id = current.profileId;
    const signedInWith = expected === undefined ? current.token() : expected;
    // A guest is not an account of the server's people: nothing binds to it,
    // and nothing is kept.
    if (account.guest) return true;
    const bound = await store.bind(id, { id: account.id, name: account.name } satisfies Principal).catch(() => null);
    if (bound === null || !bound.ok) {
      const reason = bound === null ? 'not saved' : bound.reason;
      this.#deps.log(`conch-desktop: a profile sign-in was refused: ${reason}`, { ring: true });
      this.#deps.log(`conch-desktop: a sign-in on profile ${id} was refused: ${reason}`);
      if (this.#switcher.current === current) {
        await current.signOut();
        // The client only sees a refusal it does not know; this says why.
        this.#notice = 'account-mismatch';
      }
      return false;
    }
    // A completed sign-in from a window already left must not change the
    // notice of the window now in force.
    if (this.#switcher.current !== current) return true;
    if (!account.lasting) {
      this.#notice = 'operator-window-only';
      this.#deps.log('conch-desktop: an operator sign-in is kept for this window only', { ring: true });
      return true;
    }
    if (this.#notice === 'operator-window-only') this.#notice = null;
    const token = current.token();
    // The window may have moved on while the store was written, or another
    // credential been put in force in it.
    if (token === null || token !== signedInWith || this.#switcher.current !== current) return true;
    // Declined in this window, or earlier for this profile in this run (a
    // later window of it, after a switch, shows no box: a password change
    // there must not keep what the person chose not to keep).
    if (this.#declined === current || this.#unkept.has(id)) {
      this.#keepInMemory(id, token);
      this.#deps.log('conch-desktop: a sign-in is kept until the app closes, as chosen', { ring: true });
      return true;
    }
    const put = await this.#put(id, token);
    if (this.#switcher.current !== current) return true;
    if (this.#declined === current) {
      // Declined while the write was on its way: what it wrote comes out.
      this.#keepInMemory(id, token);
      this.#deps.log('conch-desktop: a sign-in is kept until the app closes, as chosen', { ring: true });
      return true;
    }
    if (put.kind === 'stored') {
      this.#storage = { kind: 'protected' };
      this.#held.delete(id);
      this.#signedOut.delete(id);
      this.#deps.log('conch-desktop: a sign-in was kept in the keyring', { ring: true });
    } else {
      // Not kept: this run only, in memory, and said so.
      this.#held.set(id, token);
      if (put.kind === 'sessionOnly' || put.kind === 'temporarilyUnavailable') this.#storage = put;
      // A write that failed or did not finish leaves the status as it was
      // (the keyring answered a moment ago), so the notice says it instead.
      if (put.kind === 'failed') this.#notice = 'not-kept';
      const why: SessionOnlyReason | 'failed' | 'temporarilyUnavailable' = put.kind === 'sessionOnly' ? put.reason : put.kind;
      this.#deps.log(`conch-desktop: a sign-in is kept until the app closes (${why})`, { ring: true });
    }
    return true;
  }

  // keepInMemory holds a member's credential for this run only, never to be
  // put by a later Retry or keep, and deletes the profile's entry, so that
  // the next start does not bring back what was not chosen.
  #keepInMemory(id: string, token: string): void {
    this.#held.set(id, token);
    this.#unkept.add(id);
    this.#signedOut.add(id);
    void this.#purge(id);
  }

  // setKeep records the person's "Keep me signed in on this computer" choice
  // for the activation in force (the sign-in form shows it beside the
  // password). Turned back on after an unkept member sign-in, it puts that
  // sign-in in the vault. Turned off after a sign-in was kept (a click that
  // raced the sign-in), the entry is deleted and nothing is copied anywhere:
  // the window keeps its own sign-in, and the profile starts signed out next
  // time. Never moving the window's credential into main's memory here is
  // what keeps an operator's within its window (ADR-012). Nothing is kept
  // for no profile.
  async setKeep(keep: boolean): Promise<ProfilesView> {
    const current = this.#switcher.current;
    if (current === null || current.profileId === null) return this.view();
    const id = current.profileId;
    if (keep) {
      if (this.#declined === current) this.#declined = null;
      if (this.#unkept.has(id)) {
        this.#unkept.delete(id);
        const token = this.#held.get(id);
        if (token !== undefined && this.#switcher.current === current) {
          const put = await this.#put(id, token);
          if (this.#declined === current || this.#switcher.current !== current) {
            // Turned off again (or the window left) while the write was on
            // its way: not kept, and whatever it wrote comes out. The purge is
            // issued whatever the write said: one reported failed at the
            // bound may still land, and the vault runs this delete after it.
            // The credential stays "not kept" only while it is still the one
            // held: a sign-out in between ended it, and the next form starts
            // with the default.
            if (this.#held.get(id) === token) this.#unkept.add(id);
            this.#signedOut.add(id);
            await this.#purge(id);
            return this.view();
          }
          if (put.kind === 'stored' && this.#held.get(id) === token) {
            this.#held.delete(id);
            this.#signedOut.delete(id);
            this.#storage = { kind: 'protected' };
          } else if (put.kind === 'sessionOnly' || put.kind === 'temporarilyUnavailable') this.#storage = put;
        }
      }
      return this.view();
    }
    this.#declined = current;
    // Already signed in and kept in the vault: the entry comes out. A
    // credential held in memory (session-only, a keep that failed, or already
    // unkept) stays held and is never put by Retry once unkept; its entry is
    // deleted too, because a write reported failed at the bound may still
    // land, and the vault runs this delete after it.
    if (current.token() !== null && !this.#held.has(id)) {
      this.#signedOut.add(id);
      await this.#purge(id);
    } else if (this.#held.has(id)) {
      this.#unkept.add(id);
      this.#signedOut.add(id);
      await this.#purge(id);
    }
    return this.view();
  }

  // keptCredential is signedIn for a pasted credential that has just been put
  // in force: who it is has to be asked.
  async keptCredential(): Promise<boolean> {
    const current = this.#switcher.current;
    // The credential whoami is asked about is the one that may be kept.
    const asked = current?.token() ?? null;
    const account = current === null ? null : await current.whoami();
    if (account !== null) return this.signedIn(account, asked);
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
        if (this.#held.get(id) !== token || this.#signedOut.has(id) || this.#unkept.has(id) || this.#deps.store.get(id) === undefined) continue;
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
