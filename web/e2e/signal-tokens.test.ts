// Reading the tokens LiveKit hands a page from its signalling frames (issue
// #1948). A unit test: no browser, no network. The frames are encoded here by
// hand, field by field as livekit_rtc.proto numbers them.
//
// Each binary case says what the SDK's own decoder reads from the frame (sdk:
// the token, or undefined where it reads none or throws). Those were checked
// against SignalResponse.fromBinary from @livekit/protocol 1.50.4, the decoder
// livekit-client 2.22.3 bundles; the test asserts that tokensInSignal gives
// every token the SDK reads, and exactly what it gives besides.
import { describe, expect, it } from 'vitest';
import { tokensInSignal } from './signal-tokens';

const varint = (n: number): number[] => {
  const out: number[] = [];
  while (n >= 0x80) {
    out.push((n % 0x80) | 0x80);
    n = Math.floor(n / 0x80);
  }
  out.push(n);
  return out;
};
const bytes = (s: string) => [...new TextEncoder().encode(s)];
const tag = (no: number, wire: number) => varint(no * 8 + wire);
// A length-delimited field (wire type 2), a varint field (0) and a fixed32 (5).
const delimited = (no: number, body: number[]) => [...tag(no, 2), ...varint(body.length), ...body];
const number = (no: number, n: number) => [...tag(no, 0), ...varint(n)];
const fixed32 = (no: number) => [...tag(no, 5), 1, 2, 3, 4];
// A group (wire types 3 and 4) of field no around body.
const group = (no: number, body: number[] = [], endNo = no) => [...tag(no, 3), ...body, ...tag(endNo, 4)];
const frame = (...parts: number[][]) => new Uint8Array(parts.flat());

const token = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwMSJ9.c2lnbmF0dXJl';
// What the scan gives besides a token only it finds: the token's signed part.
const signed = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwMSJ9.';
// A token that is not JWT-shaped, which only the walk can find.
const opaque = 'not-a-jwt';
const refresh = (t: string) => delimited(16, bytes(t));

describe('tokensInSignal, binary frames, against what the SDK reads', () => {
  const cases: { name: string; payload: Uint8Array; sdk: string | undefined; want: string[] }[] = [
    { name: 'a refresh_token (field 16)', payload: frame(refresh(token)), sdk: token, want: [token] },
    {
      name: 'a room_moved (field 24) with its token (field 2) among a room and a participant',
      payload: frame(delimited(24, [...delimited(1, delimited(2, bytes('room'))), ...delimited(2, bytes(token)), ...delimited(3, delimited(1, bytes('p')))])),
      sdk: token,
      want: [token],
    },
    {
      name: 'a refresh_token after a pong (field 18, an int64) and unknown fields of every other wire type',
      payload: frame(number(18, 1234567890123), number(100, 300), fixed32(101), [...tag(102, 1), 1, 2, 3, 4, 5, 6, 7, 8], refresh(token)),
      sdk: token,
      want: [token],
    },
    // The security review's repro: an unknown empty group, then a refresh.
    { name: 'an unknown empty group (a3 06 a4 06), then a refresh_token', payload: frame([0xa3, 0x06, 0xa4, 0x06], refresh(token)), sdk: token, want: [token] },
    { name: 'an unknown empty group, then a token that is not JWT-shaped', payload: frame(group(100), refresh(opaque)), sdk: opaque, want: [opaque] },
    {
      name: 'an unknown group holding fields of every wire type and a group, then a refresh_token',
      payload: frame(group(100, [...number(1, 300), ...fixed32(2), ...delimited(3, bytes('x')), ...group(4, number(5, 1))]), refresh(opaque)),
      sdk: opaque,
      want: [opaque],
    },
    // A tag is a varint of up to 10 bytes of which the SDK keeps the low 32
    // bits: the fifth byte gives four, the rest are dropped.
    { name: 'a refresh_token whose tag overflows 32 bits in its fifth byte', payload: frame([0x82, 0x81, 0x80, 0x80, 0x70], varint(opaque.length), bytes(opaque)), sdk: opaque, want: [opaque] },
    {
      name: 'a refresh_token whose tag runs to 10 bytes',
      payload: frame([0x82, 0x81, 0x80, 0x80, 0xf0, 0xff, 0xff, 0xff, 0xff, 0x01], varint(opaque.length), bytes(opaque)),
      sdk: opaque,
      want: [opaque],
    },
    { name: 'a room_moved whose tag overflows 32 bits', payload: frame([0xc2, 0x81, 0x80, 0x80, 0x30], varint(opaque.length + 2), delimited(2, bytes(opaque))), sdk: opaque, want: [opaque] },
    { name: 'an unknown field whose tag overflows 32 bits, then a refresh_token', payload: frame([0xa2, 0x86, 0x80, 0x80, 0x50, 0x00], refresh(opaque)), sdk: opaque, want: [opaque] },
    // A field the SDK knows is read by its declared type, whatever its tag's wire type.
    { name: 'a refresh_token tagged as a varint', payload: frame(tag(16, 0), varint(opaque.length), bytes(opaque)), sdk: opaque, want: [opaque] },
    { name: 'a refresh_token tagged as a group', payload: frame(tag(16, 3), varint(opaque.length), bytes(opaque)), sdk: opaque, want: [opaque] },
    { name: 'an unknown varint longer than 10 bytes, then a refresh_token', payload: frame(tag(7, 0), new Array<number>(12).fill(0x80), [0], refresh(opaque)), sdk: opaque, want: [opaque] },
    {
      name: 'a room_moved token that runs past the room_moved, which the frame goes on after',
      payload: frame([...tag(24, 2), 2], tag(2, 2), varint(opaque.length), bytes(opaque), number(100, 1)),
      sdk: opaque,
      want: [opaque],
    },
    // Where the SDK throws it reads nothing; the scan still finds a JWT.
    { name: 'a group closed by another field number', payload: frame(group(100, [], 101), refresh(token)), sdk: undefined, want: [token, signed] },
    { name: 'a lone end group', payload: frame(tag(100, 4), refresh(token)), sdk: undefined, want: [token, signed] },
    { name: 'wire type 6', payload: frame(tag(100, 6), refresh(token)), sdk: undefined, want: [token, signed] },
    { name: 'field number 0', payload: frame([0, 0], refresh(token)), sdk: undefined, want: [token, signed] },
    { name: 'a varint tag longer than 10 bytes', payload: frame(new Array<number>(10).fill(0x80), [0], refresh(token)), sdk: undefined, want: [token, signed] },
    { name: 'a refresh_token whose length runs past the frame', payload: frame(tag(16, 2), varint(opaque.length + 1), bytes(opaque)), sdk: undefined, want: [] },
    { name: 'a truncated frame', payload: frame(refresh(token)).subarray(0, 10), sdk: undefined, want: [] },
    { name: 'an empty frame', payload: new Uint8Array(), sdk: undefined, want: [] },
    // Fields that hold no token.
    { name: 'a join response (field 1) holding no JWT', payload: frame(delimited(1, delimited(2, bytes(opaque)))), sdk: undefined, want: [] },
    { name: 'a string in another field is not a token', payload: frame(delimited(4, bytes(opaque))), sdk: undefined, want: [] },
    { name: 'an empty refresh_token', payload: frame(delimited(16, [])), sdk: '', want: [] },
    // Anything JWT-shaped anywhere is taken as a token: a message the SDK reads
    // by its own schema can end it somewhere other than where the walk does.
    { name: 'a JWT in a join response', payload: frame(delimited(1, delimited(2, bytes(token)))), sdk: undefined, want: [token, signed] },
    { name: 'a JWT in an unknown field', payload: frame(delimited(100, bytes(token))), sdk: undefined, want: [token, signed] },
    { name: 'a repeat is given once', payload: frame(refresh(token), refresh(token)), sdk: token, want: [token] },
  ];
  for (const c of cases) {
    it(c.name, () => {
      const got = tokensInSignal(c.payload);
      if (c.sdk !== undefined && c.sdk !== '') expect(got, 'the token the SDK reads').toContain(c.sdk);
      expect(got).toEqual(c.want);
    });
  }

  // In a binary frame the byte after a token can read as base64url (here the
  // tag of field 9, a varint: "H"), which the scan takes into the signature;
  // the signed part it gives as well is in any line that shows the token.
  it('a JWT the scan reads too long still matches a line that shows the token', () => {
    const got = tokensInSignal(frame(delimited(100, bytes(token)), number(9, 1)));
    expect(got).toEqual([`${token}H`, signed]);
    const line = `WebSocket connection to 'ws://x/rtc?access_token=${token}' failed`;
    expect(got.some((s) => line.includes(s))).toBe(true);
  });
});

describe('tokensInSignal, JSON frames', () => {
  const cases: { name: string; payload: string; want: string[] }[] = [
    { name: 'a refreshToken', payload: JSON.stringify({ refreshToken: token }), want: [token] },
    { name: 'a refresh_token, by its proto name', payload: JSON.stringify({ refresh_token: token }), want: [token] },
    { name: 'a refreshToken that is not JWT-shaped', payload: JSON.stringify({ refreshToken: opaque }), want: [opaque] },
    { name: 'a refreshToken with escaped characters', payload: `{"refreshToken":"\\u0065${token.slice(1)}"}`, want: [token] },
    { name: 'a roomMoved token', payload: JSON.stringify({ roomMoved: { room: { name: 'r' }, token } }), want: [token] },
    { name: 'a room_moved token, by its proto name', payload: JSON.stringify({ room_moved: { token } }), want: [token] },
    { name: 'a pong holds none', payload: JSON.stringify({ pongResp: { lastPingTimestamp: '1' } }), want: [] },
    { name: 'a JWT in another field', payload: JSON.stringify({ join: { x: token } }), want: [token, signed] },
    { name: 'text that is not JSON, holding a JWT', payload: `refreshToken ${token}`, want: [token, signed] },
    { name: 'text that is not JSON', payload: `refreshToken ${opaque}`, want: [] },
    { name: 'JSON that is not an object, holding a JWT', payload: JSON.stringify(token), want: [token, signed] },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(tokensInSignal(c.payload)).toEqual(c.want);
    });
  }
});
