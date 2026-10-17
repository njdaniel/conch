// A14 (issue #447): two groups on one server, and the page of a member of one
// shows nothing of the other, and is sent nothing of it on any socket or in
// any answer: not its channels, its people, its messages or who is in its
// voice room. Checked after a full load and after the page's sockets drop and
// it reconnects by itself. The server-side matrix is e2e/isolation; this is
// what a person sees.
//
// Each test makes its own two groups through the operator's API, with names
// no other spec uses, so the people it adds touch no other spec. The voice
// test runs against a conchd with LiveKit of its own (voice-world.ts):
// without Docker it fails when CI is set and is skipped otherwise.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { channelButton, expect, expectFits, messages, phones, signIn, statusLine, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { call, type World } from './world';

const desktop: BrowserContextOptions = { viewport: { width: 1280, height: 800 } };
const sizes = [['desktop', desktop], ...phones] as const;

const channelButtons = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('button');
const voiceRegion = (page: Page) => page.getByRole('region', { name: 'Voice' });

type Api = (token: string, method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown) => Promise<{ status: number; body: unknown }>;

// must makes a call that has to succeed and returns its body.
async function must(api: Api, what: string, token: string, method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<unknown> {
  const reply = await api(token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${what}: status ${String(reply.status)}`);
  return reply.body;
}

interface Person {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

// person makes a human with a credential, a member of channel, as an operator
// does.
async function person(api: Api, operator: string, name: string, channel: string): Promise<Person> {
  const made = (await must(api, `create ${name}`, operator, 'POST', '/v0/principals', { kind: 'human', name })) as { principal: { id: number } };
  const cred = (await must(api, `credential for ${name}`, operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, { label: name })) as { token: string };
  await must(api, `add ${name} to ${channel}`, operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(made.principal.id)}`);
  return { id: made.principal.id, name, token: cred.token };
}

function sharedApi(world: World): Api {
  return (token, method, path, body) => call(world, token, method, path, body);
}

function voiceApi(vw: VoiceWorld): Api {
  return async (token, method, path, body) => {
    const r = await vw.api(token, method, path, body);
    return { status: r.status, body: r.text === '' ? null : (JSON.parse(r.text) as unknown) };
  };
}

// trackSockets keeps every WebSocket the page opens, so that a test can drop
// the page's own (not LiveKit's) and watch it reconnect. Installed before the
// page's own scripts run.
const trackSockets = () => {
  const w = window as unknown as { __a14Sockets: WebSocket[] };
  w.__a14Sockets = [];
  const Original = window.WebSocket;
  const Wrapped = function (this: unknown, url: string | URL, protocols?: string | string[]) {
    const socket = new Original(url, protocols);
    w.__a14Sockets.push(socket);
    return socket;
  } as unknown as typeof WebSocket;
  Wrapped.prototype = Original.prototype;
  Object.setPrototypeOf(Wrapped, Original);
  window.WebSocket = Wrapped;
};

// ownSockets counts the page's sockets to conchd itself, open or not.
function ownSockets(page: Page): Promise<number> {
  return page.evaluate(() => (window as unknown as { __a14Sockets: WebSocket[] }).__a14Sockets.filter((s) => new URL(s.url).host === location.host).length);
}

// dropSockets closes every open socket the page has to conchd, as a network
// drop would, and waits until the page has opened as many again and says it
// is connected.
async function dropSockets(page: Page): Promise<void> {
  const before = await ownSockets(page);
  const dropped = await page.evaluate(() => {
    const open = (window as unknown as { __a14Sockets: WebSocket[] }).__a14Sockets.filter((s) => new URL(s.url).host === location.host && s.readyState === WebSocket.OPEN);
    for (const s of open) s.close();
    return open.length;
  });
  expect(dropped, 'the page had sockets to drop').toBeGreaterThan(0);
  await expect.poll(() => ownSockets(page), { message: 'the page opens its sockets again', timeout: 30_000 }).toBeGreaterThanOrEqual(before + dropped);
  await expect(statusLine(page)).toContainText('Connected', { timeout: 30_000 });
}

// Traffic is everything a page was sent: each frame on each of its sockets
// (LiveKit's included) and the body of each answer from the API.
interface Traffic {
  readonly text: () => Promise<string>;
}

// A binary frame (LiveKit's) is bytes; decoded one character per byte, a name
// in it reads as itself.
const latin1 = new TextDecoder('latin1');

// watchTraffic records what page is sent from now on. Call it before the page
// loads.
function watchTraffic(page: Page): Traffic {
  const seen: string[] = [];
  const reads: Promise<void>[] = [];
  page.on('websocket', (ws) => {
    ws.on('framereceived', (frame) => {
      const payload: unknown = frame.payload;
      seen.push(typeof payload === 'string' ? payload : latin1.decode(payload as Uint8Array));
    });
  });
  page.on('response', (res) => {
    // The API's answers; the client's own files are the same for everyone.
    if (!/^\/v[0-9]+\//.test(new URL(res.url()).pathname)) return;
    reads.push(
      res.text().then(
        (body) => {
          seen.push(body);
        },
        () => undefined,
      ),
    );
  });
  return {
    text: async () => {
      await Promise.all(reads);
      return seen.join('\n');
    },
  };
}

// expectNothingOf checks that none of a group's names or words is anywhere in
// the page (its text, its markup, its address) or in anything it was sent.
async function expectNothingOf(page: Page, traffic: Traffic, markers: readonly string[], view: string): Promise<void> {
  const shown = await page.evaluate(() => [location.href, document.documentElement.outerHTML, document.body.innerText].join('\n'));
  const sent = await traffic.text();
  expect(sent.length, `${view}: the page was sent something`).toBeGreaterThan(0);
  for (const m of markers) {
    expect(shown, `${view}: the page shows "${m}" of the other group`).not.toContain(m);
    expect(sent, `${view}: the page was sent "${m}" of the other group`).not.toContain(m);
  }
}

// openMembers opens the members panel when it is closed (it starts closed on
// a narrow screen) and returns the list.
async function openMembers(page: Page) {
  const toggle = page.getByRole('button', { name: 'Members' });
  if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click();
  const list = page.getByRole('region', { name: 'Members list' });
  await expect(list).toBeVisible();
  return list;
}

sizes.forEach(([size, options], n) => {
  test(`A14 at ${size} a member of one group sees nothing of another, after a full load and after a reconnect`, async ({ meet, world }) => {
    const api = sharedApi(world);
    const op = world.tokens.operator;
    const tag = `a14-${String(n)}-${String(Date.now())}`;
    const aChannel = `${tag}-alpha`;
    const bChannel = `${tag}-bravo`;
    for (const name of [aChannel, bChannel]) await must(api, `create ${name}`, op, 'POST', '/v0/channels', { name });
    const ann = await person(api, op, `${tag}-ann`, aChannel);
    const abe = await person(api, op, `${tag}-abe`, aChannel);
    const bea = await person(api, op, `${tag}-bea`, bChannel);
    const say = (who: Person, channel: string, body: string) => must(api, `${who.name} posts`, who.token, 'POST', `/v2/channels/${channel}/messages`, { body });
    await say(ann, aChannel, `${tag} alpha before`);
    await say(abe, aChannel, `${tag} alpha second`);
    await say(bea, bChannel, `${tag} bravo before`);
    const markersOfA = [aChannel, ann.name, abe.name, `${tag} alpha`];

    const { page } = await meet(options);
    await page.addInitScript(trackSockets);
    const traffic = watchTraffic(page);
    await signIn(page, world, bea.token, bea.name);
    // B's one channel, and nothing else.
    await expect(channelButton(page, bChannel)).toBeVisible();
    await expect(channelButtons(page)).toHaveCount(1);
    await channelButton(page, bChannel).click();
    await expect(messages(page).getByText(`${tag} bravo before`)).toBeVisible();
    const members = await openMembers(page);
    await expect(members).toContainText(bea.name);
    await expectFits(page, `${size}: B's lobby with the members panel open`);
    await expectNothingOf(page, traffic, markersOfA, 'after sign-in');

    // A full load.
    await page.reload();
    await expect(statusLine(page)).toContainText('Connected');
    await expect(channelButtons(page)).toHaveCount(1);
    await channelButton(page, bChannel).click();
    await expect(messages(page).getByText(`${tag} bravo before`)).toBeVisible();
    await expect(await openMembers(page)).toContainText(bea.name);
    await expectNothingOf(page, traffic, markersOfA, 'after a full load');

    // The sockets drop and come back. A is busy meanwhile, and B's own
    // message after it arriving shows the page is live again.
    await dropSockets(page);
    await say(ann, aChannel, `${tag} alpha after`);
    await say(bea, bChannel, `${tag} bravo after`);
    await expect(messages(page).getByText(`${tag} bravo after`)).toBeVisible();
    await expect(channelButtons(page)).toHaveCount(1);
    await expect(await openMembers(page)).toContainText(bea.name);
    await expectFits(page, `${size}: B's lobby after a reconnect`);
    await expectNothingOf(page, traffic, markersOfA, 'after a reconnect');
  });
});

test.describe('with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('the voice presence check needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the voice presence check needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    const { loadWorld } = await import('./world');
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  test("A14 a member of one group never sees who is in another group's voice room, after a full load and after a reconnect", async ({ meet }) => {
    // Group A is the voice world's channel; alice is in its voice room.
    const a = await meet(desktop);
    await signIn(a.page, vw, vw.alice.token, vw.alice.name);
    await channelButton(a.page, vw.channel).click();
    await voiceRegion(a.page).getByRole('button', { name: 'Join voice' }).click();
    await expect(voiceRegion(a.page)).toContainText(`Connected in #${vw.channel}`, { timeout: 30_000 });

    // Group B: a channel and a member of their own, in voice in their room.
    const api = voiceApi(vw);
    const bChannel = 'a14-bravo-room';
    await must(api, 'create B channel', vw.operator, 'POST', '/v0/channels', { name: bChannel });
    const bea = await person(api, vw.operator, 'a14-bea', bChannel);
    vw.addSecret(bea.token);
    const markersOfA = [vw.channel, vw.alice.name, vw.bob.name, vw.carol.name];

    const b = await meet(phones[0][1]);
    await b.page.addInitScript(trackSockets);
    const traffic = watchTraffic(b.page);
    await signIn(b.page, vw, bea.token, bea.name);
    await expect(channelButtons(b.page)).toHaveCount(1);
    await channelButton(b.page, bChannel).click();
    await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).tap();
    await expect(voiceRegion(b.page)).toContainText(`Connected in #${bChannel}`, { timeout: 30_000 });
    const inVoice = voiceRegion(b.page).getByRole('list', { name: 'In voice' });
    await expect(inVoice).toContainText('You', { timeout: 15_000 });

    // The roster calls its reader "You". alice is in A's room all the while,
    // as the operator reads it: what B does not see is there to be seen.
    const aliceInVoice = async () => {
      const presence = (await must(api, "A's presence", vw.operator, 'GET', `/v1/channels/${vw.channel}/voice`)) as { rooms: { participants: { principal_id: number }[] }[] };
      return presence.rooms.some((r) => r.participants.some((p) => p.principal_id === vw.alice.id));
    };
    await expect.poll(aliceInVoice, { message: 'alice is in A\'s voice room', timeout: 15_000 }).toBe(true);
    await expectFits(b.page, "B's lobby in voice at phone size");
    await expectNothingOf(b.page, traffic, markersOfA, 'in voice');

    await b.page.reload();
    await expect(statusLine(b.page)).toContainText('Connected');
    await expect(channelButtons(b.page)).toHaveCount(1);
    await channelButton(b.page, bChannel).click();
    await expect(voiceRegion(b.page).getByRole('button', { name: 'Join voice' })).toBeVisible();
    await expectNothingOf(b.page, traffic, markersOfA, 'after a full load');

    await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).tap();
    await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('You', { timeout: 15_000 });
    await dropSockets(b.page);
    await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('You', { timeout: 15_000 });
    expect(await aliceInVoice(), 'alice is still in A\'s voice room').toBe(true);
    await expectNothingOf(b.page, traffic, markersOfA, 'after a reconnect');
  });
});
