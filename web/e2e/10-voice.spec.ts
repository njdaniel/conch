// Browser voice (issue #442). Two people in two browser contexts, each on
// Chromium's fake microphone (a steady tone; see playwright.config.ts), against
// a conchd of its own configured for a LiveKit server from the pinned image
// (voice-world.ts). Without Docker the voice tests fail when CI is set and are
// skipped otherwise.
//
// The first test needs no LiveKit: on the shared server, which has none, the
// page says voice is not set up and asks for no device.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { channelButton, expect, expectFits, phones, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type VoiceWorld, type Watch } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

test('P3-F9c voice is not set up on a server without LiveKit, and the page asks for no microphone', async ({ meet, world }) => {
  const { page } = await meet();
  await page.addInitScript(() => {
    const w = window as unknown as { deviceRequested: boolean };
    w.deviceRequested = false;
    const refuse = () => {
      w.deviceRequested = true;
      return Promise.reject(new Error('device requested'));
    };
    Object.defineProperty(navigator, 'mediaDevices', {
      value: { getUserMedia: refuse, enumerateDevices: refuse, getDisplayMedia: refuse },
      configurable: true,
    });
  });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const voice = voiceRegion(page);
  await expect(voice).toContainText('Voice is not set up on this server');
  await expect(voice.getByRole('button')).toHaveCount(0);
  expect(await page.evaluate(() => (window as unknown as { deviceRequested: boolean }).deviceRequested)).toBe(false);
});

// Counts the audio bytes this page has received, from the peer connections the
// SDK made. The wrapper is installed before the page's own scripts run.
const countBytes = () => {
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
};

async function inboundAudioBytes(page: Page): Promise<number> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let total = 0;
    for (const pc of w.__pcs) {
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; bytesReceived?: number }) => {
        if (r.type === 'inbound-rtp' && r.kind === 'audio') total += r.bytesReceived ?? 0;
      });
    }
    return total;
  });
}

async function delta(page: Page, ms: number): Promise<number> {
  const before = await inboundAudioBytes(page);
  await new Promise((r) => setTimeout(r, ms));
  return (await inboundAudioBytes(page)) - before;
}

test.describe('with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  async function person(browser: import('@playwright/test').Browser, who: 'alice' | 'bob', options?: BrowserContextOptions) {
    const context = await browser.newContext(options);
    const page = await context.newPage();
    const watch = watchPage(page, vw);
    await page.addInitScript(countBytes);
    const m = vw[who];
    await signIn(page, vw, m.token, m.name);
    await channelButton(page, vw.channel).click();
    return { context, page, watch, member: m };
  }

  // No secret of the world, and no join token or room name the page was given,
  // in anything the page said or kept: its console, its DOM and address, its
  // storage and cookies, and (unless timing is false) its resource timing
  // entries. And no policy violation in the console.
  //
  // The one secret a page keeps by design is its own credential, under
  // conch.web.token: a member's in localStorage since A7 (issue #439), an
  // operator's in sessionStorage. Each must be exactly the signed-in member's,
  // and the rest of storage is checked without it.
  //
  // timing is false after LiveKit was down: on a failed connection the SDK
  // puts the join token in its /rtc/validate request, which the page's
  // resource timing records (README, "Voice in the browser").
  async function expectClean(p: { page: Page; watch: Watch; member: { token: string } }, timing = true): Promise<void> {
    await p.watch.settled();
    const { kept, credentials } = await p.page.evaluate((withTiming) => {
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
          withTiming ? JSON.stringify(performance.getEntries().map((e) => e.name)) : '',
        ].join('\n'),
      };
    }, timing);
    for (const credential of credentials) {
      expect(credential === null || credential === p.member.token, 'the stored credential is the member\'s own').toBe(true);
    }
    const said = p.watch.console.join('\n');
    for (const s of vw.secrets) {
      expect(kept, 'a secret in the page, its storage or its cookies').not.toContain(s);
      expect(said, 'a secret in the console').not.toContain(s);
    }
    expect(p.watch.console.filter((l) => /Content Security Policy|Refused to/.test(l))).toEqual([]);
  }

  test('a member joins, is heard only while transmitting, shows as talking, and is audited as a reported pair', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      await expect(voiceRegion(a.page).getByRole('button', { name: 'Join voice' })).toBeEnabled();
      await voiceRegion(a.page).getByRole('button', { name: 'Join voice' }).click();
      await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).click();
      for (const p of [a, b]) await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      // The roster of each page names both.
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('alice');
      await expect(voiceRegion(a.page).getByRole('list', { name: 'In voice' })).toContainText('bob');

      // Silent while the gate is shut.
      await a.page.getByRole('heading', { name: 'Conch' }).click();
      const quiet = await delta(b.page, 2000);

      // A holds the talk key; B's audio bytes rise, and the roster shows A talking.
      await a.page.keyboard.down('Backquote');
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('alice (transmitting)', { timeout: 15_000 });
      await expect(voiceRegion(a.page)).toContainText('Transmitting');
      const loud = await delta(b.page, 2000);
      await a.page.keyboard.up('Backquote');
      expect(loud, `bytes while A talks (${String(loud)}) versus quiet (${String(quiet)})`).toBeGreaterThan(quiet * 4 + 2000);
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).not.toContainText('alice (transmitting)', { timeout: 15_000 });
      const after = await delta(b.page, 2000);
      expect(after).toBeLessThan(loud / 4);

      // The audit has the reported pair for A and nothing unreported.
      await expect
        .poll(async () => (await vw.audit()).filter((e) => e.actor === `principal:${String(vw.alice.id)}` && e.action.startsWith('voice_transmit')).map((e) => e.action), { timeout: 15_000 })
        .toEqual(['voice_transmit_started', 'voice_transmit_stopped']);
      // Every join token and room name the pages were given is a secret from here.
      for (const p of [a, b]) await p.watch.settled();
      expect(vw.sessionSecrets(), 'join tokens and room names captured from the session answers').toBeGreaterThanOrEqual(3);
      const trail = await vw.audit();
      expect(trail.filter((e) => e.action === 'voice_transmit_unreported')).toEqual([]);
      for (const e of trail) for (const s of vw.secrets) expect(`${e.actor} ${e.subject} ${e.detail}`).not.toContain(s);

      // No policy violation, and nothing secret on the page, in its console,
      // its addresses, its storage or its cookies; each page's join token and
      // room name included.
      for (const p of [a, b]) await expectClean(p);
    } finally {
      await a.context.close();
      await b.context.close();
    }
  });

  test('push-to-talk by button, mute and deafen', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Connected', { timeout: 30_000 });
      const talk = voice.getByRole('button', { name: 'Hold to talk' });
      // The mouse is moved onto the button before each press: the page lays
      // out the same buttons again after a mute, and a press elsewhere is not a talk.
      const pressTalk = async () => {
        const box = await talk.boundingBox();
        if (box === null) throw new Error('no talk button');
        await a.page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
        await a.page.mouse.down();
      };
      await pressTalk();
      await expect(voice).toContainText('Transmitting');
      // Losing focus is a release.
      await a.page.evaluate(() => {
        window.dispatchEvent(new Event('blur'));
      });
      await expect(voice).toContainText('Not transmitting', { timeout: 5000 });
      await a.page.mouse.up();

      await voice.getByRole('button', { name: 'Mute' }).click();
      await expect(voice).toContainText('Muted.');
      await pressTalk();
      await expect(voice).toContainText('Not transmitting');
      await a.page.mouse.up();
      await voice.getByRole('button', { name: 'Unmute' }).click();
      await voice.getByRole('button', { name: 'Deafen' }).click();
      await expect(voice).toContainText('Deafened');
      await voice.getByRole('button', { name: 'Undeafen' }).click();

      // Open microphone: the steady tone opens it, and it is reported.
      await voice.getByLabel('Open microphone').check();
      await expect(voice.getByRole('meter', { name: 'Microphone level' })).toHaveAttribute('value', /[1-9]|0\.[1-9]/, { timeout: 10_000 });
      await expect(voice).toContainText('Transmitting', { timeout: 10_000 });
      await voice.getByLabel('Push to talk').check();
      await expect(voice).toContainText('Not transmitting', { timeout: 5000 });
      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
      await expectClean(a);
    } finally {
      await a.context.close();
    }
  });

  // Issue #467: on a phone the voice panel, joined, fits the screen in both
  // microphone modes, and is worked by tapping.
  test('on a phone the joined voice panel fits the screen and is worked by tapping', async ({ browser }) => {
    for (const [size, options] of phones) {
      const a = await person(browser, 'alice', options);
      try {
        const voice = voiceRegion(a.page);
        await expectFits(a.page, `${size}: the lobby before joining voice`);
        await voice.getByRole('button', { name: 'Join voice' }).tap();
        await expect(voice).toContainText('Connected in', { timeout: 30_000 });
        await expect(voice.getByRole('button', { name: 'Hold to talk' })).toBeVisible();
        await expectFits(a.page, `${size}: the voice panel, push to talk`);
        await voice.getByRole('button', { name: 'Mute' }).tap();
        await expect(voice).toContainText('Muted.');
        await voice.getByRole('button', { name: 'Unmute' }).tap();
        await voice.getByLabel('Open microphone').tap();
        await expect(voice.getByRole('meter', { name: 'Microphone level' })).toBeVisible();
        await expectFits(a.page, `${size}: the voice panel, open microphone`);
        await voice.getByLabel('Push to talk').tap();
        await voice.getByRole('button', { name: 'Leave voice' }).tap();
        await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
      } finally {
        await a.context.close();
      }
    }
  });

  test('removing the member disconnects the browser and the page says why', async ({ browser }) => {
    const b = await person(browser, 'bob');
    try {
      const voice = voiceRegion(b.page);
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Connected', { timeout: 30_000 });
      await vw.removeFromChannel(vw.bob);
      await expect(voice).toContainText('Voice stopped', { timeout: 60_000 });
      await expect(voice).toContainText('not a member');
      await expectClean(b);
    } finally {
      await b.context.close();
    }
  });

  test('restarting LiveKit brings the page back by itself', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Connected', { timeout: 30_000 });
      vw.stopLiveKit();
      await expect(voice).not.toContainText('Connected in', { timeout: 60_000 });
      await vw.startLiveKit();
      await expect(voice).toContainText('Connected in', { timeout: 120_000 });
      await expectClean(a, false);
    } finally {
      await a.context.close();
    }
  });
});
