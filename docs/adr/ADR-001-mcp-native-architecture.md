# ADR-001: MCP-native architecture and the API parity rule

- **Status:** Accepted
- **Date:** 2026-07-12
- **Deciders:** Nick (Tier-H); formalizes founding decisions D4 and D6 ([ADR-000](ADR-000-charter.md))

## Context

Agents are first-class citizens in Conch, not bolted-on bots driving a human-shaped API through adapters. Existing chat platforms make agents second-class: webhook shims, bot tokens with human-user semantics, no typed payloads, no native approval primitive. Meanwhile MCP has become the standard way agents discover and call tools.

We need to decide how agents connect, how humans connect, and how the two stay in lockstep.

## Decision

### conchd exposes a native MCP server endpoint (D4)

`conchd` itself serves MCP — no sidecar, no bridge process (that would also violate the single-binary invariant, [ADR-002](ADR-002-single-binary-sqlite.md)). The minimum tool set:

| Tool | Purpose |
|---|---|
| `post_message` | Post a message (rendered form + optional typed payload) to a channel. |
| `read_channel` | Read/paginate a channel's messages. |
| `request_approval` | Create a first-class approval object (typed options, deadline, quorum, escalation). |
| `await_decision` | Block until the approval resolves or a timeout elapses. |
| `check_decision` | Async poll of an approval's resolution state. |

Tool input/output schemas are owned by the protocol-designer role and derive from `pkg/schema` (D8) — the MCP layer is a projection of the canonical schemas, never a second source of truth.

### The REST/WS API is canonical; parity is a hard rule (D6)

Anything the CLI/TUI can do exists in the REST/WS API **first**. `conch` (TUI and scriptable CLI alike) is an ordinary client of that public API — it gets no private backdoor. Agents get MCP; MCP tools map onto the same core operations the REST/WS API fronts.

Consequence for review: a PR adding a CLI/TUI capability without its REST/WS equivalent, or an MCP tool that can do something the API cannot, is an invariant violation and blocks.

### Two front-ends, one core

```
 humans                agents
   │                     │
 conch CLI/TUI         MCP client
   │                     │
 REST + WebSocket      MCP endpoint
   └─────────┬───────────┘
          conchd core
   (message log, approval state
    machine, audit log, SQLite)
```

Both front-ends write to the same message log and the same audit trail. There is no agent-only or human-only data path.

## Consequences

- Agent capabilities can never silently outrun what humans can see and audit, and vice versa.
- The MCP layer stays thin; core logic lives once, behind the API. Testing the core covers both front-ends' semantics.
- An MCP SDK dependency will be needed in P1; selection is delegated to protocol-designer with Nick's sign-off (deps gate).
- Server-side capability enforcement per agent manifest (D10) attaches at the MCP endpoint as a protocol error, not a polite refusal — scheduled for P2.
