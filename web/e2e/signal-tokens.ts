// The tokens LiveKit itself hands a page over its signalling socket (issue
// #1948), read by the test from the frames the browser received, so that
// expectClean watches for them as it watches for the join tokens conchd gives.
// Input: one frame LiveKit sent the page, as Playwright gives it (a string, or
// the bytes of a binary frame). Output: every token in it. It owns the reading
// of those two fields and nothing else; it never logs what it finds.
//
// After a join LiveKit sends the page a token of its own, refreshed from the one
// conchd issued (and every few minutes after), and the SDK reconnects with it:
// it is as good as a join token for the room, and conchd never sees it. A room
// move carries one too. livekit-client 2.x reads each frame as a SignalResponse
// (@livekit/protocol, livekit_rtc.proto): protobuf in a binary frame, the
// default, or protobuf JSON in a text frame. The two fields that hold a token:
//   16 refresh_token  string
//   24 room_moved     RoomMovedResponse, whose field 2 is token (a string)
// A frame the SDK could not read either gives the page no token, so one that
// does not decode here gives none.

const refreshToken = 16;
const roomMoved = 24;
const roomMovedToken = 2;

// One field of a protobuf message: its number, and its bytes when it is
// length-delimited (wire type 2; the other wire types are skipped).
interface Field {
  readonly no: number;
  readonly bytes?: Uint8Array;
}

// fields reads the top level of one protobuf message, or undefined if it is
// not one: a truncated value, a group (wire types 3 and 4, which no field of
// these messages uses) or an unknown wire type.
function fields(buf: Uint8Array): Field[] | undefined {
  const out: Field[] = [];
  let at = 0;
  // A varint, as a number: exact up to 2^53, which every length and field
  // number here is far below. undefined if it runs past the end or past 10
  // bytes.
  const varint = (): number | undefined => {
    let value = 0;
    let scale = 1;
    for (let i = 0; i < 10; i++) {
      if (at >= buf.length) return undefined;
      const b = buf[at++] ?? 0;
      value += (b & 0x7f) * scale;
      if ((b & 0x80) === 0) return value;
      scale *= 128;
    }
    return undefined;
  };
  while (at < buf.length) {
    const key = varint();
    if (key === undefined) return undefined;
    const no = Math.floor(key / 8);
    const wire = key % 8;
    if (no === 0) return undefined;
    if (wire === 0) {
      if (varint() === undefined) return undefined;
      out.push({ no });
    } else if (wire === 1 || wire === 5) {
      at += wire === 1 ? 8 : 4;
      if (at > buf.length) return undefined;
      out.push({ no });
    } else if (wire === 2) {
      const n = varint();
      if (n === undefined || at + n > buf.length) return undefined;
      out.push({ no, bytes: buf.subarray(at, at + n) });
      at += n;
    } else {
      return undefined;
    }
  }
  return out;
}

const utf8 = new TextDecoder();

// fromBinary is the tokens of a SignalResponse in protobuf.
function fromBinary(buf: Uint8Array): string[] {
  const top = fields(buf);
  if (top === undefined) return [];
  const tokens: string[] = [];
  for (const f of top) {
    if (f.bytes === undefined) continue;
    if (f.no === refreshToken) tokens.push(utf8.decode(f.bytes));
    if (f.no === roomMoved) {
      for (const g of fields(f.bytes) ?? []) {
        if (g.no === roomMovedToken && g.bytes !== undefined) tokens.push(utf8.decode(g.bytes));
      }
    }
  }
  return tokens;
}

// fromJson is the tokens of a SignalResponse in protobuf JSON, which names a
// field in lowerCamelCase or, as a reader also accepts, by its proto name.
function fromJson(text: string): string[] {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch {
    return [];
  }
  if (typeof value !== 'object' || value === null) return [];
  const msg = value as Record<string, unknown>;
  const tokens: unknown[] = [msg['refreshToken'], msg['refresh_token']];
  for (const moved of [msg['roomMoved'], msg['room_moved']]) {
    if (typeof moved === 'object' && moved !== null) tokens.push((moved as Record<string, unknown>)['token']);
  }
  return tokens.filter((t): t is string => typeof t === 'string');
}

// tokensInSignal is every token in one frame LiveKit sent: none, usually. An
// empty token is no secret and is left out.
export function tokensInSignal(payload: string | Uint8Array): string[] {
  const tokens = typeof payload === 'string' ? fromJson(payload) : fromBinary(payload);
  return tokens.filter((t) => t !== '');
}
