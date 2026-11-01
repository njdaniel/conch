// P3-09: the server goes away and comes back on the same data and address. The
// page says it is out of date while the server is down, retries by itself, and
// shows the history once when it is back. Last, because it restarts the server
// the other specs share.
import { join } from 'node:path';
import { closeSync, openSync } from 'node:fs';
import { spawn } from 'node:child_process';
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { history, post } from './api';
import { channelButton, expect, expectStateReadable, messages, phoneSizes, say, signIn, statusLine, test } from './fixtures';
import { call, ok, repoRoot, safePath, type World } from './world';

async function healthy(world: World): Promise<boolean> {
  try {
    return (await fetch(`${world.origin}/healthz`, { signal: AbortSignal.timeout(2000) })).ok;
  } catch {
    return false;
  }
}

// The pid of the server restart() started, so a later stop kills that one.
let restarted: number | undefined;

// restart starts conchd again as the walkthrough's last command does: the same
// binary, address, web client and data directory, in the run's isolated home.
// It keeps --test-relax-invite-limits, which global-setup.ts gave the first
// server: without it the specs after this one would get the production
// per-address limits back (issue #609).
function restart(world: World): void {
  const log = openSync(join(world.root, 'logs', 'conchd-restart.log'), 'a');
  const home = join(world.root, 'home');
  const server = spawn(
    join(world.root, 'bin', 'conchd'),
    ['serve', '--listen', new URL(world.origin).host, '--web-dir', join(repoRoot, 'web', 'dist'), '--data', join(world.root, 'data'), '--test-relax-invite-limits'],
    {
      cwd: world.root,
      env: { PATH: safePath(), HOME: home, TMPDIR: join(world.root, 'tmp'), XDG_CONFIG_HOME: join(home, 'config'), XDG_DATA_HOME: join(home, 'data'), XDG_STATE_HOME: join(home, 'state'), XDG_CACHE_HOME: join(home, 'cache') },
      stdio: ['ignore', log, log],
      detached: true,
    },
  );
  closeSync(log);
  // The reset at the end stops it with everything else under the root.
  server.unref();
  restarted = server.pid;
}

// done makes an operator's call that must succeed and whose reply is not
// needed.
async function done(world: World, token: string, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<void> {
  const reply = await call(world, token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new Error(`${method} ${path}: status ${String(reply.status)}`);
}

const paused = 'Sending is paused until the server is back. Your draft is kept.';

// Issue #592: a channel with nothing in it says so, until the first message.
// Before P3-09, on a channel and a person of its own made through the API, so
// the fixture people's lists are left as they are.
test('P592 an empty channel says so until its first message is sent', async ({ meet, secrets, world }) => {
  const channel = `p592-empty-${String(Date.now())}`;
  const op = world.tokens.operator;
  const person: CreatePrincipalRequest = { kind: 'human', name: channel };
  const id = (await ok(world, op, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
  const label: CreateCredentialRequestV1 = { label: channel };
  const { token } = await ok(world, op, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, label);
  secrets.push(token);
  await done(world, op, 'POST', '/v0/channels', { name: channel });
  await done(world, op, 'PUT', `/v1/channels/${encodeURIComponent(channel)}/members/${String(id)}`);

  const { page } = await meet();
  await signIn(page, world, token, channel);
  await channelButton(page, channel).click();
  const empty = messages(page).getByText(`No messages in #${channel} yet. Say hello.`);
  await expect(empty).toBeVisible();
  // Connected is not a pause.
  await expect(page.getByText(paused)).toHaveCount(0);

  const first = 'p592 the first message';
  await say(page, channel, first);
  await expect(messages(page).getByText(first)).toHaveCount(1);
  await expect(empty).toHaveCount(0);
});

test('P3-09 after conchd restarts the page shows Reconnecting and out of date, then recovers with the history once', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  const before = ['p3-09 seed one', 'p3-09 seed two'];
  for (const body of before) await post(world, world.tokens.nick, 'general', body);
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);
  const typed = 'p3-09 typed while the server is down';
  await page.getByLabel('Message #general').fill(typed);
  // Issue #585: the voice strip reads its presence from the server's socket,
  // and this server has no LiveKit.
  const voice = page.getByRole('region', { name: 'Voice' });
  const notSetUp = 'Voice is not set up on this server';
  const unreachable = 'Voice status unavailable: the server cannot be reached.';
  await expect(voice).toContainText(notSetUp);
  await expect(page.getByText(paused)).toHaveCount(0);
  const connectedColor = await page.getByLabel('Message #general').evaluate((el) => getComputedStyle(el).color);

  // The server stops.
  const pid = world.serverPid();
  process.kill(restarted ?? pid, 'SIGTERM');
  await expect.poll(() => healthy(world), { message: 'conchd has stopped', timeout: 30_000 }).toBe(false);

  await expect(statusLine(page)).toContainText('Reconnecting', { timeout: 30_000 });
  await expect(page.getByText('This conversation may be out of date')).toBeVisible();
  // What was there stays, and nothing can be sent into the gap: the composer is
  // closed and the line typed before is still in it.
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);
  await expect(page.getByLabel('Message #general')).toBeDisabled();
  await expect(page.getByLabel('Message #general')).toHaveValue(typed);
  // Issue #592: the composer says why, and looks closed.
  await expect(page.getByText(paused)).toBeVisible();
  await expect(page.getByLabel('Message #general')).toHaveAccessibleDescription(paused);
  expect(await page.getByLabel('Message #general').evaluate((el) => getComputedStyle(el).color)).not.toBe(connectedColor);
  // The strip says voice cannot be read, not what it last read, and offers no
  // join.
  await expect(voice).toContainText(unreachable);
  await expect(voice).not.toContainText(notSetUp);
  await expect(voice.getByRole('button', { name: /Join voice/ })).toBeDisabled();
  await expect(voice.getByRole('button', { disabled: false })).toHaveCount(0);

  // It comes back on the same data directory and address.
  restart(world);
  await expect.poll(() => healthy(world), { message: 'conchd is serving again', timeout: 30_000 }).toBe(true);

  await expect(statusLine(page)).toContainText('Connected', { timeout: 60_000 });
  // Within 5 s of Connected, without opening another channel, the strip has
  // read the server again rather than waiting out its own backoff.
  await expect(voice).toContainText(notSetUp, { timeout: 5000 });
  await expect(voice).not.toContainText(unreachable);
  await expect(page.getByText('This conversation may be out of date')).toHaveCount(0);
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  await expect(page.getByText(paused)).toHaveCount(0);
  await expect(page.getByLabel('Message #general')).toHaveAccessibleDescription('');
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);

  // Live again: a message after the restart arrives once, and the line that was
  // typed during the gap was never sent by itself.
  const after = 'p3-09 after the restart';
  await post(world, world.tokens.nick, 'general', after);
  await expect(messages(page).getByText(after)).toHaveCount(1);
  await expect(page.getByLabel('Message #general')).toHaveValue(typed);
  // The replay after the reconnect has landed by now (the new message came
  // through the same socket): the history is still once each.
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);
  const stored = await history(world, world.tokens.browser, 'general');
  for (const body of [...before, after]) expect(stored.filter((m) => m.body === body)).toHaveLength(1);
  expect(stored.filter((m) => m.body === typed)).toHaveLength(0);
  await say(page, 'general', typed);
  await expect(messages(page).getByText(typed)).toHaveCount(1);
});

// Issue #1030: on a phone the top bar is the only quick sign of an outage, and
// the conversation says it is out of date as on a desktop.
test('P1030 at 320×640 an outage shows Reconnecting whole and the stale banner, and Connected is whole again after the restart', async ({ meet, world }) => {
  const { page } = await meet(phoneSizes[0][1]);
  await signIn(page, world, world.tokens.browser, 'browser-test');
  await expectStateReadable(page, 'Connected');
  await expect(statusLine(page).locator('.status-who')).toBeVisible();

  process.kill(restarted ?? world.serverPid(), 'SIGTERM');
  await expect.poll(() => healthy(world), { message: 'conchd has stopped', timeout: 30_000 }).toBe(false);

  await expect(statusLine(page)).toContainText('Reconnecting', { timeout: 30_000 });
  await expectStateReadable(page, 'Reconnecting');
  await expect(statusLine(page).locator('.status-who')).toBeVisible();
  await expect(page.getByText('This conversation may be out of date')).toBeVisible();

  restart(world);
  await expect.poll(() => healthy(world), { message: 'conchd is serving again', timeout: 30_000 }).toBe(true);
  await expect(statusLine(page)).toContainText('Connected', { timeout: 60_000 });
  await expectStateReadable(page, 'Connected');
  await expect(page.getByText('This conversation may be out of date')).toHaveCount(0);
});
