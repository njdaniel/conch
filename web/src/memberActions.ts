// The one action table for a person on the server (ADR-014 design §10.2,
// issue #1020, and #972): what the viewer is offered on a row of the Members
// section and of the member panel. An item is offered only when whoami says
// the viewer holds what it needs and the target is not protected from them
// (design §4.2). The browser only hides: the server decides every request
// again and answers a refusal by its code.
//
//   Action                          Needs
//   Change role                     owner
//   Voice on or off                 manage_members
//   Member links on or off          manage_members
//   Sign out everywhere             manage_members
//   Remove from server              manage_members
//   Disable / enable                manage_members
//   Sign-in link as this person     act_as_members
//   Credential, hook as the person  act_as_members
//   Add to channel                  manage_channels
//   Preview as member               manage_members and manage_channels
//
// Protection, which keys on the target and not on the viewer's authority:
// nobody but an owner acts on an owner; a sign-in link never targets an
// operator (an owner is one); a person's own row offers none of these, since
// the server refuses or the page would sign its own person out. An agent has
// no role, no voice and no member links, and no link to sign in with.
import { canDo } from './admin/permissions';
import type { Permission, PermissionRowV1 } from './schema/permission';
import type { ServerMemberV1, WhoAmIResponseV1 } from './schema/principal';

export type MemberActionId = 'preview' | 'voice' | 'invites' | 'channel' | 'link' | 'credential' | 'hook' | 'role' | 'signout' | 'disable' | 'enable' | 'remove';

export interface MemberAction {
  readonly id: MemberActionId;
  readonly label: string;
  // Asks first, in place, before anything is sent.
  readonly destructive: boolean;
  readonly needs: Permission | 'owner';
}

export const isOwner = (who: WhoAmIResponseV1): boolean => who.role === 'operator' && who.owner === true;

// memberActions is what who is offered on target, in menu order.
export function memberActions(who: WhoAmIResponseV1, target: ServerMemberV1): readonly MemberAction[] {
  if (target.id === who.id) return [];
  const owner = isOwner(who);
  if (target.owner === true && !owner) return [];
  const human = target.kind === 'human';
  const targetOperator = target.role === 'operator' || target.owner === true;
  const members = canDo(who, 'manage_members');
  const acting = canDo(who, 'act_as_members');
  const items: MemberAction[] = [];
  const add = (id: MemberActionId, label: string, needs: Permission | 'owner', destructive = false) => {
    items.push({ id, label, destructive, needs });
  };

  // Preview as member (docs/design/permission-preview.md §3, §7.2): a person,
  // never an agent, for a holder of both permissions. Nobody previews
  // themself, and an owner is previewed by an owner alone (both above). The
  // server decides again.
  if (members && human && canDo(who, 'manage_channels')) add('preview', 'Preview as member', 'manage_members');

  // The brakes: voice binds members and operators, never an owner; member
  // links bind members alone (design §5).
  if (members && human && target.owner !== true) add('voice', target.voice_allowed === false ? 'Turn voice on' : 'Turn voice off', 'manage_members');
  if (members && human && target.role === 'member') add('invites', target.may_invite === false ? 'Allow member links' : 'Stop member links', 'manage_members');
  if (canDo(who, 'manage_channels')) add('channel', 'Add to a channel…', 'manage_channels');
  if (acting && human && !targetOperator) add('link', 'Make a link that signs in as this person…', 'act_as_members');
  const whom = human ? 'person' : 'agent';
  if (acting) add('credential', `Make a credential that acts as this ${whom}…`, 'act_as_members');
  if (acting) add('hook', `Make a hook that posts as this ${whom}…`, 'act_as_members');
  if (owner && human) add('role', 'Change role…', 'owner');
  if (members) {
    add('signout', 'Sign out everywhere…', 'manage_members', true);
    // The directory does not say who is disabled, so both are offered; each
    // can be repeated.
    add('disable', 'Disable sign-in…', 'manage_members', true);
    add('enable', 'Enable sign-in', 'manage_members');
    add('remove', 'Remove from server…', 'manage_members', true);
  }
  return items;
}

// voiceRoleOff reports whether the permissions document has voice off for the
// role of target (the members' cell for a member), so the voice switch is
// replaced by a line that says why it would do nothing (design §5). It says
// false where the document was not read or has no such row.
export function voiceRoleOff(target: ServerMemberV1, rows: readonly PermissionRowV1[] | null): boolean {
  if (rows === null || target.kind !== 'human' || target.owner === true) return false;
  const row = rows.find((r) => r.permission === 'use_voice');
  if (row === undefined) return false;
  if (target.role === 'member') return !row.member;
  if (target.role === 'operator') return !row.operator;
  return false;
}

// The line beside the voice item when the role's cell is off.
export const voiceOffLine = (target: ServerMemberV1): string => `Voice is off for all ${target.role === 'operator' ? 'operators' : 'members'} (Roles and permissions)`;
