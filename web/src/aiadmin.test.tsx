// AI administrators in the browser (issue #1334, docs/design/ai-administrators.md
// §4.2): the owner's Add an AI administrator, the seven steps and their
// rollback, the entry shown once; the agent card with its grant and Remove
// access; and the card a conch.admin_action.v1 approval is drawn as, with its
// two buttons. Against a fake conchd (fetch) and a fake WebSocket. No network,
// no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { directoryCoalesce } from './useDirectoryChanged';
import { entryName, entryText, manifestFor } from './admin/AddAIAdmin';
import type { Permission } from './schema/permission';
import type { ServerMemberV1, WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const agentToken = 'conch_tok_AGENTTOKENAGENTTOKENAGENTTOKENAGENTTOKEN';

const catalog: Permission[] = [
  'create_public_channels',
  'create_private_channels',
  'make_member_links',
  'see_member_directory',
  'use_voice',
  'share_screen',
  'manage_members',
  'act_as_members',
  'manage_channels',
  'change_server_settings',
  'read_audit_log',
];
const auth = { scope: 'full', password_set: true, totp_enrolled: false, setup_complete: true, expires_at: later, idle_expires_at: later } as const;
const owner: WhoAmIResponseV1 = { id: 1, kind: 'human', name: 'owner', role: 'operator', owner: true, identity_class: 'operator', may_invite: true, permissions: catalog, permissions_revision: 1, auth };
const operator: WhoAmIResponseV1 = { ...owner, id: 2, name: 'operator-1', owner: false, permissions: catalog.slice(0, 9) };

const everyone: ServerMemberV1[] = [
  { id: 1, kind: 'human', name: 'owner', role: 'operator', owner: true, created_at: at },
  { id: 4, kind: 'agent', name: 'builder', created_at: at },
  { id: 5, kind: 'agent', name: 'plain-bot', created_at: at },
];

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const refusal = (status: number, code: string) => json({ code, message: 'SERVER-MESSAGE-NOT-FOR-PEOPLE' }, status);
const noContent = () => new Response(null, { status: 204 });

interface Call {
  readonly method: string;
  readonly path: string;
  readonly query: string;
  readonly body: unknown;
}
type Route = (call: Call) => Response | Promise<Response>;

class FakeSocket {
  static all: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  constructor(readonly url: string) {
    FakeSocket.all.push(this);
  }
  close() {
    // Nothing to release.
  }
  open() {
    act(() => {
      this.onopen?.();
    });
  }
  send(data: unknown) {
    act(() => {
      this.onmessage?.({ data: JSON.stringify(data) });
    });
  }
}

const manifest = (id: number, permissions: Permission[]) => ({
  manifest: {
    schema: 'conch.agent_manifest.v1',
    principal_id: id,
    display_name: 'builder',
    tier: 'A',
    capabilities: ['messages.read', 'admin.act'],
    channels: [{ channel_id: 11, permissions: ['read', 'post'] }],
    permissions,
    ...(permissions.length === 0 ? {} : { admin: { approval_channel_id: 11 } }),
    created_at: at,
    updated_at: at,
  },
});

interface Options {
  readonly routes?: Record<string, Route>;
  readonly channels?: unknown[];
  readonly messages?: unknown[];
}

function serve(who: WhoAmIResponseV1, opts: Options = {}): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const url = new URL(raw, origin);
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      const call = { method, path: url.pathname, query: url.search, body };
      calls.push(call);
      const route = opts.routes?.[`${method} ${url.pathname}`];
      if (route !== undefined) return route(call);
      const path = url.pathname;
      if (path === '/v1/whoami') return json(who);
      if (path === '/v1/channels' && method === 'GET') return json({ channels: [{ id: 7, name: 'channel-a', created_at: at }] });
      if (path === '/v1/directory/channels') return json({ channels: opts.channels ?? [{ id: 7, name: 'channel-a', created_at: at, visibility: 'public', joined: true, members: 2 }] });
      if (path === '/v1/directory/members') return json({ members: everyone });
      if (path === '/v1/permissions') return json({ revision: 1, rows: [], preset: 'group', presets: [], editable: [], warnings: [], updated_at: at });
      if (path === '/v1/ws-tickets') return json({ ticket: ticketValue, expires_at: at }, 201);
      if (path === '/v1/join/settings') {
        return json({
          settings: { open_joining: 'off', guests_enabled: false, joining_paused: false, join_voice: true, max_guests: 10, max_guests_per_address: 3, guest_idle_seconds: 3600, guest_absolute_seconds: 86400, open_channels: [], open_signup: false, member_link_default_expiry_seconds: 604800 },
          server_name: 'Alpha Server',
          live_guests: 0,
          warnings: [],
        });
      }
      if (path === '/v1/invites/settings') return json({ paused: false });
      if (path === '/v2/invites') return json({ invites: [] });
      if (path === '/v1/guests') return json({ guests: [] });
      if (path === '/v1/sessions' && method === 'POST') {
        return json({ schema: 'conch.session.v1', id: 100, principal_id: 1, kind: 'human', name: 'owner', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201);
      }
      if (path === '/v1/read-state') return json({ read_state: [] });
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[2] === 'members' && method === 'GET') return json({ members: [] });
      if (m?.[2] === 'nets') return json({ nets: [] });
      if (m?.[2] === 'messages') return json({ messages: opts.messages ?? [] });
      return json({}, 404);
    }),
  );
  return calls;
}

async function openMembers(token: string): Promise<HTMLElement> {
  window.sessionStorage.setItem(key, token);
  window.location.hash = '#/c/channel-a';
  render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  const region = await screen.findByRole('region', { name: 'Members' });
  await within(region).findByRole('list', { name: 'People' });
  return region;
}

const reads = (calls: readonly Call[], path: string) => calls.filter((call) => call.method === 'GET' && call.path === path).length;
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 50)));
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function channelEvents(): Promise<FakeSocket> {
  const isChannel = (socket: FakeSocket) => socket.url.includes('/v1/events/ws') && new URL(socket.url).searchParams.get('channel') === 'channel-a';
  await waitFor(() => {
    expect(FakeSocket.all.some(isChannel)).toBe(true);
  });
  for (const socket of FakeSocket.all) socket.open();
  await settle();
  const channels = FakeSocket.all.filter(isChannel);
  const events = channels[channels.length - 1];
  if (events === undefined) throw new Error('no channel events socket');
  return events;
}

beforeEach(() => {
  FakeSocket.all = [];
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

const writes = (calls: readonly Call[]) => calls.filter((c) => c.method !== 'GET' && !/ws-tickets|sessions|read-state/.test(c.path));
const step = (c: Call) => `${c.method} ${c.path}`;

// The routes of a good run, with a new agent #9 and an approval channel #11.
function goodRoutes(extra: Record<string, Route> = {}): Record<string, Route> {
  return {
    'POST /v0/principals': () => json({ principal: { id: 9, kind: 'agent', name: 'deploy-admin', created_at: at } }, 201),
    'POST /v1/channels': () => json({ channel: { id: 11, name: 'admin', created_at: at, visibility: 'private', manages: true } }, 201),
    'PUT /v1/channels/admin/members/1': noContent,
    'PUT /v1/channels/admin/members/9': noContent,
    'PUT /v1/principals/9/manifest': () => json({ manifest: manifest(9, []).manifest }),
    'POST /v1/principals/9/credentials': () =>
      json({ credential: { id: 30, principal_id: 9, label: 'ai-admin', created_at: at, renews: true }, token: agentToken }, 201),
    'POST /v1/principals/9/disable': noContent,
    ...extra,
  };
}

async function fill(region: HTMLElement, name: string): Promise<void> {
  fireEvent.click(within(region).getByRole('button', { name: 'Add an AI administrator' }));
  const form = await within(region).findByRole('form', { name: 'New AI administrator' });
  fireEvent.change(within(form).getByLabelText('Name'), { target: { value: name } });
}

describe('the action', () => {
  it('is offered to the owner and to no one else', async () => {
    serve(owner);
    const region = await openMembers('conch_owner_session_token');
    expect(within(region).getByRole('button', { name: 'Add an AI administrator' })).toBeTruthy();
    cleanup();
    vi.unstubAllGlobals();
    serve(operator);
    const other = await openMembers('conch_operator_session_token');
    expect(within(other).queryByRole('button', { name: 'Add an AI administrator' })).toBeNull();
  });

  it('runs the seven steps in order and shows the entry once, hidden until Reveal', async () => {
    const calls = serve(owner, { routes: goodRoutes() });
    const region = await openMembers('conch_owner_session_token');
    await fill(region, 'deploy-admin');
    fireEvent.click(within(region).getByRole('button', { name: 'Add AI administrator' }));
    const entry = await within(region).findByRole('group', { name: 'MCP entry for deploy-admin' });

    expect(writes(calls).map(step)).toEqual([
      'POST /v0/principals',
      'POST /v1/channels',
      'PUT /v1/channels/admin/members/1',
      'PUT /v1/channels/admin/members/9',
      'PUT /v1/principals/9/manifest',
      'POST /v1/principals/9/credentials',
    ]);
    const bodies = new Map(writes(calls).map((c) => [step(c), c.body]));
    expect(bodies.get('POST /v0/principals')).toEqual({ kind: 'agent', name: 'deploy-admin' });
    expect(bodies.get('POST /v1/channels')).toEqual({ name: 'admin', visibility: 'private' });
    expect(bodies.get('PUT /v1/channels/admin/members/1')).toEqual({ can_decide: true });
    expect(bodies.get('PUT /v1/channels/admin/members/9')).toEqual({ can_decide: false });
    expect(bodies.get('PUT /v1/principals/9/manifest')).toEqual({
      display_name: 'deploy-admin',
      tier: 'A',
      capabilities: ['messages.read', 'messages.post', 'approvals.await', 'approvals.check', 'admin.act'],
      channels: [{ channel_id: 11, permissions: ['read', 'post'] }],
      rate_limits: [{ capability: 'admin.act', max: 60, window_seconds: 3600 }],
      permissions: ['manage_members', 'manage_channels', 'make_member_links'],
      admin: { approval_channel_id: 11 },
    });
    expect(bodies.get('POST /v1/principals/9/credentials')).toEqual({ label: 'ai-admin', renew_lifetime_seconds: 90 * 86400 });
    expect(calls.some((c) => c.path.endsWith('/disable'))).toBe(false);

    // Hidden until Reveal, then the entry; Done takes it away and nothing keeps it.
    expect(document.body.textContent).not.toContain(agentToken);
    fireEvent.click(within(entry).getByRole('button', { name: 'Reveal' }));
    const shown = within(entry).getByRole('textbox', { name: 'MCP entry for deploy-admin' });
    if (!(shown instanceof HTMLInputElement)) throw new Error('no input');
    const parsed: unknown = JSON.parse(shown.value);
    expect(parsed).toEqual({ mcpServers: { 'conch-alpha-server': { type: 'http', url: `${origin}/mcp`, headers: { Authorization: `Bearer ${agentToken}` } } } });
    fireEvent.click(within(entry).getByRole('button', { name: 'Done' }));
    await waitFor(() => {
      expect(screen.queryByRole('group', { name: 'MCP entry for deploy-admin' })).toBeNull();
    });
    expect(document.body.textContent).not.toContain(agentToken);
    expect(JSON.stringify([Object.entries(window.sessionStorage), Object.entries(window.localStorage)])).not.toContain(agentToken);
  });

  it('uses an existing approval channel and the grant that was chosen', async () => {
    const calls = serve(owner, {
      channels: [{ id: 11, name: 'admin', created_at: at, visibility: 'private', joined: true, members: 1 }],
      routes: goodRoutes(),
    });
    const region = await openMembers('conch_owner_session_token');
    await fill(region, 'deploy-admin');
    fireEvent.click(within(region).getByRole('checkbox', { name: 'May manage channels' }));
    fireEvent.click(within(region).getByRole('checkbox', { name: 'May read the audit log' }));
    fireEvent.click(within(region).getByRole('button', { name: 'Add AI administrator' }));
    await within(region).findByRole('group', { name: 'MCP entry for deploy-admin' });
    expect(calls.some((c) => c.method === 'POST' && c.path === '/v1/channels')).toBe(false);
    expect(calls.find((c) => c.path === '/v1/principals/9/manifest')?.body).toMatchObject({ permissions: ['manage_members', 'make_member_links', 'read_audit_log'] });
  });

  it('keeps the two powerful grants off until they are chosen', async () => {
    serve(owner, { routes: goodRoutes() });
    const region = await openMembers('conch_owner_session_token');
    await fill(region, 'x');
    for (const name of ['May change server settings', 'May read the audit log']) expect(within(region).getByRole<HTMLInputElement>('checkbox', { name }).checked).toBe(false);
    for (const name of ['May manage members', 'May manage channels', 'May make member links']) expect(within(region).getByRole<HTMLInputElement>('checkbox', { name }).checked).toBe(true);
  });

  it('disables the new agent when the manifest is refused, says why in its own words, and makes no credential', async () => {
    const calls = serve(owner, { routes: goodRoutes({ 'PUT /v1/principals/9/manifest': () => refusal(403, 'forbidden') }) });
    const region = await openMembers('conch_owner_session_token');
    await fill(region, 'deploy-admin');
    fireEvent.click(within(region).getByRole('button', { name: 'Add AI administrator' }));
    await within(region).findByText(/Writing the grant failed/);
    expect(calls.some((c) => c.path === '/v1/principals/9/credentials')).toBe(false);
    expect(calls.filter((c) => c.path === '/v1/principals/9/disable')).toHaveLength(1);
    expect(region.textContent).toContain('was disabled, so nothing is left holding access');
    expect(region.textContent).not.toContain('SERVER-MESSAGE-NOT-FOR-PEOPLE');
    expect(screen.queryByRole('group', { name: 'MCP entry for deploy-admin' })).toBeNull();
  });

  it('says so when the rollback itself fails', async () => {
    serve(owner, { routes: goodRoutes({ 'POST /v1/principals/9/credentials': () => refusal(500, 'internal'), 'POST /v1/principals/9/disable': () => refusal(500, 'internal') }) });
    const region = await openMembers('conch_owner_session_token');
    await fill(region, 'deploy-admin');
    fireEvent.click(within(region).getByRole('button', { name: 'Add AI administrator' }));
    await within(region).findByText(/Disabling the new agent deploy-admin \(#9\) failed too/);
  });

  it('makes nothing to undo when the agent cannot be made', async () => {
    const calls = serve(owner, { routes: goodRoutes({ 'POST /v0/principals': () => refusal(409, 'name_taken') }) });
    const region = await openMembers('conch_owner_session_token');
    await fill(region, 'deploy-admin');
    fireEvent.click(within(region).getByRole('button', { name: 'Add AI administrator' }));
    await within(region).findByText(/Nothing was made/);
    expect(writes(calls).map(step)).toEqual(['POST /v0/principals']);
  });

  it('needs a name, a grant and a credential lifetime the server allows', async () => {
    serve(owner, { routes: goodRoutes() });
    const region = await openMembers('conch_owner_session_token');
    fireEvent.click(within(region).getByRole('button', { name: 'Add an AI administrator' }));
    const submit = within(region).getByRole('button', { name: 'Add AI administrator' });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(region).getByLabelText('Name'), { target: { value: 'a' } });
    expect((submit as HTMLButtonElement).disabled).toBe(false);
    fireEvent.change(within(region).getByLabelText(/credential lasts/), { target: { value: '1' } });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
  });
});

describe('entry and manifest', () => {
  it('names the entry from the server name', () => {
    expect(entryName('Alpha Server')).toBe('conch-alpha-server');
    expect(entryName('')).toBe('conch-conch');
  });

  it('writes the one-line form with quoting', () => {
    expect(entryText('claude-code', 'conch-a', 'https://a.example/mcp', 'tok')).toBe(
      "claude mcp add --transport http --scope user conch-a https://a.example/mcp --header 'Authorization: Bearer tok'",
    );
  });

  it('puts the grant in the manifest', () => {
    expect(manifestFor('x', 5, ['manage_members']).permissions).toEqual(['manage_members']);
  });
});

describe('the agent card', () => {
  const routes = (perms: Permission[]): Record<string, Route> => ({
    'GET /v1/principals/4/manifest': () => json(manifest(4, perms)),
    'GET /v1/principals/5/manifest': () => json(manifest(5, [])),
    'POST /v1/principals/4/disable': noContent,
  });

  it('shows the grant on an AI administrator and nothing on a plain agent', async () => {
    serve(owner, { routes: routes(['manage_members', 'manage_channels']) });
    const region = await openMembers('conch_owner_session_token');
    const card = await within(region).findByRole('group', { name: 'AI administrator builder' });
    expect(card.textContent).toContain('AI administrator: manage members, manage channels');
    expect(within(region).queryByRole('group', { name: 'AI administrator plain-bot' })).toBeNull();
  });

  it('keeps the card and its reads when a channel drops an agent still in the directory', async () => {
    const agent = { principal_id: 4, name: 'builder', kind: 'agent', created_at: at };
    let channel = [agent];
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/channels/channel-a/members': () => json({ members: channel }),
      },
    });
    const region = await openMembers('conch_owner_session_token');
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    const events = await channelEvents();
    events.send({ schema: 'conch.session_roster.v1', channel_id: 7, sessions: [] });
    const toggle = screen.getByRole('button', { name: 'Members' });
    if (toggle.getAttribute('aria-expanded') === 'false') fireEvent.click(toggle);
    await waitFor(() => {
      expect(screen.getByRole('list', { name: 'Agents' }).textContent).toContain('builder');
    });
    const before = reads(calls, '/v1/principals/4/manifest');

    events.send({ schema: 'conch.channel_members.v1', channel_id: 7, members: [] });
    channel = [];
    await waitFor(() => {
      expect(screen.queryByRole('list', { name: 'Agents' })?.textContent ?? '').not.toContain('builder');
    });
    expect(within(region).getByRole('group', { name: 'AI administrator builder' })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    const reopened = await screen.findByRole('region', { name: 'Members' });
    await within(reopened).findByRole('group', { name: 'AI administrator builder' });

    expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(before);
  });

  it('never reads a dropped agent again, even with a stale directory answer in flight', async () => {
    const stale = deferred<Response>();
    let directory = everyone;
    let hold = false;
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/directory/members': () => hold ? stale.promise : json({ members: directory }),
      },
    });
    const region = await openMembers('conch_owner_session_token');
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    const events = await channelEvents();
    const before = reads(calls, '/v1/principals/4/manifest');
    const directoryBefore = reads(calls, '/v1/directory/members');

    hold = true;
    events.send({ schema: 'conch.permissions_changed.v1' });
    await waitFor(() => {
      expect(reads(calls, '/v1/directory/members')).toBeGreaterThan(directoryBefore);
    });
    directory = everyone.filter((person) => person.id !== 4);
    hold = false;
    events.send({ schema: 'conch.directory_changed.v1' });
    await waitFor(() => {
      expect(within(region).queryByText('builder')).toBeNull();
    });
    await act(async () => {
      stale.resolve(json({ members: everyone }));
      await stale.promise;
    });
    await settle();

    expect(within(region).queryByText('builder')).toBeNull();
    expect(reads(calls, '/v1/principals/4/manifest')).toBe(before);
  });

  it('reads an agent again when a changed directory lists it again', async () => {
    let directory = everyone;
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/directory/members': () => json({ members: directory }),
      },
    });
    const region = await openMembers('conch_owner_session_token');
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    const events = await channelEvents();
    // The page reads the directory behind a coalescing timer
    // (directoryCoalesce); the test advances that timer itself, so neither
    // frame waits on the wall clock.
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout'] });
    const frameRead = async () => {
      events.send({ schema: 'conch.directory_changed.v1' });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(directoryCoalesce);
      });
    };
    directory = everyone.filter((person) => person.id !== 4);
    await frameRead();
    await waitFor(() => {
      expect(within(region).queryByText('builder')).toBeNull();
    });
    const before = reads(calls, '/v1/principals/4/manifest');

    directory = everyone;
    await frameRead();
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(before);
  });

  it('ends manifest retries after a 404 and logs no console error', async () => {
    const errors = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/principals/4/manifest': () => json({}, 404),
      },
    });
    await openMembers('conch_owner_session_token');
    await waitFor(() => {
      expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(0);
    });
    await channelEvents();
    const before = reads(calls, '/v1/principals/4/manifest');

    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    const reopened = await screen.findByRole('region', { name: 'Members' });
    await within(reopened).findByRole('list', { name: 'People' });
    await settle();

    expect(reads(calls, '/v1/principals/4/manifest')).toBe(before);
    expect(errors).not.toHaveBeenCalled();
  });

  it('ends a 404 stop when a changed directory still lists the agent', async () => {
    let missing = true;
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/principals/4/manifest': () => missing ? json({}, 404) : json(manifest(4, ['manage_members'])),
      },
    });
    const region = await openMembers('conch_owner_session_token');
    await within(region).findByRole('list', { name: 'People' });
    const events = await channelEvents();
    await waitFor(() => {
      expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(0);
    });
    await settle();
    const before = reads(calls, '/v1/principals/4/manifest');

    missing = false;
    events.send({ schema: 'conch.directory_changed.v1' });
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(before);
  });

  // The Settings view's request is held; Settings closes; the community panel
  // then answers newer questions. The old answer arrives last (#1562).
  async function twoViews() {
    const stale = deferred<Response>();
    const without = everyone.filter((person) => person.id !== 4);
    let directory = everyone;
    let hold = false;
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/directory/members': () => {
          if (!hold) return json({ members: directory });
          hold = false;
          return stale.promise;
        },
      },
    });
    const region = await openMembers('conch_owner_session_token');
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    const events = await channelEvents();
    events.send({ schema: 'conch.session_roster.v1', channel_id: 7, sessions: [] });
    const toggle = screen.getByRole('button', { name: 'Members' });
    if (toggle.getAttribute('aria-expanded') === 'false') fireEvent.click(toggle);
    await settle();
    // Settings closes and reopens: its new request is the one held.
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    hold = true;
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    await waitFor(() => {
      expect(hold).toBe(false);
    });
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    const changed = async () => {
      const asked = reads(calls, '/v1/directory/members');
      events.send({ schema: 'conch.directory_changed.v1' });
      await waitFor(() => {
        expect(reads(calls, '/v1/directory/members')).toBeGreaterThan(asked);
      });
      await settle();
    };
    directory = without;
    await changed();
    const before = reads(calls, '/v1/principals/4/manifest');
    const directoryBefore = reads(calls, '/v1/directory/members');
    await act(async () => {
      stale.resolve(json({ members: everyone }));
      await stale.promise;
    });
    await settle();
    return { calls, before, directoryBefore };
  }

  it('lets a closed view\'s stale answer neither resurrect a hidden agent nor start a read', async () => {
    const { calls, before, directoryBefore } = await twoViews();
    expect(reads(calls, '/v1/principals/4/manifest')).toBe(before);
    // The closed Settings view does not ask again for the answer it dropped.
    expect(reads(calls, '/v1/directory/members')).toBe(directoryBefore);
  });

  it('keeps the right to resume a stopped read when a routine refresh replaces the recovery request', async () => {
    // Both directory views (the panel and Settings) ask after the change; the
    // answers to both are held, then dropped as stale once the routine
    // refreshes' answers are in. Only the carried recovery can resume the read.
    const held: ((response: Response) => void)[] = [];
    let missing = true;
    let hold = 0;
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/principals/4/manifest': () => missing ? json({}, 404) : json(manifest(4, ['manage_members'])),
        'GET /v1/directory/members': () => {
          if (hold === 0) return json({ members: everyone });
          hold--;
          return new Promise<Response>((resolve) => held.push(resolve));
        },
      },
    });
    const region = await openMembers('conch_owner_session_token');
    const events = await channelEvents();
    await waitFor(() => {
      expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(0);
    });
    await settle();
    expect(within(region).queryByRole('group', { name: 'AI administrator builder' })).toBeNull();
    const before = reads(calls, '/v1/principals/4/manifest');

    missing = false;
    hold = 2;
    events.send({ schema: 'conch.directory_changed.v1' });
    await waitFor(() => {
      expect(held.length).toBe(2);
    });
    // No second directory change: only a routine refresh replaces the requests.
    events.send({ schema: 'conch.permissions_changed.v1' });
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(before);
    for (const resolve of held) resolve(json({ members: everyone }));
    await settle();
  });

  it('keeps a later change\'s recovery when an older change\'s answer arrives first', async () => {
    const held: { readonly resolve: (response: Response) => void }[] = [];
    let missing = true;
    let hold = 0;
    const calls = serve(owner, {
      routes: {
        ...routes(['manage_members']),
        'GET /v1/principals/4/manifest': () => missing ? json({}, 404) : json(manifest(4, ['manage_members'])),
        'GET /v1/directory/members': () => {
          if (hold === 0) return json({ members: everyone });
          hold--;
          return new Promise<Response>((resolve) => held.push({ resolve }));
        },
      },
    });
    const region = await openMembers('conch_owner_session_token');
    const events = await channelEvents();
    await waitFor(() => {
      expect(reads(calls, '/v1/principals/4/manifest')).toBeGreaterThan(0);
    });
    await settle();
    missing = false;
    // Two directory changes, their answers (hide, then re-enable) held.
    hold = 2;
    events.send({ schema: 'conch.directory_changed.v1' });
    await waitFor(() => {
      expect(held.length).toBe(2);
    });
    hold = 2;
    events.send({ schema: 'conch.directory_changed.v1' });
    await waitFor(() => {
      expect(held.length).toBe(4);
    });
    // The hide answer lands first; then a routine refresh is answered, newer
    // than the held re-enable answers, which are stale by then.
    const without = everyone.filter((person) => person.id !== 4);
    for (const request of held.slice(0, 2)) request.resolve(json({ members: without }));
    await settle();
    events.send({ schema: 'conch.permissions_changed.v1' });
    await within(region).findByRole('group', { name: 'AI administrator builder' });
    for (const request of held.slice(2)) request.resolve(json({ members: everyone }));
    await settle();
    expect(within(region).getByRole('group', { name: 'AI administrator builder' })).toBeTruthy();
  });

  it('asks first, then disables the principal', async () => {
    const calls = serve(owner, { routes: routes(['manage_members']) });
    const region = await openMembers('conch_owner_session_token');
    const card = await within(region).findByRole('group', { name: 'AI administrator builder' });
    fireEvent.click(within(card).getByRole('button', { name: 'Remove access' }));
    expect(calls.some((c) => c.path === '/v1/principals/4/disable')).toBe(false);
    fireEvent.click(within(card).getByRole('button', { name: 'Yes, remove access' }));
    await waitFor(() => {
      expect(calls.filter((c) => c.method === 'POST' && c.path === '/v1/principals/4/disable')).toHaveLength(1);
    });
    // The list reads again and the server still holds the manifest: the card
    // says the access is gone and offers no second removal.
    await waitFor(() => {
      expect(card.textContent).toContain('builder has no access now');
    });
    await waitFor(() => {
      expect(calls.filter((c) => c.path === '/v1/directory/members').length).toBeGreaterThan(1);
    });
    expect(within(card).queryByRole('button', { name: 'Remove access' })).toBeNull();
    expect(card.textContent).not.toContain('AI administrator: manage members');
  });

  it('leaves it as it was, with the page’s own words, when the server refuses', async () => {
    serve(owner, { routes: { ...routes(['manage_members']), 'POST /v1/principals/4/disable': () => refusal(403, 'forbidden') } });
    const region = await openMembers('conch_owner_session_token');
    const card = await within(region).findByRole('group', { name: 'AI administrator builder' });
    fireEvent.click(within(card).getByRole('button', { name: 'Remove access' }));
    fireEvent.click(within(card).getByRole('button', { name: 'Yes, remove access' }));
    await within(card).findByText(/Its access is unchanged/);
    expect(card.textContent).not.toContain('SERVER-MESSAGE-NOT-FOR-PEOPLE');
  });
});

describe('the approval card', () => {
  const action = {
    action: 'disable_member',
    route: 'POST /v1/principals/7/disable',
    target: { kind: 'principal', id: 7, name: 'alice' },
    params: { note: '<b>x</b>' },
    agent_id: 3,
    reason: 'alice asked to leave <img src=x onerror=alert(1)>',
    requested_at: at,
  };
  const approval = (payload: unknown, state = 'pending') => ({
    id: 58,
    requester_id: 3,
    channel_id: 7,
    title: 'AI admin builder: disable member "alice" (#7)',
    body: 'the body',
    ...(payload === undefined ? {} : { payload }),
    options: [
      { id: 'run', label: 'Run it', kind: 'approve' },
      { id: 'refuse', label: 'Do not run', kind: 'reject' },
    ],
    deadline: later,
    quorum: 1,
    state,
    created_at: at,
  });
  const message = (id: number, data: unknown) => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: at, body: 'fallback body', payload: { schema: 'conch.approval.v1', data } });

  // The deciding card is drawn from the server's approvals (GET
  // /v1/approvals), never from a message (security review of #1344).
  async function card(state = 'pending', extra: Record<string, Route> = {}): Promise<{ calls: Call[]; card: HTMLElement }> {
    const listed = approval({ schema: 'conch.admin_action.v1', data: action }, state);
    const calls = serve(owner, { routes: { 'GET /v1/approvals': () => json({ approvals: [listed], read_only: [] }), ...extra } });
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    return { calls, card: await screen.findByRole('article', { name: 'AI administrator request' }) };
  }

  it('draws no deciding card from a message, whatever its payload says', async () => {
    // Any member may post a message carrying a conch.approval.v1 payload
    // that names a real approval id: it must never be a place to decide.
    serve(owner, { messages: [message(1, approval({ schema: 'conch.admin_action.v1', data: action }))], routes: { 'GET /v1/approvals': () => json({ approvals: [], read_only: [] }) } });
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    const plain = await screen.findByRole('article', { name: 'Approval request' });
    expect(within(plain).queryAllByRole('button', { name: /Run it|Do not run/ })).toHaveLength(0);
    expect(screen.queryByRole('article', { name: 'AI administrator request' })).toBeNull();
  });

  it('shows the action, route, target, params and the agent’s reason, labelled, all as text', async () => {
    const { card: shown } = await card();
    expect(shown.textContent).toContain('disable_member');
    expect(shown.textContent).toContain('POST /v1/principals/7/disable');
    expect(shown.textContent).toContain('principal #7 "alice"');
    expect(shown.textContent).toContain('"note": "<b>x</b>"');
    expect(within(shown).getByText("The agent's own words")).toBeTruthy();
    expect(shown.textContent).toContain('alice asked to leave <img src=x onerror=alert(1)>');
    expect(shown.querySelector('img, b')).toBeNull();
    expect(within(shown).getByRole('button', { name: 'Run it' })).toBeTruthy();
    expect(within(shown).getByRole('button', { name: 'Do not run' })).toBeTruthy();
  });

  it('casts the decision of the button chosen, as the viewer', async () => {
    const { calls, card: shown } = await card('pending', { 'POST /v1/approvals/58/decisions': () => json({ decision: {}, state: 'resolved' }, 201) });
    fireEvent.click(within(shown).getByRole('button', { name: 'Do not run' }));
    await within(shown).findByText('You chose: Do not run.');
    const sent = calls.find((c) => c.path === '/v1/approvals/58/decisions');
    expect(sent?.body).toMatchObject({ principal_id: 1, option_id: 'refuse' });
    expect(within(shown).queryByRole('button', { name: 'Run it' })).toBeNull();
  });

  it('says it in its own words when the server will not take the decision', async () => {
    const { card: shown } = await card('pending', { 'POST /v1/approvals/58/decisions': () => refusal(403, 'forbidden') });
    fireEvent.click(within(shown).getByRole('button', { name: 'Run it' }));
    await within(shown).findByText(/Nothing was decided/);
    expect(shown.textContent).not.toContain('SERVER-MESSAGE-NOT-FOR-PEOPLE');
    expect(within(shown).getByRole('button', { name: 'Run it' })).toBeTruthy();
  });

  it('offers no button on an approval already decided', async () => {
    const { card: shown } = await card('resolved');
    expect(within(shown).queryAllByRole('button')).toHaveLength(0);
  });

  it('draws an open request from the approvals list, and no buttons where the viewer may not decide', async () => {
    const open = approval({ schema: 'conch.admin_action.v1', data: action });
    serve(owner, { routes: { 'GET /v1/approvals': () => json({ approvals: [open, { ...open, id: 59, channel_id: 99 }], read_only: [58] }) } });
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    const shown = await screen.findByRole('article', { name: 'AI administrator request' });
    expect(screen.getAllByRole('article', { name: 'AI administrator request' })).toHaveLength(1);
    expect(within(shown).queryAllByRole('button')).toHaveLength(0);
    expect(shown.textContent).toContain('you do not decide in this channel');
  });

  it('shows any other payload as the plain approval it was', async () => {
    serve(owner, { messages: [message(1, approval({ schema: 'leviathan.trade_signal.v1', data: { a: 1 } }))] });
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    const plain = await screen.findByRole('article', { name: 'Approval request' });
    expect(plain.textContent).toContain('AI admin builder: disable member');
    expect(within(plain).queryAllByRole('button')).toHaveLength(0);
  });

  it('shows an admin action it cannot read as the plain approval', async () => {
    serve(owner, { messages: [message(1, approval({ schema: 'conch.admin_action.v1', data: { nope: 1 } }))] });
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    const plain = await screen.findByRole('article', { name: 'Approval request' });
    expect(within(plain).queryAllByRole('button')).toHaveLength(0);
  });
});

describe('the approvals inbox poll', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  // Only the interval is faked, so the page's own waits stay real.
  async function rendered(route: Route): Promise<() => number> {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    const calls = serve(owner, { routes: { 'GET /v1/approvals': route } });
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    const asked = () => calls.filter((c) => c.path === '/v1/approvals').length;
    for (let i = 0; i < 200 && asked() === 0; i++) await new Promise((r) => setTimeout(r, 10));
    expect(asked()).toBeGreaterThan(0);
    // Let the first answer land.
    await new Promise((r) => setTimeout(r, 50));
    return asked;
  }

  it('stops asking once the server refuses a guest', async () => {
    const asked = await rendered(() => refusal(403, 'guest_not_allowed'));
    const before = asked();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(asked()).toBe(before);
  });

  it('keeps asking after a server hiccup, so no request is hidden by one bad answer', async () => {
    const asked = await rendered(() => refusal(500, 'internal_error'));
    const before = asked();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(9_000);
    });
    expect(asked()).toBeGreaterThan(before);
  });

  it('keeps asking while the answers are good', async () => {
    const asked = await rendered(() => json({ approvals: [] }));
    const before = asked();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(9_000);
    });
    expect(asked()).toBeGreaterThan(before);
  });
});
