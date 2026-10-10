# Design: the agent capability manifest

- **Status:** Draft for P2/V1 implementation (governing decision: D10, [ADR-000](../adr/ADR-000-charter.md); issue #77, parent #20, epic #93)
- **Owner:** protocol-designer (schemas), server-engineer (store, REST, enforcement)

An agent is a distinct principal type whose permissions are **declared in a manifest and enforced server-side** (D10). This note fixes what a manifest is on the wire and what its vocabulary means. It enforces nothing: storage and REST are the rest of #77, credential binding is #78, enforcement is #79, rate-limit counting is #80.

Exact Go types live in `pkg/schema/agent_manifest.go`; fixtures are `pkg/schema/testdata/*agent-manifest*`. This document is the semantic contract they satisfy.

## 1. Entity

There is **exactly one manifest per agent principal**, addressed by the principal id. A manifest has no id of its own. Wire name: `conch.agent_manifest.v1` (`AgentManifestSchemaV1`), type `AgentManifestV1`.

| Field | Notes |
|---|---|
| `schema` | Always `conch.agent_manifest.v1`. |
| `principal_id` | The agent principal the manifest governs. Positive. The schema cannot check that it exists or is an agent; the store must. |
| `display_name` | Human-rendered name. Required, not blank. |
| `tier` | Tier tag: `C`, `A`, or `H`. A label only (see §2). |
| `capabilities` | Set of granted capabilities (§3). Omitted or empty grants nothing. No duplicates. |
| `channels` | List of `{channel_id, permissions}` grants (§4), at most one per channel. A channel not listed is denied. |
| `rate_limits` | List of `{capability, max, window_seconds}` (§5), at most one per capability. |
| `created_at` / `updated_at` | Server-assigned, UTC millisecond timestamps. |

**Fail closed by construction.** Every list defaults to empty, and an empty `capabilities` or `channels` list grants nothing. There is no wildcard value (`*` fails validation), no capability that implies another, no tier that grants anything, and no "admin" flag. An agent with no manifest has no access; so does an agent whose stored manifest fails `Validate()`.

Lists are sets: order carries no meaning, and duplicates are rejected rather than merged so that a manifest has one spelling.

## 2. Tier

`AgentTier` is `C`, `A`, or `H`, case-sensitive. The schema attaches **no permission to a tier**. It is carried for operators, audit records, and later policy. If a later decision wants a tier to constrain what may be granted (for example "tier C may never hold `approvals.request`"), that is a validation rule added by that decision, not an implicit meaning read into the tag today.

## 3. Capabilities

Type `Capability`, a closed string enum. Names describe the permission (`<resource>.<verb>`), not the MCP tool.

| Capability | Constant | Permits |
|---|---|---|
| `messages.read` | `CapabilityMessagesRead` | Reading channel message history. |
| `messages.post` | `CapabilityMessagesPost` | Posting messages to a channel. |
| `approvals.request` | `CapabilityApprovalsRequest` | Raising an approval. |
| `approvals.await` | `CapabilityApprovalsAwait` | Blocking on an approval's resolution. |
| `approvals.check` | `CapabilityApprovalsCheck` | Polling an approval's current state. |

Message and approval capabilities are independent, and each approval operation is its own grant (#79): an agent that may post cannot thereby raise approvals, and one that may raise them cannot thereby observe them.

### Tool to capability mapping

`MCPToolCapability(tool)` and `MCPToolCapabilities()` in `pkg/schema` are the single source for this table.

| MCP tool | Required capability |
|---|---|
| `post_message` | `messages.post` |
| `read_channel` | `messages.read` |
| `request_approval` | `approvals.request` |
| `await_decision` | `approvals.await` |
| `check_decision` | `approvals.check` |

A tool absent from the table returns `ok == false`, which enforcement must treat as a denial. A test pins the table to exactly these five names; `pkg/schema` cannot import the server, so that list is restated in the test. #79 should add the matching server-side test that every tool `conchd` actually registers has a mapping.

The mapping is keyed by MCP tool name, but the capability is the unit of authorization: a REST or WebSocket path that acts for an agent principal checks the same capability (parity, rule 4).

## 4. Channel permissions

Type `ChannelPermission`, a closed string enum; `ChannelPermissions()` returns the vocabulary. A `ChannelGrant` is `{channel_id, permissions}` with a positive channel id and a non-empty, duplicate-free permission list. The permissions are independent: none implies another. In particular `post` does not imply `read`, `post_net` does not imply `post`, and `whisper` does not imply `whisper_agent`.

| Permission | Constant | Permits |
|---|---|---|
| `read` | `ChannelPermissionRead` | Reading the channel's contents, including scoped messages whose audience the agent is in. No further grant is needed to read a net or a whisper. |
| `post` | `ChannelPermissionPost` | Posting channel-wide: a message with no `audience`. |
| `post_net` | `ChannelPermissionPostNet` | Posting with `{"kind": "net"}` to a net in the channel that the agent is a member (not merely a monitor) of ([ADR-005](../adr/ADR-005-nets-and-whispers.md), #114). |
| `whisper` | `ChannelPermissionWhisper` | Posting with `{"kind": "principals"}` to a list of human principals in the channel. |
| `whisper_agent` | `ChannelPermissionWhisperAgent` | The whisper list may include other agents. Separate from `whisper` for the same reason the roadmap's `reply-to-agent` capability exists: agent-to-agent traffic no human reads is its own risk. |

The three scoped-speaking permissions were added as a compatible change; existing fixtures are byte-identical and `agent-manifest-v1-nets.json` exercises them. They are enforced for the REST post (#116) and for the MCP `post_message` tool (#117) by one function, `authorizeScopedPost`; a refusal is `forbidden`, audited with reason `audience_not_granted`. Whether the agent is on the net, and as a member or a monitor, is the net's roster and not the manifest. An agent with `post_net` or `whisper` but not `messages.post` still cannot post anywhere, because the capability and the channel permission are both required.

A capability and a channel permission are **both** required; neither substitutes for the other. `messages.post` with no channel grant can post nowhere, and a `post` grant without `messages.post` is inert. Capabilities say *what kind of thing* an agent may do; channel grants say *where*.

The schema does not check that a channel exists; the store does, on write. `PUT` rejects a grant naming a channel id that does not exist (400 `channel_not_found`). Channel ids are sequential, so accepting a grant for a channel that is not there yet would silently hand the agent whatever channel later receives that id. A grant already stored for a channel that is deleted afterwards stays valid and is inert.

## 5. Rate limits

`RateLimit` is `{capability, max, window_seconds}`: at most `max` uses of that capability in any window of `window_seconds`. Both numbers must be positive.

**One meaning for absence:** a capability with no `rate_limits` entry is *not rate limited by the manifest*. An omitted or empty `rate_limits` list therefore means no manifest-declared limit on anything. There is no other spelling of "unlimited": `max: 0` and `window_seconds: 0` are rejected, so zero never has to be interpreted. To forbid a capability outright, do not grant it.

This is the one list whose absence is permissive, and that is deliberate: a rate limit bounds a capability that has already been granted, and it never grants one. A limit entry for a capability the manifest does not grant is valid and inert, so revoking a capability does not require editing the limits in the same request.

Budgets are per capability, which gives #80 its required separation: reads have their own entry (or none) and cannot consume the `messages.post` or `approvals.request` budget.

## 6. How the vocabulary grows

Later decisions add permissions. Transmitting on a net and whispering ([ADR-005](../adr/ADR-005-nets-and-whispers.md)) landed in #114 as the per-channel permissions `post_net`, `whisper` and `whisper_agent` (§4), following this procedure; voice publishing is still to come.

- **Adding a value is compatible** and needs no version bump: add the constant, add it to `Valid()` (and to `Capabilities()` for a capability), add a row to the tables here, and add a *new* fixture that uses it. Existing fixtures stay byte-identical, which is what `scripts/schema-compat.sh` gates. Per-channel grants such as net transmit and whisper are new `ChannelPermission` constants; server-wide ones such as voice publishing are new `Capability` constants.
- **A new MCP tool** adds a row to the tool mapping in the same PR that registers the tool. Until then it is unmapped and denied.
- **New optional fields** on the manifest, a grant, or a rate limit (for example a burst size) are compatible.
- **Breaking, requiring `conch.agent_manifest.v2` and Nick's sign-off:** removing or renaming a value; changing what an existing value permits, including splitting one capability into two or making one imply another; changing the meaning of an absent list.

A value is only ever written through a server that knows it, because the server validates on write. The one way to meet an unknown value is a binary older than the stored data. In that case the manifest fails `Validate()` and the agent is denied, which is the safe direction. Clients that only display a manifest should decode it without calling `Validate()`, so that a newer server's value does not break an older `conch`.

## 7. REST shapes

Intended surface (handlers are the store/REST half of #77):

| Call | Request body | Response body |
|---|---|---|
| `PUT /v1/principals/{id}/manifest` | `PutAgentManifestRequestV1` | `PutAgentManifestResponseV1` (`{manifest}`) |
| `GET /v1/principals/{id}/manifest` | none | `GetAgentManifestResponseV1` (`{manifest}`) |

`PUT` is create-or-replace and is a **full replacement, never a merge**: the body carries `display_name`, `tier`, `capabilities`, `channels`, `rate_limits`, and a list left out is stored empty. Access that is not restated is removed. The principal id comes from the URL and `schema`, `created_at`, `updated_at` from the server; the request carries none of them. `PutAgentManifestRequestV1.Validate()` applies exactly the manifest's rules to the caller-supplied fields, so a body accepted on write validates on read.

Errors use the existing `schema.Error` body (`{code, message}`); a failed `Validate()` supplies the message.

## 8. What the consuming issues are expected to do

- **#77 (store and REST).** Persist one row per agent principal; reject a manifest for a missing or non-agent principal, or for a channel that does not exist; validate on write and again on read; set the timestamps. **Upgrade policy:** the migration creates the table and no rows. An agent that existed before has no manifest, and "no manifest" means deny once #79 enforces. No full-access manifest is generated automatically, because that would be a broad grant nobody wrote. Keeping an existing agent working across #79 therefore takes a manifest an operator writes out with `PUT`, not an exemption in code.
- **#78 (credentials).** Resolve each token to a principal id. That id is the manifest's address; there is no separate manifest id to carry.
- **#79 (enforcement) — implemented in `internal/server/authz.go`.** An agent acts only where two gates both allow it, checked in this order:
  1. **Capability.** The tool's name is mapped with `MCPToolCapability`; an unmapped tool is refused. The manifest must exist, be valid, and `Allows(capability)`. Refusal: `forbidden`. This runs before any lookup, so it reveals nothing about what exists.
  2. **Membership** of the target channel (#90). A non-member gets `channel_not_found` (or `approval_not_found`), byte-identical to an unknown target.
  3. **Channel permission.** `AllowsChannel(channel, permission)`. Refusal: `forbidden` — only members get this far.

  The permission each tool needs: `post_message` → `post` without an audience, `post_net` for a net audience, `whisper` (and `whisper_agent` if a recipient is an agent) for a whisper; `read_channel` → `read`; `request_approval` → `post` on the target channel; `check_decision` and `await_decision` → `read` on the approval's channel. Being the requester of an approval grants nothing by itself; an agent removed from a channel can no longer watch the approvals it raised there, and an in-flight `await_decision` ends on its next poll.

  Every denial is audited as `access_denied` (actor `principal:<id>`, subject `mcp:<tool>` or the route, detail `capability=… target=channel:<id> reason=…`). Reasons: `unmapped_tool`, `no_manifest`, `invalid_manifest`, `capability_not_granted`, `not_a_member`, `channel_permission_not_granted`, `audience_not_granted` (a scoped post without `post_net`, `whisper` or `whisper_agent` for that audience), `net_monitor_only` and `net_not_on` (a scoped post refused by the net's roster: the agent only monitors the net, or is not on it, which also covers an archived or unknown net; the row carries no net id), `agents_use_mcp`, `agents_no_voice` (an agent credential on a voice route: the session, the presence snapshot or the presence socket; ADR-004 defers agents in voice).

  MCP always authenticates, so this applies to `/mcp` in every mode. Where the server only knows the caller under `--auth required`, the same manifest gate applies to an agent credential used on the REST message routes and the WebSocket routes, and to a webhook hook bound to an agent. The approval REST routes are the human surface: an agent credential is refused there and uses MCP instead.

  Upgrading is deny-by-default, so `conchd` logs at startup how many agent principals have no manifest.
- **#80 (rate limits).** Read `manifest.RateLimitFor(capability)`. `ok == false` means do not limit on the manifest's account. Otherwise allow at most `max` uses per `window_seconds` for that principal and capability. The counting algorithm (fixed or sliding window, how a burst is bounded), restart behaviour, and the retry metadata are #80's to choose and document; the manifest only states the budget.

## 9. Explicitly out of scope

- Enforcement, audit events, authorization error shapes, credentials, and rate-limit counters.
- Manifests for human principals. Human roles are #89.
- Listing manifests, partial updates (`PATCH`), and deleting a manifest. An agent is cut off by replacing its manifest with an empty one.
- Any MCP tool for reading or writing manifests. Agents do not administer their own permissions.

## Open questions

Each has a default taken in this slice, so nothing blocks; the dispatcher should confirm or overrule.

1. **Which channel permission do the approval tools need?** Decided in #79 as proposed: `request_approval` needs `post` on the target channel; `await_decision` and `check_decision` need `read` on the approval's channel. It lives in the enforcement code (`authz.go` / `mcp.go`), not in `pkg/schema`.
2. **What do the tiers mean?** D10 names `C/A/H` without defining them in this repository. The schema treats the tier as an opaque label with no permission attached. In particular an agent tagged `H` is accepted; if `H` is reserved for humans, tell me and the agent vocabulary shrinks to `C`, `A` before anything is published.
3. **`display_name` and the principal's `name`.** `PrincipalV0.name` already exists. The manifest carries its own `display_name` because #77 lists it; they can disagree. Options: keep both (handle and label), or have the store copy one into the other. Default: keep both, no coupling.
4. **Is "no entry means not rate limited" the right default?** The alternative is a server-wide default budget applied when the manifest is silent. That would be a `conchd` configuration matter in #80 and would not change this shape, but the note above would need to say "the server default applies" in place of "not rate limited".
5. **Should one manifest-wide budget exist alongside per-capability ones?** Left out as not needed by #80's criteria. It could be added later as an optional field without a version bump.
6. **Canonical ordering.** `Validate()` accepts lists in any order. If the store should normalise order on write (stable diffs, stable audit records), that is a store rule; say so and it goes into the #77 store brief.
7. **Tool name list duplication.** The five tool names are restated in a `pkg/schema` test. Exporting them as constants from `pkg/schema` for the server to register with would remove the duplication, but it touches `internal/server/mcp.go`, which is approval-path and outside this slice.
