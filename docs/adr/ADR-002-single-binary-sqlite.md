# ADR-002: Deployment invariant and embedded SQLite

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Nick (Tier-H); formalizes founding decisions D2 and D3 ([ADR-000](ADR-000-charter.md))

## Context

Conch targets self-hosters and small orgs running agent operations. Its credibility in that niche rests on being trivial to deploy and impossible to half-deploy: one process, one file of state, no orchestration. Every external runtime dependency multiplies operational failure modes and shrinks the plausible install base.

## Decision

### Deployment invariant (D3), formerly "single-binary invariant"

**Single-server, dependency-free core: messaging, approvals, audit require no external process.** `conchd` alone, pointed at a data directory, is a complete working system. Clients (`conch` TUI/CLI) and optional runtime adapters run as separate processes — that was always true of a client/server split and is not a reversal of the invariant; the rename just makes that explicit instead of implying "conchd is the only process that may ever run."

Integrations are optional and must **degrade gracefully**:

- **ntfy** (approval push notifications, D7): if unreachable, approvals still work — creation, listing, resolution, audit are unaffected; the notification failure itself is recorded in the audit log.
- **Litestream** (streaming backup): an optional sidecar the operator may run; conchd neither knows nor cares.
- **LiveKit** (voice/PTT, screen sharing — P8/P9, each requiring its own future ADR): an optional second server process; text/approval core runs without it.

Review rule: any change introducing a *required* external process for core messaging, approvals, or audit is rejected outright (CLAUDE.md rule 1). "Required" means core function breaks without it.

### Embedded SQLite via modernc.org/sqlite (D2)

- **Driver:** `modernc.org/sqlite` — pure Go, no cgo. Cross-compilation stays trivial (`GOOS`/`GOARCH` builds with no C toolchain), which is what makes single-binary distribution real.
- **Mode:** WAL, for concurrent readers alongside the single writer — the right shape for a chat server (many readers, serialized writes to an append-heavy log).
- **Search:** FTS5 for full-text message search (P2).
- **Backup story:** SQLite's single-file database + optional Litestream replication.
- **Postgres:** a possible future driver behind a storage interface, explicitly **not MVP**. We do not design abstractions for it now beyond ordinary layering hygiene.

## Consequences

- Deployment is `scp conchd server: && ssh server ./conchd serve` — the demo and the docs stay honest about this.
- Write throughput is bounded by SQLite's single-writer model. Acceptable for the target scale (one org); if it ever isn't, that's the P0 kill criterion / a future ADR, not a silent workaround.
- No cgo means no fights with musl/alpine/cross-builds, at the cost of `modernc.org/sqlite` being somewhat slower than the C driver. Accepted trade.
- The test suite can run entirely in-process with temp databases — no test containers.
- Anything needing background processing (deadline expiry, escalation timers) runs as goroutines inside conchd, never as cron/external workers.
