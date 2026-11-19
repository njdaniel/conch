// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
/// <reference types="node" />
// The wire types are a verbatim copy of web/src/schema/ (CLAUDE.md rule 2,
// #1109). This is the byte-identity check; it fails on a stale or edited copy.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const repo = resolve(__dirname, '../../../../../..');
const copies = ['generated.ts', 'validate.ts'];

for (const name of copies) {
  test(`schema/${name} is byte-identical to web/src/schema/${name}`, () => {
    const ours = readFileSync(resolve(repo, 'spikes/mobile/ios/src/logic/schema', name));
    const theirs = readFileSync(resolve(repo, 'web/src/schema', name));
    assert.equal(ours.equals(theirs), true, `${name} differs from web/src/schema/${name}: copy it again, never edit it`);
  });
}
