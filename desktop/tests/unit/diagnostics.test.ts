// The diagnostics bundle (issue #2084): the ring's redaction over hostile
// lines, its bound, the bundle's size cap, and the shown-before-saved rule.
import { describe, expect, it } from 'vitest';

import { BUNDLE_MAX_BYTES, buildBundle, Diagnostics, diagnosticsLogger, LINE_MAX_CHARS, LogRing, RING_LINES, scrubLine } from '../../main/diagnostics.ts';

const NOW = new Date('2026-11-20T10:00:00Z');

// Written in two parts: this file's source holds no absolute URL.
const url = (rest: string): string => ['https:', rest].join('//');

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

describe('scrubLine', () => {
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
    log('voice: helper 1 reconnecting: connect_failed');
    log('voice: helper 1 stderr: private device words', { ring: false });
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
