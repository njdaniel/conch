// The diagnostic ring (issue #446): bounded, in memory, and never holding a
// token, a ticket, an invite code, a room name, a channel name or message text.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { forget, recent, record, ringSize, voiceState } from './diagnostics';
import { createProblemReportRequestV1 } from './schema/report';
import { VoiceClient, type Room } from './voice/connection';
import { FakeClock, settle } from './voice/fakes';

const token = 'conch_member_secret_AAAAAAAAAAAA';
const ticket = 'conch_wst_TICKETTICKETTICKET';
const inviteCode = 'conch_inv_INVITEINVITEINVITEINVITE';
const channelName = 'kestrel-ops';
const roomName = 'conch-room-falcon';
const messageText = 'the falcon flies at midnight';
const forbidden = [token, ticket, inviteCode, channelName, roomName, messageText, 'conch_'];

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });

beforeEach(() => {
  forget();
});

afterEach(async () => {
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
  window.localStorage.clear();
  await api.listChannels();
  forget();
});

// timeless is the ring without its times, which the tests do not fix.
function timeless(): Record<string, unknown>[] {
  return recent().map((e) => Object.fromEntries(Object.entries(e).filter(([k]) => k !== 'at')));
}

function expectClean() {
  const ring = JSON.stringify(recent());
  for (const f of forbidden) expect(ring, f).not.toContain(f);
  // Every event is one the server takes.
  const check = createProblemReportRequestV1({
    did: 'd',
    expected: 'e',
    happened: 'h',
    often: 'once',
    client: { name: 'conch-web', version: 'v', platform: 'p' },
    diagnostics: recent(),
  });
  expect(check).toEqual([]);
}

describe('the ring', () => {
  it('keeps the last ringSize events, oldest first', () => {
    for (let i = 1; i <= ringSize + 25; i++) record({ kind: 'reconnect', socket: 'messages', attempt: i }, Date.UTC(2026, 9, 16, 9, 0, 0, i));
    const ring = recent();
    expect(ring).toHaveLength(ringSize);
    expect(ring[0]?.attempt).toBe(26);
    expect(ring.at(-1)?.attempt).toBe(ringSize + 25);
    expect(ring.at(-1)?.at).toBe('2026-10-16T09:00:00.125Z');
  });

  it('hands out a copy, not itself', () => {
    record({ kind: 'socket_open', socket: 'voice' });
    const first = recent();
    first.length = 0;
    expect(recent()).toHaveLength(1);
  });

  it('remembers the last voice state', () => {
    expect(voiceState()).toBeNull();
    record({ kind: 'voice_state', state: 'connecting' });
    record({ kind: 'voice_state', state: 'connected' });
    expect(voiceState()).toBe('connected');
  });

  // One case per kind of field: a value of the wrong shape (here, each kind of
  // forbidden text) is never kept in it.
  it.each([
    { field: 'a socket named by its address', r: { kind: 'socket_open', socket: `/v2/ws?channel=${channelName}&ticket=${ticket}` } },
    { field: 'a close code that is text', r: { kind: 'socket_close', socket: 'messages', code: Number(messageText) } },
    { field: 'an error code that is a token', r: { kind: 'http_failure', status: 401, error: token } },
    { field: 'an error code that is message text', r: { kind: 'http_failure', status: 400, error: messageText } },
    { field: 'a request id that is a ticket', r: { kind: 'http_failure', status: 404, requestId: ticket } },
    { field: 'a request id that is an invite code', r: { kind: 'http_failure', status: 404, requestId: inviteCode } },
    { field: 'a voice state that is a room name', r: { kind: 'voice_state', state: roomName } },
    { field: 'a device error that is a channel name', r: { kind: 'device_error', name: channelName } },
    { field: 'a device error that is a sentence', r: { kind: 'device_error', name: `Refused in ${channelName} Error` } },
    { field: 'an attempt that is not a count', r: { kind: 'reconnect', socket: 'voice', attempt: -1 } },
  ] as const)('keeps nothing of $field', ({ r }) => {
    record(r as Parameters<typeof record>[0]);
    expectClean();
  });

  it('keeps what is well formed', () => {
    record({ kind: 'http_failure', status: 503, error: 'voice_unavailable', requestId: '0123456789abcdef' });
    record({ kind: 'device_error', name: 'NotAllowedError' });
    expect(timeless()).toEqual([
      { kind: 'http_failure', code: 503, error: 'voice_unavailable', request_id: '0123456789abcdef' },
      { kind: 'device_error', name: 'NotAllowedError' },
    ]);
    expectClean();
  });
});

// A whole session through the real api.ts, against a stand-in conchd whose
// every answer and every path is full of things the ring must not keep.
describe('what api.ts records', () => {
  async function signIn() {
    window.localStorage.setItem('conch.web.token', token);
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member' }))));
    expect((await api.restore()).ok).toBe(true);
  }

  it('records failures by status, code and request id, and nothing that was sent or named', async () => {
    await signIn();
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
        if (path.includes('/messages') && !path.includes('order')) {
          return Promise.resolve(json({ code: 'channel_not_found', message: `no channel ${channelName}` }, 404, { 'X-Request-Id': '00112233445566aa' }));
        }
        if (path.includes('/voice/session')) {
          return Promise.resolve(json({ code: 'voice_unavailable', message: `room ${roomName} is down` }, 503, { 'X-Request-Id': '00112233445566bb' }));
        }
        if (path.includes('/invites/preview')) {
          return Promise.resolve(json({ code: 'invite_invalid', message: `${inviteCode} is not valid` }, 404, { 'X-Request-Id': '00112233445566cc' }));
        }
        if (path.includes('order=desc')) return Promise.reject(new TypeError(`fetch failed for ${path}`));
        // A request id the client did not get from conchd's shape is not kept.
        return Promise.resolve(json({ code: messageText, message: messageText }, 500, { 'X-Request-Id': ticket }));
      }),
    );
    await api.postMessage(channelName, messageText);
    await api.createVoiceSession(channelName);
    await api.previewInvite(inviteCode);
    await api.loadMessages(channelName);
    await api.listMembers(channelName);
    expect(timeless()).toEqual([
      { kind: 'http_failure', code: 404, error: 'channel_not_found', request_id: '00112233445566aa' },
      { kind: 'http_failure', code: 503, error: 'voice_unavailable', request_id: '00112233445566bb' },
      { kind: 'http_failure', code: 404, error: 'invite_invalid', request_id: '00112233445566cc' },
      { kind: 'http_failure', error: 'network' },
      { kind: 'http_failure', code: 500 },
    ]);
    expectClean();
  });

  it('records sockets by what they carry: opens, closes with their codes, and the tries after a loss', async () => {
    await signIn();
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ ticket, expires_at: '2026-10-16T10:00:00Z' }, 201))));
    const sockets: { url: string; onopen: (() => void) | null; onclose: ((e: { code: number; reason: string }) => void) | null }[] = [];
    vi.stubGlobal(
      'WebSocket',
      vi.fn(function (this: (typeof sockets)[number], url: string) {
        this.url = url;
        this.onopen = null;
        this.onclose = null;
        sockets.push(this);
      }),
    );
    const events = { onOpen: () => undefined, onFrame: () => undefined, onClose: () => undefined };
    expect((await api.openSocket(channelName, 0, events)).ok).toBe(true);
    sockets[0]?.onopen?.();
    sockets[0]?.onclose?.({ code: 1006, reason: `lost ${channelName}` });
    expect((await api.openSocket(channelName, 5, events)).ok).toBe(true);
    sockets[1]?.onclose?.({ code: 1008, reason: 'no longer a member of this channel' });
    expect((await api.openSocket(channelName, 5, events)).ok).toBe(true);
    sockets[2]?.onopen?.();
    expect((await api.openVoiceSocket(channelName, events)).ok).toBe(true);
    sockets[3]?.onopen?.();
    // The addresses held the ticket and the channel; the ring does not.
    expect(sockets[0]?.url).toContain(ticket);
    expect(timeless()).toEqual([
      { kind: 'socket_open', socket: 'messages' },
      { kind: 'socket_close', socket: 'messages', code: 1006 },
      { kind: 'reconnect', socket: 'messages', attempt: 1 },
      { kind: 'socket_close', socket: 'messages', code: 1008 },
      { kind: 'reconnect', socket: 'messages', attempt: 2 },
      { kind: 'socket_open', socket: 'messages' },
      { kind: 'socket_open', socket: 'voice' },
    ]);
    expectClean();
  });
});

// The ring belongs to one sign-in: every way a session ends empties it, so the
// next person to sign in on a shared browser does not report the last one's
// events. A failure that ends nothing keeps it.
describe('when the session ends', () => {
  async function signedInWithEvents() {
    window.localStorage.setItem('conch.web.token', token);
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ id: 2, kind: 'human', name: 'ada', role: 'member' }))));
    expect((await api.restore()).ok).toBe(true);
    record({ kind: 'socket_open', socket: 'messages' });
    record({ kind: 'voice_state', state: 'connected' });
    expect(recent()).toHaveLength(2);
  }

  it.each([
    ['sign out', 204, () => api.signOut()],
    ['sign out, refused by the server', 500, () => api.signOut()],
    ['sign out everywhere', 200, () => api.signOutEverywhere(2)],
    ['a 401 on any call', 401, () => api.listChannels()],
    ['a 401 on a write', 401, () => api.markRead(channelName, 3)],
  ] as const)('is emptied by %s', async (_case, status, end) => {
    await signedInWithEvents();
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json(status === 200 ? { revoked: 2 } : { code: 'x', message: 'x' }, status))));
    await end();
    expect(recent()).toEqual([]);
    expect(voiceState()).toBeNull();
  });

  it('is emptied when another tab signs out', async () => {
    await signedInWithEvents();
    let told = false;
    const stop = api.onSignedOutElsewhere(() => {
      told = true;
    });
    window.localStorage.removeItem('conch.web.token');
    window.dispatchEvent(new StorageEvent('storage', { storageArea: window.localStorage, key: 'conch.web.token', oldValue: token, newValue: null }));
    stop();
    expect(told).toBe(true);
    expect(recent()).toEqual([]);
  });

  it('is kept through a failure that ends nothing', async () => {
    await signedInWithEvents();
    vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ code: 'internal_error', message: 'x' }, 500))));
    await api.listChannels();
    expect(recent()).toHaveLength(3);
  });
});

// The voice client records its states and a microphone's refusal by name,
// and nothing of the room, the token or the channel.
describe('what the voice client records', () => {
  it('records each change of state and a device error, never the room', async () => {
    const clock = new FakeClock();
    let refuse = true;
    const room = (): Room => ({
      connect: () => (refuse ? Promise.reject(new DOMException(`microphone refused for ${roomName}`, 'NotAllowedError')) : Promise.resolve()),
      publishMicrophone: () => Promise.resolve(),
      setWatching: () => undefined,
      attachShare: () => undefined,
      pickScreen: () => Promise.reject(new Error('no screen in this test')),
      publishScreen: () => Promise.resolve(),
      stopScreen: () => Promise.resolve(),
      keepScreen: () => null,
      adoptScreen: () => undefined,
      setSending: () => Promise.resolve(),
      setDeafened: () => undefined,
      level: () => 0,
      close: () => Promise.resolve(),
    });
    const client = new VoiceClient(
      {
        clock,
        random: () => 0.5,
        createSession: () =>
          Promise.resolve({
            ok: true,
            value: { livekit_url: 'wss://lk.example', identity: 'p7', rooms: [{ room: roomName, token, can_publish: true, expires_at: '2026-10-16T00:00:15.000Z' }] },
          }),
        report: () => Promise.resolve('ok'),
        newRoom: room,
        claimShare: () => Promise.resolve({ ok: false, failure: 'network' }),
        stopShare: () => Promise.resolve(),
      },
      () => clock.now,
    );
    client.join(channelName);
    await settle();
    refuse = false;
    clock.advance(60_000);
    await settle();
    expect(client.snapshot().phase).toBe('connected');
    expect(timeless()).toEqual([
      { kind: 'voice_state', state: 'connecting' },
      { kind: 'device_error', name: 'NotAllowedError' },
      { kind: 'voice_state', state: 'unavailable' },
      { kind: 'voice_state', state: 'connecting' },
      { kind: 'voice_state', state: 'connected' },
    ]);
    expect(voiceState()).toBe('connected');
    expectClean();
    client.dispose();
  });
});
