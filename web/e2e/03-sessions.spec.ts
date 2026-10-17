// P3-04: one person in two clients has two sessions, and revoking one
// credential ends only what that credential holds.
import { heartbeat, openSession, roster } from './api';
import { expect, signIn, test } from './fixtures';
import { call } from './world';

test('P3-04 revoking the credential the browser signed in with signs it out and leaves the terminal session', async ({ meet, world }) => {
  // The terminal: a session of nick-test on the first credential, as the TUI
  // opens one. Heartbeats keep it online while the test runs.
  const terminal = await openSession(world, world.tokens.nick, 'tui');
  const beat = setInterval(() => {
    void heartbeat(world, world.tokens.nick, terminal.id);
  }, 1000);
  try {
    // The browser: nick-test again, on the second credential.
    const { page } = await meet();
    await signIn(page, world, world.tokens.nick2, 'nick-test');
    await expect
      .poll(async () => (await roster(world, world.tokens.guest, 'general')).filter((s) => s.name === 'nick-test' && s.state === 'online').length, {
        message: 'two online sessions of nick-test: the terminal and the browser',
      })
      .toBe(2);
    const browserSession = (await roster(world, world.tokens.guest, 'general')).find((s) => s.name === 'nick-test' && s.id !== terminal.id);
    expect(browserSession?.client).not.toBe('tui');

    // The operator revokes the second credential only.
    const revoked = await call(world, world.tokens.operator, 'DELETE', `/v1/credentials/${String(world.nick2CredentialId)}`);
    expect(revoked.status).toBeLessThan(300);

    // The browser is back at sign-in, told why.
    await expect(page.getByRole('alert')).toHaveText('Signed out: the credential is no longer valid', { timeout: 45_000 });
    await expect(page.getByLabel('Credential')).toBeVisible();
    await expect(page.getByRole('button', { name: 'Sign in' })).toBeVisible();

    // The terminal's session is still there and online, the browser's is not.
    await expect
      .poll(async () => (await roster(world, world.tokens.nick, 'general')).filter((s) => s.name === 'nick-test' && s.state === 'online').map((s) => s.id), {
        message: 'only the terminal session of nick-test is online',
        timeout: 30_000,
      })
      .toEqual([terminal.id]);
    // The first credential still works, and the second no longer does.
    expect((await call(world, world.tokens.nick, 'GET', '/v1/whoami')).status).toBe(200);
    expect((await call(world, world.tokens.nick2, 'GET', '/v1/whoami')).status).toBe(401);
  } finally {
    clearInterval(beat);
    await call(world, world.tokens.nick, 'DELETE', `/v1/sessions/${String(terminal.id)}`);
  }
});
