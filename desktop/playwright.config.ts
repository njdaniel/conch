// The packaged smoke test (issue #940). `make desktop-smoke` runs it against
// the unpacked build in dist/; CI runs it under xvfb-run in its own job.
// Playwright connects to the app over CDP; it never launches the app itself.
// The feeds spec (#943) also launches Playwright's own Chromium as a browser
// user beside the app: fetch it once with `cd desktop && npx playwright
// install chromium` (the same build web/'s suite uses, at the same version).
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
