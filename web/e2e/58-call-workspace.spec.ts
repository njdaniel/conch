// The persistent call workspace (issue #1498): text navigation changes no
// voice state, compact is a docked in-memory layout, and cameras, speakers,
// share ends and voice removal never change that layout by themselves. The
// same machine supplies fake microphone, camera and screen devices through
// the pinned LiveKit of voice-world.ts; these are not real-device tests.
import type { Browser, BrowserContextOptions, Locator, Page } from '@playwright/test';
import { channelButton, expect, openChannel, showView, test } from './fixtures';
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

  async function expectClean(p: Person): Promise<void> {
    await p.watch.settled();
    const said = p.watch.console.join('\n');
    for (const secret of vw.secrets) expect(said, 'a secret in the console').not.toContain(secret);
    expect(p.watch.console.filter((line) => line.startsWith('error: ') || line.startsWith('pageerror: ')), 'no console error').toEqual([]);
    expect(p.watch.console.filter((line) => /Content Security Policy|Refused to|[Pp]ermissions policy/.test(line))).toEqual([]);
    expect(p.watch.policy, 'Permissions-Policy reports').toEqual([]);
    expect(p.watch.failed, 'requests to conchd that failed').toEqual([]);
  }

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
      await vw.disable(dave);
      await expect(call(d.page)).toHaveCount(0, { timeout: 30_000 });
      await expect(shareView(d.page)).toHaveCount(0);
      await voice(a.page).getByRole('button', { name: 'Stop sharing' }).click();

      // 6. Every context stayed free of console, policy, request and secret
      // failures. Dave's check ran before disablement, whose 401s are expected.
      for (const p of [a, b, c]) await expectClean(p);
    } finally {
      for (const p of people) await leave(p);
    }
  });
});
