// One instance per data root (issue #945; linux-desktop.md §4, windows-desktop.md
// §9.2). Input: Electron's app, and the data root (app.getPath('userData')).
// Output: whether this process may use the root, and the root itself made
// private.
//
// Electron scopes requestSingleInstanceLock() to the user data directory, so
// an instance started with another --user-data-dir (an isolated test
// instance) does not collide, and two on the same root do: the second focuses
// the first and exits before it has read or written anything. The lock is
// taken before the first vault call, which assumes one writer per root.
import { chmod, lstat, mkdir, realpath } from 'node:fs/promises';
import { dirname } from 'node:path';

export interface InstanceApp {
  requestSingleInstanceLock(): boolean;
  on(event: 'second-instance', listener: () => void): unknown;
  exit(code: number): void;
}

// acquire takes the lock. false: another instance has the root; this process
// has asked it to focus and has exited, and must do nothing more.
export function acquire(app: InstanceApp, focus: () => void): boolean {
  if (!app.requestSingleInstanceLock()) {
    app.exit(0);
    return false;
  }
  app.on('second-instance', focus);
  return true;
}

export type RootCheck = { readonly ok: true } | { readonly ok: false; readonly reason: 'unsafe' };

// The user the data must belong to; the process's own unless a test says
// otherwise.
function ownerOf(uid: number | undefined): number | null {
  return uid ?? (typeof process.getuid === 'function' ? process.getuid() : null);
}

// prepareRoot makes the data root and leaves it 0700: owned by this user, a
// plain directory, not a symlink. Every ancestor must be a directory that is
// owned by this user or by root and is not writable by group or others (a
// sticky one, such as /tmp, is allowed: others cannot replace our entry in
// it). POSIX only; Windows has no such mode bits and relies on the profile
// directory's ACL.
export async function prepareRoot(root: string, options: { readonly uid?: number } = {}): Promise<RootCheck> {
  const owner = ownerOf(options.uid);
  const ownedByUs = (st: { readonly uid: number }): boolean => owner === null || st.uid === owner;
  if (process.platform === 'win32') {
    await mkdir(root, { recursive: true });
    return { ok: true };
  }
  try {
    await mkdir(root, { recursive: true, mode: 0o700 });
    const st = await lstat(root);
    if (st.isSymbolicLink() || !st.isDirectory() || !ownedByUs(st)) return { ok: false, reason: 'unsafe' };
    await chmod(root, 0o700);
    // Judge the real parents: a symlinked ancestor must not hide a parent
    // that others can write to.
    const real = await realpath(root);
    const realStat = await lstat(real);
    if (!realStat.isDirectory() || !ownedByUs(realStat)) return { ok: false, reason: 'unsafe' };
    for (let dir = dirname(real); ; dir = dirname(dir)) {
      const parent = await lstat(dir);
      // A symlinked ancestor is the user's own arrangement (a home on another
      // disk); what matters is what it leads to, which lstat of the next level
      // checks. Only the mode and owner of a real directory are judged.
      if (!parent.isSymbolicLink()) {
        const writable = (parent.mode & 0o022) !== 0 && (parent.mode & 0o1000) === 0;
        if (!parent.isDirectory() || writable || !(ownedByUs(parent) || parent.uid === 0)) return { ok: false, reason: 'unsafe' };
      }
      if (dirname(dir) === dir) break;
    }
    return { ok: true };
  } catch {
    return { ok: false, reason: 'unsafe' };
  }
}
