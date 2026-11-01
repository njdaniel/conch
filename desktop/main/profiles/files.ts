// The profile store's disk: <root>/profiles.json (non-secret metadata only,
// 0600, replaced atomically) and <root>/profiles/<id>/ (a profile's own
// directory and cache, 0700). Input: the data root. Output: text in, text out.
// Together with vault/files.ts and instance.ts it is the only place under
// desktop/main that writes to disk (a unit test greps for that). It never sees
// a credential.
import { randomBytes } from 'node:crypto';
import { chmod, lstat, mkdir, open, readFile, rename, rm } from 'node:fs/promises';
import { join } from 'node:path';

import { isProfileId } from './ids.ts';

const FILE_NAME = 'profiles.json';
const DIR_MODE = 0o700;
const FILE_MODE = 0o600;
const MAX_BYTES = 1024 * 1024;

export type ReadText = { readonly kind: 'absent' } | { readonly kind: 'text'; readonly text: string } | { readonly kind: 'unreadable' };

export interface ProfileFiles {
  read(): Promise<ReadText>;
  write(text: string): Promise<void>;
  // Moves a file that could not be understood aside, to a name of its own, so
  // a fresh one can be written. Rejects if it cannot be moved. Nothing else is
  // touched.
  quarantine(): Promise<void>;
  ensureProfileDir(id: string): Promise<void>;
  removeProfileDir(id: string): Promise<void>;
}

function isCode(err: unknown, code: string): boolean {
  return typeof err === 'object' && err !== null && (err as { code?: unknown }).code === code;
}

// privateDir makes dir (and its missing parents) and leaves it 0700, refusing a
// symlink, a non-directory, or a directory that belongs to another user.
async function privateDir(dir: string, uid: number | null): Promise<void> {
  await mkdir(dir, { recursive: true, mode: DIR_MODE });
  const st = await lstat(dir);
  if (st.isSymbolicLink() || !st.isDirectory()) throw new Error('not a plain directory');
  if (uid !== null && st.uid !== uid) throw new Error('directory is not owned by this user');
  await chmod(dir, DIR_MODE);
}

export interface ProfileFilesOptions {
  // The user that must own the directories; the process's by default (a test
  // seam). Not checked where the platform has no uids.
  readonly uid?: number;
}

export function createProfileFiles(root: string, options: ProfileFilesOptions = {}): ProfileFiles {
  const uid = options.uid ?? (typeof process.getuid === 'function' ? process.getuid() : null);
  const file = join(root, FILE_NAME);
  const dirOf = (id: string): string => {
    if (!isProfileId(id)) throw new Error('invalid profile id');
    return join(root, 'profiles', id);
  };
  return {
    async read() {
      try {
        const st = await lstat(file);
        if (!st.isFile() || st.size > MAX_BYTES) return { kind: 'unreadable' };
        return { kind: 'text', text: await readFile(file, 'utf8') };
      } catch (err) {
        return isCode(err, 'ENOENT') ? { kind: 'absent' } : { kind: 'unreadable' };
      }
    },
    async write(text) {
      await privateDir(root, uid);
      const tmp = join(root, `${FILE_NAME}.${randomBytes(8).toString('hex')}.tmp`);
      try {
        const fh = await open(tmp, 'wx', FILE_MODE);
        try {
          await fh.chmod(FILE_MODE);
          await fh.writeFile(text, 'utf8');
          await fh.sync();
        } finally {
          await fh.close();
        }
        await rename(tmp, file);
      } catch (err) {
        await rm(tmp, { force: true });
        throw err;
      }
    },
    async quarantine() {
      // A name no earlier set-aside file has, so that nothing is overwritten;
      // a failure is the caller's to stop on.
      const aside = join(root, `${FILE_NAME}.corrupt-${String(Date.now())}-${randomBytes(4).toString('hex')}`);
      await rename(file, aside);
    },
    async ensureProfileDir(id) {
      await privateDir(root, uid);
      await privateDir(join(root, 'profiles'), uid);
      await privateDir(dirOf(id), uid);
    },
    async removeProfileDir(id) {
      await rm(dirOf(id), { recursive: true, force: true });
    },
  };
}
