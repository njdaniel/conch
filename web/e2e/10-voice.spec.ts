// Browser voice (issue #442). Two people in two browser contexts, each on
// Chromium's fake microphone (a steady tone; see playwright.config.ts), against
// a conchd of its own configured for a LiveKit server from the pinned image
// (voice-world.ts). Without Docker the voice tests fail when CI is set and are
// skipped otherwise.
//
// The first test needs no LiveKit: on the shared server, which has none, the
// page says voice is not set up and asks for no device.
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import type { PostMessageRequestV2 } from '../src/schema/generated';
import { channelButton, conversation, expect, expectFits, messages, phones, signIn, statusLine, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, watchPage, type VoiceWorld, type Watch } from './voice-world';

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

test('P3-F9c voice is not set up on a server without LiveKit, and the page asks for no microphone', async ({ meet, world }) => {
  const { page } = await meet();
  await page.addInitScript(() => {
    const w = window as unknown as { deviceRequested: boolean };
    w.deviceRequested = false;
    const refuse = () => {
      w.deviceRequested = true;
      return Promise.reject(new Error('device requested'));
    };
    Object.defineProperty(navigator, 'mediaDevices', {
      value: { getUserMedia: refuse, enumerateDevices: refuse, getDisplayMedia: refuse },
      configurable: true,
    });
  });
  await signIn(page, world, world.tokens.browser, 'browser-test');
  const voice = voiceRegion(page);
  await expect(voice).toContainText('Voice is not set up on this server');
  await expect(voice.getByRole('button')).toHaveCount(0);
  expect(await page.evaluate(() => (window as unknown as { deviceRequested: boolean }).deviceRequested)).toBe(false);
});

// Counts the audio bytes this page has received, from the peer connections the
// SDK made. The wrapper is installed before the page's own scripts run.
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

test.describe('with LiveKit', () => {
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

  // before runs on the new page before it signs in, to watch it from the start.
  async function person(browser: import('@playwright/test').Browser, who: 'alice' | 'bob' | 'carol', options?: BrowserContextOptions, before?: (page: Page) => void) {
    const context = await browser.newContext(options);
    const page = await context.newPage();
    before?.(page);
    const watch = watchPage(page, vw);
    await page.addInitScript(countBytes);
    const m = vw[who];
    await signIn(page, vw, m.token, m.name);
    await channelButton(page, vw.channel).click();
    return { context, page, watch, member: m };
  }

  // No secret of the world, and no join token or room name the page was given,
  // in anything the page said or kept: its console, its DOM and address, its
  // storage and cookies, and (unless timing is false) its resource timing
  // entries. And no policy violation in the console.
  //
  // The one secret a page keeps by design is its own credential, under
  // conch.web.token: a member's in localStorage since A7 (issue #439), an
  // operator's in sessionStorage. Each must be exactly the signed-in member's,
  // and the rest of storage is checked without it.
  //
  // timing is false after LiveKit was down: on a failed connection the SDK
  // puts the join token in its /rtc/validate request, which the page's
  // resource timing records (README, "Voice in the browser").
  async function expectClean(p: { page: Page; watch: Watch; member: { token: string } }, timing = true): Promise<void> {
    await p.watch.settled();
    const { kept, credentials } = await p.page.evaluate((withTiming) => {
      const without = (storage: Storage) => {
        const rest: Record<string, string | null> = {};
        for (let i = 0; i < storage.length; i++) {
          const key = storage.key(i);
          if (key !== null && key !== 'conch.web.token') rest[key] = storage.getItem(key);
        }
        return JSON.stringify(rest);
      };
      return {
        credentials: [sessionStorage.getItem('conch.web.token'), localStorage.getItem('conch.web.token')],
        kept: [
          document.documentElement.outerHTML,
          location.href,
          without(sessionStorage),
          without(localStorage),
          document.cookie,
          withTiming ? JSON.stringify(performance.getEntries().map((e) => e.name)) : '',
        ].join('\n'),
      };
    }, timing);
    for (const credential of credentials) {
      expect(credential === null || credential === p.member.token, 'the stored credential is the member\'s own').toBe(true);
    }
    const said = p.watch.console.join('\n');
    for (const s of vw.secrets) {
      expect(kept, 'a secret in the page, its storage or its cookies').not.toContain(s);
      expect(said, 'a secret in the console').not.toContain(s);
    }
    expect(p.watch.console.filter((l) => /Content Security Policy|Refused to/.test(l))).toEqual([]);
  }

  test('a member joins, is heard only while transmitting, shows as talking, and is audited as a reported pair', async ({ browser }) => {
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    try {
      // Both join buttons name the room they join (issue #584).
      await expect(voiceRegion(a.page).getByRole('button', { name: 'Join voice in #voice-room', exact: true })).toBeEnabled();
      await expect(voiceRegion(a.page).getByRole('button', { name: 'Watch only in #voice-room', exact: true })).toBeEnabled();
      await voiceRegion(a.page).getByRole('button', { name: 'Join voice' }).click();
      await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).click();
      for (const p of [a, b]) await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      // The roster of each page names both.
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('alice');
      await expect(voiceRegion(a.page).getByRole('list', { name: 'In voice' })).toContainText('bob');

      // Silent while the gate is shut.
      await a.page.getByRole('heading', { name: 'Conch' }).click();
      const quiet = await delta(b.page, 2000);

      // A holds the talk key; B's audio bytes rise, and the roster shows A talking.
      await a.page.keyboard.down('Backquote');
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('alice (transmitting)', { timeout: 15_000 });
      await expect(voiceRegion(a.page)).toContainText('Transmitting');
      const loud = await delta(b.page, 2000);
      await a.page.keyboard.up('Backquote');
      expect(loud, `bytes while A talks (${String(loud)}) versus quiet (${String(quiet)})`).toBeGreaterThan(quiet * 4 + 2000);
      await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).not.toContainText('alice (transmitting)', { timeout: 15_000 });
      const after = await delta(b.page, 2000);
      expect(after).toBeLessThan(loud / 4);

      // The audit has the reported pair for A and nothing unreported.
      await expect
        .poll(async () => (await vw.audit()).filter((e) => e.actor === `principal:${String(vw.alice.id)}` && e.action.startsWith('voice_transmit')).map((e) => e.action), { timeout: 15_000 })
        .toEqual(['voice_transmit_started', 'voice_transmit_stopped']);
      // Every join token and room name the pages were given is a secret from here.
      for (const p of [a, b]) await p.watch.settled();
      expect(vw.sessionSecrets(), 'join tokens and room names captured from the session answers').toBeGreaterThanOrEqual(3);
      const trail = await vw.audit();
      expect(trail.filter((e) => e.action === 'voice_transmit_unreported')).toEqual([]);
      for (const e of trail) for (const s of vw.secrets) expect(`${e.actor} ${e.subject} ${e.detail}`).not.toContain(s);

      // No policy violation, and nothing secret on the page, in its console,
      // its addresses, its storage or its cookies; each page's join token and
      // room name included.
      for (const p of [a, b]) await expectClean(p);
    } finally {
      await a.context.close();
      await b.context.close();
    }
  });

  // Issue #495: conchd's poller compares the microphone it sees with the
  // reports. With each report held back 700 ms on its way to conchd (several
  // gaps between passes), a page that opened its track before its report was
  // answered was seen transmitting unreported; it opens it only after.
  //
  // Carol is in no other test of this world, and the press waits until conchd
  // has seen her join: a press made before the poller has noticed a new
  // connection is a different case (the next test's), and is not what this
  // test is about.
  test('a push-to-talk whose reports are slow is still audited as only the reported pair', async ({ browser }) => {
    const c = await person(browser, 'carol');
    try {
      await c.page.route('**/voice/transmit', async (route) => {
        await new Promise((r) => setTimeout(r, 700));
        await route.continue();
      });
      const voice = voiceRegion(c.page);
      const actor = `principal:${String(vw.carol.id)}`;
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      await expect.poll(async () => (await vw.audit()).some((e) => e.actor === actor && e.action === 'voice_joined'), { timeout: 15_000 }).toBe(true);
      const transmits = async () => (await vw.audit()).filter((e) => e.actor === actor && e.action.startsWith('voice_transmit')).map((e) => e.action);
      // Five presses, each followed by its two reports. Two passes inside one
      // held-back report are likely but far from certain, so with fewer
      // presses the old order passed too often.
      const pair = ['voice_transmit_started', 'voice_transmit_stopped'];
      await c.page.getByRole('heading', { name: 'Conch' }).click();
      for (let i = 1; i <= 5; i++) {
        await c.page.keyboard.down('Backquote');
        await expect(voice).toContainText('Transmitting');
        await new Promise((r) => setTimeout(r, 1200));
        await c.page.keyboard.up('Backquote');
        await expect(voice).toContainText('Not transmitting');
        await expect.poll(transmits, { timeout: 15_000 }).toEqual(Array<string[]>(i).fill(pair).flat());
      }
      // Nothing the poller writes later, either.
      await new Promise((r) => setTimeout(r, 3000));
      expect(await transmits()).toEqual(Array<string[]>(5).fill(pair).flat());
      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
    } finally {
      await c.context.close();
    }
  });

  // Issue #501: a page reloaded and joined again replaces its old connection
  // in LiveKit (one participant per identity), and a press made at once is
  // often reported before the poller has seen the new connection. conchd took
  // the replacement for a leaving and closed that press as left, then
  // recorded the open microphone as unreported. Measured against that server:
  // 2 of 10 such presses. Bob transmits in no test before this one.
  test('a press made at once after a reload is still audited as only the reported pair', async ({ browser }) => {
    const b = await person(browser, 'bob');
    try {
      const voice = voiceRegion(b.page);
      const actor = `principal:${String(vw.bob.id)}`;
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
      await expect.poll(async () => (await vw.audit()).some((e) => e.actor === actor && e.action === 'voice_joined'), { timeout: 15_000 }).toBe(true);
      const transmits = async () => (await vw.audit()).filter((e) => e.actor === actor && e.action.startsWith('voice_transmit')).map((e) => `${e.action} ${e.detail.replace(/^channel=\d+ audience=channel /, '')}`);
      const pair = ['voice_transmit_started source=reported', 'voice_transmit_stopped source=reported'];
      for (let i = 1; i <= 5; i++) {
        await b.page.reload();
        await channelButton(b.page, vw.channel).click();
        await voice.getByRole('button', { name: 'Join voice' }).click();
        await expect(voice).toContainText(/Connected in #voice-room/, { timeout: 30_000 });
        await b.page.getByRole('heading', { name: 'Conch' }).click();
        await b.page.keyboard.down('Backquote');
        await expect(voice).toContainText('Transmitting');
        await new Promise((r) => setTimeout(r, 1200));
        await b.page.keyboard.up('Backquote');
        await expect(voice).toContainText('Not transmitting');
        await expect.poll(transmits, { timeout: 15_000 }).toEqual(Array<string[]>(i).fill(pair).flat());
      }
      // Nothing the poller writes later, either.
      await new Promise((r) => setTimeout(r, 3000));
      expect(await transmits()).toEqual(Array<string[]>(5).fill(pair).flat());
      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
    } finally {
      await b.context.close();
    }
  });

  test('push-to-talk by button, mute and deafen', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Connected', { timeout: 30_000 });
      const talk = voice.getByRole('button', { name: 'Hold to talk' });
      // The mouse is moved onto the button before each press: the page lays
      // out the same buttons again after a mute, and a press elsewhere is not a talk.
      const pressTalk = async () => {
        const box = await talk.boundingBox();
        if (box === null) throw new Error('no talk button');
        await a.page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
        await a.page.mouse.down();
      };
      await pressTalk();
      await expect(voice).toContainText('Transmitting');
      // Losing focus is a release.
      await a.page.evaluate(() => {
        window.dispatchEvent(new Event('blur'));
      });
      await expect(voice).toContainText('Not transmitting', { timeout: 5000 });
      await a.page.mouse.up();

      await voice.getByRole('button', { name: 'Mute' }).click();
      await expect(voice).toContainText('Muted.');
      await pressTalk();
      await expect(voice).toContainText('Not transmitting');
      await a.page.mouse.up();
      await voice.getByRole('button', { name: 'Unmute' }).click();

      // Deafened is muted too (issue #591, F13): Mute shows pressed and is
      // off, and Undeafen gives back the mute chosen before, in both orders.
      const muteButton = (name: string) => voice.getByRole('button', { name, exact: true });
      const deafenedMute = muteButton('Muted by deafen');
      for (const muted of [false, true]) {
        if (muted) await muteButton('Mute').click();
        await voice.getByRole('button', { name: 'Deafen', exact: true }).click();
        await expect(voice).toContainText('Deafened');
        await expect(deafenedMute).toHaveAttribute('aria-pressed', 'true');
        await expect(deafenedMute).toBeDisabled();
        await voice.getByRole('button', { name: 'Undeafen', exact: true }).click();
        await expect(deafenedMute).toHaveCount(0);
        const after = muteButton(muted ? 'Unmute' : 'Mute');
        await expect(after).toHaveAttribute('aria-pressed', String(muted));
        await expect(after).toBeEnabled();
      }
      await muteButton('Unmute').click();
      await expect(voice).not.toContainText('Muted.');

      // Open microphone, under Voice options (issue #536): the steady tone
      // opens it, and it is reported.
      await voice.getByRole('button', { name: 'Voice options' }).click();
      await voice.getByLabel('Open microphone').check();
      await expect(voice.getByRole('meter', { name: 'Microphone level' })).toHaveAttribute('value', /[1-9]|0\.[1-9]/, { timeout: 10_000 });
      await expect(voice).toContainText('Transmitting', { timeout: 10_000 });
      await voice.getByLabel('Push to talk').check();
      await expect(voice).toContainText('Not transmitting', { timeout: 5000 });
      await voice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
      await expectClean(a);
    } finally {
      await a.context.close();
    }
  });

  // Issue #590 (F09, F20): voice from the keyboard alone. Signing in leaves
  // focus on the channel's heading; Hold to talk is held with Space or Enter
  // while it has focus, and Tab away lets go; Escape does not leave voice;
  // and Leave voice hands focus to Join voice. Bob only listens.
  test('voice is worked from the keyboard alone, and focus is never dropped', async ({ browser }) => {
    const b = await person(browser, 'bob');
    const context = await browser.newContext();
    const page = await context.newPage();
    const watch = watchPage(page, vw);
    try {
      const voice = voiceRegion(page);
      const focused = (l: Locator) => l.evaluate((el) => el === document.activeElement);
      // Presses Tab until the control has focus, as a person would.
      const tabTo = async (l: Locator) => {
        for (let i = 0; i < 40 && !(await focused(l)); i++) await page.keyboard.press('Tab');
        await expect(l).toBeFocused();
      };

      await page.goto(vw.appUrl);
      await page.getByLabel('Credential').focus();
      await page.keyboard.type(vw.alice.token);
      await page.keyboard.press('Enter');
      await expect(statusLine(page)).toContainText(vw.alice.name);
      await expect(conversation(page).getByRole('heading', { level: 2, name: vw.channel })).toBeFocused();

      const join = voice.getByRole('button', { name: 'Join voice in #voice-room', exact: true });
      await expect(join).toBeEnabled();
      await tabTo(join);
      await page.keyboard.press('Enter');
      await expect(voice).toContainText('Connected in #voice-room', { timeout: 30_000 });
      const actor = `principal:${String(vw.alice.id)}`;
      await expect.poll(async () => (await vw.audit()).some((e) => e.actor === actor && e.action === 'voice_joined'), { timeout: 15_000 }).toBe(true);
      const transmits = async () => (await vw.audit()).filter((e) => e.actor === actor && e.action.startsWith('voice_transmit')).map((e) => e.action);
      const before = (await transmits()).length;
      const pair = ['voice_transmit_started', 'voice_transmit_stopped'];
      const heard = voiceRegion(b.page).getByRole('list', { name: 'In voice' });

      const talk = voice.getByRole('button', { name: 'Hold to talk' });
      await tabTo(talk);
      // Space, then Enter, each held: pressed again while down, as a held key
      // repeats, and it is still one press.
      for (const [i, key] of (['Space', 'Enter'] as const).entries()) {
        await page.keyboard.down(key);
        await page.keyboard.down(key);
        await page.keyboard.down(key);
        await expect(voice).toContainText('Transmitting', { timeout: 2000 });
        await expect(heard).toContainText('alice (transmitting)', { timeout: 15_000 });
        await new Promise((r) => setTimeout(r, 1200));
        await page.keyboard.up(key);
        await expect(voice).toContainText('Not transmitting');
        await expect(heard).not.toContainText('alice (transmitting)', { timeout: 15_000 });
        await expect.poll(async () => (await transmits()).slice(before), { timeout: 15_000 }).toEqual(Array<string[]>(i + 1).fill(pair).flat());
        await expect(talk).toBeFocused();
      }

      // Tab while holding lets go. Enter is held, not Space: a Space let go
      // on the next button would press it.
      await page.keyboard.down('Enter');
      await expect(voice).toContainText('Transmitting', { timeout: 2000 });
      await new Promise((r) => setTimeout(r, 1200));
      await page.keyboard.press('Tab');
      await expect(talk).not.toBeFocused();
      await expect(voice).toContainText('Not transmitting');
      await page.keyboard.up('Enter');
      await expect(heard).not.toContainText('alice (transmitting)', { timeout: 15_000 });
      await expect.poll(async () => (await transmits()).slice(before), { timeout: 15_000 }).toEqual(Array<string[]>(3).fill(pair).flat());
      expect((await vw.audit()).filter((e) => e.action === 'voice_transmit_unreported')).toEqual([]);

      // Escape is not a way out of voice (UX-21).
      await page.keyboard.press('Escape');
      await new Promise((r) => setTimeout(r, 500));
      await expect(voice).toContainText('Connected in #voice-room');

      const leave = voice.getByRole('button', { name: 'Leave voice' });
      await tabTo(leave);
      await page.keyboard.press('Enter');
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
      await expect(join).toBeFocused();
      await expectClean({ page, watch, member: vw.alice });
    } finally {
      await context.close();
      await b.context.close();
    }
  });

  // Issue #600 (F08, F12): bob and carol join voice, so that with the page's
  // own person three are in voice, by push to talk, and nobody shares.
  async function twoInVoice(browser: import('@playwright/test').Browser) {
    const others = [await person(browser, 'bob'), await person(browser, 'carol')];
    for (const o of others) {
      await voiceRegion(o.page).getByRole('button', { name: 'Join voice' }).click();
      await expect(voiceRegion(o.page)).toContainText('Connected in #voice-room', { timeout: 30_000 });
    }
    return others;
  }

  // Issue #600: in a test of its own, the strip's notes before a share are
  // one line on a desktop, the talk key and the line about sharing side by
  // side; More about sharing opens Voice options at the full notes.
  test('on a desktop the notes before a share are one line, and the full notes are one press away', async ({ browser }) => {
    const others = await twoInVoice(browser);
    try {
      for (const viewport of [{ width: 1440, height: 900 }, { width: 1280, height: 720 }]) {
        const size = `${String(viewport.width)}×${String(viewport.height)}`;
        const a = await person(browser, 'alice', { viewport });
        try {
          const voice = voiceRegion(a.page);
          await voice.getByRole('button', { name: 'Join voice' }).click();
          await expect(voice).toContainText('Connected in #voice-room', { timeout: 30_000 });
          await expect(voice.getByRole('list', { name: 'In voice' }).getByRole('listitem')).toHaveCount(3, { timeout: 15_000 });
          const more = voice.getByRole('button', { name: 'More about sharing' });
          await expect(more, `${size}: the line about sharing shows before Share screen is pressed`).toBeVisible();
          await expect(voice.getByText(/^Talk key: .* It works only while this tab has focus/)).toBeVisible();
          // The line about sharing is one line (the issue's criterion). The
          // talk-key note beside it may wrap to a line of its own where fonts
          // are wider (CI), so the whole block is held to two lines.
          const { height, share, line } = await notesHeight(voice);
          expect(share, `${size}: the line about sharing is ${String(share)} px high, one line is ${String(line)} px`).toBeLessThanOrEqual(line + 1);
          expect(height, `${size}: the notes are ${String(height)} px high, two lines are ${String(2 * line)} px`).toBeLessThanOrEqual(2 * line + 8);

          await more.click();
          await expect(voice.getByRole('button', { name: 'Voice options', expanded: true })).toBeVisible();
          const about = voice.getByRole('region', { name: 'Sharing your screen' });
          await expect(about).toBeFocused();
          await expect(about).toContainText(/Sound: .*tab shares its (own )?sound/);
          await expect(about).toContainText('shows this page too, chat included');
          await voice.getByRole('button', { name: 'Leave voice' }).click();
          await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
        } finally {
          await a.context.close();
        }
      }
    } finally {
      for (const o of others) await o.context.close();
    }
  });

  // Issue #467: on a phone the voice panel, joined, fits the screen in both
  // microphone modes, and is worked by tapping. Issue #600 (F08): with three
  // in voice by push to talk and nobody sharing, the strip is at most 200 px
  // high and nothing in it needs scrolling to reach, and a conversation read
  // to its newest message still shows it after Join voice.
  test('on a phone the joined voice panel fits the screen and is worked by tapping', async ({ browser }) => {
    const others = await twoInVoice(browser);
    try {
      for (const [size, options] of phones) {
        const bodies = Array.from({ length: 30 }, (_, i) => `p600 ${size} line ${String(i + 1)}`);
        for (const body of bodies) {
          const request: PostMessageRequestV2 = { body };
          const r = await vw.api(vw.bob.token, 'POST', `/v2/channels/${vw.channel}/messages`, request);
          expect(r.status, `post ${body}`).toBe(201);
        }
        await phone(browser, size, options, bodies[bodies.length - 1] ?? '');
      }
    } finally {
      for (const o of others) await o.context.close();
    }
  });

  async function phone(browser: import('@playwright/test').Browser, size: string, options: BrowserContextOptions, last: string): Promise<void> {
    const a = await person(browser, 'alice', options);
    try {
      const voice = voiceRegion(a.page);
      const log = messages(a.page);
      const newest = log.getByRole('listitem').filter({ hasText: last });
      await expect(newest).toBeVisible();
      expect(await log.evaluate((el) => el.scrollHeight > el.clientHeight), `${size}: the conversation is longer than the log`).toBe(true);
      await expectInside(newest, log, `${size}: the newest message before joining voice`);
      await expectFits(a.page, `${size}: the lobby before joining voice`);
      await voice.getByRole('button', { name: 'Join voice' }).tap();
      await expect(voice).toContainText('Connected in', { timeout: 30_000 });
      await expect(voice.getByRole('button', { name: 'Hold to talk' })).toBeVisible();
      await expect(voice.getByRole('list', { name: 'In voice' }).getByRole('listitem')).toHaveCount(3, { timeout: 15_000 });
      await expect(voice.getByRole('button', { name: 'More about sharing' }), `${size}: the line about sharing shows`).toBeVisible();
      await expectFits(a.page, `${size}: the voice panel, push to talk`);
      const dock = await voice.evaluate((el) => {
        const d = el.parentElement;
        return d === null ? null : { height: d.getBoundingClientRect().height, scroll: d.scrollHeight, client: d.clientHeight };
      });
      if (dock === null) throw new Error('the voice panel is in no dock');
      // 200 px is the 390×844 phone's bound. At 360 px the controls take two
      // rows (Hold to talk keeps its words), and the strip still needs no
      // scrolling.
      if (size === '390×844') expect(dock.height, `${size}: the strip is ${String(dock.height)} px high`).toBeLessThanOrEqual(200);
      expect(dock.scroll, `${size}: the strip holds ${String(dock.scroll)} px in ${String(dock.client)}`).toBeLessThanOrEqual(dock.client);
      await expectInside(newest, log, `${size}: the newest message after joining voice`);
      await voice.getByRole('button', { name: 'Mute' }).tap();
      await expect(voice).toContainText('Muted.');
      await voice.getByRole('button', { name: 'Unmute' }).tap();
      await voice.getByRole('button', { name: 'Voice options' }).tap();
      await expectFits(a.page, `${size}: the voice panel with its options open`);
      await voice.getByLabel('Open microphone').tap();
      await expect(voice.getByRole('meter', { name: 'Microphone level' })).toBeVisible();
      await expectFits(a.page, `${size}: the voice panel, open microphone`);
      await voice.getByLabel('Push to talk').tap();
      await voice.getByRole('button', { name: 'Leave voice' }).tap();
      await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
    } finally {
      await a.context.close();
    }
  }

  // Issue #584: Join voice and Watch only name the room they join, in full on
  // a desktop and read out in full on a phone; while another channel is on
  // screen, the channel voice is in is a button back to it, which on a phone
  // also brings it into the sideways row. Opening it joins nothing again.
  test('the strip names the room it joins, and the channel voice is in is a way back to it', async ({ browser }) => {
    // Channels of alice's after the voice world's, enough to push it off a
    // phone's row.
    const others = ['f04-book-club', 'f04-game-night', 'f04-after-hours', 'f04-film-club'];
    const done = async (what: string, call: Promise<{ status: number }>) => {
      const { status } = await call;
      expect(status >= 200 && status < 300, `${what}: status ${String(status)}`).toBe(true);
    };
    for (const name of others) {
      await done(`create ${name}`, vw.api(vw.operator, 'POST', '/v0/channels', { name }));
      await done(`add alice to ${name}`, vw.api(vw.operator, 'PUT', `/v1/channels/${name}/members/${String(vw.alice.id)}`));
    }
    const last = others[others.length - 1] ?? '';
    const sizes = [
      ['1440×900', { viewport: { width: 1440, height: 900 } }],
      phones[0],
    ] as const satisfies readonly (readonly [string, BrowserContextOptions])[];
    for (const [size, options] of sizes) {
      const phone = options.viewport.width < 700;
      const a = await person(browser, 'alice', options);
      try {
        const voice = voiceRegion(a.page);
        const press = (l: Locator) => (phone ? l.tap() : l.click());
        let sessions = 0;
        a.page.on('request', (r) => {
          if (r.method() === 'POST' && r.url().endsWith('/voice/session')) sessions++;
        });

        const join = voice.getByRole('button', { name: 'Join voice in #voice-room', exact: true });
        const watch = voice.getByRole('button', { name: 'Watch only in #voice-room', exact: true });
        await expect(join, size).toBeEnabled();
        await expect(watch, size).toHaveAccessibleDescription('Microphone off. You hear the room and can watch a share.');
        await expect(voice.getByText('Microphone off. You hear the room and can watch a share.'), size).toBeVisible();
        // The verbs are always on screen; the room's name is on a desktop,
        // and only read out on a phone.
        for (const [button, verb] of [[join, 'Join voice'], [watch, 'Watch only']] as const) {
          await expect(button, size).toBeVisible();
          await expect(button, size).toContainText(verb);
          const box = await button.getByText('in #voice-room', { exact: true }).boundingBox();
          if (phone) expect(box?.width ?? 0, `${size}: the room's name is hidden on screen`).toBeLessThanOrEqual(1);
          else expect(box?.width ?? 0, `${size}: the room's name is shown`).toBeGreaterThan(40);
        }
        await expectFits(a.page, `${size}: the strip before joining`);

        await press(join);
        await expect(voice).toContainText('Connected in #voice-room', { timeout: 30_000 });
        // On the voice channel, its name is text: there is nowhere to go.
        await expect(voice.getByRole('button', { name: '#voice-room', exact: true })).toHaveCount(0);
        expect(sessions, `${size}: one voice session for the join`).toBe(1);

        // Browsing never moves voice; the strip offers the way back.
        await press(channelButton(a.page, last));
        await expect(conversation(a.page).getByRole('heading', { name: last })).toBeVisible();
        const back = voice.getByRole('button', { name: '#voice-room', exact: true });
        await expect(back).toBeVisible();
        await expect(voice).toContainText('Connected in #voice-room');
        if (phone) {
          // The voice channel scrolled out of the row.
          await a.page.getByRole('navigation', { name: 'Channels' }).getByRole('list').evaluate((row) => {
            row.scrollLeft = row.scrollWidth;
          });
          await expect(channelButton(a.page, vw.channel), `${size}: #voice-room is off screen`).not.toBeInViewport();
        }

        await press(back);
        await expect(channelButton(a.page, vw.channel)).toHaveAttribute('aria-current', 'true');
        await expect(conversation(a.page).getByRole('heading', { name: vw.channel })).toBeVisible();
        if (phone) await expect(channelButton(a.page, vw.channel), `${size}: #voice-room is back on screen`).toBeInViewport();
        // Still the same connection: connected all along, and no new session.
        await expect(voice).toContainText('Connected in #voice-room');
        await expect(voice).not.toContainText('Reconnecting');
        await expect(back).toHaveCount(0);
        await expect(voice.getByRole('list', { name: 'In voice' })).toContainText('You');
        expect(sessions, `${size}: no new voice session`).toBe(1);
        await expectFits(a.page, `${size}: the strip after going back`);

        await press(voice.getByRole('button', { name: 'Leave voice' }));
        await expect(voice).toContainText('Not in voice', { timeout: 10_000 });
      } finally {
        await a.context.close();
      }
    }
  });

  // Issue #585 (F02): the roster of a channel not joined is live. Bob watches
  // the strip without joining; alice joining and leaving shows within 3 s,
  // with no channel opened again. Bob's page holds one presence socket all
  // along, and asks for no session, no device and no LiveKit connection.
  test('the roster of a channel not joined shows people arriving and leaving', async ({ browser }) => {
    // Each presence socket bob's page opens: its channel, and whether it is
    // still open.
    const sockets: { channel: string | null; open: boolean }[] = [];
    let sessions = 0;
    const b = await person(browser, 'bob', undefined, (page) => {
      page.on('websocket', (ws) => {
        const url = new URL(ws.url());
        if (url.pathname !== '/v1/voice/ws') return;
        const s = { channel: url.searchParams.get('channel'), open: true };
        sockets.push(s);
        ws.on('close', () => {
          s.open = false;
        });
      });
      page.on('request', (r) => {
        if (r.url().endsWith('/voice/session')) sessions++;
      });
    });
    const a = await person(browser, 'alice');
    try {
      const bob = voiceRegion(b.page);
      const alice = voiceRegion(a.page);
      const listed = bob.getByRole('list', { name: 'In voice' }).getByText('alice', { exact: true });
      // Live before anyone joins, and alice from another test is gone.
      await expect(bob.getByRole('button', { name: 'Join voice in #voice-room', exact: true })).toBeEnabled();
      await expect(listed).toHaveCount(0, { timeout: 60_000 });
      const opened = sockets.length;

      await alice.getByRole('button', { name: 'Join voice' }).click();
      await expect(alice).toContainText('Connected in #voice-room', { timeout: 30_000 });
      await expect(listed).toHaveCount(1, { timeout: 3000 });
      await expect(bob).toContainText('Not in voice');

      await alice.getByRole('button', { name: 'Leave voice' }).click();
      await expect(alice).toContainText('Not in voice', { timeout: 10_000 });
      await expect(listed).toHaveCount(0, { timeout: 3000 });

      // One presence socket, for the channel on screen, all along: none
      // opened again for alice's coming and going.
      expect(sockets.length, 'presence sockets opened while alice came and went').toBe(opened);
      expect(sockets.filter((s) => s.open).map((s) => s.channel), 'presence sockets open').toEqual([vw.channel]);
      expect(sessions, 'voice sessions bob asked for').toBe(0);
      expect(await b.page.evaluate(() => (window as unknown as { __pcs: RTCPeerConnection[] }).__pcs.length), 'peer connections on bob\'s page').toBe(0);
      await expectClean(b);
    } finally {
      await a.context.close();
      await b.context.close();
    }
  });

  // Issue #588 (F05): in voice in one channel and reading another, the
  // roster's names come from the channel voice is in. The channel alice reads
  // has her and the operator only; bob, the screen he shares, and someone who
  // joins while she reads it are still named.
  test('the roster names the people of the channel voice is in, read from another channel', async ({ browser }) => {
    const other = 'f05-alone';
    const done = async (what: string, call: Promise<{ status: number }>) => {
      const { status } = await call;
      expect(status >= 200 && status < 300, `${what}: status ${String(status)}`).toBe(true);
    };
    await done(`create ${other}`, vw.api(vw.operator, 'POST', '/v0/channels', { name: other }));
    await done(`add alice to ${other}`, vw.api(vw.operator, 'PUT', `/v1/channels/${other}/members/${String(vw.alice.id)}`));
    const dana = await vw.addMember('dana');
    const a = await person(browser, 'alice');
    const b = await person(browser, 'bob');
    const d = await browser.newContext();
    try {
      const alice = voiceRegion(a.page);
      const bob = voiceRegion(b.page);
      await alice.getByRole('button', { name: 'Join voice' }).click();
      await bob.getByRole('button', { name: 'Join voice' }).click();
      for (const v of [alice, bob]) await expect(v).toContainText('Connected in #voice-room', { timeout: 30_000 });

      await channelButton(a.page, other).click();
      await expect(conversation(a.page).getByRole('heading', { name: other })).toBeVisible();
      const roster = alice.getByRole('list', { name: 'In voice' });
      await expect(roster).toContainText('You');
      await expect(roster).toContainText('bob');
      await expect(roster).not.toContainText(/Member \d+/);

      // bob's share is announced by his name.
      await bob.getByRole('button', { name: 'Share screen' }).click();
      await expect(alice).toContainText('bob is sharing a screen', { timeout: 15_000 });
      await expect(alice).not.toContainText(/Member \d+/);
      await bob.getByRole('button', { name: 'Stop sharing' }).click();

      // Someone who joins while alice reads the other channel is named within
      // 3 s of appearing.
      const page = await d.newPage();
      await signIn(page, vw, dana.token, dana.name);
      await channelButton(page, vw.channel).click();
      await voiceRegion(page).getByRole('button', { name: 'Join voice' }).click();
      await expect(roster.getByRole('listitem')).toHaveCount(3, { timeout: 30_000 });
      await expect(roster).toContainText('dana', { timeout: 3000 });
      await expect(roster).not.toContainText(/Member \d+/);
    } finally {
      await d.close();
      await a.context.close();
      await b.context.close();
    }
  });

  // Issue #596 (F01): where people are talking is seen from any channel.
  // kim is in voice in #f01-game-night; lee reads #f01-books and max
  // #f01-chess. Each list counts the people in #f01-game-night's voice, and
  // each strip says where they are, within 3 s of kim joining and of her
  // leaving. max's Open button opens the channel and joins nothing. Three
  // people and three channels of this test's own, made through the API, so
  // no other test's voice moves these counts.
  test('people in voice in one channel are seen from the others, and opening that channel joins nothing', async ({ browser }) => {
    const must = async (what: string, call: Promise<{ status: number; text: string }>): Promise<unknown> => {
      const r = await call;
      expect(r.status >= 200 && r.status < 300, `${what}: status ${String(r.status)}`).toBe(true);
      return r.text === '' ? null : (JSON.parse(r.text) as unknown);
    };
    const room = 'f01-game-night';
    const reading = { lee: 'f01-books', max: 'f01-chess' } as const;
    for (const name of [room, reading.lee, reading.max]) await must(`create ${name}`, vw.api(vw.operator, 'POST', '/v0/channels', { name }));
    const human = async (name: string, channels: readonly string[]) => {
      const made = (await must(`create ${name}`, vw.api(vw.operator, 'POST', '/v0/principals', { kind: 'human', name }))) as { principal: { id: number } };
      const cred = (await must(`credential for ${name}`, vw.api(vw.operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, { label: name }))) as { token: string };
      for (const c of channels) await must(`add ${name} to ${c}`, vw.api(vw.operator, 'PUT', `/v1/channels/${c}/members/${String(made.principal.id)}`));
      vw.addSecret(cred.token);
      return { id: made.principal.id, name, token: cred.token };
    };
    const kim = await human('f01-kim', [room]);
    const leeMember = await human('f01-lee', [room, reading.lee]);
    const maxMember = await human('f01-max', [room, reading.max]);

    // What a reader's page asked for: each occupancy socket, each voice
    // session, and each device.
    const open = async (m: { name: string; token: string }, channel: string) => {
      const context = await browser.newContext();
      const page = await context.newPage();
      const seen = { occupancy: 0, sessions: 0 };
      page.on('websocket', (ws) => {
        if (new URL(ws.url()).pathname === '/v1/voice/occupancy/ws') seen.occupancy++;
      });
      page.on('request', (r) => {
        if (r.url().endsWith('/voice/session')) seen.sessions++;
      });
      const watch = watchPage(page, vw);
      await page.addInitScript(countBytes);
      await page.addInitScript(() => {
        const w = window as unknown as { __devices: number };
        w.__devices = 0;
        const devices = navigator.mediaDevices as MediaDevices | undefined;
        if (devices === undefined) return;
        const original = devices.getUserMedia.bind(devices);
        devices.getUserMedia = (c?: MediaStreamConstraints) => {
          w.__devices++;
          return original(c);
        };
      });
      await signIn(page, vw, m.token, m.name);
      await channelButton(page, channel).click();
      await expect(conversation(page).getByRole('heading', { name: channel })).toBeVisible();
      return { context, page, watch, member: m, seen };
    };
    const k = await open(kim, room);
    const lee = await open(leeMember, reading.lee);
    const max = await open(maxMember, reading.max);
    const mark = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('img', { name: / in voice$/ });
    // The strip's line "N in voice in #x", with its Open button.
    const elsewhere = (page: Page) => voiceRegion(page).locator('.voice-elsewhere');
    try {
      for (const r of [lee, max]) {
        await expect(mark(r.page)).toHaveCount(0);
        await expect(elsewhere(r.page)).toHaveCount(0);
      }

      await voiceRegion(k.page).getByRole('button', { name: 'Join voice' }).click();
      await expect(voiceRegion(k.page)).toContainText(`Connected in #${room}`, { timeout: 30_000 });
      for (const r of [lee, max]) {
        // The count is beside the channel's button, which keeps its name.
        await expect(mark(r.page)).toHaveAccessibleName('1 in voice', { timeout: 3000 });
        await expect(r.page.getByRole('navigation', { name: 'Channels' }).getByRole('listitem').filter({ has: r.page.getByRole('img', { name: / in voice$/ }) })).toContainText(room);
        await expect(channelButton(r.page, room)).toHaveAccessibleName(room);
        await expect(elsewhere(r.page)).toHaveText(`1 in voice in #${room} Open #${room}`, { timeout: 3000 });
      }

      // max opens it from the strip: the channel opens, and voice does not.
      await voiceRegion(max.page).getByRole('button', { name: `Open #${room}`, exact: true }).click();
      await expect(conversation(max.page).getByRole('heading', { name: room })).toBeVisible();
      await expect(voiceRegion(max.page)).toContainText('Not in voice');
      await expect(voiceRegion(max.page).getByRole('list', { name: 'In voice' })).toContainText(kim.name);
      await expect(voiceRegion(max.page).getByRole('button', { name: `Join voice in #${room}`, exact: true })).toBeEnabled();
      // Its people are the roster now, not the line.
      await expect(elsewhere(max.page)).toHaveCount(0);

      // kim leaves; lee's list and strip say so within 3 s.
      await voiceRegion(k.page).getByRole('button', { name: 'Leave voice' }).click();
      await expect(voiceRegion(k.page)).toContainText('Not in voice', { timeout: 10_000 });
      await expect(mark(lee.page)).toHaveCount(0, { timeout: 3000 });
      await expect(elsewhere(lee.page)).toHaveCount(0, { timeout: 3000 });

      // Seeing it asked for nothing of voice: no session, no device, no
      // LiveKit connection; and one occupancy socket a page all along.
      for (const r of [lee, max]) {
        expect(r.seen.sessions, `voice sessions ${r.member.name} asked for`).toBe(0);
        expect(r.seen.occupancy, `occupancy sockets ${r.member.name}'s page opened`).toBe(1);
        expect(await r.page.evaluate(() => (window as unknown as { __devices: number }).__devices), `devices ${r.member.name}'s page asked for`).toBe(0);
        expect(await r.page.evaluate(() => (window as unknown as { __pcs: RTCPeerConnection[] }).__pcs.length), `peer connections on ${r.member.name}'s page`).toBe(0);
        await expectClean(r);
      }
    } finally {
      await k.context.close();
      await lee.context.close();
      await max.context.close();
    }
  });

  // Issue #599 (F21): the other person sees a push-to-talk talker as
  // "speaking", as they do one on the open microphone. "speaking" is
  // LiveKit's own speaker detection, which needs the sound at -35 dBov or
  // louder for 40% of each 400 ms; this suite's steady tone is, and so the
  // talker shows within 3 s of the press (the track opens at most a second
  // after it), and stays while the key is held. The audit saw no "speaking"
  // because it ran on Chromium's default fake microphone, a short beep every
  // half second, loud enough for about a sixth of the time: "speaking" then
  // shows only now and then, in either mode. Two people of their own,
  // so no other test's audit trail sees their presses.
  //
  // expectHeld checks that what shows is still there at every look, four a
  // second, for ms.
  async function expectHeld(what: Locator, text: string, ms: number): Promise<void> {
    const until = Date.now() + ms;
    while (Date.now() < until) {
      expect(await what.innerText(), `"${text}" held for ${String(ms)} ms`).toContain(text);
      await new Promise((r) => setTimeout(r, 250));
    }
  }

  test('a push-to-talk talker shows as speaking to the other person, as on the open microphone', async ({ browser }) => {
    const people = [await vw.addMember('ivy'), await vw.addMember('jon')];
    const contexts = [await browser.newContext(), await browser.newContext()];
    try {
      const [talker, listener] = await Promise.all(
        people.map(async (m, i) => {
          const context = contexts[i];
          if (context === undefined) throw new Error('no context');
          const page = await context.newPage();
          await signIn(page, vw, m.token, m.name);
          await channelButton(page, vw.channel).click();
          const voice = voiceRegion(page);
          await voice.getByRole('button', { name: 'Join voice' }).click();
          await expect(voice).toContainText('Connected in #voice-room', { timeout: 30_000 });
          return { page, voice };
        }),
      );
      if (talker === undefined || listener === undefined) throw new Error('no people');
      const heard = listener.voice.getByRole('list', { name: 'In voice' }).getByRole('listitem').filter({ hasText: 'ivy' });
      await expect(heard).toBeVisible({ timeout: 15_000 });
      await expect(heard).not.toContainText('speaking');

      await talker.page.getByRole('heading', { name: 'Conch' }).click();
      await talker.page.keyboard.down('Backquote');
      await expect(heard).toContainText('speaking', { timeout: 3000 });
      // And held while the key is: on the beep it comes and goes.
      await expectHeld(heard, 'speaking', 2000);
      await talker.page.keyboard.up('Backquote');
      await expect(heard).not.toContainText('speaking', { timeout: 5000 });

      await talker.voice.getByRole('button', { name: 'Voice options' }).click();
      await talker.voice.getByLabel('Open microphone').check();
      await expect(heard).toContainText('speaking', { timeout: 10_000 });
      await expectHeld(heard, 'speaking', 2000);
      await talker.voice.getByLabel('Push to talk').check();
      await expect(heard).not.toContainText('speaking', { timeout: 5000 });
      for (const p of [talker, listener]) {
        await p.voice.getByRole('button', { name: 'Leave voice' }).click();
        await expect(p.voice).toContainText('Not in voice', { timeout: 10_000 });
      }
    } finally {
      for (const c of contexts) await c.close();
    }
  });

  // Issue #591 (F24): one person's credential open on two pages, one of them
  // in voice. The other names them "You (on another device)" and says that
  // joining there moves their voice; the one in voice names them "You", and
  // never says "another device" of its own place, while it is in voice or
  // after it leaves. A member of their own, made through the API: the
  // others are in other tests' rooms.
  test('a second page of someone in voice says they are in voice on another device', async ({ browser }) => {
    const cy = await vw.addMember('cy');
    const contexts = [await browser.newContext(), await browser.newContext()];
    try {
      const pages: Page[] = [];
      for (const context of contexts) {
        const page = await context.newPage();
        await signIn(page, vw, cy.token, cy.name);
        await channelButton(page, vw.channel).click();
        pages.push(page);
      }
      const [first, second] = pages;
      if (first === undefined || second === undefined) throw new Error('two pages');
      // Whether the first page ever says "another device", from here on.
      await first.evaluate(() => {
        const w = window as unknown as { __elsewhere: boolean };
        w.__elsewhere = false;
        new MutationObserver(() => {
          if (document.querySelector('[aria-label="Voice"]')?.textContent.includes('another device') === true) w.__elsewhere = true;
        }).observe(document.body, { subtree: true, childList: true, characterData: true });
      });
      const here = voiceRegion(first);
      const there = voiceRegion(second);
      const said = 'You are in voice on another device. Joining here moves your voice to this device.';

      await here.getByRole('button', { name: 'Join voice' }).click();
      await expect(here).toContainText('Connected in #voice-room', { timeout: 30_000 });
      await expect(here.getByRole('list', { name: 'In voice' }).getByText('You', { exact: true })).toBeVisible();
      await expect(there.getByRole('list', { name: 'In voice' }).getByText('You (on another device)', { exact: true })).toBeVisible();
      await expect(there).toContainText(said);
      await expect(there).toContainText('Not in voice');
      // Nothing stops the join there.
      await expect(there.getByRole('button', { name: 'Join voice in #voice-room', exact: true })).toBeEnabled();

      await here.getByRole('button', { name: 'Leave voice' }).click();
      await expect(here).toContainText('Not in voice', { timeout: 10_000 });
      await expect(there.getByRole('list', { name: 'In voice' }).getByText(/^You/)).toHaveCount(0);
      await expect(there).not.toContainText(said);
      await expect(here.getByRole('list', { name: 'In voice' }).getByText(/^You/)).toHaveCount(0);
      expect(await first.evaluate(() => (window as unknown as { __elsewhere: boolean }).__elsewhere), 'the page in voice said "another device" of itself').toBe(false);
    } finally {
      for (const context of contexts) await context.close();
    }
  });

  test('removing the member disconnects the browser and the page says why', async ({ browser }) => {
    const b = await person(browser, 'bob');
    try {
      const voice = voiceRegion(b.page);
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Connected', { timeout: 30_000 });
      await vw.removeFromChannel(vw.bob);
      await expect(voice).toContainText('Voice stopped', { timeout: 60_000 });
      await expect(voice).toContainText('not a member');
      await expectClean(b);
    } finally {
      await b.context.close();
    }
  });

  test('restarting LiveKit brings the page back by itself', async ({ browser }) => {
    const a = await person(browser, 'alice');
    try {
      const voice = voiceRegion(a.page);
      await voice.getByRole('button', { name: 'Join voice' }).click();
      await expect(voice).toContainText('Connected', { timeout: 30_000 });
      vw.stopLiveKit();
      await expect(voice).not.toContainText('Connected in', { timeout: 60_000 });
      await vw.startLiveKit();
      await expect(voice).toContainText('Connected in', { timeout: 120_000 });
      await expectClean(a, false);
    } finally {
      await a.context.close();
    }
  });
});

// Two frames, so that a change of size has been laid out and drawn.
async function frames(page: Page): Promise<void> {
  await page.evaluate(
    () =>
      new Promise<void>((done) => {
        requestAnimationFrame(() => {
          requestAnimationFrame(() => {
            done();
          });
        });
      }),
  );
}

// expectInside checks that element is wholly inside the log's box: in view.
async function expectInside(element: Locator, log: Locator, what: string): Promise<void> {
  await frames(element.page());
  const box = await element.boundingBox();
  const frame = await log.boundingBox();
  if (box === null || frame === null) throw new Error(`${what} has no box on the screen`);
  const where = `${what} is at y ${String(box.y)}, ${String(box.height)} high, in a log from y ${String(frame.y)}, ${String(frame.height)} high`;
  expect(box.y, where).toBeGreaterThanOrEqual(frame.y - 1);
  expect(box.y + box.height, where).toBeLessThanOrEqual(frame.y + frame.height + 1);
}

// notesHeight is how high the strip's notes are, and one line of their text
// with the notes' own padding (issue #600).
async function notesHeight(voice: Locator): Promise<{ readonly height: number; readonly share: number; readonly line: number }> {
  return voice.evaluate((el) => {
    const notes = el.querySelector('.voice-notes');
    const hint = notes?.querySelector('.hint');
    // The line about sharing: the hint that holds More about sharing.
    const share = notes?.querySelector('button.more')?.closest('.hint');
    if (!(notes instanceof HTMLElement) || !(hint instanceof HTMLElement) || !(share instanceof HTMLElement)) throw new Error('no notes in the strip');
    const own = getComputedStyle(notes);
    const text = getComputedStyle(hint);
    const line = text.lineHeight.endsWith('px') ? parseFloat(text.lineHeight) : parseFloat(text.fontSize) * parseFloat(text.lineHeight);
    return {
      height: notes.getBoundingClientRect().height - parseFloat(own.paddingTop) - parseFloat(own.paddingBottom),
      share: share.getBoundingClientRect().height,
      line,
    };
  });
}
