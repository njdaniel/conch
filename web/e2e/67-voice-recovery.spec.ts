// Voice recovery in the browser (issue #1612): what happens to a call when one
// person's signal drops, when LiveKit freezes briefly, when a page vanishes
// without leaving, and when a person publishes again. Alice, bob and carol are
// separate principals in contexts of their own (people.ts), in one voice room
// against the voice world's pinned LiveKit, each on Chromium's fake microphone.
// Without Docker it fails when CI is set and is skipped otherwise, as in
// 10-voice.
//
// The faults, and the limit of each (the PR says the same):
//   - the signal: page.routeWebSocket(/\/rtc/) wraps one page's LiveKit
//     signal socket and closes it on command, holding new ones unanswered (and
//     never connected through to LiveKit) for a while.
//     This cuts signal only. WebRTC media is UDP and does not pass through
//     Playwright.
//   - the page: context.setOffline(true) cuts that page's HTTP and WebSocket
//     traffic, conchd's sockets included. In Chromium it does not cut WebRTC
//     UDP media.
//   - the server: vw.pauseLiveKit() (docker pause) freezes LiveKit for
//     everyone: a media and signal blackhole, not a closed socket.
//   - a vanished page: its context is closed without closing the page, so no
//     pagehide ends its voice session (fixtures.ts, issue #802); as a crashed
//     tab.
//   No fault cuts one person's media alone: LiveKit here runs on the host
//   network on one machine, and there is no per-participant UDP cut.
//
// What this proves: the recovery logic on one machine, against faults injected
// at the signal, the HTTP/WS and the whole-server layer. What it does not:
// real network loss, NAT rebinding, Wi-Fi roaming, a mobile background kill, a
// TURN path. Those are by hand, on a real network.
//
// Bounds, each derived, none hard-coded without a source:
//   reconnectLimit  20 s   web/src/voice/connection.ts: how long the SDK's own
//                          reconnect may last before the client asks conchd for
//                          a new session.
//   rosterBound     11 s   10-voice.spec.ts: the longest a roster takes to show
//                          a change conchd's presence poller sees (2 x (2 s
//                          call + 1.5 s gap + 2 s call)).
//   margin          10 s   stated, for a loaded runner: the browser's own work
//                          (an ICE restart, a render) queues behind load.
//   emptiesWithin   20 s   voice-world.ts: livekit.yaml's empty_timeout (5 s) +
//                          departure_timeout (5 s) + 10 s, for the last
//                          connection gone.
//   goneWithin             LiveKit's own window for dropping a participant
//                          whose connection died is not in livekit.yaml (it is
//                          the server's ICE and signal timeouts, which this
//                          config leaves at their defaults). Measured: see
//                          vanishedWithin below.
import type { Browser, Locator, Page, WebSocketRoute } from '@playwright/test';
import { expect, messages, say, test } from './fixtures';
import { adminToken } from './livekit-admin';
import { enter, expectClean, inbound, joinVoice, leave, type Someone } from './people';
import { dockerAvailable, emptiesWithin, startVoiceWorld, type Member, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

const reconnectLimit = 20_000;
const rosterBound = 2 * (2000 + 1500 + 2000);
const margin = 10_000;
// Signal closed, new sockets held unanswered, for this long before they are
// let through again: long enough for the SDK to try and fail at least once,
// well under reconnectLimit so the SDK resumes. The client does not show
// Reconnecting for it (a signal-only resume is SignalReconnecting in
// livekit-client, which the client does not map; #1678).
const signalCutFor = 3_000;
// The page's HTTP and WS traffic is off for this long (issue #1612).
const offlineFor = 5_000;
// LiveKit frozen for this long: under reconnectLimit (20 s) and under the
// server's own ping timeout (15 s by default), so the SDK resumes.
const pausedFor = 8_000;
const recoverWithin = signalCutFor + reconnectLimit + margin;
// How long LiveKit may take to stop listing a participant whose connection
// died without a leave (a vanished page). It is not in livekit.yaml: it is the
// server's own ICE/connection timeout. Measured in this spec (issue #1612) over
// 10 runs on a Linux x86-64 desktop, LiveKit v1.13.7: 19.9 to 21.9 s from the
// context closing to the identity leaving the list. Bound: the longest seen,
// 22 s, plus the 10 s margin.
const vanishedWithin = 22_000 + margin;
// How long LiveKit may take to drop someone whose Leave reached it. The page
// sends Leave and LiveKit removes the participant at once, so 5 s is generous
// (a delivered leave measured 4 to 7 ms over 5 runs); a leave that did not arrive is
// removed only by the dead-connection window above (about 20 s), so 5 s
// tells the two apart.
const leaveWithin = 5_000;
// Requests of a page that lost them while its context was offline.
// by method, path and error, exactly as watchPage records them: conchd's
// approvals poll is the one request carol's page makes while offline.
const offlineFailure = /^GET \/v1\/approvals net::ERR_INTERNET_DISCONNECTED$/;

const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });
const roster = (page: Page) => voiceRegion(page).getByRole('list', { name: 'In voice' });

// The signal socket of one page: passed through to LiveKit until cut().
class Signal {
  private holding = false;
  private readonly live: { page: WebSocketRoute; server: WebSocketRoute }[] = [];
  // Signal sockets passed through to LiveKit, and attempts held while cut.
  opened = 0;
  held = 0;

  async install(page: Page): Promise<void> {
    await page.routeWebSocket(/\/rtc/, (ws) => {
      if (this.holding) {
        // Held: never connected to LiveKit, never answered. LiveKit sees
        // nothing of the attempt (a refusal that connected through and closed
        // would show LiveKit a second connection under bob's identity), and
        // the page's socket is left to the SDK's own timeout.
        this.held++;
        return;
      }
      this.opened++;
      const server = ws.connectToServer();
      this.live.push({ page: ws, server });
      ws.onMessage((m) => {
        server.send(m);
      });
      server.onMessage((m) => {
        ws.send(m);
      });
      ws.onClose((code, reason) => {
        void server.close({ code, reason });
      });
      server.onClose((code, reason) => {
        void ws.close({ code, reason });
      });
    });
  }

  // cut closes every live signal socket and holds new ones until restore().
  async cut(): Promise<void> {
    this.holding = true;
    for (const { page, server } of this.live.splice(0)) {
      await page.close({ code: 1011, reason: 'cut' });
      await server.close({ code: 1000 });
    }
  }

  restore(): void {
    this.holding = false;
  }
}

// Counts the moments a page did not say what it should, polled every 200 ms.
class Lapses {
  readonly seen: string[] = [];
  // Reads that timed out or failed: recorded, never counted as a lapse. A slow
  // read on a loaded machine says nothing about the page.
  unreadable = 0;
  private running = true;
  private readonly loop: Promise<void>;
  constructor(
    private readonly name: string,
    private readonly probe: () => Promise<string | null>,
  ) {
    this.loop = this.run();
  }
  private async run(): Promise<void> {
    while (this.running) {
      const at = Date.now();
      try {
        const problem = await this.probe();
        if (problem !== null) this.seen.push(`${this.name}: ${problem}`);
      } catch {
        this.unreadable++;
      }
      await new Promise((r) => setTimeout(r, Math.max(0, 200 - (Date.now() - at))));
    }
  }
  async stop(): Promise<string[]> {
    this.running = false;
    await this.loop;
    return [...new Set(this.seen)];
  }
}

const text = async (l: Locator): Promise<string> => (await l.textContent({ timeout: 1500 })) ?? '';

// connectedAndSeeing lapses when the page stops saying it is connected, or its
// roster stops naming the people.
const watching = (p: Someone, names: readonly string[]): Lapses =>
  new Lapses(p.member.name, async () => {
    if (!/Connected in #/.test(await text(voiceRegion(p.page)))) return 'not Connected';
    const shown = await text(roster(p.page));
    for (const n of names) if (!shown.includes(n)) return `roster lacks ${n}`;
    return null;
  });

const audioBytes = async (p: Someone): Promise<number> => (await inbound(p.page)).audioBytes;
async function delta(p: Someone, ms: number): Promise<number> {
  const before = await audioBytes(p);
  await new Promise((r) => setTimeout(r, ms));
  return (await audioBytes(p)) - before;
}

// press has talker hold the talk key for 2 s and says how many audio bytes each
// listener received in that time and in the 1.5 s of quiet before it, as 10-voice
// does: a gated track sends nothing, a held key sends the tone.
async function press(talker: Someone, listeners: readonly Someone[]): Promise<void> {
  await talker.page.getByRole('heading', { name: 'Conch' }).click();
  const quiet = await Promise.all(listeners.map((l) => delta(l, 1500)));
  await talker.page.keyboard.down('Backquote');
  try {
    for (const l of listeners) await expect(roster(l.page)).toContainText(`${talker.member.name} (transmitting)`, { timeout: 15_000 });
    const loud = await Promise.all(listeners.map((l) => delta(l, 2000)));
    listeners.forEach((l, i) => {
      expect(loud[i] ?? 0, `${l.member.name} hears ${talker.member.name}: ${String(loud[i])} B held versus ${String(quiet[i])} B quiet`).toBeGreaterThan((quiet[i] ?? 0) * 4 + 2000);
    });
  } finally {
    await talker.page.keyboard.up('Backquote');
  }
  for (const l of listeners) await expect(roster(l.page)).not.toContainText(`${talker.member.name} (transmitting)`, { timeout: 15_000 });
}

test.describe('voice recovery', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  const identity = (m: Member) => `p${String(m.id)}`;
  const identities = (...ms: Member[]) => ms.map(identity).sort();
  const names = (ps: readonly Someone[]) => ps.map((p) => p.member.name);

  // LiveKit's list of one member's published tracks (microphone only), asked
  // of LiveKit directly with the world's key and secret (as livekit-admin.ts
  // does for identities; the secret is the one voice-world.ts adds first
  // after the three credentials and the operator's).
  async function microphones(member: Member): Promise<string[]> {
    const secret = vw.secrets.find((s) => s.startsWith('web-voice-NOT-FOR-PRODUCTION-'));
    if (secret === undefined) throw new Error('no LiveKit secret in the world');
    const base = `http://127.0.0.1:${new URL(vw.livekitOrigin.replace(/^ws/, 'http')).port}`;
    const call = async (method: string, grant: object, body: object): Promise<unknown> => {
      // Fixed messages only, as livekit-admin.ts: a transport error names the
      // address, a parse error quotes the body, and a body can name a room.
      let res: Response;
      try {
        res = await fetch(`${base}/twirp/livekit.RoomService/${method}`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${adminToken('webvoicekey', secret, grant)}` },
          body: JSON.stringify(body),
          signal: AbortSignal.timeout(5000),
        });
      } catch {
        throw new Error(`livekit ${method}: no answer`);
      }
      if (res.status !== 200) throw new Error(`livekit ${method}: HTTP ${String(res.status)}`);
      try {
        return (await res.json()) as unknown;
      } catch {
        throw new Error(`livekit ${method}: unreadable answer`);
      }
    };
    const rooms = (await call('ListRooms', { roomList: true }, {})) as { rooms?: { name?: string }[] };
    const out: string[] = [];
    for (const room of rooms.rooms ?? []) {
      if (typeof room.name !== 'string') continue;
      const found = (await call('ListParticipants', { roomAdmin: true, room: room.name }, { room: room.name })) as {
        participants?: { identity?: string; tracks?: { sid?: string; source?: string }[] }[];
      };
      for (const p of found.participants ?? []) {
        if (p.identity !== identity(member)) continue;
        for (const t of p.tracks ?? []) if (typeof t.sid === 'string' && t.source === 'MICROPHONE') out.push(t.sid);
      }
    }
    return out.sort();
  }

  async function unreported(member: Member): Promise<number> {
    return (await vw.audit()).filter((e) => e.actor === `principal:${String(member.id)}` && e.action === 'voice_transmit_unreported').length;
  }

  // The three of the voice world, in push to talk, in voice, each seeing the
  // other two in its roster. before(name, page) runs on each new page.
  async function room(browser: Browser, before?: Partial<Record<'alice' | 'bob' | 'carol', (page: Page) => Promise<void>>>): Promise<[Someone, Someone, Someone]> {
    const people: Someone[] = [];
    try {
      for (const m of [vw.alice, vw.bob, vw.carol]) {
        const hook = before?.[m.name as 'alice' | 'bob' | 'carol'];
        people.push(await enter(browser, vw, m, hook === undefined ? {} : { before: hook }));
      }
      for (const p of people) await joinVoice(p, 'Join voice');
      for (const p of people) for (const n of names(people)) await expect(roster(p.page)).toContainText(n, { timeout: rosterBound });
      await expect.poll(() => vw.participants(), { timeout: 20_000 }).toEqual(identities(vw.alice, vw.bob, vw.carol));
    } catch (err) {
      for (const p of people) await leave(p);
      throw err;
    }
    return people as [Someone, Someone, Someone];
  }

  // Back as found: nobody connected, within empty_timeout and departure_timeout
  // and the margin.
  async function empty(): Promise<void> {
    await expect.poll(() => vw.participants(), { timeout: emptiesWithin }).toEqual([]);
  }

  test("bob's signal drops while alice and carol stay: bob resumes by himself, with no duplicate", async ({ browser }, info) => {
    test.setTimeout(150_000);
    const signal = new Signal();
    const [a, b, c] = await room(browser, { bob: (page) => signal.install(page) });
    const bobSees = watching(b, ['alice', 'carol']);
    let others: Lapses[] = [];
    let bobInRosters: Lapses[] = [];
    try {
      const micBefore = await microphones(vw.bob);
      expect(micBefore, "one microphone publication of bob's").toHaveLength(1);
      const unreportedBefore = await unreported(vw.bob);
      // Alice and carol stay Connected and keep seeing each other. Whether
      // they keep bob in their rosters while his signal is down is conchd's
      // presence poller seeing LiveKit's list: it is observed here and
      // recorded, and asserted only to be right again once bob is back.
      others = [watching(a, ['carol']), watching(c, ['alice'])];
      bobInRosters = [watching(a, ['bob']), watching(c, ['bob'])];

      const cutAt = Date.now();
      const openedBefore = signal.opened;
      await signal.cut();
      await new Promise((r) => setTimeout(r, signalCutFor));
      signal.restore();
      // The SDK resumes: once the cut is lifted it opens at least one new
      // signal socket beyond those open before the cut. (The Connected check
      // below cannot tell a resume from nothing happening, since the page
      // does not show a signal-only drop, #1678; this and the unchanged
      // publication are what prove it.)
      await expect.poll(() => signal.opened, { timeout: recoverWithin, message: 'bob opens a new signal socket after the cut' }).toBeGreaterThan(openedBefore);
      await expect(voiceRegion(b.page)).toContainText(/Connected in #voice-room/, { timeout: recoverWithin });
      info.annotations.push({ type: 'signal back after the cut', description: `${String(Date.now() - cutAt)} ms (bound ${String(recoverWithin)} ms)` });
      info.annotations.push({ type: 'signal sockets held during the cut', description: String(signal.held) });
      const bobLapses = await bobSees.stop();
      // livekit-client 2.22 reports a signal-only resume as SignalReconnecting,
      // which the client does not map to its Reconnecting phase (that is for a
      // full restart): bob's page is expected to stay Connected, and what it
      // did is recorded, not hidden.
      info.annotations.push({ type: 'bob page during the cut', description: bobLapses.length === 0 ? 'stayed Connected' : bobLapses.join('; ') });

      const lapses = (await Promise.all(others.map((l) => l.stop()))).flat();
      expect(lapses, 'alice and carol stay connected and keep seeing each other').toEqual([]);
      info.annotations.push({ type: 'unreadable reads (not counted)', description: String(others.reduce((n, l) => n + l.unreadable, 0)) });
      const bobDrops = (await Promise.all(bobInRosters.map((l) => l.stop()))).flat();
      info.annotations.push({ type: "bob in alice's and carol's rosters during the cut", description: bobDrops.length === 0 ? 'kept throughout' : `dropped and came back (${[...new Set(bobDrops.map((d) => d.split(':')[0]))].join(', ')})` });
      for (const p of [a, c]) await expect(roster(p.page)).toContainText('bob', { timeout: rosterBound + margin });
      // Chromium's own warning for a socket closed before it opened names the
      // URL, access_token included; it is the browser's, and recorded here.
      info.annotations.push({ type: 'console lines naming an access_token', description: String(b.watch.console.filter((l) => l.includes('access_token=')).length) });
      // The same publication: the SDK resumed, it did not start over.
      expect(await microphones(vw.bob), 'bob keeps his microphone publication across a signal-only resume').toEqual(micBefore);

      await press(b, [a, c]);
      expect(await vw.participants(), 'one participant for each person, none for bob twice').toEqual(identities(vw.alice, vw.bob, vw.carol));
      expect(await unreported(vw.bob), 'no voice_transmit_unreported for bob').toBe(unreportedBefore);
      for (const p of [a, b, c]) await expectClean(vw, p);
      // Bob leaves by his button while his page is alive, so that the leave
      // reaches LiveKit through the wrapper; closing the page at once could
      // lose it and leave LiveKit waiting out a dead connection (vanishedWithin).
      await showLeave(b);
      const leftAt = Date.now();
      await expect.poll(() => vw.participants(), { timeout: leaveWithin, message: 'bob leaves LiveKit at once' }).toEqual(identities(vw.alice, vw.carol));
      info.annotations.push({ type: 'LiveKit dropped bob after his Leave', description: `${String(Date.now() - leftAt)} ms (bound ${String(leaveWithin)} ms)` });
    } finally {
      await bobSees.stop();
      await Promise.all([...others, ...bobInRosters].map((l) => l.stop()));
      for (const p of [a, b, c]) await leave(p);
    }
    await empty();
  });

  test("carol's page goes offline for at least 5 s: chat and the roster recover, once", async ({ browser }, info) => {
    test.setTimeout(150_000);
    const [a, b, c] = await room(browser);
    const carolSees = watching(c, ['alice']);
    try {
      const body = `1612 while carol was offline ${Date.now().toString(36)}`;
      const quiet = await delta(c, 1500);
      await c.context.setOffline(true);
      const offlineAt = Date.now();
      let offlineLasted = 0;
      let loud = 0;
      try {
        // Meanwhile: alice says something, bob leaves voice, and alice holds
        // the talk key so that carol's media shows whether it carries on.
        await say(a.page, vw.channel, body);
        await showLeave(b);
        await a.page.getByRole('heading', { name: 'Conch' }).click();
        await a.page.keyboard.down('Backquote');
        try {
          await expect(roster(a.page)).toContainText('alice (transmitting)', { timeout: 15_000 });
          loud = await delta(c, offlineFor - 1500 > 2000 ? 2000 : offlineFor - 1500);
        } finally {
          await a.page.keyboard.up('Backquote');
        }
        await new Promise((r) => setTimeout(r, Math.max(0, offlineFor - 2000)));
      } finally {
        offlineLasted = Date.now() - offlineAt;
        await c.context.setOffline(false);
      }
      // Not exactly offlineFor: the window lasts as long as the say, the leave,
      // the press and the waits above take. The allowlist below holds for
      // whatever it was, and the real length is recorded.
      info.annotations.push({ type: 'carol offline for', description: `${String(offlineLasted)} ms (at least ${String(offlineFor)} ms)` });
      info.annotations.push({ type: 'media while offline', description: `${String(loud)} B in 2 s while alice held the key, ${String(quiet)} B in 1.5 s quiet: ${loud > quiet * 4 + 2000 ? 'carried on' : 'cut'}` });

      // Chat recovers: the message posted meanwhile is shown, once.
      await expect(messages(c.page).getByText(body)).toHaveCount(1, { timeout: recoverWithin });
      // The roster is current: bob left while carol could not hear of it.
      await expect(roster(c.page)).not.toContainText('bob', { timeout: recoverWithin });
      await expect(roster(c.page)).toContainText('alice');
      await expect(voiceRegion(c.page)).toContainText(/Connected in #voice-room/, { timeout: recoverWithin });
      info.annotations.push({ type: 'carol page while offline', description: (await carolSees.stop()).join('; ') || 'stayed Connected and saw alice' });
      await expect(messages(c.page).getByText(body)).toHaveCount(1);
      await expect(messages(a.page).getByText(body)).toHaveCount(1);
      expect(await vw.participants(), 'alice and carol are still the only ones connected').toEqual(identities(vw.alice, vw.carol));
      await press(a, [c]);

      for (const p of [a, b]) await expectClean(vw, p);
      // The only requests carol's page may have lost are those made while it
      // was offline; they are listed in the report and allowed by path.
      info.annotations.push({ type: 'carol failed requests while offline', description: c.watch.failed.join('; ') || 'none' });
      await expectClean(vw, { ...c, watch: { ...c.watch, failed: c.watch.failed.filter((f) => !offlineFailure.test(f)) } });
    } finally {
      await carolSees.stop();
      for (const p of [a, b, c]) await leave(p);
    }
    await empty();
  });

  test('@heavy LiveKit is frozen for 8 s: all three come back, audio flows, nobody is listed twice', { tag: '@heavy' }, async ({ browser }, info) => {
    test.setTimeout(180_000);
    const [a, b, c] = await room(browser);
    const sees = [watching(a, ['bob', 'carol']), watching(b, ['alice', 'carol']), watching(c, ['alice', 'bob'])];
    let frozen = false;
    try {
      const micsBefore = await Promise.all([vw.alice, vw.bob, vw.carol].map(microphones));
      vw.pauseLiveKit();
      frozen = true;
      const pausedAt = Date.now();
      try {
        await new Promise((r) => setTimeout(r, pausedFor));
      } finally {
        // If this throws, frozen stays true and the outer finally tries again
        // before it cleans up.
        vw.unpauseLiveKit();
        frozen = false;
      }
      for (const p of [a, b, c]) await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/, { timeout: reconnectLimit + margin });
      info.annotations.push({ type: 'all connected again after unpause', description: `${String(Date.now() - pausedAt - pausedFor)} ms (bound ${String(reconnectLimit + margin)} ms)` });
      const lapses = (await Promise.all(sees.map((l) => l.stop()))).flat();
      info.annotations.push({ type: 'pages during the freeze', description: `${lapses.join('; ') || 'stayed Connected and kept everyone'}; unreadable reads not counted: ${String(sees.reduce((n, l) => n + l.unreadable, 0))}` });
      for (const p of [a, b, c]) for (const n of names([a, b, c])) await expect(roster(p.page)).toContainText(n, { timeout: rosterBound + margin });
      // Resumed or restarted, each is listed once. A restart makes a new
      // connection and LiveKit drops the old one, so poll for the old to go.
      await expect.poll(() => vw.participants(), { timeout: emptiesWithin }).toEqual(identities(vw.alice, vw.bob, vw.carol));
      const micsAfter = await Promise.all([vw.alice, vw.bob, vw.carol].map(microphones));
      info.annotations.push({ type: 'publications after the freeze', description: micsAfter.every((m, i) => m.join() === (micsBefore[i] ?? []).join()) ? 'kept (SDK resume)' : 'new (restart through conchd)' });
      for (const m of micsAfter) expect(m, 'one microphone publication each').toHaveLength(1);

      await press(a, [b, c]);
      expect(await vw.participants()).toEqual(identities(vw.alice, vw.bob, vw.carol));
      for (const p of [a, b, c]) await expectClean(vw, p);
    } finally {
      try {
        if (frozen) vw.unpauseLiveKit();
      } finally {
        await Promise.all(sees.map((l) => l.stop()));
        for (const p of [a, b, c]) await leave(p);
      }
    }
    await empty();
  });

  test("carol's page vanishes without leaving: LiveKit, the rosters and conchd drop her", async ({ browser }, info) => {
    test.setTimeout(150_000);
    const [a, b, c] = await room(browser);
    try {
      const voicePeople = async (): Promise<number[]> => {
        const r = await vw.api(vw.alice.token, 'GET', `/v1/channels/${vw.channel}/voice`);
        const doc = JSON.parse(r.text) as { rooms: { audience?: unknown; participants: { principal_id: number }[] }[] };
        return doc.rooms.filter((x) => x.audience === undefined).flatMap((x) => x.participants.map((q) => q.principal_id)).sort((x, y) => x - y);
      };
      expect(await voicePeople()).toContain(vw.carol.id);

      // As a crashed tab: the context goes, the page is not closed first, so
      // no pagehide ends carol's voice session.
      const goneAt = Date.now();
      await c.context.close();
      await expect.poll(() => vw.participants(), { timeout: vanishedWithin, message: 'LiveKit drops carol' }).toEqual(identities(vw.alice, vw.bob));
      const livekitAfter = Date.now() - goneAt;
      info.annotations.push({ type: 'LiveKit stopped listing carol after', description: `${String(livekitAfter)} ms (bound ${String(vanishedWithin)} ms)` });

      const rostersAt = Date.now();
      for (const p of [a, b]) await expect(roster(p.page)).not.toContainText('carol', { timeout: rosterBound + margin });
      info.annotations.push({ type: 'alice and bob rosters dropped carol after LiveKit did', description: `${String(Date.now() - rostersAt)} ms (bound ${String(rosterBound + margin)} ms)` });
      await expect.poll(voicePeople, { timeout: rosterBound + margin, message: "conchd's voice presence drops carol" }).toEqual([vw.alice.id, vw.bob.id].sort((x, y) => x - y));
      for (const p of [a, b]) {
        await expect(voiceRegion(p.page)).toContainText(/Connected in #voice-room/);
        await expectClean(vw, p);
      }
    } finally {
      for (const p of [a, b, c]) await leave(p);
    }
    await empty();
  });

  test('bob leaves and joins again: his listeners hear him, and LiveKit lists one new microphone publication', async ({ browser }) => {
    test.setTimeout(150_000);
    const [a, b, c] = await room(browser);
    try {
      const before = await microphones(vw.bob);
      expect(before, "one microphone publication of bob's before").toHaveLength(1);
      await showLeave(b);
      await expect.poll(() => vw.participants(), { timeout: leaveWithin, message: 'bob leaves LiveKit at once' }).toEqual(identities(vw.alice, vw.carol));
      for (const p of [a, c]) await expect(roster(p.page)).not.toContainText('bob', { timeout: rosterBound + margin });

      await joinVoice(b, 'Join voice');
      await expect.poll(() => vw.participants(), { timeout: 20_000 }).toEqual(identities(vw.alice, vw.bob, vw.carol));
      await expect.poll(async () => (await microphones(vw.bob)).length, { timeout: 20_000, message: 'one microphone publication again' }).toBe(1);
      const after = await microphones(vw.bob);
      expect(after[0], 'a new track, not the old one').not.toBe(before[0]);
      expect(after.filter((sid) => before.includes(sid)), 'the old publication is not listed').toEqual([]);
      for (const p of [a, c]) await expect(roster(p.page)).toContainText('bob', { timeout: rosterBound + margin });

      await press(b, [a, c]);
      for (const p of [a, b, c]) await expectClean(vw, p);
    } finally {
      for (const p of [a, b, c]) await leave(p);
    }
    await empty();
  });

  // Leave voice by the page's own button (the person leaves; the page stays).
  async function showLeave(p: Someone): Promise<void> {
    await voiceRegion(p.page).getByRole('button', { name: 'Leave voice' }).click();
    await expect(voiceRegion(p.page).getByRole('button', { name: 'Join voice' })).toBeVisible();
  }
});
