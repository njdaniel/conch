// Electron's main entry (vite.main.config.ts bundles it to out/main/main.cjs).
import { registerScheme, start } from './app.ts';

registerScheme();
start().catch((error: unknown) => {
  console.error('conch-desktop: could not start:', error instanceof Error ? error.message : 'unknown error');
  process.exit(1);
});
