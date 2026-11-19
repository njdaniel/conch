// Issue #1798: an agent posts an image under a manifest grant. The agent's
// manifest grants the capability files.upload and, in one channel, the
// permission attach; it posts a PNG with post_message's attachments argument,
// over the real /mcp endpoint, and a member of the channel sees the image
// inline, as one a person posts (25-attachments). Without attach the same call
// is refused and nothing is posted, and the agent can never fetch a file, its
// own included.
//
// The suite shares one server, so the agent, the member and the channel have
// names no other spec uses, and both principals are removed at the end.
import { createCredentialResponseV1, createPrincipalResponse } from '../src/schema/credential';
import { fileAttachmentPayloadSchema, fileAttachmentV1 } from '../src/schema/file';
import type { MessageV2 } from '../src/schema/message';
import { history } from './api';
import { expect, messages, openChannel, signIn, test } from './fixtures';
import { call, ok, type World } from './world';

const stamp = Date.now().toString(36).slice(-6);

// A 1×1 PNG: a whole image, because the page decodes what it previews.
const pngBase64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR4nGNgAAIAAAUAAXpeqz8AAAAASUVORK5CYII=';

interface ToolResult {
  // The schema.Error code of a refused call, or null for one that worked.
  readonly error: string | null;
  readonly message: MessageV2 | null;
}

// postMessage is one tools/call of post_message on /mcp as the agent. No other
// spec speaks MCP (the scripted agents go through conch-adapter), so this is
// the whole of the client: one JSON-RPC request and its answer.
async function postMessage(world: World, token: string, args: Record<string, unknown>): Promise<ToolResult> {
  const res = await fetch(`${world.origin}/mcp`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'post_message', arguments: args } }),
    signal: AbortSignal.timeout(10_000),
  });
  expect(res.status, 'the /mcp request').toBe(200);
  const reply = (await res.json()) as {
    result: { isError?: boolean; content: { text: string }[]; structuredContent?: { message?: MessageV2 } };
  };
  if (reply.result.isError === true) {
    return { error: (JSON.parse(reply.result.content[0]?.text ?? '{}') as { code?: string }).code ?? '', message: null };
  }
  return { error: null, message: reply.result.structuredContent?.message ?? null };
}

function fileIds(message: MessageV2): number[] {
  if (message.payload?.schema !== fileAttachmentPayloadSchema) throw new Error('message has no file attachment payload');
  const problems = fileAttachmentV1(message.payload.data);
  if (problems.length > 0) throw new Error(problems.join('; '));
  return (message.payload.data as { file_ids: number[] }).file_ids;
}

interface AuditLine {
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

test('A1798 an agent with the grant posts an image over MCP and a member sees it inline', async ({ meet, secrets, world }) => {
  const owner = world.tokens.operator;
  const must = (what: string, reply: { readonly status: number; readonly body: unknown }) => {
    expect(reply.status, `${what}: ${JSON.stringify(reply.body)}`).toBeLessThan(300);
    return reply.body;
  };
  const made = async (kind: 'agent' | 'human', name: string) => {
    const id = (await ok(world, owner, 'POST', '/v0/principals', createPrincipalResponse, { kind, name })).principal.id;
    const token = (await ok(world, owner, 'POST', `/v1/principals/${String(id)}/credentials`, createCredentialResponseV1, { label: name })).token;
    secrets.push(token);
    return { id, name, token };
  };
  const channel = `img-${stamp}`;
  const agent = await made('agent', `progress-bot-${stamp}`);
  const member = await made('human', `viewer-${stamp}`);
  try {
    const created = must('channel', await call(world, owner, 'POST', '/v1/channels', { name: channel, visibility: 'private' })) as { channel: { id: number } };
    must('the agent joins', await call(world, owner, 'PUT', `/v1/channels/${channel}/members/${String(agent.id)}`, { can_decide: false }));
    must('the member joins', await call(world, owner, 'PUT', `/v1/channels/${channel}/members/${String(member.id)}`));
    // The grant, and nothing more: the agent may post and attach here. It
    // cannot read the channel, and it cannot read a file anywhere.
    const manifest = (permissions: string[]) => ({
      display_name: agent.name,
      tier: 'C',
      capabilities: ['messages.post', 'files.upload'],
      channels: [{ channel_id: created.channel.id, permissions }],
    });
    must('the manifest', await call(world, owner, 'PUT', `/v1/principals/${String(agent.id)}/manifest`, manifest(['post', 'attach'])));

    const { page } = await meet();
    await signIn(page, world, member.token, member.name);
    await openChannel(page, channel);
    await expect(page.getByLabel(`Message #${channel}`)).toBeEnabled();

    const caption = `panther progress ${stamp}`;
    const sent = await postMessage(world, agent.token, {
      channel,
      body: caption,
      attachments: [{ name: 'panther.png', media_type: 'image/png', data_base64: pngBase64 }],
    });
    expect(sent.error, 'the granted post').toBeNull();
    if (sent.message === null) throw new Error('post_message returned no message');
    const ids = fileIds(sent.message);
    expect(ids).toHaveLength(1);
    expect(sent.message.author_id).toBe(agent.id);

    // The member sees the message arrive, with the image inline: a decoded
    // preview, not a file chip.
    await expect(messages(page).getByText(caption)).toHaveCount(1);
    const preview = page.getByRole('button', { name: 'Enlarge panther.png' });
    await expect(preview).toBeVisible();
    await expect.poll(() => preview.locator('img').evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth)).toBe(1);
    // And again from history, as any attachment.
    await page.reload();
    await expect(messages(page).getByText(caption)).toHaveCount(1);
    await expect(page.getByRole('button', { name: 'Enlarge panther.png' })).toBeVisible();

    // The upload is the agent's, in the audit trail.
    const uploaded = (await audit(world)).filter((l) => l.action === 'file_uploaded' && l.subject === `file:${String(ids[0])}`);
    expect(uploaded).toEqual([expect.objectContaining({ actor: `principal:${String(agent.id)}` })]);

    // The grant is for posting. The agent cannot fetch the file it uploaded.
    for (const path of [`/v2/files/${String(ids[0])}`, `/v2/files/${String(ids[0])}/meta`]) {
      expect((await call(world, agent.token, 'GET', path)).status, `the agent fetching ${path}`).toBe(403);
    }

    // Without attach in the channel the same call is refused, whole: no
    // message, with or without its image.
    must('the manifest without attach', await call(world, owner, 'PUT', `/v1/principals/${String(agent.id)}/manifest`, manifest(['post'])));
    const refusedCaption = `not granted ${stamp}`;
    const refused = await postMessage(world, agent.token, {
      channel,
      body: refusedCaption,
      attachments: [{ name: 'refused.png', media_type: 'image/png', data_base64: pngBase64 }],
    });
    expect(refused.error).toBe('forbidden');
    expect((await history(world, member.token, channel)).filter((m) => m.body === refusedCaption)).toEqual([]);
    // It may still post words.
    expect((await postMessage(world, agent.token, { channel, body: `words only ${stamp}` })).error).toBeNull();
    await expect(messages(page).getByText(`words only ${stamp}`)).toHaveCount(1);
    await expect(messages(page).getByText(refusedCaption)).toHaveCount(0);
  } finally {
    for (const id of [agent.id, member.id]) {
      const done = await call(world, owner, 'POST', `/v1/principals/${String(id)}/hide`);
      expect(done.status, `hiding principal ${String(id)}: ${JSON.stringify(done.body)}`).toBeLessThan(300);
    }
  }
});
