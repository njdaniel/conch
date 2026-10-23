// Renaming a channel in the browser (issue #728), against a stand-in conchd
// that keeps a channel list, answers every name-addressed route by the names
// it holds now (an old name is channel_not_found, as on the server), and
// records each request; and a fake WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const key = 'conch.web.token';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-21T00:00:00Z';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Json = Record<string, unknown>;
type Call = { readonly method: string; readonly path: string; readonly body: unknown };
type Channel = { id: number; name: string; created_at: string };
type Who = { id: number; kind: string; name: string; role: string };

const operator: Who = { id: 1, kind: 'human', name: 'nick', role: 'operator' };
const member: Who = { id: 2, kind: 'human', name: 'ada', role: 'member' };

const message = (id: number, body: string, channel_id = 7, author_id = 3): Json => ({
  schema: 'conch.message.v2',
  id,
  channel_id,
  author_id,
  created_at: at,
  body,
});

class FakeSocket {
  static all: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
  closed = false;
  constructor(readonly url: string) {
    FakeSocket.all.push(this);
  }
  close() {
    this.closed = true;
  }
  open() {
    act(() => {
      this.onopen?.();
    });
  }
  send(doc: unknown) {
    act(() => {
      this.onmessage?.({ data: JSON.stringify(doc) });
    });
  }
  get path() {
    return new URL(this.url).pathname;
  }
  get channel() {
    return new URL(this.url).searchParams.get('channel');
  }
}

const sockets = (path: string) => FakeSocket.all.filter((s) => s.path === path);
const chatSockets = () => sockets('/v2/ws');
const eventSockets = () => sockets('/v1/events/ws');

interface Server {
  me: Who;
  channels: Channel[];
  calls: Call[];
  // What PUT /v1/channels/{id}/name answers; by default the server's rule:
  // the name changed when `from` is the channel's name now.
  rename: (id: number, body: { name: string; from: string }) => Promise<Response>;
  // A post answers this, when set, instead of the message.
  postAnswer: (() => Promise<Response>) | null;
  nextId: number;
}

let server: Server;

const byName = (name: string) => server.channels.find((c) => c.name === name);
const notFound = () => Promise.resolve(json({ code: 'channel_not_found', message: 'channel not found' }, 404));

// applyRename is what conchd does with a rename it accepts.
function applyRename(id: number, body: { name: string; from: string }): Promise<Response> {
  const channel = server.channels.find((c) => c.id === id);
  if (channel === undefined) return notFound();
  if (channel.name !== body.name && channel.name !== body.from) {
    return Promise.resolve(json({ code: 'channel_name_stale', message: 'channel was renamed' }, 409));
  }
  channel.name = body.name;
  return Promise.resolve(json({ channel: { ...channel } }));
}

function install(me: Who = operator) {
  server = {
    me,
    channels: [
      { id: 7, name: 'general', created_at: at },
      { id: 8, name: 'ops', created_at: at },
    ],
    calls: [],
    rename: applyRename,
    postAnswer: null,
    nextId: 10,
  };
  FakeSocket.all = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      const path = url.pathname;
      const method = init?.method ?? 'GET';
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      server.calls.push({ method, path: `${path}${url.search}`, body });
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json(server.me));
      if (path === '/v1/channels') return Promise.resolve(json({ channels: server.channels.map((c) => ({ ...c })) }));
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (path === '/v1/read-state') return Promise.resolve(json({ read_state: [] }));
      if (path === '/v1/sessions' && method === 'POST') {
        return Promise.resolve(
          json(
            {
              schema: 'conch.session.v1',
              id: 100,
              principal_id: server.me.id,
              kind: 'human',
              name: server.me.name,
              client: 'browser',
              state: 'online',
              status: 'online',
              started_at: at,
              last_seen_at: at,
              interventions: [],
              outcome_unknown: false,
            },
            201,
          ),
        );
      }
      if (path === '/v1/runs') return Promise.resolve(json({ runs: [] }));
      const renaming = /^\/v1\/channels\/(\d+)\/name$/.exec(path);
      if (renaming?.[1] !== undefined && method === 'PUT') return server.rename(Number(renaming[1]), body as { name: string; from: string });
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages|read-state)$/.exec(path);
      if (m?.[1] !== undefined) {
        const channel = byName(decodeURIComponent(m[1]));
        if (channel === undefined) return notFound();
        switch (m[2]) {
          case 'members':
            return Promise.resolve(json({ members: [{ principal_id: server.me.id, created_at: at, name: server.me.name, kind: 'human' }] }));
          case 'nets':
            return Promise.resolve(json({ nets: [] }));
          case 'read-state': {
            const id = (body as { last_read_id: number }).last_read_id;
            return Promise.resolve(json({ schema: 'conch.read_state.v1', channel_id: channel.id, last_read_id: id, unread: 0, updated_at: at }));
          }
          default:
            if (method === 'POST') {
              if (server.postAnswer !== null) return server.postAnswer();
              const text = (body as { body: string }).body;
              return Promise.resolve(json({ message: message(server.nextId++, text, channel.id, server.me.id) }, 201));
            }
            return Promise.resolve(json({ messages: channel.id === 7 ? [message(2, 'two'), message(1, 'one')] : [] }));
        }
      }
      return Promise.resolve(json({}, 404));
    }),
  );
}

const nav = () => screen.getByRole('navigation', { name: 'Channels' });
const field = () => within(screen.getByRole('form', { name: 'Rename channel' })).getByLabelText<HTMLInputElement>('New name');
const renames = () => server.calls.filter((c) => c.method === 'PUT' && /^\/v1\/channels\/\d+\/name$/.test(c.path));
const historyReads = () => server.calls.filter((c) => c.method === 'GET' && c.path.startsWith('/v2/channels/') && c.path.includes('/messages'));

// open signs in on #general, opens the chat and events sockets, and lets the
// page settle; it hands back the chat socket and the events socket.
async function open(): Promise<{ chat: FakeSocket; events: FakeSocket }> {
  window.location.hash = '#/c/general';
  render(<App origin={origin} />);
  await screen.findByRole('log', { name: 'Messages' });
  await waitFor(() => {
    expect(chatSockets().length).toBe(1);
    expect(eventSockets().length).toBe(1);
  });
  const chat = chatSockets()[0] as FakeSocket;
  const events = eventSockets()[0] as FakeSocket;
  chat.open();
  events.open();
  await waitFor(() => {
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
  });
  // Landing put the channel in the address; jsdom tells the page of it a
  // task later, which must not arrive after a rename.
  await act(() => new Promise((r) => setTimeout(r, 0)));
  return { chat, events };
}

// openForm opens the rename form of the open channel.
function openForm(name: string): HTMLElement {
  const toggle = within(nav()).getByRole('button', { name: `Rename #${name}` });
  fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  return screen.getByRole('form', { name: 'Rename channel' });
}

function type(name: string) {
  fireEvent.change(field(), { target: { value: name } });
}

// movedTo checks that the page shows the channel by its new name, as the open
// one, in the list, the heading, the composer, the address and the tab's memory.
async function movedTo(name: string, old: string) {
  const button = await within(nav()).findByRole('button', { name });
  expect(button.getAttribute('aria-current')).toBe('true');
  expect(within(nav()).queryByRole('button', { name: old })).toBeNull();
  expect(within(screen.getByRole('region', { name: 'Conversation' })).getByRole('heading', { level: 2 }).textContent).toContain(name);
  expect(screen.getByLabelText(`Message #${name}`)).toBeDefined();
  // The address and the tab's memory are written by an effect after the
  // render that shows the new name, which may not have run yet (issue #804).
  await waitFor(() => {
    expect(window.location.hash).toBe(`#/c/${name}`);
    expect(window.sessionStorage.getItem('conch.web.lastChannel')).toBe(name);
  });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.location.hash = '';
  window.sessionStorage.setItem(key, secret);
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the rename control', () => {
  it('is offered to an operator, for the open channel', async () => {
    await open();
    expect(within(nav()).getByRole('button', { name: 'Rename #general' })).toBeDefined();
    expect((document.getElementById('rename-channel-form') as HTMLFormElement).hidden).toBe(true);
  });

  it('is not offered to a member', async () => {
    window.sessionStorage.clear();
    window.localStorage.setItem(key, secret);
    install(member);
    await open();
    expect(within(nav()).queryByRole('button', { name: /^Rename/ })).toBeNull();
    expect(document.getElementById('rename-channel-form')).toBeNull();
    expect(renames()).toEqual([]);
  });
});

describe('an operator renames the open channel', () => {
  it('sends one PUT by id with the name the list shows, and the page moves without reloading or reopening anything', async () => {
    const { chat, events } = await open();
    const socketsBefore = FakeSocket.all.length;
    const readsBefore = historyReads().length;
    // Something typed and not sent goes with the channel.
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'half a thought' } });

    openForm('general');
    expect(field().value).toBe('general');
    type('lounge');
    fireEvent.click(within(screen.getByRole('form', { name: 'Rename channel' })).getByRole('button', { name: 'Rename channel' }));
    await within(nav()).findByText('Renamed to #lounge');

    expect(renames()).toEqual([{ method: 'PUT', path: '/v1/channels/7/name', body: { name: 'lounge', from: 'general' } }]);
    await movedTo('lounge', 'general');
    expect((document.getElementById('rename-channel-form') as HTMLFormElement).hidden).toBe(true);
    expect(document.activeElement).toBe(within(nav()).getByRole('button', { name: 'Rename #lounge' }));
    // Nothing reloaded, no socket reopened, the history still on screen.
    await act(async () => {});
    expect(historyReads()).toHaveLength(readsBefore);
    expect(FakeSocket.all).toHaveLength(socketsBefore);
    expect(chat.closed || events.closed).toBe(false);
    expect(within(screen.getByRole('log', { name: 'Messages' })).getByText('two')).toBeDefined();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #lounge').value).toBe('half a thought');

    // The socket's next message lands in the renamed channel; the next post
    // goes to the new name.
    chat.send(message(3, 'three'));
    await within(screen.getByRole('log', { name: 'Messages' })).findByText('three');
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => {
      expect(server.calls.filter((c) => c.method === 'POST' && c.path.includes('/messages')).map((c) => c.path)).toEqual(['/v2/channels/lounge/messages']);
    });
    // Nothing after the rename names the old name.
    const after = server.calls.slice(server.calls.indexOf(renames()[0] as Call));
    expect(after.filter((c) => c.path.includes('general'))).toEqual([]);
  });

  it.each([
    [
      'a name the server will not take, with its rule',
      () => Promise.resolve(json({ code: 'invalid_request', message: 'schema: channel name must not begin or end with whitespace' }, 400)),
      'Channel name must not begin or end with whitespace.',
    ],
    [
      'a name already taken',
      () => Promise.resolve(json({ code: 'channel_exists', message: 'taken' }, 409)),
      'There is already a channel with that name, or one that reads the same, perhaps one you are not in. Choose another name.',
    ],
    ['no answer at all', () => Promise.reject(new TypeError('failed')), 'Cannot reach the server. Try again.'],
  ])('says so for %s, keeps the name as typed and renames nothing', async (_case, answer, line) => {
    server.rename = answer;
    await open();
    openForm('general');
    type(' lounge');
    fireEvent.click(within(screen.getByRole('form', { name: 'Rename channel' })).getByRole('button', { name: 'Rename channel' }));
    const said = await within(nav()).findByText(line);
    expect(said.getAttribute('role')).toBe('status');
    expect(screen.getByRole('form', { name: 'Rename channel' }).hidden).toBe(false);
    expect(field().value).toBe(' lounge');
    expect(within(nav()).getByRole('button', { name: 'general' }).getAttribute('aria-current')).toBe('true');
    expect(window.location.hash).toBe('#/c/general');
    expect(renames()).toHaveLength(1);
    // The same name may be sent again as it stands.
    expect(within(screen.getByRole('form', { name: 'Rename channel' })).getByRole('button', { name: 'Rename channel' }).hasAttribute('disabled')).toBe(false);
  });

  it('renamed by someone else meanwhile: says what it is called now, moves the page there and keeps the name typed', async () => {
    await open();
    // Another operator renamed it after this page read the list.
    const general = server.channels[0] as Channel;
    general.name = 'town-square';
    openForm('general');
    type('lounge');
    fireEvent.click(within(screen.getByRole('form', { name: 'Rename channel' })).getByRole('button', { name: 'Rename channel' }));
    await within(nav()).findByText('The channel was renamed to #town-square meanwhile; look again.');
    expect(renames()).toEqual([{ method: 'PUT', path: '/v1/channels/7/name', body: { name: 'lounge', from: 'general' } }]);
    await movedTo('town-square', 'general');
    expect(screen.getByRole('form', { name: 'Rename channel' }).hidden).toBe(false);
    expect(field().value).toBe('lounge');
    // Sent again, it now names what the channel is called.
    fireEvent.click(within(screen.getByRole('form', { name: 'Rename channel' })).getByRole('button', { name: 'Rename channel' }));
    await within(nav()).findByText('Renamed to #lounge');
    expect(renames()[1]?.body).toEqual({ name: 'lounge', from: 'town-square' });
    await movedTo('lounge', 'town-square');
  });

  it('sends one request however often it is submitted while waiting for the answer', async () => {
    let answer: (res: Response) => void = () => undefined;
    server.rename = () => new Promise<Response>((resolve) => (answer = resolve));
    await open();
    const form = openForm('general');
    type('lounge');
    // Twice in one tick, before a render could disable anything, then again.
    act(() => {
      fireEvent.submit(form);
      fireEvent.submit(form);
    });
    fireEvent.click(within(form).getByRole('button', { name: 'Rename channel' }));
    fireEvent.submit(form);
    await waitFor(() => {
      expect(renames()).toHaveLength(1);
    });
    expect(within(form).getByRole('button', { name: 'Rename channel' }).hasAttribute('disabled')).toBe(true);
    (server.channels[0] as Channel).name = 'lounge';
    answer(json({ channel: { id: 7, name: 'lounge', created_at: at } }));
    await within(nav()).findByText('Renamed to #lounge');
    expect(renames()).toHaveLength(1);
  });

  it('sends nothing for a blank name, or for the name it already has', async () => {
    await open();
    const form = openForm('general');
    type('   ');
    expect(within(form).getByRole('button', { name: 'Rename channel' }).hasAttribute('disabled')).toBe(true);
    fireEvent.submit(form);
    type('general');
    fireEvent.submit(form);
    await within(nav()).findByText('That is already the channel’s name.');
    expect(renames()).toEqual([]);
  });

  it('returns to sign-in on a 401', async () => {
    server.rename = () => Promise.resolve(json({}, 401));
    await open();
    openForm('general');
    type('lounge');
    fireEvent.click(within(screen.getByRole('form', { name: 'Rename channel' })).getByRole('button', { name: 'Rename channel' }));
    await screen.findByText('Signed out: the credential is no longer valid');
  });
});

describe('a rename made elsewhere', () => {
  it('arrives on the events socket and moves the page, without reloading or reopening anything', async () => {
    const { chat, events } = await open();
    const socketsBefore = FakeSocket.all.length;
    const readsBefore = historyReads().length;
    (server.channels[0] as Channel).name = 'lounge';
    events.send({ schema: 'conch.channel.v1', channel_id: 7, name: 'lounge', created_at: at });
    await movedTo('lounge', 'general');
    await act(async () => {});
    expect(historyReads()).toHaveLength(readsBefore);
    expect(FakeSocket.all).toHaveLength(socketsBefore);
    expect(chat.closed || events.closed).toBe(false);
    expect(renames()).toEqual([]);
  });

  it('a channel document for another channel, or one that does not validate, changes nothing', async () => {
    const { events } = await open();
    events.send({ schema: 'conch.channel.v1', channel_id: 8, name: 'ops-room', created_at: at });
    events.send({ schema: 'conch.channel.v1', channel_id: 7, created_at: at });
    await act(async () => {});
    expect(within(nav()).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['general', 'ops']);
    expect(window.location.hash).toBe('#/c/general');
  });

  it('missed by the page: a call answered channel_not_found reads the list again and finds the channel by id under its new name', async () => {
    await open();
    (server.channels[0] as Channel).name = 'lounge';
    const listsBefore = server.calls.filter((c) => c.path === '/v1/channels').length;
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await movedTo('lounge', 'general');
    expect(server.calls.filter((c) => c.path === '/v1/channels').length).toBe(listsBefore + 1);
    // The send that was refused stays, to be sent again; Retry goes to the new name.
    const log = screen.getByRole('log', { name: 'Messages' });
    fireEvent.click(await within(log).findByRole('button', { name: 'Retry' }));
    await within(log).findByText('hello');
    await waitFor(() => {
      expect(server.calls.filter((c) => c.method === 'POST' && c.path.includes('/messages')).map((c) => c.path)).toEqual([
        '/v2/channels/general/messages',
        '/v2/channels/lounge/messages',
      ]);
    });
  });

  it('missed by the page, and the channel no longer listed: it is lost, as before', async () => {
    await open();
    server.channels = server.channels.filter((c) => c.id !== 7);
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('You no longer have access to #general');
    expect(await within(nav()).findByRole('button', { name: 'ops' })).toBeDefined();
    expect(within(nav()).queryByRole('button', { name: 'general' })).toBeNull();
  });
});

describe('a reload after a rename', () => {
  it('lands on the new name; a tab opened on the old name lands on the fallback channel', async () => {
    (server.channels[0] as Channel).name = 'lounge';
    window.location.hash = '#/c/lounge';
    const first = render(<App origin={origin} />);
    expect((await within(await screen.findByRole('navigation', { name: 'Channels' })).findByRole('button', { name: 'lounge' })).getAttribute('aria-current')).toBe('true');
    first.unmount();

    window.sessionStorage.removeItem('conch.web.lastChannel');
    window.location.hash = '#/c/general';
    render(<App origin={origin} />);
    // #general is gone by that name: the page lands where it lands for a
    // hash that names nothing it can see, the first channel.
    expect((await within(await screen.findByRole('navigation', { name: 'Channels' })).findByRole('button', { name: 'lounge' })).getAttribute('aria-current')).toBe('true');
    await waitFor(() => {
      expect(window.location.hash).toBe('#/c/lounge');
    });
  });
});
