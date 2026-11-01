// Issue #496: a person sees their own sign-ins in the lobby and ends one of
// them, on a phone's screen, and ending this browser's own is signing out.
// The person is made by the operator with two credentials (two devices), so
// the spec spends nothing of the per-address bucket that invites and
// sign-in links share, and joins no channel another spec reads.
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { makeInvite } from './api';
import { accountPassword, consoleErrors, expect, expectFits, fillAccount, phones, signIn, statusLine, test } from './fixtures';
import { call, inviteUrl, ok, type World } from './world';

for (const [size, options] of phones) {
  test(`S496 at ${size} a person lists their sign-ins and ends another, then this one`, async ({ meet, secrets, world }) => {
    const person: CreatePrincipalRequest = { kind: 'human', name: `s496-${size.slice(0, 3)}-${String(Date.now())}` };
    const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, person)).principal.id;
    const issue = async (label: string): Promise<string> => {
      const request: CreateCredentialRequestV1 = { label };
      const made = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, request);
      secrets.push(made.token);
      return made.token;
    };
    const phoneToken = await issue('phone');
    const laptopToken = await issue('laptop');

    const { page } = await meet(options);
    await signIn(page, world, phoneToken, person.name);
    // In Settings (issue #536).
    await page.getByRole('button', { name: 'Settings' }).tap();
    await page.getByRole('button', { name: 'Your sign-ins' }).tap();
    const region = page.getByRole('region', { name: 'Your sign-ins' });
    const items = region.getByRole('list', { name: 'Sign-ins' }).getByRole('listitem');
    await expect(items).toHaveCount(2);
    const here = items.filter({ hasText: 'phone' });
    const laptop = items.filter({ hasText: 'laptop' });
    await expect(here).toContainText('This browser');
    await expect(laptop).not.toContainText('This browser');
    await expectFits(page, 'the lobby with your sign-ins open');

    // Another sign-in ends after a yes, and its holder is refused at once.
    await laptop.getByRole('button', { name: 'End this sign-in' }).tap();
    await expect(laptop.getByRole('group')).toContainText('End the sign-in “laptop”?');
    await expectFits(page, 'the question before ending a sign-in');
    expect((await call(world, laptopToken, 'GET', '/v1/whoami')).status).toBe(200);
    await laptop.getByRole('button', { name: 'Yes, end it' }).tap();
    await expect(region).toContainText('Ended the sign-in “laptop”.');
    await expect(items).toHaveCount(1);
    expect((await call(world, laptopToken, 'GET', '/v1/whoami')).status).toBe(401);
    expect((await call(world, phoneToken, 'GET', '/v1/whoami')).status).toBe(200);

    // This browser's own: the sign-out's question, then signed out.
    await here.getByRole('button', { name: 'End this sign-in' }).tap();
    const asked = page.getByRole('group', { name: /^Sign out of this browser\?/ });
    await expect(asked).toContainText('you will need a new link from an operator');
    await asked.getByRole('button', { name: 'Yes, sign out', exact: true }).tap();
    await expect(page.getByLabel('Credential')).toBeVisible();
    expect((await call(world, phoneToken, 'GET', '/v1/whoami')).status).toBe(401);
  });
}

// G7a (issue #714, design §6.2): Add a device. A member who joined with a
// password makes a link for their own account from Your sign-ins: the
// server asks for a recent proof of the password first, which the page asks
// for over the form; the link is hidden until Reveal. Another browser, where
// someone else is signed in, opens it: it says whose account it signs into,
// and the other person is signed out first, on their own button. The new
// sign-in is listed on the first device, which ends it.
async function person(world: World, secrets: string[], name: string): Promise<string> {
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  const label: CreateCredentialRequestV1 = { label: 'desk' };
  const made = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, label);
  secrets.push(made.token);
  return made.token;
}

test('G7a a member adds a device with a link revealed once, the other browser switches account on a yes, and the new sign-in is ended', async ({ meet, secrets, world }) => {
  secrets.push(accountPassword);
  const invite = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const name = `g7a-dev-${String(Date.now())}`;
  const laptop = await meet({ viewport: { width: 1280, height: 800 } });
  await laptop.page.goto(inviteUrl(world, invite.code));
  await fillAccount(laptop.page, name);
  await expect(statusLine(laptop.page)).toContainText('Connected');
  const laptopToken = await laptop.page.evaluate(() => window.localStorage.getItem('conch.web.token'));
  secrets.push(laptopToken ?? '');

  await laptop.page.getByRole('button', { name: 'Settings' }).click();
  await laptop.page.getByRole('button', { name: 'Your sign-ins' }).click();
  const add = laptop.page.getByRole('region', { name: 'Add a device' });
  await add.getByLabel('Name for the device (optional)').fill('g7a phone');
  await add.getByRole('button', { name: 'Make a device link' }).click();
  const dialog = laptop.page.getByRole('dialog', { name: 'Confirm it is you' });
  await expect(dialog).toBeVisible();
  await expect(add.getByLabel('Name for the device (optional)')).toHaveValue('g7a phone');
  await dialog.getByLabel('Your password').fill(accountPassword);
  await dialog.getByRole('button', { name: 'Confirm' }).click();
  const made = add.getByRole('group', { name: 'New device link' });
  await expect(made).toBeVisible();
  await expect(made.getByLabel('Device link')).toHaveCount(0);
  await expect(made).toContainText('it cannot know about another tab, another app');
  await made.getByRole('button', { name: 'Reveal link' }).click();
  const url = await made.getByLabel('Device link').inputValue();
  expect(url.startsWith(`${world.appUrl}#/signin/`)).toBe(true);
  await made.getByRole('button', { name: 'Done' }).click();
  await expect(made).toHaveCount(0);
  await expect(add.getByRole('list', { name: 'Open device links' })).toContainText('g7a phone');

  // Another browser, where someone else is signed in.
  const otherName = `g7a-other-${String(Date.now())}`;
  const otherToken = await person(world, secrets, otherName);
  const phone = await meet(phones[0][1]);
  await signIn(phone.page, world, otherToken, otherName);
  await phone.page.goto(url);
  await expect(phone.page.getByText(/This link adds this device to your account/)).toContainText(name);
  await expect(phone.page.getByText(/You are signed in here as/)).toContainText(`and this link is for ${name}`);
  await expectFits(phone.page, 'a device link opened over another sign-in');
  // Nothing is used until the person says so.
  await expect(phone.page.getByRole('button', { name: `Sign in as ${name}` })).toHaveCount(0);
  await phone.page.getByRole('button', { name: `Sign ${otherName} out` }).tap();
  await expect(phone.page.getByRole('button', { name: `Sign in as ${name}` })).toBeVisible();
  expect((await call(world, otherToken, 'GET', '/v1/whoami')).status).toBe(401);
  await phone.page.getByRole('button', { name: `Sign in as ${name}` }).tap();
  await expect(statusLine(phone.page)).toContainText(name);
  await expect(statusLine(phone.page)).toContainText('Connected');
  const phoneToken = (await phone.page.evaluate(() => window.localStorage.getItem('conch.web.token'))) ?? '';
  secrets.push(phoneToken);
  expect((await call(world, phoneToken, 'GET', '/v1/whoami')).status).toBe(200);

  // The first device sees the new sign-in, and ends it.
  const toggle = laptop.page.getByRole('button', { name: 'Your sign-ins' });
  await toggle.click();
  await toggle.click();
  const items = laptop.page.getByRole('region', { name: 'Your sign-ins' }).getByRole('list', { name: 'Sign-ins' }).getByRole('listitem');
  const added = items.filter({ hasText: 'g7a phone' });
  await expect(added).toHaveCount(1);
  await added.getByRole('button', { name: 'End this sign-in' }).click();
  await added.getByRole('button', { name: 'Yes, end it' }).click();
  await expect(items.filter({ hasText: 'g7a phone' })).toHaveCount(0);
  expect((await call(world, phoneToken, 'GET', '/v1/whoami')).status).toBe(401);
  // The link was used: none is open.
  await expect(add.getByText('None open.')).toBeVisible();
});

// Issue #1084 (browser UX audit 4, F7): a password change that leaves the page
// signed in ends the sign-in it was made from, and the page goes on with the
// new credential. The live socket, the voice poll and every whoami stop using
// the old one before the change is sent, so no request that is known to fail
// is made, and the console logs no 401 and no refused socket. Every request
// after the change carries the new credential.
test('F7 a password change that stays signed in logs no console error and sends nothing with the old credential after it', async ({ meet, secrets, world }) => {
  const first = accountPassword;
  const second = `${accountPassword} again`;
  secrets.push(first, second);
  const invite = await makeInvite(world, world.tokens.operator, { kind: 'personal', channels: ['general'], voice: false });
  const name = `f7-${String(Date.now())}`;
  const { page } = await meet({ viewport: { width: 1440, height: 900 } });
  const errors = consoleErrors(page);
  await page.goto(inviteUrl(world, invite.code));
  await fillAccount(page, name, first);
  await expect(statusLine(page)).toContainText('Connected');
  const oldToken = (await page.evaluate(() => window.localStorage.getItem('conch.web.token'))) ?? '';
  secrets.push(oldToken);
  expect(oldToken).not.toBe('');

  // Every request to the server from here, with the credential it carried.
  const sent: { readonly line: string; readonly bearer: string }[] = [];
  // The events socket opened from here on: the page takes up a new session.
  const opened: string[] = [];
  page.on('websocket', (ws) => {
    const path = new URL(ws.url()).pathname;
    ws.on('framereceived', () => opened.push(path));
  });
  page.on('request', (req) => {
    const url = new URL(req.url());
    if (!url.pathname.startsWith('/v1/')) return;
    sent.push({ line: `${req.method()} ${url.pathname}`, bearer: req.headers()['authorization'] ?? '' });
  });

  await page.getByRole('button', { name: 'Settings' }).click();
  await page.getByRole('button', { name: 'Password', exact: true }).click();
  const region = page.getByRole('region', { name: 'Password' });
  await region.getByLabel('Current password').fill(first);
  await region.getByLabel('New password', { exact: true }).fill(second);
  await region.getByLabel('Type it again').fill(second);
  const changed = page.waitForResponse((r) => r.url().endsWith('/v1/password/mine') && r.request().method() === 'PUT');
  await region.getByRole('button', { name: 'Change password' }).click();
  expect((await changed).status()).toBe(200);
  await expect(region.getByText('Your password is changed. Every other sign-in of yours has ended; this one goes on.')).toBeVisible();
  const newToken = (await page.evaluate(() => window.localStorage.getItem('conch.web.token'))) ?? '';
  secrets.push(newToken);
  expect(newToken).not.toBe(oldToken);
  // Back on the live loop with the new credential.
  await expect(statusLine(page)).toContainText('Connected');
  await page.waitForTimeout(3_000);

  // After the change's own request, nothing carries the old credential.
  const change = sent.findIndex((s) => s.line === 'PUT /v1/password/mine');
  expect(change).toBeGreaterThanOrEqual(0);
  const afterWithOld = sent.slice(change + 1).filter((s) => s.bearer === `Bearer ${oldToken}`);
  expect(afterWithOld.map((s) => s.line), 'requests with the old credential after the change').toEqual([]);
  expect(opened, 'frames of the events socket after the change').toContain('/v1/events/ws');
  expect(errors).toEqual([]);
});
