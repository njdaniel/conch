import { rm } from 'node:fs/promises';
import { join } from 'node:path';

export default async function afterPack({ appOutDir, electronPlatformName }) {
  if (electronPlatformName !== 'win32') return;
  // electron-builder preserves the supplied electronDist, including its
  // default app. The packaged Conch executable loads resources/app.asar.
  await rm(join(appOutDir, 'resources', 'default_app.asar'), { force: true });
}
