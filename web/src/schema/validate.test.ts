// validate.ts in both modes (issue #1534). At runtime an unknown key is
// ignored and dropped; the fixture tests switch to strict so a fixture key the
// TypeScript type lacks still fails.
import { describe, expect, it } from 'vitest';
import { channelV0, type ChannelV0 } from './generated';
import { array, fromServer, int, object, str, tagged, tolerant, vocab, type Validator } from './validate';

// Today's released object(), frozen as it shipped before #1534: it refuses
// every key the type lacks. It documents what the fix replaces.
function legacyObject<T>(shape: Record<string, Validator<unknown> | { optional: Validator<unknown> }>): Validator<T> {
  return (v: unknown, path = '$') => {
    if (typeof v !== 'object' || v === null || Array.isArray(v)) return [`${path}: want an object`];
    const rec = v as Record<string, unknown>;
    const problems: string[] = [];
    for (const [key, field] of Object.entries(shape)) {
      const at = `${path}.${key}`;
      if ('optional' in field) {
        if (key in rec) problems.push(...field.optional(rec[key], at));
      } else if (key in rec) problems.push(...field(rec[key], at));
      else problems.push(`${at}: required key is missing`);
    }
    for (const key of Object.keys(rec)) {
      if (!Object.hasOwn(shape, key)) problems.push(`${path}.${key}: key is not in the TypeScript type`);
    }
    return problems;
  };
}

const at = '2026-01-02T03:04:05Z';

describe('a ChannelV0 that carries a key an older bundle lacks (the channel topic)', () => {
  const legacyChannel = legacyObject<ChannelV0>({
    id: int,
    name: str,
    created_at: (v, p) => str(v, p),
  });

  it('is refused by the validator as released before the fix', () => {
    const problems = legacyChannel({ id: 1, name: 'general', created_at: at, topic: 'hello' });
    expect(problems).toEqual(['$.topic: key is not in the TypeScript type']);
  });

  it('is accepted when read from the server, and the key is not on the value', () => {
    const value: unknown = { id: 1, name: 'general', created_at: at, topic: 'hello' };
    expect(fromServer(channelV0)(value)).toEqual([]);
    expect(value).toEqual({ id: 1, name: 'general', created_at: at });
  });

  it('is refused by default: IPC, the voice protocol and the fixtures stay strict', () => {
    expect(channelV0({ id: 1, name: 'general', created_at: at, topic: 'hello' })).toEqual([
      '$.topic: key is not in the TypeScript type',
    ]);
  });
});

describe('server data', () => {
  const inner = object<{ n: number }>({ n: int });
  const outer = fromServer(object<{ list: { n: number }[]; one: { n: number } }>({ list: array(inner), one: inner }));

  it('drops unknown keys at every depth', () => {
    const value = { list: [{ n: 1, x: 1 }], one: { n: 2, y: 2 }, z: 3 };
    expect(outer(value)).toEqual([]);
    expect(value).toEqual({ list: [{ n: 1 }], one: { n: 2 } });
  });

  it('still type-checks declared keys and requires required ones', () => {
    expect(outer({ list: [{ n: 'a', x: 1 }], one: { n: 2 } })).toEqual(['$.list[0].n: want an integer, got a string']);
    expect(outer({ one: { n: 2 } })).toEqual(['$.list: required key is missing']);
  });

  it('leaves the value as it was when the validator refuses it', () => {
    const value = { list: 'no', one: { n: 2, y: 2 }, z: 3 };
    expect(outer(value)).not.toEqual([]);
    expect(value).toEqual({ list: 'no', one: { n: 2, y: 2 }, z: 3 });
  });

  it('keeps strict vocabularies strict and refuses an unknown union tag', () => {
    const shape = fromServer(object<{ k: 'a' | 'b' }>({ k: vocab<'a' | 'b'>({ a: true, b: true }) }));
    expect(shape({ k: 'c', extra: 1 })).toHaveLength(1);
    const u = fromServer(tagged<{ kind: 'a'; n: number }, 'kind'>('kind', { a: object<{ kind: 'a'; n: number }>({ kind: str as Validator<'a'>, n: int }) }));
    expect(u({ kind: 'z' })).toHaveLength(1);
    expect(u({ kind: 'a', n: 1, more: true })).toEqual([]);
  });

  it('still tolerates an unknown value of a tolerant vocabulary', () => {
    expect(fromServer(object<{ p: string }>({ p: tolerant<'x'>({ x: true }) }))({ p: 'future' })).toEqual([]);
  });
});

describe('exceptions and frozen values', () => {
  const inner = object<{ n: number }>({ n: int });
  const boom: Validator<string> = () => {
    throw new Error('boom');
  };
  const outer = fromServer(object<{ one: { n: number }; later: string }>({ one: inner, later: boom }));

  it('drops nothing when a later field validator throws, and the next call is isolated', () => {
    const value = { one: { n: 1, extra: 1 }, later: 'x', more: 2 };
    expect(() => outer(value)).toThrow('boom');
    expect(value).toEqual({ one: { n: 1, extra: 1 }, later: 'x', more: 2 });
    // Strict again, and nothing queued by the throw is deleted by this call.
    const next = { n: 1 };
    expect(inner(next)).toEqual([]);
    expect(inner({ n: 1, extra: 1 })).toEqual(['$.extra: key is not in the TypeScript type']);
  });

  it('does not throw on a frozen value, and the next call is isolated', () => {
    const frozen = Object.freeze({ n: 1, extra: 1 });
    expect(fromServer(inner)(frozen)).toEqual([]);
    expect(frozen).toEqual({ n: 1, extra: 1 });
    const other = { n: 2, extra: 2 };
    expect(inner(other)).toEqual(['$.extra: key is not in the TypeScript type']);
    expect(other).toEqual({ n: 2, extra: 2 });
  });
});
