// Two message frames handled in one tick both reach the notification hook
// (issue #1824), against a fake conchd (fetch) and a fake WebSocket. No
// network, no browser.
import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

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
  events: boolean;
  readDelay: Promise<void> | null;
  history: unknown[];
  // What GET /v1/read-state answers; null is the refusal below.
  read: () => unknown[];
  refusal: { status: number; code: string } | null;
  gets: number;
  // Set to make a mark request fail with no answer at all.
  putsDown: boolean;
  puts: { channel: string; id: number; url: string }[];
}

let server: Server;

function install() {
  server = { events: false, readDelay: null, history: [], read: () => [], refusal: null, gets: 0, putsDown: false, puts: [] };
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


beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  FakeSocket.all = [];
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('message frames and notifications', () => {
  it('gives the notification hook both of two frames handled before a render', async () => {
    server.events = true;
    window.localStorage.setItem(`conch.web.notifications.v1.${origin}.2`, JSON.stringify({ policy: 'all', muted: [], native: false }));
    await openChat();
    await waitFor(() => {
      expect(FakeSocket.all.some((socket) => socket.url.includes('/v1/events/ws?'))).toBe(true);
    });
    const events = FakeSocket.all.find((candidate) => candidate.url.includes('/v1/events/ws?'));
    if (events === undefined) throw new Error('no events socket');
    events.send({ schema: 'conch.session_roster.v1', channel_id: 7, sessions: [] });
    await act(async () => {
      await Promise.resolve();
    });
    act(() => {
      events.onmessage?.({ data: JSON.stringify(message(2)) });
      events.onmessage?.({ data: JSON.stringify(message(3)) });
    });
    expect(await screen.findByText('Notifications (2)')).toBeDefined();
  });
});
