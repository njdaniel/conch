import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { buildManifest } from '../../build/manifest.ts';
import { parseManifest } from '../../main/scheme.ts';

const dirs: string[] = [];
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

function tree(files: Record<string, string>): string {
  const root = mkdtempSync(join(tmpdir(), 'manifest-'));
  dirs.push(root);
  for (const [path, body] of Object.entries(files)) {
    const full = join(root, path);
    mkdirSync(join(full, '..'), { recursive: true });
    writeFileSync(full, body);
  }
  return root;
}

describe('buildManifest', () => {
  it('lists every file with its hash and a content type, in a form the scheme accepts', () => {
    const root = tree({ 'index.html': '<html>', 'assets/a-1.js': 'x', 'assets/a-1.css': 'y', 'favicon-16.png': 'p' });
    const m = buildManifest(root);
    expect(Object.keys(m.files)).toEqual(['/assets/a-1.css', '/assets/a-1.js', '/favicon-16.png', '/index.html']);
    expect(m.files['/assets/a-1.js']).toEqual({ sha256: createHash('sha256').update('x').digest('hex'), type: 'text/javascript; charset=utf-8' });
    expect(parseManifest(JSON.stringify(m)).size).toBe(4);
  });

  it('fails on an extension with no content type', () => {
    expect(() => buildManifest(tree({ 'index.html': 'a', 'x.exe': 'b' }))).toThrow(/no content type/);
  });

  it('fails without an index.html', () => {
    expect(() => buildManifest(tree({ 'a.js': 'b' }))).toThrow(/index\.html/);
  });
});
