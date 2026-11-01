import { mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { isAbsolute, join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import type { Client, Exchange, Send } from '../../main/api/client.ts';
import {
  decodeBase64,
  Files,
  MAX_DOWNLOAD_BYTES,
  MAX_UPLOAD_BYTES,
  mediaTypeOf,
  OCTET_STREAM,
  parseUpload,
  plainName,
  type FilesDeps,
} from '../../main/files.ts';

const b64 = (bytes: number[]): string => Buffer.from(bytes).toString('base64');

describe('mediaTypeOf', () => {
  it.each([
    ['image/png', 'image/png'],
    ['IMAGE/PNG', 'image/png'],
    ['text/plain; charset=utf-8', 'text/plain'],
    ['application/vnd.api+json', 'application/vnd.api+json'],
    ['', OCTET_STREAM],
    ['png', OCTET_STREAM],
    ['image/png\r\nX-Evil: 1', OCTET_STREAM],
    ['image/png, text/html', OCTET_STREAM],
    ['a/b/c', OCTET_STREAM],
    ['text/html<script>', OCTET_STREAM],
    [`image/${'a'.repeat(200)}`, OCTET_STREAM],
    [7, OCTET_STREAM],
    [null, OCTET_STREAM],
  ])('reshapes %j to %s', (declared, expected) => {
    expect(mediaTypeOf(declared)).toBe(expected);
  });
});

describe('plainName', () => {
  it.each([
    ['photo.png', 'photo.png'],
    ['../../etc/passwd', 'passwd'],
    ['..\\..\\windows\\system32\\a.exe', 'a.exe'],
    ['/abs/path/file.txt', 'file.txt'],
    ['.bashrc', 'bashrc'],
    ['..', ''],
    ['name\u0000.png', 'name.png'],
    ['a\nb', 'ab'],
    ['  spaced.txt  ', 'spaced.txt'],
    ['trailing.', 'trailing'],
    ['x'.repeat(400), 'x'.repeat(255)],
    [5, ''],
  ])('makes %j into %j', (input, expected) => {
    expect(plainName(input)).toBe(expected);
  });

  it('drops direction overrides and zero-width characters', () => {
    expect(plainName(`a${String.fromCodePoint(0x202e)}b${String.fromCodePoint(0x200b)}c.txt`)).toBe('abc.txt');
  });
});

describe('decodeBase64', () => {
  it('decodes within the cap', () => {
    expect([...(decodeBase64(b64([1, 2, 3]), 10) ?? [])]).toEqual([1, 2, 3]);
    expect(decodeBase64('', 10)?.byteLength).toBe(0);
  });
  it.each(['AQI', 'AQ=', 'A*ID', 'AQID\n', '=AQI', 'AQ==AQ=='])('refuses %j', (text) => {
    expect(decodeBase64(text, 100)).toBeNull();
  });
  it('refuses more bytes than the cap, without decoding them', () => {
    expect(decodeBase64(b64([1, 2, 3, 4]), 3)).toBeNull();
    expect(decodeBase64(b64([1, 2, 3]), 3)).not.toBeNull();
    expect(decodeBase64(7, 3)).toBeNull();
  });
});

describe('the upload cap', () => {
  const request = (data: string, over: Record<string, unknown> = {}) => ({ channel: 'general', filename: 'a.png', mediaType: 'image/png', data, ...over });

  it('accepts a file at the cap and refuses one byte more', () => {
    const atCap = Buffer.alloc(MAX_UPLOAD_BYTES).toString('base64');
    const over = Buffer.alloc(MAX_UPLOAD_BYTES + 1).toString('base64');
    expect(parseUpload(request(atCap))?.bytes.byteLength).toBe(MAX_UPLOAD_BYTES);
    expect(parseUpload(request(over))).toBeNull();
  });

  it('shapes what the page declared, and refuses what is not the shape', () => {
    expect(parseUpload(request(b64([1]), { filename: '../x/evil.png', mediaType: 'text/html; x=y' }))).toMatchObject({ filename: 'evil.png', mediaType: 'text/html' });
    expect(parseUpload(request(b64([1]), { mediaType: 'nonsense' }))).toMatchObject({ mediaType: OCTET_STREAM });
    expect(parseUpload(request(b64([1]), { filename: '..' }))).toMatchObject({ filename: 'file' });
    for (const bad of [
      request(b64([1]), { extra: 1 }),
      request(b64([1]), { channel: '../x' }),
      request(b64([1]), { channel: 'a/b' }),
      request(b64([1]), { channel: 3 }),
      request(b64([1]), { filename: '' }),
      request(b64([1]), { filename: 'x'.repeat(2000) }),
      request('not base64!'),
      { channel: 'general', filename: 'a', mediaType: 'x/y' },
      null,
      [],
      'text',
    ]) {
      expect(parseUpload(bad)).toBeNull();
    }
  });
});

// A client whose answers are scripted, and which records what it was asked.
function scripted(answers: Exchange[]): { client: Pick<Client, 'send'>; sent: Send[] } {
  const sent: Send[] = [];
  return {
    sent,
    client: {
      send: (request) => {
        sent.push(request);
        return Promise.resolve(answers.shift() ?? { kind: 'none' });
      },
    },
  };
}

describe('Files.upload', () => {
  it('sends the operation itself: the channel, the plain name as the query, the shaped type, the bytes', async () => {
    const { client, sent } = scripted([{ kind: 'answer', status: 201, ok: true, body: { id: 7 } }]);
    const files = new Files({ client } as unknown as FilesDeps);
    const parsed = parseUpload({ channel: 'general', filename: 'my pic.png', mediaType: 'image/png', data: b64([9]) });
    expect(parsed).not.toBeNull();
    const reply = await files.upload(parsed as NonNullable<typeof parsed>);
    expect(reply).toEqual({ kind: 'reply', status: 201, body: { id: 7 } });
    expect(sent).toHaveLength(1);
    expect(sent[0]).toMatchObject({ op: 'uploadFile', params: { channel: 'general' }, query: 'filename=my%20pic.png', upload: { mediaType: 'image/png' } });
    expect([...(sent[0]?.upload?.bytes ?? [])]).toEqual([9]);
  });
});

describe('Files.download', () => {
  it('returns the bytes as data, capped, and never anything else', async () => {
    const { client, sent } = scripted([{ kind: 'answer', status: 200, ok: true, bytes: new Uint8Array([1, 2, 3]) }]);
    const files = new Files({ client } as unknown as FilesDeps);
    expect(await files.download(5)).toEqual({ kind: 'bytes', data: 'AQID' });
    expect(sent[0]).toMatchObject({ op: 'fileBytes', params: { id: 5 }, download: { maxBytes: MAX_DOWNLOAD_BYTES } });
  });

  it('passes on a refusal and no answer as such', async () => {
    const { client } = scripted([{ kind: 'answer', status: 404, ok: false }, { kind: 'none', reason: 'oversize' }, { kind: 'unauthorized' }]);
    const files = new Files({ client } as unknown as FilesDeps);
    expect(await files.download(5)).toEqual({ kind: 'reply', status: 404 });
    expect(await files.download(5)).toEqual({ kind: 'none', reason: 'oversize' });
    expect(await files.download(5)).toEqual({ kind: 'unauthorized' });
  });
});

describe('Files.save: the save path rule', () => {
  const dirs: string[] = [];
  afterEach(() => {
    for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
  });

  const meta = (filename: string, size = 3): Exchange => ({
    kind: 'answer',
    status: 200,
    ok: true,
    body: { schema: 'conch.file.v1', id: 5, channel_id: 1, uploader_id: 7, filename, media_type: 'image/png', size, sha256: 'a'.repeat(64), created_at: '2026-11-01T00:00:00Z' },
  });
  const bytes: Exchange = { kind: 'answer', status: 200, ok: true, bytes: new Uint8Array([1, 2, 3]) };

  // A session whose identity can be ended: switching servers or accounts.
  function identity() {
    const state = { generation: 1 };
    return {
      state,
      session: {
        capture: () => ({ generation: state.generation, origin: 'https://a.example', token: 'tok', signal: new AbortController().signal }),
        current: (c: { generation: number }) => c.generation === state.generation,
      },
    };
  }

  function setup(answers: Exchange[], choose: (suggested: string) => string | null, who = identity(), afterSend?: (n: number) => void) {
    const dir = mkdtempSync(join(tmpdir(), 'conch-save-'));
    dirs.push(dir);
    const scripts = scripted(answers);
    const sent = scripts.sent;
    const client: Pick<Client, 'send'> = {
      send: async (request) => {
        const answer = await scripts.client.send(request);
        afterSend?.(sent.length);
        return answer;
      },
    };
    const suggestions: string[] = [];
    const logs: string[] = [];
    const files = new Files({
      client,
      session: who.session,
      chooseSavePath: (suggested) => {
        suggestions.push(suggested);
        return Promise.resolve(choose(suggested));
      },
      writeFile: (path, data) => writeFile(path, data, { mode: 0o600 }),
      downloadsDir: () => join(dir, 'Downloads'),
      join,
      isAbsolute,
      log: (l) => logs.push(l),
    });
    return { files, dir, sent, suggestions, logs };
  }

  it('writes the bytes to the path the person chose, and only there, not executable', async () => {
    const target = join(tmpdir(), `conch-chosen-${String(process.pid)}.bin`);
    const g = setup([meta('photo.png'), bytes], () => target);
    dirs.push(target);
    expect(await g.files.save(5)).toBe('saved');
    expect([...readFileSync(target)]).toEqual([1, 2, 3]);
    expect(statSync(target).mode & 0o111).toBe(0);
    expect(readdirSync(g.dir)).toEqual([]);
    expect(g.suggestions).toEqual([join(g.dir, 'Downloads', 'photo.png')]);
  });

  it('writes nothing and downloads nothing when the person closes the dialog', async () => {
    const f = setup([meta('photo.png'), bytes], () => null);
    expect(await f.files.save(5)).toBe('cancelled');
    expect(f.sent.map((s) => s.op)).toEqual(['fileMeta']);
    expect(readdirSync(f.dir)).toEqual([]);
  });

  it('suggests a plain name whatever the stored name says: no directory part, no traversal', async () => {
    const f = setup([meta('../../../.ssh/authorized_keys')], () => null);
    await f.files.save(5);
    expect(f.suggestions).toEqual([join(f.dir, 'Downloads', 'authorized_keys')]);
    const g = setup([meta('..')], () => null);
    await g.files.save(5);
    expect(g.suggestions).toEqual([join(g.dir, 'Downloads', 'file-5')]);
  });

  it.each(['relative/path.png', 'x.png', '', '/tmp/a\u0000b'])('refuses a dialog answer that is not an absolute path (%j) and writes nothing', async (chosen) => {
    const f = setup([meta('a.png'), bytes], () => chosen);
    expect(await f.files.save(5)).toBe('failed');
    expect(readdirSync(f.dir)).toEqual([]);
  });

  it('makes both requests under the identity the save began with', async () => {
    const f = setup([meta('a.png'), bytes], () => join(tmpdir(), 'conch-same-identity-947.bin'));
    dirs.push(join(tmpdir(), 'conch-same-identity-947.bin'));
    expect(await f.files.save(5)).toBe('saved');
    expect(f.sent).toHaveLength(2);
    expect(f.sent[0]?.capture).toBeDefined();
    expect(f.sent[1]?.capture).toBe(f.sent[0]?.capture);
  });

  it('refuses to write, and fetches nothing, when the account or server changes while the dialog is open', async () => {
    const who = identity();
    const target = join(tmpdir(), 'conch-switched-947.bin');
    dirs.push(target);
    const f = setup([meta('a.png'), bytes], () => {
      who.state.generation++;
      return target;
    }, who);
    expect(await f.files.save(5)).toBe('failed');
    expect(f.sent.map((s) => s.op)).toEqual(['fileMeta']);
    expect(() => statSync(target)).toThrow();
  });

  it('does not show the dialog when the identity changed while the metadata was coming', async () => {
    const who = identity();
    const f = setup([meta('a.png'), bytes], () => join(tmpdir(), 'never-947'), who, (n) => {
      if (n === 1) who.state.generation++;
    });
    expect(await f.files.save(5)).toBe('failed');
    expect(f.suggestions).toEqual([]);
  });

  it('does not write bytes that arrived for an identity that has ended', async () => {
    const who = identity();
    const target = join(tmpdir(), 'conch-ended-947.bin');
    dirs.push(target);
    const f = setup([meta('a.png'), bytes], () => target, who, (n) => {
      if (n === 2) who.state.generation++;
    });
    expect(await f.files.save(5)).toBe('failed');
    expect(() => statSync(target)).toThrow();
  });

  it('takes no path from the caller: the only argument is the file number', () => {
    expect(Files.prototype.save.length).toBe(1);
  });

  it('fails, without asking where, for a file that is too large, unreadable, or not the metadata promised', async () => {
    const big = setup([meta('a.png', MAX_DOWNLOAD_BYTES + 1)], () => join(tmpdir(), 'never'));
    expect(await big.files.save(5)).toBe('failed');
    expect(big.suggestions).toEqual([]);
    const gone = setup([{ kind: 'answer', status: 404, ok: false }], () => join(tmpdir(), 'never'));
    expect(await gone.files.save(5)).toBe('failed');
    const odd = setup([{ kind: 'answer', status: 200, ok: true, body: { nope: 1 } }], () => join(tmpdir(), 'never'));
    expect(await odd.files.save(5)).toBe('failed');
    expect(odd.suggestions).toEqual([]);
  });

  it('says unauthorized for a sign-in that ended, and failed when the bytes did not come', async () => {
    const out = setup([{ kind: 'unauthorized' }], () => null);
    expect(await out.files.save(5)).toBe('unauthorized');
    const late = setup([meta('a.png'), { kind: 'unauthorized' }], () => join(tmpdir(), 'conch-never-written'));
    expect(await late.files.save(5)).toBe('unauthorized');
    const none = setup([meta('a.png'), { kind: 'none' }], () => join(tmpdir(), 'conch-never-written'));
    expect(await none.files.save(5)).toBe('failed');
  });

  it('logs a write failure without the path', async () => {
    const f = setup([meta('a.png'), bytes], () => join(tmpdir(), 'conch-no-such-dir-947', 'sub', 'x.bin'));
    expect(await f.files.save(5)).toBe('failed');
    expect(f.logs).toEqual(['conch-desktop: a file could not be saved']);
  });
});
