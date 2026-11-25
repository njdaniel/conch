// The voice strip's status copy (issue #2198, UI-talk-3 to talk-7, UI-stream-5
// and UI-stream-6): one phrase for what others hear, on one line at the three
// sizes the design is held to; no amber microphone chip; the stopped-sharing
// notice where the stage was, and gone after a few seconds; and a phone's
// watch header that says "Watching NAME's screen" once, beside the channel.
// Against the pinned LiveKit of voice-world.ts: without Docker it fails when
// CI is set and is skipped otherwise.
import type { Locator, Page } from '@playwright/test';
import { chooseMicrophone, drawDesktopStrip, expect, showView, test } from './fixtures';
import { enter, joinVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';

const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });
const sizes = [
  ['desktop 1440×900', 1440, 900, true],
  ['desktop 1215×900', 1215, 900, true],
  ['phone 390×844', 390, 844, false],
] as const;

// oneLine is whether the element's text is drawn on one line: its box is not
// taller than a line of its own text.
async function oneLine(locator: Locator): Promise<{ readonly height: number; readonly line: number }> {
  return locator.evaluate((el) => {
    const style = getComputedStyle(el);
    const line = style.lineHeight.endsWith('px') ? parseFloat(style.lineHeight) : parseFloat(style.fontSize) * 1.4;
    return { height: el.getBoundingClientRect().height, line };
  });
}

test.describe('the voice strip status', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the voice status suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the voice status suite needs it');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });

  test.afterAll(() => {
    stopWorld?.();
  });

  for (const [name, width, height, desktop] of sizes) {
    test(`P2198 ${name}: the status is one phrase on one line, with no microphone chip`, async ({ browser }) => {
      let p: Someone | undefined;
      try {
        p = await enter(browser, vw, vw.alice, { context: { viewport: { width, height }, hasTouch: !desktop } });
        const page = p.page;
        if (desktop) await drawDesktopStrip(page);
        await joinVoice(p, 'Join voice');
        const strip = voice(page);
        await showView(page, 'Voice');
        await chooseMicrophone(strip, 'push to talk', desktop ? 'click' : 'tap');
        const phrase = strip.locator('.voice-tx');
        await expect(phrase).toHaveText('Push to talk: not sending');
        await expect(strip.getByText('Microphone in use')).toHaveCount(0);
        const idle = await oneLine(phrase);
        expect(idle.height, `${name}: the idle phrase is ${String(idle.height)} px high, one line is ${String(idle.line)} px`).toBeLessThanOrEqual(idle.line + 1);
        // Muted, the longest of the phrases, is one line too.
        // Its name is Unmute once pressed, so it is found by the pair.
        const mute = strip.getByRole('button', { name: /^(Mute|Unmute)$/ });
        if (desktop) await mute.click();
        else await mute.tap();
        await expect(phrase).toHaveText('Muted: not sending');
        const muted = await oneLine(phrase);
        expect(muted.height, `${name}: the muted phrase is ${String(muted.height)} px high, one line is ${String(muted.line)} px`).toBeLessThanOrEqual(muted.line + 1);
        // The open microphone says its own mode.
        if (desktop) await mute.click();
        else await mute.tap();
        await chooseMicrophone(strip, 'open', desktop ? 'click' : 'tap');
        await expect(phrase).toHaveText('Open mic: not sending');
        // The talk key and sharing notes are behind the info button, which
        // opens and closes them; the page's hold on the microphone is said in
        // Voice options.
        await chooseMicrophone(strip, 'push to talk', desktop ? 'click' : 'tap');
        const about = strip.getByRole('button', { name: 'About talking and sharing' });
        await expect(about).toHaveAttribute('aria-expanded', 'false');
        await expect(strip.locator('.share-line')).toHaveCount(0);
        if (desktop) await about.click();
        else await about.tap();
        await expect(about).toHaveAttribute('aria-expanded', 'true');
        await expect(strip.locator('.share-line')).toBeVisible();
        await expect(strip.getByRole('button', { name: 'Hold to talk' })).toHaveAttribute('title', /^Talk key: /);
        await expect(page.locator('#voice-options')).toContainText('This page is using your microphone.');
      } finally {
        await p?.close();
      }
    });
  }

  // alice shares and bob watches; alice stops, and bob sees the notice in the
  // stage's place, which goes by itself.
  test('P2198 the share-ended notice is where the stage was, and goes away', async ({ browser }) => {
    test.setTimeout(120_000);
    let a: Someone | undefined;
    let b: Someone | undefined;
    try {
      a = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
      b = await enter(browser, vw, vw.bob, { context: { viewport: { width: 1440, height: 900 } } });
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Join voice');
      await voice(a.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voice(a.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
      await b.page.locator('.call-share').filter({ hasText: 'alice · LIVE' }).getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
      await expect(b.page.getByRole('region', { name: 'Shared screen' })).toContainText('alice · LIVE');
      await voice(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      const notice = b.page.locator('.stage').getByRole('status').filter({ hasText: 'alice stopped sharing.' });
      await expect(notice).toBeVisible({ timeout: 20_000 });
      // Not in the strip, and not small grey text after the help line.
      await expect(voice(b.page)).not.toContainText('alice stopped sharing.');
      await expect(b.page.locator('.stage')).toContainText('alice stopped sharing.');
      // The sharer never reads "You stopped sharing." as this notice.
      await expect(b.page.locator('.stage')).not.toContainText('You stopped sharing.');
      await expect(notice).toHaveCount(0, { timeout: 15_000 });
    } finally {
      await a?.close();
      await b?.close();
    }
  });

  // On a phone the watch header's switcher says "Watching alice's screen",
  // once, and the title row shows the channel's name whole.
  test('P2198 a phone\'s watch header says Watching once and shows the channel', async ({ browser }) => {
    test.setTimeout(120_000);
    let a: Someone | undefined;
    let b: Someone | undefined;
    try {
      a = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
      b = await enter(browser, vw, vw.bob, { context: { viewport: { width: 390, height: 844 }, hasTouch: true } });
      await joinVoice(a, 'Join voice');
      await joinVoice(b, 'Join voice');
      await voice(a.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voice(a.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
      await showView(b.page, 'Voice');
      await b.page.locator('.call-share').filter({ hasText: 'alice · LIVE' }).getByRole('button', { name: 'Watch' }).tap({ timeout: 30_000 });
      const call = b.page.getByRole('region', { name: 'Video call' });
      await expect(call.locator('label.call-state')).toContainText("Watching alice's screen");
      const head = await call.locator('.call-head').innerText();
      const label = await call.locator('label.call-state').innerText();
      expect(`${head}\n${label}`.match(/Watching alice's screen/g) ?? [], 'the phrase is said once in the header').toHaveLength(1);
      expect(head).not.toContain("Watching alice's screen");
      // The channel is in the title row, whole.
      const channel = call.locator('.call-channel');
      await expect(channel).toContainText(`#${b.channel}`);
      const clipped = await call.locator('.call-context').evaluate((el) => el.scrollWidth > el.clientWidth + 1);
      expect(clipped, 'the channel name is cut short').toBe(false);
    } finally {
      await a?.close();
      await b?.close();
    }
  });
});
