// Voice options is a panel over the page (issue #2189, UI-talk-2): opening it
// does not move the conversation or the voice strip, the panel lies inside the
// window (scrolling in itself if it is taller), Escape closes it and returns
// focus to its button, a press outside closes it, and More about sharing
// still opens it at the share notes. Measured as geometry, in the pinned
// LiveKit of voice-world.ts: without Docker it fails when CI is set and is
// skipped otherwise.
import type { Locator, Page } from '@playwright/test';
import { drawDesktopStrip, expect, test } from './fixtures';
import { enter, joinVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';

const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });

// box is the element's rectangle as the page lays it out now.
async function box(locator: Locator, what: string) {
  const b = await locator.boundingBox();
  if (b === null) throw new Error(`${what} has no box`);
  return b;
}

// settled waits for the element's box to stop changing: presence frames land
// after the join and redraw the roster, which is not what is measured here.
async function settled(locator: Locator, what: string): Promise<void> {
  let last = '';
  let same = 0;
  await expect(async () => {
    const now = JSON.stringify(await box(locator, what));
    same = now === last ? same + 1 : 0;
    last = now;
    expect(same, `${what} is still changing`).toBeGreaterThanOrEqual(4);
  }).toPass({ timeout: 10_000, intervals: [250] });
}

const sizes = [
  ['desktop 1440×900', 1440, 900, true],
  ['desktop 1215×900', 1215, 900, true],
  ['phone 390×844', 390, 844, false],
] as const;

test.describe('Voice options', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the Voice options suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the Voice options suite needs it');
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
    test(`P2189 ${name}: opening Voice options moves nothing under it and the panel is in the window`, async ({ browser }) => {
      let p: Someone | undefined;
      try {
        p = await enter(browser, vw, vw.alice, { context: { viewport: { width, height } } });
        const page = p.page;
        if (desktop) await drawDesktopStrip(page);
        await joinVoice(p, 'Join voice');
        const strip = voice(page);
        const conversation = page.locator('.conversation');
        const button = strip.getByRole('button', { name: 'Voice options' });
        const panel = page.locator('#voice-options');

        await expect(strip.getByRole('list', { name: / in voice$/ })).toContainText(p.member.name, { timeout: 30_000 });
        await settled(strip, 'the strip');
        const before = { conversation: await box(conversation, 'the conversation'), strip: await box(strip, 'the strip') };
        await button.click();
        await expect(button).toHaveAttribute('aria-expanded', 'true');
        await expect(panel).toBeVisible();
        // Nothing under it moved (the layout settles a frame after the click).
        await expect(async () => {
          expect(await box(conversation, 'the conversation')).toEqual(before.conversation);
          expect(await box(strip, 'the strip')).toEqual(before.strip);
        }).toPass({ timeout: 3000 });

        // The panel lies inside the window; taller content scrolls inside it.
        const b = await box(panel, 'the panel');
        expect(b.x, 'the panel starts inside the window').toBeGreaterThanOrEqual(0);
        expect(b.y, 'the panel starts below the window top').toBeGreaterThanOrEqual(0);
        expect(b.x + b.width, 'the panel ends inside the window width').toBeLessThanOrEqual(width + 0.5);
        expect(b.y + b.height, 'the panel ends inside the window height').toBeLessThanOrEqual(height + 0.5);
        // Where there is room it is above the strip; a phone's strip is too
        // near the top, and the panel is a sheet over the window instead.
        if (desktop) expect(b.y + b.height, 'the panel is above the strip').toBeLessThanOrEqual(before.strip.y + 0.5);
        else await expect(panel).toHaveClass(/sheet/);
        // The page itself gained no scroll from it.
        const page2 = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight);
        expect(page2).toBeLessThanOrEqual(1);
      } finally {
        await p?.close();
      }
    });
  }

  // A window too short for the panel: it scrolls inside itself, a control low
  // in it can be reached and used, and the page and the strip do not move.
  test('P2189 a panel taller than its room scrolls inside itself and moves nothing', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 520 } } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const strip = voice(page);
      const panel = page.locator('#voice-options');
      await expect(strip.getByRole('list', { name: / in voice$/ })).toContainText(p.member.name, { timeout: 30_000 });
      // This short window scrolls as a document: bring the button into view
      // first, so the click itself does not scroll what is measured.
      await strip.getByRole('button', { name: 'Voice options' }).scrollIntoViewIfNeeded();
      await settled(strip, 'the strip');
      const before = await box(strip, 'the strip');
      await strip.getByRole('button', { name: 'Voice options' }).click();
      await expect(panel).toBeVisible();
      // Opening moved nothing under it.
      await expect(async () => {
        expect(await box(strip, 'the strip')).toEqual(before);
      }).toPass({ timeout: 3000 });
      const room = await panel.evaluate((el) => ({ client: el.clientHeight, content: el.scrollHeight }));
      expect(room.content, 'the content is taller than the panel').toBeGreaterThan(room.client);
      // The last control in the panel is reached by scrolling the panel, and works.
      const last = panel.locator('input, select, button').last();
      // Scrolled by the wheel over the panel, as a person does.
      const over = await box(panel, 'the panel');
      await page.mouse.move(over.x + over.width / 2, over.y + over.height / 2);
      await page.mouse.wheel(0, 5000);
      await expect.poll(() => panel.evaluate((el) => el.scrollTop + el.clientHeight >= el.scrollHeight - 1)).toBe(true);
      expect(await panel.evaluate((el) => el.scrollTop), 'the panel scrolled').toBeGreaterThan(0);
      // At the panel's end a further wheel gesture does not chain into the
      // page, which would move the open panel. The page is first put where it
      // can scroll down, or the check would pass for want of room to move.
      await page.evaluate(() => {
        window.scrollTo(0, 0);
      });
      const room2 = await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight - window.scrollY);
      expect(room2, 'the page has room to scroll down').toBeGreaterThan(20);
      await expect.poll(() => panel.evaluate((el) => el.scrollTop + el.clientHeight >= el.scrollHeight - 1)).toBe(true);
      const scrolledTo = await page.evaluate(() => window.scrollY);
      const placed = await box(panel, 'the panel');
      const over2 = await box(panel, 'the panel');
      await page.mouse.move(over2.x + over2.width / 2, over2.y + over2.height / 2);
      await page.mouse.wheel(0, 3000);
      await page.waitForTimeout(300);
      expect(await page.evaluate(() => window.scrollY), 'a second wheel at the end moved the page').toBe(scrolledTo);
      expect(await box(panel, 'the panel'), 'a second wheel at the end moved the panel').toEqual(placed);
      const lb = await box(last, 'the last control');
      const pb = await box(panel, 'the panel');
      expect(lb.y).toBeGreaterThanOrEqual(pb.y - 0.5);
      expect(lb.y + lb.height).toBeLessThanOrEqual(pb.y + pb.height + 0.5);
      const type = await last.getAttribute('type');
      if (type === 'range') {
        const was = await last.inputValue();
        await last.click();
        await page.keyboard.press('ArrowRight');
        expect(await last.inputValue(), 'the slider moved').not.toBe(was);
      } else {
        await last.click();
      }
      expect(await page.evaluate(() => window.scrollY), 'the page did not scroll').toBe(scrolledTo);
    } finally {
      await p?.close();
    }
  });

  // On a phone the section has little room above it, and a scrolled page can
  // leave its top off the window: the panel is then a sheet over the window,
  // resting on the nav, and a control in it is in view and usable. Two cases:
  // the top under 40% of the window but visible, and the top off the window
  // (asserted as the sheet opens). Then the window shrinks to 240 px.
  for (const [where, height] of [['its top visible but under 40%', 520], ['its top off the window', 300]] as const) {
    test(`P2189 on a phone the panel is a sheet in the window above the nav and usable, ${where}`, async ({ browser }) => {
      let p: Someone | undefined;
      try {
        p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height }, hasTouch: true, isMobile: true } });
        const page = p.page;
        await joinVoice(p, 'Join voice');
        const strip = voice(page);
        const button = strip.getByRole('button', { name: 'Voice options' });
        await button.scrollIntoViewIfNeeded();
        await page.evaluate(() => {
          window.scrollTo(0, document.documentElement.scrollHeight);
        });
        const top = (await box(strip, 'the strip')).y;
        if (height === 300) expect(top, `the voice section's top (${String(top)}) is off the window as the sheet opens`).toBeLessThan(0);
        else {
          expect(top, `the voice section's top (${String(top)}) is on the window`).toBeGreaterThanOrEqual(0);
          expect(top, `the voice section's top (${String(top)}) is under 40% of the window`).toBeLessThan(height * 0.4);
        }
        await button.tap();
        const panel = page.locator('#voice-options');
        await expect(panel).toBeVisible();
        await expect(panel).toHaveClass(/sheet/);
        // The tap may have scrolled the trigger into view: the geometry is
        // checked again with the sheet open.
        const openTop = (await box(strip, 'the strip')).y;
        if (height === 300) expect(openTop, `the voice section's top (${String(openTop)}) is off the window with the sheet open`).toBeLessThan(0);
        else expect(openTop, `the voice section's top (${String(openTop)}) is under 40% with the sheet open`).toBeLessThan(height * 0.4);
        const nav = page.locator('.phone-nav');
        for (const h of [height, 240]) {
          await page.setViewportSize({ width: 390, height: h });
          await expect(async () => {
            const b = await box(panel, 'the panel');
            const n = await box(nav, 'the nav');
            expect(b.x).toBeGreaterThanOrEqual(0);
            expect(b.y, `${String(h)} px window: the sheet's top`).toBeGreaterThanOrEqual(-0.5);
            expect(b.x + b.width).toBeLessThanOrEqual(390.5);
            expect(b.y + b.height, `${String(h)} px window: the sheet's bottom is on the nav's top`).toBeLessThanOrEqual(n.y + 0.5);
            expect(b.height, 'a sheet is at most about 80% of the window').toBeLessThanOrEqual(h * 0.8 + 1);
          }).toPass({ timeout: 5000 });
        }
        await page.setViewportSize({ width: 390, height });
        const open = panel.getByRole('radio', { name: 'Open microphone' });
        await open.scrollIntoViewIfNeeded();
        await open.tap();
        await expect(open).toBeChecked();
      } finally {
        await p?.close();
      }
    });
  }

  // Opened by the keyboard as a sheet, the panel is modal: it takes focus, and
  // everything behind it, the strip (Leave voice among it), a watched share's
  // controls and the rest of the page, is out of the tab order until it
  // closes, in both directions; only the phone's nav, which the sheet rests
  // on, stays reachable. Focus then returns to the Voice options button.
  // (Above the strip the panel covers the conversation, not the strip's
  // controls, so they stay reachable.)
  test('P2189 a sheet opened by keyboard is modal: focus stays in it or on the nav', async ({ browser }) => {
    let sharer: Someone | undefined;
    let p: Someone | undefined;
    try {
      sharer = await enter(browser, vw, vw.bob, { context: { viewport: { width: 1440, height: 900 } } });
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 520 }, hasTouch: true, isMobile: true } });
      const page = p.page;
      await joinVoice(sharer, 'Join voice');
      await joinVoice(p, 'Join voice');
      await voice(sharer.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voice(sharer.page)).toContainText('You are sharing your screen', { timeout: 30_000 });
      const strip = voice(page);
      await page.locator('.call-share').getByRole('button', { name: 'Watch', exact: true }).tap();
      const watched = page.getByRole('region', { name: 'Shared screen' });
      await expect(watched).toContainText('bob · LIVE', { timeout: 30_000 });
      const button = strip.getByRole('button', { name: 'Voice options' });
      const leave = strip.getByRole('button', { name: 'Leave voice' });
      const panel = page.locator('#voice-options');
      await button.focus();
      await page.keyboard.press('Enter');
      await expect(panel).toHaveClass(/sheet/);
      await expect(panel).toHaveAttribute('role', 'dialog');
      await expect(panel).toHaveAttribute('aria-modal', 'true');
      await expect(panel.getByRole('button', { name: 'Close' })).toBeFocused();
      // The share's controls are on the page, and inert.
      await expect(watched.getByRole('button', { name: 'Stop watching' })).toBeAttached();
      expect(await watched.evaluate((el) => el.closest('[inert]') !== null), 'the watched share is inert').toBe(true);
      const where = () => page.evaluate(() => {
        const at = document.activeElement;
        return {
          allowed: at === null || at === document.body || at.closest('#voice-options, .phone-nav') !== null,
          label: at?.getAttribute('aria-label') ?? at?.textContent ?? '',
        };
      });
      for (const key of ['Tab', 'Shift+Tab']) {
        for (let i = 0; i < 40; i++) {
          await page.keyboard.press(key);
          const at = await where();
          expect(at.allowed, `${key} ${String(i)} reached something behind the sheet (${at.label})`).toBe(true);
        }
      }
      await expect(leave).not.toBeFocused();
      await panel.getByRole('button', { name: 'Close' }).focus();
      await page.keyboard.press('Escape');
      await expect(panel).toBeHidden();
      await expect(button).toBeFocused();
      expect(await watched.evaluate((el) => el.closest('[inert]') !== null), 'the watched share is reachable again').toBe(false);
      await page.keyboard.press('Tab');
      await expect(leave).toBeFocused();
    } finally {
      await p?.close();
      await sharer?.close();
    }
  });

  // A sheet is modal, so it is only ever opened by a press, with nothing else
  // open: an anchored panel that the window narrows under closes instead of
  // becoming a sheet over whatever else is open. Focus held in it goes to the
  // Voice options button; nothing is left inert.
  test('P2189 an anchored panel closes when the window narrows to a phone, and nothing is left inert', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('button', { name: 'Voice' }).click();
      await page.setViewportSize({ width: 1050, height: 900 });
      const options = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');
      await options.click();
      await expect(panel).toBeVisible();
      await expect(panel).not.toHaveClass(/sheet/);
      await panel.getByRole('slider', { name: /Release delay/ }).focus();
      await page.setViewportSize({ width: 390, height: 520 });
      await expect(panel).toBeHidden();
      await expect(options).toHaveAttribute('aria-expanded', 'false');
      await expect(options).toBeFocused();
      expect(await page.locator('[inert]').count(), 'nothing is left inert').toBe(0);
    } finally {
      await p?.close();
    }
  });

  // codex's repro: with the keyboard open Members and a profile card over the
  // anchored panel, then narrow. The panel closes, the card keeps focus,
  // nothing is inert, and Escape closes the card.
  test('P2189 a profile card opened over the anchored panel keeps focus when the window narrows', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      await page.getByRole('navigation', { name: 'Main navigation' }).getByRole('button', { name: 'Voice' }).click();
      await page.setViewportSize({ width: 1050, height: 900 });
      const options = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');
      await options.click();
      await expect(panel).toBeVisible();
      const members = page.getByRole('button', { name: 'Members' });
      // The resize above must have settled with the panel still open: that is
      // the state the narrowing below is tested from.
      await expect(options).toHaveAttribute('aria-expanded', 'true');
      await expect(panel).toBeVisible();
      await members.focus();
      await page.keyboard.press('Enter');
      const list = page.getByRole('region', { name: 'Members list' });
      await expect(list).toBeVisible();
      const person = list.getByRole('button', { name: new RegExp(vw.alice.name) }).first();
      await person.focus();
      await page.keyboard.press('Enter');
      const card = page.locator('.profile-card');
      await expect(card).toBeVisible();
      await page.setViewportSize({ width: 390, height: 520 });
      await expect(panel).toBeHidden();
      await expect(card).toBeVisible();
      const inCard = () => page.evaluate(() => document.activeElement?.closest('.profile-card') != null);
      expect(await inCard(), 'the card keeps focus').toBe(true);
      // Members is drawn over the page now (issue #2196): the page behind it is
      // inert, and the card and the drawer are not.
      expect(await page.evaluate(() => document.querySelector('.profile-card')?.closest('[inert]') ?? null), 'the card is not inert').toBeNull();
      expect(await list.evaluate((el) => el.closest('[inert]')), 'the drawer is not inert').toBeNull();
      await page.keyboard.press('Escape');
      await expect(card).toHaveCount(0);
    } finally {
      await p?.close();
    }
  });

  // Voice options closes by every way it goes out of sight, so coming back to
  // the Voice view never reopens a modal sheet nobody pressed for (codex's two
  // repros: Chat and back; Chat, Members, and back).
  for (const [name, withMembers] of [['Chat and back', false], ['Chat, Members and back', true]] as const) {
    test(`P2189 the sheet stays closed after the phone goes to Chat and back: ${name}`, async ({ browser }) => {
      let p: Someone | undefined;
      try {
        p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 520 }, hasTouch: true, isMobile: true } });
        const page = p.page;
        await joinVoice(p, 'Join voice');
        const nav = page.getByRole('navigation', { name: 'Main navigation' });
        const options = voice(page).getByRole('button', { name: 'Voice options' });
        const panel = page.locator('#voice-options');
        await nav.getByRole('button', { name: 'Voice' }).click();
        await options.focus();
        await page.keyboard.press('Enter');
        await expect(panel).toHaveClass(/sheet/);
        const chat = nav.getByRole('button', { name: 'Chat' });
        await chat.focus();
        await page.keyboard.press('Enter');
        await expect(panel).toBeHidden();
        if (withMembers) {
          const members = page.getByRole('button', { name: 'Members' });
          // By keyboard throughout: a pointer press outside would close the
          // hidden panel on its own, which is not the path under test.
          if ((await members.getAttribute('aria-expanded')) !== 'true') {
            await members.focus();
            await page.keyboard.press('Enter');
          }
          await expect(members).toHaveAttribute('aria-expanded', 'true');
          // The drawer is modal (issue #2196) and the navigation inert behind
          // it: it closes, by keyboard, before the Voice view is chosen.
          await page.keyboard.press('Escape');
          await expect(members).toHaveAttribute('aria-expanded', 'false');
        }
        await nav.getByRole('button', { name: 'Voice' }).focus();
        await page.keyboard.press('Enter');
        await expect(options).toHaveAttribute('aria-expanded', 'false');
        await expect(panel).toBeHidden();
        expect(await page.evaluate(() => document.activeElement?.closest('#voice-options') != null), 'focus was not moved into a sheet').toBe(false);
        expect(await page.locator('[inert]').count(), 'nothing is inert').toBe(0);
      } finally {
        await p?.close();
      }
    });
  }

  // The anchored panel is a popover and a disclosure, driven by real key
  // presses: Enter on the button opens it and Tab goes into it (and on, through
  // it), Shift+Tab from its first control goes back to the button with the
  // panel still open, and Tab past its last control goes to what follows the
  // button (Leave voice) and closes it, focus not left under the panel.
  test('P2189 the anchored panel is a keyboard disclosure: Tab in, Shift+Tab back, Tab out', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const options = voice(page).getByRole('button', { name: 'Voice options' });
      const leave = voice(page).getByRole('button', { name: /^Leave voice/ });
      const panel = page.locator('#voice-options');
      const inPanel = () => page.evaluate(() => document.activeElement?.closest('#voice-options') != null);
      await options.focus();
      await page.keyboard.press('Enter');
      await expect(panel).toBeVisible();
      await expect(panel).not.toHaveClass(/sheet/);
      expect(await inPanel(), 'Enter on the button puts focus in the panel').toBe(true);
      // Shift+Tab from the first control: back to the button, panel open.
      await page.keyboard.press('Shift+Tab');
      await expect(options).toBeFocused();
      await expect(panel).toBeVisible();
      // Tab from the button: into the panel, not Leave voice.
      await page.keyboard.press('Tab');
      expect(await inPanel(), 'Tab from the button enters the panel').toBe(true);
      await expect(panel).toBeVisible();
      // Tab through the panel to its last control: it stays open on the way.
      let steps = 0;
      for (; steps < 80; steps++) {
        const last = await page.evaluate(() => {
          const els = Array.from(document.querySelectorAll<HTMLElement>('#voice-options select:not(:disabled), #voice-options input:not(:disabled), #voice-options button:not(:disabled), #voice-options a[href]')).filter((e) => e.offsetParent !== null);
          return els[els.length - 1] === document.activeElement;
        });
        if (last) break;
        await page.keyboard.press('Tab');
        await expect(panel).toBeVisible();
      }
      expect(steps, 'a last control was reached').toBeLessThan(80);
      await page.keyboard.press('Tab');
      await expect(leave).toBeFocused();
      await expect(panel).toBeHidden();
      await expect(options).toHaveAttribute('aria-expanded', 'false');
    } finally {
      await p?.close();
    }
  });

  // Over an open sheet Ctrl+K leaves the page alone (the sheet is a dialog, and
  // the shortcuts do not run over another dialog): the sheet keeps its focus,
  // and Escape closes the sheet with focus back on its button.
  test('P2189 Ctrl+K does not open the switcher over an open sheet, and Escape closes the sheet', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 520 }, hasTouch: true, isMobile: true } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const options = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');
      await options.focus();
      await page.keyboard.press('Enter');
      await expect(panel).toHaveClass(/sheet/);
      const close = panel.getByRole('button', { name: 'Close' });
      await expect(close).toBeFocused();
      await page.keyboard.press('Control+k');
      await expect(page.getByRole('dialog', { name: 'Find a channel' })).toHaveCount(0);
      await expect(close).toBeFocused();
      await page.keyboard.press('Escape');
      await expect(panel).toBeHidden();
      await expect(options).toBeFocused();
    } finally {
      await p?.close();
    }
  });

  // The sheet turning into the panel above the strip (the window grows) hides
  // its Close: focus held there moves to a visible control in the panel.
  test('P2189 focus on the sheet\'s Close moves into the panel when the sheet becomes the anchored panel', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 390, height: 520 }, hasTouch: true, isMobile: true } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const button = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');
      await button.focus();
      await page.keyboard.press('Enter');
      await expect(panel).toHaveClass(/sheet/);
      await expect(panel.getByRole('button', { name: 'Close' })).toBeFocused();
      await page.setViewportSize({ width: 1440, height: 900 });
      await expect(panel).not.toHaveClass(/sheet/);
      await expect(panel).toBeVisible();
      await expect(async () => {
        const at = await page.evaluate(() => {
          const el = document.activeElement;
          return { inPanel: el?.closest('#voice-options') != null, visible: el instanceof HTMLElement && el.offsetParent !== null, close: el?.classList.contains('voice-options-close') === true };
        });
        expect(at, 'focus is on a visible control in the panel').toEqual({ inPanel: true, visible: true, close: false });
      }).toPass({ timeout: 5000 });
    } finally {
      await p?.close();
    }
  });

  // Watch only has no microphone or camera choices: when the sheet becomes the
  // anchored panel, focus still lands on an enabled, drawn control (or the
  // panel itself), never on body or a disabled radio.
  test('P2189 in Watch only, focus is kept in the panel when the sheet becomes the anchored panel', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      // Output selection is forced off, as LiveKit's supportsSetSinkId says it
      // when the audio element has no setSinkId: Chromium offers it, so the
      // Speakers picker would be an enabled control ahead of the disabled
      // microphone radios and this would not be the disabled-control case.
      p = await enter(browser, vw, vw.alice, {
        context: { viewport: { width: 390, height: 520 }, hasTouch: true, isMobile: true },
        before: async (page) => {
          await page.addInitScript(() => {
            Reflect.deleteProperty(HTMLMediaElement.prototype, 'setSinkId');
          });
        },
      });
      const page = p.page;
      await joinVoice(p, 'Watch only');
      await voice(page).getByRole('button', { name: 'Voice options' }).click();
      await expect(page.locator('#voice-options').getByLabel('Speakers')).toHaveCount(0);
      await page.keyboard.press('Escape');
      const button = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');
      await button.focus();
      await page.keyboard.press('Enter');
      await expect(panel).toHaveClass(/sheet/);
      await expect(panel.getByRole('button', { name: 'Close' })).toBeFocused();
      await page.setViewportSize({ width: 1440, height: 900 });
      await expect(panel).not.toHaveClass(/sheet/);
      await expect(async () => {
        const at = await page.evaluate(() => {
          const el = document.activeElement;
          return {
            inPanel: el?.closest('#voice-options') != null,
            enabled: el instanceof HTMLElement && !el.matches(':disabled'),
            drawn: el instanceof HTMLElement && (el.offsetParent !== null || el.id === 'voice-options'),
            close: el?.classList.contains('voice-options-close') === true,
          };
        });
        expect(at, 'focus is on an enabled, drawn control in the panel').toEqual({ inPanel: true, enabled: true, drawn: true, close: false });
      }).toPass({ timeout: 5000 });
    } finally {
      await p?.close();
    }
  });

  // The share quality menu (z-index 21) paints over the panel, so the two are
  // never open together: opening the menu closes the panel, and the panel
  // opens with the menu closed, by pointer and by keyboard.
  test('P2189 Voice options and the share quality menu are not open together', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const strip = voice(page);
      const options = strip.getByRole('button', { name: 'Voice options' });
      const chooser = strip.getByRole('button', { name: 'Share quality' });
      const menu = strip.getByRole('menu', { name: 'Share quality' });
      const panel = page.locator('#voice-options');

      // Pointer, both ways.
      await options.click();
      await expect(panel).toBeVisible();
      await chooser.click();
      await expect(menu).toBeVisible();
      await expect(panel).toBeHidden();
      await options.click();
      await expect(panel).toBeVisible();
      await expect(menu).toBeHidden();

      // Keyboard, both ways.
      await page.keyboard.press('Escape');
      await expect(panel).toBeHidden();
      await options.focus();
      await page.keyboard.press('Enter');
      await expect(panel).toBeVisible();
      await chooser.focus();
      await expect(panel).toBeHidden();
      await page.keyboard.press('Enter');
      await expect(menu).toBeVisible();
      await options.focus();
      await expect(menu).toBeHidden();
      await page.keyboard.press('Enter');
      await expect(panel).toBeVisible();
      await expect(menu).toBeHidden();
    } finally {
      await p?.close();
    }
  });

  test('P2189 Escape closes it and returns focus to its button; a press outside closes it; the button toggles it', async ({ browser }) => {
    let p: Someone | undefined;
    try {
      p = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
      const page = p.page;
      await joinVoice(p, 'Join voice');
      const button = voice(page).getByRole('button', { name: 'Voice options' });
      const panel = page.locator('#voice-options');

      await button.click();
      await expect(panel).toBeVisible();
      await panel.getByRole('radio', { name: 'Open microphone' }).focus();
      await page.keyboard.press('Escape');
      await expect(panel).toBeHidden();
      await expect(button).toHaveAttribute('aria-expanded', 'false');
      await expect(button).toBeFocused();

      await button.click();
      await expect(panel).toBeVisible();
      // The panel can cover the conversation; press the strip's own state line.
      await voice(page).locator('.voice-phase').click();
      await expect(panel).toBeHidden();

      await button.click();
      await expect(panel).toBeVisible();
      await button.click();
      await expect(panel).toBeHidden();
    } finally {
      await p?.close();
    }
  });
});
