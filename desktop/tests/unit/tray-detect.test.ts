// The tray host probe (issue #1589): the gdbus command line and its reading,
// against a fake runner. No bus, no gdbus.
import { describe, expect, it } from 'vitest';

import { busEnv, GDBUS, HOST_ARGV, parseHasOwner, parseHostRegistered, trayHostPresent, watcherPresent, WATCHER_ARGV, WATCHER_NAME } from '../../main/tray-detect.ts';
import type { RunFile } from '../../main/tray-detect.ts';
import { runFile, type RunResult } from '../../main/voice/spawn.ts';

function fake(result: RunResult | null): { run: RunFile; calls: [string, readonly string[], Readonly<Record<string, string>>][] } {
  const calls: [string, readonly string[], Readonly<Record<string, string>>][] = [];
  return {
    calls,
    run: (file, args, env) => {
      calls.push([file, args, env]);
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
    const f = fake({ code: 0, stdout: '(true,)\n' });
    expect(await watcherPresent(f.run)).toBe(true);
    expect(f.calls).toEqual([[GDBUS, WATCHER_ARGV, busEnv()]]);
    expect(GDBUS.startsWith('/')).toBe(true);
    expect(WATCHER_ARGV).toContain('--session');
    expect(WATCHER_ARGV.at(-1)).toBe(WATCHER_NAME);
  });

  it.each([
    ['no owner', { code: 0, stdout: '(false,)\n' }],
    ['a failed run', { code: 1, stdout: '' }],
    ['a failed run that printed true', { code: 1, stdout: '(true,)' }],
    ['an answer it does not know', { code: 0, stdout: 'yes' }],
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
    const b = bus({ code: 0, stdout: '(true,)\n' }, { code: 0, stdout: '(<true>,)\n' });
    expect(await trayHostPresent(b.run)).toBe(true);
    expect(b.calls).toEqual([WATCHER_ARGV, HOST_ARGV]);
    expect(HOST_ARGV).toContain('IsStatusNotifierHostRegistered');
    expect(HOST_ARGV).toContain('/StatusNotifierWatcher');
    expect(HOST_ARGV).toContain('--session');
    expect(HOST_ARGV[HOST_ARGV.indexOf('--dest') + 1]).toBe(WATCHER_NAME);
  });

  it.each([
    ['a watcher with no host', { code: 0, stdout: '(true,)' }, { code: 0, stdout: '(<false>,)' }],
    ['a failed host query', { code: 0, stdout: '(true,)' }, { code: 1, stdout: '(<true>,)' }],
    ['an answer for the host it does not know', { code: 0, stdout: '(true,)' }, { code: 0, stdout: 'yes' }],
  ] as const)('is false for %s', async (_name, watcher, host) => {
    expect(await trayHostPresent(bus(watcher, host).run)).toBe(false);
  });

  it('asks nothing about the host when the watcher has no owner', async () => {
    const b = bus({ code: 0, stdout: '(false,)' }, { code: 0, stdout: '(<true>,)' });
    expect(await trayHostPresent(b.run)).toBe(false);
    expect(b.calls).toEqual([WATCHER_ARGV]);
  });
});

describe('busEnv', () => {
  it('carries the session-bus names alone, not HOME or PATH', () => {
    expect(busEnv({ HOME: '/home/someone', PATH: '/bin', DBUS_SESSION_BUS_ADDRESS: 'unix:path=/x', XDG_RUNTIME_DIR: '/run/user/1' })).toEqual({
      DBUS_SESSION_BUS_ADDRESS: 'unix:path=/x',
      XDG_RUNTIME_DIR: '/run/user/1',
    });
    expect(busEnv({ HOME: '/home/someone' })).toEqual({});
  });
});

describe('the one runFile (voice/spawn.ts)', () => {
  it('is not ok for a failing or missing file, and never rejects', async () => {
    expect(await watcherPresent(() => runFile('/bin/sh', ['-c', 'exit 3'], {}))).toBe(false);
    expect(await runFile('/nonexistent/gdbus', [], {})).toBeNull();
  });
  it('does not reject when the spawn itself throws (an argument with a NUL)', async () => {
    expect(await runFile('/bin/sh', ['a\0b'], {})).toBeNull();
  });
  it('hands the child the env it is given and nothing else', async () => {
    const r = await runFile('/usr/bin/env', [], busEnv({ HOME: '/home/someone', XDG_RUNTIME_DIR: '/run/user/1' }));
    expect(r?.stdout).toContain('XDG_RUNTIME_DIR=/run/user/1');
    expect(r?.stdout).not.toContain('HOME=');
  });
});
