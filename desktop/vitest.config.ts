import { defineConfig } from 'vitest/config';

// Unit tests need no Electron runtime, no display and no network: Electron is
// mocked where a module imports it.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['tests/unit/**/*.test.ts'],
  },
});
