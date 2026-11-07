# Design: Preview as member, a read-only permission preview for administrators

- **Status:** Accepted by Nick (Tier-H), 2026-11-07 (recorded on #1007): §9's recommendations, except point 5. Nick asked to see the view of roles as well as people, so the role simulation (§8) ships with the member preview and isn't deferred. Previously: Proposed, 2026-11-04, for issue [#1007](https://github.com/njdaniel/conch/issues/1007). Nothing here is implemented. The open points of §9 are Nick's (Tier-H) to decide; the slices of §10 are filed only after he has.
- **Facts on main** are as of `d7afd8c797d0d95fd2ec0836c6ff2ac3df254539` (the merge of PR #1304, 2026-11-04). Every claim in §1 names a file and a function at that commit. Line numbers drift; search for the function.
- **Builds on:** [ADR-010](../adr/ADR-010-delegated-administration.md) and [delegated-administration.md](delegated-administration.md) (owner mark, owner protection, "grants never exceed the granter", the route classes, the order of checks); [ADR-013](../adr/ADR-013-one-community-model.md) and [community-model.md](community-model.md) (one community, private channels, the may-know predicate of §13.2, the uniform 404s); [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md) and [admin-roles.md](admin-roles.md) (the permission catalog, `can()` and `permittedTx`, the admin console).
- **Owner:** protocol-designer (this note, the shape in §6); server-engineer, web-engineer and cli-engineer for the slices in §10.

## 0. Why

Nick asked on 2026-10-30 for a way to check what another member sees, in the spirit of Discord's role preview. His case: the owner shares private channel A with alice and private channel B with bob, member hiding is on, and he wants to open alice's view, confirm she sees only her permitted channels, herself and the owner, with the controls she holds, then come back to his own view.

Today there is no such thing. The only way to see a member's view is to sign in as that member, which an operator can do by issuing a credential or a sign-in link (`act_as_members`). That creates a credential and a session for the target, marks them online, moves their read state, and is exactly the power ADR-014 split out and named "can sign in as, and post as, any member", so that the owner can switch it off. A preview must be a weaker thing: a **read**, under the administrator's own identity, that shows the target's effective permissions and visibility, executes nothing, touches no state of the target, and shows no content the administrator could not read as themselves.

## 1. Current state at `d7afd8c7`

### 1.1 Merged behaviour, by file and function

| Fact | Where |
| --- | --- |
| **One principal per request.** `authMiddleware` resolves the bearer (`store.ResolveRequestCredential`, `internal/server/store/resolve.go`) and puts it in the context with `withCaller`; everything reads it back with `callerFrom`. A ticket-authenticated socket request goes through `redeemWSTicket` and the same `withCaller`. There is no second, "acting as" value in the context. | `internal/server/auth.go`: `authMiddleware`, `withCaller`, `callerFrom`, `withCredentialID`, `credentialIDFrom`; `internal/server/operator_auth.go`: `withRequestCredential`, `requestCredentialFrom`; `internal/server/tickets.go`: `redeemWSTicket` |
| **The audit actor is the caller.** `auditActor(ctx)` returns `principal:<id>` of `callerFrom`, or `system`. Refusals are audited by `denyForbidden` and `denyForbiddenFor` as `access_denied`, subject the route pattern, detail the code and the permission. | `internal/server/auth.go`: `auditActor`, `denyForbidden`, `denyForbiddenFor` |
| **Route classes.** `type access int`: `accessAuthenticated` (zero value), `accessOperator`, `accessOwner`, `accessChannelManager`, `accessOperatorOrSelf`, `accessPermission`, `accessExempt`. `type route struct` carries `pattern`, `access`, `handler`, `acceptsTicket`, `perm` and `memberRefusal`; `needs(perm)` sets the permission class. `routeTable()` is the single source for registration and policy; `guard(rt)` enforces the class before the handler, with `recentAuthGate` inside it. `TestRouteTable` pins each route's class. | `internal/server/auth.go`: `access`, `route`, `guard`; `internal/server/routes.go`: `routeTable`; `internal/server/auth_test.go`: `TestRouteTable` |
| **The one authority question** is `can(ctx, caller, perm)`, which calls `store.Permitted(ctx, caller.ID, perm)`: it resolves **by id** and re-reads the principal row. `permittedTx` reads the principal and the document; `principalHolds` answers in order: not human, guest, disabled or hidden → false; owner → true; the role's cell (fixed cells in code, `permissionDoc.cell`) AND `brakeWithholds` AND every `Requires()`. `HeldBy(ctx, p, perms...)` answers for an already-read principal, reading the document only (the events socket uses it). `EffectivePermissions(ctx, id)` lists what a principal holds in catalog order, with the revision. | `internal/server/auth.go`: `can`, `canOrFail`; `internal/server/store/permissions.go`: `permittedTx`, `principalPermittedTx`, `principalHolds`, `brakeWithholds`, `Permitted`, `HeldBy`, `EffectivePermissions` |
| **The catalog** has twelve permissions at this commit: ADR-014's eleven plus `use_camera` (ADR-015). Fixed cells: the member cell of every operator-only permission is off; the operator cell of `see_member_directory` is on. `enforcedPermissions` lists the ones every gate reads, pinned by `TestEnforcedPermissionsAreHonest`. | `pkg/schema/permission.go`: `Permissions`, `Permission.Fixed`, `Permission.Default`, `Permission.Requires`; `internal/server/permissions.go`: `enforcedPermissions` |
| **Owner protection.** `isOwner(p)` is `p.Role == operator && p.Owner`. `ownerProtected(w, r, targetID)` writes 403 `forbidden` ("only an owner may act on an owner") when a non-owner acts on an owner, audited; an unknown target falls through to the handler's 404. The store repeats it in `ownerProtectedTx`. | `internal/server/auth.go`: `isOwner`, `ownerProtected`; `internal/server/store/queries.go`: `Principal.IsOwner`, `ownerProtectedTx` |
| **Standing in a channel.** `memberActorStandingTx` gives owner standing to an owner, operator standing to a holder of `manage_channels`, manager standing to a `channel_members.manages = 1` row, and refuses everyone else; `ManagesChannel` reads the grant. The handlers take the manager branch on `!can(manage_channels)` (`memberWriteAsManager`). | `internal/server/store/members.go`: `memberActorStandingTx`, `ManagesChannel`, `checkManagerAddTargetTx`; `internal/server/members.go`: `memberWriteAsManager` |
| **Who may know whom** (community-model §13.2, as ADR-014 recast it). `knownPrincipalsSQL(viewerID, seeAll)` is "the one place that says which principals a viewer may know": with `see_member_directory` it is everyone (`DirectoryScopeEveryone`); without, `knowsCond`: self, people the viewer's links brought in, people sharing a channel (`DirectoryScopeSharedChannels`). `mayKnowTx` asks it for one target; `ListDirectory(ctx, DirectoryViewer{ID, SeeAll, Operator}, after, limit)` pages the directory through it and fills the operator fields only for `Operator` (`manage_members`). The handler sets `SeeAll` from `can(see_member_directory)` and `Operator` from `can(manage_members)`. | `internal/server/store/directory.go`: `knowsCond`, `knowsCondExcept`, `knownPrincipalsSQL`, `mayKnowTx`, `DirectoryViewer`, `ListDirectory`; `internal/server/directory.go`: `handleListDirectoryMembers` |
| **Channel content is membership-bounded, with no operator exemption.** `channelForCaller` resolves a channel by name, requires the caller to be a member (`callerIsMember`) and answers a non-member with `writeChannelNotFound` (404 `channel_not_found`, one body for "does not exist" and "not yours"). Operators get no read exemption on content. | `internal/server/members.go`: `channelForCaller`, `callerIsMember`, `writeChannelNotFound` |
| **A channel's member list** passes `channelMembersGate`: a member, or a holder of `manage_channels` (`seesAll`), else the uniform 404. `memberDocs`/`memberDoc` show `manages` to everyone, `role` and every `can_decide` only to a privileged viewer (holds `manage_channels` or manages the channel), and otherwise the viewer's own `can_decide` only. | `internal/server/members.go`: `channelMembersGate`, `handleListChannelMembers`, `memberDocs`, `memberDoc` |
| **The channel list and directory.** `GET /v1/channels` lists the caller's channels (`ListChannelsForPrincipal(caller.ID)`, `ChannelV0` with `manages`, `visibility`, `default`, `archived_at`). `GET /v1/directory/channels` lists public channels and the caller's own private ones (`ChannelDirectory(ctx, viewerID, all)`: `visibility = 'public' OR m.principal_id IS NOT NULL`); `?scope=all` needs `manage_channels`. Both routes are `accessAuthenticated`; the directory is wrapped `humansOnlyBecause` + `membersOnly`. | `internal/server/channels.go`: `handleListChannels`, `channelDoc`; `internal/server/channel_join.go`: `handleChannelDirectory`; `internal/server/store/channels_self.go`: `ChannelDirectory`; `internal/server/store/members.go`: `ListChannelsForPrincipal` |
| **The authors route** names the ids that authored a message the caller may read, through message visibility, not the may-know predicate. | `internal/server/members.go`: `handleListChannelAuthors`; `internal/server/store/audience.go`: `ListChannelAuthors` |
| **Message readers** are built by `readerFor(r, version)` from `callerFrom`; `TestMessageReadsStayInTheVisibilityFunction` allows only `readerFor`, `store.ChannelWideOnly` and the MCP scope's `reader()` to construct one. | `internal/server/messages.go`: `readerFor`; `internal/server/visibility_guard_test.go` |
| **whoami** answers id, kind, name, role, `owner`, `server_version`, `may_invite`, `voice_allowed`, `invites_remaining` (unless the caller holds `manage_members`), `identity_class`, `guest`, `auth` (the password and authenticator state), `server_mode`, `permissions` and `permissions_revision`. There is no `manages` in whoami; it is on each channel of `GET /v1/channels`. | `internal/server/auth.go`: `handleWhoAmI`, `voiceAllowed`; `pkg/schema/whoami.go`: `WhoAmIResponseV1` |
| **Side effects of an ordinary read.** Every REST request on an idle-bounded credential (an operator's browser session) calls `NoteGuestUse` (moves `credentials.last_used_at`), and `renewCredential` slides a renewable credential's expiry. Both act on the **caller's** credential. | `internal/server/auth.go`: `authMiddleware`; `internal/server/store/guests.go`: `NoteGuestUse`; `internal/server/store/resolve.go`: `renewCredential` |
| **Presence, read state and media are explicit writes.** Sessions: `POST /v1/sessions` (`handleCreateSession`), heartbeats (`handleSessionHeartbeat` → `HeartbeatSession` writes `last_seen_at`), and the events socket's `&session=` keeps a session alive (`eventsStream.run` → `keepSessionAlive`). Read state: `PUT /v1/channels/{channel}/read-state` (`handlePutReadState` → `MarkRead`); `GET /v1/read-state` is a read. Voice: `POST …/voice/session` (`handleVoiceSession`) mints a LiveKit token behind `voicePresenceChannel` (membership, then `requireVoicePermission(use_voice)`) and audits `voice_session_issued`. | `internal/server/sessions.go`, `internal/server/events_ws.go`, `internal/server/read_state.go`, `internal/server/voice.go` |
| **Live events are per channel and per principal.** `GET /v1/events/ws?channel=&session=` (ticket-accepting) resolves the channel with `channelForCaller` and subscribes `(channelID, principalID, credentialID, human)`. Every frame is built at send time for that subscriber: `stillMember`, `sendRoster` (via `HeldBy(viewer, manage_members, manage_channels)`), `membersFrame` (`memberDocs`), `sendReadStates`. Content-free per-principal signals exist: `conch.channels_changed.v1`, `conch.permissions_changed.v1`, `conch.directory_changed.v1`. Tickets are minted by `POST /v1/ws-tickets` bound to (principal, credential). | `internal/server/events_ws.go`: `handleEventsWS`, `eventsSub`, `notifyChannelsChanged`, `notifyPermissionsChanged`, `notifyDirectoryChanged`; `internal/server/tickets.go`: `handleCreateWSTicket`, `redeemWSTicket` |
| **Acting as someone is a named permission.** The four routes under `act_as_members` (`POST /v1/principals/{id}/credentials`, `POST /v1/credentials/{credential_id}/rotate`, `POST /v1/principals/{id}/sign-in-links`, `POST /v1/hooks`) are `reauthAlways` and owner-protected. `IssuedToOthers` lists what operators issued. | `internal/server/routes.go`; `internal/server/credentials.go`, `sign_in_links.go`, `hooks.go`; `internal/server/store/permissions.go`: `IssuedToOthers` |
| **Recent authentication** is per route (`recentAuthRoutes`, `reauthRule`: none, always, on others, operators, non-owners), enforced by `recentAuthGate` on the request credential's `RecentAuthUntil`. | `internal/server/routes.go`: `recentAuthRoutes`; `internal/server/operator_auth.go`: `recentAuthGate` |
| **MCP** authenticates agents only (`authenticateMCP`: `principal.Kind != store.PrincipalAgent` is refused) and registers seventeen tools, none of which lists members or administers anything. `/mcp` is `accessExempt`. | `internal/server/mcp_auth.go`: `authenticateMCP`; `internal/server/mcp.go` |
| **Audit rows** are `AppendAuditEvent(ctx, actor, action, subject, detail)` with `action` a plain string; constants are `Audit*` in the store files. Error codes are not a closed vocabulary; `forbidden`, `channel_not_found`, `principal_not_found`, `invalid_request` are string literals in `internal/server`, while `permission_denied` and its siblings are constants in `pkg/schema/permission.go`. `schema.Error` carries `code`, `message`, `retry_after_seconds`, `permission`. | `internal/server/store/queries.go`: `AppendAuditEvent`; `pkg/schema/message.go`: `Error` |
| **Structural tests a change here must satisfy:** `TestActorAuthorityReadsPermissions` (no role comparison to decide an actor's authority outside the allowlist), `TestEveryPermissionIsChecked`, `TestMessageReadsStayInTheVisibilityFunction`, `TestRecentAuthClass`, `TestRouteTable`, and `TestEveryRouteHasARow` in `e2e/isolation` (every route has an isolation row in every phase: base, guest, restricted operator, personal preset). | `internal/server/authority_guard_test.go`, `visibility_guard_test.go`, `operator_auth_test.go`, `auth_test.go`; `e2e/isolation/routes_test.go`, `main.go` |
| **Browser.** `api.ts` is the only module that holds the credential and talks to conchd; each request is an entry of the operation table (`platform/operations.ts`); the token lives under `conch.web.token`, in `localStorage` for a member and in `sessionStorage` for an operator. `App.tsx`'s `Session` holds auth, connection, channels and chat in reducers, reads whoami through `readWho`, and re-reads on `conch.permissions_changed.v1` and `conch.directory_changed.v1`. `agents/useAgents.ts` creates the browser's own session (`api.createSession`), opens the events socket with `&session=` and loads the open channel's members; `useReadState.ts` marks read. The admin console (`AdminConsole.tsx`) has Server, Roles and permissions, Members (`admin/MembersSection.tsx` → `CommunityMembers` in `Members.tsx`), Channels and Reports. Controls key on `canDo(who, permission)` (`admin/permissions.ts`) and `memberActions(who, target)` (`memberActions.ts`), both fed by `whoami.permissions`; mention completion (`mention.ts`) draws from the open channel's member list with no server call. Server switching exists only on the desktop (`platform/desktop`, `ui/ProfileBar.tsx`); the browser is one origin per server. | `web/src/api.ts`, `platform/operations.ts`, `platform/browser/credentials.ts`, `App.tsx`, `agents/useAgents.ts`, `useReadState.ts`, `AdminConsole.tsx`, `admin/MembersSection.tsx`, `admin/permissions.ts`, `memberActions.ts`, `mention.ts` |
| **Nothing like preview exists.** No hit for preview-as, view-as, impersonation or acting-as in Go, TypeScript or the docs beyond the `act_as_members` permission and the invite and sign-in-link previews (`POST /v2/invites/preview`, `POST /v1/sign-in-links/preview`), which are unrelated. | `grep` over `internal/`, `pkg/`, `web/src`, `cmd/`, `docs/` |

### 1.2 Community-model work: merged versus planned

The issue says "do not assume those slices are already deployed". At this commit they are, in the parts this design depends on:

| Piece | State |
| --- | --- |
| `role_permissions`, `permittedTx`, `GET/PUT /v1/permissions`, `whoami.permissions` (ADR-014 A1–A3b) | merged: migration 39, `store/permissions.go`, `internal/server/permissions.go` |
| `see_member_directory` and the may-know predicate (#983, #988; ADR-014's replacement of `hide_members`) | merged: `store/directory.go`, `handleListDirectoryMembers`. The `hide_members` settings row is read once by migration 39 and mapped onto the member cell; nothing reads it after |
| The hide-members client half (#996): the `scope` line, cache replacement on `conch.directory_changed.v1` | merged: `Members.tsx`, `App.tsx` |
| Channel visibility, default channels, the channel directory, self-join and leave, channel settings (#981, #982) | merged: migration 40, `channel_join.go`, `store/channels_self.go` |
| Member links, `InviteV2`; `POST /v1/invites` retired (#985, #1134) | merged |
| The admin console with the Members section and the per-person menu (A6–A8) | merged: `AdminConsole.tsx`, `admin/` |
| Guests | removed for now (#1005, ADR-013 amendment); the tables and v1 shapes stay. #1004 is not a prerequisite |
| Role simulation, presence preferences, a preview of any kind | not built, not designed before this note |

So this design is written against merged behaviour. Where #976's remaining children could move something it relies on, §10 says which slice re-checks it.

## 2. The design in one page

**Preview is a read, under the administrator's own credential, that asks the server to answer a few existing read routes as a named member would see them.** The server, not the browser, computes the member's effective permissions and visibility with the functions it already uses for that member's own requests. Nothing is written, no credential or session is made for the member, and the administrator is shown nothing they could not already see as themselves.

- **Who.** A human caller holding both `manage_members` and `manage_channels` (every operator by default; every owner always). A non-owner cannot preview an owner. The target is a member of the community (enabled, not hidden, human, not a guest). Not oneself. (§3)
- **How.** One request header, `Conch-Preview-As: <principal id>`, on a short, explicit allowlist of GET routes. Inside `guard`, after authentication and before the class check, the server verifies the caller's authority and the target, **swaps the context's caller for the target**, and keeps the real caller as the audit actor. Every existing read handler then answers as the target, through `can()`, `knownPrincipalsSQL`, `channelMembersGate` and the rest, untouched. (§4)
- **Read-only at the boundary.** Any route not on the allowlist refuses the header with 403 `preview_read_only` **before** the swap, so no handler, socket, ticket or write ever runs with the target in the context. Deny by default: a route added later is not previewable until named. (§4.3)
- **What it shows.** The target's channel list, channel directory, member directory and channel member lists (real), and a summary of the target's effective permissions (real, as a new `PreviewResponseV1` from `GET /v1/preview`). Messages, files, approvals, read state, presence, voice occupancy, the authors route, `whoami`'s account fields: placeholders in the client, refused on the server. (§5)
- **No side effects.** The target gets no credential, session, heartbeat, read mark, LiveKit token or audit actor row. The administrator's own session carries on, as themselves. (§4.4)
- **Scope and teardown.** Preview state lives in browser memory only, per tab, attached to the admin's `Session`; it is dropped on Exit, reload, sign-out, profile switch, and when the server refuses the next preview read. The admin's own sockets stay open for the admin's own view; their content frames are never rendered in preview; the content-free signals trigger a preview re-read. (§7)
- **Role simulation** is not in this version; §8 says how a "new member" simulation would fit as a separately labelled thing, and why it is not a member preview.
- **Schema.** One new response type, one new error code, one new audit action, one new request header. All additive; no version bump. (§6)

## 3. Who may preview whom

### 3.1 The rule

| Question | Answer | Why |
| --- | --- | --- |
| Who may preview? | A human principal, not a guest, holding **`manage_members` and `manage_channels`** by `can()`, both re-checked on every preview request. | `manage_members` is "administer people"; a preview is a read about a person's standing, and everything a preview shows about people (role, brakes, memberships, may-know set) is already readable by its holder through the directory's operator fields, `GET /v1/principals` and `GET /v1/permissions`. `manage_channels` is required so that the target's channel list is inside the caller's own sight: its holder may already learn every channel's existence and membership (`?scope=all`, `channelMembersGate`'s `seesAll`). Requiring both keeps the invariant of §3.2 without any filtering: the target's view of channels and people is a subset of what the caller may see. |
| Who may be previewed? | A **member**: `kind = human`, enabled, not hidden, no active or ended guest visit (community-model §1.1's `isMemberTx` predicate). An operator or another owner may be previewed by an owner. | Agents see by manifest, not by role, and their view is not what the feature is for. A disabled or hidden person has no view. |
| May a non-owner preview an owner? | **No.** 403 `forbidden`, "only an owner may act on an owner", by `ownerProtected`, audited `access_denied`. | ADR-010 D2 and ADR-014: nobody but an owner acts on an owner's account. A preview is a read, and an owner's view is "everything", so nothing would be learned; the refusal keeps the rule uniform and keeps the owner's channel list out of a delegated administrator's reach by this route. |
| May one preview oneself? | No: 400 `invalid_request`, "cannot preview yourself". | It would be the normal view with a banner; refusing it keeps the client simple and the audit honest. |
| An unknown, hidden, disabled, agent or guest id? | The uniform 404 `principal_not_found`, byte-identical to the one the manager-add routes answer (`ErrNotAddable`), for an id beyond the sequence too. | community-model §13.4: one answer for every principal the caller may not address. (A holder of `manage_members` could learn most of this from `GET /v1/principals` anyway; the uniform answer costs nothing.) |
| A member without the permissions, or a restricted operator? | A member: 403 `forbidden` ("operator role required", the `accessOperator` text, since the member cell of both permissions is fixed off). An operator lacking either: 403 `permission_denied` naming the first missing permission in catalog order. Audited `access_denied`. | ADR-014 §7.4's refusal rule, one code per question. |
| An agent or guest caller? | Refused first: 403 as on every human-only route. | ADR-010 D6. |
| Recent authentication? | Not required. | Preview grants nothing and reveals nothing the caller cannot already read; the re-authentication class is for routes that grant rights or reveal more than the role (`reauthRule`). |

### 3.2 The invariant: preview never widens

For every previewable route R and every (caller C, target T) the server accepts, **the answer to R under preview is a subset of what C could assemble from its own requests without preview.** Concretely:

- the target's channels (`GET /v1/channels` as T) ⊆ the channels C sees with `?scope=all` (`manage_channels`);
- the target's channel member lists ⊆ what `channelMembersGate` already shows C with `seesAll`;
- the target's directory (`ListDirectory` as T, scope `shared_channels` or `everyone`) ⊆ C's directory, which is everyone (the operator cell of `see_member_directory` is fixed on, and `manage_members` fills the operator fields);
- the target's effective permissions = `EffectivePermissions(T)`, which C can compute from `GET /v1/permissions` (any operator) and the directory's `role`, `owner`, `voice_allowed` and `may_invite` (`manage_members`).

And **no authority moves**: nothing writes under preview (§4.3), so T's permissions are displayed, never exercised. A delegated administrator previewing a member holds exactly its own permissions before, during and after; previewing cannot be a path to owner authority, to `act_as_members`, or to any permission the caller lacks, because preview executes no permission at all.

### 3.3 Alternatives considered

| Option | Why not |
| --- | --- |
| Owner-only | Nick's two servers are his, so it would do; but ADR-014 made administration delegable by permission, and a delegated administrator who may already see every channel and every person gains nothing new here. Owner-only would also need a new owner-only route class use for a read, which ADR-010 reserves for granting rights. Recorded as O1's alternative. |
| A new catalog permission, `preview_members` | The catalog is closed by ADR-014 D2; adding a row is an ADR amendment (the text is in §11), a vocabulary addition in `pkg/schema`, a cell in the matrix, a console row, and a default that must reproduce today's behaviour (on for operators). It would let an owner switch preview off for operators without taking `manage_members` away. Nothing in the use case asks for that. Recorded as O1's alternative; the ADR text is ready if Nick wants it. |
| `manage_members` alone, with the target's channels filtered to the caller's own sight (`channelSightFor`) and a count of the rest | Works, and would let a restricted operator preview; but it adds a filter to every previewable route, a count field to shapes that have none (`ListChannelsResponse` is a bare list), and placeholders for channels the caller cannot name. The two-permission rule gives the same safety with no new code path. Recorded as O2; it is the natural extension if restricted operators ever need preview. |
| Reuse `act_as_members` | That permission is "sign in as, and post as"; preview is strictly weaker and must stay available where an owner has turned `act_as_members` off. |

## 4. The server mechanism

### 4.1 The request

A preview request is an ordinary authenticated request with one extra header:

```
GET /v1/channels
Authorization: Bearer <the administrator's own token>
Conch-Preview-As: 42
```

- The value is the target's principal id, decimal. Anything else (empty, non-numeric, zero, negative, with spaces) is 400 `invalid_request`.
- A header, not a query parameter: query strings land in access logs, bookmarks and the browser history; the bearer is a header already; and a header cannot be forged cross-site, since Conch has no cookie authentication.
- Not a path prefix (`/v1/preview/42/channels`): that doubles the route table and would need its own isolation rows for every mirrored route, and ADR-001 parity is easier to hold on one route family.
- Not a minted token: a "preview credential" for the target is exactly what the issue forbids (it creates a credential for the target, and it is `act_as_members` by another name).

### 4.2 Where it is checked

In `guard`, after `authMiddleware` has resolved the caller and before the class check, a new step runs whenever the header is present:

1. **Route allowlist.** `route` gains `previewable bool` (builder `previewable()` beside `needs`). If the route is not previewable, 403 `preview_read_only`, "this route cannot be read in preview", audited `access_denied` with detail `code=preview_read_only preview_as=<id>`. This runs first, so a refused route never resolves the target and learns nothing about it.
2. **Caller.** Human, not a guest (`membersOnly`'s test), then `can(manage_members)` and `can(manage_channels)`. Refusals as §3.1.
3. **Target.** `PrincipalByID`; the member predicate; `ownerProtected`; not self. Refusals as §3.1, in that order (owner protection before the member test, so an owner id answers the honest 403 rather than a 404).
4. **Swap.** `ctx = withPreview(withCaller(ctx, target), previewContext{Actor: caller, Target: target})`. `credentialIDFrom` and `requestCredentialFrom` stay the administrator's. The handler runs unchanged.

`auditActor(ctx)` changes in one line: when `previewFrom(ctx)` is set, it returns `principal:<actor id>`. Every audit row written under preview (only `access_denied` rows and `preview_viewed` can be) names the administrator, never the target. `denyForbidden` adds `preview_as=<target id>` to the detail when previewing.

With `--auth off` there is no caller; the header is 400 `invalid_request` ("preview needs authentication"), so a development server cannot preview, as it cannot do anything per-principal.

### 4.3 Read-only at the boundary

The allowlist, and nothing else, is what makes preview read-only. It is deliberately not "every GET": a GET can have side effects (`NoteGuestUse` on the caller's credential is one; the events socket upgrade is another), and a method rule would silently admit a future read that leaks content.

| Route | In preview answers |
| --- | --- |
| `GET /v1/preview` (new, §4.5) | the target's effective permissions and standing, `PreviewResponseV1`; audited `preview_viewed` |
| `GET /v1/channels` | the target's channels, `ListChannelsResponse`, with `manages`, `visibility`, `default`, `archived_at`, as the target gets them |
| `GET /v1/directory/channels` | public channels and the target's private ones, with `joined` and `default`, as the target gets them. `?scope=all` is answered by `can(target, manage_channels)`, as for the target itself |
| `GET /v1/directory/members` | the target's directory page: `knownPrincipalsSQL(target, can(target, see_member_directory))`, scope `everyone` or `shared_channels`, operator fields only if the target holds `manage_members` |
| `GET /v1/channels/{channel}/members` | through `channelMembersGate` as the target: 404 if the target is not a member and lacks `manage_channels`; `memberDocs` with the target's privilege (role and every `can_decide` only if the target manages the channel or holds `manage_channels`) |

Every other route refuses the header with 403 `preview_read_only`, including, by name, because each is a plausible mistake:

- every write (`POST`, `PUT`, `DELETE`, `PATCH`): messages, reactions, membership, roles, settings, invites, read state, sessions, heartbeats, voice sessions, transmit, share, camera, companion, hooks, credentials, reports, approvals, runs, interventions;
- every socket (`GET …/ws`) and `POST /v1/ws-tickets`: no socket ever runs with a swapped principal (§7.3 says what live preview would need);
- `GET /v1/whoami` (its `auth`, `guest`, `identity_class` and `invites_remaining` are the target's private account state; the preview summary replaces it);
- `GET /v1/read-state`, `GET /v1/sessions`, `GET /v1/channels/{channel}/voice`, `GET /v1/voice/occupancy` (the target's read marks, who is online, who is in voice: presence and private state);
- `GET /v1/channels/{channel}/authors`, every message, search, thread, file, reaction and approval read (content);
- `GET /v1/permissions`, `GET /v1/principals`, `GET /v1/audit`, every invite, hook, credential and report read (the target's administrative reach is displayed by `permissions`, not exercised);
- `/mcp`: `accessExempt`, so `guard`'s step never runs; `authenticateMCP` admits agents only and never reads the header, which is therefore inert there. A test pins that an agent token with the header gets the same answer as without.

`TestRouteTable` pins `previewable` per route like the class. A new structural test, `TestPreviewableRoutesAreReads`, fails if a previewable route's method is not `GET`, if its pattern ends in `/ws`, or if it is not in the five-entry list in the test itself, so adding one is a deliberate, reviewed act. The isolation check gets a preview phase (§10, slice P3) that sends the header to every route in the table and expects `preview_read_only` from every one not in the list.

**Why swap the caller rather than add an `effectiveFrom(ctx)` and teach each handler to use it.** Five handlers would be edited today, but they reach the caller through helpers (`channelMembersGate`, `canOrFail`, `handleListDirectoryMembers`'s viewer) that dozens of other handlers share, and a helper edited for preview is a helper every handler now depends on for the right answer. Swapping the caller makes the existing code compute the target's view with zero edits, which is what "reuse the existing authorization and visibility functions" asks for, and the allowlist guarantees the swap is only ever seen by those five reads. The cost is the audit-actor line of §4.2, and a rule for reviewers: **a previewable route's handler must never write**, which `TestPreviewableRoutesAreReads` and the isolation phase hold.

### 4.4 No side effects

| Effect | Under preview |
| --- | --- |
| Credential touched (`NoteGuestUse`, `renewCredential`) | The administrator's own, in `authMiddleware`, before the swap: the administrator is the one active. The target's credentials are never read. |
| A session, a heartbeat, "online" | Impossible: `POST /v1/sessions`, heartbeats and `&session=` sockets are not previewable. The target's sessions table is untouched; the roster never lists the target because of a preview. |
| Read state | `PUT …/read-state` is not previewable; nothing in a previewable handler calls `MarkRead`. |
| Media | `POST …/voice/session` and every voice, share and camera route are not previewable; no LiveKit token is minted; the browser never calls `captureMicrophone` or `room.connect` in preview (§7). |
| Audit | `preview_viewed` names the administrator as actor and the target as subject; `access_denied` names the administrator. No row ever names the target as an actor. |
| Rate limits | Per-caller limits apply to the administrator, as for any read. |
| Notifications and signals | A read sends none. Nothing under preview calls `notify*`. |

### 4.5 `GET /v1/preview`

The entry read. It needs the header (without it, 400 `invalid_request`, "preview target required"), runs the checks of §4.2, and answers `PreviewResponseV1` (§6.1): the target's id, kind, name, role, owner mark, effective permissions and revision, and the two brakes as the effective `voice_allowed` and `may_invite` whoami reports. It is the one preview request that is audited, `preview_viewed`, actor the administrator, subject `principal:<target id>`, detail empty. The browser calls it once on entry and again on each refresh; the CLI calls it first in `conch admin preview`.

The other four reads are not audited individually, as reads are not audited today. The limit is recorded: a client that sends the header to `GET /v1/channels` without ever calling `GET /v1/preview` leaves no row. Closing it would mean auditing reads per request, which Conch does nowhere, or a server-side preview session, which §7.3 rejects. The owner's protection against a snooping operator is the same as today's: the operator already holds `manage_channels` and `manage_members`, and every write it makes is audited.

## 5. Content: what is real and what is a placeholder

**Rule.** A preview shows a thing as the target sees it only if the administrator could see that thing as themselves. Under §3.1's two permissions, that holds for standing and visibility (channels, people, permissions) and never for content (messages, files, approvals, read state, presence, voice). Content is therefore a placeholder, whatever the target could see, and the server refuses it rather than trusting the client.

| What the preview shows | Source | Real or placeholder |
| --- | --- | --- |
| Channel navigation: the target's channels, which are default, which the target manages, archived state | `GET /v1/channels` as target | real |
| The channel directory as the target would browse it: public channels and the target's private ones, `joined` | `GET /v1/directory/channels` as target | real |
| The member panel of a channel: who is in it, `manages`, and `role` and `can_decide` only if the target is privileged there | `GET …/members` as target | real |
| The community member list as the target sees it, with its scope line ("You see the people you share a channel with" when `shared_channels`) | `GET /v1/directory/members` as target | real |
| Mention candidates in the (disabled) composer | the preview channel member list, through the same `mention.ts` | real |
| Which controls the target holds, with a one-line reason: create channel, make a member link, join voice, share screen, camera, add people (manager), rename, and the per-person actions a target operator would hold | `PreviewResponseV1.permissions`, channel `manages`, member docs, fed to the **same** `canDo`, `memberActions` and channel-control helpers the client uses for itself | real; computed on the server, rendered by the existing client rules |
| Messages, threads, search, files, reactions, approvals, runs | none: not previewable | placeholder: "Messages are not shown in preview. Open the channel as yourself to read it." |
| Unread counts and read marks | none | placeholder: no badges in preview |
| Presence (online, offline, in voice) | none | placeholder: the member panel shows no online/offline grouping in preview; one group, "Members" |
| Voice and share controls | `permissions` | a permission preview only: "alice could join voice here" / "Voice is off for members"; the button never connects |
| The target's account: password, authenticator, sign-ins, links, recovery state, `invites_remaining`, their own invites and joiners | none: `whoami`, invite and credential routes are not previewable | never shown |
| Whispers, audiences, net membership | none | never shown; nets are content (`GET …/nets` is not previewable) |

**Why messages are placeholders rather than "the intersection of what both may read".** An intersection reader (visible to the target AND to the administrator) is implementable: `store.Reader` would gain a second principal, and `messageVisible` would AND the two. But it is a change to the one visibility function the structural test guards, it reveals to the administrator which of *their own* messages the target can see (whisper audiences by inference), and the use case does not need it: the question is what alice can reach, not what was said. Recorded as O4, recommended not to build; if Nick wants it, it is a separate slice behind `TestMessageReadsStayInTheVisibilityFunction`'s allowlist and its own security review.

### 5.1 The alice/bob example

The world: the owner O (an owner, so every permission). Private channel A with members {O, alice}; private channel B with members {O, bob}. No default channel (community-model §13.7's recommended configuration for a personal server). alice and bob are members, neither manages anything, both with voice on.

**Member hiding on** (the member cell of `see_member_directory` off: the personal preset). O opens Members, chooses alice, "Preview as member". The browser calls `GET /v1/preview` with `Conch-Preview-As: <alice>`; the server audits `preview_viewed`. Then:

| Read, as alice | Answer | What O sees |
| --- | --- | --- |
| `GET /v1/preview` | role `member`, owner false, permissions: the member column under the personal preset, with `see_member_directory`, `create_*_channels` and `make_member_links` absent; `use_voice` and `share_screen` present; `voice_allowed` true, `may_invite` true | banner "Previewing alice (read-only). Exit preview"; controls: "Create channel: not available: Members can't create channels on this server"; "Member links: not available"; "Voice: available" |
| `GET /v1/channels` | [A] | navigation shows A only; B is absent |
| `GET /v1/directory/channels` | [] of public channels, A as joined private | the directory shows nothing joinable |
| `GET /v1/directory/members` | {O, alice}, `scope: shared_channels` | the member list shows O and alice with the line "You see the people you share a channel with"; bob is absent |
| `GET /v1/channels/A/members` | O, alice; `manages` false on both; no `role` (alice is not privileged) | the panel lists O and alice, no roles, no online state |
| `GET /v1/channels/B/members` | the browser never asks (B is not in alice's list); if it did, 404 `channel_not_found`, the same bytes as a ghost | nothing |
| the conversation pane | not previewable | "Messages are not shown in preview" |

O presses Exit preview: the preview store is dropped, the normal view re-renders from O's own stores, which kept updating; O was never offline and alice was never online.

**Member hiding off** (the group preset). The same steps; the only differences:

| Read, as alice | Answer |
| --- | --- |
| `GET /v1/preview` | the member column of the group preset: `see_member_directory`, channel creation and member links present |
| `GET /v1/directory/members` | {O, alice, bob}, `scope: everyone`: bob is listed as a person on the server |
| `GET /v1/channels` | still [A]: hiding never changed channel visibility, and B is private |
| `GET /v1/channels/B/members` | still 404 |

So with hiding off alice knows bob exists and still cannot see B; with hiding on she knows neither. Both are what the acceptance criterion asks, and both are the server's answers, not the browser's.

**Bob**, previewed the same way, is the mirror: [B], {O, bob} or {O, alice, bob}, never A.

## 6. Schema impact

All additive. **No version bump is needed** under this design; if Nick chooses an alternative that changes a published shape (none of §9's does), it needs his sign-off under the `schema-change` skill.

### 6.1 New: `PreviewResponseV1`

```go
// pkg/schema/preview.go

// PreviewResponseV1 answers GET /v1/preview (issue #1007,
// docs/design/permission-preview.md §4.5): the standing and effective
// permissions of the member an administrator is previewing, computed by
// the server. The fields named as in WhoAmIResponseV1 mean the same there;
// whoami's account fields (auth, guest, identity_class, invites_remaining,
// server_version, server_mode) are absent on purpose and never added.
type PreviewResponseV1 struct {
	// ID, Kind, Name and Role are the target's.
	ID   int64         `json:"id"`
	Kind PrincipalKind `json:"kind"`
	Name string        `json:"name"`
	Role PrincipalRole `json:"role"`
	// Owner is the target's owner mark.
	Owner bool `json:"owner"`
	// Permissions is what the target holds now, in catalog order, with
	// the target's brakes applied (EffectivePermissions). Never nil.
	Permissions         []Permission `json:"permissions"`
	PermissionsRevision int64        `json:"permissions_revision"`
	// MayInvite and VoiceAllowed are the effective answers whoami would
	// give the target.
	MayInvite    bool `json:"may_invite"`
	VoiceAllowed bool `json:"voice_allowed"`
	// Simulation is set when the answer is nobody's view (§8, #1519): a
	// role simulation. Absent for a person's preview.
	Simulation PreviewSimulation `json:"simulation,omitempty"`
}
```

- `Simulation` and its closed vocabulary `PreviewSimulation` (`role`) are in the shape from P1 (#1514), since Nick's acceptance put the role simulation (§8, #1519) in scope: a field that is known from the first version costs nothing, where one added later would be a surprise to a client built before it. The vocabulary is strict on the TypeScript side (`TSVocabularyModes`): a client switches on it to pick its banner, and a simulation it does not know it cannot label honestly, so it refuses the document rather than show it as a person's view.
- Fixtures: `pkg/schema/testdata/preview-response-v1.json` (a member under the personal preset), `preview-response-v1-operator.json` (a target operator), `preview-response-v1-simulation.json` (a simulated new member under the group preset), asserted with `assertGoldenFixture`.
- `ts.go`: a `TSTypes` row (`PreviewResponseV1`, family `preview-response-v1`), then `go generate ./pkg/schema` so `web/src/schema/generated.ts` carries it and `schema.test.ts` validates the fixtures.
- Unknown-field tolerance: the TypeScript validators and Go decoders tolerate extra fields as every v1 shape does; a later field (for instance a `simulation` marker, §8) is additive.
- There is no events document and no request body.

### 6.2 New error code

`ErrorCodePreviewReadOnly = "preview_read_only"` in `pkg/schema/preview.go`, beside `PreviewResponseV1`, 403, message "this route cannot be read in preview". (This note first placed it in `pkg/schema/permission.go`; the principal moved it on #1514, 2026-11-07: it is a preview error, not part of the permission catalog that file holds, and `permission.go` is on the approval-path list since #1011.) Error codes are not a closed vocabulary (`Vocabularies` does not list them), so this is an addition with no fixture change; a fixture `error-preview-read-only.json` is added for the TypeScript side. The other refusals reuse existing codes: `forbidden`, `permission_denied` (with `permission`), `principal_not_found`, `invalid_request`.

### 6.3 New request header

`Conch-Preview-As`, documented beside the `Authorization: Bearer` examples in `docs/deploy.md` (there is no separate REST reference at this commit). Not a schema type; recorded here so that a client author finds it. The CLI's `--preview-as` flag sets it.

### 6.4 New audit action

`AuditPreviewViewed = "preview_viewed"` in the store, actor `principal:<admin>`, subject `principal:<target>`, detail empty. `conch.audit_event.v1` is unchanged (`action` is a string).

### 6.5 Unchanged, on purpose

`WhoAmIResponseV1`, `ServerMembersResponseV1`, `ChannelV0`, `ChannelListingV1`, `ChannelMemberV1`, `ListChannelsResponse`, `schema.Error`, every events document, every vocabulary, the MCP tool set (administration stays a human act; parity holds through REST, as ADR-013 and ADR-014 record).

## 7. Scope and teardown

### 7.1 In the server

Preview has **no server state**. Each request carries the header or does not; there is nothing to start, time out or end. That is why reload, sign-out, revocation and a server restart all end preview trivially: the next request either carries the header and is re-checked from scratch, or does not. A permission change takes effect on the next preview read, exactly as it does for the administrator's own reads (admin-roles §7.5: rights are read per request).

### 7.2 In the browser

- **State.** `Session` gains `preview: { target: PreviewResponseV1, channels, directory, members: Map<channel, …>, loadedAt } | null`, in memory, in the React tree, never in `localStorage`, `sessionStorage` or IndexedDB. One tab at a time: another tab of the same origin is in the normal view.
- **Requests.** `api.ts` gets one call family, `previewAs(targetId)`, returning `{ summary, channels, channelDirectory, directoryMembers, listMembers(channel) }`, each attaching the header to its own request. The header is never a module-level or ambient flag: a write can never pick it up by accident, and the server refuses it if one did.
- **Entry.** The Members section's per-person menu (`memberActions`, `PersonActions.tsx`) gains "Preview as member" for targets the caller may preview (human, not oneself, not an owner unless the caller is one), shown when `canDo(who, 'manage_members') && canDo(who, 'manage_channels')`. The server decides regardless.
- **Rendering.** While `preview` is set, the shell renders the preview store only: the channel list from `preview.channels`, the member panel and directory from `preview.directory` and `preview.members`, the conversation pane as the placeholder, the composer disabled with mention completion from the preview member list, and the controls through the same `canDo`/`memberActions`/channel helpers fed `preview.target` instead of `auth.principal`. A persistent banner names the target ("Previewing alice, read-only") with Exit preview and Refresh. The admin console is not reachable in preview (it would show the administrator's console, which is not what the preview is).
- **The administrator's own live state keeps running underneath**: the events socket, session heartbeat and read state of the channel they had open continue **as the administrator**, so they stay online as themselves and their own stores are current the moment preview ends. In preview, none of those stores is rendered. The content-free signals on that socket (`conch.channels_changed.v1`, `conch.permissions_changed.v1`, `conch.directory_changed.v1`) also trigger a preview re-read, since they often mean the target's view moved too; `conch.channel_members.v1` for the open channel does the same. Message, reaction, edit, roster and read-state frames are never applied to the preview store.
- **Caches.** The preview store starts empty on entry and is **replaced**, never merged, on refresh (the rule of community-model §13.6). Nothing from `channels`, `chat`, the people cache or mention candidates of the normal view is read while previewing; nothing from the preview store survives Exit.
- **Teardown.** `preview` is set to `null`, and the preview store discarded, on: Exit; a server refusal of any preview read (403 `preview_read_only`, `forbidden`, `permission_denied`, 404 `principal_not_found`: the banner says "Preview ended: alice can no longer be previewed" and the normal view returns); sign-out (`settleRefusal` and the sign-out path reset `Session`); a 401 on any request; a reload (memory only); a desktop profile switch (`ProfileBar` rebuilds the `Session`); the tab closing. Revocation of the administrator's credential ends preview the way it ends everything.
- **Phone layout.** The banner is a top band under the existing header; Exit is a 44 px target; the placeholder pane and the member panel follow `phone.ts` as they do today.

### 7.3 Live preview: not in this version

A preview events socket would need a ticket bound to (administrator credential, target) and an `eventsSub` flag that filters by the target for membership (`stillMember`, `membersFrame`) while suppressing every content frame (messages, reactions, edits, read states, rosters) and never keeping a session alive. It is buildable, and §10 lists it as an optional later slice (P7). It is not in this version because the use case is a check, not a watch, and because every suppressed frame type is one more place for content to leak through a future change. The Refresh control and the signal-triggered re-read cover the gap.

## 8. Role-preset simulation, distinguished

A **member preview** is the view of a named person: their memberships, their manager grants, their brakes, their links' joiners, under the permissions document as it stands. A **role simulation** would be the view of nobody in particular: "what does a brand-new member see?", computed for a synthetic principal with the role's cells, no brakes, no memberships beyond the default channels and no joiners. The two must not be confused, and the issue says so.

This version ships the member preview only. If Nick wants the simulation (O5), it fits as follows, and nothing in this design has to change for it:

- a **second header**, `Conch-Preview-Role: member` (or `operator`), never combined with `Conch-Preview-As` (both present is 400). A separate header rather than a value grammar (`role:member`) keeps each one trivially validated;
- the server builds an in-memory `store.Principal{ID: 0, Kind: human, Role: member}` and answers the same five reads with it: `GET /v1/channels` lists the default channels (what a link joiner lands in), the directory is the may-know set of someone sharing those channels, `GET /v1/preview` carries `permissions` of the role's cells and a new optional field `simulation: "role"` so a client cannot mistake it for a person;
- the banner says "Simulating a new member. This is not any person's view: it ignores individual channels, manager grants, brakes and links" in those words;
- `preview_viewed` gets subject `role:member`.

A simulation shows less than a preview, never more, so §3.2 holds for it. Its one cost is the synthetic principal through `knownPrincipalsSQL` and `ListChannelsForPrincipal`, which both take an id and would need a "default channels" branch for id 0.

## 9. Open decisions for Nick

Each with a recommendation. The slices of §10 assume the recommendations; a different answer changes the slice it names.

1. **Who may preview.** (a) `manage_members` and `manage_channels`, as §3.1; (b) owner-only; (c) a new catalog permission `preview_members` (ADR-014 amendment, §11.2). **Recommend (a):** it reveals nothing to its holder that the two permissions do not already reveal, adds no ADR change and no matrix row, and keeps preview available to a delegated administrator who runs a group server. Changes slice P2 if not (a).
2. **Restricted operators.** Should an operator holding `manage_members` but not `manage_channels` preview, with the target's channels filtered to the operator's own sight and a count of the rest? **Recommend no, for now:** the filter touches every previewable read and needs a count field; the alpha's two servers have one administrator. Keep it as the recorded extension.
3. **Previewing an owner.** Refuse for non-owners (§3.1), allow for owners. **Recommend as written.** The alternative, refusing for everyone, would stop an owner checking another owner's view, which is harmless and occasionally useful.
4. **Message content.** Placeholders (§5), versus an intersection reader. **Recommend placeholders:** the check is about reach, not words; the intersection changes the one guarded visibility function and infers whisper audiences.
5. **Role simulation.** Ship a "new member" simulation now (§8), later, or not at all. **Recommend later,** as its own small issue once the member preview has been used on alpha.gankbears.com: the primary case does not need it, and it is additive.
6. **Live preview.** A filtered preview events socket (§7.3) now or later. **Recommend later (P7, optional):** Refresh and the signal-driven re-read cover a check.
7. **Audit granularity.** One `preview_viewed` row per `GET /v1/preview` (§4.5), versus a row per preview request. **Recommend one per entry read:** reads are not audited today, and a row per request would swamp the trail during one look.
8. **CLI parity surface.** `conch admin preview <member>` printing the target's permissions, channels and directory, plus `--preview-as` on `conch channels list` and `conch members list` (P5). **Recommend yes, small:** rule 4 is API-first, and the CLI is where a scripted check (the dogfood canary, the isolation harness) is easiest.

## 10. Implementation slices

Each is written to be filed cold under the task template. Order: P1 → P2 → (P3, P4, P5 in parallel) → P6; P7, P8 and P9 are optional and wait on §9. Labels follow the house rules; every server slice is `security`. None is `approval-path`: nothing here reads or writes an approval, `can_decide`, a membership or `permittedTx`'s inputs; the reviewer should confirm that on P2's diff, and if P2 ends up touching `permittedTx` or a standing function, it becomes approval-path and Nick merges it.

### P1 — schema: `PreviewResponseV1`, `preview_read_only` (area/schema)

- **Context.** [permission-preview.md §6](permission-preview.md) (this note), issue #1007, ADR-014 §7 (the wire is additive). No behaviour.
- **Files.** `pkg/schema/preview.go` (new: the shape, `PreviewSimulation` and the error code), `pkg/schema/preview_test.go`, `pkg/schema/testdata/preview-response-v1.json`, `preview-response-v1-operator.json`, `preview-response-v1-simulation.json`, `error-preview-read-only.json`, `pkg/schema/ts.go` (the `TSTypes` row and the vocabulary mode), `pkg/schema/vocabularies.go`, `web/src/schema/generated.ts` and `generated.families.ts` by `go generate ./pkg/schema`, `web/src/schema/preview.ts` (re-export, as the siblings do). `pkg/schema/permission.go` is untouched.
- **Acceptance criteria.**
  - [ ] `PreviewResponseV1` as §6.1, with a doc comment that says which whoami fields are absent and why.
  - [ ] `ErrorCodePreviewReadOnly` beside the permission codes.
  - [ ] `./scripts/schema-compat.sh` green with every existing fixture byte-identical; `scripts/schema-ts-check.sh` green.
  - [ ] No MCP tool definition changes (none references the shape).
- **Required tests.** `assertGoldenFixture` for both fixtures; `TestTSTypesFixtures` passes; `web/src/schema/schema.test.ts` validates the new fixtures.
- **Blocked by.** Nick's decisions on §9 (1 and 4 shape the response; the rest do not).

### P2 — server: the preview header, allowlist, swap and `GET /v1/preview` (area/server, security)

- **Context.** §3, §4 and §5 of this note. The caller swap in `guard`, the audit actor, the five previewable routes, the new route, the refusals.
- **Files.** `internal/server/auth.go` (`route.previewable`, the `previewable()` builder, the step in `guard`, `withPreview`/`previewFrom`, `auditActor`, `denyForbidden` detail), `internal/server/preview.go` (new: target resolution, `handlePreview`), `internal/server/routes.go` (mark the five routes; add `GET /v1/preview`), `internal/server/store/audit*.go` or the file holding the `Audit*` constants (`AuditPreviewViewed`), `internal/server/auth_test.go` (`TestRouteTable` expectations), `internal/server/preview_test.go` (new), `internal/server/preview_guard_test.go` (new: `TestPreviewableRoutesAreReads`), `internal/server/routes.go`'s `recentAuthRoutes` unchanged (and `TestRecentAuthClass` confirms the new route is `reauthNone`).
- **Acceptance criteria.**
  - [ ] A request with `Conch-Preview-As` on a non-previewable route is 403 `preview_read_only` before any target lookup, audited `access_denied` with `preview_as=<id>` in the detail, for every route in the table, sockets and `POST /v1/ws-tickets` included; `/mcp` with the header answers as without it.
  - [ ] The authority and target rules of §3.1, in §4.2's order, with the stated codes and bytes (the 404 byte-identical to `ErrNotAddable`'s).
  - [ ] The five previewable routes answer as the target would: pinned against the target's own answers in a table-driven test (same bytes for `GET /v1/channels`, `GET /v1/directory/channels`, `GET /v1/directory/members` and `GET …/members`, with the target's own token versus the administrator's token plus header).
  - [ ] `GET /v1/preview` answers `PreviewResponseV1` with `permissions` equal to `EffectivePermissions(target)` and writes one `preview_viewed` row, actor the administrator, subject `principal:<target>`.
  - [ ] No row in `sessions`, `credentials`, read state, voice or audit names the target as an actor after a full preview run (a test diffs those tables before and after).
  - [ ] `auditActor` under preview is the administrator; a forced `access_denied` under preview names the administrator.
  - [ ] `TestActorAuthorityReadsPermissions`, `TestEveryPermissionIsChecked`, `TestMessageReadsStayInTheVisibilityFunction`, `TestRecentAuthClass` and `TestRouteTable` pass without new allowlist entries, except the `TestRouteTable` rows for `previewable` and the new route.
  - [ ] `--auth off` with the header is 400.
- **Required tests.** The server matrix of #1007: callers owner, operator with both permissions, operator lacking one (seeded `role_permissions` row), member, agent, guest (seeded through the store); targets member, operator, owner, self, agent, disabled, hidden, ended guest, unknown, out-of-sequence id; under both presets. Direct mutation attempts with the header on every write route (generated from `routeTable`). The side-effect diff. The audit actor. A `security-reviewer` pass before merge.
- **Blocked by.** P1.

### P3 — e2e/isolation: the preview phase (area/server, security)

- **Context.** §4.3 and §5.1. Every route has a row in every phase (`TestEveryRouteHasARow`); the preview phase sends the header to each.
- **Files.** `e2e/isolation/preview.go` (new), `e2e/isolation/main.go` (the phase), `e2e/isolation/routes.go` and `routes_test.go` (the coverage rule knows the phase), `e2e/isolation/world.go` if the world needs a second private channel.
- **Acceptance criteria.**
  - [ ] Every route in the table has a preview row: the five previewable ones expect the target's answer; every other expects 403 `preview_read_only`, byte-identical across routes.
  - [ ] The alice/bob rows of §5.1 run under both presets (the existing personal-preset phase is reused for "hiding on"), with bob's id and name as markers in alice's preview register and alice's in bob's.
  - [ ] The restricted-operator prober (admin-roles §7.7) has rows: 403 `permission_denied` naming `manage_channels` on `GET /v1/preview`, and the same on each previewable route.
  - [ ] A non-owner operator previewing the owner: 403 `forbidden`, the owner-protection text.
- **Required tests.** `go run ./e2e/isolation` green in CI; `TestEveryRouteHasARow` extended.
- **Blocked by.** P2.

### P4 — web: Preview as member (area/web)

- **Context.** §5, §5.1 and §7.2. The entry in the Members section, the preview store, the banner, the placeholders, the controls through the existing helpers, teardown.
- **Files.** `web/src/api.ts` (`previewAs`), `web/src/platform/operations.ts` (the `preview` operation and the header on the five calls), `web/src/App.tsx` (`Session.preview`, the render switch, teardown on sign-out, 401 and profile switch), `web/src/memberActions.ts` and `web/src/admin/PersonActions.tsx` (the menu item), `web/src/Members.tsx` (preview mode of the panel: one group, no presence, the scope line), `web/src/ChannelList.tsx` (preview mode), `web/src/Conversation.tsx` and `web/src/Composer.tsx` (placeholder and disabled composer with preview mention candidates), a new `web/src/PreviewBanner.tsx`, unit tests beside each, `web/e2e/49-preview.spec.ts` (new; the last spec at this commit is `48-channel-archive.spec.ts`).
- **Acceptance criteria.**
  - [ ] "Preview as member" appears in the per-person menu only for previewable targets and only when the caller holds both permissions by `canDo`; the server's refusal is shown by `code` and `permission` otherwise.
  - [ ] In preview, the channel list, directory, member panel, mention candidates and every control come from the preview store and `preview.target`, through the same `canDo`, `memberActions` and channel helpers as the normal view. No new permission table exists in `web/src` (a unit test greps for a parallel `permission` switch in the preview components, or the reviewer confirms).
  - [ ] Messages, unread badges, presence and voice never render in preview; the voice control is a labelled permission preview; no `captureMicrophone`, `room.connect`, `createSession`, `markRead` or socket open carries the header or runs for the target (unit tests stub `platform.http` and assert the header appears only on the five operations).
  - [ ] The banner names the target and offers Exit and Refresh; Refresh replaces the preview store.
  - [ ] Teardown on Exit, server refusal, 401, sign-out, reload (nothing persisted: a test asserts `localStorage` and `sessionStorage` are untouched by preview) and desktop profile switch.
  - [ ] The administrator stays online as themselves throughout (the e2e test checks the roster from a second browser).
  - [ ] Phone layout: banner band, 44 px targets, no horizontal scroll (`12-phone.spec.ts` gains a case).
- **Required tests.** Unit tests with mocked `platform.http` for the store, the header rule, teardown and the control explanations; the Playwright scenario of #1007: the owner previews alice, sees A and not B, sees bob in the directory only under the group preset, checks the controls, exits and is back in the owner's view, under both presets; refresh, revocation (the owner's `manage_channels` removed mid-preview ends it) and reload.
- **Blocked by.** P1 (unit tests on mocked fetch can start); P2 for Playwright.

### P5 — cli: `conch admin preview` and `--preview-as` (area/cli)

- **Context.** §9 decision 8; ADR-001 parity. The CLI reaches the same five reads.
- **Files.** `internal/cli/admin_preview.go` (new), `internal/cli/admin.go` (the group), the HTTP client's header plumbing, `internal/cli/admin_preview_test.go` with hostile-server cases, the TUI untouched.
- **Acceptance criteria.**
  - [ ] `conch admin preview <member>` prints the target's role, owner mark, permissions (catalog order), channels (with manages/visibility/default) and directory scope; `conch channels list --preview-as <member>` and `conch members list <channel> --preview-as <member>` print the previewed answers; each refusal prints the server's `code`, `message` and `permission`.
  - [ ] No CLI write command accepts `--preview-as`.
- **Required tests.** Table-driven against a fake server; a hostile-server test for an oversized or malformed `PreviewResponseV1`.
- **Blocked by.** P2.

### P6 — docs: Preview as member (area/docs)

- **Context.** The operator-facing explanation and limits of §5 and §7: what preview shows, what it never shows, that it is read-only, that device permissions, microphone quality and real end-to-end actions still need a test account.
- **Files.** `docs/deploy.md` (administration section), `README.md` (one line), `CHANGELOG.md` (the release entry: new header, new route, new error code, new audit action).
- **Acceptance criteria.** Each limit of §5's table is stated; the `preview_viewed` audit row is documented for owners.
- **Required tests.** `./scripts/invisible-chars.sh`; `make check` docs-only path.
- **Blocked by.** P4.

### Optional, pending §9

- **P7 — server and web: a filtered preview events socket** (§7.3; decision 6). Blocked by P2 and P4; `security`.
- **P8 — server, web, cli: the new-member simulation** (§8; decision 5). Blocked by P2 and P4; `security`. Adds `simulation` to `PreviewResponseV1` (additive) and the second header.
- **P9 — server: preview for operators without `manage_channels`** (decision 2). Adds the sight filter and counts; `security`.

## 11. Proposed text for the ADRs

ADR changes are Nick's. Nothing is edited into an ADR by this note.

### 11.1 Under the recommendation (no new permission): no ADR amendment is required

Preview adds no role, standing, permission or wire version; it is a read under two existing permissions, enforced in the server, with the browser hiding and explaining and enforcing nothing (ADR-010 D8, ADR-014 D6). It adds no MCP tool (ADR-013 and ADR-014 consequences). It creates no credential, so ADR-014's `act_as_members` boundary is untouched. If Nick wants it recorded anyway, one line in ADR-014's Consequences would do:

> - **Preview as member** ([#1007](https://github.com/njdaniel/conch/issues/1007), [docs/design/permission-preview.md](../design/permission-preview.md)) is a read under `manage_members` and `manage_channels`, answered by the server as the member would be answered, with no write, no credential, no session and no content. It adds no permission, no standing and no version; a non-owner cannot preview an owner.

### 11.2 If Nick chooses a new catalog permission (§9 decision 1, option c): an ADR-014 amendment

> **Amendment (2026-11-__, #1007).** Decision 2's catalog (twelve since ADR-015 added `use_camera`) gains a thirteenth, operator-only permission, `preview_members`: "see the server as another member sees it, read-only". Member cell fixed off; operator default on; owners always. It depends on `manage_members` and `manage_channels` (both are needed to make the preview's answers a subset of what the holder may already see). Its default reproduces the behaviour from before it existed, per Decision 2's rule for a later permission. Decision 6's route class covers `GET /v1/preview` (`accessPermission` + `preview_members`); the header-carrying reads are checked in `guard` against the same permission. Decision 7: the vocabulary addition is client-tolerant, no bump. Design: [docs/design/permission-preview.md](../design/permission-preview.md).

With this option, §3.1's "who may preview" reads `can(preview_members)`, the console's matrix gains a row, `conch admin permissions show` prints it, and P1 adds the vocabulary value with `Fixed`, `Default` and `Requires` entries and a fixture.

## 12. What a reviewer should push on

- **The allowlist is the whole safety argument.** Confirm `TestPreviewableRoutesAreReads` names exactly five routes plus `GET /v1/preview`, that the refusal runs before target resolution, and that no previewable handler reaches a store write (read the five handlers, not the test).
- **The swap and the audit actor.** Every `auditActor`, `denyForbidden` and slog line under preview must name the administrator; grep the five handlers' helpers for `callerFrom` used as an actor.
- **Owner protection order.** An owner target by a non-owner must be the honest 403, before the member test, so it cannot be probed into a 404.
- **The browser never sends the header ambiently.** The header must be set per call in `previewAs`, not in `authed()`; a stubbed-transport unit test should prove no write carries it.
- **Nothing persisted.** No `localStorage`, `sessionStorage` or IndexedDB key for preview; reload ends it.
- **Parity.** The CLI reaches the same reads; no MCP tool.
- **Approval path.** P2 must not touch `permittedTx`, the standing functions or anything that writes a membership or `can_decide`; if it does, relabel and route the merge to Nick.
