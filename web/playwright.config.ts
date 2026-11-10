// The browser suite (issue #285): the web client in a real Chromium, against
// the fixture community that e2e/p3-local --keep sets up. `make web-e2e` runs
// it; it is not part of `make check`, which needs no browser.
//
// Tags (issue #1613; docs/testing.md has the policy). A test carries them in
// Playwright's `tag` option, e.g. test('x', { tag: '@heavy' }, async () => {}).
//   @heavy  runs only in the heavy lane (.github/workflows/e2e-heavy.yml,
//           nightly and on demand): a test over the PR budget, or a deliberate
//           fault or blackhole test. The PR lane (web-e2e in ci.yml) runs
//           with --grep-invert @heavy.
//   @smoke  the fast subset for a quick local run: --grep @smoke.
//   @cross  the subset other browser engines run (#1658); this file still
//           defines Chromium only.
// Pass any of them through `make web-e2e E2E_ARGS="--grep @smoke"`.
//
// One worker, files in name order: the specs share one server and one
// community, and the last one restarts the server.
import { defineConfig, devices } from '@playwright/test';
import { toneFile } from './e2e/tone';

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
  // A trace of a failed test holds this run's fixture credentials (the Bearer
  // headers in its network log, the value filled into the sign-in field), and
  // Playwright cannot redact them. They are throwaway: they die with the
  // server and the root in the global teardown, which runs before anyone can
  // open a trace.
  use: { trace: 'retain-on-failure' },
  // Chromium only (issue #285): no Firefox, no WebKit, no phone sizes.
  //
  // Browser voice (issue #442) runs on Chromium's fake microphone: the permission
  // prompt is answered, and the microphone plays a steady tone from a file, so
  // no test needs a real device. The local-IP flag keeps WebRTC to the LiveKit
  // on 127.0.0.1 from hiding host candidates behind mDNS names.
  //
  // Screen share (issue #444) runs on the same fake devices: the screen picker
  // is answered with a fake screen with a fake sound track, so no test needs a
  // real display. The fake screen captures at the size and rate it is asked
  // for: 1920x1080 at 30 fps by default (checked on the pinned Chromium).
  // The auto-select flag answers the picker the same way should a run use
  // Chromium's real picker (a headed run).
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        launchOptions: {
          args: [
            '--use-fake-ui-for-media-stream',
            '--use-fake-device-for-media-stream',
            `--use-file-for-fake-audio-capture=${toneFile()}`,
            '--auto-select-desktop-capture-source=Entire screen',
            '--disable-features=WebRtcHideLocalIpsWithMdns',
          ],
        },
      },
    },
  ],
});
