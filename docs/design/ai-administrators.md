# Design: AI administrators

- **Status:** Accepted, 2026-11-05 (Nick: "approve the recommendations and the ADR amendment"). The §9 decisions are adopted as recommended, and the §8 amendments are in [ADR-010](../adr/ADR-010-delegated-administration.md) decision 6, [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md) decisions 1, 6 and 7, and [ADR-013](../adr/ADR-013-one-community-model.md). Implementation follows the slices in §10 (#1326).
- **Issue:** [#1326](https://github.com/njdaniel/conch/issues/1326). The implementation slices are in §10.
- **Pinned to:** `main` at `d7afd8c797d0d95fd2ec0836c6ff2ac3df254539` (merge of #1304, 2026-11-05). Every file and line in §1 is read at that commit.
- **Owner:** protocol-designer (this note, the wire shapes, the MCP tools); server-engineer (store, dispatcher, routes); cli-engineer and web-engineer (setup); docs-agent (README, CHANGELOG).
- **Touches:**
  - [ADR-000](../adr/ADR-000-charter.md) D4 (the MCP tool set grows), D10 (unchanged: the manifest stays the agent's whole grant, and enforcement stays server-side);
  - [ADR-001](../adr/ADR-001-mcp-native-architecture.md): the new tools front REST routes, as the rule requires; the parity exceptions issue [#1172](https://github.com/njdaniel/conch/issues/1172) gains nothing from this design, since the admin-action routes exist over REST first;
  - [ADR-010](../adr/ADR-010-delegated-administration.md) decision 6, "no agent is an owner, operator or manager": amended, not reversed (§8.1);
  - [ADR-013](../adr/ADR-013-one-community-model.md) consequence "No MCP tool is added: joining, the directory and administration are human acts" (§8.3);
  - [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md) decision 1 ("agents … hold no role and no permission"), decision 7 ("No MCP tool is added: administration is a human act") and decision 6's helper rule (§8.2);
  - [docs/design/admin-roles.md](admin-roles.md) §3.3 and §7.1 (the first rule of `principalHolds`);
  - [docs/design/agent-manifest.md](agent-manifest.md) §1, §3, §6 and §9 ("Any MCP tool for reading or writing manifests" stays out; administration of *other* things comes in);
  - [docs/design/approval-object.md](approval-object.md) §7 (an approval whose approval *runs* something is new, but the object and its state machine are not touched: §3.5).

## 0. The design in five lines

1. An agent may hold a **subset of the operator permissions** (`manage_members`, `manage_channels`, `make_member_links`, `change_server_settings`, `read_audit_log`; never `act_as_members`, the owner mark, a role, or the right to decide), granted in its manifest **by an owner only**, and revoked by anyone holding `manage_members`.
2. It administers through **one REST door**, `POST /v1/admin/actions`, which dispatches a closed catalog of named actions onto the **existing REST handlers**, in process, so every authorization check those handlers and the store make today runs unchanged; the MCP tools `describe_admin_actions`, `submit_admin_action` and `run_approved_action` front that door.
3. **Risky actions are approval-gated:** the server itself raises an ordinary approval object in the agent's configured approval channel, carrying a `conch.admin_action.v1` payload that says exactly what will run; a human with that channel's decide right decides; the agent then calls `run_approved_action`, and the server runs the **stored** payload once, as the agent. The approval state machine is reused unchanged.
4. **Setup is one command per server** (`conch admin ai-admin create`, the console's "Add AI administrator", or `conchd ai-admin create` offline over SSH), and each prints one MCP client entry: server URL, `/mcp` endpoint and token. The owner keeps one entry per server in their MCP client.
5. **Prompt injection is bounded, not solved:** by the permission subset, by the gate on everything irreversible or widening, by server-rendered approval text the agent cannot forge, by refusing the agent any change to its own approval channel's roster or rights, by rate limits, and by audit rows that name the agent on every action.

## 1. Current state at `d7afd8c7`

What the brief calls the inventory. "Merged" means on `main` at the pinned commit; "planned" is listed in §1.6.

### 1.1 Agents can do nothing but what a manifest and a membership allow

- An agent is a principal of kind `agent` (`store.PrincipalAgent`). It is made by `POST /v0/principals` (`handleCreatePrincipal`, `internal/server/principals.go:16`, class `accessPermission` + `manage_members`, `routes.go:50`), and the CLI `conch admin principal create --kind agent` (`internal/cli/admin.go:117`).
- Its rights are exactly its manifest (`schema.AgentManifestV1`, `pkg/schema/agent_manifest.go:279`): `capabilities` (seven values, `Capabilities()`, line 84), `channels` (per-channel `ChannelGrant`, line 207) and `rate_limits` (line 244). There is no wildcard, no implied capability and no admin flag; an absent list grants nothing (`agent-manifest.md` §1).
- The manifest is written by `PUT /v1/principals/{id}/manifest` (`handlePutManifest`, `internal/server/manifests.go:24`; `accessPermission` + `manage_members`, `reauthAlways`, `routes.go:169`, `routes.go:393`). The store refuses a manifest for a non-agent (`ErrPrincipalNotAgent`, `store/manifests.go:21`). Each list is stored as its own JSON column (`agent_manifests`, migration 5, `store/store.go:136`) and decoded strictly (`unmarshalList`, `store/manifests.go:203`, `DisallowUnknownFields`), so a binary that does not know a field refuses the manifest and the agent is denied: the safe direction (`agent-manifest.md` §6).
- Enforcement is in `internal/server/authz.go`: every MCP tool is registered through `addAgentTool` (`mcp.go:140`), which builds an `agentScope` (`authz.go:248`) that maps the tool to a capability (`schema.MCPToolCapability`, `agent_manifest.go:130`), loads the manifest afresh, and is the only way a tool reaches a channel or an approval (`channelByName`, `channelByID`, `approval`: `authz.go:442`, `:558`, `:579`). Membership is checked before the manifest, so a non-member learns nothing (`authz.go:301`). Every denial is audited `access_denied` with a reason (`auditAgentDenial`, `authz.go:85`; reasons at `authz.go:37`). The rate limit is spent only after every gate allowed (`agentScope.spend`, `authz.go:320`; `spendAgentBudget`, `ratelimit.go:228`).
- Over REST, an agent credential is admitted on the message, WebSocket and session/run routes under the same manifest gate (`agentCallerAllowed`, `authz.go:619`; `agentSelfAllowed`, `authz.go:189`), and refused outright on: the approval routes (`humansOnly`, `authz.go:769`, `routes.go:327`), files (`humansOnlyFiles`), channel creation, the channel directory and leaving (`denyAgentChannelCreate`, `denyAgentDirectory`, `denyAgentLeave`, `routes.go:33`, `:39`, `:41`), and every `accessPermission`, `accessOperator`, `accessOwner`, `accessChannelManager` and `accessOperatorOrSelf` route, because those classes admit humans only and the store's permission helper answers false for an agent before it reads anything (`principalHolds`, `store/permissions.go:221`: `p.Kind != PrincipalHuman` → false). The member-link routes refuse an agent in the handler with `human_required` (`routes.go:225`).
- Agents cannot decide approvals: `handleCastDecision` refuses a non-human decider (`human_required`, `approvals_handlers.go:267`), and no MCP tool casts a decision. The store's deciding transaction checks `channel_members.can_decide`, disabled and guest state, but not kind (`castDecision`, `store/approvals.go:424`): the kind check is the handler's alone today.
- Agents cannot hold a role or the owner mark (`SetRole`, `store/roles.go:379`, `ErrRoleNotAllowed` "an agent has no role") and cannot have manager standing (`memberActorStandingTx`, `store/members.go:376`: a non-human actor is `ErrManagerRefused`).
- Rate limits: per (principal, capability), sliding window, in memory, from the manifest (`agent-manifest.md` §5). Humans are never limited.

### 1.2 Operators administer through permissions, owners through the mark

- Three fixed server roles (owner mark, `operator`, `member`) and the per-channel manager grant (ADR-010, ADR-014). The permission catalog has twelve values (`schema.Permissions`, `pkg/schema/permission.go:84`; the twelfth, `use_camera`, came with ADR-015). Every write that a permission gates asks `permittedTx` inside its transaction (`store/permissions.go:263`); handlers ask `can()` (`internal/server/auth.go:175`).
- Route classes at the pinned commit (`routes.go`): 22 routes under `accessPermission` + `manage_members`, 4 under `act_as_members`, 6 under `manage_channels`, 2 under `change_server_settings`, 1 under `read_audit_log`; 6 `accessOperator` (fixed operator reads: the permissions document, the joining and invite settings, the three report routes); 1 `accessOwner` (the role route); 6 `accessChannelManager`; 3 `accessOperatorOrSelf`.
- Owner protection: a non-owner acting on an owner is refused in the handler (`ownerProtected`, `auth.go:245`) and again in the store (`ownerProtectedTx`, e.g. `store/disable.go:191`). The last-owner guard refuses a disable, hide or revoke that would leave no owner able to sign in (`ErrLastOperator`, `store/disable.go:18`).
- Recent authentication: the routes in `recentAuthRoutes` (`routes.go:387`) need a recent proof of password and code, but **only from a session credential**: `recentAuthGate` lets a script credential through (`operator_auth.go:507`: `!cred.Session` → next). A script credential an operator issues, including one an AI holds today, is outside the re-authentication class by design (the S15 boundary, `operator_auth.go:478`).
- The admin console (`web/src/AdminConsole.tsx`, `web/src/admin/*`: Server, Permissions, Members with `PersonActions`, Channels, Invites, Reports; `SecretOnce.tsx` shows a token once) and the CLI (`conch admin principal|credential|member|manifest|channel|invite|permissions|hook …`, `internal/cli/admin.go:113-214`) are both clients of the routes above (ADR-001).
- The host commands (`cmd/conchd/operator.go`): `conchd operator bootstrap|setup|recover` work offline on the data directory and print a one-use setup token or link; `bootstrap-operator` prints a script credential. There is no network route that resets an operator.

### 1.3 The MCP surface

`conchd` registers **seventeen** tools at the pinned commit (`mcpServerFor`, `mcp.go:154`, and `addSessionTools`, `mcp.go:606`; the README's "Thirteen tools are registered" at `README.md:626` predates the four reaction tools and is stale). The capability map (`mcpToolCapabilities`, `agent_manifest.go:101`) has the same seventeen entries, pinned by `currentMCPTools` in `agent_manifest_test.go`:

| Tool | Capability | Channel permission | REST route it fronts |
| --- | --- | --- | --- |
| `post_message` | `messages.post` | `post`, `post_net`, `whisper` (+`whisper_agent`) | `POST /v2/channels/{channel}/messages` |
| `read_channel` | `messages.read` | `read` | `GET /v2/channels/{channel}/messages` |
| `request_approval` | `approvals.request` | `post` | `POST /v1/approvals` (human-only over REST) |
| `await_decision` | `approvals.await` | `read` | `GET /v1/approvals/{id}` (polled) |
| `check_decision` | `approvals.check` | `read` | `GET /v1/approvals/{id}` |
| `set_reaction`, `remove_reaction` | `messages.post` | by audience | `PUT`/`DELETE …/reactions/{emoji}` |
| `read_reactions`, `list_reactors` | `messages.read` | `read` | `GET …/reactions`, `GET …/reactions/{emoji}` |
| `register_session`, `heartbeat`, `end_session` | `sessions.manage` | none (own session) | `POST /v1/sessions`, `…/heartbeat`, `DELETE /v1/sessions/{id}` |
| `start_run`, `update_run`, `post_activity` | `runs.manage` | by the run's audience | `POST /v1/sessions/{id}/runs`, `…/state`, `…/activity` |
| `poll_interventions`, `ack_intervention` | `runs.manage` | none / by run | `GET /v1/sessions/{id}/interventions`, `POST /v1/interventions/{id}/ack` |

Authentication is a bearer credential resolved to an **agent** principal (`authenticateMCP`, `mcp_auth.go:83`); a human's credential is refused there. Every tool error is a `schema.Error` as structured content (`mcpToolError`, `mcp.go:268`). No tool administers anything, reads or writes a manifest, or touches a principal other than the caller.

### 1.4 Approvals

- One object (`schema.ApprovalV1`, `pkg/schema/approval.go:93`): requester, channel, title, body, optional typed `payload`, typed `options` (at least one `approve` and one `reject`), `deadline`, `quorum`, optional `escalation_target`, `state`. The payload is stored and returned by `GET /v1/approvals` and `GET /v1/approvals/{id}` (`ListApprovalsResponseV1`, `GetApprovalResponseV1`), so a client can render a known payload richly.
- The state machine (`approval-object.md` §2) and manager (`internal/server/approvals/approvals.go`): `Create` validates, stores, audits `approval_created`, notifies (ntfy) and arms the deadline; `DecideAsMember` writes the decision, counts quorum among current holders of `can_decide` and resolves in one IMMEDIATE transaction (`store.castDecision`, `store/approvals.go:384`). Resolution is one `approval.resolution.v1` event for waiters, pollers and the audit log.
- Who decides: a human member of the approval's channel holding `can_decide`, set by `PUT /v1/channels/{channel}/members/{principal_id}` (`accessChannelManager`, `reauthOperators`). Decisions are cast through `POST /v1/approvals/{id}/decisions` (`conch approve|reject`, the browser inbox).
- The audit chain is `approval_created → notify_sent|notify_failed → decision_cast… → approval_resolved | approval_escalated → approval_expired` (`approval-object.md` §6), and every approval-path PR ships a test that walks it (CLAUDE.md rule 3).

### 1.5 How an AI administers today

With a human operator's script credential in `CONCH_TOKEN`, an AI runs `conch admin …` or the REST routes directly. The audit trail names the human. The re-authentication class does not apply (§1.2). No approval is involved. Setup is per server, by hand: a browser or SSH session to issue the credential, then pasting it into the AI's environment. That is the workaround the issue describes, and all three of its faults are visible in the code above.

### 1.6 Merged versus planned

Everything in §1.1–1.4 is merged. The planned work this design must not collide with, from the open backlog on 2026-11-05:

- **Guests later** ([#1004](https://github.com/njdaniel/conch/issues/1004)): a third permission column. Nothing here assumes guests; an AI administrator never holds or grants a guest anything, and the guest routes stay out of the action catalog.
- **A12, who issued each credential and hook** ([#1024](https://github.com/njdaniel/conch/issues/1024)): adds `issued_by`. Until it lands, the AI administrator's own credential is attributed through the `credential_created` audit row's actor, like every other.
- **Moderation M1–M6** ([#849](https://github.com/njdaniel/conch/issues/849)–[#854](https://github.com/njdaniel/conch/issues/854)): message removal, voice removal, bans. Not in the v1 catalog (§3.3); when they land, each is one row added to the catalog with a gate class.
- **ADR-001 parity exceptions** ([#1172](https://github.com/njdaniel/conch/issues/1172)): records that MCP approvals are agent-only and the operator host commands are offline. The host command of §4.3 joins the second list.
- **ADR-014 D6 wording** ([#1306](https://github.com/njdaniel/conch/issues/1306)): unrelated to this design, but the ADR-014 amendment in §8.2 should land after or with it so the ADR is not amended twice in one week with conflicting diffs.

## 2. Alternatives weighed

The issue's "Proposed direction" is the principal's starting point. Three shapes were compared against the owner's words ("admin from AIs for all servers, streamlined"; "I prefer to administrate through you") and the three faults of today's workaround (the AI is hidden, no approval loop, browser steps per server).

| Shape | What it is | For | Against | Verdict |
| --- | --- | --- | --- | --- |
| **A. An agent principal holding operator permissions** (the issue's direction) | The owner grants a named agent a subset of the operator permissions; the agent acts through MCP, as itself; some actions are gated by an approval. | The actor is honestly an agent, in every audit row and roster. The manifest already is the agent's whole grant (D10), so the grant lives where every other agent right lives, is revoked the same way, and rate-limited the same way. The agent is a member of its approval channel like any requester, so the existing decide right, notifications and inbox apply unchanged. One MCP entry per server, since the agent's credential is the configuration. | Changes two accepted ADR clauses. `principalHolds` and the store's standing functions, which today refuse agents first, must admit a granted agent: approval-path work. | **Chosen**, with the gate below. |
| **B. A human operator's credential delegated to an AI and marked AI-held** | A credential flag (or scope) on an operator's script credential says "held by an AI"; the server audits `via=ai` and may restrict the routes that credential reaches. | No ADR change; no new permission holder. Cheapest. | The actor stays the human: every audit row, roster and `whoami` says the owner did it. The credential inherits the owner's whole standing, including owner protection's exemptions and the role route, unless a scope cuts it, which is a second permission system on top of ADR-014. Nothing routes it through an approval. Rate limits do not apply (humans are never limited). The owner's own re-authentication class is bypassed (§1.2) with no compensating control. | Rejected: it keeps the workaround's first two faults and formalises them. |
| **C. Approval-only administration** | The agent holds nothing; every admin action is an approval the owner decides, and the server runs the approved action. | ADR-010 D6 untouched; nothing runs without a human. | Every routine action (add someone to a channel, make a link, rename) costs the owner a decision, which is the opposite of "streamlined". And "the server runs the approved action" needs exactly the binding, one-shot run and dispatcher of §3.5 anyway; it is the gated half of A with the direct half removed. If the server ran actions *as the server*, the audit actor would be `system`, hiding the agent again. | Rejected on its own; its mechanism is kept as A's gate. |

**The chosen design is A with C's mechanism for the gated subset.** The one place it departs from the issue's text: the grant is **owner-only** to make (not "the owner grants" loosely): an operator holding `manage_members` can revoke it but cannot create it, because an agent holding operator permissions is operator-grade, and ADR-010 D4 says only owners make operators.

## 3. The design

### 3.1 The grant: manifest fields, owner-only

`AgentManifestV1` and `PutAgentManifestRequestV1` gain two optional fields (additive; §6):

```go
// Permissions is the subset of operator permissions this agent holds
// (ADR-014 as amended by #1326). Omitted or empty grants nothing. Only
// values for which Permission.AgentGrantable() is true are accepted; the
// store refuses a write that grants any permission by an actor who is not
// an owner (ErrNotOwner), and audits every change (admin_grant_changed).
Permissions []Permission `json:"permissions,omitempty"`
// Admin configures how this agent's gated actions are approved. Required
// when Permissions is non-empty (an agent with a grant and nowhere to ask
// is refused on write); omitted otherwise.
Admin *AgentAdminV1 `json:"admin,omitempty"`
```

```go
type AgentAdminV1 struct {
	// ApprovalChannelID is the channel the server raises this agent's
	// gated actions in. The agent must be a member with read there (it
	// watches its approvals through await_decision and check_decision),
	// and it may never change that channel's roster or rights (§3.5.6).
	ApprovalChannelID int64 `json:"approval_channel_id"`
	// ApprovalTTLSeconds is the deadline of each gated action's approval,
	// from its creation. Default 86400; min 300; max 604800.
	ApprovalTTLSeconds int64 `json:"approval_ttl_seconds,omitempty"`
	// RunWindowSeconds is how long after an approval resolves approved the
	// action may still be run. Default 86400; min 60; max 604800.
	RunWindowSeconds int64 `json:"run_window_seconds,omitempty"`
	// Quorum is the approval's quorum. Default 1.
	Quorum int `json:"quorum,omitempty"`
	// EscalationTarget is copied onto each approval. Optional.
	EscalationTarget *EscalationTarget `json:"escalation_target,omitempty"`
	// RequireApproval adds actions to the gated set for this agent (§3.3).
	// It can only widen the server's fixed set, never narrow it.
	RequireApproval []AdminAction `json:"require_approval,omitempty"`
	// MaxPending caps this agent's open admin approvals. Default 8; max 32.
	MaxPending int `json:"max_pending,omitempty"`
}
```

**The agent-grantable subset** is fixed in code (`Permission.AgentGrantable()`): `manage_members`, `manage_channels`, `make_member_links`, `change_server_settings`, `read_audit_log`. Never: `act_as_members` (speaking as people is the one power the issue rules out, and ADR-014 split it out for exactly this reason), the owner mark, a role, `manages`, `can_decide`, and the six member permissions (an agent has no voice, no camera, does not browse the directory as a member and creates channels through `manage_channels`, not `create_*_channels`). The `Validate()` of both manifest shapes refuses a non-grantable value, so the schema refuses it before the store does.

**Dependencies and defaults.** `change_server_settings` and `read_audit_log` carry the same warning text they carry for operators, and `conch admin ai-admin create` does not grant them unless named. There are no cells and no column: an agent's grant is a set in its own manifest, read per request like the document, with no precedence against operators (an agent never holds more than an operator *could* hold, since the subset is drawn from the operator column's catalog, and the owner who grants it holds everything).

**Who writes it.** `PUT /v1/principals/{id}/manifest` keeps its class (`manage_members`, `reauthAlways`). Inside the store's write transaction, a request whose `permissions` is non-empty is refused unless the actor is an owner (`ErrNotOwner` → 403 `forbidden` "owner required", the role route's answer); a request whose `permissions` is empty is accepted from any holder of `manage_members`, so **revocation is one manifest write by anyone who administers people**, or one `POST /v1/principals/{id}/disable`, which also revokes the credential, ends the sessions and drops the sockets (`handleDisablePrincipal`, `disable.go:30`). An agent can never write any manifest (§3.3: the route is not in the catalog), so it cannot widen itself.

**Grants never exceed the granter** (ADR-010 D4) holds trivially: only owners grant, and owners hold everything.

### 3.2 How an agent holds a permission

One rule is added to `principalHolds` (`store/permissions.go:221`), as rule 1a, between "an agent holds nothing" and "an owner holds everything":

> 1a. An agent that is not disabled or hidden holds exactly the permissions its manifest lists, provided the manifest is stored, valid, and each listed permission is agent-grantable. No brake, cell, dependency or column applies: an agent's grant is the whole answer. An agent with no manifest, or an invalid one, holds nothing.

`permittedTx` therefore reads the manifest row for an agent principal (one more indexed read, by primary key). `can()` is unchanged in signature and callers. `whoami.permissions` for an agent lists the grant, which is how the AI learns what it may do over REST; over MCP, `describe_admin_actions` says the same (§3.6).

The store's standing functions (`memberActorStandingTx` and its rename twin; `CreateInvite`/`makerStillAllowed`/`effectiveChannelsTx`/`CloseInvite`; `SetMayInvite`'s operator branch) refuse a non-human actor **before** asking a permission (`store/members.go:376`). Each becomes: refuse disabled, hidden and guest; then owner; then `principalPermittedTx(manage_channels)` (which now answers for a granted agent); then the manager row. Since `principalHolds` answers false for an ungranted agent, every ungranted agent is refused exactly where it is refused today, and the structural test of admin-roles §7.1 still holds (no actor-role comparison is added). `CreateSignInLink`/`makerIsOperatorTx` are not touched: `act_as_members` is never granted, so they refuse agents by the permission alone.

A granted agent therefore has **operator standing in the store** for the permissions it holds, and **no standing anywhere else**: it is not a manager (it holds no `manages` row and the manager branch is reached only when the permission is not held), it is not an operator by role (`isOperator` stays false; the fixed-operator reads of §1.2 admit it only through the catalog, §3.3), and it is never an owner.

### 3.3 The action catalog

Administration by an agent is a **closed catalog** of named actions, each mapped to one existing REST route, one permission and one default gate class. The catalog is a `pkg/schema` vocabulary (`AdminAction`, closed, client-tolerant) and a server table (`internal/server/admin_actions.go`) that a structural test keeps complete and honest: every action's route must exist in the route table with the class and permission the catalog says, and every route an agent reaches by dispatch must be in the catalog.

Gate classes: **direct** runs at once; **approval** raises an approval first (§3.5). The table is the server's fixed default; a manifest's `admin.require_approval` may move a direct action to approval for that agent, never the reverse (O3).

| Action | Route (existing) | Permission | Gate | Target | Notes |
| --- | --- | --- | --- | --- | --- |
| `list_members` | `GET /v1/principals` | `manage_members` | direct | — | With the route's filters as `params`. |
| `create_member` | `POST /v0/principals` | `manage_members` | direct | — | `kind` is forced to `human`; an agent never creates an agent (that is how a second, ungated identity would be made). |
| `rename_member` | `PUT /v1/principals/{id}/name` | `manage_members` | direct | principal | Owner protection applies in the handler as today. |
| `set_voice_permission` | `PUT /v1/principals/{id}/voice-permission` | `manage_members` | direct | principal | A brake; reversible. |
| `set_invite_permission` | `PUT /v1/principals/{id}/invite-permission` | `manage_members` | direct | principal | A brake; reversible. |
| `disable_member` | `POST /v1/principals/{id}/disable` | `manage_members` | **approval** | principal | Last-owner guard and owner protection run in the handler and store as today. |
| `enable_member` | `POST /v1/principals/{id}/enable` | `manage_members` | **approval** | principal | Re-enabling a possibly compromised account is an opening. |
| `hide_member` | `POST /v1/principals/{id}/hide` | `manage_members` | **approval** | principal | Removal. The bulk route `POST /v1/principals/hide` is **not** in the catalog (O4). |
| `list_credentials` | `GET /v1/principals/{id}/credentials` | `manage_members` | direct | principal | Metadata only; never a token. |
| `revoke_credential` | `DELETE /v1/credentials/{credential_id}` | `manage_members` | **approval** | credential | Owner protection as today. |
| `revoke_all_credentials` | `POST /v1/principals/{id}/credentials/revoke-all` | `manage_members` | **approval** | principal | |
| `list_hooks` | `GET /v1/hooks` | `manage_members` | direct | — | |
| `revoke_hook` | `DELETE /v1/hooks/{id}` | `manage_members` | **approval** | hook | |
| `list_channels` | `GET /v1/directory/channels?scope=all` | `manage_channels` | direct | — | The route's `denyAgentDirectory` wrapper admits a dispatched agent holding the permission (§3.4). |
| `create_channel` | `POST /v0/channels` | `manage_channels` | direct | — | Private by default; the agent is **not** added as a member (an admin does not need to read what it makes). |
| `rename_channel` | `PUT /v1/channels/{id}/name` | `manage_channels` | direct | channel | |
| `set_channel_default` | `PUT /v1/channels/{id}/settings` (`default` only) | `manage_channels` | direct | channel | |
| `set_channel_visibility` | `PUT /v1/channels/{id}/settings` (`visibility` only) | `manage_channels` | **approval** | channel | Private → public exposes content; public → private is gated too, for symmetry and simplicity. |
| `archive_channel`, `restore_channel` | `PUT /v1/channels/{id}/settings` (`archive`/`restore`) | `manage_channels` | **approval** / direct | channel | |
| `list_channel_members` | `GET /v1/channels/{channel}/members` | `manage_channels` | direct | channel | |
| `add_channel_member` | `POST /v1/channels/{channel}/members` | `manage_channels` | direct | channel, principal | `can_decide` and `manages` are forced **off** in the dispatched body whatever the agent sends; refused outright on the agent's approval channel (§3.5.6). |
| `remove_channel_member` | `DELETE /v1/channels/{channel}/members/{principal_id}` | `manage_channels` | **approval** | channel, principal | Refused outright on the approval channel. |
| `set_channel_member_rights` | `PUT /v1/channels/{channel}/members/{principal_id}` (`manages`, `can_decide`) | `manage_channels` | **approval** | channel, principal | Changes who approves; refused outright on the approval channel. |
| `create_member_link` | `POST /v2/invites` | `make_member_links` | direct | — | The dispatcher **clamps**: `max_uses` ≤ 5, expiry ≤ 7 days, no "never expires", landing channels only ones the agent could add to (`manage_channels`), never the approval channel. The link is tied to the agent as maker, so the directory shows who it brought in, and `makerStillAllowed` re-checks the agent's grant at redemption. The route's `human_required` refusal admits a dispatched agent. |
| `list_member_links` | `GET /v2/invites` | `manage_members` | direct | — | Everyone's, as a holder of `manage_members` sees. |
| `close_member_link` | `DELETE /v1/invites/{id}` | `manage_members` | direct | invite | Reduce-only. |
| `list_link_joiners` | `GET /v2/invites/{id}/joiners` | `manage_members` | direct | invite | |
| `disable_link_joiners` | `POST /v1/invites/{id}/disable-joiners` | `manage_members` | **approval** | invite | Bulk. |
| `disable_invitees` | `POST /v1/principals/{id}/disable-invitees` | `manage_members` | **approval** | principal | Bulk. |
| `pause_invites` | `PUT /v1/invites/settings` (`paused: true`) | `manage_members` | direct | — | A brake. |
| `resume_invites` | `PUT /v1/invites/settings` (`paused: false`) | `manage_members` | **approval** | — | An opening. |
| `get_join_settings` | `GET /v1/join/settings` | `change_server_settings` | direct | — | A fixed-operator read, admitted by dispatch for the holder of the write's permission. |
| `set_join_settings` | `PUT /v1/join/settings` | `change_server_settings` | **approval** | — | |
| `get_permissions` | `GET /v1/permissions` | `change_server_settings` | direct | — | As above. |
| `set_permissions` | `PUT /v1/permissions` | `change_server_settings` | **approval** | — | Member column only, since the handler requires an owner for operator cells and an agent is never one. |
| `list_reports`, `get_report`, `update_report` | the three report routes | `manage_members` | direct | report | Fixed-operator reads, admitted by dispatch. The redaction list goes only to holders of `manage_channels`, as today. |
| `export_audit` | `GET /v1/audit` | `read_audit_log` | direct | — | Off unless granted; the trail names every member and private channel. |

**Not in the catalog, and never dispatched** (the structural test pins the exclusions by route): `PUT /v1/principals/{id}/role`, `PUT /v1/principals/{id}/manifest`, `GET /v1/principals/{id}/manifest` for another principal, every `act_as_members` route (credentials for others, rotate, hooks, sign-in links), `POST /v1/principals/hide` (bulk), the guest routes, the net routes (v1 scope; one row each later), moderation (#849–#854, later), the approval routes (the agent uses MCP for those), and anything a member does for themself. An action that names the agent itself as target (its own principal id or credential) is refused `admin_action_not_allowed`: self-administration goes through the manifest, by a human.

### 3.4 One door: `POST /v1/admin/actions`, dispatching onto the existing handlers

Three routes, all `accessAuthenticated`, humans and agents alike:

| Route | Body | Answer |
| --- | --- | --- |
| `GET /v1/admin/actions` | — | `AdminCatalogResponseV1`: every action with its route, permission and gate class **as it applies to the caller** (`held`, `gated`), and the caller's `admin` settings when it is an agent. |
| `POST /v1/admin/actions` | `SubmitAdminActionRequestV1{action, target?, params?, reason?}` | `AdminActionResultV1`: `status: ran` with the dispatched route's `http_status` and `result` (its body, verbatim JSON), or `status: pending_approval` with `approval_id`, the `ApprovalV1` and its `deadline`. |
| `POST /v1/admin/actions/{approval_id}/run` | — | `AdminActionResultV1` with `status: ran`. |

**Why one door and not sixty route changes.** The issue requires that the new tools reuse the REST handlers' authorization rather than duplicate it. The dispatcher does that literally: it builds an in-process `*http.Request` for the catalog's route (method, path with the target ids filled, body = the params the catalog allows, after clamping), puts the caller, its credential and a **dispatch mark** (`adminDispatch{action, approvalID}`) in the request context, and serves it through the server's own mux (`s.mux.ServeHTTP`) into an in-memory response writer. So the route class runs, the recent-authentication gate runs, the handler runs, and the store's transaction re-checks run, all unchanged, and the answer the agent gets is the answer a human operator would get from the same route, with its status and error code passed through. Nothing is reimplemented; nothing can drift. The `calls.go` pattern (one function behind REST and MCP) was considered and rejected for admin: there are thirty-odd handlers, each with its own target protection, and extracting each is exactly the duplication the issue forbids.

**What changes to let an agent through, and only through the door:**

1. The authentication middleware trusts a context identity **only** when the dispatch mark is present. The mark is a context value that no network request can carry, so this is unforgeable from outside; a test pins that a request with an `Authorization` header and no mark is authenticated from the header alone.
2. `accessPermission`'s "an agent is refused first" becomes "an agent is refused first **unless dispatched**"; a dispatched agent is then judged by `can()` like anyone else. The fixed-operator reads (`accessOperator`) admit a dispatched agent whose catalog permission `can()` holds. `humansOnlyBecause` wrappers and the handler-level `human_required` refusals on the routes the catalog names admit a dispatched agent. Every other route, and every undispatched agent request, answers as today; the structural test fails if a wrapper admits dispatch on a route outside the catalog.
3. The dispatcher checks `can(agent, catalog.permission)` **before** building the request (audited `access_denied permission=<name>` on refusal, the same row an operator gets), and the handler and store check it again. A refused action costs no rate-limit budget and creates no approval.
4. The recent-authentication gate sees the agent's script credential and passes it, as it passes every script credential (§1.2). That is recorded here as a known property, not a hole closed by this design: the gate is a defence for operators' browser sessions, and the agent's compensating control is the approval gate.

The three routes are versioned by suffix, carry no schema-name field, and are in `recentAuthRoutes` as `reauthNone` because the dispatched route carries its own rule.

### 3.5 Gated actions: the approval, the binding, expiry, the one-shot run

This is the core. Everything here is approval-path (CLAUDE.md rule 3).

#### 3.5.1 What the decider sees

When `submit_admin_action` names a gated action, the dispatcher does not run it. It **creates an ordinary approval** in the agent's `admin.approval_channel_id`, through the same path `request_approval` uses (`grantedChannel.createApproval`, `authz.go:401`, which takes the requester and the channel from the grant and never from the request). The approval's fields are composed by the server:

- `requester_id`: the agent.
- `title`: `AI admin <agent name>: disable member "alice" (#7)` — the action's catalog verb, the target's current name and id.
- `body`, server-rendered, one line per fact:

  ```
  Requested by AI administrator "deploy-admin" (agent #12) over MCP at 2026-11-05T10:14:00Z.
  Action:     disable_member
  Will run:   POST /v1/principals/7/disable
  Target:     principal #7 "alice" (member, joined 2026-10-30)
  Parameters: {}
  The agent's stated reason: "alice asked to leave the server in #general"
  If approved, conchd runs exactly this action once, as agent #12, within 24h of the decision. Nothing else runs.
  ```

  The agent's `reason` is the only text of the agent's in the approval, it is capped at 500 characters, and it is labelled as the agent's words. Everything else is derived from the stored action, so an injected instruction cannot make the approval *say* one thing and *do* another (§5.1).
- `payload`: `{"schema": "conch.admin_action.v1", "data": <AdminActionV1>}` (§6), the machine form of the same facts: action, route with ids filled, target kind, id and name at request time, the params verbatim, the agent id, the reason, `requested_at`. Clients that know the schema render it as a card (the browser inbox and the TUI, slice S7); clients that do not show the body.
- `options`: `run` (kind `approve`, label "Run it") and `refuse` (kind `reject`, label "Do not run"). No custom option: the decision is yes or no on this exact action.
- `deadline`: now + `admin.approval_ttl_seconds` (default 24h); the grace period follows the existing default rule (equal to the window). `quorum` and `escalation_target` from `admin`.

The approval goes through `approval_created`, the ntfy push (title, requester, deadline, channel: the owner's phone buzzes with "AI admin deploy-admin: disable member alice"), the browser inbox and `conch approvals list`, exactly as every approval does. An extra audit row, `admin_action_requested` (actor the agent, subject `approval:<id>`, detail `action=disable_member route="POST /v1/principals/7/disable" target=principal:7`), follows `approval_created` so the chain says what kind of approval this is.

#### 3.5.2 Binding: the decision is bound to this exact action

- The action, its target ids and its parameters are **stored on the approval** as its payload at request time, in the same transaction as `approval_created`. The agent's later `run_approved_action(approval_id)` carries **only the approval id**: the server reads the payload back and runs *that*. There is no second place the action is spelled, so there is nothing to substitute.
- The run checks, inside one IMMEDIATE transaction before anything is dispatched: the approval exists and the caller is its requester (anyone else gets `approval_not_found`, the uniform answer); its payload is a `conch.admin_action.v1` whose `agent_id` is the caller (`approval_not_admin_action` otherwise); its state is `resolved` with outcome `approved` (`approval_not_approved` for pending, escalated, rejected or expired); `resolved_at + run_window` has not passed (`approval_run_window_passed`); no run row exists for it (`approval_already_run`); and the target's name now equals the name the decider was shown (`target_changed`: ids are the binding, the name check refuses the one case where the decider's picture and the server's differ). Then it **claims** the run by inserting `admin_action_runs(approval_id PRIMARY KEY, agent_id, claimed_at)` and commits. SQLite's single writer makes exactly one claim succeed for a given approval, so a replayed or concurrent run is refused before any dispatch.
- The dispatch then runs with the mark `adminDispatch{action, approvalID}`; the handler and store re-check the agent's grant and the target's protections at that moment, so a grant revoked or a target made owner between decision and run is refused there, as it would be for anyone.
- The claim row is updated with the outcome (`ran_at`, `http_status`, `error_code`), and `admin_action_ran` (or `admin_action_refused`, with the dispatched route's code) is audited with actor the agent, subject `approval:<id>`, detail `action=… route=… status=…`. A failed dispatch does **not** release the claim: the agent requests a new approval. Nothing is retried on its behalf.

#### 3.5.3 Expiry and denial

- `rejected` or `expired`: nothing runs, ever; `run_approved_action` answers `approval_not_approved`, audited `admin_action_refused reason=<outcome>`. The agent learns the outcome from `await_decision`/`check_decision` as for any approval.
- The approval's own deadline and escalation are the existing ones (ntfy urgent topic on escalation, `approval_expired` at the final deadline).
- The **run window** is new and separate: an approval can be approved while the agent is asleep; the window says how long "yes" stays usable. Default 24h, in the manifest (O6).

#### 3.5.4 The agent can never decide its own request

Structurally, three independent walls, each with a test:

1. No MCP tool casts a decision (§1.3), and the REST decision route refuses an agent credential before the handler (`humansOnly`, `routes.go:330`) and refuses a non-human decider in the handler (`human_required`, `approvals_handlers.go:267`).
2. **New:** the store's deciding transaction gains `p.kind = 'human'` beside `can_decide`, `disabled_at IS NULL` and the guest test (`castDecision`, `store/approvals.go:424` and the quorum count at `:460`), so no path that writes `channel_members` for an agent can make its decision count, whatever the handler does.
3. **New:** the agent cannot change the roster or rights of its approval channel (§3.5.6), and `add_channel_member` forces `can_decide` off on every channel, so it cannot seat a decider either.

A fourth, social wall is the decider's: the owner holds `can_decide` in the approval channel; anyone else there decides only if an owner or an operator with `manage_channels` gave them the right, which is the rule today.

#### 3.5.5 The state machine is untouched

Nothing in `internal/server/approvals/` or the `approvals` and `approval_decisions` tables changes. The approval object gains no field; `conch.admin_action.v1` is a payload like `leviathan.trade_signal.v1`. The one-shot run is a **separate table** keyed by approval id, and the run is a **separate call** by the requester after resolution, so resolution has no side effect and no new state ("approved but not run" is an approval in state `resolved` with no run row, visible to the inbox as resolved). If the owner later wants "run on approval without the agent calling back", that is the one change to the state machine this design avoided; it is listed as O7 with a recommendation against it.

#### 3.5.6 The approval channel is off limits to the agent

Whatever the agent holds, every catalog action that would touch `admin.approval_channel_id` (add or remove a member, set `manages` or `can_decide`, rename, change visibility or default, archive, name it as a link's landing channel) is refused `admin_action_not_allowed`, audited `access_denied reason=approval_channel_protected`, before any dispatch. The channel that decides what the agent may do is the one channel the agent may not shape. The agent reads and posts there (its manifest grants `read` and `post`, so it can explain itself to the owner), and that is all.

### 3.6 The MCP tools

Three tools, one capability. Registered through `addAgentTool` like every other, so the capability gate, the audit subject `mcp:<tool>` and the rate limit apply unchanged. The capability is `admin.act` (`CapabilityAdminAct`), one entry in `Capabilities()` and three in `mcpToolCapabilities`. The capability says the agent may *use the door*; the permissions say what it may do through it; the manifest's `rate_limits` entry for `admin.act` bounds how often (default in `conch admin ai-admin create`: 60 per hour).

| Tool | Input (`jsonschema` per field) | Output | Fronts |
| --- | --- | --- | --- |
| `describe_admin_actions` | none | `AdminCatalogResponseV1` | `GET /v1/admin/actions` |
| `submit_admin_action` | `action` (one of the catalog), `target` (`{kind, id}`; omitted for server-wide actions), `params` (any JSON object; the route's body, clamped by the catalog), `reason` (string, ≤ 500, shown to the decider for a gated action) | `AdminActionResultV1` | `POST /v1/admin/actions` |
| `run_approved_action` | `approval_id` | `AdminActionResultV1` | `POST /v1/admin/actions/{approval_id}/run` |

The tool descriptions say, in plain words, that a gated action returns `pending_approval` and must be watched with `await_decision`/`check_decision` (which need `approvals.await`/`approvals.check` and `read` in the approval channel) and then run with `run_approved_action`; that `reason` is shown to a person; and that nothing is retried.

Refusals are `schema.Error` structured content, as today. New codes (§6): `admin_action_unknown` (400), `admin_action_not_allowed` (403: excluded target, approval channel, self), `admin_action_pending` (409, with the open approval's id in `message`), `too_many_pending_admin_actions` (429, `retry_after_seconds` 0: it clears when one resolves), `approval_not_approved`, `approval_already_run`, `approval_run_window_passed`, `approval_not_admin_action`, `target_changed` (all 409). Existing codes pass through from the dispatched route unchanged: `permission_denied` with `permission`, `forbidden` ("only an owner may act on an owner"), `last_operator`, `principal_not_found`, `channel_not_found`, `invalid_request`, `reauth_required` (never for a script credential), `rate_limited`.

MCP tool errors keep carrying no `permission` field for the *agent's own* refusals that predate this design; a `permission_denied` passed through from a dispatched route carries it, since the agent now can hold one. That is an additive change to the sentence in admin-roles §8.4.

### 3.7 Audit

Every action an AI administrator takes leaves the rows a human operator's would (`principal_disabled`, `member_added`, `invite_created`, …) with actor `principal:<agent id>`, which the trail already resolves to a principal of kind `agent`. On top:

| Action | Actor | Subject | Detail |
| --- | --- | --- | --- |
| `admin_grant_changed` | the owner (or the `manage_members` holder who revoked) | `principal:<agent>` | `permissions=<before>-><after> approval_channel=<id>` |
| `admin_action_ran` | the agent | `action:<name>` (direct) or `approval:<id>` (gated) | `route="…" status=<n> via=mcp|rest` |
| `admin_action_requested` | the agent | `approval:<id>` | `action=… route="…" target=…` |
| `admin_action_refused` | the agent | `action:<name>` or `approval:<id>` | `reason=<code>` |
| `ai_admin_created`, `ai_admin_revoked` | the owner / host (`system`) | `principal:<agent>` | written by the setup and revoke commands' composite, so one row says "an AI administrator was set up here" |

A `via` of `rest` happens when a human or an agent calls the door over REST directly; it is the same code path.

### 3.8 Rate limits and bounds

- `admin.act` is rate-limited by the manifest like any capability; the setup command writes 60/hour. A refused action spends nothing (the dispatcher refuses before `spend`).
- At most `admin.max_pending` (default 8, hard cap 32) open gated approvals per agent; the ninth is `too_many_pending_admin_actions`. The same action on the same target with the same params while one is pending is `admin_action_pending`, naming the existing approval, so an injected loop cannot fill the inbox with duplicates.
- Member links the agent makes are clamped (§3.3).
- The dispatched routes keep their own bounds (`MaxPostRequestBytes`, invite limits, the daily channel cap, which does not apply to `manage_channels`).

### 3.9 Live effects

- Revoking the grant (manifest write) takes effect on the next request (rights are read per request); an in-flight `await_decision` re-reads the manifest on every poll and ends when `approvals.await` goes (`scope.refresh`, `authz.go:287`). Open gated approvals of a revoked agent stay decidable but **cannot be run**: the run re-checks the grant. The console shows them as "requested by an agent that no longer holds the permission".
- Disabling the agent revokes its credentials and ends its sessions as for any principal; its approvals are left as above.
- Changing `approval_channel_id` leaves earlier approvals in the old channel, decidable there; new ones go to the new channel.

## 4. Setup across many servers

### 4.1 The CLI command (online, as an owner)

```
conch admin ai-admin create <name> [--server <url>]
    [--channel <name>]                       default: admin (created private if absent)
    [--grant <perm,...>]                     default: manage_members,manage_channels,make_member_links
    [--require-approval <action,...>]        widens the gated set for this agent
    [--ttl <duration>] [--run-window <duration>] [--quorum <n>] [--max-pending <n>]
    [--credential-days <n>]                  default: 90, renewable (idle expiry)
    [--client json|claude-code]              default: json
conch admin ai-admin list
conch admin ai-admin show <id>
conch admin ai-admin revoke <id>             disables the principal: credentials, sessions, sockets
```

`create` composes existing routes, in this order, as the signed-in owner (a non-owner is refused at step 4 by the store, after which the command rolls back what it made by disabling the principal and says why): (1) `POST /v0/principals {kind: agent, name}`; (2) `GET /v1/directory/channels` and, if `<channel>` is absent, `POST /v1/channels {name, visibility: private}` (the owner becomes member and manager); (3) `PUT /v1/channels/<channel>/members/<owner> {can_decide: true}` (idempotent) and `POST /v1/channels/<channel>/members {principal_id: <agent>, can_decide: false}`; (4) `PUT /v1/principals/<agent>/manifest` with `capabilities: [messages.read, messages.post, approvals.await, approvals.check, admin.act]`, `channels: [{<channel>: [read, post]}]`, `permissions`, `admin {approval_channel_id, …}`, `rate_limits: [{admin.act, 60, 3600}]`; (5) `POST /v1/principals/<agent>/credentials {label: "ai-admin", renew_lifetime_seconds: <days>}`. No route is added for setup: the manifest PUT that first grants permissions writes `ai_admin_created` itself (the store knows it is the first grant), and disable writes `ai_admin_revoked` when the target holds a grant.

`approvals.request` is **not** granted: the server raises the agent's admin approvals itself, and an AI administrator has no need to raise free-form ones. The owner may add it by hand.

Output: the MCP entry on **stdout**, alone, as the operator commands print a token; everything else on stderr; the token appears nowhere else, and the command refuses to run under a shell trace it can detect (`set -x` is the caller's; the README says so, as it does for CI).

### 4.2 The console action

Settings → Members → **Add AI administrator** (owner only; the button is absent for anyone else, and the server refuses the grant for anyone else): a form with name, approval channel (existing or new), the grant checkboxes with the two off-by-default ones under a warning, and credential lifetime; it runs the same seven steps through `api.ts` and shows the MCP entry once in `SecretOnce` with a copy button. The agent's card in Members shows "AI administrator: manage_members, manage_channels" and a **Remove access** action (= disable). The approvals inbox renders a `conch.admin_action.v1` payload as a card: verb, target, route, params, the agent's reason, and the two buttons.

### 4.3 The host command (offline, over SSH)

```
conchd ai-admin create --data <dir> --origin https://<host> <name>
    [--channel admin] [--grant …] [--require-approval …] [--decider <principal id>]…
    [--ttl …] [--run-window …] [--credential-days 90] [--client json|claude-code]
conchd ai-admin revoke --data <dir> <principal id>
```

Works on the database directly like `conchd operator bootstrap` (`cmd/conchd/operator.go`), audited as `system`, and prints the same MCP entry (stdout) using `--origin` for the URL, since `conchd` is never told its public address (the same rule as `--link --origin`). It refuses when no owner can sign in (nobody could decide) and, unless `--decider` names them, seats every owner who can sign in as a decider in the approval channel. A running server notices the new credential at once. This is the second entry for #1172's offline-command list.

### 4.4 The MCP client entry

The snippet, in the JSON that Claude Code reads from `.mcp.json` or its settings (`--client json`, the default):

```json
{
  "mcpServers": {
    "conch-alpha": {
      "type": "http",
      "url": "https://alpha.gankbears.com/mcp",
      "headers": { "Authorization": "Bearer conch_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX" }
    }
  }
}
```

and, with `--client claude-code`, the one-line form:

```
claude mcp add --transport http --scope user conch-alpha https://alpha.gankbears.com/mcp \
  --header "Authorization: Bearer conch_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
```

Both forms are the ones the Claude Code MCP documentation gives (`code.claude.com/docs/en/mcp`: an entry with `type: "http"`, `url` and a `headers` object; `claude mcp add --transport http <name> <url> --header "…"`, with `--scope local|project|user`); an entry with a `url` and no `type` is read there as stdio, which is why `type` is always printed. The command prints the token as a literal and recommends `--scope user`: a project-scope `.mcp.json` is a file people commit. Claude Code also expands `${VAR}` in `headers`, but its docs say some credential-shaped variable names are read as empty in a remote server's headers and the list could not be confirmed to exclude `CONCH_TOKEN`, so the design does not rely on expansion; an owner who prefers it edits the one line by hand and checks `claude mcp get <name>`.

The entry name is `conch-<server name>`, where the server name is the one `GET /v1/join` reports (`JoinInfoV1.server_name`, `CONCHD_SERVER_NAME`, default `conch`), lowercased, with anything but `[a-z0-9-]` replaced by `-`; `--name` overrides it. The three fields (URL, `/mcp`, token) are the whole configuration: no scheme guessing, no second endpoint. Other MCP clients that take a URL and headers take the same three values; the design does not catalogue their file formats.

**One entry per server.** Each server issues its own agent and token; the owner's client holds one entry per server (`conch-alpha`, `conch-chat`, …). Tool names are the same on every server, and the client disambiguates by the entry: Claude Code names every tool `mcp__<server>__<tool>` (its permission-rule docs), so the door on alpha is `mcp__conch-alpha__submit_admin_action` and an instruction "disable alice on alpha" resolves to one server. The same entry name in two scopes is a documented precedence trap (the higher scope's whole entry wins), so each server gets a distinct name and the command refuses to print one that another configured server already uses, when it can tell. There is no cross-server tool and no aggregator: a server is one community (ADR-013), and one AI holding N entries is N independent administrators. Rotation is per server (`conch admin credential issue` on that server, then replace the one line).

### 4.5 Revocation

One action per server: `conch admin ai-admin revoke <id>` or the console's Remove access, both `POST /v1/principals/{id}/disable`, which revokes every credential, ends sessions, drops sockets and writes `principal_disabled` and `ai_admin_revoked`. Narrowing without revoking is a manifest write (`conch admin manifest put`). Deleting the MCP entry on the client side revokes nothing and the docs say so.

## 5. Security

### 5.1 Prompt injection through chat content

An AI administrator reads channels (it holds `messages.read` where granted, and it reads its approval channel). A message it reads may contain instructions. The model may obey them. The design assumes it will, and bounds what obeying can do.

| What an injected message could make the agent try | What limits it |
| --- | --- |
| "Disable/hide/remove alice", "revoke her credentials" | Gated. The owner sees a server-rendered approval naming alice and the exact route, and the agent's reason. The owner's "no" is final; the agent cannot retry without a new approval, and duplicates are refused. |
| "Give me operator", "make me a manager", "let me decide approvals" | Roles: not in the catalog (owner-only route, unreachable). `manages`/`can_decide`: gated, and refused outright on the approval channel. The agent cannot seat a decider for its own requests. |
| "Issue me a token", "sign me in as the owner", "add a hook" | `act_as_members` is never granted; those routes are not in the catalog. Structurally unreachable. |
| "Give yourself more permissions", "edit your manifest" | The manifest routes are not in the catalog; `principalHolds` reads only what an owner wrote. An agent cannot create an agent (`create_member` forces `human`). |
| "Make me an invite link" | Direct, but clamped: ≤ 5 uses, ≤ 7 days, lands only where the agent could add people, never the approval channel, tied to the agent as maker so the owner sees who it brought in and can `disable_link_joiners`. The owner may move `create_member_link` to the gated set with `--require-approval` (O2). |
| "Add me to #private" | Direct, if the agent holds `manage_channels`. This is the widest direct action and the honest cost of "streamlined": the owner who does not want it gates it per agent, or does not grant `manage_channels`. It is audited `member_added` as the agent, and the member list shows it. Never on the approval channel. |
| "Make #private public" | Gated. |
| "Open sign-up", "change the permissions" | `change_server_settings` is off by default; when granted, every write is gated. |
| "Export the audit log and post it in #general" | `read_audit_log` is off by default. When granted, the export is a direct read; what the agent then *posts* is bounded by its `post` grants and the rate limit, and is the same exposure as a human holder posting it. The setup command warns in those words. |
| Flood the owner with approvals | `max_pending` (8), duplicate refusal, and `admin.act`'s rate limit. |
| Make the approval text lie | The title, body and payload are rendered by the server from the stored action; the agent's `reason` is capped, labelled and the only text of its own. The decider decides on the server's rendering. |
| Act after the owner said no, or act twice on one yes | The run reads the stored approval; `rejected`/`expired` never run; the claim row is one-shot; the run window closes a stale yes. |
| Act on the owner | Owner protection refuses a non-owner acting on an owner, in handler and store, for every route in the catalog. The last-owner guard holds. |

What the design does **not** do: it does not inspect or filter chat content, does not try to detect injection, and does not stop a granted direct action. The owner chooses the grant and the gate per agent; the defaults make everything irreversible or widening a decision.

### 5.2 A stolen token

The token is a bearer script credential. A thief gets: the direct actions of the grant, at the rate limit, under the audit log with the agent as actor; the ability to *request* gated actions, which still need the owner's yes on a server-rendered description; and nothing of `act_as_members`. Mitigations: renewable credentials with idle expiry (default 90 days), so an unused stolen token dies; `conch admin credential list` and `revoke` (one gated action for the agent itself, since self-targets are excluded, and one direct action for a human); the connection log names the client address; `ai-admin revoke` is one command. Not done: binding a credential to a client address or a mutual-TLS client certificate.

### 5.3 Privilege creep

- The grantable subset, the catalog and the gate defaults are code, with a structural test that fails when a route is reachable by dispatch outside the catalog, when a catalog row names a route or permission the route table disagrees with, or when a `humansOnly`-style wrapper admits dispatch on a route outside the catalog.
- Only owners grant; a manifest write by anyone else that carries permissions is refused in the store.
- `require_approval` only widens the gated set.
- A permission added to ADR-014's catalog later is **not** agent-grantable until someone adds it to `AgentGrantable()` with a design note, like the rule that a new permission defaults to the old behaviour.
- The agent never holds `can_decide`, `manages`, a role, the mark or `act_as_members`; the store refuses the first three for agents already (§1.1) and the schema refuses the last.

### 5.4 Self-approval

Impossible by the three walls of §3.5.4, each with a test in the end-to-end chain: an agent credential on the decisions route (`human_required`), a raw `channel_members` row with `can_decide = 1` for an agent not counting in the store, and the approval-channel protection refusing the agent's roster changes.

### 5.5 Owner lock-out

Unchanged guards: the agent cannot act on an owner, cannot change roles, cannot disable or hide the last owner who can sign in, and cannot change the operator column. New: the agent cannot change its approval channel, so it cannot remove the owner from the one place the owner says no. An owner who loses their own credentials recovers with `conchd operator recover` as today, which also lets them revoke the agent offline (`conchd ai-admin revoke`).

### 5.6 Other

- **Confused deputy by name.** Targets are ids; names are shown and re-checked at run time (`target_changed`).
- **Audit flooding.** The agent's refusals are thinned by the existing coalescer; its successful actions are rate-limited by `admin.act`.
- **Downgrade.** An older `conchd` ignores the two new manifest columns (it does not select them) and refuses agents on every admin route as today, so a downgrade silently removes AI administration and nothing else. An older `conch` or browser shows the admin approval's body (plain text) and decides it like any approval.
- **The re-authentication class** does not apply to script credentials (§1.2). This design relies on the approval gate instead and says so; it does not add a second factor for agents.

## 6. Schema impact

All additive. **No version bump is needed**, by the rules of `agent-manifest.md` §6 and the schema-change skill: new optional fields, new types, new vocabulary values, new error codes; every published fixture stays byte-identical. Flagged for Nick because the brief asks: the one thing that *reads* like a semantic change is the comment and design sentence "agents hold no permission" (`permission.go:15`, `whoami.go` on `Permissions`, admin-roles §3.3), which becomes "agents hold only what an owner granted in their manifest". That is a change of rule, recorded in the ADR amendment (§8), not a change of any wire shape's meaning: `whoami.permissions` still means "what the caller holds now".

| Shape | New or changed | Where |
| --- | --- | --- |
| `AgentManifestV1.permissions`, `.admin`; `PutAgentManifestRequestV1` likewise | new optional fields | `pkg/schema/agent_manifest.go`; new fixture `agent-manifest-v1-admin.json`; existing fixtures unchanged |
| `AgentAdminV1` | new struct | `pkg/schema/agent_manifest.go` |
| `Capability` gains `admin.act` | vocabulary addition | `agent_manifest.go`, `vocabularies.go`, `Capabilities()`, `MCPToolCapabilities` (three entries) |
| `Permission.AgentGrantable()` | new method, no new values | `permission.go` |
| `AdminAction` | new closed vocabulary (the catalog) | `pkg/schema/admin_action.go`, `vocabularies.go` |
| `AdminActionV1` (`conch.admin_action.v1`) | new registered payload | `admin_action.go`; fixture `admin-action-v1.json` |
| `AdminTargetV1 {kind, id, name}` with `AdminTargetKind` (`principal`, `channel`, `credential`, `hook`, `invite`, `report`) | new | `admin_action.go` |
| `SubmitAdminActionRequestV1`, `AdminActionResultV1` (`status` ∈ `AdminActionStatus` {`ran`, `pending_approval`}), `AdminCatalogResponseV1` with `AdminCatalogEntryV1 {action, route, permission, gate, held, gated}` | new REST bodies | `admin_action.go` |
| Error codes: `admin_action_unknown`, `admin_action_not_allowed`, `admin_action_pending`, `too_many_pending_admin_actions`, `approval_not_approved`, `approval_already_run`, `approval_run_window_passed`, `approval_not_admin_action`, `target_changed` | new constants | `admin_action.go` |
| Audit actions of §3.7 | new constants | `internal/server/store` (not wire) |
| TypeScript: every new vocabulary and body through `go generate ./pkg/schema` | generated | `web/src/schema/` |

Nothing in `ApprovalV1`, `ApprovalResolutionV1`, `Decision`, the MCP approval outputs or the message envelopes changes.

## 7. Migration

One migration, additive, no audit row (it changes no behaviour until an owner grants something):

- `ALTER TABLE agent_manifests ADD COLUMN permissions TEXT NOT NULL DEFAULT '[]'` and `ADD COLUMN admin TEXT NOT NULL DEFAULT ''` (the JSON of `AgentAdminV1`, empty for none), read with the same strict decoder as the other lists.
- `CREATE TABLE admin_action_runs (approval_id INTEGER PRIMARY KEY REFERENCES approvals (id), agent_id INTEGER NOT NULL REFERENCES principals (id), claimed_at INTEGER NOT NULL, ran_at INTEGER, http_status INTEGER, error_code TEXT)`.

Rollback is the previous binary plus the pre-upgrade backup, as for every migration; the previous binary ignores both additions. No data is rewritten; no existing agent gains anything.

## 8. Proposed ADR text (for Nick; not edited into the ADRs)

### 8.1 ADR-010, amendment to decision 6

> **Amended 2026-11-xx ([#1326](https://github.com/njdaniel/conch/issues/1326), design [docs/design/ai-administrators.md](../design/ai-administrators.md)).** Decision 6 stands for roles, the owner mark and the manager grant: no agent is an owner, operator or manager, and the store refuses each. It is narrowed in one way: an agent may hold a **subset of the operator permissions** (`manage_members`, `manage_channels`, `make_member_links`, `change_server_settings`, `read_audit_log`; never `act_as_members`), granted only by an owner, only in the agent's manifest, enforced by the same `can()` and `permittedTx` as an operator's, and exercised only through the admin-action door (`POST /v1/admin/actions` and its MCP tools), where a fixed set of irreversible or widening actions is gated by an approval a human decides. The agent is an operator in standing for those permissions and nothing else. D4 ("grants never exceed the granter") holds: only owners grant. D5 (the last-owner guard) and D2's owner protection hold unchanged for an agent actor. The consequence "no MCP tool for administration" is superseded by this amendment for the three admin tools; parity holds because the door is a REST route first.

### 8.2 ADR-014, amendments to decisions 1, 6 and 7

> **Amended 2026-11-xx (#1326).**
> - **Decision 1,** "Agents … hold no role and no permission; their rights stay their manifest's capabilities and channel grants": agents still hold no role. An agent's manifest may now also list permissions from the agent-grantable subset of the catalog (`Permission.AgentGrantable()`), and the agent then holds exactly those, with no cell, brake, column or dependency applied. Everything else an agent may do stays its capabilities and channel grants (ADR-000 D10).
> - **Decision 6,** the helper: `permittedTx`'s first rule becomes "an agent holds exactly its manifest's agent-grantable permissions; a guest, or a disabled or hidden principal, holds nothing". The store's standing functions ask the permission before refusing a non-human actor, so a granted agent has operator standing for `manage_channels` where it holds it and no standing where it does not. Every change to these is approval-path, as the consequences already say.
> - **Decision 7,** "No MCP tool is added: administration is a human act, and parity holds through REST": three MCP tools are added (`describe_admin_actions`, `submit_admin_action`, `run_approved_action`) fronting three REST routes, and administration by an agent is a human act *at the gate*: the server raises an approval for every gated action and a human decides it.
> - The security surface listed in the consequences gains: the manifest's permission grant (owner-only), the dispatcher, the three routes, the agent-admitting branch of `accessPermission` and of the human-only wrappers on catalog routes, the one-shot run, and the approval-channel protection. Every implementing PR gets a `security-reviewer` pass; the dispatcher, the run and the standing-function change are `approval-path`, merged by Nick.

### 8.3 ADR-013, one consequence line

> "No MCP tool is added: joining, the directory and administration are human acts, and parity holds through REST (ADR-001)." becomes: "… joining and the directory are human acts; administration by an agent is possible from #1326 on, through REST routes the MCP tools front, with a human deciding every gated action."

### 8.4 ADR-001 and ADR-000

No text changes. ADR-001's minimum tool set is unchanged and the new tools obey its rule ("an MCP tool that can do something the API cannot … blocks": the door is REST first). ADR-000 D10 is unchanged: capabilities say which tools an agent may call, the manifest is the whole grant, enforcement is server-side. Issue #1172's parity-exception list gains `conchd ai-admin create|revoke` beside the operator commands.

## 9. Open decisions for Nick

Each with the recommendation the slices in §10 assume.

1. **Where the grant lives: the manifest (recommended), or a separate owner-only route (`PUT /v1/principals/{id}/admin-grant`).** The manifest is already the agent's whole grant (D10), revocation is one write, and the strict decoder makes a downgrade safe. The cost is that the manifest PUT becomes owner-only when it grants, which the store decides. A separate route would keep the manifest route's class untouched but split an agent's rights across two documents. *Recommend: the manifest.*
2. **The default gated set** (§3.3). The issue's direction gates disable, hide, remove, revoke, roles and permissions, and runs member links, adds and channel creation directly. The table follows it and adds: enable, visibility, archive, `manages`/`can_decide`, bulk disables, resume-invites and every `change_server_settings` write. Member links stay direct but clamped. *Recommend: the table as written; the owner widens per agent with `require_approval`.*
3. **May the owner narrow the gated set** (make `disable_member` direct for a trusted agent)? *Recommend: no, in v1.* The gate is the compensating control for a bearer token with no second factor (§5.6); an owner who wants an ungated AI has today's workaround. Revisit with evidence.
4. **Bulk routes in the catalog** (`POST /v1/principals/hide`, bulk guest ends when guests return). *Recommend: out of v1.* One target per approval keeps the decider's picture exact.
5. **Grantable subset: include `change_server_settings` and `read_audit_log`?** Both off by default for operators too. *Recommend: grantable but never by default, with the console's warning text; the first lets an AI manage a personal server's preset, the second lets it answer "who did what".* Excluding them is one line each if he prefers.
6. **Approval TTL and run window defaults:** 24h and 24h. *Recommend as written;* the phone push makes a day generous, and a stale yes should not run a week later.
7. **Run on approval without the agent calling back** (the server executes at resolution). *Recommend: no.* It couples the state machine to execution (a failed run after a resolved approval needs a new state), it runs with no caller to answer, and it removes the agent's re-check of its own grant at run time. The agent's `await_decision` already makes the round trip one call.
8. **Credential defaults for AI administrators:** renewable, 90-day idle expiry. *Recommend as written;* an unused token dies, a used one renews, rotation is one command.
9. **The host command's deciders:** every owner who can sign in, unless `--decider` names them. *Recommend as written.*
10. **The approval channel's name:** `admin`, private, one per server, created if absent. *Recommend as written;* on a server that already has an `admin` channel the command uses it and says so.
11. **Version bump:** none proposed (§6). *Recommend: confirm no bump;* all shapes are additive and the fixtures prove it.
12. **ADR text:** §8 as written, or edited. Implementation stays blocked until the amendment merges.

## 10. Implementation slices

Ordered; file areas named so that S1, S5 and S8 can be dispatched in parallel once S0 (the ADR amendment) merges, and S3/S4 after S2. The approval-path slices are marked and merged by Nick with the rule 3 test. Every slice gets a `security-reviewer` pass (the issue carries `security`).

### S0. ADR amendment (Nick)

- **Context:** §8 of this note into ADR-010, ADR-014 and ADR-013, by Nick's hand or a docs-agent PR he merges.
- **Files:** `docs/adr/ADR-010-delegated-administration.md`, `ADR-014-…`, `ADR-013-…`; this note's status line.
- **Acceptance:** the three amendments present; this note marked Accepted with the date.
- **Tests:** docs-only checks.
- **Blocked by:** nothing.

### S1. Schema: manifest grant, catalog, payload, REST bodies

- **Context:** §3.1, §3.3's vocabulary, §3.5.1's payload, §3.4's bodies, §6. Additive; no bump.
- **Files:** `pkg/schema/agent_manifest.go` (fields, `AgentAdminV1`, `CapabilityAdminAct`, three mapping entries, `Validate` rules: grantable only, `admin` required with permissions, bounds on the TTL/window/quorum/max_pending), `pkg/schema/permission.go` (`AgentGrantable`), new `pkg/schema/admin_action.go`, `pkg/schema/vocabularies.go`, `pkg/schema/testdata/agent-manifest-v1-admin.json`, `admin-action-v1.json`, `docs/design/agent-manifest.md` §1, §3, §6, §9; `go generate ./pkg/schema` output under `web/src/schema/`.
- **Acceptance:** existing fixtures byte-identical (`scripts/schema-compat.sh`); `Validate` refuses `act_as_members` and the six member permissions in `permissions`, refuses permissions without `admin`, refuses an unknown `AdminAction`; `AdminActionV1` round-trips its fixture; `MCPToolCapabilities` has twenty entries and `currentMCPTools` is updated; the TS check passes.
- **Required tests:** table-driven `Validate` cases for every refusal above; fixture tests; vocabularies test.
- **Blocked by:** S0.

### S2. Store: granted agents hold permissions; owner-only grant; deciding is human (approval-path)

- **Context:** §3.2, §3.1 "who writes it", §3.5.4 wall 2, §7's migration.
- **Files:** `internal/server/store/permissions.go` (`principalHolds` rule 1a; the manifest read), `store/manifests.go` (two columns, strict decode, owner check in the PUT transaction, `admin_grant_changed`, `ai_admin_created` on first grant), `store/members.go` and `store/channels_rename.go` (standing order), `store/invites.go`, `store/invites_v2.go` (`CreateInvite`, `makerStillAllowed`, `effectiveChannelsTx`, `CloseInvite`, `SetMayInvite`), `store/approvals.go` (`p.kind = 'human'` in the right check and the quorum count), `store/disable.go` (`ai_admin_revoked`), the migration in `store/store.go`, `admin_action_runs` table.
- **Acceptance:** an agent with no grant is refused everywhere exactly as at `d7afd8c7` (the existing isolation rows pass unchanged); a granted agent passes `permittedTx` for its grant only; a manifest PUT with permissions by a non-owner is `ErrNotOwner`; by an owner it audits; emptying it by a `manage_members` holder works; an agent's `can_decide = 1` row (written raw in the test) counts for nothing; the structural test of admin-roles §7.1 still passes with no new allowlist entry.
- **Required tests:** store tests for each; **the rule 3 end-to-end chain** extended with: a granted agent's raw decide row not counting; a granted agent refused as a manager where it lacks `manage_channels`.
- **Blocked by:** S1.

### S3. Server: the door, the dispatcher, the gate, the one-shot run (approval-path)

- **Context:** §3.3–§3.5, §3.7, §3.8, §3.9.
- **Files:** new `internal/server/admin_actions.go` (catalog table, clamps, exclusions, approval-channel protection), `admin_dispatch.go` (in-process request, mark, response capture), `admin_handlers.go` (three routes), `routes.go` (three rows; `accessPermission` and `accessOperator` dispatch branch), `auth.go` (middleware trusts the mark), `authz.go` (`humansOnlyBecause` admits dispatch on catalog routes), `invites_v2.go` (`human_required` likewise), `approvals_handlers.go` unchanged, `store` queries for the run claim.
- **Acceptance:** every catalog row dispatches and answers what the route answers for an operator, including each refusal code; every excluded route is unreachable by dispatch; a gated action creates an approval with the server-rendered title/body/payload and `admin_action_requested`; `run` enforces every check of §3.5.2 in order, claims once, and `admin_action_ran`/`refused` rows follow; the approval channel is protected; `max_pending` and duplicates refuse; a direct action by an agent over REST and over the door audit identically with `via`.
- **Required tests:** **the rule 3 end-to-end chain for a gated action**: `submit_admin_action` → `approval_created` → `admin_action_requested` → `notify_sent` → the owner's `decision_cast` → `approval_resolved` → `run_approved_action` → `admin_action_ran` → `principal_disabled`, asserted in log order; the negative cases: rejected, expired, run twice, run after the window, run by another agent, run after the grant was revoked, run after the target was renamed, the agent trying to decide (`human_required`), the agent trying to add a decider to the approval channel (`admin_action_not_allowed`); a structural test over the route table, the catalog and the wrappers (§5.3); an isolation-check prober for a granted agent (`e2e/isolation`): every `accessPermission` route direct → refused as today; through the door → the catalog's answer.
- **Blocked by:** S1, S2.

### S4. MCP: the three tools

- **Context:** §3.6.
- **Files:** `internal/server/mcp.go` (three `addAgentTool` registrations, input/output structs with `jsonschema` tags, projections), `mcp_test.go`, `pkg/schema/agent_manifest_test.go` (`currentMCPTools`), README §5 (and fix "Thirteen tools" to the real count).
- **Acceptance:** each tool answers what its route answers, with the same codes; an agent without `admin.act` gets `forbidden` and an `access_denied capability_not_granted` row before any dispatch; the rate limit is spent once per call; `await_decision` watches an admin approval and ends when the grant is revoked.
- **Required tests:** parity test per tool against the REST route; the end-to-end chain of S3 driven over MCP (the dogfood scenario of S9 in test form).
- **Blocked by:** S3.

### S5. CLI: `conch admin ai-admin` and the snippet

- **Context:** §4.1, §4.4, §4.5.
- **Files:** new `internal/cli/admin_ai_admin.go`, `admin.go` (verbs), `client_members.go`/`client_permissions.go` (manifest fields), `internal/cli/parity_test.go`, README.
- **Acceptance:** `create` performs the seven steps and prints exactly the snippet on stdout and nothing else there; a non-owner is refused at the manifest step and the principal is disabled; `--client claude-code` prints the one-liner; `list`/`show`/`revoke` work; `conch admin manifest put` accepts the new fields.
- **Required tests:** CLI tests with a fake server for each verb, stdout/stderr separation, the entry-name derivation from `server_name`; a hostile test that the token never appears on stderr or in an error.
- **Blocked by:** S1, S2 (the manifest PUT must accept the grant). Can be dispatched in parallel with S3.

### S6. Host command: `conchd ai-admin create|revoke`

- **Context:** §4.3.
- **Files:** new `cmd/conchd/ai_admin.go`, `main.go` (usage, dispatch), tests beside `operator_test.go`; `docs/deploy.md`; #1172's list.
- **Acceptance:** offline on the data directory; refuses with no owner able to sign in; seats deciders; prints the snippet with `--origin`; audits as `system`; a running server accepts the new token at once.
- **Required tests:** table-driven as `operator_test.go`; refusal cases; the printed entry parses as the JSON of §4.4.
- **Blocked by:** S1, S2. Parallel with S3 and S5.

### S7. Web: console action, agent card, approval card

- **Context:** §4.2.
- **Files:** `web/src/admin/MembersSection.tsx`, `PersonActions.tsx`, new `AddAIAdmin.tsx`, `SecretOnce.tsx`, the approvals inbox component, `api.ts`, generated `web/src/schema/`.
- **Acceptance:** the button is owner-only; the seven steps run; the entry is shown once; the agent card shows the grant and Remove access; a `conch.admin_action.v1` approval renders as a card with the two buttons; unknown payloads still show the body.
- **Required tests:** unit tests without network; one Playwright test for the create flow against a test server.
- **Blocked by:** S1, S3.

### S8. Docs, CHANGELOG, dogfood

- **Context:** README (agents, MCP, administration), `docs/design/approval-object.md` §7 (a note that admin approvals carry a registered payload and are run by their requester), `docs/alpha-tester-guide.md` if testers see admin approvals, CHANGELOG upgrade note ("agents hold no permission unless an owner grants one; nothing changes on upgrade").
- **Files:** as named; `.claude/skills/dogfood-check/SKILL.md` gains the AI-administrator loop as a second scenario.
- **Acceptance:** docs-only checks pass; the dogfood scenario runs green against a built `conchd`.
- **Blocked by:** S3, S4, S5.

### S9. Dogfood run before release

- **Context:** the canary: an AI client (Claude Code with the printed entry) disables a test member through the gated path on a staging server, the owner approves from the phone, the audit chain is asserted. Run by the principal, labelled agent-run.
- **Blocked by:** S8.

## 11. What a reviewer should push on

- Whether the dispatch mark can be forged or leaked into a network request's context (S3's middleware test is the proof; look for any `context.WithValue` of the mark outside the dispatcher).
- Whether any route outside the catalog becomes reachable by a dispatched agent through a wrapper that admits the mark too broadly (the structural test).
- Whether `principalHolds` rule 1a can answer true for an agent whose manifest an operator wrote with permissions (the store's owner check must be in the same transaction as the write, and a stored row that somehow carries permissions written before the check existed must be read as empty by a one-off migration check, or refused).
- Whether the run's claim and the dispatched handler's own transaction can interleave with a revoke so that the action runs after the grant is gone (the handler's `permittedTx` re-check is the answer; a test should race them).
- Whether the server-rendered body can be made to include attacker-chosen text beyond the labelled `reason` (target names are the attacker's choice too, when the attacker chose the target's name: the card shows the id beside it).
- Whether the clamps on `create_member_link` are applied before validation so a refused clamp cannot be told from a refused body.
