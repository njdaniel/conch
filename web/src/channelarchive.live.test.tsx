// Issue #844 in the whole page: archive and restore arrive on the events
// socket and move a channel in and out of the Archived group with no reload;
// an archived channel opens read-only; a 409 channel_archived from a post says
// the page's own line. A stand-in conchd and a fake WebSocket; no network.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const key = 'conch.web.token';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-11-04T00:00:00Z';
const stamp = '2026-11-04T12:00:00Z';
const leak = 'SERVER-WORDS-NOT-FOR-PEOPLE';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

type Channel = { id: number; name: string; created_at: string; visibility: 'public'; archived_at?: string };

const message = (id: number, body: string) => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: at, body });

class FakeSocket {
  static all: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number; reason: string }) => void) | null = null;
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
  send(doc: unknown) {
    act(() => {
      this.onmessage?.({ data: JSON.stringify(doc) });
    });
  }
  get path() {
    return new URL(this.url).pathname;
  }
}

let channels: Channel[];
let postAnswer: (() => Response) | null;
const sockets = (path: string) => FakeSocket.all.filter((s) => s.path === path);

function install() {
  FakeSocket.all = [];
  postAnswer = null;
  channels = [
    { id: 7, name: 'general', created_at: at, visibility: 'public' },
    { id: 8, name: 'ops', created_at: at, visibility: 'public' },
  ];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      const path = url.pathname;
      const method = init?.method ?? 'GET';
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      if (path === '/v1/whoami') return Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member' }));
      if (path === '/v1/channels') return Promise.resolve(json({ channels: channels.map((c) => ({ ...c })) }));
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (path === '/v1/read-state') return Promise.resolve(json({ read_state: [] }));
      if (path === '/v1/runs') return Promise.resolve(json({ runs: [] }));
      if (path === '/v1/sessions' && method === 'POST') {
        return Promise.resolve(
          json({ schema: 'conch.session.v1', id: 100, principal_id: 2, kind: 'human', name: 'ada', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201),
        );
      }
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages|read-state)$/.exec(path);
      if (m?.[1] !== undefined) {
        const channel = channels.find((c) => c.name === decodeURIComponent(m[1] ?? ''));
        if (channel === undefined) return Promise.resolve(json({ code: 'channel_not_found', message: 'channel not found' }, 404));
        switch (m[2]) {
          case 'members':
            return Promise.resolve(json({ members: [{ principal_id: 2, created_at: at, name: 'ada', kind: 'human' }] }));
          case 'nets':
            return Promise.resolve(json({ nets: [] }));
          case 'read-state':
            return Promise.resolve(json({ schema: 'conch.read_state.v1', channel_id: channel.id, last_read_id: 2, unread: 0, updated_at: at }));
          default:
            if (method === 'POST') return Promise.resolve(postAnswer === null ? json({ message: message(9, 'x') }, 201) : postAnswer());
            return Promise.resolve(json({ messages: channel.id === 7 ? [message(2, 'two'), message(1, 'one')] : [] }));
        }
      }
      return Promise.resolve(json({}, 404));
    }),
  );
}

const nav = () => screen.getByRole('navigation', { name: 'Channels' });

async function open(): Promise<FakeSocket> {
  window.location.hash = '#/c/general';
  render(<App origin={origin} />);
  await screen.findByRole('log', { name: 'Messages' });
  await waitFor(() => {
    expect(sockets('/v2/ws').length).toBe(1);
    expect(sockets('/v1/events/ws').length).toBe(1);
  });
  const events = sockets('/v1/events/ws')[0] as FakeSocket;
  (sockets('/v2/ws')[0] as FakeSocket).open();
  events.open();
  await act(() => new Promise((r) => setTimeout(r, 0)));
  return events;
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

describe('a channel archived and restored elsewhere', () => {
  it('moves into the Archived group on conch.channel.v1, goes read-only, keeps its history, and comes back on restore', async () => {
    const events = await open();
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
    });
    expect(within(nav()).queryByRole('button', { name: /^Archived/ })).toBeNull();

    events.send({ schema: 'conch.channel.v1', channel_id: 7, name: 'general', created_at: at, visibility: 'public', archived_at: stamp });
    const group = await within(nav()).findByRole('list', { name: 'Archived channels' });
    expect(within(group).getByRole('button', { name: 'general' }).getAttribute('aria-current')).toBe('true');
    expect(screen.getByText('This channel is archived.')).toBeTruthy();
    expect(screen.queryByLabelText('Message #general')).toBeNull();
    // History still reads.
    expect(screen.getByText('two')).toBeTruthy();
    expect(screen.getByText('one')).toBeTruthy();
    // Voice has nothing to join.
    expect(screen.queryByRole('button', { name: /Join voice/ })).toBeNull();

    events.send({ schema: 'conch.channel.v1', channel_id: 7, name: 'general', created_at: at, visibility: 'public' });
    await waitFor(() => {
      expect(screen.getByLabelText('Message #general')).toBeTruthy();
    });
    expect(within(nav()).queryByRole('list', { name: 'Archived channels' })).toBeNull();
    expect(screen.queryByText('This channel is archived.')).toBeNull();
  });
});

describe('a channel archived from the start', () => {
  it('opens read-only from the list the server gives, in a collapsed group when it is not the open one', async () => {
    (channels[1] as Channel).archived_at = stamp;
    await open();
    const toggle = within(nav()).getByRole('button', { name: 'Archived (1)' });
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    expect(within(nav()).queryByRole('button', { name: 'ops' })).toBeNull();
    expect(screen.getByLabelText('Message #general')).toBeTruthy();
    fireEvent.click(toggle);
    fireEvent.click(within(nav()).getByRole('button', { name: 'ops' }));
    expect(await screen.findByText('This channel is archived.')).toBeTruthy();
    expect(screen.queryByLabelText('Message #ops')).toBeNull();
  });
});

describe('a post the server refuses as archived', () => {
  it('says the page’s line, not the server’s, and the list is read again so the composer gives way', async () => {
    await open();
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
    });
    postAnswer = () => {
      // The channel was archived just before this post.
      (channels[0] as Channel).archived_at = stamp;
      return json({ code: 'channel_archived', message: leak }, 409);
    };
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => {
      expect(screen.getByText('This channel is archived.')).toBeTruthy();
    });
    expect(screen.queryByLabelText('Message #general')).toBeNull();
    expect(document.body.textContent).not.toContain(leak);
  });
});

describe('a post refused as archived by a server whose list still says live', () => {
  it('shows the page’s line beside the composer and the failed message, never the server’s words', async () => {
    await open();
    await waitFor(() => {
      expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false);
    });
    postAnswer = () => json({ code: 'channel_archived', message: leak }, 409);
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => {
      expect(screen.getByText('This channel is archived.')).toBeTruthy();
    });
    expect(document.body.textContent).not.toContain(leak);
    expect(screen.getByText('hello')).toBeTruthy();
  });
});
