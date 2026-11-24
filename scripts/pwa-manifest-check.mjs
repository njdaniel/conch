import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

// usage: pwa-manifest-check.mjs [dist directory]   (default: web/dist)
const dist = process.argv[2]
  ? pathToFileURL(resolve(process.argv[2]) + '/')
  : new URL('../web/dist/', import.meta.url);
const html = readFileSync(new URL('index.html', dist), 'utf8');
assert.match(html, /<link rel="manifest" href="\/app\/conch-manifest\.json"\s*\/?\s*>/);

const manifest = JSON.parse(readFileSync(new URL('conch-manifest.json', dist), 'utf8'));
assert.equal(manifest.id, '/app/');
assert.equal(manifest.scope, '/app/');
assert.equal(manifest.start_url, '/app/');
assert.equal(manifest.name, 'Conch');
assert.equal(manifest.short_name, 'Conch');
assert.equal(manifest.display, 'standalone');
const colour = /^#[0-9a-f]{6}$/i;
assert.match(manifest.theme_color, colour, 'theme_color');
assert.match(manifest.background_color, colour, 'background_color');

const signatures = new Set();
const iconOrigin = new URL('https://conch.example/app/');
for (const icon of manifest.icons) {
  const url = new URL(icon.src, iconOrigin);
  assert.equal(url.origin, iconOrigin.origin);
  assert.ok(url.pathname.startsWith(iconOrigin.pathname));
  assert.equal(icon.type, 'image/png');
  const png = readFileSync(new URL(url.pathname.slice(iconOrigin.pathname.length), dist));
  assert.equal(png.subarray(0, 8).toString('hex'), '89504e470d0a1a0a', icon.src);
  assert.equal(png.subarray(12, 16).toString('ascii'), 'IHDR', icon.src);
  assert.equal(icon.sizes, `${png.readUInt32BE(16)}x${png.readUInt32BE(20)}`, icon.src);
  signatures.add(`${icon.purpose} ${icon.sizes}`);
}
for (const expected of ['any 192x192', 'any 512x512', 'maskable 512x512']) {
  assert.ok(signatures.has(expected), `Missing PWA icon: ${expected}`);
}
console.log('PWA: built page, standalone scope and PNG icons passed');
