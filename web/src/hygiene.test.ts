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
    // Nothing exports the token's accessors for another module to use.
    // Storage is searched for a credential only by its definition, hasToken
    // and restore. Every other call sends the credential the page signed in
    // with (current(), #439 review M1), so none can fall through to another
    // one that happens to be stored.
    expect(code.match(/\breadToken\(\)/g)?.length ?? 0).toBe(3);
    expect(code).not.toMatch(/export\s+(async\s+)?function\s+((read|write|drop)Token|sendAs|current|forget)\b/);
    // The pin is set in three places only: signIn, continueAsOperator and
    // restore.
    expect(code.match(/\bpinned = \{|\bpinned = held\b/g)?.length ?? 0).toBe(3);
  });

  it('is not placed in a URL: the only fetch sends it as a header', () => {
    const code = sources['./api.ts'] ?? '';
    expect(code).not.toMatch(/\?\s*(token|access_token)=/);
    expect(code).not.toMatch(/\$\{(held\.)?token\}[^`]*`\s*,\s*\{/);
  });
});

// localStorage (issue #439, decision O1): a member's credential lasts there,
// and nothing else of ours does. Comments are left out: saying where the SDK
// keeps its log level is not using the storage.
describe('local storage', () => {
  it('is named in src/api.ts alone', () => {
    const naming = Object.entries(sources)
      .filter(([, code]) => /localStorage/.test(withoutComments(code)))
      .map(([path]) => path);
    expect(naming).toEqual(['./api.ts']);
  });

  it('is used in src/api.ts only for the token key', () => {
    const code = withoutComments(sources['./api.ts'] ?? '');
    // Named twice: where a lasting credential is kept, and the storage
    // event's check that the change was there. Never read from directly.
    expect(code.match(/localStorage/g)?.length).toBe(2);
    expect(code).not.toMatch(/localStorage\s*(\.|\[)/);
    // Every storage call in the module names the token key, and nothing
    // lists, counts or clears a storage.
    expect(code.match(storageCall)?.length).toBeGreaterThan(0);
    expect(code).not.toMatch(storageCallOtherKey);
    expect(code).not.toMatch(/\.(key|clear)\(|[sS]torage\.length/);
  });

  it('is not missed by the rules: a bare test of each', () => {
    expect(withoutComments('// keeps a level in localStorage\nconst a = 1;')).not.toMatch(/localStorage/);
    expect(withoutComments('/* localStorage */ const a = 1;')).not.toMatch(/localStorage/);
    expect(withoutComments('const s = window.localStorage; // where')).toMatch(/localStorage/);
    expect(withoutComments("const u = '//'; const s = window.localStorage;")).toMatch(/localStorage/);
    expect(withoutComments('  // a note\n  window.localStorage.getItem(k);')).toMatch(/localStorage/);
    for (const example of ['storage.getItem(otherKey)', 'storage.setItem("conch.x", v)', 'storage.removeItem( lastKey)', 'storage.getItem(tokenKeys)']) {
      expect(storageCallOtherKey.test(example), example).toBe(true);
    }
    expect(storageCallOtherKey.test('storage.getItem(tokenKey)')).toBe(false);
    expect(storageCallOtherKey.test('storage.setItem(tokenKey, token)')).toBe(false);
  });
});

const storageCall = /\.(getItem|setItem|removeItem)\(/g;
const storageCallOtherKey = /\.(getItem|setItem|removeItem)\(\s*(?!tokenKey\b)/;

// withoutComments drops whole-line // comments and /* */ blocks. A // after
// code on a line is kept, so that a // in a string cannot hide what follows it.
function withoutComments(code: string): string {
  return code.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
}

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

// Browser voice (#442): the LiveKit SDK is imported only from src/voice/sdk.ts,
// and it alone reaches for the microphone. No source of ours calls a device
// API, not even under src/voice/, so the ban of #395 stays as it was, for
// every file.
const deviceApi = /getUserMedia|mediaDevices|enumerateDevices|getDisplayMedia/;
const notVoice = (path: string) => !path.startsWith('./voice/');

// Where the join token of a voice grant may be read: a property read
// (grant.token, x.token), an index into rooms, or a destructuring that names
// token (const { token } = grant; const { token: t } = grant).
const tokenRead = /\.token\b|rooms\[|\{[^{}]*\btoken\b[^{}]*\}\s*=[^=>]/;

describe('media devices', () => {
  it('are never reached for by our code: no getUserMedia, no navigator.mediaDevices, anywhere', () => {
    const hits = Object.entries(sources)
      .filter(([, code]) => deviceApi.test(code))
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
      .filter(([, code]) => tokenRead.test(code))
      .map(([path]) => path)
      .filter((path) => path !== './api.ts' && notVoice(path));
    expect(touching).toEqual([]);
  });

  it('are not mentioned by a pattern the rule would miss: a bare test of each rule', () => {
    // Still forbidden in every file, src/voice/ included.
    for (const example of ['navigator.mediaDevices.getUserMedia({audio:true})', 'x.enumerateDevices()', 'getDisplayMedia()']) {
      expect(deviceApi.test(example)).toBe(true);
    }
    // Ways to read the token outside src/api.ts and src/voice/, each caught.
    for (const example of [
      'const t = grant.token;',
      'send(session.rooms[0])',
      'const { token } = grant;',
      'const { room, token } = grant;',
      'const { token: t } = grant;',
      'let { token } = rooms;',
      '({ token } = grant);',
      'const {\n  token,\n} = grant;',
    ]) {
      expect(tokenRead.test(example), example).toBe(true);
    }
    // Not reads of a token: a type with a token field, an arrow taking one.
    for (const example of ['type T = { token: string };', 'const f = ({ token }: T) => token.length']) {
      expect(tokenRead.test(example), example).toBe(false);
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
  it('uses the screen API only as the SDK does, and storage only as the SDK and src/api.ts do', async () => {
    const result = await build({ configFile: 'vite.config.ts', logLevel: 'silent', build: { write: false } });
    const outputs = (Array.isArray(result) ? result : [result]).flatMap((r) => ('output' in r ? r.output : []));
    const code = outputs.map((o) => (o.type === 'chunk' ? o.code : '')).join('\n');
    expect(code.length).toBeGreaterThan(1000);
    // The SDK reaches for the microphone (allowed since #442). The screen is
    // not ours until A11b: the token is microphone-only, so the SDK's screen
    // share could not publish anyway; its mentions are listed so a new one shows.
    expect(screenMentions(code)).toBeLessThanOrEqual(maxScreenMentions);
    // localStorage, counted module by module (issue #439): every mention in
    // the bundle comes from a module storageMentions names, exactly as often
    // as it says, so a mention from any other module fails here.
    const found: Record<string, number> = {};
    let inModules = 0;
    for (const chunk of outputs) {
      if (chunk.type !== 'chunk') continue;
      for (const [id, rendered] of Object.entries(chunk.modules)) {
        const moduleCode = rendered.code ?? '';
        const at = allIndexes(moduleCode, 'localStorage');
        if (at.length === 0) continue;
        const source = sourceOf(id);
        found[source] = (found[source] ?? 0) + at.length;
        inModules += at.length;
        // The SDK's are its logger's (loglevel), each beside its storage key.
        if (source === livekitBundle) {
          for (const i of at) expect(moduleCode.slice(Math.max(0, i - 600), i + 600)).toMatch(/loglevel|storageKey/);
        }
      }
    }
    expect(found).toEqual(storageMentions);
    // Nor does anything outside a module (code the bundler wrote itself).
    expect(allIndexes(code, 'localStorage').length).toBe(inModules);
  }, 60_000);
});

// Where the built bundle may name localStorage, and how many times.
//
// src/api.ts keeps a member's credential there (issue #439, decision O1).
//
// The LiveKit SDK (livekit-client 2.22.3) bundles its logger, loglevel, into
// its own ESM build, so loglevel's mentions are counted under the SDK's file.
// loglevel reads localStorage and cookies when a logger is created, for a
// level remembered there, and writes them only when a level is set with
// persist on. The client never does: setLevel is called with persist=false
// and the SDK's setLogLevel, which persists, is never called
// (sdk.storage.test.ts). loglevel reads only its own key, never the
// credential's. A bump of the SDK that changes the count fails here and is
// looked at.
const livekitBundle = 'node_modules/livekit-client/dist/livekit-client.esm.mjs';
const storageMentions: Record<string, number> = {
  'src/api.ts': 2,
  [livekitBundle]: 3,
};

// sourceOf names a bundled module by its path from web/: node_modules/... for
// a dependency, src/... for ours.
function sourceOf(id: string): string {
  const dep = id.lastIndexOf('/node_modules/');
  if (dep !== -1) return id.slice(dep + 1);
  const ours = id.lastIndexOf('/web/src/');
  return ours !== -1 ? id.slice(ours + '/web/'.length) : id;
}

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
