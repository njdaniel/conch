// Keys for anything a client holds per message or per conversation (caches,
// drafts, read marks, routes). A channel and a DM live in id spaces of their
// own (pkg/schema/dm.go): channel message 42 and DM message 42 are two
// messages, and a number alone names neither. So every key is the triple
// (kind, conversation, id), and a cache is cleared by a scope, the pair of an
// origin and an account, because two servers hand out the same small ids.

export type ThreadKind = 'channel' | 'dm';

export interface MessageRef {
  readonly kind: ThreadKind;
  readonly conversation: number;
  readonly id: number;
}

export const conversationKey = (kind: ThreadKind, conversation: number): string => `${kind}:${String(conversation)}`;

export const messageKey = (kind: ThreadKind, conversation: number, id: number): string => `${kind}:${String(conversation)}:${String(id)}`;

export const refKey = (ref: MessageRef): string => messageKey(ref.kind, ref.conversation, ref.id);

// scopeKey names whose ids a cache holds. A cache that sees another scope
// drops everything it has: nothing carries across an account or a server.
export const scopeKey = (origin: string, account: number): string => `${origin}|${String(account)}`;
