import { describe, expect, it } from 'vitest';
import type { MessageV2 } from '../schema/message';
import type { DraftAttachment } from './attachments';
import { chatFor, chatReducer, initialChat, lastId, ordered, maxHeld, pageSize, canLoadOlder, oldestId, parseFrame, type ChatAction, type ChatState } from './messages';

const msg = (id: number, body = `m${String(id)}`): MessageV2 => ({
  schema: 'conch.message.v2',
  id,
  channel_id: 7,
  author_id: 3,
  created_at: '2026-10-14T10:00:00Z',
  body,
});

const ready = (clientId: number, fileId: number): DraftAttachment => ({
  clientId,
  filename: `${String(clientId)}.png`,
  mediaType: 'image/png',
  size: 3,
  bytes: new Blob(['img']),
  status: 'ready',
  attempt: 1,
  fileId,
});

const withFiles = (message: MessageV2, fileIds: number[]): MessageV2 => ({
  ...message,
  payload: { schema: 'conch.file_attachment.v1', data: { file_ids: fileIds } },
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
    let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 });
    expect(chatFor(state, 'ops').pending).toEqual([{ localId: 1, channelId: 7, authorId: 3, body: 'hi', at: 1, status: 'sending', phase: 'posting', attachments: [], reconcileAfter: 0 }]);
    // The live frame wins the race with the POST's answer, and stands in
    // for the pending send at once: the message never shows twice (#490).
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi'), own: true });
    expect(ids(state)).toEqual([8]);
    expect(chatFor(state, 'ops').pending).toEqual([]);
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(8, 'hi') });
    expect(ids(state)).toEqual([8]);
    expect(chatFor(state, 'ops').pending).toEqual([]);
    expect(chatFor(state, 'ops').sentIds.has(8)).toBe(true);
  });

  describe("a live frame of one's own before the POST's answer (#490)", () => {
    const net = { kind: 'net', net_id: 4 } as const;
    const whisper = (ids: number[]) => ({ kind: 'principals', principal_ids: ids }) as const;
    const withAudience = (m: MessageV2, audience: MessageV2['audience']): MessageV2 => ({ ...m, audience });

    it('takes the place of the oldest of two identical sends, and the later answers change nothing', () => {
      let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 }, { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 2 });
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi'), own: true });
      expect(ids(state)).toEqual([8]);
      expect(chatFor(state, 'ops').pending.map((p) => p.localId)).toEqual([2]);
      state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(8, 'hi') });
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(9, 'hi'), own: true });
      state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 2, message: msg(9, 'hi') });
      expect(ids(state)).toEqual([8, 9]);
      expect(chatFor(state, 'ops').pending).toEqual([]);
    });

    it("never lets someone else's message with the same body stand in", () => {
      let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 });
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi') });
      expect(chatFor(state, 'ops').pending).toHaveLength(1);
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi'), own: false });
      expect(chatFor(state, 'ops').pending).toHaveLength(1);
    });

    it('requires the frozen author in live and history reconciliation despite an own flag', () => {
      const collision = { ...msg(8, 'hi'), author_id: 4 };
      let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 });
      state = chatReducer(state, { type: 'received', channel: 'ops', message: collision, own: true });
      expect(chatFor(state, 'ops').pending).toHaveLength(1);
      state = chatReducer(state, { type: 'historyLoaded', channel: 'ops', messages: [collision], more: false });
      expect(chatFor(state, 'ops').pending).toHaveLength(1);
      state = chatReducer(state, { type: 'historyLoaded', channel: 'ops', messages: [msg(9, 'hi')], more: false });
      expect(chatFor(state, 'ops').pending).toEqual([]);
    });

    it('matches the audience: the whole channel, the same net, the same people in any order', () => {
      let state = run(
        { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1, audience: net },
        { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 2, audience: whisper([5, 3]) },
      );
      // The same body to the whole channel matches neither.
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(7, 'hi'), own: true });
      expect(chatFor(state, 'ops').pending).toHaveLength(2);
      state = chatReducer(state, { type: 'received', channel: 'ops', message: withAudience(msg(8, 'hi'), whisper([3, 5])), own: true });
      expect(chatFor(state, 'ops').pending.map((p) => p.localId)).toEqual([1]);
      state = chatReducer(state, { type: 'received', channel: 'ops', message: withAudience(msg(9, 'hi'), net), own: true });
      expect(chatFor(state, 'ops').pending).toEqual([]);
    });

    it('matches the thread root as well as the body and audience', () => {
      let state = run(
        { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1, threadRootId: 10 },
        { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 2, threadRootId: 11 },
      );
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(7, 'hi'), own: true });
      expect(chatFor(state, 'ops').pending).toHaveLength(2);
      state = chatReducer(state, { type: 'received', channel: 'ops', message: { ...msg(8, 'hi'), thread_root_id: 11 }, own: true });
      expect(chatFor(state, 'ops').pending.map((pending) => [pending.localId, pending.threadRootId])).toEqual([[1, 10]]);
      state = chatReducer(state, { type: 'received', channel: 'ops', message: { ...msg(9, 'hi'), thread_root_id: 10 }, own: true });
      expect(chatFor(state, 'ops').pending).toEqual([]);
    });

    it('reconciles a reply whose delivered audience is narrower than the root audience it held', () => {
      let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'psst', at: 1, threadRootId: 10, audience: { kind: 'principals', principal_ids: [4, 5] } });
      state = chatReducer(state, { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'unconfirmed' });
      const delivered: MessageV2 = { ...msg(8, 'psst'), thread_root_id: 10, audience: { kind: 'principals', principal_ids: [4] } };
      state = chatReducer(state, { type: 'received', channel: 'ops', message: delivered, own: true });
      expect(chatFor(state, 'ops').pending).toEqual([]);
      expect([...chatFor(state, 'ops').byId.keys()]).toContain(8);
    });

    it('leaves a failed send in place: it was not delivered, whatever arrives', () => {
      let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 }, { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'failed' });
      state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi'), own: true });
      expect(chatFor(state, 'ops').pending).toHaveLength(1);
    });
  });

  it('replaces the local copy when the answer comes before the frame', () => {
    let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 });
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(8, 'hi') });
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'hi') });
    expect(ids(state)).toEqual([8]);
    expect(chatFor(state, 'ops').pending).toEqual([]);
  });

  const outcomes: ('failed' | 'unconfirmed')[] = ['failed', 'unconfirmed'];
  it.each(outcomes)('keeps a %s send in place, and sends again only on a retry', (outcome) => {
    let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 }, { type: 'sendFailed', channel: 'ops', localId: 1, outcome });
    expect(chatFor(state, 'ops').pending[0]?.status).toBe(outcome);
    expect(ids(state)).toEqual([]);
    state = chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 });
    expect(chatFor(state, 'ops').pending).toEqual([{ localId: 1, channelId: 7, authorId: 3, body: 'hi', at: 1, status: 'sending', phase: 'posting', attachments: [], reconcileAfter: 0 }]);
  });

  it('does not restart a send that is already going', () => {
    const state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 });
    expect(chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 })).toEqual(state);
  });

  it('drops a dismissed send, and an answer for a send it no longer has', () => {
    let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 }, { type: 'discard', channel: 'ops', localId: 1 });
    expect(chatFor(state, 'ops').pending).toEqual([]);
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(4) });
    expect(ids(state)).toEqual([]);
  });

  it('ignores an answer after a reset', () => {
    const state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 }, { type: 'reset' }, { type: 'sent', channel: 'ops', localId: 1, message: msg(4) });
    expect(state).toEqual(initialChat);
  });

  it('numbers local sends apart', () => {
    const state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'a', at: 1 }, { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'b', at: 2 });
    expect(chatFor(state, 'ops').pending.map((p) => p.localId)).toEqual([1, 2]);
  });

  it('merges an older page by id and ends when the server has no more', () => {
    const state = run({ type: 'historyLoaded', channel: 'ops', messages: [msg(5), msg(6)], more: true }, { type: 'olderLoaded', channel: 'ops', messages: [msg(3), msg(4), msg(5)], more: false });
    expect(ids(state)).toEqual([3, 4, 5, 6]);
    expect(oldestId(chatFor(state, 'ops'))).toBe(3);
    expect(canLoadOlder(chatFor(state, 'ops'))).toBe(false);
  });

  it('offers another older page exactly while it fits within maxHeld', () => {
    const holding = (n: number) =>
      chatFor(run({ type: 'historyLoaded', channel: 'ops', messages: Array.from({ length: n }, (_, i) => msg(i + 1)), more: true }), 'ops');
    expect(canLoadOlder(holding(maxHeld - pageSize))).toBe(true);
    expect(canLoadOlder(holding(maxHeld - pageSize + 1))).toBe(false);
    expect(canLoadOlder(holding(maxHeld))).toBe(false);
  });

  it('counts a dropped frame', () => {
    expect(chatFor(run({ type: 'dropped', channel: 'ops' }, { type: 'dropped', channel: 'ops' }), 'ops').dropped).toBe(2);
  });
});

describe('attachment sends and reconciliation', () => {
  it('freezes local attachments and their uploaded ids in composer order', () => {
    const attachments = [ready(1, 90), ready(2, 42)];
    const state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'caption', at: 1, attachments, requestId: 'send-a' });
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({
      body: 'caption',
      status: 'sending',
      phase: 'posting',
      attachments,
      payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [90, 42] } },
      reconcileAfter: 0,
      requestId: 'send-a',
    });
    expect(chatFor(state, 'ops').pending[0]?.attachments[0]?.bytes).toBe(attachments[0]?.bytes);
  });

  it.each(['failed', 'unconfirmed'] as const)('retains ready uploads and their payload after a %s post and retry', (outcome) => {
    let state = run(
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: '', at: 1, attachments: [ready(1, 90), ready(2, 42)] },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome },
    );
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({
      status: outcome,
      phase: outcome,
      attachments: [{ fileId: 90 }, { fileId: 42 }],
      payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [90, 42] } },
    });
    state = chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 });
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({
      status: 'sending',
      phase: 'posting',
      payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [90, 42] } },
    });
  });

  it('matches the whole payload as well as body and audience, including file order', () => {
    let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'files', at: 1, attachments: [ready(1, 90), ready(2, 42)] });
    state = chatReducer(state, { type: 'received', channel: 'ops', message: withFiles(msg(8, 'files'), [90]), own: true });
    expect(chatFor(state, 'ops').pending).toHaveLength(1);
    state = chatReducer(state, { type: 'received', channel: 'ops', message: withFiles(msg(9, 'files'), [42, 90]), own: true });
    expect(chatFor(state, 'ops').pending).toHaveLength(1);
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(10, 'files'), own: true });
    expect(chatFor(state, 'ops').pending).toHaveLength(1);
    state = chatReducer(state, { type: 'received', channel: 'ops', message: withFiles(msg(11, 'files'), [90, 42]), own: true });
    expect(chatFor(state, 'ops').pending).toEqual([]);
    expect(ids(state)).toEqual([8, 9, 10, 11]);
  });

  it('does not reconcile against an identical message at or before the send baseline', () => {
    let state = run(
      { type: 'historyLoaded', channel: 'ops', messages: [msg(4, 'hi')], more: false },
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'unconfirmed' },
    );
    expect(chatFor(state, 'ops').pending[0]?.reconcileAfter).toBe(4);
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(4, 'hi'), own: true });
    expect(chatFor(state, 'ops').pending).toHaveLength(1);
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(5, 'hi'), own: true });
    expect(chatFor(state, 'ops').pending).toEqual([]);
    expect(chatFor(state, 'ops').sentIds.has(5)).toBe(true);
  });

  it('settles an unconfirmed send from a later history page while reconciling', () => {
    let state = run(
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'unconfirmed' },
      { type: 'reconciling', channel: 'ops', localId: 1 },
    );
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({ status: 'sending', phase: 'reconciling' });
    state = chatReducer(state, { type: 'historyLoaded', channel: 'ops', messages: [msg(8, 'hi')], more: false });
    expect(chatFor(state, 'ops').pending).toEqual([]);
    expect(ids(state)).toEqual([8]);
    expect(chatFor(state, 'ops').sentIds.has(8)).toBe(true);
  });

  it('returns to unconfirmed when reconciliation cannot read history', () => {
    let state = run(
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1 },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'unconfirmed' },
      { type: 'reconciling', channel: 'ops', localId: 1 },
      { type: 'reconciliationFailed', channel: 'ops', localId: 1 },
    );
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({ status: 'unconfirmed', phase: 'unconfirmed' });
    state = chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 });
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({ status: 'sending', phase: 'posting', reconcileAfter: 0 });
  });

  it('reconciles identical unconfirmed sends one server message at a time', () => {
    const state = run(
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'same', at: 1 },
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'same', at: 2 },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'unconfirmed' },
      { type: 'sendFailed', channel: 'ops', localId: 2, outcome: 'unconfirmed' },
      { type: 'historyLoaded', channel: 'ops', messages: [msg(8, 'same'), msg(9, 'same')], more: false },
    );
    expect(chatFor(state, 'ops').pending).toEqual([]);
    expect([...chatFor(state, 'ops').sentIds].sort((a, b) => a - b)).toEqual([8, 9]);
  });

  it('never lets concurrent identical sends consume one server id across repeated actions', () => {
    let state = run(
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'same', at: 1 },
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'same', at: 2 },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'unconfirmed' },
      { type: 'sendFailed', channel: 'ops', localId: 2, outcome: 'unconfirmed' },
      { type: 'historyLoaded', channel: 'ops', messages: [msg(8, 'same')], more: false },
    );
    expect(chatFor(state, 'ops').pending.map((p) => p.localId)).toEqual([2]);
    state = chatReducer(state, { type: 'historyLoaded', channel: 'ops', messages: [msg(8, 'same')], more: false });
    state = chatReducer(state, { type: 'received', channel: 'ops', message: msg(8, 'same'), own: true });
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 2, message: msg(8, 'same') });
    expect(chatFor(state, 'ops').pending.map((p) => p.localId)).toEqual([2]);
    expect([...chatFor(state, 'ops').sentIds]).toEqual([8]);
  });

  it('rejects late answers from a reset send whose local id has been reused', () => {
    let state = run(
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'old', at: 1, requestId: 'old-request' },
      { type: 'reset' },
      { type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'new', at: 2, requestId: 'new-request' },
      { type: 'sent', channel: 'ops', localId: 1, message: msg(8, 'old'), requestId: 'old-request' },
      { type: 'sendFailed', channel: 'ops', localId: 1, outcome: 'failed', requestId: 'old-request' },
    );
    expect(ids(state)).toEqual([]);
    expect(chatFor(state, 'ops').pending[0]).toMatchObject({ body: 'new', status: 'sending', requestId: 'new-request' });
    state = chatReducer(state, { type: 'sent', channel: 'ops', localId: 1, message: msg(9, 'new'), requestId: 'new-request' });
    expect(ids(state)).toEqual([9]);
    expect(chatFor(state, 'ops').pending).toEqual([]);
  });

  it('does not retry or discard a keyed send without its request id', () => {
    const state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'hi', at: 1, requestId: 'mine' });
    expect(chatReducer(state, { type: 'retrying', channel: 'ops', localId: 1 })).toEqual(state);
    expect(chatReducer(state, { type: 'discard', channel: 'ops', localId: 1 })).toEqual(state);
  });
});

describe('the held messages are bounded', () => {
  it('keeps the newest maxHeld, says it dropped some, and never touches a pending send', () => {
    const many = Array.from({ length: maxHeld + 5 }, (_, i) => msg(i + 1));
    let state = run({ type: 'sending', channelId: 7, authorId: 3, channel: 'ops', body: 'mine', at: 1 }, { type: 'historyLoaded', channel: 'ops', messages: many, more: false });
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
