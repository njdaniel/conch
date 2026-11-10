// The tray host probe (issue #1589): the gdbus command line and its reading,
// against a fake runner. No bus, no gdbus.
import { describe, expect, it } from 'vitest';

import { DETECT_TIMEOUT_MS, GDBUS, parseHasOwner, watcherPresent, WATCHER_ARGV, WATCHER_NAME } from '../../main/tray-detect.ts';
import { runFile, type RunFile, type RunResult } from '../../main/voice/spawn.ts';

function fake(result: RunResult): { run: RunFile; calls: [string, readonly string[], number][] } {
  const calls: [string, readonly string[], number][] = [];
  return {
    calls,
    run: (file, args, timeoutMs) => {
      calls.push([file, args, timeoutMs]);
      return Promise.resolve(result);
    },
  };
}

describe('parseHasOwner', () => {
  it.each([
    ['(true,)\n', true],
    ['(false,)\n', false],
    ['  (true,)  ', true],
    ['', null],
    ['true', null],
    ['(true, false)', null],
    ['Error: GDBus.Error', null],
  ] as const)('%j -> %s', (text, want) => {
    expect(parseHasOwner(text)).toBe(want);
  });
});

describe('watcherPresent', () => {
  it('asks the session bus by absolute path with argv alone', async () => {
    const f = fake({ ok: true, stdout: '(true,)\n' });
    expect(await watcherPresent(f.run)).toBe(true);
    expect(f.calls).toEqual([[GDBUS, WATCHER_ARGV, DETECT_TIMEOUT_MS]]);
    expect(GDBUS.startsWith('/')).toBe(true);
    expect(WATCHER_ARGV).toContain('--session');
    expect(WATCHER_ARGV.at(-1)).toBe(WATCHER_NAME);
  });

  it.each([
    ['no owner', { ok: true, stdout: '(false,)\n' }],
    ['a failed run', { ok: false, stdout: '' }],
    ['a failed run that printed true', { ok: false, stdout: '(true,)' }],
    ['an answer it does not know', { ok: true, stdout: 'yes' }],
  ] as const)('is false for %s', async (_name, result) => {
    expect(await watcherPresent(fake(result).run)).toBe(false);
  });
});

describe('runFile', () => {
  it('runs an absolute file and reads its output', async () => {
    const r = await runFile('/bin/sh', ['-c', 'printf hi'], 5_000);
    expect(r).toEqual({ ok: true, stdout: 'hi' });
  });
  it('is not ok for a failing, missing or relative file, and never rejects', async () => {
    expect((await runFile('/bin/sh', ['-c', 'exit 3'], 5_000)).ok).toBe(false);
    expect((await runFile('/nonexistent/gdbus', [], 5_000)).ok).toBe(false);
    expect((await runFile('gdbus', [], 5_000)).ok).toBe(false);
  });
  it('kills a run past its timeout', async () => {
    const r = await runFile('/bin/sleep', ['30'], 100);
    expect(r.ok).toBe(false);
  });
});
