// The UI pass's capture run (.claude/skills/ui-pass): not a test suite. It
// drives the real client through a flow's states against the e2e world and a
// real LiveKit, and screenshots each person at each state. It never runs in
// CI: the default config matches *.spec.ts only, and these files are *.ui.ts.
//
//   cd web && npx playwright test -c playwright.ui-pass.config.ts
//   CONCH_UI_FLOWS=talk,stream (default)  CONCH_UI_OUT=<dir> (default test-results-ui/shots)
import { defineConfig } from '@playwright/test';
import base from './playwright.config';

// Local only: refuse before global setup builds anything.
if (process.env['CI'] !== undefined) {
  throw new Error('the ui-pass capture is local only; it does not run in CI');
}

export default defineConfig({
  ...base,
  testMatch: 'ui-pass/*.ui.ts',
  outputDir: 'test-results-ui/playwright',
  timeout: 600_000,
  retries: 0,
  // A control that isn't there fails its step quickly instead of hanging the capture.
  use: { ...base.use, actionTimeout: 20_000 },
  projects: (base.projects ?? []).filter((p) => p.name === 'chromium'),
});
