// The voice strip's controls must not move after joining (issue #2202): on a
// phone a row appeared above them a moment after Join voice and pushed them
// down by about 44 px. From the first in-voice frame the boxes of Share
// quality, Voice options and Leave voice stay put.
import type { BrowserContextOptions } from '@playwright/test';
import { expect, phones, showView, test } from './fixtures';
import { enter, joinVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

// who: a plain member alone in the room; a manager alone, whose chip also
// carries Remove from voice (44 px tall on a phone); a member joining a room
// where two people with long names are already in voice, so that their own
// chip wraps to a second row.
const layouts = [
  { name: '1440×900 browser', who: 'member', context: { viewport: { width: 1440, height: 900 } } },
  { name: '1215×900 browser', who: 'member', context: { viewport: { width: 1215, height: 900 } } },
  { name: '390×844 phone', who: 'member', context: phones[0][1] },
  { name: '390×844 phone, a channel manager', who: 'manager', context: phones[0][1] },
  { name: '360×740 phone, a channel manager', who: 'manager', context: phones[1][1] },
  { name: '1440×900 browser, a channel manager', who: 'manager', context: { viewport: { width: 1440, height: 900 } } },
  { name: '390×844 phone, joining two people already in voice', who: 'crowd', context: phones[0][1] },
  { name: '360×740 phone, joining two people already in voice', who: 'crowd', context: phones[1][1] },
  // The joiner has the lowest id, so the real chip sorts first; the names
  // differ in width, so a held chip in another slot wraps the row differently.
  { name: '390×844 phone, joining two people with names of 18 and 21 characters', who: 'crowd', names: ['Abcdefghijklmnop', 'Ghijklmnopqrstuvwxy'], context: phones[0][1] },
  { name: '360×740 phone, joining two people with names of 18 and 21 characters', who: 'crowd', names: ['Abcdefghijklmnop', 'Ghijklmnopqrstuvwxy'], context: phones[1][1] },
] as const satisfies readonly { name: string; who: 'member' | 'manager' | 'crowd'; names?: readonly string[]; context: BrowserContextOptions }[];

// Counts the people made for the crowd, so that no name repeats.
let crowds = 0;

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

  for (const { name, who, context, ...rest } of layouts) {
    test(`P2202 the controls do not move after joining at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      // Someone of their own each time, so nobody from an earlier case is
      // still listed in voice when they join.
      const [me] = await vw.members(1);
      if (me === undefined) throw new Error('no member');
      if (who === 'manager') expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${vw.channel}/members/${String(me.id)}`, { manages: true })).status).toBe(204);
      const others: Someone[] = [];
      const person = await enter(browser, vw, me, { context });
      try {
        const { page } = person;
        await showView(page, 'Voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const join = voice.getByRole('button', { name: 'Join voice' });
        await expect(join).toBeEnabled({ timeout: 30_000 });
        if (who === 'crowd') {
          for (const n of 'names' in rest ? rest.names : ['Abcdef', 'Ghijkl']) {
            // Eight characters each unless the case says otherwise, and not a name of an earlier case.
            const m = await vw.addMember(`${n}${String(++crowds).padStart(2, '0')}`);
            const other = await enter(browser, vw, m, { context: { viewport: { width: 1440, height: 900 } } });
            others.push(other);
            await joinVoice(other, 'Join voice');
          }
          await expect(voice.getByRole('list', { name: / in voice$/ }).getByRole('listitem')).toHaveCount(2, { timeout: 30_000 });
        } else {
          // The room is empty: the roster is not drawn before this page joins.
          await expect(voice.getByRole('heading', { name: / in voice$/ })).toHaveCount(0, { timeout: 40_000 });
        }
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
        for (const o of others) await o.close();
      }
    });
  }
});
