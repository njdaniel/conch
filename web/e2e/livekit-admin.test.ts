// The admin token and the Twirp helper of livekit-admin.ts, with no network:
// fetch is replaced.
import { createHmac } from 'node:crypto';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { adminToken, listIdentities, LiveKitStatusError, type AdminGrant } from './livekit-admin';

const secret = 'unit-test-NOT-A-REAL-SECRET-0123456789';
const unb64 = (part: string): string => atob(part.replaceAll('-', '+').replaceAll('_', '/'));
const decode = (part: string): unknown => JSON.parse(unb64(part));
// The expected signature, by the same algorithm written independently of the helper.
const sign = (signing: string): string => {
  const raw = createHmac('sha256', secret).update(signing).digest();
  return btoa(String.fromCharCode(...raw)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('adminToken', () => {
  const now = new Date('2026-11-10T12:00:00Z');
  const seconds = Math.floor(now.getTime() / 1000);
  const cases: { name: string; grant: AdminGrant }[] = [
    { name: 'list rooms', grant: { roomList: true } },
    { name: 'administer one room', grant: { roomAdmin: true, room: 'a-room' } },
  ];
  it.each(cases)('$name: header, claims and signature', ({ grant }) => {
    const token = adminToken('thekey', secret, grant, now);
    const parts = token.split('.');
    expect(parts).toHaveLength(3);
    expect(decode(parts[0] ?? '')).toEqual({ alg: 'HS256', typ: 'JWT' });
    expect(decode(parts[1] ?? '')).toEqual({ iss: 'thekey', nbf: seconds - 30, exp: seconds + 60, video: grant });
    expect(parts[2]).toBe(sign(`${parts[0] ?? ''}.${parts[1] ?? ''}`));
    // Signed by the secret, which is not in the token.
    expect(token).not.toContain(secret);
  });

  it('expires in the future, whenever it is signed', () => {
    const claims = decode(adminToken('k', secret, { roomList: true }).split('.')[1] ?? '') as { exp: number };
    expect(claims.exp).toBeGreaterThan(Date.now() / 1000);
  });
});

describe('listIdentities', () => {
  const stub = (answers: Record<string, { status: number; body: unknown }>) => {
    const asked: { url: string; auth: string }[] = [];
    vi.stubGlobal('fetch', (url: string, init: RequestInit) => {
      const headers = init.headers as Record<string, string>;
      asked.push({ url, auth: headers['Authorization'] ?? '' });
      const method = url.split('/').pop() ?? '';
      const body = JSON.parse(init.body as string) as { room?: string };
      const answer = answers[body.room === undefined ? method : `${method} ${body.room}`] ?? { status: 500, body: {} };
      return Promise.resolve(new Response(JSON.stringify(answer.body), { status: answer.status }));
    });
    return asked;
  };

  it('lists every identity of every room, sorted, duplicates kept', async () => {
    const asked = stub({
      ListRooms: { status: 200, body: { rooms: [{ name: 'r1' }, { name: 'r2' }] } },
      'ListParticipants r1': { status: 200, body: { participants: [{ identity: 'p3' }, { identity: 'p1' }] } },
      'ListParticipants r2': { status: 200, body: { participants: [{ identity: 'p1' }] } },
    });
    expect(await listIdentities('http://127.0.0.1:1', 'k', secret)).toEqual(['p1', 'p1', 'p3']);
    expect(asked.map((a) => a.url)).toEqual([
      'http://127.0.0.1:1/twirp/livekit.RoomService/ListRooms',
      'http://127.0.0.1:1/twirp/livekit.RoomService/ListParticipants',
      'http://127.0.0.1:1/twirp/livekit.RoomService/ListParticipants',
    ]);
    for (const a of asked) expect(a.auth).toMatch(/^Bearer [\w-]+\.[\w-]+\.[\w-]+$/);
  });

  it('is empty with no room, and with a room that ended between the calls', async () => {
    stub({ ListRooms: { status: 200, body: {} } });
    expect(await listIdentities('http://x', 'k', secret)).toEqual([]);
    stub({ ListRooms: { status: 200, body: { rooms: [{ name: 'gone' }] } }, 'ListParticipants gone': { status: 404, body: { msg: 'gone' } } });
    expect(await listIdentities('http://x', 'k', secret)).toEqual([]);
  });

  it('fails with the status alone: no secret, token or room name in the message', async () => {
    stub({ ListRooms: { status: 200, body: { rooms: [{ name: 'secret-room-name' }] } }, 'ListParticipants secret-room-name': { status: 403, body: { msg: 'secret-room-name' } } });
    const err = await listIdentities('http://x', 'k', secret).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LiveKitStatusError);
    expect((err as Error).message).toBe('livekit ListParticipants: HTTP 403');
    vi.stubGlobal('fetch', () => Promise.reject(new Error(`connect ECONNREFUSED http://127.0.0.1:1 ${secret}`)));
    const down = await listIdentities('http://x', 'k', secret).catch((e: unknown) => e);
    expect((down as Error).message).toBe('livekit ListRooms: no answer');
  });

  it('an unreadable 200 answer says so without quoting the body', async () => {
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('{"rooms": secret-room-name', { status: 200 })));
    const err = await listIdentities('http://x', 'k', secret).catch((e: unknown) => e);
    expect((err as Error).message).toBe('livekit ListRooms: unreadable answer');
    expect((err as Error).message).not.toContain('secret-room-name');
  });
});
