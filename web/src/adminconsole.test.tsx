// The administration console and its Server section (ADR-014 design §10,
// issue #1018), against a fake conchd (fetch) and a fake WebSocket: who is
// shown the area and what a member sees instead, each Server control's
// enabled, disabled and refused states, the setup card's three choices, the
// recent proof, the invite list with other_channels, an unknown permission,
// and the live signal. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { AdminConsole } from './AdminConsole';
import { InviteSettings } from './InviteSettings';
import { canDo, memberLine, permissionName, refusalLine } from './admin/permissions';
import { presetChanges } from './admin/ServerSection';
import { channelsLine } from './admin/InvitesList';
import { parseEventFrame } from './agents/frames';
import type { Permission, PermissionsResponseV1 } from './schema/permission';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const ownerToken = 'conch_owner_session_token';
const operatorToken = 'conch_operator_session_token';
const memberToken = 'conch_member_secret_token';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const password = 'the owner passphrase here';

const memberRights: Permission[] = ['create_public_channels', 'create_private_channels', 'make_member_links', 'see_member_directory', 'use_voice', 'share_screen'];
const personalRights: Permission[] = ['use_voice', 'share_screen'];
const operatorRights: Permission[] = [...memberRights, 'manage_members', 'act_as_members', 'manage_channels'];
const auth = { scope: 'full', password_set: true, totp_enrolled: false, setup_complete: true, expires_at: later, idle_expires_at: later } as const;

const owner: WhoAmIResponseV1 = {
  id: 1,
  kind: 'human',
  name: 'owner',
  role: 'operator',
  owner: true,
  identity_class: 'operator',
  may_invite: true,
  permissions: [...operatorRights, 'change_server_settings', 'read_audit_log'],
  permissions_revision: 1,
  auth,
};
// An operator without change_server_settings (the default for operators).
const operator: WhoAmIResponseV1 = { ...owner, id: 2, name: 'operator-1', owner: false, permissions: operatorRights };
// An operator the owner gave change_server_settings.
const settingsOperator: WhoAmIResponseV1 = { ...operator, permissions: [...operatorRights, 'change_server_settings'] };
const member: WhoAmIResponseV1 = {
  id: 3,
  kind: 'human',
  name: 'member-1',
  role: 'member',
  identity_class: 'member',
  permissions: ['create_public_channels', 'create_private_channels', 'make_member_links', 'use_voice'],
  permissions_revision: 1,
  auth: { scope: 'full', password_set: true, setup_complete: true },
};

const channelList = { channels: [{ id: 1, name: 'channel-a', created_at: at }] };

const joinSettings = {
  settings: {
    open_joining: 'off',
    guests_enabled: false,
    joining_paused: false,
    join_voice: true,
    max_guests: 10,
    max_guests_per_address: 3,
    guest_idle_seconds: 3600,
    guest_absolute_seconds: 86400,
    open_channels: ['channel-a'],
    open_signup: false,
    member_link_default_expiry_seconds: 604800,
  },
  server_name: 'conch.example',
  live_guests: 0,
  warnings: [],
};

function rows(memberOn: readonly Permission[]) {
  const all: Permission[] = [...operatorRights, 'change_server_settings', 'read_audit_log'];
  return all.map((permission) => ({ permission, member: memberOn.includes(permission), operator: !['change_server_settings', 'read_audit_log'].includes(permission) }));
}

function permissionsDoc(revision: number, memberOn: readonly Permission[] = memberRights): PermissionsResponseV1 {
  return {
    revision,
    rows: rows(memberOn),
    presets: [
      // pkg/schema PresetMemberPermissions, as the server sends it.
      { preset: 'personal', member: personalRights },
      { preset: 'group', member: memberRights },
    ],
    editable: ['member'],
    warnings: [],
    updated_at: at,
  };
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const refusal = (status: number, code: string, message = 'refused', more: Record<string, unknown> = {}) => json({ code, message, ...more }, status);

interface Call {
  readonly method: string;
  readonly path: string;
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

function serve(who: WhoAmIResponseV1 | (() => WhoAmIResponseV1), routes: Record<string, Route> = {}): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const path = new URL(raw, origin).pathname;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      const call = { method, path, body };
      calls.push(call);
      const route = routes[`${method} ${path}`];
      if (route !== undefined) return route(call);
      if (path === '/v1/whoami') return json(typeof who === 'function' ? who() : who);
      if (path === '/v1/channels') return json(channelList);
      if (path === '/v1/ws-tickets') return json({ ticket: ticketValue, expires_at: at }, 201);
      if (path === '/v1/join/settings' && method === 'GET') return json(joinSettings);
      if (path === '/v1/invites/settings' && method === 'GET') return json({ paused: false });
      if (path === '/v2/invites' && method === 'GET') return json({ invites: [] });
      if (path === '/v1/guests' && method === 'GET') return json({ guests: [] });
      if (path === '/v1/sessions' && method === 'POST') {
        return json({ schema: 'conch.session.v1', id: 100, principal_id: 3, kind: 'human', name: 'member-1', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201);
      }
      if (path === '/v1/read-state') return json({ read_state: [] });
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[2] === 'members') return json({ members: [] });
      if (m?.[2] === 'nets') return json({ nets: [] });
      if (m?.[2] === 'messages') return json({ messages: [] });
      return json({}, 404);
    }),
  );
  return calls;
}

async function signedIn(token: string) {
  window.sessionStorage.setItem(key, token);
  const view = render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  return view;
}

// joining opens the Server section's Joining panel.
async function joining(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole('button', { name: 'Joining' }));
  const region = screen.getByRole('region', { name: 'Joining' });
  await within(region).findByLabelText(/Open sign-up/);
  return region;
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
  FakeSocket.all = [];
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('who is shown the console', () => {
  it('shows a member a one-line summary of their rights from whoami, and no console', async () => {
    serve(member);
    await signedIn(memberToken);
    expect(await screen.findByText('You can create channels and make member links. Voice is on.')).toBeDefined();
    expect(screen.queryByRole('heading', { name: 'Server administration' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Joining' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Reports' })).toBeNull();
  });

  it('shows an operator and the owner the console with the sections that exist, Server first', async () => {
    serve(operator);
    await signedIn(operatorToken);
    expect(await screen.findByRole('heading', { name: 'Server administration' })).toBeDefined();
    expect(await screen.findByRole('button', { name: 'Joining' })).toBeDefined();
    // Reports and Roles and permissions are built; Members and Channels are not,
    // and so are not listed.
    expect(screen.getByRole('button', { name: 'Reports' })).toBeDefined();
    const console_ = screen.getByRole('heading', { name: 'Server administration' }).parentElement;
    if (console_ === null) throw new Error('no console');
    for (const name of ['Members', 'Channels']) expect(within(console_).queryByRole('button', { name })).toBeNull();
    cleanup();
    serve(owner);
    await signedIn(ownerToken);
    expect(await screen.findByRole('heading', { name: 'Server administration' })).toBeDefined();
  });
});

describe('the Server section', () => {
  it('shows the server name as read-only text with the host line', async () => {
    serve(owner);
    await signedIn(ownerToken);
    const group = await screen.findByRole('group', { name: 'Server settings' });
    expect(await within(group).findByText('conch.example')).toBeDefined();
    expect(within(group).getByText(/Set on the server’s host/).textContent).toContain('--server-name');
    expect(within(group).queryByLabelText(/server name/i)).toBeNull();
  });

  it('disables every write, with the reason, for an operator without change_server_settings', async () => {
    serve(operator);
    await signedIn(operatorToken);
    const region = await joining();
    expect(within(region).getByText('Needs the permission to change server settings.')).toBeDefined();
    expect(within(region).getByLabelText<HTMLInputElement>(/Open sign-up/).disabled).toBe(true);
    expect(within(region).getByLabelText<HTMLInputElement>('Pause joining from the address').disabled).toBe(true);
    expect(within(region).getByLabelText<HTMLInputElement>('Default link expiry (hours, 0 for none)').disabled).toBe(true);
    expect(within(region).getByLabelText<HTMLInputElement>('#channel-a').matches(':disabled')).toBe(true);
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Save joining' }).disabled).toBe(true);
    // Pausing every invite is manage_members's, which this operator holds.
    expect(within(region).getByLabelText<HTMLInputElement>(/^Pause all invites/).disabled).toBe(false);
  });

  it('disables the invite brake, with the reason, for a caller without manage_members', async () => {
    serve({ ...settingsOperator, permissions: [...memberRights, 'change_server_settings'] });
    await signedIn(operatorToken);
    const region = await joining();
    expect(within(region).getByLabelText<HTMLInputElement>(/^Pause all invites/).disabled).toBe(true);
    expect(within(region).getByText('Needs the permission to manage members.')).toBeDefined();
    expect(within(region).getByLabelText<HTMLInputElement>(/Open sign-up/).disabled).toBe(false);
  });

  it('saves an unrelated change when the server’s default link expiry is not a whole number of hours, leaving the expiry untouched', async () => {
    const calls = serve(settingsOperator, {
      'GET /v1/join/settings': () => json({ ...joinSettings, settings: { ...joinSettings.settings, member_link_default_expiry_seconds: 5400 } }),
      'PUT /v1/join/settings': () => json({ ...joinSettings, settings: { ...joinSettings.settings, joining_paused: true, member_link_default_expiry_seconds: 5400 } }),
    });
    await signedIn(operatorToken);
    const region = await joining();
    expect(within(region).getByLabelText<HTMLInputElement>('Default link expiry (hours, 0 for none)').value).toBe('1.5');
    fireEvent.click(within(region).getByLabelText('Pause joining from the address'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    // A patch: the field is not named, so the server keeps its 5400 s.
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ joining_paused: true });
    // An edited one is still checked.
    fireEvent.change(within(region).getByLabelText('Default link expiry (hours, 0 for none)'), { target: { value: '1.25' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('The default link expiry is a whole number of hours, or 0 for none.')).toBeDefined();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
  });

  it('sends only what changed to PUT /v1/join/settings for an operator who holds change_server_settings', async () => {
    const calls = serve(settingsOperator, {
      'PUT /v1/join/settings': () => json({ ...joinSettings, settings: { ...joinSettings.settings, open_signup: true, member_link_default_expiry_seconds: 86400 } }),
    });
    await signedIn(operatorToken);
    const region = await joining();
    fireEvent.click(within(region).getByLabelText(/Open sign-up/));
    fireEvent.change(within(region).getByLabelText('Default link expiry (hours, 0 for none)'), { target: { value: '24' } });
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ open_signup: true, member_link_default_expiry_seconds: 86400 });
  });

  it('shows no_default_channel, and open_signup_on from the permissions document', async () => {
    serve(owner, {
      'GET /v1/join/settings': () => json({ ...joinSettings, warnings: ['no_default_channel'] }),
      'GET /v1/permissions': () => json({ ...permissionsDoc(2), warnings: ['open_signup_on'] }),
    });
    await signedIn(ownerToken);
    expect(await screen.findByText(/lets anyone make an account/)).toBeDefined();
    await joining();
    expect(screen.getByText(/No channel is a default channel/)).toBeDefined();
  });

  it('shows a permission_denied refusal by its code and the permission it names, not by its message', async () => {
    serve(settingsOperator, {
      'PUT /v1/join/settings': () => refusal(403, 'permission_denied', 'some wording that must not be parsed', { permission: 'change_server_settings' }),
    });
    await signedIn(operatorToken);
    const region = await joining();
    fireEvent.click(within(region).getByLabelText('Pause joining from the address'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    expect(await within(region).findByText('You need the permission to change server settings for that. Nothing was changed.')).toBeDefined();
    expect(within(region).queryByText(/some wording/)).toBeNull();
  });

  it('opens the Reauth overlay for a reauth_required answer, then sends the change once more', async () => {
    let proved = false;
    const calls = serve(owner, {
      'PUT /v1/join/settings': () => (proved ? json({ ...joinSettings, settings: { ...joinSettings.settings, joining_paused: true } }) : refusal(403, 'reauth_required')),
      'POST /v1/reauth': () => {
        proved = true;
        return json({ until: later });
      },
    });
    await signedIn(ownerToken);
    const region = await joining();
    fireEvent.click(within(region).getByLabelText('Pause joining from the address'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(calls.filter((c) => c.method === 'PUT' && c.path === '/v1/join/settings').map((c) => c.body)).toEqual([{ joining_paused: true }, { joining_paused: true }]);
  });
});

describe('refusals use the page’s words, never the server’s', () => {
  const hostile = '<img src=x onerror=alert(1)> pay me at evil.example now';

  it('does not show the message of a join_policy_conflict or an invalid_request', async () => {
    for (const code of ['join_policy_conflict', 'invalid_request', 'permissions_conflict'] as const) {
      serve(settingsOperator, { 'PUT /v1/join/settings': () => refusal(code === 'permissions_conflict' ? 409 : 409, code, hostile) });
      const { container } = await signedIn(operatorToken);
      const region = await joining();
      fireEvent.click(within(region).getByLabelText('Pause joining from the address'));
      fireEvent.click(within(region).getByRole('button', { name: 'Save joining' }));
      await within(region).findByText(/Nothing was changed\.$/);
      expect(container.innerHTML).not.toContain('evil.example');
      expect(container.querySelector('img')).toBeNull();
      cleanup();
      window.sessionStorage.clear();
    }
  });

  it('does not show the message of a refused preset', async () => {
    serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(1)),
      'PUT /v1/permissions': () => refusal(409, 'permissions_conflict', hostile),
    });
    const { container } = await signedIn(ownerToken);
    fireEvent.click(await screen.findByRole('button', { name: 'Personal server' }));
    fireEvent.click(within(screen.getByRole('group', { name: 'Apply Personal server' })).getByRole('button', { name: 'Apply Personal server' }));
    expect(await screen.findByText('That change would break a rule of the permissions. Nothing was changed.')).toBeDefined();
    expect(container.innerHTML).not.toContain('evil.example');
  });
});

describe('the setup card', () => {
  async function card(routes: Record<string, Route> = {}, revision = 1) {
    const calls = serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(revision)), ...routes });
    await signedIn(ownerToken);
    return calls;
  }

  it('is not shown once the revision has moved', async () => {
    await card({}, 2);
    await screen.findByRole('group', { name: 'Server settings' });
    await screen.findByText(/Members see:/);
    expect(screen.queryByText('How will this server be used?')).toBeNull();
  });

  it('shows what Personal changes, applies it against the revision read, and disappears when the revision moves', async () => {
    const calls = await card({ 'PUT /v1/permissions': () => json(permissionsDoc(2, personalRights)) });
    expect(await screen.findByText('How will this server be used?')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Personal server' }));
    const ask = screen.getByRole('group', { name: 'Apply Personal server' });
    expect(ask.textContent).toContain('Members lose: create public channels, create private channels, make member links, see everyone on the server.');
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
    fireEvent.click(within(ask).getByRole('button', { name: 'Apply Personal server' }));
    await waitFor(() => {
      expect(screen.queryByText('How will this server be used?')).toBeNull();
    });
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ revision: 1, preset: 'personal' });
    // The summary lines follow the document.
    expect(await screen.findByText(/only people they share a channel with/)).toBeDefined();
  });

  it('applies Group, which the members already match: nothing changes, the revision stays, and the card is put away for good', async () => {
    const calls = await card({ 'PUT /v1/permissions': () => json(permissionsDoc(1)) });
    fireEvent.click(await screen.findByRole('button', { name: 'Group server' }));
    const ask = screen.getByRole('group', { name: 'Apply Group server' });
    expect(ask.textContent).toContain('already matches what members may do, so nothing changes.');
    expect(ask.textContent).not.toContain('record');
    fireEvent.click(within(ask).getByRole('button', { name: 'Apply Group server' }));
    await waitFor(() => {
      expect(screen.queryByText('How will this server be used?')).toBeNull();
    });
    expect(calls.find((c) => c.method === 'PUT')?.body).toEqual({ revision: 1, preset: 'group' });
    // The server is still at revision 1, and a fresh page does not ask again.
    cleanup();
    await card({}, 1);
    await screen.findByText(/Members see:/);
    expect(screen.queryByText('How will this server be used?')).toBeNull();
  });

  it('applies nothing when the question is cancelled', async () => {
    const calls = await card();
    fireEvent.click(await screen.findByRole('button', { name: 'Group server' }));
    fireEvent.click(within(screen.getByRole('group', { name: 'Apply Group server' })).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('group', { name: 'Apply Group server' })).toBeNull();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
    expect(screen.getByText('How will this server be used?')).toBeDefined();
  });

  it('dismisses with Decide later, for this browser, and sends nothing', async () => {
    const calls = await card();
    fireEvent.click(await screen.findByRole('button', { name: 'Decide later' }));
    expect(screen.queryByText('How will this server be used?')).toBeNull();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
    cleanup();
    await card();
    await screen.findByText(/Members see:/);
    expect(screen.queryByText('How will this server be used?')).toBeNull();
  });

  it('closes the confirmation when change_server_settings goes, and sends no write, even after it returns', async () => {
    let who: WhoAmIResponseV1 = owner;
    const calls = serve(() => who, { 'GET /v1/permissions': () => json(permissionsDoc(1)) });
    window.sessionStorage.setItem(key, ownerToken);
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Personal server' }));
    expect(screen.getByRole('group', { name: 'Apply Personal server' })).toBeDefined();
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    const events = FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'));
    // Another owner clears this one's owner mark.
    who = { ...operator, id: owner.id };
    events?.send({ schema: 'conch.permissions_changed.v1' });
    await waitFor(() => {
      expect(screen.queryByRole('group', { name: 'Apply Personal server' })).toBeNull();
    });
    expect(screen.queryByRole('button', { name: 'Apply Personal server' })).toBeNull();
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Personal server' }).disabled).toBe(true);
    // The permission comes back: the question is not still pending.
    who = owner;
    events?.send({ schema: 'conch.permissions_changed.v1' });
    await waitFor(() => {
      expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Personal server' }).disabled).toBe(false);
    });
    expect(screen.queryByRole('group', { name: 'Apply Personal server' })).toBeNull();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
  });

  it('offers the presets, disabled with the reason, to an operator without change_server_settings', async () => {
    serve(operator, { 'GET /v1/permissions': () => json(permissionsDoc(1)) });
    await signedIn(operatorToken);
    const personal = await screen.findByRole<HTMLButtonElement>('button', { name: 'Personal server' });
    expect(personal.disabled).toBe(true);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Group server' }).disabled).toBe(true);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Decide later' }).disabled).toBe(false);
    expect(screen.getAllByText('Needs the permission to change server settings.').length).toBeGreaterThan(0);
  });

  it('says a stale revision plainly and reads the document again', async () => {
    let reads = 0;
    await card({
      'GET /v1/permissions': () => json(permissionsDoc(++reads === 1 ? 1 : 3)),
      'PUT /v1/permissions': () => refusal(409, 'permissions_stale', 'stale'),
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Group server' }));
    fireEvent.click(within(screen.getByRole('group', { name: 'Apply Group server' })).getByRole('button', { name: 'Apply Group server' }));
    expect(await screen.findByText('The permissions changed since this page read them. Nothing was changed.')).toBeDefined();
    await waitFor(() => {
      expect(screen.queryByText('How will this server be used?')).toBeNull();
    });
  });
});

describe('the invites', () => {
  const link = {
    id: 7,
    kind: 'member',
    created_by: 5,
    created_by_name: 'operator-2',
    created_at: at,
    expires_at: later,
    uses: 2,
    joiners: 2,
    channels: [{ id: 1, name: 'channel-a' }],
    other_channels: 2,
    credential_lifetime_seconds: 0,
  };

  it('lists each link with its maker, its joiners and the other channels as a count, and closes one', async () => {
    let closed = false;
    const calls = serve(owner, {
      'GET /v2/invites': () => json({ invites: closed ? [{ ...link, closed_at: at }] : [link] }),
      'GET /v2/invites/7/joiners': () => json({ joiners: [{ id: 30, name: 'member-1', created_at: at, disabled: false }] }),
      'DELETE /v1/invites/7': () => {
        closed = true;
        return new Response(null, { status: 204 });
      },
    });
    await signedIn(ownerToken);
    const region = await joining();
    const list = await within(region).findByRole('list', { name: 'Member links' });
    expect(list.textContent).toContain('made by operator-2');
    expect(list.textContent).toContain('2 joiners');
    expect(list.textContent).toContain('#channel-a and 2 other channels');
    fireEvent.click(within(list).getByRole('button', { name: 'Show joiners of link 7' }));
    expect(await within(list).findByRole('list', { name: 'Joiners of link 7' })).toBeDefined();
    fireEvent.click(within(list).getByRole('button', { name: 'Close link 7' }));
    expect(await within(region).findByText('Link #7 is closed.')).toBeDefined();
    expect(calls.some((c) => c.method === 'DELETE' && c.path === '/v1/invites/7')).toBe(true);
  });

  it('shows a server refusal of a close by its permission', async () => {
    serve(owner, {
      'GET /v2/invites': () => json({ invites: [link] }),
      'DELETE /v1/invites/7': () => refusal(403, 'permission_denied', 'no', { permission: 'manage_members' }),
    });
    await signedIn(ownerToken);
    const region = await joining();
    fireEvent.click(await within(region).findByRole('button', { name: 'Close link 7' }));
    expect(await within(region).findByText('You need the permission to manage members for that. The link was not closed.')).toBeDefined();
  });

  describe('Disable joiners', () => {
    const disableRoute = (count: number): Route => (c) => json({ dry_run: (c.body as { dry_run: boolean }).dry_run, count, principal_ids: [30, 31] });
    const sent = (calls: Call[]) => calls.filter((c) => c.path === '/v1/invites/7/disable-joiners').map((c) => c.body);

    it('is offered to a holder of manage_members only, and only where someone joined', async () => {
      serve(owner, { 'GET /v2/invites': () => json({ invites: [link, { ...link, id: 8, joiners: 0 }] }) });
      await signedIn(ownerToken);
      const list = await within(await joining()).findByRole('list', { name: 'Member links' });
      expect(within(list).getByRole('button', { name: 'Disable joiners of link 7' })).toBeDefined();
      expect(within(list).queryByRole('button', { name: 'Disable joiners of link 8' })).toBeNull();
      cleanup();
      serve({ ...member, permissions: [...memberRights, 'make_member_links'] }, { 'GET /v2/invites': () => json({ invites: [{ ...link, created_by: 3 }] }) });
      window.sessionStorage.clear();
      await signedIn(memberToken);
      expect(screen.queryByRole('button', { name: /Disable joiners/ })).toBeNull();
    });

    it('does a dry run, then asks, then sends once, and the link is closed too', async () => {
      let closed = false;
      const calls = serve(owner, {
        'GET /v2/invites': () => json({ invites: [closed ? { ...link, closed_at: at } : link] }),
        'POST /v1/invites/7/disable-joiners': (c) => {
          if (!(c.body as { dry_run: boolean }).dry_run) closed = true;
          return disableRoute(2)(c);
        },
      });
      await signedIn(ownerToken);
      const region = await joining();
      fireEvent.click(await within(region).findByRole('button', { name: 'Disable joiners of link 7' }));
      const ask = await within(region).findByRole('group', { name: 'Disable joiners of link 7' });
      expect(ask.textContent).toContain('signs out and disables 2 people');
      expect(ask.textContent).toContain('closes the link too');
      expect(sent(calls)).toEqual([{ dry_run: true }]);
      fireEvent.click(within(ask).getByRole('button', { name: 'Disable 2 people' }));
      expect(await within(region).findByText('Disabled 2 people.')).toBeDefined();
      expect(sent(calls)).toEqual([{ dry_run: true }, { dry_run: false }]);
      expect(within(region).queryByRole('group', { name: 'Disable joiners of link 7' })).toBeNull();
      await waitFor(() => {
        expect(within(region).queryByRole('button', { name: 'Close link 7' })).toBeNull();
      });
    });

    it('sends nothing when the question is cancelled, and hands focus back', async () => {
      const calls = serve(owner, { 'GET /v2/invites': () => json({ invites: [link] }), 'POST /v1/invites/7/disable-joiners': disableRoute(2) });
      await signedIn(ownerToken);
      const region = await joining();
      const open = await within(region).findByRole('button', { name: 'Disable joiners of link 7' });
      fireEvent.click(open);
      fireEvent.click(await within(region).findByRole('button', { name: 'Cancel' }));
      expect(within(region).queryByRole('group', { name: 'Disable joiners of link 7' })).toBeNull();
      expect(document.activeElement).toBe(open);
      expect(sent(calls)).toEqual([{ dry_run: true }]);
    });

    it('closes the question when manage_members goes, and sends nothing more', async () => {
      let rights: Permission[] = [...(owner.permissions ?? [])];
      const calls = serve(() => ({ ...owner, owner: false, permissions: rights, permissions_revision: rights.length }), {
        'GET /v2/invites': () => json({ invites: [link] }),
        'POST /v1/invites/7/disable-joiners': disableRoute(2),
      });
      await signedIn(ownerToken);
      const region = await joining();
      fireEvent.click(await within(region).findByRole('button', { name: 'Disable joiners of link 7' }));
      await within(region).findByRole('group', { name: 'Disable joiners of link 7' });
      await waitFor(() => {
        expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
      });
      for (const s of FakeSocket.all) s.open();
      rights = rights.filter((r) => r !== 'manage_members');
      FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'))?.send({ schema: 'conch.permissions_changed.v1' });
      await waitFor(() => {
        expect(within(region).queryByRole('group', { name: 'Disable joiners of link 7' })).toBeNull();
      });
      expect(within(region).queryByRole('button', { name: /Disable joiners/ })).toBeNull();
      expect(sent(calls)).toEqual([{ dry_run: true }]);
    });

    it.each([
      ['permission_denied', { permission: 'manage_members' }, 'You need the permission to manage members for that. No one was disabled.'],
      ['invite_not_found', {}, 'The server refused. No one was disabled.'],
      ['invalid_request', {}, 'The server did not take that. No one was disabled.'],
    ])('shows a refusal of the send by its code: %s', async (code, more, words) => {
      const calls = serve(owner, {
        'GET /v2/invites': () => json({ invites: [link] }),
        'POST /v1/invites/7/disable-joiners': (c) => ((c.body as { dry_run: boolean }).dry_run ? disableRoute(2)(c) : refusal(403, code, 'no', more)),
      });
      await signedIn(ownerToken);
      const region = await joining();
      fireEvent.click(await within(region).findByRole('button', { name: 'Disable joiners of link 7' }));
      fireEvent.click(await within(region).findByRole('button', { name: 'Disable 2 people' }));
      expect(await within(region).findByText(words)).toBeDefined();
      expect(sent(calls)).toHaveLength(2);
    });

    it('shows a refusal of the dry run, and asks nothing', async () => {
      serve(owner, { 'GET /v2/invites': () => json({ invites: [link] }), 'POST /v1/invites/7/disable-joiners': () => refusal(403, 'forbidden') });
      await signedIn(ownerToken);
      const region = await joining();
      fireEvent.click(await within(region).findByRole('button', { name: 'Disable joiners of link 7' }));
      expect(await within(region).findByText('The server does not allow you to do that. No one was disabled.')).toBeDefined();
      expect(within(region).queryByRole('group', { name: 'Disable joiners of link 7' })).toBeNull();
    });
  });

  it('counts a single other channel in the singular', () => {
    expect(channelsLine({ ...link, kind: 'member', channels: [], other_channels: 1 })).toBe('Only 1 other channel');
    expect(channelsLine({ ...link, kind: 'member', channels: [{ id: 1, name: 'channel-a' }], other_channels: undefined })).toBe('#channel-a');
  });
});

describe('the live signal', () => {
  it('reads whoami again, and the line a member sees follows it', async () => {
    let rights: Permission[] = ['use_voice'];
    serve(() => ({ ...member, permissions: rights }));
    window.sessionStorage.setItem(key, memberToken);
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    expect(await screen.findByText('You can read and write in the channels you are in. Voice is on.')).toBeDefined();
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    rights = ['create_public_channels', 'make_member_links'];
    const events = FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'));
    events?.send({ schema: 'conch.permissions_changed.v1' });
    expect(await screen.findByText('You can create public channels and make member links. Voice is off.')).toBeDefined();
  });

  it('is told apart by its schema, and a frame of another schema is not it', () => {
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.permissions_changed.v1' }))).toEqual({ kind: 'permissionsChanged' });
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.channels_changed.v1' }))).toEqual({ kind: 'channelsChanged' });
  });

  it('reads the permissions document again for an operator', async () => {
    // Two sections read the document (Server and Roles and permissions), so
    // the answer follows what the server says, not the count of reads.
    let changed = false;
    serve(owner, { 'GET /v1/permissions': () => json(changed ? permissionsDoc(3, ['use_voice']) : permissionsDoc(2, memberRights)) });
    window.location.hash = '#/c/channel-a';
    await signedIn(ownerToken);
    const summary = () => screen.getByRole('list', { name: 'What members have' }).textContent;
    await waitFor(() => {
      expect(summary()).toContain('Members see: everyone');
    });
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    changed = true;
    FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'))?.send({ schema: 'conch.permissions_changed.v1' });
    await waitFor(() => {
      expect(summary()).toContain('Members see: only people they share a channel with');
    });
  });
});

describe('gating by permission', () => {
  // A permission this page has no word for is shown by its name and never
  // sent. (The generated validators refuse it in whoami until #1051; the
  // helpers are what the pages use once they take it.)
  const future = 'future_thing' as Permission;

  it('keys on the owner mark and on whoami.permissions', () => {
    expect(canDo(owner, 'change_server_settings')).toBe(true);
    expect(canDo(operator, 'change_server_settings')).toBe(false);
    expect(canDo(settingsOperator, 'change_server_settings')).toBe(true);
    expect(canDo(member, 'manage_members')).toBe(false);
    // A server that predates permissions: an operator is not blocked here,
    // since the server decides each request itself; a member never is one.
    expect(canDo({ ...operator, permissions: undefined }, 'manage_members')).toBe(true);
    expect(canDo({ ...member, permissions: undefined }, 'manage_members')).toBe(false);
  });

  it('tolerates an unknown permission: named, never a key to anything', () => {
    expect(permissionName(future)).toBe('future_thing');
    expect(permissionName('manage_members')).toBe('manage members');
    expect(memberLine({ ...member, permissions: ['use_voice', future] })).toBe('You can future_thing. Voice is on.');
    expect(canDo({ ...member, permissions: [future] }, 'manage_members')).toBe(false);
    expect(refusalLine({ failure: 'invalid', code: 'permission_denied', permission: future }, 'No change.')).toBe('You need the permission to future_thing for that. No change.');
  });

  it('says nothing of rights where whoami carries none', () => {
    expect(memberLine({ ...member, permissions: undefined })).toBeNull();
  });

  it('words the camera permission (ADR-015)', () => {
    expect(permissionName('use_camera')).toBe('use a camera');
    expect(memberLine({ ...member, permissions: ['use_voice', 'use_camera'] })).toBe('You can turn on your camera. Voice is on.');
    expect(memberLine({ ...member, permissions: ['use_voice', 'share_screen', 'use_camera'] })).toBe('You can share your screen and turn on your camera. Voice is on.');
    expect(canDo({ ...member, permissions: ['use_voice'] }, 'use_camera')).toBe(false);
    expect(canDo(owner, 'use_camera')).toBe(true);
  });

  it('words each refusal by its code', () => {
    const cases: [Parameters<typeof refusalLine>[0], string][] = [
      [{ failure: 'network' }, 'Cannot reach the server. X'],
      [{ failure: 'invalid', code: 'permission_denied' }, 'You do not hold the permission for that. X'],
      [{ failure: 'invalid', code: 'reauth_required' }, 'It needs your password first. X'],
      [{ failure: 'invalid', code: 'invalid_request', message: 'schema: bad thing' }, 'The server did not take that. X'],
      [{ failure: 'invalid', code: 'permissions_conflict', message: 'schema: bad thing' }, 'That change would break a rule of the permissions. X'],
      [{ failure: 'invalid', code: 'join_policy_conflict', message: 'schema: bad thing' }, 'Those joining settings cannot stand together. X'],
      [{ failure: 'invalid', code: 'something_new' }, 'The server refused. X'],
    ];
    for (const [result, line] of cases) expect(refusalLine(result, 'X')).toBe(line);
  });

  it('works out what a preset changes in the members’ column', () => {
    const doc = permissionsDoc(1);
    expect(presetChanges(doc, 'group')).toEqual({ on: [], off: [] });
    expect(presetChanges(doc, 'personal')).toEqual({ on: [], off: ['create_public_channels', 'create_private_channels', 'make_member_links', 'see_member_directory'] });
  });
});

describe('staying current', () => {
  const whoamiCalls = (calls: readonly Call[]) => calls.filter((c) => c.path === '/v1/whoami').length;

  it('reads whoami and the permissions again when Settings opens, with no channel selected', async () => {
    const calls = serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    window.sessionStorage.setItem(key, ownerToken);
    render(<App origin={origin} />);
    const button = await screen.findByRole('button', { name: 'Settings' });
    await screen.findByRole('heading', { name: 'Server administration', hidden: true });
    const who = whoamiCalls(calls);
    const docs = calls.filter((c) => c.path === '/v1/permissions').length;
    fireEvent.click(button);
    await waitFor(() => {
      expect(whoamiCalls(calls)).toBeGreaterThan(who);
      expect(calls.filter((c) => c.path === '/v1/permissions').length).toBeGreaterThan(docs);
    });
    // No socket was opened (none of FakeSocket's is open): only opening
    // Settings did it.
  });

  it('reads whoami again when a feed opens for the first time, with nothing interrupted', async () => {
    const calls = serve(member);
    window.sessionStorage.setItem(key, memberToken);
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'Settings' });
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    const before = whoamiCalls(calls);
    for (const s of FakeSocket.all) s.open();
    await waitFor(() => {
      expect(whoamiCalls(calls)).toBeGreaterThan(before);
    });
  });

  it('drops an answer to an earlier whoami read that arrives after a later one’s', async () => {
    const held: ((rights: Permission[]) => void)[] = [];
    let holding = false;
    serve(member, {
      'GET /v1/whoami': () => {
        if (!holding) return json(member);
        return new Promise<Response>((resolve) => {
          held.push((rights) => {
            resolve(json({ ...member, permissions: rights }));
          });
        });
      },
    });
    window.sessionStorage.setItem(key, memberToken);
    window.location.hash = '#/c/channel-a';
    render(<App origin={origin} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
    await screen.findByText('You can create channels and make member links. Voice is on.');
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    // Let the reads the open itself made finish before holding the next two.
    await new Promise((resolve) => setTimeout(resolve, 50));
    holding = true;
    const events = FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'));
    events?.send({ schema: 'conch.permissions_changed.v1' });
    events?.send({ schema: 'conch.permissions_changed.v1' });
    await waitFor(() => {
      expect(held).toHaveLength(2);
    });
    const [first, second] = held;
    // The later read (B) answers first with the newer rights; the earlier (A)
    // answers after it with the older ones.
    await act(async () => {
      second?.(['create_public_channels']);
      await Promise.resolve();
    });
    expect(await screen.findByText('You can create public channels. Voice is off.')).toBeDefined();
    await act(async () => {
      first?.(['use_voice']);
      await new Promise((resolve) => setTimeout(resolve, 20));
    });
    expect(screen.getByText('You can create public channels. Voice is off.')).toBeDefined();
    expect(screen.queryByText('You can read and write in the channels you are in. Voice is on.')).toBeNull();
  });
});

describe('the invite brake when the permission goes', () => {
  it('closes an open question and never sends the pause', async () => {
    const calls = serve(owner);
    // Signing in pins the credential the component's calls use.
    window.sessionStorage.setItem(key, ownerToken);
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'Settings' });
    cleanup();
    // One function throughout: a new one on each render would read afresh.
    const stop = () => undefined;
    const view = render(<InviteSettings open onUnauthorized={stop} />);
    fireEvent.click(await screen.findByLabelText(/^Pause all invites/));
    expect(await screen.findByRole('group', { name: /^Pause every invite\?/ })).toBeDefined();
    view.rerender(<InviteSettings open disabledReason="Needs the permission to manage members." onUnauthorized={stop} />);
    expect(screen.queryByRole('group', { name: /^Pause every invite\?/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Yes, pause' })).toBeNull();
    expect(screen.getByLabelText<HTMLInputElement>(/^Pause all invites/).disabled).toBe(true);
    // Back again, the question is not still pending.
    view.rerender(<InviteSettings open onUnauthorized={stop} />);
    expect(screen.queryByRole('group', { name: /^Pause every invite\?/ })).toBeNull();
    expect(calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
  });
});

describe('the console on a phone', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('keeps focus in a field when the parent renders again', async () => {
    serve(owner);
    window.sessionStorage.setItem(key, ownerToken);
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'Settings' });
    cleanup();
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: true, addEventListener: () => undefined, removeEventListener: () => undefined })),
    );
    const props = { who: owner, origin, permissionsChanged: 0, directoryChanged: 0, onChannelsChanged: () => undefined, onUnauthorized: () => undefined };
    const view = render(<AdminConsole {...props} channels={[]} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Server' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Joining' }));
    const field = await screen.findByLabelText('Default link expiry (hours, 0 for none)');
    field.focus();
    expect(document.activeElement).toBe(field);
    // A message arrives: the parent renders again with new objects.
    view.rerender(<AdminConsole {...props} channels={[]} names={new Map([[1, 'owner']])} />);
    view.rerender(<AdminConsole {...props} channels={[]} />);
    expect(document.activeElement).toBe(field);
  });
});
