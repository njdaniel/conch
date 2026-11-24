import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { expect, test } from 'vitest';

const token = 'synthetic-voice-token';
const nonce = 'synthetic-voice-attach-nonce';
const guard = new URL('../../main/child-process-env.ts', import.meta.url).href;

function probe(debug: string, guarded: boolean, packaged = false) {
  const coverageDir = mkdtempSync(join(tmpdir(), 'conch-helper-coverage-'));
  try {
    return spawnSync(process.execPath, ['--input-type=module', '-e', `
      import { spawnSync } from 'node:child_process';

      if (${String(guarded)}) {
        const { requireSafeChildProcessEnv } = await import(${JSON.stringify(guard)});
        requireSafeChildProcessEnv(${String(packaged)});
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

// The stream category (#2155): Node prints a preview of each chunk main
// reads, so a helper's output or a conchd answer would reach the log.
function streamProbe(debug: string, guarded: boolean, packaged = false) {
  return spawnSync(process.execPath, ['--input-type=module', '-e', `
    import { spawn } from 'node:child_process';

    if (${String(guarded)}) {
      const { requireSafeChildProcessEnv } = await import(${JSON.stringify(guard)});
      requireSafeChildProcessEnv(${String(packaged)});
    }
    const child = spawn(process.execPath, ['-e', 'process.stdout.write(process.argv[1])', ${JSON.stringify(token)}], { env: {} });
    child.stdout.on('data', () => undefined);
    child.on('close', (code) => { process.exit(code ?? 1); });
  `], {
    env: { ...process.env, NODE_DEBUG: debug },
    encoding: 'utf8',
    timeout: 10_000,
  });
}

test('Node previews what main reads under NODE_DEBUG=stream without the startup guard', () => {
  const result = streamProbe('stream', false);
  expect(result.status).toBe(0);
  expect(result.stderr).toMatch(/STREAM \d+:/);
});

test.each(['stream', 'net,stream', 'STREAM*'])('a development run rejects NODE_DEBUG=%s before anything is read', (debug) => {
  const result = streamProbe(debug, true);
  expect(result.status).toBe(1);
  expect(result.stderr).toContain('Remove NODE_DEBUG from the launch environment');
  expect(result.stderr).not.toMatch(/STREAM \d+:/);
});

test('a development run still allows a category that exposes nothing', () => {
  const result = streamProbe('fs', true);
  expect(result.status).toBe(0);
  expect(result.stderr).not.toMatch(/STREAM \d+:/);
});

// A packaged build has no use for Node debug output: any NODE_DEBUG is
// refused, with a line that names the variable and never echoes its value.
test.each(['fs', 'stream', 'http', '*', 'http,conchsecretmarker'])('a packaged build rejects NODE_DEBUG=%s', (debug) => {
  const result = probe(debug, true, true);
  expect(result.status).toBe(1);
  expect(result.stdout).toBe('');
  expect(result.stderr).toContain('The installed app does not run with Node debug output');
  expect(result.stderr.includes('conchsecretmarker')).toBe(false);
  expect(result.stderr.includes(token)).toBe(false);
});

test('a packaged build starts normally with no NODE_DEBUG', () => {
  const result = probe('', true, true);
  expect(result.status).toBe(0);
  expect(JSON.parse(result.stdout)).toEqual({ coverage: false, debug: false, token: true, nonce: true });
});
