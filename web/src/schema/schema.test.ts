// Checks the TypeScript wire types, generated from pkg/schema by
// `go generate ./pkg/schema`, against pkg/schema's golden fixtures, the same
// files the Go tests decode. A generated validator that disagrees with Go
// fails here; one that disagrees with its type fails the typecheck.
import { describe, expect, it } from 'vitest';
import {
  agentActivityV1,
  approvalV1,
  channelMemberV1,
  channelV0,
  errorV0,
  escalationTarget,
  health,
  interventionV1,
  joinSettingsResponseV1,
  listChannelsResponse,
  listMessagesResponseV2,
  messageV2,
  permission,
  permissionsResponseV1,
  postMessageResponseV2,
  runV1,
  sessionRosterV1,
  sessionV1,
  whoAmIResponseV1,
  wsTicketV1,
  type JoinSettingsResponseV1,
  type PermissionsResponseV1,
  type WhoAmIResponseV1,
} from './generated';
import { families } from './generated.families';
import { setStrictKeys, type Validator } from './validate';

// The fixtures are checked strictly: a key the type lacks fails here, though
// at runtime it is ignored (issue #1534).
setStrictKeys(true);

// Read by relative path, so the test sees the fixtures of this checkout.
const fixtures = import.meta.glob<unknown>('../../../pkg/schema/testdata/*.json', {
  eager: true,
  import: 'default',
});

const byName = new Map(
  Object.entries(fixtures).map(([path, doc]) => [path.slice(path.lastIndexOf('/') + 1), doc]),
);

// families, generated from pkg/schema/ts.go, is each fixture family that has
// a TypeScript type. A family is a base name and its variants: message-v2 is
// message-v2.json and every message-v2-*.json, so a fixture Go adds later is
// checked without a change here.
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

  const session = {
    schema: 'conch.session.v1',
    id: 11,
    principal_id: 1,
    kind: 'human',
    name: 'Nick',
    client: 'browser',
    state: 'online',
    status: 'online',
    started_at: '2026-10-13T08:58:12.000Z',
    last_seen_at: '2026-10-13T09:05:00.000Z',
    interventions: [],
    outcome_unknown: false,
  };

  const refusals: { name: string; check: Validator<unknown>; value: unknown; want: string }[] = [
    { name: 'a missing required key', check: whoAmIResponseV1, value: { id: 1, kind: 'human', name: 'nick' }, want: '$.role: required key is missing' },
    { name: 'a value of the wrong type', check: whoAmIResponseV1, value: { ...whoami, id: '1' }, want: '$.id: want an integer' },
    { name: 'a fractional id', check: whoAmIResponseV1, value: { ...whoami, id: 1.5 }, want: '$.id: want an integer' },
    { name: 'a value outside a closed vocabulary', check: whoAmIResponseV1, value: { ...whoami, role: 'admin' }, want: '$.role: want one of operator, member' },
    { name: 'a key the type does not have', check: whoAmIResponseV1, value: { ...whoami, extra: true }, want: '$.extra: key is not in the TypeScript type' },
    { name: 'an inherited name as a key', check: whoAmIResponseV1, value: JSON.parse(`{"id":1,"kind":"human","name":"nick","role":"operator","constructor":1}`) as unknown, want: '$.constructor: key is not in the TypeScript type' },
    { name: 'something that is not an object', check: whoAmIResponseV1, value: [whoami], want: '$: want an object, got an array' },
    { name: 'a malformed timestamp', check: channelV0, value: { id: 7, name: 'general', created_at: '13 July 2026' }, want: '$.created_at: want an RFC 3339 timestamp' },
    { name: 'a bad element in a list', check: listChannelsResponse, value: { channels: [{ id: 7, name: 3, created_at: '2026-07-13T12:34:56Z' }] }, want: '$.channels[0].name: want a string' },
    { name: 'another envelope version', check: messageV2, value: { ...message, schema: 'conch.message.v1' }, want: '$.schema: want one of conch.message.v2' },
    { name: 'an unknown audience kind', check: messageV2, value: { ...message, audience: { kind: 'channel' } }, want: '$.audience.kind: want one of net, principals' },
    { name: 'a net audience listing principals', check: messageV2, value: { ...message, audience: { kind: 'net', net_id: 3, principal_ids: [3] } }, want: '$.audience.principal_ids: key is not in the TypeScript type' },
    { name: 'a principals audience without its list', check: messageV2, value: { ...message, audience: { kind: 'principals' } }, want: '$.audience.principal_ids: required key is missing' },
    { name: 'an optional key of the wrong type', check: channelMemberV1, value: { principal_id: 3, created_at: '2026-10-08T12:00:00.000Z', kind: 'robot' }, want: '$.kind: want one of human, agent' },
    { name: 'a ticket without its expiry', check: wsTicketV1, value: { ticket: 'conch_wst_x' }, want: '$.expires_at: required key is missing' },
    { name: 'an escalation to a principal naming a topic too', check: escalationTarget, value: { kind: 'principal', principal_id: 3, topic: 't' }, want: '$.topic: key is not in the TypeScript type' },
    { name: 'an escalation to a topic without one', check: escalationTarget, value: { kind: 'ntfy_topic' }, want: '$.topic: required key is missing' },
    { name: 'an approval in a state it cannot have', check: approvalV1, value: { state: 'maybe' }, want: '$.state: want one of pending, escalated, resolved, expired' },
    { name: 'a post response without a message', check: postMessageResponseV2, value: {}, want: '$.message: required key is missing' },
    { name: 'a session status outside the vocabulary', check: sessionV1, value: { ...session, status: 'busy' }, want: '$.status: want one of online, idle' },
    { name: 'a session without outcome_unknown', check: sessionV1, value: { ...session, outcome_unknown: undefined }, want: '$.outcome_unknown: want a boolean' },
    { name: 'a roster of another schema', check: sessionRosterV1, value: { schema: 'conch.session.v1', channel_id: 7, sessions: [] }, want: '$.schema: want one of conch.session_roster.v1' },
    { name: 'a run in a state it cannot have', check: runV1, value: { schema: 'conch.run.v1', id: 1, session_id: 1, principal_id: 1, channel_id: 1, state: 'paused', task: 't', started_at: '2026-10-13T09:01:00Z' }, want: '$.state: want one of queued' },
    { name: 'an intervention state outside the vocabulary', check: interventionV1, value: { schema: 'conch.intervention.v1', id: 1, run_id: 1, kind: 'cancel', state: 'done', requested_by: 1, requested_at: '2026-10-13T09:01:00Z' }, want: '$.state: want one of requested' },
    { name: 'an activity kind outside the vocabulary', check: agentActivityV1, value: { schema: 'conch.agent.activity.v1', id: 1, run_id: 1, principal_id: 1, kind: 'dance', summary: 's', at: '2026-10-13T09:01:00Z' }, want: '$.kind: want one of run_started' },
    { name: 'a page with a bad message', check: listMessagesResponseV2, value: { messages: [{ ...message, id: 'x' }] }, want: '$.messages[0].id: want an integer' },
    // server_mode (ADR-011, issue #865): normal mode omits the field, so
    // "normal" is never a wire value and only "temporary" is one.
    { name: 'a server mode that is said by omission', check: whoAmIResponseV1, value: { ...whoami, server_mode: 'normal' }, want: '$.server_mode: want one of temporary' },
    { name: 'a health answer with a server mode that is not one', check: health, value: { status: 'ok', version: 'v1', db: 'ok', voice: 'not_configured', server_mode: 'normal' }, want: '$.server_mode: want one of temporary' },
  ];

  it.each(refusals)('$name', ({ check, value, want }) => {
    const problems = check(value);
    expect(problems.some((p) => p.startsWith(want)), problems.join('; ')).toBe(true);
  });
});

// A closed vocabulary the client tolerates (issue #1051): Permission,
// PermissionsWarning, JoinWarning and the others pkg/schema/ts.go marks
// tolerant. A bundle built before a value shipped reads a document that
// carries it, and keeps the value, so a tab left open across an upgrade
// still signs in. A strict vocabulary beside it refuses as before.
describe('tolerant vocabularies', () => {
  // A permission no bundle knows: the twelfth and every later one look like
  // this to a bundle built before them.
  const future = 'fly_the_server';
  const whoami = byName.get('whoami-response-v1-permissions.json') as WhoAmIResponseV1;
  const permissions = byName.get('permissions-response-v1.json') as PermissionsResponseV1;
  const joining = byName.get('join-settings-response-v1.json') as JoinSettingsResponseV1;

  it('reads the fixtures this test builds on', () => {
    expect(whoami.permissions).toBeDefined();
    expect(permissions.rows.length).toBeGreaterThan(0);
    expect(joining.warnings).toBeDefined();
  });

  const accepted: { name: string; check: Validator<unknown>; value: unknown }[] = [
    { name: 'a whoami naming an unknown permission', check: whoAmIResponseV1, value: { ...whoami, permissions: [...(whoami.permissions ?? []), future] } },
    { name: 'a refusal naming an unknown permission', check: errorV0, value: { code: 'permission_denied', message: 'no', permission: future } },
    {
      name: 'a permissions document with an unknown permission, warning, preset and column',
      check: permissionsResponseV1,
      value: {
        ...permissions,
        rows: [...permissions.rows, { permission: future, member: false, operator: true }],
        warnings: [...permissions.warnings, 'future_warning'],
        presets: [...permissions.presets, { preset: 'future_preset', member: [future] }],
        editable: [...permissions.editable, 'future_role'],
        issued_to_others: [{ kind: 'future_kind', id: 1, principal_id: 2, issued_by: 3, issued_at: '2026-10-23T12:00:00.000Z' }],
      },
    },
    { name: 'joining settings with an unknown warning', check: joinSettingsResponseV1, value: { ...joining, warnings: [...joining.warnings, 'future_warning'] } },
  ];

  it.each(accepted)('accepts $name', ({ check, value }) => {
    expect(check(value)).toEqual([]);
  });

  it('keeps the value as it came', () => {
    const doc = { ...whoami, permissions: [future] };
    expect(whoAmIResponseV1(doc)).toEqual([]);
    expect(doc.permissions).toEqual([future]);
  });

  const refused: { name: string; check: Validator<unknown>; value: unknown; want: string }[] = [
    { name: 'an empty string', check: permission, value: '', want: '$: want one of create_public_channels' },
    { name: 'something that is not a string', check: permission, value: 3, want: '$: want one of create_public_channels' },
    { name: 'a null', check: permission, value: null, want: '$: want one of create_public_channels' },
    { name: 'a strict vocabulary beside a tolerated value', check: whoAmIResponseV1, value: { ...whoami, permissions: [future], role: 'admin' }, want: '$.role: want one of operator, member' },
  ];

  it.each(refused)('still refuses $name', ({ check, value, want }) => {
    const problems = check(value);
    expect(problems.some((p) => p.startsWith(want)), problems.join('; ')).toBe(true);
  });
});
