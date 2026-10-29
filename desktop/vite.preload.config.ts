// Bundles the preload. A sandboxed preload can require only 'electron' (and a
// few Electron-provided modules), not local files, so it is one file.
import { defineConfig } from 'vite';

export default defineConfig({
  build: {
    target: 'node24',
    outDir: 'out/preload',
    emptyOutDir: true,
    minify: false,
    sourcemap: false,
    lib: { entry: 'preload/index.ts', formats: ['cjs'], fileName: () => 'index.cjs' },
    rollupOptions: { external: ['electron'] },
  },
});
