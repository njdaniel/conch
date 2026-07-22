# Conch roadmap (long horizon)

Governed by [ADR-000](docs/adr/ADR-000-charter.md). Roadmap *changes* require Nick's
sign-off; milestones and issues live on [GitHub](https://github.com/njdaniel/conch/milestones).

**Destination.** Conch grows from an agent-native chat-ops tool into a communications
platform where humans and AI agents are equally first-class principals on one audited
log — a "Slack 2.0" whose differentiator is *agents as persistent, governed workspace
members*, with Discord-style real-time features (PTT, screen sharing) added at the end
via LiveKit, never bespoke WebRTC.

**Two invariants hold the whole way:**

- **Usable at every phase.** Each phase ends in a deployment you actually run, not
  scaffolding. The MVP and deployment shape are stated per phase. If a phase only makes
  sense once a *later* phase exists, it's mis-ordered.
- **TUI/local-first.** The terminal + dependency-free core + local-only mode carries the
  early arc. GUI, multi-user, and media are *earned* additions, gated behind their own
  ADRs because they reverse standing non-goals.

**Deployment invariant (replaces the old "single binary" wording).**
*Single-server, dependency-free core:* `conchd` requires no external service for
messaging, agents, approvals, or audit — no mandatory Postgres, Redis, broker, object
store, or media server; SQLite is embedded. Clients (`conch` TUI/CLI) and optional
runtime adapters run as separate processes. ntfy, Litestream, and later LiveKit are
optional integrations that degrade gracefully (ADR-002).

The load-bearing enabler is **API parity** (ADR): anything a client can do exists in the
REST/WS API first, so every later front-end (dashboard GUI, Slack-style GUI, mobile) is a
peer onto the same core, not a rewrite.

**Principals.** Humans and agents are *equal* first-class principals on the same log,
but *not the same kind*. They carry distinct identity types and capability models —
different auth, permissions, presence metadata, rate limits, approval rules,
impersonation protections, and lifecycle behavior.

---

## Shipped / in flight

### P0 — Spike ✅
`conchd` skeleton: SQLite/WAL schema, WS hub, one channel, messages flowing between a
bare client and a curl-driven fake agent. CI up.

### P1 — Core Loop ✅
Typed messages, approval objects, MCP endpoint, minimal TUI + CLI, webhook ingest, ntfy
notifications. The demo: an agent posts a typed message via MCP, calls
`request_approval`; Nick's phone buzzes; Nick SSHes in, approves with a reason in the
TUI; `await_decision` resolves; the audit log shows the full chain.

### P2 — Hardening 🔨 *(in progress)*
Agent identity manifests + server-side capability enforcement, audit export, FTS5
search, file uploads, auth (OIDC + local), threads, `principal-review` on cadence, the
first runtime adapter (see P3).

> **Why P2 gates everything below:** capability manifests and real auth are
> prerequisites for agent sessions (P3), floor control (P4), and multi-human access (P7).
> Don't skip ahead of them.

*Deployment: dependency-free core, localhost, Nick + one or more agents in the TUI.*

---

## The long arc

### P3 — Agent presence & runs 🎯 *next*
Make agents visible, first-class **workspace members**, not just participants that read
and post. This is the "open Conch and see everything running" phase — the most
immediately useful thing on the roadmap.

Ships:
- **Persistent agent identities + ephemeral sessions.** An agent principal is durable;
  a session is one live connection with a runtime, model, and host.
- **Registration + heartbeats.** Agents register a session and heartbeat; missed
  heartbeats flip status to offline.
- **Runtime/model/host metadata** per session.
- **Live status + current task:** online / idle / running / blocked / waiting / failed /
  offline, plus what it's working on and when it last acted.
- **Run lifecycle:** queued → starting → running → blocked → waiting →
  completed / failed / cancelled, linked to a GitHub issue/branch/PR/project where known.
- **Structured activity events** on a runtime-neutral contract, e.g.:
  ```json
  {
    "schema": "conch.agent.activity.v1",
    "agent": "codex-worker-1",
    "run": "run-418",
    "kind": "command_finished",
    "summary": "go test ./internal/server",
    "outcome": "failed"
  }
  ```
- **Agents view + Runs view** in the TUI.
- **Reference runtime adapters.** `conch-bot` is recast as *one* reference adapter, not
  the model. Adapters translate runtime-specific events into the neutral contract:
  `conch-adapter-claude`, `conch-adapter-codex`, `conch-adapter-local`, plus a generic
  subprocess adapter and a generic webhook/event adapter. They need not be separate
  repos/executables at first — the contract is the point. Without this layer Conch
  becomes good at hosting Claude auto-reply bots rather than managing heterogeneous
  agents.
- **Intervention capabilities**, declared per adapter: message / cancel / pause / resume.

> **Shippable MVP (run it tomorrow):** register + heartbeat + status + one TUI Agents
> view + one adapter. Richer run timelines, more adapters, and full intervention follow.

> **Success criterion:** Nick opens Conch and sees Claude Code, Codex, and a local model
> running simultaneously, sees what each is doing, opens each activity stream, and sends
> an instruction to one — without switching terminals.

*Deployment: dependency-free core, local, Nick + his agents in the TUI. No new infra.*

> **Independent track — dashboard GUI.** The agent roster / run timelines / activity
> streams are the one surface genuinely better as pixels than in a terminal. A minimal,
> read-only dashboard GUI over the existing API may be built *whenever it earns its
> place* — as early as alongside P3 — and is deliberately **decoupled** from the
> Slack-style multi-human GUI in P7. The TUI stays first-class for ops and approvals.

### P4 — Assemblies (floor-controlled multi-agent rooms)
Multiple agent principals in one channel reading the **shared** log, so each sees the
others' contributions. Brainstorm with several models at once instead of tabbing between
siloed chats.

Floor control is a **channel/session mode**, not universal behavior:

```text
open        Humans and permitted agents post normally (default; everyday chat)
moderated   A facilitator grants agents permission to respond
assembly    Bounded rounds under strict floor control
```

Ships:
- **The conch = the floor token**, active only in `moderated`/`assembly` mode. Only the
  holder may post; `conchd` (or a facilitator agent) passes it. Turn-taking,
  loop-prevention, and cost-bounding in one primitive — the origin metaphor made literal.
- **Convene / adjourn.** `conch convene <channel> --agents a,b,c --topic "…" --rounds N`
  opens a bounded assembly; each agent gets one turn per round; Nick advances or
  adjourns. The hard turn cap is the API-spend guard.
- **Pass.** An agent may pass its turn without generating a full response — no speaking
  when it has nothing to add.
- **Loop/cost guards.** No self-reply; token required to post; a `reply-to-agent`
  capability gates agent-to-agent chatter.
- **Typed proposals.** Brainstorm output uses a typed schema (idea / rationale / risk),
  rankable in P5.
- **Facilitator.** First a deterministic round-robin loop (testable, cost-bounded) — but
  *not the long-term model*. The eventual facilitator decides which agents have relevant
  capabilities, whether another response is needed, who critiques whom, and when the
  discussion has converged.

> **Success criterion:** Nick convenes an assembly on a real design question; several
> frontier models take bounded turns on a shared log under floor control; the transcript
> is typed, searchable, and auditable; no runaway loop; spend stays inside the cap.

*Deployment: dependency-free core, local, TUI, solo-with-agents. No new infra.*

### P5 — Deliberation & convergence
Turn a brainstorm into a decision. Agents rank and critique each other's typed proposals;
the assembly converges to a recommendation that lands as an **approval object** Nick
resolves — closing the loop onto the existing decision/audit core. Saved assembly
transcripts become first-class, replayable artifacts; shared durable context lets agents
in a room carry state across sessions.

*Deployment: still local, still TUI, still solo-with-agents. Peak capability before
adding humans or pixels.*

### P6 — Solo daily-driver polish
Everything needed for Conch to be the tool Nick lives in for Leviathan ops: richer TUI
(assembly view, approval inbox, search), DMs to individual agents, reactions/mentions as
log primitives, `principal-review` and assemblies on schedule. No new principal types, no
media — just make the single-user-plus-agents experience excellent.

*Deployment: your real daily instance. If Conch replaces Slack for Leviathan at R2, it
happens here — terminal-first (dashboard GUI optional per the P3 track).*

### P7 — Small-team multi-human + Slack-style GUI ⚠️ *charter amendment*
Open the instance to a handful of humans (Nick + collaborators). Presence, human DMs,
per-principal permissions, auth hardening for real multi-user — plus the full graphical
client (web SPA over the existing API, TUI still first-class beside it). This is the GUI
that's genuinely justified by having humans to use it, as distinct from the P3 dashboard.

> Reverses the "web UI possibly never" and "single human" leanings. Requires an ADR.
> Single-tenant still holds — one binary = one org, no multi-tenancy.

*Deployment: small self-hosted instance, a few humans + the agents, TUI and GUI. Still no
media.*

### P8 — Voice / PTT via LiveKit ⚠️ *charter amendment*
A LiveKit room bound per channel; push-to-talk as an audio track. **Agents are eligible
room participants** under the same capability model: an agent can join, transcribe in
real time and post the transcript as typed messages (voice becomes searchable and
auditable), or hold a `publish:audio` capability and speak via TTS.

> LiveKit is an optional external process; text/approval core runs without it (ADR-002).
> Reverses "no voice before P3"; requires an ADR.
>
> **Implementation note:** a browser client handles in-app PTT, but reliable *system-wide*
> PTT hotkeys push toward a desktop wrapper with OS-level key handling — lean Wails
> (Go-first), with the web client sharing most of the UI.

*Deployment: self-hosted `conchd` + LiveKit. First phase needing a second server process.*

### P9 — Screen sharing + session media
Screen-share is a second track type on the same LiveKit rooms from P8 — one design, not
two. Agents can watch a share and raise a `request_approval` off what they see. Session
recording/transcription flows into the audit log.

*Deployment: same as P8, richer. The full Discord-style surface, earned last.*

### P10+ — Reach & interop *(only if earned)*
Mobile (PWA, then native if warranted), Matrix bridge, federation-if-ever. The
"possibly never" tier — pursued only if real usage demands it.

---

## Revised long arc at a glance

| Phase | Focus | Deployment |
| ----- | ----- | ---------- |
| P2 | Security, auth, capabilities, search, threads, files | Core, local, solo+agents |
| **P3** | **Agent presence, sessions, runs, activity, adapters** | Core, local, solo+agents (opt. dashboard GUI) |
| **P4** | **Assemblies / floor-controlled multi-agent rooms** | Core, local, solo+agents |
| **P5** | **Deliberation, critique, ranking, convergence** | Core, local, solo+agents |
| P6 | Solo daily-driver polish (R2 candidate) | Your daily instance |
| P7 | Small-team multi-human + Slack-style GUI ⚠️ | Small self-hosted, humans+agents |
| P8 | Voice rooms / PTT via LiveKit ⚠️ | Self-hosted + LiveKit |
| P9 | Screen sharing, recording, agent visual participation | Self-hosted + LiveKit |
| P10+ | Mobile, bridges, broader reach | As earned |

---

## Positioning discipline

Don't out-feature Slack or Discord on breadth or voice quality — unwinnable for a solo
builder, even one with agents. The moat is the **agent-native core**: persistent governed
agent identities, presence and runs, capability-scoped principals, floor-controlled
assemblies, first-class approvals, immutable audit. Media (P8–P9) is *table stakes* added
cheaply via LiveKit, not the thing you compete on. Conch's differentiation is that AI
agents participate as persistent, governed workspace members rather than disposable
chatbots — keep the competitive weight there.
