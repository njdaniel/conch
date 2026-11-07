// The call view's grid (design §7.4's table), and who gets a tile: a pure
// function of the tiles and the width, so nothing moves while someone speaks.
import { describe, expect, it } from 'vitest';
import { callTiles, maxTiles } from './CallView';
import { gridOf } from './gridOf';
import type { Occupant } from './presence';

describe('gridOf', () => {
  it.each([
    [1, false, 1, 1],
    [2, false, 2, 1],
    [3, false, 2, 2],
    [4, false, 2, 2],
    [1, true, 1, 1],
    [2, true, 1, 2],
    [3, true, 2, 2],
    [4, true, 2, 2],
    [5, false, 3, 2],
    [5, true, 2, 3],
    [6, false, 3, 2],
    [6, true, 2, 3],
    [7, false, 3, 3],
    [7, true, 3, 3],
    [8, false, 3, 3],
    [8, true, 3, 3],
    [9, false, 3, 3],
    [9, true, 3, 3],
  ])('%i tiles, narrow %s: %i columns by %i rows', (tiles, narrow, columns, rows) => {
    expect(gridOf(tiles, narrow)).toEqual({ columns, rows });
  });

  it('has a place for every tile', () => {
    for (let n = 1; n <= 9; n++) {
      for (const narrow of [false, true]) {
        const g = gridOf(n, narrow);
        expect(g.columns * g.rows, `${String(n)} narrow ${String(narrow)}`).toBeGreaterThanOrEqual(n);
      }
    }
  });
});

const occupant = (principal: number, joinedAt: string, camera = false): Occupant => ({ principal, transmitting: false, sharing: false, sharingAudio: false, camera, joinedAt });
const names = (id: number) => `member-${String(id)}`;

describe('callTiles', () => {
  it('gives everyone in voice a tile while there are at most nine, in join order, the viewer last', () => {
    const roster = [occupant(9, '2026-11-03T10:00:03Z', true), occupant(7, '2026-11-03T10:00:01Z'), occupant(8, '2026-11-03T10:00:02Z')];
    const { tiles, hidden } = callTiles(roster, 8, true, names);
    expect(tiles.map((t) => [t.principal, t.name, t.me, t.camera])).toEqual([
      [7, 'member-7', false, false],
      [9, 'member-9', false, true],
      [8, 'You', true, true],
    ]);
    expect(hidden).toBe(0);
  });

  it('names the viewer tile by their own name, last and with the me flag; others unchanged (#1481)', () => {
    const roster = [occupant(9, '2026-11-03T10:00:03Z'), occupant(8, '2026-11-03T10:00:02Z')];
    const { tiles } = callTiles(roster, 8, false, names, 'nick');
    expect(tiles.map((t) => [t.principal, t.name, t.me])).toEqual([
      [9, 'member-9', false],
      [8, 'nick', true],
    ]);
  });

  it('keeps a hostile viewer name as the literal string (#1481)', () => {
    const hostile = '<img src=x onerror=alert(1)>';
    expect(callTiles([occupant(8, '2026-11-03T10:00:02Z')], 8, false, names, hostile).tiles[0]?.name).toBe(hostile);
  });

  it('says You for the viewer tile while the name is not known (#1481)', () => {
    expect(callTiles([occupant(8, '2026-11-03T10:00:02Z')], 8, false, names, '').tiles[0]?.name).toBe('You');
  });

  it("the viewer's own tile follows their own track, never presence (S5)", () => {
    const roster = [occupant(7, '2026-11-03T10:00:01Z', true)];
    expect(callTiles(roster, 7, false, names).tiles[0]?.camera).toBe(false);
    expect(callTiles(roster, 7, true, names).tiles[0]?.camera).toBe(true);
  });

  it('with more than nine in voice, only cameras get tiles, at most nine', () => {
    const roster = Array.from({ length: 12 }, (_, i) => occupant(i + 1, `2026-11-03T10:00:${String(10 + i)}Z`, i !== 1));
    const { tiles, hidden } = callTiles(roster, 1, false, names);
    expect(maxTiles).toBe(9);
    expect(tiles).toHaveLength(maxTiles);
    expect(tiles.every((t) => t.camera)).toBe(true);
    expect(hidden).toBe(1);
  });

  it('nine in voice: everyone gets a tile, cameras or not, in join order with the viewer last', () => {
    const roster = Array.from({ length: 9 }, (_, i) => occupant(9 - i, `2026-11-03T10:00:0${String(9 - i)}Z`, i % 3 === 0));
    const { tiles, hidden } = callTiles(roster, 4, false, names);
    expect(tiles.map((t) => t.principal)).toEqual([1, 2, 3, 5, 6, 7, 8, 9, 4]);
    expect(tiles.filter((t) => !t.camera)).toHaveLength(6);
    expect(hidden).toBe(0);
  });

  it('ten in voice with three cameras: the three cameras alone, in join order', () => {
    const roster = Array.from({ length: 10 }, (_, i) => occupant(i + 1, `2026-11-03T10:00:${String(10 + i)}Z`, [3, 6, 9].includes(i + 1)));
    const { tiles, hidden } = callTiles(roster, 1, false, names);
    expect(tiles.map((t) => t.principal)).toEqual([3, 6, 9]);
    expect(hidden).toBe(0);
  });

  it('ten in voice with nine cameras: nine tiles, none left over', () => {
    const roster = Array.from({ length: 10 }, (_, i) => occupant(i + 1, `2026-11-03T10:00:${String(10 + i)}Z`, i + 1 !== 5));
    const { tiles, hidden } = callTiles(roster, 1, true, names);
    expect(tiles.map((t) => t.principal)).toEqual([2, 3, 4, 6, 7, 8, 9, 10, 1]);
    expect(tiles.every((t) => t.camera)).toBe(true);
    expect(hidden).toBe(0);
  });
});
