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
    // Storage is read for the token in four functions only: readToken (the
    // search), current (is the pinned one still stored), dropToken (is it
    // still the one to drop) and stillStored (is restore's unchecked one
    // still stored, issue #469). A new reader trips this.
    expect(code.match(/getItem\(tokenKey\)/g)?.length ?? 0).toBe(4);
    expect(code).not.toMatch(/export\s+(async\s+)?function\s+((read|write|drop)Token|sendAs|current|forget)\b/);
    // The pin is set in five places only: signIn, continueAsOperator,
    // restore, keepRedeemed (issue #440), which keeps the credential an
    // invite, a join or a password sign-in gives as signIn keeps a member's,
    // and convertGuest (issue #713), which keeps the member's credential a
    // guest's Create an account gives.
    expect(code.match(/\bpinned = \{|\bpinned = held\b/g)?.length ?? 0).toBe(5);
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
// API, not even under src/voice/, so the ban of #395 stays as it was for the
// microphone and the camera, in every file.
//
// Screen share (#444): the SDK opens the screen picker too. One file of ours,
// src/voice/share.ts, may name the screen API, to look for it (no iPhone or
// iPad browser has one) before offering to share; it never calls it.
const deviceApi = /getUserMedia|enumerateDevices/;
const screenApi = /getDisplayMedia|mediaDevices/;
const screenCall = /getDisplayMedia\s*\(/;
const screenChecker = './voice/share.ts';
const notVoice = (path: string) => !path.startsWith('./voice/');

// Where the join token of a voice grant may be read: a property read
// (grant.token, x.token), an index into rooms, or a destructuring that names
// token (const { token } = grant; const { token: t } = grant).
const tokenRead = /\.token\b|rooms\[|\{[^{}]*\btoken\b[^{}]*\}\s*=[^=>]/;

describe('media devices', () => {
  it('are never reached for by our code: no getUserMedia, no enumerateDevices, anywhere', () => {
    const hits = Object.entries(sources)
      .filter(([, code]) => deviceApi.test(code))
      .map(([path]) => path);
    expect(hits).toEqual([]);
  });

  it('the screen API is named only by src/voice/share.ts, which never calls it', () => {
    const naming = Object.entries(sources)
      .filter(([, code]) => screenApi.test(code))
      .map(([path]) => path);
    expect(naming).toEqual([screenChecker]);
    const code = sources[screenChecker] ?? '';
    expect(code).not.toMatch(screenCall);
    // It only looks the API up: a type of what it looks for, and the look.
    expect(withoutComments(code).match(/getDisplayMedia/g)).toHaveLength(2);
    expect(code).toContain('readonly mediaDevices?: { readonly getDisplayMedia?: unknown }');
    expect(code).toContain("typeof devices.getDisplayMedia === 'function'");
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
    for (const example of ['navigator.mediaDevices.getUserMedia({audio:true})', 'x.enumerateDevices()', 'navigator.getUserMedia(c, ok, no)']) {
      expect(deviceApi.test(example), example).toBe(true);
    }
    // The screen API: named anywhere else, it is caught; in share.ts, a call
    // of it is caught.
    for (const example of ['navigator.mediaDevices.getDisplayMedia({video:true})', 'const d = navigator.mediaDevices;', "typeof devices.getDisplayMedia === 'function'"]) {
      expect(screenApi.test(example), example).toBe(true);
    }
    for (const example of ['devices.getDisplayMedia({ video: true })', 'navigator.mediaDevices.getDisplayMedia ()']) {
      expect(screenCall.test(example), example).toBe(true);
    }
    expect(screenCall.test("typeof devices.getDisplayMedia === 'function'")).toBe(false);
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
    // Only the hash names the channel; nothing else may write the address
    // bar, but for the one history call that takes an invite code out of it.
    const navigating = Object.entries(sources)
      .filter(([path, code]) => navigates(path, code))
      .map(([path]) => path);
    expect(navigating).toEqual([]);
  });
});

// What writes the address bar or the history, or moves the page: either
// history write by any route to it (an alias, an index), an assignment to
// location itself or to any of its parts but the hash (which names the
// channel), and assign, replace, reload, go, back and forward.
const navigation: readonly RegExp[] = [
  /\b(push|replace)State\b/,
  /\blocation\s*=(?!=)/,
  /location\.(?!hash\b)\w+\s*\+?=(?!=)/,
  /location\s*\.\s*(assign|replace|reload)\b/,
  /history\s*\.\s*(go|back|forward)\b/,
];

// The one exception (issue #440): route.ts, once, replaces the history entry
// with the address without its fragment, so an invite code read from the hash
// is in neither the address bar nor the back history.
const inviteReplace = "window.history.replaceState(null, '', addressWithoutFragment(window.location))";

// navigates says whether a source writes the address bar other than through
// the hash. Comments are left out first; then the exception is taken out
// (from route.ts only, and only once).
function navigates(path: string, code: string): boolean {
  const bare = withoutComments(code);
  const checked = path === './route.ts' ? bare.replace(inviteReplace, '') : bare;
  return navigation.some((rule) => rule.test(checked));
}

describe('the history exception', () => {
  it('is one call in src/route.ts, given the address without its fragment', () => {
    const code = sources['./route.ts'] ?? '';
    expect(code.split(inviteReplace)).toHaveLength(2);
    // That address is origin, path and query, and nothing else: no hash.
    const built = /export function addressWithoutFragment\([^)]*\): string \{\n\s*return (.*);\n\}/.exec(code)?.[1];
    expect(built).toBe('`${location.origin}${location.pathname}${location.search}`');
  });

  it('still catches every forbidden form: a failing example of each', () => {
    for (const example of [
      'window.location.href = next;',
      'location.search = "?a=1";',
      'location.pathname = p;',
      'window.location.assign(u);',
      'location.replace(u);',
      'history.pushState(null, "", u);',
      "window.history.replaceState(null, '', u);",
      // The exception's own words, outside route.ts.
      inviteReplace,
      // Forms the first rule of #440 let through (security review).
      'window.location = u;',
      'location = u;',
      'location.href += u;',
      'window.location.host = h;',
      'window.location.search += "&a=1";',
      'const h = window.history;\nh.replaceState(null, "", u);',
      "history['replaceState'](null, '', u);",
      'history.go(-1);',
      'window.history.back();',
      'history.forward();',
      'location.reload();',
      'window.location.reload(true);',
      // A comment does not hide code after it on the same line.
      'x(); // note\nlocation.reload();',
    ]) {
      expect(navigates('./App.tsx', example), example).toBe(true);
    }
    // In route.ts: any other history call, the exception with another
    // address, and the exception a second time.
    for (const example of [
      'history.pushState(null, "", u);',
      "window.history.replaceState(null, '', window.location.href);",
      "window.history.replaceState(null, '', addressWithoutFragment(window.location) + window.location.hash);",
      'location.replace(u);',
      `${inviteReplace};\n${inviteReplace};`,
    ]) {
      expect(navigates('./route.ts', example), example).toBe(true);
    }
    expect(navigates('./route.ts', `${inviteReplace};`)).toBe(false);
    // Allowed: setting the hash, reading location, comparing it, and saying
    // in a comment what the code does not do.
    for (const example of [
      'window.location.hash = hash;',
      "const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';",
      'if (window.location.hash !== hash) window.location.hash = hash;',
      'const at = location == null;',
      '// replaceState fires no hashchange, and location.reload() is never called',
    ]) {
      expect(navigates('./App.tsx', example), example).toBe(false);
    }
  });
});

// The invite code (issue #440) is handled like a credential. It is held under
// the name inviteCode, in the modules that must hold it, and nowhere else: it
// is never stored, logged (no module logs at all), put in a URL other than the
// creator's link, or rendered.
const inviteHolders = ['./App.tsx', './Invite.tsx', './api.ts', './route.ts'];
const inviteStored = /(setItem|getItem|removeItem)\([^)]*\binviteCode\b/;
// A template literal's ${...} that holds the code: how a URL would be built.
const inviteInTemplate = /\$\{[^}]*\binviteCode\b[^}]*\}/g;
// In a .tsx file: a JSX child ({inviteCode} after a tag, not an arrow's =>),
// or an attribute other than the one that hands it to the landing page.
const inviteRendered = /(?<!=)>\s*\{[^}]*\binviteCode\b|\b(?!inviteCode=)[\w-]+=\{[^}]*\binviteCode\b/;

describe('the invite code', () => {
  it('is held only by src/api.ts, src/route.ts, App and the landing page', () => {
    const holding = Object.entries(sources)
      .filter(([, code]) => /\binviteCode\b/.test(code))
      .map(([path]) => path)
      .sort();
    expect(holding).toEqual(inviteHolders);
  });

  it('goes to the server from src/api.ts alone', () => {
    const calling = Object.entries(sources)
      .filter(([, code]) => /\/v1\/invites/.test(code))
      .map(([path]) => path);
    expect(calling).toEqual(['./api.ts']);
  });

  it('is never stored, put in a URL or rendered', () => {
    for (const [path, code] of Object.entries(sources)) {
      expect(inviteStored.test(code), path).toBe(false);
      // Only a .tsx file renders (in a .ts file, `Promise<T> {` is a type).
      if (path.endsWith('.tsx')) expect(inviteRendered.test(code), path).toBe(false);
      // The one URL that holds it is the link shown to its creator.
      expect(code.match(inviteInTemplate) ?? [], path).toEqual(path === './route.ts' ? ['${encodeURIComponent(inviteCode)}'] : []);
    }
    expect(sources['./route.ts']).toContain('return `${origin}/app/#/invite/${encodeURIComponent(inviteCode)}`;');
    // A creator's new code goes straight into that link, in the panel alone.
    const reading = Object.entries(sources)
      .filter(([, code]) => /\.value\.code\b/.test(code))
      .map(([path, code]) => [path, code.match(/[\w.]*\(?[^()\n]*\.value\.code\b\)?/g)]);
    expect(reading).toEqual([['./InvitePanel.tsx', ['inviteLink(origin, result.value.code)']]]);
  });

  it('is not missed by the rules: a bare test of each', () => {
    for (const example of ['sessionStorage.setItem(key, inviteCode)', 'window.localStorage.getItem(inviteCode)']) {
      expect(inviteStored.test(example), example).toBe(true);
    }
    for (const example of ['<p>{inviteCode}</p>', '<p>\n  {inviteCode}\n</p>', '<b>x</b>{inviteCode}', '<input value={inviteCode} />', '<li key={inviteCode}>', '<a title={`#/invite/${inviteCode}`}>']) {
      expect(inviteRendered.test(example), example).toBe(true);
    }
    expect('fetch(`/v1/invites/${inviteCode}`)'.match(inviteInTemplate)).toHaveLength(1);
    // Not rendering it: handing it to the landing page, and using it in code.
    for (const example of ['<Invite inviteCode={inviteCode} />', 'useEffect(() => {\n  void api.previewInvite(inviteCode);\n}, [inviteCode]);', 'if (inviteCode !== null) {']) {
      expect(inviteRendered.test(example), example).toBe(false);
    }
  });
});

// A sign-in link's code (issue #493) is held as an invite's is, under the
// name signInCode, by the modules that must hold it and no other. The page
// never builds a sign-in link, so no URL holds it at all.
const signInHolders = ['./App.tsx', './SignInLink.tsx', './api.ts', './route.ts'];
const signInStored = /(setItem|getItem|removeItem)\([^)]*\bsignInCode\b/;
const signInInTemplate = /\$\{[^}]*\bsignInCode\b[^}]*\}/g;
const signInRendered = /(?<!=)>\s*\{[^}]*\bsignInCode\b|\b(?!signInCode=)[\w-]+=\{[^}]*\bsignInCode\b/;

describe('the sign-in link code', () => {
  it('is held only by src/api.ts, src/route.ts, App and the landing page', () => {
    const holding = Object.entries(sources)
      .filter(([, code]) => /\bsignInCode\b/.test(code))
      .map(([path]) => path)
      .sort();
    expect(holding).toEqual(signInHolders);
  });

  it('goes to the server from src/api.ts alone', () => {
    const calling = Object.entries(sources)
      .filter(([, code]) => /\/v1\/sign-in-links/.test(code))
      .map(([path]) => path);
    expect(calling).toEqual(['./api.ts']);
  });

  it('is never stored, put in a URL or rendered', () => {
    for (const [path, code] of Object.entries(sources)) {
      expect(signInStored.test(code), path).toBe(false);
      if (path.endsWith('.tsx')) expect(signInRendered.test(code), path).toBe(false);
      expect(code.match(signInInTemplate) ?? [], path).toEqual([]);
    }
  });

  it('is not missed by the rules: a bare test of each', () => {
    expect(signInStored.test('localStorage.setItem(key, signInCode)')).toBe(true);
    for (const example of ['<p>{signInCode}</p>', '<input value={signInCode} />', '<a title={`#/signin/${signInCode}`}>']) {
      expect(signInRendered.test(example), example).toBe(true);
    }
    expect('fetch(`/v1/sign-in-links/${signInCode}`)'.match(signInInTemplate)).toHaveLength(1);
    for (const example of ['<SignInLink signInCode={signInCode} />', 'if (signInCode !== null) {']) {
      expect(signInRendered.test(example), example).toBe(false);
    }
  });
});

// The diagnostic ring of "Report a problem" (issue #446). It is memory only,
// only api.ts and the voice client record into it, and what they record names
// no path, body, address, channel, room, token, ticket or invite code: those
// are what an event must never hold. diagnostics.test.ts drives each kind
// through the real code; these rules keep a later change from going around it.
const ringRecorders = ['./api.ts', './voice/connection.ts'];
const importsRecord = /import\s*\{[^}]*\brecord\b[^}]*\}\s*from\s*['"](\.\.?\/)+diagnostics['"]/;
const storageNames = /localStorage|sessionStorage|indexedDB|document\.cookie|\bcaches\b/;
// A recording call, to the end of its line (each is one line).
const recordCall = /\brecord\(\{.*$/gm;
const recordsForbidden = /path|body|url|address|ticket|token|channel|room|message|reason|why/i;

describe('the diagnostic ring', () => {
  it('is in memory only: neither the ring nor the form names a storage', () => {
    for (const path of ['./diagnostics.ts', './Report.tsx']) {
      expect(sources[path], path).toBeDefined();
      expect(storageNames.test(withoutComments(sources[path] ?? '')), path).toBe(false);
    }
    // The ring needs nothing at run time but itself.
    expect(withoutComments(sources['./diagnostics.ts'] ?? '')).not.toMatch(/^import (?!type\b)/m);
  });

  it('is recorded into by src/api.ts and the voice client alone', () => {
    const recording = Object.entries(sources)
      .filter(([, code]) => importsRecord.test(code))
      .map(([path]) => path)
      .sort();
    expect(recording).toEqual(ringRecorders);
  });

  it('is told kinds, codes, states and names, never what was sent or where', () => {
    let calls = 0;
    for (const path of ringRecorders) {
      for (const call of (sources[path] ?? '').match(recordCall) ?? []) {
        calls++;
        expect(recordsForbidden.test(call), `${path}: ${call}`).toBe(false);
      }
    }
    // Every recording site was seen: two failures, an open, a close, a
    // reconnect, a voice state and a device error.
    expect(calls).toBe(7);
  });

  it('is not missed by the rules: a bare test of each', () => {
    expect(importsRecord.test("import { record } from './diagnostics';")).toBe(true);
    expect(importsRecord.test("import { forget, record } from '../diagnostics';")).toBe(true);
    expect(importsRecord.test("import { recent } from './diagnostics';")).toBe(false);
    for (const bad of ["record({ kind: 'http_failure', status: 0, error: path })", 'record({ kind: "device_error", name: room.name })', "record({ kind: 'reconnect', socket: url, attempt: 1 })"]) {
      expect(recordsForbidden.test(bad), bad).toBe(true);
    }
    expect(storageNames.test('window.sessionStorage.setItem(k, v)')).toBe(true);
    expect(storageNames.test('indexedDB.open("x")')).toBe(true);
  });
});

describe('the built bundle', () => {
  it('uses the screen API only as the SDK and src/voice/share.ts do, and storage only as the SDK and src/api.ts do', async () => {
    const result = await build({ configFile: 'vite.config.ts', logLevel: 'silent', build: { write: false } });
    const outputs = (Array.isArray(result) ? result : [result]).flatMap((r) => ('output' in r ? r.output : []));
    const code = outputs.map((o) => (o.type === 'chunk' ? o.code : '')).join('\n');
    expect(code.length).toBeGreaterThan(1000);
    // getDisplayMedia (issue #444) and localStorage (issue #439), counted
    // module by module: every mention in the bundle comes from a module the
    // table names, exactly as often as it says, so a mention from any other
    // module fails here.
    for (const [needle, table, minifiedAway] of [
      ['getDisplayMedia', screenMentions, screenMinifiedAway],
      ['localStorage', storageMentions, 0],
    ] as const) {
      const found: Record<string, number> = {};
      let inModules = 0;
      for (const chunk of outputs) {
        if (chunk.type !== 'chunk') continue;
        for (const [id, rendered] of Object.entries(chunk.modules)) {
          const moduleCode = rendered.code ?? '';
          const at = allIndexes(moduleCode, needle);
          if (at.length === 0) continue;
          const source = sourceOf(id);
          found[source] = (found[source] ?? 0) + at.length;
          inModules += at.length;
          // The SDK's storage mentions are its logger's (loglevel), each
          // beside its storage key.
          if (needle === 'localStorage' && source === livekitBundle) {
            for (const i of at) expect(moduleCode.slice(Math.max(0, i - 600), i + 600)).toMatch(/loglevel|storageKey/);
          }
        }
      }
      expect(found, needle).toEqual(table);
      // Nor does anything outside a module (code the bundler wrote itself):
      // the bundle has exactly the modules' mentions, less any the minifier
      // takes out.
      expect(allIndexes(code, needle).length, needle).toBe(inModules - minifiedAway);
    }
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

// Where the built bundle may name getDisplayMedia, and how many times.
//
// The SDK (livekit-client 2.22.3) names it 7 times in its module, measured on
// the pinned version: its browser shim (4) and its picker call,
// LocalParticipant.createScreenTracks (3), which the page uses since issue
// #444. Minifying drops the shim's function name, which is why the whole
// bundle's count before #444 was 6; using the picker call added none.
// src/voice/share.ts names it once, to see whether the browser has a picker
// at all. A bump of the SDK that changes the count fails here and is looked at.
const screenMentions: Record<string, number> = {
  'src/voice/share.ts': 1,
  [livekitBundle]: 7,
};
// The shim's function name, which the minifier drops.
const screenMinifiedAway = 1;

function allIndexes(code: string, needle: string): number[] {
  const found: number[] = [];
  for (let i = code.indexOf(needle); i !== -1; i = code.indexOf(needle, i + 1)) found.push(i);
  return found;
}
