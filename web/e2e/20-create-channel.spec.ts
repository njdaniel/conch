// Issues #695 and #991: a person makes a channel in the browser. It is listed
// and opened at once, is still there after a reload, and making it joins no
// voice. A second try at the same name is refused on the page, in the one
// line every collision gets, and makes nothing. A member is offered the same
// form with a choice of public or private, and the old operator-only route
// still refuses a member whatever the page shows. The channels made here are
// their makers' alone (the creator is a new channel's only member), so no
// other spec's people see them.
import type { Page } from '@playwright/test';
import { listChannelsResponse, type CreateChannelRequest } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { errorV0 } from '../src/schema/error';
import { channelButton, credentialField, expect, signIn, statusLine, test } from './fixtures';
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
  await (await credentialField(page)).fill(world.tokens.operator);
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
  await expect(nav(page).getByText('That name is taken', { exact: true })).toBeVisible();
  await expect(form.getByLabel('Channel name')).toHaveValue(name);
  expect((await channelNames(world, world.tokens.operator)).filter((n) => n === name)).toHaveLength(1);
  await expect(channelButton(page, name)).toHaveCount(1);

  expect(voice, 'voice sessions the page asked for').toEqual([]);
});

test('C991 a member is offered the form with a visibility choice and makes a channel, and the operator-only route still refuses a member', async ({ meet, secrets, world }) => {
  const name = `c991-m-${Date.now().toString(36)}`;
  const principal: CreatePrincipalRequest = { kind: 'human', name: `${name}-who` };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);

  const { page } = await meet();
  await signIn(page, world, token, principal.name);
  await expect(nav(page).getByRole('button', { name: 'New channel' })).toBeVisible();
  await nav(page).getByRole('button', { name: 'New channel' }).click();
  const form = page.getByRole('form', { name: 'New channel' });
  await expect(form.getByRole('radio', { name: /^Public/ })).toBeChecked();
  await expect(form.getByRole('radio', { name: /^Private/ })).not.toBeChecked();
  await form.getByLabel('Channel name').fill(name);
  await form.getByRole('button', { name: 'Create channel' }).click();
  await expect(nav(page).getByText(`Created #${name}`)).toBeVisible();
  await expect(channelButton(page, name)).toHaveAttribute('aria-current', 'true');
  expect(await channelNames(world, token)).toEqual([name]);

  // The old route is still the operator's alone.
  const request: CreateChannelRequest = { name: `${name}-v0` };
  const refused = await call(world, token, 'POST', '/v0/channels', request);
  expect(refused.status).toBe(403);
  expect(errorV0(refused.body)).toEqual([]);
  expect((refused.body as { code: string }).code).toBe('forbidden');
  expect(await channelNames(world, token)).not.toContain(request.name);
});
