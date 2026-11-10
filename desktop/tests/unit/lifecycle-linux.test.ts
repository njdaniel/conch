// The Linux lock watcher (issue #1588, D5-02): the line parser on recorded
// `gdbus` output, and the watcher against a fake gdbus script run as a real
// child (argv only, no shell). No logind, no bus, no Electron.
import { EventEmitter } from 'node:events';
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LinuxLifecycle, MAX_RESTARTS, parseLine, parseLockedHint, parseSession, type PowerMonitorLike } from '../../main/lifecycle-linux.ts';
import { Lifecycle, type LifecycleDeps } from '../../main/lifecycle.ts';
import { spawnWatch } from '../../main/voice/spawn.ts';

const SESSION = '/org/freedesktop/login1/session/_32';
const OTHER = '/org/freedesktop/login1/session/_7';
const S = 'org.freedesktop.login1.Session';

// Recorded from `gdbus monitor --system --dest org.freedesktop.login1` and
// `gdbus call` on a GLib 2.8x desktop; session numbers are the only ids, and
// no user, seat or device name is in them.
const RECORDED = {
  preamble0: 'Monitoring signals from all objects owned by org.freedesktop.login1',
  preamble1: 'The name org.freedesktop.login1 is owned by :1.15',
  session: "(objectpath '/org/freedesktop/login1/session/_32',)\n",
  hintFalse: '(<false>,)\n',
  hintTrue: '(<true>,)\n',
  noSession: 'Error: GDBus.Error:org.freedesktop.login1.NoSessionForPID: PID 154320 does not belong to any known session\n',
};

const lines = {
  lock: `${SESSION}: ${S}.Lock ()`,
  unlock: `${SESSION}: ${S}.Unlock ()`,
  hintOn: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'LockedHint': <true>}, @as [])`,
  hintOff: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'LockedHint': <false>}, @as [])`,
  hintMixed: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'IdleHint': <false>, 'LockedHint': <true>}, @as [])`,
  idleOnly: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'IdleHint': <true>, 'IdleSinceHint': <uint64 1730000000000000>}, @as [])`,
  sleepOn: '/org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForSleep (true,)',
  sleepOff: '/org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForSleep (false,)',
};

describe('parseLine', () => {
  const table: { name: string; line: string; want: ReturnType<typeof parseLine> }[] = [
    { name: 'Lock', line: lines.lock, want: { kind: 'event', event: { kind: 'lock', locked: true } } },
    { name: 'Unlock', line: lines.unlock, want: { kind: 'event', event: { kind: 'lock', locked: false } } },
    { name: 'LockedHint true', line: lines.hintOn, want: { kind: 'event', event: { kind: 'lock', locked: true } } },
    { name: 'LockedHint false', line: lines.hintOff, want: { kind: 'event', event: { kind: 'lock', locked: false } } },
    { name: 'LockedHint among other properties', line: lines.hintMixed, want: { kind: 'event', event: { kind: 'lock', locked: true } } },
    { name: 'other properties of the session', line: lines.idleOnly, want: { kind: 'ignored' } },
    { name: 'PrepareForSleep(true)', line: lines.sleepOn, want: { kind: 'event', event: { kind: 'sleep', sleeping: true } } },
    { name: 'PrepareForSleep(false)', line: lines.sleepOff, want: { kind: 'event', event: { kind: 'sleep', sleeping: false } } },
    { name: 'another session Lock', line: lines.lock.replace(SESSION, OTHER), want: { kind: 'ignored' } },
    { name: 'another session LockedHint', line: lines.hintOn.replace(SESSION, OTHER), want: { kind: 'ignored' } },
    { name: 'this session, another member', line: `${SESSION}: ${S}.PauseDevice ('x')`.replace("('x')", '(226, 0, "pause")'), want: { kind: 'ignored' } },
    { name: 'a Lock from the wrong interface', line: `${SESSION}: org.example.Other.Lock ()`, want: { kind: 'ignored' } },
    { name: 'the manager but not PrepareForSleep', line: '/org/freedesktop/login1: org.freedesktop.login1.Manager.SessionNew (\'_32\', objectpath \'/x\')', want: { kind: 'ignored' } },
    { name: 'PrepareForSleep from a session path', line: `${SESSION}: org.freedesktop.login1.Manager.PrepareForSleep (true,)`, want: { kind: 'ignored' } },
    { name: 'preamble 1', line: RECORDED.preamble0, want: { kind: 'ignored' } },
    { name: 'preamble 2', line: RECORDED.preamble1, want: { kind: 'ignored' } },
    { name: 'blank', line: '', want: { kind: 'ignored' } },
    { name: 'Lock with arguments', line: `${SESSION}: ${S}.Lock (1,)`, want: { kind: 'malformed' } },
    { name: 'PrepareForSleep with a bad argument', line: '/org/freedesktop/login1: org.freedesktop.login1.Manager.PrepareForSleep (maybe,)', want: { kind: 'malformed' } },
    { name: 'noise', line: 'Segmentation fault', want: { kind: 'malformed' } },
    { name: 'a truncated signal', line: `${SESSION}: ${S}.Lock (`, want: { kind: 'malformed' } },
    { name: 'a path with a shell character', line: `${SESSION};rm: ${S}.Lock ()`, want: { kind: 'malformed' } },
    { name: 'PropertiesChanged with no interface', line: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ()`, want: { kind: 'malformed' } },
  ];
  it.each(table)('$name', ({ line, want }) => {
    expect(parseLine(line, SESSION)).toEqual(want);
  });
});

describe('the call replies', () => {
  it('reads the session path', () => {
    expect(parseSession(RECORDED.session)).toBe(SESSION);
    expect(parseSession(RECORDED.noSession)).toBeNull();
    expect(parseSession("(objectpath '/org/freedesktop/login1/session/../x',)")).toBeNull();
    expect(parseSession('')).toBeNull();
  });
  it('reads the hint', () => {
    expect(parseLockedHint(RECORDED.hintFalse)).toBe(false);
    expect(parseLockedHint(RECORDED.hintTrue)).toBe(true);
    expect(parseLockedHint('(<uint32 1>,)')).toBeNull();
    expect(parseLockedHint('')).toBeNull();
  });
});

// ------------------------------------------------------------- the watcher

class Power extends EventEmitter implements PowerMonitorLike {
  idle = 'active';
  getSystemIdleState(): string {
    return this.idle;
  }
}

let dir = '';
beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'conch-gdbus-'));
});
afterEach(() => {
  vi.useRealTimers();
  rmSync(dir, { recursive: true, force: true });
});

// fakeGdbus writes a script that answers like gdbus: the call replies come
// from files next to it, and the monitor prints a file's lines and waits.
// argv is recorded one per line in argv.log.
function fakeGdbus(opts: { session?: string; sessionCode?: number; hint?: string; monitor?: readonly string[]; monitorExits?: boolean }): string {
  const bin = join(dir, 'bin');
  mkdirSync(bin);
  const path = join(bin, 'gdbus');
  writeFileSync(join(dir, 'session'), opts.session ?? RECORDED.session);
  writeFileSync(join(dir, 'hint'), opts.hint ?? RECORDED.hintFalse);
  writeFileSync(join(dir, 'monitor'), [RECORDED.preamble0, RECORDED.preamble1, ...(opts.monitor ?? [])].join('\n') + '\n');
  writeFileSync(
    path,
    `#!/bin/sh
D="${dir}"
echo "ARGV $*" >> "$D/argv.log"
case "$1" in
  call)
    case "$*" in
      *GetSessionByPID*) cat "$D/session"; exit ${String(opts.sessionCode ?? 0)} ;;
      *LockedHint*) cat "$D/hint"; exit 0 ;;
    esac
    exit 1 ;;
  monitor)
    echo "PID $$" >> "$D/argv.log"
    cat "$D/monitor"
    ${opts.monitorExits === true ? 'exit 0' : 'exec sleep 30'} ;;
esac
exit 2
`,
  );
  chmodSync(path, 0o755);
  return path;
}

interface Rig {
  readonly calls: string[];
  readonly logs: string[];
  readonly life: Lifecycle;
  readonly power: Power;
}

function rig(): Rig {
  const calls: string[] = [];
  const logs: string[] = [];
  const deps: LifecycleDeps = {
    voice: {
      state: () => ({ status: 'running' }),
      setMute: (m) => {
        calls.push(`mute ${String(m)}`);
        return Promise.resolve({ ok: true });
      },
      leave: () => {
        calls.push('leave');
        return Promise.resolve();
      },
      setLockMuted: () => undefined,
    },
    companion: { end: (r) => { calls.push(`end ${r}`); return Promise.resolve(true); } },
    notifications: { setLocked: (l) => { calls.push(l ? 'hidden' : 'shown'); } },
    log: (line) => logs.push(line),
  };
  return { calls, logs, life: new Lifecycle(deps), power: new Power() };
}

const until = async (cond: () => boolean): Promise<void> => {
  for (let i = 0; i < 200 && !cond(); i++) await new Promise((r) => setTimeout(r, 10));
  expect(cond()).toBe(true);
};

describe('the watcher with a fake gdbus child', () => {
  it('finds the session by this pid, reads the hint, and observes a lock and an unlock', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ monitor: [lines.lock, lines.unlock] });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 4242, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), watch: spawnWatch });
    await w.start();
    await until(() => r.calls.includes('shown') && r.calls.includes('mute true'));
    expect(r.life.lockObservation).toBe('available');
    expect(r.calls).toEqual(expect.arrayContaining(['hidden', 'end lock', 'mute true', 'shown']));
    expect(r.life.locked).toBe(false);
    w.stop();
    const argv = (await import('node:fs')).readFileSync(join(dir, 'argv.log'), 'utf8');
    // Argv only: the pid, the system bus, the destination; no shell string.
    expect(argv).toContain('ARGV call --system --dest org.freedesktop.login1 --object-path /org/freedesktop/login1 --method org.freedesktop.login1.Manager.GetSessionByPID 4242');
    expect(argv).toContain('ARGV monitor --system --dest org.freedesktop.login1');
    expect(argv).toContain(`--object-path ${SESSION} --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.Session LockedHint`);
  });

  it('starts locked when the hint says so, and counts malformed lines', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ hint: RECORDED.hintTrue, monitor: ['garbage', lines.idleOnly, 'more garbage', OTHER + ': ' + S + '.Unlock ()'] });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => w.malformed === 2);
    await until(() => r.life.locked);
    await until(() => r.calls.includes('end lock'));
    // Another session's Unlock does not unlock this one.
    expect(r.life.locked).toBe(true);
    w.stop();
  });

  it('kills its own child on stop', async () => {
    const r = rig();
    const gdbus = fakeGdbus({});
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    const pids = (await import('node:fs')).readFileSync(join(dir, 'argv.log'), 'utf8').match(/^PID (\d+)$/mu);
    const pid = Number(pids?.[1]);
    expect(pid).toBeGreaterThan(0);
    w.stop();
    await until(() => {
      try {
        process.kill(pid, 0);
        return false;
      } catch {
        return true;
      }
    });
  });

  it('suspends once for PrepareForSleep and powerMonitor together, and resume re-arms', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ monitor: [lines.sleepOn] });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => r.life.suspended);
    r.power.emit('suspend');
    expect(r.calls.filter((c) => c === 'leave')).toHaveLength(1);
    expect(r.calls.filter((c) => c === 'end suspend')).toHaveLength(1);
    r.power.emit('resume');
    expect(r.life.suspended).toBe(false);
    w.stop();
    // Stopped: powerMonitor events are no longer listened to.
    r.power.emit('suspend');
    expect(r.life.suspended).toBe(false);
  });

  it('powerMonitor alone suspends, with no gdbus at all', async () => {
    const r = rig();
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(null) });
    await w.start();
    r.power.emit('suspend');
    await until(() => r.calls.includes('end suspend'));
    w.stop();
  });

  it('treats the idle state locked as a second signal', async () => {
    vi.useFakeTimers();
    const r = rig();
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(null), idlePollMs: 1000 });
    await w.start();
    r.power.idle = 'locked';
    await vi.advanceTimersByTimeAsync(1000);
    expect(r.life.locked).toBe(true);
    expect(r.calls).toEqual(expect.arrayContaining(['end lock', 'mute true']));
    r.power.idle = 'active';
    await vi.advanceTimersByTimeAsync(1000);
    expect(r.life.locked).toBe(false);
    // It cannot make the lock observed: no logind, so text stays hidden.
    expect(r.life.lockObservation).toBe('unavailable');
    expect(r.calls).not.toContain('shown');
    w.stop();
  });
});

describe('when the lock cannot be observed', () => {
  it.each([
    { name: 'no gdbus', gdbus: null, opts: {}, why: 'gdbus is not installed' },
    { name: 'no session for this process', gdbus: 'fake', opts: { session: RECORDED.noSession, sessionCode: 1 }, why: 'no logind session' },
    { name: 'a hint that cannot be read', gdbus: 'fake', opts: { hint: 'nonsense' }, why: 'did not come back' },
  ])('$name: unavailable, hidden text, said once', async ({ gdbus, opts, why }) => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const r = rig();
    const path = gdbus === null ? null : fakeGdbus(opts);
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(path), backoffMs: [1, 1, 1], idlePollMs: 100000 });
    await w.start();
    for (let i = 0; i < 20; i++) await vi.advanceTimersByTimeAsync(5);
    await vi.waitFor(() => { expect(r.logs.some((l) => l.includes(why))).toBe(true); }, { timeout: 2000, interval: 5 });
    expect(r.life.lockObservation).toBe('unavailable');
    expect(r.calls).toEqual(['hidden']);
    expect(r.logs.filter((l) => l.includes('lock not observed'))).toHaveLength(1);
    w.stop();
  });

  it('a monitor that keeps dying is restarted at most MAX_RESTARTS times, then reported', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const r = rig();
    let starts = 0;
    const gdbus = fakeGdbus({});
    const dying = (): ReturnType<typeof spawnWatch> => {
      starts++;
      return spawnWatch('/bin/sh', ['-c', 'exit 0'], {});
    };
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), watch: dying, backoffMs: [1, 1, 1], idlePollMs: 100000 });
    await w.start();
    await vi.waitFor(
      async () => {
        await vi.advanceTimersByTimeAsync(5);
        expect(r.logs.filter((l) => l.includes('lock not observed'))).toHaveLength(1);
      },
      { timeout: 5000, interval: 10 },
    );
    expect(starts).toBe(1 + MAX_RESTARTS);
    expect(r.life.lockObservation).toBe('unavailable');
    expect(r.calls).not.toContain('shown');
    w.stop();
  });
});
