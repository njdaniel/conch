// Issue #1028 (browser UX audit 3, F3): on a phone in voice, the strip under
// Chat and Channels is one row of four controls (where voice is, a button to
// the Voice view; Hold to talk; Mute; Leave voice), at most 64 px high, and it
// never covers the composer: the log shrinks instead. Everything else of voice
// (who is in it, the status lines, Deafen, Share screen, Watch, Voice options,
// the notes) is in the Voice view.
//
// The world of 12-screen-share: three members in voice in one channel, against
// the conchd and pinned LiveKit of voice-world.ts, on Chromium's fake devices.
// One shares from a desktop window; the other two are phones, 390×844 and
// 320×640. Each phone is measured in three states (in voice, nobody sharing;
// a share on, not watched; watching it), under Chat and under Channels. It is
// the same machine with emulated phones, not a real phone. Without Docker it
// fails when CI is set and is skipped otherwise, as in 10-voice.
import type { Browser, BrowserContextOptions, Locator, Page } from '@playwright/test';
import type { PostMessageRequestV2 } from '../src/schema/generated';
import { expect, expectFits, mainNav, messages, openChannel, phoneSizes, showView, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, type Member, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });

// The two sizes of the audit's finding.
const sizes = phoneSizes.filter(([size]) => size === '320×640' || size === '390×844');

type State = 'in voice' | 'share on' | 'watching';

// The message log's least height, by size and state (the issue's bounds). At
// 320×640 a watched share's picture takes the room a log would have; the
// bound there is #641's.
function logBound(size: string, state: State): number {
  if (size === '320×640') return state === 'watching' ? 120 : 240;
  return 200;
}

// The strip's four controls, by name, in order.
const fourControls = [/^In voice in #voice-room/, /^Hold to talk$/, /^Mute$/, /^Leave voice$/] as const;

interface Box {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

async function boxOf(el: Locator, what: string): Promise<Box> {
  const found = await el.boundingBox();
  if (found === null) throw new Error(`${what} has no box`);
  return found;
}

// Whether two boxes share any area.
function overlap(a: Box, b: Box): boolean {
  return a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height;
}

// hit says whether a tap at the centre of an element reaches it: what
// document.elementFromPoint finds there is the element or inside it.
function hit(el: Locator): Promise<boolean> {
  return el.evaluate((node) => {
    const r = node.getBoundingClientRect();
    const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    return at !== null && (at === node || node.contains(at));
  });
}

// hitEverywhere says whether a tap anywhere on an element reaches it: its
// centre and a point 3 px in from each corner.
function hitEverywhere(el: Locator): Promise<boolean> {
  return el.evaluate((node) => {
    const r = node.getBoundingClientRect();
    const points = [
      [r.left + r.width / 2, r.top + r.height / 2],
      [r.left + 3, r.top + 3],
      [r.right - 3, r.top + 3],
      [r.left + 3, r.bottom - 3],
      [r.right - 3, r.bottom - 3],
    ] as const;
    return points.every(([x, y]) => {
      const at = document.elementFromPoint(x, y);
      return at !== null && (at === node || node.contains(at));
    });
  });
}

test.describe('the phone voice strip with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the phone voice strip needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the phone voice strip suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  interface Someone {
    readonly page: Page;
    readonly size: string;
    readonly close: () => Promise<void>;
  }

  async function person(browser: Browser, member: Member, size: string, options: BrowserContextOptions): Promise<Someone> {
    const context = await browser.newContext(options);
    const page = await context.newPage();
    await signIn(page, vw, member.token, member.name);
    await openChannel(page, vw.channel);
    // The page is closed before its context, as fixtures.ts does: its pagehide
    // ends the session it started (issue #802).
    const close = async () => {
      if (!page.isClosed()) await page.close();
      await context.close();
    };
    return { page, size, close };
  }

  async function join(p: Someone): Promise<void> {
    await showView(p.page, 'Voice');
    await voiceRegion(p.page).getByRole('button', { name: 'Join voice' }).click();
    await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
  }

  async function post(body: string): Promise<void> {
    const request: PostMessageRequestV2 = { body };
    const r = await vw.api(vw.alice.token, 'POST', `/v2/channels/${vw.channel}/messages`, request);
    expect(r.status, `post ${body}`).toBe(201);
  }

  // expectStrip checks the strip under the view now shown: one row of the
  // four controls, each 44 px each way at least, at most 64 px high, and
  // nothing else drawn in it.
  async function expectStrip(p: Someone, where: string): Promise<Box> {
    const voice = voiceRegion(p.page);
    const dock = await boxOf(p.page.locator('.dock'), `${where}: the strip`);
    expect.soft(dock.height, `${where}: the strip is ${String(dock.height)} px high`).toBeLessThanOrEqual(64);
    const shown = voice.getByRole('button').filter({ visible: true });
    expect.soft(await shown.count(), `${where}: the buttons the strip shows`).toBe(fourControls.length);
    const middles: number[] = [];
    for (const name of fourControls) {
      const control = voice.getByRole('button', { name });
      if (!(await control.isVisible())) {
        expect.soft(false, `${where}: ${String(name)} is in the strip`).toBe(true);
        continue;
      }
      const box = await boxOf(control, `${where}: ${String(name)}`);
      const screen = p.page.viewportSize()?.width ?? 0;
      expect.soft(box.x, `${where}: ${String(name)} starts at ${String(box.x)} px`).toBeGreaterThanOrEqual(0);
      expect.soft(box.x + box.width, `${where}: ${String(name)} ends at ${String(box.x + box.width)} px of ${String(screen)}`).toBeLessThanOrEqual(screen);
      expect.soft(await hit(control), `${where}: a tap at the centre of ${String(name)} reaches it`).toBe(true);
      expect.soft(box.width, `${where}: ${String(name)} is ${String(box.width)} px wide`).toBeGreaterThanOrEqual(44);
      expect.soft(box.height, `${where}: ${String(name)} is ${String(box.height)} px high`).toBeGreaterThanOrEqual(44);
      middles.push(box.y + box.height / 2);
    }
    if (middles.length > 0) {
      expect.soft(Math.max(...middles) - Math.min(...middles), `${where}: the controls are one row`).toBeLessThanOrEqual(4);
    }
    // Nothing in the strip is wider than the window, or scrolls inside it.
    const sideways = await p.page.locator('.dock').evaluate((el) => el.scrollWidth - el.clientWidth);
    expect.soft(sideways, `${where}: the strip is ${String(sideways)} px wider than itself`).toBeLessThanOrEqual(0);
    await expectFits(p.page, where);
    // No roster, status line, hint or share note.
    // (The strip is drawn by now: these do not wait.)
    await expect.soft(voice.getByRole('list'), `${where}: no roster in the strip`).toBeHidden({ timeout: 1000 });
    await expect.soft(voice.locator('p').filter({ visible: true }), `${where}: no line of text in the strip`).toHaveCount(0, { timeout: 1000 });
    return dock;
  }

  // expectOpenMicrophone checks the strip under Chat with the open
  // microphone: the level meter lies wholly inside its slot, which lies inside
  // the window, so nothing of it is clipped; the strip is still one row of at
  // most 64 px, and the slot still reads "Open microphone".
  async function expectOpenMicrophone(p: Someone): Promise<void> {
    const where = `${p.size}, open microphone`;
    const voice = voiceRegion(p.page);
    const meter = voice.getByRole('meter', { name: 'Microphone level' });
    await expect(meter).toBeAttached();
    const slot = await boxOf(p.page.locator('.voice.strip .open-mic'), `${where}: the slot`);
    const level = await boxOf(meter, `${where}: the meter`);
    const screen = p.page.viewportSize()?.width ?? 0;
    console.log(`phone voice strip, ${where}: ${JSON.stringify({ slot, meter: level })}`);
    expect.soft(slot.x, `${where}: the slot starts at ${String(slot.x)} px`).toBeGreaterThanOrEqual(0);
    expect.soft(slot.x + slot.width, `${where}: the slot ends at ${String(slot.x + slot.width)} px of ${String(screen)}`).toBeLessThanOrEqual(screen);
    expect.soft(level.width, `${where}: the meter is ${String(level.width)} px wide`).toBeGreaterThanOrEqual(24);
    expect.soft(level.x, `${where}: the meter starts at ${String(level.x)}, its slot at ${String(slot.x)}`).toBeGreaterThanOrEqual(slot.x);
    expect
      .soft(level.x + level.width, `${where}: the meter ends at ${String(level.x + level.width)}, its slot at ${String(slot.x + slot.width)}`)
      .toBeLessThanOrEqual(slot.x + slot.width + 0.5);
    expect.soft(level.y, `${where}: the meter's top`).toBeGreaterThanOrEqual(slot.y);
    expect.soft(level.y + level.height, `${where}: the meter's bottom`).toBeLessThanOrEqual(slot.y + slot.height + 0.5);
    await expect.soft(p.page.locator('.voice.strip .open-mic'), `${where}: the slot is named`).toContainText('Open microphone');
    const dock = await boxOf(p.page.locator('.dock'), `${where}: the strip`);
    expect.soft(dock.height, `${where}: the strip is ${String(dock.height)} px high`).toBeLessThanOrEqual(64);
    await expectFits(p.page, where);
  }

  // settled waits until the strip, the stage and the log keep their sizes for
  // two reads in a row: a watched share's first frames resize the stage, and
  // the log with it.
  async function settled(page: Page): Promise<void> {
    let last = '';
    await expect
      .poll(
        async () => {
          const now = await page.evaluate(() =>
            JSON.stringify(['.dock', '.stage', '[role="log"]'].map((s) => document.querySelector(s)?.getBoundingClientRect().toJSON() as unknown)),
          );
          const same = now === last;
          last = now;
          return same;
        },
        { timeout: 15_000, intervals: [250] },
      )
      .toBe(true);
  }

  // framesArrive waits, as 12-screen-share does, until the watched share's
  // frames arrive and keep arriving: the video has its size and more than ten
  // frames, then more.
  async function framesArrive(page: Page): Promise<void> {
    const video = shareView(page).locator('video');
    const frames = () =>
      video.evaluate((el) => {
        const v = el as HTMLVideoElement;
        return v.videoWidth > 0 ? v.getVideoPlaybackQuality().totalVideoFrames : 0;
      });
    await expect.poll(frames, { timeout: 30_000 }).toBeGreaterThan(10);
    const before = await frames();
    await expect.poll(frames, { timeout: 15_000 }).toBeGreaterThan(before);
  }

  // measure checks one phone in one state, under Chat and then Channels.
  async function measure(p: Someone, state: State): Promise<void> {
    const where = `${p.size}, ${state}`;
    const { page } = p;
    await showView(page, 'Chat');
    if (state === 'watching') await framesArrive(page);
    await settled(page);
    const dock = await expectStrip(p, `${where}, Chat`);
    const log = messages(page);
    const composer = page.locator('form.composer');
    const send = page.getByRole('button', { name: 'Send' });
    const logBox = await boxOf(log, `${where}: the log`);
    const composerBox = await boxOf(composer, `${where}: the composer`);
    const sendHit = await hit(send);
    const sendWhole = await hitEverywhere(send);
    const boxHit = await hit(page.getByLabel(`Message #${vw.channel}`));
    const sendBox = await boxOf(send, `${where}: Send`);
    const report = {
      strip: dock.height,
      stripTop: dock.y,
      log: logBox.height,
      composerBottom: composerBox.y + composerBox.height,
      sendBottom: sendBox.y + sendBox.height,
      sendHit,
      sendWhole,
      boxHit,
    };
    console.log(`phone voice strip, ${where}: ${JSON.stringify(report)}`);
    expect.soft(sendHit, `${where}: a tap at the centre of Send reaches Send`).toBe(true);
    expect.soft(sendWhole, `${where}: a tap anywhere on Send reaches Send`).toBe(true);
    expect.soft(boxHit, `${where}: a tap at the centre of the message box reaches it`).toBe(true);
    expect.soft(overlap(dock, composerBox), `${where}: the strip and the composer overlap`).toBe(false);
    expect.soft(overlap(dock, logBox), `${where}: the strip and the log overlap`).toBe(false);
    expect.soft(logBox.height, `${where}: the log is ${String(logBox.height)} px high`).toBeGreaterThanOrEqual(logBound(p.size, state));
    // A tap on Send sends (only tried where it can be reached: a covered Send
    // would hold the tap until the test's time ran out).
    if (sendHit) {
      const body = `p1028 ${where} sent`;
      await page.getByLabel(`Message #${vw.channel}`).fill(body);
      await send.tap();
      await expect(log.getByRole('listitem').filter({ hasText: body })).toBeVisible();
    }
    // Jump to latest, offered to a reader who has scrolled up, is not under
    // the strip.
    // The scroll is said to the page as well: it may come before the next
    // frame of a page that is not in front.
    await log.evaluate((el) => {
      el.scrollTop = 0;
      el.dispatchEvent(new Event('scroll'));
    });
    await expect.poll(() => log.evaluate((el) => el.scrollTop)).toBe(0);
    await post(`p1028 ${where} while scrolled up`);
    const jump = page.getByRole('button', { name: 'Jump to latest' });
    await expect(jump).toBeVisible();
    const jumpBox = await boxOf(jump, `${where}: Jump to latest`);
    expect.soft(overlap(dock, jumpBox), `${where}: the strip covers Jump to latest`).toBe(false);
    const jumpHit = await hit(jump);
    expect.soft(jumpHit, `${where}: a tap at the centre of Jump to latest reaches it`).toBe(true);
    if (jumpHit) await jump.tap();
    else {
      await log.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
        el.dispatchEvent(new Event('scroll'));
      });
    }
    await expect(jump).toBeHidden();

    await showView(page, 'Channels');
    await expectStrip(p, `${where}, Channels`);
    await showView(page, 'Chat');
  }

  test('on a phone in voice the strip under Chat and Channels is one row of four, and never covers the composer', async ({ browser }) => {
    test.setTimeout(300_000);
    // Enough messages that the log scrolls, at either size.
    for (let i = 1; i <= 30; i++) await post(`p1028 line ${String(i)}`);
    const a = await person(browser, vw.alice, '1440×900', { viewport: { width: 1440, height: 900 } });
    const phones: Someone[] = [];
    try {
      for (const [size, options] of sizes) phones.push(await person(browser, size === '390×844' ? vw.bob : vw.carol, size, options));
      await join(a);
      for (const p of phones) await join(p);
      for (const p of phones) await expect(voiceRegion(p.page).getByRole('list', { name: '3 in voice' })).toBeVisible({ timeout: 15_000 });

      // In voice, nobody sharing.
      for (const p of phones) await measure(p, 'in voice');

      // A share on, not watched. Watch shows in the Voice view once the
      // share is known.
      const desk = voiceRegion(a.page);
      await desk.getByRole('button', { name: 'Share screen' }).click();
      await expect(desk).toContainText('You are sharing your screen', { timeout: 30_000 });
      for (const p of phones) {
        await showView(p.page, 'Voice');
        await expect(voiceRegion(p.page).getByRole('button', { name: 'Watch', exact: true })).toBeVisible({ timeout: 15_000 });
        await measure(p, 'share on');
      }

      // Watching the share: the stage is above the conversation under Chat.
      for (const p of phones) {
        await showView(p.page, 'Voice');
        await voiceRegion(p.page).getByRole('button', { name: 'Watch', exact: true }).tap();
        await expect(shareView(p.page)).toContainText('alice is sharing', { timeout: 30_000 });
        await measure(p, 'watching');
      }

      // The state button opens the Voice view, where everything the strip
      // left out is, and works.
      for (const p of phones) {
        const { page } = p;
        await showView(page, 'Chat');
        const voice = voiceRegion(page);
        await voice.getByRole('button', { name: fourControls[0] }).tap();
        await expect(mainNav(page).getByRole('button', { name: 'Voice', exact: true })).toHaveAttribute('aria-current', 'page');
        // (The phone before this one has left by now.)
        await expect(voice.getByRole('list', { name: /^[23] in voice$/ })).toContainText('You');
        await expect(voice).toContainText('Connected in #voice-room');
        await expect(voice.getByText('Microphone in use')).toBeVisible();
        await voice.getByRole('button', { name: 'Deafen' }).tap();
        await expect(voice).toContainText('Deafened');
        await voice.getByRole('button', { name: 'Undeafen' }).tap();
        await expect(voice).not.toContainText('Deafened');
        // Someone else shares, so Share screen is there and off.
        await expect(voice.getByRole('button', { name: 'Share screen' })).toBeDisabled();
        await shareView(page).getByRole('button', { name: 'Stop watching' }).tap();
        await expect(shareView(page)).toHaveCount(0);
        await voice.getByRole('button', { name: 'Watch', exact: true }).tap();
        await expect(shareView(page)).toContainText('alice is sharing', { timeout: 30_000 });
        const options = voice.getByRole('button', { name: 'Voice options' });
        await options.tap();
        await expect(voice.getByLabel('Push to talk')).toBeVisible();
        // With the open microphone, the strip shows its level where Hold to
        // talk was: the meter is drawn whole inside its slot, in one row.
        await voice.getByLabel('Open microphone').tap();
        await showView(page, 'Chat');
        await expectOpenMicrophone(p);
        await voice.getByRole('button', { name: fourControls[0] }).tap();
        await voice.getByLabel('Push to talk').tap();
        await options.tap();
        await expect(voice.getByLabel('Push to talk')).toBeHidden();
        // The strip's Leave voice leaves.
        await showView(page, 'Chat');
        await voice.getByRole('button', { name: 'Leave voice' }).tap();
        await showView(page, 'Voice');
        await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
      }
      await desk.getByRole('button', { name: 'Stop sharing' }).click();
    } finally {
      for (const p of [a, ...phones]) await p.close();
    }
  });
});
