// The browser suite (issue #285): the web client in a real Chromium, against
// the fixture community that e2e/p3-local --keep sets up. `make web-e2e` runs
// it; it is not part of `make check`, which needs no browser.
//
// One worker, files in name order: the specs share one server and one
// community, and the last one restarts the server.
import { defineConfig, devices } from '@playwright/test';

export default defineConfig({
  testDir: 'e2e',
  testMatch: '*.spec.ts',
  globalSetup: './e2e/global-setup.ts',
  globalTeardown: './e2e/global-teardown.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: process.env['CI'] !== undefined,
  timeout: 90_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: 'test-results',
  use: { trace: 'retain-on-failure' },
  // Chromium only (issue #285): no Firefox, no WebKit, no phone sizes.
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
});
