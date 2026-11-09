import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { Autostart, autostartPath, carriesMarker, entryText, execValue } from '../../main/autostart.ts';

const dirs: string[] = [];
function tmp(): string {
  const dir = mkdtempSync(join(tmpdir(), 'conch-autostart-'));
  dirs.push(dir);
  return dir;
}
afterEach(() => {
  for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

const EXEC = '/opt/Conch/conch-desktop';

function make(config: string, over: { packaged?: boolean; execPath?: string } = {}) {
  const logs: string[] = [];
  const auto = new Autostart({
    packaged: over.packaged ?? true,
    execPath: over.execPath ?? EXEC,
    env: { XDG_CONFIG_HOME: config },
    home: '/nonexistent-home',
    log: (l) => logs.push(l),
  });
  return { auto, logs, file: join(config, 'autostart', 'chat.conch.desktop.desktop') };
}

describe('where the entry goes', () => {
  it('is $XDG_CONFIG_HOME/autostart/<app id>.desktop', () => {
    expect(autostartPath({ XDG_CONFIG_HOME: '/x/cfg' }, '/home/a')).toBe('/x/cfg/autostart/chat.conch.desktop.desktop');
  });
  it.each([[undefined], [''], ['relative/dir']])('falls back to ~/.config for XDG_CONFIG_HOME=%j', (value) => {
    expect(autostartPath({ XDG_CONFIG_HOME: value }, '/home/a')).toBe('/home/a/.config/autostart/chat.conch.desktop.desktop');
  });
  it('uses the app id of the builder config', () => {
    const config = JSON.parse(readFileSync(join(__dirname, '../../build/electron-builder.config.json'), 'utf8')) as { appId: string };
    expect(autostartPath({}, '/h')).toContain(`/${config.appId}.desktop`);
  });
});

describe('the entry', () => {
  it('has the packaged Exec path alone and the marker', () => {
    const text = entryText(EXEC) ?? '';
    expect(text).toContain(`Exec="${EXEC}"\n`);
    expect(text).toContain('X-Conch-Autostart=true\n');
    expect(text.startsWith('[Desktop Entry]\n')).toBe(true);
    expect(carriesMarker(text)).toBe(true);
  });
  it.each([
    ['/opt/My App/conch', '"/opt/My App/conch"'],
    ['/opt/a"b/c', '"/opt/a\\\\"b/c"'],
    ['/opt/50%/c', '"/opt/50%%/c"'],
    ['/opt/$x/c', '"/opt/\\\\$x/c"'],
  ])('quotes %s', (path, want) => {
    expect(execValue(path)).toBe(want);
  });
  it.each(['relative/conch', '/opt/a\nExec=evil', '/opt/a\u0000b', ''])('refuses the path %j', (path) => {
    expect(execValue(path)).toBeNull();
    expect(entryText(path)).toBeNull();
  });
  it('a marker must be a line of its own', () => {
    expect(carriesMarker('Name=x\nComment=X-Conch-Autostart=true\n')).toBe(false);
    expect(carriesMarker('[Desktop Entry]\nX-Conch-Autostart=false\n')).toBe(false);
  });
});

describe('turning it on and off', () => {
  it('writes exactly one file, then removes it, in a temporary config home', async () => {
    const config = tmp();
    const { auto, file } = make(config);
    expect(await auto.enable()).toEqual({ ok: true });
    expect(readdirSync(join(config, 'autostart'))).toEqual(['chat.conch.desktop.desktop']);
    expect(readFileSync(file, 'utf8')).toBe(entryText(EXEC));
    // Turning it on again replaces our own file, still one.
    expect(await auto.enable()).toEqual({ ok: true });
    expect(readdirSync(join(config, 'autostart'))).toEqual(['chat.conch.desktop.desktop']);
    expect(await auto.disable()).toEqual({ ok: true });
    expect(existsSync(file)).toBe(false);
    // Off again is not an error.
    expect(await auto.disable()).toEqual({ ok: true });
  });

  it('refuses an unpackaged build, with a reason, and writes nothing', async () => {
    const config = tmp();
    const { auto, logs } = make(config, { packaged: false });
    expect(auto.available()).toBe(false);
    expect(await auto.enable()).toEqual({ ok: false, reason: 'unpackaged' });
    expect(readdirSync(config)).toEqual([]);
    expect(logs.join('\n')).toMatch(/unpackaged/);
  });

  it('never overwrites or removes a file Conch did not write', async () => {
    const config = tmp();
    const { auto, file } = make(config);
    mkdirSync(join(config, 'autostart'));
    writeFileSync(file, '[Desktop Entry]\nName=Mine\nExec=/usr/bin/other\n');
    expect(await auto.enable()).toEqual({ ok: false, reason: 'foreign-file' });
    expect(readFileSync(file, 'utf8')).toContain('Name=Mine');
    expect(await auto.disable()).toEqual({ ok: false, reason: 'foreign-file' });
    expect(readFileSync(file, 'utf8')).toContain('Name=Mine');
    expect(readdirSync(join(config, 'autostart'))).toEqual(['chat.conch.desktop.desktop']);
  });

  it('refuses a path it cannot write safely', async () => {
    const config = tmp();
    const { auto } = make(config, { execPath: 'relative' });
    expect(auto.available()).toBe(false);
    expect(await auto.enable()).toEqual({ ok: false, reason: 'failed' });
  });
});
