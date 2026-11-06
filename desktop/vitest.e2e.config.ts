import { defineConfig } from 'vitest/config';

// The live same-machine voice test (issue #1383, D3-12): the owner side of
// e2e/desktop-voice, which runs this with its environment (the origin, the
// credentials, the observer). Not a unit test and not run by npm test: use
// `make desktop-voice-e2e`. One file, one test at a time: the tests share one
// LiveKit room and one listener.
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
