// The console's Channels section (ADR-014 design §10.2, §10.3, issue #1021),
// against a fake conchd (fetch) and a fake WebSocket: who is offered the
// section, the list from ?scope=all with its managers, the request each action
// sends, the page's own words for a refusal (never the server's message), the
// first action on a personal server, and the phone page. No network, no browser.
import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { AdminConsole } from './AdminConsole';
import type { ChannelListingV1, ChannelMemberV1 } from './schema/channel';
import type { Permission, PermissionsResponseV1 } from './schema/permission';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const leak = 'SERVER-MESSAGE-NOT-FOR-PEOPLE';

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
const operatorRights: Permission[] = catalog.slice(0, 9);
const auth = { scope: 'full', password_set: true, totp_enrolled: false, setup_complete: true, expires_at: later, idle_expires_at: later } as const;

const owner: WhoAmIResponseV1 = { id: 1, kind: 'human', name: 'owner', role: 'operator', owner: true, identity_class: 'operator', may_invite: true, permissions: catalog, permissions_revision: 1, auth };
const operator: WhoAmIResponseV1 = { ...owner, id: 2, name: 'operator-1', owner: false, permissions: operatorRights };
const bareOperator: WhoAmIResponseV1 = { ...operator, permissions: operatorRights.filter((p) => p !== 'manage_channels') };

const channelA: ChannelListingV1 = { id: 1, name: 'channel-a', created_at: at, visibility: 'public', default: true, joined: true, members: 2 };
const channelP: ChannelListingV1 = { id: 2, name: 'channel-p', created_at: at, visibility: 'private', joined: false, members: 1 };
const member = (id: number, name: string, manages = false): ChannelMemberV1 => ({ principal_id: id, name, kind: 'human', created_at: at, ...(manages ? { manages: true } : {}) });
const people: Record<string, ChannelMemberV1[]> = {
  'channel-a': [member(1, 'owner', true), member(3, 'member-1')],
  'channel-p': [member(1, 'owner')],
};

function doc(preset: 'personal' | 'group'): PermissionsResponseV1 {
  return {
    revision: 2,
    rows: catalog.map((permission) => ({ permission, member: false, operator: operatorRights.includes(permission) })),
    preset,
    presets: [
      { preset: 'personal', member: [] },
      { preset: 'group', member: [] },
    ],
    editable: ['member', 'operator'],
    warnings: [],
    updated_at: at,
  };
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const refusal = (status: number, code: string, more: Record<string, unknown> = {}) => json({ code, message: leak, ...more }, status);

interface Call {
  readonly method: string;
  readonly path: string;
  readonly query: string;
  readonly body: unknown;
}
type Route = (call: Call) => Response | Promise<Response>;

class FakeSocket {
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  constructor(readonly url: string) {}
  close() {
    // Nothing to release.
  }
}

function serve(who: WhoAmIResponseV1, opts: { preset?: 'personal' | 'group'; rows?: ChannelListingV1[]; routes?: Record<string, Route> } = {}): Call[] {
  const calls: Call[] = [];
  const rows = opts.rows ?? [channelA, channelP];
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
      if (path === '/v1/channels' && method === 'GET') return json({ channels: [{ id: 1, name: 'channel-a', created_at: at }] });
      if (path === '/v1/directory/channels') return json({ channels: rows });
      if (path === '/v1/permissions') return json(doc(opts.preset ?? 'group'));
      if (path === '/v1/ws-tickets') return json({ ticket: ticketValue, expires_at: at }, 201);
      if (path === '/v1/join/settings') {
        return json({
          settings: { open_joining: 'off', guests_enabled: false, joining_paused: false, join_voice: true, max_guests: 10, max_guests_per_address: 3, guest_idle_seconds: 3600, guest_absolute_seconds: 86400, open_channels: ['channel-a'], open_signup: false, member_link_default_expiry_seconds: 604800 },
          server_name: 'conch.example',
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
      if (m?.[2] === 'members' && method === 'GET') return json({ members: people[m[1] ?? ''] ?? [] });
      if (m?.[2] === 'nets') return json({ nets: [] });
      if (m?.[2] === 'messages') return json({ messages: [] });
      return json({}, 404);
    }),
  );
  return calls;
}

async function open(): Promise<HTMLElement> {
  window.sessionStorage.setItem(key, 'conch_owner_session_token');
  window.location.hash = '#/c/channel-a';
  render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  const region = await screen.findByRole('region', { name: 'Channels' });
  await within(region).findByRole('list', { name: 'Every channel' });
  return region;
}

const rowOf = (region: HTMLElement, name: string): HTMLElement => {
  const found = region.querySelector<HTMLElement>(`[data-channel="${name}"]`);
  if (found === null) throw new Error(`no row ${name}`);
  return found;
};

async function menu(row: HTMLElement, name: string): Promise<HTMLElement> {
  fireEvent.click(within(row).getByRole('button', { name: `Actions for ${name}` }));
  return within(row).findByRole('menu');
}

async function choose(row: HTMLElement, channel: string, item: string): Promise<void> {
  const m = await menu(row, channel);
  fireEvent.click(within(m).getByRole('menuitem', { name: item }));
}

const sent = (calls: readonly Call[], method: string, pattern: RegExp) => calls.filter((c) => c.method === method && pattern.test(c.path));

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('who is offered the section', () => {
  it('shows an owner every channel, from ?scope=all, with visibility, default, count and managers', async () => {
    const calls = serve(owner);
    const region = await open();
    expect(calls.some((c) => c.path === '/v1/directory/channels' && c.query === '?scope=all')).toBe(true);
    const a = rowOf(region, 'channel-a');
    expect(a.textContent).toContain('default channel');
    expect(a.textContent).toContain('2 people');
    await waitFor(() => {
      expect(a.textContent).toContain('Managed by owner');
    });
    expect(within(a).queryByText('Private')).toBeNull();
    const p = rowOf(region, 'channel-p');
    expect(within(p).getByText('Private')).toBeTruthy();
    expect(p.textContent).toContain('1 person');
  });

  it('shows an operator who holds manage_channels the section', async () => {
    serve(operator);
    await open();
  });

  it('omits the section for an operator without manage_channels, and asks for no list', async () => {
    const calls = serve(bareOperator);
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await screen.findByRole('region', { name: 'Roles and permissions' });
    expect(screen.queryByRole('region', { name: 'Channels' })).toBeNull();
    expect(calls.some((c) => c.query === '?scope=all')).toBe(false);
  });
});

describe('the actions send their requests', () => {
  it('makes a private channel public after asking, and sends only the visibility', async () => {
    const calls = serve(owner, {
      routes: { 'PUT /v1/channels/2/settings': () => json({ channel: { ...channelP, visibility: 'public' } }) },
    });
    const region = await open();
    const row = rowOf(region, 'channel-p');
    await choose(row, 'channel-p', 'Make public');
    expect(sent(calls, 'PUT', /settings$/)).toEqual([]);
    fireEvent.click(await within(row).findByRole('button', { name: 'Make public' }));
    await waitFor(() => {
      expect(sent(calls, 'PUT', /settings$/)).toEqual([{ method: 'PUT', path: '/v1/channels/2/settings', query: '', body: { visibility: 'public' } }]);
    });
  });

  it('marks a public channel a default and sends only the default', async () => {
    const pub: ChannelListingV1 = { ...channelA, default: false };
    const calls = serve(owner, {
      rows: [pub],
      routes: { 'PUT /v1/channels/1/settings': () => json({ channel: { ...pub, default: true } }) },
    });
    const region = await open();
    await choose(rowOf(region, 'channel-a'), 'channel-a', 'Make default channel');
    await waitFor(() => {
      expect(sent(calls, 'PUT', /settings$/).map((c) => c.body)).toEqual([{ default: true }]);
    });
  });

  it('disables making a default channel private, with the reason', async () => {
    serve(owner);
    const region = await open();
    const a = await menu(rowOf(region, 'channel-a'), 'channel-a');
    const toPrivate = within(a).getByRole('menuitem', { name: 'Make private' });
    expect(toPrivate.getAttribute('aria-disabled')).toBe('true');
    expect(a.textContent).toContain('A default channel must be public. Stop it being a default channel first.');
  });

  it('shows the 409 for a private default in the page’s words, not the server’s', async () => {
    // The list is stale: the channel was made private meanwhile, and the server refuses.
    const stale: ChannelListingV1 = { ...channelP, visibility: 'public' };
    serve(owner, {
      rows: [stale],
      routes: { 'PUT /v1/channels/2/settings': () => refusal(409, 'channel_settings_conflict') },
    });
    const region = await open();
    const row = rowOf(region, 'channel-p');
    await choose(row, 'channel-p', 'Make default channel');
    expect(await within(row).findByText('A private channel cannot be a default channel. Make it public first.')).toBeTruthy();
    expect(region.textContent).not.toContain(leak);
  });

  it('renames a channel, sending the name it shows now', async () => {
    const calls = serve(owner, {
      routes: { 'PUT /v1/channels/2/name': () => json({ channel: { id: 2, name: 'channel-q', created_at: at, visibility: 'private' } }) },
    });
    const region = await open();
    const row = rowOf(region, 'channel-p');
    await choose(row, 'channel-p', 'Rename channel…');
    fireEvent.change(within(row).getByLabelText('New name'), { target: { value: 'channel-q' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Rename channel' }));
    await waitFor(() => {
      expect(sent(calls, 'PUT', /name$/)).toEqual([{ method: 'PUT', path: '/v1/channels/2/name', query: '', body: { name: 'channel-q', from: 'channel-p' } }]);
    });
  });

  it('shows a taken name in the page’s words', async () => {
    serve(owner, { routes: { 'PUT /v1/channels/2/name': () => refusal(409, 'channel_exists') } });
    const region = await open();
    const row = rowOf(region, 'channel-p');
    await choose(row, 'channel-p', 'Rename channel…');
    fireEvent.change(within(row).getByLabelText('New name'), { target: { value: 'channel-a' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Rename channel' }));
    expect(await within(row).findByText(/That name is taken/)).toBeTruthy();
    expect(region.textContent).not.toContain(leak);
  });

  it('adds a person by name, once however often the button is pressed', async () => {
    const calls = serve(owner, { routes: { 'POST /v1/channels/channel-p/members': () => json(member(3, 'member-1'), 201) } });
    const region = await open();
    const row = rowOf(region, 'channel-p');
    await choose(row, 'channel-p', 'Add person…');
    fireEvent.change(within(row).getByLabelText('Their name'), { target: { value: 'member-1' } });
    const submit = within(row).getByRole('button', { name: 'Add person' });
    fireEvent.click(submit);
    fireEvent.click(submit);
    await waitFor(() => {
      expect(sent(calls, 'POST', /\/members$/)).toEqual([{ method: 'POST', path: '/v1/channels/channel-p/members', query: '', body: { name: 'member-1' } }]);
    });
    expect(await within(row).findByText('Added member-1 to channel-p.')).toBeTruthy();
  });

  it('says nobody has that name when the server cannot find them', async () => {
    serve(owner, { routes: { 'POST /v1/channels/channel-p/members': () => refusal(404, 'principal_not_found') } });
    const region = await open();
    const row = rowOf(region, 'channel-p');
    await choose(row, 'channel-p', 'Add person…');
    fireEvent.change(within(row).getByLabelText('Their name'), { target: { value: 'nobody-1' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Add person' }));
    expect(await within(row).findByText(/Nobody on this server has that name/)).toBeTruthy();
    expect(region.textContent).not.toContain(leak);
  });

  it('toggles manages per member, and removes a person only after asking', async () => {
    const calls = serve(owner, {
      routes: {
        'DELETE /v1/channels/channel-a/members/3': () => new Response(null, { status: 204 }),
        'PUT /v1/channels/channel-a/members/3': () => new Response(null, { status: 204 }),
      },
    });
    const region = await open();
    const row = rowOf(region, 'channel-a');
    await choose(row, 'channel-a', 'People and managers…');
    fireEvent.click(await within(row).findByRole('button', { name: 'Let member-1 manage channel-a' }));
    await waitFor(() => {
      expect(sent(calls, 'PUT', /members\/3$/).map((c) => c.body)).toEqual([{ manages: true }]);
    });
    fireEvent.click(await within(row).findByRole('button', { name: 'Remove member-1 from channel-a' }));
    expect(sent(calls, 'DELETE', /members/)).toEqual([]);
    fireEvent.click(within(row).getByRole('button', { name: 'Remove member-1' }));
    await waitFor(() => {
      expect(sent(calls, 'DELETE', /members/).map((c) => c.path)).toEqual(['/v1/channels/channel-a/members/3']);
    });
  });

  it('shows a refusal by its permission, never the server’s message', async () => {
    serve(owner, { routes: { 'DELETE /v1/channels/channel-a/members/3': () => refusal(403, 'permission_denied', { permission: 'manage_channels' }) } });
    const region = await open();
    const row = rowOf(region, 'channel-a');
    await choose(row, 'channel-a', 'People and managers…');
    fireEvent.click(await within(row).findByRole('button', { name: 'Remove member-1 from channel-a' }));
    fireEvent.click(within(row).getByRole('button', { name: 'Remove member-1' }));
    expect(await within(row).findByText('You need the permission to manage channels for that. Nobody was removed.')).toBeTruthy();
    expect(region.textContent).not.toContain(leak);
  });

  it('creates a private channel', async () => {
    const calls = serve(owner, {
      routes: { 'POST /v1/channels': () => json({ channel: { id: 9, name: 'channel-n', created_at: at, visibility: 'private', manages: true } }, 201) },
    });
    const region = await open();
    fireEvent.click(within(region).getByRole('button', { name: 'New channel' }));
    fireEvent.change(within(region).getByLabelText('Channel name'), { target: { value: 'channel-n' } });
    fireEvent.click(within(region).getByRole('radio', { name: /Private/ }));
    fireEvent.click(within(region).getByRole('button', { name: 'Create channel' }));
    await waitFor(() => {
      expect(sent(calls, 'POST', /^\/v1\/channels$/).map((c) => c.body)).toEqual([{ name: 'channel-n', visibility: 'private' }]);
    });
  });
});

describe('a personal server', () => {
  it('leads each row’s menu with Add person', async () => {
    serve(owner, { preset: 'personal' });
    const region = await open();
    await waitFor(async () => {
      const m = await menu(rowOf(region, 'channel-a'), 'channel-a');
      expect(within(m).getAllByRole('menuitem')[0]?.textContent).toBe('Add person…');
    });
  });

  it('leads with Rename on a group server', async () => {
    serve(owner, { preset: 'group' });
    const region = await open();
    const m = await menu(rowOf(region, 'channel-a'), 'channel-a');
    expect(within(m).getAllByRole('menuitem')[0]?.textContent).toBe('Rename channel…');
    expect(within(m).getByRole('menuitem', { name: 'Add person…' })).toBeTruthy();
  });
});

describe('the phone', () => {
  it('lists Channels among the sections, opens it as a page, and offers row actions from the ⋯ button', async () => {
    serve(owner);
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'Settings' });
    cleanup();
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: true, addEventListener: () => undefined, removeEventListener: () => undefined })),
    );
    render(<AdminConsole who={owner} origin={origin} channels={[]} permissionsChanged={0} onChannelsChanged={() => undefined} onUnauthorized={() => undefined} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Channels' }));
    const region = await screen.findByRole('region', { name: 'Channels' });
    const row = rowOf(region, 'channel-a');
    const m = await menu(row, 'channel-a');
    expect(within(m).getAllByRole('menuitem').length).toBeGreaterThan(3);
  });
});
