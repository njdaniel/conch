// Talk-key devices (issue #1381, D3-10 of #771): the listing opens nothing,
// the rule is the helper's, the save is two checked `config set` runs, and no
// privileged command is spawned anywhere in main.
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { VoiceSaveResult } from '../../../web/src/platform/desktop/bridge.ts';
import {
  BY_ID_DIR,
  KEY_DEVICE_DISCLOSURE,
  KEY_DEVICE_RELOAD,
  RULES_FILE,
  fitForARule,
  listKeyDevices,
  realKeyDeviceFs,
  udevRule,
  type KeyDeviceFs,
} from '../../main/voice/keydevices.ts';
import { PAGE_SAVE_KEYS, SAVE_KEYS, isPageSaveKey, saveValueFits } from '../../main/voice/oneshot.ts';
import { VoiceService, type VoiceIdentity } from '../../main/voice/service.ts';

// The test vector of voice/conch-voice/src/devices.rs.
const HELPER_RULE =
  'ACTION!="remove", SUBSYSTEM=="input", KERNEL=="event*", SYMLINK=="input/by-id/usb-Example_Keyboard-event-kbd", RUN+="/usr/bin/setfacl -m u:1000:r $devnode"';

// A fake /dev/input: by-id links to nodes, each a character device or not, and
// a record of every call, so that an open can be seen to be absent.
function fakeFs(links: Record<string, string>, nodes: Record<string, { char: boolean; readable: boolean }>): KeyDeviceFs & { calls: string[] } {
  const calls: string[] = [];
  return {
    calls,
    readdir: (dir) => {
      calls.push(`readdir ${dir}`);
      return Promise.resolve(Object.keys(links));
    },
    readlink: (path) => {
      calls.push(`readlink ${path}`);
      const name = path.slice(BY_ID_DIR.length + 1);
      const target = links[name];
      return target === undefined ? Promise.reject(new Error('EINVAL')) : Promise.resolve(target);
    },
    lstat: (path) => {
      calls.push(`lstat ${path}`);
      const node = nodes[path];
      return node === undefined ? Promise.reject(new Error('ENOENT')) : Promise.resolve({ isCharacterDevice: () => node.char });
    },
    canRead: (path) => {
      calls.push(`access ${path}`);
      return Promise.resolve(nodes[path]?.readable === true);
    },
  };
}

const counter = (): (() => string) => {
  let n = 0;
  return () => `id-${String(++n)}`;
};

describe('the listing', () => {
  it('lists keyboards and mice that resolve to a character device event<N> in /dev/input, and says which can be read', async () => {
    const fs = fakeFs(
      {
        'usb-Example_Keyboard-event-kbd': '../event3',
        'usb-Example_Mouse-event-mouse': '../event5',
        'usb-Example_Pad-event-kbd': '../event7',
        'usb-Example_Pad-if01-event-mouse': '../event8',
        'usb-Example_Mouse-mouse': '../mouse0',
        'usb-Other_Thing-event-joystick': '../event9',
      },
      {
        '/dev/input/event3': { char: true, readable: false },
        '/dev/input/event5': { char: true, readable: true },
        '/dev/input/event7': { char: true, readable: false },
        '/dev/input/event8': { char: true, readable: true },
        '/dev/input/event9': { char: true, readable: true },
      },
    );
    const { devices } = await listKeyDevices({ fs, uid: 1000, newId: counter() });
    expect(devices.map(({ name, kind, readable }) => ({ name, kind, readable }))).toEqual([
      { name: 'usb-Example_Keyboard-event-kbd', kind: 'keyboard', readable: false },
      { name: 'usb-Example_Mouse-event-mouse', kind: 'mouse', readable: true },
      { name: 'usb-Example_Pad-event-kbd', kind: 'keyboard', readable: false },
      { name: 'usb-Example_Pad-if01-event-mouse', kind: 'mouse', readable: true },
    ]);
    expect(new Set(devices.map((d) => d.id)).size).toBe(4);
  });

  it('gives a rule only to a device this user cannot read, and it is the helper’s rule for the same inputs', async () => {
    const fs = fakeFs(
      { 'usb-Example_Keyboard-event-kbd': '../event3', 'usb-Example_Mouse-event-mouse': '../event5' },
      { '/dev/input/event3': { char: true, readable: false }, '/dev/input/event5': { char: true, readable: true } },
    );
    const { devices } = await listKeyDevices({ fs, uid: 1000 });
    const kbd = devices.find((d) => d.kind === 'keyboard');
    expect(kbd?.rule).toEqual({ file: '/etc/udev/rules.d/70-conch-voice.rules', line: HELPER_RULE, reload: KEY_DEVICE_RELOAD });
    expect(devices.find((d) => d.kind === 'mouse')?.rule).toBeUndefined();
    expect(RULES_FILE).toBe('/etc/udev/rules.d/70-conch-voice.rules');
    expect(udevRule('usb-Example_Keyboard-event-kbd', 1000)).toBe(HELPER_RULE);
  });

  it.each([
    ['root', 0],
    ['an unknown user', undefined],
  ])('gives no rule for %s', async (_who, uid) => {
    const fs = fakeFs({ 'usb-K-event-kbd': '../event3' }, { '/dev/input/event3': { char: true, readable: false } });
    const { devices } = await listKeyDevices({ fs, uid });
    expect(devices).toHaveLength(1);
    expect(devices[0]?.rule).toBeUndefined();
  });

  it('refuses a name the helper would refuse: it is not listed and has no rule', async () => {
    const bad = ['usb-Evil", RUN+="/bin/sh-event-kbd', 'usb-Evil $devnode-event-kbd', 'usb-Evil\nKERNEL=="*"-event-kbd', 'usb-Evil/../x-event-kbd', 'usb-Evil\\x-event-kbd', 'usb-Evil*-event-kbd', 'usb-Évil-event-kbd', 'usb-Evil‮-event-kbd', ' -event-kbd'];
    for (const name of bad) {
      expect(udevRule(name, 1000), name).toBeNull();
      expect(fitForARule(name), name).toBe(false);
    }
    expect(udevRule('usb-0c45:7403_USB#Kbd+a=b@c.d-event-kbd', 1000)).not.toBeNull();
    const links = Object.fromEntries(bad.map((n) => [n, '../event3']));
    const fs = fakeFs({ ...links, 'usb-Good-event-kbd': '../event3' }, { '/dev/input/event3': { char: true, readable: false } });
    expect((await listKeyDevices({ fs, uid: 1000 })).devices.map((d) => d.name)).toEqual(['usb-Good-event-kbd']);
  });

  it('never puts GROUP, MODE, uaccess, the input group or a command in a rule or its words', async () => {
    const fs = fakeFs({ 'usb-K-event-kbd': '../event3' }, { '/dev/input/event3': { char: true, readable: false } });
    const { devices } = await listKeyDevices({ fs, uid: 1000 });
    const line = devices[0]?.rule?.line ?? '';
    for (const word of ['GROUP', 'MODE', 'uaccess', 'OWNER', 'TAG', 'chmod', 'chown']) expect(line, word).not.toContain(word);
    expect(line).toContain('/usr/bin/setfacl -m u:1000:r $devnode');
    const words = [...KEY_DEVICE_DISCLOSURE, ...KEY_DEVICE_RELOAD].join('\n');
    expect(words).not.toMatch(/usermod|pkexec|gpasswd|adduser|input group|\bgroup input\b|-aG/u);
    // The reload words are for the person; the disclosure runs nothing.
    expect(KEY_DEVICE_DISCLOSURE.join('\n')).not.toContain('sudo');
  });

  it('omits a link that resolves outside /dev/input, to a non-character device, or to anything not named event<N>', async () => {
    const fs = fakeFs(
      {
        'a-event-kbd': '../event1',
        'b-outside-event-kbd': '../../etc/event2',
        'c-absolute-event-kbd': '/etc/event3',
        'd-subdir-event-kbd': '../sub/event4',
        'e-notchar-event-kbd': '../event5',
        'f-name-event-kbd': '../mouse0',
        'g-name-event-kbd': '../event6x',
        'h-loop-event-kbd': 'i-loop-event-kbd',
        'i-loop-event-kbd': 'h-loop-event-kbd',
        'j-self-event-kbd': 'j-self-event-kbd',
        'k-missing-event-kbd': '../event99',
        'l-dir-event-kbd': '.',
        'm-prefix-event-kbd': '../xevent1',
      },
      {
        '/dev/input/event1': { char: true, readable: true },
        '/dev/input/event2': { char: true, readable: true },
        '/etc/event3': { char: true, readable: true },
        '/dev/input/sub/event4': { char: true, readable: true },
        '/dev/input/event5': { char: false, readable: true },
        '/dev/input/mouse0': { char: true, readable: true },
        '/dev/input/event6x': { char: true, readable: true },
      },
    );
    expect((await listKeyDevices({ fs })).devices.map((d) => d.name)).toEqual(['a-event-kbd']);
  });

  it('opens nothing: only readdir, readlink, lstat and access reach the file system', async () => {
    const fs = fakeFs({ 'usb-K-event-kbd': '../event3' }, { '/dev/input/event3': { char: true, readable: true } });
    await listKeyDevices({ fs });
    expect(fs.calls).toEqual(['readdir /dev/input/by-id', 'readlink /dev/input/by-id/usb-K-event-kbd', 'lstat /dev/input/event3', 'access /dev/input/event3']);
    // The seam has no open, read or write to call.
    expect(Object.keys(realKeyDeviceFs).sort()).toEqual(['canRead', 'lstat', 'readdir', 'readlink']);
    for (const f of fs.calls) expect(f).toMatch(/^(readdir|readlink|lstat|access) /u);
  });

  it('opens nothing in main/voice/keydevices.ts: it imports no open, read or process API', () => {
    const src = readFileSync(join(import.meta.dirname, '../../main/voice/keydevices.ts'), 'utf8')
      .split('\n')
      .filter((line) => !line.trimStart().startsWith('//'))
      .join('\n');
    expect(src).not.toMatch(/\b(open|openSync|readFile|readFileSync|createReadStream|createWriteStream|writeFile|spawn|exec|execFile|fork)\b\s*[(,}]/u);
    expect(src).not.toContain('child_process');
  });

  describe('against real directories of links', () => {
    let root: string;
    beforeEach(() => {
      root = mkdtempSync(join(tmpdir(), 'conch-keydev-'));
    });
    afterEach(() => {
      rmSync(root, { recursive: true, force: true });
    });

    it('omits a regular file, a loop and a link out of the directory (a real node is not a character device here)', async () => {
      const input = join(root, 'input');
      const byId = join(input, 'by-id');
      mkdirSync(byId, { recursive: true });
      writeFileSync(join(input, 'event1'), 'x');
      symlinkSync('../event1', join(byId, 'usb-File-event-kbd'));
      symlinkSync('b-event-kbd', join(byId, 'a-event-kbd'));
      symlinkSync('a-event-kbd', join(byId, 'b-event-kbd'));
      symlinkSync('/etc/passwd', join(byId, 'usb-Out-event-kbd'));
      symlinkSync('usb-File-event-kbd', join(byId, 'usb-Chain-event-kbd'));
      expect((await listKeyDevices({ byIdDir: byId, inputDir: input })).devices).toEqual([]);
      // Nothing was changed.
      expect(readdirSync(byId).sort()).toEqual(['a-event-kbd', 'b-event-kbd', 'usb-Chain-event-kbd', 'usb-File-event-kbd', 'usb-Out-event-kbd']);
      expect(statSync(join(input, 'event1')).size).toBe(1);
    });

    it('lists a link whose node is a character device (the lstat of a real one is faked: mknod needs root)', async () => {
      const input = join(root, 'input');
      const byId = join(input, 'by-id');
      mkdirSync(byId, { recursive: true });
      writeFileSync(join(input, 'event1'), 'x');
      symlinkSync('../event1', join(byId, 'usb-File-event-kbd'));
      const fs: KeyDeviceFs = { ...realKeyDeviceFs, lstat: () => Promise.resolve({ isCharacterDevice: () => true }) };
      const { devices } = await listKeyDevices({ fs, byIdDir: byId, inputDir: input, uid: 1000 });
      expect(devices.map((d) => [d.name, d.readable])).toEqual([['usb-File-event-kbd', true]]);
    });

    it('an absent by-id directory is an empty list; another failure is thrown', async () => {
      expect(await listKeyDevices({ byIdDir: join(root, 'nope'), inputDir: root })).toEqual({ devices: [] });
      writeFileSync(join(root, 'file'), 'x');
      await expect(listKeyDevices({ byIdDir: join(root, 'file'), inputDir: root })).rejects.toThrow();
    });
  });

  it('lists at most 64 devices', async () => {
    const links: Record<string, string> = {};
    const nodes: Record<string, { char: boolean; readable: boolean }> = {};
    for (let i = 0; i < 100; i++) {
      links[`usb-${String(i).padStart(3, '0')}-event-kbd`] = `../event${String(i)}`;
      nodes[`/dev/input/event${String(i)}`] = { char: true, readable: true };
    }
    expect((await listKeyDevices({ fs: fakeFs(links, nodes) })).devices).toHaveLength(64);
  });
});

describe('what the save takes', () => {
  it('keys.device is a by-id path of a name fit for a rule; keys.talk a code 1..767; the page names neither', () => {
    expect([...SAVE_KEYS]).toEqual([...PAGE_SAVE_KEYS, 'keys.device', 'keys.talk']);
    for (const k of ['keys.device', 'keys.talk']) expect(isPageSaveKey(k), k).toBe(false);
    for (const v of ['/dev/input/by-id/usb-K-event-kbd', '/dev/input/by-id/usb-0c45:7403_USB#Kbd+a=b@c.d-event-mouse']) expect(saveValueFits('keys.device', v), v).toBe(true);
    for (const v of ['', '/dev/input/event3', '/dev/input/by-id/', '/dev/input/by-id/../event3', '/dev/input/by-id/a/b', '/dev/input/by-id/a"b', '/dev/input/by-id/a\\b', '/dev/input/by-id/a\nb', ' /dev/input/by-id/a', '/dev/input/by-id/a ', '/etc/passwd', 'usb-K-event-kbd', '--help', '/dev/input/by-id/é', `/dev/input/by-id/${'x'.repeat(201)}`]) {
      expect(saveValueFits('keys.device', v), v).toBe(false);
    }
    for (const v of ['1', '30', '275', '767']) expect(saveValueFits('keys.talk', v), v).toBe(true);
    for (const v of ['0', '768', '-1', '01', '1.5', '1e2', ' 5', '5 ', 'KEY_F13', '', '0x10', '+5', '99999']) expect(saveValueFits('keys.talk', v), v).toBe(false);
    for (const k of ['keys.mute', 'keys.deafen']) expect(saveValueFits(k, '30'), k).toBe(false);
  });
});

// ---------------------------------------------------------- the service

class Identity implements VoiceIdentity {
  readonly profileId = 'p1';
  readonly configDir = '/nonexistent/config/p1';
  readonly principalId = 7;
  signedIn = true;
  credential(): null {
    return null;
  }
  current(): boolean {
    return this.signedIn;
  }
}

class FakeOneShot {
  calls: [string, string, string][] = [];
  answers: VoiceSaveResult[] = [];
  gate: Promise<void> | null = null;
  devices(): Promise<never> {
    return Promise.reject(new Error('unused'));
  }
  async save(configDir: string, key: string, value: string): Promise<VoiceSaveResult> {
    this.calls.push([configDir, key, value]);
    if (this.gate !== null) await this.gate;
    return this.answers.shift() ?? { ok: true };
  }
  cancel(): Promise<void> {
    return Promise.resolve();
  }
}

const noSupervisor = {
  state: { status: 'idle' as const },
  subscribe: () => () => undefined,
  start: () => Promise.reject(new Error('unused')),
  stop: () => Promise.resolve(),
  setMute: () => Promise.reject(new Error('unused')),
  setDeafen: () => Promise.reject(new Error('unused')),
  setDevices: () => Promise.reject(new Error('unused')),
  setTalkMode: () => Promise.reject(new Error('unused')),
};

describe('saving the binding', () => {
  let one: FakeOneShot;
  let logs: string[];
  let id: Identity;
  let svc: VoiceService;
  const fs = (): KeyDeviceFs =>
    fakeFs({ 'usb-Example_Keyboard-event-kbd': '../event3', 'usb-Mouse-event-mouse': '../event5' }, { '/dev/input/event3': { char: true, readable: false }, '/dev/input/event5': { char: true, readable: true } });
  beforeEach(() => {
    one = new FakeOneShot();
    logs = [];
    id = new Identity();
    svc = new VoiceService({ supervisor: noSupervisor, oneShot: one, identity: () => Promise.resolve(id), log: (l) => logs.push(l), keyDevices: { fs: fs(), uid: 1000, newId: counter() } });
  });

  const listed = async (): Promise<string> => {
    const r = await svc.keyDevices();
    if (!r.ok) throw new Error('no list');
    return r.devices[0]?.id ?? '';
  };

  it('lists with the disclosure, and the ids belong to the current listing', async () => {
    const r = await svc.keyDevices();
    expect(r.ok && r.disclosure).toEqual(KEY_DEVICE_DISCLOSURE);
    expect(r.ok && r.devices.map((d) => d.id)).toEqual(['id-1', 'id-2']);
  });

  it('runs keys.device then keys.talk for the profile in force', async () => {
    const deviceId = await listed();
    expect(await svc.saveTalkBinding({ deviceId, code: 275 })).toEqual({ ok: true });
    expect(one.calls).toEqual([
      ['/nonexistent/config/p1', 'keys.device', '/dev/input/by-id/usb-Example_Keyboard-event-kbd'],
      ['/nonexistent/config/p1', 'keys.talk', '275'],
    ]);
  });

  it('reports which of the two failed, and does not run the second after the first fails', async () => {
    const deviceId = await listed();
    one.answers = [{ ok: false, error: 'failed' }];
    expect(await svc.saveTalkBinding({ deviceId, code: 275 })).toEqual({ ok: false, error: 'failed', failed: 'device' });
    expect(one.calls).toHaveLength(1);
    one.calls = [];
    one.answers = [{ ok: true }, { ok: false, error: 'timed-out' }];
    expect(await svc.saveTalkBinding({ deviceId, code: 275 })).toEqual({ ok: false, error: 'timed-out', failed: 'talk' });
    expect(one.calls).toHaveLength(2);
  });

  it.each([
    ['an id never listed', 'nope', 275],
    ['an id from a path', '/dev/input/by-id/usb-Example_Keyboard-event-kbd', 275],
    ['the empty id', '', 275],
    ['code 0', 'id-1', 0],
    ['code 768', 'id-1', 768],
    ['a fraction', 'id-1', 1.5],
    ['NaN', 'id-1', Number.NaN],
    ['a string code', 'id-1', '275' as unknown as number],
  ])('refuses %s and runs nothing', async (_why, deviceId, code) => {
    await listed();
    expect(await svc.saveTalkBinding({ deviceId, code })).toEqual({ ok: false, error: 'invalid' });
    expect(one.calls).toEqual([]);
  });

  it('an id is good only for its own listing', async () => {
    const old = await listed();
    expect(old).toBe('id-1');
    await svc.keyDevices();
    expect(await svc.saveTalkBinding({ deviceId: old, code: 30 })).toEqual({ ok: false, error: 'invalid' });
    expect(await svc.saveTalkBinding({ deviceId: 'id-3', code: 30 })).toEqual({ ok: true });
  });

  it('an id does not survive a teardown', async () => {
    const deviceId = await listed();
    await svc.teardown();
    expect(await svc.saveTalkBinding({ deviceId, code: 30 })).toEqual({ ok: false, error: 'invalid' });
  });

  it('is not-signed-in without an identity, and one save at a time', async () => {
    const deviceId = await listed();
    const none = new VoiceService({ supervisor: noSupervisor, oneShot: one, identity: () => Promise.resolve(null), log: () => undefined, keyDevices: { fs: fs(), newId: counter() } });
    await none.keyDevices();
    expect(await none.saveTalkBinding({ deviceId: 'id-1', code: 30 })).toEqual({ ok: false, error: 'not-signed-in' });
    let open: () => void = () => undefined;
    one.gate = new Promise<void>((resolve) => {
      open = resolve;
    });
    const first = svc.saveTalkBinding({ deviceId, code: 30 });
    expect(await svc.saveTalkBinding({ deviceId, code: 31 })).toEqual({ ok: false, error: 'busy' });
    open();
    expect(await first).toEqual({ ok: true });
  });

  it('the page cannot reach keys.* through saveDevice', async () => {
    for (const key of ['keys.device', 'keys.talk'] as const) {
      expect(await svc.saveDevice({ key: key as never, value: key === 'keys.talk' ? '30' : '/dev/input/by-id/usb-K-event-kbd' })).toEqual({ ok: false, error: 'invalid' });
    }
    expect(one.calls).toEqual([]);
  });

  it('writes no device name or path to a log line (canary)', async () => {
    const throwing: KeyDeviceFs = { ...fs(), readdir: () => Promise.reject(new Error('EACCES /dev/input/by-id/usb-Canary_Name-event-kbd')) };
    const bad = new VoiceService({ supervisor: noSupervisor, oneShot: one, identity: () => Promise.resolve(id), log: (l) => logs.push(l), keyDevices: { fs: throwing } });
    expect(await bad.keyDevices()).toEqual({ ok: false, error: 'failed' });
    const deviceId = await listed();
    one.answers = [{ ok: false, error: 'failed' }];
    await svc.saveTalkBinding({ deviceId, code: 275 });
    expect(logs.join('\n')).not.toMatch(/Canary|Example_Keyboard|\/dev\/input/u);
  });
});

// ---------------------------------------------------------- nothing privileged

describe('nothing privileged is run', () => {
  const walk = (dir: string): string[] =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? walk(join(dir, e.name)) : e.name.endsWith('.ts') ? [join(dir, e.name)] : []));
  const files = walk(join(import.meta.dirname, '../../main'));

  it('only spawn.ts and the supervisor import child_process, and no shell is used', () => {
    const importers = files.filter((f) => /from 'node:child_process'|from 'child_process'|require\('child_process'\)/u.test(readFileSync(f, 'utf8'))).map((f) => f.slice(f.lastIndexOf('/main/') + 6));
    expect(importers.sort()).toEqual(['voice/spawn.ts', 'voice/supervisor.ts']);
    for (const f of files) {
      const src = readFileSync(f, 'utf8');
      expect(src, f).not.toMatch(/\bshell\s*:\s*(true|'|")/u);
      expect(src, f).not.toMatch(/\bexecSync\b|\bexecFileSync\b|\bspawnSync\b/u);
    }
  });

  it('no spawn call names sudo, pkexec, setfacl, usermod, udevadm or a shell', () => {
    for (const f of files) {
      const src = readFileSync(f, 'utf8');
      for (const m of src.matchAll(/\b(?:spawn|execFile|exec|fork)\s*\(([^)]*)\)/gu)) {
        expect(m[1], `${f}: ${m[0]}`).not.toMatch(/sudo|pkexec|setfacl|usermod|udevadm|gpasswd|\/bin\/(?:ba)?sh|'sh'|"sh"/u);
      }
    }
  });

  it('the privileged words appear in main only as the text of the rule and its reload instructions', () => {
    for (const f of files) {
      const src = readFileSync(f, 'utf8');
      if (!/sudo|pkexec|usermod|udevadm|setfacl/u.test(src)) continue;
      expect(f.endsWith('/voice/keydevices.ts'), f).toBe(true);
    }
  });
});
