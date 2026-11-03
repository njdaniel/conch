// The console's Members section and the one permission-aware menu on a person
// (ADR-014 design §10.2, §10.3, issue #1020, and #972), against a fake conchd
// (fetch) and a fake WebSocket: the directory with the operator fields for a
// holder of manage_members, what each viewer is offered on each kind of person,
// the request each action sends and when, the in-place confirmations, the
// page's own words for a refusal (never the server's), the voice line when the
// role's cell is off, the ways in and out of the menu, a grant that goes while
// a question is open, and the phone page. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { AdminConsole } from './AdminConsole';
import type { ChannelListingV1 } from './schema/channel';
import type { Permission, PermissionsResponseV1 } from './schema/permission';
import type { ServerMemberV1, WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const leak = 'SERVER-MESSAGE-NOT-FOR-PEOPLE';
const secretCode = 'conch_sil_SECRETCODESECRETCODESECRETCODE';
const secretToken = 'conch_tok_SECRETTOKENSECRETTOKENSECRET';

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
const withoutActing: WhoAmIResponseV1 = { ...operator, permissions: operatorRights.filter((p) => p !== 'act_as_members') };
const withoutMembers: WhoAmIResponseV1 = { ...operator, permissions: operatorRights.filter((p) => p !== 'manage_members' && p !== 'act_as_members') };

const operatorFields = { voice_allowed: true, may_invite: true };
const person = (id: number, name: string, more: Partial<ServerMemberV1> = {}): ServerMemberV1 => ({ id, kind: 'human', name, role: 'member', created_at: at, ...operatorFields, ...more });
const everyone: ServerMemberV1[] = [
  person(1, 'owner', { role: 'operator', owner: true }),
  person(2, 'operator-1', { role: 'operator', invited_by: 1 }),
  person(3, 'member-1', { invited_by: 1 }),
  { id: 4, kind: 'agent', name: 'builder', created_at: at },
  person(5, 'owner-2', { role: 'operator', owner: true }),
];
// What a viewer without manage_members is told: no operator fields.
const stripped = (members: readonly ServerMemberV1[]): ServerMemberV1[] =>
  members.map((m) => {
    const rest = { ...m };
    delete rest.voice_allowed;
    delete rest.may_invite;
    delete rest.invited_by;
    return rest;
  });

const channelA: ChannelListingV1 = { id: 1, name: 'channel-a', created_at: at, visibility: 'public', default: true, joined: true, members: 2 };
const channelP: ChannelListingV1 = { id: 2, name: 'channel-p', created_at: at, visibility: 'private', joined: false, members: 1 };

function doc(voiceForMembers: boolean): PermissionsResponseV1 {
  return {
    revision: 2,
    rows: catalog.map((permission) => ({ permission, member: permission === 'use_voice' ? voiceForMembers : false, operator: operatorRights.includes(permission) })),
    preset: 'group',
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

async function tick(): Promise<void> {
  await waitFor(() => {
    expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
  });
  for (const s of FakeSocket.all) s.open();
  FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'))?.send({ schema: 'conch.permissions_changed.v1' });
}

interface Options {
  readonly members?: () => ServerMemberV1[];
  readonly voiceForMembers?: boolean;
  readonly routes?: Record<string, Route>;
}

function serve(who: WhoAmIResponseV1 | (() => WhoAmIResponseV1), opts: Options = {}): Call[] {
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
      if (path === '/v1/whoami') return json(typeof who === 'function' ? who() : who);
      if (path === '/v1/channels' && method === 'GET') return json({ channels: [{ id: 1, name: 'channel-a', created_at: at }] });
      if (path === '/v1/directory/channels') return json({ channels: [channelA, channelP] });
      if (path === '/v1/directory/members') return json({ members: opts.members?.() ?? everyone });
      if (path === '/v1/permissions') return json(doc(opts.voiceForMembers ?? true));
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
      if (m?.[2] === 'members' && method === 'GET') return json({ members: [] });
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
  const region = await screen.findByRole('region', { name: 'Members' });
  await within(region).findByRole('list', { name: 'People' });
  return region;
}

const rowOf = (region: HTMLElement, name: string): HTMLElement => {
  const found = Array.from(region.querySelectorAll<HTMLElement>('li.community-member')).find((li) => li.querySelector('.community-name')?.textContent === name);
  if (found === undefined) throw new Error(`no row ${name}`);
  return found;
};

async function menu(row: HTMLElement, name: string): Promise<HTMLElement> {
  fireEvent.click(within(row).getByRole('button', { name: `Actions for ${name}` }));
  return within(row).findByRole('menu');
}

const labels = (m: HTMLElement) => within(m).getAllByRole('menuitem').map((i) => i.textContent);

async function choose(row: HTMLElement, name: string, item: string): Promise<void> {
  const m = await menu(row, name);
  fireEvent.click(within(m).getByRole('menuitem', { name: item }));
}

const sent = (calls: readonly Call[], method: string, pattern: RegExp) => calls.filter((c) => c.method === method && pattern.test(c.path));
const writes = (calls: readonly Call[]) => calls.filter((c) => c.method !== 'GET' && !/ws-tickets|sessions|read-state/.test(c.path));

beforeEach(() => {
  FakeSocket.all = [];
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the list', () => {
  it('shows the directory with the operator fields for a holder of manage_members', async () => {
    const calls = serve(owner);
    const region = await open();
    expect(calls.some((c) => c.path === '/v1/directory/members')).toBe(true);
    const row = rowOf(region, 'operator-1');
    expect(row.textContent).toContain('Operator');
    expect(row.textContent).toContain('voice on');
    expect(row.textContent).toContain('can invite');
    expect(row.textContent).toContain('invited by owner');
    expect(within(rowOf(region, 'builder')).getByText('Agent')).toBeTruthy();
    expect(within(rowOf(region, 'owner-2')).getByText('Owner')).toBeTruthy();
  });

  it('shows no operator field the server did not send, and offers no action, to an operator without manage_members', async () => {
    serve(withoutMembers, { members: () => stripped(everyone) });
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    expect(region.textContent).not.toContain('voice on');
    expect(region.textContent).not.toContain('can invite');
    expect(region.textContent).not.toContain('invited by');
    // manage_channels is still held: adding to a channel is all there is.
    const m = await menu(rowOf(region, 'member-1'), 'member-1');
    expect(labels(m)).toEqual(['Add to a channel…']);
  });

  it('reads the directory for the console only while Settings is open', async () => {
    const calls = serve(owner);
    window.sessionStorage.setItem(key, 'conch_owner_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    const settings = await screen.findByRole('button', { name: 'Settings' });
    await later10();
    const reads = () => calls.filter((c) => c.path === '/v1/directory/members').length;
    const before = reads();
    await later10();
    expect(reads()).toBe(before);
    fireEvent.click(settings);
    await screen.findByRole('region', { name: 'Members' });
    await waitFor(() => {
      expect(reads()).toBeGreaterThan(before);
    });
  });
});

describe('what each viewer is offered', () => {
  it('offers an owner everything on a member, in words that say it acts as the person', async () => {
    serve(owner);
    const region = await open();
    const m = await menu(rowOf(region, 'member-1'), 'member-1');
    expect(labels(m)).toEqual([
      'Turn voice off',
      'Stop member links',
      'Add to a channel…',
      'Make a link that signs in as this person…',
      'Make a credential that acts as this person…',
      'Make a hook that posts as this person…',
      'Change role…',
      'Sign out everywhere…',
      'Disable sign-in…',
      'Enable sign-in',
      'Remove from server…',
    ]);
  });

  it('offers an operator the same but for the role, and nothing on an owner', async () => {
    serve(operator);
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    expect(labels(await menu(rowOf(region, 'member-1'), 'member-1'))).not.toContain('Change role…');
    // Nobody but an owner acts on an owner.
    expect(within(rowOf(region, 'owner-2')).queryByRole('button', { name: 'Actions for owner-2' })).toBeNull();
    expect(within(rowOf(region, 'owner')).queryByRole('button', { name: /^Actions for/ })).toBeNull();
    // Their own row offers nothing.
    expect(within(rowOf(region, 'operator-1')).queryByRole('button', { name: /^Actions for/ })).toBeNull();
  });

  it('offers no sign-in-link item to an operator without act_as_members, and none for an operator target', async () => {
    serve(withoutActing);
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    const m = await menu(rowOf(region, 'member-1'), 'member-1');
    expect(labels(m).some((l) => /signs in as|acts as|posts as/.test(l))).toBe(false);
    expect(labels(m)).toContain('Remove from server…');
  });

  it('offers the owner no link to sign in as an operator, and no link brake or role on an agent', async () => {
    serve(owner);
    const region = await open();
    const op = labels(await menu(rowOf(region, 'operator-1'), 'operator-1'));
    expect(op).not.toContain('Make a link that signs in as this person…');
    expect(op).toContain('Make a credential that acts as this person…');
    fireEvent.keyDown(within(rowOf(region, 'operator-1')).getByRole('menu'), { key: 'Escape' });
    const agent = labels(await menu(rowOf(region, 'builder'), 'builder'));
    expect(agent).not.toContain('Change role…');
    expect(agent).not.toContain('Turn voice off');
    expect(agent).not.toContain('Stop member links');
  });
});

describe('the voice line', () => {
  it('shows the role-off line in place of a voice switch that would do nothing', async () => {
    serve(owner, { voiceForMembers: false });
    const region = await open();
    const row = rowOf(region, 'member-1');
    const first = await menu(row, 'member-1');
    await waitFor(() => {
      expect(first.textContent).toContain('Voice is off for all members (Roles and permissions)');
    });
    const item = within(first).getByRole('menuitem', { name: 'Turn voice off' });
    expect(item.getAttribute('aria-disabled')).toBe('true');
  });

  it('is a plain switch when the role has voice', async () => {
    serve(owner, { voiceForMembers: true });
    const region = await open();
    const m = await menu(rowOf(region, 'member-1'), 'member-1');
    await later10();
    expect(m.textContent).not.toContain('Voice is off for all members');
    expect(within(m).getByRole('menuitem', { name: 'Turn voice off' }).getAttribute('aria-disabled')).toBeNull();
  });
});

const later10 = () => new Promise<void>((resolve) => setTimeout(resolve, 30));

describe('the actions send their requests', () => {
  it('switches voice off and on for a member', async () => {
    const calls = serve(owner, { routes: { 'PUT /v1/principals/3/voice-permission': (c) => json({ principal_id: 3, voice_allowed: (c.body as { voice_allowed: boolean }).voice_allowed, changed: true }) } });
    const region = await open();
    await choose(rowOf(region, 'member-1'), 'member-1', 'Turn voice off');
    await waitFor(() => {
      expect(sent(calls, 'PUT', /voice-permission$/).map((c) => c.body)).toEqual([{ voice_allowed: false }]);
    });
    expect(await within(region).findByText('Voice is off for member-1.')).toBeTruthy();
  });

  it('switches member links off', async () => {
    const calls = serve(owner, { routes: { 'PUT /v1/principals/3/invite-permission': noContent } });
    const region = await open();
    await choose(rowOf(region, 'member-1'), 'member-1', 'Stop member links');
    await waitFor(() => {
      expect(sent(calls, 'PUT', /invite-permission$/).map((c) => c.body)).toEqual([{ may_invite: false }]);
    });
  });

  it('changes a role, sending the owner mark with it, and says what it ended', async () => {
    const response = {
      principal: { id: 3, kind: 'human', name: 'member-1', role: 'operator', created_at: at, live_credentials: 0 },
      manages: [],
      invites_closed: 0,
      credentials_revoked: 2,
      hooks_revoked: 1,
      live_credentials: 0,
      hooks_bound: 0,
    };
    const calls = serve(owner, { routes: { 'PUT /v1/principals/3/role': () => json(response) } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Change role…');
    const submit = within(row).getByRole('button', { name: 'Make a member' });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(within(row).getByRole('radio', { name: 'Operator' }));
    expect(sent(calls, 'PUT', /role$/)).toEqual([]);
    fireEvent.click(within(row).getByRole('button', { name: 'Make an operator' }));
    await waitFor(() => {
      expect(sent(calls, 'PUT', /role$/).map((c) => c.body)).toEqual([{ role: 'operator', owner: false }]);
    });
    expect(await within(region).findByText('member-1 is now an operator. 2 sign-ins and 1 hooks ended.')).toBeTruthy();
  });

  it('makes an owner by sending the owner mark', async () => {
    const calls = serve(owner, { routes: { 'PUT /v1/principals/2/role': () => refusal(403, 'forbidden') } });
    const region = await open();
    const row = rowOf(region, 'operator-1');
    await choose(row, 'operator-1', 'Change role…');
    fireEvent.click(within(row).getByRole('radio', { name: 'Owner' }));
    fireEvent.click(within(row).getByRole('button', { name: 'Make an owner, who holds every permission' }));
    await waitFor(() => {
      expect(sent(calls, 'PUT', /role$/).map((c) => c.body)).toEqual([{ role: 'operator', owner: true }]);
    });
  });

  it('adds a person to a channel without letting them decide approvals', async () => {
    const calls = serve(owner, { routes: { 'PUT /v1/channels/channel-p/members/3': noContent } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Add to a channel…');
    const select = within(row).getByLabelText('Add member-1 to');
    await waitFor(() => {
      expect(within(select).getAllByRole('option').length).toBeGreaterThan(2);
    });
    fireEvent.change(select, { target: { value: 'channel-p' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Add to channel' }));
    await waitFor(() => {
      expect(sent(calls, 'PUT', /members\/3$/)).toEqual([{ method: 'PUT', path: '/v1/channels/channel-p/members/3', query: '', body: { can_decide: false } }]);
    });
  });

  it('enables a person at once', async () => {
    const calls = serve(owner, { routes: { 'POST /v1/principals/3/enable': noContent } });
    const region = await open();
    await choose(rowOf(region, 'member-1'), 'member-1', 'Enable sign-in');
    await waitFor(() => {
      expect(sent(calls, 'POST', /enable$/).length).toBe(1);
    });
  });
});

describe('destructive actions ask first', () => {
  const cases = [
    { item: 'Sign out everywhere…', yes: 'Yes, sign out', method: 'POST', route: 'POST /v1/principals/3/credentials/revoke-all', path: '/v1/principals/3/credentials/revoke-all', answer: () => json({ revoked: 2 }) },
    { item: 'Disable sign-in…', yes: 'Yes, disable', method: 'POST', route: 'POST /v1/principals/3/disable', path: '/v1/principals/3/disable', answer: noContent },
    {
      item: 'Remove from server…',
      yes: 'Yes, remove',
      method: 'POST',
      route: 'POST /v1/principals/3/hide',
      path: '/v1/principals/3/hide',
      answer: () => json({ principal: { id: 3, kind: 'human', name: 'member-1', role: 'member', created_at: at, hidden_at: at, live_credentials: 0 } }),
    },
  ];

  it.each(cases)('$item sends nothing until the question is answered yes', async ({ item, yes, method, route, path, answer }) => {
    const calls = serve(owner, { routes: { [route]: answer } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', item);
    const question = within(row).getByRole('group');
    expect(question.textContent).toContain('member-1');
    expect(writes(calls)).toEqual([]);
    // Cancel is where focus starts, so Enter does not confirm by accident.
    expect(document.activeElement).toBe(within(row).getByRole('button', { name: 'Cancel' }));
    fireEvent.click(within(row).getByRole('button', { name: yes }));
    await waitFor(() => {
      expect(writes(calls).map((c) => `${c.method} ${c.path}`)).toEqual([`${method} ${path}`]);
    });
  });

  it('Cancel and Escape send nothing and put focus back on the ⋯ button', async () => {
    const calls = serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Remove from server…');
    fireEvent.click(within(row).getByRole('button', { name: 'Cancel' }));
    expect(within(row).queryByRole('group')).toBeNull();
    expect(document.activeElement).toBe(within(row).getByRole('button', { name: 'Actions for member-1' }));
    await choose(row, 'member-1', 'Disable sign-in…');
    fireEvent.keyDown(within(row).getByRole('button', { name: 'Cancel' }), { key: 'Escape' });
    expect(within(row).queryByRole('group')).toBeNull();
    expect(document.activeElement).toBe(within(row).getByRole('button', { name: 'Actions for member-1' }));
    expect(writes(calls)).toEqual([]);
  });

  it('sends once however often Yes is pressed', async () => {
    const calls = serve(owner, { routes: { 'POST /v1/principals/3/disable': noContent } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Disable sign-in…');
    const yes = within(row).getByRole('button', { name: 'Yes, disable' });
    fireEvent.click(yes);
    fireEvent.click(yes);
    await later10();
    expect(sent(calls, 'POST', /disable$/).length).toBe(1);
  });

  it('removes a person from the list once the server has said so, and says it', async () => {
    let gone = false;
    serve(owner, {
      members: () => (gone ? everyone.filter((p) => p.id !== 3) : everyone),
      routes: {
        'POST /v1/principals/3/hide': () => {
          gone = true;
          return json({ principal: { id: 3, kind: 'human', name: 'member-1', role: 'member', created_at: at, hidden_at: at, live_credentials: 0 } });
        },
      },
    });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Remove from server…');
    fireEvent.click(within(row).getByRole('button', { name: 'Yes, remove' }));
    expect(await within(region).findByText('member-1 is removed from the server.')).toBeTruthy();
    await waitFor(() => {
      expect(Array.from(region.querySelectorAll('.community-name')).map((e) => e.textContent)).not.toContain('member-1');
    });
  });
});

describe('what acts as a person shows its secret once', () => {
  it('makes a link, hides it until Reveal and forgets it at Done', async () => {
    const calls = serve(owner, {
      routes: {
        'POST /v1/principals/3/sign-in-links': () =>
          json(
            { sign_in_link: { id: 7, principal_id: 3, created_by: 1, purpose: 'reset', created_at: at, expires_at: later }, code: secretCode },
            201,
          ),
      },
    });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Make a link that signs in as this person…');
    // The default is the milder one: a password reset, not signing in as them.
    expect(within(row).getByLabelText<HTMLSelectElement>('What the link does').value).toBe('reset');
    fireEvent.change(within(row).getByLabelText('What the link does'), { target: { value: 'sign_in' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Make link' }));
    await waitFor(() => {
      expect(sent(calls, 'POST', /sign-in-links$/).map((c) => c.body)).toEqual([{ purpose: 'sign_in' }]);
    });
    const secret = await within(row).findByRole('group', { name: 'Link that signs in as member-1' });
    expect(document.body.textContent).not.toContain(secretCode);
    fireEvent.click(within(secret).getByRole('button', { name: 'Reveal' }));
    const shown = within(secret).getByRole<HTMLInputElement>('textbox');
    expect(shown.value).toContain(encodeURIComponent(secretCode));
    fireEvent.click(within(secret).getByRole('button', { name: 'Done' }));
    expect(row.querySelector('.secret-once')).toBeNull();
    expect(document.body.innerHTML).not.toContain(secretCode);
  });

  it('issues a credential and binds a hook, each with a label, and shows the token once', async () => {
    const calls = serve(owner, {
      routes: {
        'POST /v1/principals/4/credentials': () => json({ credential: { id: 9, principal_id: 4, label: 'ci', created_at: at }, token: secretToken }, 201),
        'POST /v1/hooks': () => json({ id: 3, token: secretToken }, 201),
      },
    });
    const region = await open();
    const row = rowOf(region, 'builder');
    await choose(row, 'builder', 'Make a credential that acts as this agent…');
    const submit = within(row).getByRole('button', { name: 'Make credential' });
    expect((submit as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(within(row).getByLabelText('Name for it'), { target: { value: 'ci' } });
    fireEvent.click(submit);
    await waitFor(() => {
      expect(sent(calls, 'POST', /credentials$/).map((c) => c.body)).toEqual([{ label: 'ci' }]);
    });
    fireEvent.click(await within(row).findByRole('button', { name: 'Done' }));
    expect(document.body.textContent).not.toContain(secretToken);

    await choose(row, 'builder', 'Make a hook that posts as this agent…');
    await waitFor(() => {
      expect(within(within(row).getByLabelText('Channel')).getAllByRole('option').length).toBeGreaterThan(2);
    });
    fireEvent.change(within(row).getByLabelText('Channel'), { target: { value: 'channel-a' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Make hook' }));
    await waitFor(() => {
      expect(sent(calls, 'POST', /\/v1\/hooks$/).map((c) => c.body)).toEqual([{ channel: 'channel-a', principal: 4 }]);
    });
    expect(await within(row).findByRole('group', { name: 'Hook that posts as builder' })).toBeTruthy();
    expect(document.body.textContent).not.toContain(secretToken);
  });
});

describe('refusals', () => {
  it('shows a refusal by its permission, never the server’s message', async () => {
    serve(owner, { routes: { 'PUT /v1/principals/3/invite-permission': () => refusal(403, 'permission_denied', { permission: 'manage_members' }) } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Stop member links');
    expect(await within(row).findByText('You need the permission to manage members for that. Member links were not changed.')).toBeTruthy();
    expect(region.textContent).not.toContain(leak);
  });

  it.each([
    { code: 'last_operator', status: 409, line: 'The server keeps at least one operator. Nothing was changed.' },
    { code: 'principal_not_found', status: 404, line: 'That person is gone, or you can no longer see them. Nothing was changed.' },
    { code: 'forbidden', status: 403, line: 'The server does not allow you to do that. Nothing was changed.' },
  ])('says $code in the page’s own words', async ({ code, status, line }) => {
    serve(owner, { routes: { 'POST /v1/principals/3/disable': () => refusal(status, code) } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Disable sign-in…');
    fireEvent.click(within(row).getByRole('button', { name: 'Yes, disable' }));
    expect(await within(row).findByText(line)).toBeTruthy();
    expect(region.textContent).not.toContain(leak);
  });

  it('says plainly that nothing was reached when the network fails', async () => {
    serve(owner, { routes: { 'PUT /v1/principals/3/voice-permission': () => Promise.reject(new TypeError('offline')) } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Turn voice off');
    expect(await within(row).findByText('Cannot reach the server. Voice was not changed.')).toBeTruthy();
  });
});

describe('the ways in and out of the menu', () => {
  it('opens from the ⋯ button, a right-click on the row and a long press, and the native menu is kept elsewhere', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    // Right-click on the row: the native menu is suppressed, ours opens.
    expect(fireEvent.contextMenu(within(row).getByText('member-1'))).toBe(false);
    expect(await within(row).findByRole('menu')).toBeTruthy();
    fireEvent.keyDown(within(row).getByRole('menu'), { key: 'Escape' });
    expect(within(row).queryByRole('menu')).toBeNull();
    // Right-click outside any row: the browser's own menu stays.
    expect(fireEvent.contextMenu(within(region).getByRole('heading', { name: 'Members' }))).toBe(true);
    expect(fireEvent.contextMenu(region)).toBe(true);
    // A row with no actions (the viewer's own) keeps it too.
    expect(fireEvent.contextMenu(within(rowOf(region, 'owner')).getByText('owner'))).toBe(true);
    // A long press opens it.
    const target = within(row).getByText('member-1');
    fireEvent.touchStart(target, { touches: [{ clientX: 5, clientY: 5 }] });
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(await within(row).findByRole('menu')).toBeTruthy();
  });

  it('opens from the keyboard: the context-menu key on the focused ⋯ button', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    const button = within(row).getByRole('button', { name: 'Actions for member-1' });
    button.focus();
    // The context-menu key and Shift+F10 reach the page as a contextmenu event on the focused control.
    expect(fireEvent.contextMenu(button)).toBe(false);
    expect(await within(row).findByRole('menu')).toBeTruthy();
    expect(document.activeElement).toBe(within(row).getAllByRole('menuitem')[0]);
  });

  it('does not open on a touch that moved, as a scroll does', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    const target = within(row).getByText('member-1');
    fireEvent.touchStart(target, { touches: [{ clientX: 5, clientY: 5 }] });
    fireEvent.touchMove(target, { touches: [{ clientX: 5, clientY: 60 }] });
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(within(row).queryByRole('menu')).toBeNull();
  });

  it('moves with the arrow keys, closes on Escape with focus back on the button, and on an outside press', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    const button = within(row).getByRole('button', { name: 'Actions for member-1' });
    expect(button.getAttribute('aria-haspopup')).toBe('menu');
    const m = await menu(row, 'member-1');
    expect(button.getAttribute('aria-expanded')).toBe('true');
    const items = within(m).getAllByRole('menuitem');
    expect(document.activeElement).toBe(items[0]);
    fireEvent.keyDown(m, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(items[1]);
    fireEvent.keyDown(m, { key: 'End' });
    expect(document.activeElement).toBe(items[items.length - 1]);
    fireEvent.keyDown(m, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(items[0]);
    fireEvent.keyDown(m, { key: 'Escape' });
    expect(within(row).queryByRole('menu')).toBeNull();
    expect(document.activeElement).toBe(button);
    await menu(row, 'member-1');
    fireEvent.pointerDown(document.body);
    expect(within(row).queryByRole('menu')).toBeNull();
  });

  it('closes after an action is chosen', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Disable sign-in…');
    expect(within(row).queryByRole('menu')).toBeNull();
  });
});

describe('a grant that goes while a question is open', () => {
  it('takes the question away and sends nothing', async () => {
    let who: WhoAmIResponseV1 = operator;
    const calls = serve(() => who);
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Remove from server…');
    const yes = within(row).getByRole('button', { name: 'Yes, remove' });
    who = { ...operator, permissions: operatorRights.filter((p) => p !== 'manage_members' && p !== 'act_as_members') };
    await tick();
    await waitFor(() => {
      expect(within(row).queryByRole('group', { name: /Remove/ })).toBeNull();
    });
    fireEvent.click(yes);
    await later10();
    expect(sent(calls, 'POST', /hide$/)).toEqual([]);
    expect(await within(region).findByText('You can no longer do that. Nothing was sent.')).toBeTruthy();
  });
});

describe('Add a person', () => {
  it('is offered with make_member_links, closes Settings and opens the member-link panel', async () => {
    serve(owner);
    const region = await open();
    fireEvent.click(within(region).getByRole('button', { name: 'Add a person' }));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'Members' })).toBeNull();
    });
    // The member-link panel is open, with the open channel ticked, and focus is in it.
    const invites = await screen.findByRole('region', { name: 'Invites' });
    await waitFor(() => {
      expect(within(invites).getByRole('button', { name: 'Invite someone' }).getAttribute('aria-expanded')).toBe('true');
    });
    expect(within(invites).getByRole<HTMLInputElement>('checkbox', { name: /channel-a/ }).checked).toBe(true);
    await waitFor(() => {
      expect(invites.contains(document.activeElement)).toBe(true);
    });
  });

  it('is not offered without make_member_links', async () => {
    serve({ ...operator, permissions: operatorRights.filter((p) => p !== 'make_member_links') });
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    expect(within(region).queryByRole('button', { name: 'Add a person' })).toBeNull();
  });
});

describe('the phone', () => {
  it('lists Members among the sections, opens it as a page, and offers the actions from the ⋯ button', async () => {
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
    fireEvent.click(await screen.findByRole('button', { name: 'Members' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    const m = await menu(rowOf(region, 'member-1'), 'member-1');
    expect(within(m).getAllByRole('menuitem').length).toBeGreaterThan(5);
    // A destructive item asks in the page, with a button that has room for a finger.
    fireEvent.click(within(m).getByRole('menuitem', { name: 'Remove from server…' }));
    expect(within(rowOf(region, 'member-1')).getByRole('button', { name: 'Yes, remove' })).toBeTruthy();
  });
});


describe('adding to a channel never changes who decides approvals', () => {
  const inChannel = (id: number) => ({ members: [{ principal_id: id, name: 'member-1', kind: 'human', created_at: at, can_decide: true }] });

  async function submit(row: HTMLElement) {
    await choose(row, 'member-1', 'Add to a channel…');
    const select = within(row).getByLabelText('Add member-1 to');
    await waitFor(() => {
      expect(within(select).getAllByRole('option').length).toBeGreaterThan(2);
    });
    fireEvent.change(select, { target: { value: 'channel-p' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Add to channel' }));
  }

  it('sends no write for someone already in the channel, and says so', async () => {
    const calls = serve(owner, { routes: { 'GET /v1/channels/channel-p/members': () => json(inChannel(3)), 'PUT /v1/channels/channel-p/members/3': noContent } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await submit(row);
    expect(await within(row).findByText('member-1 is already in that channel. Nothing was sent.')).toBeTruthy();
    expect(sent(calls, 'PUT', /members/)).toEqual([]);
  });

  it('sends nothing when the channel’s people cannot be read first', async () => {
    const calls = serve(owner, { routes: { 'GET /v1/channels/channel-p/members': () => refusal(500, 'internal_error'), 'PUT /v1/channels/channel-p/members/3': noContent } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await submit(row);
    expect(await within(row).findByText("The channel's people could not be read first. Nothing was sent.")).toBeTruthy();
    expect(sent(calls, 'PUT', /members/)).toEqual([]);
  });
});

describe('after an action', () => {
  it('puts focus back on the row’s ⋯ button', async () => {
    serve(owner, { routes: { 'PUT /v1/principals/3/invite-permission': noContent } });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Stop member links');
    await waitFor(() => {
      expect(document.activeElement).toBe(within(rowOf(region, 'member-1')).getByRole('button', { name: 'Actions for member-1' }));
    });
  });

  it('puts focus on the list’s status line when the row is gone', async () => {
    let gone = false;
    serve(owner, {
      members: () => (gone ? everyone.filter((p) => p.id !== 3) : everyone),
      routes: {
        'POST /v1/principals/3/hide': () => {
          gone = true;
          return json({ principal: { id: 3, kind: 'human', name: 'member-1', role: 'member', created_at: at, hidden_at: at, live_credentials: 0 } });
        },
      },
    });
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Remove from server…');
    fireEvent.click(within(row).getByRole('button', { name: 'Yes, remove' }));
    const line = await within(region).findByText('member-1 is removed from the server.');
    await waitFor(() => {
      expect(document.activeElement).toBe(line);
    });
  });
});

describe('the owner mark', () => {
  it('shows its own warning, and its own button, for Owner only', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Change role…');
    const warning = 'An owner holds every permission, is never braked, and only another owner can change or remove them.';
    expect(within(row).queryByText(warning)).toBeNull();
    fireEvent.click(within(row).getByRole('radio', { name: 'Operator' }));
    expect(within(row).queryByText(warning)).toBeNull();
    fireEvent.click(within(row).getByRole('radio', { name: 'Owner' }));
    expect(within(row).getByText(warning)).toBeTruthy();
    expect(within(row).getByRole('button', { name: 'Make an owner, who holds every permission' })).toBeTruthy();
    fireEvent.click(within(row).getByRole('radio', { name: 'Member' }));
    expect(within(row).queryByText(warning)).toBeNull();
  });
});

describe('the secret panel', () => {
  const credentialRoute = (held?: Promise<void>): Route => async () => {
    if (held !== undefined) await held;
    return json({ credential: { id: 9, principal_id: 4, label: 'ci', created_at: at }, token: secretToken }, 201);
  };

  async function makeCredential(row: HTMLElement) {
    await choose(row, 'builder', 'Make a credential that acts as this agent…');
    fireEvent.change(within(row).getByLabelText('Name for it'), { target: { value: 'ci' } });
    fireEvent.click(within(row).getByRole('button', { name: 'Make credential' }));
  }

  it('keeps Escape to itself, so Settings stays open and the secret is not lost', async () => {
    serve(owner, { routes: { 'POST /v1/principals/4/credentials': credentialRoute() } });
    const region = await open();
    const row = rowOf(region, 'builder');
    await makeCredential(row);
    const secret = await within(row).findByRole('group', { name: 'Credential that acts as builder' });
    fireEvent.keyDown(within(secret).getByRole('button', { name: 'Reveal' }), { key: 'Escape' });
    // Settings is still open (Escape would close it) and the secret is still there.
    expect(screen.getByRole('dialog', { name: 'Settings' })).toBeTruthy();
    expect(within(row).getByRole('group', { name: 'Credential that acts as builder' })).toBeTruthy();
  });

  it('never shows a secret that arrives after its row is gone, and says what became of it', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    let gone = false;
    serve(owner, { members: () => (gone ? everyone.filter((p) => p.id !== 4) : everyone), routes: { 'POST /v1/principals/4/credentials': credentialRoute(held) } });
    const region = await open();
    const row = rowOf(region, 'builder');
    await makeCredential(row);
    // The agent leaves the list while the answer is on its way.
    gone = true;
    fireEvent.click(within(region).getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(document.body.contains(row)).toBe(false);
    });
    release();
    expect(await within(region).findByText('A credential for builder was made but could not be shown; revoke it under Sign-ins.')).toBeTruthy();
    expect(document.body.textContent).not.toContain(secretToken);
  });
});

describe('the hook form’s channels', () => {
  it('lists the viewer’s own channels, not the directory, without manage_channels', async () => {
    const calls = serve({ ...operator, permissions: operatorRights.filter((p) => p !== 'manage_channels') });
    window.sessionStorage.setItem(key, 'conch_operator_session_token');
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    const region = await screen.findByRole('region', { name: 'Members' });
    await within(region).findByRole('list', { name: 'People' });
    const row = rowOf(region, 'builder');
    await choose(row, 'builder', 'Make a hook that posts as this agent…');
    await waitFor(() => {
      expect(within(within(row).getByLabelText('Channel')).getAllByRole('option').map((o) => o.textContent)).toEqual(['Choose a channel', 'channel-a']);
    });
    expect(calls.some((c) => c.path === '/v1/directory/channels')).toBe(false);
  });
});

describe('the row’s status line', () => {
  it('is drawn only for a row with actions or a line', async () => {
    serve(owner);
    const region = await open();
    expect(rowOf(region, 'owner').querySelector('[role="status"]')).toBeNull();
    expect(rowOf(region, 'member-1').querySelector('[role="status"]')).not.toBeNull();
  });
});

describe('a secondary click inside an open question', () => {
  it('does not open the menu again', async () => {
    serve(owner);
    const region = await open();
    const row = rowOf(region, 'member-1');
    await choose(row, 'member-1', 'Remove from server…');
    expect(fireEvent.contextMenu(within(row).getByRole('button', { name: 'Cancel' }))).toBe(true);
    expect(within(row).queryByRole('menu')).toBeNull();
  });
});
