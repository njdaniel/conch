// The members panel, agent detail, run detail and interventions as the person
// sees them, against a fake conchd (fetch) and a fake WebSocket that sends the
// frames. No network, no browser.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
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
  runs: Record<number, Json>;
  activity: Json[];
  interventions: Json[];
  asks: { run: number; body: Json }[];
  askReply: () => Response;
  nextSession: number;
}

let server: Server;

function install() {
  server = {
    sessionCreates: [],
    sessionStatus: () => 201,
    deletes: [],
    runList: [],
    runListUrls: [],
    runs: { 41: run() },
    activity: [],
    interventions: [],
    asks: [],
    askReply: () => json({}, 500),
    nextSession: 100,
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
      if (m?.[2] === 'members') return Promise.resolve(json(members));
      if (m?.[2] === 'nets') return Promise.resolve(json({ nets: [] }));
      if (m?.[2] === 'messages') return Promise.resolve(json({ messages: [] }));
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
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
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
    const people = panel().getByRole('list', { name: 'People' });
    const ada = within(people).getByText('ada').closest('li');
    expect(ada?.textContent).toContain('(2)');
    expect(ada?.textContent).toContain('online');
    // A member with no session reads offline, and its name is text, not markup.
    const mallory = within(people).getByText('<b>mallory</b>').closest('li');
    expect(mallory?.textContent).toContain('offline');
    expect(people.querySelector('b')).toBeNull();
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
    expect(within(detail.getByRole('list', { name: 'Interventions' })).getByTestId('intervention-9').textContent).toBe('Applied');
    socket.send(activity(3));
    await detail.findByText('ran step 3');
    socket.send(activity(3, { summary: 'another run', run_id: 99 }));
    expect(detail.queryByText('another run')).toBeNull();
    expect(detail.getByRole('heading', { name: 'Activity (3)' })).toBeDefined();
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
  it('offers Message and Cancel when the session declares them', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    expect((detail.getByRole<HTMLButtonElement>('button', { name: 'Message' })).disabled).toBe(false);
    expect((detail.getByRole<HTMLButtonElement>('button', { name: 'Cancel' })).disabled).toBe(false);
    expect(detail.queryByText('Not supported by this agent')).toBeNull();
  });

  it.each([
    ['message', 'Message', ['cancel']],
    ['cancel', 'Cancel', ['message']],
  ])('shows %s disabled, saying Not supported by this agent, when it is not declared', async (_kind, name, declared) => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: declared }));
    const button = detail.getByRole<HTMLButtonElement>('button', { name });
    expect(button.disabled).toBe(true);
    expect(detail.getAllByText('Not supported by this agent')).toHaveLength(1);
    const note = document.getElementById(button.getAttribute('aria-describedby') ?? '');
    expect(note?.textContent).toBe('Not supported by this agent');
  });

  it('shows both unsupported for a session that declares nothing', async () => {
    const socket = await openApp();
    const detail = within(await openRunDetail(socket, { interventions: [] }));
    expect(detail.getAllByText('Not supported by this agent')).toHaveLength(2);
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
      expect(detail.getByTestId('intervention-7').textContent).toBe('Requested');
    });
    expect(server.asks).toEqual([{ run: 41, body: { kind: 'message', body: 'hello agent' } }]);
    socket.send(intervention(7, 'acknowledged'));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-7').textContent).toBe('Acknowledged');
    });
    socket.send(intervention(7, 'applied'));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-7').textContent).toBe('Applied');
    });
    // A repeated, older document does not take it back.
    socket.send(intervention(7, 'requested'));
    expect(detail.getByTestId('intervention-7').textContent).toBe('Applied');
    expect(server.asks).toHaveLength(1);
  });

  it('shows Failed with the reason from the frame, as text', async () => {
    server.interventions = [intervention(8, 'requested')];
    const socket = await openApp();
    const detail = within(await openRunDetail(socket));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-8').textContent).toBe('Requested');
    });
    socket.send(intervention(8, 'failed', { reason: 'the agent is <b>busy</b>' }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-8').textContent).toBe('Failed: the agent is <b>busy</b>');
    });
    expect(detail.getByRole('list', { name: 'Interventions' }).querySelector('b')).toBeNull();
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
      expect(detail.getByTestId('intervention-5').textContent).toBe('Requested');
    });
    expect(server.asks).toEqual([{ run: 41, body: { kind: 'cancel' } }]);
    expect(detail.getByTestId('run-state').textContent).toBe('running');
    socket.send(intervention(5, 'acknowledged', { kind: 'cancel', body: undefined }));
    socket.send(intervention(5, 'applied', { kind: 'cancel', body: undefined }));
    await waitFor(() => {
      expect(detail.getByTestId('intervention-5').textContent).toBe('Applied');
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
    expect(detail.getByRole('heading', { name: 'Interventions (0)' })).toBeDefined();
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
