// The community follows the server (issue #993, community-model §13.6):
// conch.directory_changed.v1 carries nothing, and the page reads the directory
// again and replaces what it holds, against a fake conchd (fetch) and a fake
// WebSocket. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { parseEventFrame } from './agents/frames';
import { directoryCoalesce } from './useDirectoryChanged';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:42:00Z';
const frame = { schema: 'conch.directory_changed.v1' };
const general = { id: 7, name: 'general', created_at: at };

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

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const person = (id: number, name: string) => ({ id, kind: 'human', name, role: 'member', created_at: at });
const member = (id: number, name: string) => ({ principal_id: id, created_at: at, name, kind: 'human' });

interface Server {
  directory: unknown[];
  directoryGets: number;
  members: unknown[];
}
let server: Server;

function install() {
  server = {
    directory: [person(2, 'ada'), person(3, 'bob'), person(4, 'cyd')],
    directoryGets: 0,
    members: [member(2, 'ada'), member(3, 'bob'), member(4, 'cyd')],
  };
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      const path = url.pathname;
      if (path === '/v1/whoami') return Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member', voice_allowed: false }));
      if (path === '/v1/channels') return Promise.resolve(json({ channels: [general] }));
      if (path === '/v1/directory/members') {
        server.directoryGets++;
        return Promise.resolve(json({ members: server.directory, scope: 'everyone' }));
      }
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (path === '/v1/sessions' && init?.method === 'POST') {
        return Promise.resolve(json({ schema: 'conch.session.v1', id: 100, principal_id: 2, kind: 'human', name: 'ada', client: 'browser', state: 'online', status: 'online', started_at: at, last_seen_at: at, interventions: [], outcome_unknown: false }, 201));
      }
      if (path === '/v1/read-state') return Promise.resolve(json({ read_state: [] }));
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages|read-state)$/.exec(path);
      if (m?.[2] === 'members') return Promise.resolve(json({ members: server.members }));
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') return Promise.resolve(json({ messages: [] }));
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
  await screen.findByRole('list', { name: 'People' });
  const events = eventsSockets()[0];
  if (events === undefined) throw new Error('no events socket');
  return events;
}

const names = () => within(screen.getByRole('list', { name: 'People' })).getAllByRole('listitem').map((li) => li.querySelector('.community-name')?.textContent);

// What typing "@" in the composer offers, the box left empty after.
function mentionOffers(): string[] {
  const box = screen.getByLabelText<HTMLTextAreaElement>('Message #general');
  fireEvent.change(box, { target: { value: '@', selectionStart: 1 } });
  const list = screen.queryByRole('listbox', { name: 'Mention suggestions' });
  const offered = list === null ? [] : within(list).getAllByRole('option').map((o) => o.textContent);
  fireEvent.change(box, { target: { value: '' } });
  return offered;
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

describe('the directory_changed frame', () => {
  it('is parsed on its schema alone, and a field it does not know is tolerated', () => {
    expect(parseEventFrame(JSON.stringify(frame))).toEqual({ kind: 'directoryChanged' });
    expect(parseEventFrame(JSON.stringify({ ...frame, later: 1 }))).toEqual({ kind: 'directoryChanged' });
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.directory_changed.v2' }))).toBeNull();
  });
});

describe('the community follows the server', () => {
  it('replaces the panel and the mention candidates on the frame: a rename shows, a hidden person goes', async () => {
    const events = await openChat();
    expect(names()).toEqual(['ada', 'bob', 'cyd']);
    expect(mentionOffers()).toEqual(['@bob', '@cyd']);
    const reads = server.directoryGets;
    server.directory = [person(2, 'ada'), person(3, 'robert')];
    server.members = [member(2, 'ada'), member(3, 'robert')];
    events.send(frame);
    await waitFor(() => {
      expect(names()).toEqual(['ada', 'robert']);
    });
    expect(server.directoryGets).toBe(reads + 1);
    expect(screen.queryByText('bob')).toBeNull();
    expect(screen.queryByText('cyd')).toBeNull();
    await waitFor(() => {
      expect(mentionOffers()).toEqual(['@robert']);
    });
  });

  it('reads once for frames that land close together', async () => {
    const events = await openChat();
    const reads = server.directoryGets;
    server.directory = [person(2, 'ada')];
    events.send(frame);
    events.send(frame);
    events.send(frame);
    await waitFor(() => {
      expect(names()).toEqual(['ada']);
    });
    await new Promise((resolve) => setTimeout(resolve, directoryCoalesce + 100));
    expect(server.directoryGets).toBe(reads + 1);
  });

  it('keeps the fallback: the directory is read again when the permissions change, with no frame', async () => {
    const events = await openChat();
    const reads = server.directoryGets;
    events.send({ schema: 'conch.permissions_changed.v1' });
    await waitFor(() => {
      expect(server.directoryGets).toBeGreaterThan(reads);
    });
  });

  it('no longer reads on a tab coming back, and has no Refresh button', async () => {
    await openChat();
    const reads = server.directoryGets;
    await act(async () => {
      document.dispatchEvent(new Event('visibilitychange'));
      await Promise.resolve();
    });
    expect(server.directoryGets).toBe(reads);
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
  });
});
