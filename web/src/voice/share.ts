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
// would end another person's share as a moderation stop. A share the server
// ended (an operator, the sharer's access, the room) is not asked about again:
// the page only stops publishing and says so.
import type { ShareClaimResult } from '../api';

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

// What a share needs of the room connection (sdk.ts implements it).
export interface ShareRoom {
  // Opens the browser's picker and keeps what was chosen, unpublished. Rejects
  // when nothing was chosen.
  readonly pickScreen: (content: ShareContent) => Promise<Captured>;
  // Publishes what was picked, with its simulcast layers.
  readonly publishScreen: () => Promise<void>;
  // Unpublishes and stops whatever was picked or published. Never rejects.
  readonly stopScreen: () => Promise<void>;
}

// Why the last share ended, or why one did not start.
export type ShareEnd =
  | 'self'
  | 'browser'
  | 'left'
  | 'lost'
  | 'server'
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
  | { readonly kind: 'sharing'; readonly audio: boolean }
  | { readonly kind: 'stopping' };

export const idleShare: ShareState = { kind: 'idle', ended: null };

export interface ShareApi {
  readonly claim: (channel: string) => Promise<ShareClaimResult>;
  readonly release: (channel: string) => Promise<unknown>;
}

type Refusal = Extract<ShareClaimResult, { ok: false }>;

// refusalEnd names a refused claim.
export function refusalEnd(r: Refusal): ShareEnd {
  if (r.code === 'voice_share_busy') return 'busy';
  if (r.code === 'voice_no_session') return 'not_connected';
  if (r.code === 'voice_unavailable' || r.failure === 'network') return 'unavailable';
  return 'failed';
}

export class ScreenShare {
  private readonly api: ShareApi;
  private readonly onChange: (state: ShareState) => void;
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

  constructor(api: ShareApi, onChange: (state: ShareState) => void) {
    this.api = api;
    this.onChange = onChange;
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
    this.set({ kind: 'claiming' });
    const first = await this.api.claim(channel);
    if (first.ok) this.held = true;
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
    if (again.ok) this.held = true;
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
    this.set({ kind: 'stopping' });
    await room?.stopScreen();
    if (channel !== null) await this.releaseHeld(channel);
    if (mine === this.attempt) this.set({ kind: 'idle', ended: cause });
  }

  // isIdle reads the state afresh: after an await it may have changed.
  private isIdle(): boolean {
    return this.state.kind === 'idle';
  }

  // releaseHeld asks conchd to end the share if it is known to be ours, once.
  private async releaseHeld(channel: string): Promise<void> {
    if (!this.held) return;
    this.held = false;
    await this.api.release(channel).catch(() => undefined);
  }

  // ended is the room saying the published share is gone: the browser's stop
  // control (which is the person's own stop) or the server's withdrawal.
  ended(cause: ScreenEnd): void {
    if (this.state.kind !== 'sharing' && this.state.kind !== 'publishing') return;
    if (cause === 'browser') void this.stop('browser');
    else void this.serverEnded();
  }

  // presence is who the voice socket shows sharing. A share shown as someone
  // else's, or nobody's, is no longer ours to stop. While the picker is open a
  // share gone is not an end: the claim is made again after it.
  presence(holder: Holder): void {
    if (holder !== 'me') this.held = false;
    if (this.state.kind === 'idle' || this.state.kind === 'stopping') return;
    if (holder === 'me') {
      this.held = true;
      this.seen = true;
      return;
    }
    if (this.seen && (this.state.kind === 'sharing' || this.state.kind === 'publishing')) void this.serverEnded();
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
      this.held = true;
      return;
    }
    if (refusalEnd(answer) === 'busy') await this.serverEnded();
  }

  private async serverEnded(): Promise<void> {
    const mine = ++this.attempt;
    this.held = false;
    const room = this.room;
    this.set({ kind: 'stopping' });
    await room?.stopScreen();
    if (mine === this.attempt) this.set({ kind: 'idle', ended: 'server' });
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
