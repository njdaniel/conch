// @mention completion for the composer. The candidates are the names the page
// already holds for the open channel (its member list), so nobody outside the
// channel is ever offered. What counts as a mention of a name is
// `mentions` in useNotifications.ts, the notifications' own matcher.

export const mentionMax = 8;

export interface MentionCompletion {
  // Where the '@' is, and the typed text after it.
  readonly start: number;
  readonly query: string;
}

// The `@pre` the caret is at the end of, if any. The '@' must start the text
// or follow a space, a line break or an opening bracket, so an e-mail address
// does not open it. An empty query ("@" alone) offers the first names.
export function mentionAt(text: string, caret: number): MentionCompletion | null {
  const before = text.slice(0, caret);
  const m = /(^|[\s([{])@([^\s@]*)$/u.exec(before);
  if (m === null) return null;
  const query = m[2] ?? '';
  return { start: before.length - query.length - 1, query };
}

// Names for `query`: those that start with it first, then those that contain
// it, each group in name order, at most `limit`. Case does not matter, as it
// does not in the matcher.
export function completeMention(names: readonly string[], query: string, limit = mentionMax): string[] {
  const q = query.toLowerCase();
  const sorted = [...new Set(names)].filter((n) => n !== '').sort((a, b) => a.localeCompare(b));
  const prefix = sorted.filter((n) => n.toLowerCase().startsWith(q));
  const inside = sorted.filter((n) => !n.toLowerCase().startsWith(q) && n.toLowerCase().includes(q));
  return [...prefix, ...inside].slice(0, limit);
}
