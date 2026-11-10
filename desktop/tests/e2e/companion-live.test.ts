// The screen companion, live (issue #1583, D4-06 of #772): main's own
// companion service (desktop/main/voice/companion.ts) hands a grant to the web
// client's companion mode (web/src/voice/connection.ts, #1581) running in a
// Chromium page, beside the desktop's VoiceService with the real conch-voice
// started as the desktop starts it (--share-audio-owner companion), against the
// real conchd and the pinned LiveKit. V is a second principal in a second
// Chromium, the web client itself signed in at conchd's /app/. e2e/desktop-voice
// (make desktop-voice-e2e) is the world and gives this its facts.
//
// What is real: the helper, the companion service and the share allowance
// (ShareAllowance, spent by the Share click's arm, the grant and the capture),
// the voice client's companion mode, the LiveKit SDK in two browsers, conchd's
// grant, claim and presence, LiveKit's own list of who is in the room.
//
// What stands in, and why it reaches no release: there is no ScreenCast portal
// and no Electron here. The screen is Chromium's fake screen, given by launch
// flags of the Chromium THIS TEST starts (--use-fake-ui-for-media-stream,
// --auto-select-desktop-capture-source); the bridge the page would find in the
// desktop shell (window.conchDesktop) is a handful of functions this process
// exposes to the page (page/main.ts), which call the real service. The page is
// a separate Vite entry (tests/e2e/page) that nothing under web/ or
// desktop/main imports and no build of the app includes, so no flag, variable
// or hook was added to any code a release ships: the packaged app is unchanged
// and its allowance, request filter and portal checks are exactly as before.
// What this does not prove: the portal, Electron's own display-capture
// handler, real audio devices, or LAN behaviour (#1584, D4-07).
//
// Not observable here, and said so rather than faked: A's helper runs as the
// desktop runs it (`conch-voice managed`), which writes no stats lines (those
// are `join --json`'s), so what its counting sink heard cannot be read.
// The companion half of "no voice" is read from the companion's room (the
// publications it holds and whether each is subscribed) and from LiveKit's
// list; the helper half (it plays B's voice once and no share sound under
// --share-audio-owner companion) is the Rust suite's (#934).
import { mkdtempSync, rmSync, readFileSync, existsSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { extname, join } from 'node:path';

import { chromium, type Browser, type BrowserContext, type Page } from '@playwright/test';
import { build } from 'vite';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

import type { CompanionEvent } from '../../../web/src/platform/desktop/bridge.ts';
import { voicePresenceV1, voiceShareV1, voiceShareV2, type VoicePresenceV1 } from '../../../web/src/schema/voice.ts';
import { fromServer } from '../../../web/src/schema/validate.ts';
import { occupants } from '../../../web/src/voice/presence.ts';
import { Client } from '../../main/api/client.ts';
import { targetOf } from '../../main/api/target.ts';
import { Session } from '../../main/api/session.ts';
import { Companion } from '../../main/voice/companion.ts';
import { ShareAllowance } from '../../main/share-allowance.ts';
import { absent, absentSoon, closeOwners, eventually, inRoom, note, onlyOnce, owner, sleep, world, type Owner, type Who } from './support.ts';

// ---------------------------------------------------------------- the world

function needV(name: string): string {
  const v = process.env[name];
  if (v === undefined || v === '') throw new Error(`${name} is not set: this test is the owner side of e2e/desktop-voice; run make desktop-voice-e2e`);
  return v;
}

const v: Who = { id: Number(needV('CONCH_E2E_V_ID')), token: needV('CONCH_E2E_V_TOKEN'), tone: 0 };
const appUrl = needV('CONCH_E2E_APP_URL');
const VIEWER_NAME = 'view-v';
const A_NAME = 'desk-a';

// voiceCompanionGrace of internal/server/voice_presence.go: one pass gap
// (voicePassGapMax, 1.5 s) and the time a removal may take
// (voiceEvictTimeout, 4 s). A companion whose person has gone is removed by
// LiveKit within it even if nobody sends the DELETE.
const COMPANION_GRACE_MS = 5_500;

const CHROMIUM_ARGS = [
  '--use-fake-ui-for-media-stream',
  '--use-fake-device-for-media-stream',
  '--auto-select-desktop-capture-source=Entire screen',
  '--disable-features=WebRtcHideLocalIpsWithMdns',
];

interface PageFacts {
  readonly phase: string;
  readonly companion: boolean;
  readonly share: string;
  readonly watched: number[];
  readonly why: string | null;
  readonly captures: number;
  readonly rooms: number;
  readonly roomState: string | null;
  readonly publications: { who: string; local: boolean; source: string; kind: string; subscribed: boolean }[];
}

// ---------------------------------------------------------------- the pages

let tempDir = '';
let pageServer: Server | null = null;
let pageUrl = '';
let browser: Browser | null = null;

const MIME: Readonly<Record<string, string>> = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8' };

beforeAll(async () => {
  tempDir = mkdtempSync(join(tmpdir(), 'companion-page-'));
  // The companion's page: web/src/voice in companion mode, built as a Vite
  // entry of this directory (see page/main.ts).
  await build({
    root: join(import.meta.dirname, 'page'),
    configFile: false,
    logLevel: 'error',
    base: './',
    // One copy of the SDK: the page keeps the rooms that web/src/voice makes,
    // so both must import the same module (web's own install of it).
    resolve: { alias: { 'livekit-client': join(import.meta.dirname, '..', '..', '..', 'web', 'node_modules', 'livekit-client') } },
    build: { outDir: join(tempDir, 'page'), emptyOutDir: true, minify: false, sourcemap: false, target: 'es2022' },
  });
  const root = join(tempDir, 'page');
  pageServer = createServer((req, res) => {
    const name = (req.url ?? '/').split('?')[0] ?? '/';
    const file = join(root, name === '/' ? 'index.html' : name.replace(/\.\.+/g, ''));
    if (!file.startsWith(root) || !existsSync(file)) {
      res.writeHead(404).end();
      return;
    }
    res.writeHead(200, { 'content-type': MIME[extname(file)] ?? 'application/octet-stream' }).end(readFileSync(file));
  });
  await new Promise<void>((resolve) => pageServer?.listen(0, '127.0.0.1', resolve));
  pageUrl = `http://127.0.0.1:${String((pageServer.address() as AddressInfo).port)}/`;
  if (!existsSync(chromium.executablePath())) {
    throw new Error('the browser Playwright pins is not installed; run: cd desktop && npx playwright install chromium');
  }
  browser = await chromium.launch({ args: CHROMIUM_ARGS });
}, 120_000);

afterAll(async () => {
  await browser?.close();
  await new Promise<void>((resolve) => {
    if (pageServer === null) resolve();
    else pageServer.close(() => {
      resolve();
    });
  });
  if (tempDir !== '') rmSync(tempDir, { recursive: true, force: true });
});

// -------------------------------------------------------------- the desktop

// Desk is what main holds for A: its API client (the bearer stays here), the
// share allowance, the companion service over A's VoiceService, and the page
// that stands for the owned window's document.
class Desk {
  readonly logs: string[] = [];
  readonly grants: string[] = [];
  readonly ends: string[] = [];
  readonly claims: string[] = [];
  readonly consoleLines: string[] = [];
  readonly contents: object = {};
  readonly allowance = new ShareAllowance({ now: Date.now, generation: () => 1 });
  readonly client: Client;
  readonly companion: Companion;
  context: BrowserContext | null = null;
  page: Page | null = null;

  constructor(readonly owner: Owner) {
    const session = new Session();
    session.setOrigin(world.origin);
    session.adopt(world.a.token);
    this.client = new Client({ fetch: globalThis.fetch, session });
    this.companion = new Companion({
      voice: owner.service,
      locked: () => false,
      client: () => this.client,
      document: () => 1,
      tell: (_document: number, event: CompanionEvent) => {
        this.ends.push(event.reason);
        // The page may be closed by the time the owner is torn down.
        void this.page
          ?.evaluate((reason) => {
            (window as unknown as { companionPage: { mainEnded: (r: string) => void } }).companionPage.mainEnded(reason);
          }, event.reason)
          .catch(() => undefined);
      },
      log: (line) => this.logs.push(line),
      contents: () => this.contents,
      granted: (contents, document) => {
        this.allowance.confirmGrant(contents, document);
      },
      ended: () => {
        this.allowance.clear();
      },
    });
  }

  get p(): Page {
    if (this.page === null) throw new Error('the companion page is not open');
    return this.page;
  }

  // open starts the page for the owned window's document and gives it the
  // bridge: the same four calls the preload would (grant, arm, end, and the
  // capture check main's display-capture handler makes), over the real
  // service and allowance.
  async open(): Promise<void> {
    if (browser === null) throw new Error('no browser');
    this.context = await browser.newContext();
    const page = await this.context.newPage();
    this.page = page;
    page.on('console', (m) => this.consoleLines.push(m.text()));
    page.on('pageerror', (e) => this.consoleLines.push(e.message));
    await page.exposeFunction('bridgeGrant', async (channel: string) => {
      // Main's companionGrant: only on the Share click's allowance (ipc.ts).
      if (!this.allowance.takeGrant(this.contents)) return { ok: false, error: 'failed' };
      const answer = await this.companion.grant({ channel });
      if (answer.ok) this.grants.push(answer.grant.token);
      return answer;
    });
    await page.exposeFunction('bridgeArm', () => this.allowance.arm(this.contents));
    await page.exposeFunction('bridgeEnd', () => this.companion.stop());
    await page.exposeFunction('bridgeCapture', () => this.allowance.takeCapture(this.contents));
    // The claim and its release. The answer to a claim is a document the page
    // reads (VoiceShareV1 or V2), but main's table of operations
    // (web/src/platform/operations.ts) gives claimShare no response validator,
    // and Client.send hands back no body for such an operation: through the
    // desktop's own http bridge the page would be told 200 with nothing, and
    // its claim would read as invalid. That is a gap in main, outside this
    // test's issue: it is #1638, fixed by PR #1639. Until #1639 merges, these
    // two go to conchd with A's bearer directly, at the path of the same
    // table, so the rest is exercised. Switch to the real bridge once #1639
    // merges.
    await page.exposeFunction('bridgeClaim', async (channel: string, quality: { resolution: string; frameRate: number } | null) => {
      const path = targetOf('claimShare', { channel }, undefined);
      if (path === null) return { ok: false, failure: 'invalid' };
      const body = quality === null ? undefined : { quality: { resolution: quality.resolution, frame_rate: quality.frameRate } };
      const res = await fetch(world.origin + path, {
        method: 'POST',
        headers: { authorization: `Bearer ${world.a.token}`, ...(body === undefined ? {} : { 'content-type': 'application/json' }) },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const text = await res.text();
      if (!res.ok) {
        this.claims.push(`claim refused: ${String(res.status)} ${text.slice(0, 200)}`);
        return { ok: false, failure: 'invalid', status: res.status };
      }
      const parsed: unknown = JSON.parse(text);
      const problems = fromServer(quality === null ? voiceShareV1 : voiceShareV2)(parsed);
      if (problems.length > 0) this.claims.push(`claim answer invalid: ${JSON.stringify(problems)}`);
      return problems.length === 0 ? { ok: true, value: parsed } : { ok: false, failure: 'invalid' };
    });
    await page.exposeFunction('bridgeRelease', async (channel: string, startedAt: string) => {
      const path = targetOf('stopShare', { channel }, `started_at=${encodeURIComponent(startedAt)}`);
      if (path !== null) await fetch(world.origin + path, { method: 'DELETE', headers: { authorization: `Bearer ${world.a.token}` } });
    });
    await page.addInitScript(`window.__bridge = {
      grant: (c) => window.bridgeGrant(c),
      arm: () => window.bridgeArm(),
      end: () => window.bridgeEnd(),
      capture: () => window.bridgeCapture(),
      claim: (c, q) => window.bridgeClaim(c, q),
      release: (c, s) => window.bridgeRelease(c, s),
    };`);
    await page.goto(pageUrl);
    await page.waitForFunction('typeof window.companionPage === "object"');
  }

  facts(): Promise<PageFacts> {
    return this.p.evaluate(() => (window as unknown as { companionPage: { facts: () => PageFacts } }).companionPage.facts());
  }

  // The Share button: its click arms the allowance, then the page joins the
  // companion, claims and publishes (connection.ts startCompanionShare).
  share(): Promise<void> {
    return this.p.evaluate((channel) => {
      (window as unknown as { companionPage: { share: (c: string) => void } }).companionPage.share(channel);
    }, world.channel);
  }

  watch(principal: number): Promise<void> {
    return this.p.evaluate(
      ([channel, who]) => {
        (window as unknown as { companionPage: { watch: (c: string, p: number) => void } }).companionPage.watch(channel as string, who as number);
      },
      [world.channel, principal],
    );
  }

  stopShare(): Promise<void> {
    return this.p.evaluate(() => {
      (window as unknown as { companionPage: { stopShare: () => void } }).companionPage.stopShare();
    });
  }

  // presence is what conchd lists for the channel's voice, read as A (the
  // REST snapshot is version 1; the socket is version 2).
  async presence(): Promise<VoicePresenceV1> {
    const path = targetOf('voicePresence', { channel: world.channel }, undefined);
    if (path === null) throw new Error('no path for voice presence');
    const res = await fetch(world.origin + path, { headers: { authorization: `Bearer ${world.a.token}` } });
    const body: unknown = await res.json();
    const problems = fromServer(voicePresenceV1)(body);
    if (!res.ok || problems.length > 0) throw new Error(`voice presence: ${String(res.status)} ${JSON.stringify(problems)} ${JSON.stringify(body).slice(0, 600)}`);
    return body as VoicePresenceV1;
  }

  async close(): Promise<void> {
    await this.context?.close().catch(() => undefined);
  }
}

const desks: Desk[] = [];
function desk(o: Owner): Desk {
  const made = new Desk(o);
  desks.push(made);
  return made;
}

// ------------------------------------------------------------- V, a browser

// V is the web client itself, signed in at conchd's /app/, with Chromium's
// fake microphone and fake screen (the flags above).
class Viewer {
  context: BrowserContext | null = null;
  page: Page | null = null;
  readonly consoleLines: string[] = [];

  get p(): Page {
    if (this.page === null) throw new Error('V has no page');
    return this.page;
  }

  private voice() {
    return this.p.getByRole('region', { name: 'Voice' });
  }

  async join(): Promise<void> {
    if (browser === null) throw new Error('no browser');
    this.context = await browser.newContext();
    const page = await this.context.newPage();
    this.page = page;
    page.on('console', (m) => this.consoleLines.push(m.text()));
    await page.addInitScript(() => {
      const w = window as unknown as { __pcs: RTCPeerConnection[] };
      w.__pcs = [];
      const Original = window.RTCPeerConnection;
      const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
        const pc = new Original(...args);
        w.__pcs.push(pc);
        return pc;
      } as unknown as typeof RTCPeerConnection;
      Wrapped.prototype = Original.prototype;
      Object.setPrototypeOf(Wrapped, Original);
      window.RTCPeerConnection = Wrapped;
    });
    await page.goto(appUrl);
    const disclosure = page.locator('details.operator-signin');
    await disclosure.waitFor({ state: 'attached' });
    if (!(await disclosure.evaluate((d) => (d as HTMLDetailsElement).open))) await disclosure.locator('summary').click();
    await page.getByLabel('Credential').fill(v.token);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await page.getByRole('status', { name: 'Status' }).filter({ hasText: VIEWER_NAME }).filter({ hasText: 'Connected' }).waitFor({ timeout: 30_000 });
    await page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name: world.channel, exact: true }).click();
    await this.voice().getByRole('button', { name: 'Join voice' }).click();
    await this.voice().filter({ hasText: new RegExp(`Connected in #${world.channel}`) }).waitFor({ timeout: 30_000 });
  }

  // share: V shares a fake screen with sound.
  async share(): Promise<void> {
    await this.voice().getByRole('button', { name: 'Share screen' }).click();
    await this.voice().filter({ hasText: 'You are sharing your screen with sound.' }).waitFor({ timeout: 30_000 });
  }

  // watch: V's Watch button for A's stream, and the stage shows A's name.
  async watch(): Promise<void> {
    await this.p.getByRole('button', { name: 'Watch', exact: true }).click({ timeout: 30_000 });
    await this.p.getByRole('region', { name: 'Shared screen' }).filter({ hasText: `${A_NAME} · LIVE` }).waitFor({ timeout: 30_000 });
  }

  // frames: video frames this page has decoded, summed over its connections.
  frames(): Promise<number> {
    return this.p.evaluate(async () => {
      const w = window as unknown as { __pcs: RTCPeerConnection[] };
      let frames = 0;
      for (const pc of w.__pcs) {
        if (pc.signalingState === 'closed') continue;
        (await pc.getStats()).forEach((r: { type: string; kind?: string; framesDecoded?: number }) => {
          if (r.type === 'inbound-rtp' && r.kind === 'video') frames += r.framesDecoded ?? 0;
        });
      }
      return frames;
    });
  }

  async close(): Promise<void> {
    await this.page?.close().catch(() => undefined);
    await this.context?.close().catch(() => undefined);
  }
}

const viewers: Viewer[] = [];
function viewer(): Viewer {
  const made = new Viewer();
  viewers.push(made);
  return made;
}

afterEach(async () => {
  // The companion first (its removal at conchd), then the pages, then the
  // owners (the helpers).
  for (const d of desks) await d.companion.stop().catch(() => false);
  for (const d of desks.splice(0)) await d.close();
  for (const w of viewers.splice(0)) await w.close();
  await closeOwners();
});

// ---------------------------------------------------------------- the facts

// listed: who LiveKit lists in the room, by identity, with their sources.
async function listed(identity: string): Promise<string[] | null> {
  const found = (await inRoom()).participants.find((p) => p.identity === identity);
  return found === undefined ? null : found.sources;
}
const SCREEN = /^(SCREEN_SHARE|3)$/;
const SCREEN_AUDIO = /^(SCREEN_SHARE_AUDIO|4)$/;

const idOf = (who: Who, suffix = ''): string => `p${String(who.id)}${suffix}`;

// noMicrophoneSubscribed: the companion's room holds no microphone of anyone
// subscribed and publishes none (client-side publication state).
function expectNoVoice(facts: PageFacts, where: string): void {
  const voice = facts.publications.filter((p) => p.source === 'microphone' || (p.kind === 'audio' && p.source === 'unknown'));
  expect(voice.filter((p) => p.local), `${where}: the companion published a microphone`).toEqual([]);
  expect(voice.filter((p) => !p.local && p.subscribed), `${where}: the companion subscribed to a microphone`).toEqual([]);
  expect(facts.publications.filter((p) => p.local && p.kind === 'audio'), `${where}: the companion published audio`).toEqual([]);
}

// The two refusals an unarmed or spent allowance gives. The grant's literal
// is this harness's own (bridgeGrant above), not main's IPC answer.
const REFUSED = { capture: 'NotAllowedError', grant: { ok: false, error: 'failed' } };

// askWithoutClick has the page ask for a screen and for a grant with no Share
// click behind either, and reports what it was told.
async function askWithoutClick(a: Desk): Promise<{ capture: string; grant: unknown }> {
  const capture = await a.p.evaluate(async () => {
    try {
      await navigator.mediaDevices.getDisplayMedia({ video: true });
      return 'captured';
    } catch (error) {
      return error instanceof DOMException ? error.name : 'error';
    }
  });
  const grant = await a.p.evaluate((channel) => (window as unknown as { __bridge: { grant: (c: string) => Promise<unknown> } }).__bridge.grant(channel), world.channel);
  return { capture, grant };
}

async function startShare(a: Desk): Promise<void> {
  if (a.page === null) await a.open();
  await a.share();
  try {
    await eventually(async () => (await a.facts()).share === 'sharing', 45_000, 'the companion to be sharing');
  } catch (error) {
    // What the page and main said, for finding why (no token is in any of it).
    const said = JSON.stringify({ facts: await a.facts(), main: a.logs, claims: a.claims, page: a.consoleLines.slice(-40) });
    throw new Error(`${error instanceof Error ? error.message : 'failed'}: ${said}`);
  }
}

// ------------------------------------------------------------------- tests

describe('the desktop screen companion with the real helper, conchd, LiveKit and a browser', () => {
  it('shares to a browser viewer, watches the viewer share, and carries no voice', async () => {
    const a = desk(owner({ name: 'desk-a', who: world.a }));
    await a.owner.join();
    await onlyOnce(world.a);

    const w = viewer();
    await w.join();

    // Share: the Share click arms main's allowance, the page asks main for
    // the grant, the grant names p<A>.screen, the page claims and publishes.
    await startShare(a);
    const facts = await a.facts();
    expect(facts.companion).toBe(true);
    expect(facts.captures, 'the picker was used once, on the allowance').toBe(1);
    expect(a.grants).toHaveLength(1);
    expect(a.companion.liveOrigin(), 'main holds the live grant').not.toBeNull();

    // The Share click's allowance is spent, not just unarmed: asking again
    // for a screen and for a grant, with no new click, is refused on both.
    expect(await askWithoutClick(a), 'the spent allowance refuses a second capture and a second grant').toEqual(REFUSED);
    expect(a.grants, 'no second grant was made').toHaveLength(1);
    expect((await a.facts()).captures, 'the picker was not used again').toBe(1);

    // LiveKit lists p<A> and p<A>.screen together; presence lists A once.
    await eventually(async () => (await listed(idOf(world.a, '.screen')))?.some((s) => SCREEN.test(s)) === true, 20_000, 'LiveKit to list p<A>.screen with a screen');
    expect(await listed(idOf(world.a)), 'p<A> still publishes only the microphone').toEqual(expect.arrayContaining([expect.stringMatching(/^(MICROPHONE|2)$/)]));
    expect((await listed(idOf(world.a)))?.some((s) => SCREEN.test(s))).toBe(false);
    const screenSources = (await listed(idOf(world.a, '.screen'))) ?? [];
    expect(screenSources.some((s) => SCREEN_AUDIO.test(s)), 'no ScreenShareAudio from A: the companion publishes video only').toBe(false);
    expect(screenSources.filter((s) => !SCREEN.test(s)), 'the companion publishes nothing but its screen').toEqual([]);
    const mine = occupants(await a.presence()).filter((o) => o.principal === world.a.id);
    expect(mine, 'presence lists A once').toHaveLength(1);
    expect(mine[0]?.sharing, 'presence says A is sharing').toBe(true);

    // V receives it as A's stream.
    await w.watch();
    await eventually(async () => (await w.frames()) > 10, 30_000, 'V to decode A\'s frames');
    const before = await w.frames();
    await eventually(async () => (await w.frames()) > before, 15_000, 'V to keep receiving frames');
    expectNoVoice(await a.facts(), 'while sharing');

    // Watch: V shares with sound; A's companion watches it.
    await w.share();
    // (The REST snapshot lists one share; V's is read from LiveKit's list.)
    await eventually(async () => ((await listed(idOf(v))) ?? []).some((s) => SCREEN_AUDIO.test(s)), 20_000, 'LiveKit to list V sharing with sound');
    expect(((await listed(idOf(v))) ?? []).some((s) => SCREEN.test(s)), 'V shares a screen').toBe(true);
    await a.watch(v.id);
    await eventually(
      async () => {
        const f = await a.facts();
        const mineTracks = f.publications.filter((p) => !p.local && p.who === idOf(v));
        return f.watched.includes(v.id) && mineTracks.some((p) => p.kind === 'video' && p.subscribed) && mineTracks.some((p) => p.source === 'screen_share_audio' && p.subscribed);
      },
      30_000,
      "the companion to subscribe to V's screen and its sound",
    );

    // No voice in the companion, throughout: it never subscribed to V's
    // microphone, and publishes no audio of its own. Read again after a pause,
    // for a late subscription.
    await sleep(3_000);
    const after = await a.facts();
    expectNoVoice(after, 'while watching');
    expect(after.rooms, 'the companion holds one room, so no second one hides a microphone subscription').toBe(1);
    const vmic = after.publications.filter((p) => !p.local && p.who === idOf(v) && p.source === 'microphone');
    expect(vmic, "V's microphone is in the room the companion sees").not.toEqual([]);
    expect(vmic.every((p) => !p.subscribed)).toBe(true);
    expect(after.publications.filter((p) => p.local).map((p) => p.source)).toEqual(['screen_share']);
    // V's voice is published once, by V's one participant.
    const vSources = (await listed(idOf(v))) ?? [];
    expect(vSources.filter((s) => /^(MICROPHONE|2)$/.test(s)), 'V publishes one microphone').toHaveLength(1);
    expect((await inRoom()).participants.filter((p) => p.identity === idOf(v))).toHaveLength(1);
    expect((await inRoom()).participants.filter((p) => p.identity === idOf(world.a))).toHaveLength(1);

    // No token appears in a line this test captured.
    const lines = [...a.logs, ...a.consoleLines, ...a.owner.logs.map((l) => l.line), ...w.consoleLines];
    for (const secret of [world.a.token, v.token, ...a.grants]) {
      expect(lines.filter((l) => l.includes(secret)), 'a token in a captured log or console line').toEqual([]);
    }
    note('share, watch and no voice held');
  });

  it('an unarmed allowance refuses a capture and a grant, and the page honours the refusal (the refusal is the harness\'s stand-in for main\'s IPC, not main\'s)', async () => {
    const a = desk(owner({ name: 'desk-a', who: world.a }));
    await a.owner.join();
    await a.open();
    // The page asks for a screen and for a grant with no click behind them
    // (no arm): the allowance gives neither, exactly as it does in the app.
    expect(await askWithoutClick(a)).toEqual(REFUSED);
    expect(a.grants).toEqual([]);
    expect(a.companion.liveOrigin()).toBeNull();
    expect((await a.facts()).captures).toBe(0);
    await sleep(1_000);
    expect(await listed(idOf(world.a, '.screen')), 'no companion was made').toBeNull();
  });

  it("the page's own end removes p<A>.screen and never p<A>; the person's Leave ends the room", async () => {
    const a = desk(owner({ name: 'desk-a', who: world.a }));
    await a.owner.join();
    await onlyOnce(world.a);
    const w = viewer();
    await w.join();

    await startShare(a);
    await eventually(async () => (await listed(idOf(world.a, '.screen'))) !== null, 20_000, 'p<A>.screen to be listed');

    // The page stops: the share ends, the companion has nothing to do, it
    // leaves and tells main (companionEnd), which sends the DELETE.
    await a.stopShare();
    await eventually(async () => (await listed(idOf(world.a, '.screen'))) === null, COMPANION_GRACE_MS + 5_000, 'p<A>.screen to be removed after the page ended it');
    expect(a.companion.liveOrigin(), 'main forgot the grant').toBeNull();
    expect(await listed(idOf(world.a)), 'the DELETE did not remove p<A>').not.toBeNull();
    expect(a.owner.view.helper?.phase).toBe('connected');
    expect(await absent(world.a)).toBe(false);

    // A new Share is a new act and a new grant; then Leave, as ipc.ts's
    // voiceLeave does it: the companion first, then the helper.
    await startShare(a);
    expect(a.grants).toHaveLength(2);
    await eventually(async () => (await listed(idOf(world.a, '.screen'))) !== null, 20_000, 'p<A>.screen to be listed again');
    const ended = a.companion.end('leave');
    await a.owner.service.leave();
    await ended;
    await eventually(async () => (await listed(idOf(world.a, '.screen'))) === null, 10_000, 'p<A>.screen to be removed by Leave');
    await eventually(async () => (await a.facts()).phase === 'stopped' || (await a.facts()).phase === 'idle', 10_000, "the companion's room to be left");
    expect((await a.facts()).roomState, "the companion's room disconnected").toBe('disconnected');
    expect(a.ends).toContain('leave');
    expect(await absentSoon(world.a), 'p<A> went with the helper').toBe(true);
  });

  it('the helper killed by its pid ends the companion, removed within the poller grace', async () => {
    const a = desk(owner({ name: 'desk-a', who: world.a }));
    await a.owner.join();
    await onlyOnce(world.a);
    const w = viewer();
    await w.join();
    await startShare(a);
    await eventually(async () => (await listed(idOf(world.a, '.screen'))) !== null, 20_000, 'p<A>.screen to be listed');

    const killed = Date.now();
    process.kill(a.owner.pid(), 'SIGKILL');
    await eventually(() => Promise.resolve(a.ends.length > 0), 15_000, 'main to end the companion');
    expect(a.ends).toEqual(['helper']);
    await eventually(async () => (await listed(idOf(world.a, '.screen'))) === null, COMPANION_GRACE_MS + 5_000, 'LiveKit to drop p<A>.screen');
    note(`helper killed to p<A>.screen gone took ${String(Date.now() - killed)} ms (grace ${String(COMPANION_GRACE_MS)} ms)`);
    expect(a.companion.liveOrigin()).toBeNull();
    await eventually(async () => (await a.facts()).roomState === 'disconnected', 10_000, "the companion's room to disconnect");
  });
});
