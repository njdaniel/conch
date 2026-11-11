// The admin token and the Twirp helper of livekit-admin.ts, with no network:
// fetch is replaced.
import { createHmac } from 'node:crypto';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { adminToken, answered, listIdentities, listTracks, LiveKitNoAnswerError, LiveKitStatusError, mustAnswer, noAnswer, roomCount, untilAnswered, type AdminGrant } from './livekit-admin';

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

  it('asks with the grant each call needs: roomList for ListRooms, roomAdmin on that room for ListParticipants', async () => {
    const asked = stub({
      ListRooms: { status: 200, body: { rooms: [{ name: 'r1' }, { name: 'r2' }] } },
      'ListParticipants r1': { status: 200, body: { participants: [] } },
      'ListParticipants r2': { status: 200, body: { participants: [] } },
    });
    await listIdentities('http://x', 'k', secret);
    const grants = asked.map((a) => ({ method: a.url.split('/').pop(), video: (decode(a.auth.replace('Bearer ', '').split('.')[1] ?? '') as { video: AdminGrant }).video }));
    expect(grants).toEqual([
      { method: 'ListRooms', video: { roomList: true } },
      { method: 'ListParticipants', video: { roomAdmin: true, room: 'r1' } },
      { method: 'ListParticipants', video: { roomAdmin: true, room: 'r2' } },
    ]);
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

describe('listTracks and roomCount', () => {
  const answers = () =>
    vi.stubGlobal('fetch', (url: string, init: RequestInit) => {
      const method = url.split('/').pop() ?? '';
      const room = (JSON.parse(init.body as string) as { room?: string }).room;
      const body =
        method === 'ListRooms'
          ? { rooms: [{ name: 'r1' }, { name: 'r2' }, { name: 'ended' }] }
          : room === 'r1'
            ? {
                participants: [
                  { identity: 'p1', tracks: [{ sid: 'TR_b', source: 'MICROPHONE' }, { sid: 'TR_a', source: 'SCREEN_SHARE' }, { sid: 'TR_c', source: 'SCREEN_SHARE_AUDIO' }] },
                  { identity: 'p2', tracks: [{ sid: 'TR_x', source: 'MICROPHONE' }] },
                ],
              }
            : { participants: [{ identity: 'p1', tracks: [{ sid: 'TR_0', source: 'MICROPHONE' }] }] };
      return Promise.resolve(new Response(JSON.stringify(body), { status: room === 'ended' ? 404 : 200 }));
    });

  it('lists one identity\'s tracks of the wanted sources across rooms, sorted by sid, and skips an ended room', async () => {
    answers();
    expect(await listTracks('http://x', 'k', secret, 'p1', (s) => s === 'MICROPHONE')).toEqual([
      { sid: 'TR_0', source: 'MICROPHONE' },
      { sid: 'TR_b', source: 'MICROPHONE' },
    ]);
    expect(await listTracks('http://x', 'k', secret, 'p1', (s) => s.startsWith('SCREEN_SHARE'))).toEqual([
      { sid: 'TR_a', source: 'SCREEN_SHARE' },
      { sid: 'TR_c', source: 'SCREEN_SHARE_AUDIO' },
    ]);
    expect(await listTracks('http://x', 'k', secret, 'p9', () => true)).toEqual([]);
  });

  it('counts rooms, and zero when there are none', async () => {
    answers();
    expect(await roomCount('http://x', 'k', secret)).toBe(3);
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('{}', { status: 200 })));
    expect(await roomCount('http://x', 'k', secret)).toBe(0);
  });
});

describe('untilAnswered, answered and mustAnswer', () => {
  const down = () => vi.stubGlobal('fetch', () => Promise.reject(new Error('connect ECONNREFUSED')));

  it('is noAnswer, a value of its own, for a transport error, a timeout and an unreadable answer', async () => {
    down();
    expect(await untilAnswered(() => roomCount('http://x', 'k', secret))).toBe(noAnswer);
    vi.stubGlobal('fetch', () => Promise.reject(new DOMException('timed out', 'TimeoutError')));
    expect(await untilAnswered(() => listIdentities('http://x', 'k', secret))).toBe(noAnswer);
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('not json', { status: 200 })));
    expect(await untilAnswered(() => roomCount('http://x', 'k', secret))).toBe(noAnswer);
  });

  it('passes an answer through, and still throws a status error or anything else', async () => {
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('{}', { status: 200 })));
    expect(await untilAnswered(() => roomCount('http://x', 'k', secret))).toBe(0);
    vi.stubGlobal('fetch', () => Promise.resolve(new Response('{}', { status: 401 })));
    await expect(untilAnswered(() => roomCount('http://x', 'k', secret))).rejects.toBeInstanceOf(LiveKitStatusError);
    await expect(untilAnswered(() => Promise.reject(new Error('other')))).rejects.toThrow('other');
  });

  // The assertions of the specs, on a reading that got no answer: none may pass.
  describe('a reading with no answer cannot satisfy an assertion', () => {
    const reading = async <T>(ask: () => Promise<T>) => {
      down();
      return untilAnswered(ask);
    };

    it('in a poll (answered is undefined, which no matcher takes), for a list, a count and a length', async () => {
      const ids = await answered(reading(() => listIdentities('http://x', 'k', secret)));
      const tracks = await answered(reading(() => listTracks('http://x', 'k', secret, 'p1', () => true)));
      const rooms = await answered(reading(() => roomCount('http://x', 'k', secret)));
      expect(ids).toBeUndefined();
      expect(tracks).toBeUndefined();
      expect(rooms).toBeUndefined();
      // The spec's poll matchers: participants/tracks .toEqual([]), rooms .toBe(0), a length .toBe(1), toHaveLength.
      expect(ids).not.toEqual([]);
      expect(tracks).not.toEqual([]);
      expect(rooms).not.toBe(0);
      expect(tracks?.length).not.toBe(1);
      expect(() => { expect(tracks).toHaveLength(1); }).toThrow();
      expect(() => { expect(tracks).toHaveLength(0); }).toThrow();
    });

    it('in a one-off assertion (mustAnswer fails the test, so [] or a length of 1 is never read from it)', async () => {
      await expect(mustAnswer(reading(() => listTracks('http://x', 'k', secret, 'p1', () => true)))).rejects.toThrow('LiveKit gave no answer');
      await expect(mustAnswer(reading(() => listIdentities('http://x', 'k', secret)))).rejects.toThrow('LiveKit gave no answer');
    });

    it('mustAnswer and answered pass a real answer, an empty list included', async () => {
      vi.stubGlobal('fetch', () => Promise.resolve(new Response('{}', { status: 200 })));
      expect(await mustAnswer(untilAnswered(() => listIdentities('http://x', 'k', secret)))).toEqual([]);
      expect(await answered(untilAnswered(() => listIdentities('http://x', 'k', secret)))).toEqual([]);
    });
  });

  it('LiveKitNoAnswerError says the method and why, nothing else', () => {
    expect(new LiveKitNoAnswerError('ListRooms', 'no answer').message).toBe('livekit ListRooms: no answer');
  });
});
