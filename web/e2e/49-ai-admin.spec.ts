// Issue #1334 (AI administrators, #1326): the console's Add an AI administrator
// against the real server. The owner makes one from Settings -> Members; the
// seven steps run through the public routes and the entry is shown once; the
// grant is on the server and the credential in the entry is the agent's own;
// the agent's card shows the grant, and Remove access ends the credential.
//
// The suite shares one server, so the agent and the channel have names no other
// spec uses and the agent is removed from the server at the end.
import type { BrowserContextOptions, Page } from '@playwright/test';
import { getAgentManifestResponseV1 } from '../src/schema/admin';
import { whoAmIResponseV1 } from '../src/schema/principal';
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { credentialField, expect, expectFits, openChannel, statusLine, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36).slice(-6);

async function signedIn(page: Page, appUrl: string, token: string): Promise<void> {
  await page.goto(appUrl);
  await (await credentialField(page)).fill(token);
  await page.getByRole('button', { name: 'Use credential' }).click();
  await page.getByRole('button', { name: 'Continue anyway' }).click();
  await expect(statusLine(page)).toContainText('Connected');
}

test('A1334 at 1440×900 the owner adds an AI administrator, sees its entry once, and removes its access', async ({ meet, secrets, world }) => {
  const name = `ai-admin-${stamp}`;
  const channel = `aia-${stamp}`;
  let agentId: number | undefined;
  try {
    const { page } = await meet({ viewport: { width: 1440, height: 900 } });
    await signedIn(page, world.appUrl, world.tokens.operator);
    await page.getByRole('button', { name: 'Settings' }).click();
    const dialog = page.getByRole('dialog', { name: 'Settings' });
    const region = dialog.getByRole('region', { name: 'Members', exact: true });
    await expect(region.getByRole('list', { name: 'People' })).toBeVisible();

    await region.getByRole('button', { name: 'Add an AI administrator' }).click();
    const form = region.getByRole('form', { name: 'New AI administrator' });
    await form.getByLabel('Name').fill(name);
    await form.getByLabel('Approval channel').fill(channel);
    // The two powerful grants are off until chosen.
    await expect(form.getByRole('checkbox', { name: 'May change server settings' })).not.toBeChecked();
    await expect(form.getByRole('checkbox', { name: 'May manage members' })).toBeChecked();
    await expectFits(page, '1440×900: Add an AI administrator');
    await form.getByRole('button', { name: 'Add AI administrator' }).click();

    // The entry, once: hidden until Reveal.
    const entry = region.getByRole('group', { name: `MCP entry for ${name}` });
    await expect(entry).toBeVisible();
    await expect(entry.getByRole('textbox')).toHaveCount(0);
    await entry.getByRole('button', { name: 'Reveal' }).click();
    const text = await entry.getByRole('textbox', { name: `MCP entry for ${name}` }).inputValue();
    const parsed: unknown = JSON.parse(text);
    const header = ((parsed as { mcpServers: Record<string, { url: string; headers: { Authorization: string } }> }).mcpServers);
    const only = Object.values(header);
    expect(only).toHaveLength(1);
    const token = (only[0]?.headers.Authorization ?? '').replace(/^Bearer /, '');
    expect(token).toMatch(/^conch_/);
    expect(only[0]?.url).toBe(`${world.origin}/mcp`);
    secrets.push(token);
    await entry.getByRole('button', { name: 'Done' }).click();
    await expect(entry).toHaveCount(0);
    await expect(page.locator('body')).not.toContainText(token);

    // On the server: the agent holds the grant, in the channel it watches; the
    // entry's credential is the agent's own.
    const me = await ok(world, token, 'GET', '/v1/whoami', whoAmIResponseV1);
    expect(me.kind).toBe('agent');
    expect(me.name).toBe(name);
    agentId = me.id;
    const manifest = (await ok(world, world.tokens.operator, 'GET', `/v1/principals/${String(agentId)}/manifest`, getAgentManifestResponseV1)).manifest;
    expect(manifest.permissions).toEqual(['manage_members', 'manage_channels', 'make_member_links']);
    expect(manifest.capabilities).toContain('admin.act');
    expect(manifest.admin?.approval_channel_id).toBe(manifest.channels?.[0]?.channel_id);

    // The agent's card shows the grant; Remove access asks, then ends it.
    const card = region.getByRole('group', { name: `AI administrator ${name}` });
    await expect(card).toContainText('AI administrator: manage members, manage channels, make member links');
    await card.getByRole('button', { name: 'Remove access' }).click();
    await card.getByRole('button', { name: 'Yes, remove access' }).click();
    await expect(card).toHaveCount(0);
    const after = await call(world, token, 'GET', '/v1/whoami');
    expect(after.status, 'the removed agent’s credential').toBe(401);
  } finally {
    if (agentId !== undefined) {
      const done = await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(agentId)}/hide`);
      expect(done.status, `hiding the agent: ${JSON.stringify(done.body)}`).toBeLessThan(300);
    }
  }
});

// ------------------------------------------------- the approval chain (approval-path)

const phone: BrowserContextOptions = { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true };

interface Rig {
  readonly agentId: number;
  readonly agentToken: string;
  readonly channel: string;
  readonly memberId: number;
  readonly memberToken: string;
}

const must = (what: string, reply: { readonly status: number; readonly body: unknown }) => {
  expect(reply.status, `${what}: ${JSON.stringify(reply.body)}`).toBeLessThan(300);
  return reply.body;
};

// rig makes, as the owner, an AI administrator that holds manage_members with
// a private approval channel the owner decides in, and a member to act on: the
// same routes the console's Add an AI administrator composes.
async function rig(world: World, secrets: string[], tag: string): Promise<Rig> {
  const owner = world.tokens.operator;
  const made = async (kind: 'agent' | 'human', name: string) => (await ok(world, owner, 'POST', '/v0/principals', createPrincipalResponse, { kind, name })).principal.id;
  const token = async (id: number, label: string) => {
    const t = (await ok(world, owner, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, { label })).token;
    secrets.push(t);
    return t;
  };
  const me = (await call(world, owner, 'GET', '/v1/whoami')).body as { id: number };
  const channel = `aiq-${stamp}-${tag}`;
  const agentId = await made('agent', `ai-${stamp}-${tag}`);
  const memberId = await made('human', `victim-${stamp}-${tag}`);
  const created = must('channel', await call(world, owner, 'POST', '/v1/channels', { name: channel, visibility: 'private' })) as { channel: { id: number } };
  must('owner decides', await call(world, owner, 'PUT', `/v1/channels/${channel}/members/${String(me.id)}`, { can_decide: true }));
  must('agent watches', await call(world, owner, 'PUT', `/v1/channels/${channel}/members/${String(agentId)}`, { can_decide: false }));
  must(
    'manifest',
    await call(world, owner, 'PUT', `/v1/principals/${String(agentId)}/manifest`, {
      display_name: `ai-${stamp}-${tag}`,
      tier: 'A',
      capabilities: ['messages.read', 'messages.post', 'approvals.await', 'approvals.check', 'admin.act'],
      channels: [{ channel_id: created.channel.id, permissions: ['read', 'post'] }],
      rate_limits: [{ capability: 'admin.act', max: 60, window_seconds: 3600 }],
      permissions: ['manage_members'],
      admin: { approval_channel_id: created.channel.id },
    }),
  );
  return { agentId, agentToken: await token(agentId, 'ai-admin'), channel, memberId, memberToken: await token(memberId, 'victim') };
}

const reason = 'the member asked to leave <b>#general</b>';

// raise has the agent submit a gated action through the REST door: 201 with an approval.
async function raise(world: World, r: Rig): Promise<number> {
  const sent = await call(world, r.agentToken, 'POST', '/v1/admin/actions', { action: 'disable_member', target: { kind: 'principal', id: r.memberId }, reason });
  expect(sent.status, JSON.stringify(sent.body)).toBe(201);
  const result = sent.body as { status: string; approval_id: number };
  expect(result.status).toBe('pending_approval');
  return result.approval_id;
}

interface AuditLine {
  readonly id: number;
  readonly actor: string;
  readonly action: string;
  readonly subject: string;
}
async function audit(world: World): Promise<AuditLine[]> {
  const reply = await call(world, world.tokens.operator, 'GET', '/v1/audit');
  expect(reply.status).toBe(200);
  const text = typeof reply.body === 'string' ? reply.body : JSON.stringify(reply.body);
  return text
    .split('\n')
    .filter((l) => l !== '')
    .map((l) => JSON.parse(l) as AuditLine);
}

async function approvalState(world: World, id: number): Promise<{ state: string; outcome?: string }> {
  const body = (await call(world, world.tokens.operator, 'GET', `/v1/approvals/${String(id)}`)).body as { approval: { state: string }; resolution?: { outcome: string } };
  return { state: body.approval.state, ...(body.resolution === undefined ? {} : { outcome: body.resolution.outcome }) };
}

async function retire(world: World, r: Rig): Promise<void> {
  for (const id of [r.agentId, r.memberId]) await call(world, world.tokens.operator, 'POST', `/v1/principals/${String(id)}/hide`);
}

test('A1334 the browser decision, end to end: request, notify, resolve, run, audit', async ({ meet, secrets, world }) => {
  const r = await rig(world, secrets, 'y');
  try {
    const { page } = await meet({ viewport: { width: 1440, height: 900 } });
    await signedIn(page, world.appUrl, world.tokens.operator);
    await openChannel(page, r.channel);

    // 1 Request: the agent raises a gated action; nothing has run.
    const approval = await raise(world, r);
    // 2 Notify: the owner's page shows the card, with the facts and the reason
    // as the agent's words (the e2e server has no ntfy capture).
    const card = page.getByRole('article', { name: 'AI administrator request' });
    if ((await card.count()) === 0) await page.reload();
    await expect(card).toBeVisible();
    await expect(card).toContainText('disable_member');
    await expect(card).toContainText(`POST /v1/principals/${String(r.memberId)}/disable`);
    await expect(card).toContainText(`principal #${String(r.memberId)}`);
    await expect(card.getByText("The agent's own words")).toBeVisible();
    await expect(card).toContainText(reason);
    expect(await card.locator('b').count()).toBe(0);
    expect((await approvalState(world, approval)).state).toBe('pending');
    expect((await call(world, r.memberToken, 'GET', '/v1/whoami')).status, 'not run before it is decided').toBe(200);

    // 3 Resolve: the owner clicks Run it.
    await card.getByRole('button', { name: 'Run it' }).click();
    await expect(card.getByText('You chose: Run it.')).toBeVisible();
    await expect(card.getByRole('button')).toHaveCount(0);
    expect(await approvalState(world, approval)).toEqual({ state: 'resolved', outcome: 'approved' });
    expect((await call(world, r.memberToken, 'GET', '/v1/whoami')).status, 'the decision alone runs nothing').toBe(200);

    // 4 Run: the agent claims the one run, and the member is disabled.
    const ran = await call(world, r.agentToken, 'POST', `/v1/admin/actions/${String(approval)}/run`);
    expect(ran.status, JSON.stringify(ran.body)).toBe(200);
    expect(ran.body).toMatchObject({ status: 'ran', http_status: 200 });
    expect((await call(world, r.memberToken, 'GET', '/v1/whoami')).status).toBe(401);
    const again = await call(world, r.agentToken, 'POST', `/v1/admin/actions/${String(approval)}/run`);
    expect(again.status, 'a second run').toBeGreaterThanOrEqual(400);

    // 5 Audit, in order.
    const lines = await audit(world);
    const subject = `approval:${String(approval)}`;
    const agent = `principal:${String(r.agentId)}`;
    const wanted: readonly (readonly [string, string, string | null])[] = [
      ['approval_created', subject, agent],
      ['admin_action_requested', subject, agent],
      ['decision_cast', subject, null],
      ['approval_resolved', subject, null],
      ['principal_disabled', `principal:${String(r.memberId)}`, null],
      ['admin_action_ran', subject, agent],
    ];
    let from = -1;
    for (const [action, on, actor] of wanted) {
      const at = lines.findIndex((l, i) => i > from && l.action === action && l.subject === on);
      expect(at, `${action} on ${on} after the one before it`).toBeGreaterThan(from);
      if (actor !== null) expect(lines[at]?.actor, `${action}'s actor`).toBe(actor);
      from = at;
    }
    const me = (await call(world, world.tokens.operator, 'GET', '/v1/whoami')).body as { id: number };
    expect(lines.find((l) => l.action === 'decision_cast' && l.subject === subject)?.actor).toBe(`principal:${String(me.id)}`);
  } finally {
    await retire(world, r);
  }
});

test('A1334 Do not run resolves rejected and the run is refused', async ({ meet, secrets, world }) => {
  const r = await rig(world, secrets, 'n');
  try {
    const { page } = await meet({ viewport: { width: 1440, height: 900 } });
    await signedIn(page, world.appUrl, world.tokens.operator);
    await openChannel(page, r.channel);
    const approval = await raise(world, r);
    const card = page.getByRole('article', { name: 'AI administrator request' });
    if ((await card.count()) === 0) await page.reload();
    await card.getByRole('button', { name: 'Do not run' }).click();
    await expect(card.getByText('You chose: Do not run.')).toBeVisible();
    expect(await approvalState(world, approval)).toEqual({ state: 'resolved', outcome: 'rejected' });
    const ran = await call(world, r.agentToken, 'POST', `/v1/admin/actions/${String(approval)}/run`);
    expect(ran.status).toBe(409);
    expect(ran.body).toMatchObject({ code: 'approval_not_approved' });
    expect((await call(world, r.memberToken, 'GET', '/v1/whoami')).status, 'nothing ran').toBe(200);
  } finally {
    await retire(world, r);
  }
});

test('A1334 at 390×844 the form, the agent card and the approval card fit the phone', async ({ meet, secrets, world }) => {
  const r = await rig(world, secrets, 'p');
  try {
    const { page } = await meet(phone);
    await signedIn(page, world.appUrl, world.tokens.operator);
    await openChannel(page, r.channel);
    await raise(world, r);
    const card = page.getByRole('article', { name: 'AI administrator request' });
    if ((await card.count()) === 0) await page.reload();
    await expect(card).toBeVisible();
    await expectFits(page, '390×844: the approval card');
    for (const button of await card.getByRole('button').all()) expect((await button.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(43.5);
    await page.screenshot({ path: 'test-results/49-phone-approval-card.png' });

    await page.getByRole('button', { name: 'Settings' }).tap();
    const dialog = page.getByRole('dialog', { name: 'Settings' });
    await dialog.getByRole('button', { name: 'Members', exact: true }).tap();
    const region = dialog.getByRole('region', { name: 'Members', exact: true });
    const agentCard = region.getByRole('group', { name: `AI administrator ai-${stamp}-p` });
    await agentCard.evaluate((el) => {
      el.scrollIntoView({ block: 'center' });
    });
    await expect(agentCard).toBeVisible();
    await expectFits(page, '390×844: the agent card');
    await agentCard.getByRole('button', { name: 'Remove access' }).tap();
    await expectFits(page, '390×844: Remove access asked');
    for (const button of await agentCard.getByRole('button').all()) expect((await button.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(43.5);
    await agentCard.getByRole('button', { name: 'Cancel' }).tap();

    await region.getByRole('button', { name: 'Add an AI administrator' }).tap();
    const form = region.getByRole('form', { name: 'New AI administrator' });
    await expect(form).toBeVisible();
    await expectFits(page, '390×844: Add an AI administrator');
    await page.screenshot({ path: 'test-results/49-phone-add-form.png' });
    for (const control of await form.locator('input[type=text], input[type=number], select, button').all()) {
      expect((await control.boundingBox())?.height ?? 0, 'finger-sized controls').toBeGreaterThanOrEqual(43.5);
    }
  } finally {
    await retire(world, r);
  }
});
