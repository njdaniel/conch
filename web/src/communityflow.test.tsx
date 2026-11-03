// Issue #991: a member's whole page against a stand-in conchd (fetch): the
// directory, joining a public channel and reading its full history (an old
// guest's message still under the guest's name), the lock on a private
// channel's heading, the community member panel, and leaving. Whatever the
// stand-in does not list is never shown. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const key = 'conch.web.token';
const at = '2026-11-01T00:00:00Z';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

const me = {
  id: 2,
  kind: 'human',
  name: 'ada',
  role: 'member',
  permissions: ['create_public_channels', 'create_private_channels', 'see_member_directory'],
};
const general = { id: 1, name: 'general', created_at: at, visibility: 'public', default: true };
const ideas = { id: 2, name: 'ideas', created_at: at, visibility: 'public' };
const vault = { id: 3, name: 'vault', created_at: at, visibility: 'private', manages: true };

const message = (id: number, channel_id: number, body: string, extra: Record<string, unknown> = {}) => ({
  schema: 'conch.message.v2',
  id,
  channel_id,
  author_id: 1,
  created_at: at,
  body,
  ...extra,
});

interface World {
  mine: unknown[];
  directory: unknown[];
  calls: { method: string; path: string; query: string }[];
  leaveAnswer: Response | null;
}

function serve(world: World) {
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      const path = url.pathname;
      const method = init?.method ?? 'GET';
      world.calls.push({ method, path, query: url.search });
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json(me));
      if (method === 'GET' && path === '/v1/channels') return Promise.resolve(json({ channels: world.mine }));
      if (path === '/v1/directory/channels') return Promise.resolve(json({ channels: world.directory }));
      if (path === '/v1/directory/members') {
        return Promise.resolve(
          json({
            members: [
              { id: 1, kind: 'human', name: 'nick', role: 'operator', created_at: at },
              { id: 2, kind: 'human', name: 'ada', role: 'member', created_at: at },
              { id: 9, kind: 'agent', name: 'builder', created_at: at },
            ],
          }),
        );
      }
      if (method === 'POST' && path === '/v1/channels/ideas/join') {
        world.mine = [...world.mine, ideas];
        world.directory = world.directory.map((c) => ((c as { name: string }).name === 'ideas' ? { ...ideas, joined: true } : c));
        return Promise.resolve(
          json({ channel: ideas, member: { principal_id: 2, created_at: at, name: 'ada', kind: 'human', role: 'member', can_decide: false, manages: false } }, 201),
        );
      }
      if (method === 'POST' && path === '/v1/channels/ideas/leave') {
        if (world.leaveAnswer !== null) return Promise.resolve(world.leaveAnswer);
        world.mine = world.mine.filter((c) => (c as { name: string }).name !== 'ideas');
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      const messages = /^\/v2\/channels\/([^/]+)\/messages$/.exec(path);
      if (messages !== null) {
        if (messages[1] === 'ideas') {
          // The channel's whole history, from before this member joined: a
          // person's message and an old guest's, newest first.
          return Promise.resolve(
            json({
              messages: [
                message(12, 2, 'the guest says hello', { author_id: 40, guest_author: { handle: 'guest-a7k4m2q9x3h5t8wz', label: 'Pat', tag: 'a7k4' } }),
                message(11, 2, 'welcome to ideas, history included'),
              ],
            }),
          );
        }
        return Promise.resolve(json({ messages: [] }));
      }
      if (/^\/v1\/channels\/[^/]+\/members$/.test(path)) return Promise.resolve(json({ members: [] }));
      return Promise.resolve(json({}, 404));
    }),
  );
}

let world: World;

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
  window.localStorage.setItem(key, secret);
  world = {
    mine: [general, vault],
    directory: [
      { ...general, joined: true },
      { ...ideas, joined: false },
      { ...vault, joined: true },
    ],
    calls: [],
    leaveAnswer: null,
  };
  serve(world);
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const nav = () => screen.getByRole('navigation', { name: 'Channels' });
const heading = () => within(screen.getByRole('region', { name: 'Conversation' })).getByRole('heading', { level: 2 });

async function browse() {
  render(<App origin={origin} />);
  await within(await screen.findByRole('navigation', { name: 'Channels' })).findByRole('button', { name: 'Browse channels' });
  await act(() => new Promise((r) => setTimeout(r, 0)));
  fireEvent.click(within(nav()).getByRole('button', { name: 'Browse channels' }));
  await within(nav()).findByRole('list', { name: 'All channels' });
}

describe('a member’s page', () => {
  it('joins a public channel from the directory, opens it, and reads its full history with an old guest still the guest', async () => {
    await browse();
    fireEvent.click(within(nav()).getByRole('button', { name: 'Join ideas' }));
    await within(nav()).findByText('Joined #ideas');
    expect(world.calls.filter((c) => c.method === 'POST' && c.path === '/v1/channels/ideas/join')).toHaveLength(1);
    // The channel is in the list and open.
    await waitFor(() => {
      expect(heading().textContent).toBe('ideas');
    });
    expect(within(nav()).getAllByRole('button', { name: 'ideas' }).some((b) => b.getAttribute('aria-current') === 'true')).toBe(true);
    // Its history is read newest page first, with no lower bound.
    const log = await screen.findByRole('log', { name: 'Messages' });
    expect(await within(log).findByText('welcome to ideas, history included')).toBeDefined();
    expect(within(log).getByText('the guest says hello')).toBeDefined();
    expect(log.textContent).toContain('Pat');
    const read = world.calls.find((c) => c.path === '/v2/channels/ideas/messages');
    expect(read?.query).not.toContain('after=');
    // A public channel has no lock beside its heading.
    expect(document.querySelector('.conv-head .private-mark')).toBeNull();
  });

  it('puts a lock, with “Private”, before a private channel’s heading and in its row, and none for a public one', async () => {
    render(<App origin={origin} />);
    await screen.findByRole('button', { name: 'general' });
    expect(document.querySelector('.conv-head .private-mark')).toBeNull();
    fireEvent.click(within(await screen.findByRole('navigation', { name: 'Channels' })).getByRole('button', { name: 'vault' }));
    await waitFor(() => {
      expect(heading().textContent).toBe('vault');
    });
    const mark = document.querySelector('.conv-head .private-mark');
    expect(mark?.querySelector('svg')?.getAttribute('aria-hidden')).toBe('true');
    expect(mark?.querySelector('.vh')?.textContent).toBe('Private');
    // The heading keeps its name.
    expect(screen.getByRole('heading', { name: 'vault', level: 2 })).toBeDefined();
  });

  it('lists the community in its panel with the agent marked, beside the channel’s own people', async () => {
    render(<App origin={origin} />);
    const list = await screen.findByRole('list', { name: 'People' });
    const items = within(list).getAllByRole('listitem');
    expect(items.map((li) => li.querySelector('.community-name')?.textContent)).toEqual(['nick', 'ada', 'builder']);
    expect(within(items[2] as HTMLElement).getByText('Agent')).toBeDefined();
    expect(within(items[1] as HTMLElement).getByText('You')).toBeDefined();
    expect(within(items[0] as HTMLElement).getByText('Operator')).toBeDefined();
  });

  it('leaves a joined public channel, which goes from the list, and the open one gives way', async () => {
    world.mine = [general, ideas];
    world.directory = [
      { ...general, joined: true },
      { ...ideas, joined: true },
    ];
    window.location.hash = '#/c/ideas';
    await browse();
    await waitFor(() => {
      expect(heading().textContent).toBe('ideas');
    });
    fireEvent.click(within(nav()).getByRole('button', { name: 'Leave ideas' }));
    await within(nav()).findByText('Left #ideas');
    await waitFor(() => {
      expect(heading().textContent).toBe('general');
    });
    expect(world.calls.filter((c) => c.path === '/v1/channels/ideas/leave')).toHaveLength(1);
    expect(within(nav()).getByRole('list', { name: 'All channels' })).toBeDefined();
  });

  it('shows the last member of a private channel why they cannot leave, in the page’s words', async () => {
    world.mine = [general, ideas];
    world.directory = [
      { ...general, joined: true },
      { ...ideas, joined: true, visibility: 'private' },
    ];
    world.mine = [general, { ...ideas, visibility: 'private' }];
    world.leaveAnswer = json({ code: 'channel_last_member', message: 'you are the last member of this private channel: someone else must be added before you leave' }, 409);
    await browse();
    fireEvent.click(within(nav()).getByRole('button', { name: 'Leave ideas' }));
    fireEvent.click(within(screen.getByRole('group', { name: 'Leave ideas?' })).getByRole('button', { name: 'Yes, leave' }));
    await within(nav()).findByText('You are the last member of this private channel. Add someone else before you leave.');
    expect(within(nav()).getAllByRole('button', { name: 'ideas' }).length).toBeGreaterThan(0);
  });
});
