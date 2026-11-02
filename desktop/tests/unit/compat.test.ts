import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { allowUnlisted, judge, OVERRIDE_FLAG, parseCompat, permits, type CompatPolicy } from '../../main/compat.ts';

const policy = (over: Partial<CompatPolicy> = {}): CompatPolicy => ({ servers: ['v0.1.0-alpha.9', 'v0.1.0-alpha.10'], override: false, ...over });

describe('the allowlist', () => {
  const cases: [string, string | null, 'supported' | 'unsupported'][] = [
    ['a listed release', 'v0.1.0-alpha.9', 'supported'],
    ['another listed release', 'v0.1.0-alpha.10', 'supported'],
    ['an unlisted release', 'v0.1.0-alpha.11', 'unsupported'],
    ['a later release (this is not a >= range)', 'v0.2.0', 'unsupported'],
    ['a development build', 'v0.0.0-dev', 'unsupported'],
    ['a commit-named build', '9985704abcde', 'unsupported'],
    ['a dirty build', 'v0.1.0-alpha.9-dirty', 'unsupported'],
    ['a listed release in other case', 'V0.1.0-ALPHA.9', 'unsupported'],
    ['a listed release with a space', 'v0.1.0-alpha.9 ', 'unsupported'],
    ['a listed release with build metadata', 'v0.1.0-alpha.9+x', 'unsupported'],
    ['an empty version', '', 'unsupported'],
    ['a missing version', null, 'unsupported'],
    ['a malformed version', '<script>alert(1)</script>', 'unsupported'],
    ['an over-long version', 'v'.repeat(500), 'unsupported'],
  ];
  it.each(cases)('%s', (_name, version, state) => {
    const verdict = judge(policy(), { kind: 'answered', version });
    expect(verdict.state).toBe(state);
    expect(permits(verdict.state)).toBe(state === 'supported');
  });

  it('shows a version only when it is made of safe characters, and never the malformed text', () => {
    expect(judge(policy(), { kind: 'answered', version: 'v0.2.0' })).toEqual({ state: 'unsupported', server: 'v0.2.0' });
    expect(judge(policy(), { kind: 'answered', version: '<script>' })).toEqual({ state: 'unsupported', server: null });
    expect(judge(policy(), { kind: 'answered', version: null })).toEqual({ state: 'unsupported', server: null });
    expect(judge(policy(), { kind: 'answered', version: 'v'.repeat(500) }).server).toBeNull();
  });

  it('an empty list supports nothing', () => {
    expect(judge(policy({ servers: [] }), { kind: 'answered', version: 'v0.1.0-alpha.9' }).state).toBe('unsupported');
  });

  it('a server that did not answer is never supported, override or not', () => {
    for (const override of [false, true]) {
      const verdict = judge(policy({ override }), { kind: 'unreachable' });
      expect(verdict).toEqual({ state: 'unreachable', server: null });
      expect(permits(verdict.state)).toBe(false);
    }
  });

  it('the development override lets an unlisted version that answered through, and says so', () => {
    for (const version of ['v0.2.0', 'v0.0.0-dev', '9985704abcde', null]) {
      const verdict = judge(policy({ override: true }), { kind: 'answered', version });
      expect(verdict.state).toBe('unlisted');
      expect(permits(verdict.state)).toBe(true);
    }
    expect(judge(policy({ override: true }), { kind: 'answered', version: 'v0.1.0-alpha.9' }).state).toBe('supported');
  });

  it('only supported and unlisted may sign in', () => {
    expect(permits('none')).toBe(false);
    expect(permits('unsupported')).toBe(false);
    expect(permits('unreachable')).toBe(false);
    expect(permits('supported')).toBe(true);
    expect(permits('unlisted')).toBe(true);
  });
});

describe('the manifest', () => {
  it('parses a list of releases', () => {
    expect(parseCompat('{"version":1,"servers":["v0.1.0-alpha.9","v1.0.0"]}')).toEqual({ servers: ['v0.1.0-alpha.9', 'v1.0.0'] });
  });

  const bad: [string, string][] = [
    ['not JSON', 'nope'],
    ['an array', '[]'],
    ['no version field', '{"servers":[]}'],
    ['another format version', '{"version":2,"servers":["v1.0.0"]}'],
    ['no servers', '{"version":1}'],
    ['servers not an array', '{"version":1,"servers":"v1.0.0"}'],
    ['a non-string entry', '{"version":1,"servers":[1]}'],
    ['a range', '{"version":1,"servers":[">=0.1.0"]}'],
    ['a wildcard', '{"version":1,"servers":["v0.1.*"]}'],
    ['an entry without v', '{"version":1,"servers":["0.1.0"]}'],
    ['a duplicate', '{"version":1,"servers":["v1.0.0","v1.0.0"]}'],
    ['an extra key', '{"version":1,"servers":[],"other":1}'],
    ['too many entries', JSON.stringify({ version: 1, servers: Array.from({ length: 200 }, (_, i) => `v1.0.${String(i)}`) })],
  ];
  it.each(bad)('refuses %s', (_name, text) => {
    expect(parseCompat(text)).toBeNull();
  });

  it('the checked-in desktop/build/compat.json is valid and lists at least one release', () => {
    const text = readFileSync(join(import.meta.dirname, '..', '..', 'build', 'compat.json'), 'utf8');
    const parsed = parseCompat(text);
    expect(parsed).not.toBeNull();
    expect(parsed?.servers.length).toBeGreaterThan(0);
  });
});

describe('the development flag', () => {
  it('is honoured by an unpackaged build', () => {
    expect(allowUnlisted(false, ['electron', '.', OVERRIDE_FLAG])).toBe(true);
    expect(allowUnlisted(false, ['electron', '.'])).toBe(false);
  });

  it('is ignored by the packaged build, however it is spelled', () => {
    expect(allowUnlisted(true, ['conch', OVERRIDE_FLAG])).toBe(false);
    expect(allowUnlisted(true, ['conch', `${OVERRIDE_FLAG}=1`])).toBe(false);
    expect(allowUnlisted(true, [])).toBe(false);
  });

  it('is matched exactly: no prefix, no value', () => {
    expect(allowUnlisted(false, ['x', '--allow-unlisted-servers'])).toBe(false);
    expect(allowUnlisted(false, ['x', `${OVERRIDE_FLAG}=0`])).toBe(false);
    expect(allowUnlisted(false, ['x', 'allow-unlisted-server'])).toBe(false);
  });
});
