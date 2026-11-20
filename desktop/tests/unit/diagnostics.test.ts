// The diagnostics bundle (issue #2084): the ring's redaction over hostile
// lines, its bound, the bundle's size cap, and the shown-before-saved rule.
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import { describe, expect, it } from 'vitest';

import { BUNDLE_MAX_BYTES, buildBundle, Diagnostics, diagnosticsLogger, LINE_MAX_CHARS, LogRing, RING_LINES, scrubLine } from '../../main/diagnostics.ts';

const NOW = new Date('2026-11-20T10:00:00Z');

// Written in two parts: this file's source holds no absolute URL.
const url = (rest: string): string => ['https:', rest].join('//');

// Exact expressions, scoped to their source file. New values need review at
// the opt-in call site, even when a scrub rule would catch their current form.
const ringValues: Readonly<Record<string, readonly string[]>> = {
  'api/feeds.ts': [
    'sub.kind', // FeedKind selected by the IPC parser.
    'held.kind', // The socket's stored FeedKind.
    'why', // Ending, the main-owned subscription close reasons.
    'what', // Fixed socket error/close outcomes from main.
    'this.#counts()', // Two numeric socket counts with fixed labels.
    'String(this.#invalid)', // Count of dropped frames.
    'String(CLOSE_DEADLINE_MS)', // Numeric close deadline in milliseconds.
  ],
  'installed.ts': [
    'why', // Two fixed reasons for an unreadable installed-version stamp.
  ],
  'ipc.ts': [
    'reason', // IPC Refusal, chosen by main.
    'String(this.#refusals)', // Count of refused calls.
  ],
  'profiles/service.ts': [
    'reason', // The fixed missing or refused bound-credential reason.
  ],
  'vault/vault.ts': [
    'name', // Fixed vault event names, including unprotected storage states.
  ],
  'voice/endpoint.ts': [
    'String(removed)', // Count of removed stale sockets.
  ],
  'voice/oneshot.ts': [
    'what', // OneShotOperation, the helper commands selected by main.
    'outcome', // VoiceOneShotError, a fixed helper outcome.
    'exitNote', // Fixed text with an optional numeric exit status.
    'err.reason', // HelperError's closed HelperRefusal reason.
  ],
  'voice/renderer-diagnostics.ts': [
    'lines[diagnostic]', // A fixed message selected by a validated VoiceDiagnostic.
  ],
  'voice/share-audio.ts': [
    'reason', // ShareSoundReason or RemoveReason, both closed vocabularies.
    'sent.error', // A validated voice command error or a fixed local failure.
  ],
  'voice/supervisor.ts': [
    'String(generation)', // Numeric helper generation.
    'String(run.generation)', // The numeric generation saved on this run.
    'version', // Handshake version selected from supported helper versions.
    'phase', // Validated helper phase or the fixed word "ended".
    'reason', // HelperRefusal, EndpointRefusal, LossReason or DETAIL_REASONS.
    'exitStatus', // OS signal name or fixed text with a numeric/null exit code.
    'String(run.stderr.lines.length)', // Count of omitted stderr lines.
    'ev.error.code', // Validated helper event error code.
    "pttProblem ?? 'no reason'", // Validated talk key problem or fixed fallback.
    "talkMode ?? 'unknown'", // Validated talk mode or fixed fallback.
    'c.kind', // Command kind from the helper protocol vocabulary.
    "windows ? 'killing it' : 'SIGKILL'", // The two fixed platform outcomes.
  ],
};

// Require every opt-in to be an inline literal option beside a string literal
// or template. Aliases and computed log messages should fail closed here too.
function unsafeRingCalls(file: string, source: string): string[] {
  const errors: string[] = [];
  for (const match of source.matchAll(/(?<=[{,]\s*)(?:ring\b|['"]ring['"]|\[\s*['"]ring['"]\s*\])\s*:/gu)) {
    const before = source.slice(0, match.index);
    const where = `${file}:${String(before.split('\n').length)}`;
    const after = source.slice(match.index + match[0].length);
    const call = /\blog\s*\(\s*(?:'(?:\\[\s\S]|[^'\\])*'|"(?:\\[\s\S]|[^"\\])*"|`((?:\\[\s\S]|[^`\\])*)`)\s*,\s*\{\s*$/u.exec(before);
    if (call === null || !match[0].startsWith('ring') || !/^\s*true\s*,?\s*\}\s*,?\s*\)/u.test(after)) {
      errors.push(`${where} requires an inline log call with a literal first argument`);
      continue;
    }
    for (const [, value] of (call[1] ?? '').matchAll(/\\[\s\S]|\$\{([^}]*)\}/gu)) {
      const expression = value?.trim();
      if (expression !== undefined && !/^-?\d+(?:\.\d+)?$/u.test(expression) && !(ringValues[file] ?? []).includes(expression)) {
        errors.push(`${where} has an unreviewed interpolation ${expression}`);
      }
    }
  }
  return errors;
}

describe('diagnostic export call sites', () => {
  it('admits only fixed text and reviewed numeric or closed-vocabulary interpolations', () => {
    const main = join(import.meta.dirname, '../../main');
    const errors = readdirSync(main, { recursive: true, withFileTypes: true })
      .filter((entry) => entry.isFile() && entry.name.endsWith('.ts'))
      .flatMap((entry) => {
        const path = join(entry.parentPath, entry.name);
        return unsafeRingCalls(relative(main, path).split(sep).join('/'), readFileSync(path, 'utf8'));
      });
    expect(errors).toEqual([]);
  });

  it('allows numeric literals and the reviewed values for their own file', () => {
    const sample = 'deps.log(`refused ${reason}; ${4} so far`, { ring: true });';
    expect(unsafeRingCalls('ipc.ts', sample)).toEqual([]);
    expect(unsafeRingCalls('sample.ts', sample)).toHaveLength(1);
  });

  it.each([
    'deps.log(`helper failed: ${err.message}`, { ring: true });',
    'deps.log(\n  `joined ${channel}`,\n  {\n    ring: true,\n  },\n);',
    'deps.log(summary, { ring: true });',
    'deps.log(`requested ${channel}`, { ring: (true) });',
    'deps.log(`requested ${channel}`, { ["ring"]: true });',
    'const options = { "ring": true }; deps.log(channel, options);',
    'deps.log(`joined \\\\${channel}`, { ring: true });',
  ])('rejects unreviewed exports in %s', (source) => {
    expect(unsafeRingCalls('sample.ts', source)).toHaveLength(1);
  });
});

// Each: a hostile line, and strings that must not survive in the ring.
const hostile: readonly (readonly [string, string, readonly string[]])[] = [
  ['an access token in a URL', `conch-desktop: feed failed ${url('chat.example.org/ws?access_token=abc123SECRET&x=1')}`, ['abc123SECRET', 'chat.example.org', 'access_token']],
  ['a bearer header', 'conch-desktop: sent Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig', ['eyJhbGci', 'payload']],
  ['a token pair', 'conch-desktop: token=sekrit-value-1 password: hunter2', ['sekrit-value-1', 'hunter2']],
  ['a channel and a room', 'conch-desktop: joined #secret-launch room "Payroll Team" in channel @alice', ['secret-launch', 'Payroll', 'alice']],
  ['a profile label in quotes', 'conch-desktop: teardown voice "Acme Corp prod" failed', ['Acme']],
  ['message text', 'conch-desktop: dropped frame {"body":"my password is swordfish"}', ['swordfish']],
  ['a conch link', 'conch-desktop: refused conch://invite/ABCDEFGHJKLMNPQRSTUVWXYZ23456789', ['ABCDEFGH']],
  ['an address', 'conch-desktop: could not reach 192.168.1.20:8443 or box.example.org:443', ['192.168', 'box.example']],
  ['a path with a user name', 'conch-desktop: /home/nick/.config/conch-desktop/profiles.json is damaged', ['nick', '.config']],
  ['a Windows path', 'conch-desktop: C:\\Users\\Nick\\AppData\\conch\\profiles.json is damaged', ['Nick', 'AppData']],
  ['a long key', 'conch-desktop: lk key APIabcdefghijklmnopqrstuvwxyz0123456789', ['APIabcdef']],
  ['control characters that would split the line', 'conch-desktop: a\nconch-desktop: forged line\r\u202egnirts', []],
];

// Exact outcomes for scrub-probe.mjs and scrub-probe2.mjs from the review.
const reviewProbes: readonly (readonly [string, string, string])[] = [
  ['host other TLD', 'conch-desktop: an address did not answer: chat.nickd.me:8443', 'conch-desktop: an address did not answer: [host]'],
  ['host co.uk + path', 'GET chat.example.co.uk/api/v1/invites/AbC123xyz failed', 'GET [host] failed'],
  ['host .xyz', 'connect conch.family.xyz refused', 'connect [host] refused'],
  ['single label host', 'connect nas:8443 refused; localhost:7880 too', 'connect [host] refused; [host] too'],
  ['tailscale', 'dial box.tail1234.ts.net:443', 'dial [host]'],
  ['idn host', 'dial chät.com:443', 'dial [host]'],
  ['email', 'account nick.daniel@gmail.com and nd@njd.me', 'account [redacted] and [redacted]'],
  ['url with scheme', 'open https://chat.example.co.uk/invite/AbC123?x=1', 'open [url]'],
  ['file url with space', 'file:///home/nick/My Documents/secret plan.txt missing', '[url]'],
  ['scheme-less query', 'refused chat.example.org/join?code=AbC123xyz&invite=ZZZ999', 'refused [host]'],
  ['unix path with space', 'cannot read /home/nick/My Stuff/notes about bob.txt', 'cannot read [path]'],
  ['win path with space', 'cannot read C:\\Users\\Nick Daniel\\AppData\\Roaming\\conch\\profiles.json', 'cannot read [path]'],
  ['win fwd slashes', 'cannot read C:/Users/nick/AppData/Roaming/conch/profiles.json', 'cannot read [path]'],
  ['UNC', 'cannot read \\\\fileserver\\share\\nick\\conch.json', 'cannot read [path]'],
  ['relative', 'cannot read conch/profiles/work/vault.bin', 'cannot read [path]'],
  ['one segment', 'cannot read /etc and ~/secret.txt and ./nick/x', 'cannot read [path]'],
  ['path after colon', 'ENOENT:/home/nick/.config/conch/x', 'ENOENT:[path]'],
  ['path after equals', 'path=/home/nick/.config/conch/x', 'path=[path]'],
  ['unlabelled invite', 'the invite AbC123xyz was refused', '[redacted]'],
  ['invite code w/ space', 'invite code: AbC123xyz', '[redacted]'],
  ['invite=', 'invite=AbC123xyz', '[redacted]'],
  ['code=', 'code=AbC123xyz otp: 123456 pin=4321', '[redacted]'],
  ['pass/pwd', 'pass=hunter2 pwd=hunter2 passphrase: correct horse battery staple', '[redacted]'],
  ['password multiword', 'password: correct horse battery staple', '[redacted]'],
  ['cookie list', 'Cookie: session=abc123; csrf=xyz789', '[redacted]'],
  ['api key spaced', 'API key: sk-live-12345', '[redacted]'],
  ['auth=', 'auth=abc123 session=abc123 sid=abc123 sig=abc123 nonce=abc123', '[redacted]'],
  ['22-char b64url', 'refused GZ3kq0aQ2vJm1xYtR8bN4w for join', 'refused [redacted] for join'],
  ['dotted token', 'got cnch_a1b2c3d4e5.f6g7h8i9j0k1.l2m3n4o5p6q7', 'got [host]'],
  ['token is', 'token is abc123def456', '[redacted]'],
  ['bearer', 'Authorization: Bearer abc.def.ghi', '[redacted]'],
  ['jwt', 'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMiJ9.sgn4tur3', '[host]'],
  ['fingerprint', 'cert AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD', 'cert [address]'],
  ['soft hyphen', 'to\u00adken=abc123', '[redacted]'],
  ['zero width split', 'AAAAAAAAAAAAAAAA\u200bBBBBBBBBBBBBBBBB', '[redacted]'],
  ['plain room', 'joined room general as Nick Daniel', '[redacted]'],
  ['unicode channel', 'left #café and #日本語 and #général', 'left [name]'],
  ['glued channel', 'in#general by@nick', 'in[name]'],
  ['spaced name', 'left #my room', 'left [name]'],
  ['fullwidth', 'left ＃secret ＠nick', 'left [name]'],
  ['apostrophes', 'can\'t parse "Bob\'s secret message"', 'can\'t parse "[text]"'],
  ['unbalanced', 'said "meet me at five', 'said "[text]"'],
  ['win path in quotes', 'open "C:\\Users\\nick\\" then "other words"', 'open "[text]" then "[text]"'],
  ['guillemets', 'said «meet me at five» and „hello“ and 「secret」', 'said "[text]" and "[text]" and "[text]"'],
  ['escaped', 'said "he said \\"hi\\" to me" ok', 'said "[text]" ok'],
  ['v6 trailing colon', 'connect 2001:db8::1: no route to host', 'connect [address] no route to host'],
  ['v6 after colon', 'remote:2001:db8::1 refused', 'remote:[address] refused'],
  ['v6 label', 'addr=2001:db8::1 ok; [2001:db8::1]:443; fe80::1%eth0', 'addr=[address] ok; [address]; [address]'],
  ['v6 glued word', 'ip2001:db8::1 and x::1', 'ip[address] and x[address]'],
  ['v6 mapped port', '::ffff:192.168.1.1:443', '[address]'],
  ['v6 full then colon port', '2001:db8:0:0:0:0:0:1:443', '[address]'],
  ['v6 cidr', 'route fe80::/64 and 2001:db8::/32', 'route [address] and [address]'],
  ['v6 trailing junk', '2001:db8::1deadbeef and 2001:db8::1.', '[address] and [address]'],
  ['mac', 'device aa:bb:cc:dd:ee:ff', 'device [address]'],
  ['v4 glued', 'ip192.168.1.10 addr_10.0.0.5 v10.0.0.5', 'ip[address] addr_[address] v[address]'],
  ['v4 decimal/hex', 'connect 3232235777 and 0xC0A80101', 'connect [address] and [address]'],
  ['chrome ver', '130.0.672.59', '130.0.672.59'],
  ['IPv6 with a nine-part address and port', 'connect ETIMEDOUT 2a02:1234:5678:9abc:def0:1234:5678:9abc:443', 'connect ETIMEDOUT [address]'],
  ['loopback IPv6 port', 'connect ECONNREFUSED ::1:8080', 'connect ECONNREFUSED [address]'],
  ['compressed IPv6 port', 'connect ECONNREFUSED 2001:db8::1:443', 'connect ECONNREFUSED [address]'],
  ['forward slash drive path', 'cannot read C:/Users/nick/x.json', 'cannot read [path]'],
  ['truncated quote', 'helper said "the room Payroll West could not be jo', 'helper said "[text]"'],
  ['Token authorization', 'Authorization: Token abc123def456', '[redacted]'],
  ['proxy authorization', 'Proxy-Authorization: Negotiate YIIabc123', '[redacted]'],
  ['URL userinfo without scheme', 'dial nick:hunter2@chat.example.org failed', 'dial [redacted] failed'],
  ['case-insensitive URL schemes', 'open HTTPS://Chat.Example.Org/x and WSS:chat.example.org', 'open [url] and [url]'],
  ['Unicode format characters', 'a\u061cb \u202eevil\u202c c\u00adookie=abc d\ufe0f e\u{e0041}', 'ab evil [redacted]'],
  ['Unicode and ASCII line separators', 'line one\nconch-desktop: forged\r\nline three\u2028four\u0085five\u000bsix', 'line one conch-desktop: forged  line three four five six'],
  ['long run followed by emoji', 'x'.repeat(280) + ' ' + '😀'.repeat(10), '[redacted] ' + '😀'.repeat(10)],
  ['quoted ENOENT path', 'ENOENT: no such file or directory, open \'/home/nick/.config/conch/profiles.json\'', 'ENOENT: no such file or directory, open "[text]"'],
  ['unquoted EACCES path', 'EACCES: permission denied, open /home/nick/.config/conch/profiles.json', 'EACCES: permission denied, open [path]'],
  ['spawn path', 'spawn /opt/Conch/resources/conch-voice ENOENT', 'spawn [path]'],
  ['stack trace path', 'failed at Object.<anonymous> (/home/nick/code/app/main.js:10:5)', 'failed at Object.<anonymous> ([path]'],
  ['stack trace file URL', 'failed (file:///home/nick/code/app/main.js:10:5)', 'failed ([url]'],
  ['plain identity fields', 'room=general channel=payroll profile=Work user=nick name=Nick', '[redacted]'],
  ['LiveKit URL', 'livekit wss://lk.example.org?access_token=eyJabc.def.ghi', 'livekit [url]'],
  ['LiveKit host and token', 'livekit lk.nickd.me access_token=eyJabc.def.ghi', 'livekit [host] [redacted]'],
  ['plain identity and room', 'identity p12 room conch-7f3a joined', '[redacted]'],
  ['pass assignment by itself', 'prefix pass=two words', 'prefix [redacted]'],
  ['session assignment by itself', 'session=abc123', '[redacted]'],
  ['OTP by itself', 'otp: 123456', '[redacted]'],
  ['22-character standard base64 key', 'key seen: GZ3k+0aQ/vJm1xYtR8bN4w', 'key seen: [redacted]'],
  ['tag character inside a cookie label', 'co\u{e0041}okie=secret', '[redacted]'],
  ['Arabic letter mark inside a token label', 'to\u061cken=secret', '[redacted]'],
  ['non-Latin channel by itself', 'left #日本語', 'left [name]'],
];

describe('scrubLine', () => {
  it.each(reviewProbes)('handles the %s probe', (_name, line, expected) => {
    const out = scrubLine(line);
    expect(out).toBe(expected);
    expect(out).not.toMatch(/[\p{Cf}\p{Cc}]/u);
  });

  // All three lengths from scrub-time.mjs, across each backtracking probe.
  const longShapes = [
    ['hyphens', (n: number) => 'a-'.repeat(n / 2)],
    ['dots', (n: number) => 'a.'.repeat(n / 2)],
    ['curly quotes', (n: number) => '“'.repeat(n)],
    ['colons', (n: number) => ':'.repeat(n)],
    ['numeric pairs', (n: number) => '1:'.repeat(n / 2)],
    ['slashes', (n: number) => 'x' + '/a'.repeat(n / 2)],
  ] as const;
  const longProbes = longShapes.flatMap(([name, make]) =>
    [4000, 16000, 64000].map((size) => [name, size, make(size)] as const));
  it.each(longProbes)('bounds the %s probe at size %i', (_name, _size, line) => {
    expect(scrubLine(line)).toBe(`[a ${String(line.length)}-character line, not kept]`);
  });

  it.each(hostile)('keeps nothing of %s', (_name, line, gone) => {
    const out = scrubLine(line);
    for (const secret of gone) expect(out, secret).not.toContain(secret);
    expect(out).not.toMatch(/[\n\r\u202e]/u);
    expect(out.startsWith('conch-desktop:')).toBe(true);
  });

  it('leaves a plain fixed log line as it is', () => {
    const line = 'conch-desktop: feed messages opening; 2 sockets held';
    expect(scrubLine(line)).toBe(line);
  });

  it.each([
    'voice: helper 1 exited (exit code: 1)',
    'voice: session phase: joined',
    'voice: talk key: F9, mute key: F10',
    'share: diagnostic code=capture_stalled',
  ])('keeps what a diagnosis needs: %s', (line) => {
    expect(scrubLine(line)).toBe(line);
  });

  it('still redacts a secret named for one', () => {
    for (const line of ['x api_key=abc123', 'x session_id: s-9', 'x invite_code=QWERTY', 'x ticket=t1']) {
      expect(scrubLine(line)).toBe('x [redacted]');
    }
  });

  it.each([
    ['password="red fox"', '[redacted]'],
    ['key="two words"', '[redacted]'],
    [String.raw`helper failed: "first \"escaped quote\" still private"`, 'helper failed: "[text]"'],
    ['[2001:db8::9]:8443', '[address]'],
    ['2001:db8::9', '[address]'],
    ['2001:db8:0:0:0:0:0:9', '[address]'],
    ['::ffff:192.0.2.1', '[address]'],
    ['a conch: invite link arrived', 'a conch: invite link arrived'],
    ['refused conch:invite/example', 'refused [url]'],
  ])('scrubs %s to %s', (line, expected) => {
    expect(scrubLine(line)).toBe(expected);
  });

  it('does not split an emoji at the line bound', () => {
    expect(scrubLine('a '.repeat(149) + '😀 x')).toBe('a '.repeat(149) + '…');
  });

  it('scrubs a line at the record bound and replaces a line just above it', () => {
    expect(scrubLine('word '.repeat(400))).toHaveLength(LINE_MAX_CHARS);
    expect(scrubLine('word '.repeat(400) + 'x')).toBe('[a 2001-character line, not kept]');
  });

  it('counts the ellipsis within the line bound', () => {
    const line = scrubLine(`conch-desktop: ${'word '.repeat(200)}`);
    expect(line).toHaveLength(LINE_MAX_CHARS);
    expect(line.endsWith('…')).toBe(true);
  });
});

describe('LogRing', () => {
  it('keeps local-only details entirely out of the ring', () => {
    const ring = new LogRing();
    const local: string[] = [];
    const log = diagnosticsLogger(ring, (line) => local.push(line));
    log('voice: helper 1 reconnecting: connect_failed', { ring: true });
    log('voice: helper 1 stderr: private device words');
    expect(local).toEqual(['voice: helper 1 reconnecting: connect_failed', 'voice: helper 1 stderr: private device words']);
    expect(ring.lines()).toEqual(['voice: helper 1 reconnecting: connect_failed']);
  });

  it('holds the last 500 lines, oldest first', () => {
    const ring = new LogRing();
    for (let n = 0; n < RING_LINES + 25; n += 1) ring.record(`conch-desktop: line ${String(n)}`);
    const lines = ring.lines();
    expect(lines).toHaveLength(500);
    expect(lines[0]).toBe('conch-desktop: line 25');
    expect(lines[499]).toBe('conch-desktop: line 524');
  });

  it('holds nothing a hostile line would leak', () => {
    const ring = new LogRing();
    for (const [, line] of hostile) ring.record(line);
    const held = ring.lines().join('\n');
    for (const [, , gone] of hostile) for (const secret of gone) expect(held, secret).not.toContain(secret);
  });

  it('keeps a very long line as a fixed marker, without scrubbing it', () => {
    const ring = new LogRing();
    const start = Date.now();
    const line = `conch-desktop: ${'\u201c'.repeat(64 * 1024)}`;
    ring.record(line);
    expect(Date.now() - start).toBeLessThan(500);
    expect(ring.lines()).toEqual([`[a ${String(line.length)}-character line, not kept]`]);
  });

  it('gives a copy: changing it does not change the ring', () => {
    const ring = new LogRing(2);
    ring.record('a');
    (ring.lines() as string[]).push('b');
    expect(ring.lines()).toEqual(['a']);
  });
});

describe('buildBundle', () => {
  it('keeps version names and values from forging header lines', () => {
    const text = buildBundle({ versions: { 'a\nb': 'c\nlog (last 0 lines' }, lines: [], now: new Date(0) });
    expect(text.split('\n').filter((line) => line.startsWith('log ('))).toEqual(['log (last 0 lines, oldest first):']);
    expect(text).toContain('a b: c log (last 0 lines\n');
  });

  it('has the versions and the lines, and says it was not sent', () => {
    const text = buildBundle({ versions: { electron: '41.0.0', platform: 'linux' }, lines: ['conch-desktop: one'], now: NOW });
    expect(text).toContain('electron: 41.0.0');
    expect(text).toContain('conch-desktop: one');
    expect(text).toContain('not been sent anywhere');
    expect(text).toContain('2026-11-20T10:00:00.000Z');
  });

  it('is at most 256 KiB whatever it is given, dropping the oldest lines and saying so', () => {
    const lines = Array.from({ length: 5000 }, (_, n) => `conch-desktop: ${String(n).padStart(5, '0')} ${'x'.repeat(200)}`);
    const text = buildBundle({ versions: {}, lines, now: NOW });
    expect(Buffer.byteLength(text, 'utf8')).toBeLessThanOrEqual(BUNDLE_MAX_BYTES);
    expect(text).toContain('older log lines left out');
    expect(text).toContain('04999');
    expect(text).not.toContain('conch-desktop: 00000');
  });

  it('is under the cap for a full ring of full-length lines', () => {
    const ring = new LogRing();
    for (let n = 0; n < RING_LINES * 2; n += 1) ring.record(`conch-desktop: ${'a b '.repeat(200)}`);
    const text = buildBundle({ versions: { app: '1.0.0' }, lines: ring.lines(), now: NOW });
    expect(Buffer.byteLength(text, 'utf8')).toBeLessThanOrEqual(BUNDLE_MAX_BYTES);
    expect(text).not.toContain('left out');
  });
});

function fixture(chosen: string | null = '/tmp/out/diag.txt') {
  const ring = new LogRing();
  const written: [string, string][] = [];
  const logs: string[] = [];
  const asked: string[] = [];
  const state = { chosen, fail: false };
  const diagnostics = new Diagnostics({
    ring,
    versions: { electron: '41.0.0' },
    now: () => NOW,
    chooseSavePath: (suggested) => {
      asked.push(suggested);
      return Promise.resolve(state.chosen);
    },
    writeFile: (path, bytes) => {
      if (state.fail) return Promise.reject(new Error('disk'));
      written.push([path, new TextDecoder().decode(bytes)]);
      return Promise.resolve();
    },
    downloadsDir: () => '/home/p/Downloads',
    join: (...parts) => parts.join('/'),
    isAbsolute: (path) => path.startsWith('/'),
    log: (line) => logs.push(line),
  });
  return { ring, written, logs, asked, state, diagnostics };
}

describe('Diagnostics', () => {
  it('writes the preview selected by ID even after a newer preview', async () => {
    const f = fixture();
    f.ring.record('conch-desktop: before');
    const shown = f.diagnostics.preview();
    f.ring.record('conch-desktop: after the preview');
    const newer = f.diagnostics.preview();
    expect(newer.text).toContain('after the preview');
    expect(newer.id).not.toBe(shown.id);
    expect(await f.diagnostics.save(shown.id)).toBe('saved');
    expect(f.written).toEqual([['/tmp/out/diag.txt', shown.text]]);
    expect(shown.text).not.toContain('after the preview');
  });

  it('refuses a save nobody was shown, and writes nothing', async () => {
    const f = fixture();
    expect(await f.diagnostics.save('unknown')).toBe('failed');
    expect(f.asked).toEqual([]);
    expect(f.written).toEqual([]);
  });

  it('keeps only the last four previews', async () => {
    const f = fixture();
    const first = f.diagnostics.preview();
    const retained = Array.from({ length: 4 }, () => f.diagnostics.preview());
    expect(await f.diagnostics.save(first.id)).toBe('failed');
    expect(f.asked).toEqual([]);
    for (const { id } of retained) expect(await f.diagnostics.save(id)).toBe('saved');
  });

  it('suggests a plain name in the downloads directory', async () => {
    const f = fixture();
    const { id } = f.diagnostics.preview();
    await f.diagnostics.save(id);
    expect(f.asked).toEqual(['/home/p/Downloads/conch-diagnostics-2026-11-20.txt']);
  });

  it('writes nothing when the dialog is closed, or gives no usable path, or the write fails', async () => {
    const f = fixture(null);
    const { id } = f.diagnostics.preview();
    expect(await f.diagnostics.save(id)).toBe('cancelled');
    for (const bad of ['', 'relative/path', '/tmp/a\u0000b']) {
      f.state.chosen = bad;
      expect(await f.diagnostics.save(id), bad).toBe('failed');
    }
    f.state.chosen = '/tmp/ok.txt';
    f.state.fail = true;
    expect(await f.diagnostics.save(id)).toBe('failed');
    expect(f.written).toEqual([]);
    expect(f.logs).not.toContain(expect.stringContaining('/tmp'));
  });
});
