// Source-level rules for secrets (#944): grep tests over desktop/main.
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';

import { describe, expect, it } from 'vitest';

const MAIN = join(import.meta.dirname, '..', '..', 'main');

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) {
      out.push(...sources(p));
    } else if (e.name.endsWith('.ts')) {
      out.push(p);
    }
  }
  return out;
}

const files = sources(MAIN).map((p) => ({
  rel: relative(MAIN, p),
  text: readFileSync(p, 'utf8'),
}));

describe('secret handling in desktop/main', () => {
  it('finds the vault sources (the grep is not vacuous)', () => {
    const rels = files.map((f) => f.rel);
    expect(rels).toEqual(expect.arrayContaining(['vault/vault.ts', 'vault/classify.ts', 'vault/files.ts']));
  });

  it('never mentions plain-text mode for safeStorage', () => {
    const hits = files.filter((f) => /setUsePlainTextEncryption/.test(f.text)).map((f) => f.rel);
    expect(hits).toEqual([]);
  });

  it('never uses the synchronous safeStorage API (removed in Electron 46)', () => {
    const hits = files
      .filter((f) => /\b(encryptString|decryptString|isEncryptionAvailable)\s*\(/.test(f.text))
      .map((f) => f.rel);
    expect(hits).toEqual([]);
  });

  it('only the vault names safeStorage, and only files.ts writes to disk', () => {
    const names = files.filter((f) => /safeStorage/.test(f.text.replace(/^\s*\/\/.*$/gm, ''))).map((f) => f.rel);
    expect(names.every((r) => r.startsWith('vault/'))).toBe(true);
    const writers = files
      .filter((f) =>
        /\b(writeFile|writeFileSync|appendFile|appendFileSync|createWriteStream|copyFile|rename|openSync)\b|\bopen\(/.test(
          f.text.replace(/^\s*\/\/.*$/gm, ''),
        ),
      )
      .map((f) => f.rel);
    expect(writers).toEqual(['vault/files.ts']);
  });

  it('the vault modules import no logger, console or IPC', () => {
    for (const f of files.filter((x) => x.rel.startsWith('vault/'))) {
      expect(f.text, f.rel).not.toMatch(/console\.|ipcMain|contextBridge|from 'electron'/);
    }
  });
});
