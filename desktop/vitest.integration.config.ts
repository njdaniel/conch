import { defineConfig } from 'vitest/config';

// Integration tests: the real conch-voice (make rust-build) against a stub
// conchd on loopback. Labelled and run apart from the unit tests
// (npm run test:integration, make desktop-voice-it); no Electron, no audio.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['tests/integration/**/*.test.ts'],
    testTimeout: 60_000,
  },
});
