// P3-09: the server goes away and comes back on the same data and address. The
// page says it is out of date while the server is down, retries by itself, and
// shows the history once when it is back. Last, because it restarts the server
// the other specs share.
import { join } from 'node:path';
import { closeSync, openSync } from 'node:fs';
import { spawn } from 'node:child_process';
import { history, post } from './api';
import { expect, messages, say, signIn, statusLine, test } from './fixtures';
import { repoRoot, safePath, type World } from './world';

async function healthy(world: World): Promise<boolean> {
  try {
    return (await fetch(`${world.origin}/healthz`, { signal: AbortSignal.timeout(2000) })).ok;
  } catch {
    return false;
  }
}

// restart starts conchd again as the walkthrough's last command does: the same
// binary, address, web client and data directory, in the run's isolated home.
function restart(world: World): void {
  const log = openSync(join(world.root, 'logs', 'conchd-restart.log'), 'a');
  const home = join(world.root, 'home');
  const server = spawn(
    join(world.root, 'bin', 'conchd'),
    ['serve', '--listen', new URL(world.origin).host, '--web-dir', join(repoRoot, 'web', 'dist'), '--data', join(world.root, 'data')],
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
}

test('P3-09 after conchd restarts the page shows Reconnecting and out of date, then recovers with the history once', async ({ meet, world }) => {
  const { page } = await meet();
  await signIn(page, world, world.tokens.browser, 'browser-test');

  const before = ['p3-09 seed one', 'p3-09 seed two'];
  for (const body of before) await post(world, world.tokens.nick, 'general', body);
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);
  const typed = 'p3-09 typed while the server is down';
  await page.getByLabel('Message #general').fill(typed);

  // The server stops.
  const pid = world.serverPid();
  process.kill(pid, 'SIGTERM');
  await expect.poll(() => healthy(world), { message: 'conchd has stopped', timeout: 30_000 }).toBe(false);

  await expect(statusLine(page)).toContainText('Reconnecting', { timeout: 30_000 });
  await expect(page.getByText('This conversation may be out of date')).toBeVisible();
  // What was there stays, and nothing can be sent into the gap: the composer is
  // closed and the line typed before is still in it.
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);
  await expect(page.getByLabel('Message #general')).toBeDisabled();
  await expect(page.getByLabel('Message #general')).toHaveValue(typed);

  // It comes back on the same data directory and address.
  restart(world);
  await expect.poll(() => healthy(world), { message: 'conchd is serving again', timeout: 30_000 }).toBe(true);

  await expect(statusLine(page)).toContainText('Connected', { timeout: 60_000 });
  await expect(page.getByText('This conversation may be out of date')).toHaveCount(0);
  await expect(page.getByLabel('Message #general')).toBeEnabled();
  for (const body of before) await expect(messages(page).getByText(body)).toHaveCount(1);

  // Live again: a message after the restart arrives once, and the line that was
  // typed during the gap was never sent by itself.
  const after = 'p3-09 after the restart';
  await post(world, world.tokens.nick, 'general', after);
  await expect(messages(page).getByText(after)).toHaveCount(1);
  await expect(page.getByLabel('Message #general')).toHaveValue(typed);
  const stored = await history(world, world.tokens.browser, 'general');
  for (const body of [...before, after]) expect(stored.filter((m) => m.body === body)).toHaveLength(1);
  expect(stored.filter((m) => m.body === typed)).toHaveLength(0);
  await say(page, 'general', typed);
  await expect(messages(page).getByText(typed)).toHaveCount(1);
});
