// The call view's grid (ADR-015; design docs/design/webcam-video.md §7.4): how
// many columns and rows a number of tiles takes, on a desktop window and on a
// narrow one (below 700 px). A pure function of the two, so the layout never
// depends on who speaks. The rows of 5 to 9 tiles are the design's table too,
// though this delivery draws at most 4 tiles (the larger grid is C6b's).
export interface Grid {
  readonly columns: number;
  readonly rows: number;
}

// The widest a window is still narrow, as the phone layout reads it (phone.ts).
export const narrowBelow = 700;

export function gridOf(tiles: number, narrow: boolean): Grid {
  if (tiles <= 1) return { columns: 1, rows: 1 };
  if (tiles === 2) return narrow ? { columns: 1, rows: 2 } : { columns: 2, rows: 1 };
  if (tiles <= 4) return { columns: 2, rows: 2 };
  if (tiles <= 6) return narrow ? { columns: 2, rows: 3 } : { columns: 3, rows: 2 };
  return { columns: 3, rows: 3 };
}
