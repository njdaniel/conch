// The voice strip's controls must not move after joining (issue #2202): on a
// phone a row appeared above them a moment after Join voice and pushed them
// down by about 44 px. From the first in-voice frame the boxes of Share
// quality, Voice options and Leave voice stay put.
import type { BrowserContextOptions } from '@playwright/test';
import { expect, phones, showView, test } from './fixtures';
import { enter } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

const layouts = [
  { name: '1440×900 browser', member: 'alice', context: { viewport: { width: 1440, height: 900 } } },
  { name: '1215×900 browser', member: 'carol', context: { viewport: { width: 1215, height: 900 } } },
  { name: '390×844 phone', member: 'bob', context: phones[0][1] },
] as const satisfies readonly { name: string; member: 'alice' | 'bob' | 'carol'; context: BrowserContextOptions }[];

const controls = ['Share quality', 'Voice options', 'Leave voice'] as const;

test.describe('the voice strip', { tag: '@voice' }, () => {
  let vw: VoiceWorld | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the voice strip suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the voice strip suite needs it');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
  });

  test.afterAll(() => { vw?.stop(); });

  for (const { name, member, context } of layouts) {
    test(`P2202 the controls do not move after joining at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      const person = await enter(browser, vw, vw[member], { context });
      try {
        const { page } = person;
        await showView(page, 'Voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const join = voice.getByRole('button', { name: 'Join voice' });
        await expect(join).toBeEnabled({ timeout: 30_000 });
        // Watch from inside the page, so the first frame with the controls is
        // read in the same task that added them.
        await page.evaluate((labels) => {
          const w = window as unknown as { __strip: { t: number; boxes: Record<string, number[] | null> }[] };
          w.__strip = [];
          const read = () => {
            const region = document.querySelector('section[aria-label="Voice"]');
            const boxes: Record<string, number[] | null> = {};
            for (const l of labels) {
              const el = Array.from(region?.querySelectorAll('button') ?? []).find((b) => b.getAttribute('aria-label') === l || b.textContent.trim() === l);
              const r = el?.getBoundingClientRect();
              boxes[l] = r === undefined ? null : [r.left, r.top, r.width, r.height];
            }
            return boxes;
          };
          const t0 = performance.now();
          let started = false;
          const note = () => {
            const boxes = read();
            if (!started && Object.values(boxes).some((b) => b === null)) return;
            started = true;
            w.__strip.push({ t: performance.now() - t0, boxes });
          };
          new MutationObserver(note).observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true });
          setInterval(note, 50);
        }, [...controls]);
        await join.click();
        await expect(voice.getByRole('button', { name: 'Leave voice' })).toBeVisible({ timeout: 30_000 });
        await page.waitForTimeout(3_000);
        const samples = await page.evaluate(() => (window as unknown as { __strip: { t: number; boxes: Record<string, number[] | null> }[] }).__strip);
        const first = samples[0];
        if (first === undefined) throw new Error('the controls never appeared');
        for (const label of controls) {
          const want = first.boxes[label];
          if (want === null || want === undefined) throw new Error(`${label} was not there in the first frame`);
          for (const s of samples) {
            const got = s.boxes[label];
            if (got === null || got === undefined) throw new Error(`${label} vanished at ${String(Math.round(s.t))} ms`);
            for (const [i, v] of got.entries()) {
              expect(Math.abs(v - (want[i] ?? Number.NaN)), `${label} box[${String(i)}] at ${String(Math.round(s.t))} ms (${JSON.stringify(first.boxes[label])} to ${JSON.stringify(got)})`).toBeLessThanOrEqual(0.5);
            }
          }
        }
      } finally {
        await person.close();
      }
    });
  }
});
