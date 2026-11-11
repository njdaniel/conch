// Checks the browser's mention matcher against the golden table of the
// shared mention rule, pkg/schema/testdata/rules/mentions.json (issue #1712,
// docs/design/mobile-notifications.md §9.1 S6). Go's schema.Mentions is
// tested against the same file (pkg/schema/mention_test.go), so the server
// and the browser mean one thing by "mentions". The table records the
// browser's rule: a row that fails here is a difference to report, not a
// reason to change mentions().
import { describe, expect, it } from 'vitest';
import { mentions } from './useNotifications';

interface Row {
  readonly body: string;
  readonly name: string;
  readonly want: boolean;
}

// Read by relative path, so the test sees the table of this checkout.
const tables = import.meta.glob<unknown>('../../pkg/schema/testdata/rules/mentions.json', {
  eager: true,
  import: 'default',
});

function isRow(value: unknown): value is Row {
  if (typeof value !== 'object' || value === null) return false;
  const row = value as Record<string, unknown>;
  return (
    Object.keys(row).length === 3 &&
    typeof row.body === 'string' &&
    typeof row.name === 'string' &&
    typeof row.want === 'boolean'
  );
}

function rows(): readonly Row[] {
  const [table] = Object.values(tables);
  if (!Array.isArray(table)) throw new Error('the mention table was not found, or is not a list');
  const list: readonly unknown[] = table;
  return list.map((row, i) => {
    if (!isRow(row)) throw new Error(`row ${String(i)} of the mention table is not {body, name, want}`);
    return row;
  });
}

// A row's text as ASCII, so a failure names an invisible character by number.
function shown(text: string): string {
  return JSON.stringify(text).replace(/[^\x20-\x7e]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`);
}

describe('mentions() and the golden table of the shared mention rule', () => {
  const table = rows();

  it('has the rows the rule needs', () => {
    expect(table.length).toBeGreaterThanOrEqual(40);
    expect(table.some((row) => row.want)).toBe(true);
    expect(table.some((row) => !row.want)).toBe(true);
  });

  it.each(table.map((row, i) => ({ i, row, title: `${shown(row.body)} for ${shown(row.name)}` })))(
    'row $i: $title',
    ({ row }) => {
      expect(mentions(row.body, row.name)).toBe(row.want);
    },
  );
});
