import { describe, expect, it } from 'vitest';
import { memberActions, voiceRoleOff, type MemberActionId } from './memberActions';
import type { Permission, PermissionRowV1 } from './schema/permission';
import type { ServerMemberV1, WhoAmIResponseV1 } from './schema/principal';

const at = '2026-01-01T00:00:00Z';

function viewer(role: 'member' | 'operator', owner: boolean, permissions: readonly Permission[]): WhoAmIResponseV1 {
  return { id: 1, kind: 'human', name: 'viewer', role, ...(owner ? { owner: true } : {}), permissions: [...permissions] };
}

const everything: readonly Permission[] = ['manage_members', 'act_as_members', 'manage_channels', 'make_member_links'];
const owner = viewer('operator', true, []);
const operator = viewer('operator', false, everything);
const lean = viewer('operator', false, ['manage_members']);
const bare = viewer('operator', false, []);
const plain = viewer('member', false, []);

const member: ServerMemberV1 = { id: 10, kind: 'human', name: 'member-1', role: 'member', created_at: at, voice_allowed: true, may_invite: true };
const otherOperator: ServerMemberV1 = { id: 11, kind: 'human', name: 'operator-1', role: 'operator', created_at: at, voice_allowed: true };
const otherOwner: ServerMemberV1 = { id: 12, kind: 'human', name: 'owner-2', role: 'operator', owner: true, created_at: at };
const agent: ServerMemberV1 = { id: 13, kind: 'agent', name: 'builder', created_at: at };

const ids = (who: WhoAmIResponseV1, target: ServerMemberV1): MemberActionId[] => memberActions(who, target).map((a) => a.id);

describe('the member action table', () => {
  // The table of design §10.2 over the viewer and the kind of target.
  const cases: readonly { readonly name: string; readonly who: WhoAmIResponseV1; readonly target: ServerMemberV1; readonly expected: readonly MemberActionId[] }[] = [
    {
      name: 'the owner on a member: everything',
      who: owner,
      target: member,
      expected: ['preview', 'voice', 'invites', 'channel', 'link', 'credential', 'hook', 'role', 'signout', 'disable', 'enable', 'remove'],
    },
    {
      name: 'an operator holding everything on a member: all but the role',
      who: operator,
      target: member,
      expected: ['preview', 'voice', 'invites', 'channel', 'link', 'credential', 'hook', 'signout', 'disable', 'enable', 'remove'],
    },
    {
      name: 'an operator with manage_members alone on a member: no acting as, no channels',
      who: lean,
      target: member,
      expected: ['voice', 'invites', 'signout', 'disable', 'enable', 'remove'],
    },
    { name: 'an operator with no permission on a member', who: bare, target: member, expected: [] },
    { name: 'a member on a member', who: plain, target: member, expected: [] },
    {
      name: 'the owner on an operator: no sign-in link (an operator is never a target of one), and no link brake (it binds members)',
      who: owner,
      target: otherOperator,
      expected: ['preview', 'voice', 'channel', 'credential', 'hook', 'role', 'signout', 'disable', 'enable', 'remove'],
    },
    {
      name: 'an operator on an operator',
      who: operator,
      target: otherOperator,
      expected: ['preview', 'voice', 'channel', 'credential', 'hook', 'signout', 'disable', 'enable', 'remove'],
    },
    {
      name: 'the owner on another owner: no brakes (an owner has none), no link',
      who: owner,
      target: otherOwner,
      expected: ['preview', 'channel', 'credential', 'hook', 'role', 'signout', 'disable', 'enable', 'remove'],
    },
    { name: 'an operator on an owner: nobody but an owner acts on an owner', who: operator, target: otherOwner, expected: [] },
    {
      name: 'the owner on an agent: no voice, no link brake, no role, no sign-in link',
      who: owner,
      target: agent,
      expected: ['channel', 'credential', 'hook', 'signout', 'disable', 'enable', 'remove'],
    },
    {
      name: 'an operator on an agent',
      who: operator,
      target: agent,
      expected: ['channel', 'credential', 'hook', 'signout', 'disable', 'enable', 'remove'],
    },
    {
      name: 'preview needs both manage_members and manage_channels: one alone is not enough',
      who: viewer('operator', false, ['manage_channels']),
      target: member,
      expected: ['channel'],
    },
    { name: 'the owner on themself', who: { ...owner, id: 10 }, target: member, expected: [] },
    { name: 'an operator on themself', who: { ...operator, id: 10 }, target: member, expected: [] },
  ];

  it.each(cases)('$name', ({ who, target, expected }) => {
    expect(ids(who, target)).toEqual(expected);
  });

  it('offers only the permissions whoami lists; an owner needs none listed', () => {
    for (const permission of ['manage_members', 'act_as_members', 'manage_channels'] as const) {
      const without = viewer('operator', false, everything.filter((p) => p !== permission));
      const held = ids(operator, member);
      const lost = ids(without, member);
      expect(lost.length, permission).toBeLessThan(held.length);
    }
    expect(ids(viewer('operator', true, []), member)).toContain('remove');
  });

  it('words the acting-as items as acting as the person', () => {
    const labels = Object.fromEntries(memberActions(owner, member).map((a) => [a.id, a.label]));
    expect(labels.link).toBe('Make a link that signs in as this person…');
    expect(labels.credential).toBe('Make a credential that acts as this person…');
    expect(labels.hook).toBe('Make a hook that posts as this person…');
  });

  it('says agent for an agent', () => {
    const labels = Object.fromEntries(memberActions(owner, agent).map((a) => [a.id, a.label]));
    expect(labels.credential).toBe('Make a credential that acts as this agent…');
    expect(labels.hook).toBe('Make a hook that posts as this agent…');
  });

  it('names the brake in the direction it would turn', () => {
    const on = memberActions(owner, member).find((a) => a.id === 'voice');
    const off = memberActions(owner, { ...member, voice_allowed: false }).find((a) => a.id === 'voice');
    expect(on?.label).toBe('Turn voice off');
    expect(off?.label).toBe('Turn voice on');
    const links = memberActions(owner, { ...member, may_invite: false }).find((a) => a.id === 'invites');
    expect(links?.label).toBe('Allow member links');
  });

  it('marks the destructive actions', () => {
    const destructive = memberActions(owner, member)
      .filter((a) => a.destructive)
      .map((a) => a.id);
    expect(destructive).toEqual(['signout', 'disable', 'remove']);
  });

  it('tolerates a server that predates permissions: an operator holds them all, a member none', () => {
    const old: WhoAmIResponseV1 = { id: 1, kind: 'human', name: 'viewer', role: 'operator' };
    expect(ids(old, member)).toContain('remove');
    expect(ids({ ...old, role: 'member' }, member)).toEqual([]);
  });
});

describe('the voice line', () => {
  const row = (member: boolean, operator: boolean): readonly PermissionRowV1[] => [
    { permission: 'create_public_channels', member: true, operator: true },
    { permission: 'use_voice', member, operator },
  ];

  it('says voice is off for all members when the role cell is off, and only for the role it is off for', () => {
    expect(voiceRoleOff(member, row(false, true))).toBe(true);
    expect(voiceRoleOff(member, row(true, false))).toBe(false);
    expect(voiceRoleOff(otherOperator, row(false, true))).toBe(false);
    expect(voiceRoleOff(otherOperator, row(true, false))).toBe(true);
    expect(voiceRoleOff(otherOwner, row(false, false))).toBe(false);
    expect(voiceRoleOff(agent, row(false, false))).toBe(false);
  });

  it('says nothing when the document was not read or has no such row', () => {
    expect(voiceRoleOff(member, null)).toBe(false);
    expect(voiceRoleOff(member, [])).toBe(false);
  });
});
