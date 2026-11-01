import { chmodSync, existsSync, mkdtempSync, mkdirSync, rmSync, statSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it, vi } from 'vitest';

import { acquire, prepareRoot, type InstanceApp } from '../../main/instance.ts';

const dirs: string[] = [];
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});
// Under /tmp, whose ancestors are the sticky root-owned kind the rule allows:
// $TMPDIR may be a group-writable directory of the developer's.
function temp(): string {
  const d = mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), 'conch-instance-'));
  dirs.push(d);
  return d;
}

function fakeApp(lock: boolean) {
  const listeners: (() => void)[] = [];
  const exit = vi.fn();
  const app: InstanceApp = {
    requestSingleInstanceLock: () => lock,
    on: (_event, listener) => listeners.push(listener),
    exit,
  };
  return { app, listeners, exit };
}

describe('one instance per data root', () => {
  it('the first instance keeps the root and focuses its window when a second is started', () => {
    const { app, listeners, exit } = fakeApp(true);
    const focus = vi.fn();
    expect(acquire(app, focus)).toBe(true);
    expect(exit).not.toHaveBeenCalled();
    listeners[0]?.();
    expect(focus).toHaveBeenCalledOnce();
  });

  it('a second instance on the same root exits at once and registers nothing', () => {
    const { app, listeners, exit } = fakeApp(false);
    expect(acquire(app, vi.fn())).toBe(false);
    expect(exit).toHaveBeenCalledWith(0);
    expect(listeners).toEqual([]);
  });
});

describe('the data root', () => {
  it('is made 0700, and an existing loose one is closed', async () => {
    const base = temp();
    const root = join(base, 'data');
    mkdirSync(root, { mode: 0o755 });
    chmodSync(root, 0o755);
    expect(await prepareRoot(root)).toEqual({ ok: true });
    expect(statSync(root).mode & 0o777).toBe(0o700);
    expect(await prepareRoot(join(base, 'new', 'deeper'))).toEqual({ ok: true });
    expect(statSync(join(base, 'new', 'deeper')).mode & 0o777).toBe(0o700);
  });

  it('refuses a symlink in its place', async () => {
    const base = temp();
    mkdirSync(join(base, 'real'));
    symlinkSync(join(base, 'real'), join(base, 'link'));
    expect(await prepareRoot(join(base, 'link'))).toEqual({ ok: false, reason: 'unsafe' });
  });

  it('judges the real parents of a root reached through a symlinked ancestor', async () => {
    const base = temp();
    const loose = join(base, 'loose');
    mkdirSync(loose);
    chmodSync(loose, 0o777);
    symlinkSync(loose, join(base, 'link'));
    // The lexical parent is a symlink; the real parent is open to everyone.
    expect(await prepareRoot(join(base, 'link', 'data'))).toEqual({ ok: false, reason: 'unsafe' });
    // A symlinked ancestor that leads somewhere private is fine.
    const tight = join(base, 'tight');
    mkdirSync(tight, { mode: 0o700 });
    symlinkSync(tight, join(base, 'tlink'));
    expect(await prepareRoot(join(base, 'tlink', 'data'))).toEqual({ ok: true });
  });

  it('refuses a link into a directory another user owns', async () => {
    const base = temp();
    const theirs = join(base, 'theirs');
    mkdirSync(theirs, { mode: 0o700 });
    symlinkSync(theirs, join(base, 'link'));
    // As if the process were another user: everything here belongs to someone else.
    expect(await prepareRoot(join(base, 'link', 'data'), { uid: (process.getuid?.() ?? 0) + 1 })).toEqual({ ok: false, reason: 'unsafe' });
    expect(await prepareRoot(join(base, 'link', 'data'))).toEqual({ ok: true });
  });

  it('refuses a root under a directory that group or others can write to', async () => {
    const base = temp();
    const loose = join(base, 'loose');
    mkdirSync(loose);
    chmodSync(loose, 0o777);
    expect(await prepareRoot(join(loose, 'data'))).toEqual({ ok: false, reason: 'unsafe' });
    chmodSync(loose, 0o775);
    expect(await prepareRoot(join(loose, 'data'))).toEqual({ ok: false, reason: 'unsafe' });
    // A sticky world-writable parent, as /tmp is, is allowed.
    chmodSync(loose, 0o1777);
    expect(await prepareRoot(join(loose, 'data2'))).toEqual({ ok: true });
  });
});
