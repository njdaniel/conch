// Unread counts and the read mark (UX-02), against a fake conchd (fetch) and a
// fake WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { restore } from './api';
import { badgeText, markInterval, useReadState } from './useReadState';
import { Conversation } from './Conversation';
import { conversation, snapshot } from './dm/testkit';
import type { ReadStateV1 } from './schema/readstate';
import type { MessageV2 } from './schema/message';
import { chatFor, chatReducer, initialChat, maxHeld, pageSize } from './state/messages';

// A test that renders a whole held history, or is otherwise slow: about 6 s
// locally and three times that on CI runners (#1940).
const heldHistoryTimeout = 60_000;

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:42:00Z';
const originalScrollIntoView = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'scrollIntoView');

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
const state = (channel_id: number, unread: number, last_read_id = 0): ReadStateV1 => ({
  schema: 'conch.read_state.v1',
  channel_id,
  last_read_id,
  unread,
  ...(last_read_id > 0 ? { updated_at: at } : {}),
});

interface Server {
  dms?: boolean;
  events: boolean;
  readDelay: Promise<void> | null;
  history: unknown[];
  firstPage?: number;
  // What GET /v1/read-state answers; null is the refusal below.
  read: () => unknown[];
  refusal: { status: number; code: string } | null;
  gets: number;
  pages: number[];
  olderDown: boolean;
  // Set to make a mark request fail with no answer at all.
  putsDown: boolean;
  puts: { channel: string; id: number; url: string }[];
}

let server: Server;

function install() {
  server = { events: false, readDelay: null, history: [], read: () => [], refusal: null, gets: 0, pages: [], olderDown: false, putsDown: false, puts: [] };
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      const path = url.pathname;
      // Without voice, so the page opens no voice socket beside the
      // conversation's (issue #585): every socket counted here is its.
      if (path === '/v1/whoami') return Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member', voice_allowed: false }));
      if (path === '/v1/channels') return Promise.resolve(json({ channels }));
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (server.events && path === '/v1/sessions' && init?.method === 'POST') {
        return Promise.resolve(json({ schema: 'conch.session.v1', id: 100, principal_id: 2, kind: 'human', name: 'ada', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201));
      }
      if (server.dms) {
        if (path === '/v1/dms/snapshot') return Promise.resolve(json(snapshot()));
        if (path === '/v1/dms/12') return Promise.resolve(json(conversation()));
        if (path === '/v1/dms/12/messages') return Promise.resolve(json({ order: 'desc', messages: [] }));
        if (path === '/v1/directory/channels') return Promise.resolve(json({ channels: channels.map((channel) => ({
          ...channel, visibility: 'public', joined: true, default: false, manages: false,
        })) }));
      }
      if (path === '/v1/read-state') {
        server.gets++;
        if (server.refusal !== null) return Promise.resolve(json({ code: server.refusal.code, message: 'no' }, server.refusal.status));
        return (server.readDelay ?? Promise.resolve()).then(() => json({ read_state: server.read() }));
      }
      const put = /^\/v1\/channels\/([^/]+)\/read-state$/.exec(path);
      if (put?.[1] !== undefined && init?.method === 'PUT') {
        const channel = decodeURIComponent(put[1]);
        const id = (JSON.parse(init.body as string) as { last_read_id: number }).last_read_id;
        server.puts.push({ channel, id, url: url.href });
        if (server.putsDown) return Promise.reject(new TypeError('connection refused'));
        if (server.refusal !== null) return Promise.resolve(json({ code: server.refusal.code, message: 'no' }, server.refusal.status));
        return Promise.resolve(json(state(channel === 'general' ? 7 : 8, 0, id)));
      }
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[2] === 'members') return Promise.resolve(json({ members: [{ principal_id: 2, created_at: at, name: 'ada', kind: 'human' }] }));
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') {
        const before = Number(url.searchParams.get('before') ?? Infinity);
        server.pages.push(before);
        if (before !== Infinity && server.olderDown) return Promise.resolve(json({}, 500));
        const messages = m[1] === 'general' ? server.history.filter((m) => (m as { id: number }).id < before).slice(-(before === Infinity ? server.firstPage ?? pageSize : pageSize)).reverse() : [];
        return Promise.resolve(json({ messages }));
      }
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
    expect(FakeSocket.all.some((socket) => socket.url.includes('/v2/ws?'))).toBe(true);
    if (server.events) expect(FakeSocket.all.some((socket) => socket.url.includes('/v1/events/ws?'))).toBe(true);
  });
  for (const socket of FakeSocket.all) socket.open();
  const socket = FakeSocket.all.find((candidate) => candidate.url.includes('/v2/ws?'));
  if (socket === undefined) throw new Error('no message socket');
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
  window.localStorage.clear();
  FakeSocket.all = [];
  install();
});
afterEach(() => {
  cleanup();
  if (originalScrollIntoView === undefined) Reflect.deleteProperty(HTMLElement.prototype, 'scrollIntoView');
  else Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', originalScrollIntoView);
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

  it('keeps a hidden channel unread and reveals a channel opened from the directory or a message link', async () => {
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
    server.dms = true;
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => { expect(server.puts.map((mark) => mark.id)).toEqual([1]); });
    const inbox = await screen.findByRole('navigation', { name: 'Direct messages' });
    const openDm = () => fireEvent.click(within(inbox).getByRole('button', { name: /^Grace/ }));
    openDm();
    await screen.findByRole('textbox', { name: 'Message to Grace' });
    socket.send(message(2));
    expect(await badge('1 unread')).toBeDefined();
    expect(server.puts.map((mark) => mark.id)).toEqual([1]);

    fireEvent.click(screen.getByRole('button', { name: 'Browse channels' }));
    const directory = await screen.findByRole('region', { name: 'Channel directory' });
    fireEvent.click(await within(directory).findByRole('button', { name: 'ops' }));
    expect(await screen.findByLabelText('Message #ops')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();

    openDm();
    await screen.findByRole('textbox', { name: 'Message to Grace' });
    act(() => {
      window.history.replaceState(null, '', '#/c/general/m/1');
      window.dispatchEvent(new Event('hashchange'));
    });
    expect(await screen.findByLabelText('Message #general')).toBeDefined();
    expect(screen.queryByRole('button', { name: 'Back' })).toBeNull();
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

describe('live counts from the events socket', () => {
  async function liveChat() {
    server.events = true;
    await openChat();
    await waitFor(() => {
      expect(FakeSocket.all.some((socket) => socket.url.includes('/v1/events/ws?'))).toBe(true);
    });
    const socket = FakeSocket.all.find((candidate) => candidate.url.includes('/v1/events/ws?'));
    if (socket === undefined) throw new Error('no events socket');
    return socket;
  }

  it('updates a non-open channel without another read, and a shared mark clears it', async () => {
    const socket = await liveChat();
    expect(server.gets).toBe(1);
    socket.send(state(8, 1));
    expect(await badge('1 unread')).toBeDefined();
    socket.send(state(8, 2));
    expect(await badge('2 unread')).toBeDefined();
    expect(server.gets).toBe(1);
    socket.send(state(8, 0, 10));
    await waitFor(() => {
      expect(noBadges()).toHaveLength(0);
    });
    socket.send(state(8, 5, 9));
    await act(async () => {
      await Promise.resolve();
    });
    expect(noBadges()).toHaveLength(0);
  });

  it('ignores a channel the client may no longer see', async () => {
    const socket = await liveChat();
    socket.send(state(42, 3));
    await act(async () => {
      await Promise.resolve();
    });
    expect(noBadges()).toHaveLength(0);
  });

  it('applies an event only once across unrelated UI changes', async () => {
    server.history = [message(1)];
    const socket = await liveChat();
    const messages = FakeSocket.all.find((candidate) => candidate.url.includes('/v2/ws?'));
    if (messages === undefined) throw new Error('no message socket');
    scrollTo(400);
    socket.send(state(7, 1, 1));
    expect(await badge('1 unread')).toBeDefined();
    messages.send(message(2));
    expect(await badge('2 unread')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    expect(screen.getByRole('img', { name: '2 unread' })).toBeDefined();
  });

  it('does not replay a retained event after signing in again', async () => {
    const socket = await liveChat();
    socket.send(state(8, 0, 100));
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, sign out' }));
    await screen.findByLabelText('Credential');
    server.read = () => [state(8, 2)];
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByLabelText('Message #general');
    expect(await badge('2 unread')).toBeDefined();
  });

  it('does not add a message again after its absolute event count', async () => {
    server.history = [message(1)];
    const socket = await liveChat();
    const messages = FakeSocket.all.find((candidate) => candidate.url.includes('/v2/ws?'));
    if (messages === undefined) throw new Error('no message socket');
    scrollTo(400);
    server.read = () => [state(7, 1, 1)];
    socket.send({ schema: 'conch.session_roster.v1', channel_id: 7, sessions: [] });
    await waitFor(() => {
      expect(server.gets).toBe(2);
    });
    socket.send(state(7, 1, 1));
    expect(await badge('1 unread')).toBeDefined();
    messages.send(message(2));
    await screen.findByText('m2');
    expect(screen.getByRole('img', { name: '1 unread' })).toBeDefined();
  });

  it('does not resurrect a delayed message after a shared mark', async () => {
    server.history = [message(1)];
    const socket = await liveChat();
    const messages = FakeSocket.all.find((candidate) => candidate.url.includes('/v2/ws?'));
    if (messages === undefined) throw new Error('no message socket');
    scrollTo(400);
    socket.send(state(7, 0, 2));
    await waitFor(() => {
      expect(noBadges()).toHaveLength(0);
    });
    messages.send(message(2));
    await screen.findByText('m2');
    expect(noBadges()).toHaveLength(0);
  });

  it('reads again when only the events socket reconnects', async () => {
    const socket = await liveChat();
    server.read = () => [state(8, 2)];
    socket.drop(1006);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(3);
    }, { timeout: 3000 });
    const reconnected = FakeSocket.all[2] as FakeSocket;
    reconnected.open();
    reconnected.send({ schema: 'conch.session_roster.v1', channel_id: 7, sessions: [] });
    expect(await badge('2 unread')).toBeDefined();
    // The reconnect also reads the channel list again (issue #1027), which
    // replaces the counts once more: three reads, not two.
    await waitFor(() => {
      expect(server.gets).toBe(3);
    });
  });

  it('does not let an older reconnect response erase a newer event', async () => {
    const socket = await liveChat();
    let release = () => {};
    server.readDelay = new Promise<void>((resolve) => {
      release = resolve;
    });
    const messages = FakeSocket.all.find((candidate) => candidate.url.includes('/v2/ws?'));
    if (messages === undefined) throw new Error('no message socket');
    messages.drop(1006);
    await waitFor(() => {
      expect(FakeSocket.all).toHaveLength(3);
    }, { timeout: 3000 });
    (FakeSocket.all[2] as FakeSocket).open();
    await waitFor(() => {
      expect(server.gets).toBe(2);
    });
    socket.send(state(8, 2));
    expect(await badge('2 unread')).toBeDefined();
    release();
    await act(async () => {
      await server.readDelay;
    });
    expect(screen.getByRole('img', { name: '2 unread' })).toBeDefined();
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
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    server.read = () => [state(7, 0, 4), state(8, 0, 4), state(9, 2)];
    await openChat();
    await badge('2 unread');
    expect(server.gets).toBe(1);
    server.read = () => [state(7, 0, 4), state(8, 0, 4), state(9, 6)];
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    // Within the interval of the last read it waits for the interval to end.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(markInterval);
    });
    await waitFor(() => {
      expect(server.gets).toBe(2);
    });
    await badge('6 unread');
  });
});

describe('after review', () => {
  const fake = () => vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'Date'] });
  const advance = async (ms: number) => {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(ms);
    });
  };
  // reconnect lets the feed's retry timer run and opens the new socket.
  async function reconnect(count: number) {
    for (let i = 0; i < 10 && FakeSocket.all.length < count; i++) await advance(1000);
    expect(FakeSocket.all).toHaveLength(count);
    (FakeSocket.all[count - 1] as FakeSocket).open();
  }

  it('sends no mark while the socket is down, even when the interval ends, and one at the newest id after reconnect', async () => {
    fake();
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    socket.send(message(2));
    await screen.findByText('m2');
    socket.drop(1006);
    await screen.findByText('This conversation may be out of date');
    await advance(markInterval * 2);
    expect(server.puts).toHaveLength(1);
    await reconnect(2);
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 2]);
    });
    await advance(markInterval * 3);
    expect(server.puts).toHaveLength(2);
  });

  it('sends no mark for scrolling to the bottom while the socket is down, and one after reconnect', async () => {
    fake();
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    scrollTo(400);
    socket.send(message(2));
    await badge('1 unread');
    await advance(markInterval * 2);
    socket.drop(1006);
    await screen.findByText('This conversation may be out of date');
    scrollTo(0);
    await advance(markInterval);
    expect(server.puts).toHaveLength(1);
    await reconnect(2);
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 2]);
    });
    await advance(markInterval * 3);
    expect(server.puts).toHaveLength(2);
  });

  it('reads at most twice for many channels opened within the interval', async () => {
    fake();
    server.read = () => [state(7, 0, 4), state(8, 0, 4), state(9, 2)];
    await openChat();
    await badge('2 unread');
    expect(server.gets).toBe(1);
    for (const name of ['ops', 'general', 'ops', 'general', 'ops']) {
      fireEvent.click(screen.getByRole('button', { name }));
    }
    await advance(markInterval + 100);
    expect(server.gets).toBe(2);
  });

  it('moves the mark and clears the count when a channel left scrolled up is opened again', async () => {
    server.history = [message(1), message(2)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([2]);
    });
    scrollTo(400);
    socket.send(message(3));
    socket.send(message(4));
    socket.send(message(5));
    await screen.findByText('m5');
    const openLatest = async (count: number) => {
      await waitFor(() => {
        expect(FakeSocket.all).toHaveLength(count);
      });
      (FakeSocket.all[count - 1] as FakeSocket).open();
    };
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await openLatest(2);
    await screen.findByLabelText('Message #ops');
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await openLatest(3);
    await screen.findByLabelText('Message #general');
    await waitFor(() => {
      expect(server.puts.filter((p) => p.channel === 'general').map((p) => p.id)).toEqual([2, 5]);
    }, { timeout: 4000 });
    await waitFor(() => {
      expect(screen.queryByRole('img', { name: /unread/ })).toBeNull();
    });
  });

  it('takes back a mark that got no answer while the socket still reads open, and sends it again after reconnect (the rollback)', async () => {
    fake();
    server.history = [message(1)];
    const socket = await openChat();
    await waitFor(() => {
      expect(server.puts).toHaveLength(1);
    });
    await advance(markInterval);
    server.putsDown = true;
    socket.send(message(2));
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 2]);
    });
    // The server comes back, but the socket has not noticed yet.
    server.putsDown = false;
    await advance(markInterval * 2);
    socket.drop(1006);
    await screen.findByText('This conversation may be out of date');
    await reconnect(2);
    await waitFor(() => {
      expect(server.puts.map((p) => p.id)).toEqual([1, 2, 2]);
    });
    await advance(markInterval * 3);
    expect(server.puts).toHaveLength(3);
  });
});


describe('the opening unread position', () => {
  const divider = () => screen.queryByRole('separator', { name: 'New messages' });
  const nextRow = () => divider()?.parentElement?.nextElementSibling?.getAttribute('data-message-id');

  it.each([
    { mark: 0, target: '1' },
    { mark: 2, target: '3' },
    { mark: 4, target: undefined },
  ])('places the divider for mark $mark', async ({ mark, target }) => {
    server.history = [1, 2, 3, 4].map((id) => message(id));
    server.read = () => [state(7, 4 - mark, mark)];
    await openChat();
    await waitFor(() => { expect(server.puts.length).toBe(1); });
    expect(nextRow()).toBe(target);
    expect(divider()?.tabIndex).toBe(target === undefined ? undefined : -1);
  });

  it('does not mark own history or messages received while watching as unread', async () => {
    server.history = [message(1, 'my first post', 2), message(2, 'my latest post', 2)];
    server.read = () => [state(7, 0, 0)];
    const socket = await openChat();
    await waitFor(() => { expect(server.puts.at(-1)?.id).toBe(2); });
    socket.send(message(3, 'my live post', 2));
    socket.send(message(4, 'seen while watching'));
    await screen.findByText('seen while watching');
    expect(divider()).toBeNull();
    expect(screen.queryByRole('button', { name: 'Jump to first unread' })).toBeNull();
  });

  it.each([0, 10])('resolves a full first page beginning immediately after mark %i', async (mark) => {
    server.history = Array.from({ length: pageSize }, (_, i) => message(mark + i + 1));
    server.read = () => [state(7, pageSize, mark)];
    await openChat();
    await waitFor(() => { expect(nextRow()).toBe(String(mark + 1)); });
    expect(server.pages).toEqual([Infinity]);
  });

  // A whole held history: about 6 s here, three times that on CI (#1940).
  it.each([0, 10])('reaches the first unread exactly at maxHeld after mark %i', async (mark) => {
    server.history = Array.from({ length: maxHeld }, (_, i) => message(mark + i + 1));
    // Start one page short of the cap to exercise the final page transition.
    server.firstPage = maxHeld - pageSize;
    server.read = () => [state(7, 1000, mark)];
    await openChat();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: vi.fn() });
    fireEvent.click(screen.getByRole('button', { name: 'Jump to first unread' }));
    await waitFor(() => { expect(nextRow()).toBe(String(mark + 1)); }, { timeout: 10000 });
    await waitFor(() => { expect(document.activeElement?.getAttribute('data-message-id')).toBe(String(mark + 1)); });
    expect(server.pages).toEqual([Infinity, mark + pageSize + 1]);
    expect(screen.queryByText(/history limit has been reached/)).toBeNull();
  }, heldHistoryTimeout);

  it.each([{ length: 450, mark: 300 }, { length: 150, mark: 0 }])('stops when only own posts and tombstones remain after mark $mark, and dismisses the notice', async ({ length, mark }) => {
    server.history = Array.from({ length }, (_, i) => i % 2 === 0
      ? message(i + 1, 'mine', 2) : { ...message(i + 1), removed: true });
    server.read = () => [state(7, 1, mark)];
    await openChat();
    fireEvent.click(screen.getByRole('button', { name: 'Jump to first unread' }));
    await screen.findByText('The first unread message is no longer available.');
    expect(server.pages).toEqual([Infinity, length - pageSize + 1]);
    expect(screen.queryByRole('button', { name: 'Jump to first unread' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss unread notice' }));
    expect(screen.queryByText('The first unread message is no longer available.')).toBeNull();
  });

  it('allows dismissing a failed jump after manually loading the unread row', async () => {
    server.history = Array.from({ length: 150 }, (_, i) => message(i + 1));
    server.read = () => [state(7, 150, 0)];
    await openChat();
    server.olderDown = true;
    fireEvent.click(screen.getByRole('button', { name: 'Jump to first unread' }));
    await screen.findByText('Cannot load older messages. Try jumping again.');
    server.olderDown = false;
    fireEvent.click(screen.getByRole('button', { name: /Load older messages/ }));
    await waitFor(() => { expect(nextRow()).toBe('1'); });
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss unread notice' }));
    expect(screen.queryByText('Cannot load older messages. Try jumping again.')).toBeNull();
  });

  it('skips own posts and tombstones, and keeps a single divider after marking and new live messages', async () => {
    server.history = [message(1), message(2, 'mine', 2), { ...message(3, 'This message was removed by a moderator.'), removed: true }, message(4)];
    server.read = () => [state(7, 1, 1)];
    const socket = await openChat();
    await waitFor(() => { expect(nextRow()).toBe('4'); });
    await waitFor(() => { expect(server.puts.at(-1)?.id).toBe(4); });
    socket.send(message(5));
    expect(nextRow()).toBe('4');
    expect(screen.getAllByRole('separator', { name: 'New messages' })).toHaveLength(1);
    await waitFor(() => { expect(server.puts.at(-1)?.id).toBe(5); }, { timeout: 3000 });
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Message #ops');
    server.history.push(message(6));
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await screen.findByText('m6');
    await waitFor(() => { expect(nextRow()).toBe('6'); });
  });

  it.each([1, 2])('waits for opening mark %i and excludes live arrivals while the read is pending', async (mark) => {
    let release: () => void = () => {};
    server.readDelay = new Promise<void>((resolve) => { release = resolve; });
    server.history = [message(1), message(2)];
    server.read = () => [state(7, 2 - mark, mark)];
    const socket = await openChat();
    expect(server.puts).toHaveLength(0);
    socket.send(message(3));
    release();
    await waitFor(() => { expect(server.puts.at(-1)?.id).toBe(3); });
    expect(nextRow()).toBe(mark === 1 ? '2' : undefined);
  });

  it.each([0, 10])('loads across pages before placing and focusing the first unread after mark %i', async (mark) => {
    server.history = Array.from({ length: 320 }, (_, i) => message(i + 1));
    server.read = () => [state(7, 320 - mark, mark)];
    await openChat();
    expect(divider()).toBeNull();
    const scroll = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll });
    fireEvent.click(screen.getByRole('button', { name: 'Jump to first unread' }));
    await waitFor(() => { expect(nextRow()).toBe(String(mark + 1)); });
    await waitFor(() => { expect(document.activeElement?.getAttribute('data-message-id')).toBe(String(mark + 1)); });
    expect(scroll).toHaveBeenCalledWith({ block: 'center' });
  });

  it('says when the history limit prevents the jump, without focusing a substitute row', async () => {
    server.history = Array.from({ length: maxHeld + 100 }, (_, i) => message(i + 1));
    server.firstPage = maxHeld;
    server.read = () => [state(7, 1000, 1)];
    await openChat();
    const scroll = vi.fn();
    Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', { configurable: true, value: scroll });
    fireEvent.click(screen.getByRole('button', { name: 'Jump to first unread' }));
    await screen.findByText('Cannot reach the first unread message: the history limit has been reached.', {}, { timeout: 10000 });
    expect(divider()).toBeNull();
    expect(scroll).not.toHaveBeenCalled();
  }, heldHistoryTimeout);

  it('keeps the opening divider after a reconnect returns an advanced mark', async () => {
    server.history = [message(1), message(2), message(3)];
    server.read = () => [state(7, 2, 1)];
    const socket = await openChat();
    await waitFor(() => { expect(nextRow()).toBe('2'); });
    server.read = () => [state(7, 0, 3)];
    socket.drop(1006);
    await waitFor(() => { expect(FakeSocket.all).toHaveLength(2); }, { timeout: 3000 });
    (FakeSocket.all[1] as FakeSocket).open();
    await waitFor(() => { expect(server.gets).toBe(2); });
    expect(nextRow()).toBe('2');
  });

  it('does not introduce a divider for new live messages after opening a fully read channel', async () => {
    server.history = [message(1)];
    server.read = () => [state(7, 0, 1)];
    const socket = await openChat();
    socket.send(message(2));
    expect(divider()).toBeNull();
    expect(screen.queryByRole('button', { name: 'Jump to first unread' })).toBeNull();
  });
});

// These fixtures keep the owning hooks/reducer alive while the view remounts.
describe('read-state recovery and retained history', () => {
  beforeEach(async () => {
    window.sessionStorage.setItem('conch.web.token', secret);
    await restore();
  });
  const props = () => ({ signedIn: true, channels, loaded: true, open: 'general' as string | null,
    ready: true, newest: { id: 2, author: 3 }, atBottom: false, status: 'open' as const,
    eventsLive: true, event: null as ReturnType<typeof state> | null, me: 2, onUnauthorized: vi.fn() });

  it.each([500, 0])('marks and clears badges after an initial read failure %i', async (status) => {
    const fetch = vi.mocked(globalThis.fetch);
    fetch.mockImplementationOnce(() => status === 0 ? Promise.reject(new TypeError('offline')) : Promise.resolve(json({}, status)));
    const initial = props();
    const view = renderHook((p) => useReadState(p), { initialProps: initial });
    await act(async () => { await Promise.resolve(); });
    expect(view.result.current.openingMark).toBeNull();
    view.rerender({ ...initial, event: state(7, 2, 0) });
    await waitFor(() => { expect(view.result.current.counts.get('general')).toBe(2); });
    view.rerender({ ...initial, atBottom: true });
    await waitFor(() => { expect(server.puts.map((p) => p.id)).toEqual([2]); });
    expect(view.result.current.counts.get('general')).toBeUndefined();
  });

  it('can mark read after a failed snapshot without an event or reconnect', async () => {
    server.refusal = { status: 500, code: 'internal_error' };
    const initial = props();
    const view = renderHook((p) => useReadState(p), { initialProps: initial });
    await act(async () => { await Promise.resolve(); });
    server.refusal = null;
    view.rerender({ ...initial, atBottom: true });
    await waitFor(() => { expect(server.puts.map((p) => p.id)).toEqual([2]); });
    expect(view.result.current.openingMark).toBeNull();
  });

  it('reads and marks the first channel after starting with none', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true, toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    const initial = { ...props(), channels: [] as typeof channels, open: null as string | null, atBottom: true };
    const view = renderHook((p) => useReadState(p), { initialProps: initial });
    await waitFor(() => { expect(server.gets).toBe(1); });
    view.rerender({ ...initial, channels, open: 'general' });
    await act(async () => { await vi.advanceTimersByTimeAsync(markInterval); });
    await waitFor(() => { expect(server.gets).toBe(2); expect(server.puts.map((p) => p.id)).toEqual([2]); });
    expect(view.result.current.openingMark).toBe(0);
  });

  it.each([0, 1])('remounts ready history with opening mark %i and excludes later live arrivals', (mark) => {
    let store = chatReducer(initialChat, { type: 'historyLoaded', channel: 'general', messages: [message(1) as MessageV2], more: false });
    const conversation = () => <Conversation channel="general" chat={chatFor(store, 'general')} openingMark={mark}
      live="open" people={new Map()} nets={new Map()} viewer={2} draft="" target={{ net: null, whisper: null, refusal: null, note: null }}
      monitored={new Set()} onDraft={() => {}} onTarget={() => {}} onSend={() => {}} onReplySend={() => {}}
      onRetrySend={() => {}} onDismissSend={() => {}} onLoadOlder={() => {}} onReload={() => {}} onBottom={() => {}} />;
    const first = render(conversation());
    first.unmount();
    store = chatReducer(store, { type: 'received', channel: 'general', message: message(2) as MessageV2 });
    const view = render(conversation());
    const divider = () => screen.queryByRole('separator', { name: 'New messages' });
    expect(divider()?.parentElement?.nextElementSibling?.getAttribute('data-message-id')).toBe(mark === 0 ? '1' : undefined);
    store = chatReducer(store, { type: 'received', channel: 'general', message: message(3) as MessageV2 });
    view.rerender(conversation());
    expect(divider()?.parentElement?.nextElementSibling?.getAttribute('data-message-id')).toBe(mark === 0 ? '1' : undefined);
  });
});
