# CLAUDE.md — Conch build rules

Conch is an MCP-native, self-hosted chat platform: `conchd` (server) + `conch` (CLI/TUI), Go, embedded SQLite. One more binary, the voice client `conch-voice`, is Rust and lives in its own workspace under `voice/` (ADR-006). Charter and authority table: [docs/adr/ADR-000-charter.md](docs/adr/ADR-000-charter.md). Roadmap: [ROADMAP.md](ROADMAP.md).

## Golden rules

1. **Deployment invariant.** Single-server, dependency-free core: `conchd` requires no external process for messaging, approvals, or audit. Any change introducing a *required* external process for core function is rejected. Clients (`conch` TUI/CLI) and optional runtime adapters run as separate processes; ntfy, Litestream, and later LiveKit are optional integrations that must degrade gracefully. (ADR-002)
2. **Schema-first.** All wire shapes — messages, payloads, approval objects, resolutions — come from `pkg/schema`. Never hand-roll JSON shapes elsewhere. Schema breaking changes need a version bump and Nick's sign-off; use the `schema-change` skill.
3. **Approval-path changes ship with an end-to-end test** exercising the full request → notify → resolve → audit chain. No exceptions. PRs touching the approval path are labeled `approval-path` and merged by Nick personally.
4. **API parity.** Anything the CLI/TUI can do exists in the REST/WS API first. `conch` is a client of the public API; agents get MCP; both front the same core. (ADR-001)
5. **One issue = one branch = one PR = one session.** Reference the issue in the PR. No drive-by changes outside the issue's scope — file a new issue instead.
6. **Idiomatic Go.** `golangci-lint` clean, table-driven tests. Never skip or delete a failing test without flagging it in the PR description.
   **And idiomatic Rust under `voice/`:** `cargo fmt` and `cargo clippy -D warnings` clean. No `unwrap` or `expect` outside tests and `main`; one `thiserror` enum per crate. No `unsafe` in this repository's Rust (every crate forbids it; native code is reached only through dependencies). Tests never need a microphone, PipeWire, a keyboard or LiveKit: one that does is marked `#[ignore]` and says what it needs. Every crate's `lib.rs` doc comment states its input, its output and what it owns.
7. **Issues must be executable cold** — a fresh session with zero chat context can pick one up. The issue template (Context / Files / Acceptance criteria / Required tests / Blocked-by) is normative.

## Working here

- Build/verify: `make check` (fmt, vet, lint, test, schema-compat, depgate, rust-check). Install local hooks once with `make hooks-install`. The Rust leg needs a pinned clang, cargo-deny and libwebrtc: fetch them once with `scripts/voice-toolchain.sh` (no root; into `~/.cache/conch`). Without them `make check` fails and says so; it does not skip.
- GitHub work (issues, PRs, milestones) goes through the `gh` CLI, not a GitHub MCP server.
- Security-sensitive PRs (authn/authz, tokens, capability enforcement, webhook ingest) get a `security-reviewer` agent pass before merge.
- New `go.mod` dependencies require Nick's sign-off and an entry in `deps-allowlist.txt` (enforced by `scripts/depgate.sh`). The same holds for Rust: a new direct dependency of a crate under `voice/` needs his sign-off and a line in `voice/deps-allowlist.txt` (enforced by `scripts/voice-depgate.sh`), and so does any change to `scripts/voice-pins.sh`, which pins the compiler and the prebuilt libwebrtc that `conch-voice` is built with.
- Who decides what: Fable (Principal Engineer) owns issues, reviews, refactors, and roadmap *proposals*; Nick (Tier-H) signs off on ADRs, schema version bumps, new dependencies, releases, roadmap changes, and merges approval-path PRs. Full table in ADR-000.
- Merge rule: green CI + reviewer approval on everything; approval-path PRs additionally merged by Nick himself.
