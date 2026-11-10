// Writes the asset manifest the conch-app: scheme serves from (D2-1), and the
// server allowlist beside it (issues #946, #1596).
// Input: the web build for the desktop (out/ui), build/compat.json (the hand
// list), build/compat-extra.json (patch releases to add to a release's list)
// and the environment's CONCH_RELEASE_VERSION. Output: out/ui-manifest.json,
// `{ version: 1, files: { "/path": { sha256, type } } }` for every file under
// it, and out/compat.json. A file whose extension has no content type here
// fails the build: nothing is served with a guessed type.
//
// With CONCH_RELEASE_VERSION (the tag, v0.1.0-alpha.18) the allowlist is that
// tag plus the extra list; without it, the hand list, and the build says so.
//
// Run by node (type stripping), so no import of anything but node: modules
// and the type-only-importing main/compat.ts, which owns the list's shape.
import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

import { parseCompat } from '../main/compat.ts';

const TYPES: Readonly<Record<string, string>> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.txt': 'text/plain; charset=utf-8',
};

export interface ManifestFile {
  readonly sha256: string;
  readonly type: string;
}

function walk(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true })
    .flatMap((entry) => (entry.isDirectory() ? walk(join(dir, entry.name)) : [join(dir, entry.name)]))
    .sort();
}

function extension(file: string): string {
  const dot = file.lastIndexOf('.');
  return dot < 0 ? '' : file.slice(dot).toLowerCase();
}

export function buildManifest(root: string): { version: 1; files: Record<string, ManifestFile> } {
  const files: Record<string, ManifestFile> = {};
  for (const file of walk(root)) {
    const path = '/' + relative(root, file).split(sep).join('/');
    const type = TYPES[extension(file)];
    if (type === undefined) throw new Error(`${path} has an extension with no content type in build/manifest.ts`);
    files[path] = { sha256: createHash('sha256').update(readFileSync(file)).digest('hex'), type };
  }
  if (!('/index.html' in files)) throw new Error(`${root} has no index.html`);
  return { version: 1, files };
}

// buildCompat: the text of out/compat.json. A release tag gives the tag plus
// the extra patch releases (each once); no tag gives the hand list unchanged.
// Every list is checked by the app's own parser, so a malformed tag or entry
// fails the build here rather than shipping a list the app would refuse.
export function buildCompat(release: string | null, hand: string, extra: string): string {
  if (release === null) {
    if (parseCompat(hand) === null) throw new Error('build/compat.json is not a valid server list');
    return hand;
  }
  const more = parseCompat(extra);
  if (more === null) throw new Error('build/compat-extra.json is not a valid server list');
  const servers = [release, ...more.servers.filter((s) => s !== release)];
  const text = JSON.stringify({ version: 1, servers }, null, 2) + '\n';
  if (parseCompat(text) === null) throw new Error(`${release} is not a release tag (v<major>.<minor>.<patch>[-<prerelease>])`);
  return text;
}

if (import.meta.main) {
  const here = dirname(fileURLToPath(import.meta.url));
  const out = resolve(here, '..', 'out');
  const manifest = buildManifest(join(out, 'ui'));
  writeFileSync(join(out, 'ui-manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
  // The allowlist of server releases (issue #946) rides in the asar beside it.
  const release = process.env['CONCH_RELEASE_VERSION'] ?? '';
  const compat = buildCompat(
    release === '' ? null : release,
    readFileSync(join(here, 'compat.json'), 'utf8'),
    readFileSync(join(here, 'compat-extra.json'), 'utf8'),
  );
  writeFileSync(join(out, 'compat.json'), compat);
  console.log(`manifest: ${String(Object.keys(manifest.files).length)} files`);
  console.log(
    release === ''
      ? 'compat: the development list (build/compat.json), not generated from a release tag'
      : `compat: ${release} plus build/compat-extra.json`,
  );
}
