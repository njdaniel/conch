// Preview as member in the page (issue #1517, docs/design/permission-preview.md
// §5, §7.2), against a fake conchd that answers the five previewable reads as
// the member named in Conch-Preview-As, and refuses every other request that
// carries it. The design's alice/bob world: private channels A (owner, alice)
// and B (owner, bob). No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { memberActions } from './memberActions';
import { call, previewCall } from './platform/operations';
import { http } from './platform/browser/http';
import type { Permission } from './schema/permission';
import type { ServerMemberV1, WhoAmIResponseV1 } from './schema/principal';

// Whether the administrator is in voice, and whether anything asked to leave it.
const voice = vi.hoisted(() => ({ in: false, left: 0 }));
vi.mock('./voice/useVoice', async (original) => {
  const real = await original<typeof import('./voice/useVoice')>();
  return {
    ...real,
    useInVoice: () => voice.in,
    leaveVoice: () => {
      voice.left++;
      return real.leaveVoice();
    },
  };
});

const origin = 'http://127.0.0.1:8080';
const key = 'conch.web.token';
const at = '2026-10-23T12:00:00.000Z';
const later = '2099-10-23T12:00:00.000Z';
const ticket = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';

const catalog: Permission[] = ['create_public_channels', 'create_private_channels', 'make_member_links', 'see_member_directory', 'use_voice', 'share_screen', 'manage_members', 'act_as_members', 'manage_channels', 'change_server_settings', 'read_audit_log'];
const auth = { scope: 'full', password_set: true, totp_enrolled: false, setup_complete: true, expires_at: later, idle_expires_at: later } as const;
const owner: WhoAmIResponseV1 = { id: 1, kind: 'human', name: 'owner', role: 'operator', owner: true, identity_class: 'operator', may_invite: true, voice_allowed: true, permissions: catalog, permissions_revision: 1, auth };

const person = (id: number, name: string, more: Partial<ServerMemberV1> = {}): ServerMemberV1 => ({ id, kind: 'human', name, role: 'member', created_at: at, voice_allowed: true, may_invite: true, ...more });
const everyone: ServerMemberV1[] = [person(1, 'owner', { role: 'operator', owner: true }), person(2, 'alice'), person(3, 'bob'), { id: 4, kind: 'agent', name: 'builder', created_at: at }];

const chA = { id: 1, name: 'channel-a', created_at: at, visibility: 'private' };
const chB = { id: 2, name: 'channel-b', created_at: at, visibility: 'private' };
const member = (id: number, name: string) => ({ principal_id: id, name, kind: 'human', created_at: at });

interface Seen {
  readonly method: string;
  readonly path: string;
  readonly headers: Record<string, string>;
}

interface World {
  // Member hiding on: alice sees only the people she shares a channel with.
  hiding: boolean;
  // What the server does with a preview read.
  refuse: { readonly status: number; readonly code: string; readonly permission?: string } | null;
  // The channel's people are gone between the list and their read.
  membersGone: boolean;
  unauthorized: boolean;
  who: WhoAmIResponseV1;
  // When set, what the previewed person's directory and channel people read answer.
  previewDirectory: ServerMemberV1[] | null;
  previewMembers: Record<string, unknown>[] | null;
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

let seen: Seen[] = [];
let world: World;

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

function serve(): void {
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      const url = new URL(raw, origin);
      const method = init?.method ?? 'GET';
      const headers = (init?.headers ?? {}) as Record<string, string>;
      seen.push({ method, path: url.pathname, headers });
      const as = headers['Conch-Preview-As'];
      const path = url.pathname;
      if (as !== undefined) {
        if (world.unauthorized) return Promise.resolve(json({ code: 'unauthorized', message: 'no' }, 401));
        if (world.refuse !== null) return Promise.resolve(json({ code: world.refuse.code, message: 'x', ...(world.refuse.permission === undefined ? {} : { permission: world.refuse.permission }) }, world.refuse.status));
        if (method !== 'GET') return Promise.resolve(json({ code: 'preview_read_only', message: 'x' }, 403));
        const alice = as === '2';
        if (path === '/v1/preview') {
          return Promise.resolve(json({ id: 2, kind: 'human', name: 'alice', role: 'member', owner: false, permissions: world.hiding ? ['use_voice', 'share_screen'] : ['use_voice', 'share_screen', 'see_member_directory', 'create_public_channels', 'create_private_channels', 'make_member_links'], permissions_revision: 3, may_invite: true, voice_allowed: true }));
        }
        if (path === '/v1/channels') return Promise.resolve(json({ channels: alice ? [chA] : [chA, chB] }));
        if (path === '/v1/directory/channels') return Promise.resolve(json({ channels: [{ ...chA, joined: true }] }));
        if (path === '/v1/directory/members' && world.previewDirectory !== null) return Promise.resolve(json({ members: world.previewDirectory, scope: 'everyone' }));
        if (path === '/v1/directory/members') {
          return Promise.resolve(json(world.hiding ? { members: [person(1, 'owner', { role: 'operator', owner: true }), person(2, 'alice')].map(stripped), scope: 'shared_channels' } : { members: everyone.map(stripped), scope: 'everyone' }));
        }
        if (path === '/v1/channels/channel-a/members' && world.membersGone) return Promise.resolve(json({ code: 'channel_not_found', message: 'x' }, 404));
        if (path === '/v1/channels/channel-a/members' && world.previewMembers !== null) return Promise.resolve(json({ members: world.previewMembers }));
        if (path === '/v1/channels/channel-a/members') return Promise.resolve(json({ members: [member(1, 'owner'), member(2, 'alice')] }));
        return Promise.resolve(json({ code: 'channel_not_found', message: 'x' }, 404));
      }
      if (path === '/v1/whoami') return Promise.resolve(json(world.who));
      if (path === '/v1/channels' && method === 'GET') return Promise.resolve(json({ channels: [chA, chB] }));
      if (path === '/v1/directory/channels') return Promise.resolve(json({ channels: [{ ...chA, joined: true }, { ...chB, joined: true }] }));
      if (path === '/v1/directory/members') return Promise.resolve(json({ members: everyone }));
      if (path === '/v1/permissions') return Promise.resolve(json({ revision: 2, rows: [], preset: 'group', presets: [], editable: [], warnings: [], updated_at: at }));
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket, expires_at: at }, 201));
      if (path === '/v1/read-state') return Promise.resolve(json({ read_state: [] }));
      if (path === '/v1/sessions' && method === 'POST') return Promise.resolve(json({ schema: 'conch.session.v1', id: 100, principal_id: 1, kind: 'human', name: 'owner', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201));
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[2] === 'members' && method === 'GET') return Promise.resolve(json({ members: [member(1, 'owner'), member(m[1] === 'channel-b' ? 3 : 2, m[1] === 'channel-b' ? 'bob' : 'alice')] }));
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') return Promise.resolve(json({ messages: [] }));
      return Promise.resolve(json({}, 404));
    }),
  );
}

// stripped is a member as a viewer without manage_members is told of them.
function stripped(m: ServerMemberV1): ServerMemberV1 {
  const rest = { ...m };
  delete rest.voice_allowed;
  delete rest.may_invite;
  delete rest.invited_by;
  return rest;
}

beforeEach(() => {
  seen = [];
  voice.in = false;
  voice.left = 0;
  FakeSocket.all = [];
  world = { hiding: true, refuse: null, membersGone: false, unauthorized: false, who: owner, previewDirectory: null, previewMembers: null };
  serve();
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

async function openMembers(): Promise<HTMLElement> {
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

async function previewAlice(): Promise<HTMLElement> {
  const region = await openMembers();
  const row = rowOf(region, 'alice');
  fireEvent.click(within(row).getByRole('button', { name: 'Actions for alice' }));
  fireEvent.click(await within(row).findByRole('menuitem', { name: 'Preview as member' }));
  return screen.findByRole('region', { name: 'Preview' });
}

const previewReads = () => seen.filter((s) => s.headers['Conch-Preview-As'] !== undefined);
const snapshot = () => JSON.stringify([Object.entries(window.localStorage).sort(), Object.entries(window.sessionStorage).sort()]);

describe('the way in', () => {
  it('is offered on a person to a holder of both permissions, and on no agent', async () => {
    const region = await openMembers();
    for (const name of ['alice', 'bob']) {
      fireEvent.click(within(rowOf(region, name)).getByRole('button', { name: `Actions for ${name}` }));
      expect(within(rowOf(region, name)).getByRole('menuitem', { name: 'Preview as member' })).toBeTruthy();
      fireEvent.keyDown(within(rowOf(region, name)).getByRole('menu'), { key: 'Escape' });
    }
    fireEvent.click(within(rowOf(region, 'builder')).getByRole('button', { name: 'Actions for builder' }));
    expect(within(rowOf(region, 'builder')).queryByRole('menuitem', { name: 'Preview as member' })).toBeNull();
  });

  it('is not offered for oneself or an owner by an operator, nor without manage_channels', () => {
    const operator: WhoAmIResponseV1 = { ...owner, id: 9, owner: false, permissions: catalog };
    const noChannels: WhoAmIResponseV1 = { ...operator, permissions: catalog.filter((p) => p !== 'manage_channels') };
    const ids = (who: WhoAmIResponseV1, target: ServerMemberV1) => memberActions(who, target).map((a) => a.id);
    expect(ids(operator, person(2, 'alice'))).toContain('preview');
    expect(ids(operator, person(9, 'self'))).not.toContain('preview');
    expect(ids(operator, person(5, 'boss', { role: 'operator', owner: true }))).toEqual([]);
    expect(ids(noChannels, person(2, 'alice'))).not.toContain('preview');
  });
});

describe('the header', () => {
  it('rides only on the five previewable reads, never on a write, and is never ambient', async () => {
    await previewAlice();
    const reads = previewReads();
    expect(reads.length).toBeGreaterThan(0);
    for (const r of reads) expect(r.method).toBe('GET');
    expect(new Set(reads.map((r) => r.path))).toEqual(new Set(['/v1/preview', '/v1/channels', '/v1/directory/channels', '/v1/directory/members', '/v1/channels/channel-a/members']));
    // Every request the page made as itself carries none.
    for (const s of seen.filter((x) => x.headers['Conch-Preview-As'] === undefined)) expect(s.headers['Conch-Preview-As']).toBeUndefined();
    expect(reads.every((r) => r.headers['Conch-Preview-As'] === '2')).toBe(true);
  });

  it('is refused by the client on any call that is not a previewable read', () => {
    expect(() => previewCall('postMessage', 2, { channel: 'x' })).toThrow();
    expect(() => previewCall('listMessages', 2, { channel: 'x' })).toThrow();
    expect(() => previewCall('whoami', 2)).toThrow();
    const fetchMock = vi.fn(() => Promise.resolve(new Response('{}')));
    vi.stubGlobal('fetch', fetchMock);
    const asked = { ...call('postMessage', { channel: 'x' }), previewAs: 2 };
    expect(() => http.send(asked, 'tok', { keepalive: false })).toThrow();
    expect(() => http.get({ ...call('listMessages', { channel: 'x' }), previewAs: 2 }, 'tok')).toThrow();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('the preview', () => {
  it.each([
    { hiding: true, people: ['owner', 'alice'], absent: ['bob'], scope: true },
    { hiding: false, people: ['owner', 'alice', 'bob', 'builder'], absent: [], scope: false },
  ])('shows only what alice sees (member hiding $hiding)', async ({ hiding, people, absent, scope }) => {
    world.hiding = hiding;
    const band = await previewAlice();
    expect(band.textContent).toContain('Previewing alice, read-only');
    expect(within(band).getByRole('button', { name: 'Exit preview' })).toBeTruthy();
    expect(within(band).getByRole('button', { name: 'Refresh' })).toBeTruthy();

    const channels = screen.getByRole('navigation', { name: 'Channels' });
    await within(channels).findByRole('button', { name: /^channel-a$/ });
    expect(within(channels).queryByRole('button', { name: /^channel-b$/ })).toBeNull();

    const community = await screen.findByRole('region', { name: 'Community members' });
    const names = Array.from(community.querySelectorAll('.community-name')).map((n) => n.textContent);
    expect(names).toEqual(people);
    for (const a of absent) expect(names).not.toContain(a);
    expect(community.textContent.includes('You see the people you share a channel with')).toBe(scope);
    // The page asked for no channel of B's, which alice cannot see.
    expect(previewReads().some((r) => r.path.includes('channel-b'))).toBe(false);
  });

  describe('identity marks', () => {
    const marksOf = (row: HTMLElement) => Array.from(row.querySelectorAll('[data-identity]')).map((m) => m.getAttribute('data-identity'));

    it('draws no Owner and no Operator the previewed payloads do not carry, whatever the administrator is', async () => {
      // The administrator (owner, operator) is the signed-in person; the
      // previewed rows omit owner and role altogether.
      world.previewDirectory = [
        { id: 1, kind: 'human', name: 'owner', created_at: at },
        { id: 2, kind: 'human', name: 'alice', created_at: at },
        { id: 4, kind: 'agent', name: 'builder', created_at: at },
      ];
      world.previewMembers = [member(1, 'owner'), member(2, 'alice')];
      await previewAlice();
      const community = await screen.findByRole('region', { name: 'Community members' });
      await within(community).findByText('builder');
      expect(marksOf(rowOf(community, 'owner'))).toEqual([]);
      expect(marksOf(rowOf(community, 'alice'))).toEqual(['you']);
      expect(marksOf(rowOf(community, 'builder'))).toEqual(['agent']);
      const channel = await screen.findByRole('region', { name: 'Channel members' });
      expect(marksOf(rowOf(channel, 'owner'))).toEqual([]);
      expect(marksOf(rowOf(channel, 'alice'))).toEqual(['you']);
    });

    it('keeps the marks the previewed payloads do carry', async () => {
      world.previewDirectory = [
        { id: 1, kind: 'human', name: 'owner', role: 'operator', owner: true, created_at: at },
        { id: 2, kind: 'human', name: 'alice', role: 'member', created_at: at },
        { id: 3, kind: 'human', name: 'bob', role: 'operator', created_at: at },
      ];
      world.previewMembers = [{ ...member(1, 'owner') }, { ...member(2, 'alice'), manages: true }];
      await previewAlice();
      const community = await screen.findByRole('region', { name: 'Community members' });
      await within(community).findByText('bob');
      expect(marksOf(rowOf(community, 'owner'))).toEqual(['owner', 'operator']);
      expect(marksOf(rowOf(community, 'bob'))).toEqual(['operator']);
      expect(marksOf(rowOf(community, 'alice'))).toEqual(['you']);
      const channel = await screen.findByRole('region', { name: 'Channel members' });
      expect(marksOf(rowOf(channel, 'alice'))).toEqual(['you', 'manager']);
    });
  });

  it('shows no messages, no unread badge and no presence, and a disabled composer', async () => {
    await previewAlice();
    expect(await screen.findByText('Messages are not shown in preview. Open the channel as yourself to read it.')).toBeTruthy();
    expect(screen.queryByRole('img', { name: /unread/ })).toBeNull();
    expect(screen.queryByText('Online')).toBeNull();
    expect(screen.queryByText('Offline')).toBeNull();
    const box = screen.getByRole('textbox');
    expect((box as HTMLTextAreaElement).disabled).toBe(true);
    expect(screen.getByText(/Read-only preview/)).toBeTruthy();
  });

  it('shows the controls alice holds, with a line for each she does not', async () => {
    await previewAlice();
    const controls = await screen.findByRole('region', { name: 'Controls' });
    expect(controls.textContent).toContain("Not available: Members can't create channels on this server.");
    expect(controls.textContent).toContain("Not available: Members can't make member links on this server.");
    expect(controls.textContent).toContain('alice could join voice here.');
    for (const b of within(controls).getAllByRole('button')) expect((b as HTMLButtonElement).disabled).toBe(true);
    const voice = screen.getByRole('region', { name: 'Voice preview' });
    expect(within(voice).getByRole<HTMLButtonElement>('button', { name: 'Join voice' }).disabled).toBe(true);
  });

  it('sends no write, makes no read mark, joins no voice and touches no storage while it is shown', async () => {
    const region = await openMembers();
    const before = snapshot();
    const row = rowOf(region, 'alice');
    fireEvent.click(within(row).getByRole('button', { name: 'Actions for alice' }));
    fireEvent.click(await within(row).findByRole('menuitem', { name: 'Preview as member' }));
    await screen.findByRole('region', { name: 'Preview' });
    const mark = seen.length;
    // The channel list, a refresh and a channel opened, in the preview.
    fireEvent.click(await screen.findByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(previewReads().filter((r) => r.path === '/v1/preview').length).toBeGreaterThan(1);
    });
    const during = seen.slice(mark);
    for (const s of during) {
      expect(s.method, `${s.method} ${s.path}`).toBe('GET');
      expect(s.path).not.toMatch(/voice|read-state|messages/);
    }
    expect(snapshot()).toBe(before);
    expect(seen.some((s) => s.path.includes('/voice/') && s.method === 'POST')).toBe(false);
  });

  it('is replaced, not merged, by Refresh', async () => {
    await previewAlice();
    await screen.findByRole('button', { name: /^channel-a$/ });
    const channels = screen.getByRole('navigation', { name: 'Channels' });
    expect(within(channels).queryByRole('button', { name: /^channel-b$/ })).toBeNull();
    // alice is later added to B: the next read lists both, and the old list is gone.
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const raw = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
        const headers = (init?.headers ?? {}) as Record<string, string>;
        const path = new URL(raw, origin).pathname;
        seen.push({ method: init?.method ?? 'GET', path, headers });
        if (headers['Conch-Preview-As'] === undefined) return Promise.resolve(json({}, 404));
        if (path === '/v1/preview') return Promise.resolve(json({ id: 2, kind: 'human', name: 'alice', role: 'member', owner: false, permissions: ['use_voice'], permissions_revision: 4, may_invite: true, voice_allowed: true }));
        if (path === '/v1/channels') return Promise.resolve(json({ channels: [chB] }));
        if (path === '/v1/directory/channels') return Promise.resolve(json({ channels: [] }));
        if (path === '/v1/directory/members') return Promise.resolve(json({ members: [person(2, 'alice')], scope: 'shared_channels' }));
        return Promise.resolve(json({ members: [member(2, 'alice')] }));
      }),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await within(channels).findByRole('button', { name: /^channel-b$/ });
    expect(within(channels).queryByRole('button', { name: /^channel-a$/ })).toBeNull();
  });
});

describe('voice', () => {
  it('leaves the entry disabled, with its reason, while the administrator is in voice, and starts nothing', async () => {
    voice.in = true;
    const region = await openMembers();
    const row = rowOf(region, 'alice');
    fireEvent.click(within(row).getByRole('button', { name: 'Actions for alice' }));
    const item = await within(row).findByRole('menuitem', { name: /Preview as member/ });
    expect(item.getAttribute('aria-disabled') === 'true' || (item as HTMLButtonElement).disabled).toBe(true);
    expect(row.textContent).toContain('Leave voice to start a preview.');
    fireEvent.click(item);
    expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
    expect(previewReads()).toEqual([]);
    expect(voice.left).toBe(0);
  });

  it('never leaves the administrator\'s voice to start, and ends a preview when voice starts under it', async () => {
    await previewAlice();
    expect(voice.left).toBe(0);
    voice.in = true;
    // Any render of the page: the server's word that the permissions changed.
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'))?.send({ schema: 'conch.permissions_changed.v1' });
    expect(await screen.findByText('Preview ended: voice started. Leave voice before previewing.')).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
    expect(voice.left).toBe(0);
  });
});

describe('the permission rules', () => {
  const sources = import.meta.glob<string>(['./preview/*.ts', './preview/*.tsx', './PreviewBanner.tsx', '!./preview/*.test.*'], { eager: true, query: '?raw', import: 'default' });

  it('are the page\'s own: no table of permissions in the preview components', () => {
    expect(Object.keys(sources).length).toBeGreaterThan(3);
    for (const [path, code] of Object.entries(sources)) {
      const text = code.replace(/\/\/.*$/gm, '');
      expect(text, path).not.toMatch(/permissions\s*(\?\.)?\s*(includes|some|has|find)\s*\(/);
      expect(text, path).not.toMatch(/switch\s*\([^)]*permission/i);
      expect(text, path).not.toMatch(/Record<\s*Permission/);
    }
  });

  it('are fed the preview target through canDo and memberActions', () => {
    const parts = Object.entries(sources).filter(([path]) => path.endsWith('PreviewParts.tsx') || path.endsWith('words.ts'));
    expect(parts.map(([, code]) => /\bcanDo\b|\bmemberActions\b/.test(code)).every(Boolean)).toBe(true);
  });
});

describe('teardown', () => {
  it('ends when the person signs out', async () => {
    await previewAlice();
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Sign out' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    await screen.findByRole('button', { name: 'Sign in' });
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
    });
  });

  it('Exit returns to the owner view, which shows both channels and the Members of the console', async () => {
    await previewAlice();
    fireEvent.click(await screen.findByRole('button', { name: 'Exit preview' }));
    const channels = await screen.findByRole('navigation', { name: 'Channels' });
    await within(channels).findByRole('button', { name: /^channel-b$/ });
    expect(within(channels).getByRole('button', { name: /^channel-a$/ })).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
    expect(screen.queryByText('Messages are not shown in preview. Open the channel as yourself to read it.')).toBeNull();
  });

  it('ends on a server refusal of a preview read, and says so', async () => {
    await previewAlice();
    world.refuse = { status: 403, code: 'preview_read_only' };
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByText('Preview ended: alice can no longer be previewed. The server refused.')).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
    await screen.findByRole('navigation', { name: 'Channels' });
  });

  it('ends, and signs the page out, on a 401', async () => {
    await previewAlice();
    world.unauthorized = true;
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => {
      expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
    });
    expect(await screen.findByRole('button', { name: /sign in/i })).toBeTruthy();
  });

  it('ends when the administrator loses manage_channels mid-preview', async () => {
    await previewAlice();
    world.who = { ...owner, owner: false, permissions: catalog.filter((p) => p !== 'manage_channels') };
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'))?.send({ schema: 'conch.permissions_changed.v1' });
    expect(await screen.findByText('Preview ended: you can no longer preview members.')).toBeTruthy();
    expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
  });

  it('a content-free signal reads the preview again, and a message frame is never applied to it', async () => {
    await previewAlice();
    const before = previewReads().filter((r) => r.path === '/v1/channels').length;
    await waitFor(() => {
      expect(FakeSocket.all.some((s) => s.url.includes('/v1/events/ws?'))).toBe(true);
    });
    for (const s of FakeSocket.all) s.open();
    FakeSocket.all.find((s) => s.url.includes('/v1/events/ws?'))?.send({ schema: 'conch.channels_changed.v1' });
    await waitFor(() => {
      expect(previewReads().filter((r) => r.path === '/v1/channels').length).toBeGreaterThan(before);
    });
    // A message frame on the administrator's own sockets: the page takes it as
    // the administrator's (it shows when the preview is exited), and never
    // into the preview.
    const frame = { schema: 'conch.message.v2', id: 900, channel_id: 1, author_id: 2, created_at: at, body: 'SECRET-MESSAGE-BODY' };
    for (const socket of FakeSocket.all) socket.send(frame);
    await waitFor(() => {
      expect(FakeSocket.all.length).toBeGreaterThan(0);
    });
    expect(screen.queryByText('SECRET-MESSAGE-BODY')).toBeNull();
    expect(screen.queryByRole('log', { name: 'Messages' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Exit preview' }));
    expect(await screen.findByText('SECRET-MESSAGE-BODY')).toBeTruthy();
  });

  it('says, when the first read is refused, that the preview never began, and why', async () => {
    world.refuse = { status: 403, code: 'permission_denied', permission: 'manage_channels' };
    const region = await openMembers();
    const row = rowOf(region, 'alice');
    fireEvent.click(within(row).getByRole('button', { name: 'Actions for alice' }));
    fireEvent.click(await within(row).findByRole('menuitem', { name: 'Preview as member' }));
    expect(await screen.findByText('Cannot preview alice. You need the permission to manage channels for that. Nothing was changed.')).toBeTruthy();
    expect(screen.queryByText(/Preview ended/)).toBeNull();
  });

  it('carries the code of a refusal mid-preview', async () => {
    await previewAlice();
    world.refuse = { status: 404, code: 'principal_not_found' };
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    expect(await screen.findByText(/^Preview ended: alice can no longer be previewed\. That person is gone/)).toBeTruthy();
  });

  it('takes a channel gone between the list and its people as no people, not a refusal', async () => {
    world.membersGone = true;
    await previewAlice();
    const members = await screen.findByRole('region', { name: 'Channel members' });
    expect(members.textContent).toContain('Nobody to show');
    expect(screen.getByRole('region', { name: 'Preview' })).toBeTruthy();
    expect(previewReads().filter((r) => r.path === '/v1/channels').length).toBeGreaterThan(1);
  });

  it('is gone after a reload: the page starts in the owner view', async () => {
    await previewAlice();
    cleanup();
    render(<App origin={origin} />);
    const channels = await screen.findByRole('navigation', { name: 'Channels' });
    await within(channels).findByRole('button', { name: /^channel-b$/ });
    expect(screen.queryByRole('region', { name: 'Preview' })).toBeNull();
  });
});
