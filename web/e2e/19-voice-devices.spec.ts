// Choosing the microphone (issue #658). Chromium's fake devices list three
// microphones ("Fake Default Audio Input", whose id is "default", then "Fake
// Audio Input 1" and "Fake Audio Input 2"), each playing the suite's tone. A
// member chooses one, is heard on it by another page, keeps the choice across
// a reload, and switches while transmitting, which ends the press and is
// audited as a reported pair. Then the device goes, twice: once dropped from
// the list with a devicechange event (as the sound server can leave a stream
// running on another device), once by its capture ending. Each time the page
// says the microphone was disconnected, captures nothing more, and sends
// nothing while the talk key is held.
//
// Voice processing (issue #659): a member turns it off, the browser is asked
// for a capture with echo cancellation, noise suppression and auto gain off,
// the page says the capture reports them off, and another page still hears a
// press.
//
// An init script, installed before the page's own scripts as
// 17-voice-microphone.spec.ts installs its own, counts and keeps every
// capture the page asks the browser for, and what it asked, hides devices
// from the list on request, and keeps the captures so a test can end one. Against a conchd with LiveKit of its own
// (voice-world.ts): without Docker it fails when CI is set and is skipped
// otherwise.
import type { Locator, Page } from '@playwright/test';
import { channelButton, expect, signIn, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type VoiceWorld } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

const missingNote = 'Your microphone was disconnected. Choose a microphone in Voice options.';

// devices wraps the page's device calls: every capture is counted and kept,
// and a device whose id is in __hidden is left out of the list.
const devices = () => {
  const w = window as unknown as { __captures: number; __asked: unknown[]; __hidden: string[]; __tracks: MediaStreamTrack[] };
  w.__captures = 0;
  w.__asked = [];
  w.__hidden = [];
  w.__tracks = [];
  const media = navigator.mediaDevices as MediaDevices | undefined;
  if (media === undefined) return;
  const capture = media.getUserMedia.bind(media);
  const list = media.enumerateDevices.bind(media);
  media.getUserMedia = async (constraints?: MediaStreamConstraints) => {
    w.__captures++;
    w.__asked.push(JSON.parse(JSON.stringify(constraints ?? null)) as unknown);
    const stream = await capture(constraints);
    w.__tracks.push(...stream.getAudioTracks());
    return stream;
  };
  media.enumerateDevices = async () => (await list()).filter((d) => !w.__hidden.includes(d.deviceId));
};

const captures = (page: Page): Promise<number> => page.evaluate(() => (window as unknown as { __captures: number }).__captures);

// What every capture asked the browser for, in order.
const asked = (page: Page): Promise<unknown[]> => page.evaluate(() => (window as unknown as { __asked: unknown[] }).__asked);

// unplug drops a device from the list and tells the page its devices changed.
const unplug = (page: Page, id: string): Promise<void> =>
  page.evaluate((deviceId) => {
    (window as unknown as { __hidden: string[] }).__hidden.push(deviceId);
    navigator.mediaDevices.dispatchEvent(new Event('devicechange'));
  }, id);

// endCapture ends the capture the page asked for last, as a browser does when
// its device goes.
const endCapture = (page: Page): Promise<void> =>
  page.evaluate(() => {
    const track = (window as unknown as { __tracks: MediaStreamTrack[] }).__tracks.at(-1);
    if (track === undefined) throw new Error('nothing was captured');
    track.dispatchEvent(new Event('ended'));
  });

// Counts the audio bytes a page has received, as 10-voice.spec.ts does.
const countBytes = () => {
  const w = window as unknown as { __pcs: RTCPeerConnection[] };
  w.__pcs = [];
  const Original = window.RTCPeerConnection;
  const Wrapped = function (this: unknown, ...args: ConstructorParameters<typeof RTCPeerConnection>) {
    const pc = new Original(...args);
    w.__pcs.push(pc);
    return pc;
  } as unknown as typeof RTCPeerConnection;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.RTCPeerConnection = Wrapped;
};

async function inboundAudioBytes(page: Page): Promise<number> {
  return page.evaluate(async () => {
    const w = window as unknown as { __pcs: RTCPeerConnection[] };
    let total = 0;
    for (const pc of w.__pcs) {
      const stats = await pc.getStats();
      stats.forEach((r: { type: string; kind?: string; bytesReceived?: number }) => {
        if (r.type === 'inbound-rtp' && r.kind === 'audio') total += r.bytesReceived ?? 0;
      });
    }
    return total;
  });
}

async function delta(page: Page, ms: number): Promise<number> {
  const before = await inboundAudioBytes(page);
  await new Promise((r) => setTimeout(r, ms));
  return (await inboundAudioBytes(page)) - before;
}

test.describe('choosing the microphone', () => {
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

  async function person(browser: import('@playwright/test').Browser, who: 'alice' | 'bob') {
    const context = await browser.newContext();
    const page = await context.newPage();
    const watch = await watchPage(page, vw);
    await page.addInitScript(countBytes);
    await page.addInitScript(devices);
    await signIn(page, vw, vw[who].token, vw[who].name);
    await channelButton(page, vw.channel).click();
    return { context, page, watch };
  }

  // join joins voice and opens Voice options; the microphone list, once read.
  async function join(page: Page): Promise<Locator> {
    const voice = voiceRegion(page);
    await voice.getByRole('button', { name: 'Join voice' }).click();
    await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
    await voice.getByRole('button', { name: 'Voice options' }).click();
    const list = voice.getByRole('combobox', { name: 'Microphone' });
    await expect(list).toBeVisible();
    return list;
  }

  // The id of a listed microphone, from the list's own entry.
  async function idOf(list: Locator, label: string): Promise<string> {
    const value = await list.locator('option', { hasText: label }).getAttribute('value');
    if (value === null) throw new Error(`no entry for ${label}`);
    return value.replace(/^device:/, '');
  }

  // holdTalk holds the talk key on a page, with focus away from any field.
  async function holdTalk(page: Page): Promise<void> {
    await page.getByRole('heading', { name: 'Conch' }).click();
    await page.keyboard.down('Backquote');
  }

  test('a chosen microphone is heard, kept across a reload, switched while talking, and never replaced when it goes', async ({ browser }) => {
    test.setTimeout(180_000);
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      const voice = voiceRegion(a.page);
      const actor = `principal:${String(vw.alice.id)}`;
      const list = await join(a.page);
      await join(b.page);

      // Every microphone, after System default; System default is the
      // browser's default entry.
      await expect(list.locator('option')).toHaveText(['System default', 'Fake Default Audio Input', 'Fake Audio Input 1', 'Fake Audio Input 2']);
      await expect(list).toHaveValue('system');
      await expect(voice).toContainText('In use: Fake Default Audio Input');
      await expect(voice).toContainText("System default follows your computer's sound settings.");

      // Choose Fake Audio Input 1: in use within 2 s, still in voice, and heard.
      await list.selectOption({ label: 'Fake Audio Input 1' });
      await expect(voice).toContainText('In use: Fake Audio Input 1', { timeout: 2_000 });
      await expect(voice).toContainText(/Connected in #voice-room/);
      await expect(voice.getByText('Microphone in use')).toBeVisible();
      await expect.poll(async () => (await vw.audit()).some((e) => e.actor === actor && e.action === 'voice_joined'), { timeout: 15_000 }).toBe(true);
      const quiet = await delta(b.page, 2000);
      await holdTalk(a.page);
      await expect(voice).toContainText('Transmitting');
      const loud = await delta(b.page, 2000);
      await a.page.keyboard.up('Backquote');
      await expect(voice).toContainText('Not transmitting');
      expect(loud, `bytes while A talks on Fake Audio Input 1 (${String(loud)}) versus quiet (${String(quiet)})`).toBeGreaterThan(quiet * 4 + 2000);

      // A reload keeps the choice for the tab. Chromium gives every device a
      // new id on a reload (the fake permission is not kept), so the choice
      // is found again by its name.
      await a.page.reload();
      await channelButton(a.page, vw.channel).click();
      const again = await join(a.page);
      await expect(again.locator('option:checked')).toHaveText('Fake Audio Input 1');
      await expect(voice).toContainText('In use: Fake Audio Input 1', { timeout: 2_000 });

      // A switch while transmitting ends the press: reported as a pair, and
      // nothing sent again until a new press.
      await holdTalk(a.page);
      await expect(voice).toContainText('Transmitting');
      await again.selectOption({ label: 'Fake Audio Input 2' });
      await expect(voice).toContainText('Not transmitting');
      await expect(voice).toContainText('In use: Fake Audio Input 2', { timeout: 2_000 });
      await new Promise((r) => setTimeout(r, 1000));
      await expect(voice).toContainText('Not transmitting');
      await a.page.keyboard.up('Backquote');
      const transmits = async () => (await vw.audit()).filter((e) => e.actor === actor && e.action.startsWith('voice_transmit')).map((e) => e.action);
      const pair = ['voice_transmit_started', 'voice_transmit_stopped'];
      await expect.poll(transmits, { timeout: 15_000 }).toEqual([...pair, ...pair]);

      // The device chosen leaves the list: the microphone is missing, and
      // nothing else is captured, or sent while the key is held.
      const two = await idOf(again, 'Fake Audio Input 2');
      const before = await captures(a.page);
      await unplug(a.page, two);
      await expect(voice.getByRole('status').filter({ hasText: missingNote })).toBeVisible();
      await expect(voice.getByText('Microphone in use')).toHaveCount(0);
      await expect(again.locator('option', { hasText: 'Fake Audio Input 2' })).toHaveCount(0);
      await holdTalk(a.page);
      const unplugged = await delta(b.page, 3000);
      await expect(voice).toContainText('Not transmitting');
      await a.page.keyboard.up('Backquote');
      expect(unplugged, `bytes while A holds the key with the microphone gone (${String(unplugged)}) versus talking (${String(loud)})`).toBeLessThan(loud / 4);
      expect(await captures(a.page), 'captures after the device went').toBe(before);

      // System default is a choice: captured again, and the note goes.
      await again.selectOption({ label: 'System default' });
      await expect(voice.getByText(missingNote)).toHaveCount(0);
      await expect(voice.getByText('Microphone in use')).toBeVisible();
      await expect(voice).toContainText('In use: Fake Default Audio Input', { timeout: 2_000 });

      // Its capture ends: missing again, and nothing is captured after.
      const chosen = await captures(a.page);
      await endCapture(a.page);
      await expect(voice.getByRole('status').filter({ hasText: missingNote })).toBeVisible();
      await expect(voice.getByText('Microphone in use')).toHaveCount(0);
      await holdTalk(a.page);
      const ended = await delta(b.page, 3000);
      await expect(voice).toContainText('Not transmitting');
      await a.page.keyboard.up('Backquote');
      expect(ended, `bytes while A holds the key with the capture ended (${String(ended)}) versus talking (${String(loud)})`).toBeLessThan(loud / 4);
      expect(await captures(a.page), 'captures after the capture ended').toBe(chosen);

      // Nothing was audited as unreported; the reported pairs are all there are.
      await new Promise((r) => setTimeout(r, 3000));
      expect(await transmits()).toEqual([...pair, ...pair]);
      expect((await vw.audit()).filter((e) => e.action === 'voice_transmit_unreported')).toEqual([]);
      expect(a.watch.console.filter((l) => /Content Security Policy|Refused to/.test(l))).toEqual([]);

      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
    } finally {
      await a.context.close();
      await b.context.close();
    }
  });

  test('voice processing turned off captures the microphone without it, says so, and is still heard', async ({ browser }) => {
    test.setTimeout(120_000);
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      const voice = voiceRegion(a.page);
      const actor = `principal:${String(vw.alice.id)}`;
      const transmits = async () => (await vw.audit()).filter((e) => e.actor === actor && e.action.startsWith('voice_transmit')).map((e) => e.action);
      const before = await transmits();
      await join(a.page);
      await join(b.page);
      const box = voice.getByRole('checkbox', { name: 'Voice processing (echo cancellation, noise suppression, automatic gain)' });
      await expect(box).toBeChecked();
      const first = (await asked(a.page)).length;

      // Off: one new capture, all three filters off, in voice with the
      // microphone still in use, and the capture says them off.
      await box.uncheck();
      await expect(voice).toContainText('Off: your microphone is sent as it is.', { timeout: 5_000 });
      await expect(voice).toContainText(/Connected in #voice-room/);
      await expect(voice.getByText('Microphone in use')).toBeVisible();
      const requests = await asked(a.page);
      expect(requests).toHaveLength(first + 1);
      expect(requests.at(-1)).toMatchObject({ audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false } });

      // Another page hears a press.
      await expect.poll(async () => (await vw.audit()).some((e) => e.actor === actor && e.action === 'voice_joined'), { timeout: 15_000 }).toBe(true);
      const quiet = await delta(b.page, 2000);
      await holdTalk(a.page);
      await expect(voice).toContainText('Transmitting');
      const loud = await delta(b.page, 2000);
      await a.page.keyboard.up('Backquote');
      await expect(voice).toContainText('Not transmitting');
      expect(loud, `bytes while A talks with processing off (${String(loud)}) versus quiet (${String(quiet)})`).toBeGreaterThan(quiet * 4 + 2000);

      // One reported pair, and nothing audited as unreported.
      await expect.poll(transmits, { timeout: 15_000 }).toEqual([...before, 'voice_transmit_started', 'voice_transmit_stopped']);
      expect((await vw.audit()).filter((e) => e.action === 'voice_transmit_unreported')).toEqual([]);
      expect(a.watch.console.filter((l) => /Content Security Policy|Refused to/.test(l))).toEqual([]);

      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
    } finally {
      await a.context.close();
      await b.context.close();
    }
  });
});
