import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { expect, test } from 'vitest';

const token = 'synthetic-voice-token';
const nonce = 'synthetic-voice-attach-nonce';
const guard = new URL('../../main/child-process-env.ts', import.meta.url).href;

function probe(debug: string, guarded: boolean) {
  const coverageDir = mkdtempSync(join(tmpdir(), 'conch-helper-coverage-'));
  try {
    return spawnSync(process.execPath, ['--input-type=module', '-e', `
      import { spawnSync } from 'node:child_process';

      if (${String(guarded)}) {
        const { requireSafeChildProcessEnv } = await import(${JSON.stringify(guard)});
        requireSafeChildProcessEnv();
      }
      const child = spawnSync(process.execPath, ['-e', ${JSON.stringify(`
        process.stdout.write(JSON.stringify({
          coverage: Object.hasOwn(process.env, 'NODE_V8_COVERAGE'),
          debug: Object.hasOwn(process.env, 'NODE_DEBUG'),
          token: process.env.CONCH_TOKEN === ${JSON.stringify(token)},
          nonce: process.env.CONCH_VOICE_ATTACH === ${JSON.stringify(nonce)},
        }));
      `)}], {
        env: { CONCH_TOKEN: ${JSON.stringify(token)}, CONCH_VOICE_ATTACH: ${JSON.stringify(nonce)} },
        encoding: 'utf8',
      });
      if (child.status !== 0) process.exit(1);
      process.stdout.write(child.stdout);
    `], {
      env: { ...process.env, NODE_DEBUG: debug, NODE_V8_COVERAGE: coverageDir },
      encoding: 'utf8',
      timeout: 10_000,
    });
  } finally {
    rmSync(coverageDir, { recursive: true, force: true });
  }
}

test('Node forwards coverage and logs secret spawn options without the startup guard', () => {
  const result = probe('child_process', false);
  expect(result.status).toBe(0);
  expect(JSON.parse(result.stdout)).toEqual({ coverage: true, debug: false, token: true, nonce: true });
  expect(result.stderr.includes(token)).toBe(true);
  expect(result.stderr.includes(nonce)).toBe(true);
});

test('the startup guard keeps coverage out of a real child with a minimal env', () => {
  const result = probe('fs', true);
  expect(result.status).toBe(0);
  expect(JSON.parse(result.stdout)).toEqual({ coverage: false, debug: false, token: true, nonce: true });
  expect(result.stderr.includes(token)).toBe(false);
  expect(result.stderr.includes(nonce)).toBe(false);
});

test.each(['child_process', 'CHILD_*', '*'])('the startup guard rejects NODE_DEBUG=%s before the spawn', (debug) => {
  const result = probe(debug, true);
  expect(result.status).toBe(1);
  expect(result.stdout).toBe('');
  expect(result.stderr).toContain('Remove NODE_DEBUG from the launch environment');
  expect(result.stderr.includes(token)).toBe(false);
  expect(result.stderr.includes(nonce)).toBe(false);
});
