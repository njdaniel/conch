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

  it('only the vault names safeStorage, and only vault/electron-storage.ts imports electron for it', () => {
    const names = files.filter((f) => /safeStorage/.test(stripComments(f.text))).map((f) => f.rel);
    // app.ts hands electron's safeStorage to that one adapter and calls nothing on it.
    expect(names.every((r) => r.startsWith('vault/') || r === 'app.ts')).toBe(true);
    const app = files.find((f) => f.rel === 'app.ts');
    expect(/safeStorage\./.test(stripComments(app?.text ?? ''))).toBe(false);
    const adapters = files.filter((f) => f.rel.startsWith('vault/') && /from 'electron'/.test(f.text)).map((f) => f.rel);
    expect(adapters).toEqual(['vault/electron-storage.ts']);
  });

  it('only the named writers may import write-capable node:fs names; the rest are read-only', () => {
    // vault/files.ts writes the sealed credentials; profiles/files.ts the
    // non-secret profiles.json and the profile directories; instance.ts makes
    // the data root private. Each says what it writes in its own header.
    const writers = new Set(['vault/files.ts', 'profiles/files.ts', 'instance.ts']);
    const readOnly = new Set(['readFile', 'readFileSync', 'readdir', 'readdirSync', 'stat', 'lstat', 'realpath', 'access', 'existsSync']);
    // The voice endpoint (#948) makes its private runtime directories and
    // removes stale sockets from them; it writes no file's content.
    const endpointOnly = new Set(['mkdir', 'chmod', 'unlink']);
    // The one attachment writer (#947): bytes at a path the person chose in
    // the save dialog.
    const chosenFileOnly = new Set(['open', 'constants']);
    const offenders: string[] = [];
    for (const f of files) {
      const text = stripComments(f.text);
      // Any import of node:fs, node:fs/promises, fs or fs/promises, and a namespace import.
      for (const m of text.matchAll(/import\s+([^;]*?)\s+from\s+'(?:node:)?fs(?:\/promises)?'/gs)) {
        const clause = m[1] ?? '';
        const named = /\{([^}]*)\}/.exec(clause)?.[1];
        if (named === undefined) {
          if (!writers.has(f.rel)) offenders.push(`${f.rel}: namespace or default import`);
          continue;
        }
        for (const raw of named.split(',')) {
          const name = raw.trim().split(/\s+as\s+/)[0] ?? '';
          const allowed = readOnly.has(name) || (f.rel === 'voice/endpoint.ts' && endpointOnly.has(name)) || (f.rel === 'save-file.ts' && chosenFileOnly.has(name));
          if (name !== '' && !allowed && !writers.has(f.rel)) {
            offenders.push(`${f.rel}: ${name}`);
          }
        }
      }
      if (/\b(?:import|require)\(\s*['"`](?:node:)?fs/.test(text) && !writers.has(f.rel)) {
        offenders.push(`${f.rel}: dynamic import`);
      }
      if (/\brequire\(\s*'(?:node:)?fs/.test(text) && !writers.has(f.rel)) {
        offenders.push(`${f.rel}: require`);
      }
    }
    expect(offenders).toEqual([]);
    const filesTs = files.find((f) => f.rel === 'vault/files.ts');
    expect(filesTs && /\bopen\b/.test(filesTs.text)).toBe(true); // not vacuous
  });

  it('the vault modules import no logger, console or IPC', () => {
    for (const f of files.filter((x) => x.rel.startsWith('vault/') && x.rel !== 'vault/electron-storage.ts')) {
      expect(f.text, f.rel).not.toMatch(/console\.|ipcMain|contextBridge|from 'electron'/);
    }
  });
});
