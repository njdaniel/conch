import { describe, expect, it } from 'vitest';
import { conversationKey, messageKey, scopeKey } from './keys';
import { merge, ordered, removedBody, settle, supersedes } from './model';
import { ownMessage, peerMessage, removedMessage } from './testkit';

describe('keys', () => {
  it('never lets channel message 42 and DM message 42 meet', () => {
    expect(messageKey('channel', 7, 42)).not.toBe(messageKey('dm', 7, 42));
    expect(messageKey('dm', 7, 42)).not.toBe(messageKey('dm', 42, 7));
    expect(conversationKey('dm', 12)).not.toBe(conversationKey('channel', 12));
  });
  it('keeps two servers with the same account id apart', () => {
    expect(scopeKey('https://a.example', 3)).not.toBe(scopeKey('https://b.example', 3));
  });
});

describe('merge', () => {
  it('lets the higher revision of one message win and a lower one never replace it', () => {
    const first = peerMessage();
    const edited = { ...first, revision: 3, body: 'edited', edited_at: '2026-11-19T09:02:00.000Z' };
    const held = merge(new Map(), [first]);
    const next = merge(held, [edited]);
    expect(next.get(first.id)?.body).toBe('edited');
    expect(merge(next, [first])).toBe(next);
  });
  it('keeps the tombstone: a removed copy is never replaced by one that is not', () => {
    const gone = removedMessage();
    const held = merge(new Map(), [gone]);
    expect(supersedes(gone, { ...gone, removed: false, revision: gone.revision + 5, body: 'back' })).toBe(false);
    expect(merge(held, [{ ...gone, removed: false, revision: 99, body: 'back' }]).get(gone.id)?.body).toBe(removedBody);
  });
  it('applies a removal that has the same revision as the send it follows', () => {
    const sent = peerMessage();
    const held = merge(new Map(), [sent]);
    expect(merge(held, [{ ...sent, removed: true, body: removedBody }]).get(sent.id)?.removed).toBe(true);
  });
  it('orders by id, oldest first, and does not duplicate a live and a historical copy', () => {
    const a = peerMessage();
    const b = ownMessage();
    const held = merge(merge(new Map(), [b, a]), [a, b]);
    expect(ordered(held).map((m) => m.id)).toEqual([a.id, b.id]);
  });
});

describe('settle', () => {
  it('ends a pending send when the author’s copy comes back with its nonce', () => {
    const mine = ownMessage();
    const pending = [{ nonce: mine.client_nonce ?? '', body: mine.body, fileIds: [], status: 'unconfirmed' as const }];
    expect(settle(pending, [mine])).toEqual([]);
    expect(settle(pending, [peerMessage()])).toBe(pending);
  });
});
