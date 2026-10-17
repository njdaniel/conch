// Issue #496: a person sees their own sign-ins in the lobby and ends one of
// them, on a phone's screen, and ending this browser's own is signing out.
// The person is made by the operator with two credentials (two devices), so
// the spec spends nothing of the per-address bucket that invites and
// sign-in links share, and joins no channel another spec reads.
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { expect, expectFits, phones, signIn, test } from './fixtures';
import { call, ok } from './world';

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
