import { describe, expect, it } from 'vitest';
import type { AgentActivityV1 } from '../schema/activity';
import type { InterventionState, InterventionV1 } from '../schema/intervention';
import type { RunState, RunV1 } from '../schema/run';
import type { SessionV1 } from '../schema/session';
import { agentsReducer, initialAgents, keptActivity, mergeActivity, recentRuns, upsertIntervention, type AgentsAction, type AgentsState } from './state';
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

  it.each([
    ['not JSON', 'nope'],
    ['not text', 12],
    ['an unknown schema', JSON.stringify({ schema: 'conch.future.v9' })],
    ['a run with an unknown state', JSON.stringify(run({ state: 'exploded' as RunState }))],
    ['a null', 'null'],
  ])('skips %s', (_name, data) => {
    expect(parseEventFrame(data)).toBeNull();
  });
});
