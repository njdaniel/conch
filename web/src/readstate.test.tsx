// Unread counts and the read mark (UX-02), against a fake conchd (fetch) and a
// fake WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { badgeText, markInterval } from './useReadState';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:42:00Z';

const channels = [
  { id: 7, name: 'general', created_at: at },
  { id: 8, name: 'ops', created_at: at },
  { id: 9, name: 'dev', created_at: at },
];

const message = (id: number, body = `m${String(id)}`, author_id = 3) => ({
  schema: 'conch.message.v2',
  id,
  channel_id: 7,
  author_id,
  created_at: at,
  body,
});

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
  drop(code: number) {
    act(() => {
      this.onclose?.({ code });
    });
  }
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const state = (channel_id: number, unread: number, last_read_id = 0) => ({
  schema: 'conch.read_state.v1',
  channel_id,
  last_read_id,
  unread,
  ...(last_read_id > 0 ? { updated_at: at } : {}),
});

interface Server {
  history: unknown[];
  // What GET /v1/read-state answers; null is the refusal below.
  read: () => unknown[];
  refusal: { status: number; code: string } | null;
  gets: number;
  puts: { channel: string; id: number; url: string }[];
}

let server: Server;

function install() {
  server = { history: [], read: () => [], refusal: null, gets: 0, puts: [] };
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      const path = url.pathname;
      if (path === '/v1/whoami') return Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member' }));
      if (path === '/v1/channels') return Promise.resolve(json({ channels }));
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (path === '/v1/read-state') {
        server.gets++;
        if (server.refusal !== null) return Promise.resolve(json({ code: server.refusal.code, message: 'no' }, server.refusal.status));
        return Promise.resolve(json({ read_state: server.read() }));
      }
      const put = /^\/v1\/channels\/([^/]+)\/read-state$/.exec(path);
      if (put?.[1] !== undefined && init?.method === 'PUT') {
        const channel = decodeURIComponent(put[1]);
        const id = (JSON.parse(init.body as string) as { last_read_id: number }).last_read_id;
        server.puts.push({ channel, id, url: url.href });
        if (server.refusal !== null) return Promise.resolve(json({ code: server.refusal.code, message: 'no' }, server.refusal.status));
        return Promise.resolve(json(state(channel === 'general' ? 7 : 8, 0, id)));
      }
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[2] === 'members') return Promise.resolve(json({ members: [{ principal_id: 2, created_at: at, name: 'ada', kind: 'human' }] }));
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') return Promise.resolve(json({ messages: m[1] === 'general' ? [...server.history].reverse() : [] }));
      return Promise.resolve(json({}, 404));
    }),
  );
}

async function openChat() {
  window.sessionStorage.setItem('conch.web.token', secret);
  window.location.hash = '#/c/general';
  render(<App origin={origin} />);
  await screen.findByRole('log', { name: 'Messages' });
  await waitFor(() => {
    expect(FakeSocket.all.length).toBeGreaterThan(0);
  });
  const socket = FakeSocket.all[FakeSocket.all.length - 1] as FakeSocket;
  socket.open();
  await waitFor(() => {
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
  });
  return socket;
}

// scrollTo puts the message log `fromBottom` pixels above its newest message.
function scrollTo(fromBottom: number) {
  const log = screen.getByRole('log');
  const geo = { height: 1000, client: 300, top: 700 - fromBottom };
  Object.defineProperty(log, 'scrollHeight', { configurable: true, get: () => geo.height });
  Object.defineProperty(log, 'clientHeight', { configurable: true, get: () => geo.client });
  Object.defineProperty(log, 'scrollTop', { configurable: true, get: () => geo.top, set: (v: number) => (geo.top = v) });
  fireEvent.scroll(log);
}

function setVisibility(value: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
  act(() => {
    document.dispatchEvent(new Event('visibilitychange'));
  });
}

const badge = (name: string) => screen.findByRole('img', { name });
const noBadges = () => screen.queryAllByRole('img');

beforeEach(() => {
  window.sessionStorage.clear();
  FakeSocket.all = [];
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  setVisibility('visible');
});

describe('badgeText', () => {
  it.each([
    { count: 0, want: null },
    { count: 1, want: '1' },
    { count: 999, want: '999' },
    { count: 1000, want: '999+' },
  ])('shows $count as $want', ({ count, want }) => {
    expect(badgeText(count)).toBe(want);
  });
});

describe('the badges', () => {
  it('shows a count with an accessible name beside the channel, and nothing at 0', async () => {
    server.read = () => [state(7, 0, 4), state(8, 3)];
    await openChat();
    const three = await badge('3 unread');
    expect(three.textContent).toBe('3');
    expect(noBadges()).toHaveLength(1);
    // The button keeps the channel's name and is described by its count.
    const ops = screen.getByRole('button', { name: 'ops' });
    expect(ops.getAttribute('aria-describedby')).toBe(three.id);
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-describedby')).toBeNull();
  });

  it('shows 999+ at the cap', async () => {
    server.read = () => [state(7, 0, 4), state(8, 1000)];
    await openChat();
    expect((await badge('999+ unread')).textContent).toBe('999+');
  });

  it('shows no count and no error when the server refuses read state for an agent or with auth off', async () => {
    for (const refusal of [
      { status: 403, code: 'human_required' },
      { status: 400, code: 'read_state_require_auth' },
    ]) {
      cleanup();
      FakeSocket.all = [];
      install();
      server.refusal = refusal;
      server.history = [message(1), message(2)];
      const socket = await openChat();
      await waitFor(() => {
        expect(server.gets).toBe(1);
      });
      socket.send(message(3));
      await screen.findByText('m3');
      expect(noBadges()).toHaveLength(0);
      expect(screen.queryByRole('alert')).toBeNull();
      // Nothing more is asked once it is refused.
      expect(server.gets).toBe(1);
      expect(server.puts.length).toBeLessThanOrEqual(1);
    }
  });

  it('keeps its counts and says nothing when a read fails', async () => {
    server.read = () => [state(7, 0, 4), state(8, 3)];
    const socket = await openChat();
    await badge('3 unread');
    server.refusal = { status: 500, code: 'internal_error' };
    socket.drop(1006);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(2);
    }, { timeout: 3000 });
    (FakeSocket.all[1] as FakeSocket).open();
    await waitFor(() => {
      expect(server.gets).toBe(2);
    });
    expect(screen.getByRole('img', { name: '3 unread' })).toBeDefined();
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('the mark', () => {
  it('moves to the newest message on open, once, and clears the count', async () => {
    server.history = [message(1), message(2)];
    server.read = () => [state(7, 2), state(8, 0)];
    await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    expect(server.puts[0]).toMatchObject({ channel: 'general', id: 2 });
    await waitFor(() => {
      expect(noBadges()).toHaveLength(0);
    });
  });

  it('never puts the token in a URL', async () => {
    server.history = [message(1)];
    await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    expect(server.puts[0]?.url).not.toContain(secret);
  });

  it('moves again for a message that arrives while the reader is at the bottom', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    scrollTo(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(markInterval);
    });
    socket.send(message(2));
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 2]);
    });
  });

  it('is sent at most once per interval per channel, the newest id winning', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    socket.send(message(2));
    socket.send(message(3));
    socket.send(message(4));
    await screen.findByText('m4');
    expect(server.puts).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(markInterval);
    });
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 4]);
    });
    // Nothing further is waiting.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(markInterval * 3);
    });
    expect(server.puts).toHaveLength(2);
  });

  it('does not move while the reader is scrolled up, counts what arrives, and moves when the newest is back in view', async () => {
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    scrollTo(400);
    socket.send(message(2));
    expect(await badge('1 unread')).toBeDefined();
    socket.send(message(3));
    expect(await badge('2 unread')).toBeDefined();
    expect(server.puts).toHaveLength(1);
    scrollTo(0);
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 3]);
    }, { timeout: 3000 });
    await waitFor(() => {
      expect(noBadges()).toHaveLength(0);
    });
  });

  it('does not move while the tab is hidden, and moves when it is seen again', async () => {
    setVisibility('hidden');
    server.history = [message(1), message(2)];
    const socket = await openChat();
    socket.send(message(3));
    await screen.findByText('m3');
    expect(server.puts).toHaveLength(0);
    setVisibility('visible');
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([3]);
    });
  });

  it('does not count your own message as unread', async () => {
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    scrollTo(400);
    socket.send(message(2, 'mine', 2));
    await screen.findByText('mine');
    expect(noBadges()).toHaveLength(0);
  });
});

describe('the counts are read again', () => {
  it('after a reconnect, and an answer older than our own mark does not bring a count back', async () => {
    server.history = [message(1), message(2)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    await waitFor(() => {
      expect(server.gets).toBe(1);
    });
    // The server has not yet counted our mark of 2 when this read is served.
    server.read = () => [state(7, 5, 1), state(8, 4)];
    socket.drop(1006);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(2);
    }, { timeout: 3000 });
    (FakeSocket.all[1] as FakeSocket).open();
    await waitFor(() => {
      expect(server.gets).toBe(2);
    });
    // ops is counted; general is not, whatever the stale answer says.
    expect(await badge('4 unread')).toBeDefined();
    expect(noBadges()).toHaveLength(1);
  });

  it('when another channel is opened', async () => {
    server.read = () => [state(7, 0, 4), state(8, 0, 4), state(9, 2)];
    await openChat();
    await badge('2 unread');
    expect(server.gets).toBe(1);
    server.read = () => [state(7, 0, 4), state(8, 0, 4), state(9, 6)];
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await waitFor(() => {
      expect(server.gets).toBe(2);
    });
    await badge('6 unread');
  });
});
