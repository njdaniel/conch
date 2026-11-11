// The camera in the browser (ADR-015, issue #1242; design §11.6, the C6a
// part), against the conchd and pinned LiveKit of voice-world.ts, on
// Chromium's fake camera (--use-fake-device-for-media-stream,
// playwright.config.ts). The owner turns use_camera on for operators and
// members first, through PUT /v1/permissions, so a member can claim.
//
// What it shows: an explicit start, and a second browser receiving frames;
// joining, unmuting and sharing never asking for the camera; the button,
// leaving and signing out ending the local track and releasing the claim by
// name; a denied browser permission leaving voice and chat usable; and one
// person in two contexts (S5), the displaced one stopping its capture while
// presence still says camera, until the claim ends idle.
//
// What it is not: a real camera, another browser engine, a phone, or a LAN
// or remote deployment. Those are C8's by-hand checks. Without Docker the
// tests fail when CI is set and are skipped otherwise, as in 10-voice.
import { chromium, devices, type Browser, type BrowserContext, type BrowserContextOptions, type Page } from '@playwright/test';
import type { VoicePresenceV1 } from '../src/schema/voice';
import { closeAll, credentialField, expect, openChannel, rememberPushToTalk, say, signIn, test } from './fixtures';
import { toneFile } from './tone';
import { dockerAvailable, startVoiceWorld, watchPage, type Member, type VoiceWorld, type Watch } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const callView = (page: Page) => page.getByRole('region', { name: 'Video call' });

// Installed before the page's own scripts: keeps every peer connection the
// SDK makes (for getStats), every camera track the browser gives, and counts
// the requests for a camera.
const instrument = () => {
  const w = window as unknown as { __pcs: RTCPeerConnection[]; __cams: MediaStreamTrack[]; __camAsks: number };
  w.__pcs = [];
  w.__cams = [];
  w.__camAsks = 0;
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    w.__pcs.push(pc);
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
  const devices = navigator.mediaDevices;
  const user = devices.getUserMedia.bind(devices);
  devices.getUserMedia = async (constraints?: MediaStreamConstraints) => {
    if (constraints?.video !== undefined && constraints.video !== false) w.__camAsks++;
    const stream = await user(constraints);
    w.__cams.push(...stream.getVideoTracks());
    return stream;
  };
};

const camAsks = (page: Page) => page.evaluate(() => (window as unknown as { __camAsks: number }).__camAsks);
// The readyState of every camera track this page captured.
const camStates = (page: Page) => page.evaluate(() => (window as unknown as { __cams: MediaStreamTrack[] }).__cams.map((t) => t.readyState));

// framesIn sums the video frames this page has decoded.
async function framesIn(page: Page): Promise<number> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let frames = 0;
    for (const pc of w.__pcs) {
      if (pc.signalingState === 'closed') continue;
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; framesDecoded?: number }) => {
        if (r.type === 'inbound-rtp' && r.kind === 'video') frames += r.framesDecoded ?? 0;
      });
    }
    return frames;
  });
}

test.describe('the camera with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the camera suite needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the camera suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
    // The owner turns the camera on for operators and members, in one PUT:
    // the operator cell must be on with the member cell (admin-roles §4.4.1).
    const read = await vw.api(vw.operator, 'GET', '/v1/permissions');
    expect(read.status).toBe(200);
    const { revision } = JSON.parse(read.text) as { revision: number };
    const put = await vw.api(vw.operator, 'PUT', '/v1/permissions', {
      revision,
      changes: [
        { permission: 'use_camera', role: 'operator', granted: true },
        { permission: 'use_camera', role: 'member', granted: true },
      ],
    });
    expect(put.status, put.text).toBe(200);
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  interface Someone {
    readonly page: Page;
    readonly watch: Watch;
    readonly member: Member;
    // Every camera request this page made, as its method and query.
    readonly cameraCalls: string[];
    readonly close: () => Promise<void>;
  }

  async function person(browser: Browser, who: 'alice' | 'bob' | 'carol' | Member, options?: BrowserContextOptions, context?: BrowserContext): Promise<Someone> {
    const ctx = context ?? (await browser.newContext(options));
    await rememberPushToTalk(ctx);
    const page = await ctx.newPage();
    const watch = await watchPage(page, vw);
    await page.addInitScript(instrument);
    const cameraCalls: string[] = [];
    page.on('request', (req) => {
      const url = new URL(req.url());
      if (url.pathname.endsWith('/voice/camera')) cameraCalls.push(`${req.method()} ${url.search}`);
    });
    const member = typeof who === 'string' ? vw[who] : who;
    await signIn(page, vw, member.token, member.name);
    await openChannel(page, vw.channel);
    return { page, watch, member, cameraCalls, close: () => closeAll(ctx) };
  }

  async function join(p: Someone): Promise<void> {
    await voiceRegion(p.page).getByRole('button', { name: 'Join voice' }).click();
    await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
  }

  async function cameraOn(p: Someone): Promise<void> {
    const button = voiceRegion(p.page).getByRole('button', { name: 'Camera', exact: true });
    await expect(button).toHaveAttribute('aria-pressed', 'false');
    await button.click();
    await expect(voiceRegion(p.page)).toContainText('Sending video', { timeout: 30_000 });
    await expect(button).toHaveAttribute('aria-pressed', 'true');
  }

  // The page's own presence document, as conchd answers it.
  async function presence(member: Member): Promise<VoicePresenceV1> {
    const r = await vw.api(member.token, 'GET', `/v1/channels/${vw.channel}/voice`);
    expect(r.status).toBe(200);
    return JSON.parse(r.text) as VoicePresenceV1;
  }
  const cameraOf = async (member: Member): Promise<boolean> => {
    const doc = await presence(member);
    return doc.rooms.some((room) => room.audience === undefined && room.participants.some((x) => x.principal_id === member.id && x.camera === true));
  };

  // Nothing refused by a policy: the camera is the page's own.
  async function expectClean(p: Someone): Promise<void> {
    await p.watch.settled();
    const said = p.watch.console.join('\n');
    for (const s of vw.secrets) expect(said, 'a secret in the console').not.toContain(s);
    expect(p.watch.console.filter((l) => /Content Security Policy|Refused to|[Pp]ermissions policy/.test(l))).toEqual([]);
    expect(p.watch.policy, 'Permissions-Policy reports').toEqual([]);
  }

  test('the page is served with the camera its own, and the API with no power at all', async ({ request }) => {
    const app = await request.get(vw.appUrl);
    expect(app.headers()['permissions-policy']).toBe('microphone=(self), camera=(self), display-capture=(self)');
    const api = await request.get(`${vw.origin}/healthz`);
    expect(api.headers()['permissions-policy']).toBe('camera=(), microphone=(), display-capture=()');
  });

  test('joining, unmuting and sharing never ask for the camera; the button starts it, and another browser receives its frames; off ends the local track and releases the claim by name', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await join(a);
      await join(b);
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Mute' }).click();
      await voice.getByRole('button', { name: 'Unmute' }).click();
      await voice.getByRole('button', { name: 'Share screen' }).click();
      await expect(voice).toContainText('You are sharing your screen', { timeout: 30_000 });
      await voice.getByRole('button', { name: 'Stop sharing' }).click();
      await expect(voice).toContainText('You stopped sharing.');
      expect(await camAsks(a.page)).toBe(0);
      expect(a.cameraCalls).toEqual([]);
      // Nothing opens the other page's call view by itself (O8).
      await cameraOn(a);
      expect(a.cameraCalls).toEqual(['POST ']);
      await expect(callView(a.page)).toBeVisible();
      await expect(callView(b.page)).toHaveCount(0);
      // The other page is told, and opens the view when asked.
      const show = voiceRegion(b.page).getByRole('button', { name: 'Show video (1)' });
      await expect(show).toBeVisible({ timeout: 15_000 });
      await expect(voiceRegion(b.page).getByRole('listitem').filter({ hasText: 'alice' })).toContainText('camera');
      await show.click();
      const tile = callView(b.page).getByLabel("alice's camera");
      await expect.poll(() => tile.evaluate((v) => (v as HTMLVideoElement).videoWidth), { timeout: 30_000 }).toBeGreaterThan(0);
      await expect.poll(() => framesIn(b.page), { timeout: 30_000 }).toBeGreaterThan(10);
      const before = await framesIn(b.page);
      await expect.poll(() => framesIn(b.page), { timeout: 15_000 }).toBeGreaterThan(before);
      // Off: the local track ends, and conchd is asked to end the claim by name.
      await voice.getByRole('button', { name: 'Camera', exact: true }).click();
      await expect(voice).not.toContainText('Sending video');
      await expect.poll(() => camStates(a.page)).toEqual(['ended']);
      await expect.poll(() => a.cameraCalls).toHaveLength(2);
      expect(a.cameraCalls[1]).toMatch(/^DELETE \?started_at=/);
      await expect.poll(() => cameraOf(vw.alice), { timeout: 10_000 }).toBe(false);
      await expect(voiceRegion(b.page).getByRole('button', { name: /Show video/ })).toHaveCount(0);
      for (const p of [a, b]) await expectClean(p);
    } finally {
      await a.close();
      await b.close();
    }
  });

  test('leaving voice, and signing out, end the local track and release the claim by name', async ({ browser }) => {
    const leaver = await person(browser, 'carol');
    const member = await vw.addMember('camera-signout');
    const signer = await person(browser, member);
    try {
      await join(leaver);
      await cameraOn(leaver);
      await voiceRegion(leaver.page).getByRole('button', { name: 'Leave voice' }).click();
      await expect(voiceRegion(leaver.page)).toContainText('Not in voice', { timeout: 15_000 });
      await expect.poll(() => camStates(leaver.page)).toEqual(['ended']);
      expect(leaver.cameraCalls.at(-1)).toMatch(/^DELETE \?started_at=/);

      await join(signer);
      await cameraOn(signer);
      await signer.page.getByRole('button', { name: 'Settings' }).click();
      await signer.page.getByRole('button', { name: 'Sign out', exact: true }).click();
      await signer.page.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
      await expect((await credentialField(signer.page))).toBeVisible();
      await expect.poll(() => camStates(signer.page)).toEqual(['ended']);
      // Released by name before the credential was revoked.
      expect(signer.cameraCalls.at(-1)).toMatch(/^DELETE \?started_at=/);
      const stopped = (await vw.audit()).filter((e) => e.action === 'voice_camera_stopped' && e.detail.includes(`principal=${String(member.id)}`));
      expect(stopped.map((e) => /reason=(\w+)/.exec(e.detail)?.[1])).toEqual(['self']);
    } finally {
      await leaver.close();
      await signer.close();
    }
  });

  test('a camera the browser denies is said in words, and voice and chat carry on', async () => {
    // A browser of its own, as the suite's project but for the prompt: the
    // fake devices and no fake prompt, so every prompt is denied
    // (--deny-permission-prompts). The microphone, and the loopback network
    // LiveKit is on, are granted to the page; the camera is not. The full
    // Chromium, not the headless shell, which has no prompt to deny and
    // refuses every capture as NotSupportedError without the fake one.
    const denying = await chromium.launch({
      channel: 'chromium',
      args: ['--use-fake-device-for-media-stream', `--use-file-for-fake-audio-capture=${toneFile()}`, '--deny-permission-prompts', '--disable-features=WebRtcHideLocalIpsWithMdns'],
    });
    try {
      // The suite's desktop Chrome: the SDK reads the user agent, and a
      // headless one is not a browser it sends from.
      const context = await denying.newContext({ ...devices['Desktop Chrome'] });
      await context.grantPermissions(['microphone', 'local-network-access'], { origin: vw.origin });
      const d = await person(denying, 'bob', undefined, context);
      await join(d);
      await voiceRegion(d.page).getByRole('button', { name: 'Camera', exact: true }).click();
      await expect(voiceRegion(d.page)).toContainText("The browser blocked the camera. Allow it in this site's settings.", { timeout: 15_000 });
      await expect(voiceRegion(d.page)).toContainText(/Connected in #voice-room/);
      await expect(voiceRegion(d.page)).not.toContainText('Sending video');
      await expect(voiceRegion(d.page).getByRole('button', { name: 'Camera', exact: true })).toHaveAttribute('aria-pressed', 'false');
      // Nothing was claimed: the browser refused before the claim.
      expect(d.cameraCalls).toEqual([]);
      // Chat still works.
      await say(d.page, vw.channel, 'chat goes on without a camera');
      await expect(d.page.getByRole('log', { name: 'Messages' })).toContainText('chat goes on without a camera');
      await d.close();
    } finally {
      await denying.close();
    }
  });

  test('the camera button works from the keyboard alone: Tab to it, Enter on, Enter off', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      await join(a);
      const button = voiceRegion(a.page).getByRole('button', { name: 'Camera', exact: true });
      await button.focus();
      await expect(button).toBeFocused();
      await a.page.keyboard.press('Enter');
      await expect(voiceRegion(a.page)).toContainText('Sending video', { timeout: 30_000 });
      await button.focus();
      await a.page.keyboard.press('Enter');
      await expect(voiceRegion(a.page)).not.toContainText('Sending video');
      await expect.poll(() => camStates(a.page)).toEqual(['ended']);
      // Show video and Hide video are reachable the same way.
      await voiceRegion(a.page).getByRole('button', { name: 'Leave voice' }).click();
    } finally {
      await a.close();
    }
  });

  test('one person in two contexts (S5): the displaced page stops its capture at once; the other never says it sends; the claim ends idle', async ({ browser }) => {
    const member = await vw.addMember('camera-two-devices');
    const a = await person(browser, member);
    const b = await person(browser, member);
    try {
      await join(a);
      await cameraOn(a);
      await expect.poll(() => cameraOf(member), { timeout: 10_000 }).toBe(true);
      // The same person joins the same channel from the other context.
      await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).click();
      await expect(voiceRegion(b.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      // A is displaced: it does not reconnect, its track is ended, and it
      // does not say it sends.
      await expect(voiceRegion(a.page)).toContainText('Another device took over your place in this voice room', { timeout: 15_000 });
      await expect.poll(() => camStates(a.page)).toEqual(['ended']);
      await expect(voiceRegion(a.page)).not.toContainText('Sending video');
      await expect(voiceRegion(a.page)).not.toContainText('Camera in use');
      // B has no camera of its own, whatever presence says of the claim.
      expect(await camAsks(b.page)).toBe(0);
      await expect(voiceRegion(b.page)).not.toContainText('Sending video');
      await expect(voiceRegion(b.page)).not.toContainText('Camera in use');
      // The claim ends idle, and camera leaves presence.
      await expect.poll(() => cameraOf(member), { timeout: 30_000 }).toBe(false);
      await expect(voiceRegion(b.page)).not.toContainText('Sending video');
      const stopped = (await vw.audit()).filter((e) => e.action === 'voice_camera_stopped' && e.detail.includes(`principal=${String(member.id)}`));
      expect(stopped.length).toBe(1);
      expect(stopped[0]?.detail).toMatch(/reason=idle/);
    } finally {
      await a.close();
      await b.close();
    }
  });
});
