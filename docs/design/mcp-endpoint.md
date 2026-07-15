# Design: the MCP endpoint — `post_message` and `read_channel`

- **Status:** Draft for P1 implementation (governing decisions: D4, D6, D8, ADR-001; SDK: [mcp-sdk-selection.md](mcp-sdk-selection.md))
- **Owner:** protocol-designer (tool schemas), server-engineer (endpoint wiring, principal auth)
- **Issue:** #11 (P1: MCP endpoint in conchd)

`conchd` serves a native MCP endpoint (D4). MCP tools are thin projections of
the REST/WS core, typed by `pkg/schema` (ADR-001) — never a second source of
truth (D8). This document defines the endpoint (transport, auth, principal
attribution) and the first two tools, `post_message` and `read_channel`, mapping
each field 1:1 onto existing `pkg/schema` types. It ends with the `pkg/schema`
gaps this mapping exposes, flagged rather than papered over.

## 1. Endpoint and transport

- **Transport:** streamable HTTP (current MCP spec transport), served by the
  official SDK (`modelcontextprotocol/go-sdk`, [mcp-sdk-selection.md](mcp-sdk-selection.md)).
  It mounts as an ordinary `net/http` handler on the existing mux
  (`internal/server/server.go`), alongside the REST and WS routes — no separate
  listener, no sidecar (ADR-002).
- **Path:** `/mcp` (single streamable-HTTP endpoint; the SDK handles the
  POST/GET/session-id mechanics of the transport). The versioned `/v0`, `/v1`
  REST paths stay as they are; MCP carries its own protocol version in the
  initialize handshake, and tool payload shapes are versioned by their
  `pkg/schema` envelope (`conch.message.v1`), not by the URL.
- **Registration:** the endpoint advertises exactly the tools registered on it.
  P1 registers `post_message` and `read_channel`. The remaining D4 tools
  (`request_approval`, `await_decision`, `check_decision`) register later on the
  same endpoint (approval-path work, separate issues).

## 2. Authentication and principal attribution

This is the one place the MCP surface must **diverge in shape** from the REST
surface, and the divergence is deliberate.

- **Auth:** a simple bearer token on the streamable-HTTP request
  (`Authorization: Bearer <token>`), acceptable for P1 per issue #11. Manifests
  and capability enforcement are P2 (#20). A missing or unknown token is a
  transport-level rejection (HTTP 401 before any tool runs), not a tool result.
- **Principal attribution:** the token maps server-side to exactly one **agent
  principal** (`schema.PrincipalKind == "agent"`). The authenticated principal
  is the message author. Agents therefore **must not** supply `author_id`
  themselves — an agent that could name an arbitrary author could forge
  attribution, which the audit trail (D8/D9) cannot allow.
- **Consequence for the tool schema:** the REST body type
  `schema.PostMessageRequestV1` carries a client-asserted `author_id` (correct
  for a trusted first-party REST caller). The MCP `post_message` input **omits
  `author_id`**; the server injects the token's principal id and constructs the
  `PostMessageRequestV1` internally. `pkg/schema` stays the single source of
  truth (the server still builds and validates the canonical request type); the
  agent wire simply never sees the author field. See gap G2 (§7) for whether
  this projection warrants its own `pkg/schema` type.

The token-to-principal store is server state, not a wire shape — no `pkg/schema`
change is needed for it.

## 3. Tool: `post_message`

Post a message (rendered body + optional typed payload) to a channel. Projection
of `POST /channels/{channel}/messages` (the v1 form; see gap G1).

**Input** — a projection of `schema.PostMessageRequestV1` minus `author_id`
(supplied by auth, §2):

| Field | JSON | Type | Notes |
|---|---|---|---|
| channel | `channel` | string | Channel **name** (matches REST, which resolves by name via `store.ChannelByName`). |
| body | `body` | string | Rendered, human-readable form. Required, non-empty. Maps to `PostMessageRequestV1.Body`. |
| payload | `payload` | object, optional | Typed machine payload. Exactly `schema.Payload`: `{ "schema": "<name>.v<N>", "data": <json> }`. Omitted when absent. Maps to `PostMessageRequestV1.Payload`. |

`payload.schema` must be a well-formed versioned name (`schema.ValidPayloadName`,
e.g. `leviathan.trade_signal.v1`); `payload.data` is preserved verbatim
(`json.RawMessage`) so an unregistered schema round-trips unchanged — forward
compatibility is a property of the schema layer, inherited for free.

**Output** — exactly `schema.PostMessageResponseV1`:

```jsonc
{ "message": {            // schema.MessageV1
    "schema": "conch.message.v1",
    "id": 42,
    "channel_id": 7,
    "author_id": 3,        // the authenticated agent principal
    "created_at": "2026-07-14T12:34:56.789Z",  // RFC 3339 UTC, ms (schema.Timestamp)
    "body": "...",
    "payload": { "schema": "leviathan.trade_signal.v1", "data": { } }
} }
```

**Parity:** the posted message is durable, then broadcast to the WS hub and
readable via the REST `GET` — the same `persist-then-broadcast` path
`handlePostMessage` already uses (`internal/server/messages.go`). MCP adds no
write path of its own; it calls the same core operation.

## 4. Tool: `read_channel`

Read/paginate a channel's messages. Projection of
`GET /channels/{channel}/messages`.

**Input:**

| Field | JSON | Type | Notes |
|---|---|---|---|
| channel | `channel` | string | Channel **name** (matches REST). |
| after | `after` | integer, optional | Cursor: return messages with id > `after`. Default 0 (from the start). Maps to the REST `after` query param. |
| limit | `limit` | integer, optional | Page size. Default 50, max 100 — **identical bounds to REST** (`defaultMessageLimit`/`maxMessageLimit`). Out-of-range is an input error. |

**Output** — exactly `schema.ListMessagesResponseV1`:

```jsonc
{
  "messages": [ /* schema.MessageV1, ... */ ],
  "next_after": 91          // omitted/0 when no further page (same convention as REST)
}
```

**Parity:** identical pagination semantics, identical caps, identical cursor.
A reader must see the same messages via `read_channel` and via the REST `GET`
(the dogfood-check parity assertion). Because both return `MessageV1`, a payload
posted through either front-end reads back byte-identical through both.

## 5. Parity mapping (ADR-001 D6)

| MCP tool | REST/WS equivalent | Core operation | Canonical types |
|---|---|---|---|
| `post_message` | `POST /channels/{channel}/messages` + WS broadcast | `store.InsertMessage` → hub/broadcaster | in: `PostMessageRequestV1` (author from auth); out: `PostMessageResponseV1` |
| `read_channel` | `GET /channels/{channel}/messages` | `store.ListMessages` | out: `ListMessagesResponseV1` |

No MCP-only capability: every tool is a call into an operation the REST/WS API
already fronts. The one shape difference (`author_id` sourced from auth, not the
body) is a *narrowing* of the REST input, never a new capability — MCP can do
strictly less than a trusted REST caller, not more.

## 6. Error semantics and result embedding

MCP separates two failure classes; Conch maps its existing REST error taxonomy
(`schema.Error{code,message}`, from `internal/server/messages.go`) onto them:

- **Transport / protocol errors** (before or around tool execution): auth
  failure → HTTP 401; malformed JSON-RPC → the SDK's protocol error. These never
  reach a tool.
- **Tool-execution errors** (the call ran, the operation was refused): returned
  as a tool result with `isError: true`, carrying the **same `schema.Error`
  shape** the REST endpoint returns, so callers get one error vocabulary across
  both front-ends. No new error type.

| Condition | REST today | MCP result |
|---|---|---|
| Channel does not exist | 404 `channel_not_found` | tool error, `schema.Error{code:"channel_not_found"}` |
| Empty/invalid body, bad `after`/`limit` | 400 `invalid_request` | tool error, `schema.Error{code:"invalid_request"}` |
| Payload schema name malformed / data not JSON | (v1) validation error | tool error, `schema.Error{code:"invalid_request"}` (from `Payload.Validate`) |
| Body over size cap | 413 `request_too_large` | tool error, `schema.Error{code:"request_too_large"}` |
| Author (token principal) unknown/disabled | 400 `author_not_found` | **transport 401** (it is an auth failure at the MCP layer, not a body field) |
| Store failure | 500 `internal_error` | tool error, `schema.Error{code:"internal_error"}`, detail withheld |

**Structured output embedding:** successful results are returned as the SDK's
structured tool output — the `PostMessageResponseV1` / `ListMessagesResponseV1`
JSON above — with a short human-readable text block alongside (SDK convention).
The structured half is authoritative and is literally the `pkg/schema` type
marshalled; agents parse that, not the prose.

**Tool JSON Schemas** are generated from the `pkg/schema` Go types via the SDK's
`google/jsonschema-go` rather than hand-authored, so the advertised
`inputSchema`/`outputSchema` track `pkg/schema` and cannot drift into a second
source of truth (D8). The one exception is `post_message`'s input, which is the
`PostMessageRequestV1` shape **with `author_id` removed** (§2) — see gap G2.

## 7. `pkg/schema` gaps flagged (not invented)

The mapping above is clean against existing types except for these, which the
#11 implementation (and, where noted, a `schema-change`) must resolve. Flagged
per the brief; **no new shapes invented here.**

- **G1 — the v1 message surface (payload-carrying REST/WS) must exist before
  MCP.** `pkg/schema` defines `MessageV1`, `PostMessageRequestV1`,
  `PostMessageResponseV1`, `ListMessagesResponseV1` (with payload support), but
  the committed baseline (`internal/server`) serves only `MessageV0` on
  `/v0/...` (no payload). Parity (D6) requires the REST/WS surface to expose the
  v1 (payload-carrying) shape **before or alongside** MCP — otherwise
  `post_message` with a payload would be an MCP-only capability, an ADR-001
  violation. This is the single biggest dependency and is **not** a schema
  change (the types already exist): it is server wiring plus
  `store.InsertMessage`/`ListMessages` gaining payload persistence.
  *Resolution:* the v1 REST message endpoints are a prerequisite for (or part
  of) #11; if not separately scoped, file the issue and mark #11 blocked-by it.
  (A parallel v1-REST change — `handlePostMessageV1`, `messageV1FromStore`,
  payload persistence — was present uncommitted in the dispatch worktree while
  this doc was written, which is exactly this prerequisite; the dispatcher
  should reconcile the two so MCP lands on top of a merged v1 REST surface.)

- **G2 — no exact type for the agent `post_message` input.** The MCP input is
  `PostMessageRequestV1` minus the client-asserted `author_id` (§2). No
  `pkg/schema` type matches that projection exactly. *Recommended resolution
  (no schema change):* the server injects the authenticated principal and
  constructs `PostMessageRequestV1` internally; the tool's advertised
  `inputSchema` is `PostMessageRequestV1`'s schema with `author_id` omitted, kept
  in sync in the same PR as the tool definition. If the team instead prefers an
  explicit wire type (e.g. `AgentPostMessageRequestV1` with no author field),
  that is a `pkg/schema` addition and goes through the `schema-change` skill with
  Nick's sign-off — flagged, not assumed.

- **G3 — payload persistence in the store layer.** In the committed baseline
  `store.Message` and the store→wire mapping carry no payload, and
  `store.InsertMessage(ctx, channelID, authorID, body)` takes no payload
  argument. The v1 round-trip that `post_message`/`read_channel` promise (post a
  payload, read it back byte-identical) depends on the store persisting
  `schema.Payload` verbatim. This is storage work under G1, called out
  separately so it is not lost, and is part of the parallel v1-REST change noted
  there. No wire-shape change — `MessageV1.Payload` already defines the shape.

None of these are blockers to *this document*; they are the handoff list the
implementation issue must close for the tools to ship parity-clean.

## 8. Explicitly out of scope

- The other D4 tools (`request_approval`, `await_decision`, `check_decision`) —
  approval-path work, separate issues, governed by
  [approval-object.md](approval-object.md).
- Agent manifests, per-tool capability enforcement, rate limits — P2 (#20). P1
  auth is a bearer token mapped to one agent principal (§2).
- OAuth/JWT auth via the SDK's auth subpackage — deferred with P2 (P1 should avoid importing the
  SDK auth subpackages so `oauth2`/`jwt` aren’t linked into the `conchd` binary; see
  [mcp-sdk-selection.md](mcp-sdk-selection.md) §3).
- Any `pkg/schema`, `go.mod`, or `internal/server` edit — this is the design;
  the code lands in the #11 implementation PR after Nick's SDK sign-off.
