// The persistent call workspace (issue #1498): text navigation changes no
// voice state, compact is a docked in-memory layout, and cameras, speakers,
// share ends and voice removal never change that layout by themselves. The
// same machine supplies fake microphone, camera and screen devices through
// the pinned LiveKit of voice-world.ts; these are not real-device tests.
import type { Browser, BrowserContextOptions, Locator, Page } from '@playwright/test';
import { secretLines, withoutJoinTokenInSocketRefusals, type BrowserLogEntry } from './console-lines';
import { channelButton, drawDesktopStrip, expect, openChannel, showView, test } from './fixtures';
import { enter, inbound as inboundStats, joinVoice, leave, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type Member, type VoiceWorld } from './voice-world';

// A person is people.ts's Someone: its pages keep every peer connection (for
// inbound and kept) and are watched.
type Person = Someone;

const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });
const call = (page: Page) => page.getByRole('region', { name: 'Video call' });
const shareView = (page: Page) => page.getByRole('region', { name: 'Shared screen' });
const tile = (page: Page, name: string): Locator => page.locator('.call-share').filter({ hasText: `${name} · LIVE` });

// inbound is the page's total decoded video frames from every open connection.
const inbound = async (page: Page): Promise<number> => (await inboundStats(page)).frames;

// kept is whether the originally watched element is still the one on screen,
// and how many peer connections this page has made.
async function kept(page: Page): Promise<{ readonly same: boolean; readonly connections: number }> {
  return page.evaluate(() => {
    const w = window as unknown as { __pcs: RTCPeerConnection[]; __kept?: Element | null };
    const video = document.querySelector('[aria-label="Shared screen"] video');
    w.__kept ??= video;
    return { same: video !== null && w.__kept === video, connections: w.__pcs.length };
  });
}

// keptCamera is the same identity check for a received camera across layout
// toggles. Camera off/on deliberately replaces it, so check only across toggles.
async function keptCamera(page: Page): Promise<boolean> {
  return page.evaluate(() => {
    const w = window as unknown as { __keptCamera?: Element | null };
    const video = document.querySelector('[aria-label="carol\'s camera"]');
    w.__keptCamera ??= video;
    return video !== null && w.__keptCamera === video;
  });
}

// voiceSnapshot reads what the strip exposes of VoiceState: phase and channel,
// mute, deafen, push-to-talk, camera and share, not the viewed text channel.
async function voiceSnapshot(page: Page) {
  const strip = voice(page);
  return {
    phase: await strip.locator('.voice-phase').textContent(),
    transmit: await strip.locator('.voice-bar .voice-tx').textContent(),
    muted: await strip.getByRole('button', { name: 'Mute', exact: true }).getAttribute('aria-pressed'),
    deafened: await strip.getByRole('button', { name: 'Deafen', exact: true }).getAttribute('aria-pressed'),
    pushToTalk: await strip.getByRole('button', { name: 'Open mic', exact: true }).getAttribute('aria-pressed'),
    camera: await strip.getByRole('button', { name: 'Camera', exact: true }).getAttribute('aria-pressed'),
    share: await strip.getByRole('button', { name: 'Share screen', exact: true }).textContent(),
  };
}

// expectComposerShown checks the composer as the page is, without scrolling:
// its field and Send are wholly on screen and nothing covers either centre.
async function expectComposerShown(page: Page): Promise<void> {
  const size = page.viewportSize();
  if (size === null) throw new Error('no viewport');
  for (const element of [page.getByLabel('Message #voice-room'), page.getByRole('button', { name: 'Send' })]) {
    const box = await element.boundingBox();
    if (box === null) throw new Error('the composer has no box');
    expect(box.y).toBeGreaterThanOrEqual(0);
    expect(box.y + box.height).toBeLessThanOrEqual(size.height);
    const reached = await element.evaluate((el) => {
      const r = el.getBoundingClientRect();
      const at = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return at !== null && (at === el || el.contains(at));
    });
    expect(reached, 'nothing covers the composer').toBe(true);
  }
}

// openWorkspaceChannel also opens the archived group, whose rows are collapsed
// until asked for; the ordinary openChannel helper expects a visible row.
async function openWorkspaceChannel(page: Page, name: string): Promise<void> {
  if (name === 'workspace-archived' && !(await channelButton(page, name).isVisible())) {
    await showView(page, 'Channels');
    await page.getByRole('button', { name: /^Archived \(\d+\)$/ }).click();
  }
  await openChannel(page, name);
}

test.describe('the persistent call workspace', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the call workspace suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the call workspace suite needs it');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
    // Carol's camera scenario needs the member grant, as spec 45 sets it.
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
    for (const [name, archived] of [['workspace-text', false], ['workspace-archived', true]] as const) {
      const made = await vw.api(vw.operator, 'POST', '/v0/channels', { name });
      expect(made.status, made.text).toBe(201);
      const id = (JSON.parse(made.text) as { channel: { id: number } }).channel.id;
      // Membership puts the channel directly in each person's navigation.
      for (const member of [vw.alice, vw.bob, vw.carol]) {
        const added = await vw.api(vw.operator, 'PUT', `/v1/channels/${name}/members/${String(member.id)}`);
        expect(added.status, added.text).toBe(204);
      }
      if (!archived) continue;
      const putArchived = await vw.api(vw.operator, 'PUT', `/v1/channels/${String(id)}/settings`, { archived: true });
      expect(putArchived.status, putArchived.text).toBe(200);
    }
  });

  test.afterAll(() => {
    stopWorld?.();
  });

  // The member cell of use_camera, restored after every revocation check.
  async function memberCamera(on: boolean): Promise<void> {
    const read = await vw.api(vw.operator, 'GET', '/v1/permissions');
    expect(read.status).toBe(200);
    const { revision } = JSON.parse(read.text) as { revision: number };
    const changes = on
      ? [
          { permission: 'use_camera', role: 'operator', granted: true },
          { permission: 'use_camera', role: 'member', granted: true },
        ]
      : [{ permission: 'use_camera', role: 'member', granted: false }];
    const put = await vw.api(vw.operator, 'PUT', '/v1/permissions', { revision, changes });
    expect(put.status, put.text).toBe(200);
  }

  const person = (browser: Browser, member: Member, options?: BrowserContextOptions, before?: (page: Page) => Promise<void>): Promise<Person> =>
    enter(browser, vw, member, { ...(options === undefined ? {} : { context: options }), ...(before === undefined ? {} : { before }) });

  const join = (p: Person): Promise<void> => joinVoice(p, 'Join voice');

  async function share(p: Person): Promise<void> {
    await voice(p.page).getByRole('button', { name: 'Share screen' }).click();
    await expect(voice(p.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
  }

  async function watching(p: Person): Promise<void> {
    await tile(p.page, 'alice').getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
    await expect(shareView(p.page)).toContainText('alice · LIVE');
    await expect.poll(() => inbound(p.page), { timeout: 30_000 }).toBeGreaterThan(10);
    const before = await inbound(p.page);
    await expect.poll(() => inbound(p.page), { timeout: 15_000 }).toBeGreaterThan(before);
  }

  async function camera(p: Person, on: boolean): Promise<void> {
    const button = voice(p.page).getByRole('button', { name: 'Camera', exact: true });
    await expect(button).toHaveAttribute('aria-pressed', String(!on));
    await button.click();
    if (on) await expect(voice(p.page)).toContainText('Sending video', { timeout: 30_000 });
    else await expect(voice(p.page)).not.toContainText('Sending video', { timeout: 30_000 });
    await expect(button).toHaveAttribute('aria-pressed', String(on));
  }

  async function expectClean(p: Person, renewalLog: readonly BrowserLogEntry[] = []): Promise<void> {
    await p.watch.settled();
    const lines = withoutJoinTokenInSocketRefusals(p.watch.console, renewalLog, vw.livekitOrigin);
    expect(secretLines(lines, p.watch.browserLog, vw.secrets), 'a secret in the console').toEqual([]);
    // Only lines whose token the shared matcher removed can skip the error check.
    const errors = lines.filter((line, i) => line === p.watch.console[i] && /^(error|pageerror): /.test(line));
    expect(errors, 'no console error').toEqual([]);
    expect(p.watch.console.filter((line) => /Content Security Policy|Refused to|[Pp]ermissions policy/.test(line))).toEqual([]);
    expect(p.watch.policy, 'Permissions-Policy reports').toEqual([]);
    expect(p.watch.failed, 'requests to conchd that failed').toEqual([]);
  }

  // The share's own <audio> refuses to play until window.__blockShareSound is
  // cleared, as a phone's autoplay rule does: ShareView then offers "Tap to
  // play sound".
  const blockShareSound = async (page: Page): Promise<void> => {
    await page.addInitScript(() => {
      const w = window as unknown as { __blockShareSound?: boolean };
      w.__blockShareSound = true;
      // The original method is called back with the element it was called on.
      // eslint-disable-next-line @typescript-eslint/unbound-method
      const play = HTMLMediaElement.prototype.play;
      HTMLMediaElement.prototype.play = function (this: HTMLMediaElement) {
        if (w.__blockShareSound === true && this instanceof HTMLAudioElement && (this.getAttribute('aria-label') ?? '').endsWith('stream audio')) {
          return Promise.reject(new DOMException('blocked for the test', 'NotAllowedError'));
        }
        return play.call(this);
      };
    });
  };
  // A pointer over the share shows its controls (they fade after a pause).
  const wake = async (page: Page): Promise<void> => {
    const box = await shareView(page).locator('.share-frame').boundingBox();
    if (box === null) throw new Error('no share frame');
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 3);
  };
  const controlsOpacity = (page: Page): Promise<string> => shareView(page).locator('.share-controls').evaluate((el) => getComputedStyle(el).opacity);

  // Read the layout, not pixels. The stage keeps the size it always had. In
  // the open call view the share's controls are icon buttons on the picture's
  // bottom edge, like a video player's, so the frame takes what the view
  // leaves under the title (6rem at the least) and the video is contained in
  // it. In the closed phone Chat stage the controls stay in a row below.
  const checkShare = async (page: Page, where: string, opts: { log?: boolean; open?: boolean } = {}) => {
    const open = opts.open ?? true;
    const g = await shareView(page).evaluate((share) => {
      const stage = share.closest('.stage');
      const frame = share.querySelector('.share-frame');
      const video = frame?.querySelector('video');
      const controls = share.querySelector('.share-controls');
      const title = share.querySelector('.share-title');
      if (stage === null || frame === null || video == null || controls === null || title === null) throw new Error('no share layout boxes');
      const view = share.closest('.call-view') ?? stage;
      const rect = (element: Element) => element.getBoundingClientRect().toJSON() as DOMRectInit;
      const within = (element: Element, outer: Element) => {
        const box = element.getBoundingClientRect();
        const bounds = outer.getBoundingClientRect();
        return box.width > 0 && box.height > 0 && box.left >= bounds.left - 1 && box.right <= bounds.right + 1 &&
          box.top >= bounds.top - 1 && box.bottom <= bounds.bottom + 1;
      };
      const style = getComputedStyle(view);
      const content = view.getBoundingClientRect().bottom - parseFloat(style.paddingBottom) - parseFloat(style.borderBottomWidth);
      const box = controls.getBoundingClientRect();
      // One line: every drawn control starts at the same height.
      const tops = [...controls.querySelectorAll('button, input')].map((e) => e.getBoundingClientRect()).filter((r) => r.width > 0).map((r) => Math.round(r.top));
      return {
        stage: rect(stage), view: rect(view), frame: rect(frame), video: rect(video), controls: rect(controls),
        floor: 6 * parseFloat(getComputedStyle(document.documentElement).fontSize),
        fit: getComputedStyle(video).objectFit,
        scrolls: view.scrollHeight > view.clientHeight + 1,
        videoInStage: within(video, stage) && within(video, view),
        controlsInFrame: within(controls, frame),
        controlsInStage: within(controls, stage) && within(controls, view),
        controlsInWindow: box.left >= -1 && box.top >= -1 && box.right <= window.innerWidth + 1 && box.bottom <= window.innerHeight + 1,
        oneLine: tops.length > 0 && Math.max(...tops) - Math.min(...tops) <= 2,
        visibleLabels: [...controls.querySelectorAll('.label')].filter((e) => e.getBoundingClientRect().width > 2).length,
        // Under the frame: what the view's content leaves unused.
        slack: content - frame.getBoundingClientRect().bottom,
        // What the view has under the title (the call head is above it, so is
        // not taken off again).
        room: content - title.getBoundingClientRect().bottom,
        log: document.querySelector('[role="log"]')?.getBoundingClientRect().height ?? 0,
      };
    });
    const details = JSON.stringify(g);
    expect.soft(g.fit, where).toBe('contain');
    expect.soft(g.video, `${where}: the video fills its frame\n${details}`).toEqual(g.frame);
    expect.soft(g.frame.height, `${where}: the frame keeps its floor\n${details}`).toBeGreaterThanOrEqual(g.floor - 1);
    expect.soft(g.videoInStage, `${where}: the video is inside the stage\n${details}`).toBe(true);
    expect.soft(g.controlsInWindow, `${where}: the controls are inside the window\n${details}`).toBe(true);
    if (open) {
      expect.soft(g.scrolls, `${where}: the call view needs no scrolling\n${details}`).toBe(false);
      expect.soft(g.controlsInFrame, `${where}: the controls are inside the frame\n${details}`).toBe(true);
      expect.soft(g.oneLine, `${where}: the controls are one line\n${details}`).toBe(true);
      expect.soft(g.controls.height, `${where}: the controls cover at most 64 px\n${details}`).toBeLessThanOrEqual(64);
      expect.soft(g.visibleLabels, `${where}: the controls are icons, their names read out\n${details}`).toBe(0);
      // At its floor the frame may use the view's bottom padding (8 px);
      // above it, it takes exactly what the view leaves.
      const atFloor = (g.frame.height ?? 0) <= g.floor + 1;
      expect.soft(g.slack <= 3 && g.slack >= (atFloor ? -9 : -3), `${where}: the frame fills the stage below the title\n${details}`).toBe(true);
    } else {
      expect.soft(g.controlsInStage, `${where}: the controls are inside the stage\n${details}`).toBe(true);
    }
    if (opts.log === true) expect.soft(g.log, `${where}: the message log keeps its 6rem floor\n${details}`).toBeGreaterThanOrEqual(95);
    return g;
  };
  // Each control, the pop-out and the focus button take a real pointer: a
  // trial click checks the point it would press is its own, not covered.
  const clickable = async (page: Page) => {
    await wake(page);
    const view = shareView(page);
    for (const control of [
      view.getByRole('button', { name: /^(Mute|Unmute) share$/ }),
      view.getByRole('slider', { name: 'Share volume' }),
      view.getByRole('button', { name: 'Full screen' }),
      view.getByRole('button', { name: 'Stop watching' }),
      view.getByRole('button', { name: 'Pop out' }),
    ]) {
      await expect(control).toBeVisible();
      await control.click({ trial: true, timeout: 5000 });
    }
    await view.getByRole('button', { name: /^Focus .*screen$/ }).click({ trial: true, position: { x: 8, y: 8 }, timeout: 5000 });
  };

  test('P2187 a watched share fills its stage, with its controls over the picture', async ({ browser }) => {
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const b = await person(browser, vw.bob, { viewport: { width: 1215, height: 900 } });
      people.push(b);
      const c = await person(browser, vw.carol, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
      people.push(c);
      await drawDesktopStrip(b.page);
      await join(a);
      await join(b);
      await join(c);
      await share(a);

      await watching(b);
      const strip = await checkShare(b.page, '1215×900 under the desktop strip', { log: true });
      // The picture takes at least 85% of what the stage has under the title.
      expect.soft(strip.frame.height, `the picture under the strip, of ${String(strip.room)} px\n${JSON.stringify(strip)}`).toBeGreaterThanOrEqual(0.85 * strip.room);
      await clickable(b.page);
      await b.page.evaluate(() => { document.querySelector('.desktop-bar')?.remove(); });
      for (const [width, height] of [[1440, 900], [1920, 1080], [800, 700], [768, 700], [720, 700]] as const) {
        await b.page.setViewportSize({ width, height });
        await checkShare(b.page, `${String(width)}×${String(height)} browser`, { log: true });
        if (width < 1000) await clickable(b.page);
      }

      await watching(c);
      // The open view's picture has its focus button; the closed Chat stage's
      // has none (the empty box drawn over the video on main).
      await expect.soft(shareView(c.page).locator('.call-enlarge')).toHaveCount(1);
      for (const [width, height] of [[390, 844], [360, 740], [360, 640]] as const) {
        await c.page.setViewportSize({ width, height });
        await checkShare(c.page, `${String(width)}×${String(height)} Voice`);
        await clickable(c.page);
      }
      await c.page.setViewportSize({ width: 390, height: 844 });
      await showView(c.page, 'Chat');
      await expect(c.page.locator('.call-stage')).toHaveCount(1);
      await checkShare(c.page, '390×844 Chat', { open: false });
      await expect.soft(shareView(c.page).locator('.call-enlarge')).toHaveCount(0);
    } finally {
      for (const p of people) await leave(p);
    }
  });

  test('P2187 the controls fade after a pause and return on a move, a tap or focus', async ({ browser }) => {
    test.setTimeout(120_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const b = await person(browser, vw.bob, { viewport: { width: 1440, height: 900 } });
      people.push(b);
      const c = await person(browser, vw.carol, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
      people.push(c);
      await join(a);
      await join(b);
      await join(c);
      await share(a);
      await watching(b);
      await watching(c);

      // Desktop: shown at first, hidden after about 3 s (opacity 0, still in
      // the tab order), back on a pointer move.
      await wake(b.page);
      await expect.poll(() => controlsOpacity(b.page)).toBe('1');
      await expect.poll(() => controlsOpacity(b.page), { timeout: 10_000 }).toBe('0');
      await expect(shareView(b.page).getByRole('button', { name: 'Full screen' })).toHaveCount(1);
      await wake(b.page);
      await b.page.mouse.move(300, 400);
      await wake(b.page);
      await expect.poll(() => controlsOpacity(b.page)).toBe('1');
      // Tab into them: the row is shown and stays while focus is in it.
      await expect.poll(() => controlsOpacity(b.page), { timeout: 10_000 }).toBe('0');
      await b.page.mouse.move(2, 2);
      await shareView(b.page).getByRole('button', { name: /^Focus .*screen$/ }).focus();
      for (let i = 0; i < 6; i++) {
        await b.page.keyboard.press('Tab');
        if (await shareView(b.page).locator('.share-controls :focus').count() > 0) break;
      }
      await expect(shareView(b.page).locator('.share-controls :focus')).toHaveCount(1);
      await expect.poll(() => controlsOpacity(b.page)).toBe('1');
      await b.page.waitForTimeout(4500);
      expect(await controlsOpacity(b.page), 'still shown after 4.5 s with focus inside').toBe('1');
      await b.page.keyboard.press('Shift+Tab');
      await b.page.keyboard.press('Shift+Tab');
      await b.page.keyboard.press('Shift+Tab');
      await expect.poll(() => controlsOpacity(b.page), { timeout: 10_000 }).toBe('0');

      // Phone: a tap on the picture shows them.
      await expect.poll(() => controlsOpacity(c.page), { timeout: 10_000 }).toBe('0');
      const frame = await shareView(c.page).locator('.share-frame').boundingBox();
      if (frame === null) throw new Error('no share frame');
      await c.page.touchscreen.tap(frame.x + frame.width / 2, frame.y + frame.height / 4);
      await expect.poll(() => controlsOpacity(c.page)).toBe('1');
    } finally {
      for (const p of people) await leave(p);
    }
  });

  // The hit target at the centre of each control: the control (or something in
  // it), not the picture's focus button over the whole frame.
  const hitTargets = (page: Page) => shareView(page).evaluate((share) => {
    const out: { name: string; own: boolean; enlarge: boolean; row: boolean }[] = [];
    for (const el of share.querySelectorAll<HTMLElement>('.share-controls button, .share-controls input, .share-frame .share-pip')) {
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) continue;
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      out.push({
        name: el.getAttribute('aria-label') ?? el.className,
        own: hit !== null && el.contains(hit),
        enlarge: hit?.closest('.call-enlarge') != null,
        row: el.closest('.share-controls') !== null,
      });
    }
    return out;
  });

  test('P2187 every control on the picture is its own hit target when shown, and the picture\'s when faded', async ({ browser }) => {
    test.setTimeout(120_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const b = await person(browser, vw.bob, { viewport: { width: 1440, height: 900 } });
      people.push(b);
      const c = await person(browser, vw.carol, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
      people.push(c);
      await join(a);
      await join(b);
      await join(c);
      await share(a);
      await watching(b);
      await watching(c);

      for (const [p, sizes] of [[b, [[1440, 900], [800, 700]]], [c, [[390, 844], [360, 640]]]] as const) {
        for (const [width, height] of sizes) {
          await p.page.setViewportSize({ width, height });
          const where = `${String(width)}×${String(height)}`;
          await wake(p.page);
          await expect.poll(() => controlsOpacity(p.page), { timeout: 10_000 }).toBe('1');
          const shown = await hitTargets(p.page);
          expect(shown.length, `${where}: the controls are there`).toBeGreaterThanOrEqual(3);
          expect(shown.filter((t) => !t.own), `${where}: shown, each control is what a press at its centre reaches`).toEqual([]);
          // Faded, nothing invisible in the row takes a press: it is the
          // picture's, which a tap there also wakes the controls with (the
          // pop-out is not in the row and stays).
          await expect.poll(() => controlsOpacity(p.page), { timeout: 10_000 }).toBe('0');
          const faded = await hitTargets(p.page);
          expect(faded.filter((t) => t.row && (t.own || !t.enlarge)), `${where}: faded, a press at a control reaches the picture`).toEqual([]);
        }
      }
    } finally {
      for (const p of people) await leave(p);
    }
  });

  const faded = (page: Page) => expect.poll(() => controlsOpacity(page), { timeout: 10_000 });

  test('P2187 on touch a tap on a faded control only brings the row back; a second tap presses it', async ({ browser }) => {
    test.setTimeout(240_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const phone = await person(browser, vw.carol, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 });
      people.push(phone);
      const tablet = await person(browser, vw.bob, { viewport: { width: 1024, height: 768 }, hasTouch: true });
      people.push(tablet);
      await join(a);
      await join(phone);
      await join(tablet);
      await share(a);
      await watching(phone);
      await watching(tablet);

      for (const p of [phone, tablet]) {
        const { page } = p;
        const view = shareView(page);
        const where = page.viewportSize()?.width === 390 ? '390×844' : '1024×768';
        const centre = async (control: Locator): Promise<[number, number]> => {
          const box = await control.boundingBox();
          if (box === null) throw new Error('control not drawn');
          return [box.x + box.width / 2, box.y + box.height / 2];
        };
        const mute = view.getByRole('button', { name: /^(Mute|Unmute) share$/ });
        const slider = view.getByRole('slider', { name: 'Share volume' });
        const full = view.getByRole('button', { name: 'Full screen' });
        const stop = view.getByRole('button', { name: 'Stop watching' });
        // The state each control would change.
        const state = (): Promise<string> => page.evaluate(() => {
          const v = document.querySelector('section[aria-label="Shared screen"]');
          const m = v?.querySelector('.share-controls button[aria-pressed]')?.getAttribute('aria-pressed') ?? '';
          const r = v?.querySelector<HTMLInputElement>('.share-controls input[type="range"]')?.value ?? '';
          return `${m}|${r}|${String(document.fullscreenElement !== null)}|${String(v !== null)}`;
        });
        for (const [name, control] of [['Mute', mute], ['volume', slider], ['Full screen', full], ['Stop watching', stop]] as const) {
          await faded(page).toBe('0');
          const before = await state();
          const [x, y] = await centre(control);
          await page.touchscreen.tap(x, y);
          await page.waitForTimeout(700);
          expect(await state(), `${where}: the first tap on the faded ${name} presses nothing`).toBe(before);
          await expect.poll(() => controlsOpacity(page), { message: `${where}: and brings the row back` }).toBe('1');
          await page.touchscreen.tap(x, y);
          await page.waitForTimeout(700);
          if (name === 'Stop watching') await expect(view, `${where}: the second tap stops watching`).toHaveCount(0);
          else if (name === 'Full screen') {
            // Headless may refuse the request; the tap must still reach the button.
            await page.evaluate(() => { void document.exitFullscreen().catch(() => undefined); });
          } else expect(await state(), `${where}: the second tap presses ${name}`).not.toBe(before);
          await page.waitForTimeout(300);
        }
      }
    } finally {
      for (const p of people) await leave(p);
    }
  });

  test('P2187 after a pointer press on a control the row still fades; keyboard focus holds it', async ({ browser }) => {
    test.setTimeout(120_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const b = await person(browser, vw.bob, { viewport: { width: 1440, height: 900 } });
      people.push(b);
      await join(a);
      await join(b);
      await share(a);
      await watching(b);
      const view = shareView(b.page);
      await wake(b.page);
      await expect.poll(() => controlsOpacity(b.page)).toBe('1');
      for (const control of [view.getByRole('button', { name: /^(Mute|Unmute) share$/ }), view.getByRole('slider', { name: 'Share volume' })]) {
        await wake(b.page);
        await control.click();
        await b.page.mouse.move(2, 2);
        await b.page.waitForTimeout(500);
        // The control still has focus, from the press, and the row fades anyway.
        await expect(control).toBeFocused();
        await faded(b.page).toBe('0');
        await wake(b.page);
      }
    } finally {
      for (const p of people) await leave(p);
    }
  });

  // Where "Tap to play sound" is, with the audio refused: wholly inside its
  // picture (a frame under 44 px tall gives it a smaller pill) and what a press
  // at its centre reaches.
  const tapGeometry = async (page: Page, where: string): Promise<void> => {
    await page.waitForTimeout(300);
    const taps = page.getByRole('button', { name: 'Tap to play sound' });
    await expect(taps.first(), where).toBeVisible({ timeout: 15_000 });
    for (let i = 0; i < await taps.count(); i += 1) {
      const tap = taps.nth(i);
      await tap.scrollIntoViewIfNeeded();
      const g = await tap.evaluate((el) => {
        const frame = el.closest('.share-frame');
        if (frame === null) throw new Error('the tap is not on a picture');
        const r = el.getBoundingClientRect();
        const f = frame.getBoundingClientRect();
        const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
        return {
          inFrame: r.left >= f.left - 1 && r.right <= f.right + 1 && r.top >= f.top - 1 && r.bottom <= f.bottom + 1,
          own: hit !== null && el.contains(hit),
          tall: r.height >= 44 - 1 || f.height < 44 + 4,
          size: [Math.round(r.width), Math.round(r.height), Math.round(f.width), Math.round(f.height)],
        };
      });
      const details = `${where}: ${JSON.stringify(g)}`;
      expect.soft(g.inFrame, `the tap is inside its picture, ${details}`).toBe(true);
      expect.soft(g.own, `a press at the tap's centre reaches it, ${details}`).toBe(true);
      expect.soft(g.tall, `the tap is a full target where the picture has room, ${details}`).toBe(true);
    }
  };
  // A real press starts the sound: the page lets its audio play, and the tap goes.
  const pressTap = async (page: Page, where: string, touch: boolean): Promise<void> => {
    await page.evaluate(() => { (window as unknown as { __blockShareSound?: boolean }).__blockShareSound = false; });
    const tap = page.getByRole('button', { name: 'Tap to play sound' }).first();
    await tap.scrollIntoViewIfNeeded();
    const box = await tap.boundingBox();
    if (box === null) throw new Error('no tap');
    const count = await page.getByRole('button', { name: 'Tap to play sound' }).count();
    if (touch) await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
    else await page.mouse.click(box.x + box.width / 2, box.y + box.height / 2);
    await expect(page.getByRole('button', { name: 'Tap to play sound' }), `${where}: a press starts the sound`).toHaveCount(count - 1);
  };
  const reblock = async (page: Page, name: string): Promise<void> => {
    await page.evaluate(() => { (window as unknown as { __blockShareSound?: boolean }).__blockShareSound = true; });
    await page.evaluate(() => { document.querySelector<HTMLElement>('.share-controls .stop')?.click(); });
    await tile(page, name).getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
  };

  test('P2187 the tap to play sound is whole, on the picture and pressable in every layout, on a desktop', async ({ browser }) => {
    test.setTimeout(300_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const b = await person(browser, vw.bob, { viewport: { width: 1440, height: 900 } }, blockShareSound);
      people.push(b);
      const c = await person(browser, vw.carol);
      people.push(c);
      await join(a);
      await join(b);
      await join(c);
      await share(a);
      await watching(b);
      const page = b.page;
      for (const [w, h] of [[1440, 900], [1000, 540], [1280, 560]] as const) {
        await page.setViewportSize({ width: w, height: h });
        await tapGeometry(page, `single share ${String(w)}×${String(h)}`);
      }
      await pressTap(page, 'single share 1280×560', false);

      // A second stream: a grid of two, each with its own tap.
      await page.setViewportSize({ width: 1440, height: 900 });
      await share(c);
      await expect(page.locator('.call-share')).toHaveCount(2, { timeout: 30_000 });
      await reblock(page, 'alice');
      await tile(page, 'carol').getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
      for (const [w, h] of [[1440, 900], [1024, 768], [1920, 1080]] as const) {
        await page.setViewportSize({ width: w, height: h });
        await tapGeometry(page, `grid of two ${String(w)}×${String(h)}`);
      }

      // Focused: the enlarged picture and the strip.
      await page.setViewportSize({ width: 1440, height: 900 });
      // Its corner: the middle of the picture is the tap's.
      await page.locator('.call-enlarge').first().click({ position: { x: 8, y: 8 } });
      await expect(page.locator('ul.call-grid.focus')).toHaveCount(1);
      await tapGeometry(page, 'focused with a strip 1440×900');
      await pressTap(page, 'focused with a strip 1440×900', false);
    } finally {
      for (const p of people) await leave(p);
    }
  });

  test('P2187 the tap to play sound is whole, on the picture and pressable in every layout, on a phone', async ({ browser }) => {
    test.setTimeout(240_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const c = await person(browser, vw.carol, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }, blockShareSound);
      people.push(c);
      await join(a);
      await join(c);
      await share(a);
      await watching(c);
      const page = c.page;
      for (const [w, h] of [[390, 844], [360, 640]] as const) {
        await page.setViewportSize({ width: w, height: h });
        await tapGeometry(page, `phone Voice ${String(w)}×${String(h)}`);
      }
      const stopWidth = () => shareView(page).locator('.share-controls .stop .label').evaluate((el) => el.getBoundingClientRect().width);
      // Icons only on the picture; the words back where the row is below it.
      expect(await stopWidth(), 'phone Voice: Stop watching is an icon, its name read out').toBeLessThanOrEqual(2);
      await page.setViewportSize({ width: 390, height: 844 });
      await showView(page, 'Chat');
      expect(await stopWidth(), 'phone Chat: Stop watching says so').toBeGreaterThan(40);
      for (const [w, h] of [[390, 844], [360, 740], [360, 640]] as const) {
        await page.setViewportSize({ width: w, height: h });
        await tapGeometry(page, `phone Chat ${String(w)}×${String(h)}`);
      }
      // Held sideways: wide enough for the desktop layout, 390 px tall.
      await page.setViewportSize({ width: 844, height: 390 });
      await tapGeometry(page, 'phone held sideways 844×390');
      await page.setViewportSize({ width: 360, height: 740 });
      await pressTap(page, 'phone Chat 360×740', true);
    } finally {
      for (const p of people) await leave(p);
    }
  });

  test('P2187 the tap to play sound is the picture\'s, the row stays one line and the waiting hint is not covered', async ({ browser }) => {
    test.setTimeout(120_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      people.push(a);
      const b = await person(browser, vw.bob, { viewport: { width: 1440, height: 900 } }, blockShareSound);
      people.push(b);
      const c = await person(browser, vw.carol, { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 }, blockShareSound);
      people.push(c);
      await join(a);
      await join(b);
      await join(c);
      await share(a);
      await watching(b);
      await watching(c);

      for (const [p, sizes] of [[b, [[1440, 900], [800, 700]]], [c, [[390, 844], [360, 640]]]] as const) {
        for (const [width, height] of sizes) {
          await p.page.setViewportSize({ width, height });
          const where = `${String(width)}×${String(height)} with the sound waiting for a tap`;
          const tap = shareView(p.page).getByRole('button', { name: 'Tap to play sound' });
          await expect(tap, where).toBeVisible({ timeout: 15_000 });
          await tap.click({ trial: true });
          const g = await checkShare(p.page, where);
          expect.soft(g.oneLine, `${where}: the row is still one line`).toBe(true);
          // The tap is on the picture and not in the control row.
          const inRow = await shareView(p.page).locator('.share-controls').getByRole('button', { name: 'Tap to play sound' }).count();
          expect.soft(inRow, `${where}: the tap is not among the controls`).toBe(0);
          // While it waits the controls stay shown, however long the pause.
          await p.page.mouse.move(2, 2);
          await p.page.waitForTimeout(4500);
          expect.soft(await controlsOpacity(p.page), `${where}: the controls stay shown`).toBe('1');
        }
      }

      // The waiting hint ("Waiting for the picture", "Waiting for the stream
      // to come back") is drawn when the picture is not playing, a state
      // LiveKit does not hold still: so the page's own hint is drawn here as
      // ShareView draws it (same element and class), and must clear the row.
      for (const [p, sizes] of [[b, [[1440, 900], [720, 700]]], [c, [[390, 844], [360, 640]]]] as const) {
        for (const [width, height] of sizes) {
          await p.page.setViewportSize({ width, height });
          const where = `${String(width)}×${String(height)} waiting hint`;
          const g = await shareView(p.page).evaluate((share) => {
            const frame = share.querySelector('.share-frame');
            const controls = share.querySelector('.share-controls');
            if (frame === null || controls === null) throw new Error('no frame');
            const hint = document.createElement('p');
            hint.className = 'hint';
            hint.textContent = 'Waiting for the stream to come back';
            frame.append(hint);
            const h = hint.getBoundingClientRect();
            const c = controls.getBoundingClientRect();
            const f = frame.getBoundingClientRect();
            hint.remove();
            return { hintBottom: h.bottom, controlsTop: c.top, inFrame: h.top >= f.top - 1 && h.bottom <= f.bottom + 1 };
          });
          const details = JSON.stringify(g);
          expect.soft(g.inFrame, `${where}: the hint is inside the frame\n${details}`).toBe(true);
          expect.soft(g.hintBottom, `${where}: the hint is above the controls\n${details}`).toBeLessThanOrEqual(g.controlsTop + 1);
        }
      }
    } finally {
      for (const p of people) await leave(p);
    }
  });

  test('all six call workspace scenarios', async ({ browser }) => {
    test.setTimeout(180_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      const b = await person(browser, vw.bob, { viewport: { width: 1440, height: 900 } });
      const c = await person(browser, vw.carol);
      people.push(a, b, c);
      await join(a);
      await join(b);
      await join(c);

      // 1. Five text-channel switches include a channel with no voice, an
      // archived channel and the way back. Voice and the media identity hold.
      await share(a);
      await watching(b);
      const before = await kept(b.page);
      const voiceBefore = await voiceSnapshot(b.page);
      expect(before.same).toBe(true);
      for (const channel of ['workspace-text', 'voice-room', 'workspace-archived', 'workspace-text', 'voice-room']) {
        await openWorkspaceChannel(b.page, channel);
        await expect(channelButton(b.page, channel)).toHaveAttribute('aria-current', 'true');
        expect(await voiceSnapshot(b.page)).toEqual(voiceBefore);
        expect(await kept(b.page)).toEqual(before);
      }
      const frames = await inbound(b.page);
      await expect.poll(() => inbound(b.page), { timeout: 15_000 }).toBeGreaterThan(frames);

      // 2. Compact is docked, leaves at least 60% of the centre for the log,
      // names the call and sharer, opens the voice channel, and expands in place.
      await openChannel(b.page, 'workspace-text');
      const compact = call(b.page).getByRole('button', { name: 'Compact', exact: true });
      await compact.focus();
      await b.page.keyboard.press('Enter');
      const expand = call(b.page).getByRole('button', { name: 'Expand', exact: true });
      await expect(expand).toBeFocused();
      await expect(expand).toHaveAttribute('aria-pressed', 'true');
      await expect(call(b.page).locator('.call-context')).toContainText('#voice-room');
      await expect(call(b.page).locator('.call-context')).toContainText('Sharing: alice');
      await b.page.setViewportSize({ width: 1280, height: 800 });
      const room = await b.page.locator('.centre').evaluate((centre) => {
        const log = centre.querySelector('.log-wrap');
        const stage = centre.querySelector('.stage');
        const conversation = centre.querySelector('.conversation');
        if (log === null || stage === null || conversation === null) throw new Error('no workspace boxes');
        const parts = ['.call-head', '.call-grid', '.share-view', '.share-frame', '.share-controls'].map((selector) => [selector, stage.querySelector(selector)?.getBoundingClientRect().height ?? 0]);
        return { centre: centre.getBoundingClientRect().height, log: log.getBoundingClientRect().height, stage: stage.getBoundingClientRect().height, conversation: conversation.getBoundingClientRect().height, parts };
      });
      expect(room.log / room.centre, JSON.stringify(room)).toBeGreaterThanOrEqual(0.6);
      const docked = await b.page.locator('.stage').evaluate((stage) => {
        const conversation = document.querySelector('.conversation');
        if (conversation === null) throw new Error('no conversation');
        return stage.getBoundingClientRect().bottom <= conversation.getBoundingClientRect().top + 1;
      });
      expect(docked, 'the strip never overlaps the conversation').toBe(true);
      await call(b.page).getByRole('button', { name: '#voice-room', exact: true }).click();
      await expect(channelButton(b.page, 'voice-room')).toHaveAttribute('aria-current', 'true');
      expect(await voiceSnapshot(b.page)).toEqual(voiceBefore);
      expect(await kept(b.page)).toEqual(before);
      const compactFrames = await inbound(b.page);
      await expect.poll(() => inbound(b.page), { timeout: 15_000 }).toBeGreaterThan(compactFrames);
      await b.page.setViewportSize({ width: 1440, height: 900 });
      await expand.focus();
      await b.page.keyboard.press('Enter');
      await expect(call(b.page).getByRole('button', { name: 'Compact', exact: true })).toBeFocused();
      expect(await kept(b.page)).toEqual(before);
      const expandedFrames = await inbound(b.page);
      await expect.poll(() => inbound(b.page), { timeout: 15_000 }).toBeGreaterThan(expandedFrames);

      // 3. In a three-person call, Carol speaks and her camera goes off and on.
      // Bob chose Show video and Compact; neither event changes that choice.
      await camera(c, true);
      await voice(b.page).getByRole('button', { name: 'Show video (1)' }).click();
      await expect.poll(() => keptCamera(b.page)).toBe(true);
      await call(b.page).getByRole('button', { name: 'Compact', exact: true }).click();
      await expect(call(b.page)).toHaveClass(/compact/);
      expect(await keptCamera(b.page)).toBe(true);
      await call(b.page).getByRole('button', { name: 'Expand', exact: true }).click();
      expect(await keptCamera(b.page)).toBe(true);
      await call(b.page).getByRole('button', { name: 'Compact', exact: true }).click();
      expect(await keptCamera(b.page)).toBe(true);
      await c.page.getByRole('heading', { name: 'Conch' }).click();
      await c.page.keyboard.down('Backquote');
      await expect(call(b.page).locator('.call-context')).toContainText('Speaking: carol', { timeout: 5000 });
      await c.page.keyboard.up('Backquote');
      await camera(c, false);
      await expect(call(b.page)).toHaveClass(/compact/);
      await camera(c, true);
      await expect(call(b.page)).toHaveClass(/compact/);
      // Keep watching without the camera subscription for the share-end path.
      await call(b.page).getByRole('button', { name: 'Expand', exact: true }).click();
      await call(b.page).getByRole('button', { name: 'Hide video', exact: true }).click();
      await call(b.page).getByRole('button', { name: 'Compact', exact: true }).click();
      expect(await kept(b.page)).toEqual(before);

      // 4. On a phone with reduced motion, the compact share leaves the composer
      // reachable, names the call and statuses, announces each toggle once,
      // keeps decoded frames flowing, has 44 px controls and runs no animation.
      // Under Chat the expanded stage is the stream alone (issue #1028), so
      // Compact is the Voice view's header's, and Expand the compact strip's.
      await b.page.emulateMedia({ reducedMotion: 'reduce' });
      await b.page.setViewportSize({ width: 390, height: 844 });
      await openChannel(b.page, 'workspace-text');
      const phoneStage = b.page.locator('.call-stage.compact');
      await expect(phoneStage.locator('.call-head')).toBeVisible();
      await expect(phoneStage.locator('.call-context')).toContainText('#voice-room');
      await expect(phoneStage.locator('.call-context')).toContainText('Sharing: alice');
      await expect(phoneStage.locator('.voice-sub')).toContainText('Microphone in use');
      await expect(phoneStage.locator('.voice-sub')).toContainText('Push to talk.');
      // The header's text is on screen, not only in the DOM: every piece of
      // the context and status rows lies inside the 390 px viewport.
      const offscreen = await phoneStage.locator('.call-context > *, .voice-sub > *').evaluateAll((parts) => parts
        .map((part) => ({ text: part.textContent, box: part.getBoundingClientRect() }))
        .filter(({ box }) => box.width > 0 && (box.left < 0 || box.right > window.innerWidth))
        .map(({ text, box }) => `${text} (${String(Math.round(box.left))}..${String(Math.round(box.right))})`));
      expect(offscreen, 'header parts outside the viewport').toEqual([]);
      await phoneStage.getByRole('button', { name: '#voice-room', exact: true }).click();
      await expectComposerShown(b.page);
      await phoneStage.getByRole('button', { name: 'Expand', exact: true }).click();
      await expect(b.page.getByText('Call view expanded.', { exact: true })).toHaveCount(1);
      await expect(shareView(b.page)).toBeVisible();
      await expect(b.page.locator('.stage').getByRole('button', { name: 'Compact', exact: true })).toHaveCount(0);
      await showView(b.page, 'Voice');
      // The open view's header is one line that may end in an ellipsis: each
      // part of it lies inside the 390 px viewport, cut short or not.
      const outside = await call(b.page).locator('.call-title, .call-context, .call-controls > *').evaluateAll((parts) => parts
        .map((part) => ({ text: part.textContent, box: part.getBoundingClientRect() }))
        .filter(({ box }) => box.width > 0 && (box.left < 0 || box.right > window.innerWidth))
        .map(({ text, box }) => `${text} (${String(Math.round(box.left))}..${String(Math.round(box.right))})`));
      expect(outside, 'open header parts outside the viewport').toEqual([]);
      await expect(call(b.page).locator('.call-context')).toHaveAttribute('title', /^#voice-room · Sharing: alice/);
      await call(b.page).getByRole('button', { name: 'Compact', exact: true }).click();
      await expect(b.page.getByText('Call view compact.', { exact: true })).toHaveCount(1);
      await showView(b.page, 'Chat');
      await expect(phoneStage.getByRole('button', { name: 'Expand', exact: true })).toBeVisible();
      await expectComposerShown(b.page);
      const phoneFrames = await inbound(b.page);
      await expect.poll(() => inbound(b.page), { timeout: 15_000 }).toBeGreaterThan(phoneFrames);
      expect(await b.page.locator('.stage').evaluate((stage) => stage.getAnimations({ subtree: true }).length)).toBe(0);
      const targets = await b.page.locator('.stage button').evaluateAll((buttons) => buttons.map((button) => {
        const box = button.getBoundingClientRect();
        return { name: button.textContent, width: box.width, height: box.height };
      }));
      expect(targets.length).toBeGreaterThan(0);
      for (const target of targets) {
        expect(target.width, `${target.name} width`).toBeGreaterThanOrEqual(44);
        expect(target.height, `${target.name} height`).toBeGreaterThanOrEqual(44);
      }
      expect(await kept(b.page)).toEqual(before);
      await b.page.setViewportSize({ width: 1440, height: 900 });
      await b.page.emulateMedia({ reducedMotion: 'no-preference' });

      // 5a. Alice's stopped-sharing note stays visible while Carol's stream
      // holds compact open. The last stream ending closes it; with no share or
      // chosen camera view, Carol's camera cannot open the stage by itself.
      await share(c);
      await tile(b.page, 'carol').getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
      await expect(shareView(b.page)).toHaveCount(2, { timeout: 30_000 });
      await voice(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(b.page)).toHaveCount(1, { timeout: 20_000 });
      await expect(call(b.page)).toHaveClass(/compact/);
      await expect(voice(b.page).getByText('alice stopped sharing.', { exact: true })).toBeVisible();
      await voice(c.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(shareView(b.page)).toHaveCount(0, { timeout: 20_000 });
      await expect(call(b.page)).toHaveCount(0);
      await camera(c, false);
      await camera(c, true);
      await expect(call(b.page)).toHaveCount(0);
      await expect(shareView(b.page)).toHaveCount(0);

      // 5b. Bob's compact choice survives watching again. Revoking use_camera
      // ends his own camera element and keeps the explanation visible beside
      // the compact share. An operator's voice removal then closes the stage.
      await share(a);
      await watching(b);
      await expect(call(b.page)).toHaveClass(/compact/);
      await camera(b, true);
      await expect(call(b.page)).toHaveClass(/compact/);
      await expect(call(b.page).getByLabel('Your camera')).toBeVisible({ timeout: 30_000 });
      await memberCamera(false);
      try {
        await expect(call(b.page).getByLabel('Your camera')).toHaveCount(0, { timeout: 10_000 });
        await expect(call(b.page)).toHaveClass(/compact/);
        await expect(voice(b.page).getByText('Camera is not allowed for you.', { exact: true })).toBeVisible({ timeout: 10_000 });
        await expect(voice(b.page)).not.toContainText('Sending video');
      } finally {
        await memberCamera(true);
      }
      await call(b.page).getByRole('button', { name: 'Expand', exact: true }).click();
      await call(b.page).getByRole('button', { name: 'Hide video', exact: true }).click();
      await call(b.page).getByRole('button', { name: 'Compact', exact: true }).click();
      const removed = await vw.api(vw.operator, 'DELETE', `/v1/channels/${vw.channel}/voice/participants/${String(b.member.id)}`);
      expect(removed.status, removed.text).toBe(204);
      await expect(call(b.page)).toHaveCount(0, { timeout: 30_000 });
      await expect(shareView(b.page)).toHaveCount(0);
      await expect(voice(b.page)).toContainText('You were removed from voice');
      await expect(voice(b.page)).not.toContainText('Connected in #voice-room');

      // 5c. Disabling the viewer's account also closes a compact stage.
      const dave = await vw.addMember('dave');
      const d = await person(browser, dave);
      people.push(d);
      await join(d);
      await watching(d);
      await call(d.page).getByRole('button', { name: 'Compact', exact: true }).click();
      await expect(call(d.page)).toHaveClass(/compact/);
      await expectClean(d);

      // Disabling Dave rotates the room (#516). If signaling drops before
      // LiveKit's room-deleted message arrives, the SDK can retry the retired
      // room and Chromium can log its 404. Capture only the surviving pages'
      // browser log entries from the disable through their fresh joins.
      const renewing = await Promise.all([a, c].map(async (p) => {
        await p.watch.settled();
        return { p, logFrom: p.watch.browserLog.length, tokensBefore: p.watch.conchdTokens.length };
      }));
      await vw.disable(dave);
      const renewed = await Promise.all(renewing.map(async ({ p, logFrom, tokensBefore }) => {
        await expect.poll(() => p.watch.conchdTokens.length, { timeout: 30_000 }).toBeGreaterThan(tokensBefore);
        await expect(voice(p.page)).toContainText('Connected in #voice-room', { timeout: 30_000 });
        await p.watch.settled();
        const log = p.watch.browserLog.slice(logFrom).filter(({ text }) =>
          text.endsWith("' failed: Error during WebSocket handshake: Unexpected response code: 404"));
        return { p, log };
      }));
      await expect(call(d.page)).toHaveCount(0, { timeout: 30_000 });
      await expect(shareView(d.page)).toHaveCount(0);
      await voice(a.page).getByRole('button', { name: 'Stop sharing' }).click();

      // 6. Every context stayed free of console, policy, request and secret
      // failures. Dave's check ran before disablement, whose 401s are expected.
      await expectClean(b);
      for (const { p, log } of renewed) await expectClean(p, log);
    } finally {
      for (const p of people) await leave(p);
    }
  });
});
