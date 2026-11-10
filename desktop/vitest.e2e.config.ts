import { defineConfig } from 'vitest/config';

// The live same-machine voice tests (issue #1383, D3-12: voice-live.test.ts;
// issue #1583, D4-06: companion-live.test.ts): the owner side of
// e2e/desktop-voice, which runs this with its environment (the origin, the
// credentials, the observer). Not unit tests and not run by npm test: use
// `make desktop-voice-e2e`. One file, one test at a time: the tests share one
// LiveKit room and one listener. page/ holds the companion page's source (a
// Vite entry the second file builds), not a test.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['tests/e2e/**/*.test.ts'],
    testTimeout: 120_000,
    hookTimeout: 60_000,
    reporters: ['verbose'],
    fileParallelism: false,
    sequence: { concurrent: false },
  },
});
