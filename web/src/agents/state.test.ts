import { describe, expect, it } from 'vitest';
import type { AgentActivityV1 } from '../schema/activity';
import type { InterventionState, InterventionV1 } from '../schema/intervention';
import type { RunState, RunV1 } from '../schema/run';
import type { SessionV1 } from '../schema/session';
import { agentsReducer, initialAgents, initialRuns, keptActivity, keptInterventions, mergeActivity, recentRuns, runsPageSize, upsertIntervention, type AgentsAction, type AgentsState } from './state';
import { parseEventFrame } from './frames';

const at = '2026-10-14T10:00:00Z';

const run = (over: Partial<RunV1> = {}): RunV1 => ({
  schema: 'conch.run.v1',
  id: 41,
  session_id: 12,
  principal_id: 3,
  channel_id: 7,
  state: 'running',
  task: 'task',
  started_at: at,
  ...over,
});

const iv = (id: number, state: InterventionState, over: Partial<InterventionV1> = {}): InterventionV1 => ({
  schema: 'conch.intervention.v1',
  id,
  run_id: 41,
  kind: 'cancel',
  state,
  requested_by: 2,
  requested_at: at,
  ...over,
});

const entry = (id: number, run_id = 41): AgentActivityV1 => ({
  schema: 'conch.agent.activity.v1',
  id,
  run_id,
  principal_id: 3,
  kind: 'command_started',
  summary: `s${String(id)}`,
  at,
});

function play(actions: AgentsAction[], from: AgentsState = initialAgents): AgentsState {
  return actions.reduce(agentsReducer, from);
}

const open = (state: RunState = 'running'): AgentsAction[] => [
  { type: 'reset', channelId: 7 },
  { type: 'open', runId: 41, sessionId: 12, run: run({ state }) },
];

describe('the run is never shown ahead of its document', () => {
  it('keeps the state after a cancel is requested, acknowledged and applied, until a run document says cancelled', () => {
    let s = play(open());
    s = play([{ type: 'sent', runId: 41, intervention: iv(1, 'requested') }], s);
    expect(s.detail?.run?.state).toBe('running');
    s = play([{ type: 'intervention', intervention: iv(1, 'acknowledged', { answered_at: at }) }], s);
    s = play([{ type: 'intervention', intervention: iv(1, 'applied', { answered_at: at }) }], s);
    expect(s.detail?.interventions.map((x) => x.state)).toEqual(['applied']);
    expect(s.detail?.run?.state).toBe('running');
    s = play([{ type: 'run', run: run({ state: 'cancelled', ended_at: at }) }], s);
    expect(s.detail?.run?.state).toBe('cancelled');
  });

  it('does not take a run back from an ended state', () => {
    const s = play([...open('completed'), { type: 'run', run: run({ state: 'running' }) }]);
    expect(s.detail?.run?.state).toBe('completed');
  });

  it('lets a frame win over a read that was asked before it', () => {
    let s = play(open());
    const stamp = s.detail?.runStamp ?? -1;
    s = play([{ type: 'run', run: run({ state: 'blocked' }) }], s);
    s = play([{ type: 'runRead', runId: 41, stamp, run: run({ state: 'running' }) }], s);
    expect(s.detail?.run?.state).toBe('blocked');
  });
});

describe('frames of something else are ignored', () => {
  it('a run of another channel', () => {
    const s = play([...open(), { type: 'run', run: run({ channel_id: 8, state: 'cancelled', ended_at: at }) }]);
    expect(s.detail?.run?.state).toBe('running');
  });

  it('a roster of another channel', () => {
    const s = play([{ type: 'reset', channelId: 7 }, { type: 'roster', channelId: 8, sessions: [] }]);
    expect(s.link).toBe('connecting');
  });

  it('activity and interventions of another run', () => {
    const s = play([...open(), { type: 'activity', entry: entry(1, 99) }, { type: 'intervention', intervention: iv(1, 'applied', { run_id: 99, answered_at: at }) }]);
    expect(s.detail?.activity).toEqual([]);
    expect(s.detail?.interventions).toEqual([]);
  });

  it('a run frame of a run that is not open updates only the session list', () => {
    const s = play([
      ...open(),
      { type: 'sessionRuns', session: 12, runs: [run({ id: 40, state: 'running' })] },
      { type: 'run', run: run({ id: 40, state: 'failed', ended_at: at }) },
    ]);
    expect(s.detail?.run?.state).toBe('running');
    expect(s.sessionRuns[12]?.[0]?.state).toBe('failed');
  });
});

describe('the roster', () => {
  const sessions: SessionV1[] = [];
  it('is dropped when the socket goes down and is live again with the next one', () => {
    let s = play([{ type: 'reset', channelId: 7 }, { type: 'roster', channelId: 7, sessions }]);
    expect(s.link).toBe('live');
    expect(s.liveGen).toBe(1);
    s = play([{ type: 'down' }], s);
    expect(s.link).toBe('down');
    expect(s.roster).toEqual([]);
    s = play([{ type: 'roster', channelId: 7, sessions }], s);
    expect(s.liveGen).toBe(2);
  });

  it('does not count a second roster of one socket as a new connection', () => {
    const s = play([{ type: 'reset', channelId: 7 }, { type: 'roster', channelId: 7, sessions }, { type: 'roster', channelId: 7, sessions }]);
    expect(s.liveGen).toBe(1);
  });
});

describe('merging', () => {
  it('upserts an intervention by id and never takes its state back', () => {
    const list = upsertIntervention([iv(1, 'applied', { answered_at: at })], iv(1, 'requested'));
    expect(list.map((x) => x.state)).toEqual(['applied']);
    expect(upsertIntervention([iv(2, 'requested')], iv(1, 'requested')).map((x) => x.id)).toEqual([1, 2]);
  });

  it('keeps the newest interventions when frames keep adding them', () => {
    let list: InterventionV1[] = [];
    for (let i = 1; i <= keptInterventions + 5; i++) list = upsertIntervention(list, iv(i, 'requested'));
    expect(list).toHaveLength(keptInterventions);
    expect(list[0]?.id).toBe(6);
  });

  it('merges activity by id, oldest first, keeping the newest', () => {
    expect(mergeActivity([entry(2), entry(3)], [entry(1), entry(3)]).map((e) => e.id)).toEqual([1, 2, 3]);
    const many = Array.from({ length: keptActivity + 5 }, (_, i) => entry(i + 1));
    const merged = mergeActivity([], many);
    expect(merged).toHaveLength(keptActivity);
    expect(merged[0]?.id).toBe(6);
  });

  it('keeps a session run list to the newest few, newest first', () => {
    const runs = Array.from({ length: recentRuns }, (_, i) => run({ id: 100 - i, state: 'completed', ended_at: at }));
    const s = play([{ type: 'reset', channelId: 7 }, { type: 'sessionRuns', session: 12, runs }, { type: 'run', run: run({ id: 200 }) }]);
    expect(s.sessionRuns[12]).toHaveLength(recentRuns);
    expect(s.sessionRuns[12]?.[0]?.id).toBe(200);
  });
});

describe('parseEventFrame', () => {
  it('reads the four documents and nothing else', () => {
    expect(parseEventFrame(JSON.stringify(run()))?.kind).toBe('run');
    expect(parseEventFrame(JSON.stringify(entry(1)))?.kind).toBe('activity');
    expect(parseEventFrame(JSON.stringify(iv(1, 'requested')))?.kind).toBe('intervention');
    expect(parseEventFrame(JSON.stringify({ schema: 'conch.session_roster.v1', channel_id: 7, sessions: [] }))?.kind).toBe('roster');
  });

  // Issue #728: the channel as it now is, sent when it is renamed.
  it('reads the channel document, the whole of it', () => {
    const doc = { schema: 'conch.channel.v1', channel_id: 7, name: 'ops-room', created_at: '2026-10-21T00:00:00Z' };
    expect(parseEventFrame(JSON.stringify(doc))).toEqual({ kind: 'channel', channel: doc });
  });

  it.each([
    ['not JSON', 'nope'],
    ['not text', 12],
    ['an unknown schema', JSON.stringify({ schema: 'conch.future.v9' })],
    ['a run with an unknown state', JSON.stringify(run({ state: 'exploded' as RunState }))],
    ['a null', 'null'],
    ['a channel document with no name', JSON.stringify({ schema: 'conch.channel.v1', channel_id: 7, created_at: '2026-10-21T00:00:00Z' })],
    ['a channel document with a name that is not text', JSON.stringify({ schema: 'conch.channel.v1', channel_id: 7, name: 7, created_at: '2026-10-21T00:00:00Z' })],
    ['a channel document of a later version', JSON.stringify({ schema: 'conch.channel.v2', channel_id: 7, name: 'ops-room', created_at: '2026-10-21T00:00:00Z' })],
  ])('skips %s', (_name, data) => {
    expect(parseEventFrame(data)).toBeNull();
  });
});

describe('the Runs view', () => {
  const page = (from: number, count: number): RunV1[] => Array.from({ length: count }, (_, i) => run({ id: from - i }));
  const loaded = (): AgentsState => play([{ type: 'reset', channelId: 7 }, { type: 'runsAsk' }, { type: 'runsPage', before: 0, runs: page(100, runsPageSize) }]);

  it('keeps the paging of the first page: the smallest id, more below', () => {
    const s = loaded();
    expect(s.runs).toMatchObject({ loaded: true, loading: false, more: true, before: 51, pages: 1 });
  });

  it('drops a page asked before a reset, which is not the first page of the new view', () => {
    const s = play([{ type: 'runsAsk' }, { type: 'reset', channelId: 7 }, { type: 'runsPage', before: 51, runs: page(50, 50) }], loaded());
    expect(s.runs).toEqual(initialRuns);
  });

  it('reads the first page again as a merge: no duplicates, the paging as it was', () => {
    const s = play([{ type: 'runsPage', before: 0, runs: [run({ id: 101 }), run({ id: 100, state: 'blocked' }), ...page(99, 3)] }], loaded());
    expect(s.runs.list).toHaveLength(51);
    expect(s.runs.list.map((r) => r.id).slice(0, 3)).toEqual([101, 100, 99]);
    expect(s.runs.list[1]?.state).toBe('blocked');
    expect(s.runs).toMatchObject({ before: 51, pages: 1, more: true });
  });

  it('does not take an ended run held from a frame back to a state a read held earlier', () => {
    let s = play([{ type: 'run', run: run({ id: 100, state: 'completed', ended_at: at }) }], loaded());
    s = play([{ type: 'runsPage', before: 0, runs: [run({ id: 100, state: 'running' })] }], s);
    expect(s.runs.list.find((r) => r.id === 100)?.state).toBe('completed');
  });

  it('lets the failure of a refresh of a held first page, and of a page no longer asked for, change nothing', () => {
    const s = loaded();
    expect(play([{ type: 'runsFailed', before: 0, line: 'x' }], s)).toBe(s);
    expect(play([{ type: 'runsFailed', before: 7, line: 'x' }], s)).toBe(s);
    const first = play([{ type: 'reset', channelId: 7 }, { type: 'runsAsk' }, { type: 'runsFailed', before: 0, line: 'x' }]);
    expect(first.runs).toMatchObject({ loading: false, note: 'x', loaded: false });
  });
});
