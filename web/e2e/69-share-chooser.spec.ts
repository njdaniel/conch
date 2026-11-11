// Choosing the share quality where Share screen is (issue #1659), with the
// pinned LiveKit of voice-world.ts and Chromium's fake screen. The fake screen
// captures at the size and rate it is asked for; the instrument script below
// rewrites that ask to the size a test wants the "screen" to be, which is how
// Auto is tried on a 1920x1080 and on a 2560x1440 screen.
//
// Without Docker the tests fail when CI is set and are skipped otherwise, as
// in 12-screen-share.
//
// What this cannot try: a real screen's size, a real encoder's limits (the
// target rows record what the fake screen and this runner's encoder reached;
// real hardware is #1662's), Firefox, Safari.
import type { Browser, BrowserContextOptions, Page } from '@playwright/test';
import { expect, openChannel, test } from './fixtures';
import { enter, joinVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';

const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });
const tile = (page: Page, name: string) => page.locator('.call-share').filter({ hasText: `${name} · LIVE` });

// Installed before the page's own scripts: rewrites the size the picker is
// asked for to window.__screenSize, when a test has set one.
const instrument = () => {
  const w = window as unknown as { __screenSize?: { width: number; height: number }; __asked?: string; __got?: MediaTrackSettings };
  const devices = navigator.mediaDevices;
  const display = devices.getDisplayMedia.bind(devices);
  devices.getDisplayMedia = (options?: DisplayMediaStreamOptions) => {
    const size = w.__screenSize;
    const video = options?.video;
    w.__asked = JSON.stringify(options?.video);
    const asked = size === undefined || typeof video !== 'object' ? options : { ...options, video: { ...video, width: { ideal: size.width }, height: { ideal: size.height } } };
    return display(asked).then((stream) => {
      w.__got = stream.getVideoTracks()[0]?.getSettings();
      return stream;
    });
  };
};

const screenSize = (page: Page, width: number, height: number) =>
  page.evaluate(([w, h]) => {
    (window as unknown as { __screenSize: { width: number; height: number } }).__screenSize = { width: w ?? 0, height: h ?? 0 };
  }, [width, height]);

test.describe('the share quality chooser', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;
  // Each page's claim bodies, in order, as sent to conchd.
  const claims = new WeakMap<Page, string[]>();

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the share chooser needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the share chooser suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  function person(browser: Browser, who: 'alice' | 'bob', context?: BrowserContextOptions): Promise<Someone> {
    return enter(browser, vw, vw[who], {
      ...(context === undefined ? {} : { context }),
      before: async (page) => {
        await page.addInitScript(instrument);
        const bodies: string[] = [];
        claims.set(page, bodies);
        page.on('request', (request) => {
          if (request.method() === 'POST' && /\/voice\/share$/.test(request.url())) bodies.push(request.postData() ?? '');
        });
      },
    });
  }

  const bodiesOf = (page: Page): string[] => claims.get(page) ?? [];
  const opener = (page: Page) => voice(page).getByRole('button', { name: 'Share quality' });
  const qualityBody = (resolution: string, frameRate: number) => JSON.stringify({ quality: { resolution, frame_rate: frameRate } });

  async function stopAndWait(a: Someone): Promise<void> {
    await voice(a.page).getByRole('button', { name: 'Stop sharing' }).click();
    await expect(voice(a.page)).toContainText('You stopped sharing.');
    await expect.poll(async () => (await vw.audit()).filter((e) => e.action.startsWith('voice_share_')).at(-1)?.action, { timeout: 15_000 }).toBe('voice_share_stopped');
  }

  test('the menu works by keyboard and mouse, the choice survives a reload, and Share screen alone shares at once at it', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      await joinVoice(a, 'Join voice');
      const button = opener(a.page);
      await expect(button).toHaveAttribute('aria-haspopup', 'menu');
      // By keyboard: Enter opens it at the checked resolution (1080p, the
      // default), Down moves to Source, Enter chooses it, Escape closes it and
      // returns focus to the button.
      await button.focus();
      await a.page.keyboard.press('Enter');
      await expect(button).toHaveAttribute('aria-expanded', 'true');
      const menu = voice(a.page).getByRole('menu', { name: 'Share quality' });
      await expect(menu.getByRole('menuitemradio', { name: /^1080p/ })).toBeFocused();
      await a.page.keyboard.press('ArrowDown');
      await expect(menu.getByRole('menuitemradio', { name: /^Source/ })).toBeFocused();
      await a.page.keyboard.press('Space');
      await expect(menu.getByRole('menuitemradio', { name: /^Source/ })).toHaveAttribute('aria-checked', 'true');
      // Estimates are labelled as such.
      await expect(menu.getByRole('menuitemradio', { name: /^Source/ })).toContainText(/about \d+ Mbps up \(estimate\)/);
      await a.page.keyboard.press('Escape');
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toBeFocused();
      // By mouse: 60 fps, and the caution that Source at 60 earns.
      await button.click();
      await menu.getByRole('menuitemradio', { name: '60 fps' }).click();
      await expect(menu).toContainText("Many computers and browsers can't encode that and will send less");
      await a.page.keyboard.press('Escape');
      // The Voice options selects show the same choice.
      await voice(a.page).getByRole('button', { name: 'Voice options' }).click();
      await expect(voice(a.page).getByLabel('Share resolution')).toHaveValue('source');
      await expect(voice(a.page).getByLabel('Share frame rate')).toHaveValue('60');

      await a.page.reload();
      await openChannel(a.page, vw.channel);
      await joinVoice(a, 'Join voice');
      await voice(a.page).getByRole('button', { name: 'Voice options' }).click();
      await expect(voice(a.page).getByLabel('Share resolution')).toHaveValue('source');
      await expect(voice(a.page).getByLabel('Share frame rate')).toHaveValue('60');

      // Share screen alone shares at once, declaring the remembered choice.
      await voice(a.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voice(a.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
      expect(bodiesOf(a.page)[0]).toBe(qualityBody('source', 60));
      await stopAndWait(a);
    } finally {
      await a.close();
    }
  });

  test('a browser with no stored choice declares 1080p at 30 fps, as before', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      await joinVoice(a, 'Join voice');
      await voice(a.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voice(a.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
      expect(bodiesOf(a.page)).toEqual([qualityBody('1080p', 30), qualityBody('1080p', 30)]);
      await stopAndWait(a);
    } finally {
      await a.close();
    }
  });

  // Q10: a capture of 1920 px or less across is shared as it is (Source), a
  // larger one at 1080p, at 30 fps. The claim never says auto.
  for (const row of [
    { what: '1920x1080', width: 1920, height: 1080, resolved: 'source', label: 'Source · 30 fps' },
    { what: '2560x1440', width: 2560, height: 1440, resolved: '1080p', label: '1080p · 30 fps' },
  ]) {
    test(`Auto on a ${row.what} screen declares ${row.resolved}/30, and the viewer's tile says so`, async ({ browser }) => {
      const a = await person(browser, 'alice');
      const b = await person(browser, 'bob');
      try {
        await joinVoice(a, 'Join voice');
        await joinVoice(b, 'Watch only');
        await screenSize(a.page, row.width, row.height);
        await opener(a.page).click();
        await voice(a.page).getByRole('menu', { name: 'Share quality' }).getByRole('menuitemradio', { name: /^Auto/ }).click();
        await a.page.keyboard.press('Escape');
        await voice(a.page).getByRole('button', { name: 'Share screen' }).click();
        await expect(voice(a.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
        test.info().annotations.push({ type: 'capture', description: JSON.stringify(await a.page.evaluate(() => { const w = window as unknown as { __asked?: string; __got?: unknown }; return { asked: w.__asked, got: w.__got }; })) });
        // Provisional first, then what the capture came to, on the one repeat
        // claim: both concrete values.
        await expect.poll(() => bodiesOf(a.page), { timeout: 15_000 }).toEqual([qualityBody('1080p', 30), qualityBody(row.resolved, 30)]);
        expect(bodiesOf(a.page).join('')).not.toContain('auto');
        await expect(tile(b.page, 'alice')).toContainText(row.label, { timeout: 15_000 });
        // The Voice options still say Auto, which is the local choice.
        await voice(a.page).getByRole('button', { name: 'Voice options' }).click();
        await expect(voice(a.page).getByLabel('Share resolution')).toHaveValue('auto');
        await stopAndWait(a);
      } finally {
        for (const p of [a, b]) await p.close();
      }
    });
  }

  // The targets: each sends a top layer no larger than asked, and the sharer's
  // readout says what was asked beside what was sent, and a reason when the
  // browser gives one. What the fake screen and this runner's encoder reach is
  // recorded, not asserted beyond that.
  for (const row of [
    { what: '720p30', resolution: '720p', frameRate: 30, long: 1280 },
    { what: '1080p30', resolution: '1080p', frameRate: 30, long: 1920 },
    { what: '1080p60', resolution: '1080p', frameRate: 60, long: 1920 },
    { what: 'Source at 30', resolution: 'source', frameRate: 30, long: 3840 },
    { what: 'Source at 60', resolution: 'source', frameRate: 60, long: 3840 },
  ]) {
    test(`target ${row.what}: no larger than asked, and the readout says what was asked and sent`, async ({ browser }) => {
      test.setTimeout(120_000);
      const a = await person(browser, 'alice');
      const b = await person(browser, 'bob');
      try {
        await joinVoice(a, 'Join voice');
        await joinVoice(b, 'Watch only');
        await voice(a.page).getByRole('button', { name: 'Voice options' }).click();
        await voice(a.page).getByLabel('Share resolution').selectOption(row.resolution);
        await voice(a.page).getByLabel('Share frame rate').selectOption(String(row.frameRate));
        await voice(a.page).getByRole('button', { name: 'Share screen' }).click();
        await expect(voice(a.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
        // Focused, so bob takes the top layer.
        await b.page.getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
        await expect(shareView(b.page)).toContainText('alice · LIVE');
        await shareView(b.page).getByRole('button', { name: "Focus alice's screen" }).click();
        const line = voice(a.page).getByRole('status', { name: 'Share quality' });
        await expect(line).toContainText(/Sending: \d+×\d+ · \d+ fps/, { timeout: 40_000 });
        let outcome = 'neither';
        let last = '';
        for (let i = 0; i < 20 && outcome === 'neither'; i++) {
          await a.page.waitForTimeout(1000);
          last = (await line.textContent()) ?? '';
          const sent = /Sending: (\d+)×(\d+) · (\d+) fps/.exec(last);
          if (sent !== null) {
            expect(Math.max(Number(sent[1]), Number(sent[2])), `the top layer is no larger than asked: ${last}`).toBeLessThanOrEqual(row.long);
            if (Math.max(Number(sent[1]), Number(sent[2])) >= row.long * 0.98 && Number(sent[3]) >= row.frameRate * 0.8) outcome = 'reached';
          }
          const reason = /Limited by (CPU|bandwidth)|smaller than|doesn't say why/.exec(last)?.[0];
          if (reason !== undefined) outcome = `reason: ${reason}`;
        }
        test.info().annotations.push({ type: 'readout', description: `${row.what}: ${outcome}: ${last}` });
        // What this runner's fake screen and encoder reach is recorded. The
        // sharer's line (#1660) explains a shortfall only when the browser
        // says why (qualityLimitationReason): with none reported it says
        // nothing, so "neither" is a possible, and recorded, outcome here, and
        // real hardware's is #1662's. What is asserted is that the line says
        // what was asked and what was sent, and that the picture is not
        // larger than the target.
        await expect(line).toContainText(`Asked ${row.resolution === 'source' ? 'source size' : row.resolution} · ${String(row.frameRate)} fps`);
        await stopAndWait(a);
      } finally {
        for (const p of [a, b]) await p.close();
      }
    });
  }
});
