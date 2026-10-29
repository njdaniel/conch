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

function stripComments(t: string): string {
  return t.replace(/^\s*\/\/.*$/gm, '');
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
      .filter((f) => /\b(encryptString|decryptString|isEncryptionAvailable)\b/.test(f.text))
      .map((f) => f.rel);
    expect(hits).toEqual([]);
  });

  it('only the vault names safeStorage', () => {
    const names = files.filter((f) => /safeStorage/.test(stripComments(f.text))).map((f) => f.rel);
    expect(names.every((r) => r.startsWith('vault/'))).toBe(true);
  });

  it('only vault/files.ts may import write-capable node:fs names; the rest are read-only', () => {
    const readOnly = new Set(['readFile', 'readFileSync', 'readdir', 'readdirSync', 'stat', 'lstat', 'access', 'existsSync']);
    const offenders: string[] = [];
    for (const f of files) {
      const text = stripComments(f.text);
      // Any import of node:fs, node:fs/promises, fs or fs/promises, and a namespace import.
      for (const m of text.matchAll(/import\s+([^;]*?)\s+from\s+'(?:node:)?fs(?:\/promises)?'/gs)) {
        const clause = m[1] ?? '';
        const named = /\{([^}]*)\}/.exec(clause)?.[1];
        if (named === undefined) {
          if (f.rel !== 'vault/files.ts') offenders.push(`${f.rel}: namespace or default import`);
          continue;
        }
        for (const raw of named.split(',')) {
          const name = raw.trim().split(/\s+as\s+/)[0] ?? '';
          if (name !== '' && !readOnly.has(name) && f.rel !== 'vault/files.ts') {
            offenders.push(`${f.rel}: ${name}`);
          }
        }
      }
      if (/\b(?:import|require)\(\s*['"`](?:node:)?fs/.test(text) && f.rel !== 'vault/files.ts') {
        offenders.push(`${f.rel}: dynamic import`);
      }
      if (/\brequire\(\s*'(?:node:)?fs/.test(text) && f.rel !== 'vault/files.ts') {
        offenders.push(`${f.rel}: require`);
      }
    }
    expect(offenders).toEqual([]);
    const filesTs = files.find((f) => f.rel === 'vault/files.ts');
    expect(filesTs && /\bopen\b/.test(filesTs.text)).toBe(true); // not vacuous
  });

  it('the vault modules import no logger, console or IPC', () => {
    for (const f of files.filter((x) => x.rel.startsWith('vault/'))) {
      expect(f.text, f.rel).not.toMatch(/console\.|ipcMain|contextBridge|from 'electron'/);
    }
  });
});
