// The tokens LiveKit itself hands a page over its signalling socket (issue
// #1948), read by the test from the frames the browser received, so that
// expectClean watches for them as it watches for the join tokens conchd gives.
// Input: one frame LiveKit sent the page, as Playwright gives it (a string, or
// the bytes of a binary frame). Output: every token in it. It owns the reading
// of those fields and nothing else; it never logs what it finds.
//
// After a join LiveKit sends the page a token of its own, refreshed from the one
// conchd issued (and every few minutes after), and the SDK reconnects with it:
// it is as good as a join token for the room, and conchd never sees it. A room
// move carries one too. livekit-client 2.x reads each frame as a SignalResponse
// (@livekit/protocol, livekit_rtc.proto): protobuf in a binary frame, the
// default, or protobuf JSON in a text frame. The two fields that hold a token:
//   16 refresh_token  string
//   24 room_moved     RoomMovedResponse, whose field 2 is token (a string)
//
// A token the SDK would accept must never be missed, so a frame is read two
// ways and every token either finds is given:
//   - Walked as the SDK's decoder walks it (the protobuf-es 1.x BinaryReader
//     that livekit-client 2.22 bundles; see Reader): a field it does not know is
//     skipped by its wire type, groups included, and the walk goes on; a field
//     it knows is read by its declared type, whatever wire type its tag says; a
//     tag keeps only its low 32 bits. Where the SDK would throw, the walk stops,
//     keeping what it had found.
//   - Scanned, bytes or text, for anything shaped like a JWT (which every
//     LiveKit token is). The walk follows the SDK exactly only in the two
//     messages that hold a token; the SDK reads every other message by its own
//     schema, which can end it somewhere other than where its length said (a
//     field it knows read past the end), and the walk cannot follow that. The
//     scan finds a token wherever such a walk would have put it.
// Reading a frame never throws.

// How the SDK reads a field it knows, whatever the wire type of its tag:
// 'message' and 'string' as a length and that many bytes, 'varint' as one
// varint (an int64).
type Kind = 'message' | 'string' | 'varint';

// SignalResponse, livekit_rtc.proto in @livekit/protocol 1.50: fields 1 to 31
// (one oneof), all messages but 16 (refresh_token, a string) and 18 (pong, an
// int64); 7 is not used.
const refreshToken = 16;
const roomMoved = 24;
const signalResponse = (no: number): Kind | undefined => {
  if (no === refreshToken) return 'string';
  if (no === 18) return 'varint';
  if (no >= 1 && no <= 31 && no !== 7) return 'message';
  return undefined;
};
// RoomMovedResponse: 1 room, 3 participant and 4 other_participants are
// messages, 2 token a string.
const roomMovedToken = 2;
const roomMovedResponse = (no: number): Kind | undefined => {
  if (no === roomMovedToken) return 'string';
  if (no === 1 || no === 3 || no === 4) return 'message';
  return undefined;
};

// Thrown where the SDK's decoder throws, so that the frame gives it no message.
class Malformed extends Error {}

// Reader is the protobuf-es 1.x BinaryReader as livekit-client 2.22.3 bundles
// it, in the parts a SignalResponse needs: what it accepts, where it throws, and
// where each read leaves it.
class Reader {
  at = 0;
  readonly buf: Uint8Array;
  constructor(buf: Uint8Array) {
    this.buf = buf;
  }

  // The SDK checks its bounds after a read, not before: reading on past the
  // end throws.
  private byte(): number {
    const b = this.buf[this.at++];
    if (b === undefined) throw new Malformed();
    return b;
  }

  bounds(): void {
    if (this.at > this.buf.length) throw new Malformed();
  }

  // uint32 reads a varint of at most 10 bytes and keeps its low 32 bits (the
  // fifth byte gives four), as varint32read does; a tag or a length too big for
  // 32 bits is cut down, not refused.
  uint32(): number {
    let value = 0;
    for (let i = 0; i < 10; i++) {
      const b = this.byte();
      if (i < 4) value += (b & 0x7f) * 2 ** (7 * i);
      else if (i === 4) value += (b & 0x0f) * 2 ** 28;
      if ((b & 0x80) === 0) return value;
    }
    throw new Malformed();
  }

  // varint64 reads a varint of at most 10 bytes, as varint64read does; its
  // value is not needed here.
  varint64(): void {
    for (let i = 0; i < 10; i++) {
      if ((this.byte() & 0x80) === 0) return;
    }
    throw new Malformed();
  }

  // tag is a field number and a wire type. The SDK refuses field number 0 and
  // wire types 6 and 7.
  tag(): [number, number] {
    const t = this.uint32();
    const no = Math.floor(t / 8);
    const wire = t % 8;
    if (no <= 0 || wire > 5) throw new Malformed();
    return [no, wire];
  }

  // skip steps over a field the SDK does not know, by its wire type: a varint
  // of any length, 8 or 4 bytes, a length and that many bytes, or a group up to
  // the end tag of the same field number. A lone end group (4) is refused.
  skip(wire: number, no: number): void {
    switch (wire) {
      case 0:
        while ((this.byte() & 0x80) !== 0) {
          // on to the varint's last byte
        }
        break;
      case 1:
        this.at += 8;
        break;
      case 5:
        this.at += 4;
        break;
      case 2: {
        // The length first: `this.at += this.uint32()` would add it to where
        // the reader was before the length was read.
        const n = this.uint32();
        this.at += n;
        break;
      }
      case 3:
        for (;;) {
          const [fn, wt] = this.tag();
          if (wt === 4) {
            if (fn !== no) throw new Malformed();
            break;
          }
          this.skip(wt, fn);
        }
        break;
      default:
        throw new Malformed();
    }
    this.bounds();
  }

  // bytes reads a length and that many bytes, as a string or a message field
  // is read.
  bytes(): Uint8Array {
    const n = this.uint32();
    const start = this.at;
    this.at += n;
    this.bounds();
    return this.buf.subarray(start, start + n);
  }
}

// The SDK's TextDecoder is the default one, which replaces what is not UTF-8
// rather than throwing.
const utf8 = new TextDecoder();

// walk reads one message up to end as the SDK's readMessage does, giving each
// field it knows to visit, whose read must leave the reader past the field.
// A message is read until the reader is at or past end: a field read past end
// is not refused, and the message around it goes on from there.
function walk(r: Reader, end: number, kind: (no: number) => Kind | undefined, visit: (no: number, kind: Kind) => void): void {
  while (r.at < end) {
    const [no, wire] = r.tag();
    const k = kind(no);
    if (k === undefined) r.skip(wire, no);
    else visit(no, k);
  }
}

// fromBinary is the tokens of a SignalResponse in protobuf, every one the SDK
// would read (a oneof keeps only the last, but every one is a token LiveKit
// sent). If the SDK would throw, the tokens read before that point.
function fromBinary(buf: Uint8Array): string[] {
  const tokens: string[] = [];
  const r = new Reader(buf);
  try {
    walk(r, buf.length, signalResponse, (no, k) => {
      if (k === 'varint') r.varint64();
      else if (no === refreshToken) tokens.push(utf8.decode(r.bytes()));
      else if (no === roomMoved) {
        // Read with the same reader, as the SDK does, so that a token read
        // past the message's end moves the frame on as it moves the SDK on.
        const n = r.uint32();
        walk(r, r.at + n, roomMovedResponse, (inner, ik) => {
          if (ik === 'varint') r.varint64();
          else if (inner === roomMovedToken) tokens.push(utf8.decode(r.bytes()));
          else r.bytes();
        });
      } else r.bytes();
    });
  } catch (e) {
    // A RangeError is groups nested past the stack, where the SDK's
    // recursion fails as well.
    if (!(e instanceof Malformed) && !(e instanceof RangeError)) throw e;
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

// A JWT: a header (a JSON object, so base64url "eyJ"), a payload and a
// signature, each base64url, joined by dots.
const jwtShaped = /eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g;

// One char per byte, so that a token in a binary frame reads as it was sent.
const latin1 = new TextDecoder('latin1');

// scanned is every JWT-shaped string in a frame that the read did not find,
// with its signed part (header, payload and the dot after) beside it. In a
// binary frame the byte after a token can be a base64url character too (a tag
// or a length), which the match takes as part of the signature, so the match
// can be longer than the token; the signed part is not, and any line that
// shows the token shows it.
function scanned(text: string, read: readonly string[]): string[] {
  const out: string[] = [];
  for (const [m] of text.matchAll(jwtShaped)) {
    const signed = m.slice(0, m.indexOf('.', m.indexOf('.') + 1) + 1);
    if (read.some((t) => t.startsWith(signed))) continue;
    out.push(m, signed);
  }
  return out;
}

// tokensInSignal is every token in one frame LiveKit sent: none, usually.
// Those the read finds come first, in the order the frame holds them, then
// what only the scan finds. An empty token is no secret and is left out, as is
// a repeat.
export function tokensInSignal(payload: string | Uint8Array): string[] {
  const read = typeof payload === 'string' ? fromJson(payload) : fromBinary(payload);
  const text = typeof payload === 'string' ? payload : latin1.decode(payload);
  const all = [...read, ...scanned(text, read)];
  return all.filter((t, i) => t !== '' && all.indexOf(t) === i);
}
