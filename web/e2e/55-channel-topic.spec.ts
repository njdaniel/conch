import type { Locator, Page } from '@playwright/test';
import { createChannelResponseV1, setChannelTopicResponseV1, type ChannelVisibility, type CreateChannelRequestV1 } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { channelButton, expect, openChannel, showView, signIn, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const nav = (page: Page) => page.getByRole('navigation', { name: 'Channels' });
const directory = (page: Page) => nav(page).getByRole('list', { name: 'All channels' });
const topic = (page: Page, text: string) => page.locator('.conv-head .channel-topic').getByText(text, { exact: true });

interface Member {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

async function member(world: World, secrets: string[], name: string): Promise<Member> {
  const principal: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  return { id, name, token };
}

async function make(world: World, token: string, name: string, visibility: ChannelVisibility) {
  const request: CreateChannelRequestV1 = { name, visibility };
  return (await ok(world, token, 'POST', '/v1/channels', createChannelResponseV1, request)).channel;
}

async function add(world: World, channel: string, person: Member): Promise<void> {
  const reply = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(person.id)}`);
  expect(reply.status).toBeGreaterThanOrEqual(200);
  expect(reply.status).toBeLessThan(300);
}

function directoryRow(page: Page, name: string): Locator {
  return directory(page).getByRole('listitem').filter({ has: page.locator('.channel-name').getByText(name, { exact: true }) });
}

test('C1504 a manager sets a topic; members and the directory learn it, while private topics and member controls stay bounded', async ({ meet, secrets, world }) => {
  const suffix = Date.now().toString(36);
  const manager = await member(world, secrets, `c1504-${suffix}-manager`);
  const reader = await member(world, secrets, `c1504-${suffix}-reader`);
  const outsider = await member(world, secrets, `c1504-${suffix}-outsider`);
  const publicName = `c1504-${suffix}-public`;
  const privateName = `c1504-${suffix}-private`;
  const publicTopic = `Public purpose ${suffix}`;
  const privateTopic = `Private purpose ${suffix}`;
  const publicChannel = await make(world, manager.token, publicName, 'public');
  const privateChannel = await make(world, manager.token, privateName, 'private');
  await add(world, publicName, reader);
  await ok(world, manager.token, 'PUT', `/v1/channels/${String(privateChannel.id)}/topic`, setChannelTopicResponseV1, { topic: privateTopic });

  const managerPage = (await meet()).page;
  const readerPage = (await meet()).page;
  await signIn(managerPage, world, manager.token, manager.name);
  await signIn(readerPage, world, reader.token, reader.name);
  await openChannel(managerPage, publicName);
  await openChannel(readerPage, publicName);
  await expect(readerPage.getByLabel(`Message #${publicName}`)).toBeEnabled();

  await nav(managerPage).getByRole('button', { name: `Actions for ${publicName}` }).click();
  await nav(managerPage).getByRole('menuitem', { name: 'Set topic' }).click();
  const form = nav(managerPage).getByRole('form', { name: 'Set topic' });
  await expect(form.getByLabel('Topic')).toHaveValue('');
  await form.getByLabel('Topic').fill(publicTopic);
  await form.getByRole('button', { name: 'Set topic' }).click();
  await expect(form).toHaveCount(0);
  await expect(topic(managerPage, publicTopic)).toBeVisible();
  await expect(topic(readerPage, publicTopic)).toBeVisible();

  const readerActions = nav(readerPage).getByRole('button', { name: `Actions for ${publicName}` });
  if (await readerActions.count()) {
    await readerActions.click();
    await expect(nav(readerPage).getByRole('menuitem', { name: 'Set topic' })).toHaveCount(0);
    await readerPage.keyboard.press('Escape');
  }
  await expect(nav(readerPage).getByRole('form', { name: 'Set topic' })).toHaveCount(0);

  await showView(managerPage, 'Channels');
  await nav(managerPage).getByRole('button', { name: 'Browse channels' }).click();
  await expect(directoryRow(managerPage, publicName).getByText(publicTopic, { exact: true })).toBeVisible();
  await managerPage.reload();
  await expect(statusLine(managerPage)).toContainText('Connected');
  await expect(topic(managerPage, publicTopic)).toBeVisible();

  const outsiderPage = (await meet()).page;
  await signIn(outsiderPage, world, outsider.token, outsider.name);
  await showView(outsiderPage, 'Channels');
  await nav(outsiderPage).getByRole('button', { name: 'Browse channels' }).click();
  await expect(directoryRow(outsiderPage, publicName).getByText(publicTopic, { exact: true })).toBeVisible();
  await expect(directory(outsiderPage).getByText(privateName, { exact: true })).toHaveCount(0);
  await expect(outsiderPage.locator('body')).not.toContainText(privateTopic);
  expect(publicChannel.topic).toBeUndefined();
  await expect(channelButton(readerPage, publicName)).toHaveAttribute('aria-current', 'true');
});
