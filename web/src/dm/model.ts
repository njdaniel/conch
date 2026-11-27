// The merge rules of a DM transcript, with no React in them, so that live
// frames, history pages, the changes route and a send's own answer all land in
// one place and cannot disagree (pkg/schema/dm.go, DMMessageV1.Revision).
//
// A message is one id with several copies over time: its send, each edit, its
// removal. The copy with the higher Revision wins; a removed copy is never
// replaced by one that is not (the tombstone is final, and it is what a
// reconnect shows). Messages of one conversation are ordered by id, oldest
// first. Ids are only comparable inside one conversation.
import type { DMConversationV1, DMMessageV1 } from '../schema/generated';
import type { DmFailure } from './api';

export const removedBody = 'This message was removed by its author.';

export const peerName = (conversation: DMConversationV1): string => conversation.peer_name?.trim() ? conversation.peer_name : 'Former member';

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

// The nonce identifies a pending send. Retrying an unconfirmed send reuses it,
// and the author's own copy of the stored message carries it back to settle the entry.
export interface Pending {
  readonly nonce: string;
  readonly body: string;
  readonly fileIds: readonly number[];
  readonly status: 'sending' | 'failed' | 'unconfirmed';
  readonly failure?: DmFailure | 'unconfirmed';
}

export function canRetrySend(send: Pending): boolean {
  return send.status === 'unconfirmed' || (send.status === 'failed' && send.failure === 'rate_limited');
}

export function settle(pending: readonly Pending[], arrived: readonly DMMessageV1[]): readonly Pending[] {
  const nonces = new Set(arrived.flatMap((m) => (m.client_nonce === undefined ? [] : [m.client_nonce])));
  if (nonces.size === 0) return pending;
  const kept = pending.filter((p) => !nonces.has(p.nonce));
  return kept.length === pending.length ? pending : kept;
}

// Each conversation keeps its sends across transcript mounts for this sign-in.
export function pendingSends() {
  let pending: readonly Pending[] = [];
  const listeners = new Set<() => void>();
  return {
    get: () => pending,
    subscribe: (listener: () => void) => {
      listeners.add(listener);
      return () => { listeners.delete(listener); };
    },
    update: (apply: (held: readonly Pending[]) => readonly Pending[]) => {
      const next = apply(pending);
      if (next === pending) return;
      pending = next;
      for (const listener of listeners) listener();
    },
  };
}

export type PendingSends = ReturnType<typeof pendingSends>;

// What the person is told when the peer cannot be reached: one line for every
// reason, never which (ADR-017).
export const cannotMessage = "Can't send a message to this person";

export const noAccess = 'This conversation is no longer available to you.';
