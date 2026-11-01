// Issue #1029 (browser UX audit 3, F4): problem reports for operators, against
// the real server. A member at 390×844 sends a report from Settings → Report
// a problem and is told its number; their Settings has no Reports item. The
// owner at 1440×900 finds it in Settings → Reports, reads what happened as
// text, and marks it filed with an https address; GET /v1/reports/{id} then
// answers state "filed" with that address.
//
// The report is found by the words the member typed, which no other spec
// uses: the specs share one server, and its list holds their reports too.
import { createCredentialResponseV1, createPrincipalResponse, type CreateCredentialRequestV1, type CreatePrincipalRequest } from '../src/schema/credential';
import { problemReportV1 } from '../src/schema/report';
import { expect, expectFits, phones, signIn, statusLine, test } from './fixtures';
import { ok } from './world';

const stamp = Date.now().toString(36);

test('R1029 a member sends a report and sees no Reports; the owner reads it and marks it filed', async ({ meet, secrets, world }) => {
  const name = `member-1-${stamp}`;
  const request: CreatePrincipalRequest = { kind: 'human', name };
  const id = (await ok(world, world.tokens.operator, 'POST', '/v0/principals', createPrincipalResponse, request)).principal.id;
  const credential: CreateCredentialRequestV1 = { label: name };
  const { token } = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, credential);
  secrets.push(token);

  const happened = `r1029 nothing arrived <b>${stamp}</b>`;
  const member = await meet(phones[0][1]);
  await signIn(member.page, world, token, name);
  await member.page.getByRole('button', { name: 'Settings' }).tap();
  // A member's Settings has no Reports item.
  await expect(member.page.getByRole('button', { name: 'Report a problem' })).toBeVisible();
  await expect(member.page.getByRole('button', { name: 'Reports', exact: true })).toHaveCount(0);
  await member.page.getByRole('button', { name: 'Report a problem' }).tap();
  const form = member.page.getByRole('form', { name: 'Report a problem' });
  await form.getByLabel('What you did').fill('Sent a message');
  await form.getByLabel('What you expected').fill('It to arrive');
  await form.getByLabel('What happened instead').fill(happened);
  await form.getByLabel('How often does it happen?').selectOption('once');
  await form.getByRole('button', { name: 'Review report' }).tap();
  await member.page.getByRole('button', { name: 'Send report' }).tap();
  const sent = member.page.getByText(/Your report was sent as number \d+/);
  await expect(sent).toBeVisible();
  const number = Number(/number (\d+)/.exec((await sent.textContent()) ?? '')?.[1]);
  expect(number).toBeGreaterThan(0);
  await expectFits(member.page, 'Settings with a report sent');

  const owner = await meet({ viewport: { width: 1440, height: 900 } });
  await owner.page.goto(world.appUrl);
  await owner.page.getByLabel('Credential').fill(world.tokens.operator);
  await owner.page.getByRole('button', { name: 'Use credential' }).click();
  await owner.page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(owner.page)).toContainText('Connected');
  await owner.page.getByRole('button', { name: 'Settings' }).click();
  await owner.page.getByRole('button', { name: 'Reports', exact: true }).click();
  const region = owner.page.getByRole('region', { name: 'Reports' });
  const item = region.getByRole('listitem').filter({ has: owner.page.getByRole('button', { name: `Report #${String(number)}`, exact: true }) });
  await expect(item).toContainText('Open');
  await expect(item).toContainText('conch-web');
  await expect(item).toContainText('Once so far');
  await item.getByRole('button', { name: `Report #${String(number)}`, exact: true }).click();
  const view = region.getByRole('group', { name: `Report #${String(number)}` });
  // The words are shown as typed, tags included.
  await expect(view).toContainText(happened);
  await expect(view.locator('b')).toHaveCount(0);
  await view.getByLabel('Issue address').fill('not an address');
  await view.getByRole('button', { name: 'Mark filed' }).click();
  await expect(view).toContainText('The issue address must be an http or https address.');

  const issue = `https://example.test/conch/issues/${stamp}`;
  await view.getByLabel('Issue address').fill(issue);
  await view.getByRole('button', { name: 'Mark filed' }).click();
  await expect(view.getByRole('link', { name: issue })).toBeVisible();

  const stored = await ok(world, world.tokens.operator, 'GET', `/v1/reports/${String(number)}`, problemReportV1);
  expect(stored.state).toBe('filed');
  expect(stored.issue).toBe(issue);
});
