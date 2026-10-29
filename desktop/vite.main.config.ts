// Bundles main (and, later, web/src/schema) into one CommonJS file. Electron
// and Node's own modules stay external; nothing from node_modules is copied
// into the app, because there are no runtime dependencies.
import { defineConfig } from 'vite';

export default defineConfig({
  build: {
    target: 'node24',
    outDir: 'out/main',
    emptyOutDir: true,
    minify: false,
    sourcemap: false,
    lib: { entry: 'main/main.ts', formats: ['cjs'], fileName: () => 'main.cjs' },
    rollupOptions: { external: ['electron', /^node:/] },
  },
});
