// The vault's files: <root>/profiles/<id>/credential.bin, directories 0700,
// the file 0600, replaced atomically (temp file, fsync, rename, fsync of the
// directory).
//
// Input: a root directory and a profile id. Output: bytes in, bytes out. This
// is the only module under desktop/main that writes a secret's ciphertext to
// disk (a unit test greps for that). It never sees a plaintext secret and
// logs nothing.
import { randomBytes } from 'node:crypto';
import { chmod, lstat, mkdir, open, readFile, rename, rm } from 'node:fs/promises';
import { join } from 'node:path';

const FILE_NAME = 'credential.bin';
const DIR_MODE = 0o700;
const FILE_MODE = 0o600;
// A sealed token is a few hundred bytes. Anything larger is not ours.
const MAX_FILE_BYTES = 64 * 1024;

const PROFILE_ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

export function validProfileId(id: string): boolean {
  return PROFILE_ID.test(id);
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

class UnsafeRoot extends Error {}

// privateDir makes dir (and its missing parents) and leaves it 0700 whatever
// the umask was, refusing a symlink or a non-directory in its place.
async function privateDir(dir: string): Promise<void> {
  await mkdir(dir, { recursive: true, mode: DIR_MODE });
  const st = await lstat(dir);
  if (st.isSymbolicLink() || !st.isDirectory()) {
    throw new Error('vault path is not a plain directory');
  }
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
  | { readonly kind: 'corrupt' };

// readCredential reads the profile's file. A missing file is absent; an empty,
// oversized or non-regular one is corrupt (and is left where it is).
export async function readCredential(root: string, id: string): Promise<ReadResult> {
  const dir = profileDir(root, id);
  const file = join(dir, FILE_NAME);
  let st;
  try {
    if (!(await checkRoot(root))) {
      return { kind: 'absent' };
    }
    const dirStat = await lstat(dir);
    if (dirStat.isSymbolicLink() || !dirStat.isDirectory()) {
      return { kind: 'corrupt' };
    }
    st = await lstat(file);
  } catch (err) {
    if (isCode(err, 'ENOENT') || isCode(err, 'ENOTDIR')) {
      return { kind: 'absent' };
    }
    if (err instanceof UnsafeRoot) {
      return { kind: 'corrupt' };
    }
    throw err;
  }
  if (!st.isFile() || st.size === 0 || st.size > MAX_FILE_BYTES) {
    return { kind: 'corrupt' };
  }
  return { kind: 'bytes', data: await readFile(file) };
}

// removeProfile deletes the profile's directory and what is in it. Missing is
// not an error. Only this profile's directory is touched.
export async function removeProfile(root: string, id: string): Promise<void> {
  await rm(profileDir(root, id), { recursive: true, force: true });
}

function isCode(err: unknown, code: string): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === code;
}
