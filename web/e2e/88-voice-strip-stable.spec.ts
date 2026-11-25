// A roster chip's size does not depend on its badge (issue #2211): on a phone a
// manager's own chip, which also carries Remove from voice, wrapped onto a
// second line when "sending" or "speaking" showed, and every control below it
// moved 24.8 px, Hold to talk under the finger while it was held. A fourth
// person joining or leaving moved them by the roster's gap.
//
// The voice strip's controls must not move after joining (issue #2202): on a
// phone a row appeared above them a moment after Join voice and pushed them
// down by about 44 px. From the first in-voice frame the boxes of Share
// quality, Voice options and Leave voice stay put.
// Watching beside your voice on another device keeps the whole row in place
// while the connection confirms the viewer's listen-only grant (issue #2210).
import type { BrowserContextOptions, Page } from '@playwright/test';
import { expect, phones, showView, test } from './fixtures';
import { enter, joinVoice, leaveVoice, type Someone } from './people';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

// who: a plain member alone in the room; a manager alone, whose chip also
// carries Remove from voice (44 px tall on a phone); a member joining a room
// where two people with long names are already in voice, so that their own
// chip wraps to a second row; or a viewer beside their own voice on another page.
const layouts = [
  { name: '1440×900 browser', who: 'member', context: { viewport: { width: 1440, height: 900 } } },
  { name: '1215×900 browser', who: 'member', context: { viewport: { width: 1215, height: 900 } } },
  { name: '390×844 phone', who: 'member', context: phones[0][1] },
  { name: '390×844 phone, a channel manager', who: 'manager', context: phones[0][1] },
  { name: '360×740 phone, a channel manager', who: 'manager', context: phones[1][1] },
  { name: '1440×900 browser, a channel manager', who: 'manager', context: { viewport: { width: 1440, height: 900 } } },
  // Open microphone: the chip's sending badge shows about 333 ms after the join.
  { name: '390×844 phone, a channel manager in the open microphone', who: 'manager', mode: 'unchosen', context: phones[0][1] },
  { name: '360×740 phone, a channel manager in the open microphone', who: 'manager', mode: 'unchosen', context: phones[1][1] },
  { name: '390×844 phone, a member in the open microphone', who: 'member', mode: 'unchosen', context: phones[0][1] },
  { name: '1440×900 browser, a channel manager in the open microphone', who: 'manager', mode: 'unchosen', context: { viewport: { width: 1440, height: 900 } } },
  { name: '390×844 phone, joining two people already in voice', who: 'crowd', context: phones[0][1] },
  { name: '360×740 phone, joining two people already in voice', who: 'crowd', context: phones[1][1] },
  // The joiner has the lowest id, so the real chip sorts first; the names
  // differ in width, so a held chip in another slot wraps the row differently.
  { name: '390×844 phone, joining two people with names of 18 and 21 characters', who: 'crowd', names: ['Abcdefghijklmnop', 'Ghijklmnopqrstuvwxy'], context: phones[0][1] },
  { name: '360×740 phone, joining two people with names of 18 and 21 characters', who: 'crowd', names: ['Abcdefghijklmnop', 'Ghijklmnopqrstuvwxy'], context: phones[1][1] },
  { name: '390×844 phone with Watch only beside your own voice', who: 'viewer', context: phones[0][1] },
] as const satisfies readonly { name: string; who: 'member' | 'manager' | 'crowd' | 'viewer'; mode?: 'unchosen'; names?: readonly string[]; context: BrowserContextOptions }[];

// Counts the people made for the crowd, so that no name repeats.
let crowds = 0;

const controls = ['Share quality', 'Voice options', 'Leave voice'] as const;
const viewerControls = ['Hold to talk', 'Open mic', 'Mute', 'Deafen', 'Voice options', 'Leave voice'] as const;
type JoinSample = { t: number; mark: boolean; boxes: Boxes };

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
    test(`${who === 'viewer' ? 'P2210' : 'P2202'} the controls do not move after joining at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      // Someone of their own each time, so nobody from an earlier case is
      // still listed in voice when they join.
      const [me] = await vw.members(1);
      if (me === undefined) throw new Error('no member');
      if (who === 'manager') expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${vw.channel}/members/${String(me.id)}`, { manages: true })).status).toBe(204);
      const others: Someone[] = [];
      const person = await enter(browser, vw, me, { context, ...('mode' in rest ? { mode: rest.mode } : {}) });
      try {
        const { page } = person;
        await showView(page, 'Voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const join = voice.getByRole('button', { name: who === 'viewer' ? 'Watch only' : 'Join voice' });
        await expect(join).toBeEnabled({ timeout: 30_000 });
        if (who === 'viewer') {
          const other = await enter(browser, vw, me, { context: { viewport: { width: 1440, height: 900 } } });
          others.push(other);
          await joinVoice(other, 'Join voice');
          await expect(voice).toContainText('You are in voice on another device.', { timeout: 30_000 });
        } else if (who === 'crowd') {
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
        const labels = who === 'viewer' ? viewerControls : controls;
        await page.evaluate((labels) => {
          const w = window as unknown as { __strip: JoinSample[] };
          w.__strip = [];
          const read = () => {
            const row = document.querySelector('section[aria-label="Voice"] .voice-controls');
            const buttons = Array.from(row?.querySelectorAll('button') ?? []);
            const boxes: Boxes = {};
            for (const l of labels) {
              const el = buttons.find((b) => b.getAttribute('aria-label') === l || b.textContent.trim() === l);
              const r = el === undefined || getComputedStyle(el).visibility !== 'visible' ? undefined : el.getBoundingClientRect();
              boxes[l] = r === undefined ? null : [r.left, r.top, r.width, r.height];
            }
            return boxes;
          };
          let started: number | undefined;
          const note = () => {
            const boxes = read();
            if (started === undefined && boxes['Leave voice'] == null) return;
            const now = performance.now();
            started ??= now;
            w.__strip.push({ t: now - started, mark: document.querySelector('.voice-roster .state') !== null, boxes });
          };
          new MutationObserver(note).observe(document.body, { subtree: true, childList: true, attributes: true, characterData: true });
          setInterval(note, 50);
        }, [...labels]);
        await join.click();
        await expect(voice.getByRole('button', { name: 'Leave voice' })).toBeVisible({ timeout: 30_000 });
        if (who === 'viewer') {
          await expect(voice.locator('.voice-phase')).toHaveText(`Connected in #${vw.channel}`, { timeout: 30_000 });
          await expect(voice.locator('.voice-sub')).toContainText('Watching beside your voice on another device.');
        }
        await page.waitForFunction(() => ((window as unknown as { __strip: JoinSample[] }).__strip.at(-1)?.t ?? 0) >= 3_000);
        const samples = await page.evaluate(() => (window as unknown as { __strip: JoinSample[] }).__strip);
        const first = samples[0];
        if (first === undefined) throw new Error('the controls never appeared');
        // In the open microphone the viewer's own sending mark must have shown,
        // or the case proves nothing about it.
        if ('mode' in rest) expect(samples.some((s) => s.mark), 'the viewer\'s sending or speaking mark never showed').toBe(true);
        for (const label of labels) {
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

// The boxes of the controls named, as [left, top, width, height], or null where
// one is not there.
type Boxes = Record<string, readonly number[] | null>;

async function boxesOf(page: Page, labels: readonly string[]): Promise<Boxes> {
  const out: Boxes = {};
  for (const label of labels) {
    const box = await page.getByRole('region', { name: 'Voice' }).getByRole('button', { name: label, exact: true }).boundingBox();
    out[label] = box === null ? null : [box.x, box.y, box.width, box.height];
  }
  return out;
}

// sampleEvery reads the boxes every 50 ms until stop is called, and says each
// box that moved from the first reading by more than half a pixel.
function sampleEvery(page: Page, labels: readonly string[]) {
  const samples: { t: number; boxes: Boxes }[] = [];
  const state = { running: true };
  const t0 = Date.now();
  const loop = (async () => {
    while (state.running) {
      // A page closed under it ends the watching, not the test's own verdict.
      try {
        samples.push({ t: Date.now() - t0, boxes: await boxesOf(page, labels) });
        await page.waitForTimeout(50);
      } catch { break; }
    }
  })();
  return {
    stop: async (): Promise<string[]> => {
      state.running = false;
      await loop;
      const first = samples[0];
      if (first === undefined) throw new Error('no sample was taken');
      const moved: string[] = [];
      for (const label of labels) {
        const want = first.boxes[label];
        if (want === null || want === undefined) throw new Error(`${label} was not there at the start`);
        for (const s of samples) {
          const got = s.boxes[label];
          if (got === null || got === undefined) { moved.push(`${label} vanished at ${String(s.t)} ms`); break; }
          if (got.some((v, i) => Math.abs(v - (want[i] ?? Number.NaN)) > 0.5)) { moved.push(`${label} at ${String(s.t)} ms: ${JSON.stringify(want)} to ${JSON.stringify(got)}`); break; }
        }
      }
      return moved;
    },
  };
}

const hold = [
  { name: '390×844 phone', context: phones[0][1] },
  { name: '360×740 phone', context: phones[1][1] },
] as const;

test.describe('the roster chips', { tag: '@voice' }, () => {
  let vw: VoiceWorld | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the roster suite needs Docker for the pinned LiveKit image');
      info.skip(true, 'Docker is not available; the roster suite needs it');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
  });

  test.afterAll(() => { vw?.stop(); });

  for (const { name, context } of hold) {
    test(`P2211 Hold to talk does not move while held by a channel manager at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      const [me] = await vw.members(1);
      if (me === undefined) throw new Error('no member');
      expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${vw.channel}/members/${String(me.id)}`, { manages: true })).status).toBe(204);
      const person = await enter(browser, vw, me, { context });
      try {
        const { page } = person;
        await joinVoice(person, 'Join voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const talk = voice.getByRole('button', { name: 'Hold to talk' });
        await expect(talk).toBeVisible({ timeout: 30_000 });
        const roster = voice.getByRole('list', { name: / in voice$/ });
        await expect(roster).toContainText(me.name);
        await page.waitForTimeout(1_000);
        const sampler = sampleEvery(page, ['Hold to talk', 'Share quality', 'Voice options', 'Leave voice']);
        await page.waitForTimeout(300);
        const at = await talk.boundingBox();
        if (at === null) throw new Error('Hold to talk has no box');
        await page.mouse.move(at.x + at.width / 2, at.y + at.height / 2);
        await page.mouse.down();
        // Its own chip says it is sending, and that is the change under test.
        await expect(roster).toContainText(/\((sending|speaking)\)/, { timeout: 10_000 });
        await page.waitForTimeout(1_500);
        await page.mouse.up();
        await expect(roster).not.toContainText(/\((sending|speaking)\)/, { timeout: 10_000 });
        await page.waitForTimeout(500);
        expect(await sampler.stop()).toEqual([]);
      } finally {
        await person.close();
      }
    });
  }

  // The sending mark sits on its own avatar's corner however the chip wraps, and
  // never over Remove from voice (it was anchored to the chip's bottom).
  test('P2211 the sending mark stays on the avatar of a wrapped chip and covers no button at 390×844', async ({ browser }) => {
    if (vw === undefined) throw new Error('no voice world');
    const me = await vw.addMember(`Alexandria Wellington ${String(++crowds)}`);
    expect((await vw.api(vw.operator, 'PUT', `/v1/channels/${vw.channel}/members/${String(me.id)}`, { manages: true })).status).toBe(204);
    const person = await enter(browser, vw, me, { context: phones[0][1], mode: 'unchosen' });
    try {
      const { page } = person;
      await joinVoice(person, 'Join voice');
      const voice = page.getByRole('region', { name: 'Voice' });
      const chip = voice.getByRole('list', { name: / in voice$/ }).getByRole('listitem').first();
      const remove = chip.getByRole('button', { name: /^Remove .* from voice$/ });
      await expect(chip.locator('.state')).toHaveCount(1, { timeout: 15_000 });
      const read = () => page.evaluate(() => {
        const chipEl = document.querySelector('.voice-roster li');
        const dot = chipEl?.querySelector('.state')?.getBoundingClientRect();
        const face = chipEl?.querySelector('.avatar')?.getBoundingClientRect();
        const button = chipEl?.querySelector('button')?.getBoundingClientRect();
        if (dot === undefined || face === undefined || button === undefined) return null;
        const at = document.elementFromPoint(button.x + button.width / 2, button.y + button.height / 2);
        return { dot: [dot.left, dot.top, dot.right, dot.bottom], face: [face.left, face.top, face.right, face.bottom], onButton: at?.tagName === 'BUTTON', chipHeight: chipEl?.getBoundingClientRect().height ?? 0, radius: dot.width / 2 };
      });
      const got = await read();
      if (got === null) throw new Error('no mark, avatar or button');
      // The chip is the wrapped one: taller than a single line.
      expect(got.chipHeight, 'the name should wrap the chip').toBeGreaterThan(40);
      const [dl, dt, dr, db] = got.dot;
      const [fl, ft, fr, fb] = got.face;
      for (const [d, f, side] of [[dl, fl, 'left'], [dt, ft, 'top']] as const) expect(d, side).toBeGreaterThanOrEqual((f ?? 0) - got.radius - 0.5);
      for (const [d, f, side] of [[dr, fr, 'right'], [db, fb, 'bottom']] as const) expect(d, side).toBeLessThanOrEqual((f ?? 0) + got.radius + 0.5);
      expect(got.onButton, 'the mark must not take a press meant for Remove from voice').toBe(true);
      await expect(remove).toBeVisible();
    } finally {
      await person.close();
    }
  });

  for (const { name, context } of hold) {
    test(`P2211 a fourth person joining and leaving moves no control at ${name}`, async ({ browser }) => {
      if (vw === undefined) throw new Error('no voice world');
      const [me] = await vw.members(1);
      if (me === undefined) throw new Error('no member');
      const person = await enter(browser, vw, me, { context });
      const others: Someone[] = [];
      try {
        const { page } = person;
        // Names of seven characters, so that three chips already fill two rows
        // and a fourth takes a place in the second: only the roster's gap
        // could move anything.
        for (const n of ['Aaaaa', 'Bbbbb']) {
          const m = await vw.addMember(`${n}${String(++crowds).padStart(2, '0')}`);
          const other = await enter(browser, vw, m, { context: { viewport: { width: 1440, height: 900 } } });
          others.push(other);
          await joinVoice(other, 'Join voice');
        }
        await joinVoice(person, 'Join voice');
        const voice = page.getByRole('region', { name: 'Voice' });
        const roster = voice.getByRole('list', { name: / in voice$/ });
        await expect(roster.getByRole('listitem')).toHaveCount(3, { timeout: 30_000 });
        await page.waitForTimeout(1_000);
        const sampler = sampleEvery(page, ['Share quality', 'Voice options', 'Leave voice']);
        const m = await vw.addMember(`Ccccc${String(++crowds).padStart(2, '0')}`);
        const fourth = await enter(browser, vw, m, { context: { viewport: { width: 1440, height: 900 } } });
        others.push(fourth);
        await joinVoice(fourth, 'Join voice');
        await expect(roster.getByRole('listitem')).toHaveCount(4, { timeout: 30_000 });
        await page.waitForTimeout(1_000);
        await leaveVoice(fourth);
        await expect(roster.getByRole('listitem')).toHaveCount(3, { timeout: 30_000 });
        await page.waitForTimeout(1_000);
        expect(await sampler.stop()).toEqual([]);
      } finally {
        await person.close();
        for (const o of others) await o.close();
      }
    });
  }
});
