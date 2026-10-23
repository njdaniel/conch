// Issue #695: an operator makes a channel in the browser. It is listed and
// opened at once, is still there after a reload, and making it joins no voice.
// A second try at the same name is refused on the page and makes nothing. A
// member is offered no control, and the server refuses the member's own
// request whatever the page shows. The channels made here are the operator's
// alone (the creator is a new channel's only member), so no other spec's
// people see them.
import type { Page } from '@playwright/test';
import { listChannelsResponse, type CreateChannelRequest } from '../src/schema/channel';
import { errorV0 } from '../src/schema/error';
import { channelButton, expect, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });

async function channelNames(world: World, token: string): Promise<string[]> {
  return (await ok(world, token, 'GET', '/v1/channels', listChannelsResponse)).channels.map((c) => c.name);
}

// voiceRequests lists every request the page makes for a voice session: the
// one way into a channel's voice.
function voiceRequests(page: Page): string[] {
  const seen: string[] = [];
  page.on('request', (req) => {
    const path = new URL(req.url()).pathname;
    if (path.endsWith('/voice/session')) seen.push(`${req.method()} ${path}`);
  });
  return seen;
}

// The test does not sign out: that would revoke the world's operator
// credential, which later specs use.
test('C695 an operator makes a channel, sees it, reloads and still sees it, without joining its voice', async ({ meet, world }) => {
  const name = `c695-${String(Date.now())}`;
  const { page } = await meet();
  const voice = voiceRequests(page);
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');

  await nav(page).getByRole('button', { name: 'New channel' }).click();
  const form = page.getByRole('form', { name: 'New channel' });
  await form.getByLabel('Channel name').fill(name);
  await form.getByRole('button', { name: 'Create channel' }).click();
  await expect(nav(page).getByText(`Created #${name}`)).toBeVisible();
  await expect(channelButton(page, name)).toHaveAttribute('aria-current', 'true');
  await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
  expect(await channelNames(world, world.tokens.operator)).toContain(name);

  await page.reload();
  await expect(statusLine(page)).toContainText('Connected');
  await expect(channelButton(page, name)).toBeVisible();

  // The same name again: refused, said so, the name kept, nothing made.
  await nav(page).getByRole('button', { name: 'New channel' }).click();
  await form.getByLabel('Channel name').fill(name);
  await form.getByRole('button', { name: 'Create channel' }).click();
  await expect(nav(page).getByText('There is already a channel with that name, perhaps one you are not in. Choose another name.')).toBeVisible();
  await expect(form.getByLabel('Channel name')).toHaveValue(name);
  expect((await channelNames(world, world.tokens.operator)).filter((n) => n === name)).toHaveLength(1);
  await expect(channelButton(page, name)).toHaveCount(1);

  expect(voice, 'voice sessions the page asked for').toEqual([]);
});

test('C695 a member is offered no control, and the server refuses the member anyway', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expect(channelButton(page, 'general')).toBeVisible();
  await expect(nav(page).getByRole('button', { name: 'New channel' })).toHaveCount(0);
  await expect(page.getByRole('form', { name: 'New channel' })).toHaveCount(0);

  // Made, the channel would be the member's alone, so their list would show it.
  const request: CreateChannelRequest = { name: `c695-member-${String(Date.now())}` };
  const refused = await call(world, world.tokens.browser, 'POST', '/v0/channels', request);
  expect(refused.status).toBe(403);
  expect(errorV0(refused.body)).toEqual([]);
  expect((refused.body as { code: string }).code).toBe('forbidden');
  expect(await channelNames(world, world.tokens.browser)).not.toContain(request.name);
});
