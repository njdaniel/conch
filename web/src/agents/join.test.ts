import { describe, expect, it } from 'vitest';
import type { ChannelMemberV1 } from '../schema/channel';
import type { SessionStatus, SessionV1 } from '../schema/session';
import { joinMembers } from './join';

const at = '2026-10-14T10:00:00Z';

const member = (principal_id: number, name: string | undefined, kind: 'human' | 'agent' | undefined): ChannelMemberV1 => ({
  principal_id,
  created_at: at,
  ...(name === undefined ? {} : { name }),
  ...(kind === undefined ? {} : { kind }),
});

const session = (id: number, principal_id: number, over: Partial<SessionV1> = {}): SessionV1 => ({
  schema: 'conch.session.v1',
  id,
  principal_id,
  kind: 'human',
  name: `p${String(principal_id)}`,
  client: 'browser',
  state: 'online',
  status: 'online',
  started_at: at,
  last_seen_at: at,
  interventions: [],
  outcome_unknown: false,
  ...over,
});

describe('joinMembers', () => {
  it('lists each principal once, people and agents apart, in principal id order', () => {
    const joined = joinMembers(
      [member(4, 'dee', 'human'), member(3, 'bot', 'agent'), member(2, 'ada', 'human')],
      [session(1, 2), session(2, 3, { kind: 'agent', status: 'idle' })],
    );
    expect(joined.people.map((p) => p.name)).toEqual(['ada', 'dee']);
    expect(joined.agents.map((a) => a.name)).toEqual(['bot']);
  });

  it('counts a person with several live sessions once, and only the online ones', () => {
    const joined = joinMembers(
      [member(2, 'ada', 'human')],
      [session(1, 2), session(2, 2), session(3, 2, { state: 'stale', status: 'stale' })],
    );
    expect(joined.people).toHaveLength(1);
    expect(joined.people[0]?.live).toBe(2);
    expect(joined.people[0]?.sessions).toHaveLength(3);
    expect(joined.people[0]?.state).toBe('online');
  });

  it('shows a member with no session as offline', () => {
    const joined = joinMembers([member(2, 'ada', 'human'), member(3, 'bot', 'agent')], []);
    expect(joined.people[0]).toMatchObject({ name: 'ada', live: 0, state: 'offline' });
    expect(joined.agents[0]).toMatchObject({ name: 'bot', status: 'offline', sessions: [] });
  });

  it('adds a principal only the roster knows after the members', () => {
    const joined = joinMembers([member(2, 'ada', 'human')], [session(9, 7, { kind: 'agent', name: 'newcomer', status: 'running' })]);
    expect(joined.agents.map((a) => a.name)).toEqual(['newcomer']);
    expect(joined.agents[0]?.status).toBe('running');
  });

  it('falls back to #id for a member without a name, and reads a missing kind as a person', () => {
    const joined = joinMembers([member(5, undefined, undefined)], []);
    expect(joined.people[0]?.name).toBe('#5');
  });

  it.each<[SessionStatus[], SessionStatus]>([
    [['idle', 'blocked', 'running'], 'blocked'],
    [['running', 'waiting'], 'waiting'],
    [['idle', 'failed'], 'failed'],
    [['stale', 'idle'], 'idle'],
    [['offline', 'stale'], 'stale'],
  ])('shows the status that needs a person first: %j gives %s', (statuses, want) => {
    const sessions = statuses.map((status, i) => session(i + 1, 3, { kind: 'agent', status, state: status === 'stale' ? 'stale' : status === 'offline' ? 'offline' : 'online' }));
    expect(joinMembers([member(3, 'bot', 'agent')], sessions).agents[0]?.status).toBe(want);
  });
});
