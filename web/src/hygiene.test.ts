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

  it('is read only through authed and authedPost (or by signIn, for a candidate)', () => {
    const code = sources['./api.ts'] ?? '';
    // Every readToken() call is inside authed: one call, and nothing exports
    // the token's accessors for another module to use.
    // Its definition, authed, authedPost and hasToken: no other call reads the token.
    expect(code.match(/\breadToken\(\)/g)?.length ?? 0).toBe(4);
    expect(code).not.toMatch(/export\s+(async\s+)?function\s+(read|write|drop)Token/);
  });

  it('is not placed in a URL: the only fetch sends it as a header', () => {
    const code = sources['./api.ts'] ?? '';
    expect(code).not.toMatch(/\?\s*(token|access_token)=/);
    expect(code).not.toMatch(/\$\{token\}[^`]*`\s*,\s*\{/);
  });
});

// What the page's CSP forbids and what would let someone else's text become
// markup: none may appear in the source. #281 and #282 render messages and
// names written by other people; this is the tripwire before they do.
const cssSources = import.meta.glob<string>(['./**/*.css'], { eager: true, query: '?raw', import: 'default' });

describe('rendering hygiene', () => {
  it('uses no HTML sink, eval, inline style or external resource', () => {
    expect(Object.keys(cssSources).length).toBeGreaterThan(0);
    const forbidden = /\binnerHTML\b|\bouterHTML\b|insertAdjacentHTML|dangerouslySetInnerHTML|\beval\s*\(|new\s+Function\b|\bimport\s*\(|\bstyle=\{|https?:\/\//;
    const hits = Object.entries({ ...sources, ...cssSources })
      .filter(([, code]) => forbidden.test(code))
      .map(([path]) => path);
    expect(hits).toEqual([]);
  });
});

// The browser has no voice (#395): no device permission is ever asked for.
describe('media devices', () => {
  it('are never reached for: no getUserMedia, no navigator.mediaDevices', () => {
    const hits = Object.entries(sources)
      .filter(([, code]) => /getUserMedia|mediaDevices|enumerateDevices|getDisplayMedia/.test(code))
      .map(([path]) => path);
    expect(hits).toEqual([]);
  });
});

describe('the socket ticket', () => {
  it('is in src/api.ts alone, and the page is never navigated by script', () => {
    const touching = Object.entries(sources)
      .filter(([, code]) => /\bWebSocket\s*\(|ws-tickets/.test(code))
      .map(([path]) => path);
    expect(touching).toEqual(['./api.ts']);
    // Only the hash names the channel; nothing else may write the address bar.
    const navigating = Object.entries(sources)
      .filter(([, code]) => /location\.(href|search|pathname)\s*=|location\.(assign|replace)\(|history\.(push|replace)State/.test(code))
      .map(([path]) => path);
    expect(navigating).toEqual([]);
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
