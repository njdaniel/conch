---
name: release
description: The gated release procedure — preconditions, version choice, CHANGELOG, Nick-signed tag, GitHub release via release.yml (a draft for a plain tag, a published pre-release for a -suffix tag). Use for any tagged release; releases are Tier-H (Nick signs off, per ADR-000).
---

# release

Releases are Tier-H: Nick signs off, always (ADR-000). The tag push is the
sign-off act; `release.yml` turns a plain `v*` tag into a draft GitHub release, and a tag with a pre-release suffix (`v0.1.0-alpha.1`) into a published pre-release for testers.

## Preconditions — all four, verified fresh, before any tag

1. CI green on `main` (`gh run list --branch main`), including the
   dogfood-check and p2-hardening-check jobs.
2. `go run ./e2e/p2-hardening` passes locally against the release commit. It
   is the P2 exit gate (issue #81) and runs the P1 canary, `e2e/dogfood`, as
   its last step, so this one command is both. A release **does not ship**
   when it fails. What it proves and how to read a failure: the README
   section "The P2 hardening gate".
3. The latest principal-review report shows no unresolved invariant drift
   (open drift → fix or get Nick's explicit acceptance first).
4. **Nick's sign-off is recorded** — on the release issue or in writing.
   Session-scoped authorization counts only if it explicitly names releasing.

## Procedure

1. Pick the version: `v0.x.y` SemVer (session-1 decision — pre-1.0, tags only
   at Nick-gated releases). Minor for features, patch for fixes; pre-1.0
   breaking changes ride minor bumps but must be called out in the CHANGELOG.
2. Update `CHANGELOG.md` (Keep a Changelog format; create the file on the
   first release): move Unreleased into the new version section, dated. Land
   this via a normal PR before tagging — the tag points at a clean `main`.
3. Tag: annotated, on the reviewed `main` commit —
   `git tag -a vX.Y.Z -m "..."` — pushed **by Nick**, or by the session only
   under his explicit, recorded authorization for this release.
4. Verify the `release.yml` run: `make check`, dogfood-check and
   p2-hardening-check re-ran green, binaries built for all three platforms,
   the release created (a draft, or for a pre-release tag a published pre-release).
5. Sanity-check the artifacts (download one, `--version`, run against a temp
   db), note the dogfood-check run in the release notes, hand the draft to
   Nick to publish (a pre-release is already published: check its notes and tester checklist).

## Never

- Tag on a failing or stale dogfood-check or p2-hardening-check.
- Tag without recorded sign-off, or move/delete a pushed tag.
- Edit a published release's artifacts — regressions get a new patch release.
