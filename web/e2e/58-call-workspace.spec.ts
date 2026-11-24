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

  const person = (browser: Browser, member: Member, options?: BrowserContextOptions): Promise<Person> =>
    enter(browser, vw, member, options === undefined ? {} : { context: options });

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

  test('P2187 a watched share fills its stage, with its controls over the picture', async ({ browser }) => {
    // Read the layout, not pixels. The stage keeps the size it always had. In
    // the open call view the share's controls are drawn over the picture's
    // bottom edge, like a video player's, so the frame takes what the view
    // leaves under the title (6rem at the least) and the video is contained
    // in it. In the closed phone Chat stage the controls stay in a row below.
    const check = async (page: Page, where: string, opts: { log?: boolean; open?: boolean } = {}) => {
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
        const head = view.querySelector('.call-head')?.getBoundingClientRect().height ?? 0;
        const gap = parseFloat(style.rowGap) || 0;
        const content = view.getBoundingClientRect().bottom - parseFloat(style.paddingBottom) - parseFloat(style.borderBottomWidth);
        const box = controls.getBoundingClientRect();
        return {
          stage: rect(stage), view: rect(view), frame: rect(frame), video: rect(video), controls: rect(controls),
          floor: 6 * parseFloat(getComputedStyle(document.documentElement).fontSize),
          fit: getComputedStyle(video).objectFit,
          scrolls: view.scrollHeight > view.clientHeight + 1,
          videoInStage: within(video, stage) && within(video, view),
          controlsInFrame: within(controls, frame),
          controlsInStage: within(controls, stage) && within(controls, view),
          controlsInWindow: box.left >= -1 && box.top >= -1 && box.right <= window.innerWidth + 1 && box.bottom <= window.innerHeight + 1,
          // Under the frame: what the view's content leaves unused.
          slack: content - frame.getBoundingClientRect().bottom,
          // What the stage has under the title, less the call head above it.
          room: content - title.getBoundingClientRect().bottom - head - gap,
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
    // Each control takes a real pointer: a trial click checks that the point
    // it would press is the control's own, not covered by anything else.
    const reachable = async (page: Page, where: string) => {
      const view = shareView(page);
      for (const control of [
        view.getByRole('button', { name: /^(Mute|Unmute) share$/ }),
        view.getByRole('slider', { name: 'Share volume' }),
        view.getByRole('button', { name: 'Full screen' }),
        view.getByRole('button', { name: 'Stop watching' }),
      ]) {
        await control.click({ trial: true, timeout: 5000 });
      }
      expect.soft(true, where).toBe(true);
    };

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
      const strip = await check(b.page, '1215×900 under the desktop strip', { log: true });
      // The picture takes at least 85% of what the stage has under the title.
      expect.soft(strip.frame.height, `the picture under the strip, of ${String(strip.room)} px\n${JSON.stringify(strip)}`).toBeGreaterThanOrEqual(0.85 * strip.room);
      await reachable(b.page, '1215×900 under the desktop strip');
      await b.page.evaluate(() => { document.querySelector('.desktop-bar')?.remove(); });
      for (const [width, height] of [[1440, 900], [1920, 1080]] as const) {
        await b.page.setViewportSize({ width, height });
        await check(b.page, `${String(width)}×${String(height)} browser`, { log: true });
      }

      await watching(c);
      for (const [width, height] of [[390, 844], [360, 740], [360, 640]] as const) {
        await c.page.setViewportSize({ width, height });
        await check(c.page, `${String(width)}×${String(height)} Voice`);
        if (width === 390) await reachable(c.page, '390×844 Voice');
      }
      await c.page.setViewportSize({ width: 390, height: 844 });
      await showView(c.page, 'Chat');
      await expect(c.page.locator('.call-stage')).toHaveCount(1);
      await check(c.page, '390×844 Chat', { open: false });
      await expect.soft(shareView(c.page).locator('.call-enlarge')).toHaveCount(0);
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
