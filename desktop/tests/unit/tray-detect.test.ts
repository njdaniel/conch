// The tray host probe (issue #1589): the gdbus command line and its reading,
// against a fake runner. No bus, no gdbus.
import { describe, expect, it } from 'vitest';

import { DETECT_TIMEOUT_MS, GDBUS, HOST_ARGV, parseHasOwner, parseHostRegistered, trayHostPresent, watcherPresent, WATCHER_ARGV, WATCHER_NAME } from '../../main/tray-detect.ts';
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

describe('parseHostRegistered', () => {
  it.each([
    ['(<true>,)\n', true],
    ['(<false>,)\n', false],
    ['  (<true>,)  ', true],
    ['(true,)', null],
    ['(<true>, <false>)', null],
    ['', null],
    ['Error: GDBus.Error:org.freedesktop.DBus.Error.ServiceUnknown', null],
  ] as const)('%j -> %s', (text, want) => {
    expect(parseHostRegistered(text)).toBe(want);
  });
});

describe('trayHostPresent', () => {
  // A runner that answers by which command it was given.
  function bus(watcher: RunResult, host: RunResult): { run: RunFile; calls: (readonly string[])[] } {
    const calls: (readonly string[])[] = [];
    return {
      calls,
      run: (_file, args) => {
        calls.push(args);
        return Promise.resolve(args === WATCHER_ARGV ? watcher : host);
      },
    };
  }

  it('needs the watcher to have an owner and a host to be registered, both by absolute-path argv', async () => {
    const b = bus({ ok: true, stdout: '(true,)\n' }, { ok: true, stdout: '(<true>,)\n' });
    expect(await trayHostPresent(b.run)).toBe(true);
    expect(b.calls).toEqual([WATCHER_ARGV, HOST_ARGV]);
    expect(HOST_ARGV).toContain('IsStatusNotifierHostRegistered');
    expect(HOST_ARGV).toContain('/StatusNotifierWatcher');
    expect(HOST_ARGV).toContain('--session');
    expect(HOST_ARGV[HOST_ARGV.indexOf('--dest') + 1]).toBe(WATCHER_NAME);
  });

  it.each([
    ['a watcher with no host', { ok: true, stdout: '(true,)' }, { ok: true, stdout: '(<false>,)' }],
    ['a failed host query', { ok: true, stdout: '(true,)' }, { ok: false, stdout: '(<true>,)' }],
    ['an answer for the host it does not know', { ok: true, stdout: '(true,)' }, { ok: true, stdout: 'yes' }],
  ] as const)('is false for %s', async (_name, watcher, host) => {
    expect(await trayHostPresent(bus(watcher, host).run)).toBe(false);
  });

  it('asks nothing about the host when the watcher has no owner', async () => {
    const b = bus({ ok: true, stdout: '(false,)' }, { ok: true, stdout: '(<true>,)' });
    expect(await trayHostPresent(b.run)).toBe(false);
    expect(b.calls).toEqual([WATCHER_ARGV]);
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
  it('does not reject when the spawn itself throws (an argument with a NUL)', async () => {
    expect(await runFile('/bin/sh', ['a\0b'], 5_000)).toEqual({ ok: false, stdout: '' });
  });
  it('passes the child the session-bus names alone, not HOME', async () => {
    const before = process.env['HOME'];
    process.env['HOME'] = '/home/someone';
    try {
      const r = await runFile('/usr/bin/env', [], 5_000);
      expect(r.stdout).not.toContain('HOME=');
    } finally {
      if (before === undefined) delete process.env['HOME'];
      else process.env['HOME'] = before;
    }
  });
  it('kills a run past its timeout', async () => {
    const r = await runFile('/bin/sleep', ['30'], 100);
    expect(r.ok).toBe(false);
  });
});
