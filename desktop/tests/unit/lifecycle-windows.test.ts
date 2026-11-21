// Windows lifecycle (#1935): fake powerMonitor and Tray, shared rules and
// settings, and a real owned child at Quit. No Electron, display or LiveKit.
// Real notification-area visibility, lock and PTT hardware are F14 (#903).
import { spawn, type ChildProcessWithoutNullStreams } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it, vi } from 'vitest';

import type { NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import { Lifecycle } from '../../main/lifecycle.ts';
import { WindowsLifecycle } from '../../main/lifecycle-windows.ts';
import { SettingsService } from '../../main/settings.ts';
import { trayHostPresent, type RunFile } from '../../main/tray-detect.ts';
import { TrayController, type TrayLike, type TrayMenuItem } from '../../main/tray.ts';
import { TeardownHooks } from '../../main/profiles/hooks.ts';
import { Switcher } from '../../main/profiles/switch.ts';
import { VoiceService } from '../../main/voice/service.ts';
import { Supervisor } from '../../main/voice/supervisor.ts';
import { WINDOWS_HELPER_NAME } from '../../main/voice/spawn.ts';
import { until } from './voice-support.ts';

class Power extends EventEmitter {
  state = 'active';
  getSystemIdleState(): string { return this.state; }
}

function powerRig(initial = 'active') {
  const power = new Power();
  power.state = initial;
  let muted = false;
  const voice = {
    state: () => ({ status: 'running', helper: { muted } }),
    setMute: vi.fn((value: boolean) => { muted = value; return Promise.resolve({ ok: true }); }),
    leave: vi.fn(() => Promise.resolve()),
    setLockMuted: vi.fn(),
  };
  const companion = { end: vi.fn(() => Promise.resolve()) };
  const notifications = { setLocked: vi.fn() };
  const rules = new Lifecycle({ voice, companion, notifications, log: () => undefined });
  const watcher = new WindowsLifecycle({ rules, powerMonitor: power });
  watcher.start();
  return { power, voice, companion, notifications, rules, watcher };
}

afterEach(() => { vi.useRealTimers(); });

describe('Windows powerMonitor uses the shared contract', () => {
  it('mutes on lock and before leaving on suspend, and keeps mute on unlock and resume', async () => {
    const r = powerRig();
    r.power.emit('lock-screen');
    expect(r.rules.locked).toBe(true);
    await vi.waitFor(() => { expect(r.voice.setLockMuted).toHaveBeenCalledWith(true); });
    expect(r.voice.setMute.mock.calls).toEqual([[true]]);
    expect(r.companion.end).toHaveBeenCalledWith('lock');
    expect(r.notifications.setLocked).toHaveBeenLastCalledWith(true);
    r.power.emit('unlock-screen');
    expect(r.rules.locked).toBe(false);
    expect(r.voice.state().helper.muted).toBe(true);
    expect(r.voice.setMute.mock.calls).toEqual([[true]]);
    expect(r.voice.leave).not.toHaveBeenCalled();
    r.power.emit('suspend');
    await vi.waitFor(() => { expect(r.voice.leave).toHaveBeenCalledOnce(); });
    expect(r.companion.end).toHaveBeenLastCalledWith('suspend');
    r.power.emit('resume');
    expect(r.voice.leave).toHaveBeenCalledOnce();
    expect(r.voice.setMute.mock.calls).toEqual([[true], [true]]);
    expect(r.companion.end).toHaveBeenCalledTimes(2);
    r.watcher.stop();
  });

  it('starts locked when Windows reports locked and installs listeners only once', async () => {
    const r = powerRig('locked');
    r.watcher.start();
    expect(r.rules.locked).toBe(true);
    expect(r.power.listenerCount('lock-screen')).toBe(1);
    await vi.waitFor(() => { expect(r.voice.setLockMuted).toHaveBeenCalledWith(true); });
    expect(r.voice.setMute.mock.calls).toEqual([[true]]);
    r.watcher.stop();
    r.watcher.stop();
    for (const event of ['lock-screen', 'unlock-screen', 'suspend', 'resume']) {
      expect(r.power.listenerCount(event)).toBe(0);
      r.power.emit(event);
    }
    expect(r.voice.leave).not.toHaveBeenCalled();
    expect(r.rules.lockObservation).toBe('unavailable');
  });

  it.each(['unknown', 'throws'])('keeps text hidden on an initial %s read until a lock event', (state) => {
    const r = powerRig(state);
    r.watcher.stop();
    if (state === 'throws') vi.spyOn(r.power, 'getSystemIdleState').mockImplementation(() => { throw new Error('unavailable'); });
    r.watcher.start();
    expect(r.rules.lockObservation).toBe('unavailable');
    expect(r.notifications.setLocked).toHaveBeenLastCalledWith(true);
    r.power.emit('unlock-screen');
    expect(r.rules.lockObservation).toBe('available');
    expect(r.notifications.setLocked).toHaveBeenLastCalledWith(false);
    expect(r.voice.setMute).not.toHaveBeenCalled();
    r.watcher.stop();
  });

  it('requires fresh lock evidence when the idle state is unknown on resume', () => {
    const r = powerRig();
    expect(r.rules.lockObservation).toBe('available');
    r.power.emit('suspend');
    r.power.state = 'unknown';
    r.power.emit('resume');
    expect(r.rules.sleeping).toBe(false);
    expect(r.rules.lockObservation).toBe('unavailable');
    r.power.emit('unlock-screen');
    expect(r.rules.lockObservation).toBe('available');
    r.watcher.stop();
  });

  it('uses the shared fail-closed leave when the mute is refused', async () => {
    const r = powerRig();
    r.voice.setMute.mockResolvedValue({ ok: false });
    r.power.emit('lock-screen');
    await vi.waitFor(() => { expect(r.voice.leave).toHaveBeenCalledOnce(); });
    r.watcher.stop();
  });
});

class FakeTray implements TrayLike {
  menu: readonly TrayMenuItem[] = [];
  setImage(): void {}
  setToolTip(): void {}
  setContextMenu(menu: unknown): void { this.menu = menu as readonly TrayMenuItem[]; }
  on(): void {}
  destroy(): void {}
}

function trayRig(teardown?: () => Promise<void>) {
  const icon = new FakeTray();
  const state = { available: true, createFails: false, hidden: false };
  // Feed tasklist output through the production Windows detection path, both
  // for settings and the controller's periodic/close-time checks.
  const run: RunFile = (_file, args) => Promise.resolve({
    code: 0,
    stdout: args.includes('IMAGENAME eq explorer.exe')
      ? state.available ? '"explorer.exe","4321","Console","2","123 K"' : 'INFO: No tasks are running which match the specified criteria.'
      : `"Conch.exe","${String(process.pid)}","Console","2","12 K"`,
  });
  const detect = () => trayHostPresent(run, { SystemRoot: 'C:\\Windows' }, 'win32');
  let tick = (): void => undefined;
  let view: NativeVoiceView = {
    status: 'running', generation: 1, channel: 'general', retryable: false,
    helper: { version: 2, phase: 'connected', capture: 'on', playback: 'on', transmitting: true, muted: false, deafened: false, blocked: [], ptt: 'ready' },
  };
  const voice = {
    state: () => view,
    subscribe: () => () => undefined,
    setMute: vi.fn(() => Promise.resolve({ ok: true as const })),
    setDeafen: () => Promise.resolve({ ok: true as const }),
    leave: vi.fn(() => {
      view = { status: 'idle', generation: 1, channel: null, retryable: false };
      return Promise.resolve(view);
    }),
  };
  const hooks = new TeardownHooks();
  hooks.media('voice', async () => { await voice.leave(); await teardown?.(); });
  const switcher = new Switcher({ hooks, open: () => { throw new Error('Quit must never activate'); }, now: Date.now, log: () => undefined });
  let ending: Promise<unknown> | undefined;
  const settings: SettingsService = new SettingsService({
    files: { read: () => Promise.resolve({ kind: 'absent' }), write: () => Promise.resolve() },
    autostart: { availability: () => Promise.resolve(null), enable: () => Promise.resolve({ ok: true }), disable: () => Promise.resolve({ ok: true }) },
    trayDetected: detect,
    trayWorking: () => tray.working(), trayExists: () => tray.exists, log: () => undefined,
  });
  const tray: TrayController = new TrayController({
    createTray: () => { if (state.createFails) throw new Error('unavailable'); return icon; },
    buildMenu: (items) => items, iconDir: 'icons', voice,
    sharing: () => false, endCompanion: () => Promise.resolve(true), profile: () => 'Home',
    show: () => { state.hidden = false; }, hidden: () => state.hidden,
    quit: () => { ending = switcher.close({ reason: 'quit' }); },
    detect, revoke: () => { settings.revokeTrayConfirmation(); },
    settings: () => settings.current, confirmed: () => settings.trayConfirmed,
    every: (fn) => { tick = fn; return () => undefined; }, log: () => undefined,
  });
  const close = async (): Promise<void> => {
    await tray.check();
    if (settings.closeAction(false) === 'stay') state.hidden = true;
    else await switcher.close({ reason: 'quit' });
  };
  return { tray, settings, state, voice, icon, close, tick: () => { tick(); }, ending: () => ending };
}

describe('Windows close and tray reuse the shared policy', () => {
  it('restores a confirmed hidden call when Explorer disappears at a periodic check', async () => {
    const r = trayRig();
    expect((await r.settings.set({ closeToTray: true })).ok).toBe(true);
    await r.tray.sync();
    expect((await r.settings.set({ trayConfirmed: true })).ok).toBe(true);
    await r.close();
    expect(r.state.hidden).toBe(true);
    expect(r.tray.working()).toBe(true);
    r.state.available = false;
    r.tick();
    await vi.waitFor(() => { expect(r.state.hidden).toBe(false); });
    expect(r.settings.trayConfirmed).toBe(false);
    expect(r.tray.working()).toBe(false);
    expect(r.voice.setMute).toHaveBeenCalledWith(true);
    r.state.available = true;
    await r.tray.check();
    expect(r.tray.working()).toBe(false); // Recovery requires fresh confirmation.
  });

  it('close quits and stops media by default', async () => {
    const r = trayRig();
    await r.tray.sync();
    await r.close();
    expect(r.state.hidden).toBe(false);
    expect(r.tray.exists).toBe(false);
    expect(r.voice.leave).toHaveBeenCalledOnce();
  });

  it('hides only after opt-in and confirmation, and shows only acknowledged voice state', async () => {
    const r = trayRig();
    expect((await r.settings.set({ closeToTray: true })).ok).toBe(true);
    await r.tray.sync();
    expect(r.settings.closeAction(false)).toBe('quit');
    expect((await r.settings.set({ trayConfirmed: true })).ok).toBe(true);
    await r.close();
    expect(r.state.hidden).toBe(true);
    expect(r.voice.leave).not.toHaveBeenCalled();
    expect(r.icon.menu.map((i) => i.label)).toContain('Transmitting');
    r.icon.menu.find((i) => i.label === 'Mute')?.click?.();
    await vi.waitFor(() => { expect(r.voice.setMute).toHaveBeenCalledOnce(); });
    expect(r.icon.menu.map((i) => i.label)).toContain('Transmitting');
    expect(r.icon.menu.map((i) => i.label)).not.toContain('Muted');
    r.icon.menu.find((i) => i.label === 'Quit')?.click?.();
    await r.ending();
    expect(r.voice.leave).toHaveBeenCalledOnce();
  });

  it.each(['no host', 'creation fails', 'host lost'])('never hides when the tray is unavailable: %s', async (failure) => {
    const r = trayRig();
    if (failure === 'no host') r.state.available = false;
    const result = await r.settings.set({ closeToTray: true });
    expect(result.ok).toBe(failure !== 'no host');
    r.state.createFails = failure === 'creation fails';
    await r.tray.sync();
    if (failure === 'host lost') {
      await r.settings.set({ trayConfirmed: true });
      r.state.available = false;
    }
    await r.close();
    expect(r.state.hidden).toBe(false);
    expect(r.voice.leave).toHaveBeenCalledOnce();
  });

  it('Quit reaps a real hung owned helper process, including a join still starting', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'conch-quit-'));
    const resources = join(dir, 'resources');
    mkdirSync(resources);
    writeFileSync(join(resources, WINDOWS_HELPER_NAME), 'test executable placeholder');
    let child: ChildProcessWithoutNullStreams | undefined;
    const supervisor = new Supervisor({
      resources, platform: 'win32', env: { SystemRoot: 'C:\\Windows' }, log: () => undefined,
      // This helper never attaches: stub only binding, so the real process
      // teardown needs no socket permission. Pipe I/O is voice-windows.test.ts.
      endpoint: { listen: () => Promise.resolve() },
      spawn: () => {
        child = spawn(process.execPath, ['-e', 'process.stdin.resume(); setInterval(() => {}, 1000)'], { stdio: ['pipe', 'pipe', 'pipe'] });
        return child;
      },
      timings: { helloTimeoutMs: 5000, killAfterMs: 100, termAfterMs: 50 },
    });
    const voice = new VoiceService({
      supervisor, locked: () => false, log: () => undefined,
      identity: () => Promise.resolve({ profileId: 'p1', configDir: 'C:\\Conch\\profiles\\p1', principalId: 7, credential: () => ({ origin: 'https://example.org', token: 'test' }), current: () => true }),
      oneShot: { cancel: () => Promise.resolve(), devices: () => Promise.resolve({ ok: false, error: 'not-installed' }), save: () => Promise.resolve({ ok: false, error: 'not-installed' }), saveMuteBinding: () => Promise.resolve({ ok: false, error: 'not-installed' }) },
    });
    try {
      const joining = voice.join({ channel: 'general', muted: false, deafened: false });
      await until(() => child?.pid !== undefined || voice.state().status === 'unavailable' || voice.state().status === 'failed', 3000, 'helper spawn');
      expect(voice.state().problem).toBeUndefined();
      expect(voice.state().status).toBe('starting');
      if (child?.pid === undefined) throw new Error('helper did not spawn');
      const owned = child;
      const pid = child.pid;
      expect(() => process.kill(pid, 0)).not.toThrow();
      const r = trayRig(() => voice.teardown());
      await r.settings.set({ closeToTray: true });
      await r.tray.sync();
      r.icon.menu.find((i) => i.label === 'Quit')?.click?.();
      await r.ending();
      await joining;
      expect(owned.exitCode !== null || owned.signalCode !== null).toBe(true);
      expect(() => process.kill(pid, 0)).toThrow();
      expect(voice.state().status).toBe('idle');
    } finally {
      await supervisor.stop();
      child?.kill();
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
