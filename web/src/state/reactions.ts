// Whole server summaries only. Optimistic intent is separate from counts and
// lasts until an authoritative answer arrives. No message/read-state action.
import type { ReactionCountV1, ReactionSummaryV1 } from '../schema/reaction';

export interface ReactionState {
  // Batch omissions have no as_of. Frames for these ids need a fresh batch
  // confirmation before they can repopulate an unversioned empty snapshot.
  readonly unversioned: ReadonlySet<number>;
  readonly summaries: ReadonlyMap<number, ReactionSummaryV1>;
  readonly revisions: ReadonlyMap<number, number>;
}
export const emptyReactions: ReactionState = { summaries: new Map(), revisions: new Map(), unversioned: new Set() };
export type ReactionAction =
  | { readonly type: 'summary'; readonly summary: ReactionSummaryV1; readonly channelId: number; readonly authoritative?: boolean }
  | { readonly type: 'batch'; readonly ids: readonly number[]; readonly summaries: readonly ReactionSummaryV1[]; readonly channelId: number; readonly before: ReactionState }
  | { readonly type: 'prune'; readonly ids: ReadonlySet<number> }
  | { readonly type: 'reset' };

// Date.parse truncates Go's nanoseconds. Compare the fractional part too,
// including timestamps whose fractional seconds have different lengths.
export function compareReactionTime(a: string, b: string): number {
  const seconds = (s: string) => Date.parse(s.replace(/\.\d+/, ''));
  const diff = seconds(a) - seconds(b);
  if (diff !== 0) return diff;
  const fraction = (s: string) => (/\.(\d+)/.exec(s)?.[1] ?? '').padEnd(9, '0');
  return fraction(a).localeCompare(fraction(b));
}

export function reactionReducer(state: ReactionState, action: ReactionAction): ReactionState {
  switch (action.type) {
    case 'reset': return emptyReactions;
    case 'prune': return {
      unversioned: new Set([...state.unversioned].filter((id) => action.ids.has(id))),
      summaries: new Map([...state.summaries].filter(([id]) => action.ids.has(id))),
      revisions: new Map([...state.revisions].filter(([id]) => action.ids.has(id))),
    };
    case 'summary': {
      const incoming = action.summary;
      if (incoming.channel_id !== action.channelId || (state.unversioned.has(incoming.message_id) && action.authoritative !== true)) return state;
      const held = state.summaries.get(incoming.message_id);
      if (held !== undefined && compareReactionTime(incoming.as_of, held.as_of) < 0) return state;
      return {
        unversioned: new Set([...state.unversioned].filter((id) => id !== incoming.message_id)),
        summaries: new Map(state.summaries).set(incoming.message_id, incoming),
        revisions: new Map(state.revisions).set(incoming.message_id, (state.revisions.get(incoming.message_id) ?? 0) + 1),
      };
    }
    case 'batch': {
      let next = state;
      const returned = new Set<number>();
      for (const summary of action.summaries) {
        if (!action.ids.includes(summary.message_id) || summary.channel_id !== action.channelId) continue;
        returned.add(summary.message_id);
        if (state.unversioned.has(summary.message_id) && state.revisions.get(summary.message_id) !== action.before.revisions.get(summary.message_id)) continue;
        next = reactionReducer(next, { type: 'summary', summary, channelId: action.channelId, authoritative: true });
      }
      for (const id of action.ids) {
        if (returned.has(id) || state.revisions.get(id) !== action.before.revisions.get(id)) continue;
        const held = next.summaries.get(id);
        // Preserve the high-water timestamp when a batch omits an empty or
        // inaccessible message; an older frame must not resurrect its counts.
        next = {
          unversioned: new Set(next.unversioned).add(id),
          summaries: held === undefined ? next.summaries : new Map(next.summaries).set(id, { ...held, reactions: [] }),
          revisions: new Map(next.revisions).set(id, (next.revisions.get(id) ?? 0) + 1),
        };
      }
      return next;
    }
  }
}

export function optimisticReactions(counts: readonly ReactionCountV1[], emoji: string, set: boolean): readonly ReactionCountV1[] {
  const held = counts.find((r) => r.emoji === emoji);
  if (Boolean(held?.me) === set) return counts;
  if (held === undefined) return [...counts, { emoji, count: 1, me: true }];
  return counts.map((r) => r.emoji === emoji ? { ...r, count: r.count + (set ? 1 : -1), me: set } : r).filter((r) => r.count > 0);
}
