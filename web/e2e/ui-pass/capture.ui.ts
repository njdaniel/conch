// The UI pass's capture (.claude/skills/ui-pass/SKILL.md): screenshots of the
// client through the talk and stream flows, at three sizes, against the e2e
// world and a real LiveKit. Not a test: a step that fails is recorded in the
// manifest and the capture goes on, so one broken state never hides the rest.
//
// People and sizes:
// - alice: a desktop browser, 1440×900;
// - bob: a phone, 390×844;
// - carol: 1215×900 with the desktop app's strip drawn (drawDesktopStrip). The
//   packaged app's own voice needs the Rust helper, so this is its layout in
//   the browser, not the packaged app.
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { Locator, Page } from '@playwright/test';
import { drawDesktopStrip, expect, showView, test } from '../fixtures';
import { enter, inbound, joinVoice, type Someone } from '../people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from '../voice-world';

const out = process.env['CONCH_UI_OUT'] ?? fileURLToPath(new URL('../../test-results-ui/shots', import.meta.url));
const flows = new Set((process.env['CONCH_UI_FLOWS'] ?? 'talk,stream').split(',').map((f) => f.trim()));

interface Shot {
  readonly step: string;
  readonly person: string;
  readonly size: string;
  readonly file: string;
  readonly note: string;
}
interface Failure {
  readonly step: string;
  readonly error: string;
}

const voice = (page: Page): Locator => page.getByRole('region', { name: 'Voice' });
const tile = (page: Page, name: string): Locator => page.locator('.call-share').filter({ hasText: `${name} · LIVE` });

// Two animation frames and a short pause, so transitions and late layout settle.
async function settle(page: Page, ms = 400): Promise<void> {
  await page.evaluate(() => new Promise<void>((resolve) => {
    requestAnimationFrame(() => { requestAnimationFrame(() => { resolve(); }); });
  }));
  await page.waitForTimeout(ms);
}

test('capture the talk and stream flows', async ({ browser }, info) => {
  if (!dockerAvailable()) {
    info.skip(true, 'the capture needs Docker for the pinned LiveKit image');
    return;
  }
  mkdirSync(out, { recursive: true });
  const shots: Shot[] = [];
  const failures: Failure[] = [];
  const { loadWorld } = await import('../world');
  const vw: VoiceWorld = await startVoiceWorld(loadWorld());
  const people: { readonly name: string; size: string; readonly p: Someone }[] = [];
  let n = 0;

  const shoot = async (step: string, note: string, only?: readonly string[]): Promise<void> => {
    n += 1;
    for (const { name, size, p } of people) {
      if (only !== undefined && !only.includes(name)) continue;
      const file = `${String(n).padStart(2, '0')}-${step}-${name}.png`;
      try {
        await settle(p.page);
        await p.page.screenshot({ path: join(out, file) });
        shots.push({ step, person: name, size, file, note });
      } catch (err) {
        failures.push({ step: `${step} (${name} screenshot)`, error: String(err).slice(0, 300) });
      }
    }
  };
  // A state that fails is recorded and skipped; the capture goes on.
  const step = async (name: string, act: () => Promise<void>): Promise<boolean> => {
    try {
      await act();
      return true;
    } catch (err) {
      failures.push({ step: name, error: String(err).slice(0, 300) });
      return false;
    }
  };

  try {
    const alice = await enter(browser, vw, vw.alice, { context: { viewport: { width: 1440, height: 900 } } });
    people.push({ name: 'alice', size: '1440×900 browser', p: alice });
    const bob = await enter(browser, vw, vw.bob, { context: { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true, deviceScaleFactor: 2 } });
    people.push({ name: 'bob', size: '390×844 phone', p: bob });
    const carol = await enter(browser, vw, vw.carol, { context: { viewport: { width: 1215, height: 900 } } });
    // Registered before anything else can fail, so cleanup always closes her.
    people.push({ name: 'carol', size: '1215×900 desktop layout', p: carol });
    // Without the strip, carol is a plain browser at 1215, and is labelled so.
    if (!await step('carol draws the desktop strip', async () => { await drawDesktopStrip(carol.page); })) {
      const c = people.find((x) => x.name === 'carol');
      if (c !== undefined) c.size = '1215×900 browser (desktop strip failed)';
    }
    const nameOf = (who: Someone): string => people.find((x) => x.p === who)?.name ?? '?';

    await shoot('lobby', 'Signed in, channel open, nobody in voice.');

    // Each person joins on their own, so one failure doesn't stop the others,
    // and a shot only ever names people who reached its state.
    const joined: string[] = [];
    if (flows.has('talk') || flows.has('stream')) {
      for (const { name, p } of people) {
        if (await step(`${name} joins voice`, async () => { await joinVoice(p, 'Join voice'); })) joined.push(name);
      }
      await shoot('in-voice-idle', `In voice (${joined.join(', ')}), push to talk, nobody talking.`, joined);
      if (joined.includes('bob') && await step('phone chat view in voice', async () => { await showView(bob.page, 'Chat'); })) {
        await shoot('phone-chat-in-voice', 'Phone on the Chat view while in voice.', ['bob']);
      }
      await step('phone back to voice view', async () => { await showView(bob.page, 'Voice'); });
    }

    if (flows.has('talk')) {
      if (await step('alice holds the talk key', async () => {
        await alice.page.keyboard.down('Backquote');
        await alice.page.waitForTimeout(1500);
      })) {
        await shoot('talking', 'alice holds push to talk: her own strip, and what the others see.');
        await step('alice releases the talk key', async () => { await alice.page.keyboard.up('Backquote'); });
      }
      await step('alice mutes', async () => { await voice(alice.page).getByRole('button', { name: 'Mute', exact: true }).click(); });
      await shoot('muted', 'alice muted.', ['alice', 'carol']);
      await step('alice unmutes', async () => { await voice(alice.page).getByRole('button', { name: 'Unmute', exact: true }).click(); });
      for (const who of [alice, bob, carol]) {
        const name = nameOf(who);
        const toggle = voice(who.page).getByRole('button', { name: 'Voice options' });
        if (await step(`${name} opens Voice options`, async () => { await toggle.click(); })) {
          await shoot('voice-options', 'Voice options open.', [name]);
          // Escape does not close it (finding UI-talk-2): use its toggle, as
          // a person would, so later states are not shot with it open.
          await step(`${name} closes Voice options`, async () => { await toggle.click(); });
        }
      }
    }

    if (flows.has('stream')) {
      if (await step('alice opens the share quality menu', async () => { await voice(alice.page).getByRole('button', { name: /^Share quality/ }).click(); })) {
        await shoot('share-quality-menu', 'The share quality menu, before sharing.', ['alice']);
        await step('alice closes the share quality menu', async () => { await alice.page.keyboard.press('Escape'); });
      }
      if (await step('alice shares her screen', async () => {
        await voice(alice.page).getByRole('button', { name: 'Share screen' }).click();
        await voice(alice.page).getByText(/You are sharing your screen/).waitFor({ timeout: 30_000 });
      })) {
        await shoot('sharing', 'alice shares: her own view, and the share offered to the others before they watch.');
        // Watching means decoded frames are arriving, not only that the
        // player is on screen (as 58-call-workspace waits).
        const watchers: string[] = [];
        for (const who of [bob, carol]) {
          const name = nameOf(who);
          if (await step(`${name} watches`, async () => {
            await tile(who.page, 'alice').getByRole('button', { name: 'Watch' }).click({ timeout: 30_000 });
            await who.page.getByRole('region', { name: 'Shared screen' }).waitFor({ timeout: 30_000 });
            await expect.poll(async () => (await inbound(who.page)).frames, { timeout: 30_000 }).toBeGreaterThan(10);
          })) watchers.push(name);
        }
        if (watchers.length > 0) {
          await shoot('watching', `alice shares; ${watchers.join(' and ')} watch.`, ['alice', ...watchers]);
        }
        if (watchers.includes('bob') && await step('phone chat while watching', async () => { await showView(bob.page, 'Chat'); })) {
          await shoot('phone-chat-while-watching', 'Phone on the Chat view while watching a share.', ['bob']);
        }
        await step('phone back to voice while watching', async () => { await showView(bob.page, 'Voice'); });
        await step('alice stops sharing', async () => {
          await voice(alice.page).getByRole('button', { name: /^Stop shar/ }).click();
          await alice.page.waitForTimeout(2500);
        });
        await shoot('share-ended', 'alice stopped sharing.');
      }
    }

    if (joined.includes('alice') && await step('alice leaves voice', async () => {
      await voice(alice.page).getByRole('button', { name: 'Leave voice' }).click();
      await alice.page.waitForTimeout(1500);
    })) {
      const remain = joined.filter((x) => x !== 'alice');
      await shoot('after-leave', `alice left voice; ${remain.length === 0 ? 'nobody remains' : `${remain.join(' and ')} remain`}.`, ['alice', ...remain]);
    }
  } finally {
    // Teardown runs whatever happened above, the manifest write included, so
    // LiveKit and conchd are never left running.
    try {
      writeFileSync(join(out, 'manifest.json'), `${JSON.stringify({ shots, failures }, null, 2)}\n`);
    } finally {
      for (const { p } of people) await p.close().catch(() => undefined);
      vw.stop();
    }
  }
});
