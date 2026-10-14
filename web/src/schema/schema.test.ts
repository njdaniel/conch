// Checks the hand-written TypeScript wire types against pkg/schema's golden
// fixtures, the same files the Go tests decode. A type that has drifted from
// Go fails here; a validator that has drifted from its type fails the
// typecheck.
import { describe, expect, it } from 'vitest';
import { listChannelMembersResponseV1, listChannelsResponse, channelMemberV1, channelV0 } from './channel';
import { messageV2 } from './message';
import { whoAmIResponseV1 } from './principal';
import type { Validator } from './validate';

// Read by relative path, so the test sees the fixtures of this checkout.
const fixtures = import.meta.glob<unknown>('../../../pkg/schema/testdata/*.json', {
  eager: true,
  import: 'default',
});

const byName = new Map(
  Object.entries(fixtures).map(([path, doc]) => [path.slice(path.lastIndexOf('/') + 1), doc]),
);

// Each fixture family that has a TypeScript type. A family is a base name and
// its variants: message-v2 is message-v2.json and every message-v2-*.json, so
// a fixture Go adds later is checked without a change here.
const families: [string, Validator<unknown>][] = [
  ['whoami-response-v1', whoAmIResponseV1],
  ['channel-v0', channelV0],
  ['list-channels-response', listChannelsResponse],
  ['message-v2', messageV2],
  ['channel-member-v1', channelMemberV1],
  ['list-channel-members-response-v1', listChannelMembersResponseV1],
];

function inFamily(file: string, family: string): boolean {
  return file === `${family}.json` || (file.startsWith(`${family}-`) && file.endsWith('.json'));
}

describe('golden fixtures', () => {
  it('are found', () => {
    expect(byName.size).toBeGreaterThan(0);
  });

  const cases = families.flatMap(([family, check]) =>
    [...byName.keys()].filter((file) => inFamily(file, family)).map((file) => ({ family, file, check })),
  );

  it.each(families.map(([family]) => family))('family %s has at least one fixture', (family) => {
    expect(cases.some((c) => c.family === family)).toBe(true);
  });

  it.each(cases)('$file matches its TypeScript type', ({ file, check }) => {
    expect(check(byName.get(file))).toEqual([]);
  });
});

// The validators must be able to fail, or the test above proves nothing.
describe('validators refuse', () => {
  const whoami = { id: 1, kind: 'human', name: 'nick', role: 'operator' };
  const message = {
    schema: 'conch.message.v2',
    id: 42,
    channel_id: 7,
    author_id: 3,
    created_at: '2026-07-13T12:34:56.789Z',
    body: 'hello',
  };

  const refusals: { name: string; check: Validator<unknown>; value: unknown; want: string }[] = [
    { name: 'a missing required key', check: whoAmIResponseV1, value: { id: 1, kind: 'human', name: 'nick' }, want: '$.role: required key is missing' },
    { name: 'a value of the wrong type', check: whoAmIResponseV1, value: { ...whoami, id: '1' }, want: '$.id: want an integer' },
    { name: 'a fractional id', check: whoAmIResponseV1, value: { ...whoami, id: 1.5 }, want: '$.id: want an integer' },
    { name: 'a value outside a closed vocabulary', check: whoAmIResponseV1, value: { ...whoami, role: 'admin' }, want: '$.role: want one of operator, member' },
    { name: 'a key the type does not have', check: whoAmIResponseV1, value: { ...whoami, extra: true }, want: '$.extra: key is not in the TypeScript type' },
    { name: 'something that is not an object', check: whoAmIResponseV1, value: [whoami], want: '$: want an object, got an array' },
    { name: 'a malformed timestamp', check: channelV0, value: { id: 7, name: 'general', created_at: '13 July 2026' }, want: '$.created_at: want an RFC 3339 timestamp' },
    { name: 'a bad element in a list', check: listChannelsResponse, value: { channels: [{ id: 7, name: 3, created_at: '2026-07-13T12:34:56Z' }] }, want: '$.channels[0].name: want a string' },
    { name: 'another envelope version', check: messageV2, value: { ...message, schema: 'conch.message.v1' }, want: '$.schema: want one of conch.message.v2' },
    { name: 'an unknown audience kind', check: messageV2, value: { ...message, audience: { kind: 'channel' } }, want: '$.audience.kind: want one of net, principals' },
    { name: 'a net audience listing principals', check: messageV2, value: { ...message, audience: { kind: 'net', net_id: 3, principal_ids: [3] } }, want: '$.audience.principal_ids: key is not in the TypeScript type' },
    { name: 'a principals audience without its list', check: messageV2, value: { ...message, audience: { kind: 'principals' } }, want: '$.audience.principal_ids: required key is missing' },
    { name: 'an optional key of the wrong type', check: channelMemberV1, value: { principal_id: 3, created_at: '2026-10-08T12:00:00.000Z', kind: 'robot' }, want: '$.kind: want one of human, agent' },
  ];

  it.each(refusals)('$name', ({ check, value, want }) => {
    const problems = check(value);
    expect(problems.some((p) => p.startsWith(want)), problems.join('; ')).toBe(true);
  });
});
