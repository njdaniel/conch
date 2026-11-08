import type { ChannelMemberV1 } from './schema/channel';
import type { PrincipalKind, PrincipalRole } from './schema/principal';

export type IdentityKey = 'you' | 'agent' | 'owner' | 'operator' | 'manager' | 'approver';

export interface IdentityMark {
  readonly key: IdentityKey;
  readonly label: string;
}

export interface IdentityInput {
  readonly id?: number;
  readonly viewer?: number;
  readonly kind?: PrincipalKind | null;
  readonly owner?: boolean;
  readonly role?: PrincipalRole | null;
  readonly manages?: boolean;
  readonly can_decide?: boolean;
  readonly guest?: ChannelMemberV1['guest'] | null;
}

const marks: readonly IdentityMark[] = [
  { key: 'you', label: 'You' },
  { key: 'agent', label: 'Agent' },
  { key: 'owner', label: 'Owner' },
  { key: 'operator', label: 'Operator' },
  { key: 'manager', label: 'Manager' },
  { key: 'approver', label: 'Approver' },
];

// identityMarks maps only fields the caller already received, in the one order
// used by every list. A guest or an unknown kind has no human standing. The
// textual form for screen readers and a later TUI issue is [you], [agent],
// [owner], [operator], [manager] and [approver], in that same order.
export function identityMarks(input: IdentityInput): IdentityMark[] {
  const present = new Set<IdentityKey>();
  if (input.id !== undefined && input.id === input.viewer) present.add('you');
  if (input.kind === 'agent') present.add('agent');
  if (input.kind === 'human' && input.guest === undefined) {
    if (input.owner === true) present.add('owner');
    if (input.role === 'operator') present.add('operator');
    if (input.manages === true) present.add('manager');
    if (input.can_decide === true) present.add('approver');
  }
  return marks.filter((mark) => present.has(mark.key));
}

export function identityText(mark: IdentityMark): string {
  return `[${mark.key}]`;
}
