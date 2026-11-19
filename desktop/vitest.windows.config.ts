import { defineConfig } from 'vitest/config';

// The unit tests the desktop-windows job runs (issue #1785): the ones written
// to hold on Windows as on Linux. The rest of tests/unit checks POSIX file
// modes, uids and Unix sockets in a private runtime directory, which have no
// Windows meaning; they run on the desktop job. Add a file here when it is
// written without those assumptions. #1935 adds the shared lifecycle rules
// and Windows adapter/close/tray/Quit tests; hardware remains F14 (#903).
// #1957 adds shared notification ownership and lock-redaction coverage.
export default defineConfig({
  test: {
    environment: 'node',
    include: [
      'tests/unit/voice-windows.test.ts',
      'tests/unit/voice-protocol.test.ts',
      'tests/unit/app-user-model.test.ts',
      'tests/unit/data-root.test.ts',
      'tests/unit/lifecycle-windows.test.ts',
      'tests/unit/lifecycle.test.ts',
      'tests/unit/notifications.test.ts',
    ],
  },
});
