// Checks on the source and on the build, not on behaviour: where the credential
// may be touched, and that the shipped bundle never reaches for localStorage.
import { build } from 'vite';
import { describe, expect, it } from 'vitest';

const sources = import.meta.glob<string>(['./**/*.ts', './**/*.tsx', '!./**/*.test.*'], {
  eager: true,
  query: '?raw',
  import: 'default',
});

describe('credential handling', () => {
  it('finds the sources', () => {
    expect(Object.keys(sources).length).toBeGreaterThan(5);
  });

  it('is in src/api.ts alone', () => {
    const touching = Object.entries(sources)
      .filter(([, code]) => /conch\.web\.token|Authorization|sessionStorage\.(get|set)Item\(tokenKey/.test(code))
      .map(([path]) => path);
    expect(touching).toEqual(['./api.ts']);
  });

  it('is never logged: the page makes no console call', () => {
    const logging = Object.entries(sources)
      .filter(([, code]) => /\bconsole\s*\./.test(code))
      .map(([path]) => path);
    expect(logging).toEqual([]);
  });

  it('is not placed in a URL: the only fetch sends it as a header', () => {
    const code = sources['./api.ts'] ?? '';
    expect(code).not.toMatch(/\?\s*(token|access_token)=/);
    expect(code).not.toMatch(/\$\{token\}[^`]*`\s*,\s*\{/);
  });
});

describe('the built bundle', () => {
  it('never mentions localStorage', async () => {
    const result = await build({ configFile: 'vite.config.ts', logLevel: 'silent', build: { write: false } });
    const outputs = (Array.isArray(result) ? result : [result]).flatMap((r) => ('output' in r ? r.output : []));
    const code = outputs.map((o) => (o.type === 'chunk' ? o.code : '')).join('\n');
    expect(code.length).toBeGreaterThan(1000);
    expect(code).not.toContain('localStorage');
  }, 60_000);
});
