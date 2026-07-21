# Conch roadmap

Governed by [ADR-000](docs/adr/ADR-000-charter.md). Roadmap *changes* require Nick's sign-off; milestones and issues live on [GitHub](https://github.com/njdaniel/conch/milestones).

## P0 — Spike (~1 week)

`conchd` skeleton: SQLite/WAL schema, WS hub, one channel, messages flowing between a bare client and a curl-driven fake agent. CI up.

> **Kill criterion:** if realtime feels bad or the architecture fights back hard, stop and report.

## P1 — Core Loop

Typed messages, approval objects, MCP endpoint, minimal TUI + CLI, webhook ingest, ntfy notifications.

> **Success criterion (this is the demo and the whole pitch):** a Claude agent connects via MCP, posts a typed message, calls `request_approval`; Nick's phone buzzes via ntfy; Nick SSHes in, sees it in the TUI approvals inbox, approves with a reason; the agent's `await_decision` resolves with the structured outcome; the audit log shows the full chain.

## P2 — Hardening

Agent identity manifests + server-side capability enforcement, audit export, FTS5 search, file uploads, auth (OIDC + local), threads, `principal-review` running on cadence, `conch-bot` auto-reply agent (#71, added 2026-07-21 by Nick's direct request).

## P3 — Only if earned

DMs, mobile PWA/push, LiveKit voice, Matrix bridge, web UI.
