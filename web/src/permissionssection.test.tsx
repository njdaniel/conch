// The console's Roles and permissions section (ADR-014 design §10.2, §10.3,
// issue #1019), against a fake conchd (fetch) and a fake WebSocket: the matrix
// in server order with its locked cells and their reasons, the preset cards,
// the patch a save sends, the stale and conflict answers, the confirmation for
// Act as members, the list of what operators issued to others, the live
// re-read with an unsaved edit, and the phone layout. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { AdminConsole } from './AdminConsole';
import { actAsMembersNotEnded, actAsMembersSentence, auditSentence, managersBlock, movedCells, ownersLine, PermissionMatrix } from './admin/PermissionsSection';
import type { IssuedByOperatorV1, KnownPermission, Permission, PermissionsResponseV1 } from './schema/permission';
import type { WhoAmIResponseV1 } from './schema/principal';

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const ownerToken = 'conch_owner_session_token';
const operatorToken = 'conch_operator_session_token';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const password = 'the owner passphrase here';

const catalog: KnownPermission[] = [
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
const memberRights: Permission[] = catalog.slice(0, 6);
const operatorRights: Permission[] = catalog.slice(0, 9);
const personalRights: Permission[] = ['use_voice', 'share_screen'];
const auth = { scope: 'full', password_set: true, totp_enrolled: false, setup_complete: true, expires_at: later, idle_expires_at: later } as const;

const owner: WhoAmIResponseV1 = {
  id: 1,
  kind: 'human',
  name: 'owner',
  role: 'operator',
  owner: true,
  identity_class: 'operator',
  may_invite: true,
  permissions: catalog,
  permissions_revision: 1,
  auth,
};
const operator: WhoAmIResponseV1 = { ...owner, id: 2, name: 'operator-1', owner: false, permissions: operatorRights };
const settingsOperator: WhoAmIResponseV1 = { ...operator, permissions: [...operatorRights, 'change_server_settings'] };

function permissionsDoc(
  revision: number,
  opts: { member?: readonly Permission[]; operator?: readonly Permission[]; editable?: PermissionsResponseV1['editable']; warnings?: PermissionsResponseV1['warnings']; presets?: PermissionsResponseV1['presets'] } = {},
): PermissionsResponseV1 {
  const member = opts.member ?? memberRights;
  const op = opts.operator ?? operatorRights;
  const matches = (preset: readonly Permission[]) => preset.length === member.length && preset.every((p) => member.includes(p));
  const presets = opts.presets ?? [
    { preset: 'personal' as const, member: personalRights },
    { preset: 'group' as const, member: memberRights },
  ];
  const current = presets.find((p) => matches(p.member));
  return {
    revision,
    rows: catalog.map((permission, i) => ({
      permission,
      member: member.includes(permission),
      operator: op.includes(permission),
      ...(i >= 6 ? { member_fixed: true } : {}),
      ...(permission === 'see_member_directory' ? { operator_fixed: true } : {}),
    })),
    ...(current === undefined ? {} : { preset: current.preset }),
    presets,
    editable: opts.editable ?? ['member', 'operator'],
    warnings: opts.warnings ?? [],
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
      if (path === '/v1/channels') return json({ channels: [{ id: 1, name: 'channel-a', created_at: at }] });
      if (path === '/v1/ws-tickets') return json({ ticket: ticketValue, expires_at: at }, 201);
      if (path === '/v1/join/settings' && method === 'GET') {
        return json({
          settings: { open_joining: 'off', guests_enabled: false, joining_paused: false, join_voice: true, max_guests: 10, max_guests_per_address: 3, guest_idle_seconds: 3600, guest_absolute_seconds: 86400, open_channels: ['channel-a'], open_signup: false, member_link_default_expiry_seconds: 604800 },
          server_name: 'conch.example',
          live_guests: 0,
          warnings: [],
        });
      }
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

// open signs in, opens Settings and waits for the matrix.
async function open(token: string): Promise<HTMLElement> {
  window.sessionStorage.setItem(key, token);
  window.location.hash = '#/c/channel-a';
  render(<App origin={origin} />);
  fireEvent.click(await screen.findByRole('button', { name: 'Settings' }));
  const region = await screen.findByRole('region', { name: 'Roles and permissions' });
  await within(region).findByRole('table');
  return region;
}

function cell(region: HTMLElement, permission: string, role: 'member' | 'operator'): HTMLInputElement {
  const found = region.querySelector<HTMLInputElement>(`[data-cell="${permission}:${role}"]`);
  if (found === null) throw new Error(`no cell ${permission}:${role}`);
  return found;
}

const locked = (input: HTMLInputElement) => input.getAttribute('aria-disabled') === 'true';
const puts = (calls: readonly Call[]) => calls.filter((c) => c.method === 'PUT' && c.path === '/v1/permissions');

// events opens the sockets and returns the events one.
async function events(): Promise<FakeSocket> {
  await waitFor(() => {
    expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
  });
  for (const s of FakeSocket.all) s.open();
  const found = FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'));
  if (found === undefined) throw new Error('no events socket');
  return found;
}

const changed = { schema: 'conch.permissions_changed.v1' };

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

describe('the matrix', () => {
  it('shows every permission in the server’s order, with its words, and an all-on locked Owners column', async () => {
    serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    const region = await open(ownerToken);
    const body = within(region).getAllByRole('rowgroup')[1];
    if (body === undefined) throw new Error('no body');
    const names = within(body).getAllByRole('rowheader').map((h) => h.querySelector('strong')?.textContent);
    expect(names).toEqual([
      'Create public channels',
      'Create private channels',
      'Make member links',
      'See the member directory',
      'Use voice',
      'Share a screen',
      'Manage members',
      'Act as members',
      'Manage channels',
      'Change server settings',
      'Read the audit log',
    ]);
    expect(within(region).getAllByRole('columnheader').map((h) => h.textContent)).toEqual(['Permission', 'Members', 'Operators', 'Owners']);
    // The sentences the design fixes.
    expect(region.textContent).toContain(actAsMembersSentence);
    expect(region.textContent).toContain(actAsMembersNotEnded);
    expect(region.textContent).toContain(auditSentence);
    expect(region.textContent).toContain(ownersLine);
    expect(region.textContent).toContain(managersBlock);
    for (const p of catalog) {
      const owners = within(region).getByRole<HTMLInputElement>('checkbox', { name: `${info(p)}, Owners` });
      expect(owners.checked).toBe(true);
      expect(locked(owners)).toBe(true);
    }
  });

  it('locks the fixed cells and explains them on keyboard focus, not only on hover', async () => {
    serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    const region = await open(ownerToken);
    const memberFixed = cell(region, 'manage_members', 'member');
    const operatorFixed = cell(region, 'see_member_directory', 'operator');
    for (const [input, reason] of [
      [memberFixed, 'Fixed: members cannot hold this permission.'],
      [operatorFixed, 'Fixed: operators always hold this permission.'],
    ] as const) {
      expect(locked(input)).toBe(true);
      // Not `disabled`: it can be reached with the keyboard.
      expect(input.disabled).toBe(false);
      input.focus();
      expect(document.activeElement).toBe(input);
      const described = input.getAttribute('aria-describedby');
      expect(document.getElementById(described ?? '')?.textContent).toBe(reason);
      expect(input.closest('.cell')?.getAttribute('title')).toBe(reason);
      // Pressing it changes nothing.
      const was = input.checked;
      fireEvent.click(input);
      expect(input.checked).toBe(was);
    }
    expect(screen.queryByText(/unsaved/)).toBeNull();
  });

  it('shows an unknown permission by its name, read-only', () => {
    const doc = permissionsDoc(2);
    const future = { permission: 'future_thing' as Permission, member: true, operator: true };
    render(
      <PermissionMatrix
        doc={{ ...doc, rows: [...doc.rows, future] }}
        who={owner}
        phone={false}
        column="member"
        edits={{}}
        stale={false}
        flagged={null}
        busy={false}
        onToggle={() => {
          throw new Error('an unknown permission is never toggled');
        }}
      />,
    );
    const header = screen.getByRole('rowheader', { name: /future_thing/ });
    expect(header.querySelector('strong')?.textContent).toBe('future_thing');
    const input = screen.getByRole<HTMLInputElement>('checkbox', { name: 'future_thing, Members' });
    expect(locked(input)).toBe(true);
    fireEvent.click(input);
    expect(input.checked).toBe(true);
    expect(document.getElementById(input.getAttribute('aria-describedby') ?? '')?.textContent).toContain('no words for this permission');
  });
});

function info(p: KnownPermission): string {
  const words: Record<KnownPermission, string> = {
    create_public_channels: 'Create public channels',
    create_private_channels: 'Create private channels',
    make_member_links: 'Make member links',
    see_member_directory: 'See the member directory',
    use_voice: 'Use voice',
    share_screen: 'Share a screen',
    manage_members: 'Manage members',
    act_as_members: 'Act as members',
    manage_channels: 'Manage channels',
    change_server_settings: 'Change server settings',
    read_audit_log: 'Read the audit log',
  };
  return words[p];
}

describe('who can edit what', () => {
  it('lets the owner edit both editable columns', async () => {
    serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    const region = await open(ownerToken);
    expect(locked(cell(region, 'use_voice', 'member'))).toBe(false);
    expect(locked(cell(region, 'manage_members', 'operator'))).toBe(false);
    expect(locked(cell(region, 'change_server_settings', 'operator'))).toBe(false);
  });

  it('lets an operator with change_server_settings edit the member column only, and says why for the other', async () => {
    serve(settingsOperator, { 'GET /v1/permissions': () => json(permissionsDoc(2, { editable: ['member'] })) });
    const region = await open(operatorToken);
    expect(locked(cell(region, 'create_public_channels', 'member'))).toBe(false);
    const op = cell(region, 'use_voice', 'operator');
    expect(locked(op)).toBe(true);
    expect(document.getElementById(op.getAttribute('aria-describedby') ?? '')?.textContent).toBe('Only an owner changes what operators may do.');
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Use Personal preset' }).disabled).toBe(false);
  });

  it('shows an operator without change_server_settings the matrix read-only, with the reason, and offers no write', async () => {
    const calls = serve(operator, { 'GET /v1/permissions': () => json(permissionsDoc(2, { editable: [] })) });
    const region = await open(operatorToken);
    expect(within(region).getAllByText('Needs the permission to change server settings.').length).toBeGreaterThan(0);
    for (const p of catalog) for (const role of ['member', 'operator'] as const) expect(locked(cell(region, p, role))).toBe(true);
    const voice = cell(region, 'use_voice', 'member');
    expect(document.getElementById(voice.getAttribute('aria-describedby') ?? '')?.textContent).toBe('Needs the permission to change server settings.');
    fireEvent.click(voice);
    expect(voice.checked).toBe(true);
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Save changes' }).disabled).toBe(true);
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Use Personal preset' }).disabled).toBe(true);
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Use Group preset' }).disabled).toBe(true);
    expect(puts(calls)).toHaveLength(0);
  });

  it('does not offer an operator a member cell they do not hold themselves', async () => {
    serve({ ...settingsOperator, permissions: [...operatorRights.filter((p) => p !== 'use_voice'), 'change_server_settings'] }, { 'GET /v1/permissions': () => json(permissionsDoc(2, { editable: ['member'], member: ['create_public_channels'] })) });
    const region = await open(operatorToken);
    const voice = cell(region, 'use_voice', 'member');
    expect(locked(voice)).toBe(true);
    expect(document.getElementById(voice.getAttribute('aria-describedby') ?? '')?.textContent).toContain('do not hold this permission yourself');
    // Turning one off is never bounded.
    expect(locked(cell(region, 'create_public_channels', 'member'))).toBe(false);
  });
});

describe('the presets', () => {
  it('shows each preset’s diff against the member column, marks the matching one, and warns without refusing', async () => {
    serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2, { warnings: ['default_channel_exposes_members', 'open_signup_on'] })),
    });
    const region = await open(ownerToken);
    const personal = within(region).getByRole('region', { name: 'Personal preset' });
    expect(personal.textContent).toContain('Would turn off: Create public channels, Create private channels, Make member links, See the member directory.');
    expect(personal.textContent).not.toContain('Would turn on');
    expect(personal.textContent).not.toContain('matches');
    const group = within(region).getByRole('region', { name: 'Group preset' });
    expect(group.textContent).toContain('Changes nothing in the members’ column now.');
    expect(group.textContent).toContain('the members’ column matches this');
    const warnings = within(region).getByRole('list', { name: 'Warnings about these permissions' });
    expect(within(warnings).getAllByRole('listitem')).toHaveLength(2);
    // Warnings are not refusals: the preset that changes something still works.
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Use Personal preset' }).disabled).toBe(false);
    // The one already applied changes nothing, and says so.
    const same = within(region).getByRole<HTMLButtonElement>('button', { name: 'Use Group preset' });
    expect(same.disabled).toBe(true);
    expect(same.title).toBe('Already applied.');
  });

  it('takes the preset table from the server’s answer, never from a copy', async () => {
    serve(owner, {
      'GET /v1/permissions': () =>
        json(permissionsDoc(2, { presets: [{ preset: 'personal', member: ['use_voice'] }, { preset: 'group', member: [...memberRights, 'manage_members' as Permission].slice(0, 6) }] })),
    });
    const region = await open(ownerToken);
    const personal = within(region).getByRole('region', { name: 'Personal preset' });
    expect(personal.textContent).toContain('Would turn off: Create public channels, Create private channels, Make member links, See the member directory, Share a screen.');
  });

  // Issue #1051: PermissionPreset and PermissionsWarning are client-tolerant.
  // A newer server's preset or warning reads, and is shown by its name.
  it('shows a preset and a warning this page has no words for by their names', async () => {
    serve(owner, {
      'GET /v1/permissions': () =>
        json(
          permissionsDoc(2, {
            presets: [
              { preset: 'personal', member: personalRights },
              { preset: 'group', member: memberRights },
              { preset: 'future_preset', member: ['use_voice', 'fly_the_server'] },
            ],
            warnings: ['open_signup_on', 'future_warning'],
          }),
        ),
    });
    const region = await open(ownerToken);
    const card = within(region).getByRole('region', { name: 'future_preset preset' });
    expect(within(card).getByRole('heading').textContent).toBe('future_preset');
    expect(card.textContent).toContain('Would turn on: fly_the_server.');
    expect(within(card).getByRole<HTMLButtonElement>('button', { name: 'Use future_preset preset' }).disabled).toBe(false);
    const warnings = within(region).getByRole('list', { name: 'Warnings about these permissions' });
    expect(within(warnings).getAllByRole('listitem').map((li) => li.textContent)).toEqual([
      'Members cannot make member links, yet open sign-up is on, so the server’s address still admits anyone.',
      'future_warning',
    ]);
  });

  it('applies a preset through PUT /v1/permissions with the revision read, and shows the answer', async () => {
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => json(permissionsDoc(3, { member: personalRights })),
    });
    const region = await open(ownerToken);
    fireEvent.click(within(region).getByRole('button', { name: 'Use Personal preset' }));
    expect(await within(region).findByText('Applied Personal server.')).toBeDefined();
    expect(puts(calls).map((c) => c.body)).toEqual([{ revision: 2, preset: 'personal' }]);
    expect(cell(region, 'create_public_channels', 'member').checked).toBe(false);
    expect(within(region).getByRole('region', { name: 'Personal preset' }).textContent).toContain('the members’ column matches this');
    fireEvent.click(within(region).getByRole('button', { name: 'Use Group preset' }));
    await waitFor(() => {
      expect(puts(calls)).toHaveLength(2);
    });
    expect(puts(calls)[1]?.body).toEqual({ revision: 3, preset: 'group' });
  });

  it('is not offered while there are unsaved changes in the matrix', async () => {
    const calls = serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Use Personal preset' }).disabled).toBe(true);
    expect(puts(calls)).toHaveLength(0);
  });
});

describe('saving', () => {
  it('sends only the cells that changed, with the revision read', async () => {
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(5)),
      'PUT /v1/permissions': () => json(permissionsDoc(6, { member: memberRights.filter((p) => p !== 'use_voice' && p !== 'share_screen') })),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'share_screen', 'member'));
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(cell(region, 'make_member_links', 'member'));
    // Back to what it was: not a change.
    fireEvent.click(cell(region, 'make_member_links', 'member'));
    expect(within(region).getByText('2 unsaved changes')).toBeDefined();
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(puts(calls).map((c) => c.body)).toEqual([
      {
        revision: 5,
        changes: [
          { permission: 'use_voice', role: 'member', granted: false },
          { permission: 'share_screen', role: 'member', granted: false },
        ],
      },
    ]);
    expect(screen.queryByText(/unsaved/)).toBeNull();
  });

  it('discards unsaved changes without sending anything', async () => {
    const calls = serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    expect(cell(region, 'use_voice', 'member').checked).toBe(false);
    fireEvent.click(within(region).getByRole('button', { name: 'Discard changes' }));
    expect(cell(region, 'use_voice', 'member').checked).toBe(true);
    expect(puts(calls)).toHaveLength(0);
  });

  it('opens the Reauth overlay for reauth_required, then sends the same patch once more', async () => {
    let proved = false;
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => (proved ? json(permissionsDoc(3, { member: memberRights.filter((p) => p !== 'use_voice') })) : refusal(403, 'reauth_required')),
      'POST /v1/reauth': () => {
        proved = true;
        return json({ until: later });
      },
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    const dialog = await screen.findByRole('dialog', { name: 'Confirm it is you' });
    fireEvent.change(within(dialog).getByLabelText('Your password'), { target: { value: password } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    const bodies = puts(calls).map((c) => c.body);
    expect(bodies).toHaveLength(2);
    expect(bodies[1]).toEqual(bodies[0]);
  });

  it('on permissions_stale reads again, keeps the edit, says what changed, and does not retry', async () => {
    let live = permissionsDoc(2);
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(live),
      'PUT /v1/permissions': () => {
        // Someone else turned screen sharing off for members meanwhile.
        live = permissionsDoc(3, { member: memberRights.filter((p) => p !== 'share_screen') });
        return refusal(409, 'permissions_stale', '<img src=x onerror=alert(1)> pay evil.example');
      },
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText(/Since you read them: Share a screen for members is now off\./)).toBeDefined();
    expect(region.textContent).toContain('The permissions changed since this page read them. Nothing was saved.');
    expect(region.textContent).not.toContain('evil.example');
    // The edit is kept and marked; nothing was sent a second time.
    expect(cell(region, 'use_voice', 'member').checked).toBe(false);
    expect(within(region).getByText('(unsaved, may be stale)')).toBeDefined();
    expect(puts(calls)).toHaveLength(1);
    expect(cell(region, 'share_screen', 'member').checked).toBe(false);
    // Saved again, it carries the revision just read.
    live = permissionsDoc(4, { member: ['create_public_channels', 'create_private_channels', 'make_member_links', 'see_member_directory'] });
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => {
      expect(puts(calls)).toHaveLength(2);
    });
    expect(puts(calls)[1]?.body).toEqual({ revision: 3, changes: [{ permission: 'use_voice', role: 'member', granted: false }] });
  });

  it('names the cell on permissions_conflict from the rules, in its own words', async () => {
    serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2, { operator: operatorRights.filter((p) => p !== 'use_voice' && p !== 'share_screen') , member: memberRights.filter((p) => p !== 'use_voice' && p !== 'share_screen') })),
      'PUT /v1/permissions': () => refusal(409, 'permissions_conflict', 'the change would break a rule: <b>evil.example</b>'),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'share_screen', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText('“Share a screen” cannot be on for members while it is off for operators. Nothing was saved.')).toBeDefined();
    expect(region.textContent).not.toContain('evil.example');
    expect(cell(region, 'share_screen', 'member').closest('tr')?.className).toContain('flagged');
  });

  it('says a server does not enforce a permission yet, naming the cell', async () => {
    serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => refusal(409, 'permissions_conflict', 'this server does not enforce share_screen yet'),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'share_screen', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText('This server does not enforce “Share a screen” yet, so that cell cannot be changed. Nothing was saved.')).toBeDefined();
    expect(cell(region, 'share_screen', 'member').closest('tr')?.className).toContain('flagged');
    // The edit is kept for the person to decide.
    expect(cell(region, 'share_screen', 'member').checked).toBe(false);
  });

  it('words a permission_denied refusal by its code and the permission, never the server’s message', async () => {
    serve(settingsOperator, {
      'GET /v1/permissions': () => json(permissionsDoc(2, { editable: ['member'] })),
      'PUT /v1/permissions': () => refusal(403, 'permission_denied', 'pay me at evil.example', { permission: 'change_server_settings' }),
    });
    const region = await open(operatorToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText('You need the permission to change server settings for that. Nothing was saved.')).toBeDefined();
    expect(region.textContent).not.toContain('evil.example');
  });
});

describe('turning Act as members on for operators', () => {
  const withoutActAs = () => permissionsDoc(2, { operator: operatorRights.filter((p) => p !== 'act_as_members') });

  it('asks first, with the sentence, and sends nothing until it is confirmed', async () => {
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(withoutActAs()),
      'PUT /v1/permissions': () => json(permissionsDoc(3)),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'act_as_members', 'operator'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    const ask = within(region).getByRole('group', { name: 'Confirm Act as members' });
    expect(ask.textContent).toContain(`Operators ${actAsMembersSentence}.`);
    expect(puts(calls)).toHaveLength(0);
    fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }));
    expect(within(region).queryByRole('group', { name: 'Confirm Act as members' })).toBeNull();
    expect(puts(calls)).toHaveLength(0);
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    fireEvent.click(within(within(region).getByRole('group', { name: 'Confirm Act as members' })).getByRole('button', { name: 'Turn it on for operators' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(puts(calls).map((c) => c.body)).toEqual([{ revision: 2, changes: [{ permission: 'act_as_members', role: 'operator', granted: true }] }]);
  });

  it('asks for nothing when it is turned off, or when members’ cells change', async () => {
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => json(permissionsDoc(3)),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText('Saved.')).toBeDefined();
    expect(within(region).queryByRole('group', { name: 'Confirm Act as members' })).toBeNull();
    expect(puts(calls)).toHaveLength(1);
  });

  it('closes the question when change_server_settings goes, and sends nothing, even after it returns', async () => {
    let who: WhoAmIResponseV1 = owner;
    const calls = serve(() => who, { 'GET /v1/permissions': () => json(withoutActAs()) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'act_as_members', 'operator'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(within(region).getByRole('group', { name: 'Confirm Act as members' })).toBeDefined();
    const socket = await events();
    who = { ...operator, id: owner.id };
    socket.send(changed);
    await waitFor(() => {
      expect(within(region).queryByRole('group', { name: 'Confirm Act as members' })).toBeNull();
    });
    expect(within(region).queryByRole('button', { name: 'Turn it on for operators' })).toBeNull();
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Save changes' }).disabled).toBe(true);
    who = owner;
    socket.send(changed);
    await waitFor(() => {
      expect(locked(cell(region, 'act_as_members', 'operator'))).toBe(false);
    });
    // The edit was dropped while the permission was gone: nothing is left to save.
    expect(within(region).getByRole<HTMLButtonElement>('button', { name: 'Save changes' }).disabled).toBe(true);
    expect(within(region).queryByRole('group', { name: 'Confirm Act as members' })).toBeNull();
    expect(puts(calls)).toHaveLength(0);
  });
});

describe('what operators issued to others', () => {
  const issued: IssuedByOperatorV1[] = [
    { kind: 'credential', id: 41, principal_id: 7, issued_by: 2, issued_at: at },
    { kind: 'credential', id: 42, principal_id: 7, issued_by: 2, issued_at: at },
    { kind: 'hook', id: 9, principal_id: 8, issued_by: 2, issued_at: at },
  ];

  async function turnedOff(routes: Record<string, Route> = {}) {
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => json({ ...permissionsDoc(3, { operator: operatorRights.filter((p) => p !== 'act_as_members') }), issued_to_others: issued }),
      ...routes,
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'act_as_members', 'operator'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    const list = await within(region).findByRole('region', { name: 'Issued to others by operators' });
    return { calls, region, list };
  }

  it('shows the list from the PUT answer, with Sign out everywhere and Revoke beside each', async () => {
    const { list } = await turnedOff();
    expect(within(list).getAllByRole('listitem')).toHaveLength(3);
    expect(list.textContent).toContain('Credential #41 for person #7');
    expect(list.textContent).toContain('Hook #9 for person #8');
    // One for each person, not each credential: #41 and #42 are one person's.
    expect(within(list).getAllByRole('button', { name: /^Sign out everywhere/ })).toHaveLength(1);
    expect(within(list).getByRole('button', { name: 'Revoke hook #9' })).toBeDefined();
    // A hook is not a credential: no sign-out for it.
    expect(within(list).queryByRole('button', { name: 'Sign out everywhere, person #8' })).toBeNull();
  });

  // Issue #1051: IssuedKind is client-tolerant. A kind a newer server added
  // is listed by its name; this page has no route to revoke it by, so it
  // offers none, and never sends it to another kind's route.
  it('shows an issued kind this page has no words for by its name, and does not revoke it', async () => {
    const { calls, list } = await turnedOff({
      'PUT /v1/permissions': () =>
        json({
          ...permissionsDoc(3, { operator: operatorRights.filter((p) => p !== 'act_as_members') }),
          issued_to_others: [...issued, { kind: 'future_kind', id: 77, principal_id: 8, issued_by: 2, issued_at: at }],
        }),
    });
    expect(within(list).getAllByRole('listitem')).toHaveLength(4);
    expect(list.textContent).toContain('future_kind #77 for person #8');
    const revoke = within(list).getByRole<HTMLButtonElement>('button', { name: 'Revoke future_kind #77' });
    expect(revoke.disabled).toBe(true);
    expect(revoke.title).toBe('This page has no words for this kind, so it cannot revoke it here.');
    fireEvent.click(revoke);
    expect(calls.filter((c) => c.method === 'DELETE')).toHaveLength(0);
    // The known ones beside it still work as before.
    expect(within(list).getByRole<HTMLButtonElement>('button', { name: 'Revoke hook #9' }).disabled).toBe(false);
  });

  it('revokes a credential and a hook by their routes, and the item goes', async () => {
    const { calls, list } = await turnedOff({
      'DELETE /v1/credentials/41': () => new Response(null, { status: 204 }),
      'DELETE /v1/credentials/42': () => new Response(null, { status: 204 }),
      'DELETE /v1/hooks/9': () => new Response(null, { status: 204 }),
    });
    fireEvent.click(within(list).getByRole('button', { name: 'Revoke credential #41' }));
    expect(await within(list).findByText('Revoked credential #41.')).toBeDefined();
    fireEvent.click(within(list).getByRole('button', { name: 'Revoke hook #9' }));
    await waitFor(() => {
      expect(within(list).queryByRole('button', { name: 'Revoke hook #9' })).toBeNull();
    });
    expect(calls.filter((c) => c.method === 'DELETE').map((c) => c.path)).toEqual(['/v1/credentials/41', '/v1/hooks/9']);
    expect(within(list).getAllByRole('listitem')).toHaveLength(1);
    // The last one revoked leaves its confirmation in view, not a vanished list.
    fireEvent.click(within(list).getByRole('button', { name: 'Revoke credential #42' }));
    expect(await within(list).findByText('Revoked credential #42.')).toBeDefined();
    expect(within(list).queryAllByRole('listitem')).toHaveLength(0);
    expect(within(list).getByText('Nothing issued by operators is left on this list.')).toBeDefined();
  });

  it('asks before signing a person out everywhere, then does it and clears their credentials from the list', async () => {
    const { calls, list } = await turnedOff({ 'POST /v1/principals/7/credentials/revoke-all': () => json({ revoked: 2 }) });
    fireEvent.click(within(list).getByRole('button', { name: 'Sign out everywhere, person #7' }));
    const ask = within(list).getByRole('group', { name: 'Confirm' });
    expect(ask.textContent).toContain('Sign person #7 out everywhere?');
    expect(calls.some((c) => c.path === '/v1/principals/7/credentials/revoke-all')).toBe(false);
    fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }));
    expect(calls.some((c) => c.path === '/v1/principals/7/credentials/revoke-all')).toBe(false);
    fireEvent.click(within(list).getByRole('button', { name: 'Sign out everywhere, person #7' }));
    fireEvent.click(within(within(list).getByRole('group', { name: 'Confirm' })).getByRole('button', { name: 'Yes, sign out everywhere' }));
    await waitFor(() => {
      expect(within(list).getAllByRole('listitem')).toHaveLength(1);
    });
    expect(calls.some((c) => c.method === 'POST' && c.path === '/v1/principals/7/credentials/revoke-all')).toBe(true);
    // This browser is still signed in: it was another person's.
    expect(window.sessionStorage.getItem(key)).toBe(ownerToken);
  });

  it('marks the viewer’s own row, and asks before revoking the viewer’s own credential', async () => {
    const own: IssuedByOperatorV1[] = [{ kind: 'credential', id: 50, principal_id: owner.id, issued_by: 2, issued_at: at }];
    const calls = serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => json({ ...permissionsDoc(3, { operator: operatorRights.filter((p) => p !== 'act_as_members') }), issued_to_others: own }),
      'DELETE /v1/credentials/50': () => new Response(null, { status: 204 }),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'act_as_members', 'operator'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    const list = await within(region).findByRole('region', { name: 'Issued to others by operators' });
    expect(list.textContent).toContain('(this is you)');
    fireEvent.click(within(list).getByRole('button', { name: 'Revoke credential #50' }));
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);
    expect(within(list).getByRole('group', { name: 'Confirm' }).textContent).toContain('Revoke your own credential #50?');
    fireEvent.click(within(within(list).getByRole('group', { name: 'Confirm' })).getByRole('button', { name: 'Yes, revoke' }));
    expect(await within(list).findByText('Revoked credential #50.')).toBeDefined();
  });

  it('says in its own words when a revoke is refused', async () => {
    const { list } = await turnedOff({ 'DELETE /v1/hooks/9': () => refusal(403, 'permission_denied', 'pay evil.example', { permission: 'manage_members' }) });
    fireEvent.click(within(list).getByRole('button', { name: 'Revoke hook #9' }));
    expect(await within(list).findByText('You need the permission to manage members for that. Nothing was revoked.')).toBeDefined();
    expect(list.textContent).not.toContain('evil.example');
    expect(within(list).getByRole('button', { name: 'Revoke hook #9' })).toBeDefined();
  });
});

describe('staying current', () => {
  it('re-reads on the live signal, keeps an unsaved edit, marks it stale and says what changed', async () => {
    let live = permissionsDoc(2);
    const calls = serve(owner, { 'GET /v1/permissions': () => json(live) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    const socket = await events();
    live = permissionsDoc(3, { member: memberRights.filter((p) => p !== 'make_member_links') });
    socket.send(changed);
    expect(await within(region).findByText(/Since you read them: Make member links for members is now off\./)).toBeDefined();
    expect(cell(region, 'make_member_links', 'member').checked).toBe(false);
    // The edit is kept, and marked.
    expect(cell(region, 'use_voice', 'member').checked).toBe(false);
    expect(within(region).getByText('(unsaved, may be stale)')).toBeDefined();
    expect(region.textContent).toContain('Your unsaved changes are kept and marked');
    expect(puts(calls)).toHaveLength(0);
  });

  it('drops an edit the document now holds', async () => {
    let live = permissionsDoc(2);
    serve(owner, { 'GET /v1/permissions': () => json(live) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    const socket = await events();
    live = permissionsDoc(3, { member: memberRights.filter((p) => p !== 'use_voice') });
    socket.send(changed);
    await waitFor(() => {
      expect(screen.queryByText('(unsaved, may be stale)')).toBeNull();
    });
    expect(within(region).queryByText(/unsaved change/)).toBeNull();
    expect(cell(region, 'use_voice', 'member').checked).toBe(false);
  });

  it('keeps the newest answer when an earlier read answers late', async () => {
    let n = 0;
    const releases: ((r: Response) => void)[] = [];
    serve(owner, {
      'GET /v1/permissions': () => {
        n++;
        // The first two reads (Server and this section) answer at once. The
        // next two, one for each section, are held back; the two after them
        // (the second signal) answer at once, so they overtake.
        if (n === 3 || n === 4) return new Promise<Response>((resolve) => releases.push(resolve));
        return json(n >= 5 ? permissionsDoc(4, { member: ['use_voice'] }) : permissionsDoc(2));
      },
    });
    const region = await open(ownerToken);
    const socket = await events();
    socket.send(changed);
    await waitFor(() => {
      expect(releases).toHaveLength(2);
    });
    socket.send(changed);
    await waitFor(() => {
      expect(cell(region, 'create_public_channels', 'member').checked).toBe(false);
    });
    // The old answers arrive after the newer one, and change nothing.
    await act(async () => {
      for (const release of releases) release(json(permissionsDoc(3)));
      await Promise.resolve();
    });
    expect(cell(region, 'create_public_channels', 'member').checked).toBe(false);
  });

  it('drops an edit to a cell that a re-read has locked, and closes the confirmation', async () => {
    let live = permissionsDoc(2, { operator: operatorRights.filter((p) => p !== 'act_as_members') });
    let who: WhoAmIResponseV1 = owner;
    const calls = serve(() => who, { 'GET /v1/permissions': () => json(live) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'act_as_members', 'operator'));
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(within(region).getByRole('group', { name: 'Confirm Act as members' })).toBeDefined();
    const socket = await events();
    // The owner mark has gone: only the member column is editable now.
    live = permissionsDoc(2, { operator: operatorRights.filter((p) => p !== 'act_as_members'), editable: ['member'] });
    who = settingsOperator;
    socket.send(changed);
    await waitFor(() => {
      expect(within(region).queryByRole('group', { name: 'Confirm Act as members' })).toBeNull();
    });
    expect(within(region).getByText(/Act as members for operators is no longer yours to change/)).toBeDefined();
    expect(cell(region, 'act_as_members', 'operator').checked).toBe(false);
    // The member edit is still there, and only it would be sent.
    expect(cell(region, 'use_voice', 'member').checked).toBe(false);
    expect(within(region).getByText('1 unsaved change')).toBeDefined();
    expect(puts(calls)).toHaveLength(0);
  });

  it('says it is reading again after a stale answer, then that it has', async () => {
    let release: (r: Response) => void = () => undefined;
    let held = false;
    serve(owner, {
      'GET /v1/permissions': () => {
        if (!held) return json(permissionsDoc(2));
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      },
      'PUT /v1/permissions': () => {
        held = true;
        return refusal(409, 'permissions_stale');
      },
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText(/Reading them again…/)).toBeDefined();
    await act(async () => {
      release(json(permissionsDoc(3)));
      await Promise.resolve();
    });
    expect(await within(region).findByText(/They have been read again; check your changes and save again\./)).toBeDefined();
    expect(within(region).queryByText(/Reading them again…/)).toBeNull();
  });

  it('does not call a permissions_conflict "not enforced" unless the message says so', async () => {
    serve(owner, {
      'GET /v1/permissions': () => json(permissionsDoc(2)),
      'PUT /v1/permissions': () => refusal(409, 'permissions_conflict', 'share_screen needs something else'),
    });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'make_member_links', 'member'));
    fireEvent.click(within(region).getByRole('button', { name: 'Save changes' }));
    expect(await within(region).findByText('That change would break a rule of the permissions. Nothing was saved.')).toBeDefined();
    expect(region.textContent).not.toContain('does not enforce');
  });

  it('clears the notice of what moved when changes are discarded', async () => {
    let live = permissionsDoc(2);
    serve(owner, { 'GET /v1/permissions': () => json(live) });
    const region = await open(ownerToken);
    fireEvent.click(cell(region, 'use_voice', 'member'));
    const socket = await events();
    live = permissionsDoc(3, { member: memberRights.filter((p) => p !== 'make_member_links') });
    socket.send(changed);
    expect(await within(region).findByText(/Since you read them/)).toBeDefined();
    fireEvent.click(within(region).getByRole('button', { name: 'Discard changes' }));
    expect(within(region).queryByText(/Since you read them/)).toBeNull();
    expect(within(region).queryByText(/The permissions changed on the server/)).toBeNull();
  });

  it('works out what moved between two documents', () => {
    expect(movedCells(permissionsDoc(1), permissionsDoc(2, { operator: operatorRights.filter((p) => p !== 'manage_channels') }))).toEqual(['Manage channels for operators is now off']);
  });
});

describe('the Server section’s summary lines', () => {
  it('link to the cell, and bring it into focus', async () => {
    serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    const region = await open(ownerToken);
    const lines = await screen.findByRole('list', { name: 'What members have' });
    fireEvent.click(within(lines).getByRole('button', { name: 'everyone' }));
    await waitFor(() => {
      expect(document.activeElement).toBe(cell(region, 'see_member_directory', 'member'));
    });
  });
});

describe('the table on a wide window', () => {
  it('keeps focus on a cell when the parent renders again', async () => {
    serve(owner, { 'GET /v1/permissions': () => json(permissionsDoc(2)) });
    window.sessionStorage.setItem(key, ownerToken);
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'Settings' });
    cleanup();
    const props = { origin, permissionsChanged: 0, directoryChanged: 0, onChannelsChanged: () => undefined, onUnauthorized: () => undefined, channels: [] as [] };
    const view = render(<AdminConsole {...props} who={owner} />);
    const input = await screen.findByRole<HTMLInputElement>('checkbox', { name: 'Use voice, Members' });
    input.focus();
    expect(document.activeElement).toBe(input);
    view.rerender(<AdminConsole {...props} who={owner} names={new Map([[1, 'owner']])} />);
    view.rerender(<AdminConsole {...props} who={{ ...owner }} />);
    expect(document.activeElement).toBe(input);
  });
});

describe('the phone layout', () => {
  function phoneProps() {
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => ({ matches: true, addEventListener: () => undefined, removeEventListener: () => undefined })),
    );
    return { origin, permissionsChanged: 0, directoryChanged: 0, onChannelsChanged: () => undefined, onUnauthorized: () => undefined, channels: [] as [] };
  }

  async function phonePage(who: WhoAmIResponseV1, routes: Record<string, Route> = {}) {
    const calls = serve(who, { 'GET /v1/permissions': () => json(permissionsDoc(2)), ...routes });
    window.sessionStorage.setItem(key, ownerToken);
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'Settings' });
    cleanup();
    const props = phoneProps();
    const view = render(<AdminConsole {...props} who={who} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Roles and permissions' }));
    const list = await screen.findByRole('list', { name: 'Members permissions' });
    return { calls, view, list, props };
  }

  it('shows one list with a switch per row, a segmented control, and the Owners column as one line', async () => {
    const { list } = await phonePage(owner);
    expect(screen.queryByRole('table')).toBeNull();
    expect(within(list).getAllByRole('switch')).toHaveLength(catalog.length);
    expect(list.textContent).toContain(ownersLine);
    const group = screen.getByRole('group', { name: 'Column' });
    expect(within(group).getByRole('button', { name: 'Members' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(within(group).getByRole('button', { name: 'Operators' }));
    const operators = await screen.findByRole('list', { name: 'Operators permissions' });
    expect(within(operators).getAllByRole('switch')).toHaveLength(catalog.length);
    // The same locking, with its reason in view (no hover on a phone).
    const fixed = within(operators).getByRole('switch', { name: 'See the member directory, Operators' });
    expect(fixed.getAttribute('aria-disabled')).toBe('true');
    expect(document.getElementById(fixed.getAttribute('aria-describedby') ?? '')?.textContent).toBe('Fixed: operators always hold this permission.');
  });

  it('puts every switch inside a label, so the target is the whole row', async () => {
    const { list } = await phonePage(owner);
    for (const input of within(list).getAllByRole('switch')) expect(input.closest('label')?.className).toContain('switch-row');
  });

  it('sends a changed switch as a patch for the column chosen', async () => {
    const { calls } = await phonePage(owner, { 'PUT /v1/permissions': () => json(permissionsDoc(3, { operator: operatorRights.filter((p) => p !== 'manage_channels') })) });
    fireEvent.click(screen.getByRole('button', { name: 'Operators' }));
    fireEvent.click(await screen.findByRole('switch', { name: 'Manage channels, Operators' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    expect(await screen.findByText('Saved.')).toBeDefined();
    expect(puts(calls).map((c) => c.body)).toEqual([{ revision: 2, changes: [{ permission: 'manage_channels', role: 'operator', granted: false }] }]);
  });

  it('keeps focus on a switch when the parent renders again', async () => {
    const { view, props } = await phonePage(owner);
    const input = screen.getByRole<HTMLInputElement>('switch', { name: 'Use voice, Members' });
    input.focus();
    expect(document.activeElement).toBe(input);
    view.rerender(<AdminConsole {...props} who={owner} names={new Map([[1, 'owner']])} />);
    view.rerender(<AdminConsole {...props} who={{ ...owner }} />);
    expect(document.activeElement).toBe(input);
  });
});
