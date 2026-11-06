import { describe, expect, it } from 'vitest';
import { reactionSummarySchema, type ReactionSummaryV1 } from '../schema/reaction';
import { compareReactionTime, emptyReactions, optimisticReactions, reactionReducer } from './reactions';
const summary = (count = 1, at = '2026-11-06T10:00:00.000000001Z', me = true): ReactionSummaryV1 => ({ schema: reactionSummarySchema, channel_id: 2, message_id: 4, as_of: at, reactions: count === 0 ? [] : [{ emoji: '👍', count, me }] });
const apply = (state = emptyReactions, value = summary()) => reactionReducer(state, { type: 'summary', summary: value, channelId: 2 });
describe('whole reaction state', () => {
  it('duplicates, concurrent principals and retried set responses replace counts', () => {
    let state = apply();
    state = apply(state);
    expect(state.summaries.get(4)?.reactions[0]?.count).toBe(1);
    state = apply(state, summary(2));
    state = apply(state, summary(2));
    expect(state.summaries.get(4)?.reactions[0]?.count).toBe(2);
  });
  it('a removed reaction stays removed when older responses and frames arrive', () => {
    const removed = apply(apply(), summary(0, '2026-11-06T10:00:00.000000002Z'));
    expect(apply(removed)).toBe(removed);
    expect(removed.summaries.get(4)?.reactions).toEqual([]);
    expect(compareReactionTime('2026-11-06T10:00:00.1Z', '2026-11-06T10:00:00.100000001Z')).toBeLessThan(0);
  });
  it('equal timestamps use the later arrival, and another channel is ignored', () => {
    const state = apply();
    expect(apply(state, summary(2)).summaries.get(4)?.reactions[0]?.count).toBe(2);
    expect(apply(state, { ...summary(), channel_id: 3 })).toBe(state);
  });
  it('missing batch ids clear counts without overwriting a frame received during hydration', () => {
    const before = apply();
    const after = apply(before, summary(3, '2026-11-06T10:00:00.2Z'));
    const batch = { type: 'batch' as const, ids: [4], summaries: [], channelId: 2, before };
    expect(reactionReducer(after, batch).summaries.get(4)?.reactions[0]?.count).toBe(3);
    const cleared = reactionReducer(before, batch);
    expect(cleared.summaries.get(4)?.reactions).toEqual([]);
    expect(apply(cleared, summary(1, '2026-11-06T09:59:59Z'))).toBe(cleared);
  });
  it('reordered overlapping batches cannot clear a newer response', () => {
    const before = emptyReactions;
    const state = reactionReducer(before, { type: 'batch', ids: [4], summaries: [summary()], channelId: 2, before });
    expect(reactionReducer(state, { type: 'batch', ids: [4], summaries: [], channelId: 2, before })).toEqual(state);
  });
  it('prunes evicted messages and resets on scope changes', () => {
    expect(reactionReducer(apply(), { type: 'prune', ids: new Set() })).toEqual(emptyReactions);
    expect(reactionReducer(apply(), { type: 'reset' })).toBe(emptyReactions);
  });
  it('optimistic set/remove intent is idempotent and never mutates authority', () => {
    const held = summary().reactions;
    expect(optimisticReactions(held, '👍', true)).toBe(held);
    expect(optimisticReactions(held, '👍', false)).toEqual([]);
    expect(held[0]?.count).toBe(1);
    expect(optimisticReactions([], '❤️', true)).toEqual([{ emoji: '❤️', count: 1, me: true }]);
  });
});

it('does not resurrect an omitted empty id from a later-arriving frame or an older overlapping batch', () => {
  const cleared = reactionReducer(emptyReactions, { type: 'batch', ids: [4], summaries: [], channelId: 2, before: emptyReactions });
  expect(apply(cleared, summary(2, '2026-11-06T10:00:04Z'))).toBe(cleared);
  const stale = reactionReducer(cleared, { type: 'batch', ids: [4], summaries: [summary()], channelId: 2, before: emptyReactions });
  expect(stale.summaries.get(4)).toBeUndefined();
  const fresh = reactionReducer(cleared, { type: 'batch', ids: [4], summaries: [summary(3, '2026-11-06T10:00:05Z')], channelId: 2, before: cleared });
  expect(fresh.summaries.get(4)?.reactions[0]?.count).toBe(3);
});
