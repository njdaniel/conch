// Main's part of the Linux screen share's sound (issue #1854, D4-09e of #772;
// docs/design/linux-share-audio.md §5.3, §9, §10, §11, §12; linux-desktop.md
// §4, §8). Input: the page's requests (IPC has checked the caller, the
// document, the person's act), the voice service's confirmed view, the
// companion's live share and every end of it, and the profile's stored choice.
// Output: the consent PUT and its DELETE (main's authenticated client), the
// helper's set_share_audio and set_share_audio_mute (through the voice
// service), and main's part of the view (ShareSoundView).
// Owns: the one share's sound at a time, from the consent to its withdrawal.
//
// The order, always (§5.3):
//   start:  consent PUT {scope: system_excluding_call, publisher: person}, its
//           answer validated and bound to this share, and only then
//           set_share_audio {capturing: true, scope, application_name};
//   end:    set_share_audio {capturing: false}, then DELETE, every time.
// Nothing sends a capture command without a recorded, valid consent for the
// share it names; the scope is the one literal of the Linux route (never
// application) and the application name is main's own registered one: the page
// supplies neither, nor any process, device or share identifier.
//
// What it keeps:
//  - the switch (CONCH_LINUX_SHARE_AUDIO=1 on Linux) is read by main once, at
//    startup, and given to this module; with it off no instance is made, the
//    view says nothing of sound and no IPC function answers;
//  - a request is bound to the live share of the document that asked: a
//    capture main itself granted (display-capture.ts answered the portal's
//    pick, captureGranted) while this companion was held, in this document. A
//    companion grant alone is not a share: Watch holds one too. Neither is the
//    page's word. A share's sound is one Owned record with a token the page
//    must return to end it, so a late end of an older share never stops a
//    newer one's. The record of the capture ends with the share's sound, with
//    the companion, and with the page's word that its share ended
//    (shareEnded, which needs no token, so a share that never had a sound
//    record clears it too). What that leaves: a compromised page that does not
//    say its share ended keeps the record as long as the companion lives once
//    one pick was answered, and can ask for sound on that companion later.
//    conchd records the consent only for a share claimed at that moment, which
//    such a page can claim again. Main ending the sound when presence shows the
//    share ended is a separate issue;
//  - every step after an await asks again whether this record is still the
//    one (not ended, not replaced) before it writes anything, so a consent
//    answer that arrives after a stop, a leave or a new share sends no command
//    and withdraws nothing of another share's;
//  - an end waits for a consent still in flight before it withdraws (a late
//    PUT could otherwise land after the DELETE), and a new share's sound waits
//    for the older one's end to finish;
//  - failures leave the share running silent with fixed words (ShareSoundReason):
//    the server's 400 on `publisher` is "this server has no route"; any other
//    refusal is asked once more after one poller pass; publish_refused is
//    repeated once after 1.5 s; both waits are cancelled by the end, and the
//    repeat also by the sound being turned off, so it never writes a capture
//    for a share whose consent was withdrawn;
//  - a 2xx consent answer that is not this share's (another scope, publisher
//    or start time) is withdrawn at once, not left recorded until the end.
//
// No log line carries a token, a room, a channel, a path, a device or an
// application name: reasons only.
import { VOICE_CONTROL_CAPTURE_STOP_MS, type VoiceCompanionAudioRequestV1, type VoiceControlShareAudioProblem } from '../../../web/src/schema/generated.ts';
import type { NativeVoiceView, ShareSoundChooseResult, ShareSoundChoice, ShareSoundReason, ShareSoundStartResult, ShareSoundView, VoiceCommandResult } from '../../../web/src/platform/desktop/bridge.ts';
import type { Exchange } from '../api/client.ts';
import { LINUX_SHARE_AUDIO_SCOPE, validApplicationName } from './protocol.ts';
import type { CommandResult } from './supervisor.ts';
import type { Companion, RemoveReason, ShareRef } from './companion.ts';
import { COMPANION_PASS_MS } from './companion.ts';

// The one switch, read by main alone: the exact value "1", on Linux. Never the
// renderer's, never a profile's, never an argument.
export const SHARE_AUDIO_FLAG = 'CONCH_LINUX_SHARE_AUDIO';

export function linuxShareAudioEnabled(env: NodeJS.ProcessEnv, platform: NodeJS.Platform): boolean {
  return platform === 'linux' && env[SHARE_AUDIO_FLAG] === '1';
}

// The name this app's own audio streams carry (the registered application
// name). The helper leaves it out of the capture so the call is not shared
// back into itself. A name the command could not carry is the product's.
export const FALLBACK_APPLICATION_NAME = 'Conch';

export function registeredApplicationName(name: string): string {
  return validApplicationName(name) ? name : FALLBACK_APPLICATION_NAME;
}

// One poller pass before the one more ask of a refused consent, and before the
// one repeat of a refused publish (§11).
export const CONSENT_RETRY_MS = COMPANION_PASS_MS;
export const PUBLISH_RETRY_MS = 1_500;

// How long a stop waits for the helper's ack before the withdrawal goes on:
// the contract's capture-stop bound, so no end is held by a slow helper.
export const STOP_ACK_BOUND_MS = VOICE_CONTROL_CAPTURE_STOP_MS;
// The pause before a stop is written again after the helper said busy.
const STOP_BUSY_PAUSE_MS = 50;
// The bounds of the two REST calls. Stop (500) and withdrawal (2000) together
// fit the teardown sequence's step bound (3000).
export const CONSENT_TIMEOUT_MS = 5_000;
export const WITHDRAW_TIMEOUT_MS = 2_000;

// The helper's problems, in the words' vocabulary. not_connected is absent: a
// refusal while the helper reconnects is silent until it is connected again.
const PROBLEM_REASONS: Readonly<Partial<Record<VoiceControlShareAudioProblem, ShareSoundReason>>> = {
  no_pipewire: 'no-pipewire',
  no_process_tree: 'no-process-tree',
  publish_refused: 'publish-refused',
  unsupported_scope: 'unavailable',
  unsupported: 'unavailable',
};

// What the supervisor and the REST calls are asked through; a test stands in.
export interface ShareSoundVoice {
  // The view without main's share sound: the helper's confirmed state.
  plain(): NativeVoiceView;
  subscribe(listener: (view: NativeVoiceView) => void): () => void;
  setShareAudio(want: { readonly capturing: false } | { readonly capturing: true; readonly applicationName: string }): Promise<CommandResult>;
  setShareAudioMute(muted: boolean): Promise<CommandResult>;
  // Sends the view again after this module's part of it changed.
  shareSoundChanged(): void;
}

export interface ShareSoundDeps {
  readonly voice: ShareSoundVoice;
  readonly companion: Pick<Companion, 'current'>;
  // The stored choice of the profile in force (null: no profile), and the
  // person's act of changing it (the IPC layer has checked the act).
  readonly choice: () => ShareSoundChoice | null;
  readonly setChoice: (choice: ShareSoundChoice) => Promise<boolean>;
  // The registered application name (registeredApplicationName).
  readonly applicationName: string;
  readonly log: (line: string) => void;
  readonly consentRetryMs?: number;
  readonly publishRetryMs?: number;
  readonly stopAckBoundMs?: number;
}

type Outcome = 'ok' | 'unavailable' | 'failed' | 'ended';

// One share's sound, from its consent to its withdrawal.
class Owned {
  cancelled = false;
  // Whether the sound is wanted now: the person's last choice for this share.
  // An enable that finds it false after its consent sends no command.
  wanted = true;
  // Enable and disable run one at a time, in the order asked.
  chain: Promise<unknown> = Promise.resolve();
  // The enable still running, for a second start of the same moment.
  starting: Promise<Outcome> | null = null;
  // The consent PUT in flight, if one is.
  inflight: Promise<Exchange> | null = null;
  // A PUT was sent: a withdrawal is owed.
  consentSent = false;
  // set_share_audio {capturing: true} was written: a stop is owed.
  commanded = false;
  // The share's start time as the server recorded it with the consent: the
  // binding of the consent to this share.
  startedAt: string | null = null;
  // The words, once a failure is final.
  problem: ShareSoundReason | undefined;
  // publish_refused: repeated once, and not shown while the repeat is pending.
  retried = false;
  retrying = false;
  // The record's waits: woken (cancelled) by its end.
  readonly wakers = new Set<() => void>();
  // The publish_refused repeat's wait: woken by the end, and by the sound
  // being turned off (a stop and withdrawal of this record).
  readonly repeatWakers = new Set<() => void>();

  constructor(
    readonly share: ShareRef,
    readonly token: number,
  ) {}
}

export class ShareSound {
  readonly #deps: ShareSoundDeps;
  #owned: Owned | null = null;
  #serial = 0;
  // The end of the share's sound before: a new share's waits for it.
  #tail: Promise<unknown> = Promise.resolve();
  // The server refused `publisher` (an older server): no route until the
  // identity changes (forget).
  #serverRefused = false;
  #lastView = '';
  // The capture main granted for the live share (captureGranted): the
  // companion held and the document, when the portal's pick was answered with
  // a source. Cleared by the share sound's end and by the companion's.
  #capture: { readonly companion: number; readonly document: number } | null = null;
  readonly #off: () => void;

  constructor(deps: ShareSoundDeps) {
    this.#deps = deps;
    this.#lastView = JSON.stringify(this.view());
    this.#off = deps.voice.subscribe(() => {
      this.#onVoice();
    });
  }

  // close detaches from the voice service.
  close(): void {
    this.#off();
  }

  // forget drops what was learned of the server (the identity is ending:
  // a switch, a sign-out or quit). The share's own end is the companion's.
  forget(): void {
    this.#serverRefused = false;
    this.#changed();
  }

  // captureGranted is display-capture's word that main answered the portal's
  // pick with a source for `document` (the person chose a screen or window):
  // the share the sound may be added to. It is bound to the companion held
  // now; with none held, or one of another document, nothing is recorded.
  captureGranted(document: number): void {
    const share = this.#deps.companion.current();
    this.#capture = share === null || share.document !== document ? null : { companion: share.id, document };
  }

  // captured says whether main granted a capture while this companion was
  // held, in its document: whether `share` is a share and not only a watch.
  #captured(share: ShareRef): boolean {
    const c = this.#capture;
    return c !== null && c.companion === share.id && c.document === share.document;
  }

  // ---------------------------------------------------------------- view

  // view is main's part of the voice view: the profile's choice and, at most,
  // one fixed reason. "With sound" is not here: only the helper's confirmed
  // state (helper.shareAudio) says that.
  // A record whose capture is no longer the live one (the share ended and the
  // page's end did not come) reports no failure: it is no share's any more.
  view(): ShareSoundView {
    const choice = this.#deps.choice() ?? 'system_excluding_call';
    const owned = this.#owned;
    const reason = owned === null || !this.#captured(owned.share) ? this.#route() : owned.problem;
    return reason === undefined ? { choice } : { choice, unavailable: reason };
  }

  #changed(): void {
    const now = JSON.stringify(this.view());
    if (now === this.#lastView) return;
    this.#lastView = now;
    this.#deps.voice.shareSoundChanged();
  }

  // route says why no route is available at this moment, or undefined: the
  // server refused `publisher`, or the helper running does not speak v3, or
  // (during an asked-for sound) it said unavailable.
  #route(): ShareSoundReason | undefined {
    if (this.#serverRefused) return 'old-server';
    const view = this.#deps.voice.plain();
    const helper = view.status === 'running' ? view.helper : undefined;
    if (helper === undefined) return undefined;
    if (helper.version < 3) return 'old-helper';
    const audio = helper.shareAudio;
    if (audio?.state === 'unavailable' && audio.problem !== undefined) return PROBLEM_REASONS[audio.problem];
    return undefined;
  }

  // ------------------------------------------------------------ requests

  // start adds the sound to the live share of `document`: the page asks after
  // its share is live. The checks are main's: a capture main granted for that
  // document under the companion held (not a companion alone), the stored
  // choice, a route. The result says what came of the ask; the sound is on
  // only when the view's helper.shareAudio says so. A start that failed still
  // names its record (share), so the page ends it with the share and its
  // words do not outlive it.
  async start(document: number): Promise<ShareSoundStartResult> {
    const share = this.#deps.companion.current();
    if (share === null || share.document !== document || !this.#captured(share)) return { ok: false, error: 'no-share' };
    if (this.#deps.choice() !== 'system_excluding_call') return { ok: false, error: 'not-offered' };
    let o = this.#owned;
    // A second ask while the first is still running is the same moment.
    if (o !== null && o.share.id === share.id && o.starting !== null) return this.#report(o, await o.starting);
    // Any record of this companion's still here is of an earlier share of it
    // (the page's end did not arrive): it ends first, in order. The capture
    // just checked is this share's, so that end keeps it.
    if (o !== null) void this.#end(o, 'unused', true);
    o = new Owned(share, ++this.#serial);
    this.#owned = o;
    const mine = o;
    const run = this.#enable(mine);
    mine.starting = run;
    void run.finally(() => {
      if (mine.starting === run) mine.starting = null;
    });
    return this.#report(mine, await run);
  }

  #report(o: Owned, outcome: Outcome): ShareSoundStartResult {
    switch (outcome) {
      case 'ok':
        return { ok: true, share: o.token };
      case 'ended':
        return { ok: false, error: 'no-share' };
      case 'unavailable':
        return this.#owned === o ? { ok: false, error: 'unavailable', share: o.token } : { ok: false, error: 'unavailable' };
      case 'failed':
        return this.#owned === o ? { ok: false, error: 'failed', share: o.token } : { ok: false, error: 'failed' };
    }
  }

  // choose keeps the person's choice for the profile in force and, if a share
  // of `document` is live, applies it: off is the stop then the withdrawal;
  // on is the consent then the command, the same order as a start. On asks
  // nothing of conchd or the helper unless main granted a capture for the
  // companion held (a watch is no share): the choice is kept for the next
  // share. The IPC layer has taken the person's act.
  async choose(choice: ShareSoundChoice, document: number): Promise<ShareSoundChooseResult> {
    if (!(await this.#deps.setChoice(choice))) return { ok: false, error: 'not-saved' };
    this.#changed();
    const share = this.#deps.companion.current();
    if (share === null || share.document !== document) return { ok: true, share: null };
    if (choice === 'none') {
      const o = this.#owned;
      if (o !== null && o.share.id === share.id) {
        o.wanted = false;
        await this.#disable(o);
      }
      return { ok: true, share: null };
    }
    if (!this.#captured(share)) return { ok: true, share: null };
    let o = this.#owned;
    if (o === null || o.share.id !== share.id) {
      if (o !== null) void this.#end(o, 'unused', true);
      o = new Owned(share, ++this.#serial);
      this.#owned = o;
    }
    o.wanted = true;
    await this.#enable(o);
    // The token is for the share's sound this record holds, if it is still the live one.
    return { ok: true, share: this.#owned === o ? o.token : null };
  }

  // mute is the share-sound mute, the helper's (confirmed in the view). Only
  // while a sound was asked for of the live share; microphone mute and deafen
  // are other commands and this touches neither.
  mute(muted: boolean, document: number): Promise<VoiceCommandResult> {
    const o = this.#owned;
    const share = this.#deps.companion.current();
    if (o === null || o.cancelled || !o.commanded || share === null || share.id !== o.share.id || share.document !== document) {
      return Promise.resolve({ ok: false, error: 'not-running' });
    }
    return this.#deps.voice.setShareAudioMute(muted);
  }

  // end is the page's word that the share it started the sound for has ended.
  // It names that share by its token; one that is not the live one is ignored.
  async end(token: number): Promise<boolean> {
    const o = this.#owned;
    if (o === null || o.token !== token) return false;
    await this.#end(o, 'page');
    return true;
  }

  // shareEnded is the page's word that its share in `document` ended, whether
  // or not a sound was ever asked for it: the capture is no share any more,
  // and any record of a share's sound in that document ends (the stop, then
  // the withdrawal). It needs no token and no act: it only takes away. A
  // later choice of "on" while the companion stays for watching then asks
  // nothing. Answers whether it ended or cleared anything.
  async shareEnded(document: number): Promise<boolean> {
    const had = this.#capture?.document === document;
    if (had) this.#capture = null;
    const o = this.#owned;
    if (o !== null && o.share.document === document) {
      await this.#end(o, 'page share end');
      return true;
    }
    if (had) this.#changed();
    return had;
  }

  // ------------------------------------------------------------ the ends

  // ended is the companion's word that its share ended, for any reason. It
  // names the companion held, so a record of another (newer) one is untouched.
  ended(share: ShareRef, reason: RemoveReason): Promise<void> {
    if (reason === 'switch' || reason === 'sign-out' || reason === 'quit') this.#serverRefused = false;
    if (this.#capture?.companion === share.id) this.#capture = null;
    const o = this.#owned;
    if (o === null || o.share.id !== share.id) return Promise.resolve();
    return this.#end(o, reason);
  }

  // leaving is a leave of voice (any: the person's, a suspend's, a teardown's):
  // the sound ends before the helper's leave is written.
  leaving(): Promise<void> {
    const o = this.#owned;
    return o === null ? Promise.resolve() : this.#end(o, 'leave');
  }

  // #end: the record goes first, so nothing after an await in a step of it
  // writes anything; then the stop is written (now, synchronously) and the
  // withdrawal follows it. A new share's sound waits for this to finish. The
  // share's capture goes with it, unless a newer record of the same companion
  // is replacing this one (keepCapture). This covers a share that had a sound
  // record; one that never did (the choice none, a start refused before it
  // made one) is cleared by shareEnded.
  #end(o: Owned, reason: string, keepCapture = false): Promise<void> {
    if (this.#owned === o) this.#owned = null;
    if (!keepCapture && this.#capture?.companion === o.share.id) this.#capture = null;
    o.cancelled = true;
    for (const wake of [...o.wakers, ...o.repeatWakers]) wake();
    this.#changed();
    this.#deps.log(`share sound: ending (${reason})`);
    const done = this.#stopAndWithdraw(o);
    this.#tail = this.#tail.then(() => done);
    return done;
  }

  // stopAndWithdraw: the helper's stop, then the DELETE; the stop is written
  // before this returns its first promise. A consent still in flight lands
  // before the DELETE, which then removes whatever it recorded.
  // A publish_refused repeat still waiting is cancelled first: once the stop
  // is written nothing may write a capture for this record until a new
  // consent is recorded.
  async #stopAndWithdraw(o: Owned): Promise<void> {
    for (const wake of [...o.repeatWakers]) wake();
    o.retrying = false;
    const stop = o.commanded ? this.#stop() : Promise.resolve();
    o.commanded = false;
    await o.inflight?.catch(() => undefined);
    await stop;
    if (o.consentSent) {
      o.consentSent = false;
      await this.#withdraw(o);
    }
  }

  // #stop writes set_share_audio {capturing: false} and waits for the ack, at
  // most STOP_ACK_BOUND_MS. A helper that is gone (not running, lost) leaves
  // nothing to stop: the withdrawal alone follows. A busy helper (too many
  // commands unanswered) is asked again until the bound.
  async #stop(): Promise<void> {
    const bound = this.#deps.stopAckBoundMs ?? STOP_ACK_BOUND_MS;
    const bounded = { over: false, timer: undefined as NodeJS.Timeout | undefined };
    const isOver = (): boolean => bounded.over;
    const late = new Promise<void>((resolve) => {
      bounded.timer = setTimeout(() => {
        bounded.over = true;
        resolve();
      }, bound);
    });
    const attempts = (async () => {
      for (;;) {
        const result = await this.#deps.voice.setShareAudio({ capturing: false });
        if (result.ok || result.error !== 'busy' || isOver()) return;
        await new Promise<void>((resolve) => setTimeout(resolve, STOP_BUSY_PAUSE_MS));
        if (isOver()) return;
      }
    })().catch(() => undefined);
    try {
      await Promise.race([attempts, late]);
    } finally {
      clearTimeout(bounded.timer);
    }
    if (bounded.over) this.#deps.log('share sound: the helper did not answer the stop in time');
  }

  // #withdraw is the DELETE of the consent: once, never retried, bounded. A
  // failure is logged by its kind alone.
  async #withdraw(o: Owned): Promise<void> {
    let exchange: Exchange;
    try {
      exchange = await o.share.client.send({ op: 'voiceShareAudioEnd', params: { channel: o.share.channel }, timeoutMs: WITHDRAW_TIMEOUT_MS });
    } catch {
      exchange = { kind: 'none' };
    }
    if (!(exchange.kind === 'answer' && exchange.ok)) this.#deps.log('share sound: the withdrawal was not confirmed');
  }

  // ----------------------------------------------------- enable and disable

  // gone says whether this record is no longer the share's sound.
  #gone(o: Owned): boolean {
    return o.cancelled || this.#owned !== o;
  }

  // sameShare says whether the share and the helper session this record was
  // made under are still the live ones.
  #sameShare(o: Owned): boolean {
    const share = this.#deps.companion.current();
    const view = this.#deps.voice.plain();
    return share !== null && share.id === o.share.id && view.status === 'running' && view.generation === o.share.generation && view.channel === o.share.channel;
  }

  #enable(o: Owned): Promise<Outcome> {
    const run = o.chain.then(() => this.#enableNow(o));
    o.chain = run.catch(() => undefined);
    return run.catch(() => 'failed' as const);
  }

  #disable(o: Owned): Promise<void> {
    const run = o.chain.then(() => this.#disableNow(o));
    o.chain = run.catch(() => undefined);
    return run.catch(() => undefined);
  }

  async #enableNow(o: Owned): Promise<Outcome> {
    if (this.#gone(o)) return 'ended';
    // An older share's end finishes first.
    await this.#tail;
    if (this.#gone(o)) return 'ended';
    if (o.commanded) return 'ok';
    o.problem = undefined;
    const route = this.#route();
    if (route !== undefined) {
      this.#fail(o, route);
      return 'unavailable';
    }
    const consent = await this.#consent(o);
    if (consent === 'ended' || this.#gone(o)) return 'ended';
    if (consent !== 'ok') {
      this.#fail(o, consent);
      return 'failed';
    }
    if (!this.#sameShare(o)) return 'ended';
    // Turned off while the consent was being asked: no command; the disable
    // queued behind this withdraws the consent.
    if (!o.wanted) return 'ended';
    // The consent is recorded and bound to this share, this turn: the command.
    o.commanded = true;
    const sent = await this.#deps.voice.setShareAudio({ capturing: true, applicationName: this.#deps.applicationName });
    if (this.#gone(o)) return 'ended';
    if (!sent.ok) {
      this.#deps.log(`share sound: the helper refused the command (${sent.error})`);
      await this.#stopAndWithdraw(o);
      this.#fail(o, 'unavailable');
      return 'failed';
    }
    this.#judge(o);
    return 'ok';
  }

  async #disableNow(o: Owned): Promise<void> {
    if (this.#gone(o)) return;
    o.problem = undefined;
    o.retried = false;
    await this.#stopAndWithdraw(o);
    this.#changed();
  }

  #fail(o: Owned, reason: ShareSoundReason): void {
    if (this.#owned !== o) return;
    o.problem = reason;
    this.#deps.log(`share sound: not carried (${reason})`);
    this.#changed();
  }

  // #consent records the consent with conchd: the one body main builds, the
  // answer validated and bound to this share. A refusal of `publisher` (400) is
  // an older server; any other refusal is asked once more after one pass.
  async #consent(o: Owned): Promise<'ok' | 'ended' | ShareSoundReason> {
    for (let attempt = 0; attempt < 2; attempt++) {
      const answer = await this.#put(o);
      if (this.#gone(o)) return 'ended';
      if (answer.kind === 'answer' && answer.ok) {
        const bound = this.#bound(o, answer.body);
        // A 2xx that is not this share's consent may still have recorded one:
        // it is withdrawn now, in order with any other end, not at the share's.
        if (bound !== 'ok' && o.consentSent) {
          o.consentSent = false;
          const done = this.#withdraw(o);
          this.#tail = this.#tail.then(() => done);
          await done;
        }
        return bound;
      }
      if (answer.kind === 'answer' && answer.status === 400) {
        // Nothing was recorded: no withdrawal is owed for it.
        o.consentSent = false;
        this.#serverRefused = true;
        return 'old-server';
      }
      // A session that is over, or an identity that ended, is not asked again.
      if (answer.kind === 'unauthorized' || (answer.kind === 'none' && answer.reason === 'stale')) return 'consent-failed';
      if (attempt === 0 && !(await this.#wait(o, this.#deps.consentRetryMs ?? CONSENT_RETRY_MS))) return 'ended';
    }
    return 'consent-failed';
  }

  // #bound takes a consent's answer if it is the one asked for and this
  // share's: the scope and the publisher as sent, and the share's start time
  // the same as any earlier consent of this record (another is another share).
  #bound(o: Owned, body: unknown): 'ok' | ShareSoundReason {
    const answer = typeof body === 'object' && body !== null ? (body as { scope?: unknown; publisher?: unknown; started_at?: unknown }) : {};
    if (answer.scope !== LINUX_SHARE_AUDIO_SCOPE || answer.publisher !== 'person' || typeof answer.started_at !== 'string') return 'consent-failed';
    if (o.startedAt !== null && o.startedAt !== answer.started_at) return 'consent-failed';
    o.startedAt = answer.started_at;
    return 'ok';
  }

  #put(o: Owned): Promise<Exchange> {
    const body: VoiceCompanionAudioRequestV1 = { scope: LINUX_SHARE_AUDIO_SCOPE, publisher: 'person' };
    o.consentSent = true;
    const sent = (async (): Promise<Exchange> => {
      try {
        return await o.share.client.send({ op: 'voiceShareAudio', params: { channel: o.share.channel }, body, timeoutMs: CONSENT_TIMEOUT_MS });
      } catch {
        return { kind: 'none' };
      }
    })();
    o.inflight = sent;
    void sent.finally(() => {
      if (o.inflight === sent) o.inflight = null;
    });
    return sent;
  }

  // #wait is a pause that the record's end cancels (and, for the repeat's,
  // its stop: `wakers` names the set that cancels it): false when it was.
  #wait(o: Owned, ms: number, wakers: Set<() => void> = o.wakers): Promise<boolean> {
    if (o.cancelled) return Promise.resolve(false);
    return new Promise<boolean>((resolve) => {
      const pause: { timer?: NodeJS.Timeout } = {};
      const wake = (): void => {
        clearTimeout(pause.timer);
        wakers.delete(wake);
        resolve(false);
      };
      pause.timer = setTimeout(() => {
        wakers.delete(wake);
        resolve(true);
      }, ms);
      wakers.add(wake);
    });
  }

  // ------------------------------------------------ the helper's own words

  #onVoice(): void {
    const o = this.#owned;
    if (o !== null && !o.cancelled && o.commanded) this.#judge(o);
    this.#changed();
  }

  // #judge reads the helper's confirmed state of the share's sound and sets
  // the words for a final failure. publish_refused is repeated once first.
  #judge(o: Owned): void {
    const audio = this.#deps.voice.plain().helper?.shareAudio;
    if (audio === undefined || this.#gone(o)) return;
    switch (audio.state) {
      case 'on':
        o.problem = undefined;
        o.retried = false;
        this.#changed();
        return;
      case 'off':
        return;
      case 'unavailable': {
        const problem = audio.problem;
        // A refusal while the helper reconnects is silent until it is back.
        if (problem === undefined || problem === 'not_connected') return;
        if (problem === 'publish_refused') {
          if (o.retrying) return;
          if (!o.retried) {
            void this.#republish(o);
            return;
          }
        }
        const reason = PROBLEM_REASONS[problem];
        if (reason !== undefined && o.problem !== reason) this.#fail(o, reason);
      }
    }
  }

  // #republish repeats the command once after one pass (the publish may have
  // lost a race with the share's grant), then judges again. It writes the
  // capture only while the consent that the first command rested on still
  // stands: the record is the live one, the sound is still wanted, and its
  // command was not stopped (commanded). A stop cancels the wait; a choice of
  // "on" again after it starts over with a new consent, not with this repeat.
  async #republish(o: Owned): Promise<void> {
    o.retried = true;
    o.retrying = true;
    try {
      const waited = await this.#wait(o, this.#deps.publishRetryMs ?? PUBLISH_RETRY_MS, o.repeatWakers);
      if (!waited || this.#gone(o) || !o.commanded || !o.wanted || !this.#sameShare(o)) return;
      await this.#deps.voice.setShareAudio({ capturing: true, applicationName: this.#deps.applicationName });
    } finally {
      o.retrying = false;
    }
    // Read again after the await: a stop may have come in the meantime.
    if (!this.#gone(o) && this.#stillCommanded(o)) this.#judge(o);
  }

  // stillCommanded reads the record's command flag afresh (a stop clears it
  // while another step awaits).
  #stillCommanded(o: Owned): boolean {
    return o.commanded;
  }
}
