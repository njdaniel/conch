// Checks the installed app's metadata against the tag and the checked-in
// release extras. The generated out/compat.json could contain the development
// list, so compute the expectation using the same release path as the .deb.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { extractFile } from '@electron/asar';

import { buildCompat } from './manifest.ts';

const asar = process.argv[2];
const tag = process.env['CONCH_RELEASE_VERSION'];
const appVersion = process.env['APP_VERSION'];
assert(asar && tag && appVersion, 'expected an app.asar path, CONCH_RELEASE_VERSION and APP_VERSION');

const pkg: unknown = JSON.parse(extractFile(asar, 'package.json').toString());
assert(typeof pkg === 'object' && pkg !== null && 'version' in pkg, 'packaged package.json must have a version');
assert.equal(pkg.version, appVersion, 'packaged app version must match APP_VERSION');

const extras = readFileSync(new URL('compat-extra.json', import.meta.url), 'utf8');
const expected = buildCompat(tag, '', extras);
const packaged = extractFile(asar, join('out', 'compat.json')).toString();
assert.deepEqual(JSON.parse(packaged), JSON.parse(expected), 'packaged compat list must match the release tag and compat-extra.json');
