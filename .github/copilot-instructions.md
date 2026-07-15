# Copilot instructions for Conch

Conch is an **open-source, MCP-native, self-hosted chat platform** where AI agents are first-class citizens. Humans connect via a Go CLI/TUI (`conch`); agents connect via MCP. Both front the same core: one message log, one audit trail. The project is pre-alpha (P0 spike landed; P1 in progress).

---

## Quick-start for a cold session

```sh
# Verify everything before touching code
make check          # fmt-check → vet → lint → test → schema-compat → depgate

# Build both binaries
make build          # produces bin/conchd and bin/conch

# Run tests only
go test -race ./...

# Format
gofmt -w .
```

`make check` is the CI gate — run it before every commit. All six steps must be green. CI runs `go test -race ./...` (not plain `go test`), so always test with `-race` locally too.

---

## Repository layout

```
cmd/
  conchd/         main.go — server binary entry point
  conch/          main.go — CLI/TUI client entry point
internal/
  server/         HTTP server, mux, handlers
    approvals/    approval state-machine manager (timers, escalation)
    hub/          WebSocket fan-out hub
    store/        SQLite layer (migrations, queries)
  cli/
    tui/          Bubble Tea TUI model
pkg/
  schema/         *** THE SINGLE SOURCE OF TRUTH FOR ALL WIRE SHAPES ***
    testdata/     Golden JSON fixtures — immutable once published
docs/
  adr/            Architecture Decision Records (governing documents)
  design/         Design docs (approval-object.md is the approval bible)
scripts/
  schema-compat.sh  Fails if golden fixtures are modified
  depgate.sh        Fails if a direct go.mod dep is not in deps-allowlist.txt
.github/
  workflows/ci.yml
  ISSUE_TEMPLATE/task.md       (normative issue format)
  pull_request_template.md     (normative PR format)
```

---

## Non-negotiable golden rules

Violating any of these is a hard review blocker, not a style comment.

### 1. Single-binary invariant (ADR-002)

Core function (messaging, approvals, audit) requires **no external processes**. `conchd` alone, pointed at a data directory, is a complete working system. Integrations (ntfy, Litestream) are optional and must degrade gracefully — if they are down, the approval lifecycle is unaffected. Any change introducing a *required* external process is rejected.

### 2. Schema-first (ADR-000 D8)

**All wire shapes** — message envelopes, typed payloads, approval objects, resolution events, API request/response bodies — live in `pkg/schema`. Never hand-roll JSON shapes elsewhere. The store, handlers, and MCP layer are all projections of `pkg/schema`, never a second source of truth.

Breaking changes to `pkg/schema` (modifying or deleting an existing type or JSON key in a published version) require:
- a **new versioned type** (e.g. `foo_v2.go`, new golden fixture `foo-v2.json`)
- the **`schema-change` skill** (see `.claude/skills/schema-change/`)
- **Nick's sign-off**

Adding a new type or fixture is fine without a version bump.

### 3. Approval-path changes ship with an end-to-end test

Any PR touching the approval path (request → notify → resolve → audit chain) must:
- include a full-chain E2E test exercising all four links
- carry the `approval-path` label
- be merged by **Nick personally** (not just approved)

"Approval path" means: `internal/server/approvals/`, `internal/server/approvals_handlers.go`, `internal/server/store/approvals.go`, the ntfy notification path, and any audit-event writes for approval events.

### 4. API parity (ADR-001)

Anything the CLI/TUI can do exists in the REST/WS API first. `conch` is an ordinary client of the public API — no private backdoor. Agents get MCP; MCP tools map to the same core operations the REST/WS API fronts. A PR adding a CLI capability without its API equivalent is an invariant violation.

### 5. One issue = one branch = one PR = one session

Reference the issue number in every PR (the PR template has a `Closes #` field). No drive-by changes outside the issue scope — file a new issue instead.

### 6. Idiomatic Go

- `golangci-lint` clean (standard linters + `misspell`, `unconvert`, `unparam`, `nilerr`, `errorlint`, `copyloopvar`, `gosec`)
- Table-driven tests throughout
- Never skip or delete a failing test without flagging it in the PR description

### 7. Issues must be self-contained

A cold session with zero chat context must be able to execute an issue. The normative template fields are: **Context** / **Files** / **Acceptance criteria** / **Required tests** / **Blocked-by**.

---

## Governance — who signs off on what

| Decision type | Owner |
|---|---|
| Issue creation, sequencing, grooming | Fable (Principal Engineer / the agent) |
| Code review verdicts | Fable |
| Refactors within existing ADRs | Fable |
| New or amended ADRs | **Nick (Tier-H) sign-off required** |
| `pkg/schema` version bumps | **Nick sign-off required** |
| New `go.mod` direct dependencies | **Nick sign-off required** |
| Releases | **Nick sign-off required** |
| Roadmap changes | **Nick sign-off required** |
| Approval-path PR merges | **Nick merges personally** |

Use `gh` CLI for GitHub operations (issues, PRs, milestones) — not the GitHub MCP server (ADR-000 D15).

---

## Dependencies

Adding a new direct dependency requires:
1. Nick's explicit sign-off
2. Adding the module path to `deps-allowlist.txt` (one path per line) in the same PR

`scripts/depgate.sh` reads `go.mod` direct requirements and fails CI if any are absent from `deps-allowlist.txt`. Currently approved direct deps:
- `modernc.org/sqlite` — pure-Go SQLite, WAL mode, no cgo (founding decision D2)
- `github.com/charmbracelet/bubbletea` — TUI framework
- `github.com/charmbracelet/lipgloss` — TUI styling
- `github.com/coder/websocket` — WebSocket hub

The MCP SDK is **not yet chosen or added** — it is a P1 item. The protocol-designer role (`.claude/agents/protocol-designer.md`) selects the SDK using the `mcp-sdk-selection` design doc; Nick must sign off before it lands in `go.mod` and `deps-allowlist.txt`.

---

## Schema golden fixtures

`pkg/schema/testdata/` holds JSON golden fixtures for every published schema version. These are **immutable once committed to `main`**: modifying or deleting an existing fixture is a breaking change and hard-fails CI (`scripts/schema-compat.sh` diffs them against the merge base). Published fixtures as of now:

- `channel-v0.json`, `create-channel-{request,response}-v0.json`
- `create-principal-{request,response}-v0.json`
- `list-messages-response-v0.json`
- `conch-approval-v1.json`, `create-approval-{request,response}-v1.json`
- `approval-decision-v1.json`, `approval-option-v1.json`
- `approval-resolution-v1.json`, `approval-resolution-v1-expired.json`
- `cast-decision-{request,response}-v1.json`
- `list-approvals-response-v1.json`
- `create-hook-{request,response}-v1.json`
- `leviathan-trade-signal-v1.json`
- `error.json`

---

## API surface (current)

`conchd` listens on HTTP. Versioned routes:

| Method | Path | Description |
|---|---|---|
| GET | `/healthz` | Health check (returns `schema.Health`) |
| GET | `/v0/ws` | WebSocket (v0 message fan-out) |
| GET | `/v1/ws` | WebSocket (v1 message fan-out) |
| POST | `/v0/channels` | Create channel |
| POST | `/v0/principals` | Create principal |
| POST | `/v0/channels/{channel}/messages` | Post message (v0) |
| GET | `/v0/channels/{channel}/messages` | List messages (v0) |
| POST | `/v1/channels/{channel}/messages` | Post message (v1, typed payload) |
| GET | `/v1/channels/{channel}/messages` | List messages (v1) |
| POST | `/v1/hooks` | Create webhook token |
| POST | `/v1/hooks/{token}` | Ingest webhook message |
| POST | `/v1/approvals` | Create approval object |
| GET | `/v1/approvals` | List open approvals |
| POST | `/v1/approvals/{id}/decisions` | Cast a decision |

---

## Database schema (SQLite, WAL, pure Go)

Migrations live inline in `internal/server/store/store.go`. Migrations are append-only — never edit or reorder a committed migration; only append new ones. Schema version = number of migrations applied (`PRAGMA user_version`). Current migrations:

1. `principals`, `channels`, `messages`, `audit_events` (append-only enforced by triggers)
2. Typed payload columns (`payload_schema`, `payload_json`) on `messages`
3. `approvals`, `approval_decisions`, `approval_resolutions` (P1 approval state machine)
4. `hooks` (webhook token → channel + principal)

`audit_events` has `BEFORE UPDATE` and `BEFORE DELETE` triggers that `RAISE(ABORT, ...)` — the table is physically append-only, not just by convention.

---

## Approval state machine

States: `pending` → `escalated` → `resolved` (terminal) or `expired` (terminal). The design doc is `docs/design/approval-object.md`. Key invariants:
- Terminal states never transition.
- Every state transition writes an audit event **in the same transaction**.
- Decisions serialize through the SQLite single-writer; quorum check is inside the transaction.
- A decision cast against a resolved or expired approval is a protocol error (`409 Conflict`).
- Decisions are cast only by **human** principals; agents request and observe.
- `reason` is required and non-empty on every decision (enforced at the API layer and the DB `CHECK` constraint).

---

## PR checklist (from `.github/pull_request_template.md`)

Before opening a PR, verify:
- [ ] No required external process introduced (single-binary invariant, ADR-002)
- [ ] All wire shapes come from `pkg/schema`; schema changes followed the `schema-change` skill
- [ ] API parity holds — no CLI/TUI/MCP capability without a REST/WS equivalent
- [ ] No failing test skipped or deleted without being flagged
- [ ] Scope limited to the referenced issue
- [ ] If the approval path is touched: `approval-path` label applied, full-chain E2E test included, Nick merges

---

## Common pitfalls

- **Do not use the GitHub MCP server for issues/PRs** — use `gh` CLI (ADR-000 D15).
- **Do not add a `go.mod` dependency** without Nick's sign-off and an entry in `deps-allowlist.txt` first; CI will fail immediately.
- **Do not modify an existing golden fixture** in `pkg/schema/testdata/` — add a new versioned one instead.
- **Do not hand-roll JSON structs** for any wire type outside `pkg/schema`.
- **Do not introduce goroutines or background workers outside `conchd`** — all background processing (timers, escalation) runs as goroutines inside the server process.
- **Do not skip `go test -race`** — CI always runs with `-race`; data races will catch you there if not locally.
- **Approval-path PRs are not self-mergeable** — flag them and wait for Nick even if CI is green and Fable has approved.
