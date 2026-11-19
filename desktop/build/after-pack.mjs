import { lstat, realpath, unlink } from 'node:fs/promises';
import { join, relative } from 'node:path';

export default async function afterPack({ appOutDir, electronPlatformName }) {
  if (electronPlatformName !== 'win32') return;
  // electron-builder preserves the supplied electronDist, including its
  // default app. The packaged Conch executable loads resources/app.asar.
  try {
    if (!(await lstat(appOutDir)).isDirectory()) throw new Error('Electron staging output must be a real directory');
    const staging = await realpath(appOutDir);
    const resources = join(staging, 'resources');
    // A custom distribution may leave a symlink or junction in the staging
    // tree. Check each entry without following links before resolving the file.
    if (!(await lstat(resources)).isDirectory()) throw new Error('Electron staging resources must be a real directory');
    const archive = join(resources, 'default_app.asar');
    if (!(await lstat(archive)).isFile()) throw new Error('Electron staging default_app.asar must be a regular file');
    const resolved = await realpath(archive);
    if (relative(archive, resolved) !== '') throw new Error('default_app.asar resolves outside its expected staging path');
    await unlink(resolved);
  } catch (error) {
    if (error?.code !== 'ENOENT') throw error;
  }
}
