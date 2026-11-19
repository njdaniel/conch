// Electron's main entry (vite.main.config.ts bundles it to out/main/main.cjs).
import { app } from 'electron';

import { applyAppUserModelId } from './app-user-model.ts';
import { registerScheme, start } from './app.ts';

registerScheme();
// Windows: the AppUserModelID, before any window or notification exists.
applyAppUserModelId(app);
start().catch((error: unknown) => {
  console.error('conch-desktop: could not start:', error instanceof Error ? error.message : 'unknown error');
  process.exit(1);
});
