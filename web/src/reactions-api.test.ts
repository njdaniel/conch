import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { reactionSummarySchema, type ReactionSummaryV1 } from './schema/reaction';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const summary: ReactionSummaryV1 = { schema: reactionSummarySchema, channel_id: 2, message_id: 4, reactions: [{ emoji: '#️⃣', count: 2, me: true }], as_of: '2026-11-06T10:00:00.123456789Z' };
let calls: { readonly path: string; readonly method: string; readonly body: BodyInit | null | undefined }[];
let reply: () => Response | Promise<Response>;
beforeEach(async () => {
  calls = [];
  reply = () => json(summary);
  vi.stubGlobal('fetch', vi.fn((path: string, init?: RequestInit) => {
    calls.push({ path, method: init?.method ?? 'GET', body: init?.body });
    if (path === '/v1/whoami') return Promise.resolve(json({ id: 7, kind: 'human', name: 'Ada', role: 'member' }));
    return Promise.resolve(reply());
  }));
  expect((await api.signIn('conch_reaction_test')).ok).toBe(true);
  calls = [];
});
afterEach(async () => {
  vi.unstubAllGlobals(); window.localStorage.clear(); window.sessionStorage.clear();
  await api.listChannels();
});
describe('reaction API', () => {
  it('uses generated response validators, encoded emoji/channel paths and no write body', async () => {
    expect(await api.writeReaction('red team', 4, '#️⃣', true)).toEqual({ ok: true, value: summary });
    expect(await api.writeReaction('red team', 4, '#️⃣', false)).toEqual({ ok: true, value: summary });
    expect(calls.map((c) => c.method)).toEqual(['PUT', 'DELETE']);
    expect(calls[0]?.path).toBe(`/v2/channels/red%20team/messages/4/reactions/${encodeURIComponent('#️⃣')}`);
    expect(calls[0]?.body).toBeUndefined();
    reply = () => json({ ...summary, reactions: 'invalid' });
    expect(await api.writeReaction('x', 4, '👍', true)).toEqual({ ok: false, failure: 'network' });
  });
  it('repeats explicit idempotent intent on retries and concurrent requests', async () => {
    await Promise.all([api.writeReaction('x', 4, '#️⃣', true), api.writeReaction('x', 4, '#️⃣', true)]);
    await api.writeReaction('x', 4, '#️⃣', true);
    expect(calls.map((c) => c.method)).toEqual(['PUT', 'PUT', 'PUT']);
  });
  it('bounds batches locally and sends only distinct positive ids', async () => {
    for (const ids of [[], [1, 1], [0], Array.from({ length: 101 }, (_, i) => i + 1)]) {
      expect(await api.readReactions('x', ids)).toEqual({ ok: false, failure: 'invalid', code: 'invalid_request' });
    }
    expect(calls).toEqual([]);
    reply = () => json({ summaries: [summary] });
    expect((await api.readReactions('x', [4, 5])).ok).toBe(true);
    expect(calls[0]?.path).toBe('/v2/channels/x/reactions?messages=4%2C5');
  });
  it('distinguishes a mux 404 from an authorized schema refusal', async () => {
    reply = () => new Response('404 page not found', { status: 404 });
    expect(await api.readReactions('x', [4])).toEqual({ ok: false, failure: 'invalid', code: 'reactions_unsupported' });
    reply = () => json({ code: 'message_not_found', message: 'message not found' }, 404);
    expect(await api.readReactions('x', [4])).toEqual({ ok: false, failure: 'invalid', code: 'message_not_found' });
  });
  it('keeps server refusal codes and the rate-limit wait', async () => {
    reply = () => json({ code: 'rate_limited', message: 'wait', retry_after_seconds: 8 }, 429);
    expect(await api.writeReaction('x', 4, '👍', true)).toEqual({ ok: false, failure: 'invalid', code: 'rate_limited', retryAfter: 8 });
    reply = () => { throw new TypeError('offline'); };
    expect(await api.writeReaction('x', 4, '👍', true)).toEqual({ ok: false, failure: 'network' });
  });
  it('paginates identities using the server cursor and generated list shape', async () => {
    reply = () => json({ reactors: [], next_after: 9 });
    expect(await api.listReactors('x', 4, '👍', 7, 20)).toEqual({ ok: true, value: { reactors: [], next_after: 9 } });
    expect(calls[0]?.path).toBe(`/v2/channels/x/messages/4/reactions/${encodeURIComponent('👍')}?after=7&limit=20`);
  });
  it('decodes a server response that carries a key this bundle does not know (issue #1534)', async () => {
    reply = () => json({ reactors: [], next_after: 9, added_later: true });
    expect(await api.listReactors('x', 4, '👍', 7, 20)).toEqual({ ok: true, value: { reactors: [], next_after: 9 } });
  });
  it('signs out on a revoked reaction credential', async () => {
    reply = () => json({ code: 'unauthorized', message: 'revoked' }, 401);
    expect(await api.writeReaction('x', 4, '👍', true)).toEqual({ ok: false, failure: 'unauthorized' });
    expect(api.hasToken()).toBe(false);
  });
});

it.each([
  { schema: 'conch.session_roster.v1', channel_id: 2, sessions: [] },
  { schema: 'conch.channel.v1', channel_id: 2, name: 'renamed', created_at: summary.as_of },
  summary,
])('learns channel identity from %j and scopes socket events to the live credential', async (identity) => {
  class Socket {
    onopen: (() => void) | null = null;
    onmessage: ((event: MessageEvent) => void) | null = null;
    onclose: ((event: CloseEvent) => void) | null = null;
    close() { /* The transport owns closing; no real sockets in unit tests. */ }
  }
  const socket = new Socket();
  vi.stubGlobal('WebSocket', vi.fn(function () { return socket; }));
  reply = () => json({ ticket: 'conch_wst_reaction_test', expires_at: '2026-11-06T10:01:00Z' });
  const receive = vi.fn(); const other = vi.fn();
  const stop = api.watchReactions('renamed', 2, receive);
  const stopOther = api.watchReactions('other', 3, other);
  const events = { onOpen: vi.fn(), onFrame: vi.fn(), onClose: vi.fn() };
  const opened = await api.openEventsSocket('old-name', 8, events);
  expect(opened.ok).toBe(true);
  socket.onopen?.();
  for (const body of ['not JSON', JSON.stringify({ schema: 'conch.future.v1' }), JSON.stringify({ ...summary, reactions: null })]) socket.onmessage?.(new MessageEvent('message', { data: body }));
  expect(receive).not.toHaveBeenCalled();
  socket.onmessage?.(new MessageEvent('message', { data: JSON.stringify(identity) }));
  socket.onclose?.(new CloseEvent('close', { code: 1008 }));
  expect(receive).toHaveBeenLastCalledWith({ kind: 'closed' });
  socket.onmessage?.(new MessageEvent('message', { data: JSON.stringify(summary) }));
  expect(receive).toHaveBeenLastCalledWith({ kind: 'summary', summary });
  expect(other).not.toHaveBeenCalled();
  socket.onmessage?.(new MessageEvent('message', { data: JSON.stringify(identity) }));
  socket.onclose?.(new CloseEvent('close', { code: 1008 }));
  expect(receive).toHaveBeenLastCalledWith({ kind: 'closed' });
  // A fresh sign-in invalidates callbacks of the earlier credential/session,
  // even when its principal id and server are the same.
  await api.signOut();
  expect((await api.signIn('conch_new_reaction_test')).ok).toBe(true);
  receive.mockClear();
  socket.onmessage?.(new MessageEvent('message', { data: JSON.stringify(summary) }));
  expect(receive).not.toHaveBeenCalled();
  stop(); stopOther(); if (opened.ok) opened.value.close();
});
