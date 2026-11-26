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
//    such a page can claim again. Main closes that gap itself (issue #1876,
//    the presence section below): while a capture or a sound record stands,
//    it reads conchd's voice presence for the share's channel with the
//    companion's own client, every PRESENCE_POLL_MS, and ends the record (the
//    stop, then the DELETE, then the capture) once conchd says the share
//    ended, whatever the page does or opens;
//  - a channel's rename is followed by its id (issue #1880). conchd resolves
//    /v1/channels/{channel} by the channel's current name, and the companion
//    names it by the name it held at the grant, so after a rename the
//    presence read, the consent and its withdrawal answer 404. Main learns
//    the channel's id from a valid presence answer for the share (never the
//    page's word), and on a 404 asks conchd's channel list (the companion's
//    own client) for the current name of that id, retries the request once
//    with it and uses it for every later request of that share's sound. A
//    channel that cannot be named any more (no id learned yet, the id absent
//    from the list, the retry still refused, or an old name that now answers
//    for another channel) ends the sound as every other end does: stop, then
//    the DELETE attempt, then the capture cleared. A list that cannot be read
//    says nothing, and is tried again with the next read;
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
import type { MainLog } from '../diagnostics.ts';
import { VOICE_CONTROL_CAPTURE_STOP_MS, voicePresenceV2, voicePresenceV2Schema, type VoiceCompanionAudioRequestV1, type VoiceControlShareAudioProblem, type VoicePresenceV2 } from '../../../web/src/schema/generated.ts';
import type { NativeVoiceView, ShareSoundChooseResult, ShareSoundChoice, ShareSoundReason, ShareSoundStartResult, ShareSoundView, VoiceCommandResult } from '../../../web/src/platform/desktop/bridge.ts';
import { fromServer } from '../../../web/src/schema/validate.ts';
import type { Exchange, Send } from '../api/client.ts';
import { currentName, isNotFound, isMoved, type CurrentName } from './channel-follow.ts';
import { LINUX_SHARE_AUDIO_SCOPE, validApplicationName } from './protocol.ts';
import type { CommandResult } from './supervisor.ts';
import type { Companion, RemoveReason, ShareRef } from './companion.ts';

// Why a share's sound record ends: the companion's RemoveReason, or main's own.
type EndReason = RemoveReason | 'unused' | 'page' | 'page share end' | 'presence share end' | 'leave';
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
  readonly log: MainLog;
  readonly consentRetryMs?: number;
  readonly publishRetryMs?: number;
  readonly stopAckBoundMs?: number;
  // How often main reads presence while a capture or a record stands.
  readonly presencePollMs?: number;
}

// Main's own read of conchd's presence (issue #1876): how often, and the
// bound on one read. A read that fails or times out says nothing.
const PRESENCE_POLL_MS = 5000;
const PRESENCE_TIMEOUT_MS = 10_000;
// Answers in a row that list this person sharing nothing, before presence
// has shown the share, after which the pick is taken to be no share (issue
// #1876): conchd shows a claim from its answer, and an honest page claims
// before the pick, so one such answer can be a read that raced the claim and
// two cannot.
const UNSHOWN_READS = 2;

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
    readonly pick: number,
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
  // What main's own presence reads have learned of the live companion's
  // share: the start (ms) presence showed this person sharing with, once it
  // showed one. Cleared by a new pick and by the presence end.
  #watch: { readonly companion: number; readonly pick: number; start: number | null; unshown: number } | null = null;
  // Bumped by every pick (captureGranted): a read sent before a pick says
  // nothing of the pick after it.
  #pick = 0;
  // The channel of the live companion as conchd names it now (issue #1880):
  // its id (the grant's when it carried one, ShareRef.channelId, issue #1883;
  // else learned from a presence answer), and the current name once a rename
  // was followed. Kept per companion, as #watch is, and dropped when
  // that companion's end has finished.
  #channel: { readonly companion: number; id: number | null; name: string | null } | null = null;
  // The next presence read, and whether one is in flight: one at a time, so
  // the answers are judged in the order conchd gave them.
  #pollTimer: NodeJS.Timeout | null = null;
  #polling = false;
  #closed = false;
  readonly #off: () => void;

  constructor(deps: ShareSoundDeps) {
    this.#deps = deps;
    this.#lastView = JSON.stringify([this.view(), this.allowsStreams()]);
    this.#off = deps.voice.subscribe(() => {
      this.#onVoice();
    });
  }

  // close detaches from the voice service.
  close(): void {
    this.#closed = true;
    if (this.#pollTimer !== null) clearTimeout(this.#pollTimer);
    this.#pollTimer = null;
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
    // A new pick is learned afresh by presence.
    this.#pick++;
    this.#watch = null;
    this.#changed();
    this.#schedulePresence();
  }

  // captured says whether main granted a capture while this companion was
  // held, in its document: whether `share` is a share and not only a watch.
  #captured(share: ShareRef): boolean {
    const c = this.#capture;
    return c !== null && c.companion === share.id && c.document === share.document;
  }

  // ---------------------------------------------------------------- view

  // The saved choice and any fixed reason.
  // Only the helper's confirmed state (helper.shareAudio) says "with sound".
  // A record whose capture is no longer the live one (the share ended and the
  // page's end did not come) reports no failure: it is no share's any more.
  view(): ShareSoundView {
    const choice = this.#deps.choice() ?? 'system_excluding_call';
    const owned = this.#owned;
    const reason = owned === null || !this.#captured(owned.share) ? this.#route() : owned.problem;
    return { choice, ...(reason === undefined ? {} : { unavailable: reason }) };
  }

  // Only show diagnostics for the current pick that this ShareSound commanded.
  allowsStreams(): boolean {
    const choice = this.#deps.choice() ?? LINUX_SHARE_AUDIO_SCOPE;
    const owned = this.#owned;
    return choice === LINUX_SHARE_AUDIO_SCOPE && owned?.pick === this.#pick
      && owned.commanded && owned.wanted && this.#captured(owned.share) && this.#sameShare(owned);
  }

  #changed(): void {
    const now = JSON.stringify([this.view(), this.allowsStreams()]);
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
    if (o !== null && o.pick === this.#pick && o.share.id === share.id && o.starting !== null) return this.#report(o, await o.starting);
    // Any record of this companion's still here is of an earlier share of it
    // (the page's end did not arrive): it ends first, in order. The capture
    // just checked is this share's, so that end keeps it.
    if (o !== null) void this.#end(o, 'unused', true);
    o = new Owned(share, ++this.#serial, this.#pick);
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
    if (o === null || o.pick !== this.#pick || o.share.id !== share.id) {
      if (o !== null) void this.#end(o, 'unused', true);
      o = new Owned(share, ++this.#serial, this.#pick);
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

  // ------------------------------------------------------------ presence

  // Main's own read of conchd's presence (issue #1876). While a capture or a
  // sound record of the live companion stands, main asks conchd for the
  // share's channel's presence (v2) with the companion's own client: not the
  // page's feed, so a page that opens no feed, or another channel's, changes
  // nothing. The answers are read one at a time, in order.

  // #presenceWanted: a capture or a record of the live companion stands.
  #presenceWanted(): ShareRef | null {
    const share = this.#deps.companion.current();
    if (share === null) return null;
    return this.#captured(share) || this.#owned?.share.id === share.id ? share : null;
  }

  #schedulePresence(): void {
    if (this.#closed || this.#pollTimer !== null || this.#polling || this.#presenceWanted() === null) return;
    const timer = setTimeout(() => {
      this.#pollTimer = null;
      void this.#pollPresence();
    }, this.#deps.presencePollMs ?? PRESENCE_POLL_MS);
    timer.unref();
    this.#pollTimer = timer;
  }

  async #pollPresence(): Promise<void> {
    const share = this.#presenceWanted();
    if (share === null || this.#closed) return;
    const pick = this.#pick;
    this.#polling = true;
    try {
      let answer: Exchange;
      let lost = false;
      try {
        const read = await this.#sendFollowing(share, { op: 'voicePresence', query: `schema=${voicePresenceV2Schema}`, timeoutMs: PRESENCE_TIMEOUT_MS }, (e) => this.#presenceMoved(share, e));
        answer = read.exchange;
        lost = read.lost;
      } catch {
        answer = { kind: 'none' };
      }
      // Closed, or a new pick since the read was sent: the answer is of
      // another moment and says nothing.
      if (!this.#samePick(pick)) return;
      if (lost) this.#channelLost(share);
      else if (answer.kind === 'answer' && answer.ok && fromServer(voicePresenceV2)(answer.body).length === 0) this.#judgePresence(share, answer.body as VoicePresenceV2);
    } finally {
      this.#polling = false;
      this.#schedulePresence();
    }
  }

  // ------------------------------------------------- following a rename

  // #channelOf: the name to put in a request of this share's sound: the
  // current one once a rename was followed, else the one the grant named.
  #channelOf(share: ShareRef): string {
    const c = this.#channel;
    return c !== null && c.companion === share.id && c.name !== null ? c.name : share.channel;
  }

  // channelId is the id of the share's channel: the grant's, else the one this
  // module learned from an answer that showed the share (#learnId), or null. Read-only, for the
  // companion's own removal (issue #1881): the companion takes the id before
  // it tells this module the share ended (ended()), so it never depends on
  // the order of the two ends.
  channelId(share: ShareRef): number | null {
    return this.#idOf(share);
  }

  #idOf(share: ShareRef): number | null {
    if (share.channelId !== null) return share.channelId;
    const c = this.#channel;
    return c !== null && c.companion === share.id ? c.id : null;
  }

  // #learnId keeps the channel's id from a presence answer that showed this
  // share (this person sharing, with the consent's start once there is one):
  // the first one only. An answer read by name that does not show the share
  // may be another channel's, one that took the name, so it teaches nothing. A later answer for another id never replaces it
  // (#presenceMoved handles that before it gets here).
  #learnId(share: ShareRef, id: number): void {
    // The grant named the id: presence teaches nothing (the fallback only).
    if (share.channelId !== null || !Number.isSafeInteger(id) || id <= 0) return;
    if (this.#channel?.companion !== share.id) this.#channel = { companion: share.id, id: null, name: null };
    if (this.#channel.id === null) this.#channel.id = id;
  }

  // #dropChannel forgets what was learned of this companion's channel.
  #dropChannel(companionId: number): void {
    if (this.#channel?.companion === companionId) this.#channel = null;
  }

  // #presenceMoved: the read answered 404, or answered for another channel
  // than the one learned (the old name now belongs to a new channel).
  #presenceMoved(share: ShareRef, e: Exchange): boolean {
    if (isNotFound(e)) return true;
    const id = this.#idOf(share);
    if (id === null || e.kind !== 'answer' || !e.ok || fromServer(voicePresenceV2)(e.body).length !== 0) return false;
    return (e.body as VoicePresenceV2).channel_id !== id;
  }

  // #sendFollowing sends one request of the share's sound by the channel's
  // current name. When `moved` says the answer shows the channel is not at
  // that name, it looks the name up by id and sends the request once more
  // with it. `lost`: the channel cannot be named any more (no id learned, the
  // id not in the list, a name main's client cannot send, or the retry moved
  // again). A list that cannot be read is not lost, and the next request asks
  // again; an answer that was another channel's is then not handed on to be
  // judged. The request's timeout bounds all of it, the list read and the
  // retry included, so an end's withdrawal keeps its bound.
  async #sendFollowing(share: ShareRef, request: Omit<Send, 'params'> & { readonly timeoutMs: number }, moved: (e: Exchange) => boolean = isMoved): Promise<{ exchange: Exchange; lost: boolean }> {
    const deadline = Date.now() + request.timeoutMs;
    const left = (): number => deadline - Date.now();
    const sendBy = async (name: string): Promise<Exchange> => {
      const ms = left();
      if (ms <= 0) return { kind: 'none' };
      try {
        const id = this.#idOf(share);
        const query = id !== null && (request.op === 'voiceShareAudio' || request.op === 'voiceShareAudioEnd') ? `channel_id=${String(id)}` : request.query;
        return await share.client.send({ ...request, query, params: { channel: name }, timeoutMs: ms });
      } catch {
        return { kind: 'none' };
      }
    };
    const first = await sendBy(this.#channelOf(share));
    if (!moved(first)) return { exchange: first, lost: false };
    const name = await this.#currentName(share, left());
    if (name === 'unknown') return { exchange: isNotFound(first) ? first : { kind: 'none' }, lost: false };
    if (name === 'gone') return { exchange: first, lost: true };
    const again = await sendBy(name.name);
    if (again.kind === 'none' && left() <= 0) return { exchange: again, lost: false };
    return { exchange: again, lost: moved(again) };
  }

  // #currentName reads conchd's channel list for the id learned, with the
  // companion's own client (channel-follow.ts), and keeps and returns the name
  // it has now. A name main's client would refuse to put in a path is 'gone':
  // the sound ends rather than every later request being refused before the
  // wire.
  async #currentName(share: ShareRef, timeoutMs: number): Promise<CurrentName> {
    const id = this.#idOf(share);
    if (id === null) return 'gone';
    const found = await currentName(share.client, id, timeoutMs);
    if (found === 'gone' || found === 'unknown') return found;
    // With the grant's id there is no record from presence yet: make it, to
    // keep the name followed.
    // Never another companion's record: a late follow by an ended share must
    // not wipe a newer share's followed name.
    if (this.#channel === null) this.#channel = { companion: share.id, id: null, name: null };
    if (this.#channel.companion === share.id) this.#channel.name = found.name;
    return found;
  }

  // #channelLost: main can no longer name the share's channel, so it cannot
  // read its presence nor be sure to withdraw: the sound ends as every end
  // does (the stop, the DELETE attempt, the capture cleared).
  #channelLost(share: ShareRef): void {
    if (this.#presenceWanted()?.id !== share.id) return;
    this.#deps.log('share sound: the share’s channel can no longer be named', { ring: true });
    this.#presenceEnd(share, this.#owned?.share.id === share.id ? this.#owned : null);
  }

  // #samePick: not closed, and no new pick since `pick` was noted.
  #samePick(pick: number): boolean {
    return !this.#closed && pick === this.#pick;
  }

  // #judgePresence reads one presence answer for `share`. Only this person's
  // own participant counts (the principal the companion's grant named,
  // p<id>.screen); another participant's share never touches the sound.
  //
  // The share is named by its start: the consent's started_at once there is
  // one, else the start presence first showed after the pick. The share has
  // ended when conchd says so:
  //  - share_ended (conchd's own record of the sharer's last share, in their
  //    document alone) names that start or a newer one; no earlier sight is
  //    needed;
  //  - presence lists this person sharing with a newer start (a new claim
  //    replaced it), or a start other than the consent's after it learned one;
  //  - presence showed the share and now lists this person, not sharing;
  //  - presence has not shown it, and UNSHOWN_READS answers in a row list this
  //    person sharing nothing: a pick no claim stood behind (or one released
  //    before main's first read) keeps no capture for a later share.
  // An answer that is not `available` (LiveKit unreachable) says nothing, and
  // once presence has shown the share, neither does one that does not list
  // this person (a room hidden after an outage), so a failed LiveKit call
  // never cuts a live share's sound. A share listed with no start ties to
  // nothing and ends nothing.
  //
  // A rename of the channel is followed before an answer gets here
  // (#sendFollowing, by the id learned from `channel_id` of an answer that
  // showed the share): once the id is known an answer that names another
  // channel is never judged, and one that cannot be followed ends the sound
  // (#channelLost). Before the id is known, the two-answer rule above ends a
  // pick whose answers never show the share, whichever channel answered.
  #judgePresence(share: ShareRef, doc: VoicePresenceV2): void {
    if (this.#presenceWanted()?.id !== share.id) return;
    const o = this.#owned?.share.id === share.id ? this.#owned : null;
    if (this.#watch?.companion !== share.id || this.#watch.pick !== this.#pick) this.#watch = { companion: share.id, pick: this.#pick, start: null, unshown: 0 };
    const watch = this.#watch;
    const ours = o?.startedAt == null ? Number.NaN : Date.parse(o.startedAt);
    const bound = Number.isNaN(ours) ? watch.start : ours;
    const ended = doc.share_ended === undefined ? Number.NaN : Date.parse(doc.share_ended.started_at);
    if (!Number.isNaN(ended) && bound !== null && ended >= bound) {
      this.#presenceEnd(share, o);
      return;
    }
    if (!doc.available) return;
    let listed = false;
    let unbound = false;
    const starts: number[] = [];
    for (const room of doc.rooms) {
      for (const p of room.participants) {
        if (p.principal_id !== share.principal) continue;
        listed = true;
        if (p.sharing !== true) continue;
        const at = p.share_started_at === undefined ? Number.NaN : Date.parse(p.share_started_at);
        if (Number.isNaN(at)) unbound = true;
        else starts.push(at);
      }
    }
    const shown = watch.start !== null && watch.start === bound;
    // Not listed: no word, once presence has shown the share (a room hidden
    // after an outage). Before that it counts as sharing nothing: an answer
    // read by name that never shows the person may be another channel's.
    if (!listed) {
      if (!shown && ++watch.unshown >= UNSHOWN_READS) this.#presenceEnd(share, o);
      return;
    }
    if (watch.start !== null && !Number.isNaN(ours) && ours !== watch.start) {
      this.#presenceEnd(share, o);
      return;
    }
    if (bound !== null && starts.some((at) => at > bound)) {
      this.#presenceEnd(share, o);
      return;
    }
    if (bound === null ? starts.length > 0 : starts.includes(bound)) {
      watch.start = bound ?? Math.max(...starts);
      watch.unshown = 0;
      this.#learnId(share, doc.channel_id);
      return;
    }
    if (starts.length > 0 || unbound) return;
    // Listed, sharing nothing.
    if (shown || ++watch.unshown >= UNSHOWN_READS) this.#presenceEnd(share, o);
  }

  // #presenceEnd: the record ends as every other end does (the stop, then the
  // DELETE, then the capture); with no record, the capture alone goes, so no
  // later choice can add sound to a share that has ended.
  #presenceEnd(share: ShareRef, o: Owned | null): void {
    this.#watch = null;
    this.#deps.log('share sound: presence shows the share ended', { ring: true });
    if (o !== null) {
      void this.#end(o, 'presence share end').catch(() => undefined);
      return;
    }
    if (this.#capture?.companion === share.id) {
      this.#capture = null;
      this.#changed();
    }
  }

  // ------------------------------------------------------------ the ends

  // ended is the companion's word that its share ended, for any reason. It
  // names the companion held, so a record of another (newer) one is untouched.
  ended(share: ShareRef, reason: RemoveReason): Promise<void> {
    if (reason === 'switch' || reason === 'sign-out' || reason === 'quit') this.#serverRefused = false;
    if (this.#capture?.companion === share.id) this.#capture = null;
    const o = this.#owned;
    const done = o === null || o.share.id !== share.id ? Promise.resolve() : this.#end(o, reason);
    // The withdrawals still in flight (this end's, and an earlier end's of the
    // same companion, such as a leave's) need the channel's current name:
    // it is forgotten only after them.
    void this.#tail
      .catch(() => undefined)
      .then(() => {
        this.#dropChannel(share.id);
      });
    return done;
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
  // reason is a closed vocabulary: it reaches the diagnostics export (#2088).
  #end(o: Owned, reason: EndReason, keepCapture = false): Promise<void> {
    if (this.#owned === o) this.#owned = null;
    if (!keepCapture && this.#capture?.companion === o.share.id) this.#capture = null;
    o.cancelled = true;
    for (const wake of [...o.wakers, ...o.repeatWakers]) wake();
    this.#changed();
    this.#deps.log(`share sound: ending (${reason})`, { ring: true });
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
    if (bounded.over) this.#deps.log('share sound: the helper did not answer the stop in time', { ring: true });
  }

  // #withdraw is the DELETE of the consent: once, never retried, bounded. A
  // failure is logged by its kind alone.
  // With the channel's id held, a 2xx by the name the DELETE went to is
  // checked after the fact, as the companion's removal is (#1884): conchd
  // answers 204 to this DELETE on any channel of the caller's, with no body
  // naming it, so a channel that took the name would confirm a withdrawal that
  // never reached the share's. A name that has changed gets the DELETE by the
  // name now; one that cannot be checked leaves it unconfirmed (logged). It is
  // after the fact for older servers only: a grant carrying channel_id proves
  // the server checks the precondition (#1891), so no list read is needed.
  // One bound covers all of it.
  async #withdraw(o: Owned): Promise<void> {
    const deadline = Date.now() + WITHDRAW_TIMEOUT_MS;
    const sentBy = this.#channelOf(o.share);
    let { exchange } = await this.#sendFollowing(o.share, { op: 'voiceShareAudioEnd', timeoutMs: WITHDRAW_TIMEOUT_MS });
    const id = this.#idOf(o.share);
    // Followed already (the name changed during the send): conchd's own list
    // gave that name for the id just now.
    if (id !== null && o.share.channelId === null && exchange.kind === 'answer' && exchange.ok && this.#channelOf(o.share) === sentBy) {
      const now = await currentName(o.share.client, id, deadline - Date.now());
      if (now === 'gone' || now === 'unknown') {
        exchange = { kind: 'none' };
      } else if (now.name !== sentBy) {
        const ms = deadline - Date.now();
        try {
          exchange = ms <= 0 ? { kind: 'none' } : await o.share.client.send({ op: 'voiceShareAudioEnd', params: { channel: now.name }, query: `channel_id=${String(id)}`, timeoutMs: ms });
        } catch {
          exchange = { kind: 'none' };
        }
      }
    }
    if (!(exchange.kind === 'answer' && exchange.ok)) this.#deps.log('share sound: the withdrawal was not confirmed', { ring: true });
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
      this.#deps.log(`share sound: the helper refused the command (${sent.error})`, { ring: true });
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
    this.#deps.log(`share sound: not carried (${reason})`, { ring: true });
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
    const answer = typeof body === 'object' && body !== null ? (body as { scope?: unknown; publisher?: unknown; started_at?: unknown; channel_id?: unknown }) : {};
    // A consent that names another channel than the share's is another
    // channel's (a name retaken): never this share's.
    const id = this.#idOf(o.share);
    // A grant that named its channel comes from a server that names it in the
    // consent answer too (#1883): one that is missing or names another
    // channel is refused, and withdrawn as every refused consent is.
    if (o.share.channelId !== null && answer.channel_id !== o.share.channelId) return 'consent-failed';
    if (id !== null && answer.channel_id !== undefined && answer.channel_id !== id) return 'consent-failed';
    if (answer.scope !== LINUX_SHARE_AUDIO_SCOPE || answer.publisher !== 'person' || typeof answer.started_at !== 'string') return 'consent-failed';
    if (o.startedAt !== null && o.startedAt !== answer.started_at) return 'consent-failed';
    o.startedAt = answer.started_at;
    return 'ok';
  }

  #put(o: Owned): Promise<Exchange> {
    const body: VoiceCompanionAudioRequestV1 = { scope: LINUX_SHARE_AUDIO_SCOPE, publisher: 'person' };
    o.consentSent = true;
    const sent = (async (): Promise<Exchange> => {
      return (await this.#sendFollowing(o.share, { op: 'voiceShareAudio', body, timeoutMs: CONSENT_TIMEOUT_MS })).exchange;
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
