import { chmodSync, mkdtempSync, readFileSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { saveChosenFile, SAVED_MODE, type Handle } from '../../main/save-file.ts';

const dirs: string[] = [];
afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

function dir(): string {
  const d = mkdtempSync(join(tmpdir(), 'conch-save-file-'));
  dirs.push(d);
  return d;
}

const mode = (path: string): number => statSync(path).mode & 0o777;

describe('the real writer of a saved attachment', () => {
  it('makes a new file 0600', async () => {
    const path = join(dir(), 'new.bin');
    await saveChosenFile(path, new Uint8Array([1, 2, 3]));
    expect([...readFileSync(path)]).toEqual([1, 2, 3]);
    expect(mode(path)).toBe(SAVED_MODE);
  });

  it.each([0o644, 0o755, 0o666, 0o777])('makes an existing %o file 0600, and replaces all of its content', async (before) => {
    const path = join(dir(), 'old.bin');
    writeFileSync(path, 'a much longer old content');
    chmodSync(path, before);
    await saveChosenFile(path, new Uint8Array([9]));
    expect(mode(path)).toBe(SAVED_MODE);
    expect([...readFileSync(path)]).toEqual([9]);
  });

  it('never leaves an executable bit, whatever the file was', async () => {
    const path = join(dir(), 'tool.sh');
    writeFileSync(path, '#!/bin/sh\n');
    chmodSync(path, 0o755);
    await saveChosenFile(path, new Uint8Array([1]));
    expect(mode(path) & 0o111).toBe(0);
  });

  it('does not follow a symlink at the chosen path to write somewhere else', async () => {
    const d = dir();
    const target = join(d, 'target.txt');
    writeFileSync(target, 'precious');
    const link = join(d, 'link.bin');
    symlinkSync(target, link);
    await expect(saveChosenFile(link, new Uint8Array([1, 2, 3]))).rejects.toThrow();
    expect(readFileSync(target, 'utf8')).toBe('precious');
  });

  it('does not follow a dangling symlink to create a file where it points', async () => {
    const d = dir();
    const link = join(d, 'dangling.bin');
    symlinkSync(join(d, 'created-through-link.txt'), link);
    await expect(saveChosenFile(link, new Uint8Array([1]))).rejects.toThrow();
    expect(() => statSync(join(d, 'created-through-link.txt'))).toThrow();
  });

  it('fails, writing nothing, when the directory is missing', async () => {
    await expect(saveChosenFile(join(dir(), 'no', 'such', 'dir', 'x.bin'), new Uint8Array([1]))).rejects.toThrow();
  });

  // A handle whose writes take at most `most` bytes a call, into `out`.
  function slow(most: number, out: number[], closed: { n: number }): Handle {
    return {
      chmod: () => Promise.resolve(),
      truncate: () => Promise.resolve(),
      write: (bytes, offset, length, position) => {
        const n = Math.min(most, length);
        for (let i = 0; i < n; i++) out[position + i] = bytes[offset + i] ?? -1;
        return Promise.resolve({ bytesWritten: n });
      },
      close: () => {
        closed.n++;
        return Promise.resolve();
      },
    };
  }

  it('goes on after a partial write until every byte is written', async () => {
    const out: number[] = [];
    const closed = { n: 0 };
    await saveChosenFile('/unused', new Uint8Array([7, 8, 9]), () => Promise.resolve(slow(1, out, closed)));
    expect(out).toEqual([7, 8, 9]);
    expect(closed.n).toBe(1);
  });

  it('rejects, and closes the file, when a write makes no progress (not "saved")', async () => {
    const out: number[] = [];
    const closed = { n: 0 };
    await expect(saveChosenFile('/unused', new Uint8Array([7, 8, 9]), () => Promise.resolve(slow(0, out, closed)))).rejects.toThrow('short write');
    expect(closed.n).toBe(1);
  });

  it('rejects when the disk fills part of the way', async () => {
    let calls = 0;
    const handle: Handle = {
      ...slow(1, [], { n: 0 }),
      write: () => (++calls < 3 ? Promise.resolve({ bytesWritten: 1 }) : Promise.reject(new Error('ENOSPC'))),
    };
    await expect(saveChosenFile('/unused', new Uint8Array([1, 2, 3, 4]), () => Promise.resolve(handle))).rejects.toThrow('ENOSPC');
  });
});
