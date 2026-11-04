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
    [6, true, 2, 3],
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
  it('gives everyone in voice a tile while there are at most four, in join order, the viewer last', () => {
    const roster = [occupant(9, '2026-11-03T10:00:03Z', true), occupant(7, '2026-11-03T10:00:01Z'), occupant(8, '2026-11-03T10:00:02Z')];
    const { tiles, hidden } = callTiles(roster, 8, true, names);
    expect(tiles.map((t) => [t.principal, t.name, t.me, t.camera])).toEqual([
      [7, 'member-7', false, false],
      [9, 'member-9', false, true],
      [8, 'You', true, true],
    ]);
    expect(hidden).toBe(0);
  });

  it("the viewer's own tile follows their own track, never presence (S5)", () => {
    const roster = [occupant(7, '2026-11-03T10:00:01Z', true)];
    expect(callTiles(roster, 7, false, names).tiles[0]?.camera).toBe(false);
    expect(callTiles(roster, 7, true, names).tiles[0]?.camera).toBe(true);
  });

  it('with more than four in voice, only cameras get tiles, at most four', () => {
    const roster = Array.from({ length: 7 }, (_, i) => occupant(i + 1, `2026-11-03T10:00:0${String(i)}Z`, i !== 1));
    const { tiles, hidden } = callTiles(roster, 1, false, names);
    expect(tiles).toHaveLength(maxTiles);
    expect(tiles.every((t) => t.camera)).toBe(true);
    expect(hidden).toBe(1);
  });
});
