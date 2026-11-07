import { isArchived } from './archived';
import type { ChannelV0 } from './schema/channel';

// How one person arranges their channel list on one server (issue #1493):
// favorites, named groups and the order within them. It is a view of the list
// the server answered and nothing more: grouping grants, hides and changes
// nothing about a channel. Channels are held by id, so a rename keeps its
// place; an id the viewer no longer holds is pruned on every list change and
// never rendered or counted. Pure: the kept shape is read defensively, since
// whatever is in storage may be old, edited or corrupt.

export interface Group {
  readonly id: string;
  readonly name: string;
  readonly collapsed: boolean;
  readonly channels: readonly number[];
}

export interface Arrangement {
  readonly favorites: readonly number[];
  readonly groups: readonly Group[];
}

export const emptyArrangement: Arrangement = { favorites: [], groups: [] };

export const maxGroups = 20;
export const maxGroupName = 40;

const isId = (v: unknown): v is number => typeof v === 'number' && Number.isSafeInteger(v) && v > 0;
const sameName = (a: string, b: string) => a.trim().toLowerCase() === b.trim().toLowerCase();

function ids(raw: unknown): number[] {
  return Array.isArray(raw) ? [...new Set((raw as unknown[]).filter(isId))] : [];
}

// parseArrangement reads whatever was kept. Anything that is not an
// arrangement is none; a good part of a damaged one is kept.
export function parseArrangement(raw: unknown): Arrangement {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return emptyArrangement;
  const value = raw as { favorites?: unknown; groups?: unknown };
  const groups: Group[] = [];
  const taken = new Set<number>();
  if (Array.isArray(value.groups)) {
    for (const g of value.groups as unknown[]) {
      if (typeof g !== 'object' || g === null) continue;
      const { id, name, collapsed, channels } = g as { id?: unknown; name?: unknown; collapsed?: unknown; channels?: unknown };
      if (typeof id !== 'string' || id === '' || typeof name !== 'string') continue;
      const trimmed = name.trim();
      if (trimmed === '' || trimmed.length > maxGroupName) continue;
      if (groups.some((x) => x.id === id || sameName(x.name, trimmed)) || groups.length >= maxGroups) continue;
      const mine = ids(channels).filter((c) => !taken.has(c));
      for (const c of mine) taken.add(c);
      groups.push({ id, name: trimmed, collapsed: collapsed === true, channels: mine });
    }
  }
  const favorites = ids(value.favorites);
  if (favorites.length === 0 && groups.length === 0) return emptyArrangement;
  return { favorites, groups };
}

// pruneArrangement drops every channel the viewer does not hold. It returns the
// same object when nothing was dropped.
export function pruneArrangement(a: Arrangement, channels: readonly ChannelV0[]): Arrangement {
  const held = new Set(channels.map((c) => c.id));
  const favorites = a.favorites.filter((id) => held.has(id));
  let changed = favorites.length !== a.favorites.length;
  const groups = a.groups.map((g) => {
    const kept = g.channels.filter((id) => held.has(id));
    if (kept.length === g.channels.length) return g;
    changed = true;
    return { ...g, channels: kept };
  });
  return changed ? { favorites, groups } : a;
}

export interface Layout {
  readonly favorites: readonly ChannelV0[];
  readonly groups: readonly { readonly group: Group; readonly channels: readonly ChannelV0[] }[];
  // In no favorites and no group: the list as the server gave it.
  readonly rest: readonly ChannelV0[];
}

// layoutOf sorts the live (not archived) channels into the arrangement. A
// channel shows once: in favorites if it is one, else in its group, else in
// the rest.
export function layoutOf(a: Arrangement, live: readonly ChannelV0[]): Layout {
  const byId = new Map(live.map((c) => [c.id, c]));
  const shown = new Set<number>();
  const pick = (list: readonly number[]): ChannelV0[] => {
    const out: ChannelV0[] = [];
    for (const id of list) {
      const c = byId.get(id);
      if (c !== undefined && !shown.has(id)) {
        shown.add(id);
        out.push(c);
      }
    }
    return out;
  };
  const favorites = pick(a.favorites);
  const groups = a.groups.map((group) => ({ group, channels: pick(group.channels) }));
  const grouped = new Set(a.groups.flatMap((g) => g.channels));
  const rest = live.filter((c) => !shown.has(c.id) && !grouped.has(c.id));
  return { favorites, groups, rest };
}

// visualOrder is every channel in the order the list draws it (collapsed
// groups included), archived last. The quick switcher steps through it.
export function visualOrder(a: Arrangement, channels: readonly ChannelV0[]): ChannelV0[] {
  const live = channels.filter((c) => !isArchived(c));
  const layout = layoutOf(a, live);
  return [...layout.favorites, ...layout.groups.flatMap((g) => g.channels), ...layout.rest, ...channels.filter(isArchived)];
}

export function toggleFavorite(a: Arrangement, id: number): Arrangement {
  return { ...a, favorites: a.favorites.includes(id) ? a.favorites.filter((x) => x !== id) : [...a.favorites, id] };
}

// moveWithin moves a channel one place up (-1) or down (1) among those drawn
// in its section: favorites, or its group. The same object when it cannot move.
export function moveWithin(a: Arrangement, live: readonly ChannelV0[], id: number, direction: -1 | 1): Arrangement {
  // Only what the section draws can be passed: in a group, not a favorite
  // (which is drawn under Favorites instead).
  const inFavorites = a.favorites.includes(id);
  const drawn = new Set(live.filter((c) => inFavorites || !a.favorites.includes(c.id)).map((c) => c.id));
  const swap = (list: readonly number[]): number[] | null => {
    const at = list.indexOf(id);
    if (at < 0) return null;
    for (let i = at + direction; i >= 0 && i < list.length; i += direction) {
      const other = list[i];
      if (other !== undefined && drawn.has(other)) {
        const next = [...list];
        next[at] = other;
        next[i] = id;
        return next;
      }
    }
    return null;
  };
  if (a.favorites.includes(id)) {
    const next = swap(a.favorites);
    return next === null ? a : { ...a, favorites: next };
  }
  const group = a.groups.find((g) => g.channels.includes(id));
  if (group === undefined) return a;
  const next = swap(group.channels);
  return next === null ? a : { ...a, groups: a.groups.map((g) => (g === group ? { ...g, channels: next } : g)) };
}

// moveToGroup puts a channel in one group (at its end), or in none.
export function moveToGroup(a: Arrangement, id: number, groupId: string | null): Arrangement {
  return {
    ...a,
    groups: a.groups.map((g) => {
      const without = g.channels.filter((x) => x !== id);
      return g.id === groupId ? { ...g, channels: [...without, id] } : without.length === g.channels.length ? g : { ...g, channels: without };
    }),
  };
}

// groupNameProblem says why a group name is refused, or null when it stands.
export function groupNameProblem(a: Arrangement, name: string, exceptId?: string): string | null {
  const trimmed = name.trim();
  if (trimmed === '') return 'Give the group a name.';
  if (trimmed.length > maxGroupName) return `Use ${String(maxGroupName)} characters or fewer.`;
  if (a.groups.some((g) => g.id !== exceptId && sameName(g.name, trimmed))) return 'A group already has that name.';
  return null;
}

export function addGroup(a: Arrangement, name: string): Arrangement {
  if (groupNameProblem(a, name) !== null || a.groups.length >= maxGroups) return a;
  let n = 1;
  while (a.groups.some((g) => g.id === `g${String(n)}`)) n++;
  return { ...a, groups: [...a.groups, { id: `g${String(n)}`, name: name.trim(), collapsed: false, channels: [] }] };
}

export function renameGroup(a: Arrangement, id: string, name: string): Arrangement {
  if (groupNameProblem(a, name, id) !== null) return a;
  return { ...a, groups: a.groups.map((g) => (g.id === id ? { ...g, name: name.trim() } : g)) };
}

export function toggleCollapsed(a: Arrangement, id: string): Arrangement {
  return { ...a, groups: a.groups.map((g) => (g.id === id ? { ...g, collapsed: !g.collapsed } : g)) };
}

export function expandGroup(a: Arrangement, id: string): Arrangement {
  return a.groups.some((g) => g.id === id && g.collapsed) ? toggleCollapsed(a, id) : a;
}

export function deleteGroup(a: Arrangement, id: string): Arrangement {
  return { ...a, groups: a.groups.filter((g) => g.id !== id) };
}
