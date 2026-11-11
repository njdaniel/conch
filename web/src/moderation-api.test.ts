import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import * as api from './api';
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
let reply: () => Response;
let calls: { readonly path: string; readonly method: string; readonly body: BodyInit | null | undefined }[];
beforeEach(async () => {
  calls = [];
  reply = () => new Response(null, { status: 204 });
  vi.stubGlobal('fetch', vi.fn((path: string, init?: RequestInit) => {
    calls.push({ path, method: init?.method ?? 'GET', body: init?.body });
    if (path === '/v1/whoami') return Promise.resolve(json({ id: 1, kind: 'human', name: 'Manager', role: 'member' }));
    return Promise.resolve(reply());
  }));
  expect((await api.signIn('conch_moderation_test')).ok).toBe(true);
  calls = [];
});
afterEach(async () => { vi.unstubAllGlobals(); localStorage.clear(); sessionStorage.clear(); await api.listChannels(); });
it('uses the exact encoded REST routes with explicit methods and no invented request bodies', async () => {
  for (const op of ['kickVoice', 'banMember', 'unbanMember', 'removeMessage'] as const) expect(await api.moderate('red team', op, 9)).toEqual({ ok: true, value: null });
  expect(await api.removeChannelMember('red team', 9)).toEqual({ ok: true, value: null });
  expect(calls).toEqual([
    { path: '/v1/channels/red%20team/voice/participants/9', method: 'DELETE', body: undefined },
    { path: '/v1/channels/red%20team/bans/9', method: 'PUT', body: undefined },
    { path: '/v1/channels/red%20team/bans/9', method: 'DELETE', body: undefined },
    { path: '/v2/channels/red%20team/messages/9', method: 'DELETE', body: undefined },
    { path: '/v1/channels/red%20team/members/9', method: 'DELETE', body: undefined },
  ]);
  reply = () => json({ bans: [] });
  expect(await api.listChannelBans('red team')).toEqual({ ok: true, value: { bans: [] } });
  expect(calls.at(-1)).toMatchObject({ path: '/v1/channels/red%20team/bans', method: 'GET' });
  reply = () => json({ bans: [{ principal_id: 'bad' }] });
  expect((await api.listChannelBans('ops')).ok).toBe(false);
});
it.each([
  [503, 'voice_unavailable', 'voice is temporarily unavailable; retry'],
  [403, 'forbidden', 'manager target refused'],
  [404, 'message_not_found', 'message not found'],
])('keeps %i refusal details and never retries writes automatically', async (status, code, message) => {
  reply = () => json({ code, message }, status);
  expect(await api.moderate('ops', 'kickVoice', 9)).toEqual({ ok: false, failure: 'invalid', code, message });
  expect(calls).toHaveLength(1);
});
it('publishes only validated safe removal updates from sockets of the current session', async () => {
  class Socket {
    onopen: (() => void) | null = null;
    onmessage: ((event: MessageEvent) => void) | null = null;
    onclose: ((event: CloseEvent) => void) | null = null;
    close() { /* Unit test transport. */ }
  }
  const socket = new Socket();
  vi.stubGlobal('WebSocket', vi.fn(function () { return socket; }));
  reply = () => json({ ticket: 'conch_wst_moderation_test', expires_at: '2026-11-06T10:01:00Z' });
  const receive = vi.fn();
  const stop = api.watchRemovals(receive);
  const opened = await api.openEventsSocket('ops', 1, { onOpen: vi.fn(), onFrame: vi.fn(), onClose: vi.fn() });
  const message = { schema: 'conch.message.v2', id: 8, channel_id: 2, author_id: 3, created_at: '2026-11-06T10:00:00Z', removed: true, body: 'This message was removed by a moderator.' };
  for (const data of ['bad', JSON.stringify({ schema: 'conch.message_removed.v1', message: null }), JSON.stringify({ ...message, removed: false, body: 'original' })]) socket.onmessage?.(new MessageEvent('message', { data }));
  expect(receive).not.toHaveBeenCalled();
  socket.onmessage?.(new MessageEvent('message', { data: JSON.stringify({ schema: 'conch.message_removed.v1', message }) }));
  expect(receive).toHaveBeenCalledExactlyOnceWith(message);
  receive.mockClear();
  await api.signOut();
  socket.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) }));
  expect(receive).not.toHaveBeenCalled();
  stop(); if (opened.ok) opened.value.close();
});
