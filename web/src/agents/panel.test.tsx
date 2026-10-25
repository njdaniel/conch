// The members panel, agent detail, run detail and interventions as the person
// sees them, against a fake conchd (fetch) and a fake WebSocket that sends the
// frames. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { BoundFunctions, queries } from '@testing-library/dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../App';
import type { SessionStatus } from '../schema/session';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticketValue = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-14T10:00:00.000Z';
const me = { id: 2, kind: 'human', name: 'ada', role: 'member' };

const members = {
  members: [
    { principal_id: 2, created_at: at, name: 'ada', kind: 'human' },
    { principal_id: 3, created_at: at, name: 'bot', kind: 'agent' },
    { principal_id: 4, created_at: at, name: '<b>mallory</b>', kind: 'human' },
    { principal_id: 5, created_at: at, name: 'ghost', kind: 'agent' },
  ],
};

type Json = Record<string, unknown>;

const session = (id: number, principal_id: number, over: Json = {}): Json => ({
  schema: 'conch.session.v1',
  id,
  principal_id,
  kind: 'human',
  name: principal_id === 2 ? 'ada' : 'bot',
  client: 'browser',
  state: 'online',
  status: 'online',
  started_at: at,
  last_seen_at: at,
  interventions: [],
  outcome_unknown: false,
  ...over,
});

const agentSession = (over: Json = {}): Json =>
  session(12, 3, {
    kind: 'agent',
    name: 'bot',
    client: 'adapter/1',
    status: 'running',
    runtime: 'claude-code',
    model: 'fable',
    interventions: ['message', 'cancel'],
    current_run: 41,
    task: 'Answer <i>the</i> question',
    ...over,
  });

const roster = (sessions: Json[], channel_id = 7): Json => ({ schema: 'conch.session_roster.v1', channel_id, sessions });

const run = (over: Json = {}): Json => ({
  schema: 'conch.run.v1',
  id: 41,
  session_id: 12,
  principal_id: 3,
  channel_id: 7,
  state: 'running',
  task: 'Answer <i>the</i> question',
  started_at: at,
  ...over,
});

const intervention = (id: number, state: string, over: Json = {}): Json => ({
  schema: 'conch.intervention.v1',
  id,
  run_id: 41,
  kind: 'message',
  body: 'hello agent',
  state,
  requested_by: 2,
  requested_at: at,
  ...(state === 'requested' ? {} : { answered_at: at }),
  ...over,
});

const activity = (id: number, over: Json = {}): Json => ({
  schema: 'conch.agent.activity.v1',
  id,
  run_id: 41,
  principal_id: 3,
  kind: 'command_finished',
  summary: `ran step ${String(id)}`,
  at,
  ...over,
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
  drop(code: number, reason = '') {
    act(() => {
      this.onclose?.({ code, reason });
    });
  }
  get query() {
    return new URL(this.url).searchParams;
  }
}

const events = () => FakeSocket.all.filter((s) => s.url.includes('/v1/events/ws'));
const lastEvents = () => events()[events().length - 1] as FakeSocket;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

interface Server {
  sessionCreates: Json[];
  sessionStatus: () => number;
  deletes: { id: number; keepalive: boolean | undefined }[];
  runList: Json[];
  runListUrls: string[];
  // The channel's runs (no session in the query), and the queries asked.
  channelRuns: Json[];
  channelRunUrls: string[];
  // The channel's messages, newest first, as the history read returns them.
  messages: Json[];
  // While holdBefore matches the `before` of a channel runs read ('' for the
  // first page), its answer waits in `held` until a test calls it.
  holdBefore: string | null;
  held: (() => void)[];
  runs: Record<number, Json>;
  activity: Json[];
  interventions: Json[];
  asks: { run: number; body: Json }[];
  askReply: () => Response;
  nextSession: number;
  // The members list the read answers, how many reads there were, and, while
  // holdMembers is set, the answers waiting until a test releases them.
  members: Json;
  membersReads: number;
  holdMembers: boolean;
  heldMembers: (() => void)[];
}

let server: Server;

// beforeOf keeps the runs below the query's `before`, as the server pages.
const beforeOf = (url: URL) => (r: Json) => {
  const before = Number(url.searchParams.get('before') ?? 0);
  return before === 0 || (r.id as number) < before;
};

function install() {
  server = {
    sessionCreates: [],
    sessionStatus: () => 201,
    deletes: [],
    runList: [],
    runListUrls: [],
    channelRuns: [],
    channelRunUrls: [],
    messages: [],
    holdBefore: null,
    held: [],
    runs: { 41: run() },
    activity: [],
    interventions: [],
    asks: [],
    askReply: () => json({}, 500),
    nextSession: 100,
    members,
    membersReads: 0,
    holdMembers: false,
    heldMembers: [],
  };
  FakeSocket.all = [];
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
      if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
      const path = url.pathname;
      const method = init?.method ?? 'GET';
      if (path === '/v1/whoami') return Promise.resolve(json(me));
      if (path === '/v1/channels') return Promise.resolve(json({ channels: [{ id: 7, name: 'general', created_at: at }, { id: 8, name: 'ops', created_at: at }] }));
      if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket: ticketValue, expires_at: at }, 201));
      if (path === '/v1/sessions' && method === 'POST') {
        server.sessionCreates.push(JSON.parse(init?.body as string) as Json);
        const status = server.sessionStatus();
        if (status !== 201) return Promise.resolve(json({ code: 'too_many_sessions', message: 'no' }, status));
        return Promise.resolve(json(session(server.nextSession++, 2), 201));
      }
      const del = /^\/v1\/sessions\/(\d+)$/.exec(path);
      if (del?.[1] !== undefined && method === 'DELETE') {
        server.deletes.push({ id: Number(del[1]), keepalive: init?.keepalive });
        return Promise.resolve(new Response(null, { status: 204 }));
      }
      if (path === '/v1/runs') {
        if (!url.searchParams.has('session')) {
          server.channelRunUrls.push(url.search);
          const answer = () => json({ runs: server.channelRuns.filter(beforeOf(url)).slice(0, Number(url.searchParams.get('limit'))) });
          // The first page has no `before`: holdBefore '' holds it.
          if (server.holdBefore !== null && (url.searchParams.get('before') ?? '') === server.holdBefore) {
            return new Promise<Response>((done) => {
              server.held.push(() => {
                done(answer());
              });
            });
          }
          return Promise.resolve(json({ runs: server.channelRuns.filter(beforeOf(url)).slice(0, Number(url.searchParams.get('limit'))) }));
        }
        server.runListUrls.push(url.search);
        return Promise.resolve(json({ runs: server.runList }));
      }
      const r = /^\/v1\/runs\/(\d+)(?:\/(activity|interventions))?$/.exec(path);
      if (r?.[1] !== undefined) {
        const id = Number(r[1]);
        if (r[2] === undefined) {
          const doc = server.runs[id];
          return Promise.resolve(doc === undefined ? json({ code: 'run_not_found', message: 'gone' }, 404) : json(doc));
        }
        if (r[2] === 'activity') return Promise.resolve(json({ activity: server.activity }));
        if (method === 'POST') {
          server.asks.push({ run: id, body: JSON.parse(init?.body as string) as Json });
          return Promise.resolve(server.askReply());
        }
        return Promise.resolve(json({ interventions: server.interventions }));
      }
      const m = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages)$/.exec(path);
      if (m?.[2] === 'members') {
        server.membersReads++;
        // The answer is the list as it stands when the read is made.
        const answer = json(server.members);
        if (!server.holdMembers) return Promise.resolve(answer);
        return new Promise<Response>((done) => {
          server.heldMembers.push(() => {
            done(answer);
          });
        });
      }
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') return Promise.resolve(json({ messages: server.messages }));
      return Promise.resolve(json({}, 404));
    }),
  );
}

async function openApp() {
  window.sessionStorage.setItem('conch.web.token', secret);
  window.location.hash = '';
  render(<App origin={origin} />);
  await waitFor(() => {
    expect(events().length).toBeGreaterThan(0);
  });
  const socket = lastEvents();
  socket.open();
  return socket;
}

const panel = () => within(screen.getByRole('region', { name: 'Members list' }));

async function showRoster(socket: FakeSocket, sessions: Json[]) {
  socket.send(roster(sessions));
  await panel().findByRole('heading', { name: 'People' });
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('the browser own session', () => {
  it('is created as client browser after sign-in and the events socket follows it with a ticket and the channel', async () => {
    const socket = await openApp();
    expect(server.sessionCreates).toEqual([{ client: 'browser' }]);
    expect(socket.query.get('channel')).toBe('general');
    expect(socket.query.get('session')).toBe('100');
    expect(socket.query.get('ticket')).toBe(ticketValue);
    expect(socket.url).toContain('/v1/events/ws?');
    expect(window.location.href).not.toContain(ticketValue);
  });

  it('is ended on sign-out, with keepalive, before the credential goes', async () => {
    await openApp();
    // Signing out is in Settings (issue #536).
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    // Sign out asks first (issue #493).
    fireEvent.click(await screen.findByRole('button', { name: 'Yes, sign out' }));
    await waitFor(() => {
      expect(server.deletes).toEqual([{ id: 100, keepalive: true }]);
    });
    expect(window.sessionStorage.getItem('conch.web.token')).toBeNull();
  });

  it('is ended on pagehide, with keepalive, once', async () => {
    await openApp();
    act(() => {
      window.dispatchEvent(new Event('pagehide'));
      window.dispatchEvent(new Event('pagehide'));
    });
    await waitFor(() => {
      expect(server.deletes).toEqual([{ id: 100, keepalive: true }]);
    });
  });

  it('opens the socket again for another channel with the same session', async () => {
    const first = await openApp();
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await waitFor(() => {
      expect(events().length).toBe(2);
    });
    expect(first.closed).toBe(true);
    expect(lastEvents().query.get('channel')).toBe('ops');
    expect(lastEvents().query.get('session')).toBe('100');
    expect(server.sessionCreates).toHaveLength(1);
  });

  it('leaves the panel unavailable with one line when no session can be made, and does not retry', async () => {
    server.sessionStatus = () => 429;
    window.sessionStorage.setItem('conch.web.token', secret);
    render(<App origin={origin} />);
    await screen.findByRole('region', { name: 'Members list' });
    await panel().findByText('Members unavailable');
    expect(panel().getByText('Could not start a session')).toBeDefined();
    expect(events()).toHaveLength(0);
    expect(server.sessionCreates).toHaveLength(1);
  });
});

describe('the panel', () => {
  it('says Connecting until the first roster, then lists People and Agents joined with the members', async () => {
    const socket = await openApp();
    await panel().findByText('Connecting…');
    await showRoster(socket, [session(100, 2), session(101, 2), agentSession()]);
    const online = panel().getByRole('list', { name: 'Online — 1' });
    const ada = within(online).getByText('ada').closest('li');
    expect(ada?.textContent).toContain('(2 sessions)');
    expect(ada?.textContent).toContain('online');
    // A member with no session reads offline, and its name is text, not markup.
    const offline = panel().getByRole('list', { name: 'Offline — 1' });
    const mallory = within(offline).getByText('<b>mallory</b>').closest('li');
    expect(mallory?.textContent).toContain('offline');
    expect(offline.querySelector('b')).toBeNull();
    const agents = panel().getByRole('list', { name: 'Agents' });
    const bot = within(agents).getByText('bot').closest('li');
    expect(bot?.textContent).toContain('Agent');
    expect(bot?.textContent).toContain('running');
    expect(bot?.textContent).toContain('task: Answer <i>the</i> question');
    expect(agents.querySelector('i')).toBeNull();
    // An agent with no session is offline.
    expect(within(agents).getByText('ghost').closest('li')?.textContent).toContain('offline');
  });

  it.each<[SessionStatus, string]>([
    ['online', 'online'],
    ['idle', 'online'],
    ['running', 'online'],
    ['blocked', 'online'],
    ['waiting', 'online'],
    ['failed', 'online'],
    ['stale', 'stale'],
    ['offline', 'offline'],
  ])('renders the agent status %s', async (status, state) => {
    const socket = await openApp();
    await showRoster(socket, [agentSession({ status, state, current_run: undefined, task: undefined })]);
    const row = within(panel().getByRole('list', { name: 'Agents' })).getByText('bot').closest('li');
    expect(within(row as HTMLElement).getAllByText(status).length).toBeGreaterThan(0);
  });

  it('says outcome unknown when the roster says so', async () => {
    const socket = await openApp();
    await showRoster(socket, [agentSession({ state: 'offline', status: 'offline', outcome_unknown: true })]);
    const row = within(panel().getByRole('list', { name: 'Agents' })).getByText('bot').closest('li');
    expect(row?.textContent).toContain('outcome unknown');
  });

  it('never shows a stale roster: Members unavailable when the socket drops, until a new roster', async () => {
    const socket = await openApp();
    await showRoster(socket, [session(100, 2), agentSession()]);
    socket.drop(1006);
    await panel().findByText('Members unavailable');
    expect(panel().queryByText('bot')).toBeNull();
    const next = await waitFor(
      () => {
        expect(events().length).toBeGreaterThan(1);
        return lastEvents();
      },
      { timeout: 3000 },
    );
    next.open();
    next.send(roster([session(100, 2)]));
    await panel().findByRole('heading', { name: 'People' });
    expect(panel().queryByText('Members unavailable')).toBeNull();
    // The new socket asks for a fresh ticket and the same session.
    expect(next.query.get('session')).toBe('100');
  });

  it('ignores a roster that is for another channel', async () => {
    const socket = await openApp();
    socket.send(roster([agentSession()], 8));
    await panel().findByText('Connecting…');
    expect(panel().queryByRole('heading', { name: 'People' })).toBeNull();
  });

  it('expands an agent to its sessions and their newest runs, read with one request', async () => {
    server.runList = [run({ id: 41 }), run({ id: 40, state: 'completed', ended_at: at, task: 'Earlier job' })];
    const socket = await openApp();
    await showRoster(socket, [agentSession({ outcome_unknown: false })]);
    fireEvent.click(panel().getByRole('button', { name: /Agent\s*bot/ }));
    const sessions = await panel().findByRole('list', { name: 'Sessions of bot' });
    expect(sessions.textContent).toContain('client: adapter/1');
    expect(sessions.textContent).toContain('claude-code/fable');
    expect(sessions.textContent).toContain('task: Answer <i>the</i> question');
    await within(sessions).findByRole('button', { name: 'Run #40 · completed' });
    expect(server.runListUrls).toHaveLength(1);
    const query = new URLSearchParams(server.runListUrls[0]);
    expect(Object.fromEntries(query)).toEqual({ channel: 'general', session: '12', order: 'desc', limit: '10' });
  });
});

// The member list as the events socket sends it when it changes (#515).
const memberList = (list: readonly Json[], channel_id = 7): Json => ({ schema: 'conch.channel_members.v1', channel_id, members: list });
const without = (id: number): Json[] => members.members.filter((m) => m.principal_id !== id);

// openSettled opens the app and shows a roster, and returns once the reads of
// the members list that opening makes are in.
async function openSettled(sessions: Json[] = [session(100, 2)]) {
  const socket = await openApp();
  await showRoster(socket, sessions);
  await waitFor(() => {
    expect(server.membersReads).toBeGreaterThanOrEqual(2);
  });
  await panel().findByText('<b>mallory</b>');
  await act(async () => {});
  return socket;
}

describe('the member list the socket sends', () => {
  it('applies a live grant and revocation to the badge, rename and member controls, and the standing line', async () => {
    const socket = await openSettled();
    const nav = () => within(screen.getByRole('navigation', { name: 'Channels' }));
    const granted = members.members.map((member) => (member.principal_id === 2 ? { ...member, manages: true, can_decide: false, role: 'member' } : member));

    socket.send(memberList(granted, 8));
    await act(async () => {});
    expect(nav().queryByText('manages')).toBeNull();
    expect(panel().queryByRole('form', { name: 'Add person by name' })).toBeNull();

    socket.send(memberList(granted));
    await panel().findByRole('form', { name: 'Add person by name' });
    expect(nav().getByText('manages')).toBeDefined();
    expect(nav().getByRole('button', { name: 'Rename #general' })).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect(screen.getByText('You manage #general.')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));

    socket.send(memberList(granted.map((member) => (member.principal_id === 2 ? { ...member, manages: false } : member))));
    await waitFor(() => {
      expect(panel().queryByRole('form', { name: 'Add person by name' })).toBeNull();
    });
    expect(nav().queryByText('manages')).toBeNull();
    expect(nav().queryByRole('button', { name: 'Rename #general' })).toBeNull();
    expect(panel().getByText('Only operators and this channel\'s managers add people.')).toBeDefined();
    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));
    expect(screen.getByText('You are a member.')).toBeDefined();
  });

  it('drops a member who left, without reading the list again', async () => {
    const socket = await openSettled();
    const reads = server.membersReads;
    socket.send(memberList(without(4)));
    await waitFor(() => {
      expect(panel().queryByText('<b>mallory</b>')).toBeNull();
    });
    const online = panel().getByRole('list', { name: 'Online — 1' });
    expect(within(online).getByText('ada')).toBeDefined();
    expect(panel().queryByRole('list', { name: /^Offline/ })).toBeNull();
    expect(within(panel().getByRole('list', { name: 'Agents' })).getByText('ghost')).toBeDefined();
    await act(async () => {});
    expect(server.membersReads).toBe(reads);
  });

  it.each<[string, Json[], string]>([
    // Hiding takes the account out of every channel (#445).
    ['a hidden member', without(4), '#4'],
    ['a renamed member', members.members.map((m) => (m.principal_id === 4 ? { ...m, name: 'mel' } : m)), 'mel'],
  ])('shows no old name of %s, in the panel or on their messages', async (_, list, author) => {
    server.messages = [{ schema: 'conch.message.v2', id: 9, channel_id: 7, author_id: 4, created_at: at, body: 'hello from four' }];
    const socket = await openSettled();
    const line = (await screen.findByText('hello from four')).closest('li') as HTMLElement;
    expect(within(line).getByText('<b>mallory</b>')).toBeDefined();
    socket.send(memberList(list));
    await waitFor(() => {
      expect(screen.queryByText('<b>mallory</b>')).toBeNull();
    });
    expect(within(line).getByText(author)).toBeDefined();
  });

  it('ignores a list that is for another channel', async () => {
    const socket = await openSettled();
    socket.send(memberList(without(4), 8));
    await act(async () => {});
    expect(panel().getByText('<b>mallory</b>')).toBeDefined();
  });

  it('ignores a list that does not validate', async () => {
    const socket = await openSettled();
    socket.send({ schema: 'conch.channel_members.v1', channel_id: 7, members: null });
    await act(async () => {});
    expect(panel().getByText('<b>mallory</b>')).toBeDefined();
  });

  it('keeps the list the socket sent over a read asked before it', async () => {
    const socket = await openSettled();
    // A roster naming someone the list lacks asks for the list again; that
    // read is answered only after the socket's newer list.
    server.holdMembers = true;
    const reads = server.membersReads;
    socket.send(roster([session(100, 2), session(110, 9)]));
    await waitFor(() => {
      expect(server.membersReads).toBe(reads + 1);
    });
    socket.send(memberList(without(4)));
    await waitFor(() => {
      expect(panel().queryByText('<b>mallory</b>')).toBeNull();
    });
    for (const release of server.heldMembers) release();
    // The held answer resolves, and its read settles, before the check.
    await act(async () => {});
    await act(async () => {});
    expect(panel().queryByText('<b>mallory</b>')).toBeNull();
  });
});

async function openRunDetail(socket: FakeSocket, sessionOver: Json = {}) {
  await showRoster(socket, [session(100, 2), agentSession(sessionOver)]);
  fireEvent.click(panel().getByRole('button', { name: /Agent\s*bot/ }));
  fireEvent.click(await panel().findByRole('button', { name: 'Open run #41' }));
  return screen.findByRole('article', { name: 'Run detail' });
}

describe('run detail', () => {
  it('shows the run, reads its activity and interventions, and takes more from frames', async () => {
    server.activity = [activity(1), activity(2)];
    server.interventions = [intervention(9, 'applied')];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    expect(detail.getByRole('heading', { name: 'Run #41' })).toBeDefined();
    expect(detail.getByTestId('run-state').textContent).toBe('running');
    expect(detail.getByText('Answer <i>the</i> question')).toBeDefined();
    await detail.findByText('ran step 2');
    expect(detail.getByTestId('intervention-9').textContent).toBe('↳ #9 applied');
    socket.send(activity(3));
    await detail.findByText('ran step 3');
    socket.send(activity(3, { summary: 'another run', run_id: 99 }));
    expect(detail.queryByText('another run')).toBeNull();
    expect(detail.getByRole('heading', { name: 'Activity (3) · ↳ interventions (1)' })).toBeDefined();
  });

  it('opens the same run from the list of recent runs', async () => {
    server.runList = [run({ id: 40, state: 'completed', ended_at: at, task: 'Earlier job' })];
    server.runs[40] = run({ id: 40, state: 'completed', ended_at: at, task: 'Earlier job' });
    const socket = await openApp();
    await showRoster(socket, [agentSession({ current_run: undefined, task: undefined })]);
    fireEvent.click(panel().getByRole('button', { name: /Agent\s*bot/ }));
    fireEvent.click(await panel().findByRole('button', { name: 'Run #40 · completed' }));
    const detail = within(await screen.findByRole('article', { name: 'Run detail' }));
    expect(detail.getByTestId('run-state').textContent).toBe('completed');
    expect(detail.getByText('The run has ended')).toBeDefined();
    expect(detail.queryByRole('button', { name: 'Cancel' })).toBeNull();
  });

  it('closes on Back and returns to the conversation', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    expect(screen.queryByRole('log', { name: 'Messages' })).toBeNull();
    fireEvent.click(detail.getByRole('button', { name: 'Back to conversation' }));
    expect(screen.queryByRole('article', { name: 'Run detail' })).toBeNull();
    expect(screen.getByRole('log', { name: 'Messages' })).toBeDefined();
  });

  it('says so and closes when the run is gone', async () => {
    delete server.runs[41];
    const socket = await openApp();
    await showRoster(socket, [agentSession()]);
    fireEvent.click(panel().getByRole('button', { name: /Agent\s*bot/ }));
    fireEvent.click(await panel().findByRole('button', { name: 'Open run #41' }));
    await panel().findByText('This run is gone, or you can no longer see it');
    expect(screen.queryByRole('article', { name: 'Run detail' })).toBeNull();
  });

  it('follows the run state from a RunV1 frame and ignores a frame for another run or channel', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    socket.send(run({ id: 99, state: 'failed', ended_at: at }));
    socket.send(run({ channel_id: 8, state: 'failed', ended_at: at }));
    expect(detail.getByTestId('run-state').textContent).toBe('running');
    socket.send(run({ state: 'blocked' }));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe('blocked');
    });
  });

  it('shows the same id, state and task as the API', async () => {
    server.runs[41] = run({ state: 'waiting', task: 'Same task' });
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe('waiting');
    });
    expect(detail.getByText('Same task')).toBeDefined();
    expect(detail.getByRole('heading', { name: 'Run #41' })).toBeDefined();
  });

  it('says outcome unknown beside the state of a run whose session is gone stale', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { state: 'stale', status: 'stale', outcome_unknown: true }));
    expect(detail.getByText(/outcome unknown/)).toBeDefined();
  });

  it('says live updates are paused while the socket is down and reads again when it is back', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    socket.drop(1006);
    await detail.findByText('Live updates paused: reconnecting…');
    server.runs[41] = run({ state: 'blocked' });
    const next = await waitFor(
      () => {
        expect(events().length).toBeGreaterThan(1);
        return lastEvents();
      },
      { timeout: 3000 },
    );
    next.open();
    next.send(roster([session(100, 2), agentSession()]));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe('blocked');
    });
    expect(detail.queryByText('Live updates paused: reconnecting…')).toBeNull();
  });
});

describe('a run link', () => {
  it('is a link only for https, opened in a new tab without an opener', async () => {
    const target = ['ht', 'tps://example.com/pull/262'].join('');
    server.runs[41] = run({ link: target });
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    const link = await detail.findByRole('link', { name: target });
    expect(link.getAttribute('href')).toBe(target);
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');
  });

  it.each([
    ['plain http', ['ht', 'tp://example.com/x'].join('')],
    ['a javascript URL', 'javascript:alert(1)'],
    ['text', 'in the other tab'],
  ])('is text for %s', async (_name, link) => {
    server.runs[41] = run({ link });
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await detail.findByText(link);
    expect(detail.queryByRole('link')).toBeNull();
  });
});

describe('interventions', () => {
  it('offers Message and Cancel when the session declares them, and says Pause and Resume are not supported when it does not', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    expect((detail.getByRole<HTMLButtonElement>('button', { name: 'Message' })).disabled).toBe(false);
    expect((detail.getByRole<HTMLButtonElement>('button', { name: 'Cancel' })).disabled).toBe(false);
    expect(detail.getByRole<HTMLButtonElement>('button', { name: 'Pause' }).disabled).toBe(true);
    expect(detail.getByRole<HTMLButtonElement>('button', { name: 'Resume' }).disabled).toBe(true);
    expect(detail.getAllByText('Not supported by this agent')).toHaveLength(2);
  });

  it('offers all four, with no note, when the session declares all four', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: ['message', 'cancel', 'pause', 'resume'] }));
    expect(detail.queryByText('Not supported by this agent')).toBeNull();
    for (const name of ['Message', 'Cancel', 'Pause']) {
      expect(detail.getByRole<HTMLButtonElement>('button', { name }).disabled).toBe(false);
    }
    // Resume waits for a run that reads waiting.
    expect(detail.getByRole<HTMLButtonElement>('button', { name: 'Resume' }).disabled).toBe(true);
  });

  it.each([
    ['message', 'Message', ['cancel', 'pause', 'resume']],
    ['cancel', 'Cancel', ['message', 'pause', 'resume']],
    ['pause', 'Pause', ['message', 'cancel', 'resume']],
    ['resume', 'Resume', ['message', 'cancel', 'pause']],
  ])('shows %s disabled, saying Not supported by this agent, when it is not declared', async (_kind, name, declared) => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: declared }));
    const button = detail.getByRole<HTMLButtonElement>('button', { name });
    expect(button.disabled).toBe(true);
    expect(detail.getAllByText('Not supported by this agent')).toHaveLength(1);
    const note = document.getElementById(button.getAttribute('aria-describedby') ?? '');
    expect(note?.textContent).toBe('Not supported by this agent');
  });

  it('shows all four unsupported for a session that declares nothing', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: [] }));
    expect(detail.getAllByText('Not supported by this agent')).toHaveLength(4);
    for (const name of ['Message', 'Cancel', 'Pause', 'Resume']) {
      expect(detail.getByRole<HTMLButtonElement>('button', { name }).disabled).toBe(true);
    }
  });

  // A declared kind is enabled only in the states it applies to, and then
  // without a note: Pause on running, Resume on waiting.
  it.each([
    ['running', true, false],
    ['waiting', false, true],
    ['blocked', false, false],
    ['starting', false, false],
  ])('in state %s enables Pause=%s and Resume=%s when both are declared', async (state, pause, resume) => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: ['message', 'cancel', 'pause', 'resume'] }));
    socket.send(run({ state }));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe(state);
    });
    expect(detail.getByRole<HTMLButtonElement>('button', { name: 'Pause' }).disabled).toBe(!pause);
    expect(detail.getByRole<HTMLButtonElement>('button', { name: 'Resume' }).disabled).toBe(!resume);
    expect(detail.queryByText('Not supported by this agent')).toBeNull();
  });

  it('pauses with no question, shows Requested, and reads waiting only from the run frame; then resumes the same way', async () => {
    // The reply is for the ask just recorded: #20 for the first, #21 for the second.
    server.askReply = () => json(intervention(19 + server.asks.length, 'requested', { kind: server.asks[server.asks.length - 1]?.body.kind, body: undefined }), 201);
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: ['message', 'cancel', 'pause', 'resume'] }));
    fireEvent.click(detail.getByRole('button', { name: 'Pause' }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-20').textContent).toBe('↳ #20 pause · requested…');
    });
    expect(server.asks).toEqual([{ run: 41, body: { kind: 'pause' } }]);
    expect(detail.queryByRole('alertdialog')).toBeNull();
    expect(detail.getByTestId('run-state').textContent).toBe('running');
    for (const state of ['acknowledged', 'applied']) {
      socket.send(intervention(20, state, { kind: 'pause', body: undefined }));
      await waitFor(() => {
        expect(detail.getByTestId('intervention-20').textContent).toBe(`↳ #20 ${state}`);
      });
      expect(detail.getByTestId('run-state').textContent).toBe('running');
    }
    socket.send(run({ state: 'waiting', outcome: 'paused' }));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe('waiting');
    });
    expect(detail.getByRole<HTMLButtonElement>('button', { name: 'Pause' }).disabled).toBe(true);

    fireEvent.click(detail.getByRole('button', { name: 'Resume' }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-21').textContent).toBe('↳ #21 resume · requested…');
    });
    expect(server.asks).toEqual([
      { run: 41, body: { kind: 'pause' } },
      { run: 41, body: { kind: 'resume' } },
    ]);
    socket.send(intervention(21, 'applied', { kind: 'resume', body: undefined }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-21').textContent).toBe('↳ #21 applied');
    });
    expect(detail.getByTestId('run-state').textContent).toBe('waiting');
    socket.send(run({ state: 'running' }));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe('running');
    });
  });

  it.each(['not paused', 'already paused'])('shows a failed answer %j as its reason, as text, and leaves the run as it was', async (reason) => {
    server.interventions = [intervention(9, 'requested', { kind: reason === 'not paused' ? 'resume' : 'pause', body: undefined })];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: ['pause', 'resume'] }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-9').textContent).toMatch(/requested…$/);
    });
    socket.send(intervention(9, 'failed', { kind: reason === 'not paused' ? 'resume' : 'pause', body: undefined, reason }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-9').textContent).toBe(`↳ #9 failed: ${reason}`);
    });
    expect(detail.getByTestId('run-state').textContent).toBe('running');
  });

  it('says the interventions are unavailable when the session is not in the roster', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    socket.send(roster([session(100, 2)]));
    await detail.findByText('Interventions unavailable: the session is not in the roster');
    expect(detail.queryByRole('button', { name: 'Cancel' })).toBeNull();
  });

  it('sends a message once, shows Requested, and moves through Acknowledged and Applied only from frames', async () => {
    server.askReply = () => json(intervention(7, 'requested'), 201);
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    fireEvent.click(detail.getByRole('button', { name: 'Message' }));
    fireEvent.change(detail.getByLabelText('Message to the agent'), { target: { value: 'hello agent' } });
    fireEvent.click(detail.getByRole('button', { name: 'Send' }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-7').textContent).toBe('↳ #7 message · requested… · hello agent');
    });
    expect(server.asks).toEqual([{ run: 41, body: { kind: 'message', body: 'hello agent' } }]);
    socket.send(intervention(7, 'acknowledged'));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-7').textContent).toBe('↳ #7 acknowledged');
    });
    socket.send(intervention(7, 'applied'));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-7').textContent).toBe('↳ #7 applied');
    });
    // A repeated, older document does not take it back.
    socket.send(intervention(7, 'requested'));
    expect(detail.getByTestId('intervention-7').textContent).toBe('↳ #7 applied');
    expect(server.asks).toHaveLength(1);
  });

  it('shows Failed with the reason from the frame, as text', async () => {
    server.interventions = [intervention(8, 'requested')];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-8').textContent).toBe('↳ #8 message · requested… · hello agent');
    });
    socket.send(intervention(8, 'failed', { reason: 'the agent is <b>busy</b>' }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-8').textContent).toBe('↳ #8 failed: the agent is <b>busy</b>');
    });
    expect(detail.getByTestId('intervention-8').querySelector('b')).toBeNull();
  });

  it('asks for confirmation before cancelling, and a keep leaves nothing sent', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    fireEvent.click(detail.getByRole('button', { name: 'Cancel' }));
    expect(detail.getByText('Cancel this run?')).toBeDefined();
    expect(server.asks).toHaveLength(0);
    fireEvent.click(detail.getByRole('button', { name: 'Keep running' }));
    expect(detail.queryByText('Cancel this run?')).toBeNull();
    expect(server.asks).toHaveLength(0);
  });

  it('never shows the run cancelled before a RunV1 frame reports it', async () => {
    server.askReply = () => json(intervention(5, 'requested', { kind: 'cancel', body: undefined }), 201);
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    fireEvent.click(detail.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(detail.getByRole('button', { name: 'Yes, cancel the run' }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-5').textContent).toBe('↳ #5 cancel · requested…');
    });
    expect(server.asks).toEqual([{ run: 41, body: { kind: 'cancel' } }]);
    expect(detail.getByTestId('run-state').textContent).toBe('running');
    socket.send(intervention(5, 'acknowledged', { kind: 'cancel', body: undefined }));
    socket.send(intervention(5, 'applied', { kind: 'cancel', body: undefined }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-5').textContent).toBe('↳ #5 applied');
    });
    expect(detail.getByTestId('run-state').textContent).toBe('running');
    socket.send(run({ state: 'cancelled', ended_at: at }));
    await waitFor(() => {
      expect(detail.getByTestId('run-state').textContent).toBe('cancelled');
    });
    expect(detail.getByText('The run has ended')).toBeDefined();
  });

  it('says why a refused ask was refused, and keeps the history as it was', async () => {
    server.askReply = () => json({ code: 'intervention_unsupported', message: 'no' }, 409);
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    fireEvent.click(detail.getByRole('button', { name: 'Message' }));
    fireEvent.change(detail.getByLabelText('Message to the agent'), { target: { value: 'hi' } });
    fireEvent.click(detail.getByRole('button', { name: 'Send' }));
    await detail.findByText('This session does not accept that request');
    expect(detail.getByRole('heading', { name: 'Activity (0)' })).toBeDefined();
  });

  it('does not send an empty message', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    fireEvent.click(detail.getByRole('button', { name: 'Message' }));
    fireEvent.click(detail.getByRole('button', { name: 'Send' }));
    expect(detail.getByText('Type a message to send')).toBeDefined();
    expect(server.asks).toHaveLength(0);
  });
});

describe('a session the server ended', () => {
  it('is replaced once, and the socket reopens with the new one', async () => {
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    socket.drop(1008, 'session ended');
    await waitFor(() => {
      expect(events()).toHaveLength(2);
    });
    expect(server.sessionCreates).toHaveLength(2);
    expect(lastEvents().query.get('session')).toBe('101');
    // The old session is already ended: it is not ended a second time.
    expect(server.deletes).toEqual([]);
    lastEvents().open();
    lastEvents().send(roster([session(101, 2)]));
    await panel().findByRole('heading', { name: 'People' });
  });

  it('is not replaced in a loop when the new one is ended before the socket was healthy', async () => {
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    socket.drop(1008, 'session ended');
    await waitFor(() => {
      expect(events()).toHaveLength(2);
    });
    lastEvents().open();
    lastEvents().drop(1008, 'session ended');
    await panel().findByText('Session ended; could not start a new one');
    expect(server.sessionCreates).toHaveLength(2);
    expect(panel().getByText('Members unavailable')).toBeDefined();
  });

  it('waits before replacing again when the server ends each new session after one roster', async () => {
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    socket.drop(1008, 'session ended');
    await waitFor(() => {
      expect(events()).toHaveLength(2);
    });
    lastEvents().open();
    lastEvents().send(roster([session(101, 2)]));
    await panel().findByRole('heading', { name: 'People' });
    lastEvents().drop(1008, 'session ended');
    // Not at network speed: the second replacement waits about a second.
    expect(server.sessionCreates).toHaveLength(2);
    await waitFor(
      () => {
        expect(server.sessionCreates).toHaveLength(3);
      },
      { timeout: 3000 },
    );
  });

  it('leaves the panel unavailable with one line when the new session cannot be made', async () => {
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    server.sessionStatus = () => 429;
    socket.drop(1008, 'session ended');
    await panel().findByText('Session ended; could not start a new one');
    expect(server.sessionCreates).toHaveLength(2);
    expect(events()).toHaveLength(1);
  });
});

// The Runs view and the richer timeline (issue #350).

const runsList = () => panel().findByRole('list', { name: 'Channel runs' });
const rowOf = (list: HTMLElement, label: string) => within(list).getByRole('button', { name: label }).closest('li') as HTMLElement;

async function openRuns(runs: Json[]) {
  server.channelRuns = runs;
  const socket = await openApp();
  await showRoster(socket, [session(100, 2), agentSession({ current_run: undefined, task: undefined })]);
  return socket;
}

describe('the Runs view', () => {
  it('lists the channel runs newest first with state, agent, task, start, end and outcome, from one request', async () => {
    await openRuns([
      run({ id: 40, state: 'completed', ended_at: '2026-10-14T10:00:12.000Z', outcome: 'all <b>done</b>', task: 'Earlier job' }),
      run({ id: 42, state: 'failed', ended_at: '2026-10-14T10:03:00.000Z', task: 'Third' }),
      run({ id: 41 }),
    ]);
    const list = await runsList();
    const rows = within(list).getAllByRole('listitem');
    expect(rows.map((r) => within(r).getByRole('button').textContent)).toEqual(['#42 · failed', '#41 · running', '#40 · completed']);
    expect(rowOf(list, '#40 · completed').textContent).toContain('bot');
    expect(rowOf(list, '#40 · completed').textContent).toContain('task: Earlier job');
    expect(rowOf(list, '#40 · completed').textContent).toContain('ended');
    expect(rowOf(list, '#40 · completed').textContent).toContain('all <b>done</b>');
    expect(list.querySelector('b')).toBeNull();
    expect(rowOf(list, '#41 · running').textContent).toContain('for ');
    expect(server.channelRunUrls).toHaveLength(1);
    const query = new URLSearchParams(server.channelRunUrls[0]);
    expect(query.get('channel')).toBe('general');
    expect(query.get('order')).toBe('desc');
    expect(query.get('limit')).toBe('50');
    expect(query.has('before')).toBe(false);
    expect(query.has('session')).toBe(false);
  });

  it('says so when the channel has no runs', async () => {
    await openRuns([]);
    await panel().findByText('No runs in this channel');
  });

  it('filters all, open and ended, with a named control', async () => {
    await openRuns([run({ id: 42, state: 'cancelled', ended_at: at }), run({ id: 41 }), run({ id: 40, state: 'blocked' })]);
    const list = await runsList();
    const filter = panel().getByRole('combobox', { name: 'Filter runs' });
    fireEvent.change(filter, { target: { value: 'open' } });
    expect(within(list).getAllByRole('button').map((b) => b.textContent)).toEqual(['#41 · running', '#40 · blocked']);
    fireEvent.change(filter, { target: { value: 'ended' } });
    expect(within(await runsList()).getAllByRole('button').map((b) => b.textContent)).toEqual(['#42 · cancelled']);
    fireEvent.change(filter, { target: { value: 'all' } });
    expect(within(await runsList()).getAllByRole('button')).toHaveLength(3);
  });

  it('says no runs of that kind when the filter keeps none', async () => {
    await openRuns([run({ id: 41 })]);
    await runsList();
    fireEvent.change(panel().getByRole('combobox', { name: 'Filter runs' }), { target: { value: 'ended' } });
    expect(panel().getByText('No ended runs here')).toBeDefined();
  });

  it('is kept current by run frames: a state change, a new run, and never a step back from ended', async () => {
    const socket = await openRuns([run({ id: 41 })]);
    await runsList();
    socket.send(run({ id: 41, state: 'blocked' }));
    await panel().findByRole('button', { name: '#41 · blocked' });
    socket.send(run({ id: 43, task: 'brand new' }));
    const list = await runsList();
    await waitFor(() => {
      expect(within(list).getAllByRole('button').map((b) => b.textContent)).toEqual(['#43 · running', '#41 · blocked']);
    });
    socket.send(run({ id: 41, state: 'completed', ended_at: at }));
    await panel().findByRole('button', { name: '#41 · completed' });
    socket.send(run({ id: 41, state: 'running' }));
    socket.send(run({ id: 44, channel_id: 8 }));
    socket.send(run({ id: 45 }));
    await panel().findByRole('button', { name: '#45 · running' });
    expect(within(list).queryByRole('button', { name: '#44 · running' })).toBeNull();
    expect(within(list).getByRole('button', { name: '#41 · completed' })).toBeDefined();
  });

  it('pages: a full page offers older runs, read below the smallest id, until a short page', async () => {
    const all = Array.from({ length: 60 }, (_, i) => run({ id: 100 - i }));
    await openRuns(all);
    const list = await runsList();
    expect(within(list).getAllByRole('button')).toHaveLength(50);
    fireEvent.click(panel().getByRole('button', { name: 'Show older runs' }));
    await waitFor(() => {
      expect(within(list).getAllByRole('button')).toHaveLength(60);
    });
    expect(new URLSearchParams(server.channelRunUrls[1]).get('before')).toBe('51');
    expect(panel().queryByRole('button', { name: 'Show older runs' })).toBeNull();
    // Newest first still.
    expect(within(list).getAllByRole('button')[59]?.textContent).toBe('#41 · running');
  });

  it('does not add a frame for a run older than the pages read, since paging will reach it', async () => {
    const socket = await openRuns(Array.from({ length: 50 }, (_, i) => run({ id: 100 - i })));
    const list = await runsList();
    socket.send(run({ id: 10 }));
    socket.send(run({ id: 101 }));
    await panel().findByRole('button', { name: '#101 · running' });
    expect(within(list).queryByRole('button', { name: '#10 · running' })).toBeNull();
  });

  it('drops a page read for a channel that was left and came back to, and one for a channel no longer on screen', async () => {
    server.holdBefore = '51';
    const socket = await openRuns(Array.from({ length: 60 }, (_, i) => run({ id: 100 - i })));
    const list = await runsList();
    expect(within(list).getAllByRole('button')).toHaveLength(50);
    fireEvent.click(panel().getByRole('button', { name: 'Show older runs' }));
    await waitFor(() => {
      expect(server.held).toHaveLength(1);
    });
    // Away to ops: the page for general lands on ops, which has its own first page.
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await waitFor(() => {
      expect(events()).toHaveLength(2);
    });
    expect(socket.closed).toBe(true);
    lastEvents().open();
    lastEvents().send(roster([session(100, 2)], 8));
    await waitFor(() => {
      expect(within(panel().getByRole('list', { name: 'Channel runs' })).getAllByRole('button')).toHaveLength(50);
    });
    server.held[0]?.();
    await act(() => Promise.resolve());
    expect(within(panel().getByRole('list', { name: 'Channel runs' })).getAllByRole('button')).toHaveLength(50);
    expect(panel().getByRole('button', { name: 'Show older runs' })).toBeDefined();

    // Away to general and back to ops, with a page of the first stay still to land.
    server.held = [];
    fireEvent.click(panel().getByRole('button', { name: 'Show older runs' }));
    await waitFor(() => {
      expect(server.held).toHaveLength(1);
    });
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await waitFor(() => {
      expect(events()).toHaveLength(3);
    });
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await waitFor(() => {
      expect(events()).toHaveLength(4);
    });
    lastEvents().open();
    lastEvents().send(roster([session(100, 2)], 8));
    await waitFor(() => {
      expect(within(panel().getByRole('list', { name: 'Channel runs' })).getAllByRole('button')).toHaveLength(50);
    });
    server.held[0]?.();
    await act(() => Promise.resolve());
    expect(within(panel().getByRole('list', { name: 'Channel runs' })).getAllByRole('button')).toHaveLength(50);
    expect(panel().getByRole('button', { name: 'Show older runs' })).toBeDefined();
  });

  it('opens the run in the centre when selected', async () => {
    server.runs[42] = run({ id: 42, state: 'failed', ended_at: at, task: 'Third' });
    await openRuns([run({ id: 42, state: 'failed', ended_at: at, task: 'Third' })]);
    fireEvent.click(await panel().findByRole('button', { name: '#42 · failed' }));
    const detail = within(await screen.findByRole('article', { name: 'Run detail' }));
    expect(detail.getByRole('heading', { name: 'Run #42' })).toBeDefined();
    expect(detail.getByTestId('run-state').textContent).toBe('failed');
  });

  it('says it could not list runs, and tries again', async () => {
    const real = globalThis.fetch;
    let fail = true;
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, origin);
        if (fail && url.pathname === '/v1/runs') return Promise.resolve(json({}, 500));
        return real(input, init);
      }),
    );
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    await panel().findByText('Could not list runs');
    server.channelRuns = [run({ id: 41 })];
    fail = false;
    fireEvent.click(panel().getByRole('button', { name: 'Try again' }));
    await panel().findByRole('button', { name: '#41 · running' });
  });

  it('shows the duration of an open run moving, and of an ended one fixed', async () => {
    vi.useFakeTimers({ toFake: ['Date'] });
    try {
      vi.setSystemTime(Date.parse('2026-10-14T10:01:30.000Z'));
      await openRuns([run({ id: 42, state: 'completed', ended_at: '2026-10-14T10:00:07.900Z' }), run({ id: 41 })]);
      const list = await runsList();
      expect(rowOf(list, '#41 · running').textContent).toContain('for 1m30s');
      expect(rowOf(list, '#42 · completed').textContent).not.toContain('for ');
    } finally {
      vi.useRealTimers();
    }
  });
});

const start = Date.parse(at);
const stamp = (ms: number) => new Date(start + ms).toISOString();

describe('the run timeline', () => {
  const lineOf = (detail: BoundFunctions<typeof queries>) =>
    detail.getAllByRole('listitem').filter((li) => li.closest('ol')?.getAttribute('aria-label') === 'Timeline');

  it('gives each line its offset from the start and a gap only of a second or more', async () => {
    server.activity = [
      activity(1, { kind: 'run_started', summary: 'begin', at: stamp(0) }),
      activity(2, { summary: 'a', at: stamp(3200) }),
      activity(3, { summary: 'b', at: stamp(3900) }),
      activity(4, { summary: 'c', at: stamp(65_000) }),
      activity(5, { summary: 'd', at: stamp(3_725_000) }),
    ];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await detail.findByText('d');
    const lines = lineOf(detail).map((li) => li.textContent.replace(/\s+/g, ' ').trim());
    expect(lines).toEqual([
      '+0.0s run_started begin',
      '+3.2s (+3s) command_finished a',
      '+3.9s command_finished b',
      '+1m05s (+1m01s) command_finished c',
      '+1h02m05s (+1h01m00s) command_finished d',
    ]);
  });

  it('shows the live duration of an open run, and the fixed one of an ended run', async () => {
    server.runs[41] = run({ ended_at: stamp(83_000), state: 'completed' });
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await waitFor(() => {
      expect(detail.getByTestId('run-duration').textContent).toBe('1m23s');
    });
  });

  it('interleaves the interventions by time among the activity, marked, a request and its answer each at its own time', async () => {
    server.activity = [
      activity(1, { summary: 'one', at: stamp(4000) }),
      activity(2, { summary: 'two', at: stamp(6000) }),
      activity(3, { summary: 'three', at: stamp(9000) }),
    ];
    server.interventions = [intervention(9, 'applied', { requested_at: stamp(5000), answered_at: stamp(8000) })];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await detail.findByText('three');
    await waitFor(() => {
      expect(lineOf(detail)).toHaveLength(5);
    });
    const lines = lineOf(detail).map((li) => li.textContent.replace(/\s+/g, ' ').trim());
    expect(lines).toEqual([
      '+4.0s command_finished one',
      '+5.0s (+1s) ↳ #9 message · asked · hello agent',
      '+6.0s (+1s) command_finished two',
      '+8.0s (+2s) ↳ #9 applied',
      '+9.0s (+1s) command_finished three',
    ]);
    // The mark is a class, not only the arrow.
    expect(lineOf(detail)[1]?.querySelector('.ag-iv')).not.toBeNull();
    expect(lineOf(detail)[0]?.querySelector('.ag-iv')).toBeNull();
    // One timeline: no second list, and the heading counts both.
    expect(detail.queryByRole('list', { name: 'Interventions' })).toBeNull();
    expect(detail.getByRole('heading', { name: 'Activity (3) · ↳ interventions (1)' })).toBeDefined();
    expect(detail.getByTestId('intervention-9').textContent).toBe('↳ #9 applied');
  });

  it('words a request not yet answered as requested… and a failed one with its reason', async () => {
    server.interventions = [
      intervention(9, 'requested', { requested_at: stamp(1000) }),
      intervention(10, 'failed', { requested_at: stamp(2000), answered_at: stamp(2500), reason: 'no <b>way</b>' }),
    ];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await waitFor(() => {
      expect(lineOf(detail)).toHaveLength(3);
    });
    const text = lineOf(detail).map((li) => li.textContent.replace(/\s+/g, ' ').trim());
    expect(text[0]).toBe('+1.0s ↳ #9 message · requested… · hello agent');
    expect(text[2]).toBe('+2.5s ↳ #10 failed: no <b>way</b>');
  });

  it('takes a new activity frame into place by its time', async () => {
    server.activity = [activity(1, { summary: 'one', at: stamp(1000) }), activity(3, { summary: 'three', at: stamp(5000) })];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await detail.findByText('three');
    socket.send(activity(2, { summary: 'two', at: stamp(3000) }));
    await detail.findByText('two');
    expect(lineOf(detail).map((li) => li.textContent.replace(/\s+/g, ' ').trim().split(' ').pop())).toEqual(['one', 'two', 'three']);
  });

  it('unfolds and folds an entry with data, as text, and an entry without data is a plain line', async () => {
    server.activity = [activity(1, { summary: 'with data', data: { a: 1, tag: '<i>x</i>' } }), activity(2, { summary: 'plain' })];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    const group = await detail.findByRole('group', { name: 'Data of command_finished with data' });
    const body = group.querySelector('pre');
    expect(group.hasAttribute('open')).toBe(false);
    fireEvent.click(group.querySelector('summary') as HTMLElement);
    expect(group.hasAttribute('open')).toBe(true);
    expect(body?.textContent).toBe(JSON.stringify({ a: 1, tag: '<i>x</i>' }, null, 2));
    expect(body?.querySelector('i')).toBeNull();
    fireEvent.click(group.querySelector('summary') as HTMLElement);
    expect(group.hasAttribute('open')).toBe(false);
    expect(detail.getAllByRole('group')).toHaveLength(1);
  });

  it('cuts a body over 2 KiB with an ellipsis', async () => {
    server.activity = [activity(1, { summary: 'big', data: { text: 'x'.repeat(5000) } })];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    const body = (await detail.findByRole('group', { name: 'Data of command_finished big' })).querySelector('pre');
    const text = body?.textContent ?? '';
    expect(text.endsWith('…')).toBe(true);
    expect(new TextEncoder().encode(text.slice(0, -1))).toHaveLength(2048);
  });
});

describe('Go to message', () => {
  const held = (id: number) => ({ schema: 'conch.message.v2', id, channel_id: 7, author_id: 3, created_at: at, body: `message ${String(id)}` });
  let scrolled: Element[];
  beforeEach(() => {
    scrolled = [];
    // The channel list keeps the open channel in sight (issue #584); every
    // other scroll is counted.
    Element.prototype.scrollIntoView = function (this: Element) {
      if (this.closest('.channel-list') === null) scrolled.push(this);
    };
  });
  afterEach(() => {
    Reflect.deleteProperty(Element.prototype, 'scrollIntoView');
  });

  it('closes run detail and scrolls to the message the run answers, with focus on it', async () => {
    server.messages = [held(6), held(5), held(4)];
    server.runs[41] = run({ message_id: 5 });
    const socket = await openApp();
    await screen.findByText('message 5');
    const detail = within(await openRunDetail(socket, {}));
    fireEvent.click(await detail.findByRole('button', { name: 'Go to message #5' }));
    await waitFor(() => {
      expect(scrolled).toHaveLength(1);
    });
    expect(screen.queryByRole('article', { name: 'Run detail' })).toBeNull();
    expect(scrolled[0]?.getAttribute('data-message-id')).toBe('5');
    expect(scrolled[0]?.textContent).toContain('message 5');
    expect(document.activeElement).toBe(scrolled[0]);
  });

  it('says the message is older than the history held when it is not there', async () => {
    server.messages = [held(6)];
    server.runs[41] = run({ message_id: 5 });
    const socket = await openApp();
    await screen.findByText('message 6');
    const detail = within(await openRunDetail(socket, {}));
    fireEvent.click(await detail.findByRole('button', { name: 'Go to message #5' }));
    await panel().findByText('Message #5 is older than the history held');
    expect(scrolled).toHaveLength(0);
    expect(screen.queryByRole('article', { name: 'Run detail' })).toBeNull();
  });

  it('is disabled and says No message when the run answers none', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    const button = await detail.findByRole('button', { name: 'Go to message: this run answers no message' });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect(button.textContent).toBe('No message');
  });
});

// The people groups and the folded agent sections (issue #594).

const humans = { members: members.members.filter((m) => m.kind === 'human') };
const listed = (list: HTMLElement) => within(list).getAllByRole('listitem').map((li) => li.querySelector('.ag-name')?.textContent);

describe('the members column', () => {
  it('names the count of a person with more than one online session, and shows none for one', async () => {
    const socket = await openApp();
    await showRoster(socket, [session(100, 2), session(101, 2)]);
    const ada = within(panel().getByRole('list', { name: 'Online — 1' })).getByText('ada').closest('li') as HTMLElement;
    expect(within(ada).getByText('(2 sessions)')).toBeDefined();
    socket.send(roster([session(100, 2)]));
    await waitFor(() => {
      expect(panel().queryByText('(2 sessions)')).toBeNull();
    });
    expect(within(panel().getByRole('list', { name: 'Online — 1' })).getByText('ada').closest('li')?.textContent).not.toMatch(/\(\d/);
  });

  it('groups people by Online (with stale) and Offline, with counts, and moves one who goes offline at the next roster', async () => {
    server.members = {
      members: [
        ...humans.members,
        { principal_id: 6, created_at: at, name: 'cy', kind: 'human' },
        { principal_id: 8, created_at: at, name: 'dee', kind: 'human' },
      ],
    };
    const cy = (state: string): Json => session(102, 6, { name: 'cy', state, status: state });
    const socket = await openApp();
    await showRoster(socket, [session(100, 2), cy('stale'), session(103, 8, { name: 'dee', state: 'offline', status: 'offline' })]);
    await panel().findByText('dee');
    const onlineHeading = panel().getByRole('heading', { name: 'Online — 2' });
    const offlineHeading = panel().getByRole('heading', { name: 'Offline — 2' });
    expect(onlineHeading.compareDocumentPosition(offlineHeading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    const online = panel().getByRole('list', { name: 'Online — 2' });
    expect(listed(online)).toEqual(['ada', 'cy']);
    // Stale is still here, and still says so.
    expect(within(online).getByText('cy').closest('li')?.textContent).toContain('stale');
    expect(listed(panel().getByRole('list', { name: 'Offline — 2' }))).toEqual(['<b>mallory</b>', 'dee']);

    socket.send(roster([session(100, 2), cy('offline'), session(103, 8, { name: 'dee', state: 'offline', status: 'offline' })]));
    const offline = await panel().findByRole('list', { name: 'Offline — 3' });
    expect(listed(offline)).toEqual(['<b>mallory</b>', 'cy', 'dee']);
    expect(listed(panel().getByRole('list', { name: 'Online — 1' }))).toEqual(['ada']);
  });

  it('folds Agents and Runs to one line in a channel with neither, once the runs are read', async () => {
    server.members = humans;
    server.holdBefore = '';
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    // While the runs load, the sections show as before.
    await panel().findByText('Loading runs…');
    expect(panel().getByRole('heading', { name: 'Agents' })).toBeDefined();
    await waitFor(() => {
      expect(server.held).toHaveLength(1);
    });
    server.held[0]?.();
    await panel().findByText('No agents or runs in this channel.');
    expect(panel().queryByRole('heading', { name: 'Agents' })).toBeNull();
    expect(panel().queryByRole('heading', { name: 'Runs' })).toBeNull();
    expect(panel().queryByRole('combobox', { name: 'Filter runs' })).toBeNull();
    // A run that starts brings the sections back.
    socket.send(run({ id: 41 }));
    await panel().findByRole('list', { name: 'Channel runs' });
    expect(panel().queryByText('No agents or runs in this channel.')).toBeNull();
  });

  it('shows both sections for an agent with no runs', async () => {
    await openRuns([]);
    await panel().findByText('No runs in this channel');
    expect(panel().getByRole('heading', { name: 'Agents' })).toBeDefined();
    expect(within(panel().getByRole('list', { name: 'Agents' })).getByText('bot')).toBeDefined();
    expect(panel().getByRole('heading', { name: 'Runs' })).toBeDefined();
    expect(panel().getByRole('combobox', { name: 'Filter runs' })).toBeDefined();
    expect(panel().queryByText('No agents or runs in this channel.')).toBeNull();
  });

  it('shows both sections for runs with no agents', async () => {
    server.members = humans;
    server.channelRuns = [run({ id: 41 })];
    const socket = await openApp();
    await showRoster(socket, [session(100, 2)]);
    await runsList();
    expect(panel().getByRole('heading', { name: 'Agents' })).toBeDefined();
    expect(panel().getByText('No agents in this channel')).toBeDefined();
    expect(panel().getByRole('heading', { name: 'Runs' })).toBeDefined();
    expect(panel().queryByText('No agents or runs in this channel.')).toBeNull();
  });
});
