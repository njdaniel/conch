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
import type { ShareClaimResult } from '../api';
import type { VoiceShareEnded, VoiceShareEndReason } from '../schema/voice';
import type { Clock } from '../state/feed';

// What is being shared: text and pictures, or video or a game. It becomes the
// capture's content hint, which tells the encoder what to keep.
export type ShareContent = 'detail' | 'motion';

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
  // Opens the browser's picker and keeps what was chosen, unpublished. Rejects
  // when nothing was chosen.
  readonly pickScreen: (content: ShareContent) => Promise<Captured>;
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
  | 'busy'
  | 'cancelled'
  | 'not_connected'
  | 'unavailable'
  | 'failed';

export type ShareState =
  | { readonly kind: 'idle'; readonly ended: ShareEnd | null }
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

// How long a share waits to be resumed after its room was renewed: longer
// than conchd's sweep takes to delete an old room whose deletion failed (30
// s), and a reconnect after it. Past it the capture is stopped.
export const resumeLimit = 45_000;

export interface ShareApi {
  readonly claim: (channel: string) => Promise<ShareClaimResult>;
  // Releases the claim whose started_at is startedAt, and no other.
  readonly release: (channel: string, startedAt: string) => Promise<unknown>;
}

type Refusal = Extract<ShareClaimResult, { ok: false }>;

// refusalEnd names a refused claim.
export function refusalEnd(r: Refusal): ShareEnd {
  if (r.code === 'voice_share_busy') return 'busy';
  if (r.code === 'voice_no_session') return 'not_connected';
  if (r.code === 'voice_unavailable' || r.failure === 'network') return 'unavailable';
  return 'failed';
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
    this.onChange(state);
  }

  // start claims the room's share, opens the picker, claims again and
  // publishes. It does nothing unless the last share is over.
  async start(channel: string, room: ShareRoom, content: ShareContent): Promise<void> {
    if (this.state.kind !== 'idle') return;
    const mine = ++this.attempt;
    this.channel = channel;
    this.room = room;
    this.seen = false;
    this.renewal = false;
    this.set({ kind: 'claiming' });
    const first = await this.api.claim(channel);
    if (first.ok) this.granted(first.value.started_at);
    if (mine !== this.attempt) {
      // Stopped while the claim was on its way: a claim granted since is ours
      // and is let go, unless a newer share has begun.
      if (this.isIdle()) await this.releaseHeld(channel);
      return;
    }
    if (!first.ok) {
      this.set({ kind: 'idle', ended: refusalEnd(first) });
      return;
    }
    this.set({ kind: 'picking' });
    let captured: Captured;
    try {
      captured = await room.pickScreen(content);
    } catch {
      if (mine !== this.attempt) return;
      // Closed without choosing, or refused by the browser: the claim goes.
      this.set({ kind: 'stopping' });
      await this.releaseHeld(channel);
      if (mine === this.attempt) this.set({ kind: 'idle', ended: 'cancelled' });
      return;
    }
    if (mine !== this.attempt) {
      await room.stopScreen();
      return;
    }
    this.set({ kind: 'publishing' });
    this.seen = false;
    const again = await this.api.claim(channel);
    if (again.ok) this.granted(again.value.started_at);
    if (mine !== this.attempt) {
      await room.stopScreen();
      if (this.isIdle()) await this.releaseHeld(channel);
      return;
    }
    if (!again.ok) {
      const end = refusalEnd(again);
      // A share now someone else's is not ours to stop.
      if (end === 'busy') this.held = false;
      await room.stopScreen();
      await this.releaseHeld(channel);
      if (mine === this.attempt) this.set({ kind: 'idle', ended: end });
      return;
    }
    try {
      await room.publishScreen();
    } catch {
      if (mine !== this.attempt) return;
      await room.stopScreen();
      await this.releaseHeld(channel);
      if (mine === this.attempt) this.set({ kind: 'idle', ended: 'failed' });
      return;
    }
    if (mine !== this.attempt) return;
    this.set({ kind: 'sharing', audio: captured.audio });
  }

  // stop ends the share from this side: the Stop sharing button, the
  // browser's own stop control, leaving voice, or a connection that was lost
  // and replaced. conchd is asked to end it while it is still held.
  async stop(cause: 'self' | 'browser' | 'left' | 'lost'): Promise<void> {
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
      if (this.told === 'rotated' && (this.state.kind === 'sharing' || this.state.kind === 'resuming')) this.renewal = true;
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
    else if (this.seen && (this.state.kind === 'sharing' || this.state.kind === 'publishing')) void this.serverEnded();
  }

  // lost is the connection ending (connection.ts), before it is closed. A
  // share whose room was deleted, or that presence has said was renewed, is
  // kept for the next connection; any other ends, as before.
  lost(roomDeleted: boolean): Promise<void> {
    if (this.state.kind === 'sharing' && (roomDeleted || this.renewed())) this.suspend();
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

  // abandon is the client stopping (signed out, no longer a member, another
  // device took over): a share being resumed ends.
  abandon(): void {
    if (this.state.kind === 'resuming') void this.giveUp(this.told ?? 'lost');
  }

  // renewed reports whether the server ended the published share because its
  // room was renewed.
  private renewed(): boolean {
    return this.state.kind === 'sharing' && this.told === 'rotated';
  }

  // suspend begins waiting to resume: the capture goes on, and is stopped if
  // the share is not resumed in time.
  //
  // The capture is taken off the old connection at once. The room conchd
  // retired may live on for a while (its deletion can fail, and the sweep
  // retries for up to 30 s), and someone removed from the channel may still
  // be in it: nothing is shown there once conchd has ended the share.
  private suspend(): void {
    this.attempt++;
    this.set({ kind: 'resuming' });
    this.resumeTimer = this.clock.setTimeout(() => {
      this.resumeTimer = null;
      if (this.state.kind === 'resuming') void this.giveUp(this.told ?? 'lost');
    }, resumeLimit);
    this.keepCapture();
    if (this.kept === null) void this.giveUp(this.told ?? 'lost');
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
    const answer = await this.api.claim(channel);
    this.reclaiming = false;
    if (answer.ok) this.granted(answer.value.started_at);
    if (mine !== this.attempt) {
      // Stopped while the claim was on its way: a claim granted since is let
      // go, unless a newer share has begun.
      if (this.isIdle()) await this.releaseHeld(channel);
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
      // Someone else claimed the share first, or this person may no longer
      // share here: conchd said no, and the capture stops.
      if (end === 'busy') this.held = false;
      await this.giveUp(end);
      return;
    }
    if (!kept.live()) {
      await this.giveUp('browser');
      return;
    }
    this.kept = null;
    kept.onEnded = null;
    this.clearResumeTimer();
    this.set({ kind: 'publishing' });
    try {
      room.adoptScreen(kept);
      await room.publishScreen();
    } catch {
      kept.stop();
      if (mine !== this.attempt) return;
      await room.stopScreen();
      await this.releaseHeld(channel);
      if (mine === this.attempt) this.set({ kind: 'idle', ended: 'failed' });
      return;
    }
    if (mine !== this.attempt) return;
    // The share carried on: what ended the last room's is not about this one.
    this.renewal = false;
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
    this.clearResumeTimer();
    const kept = this.kept;
    this.kept = null;
    if (kept === null) return;
    kept.onEnded = null;
    kept.stop();
  }

  private clearResumeTimer(): void {
    if (this.resumeTimer !== null) this.clock.clearTimeout(this.resumeTimer);
    this.resumeTimer = null;
  }

  // reconnected claims the share again after the SDK reconnected, as conchd
  // asks: a connection made anew needs the sources granted to it again. A
  // claim refused as busy means the share is gone; any other failure leaves
  // it to the server's own passes.
  async reconnected(): Promise<void> {
    if (this.state.kind !== 'sharing' || this.channel === null) return;
    const mine = this.attempt;
    const answer = await this.api.claim(this.channel);
    if (mine !== this.attempt) return;
    if (answer.ok) {
      this.granted(answer.value.started_at);
      return;
    }
    if (refusalEnd(answer) === 'busy') await this.serverEnded();
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

// endedNote says why the last share ended, or why one did not start. who is
// the person presence shows sharing, when it shows someone.
export function endedNote(end: ShareEnd, who: string | null): string {
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
    case 'busy':
      return who === null ? 'Someone else is sharing. One person shares at a time.' : `${who} is sharing. One person shares at a time.`;
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
  if (audio) return 'You are sharing your screen with its sound.';
  if (f.chromium && f.platform !== 'android') return 'You are sharing your screen without sound. To share sound, stop and pick a tab with its sound box ticked.';
  return 'You are sharing your screen without sound.';
}
