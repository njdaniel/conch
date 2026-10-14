import { describe, expect, it } from 'vitest';
import type { MessageV2 } from '../schema/message';
import { chatFor, chatReducer, initialChat, lastId, ordered, maxHeld, canLoadOlder, oldestId, parseFrame, type ChatAction, type ChatState } from './messages';

const msg = (id: number, body = `m${String(id)}`): MessageV2 => ({
  schema: 'conch.message.v2',
  id,
  channel_id: 7,
  author_id: 3,
  created_at: '2026-10-14T10:00:00Z',
  body,
});

function run(...actions: ChatAction[]): ChatState {
  return actions.reduce(chatReducer, initialChat);
}
const ids = (state: ChatState) => ordered(chatFor(state, 'ops')).map((m) => m.id);

describe('the message store', () => {
  it('keeps messages in id order whatever order they arrive in', () => {
    const state = run(
      { type: 'historyLoaded', channel: 'ops', messages: [msg(5), msg(2)], more: false },
      { type: 'received', channel: 'ops', message: msg(9) },
      { type: 'received', channel: 'ops', message: msg(3) },
    );
    expect(ids(state)).toEqual([2, 3, 5, 9]);
    expect(lastId(chatFor(state, 'ops'))).toBe(9);
  });

  it('shows a message that arrives twice once: in history and again on the socket', () => {
    const state = run(
      { type: 'historyLoaded', channel: 'ops', messages: [msg(1), msg(2)], more: false },
      { type: 'received', channel: 'ops', message: msg(2) },
      { type: 'received', channel: 'ops', message: msg(2) },
    );
    expect(ids(state)).toEqual([1, 2]);
  });

  it('keeps channels apart', () => {
    const state = run({ type: 'received', channel: 'ops', message: msg(1) });
    expect(ordered(chatFor(state, 'dev'))).toEqual([]);
    expect(chatFor(state, null).byId.size).toBe(0);
  });

  it('shows a message as Sending, then Sent in place of the server copy, matched by id', () => {
    let state = run({ type: 'sending', channel: 'ops', body: 'hi', at: 1 });
    expect(chatFor(state, 'ops').pending).toEqual([{ localId: 1, body: 'hi', at: 1, status: 'sending' }]);
    // The live frame wins the race with the POST's answer.
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi') });
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(8, 'hi') });
    expect(ids(state)).toEqual([8]);
    expect(chatFor(state, 'ops').pending).toEqual([]);
    expect(chatFor(state, 'ops').sentIds.has(8)).toBe(true);
  });

  it('replaces the local copy when the answer comes before the frame', () => {
    let state = run({ type: 'sending', channel: 'ops', body: 'hi', at: 1 });
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(8, 'hi') });
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi') });
    expect(ids(state)).toEqual([8]);
    expect(chatFor(state, 'ops').pending).toEqual([]);
  });

  const outcomes: ('failed' | 'unconfirmed')[] = ['failed', 'unconfirmed'];
  it.each(outcomes)('keeps a %s send in place, and sends again only on a retry', (outcome) => {
    let state = run({ type: 'sending', channel: 'ops', body: 'hi', at: 1 }, { type: 'sendFailed', channel: 'ops', localId: 1, outcome });
    expect(chatFor(state, 'ops').pending[0]?.status).toBe(outcome);
    expect(ids(state)).toEqual([]);
    state = chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 });
    expect(chatFor(state, 'ops').pending).toEqual([{ localId: 1, body: 'hi', at: 1, status: 'sending' }]);
  });

  it('does not restart a send that is already going', () => {
    const state = run({ type: 'sending', channel: 'ops', body: 'hi', at: 1 });
    expect(chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 })).toEqual(state);
  });

  it('drops a dismissed send, and an answer for a send it no longer has', () => {
    let state = run({ type: 'sending', channel: 'ops', body: 'hi', at: 1 }, { type: 'discard', channel: 'ops', localId: 1 });
    expect(chatFor(state, 'ops').pending).toEqual([]);
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(4) });
    expect(ids(state)).toEqual([]);
  });

  it('ignores an answer after a reset', () => {
    const state = run({ type: 'sending', channel: 'ops', body: 'hi', at: 1 }, { type: 'reset' }, { type: 'sent', channel: 'ops', localId: 1, message: msg(4) });
    expect(state).toEqual(initialChat);
  });

  it('numbers local sends apart', () => {
    const state = run({ type: 'sending', channel: 'ops', body: 'a', at: 1 }, { type: 'sending', channel: 'ops', body: 'b', at: 2 });
    expect(chatFor(state, 'ops').pending.map((p) => p.localId)).toEqual([1, 2]);
  });

  it('merges an older page by id and ends when the server has no more', () => {
    const state = run({ type: 'historyLoaded', channel: 'ops', messages: [msg(5), msg(6)], more: true }, { type: 'olderLoaded', channel: 'ops', messages: [msg(3), msg(4), msg(5)], more: false });
    expect(ids(state)).toEqual([3, 4, 5, 6]);
    expect(oldestId(chatFor(state, 'ops'))).toBe(3);
    expect(canLoadOlder(chatFor(state, 'ops'))).toBe(false);
  });

  it('counts a dropped frame', () => {
    expect(chatFor(run({ type: 'dropped', channel: 'ops' }, { type: 'dropped', channel: 'ops' }), 'ops').dropped).toBe(2);
  });
});

describe('the held messages are bounded', () => {
  it('keeps the newest maxHeld, says it dropped some, and never touches a pending send', () => {
    const many = Array.from({ length: maxHeld + 5 }, (_, i) => msg(i + 1));
    let state = run({ type: 'sending', channel: 'ops', body: 'mine', at: 1 }, { type: 'historyLoaded', channel: 'ops', messages: many, more: false });
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(maxHeld + 6) });
    const held = ids(state);
    expect(held).toHaveLength(maxHeld);
    expect(held[0]).toBe(7);
    expect(held.at(-1)).toBe(maxHeld + 6);
    expect(chatFor(state, 'ops').trimmed).toBe(true);
    expect(chatFor(state, 'ops').pending).toHaveLength(1);
  });
  it('does not say so when nothing was dropped', () => {
    expect(chatFor(run({ type: 'received', channel: 'ops', message: msg(1) }), 'ops').trimmed).toBe(false);
  });
});

describe('parseFrame', () => {
  it('reads a message of this channel', () => {
    expect(parseFrame(JSON.stringify(msg(1)), 7)).toEqual(msg(1));
  });
  const bad: [string, unknown][] = [
    ['text that is not JSON', 'not json'],
    ['a binary frame', new ArrayBuffer(2)],
    ['another schema', JSON.stringify({ ...msg(1), schema: 'conch.message.v1' })],
    ['a missing id', JSON.stringify({ ...msg(1), id: undefined })],
    ['an extra key', JSON.stringify({ ...msg(1), extra: 1 })],
    ['another channel', JSON.stringify({ ...msg(1), channel_id: 8 })],
  ];
  it.each(bad)('refuses %s', (_name, data) => {
    expect(parseFrame(data, 7)).toBeNull();
  });
});
