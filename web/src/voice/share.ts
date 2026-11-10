// Screen share (issue #444): the sharer's side, as a state machine the voice
// client drives, and what the page can say about sound before the picker opens.
//
// The order is conchd's (voice-control-plane.md section 12): connect, claim,
// then publish. The join token allows the microphone alone, so a share
// published before the claim is refused by LiveKit. The claim is made before
// the picker opens, so that someone else's share is told at once, and again
// once something was picked: the picker may stay open longer than conchd waits
// for a share's first track, and asking again keeps a share still held and
// claims one that lapsed.
//
// Stopping by the sharer, by the browser's own stop control and by leaving
// voice asks conchd to end the share, but only while the share is known to be
// this person's: claimed, and not shown by presence as someone else's or
// nobody's since. conchd's stop ends whoever holds the share when the caller
// is an operator, so a stop sent after the claim lapsed (a picker left open)
// would end another person's share as a moderation stop. Every release also
// names the claim it means (issue #487), so conchd itself refuses one that
// arrives after that claim ended. A share the server ended (an operator, the
// sharer's access, the room) is not asked about again: the page only stops
// publishing and says why, from the reason the sharer's own presence carries
// (share_ended), matched to the claim by its started_at.
//
// One end is not final (issue #516): a share the server ended because the
// room was renewed (rotated), which it does whenever anyone in the room loses
// access. The browser still holds the capture, so the share is resumed: the
// capture is kept off the old connection, and once this page is connected to
// the new room the share is claimed again and the same capture published, with
// no picker. The claim is conchd's check, as for any share: a sharer who was
// the one removed gets no session, or has the claim refused, and the share
// ends. A capture the browser has ended, a share stopped meanwhile, and any
// other reason are not resumed.
//
// A room renewed while the share is being published (issue #535), whether a
// first share or one being resumed, is a renewal like any other: the capture
// is taken off that connection at once (blanked, and a publish under way
// taken down when it completes; sdk.ts), and is published on the next. One wait (resumeLimit) covers the whole of it,
// however many times the room is renewed before the share is back.
import type { ShareClaimResult } from '../api';
import type { VoiceShareEnded, VoiceShareEndReason } from '../schema/voice';
import type { Clock } from '../state/feed';

// The sharer's choice of capture and top layer. It is a local preference,
// never part of presence or another wire document.
export type ShareResolution = '720p' | '1080p' | 'source';
export type ShareFrameRate = 15 | 30 | 60;
export interface ShareQuality {
  readonly resolution: ShareResolution;
  readonly frameRate: ShareFrameRate;
}
export const defaultShareQuality: ShareQuality = { resolution: '1080p', frameRate: 30 };

// One stats sample of the received share, or of its sent top layer.
export interface ShareSample {
  readonly width: number;
  readonly height: number;
  readonly fps: number;
  readonly bytes?: number;
  readonly timestamp?: number;
  // Which RTP stream of which publication it is, so that bitrate is never
  // taken between two different ones.
  readonly stream?: string;
  readonly publication?: string;
  // Why the encoder holds the picture back (outbound-rtp
  // qualityLimitationReason), where the engine says. Absent where it does not.
  readonly limitation?: ShareLimitation;
  // Share of packets lost, 0 to 1: the receiver's report on the sent top layer
  // (remote-inbound-rtp), or the lost share of a received stream. Absent where
  // the engine does not say.
  readonly lost?: number;
  // The capture's own size and rate (getSettings) of the sender's screen, when
  // the browser reports them. A sent sample only.
  readonly captured?: { readonly width?: number; readonly height?: number; readonly fps?: number };
}
export type ShareLimitation = 'none' | 'cpu' | 'bandwidth' | 'other';
export type ShareDirection = 'received' | 'sent';

// What the browser's picker gave: the picture always, sound when the browser
// offered it and the person ticked it.
export interface Captured {
  readonly audio: boolean;
}

// How the published share ended, as the room saw it: the browser's own stop
// control ended the capture, or the server withdrew it.
export type ScreenEnd = 'browser' | 'server';

// Who presence shows holding the room's share: this person, someone else, or
// nobody.
export type Holder = 'me' | 'other' | 'none';

// A capture taken off a connection that ended, still capturing, for the next
// connection to publish (issue #516). sdk.ts implements it.
export interface KeptScreen {
  readonly audio: boolean;
  // Whether the browser still captures: none of its tracks has ended.
  readonly live: () => boolean;
  // Called once if the browser ends the capture while it is kept.
  onEnded: (() => void) | null;
  // Stops the capture.
  readonly stop: () => void;
}

// What a share needs of the room connection (sdk.ts implements it).
export interface ShareRoom {
  // Opens the browser's picker at the chosen caps and keeps what was chosen,
  // unpublished. Rejects when nothing was chosen.
  readonly pickScreen: (quality?: ShareQuality) => Promise<Captured>;
  // Changes the capture and its existing sender encodings without republishing.
  readonly setShareQuality: (quality: ShareQuality) => Promise<void>;
  // The latest stats of the received share or the sent top layer.
  readonly readShareStats: (direction: ShareDirection, principal?: number) => Promise<ShareSample | null>;
  // Publishes what was picked, with its simulcast layers.
  readonly publishScreen: () => Promise<void>;
  // Unpublishes and stops whatever was picked or published. Never rejects.
  readonly stopScreen: () => Promise<void>;
  // Takes whatever was picked or published off this connection without
  // stopping it, for the next one; null when there was nothing.
  readonly keepScreen: () => KeptScreen | null;
  // Gives this connection a capture kept from an earlier one, as if picked
  // here: publishScreen publishes it.
  readonly adoptScreen: (kept: KeptScreen) => void;
}

// Why the last share ended, or why one did not start. 'server' is the server
// ending it for a reason the page was not told; 'operator', 'access_lost',
// 'rotated' and 'idle' are the reasons it tells the sharer.
export type ShareEnd =
  | 'self'
  | 'browser'
  | 'left'
  | 'lost'
  | 'server'
  | 'operator'
  | 'access_lost'
  | 'rotated'
  | 'idle'
  | 'full'
  | 'cancelled'
  | 'not_connected'
  | 'unavailable'
  | 'failed';

// What more is known of a share that did not start, for a client that says it
// in its own words (the desktop's, issue #1582). It never changes `ended`, so
// the browser's words are the same with or without it.
//   denied: the pick failed in a way the person should hear of. In the
//     browser that is NotAllowedError, the picker closed or refused, which it
//     says (as it always has); on the desktop companion it is any failure but
//     the person cancelling the picker (AbortError, silent): main refusing
//     (no allowance, no grant), the PipeWire stream failing, a capture that
//     could not be constrained. A
//     desktop with no screen-cast portal is said before the pick, from the
//     grant (VoiceState.companionShareUnavailable), never from a failed pick.
export type ShareCause = 'denied';

export type ShareState =
  | { readonly kind: 'idle'; readonly ended: ShareEnd | null; readonly cause?: ShareCause }
  | { readonly kind: 'claiming' }
  | { readonly kind: 'picking' }
  | { readonly kind: 'publishing' }
  // resumed: the share carried on into a renewed room (issue #516).
  | { readonly kind: 'sharing'; readonly audio: boolean; readonly resumed?: true }
  // The room was renewed: the capture is kept until this page is in the new
  // room and the share is claimed again.
  | { readonly kind: 'resuming' }
  | { readonly kind: 'stopping' };

export const idleShare: ShareState = { kind: 'idle', ended: null };

// Whether this tab is sharing its screen (issue #714), for the pages that
// show a secret once (Add a device's link, an authenticator's setup key) and
// refuse to while it is: a share of this tab's in any state but idle, from
// the claim before the picker opens to the end of its stop, since the
// picker may be choosing a screen that shows this page, and a capture lives
// until it is stopped. A share that never went back to idle (its connection
// let go mid-share) still counts, which errs towards refusing.
//
// What it cannot know: another tab, another browser or another application
// capturing or sharing the screen, a screen recorder, a remote-desktop
// session or someone looking over a shoulder. The browser tells a page none
// of these. The pages that ask say so beside the secret.
const sharing = new Set<ScreenShare>();
const sharingWatchers = new Set<() => void>();

function noteSharing(share: ScreenShare, state: ShareState): void {
  const was = sharing.size > 0;
  if (state.kind === 'idle') sharing.delete(share);
  else sharing.add(share);
  if (was !== sharing.size > 0) for (const watcher of sharingWatchers) watcher();
}

// tabSharing says whether a share of this tab is under way.
export function tabSharing(): boolean {
  return sharing.size > 0;
}

// onTabSharing calls back each time tabSharing changes, and returns the
// function that stops it.
export function onTabSharing(callback: () => void): () => void {
  sharingWatchers.add(callback);
  return () => {
    sharingWatchers.delete(callback);
  };
}

// How long a share waits to be resumed after its room was renewed: longer
// than conchd's sweep takes to delete an old room whose deletion failed (30
// s), and a reconnect after it. Past it the capture is stopped.
export const resumeLimit = 45_000;

// What a quality change while live came to: sent (or nothing to send), or
// refused for now by conchd's bound on quality changes (#1546) and to be sent
// again by itself in a moment (changeQuality).
export type QualityAnswer = 'sent' | 'later';

// The longest a quality change refused for the rate bound waits before it is
// sent again, in seconds, whatever Retry-After asks for.
export const qualityRetryLimit = 10;

// What the sharer is told when conchd refused a quality change for its rate
// bound: the picture already has the new quality, and the others' label
// catches up.
export const qualityLaterNote = 'Quality changes are limited to a few at a time: the others see the new quality on your stream in a moment.';

export interface ShareApi {
  readonly claim: (channel: string, quality?: ShareQuality) => Promise<ShareClaimResult>;
  // Releases the claim whose started_at is startedAt, and no other.
  readonly release: (channel: string, startedAt: string) => Promise<unknown>;
}

type Refusal = Extract<ShareClaimResult, { ok: false }>;

// refusalEnd names a refused claim.
export function refusalEnd(r: Refusal): ShareEnd {
  if (r.code === 'voice_share_full') return 'full';
  if (r.code === 'voice_no_session') return 'not_connected';
  if (r.code === 'voice_unavailable' || r.failure === 'network') return 'unavailable';
  return 'failed';
}

// pickCause is the more that a failed pick says, from the shapes getDisplayMedia
// gives. In the browser, NotAllowedError is the picker closed or refused (or
// main refusing, on the desktop), which is said; AbortError and anything else
// say nothing more. On the desktop companion (companion), only AbortError, the
// picker cancelled, is silent: NotAllowedError (main refusing: no allowance,
// no grant), NotReadableError (the PipeWire stream failed), LiveKit's
// DeviceUnsupportedError, a failure to constrain the capture, or a rejection
// that is no Error at all, are all a screen that could not be captured.
export function pickCause(err: unknown, companion = false): ShareCause | undefined {
  if (companion) return err instanceof Error && err.name === 'AbortError' ? undefined : 'denied';
  if (!(err instanceof Error)) return undefined;
  return err.name === 'NotAllowedError' ? 'denied' : undefined;
}

// Every reason the server gives the sharer is an end the page can say: its
// own stop and leaving as the page says them, the rest in their own words. A
// reason added to the schema without a word for it fails to compile here.
const toldEnd: (reason: VoiceShareEndReason) => ShareEnd = (reason) => reason;

// sameInstant reports whether two wire timestamps are one moment, whatever
// their spelling.
function sameInstant(a: string, b: string): boolean {
  const x = Date.parse(a);
  return !Number.isNaN(x) && x === Date.parse(b);
}

function causeOf(cause: ShareCause | undefined): { readonly cause?: ShareCause } {
  return cause === undefined ? {} : { cause };
}

export class ScreenShare {
  private readonly api: ShareApi;
  private readonly onChange: (state: ShareState) => void;
  private readonly clock: Clock;
  private state: ShareState = idleShare;
  // Each start or stop has a number; a step that resumes after a newer one
  // began does nothing more.
  private attempt = 0;
  private channel: string | null = null;
  private room: ShareRoom | null = null;
  private quality: ShareQuality = defaultShareQuality;
  // Whether presence has shown this person sharing since the last claim: a
  // presence without them after that is the server ending the share.
  private seen = false;
  // Whether the room's share is known to be this person's: set by a granted
  // claim, cleared by presence showing someone else or nobody, and by every
  // stop. Only a held share is ever asked to stop.
  private held = false;
  // The started_at of the last claim granted: what a release names, and how
  // the reason in presence is known to be about this share.
  private claimed: string | null = null;
  // Why the server ended the share of that claim, once presence said.
  private told: ShareEnd | null = null;
  // While resuming: the capture taken off the old connection, the timer that
  // gives up, and whether the claim is on its way. room is null from the
  // moment the old connection is let go until the new one is up.
  private kept: KeptScreen | null = null;
  private resumeTimer: unknown = null;
  private reclaiming = false;
  // Whether presence said the share being resumed ended because its room was
  // renewed. Kept apart from told, which a claim granted again clears: a
  // resume cut short by another lost connection is tried again.
  private renewal = false;
  // A quality change refused for the rate bound, waiting to be sent again
  // (changeQuality); the one quality change on its way, and whether a newer
  // choice waits for it to be answered.
  private qualityRetry: unknown = null;
  private qualitySending: Promise<QualityAnswer> | null = null;
  private qualityDue = false;
  // Whether the share is between rooms: from the renewal that took it off the
  // air until it is shared again or ends. The resume timer runs for the whole
  // of it, so a room renewed again meanwhile does not start the wait over.
  private interrupted = false;
  // Whether this is the desktop's screen companion (issue #1581). Its share is
  // never resumed after a renewal without a new consent (Q-D4-2 on #1578): it
  // ends, with the renewal's words, whichever of presence or the room said so
  // first.
  private companion = false;

  constructor(api: ShareApi, onChange: (state: ShareState) => void, clock: Clock) {
    this.api = api;
    this.onChange = onChange;
    this.clock = clock;
  }

  get current(): ShareState {
    return this.state;
  }

  private set(state: ShareState): void {
    this.state = state;
    // A quality change waits to be sent again only while the share is live.
    if (state.kind !== 'sharing') this.endQualityRetry();
    noteSharing(this, state);
    this.onChange(state);
  }

  // start claims the room's share, opens the picker, claims again and
  // publishes. It does nothing unless the last share is over.
  async start(channel: string, room: ShareRoom, quality: ShareQuality = defaultShareQuality): Promise<void> {
    if (this.state.kind !== 'idle') return;
    const mine = ++this.attempt;
    this.channel = channel;
    this.room = room;
    this.quality = quality;
    this.seen = false;
    this.renewal = false;
    this.set({ kind: 'claiming' });
    // The channel as it is called now: renamed (issue #728) while the picker
    // was open, the next call names the new name.
    const at = () => this.channel ?? channel;
    const first = await this.api.claim(channel, this.quality);
    if (first.ok) this.granted(first.value.started_at);
    if (mine !== this.attempt) {
      // Stopped while the claim was on its way: a claim granted since is ours
      // and is let go, unless a newer share has begun.
      if (this.isIdle()) await this.releaseHeld(at());
      return;
    }
    if (!first.ok) {
      this.set({ kind: 'idle', ended: refusalEnd(first) });
      return;
    }
    this.set({ kind: 'picking' });
    let captured: Captured;
    try {
      captured = await room.pickScreen(quality);
    } catch (err: unknown) {
      if (mine !== this.attempt) return;
      // Closed without choosing, or refused by the browser: the claim goes.
      this.set({ kind: 'stopping' });
      await this.releaseHeld(at());
      if (mine === this.attempt) this.set({ kind: 'idle', ended: 'cancelled', ...causeOf(pickCause(err, this.companion)) });
      return;
    }
    if (mine !== this.attempt) {
      await room.stopScreen();
      return;
    }
    this.set({ kind: 'publishing' });
    this.seen = false;
    // The choice as it is now: one changed while the picker was open is the
    // one conchd is told.
    const again = await this.api.claim(at(), this.quality);
    if (again.ok) this.granted(again.value.started_at);
    if (mine !== this.attempt) {
      await room.stopScreen();
      if (this.isIdle()) await this.releaseHeld(at());
      return;
    }
    if (!again.ok) {
      const end = refusalEnd(again);
      await room.stopScreen();
      await this.releaseHeld(at());
      if (mine === this.attempt) this.set({ kind: 'idle', ended: end });
      return;
    }
    try {
      await room.publishScreen();
    } catch {
      // Taken over meanwhile (a stop, or the room renewed, which kept the
      // capture for the next connection): not this step's to end.
      if (mine !== this.attempt) return;
      if (this.renewed()) {
        this.suspend();
        return;
      }
      await room.stopScreen();
      await this.releaseHeld(at());
      if (mine === this.attempt) this.set({ kind: 'idle', ended: 'failed' });
      return;
    }
    if (mine !== this.attempt) return;
    this.set({ kind: 'sharing', audio: captured.audio });
  }

  // changeQuality keeps the declared choice for later claims and, while live,
  // tells conchd so presence can show it: a holder's repeat claim with the
  // new body (voice-multi-share.md §3.1, #1509). Its answer is a claim's:
  //  - granted: the share of that started_at, which a conchd restarted
  //    meanwhile may have issued anew;
  //  - full or no session: the server no longer holds this share, which ends
  //    as one the server ended (or waits, if presence said the room was
  //    renewed);
  //  - 429 rate_limited (#1546): conchd kept the quality it had. The page
  //    keeps the person's choice (the encodings already carry it) and sends it
  //    once more after Retry-After, the newest choice by then; 'later' says
  //    so, for the sharer to be told.
  // A change sent while a stop is under way, or after it, does not hold the
  // share: a grant it is answered with then is released by its own
  // started_at, so no claim outlives the share and takes a place under the
  // cap (voice-multi-share.md §4).
  //
  // One change is on its way to conchd at a time. Choices made while one is
  // are coalesced: when it is answered the newest is sent, once, so the last
  // quality conchd applies is always the last one chosen, whatever order the
  // network would have delivered several in. Every caller is answered with
  // what the last send came to.
  changeQuality(quality: ShareQuality): Promise<QualityAnswer> {
    this.quality = quality;
    this.endQualityRetry();
    return this.queueQuality(true);
  }

  // queueQuality marks the current choice as to be sent and starts the one
  // sender, unless it is running: it picks the choice up when its send is
  // answered.
  private queueQuality(retry: boolean): Promise<QualityAnswer> {
    this.qualityDue = true;
    if (this.qualitySending === null) {
      this.qualitySending = this.drainQuality(retry).finally(() => {
        this.qualitySending = null;
      });
    }
    return this.qualitySending;
  }

  // drainQuality sends until no newer choice waits. A refusal for the rate
  // bound stops it: its one later send (sendQuality) takes the newest choice
  // by then.
  private async drainQuality(retry: boolean): Promise<QualityAnswer> {
    let answer: QualityAnswer = 'sent';
    let first = true;
    while (this.qualityDue) {
      this.qualityDue = false;
      answer = await this.sendQuality(first ? retry : true);
      first = false;
      if (answer === 'later') {
        this.qualityDue = false;
        break;
      }
    }
    return answer;
  }

  // sendQuality sends the current choice as a holder's repeat claim; retry
  // says whether a refusal for the rate bound may be sent once more.
  private async sendQuality(retry: boolean): Promise<QualityAnswer> {
    const channel = this.channel;
    if (this.state.kind !== 'sharing' || channel === null) return 'sent';
    const mine = this.attempt;
    const answer = await this.api.claim(channel, this.quality);
    if (mine !== this.attempt) {
      // Stopped or ended meanwhile; a newer share's claim would have its own
      // start, and is left alone.
      if (answer.ok && this.ending()) await this.api.release(this.channel ?? channel, answer.value.started_at).catch(() => undefined);
      return 'sent';
    }
    if (answer.ok) {
      if (await this.newShare(answer.value.started_at)) return 'sent';
      this.granted(answer.value.started_at);
      return 'sent';
    }
    if (answer.status === 429 || answer.code === 'rate_limited') {
      if (!retry) return 'sent';
      const wait = Math.min(Math.max(answer.retryAfter ?? 1, 1), qualityRetryLimit);
      this.qualityRetry = this.clock.setTimeout(() => {
        this.qualityRetry = null;
        if (mine === this.attempt) void this.queueQuality(false);
      }, wait * 1000);
      return 'later';
    }
    const end = refusalEnd(answer);
    if (end === 'full' || end === 'not_connected') {
      if (this.renewed()) this.suspend();
      else await this.serverEnded();
    }
    return 'sent';
  }

  // endQualityRetry drops a quality change waiting to be sent again.
  private endQualityRetry(): void {
    if (this.qualityRetry !== null) this.clock.clearTimeout(this.qualityRetry);
    this.qualityRetry = null;
  }

  // stop ends the share from this side: the Stop sharing button, the
  // browser's own stop control, leaving voice, or a connection that was lost
  // and replaced. conchd is asked to end it while it is still held. 'rotated'
  // is the desktop companion's room renewed: its share is not resumed (Q-D4-2
  // on #1578), so it ends with the renewal's words.
  async stop(cause: 'self' | 'browser' | 'left' | 'lost' | 'rotated'): Promise<void> {
    if (this.state.kind === 'idle' || this.state.kind === 'stopping') return;
    const mine = ++this.attempt;
    const { channel, room } = this;
    this.dropKept();
    this.set({ kind: 'stopping' });
    await room?.stopScreen();
    if (channel !== null) await this.releaseHeld(channel);
    if (mine === this.attempt) this.set({ kind: 'idle', ended: cause });
  }

  // isIdle reads the state afresh: after an await it may have changed.
  private isIdle(): boolean {
    return this.state.kind === 'idle';
  }

  // ending reads afresh whether the share is over or being stopped.
  private ending(): boolean {
    return this.state.kind === 'idle' || this.state.kind === 'stopping';
  }

  // granted records a claim conchd granted: the share is ours, and is the
  // one of that started_at. Asking again while holding it answers the same
  // time; a new time is a new share, and what presence said of the last one
  // is not about it.
  private granted(startedAt: string): void {
    this.held = true;
    if (this.claimed !== null && sameInstant(this.claimed, startedAt)) return;
    this.claimed = startedAt;
    this.told = null;
  }

  // releaseHeld asks conchd to end the share if it is known to be ours, once,
  // naming the claim: conchd refuses it if that claim has ended meanwhile.
  private async releaseHeld(channel: string): Promise<void> {
    if (!this.held || this.claimed === null) return;
    this.held = false;
    await this.api.release(channel, this.claimed).catch(() => undefined);
  }

  // ended is the room saying the published share is gone: the browser's stop
  // control (which is the person's own stop) or the server's withdrawal.
  ended(cause: ScreenEnd): void {
    if (this.state.kind === 'resuming') {
      // The old room going is expected; the capture ending is not.
      if (cause === 'browser') void this.giveUp('browser');
      return;
    }
    if (this.state.kind !== 'sharing' && this.state.kind !== 'publishing') return;
    if (cause === 'browser') void this.stop('browser');
    else if (this.renewed()) this.suspend();
    else void this.serverEnded();
  }

  // presence is who the voice socket shows sharing, and why this person's
  // last share ended when the server ended it. A share shown as someone
  // else's, or nobody's, is no longer ours to stop. While the picker is open a
  // share gone is not an end: the claim is made again after it.
  //
  // The reason counts only for the share of the last claim (by its
  // started_at). It may come before the room's withdrawal is seen, or after
  // the share already ended without it: then the end is said again, with
  // the reason.
  presence(holder: Holder, ended?: VoiceShareEnded): void {
    if (ended !== undefined && this.claimed !== null && sameInstant(ended.started_at, this.claimed)) {
      this.told = toldEnd(ended.reason);
      if (this.told === 'rotated' && (this.onAir() || this.state.kind === 'resuming')) this.renewal = true;
      if (this.state.kind === 'idle' && this.state.ended === 'server') this.set({ kind: 'idle', ended: this.told });
    }
    if (holder !== 'me') this.held = false;
    if (this.state.kind === 'resuming') {
      // Only the room's renewal is resumed: a share told to have ended for
      // any other reason (an operator, say) stays ended.
      if (this.told !== null && this.told !== 'rotated') void this.giveUp(this.told);
      else void this.resume();
      return;
    }
    if (this.state.kind === 'idle' || this.state.kind === 'stopping') return;
    if (holder === 'me') {
      this.held = true;
      this.seen = true;
      return;
    }
    if (this.renewed()) this.suspend();
    else if (this.seen && this.onAir()) void this.serverEnded();
  }

  // lost is the connection ending (connection.ts), before it is closed. A
  // share published or being published whose room was deleted, or that
  // presence has said was renewed, is kept for the next connection; any other
  // ends, as before.
  lost(roomDeleted: boolean): Promise<void> {
    if (this.companion && this.renewed()) return this.stop('rotated');
    if (!this.companion && this.onAir() && (roomDeleted || this.renewed())) this.suspend();
    // A resume under way on the connection going stops where it is; the next
    // connection resumes again.
    else if (this.state.kind === 'resuming') this.attempt++;
    if (this.state.kind !== 'resuming') return this.stop('lost');
    // Taken off the old connection now, if it was not already: closing it
    // stops what it holds.
    this.keepCapture();
    if (this.kept === null) return this.giveUp(this.told ?? 'lost');
    return Promise.resolve();
  }

  // keepCapture takes the capture off the connection it was published on,
  // still running, and lets that connection go: nothing is claimed or
  // published on it again.
  private keepCapture(): void {
    if (this.kept === null && this.room !== null) {
      const kept = this.room.keepScreen();
      this.kept = kept;
      if (kept !== null) {
        kept.onEnded = () => {
          if (this.kept === kept) void this.giveUp('browser');
        };
      }
    }
    this.room = null;
  }

  // connected is a new connection up (connection.ts): a share kept from the
  // last one is resumed on it once presence has said the room was renewed.
  connected(channel: string, room: ShareRoom): void {
    if (this.state.kind !== 'resuming' || this.kept === null) return;
    this.channel = channel;
    this.room = room;
    void this.resume();
  }

  // renamed is the channel renamed (issue #728): the share and its claim stay
  // as they are, and what is asked of conchd from now on names the new name.
  renamed(channel: string): void {
    if (this.channel !== null) this.channel = channel;
  }

  // abandon is the client stopping (signed out, no longer a member, another
  // device took over): a share being resumed ends.
  abandon(): void {
    if (this.state.kind === 'resuming') void this.giveUp(this.told ?? 'lost');
  }

  // onAir reports whether the share is published, or being published, on the
  // connection: the states a renewal takes it from (issue #535).
  private onAir(): boolean {
    return this.state.kind === 'sharing' || this.state.kind === 'publishing';
  }

  // setCompanion makes this the desktop's screen companion's share (set when
  // the companion is joined), which is never suspended to be resumed.
  setCompanion(on: boolean): void {
    this.companion = on;
  }

  // renewed reports whether the server ended the share published or being
  // published because its room was renewed.
  private renewed(): boolean {
    return this.onAir() && this.told === 'rotated';
  }

  // suspend begins waiting to resume: the capture goes on, and is stopped if
  // the share is not resumed in time.
  //
  // The capture is taken off the old connection at once. The room conchd
  // retired may live on for a while (its deletion can fail, and the sweep
  // retries for up to 30 s), and someone removed from the channel may still
  // be in it: nothing is shown there once conchd has ended the share. A
  // publish still under way there sends only a blanked capture, and is taken
  // down when it completes (sdk.ts keepScreen).
  //
  // A share renewed again before it was back (issue #535) carries on with
  // what is left of the first wait, and one whose wait ran out while it was
  // being published ends here.
  private suspend(): void {
    // A companion's share is not resumable: the helper's connection may
    // already be in the renewed room, so a claim made again without a click
    // could succeed and publish onto the retired one.
    if (this.companion) {
      void this.serverEnded();
      return;
    }
    this.attempt++;
    this.set({ kind: 'resuming' });
    if (!this.interrupted) {
      this.interrupted = true;
      this.resumeTimer = this.clock.setTimeout(() => {
        this.resumeTimer = null;
        if (this.state.kind === 'resuming') void this.giveUp(this.told ?? 'lost');
      }, resumeLimit);
    }
    this.keepCapture();
    if (this.kept === null || this.resumeTimer === null) void this.giveUp(this.told ?? 'lost');
  }

  // resume claims the share again and publishes the kept capture on the new
  // connection, once all three are there: the capture, the connection, and
  // presence saying the room was renewed.
  private async resume(): Promise<void> {
    const { kept, room, channel } = this;
    if (this.state.kind !== 'resuming' || this.reclaiming || kept === null || room === null || channel === null || !this.renewal) return;
    if (!kept.live()) {
      await this.giveUp('browser');
      return;
    }
    const mine = ++this.attempt;
    this.reclaiming = true;
    this.seen = false;
    const answer = await this.api.claim(channel, this.quality);
    this.reclaiming = false;
    // Renamed meanwhile (issue #728): what follows names the new name.
    const at = () => this.channel ?? channel;
    if (answer.ok) this.granted(answer.value.started_at);
    if (mine !== this.attempt) {
      // Stopped while the claim was on its way: a claim granted since is let
      // go, unless a newer share has begun.
      if (this.isIdle()) await this.releaseHeld(at());
      return;
    }
    if (!answer.ok) {
      const end = refusalEnd(answer);
      if (end === 'not_connected') {
        // No session for the channel's current room: the room was renewed
        // again after this connection was made (several people removed one
        // after another). The next connection resumes, within the limit.
        if (this.room === room) this.room = null;
        return;
      }
      // The room is full, or this person may no longer share here: conchd said
      // no, and the capture stops (voice-multi-share.md §5.1, #1509).
      await this.giveUp(end);
      return;
    }
    if (!kept.live()) {
      await this.giveUp('browser');
      return;
    }
    this.kept = null;
    kept.onEnded = null;
    // Claimed in the new room: the renewal that ended the last room's share
    // is spent, and only presence saying this claim's room was renewed too
    // resumes it again. The resume timer runs on while it is published.
    this.renewal = false;
    this.set({ kind: 'publishing' });
    let adopted = false;
    try {
      room.adoptScreen(kept);
      adopted = true;
      await room.publishScreen();
    } catch {
      // Taken over meanwhile: stopped (the capture with it), or the room
      // renewed again, which kept the capture for the next connection, still
      // running (issue #535). The renewal is known before the publish fails
      // for it: the SDK reports a deleted room (lost) before it fails a
      // publish under way, and the connection is closed only after lost.
      if (mine !== this.attempt) return;
      // Presence said the room was renewed, before the room withdrew
      // anything: the capture is kept from the connection that has it.
      if (adopted && this.renewed()) {
        this.suspend();
        return;
      }
      kept.stop();
      this.endInterruption();
      await room.stopScreen();
      await this.releaseHeld(at());
      if (mine === this.attempt) this.set({ kind: 'idle', ended: 'failed' });
      return;
    }
    if (mine !== this.attempt) return;
    // The share carried on: what ended the last room's is not about this one.
    this.endInterruption();
    this.told = null;
    this.set({ kind: 'sharing', audio: kept.audio, resumed: true });
  }

  // giveUp ends a share that was being resumed: the capture stops, and a
  // claim still known to be this person's is let go.
  private async giveUp(end: ShareEnd): Promise<void> {
    const mine = ++this.attempt;
    const { channel, room, kept } = this;
    this.dropKept();
    this.set({ kind: 'stopping' });
    // Not yet kept, the capture is still the old connection's.
    if (kept === null) await room?.stopScreen();
    if (channel !== null) await this.releaseHeld(channel);
    if (mine === this.attempt) this.set({ kind: 'idle', ended: end });
  }

  // dropKept stops a kept capture and the wait for it.
  private dropKept(): void {
    this.endInterruption();
    const kept = this.kept;
    this.kept = null;
    if (kept === null) return;
    kept.onEnded = null;
    kept.stop();
  }

  // endInterruption ends the wait to resume: the share is back, or over.
  private endInterruption(): void {
    if (this.resumeTimer !== null) this.clock.clearTimeout(this.resumeTimer);
    this.resumeTimer = null;
    this.interrupted = false;
  }

  // reconnected claims the caller's share again after the SDK reconnected, as
  // conchd asks: a connection made anew needs the sources granted to it again,
  // with the same declared quality. A claim refused as full means the share is
  // gone; any other failure leaves it to the server's own passes
  // (voice-multi-share.md §5.1, #1509).
  async reconnected(): Promise<void> {
    if (this.state.kind !== 'sharing' || this.channel === null) return;
    const mine = this.attempt;
    const answer = await this.api.claim(this.channel, this.quality);
    if (mine !== this.attempt) return;
    if (answer.ok) {
      if (await this.newShare(answer.value.started_at)) return;
      this.granted(answer.value.started_at);
      return;
    }
    if (refusalEnd(answer) === 'full') await this.serverEnded();
  }

  // newShare handles a claim granted again to a companion with a started_at
  // other than the share's: the server ended that share and this is a new one,
  // made without a click. A companion's share is never adopted so (Q-D4-2 on
  // #1578): the new claim is taken only to be released, and the share ends
  // with the renewal's words. It says whether it did.
  private async newShare(startedAt: string): Promise<boolean> {
    if (!this.companion || this.claimed === null || sameInstant(this.claimed, startedAt)) return false;
    this.granted(startedAt);
    await this.stop('rotated');
    return true;
  }

  private async serverEnded(): Promise<void> {
    const mine = ++this.attempt;
    this.held = false;
    const room = this.room;
    this.dropKept();
    this.set({ kind: 'stopping' });
    await room?.stopScreen();
    // The reason, if presence has said it by now; if not, presence says it
    // when it comes (presence above).
    if (mine === this.attempt) this.set({ kind: 'idle', ended: this.told ?? 'server' });
  }
}

// What the page knows of the browser before sharing, from feature detection
// and the browser's own client hints. Firefox and Safari give no client hints.
export type Platform = 'windows' | 'mac' | 'linux' | 'chromeos' | 'android' | 'ios' | 'other';

export interface ShareFeatures {
  // The browser has a screen picker (no iPhone or iPad browser does).
  readonly canShare: boolean;
  // Chrome, Edge or another Chromium: the browsers that share sound.
  readonly chromium: boolean;
  readonly platform: Platform;
}

interface ClientHints {
  readonly brands?: readonly { readonly brand: string }[];
  readonly platform?: string;
}

interface Browser {
  readonly userAgent: string;
  readonly mediaDevices?: { readonly getDisplayMedia?: unknown };
  readonly userAgentData?: ClientHints;
}

function platformOf(hint: string | undefined, agent: string): Platform {
  const name = (hint ?? '').toLowerCase();
  if (name === 'windows') return 'windows';
  if (name === 'macos') return 'mac';
  if (name === 'linux') return 'linux';
  if (name === 'chrome os' || name === 'chromeos') return 'chromeos';
  if (name === 'android' || /Android/.test(agent)) return 'android';
  if (/iPhone|iPad|iPod/.test(agent)) return 'ios';
  if (/Windows/.test(agent)) return 'windows';
  if (/Macintosh/.test(agent)) return 'mac';
  if (/Linux/.test(agent)) return 'linux';
  return 'other';
}

// browserFeatures reads this browser's features. It asks for no device: the
// picker is only looked for.
export function browserFeatures(): ShareFeatures {
  // Not every browser has these: a page that is not a secure context has no
  // mediaDevices, and only Chromium gives client hints.
  const browser: Browser = navigator;
  const devices = browser.mediaDevices;
  const hints = browser.userAgentData;
  return {
    canShare: devices !== undefined && typeof devices.getDisplayMedia === 'function',
    chromium: hints?.brands?.some((b) => b.brand === 'Chromium') ?? false,
    platform: platformOf(hints?.platform, navigator.userAgent),
  };
}

// soundNote says, before the picker opens, what sound a share will carry from
// this browser (plan section 2: tab sound in Chrome or Edge, the whole
// system's sound on Windows, none in Firefox or Safari).
export function soundNote(f: ShareFeatures): string {
  if (!f.canShare) return 'This browser cannot share a screen (no iPhone or iPad browser can). You can still watch.';
  if (!f.chromium) return 'No sound: this browser shares the picture only. Chrome or Edge share a tab\'s sound.';
  switch (f.platform) {
    case 'windows':
      return 'Sound: a tab shares its own sound, and the entire screen shares the whole system\'s sound. Tick the sound box in the picker.';
    case 'android':
      return 'No sound: a phone shares the picture only.';
    default:
      return 'Sound: only a tab shares its sound (tick the sound box in the picker). A window or the entire screen is shared without sound.';
  }
}

// shortShareNote is soundNote and ownPageNote in one line, which is what the
// strip shows before a share (issue #600): whether sound comes, and that a
// whole screen shows this page. The full notes are in Voice options.
export function shortShareNote(f: ShareFeatures): string {
  if (!f.canShare) return soundNote(f);
  const page = 'a whole screen also shows this page.';
  if (!f.chromium || f.platform === 'android') return `Sharing: the picture only, no sound; ${page}`;
  if (f.platform === 'windows') return `Sharing: a tab or the entire screen carries sound; ${page}`;
  return `Sharing: only a tab carries sound; ${page}`;
}

// othersShareNote says who else shares a screen, and whether with sound.
export function othersShareNote(who: string | null, audio: boolean): string {
  return `${who ?? 'Someone else'} is sharing a screen${audio ? ', with sound' : ''}.`;
}

// watchNote is what a page that is not watching is told of another's share:
// where to see it (issue #642, N02). Multi-share puts Watch on each stream's
// own tile instead of disabling Share screen (voice-multi-share.md §7.1, #1509).
export function watchNote(who: string | null, audio: boolean): string {
  return `${othersShareNote(who, audio)} Watch to see it.`;
}

// endedNote says why the last share ended, or why one did not start. A full
// refusal names the room's six-stream cap, not another sharer (#1509).
export function endedNote(end: ShareEnd): string {
  switch (end) {
    case 'self':
      return 'You stopped sharing.';
    case 'browser':
      return 'Sharing stopped from the browser.';
    case 'left':
      return 'You left voice, so your share ended.';
    case 'lost':
      return 'Your share ended when the voice connection dropped. Share again if you want to.';
    case 'server':
      return 'The server ended your share: an operator stopped it, or you may no longer share here.';
    case 'operator':
      return 'An operator stopped your share.';
    case 'access_lost':
      return 'Your share ended: you no longer have voice in this channel.';
    case 'rotated':
      return 'Your share ended because the voice room was renewed. Share again once you are back in voice.';
    case 'idle':
      return 'Your share ended because nothing was shared for a while.';
    case 'full':
      return 'This call already has 6 streams live.';
    case 'cancelled':
      return 'Nothing was shared: the picker was closed.';
    case 'not_connected':
      return 'Your voice connection is not ready yet. Try again in a moment.';
    case 'unavailable':
      return 'Sharing is unavailable right now. Try again in a moment.';
    case 'failed':
      return 'Your share could not start.';
  }
}

// What the sharer is told while a share is carried into a renewed room
// (issue #516), and once it was.
export const resumingNote = 'The voice room was renewed. Resuming your share…';
export const resumedNote = 'The voice room was renewed, and your share carried on in the new one.';

// ownPageNote is said before the picker opens: the Conch tab is left out of
// the picker, but a whole screen or this browser's window shows the page, and
// with it every message on screen.
export const ownPageNote = 'Sharing your entire screen or this browser window shows this page too, chat included. Share a tab or another window to keep it out.';

// sharedNote says, once the share is published, whether sound came with it.
export function sharedNote(audio: boolean, f: ShareFeatures): string {
  if (audio) return 'You are sharing your screen with sound.';
  if (f.chromium && f.platform !== 'android') return "Sharing without sound. To share sound, share a browser tab and keep 'Share tab audio' on.";
  return "Sharing without sound. This browser can't share sound.";
}
