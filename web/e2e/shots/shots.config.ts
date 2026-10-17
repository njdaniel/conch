// Screenshots for design review (issue #536), not a test suite: the same
// seeded state photographed at a desktop and a phone size, so that a change to
// the look can be compared before and after. It runs on the browser suite's
// own community and voice world (which needs Docker), and nothing in CI runs
// it:
//
//   cd web && CONCH_SHOTS_DIR=../docs/design/browser-ui-polish CONCH_SHOTS_TAG=after \
//     npx playwright test -c e2e/shots/shots.config.ts
import { defineConfig } from '@playwright/test';
import base from '../../playwright.config';

export default defineConfig({
  ...base,
  testDir: '.',
  testMatch: '*.shots.ts',
  // Under web/test-results, which git ignores, as the suite's own output is.
  outputDir: '../../test-results/shots',
  globalSetup: '../global-setup.ts',
  globalTeardown: '../global-teardown.ts',
  timeout: 300_000,
});
