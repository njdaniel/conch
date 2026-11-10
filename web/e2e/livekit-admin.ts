// LiveKit's own view of its rooms, for the browser suite (issue #1608): who is
// in them, asked of LiveKit directly with the API secret the voice world chose,
// so the answer does not pass through conchd or any page. It mirrors
// e2e/internal/lkit/livekit.go's Admin (an HS256 admin token and the Twirp
// RoomService), with node:crypto and fetch alone: no dependency.
//
// It owns the token and the two calls. Room names and the token never leave
// it: an error says the method and the HTTP status, nothing else, and the
// result is identities, their tracks and a count of rooms (conchd's identities
// are "p<id>", with ".screen" or ".view" for a companion).
//
// Polling: a throw inside an expect.poll callback fails the poll at once in
// Playwright 1.61 (expect.js, invokePollMatcher awaits the callback outside its
// try), so a poll must not see LiveKitNoAnswerError. untilAnswered turns it
// into a value the poll's matcher rejects, and the poll goes on.
import { createHmac } from 'node:crypto';

// Unpadded URL-safe base64 of a string's UTF-8 bytes, or of bytes.
const b64 = (data: Uint8Array | string): string => {
  const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : data;
  return btoa(String.fromCharCode(...bytes)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
};

export interface AdminGrant {
  readonly roomAdmin?: boolean;
  readonly roomList?: boolean;
  readonly room?: string;
}

// adminToken signs an HS256 token for LiveKit's admin API carrying grant,
// valid from 30 s before now (clock skew with the container) for a minute.
export function adminToken(key: string, secret: string, grant: AdminGrant, now: Date = new Date()): string {
  const seconds = Math.floor(now.getTime() / 1000);
  const signing = `${b64('{"alg":"HS256","typ":"JWT"}')}.${b64(JSON.stringify({ iss: key, nbf: seconds - 30, exp: seconds + 60, video: grant }))}`;
  return `${signing}.${b64(createHmac('sha256', secret).update(signing).digest())}`;
}

// A failed call, as its method and status only: the body can name a room and
// the address a port, and neither is for a log.
export class LiveKitStatusError extends Error {
  constructor(
    readonly method: string,
    readonly status: number,
  ) {
    super(`livekit ${method}: HTTP ${String(status)}`);
  }
}

// A call that got no usable answer: a timeout, a refused or dropped
// connection, or a body that was not JSON. Retryable, unlike a status.
export class LiveKitNoAnswerError extends Error {
  constructor(
    readonly method: string,
    readonly why: 'no answer' | 'unreadable answer',
  ) {
    super(`livekit ${method}: ${why}`);
  }
}

async function call(base: string, key: string, secret: string, method: string, grant: AdminGrant, body: unknown): Promise<unknown> {
  let res: Response;
  try {
    res = await fetch(`${base}/twirp/livekit.RoomService/${method}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${adminToken(key, secret, grant)}` },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(5000),
    });
  } catch {
    // The transport error names the address; say only that there was none.
    throw new LiveKitNoAnswerError(method, 'no answer');
  }
  if (res.status !== 200) throw new LiveKitStatusError(method, res.status);
  try {
    return (await res.json()) as unknown;
  } catch {
    // A parse error quotes part of the body, which can hold a room name.
    throw new LiveKitNoAnswerError(method, 'unreadable answer');
  }
}

// A track LiveKit lists for a participant: its sid and source
// (MICROPHONE, CAMERA, SCREEN_SHARE, SCREEN_SHARE_AUDIO).
export interface PublishedTrack {
  readonly sid: string;
  readonly source: string;
}

export interface Listed {
  readonly identity: string;
  readonly tracks: readonly PublishedTrack[];
}

async function roomNames(base: string, key: string, secret: string): Promise<string[]> {
  const rooms = (await call(base, key, secret, 'ListRooms', { roomList: true }, {})) as { rooms?: { name?: string }[] };
  return (rooms.rooms ?? []).flatMap((r) => (typeof r.name === 'string' ? [r.name] : []));
}

// listParticipants is every participant in every room of the LiveKit at base
// (http://127.0.0.1:port) with the tracks it publishes, one entry per
// connection: a person connected twice is listed twice. A room that ends
// between the two calls has no one.
export async function listParticipants(base: string, key: string, secret: string): Promise<Listed[]> {
  const out: Listed[] = [];
  for (const room of await roomNames(base, key, secret)) {
    type Found = { participants?: { identity?: string; tracks?: { sid?: string; source?: string }[] }[] };
    let found: Found;
    try {
      found = (await call(base, key, secret, 'ListParticipants', { roomAdmin: true, room }, { room })) as Found;
    } catch (err) {
      if (err instanceof LiveKitStatusError && err.status === 404) continue;
      throw err;
    }
    for (const p of found.participants ?? []) {
      if (typeof p.identity !== 'string') continue;
      const tracks: PublishedTrack[] = [];
      for (const t of p.tracks ?? []) if (typeof t.sid === 'string' && typeof t.source === 'string') tracks.push({ sid: t.sid, source: t.source });
      out.push({ identity: p.identity, tracks });
    }
  }
  return out;
}

// listIdentities is the identity of every participant in every room, sorted,
// one entry per connection: a person connected twice is listed twice, which is
// what a duplicate check needs.
export async function listIdentities(base: string, key: string, secret: string): Promise<string[]> {
  return (await listParticipants(base, key, secret)).map((p) => p.identity).sort();
}

// roomCount is how many rooms LiveKit lists: 0 once the last room has closed.
export async function roomCount(base: string, key: string, secret: string): Promise<number> {
  return (await roomNames(base, key, secret)).length;
}

// listTracks is the tracks one identity publishes, of the sources keep
// accepts, sorted by sid, across every room.
export async function listTracks(base: string, key: string, secret: string, identity: string, keep: (source: string) => boolean): Promise<PublishedTrack[]> {
  const out: PublishedTrack[] = [];
  for (const p of await listParticipants(base, key, secret)) {
    if (p.identity === identity) out.push(...p.tracks.filter((t) => keep(t.source)));
  }
  return out.sort((a, b) => (a.sid < b.sid ? -1 : a.sid > b.sid ? 1 : 0));
}

// untilAnswered runs ask and, if LiveKit gave no usable answer, returns
// sentinel instead of throwing, for a caller that polls: see the top of this
// file. A status error still throws, as that is a fault and not a wait; so
// does anything else. The sentinel must be a value the poll can never accept.
export async function untilAnswered<T>(ask: () => Promise<T>, sentinel: T): Promise<T> {
  try {
    return await ask();
  } catch (err) {
    if (err instanceof LiveKitNoAnswerError) return sentinel;
    throw err;
  }
}
