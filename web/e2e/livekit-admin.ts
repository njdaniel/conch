// LiveKit's own view of its rooms, for the browser suite (issue #1608): who is
// in them, asked of LiveKit directly with the API secret the voice world chose,
// so the answer does not pass through conchd or any page. It mirrors
// e2e/internal/lkit/livekit.go's Admin (an HS256 admin token and the Twirp
// RoomService), with node:crypto and fetch alone: no dependency.
//
// It owns the token and the two calls. Room names and the token never leave
// it: an error says the method and the HTTP status, nothing else, and the
// result is identities only (conchd's "p<id>", with ".screen" or ".view" for a
// companion).
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
    throw new Error(`livekit ${method}: no answer`);
  }
  if (res.status !== 200) throw new LiveKitStatusError(method, res.status);
  try {
    return (await res.json()) as unknown;
  } catch {
    // A parse error quotes part of the body, which can hold a room name.
    throw new Error(`livekit ${method}: unreadable answer`);
  }
}

// listIdentities is the identity of every participant in every room of the
// LiveKit at base (http://127.0.0.1:port), sorted, one entry per connection: a
// person connected twice is listed twice, which is what a duplicate check
// needs. A room that ends between the two calls has no one.
export async function listIdentities(base: string, key: string, secret: string): Promise<string[]> {
  const rooms = (await call(base, key, secret, 'ListRooms', { roomList: true }, {})) as { rooms?: { name?: string }[] };
  const identities: string[] = [];
  for (const room of rooms.rooms ?? []) {
    if (typeof room.name !== 'string') continue;
    try {
      const found = (await call(base, key, secret, 'ListParticipants', { roomAdmin: true, room: room.name }, { room: room.name })) as { participants?: { identity?: string }[] };
      for (const p of found.participants ?? []) if (typeof p.identity === 'string') identities.push(p.identity);
    } catch (err) {
      if (!(err instanceof LiveKitStatusError && err.status === 404)) throw err;
    }
  }
  return identities.sort();
}
