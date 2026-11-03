// A14 (issue #447, the private register of issue #991): two groups on one
// server, and the page of a member of one shows nothing of the other's
// channels, and is sent nothing of them on any socket or in any answer: not
// the channel's name, its messages, who is in its voice room or how many are
// (issue #596: the occupancy socket counts the reader's own channels only).
// The other group's people are another matter: they are members of the
// community, and the community member panel lists them (design section 5,
// see_member_directory is on for members), so the page may show their names,
// but never the channel they are in or anything they said there. Checked
// after a full load and after the page's sockets drop and it reconnects by
// itself. The server-side matrix is e2e/isolation; this is what a person sees.
//
// Each test makes its own two groups through the operator's API, with names
// no other spec uses, so the people it adds touch no other spec. The voice
// test runs against a conchd with LiveKit of its own (voice-world.ts):
// without Docker it fails when CI is set and is skipped otherwise.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { channelButton, consoleErrors, expect, expectFits, messages, openChannel, phones, showView, signIn, statusLine, test } from './fixtures';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { findLeak } from './traffic';
import { watchTraffic, type Traffic } from './watch';
import { call, type World } from './world';

const desktop: BrowserContextOptions = { viewport: { width: 1280, height: 800 } };
const desktopWide: BrowserContextOptions = { viewport: { width: 1440, height: 900 } };
const sizes = [['desktop', desktop], ...phones] as const;

// The channels' own buttons: not a row's ⋯, and not the directory's or the
// creation form's toggles (issue #991).
const channelButtons = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name: /^(?!Actions for |Browse channels$|New channel$)/ });
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

// expectNothingOf checks that none of a group's names or words is anywhere in
// the page (its text, its markup, its address) or in anything it was sent.
//
// people are names that are allowed in the community member list (issue #991)
// and nowhere else the page is sent: not on a socket, not in a channel's
// answers, so a leak by another way is still caught.
async function expectNothingOf(page: Page, traffic: Traffic, markers: readonly string[], view: string, people: readonly string[] = []): Promise<void> {
  const shown = await page.evaluate(() => [location.href, document.documentElement.outerHTML, document.body.innerText].join('\n'));
  const sent = await traffic.text();
  expect(sent.length, `${view}: the page was sent something`).toBeGreaterThan(0);
  for (const m of markers) {
    expect(shown, `${view}: the page shows "${m}" of the other group`).not.toContain(m);
    // Random tickets and tokens are blanked first (traffic.ts, issue #1069).
    expect(findLeak(sent, m), `${view}: the page was sent something of the other group`).toBeUndefined();
  }
  const elsewhere = await traffic.textExcept('/v1/directory/members');
  for (const name of people) {
    expect(findLeak(elsewhere, name), `${view}: the other group's person was sent outside the member directory`).toBeUndefined();
  }
}

// The voice counts in the channel list (issue #596).
const voiceMarks = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('img', { name: / in voice$/ });

// Occupancy is every document the page's occupancy socket was sent, and how
// many such sockets it opened. A row names who is there by principal id
// (issue #1103).
interface Occupancy {
  readonly frames: { channels: { channel_id: number; in_voice: number; principal_ids?: number[] }[] }[];
  readonly opened: () => number;
}

// watchOccupancy records the occupancy socket's frames (issue #596). Call it
// before the page loads.
function watchOccupancy(page: Page): Occupancy {
  const frames: Occupancy['frames'] = [];
  let opened = 0;
  page.on('websocket', (ws) => {
    if (new URL(ws.url()).pathname !== '/v1/voice/occupancy/ws') return;
    opened++;
    ws.on('framereceived', (frame) => {
      if (typeof frame.payload !== 'string') return;
      const doc = JSON.parse(frame.payload) as { schema?: unknown; channels?: unknown };
      if (doc.schema === 'conch.voice_occupancy.v1' && Array.isArray(doc.channels)) frames.push(doc as Occupancy['frames'][number]);
    });
  });
  return { frames, opened: () => opened };
}

// expectOnlyOwn checks that the occupancy socket sent something, and that
// every channel it counted is one of the reader's own, never one of the
// other group's: absent, not zero; and that nobody of the other group's
// room is named in any row (issue #1103).
function expectOnlyOwn(occupancy: Occupancy, own: readonly number[], other: readonly number[], view: string, otherPeople: readonly number[] = []): void {
  expect(occupancy.frames.length, `${view}: the occupancy socket sent a document`).toBeGreaterThan(0);
  for (const f of occupancy.frames) {
    for (const c of f.channels) {
      expect(own, `${view}: the occupancy socket counted channel ${String(c.channel_id)}`).toContain(c.channel_id);
      expect(other, `${view}: the occupancy socket counted a channel of the other group`).not.toContain(c.channel_id);
      for (const id of c.principal_ids ?? []) expect(otherPeople, `${view}: the occupancy socket named principal ${String(id)} of the other group's room`).not.toContain(id);
    }
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
    // A's channel and its words are never shown or sent; its people are listed.
    const markersOfA = [aChannel, `${tag} alpha`];
    const peopleOfA = [ann.name, abe.name];

    const { page } = await meet(options);
    await page.addInitScript(trackSockets);
    const traffic = watchTraffic(page);
    await signIn(page, world, bea.token, bea.name);
    // B's one channel, and nothing else.
    await showView(page, 'Channels');
    await expect(channelButton(page, bChannel)).toBeVisible();
    await expect(channelButtons(page)).toHaveCount(1);
    await openChannel(page, bChannel);
    await expect(messages(page).getByText(`${tag} bravo before`)).toBeVisible();
    const members = await openMembers(page);
    await expect(members).toContainText(bea.name);
    // The private register: A's people are in the community, so they are
    // listed, by name alone; the channel they share is not.
    const community = members.getByRole('list', { name: 'People' });
    await expect(community).toContainText(ann.name);
    await expect(community).toContainText(abe.name);
    await expectFits(page, `${size}: B's lobby with the members panel open`);
    await expectNothingOf(page, traffic, markersOfA, 'after sign-in', peopleOfA);
    // Nor does the directory list A's channel.
    await showView(page, 'Channels');
    await page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name: 'Browse channels' }).click();
    await expect(page.getByRole('navigation', { name: 'Channels' }).getByRole('list', { name: 'All channels' })).toContainText(bChannel);
    await expectNothingOf(page, traffic, markersOfA, 'with the directory open', peopleOfA);
    await page.getByRole('navigation', { name: 'Channels' }).getByRole('button', { name: 'Browse channels' }).click();

    // A full load.
    await page.reload();
    await expect(statusLine(page)).toContainText('Connected');
    await showView(page, 'Channels');
    await expect(channelButtons(page)).toHaveCount(1);
    await openChannel(page, bChannel);
    await expect(messages(page).getByText(`${tag} bravo before`)).toBeVisible();
    await expect(await openMembers(page)).toContainText(bea.name);
    await expectNothingOf(page, traffic, markersOfA, 'after a full load', peopleOfA);

    // The sockets drop and come back. A is busy meanwhile, and B's own
    // message after it arriving shows the page is live again.
    await dropSockets(page);
    await say(ann, aChannel, `${tag} alpha after`);
    await say(bea, bChannel, `${tag} bravo after`);
    await expect(messages(page).getByText(`${tag} bravo after`)).toBeVisible();
    await showView(page, 'Channels');
    await expect(channelButtons(page)).toHaveCount(1);
    await expect(await openMembers(page)).toContainText(bea.name);
    await expectFits(page, `${size}: B's lobby after a reconnect`);
    await expectNothingOf(page, traffic, markersOfA, 'after a reconnect', peopleOfA);
  });
});

// Issue #515: someone taken out of the channel, or hidden, leaves the panel of
// a member who has it open, within seconds and without a reload, and a hidden
// account's old name leaves the page with it. amy never signs in, so she has
// no session and her removal changes no roster: only the member list the
// events socket sends can tell the page.
test('A14 a member removed from the channel, or hidden, leaves the open page of another member without a reload', async ({ meet, world }) => {
  const api = sharedApi(world);
  const op = world.tokens.operator;
  const tag = `a14-gone-${String(Date.now())}`;
  const channel = `${tag}-alpha`;
  await must(api, `create ${channel}`, op, 'POST', '/v0/channels', { name: channel });
  const ann = await person(api, op, `${tag}-ann`, channel);
  const abe = await person(api, op, `${tag}-abe`, channel);
  const amy = await person(api, op, `${tag}-amy`, channel);
  await must(api, `${abe.name} posts`, abe.token, 'POST', `/v2/channels/${channel}/messages`, { body: `${tag} said before` });

  const { page } = await meet(desktop);
  await signIn(page, world, ann.token, ann.name);
  await channelButton(page, channel).click();
  const said = messages(page).getByRole('listitem').filter({ hasText: `${tag} said before` });
  await expect(said).toContainText(abe.name);
  // The channel's own people (the community panel below lists everyone, and
  // amy stays a member of the community when she leaves the channel).
  const region = await openMembers(page);
  const members = region.locator('.ag-panel');
  await expect(members).toContainText(abe.name);
  await expect(members).toContainText(amy.name);

  await must(api, `remove ${amy.name}`, op, 'DELETE', `/v1/channels/${channel}/members/${String(amy.id)}`);
  await expect(members).not.toContainText(amy.name, { timeout: 5_000 });
  await expect(members).toContainText(abe.name);

  await must(api, `hide ${abe.name}`, op, 'POST', `/v1/principals/${String(abe.id)}/hide`);
  await expect(members).not.toContainText(abe.name, { timeout: 5_000 });
  // His message stays, by #<id> now, as a reload would show it.
  await expect(said).toContainText(`#${String(abe.id)}`);
  // The server says the directory changed (conch.directory_changed.v1) and the
  // community list is read again with no click: the hidden account is gone.
  await expect(page.locator('body')).not.toContainText(abe.name, { timeout: 5_000 });
  await expect(members).toContainText(ann.name);
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

  // Issue #1084 (browser UX audit 4, F7): a member removed from the channel
  // they are viewing, in a server with voice. Once the page has handled the
  // removal (the channel is gone from their list, #1027) it makes no request
  // and opens no socket for that channel, voice included. A request already
  // in flight when the removal came may still be answered 404, and that is
  // the only error the console may hold.
  test('F7 a member removed from the channel they are viewing makes no further request for it, and the console holds only the 404 of one in flight', async ({ meet }) => {
    const gone = await vw.addMember(`f7-gone-${String(Date.now())}`);
    const { page } = await meet(desktopWide);
    const errors = consoleErrors(page);
    await signIn(page, vw, gone.token, gone.name);
    await channelButton(page, vw.channel).click();
    await expect(voiceRegion(page).getByRole('button', { name: 'Join voice' })).toBeVisible();
    // Let the voice socket open and settle before the removal.
    await page.waitForTimeout(1_000);

    const asked: string[] = [];
    page.on('request', (req) => {
      const url = new URL(req.url());
      asked.push(`${req.method()} ${url.pathname}${url.search.includes(vw.channel) ? url.search.replace(/ticket=[^&]*/, '') : ''}`);
    });
    page.on('websocket', (ws) => {
      const url = new URL(ws.url());
      asked.push(`WS ${url.pathname}${url.search.replace(/ticket=[^&]*/, '')}`);
    });
    await vw.removeFromChannel(gone);
    // Handled: the channel is gone from their list.
    await expect(channelButton(page, vw.channel)).toHaveCount(0);
    const handled = asked.length;
    await page.waitForTimeout(5_000);

    const forChannel = (line: string) => line.includes(`/channels/${vw.channel}`) || line.includes(`channel=${vw.channel}`);
    expect(asked.slice(handled).filter(forChannel), 'requests for the lost channel after the removal was handled').toEqual([]);
    const inFlight404 = (line: string) => line.includes('status of 404') && line.includes(`/channels/${vw.channel}/`);
    expect(errors.filter((line) => !inFlight404(line)), 'console errors beyond the 404 of a request in flight').toEqual([]);
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
    // A's channel and its voice room are never shown or sent. Its people are
    // members of the community and are listed by name (issue #991), so the
    // check on who is in the room is on the room's own list below.
    const markersOfA = [vw.channel];
    const inRoomOfA = [vw.alice.name, vw.bob.name, vw.carol.name];
    const idsInRoomOfA = [vw.alice.id, vw.bob.id, vw.carol.id];
    // Nobody of A's room is named under any channel of B's list (issue #1103).
    const namedInList = (page: Page) => page.getByRole('navigation', { name: 'Channels' }).getByRole('list', { name: /^In voice in/ });

    // The channel ids of each group, for the occupancy socket (issue #596),
    // whose frames name channels by id alone.
    const idsOf = async (token: string) =>
      ((await must(api, 'channels', token, 'GET', '/v1/channels')) as { channels: { id: number }[] }).channels.map((c) => c.id);
    const aIds = await idsOf(vw.alice.token);
    const bIds = await idsOf(bea.token);
    expect(bIds).toHaveLength(1);

    const b = await meet(phones[0][1]);
    await b.page.addInitScript(trackSockets);
    const traffic = watchTraffic(b.page);
    const occupancy = watchOccupancy(b.page);
    await signIn(b.page, vw, bea.token, bea.name);
    await showView(b.page, 'Channels');
    await expect(channelButtons(b.page)).toHaveCount(1);
    await openChannel(b.page, bChannel);
    await showView(b.page, 'Voice');
    await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).tap();
    await expect(voiceRegion(b.page)).toContainText(`Connected in #${bChannel}`, { timeout: 30_000 });
    const inVoice = voiceRegion(b.page).getByRole('list', { name: 'In voice' });
    await expect(inVoice).toContainText('You', { timeout: 15_000 });
    // B's own count rises when B's person joins, so the silence about A
    // below means something.
    await showView(b.page, 'Channels');
    await expect(voiceMarks(b.page)).toHaveText(['1'], { timeout: 15_000 });
    await expect(voiceMarks(b.page).first()).toHaveAccessibleName('1 in voice');
    // The channel list names bea alone, as You, under B's channel (issue
    // #1103); the people of A's room are named under nothing.
    await expect(namedInList(b.page)).toHaveCount(1);
    await expect(namedInList(b.page).getByRole('listitem')).toHaveText(['You']);
    await showView(b.page, 'Voice');

    // The roster calls its reader "You". alice is in A's room all the while,
    // as the operator reads it: what B does not see is there to be seen.
    const aliceInVoice = async () => {
      const presence = (await must(api, "A's presence", vw.operator, 'GET', `/v1/channels/${vw.channel}/voice`)) as { rooms: { participants: { principal_id: number }[] }[] };
      return presence.rooms.some((r) => r.participants.some((p) => p.principal_id === vw.alice.id));
    };
    await expect.poll(aliceInVoice, { message: 'alice is in A\'s voice room', timeout: 15_000 }).toBe(true);
    await expectFits(b.page, "B's lobby in voice at phone size");
    for (const name of inRoomOfA) await expect(inVoice, `${name} of A's room is not in B's list`).not.toContainText(name);
    await expectNothingOf(b.page, traffic, markersOfA, 'in voice');
    expectOnlyOwn(occupancy, bIds, aIds, 'in voice', idsInRoomOfA);

    await b.page.reload();
    await expect(statusLine(b.page)).toContainText('Connected');
    await showView(b.page, 'Channels');
    await expect(channelButtons(b.page)).toHaveCount(1);
    await openChannel(b.page, bChannel);
    await showView(b.page, 'Voice');
    await expect(voiceRegion(b.page).getByRole('button', { name: 'Join voice' })).toBeVisible();
    // Not in voice, with alice in A's room: no line says anyone is in voice
    // anywhere else, and the only mark there could be is B's own channel's.
    await expect(voiceRegion(b.page).locator('.voice-elsewhere')).toHaveCount(0);
    await showView(b.page, 'Channels');
    expect(await voiceMarks(b.page).count()).toBeLessThanOrEqual(1);
    // Nobody of A's is named under any channel; at most B's own room's list.
    expect(await namedInList(b.page).count()).toBeLessThanOrEqual(1);
    for (const name of inRoomOfA) await expect(b.page.getByRole('navigation', { name: 'Channels' }), `${name} of A's room is not named in B's channel list`).not.toContainText(name);
    await expectNothingOf(b.page, traffic, markersOfA, 'after a full load');
    expectOnlyOwn(occupancy, bIds, aIds, 'after a full load', idsInRoomOfA);

    await showView(b.page, 'Voice');
    await voiceRegion(b.page).getByRole('button', { name: 'Join voice' }).tap();
    await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('You', { timeout: 15_000 });
    await dropSockets(b.page);
    await expect(voiceRegion(b.page).getByRole('list', { name: 'In voice' })).toContainText('You', { timeout: 15_000 });
    expect(await aliceInVoice(), 'alice is still in A\'s voice room').toBe(true);
    await expectNothingOf(b.page, traffic, markersOfA, 'after a reconnect');
    expectOnlyOwn(occupancy, bIds, aIds, 'after a reconnect', idsInRoomOfA);
    // One occupancy socket a load, opened again after the drop: three in all.
    expect(occupancy.opened(), 'occupancy sockets B\'s page opened').toBe(3);
  });
});
