// The review screenshots of issue #536: sign-in, an invite, the lobby, the
// settings, in voice, watching a share, and the page that cannot reach the
// server, each at 1440×900 and 390×844. Every state is made the same way each
// run, by the fixture's own people (alice, bob, carol); only the clock differs.
// See shots.config.ts for how to run it.
import { mkdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import type { Browser, BrowserContextOptions, Page } from '@playwright/test';
import { channelButton, expect, signIn, test } from '../fixtures';
import { dockerAvailable, startVoiceWorld, type Member, type VoiceWorld } from '../voice-world';
import { inviteUrl, loadWorld } from '../world';

const sizes = [
  ['desktop', { viewport: { width: 1440, height: 900 } }],
  ['phone', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true }],
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

test('screenshots for design review', async ({ browser }, info) => {
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

      const signInPage = await browser.newContext(options);
      opened.push(signInPage);
      const s = await signInPage.newPage();
      await s.goto(vw.appUrl);
      await expect(s.getByLabel('Credential')).toBeVisible();
      await shot(s, 'signin');

      const invite = await vw.api(vw.operator, 'POST', '/v1/invites', { kind: 'personal', channels: [vw.channel], voice: true });
      expect(invite.status).toBe(201);
      const code = (JSON.parse(invite.text) as { code: string }).code;
      const guest = await signInPage.newPage();
      await guest.goto(inviteUrl(vw, code));
      await expect(guest.getByLabel('Your name')).toBeVisible();
      await shot(guest, 'invite');

      const alice = await person(browser, vw, vw.alice, options);
      opened.push(alice.context);
      const page = alice.page;
      await expect(page.getByText('Two lines,')).toBeVisible();
      await expect(voiceRegion(page)).toContainText('bob');
      await shot(page, 'lobby');

      await openSettings(page);
      await shot(page, 'settings');
      await page.keyboard.press('Escape');
      await page.reload();
      await channelButton(page, vw.channel).click();

      await joinVoice(page);
      await voiceRegion(page).scrollIntoViewIfNeeded();
      await shot(page, 'voice');

      await voiceRegion(carol.page).getByRole('button', { name: 'Share screen' }).click();
      await expect(voiceRegion(carol.page)).toContainText('You are sharing your screen', { timeout: 30_000 });
      await voiceRegion(page).getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
      const view = page.getByRole('region', { name: 'Shared screen' });
      await expect(view).toBeVisible();
      await expect(view.getByText('Waiting for the picture')).toHaveCount(0, { timeout: 30_000 });
      await page.waitForTimeout(1500);
      await view.scrollIntoViewIfNeeded();
      await shot(page, 'share');
      await voiceRegion(carol.page).getByRole('button', { name: 'Stop sharing' }).click();
      await expect(view).toHaveCount(0, { timeout: 15_000 });
      await voiceRegion(page).getByRole('button', { name: 'Leave voice' }).click();
      await alice.context.close();

      // The lobby that cannot reach the server: the channel list never answers.
      const lost = await browser.newContext(options);
      opened.push(lost);
      const l = await lost.newPage();
      await l.route('**/v1/channels', (route) => route.abort());
      await l.goto(vw.appUrl);
      await l.getByLabel('Credential').fill(vw.alice.token);
      await l.getByRole('button', { name: 'Sign in' }).click();
      await expect(l.getByText('Cannot reach the server')).toBeVisible();
      await shot(l, 'error');
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
  await channelButton(page, vw.channel).click();
  return { context, page };
}

async function joinVoice(page: Page): Promise<void> {
  await voiceRegion(page).getByRole('button', { name: 'Join voice' }).click();
  await expect(voiceRegion(page)).toContainText('Connected in #voice-room', { timeout: 30_000 });
}

// The settings: a panel of their own where there is one, else the account
// controls in the header, with "Your sign-ins" open.
async function openSettings(page: Page): Promise<void> {
  const settings = page.getByRole('button', { name: 'Settings' });
  if ((await settings.count()) > 0) {
    await settings.click();
    return;
  }
  await page.getByRole('button', { name: 'Your sign-ins' }).click();
  await expect(page.getByRole('region', { name: 'Your sign-ins' })).toContainText('This browser');
}
