// The channel list follows membership changes live (issue #1027), against a
// fake conchd (fetch) and a fake WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { parseEventFrame } from './agents/frames';
import { emptyListInterval } from './useChannelRefresh';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:42:00Z';
const frame = { schema: 'conch.channels_changed.v1' };

const general = { id: 7, name: 'general', created_at: at };
const ops = { id: 8, name: 'ops', created_at: at };
const dev = { id: 9, name: 'dev', created_at: at };

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
const readState = (channel_id: number, unread: number) => ({ schema: 'conch.read_state.v1', channel_id, last_read_id: 0, unread });
const message = (id: number, body: string) => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: at, body });

interface Server {
  channels: unknown[];
  unread: Record<number, number>;
  listGets: number;
  // When set, a channel list answer waits for it.
  hold: Promise<void> | null;
  // When set, the next channel list answer alone waits for it.
  holdOnce: Promise<void> | null;
  // When set, a message history answer waits for it.
  holdHistory: Promise<void> | null;
}

let server: Server;

function install() {
  server = { channels: [general, ops], unread: {}, listGets: 0, hold: null, holdOnce: null, holdHistory: null };
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      const path = url.pathname;
      if (path === '/v1/whoami') return Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member', voice_allowed: false }));
      if (path === '/v1/channels') {
        server.listGets++;
        const answer = server.channels;
        const once = server.holdOnce;
        server.holdOnce = null;
        return (once ?? server.hold ?? Promise.resolve()).then(() => json({ channels: answer }));
      }
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (path === '/v1/sessions' && init?.method === 'POST') {
        return Promise.resolve(json({ schema: 'conch.session.v1', id: 100, principal_id: 2, kind: 'human', name: 'ada', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201));
      }
      if (path === '/v1/read-state') {
        return Promise.resolve(json({ read_state: Object.entries(server.unread).map(([id, n]) => readState(Number(id), n)) }));
      }
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages|read-state)$/.exec(path);
      if (m?.[2] === 'members') return Promise.resolve(json({ members: [{ principal_id: 2, created_at: at, name: 'ada', kind: 'human' }] }));
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') return (server.holdHistory ?? Promise.resolve()).then(() => json({ messages: [] }));
      return Promise.resolve(json({}, 404));
    }),
  );
}

const eventsSockets = () => FakeSocket.all.filter((s) => s.url.includes('/v1/events/ws?'));

async function openChat(): Promise<FakeSocket> {
  window.sessionStorage.setItem('conch.web.token', secret);
  window.location.hash = '#/c/general';
  render(<App origin={origin} />);
  await screen.findByRole('log', { name: 'Messages' });
  await waitFor(() => {
    expect(eventsSockets().length).toBeGreaterThan(0);
  });
  for (const socket of FakeSocket.all) socket.open();
  await waitFor(() => {
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
  });
  const events = eventsSockets()[0];
  if (events === undefined) throw new Error('no events socket');
  return events;
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  FakeSocket.all = [];
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the channels_changed frame', () => {
  it('is parsed, and a field it does not know is tolerated', () => {
    expect(parseEventFrame(JSON.stringify(frame))).toEqual({ kind: 'channelsChanged' });
    expect(parseEventFrame(JSON.stringify({ ...frame, later: 1 }))).toEqual({ kind: 'channelsChanged' });
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.channels_changed.v2' }))).toBeNull();
  });
});

describe('the list follows the server', () => {
  it('shows a channel added to the person, with its unread count, with no reload', async () => {
    const events = await openChat();
    expect(screen.queryByRole('button', { name: 'dev' })).toBeNull();
    server.channels = [general, ops, dev];
    server.unread = { 9: 4 };
    events.send(frame);
    await screen.findByRole('button', { name: 'dev' });
    await screen.findByRole('img', { name: '4 unread' });
  });

  it('replaces the counts after a refresh', async () => {
    server.unread = { 8: 3 };
    const events = await openChat();
    await screen.findByRole('img', { name: '3 unread' });
    server.unread = { 8: 1 };
    events.send(frame);
    await screen.findByRole('img', { name: '1 unread' });
    expect(screen.queryByRole('img', { name: '3 unread' })).toBeNull();
  });

  it('leaves a removed open channel with the line and none of its messages', async () => {
    const events = await openChat();
    const chat = FakeSocket.all.find((s) => s.url.includes('/v2/ws?'));
    chat?.send(message(1, 'a secret of general'));
    await screen.findByText('a secret of general');
    server.channels = [ops, dev];
    events.send(frame);
    await screen.findByText('You no longer have access to #general');
    expect(screen.queryByRole('button', { name: 'general' })).toBeNull();
    expect(screen.queryByText('a secret of general')).toBeNull();
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'ops' }).getAttribute('aria-current')).toBe('true');
    });
  });

  it('drops a channel that is not open, without the line', async () => {
    const events = await openChat();
    server.channels = [general];
    events.send(frame);
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'ops' })).toBeNull();
    });
    expect(screen.queryByText(/You no longer have access/)).toBeNull();
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-current')).toBe('true');
  });

  it('applies a rename the answer carries to the open channel', async () => {
    const events = await openChat();
    server.channels = [{ ...general, name: 'lobby' }, ops];
    events.send(frame);
    await screen.findByRole('button', { name: 'lobby' });
    expect(screen.queryByText(/You no longer have access/)).toBeNull();
  });

  it('reads the list again once after a reconnect', async () => {
    const events = await openChat();
    expect(server.listGets).toBe(1);
    events.drop(1006);
    await waitFor(() => {
      expect(eventsSockets()).toHaveLength(2);
    }, { timeout: 3000 });
    server.channels = [general, ops, dev];
    (eventsSockets()[1] as FakeSocket).open();
    await screen.findByRole('button', { name: 'dev' });
    expect(server.listGets).toBe(2);
  });

  it('turns frames that overlap a read into exactly one more read, never parallel', async () => {
    const events = await openChat();
    let release: () => void = () => undefined;
    server.hold = new Promise<void>((resolve) => {
      release = resolve;
    });
    const before = server.listGets;
    events.send(frame);
    events.send(frame);
    events.send(frame);
    await waitFor(() => {
      expect(server.listGets).toBe(before + 1);
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(server.listGets).toBe(before + 1);
    server.hold = null;
    server.channels = [general, ops, dev];
    release();
    await screen.findByRole('button', { name: 'dev' });
    await waitFor(() => {
      expect(server.listGets).toBe(before + 2);
    });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(server.listGets).toBe(before + 2);
  });

  it('reads the list again when the window regains focus and when the page becomes visible', async () => {
    await openChat();
    const before = server.listGets;
    server.channels = [general, ops, dev];
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    await screen.findByRole('button', { name: 'dev' });
    expect(server.listGets).toBe(before + 1);
    server.channels = [general, ops];
    act(() => {
      document.dispatchEvent(new Event('visibilitychange'));
    });
    await waitFor(() => {
      expect(screen.queryByRole('button', { name: 'dev' })).toBeNull();
    });
  });

  it('reads an empty list every 30 s, and opens the first channel it gets', async () => {
    server.channels = [];
    const timers: { run: () => void; ms: number }[] = [];
    const real = window.setInterval.bind(window);
    vi.spyOn(window, 'setInterval').mockImplementation(((run: () => void, ms?: number) => {
      if (ms === emptyListInterval) timers.push({ run, ms });
      return real(run, ms);
    }) as typeof window.setInterval);
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByText(/No channels yet/);
    expect(server.listGets).toBe(1);
    // Another part of the page may keep a 30 s timer of its own: all are run.
    expect(timers.length).toBeGreaterThan(0);
    server.channels = [general];
    await act(async () => {
      for (const timer of timers) timer.run();
      await Promise.resolve();
    });
    await screen.findByRole('button', { name: 'general' });
    expect(screen.getByRole('button', { name: 'general' }).getAttribute('aria-current')).toBe('true');
    expect(server.listGets).toBe(2);
  });
});

describe('a frame is not list information', () => {
  it('ignores a read-state frame for a channel not in the list', async () => {
    const events = await openChat();
    events.send(readState(99, 5));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryAllByRole('img')).toHaveLength(0);
  });
});

describe('a lost channel leaves nothing behind', () => {
  it('keeps no message, draft or audience of it, so a new channel of the same name starts empty', async () => {
    const events = await openChat();
    const chat = FakeSocket.all.find((s) => s.url.includes('/v2/ws?'));
    chat?.send(message(1, 'a secret of the old general'));
    await screen.findByText('a secret of the old general');
    fireEvent.change(screen.getByLabelText<HTMLTextAreaElement>('Message #general'), { target: { value: 'a draft for the old general' } });
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('a draft for the old general');
    server.channels = [ops];
    events.send(frame);
    await screen.findByText('You no longer have access to #general');
    // Another channel, new, is given the name, and the person is added to it.
    const reborn = { id: 10, name: 'general', created_at: at };
    server.channels = [ops, reborn];
    // Its history is slow: what the page shows meanwhile is what it held.
    server.holdHistory = new Promise<void>(() => undefined);
    const opsEvents = eventsSockets().at(-1);
    if (opsEvents === undefined) throw new Error('no events socket');
    opsEvents.open();
    opsEvents.send(frame);
    const button = await screen.findByRole('button', { name: 'general' });
    act(() => {
      button.click();
    });
    const box = await screen.findByLabelText<HTMLTextAreaElement>('Message #general');
    expect(screen.queryByText('a secret of the old general')).toBeNull();
    expect(box.value).toBe('');
  });
});

describe('a stale answer', () => {
  it('does not put back a channel the page lost while it was on its way', async () => {
    await openChat();
    let release: () => void = () => undefined;
    server.holdOnce = new Promise<void>((resolve) => {
      release = resolve;
    });
    const before = server.listGets;
    // A focus read begins while general is still the person's.
    act(() => {
      window.dispatchEvent(new Event('focus'));
    });
    await waitFor(() => {
      expect(server.listGets).toBe(before + 1);
    });
    // Then general is lost through the chat socket's 1008.
    server.channels = [ops];
    const chat = FakeSocket.all.find((s) => s.url.includes('/v2/ws?'));
    chat?.drop(1008);
    await screen.findByText('You no longer have access to #general');
    // The older answer, which still lists general, now arrives.
    await act(async () => {
      release();
      await Promise.resolve();
    });
    await act(async () => {
      await new Promise((r) => setTimeout(r, 100));
    });
    expect(screen.queryByRole('button', { name: 'general' })).toBeNull();
    expect(screen.getByRole('button', { name: 'ops' }).getAttribute('aria-current')).toBe('true');
    // The stale answer was not applied but read again: the focus read, the
    // 1008 path's own, and the one after the discard.
    expect(server.listGets).toBe(before + 3);
  });
});
