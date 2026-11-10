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
//   @cross  the subset Firefox and WebKit run (#1658), when CONCH_E2E_BROWSERS
//           names them; Chromium runs it with the rest.
//   @voice  needs a fake microphone: Firefox gets one from its prefs, WebKit
//           has none, so the webkit project leaves @voice out.
// Pass any of them through `make web-e2e E2E_ARGS="--grep @smoke"`.
//
// One worker, files in name order: the specs share one server and one
// community, and the last one restarts the server.
import { defineConfig, devices, type Project } from '@playwright/test';
import { toneFile } from './e2e/tone';

// Other engines (issue #1658): Playwright's own Firefox and WebKit builds,
// present only when CONCH_E2E_BROWSERS names them (a comma list: "firefox",
// "firefox,webkit"). Unset, `make web-e2e` and the pull request lane are
// Chromium only, as before. Each runs only the tests tagged @cross; with the
// variable set and no --project, Chromium runs as well. docs/testing.md has
// the capability table that says what each engine can do and how it was found.
//
// Firefox has no launch flags for fake devices; its user prefs do the same:
// a fake microphone and camera (a tone and a test card), no permission prompt,
// and no mDNS obfuscation of host candidates, so WebRTC to the LiveKit on
// 127.0.0.1 sees the host candidate. Its getDisplayMedia is not answered
// headless (the picker never closes), so no @cross test shares a screen.
//
// WebKit has no equivalent prefs: tests tagged @voice (they need a fake
// microphone) are left out of it. WebKit in Playwright is a build of the
// engine on Linux; it is not Safari, and says nothing about macOS or iOS.
const named = new Set((process.env['CONCH_E2E_BROWSERS'] ?? '').split(',').map((n) => n.trim()).filter((n) => n !== ''));
for (const n of named) {
  if (n !== 'firefox' && n !== 'webkit') throw new Error(`CONCH_E2E_BROWSERS: unknown engine "${n}" (firefox, webkit)`);
}

const crossProjects: Project[] = [];
if (named.has('firefox')) {
  crossProjects.push({
    name: 'firefox',
    grep: /@cross/,
    use: {
      ...devices['Desktop Firefox'],
      launchOptions: {
        firefoxUserPrefs: {
          'media.navigator.streams.fake': true,
          'media.navigator.permission.disabled': true,
          'media.peerconnection.ice.obfuscate_host_addresses': false,
        },
      },
    },
  });
}
if (named.has('webkit')) {
  crossProjects.push({
    name: 'webkit',
    grep: /@cross/,
    grepInvert: /@voice/,
    use: { browserName: 'webkit' },
  });
}

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
  // Chromium by default (issue #285), no phone sizes; Firefox and WebKit only on request (above).
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
    ...crossProjects,
  ],
});
