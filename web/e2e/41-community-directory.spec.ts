// Issue #991: the browser's half of the tester story. A newcomer who belongs to
// no channel finds a public channel in the directory, joins it, and reads the
// history from before they came; sees the owner in the community panel with the
// agent marked; leaves; and makes a private channel of their own, which carries
// a lock, which nobody else's directory or list shows, and whose name is taken
// in the one way every name is. Wide and on a phone, against the real server.
//
// The owner and the newcomer are made here by the operator's API with names no
// other spec uses, and each channel is theirs alone, so no other spec's people
// see anything of it. (The member-link half of the story, the newcomer
// arriving by a link and landing in a default channel, is #990's.)
import type { BrowserContextOptions, Locator, Page } from '@playwright/test';
import { channelDirectoryResponseV1, createChannelResponseV1, listChannelsResponse, type CreateChannelRequestV1 } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { serverMembersResponseV1 } from '../src/schema/principal';
import { post } from './api';
import { channelButton, expect, expectFits, showView, signIn, statusLine, test } from './fixtures';
import { watchTraffic } from './watch';
import { call, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const directory = (page: Page) => nav(page).getByRole('list', { name: 'All channels' });
const row = (page: Page, name: string): Locator => directory(page).getByRole('listitem').filter({ hasText: name });
const heading = (page: Page) => page.getByRole('region', { name: 'Conversation' }).getByRole('heading', { level: 2 });

const sizes: readonly (readonly [string, BrowserContextOptions, boolean])[] = [
  ['1440×900', { viewport: { width: 1440, height: 900 } }, false],
  ['390×844', { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true }, true],
];

interface Member {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

// member makes a human with a credential and no channel, as an operator does.
async function member(world: World, secrets: string[], name: string): Promise<Member> {
  const principal: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

async function make(world: World, token: string, name: string, visibility: 'public' | 'private') {
  const request: CreateChannelRequestV1 = { name, visibility };
  return (await ok(world, token, 'POST', '/v1/channels', createChannelResponseV1, request)).channel;
}

const directoryOf = async (world: World, token: string) => (await ok(world, token, 'GET', '/v1/directory/channels', channelDirectoryResponseV1)).channels.map((c) => c.name);
const listOf = async (world: World, token: string) => (await ok(world, token, 'GET', '/v1/channels', listChannelsResponse)).channels.map((c) => c.name);

async function openMembers(page: Page): Promise<Locator> {
  const toggle = page.getByRole('button', { name: 'Members' });
  if ((await toggle.getAttribute('aria-expanded')) === 'false') await toggle.click();
  const list = page.getByRole('region', { name: 'Members list' });
  await expect(list).toBeVisible();
  return list;
}

for (const [size, options, phone] of sizes) {
  test(`C991 at ${size} a newcomer finds a public channel, joins it, reads its history, sees the owner and the agent, and leaves`, async ({ meet, secrets, world }) => {
    const tag = `c991-${Date.now().toString(36).slice(-6)}${phone ? 'p' : 'w'}`;
    const owner = await member(world, secrets, `${tag}-owner`);
    const newcomer = await member(world, secrets, `${tag}-new`);
    const pub = `${tag}-lobby`;
    const priv = `${tag}-secret`;
    await make(world, owner.token, pub, 'public');
    await make(world, owner.token, priv, 'private');
    const before = `${tag} said before the newcomer came`;
    await post(world, owner.token, pub, before);
    await post(world, owner.token, priv, `${tag} private words`);

    const { page } = await meet(options);
    const traffic = watchTraffic(page);
    await signIn(page, world, newcomer.token, newcomer.name);
    await showView(page, 'Channels');
    // In no channel yet, and told what to do; not told of anyone's.
    await expect(nav(page).getByText('No channels yet')).toBeVisible();
    await expect(nav(page).getByRole('button', { name: 'Browse channels' })).toBeVisible();

    await nav(page).getByRole('button', { name: 'Browse channels' }).click();
    await expect(directory(page)).toBeVisible();
    await expect(row(page, pub)).toBeVisible();
    await expect(row(page, pub).getByRole('button', { name: `Join ${pub}` })).toBeVisible();
    // No lock on a public channel, no counts of people, and the private channel
    // is not there at all.
    await expect(row(page, pub).locator('.private-mark')).toHaveCount(0);
    await expect(row(page, pub)).not.toContainText(/\b\d+\b/);
    await expect(directory(page).getByText(priv)).toHaveCount(0);
    await expectFits(page, `${size}: the directory`);
    expect(await directoryOf(world, newcomer.token)).not.toContain(priv);

    // Join: the channel is opened, with the history from before the newcomer.
    await row(page, pub).getByRole('button', { name: `Join ${pub}` }).click();
    // A phone is taken to the chat at once, so the row's own line is for the wide list.
    if (!phone) await expect(row(page, pub).getByText(`Joined #${pub}`)).toBeVisible();
    await expect(heading(page)).toHaveText(pub);
    await expect(page.getByRole('log', { name: 'Messages' }).getByText(before)).toBeVisible();
    expect(await listOf(world, newcomer.token)).toEqual([pub]);
    if (phone) await expect(page.getByRole('region', { name: 'Conversation' })).toBeVisible();

    // The community panel: the owner is listed, the newcomer is "You", and an
    // agent is marked as one. The directory lists no one else.
    const people = await openMembers(page);
    const community = people.getByRole('list', { name: 'People' });
    await expect(community.getByRole('listitem').filter({ hasText: owner.name })).toBeVisible();
    await expect(community.getByRole('listitem').filter({ hasText: newcomer.name }).getByText('You')).toBeVisible();
    const listed = await ok(world, newcomer.token, 'GET', '/v1/directory/members', serverMembersResponseV1);
    const agent = listed.members.find((m) => m.kind === 'agent');
    expect(agent, 'the fixture community has an agent the directory lists').toBeDefined();
    await expect(community.getByRole('listitem').filter({ hasText: agent?.name ?? '' }).getByText('Agent')).toBeVisible();
    await expectFits(page, `${size}: the community panel`);

    // Leave: it goes from the list, and the directory offers Join again.
    await showView(page, 'Channels');
    await row(page, pub).getByRole('button', { name: `Leave ${pub}` }).click();
    await expect(row(page, pub).getByRole('button', { name: `Join ${pub}` })).toBeVisible();
    await expect(channelButton(page, pub)).toHaveCount(0);
    expect(await listOf(world, newcomer.token)).toEqual([]);

    // Nothing of the private channel reached this page, in any answer.
    // In no API answer and on no socket frame either.
    expect((await traffic.text()).length, 'the page was sent something').toBeGreaterThan(0);
    expect(await traffic.text(), 'the private channel appeared in an answer or a socket frame the newcomer was sent').not.toContain(priv);
    await expect(page.locator('body')).not.toContainText(priv);
  });

  test(`C991 at ${size} a member makes a private channel: it has a lock, it is theirs alone, the last member cannot leave it, and its name is taken the way every name is`, async ({ meet, secrets, world }) => {
    const tag = `c991m-${Date.now().toString(36).slice(-6)}${phone ? 'p' : 'w'}`;
    const maker = await member(world, secrets, `${tag}-maker`);
    const other = await member(world, secrets, `${tag}-other`);
    const taken = `${tag}-public`;
    await make(world, other.token, taken, 'public');
    const priv = `${tag}-private`;

    const { page } = await meet(options);
    await signIn(page, world, maker.token, maker.name);
    await showView(page, 'Channels');
    await nav(page).getByRole('button', { name: 'New channel' }).click();
    const form = page.getByRole('form', { name: 'New channel' });
    await form.getByLabel('Channel name').fill(priv);
    await form.getByRole('radio', { name: /^Private/ }).check();
    await form.getByRole('button', { name: 'Create channel' }).click();
    await expect(nav(page).getByText(`Created #${priv}`)).toBeVisible();
    // Opened, as its manager, and marked private: the list, and the heading.
    await expect(channelButton(page, priv)).toHaveAttribute('aria-current', 'true');
    await expect(nav(page).locator('li').filter({ has: channelButton(page, priv) }).getByRole('button', { name: `Actions for ${priv}` })).toHaveCount(0);
    await expect(nav(page).locator('.ag-badge.manages')).toBeVisible();
    await showView(page, 'Chat');
    await expect(heading(page)).toHaveText(priv);
    await expect(page.locator('.conv-head .private-mark')).toBeVisible();
    await expect(page.locator('.conv-head .private-mark .vh')).toHaveText('Private');
    await expectFits(page, `${size}: a private channel's heading`);

    // Theirs alone: nobody else's directory or list has it.
    expect(await directoryOf(world, other.token)).not.toContain(priv);
    expect(await listOf(world, other.token)).not.toContain(priv);
    expect(await directoryOf(world, maker.token)).toContain(priv);

    // The directory marks it, and the last member is told why they cannot leave.
    await showView(page, 'Channels');
    await nav(page).getByRole('button', { name: 'Browse channels' }).click();
    const mine = row(page, priv);
    await expect(mine.locator('.private-mark .vh')).toHaveText('Private');
    await expect(mine.getByText('Manager')).toBeVisible();
    await expectFits(page, `${size}: the directory with a private channel`);
    await mine.getByRole('button', { name: `Leave ${priv}` }).click();
    await page.getByRole('group', { name: `Leave ${priv}?` }).getByRole('button', { name: 'Yes, leave' }).click();
    await expect(mine.getByText('You are the last member of this private channel. Add someone else before you leave.')).toBeVisible();
    expect(await listOf(world, maker.token)).toContain(priv);

    // A name in the way reads the same whether the channel is public or the
    // maker's own private one; and to a third person who cannot see that
    // private channel, the server's answer is the one it gives for a public
    // name.
    await nav(page).getByRole('button', { name: 'Browse channels' }).click();
    await nav(page).getByRole('button', { name: 'New channel' }).click();
    for (const name of [taken, priv]) {
      await form.getByLabel('Channel name').fill(name);
      await form.getByRole('button', { name: 'Create channel' }).click();
      await expect(nav(page).getByText('That name is taken', { exact: true })).toBeVisible();
      await expect(form.getByLabel('Channel name')).toHaveValue(name);
    }
    const asOther = await call(world, other.token, 'POST', '/v1/channels', { name: priv, visibility: 'private' });
    const asPublic = await call(world, other.token, 'POST', '/v1/channels', { name: taken, visibility: 'private' });
    expect(asOther.status).toBe(409);
    expect(asOther).toEqual(asPublic);
    await expect(statusLine(page)).toContainText('Connected');
  });
}
