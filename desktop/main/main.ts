// Electron's main entry (vite.main.config.ts bundles it to out/main/main.cjs).
import { app } from 'electron';

import { applyAppUserModelId } from './app-user-model.ts';
import { registerScheme, start } from './app.ts';
import { requireSafeChildProcessEnv } from './child-process-env.ts';

async function launch(): Promise<void> {
  requireSafeChildProcessEnv(app.isPackaged);
  registerScheme();
  // Windows: the AppUserModelID, before any window or notification exists.
  applyAppUserModelId(app);
  await start();
}

launch().catch((error: unknown) => {
  console.error('conch-desktop: could not start:', error instanceof Error ? error.message : 'unknown error');
  process.exit(1);
});
