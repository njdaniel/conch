// The helper the build copies passes findHelper unchanged (issue #1377): the
// real scripts/desktop-voice-helper.sh installs a stand-in conch-voice into a
// resources directory, and main's own trusted-path checks accept the result.
// No check is relaxed for it: the same findHelper that refuses a link or a
// group-writable file runs here.
import { execFileSync } from 'node:child_process';
import { chmodSync, lstatSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { HELPER_NAME, HelperError, findHelper } from '../../main/voice/spawn.ts';
import { scratch, type Scratch } from './voice-support.ts';

const SCRIPT = resolve(import.meta.dirname, '../../../scripts/desktop-voice-helper.sh');

let s: Scratch;
beforeEach(() => {
  s = scratch();
});
afterEach(() => {
  s.cleanup();
});

function bundle(env: Record<string, string>): string {
  return execFileSync(SCRIPT, [s.resources], { env: { PATH: process.env['PATH'] ?? '', ...env }, encoding: 'utf8' });
}

describe('the helper copied into the build', () => {
  it('is found by findHelper, mode 0755', async () => {
    const stand = join(s.root, 'stand-in');
    writeFileSync(stand, '#!/bin/sh\necho "conch-voice 0.0.0"\n');
    chmodSync(stand, 0o755);
    const out = bundle({ CONCH_VOICE_BIN: stand });
    expect(out).toContain('bundled');
    expect(lstatSync(join(s.resources, HELPER_NAME)).mode & 0o777).toBe(0o755);
    expect(await findHelper(s.resources)).toBe(join(s.resources, HELPER_NAME));
  });

  it('is not installed when the build had none', async () => {
    const out = bundle({ DESKTOP_VOICE_ROOT: s.root });
    expect(out).toContain('built without the voice helper');
    await expect(findHelper(s.resources)).rejects.toSatisfy((e: unknown) => e instanceof HelperError && e.reason === 'not-installed');
  });
});
