import { describe, expect, it } from 'vitest';
import {
  addGroup,
  deleteGroup,
  emptyArrangement,
  groupNameProblem,
  layoutOf,
  moveToGroup,
  moveWithin,
  parseArrangement,
  pruneArrangement,
  renameGroup,
  expandGroup,
  toggleCollapsed,
  toggleFavorite,
  visualOrder,
  type Arrangement,
} from './arrangement';
import type { ChannelV0 } from './schema/channel';

const ch = (id: number, name: string, extra: Partial<ChannelV0> = {}): ChannelV0 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z', ...extra });
const channels = [ch(1, 'general'), ch(2, 'games'), ch(3, 'books'), ch(4, 'music'), ch(5, 'old', { archived_at: '2026-10-19T00:00:00.000Z' })];
const names = (list: readonly ChannelV0[]) => list.map((c) => c.name);

describe('parseArrangement', () => {
  it('reads a kept arrangement', () => {
    const kept: Arrangement = { favorites: [2, 1], groups: [{ id: 'g1', name: 'Play', collapsed: true, channels: [3] }] };
    expect(parseArrangement(JSON.parse(JSON.stringify(kept)))).toEqual(kept);
  });
  it.each([null, undefined, 'x', 7, [], { favorites: 'no' }, { groups: {} }, { favorites: [1.5, -2, 'a', null, 0], groups: [null, 4, { id: 3 }, { id: 'g', name: '' }] }])('treats %j as corrupt data: nothing arranged', (raw) => {
    expect(parseArrangement(raw)).toEqual(emptyArrangement);
  });
  it('keeps what is good of a half-corrupt arrangement and drops duplicates', () => {
    const raw = {
      favorites: [1, 1, 'x', 2],
      groups: [
        { id: 'g1', name: ' Play ', collapsed: 'yes', channels: [3, 3, 'z', 4] },
        { id: 'g1', name: 'Dup id', channels: [] },
        { id: 'g2', name: 'play', channels: [] },
        { id: 'g3', name: 'Other', channels: [4, 2] },
      ],
    };
    expect(parseArrangement(raw)).toEqual({
      favorites: [1, 2],
      groups: [
        { id: 'g1', name: 'Play', collapsed: false, channels: [3, 4] },
        { id: 'g3', name: 'Other', collapsed: false, channels: [2] },
      ],
    });
  });
});

describe('pruneArrangement', () => {
  const a: Arrangement = { favorites: [1, 9], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [8, 2] }] };
  it('drops channels the viewer no longer holds, from every list', () => {
    expect(pruneArrangement(a, channels)).toEqual({ favorites: [1], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [2] }] });
  });
  it('is the same object when nothing changed, and keeps archived channels', () => {
    const ok: Arrangement = { favorites: [5], groups: [] };
    expect(pruneArrangement(ok, channels)).toBe(ok);
  });
  it('leaves an empty group in place', () => {
    expect(pruneArrangement({ favorites: [], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [9] }] }, channels).groups).toHaveLength(1);
  });
});

describe('layoutOf and visualOrder', () => {
  const a: Arrangement = {
    favorites: [3, 1],
    groups: [
      { id: 'g1', name: 'Play', collapsed: false, channels: [2, 3] },
      { id: 'g2', name: 'Empty', collapsed: true, channels: [] },
    ],
  };
  const live = channels.filter((c) => c.archived_at === undefined);
  it('puts favorites first in their order, each channel once, the rest in today\'s order', () => {
    const layout = layoutOf(a, live);
    expect(names(layout.favorites)).toEqual(['books', 'general']);
    expect(layout.groups.map((g) => names(g.channels))).toEqual([['games'], []]);
    expect(names(layout.rest)).toEqual(['music']);
  });
  it('is just today\'s list when nothing is arranged', () => {
    const layout = layoutOf(emptyArrangement, live);
    expect(layout.favorites).toEqual([]);
    expect(layout.groups).toEqual([]);
    expect(names(layout.rest)).toEqual(['general', 'games', 'books', 'music']);
  });
  it('gives the visual order, archived last', () => {
    expect(names(visualOrder(a, channels))).toEqual(['books', 'general', 'games', 'music', 'old']);
  });
});

describe('editing', () => {
  const live = channels.filter((c) => c.archived_at === undefined);
  it('favorites and unfavorites, newest last', () => {
    const one = toggleFavorite(emptyArrangement, 2);
    const two = toggleFavorite(one, 1);
    expect(two.favorites).toEqual([2, 1]);
    expect(toggleFavorite(two, 2).favorites).toEqual([1]);
  });
  it('moves within favorites and stops at the ends', () => {
    const a: Arrangement = { favorites: [1, 2, 3], groups: [] };
    expect(moveWithin(a, live, 3, -1).favorites).toEqual([1, 3, 2]);
    expect(moveWithin(a, live, 1, -1)).toBe(a);
    expect(moveWithin(a, live, 3, 1)).toBe(a);
  });
  it('moves within a group and passes over channels not shown', () => {
    const a: Arrangement = { favorites: [], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [1, 5, 2] }] };
    expect(moveWithin(a, live, 2, -1).groups[0]?.channels).toEqual([2, 5, 1]);
  });
  it('reorders a group among the channels it draws, never swapping with a favorite shown elsewhere', () => {
    const a: Arrangement = { favorites: [2], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [1, 2, 3] }] };
    expect(moveWithin(a, live, 3, -1).groups[0]?.channels).toEqual([3, 2, 1]);
    expect(moveWithin(a, live, 1, 1).groups[0]?.channels).toEqual([3, 2, 1]);
  });
  it('offers no move to the only channel a group draws', () => {
    const a: Arrangement = { favorites: [2], groups: [{ id: 'g1', name: 'Play', collapsed: false, channels: [2, 3] }] };
    expect(moveWithin(a, live, 3, -1)).toBe(a);
    expect(moveWithin(a, live, 3, 1)).toBe(a);
  });
  it('expands a group', () => {
    const a: Arrangement = { favorites: [], groups: [{ id: 'g1', name: 'Play', collapsed: true, channels: [] }] };
    expect(expandGroup(a, 'g1').groups[0]?.collapsed).toBe(false);
    const open = expandGroup(a, 'g1');
    expect(expandGroup(open, 'g1')).toBe(open);
  });
  it('does nothing for a channel in no group and no favorites', () => {
    const a: Arrangement = { favorites: [], groups: [] };
    expect(moveWithin(a, live, 1, 1)).toBe(a);
  });
  it('moves a channel into one group at a time, and out again', () => {
    let a = addGroup(addGroup(emptyArrangement, 'One'), 'Two');
    a = moveToGroup(a, 1, 'g1');
    a = moveToGroup(a, 1, 'g2');
    expect(a.groups.map((g) => g.channels)).toEqual([[], [1]]);
    expect(moveToGroup(a, 1, null).groups.map((g) => g.channels)).toEqual([[], []]);
  });
  it('adds, renames, collapses and deletes groups; deleting returns channels to the rest', () => {
    let a = addGroup(emptyArrangement, ' Raids ');
    expect(a.groups).toEqual([{ id: 'g1', name: 'Raids', collapsed: false, channels: [] }]);
    a = moveToGroup(addGroup(a, 'Chill'), 2, 'g2');
    expect(a.groups[1]?.id).toBe('g2');
    a = renameGroup(a, 'g1', 'Raid night');
    a = toggleCollapsed(a, 'g1');
    expect(a.groups[0]).toMatchObject({ name: 'Raid night', collapsed: true });
    a = deleteGroup(a, 'g2');
    expect(a.groups.map((g) => g.id)).toEqual(['g1']);
    expect(names(layoutOf(a, live).rest)).toContain('games');
    // Ids are not reused while others stand.
    expect(addGroup(a, 'New').groups.at(-1)?.id).toBe('g2');
  });
  it('refuses empty, long and duplicate names', () => {
    const a = addGroup(emptyArrangement, 'Play');
    expect(groupNameProblem(a, '   ')).not.toBeNull();
    expect(groupNameProblem(a, 'x'.repeat(41))).not.toBeNull();
    expect(groupNameProblem(a, ' play ')).not.toBeNull();
    expect(groupNameProblem(a, 'play', 'g1')).toBeNull();
    expect(groupNameProblem(a, 'Other')).toBeNull();
    expect(addGroup(a, '')).toBe(a);
  });
});
