// The chat as the person sees it, against a fake conchd (fetch) and a fake
// WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { pausedText } from './Composer';
import { clock } from './Conversation';
import { maxHeld, pageSize } from './state/messages';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:42:00Z';

// Without voice, so the page opens no voice socket beside the conversation's
// (issue #585): every socket and ticket counted here is the conversation's.
const member = { id: 2, kind: 'human', name: 'ada', role: 'member', voice_allowed: false };
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
  posts: { channel: string; body: string; audience?: unknown }[];
  nets: unknown;
  post: (channel: string, body: string) => Response | Promise<Response>;
  whoami: () => Response;
  ticketCalls: number;
  channels: { id: number; name: string; created_at: string }[];
  historyRequests: URL[];
  whoamiCalls: number;
  // firstPage, when set, is how many messages the first history read
  // returns whatever limit it asked for: a test that needs a nearly full
  // channel gets there without one render per page (#459).
  firstPage?: number;
  // netsHeld, when set, holds every nets answer until it resolves.
  netsHeld?: Promise<void>;
}

let server: Server;
let nextId = 100;

function install() {
  server = {
    history: { general: [], ops: [] },
    posts: [],
    nets,
    post: (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201),
    whoami: () => json(member),
    ticketCalls: 0,
    channels: channelList.channels,
    historyRequests: [],
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
      if (m[2] === 'nets') return (server.netsHeld ?? Promise.resolve()).then(() => json(server.nets));
      if (init?.method === 'POST') {
        const sent = JSON.parse(init.body as string) as { body: string; audience?: unknown };
        const body = sent.body;
        server.posts.push(sent.audience === undefined ? { channel, body } : { channel, body, audience: sent.audience });
        return Promise.resolve(server.post(channel, body));
      }
      // The server's read: ids below `before`, newest first, at most `limit`
      // (or `firstPage` for the test's first history read, of any channel).
      server.historyRequests.push(url);
      const before = url.searchParams.get('before');
      const asked = Number(url.searchParams.get('limit'));
      const limit = server.firstPage !== undefined && server.historyRequests.length === 1 ? server.firstPage : asked;
      if (url.searchParams.get('order') === 'asc') {
        const after = Number(url.searchParams.get('after') ?? '0');
        const visible = (server.history[channel] ?? []).filter((x) => (x as { id: number }).id > after);
        const page = visible.slice(0, limit);
        return Promise.resolve(json({ messages: page, ...(page.length === limit ? { next_after: (page.at(-1) as { id: number } | undefined)?.id ?? after } : {}) }));
      }
      const visible = (server.history[channel] ?? []).filter((x) => before === null || (x as { id: number }).id < Number(before));
      return Promise.resolve(json({ messages: visible.slice(-limit).reverse() }));
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

const lines = () => within(screen.getByRole('log', { name: 'Messages' })).queryAllByRole('listitem').map((li) => {
  const copy = li.cloneNode(true) as HTMLElement;
  // The Reply and Open thread buttons are controls, not message text.
  copy.querySelectorAll('button').forEach((button) => { button.remove(); });
  return copy.textContent;
});

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
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

// Issue #845: an edit arrives on the message socket as the message again, with
// edited_at. It changes the message held and adds the marker; one for a
// message the page does not hold is not drawn; a stale copy changes nothing.
describe('a live edit', () => {
  it('replaces the text in place, marks it, and is never a new message', async () => {
    server.history.general = [message(1, 'helo'), message(2, 'two')];
    const socket = await openChat();
    await connect(socket);
    socket.send(message(1, 'hello', { edited_at: '2026-10-14T10:43:00Z' }));
    await screen.findByText('hello');
    expect(screen.queryByText('helo')).toBeNull();
    expect(screen.getAllByText('(edited)')).toHaveLength(1);
    // An edit of a message older than anything held: dropped, not appended.
    socket.send(message(0, 'from before the page', { edited_at: '2026-10-14T10:43:00Z' }));
    // A stale copy of message 1 (the original, replayed) changes nothing.
    socket.send(message(1, 'helo'));
    socket.send(message(3, 'three'));
    await screen.findByText('three');
    expect(lines()).toHaveLength(3);
    expect(screen.queryByText('from before the page')).toBeNull();
    expect(screen.getByText('hello')).toBeTruthy();
  });
});

describe('an empty channel', () => {
  const emptyLine = 'No messages in #general yet. Say hello.';

  it('says so once its history has loaded with nothing in it', async () => {
    await openChat();
    await screen.findByText(emptyLine);
    expect(within(screen.getByRole('log', { name: 'Messages' })).getByText(emptyLine).tagName).toBe('P');
  });

  it('says nothing of the kind for a channel with messages', async () => {
    server.history.general = [message(1, 'one')];
    await openChat();
    await screen.findByText('one');
    expect(screen.queryByText(emptyLine)).toBeNull();
  });

  it('says nothing of the kind while the history is loading', async () => {
    const real = globalThis.fetch;
    let release: () => void = () => undefined;
    const held = new Promise<void>((resolve) => (release = resolve));
    vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      if (path.includes('/v2/channels/general/messages')) await held;
      return real(input, init);
    });
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByRole('log', { name: 'Messages' });
    await act(async () => {});
    expect(screen.queryByText(emptyLine)).toBeNull();
    release();
    await screen.findByText(emptyLine);
  });

  it('says nothing of the kind after the history could not be read', async () => {
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      if (path.includes('/v2/channels/general/messages')) return Promise.resolve(json({}, 500));
      return real(input, init);
    });
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByText('Cannot load the messages');
    expect(screen.queryByText(emptyLine)).toBeNull();
  });

  it('goes as soon as the first message is sending', async () => {
    const socket = await openChat();
    await connect(socket);
    await screen.findByText(emptyLine);
    server.post = () => new Promise<Response>(() => undefined);
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'hello' } });
    fireEvent.keyDown(screen.getByLabelText('Message #general'), { key: 'Enter' });
    await screen.findByText('Sending');
    expect(screen.queryByText(emptyLine)).toBeNull();
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
  async function openWithEvents(): Promise<FakeSocket> {
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (url.pathname === '/v1/sessions' && init?.method === 'POST') {
        return Promise.resolve(json({
          schema: 'conch.session.v1', id: 100, principal_id: 2, kind: 'human', name: 'ada', client: 'browser',
          state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false,
        }, 201));
      }
      if (url.pathname === '/v1/runs') return Promise.resolve(json({ runs: [] }));
      return real(input, init);
    });
    await connect(await openChat());
    await waitFor(() => {
      expect(FakeSocket.all.some((socket) => new URL(socket.url).pathname === '/v1/events/ws')).toBe(true);
    });
    const events = FakeSocket.all.find((socket) => new URL(socket.url).pathname === '/v1/events/ws') as FakeSocket;
    events.open();
    return events;
  }

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

  it('reconciles concurrent identical retries against one stored message only once', async () => {
    await connect(await openChat());
    let calls = 0;
    server.post = (_channel, body) => {
      calls += 1;
      if (calls === 1) server.history.general = [message(100, body, { author_id: 2 })];
      return Promise.reject(new TypeError('connection dropped'));
    };
    type('same');
    enter();
    type('same');
    enter();
    await waitFor(() => { expect(screen.getAllByRole('button', { name: 'Retry' })).toHaveLength(2); });
    nextId = 101;
    server.post = (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201);
    for (const retry of screen.getAllByRole('button', { name: 'Retry' })) fireEvent.click(retry);
    await waitFor(() => { expect(server.posts).toHaveLength(3); });
    await waitFor(() => { expect(screen.getAllByText('Sent')).toHaveLength(2); });
    expect(server.posts[2]).toEqual({ channel: 'general', body: 'same' });
    expect(lines().filter((line) => line.includes('same'))).toHaveLength(2);
  });

  it('invalidates an initial POST on rename and retries by id, not a recreated old name', async () => {
    const events = await openWithEvents();
    let answer: (response: Response) => void = () => undefined;
    server.post = () => new Promise<Response>((resolve) => { answer = resolve; });
    type('original only');
    enter();
    await screen.findByText('Sending');
    server.channels = [
      { id: 9, name: 'general', created_at: at },
      { id: 7, name: 'original', created_at: at },
      { id: 8, name: 'ops', created_at: at },
    ];
    events.send({ schema: 'conch.channel.v1', channel_id: 7, name: 'original', created_at: at });
    await screen.findByLabelText('Message #original');
    await screen.findByText(/^Not confirmed/);
    await act(async () => {
      answer(json({ message: message(100, 'original only', { author_id: 2 }) }, 201));
      await Promise.resolve();
    });
    expect(screen.getByText(/^Not confirmed/)).toBeDefined();
    expect(server.posts).toHaveLength(1);

    server.post = (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts.map((post) => post.channel)).toEqual(['general', 'original']);
  });

  it('drops reconciliation history after rename and old-name recreation, then retries under the current name', async () => {
    const events = await openWithEvents();
    server.post = () => Promise.reject(new TypeError('connection dropped'));
    type('reconcile by id');
    enter();
    await screen.findByText(/^Not confirmed/);
    const real = globalThis.fetch;
    let answerHistory: (response: Response) => void = () => undefined;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (url.pathname === '/v2/channels/general/messages' && url.searchParams.get('order') === 'asc') {
        return new Promise<Response>((resolve) => { answerHistory = resolve; });
      }
      return real(input, init);
    });
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sending');
    server.channels = [
      { id: 9, name: 'general', created_at: at },
      { id: 7, name: 'original', created_at: at },
      { id: 8, name: 'ops', created_at: at },
    ];
    server.history.original = [];
    events.send({ schema: 'conch.channel.v1', channel_id: 7, name: 'original', created_at: at });
    await screen.findByLabelText('Message #original');
    await act(async () => {
      answerHistory(json({ messages: [] }));
      await Promise.resolve();
    });
    await screen.findByText(/^Not confirmed/);
    expect(server.posts).toHaveLength(1);

    server.post = (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts.map((post) => post.channel)).toEqual(['general', 'original']);
  });

  it('restores a pending send by channel id after a rename and old-name recreation during signout', async () => {
    await connect(await openChat());
    server.post = () => Promise.reject(new TypeError('connection dropped'));
    type('for the original channel');
    enter();
    await screen.findByText(/^Not confirmed/);
    const real = globalThis.fetch;
    let expireHistory = false;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (expireHistory && url.pathname === '/v2/channels/general/messages' && url.searchParams.get('order') === 'asc') return Promise.resolve(json({}, 401));
      return real(input, init);
    });
    expireHistory = true;
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByLabelText('Credential');
    expireHistory = false;
    server.channels = [
      { id: 9, name: 'general', created_at: at },
      { id: 7, name: 'original', created_at: at },
      { id: 8, name: 'ops', created_at: at },
    ];
    server.history.original = [];
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByLabelText('Message #general');
    fireEvent.click(screen.getByRole('button', { name: 'original' }));
    await screen.findByText('for the original channel');
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await screen.findByLabelText('Message #general');
    expect(screen.queryByText('for the original channel')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'original' }));
    await screen.findByText('for the original channel');
    server.post = (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts).toEqual([
      { channel: 'general', body: 'for the original channel' },
      { channel: 'original', body: 'for the original channel' },
    ]);
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

describe('composing to a net or a whisper', () => {
  // The nets of general as the server lists them for ada (principal 2).
  const netsOf = (...list: { id: number; name: string; role?: string }[]) => ({
    nets: list.map((n) => ({
      id: n.id,
      channel_id: 7,
      name: n.name,
      members: [{ principal_id: 2, role: n.role ?? 'member' }],
      created_at: at,
    })),
  });
  const to = () => screen.getByLabelText<HTMLSelectElement>('To:');
  const optionsOf = () => Array.from(to().options).map((o) => o.textContent);
  const choose = (value: string) => {
    fireEvent.change(to(), { target: { value } });
  };
  const type = (text: string, label = 'Message #general') => {
    fireEvent.change(screen.getByLabelText(label), { target: { value: text } });
  };
  const send = () => {
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
  };
  const sendButton = () => screen.getByRole<HTMLButtonElement>('button', { name: 'Send' });
  async function ready() {
    server.nets = netsOf({ id: 5, name: 'alpha' }, { id: 6, name: 'watch', role: 'monitor' });
    await connect(await openChat());
    await screen.findByRole('option', { name: 'net alpha' });
  }

  it('lists the channel, the nets the viewer can transmit to, and Whisper to…', async () => {
    await ready();
    // A net the viewer only monitors is not offered, as in the TUI.
    expect(optionsOf()).toEqual(['#general', 'net alpha', 'Whisper to…']);
    expect(to().value).toBe('channel');
  });

  it('lists only the channel and the whisper for a channel without nets', async () => {
    server.nets = { nets: [] };
    await connect(await openChat());
    await waitFor(() => {
      expect(optionsOf()).toEqual(['#general', 'Whisper to…']);
    });
  });

  it('names the audience in the composer label', async () => {
    await ready();
    choose('net:5');
    expect(screen.getByLabelText('Message net alpha')).toBeDefined();
    choose('whisper');
    expect(screen.getByLabelText('Whisper to…')).toBeDefined();
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    fireEvent.click(screen.getByRole('checkbox', { name: '<b>mallory</b>' }));
    expect(screen.getByLabelText('Whisper to bot, <b>mallory</b>')).toBeDefined();
  });

  it('keeps Send disabled in a whisper until someone other than the viewer is chosen', async () => {
    await ready();
    choose('whisper');
    // The viewer is not on the list.
    expect(screen.queryByRole('checkbox', { name: 'ada' })).toBeNull();
    type('psst', 'Whisper to…');
    expect(sendButton().disabled).toBe(true);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Enter' });
    expect(server.posts).toHaveLength(0);
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    expect(sendButton().disabled).toBe(false);
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    expect(sendButton().disabled).toBe(true);
  });

  it('closes the picker on Escape and keeps the draft', async () => {
    await ready();
    choose('whisper');
    type('half a thought', 'Whisper to…');
    fireEvent.keyDown(screen.getByRole('checkbox', { name: 'bot [agent]' }), { key: 'Escape' });
    expect(screen.queryByRole('group', { name: 'Whisper to' })).toBeNull();
    expect(document.activeElement).toBe(to());
    // Escape in the textarea closes it as well.
    choose('whisper');
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Escape' });
    expect(screen.queryByRole('group', { name: 'Whisper to' })).toBeNull();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('half a thought');
    expect(to().value).toBe('channel');
  });

  // A reload keeps the tab's sessionStorage (issue #1039): a draft kept for a
  // few is never sent to everyone.
  async function reopen(label: string) {
    const socket = await openChat();
    socket.open();
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>(label).disabled).toBe(false);
    });
  }
  async function reload(label: string) {
    cleanup();
    FakeSocket.all = [];
    await reopen(label);
  }
  const keepDraft = (target: { net: number | null; whisper: number[] | null }) => {
    window.sessionStorage.setItem('conch.web.drafts', JSON.stringify({ principal: 2, drafts: { '7': 'psst' }, targets: { '7': target }, edits: {} }));
  };

  it('a whisper draft is still a whisper to the same people after a reload', async () => {
    await ready();
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    await reload('Whisper to bot');
    await waitFor(() => {
      expect(sendButton().disabled).toBe(false);
    });
    send();
    await waitFor(() => {
      expect(server.posts).toEqual([{ channel: 'general', body: 'psst', audience: { kind: 'principals', principal_ids: [3] } }]);
    });
  });

  it('a net draft is still for the same net after a reload', async () => {
    await ready();
    choose('net:5');
    type('on the net', 'Message net alpha');
    await reload('Message net alpha');
    await waitFor(() => {
      expect(sendButton().disabled).toBe(false);
    });
    send();
    await waitFor(() => {
      expect(server.posts).toEqual([{ channel: 'general', body: 'on the net', audience: { kind: 'net', net_id: 5 } }]);
    });
  });

  it('a whisper to someone who has left keeps the text, cannot be sent, and does not go to the channel', async () => {
    keepDraft({ net: null, whisper: [3, 99] });
    server.nets = netsOf({ id: 5, name: 'alpha' });
    await reopen('Whisper to…');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to…').value).toBe('psst');
    await screen.findByText(/no longer available/);
    expect(sendButton().disabled).toBe(true);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Enter' });
    expect(server.posts).toHaveLength(0);
    // Choosing who it goes to again makes it sendable.
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    expect(sendButton().disabled).toBe(false);
  });

  it('a net the person can no longer transmit to keeps the text, cannot be sent, and does not go to the channel', async () => {
    keepDraft({ net: 77, whisper: null });
    server.nets = netsOf({ id: 5, name: 'alpha' });
    await reopen('Whisper to…');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to…').value).toBe('psst');
    await screen.findByText(/no longer available/);
    expect(sendButton().disabled).toBe(true);
    expect(server.posts).toHaveLength(0);
  });

  // checked waits until no kept audience is still being checked.
  async function checked() {
    await waitFor(() => {
      expect(screen.queryByText(/Checking who this draft goes to/)).toBeNull();
    });
  }

  it('a whisper chosen over a net is still that whisper after a reload, never the net', async () => {
    await ready();
    choose('net:5');
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    cleanup();
    FakeSocket.all = [];
    (await openChat()).open();
    await checked();
    await waitFor(() => {
      expect(sendButton().disabled).toBe(false);
    });
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'general', body: 'psst', audience: { kind: 'principals', principal_ids: [3] } }]);
  });

  it('a choice made while a kept whisper is still being checked wins, and the check adds nobody to it', async () => {
    keepDraft({ net: null, whisper: [3] });
    server.nets = netsOf({ id: 5, name: 'alpha' });
    let release = () => {};
    server.netsHeld = new Promise<void>((resolve) => {
      release = resolve;
    });
    await reopen('Whisper to…');
    await screen.findByText(/Checking who this draft goes to/);
    fireEvent.click(screen.getByRole('checkbox', { name: '<b>mallory</b>' }));
    await act(async () => {
      release();
      await Promise.resolve();
    });
    await screen.findByRole('option', { name: 'net alpha' });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to <b>mallory</b>').value).toBe('psst');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'general', body: 'psst', audience: { kind: 'principals', principal_ids: [4] } }]);
  });

  // A sign-in that ends by itself keeps the text for the same person (issue
  // #714), and now who it was for (issue #1039). The credential stops working
  // when ops is opened; the person signs back in with `token`, who is ada
  // unless it is `other`, eve.
  const otherToken = 'conch_other_token_value';
  async function expireThenSignIn(token: string, meanwhile: () => void = () => {}) {
    const ada = token === secret;
    const real = globalThis.fetch;
    let expired = true;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      const headers = new Headers(init?.headers);
      if (headers.get('Authorization') === `Bearer ${otherToken}`) {
        if (url.pathname === '/v1/whoami') return Promise.resolve(json({ id: 9, kind: 'human', name: 'eve', role: 'member', voice_allowed: false }));
        headers.set('Authorization', `Bearer ${secret}`);
        return real(input, { ...init, headers });
      }
      if (expired && url.pathname === '/v2/channels/ops/messages') return Promise.resolve(json({}, 401));
      return real(input, init);
    });
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByText('Signed out: the credential is no longer valid');
    expired = false;
    meanwhile();
    await signBackIn(token);
    // The nets list ada alone, so eve is offered none.
    if (ada) await screen.findByRole('option', { name: 'net alpha' });
    else await screen.findByLabelText('Message #general');
    await checked();
  }

  // signBackIn signs in again from the sign-in screen and opens `channel`,
  // letting in every socket the page opens from here, once.
  async function signBackIn(token: string, channel = 'general') {
    FakeSocket.all = [];
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: token } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('log', { name: 'Messages' });
    fireEvent.click(await screen.findByRole('button', { name: channel }));
    await letIn();
  }

  // letIn opens every socket the page has opened and not yet been let in on,
  // until the composer is usable.
  const admitted = new WeakSet<FakeSocket>();
  async function letIn() {
    await waitFor(() => {
      for (const socket of FakeSocket.all) {
        if (admitted.has(socket) || socket.closed) continue;
        admitted.add(socket);
        socket.open();
      }
      expect(document.querySelector<HTMLTextAreaElement>('textarea')?.disabled).toBe(false);
    });
  }

  it('a whisper draft kept across a sign-in that ended goes to the same people', async () => {
    await ready();
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    await expireThenSignIn(secret);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to bot').value).toBe('psst');
    await waitFor(() => {
      expect(sendButton().disabled).toBe(false);
    });
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'general', body: 'psst', audience: { kind: 'principals', principal_ids: [3] } }]);
  });

  it('a net draft kept across a sign-in that ended goes to the same net', async () => {
    await ready();
    choose('net:5');
    type('on the net', 'Message net alpha');
    await expireThenSignIn(secret);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message net alpha').value).toBe('on the net');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'general', body: 'on the net', audience: { kind: 'net', net_id: 5 } }]);
  });

  it('a net lost while signed out keeps the text, cannot be sent, and does not go to the channel', async () => {
    await ready();
    choose('net:5');
    type('on the net', 'Message net alpha');
    await expireThenSignIn(secret, () => {
      server.nets = netsOf({ id: 5, name: 'alpha', role: 'monitor' }, { id: 8, name: 'alpha' });
    });
    await screen.findByText(/no longer available/);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to…').value).toBe('on the net');
    expect(sendButton().disabled).toBe(true);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Enter' });
    expect(server.posts).toHaveLength(0);
  });

  it('a whisper draft kept across a sign-in follows its channel through a rename, and the new channel of the old name gets nothing', async () => {
    await ready();
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    await expireThenSignIn(secret, () => {
      server.channels = [
        { id: 9, name: 'general', created_at: at },
        { id: 7, name: 'original', created_at: at },
        { id: 8, name: 'ops', created_at: at },
      ];
      server.history.original = [];
    });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    expect(to().value).toBe('channel');
    fireEvent.click(screen.getByRole('button', { name: 'original' }));
    await letIn();
    await screen.findByRole('option', { name: 'net alpha' });
    await checked();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to bot').value).toBe('psst');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'original', body: 'psst', audience: { kind: 'principals', principal_ids: [3] } }]);
  });

  it('a new channel given the old name while signed out never gets the text kept for the one that went', async () => {
    await ready();
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    await expireThenSignIn(secret, () => {
      server.channels = [
        { id: 9, name: 'general', created_at: at },
        { id: 8, name: 'ops', created_at: at },
      ];
    });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    expect(to().value).toBe('channel');
    expect(document.body.innerHTML).not.toContain('psst');
  });

  it('a whisper a 401 gives back keeps the audience it was sent to, not the one chosen since, and is held until checked', async () => {
    await ready();
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    let answer: (response: Response) => void = () => undefined;
    server.post = () =>
      new Promise<Response>((resolve) => {
        answer = resolve;
      });
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    // The person moves on to the whole channel before the answer comes.
    choose('channel');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    await act(async () => {
      answer(json({}, 401));
      await Promise.resolve();
    });
    await screen.findByText('Signed out: the credential is no longer valid');
    server.post = (_channel, body) => json({ message: message(nextId++, body, { author_id: 2 }) }, 201);
    let release = () => {};
    server.netsHeld = new Promise<void>((resolve) => {
      release = resolve;
    });
    await signBackIn(secret);
    await screen.findByText(/Checking who this draft goes to/);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to…').value).toBe('psst');
    expect(sendButton().disabled).toBe(true);
    await act(async () => {
      release();
      await Promise.resolve();
    });
    await checked();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to bot').value).toBe('psst');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(2);
    });
    expect(server.posts[1]).toEqual({ channel: 'general', body: 'psst', audience: { kind: 'principals', principal_ids: [3] } });
  });

  it('Escape does not end a hold while the kept audience is being checked', async () => {
    keepDraft({ net: 5, whisper: null });
    server.nets = netsOf({ id: 5, name: 'alpha' });
    let release = () => {};
    server.netsHeld = new Promise<void>((resolve) => {
      release = resolve;
    });
    await reopen('Whisper to…');
    await screen.findByText(/Checking who this draft goes to/);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Escape' });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to…').value).toBe('psst');
    expect(sendButton().disabled).toBe(true);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Enter' });
    expect(server.posts).toHaveLength(0);
    // The check still runs, and gives the net back.
    await act(async () => {
      release();
      await Promise.resolve();
    });
    await checked();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message net alpha').value).toBe('psst');
  });

  it('Escape does not end a hold once the kept audience is no longer available', async () => {
    keepDraft({ net: 77, whisper: null });
    server.nets = netsOf({ id: 5, name: 'alpha' });
    await reopen('Whisper to…');
    await screen.findByText(/no longer available/);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Escape' });
    fireEvent.keyDown(screen.getByRole('checkbox', { name: 'bot [agent]' }), { key: 'Escape' });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to…').value).toBe('psst');
    expect(sendButton().disabled).toBe(true);
    fireEvent.keyDown(screen.getByLabelText('Whisper to…'), { key: 'Enter' });
    expect(server.posts).toHaveLength(0);
    // Choosing the channel is a choice, and ends it.
    choose('channel');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('psst');
  });

  it('Escape on a whisper over a net, given back after a reload, returns to that net as it would have before', async () => {
    await ready();
    choose('net:5');
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    await reload('Whisper to bot');
    await checked();
    fireEvent.keyDown(screen.getByLabelText('Whisper to bot'), { key: 'Escape' });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message net alpha').value).toBe('psst');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'general', body: 'psst', audience: { kind: 'net', net_id: 5 } }]);
  });

  it('Escape on a whisper whose net under it is gone keeps the whisper, never the whole channel', async () => {
    keepDraft({ net: 77, whisper: [3] });
    server.nets = netsOf({ id: 5, name: 'alpha' });
    await reopen('Whisper to bot');
    await checked();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to bot').value).toBe('psst');
    fireEvent.keyDown(screen.getByLabelText('Whisper to bot'), { key: 'Escape' });
    fireEvent.keyDown(screen.getByRole('checkbox', { name: 'bot [agent]' }), { key: 'Escape' });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to bot').value).toBe('psst');
    expect(to().value).toBe('whisper');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(server.posts).toEqual([{ channel: 'general', body: 'psst', audience: { kind: 'principals', principal_ids: [3] } }]);
  });

  it('a whisper draft is not given, nor who it was for, to someone else signing in next', async () => {
    await ready();
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('psst', 'Whisper to bot');
    await expireThenSignIn(otherToken);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    expect(to().value).toBe('channel');
    expect(document.body.innerHTML).not.toContain('psst');
  });

  it('sends a whisper once, returns to the channel, and shows the audit notice only for the first', async () => {
    await ready();
    const notice = 'note: whispers are recorded in the audit log';
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: '<b>mallory</b>' }));
    type('first', 'Whisper to <b>mallory</b>');
    send();
    await screen.findByText(notice);
    expect(server.posts).toEqual([{ channel: 'general', body: 'first', audience: { kind: 'principals', principal_ids: [4] } }]);
    expect(to().value).toBe('channel');
    expect(screen.getByLabelText('Message #general')).toBeDefined();
    // A message now goes to the whole channel.
    type('hello all');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(2);
    });
    expect(server.posts[1]).toEqual({ channel: 'general', body: 'hello all' });
    expect(screen.queryByText(notice)).toBeNull();
    // A second whisper in the same tab does not repeat it.
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('second', 'Whisper to bot');
    send();
    await waitFor(() => {
      expect(server.posts).toHaveLength(3);
    });
    await waitFor(() => {
      expect(to().value).toBe('channel');
    });
    expect(screen.queryByText(notice)).toBeNull();
  });

  it('keeps a net after a post, and restores it with the draft after switching channels', async () => {
    await ready();
    choose('net:5');
    type('to alpha', 'Message net alpha');
    send();
    await waitFor(() => {
      expect(server.posts).toEqual([{ channel: 'general', body: 'to alpha', audience: { kind: 'net', net_id: 5 } }]);
    });
    await waitFor(() => {
      expect(screen.queryByText('Sending')).toBeNull();
    });
    expect(to().value).toBe('net:5');
    type('unsent', 'Message net alpha');
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Message #ops');
    expect(to().value).toBe('channel');
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    const box = await screen.findByLabelText<HTMLTextAreaElement>('Message net alpha');
    expect(box.value).toBe('unsent');
    expect(to().value).toBe('net:5');
  });

  it('shows the audience badge on the pending message, and keeps it on a retry', async () => {
    await ready();
    server.post = () => Promise.reject(new TypeError('connection dropped'));
    choose('net:5');
    type('badge me', 'Message net alpha');
    send();
    await screen.findByText(/^Not confirmed/);
    const line = () => within(screen.getByRole('log', { name: 'Messages' })).getAllByRole('listitem')[0]?.textContent;
    expect(line()).toContain('→ net alpha');
    server.post = (_c, body) => json({ message: message(nextId++, body, { author_id: 2, audience: { kind: 'net', net_id: 5 } }) }, 201);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts[1]).toEqual({ channel: 'general', body: 'badge me', audience: { kind: 'net', net_id: 5 } });
    expect(line()).toContain('→ net alpha');
  });

  it('badges a pending whisper with how many hear it', async () => {
    await ready();
    server.post = () => new Promise<Response>(() => undefined);
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    fireEvent.click(screen.getByRole('checkbox', { name: '<b>mallory</b>' }));
    type('quietly', 'Whisper to bot, <b>mallory</b>');
    send();
    await screen.findByText('Sending');
    expect(lines()[0]).toContain('→ whisper to 2');
  });

  it('shows the whole refusal line beside the composer, keeps the text as a Failed line, keeps the audience, and posts nothing more', async () => {
    await ready();
    server.post = () => json({ code: 'net_not_found', message: 'net alpha is not a net you can post to' }, 404);
    choose('net:5');
    type('not allowed', 'Message net alpha');
    send();
    expect((await screen.findByRole('alert')).textContent).toBe('net alpha is not a net you can post to');
    await new Promise((r) => setTimeout(r, 50));
    expect(server.posts).toHaveLength(1);
    expect(await screen.findByText('Failed')).toBeDefined();
    expect(lines()[0]).toContain('not allowed');
    expect(to().value).toBe('net:5');
    // Retry goes to the same net, and only on a click.
    server.post = (_c, body) => json({ message: message(nextId++, body, { author_id: 2, audience: { kind: 'net', net_id: 5 } }) }, 201);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts[1]).toEqual({ channel: 'general', body: 'not allowed', audience: { kind: 'net', net_id: 5 } });
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('keeps a whisper chosen after it is refused, and its text on a Failed line', async () => {
    await ready();
    server.post = () => json({ code: 'forbidden', message: 'whispers are not allowed here' }, 403);
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('nope', 'Whisper to bot');
    send();
    await screen.findByText('whispers are not allowed here');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to bot').value).toBe('');
    expect(lines()[0]).toContain('nope');
    expect(screen.queryByText('note: whispers are recorded in the audit log')).toBeNull();
  });

  it('does not let a confirmed whisper end a second one being composed', async () => {
    await ready();
    let release: (r: Response) => void = () => undefined;
    server.post = () => new Promise<Response>((resolve) => (release = resolve));
    choose('whisper');
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    type('first', 'Whisper to bot');
    send();
    await screen.findByText('Sending');
    // While the first is on its way: a different recipient and new text.
    fireEvent.click(screen.getByRole('checkbox', { name: 'bot [agent]' }));
    fireEvent.click(screen.getByRole('checkbox', { name: '<b>mallory</b>' }));
    type('second', 'Whisper to <b>mallory</b>');
    await act(async () => {
      release(json({ message: message(nextId++, 'first', { author_id: 2, audience: { kind: 'principals', principal_ids: [2, 3] } }) }, 201));
      await Promise.resolve();
    });
    await screen.findByText('Sent');
    expect(to().value).toBe('whisper');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Whisper to <b>mallory</b>').value).toBe('second');
  });

  it('does not widen a net post to the channel while the nets are not known', async () => {
    server.nets = netsOf({ id: 5, name: 'alpha' });
    await connect(await openChat());
    await screen.findByRole('option', { name: 'net alpha' });
    choose('net:5');
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Message #ops');
    server.nets = new Promise(() => undefined);
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await screen.findByLabelText('Message net #5');
    expect(to().value).toBe('net:5');
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

  // A reload keeps what the tab holds in sessionStorage (issue #1039).
  const reload = async () => {
    cleanup();
    FakeSocket.all = [];
    await connect(await openChat());
  };

  it('survive a reload, and a reload after sending shows an empty box', async () => {
    await connect(await openChat());
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'half a thought' } });
    await reload();
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('half a thought');
    });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => {
      expect(server.posts).toHaveLength(1);
    });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    await reload();
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    expect(window.sessionStorage.getItem('conch.web.drafts')).toBeNull();
  });

  it('are kept under the channel id, never the name, and hold no credential', async () => {
    await connect(await openChat());
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'secretive draft' } });
    const kept = window.sessionStorage.getItem('conch.web.drafts') ?? '';
    expect(JSON.parse(kept)).toEqual({ principal: 2, drafts: { '7': 'secretive draft' }, targets: {}, edits: {} });
    expect(kept).not.toContain(secret);
    expect(kept).not.toContain('general');
  });

  it('survive a reload while the server cannot be reached, and are there when it is back', async () => {
    window.sessionStorage.setItem('conch.web.drafts', JSON.stringify({ principal: 2, drafts: { '7': 'kept offline' }, targets: {}, edits: {} }));
    server.whoami = () => {
      throw new TypeError('Failed to fetch');
    };
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByText('Cannot reach the server');
    expect(JSON.parse(window.sessionStorage.getItem('conch.web.drafts') ?? '{}')).toMatchObject({ drafts: { '7': 'kept offline' } });
    cleanup();
    server.whoami = () => json(member);
    await connect(await openChat());
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('kept offline');
    });
  });

  it('are pruned of channels the person lost while the page was closed', async () => {
    window.sessionStorage.setItem(
      'conch.web.drafts',
      JSON.stringify({ principal: 2, drafts: { '7': 'mine', '99': 'lost channel' }, targets: { '99': { net: 5, whisper: null } }, edits: { '99': { message: 1, text: 'lost edit' } } }),
    );
    await connect(await openChat());
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('mine');
    });
    expect(JSON.parse(window.sessionStorage.getItem('conch.web.drafts') ?? '{}')).toEqual({ principal: 2, drafts: { '7': 'mine' }, targets: {}, edits: {} });
  });

  it('leave nothing readable in storage after sign-out', async () => {
    await connect(await openChat());
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'secretive draft' } });
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    await screen.findByLabelText('Credential');
    expect(JSON.stringify(Object.entries(window.sessionStorage))).not.toContain('secretive draft');
  });

  it('are not given to another person signing in on the tab', async () => {
    window.sessionStorage.setItem('conch.web.drafts', JSON.stringify({ principal: 9, drafts: { '7': 'someone else’s' }, edits: {} }));
    await connect(await openChat());
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    expect(window.sessionStorage.getItem('conch.web.drafts') ?? '').not.toContain('someone else');
  });

  it('work as before when storage is refused', async () => {
    await connect(await openChat());
    const refuse = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    try {
      fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'still here' } });
      fireEvent.click(screen.getByRole('button', { name: 'ops' }));
      await screen.findByLabelText('Message #ops');
      fireEvent.click(screen.getByRole('button', { name: 'general' }));
      expect((await screen.findByLabelText<HTMLTextAreaElement>('Message #general')).value).toBe('still here');
    } finally {
      refuse.mockRestore();
    }
  });
});

describe('reconnecting', () => {
  // described is the text of what an element's aria-describedby names.
  const described = (el: HTMLElement) =>
    (el.getAttribute('aria-describedby') ?? '')
      .split(' ')
      .filter((id) => id !== '')
      .map((id) => document.getElementById(id)?.textContent ?? '')
      .join(' ');

  it('says sending is paused, and describes the box and the audience with it, only while reconnecting', async () => {
    const first = await openChat();
    // The first load is not a pause.
    const box = screen.getByLabelText<HTMLTextAreaElement>('Message #general');
    const to = screen.getByLabelText<HTMLSelectElement>('To:');
    expect(screen.queryByText(pausedText)).toBeNull();
    expect(box.hasAttribute('aria-describedby')).toBe(false);
    await connect(first);
    expect(screen.queryByText(pausedText)).toBeNull();
    expect(box.hasAttribute('aria-describedby')).toBe(false);
    expect(box.parentElement?.classList.contains('paused')).toBe(false);
    fireEvent.change(box, { target: { value: 'kept through the gap' } });

    first.drop(1006);
    await screen.findByText(pausedText);
    expect(described(box)).toContain(pausedText);
    expect(described(to)).toContain(pausedText);
    expect(box.parentElement?.classList.contains('paused')).toBe(true);
    // The box stays closed as before, with the draft in it.
    expect(box.disabled).toBe(true);
    expect(to.disabled).toBe(true);
    expect(box.value).toBe('kept through the gap');

    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(2);
    }, { timeout: 3000 });
    (FakeSocket.all[1] as FakeSocket).open();
    await waitFor(() => {
      expect(screen.queryByText(pausedText)).toBeNull();
    });
    expect(box.hasAttribute('aria-describedby')).toBe(false);
    expect(box.parentElement?.classList.contains('paused')).toBe(false);
    expect(box.value).toBe('kept through the gap');
  });
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

describe('a long channel', () => {
  const long = () => Array.from({ length: 500 }, (_, i) => message(i + 1, `m${String(i + 1)}`));
  const olderButton = () => screen.queryByRole('button', { name: 'Load older messages' });

  it('opens with one history request for the newest 100, in order, and the socket resumes from the newest id', async () => {
    server.history.general = long();
    const socket = await openChat();
    await screen.findByText('m500');
    expect(server.historyRequests).toHaveLength(1);
    expect(server.historyRequests[0]?.search).toBe('?order=desc&limit=100');
    expect(lines().map((l) => l.match(/m\d+/)?.[0])).toEqual(Array.from({ length: 100 }, (_, i) => `m${String(401 + i)}`));
    expect(new URL(socket.url).searchParams.get('after')).toBe('500');
  });

  it('Load older asks below the smallest id held and draws the next 100 above, once each', async () => {
    server.history.general = long();
    await openChat();
    await screen.findByText('m500');
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('m301');
    expect(server.historyRequests).toHaveLength(2);
    expect(server.historyRequests[1]?.search).toBe('?order=desc&limit=100&before=401');
    expect(lines().map((l) => l.match(/m\d+/)?.[0])).toEqual(Array.from({ length: 200 }, (_, i) => `m${String(301 + i)}`));
  });

  it('shows a message that arrives on the socket and again in an older page once', async () => {
    server.history.general = long();
    const socket = await openChat();
    await connect(socket);
    await screen.findByText('m500');
    socket.send(message(400, 'm400'));
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('m301');
    expect(lines().filter((l) => l.includes('m400'))).toHaveLength(1);
    expect(lines()).toHaveLength(201);
  });

  it('disables the button while a page is on its way', async () => {
    server.history.general = long();
    await openChat();
    await screen.findByText('m500');
    const real = globalThis.fetch;
    let release: (() => void) | undefined;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      return gate.then(() => real(input, init));
    });
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await waitFor(() => {
      expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Load older messages' }).disabled).toBe(true);
    });
    release?.();
    await screen.findByText('m301');
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Load older messages' }).disabled).toBe(false);
  });

  it('hides the button at the end of history: a page shorter than 100', async () => {
    server.history.general = Array.from({ length: 150 }, (_, i) => message(i + 1, `m${String(i + 1)}`));
    await openChat();
    await screen.findByText('m150');
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('m1');
    expect(lines()).toHaveLength(150);
    expect(olderButton()).toBeNull();
  });

  it('hides the button at once when the newest page is short', async () => {
    server.history.general = [message(1, 'one')];
    await openChat();
    await screen.findByText('one');
    expect(olderButton()).toBeNull();
  });

  it('says so when an older page cannot be read, and tries again on a click', async () => {
    server.history.general = long();
    await openChat();
    await screen.findByText('m500');
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (path.includes('before=')) return Promise.resolve(json({}, 500));
      return real(input, init);
    });
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('Cannot load older messages');
    expect(lines()).toHaveLength(100);
    vi.stubGlobal('fetch', real);
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('m301');
    expect(screen.queryByText('Cannot load older messages')).toBeNull();
  });

  it('does not draw an older page that fails the schema', async () => {
    server.history.general = long();
    await openChat();
    await screen.findByText('m500');
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (path.includes('before=')) return Promise.resolve(json({ messages: [{ id: 'x' }] }));
      return real(input, init);
    });
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('Cannot load older messages');
    expect(lines()).toHaveLength(100);
  });

  it('signs out on a 401 while loading older messages', async () => {
    server.history.general = long();
    await openChat();
    await screen.findByText('m500');
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (path.includes('before=')) return Promise.resolve(json({}, 401));
      return real(input, init);
    });
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await waitFor(() => {
      expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
    });
    expect(screen.queryByRole('log', { name: 'Messages' })).toBeNull();
  });

  const grow = (n: number) => Array.from({ length: n }, (_, i) => message(i + 1, `m${String(i + 1)}`));

  // gateOlder holds back every request with before= until release is called.
  function gateOlder() {
    const real = globalThis.fetch;
    let release = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      return path.includes('before=') ? gate.then(() => real(input, init)) : real(input, init);
    });
    return release;
  }

  it('holds only the newest page when a channel is opened again after it grew', async () => {
    server.history.general = grow(500);
    await openChat();
    await screen.findByText('m500');
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByRole('heading', { name: 'ops' });
    server.history.general = grow(800);
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await screen.findByText('m800');
    expect(lines().map((l) => l.match(/m\d+/)?.[0])).toEqual(Array.from({ length: 100 }, (_, i) => `m${String(701 + i)}`));
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    await screen.findByText('m601');
    expect(server.historyRequests.at(-1)?.search).toBe('?order=desc&limit=100&before=701');
  });

  it('drops an older page that lands after the channel was left and opened again', async () => {
    server.history.general = grow(500);
    await openChat();
    await screen.findByText('m500');
    const release = gateOlder();
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByRole('heading', { name: 'ops' });
    server.history.general = grow(800);
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await screen.findByText('m800');
    release();
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
    });
    expect(lines()).toHaveLength(100);
    expect(screen.queryByText('m301')).toBeNull();
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Load older messages' }).disabled).toBe(false);
  });

  it('drops an older page that lands after a sign-out and a new sign-in', async () => {
    server.history.general = grow(500);
    await openChat();
    await screen.findByText('m500');
    const release = gateOlder();
    fireEvent.click(screen.getByRole('button', { name: 'Load older messages' }));
    // Signing out is in Settings (issue #536).
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    // Sign out asks first (issue #493).
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    fireEvent.change(await screen.findByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByText('m500');
    release();
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
    });
    expect(lines()).toHaveLength(100);
    expect(screen.queryByText('m301')).toBeNull();
  });

  // The exact rule (another page only while it fits within maxHeld) is in
  // state/messages.test.ts. This shows what the reader sees at the cap: the
  // channel starts nearly full, so it takes two pages and not nineteen (#459).
  it('stops offering older pages when another would not fit within maxHeld, and says so', async () => {
    server.history.general = Array.from({ length: maxHeld + 100 }, (_, i) => message(i + 1, `m${String(i + 1)}`));
    server.firstPage = maxHeld - 2 * pageSize;
    await openChat();
    await screen.findByText(`m${String(maxHeld + 100)}`);
    // Counted, not read: lines() copies every row to drop its buttons, and a
    // role query walks every message's Reply button, both slow at maxHeld rows
    // in jsdom (issue #842 added the buttons).
    const rows = () => screen.getByRole('log', { name: 'Messages' }).querySelectorAll('li[data-message-id]').length;
    for (const held of [maxHeld - pageSize, maxHeld]) {
      const older = screen.queryByText('Load older messages', { selector: 'button' });
      if (older === null) throw new Error('no Load older messages button');
      fireEvent.click(older);
      await waitFor(() => {
        expect(rows()).toBe(held);
      });
    }
    expect(olderButton()).toBeNull();
    expect(screen.getByText('older history is not loaded')).toBeDefined();
    expect(screen.getByText(`m${String(maxHeld + 100)}`)).toBeDefined();
    expect(server.historyRequests).toHaveLength(3);
    // About 3.7 s locally; the default 5 s is too close on a busy runner.
  }, 15000);
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

  // A stand-in ResizeObserver (jsdom has none) that keeps what it watches and
  // its callback, so a test can say when the log's height has changed.
  class FakeObserver {
    static all: FakeObserver[] = [];
    watched: Element[] = [];
    disconnected = false;
    constructor(readonly callback: ResizeObserverCallback) {
      FakeObserver.all.push(this);
    }
    observe(el: Element) {
      this.watched.push(el);
    }
    unobserve() {}
    disconnect() {
      this.disconnected = true;
    }
  }
  const watching = (log: HTMLElement) => FakeObserver.all.filter((o) => !o.disconnected && o.watched.includes(log));
  function resized(log: HTMLElement) {
    const observers = watching(log);
    expect(observers).toHaveLength(1);
    act(() => {
      for (const o of observers) o.callback([], o);
    });
  }
  function stubObserver() {
    FakeObserver.all = [];
    vi.stubGlobal('ResizeObserver', FakeObserver);
  }

  it('keeps a reader at the newest message there when the log gets shorter or taller', async () => {
    stubObserver();
    const socket = await openChat();
    await connect(socket);
    const log = screen.getByRole('log');
    const geo = { height: 1000, top: 700, client: 300 };
    geometry(log, geo);
    fireEvent.scroll(log);
    // Something below the log (the whisper picker) takes 100px of it.
    geo.client = 200;
    resized(log);
    expect(geo.top).toBe(1000);
    // And gives it back.
    fireEvent.scroll(log);
    geo.client = 300;
    resized(log);
    expect(geo.top).toBe(1000);
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
  });

  // What Chrome does (issue #585 against #589): the voice strip gets shorter,
  // the log taller, and the browser clamps the scroll; when the log gets
  // shorter again (the whisper picker), scroll anchoring puts the clamped
  // position back, 43px above the bottom, and says so with a scroll event
  // before the observer runs. That scroll is not the reader leaving the end.
  it('keeps a reader at the newest message there when the browser scrolls as the log changes height', async () => {
    stubObserver();
    const socket = await openChat();
    await connect(socket);
    const log = screen.getByRole('log');
    const geo = { height: 1014, top: 586, client: 428 };
    geometry(log, geo);
    resized(log);
    fireEvent.scroll(log);
    // Taller: clamped to the new end, before the observer has run.
    geo.client = 503;
    geo.top = 511;
    fireEvent.scroll(log);
    resized(log);
    geo.top = 511;
    // Shorter: the browser's anchoring moves the scroll back to 586.
    geo.client = 385;
    geo.top = 586;
    fireEvent.scroll(log);
    resized(log);
    expect(geo.top).toBe(1014);
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull();
    // The reader's own scrolls still count once the observer has the height.
    geo.top = 300;
    fireEvent.scroll(log);
    geo.client = 503;
    resized(log);
    expect(geo.top).toBe(300);
  });

  it('leaves a reader who has scrolled up where they are when the log changes height', async () => {
    stubObserver();
    server.history.general = [message(1, 'one')];
    const socket = await openChat();
    await connect(socket);
    const log = screen.getByRole('log');
    const geo = { height: 1000, top: 100, client: 300 };
    geometry(log, geo);
    fireEvent.scroll(log);
    geo.client = 200;
    resized(log);
    expect(geo.top).toBe(100);
    geo.client = 300;
    resized(log);
    expect(geo.top).toBe(100);
    // Jump to latest still comes and works as before.
    socket.send(message(2, 'two'));
    const jump = await screen.findByRole('button', { name: 'Jump to latest' });
    expect(geo.top).toBe(100);
    fireEvent.click(jump);
    expect(geo.top).toBe(1000);
  });

  it('stops watching the log when the conversation goes', async () => {
    stubObserver();
    await openChat();
    const log = screen.getByRole('log');
    expect(watching(log)).toHaveLength(1);
    cleanup();
    expect(FakeObserver.all.every((o) => o.disconnected)).toBe(true);
  });

  it('works without a ResizeObserver, as in jsdom', async () => {
    expect(typeof globalThis.ResizeObserver).toBe('undefined');
    const errors = vi.spyOn(console, 'error');
    const socket = await openChat();
    await connect(socket);
    const log = screen.getByRole('log');
    const geo = { height: 1000, top: 700, client: 300 };
    geometry(log, geo);
    fireEvent.scroll(log);
    geo.client = 200;
    geo.height = 1020;
    socket.send(message(1, 'one'));
    await screen.findByText('one');
    expect(geo.top).toBe(1020);
    expect(errors).not.toHaveBeenCalled();
    errors.mockRestore();
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
