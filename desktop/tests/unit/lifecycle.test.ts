// The lock and suspend rules (issue #1588, D5-02): events in, actions out,
// with no Electron and no process. Table-driven over events, voice state and
// share state.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { Lifecycle, MUTE_BOUND_MS, type LifecycleDeps, type LockSource } from '../../main/lifecycle.ts';

type VoiceState = 'idle' | 'running' | 'starting' | 'ended' | 'lost' | 'failed' | 'unavailable';
// ack: acknowledged and shown muted. ackUnseen: acknowledged but the view never
// shows it.
type MuteOutcome = 'ack' | 'ackUnseen' | 'refused' | 'hang' | 'throws';

function rig(state: VoiceState, mute: MuteOutcome = 'ack') {
  const calls: string[] = [];
  let muted = false;
  const deps: LifecycleDeps = {
    voice: {
      state: () => (state === 'running' ? { status: state, helper: { muted } } : { status: state }),
      setMute: (m) => {
        calls.push(`mute ${String(m)}`);
        if (mute === 'hang') return new Promise(() => undefined);
        if (mute === 'throws') return Promise.reject(new Error('gone'));
        if (mute === 'ack') muted = m;
        return Promise.resolve({ ok: mute === 'ack' || mute === 'ackUnseen' });
      },
      leave: () => {
        calls.push('leave');
        return Promise.resolve();
      },
      setLockMuted: (on) => {
        calls.push(`lockMuted ${String(on)}`);
      },
    },
    companion: {
      end: (reason) => {
        calls.push(`end ${reason}`);
        return Promise.resolve(true);
      },
    },
    notifications: {
      setLocked: (l) => {
        calls.push(`notify ${l ? 'hidden' : 'shown'}`);
      },
    },
    log: (line) => {
      calls.push(`log ${line.slice(0, 40)}`);
    },
  };
  const life = new Lifecycle(deps);
  // The constructor hides notification text: nothing observes the lock yet.
  calls.length = 0;
  return { life, calls };
}

const observed = (state: VoiceState, mute: MuteOutcome = 'ack') => {
  const r = rig(state, mute);
  r.life.observe('available');
  r.calls.length = 0;
  return r;
};

beforeEach(() => {
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
});

describe('lock', () => {
  const table: { name: string; state: VoiceState; mute: MuteOutcome; want: string[] }[] = [
    { name: 'in a call: hide text, end the share, mute', state: 'running', mute: 'ack', want: ['notify hidden', 'end lock', 'mute true', 'lockMuted true'] },
    { name: 'in a call, mute refused: leave', state: 'running', mute: 'refused', want: ['notify hidden', 'end lock', 'mute true', expect.stringContaining('log lifecycle: the mute') as string, 'leave'] },
    { name: 'in a call, mute call fails: leave', state: 'running', mute: 'throws', want: ['notify hidden', 'end lock', 'mute true', expect.stringContaining('log lifecycle: the mute') as string, 'leave'] },
    { name: 'joining: a mute that cannot be acknowledged leaves', state: 'starting', mute: 'refused', want: ['notify hidden', 'end lock', 'mute true', expect.stringContaining('log lifecycle: the mute') as string, 'leave'] },
    // No helper to mute: the status says idle while a join (before
    // Supervisor.start) or a retry from ended or lost may be in flight, and
    // leave overtakes it. Plain idle: leave is harmless.
    { name: 'idle, a join in flight or nothing at all: leave, no mute', state: 'idle', mute: 'ack', want: ['notify hidden', 'end lock', 'leave'] },
    { name: 'ended, a retry may be in flight: leave', state: 'ended', mute: 'ack', want: ['notify hidden', 'end lock', 'leave'] },
    { name: 'lost, a retry may be in flight: leave', state: 'lost', mute: 'ack', want: ['notify hidden', 'end lock', 'leave'] },
    { name: 'failed: leave', state: 'failed', mute: 'ack', want: ['notify hidden', 'end lock', 'leave'] },
    { name: 'unavailable: leave', state: 'unavailable', mute: 'ack', want: ['notify hidden', 'end lock', 'leave'] },
  ];
  it.each(table)('$name', async ({ state, mute, want }) => {
    const { life, calls } = observed(state, mute);
    await life.lockSignal('logind', true);
    expect(calls).toEqual(want);
    expect(life.locked).toBe(true);
  });

  it('leaves when the ack came but the view still shows unmuted at the bound (S4)', async () => {
    const { life, calls } = observed('running', 'ackUnseen');
    const done = life.lockSignal('logind', true);
    await vi.advanceTimersByTimeAsync(MUTE_BOUND_MS - 1);
    expect(calls).not.toContain('leave');
    await vi.advanceTimersByTimeAsync(1);
    await done;
    expect(calls).toContain('leave');
    expect(calls).not.toContain('lockMuted true');
  });

  it('leaves when the mute is not acknowledged within the contract bound', async () => {
    const { life, calls } = observed('running', 'hang');
    const done = life.lockSignal('logind', true);
    await vi.advanceTimersByTimeAsync(MUTE_BOUND_MS - 1);
    expect(calls).not.toContain('leave');
    await vi.advanceTimersByTimeAsync(1);
    await done;
    expect(calls).toContain('leave');
    expect(calls).not.toContain('lockMuted true');
  });

  it('a lock on top of a lock does nothing new, from the same or another source', async () => {
    const { life, calls } = observed('running');
    await life.lockSignal('logind', true);
    calls.length = 0;
    await life.lockSignal('logind', true);
    await life.lockSignal('idle', true);
    expect(calls).toEqual([]);
  });

  it('unlock shows text again and nothing else: no unmute, no share, no rejoin', async () => {
    const { life, calls } = observed('running');
    await life.lockSignal('logind', true);
    calls.length = 0;
    await life.lockSignal('logind', false);
    expect(calls).toEqual(['notify shown']);
    expect(life.locked).toBe(false);
  });

  it('is locked while either source says so', async () => {
    const { life, calls } = observed('running');
    await life.lockSignal('idle', true);
    calls.length = 0;
    await life.lockSignal('logind', true);
    await life.lockSignal('idle', false);
    expect(life.locked).toBe(true);
    expect(calls).toEqual([]);
    await life.lockSignal('logind', false);
    expect(calls).toEqual(['notify shown']);
  });

  it('a second lock after an unlock acts again', async () => {
    const { life, calls } = observed('running');
    for (const source of ['logind', 'logind'] as LockSource[]) {
      await life.lockSignal(source, true);
      await life.lockSignal(source, false);
    }
    expect(calls.filter((c) => c === 'mute true')).toHaveLength(2);
  });
});

describe('suspend', () => {
  it('leaves voice and ends the share, once however many signals say so while it acts', async () => {
    const { life, calls } = observed('running');
    const first = life.suspend();
    expect(life.suspended).toBe(true);
    await Promise.all([first, life.suspend(), life.suspend()]);
    expect(calls).toEqual(['leave', 'end suspend']);
  });

  it('clears the latch when the leave and the end settle, with no resume event (S3)', async () => {
    const { life, calls } = observed('running');
    await life.suspend();
    expect(life.suspended).toBe(false);
    calls.length = 0;
    // A suspend signal after a missed resume acts again; both actions are idempotent.
    await life.suspend();
    expect(calls).toEqual(['leave', 'end suspend']);
  });

  it('clears the latch when the actions fail too (S3)', async () => {
    const life = new Lifecycle({
      voice: { state: () => ({ status: 'running' }), setMute: () => Promise.resolve({ ok: true }), leave: () => Promise.reject(new Error('stuck')), setLockMuted: () => undefined },
      companion: { end: () => Promise.resolve(true) },
      notifications: { setLocked: () => undefined },
      log: () => undefined,
    });
    await life.suspend();
    expect(life.suspended).toBe(false);
  });

  it('resume does nothing to voice or the share, and a later suspend acts again', async () => {
    const { life, calls } = observed('running');
    await life.suspend();
    calls.length = 0;
    life.resume();
    expect(calls).toEqual([]);
    await life.suspend();
    expect(calls).toEqual(['leave', 'end suspend']);
  });

  it('still ends the share when leaving voice fails', async () => {
    const calls: string[] = [];
    const life = new Lifecycle({
      voice: { state: () => ({ status: 'running' }), setMute: () => Promise.resolve({ ok: true }), leave: () => Promise.reject(new Error('stuck')), setLockMuted: () => undefined },
      companion: { end: (reason) => { calls.push(`end ${reason}`); return Promise.resolve(true); } },
      notifications: { setLocked: () => undefined },
      log: (line) => calls.push(line),
    });
    await life.suspend();
    expect(calls).toContain('end suspend');
  });
});

describe('the lock cannot be observed (fail closed)', () => {
  it('hides notification text from the start, before anything observes the lock', () => {
    const calls: string[] = [];
    const life = new Lifecycle({
      voice: { state: () => ({ status: 'idle' }), setMute: () => Promise.resolve({ ok: true }), leave: () => Promise.resolve(), setLockMuted: () => undefined },
      companion: { end: () => Promise.resolve(true) },
      notifications: { setLocked: (l) => calls.push(String(l)) },
      log: () => undefined,
    });
    expect(life.lockObservation).toBe('unavailable');
    expect(calls).toEqual(['true']);
  });

  it('shows text only once the lock is observed and open, and hides it again when observation is lost', () => {
    const { life, calls } = rig('idle');
    life.observe('available');
    expect(calls).toEqual(['notify shown']);
    expect(life.lockObservation).toBe('available');
    life.observe('unavailable');
    expect(calls).toEqual(['notify shown', 'log lifecycle: the screen lock cannot be obs', 'notify hidden']);
    life.observe('unavailable');
    expect(calls).toHaveLength(3);
  });

  it('an unlock while the lock is not observed does not show text', async () => {
    const { life, calls } = rig('idle');
    await life.lockSignal('idle', true);
    calls.length = 0;
    await life.lockSignal('idle', false);
    expect(calls).toEqual([]);
  });
});

describe('dropSource (S-2)', () => {
  it('forgets a locked source that is gone: unlocked, no action, text hidden while unobserved', async () => {
    const r = observed('running');
    await r.life.lockSignal('logind', true);
    r.calls.length = 0;
    r.life.observe('unavailable');
    r.life.dropSource('logind');
    expect(r.life.locked).toBe(false);
    // Only the notification redaction (from observe); no mute, leave or end.
    expect(r.calls.filter((c) => !c.startsWith('log '))).toEqual([]);
    expect(r.life.lockObservation).toBe('unavailable');
  });

  it('another source that still says locked keeps the screen locked', async () => {
    const r = observed('running');
    await r.life.lockSignal('logind', true);
    await r.life.lockSignal('idle', true);
    r.life.dropSource('logind');
    expect(r.life.locked).toBe(true);
    r.life.dropSource('idle');
    expect(r.life.locked).toBe(false);
  });

  it('dropping a source that never spoke does nothing; a later lock acts again', async () => {
    const r = observed('running');
    r.life.dropSource('logind');
    expect(r.calls).toEqual([]);
    await r.life.lockSignal('logind', true);
    expect(r.calls).toContain('end lock');
  });
});
