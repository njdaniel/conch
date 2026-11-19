import { describe, expect, it } from 'vitest';
import { identityMarks, identityText, type IdentityInput, type IdentityKey } from './identityMarks';

const all: IdentityInput = {
  id: 3,
  viewer: 3,
  kind: 'human',
  owner: true,
  role: 'operator',
  manages: true,
  can_decide: true,
};
const order: readonly IdentityKey[] = ['you', 'agent', 'owner', 'operator', 'manager', 'approver'];

const cases: readonly { readonly name: string; readonly input: IdentityInput; readonly want: readonly IdentityKey[] }[] = [
  { name: 'nothing arrived', input: {}, want: [] },
  { name: 'a plain human', input: { kind: 'human' }, want: [] },
  { name: 'an agent', input: { kind: 'agent' }, want: ['agent'] },
  { name: 'missing kind has no human privilege', input: { owner: true, role: 'operator', manages: true, can_decide: true }, want: [] },
  { name: 'unknown kind has no human privilege', input: { ...all, kind: 'robot' as IdentityInput['kind'] }, want: ['you'] },
  { name: 'self', input: { id: 3, viewer: 3 }, want: ['you'] },
  { name: 'someone else', input: { id: 3, viewer: 4 }, want: [] },
  { name: 'owner', input: { kind: 'human', owner: true }, want: ['owner'] },
  { name: 'not owner', input: { kind: 'human', owner: false }, want: [] },
  { name: 'operator', input: { kind: 'human', role: 'operator' }, want: ['operator'] },
  { name: 'member role', input: { kind: 'human', role: 'member' }, want: [] },
  { name: 'manager', input: { kind: 'human', manages: true }, want: ['manager'] },
  { name: 'not manager', input: { kind: 'human', manages: false }, want: [] },
  { name: 'approver', input: { kind: 'human', can_decide: true }, want: ['approver'] },
  { name: 'not approver', input: { kind: 'human', can_decide: false }, want: [] },
  { name: 'an agent cannot inherit human fields', input: { kind: 'agent', owner: true, role: 'operator', manages: true, can_decide: true }, want: ['agent'] },
  {
    name: 'a guest cannot inherit member fields',
    input: { ...all, guest: { handle: 'guest-1', tag: 'a7k4' } },
    want: ['you'],
  },
  { name: 'every human mark in fixed order', input: all, want: ['you', 'owner', 'operator', 'manager', 'approver'] },
];

describe('identityMarks', () => {
  it.each(cases)('$name', ({ input, want }) => {
    expect(identityMarks(input).map((mark) => mark.key)).toEqual(want);
  });

  it('returns labels and the bracketed text form in the same fixed order', () => {
    const marks = identityMarks(all);
    expect(marks.map((mark) => mark.label)).toEqual(['You', 'Owner', 'Operator', 'Manager', 'Approver']);
    expect(marks.map(identityText)).toEqual(['[you]', '[owner]', '[operator]', '[manager]', '[approver]']);
    expect(order).toEqual(['you', 'agent', 'owner', 'operator', 'manager', 'approver']);
    expect(identityText({ key: 'agent', label: 'Agent' })).toBe('[agent]');
  });
});

const sources = import.meta.glob<string>(['./**/*.tsx', '!./**/*.test.tsx'], { eager: true, query: '?raw', import: 'default' });

describe('identity mark hygiene', () => {
  it('writes no identity ag-badge text outside Identity.tsx', () => {
    const badge = /<span\b[^>]*className=(["'])[^"']*\bag-badge\b[^"']*\1[^>]*>[\s\S]*?<\/span>/g;
    const hits = Object.entries(sources).filter(([path, code]) => {
      if (path === './Identity.tsx') return false;
      return (code.match(badge) ?? []).some((text) => /\b(?:Agent|Operator|Owner|Manager|Approver)\b/.test(text));
    });
    expect(hits.map(([path]) => path)).toEqual([]);
  });
});
