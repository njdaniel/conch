// The vault's files: <root>/profiles/<id>/credential.bin, directories 0700,
// the file 0600, replaced atomically (temp file, fsync, rename, fsync of the
// directory).
//
// Input: a root directory and a profile id. Output: bytes in, bytes out, and
// the list of profile ids for the start sweep of issue #2217. This
// is the only module under desktop/main that writes a secret's ciphertext to
// disk (a unit test greps for that). It never sees a plaintext secret and
// logs nothing.
import { randomBytes } from 'node:crypto';
import { constants as fsConstants, type Stats } from 'node:fs';
import { chmod, lstat, mkdir, open, opendir, rename, rm } from 'node:fs/promises';
import { join } from 'node:path';

import { isProfileId } from '../profiles/ids.ts';

const FILE_NAME = 'credential.bin';
const DIR_MODE = 0o700;
const FILE_MODE = 0o600;
// A sealed token is a few hundred bytes. Anything larger is not ours.
const MAX_FILE_BYTES = 64 * 1024;

const PROFILE_ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

export function validProfileId(id: string): boolean {
  return PROFILE_ID.test(id);
}

// profileConfigDir is <root>/profiles/<id>: one profile's own folder under the
// data root, which is also the CONCH_CONFIG_DIR the voice helper is given
// (issue #1785): it reads its voice.toml from there and from nowhere shared,
// so it can never read another profile's. On Windows the root is userData,
// under %APPDATA%, whose default ACL is the protection; no drive root is ever
// the base (windows-desktop.md §2.7).
export function profileConfigDir(root: string, id: string): string {
  // The profile store's own id: 32 lowercase hex, which no path syntax can
  // use to leave profiles/.
  if (!isProfileId(id)) throw new Error('invalid profile id');
  return profileDir(root, id);
}

function profileDir(root: string, id: string): string {
  if (!validProfileId(id)) {
    // Never echo the value: it may be hostile input.
    throw new Error('invalid profile id');
  }
  return join(root, 'profiles', id);
}

// checkRoot decides whether a pre-existing root may be trusted: a plain
// directory (not a symlink), owned by this user and closed to group and
// others. POSIX only; Windows has no such mode bits. Returns false when the
// root does not exist.
async function checkRoot(root: string): Promise<boolean> {
  let st;
  try {
    st = await lstat(root);
  } catch (err) {
    if (isCode(err, 'ENOENT')) {
      return false;
    }
    throw err;
  }
  if (st.isSymbolicLink() || !st.isDirectory()) {
    throw new UnsafeRoot('vault root is not a plain directory');
  }
  if (typeof process.getuid === 'function') {
    if (st.uid !== process.getuid() || (st.mode & 0o077) !== 0) {
      throw new UnsafeRoot('vault root is not private to this user');
    }
  }
  return true;
}

// Thrown when a vault path fails a directory safety check. The vault maps
// it to the session-only reason 'unsafe-root' when a write is refused.
export class UnsafeRoot extends Error {}

// prepareRoot checks the data root's ancestors at startup. Inside the
// private vault root, neither profiles/ nor the profile directory may be
// substituted by another user before a Linux read or cleanup.
function safeDirectory(st: Stats): boolean {
  const uid = process.getuid?.();
  return st.isDirectory() && (uid === undefined || ((st.uid === uid || st.uid === 0) && (st.mode & 0o022) === 0));
}

// privateDir makes dir and leaves it 0700, refusing to chmod a directory
// that fails the owner, mode or plain-directory check.
async function privateDir(dir: string): Promise<void> {
  await mkdir(dir, { recursive: true, mode: DIR_MODE });
  if (!safeDirectory(await lstat(dir))) throw new UnsafeRoot('unsafe vault directory');
  await chmod(dir, DIR_MODE);
}

async function syncDir(dir: string): Promise<void> {
  const fh = await open(dir, 'r');
  try {
    await fh.sync();
  } finally {
    await fh.close();
  }
}

export interface WriteHooks {
  // Test seam for the directory fsync.
  readonly syncDir?: ((dir: string) => Promise<void>) | undefined;
}

// writeCredential replaces the profile's file with data. On any failure the
// previous file is as it was and no temp file remains. It does not guard
// other processes: the single-instance lock of #945 does that.
export async function writeCredential(
  root: string,
  id: string,
  data: Uint8Array,
  hooks: WriteHooks = {},
): Promise<void> {
  const dir = profileDir(root, id);
  // A root that exists must already be ours and private; one that does not is
  // created 0700 below.
  await checkRoot(root);
  await privateDir(root);
  await privateDir(join(root, 'profiles'));
  await privateDir(dir);
  const tmp = join(dir, `${FILE_NAME}.${randomBytes(8).toString('hex')}.tmp`);
  try {
    const fh = await open(tmp, 'wx', FILE_MODE);
    try {
      await fh.chmod(FILE_MODE);
      await fh.writeFile(data);
      await fh.sync();
    } finally {
      await fh.close();
    }
    await rename(tmp, join(dir, FILE_NAME));
  } catch (err) {
    await rm(tmp, { force: true });
    throw err;
  }
  // The rename has happened: the credential is in place. The directory fsync
  // only makes that survive a power cut, so its failure must not turn a
  // success into an error. On Windows, fsync of a directory handle can throw
  // EPERM; the error is ignored on every platform. The file's own fsync
  // before the rename stays mandatory.
  try {
    await (hooks.syncDir ?? syncDir)(dir);
  } catch {
    // best-effort durability
  }
}

export type ReadResult =
  | { readonly kind: 'absent' }
  | { readonly kind: 'bytes'; readonly data: Buffer }
  | { readonly kind: 'corrupt' }
  // A regular file of ours, of a sane size, with group or other mode bits.
  // Its contents are refused. Linux vault cleanup deletes it unread.
  | { readonly kind: 'loose-mode' }
  // The root failed its ownership, directory or permission checks.
  | { readonly kind: 'unsafe-root' }
  // A directory below the vault root is unsafe to traverse.
  | { readonly kind: 'unsafe-directory' };

interface ReadOptions {
  readonly checkAncestors?: boolean;
  // Test seam: runs after the path checks and before the file is opened, so a
  // test can swap the file for a symlink in between.
  readonly beforeOpen?: (() => Promise<void>) | undefined;
}

// Where the platform has it (Linux, macOS), opening a symlink fails instead of
// following it. Windows has none; there the lstat before the open is the check.
const NO_FOLLOW = 'O_NOFOLLOW' in fsConstants ? fsConstants.O_NOFOLLOW : 0;

// readCredential reads the profile's file. A missing file is absent; an empty,
// oversized or non-regular one is corrupt (and is left where it is).
//
// The path is checked with lstat, then the file is opened without following a
// symlink, and the open handle is checked again with fstat (a regular file of a
// sane size and, where there are uids, owned by this user and closed to group
// and others) and read through that handle, at most MAX_FILE_BYTES (issue
// #2217's review). Linux cleanup also checks the directories inside the
// vault root and requires the opened inode to match the lstat.
export async function readCredential(root: string, id: string, options: ReadOptions = {}): Promise<ReadResult> {
  const dir = profileDir(root, id);
  const file = join(dir, FILE_NAME);
  let st;
  try {
    if (!(await checkRoot(root))) {
      return { kind: 'absent' };
    }
    if (options.checkAncestors === true && !safeDirectory(await lstat(join(root, 'profiles')))) return { kind: 'unsafe-directory' };
    const dirStat = await lstat(dir);
    if (dirStat.isSymbolicLink() || !dirStat.isDirectory()) {
      return { kind: 'corrupt' };
    }
    if (options.checkAncestors === true && !safeDirectory(dirStat)) return { kind: 'unsafe-directory' };
    st = await lstat(file);
  } catch (err) {
    if (isCode(err, 'ENOENT') || isCode(err, 'ENOTDIR')) {
      return { kind: 'absent' };
    }
    if (err instanceof UnsafeRoot) {
      return { kind: 'unsafe-root' };
    }
    throw err;
  }
  if (!st.isFile() || st.size === 0 || st.size > MAX_FILE_BYTES) {
    return { kind: 'corrupt' };
  }
  await options.beforeOpen?.();
  let fh;
  try {
    fh = await open(file, fsConstants.O_RDONLY | NO_FOLLOW);
  } catch (err) {
    if (isCode(err, 'ENOENT') || isCode(err, 'ENOTDIR')) return { kind: 'absent' };
    // ELOOP (EMLINK on some BSDs): it became a symlink after the check.
    if (isCode(err, 'ELOOP') || isCode(err, 'EMLINK')) return { kind: 'corrupt' };
    // A file can deny its owner read access and still expose it to others.
    if (options.checkAncestors === true && isCode(err, 'EACCES') && process.getuid?.() === st.uid && (st.mode & 0o077) !== 0) {
      return { kind: 'loose-mode' };
    }
    throw err;
  }
  try {
    const held = await fh.stat();
    if (!held.isFile() || held.size === 0 || held.size > MAX_FILE_BYTES) return { kind: 'corrupt' };
    if (options.checkAncestors === true && (held.dev !== st.dev || held.ino !== st.ino)) return { kind: 'corrupt' };
    if (typeof process.getuid === 'function') {
      if (held.uid !== process.getuid()) return { kind: 'corrupt' };
      if (((st.mode | held.mode) & 0o077) !== 0) return { kind: 'loose-mode' };
    }
    // One byte more than the limit, to see a file that grew after the fstat.
    const buf = Buffer.alloc(MAX_FILE_BYTES + 1);
    let length = 0;
    for (;;) {
      const { bytesRead } = await fh.read(buf, length, buf.length - length, length);
      if (bytesRead === 0) break;
      length += bytesRead;
      if (length > MAX_FILE_BYTES) return { kind: 'corrupt' };
    }
    if (length === 0) return { kind: 'corrupt' };
    return { kind: 'bytes', data: Buffer.from(buf.subarray(0, length)) };
  } finally {
    await fh.close();
  }
}

// The most directory entries a listing looks at (issue #2217): the store keeps
// at most 32 profiles, so a folder with more than this holds things that are
// not ours, and the rest are not looked at.
const MAX_LISTED = 256;

// profileIds lists the names under <root>/profiles that are profile ids and
// are directories, at most MAX_LISTED entries looked at, sorted. It follows
// nothing: a symlink, a file or a name that is not an id is skipped, and each
// id's file is then checked by readCredential. A missing or unsafe root
// lists none. An unsafe profiles/ is reported without listing its contents.
export async function profileIds(root: string): Promise<readonly string[] | 'unsafe-directory'> {
  try {
    if (!(await checkRoot(root))) return [];
  } catch (err) {
    if (err instanceof UnsafeRoot) return [];
    throw err;
  }
  const dir = join(root, 'profiles');
  let st;
  try {
    st = await lstat(dir);
  } catch (err) {
    if (isCode(err, 'ENOENT') || isCode(err, 'ENOTDIR')) return [];
    throw err;
  }
  if (!safeDirectory(st)) return 'unsafe-directory';
  const ids: string[] = [];
  let seen = 0;
  // Leaving the loop early closes the directory.
  for await (const entry of await opendir(dir)) {
    if (++seen > MAX_LISTED) break;
    if (entry.isDirectory() && validProfileId(entry.name)) ids.push(entry.name);
  }
  return ids.sort();
}

// removeProfile deletes the profile's directory and what is in it. Linux
// checks the directories again before deletion. Missing is not an error.
export async function removeProfile(root: string, id: string, checkAncestors: boolean): Promise<void> {
  const dir = profileDir(root, id);
  if (checkAncestors) {
    try {
      if (!(await checkRoot(root))) return;
      if (!safeDirectory(await lstat(join(root, 'profiles'))) || !safeDirectory(await lstat(dir))) {
        throw new UnsafeRoot('unsafe vault directory');
      }
    } catch (err) {
      if (isCode(err, 'ENOENT')) return;
      throw err;
    }
  }
  await rm(dir, { recursive: true, force: true });
}

function isCode(err: unknown, code: string): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === code;
}
