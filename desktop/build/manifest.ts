// Writes the asset manifest the conch-app: scheme serves from (D2-1).
// Input: the web build for the desktop (out/ui). Output: out/ui-manifest.json,
// `{ version: 1, files: { "/path": { sha256, type } } }` for every file under
// it. A file whose extension has no content type here fails the build: nothing
// is served with a guessed type.
//
// Run by node (type stripping), so no import of anything but node: modules.
import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';

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

if (import.meta.main) {
  const here = dirname(fileURLToPath(import.meta.url));
  const out = resolve(here, '..', 'out');
  const manifest = buildManifest(join(out, 'ui'));
  writeFileSync(join(out, 'ui-manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
  console.log(`manifest: ${String(Object.keys(manifest.files).length)} files`);
}
