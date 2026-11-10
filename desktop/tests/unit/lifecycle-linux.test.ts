// The Linux lock watcher (issue #1588, D5-02): the line parser on recorded
// `gdbus` output, and the watcher against a fake gdbus script run as a real
// child (argv only, no shell). No logind, no bus, no Electron.
import { EventEmitter } from 'node:events';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { LinuxLifecycle, MAX_RESTARTS, parseDisplaySession, parseLine, parseLockedHint, parseSession, parseSessionType, parseUserSessions, SLOW_RETRY_MS, type PowerMonitorLike } from '../../main/lifecycle-linux.ts';
import { Lifecycle, type LifecycleDeps } from '../../main/lifecycle.ts';
import { spawnWatch, type WatchChild } from '../../main/voice/spawn.ts';

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
  // Recorded on a COSMIC desktop: the user's Display property.
  display: "(<('5', objectpath '/org/freedesktop/login1/session/_35')>,)\n",
  // The user's Sessions property, one graphical session and two.
  sessions1: "(<[('5', objectpath '/org/freedesktop/login1/session/_35')]>,)\n",
  sessions2: "(<[('5', objectpath '/org/freedesktop/login1/session/_35'), ('7', objectpath '/org/freedesktop/login1/session/_37')]>,)\n",
  type: "(<'wayland'>,)\n",
  noSession: 'Error: GDBus.Error:org.freedesktop.login1.NoSessionForPID: PID 154320 does not belong to any known session\n',
};

const lines = {
  lock: `${SESSION}: ${S}.Lock ()`,
  unlock: `${SESSION}: ${S}.Unlock ()`,
  hintOn: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'LockedHint': <true>}, @as [])`,
  hintOff: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'LockedHint': <false>}, @as [])`,
  hintMixed: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'IdleHint': <false>, 'LockedHint': <true>}, @as [])`,
  // A string property whose text looks like the key; only the real key counts.
  hintInString: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'Note': <'x, 'LockedHint': <false>'>, 'IdleHint': <true>}, @as [])`,
  hintInStringThenReal: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'Note': <'x, 'LockedHint': <false>'>, 'LockedHint': <true>}, @as [])`,
  hintInEscaped: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'Note': <'it\\'s {'LockedHint': <true>}'>}, @as [])`,
  hintBadValue: `${SESSION}: org.freedesktop.DBus.Properties.PropertiesChanged ('${S}', {'LockedHint': <uint32 1>}, @as [])`,
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
    { name: 'LockedHint text inside a string value is not the key (S6)', line: lines.hintInString, want: { kind: 'ignored' } },
    { name: 'the real key after a string that looks like it (S6)', line: lines.hintInStringThenReal, want: { kind: 'event', event: { kind: 'lock', locked: true } } },
    { name: 'an escaped quote inside a string value (S6)', line: lines.hintInEscaped, want: { kind: 'ignored' } },
    { name: 'a LockedHint that is not a boolean', line: lines.hintBadValue, want: { kind: 'malformed' } },
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
  it('reads the display session of the user', () => {
    expect(parseDisplaySession(RECORDED.display)).toBe('/org/freedesktop/login1/session/_35');
    // No display session, a bad path, garbage, and an unanchored match.
    expect(parseDisplaySession("(<('', objectpath '/')>,)")).toBeNull();
    expect(parseDisplaySession("(<('5', objectpath '/org/freedesktop/login1/session/../x')>,)")).toBeNull();
    expect(parseDisplaySession("(<('5', objectpath '/org/freedesktop/login1/session/_35')>,) extra")).toBeNull();
    expect(parseDisplaySession("x(<('5', objectpath '/org/freedesktop/login1/session/_35')>,)")).toBeNull();
    expect(parseDisplaySession(RECORDED.session)).toBeNull();
    expect(parseDisplaySession(RECORDED.noSession)).toBeNull();
    expect(parseDisplaySession('')).toBeNull();
  });
  it('reads the sessions of the user', () => {
    expect(parseUserSessions(RECORDED.sessions1)).toEqual([{ id: '5', path: '/org/freedesktop/login1/session/_35' }]);
    expect(parseUserSessions(RECORDED.sessions2)).toEqual([
      { id: '5', path: '/org/freedesktop/login1/session/_35' },
      { id: '7', path: '/org/freedesktop/login1/session/_37' },
    ]);
    // None, a bad path, garbage, and a good entry followed by a bad one.
    expect(parseUserSessions('(<@a(so) []>,)')).toEqual([]);
    expect(parseUserSessions("(<[('5', objectpath '/org/freedesktop/login1/session/../x')]>,)")).toBeNull();
    expect(parseUserSessions("(<[('5', objectpath '/org/freedesktop/login1/session/_35'), junk]>,)")).toBeNull();
    expect(parseUserSessions("(<[('5', objectpath '/org/freedesktop/login1/session/_35')]>,) extra")).toBeNull();
    expect(parseUserSessions(RECORDED.display)).toBeNull();
    expect(parseUserSessions(RECORDED.noSession)).toBeNull();
    expect(parseUserSessions('')).toBeNull();
  });
  it('reads the type of a session', () => {
    expect(parseSessionType(RECORDED.type)).toBe('wayland');
    expect(parseSessionType("(<'tty'>,)")).toBe('tty');
    expect(parseSessionType('(<true>,)')).toBeNull();
    expect(parseSessionType('')).toBeNull();
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
  gate.leave = null;
  dir = mkdtempSync(join(tmpdir(), 'conch-gdbus-'));
});
afterEach(() => {
  vi.useRealTimers();
  rmSync(dir, { recursive: true, force: true });
});

// fakeGdbus writes a script that answers like gdbus: the call replies come
// from files next to it, and the monitor prints a file's lines and waits.
// argv is recorded one per line in argv.log.
function fakeGdbus(opts: { session?: string; sessionCode?: number; display?: string; displayCode?: number; sessions?: string; types?: Readonly<Record<string, string>>; hint?: string; monitor?: readonly string[]; monitorExits?: boolean; monitorNoOwner?: boolean; monitorDelaySecs?: string }): string {
  const bin = join(dir, 'bin');
  mkdirSync(bin);
  const path = join(bin, 'gdbus');
  writeFileSync(join(dir, 'session'), opts.session ?? RECORDED.session);
  writeFileSync(join(dir, 'hint'), opts.hint ?? RECORDED.hintFalse);
  writeFileSync(join(dir, 'display'), opts.display ?? RECORDED.noSession);
  writeFileSync(join(dir, 'sessions'), opts.sessions ?? RECORDED.sessions1);
  const typeCases = Object.entries(opts.types ?? { '/org/freedesktop/login1/session/_35': 'wayland' })
    .map(([path, type]) => `      *--object-path\\ ${path}\\ *login1.Session\\ Type*) echo "(<'${type}'>,)"; exit 0 ;;`)
    .join('\n');
  writeFileSync(join(dir, 'monitor'), (opts.monitor ?? []).map((l) => l + '\n').join(''));
  writeFileSync(
    path,
    `#!/bin/sh
D="${dir}"
echo "ARGV $*" >> "$D/argv.log"
case "$1" in
  call)
    case "$*" in
      *GetSessionByPID*) cat "$D/session"; exit ${String(opts.sessionCode ?? 0)} ;;
      *Properties.Get*login1.User*Display*) cat "$D/display"; exit ${String(opts.displayCode ?? 1)} ;;
      *Properties.Get*login1.User*Sessions*) cat "$D/sessions"; exit 0 ;;
${typeCases}
      *LockedHint*) cat "$D/hint"; exit 0 ;;
    esac
    exit 1 ;;
  monitor)
    echo "PID $$" >> "$D/argv.log"
    echo '${RECORDED.preamble0}'
    ${opts.monitorNoOwner === true ? '' : `sleep ${opts.monitorDelaySecs ?? '0'}; echo OWNED >> "$D/argv.log"; echo '${RECORDED.preamble1}'`}
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

// A promise a test can hold the leave behind, so a suspend stays in flight.
const gate: { leave: Promise<void> | null } = { leave: null };

function rig(): Rig {
  const calls: string[] = [];
  const logs: string[] = [];
  let muted = false;
  const deps: LifecycleDeps = {
    voice: {
      state: () => ({ status: 'running', helper: { muted } }),
      setMute: (m) => {
        calls.push(`mute ${String(m)}`);
        muted = m;
        return Promise.resolve({ ok: true });
      },
      leave: () => {
        calls.push('leave');
        return gate.leave ?? Promise.resolve();
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
    // Hold the leave, so the first suspend is still acting when the second
    // signal arrives.
    let release: () => void = () => undefined;
    gate.leave = new Promise<void>((resolve) => {
      release = resolve;
    });
    const gdbus = fakeGdbus({ monitor: [lines.sleepOn] });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => r.life.suspended);
    r.power.emit('suspend');
    release();
    await until(() => r.calls.includes('end suspend'));
    expect(r.calls.filter((c) => c === 'leave')).toHaveLength(1);
    expect(r.calls.filter((c) => c === 'end suspend')).toHaveLength(1);
    await until(() => !r.life.suspended);
    r.power.emit('resume');
    expect(r.life.suspended).toBe(false);
    w.stop();
    // Stopped: powerMonitor events are no longer listened to.
    gate.leave = null;
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

const argvLog = (): string => readFileSync(join(dir, 'argv.log'), 'utf8');

describe('a process with no session of its own (M1)', () => {
  it('falls back to the display session of the user and watches it', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ session: RECORDED.noSession, sessionCode: 1, display: RECORDED.display, displayCode: 0, hint: RECORDED.hintTrue });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 4242, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    // Started locked: the hint of the display session says so.
    await until(() => r.calls.includes('end lock'));
    expect(r.calls).toEqual(expect.arrayContaining(['end lock', 'mute true']));
    const argv = argvLog();
    expect(argv).toContain('ARGV call --system --dest org.freedesktop.login1 --object-path /org/freedesktop/login1/user/self --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.User Display');
    expect(argv).toContain('--object-path /org/freedesktop/login1/user/self --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.User Sessions');
    expect(argv).toContain('--object-path /org/freedesktop/login1/session/_35 --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.Session Type');
    expect(argv).toContain('--object-path /org/freedesktop/login1/session/_35 --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.Session LockedHint');
    w.stop();
  });

  it('does not ask for the display session when the process has its own', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ display: RECORDED.display, displayCode: 0 });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    expect(argvLog()).not.toContain('login1.User Display');
    w.stop();
  });

  it.each([
    { name: 'both fail', opts: { session: RECORDED.noSession, sessionCode: 1, display: RECORDED.noSession, displayCode: 1 } },
    { name: 'the fallback answers garbage', opts: { session: RECORDED.noSession, sessionCode: 1, display: 'garbage', displayCode: 0 } },
    { name: 'the fallback answers no display session', opts: { session: RECORDED.noSession, sessionCode: 1, display: "(<('', objectpath '/')>,)\n", displayCode: 0 } },
    { name: 'the fallback names a path outside the sessions', opts: { session: RECORDED.noSession, sessionCode: 1, display: "(<('5', objectpath '/org/freedesktop/login1/session/_35/../../x')>,)\n", displayCode: 0 } },
  ])('$name: stays unobserved, starts no monitor, says so once', async ({ opts }) => {
    const r = rig();
    const gdbus = fakeGdbus(opts);
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), idlePollMs: 100000 });
    await w.start();
    expect(r.life.lockObservation).toBe('unavailable');
    expect(r.logs.filter((l) => l.includes('lock not observed'))).toHaveLength(1);
    expect(argvLog()).not.toContain('ARGV monitor');
    expect(r.calls).toEqual(['hidden']);
    w.stop();
  });
});

describe('the hint is read only once the monitor is subscribed (S2)', () => {
  it('waits for the owner line, at start', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ monitorDelaySecs: '0.3' });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus) });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    const argv = argvLog();
    expect(argv.indexOf('OWNED')).toBeGreaterThan(-1);
    expect(argv.indexOf('OWNED')).toBeLessThan(argv.indexOf('LockedHint'));
    w.stop();
  });

  it('waits at a restart too', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ monitorDelaySecs: '0.2' });
    let n = 0;
    // The first monitor dies at once, the second is the delayed fake.
    const watch = (file: string, args: readonly string[], env: Readonly<Record<string, string>>): ReturnType<typeof spawnWatch> => (++n === 1 ? spawnWatch('/bin/sh', ['-c', 'exit 0'], {}) : spawnWatch(file, args, env));
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), watch, backoffMs: [1, 1, 1] });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    expect(n).toBe(2);
    const argv = argvLog();
    // The only hint read follows the only owner line.
    expect(argv.indexOf('OWNED')).toBeLessThan(argv.indexOf('LockedHint'));
    w.stop();
  });

  it('a monitor that never says it is subscribed is not trusted: no hint read, retried, then reported', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ monitorNoOwner: true });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), readyMs: 30, backoffMs: [1], idlePollMs: 100000 });
    await w.start();
    await until(() => r.logs.some((l) => l.includes('lock not observed')));
    expect(r.logs.join('\n')).toContain('did not say it was subscribed');
    expect(argvLog()).not.toContain('LockedHint');
    expect(r.life.lockObservation).toBe('unavailable');
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

// ---------------------------------------- two graphical sessions (S-1)

describe('the user has more than one session (S-1)', () => {
  const noOwn = { session: RECORDED.noSession, sessionCode: 1, display: RECORDED.display, displayCode: 0 } as const;
  const T35 = '/org/freedesktop/login1/session/_35';
  const T37 = '/org/freedesktop/login1/session/_37';

  it('one graphical session among others: the display session is watched', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ ...noOwn, sessions: RECORDED.sessions2, types: { [T35]: 'wayland', [T37]: 'tty' } });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), sessionId: null });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    expect(argvLog()).toContain(`--object-path ${T35} --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.Session LockedHint`);
    w.stop();
  });

  it('two graphical sessions and XDG_SESSION_ID names one of them: that one is watched', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ ...noOwn, sessions: RECORDED.sessions2, types: { [T35]: 'x11', [T37]: 'wayland' } });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), sessionId: '7' });
    await w.start();
    await until(() => r.life.lockObservation === 'available');
    const argv = argvLog();
    expect(argv).toContain(`--object-path ${T37} --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.Session LockedHint`);
    expect(argv).not.toContain(`${T35} --method org.freedesktop.DBus.Properties.Get org.freedesktop.login1.Session LockedHint`);
    w.stop();
  });

  it.each([
    { name: 'XDG_SESSION_ID names a session that is not in the list', id: '9' },
    { name: 'XDG_SESSION_ID is absent', id: null },
  ])('two graphical sessions and $name: unobserved, no monitor', async ({ id }) => {
    const r = rig();
    const gdbus = fakeGdbus({ ...noOwn, sessions: RECORDED.sessions2, types: { [T35]: 'x11', [T37]: 'wayland' } });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), sessionId: id, idlePollMs: 100000, slowRetryMs: 100000 });
    await w.start();
    expect(r.life.lockObservation).toBe('unavailable');
    expect(r.logs.filter((l) => l.includes('lock not observed'))).toHaveLength(1);
    expect(argvLog()).not.toContain('ARGV monitor');
    w.stop();
  });

  it('two graphical sessions and XDG_SESSION_ID names the text one: unobserved', async () => {
    const r = rig();
    const gdbus = fakeGdbus({ ...noOwn, sessions: RECORDED.sessions2, types: { [T35]: 'wayland', [T37]: 'x11' } });
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), sessionId: '3', idlePollMs: 100000, slowRetryMs: 100000 });
    await w.start();
    expect(r.life.lockObservation).toBe('unavailable');
    w.stop();
  });

  it.each([
    { name: 'Sessions cannot be read', opts: { ...noOwn, sessions: 'garbage' } },
    { name: 'a session type cannot be read', opts: { ...noOwn, sessions: RECORDED.sessions2, types: { [T35]: 'wayland' } } },
  ])('$name: unobserved, no monitor', async ({ opts }) => {
    const r = rig();
    const gdbus = fakeGdbus(opts);
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), sessionId: '5', idlePollMs: 100000, slowRetryMs: 100000 });
    await w.start();
    expect(r.life.lockObservation).toBe('unavailable');
    expect(argvLog()).not.toContain('ARGV monitor');
    w.stop();
  });
});

// --------------------------------------- fake monitor children (S-2..S-5)

class FakeStream extends EventEmitter {
  setEncoding(): void {
    // Chunks are emitted as strings already.
  }
  resume(): void {
    // Nothing is buffered.
  }
}

class FakeChild extends EventEmitter {
  readonly stdout = new FakeStream();
  readonly stderr = new FakeStream();
  killed = false;
  kill(): boolean {
    this.killed = true;
    return true;
  }
}

const asWatch = (c: FakeChild): WatchChild => c as unknown as WatchChild;

describe('a burst of lines in one chunk (S-5)', () => {
  it('a Lock after thousands of other-session lines in the same chunk still locks', async () => {
    const r = rig();
    const gdbus = fakeGdbus({});
    const child = new FakeChild();
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), watch: () => asWatch(child), idlePollMs: 100000 });
    const started = w.start();
    await until(() => child.stdout.listenerCount('data') > 0);
    const other = lines.hintOn.replace(SESSION, OTHER);
    const burst = Array.from({ length: 3000 }, () => other);
    const chunk = [RECORDED.preamble1, ...burst, lines.lock, ''].join('\n');
    expect(chunk.length).toBeGreaterThan(64 * 1024);
    child.stdout.emit('data', chunk);
    await until(() => r.calls.includes('end lock'));
    expect(r.life.locked).toBe(true);
    await started;
    // A remainder with no newline past the cap is dropped, not parsed, and
    // the next line is read whole.
    child.stdout.emit('data', 'x'.repeat(70 * 1024));
    child.stdout.emit('data', '\n' + lines.unlock + '\n');
    await until(() => !r.life.locked);
    w.stop();
  });
});

describe('a logind source that is gone (S-2)', () => {
  it('the monitor dies while locked and the watcher gives up: the lock is released, text stays hidden', async () => {
    const r = rig();
    const gdbus = fakeGdbus({});
    let starts = 0;
    const watch = (): WatchChild => {
      const child = new FakeChild();
      const first = ++starts === 1;
      setTimeout(() => {
        if (first) child.stdout.emit('data', RECORDED.preamble1 + '\n' + lines.lock + '\n');
        child.emit('close');
      }, 0);
      return asWatch(child);
    };
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), watch, backoffMs: [1, 1, 1], idlePollMs: 100000, slowRetryMs: 100000 });
    await w.start();
    await until(() => r.calls.includes('end lock'));
    await until(() => r.logs.some((l) => l.includes('lock not observed')));
    expect(starts).toBe(1 + MAX_RESTARTS);
    // Locked, then the source was dropped: unmute and join work again (they
    // read locked), nothing acted, and the text is still hidden.
    expect(r.life.locked).toBe(false);
    expect(r.life.lockObservation).toBe('unavailable');
    expect(r.calls).toEqual(['hidden', 'end lock', 'mute true']);
    w.stop();
  });
});

describe('the give-up is not permanent (S-3)', () => {
  it('keeps trying at SLOW_RETRY_MS, recovers when the monitor comes back, and stops at stop()', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const r = rig();
    const gdbus = fakeGdbus({});
    let starts = 0;
    let recovered = false;
    const watch = (file: string, args: readonly string[], env: Readonly<Record<string, string>>): WatchChild => {
      starts++;
      return recovered ? spawnWatch(file, args, env) : spawnWatch('/bin/sh', ['-c', 'exit 0'], {});
    };
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), watch, backoffMs: [1, 1, 1], idlePollMs: 100000 });
    await w.start();
    await vi.waitFor(
      async () => {
        await vi.advanceTimersByTimeAsync(5);
        expect(r.logs.filter((l) => l.includes('lock not observed'))).toHaveLength(1);
      },
      { timeout: 5000, interval: 10 },
    );
    expect(starts).toBe(1 + MAX_RESTARTS);
    // Not before the interval...
    await vi.advanceTimersByTimeAsync(SLOW_RETRY_MS - 1000);
    expect(starts).toBe(1 + MAX_RESTARTS);
    // ...and again at it, and again after that (still dying).
    await vi.advanceTimersByTimeAsync(1000);
    await vi.waitFor(() => { expect(starts).toBe(2 + MAX_RESTARTS); }, { timeout: 5000, interval: 10 });
    await vi.waitFor(async () => { await vi.advanceTimersByTimeAsync(1); expect(vi.getTimerCount()).toBe(1); }, { timeout: 5000, interval: 10 });
    await vi.advanceTimersByTimeAsync(SLOW_RETRY_MS);
    await vi.waitFor(() => { expect(starts).toBe(3 + MAX_RESTARTS); }, { timeout: 5000, interval: 10 });
    // The monitor comes back: observed again.
    recovered = true;
    await vi.waitFor(async () => { await vi.advanceTimersByTimeAsync(1); expect(vi.getTimerCount()).toBe(1); }, { timeout: 5000, interval: 10 });
    await vi.advanceTimersByTimeAsync(SLOW_RETRY_MS);
    await vi.waitFor(() => { expect(r.life.lockObservation).toBe('available'); }, { timeout: 5000, interval: 10 });
    w.stop();
  });

  it('stop() ends the retries', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const r = rig();
    const gdbus = fakeGdbus({});
    let starts = 0;
    const watch = (): WatchChild => {
      starts++;
      return spawnWatch('/bin/sh', ['-c', 'exit 0'], {});
    };
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: (l) => r.logs.push(l), findGdbus: () => Promise.resolve(gdbus), watch, backoffMs: [1, 1, 1], idlePollMs: 100000 });
    await w.start();
    await vi.waitFor(
      async () => {
        await vi.advanceTimersByTimeAsync(5);
        expect(r.logs.filter((l) => l.includes('lock not observed'))).toHaveLength(1);
      },
      { timeout: 5000, interval: 10 },
    );
    w.stop();
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(SLOW_RETRY_MS * 3);
    expect(starts).toBe(1 + MAX_RESTARTS);
  });

  it('stop() while waiting for the monitor to say it is subscribed clears that wait', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const r = rig();
    const gdbus = fakeGdbus({});
    const child = new FakeChild();
    const w = new LinuxLifecycle({ rules: r.life, powerMonitor: r.power, pid: 1, log: () => undefined, findGdbus: () => Promise.resolve(gdbus), watch: () => asWatch(child), readyMs: 60_000, idlePollMs: 100000 });
    const started = w.start();
    await vi.waitFor(() => { expect(child.stdout.listenerCount('data')).toBe(1); }, { timeout: 5000, interval: 10 });
    expect(vi.getTimerCount()).toBe(1);
    w.stop();
    expect(vi.getTimerCount()).toBe(0);
    expect(child.killed).toBe(true);
    // start() does not hang on the wait that was cleared.
    await started;
  });
});
