// P3-05: what is restricted to test-private stays there, a run in it included.
import type { Locator, Page } from '@playwright/test';
import { history, post, roster, run, runsIn } from './api';
import { launchWorker } from './agents';
import { createChannelResponse, type CreateChannelRequest } from '../src/schema/channel';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { channelButton, conversation, expect, messages, signIn, statusLine, test } from './fixtures';
import { call, ok, principalId, type World } from './world';

test('P3-05 browser-test cannot open test-private and nothing about a run there shows in its members panel', async ({ meet, world }) => {
  // worker-test is busy in test-private (a slow fixture, so that the run stays)
  // with something nick-test asked of it there.
  const worker = await launchWorker(world, 'slow.sh', { channel: 'test-private', sleep: 60 });
  try {
    const task = 'p3-05 private work for the worker';
    const asked = await post(world, world.tokens.nick, 'test-private', task);
    let runId = 0;
    await expect
      .poll(
        async () => {
          runId = (await runsIn(world, world.tokens.nick, 'test-private')).find((r) => r.message_id === asked.id)?.id ?? 0;
          return runId;
        },
        { message: 'a run answering the message in test-private' },
      )
      .toBeGreaterThan(0);

    // The page is asked for test-private by name, and does not open it.
    const { page } = await meet();
    await page.goto(`${world.appUrl}#/c/test-private`);
    await page.getByLabel('Credential').fill(world.tokens.browser);
    await page.getByRole('button', { name: 'Use credential' }).click();
    await expect(channelButton(page, 'general')).toBeVisible();
    await expect(channelButton(page, 'test-private')).toHaveCount(0);
    await expect(conversation(page).getByRole('heading', { name: 'general' })).toBeVisible();
    await expect(conversation(page).getByRole('heading', { name: 'test-private' })).toHaveCount(0);

    // The server refuses it too: the channel, and the run by its id.
    expect((await call(world, world.tokens.browser, 'GET', '/v2/channels/test-private/messages?after=0&limit=10')).status).toBeGreaterThanOrEqual(400);
    expect((await call(world, world.tokens.browser, 'GET', `/v1/runs/${String(runId)}`)).status).toBe(404);

    // The panel of general knows that worker-test is busy (its status is on the
    // roster for every member), and that is all: no run, no task, no session
    // detail that leads to either.
    const panel = page.getByRole('region', { name: 'Members list' });
    const agent = panel.getByRole('listitem').filter({ has: page.getByRole('button', { name: /worker-test/ }) });
    await expect(agent).toContainText('running');
    // The runs of an unfolded agent's sessions are read when it is opened;
    // wait for that read to finish so that the negative checks follow it.
    const runsRead = page.waitForResponse((r) => r.url().includes('/v1/runs?') && r.request().method() === 'GET');
    await agent.getByRole('button', { name: /worker-test/ }).click();
    await runsRead;
    await expect(panel.getByRole('list', { name: 'Sessions of worker-test' })).toBeVisible();
    await expect(panel).not.toContainText('task:');
    await expect(panel).not.toContainText(task);
    await expect(panel.getByRole('button', { name: /Open run|^Run #/ })).toHaveCount(0);
    await expect(panel.getByRole('list', { name: /Recent runs/ })).toHaveCount(0);
    await expect(panel).not.toContainText('test-private');

    // The API's roster and run list say the same for browser-test, while the run
    // is still going on for those who may see it.
    const lines = (await roster(world, world.tokens.browser, 'general')).filter((s) => s.name === 'worker-test');
    expect(lines.length).toBeGreaterThan(0);
    for (const line of lines) {
      expect(line.current_run).toBeUndefined();
      expect(line.task).toBeUndefined();
    }
    expect((await runsIn(world, world.tokens.browser, 'general')).some((r) => r.id === runId)).toBe(false);
    expect((await run(world, world.tokens.nick, runId)).state).toMatch(/running|starting|queued/);
  } finally {
    await worker.stop();
  }
});

// The browser as sender (issue #352). nets-demo has two nets: red (nick-test
// transmits, jamie-test monitors) and blue (jamie-test and browser-test). Who
// reads what is checked through the API with the participants' own
// credentials, as the TUI's would be: the server decides the audience.
test('P3-05 the browser composes to a net and to a whisper, and only the audience reads it', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await channelButton(page, 'nets-demo').click();
  await expect(conversation(page).getByRole('heading', { name: 'nets-demo' })).toBeVisible();

  // The control offers the channel, the viewer's nets (blue, not red) and the whisper.
  const to = page.getByLabel('To:');
  await expect(to.getByRole('option')).toHaveText(['#nets-demo', 'net blue', 'Whisper to…']);

  // A net post: a member (jamie-test) reads it with the net's audience; a
  // non-member (nick-test) never does.
  // Unique per run: a repeated run on the same server must not see an
  // earlier run's message as a second copy.
  const runTag = Date.now().toString(36);
  const netBody = `p3-05 browser to blue ${runTag}`;
  await to.selectOption({ label: 'net blue' });
  await page.getByLabel('Message net blue').fill(netBody);
  await page.getByRole('button', { name: 'Send' }).click();
  // The socket's frame can arrive before the POST's answer, and for that
  // moment the confirmed message and the pending copy (tagged Sending) are both
  // shown. Wait for the confirmed one, then for the pending one to go.
  const netItems = messages(page).getByRole('listitem').filter({ hasText: netBody });
  // The settled copy carries the Sent tag (not a substring of Sending); a
  // failed or unconfirmed send never does.
  await expect(netItems.filter({ hasText: 'Sent' })).toHaveCount(1);
  await expect(netItems).toHaveCount(1);
  await expect(netItems).toContainText('→ net blue');
  // A net stays chosen after a post.
  await expect(to).toHaveValue(/^net:/);
  const seenByGuest = (await history(world, world.tokens.guest, 'nets-demo')).filter((m) => m.body === netBody);
  expect(seenByGuest).toHaveLength(1);
  expect(seenByGuest[0]?.audience?.kind).toBe('net');
  expect((await history(world, world.tokens.nick, 'nets-demo')).filter((m) => m.body === netBody)).toHaveLength(0);
  // Still once after the round trips: a late duplicate would show here.
  await expect(netItems).toHaveCount(1);

  // A whisper to jamie-test only; the composer is the channel's again after it,
  // and the audit notice shows.
  await to.selectOption({ label: 'Whisper to…' });
  await page.getByRole('group', { name: 'Whisper to' }).getByRole('checkbox', { name: 'jamie-test' }).check();
  const whisperBody = `p3-05 browser whispers to guest ${runTag}`;
  await page.getByLabel('Whisper to jamie-test').fill(whisperBody);
  await page.getByRole('button', { name: 'Send' }).click();
  // As for the net: the settled copy, once, with the whisper's badge.
  const whisperItems = messages(page).getByRole('listitem').filter({ hasText: whisperBody });
  await expect(whisperItems.filter({ hasText: 'Sent' })).toHaveCount(1);
  await expect(whisperItems).toHaveCount(1);
  await expect(whisperItems).toContainText('→ whisper to 1');
  await expect(page.getByText('note: whispers are recorded in the audit log')).toBeVisible();
  await expect(to).toHaveValue('channel');
  const guest = await principalId(world, world.tokens.guest);
  const whispered = (await history(world, world.tokens.guest, 'nets-demo')).filter((m) => m.body === whisperBody);
  expect(whispered).toHaveLength(1);
  expect(whispered[0]?.audience).toMatchObject({ kind: 'principals' });
  expect(whispered[0]?.audience?.kind === 'principals' ? whispered[0].audience.principal_ids : []).toContain(guest);
  expect((await history(world, world.tokens.nick, 'nets-demo')).filter((m) => m.body === whisperBody)).toHaveLength(0);
  expect((await history(world, world.tokens.worker, 'nets-demo')).filter((m) => m.body === whisperBody)).toHaveLength(0);
});

// Issue #1027: a person's channel list follows membership changes with no
// reload. The owner (an operator) works in a browser of their own; member-1 is
// in channel-b only, and is added to, and removed from, channel-a. Each wait is
// two seconds: the server coalesces for 250 ms.
const live = { timeout: 2000 };
const panel = (page: Page) => page.getByRole('region', { name: 'Members list' });
const memberRow = (page: Page, name: string): Locator =>
  panel(page).locator('li').filter({ has: page.locator('.ag-name').getByText(name, { exact: true }) });

// chooseFor opens a member row's ⋯ menu and chooses one item (issue #1094).
async function chooseFor(row: Locator, name: string, item: string): Promise<void> {
  await row.getByRole('button', { name: `Actions for ${name}` }).click();
  await row.getByRole('menuitem', { name: item }).click();
  // Removal asks first (issue #1083).
  if (item === 'Remove from channel') await row.getByRole('button', { name: 'Yes, remove' }).click();
}

async function seedMembership(world: World, secrets: string[], suffix: string) {
  const a = `channel-a-${suffix}`;
  const b = `channel-b-${suffix}`;
  for (const name of [a, b]) {
    const request: CreateChannelRequest = { name };
    await ok(world, world.tokens.operator, 'POST', '/v0/channels', createChannelResponse, request);
  }
  const member = `member-1-${suffix}`;
  const principal: CreatePrincipalRequest = { kind: 'human', name: member };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, principal)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: member };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);
  const put = await call(world, world.tokens.operator, 'PUT', `/v1/channels/${encodeURIComponent(b)}/members/${String(id)}`);
  expect(put.status).toBeLessThan(300);
  return { a, b, member, token };
}

async function ownerPage(meet: () => Promise<{ page: Page }>, world: World): Promise<Page> {
  const { page } = await meet();
  await page.goto(world.appUrl);
  await page.getByLabel('Credential').fill(world.tokens.operator);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
  return page;
}

async function addByName(owner: Page, name: string): Promise<void> {
  const form = panel(owner).getByRole('form', { name: 'Add person by name' });
  const button = form.getByRole('button', { name: 'Add by name' });
  // The form is busy while an earlier change is on its way: fill until it takes.
  await expect(async () => {
    await form.getByLabel('Exact name').fill(name);
    await expect(button).toBeEnabled({ timeout: 1000 });
  }).toPass();
  await button.click();
  await expect(memberRow(owner, name)).toBeVisible();
}

test('C1027 a channel the owner adds member-1 to shows in their list with its unread count, and goes when they are removed, with no reload', async ({ meet, secrets, world }) => {
  const { a, b, member, token } = await seedMembership(world, secrets, String(Date.now()));
  const owner = await ownerPage(() => meet({ viewport: { width: 1440, height: 900 } }), world);
  const other = (await meet({ viewport: { width: 1440, height: 900 } })).page;
  await signIn(other, world, token, member);
  await expect(channelButton(other, b)).toHaveAttribute('aria-current', 'true');
  await expect(channelButton(other, a)).toHaveCount(0);

  // Added: the channel shows within two seconds, with no reload.
  await channelButton(owner, a).click();
  await addByName(owner, member);
  await expect(channelButton(other, a)).toBeVisible(live);
  await expect(channelButton(other, b)).toHaveAttribute('aria-current', 'true');

  // A post in it: member-1's badge shows 1 within two seconds.
  await owner.getByLabel(`Message #${a}`).fill('hello from the owner');
  await owner.getByRole('button', { name: 'Send' }).click();
  await expect(other.getByRole('img', { name: '1 unread' })).toBeVisible(live);

  // Removed while it is not open: it goes from the list, with no line.
  await chooseFor(memberRow(owner, member), member, 'Remove from channel');
  await expect(memberRow(owner, member)).toHaveCount(0);
  await expect(channelButton(other, a)).toHaveCount(0, live);
  await expect(other.getByText(/You no longer have access/)).toHaveCount(0);
  await expect(channelButton(other, b)).toHaveAttribute('aria-current', 'true');

  // Added again and opened, then removed: another channel opens with the line.
  await addByName(owner, member);
  await expect(channelButton(other, a)).toBeVisible(live);
  await channelButton(other, a).click();
  await expect(other.getByText('hello from the owner')).toBeVisible();
  await chooseFor(memberRow(owner, member), member, 'Remove from channel');
  await expect(memberRow(owner, member)).toHaveCount(0);
  await expect(other.getByText(`You no longer have access to #${a}`)).toBeVisible(live);
  await expect(channelButton(other, a)).toHaveCount(0);
  await expect(channelButton(other, b)).toHaveAttribute('aria-current', 'true');
  await expect(other.getByText('hello from the owner')).toHaveCount(0);
});
