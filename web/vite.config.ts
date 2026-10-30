import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// conchd serves the build at /app/ (conchd serve --web-dir web/dist) with
// script-src 'self' and style-src 'self', so the build may contain no inline
// script or style. Hashed files go under dist/assets/, which conchd caches as
// immutable.
//
// The platform (src/platform) is chosen here, not at run time: a normal build
// aliases #platform-selected to the browser's, and only `--mode desktop`
// aliases it to the desktop shell's, so the browser bundle holds no desktop
// code. tsconfig.json's paths type-checks the browser's.
export default defineConfig(({ mode }) => ({
  base: '/app/',
  resolve: {
    alias: {
      '#platform-selected': fileURLToPath(new URL(`./src/platform/${mode === 'desktop' ? 'desktop' : 'browser'}/index.ts`, import.meta.url)),
    },
  },
  plugins: [react()],
  build: {
    outDir: 'dist',
    assetsDir: 'assets',
    emptyOutDir: true,
    // Never turn a small file into a data: URI: the CSP allows data: for
    // images only, so an inlined font or script would be refused.
    assetsInlineLimit: 0,
  },
  test: {
    environment: 'jsdom',
    // The e2e helpers' own analysis is unit-tested too (e2e/onset.test.ts);
    // Playwright runs only *.spec.ts.
    include: ['src/**/*.test.{ts,tsx}', 'e2e/**/*.test.ts'],
  },
}));
