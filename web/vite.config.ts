import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// conchd serves the build at /app/ (conchd serve --web-dir web/dist) with
// script-src 'self' and style-src 'self', so the build may contain no inline
// script or style. Hashed files go under dist/assets/, which conchd caches as
// immutable.
export default defineConfig({
  base: '/app/',
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
    include: ['src/**/*.test.{ts,tsx}'],
  },
});
