# Conch roadmap (long horizon)

Governed by [ADR-000](docs/adr/ADR-000-charter.md). Roadmap *changes* require Nick's
sign-off; milestones and issues live on [GitHub](https://github.com/njdaniel/conch/milestones).

**Destination.** Conch grows from an agent-native chat-ops tool into a communications
platform where humans and AI agents are equally first-class principals on one audited
log — a "Slack 2.0" whose differentiator is *agents as persistent, governed workspace
members*, with Discord-style real-time features (PTT, screen sharing) added at the end
via LiveKit, never bespoke WebRTC.

> **Amended 2026-10-05:** a parallel **nets-and-voice track** (V0–V6) now runs alongside
> the numbered phases — see [its section](#parallel-track--nets-and-voice-v0v6). It pulls
> P2 auth and multi-human access forward and starts voice before P8.

> **Amended 2026-10-13 (ADR-007, #259):** P3's first slice is a **local multi-client
> prototype**: two terminal clients, a small read/write browser client and two scripted
> agents on one server. See the P3 section and the plan on
> [#260](https://github.com/njdaniel/conch/issues/260).

> **Amended 2026-10-13:** **P2 is done** (#253). Its exit gate, `go run ./e2e/p2-hardening`
> (#81), runs on every pull request. Password login and OIDC (#24) did not ship in it and
> move to P7; see the P2 paragraph. P3 is next among the numbered phases.

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

### P2 — Hardening ✅
Agent identity manifests + server-side capability enforcement with per-agent rate limits,
credentials that rotate and revoke, channel membership, audit export, FTS5 search, file
uploads, threads, and `principal-review` on cadence. The exit: one command,
`go run ./e2e/p2-hardening`, upgrades a database written by a P1 `conchd` and drives real
binaries through all of it; it runs on every pull request and before every release.

Two things this paragraph used to list did not ship here:

- **Auth (OIDC + local).** P2 exits on bearer credentials issued by an operator (track
  phase V1), which is real auth and enough for a small trusted group. Interactive
  password login and optional OIDC (#24) move to P7, or earlier if a real user needs
  them. *(Nick, 2026-10-13.)*
- **The first runtime adapter** is P3's, where this paragraph already pointed.

> **Why P2 gated everything below:** capability manifests and real auth are
> prerequisites for agent sessions (P3), floor control (P4), and multi-human access. The
> nets-and-voice track took P2's auth and capability issues as its first phase (V1)
> rather than skipping them.

*Deployment: dependency-free core, localhost, Nick + one or more agents in the TUI.*

---

## The long arc

> **Client/P3 amendment (2026-10-13, #259):**
> [ADR-007](docs/adr/ADR-007-client-interfaces.md) selects the client stack and adds a
> small read/write browser prototype alongside the TUI at P3. The
> [P3 local prototype brief](docs/design/p3-local-prototype.md) and the plan on
> [#260](https://github.com/njdaniel/conch/issues/260) define the work; none of it is
> built yet. No V4–V6 exit gate is removed, and full P7/P9 scope is not pulled into P3.

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

> **First local prototype (ADR-007):** register + heartbeat + status,
> basic runs/activity, TUI and minimal browser lobby/chat/agent views, two scripted
> agents and one opt-in real adapter. Nick can connect from two isolated TUI clients
> and a browser at once, then repeat with multiple sessions of one identity.
> The [planning brief](docs/design/p3-local-prototype.md) defines deterministic tests,
> optional VM/LAN and voice extensions, and the clean-checkout walkthrough.
> Richer timelines, additional adapters and full intervention follow; this first
> slice does not by itself complete all P3 objectives.

> **First prototype exited 2026-10-14 (#286).** The code is on `main` (#264–#285): the
> walkthrough (`docs/p3-walkthrough.md`), the automated suite (`go run ./e2e/p3-local`)
> and the browser suite (`make web-e2e`), both run in CI on every pull request. Every row
> P3-01 to P3-13 passed in an agent run on Nick's machine, P3-13 with one real Claude Code
> reply; the [exit record](docs/design/p3-exit-record.md) says so plainly and keeps a place
> for Nick's own run.

> **Follow-ons merged by 2026-10-15.** These are on `main`:
> - the Codex and local-model adapters (#343, #344);
> - `pause` and `resume` (#345, #346);
> - the web client embedded in the binary (#347), and generated TypeScript bindings (#348);
> - Runs views and richer timelines (#349, #350);
> - read state and unread counts (#351);
> - nets and whispers in the browser (#352);
> - the webhook/event adapter (#359);
> - the parts of the VM/LAN extension that need no second machine (#379, #380, #382).
>
> The success criterion below was **met in an agent run** on 2026-10-15 (walkthrough row
> P3-14, recorded in the [exit record](docs/design/p3-exit-record.md), by Nick's choice).
> Claude Code, Codex and a local model ran at once, each was shown with its runtime and
> model, its activity could be read, and an instruction to one was applied. It is not yet
> comfortable in practice: until #410 is fixed, agents in one channel answer each other
> without end.
>
> What remains:
> - #410 and #411, from that run;
> - the guest session by hand, #381 (Nick, VM/LAN, tracker #353);
> - the voice extension, gated on #189 (#393, #394, #396; tracker #354), which is
>   non-blocking for P3.

> **Success criterion:** Nick opens Conch and sees Claude Code, Codex, and a local model
> running simultaneously, sees what each is doing, opens each activity stream, and sends
> an instruction to one — without switching terminals.

*Deployment: dependency-free core, local, Nick + his agents in the TUI;
ADR-007 adds a statically served browser prototype without a required Node service.
LiveKit remains optional for the parallel voice track.*

> **Independent track — dashboard GUI.** The agent roster / run timelines / activity
> streams are the one surface genuinely better as pixels than in a terminal. A minimal,
> read-only dashboard GUI over the existing API may be built *whenever it earns its
> place* — as early as alongside P3 — and is deliberately **decoupled** from the
> Slack-style multi-human GUI in P7. The TUI stays first-class for ops and approvals.
> **Extended by ADR-007:** the early GUI allowance now covers the bounded read/write
> local prototype above, and no more than that.

### Parallel track — nets and voice (V0–V6)

Layered comms: several **nets** inside one channel (two squads of four plus a command net
the leads bridge), heard at once and transmitted on one at a time, plus **whispers** to
specific people — in text and in voice. `conchd` decides who may hear what and logs it.
Governed by ADR-003 (multi-human access), ADR-004 (voice via LiveKit), ADR-005 (nets and
whispers), ADR-006 (Rust voice client).

| Phase | Ships | Usable result |
| ----- | ----- | ------------- |
| V0 | ADRs signed; throwaway LiveKit spike (audience mapping, Rust SDK fit, echo cancellation, Wayland push-to-talk) | Two machines talking with hold-to-talk; spike report |
| V1 | Human accounts and sessions, agent credential binding, capability enforcement, channel membership (P2: #24, #77–#79, #57) | Separate logins; non-members cannot read or post |
| V2 | Text nets and whispers: message envelope v2, visibility predicate on every read path, CLI/TUI/MCP | The 2×4 squad layout in text |
| V3 | Voice control plane in `conchd`: optional LiveKit config, session tokens, voice presence, audit | Headless client joins; core unaffected with LiveKit down |
| V4 | `conch-voice` MVP: PipeWire capture and playback, one push-to-talk key, mixing | Three people on Linux talking in a channel |
| V5 | Layered voice: a key per net, simultaneous listening with ducking and stereo placement, whisper | The 2×4 scenario in voice |
| V6 | Reconnect, noise suppression, setup doctor, CI and packaging | Something you leave running |

```text
V0 ──► V1 ──► V2 ───────────────┐
        └───► V3 ──► V4 ──► V5 ─┴─► V6      (V5 needs V2)
```

Later, unscheduled: agents in voice (live transcription posted as typed messages with the
audio's audience; TTS behind `publish:audio`), Windows/macOS clients.

> **What this displaces.** V1 *is* the bulk of P2, so P2's remaining items (search,
> uploads, threads, audit export) and P3 (agent presence) wait behind it or share
> attention with V3–V5. That is a deliberate trade, not free. *(2026-10-13: P2's remaining
> items have since shipped; P3 still shares attention with V4–V6.)*

*Deployment: `conchd` alone through V2; `conchd` + optional LiveKit from V3; Linux/PipeWire
clients; a few humans plus agents.*

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
- **Per-net floors.** Where a channel has nets (track phase V2), each net carries its own
  floor token, so teams take bounded turns in parallel.
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

ADR-007 brings the shared React foundation and a limited browser lobby/chat/agent
view into P3, and with it a basic online / stale / offline roster for people as well
as agents. P7 still owns the full small-team GUI, presence preferences and social /
onboarding polish; an early prototype is not completion of this phase. Electron
is the chosen desktop shell, sharing browser UI; platform release scope and
native media/input support need their own measured implementation work.

Multi-human *access* arrives earlier, in the nets-and-voice track (ADR-003). P7 adds what
is left: presence, human DMs, per-principal permissions polish — plus the full graphical
client (web SPA over the existing API, TUI still first-class beside it). This is the GUI
that's genuinely justified by having humans to use it, as distinct from the P3 dashboard.
Interactive password login and optional OIDC (#24) land here too, moved from P2 on
2026-10-13; until then people sign in with a credential an operator issues.

> Reverses the "web UI possibly never" leaning. The GUI requires its own ADR; ADR-003
> does not cover it.
> Single-tenant still holds — one binary = one org, no multi-tenancy.

*Deployment: small self-hosted instance, a few humans + the agents, TUI and GUI. Still no
media.*

### P8 — Agents in voice
Human voice and push-to-talk now ship in the nets-and-voice track (V3–V6, ADR-004). What
remains here is agent participation. **Agents are eligible
room participants** under the same capability model: an agent can join, transcribe in
real time and post the transcript as typed messages (voice becomes searchable and
auditable), or hold a `publish:audio` capability and speak via TTS.

> LiveKit is an optional external process; text/approval core runs without it (ADR-002).
> Agent voice capabilities are outside ADR-004 and need their own decision.

*Deployment: self-hosted `conchd` + LiveKit, as established by the track.*

### P9 — Screen sharing + session media
Screen-share is a second track type on the same LiveKit rooms as voice — one design, not
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
| P2 ✅ | Security, auth, capabilities, search, threads, files | Core, local, solo+agents |
| **P3** | **Agent presence, sessions, runs, activity, adapters**; local multi-client prototype first | Core, local, TUI + minimal browser (ADR-007) |
| **P4** | **Assemblies / floor-controlled multi-agent rooms** | Core, local, solo+agents |
| **P5** | **Deliberation, critique, ranking, convergence** | Core, local, solo+agents |
| P6 | Solo daily-driver polish (R2 candidate) | Your daily instance |
| P7 | Small-team multi-human + Slack-style GUI ⚠️ | Small self-hosted, humans+agents |
| V0–V6 | Nets and voice track (parallel): auth, membership, text nets, PTT voice | Core, then + optional LiveKit |
| P8 | Agents in voice | Self-hosted + LiveKit |
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
