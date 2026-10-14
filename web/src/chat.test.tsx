// The chat as the person sees it, against a fake conchd (fetch) and a fake
// WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { clock } from './Conversation';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:42:00Z';

const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const channelList = {
  channels: [
    { id: 7, name: 'general', created_at: at },
    { id: 8, name: 'ops', created_at: at },
  ],
};
const members = {
  members: [
    { principal_id: 2, created_at: at, name: 'ada', kind: 'human' },
    { principal_id: 3, created_at: at, name: 'bot', kind: 'agent' },
    { principal_id: 4, created_at: at, name: '<b>mallory</b>', kind: 'human' },
  ],
};
const nets = {
  nets: [{ id: 5, channel_id: 7, name: '<i>red</i>', members: [], created_at: at }],
};

const message = (id: number, body: string, extra: Record<string, unknown> = {}, channel_id = 7) => ({
  schema: 'conch.message.v2',
  id,
  channel_id,
  author_id: 3,
  created_at: at,
  body,
  ...extra,
});

class FakeSocket {
  static all: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
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
  send(data: unknown) {
    act(() => {
      this.onmessage?.({ data: typeof data === 'string' ? data : JSON.stringify(data) });
    });
  }
  drop(code: number) {
    act(() => {
      this.onclose?.({ code });
    });
  }
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

interface Server {
  history: Record<string, unknown[]>;
  posts: { channel: string; body: string }[];
  post: (channel: string, body: string) => Response | Promise<Response>;
  whoami: () => Response;
  ticketCalls: number;
  channels: { id: number; name: string; created_at: string }[];
  endless: boolean;
  historyCalls: number;
  whoamiCalls: number;
}

let server: Server;
let nextId = 100;

function install() {
  server = {
    history: { general: [], ops: [] },
    posts: [],
    post: (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201),
    whoami: () => json(member),
    ticketCalls: 0,
    channels: channelList.channels,
    endless: false,
    historyCalls: 0,
    whoamiCalls: 0,
  };
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      const auth = new Headers(init?.headers).get('Authorization');
      if (auth !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      const path = url.pathname;
      if (path === '/v1/whoami') {
        server.whoamiCalls++;
        return Promise.resolve(server.whoami());
      }
      if (path === '/v1/channels') return Promise.resolve(json({ channels: server.channels }));
      if (path === '/v1/ws-tickets') {
        server.ticketCalls++;
        return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      }
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[1] === undefined) return Promise.resolve(json({}, 404));
      const channel = decodeURIComponent(m[1]);
      if (m[2] === 'members') return Promise.resolve(json(members));
      if (m[2] === 'nets') return Promise.resolve(json(nets));
      if (init?.method === 'POST') {
        const body = (JSON.parse(init.body as string) as { body: string }).body;
        server.posts.push({ channel, body });
        return Promise.resolve(server.post(channel, body));
      }
      const after = Number(url.searchParams.get('after'));
      server.historyCalls++;
      if (server.endless) return Promise.resolve(json({ messages: [message(after + 1, `e${String(after + 1)}`)], next_after: after + 1 }));
      return Promise.resolve(json({ messages: (server.history[channel] ?? []).filter((x) => (x as { id: number }).id > after) }));
    }),
  );
}

async function openChat(hash = '') {
  window.sessionStorage.setItem('conch.web.token', secret);
  window.location.hash = hash;
  render(<App origin={origin} />);
  await screen.findByRole('log', { name: 'Messages' });
  await waitFor(() => {
    expect(FakeSocket.all.length).toBeGreaterThan(0);
  });
  return FakeSocket.all[FakeSocket.all.length - 1] as FakeSocket;
}

// connect opens the latest socket and waits for the composer to be usable.
async function connect(socket: FakeSocket) {
  socket.open();
  await waitFor(() => {
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
  });
}

const lines = () => within(screen.getByRole('log', { name: 'Messages' })).queryAllByRole('listitem').map((li) => li.textContent);

beforeEach(() => {
  window.sessionStorage.clear();
  FakeSocket.all = [];
  nextId = 100;
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('loading', () => {
  it('reads history, then opens the socket from the newest id with a ticket, and shows each message once', async () => {
    server.history.general = [message(1, 'one'), message(2, 'two')];
    const socket = await openChat();
    const url = new URL(socket.url);
    expect(url.protocol).toBe('ws:');
    expect(url.host).toBe(window.location.host);
    expect(url.pathname).toBe('/v2/ws');
    expect(url.searchParams.get('channel')).toBe('general');
    expect(url.searchParams.get('after')).toBe('2');
    expect(url.searchParams.get('ticket')).toBe(ticketValue);
    await connect(socket);
    // The socket replays what was posted between the history and the socket.
    socket.send(message(2, 'two'));
    socket.send(message(3, 'three'));
    socket.send(message(3, 'three'));
    await waitFor(() => {
      expect(lines()).toHaveLength(3);
    });
    expect(lines().map((l) => l.replace(/^\d\d:\d\d /, ''))).toEqual(['bot [agent]: one', 'bot [agent]: two', 'bot [agent]: three']);
  });

  it('draws in id order whatever order frames arrive', async () => {
    const socket = await openChat();
    await connect(socket);
    socket.send(message(5, 'five'));
    socket.send(message(4, 'four'));
    await waitFor(() => {
      expect(lines()).toHaveLength(2);
    });
    expect(lines()[0]).toContain('four');
  });

  it('writes HH:MM name [agent]: body, and #<id> for an author it does not know', async () => {
    server.history.general = [message(1, 'hello'), message(2, 'who am i', { author_id: 99 })];
    await openChat();
    await screen.findByText('hello');
    expect(lines()).toEqual([`${clock(at)} bot [agent]: hello`, `${clock(at)} #99: who am i`]);
  });

  it('counts and does not draw a frame that fails the schema or is for another channel', async () => {
    const socket = await openChat();
    await connect(socket);
    socket.send('not json');
    socket.send({ ...message(1, 'x'), extra: true });
    socket.send(message(2, 'elsewhere', {}, 8));
    socket.send(message(3, 'fine'));
    await screen.findByText('fine');
    expect(lines()).toHaveLength(1);
    expect(screen.getByText('3 unreadable messages were ignored')).toBeDefined();
  });

  it('says so when the history cannot be read, and tries again on a click', async () => {
    const real = globalThis.fetch;
    let fail = true;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      if (fail && path.includes('/v2/channels/general/messages')) return Promise.resolve(json({}, 500));
      return real(input, init);
    });
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByText('Cannot load the messages');
    expect(FakeSocket.all).toHaveLength(0);
    fail = false;
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(1);
    });
  });
});

describe('rendering', () => {
  it('shows <img src=x onerror=alert(1)> as that text', async () => {
    const hostile = '<img src=x onerror=alert(1)>';
    server.history.general = [message(1, hostile)];
    await openChat();
    const body = await screen.findByText(hostile);
    expect(body.tagName).toBe('SPAN');
    expect(screen.getByRole('log').querySelector('img')).toBeNull();
    expect(screen.getByRole('log').innerHTML).toContain('&lt;img src=x onerror=alert(1)&gt;');
  });

  it('shows a name and an audience label as text too', async () => {
    server.history.general = [
      message(1, 'named', { author_id: 4 }),
      message(2, 'to a net', { audience: { kind: 'net', net_id: 5 } }),
      message(3, 'quiet', { audience: { kind: 'principals', principal_ids: [2, 3, 4] } }),
      message(4, 'lost net', { audience: { kind: 'net', net_id: 9 } }),
    ];
    await openChat();
    await screen.findByText('named');
    const log = screen.getByRole('log');
    expect(log.querySelector('b')).toBeNull();
    expect(log.querySelector('i')).toBeNull();
    expect(lines()).toEqual([
      `${clock(at)} <b>mallory</b>: named`,
      `→ net <i>red</i> ${clock(at)} bot [agent]: to a net`,
      `→ whisper to 2 ${clock(at)} bot [agent]: quiet`,
      `→ net #9 ${clock(at)} bot [agent]: lost net`,
    ]);
  });

  it('renders an approval message as a card with no controls', async () => {
    const approval = {
      id: 42,
      requester_id: 3,
      channel_id: 7,
      title: 'Approve trade: buy 0.5 BTC-USD',
      body: 'details',
      options: [{ id: 'approve', label: 'Approve', kind: 'approve' }],
      deadline: at,
      quorum: 1,
      state: 'pending',
      created_at: at,
    };
    server.history.general = [message(1, 'fallback body', { payload: { schema: 'conch.approval.v1', data: approval } })];
    await openChat();
    const card = await screen.findByRole('article', { name: 'Approval request' });
    expect(card.textContent).toContain('Approve trade: buy 0.5 BTC-USD');
    expect(card.textContent).toContain('Requested by bot');
    expect(card.textContent).toContain('State: pending');
    expect(card.textContent).toContain('Decide in the terminal client: conch approvals');
    expect(within(card).queryAllByRole('button')).toHaveLength(0);
    expect(card.querySelector('input, select, textarea, a')).toBeNull();
    expect(screen.queryByText('fallback body')).toBeNull();
    expect(screen.queryByRole('button', { name: /approve|reject/i })).toBeNull();
  });

  it('shows an approval payload it cannot read as the plain message', async () => {
    server.history.general = [message(1, 'plain words', { payload: { schema: 'conch.approval.v1', data: { nope: 1 } } })];
    await openChat();
    await screen.findByText('plain words');
    expect(screen.queryByRole('article')).toBeNull();
  });
});

describe('sending', () => {
  function type(text: string) {
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: text } });
  }
  const enter = () => {
    fireEvent.keyDown(screen.getByLabelText('Message #general'), { key: 'Enter' });
  };

  it('names the audience, sends on Enter but not on Shift+Enter, shows Sending then Sent once', async () => {
    const socket = await openChat();
    await connect(socket);
    let release: (r: Response) => void = () => undefined;
    server.post = () => new Promise<Response>((resolve) => (release = resolve));
    type('hello\nthere');
    fireEvent.keyDown(screen.getByLabelText('Message #general'), { key: 'Enter', shiftKey: true });
    expect(server.posts).toHaveLength(0);
    enter();
    await screen.findByText('Sending');
    expect(server.posts).toEqual([{ channel: 'general', body: 'hello\nthere' }]);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    // The live frame comes before the POST's answer, as it can.
    socket.send(message(100, 'hello\nthere', { author_id: 2 }));
    await act(async () => {
      release(json({ message: message(100, 'hello\nthere', { author_id: 2 }) }, 201));
      await Promise.resolve();
    });
    await screen.findByText('Sent');
    expect(lines()).toHaveLength(1);
    expect(screen.queryByText('Sending')).toBeNull();
  });

  it('does not send an empty message', async () => {
    await connect(await openChat());
    type('   ');
    enter();
    expect(server.posts).toHaveLength(0);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(true);
  });

  it('shows Failed with a Retry that sends again only when clicked', async () => {
    await connect(await openChat());
    server.post = () => json({ code: 'no' }, 403);
    type('try me');
    enter();
    await screen.findByText('Failed');
    await new Promise((r) => setTimeout(r, 50));
    expect(server.posts).toHaveLength(1);
    server.post = (_c, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts).toHaveLength(2);
    expect(screen.queryByText('Failed')).toBeNull();
  });

  it('shows Not confirmed when the answer never came, and never resends by itself', async () => {
    await connect(await openChat());
    server.post = () => Promise.reject(new TypeError('connection dropped'));
    type('maybe');
    enter();
    await screen.findByText('Not confirmed: check the conversation before retrying');
    await new Promise((r) => setTimeout(r, 50));
    expect(server.posts).toHaveLength(1);
    expect(screen.getByRole('button', { name: 'Retry' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByText('maybe')).toBeNull();
  });

  it('treats a server error as not confirmed, and a refusal as failed', async () => {
    await connect(await openChat());
    server.post = () => json({}, 502);
    type('behind a proxy');
    enter();
    await screen.findByText(/^Not confirmed/);
  });

  it('signs out on a 401', async () => {
    await connect(await openChat());
    server.post = () => json({}, 401);
    type('x');
    enter();
    await screen.findByText('Signed out: the credential is no longer valid');
  });
});

describe('drafts', () => {
  it('survive switching channels', async () => {
    await connect(await openChat());
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'half a thought' } });
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Message #ops');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #ops').value).toBe('');
    fireEvent.change(screen.getByLabelText('Message #ops'), { target: { value: 'ops note' } });
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    expect((await screen.findByLabelText<HTMLTextAreaElement>('Message #general')).value).toBe('half a thought');
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    expect((await screen.findByLabelText<HTMLTextAreaElement>('Message #ops')).value).toBe('ops note');
  });

  it('are kept in memory only', async () => {
    await connect(await openChat());
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'secretive draft' } });
    expect(JSON.stringify(Object.entries(window.sessionStorage))).not.toContain('secretive draft');
  });
});

describe('reconnecting', () => {
  it('says Reconnecting, marks the conversation out of date and disables the composer, then recovers with a fresh ticket and after', async () => {
    server.history.general = [message(1, 'one')];
    const first = await openChat();
    await connect(first);
    first.send(message(2, 'two'));
    await screen.findByText('two');
    first.drop(1006);
    await screen.findByText('This conversation may be out of date');
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Reconnecting');
    const box = screen.getByLabelText<HTMLTextAreaElement>('Message #general');
    expect(box.disabled).toBe(true);
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(true);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(2);
    }, { timeout: 3000 });
    const second = FakeSocket.all[1] as FakeSocket;
    expect(server.ticketCalls).toBe(2);
    expect(new URL(second.url).searchParams.get('after')).toBe('2');
    second.open();
    await waitFor(() => {
      expect(screen.queryByText('This conversation may be out of date')).toBeNull();
    });
    expect(screen.getByRole('status', { name: 'Status' }).textContent).toContain('Connected');
    expect(box.disabled).toBe(false);
  });

  it('removes the channel and says so on a 1008 when whoami still works', async () => {
    const socket = await openChat();
    await connect(socket);
    server.channels = channelList.channels.filter((c) => c.name !== 'general');
    socket.drop(1008);
    await screen.findByText('You no longer have access to #general');
    expect(screen.queryByRole('button', { name: 'general' })).toBeNull();
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'ops' }).getAttribute('aria-current')).toBe('true');
    });
    await new Promise((r) => setTimeout(r, 1300));
    expect(FakeSocket.all.filter((s) => new URL(s.url).searchParams.get('channel') === 'general')).toHaveLength(1);
  });

  it('does not remove the channel on a 1008 when it is still listed (a reader too slow), and retries after the backoff', async () => {
    const socket = await openChat();
    await connect(socket);
    socket.drop(1008);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(2);
    }, { timeout: 3000 });
    expect(screen.queryByText('You no longer have access to #general')).toBeNull();
    expect(screen.getByRole('button', { name: 'general' })).toBeDefined();
  });

  it('makes one further attempt, not many, when whoami fails after a 1008', async () => {
    const socket = await openChat();
    await connect(socket);
    server.whoami = () => json({}, 500);
    const before = server.whoamiCalls;
    socket.drop(1008);
    await new Promise((r) => setTimeout(r, 700));
    expect(FakeSocket.all).toHaveLength(1);
    expect(server.whoamiCalls - before).toBe(1);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(2);
    }, { timeout: 3000 });
    // The next wait is 2 s: nothing more arrives in the next second.
    await new Promise((r) => setTimeout(r, 1000));
    expect(FakeSocket.all).toHaveLength(2);
  });

  it('signs out on a 1008 when whoami is refused too', async () => {
    const socket = await openChat();
    await connect(socket);
    server.whoami = () => json({}, 401);
    socket.drop(1008);
    await screen.findByText('Signed out: the credential is no longer valid');
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
  });
});

describe('a channel with endless history', () => {
  it('is read for at most 20 pages, then follows the socket', async () => {
    server.endless = true;
    await openChat();
    expect(server.historyCalls).toBe(20);
    await waitFor(() => {
      expect(lines()).toHaveLength(20);
    });
  });
});

describe('the address bar', () => {
  it('never holds a ticket or the token', async () => {
    const socket = await openChat();
    await connect(socket);
    expect(socket.url).toContain(ticketValue);
    expect(window.location.href).not.toContain(ticketValue);
    expect(window.location.href).not.toContain(secret);
    expect(window.location.href).not.toContain('ticket');
    expect(window.location.search).toBe('');
    expect(window.location.hash).toBe('#/c/general');
    expect(document.body.innerHTML).not.toContain(ticketValue);
  });
});

describe('scrolling', () => {
  function geometry(el: HTMLElement, state: { height: number; top: number; client: number }) {
    Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => state.height });
    Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => state.client });
    Object.defineProperty(el, 'scrollTop', { configurable: true, get: () => state.top, set: (v: number) => (state.top = v) });
  }

  it('keeps the reader in place when older history is drawn above them', async () => {
    server.history.general = Array.from({ length: 150 }, (_, i) => message(i + 1, `m${String(i + 1)}`));
    await openChat();
    await screen.findByText('m150');
    expect(lines()).toHaveLength(100);
    const log = screen.getByRole('log');
    // The content is as tall as its lines: 10px each, plus 500px of padding.
    const geo = { top: 0, client: 300 };
    Object.defineProperty(log, 'scrollHeight', { configurable: true, get: () => 500 + 10 * log.querySelectorAll('li').length });
    Object.defineProperty(log, 'clientHeight', { configurable: true, get: () => geo.client });
    Object.defineProperty(log, 'scrollTop', { configurable: true, get: () => geo.top, set: (v: number) => (geo.top = v) });
    fireEvent.scroll(log);
    await waitFor(() => {
      expect(lines()).toHaveLength(150);
    });
    // Fifty lines of 10px were added above: the reader is moved down by that.
    expect(geo.top).toBe(500);
  });

  it('offers Jump to latest instead of moving a reader who has scrolled up', async () => {
    server.history.general = [message(1, 'one')];
    const socket = await openChat();
    await connect(socket);
    const log = screen.getByRole('log');
    const geo = { height: 1000, top: 100, client: 300 };
    geometry(log, geo);
    fireEvent.scroll(log);
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
    socket.send(message(2, 'two'));
    const jump = await screen.findByRole('button', { name: 'Jump to latest' });
    expect(geo.top).toBe(100);
    fireEvent.click(jump);
    expect(geo.top).toBe(1000);
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
  });

  it('follows new messages for a reader at the bottom', async () => {
    const socket = await openChat();
    await connect(socket);
    const log = screen.getByRole('log');
    const geo = { height: 1000, top: 700, client: 300 };
    geometry(log, geo);
    fireEvent.scroll(log);
    geo.height = 1020;
    socket.send(message(1, 'one'));
    await screen.findByText('one');
    expect(geo.top).toBe(1020);
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
  });

  it('has a Load older messages button for keyboard readers', async () => {
    server.history.general = Array.from({ length: 101 }, (_, i) => message(i + 1, `m${String(i + 1)}`));
    await openChat();
    await screen.findByText('m101');
    expect(screen.queryByText('m1')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('m1');
    expect(screen.queryByRole('button', { name: 'Load older messages' })).toBeNull();
  });
});
