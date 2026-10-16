# Changelog

All notable changes to Conch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). A tag with a pre-release suffix
(`v0.1.0-alpha.1`) is published as a GitHub pre-release; a plain tag is a draft
release that Nick publishes.

## [Unreleased]

### Added

- Every build says which it is. Release builds report their tag, `make build`
  reports `git describe`, and a plain `go build` reports its commit. `conchd
  version`, `conch version`, `GET /healthz`, `GET /v1/whoami` (a new optional
  `server_version` field), `conch whoami` and the web client (sign-in page and
  the lobby footer) show it.
- Tags with a pre-release suffix produce a GitHub pre-release.
