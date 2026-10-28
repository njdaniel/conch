# Changelog

All notable changes to Conch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). A tag with a pre-release suffix
(`v0.1.0-alpha.1`) is published as a GitHub pre-release; a plain tag is a draft
release that Nick publishes.

## [Unreleased]

## [0.1.0-alpha.4] - 2026-10-28

Upgrade with `conch-upgrade.sh` (docs/deploy.md step 11). There is no
migration, so a rollback to `alpha.3` keeps the data as it is.

### Added

- **The Conch icon in the browser tab** (#917): the app icon's dark tile, in
  place of a blank tab.

### Changed

- **A first password set without its enrolment code** now answers 403
  `enrollment_proof_required` instead of `reauth_required` (#815). The
  browser points at the enrolment-code field, and `conch password set` asks
  for `--proof`, instead of asking a member with no password to confirm
  their password.
- **`conch-voice` refuses an empty or space-padded `audio.input` or
  `audio.output`** in `voice.toml` as a bad configuration naming the key
  (#691). It used to silently mean the default device. Use `default`, or
  remove the key.

### Fixed

- **Push-to-talk always ends when the microphone stalls** (#686). If the
  microphone stopped delivering during the release tail, the transmission
  never ended and the status kept saying "transmitting". It now ends within
  the tail plus 50 ms.
- **The voice helper and the TUI agree on every control line** (#690). Both
  readers refuse a `\u` escape of an unpaired surrogate as malformed, and
  both refuse an empty `error`, `reason` or `ptt_problem`.

## [0.1.0-alpha.3] - 2026-10-28

A small build so an operator can resume invites from the browser. Upgrade
with `conch-upgrade.sh` (docs/deploy.md step 11). There is no migration, so a
rollback to `alpha.2` keeps the data as it is.

### Added

- **Pause and resume invites in the browser** (#882): a "Pause all invites" switch
  in Settings → Joining, beside the guest-joining pause. It does what
  `conch admin invite pause|resume` does. Restoring a backup pauses both, on
  purpose, and until now invites could only be resumed with the CLI.

### Fixed

- After a cancelled re-authentication, Settings → Joining no longer says it
  needs "a code" from an operator who has no authenticator.

## [0.1.0-alpha.2] - 2026-10-28

The second alpha build. Operators can sign in with a password alone, and
short-lived test servers have their own mode. Upgrade with
`conch-upgrade.sh` (docs/deploy.md step 11). Migration 36 adds the server's
mode record; a rollback to `alpha.1` restores the pre-upgrade backup.

### Added

- **Password-only operator sign-in** (ADR-012). With
  `CONCHD_OPERATOR_TOTP=optional` in the instance's env file, an operator who
  has not enrolled an authenticator signs in, re-authenticates and changes
  their password with the password alone. An operator who enrolled one is
  always asked for its code. The default stays `required`. Switching back to
  `required` signs operators' browser sessions out and logs how many
  operators still have no authenticator.
- **Temporary servers** (ADR-011). `CONCHD_MODE=temporary` makes a
  short-lived server:
  - password-only operators, as above;
  - every user sees "Temporary server: no backups, and it may be removed";
  - backups are local only, with no off-site copy.
- **Setup without an authenticator.** Where the server allows it, the
  browser's setup page offers **Skip** after the password, and `conch setup`
  asks "Set up an authenticator now? [y/N]".
- **Setup links.** `conchd operator setup --link --origin https://<host>`
  prints a link that opens the setup page with the token filled in; nothing
  is spent until you press Continue.
- **Replies in the browser**: one-level threads, with the audience inherited
  from the message replied to.
- **A private microphone test** in the voice panel: a level meter, and your
  own voice played back only if you ask, with nothing sent anywhere.

### Changed

- The browser and `conch` ask an operator for an authenticator code at
  re-authentication and password change only when one is enrolled.

### Fixed

- **Security:** rotating a browser session
  (`POST /v1/credentials/{id}/rotate`) produced a credential that was no
  longer treated as a session, so it skipped recent re-authentication for up
  to its 12-hour expiry. A rotated session now stays a session.
- A password-only operator's browser sign-in is kept for that tab instead of
  being revoked straight after.
- The deployment runbook now matches the first real droplet run.

## [0.1.0-alpha.1] - 2026-10-27

The first tagged build: the private communications alpha (ADR-008), deployed
on one droplet for invited testers. Pre-1.0: wire and storage formats may
change between alpha builds. What testers can expect, and what does not work
yet, is in [docs/alpha-tester-guide.md](docs/alpha-tester-guide.md).

### Added

- **Browser client** at `/app/`, served by `conchd`: channels, one-level
  threads, unread badges kept live, mentions, notifications and per-channel
  mute, search in the current channel, and pasted or attached images with
  private inline previews.
- **Voice and screen share** through an optional LiveKit server: voice
  channels with mute, deafen and push-to-talk, and screen share with sound.
- **Invites and onboarding** (ADR-009): personal invites and time-limited open
  links; temporary guests; password accounts made directly or by converting
  a guest; return sign-in from a new browser; sign out everywhere.
- **Operators** sign in in the browser with a password and an authenticator
  code, re-authenticate for sensitive actions, and recover a lost account
  from the host (`conchd operator recover`).
- **Delegated administration** (ADR-010): a server owner, delegated
  operators, and channel managers who manage their own channel's members and
  name, in the browser and through `conch admin`.
- **Isolation**: each person sees only the channels they belong to; guests
  never see history from before they were admitted, and cannot decide
  approvals.
- **Agents** connect over MCP, as before, beside people in the same channels.
- **Deployment** on one droplet (`docs/deploy.md`): systemd units, Caddy,
  LiveKit with TURN/TLS, nightly encrypted off-site backups, upgrade, rollback
  and a restore drill.
- Every build says which it is. Release builds report their tag, `make build`
  reports `git describe`, and a plain `go build` reports its commit. `conchd
  version`, `conch version`, `GET /healthz`, `GET /v1/whoami` (a new optional
  `server_version` field), `conch whoami` and the web client (sign-in page and
  the lobby footer) show it.
- Tags with a pre-release suffix produce a GitHub pre-release.
