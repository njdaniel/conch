import { defineConfig } from 'vitest/config';

// The unit tests the desktop-windows job runs (issue #1785): the ones written
// to hold on Windows as on Linux. The rest of tests/unit checks POSIX file
// modes, uids and Unix sockets in a private runtime directory, which have no
// Windows meaning; they run on the desktop job. Add a file here when it is
// written without those assumptions.
export default defineConfig({
  test: {
    environment: 'node',
    include: ['tests/unit/voice-windows.test.ts', 'tests/unit/voice-protocol.test.ts'],
  },
});
