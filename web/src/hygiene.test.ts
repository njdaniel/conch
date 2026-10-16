// Checks on the source and on the build, not on behaviour: where the credential
// may be touched, where the microphone and the SDK may be, and what the shipped
// bundle may mention.
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

// Browser voice (#442): the microphone is reached only from src/voice/, and the
// LiveKit SDK is imported only there. Everywhere else the page asks for no
// device, as before (#395).
const deviceApi = /getUserMedia|mediaDevices|enumerateDevices|getDisplayMedia/;
const notVoice = (path: string) => !path.startsWith('./voice/');

describe('media devices', () => {
  it('are reached for only under src/voice/', () => {
    const hits = Object.entries(sources)
      .filter(([path, code]) => notVoice(path) && deviceApi.test(code))
      .map(([path]) => path);
    expect(hits).toEqual([]);
  });

  it('are never used for the screen: no getDisplayMedia anywhere in the sources (A11b)', () => {
    const hits = Object.entries(sources)
      .filter(([, code]) => /getDisplayMedia/.test(code))
      .map(([path]) => path);
    expect(hits).toEqual([]);
  });

  it('the LiveKit SDK is imported only from src/voice/sdk.ts', () => {
    const hits = Object.entries(sources)
      .filter(([, code]) => /from\s+['"]livekit-client['"]/.test(code))
      .map(([path]) => path);
    expect(hits).toEqual(['./voice/sdk.ts']);
  });

  it('the join token is in src/api.ts and src/voice/ only, and never logged', () => {
    const touching = Object.entries(sources)
      .filter(([, code]) => /\.token\b|grant\.token|rooms\[/.test(code))
      .map(([path]) => path)
      .filter((path) => path !== './api.ts' && notVoice(path));
    expect(touching).toEqual([]);
  });

  it('are not mentioned by a pattern the rule would miss: a bare test of the rule', () => {
    // Still forbidden outside src/voice/: each of these would be caught.
    for (const example of ['navigator.mediaDevices.getUserMedia({audio:true})', 'x.enumerateDevices()', 'getDisplayMedia()']) {
      expect(deviceApi.test(example)).toBe(true);
    }
    expect(notVoice('./Composer.tsx')).toBe(true);
    expect(notVoice('./voice/sdk.ts')).toBe(false);
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
  it('uses the screen and storage APIs only as the SDK does', async () => {
    const result = await build({ configFile: 'vite.config.ts', logLevel: 'silent', build: { write: false } });
    const outputs = (Array.isArray(result) ? result : [result]).flatMap((r) => ('output' in r ? r.output : []));
    const code = outputs.map((o) => (o.type === 'chunk' ? o.code : '')).join('\n');
    expect(code.length).toBeGreaterThan(1000);
    // The SDK reaches for the microphone (allowed since #442). The screen is
    // not ours until A11b: the token is microphone-only, so the SDK's screen
    // share could not publish anyway; its mentions are listed so a new one shows.
    expect(screenMentions(code)).toBeLessThanOrEqual(maxScreenMentions);
    // localStorage: only the SDK's logger (loglevel) mentions it, to remember
    // a log level; the client persists nothing (setLevel is called with
    // persist=false) and never touches it itself.
    for (const at of allIndexes(code, 'localStorage')) {
      expect(code.slice(Math.max(0, at - 600), at + 600)).toMatch(/loglevel|storageKey/);
    }
  }, 60_000);
});

// How many times the SDK's own code names getDisplayMedia (its screen-share methods
// are not tree-shaken away), measured on
// the pinned version (livekit-client 2.22.3). A bump that adds one fails here
// and is looked at.
const maxScreenMentions = 6;

function screenMentions(code: string): number {
  return allIndexes(code, 'getDisplayMedia').length;
}

function allIndexes(code: string, needle: string): number[] {
  const found: number[] = [];
  for (let i = code.indexOf(needle); i !== -1; i = code.indexOf(needle, i + 1)) found.push(i);
  return found;
}
