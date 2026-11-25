// The diagnostics bundle (issue #2084): the ring's redaction over hostile
// lines, its bound, the bundle's size cap, and the shown-before-saved rule.
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import type { Session } from 'electron';
import { describe, expect, it } from 'vitest';

import { RECORD_MAX_CHARS, BUNDLE_MAX_BYTES, buildBundle, Diagnostics, DiagnosticSamples, diagnosticsLogger, GeometricSampler, LINE_MAX_CHARS, LogRing, MAX_SAMPLE_KEYS, RING_LINES, scrubLine, type BlockedDiagnosticKey, type FeedDiagnosticEvent, type MainLog, type Refusal } from '../../main/diagnostics.ts';
import { guardSession } from '../../main/guards.ts';
import { voiceDiagnosticLog } from '../../main/voice/renderer-diagnostics.ts';
import type { FeedKind, VoiceDiagnostic } from '../../../web/src/platform/desktop/bridge.ts';

const NOW = new Date('2026-11-20T10:00:00Z');

// Written in two parts: this file's source holds no absolute URL.
const url = (rest: string): string => ['https:', rest].join('//');

// Exact expressions, scoped to their source file. New values need review at
// the opt-in call site, even when a scrub rule would catch their current form.
const ringValues: Readonly<Record<string, readonly string[]>> = {
  'guards.ts': [
    'String(blocked)', // Count of blocked requests since start.
  ],
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
    'got.reason', // SessionOnlyReason, the vault's closed vocabulary (#2077).
    'why', // SessionOnlyReason, or the fixed words failed / temporarilyUnavailable (#2077).
    'outcome', // LegacyOutcome, the closed vocabulary of the v10 sweep's lines (#2217).
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
  // Every 'ring:' key, wherever it stands (a comment before it must not hide
  // it: #2089's review). diagnostics.ts declares the option and is exempt.
  if (file === 'diagnostics.ts') return errors;
  for (const match of source.matchAll(/(?:\bring\b|['"]ring['"]|\[\s*['"]ring['"]\s*\])\s*:/gu)) {
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

  // The ways past the key check (#2089's review): a shorthand { ring } option,
  // and writing to the ring directly. The tree has exactly one of each, pinned.
  it('has no shorthand ring option and no direct ring write but the pinned ones', () => {
    const main = join(import.meta.dirname, '../../main');
    const found = readdirSync(main, { recursive: true, withFileTypes: true })
      .filter((entry) => entry.isFile() && entry.name.endsWith('.ts'))
      .flatMap((entry) => {
        const file = relative(main, join(entry.parentPath, entry.name)).split(sep).join('/');
        const source = readFileSync(join(entry.parentPath, entry.name), 'utf8');
        return [...source.matchAll(/[{,]\s*ring\s*[,}]|\.record\s*\(/gu)].map((m) => `${file}: ${m[0].replace(/\s+/gu, ' ').trim()}`);
      });
    // app.ts hands the ring to Diagnostics ({ ring, ... }); diagnosticsLogger records.
    expect(found.sort()).toEqual(['app.ts: { ring,', 'diagnostics.ts: .record('].sort());
  });

  it('refuses a ring option hidden behind a comment', () => {
    expect(unsafeRingCalls('ipc.ts', 'deps.log(`refused ${reason}`, { /* export */ ring: true });')).toHaveLength(1);
    expect(unsafeRingCalls('ipc.ts', 'deps.log(`refused`, {\n  // export\n  ring: true,\n});')).toHaveLength(1);
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

// The fixed log lines reviewed in vocab.mjs and fixed2.mjs.
const fixedVocabulary = [
  'voice: unavailable: runtime-dir-not-directory',
  'voice: unavailable: runtime-dir-not-private',
  'voice: unavailable: runtime-dir-not-owned',
  'voice: helper 2 exited (code 3221226505)',
  'voice: helper 2 exited (code 1073741819)',
  'voice: config set: failed (exit status 3221225477)',
  'app: 0.1.0-alpha.20',
  'chrome: 144.0.7559.60',
  'conch-desktop: compat.json is missing or malformed; no server release is supported',
  'conch-desktop: CONCH_LINUX_SHARE_AUDIO is on: the Linux screen share offers its sound (experimental)',
  'conch-desktop: could not register the conch: protocol',
  'voice: helper 3 speaks conch.voice_control.v1',
  'voice: helper 3 speaks conch.voice_control.v2',
  'voice: helper 3 speaks conch.voice_control.v3',
  'voice: unavailable: runtime-dir-not-absolute',
  'voice: unavailable: socket-path-too-long',
  'voice: unavailable: profile-id-invalid',
  'voice: unavailable: runtime-dir-missing',
  'voice: helper 3 exited (code 3221225477)',
  'voice: helper 3 exited (code null)',
  'voice: helper 3 exited (SIGKILL)',
  'voice: config set-mute-binding: failed (exit status 101)',
  'voice: config set: unsupported (exit status 2)',
  'voice: devices: not-installed',
  'voice: helper 3 reconnecting: server_unreachable: no detail from the helper',
  'voice: helper 3 ended: connect_failed',
  'voice: Windows talk key lost (device_removed, mode push_to_talk)',
  'voice: helper 3 did not answer set_share_audio_mute in time',
  'voice: helper 3 reported id_not_increasing',
  'voice: helper 3 lost: ack-timeout',
  'voice: removed 2 stale socket(s) from the profile\'s runtime directory',
  'share sound: the share’s channel can no longer be named',
  'share sound: ending (presence share end)',
  'share sound: not carried (no-process-tree)',
  'share sound: the helper refused the command (id_not_increasing)',
  'conch-desktop: feed occupancy closed (signed out); 3 open, 1 closing',
  'conch-desktop: feed messages socket destroyed: no close within 5000 ms; 3 open, 1 closing',
  'conch-desktop: a voice feed socket did not close within 5000 ms and cannot be destroyed; 0 open, 1 closing',
  'conch-desktop: dropped an invalid messages frame; 100 so far',
  'conch-desktop: refused an IPC call (name); 1000000000 so far',
  'conch-desktop: a profile sign-in was refused: other-account',
  'vault: temporarilyUnavailable',
  'vault: reencrypt-failed',
  'installed: the installed version stamp is not a version; updates cannot be noticed',
  'voice: helper 2 exited (code -1)',
  'voice: helper 2 exited (code 42949672950)',
  'voice: unavailable: runtime-dir-symlink',
];

// probe3.mjs explored arbitrary input outside the opt-in boundary. The last
// column records canaries the scrub already missed so this checks for new
// leaks without requiring the scrub to recognise arbitrary private prose.
const exploratoryProbes: readonly (readonly [string, string, readonly string[], readonly string[]])[] = [
  ['room_name=', 'join failed room_name=Payroll West', ['Payroll'], ['Payroll']],
  ['channel_name:', 'channel_name: payroll-west', ['payroll'], ['payroll']],
  ['channelName:', 'left channelName: payroll', ['payroll'], ['payroll']],
  ['username=', 'Digest username=nickd realm=conch', ['nickd'], ['nickd']],
  ['display_name=', 'display_name=Nick Daniel', ['Nick', 'Daniel'], ['Nick', 'Daniel']],
  ['profileName=', 'profileName=Work', ['Work'], ['Work']],
  ['user_id/name json-ish', 'principal{id:12 displayName:Nick}', ['Nick'], ['Nick']],
  ['key: (colon)', 'key: sk-live-12345', ['sk-live-12345'], ['sk-live-12345']],
  ['signature=', 'signature=ab12cd34ef56', ['ab12cd34ef56'], []],
  ['jwt= short', 'jwt=abc.def.ghi', ['abc.def.ghi'], []],
  ['psk/seed/salt/hash', 'psk=hunter2 seed=abcd1234 hash=deadbeef', ['hunter2', 'abcd1234'], ['hunter2', 'abcd1234']],
  ['code (no sep)', 'invite AbC123xyz was refused', ['AbC123xyz'], ['AbC123xyz']],
  ['code after "code"', 'the code AbC123xyz expired', ['AbC123xyz'], ['AbC123xyz']],
  ['exit code exemption abuse', 'exit code: AbC123xyz', ['AbC123xyz'], []],
  ['diagnostic code exemption abuse', 'diagnostic code=AbC123xyz', ['AbC123xyz'], []],
  ['token no sep', 'token abc123def456 rejected', ['abc123def456'], ['abc123def456']],
  ['password no sep', 'password hunter2 rejected', ['hunter2'], ['hunter2']],
  ['token ->', 'token -> abc123def456', ['abc123def456'], ['abc123def456']],
  ['token (paren)', 'token (abc123def456) rejected', ['abc123def456'], ['abc123def456']],
  ['fullwidth =', 'token＝abc123def456', ['abc123def456'], ['abc123def456']],
  ['fullwidth :', 'password：hunter2', ['hunter2'], ['hunter2']],
  ['fullwidth label', 'ｔｏｋｅｎ=abc123def456', ['abc123def456'], ['abc123def456']],
  ['variation selector in label', 'to\ufe0fken=abc123def456', ['abc123def456'], ['abc123def456']],
  ['combining mark in label', 'to\u0301ken=abc123def456', ['abc123def456'], ['abc123def456']],
  ['tab in label', 'pass\tword=hunter2', ['hunter2'], ['hunter2']],
  ['short unlabelled token', 'refused cnch_a1b2c3d4e5f6 for join', ['cnch_a1b2c3d4e5f6'], ['cnch_a1b2c3d4e5f6']],
  ['21-char token', 'refused GZ3kq0aQ2vJm1xYtR8bN4 for join', ['GZ3kq0aQ2vJm1xYtR8bN4'], ['GZ3kq0aQ2vJm1xYtR8bN4']],
  ['token split by dot', 'got abcdefghij.klmnopqrst', ['abcdefghij'], []],
  ['token split by colon', 'got cnch:abcdefghijklmnopqrstu', ['abcdefghijklmnopqrstu'], ['abcdefghijklmnopqrstu']],
  ['hex 16', 'session deadbeefdeadbeef ended', ['deadbeefdeadbeef'], ['deadbeefdeadbeef']],
  ['base64 with spaces', 'got GZ3kq0aQ2vJm 1xYtR8bN4wAb', ['GZ3kq0aQ2vJm'], ['GZ3kq0aQ2vJm']],
  ['percent-encoded token', 'got %47%5A%33%6B%71%30%61%51%32%76%4A%6D', ['%47%5A'], ['%47%5A']],
  ['room word', 'left room general', ['general'], ['general']],
  ['in channel', 'no such channel payroll-west', ['payroll'], ['payroll']],
  ['profile word', 'profile Work could not be opened', ['Work'], ['Work']],
  ['user word', 'user nickd not found', ['nickd'], ['nickd']],
  ['bracket name', 'channel [payroll] <payroll> (payroll)', ['payroll'], ['payroll']],
  ['livekit sids', 'participant PA_x7Kq2mZp9LbT in RM_a8Jw3nYq5VcR', ['PA_x7Kq2mZp9LbT', 'RM_a8Jw3nYq5VcR'], ['PA_x7Kq2mZp9LbT', 'RM_a8Jw3nYq5VcR']],
  ['room conch id', 'publish to room conch-7f3a2b1c', ['conch-7f3a2b1c'], ['conch-7f3a2b1c']],
  ['as name', 'joined as Nick Daniel', ['Nick'], ['Nick']],
  ['CN=', 'cert CN=Nick Daniel, O=Home', ['Nick'], ['Nick']],
  ['message:', 'message: meet me at five', ['meet me at five'], ['meet me at five']],
  ['text=', 'text=meet me at five', ['meet me at five'], ['meet me at five']],
  ['body json unquoted', 'body {text:meet me at five}', ['meet me'], ['meet me']],
  ['py prefix quote', 'said u\'meet me at five\'', ['meet me at five'], ['meet me at five']],
  ['quote after letter', 'msg\'meet me at five\'', ['meet me at five'], ['meet me at five']],
  ['swedish quotes', 'said ”meet me at five”', ['meet me at five'], ['meet me at five']],
  ['reversed guillemets', 'said »meet me at five«', ['meet me at five'], ['meet me at five']],
  ['single guillemets', 'said ‹meet me at five›', ['meet me at five'], ['meet me at five']],
  ['cjk quotes', 'said 『meet me at five』 《x y》', ['meet me at five'], ['meet me at five']],
  ['fullwidth quotes', 'said ＂meet me at five＂', ['meet me at five'], ['meet me at five']],
  ['low-9 single', 'said ‚meet me at five‘', ['meet me at five'], ['meet me at five']],
  ['typographic apostrophe pair', 'said ’meet me at five’', ['meet me at five'], ['meet me at five']],
  ['quote closed then text', 'said "a" meet me at five', ['meet me at five'], ['meet me at five']],
  ['escaped-quote spill', 'said "a\\" meet me at five', ['meet me at five'], []],
  ['relative backslash', 'cannot read Users\\nick\\AppData\\conch.json', ['nick'], ['nick']],
  ['root backslash', 'cannot read \\Users\\nick\\conch.json', ['nick'], ['nick']],
  ['env var path', 'cannot read %APPDATA%\\conch\\nickd\\x', ['nickd'], ['nickd']],
  ['tilde user', 'cannot read ~nick/secret', ['nick'], ['nick']],
  ['one-slash relative', 'cannot read nick/secret', ['nick'], ['nick']],
  ['glued path', 'ENOENTopen/home/nick/x', ['nick'], []],
  ['path after letter', 'at/home/nick', ['nick'], []],
  ['path in parens', 'spawn(/home/nick/bin/x)', ['nick'], []],
  ['path after digit', 'errno2/home/nick/x', ['nick'], []],
  ['windows drive lowercase no sep', 'c:Users\\nick', ['nick'], ['nick']],
  ['windows device path', '\\\\?\\C:\\Users\\nick', ['nick'], []],
  ['home env', 'HOME=/home/nick XDG=/run/user/1000', ['nick'], []],
  ['path then quoted', '/home/nick "x"', ['nick'], []],
  ['$HOME', 'cannot read $HOME/nick/x', ['nick'], []],
  ['data url', 'open data:text/plain;base64,bWVldCBtZQ==', ['bWVldCBtZQ'], ['bWVldCBtZQ']],
  ['scheme-less localhost path', 'GET localhost/AbC123xyz', ['AbC123xyz'], ['AbC123xyz']],
  ['scheme-less single label + query', 'GET nas?code=AbC123xyz', ['AbC123xyz'], []],
  ['mailto', 'open mailto:nick@example.org', ['nick'], []],
  ['magnet/urn', 'urn:uuid:550e8400-e29b-41d4-a716-446655440000', ['550e8400'], []],
  ['url split by space', 'open https ://chat.nickd.me/x', ['nickd'], []],
  ['conch: space', 'open conch: //join/AbC123xyz', ['AbC123xyz'], ['AbC123xyz']],
  ['url fragment after host', 'chat.nickd.me#invite=AbC123xyz', ['AbC123xyz'], []],
  ['query only', 'refused ?t=AbC123xyz&c=payroll', ['AbC123xyz', 'payroll'], ['AbC123xyz', 'payroll']],
  ['slash-join path', 'GET /join/AbC123xyz', ['AbC123xyz'], []],
  ['email at single label', 'account nick@localhost', ['nick'], ['nick']],
  ['ssh style', 'dial nick@nas:8443', ['nick'], ['nick']],
  ['digit-first host:port', 'dial 1nas:8443', ['1nas'], ['1nas']],
  ['underscore host:port', 'dial conch_db:5432', ['conch_db'], ['conch_db']],
  ['bare single label', 'getaddrinfo ENOTFOUND nickd-nas', ['nickd-nas'], ['nickd-nas']],
  ['dashed mac', 'device aa-bb-cc-dd-ee-ff', ['aa-bb-cc-dd-ee-ff'], ['aa-bb-cc-dd-ee-ff']],
  ['dotted mac (cisco)', 'device aabb.ccdd.eeff', ['aabb'], []],
  ['ipv4 leading zero', 'connect 010.001.001.001', ['010.001'], ['010.001']],
  ['ipv4 three part', 'connect 192.168.1', ['192.168.1'], ['192.168.1']],
  ['ipv4 dashed', 'host ip-192-168-1-10', ['192-168-1-10'], ['192-168-1-10']],
  ['ipv4 with spaces', 'connect 192 . 168 . 1 . 10', ['168'], ['168']],
  ['ipv4 five-part', 'connect 192.168.1.10.5', ['192.168.1.10'], []],
  ['ipv4 then version-like', 'v 300.168.1.10', ['300.168.1.10'], ['300.168.1.10']],
  ['ipv4 long', 'connect 192.168.001.010', ['192.168'], ['192.168']],
  ['ipv6 one colon pair', 'route fe80:1', ['fe80:1'], []],
  ['ipv6 upper glued letters', 'xfe80::1', ['fe80'], ['fe80']],
  ['ipv6 after g', 'logfe80::abcd:1', ['abcd'], []],
  ['ipv4 hex dotted', 'connect 0xC0.0xA8.0x01.0x01', ['0xC0'], ['0xC0']],
  ['ipv4 9-digit decimal', 'connect 167772165', ['167772165'], ['167772165']],
  ['email quoted local', 'o\'brien@example.org', ['o\'brien', 'brien'], []],
  ['phone', 'call +1 415 555 0100', ['555'], ['555']],
];

describe('scrubLine', () => {
  it.each(fixedVocabulary)('keeps the reviewed fixed line %s', (line) => {
    expect(scrubLine(line)).toBe(line);
  });

  it.each(exploratoryProbes)('introduces no newly visible canaries in %s', (_name, line, canaries, alreadyVisible) => {
    const out = scrubLine(line);
    expect(canaries.filter((canary) => out.includes(canary) && !alreadyVisible.includes(canary))).toEqual([]);
  });

  it.each([
    ['voice: helper 2 speaks conch.voice_control.v2?token=private', 'private'],
    ['voice: helper 2 exited (code 3221225477) password: private', 'private'],
    ['voice: unavailable: runtime-dir-not-private /home/private/file', '/home/private/file'],
    ['vault: temporarilyUnavailable user=private', 'user=private'],
  ])('still scrubs an adjoining payload in %s', (line, canary) => {
    expect(scrubLine(line)).not.toContain(canary);
  });

  it.each(reviewProbes)('handles the %s probe', (_name, line, expected) => {
    const out = scrubLine(line);
    expect(out).toBe(expected);
    expect(out).not.toMatch(/[\p{Cf}\p{Cc}]/u);
  });

  it.each([
    ['U+3164', '\u3164'],
    ['U+115F', '\u115f'],
    ['U+1160', '\u1160'],
    ['U+FFA0', '\uffa0'],
  ])('scrubs a path after the invisible filler %s', (_name, filler) => {
    expect(scrubLine(`cannot read ${filler}/home/nick/secret plan.txt`)).toBe('cannot read [path]');
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
    'voice: helper 1 exited (code 1)',
    'voice: session phase: joined',
    'voice: talk key: F9, mute key: F10',
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
    const atBound = 'word '.repeat(RECORD_MAX_CHARS / 4).slice(0, RECORD_MAX_CHARS);
    expect(scrubLine(atBound)).toHaveLength(LINE_MAX_CHARS);
    expect(scrubLine(`${atBound}x`)).toBe(`[a ${String(RECORD_MAX_CHARS + 1)}-character line, not kept]`);
  });

  it('counts the ellipsis within the line bound', () => {
    const line = scrubLine(`conch-desktop: ${'word '.repeat(80)}`);
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

const refusalKeys = Object.keys({
  sender: true, destroyed: true, frame: true, subframe: true, origin: true,
  generation: true, name: true, rate: true, size: true, shape: true, gesture: true, disabled: true, error: true,
} satisfies Record<Refusal, true>);
const feedKinds = Object.keys({
  messages: true, events: true, voice: true, occupancy: true,
} satisfies Record<FeedKind, true>);
const feedEvents = Object.keys({
  opening: true, unsubscribed: true, document: true, 'signed out': true, server: true, behind: true,
  'socket closed': true, 'socket destroyed': true, 'close deadline': true, 'invalid frame': true,
} satisfies Record<FeedDiagnosticEvent, true>);
const rendererCodes = Object.keys({
  'capture-stalled': true, 'encoder-cpu': true, 'sdp-retry': true, 'sdp-rejected': true,
  'sdp-bundle-collision': true, 'sdk-warning': true, 'sdk-error': true,
} satisfies Record<VoiceDiagnostic, true>) as VoiceDiagnostic[];

describe('GeometricSampler', () => {
  it.each([
    { name: 'IPC refusals', keys: refusalKeys },
    { name: 'feed kinds × events', keys: feedKinds.flatMap((kind) => feedEvents.map((event) => `${kind} ${event}`)) },
    { name: 'blocked requests', keys: Object.keys({ blocked: true } satisfies Record<BlockedDiagnosticKey, true>) },
    { name: 'renderer diagnostics', keys: rendererCodes },
  ])('fits every key for $name under the sampler cap', ({ keys }) => {
    expect(keys.length).toBeLessThanOrEqual(MAX_SAMPLE_KEYS);
    const sampler = new GeometricSampler<string>();
    for (const key of keys) expect(sampler.sample(key), key).toBe(true);
    expect(sampler.counts()).toEqual(keys.map((key) => [key, 1]));
  });

  // burn.mjs counted how many export slots a million events of one key use.
  it.each([
    { events: 1, samples: 1 },
    { events: 7, samples: 3 },
    { events: 8, samples: 4 },
    { events: 1_000_000, samples: 20 },
  ])('uses $samples export slots for $events events of one key', ({ events, samples }) => {
    const sampler = new GeometricSampler<'event'>();
    let exported = 0;
    for (let n = 0; n < events; n++) if (sampler.sample('event')) exported++;
    expect(exported).toBe(samples);
    expect(sampler.counts()).toEqual([['event', events]]);
  });

  it('keeps its first 64 keys and their sampling sequence when more arrive', () => {
    const sampler = new GeometricSampler<string>();
    const keys = Array.from({ length: 64 }, (_, n) => `key ${String(n)}`);
    expect(keys.map((key) => sampler.sample(key))).toEqual(keys.map(() => true));
    for (let n = 0; n < 1_000; n++) expect(sampler.sample(`overflow ${String(n)}`)).toBe(false);
    expect(sampler.sample('overflow 0')).toBe(false);
    expect(sampler.counts()).toEqual(keys.map((key) => [key, 1]));
    expect(sampler.sample('key 0')).toBe(true);
    expect(sampler.sample('key 0')).toBe(false);
    expect(sampler.count('key 0')).toBe(3);
  });
});

describe('buildBundle header keys', () => {
  it('prints a key that is not a closed-vocabulary word as other, so it cannot forge a line', () => {
    const forged = 'rate\nBlocked requests since start: 0' as unknown as Parameters<typeof buildBundle>[0]['refusalCounts'] extends readonly (readonly [infer K, number])[] | undefined ? K : never;
    const text = buildBundle({ versions: {}, lines: [], refusals: 3, refusalCounts: [[forged, 2], ['rate', 1]], now: new Date(0) });
    expect(text).toContain('IPC refusals since start: 3 (other 2, rate 1)');
    expect(text.split('\n').filter((line) => line.startsWith('Blocked requests since start:'))).toHaveLength(1);
  });
});

describe('buildBundle', () => {
  it('keeps version names and values from forging header lines', () => {
    const text = buildBundle({ versions: { 'a\nb': 'c\nlog (last 0 lines' }, lines: [], refusals: 0, now: new Date(0) });
    expect(text.split('\n').filter((line) => line.startsWith('log ('))).toEqual(['log (last 0 lines, oldest first):']);
    expect(text).toContain('a b: c log (last 0 lines\n');
  });

  it('has the versions and the lines, and says it was not sent', () => {
    const text = buildBundle({ versions: { electron: '41.0.0', platform: 'linux' }, lines: ['conch-desktop: one'], refusals: 0, now: NOW });
    expect(text).toContain('electron: 41.0.0');
    expect(text).toContain('conch-desktop: one');
    expect(text).toContain('not been sent anywhere');
    expect(text).toContain('2026-11-20T10:00:00.000Z');
  });

  it('is at most 256 KiB whatever it is given, dropping the oldest lines and saying so', () => {
    const lines = Array.from({ length: 5000 }, (_, n) => `conch-desktop: ${String(n).padStart(5, '0')} ${'x'.repeat(200)}`);
    const text = buildBundle({
      versions: {}, lines, refusals: 3, refusalCounts: [['gesture', 3]],
      feedCounts: [['messages opening', 2], ['voice invalid frame', 1]], blocked: 25, now: NOW,
    });
    expect(Buffer.byteLength(text, 'utf8')).toBeLessThanOrEqual(BUNDLE_MAX_BYTES);
    expect(text).toContain('older log lines left out');
    expect(text).toContain('04999');
    expect(text).not.toContain('conch-desktop: 00000');
    expect(text).toContain('IPC refusals since start: 3 (gesture 3)\nFeed events since start: 3 (messages opening 2, voice invalid frame 1)\nBlocked requests since start: 25\n');
  });

  it('is under the cap for a full ring of full-length lines', () => {
    const ring = new LogRing();
    for (let n = 0; n < RING_LINES * 2; n += 1) ring.record(`conch-desktop: ${'a b '.repeat(200)}`);
    const text = buildBundle({ versions: { app: '1.0.0' }, lines: ring.lines(), refusals: 0, now: NOW });
    expect(Buffer.byteLength(text, 'utf8')).toBeLessThanOrEqual(BUNDLE_MAX_BYTES);
    expect(text).not.toContain('left out');
  });
});

function fixture(chosen: string | null = '/tmp/out/diag.txt') {
  const ring = new LogRing();
  const written: [string, string][] = [];
  const logs: string[] = [];
  const log = diagnosticsLogger(ring, (line) => logs.push(line));
  const asked: string[] = [];
  const state = { chosen, fail: false };
  const samples = new DiagnosticSamples();
  const diagnostics = new Diagnostics({
    ring,
    samples,
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
    log,
  });
  return { ring, written, logs, log, asked, state, samples, diagnostics };
}

// Simulate evict.mjs with an in-memory session and clock.
function exerciseBlocked({ log, logs, samples }: ReturnType<typeof fixture>, requests: readonly number[], wrap: boolean): readonly string[] {
  type Listener = (details: { url: string }, callback: (result: { cancel?: boolean }) => void) => void;
  const webRequest = {
    listener: undefined as Listener | undefined,
    onBeforeRequest(listener: Listener): void {
      this.listener = listener;
    },
  };
  const noop = (): void => undefined;
  const ses = {
    setPermissionRequestHandler: noop,
    setPermissionCheckHandler: noop,
    setDevicePermissionHandler: noop,
    setDisplayMediaRequestHandler: noop,
    on: noop,
    setSpellCheckerEnabled: noop,
    webRequest,
  };
  const wrapped: MainLog = (line, options) => { log(line, options); };
  let cancelled = 0;
  let allowed = 0;
  for (const count of requests) {
    guardSession(ses as unknown as Session, { log: wrap ? wrapped : log, blockedSamples: samples.blocked });
    webRequest.listener?.({ url: 'conch-app://ui/' }, (result) => {
      if (result.cancel !== true) allowed++;
    });
    for (let n = 0; n < count; n++) {
      webRequest.listener?.({ url: url('foreign.invalid/private') }, (result) => {
        if (result.cancel === true) cancelled++;
      });
    }
  }
  const total = requests.reduce((sum, count) => sum + count, 0);
  expect(cancelled).toBe(total);
  expect(allowed).toBe(requests.length);
  expect(logs.filter((line) => line === `conch-desktop: blocked a request to ${url('foreign.invalid')}`)).toHaveLength(total);
  const sampled: string[] = [];
  for (let n = 1; n <= total; n *= 2) sampled.push(`conch-desktop: blocked a request; ${String(n)} so far`);
  return sampled;
}

const evictionProbes = [
  {
    name: 'blocked requests across guard installations',
    exercise: (f: ReturnType<typeof fixture>) => exerciseBlocked(f, [512, 513], false),
    blocked: 1025,
    rendererHeader: '0',
  },
  {
    name: '1000 blocked requests with a wrapped logger',
    exercise: (f: ReturnType<typeof fixture>) => exerciseBlocked(f, [1000], true),
    blocked: 1000,
    rendererHeader: '0',
  },
  {
    name: 'renderer diagnostics at 42 reports per minute',
    exercise: ({ log, logs }: ReturnType<typeof fixture>, samples: GeometricSampler<VoiceDiagnostic>): readonly string[] => {
      let now = 0;
      const report = voiceDiagnosticLog(log, () => now, samples);
      for (let n = 0; n < 128; n++, now += 10_000) for (const code of rendererCodes) report(code);
      expect(logs).toHaveLength(128 * rendererCodes.length);
      const sampled: string[] = [];
      for (const n of [1, 2, 4, 8, 16, 32, 64, 128]) sampled.push(...logs.slice((n - 1) * rendererCodes.length, n * rendererCodes.length));
      return sampled;
    },
    blocked: 0,
    rendererHeader: '896 (capture-stalled 128, encoder-cpu 128, sdk-error 128, sdk-warning 128, sdp-bundle-collision 128, sdp-rejected 128, sdp-retry 128)',
  },
];

describe('Diagnostics', () => {
  it.each(evictionProbes)('keeps export history and shows totals through $name', ({ exercise, blocked, rendererHeader }) => {
    const f = fixture();
    const history = [
      'voice: helper 1 lost: ack-timeout',
      'conch-desktop: refused an IPC call (gesture); 1 so far',
      'conch-desktop: refused an IPC call (shape); 2 so far',
    ];
    for (const line of history) f.ring.record(line);
    const first = f.diagnostics.preview(2, [['gesture', 1], ['shape', 1]]);
    const renderer = new GeometricSampler<VoiceDiagnostic>();
    const sampled = exercise(f, renderer);
    const next = f.diagnostics.preview(2, [['gesture', 1], ['shape', 1]], renderer.counts());
    expect(f.ring.lines()).toEqual([...history, ...sampled]);
    expect(next.text).toContain('IPC refusals since start: 2 (gesture 1, shape 1)\n');
    expect(next.text).toContain(`Blocked requests since start: ${String(blocked)}\n`);
    expect(next.text).toContain(`Renderer diagnostics since start: ${rendererHeader}\n`);
    expect(next.text).toContain(history[0]);
    expect(first.text).toContain('Blocked requests since start: 0\n');
  });

  it('writes the preview selected by ID even after a newer preview', async () => {
    const f = fixture();
    const { feeds } = f.samples;
    feeds.sample('messages opening');
    f.ring.record('conch-desktop: before');
    const shown = f.diagnostics.preview(12);
    feeds.sample('messages opening');
    feeds.sample('occupancy invalid frame');
    f.ring.record('conch-desktop: after the preview');
    const newer = f.diagnostics.preview(34);
    expect(shown.text).toContain('IPC refusals since start: 12');
    expect(shown.text).toContain('Feed events since start: 1 (messages opening 1)\n');
    expect(newer.text).toContain('IPC refusals since start: 34');
    expect(newer.text).toContain('Feed events since start: 3 (messages opening 2, occupancy invalid frame 1)\n');
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
    const first = f.diagnostics.preview(0);
    const retained = Array.from({ length: 4 }, () => f.diagnostics.preview(0));
    expect(await f.diagnostics.save(first.id)).toBe('failed');
    expect(f.asked).toEqual([]);
    for (const { id } of retained) expect(await f.diagnostics.save(id)).toBe('saved');
  });

  it('suggests a plain name in the downloads directory', async () => {
    const f = fixture();
    const { id } = f.diagnostics.preview(0);
    await f.diagnostics.save(id);
    expect(f.asked).toEqual(['/home/p/Downloads/conch-diagnostics-2026-11-20.txt']);
  });

  it('writes nothing when the dialog is closed, or gives no usable path, or the write fails', async () => {
    const f = fixture(null);
    const { id } = f.diagnostics.preview(0);
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
