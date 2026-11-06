// Talk-key devices (issue #1381, D3-10 of #771; decision D-4; linux-desktop.md
// §7, conch-voice.md §4). Input: the entries of /dev/input/by-id. Output: the
// devices a person could bind a talk key on, by name, whether this user can
// read each, and for one that is not readable the exact udev rule the person
// would install themselves. Owns: nothing; it keeps no state.
//
// What it keeps:
//   - no device is opened or read: the directory is listed, each link is read
//     (readlink), its target is looked at (lstat) and asked about
//     (access R_OK), and that is all. There is no `open` in the file system
//     seam below, so that nothing here can;
//   - nothing is run: no sudo, pkexec, setfacl, usermod or udevadm, and no
//     shell. The rule is text for the person to read and install;
//   - a rule is made only for a name of the characters udev leaves, as the
//     helper's own rule is (voice/conch-voice/src/devices.rs fit_for_a_rule,
//     udev_rule), and is that rule, character for character; it names one
//     device and one user, reads only, and never sets a group, a mode or
//     uaccess; the `input` group is never suggested;
//   - a name that is not fit for a rule is not listed at all, nor saved.
import { access, lstat, readdir, readlink } from 'node:fs/promises';
import { basename, dirname, resolve } from 'node:path';

import type { VoiceKeyDevice, VoiceKeyDeviceRule } from '../../../web/src/platform/desktop/bridge.ts';

export const INPUT_DIR = '/dev/input';
export const BY_ID_DIR = '/dev/input/by-id';

// The file the person puts the rule in; it must sort after the rules that make
// the by-id links (60).
export const RULES_FILE = '/etc/udev/rules.d/70-conch-voice.rules';

// The most devices listed, and the longest name taken.
export const MAX_KEY_DEVICES = 64;
const MAX_NAME_BYTES = 200;

// access(2)'s R_OK.
const R_OK = 4;

export const KEYBOARD_SUFFIX = '-event-kbd';
export const MOUSE_SUFFIX = '-event-mouse';

// What the person is told before a rule is shown. It is the helper's own
// disclosure in short: the grant is to every program of this user.
export const KEY_DEVICE_DISCLOSURE: readonly string[] = [
  'A rule below gives your user read access to one input device, and to nothing else. It is not for Conch alone: every program you run, as you, can then read that device, including what is typed on it.',
  'Conch acts only on the keys you bind for talk, and discards every other key as it reads, but that is a property of its code, not of the permission.',
  'Conch does not install the rule, change any permission or run anything as root. You install it yourself, as root, if you accept that.',
  'Do not widen this to a group that covers every input device on the machine: that would give every program you run access to all of them.',
];

// What follows installing the rule; the helper's own instructions.
export const KEY_DEVICE_RELOAD: readonly string[] = [
  'Then run `sudo udevadm control --reload` and unplug and replug the device (or `sudo udevadm trigger --subsystem-match=input --action=change`).',
  'To take it back, delete the line, reload, and unplug and replug the device.',
  'Permission is checked when a device is opened: a program that already has it open goes on reading until it closes it or the device is replugged.',
];

// The file system calls this file may make. There is no open, read or write.
export interface KeyDeviceFs {
  readdir(dir: string): Promise<string[]>;
  readlink(path: string): Promise<string>;
  // The entry itself, not what a link points to.
  lstat(path: string): Promise<{ isCharacterDevice(): boolean }>;
  // Resolves if the calling user could read path (access R_OK); never opens it.
  canRead(path: string): Promise<boolean>;
}

export const realKeyDeviceFs: KeyDeviceFs = {
  readdir: (dir) => readdir(dir),
  readlink: (path) => readlink(path),
  lstat: (path) => lstat(path),
  canRead: (path) =>
    access(path, R_OK).then(
      () => true,
      () => false,
    ),
};

export interface KeyDeviceDeps {
  readonly fs?: KeyDeviceFs;
  // The user's id, for the rule; undefined when it is not known.
  readonly uid?: number | undefined;
  readonly byIdDir?: string;
  readonly inputDir?: string;
  readonly newId?: () => string;
}

// fitForARule: ASCII letters and digits and `#+-.:=@_`, as the helper's rule.
export function fitForARule(name: string): boolean {
  return name !== '' && /^[A-Za-z0-9#+\-.:=@_]+$/u.test(name);
}

// udevRule is the helper's udev_rule, for the same inputs: the rule that
// grants user uid read access to the one device linked as name, or null for a
// name that is not fit to put in a rule.
export function udevRule(name: string, uid: number): string | null {
  if (!fitForARule(name) || !Number.isInteger(uid) || uid < 0) return null;
  return `ACTION!="remove", SUBSYSTEM=="input", KERNEL=="event*", SYMLINK=="input/by-id/${name}", RUN+="/usr/bin/setfacl -m u:${String(uid)}:r $devnode"`;
}

function ruleFor(name: string, uid: number | undefined): VoiceKeyDeviceRule | undefined {
  // Root needs none, and an unknown user cannot be named in one.
  if (uid === undefined || uid === 0) return undefined;
  const line = udevRule(name, uid);
  return line === null ? undefined : { file: RULES_FILE, line, reload: KEY_DEVICE_RELOAD };
}

function kindOf(name: string): 'keyboard' | 'mouse' | null {
  if (name.endsWith(KEYBOARD_SUFFIX)) return 'keyboard';
  if (name.endsWith(MOUSE_SUFFIX)) return 'mouse';
  return null;
}

const EVENT_NODE = /^event\d+$/u;

// The by-id path the helper's keys.device takes for a listed name.
export function devicePath(name: string, byIdDir: string = BY_ID_DIR): string {
  return `${byIdDir}/${name}`;
}

export interface KeyDeviceListing {
  readonly devices: readonly VoiceKeyDevice[];
}

// listKeyDevices lists the candidates. A directory that does not exist is an
// empty list; any other failure to list it is thrown.
export async function listKeyDevices(deps: KeyDeviceDeps = {}): Promise<KeyDeviceListing> {
  const fs = deps.fs ?? realKeyDeviceFs;
  const byId = deps.byIdDir ?? BY_ID_DIR;
  const inputDir = deps.inputDir ?? INPUT_DIR;
  const newId = deps.newId ?? (() => crypto.randomUUID());
  let entries: string[];
  try {
    entries = await fs.readdir(byId);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return { devices: [] };
    throw err;
  }
  const candidates = entries
    .filter((name) => kindOf(name) !== null && fitForARule(name) && Buffer.byteLength(name) <= MAX_NAME_BYTES)
    .sort()
    .slice(0, MAX_KEY_DEVICES);
  const devices: VoiceKeyDevice[] = [];
  for (const name of candidates) {
    const kind = kindOf(name);
    if (kind === null) continue;
    const node = await nodeOf(fs, byId, inputDir, name);
    if (node === null) continue;
    const readable = await fs.canRead(node).catch(() => false);
    const rule = readable ? undefined : ruleFor(name, deps.uid);
    devices.push({ id: newId(), name, kind, readable, ...(rule === undefined ? {} : { rule }) });
  }
  return { devices };
}

// nodeOf is the event node a by-id link names, or null when the link does not
// resolve to a character device `event<N>` directly in /dev/input. One link is
// followed, never a chain, so a loop is not one.
async function nodeOf(fs: KeyDeviceFs, byId: string, inputDir: string, name: string): Promise<string | null> {
  try {
    const target = resolve(byId, await fs.readlink(`${byId}/${name}`));
    if (dirname(target) !== inputDir || !EVENT_NODE.test(basename(target))) return null;
    return (await fs.lstat(target)).isCharacterDevice() ? target : null;
  } catch {
    return null;
  }
}
