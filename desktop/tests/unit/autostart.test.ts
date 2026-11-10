import { chmodSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, realpathSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
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

// The tree a build is unpacked into: <tmp>/app/conch, 0755 all the way down
// from the temporary root (which is not judged: see trustedTarget's seam).
function tree(): { readonly top: string; readonly exec: string } {
  const top = tmp();
  chmodSync(top, 0o755);
  mkdirSync(join(top, 'app'), { mode: 0o755 });
  chmodSync(join(top, 'app'), 0o755);
  const exec = join(top, 'app', 'conch');
  writeFileSync(exec, '#!/bin/sh\n', { mode: 0o755 });
  return { top, exec };
}
const EXEC = '/opt/Conch/conch-desktop';

function make(config: string, over: { packaged?: boolean; execPath?: string; above?: string } = {}) {
  const logs: string[] = [];
  const target = over.execPath === undefined ? tree() : null;
  const auto = new Autostart({
    packaged: over.packaged ?? true,
    execPath: over.execPath ?? target?.exec ?? EXEC,
    ...(over.execPath === undefined ? { trustedAbove: target?.top } : over.above === undefined ? {} : { trustedAbove: over.above }),
    env: { XDG_CONFIG_HOME: config },
    home: '/nonexistent-home',
    log: (l) => logs.push(l),
  });
  return { auto, logs, file: join(config, 'autostart', 'chat.conch.desktop.desktop'), exec: target?.exec ?? EXEC };
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
    const { auto, file, exec } = make(config);
    expect(await auto.enable()).toEqual({ ok: true });
    expect(readdirSync(join(config, 'autostart'))).toEqual(['chat.conch.desktop.desktop']);
    expect(readFileSync(file, 'utf8')).toBe(entryText(exec));
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
    expect(await auto.availability()).toBe('unpackaged');
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
    expect(await auto.availability()).toBe('unsafe-location');
    expect(await auto.enable()).toEqual({ ok: false, reason: 'unsafe-location' });
  });
});

describe('where the executable may sit', () => {
  it('allows a 0755 tree owned by the user', async () => {
    const config = tmp();
    const { auto } = make(config);
    expect(await auto.availability()).toBeNull();
  });

  it.each([
    ['world-writable', 0o777],
    ['group-writable', 0o775],
  ])('refuses a %s ancestor, writes nothing, and says why', async (_name, mode) => {
    const config = tmp();
    const { top, exec } = tree();
    chmodSync(join(top, 'app'), mode);
    const { auto, logs } = make(config, { execPath: exec, above: top });
    expect(await auto.availability()).toBe('unsafe-location');
    expect(await auto.enable()).toEqual({ ok: false, reason: 'unsafe-location' });
    expect(existsSync(join(config, 'autostart'))).toBe(false);
    expect(logs.join('\n')).toMatch(/only you or the system/);
  });

  it('gives a sticky, world-writable ancestor no allowance', async () => {
    const config = tmp();
    const { top, exec } = tree();
    chmodSync(join(top, 'app'), 0o1777);
    const { auto } = make(config, { execPath: exec, above: top });
    expect(await auto.availability()).toBe('unsafe-location');
  });

  it('judges the real path: a symlinked execPath resolves first', async () => {
    const config = tmp();
    const { top, exec } = tree();
    // A safe-looking link into a world-writable directory.
    const bad = join(top, 'bad');
    mkdirSync(bad);
    chmodSync(bad, 0o777);
    writeFileSync(join(bad, 'conch'), '#!/bin/sh\n', { mode: 0o755 });
    const link = join(top, 'app', 'link');
    symlinkSync(join(bad, 'conch'), link);
    expect(await make(config, { execPath: link, above: top }).auto.availability()).toBe('unsafe-location');
    const good = join(top, 'app', 'link2');
    symlinkSync(exec, good);
    expect(await make(config, { execPath: good, above: top }).auto.availability()).toBeNull();
  });

  it('refuses an executable that group or others can write', async () => {
    const config = tmp();
    const { top, exec } = tree();
    chmodSync(exec, 0o775);
    expect(await make(config, { execPath: exec, above: top }).auto.availability()).toBe('unsafe-location');
    chmodSync(exec, 0o757);
    expect(await make(config, { execPath: exec, above: top }).auto.availability()).toBe('unsafe-location');
    expect(existsSync(join(config, 'autostart'))).toBe(false);
  });

  it('names the real path in Exec=, never the symlink that was given', async () => {
    const config = tmp();
    const { top, exec } = tree();
    const link = join(top, 'app', 'link');
    symlinkSync(exec, link);
    const { auto, file } = make(config, { execPath: link, above: top });
    expect(await auto.enable()).toEqual({ ok: true });
    expect(readFileSync(file, 'utf8')).toBe(entryText(realpathSync(exec)));
    expect(readFileSync(file, 'utf8')).not.toContain(link);
  });

  it('refuses a missing file and a directory', async () => {
    const config = tmp();
    const { top } = tree();
    expect(await make(config, { execPath: join(top, 'app', 'nope'), above: top }).auto.availability()).toBe('unsafe-location');
    expect(await make(config, { execPath: join(top, 'app'), above: top }).auto.availability()).toBe('unsafe-location');
  });
});
