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
// fixed, with room for the measurement's own noise (about 20 ms).
//
// Without Docker it fails when CI is set and is skipped otherwise.
import type { Page } from '@playwright/test';
import { expect, openChannel, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

// syntheticMicrophone answers a request for audio with a tone whose gain the
// spec controls (window.__signal), and marks the talk key's presses and the
// indicator's changes. The device's own capture is asked for as before, so
// permissions and device errors are the browser's, and is stopped with the
// tone.
const syntheticMicrophone = () => {
  const now = () => performance.timeOrigin + performance.now();
  const w = window as unknown as {
    __signal?: (on: boolean) => number;
    __marks: { down: number[]; up: number[]; on: number[]; off: number[] };
  };
  w.__marks = { down: [], up: [], on: [], off: [] };
  window.addEventListener(
    'keydown',
    (e) => {
      if (e.code === 'Backquote' && !e.repeat) w.__marks.down.push(now());
    },
    true,
  );
  window.addEventListener(
    'keyup',
    (e) => {
      if (e.code === 'Backquote') w.__marks.up.push(now());
    },
    true,
  );
  // The indicator: "Transmitting", as the strip says it.
  let shown = false;
  new MutationObserver(() => {
    const text = document.querySelector('.voice-tx')?.textContent ?? '';
    const on = text.endsWith('Transmitting') && !text.endsWith('Not transmitting');
    if (on !== shown) {
      shown = on;
      (on ? w.__marks.on : w.__marks.off).push(now());
    }
  }).observe(document, { subtree: true, childList: true, characterData: true });
  const devices = navigator.mediaDevices;
  const original = devices.getUserMedia.bind(devices);
  devices.getUserMedia = async (constraints?: MediaStreamConstraints) => {
    const real = await original(constraints);
    if (constraints?.audio === undefined || constraints.audio === false) return real;
    const context = new AudioContext();
    const tone = context.createOscillator();
    tone.frequency.value = 440;
    const gain = context.createGain();
    gain.gain.value = 0;
    const out = context.createMediaStreamDestination();
    tone.connect(gain).connect(out);
    tone.start();
    void context.resume();
    w.__signal = (on: boolean) => {
      gain.gain.setValueAtTime(on ? 0.6 : 0, context.currentTime);
      return now();
    };
    const track = out.stream.getAudioTracks()[0];
    if (track === undefined) return real;
    const stop = track.stop.bind(track);
    track.stop = () => {
      for (const t of real.getTracks()) t.stop();
      stop();
    };
    return new MediaStream([track]);
  };
};

// recordHeard keeps every audio track the listener's page receives, so what
// it plays can be read.
const recordHeard = () => {
  const w = window as unknown as { __heardTracks: MediaStreamTrack[] };
  w.__heardTracks = [];
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    pc.addEventListener('track', (e) => {
      if (e.track.kind === 'audio') w.__heardTracks.push(e.track);
    });
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
};

interface Sample {
  readonly t: number;
  readonly rms: number;
}

// startListening reads the level of what the listener's page receives every
// 5 ms, from 256 samples (5 ms at 48 kHz), into window.__heard: the loudest
// of the tracks received.
async function startListening(page: Page): Promise<void> {
  await page.evaluate(() => {
    const w = window as unknown as { __heardTracks: MediaStreamTrack[]; __heard: { t: number; rms: number }[] };
    w.__heard = [];
    const context = new AudioContext();
    void context.resume();
    const analysers: AnalyserNode[] = [];
    let wired = 0;
    const buffer = new Float32Array(256);
    const read = () => {
      for (; wired < w.__heardTracks.length; wired++) {
        const track = w.__heardTracks[wired];
        if (track === undefined) continue;
        const analyser = context.createAnalyser();
        analyser.fftSize = 256;
        context.createMediaStreamSource(new MediaStream([track])).connect(analyser);
        analysers.push(analyser);
      }
      let rms = 0;
      for (const a of analysers) {
        a.getFloatTimeDomainData(buffer);
        let sum = 0;
        for (const v of buffer) sum += v * v;
        rms = Math.max(rms, Math.sqrt(sum / buffer.length));
      }
      w.__heard.push({ t: performance.timeOrigin + performance.now(), rms });
      setTimeout(read, 5);
    };
    read();
  });
}

async function heard(page: Page): Promise<Sample[]> {
  return page.evaluate(() => (window as unknown as { __heard: Sample[] }).__heard.slice());
}

// loudSpans is when the listener heard the signal: runs of loud samples, a
// gap of up to 20 ms inside one.
function loudSpans(samples: readonly Sample[]): { from: number; to: number }[] {
  const spans: { from: number; to: number }[] = [];
  for (const s of samples) {
    if (s.rms < 0.05) continue;
    const last = spans.at(-1);
    if (last !== undefined && s.t - last.to < 20) last.to = s.t;
    else spans.push({ from: s.t, to: s.t });
  }
  return spans;
}

async function marks(page: Page): Promise<{ down: number[]; up: number[]; on: number[]; off: number[] }> {
  return page.evaluate(() => (window as unknown as { __marks: { down: number[]; up: number[]; on: number[]; off: number[] } }).__marks);
}

const signal = (page: Page, on: boolean) => page.evaluate((v) => (window as unknown as { __signal: (on: boolean) => number }).__signal(v), on);
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const median = (xs: readonly number[]) => {
  const s = [...xs].sort((a, b) => a - b);
  return s[Math.floor(s.length / 2)] ?? NaN;
};
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
    test.setTimeout(240_000);
    const talkerContext = await browser.newContext();
    const listenerContext = await browser.newContext();
    try {
      const talker = await talkerContext.newPage();
      const listener = await listenerContext.newPage();
      await watchPage(talker, vw);
      await watchPage(listener, vw);
      await talker.addInitScript(syntheticMicrophone);
      await listener.addInitScript(recordHeard);
      await signIn(talker, vw, vw.alice.token, vw.alice.name);
      await signIn(listener, vw, vw.bob.token, vw.bob.name);
      await openChannel(talker, vw.channel);
      await openChannel(listener, vw.channel);
      const tv = voiceRegion(talker);
      await tv.getByRole('button', { name: 'Join voice' }).click();
      await voiceRegion(listener).getByRole('button', { name: 'Join voice' }).click();
      for (const p of [talker, listener]) await expect(voiceRegion(p)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
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
      const pttBatch = async (n: number) => {
        const reportsBefore = (await reportTimes()).length;
        const lost: number[] = [];
        const kept: number[] = [];
        const ahead: number[] = [];
        const path: number[] = [];
        await signal(talker, true);
        for (let i = 0; i < n; i++) {
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
          const ss = loudSpans(await heard(listener)).filter((s) => s.from >= down && s.from < up + 700);
          const first = ss[0];
          const last = ss.at(-1);
          if (first === undefined || last === undefined || ss.length < 2) continue;
          const p = first.to - gap;
          path.push(p);
          lost.push(first.from - down - p);
          kept.push(last.to - p - up);
          const shown = m.on[before.on.length];
          if (shown !== undefined) ahead.push(first.from - p - shown);
        }
        await signal(talker, false);
        return { pathMs: round(path), lost: round(lost), kept: round(kept), indicatorAhead: round(ahead), reportMs: round((await reportTimes()).slice(reportsBefore)) };
      };
      const fast = await pttBatch(5);
      // Again with every transmit report held back 150 ms on its way to
      // conchd, as on a slow link: the press waits for its answer.
      await talker.route('**/voice/transmit', async (route) => {
        await sleep(150);
        await route.continue();
      });
      const slow = await pttBatch(5);
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
      for (let i = 0; i < 7; i++) {
        const on = await signal(talker, true);
        await sleep(800);
        const off = await signal(talker, false);
        await sleep(1500);
        const ss = loudSpans(await heard(listener)).filter((s) => s.from >= on && s.from < off + 1000);
        const first = ss[0];
        const last = ss.at(-1);
        if (first === undefined || last === undefined) continue;
        const p = last.to - off;
        openPath.push(p);
        onsetLost.push(first.from - on - p);
        const opened = await startedAt(on);
        if (opened !== null) detect.push(opened - on);
      }
      await tv.getByLabel('Push to talk').check();

      const report = {
        ptt: fast,
        pttReportsHeld150ms: slow,
        openMic: { pathMs: round(openPath), gateOpenedAfterMs: round(detect), lostAtOnset: round(onsetLost) },
      };
      test.info().annotations.push({ type: 'measurement', description: JSON.stringify(report) });
      console.log(`voice onset measurement: ${JSON.stringify(report)}`);
      expect(fast.lost.length).toBe(5);
      expect(slow.lost.length).toBe(5);
      expect(onsetLost.length).toBe(7);
      // The release delay is heard (issue #965): before, a 100 ms delay was
      // heard as 36 to 51 ms.
      expect(median(fast.kept), 'ms heard after the key came up').toBeGreaterThanOrEqual(100);
      expect(median(slow.kept), 'ms heard after the key came up, reports slow').toBeGreaterThanOrEqual(100);
      // Transmitting is said once audio is sent, not at the key: before, with
      // reports held back 150 ms, it was said 125 to 149 ms early.
      expect(median(slow.indicatorAhead), 'ms the indicator was ahead of the audio, reports slow').toBeLessThan(60);
      await tv.getByRole('button', { name: 'Leave voice' }).click();
      await voiceRegion(listener).getByRole('button', { name: 'Leave voice' }).click();
    } finally {
      await talkerContext.close();
      await listenerContext.close();
    }
  });
});
