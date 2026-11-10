// Separate people in a voice world, for every spec that needs more than one
// (issue #1608). A person is a browser context of their own on Chromium's fake
// devices, a principal of their own signed in, and the channel open: the
// plumbing that 10-voice, 12-screen-share, 44-camera and others each wrote for
// themselves. A new multi-user spec starts here.
//
// It owns: building a person (enter), joining and leaving voice (joinVoice,
// leave), reading what a page has received (inbound) and the check that no
// secret shows on a page (expectClean). It owns no world: the caller starts
// one (voice-world.ts) and passes it in. The specs written before it keep
// their own copies, which are not touched here.
import type { Browser, BrowserContext, BrowserContextOptions, Page } from '@playwright/test';
import { expect, openChannel, rememberPushToTalk, showView, signIn } from './fixtures';
import { watchPage, type Member, type VoiceWorld, type Watch } from './voice-world';

export interface Someone {
  readonly page: Page;
  readonly context: BrowserContext;
  readonly watch: Watch;
  readonly member: Member;
  // leave(p) under the name a person's own: closes the page, then the context.
  readonly close: () => Promise<void>;
}

export interface EnterOptions {
  // The context's own options (a phone's screen, say).
  readonly context?: BrowserContextOptions;
  // Runs on the new page before it loads, to hook it from the start.
  readonly before?: (page: Page) => void | Promise<void>;
  // 'push to talk' (the default) is a browser that chose it before it joined
  // (issue #1244); 'unchosen' is a fresh browser, which joins in the open
  // microphone.
  readonly mode?: 'push to talk' | 'unchosen';
}

// Installed before the page's own scripts: keeps every peer connection the SDK
// makes, for getStats (inbound reads them).
const keepConnections = () => {
  const w = window as unknown as { __pcs?: RTCPeerConnection[] };
  if (w.__pcs !== undefined) return;
  w.__pcs = [];
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    (w.__pcs ?? []).push(pc);
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
};

// enter makes one person: a context of its own (never a tab of another's), the
// page watched from its first request, member signed in, and vw's channel
// open. Close it with leave, or its close, whatever the test finds.
export async function enter(browser: Browser, vw: VoiceWorld, member: Member, options: EnterOptions = {}): Promise<Someone> {
  const context = await browser.newContext(options.context);
  try {
    if ((options.mode ?? 'push to talk') === 'push to talk') await rememberPushToTalk(context);
    const page = await context.newPage();
    await options.before?.(page);
    const watch = await watchPage(page, vw);
    await page.addInitScript(keepConnections);
    await signIn(page, vw, member.token, member.name);
    await openChannel(page, vw.channel);
    const someone: Someone = { page, context, watch, member, close: () => leave(someone) };
    return someone;
  } catch (err) {
    await context.close();
    throw err;
  }
}

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

// joinVoice joins the channel's voice, with the microphone ('Join voice') or
// without ('Watch only'), and waits for the connection.
export async function joinVoice(p: Someone, how: 'Join voice' | 'Watch only'): Promise<void> {
  await showView(p.page, 'Voice');
  // Watch only beside the person's voice in another client is the viewer
  // (issue #936); presence lingers a few seconds after an earlier page of the
  // same member closed and reads as that other client, so wait for it to clear.
  if (how === 'Watch only') await expect(voiceRegion(p.page)).not.toContainText('You are in voice on another device', { timeout: 30_000 });
  await voiceRegion(p.page).getByRole('button', { name: how }).click();
  await expect(voiceRegion(p.page)).toContainText(/Connected in #/, { timeout: 30_000 });
}

// leave closes the person's page and then their context, as a person closes a
// tab: the page's pagehide ends its voice session (issue #802). Safe to call
// twice.
export async function leave(p: Pick<Someone, 'page' | 'context'>): Promise<void> {
  if (!p.page.isClosed()) await p.page.close();
  await p.context.close();
}

export interface Inbound {
  // Video frames decoded, and the width of the latest video frame.
  readonly frames: number;
  readonly width: number;
  // Audio bytes received.
  readonly audioBytes: number;
  // With perTrack: one entry per received stream, in the order the browser
  // lists them; the counters above are the sums of these.
  readonly tracks?: readonly { readonly kind: string; readonly id: string; readonly frames: number; readonly width: number; readonly bytes: number }[];
}

// inbound sums what the page has received, from the peer connections the SDK
// made. A connection that has closed (a room renewed, issue #516) has no
// stats to give and counts for nothing.
export async function inbound(page: Page, options: { perTrack?: boolean } = {}): Promise<Inbound> {
  return page.evaluate(async (perTrack) => {
    const w = window as unknown as { __pcs?: RTCPeerConnection[] };
    let frames = 0;
    let width = 0;
    let audioBytes = 0;
    const tracks: { kind: string; id: string; frames: number; width: number; bytes: number }[] = [];
    for (const pc of w.__pcs ?? []) {
      if (pc.signalingState === 'closed') continue;
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; id: string; kind?: string; framesDecoded?: number; frameWidth?: number; bytesReceived?: number }) => {
        if (r.type !== 'inbound-rtp') return;
        const entry = { kind: r.kind ?? '?', id: r.id, frames: r.framesDecoded ?? 0, width: r.frameWidth ?? 0, bytes: r.bytesReceived ?? 0 };
        if (r.kind === 'video') {
          frames += entry.frames;
          width = Math.max(width, entry.width);
        } else if (r.kind === 'audio') {
          audioBytes += entry.bytes;
        }
        tracks.push(entry);
      });
    }
    return perTrack ? { frames, width, audioBytes, tracks } : { frames, width, audioBytes };
  }, options.perTrack === true);
}

// expectClean checks that no secret of the world, and no join token or room
// name the page was given, is in anything the page said or kept: its console,
// its DOM and address, its storage and cookies, and its resource timing
// entries; that no policy violation is in the console; and that the browser
// reports nothing wrong that the person never sees (a Permissions-Policy
// report, a failed request to conchd, issue #595).
//
// The one secret a page keeps by design is its own credential, under
// conch.web.token: a member's in localStorage (issue #439), an operator's in
// sessionStorage. Each must be exactly the member's, and the rest of storage
// is checked without it. The one named exception: on a failed LiveKit
// connection, livekit-client sends the join token in the access_token query of
// its /rtc/v1/validate request and the resource timing records that URL
// (docs/manual.md, "Voice in the browser"); that parameter, in a URL on
// LiveKit's origin with exactly that path, is left out of the timing entries.
export async function expectClean(vw: VoiceWorld, p: Pick<Someone, 'page' | 'watch' | 'member'>): Promise<void> {
  await p.watch.settled();
  const livekit = new URL(vw.livekitOrigin.replace(/^ws/, 'http')).origin;
  const { kept, credentials } = await p.page.evaluate((livekitHttp) => {
    const timingName = (name: string) => {
      try {
        const url = new URL(name);
        if (url.origin === livekitHttp && url.pathname === '/rtc/v1/validate') url.searchParams.delete('access_token');
        return url.href;
      } catch {
        return name;
      }
    };
    const without = (storage: Storage) => {
      const rest: Record<string, string | null> = {};
      for (let i = 0; i < storage.length; i++) {
        const key = storage.key(i);
        if (key !== null && key !== 'conch.web.token') rest[key] = storage.getItem(key);
      }
      return JSON.stringify(rest);
    };
    return {
      credentials: [sessionStorage.getItem('conch.web.token'), localStorage.getItem('conch.web.token')],
      kept: [
        document.documentElement.outerHTML,
        location.href,
        without(sessionStorage),
        without(localStorage),
        document.cookie,
        JSON.stringify(performance.getEntries().map((e) => timingName(e.name))),
      ].join('\n'),
    };
  }, livekit);
  for (const credential of credentials) {
    expect(credential === null || credential === p.member.token, "the stored credential is the member's own").toBe(true);
  }
  const said = p.watch.console.join('\n');
  for (const s of vw.secrets) {
    expect(kept, 'a secret in the page, its storage or its cookies').not.toContain(s);
    expect(said, 'a secret in the console').not.toContain(s);
  }
  expect(p.watch.console.filter((l) => /Content Security Policy|Refused to|[Pp]ermissions policy/.test(l))).toEqual([]);
  expect(p.watch.policy, 'Permissions-Policy reports').toEqual([]);
  expect(p.watch.failed, 'requests to conchd that failed').toEqual([]);
}
