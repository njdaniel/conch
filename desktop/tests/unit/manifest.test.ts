import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { buildCompat, buildManifest } from '../../build/manifest.ts';
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

describe('buildCompat', () => {
  const hand = '{"version":1,"servers":["v0.1.0-alpha.9","v0.1.0-alpha.18"]}\n';
  const none = '{"version":1,"servers":[]}';
  const servers = (text: string): unknown => (JSON.parse(text) as { servers: unknown }).servers;

  it('without a tag keeps the hand list exactly', () => {
    expect(buildCompat(null, hand, none)).toBe(hand);
  });

  it('with a tag lists it first, then the extra releases', () => {
    const extra = '{"version":1,"servers":["v0.1.0-alpha.17.1"]}';
    expect(servers(buildCompat('v0.1.0-alpha.18', hand, extra))).toEqual(['v0.1.0-alpha.18', 'v0.1.0-alpha.17.1']);
    expect(servers(buildCompat('v0.1.0-alpha.18', hand, none))).toEqual(['v0.1.0-alpha.18']);
  });

  it('does not list the tag twice and ignores the hand list', () => {
    const extra = '{"version":1,"servers":["v0.1.0-alpha.18","v0.1.0-alpha.19"]}';
    expect(servers(buildCompat('v0.1.0-alpha.18', hand, extra))).toEqual(['v0.1.0-alpha.18', 'v0.1.0-alpha.19']);
  });

  it.each(['0.1.0', 'v1', 'v1.2.3+build', 'latest', 'v1.2.3 ', '>=v1.0.0'])('refuses the tag %s', (tag) => {
    expect(() => buildCompat(tag, hand, none)).toThrow(/not a release tag/);
  });

  it('refuses an invalid extra or hand list', () => {
    expect(() => buildCompat('v1.2.3', hand, '{"version":1,"servers":[">=v1.0.0"]}')).toThrow(/compat-extra/);
    expect(() => buildCompat(null, '{}', none)).toThrow(/compat\.json/);
  });

  it('the checked-in lists are valid', () => {
    const read = (f: string): string => readFileSync(join(import.meta.dirname, '..', '..', 'build', f), 'utf8');
    expect(() => buildCompat(null, read('compat.json'), read('compat-extra.json'))).not.toThrow();
    expect(() => buildCompat('v0.1.0-alpha.18', read('compat.json'), read('compat-extra.json'))).not.toThrow();
  });
});
