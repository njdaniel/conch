import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import afterPack from '../../build/after-pack.mjs';

const dirs: string[] = [];
function tmp(): string {
  const dir = mkdtempSync(join(tmpdir(), 'conch-after-pack-'));
  dirs.push(dir);
  return dir;
}
afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

describe('afterPack', () => {
  it('removes only the Windows default app and tolerates a missing default app', async () => {
    const appOutDir = tmp();
    const resources = join(appOutDir, 'resources');
    mkdirSync(resources);
    const app = join(resources, 'app.asar');
    const defaultApp = join(resources, 'default_app.asar');
    writeFileSync(app, 'Conch');
    writeFileSync(defaultApp, 'Electron');

    await afterPack({ appOutDir, electronPlatformName: 'linux' });
    expect(existsSync(defaultApp)).toBe(true);
    await afterPack({ appOutDir, electronPlatformName: 'win32' });
    expect(existsSync(defaultApp)).toBe(false);
    expect(readFileSync(app, 'utf8')).toBe('Conch');
    await afterPack({ appOutDir, electronPlatformName: 'win32' });
  });

  it('rejects linked resources and preserves the shared default app', async () => {
    const appOutDir = tmp();
    const shared = tmp();
    const defaultApp = join(shared, 'default_app.asar');
    writeFileSync(defaultApp, 'shared Electron app');
    symlinkSync(shared, join(appOutDir, 'resources'), 'junction');

    await expect(afterPack({ appOutDir, electronPlatformName: 'win32' })).rejects.toThrow('Electron staging resources must be a real directory');
    expect(readFileSync(defaultApp, 'utf8')).toBe('shared Electron app');
  });
});
