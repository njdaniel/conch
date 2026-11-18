// Reading the tokens LiveKit hands a page from its signalling frames (issue
// #1948). A unit test: no browser, no network. The frames are encoded here by
// hand, field by field as livekit_rtc.proto numbers them.
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
// A length-delimited field (wire type 2), a varint field (0) and a fixed32 (5).
const delimited = (no: number, body: number[]) => [...varint(no * 8 + 2), ...varint(body.length), ...body];
const number = (no: number, n: number) => [...varint(no * 8), ...varint(n)];
const fixed32 = (no: number) => [...varint(no * 8 + 5), 1, 2, 3, 4];
const frame = (...parts: number[][]) => new Uint8Array(parts.flat());

const token = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwMSJ9.c2lnbmF0dXJl';

describe('tokensInSignal', () => {
  const cases: { name: string; payload: string | Uint8Array; want: string[] }[] = [
    { name: 'a binary refresh_token (field 16)', payload: frame(delimited(16, bytes(token))), want: [token] },
    {
      name: 'a binary room_moved (field 24) with its token (field 2) among a room and a participant',
      payload: frame(delimited(24, [...delimited(1, bytes('room')), ...delimited(2, bytes(token)), ...delimited(3, [...number(1, 7)])])),
      want: [token],
    },
    {
      name: 'a refresh_token after fields of every other wire type',
      payload: frame(number(18, 1234567890123), fixed32(20), [...varint(19 * 8 + 1), 1, 2, 3, 4, 5, 6, 7, 8], delimited(16, bytes(token))),
      want: [token],
    },
    { name: 'a join response (field 1) holds none', payload: frame(delimited(1, delimited(2, bytes(token)))), want: [] },
    { name: 'a string in another field is not a token', payload: frame(delimited(4, bytes(token))), want: [] },
    { name: 'an empty refresh_token', payload: frame(delimited(16, [])), want: [] },
    { name: 'a truncated frame gives none', payload: frame(delimited(16, bytes(token))).subarray(0, 10), want: [] },
    { name: 'a group wire type gives none', payload: frame([...varint(16 * 8 + 3)], delimited(16, bytes(token))), want: [] },
    { name: 'field number 0 gives none', payload: frame([0, 0], delimited(16, bytes(token))), want: [] },
    { name: 'an empty frame', payload: new Uint8Array(), want: [] },
    { name: 'a JSON refreshToken', payload: JSON.stringify({ refreshToken: token }), want: [token] },
    { name: 'a JSON refresh_token, by its proto name', payload: JSON.stringify({ refresh_token: token }), want: [token] },
    { name: 'a JSON roomMoved token', payload: JSON.stringify({ roomMoved: { room: { name: 'r' }, token } }), want: [token] },
    { name: 'a JSON room_moved token, by its proto name', payload: JSON.stringify({ room_moved: { token } }), want: [token] },
    { name: 'a JSON pong holds none', payload: JSON.stringify({ pongResp: { lastPingTimestamp: '1' } }), want: [] },
    { name: 'text that is not JSON', payload: `refreshToken ${token}`, want: [] },
    { name: 'JSON that is not an object', payload: JSON.stringify(token), want: [] },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(tokensInSignal(c.payload)).toEqual(c.want);
    });
  }
});
