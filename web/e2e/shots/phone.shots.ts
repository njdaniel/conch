// The review screenshots of issue #929: the phone's Chat, Channels, Voice,
// Settings and a watched share, from the same seeded state (the voice world:
// alice, bob and carol in #voice-room, the same seven messages), at 390×844
// and at 320×640. Run against the build before the change and again after it
// (CONCH_SHOTS_TAG=before / after): where the build has no Chat / Channels /
// Voice navigation, showView does nothing and the page is photographed as it
// is. Nothing in CI runs it. See shots.config.ts; for these:
//
//   cd web && CONCH_SHOTS_DIR=../docs/design/mobile-browser-layout CONCH_SHOTS_TAG=after \
//     npx playwright test -c e2e/shots/shots.config.ts phone.shots.ts
import { mkdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import type { Browser, BrowserContextOptions, Page } from '@playwright/test';
import { expect, openChannel, showView, signIn, test } from '../fixtures';
import { dockerAvailable, startVoiceWorld, type Member, type VoiceWorld } from '../voice-world';
import { loadWorld } from '../world';

const sizes = [
  ['390x844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }],
  ['320x640', { viewport: { width: 320, height: 640 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }],
] as const satisfies readonly (readonly [string, BrowserContextOptions])[];

const said: readonly (readonly ['alice' | 'bob' | 'carol', string])[] = [
  ['bob', 'Anyone around tonight?'],
  ['carol', 'bob and I are in voice. Come and join us.'],
  ['alice', 'Just got in. What are we watching later?'],
  ['bob', 'Something funny. We can pick together.'],
  ['carol', `A long one, to see how it wraps: ${'a line that goes on for a while, '.repeat(6)}and ends.`],
  ['carol', 'and a link with no place to break: https://example.com/a/very/long/path/that/keeps/going/and/going/end'],
  ['bob', 'Two lines,\nas typed.'],
];

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

test('phone screenshots for design review', async ({ browser }, info) => {
  if (!dockerAvailable()) {
    info.skip(true, 'the screenshots need Docker for the voice world');
    return;
  }
  const dir = resolve(process.env['CONCH_SHOTS_DIR'] ?? 'test-results/shots');
  const tag = process.env['CONCH_SHOTS_TAG'] ?? 'shot';
  mkdirSync(dir, { recursive: true });
  const vw = await startVoiceWorld(loadWorld());
  const opened: { close: () => Promise<void> }[] = [];
  try {
    for (const [who, body] of said) {
      const r = await vw.api(vw[who].token, 'POST', `/v2/channels/${vw.channel}/messages`, { body });
      expect(r.status).toBe(201);
    }
    // bob and carol are in voice throughout; carol shares when asked to.
    const bob = await person(browser, vw, vw.bob, sizes[0][1]);
    const carol = await person(browser, vw, vw.carol, sizes[0][1]);
    opened.push(bob.context, carol.context);
    for (const p of [bob.page, carol.page]) await joinVoice(p);

    for (const [size, options] of sizes) {
      const shot = async (page: Page, name: string) => {
        await page.waitForTimeout(400);
        await page.screenshot({ path: join(dir, `${tag}-${name}-${size}.png`) });
      };
      const alice = await person(browser, vw, vw.alice, options);
      opened.push(alice.context);
      const page = alice.page;
      await expect(page.getByText('Two lines,')).toBeVisible();

      // Chat, not in voice; then Channels; then joined, Chat with the strip.
      await shot(page, 'chat');
      await showView(page, 'Channels');
      await shot(page, 'channels');
      await showView(page, 'Voice');
      await expect(voiceRegion(page)).toContainText('bob');
      await shot(page, 'voice-idle');
      await joinVoice(page);
      await shot(page, 'voice');
      await showView(page, 'Chat');
      await shot(page, 'chat-in-voice');

      await page.getByRole('button', { name: 'Settings' }).click();
      await expect(page.getByRole('dialog', { name: 'Settings' })).toBeVisible();
      await shot(page, 'settings');
      await page.keyboard.press('Escape');

      await showView(page, 'Voice');
      await voiceRegion(carol.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voiceRegion(carol.page)).toContainText('You are sharing your screen', { timeout: 30_000 });
      await voiceRegion(page).getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
      const view = page.getByRole('region', { name: 'Shared screen' });
      await expect(view).toBeVisible();
      await expect(view.getByText('Waiting for the picture')).toHaveCount(0, { timeout: 30_000 });
      await page.waitForTimeout(1500);
      await shot(page, 'share');
      await showView(page, 'Chat');
      await shot(page, 'chat-watching');
      await voiceRegion(carol.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(view).toHaveCount(0, { timeout: 15_000 });
      await showView(page, 'Voice');
      await voiceRegion(page).getByRole('button', { name: 'Leave voice' }).click();
      await alice.context.close();
    }
  } finally {
    for (const c of opened) await c.close().catch(() => undefined);
    vw.stop();
  }
});

async function person(browser: Browser, vw: VoiceWorld, member: Member, options: BrowserContextOptions) {
  const context = await browser.newContext(options);
  const page = await context.newPage();
  await signIn(page, vw, member.token, member.name);
  await openChannel(page, vw.channel);
  return { context, page };
}

async function joinVoice(page: Page): Promise<void> {
  await showView(page, 'Voice');
  await voiceRegion(page).getByRole('button', { name: 'Join voice' }).click();
  await expect(voiceRegion(page)).toContainText('Connected in #voice-room', { timeout: 30_000 });
}
