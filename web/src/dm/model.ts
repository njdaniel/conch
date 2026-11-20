// The merge rules of a DM transcript, with no React in them, so that live
// frames, history pages, the changes route and a send's own answer all land in
// one place and cannot disagree (pkg/schema/dm.go, DMMessageV1.Revision).
//
// A message is one id with several copies over time: its send, each edit, its
// removal. The copy with the higher Revision wins; a removed copy is never
// replaced by one that is not (the tombstone is final, and it is what a
// reconnect shows). Messages of one conversation are ordered by id, oldest
// first. Ids are only comparable inside one conversation.
import type { DMMessageV1 } from '../schema/generated';
import type { DmFailure } from './api';

export const removedBody = 'This message was removed by its author.';

export type Held = ReadonlyMap<number, DMMessageV1>;

// supersedes says whether next should replace held (undefined: nothing held).
export function supersedes(held: DMMessageV1 | undefined, next: DMMessageV1): boolean {
  if (held === undefined) return true;
  if (held.removed === true) return false;
  if (next.removed === true) return true;
  return next.revision > held.revision;
}

// merge returns held with incoming applied, or held itself when nothing changed,
// so a state that did not change is not re-rendered.
export function merge(held: Held, incoming: readonly DMMessageV1[]): Held {
  let next: Map<number, DMMessageV1> | null = null;
  for (const message of incoming) {
    const current = (next ?? held).get(message.id);
    if (!supersedes(current, message)) continue;
    next ??= new Map(held);
    next.set(message.id, message);
  }
  return next ?? held;
}

export const ordered = (held: Held): DMMessageV1[] => [...held.values()].sort((a, b) => a.id - b.id);

// The highest Revision among messages, or floor if higher.
export const revisionOf = (messages: readonly DMMessageV1[], floor: number): number => messages.reduce((top, m) => Math.max(top, m.revision), floor);

// A send the person made that the server has not confirmed yet. nonce is the
// whole identity of the send: sending it again is safe, and the author's own
// copy of the stored message carries it back, which ends the pending entry.
export interface Pending {
  readonly nonce: string;
  readonly body: string;
  readonly fileIds: readonly number[];
  readonly status: 'sending' | 'failed' | 'unconfirmed';
  readonly failure?: DmFailure | 'unconfirmed';
}

export function settle(pending: readonly Pending[], arrived: readonly DMMessageV1[]): readonly Pending[] {
  const nonces = new Set(arrived.flatMap((m) => (m.client_nonce === undefined ? [] : [m.client_nonce])));
  if (nonces.size === 0) return pending;
  const kept = pending.filter((p) => !nonces.has(p.nonce));
  return kept.length === pending.length ? pending : kept;
}

// What the person is told when the peer cannot be reached: one line for every
// reason, never which (ADR-017).
export const cannotMessage = "Can't send a message to this person";

export const noAccess = 'This conversation is no longer available to you.';
