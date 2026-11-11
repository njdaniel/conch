// How much speech the browser loses at the start and end of a transmission
// (issue #965), measured against a real LiveKit: what the listener's page
// actually plays, timed against the talker's key and the talker's signal.
//
// The talker's microphone is a test signal the spec switches on and off (an
// init script hands the page a 440 Hz tone through a gain it controls, in
// place of the fake device's file), so an onset happens at a known time.
// The listener's page reads the level of what it receives every 5 ms. Both
// pages are in one browser on one machine, and time is read from the same
// clock (performance.timeOrigin + performance.now()).
//
// Each time is corrected by the path's own latency (network, jitter buffer,
// playout), measured in the same transmission (below). "Lost at the start"
// is then the delay from the key (or the signal) to the first sound heard,
// less that latency; "kept after the key" is how long sound goes on after
// the key is let go, less the latency. The figures go into the test's
// annotations and its output; the assertions hold only what issue #965
// fixed (and issue #970's: a press opens at once, so reports held back do
// not cost speech), with room for the measurement's own noise (about 20 ms).
//
// Without Docker it fails when CI is set and is skipped otherwise.
import type { Page } from '@playwright/test';
import { chooseMicrophone, expect, test } from './fixtures';
import { enter, joinVoice, leave, type Someone } from './people';
import { burstAudioFailure, burstMeasure, median, p90, pressMeasure, type Sample } from './onset';
import { recordHeard, startListening, syntheticMicrophone } from './synthetic-mic';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

async function heard(page: Page): Promise<Sample[]> {
  return page.evaluate(() => (window as unknown as { __heard: Sample[] }).__heard.slice());
}

async function marks(page: Page): Promise<{ down: number[]; up: number[]; on: number[]; off: number[] }> {
  return page.evaluate(() => (window as unknown as { __marks: { down: number[]; up: number[]; on: number[]; off: number[] } }).__marks);
}

const signal = (page: Page, on: boolean) => page.evaluate((v) => (window as unknown as { __signal: (on: boolean) => number }).__signal(v), on);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const round = (xs: readonly number[]) => xs.map((x) => Math.round(x));

test.describe('speech kept at the edges of a transmission', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({ }, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  test('push to talk and the open microphone, timed at the listener', async ({ browser }) => {
    // Full retry allowances sleep for 450 s; leave 150 s for setup and browser work.
    test.setTimeout(600_000);
    const people: Someone[] = [];
    try {
      // Both on the open microphone, the default (issue #1244): neither chose push to talk.
      const alice = await enter(browser, vw, vw.alice, { mode: 'unchosen', before: async (page) => { await page.addInitScript(syntheticMicrophone); } });
      people.push(alice);
      const bob = await enter(browser, vw, vw.bob, { mode: 'unchosen', before: async (page) => { await page.addInitScript(recordHeard); } });
      people.push(bob);
      const talker = alice.page;
      const listener = bob.page;
      const tv = voiceRegion(talker);
      await joinVoice(alice, 'Join voice');
      await joinVoice(bob, 'Join voice');
      // The open microphone is the default (issue #1244); the first half is of push to talk.
      for (const p of [talker, listener]) await chooseMicrophone(voiceRegion(p), 'push to talk');
      await expect(voiceRegion(listener).getByRole('list', { name: 'In voice' })).toContainText('alice');
      await talker.getByRole('heading', { name: 'Conch' }).click();
      await startListening(listener);

      // Every time is corrected by the path's own latency, measured within
      // the same transmission: the signal stops for 100 ms while the
      // microphone is open, and the end of what was heard before the gap,
      // less when the signal stopped, is the latency then. The jitter buffer
      // changes it by tens of ms over a run, so one figure for the whole run
      // is not good enough.
      const reportTimes = () =>
        talker.evaluate(() =>
          performance
            .getEntriesByType('resource')
            .filter((e) => e.name.endsWith('/voice/transmit'))
            .map((e) => e.duration),
        );

      // Push to talk with the signal on throughout but for the gap: what is
      // lost after the key goes down, what is kept after it comes up, and how
      // far ahead of what is sent the indicator says Transmitting.
      //
      // A press whose readings a stall of the listener's page made unclear is
      // refused (onset.ts) and made again, up to three times as many presses
      // as wanted; fewer than 20 good ones fails, saying so.
      const pttBatch = async (n: number) => {
        const reportsBefore = (await reportTimes()).length;
        const lost: number[] = [];
        const kept: number[] = [];
        const ahead: number[] = [];
        const path: number[] = [];
        let refused = 0;
        await signal(talker, true);
        for (let tries = 0; tries < 3 * n && path.length < n; tries++) {
          await sleep(700);
          const before = await marks(talker);
          await talker.keyboard.down('Backquote');
          await sleep(600);
          const gap = await signal(talker, false);
          await sleep(100);
          await signal(talker, true);
          await sleep(400);
          await talker.keyboard.up('Backquote');
          await sleep(800);
          const m = await marks(talker);
          const down = m.down[before.down.length] ?? NaN;
          const up = m.up[before.up.length] ?? NaN;
          const got = pressMeasure(await heard(listener), down, up, gap);
          if (got === null) {
            refused++;
            continue;
          }
          path.push(got.path);
          lost.push(got.lost);
          kept.push(got.kept);
          // When the audio sent began, against when the strip said so.
          const shown = m.on[before.on.length];
          if (shown !== undefined) ahead.push(down + got.lost - shown);
        }
        await signal(talker, false);
        return { pathMs: round(path), lost: round(lost), kept: round(kept), indicatorAhead: round(ahead), refused, reportMs: round((await reportTimes()).slice(reportsBefore)) };
      };
      const fast = await pttBatch(20);
      // Again with every transmit report held back 150 ms on its way to
      // conchd, as on a slow link: the press does not wait for its answer.
      await talker.route('**/voice/transmit', async (route) => {
        await sleep(150);
        await route.continue();
      });
      const slow = await pttBatch(20);
      await talker.unroute('**/voice/transmit');

      // The open microphone: bursts of the signal from quiet. The end of a
      // burst is heard while the gate is open (the hang), so it gives the
      // latency of that burst.
      await tv.getByRole('button', { name: 'Voice options' }).click();
      await tv.getByLabel('Open microphone').check();
      await sleep(1500);
      const onsetLost: number[] = [];
      const openPath: number[] = [];
      // How long the gate took to open: from the signal to the `started`
      // report's request, which the gate's opening sends.
      const detect: number[] = [];
      const startedAt = (after: number) =>
        talker.evaluate(
          (t) =>
            performance
              .getEntriesByType('resource')
              .filter((e) => e.name.endsWith('/voice/transmit'))
              .map((e) => performance.timeOrigin + e.startTime)
              .find((s) => s >= t) ?? null,
          after,
        );
      let burstsRefused = 0;
      for (let tries = 0; tries < 60 && onsetLost.length < 20; tries++) {
        const on = await signal(talker, true);
        await sleep(800);
        const off = await signal(talker, false);
        await sleep(1500);
        const samples = await heard(listener);
        const got = burstMeasure(samples, on, off);
        if (got === null) {
          expect(burstAudioFailure(samples, on, off), 'open microphone audio missing or dropped out while sampling was reliable').toBe(false);
          burstsRefused++;
          continue;
        }
        openPath.push(got.path);
        onsetLost.push(got.lost);
        const opened = await startedAt(on);
        if (opened !== null) detect.push(opened - on);
      }
      await tv.getByLabel('Push to talk').check();

      const report = {
        ptt: fast,
        pttReportsHeld150ms: slow,
        openMic: { pathMs: round(openPath), gateOpenedAfterMs: round(detect), lostAtOnset: round(onsetLost), refused: burstsRefused },
      };
      test.info().annotations.push({ type: 'measurement', description: JSON.stringify(report) });
      console.log(`voice onset measurement: ${JSON.stringify(report)}`);
      const tooFew = 'presses measured cleanly (the rest were refused: the listening page stalled)';
      expect(fast.kept.length, tooFew).toBe(20);
      expect(slow.kept.length, tooFew).toBe(20);
      expect(onsetLost.length, 'open microphone bursts measured cleanly').toBe(20);
      // At least 18 of the 20 onsets lose no more than 80 ms beyond the path.
      // With the old 50 ms reads the 90th percentile was 86 ms.
      const onsetP90 = p90(onsetLost);
      expect(onsetP90, '90th percentile of ms lost at an open-microphone onset').toBeLessThanOrEqual(80);
      // The bounds are wide, for a loaded runner; what they tell apart is
      // far wider still. The release delay is heard (issue #965): a 200 ms
      // delay is heard as about 120 to 180 ms; before, a 100 ms one was heard
      // as 6 to 67 ms.
      expect(median(fast.kept), 'ms heard after the key came up').toBeGreaterThanOrEqual(90);
      expect(median(slow.kept), 'ms heard after the key came up, reports slow').toBeGreaterThanOrEqual(90);
      // Transmitting is said once audio is sent, not at the key: it is within
      // about 20 ms of the audio now; before, with reports held back 150 ms,
      // it was said 133 to 173 ms early.
      expect(median(slow.indicatorAhead), 'ms the indicator was ahead of the audio, reports slow').toBeLessThan(80);
      // The press opens at once and reports in parallel (issue #970): with
      // reports held back 150 ms the start loss is still within about 30 ms
      // of the path's latency (the measurement's own noise is about 20 ms);
      // before, it was the held-back 134 to 174 ms.
      expect(median(slow.lost), 'ms of speech lost at the start of a press, beyond the path, reports slow').toBeLessThanOrEqual(30);
      await tv.getByRole('button', { name: 'Leave voice' }).click();
      await voiceRegion(listener).getByRole('button', { name: 'Leave voice' }).click();
    } finally {
      for (const p of people) await leave(p);
    }
  });
});
