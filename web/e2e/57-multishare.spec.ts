// Several browser streams in one voice room (issue #1509), with fake devices
// and the pinned LiveKit of voice-world.ts. The numbered steps are the issue's
// seven required scenarios, run in order against the same room.
import type { Browser, Locator, Page } from '@playwright/test';
import { expect, test } from './fixtures';
import { enter, inbound, joinVoice, leave, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type Member, type VoiceWorld } from './voice-world';

type Person = Someone;

const voice = (page: Page) => page.getByRole('region', { name: 'Voice' });
const tile = (page: Page, name: string): Locator => page.locator('.call-share').filter({ hasText: `${name} · LIVE` });

const videoFrames = async (page: Page): Promise<number> => (await inbound(page)).frames;

// shownFrames is how many frames a tile's own video element has shown, and
// its picture's width: one stream's, never the page's total.
const shownFrames = (video: Locator) =>
  video.evaluate((el) => {
    const v = el as HTMLVideoElement;
    return { frames: v.getVideoPlaybackQuality().totalVideoFrames, width: v.videoWidth };
  });

// expectPlaying is one stream playing in its own tile: its sound unmuted, not
// paused and its clock moving, and its own video element showing frames that
// keep coming. The page's received total is checked too, but proves nothing
// of one tile when two play.
async function expectPlaying(page: Page, stream: Locator): Promise<void> {
  const audio = stream.locator('audio');
  await expect(audio).toHaveJSProperty('muted', false);
  await expect.poll(() => audio.evaluate((a: HTMLAudioElement) => !a.paused && a.readyState >= 2), { timeout: 20_000 }).toBe(true);
  const before = await audio.evaluate((a: HTMLAudioElement) => a.currentTime);
  await expect.poll(() => audio.evaluate((a: HTMLAudioElement) => a.currentTime), { timeout: 15_000 }).toBeGreaterThan(before);
  const video = stream.locator('video');
  await expect.poll(async () => (await shownFrames(video)).width, { timeout: 30_000 }).toBeGreaterThan(0);
  const shown = (await shownFrames(video)).frames;
  await expect.poll(async () => (await shownFrames(video)).frames, { timeout: 15_000 }).toBeGreaterThan(shown);
  await expect.poll(() => videoFrames(page), { timeout: 30_000 }).toBeGreaterThan(10);
}

test.describe('voice multi-share', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('multishare needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the multishare suite needs it');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  const person = (browser: Browser, member: Member): Promise<Person> => enter(browser, vw, member);

  const join = (p: Person, watchOnly = false): Promise<void> => joinVoice(p, watchOnly ? 'Watch only' : 'Join voice');

  async function share(p: Person, quality = false): Promise<void> {
    if (quality) {
      await voice(p.page).getByRole('button', { name: 'Voice options' }).click();
      await voice(p.page).getByLabel('Share resolution').selectOption('720p');
      await voice(p.page).getByLabel('Share frame rate').selectOption('15');
    }
    await voice(p.page).getByRole('button', { name: 'Share screen' }).click();
    await expect(voice(p.page)).toContainText('You are sharing your screen with sound.', { timeout: 30_000 });
  }

  test('all seven multishare scenarios', async ({ browser }) => {
    test.setTimeout(300_000);
    const people: Person[] = [];
    try {
      const a = await person(browser, vw.alice);
      const b = await person(browser, vw.bob);
      const c = await person(browser, vw.carol);
      people.push(a, b, c);
      await join(a);
      await join(b);
      await join(c, true);

      // 1. Two contexts stream; one picks 720p/15 and the other's tile says so.
      await share(a, true);
      await share(b);
      await expect(tile(b.page, 'alice')).toContainText('720p · 15 fps', { timeout: 20_000 });
      await expect(tile(a.page, 'bob')).toContainText('LIVE');

      // 2. A third context sees both with Watch and receives no video before one.
      await expect(tile(c.page, 'alice').getByRole('button', { name: 'Watch' })).toBeVisible();
      await expect(tile(c.page, 'bob').getByRole('button', { name: 'Watch' })).toBeVisible();
      expect(await videoFrames(c.page)).toBe(0);
      await tile(c.page, 'alice').getByRole('button', { name: 'Watch' }).click();
      await expectPlaying(c.page, tile(c.page, 'alice'));

      // 3. Both play together, each in its own tile, with independent mutes.
      await tile(c.page, 'bob').getByRole('button', { name: 'Watch' }).click();
      await expectPlaying(c.page, tile(c.page, 'bob'));
      await expectPlaying(c.page, tile(c.page, 'alice'));
      await tile(c.page, 'alice').getByRole('button', { name: 'Mute share' }).click();
      await expect(tile(c.page, 'alice').locator('audio')).toHaveJSProperty('muted', true);
      await expect(tile(c.page, 'bob').locator('audio')).toHaveJSProperty('muted', false);
      await tile(c.page, 'alice').getByRole('button', { name: 'Unmute share' }).click();
      await expectPlaying(c.page, tile(c.page, 'alice'));

      // 4. Focus takes the stage; the other watched stream goes to the strip.
      await tile(c.page, 'alice').getByRole('button', { name: "Focus alice's screen" }).click();
      await expect(tile(c.page, 'alice')).toHaveClass(/main/);
      await expect(tile(c.page, 'bob')).toHaveClass(/in-strip/);
      await expect(c.page.getByRole('button', { name: 'Back to grid' })).toBeVisible();

      // 5. Stop watching unsubscribes only that tile.
      await tile(c.page, 'bob').getByRole('button', { name: 'Stop watching' }).click();
      await expect(tile(c.page, 'bob').locator('audio')).toHaveCount(0);
      await expect(tile(c.page, 'bob').getByRole('button', { name: 'Watch' })).toBeVisible();
      await expectPlaying(c.page, tile(c.page, 'alice'));

      // 6. The first sharer stops: its tile leaves, with a note, and focus returns.
      await voice(a.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(tile(c.page, 'alice')).toHaveCount(0, { timeout: 20_000 });
      await expect(voice(c.page)).toContainText('alice stopped sharing.');
      await expect(c.page.locator('.call-grid')).not.toHaveClass(/focus/);

      // 7. Restart the first stream and add four headless sharers: six are live,
      // so the next context's claim is refused with the cap note.
      await share(a);
      for (const name of ['multi-d', 'multi-e', 'multi-f', 'multi-g']) {
        const member = await vw.addMember(name);
        const p = await person(browser, member);
        people.push(p);
        await join(p);
        await share(p);
      }
      const seventh = await person(browser, await vw.addMember('multi-seventh'));
      people.push(seventh);
      await join(seventh);
      await voice(seventh.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voice(seventh.page)).toContainText('This call already has 6 streams live.', { timeout: 20_000 });
      await expect(voice(seventh.page)).not.toContainText('One person shares at a time.');
    } finally {
      for (const p of people) await leave(p);
    }
  });
});
