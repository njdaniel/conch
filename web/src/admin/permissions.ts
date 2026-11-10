// What the administration console decides from whoami (ADR-014 design §10):
// which controls the caller is offered, the words for a permission, and the
// words for a refusal. The browser only hides or disables; the server decides
// every request again, so none of this is enforcement.
//
// Unknown permission values are tolerated: a permission a newer server
// names is shown by its name and never sent anywhere.
import type { ApiFailure } from '../api';
import type { Permission } from '../schema/permission';
import type { WhoAmIResponseV1 } from '../schema/principal';

// canDo reports whether whoami says the caller holds permission. The owner
// always does. A server that predates permissions sends none: an operator is
// then taken to hold all of them (the server answers each request itself).
export function canDo(who: Pick<WhoAmIResponseV1, 'role' | 'owner' | 'permissions'>, permission: Permission): boolean {
  if (who.role === 'operator' && who.owner === true) return true;
  if (who.permissions === undefined) return who.role === 'operator';
  return who.permissions.includes(permission);
}

// The words for each permission the page knows, as part of a sentence about
// what a person may do.
const doing: Readonly<Record<Permission, string>> = {
  create_public_channels: 'create public channels',
  create_private_channels: 'create private channels',
  make_member_links: 'make member links',
  see_member_directory: 'see everyone on the server',
  use_voice: 'use voice',
  share_screen: 'share their screen',
  manage_members: 'manage members',
  act_as_members: 'act as members',
  manage_channels: 'manage channels',
  change_server_settings: 'change server settings',
  read_audit_log: 'read the audit log',
  use_camera: 'use a camera',
};

// permissionName is a permission's words, or the value itself when this page
// has none for it (a permission a newer server added).
export function permissionName(permission: string): string {
  return doingByName.get(permission) ?? permission;
}

const doingByName: ReadonlyMap<string, string> = new Map(Object.entries(doing));

function list(items: readonly string[]): string {
  if (items.length <= 1) return items.join('');
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1] ?? ''}`;
}

// memberLine is the read-only line a member sees under "Who can administer",
// drawn from whoami.permissions: "You can create channels and make member
// links. Voice is on." Null where whoami carries no permissions (an older
// server), so the page says nothing it was not told.
export function memberLine(who: WhoAmIResponseV1): string | null {
  const held = who.permissions;
  if (held === undefined) return null;
  const has = (p: Permission) => held.includes(p);
  const things: string[] = [];
  if (has('create_public_channels') && has('create_private_channels')) things.push('create channels');
  else if (has('create_public_channels')) things.push('create public channels');
  else if (has('create_private_channels')) things.push('create private channels');
  if (has('make_member_links')) things.push('make member links');
  if (has('see_member_directory')) things.push('see everyone on the server');
  if (has('share_screen')) things.push('share your screen');
  if (has('use_camera')) things.push('turn on your camera');
  // Anything this page has no word for is named, not hidden.
  for (const p of held) if (!doingByName.has(p)) things.push(p);
  const can = things.length === 0 ? 'You can read and write in the channels you are in.' : `You can ${list(things)}.`;
  return `${can} Voice is ${has('use_voice') ? 'on' : 'off'}.`;
}

// A failed call, as the console's writes and reads hand it back.
export interface Refusal {
  readonly failure: ApiFailure;
  readonly code?: string;
  readonly message?: string;
  readonly permission?: string;
}

// refusalLine is the line for a refused or failed call, from the server's
// code and the permission it names (design §7.4). The server's message is
// other people's text as far as this page is concerned: it is never shown.
// nothing says what was left as it was.
export function refusalLine(result: Refusal, nothing: string): string {
  if (result.failure === 'network') return `Cannot reach the server. ${nothing}`;
  switch (result.code) {
    case 'permission_denied':
      return result.permission === undefined
        ? `You do not hold the permission for that. ${nothing}`
        : `You need the permission to ${permissionName(result.permission)} for that. ${nothing}`;
    case 'forbidden':
      return `The server does not allow you to do that. ${nothing}`;
    case 'reauth_required':
      return `It needs your password first. ${nothing}`;
    case 'permissions_stale':
      return `The permissions changed since this page read them. ${nothing}`;
    case 'permissions_conflict':
      return `That change would break a rule of the permissions. ${nothing}`;
    case 'join_policy_conflict':
      return `Those joining settings cannot stand together. ${nothing}`;
    case 'invalid_request':
      return `The server did not take that. ${nothing}`;
    default:
      return `The server refused. ${nothing}`;
  }
}

// reasonFor is why a control is disabled when the caller lacks permission.
export function reasonFor(permission: Permission): string {
  return `Needs the permission to ${permissionName(permission)}.`;
}
