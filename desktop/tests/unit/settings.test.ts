import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync, readdirSync, mkdirSync, symlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { Autostart, entryText, type AutostartResult } from '../../main/autostart.ts';
import { closeAction, createSettingsFiles, DEFAULT_SETTINGS, DETECT_CACHE_MS, parseSettings, SettingsService, type SettingsFiles } from '../../main/settings.ts';

const dirs: string[] = [];
function tmp(): string {
  const dir = mkdtempSync(join(tmpdir(), 'conch-settings-'));
  dirs.push(dir);
  return dir;
}
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

describe('the close rule', () => {
  it.each([
    // destroying, closeToTray, trayWorking, expected
    [false, false, false, 'quit'],
    [false, true, false, 'quit'],
    [false, false, true, 'quit'],
    [false, true, true, 'stay'],
    [true, false, false, 'stay'],
    [true, true, true, 'stay'],
  ] as const)('destroying=%s closeToTray=%s trayWorking=%s -> %s', (destroying, closeToTray, trayWorking, want) => {
    expect(closeAction({ destroying, closeToTray, trayWorking })).toBe(want);
  });

  it('with the seam main passes today (no tray), the service always quits, even with closeToTray on', async () => {
    const service = new SettingsService({ files: memory(), autostart: fakeAutostart(), trayWorking: () => false, log: () => undefined });
    await service.set({ closeToTray: true });
    expect(service.current.closeToTray).toBe(true);
    expect(service.closeAction(false)).toBe('quit');
    expect((await service.view()).trayWorking).toBe(false);
  });

  it('keeps running when the person chose it and a fake tray works', async () => {
    const tray = { working: false };
    const service = new SettingsService({ files: memory(), autostart: fakeAutostart(), trayWorking: () => tray.working, log: () => undefined });
    await service.set({ closeToTray: true });
    expect(service.closeAction(false)).toBe('quit');
    tray.working = true;
    expect(service.closeAction(false)).toBe('stay');
    await service.set({ closeToTray: false });
    expect(service.closeAction(false)).toBe('quit');
  });
});

describe('parseSettings', () => {
  it('reads the two booleans and drops unknown keys', () => {
    expect(parseSettings('{"closeToTray":true,"launchAtLogin":true,"theme":"x"}')).toEqual({ closeToTray: true, launchAtLogin: true });
  });
  it('takes the default for a key that is absent or not a boolean', () => {
    expect(parseSettings('{"closeToTray":"yes"}')).toEqual(DEFAULT_SETTINGS);
    expect(parseSettings('{"launchAtLogin":true}')).toEqual({ closeToTray: false, launchAtLogin: true });
  });
  it.each(['', 'not json', '[]', 'null', '5', '"x"'])('is null (malformed) for %j', (text) => {
    expect(parseSettings(text)).toBeNull();
  });
});

describe('settings.json', () => {
  it('round-trips, is 0600, and leaves no temporary file', async () => {
    const root = tmp();
    const service = new SettingsService({ files: createSettingsFiles(root), autostart: fakeAutostart(), trayWorking: () => false, log: () => undefined });
    await service.load();
    expect(service.current).toEqual(DEFAULT_SETTINGS);
    const result = await service.set({ closeToTray: true, launchAtLogin: true });
    expect(result.ok).toBe(true);
    expect(statSync(join(root, 'settings.json')).mode & 0o777).toBe(0o600);
    expect(readdirSync(root)).toEqual(['settings.json']);
    const again = new SettingsService({ files: createSettingsFiles(root), autostart: fakeAutostart(), trayWorking: () => false, log: () => undefined });
    await again.load();
    expect(again.current).toEqual({ closeToTray: true, launchAtLogin: true });
  });

  it('replaces atomically: an existing file with a loose mode is rewritten 0600', async () => {
    const root = tmp();
    writeFileSync(join(root, 'settings.json'), '{"closeToTray":false}', { mode: 0o644 });
    await createSettingsFiles(root).write('{"closeToTray":true}\n');
    expect(statSync(join(root, 'settings.json')).mode & 0o777).toBe(0o600);
    expect(readFileSync(join(root, 'settings.json'), 'utf8')).toBe('{"closeToTray":true}\n');
  });

  it.each([
    ['missing', undefined, false],
    ['malformed', '{{{', true],
    ['not an object', '[1]', true],
    ['too large', 'x'.repeat(10_000), true],
  ])('applies the defaults for a %s file (logged unless missing)', async (_name, text, logged) => {
    const root = tmp();
    if (text !== undefined) writeFileSync(join(root, 'settings.json'), text);
    const logs: string[] = [];
    const service = new SettingsService({ files: createSettingsFiles(root), autostart: fakeAutostart(), trayWorking: () => false, log: (l) => logs.push(l) });
    await service.load();
    expect(service.current).toEqual(DEFAULT_SETTINGS);
    expect(logs.length > 0).toBe(logged);
  });

  it('applies the defaults for a file that cannot be read (a directory, a symlink)', async () => {
    const root = tmp();
    mkdirSync(join(root, 'settings.json'));
    const service = new SettingsService({ files: createSettingsFiles(root), autostart: fakeAutostart(), trayWorking: () => false, log: () => undefined });
    await service.load();
    expect(service.current).toEqual(DEFAULT_SETTINGS);
    const other = tmp();
    writeFileSync(join(other, 'real'), '{"closeToTray":true}');
    symlinkSync(join(other, 'real'), join(other, 'settings.json'));
    const linked = new SettingsService({ files: createSettingsFiles(other), autostart: fakeAutostart(), trayWorking: () => false, log: () => undefined });
    await linked.load();
    expect(linked.current).toEqual(DEFAULT_SETTINGS);
  });
});

describe('changing launch at login', () => {
  it('enables the entry before storing, and stores nothing if it is refused', async () => {
    const files = memory();
    const auto = fakeAutostart({ enable: { ok: false, reason: 'unpackaged' } });
    const service = new SettingsService({ files, autostart: auto, trayWorking: () => false, log: () => undefined });
    expect(await service.set({ launchAtLogin: true })).toEqual({ ok: false, reason: 'unpackaged' });
    expect(service.current.launchAtLogin).toBe(false);
    expect(files.text).toBeNull();
  });

  it('does not touch the entry for a change that is not launchAtLogin', async () => {
    const auto = fakeAutostart();
    const service = new SettingsService({ files: memory(), autostart: auto, trayWorking: () => false, log: () => undefined });
    await service.set({ closeToTray: true });
    expect(auto.calls).toEqual([]);
  });

  it('turning it off over a file Conch did not write still stores off', async () => {
    const auto = fakeAutostart({ disable: { ok: false, reason: 'foreign-file' } });
    const service = new SettingsService({ files: memory('{"launchAtLogin":true}'), autostart: auto, trayWorking: () => false, log: () => undefined });
    await service.load();
    expect((await service.set({ launchAtLogin: false })).ok).toBe(true);
    expect(service.current.launchAtLogin).toBe(false);
  });

  it('puts the entry back when the file cannot be written', async () => {
    const files = memory();
    files.failWrites = true;
    const auto = fakeAutostart();
    const service = new SettingsService({ files, autostart: auto, trayWorking: () => false, log: () => undefined });
    expect(await service.set({ launchAtLogin: true })).toEqual({ ok: false, reason: 'failed' });
    expect(auto.calls).toEqual(['enable', 'disable']);
    expect(service.current.launchAtLogin).toBe(false);
  });
});

describe('start-up re-judges an existing launch-at-login entry', () => {
  // A good 0755 tree <top>/app/conch, a config dir, and the real Autostart.
  function setup() {
    const top = tmp();
    chmodSync(top, 0o755);
    mkdirSync(join(top, 'app'), { mode: 0o755 });
    chmodSync(join(top, 'app'), 0o755);
    const exec = join(top, 'app', 'conch');
    writeFileSync(exec, '#!/bin/sh\n', { mode: 0o755 });
    const config = tmp();
    const logs: string[] = [];
    const autostart = new Autostart({ packaged: true, execPath: exec, trustedAbove: top, env: { XDG_CONFIG_HOME: config }, home: '/nonexistent-home', log: (l) => logs.push(l) });
    const file = join(config, 'autostart', 'chat.conch.desktop.desktop');
    mkdirSync(join(config, 'autostart'));
    const files = memory('{"closeToTray":false,"launchAtLogin":true}');
    const service = new SettingsService({ files, autostart, trayWorking: () => false, log: (l) => logs.push(l) });
    return { top, exec, file, files, logs, service };
  }

  it('removes its own entry from a loosened install and reads off', async () => {
    const t = setup();
    writeFileSync(t.file, entryText(t.exec) ?? '');
    await t.service.load();
    chmodSync(join(t.top, 'app'), 0o777);
    await t.service.rejudge();
    expect(existsSync(t.file)).toBe(false);
    expect(t.service.current.launchAtLogin).toBe(false);
    expect(t.files.text).toContain('"launchAtLogin": false');
    expect(t.logs.join('\n')).toMatch(/no longer sits where only you or the system can change it/);
    const view = await t.service.view();
    expect(view.launchAtLogin).toBe(false);
    expect(view.launchAvailable).toBe(false);
    expect(view.launchUnavailable).toBe('unsafe-location');
  });

  it('never touches a foreign file, though the setting reads off', async () => {
    const t = setup();
    writeFileSync(t.file, '[Desktop Entry]\nName=Someone else\n');
    await t.service.load();
    chmodSync(join(t.top, 'app'), 0o777);
    await t.service.rejudge();
    expect(readFileSync(t.file, 'utf8')).toBe('[Desktop Entry]\nName=Someone else\n');
    expect(t.service.current.launchAtLogin).toBe(false);
  });

  it('leaves a safe install, and a setting that is off, alone', async () => {
    const t = setup();
    writeFileSync(t.file, entryText(t.exec) ?? '');
    await t.service.load();
    await t.service.rejudge();
    expect(existsSync(t.file)).toBe(true);
    expect(t.service.current.launchAtLogin).toBe(true);
    expect(t.logs).toEqual([]);
    const off = new SettingsService({ files: memory('{"launchAtLogin":false}'), autostart: fakeAutostart(), trayWorking: () => false, log: () => undefined });
    await off.load();
    await off.rejudge();
    expect(off.current.launchAtLogin).toBe(false);
  });

  it('keeps the setting on when its entry cannot be removed', async () => {
    const files = memory('{"launchAtLogin":true}');
    const logs: string[] = [];
    const autostart = { ...fakeAutostart({ disable: { ok: false, reason: 'failed' } }), availability: () => Promise.resolve('unsafe-location' as const) };
    const service = new SettingsService({ files, autostart, trayWorking: () => false, log: (l) => logs.push(l) });
    await service.load();
    await service.rejudge();
    expect(service.current.launchAtLogin).toBe(true);
  });
});

describe('opening at login starts nothing', () => {
  it('app.ts starts no helper or capture on its own: voice is only joined over IPC', () => {
    const text = readFileSync(join(__dirname, '../../main/app.ts'), 'utf8')
      .replace(/\/\*[\s\S]*?\*\//g, '')
      .replace(/^\s*\/\/.*$/gm, '');
    expect(text).not.toMatch(/\bvoice\.(join|retry|setMute|setDeafen|setDevices|setTalkMode|devices|keyDevices)\b/);
    expect(text).not.toMatch(/\bsupervisor\.(start|join)\b/);
  });

  it('the autostart entry has exactly one Exec line, the quoted path alone', () => {
    const exec = (entryText('/opt/My App/conch') ?? '').split('\n').filter((line) => line.startsWith('Exec='));
    expect(exec).toHaveLength(1);
    expect(exec[0]).toMatch(/^Exec="[^"\n]*"$/);
  });
});

describe('turning the tray on (Q-D5-2)', () => {
  function tray(detected: { value: boolean }, exists: { value: boolean } = { value: true }, clock: { t: number } = { t: 0 }) {
    const changes: boolean[] = [];
    const asked = { n: 0 };
    const service = new SettingsService({
      files: memory(),
      autostart: fakeAutostart(),
      trayWorking: () => false,
      trayExists: () => exists.value,
      trayDetected: () => {
        asked.n++;
        return Promise.resolve(detected.value);
      },
      changed: (s) => changes.push(s.closeToTray),
      now: () => clock.t,
      log: () => undefined,
    });
    return { service, changes, asked };
  }

  it('is refused while no tray host is on the bus, and stores nothing', async () => {
    const { service, changes } = tray({ value: false });
    expect(await service.set({ closeToTray: true })).toEqual({ ok: false, reason: 'no-tray' });
    expect(service.current.closeToTray).toBe(false);
    expect(changes).toEqual([]);
  });

  it('a refused no-tray is not followed by a stale trayDetected: true from the cache', async () => {
    const detected = { value: true };
    const { service } = tray(detected);
    expect((await service.view()).trayDetected).toBe(true);
    detected.value = false;
    expect(await service.set({ closeToTray: true })).toEqual({ ok: false, reason: 'no-tray' });
    expect((await service.view()).trayDetected).toBe(false);
  });

  it('is accepted with a host, unconfirmed until the person says yes', async () => {
    const { service, changes } = tray({ value: true });
    const on = await service.set({ closeToTray: true });
    expect(on.ok && on.view.trayConfirmed).toBe(false);
    expect(on.ok && on.view.trayDetected).toBe(true);
    const yes = await service.set({ trayConfirmed: true });
    expect(yes.ok && yes.view.trayConfirmed).toBe(true);
    expect(changes).toEqual([true, true]);
  });

  it('cannot be confirmed with the tray off, or without a host', async () => {
    const detected = { value: true };
    const { service } = tray(detected);
    expect(await service.set({ trayConfirmed: true })).toEqual({ ok: false, reason: 'no-tray' });
    await service.set({ closeToTray: true });
    detected.value = false;
    expect(await service.set({ trayConfirmed: true })).toEqual({ ok: false, reason: 'no-tray' });
    expect(service.trayConfirmed).toBe(false);
  });

  it('cannot be turned on and confirmed in one call, whatever the values', async () => {
    const { service, changes } = tray({ value: true });
    expect(await service.set({ closeToTray: true, trayConfirmed: true })).toEqual({ ok: false, reason: 'no-tray' });
    expect(await service.set({ closeToTray: true, trayConfirmed: false })).toEqual({ ok: false, reason: 'no-tray' });
    expect(service.current.closeToTray).toBe(false);
    expect(service.trayConfirmed).toBe(false);
    expect(changes).toEqual([]);
    // Already on, a combined change is still refused.
    await service.set({ closeToTray: true });
    expect(await service.set({ closeToTray: true, trayConfirmed: true })).toEqual({ ok: false, reason: 'no-tray' });
    expect(service.trayConfirmed).toBe(false);
  });

  it('cannot be confirmed while the tray icon does not exist', async () => {
    const exists = { value: false };
    const { service } = tray({ value: true }, exists);
    await service.set({ closeToTray: true });
    expect(await service.set({ trayConfirmed: true })).toEqual({ ok: false, reason: 'no-tray' });
    expect(service.trayConfirmed).toBe(false);
    exists.value = true;
    expect((await service.set({ trayConfirmed: true })).ok).toBe(true);
    expect(service.trayConfirmed).toBe(true);
  });

  it('cannot be confirmed when the service is not told whether the icon exists', async () => {
    const service = new SettingsService({ files: memory(), autostart: fakeAutostart(), trayWorking: () => false, trayDetected: () => Promise.resolve(true), log: () => undefined });
    await service.set({ closeToTray: true });
    expect(await service.set({ trayConfirmed: true })).toEqual({ ok: false, reason: 'no-tray' });
  });

  it('reads of the view reuse the detector for a few seconds, a change always asks', async () => {
    const clock = { t: 1_000 };
    const { service, asked } = tray({ value: true }, { value: true }, clock);
    await service.view();
    await service.view();
    expect(asked.n).toBe(1);
    clock.t += DETECT_CACHE_MS;
    await service.view();
    expect(asked.n).toBe(2);
    await service.set({ closeToTray: true });
    expect(asked.n).toBeGreaterThan(2);
  });

  it('turning it off, or losing the host, asks again', async () => {
    const { service } = tray({ value: true });
    await service.set({ closeToTray: true });
    await service.set({ trayConfirmed: true });
    service.revokeTrayConfirmation();
    expect(service.trayConfirmed).toBe(false);
    await service.set({ trayConfirmed: true });
    await service.set({ closeToTray: false });
    expect(service.trayConfirmed).toBe(false);
  });
});

interface MemoryFiles extends SettingsFiles {
  text: string | null;
  failWrites: boolean;
}

function memory(initial?: string): MemoryFiles {
  const files: MemoryFiles = {
    text: initial ?? null,
    failWrites: false,
    read: () => Promise.resolve(files.text === null ? { kind: 'absent' } : { kind: 'text', text: files.text }),
    write: (text) => {
      if (files.failWrites) return Promise.reject(new Error('disk'));
      files.text = text;
      return Promise.resolve();
    },
  };
  return files;
}

function fakeAutostart(results: { enable?: AutostartResult; disable?: AutostartResult } = {}) {
  const calls: string[] = [];
  const ok: AutostartResult = { ok: true };
  return {
    calls,
    availability: () => Promise.resolve(null),
    enable: () => {
      calls.push('enable');
      return Promise.resolve(results.enable ?? ok);
    },
    disable: () => {
      calls.push('disable');
      return Promise.resolve(results.disable ?? ok);
    },
  };
}
