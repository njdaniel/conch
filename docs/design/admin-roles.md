# Design: fixed roles with configurable permissions, and the admin console

- **Status:** Proposed 2026-10-30, for Nick's sign-off with [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md). Revised the same day after the security review of PR #1008 (two blocks, five should-fix items and the nits, all adopted; §16 lists what changed). No code, schema file, migration or issue exists for this yet. Nothing here is implemented behaviour.
- **Facts on main** are as of `53aa569`. The community model ([ADR-013](../adr/ADR-013-one-community-model.md), [community-model.md](community-model.md)) is accepted, but none of its issues (#978–#998) has merged, so none of its shapes is published. Guests are being removed for now (#1005, and an amendment to ADR-013 that another designer is writing); this note leaves them out of the role set and says in §3.4 how a guest role would fit later.
- **Builds on:** [ADR-010](../adr/ADR-010-delegated-administration.md) and [delegated-administration.md](delegated-administration.md) (owner mark, channel manager, precedence, "grants never exceed the granter", owner protection, the last-owner guard, the route classes); ADR-013 (member channel creation, member links and the `may_invite` brake, per-person voice, open sign-up, default channels, `hide_members`); [ADR-012](../adr/ADR-012-optional-operator-totp.md) (operator TOTP is optional and set on the host).
- **Owner:** protocol-designer (this note, the shapes in §8); server-engineer, web-engineer and cli-engineer for the issues in §12.

## 0. Why

Nick, 2026-10-30: "I want to have settings for the admin to be able to control that, members roles, server settings", and "for admin lets have fixed roles with configurable permissions."

He has two servers in mind:

- **alpha.gankbears.com**, his personal server. He holds separate private conversations with different people, and the people should not see each other (hide members on).
- **chat.gankbears.com**, later, a group server. People see each other, make channels and bring friends.

ADR-013 fixes one answer for both: every member creates channels, makes member links and sees every other member, and one optional setting (`hide_members`) narrows the last of these. That fits the group server. On the personal server, any member could create channels, bring in strangers through a member link, or (once the directory lands, #983) see everyone. ADR-010 holds that "openings and policy are owner", but it gives the owner no switch for any of these.

This note gives the owner those switches. They are a small, closed list of named permissions, each with a default per role, set from one Settings area, and two presets make the two servers one click each. The roles themselves stay fixed.

## 1. The model in one page

**The roles are fixed.** There are three at server level: **owner**, **operator** and **member**. There is one per channel: **channel manager**. Nobody can add, rename or delete a role. Agents are a kind of principal, not a role. Their rights stay in their manifests (§3.3).

**Permissions are configurable.** There are eleven named permissions (§4). Each has a cell for members and a cell for operators, and each cell is on or off. Owners hold every permission, always. A few cells are fixed: members can never hold an operator-only permission, and operators always see the member directory. The defaults reproduce what the server does today and what ADR-013 decided.

**Who configures what:**

- The owner sets both columns.
- An operator who holds `change_server_settings` (off by default) sets the member column only.
- A member column can never exceed the operator column. This is ADR-010's precedence (owner ⊇ operator ⊇ member), checked on every write.

**Per-person switches only take away** (§5). Two exist already, and no new one is added:

- `voice_allowed` covers `use_voice` and `share_screen`.
- `may_invite` covers `make_member_links`.

A person holds a permission when their role holds it and no brake on them withholds it.

**Presets set the member column and nothing else** (§6). "Personal server" turns off the directory, channel creation and member links. "Group server" is the defaults. A preset is applied once and is not a mode the server stays in. The settings answer says which preset the current column matches, if any.

**Everything is enforced in the server** through one helper, `can(principal, permission)`, and one new route class that names a permission (§7). A refusal is 403 `permission_denied`, and it names the permission.

**The admin console** is one Settings area with four sections: Server, Roles and permissions, Members, and Channels (§10). Each control maps onto a REST route, and `conch admin` has a command for every one of them.

## 2. What exists, verified at `53aa569`

| Fact | Where |
| --- | --- |
| `principals.role` is `operator` or `member`; `principals.owner` marks owners; only an owner changes either (`PUT /v1/principals/{id}/role`, `accessOwner`, `reauthAlways`). | `routes.go`, `pkg/schema/role.go` (#788, #790, PR #829) |
| `channel_members.manages` is the manager grant. The manager class lets in an operator or a member of the path's channel holding `manages`, and answers a non-member 404. | `auth.go` `guard`, `accessChannelManager` (#789, #830) |
| Route classes: 35 `accessOperator`, 3 `accessOwner` (role, audit export, `PUT /v1/join/settings`), 6 `accessChannelManager`, 3 `accessOperatorOrSelf`. | `routes.go` |
| Two per-person flags on `principals`: `voice_allowed` (voice and share) and `may_invite`. One per-membership flag: `can_decide`. | ADR-010 context; `whoami.go` |
| `whoami` carries `role`, `owner`, `may_invite` and `voice_allowed`. | `pkg/schema/whoami.go` |
| Server-wide settings are `server_settings` rows; an absent row reads as its default. | `store/guest_policy.go`, `store/invites.go` |
| Every route has a row in the isolation matrix, or a recorded exemption; `TestEveryRouteHasARow` fails otherwise. | `e2e/isolation/routes_test.go` |
| `TestRouteTable` pins each route's class. `TestRecentAuthClass` pins each operator route's re-authentication decision. | `internal/server` tests |
| The browser's admin surface is `GuestSettings.tsx` in Settings' `admin` slot, shown to operators. Member actions sit inline in `Members.tsx`; #972 proposes a context menu. | `web/src/App.tsx` |
| `conch admin` has groups `channel`, `principal`, `credential`, `member`, `manifest`, `invite`, `sign-in-link`, `join`, `guest` and `reports`. | `internal/cli/admin.go` |
| The server name is a host setting (`--server-name`), not an API one. | `internal/server/server.go`, `cmd/conchd` |
| Nothing in ADR-013 is built: no `POST /v1/channels`, no directory, no voice-permission route, no `hide_members`, no v2 invites. | issues #978–#998, all open |

The last row matters. The member permissions mostly gate routes that do not exist yet, so this design can be the rule those routes are built under instead of a retrofit (§11).

## 3. The roles

### 3.1 The fixed set

| Role | Stored as | Holds | Who makes it |
| --- | --- | --- | --- |
| **Owner** | `role = 'operator'`, `owner = 1` | Every permission, always; and the owner-only rights of §4.3 | An owner (ADR-010 D4) |
| **Operator** | `role = 'operator'`, `owner = 0` | The operator column (§4) | An owner |
| **Member** | `role = 'member'` | The member column (§4) | A member link, open sign-up, or an operator with `manage_members` |
| **Channel manager** (per channel) | `channel_members.manages = 1` | The fixed manager rights of ADR-010 §3.2, as ADR-013 amends them, in that channel only | An operator with `manage_channels`; or creating the channel (ADR-013 O9) |

An operator who lacks `manage_channels` is not above a manager. In a channel where it holds `manages` it has manager standing, and in any other channel it has member standing (§7.2.1). ADR-010 D7's "operator ⊇ manager" therefore holds only for operators holding `manage_channels`.

Nothing is stored for a role beyond what ADR-010 already stores. The wire keeps `role` as `operator|member` plus the `owner` bit. ADR-010 rejected a third value in `principals.role`, and that reason still holds: every client switches on that enum. The permissions document names its columns with a new vocabulary, `PermissionRole` (`member`, `operator`), and owners need no column because they hold every permission.

*Naming.* The brief calls the operator "admin". This note keeps "operator", the word used by every ADR, error message, CLI verb and page (O1).

### 3.2 Why fixed roles

There are no custom roles, by choice:

- Every role is a standing that ADR-010 already wrote the protection rules for: owner protection, the last-owner guard, and who may touch whom. A custom role would reopen each of those rules for each role someone makes up.
- Both of Nick's servers can be expressed with three roles and eleven switches (§6).
- The cost is that "this one member may also create channels" has no answer except making them an operator, with whatever the operator column allows. §5 records this as a consequence.

### 3.3 Agents

Agents are a principal kind (`kind = 'agent'`) and hold **no role and no permission**:

- `can()` answers false for every permission when the principal is an agent, and the store re-checks this.
- An agent's rights stay what ADR-000 D10 says: the capabilities its manifest declares, and the channels it has been added to. They are enforced at the MCP endpoint as today.
- The two systems do not meet. A permission never grants a capability, and a capability never grants a permission. Their names do not overlap: permissions are snake_case verbs (`create_public_channels`), and capabilities are dotted nouns (`messages.read`).
- Who may manage agents is a permission. Writing a manifest is `manage_members`. Issuing an agent's credential or binding a hook to it is `act_as_members`, because the holder of that credential or hook speaks as the agent (§4.1).
- Agents appear in the member directory under the directory's rules (ADR-013 decision 3, §13 of the community design). Nothing here changes that.
- No MCP tool is added (§8.6).

### 3.4 How a guest role would fit later

Guests are out of the role set for now (#1005). If #1004 brings them back, the change would be:

- `PermissionRole` gains `guest`. The vocabulary is closed and client-tolerant (§8.1), so this is an addition, not a version bump.
- `PermissionRowV1` gains an optional `guest` cell (additive).
- The precedence check becomes member ⊇ guest, beside operator ⊇ member.
- Most guest cells would be **fixed off**, since they encode ADR-013 decision 6: `create_public_channels`, `create_private_channels`, `make_member_links`, `see_member_directory` and every operator-only permission.
- Only `use_voice` and `share_screen` would be configurable for guests. The guest link's own voice choice would act as a brake on top, as `voice_allowed` does for members.
- The isolation matrix would gain a guest column of rows.

No existing shape changes for any of this.

## 4. The permission catalog

### 4.1 The list

The catalog is closed: eleven permissions, in this order on the wire and in every view.

| # | Permission | What it allows | Member default | Operator default | Owner | Configurable by |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | `create_public_channels` | Create a public channel (`POST /v1/channels`, `visibility: public`), and become its manager | on | on | always | member cell: owner, or an operator with `change_server_settings`; operator cell: owner |
| 2 | `create_private_channels` | Create a private channel, and become its manager | on | on | always | as 1 |
| 3 | `make_member_links` | Make member links (`POST /v2/invites`, `kind: member`). Also personal invites on v1 while that route answers | on | on | always | as 1 |
| 4 | `see_member_directory` | Read the whole member directory. Without it, a member knows only themself, people they share a channel with, and people their own links brought in (community-model §13.2) | on | **fixed on** | always | member cell as 1; operator cell fixed |
| 5 | `use_voice` | Join voice in channels they are in | on | on | always | as 1 |
| 6 | `share_screen` | Share a screen in voice (needs `use_voice`) | on | on | always | as 1 |
| 7 | `manage_members` | Administer people and agents without speaking as them: create, rename, disable, enable, remove (hide), list and revoke credentials, close sign-in links, list and revoke hooks, manifests, invite administration (everyone's links, joiners, open links, granting `may_invite` through a link), the brakes, the voice switch, the operator fields of the directory, `GET /v1/principals`, a session's runtime and host | **fixed off** | on | always | operator cell: owner |
| 8 | `act_as_members` | **Sign in as, and post as, any person or agent who isn't an owner:** issue or rotate a credential for someone else, make a sign-in, reset or enrolment link for someone else, bind a hook to someone. Needs `manage_members`. | **fixed off** | on | always | operator cell: owner |
| 9 | `manage_channels` | Administer every channel: create without a daily cap, visibility and default, add and remove members, `manages` and `can_decide`, add agents, rename, nets, stop a share, moderation (#698), `?scope=all`, read any channel's member list and every net. Also **name landing channels the holder is not in**: a link's channels, O5's field, `open_channels`. | **fixed off** | on | always | operator cell: owner |
| 10 | `change_server_settings` | Change server-wide policy: joining and open sign-up, the default link expiry, and the member column of this document. Naming channels in `open_channels` also needs `manage_channels`. | **fixed off** | **off** | always | operator cell: owner |
| 11 | `read_audit_log` | Export the audit trail (`GET /v1/audit`). **The trail names every member and every private channel**, including channels the holder is not in, so granting this reveals them. Recent authentication for anyone but an owner. | **fixed off** | **off** | always | operator cell: owner |

`act_as_members` is split from `manage_members` because it is a different kind of power. On main an operator issues a full credential to any non-owner (`credentials.go`), makes sign-in, reset and enrolment links for any member, and binds hooks that post as any member (`hooks.go`). Each of those lets the operator become that person. `manage_members` keeps the roster, the brakes and removal, which act on people without speaking for them. The console and `conch admin permissions show` carry the plain description: "can sign in as, and post as, any member who isn't an owner". The default is on for operators, so today's behaviour holds.

With these defaults a server behaves as it does today and as ADR-013 decided:

- Operators hold every right they hold today (rows 7–9 on).
- Policy and audit stay owner-only (ADR-010 D2).
- Members create channels, make links, see the directory and use voice (ADR-013 decisions 2–5).

Rows 1–3 and 4 gate routes that do not exist yet (§2). Until those routes exist, the cells have no effect.

**The rule for adding a permission later:** its default must reproduce what the server did before it existed, so that an upgrade changes nothing. The new permission is added to the vocabulary (closed, client-tolerant), and no migration is needed, because an absent row reads as the default (§9).

### 4.2 Fixed rights: not permissions, by choice

Each of the following stays as it is, with the reason:

| Right | Who | Why it is not configurable |
| --- | --- | --- |
| Read, post and react in channels one is in | every member | Membership is the content boundary (ADR-003, ADR-013 decision 1). A server where members cannot post is not a chat server; take people out of the channel instead. Reactions follow posting (message-reactions.md). |
| Join and leave public channels | every member | A public channel is public by definition. A server that wants nothing joinable has no public channels. |
| A channel's creator becomes its manager | creator | ADR-013 O9. A private channel whose creator cannot add anyone is a dead room. The way to stop it is to switch creation off (rows 1–2), not to create managerless channels. |
| Channel manager rights | managers | ADR-010 §3.2 lists them and their protections (no approvers, no operators, no agents). Making them configurable reopens those protections (O8). |
| Remove a message, remove from voice, ban from a channel (#698, when it lands) | managers in their channel; operators with `manage_channels` | Moderation is per channel, and the moderation design assigns it there. |
| Rename oneself, change one's password, sign out everywhere, see one's own sign-ins and links, send a problem report | every person | These act on one's own account. |
| Read and file problem reports | every operator | Reports are members' channel to whoever runs the server. Every operator can see them, whatever else they hold. |
| Read the joining, invite and permissions settings | every operator | These are reads, and an operator needs to see why something is refused. |
| Brakes: pause invites, the per-person brakes | operators with `manage_members` | "Brakes are operator; openings are owner" (ADR-010 §3.1). A brake stays with the operator on duty. |
| Protection of a target: a manager cannot remove an operator, an owner, an agent or an approver; bulk hide and disable never touch operators; sign-in links never target an operator; nobody acts on an owner | everyone | These rules say who is *protected*, not who may act, so they stay keyed on the target's role and owner mark. Only checks on the *actor's* authority move onto permissions (§7.1). |
| Sign-in strength: an operator signs in with a second factor where required, re-authenticates, cannot use sign-in or device links, and is classed `operator` in whoami | every operator | An account is as strong as the most power it could hold, whatever the document grants today. These are listed exceptions to the structural test (§7.1): `login.go`, `password_recovery.go`, `operator_auth.go`, the operator-target rule in `sign_in_links.go`, the identity class in `guests.go`, and the store's `operator_auth.go`, `passwords.go` and `mode.go`. |

### 4.3 Owner-only, fixed

The owner-only rights are ADR-010 §3.1, minus the two that become operator permissions (audit, server policy):

- Change roles and the owner mark (`PUT /v1/principals/{id}/role`).
- Change the operator column of the permissions document.
- Act on an owner: every exclusion in ADR-010's owner-protection rule, unchanged.
- Hold every permission. Owners have no column, so no cell can switch an owner off.
- **Skip brakes.** A brake on an owner is never applied. The routes already refuse to set one on an owner, but a stored `voice_allowed = 0` or `may_invite = 0` on an owner (from a database edit, or set before the person was marked owner) stops counting. The CHANGELOG's upgrade note says so: "an owner whose voice or link-making was switched off has them again".

Host settings stay on the host: `CONCHD_OPERATOR_TOTP` (ADR-012), the mode (ADR-011), the server name, `--invite-allowance`. Nothing in the API changes them.

### 4.4 Invariants, checked on every write

1. **Precedence:** for every permission, member ⇒ operator. The owner is always above both. A write that would grant members something operators lack is refused with 409 `permissions_conflict`, naming the permission.
2. **Dependencies,** in the same column: `share_screen` ⇒ `use_voice`, and `act_as_members` ⇒ `manage_members`. Otherwise 409 `permissions_conflict`.
3. **Fixed cells** cannot be written. Members cannot hold rows 7–11, and the operator's row 4 is always on. A write to a fixed cell is 400 `invalid_request`, because the schema's `Validate` already refuses it.
4. **Grants never exceed the granter** (ADR-010 D4):
   - An operator never writes the operator column.
   - An operator turning a member cell **on** must hold that permission itself, by `can()`: its role's cell **and** its own brakes, not merely the operator column. An operator whose voice is braked cannot turn members' voice on. Turning a cell off is never bounded.
   - The owner holds everything, so nothing an owner writes exceeds the owner.
5. **No lock-out is possible.** Owners hold every permission whatever the document says, and the last-owner guard is unchanged.

The store checks all five inside the PUT's IMMEDIATE transaction. It checks them against the caller as re-read there (role, mark, brakes, `change_server_settings`) and against the rows as they stand under the lock. `Validate` and the handler check first only so that a refusal comes early.

## 5. Per-person switches: brakes only

**Rule.** A person holds a permission when their role's cell is on **and** no per-person brake on them withholds it. Brakes can only take a permission away.

Only the two brakes that already exist are kept:

| Brake | Column | Withholds | Set by |
| --- | --- | --- | --- |
| Voice switch | `principals.voice_allowed` | `use_voice`, `share_screen` | an operator with `manage_members`, never on an owner (#984's route) |
| Link brake | `principals.may_invite` | `make_member_links` | an operator with `manage_members`, never on an owner (`PUT /v1/principals/{id}/invite-permission`) |

Why this rule:

- **It is the smallest rule that keeps what exists.** ADR-013 already turned `may_invite` into a brake, and `voice_allowed` already behaves like one. No new column, route or state is needed.
- **A tri-state override (inherit, allow, deny) would let a brake grant more than the role.** That needs a new column per permission, a rule for who may grant above the role, and an answer to "what does the matrix mean?" when the person-level overrides contradict it. Fixed roles exist so that the matrix is the truth.
- **What it gives up:** "voice for three of thirty members" means leaving the role's voice on and switching it off for 27 people. Neither server Nick described needs that (O3).

**What the voice switch does when the role's voice is off.** It still records the brake but grants nothing. The console says "Voice is off for all members (Roles and permissions)" beside the switch rather than offering a switch that seems to do nothing.

**Stored defaults are unchanged.** Every path that makes a member writes `voice_allowed = 1` and `may_invite = 1`, as ADR-013 says. A brake is therefore always a deliberate act by an operator.

**Owners skip brakes** (§4.3).

## 6. Presets

A preset is a named value of the member column. Applying one writes that column and nothing else:

- It does not change the operator column, joining, open sign-up, default channels or any person's brakes.
- It is not stored as a mode.
- The settings answer reports the preset that the current member column equals (`preset`), or omits it.

| Member cell | **Personal server** | **Group server** (= the defaults) |
| --- | --- | --- |
| `create_public_channels` | off | on |
| `create_private_channels` | off | on |
| `make_member_links` | off | on |
| `see_member_directory` | off | on |
| `use_voice` | on | on |
| `share_screen` | on | on |

**Personal server.** Members see only the owner, other operators they share a channel with, and the people in their own channels. They cannot create channels or bring anyone in. The owner (or an operator) makes the member links, creates one private channel per conversation, and adds each person to theirs.

**Group server.** This is ADR-013 as accepted: a directory, member-made channels, and member links.

**Warnings, not refusals.** These come back with the answer to applying a preset, and with every GET:

- `default_channel_exposes_members`: the member directory is off and at least one default channel exists. Everyone in a default channel sees everyone else in it (community-model §13.7), so on a personal server a default channel defeats the purpose.
- `open_signup_on`: member links are off, but open sign-up is on, so the address still lets anyone in.

Nick may want either of these on purpose, so neither is refused.

**One gap.** A member link lands its joiner in the default channels (ADR-013 decision 5). On a personal server there are none, so a newcomer lands in nothing and waits until the owner adds them. O5 proposes that an **operator's** member link may name the channels it lands in. That is a change to ADR-013's link shape, so it is listed in §13 and not decided here.

The preset table lives in `pkg/schema` and is sent in the GET answer, so the browser and the CLI never hold a copy of it (§8.2).

## 7. Enforcement

### 7.1 One helper

```go
// internal/server: the one authorization question for a permission.
func (s *Server) can(ctx context.Context, c caller, p schema.Permission) (bool, error)

// internal/server/store: the same question, inside a write transaction.
func (s *Store) Permitted(ctx context.Context, principalID int64, p schema.Permission) (bool, error)
func permittedTx(ctx context.Context, tx *sql.Tx, principalID int64, p schema.Permission) (bool, error)
```

`permittedTx` reads the principal (kind, role, owner, disabled, hidden, the two brakes) and the document's cell for the principal's role. It answers in this order:

1. An agent, a guest, or a disabled or hidden principal: **false**.
2. An owner: **true**. Brakes are not applied.
3. A **fixed cell** is decided in code and never read from a row. A member asking for rows 7–11 gets false, and an operator asking for `see_member_directory` gets true. A stored row that contradicts a fixed cell is ignored, and the store never writes one (§9).
4. Otherwise, the cell for the principal's role (an absent row reads as the catalog default), ANDed with the matching brake for rows 3, 5 and 6, and with the dependencies of §4.4.2: `share_screen` needs `use_voice`, and `act_as_members` needs `manage_members`.

The document is read per request from SQLite, so there is no cache to invalidate. That is the same choice as community-model §13.6: one indexed read of at most twenty rows.

Every write that a permission gates calls `permittedTx` again inside its IMMEDIATE transaction (ADR-010 D7). So a request racing a change to the document cannot pass on the old answer.

A structural test, in the style of `TestMessageReadsStayInTheVisibilityFunction`, keeps the helper the only door. It covers the handlers **and the store**, because the store makes authorization decisions of its own (`memberActorStandingTx`, `CreateInvite`, `makerStillAllowed`, `CloseInvite`):

- No handler may call `isOperator(caller)`, and no store function may compare an **actor's** `Role` with `RoleOperator`, to decide what the actor may do, except at listed sites. Each listed site carries its reason. The list holds §4.2's two rows (target protection; sign-in strength) and the fixed operator reads of §4.2 (reports, the settings reads).
- `isOwner(caller)` passed as a `forbidOwner` argument is target protection, and is allowed.
- Every `schema.Permission` value must be named by at least one route or handler check, so there is no dead permission.

### 7.2 Route classes

There is one new class. The others stay:

```go
// accessPermission: a valid human credential whose principal holds the
// route's permission (route.perm) by can(). An owner always does. An agent
// is refused first, as on every human route.
accessPermission
```

`route` gains `perm schema.Permission`, used only with `accessPermission`. The guard behaves as follows:

- A **member** on a route that `accessOperator` guards today keeps today's answer, byte for byte: 403 `forbidden`, "operator role required". The isolation rows pin that answer, and members cannot hold rows 7–11 anyway.
- An **operator without the permission** gets 403 `permission_denied` with `permission` set. The refusal is audited `access_denied` with detail `permission=<name>`.

| Class today | After | Routes |
| --- | --- | --- |
| `accessOperator` | `accessPermission` + `manage_members` (21) | `POST /v0/principals`, the manifest PUT, listing and revoking credentials, disable, enable, the two hide routes, `GET /v1/principals`, listing and revoking hooks, the four guest routes (while they exist), invite joiners, disable-joiners, disable-invitees, invite-permission, `PUT /v1/invites/settings`, closing a sign-in link |
| `accessOperator` | `accessPermission` + `act_as_members` (4) | `POST /v1/principals/{id}/credentials`, `POST /v1/credentials/{credential_id}/rotate`, `POST /v1/hooks`, `POST /v1/principals/{id}/sign-in-links` |
| `accessOperator` | `accessPermission` + `manage_channels` (5) | `POST /v0/channels`, the four net write routes (create, archive, add and remove a member) |
| `accessOperator` | unchanged, fixed operator (5) | `GET /v1/join/settings`, `GET /v1/invites/settings`, the three report routes |
| `accessOwner` | `accessPermission` + `change_server_settings` | `PUT /v1/join/settings` |
| `accessOwner` | `accessPermission` + `read_audit_log` | `GET /v1/audit`, with recent authentication for anyone but an owner (below) |
| `accessOwner` | unchanged | `PUT /v1/principals/{id}/role` |
| `accessChannelManager` | unchanged class; its operator branch reads `can(manage_channels)` instead of `isOperator`, and so do the handlers and store functions behind it (§7.2.1) | the three membership routes, rename, the two reaction-moderation routes |
| `accessOperatorOrSelf` | unchanged class; the handler's operator branch reads `can(manage_members)` | rename, manifest GET, revoke-all |
| new | `accessOperator` | `GET /v1/permissions` |
| new | `accessPermission` + `change_server_settings`; the handler also requires an owner for any operator-column change | `PUT /v1/permissions` |

Today's 35 operator routes split 21 + 4 + 5 + 5. Under the defaults every operator holds rows 7–9 and only owners hold rows 10–11, so every route above answers exactly as it does today.

An operator without `manage_channels`, on the manager class, is treated as a member: a non-member gets 404 `channel_not_found`, and a member without `manages` gets 403. That keeps a channel-less operator from learning about private channels it is not in.

**Re-authentication** follows the route, not the class, with two additions:

- `PUT /v1/permissions` is `reauthAlways`, since it grants rights.
- `GET /v1/audit` gets a new rule value, `reauthNonOwners`. An operator holding `read_audit_log` needs a recent proof; an owner does not, as today. The trail names every member and every private channel.

`TestRecentAuthClass` gains both.

#### 7.2.1 Standing in the store: the manager-class fix

The route guard is not enough on its own. Behind the manager class, the handlers decide standing **by role** (`members.go`: `manager := authed && !isOperator(caller)`), and so does the store (`memberActorStandingTx` in `store/members.go`, and its twin in `store/channels_rename.go`).

Without a fix, an operator R that lacks `manage_channels` could:

1. create a channel, and so manage it;
2. pass the guard as a manager;
3. then get **operator** standing from the handler and the store.

With operator standing, R could set `can_decide` and `manages` and add agents, which changes who approves. So every store standing function becomes:

1. Actor 0 (system or `--auth off`): owner standing, as today.
2. Disabled, hidden, guest or agent: refused, as today.
3. An owner: owner standing.
4. An operator **and** `permittedTx(manage_channels)`: operator standing.
5. Otherwise, operator or member: manager standing if the membership row holds `manages = 1`, else refused.

The handlers take the manager branch on the same test, `!can(manage_channels)`. A restricted operator-manager therefore gets exactly a member-manager's rights: no `can_decide`, no `manages`, no agents, no removal of operators or approvers, and the manager's limits. Each refusal is audited as a manager's is today (`member_add_refused`, `access_denied`).

The same rule applies to every other store function that gives an operator wider standing on a write. Each reads the permission named in §7.3, and the structural test of §7.1 keeps any new one from reading the role:

- `CreateInvite`;
- `makerStillAllowed` and `effectiveChannelsTx`;
- the `may_invite` grant at redemption;
- `CloseInvite`;
- `CreateSignInLink` and `makerIsOperatorTx`;
- `SetMayInvite`'s "an operator invites by role".

These functions write memberships and `can_decide`, so the PR that changes them is **approval-path** (A3a, §12).

### 7.3 Checks inside handlers

Member permissions gate routes anyone authenticated may call, so they are checked in the handler with `can()` and again in the store. The route stays `accessAuthenticated`.

| Route | Check |
| --- | --- |
| `POST /v1/channels` (#981) | `create_public_channels` or `create_private_channels`, by the body's `visibility`. The daily cap applies unless the caller holds `manage_channels`. |
| `PUT /v1/channels/{id}/settings` (#981) | `manage_channels` (ADR-013 O5 said "operators") |
| `GET /v1/directory/channels?scope=all` (#982) | `manage_channels` |
| `GET /v1/directory/members` (#983, #988) | `see_member_directory` decides **everyone** versus the **may-know** predicate. `manage_members` decides whether the operator fields are filled. |
| `PUT /v1/principals/{id}/voice-permission` (#984) | `manage_members` (class `accessPermission`) |
| `POST /v2/invites` (#985), and `POST /v1/invites` while it answers | `make_member_links` |
| `makerStillAllowed`, at redemption | the maker still holds `make_member_links` (§7.5) |
| `POST …/voice/session`, `…/transmit`, `…/companion`; `voiceEntitlement`; `voiceStillEntitled` | `use_voice`. Today these read `voice_allowed` alone. |
| `POST …/voice/share`, and the poller's per-pass share check (`voice_share.go`) | `share_screen`, read by `can()` on every pass, as `use_voice` is |
| `DELETE …/voice/share` by someone other than the sharer | the channel's manager, or `manage_channels` |
| `GET /v1/channels/{channel}/members` for a non-member, and the privileged view of a member list (roles shown) (`members.go`) | `manage_channels`; otherwise a non-member gets the uniform 404 `channel_not_found` |
| `GET /v1/channels/{channel}/nets` seeing every net (`nets.go` `seeAll`) | `manage_channels`; otherwise only the nets the caller is on |
| `GET /v1/invites` listing everyone's invites (`invites.go`); `DELETE /v1/invites/{id}` closing someone else's (`CloseInvite`) | `manage_members`; otherwise one's own, and someone else's is 404 |
| A session's `runtime` and `host` (`sessions.go`) | `manage_members`, or the session's own principal |
| **Making a link** (`CreateInvite`, v1 and v2) | Checked by the store in the creation transaction. An **open** (reusable) v1 link needs `manage_members`, and so does **granting `may_invite`** through a link. **Naming a channel the maker is not in**, in v1 `channels` or O5's field, needs `manage_channels`. Without it, the answer is the one a member gets today (`ErrInviteGrantExceedsInviter`, "a channel you are not in"), so it reveals nothing. A landing membership never carries `can_decide` or `manages`. |
| **Redeeming a link** (`makerStillAllowed`, `effectiveChannelsTx`, the `may_invite` grant) | Re-checked by the store in the redemption transaction, against the maker as they are then. The link needs its own permission (§7.5). Channels the maker is not in are kept only while the maker holds `manage_channels`; otherwise they are dropped (audited in `dropped_channels`, the existing field), and the link is `invite_invalid` when none is left. `may_invite` is granted only while the maker holds `manage_members`. |
| `PUT /v1/join/settings` with `open_channels` (`pkg/schema/guest.go`) | `change_server_settings` for the route, **and** `manage_channels` for the channels, checked in the store transaction. Without it, 403 `permission_denied` naming `manage_channels`. The setting has no maker, so nothing is re-checked at an address join: it is server policy until someone changes it. |
| Sign-in, reset and enrolment links at redemption (`makerIsOperatorTx`, `usableSignInLinkTx`) | The maker still holds `act_as_members` (§7.5). A device link, a member's own, is unchanged. |

### 7.4 The refusal

- 403, `code: "permission_denied"`, a plain message ("Members can't create private channels on this server"), and `permission: "create_private_channels"`. `permission` is a new optional field on `schema.Error` (§8.4).
- Clients choose their words from `permission`, not from the message.
- Existing codes stay wherever they already answer the same question, **one code per question, whichever check refused**:
  - `voice_not_allowed` for voice and share, whether the role cell or the brake said no;
  - `invite_not_allowed` for making a member link, whether the role cell or the brake said no;
  - `forbidden` for a member on an operator route.

  Each also carries `permission`. A client that reads the old codes keeps working.
- `permission_denied` is new, and it is used only where no code already says the thing.

### 7.5 Live effects of a change

Rights are read per request, so most changes take effect on the next request with no revocation. Four changes need more.

| Change | Effect |
| --- | --- |
| `use_voice` off for a role | Every person the change affects leaves voice at once. #984's immediate hook (`voicePrincipalLostAccess` with `voice_not_allowed`) runs for each affected person in voice, and at worst they are out within one poller pass plus one join-token lifetime, as #984 bounds it. Rooms are not rotated. |
| `share_screen` off for a role | Each live share by an affected person stops through the existing stop path, audited as a share stop. |
| `see_member_directory` changed for members | The `conch.directory_changed.v1` signal (community-model §13.6) goes to every member's and operator's events socket, as "the setting toggled" does there. |
| `make_member_links` off for a role | Open member links of affected makers are **suspended, not closed**. Redemption and preview refuse them (`invite_invalid`) while the maker lacks the permission, and they work again when it comes back. Closing them, as the per-person brake does, would make an experiment with a preset permanent (O4). The per-person brake keeps its close-on-off rule (ADR-013). |
| `manage_members` or `manage_channels` off for operators | Their outstanding links are judged again at redemption (§7.3). Without `manage_members`, open links and `may_invite` grants are suspended; without `manage_channels`, channels the maker is not in are dropped. Nothing is closed. |
| `act_as_members` off for operators | Their unredeemed sign-in, reset and enrolment links are **suspended**: while the maker lacks the permission, redemption and preview refuse them with the existing answer for an unusable link. **Limit:** credentials and hooks already issued stay live. They belong to their target, and neither table records who issued them, so there is nothing to suspend them by. The audit trail names the issuer, and the owner ends them with sign-out-everywhere or a hook revoke (O13). |

Raising a permission revokes nothing. ADR-010 D1 revokes a target's credentials when the owner mark is set, so that no token another operator issued becomes owner-grade. Raising an operator cell raises every operator equally, including the operator who issued any given token, so no token ends up above its issuer.

Lowering a permission revokes nothing either. The next request is refused.

### 7.6 Escalation analysis

| Question | Answer |
| --- | --- |
| Can an operator raise itself? | No. The operator column is owner-only. |
| Can an operator with `change_server_settings` grant members more than operators hold, or more than it holds itself? | No. Precedence (§4.4.1) and the writer's effective permissions, brakes included (§4.4.4), are checked in the store transaction. |
| Can an operator with `change_server_settings` open the server? | Yes: open sign-up and member links for members. That is what the owner delegated by granting it, which is why it is off by default. |
| Can a member reach an operator-only permission? | No. Those cells are fixed off; `Validate` refuses them and the store re-checks. |
| Can a brake grant? | No. It is ANDed (§5). |
| Can an agent hold a permission? | No. `permittedTx` answers false first, and the store refuses agent roles as ADR-010 already does. |
| Can a racing request use a permission that was just removed? | No. Every gated write re-checks inside its transaction. A read that was already answered is answered, as with any read. |
| Can an operator without `manage_members` act on people through another route? | No. Each such route is mapped in §7.2 or §7.3, and the structural test of §7.1 catches a new one. |
| Can an operator without `manage_channels` gain operator standing in a channel it manages? | No, once §7.2.1 lands. On main, the store's standing functions decide by role, so the guard change alone would let such an operator set `can_decide` and `manages` and add agents in any channel it creates. Every standing function reads `permittedTx(manage_channels)`, and an isolation row and an approval end-to-end case pin the refusal (§7.7). |
| Can an operator without `manage_channels` land an account in a private channel it is not in? | No, once §7.3's link and joining rows land. On main, `CreateInvite` skips the "a channel you are not in" check for any operator, and `open_channels` can name any channel. Both read `manage_channels`, at creation and, for links, at redemption. |
| Can an operator without `manage_channels` learn about private channels it is not in? | Not through the routes this note maps, which give the uniform 404: the manager class, a channel's member list, the nets list, `?scope=all`. **But `read_audit_log` reveals them**, because the trail names every channel and every member. That is why it is off by default and needs recent authentication. |
| Can an operator without `act_as_members` become someone? | Not through any new credential, link or hook. Those it issued earlier stay live (§7.5, O13). |
| Can `manage_members` alone be used to become someone? | No. It lists and revokes credentials and hooks, closes links and removes people. Everything that hands over a way to speak as someone is `act_as_members`. |

### 7.7 The isolation matrix and the coverage test

The isolation check (`go run ./e2e/isolation`) extends in three ways:

1. **A restricted-operator prober.** The world gains an operator whose column the owner has cut to the member set: no `manage_members`, `act_as_members` or `manage_channels`.
   - Every `accessPermission` route gets a row for it, expecting 403 `permission_denied` with the route's `permission`.
   - `accessChannelManager` routes get a row for it on a channel it is not in, expecting the uniform 404.

   The prober also **creates a private channel of its own, and so manages it**. These rows run against that channel and against A's channels:
   - setting `can_decide` or `manages` on a member of its own channel: 403, audited;
   - adding an agent, or removing an approver, an operator or an owner, in its own channel: refused as a manager is, audited;
   - a link naming A's private channel, in v1 `channels` and in O5's field: the "a channel you are not in" refusal;
   - an open v1 link, or a link granting `may_invite`: `invite_not_allowed`;
   - `PUT /v1/join/settings` with `open_channels` naming A's channel, after the owner grants it `change_server_settings`: 403 `permission_denied`, `manage_channels`;
   - `GET /v1/channels/{A's private}/members`: 404;
   - the nets list of A's channel: 404;
   - `GET /v1/invites`: its own invites only;
   - closing A's invite: 404;
   - a session of A's: no `runtime` or `host`;
   - a sign-in link it made before the owner removed `act_as_members`: refused at redemption;
   - a link it made before losing `manage_channels`, naming a channel it was not in: redeemed without that channel.
2. **A coverage rule beside `TestEveryRouteHasARow`.** `TestEveryPermissionRouteHasARestrictedRow` fails if a route of class `accessPermission`, or a handler check listed in the table of §7.3, has no restricted-operator or restricted-member row. The route table exposes `perm`, so the check reads it the way it reads classes today.
3. **A personal-preset phase.** This replaces the hide-members phase of community-model §13.9, which it absorbs. The owner applies `preset: personal` through `PUT /v1/permissions`. Then:
   - member rows for channel creation and link creation expect 403 `permission_denied`;
   - the directory rows expect the may-know list (`scope: shared_channels`);
   - the §13.9 rows run unchanged.
   The phase ends by applying `group` and checking the directory widens and the `conch.directory_changed.v1` signal arrives.

`TestRouteTable`'s expectations gain the class `classPermission` with the permission name, so a route's permission is pinned like its class.

**The approval end-to-end test** (CLAUDE.md rule 3) gains one case in A3b. The restricted operator-manager tries to set `can_decide` in the channel of an open approval, which would make it an approver, and tries to add an agent there. Both are refused and audited. The approval then resolves with the quorum it had, through notify, resolve and audit.

## 8. Wire shapes (`pkg/schema`)

All of this is **additive: no version bump.** It adds new types, new vocabularies, new routes, optional fields on `WhoAmIResponseV1` and `schema.Error`, and one new events document. No published fixture changes. The `schema-change` skill governs the PR, and the compatibility table is in §8.7. The REST bodies are versioned by their type suffix and carry no schema-name field, as the house rules have it. The events document carries one.

### 8.1 Vocabularies

```go
// permission.go (new file)

// Permission names one configurable right of a server role (ADR-014). The
// vocabulary is closed on the server, which refuses an unknown value, and
// client-tolerant: a client shows a value it does not know by its name and
// never sends one. It grows only by adding a constant whose default
// reproduces the server's behaviour before it existed.
type Permission string

const (
	PermCreatePublicChannels  Permission = "create_public_channels"
	PermCreatePrivateChannels Permission = "create_private_channels"
	PermMakeMemberLinks       Permission = "make_member_links"
	PermSeeMemberDirectory    Permission = "see_member_directory"
	PermUseVoice              Permission = "use_voice"
	PermShareScreen           Permission = "share_screen"
	PermManageMembers         Permission = "manage_members"
	PermActAsMembers          Permission = "act_as_members"
	PermManageChannels        Permission = "manage_channels"
	PermChangeServerSettings  Permission = "change_server_settings"
	PermReadAuditLog          Permission = "read_audit_log"
)

// Permissions is the catalog in wire order.
var Permissions = []Permission{ /* the eleven, in the order above */ }

func (p Permission) Valid() bool
// OperatorOnly reports whether p can never be granted to members (rows 7-11).
// Fixed reports whether role r's cell for p is fixed, and its value; the
// store applies fixed cells in code and never reads them from a row.
func (p Permission) Fixed(r PermissionRole) (fixed, value bool)
func (p Permission) OperatorOnly() bool
// Default reports p's default for role r.
func (p Permission) Default(r PermissionRole) bool

// PermissionRole names a column of the permissions document. Closed and
// client-tolerant. Owners have no column: they hold every permission. A
// later "guest" is an addition (design §3.4).
type PermissionRole string

const (
	PermissionRoleMember   PermissionRole = "member"
	PermissionRoleOperator PermissionRole = "operator"
)

// PermissionPreset names a value of the member column (design §6). Closed
// and client-tolerant.
type PermissionPreset string

const (
	PresetPersonal PermissionPreset = "personal"
	PresetGroup    PermissionPreset = "group"
)

// PresetMemberPermissions is each preset's member column: the permissions
// it turns on. Every other member permission it turns off.
var PresetMemberPermissions = map[PermissionPreset][]Permission{ /* §6 */ }

// PermissionsWarning: a condition the owner should know about, never a
// refusal. Closed and client-tolerant.
type PermissionsWarning string

const (
	PermissionsWarningDefaultChannelExposesMembers PermissionsWarning = "default_channel_exposes_members"
	PermissionsWarningOpenSignupOn                 PermissionsWarning = "open_signup_on"
)
```

All four vocabularies go into `Vocabularies` (`vocabularies.go`), so `go generate ./pkg/schema` emits their TypeScript unions.

### 8.2 The document and its routes

```go
// PermissionRowV1 is one permission's cells. Fixed cells are reported so a
// client draws them locked without knowing the rules.
type PermissionRowV1 struct {
	Permission    Permission `json:"permission"`
	Member        bool       `json:"member"`
	Operator      bool       `json:"operator"`
	MemberFixed   bool       `json:"member_fixed,omitempty"`
	OperatorFixed bool       `json:"operator_fixed,omitempty"`
}

// PermissionPresetV1 is one preset as the server defines it.
type PermissionPresetV1 struct {
	Preset PermissionPreset `json:"preset"`
	Member []Permission     `json:"member"` // never null
}

// PermissionsResponseV1 is the body of GET /v1/permissions (any operator)
// and the answer to PUT. Rows lists every permission once, in catalog
// order. Revision increases by one on every change that changes a cell.
// Editable lists the columns the caller may change: both for an owner,
// ["member"] for an operator holding change_server_settings, [] otherwise.
type PermissionsResponseV1 struct {
	Revision  int64                 `json:"revision"`
	Rows      []PermissionRowV1     `json:"rows"`
	Preset    PermissionPreset      `json:"preset,omitempty"` // the preset the member column equals
	Presets   []PermissionPresetV1  `json:"presets"`
	Editable  []PermissionRole      `json:"editable"`
	Warnings  []PermissionsWarning  `json:"warnings"`
	UpdatedAt Timestamp             `json:"updated_at"`
}

// PermissionChangeV1 sets one cell.
type PermissionChangeV1 struct {
	Permission Permission     `json:"permission"`
	Role       PermissionRole `json:"role"`
	Granted    bool           `json:"granted"`
}

// PutPermissionsRequestV1 is the body of PUT /v1/permissions (owner, or an
// operator with change_server_settings for member cells only; recent
// authentication). It is a patch: Changes sets the cells it names and
// leaves every other cell, so a client that does not know a later
// permission never clears it. Preset sets the member column to that
// preset. Exactly one of Changes and Preset. Revision is the revision the
// caller read; a stale one is 409 permissions_stale, and the client
// re-reads GET /v1/permissions before deciding again.
type PutPermissionsRequestV1 struct {
	Revision int64                `json:"revision"`
	Changes  []PermissionChangeV1 `json:"changes,omitempty"`
	Preset   PermissionPreset     `json:"preset,omitempty"`
}

// Validate: revision positive; exactly one of changes (1 to 32 entries, no
// (permission, role) twice) and preset; every value known; no grant of an
// operator-only permission to members; no change to a fixed cell.
func (r PutPermissionsRequestV1) Validate() error
```

The patch shape is deliberate. If a PUT carried whole columns, a client from before permission eleven existed would turn eleven off every time it saved. The PUT shape is boring and flat, and it fails safe.

**Errors**, all new and additive on `schema.Error.code`:

| Status | Code | When |
| --- | --- | --- |
| 403 | `permission_denied` | a member or operator lacks the permission; with `permission` set (§7.4) |
| 409 | `permissions_stale` | the `revision` is not the current one (the `channel_name_stale` precedent) |
| 409 | `permissions_conflict` | precedence or dependency (§4.4) would break; the message names the permission |
| 403 | `forbidden` "owner required" | an operator sends an operator-column change |

### 8.3 Whoami

`WhoAmIResponseV1` gains two optional fields:

```go
	// Permissions is what the caller holds now: its role's cells, its
	// brakes applied; every permission for an owner; none for an agent.
	// Catalog order. A server that predates ADR-014 omits it and
	// PermissionsRevision; a server with it always sends PermissionsRevision.
	Permissions         []Permission `json:"permissions,omitempty"`
	PermissionsRevision int64        `json:"permissions_revision,omitempty"`
```

`MayInvite` and `VoiceAllowed` keep their documented meanings ("whether the caller may make invites", "whether the caller may use voice"), and from now on they report the effective answer: role AND brake. That is within their meaning, not a semantic change. The browser keys its controls on `permissions` (and #972's menu does too), with the server deciding every request regardless.

The full matrix is not shown to members. A member learns what they may do, not how the server is configured (O10).

### 8.4 `schema.Error`

```go
	// Permission names the permission a 403 permission_denied refused
	// (ADR-014). Omitted on every other error.
	Permission Permission `json:"permission,omitempty"`
```

This is an additive optional field. MCP tool errors never carry it, since agents hold no permissions.

### 8.5 Audit rows

| Action | Actor | Subject | Detail |
| --- | --- | --- | --- |
| `permissions_changed` | the caller | `server` | `revision=<n>;preset=<name>` (when a preset), then each changed cell `member.make_member_links=true->false`. A PUT that changes nothing writes no row and keeps the revision. |
| `permissions_initialized` | `system` | `server` | Written once by the migration (§9): `revision=1;source=defaults`, or `…;hide_members=true->member.see_member_directory=false` if the hide-members row exists |
| `access_denied` (existing) | the caller | the route | gains `permission=<name>` in its detail for a permission refusal |
| `invite_suspended`, `invite_resumed` | `system` | the invite | Written when a role change suspends or resumes a maker's links (§7.5), with `reason=permission`. One row per link. |
| `sign_in_link_suspended`, `sign_in_link_resumed` | `system` | the link | The same, for sign-in, reset and enrolment links when `act_as_members` changes |
| `member_add_refused`, `access_denied` (existing) | the caller | the channel | A restricted operator-manager refused operator standing in a channel it manages (§7.2.1), with `permission=manage_channels` |

### 8.6 Live refresh

```go
// PermissionsChangedSchemaV1 is the events-socket document that says the
// receiver's effective permissions may have changed. It carries nothing
// else: the client re-reads GET /v1/whoami (and, if it shows the matrix,
// GET /v1/permissions). Content-free, as conch.directory_changed.v1 is.
const PermissionsChangedSchemaV1 = "conch.permissions_changed.v1"

type PermissionsChangedV1 struct {
	Schema string `json:"schema"`
}
```

It is sent on `GET /v1/events/ws`, coalesced under the roster floor:

- After a document change, to every human principal's sockets. The document is server-wide, so telling everyone that it changed tells no one anything about anyone else.
- After a brake change, to the target only.

A client without an events socket (the CLI) reads `whoami` the next time it needs to.

**MCP.** No tool is added and none changes. Administration is a human act, and parity holds because the browser, the CLI and the TUI all use the same REST routes (ADR-001). Agents are outside the permission system (§3.3), so `post_message`, `read_channel` and the approval tools gate on the manifest and membership as before.

### 8.7 Compatibility summary (for Nick)

| Change | Kind | Needs Nick |
| --- | --- | --- |
| `Permission`, `PermissionRole`, `PermissionPreset`, `PermissionsWarning` | new closed, client-tolerant vocabularies | no |
| `PermissionRowV1`, `PermissionPresetV1`, `PermissionsResponseV1`, `PermissionChangeV1`, `PutPermissionsRequestV1`; `GET/PUT /v1/permissions` | new types and routes | no |
| `PermissionsChangedV1` (`conch.permissions_changed.v1`) | new events document | no |
| `WhoAmIResponseV1.permissions`, `.permissions_revision` | additive optional | no |
| `schema.Error.permission` | additive optional | no |
| `permission_denied`, `permissions_stale`, `permissions_conflict` | additive codes | no |
| `PUT /v1/join/settings` and `GET /v1/audit` move from `accessOwner` to permission classes | behaviour on a published route. Unchanged under the defaults: only owners pass | **yes, as part of ADR-014** (it amends ADR-010 D2) |
| An operator without `manage_members`, `act_as_members` or `manage_channels` refused on today's operator routes, links and channel reads | behaviour on published routes, only after an owner removes the permission | yes, ADR-014 |
| `GET /v1/audit` needs recent authentication for a non-owner holder | behaviour on a published route, only after an owner grants `read_audit_log` | yes, ADR-014 |
| A brake stored on an owner stops counting (§4.3) | behaviour, in practice never reached through the API | yes, ADR-014 (and the CHANGELOG) |
| Drop the unpublished `DirectorySettingsV1`, `PutDirectorySettingsRequestV1`, `DirectorySettingsResponseV1` and `GET/PUT /v1/directory/settings` from #978/#988 | nothing published is removed | yes, ADR-014 (amends ADR-013 decision 9) |

## 9. Storage and migration

```sql
CREATE TABLE role_permissions (
  role       TEXT    NOT NULL CHECK (role IN ('member', 'operator')),
  permission TEXT    NOT NULL,
  granted    INTEGER NOT NULL CHECK (granted IN (0, 1)),
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (role, permission)
);
-- server_settings row 'permissions_revision' (absent reads 1).
```

- **An absent row reads as the catalog default.** A new server writes nothing, and the defaults apply. A permission added later needs no migration. The `permission` column is not CHECK-constrained, so adding a value needs no table rebuild. The store refuses unknown values on write, and on read it ignores a row it does not know.
- **The store rejects a fixed cell on write**: an operator-only permission granted to members, or the operator's `see_member_directory` off. Defence in depth behind `Validate`. **On read, `permittedTx` decides fixed cells in code** (`Permission.Fixed`), so a row written by hand or by a buggy build cannot grant one.
- **The migration** (the next free number when it lands; independent of community migration 37):
  1. Create the table. It starts empty, so every server keeps today's behaviour on every existing route (§4.1).
  2. **If a `hide_members` row exists in `server_settings`** (only if #988 shipped first), write `member.see_member_directory = NOT hide_members` and leave the old row unread. On main today the row does not exist, so this step does nothing.
  3. Write `permissions_revision = 1` and one `permissions_initialized` audit row.
- **Rollback** is the previous binary plus the pre-upgrade backup, as for every migration. A previous binary ignores the table. Under the defaults it behaves the same; after a preset it would forget the preset, which the CHANGELOG says.
- **Nick's alpha.** After the upgrade, apply `personal` once: `conch admin permissions preset personal`, or the button. This must happen before the release that ships the member directory (#983) reaches alpha, or alpha's members will see each other in that window. §12 orders the work so that cannot happen.

## 10. The admin console

### 10.1 Where it lives

The console replaces the `admin` slot of Settings, today `GuestSettings.tsx`, with a **Server administration** area of four sections:

- It is shown to operators and owners. Members see none of it.
- A member's Settings gains one read-only line under "Who can administer", listing what they may do on this server, drawn from `whoami.permissions`: "You can create channels and make member links. Voice is on."
- Every control keys on `whoami.permissions` (and `owner`). A control the caller cannot use is absent or disabled with the reason, never shown as working. The server refuses regardless, and the console shows its refusal by `code` and `permission`.
- Writes in `reauthAlways` open the existing `Reauth` overlay, as `GuestSettings` does today.

### 10.2 The sections

**Server.** This is the server-wide policy. Writes need `change_server_settings` (owners always).

| Control | Route | Notes |
| --- | --- | --- |
| Server name | — | Read-only, with the line "Set on the server's host (`--server-name`)" (O7) |
| Open sign-up; joining paused | `PUT /v1/join/settings` (ADR-013 Option B fields) | Shows `no_default_channel`, and `open_signup_on` when member links are off |
| Default link expiry | the same route | |
| Invites: every link, its maker, joiners, close; pause | `GET /v2/invites`, `DELETE /v1/invites/{id}`, `PUT /v1/invites/settings` | The brakes need `manage_members` |
| Setup card | — | Shown while `revision == 1`: "How will this server be used? Personal server / Group server / Decide later". It applies the preset through Roles and permissions. |
| Summary lines | — | "Members see: only people they share a channel with" and "Voice: on for members". Each links to its cell in Roles and permissions. |

The brief puts "voice default" and "hide members" in this section. They are cells of the matrix, and one control per thing avoids two switches that can disagree. So this section shows them as summary lines that link to the cell.

**Roles and permissions.** This is the matrix:

- **Presets on top:** two cards, Personal server and Group server, each with the list of what it changes against the current column. The card the column matches is marked. The warnings are shown here.
- **The matrix:** rows are the eleven permissions in catalog order, with a one-line description each. The columns are **Members**, **Operators** and **Owners**. The Owners column is all on and locked: "Owners hold every permission." Fixed cells are locked, with a reason on hover or focus. Columns the caller may not edit are read-only (`editable`).
- **`act_as_members`** is described in its row as "can sign in as, and post as, any member who isn't an owner", and turning it on for operators asks for confirmation with that sentence. **`read_audit_log`** is described as "sees every member and every private channel's name in the audit trail".
- **A read-only block for channel managers:** "In their channels: add and remove members, rename, stop a share, moderate. Set per channel in Channels."
- **Save** sends a `changes` patch with the revision read. On `permissions_stale` it re-reads and shows what changed. On `permissions_conflict` it names the cell.

**Members.** This is the community directory with the operator fields (#983):

- One row per person or agent: name, kind (an agent marked), role and owner badge, voice brake, link brake, invited by.
- **Actions** come from #972's context menu on desktop and from a ⋯ button or long press on the phone. They are the same action table either way, and each is shown only when `can()` allows it and the target is not protected:

| Action | Route | Needs |
| --- | --- | --- |
| Change role (Member / Operator / Owner) | `PUT /v1/principals/{id}/role` | owner |
| Voice on/off | `PUT /v1/principals/{id}/voice-permission` | `manage_members`; shows "off for all members" when the role cell is off |
| Member links on/off | `PUT /v1/principals/{id}/invite-permission` | `manage_members` |
| Sign out everywhere | `POST /v1/principals/{id}/credentials/revoke-all` | `manage_members` |
| Remove from server (confirm) | `POST /v1/principals/{id}/hide` | `manage_members` |
| Disable / enable | `…/disable`, `…/enable` | `manage_members` |
| Sign-in, reset or enrolment link | `POST /v1/principals/{id}/sign-in-links` | `act_as_members`. The menu item says "Make a link that signs in as this person". |
| Issue a credential; bind a hook | `POST /v1/principals/{id}/credentials`, `POST /v1/hooks` | `act_as_members`, said the same way |
| Add to channel… | `PUT /v1/channels/{channel}/members/{id}` | `manage_channels` |

- **Add a person** opens member-link creation (`make_member_links`).

**Channels.** This is every channel (`GET /v1/directory/channels?scope=all`, `manage_channels`):

- One row per channel: name, visibility, default mark, member count, managers.
- Actions:
  - set visibility and default (`PUT /v1/channels/{id}/settings`);
  - rename;
  - add and remove members;
  - toggle `manages` per member;
  - create a channel (public or private), unlimited.
- On a personal server the row's first action is **Add person**. That is how the owner opens a new private conversation.

### 10.3 The phone layout

The console follows the existing phone layout (`phone.ts`, mobile-browser-layout.md). Settings is a modal page:

- The four sections are a list. Each opens as its own page with a back control.
- The matrix becomes one list of permissions. A segmented control at the top picks **Members** or **Operators**, and each row has one switch. The Owners column becomes a single line of text.
- Member and channel actions are a bottom sheet from the ⋯ button or a long press (#972's touch fallback). Destructive actions confirm in the sheet.
- No table scrolls horizontally, and every target is at least 44 px. The phone spec (`12-phone.spec.ts`) gains a case per section.

### 10.4 CLI parity (ADR-001)

Every console control is a REST route first. The CLI and TUI call the same routes:

| Console | CLI |
| --- | --- |
| Roles and permissions: view | `conch admin permissions show` (table: permission, members, operators, fixed; the matched preset; warnings) |
| Change cells | `conch admin permissions set <permission> [--members on\|off] [--operators on\|off]` |
| Apply a preset | `conch admin permissions preset personal\|group [--yes]` (shows the diff and asks unless `--yes`) |
| What I may do | `conch whoami` prints `permissions` |
| Server section | `conch admin join status\|set` (existing, on ADR-013 Option B), `conch admin invite …` (#994) |
| Members section | `conch admin principal list\|role\|voice\|invite-permission\|disable\|enable\|hide\|sign-in-link`, `conch admin credential revoke …`, `revoke-all` (existing, plus #994's `voice`) |
| Channels section | `conch channels list --all`, `conch admin channel create\|settings`, `conch admin member add\|remove\|manage` (existing, plus #994) |

The TUI shows the caller's permissions in its status pane and hides the commands it cannot use. Its admin panes follow #995.

## 11. How this relates to the community issues

The community issues are all open, so the cheapest path is to land the permission core (A1, A2, A3a, A3b; §12) first and have each community slice call `can()` as it is built. Each change below is one line in the issue's acceptance criteria.

| Issue | Change |
| --- | --- |
| #978 I1 schema | Drop `DirectorySettingsV1`, `PutDirectorySettingsRequestV1`, `DirectorySettingsResponseV1`, `DirectoryWarning` and the `/v1/directory/settings` routes (absorbed by A1). Keep `DirectoryScope`, `ServerMembersResponseV1.scope`, `ChannelAuthorV1` and `DirectoryChangedV1`. If O5 is accepted, add the landing-channels field to `CreateInviteRequestV2`. |
| #981 I3a | `POST /v1/channels` checks the create permission matching the body's `visibility`. The daily cap is skipped for `manage_channels`, not for "operators". `PUT …/settings` is `accessPermission` + `manage_channels`. The creator's membership row is written by the store standing rule of §7.2.1, so a restricted operator creating a channel becomes a manager, not an operator there. |
| #982 I3b | `?scope=all` needs `manage_channels`. |
| #983 I4 | The operator fields are filled for `manage_members`. Ships no earlier than A3b (§9, alpha). |
| #984 I5 (voice) | The route is `accessPermission` + `manage_members`. The entitlement is `use_voice` (role) AND `voice_allowed` everywhere it is read (`voiceEntitlement`, `voiceStillEntitled`). It gains the role-level eviction of §7.5 if A4 has not landed, or A4 adds it. |
| #985 I6a (`may_invite`) | `POST /v2/invites` checks `make_member_links`, and refuses with `invite_not_allowed` whether the role cell or the brake said no. `makerStillAllowed` checks the same, so role-level suspension works without closing links (§7.5). The brake keeps its close-on-off rule. `whoami.may_invite` is the effective answer. Every "operator" branch in link creation and redemption reads §7.3's permissions (open links and `may_invite` grants: `manage_members`; channels beyond the maker's own and O5's landing channels: `manage_channels`). |
| #986 I6b (redemption) | Redemption re-checks the maker's permissions in its transaction (§7.3), and a landing membership never carries `can_decide` or `manages`. |
| #987 I7 | `PUT /v1/join/settings` is `accessPermission` + `change_server_settings`. Open sign-up is set by that class (amends O1's "owner"). |
| #988 I14a (`hide_members`) | **Absorbed in part.** No `hide_members` setting and no `/v1/directory/settings` route. The predicate's clause 1 becomes "the viewer holds `see_member_directory`". The predicate, the authors route, the targeted signal and the warnings stay. The isolation phase becomes A5's personal-preset phase. |
| #992 I9c (operator controls) | **Superseded** by A6 (Server: open sign-up, expiry), A8 (Members: voice switch) and A9 (Channels: visibility and default). Close it when those three are filed. |
| #996 I14b | The setting UI is the matrix cell (A7). `conch admin directory` becomes `conch admin permissions`. The member panel's scope line stays. |
| #994 I11a, #995 I11b | Unchanged in scope. A10 adds the permissions commands. |
| #972 (context menu) | Keys its actions on `whoami.permissions` and the target's role, owner and kind (the table in §10.2). It is the Members section's menu (A8). |

## 12. Proposed issues, ordered

Each would be filed to the issue template once ADR-014 is accepted. The labels follow the house rules. Every server slice is `security`. **A3a and A3b are also `approval-path`**, because they change who may set `can_decide` and who writes memberships; each ships the full request → notify → resolve → audit test, and Nick merges them.

1. **A1 — schema: roles and permissions (ADR-014).** Everything in §8, in `pkg/schema`: the four vocabularies (eleven permissions) with `Valid`, `OperatorOnly`, `Fixed` and `Default`; the document, change, request and response types with `Validate`; the preset table; `PermissionsChangedV1`; the whoami fields; `schema.Error.permission`; the three error codes; fixtures in `testdata/`; the `ts.go` and `vocabularies.go` entries; `go generate`; `schema-compat.sh` green with every existing fixture byte-identical. No behaviour. If #978 is still open, it also removes the directory-settings shapes from #978's scope (§11). *Blocked by:* ADR-014.

2. **A2 — store: `role_permissions`, `permittedTx` and the migration.** The table, the `permissions_revision` row, and `Permitted`/`permittedTx` (agent false; owner true, brakes skipped; fixed cells in code; role cell or default; the brakes and the two dependencies ANDed). The precedence, dependency, fixed-cell and writer checks of §4.4, inside the write transaction. The migration of §9, with the hide-members mapping and `permissions_initialized`. Table-driven tests over every role × permission × brake, including an absent row, an unknown row, a hand-written row contradicting a fixed cell, an owner with a stored brake, and an agent. No caller yet. *Blocked by:* A1.

3. **A3a — store and handlers: actor authority reads permissions, not role.** Every store function that grants an operator wider standing on a write reads `permittedTx` (§7.2.1): the two standing functions, `CreateInvite`, `makerStillAllowed`, `effectiveChannelsTx`, the `may_invite` grant, `CloseInvite`, `CreateSignInLink`, `makerIsOperatorTx`, `SetMayInvite`. Every handler branch of §7.3's table reads `can()`: members.go (both write handlers, the member list and its privileged view), nets.go `seeAll`, invites.go, sessions.go, voice_share.go. Add the structural test of §7.1 over handlers and store, with its allowlist. Under the defaults nothing changes, and no route can change the document yet, so this lands safely alone. The approval e2e test runs unchanged, plus a store-level case: an operator without `manage_channels` who manages a channel is refused `can_decide`, `manages` and an agent add, audited. *Labels:* approval-path, security. *Blocked by:* A2.

4. **A3b — server: the permission class, `GET/PUT /v1/permissions`, whoami, audit, live signal.** The scope:
   - `accessPermission` and `route.perm`; the remapping of §7.2 (21 + 4 + 5 + 5);
   - `TestRouteTable` with `classPermission`; `PUT /v1/permissions` in `reauthAlways`, and `reauthNonOwners` for `GET /v1/audit`;
   - the two routes with `revision`, the patch and preset semantics, `editable`, warnings; the writer rule re-checked in the transaction; `permission_denied` refusals and audit;
   - whoami's `permissions`; `conch.permissions_changed.v1`;
   - the restricted-operator isolation rows of §7.7 for every remapped route and every §7.3 branch, the operator-manager rows included;
   - the approval e2e case of §7.7 (CLAUDE.md rule 3).

   This is the issue that makes restriction possible, so it must not merge before A3a. *Labels:* approval-path, security. *Blocked by:* A3a.

5. **A4 — server: member permissions on today's routes, and the live effects.** The scope:
   - `use_voice` and `share_screen` in the voice session, transmit, companion and share handlers, `voiceEntitlement`, `voiceStillEntitled`, and the poller's per-pass share check;
   - `make_member_links` on `POST /v1/invites` while it answers, with `invite_not_allowed` for both the role cell and the brake;
   - the live effects of §7.5: role-level voice eviction, share stop, link suspension, sign-in-link suspension on `act_as_members`, with their audit rows;
   - the `conch.directory_changed.v1` send on a directory-cell change, if #988 has landed.

   Tests include a join token minted before a role-level voice change, a live share when `share_screen` goes off, and a sign-in link redeemed after its maker lost `act_as_members`. *Labels:* security. *Blocked by:* A3b, and #984 for the immediate hook (or it carries the hook if it lands first).

6. **A5 — e2e/isolation: the personal-preset phase and the coverage rule.** `TestEveryPermissionRouteHasARestrictedRow` and the preset phase of §7.7, which takes over community-model §13.9's phase when #988 lands. (A3b carries the restricted-operator rows themselves.) *Blocked by:* A3b. It grows with each community slice.

7. **A6 — web: the admin console and its Server section.** The Server administration area replaces the `admin` slot: the four-section shell, the phone section list, the Server section of §10.2 (on the post-#1005 `GuestSettings` content and ADR-013 Option B), the setup card, the summary lines, and the member's read-only permissions line. Unit tests with mocked fetch; Playwright, including the phone. *Blocked by:* A1 (can start on mocked fetch); A3b for Playwright.

8. **A7 — web: Roles and permissions.** The matrix, the presets with their diff, warnings, the stale and conflict answers, the locked cells, the confirmation for `act_as_members`, the phone segmented layout, and the live re-read on `conch.permissions_changed.v1`. *Blocked by:* A6, A3b.

9. **A8 — web: the Members section, with #972's context menu.** The directory with the operator fields, the action table of §10.2 behind one menu (pointer, long press, keyboard, ⋯ button), the `act_as_members` items worded as acting as the person, confirmations, owner protection, and the role-off explanation on the voice switch. Absorbs #992's voice switch and #972. *Blocked by:* A6, #983, #984.

10. **A9 — web: the Channels section.** The `?scope=all` list, visibility and default, managers per channel, add and remove, create unlimited, and the "Add person" first action on a personal server. Absorbs #992's channel controls. *Blocked by:* A6, #981, #982.

11. **A10 — cli: `conch admin permissions` and permissions in whoami.** `show`, `set` and `preset` as in §10.4, with the plain descriptions of `act_as_members` and `read_audit_log`; `conch whoami` prints permissions; refusals print the permission; hostile-server tests; the TUI status pane line. *Blocked by:* A3b.

12. **A11 — docs: running a personal or a group server.** `docs/deploy.md` gets a section per preset (alpha.gankbears.com and chat.gankbears.com as the examples), the CHANGELOG upgrade note (the migration, the alpha ordering of §9, rollback forgetting a preset, and owners skipping brakes), and README's administration paragraph. *Blocked by:* A6–A10.

**Order and lanes:**

- ADR-014, then A1, A2, A3a, A3b, in that order. A3b must not merge before A3a: the route remap alone opens the hole of §7.2.1.
- After A3b: A4, A5, A6 and A10 can run in parallel, and A7 follows A6.
- A8 and A9 follow their community slices. A11 comes last.
- The community slices #981, #983, #984, #985 and #987 take their one-line changes from §11, and are built after A3a where possible. **A3b must be released no later than #983**, so that a personal server can turn the directory off before it exists.

## 13. What community-model.md and ADR-013 would need to change

These are listed for the designer amending those files now. This note edits neither.

1. **ADR-013 decision 9, community-model §13.1, and the §3.1 rows for §13:**
   - The `hide_members` setting and `GET/PUT /v1/directory/settings` are replaced by the member cell `see_member_directory` (off means members are hidden from each other), set through `PUT /v1/permissions`.
   - Drop `DirectorySettingsV1`, `PutDirectorySettingsRequestV1`, `DirectorySettingsResponseV1` and `DirectoryWarning` from the shapes. The `default_channel_shared` warning becomes `PermissionsWarning` `default_channel_exposes_members`, on the permissions answer.
   - Drop `directory_settings_changed`; the change is `permissions_changed`.
   - The setting's description text (§13.1) moves to the matrix row's description, unchanged in substance.
2. **Community-model §13.2, clause 1** reads "the viewer holds `see_member_directory`". Clause 2 (operators) stands as written: the operator cell is fixed on.
3. **ADR-013 decision 2 and community-model §2.6:** "any member creates one" becomes "any member holding `create_public_channels` or `create_private_channels` (on by default, ADR-014)".
4. **ADR-013 decision 4 and community-model §2.5:** voice is on for every member *by default*, through `use_voice`. The per-person switch is a brake under the role cell: switching a person on grants nothing while the role cell is off. Screen share is its own cell, `share_screen`, and the brake covers it.
5. **ADR-013 decision 5 and community-model §2.1:**
   - Member links are made by members holding `make_member_links`, with the `may_invite` brake on.
   - A role-level change suspends links rather than closing them (O4).
   - If O5 is accepted, a link made by a holder of `manage_channels` may name the channels it lands in, in place of the defaults. That is a field on `CreateInviteRequestV2` before #978 publishes it, checked at creation and again at redemption.
   - Every "operator" in link creation and redemption becomes a permission: open links and `may_invite` grants need `manage_members`; channels beyond the maker's own need `manage_channels`. A landing membership never carries `can_decide` or `manages`.
6. **ADR-013 O1 and the §4 row "Turn on open sign-up…":** the class is `change_server_settings` (owners always, operators when granted, default owner-only). The same goes for hide members.
7. **ADR-013 O5 and community-model §2.6:** channel settings, `?scope=all`, adding agents and setting `manages`/`can_decide` are for operators holding `manage_channels`. An operator without it, in a channel it created, is that channel's manager and nothing more (§7.2.1). The voice switch and the directory's operator fields are for those holding `manage_members`. `open_channels` (Option B) needs `manage_channels` beside the settings permission.
8. **Community-model §4 (the permissions table):** each row that a permission now decides names it, and the header points to ADR-014 for the configurable cells.
9. **Community-model §9 and §13.11:** the issue table takes §11's changes. I9c is superseded by A6/A8/A9. I14a loses the setting and route. I14b's setting UI moves to A7.
10. **Community-model §13.9:** the hide-members isolation phase is run as A5's personal-preset phase.
11. **Community-model §1.1 (terms):** "operator, owner, channel manager: as ADR-010" gains "with the configurable permissions of ADR-014".

## 14. Open points for Nick

Each has a recommendation. The design above takes the recommended reading wherever it bites.

- **O1. The word for the role.** Keep "operator" everywhere (wire, CLI, console), or show "Admin" in the console? *Recommend:* keep "operator". It is the word in every ADR, error and command, and a second word for one role needs explaining everywhere.
- **O2. May an operator edit the member column?** *Recommend:* only an operator holding `change_server_settings`, which is off by default. Any operator by default would let a delegated operator open the server (member links, sign-up), against ADR-010 D2's "openings and policy are owner". Precedence still bounds what such an operator can grant.
- **O3. Brakes only, or person-level grants too?** *Recommend:* brakes only (§5). A grant above the role would turn the matrix into a default rather than the truth. To give one person more, make them an operator.
- **O4. Role-level `make_member_links` off: suspend or close open links?** *Recommend:* suspend. A preset applied by mistake should be reversible. The per-person brake keeps ADR-013's close.
- **O5. Should a member link name the channels it lands in?** *Recommend:* yes, for holders of `manage_channels` only (owners always; operators by default), any visibility, in place of the default channels. It is checked with `permittedTx` at creation and again in the redemption transaction, and a landing membership never carries `can_decide` or `manages`. Without it, a newcomer on a personal server lands in nothing until the owner adds them. This changes ADR-013's link shape, so it needs to land in #978 before `InviteV2` is published.
- **O6. Audit export for operators** as a permission (`read_audit_log`, off by default)? *Recommend:* yes. ADR-010 kept it owner-only with the counter-argument recorded ("if it wins, the row flips"). A cell lets the owner flip it without another ADR.
- **O7. Server name editable from the console?** *Recommend:* not now. It is a host setting like the TOTP rule, and making it an API setting adds a route, an audit row and a rename story for the front door. The console shows it read-only.
- **O8. Configurable channel-manager rights?** *Recommend:* no. ADR-010's manager protections are written for a fixed list.
- **O9. Screen share as its own permission?** *Recommend:* yes, `share_screen`, requiring `use_voice`. Nick listed it, it costs one handler check, and a server may want voice without sharing. The brake covers both, as today.
- **O10. Who sees the matrix?** *Recommend:* every operator reads it, and only the owner (and an operator with `change_server_settings`, for members) writes it. Members see their own effective list in whoami and Settings, not the configuration.
- **O11. Preset warnings or refusals** for a default channel or open sign-up on a personal server? *Recommend:* warnings (§6). Either may be deliberate.
- **O12. The refusal code.** `permission_denied` with an optional `permission` field on `schema.Error`, keeping one existing code per question where one already answers? *Recommend:* yes (§7.4).
- **O13. Credentials and hooks already issued when `act_as_members` is taken away.** They stay live (§7.5): neither table records an issuer. *Recommend:* accept this for now, and say so in the matrix row ("taking this away does not end credentials and hooks already issued; use Sign out everywhere"). If Nick wants them ended, a later issue adds `issued_by` to both tables, which cannot cover the ones issued before it.

Decided by the security review rather than open: `act_as_members` as its own permission (§4.1).

## 15. What a reviewer should push on

- §7.2's remapping: every one of the 35 operator routes is in exactly one row (21 + 4 + 5 + 5), and under the defaults no answer changes.
- §7.2.1: no store function still grants an operator standing by role alone, and the structural test's allowlist holds only target protection and sign-in strength.
- §7.3: every link path (creation and redemption, v1 and v2, `open_channels`) reads the maker's permissions in its own transaction.
- §4.4's invariants are checked in the store transaction, not only in `Validate`.
- §7.5: the link suspension really is reversible, and role-level voice eviction meets #984's bound.
- §9: the alpha ordering. A release with the directory but without A3b must not reach alpha.
- §8.2: the patch shape fails safe for a client from before a later permission.

## 16. Revision after the security review (PR #1008), 2026-10-30

- **BLOCK-1, adopted.** A restricted operator who manages a channel no longer gets operator standing there. The store's standing functions and the handlers read `manage_channels` (§3.1, §7.2.1). The structural test covers the store's actor-role checks (§7.1). Isolation rows and an approval end-to-end case pin it (§7.7). The PRs are approval-path (A3a, A3b).
- **BLOCK-2, adopted.** Link creation and redemption, and `open_channels`, read `manage_channels` for channels beyond the maker's own and `manage_members` for open links and `may_invite` grants. Both are checked at creation and again at redemption (§7.3). O5 is narrowed to holders of `manage_channels`. §7.6's private-channel answer is corrected: the audit export reveals them.
- **Should-fix 1, adopted as decided.** `act_as_members` is split from `manage_members`. The catalog is now eleven, and the remap is 21 + 4 + 5 + 5 (§4.1, §7.2).
- **Should-fix 2, adopted.** Sign-in, reset and enrolment links check `act_as_members` at redemption and are suspended without it (§7.3, §7.5, A4). The unfixable remainder, credentials and hooks already issued, is O13.
- **Should-fix 3, adopted.** Channel member lists, nets, everyone's invites, closing another's invite, and session runtime and host are mapped (§7.3). The sign-in-strength branches are listed exceptions (§4.2).
- **Should-fix 4, adopted.** `share_screen` is checked on every poller pass (§7.3).
- **Should-fix 5, adopted.** Fixed cells are applied in code on read (§7.1, §9). The writer rule is re-checked in the PUT's transaction (§4.4).
- **Nits, adopted:**
  - `GET /v1/audit` needs recent authentication for non-owners (`reauthNonOwners`), and `read_audit_log`'s description says what it reveals;
  - invariant 4 checks the writer's effective permissions, brakes included;
  - owners skip brakes, recorded in §4.3 with the CHANGELOG line;
  - one refusal code per question;
  - the ADR-010 amendments are restated in ADR-014.
- **Where this revision goes slightly beyond or differs from the review:**
  - `act_as_members` requires `manage_members` (a dependency, §4.4.2), so that nobody can become a person they cannot see administered.
  - `open_channels` is checked only when written, not at each address join, because it has no maker to re-check. It stays server policy until someone changes it.
  - Issue A3 is split into A3a (store authority, safe alone) and A3b (routes), with A3a first. The remap must never ship without the store fix.
