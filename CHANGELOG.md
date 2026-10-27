# Changelog

All notable changes to Conch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). A tag with a pre-release suffix
(`v0.1.0-alpha.1`) is published as a GitHub pre-release; a plain tag is a draft
release that Nick publishes.

## [Unreleased]

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
