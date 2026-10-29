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

// writeCredential replaces the profile's file with data. On any failure the
// previous file is as it was and no temp file remains.
export async function writeCredential(root: string, id: string, data: Uint8Array): Promise<void> {
  const dir = profileDir(root, id);
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
  await syncDir(dir);
}

export type ReadResult =
  | { readonly kind: 'absent' }
  | { readonly kind: 'bytes'; readonly data: Buffer }
  | { readonly kind: 'corrupt' };

// readCredential reads the profile's file. A missing file is absent; an empty,
// oversized or non-regular one is corrupt (and is left where it is).
export async function readCredential(root: string, id: string): Promise<ReadResult> {
  const file = join(profileDir(root, id), FILE_NAME);
  let st;
  try {
    st = await lstat(file);
  } catch (err) {
    if (isCode(err, 'ENOENT') || isCode(err, 'ENOTDIR')) {
      return { kind: 'absent' };
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
