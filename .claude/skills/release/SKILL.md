---
name: release
description: The gated release procedure — preconditions, version choice, CHANGELOG, Nick-signed tag, draft GitHub release via release.yml. Use for any tagged release; releases are Tier-H (Nick signs off, per ADR-000).
---

# release

Releases are Tier-H: Nick signs off, always (ADR-000). The tag push is the
sign-off act; `release.yml` turns a `v*` tag into a draft GitHub release.

## Preconditions — all four, verified fresh, before any tag

1. CI green on `main` (`gh run list --branch main`), including the
   dogfood-check job.
2. `go run ./e2e/dogfood` passes locally against the release commit — the
   ROADMAP success criterion is the release canary; a release **does not
   ship** on a failing dogfood-check.
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
4. Verify the `release.yml` run: `make check` + dogfood-check re-ran green,
   binaries built for all three platforms, draft release created.
5. Sanity-check the artifacts (download one, `--version`, run against a temp
   db), note the dogfood-check run in the release notes, hand the draft to
   Nick to publish.

## Never

- Tag on a failing or stale dogfood-check.
- Tag without recorded sign-off, or move/delete a pushed tag.
- Edit a published release's artifacts — regressions get a new patch release.
