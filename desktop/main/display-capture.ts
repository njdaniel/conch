// The renderer's one screen capture (issue #1580, D4-03 of #772;
// linux-desktop.md §3, §6.3, §8; Q-D4-4 on #1578). Input: Chromium's
// permission request and display-media request for the window, the person's
// share allowance (share-allowance.ts), the companion's live grant (voice/companion.ts),
// the window's document gate and the portal's answer (portal.ts). Output:
// a yes to exactly one permission, and one source the portal's own picker
// returned, video only; no to everything else. Owns: the one pick in flight.
//
// What Chromium asks, measured on the pinned Electron 44.10.0 (a headless
// probe logging every handler): getDisplayMedia makes one permission request,
// 'media' with mediaTypes [] (Electron names display capture's request
// "media", and its media types list only devices), and no permission check;
// 'display-capture' is never asked. Then, synchronously inside the
// permission's callback, the display-media handler runs. So:
//  - the permission request 'media' with mediaTypes exactly [] is allowed,
//    for the owned window's main frame at conch-app://ui/, with the share
//    allowance's capture use unspent, a live companion grant and the portal
//    available. It uses up nothing; the display-media request takes the use;
//  - the legacy getUserMedia({video: {mandatory: {chromeMediaSource:
//    'desktop'}}}) asks the same permission with the same details, and on
//    a yes Electron opens the screen (and "loopback" system audio) itself,
//    with no display-media request and no picker of ours. The two cannot be
//    told apart when asked, only afterwards: a yes that is not followed,
//    inside its callback, by the display-media request was the legacy path,
//    and the renderer is ended at once (a compromised page; the web client
//    never asks it). Ending the process ends whatever was opened for it, and
//    the companion with it (render-process-gone, app.ts);
//  - microphone and camera ('media' with "audio" or "video") stay denied,
//    always: the microphone is the helper's (#1374).
//
// The display-media request is granted only when every one of these holds,
// each checked again after the picker returns:
//  - it comes from the owned window's live main frame, at conch-app://ui/, in
//    the current document generation;
//  - the person's Share click armed a share allowance a moment ago (the page's
//    shareArm(), which took the click; share-allowance.ts) and its capture use
//    is unspent: the request uses it up, granted or not. A raw click, or the
//    Enter that sends a message, arms nothing here (S3 of #1621). The capture
//    use is good for SHARE_CAPTURE_MS (10 s) from the later of the arm and the
//    grant being issued. Residual: a compromised page can still call shareArm()
//    within 1 s of any act, so the OS portal picker is the consent, and the
//    allowance only stops an honest page arming capture by accident (S1);
//  - a companion grant is live (the companion ends on switch, sign-out,
//    reload and crash, so it is always this activation's and this document's);
//  - the portal is available, and Electron's desktopCapturer returned exactly
//    one source: the one the portal's picker chose. None is a cancel or a
//    denial; more than one is not a portal at all, and is never narrowed to
//    its first (#772: never "automatically choose the first available
//    screen").
// Audio is never given: Electron's 'loopback' is Windows-only, and Linux has
// no share-audio route (§8). A request that asks for audio gets video only.
import type { DesktopCapturerSource, SourcesOptions } from 'electron';

import type { ShareAvailability } from './portal.ts';
import { INITIAL_URL } from './scheme.ts';
import type { ShareAllowance } from './share-allowance.ts';

export interface CaptureFrame {
  readonly frameToken: string;
  readonly url: string;
  readonly parent: unknown;
  readonly detached: boolean;
}

export interface CaptureContents {
  readonly mainFrame: { readonly frameToken: string };
  isDestroyed(): boolean;
  forcefullyCrashRenderer(): void;
  // The fallback when the crash does not take (S5 of #1621): the renderer's
  // process is gone when it emits this, and close() ends the page.
  once(event: 'render-process-gone', listener: () => void): unknown;
  close(): void;
}

// How long a crashed renderer has to be gone before the page is closed
// instead. forcefullyCrashRenderer is a request to the process, not a kill.
export const CRASH_GRACE_MS = 1_000;

// The Chromium feature that makes screen capture go through the portal's own
// picker (set on by app.ts).
export const PIPEWIRE_FEATURE = 'WebRTCPipeWireCapturer';

// captureVerdict refuses a packaged launch that turned the PipeWire capturer
// off (S4 of #1621). Without it Chromium lists the sources itself, with no
// portal picker, and the one source desktopCapturer then returned would stand
// in for the person's pick. Like debugging.ts it catches a launcher's mistake
// (a wrapper, a stale desktop entry), not a same-user attacker. The switch
// value is a comma list whose entries may carry a trial ("Name<Trial") or
// parameters ("Name:k/v"), or a leading "*" ("*Name"); Chromium lets disable-features win over
// enable-features, and "-Name" in enable-features also turns a feature off.
export function captureVerdict(packaged: boolean, switchValue: (name: string) => string, log: (line: string) => void): 'ok' | 'refused' {
  if (!packaged) return 'ok';
  const entries = (name: string): string[] => switchValue(name).split(',').map((e) => e.trim().replace(/[<:].*$/u, '').replace(/^\*+/u, ''));
  const off = entries('disable-features').includes(PIPEWIRE_FEATURE) || entries('enable-features').includes(`-${PIPEWIRE_FEATURE}`);
  if (!off) return 'ok';
  log(`conch-desktop: refusing to start with ${PIPEWIRE_FEATURE} turned off: screen capture would not go through the portal's picker`);
  return 'refused';
}

// What the display-media handler is given, as Electron gives it.
export interface CaptureRequest {
  readonly frame: CaptureFrame | null;
  readonly videoRequested: boolean;
  readonly audioRequested: boolean;
}

export type CaptureSource = Pick<DesktopCapturerSource, 'id' | 'name'>;

export interface DisplayCaptureDeps {
  // The app's one window's web contents, or null.
  readonly owned: () => CaptureContents | null;
  // The owned window's document gate now.
  readonly gate: () => { current(frame: { readonly frameToken: string }): boolean };
  // The share allowance (share-allowance.ts): the capture use is the only
  // thing that arms a capture. A raw click or key no longer does.
  readonly share: Pick<ShareAllowance, 'peekCapture' | 'takeCapture'>;
  // Whether a companion grant is live (companion.liveOrigin() !== null).
  readonly live: () => boolean;
  // The portal's latest answer (Portal.last).
  readonly available: () => ShareAvailability | 'unknown';
  readonly getSources: (options: SourcesOptions) => Promise<readonly CaptureSource[]>;
  readonly log: (line: string) => void;
}

export const CAPTURE_SOURCES: SourcesOptions = { types: ['screen', 'window'] };

type Refusal =
  | 'not the window'
  | 'a subframe'
  | 'another origin'
  | 'a stale document'
  | 'not display capture'
  | 'no share allowance'
  | 'no grant'
  | 'share unavailable: no screen-cast portal'
  | 'a pick is already open'
  | 'cancelled or denied'
  | 'more than one source, so no portal picker'
  | 'the grant or the document ended during the pick';

// The permission request's details, as far as they are read.
export interface PermissionDetails {
  readonly isMainFrame?: boolean;
  readonly requestingUrl?: string;
  readonly mediaTypes?: readonly string[];
}

// displayPermission says whether a permission request is display capture's:
// 'media' with a media types list that is present and empty.
export function displayPermission(permission: string, details: PermissionDetails | undefined): boolean {
  return permission === 'media' && Array.isArray(details?.mediaTypes) && details.mediaTypes.length === 0;
}

export class DisplayCapture {
  readonly #deps: DisplayCaptureDeps;
  // The contents a display permission was just given to, until its
  // display-media request arrives inside the callback.
  #expecting: object | null = null;
  #picking = false;

  constructor(deps: DisplayCaptureDeps) {
    this.#deps = deps;
  }

  #refuse(reason: Refusal): false {
    this.#deps.log(`conch-desktop: display capture refused: ${reason}`);
    return false;
  }

  #frameRefusal(frame: CaptureFrame | null, owned: CaptureContents | null): Refusal | null {
    if (owned === null || owned.isDestroyed() || frame === null || frame.detached) return 'not the window';
    if (frame.parent !== null) return 'a subframe';
    if (frame.frameToken !== owned.mainFrame.frameToken) return 'not the window';
    if (!frame.url.startsWith(INITIAL_URL)) return 'another origin';
    if (!this.#deps.gate().current(frame)) return 'a stale document';
    return null;
  }

  // #ready is what both questions need beyond the frame: a live grant and
  // the portal.
  #ready(): Refusal | null {
    if (!this.#deps.live()) return 'no grant';
    if (this.#deps.available() !== 'available') return 'share unavailable: no screen-cast portal';
    return null;
  }

  // permits answers the permission request for display capture (the caller
  // has checked displayPermission). Nothing is used up.
  permits(contents: object | null, details: PermissionDetails | undefined): boolean {
    const owned = this.#deps.owned();
    if (contents === null || owned === null || contents !== owned || owned.isDestroyed()) return this.#refuse('not the window');
    if (details?.isMainFrame !== true) return this.#refuse('a subframe');
    if (!(details.requestingUrl ?? '').startsWith(INITIAL_URL)) return this.#refuse('another origin');
    if (!this.#deps.share.peekCapture(owned)) return this.#refuse('no share allowance');
    const not = this.#ready();
    return not === null ? true : this.#refuse(not);
  }

  // allow gives the permission permits allowed, and ends the renderer if the
  // display-media request did not follow inside the callback: the legacy
  // desktop getUserMedia, which no picker of ours would see.
  allow(contents: CaptureContents, callback: (granted: boolean) => void): void {
    this.#expecting = contents;
    try {
      callback(true);
    } finally {
      const followed = this.#followed();
      this.#expecting = null;
      if (!followed) {
        this.#deps.log('conch-desktop: a desktop capture was asked for without a picker; ending the renderer');
        this.#end(contents);
      }
    }
  }

  // #end crashes the renderer, and if it is not gone within CRASH_GRACE_MS
  // (the crash is cooperative: a request to the process), closes the page.
  #end(contents: CaptureContents): void {
    let gone = false;
    try {
      if (contents.isDestroyed()) return;
      contents.once('render-process-gone', () => {
        gone = true;
      });
      contents.forcefullyCrashRenderer();
    } catch {
      // Already gone.
      return;
    }
    const timer = setTimeout(() => {
      if (gone) return;
      try {
        if (contents.isDestroyed()) return;
        this.#deps.log('conch-desktop: the renderer outlived the crash; closing the page');
        contents.close();
      } catch {
        // Already gone.
      }
    }, CRASH_GRACE_MS);
    timer.unref();
  }

  // #followed says whether handle() ran since allow() set the expectation
  // (a method, so the read is not narrowed by the assignment above it).
  #followed(): boolean {
    return this.#expecting === null;
  }

  // handle answers a display-media request: one portal-picked source, video
  // only, or nothing.
  handle(request: CaptureRequest, callback: (streams: { readonly video: CaptureSource } | null) => void): void {
    this.#expecting = null;
    const answer = (streams: { readonly video: CaptureSource } | null): void => {
      try {
        callback(streams);
      } catch {
        // The frame went while the picker was open.
      }
    };
    const owned = this.#deps.owned();
    const frameRefusal = this.#frameRefusal(request.frame, owned);
    if (frameRefusal !== null || owned === null) {
      this.#refuse(frameRefusal ?? 'not the window');
      answer(null);
      return;
    }
    // The allowance's capture use is used up here, whatever follows.
    if (!this.#deps.share.takeCapture(owned)) {
      this.#refuse('no share allowance');
      answer(null);
      return;
    }
    if (!request.videoRequested) {
      this.#refuse('not display capture');
      answer(null);
      return;
    }
    const not = this.#ready();
    if (not !== null) {
      this.#refuse(not);
      answer(null);
      return;
    }
    if (this.#picking) {
      this.#refuse('a pick is already open');
      answer(null);
      return;
    }
    if (request.audioRequested) this.#deps.log('conch-desktop: display capture: audio was asked for and is not given');
    this.#picking = true;
    const frame = request.frame;
    void this.#pick(frame, answer).finally(() => {
      this.#picking = false;
    });
  }

  async #pick(frame: CaptureFrame | null, answer: (streams: { readonly video: CaptureSource } | null) => void): Promise<void> {
    let sources: readonly CaptureSource[];
    try {
      sources = await this.#deps.getSources(CAPTURE_SOURCES);
    } catch {
      // A cancelled PipeWire picker rejects (Electron's desktopCapturer docs).
      sources = [];
    }
    const [only, ...more] = sources;
    if (only === undefined) {
      this.#refuse('cancelled or denied');
      answer(null);
      return;
    }
    if (more.length > 0) {
      this.#refuse('more than one source, so no portal picker');
      answer(null);
      return;
    }
    if (this.#frameRefusal(frame, this.#deps.owned()) !== null || this.#ready() !== null) {
      this.#refuse('the grant or the document ended during the pick');
      answer(null);
      return;
    }
    // Video only: never an audio member.
    answer({ video: only });
  }
}
