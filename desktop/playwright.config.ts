// The packaged smoke test (issue #940). `make desktop-smoke` runs it against
// the unpacked build in dist/; CI runs it under xvfb-run in its own job.
// Playwright only connects to the app over CDP: it launches no browser of its
// own, so no browser download is needed.
import { defineConfig } from '@playwright/test';

export default defineConfig({
  testDir: 'tests/smoke',
  testMatch: '*.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  forbidOnly: process.env['CI'] !== undefined,
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: [['list']],
  outputDir: 'test-results',
});
