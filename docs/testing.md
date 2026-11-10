# Testing: the end-to-end suites

Which suite runs where, what it proves and what it does not, and the rules for
the tags, the pull request budget, flakes and artifacts (issue #1613, epic
#1607). The unit tests (`go test`, `cargo test`, Vitest) are not here: `make
check` runs them. Nothing below runs against production or the alpha by
default; every suite starts its own throwaway `conchd`.

## The suites

| Suite (CI job) | Lane | What it proves | What it does not | Local command | Artifacts |
| --- | --- | --- | --- | --- | --- |
| `web/e2e` in Chromium (`web-e2e`, three shards) | every pull request and push to `main`, with `--grep-invert @heavy` | the browser client against a fixture community: sign-in, messages, admin, browser voice, camera and screen share on Chromium's fake devices, with the pinned LiveKit in Docker | Firefox and WebKit (#1658); real microphones, cameras and screens; a LAN or a second machine; real AI agents (scripted ones only) | `make web-e2e` (once: `cd web && npx playwright install chromium`) | traces of failed tests, `web-e2e-test-results-N`, 7 days |
| `web/e2e` heavy lane (`e2e-heavy.yml`) | daily on `main`, and on demand for any branch | the tests tagged `@heavy`: those over the pull request budget, and deliberate fault or blackhole tests | the same limits as the row above | `make web-e2e E2E_ARGS="--grep @heavy"` (or `make web-e2e-heavy`) | traces of failed tests, `e2e-heavy-test-results`, 7 days |
| `voice-check` | every pull request | the voice control plane: a real `conchd`, the real `conch` CLI, three `conch-voice` clients and a real LiveKit; the core unaffected with LiveKit down | a human's ears; real audio devices; PipeWire | `go run ./e2e/voice` (Docker) | the log |
| `desktop-voice-check` | every pull request | the Linux desktop shell's voice and screen companion against the web client and LiveKit | a real display or sound card; a second machine (#1584) | `make desktop-voice-e2e` (Docker) | the log |
| `isolation-check` | every pull request | access between groups: what a principal outside a group can and cannot reach, with LiveKit rooms | the browser; more than the scripted principals | `go run ./e2e/isolation` (Docker) | the log |
| `community-check` | every pull request | the community model's newcomer scenario: invite, sign-in, default channel, a private channel (separate principals) | the browser's rendering of it | `go run ./e2e/community` (Docker) | the log |
| `p3-local-check` | every pull request | the P3 fixture community, on loopback and then non-loopback | anything beyond one machine's network stack | `go run ./e2e/p3-local`, then `go run ./e2e/p3-local --non-loopback` | the log |
| `p2-hardening-check` | every pull request | routes, audit rows, credentials and their hardening | the browser | `go run ./e2e/p2-hardening` | the log |
| `nets-check` | every pull request | nets and whispers over the public API | voice media | `go run ./e2e/nets` | the log |
| `dogfood-check` | pull requests labelled `approval-path`, and every push to `main` | the approval chain: an agent over MCP requests, ntfy fires, a human resolves with `conch approve`, the audit chain is asserted | a real AI agent (scripted); a phone's notification | `go run ./e2e/dogfood` | the log |
| `conch-bot-check` | every pull request | the reference bot as a client of the public API | other bots | `go run ./e2e/conch-bot` | the log |
| `desktop-smoke` | every pull request | the packaged desktop app starts under a virtual display and serves the web client | real windowing, the installed `.deb`, other distributions (#1603) | `xvfb-run -a make desktop-smoke` (after `make desktop-build`) | the log |

Docs-only pull requests run none of these (`scripts/ci-docs-only.sh`).

Two limits hold for every row. Principals in these suites are separate
principals or sessions of one principal as each suite says, but all run on one
machine; none crosses a LAN or a VM. Audio and video are fake devices (a tone
file, Chromium's test pattern), so a green run says the media path connects and
flows, not that anyone can hear or see well.

## By-hand coverage that automation does not replace

- #474: the alpha's browser flows in real Chrome and Firefox.
- #1584: desktop share and watch on Pop!_OS with a second machine.
- #1603: Linux desktop release acceptance: mixed clients, two servers, gaming
  push-to-talk, share and watch, every failure path.
- #448 and #1513: screen share to many viewers, and multi-share measurements,
  on the droplet.

## Tags

A test carries tags in Playwright's `tag` option:
`test('name', { tag: '@heavy' }, async ({ page }) => { ... })`. Three exist
(`web/playwright.config.ts` repeats this):

- `@heavy` runs only in the heavy lane. Use it for a test over the pull request
  budget below, or a deliberate fault or blackhole test. The `web-e2e` job runs
  with `--grep-invert @heavy`, so a tagged test is never in a pull request
  shard; `e2e-heavy.yml` runs `--grep @heavy`.
- `@smoke` marks the fast subset for a quick local run: `make web-e2e
  E2E_ARGS="--grep @smoke"`.
- `@cross` marks the subset other browser engines run (#1658). Only Chromium
  is configured today.

Untagged tests stay in the pull request lane.

## Lanes and cost

- **Pull request lane:** `web-e2e`, three shards on `ubuntu-latest`, bounded at
  30 minutes. Budget: a new LiveKit spec file adds at most about 2 minutes to
  its shard, and a shard stays under about 20 minutes. A spec over that is
  `@heavy`.
- **Heavy lane:** `.github/workflows/e2e-heavy.yml` on `ubuntu-latest`, daily
  (GitHub runs schedules on `main` only) and on `workflow_dispatch`. To run it
  on a branch: Actions, e2e-heavy, Run workflow, pick the branch (or `gh
  workflow run e2e-heavy.yml --ref <branch>`). It has its own concurrency group,
  a 40-minute bound and `contents: read` permission only. With no `@heavy` test
  yet it passes through `--pass-with-no-tests`. It does not use the
  self-hosted `HEAVY_RUNNER` path. The repository is public, so hosted minutes
  cost nothing; wall time and queue contention are the cost.
- Windows, macOS and engines other than Chromium are not in this lane.

## Flake policy

`retries: 0` in both lanes: a retry hides the flake. A failing heavy run is
triaged at the principal's next session. A test that fails twice in 7 heavy
runs with no related change gets an issue. It may be quarantined with
`test.fixme` and a link to that issue only if the pull request description
says so (CLAUDE.md, rule 6): never skip or delete a failing test silently.

## Artifact policy

- On failure each lane uploads `web/test-results` (Playwright traces) with
  7-day retention.
- A trace holds the run's fixture credentials (Bearer headers, the filled
  sign-in value), and Playwright cannot redact it. They are throwaway: the
  world is torn down (server stopped, root removed) before the upload, so a
  downloaded trace opens nothing.
- No `conchd` or LiveKit log is uploaded unless it has first been passed
  through the world's secret list. No secrets are printed to a log.
- Each LiveKit container is labelled with the run (`VOICE_CHECK_RUN`), and an
  `if: always()` step removes only that run's containers.
