# Design: delegated administration and channel permissions

- **Status:** Accepted by the principal, 2026-10-21 (issue #697), after a security review (BLOCK, then PASS with should-fix on re-check) whose findings are folded in (manager adds bounded to shared channels; the owner mark revokes the target's credentials and hooks; managers cannot remove approvers; owner protection covers memberships; owner-bound hooks hidden from the operator list; the owner CHECK). [ADR-010](../adr/ADR-010-delegated-administration.md) was accepted by the owner on 2026-10-22 (`53fb8e3`, PR #726); the role model it records amends ADR-003 and ADR-008, and implementation follows §10.
- **Decided before this note** by the principal, D1–D8 on #697: two server roles stay plus an owner mark; a delegated administrator is an operator who is not an owner; a moderator is a per-channel grant, not a role; grants never exceed the granter; the last owner who can sign in is never locked out; agents stay separate; precedence is additive and server-enforced; the browser only hides and explains. This note fills those in and says where it chose something they leave open (marked *choice*).
- **Touches:** [ADR-003](../adr/ADR-003-multi-human-access.md) (membership is the visibility boundary), [ADR-008](../adr/ADR-008-private-communications-alpha.md) Decision 4 (an invite grants only what its maker holds; it never grants operator rights), [ADR-009](../adr/ADR-009-guests-and-open-joining.md) items 3, 4 and 6 (guests hold no durable role; operators re-authenticate for sensitive changes), [private-alpha.md](private-alpha.md) §3 ("no per-group moderators in the alpha", which this note supersedes once ADR-010 is accepted), [guests-and-accounts.md](guests-and-accounts.md) §4.3 and §5.3, [channel-rename.md](channel-rename.md) §3.7 (who may rename), and #698 (moderation, which builds on the manager grant).
- **Owner:** protocol-designer (this note, the shapes in §7); server-engineer, web-engineer and cli-engineer for the issues in §10.

Today one role administers everything: an operator may do anything on the server, and a member may do nothing beyond their own channels. The alpha puts several separate groups on one server, and the person who runs it cannot be the only one who adds a tester to a channel, renames one, or stops a share. This note adds two narrower standings, each enforced in the server: a **delegated administrator**, an operator the owner has made but who cannot touch the owner or the roles; and a **channel manager**, a member who runs one channel's membership and name and nothing else.

## 1. Terms

| Term | Means | Stored as |
| --- | --- | --- |
| **Owner** | An operator with the owner mark: the person who runs the server. The only standing that changes roles, makes other owners, reads the audit trail, or acts on another owner. The migration marks every existing operator an owner, so nobody loses power on upgrade. | `principals.role = 'operator' AND principals.owner = 1` |
| **Operator** (delegated administrator) | An operator without the mark. Everything an operator does today, except what §3 lists as owner-only. Made and unmade only by an owner. | `role = 'operator' AND owner = 0` |
| **Channel manager** | A human member of one channel with the `manages` grant on that membership. In that channel only: adds and removes people, renames it, stops a share, and the moderation actions #698 defines. Granted and taken away only by operators. | `channel_members.manages = 1` |
| **Member** | A human or agent principal with `role = 'member'`; what it may do in a channel is its membership, `can_decide`, its manifest (agents) and its grants (`may_invite`, `voice_allowed`). | unchanged |
| **Guest** | ADR-009's temporary visitor: `kind = human`, `role = member`, a guest state, no durable role, no grant of any kind. Design-only today (no code on main). | ADR-009 / #705 |

"Operator" below means a non-owner operator unless it says "any operator". An owner is always also an operator: every check for "operator" passes for an owner.

## 2. Facts on main, verified at 900b43a

- Roles are two strings with a CHECK constraint (`internal/server/store/store.go:167`, migration 7). `Store.BootstrapOperator` is the only code that writes `'operator'` (`store/roles.go:52-108`); `CreatePrincipal` and invite redemption always write `'member'` (`store/queries.go:168`, `store/invites.go:636`). No route changes a role.
- Four route classes exist (`internal/server/auth.go:55-70`): `accessAuthenticated`, `accessOperator`, `accessOperatorOrSelf`, `accessExempt`. `guard` (`auth.go:196-215`) wraps only `accessOperator` routes, with `isOperator` (`auth.go:124`), and only under AuthRequired; a refusal is 403 `forbidden` with an `access_denied` audit row (`denyForbidden`). `accessOperatorOrSelf` is authentication only; the handler decides. `TestRouteTable` (`internal/server/auth_test.go:349`) fails on any route without an expectation of its class.
- `internal/server/routes.go` has 33 `accessOperator` and 3 `accessOperatorOrSelf` routes: 36, every one in §4.
- Operator behaviour also lives inside `accessAuthenticated` handlers: a non-member's member list (`members.go:97`), all nets (`nets.go:134`), everyone's invites (`invites.go:166`), stopping a share from outside the room (`voice_share.go:219`), a session's runtime and host (`sessions.go:401`), open links and the `may_invite` grant in an invite (`store/invites.go:375-440`, re-checked against the maker's role as it is now at redemption, `store/invites.go:625-628`).
- Approvals give operators nothing: deciding needs membership and `can_decide` (`approvals_handlers.go:285-297`; quorum counts only `can_decide = 1`, `store/approvals.go:415, 450`). `can_decide` is set only by `PUT /v1/channels/{channel}/members/{principal_id}` (`store/members.go:140-168`), operator-only; an invite joiner is inserted with 0 (`store/invites.go:659`).
- The lock-out guard `ErrLastOperator` (`store/disable.go:16`) counts live credentials of *other enabled operators* (`liveOperatorCredentialsTx`, `store/disable.go:19-34`) inside the IMMEDIATE transaction of: `disablePrincipalTx` (`store/disable.go:140-160`; reached by disable, hide and both bulk disables), `RevokeAllCredentials` (`store/disable.go:244-263`) and the single-credential revoke (`store/credentials.go:402-418`, which leaves that credential out of the count). Bulk hide refuses operators (`ErrHideOperator`, `store/hide.go:38-41, 105`); bulk disable skips operators and the caller (`invites.go:480-490`); an operator may not hide themselves (`principals_admin.go:128`).
- The member list shows `can_decide` to operators and to each member about themselves only, and never a member's role (`members.go:135-150`, `pkg/schema/channel_member.go:15-33`). `GET /v1/channels` lists the caller's own channels, operators included (`channels.go:51-56`).
- The isolation suite (`e2e/isolation/matrix.go`) has one row per route and fails CI on a route without one (`e2e/isolation/routes.go`). Operator routes are rows with `want: operatorOnly` (403 `forbidden`); content routes expect `channelNotFound` (404), and a refusal must be byte-identical for a thing that never existed.
- The browser hides almost nothing by role: `may_invite` gates the invite panel, `role === 'operator'` keeps the credential out of lasting storage and shows the operator warning (`web/src/api.ts:377, 422`, `web/src/state/auth.ts:50`). It has no UI for membership, principals, credentials, nets, manifests, audit or settings; the CLI's `conch admin` commands call the API and print the server's refusal (`internal/cli/command.go:195-200`).
- Migrations run to 30 on main (`store/store.go:636`). Onboarding issues #705 and #707 take 31 and 32; this design's migration is numbered after them, when its issue is dispatched.

## 3. The standings, in full

### 3.1 Owner and operator

An owner holds every right. An operator holds every right an operator holds today, with these exceptions, all server-enforced (§5 says where):

| Owner-only | Why |
| --- | --- |
| Change anyone's role, or the owner mark (`PUT /v1/principals/{id}/role`, new) | D2. A right nobody grants to themselves. |
| Act on an owner through any principal-addressed operator route: rename, disable, enable, hide, issue or list credentials, revoke all credentials, invite permission, sign-in links, disable-invitees, manifest (an owner is human, so this is moot) | D2, one rule: *an owner's account is not an operator's to touch.* One rule means one test per route rather than a list of exceptions. Includes "reduce-only" actions (enable, close a link) so that the rule has no edge an operator has to remember. |
| Act on an owner's credential by its id (rotate, revoke), or on a sign-in link made for an owner | Same rule. Answered 404 (§5.3): credential and link ids are not an operator's to enumerate. |
| Remove an owner from a channel or a net, or change an owner's `can_decide` (routes 4, 5, 9) | The same rule (security review of this note, finding 4): an operator who removed the owner from every channel would cut the owner off from the content and from the approval loop until they re-added themselves. Adding an owner to a channel or net stays allowed. An owner may still leave a channel themselves. |
| Bind a webhook to an owner (`POST /v1/hooks` with an owner's `principal`), or revoke one so bound | A hook token posts as its principal without a credential (`store/roles.go:96-104`); binding one to an owner is impersonating the owner. Revoking stays under the same one rule. |
| Export the audit trail (`GET /v1/audit`) | *Choice.* The trail names every principal, every refusal and every operator's own actions server-wide; it is the record owners check operators by, and the one route that hands over everything in bulk. An operator investigating one person has `GET /v1/principals`, invite joiners and the channel's own history. The counter-argument (an on-duty operator wants the trail during an incident) is real; if it wins, the row flips and nothing else here moves. |
| Server-wide policy settings that ADR-009 adds (`open_joining`, `guests_enabled`, `max_guests`, guest bounds, `server_name`, the open-admission channel set) | *Choice.* Opening the server, or how wide, is the owner's decision; these do not exist on main, so the rule is recorded here for #710 to apply. |

Operator-level, with reasons:

| Stays operator | Why |
| --- | --- |
| Closing any invite, the owner's included (`DELETE /v1/invites/{id}`, `store/invites.go:732`) | An accepted edge of the owner rule: closing an invite is a brake and reduce-only, and an operator on duty must be able to stop a link that is being abused whoever made it. |
| Invite pause/resume (`PUT /v1/invites/settings`) and ADR-009's "End all guest visits" | *Choice.* Brakes. Whoever is on duty when something goes wrong must be able to pull them without waking the owner; both are reversible and audited. Rule of thumb used throughout: **brakes are operator; openings and policy are owner.** |
| Create channels, people and agents; manifests; nets; membership and `can_decide`; `manages`; credentials, links, disable, hide and invite administration for non-owners; hooks for non-owners; reports | Today's rights, D2. `POST /v0/principals` always makes a member (`store/queries.go:168`), so this creates nothing above a member. |
| Peer actions: an operator may disable, hide, rename, credential or sign-in-link another operator | Nothing there exceeds what the actor holds (D4), and the audit trail records it. Two operators can undo each other; the owner resolves that, with the trail. |

Demoting an operator ends nothing else (D5): their credentials, memberships, `may_invite`, `voice_allowed` and any `manages` stay. Three things it does close, in the same transaction, because they are rights of the role, not of the person (§8, question 4): the open invite links they made, the sign-in links they made, and their personal invites unless they hold `may_invite` in their own right (the rule `SetMayInvite(false)` already applies, `store/invites.go:1044-1050`). Main already refuses the first two at redemption once the maker is no longer an operator (`makerStillAllowed`, `store/invites.go:206-214`; `usableSignInLinkTx`, `store/sign_in_links.go:199-206`), so closing them is not a new safeguard; it makes the lists say so, each with its `invite_closed` or `sign_in_link_closed` row, as `disablePrincipalTx` writes them through `closeCreatorInvitesTx` and `closeSignInLinksTx`.

### 3.2 Channel manager

A grant on one membership row, `manages`, set and cleared only by operators through `PUT /v1/channels/{channel}/members/{principal_id}` (body `manages`). In that channel a manager may:

- **Add a person** (`PUT …/members/{principal_id}`, and the new `POST …/members` by name, §3.3), without `can_decide` or `manages` in the body. Who may be added by a manager: an enabled, non-hidden human member who is not a guest, not an operator or owner, **and who already shares at least one channel with the manager** (security review, finding 1). Principal ids are small sequential integers, so without that bound a manager could step through ids on its own channel and read back the whole server's directory, sorted by role. Every other id (outside the manager's channels, an operator, an owner, an agent, hidden, disabled, a guest, unknown) answers the same 404 `principal_not_found`, byte for byte, counts against the per-caller daily refusal limit of §3.3, and is audited `member_add_refused`. Operators are not bounded this way. *Choice:* not agents. An agent's channel membership is half of its permission (gate 1 of `authz.go`, the manifest is the other half) and both halves are operator-set today; letting a manager add an agent would let a manager widen what an operator's agent reads. The added person gets `can_decide = 0` (the invite-joiner rule, not the operator-add default of 1: a manager cannot grant what it does not hold, and it holds no approval right by this grant).
- **Remove a person** (`DELETE …/members/{principal_id}`) who is not an operator, not an owner, not an agent, and does not hold `can_decide`. The target's membership of the channel is checked **first**: a target who is not a member is 204 whatever their role, as today, so the route tells a manager nothing about non-members (finding 1). Only a current member who is an operator, owner or agent, or who holds `can_decide`, is 403; the manager already sees that member in the list. *Why `can_decide` (finding 3):* quorum counts decisions against current `channel_members` with `can_decide = 1` (`store/approvals.go:449-455`), so removing an approver drops their cast decision, and a manager's re-add inserts `can_decide = 0`. A manager could stall any approval in its channel and strip its approvers. Approvers are removed by an operator, which keeps the approval path out of a manager's reach. A manager may remove themselves; removing a channel's last manager is allowed (operators remain, D5).
- **Rename the channel** (`PUT /v1/channels/{id}/name`, #696) once it lands, under an attempt limit (§5.4).
- **Stop a screen share** in the channel (`DELETE …/voice/share`), beside the sharer and operators.
- The moderation actions #698 defines, each named there with its enforcement.

A manager may not: set `can_decide` (approval path, operator-only); grant or clear `manages`; create, archive or see channels they are not in; touch nets, manifests, credentials, hooks, reports or settings; invite beyond what `may_invite` already gives them; or anything server-wide. A manager sees the member list as a member does, plus each member's `role` (§7) so that the browser can say whom they cannot remove.

`manages` on an operator is allowed and inert (operator ⊇ manager); it becomes live if the person is demoted. *Choice:* this keeps "demotion ends nothing" literal. The demotion's response and audit detail name the channels the person manages (`manages=1,4,9`), so the owner sees what remains and can clear it per channel. The alternative, refusing `manages` on operators, would mean an owner who wants a demoted person to keep running their group has to grant it afterwards and in between the group has no manager.

`manages` is refused in the store, not only the handler, on: an agent (`ErrInviteNotAllowed`'s pattern: "only a person may manage a channel"), a guest (`guest_not_allowed`, the `can_decide` rule of guests-and-accounts §4.3 applied to the second flag), a disabled or hidden principal. Conversion of a guest to a member (ADR-009 item 4) leaves `manages = 0`: it grants nothing.

### 3.3 How a manager finds people to add

A manager must not learn about principals outside their channels (ADR-003; the isolation suite's first rule). So there is no principal directory for managers, and `GET /v1/principals` stays operator-only. Two ways in, neither widening what a manager can see:

1. **People they already see.** The browser offers, for adding, the members of the manager's other channels: lists it can read today (`GET /v1/channels/{channel}/members` for each channel it is in). No new route; the server tells the manager nothing it did not.
2. **By exact name.** *Choice:* `POST /v1/channels/{channel}/members` with `{"name": "…"}` adds the person so named and answers the new member entry (201, `ChannelMemberV1`) with their id. A name that is not an addable person — unknown, hidden, disabled, a guest, an agent, an operator (for a manager) — answers 404 `principal_not_found`, the same bytes for every such case, so the route confirms only that an exact name is a person a manager could add. That is the information a member already gets, at a limited rate, from renaming themselves to a taken name (409 `principal_exists`, `renameAttemptsPerDay`, audited `principal_rename_refused`, `principals.go:112-140`), so this is no new class of leak; the route gets the same per-caller daily limit on refusals and an audit row (`member_add_refused`, no name in it). Operators may use it too, for convenience and parity. The by-name route always inserts `can_decide = 0`, whoever calls it (finding 3); for a person already in the channel it is idempotent (200 with the member as they are, nothing written), so it can never reset an existing approver; an operator grants the right with `PUT …/members/{principal_id}` as today. For a manager, the by-name route has the same bound as the id route: only a person who shares a channel with the manager can be added; anyone else is the same 404.

Not chosen: a lookup route without the add (one more route for the same leak), and a name in the `PUT` body beside the path id (two addresses for one thing).

### 3.4 What does not change

- Approvals: `can_decide` is the only approval right. The owner mark grants none; neither does `manages`. Operators set `can_decide`. On the approval path, the membership-write transaction gains a second flag and a manager actor, and a manager is refused removing a member who holds `can_decide` (§3.2), so a manager cannot change any approval's quorum (§10, issue B2 is labelled accordingly).
- Agents: manifests, capabilities, the manifest routes and the MCP endpoint are untouched. No agent can be an owner, an operator or a manager; the store refuses each (`owner = 1` or `role = 'operator'` on `kind = 'agent'`, `manages = 1` on an agent membership) so that no handler can forget it. There is no MCP tool for any of this and none is needed: administration is a human act (parity holds; the browser and CLI call the same routes).
- Guests: no role, no owner mark, no `manages` (ADR-009 item 3), refused in the store. On every route in §4 a guest is a member who holds nothing, and the answer is the member's.
- Voice, occupancy, sessions, runs, read state, files, search: no change. `--auth off`: every route is open as today; roles, marks and grants do not apply.
- The host: whoever runs `conchd bootstrap` or restarts it with `--auth off` is above every rule here. Owner recovery when every owner has lost access is that, and ADR-009's offline recovery command once it exists.

## 4. The capability matrix

One row for each of the 36 operator-class routes on main, in `routes.go` order, then the operator behaviours inside other handlers, then what this design and #696 add. Columns: what each standing gets. "Yes" is the route's success; "403" is `forbidden`; "404" is the route's not-found answer; "self" means only about the caller's own account. The enforcement column says where the check lives (§5); "class" alone means the route's class is unchanged.

Legend for the operator column: **not owner** = yes, unless the target is an owner (403, or 404 where §5.3 says); **peer** = including other operators.

### 4.1 Routes in `accessOperator` and `accessOperatorOrSelf` today

| # | Route (`routes.go`) | Owner | Operator | Manager | Member | Guest | Enforcement |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | `POST /v0/channels` (:26) | yes | yes | 403 | 403 | 403 | class `accessOperator` |
| 2 | `POST /v0/principals` (:28) | yes | yes (makes a member or an agent; never a role, never the mark) | 403 | 403 | 403 | class |
| 3 | `PUT /v1/principals/{id}/name` (:31, OrSelf) | anyone | not owner, peer | self | self | 403 (a guest has a label, not a name: guests-and-accounts §4.1) | class + handler: target owner and caller not owner → 403; guest caller → 403 `guest_not_allowed` |
| 4 | `PUT /v1/channels/{channel}/members/{principal_id}` (:49) | yes | yes; a body with `can_decide` on an owner target → 403 unless the caller is an owner, whether or not the owner is already a member (one rule, one test) | add a person who shares a channel with the manager (§3.2); body with `can_decide` or `manages` → 403; any other target (outside its channels, operator, owner, agent, hidden, disabled, guest, unknown) → 404 `principal_not_found`, limited and audited | 403 if a member of the channel, 404 if not | as member | **class → `accessChannelManager`** (new, §5.2) + handler + store re-check |
| 5 | `DELETE /v1/channels/{channel}/members/{principal_id}` (:50) | yes | not owner (403), peer | membership checked first: a non-member target is 204 whatever its role; a member who is an operator, owner or agent, or holds `can_decide` → 403; self allowed | 403 / 404 | as member | **class → `accessChannelManager`** + handler + store re-check |
| 6 | `POST /v1/channels/{channel}/nets` (:53) | yes | yes | 403 | 403 | 403 | class |
| 7 | `DELETE /v1/channels/{channel}/nets/{net}` (:55) | yes | yes | 403 | 403 | 403 | class |
| 8 | `PUT …/nets/{net}/members/{principal_id}` (:56) | yes | yes | 403 | 403 | 403 | class |
| 9 | `DELETE …/nets/{net}/members/{principal_id}` (:57) | yes | not owner (403), peer | 403 | 403 | 403 | class + handler owner check |
| 10 | `PUT /v1/principals/{id}/manifest` (:109) | yes | yes | 403 | 403 | 403 | class |
| 11 | `GET /v1/principals/{id}/manifest` (:111, OrSelf) | yes | yes | 403 | 403 (the agent itself: yes) | 403 | class + handler (unchanged) |
| 12 | `POST /v1/principals/{id}/credentials` (:112) | yes | not owner, peer | 403 | 403 | 403 | class + handler owner check |
| 13 | `GET /v1/principals/{id}/credentials` (:113) | yes | not owner, peer | 403 | 403 | 403 | class + handler owner check |
| 14 | `POST /v1/credentials/{credential_id}/rotate` (:114) | yes | yes; an owner's credential → 404 `credential_not_found` | 403 | 403 | 403 | class + store (`ErrCredentialNotFound` when the credential's principal is an owner and the actor is not) |
| 15 | `DELETE /v1/credentials/{credential_id}` (:115) | yes; last-owner guard | as 14 | 403 | 403 | 403 | class + store |
| 16 | `POST /v1/principals/{id}/disable` (:128) | yes; last-owner guard | not owner, peer | 403 | 403 | 403 | class + handler owner check + store guard |
| 17 | `POST /v1/principals/{id}/enable` (:129) | yes | not owner, peer | 403 | 403 | 403 | class + handler owner check |
| 18 | `POST /v1/principals/{id}/hide` (:135) | yes; not self (400); last-owner guard | not owner, peer, not self | 403 | 403 | 403 | class + handler owner check + store guard |
| 19 | `POST /v1/principals/hide` (:136) | yes (never hides an operator, so never an owner) | yes | 403 | 403 | 403 | class (unchanged: `RefuseOperator`) |
| 20 | `GET /v1/principals` (:137) | yes; shows `owner` | yes; shows `owner` | 403 | 403 | 403 | class |
| 21 | `POST /v1/principals/{id}/credentials/revoke-all` (:140, OrSelf) | anyone; last-owner guard | not owner, peer; self | self | self | self (ends the visit) | class + handler owner check + store guard |
| 22 | `GET /v1/audit` (:141) | yes | **403** | 403 | 403 | 403 | **class → `accessOwner`** (new, §5.2) |
| 23 | `POST /v1/hooks` (:142) | yes | yes, unless `principal` is an owner (403) | 403 | 403 | 403 | class + handler owner check |
| 24 | `GET /v1/hooks` (:143) | yes | yes, without the hooks bound to an owner (finding 5: route 25 answers 404 for those, so the list must not show them) | 403 | 403 | 403 | class + handler filter |
| 25 | `DELETE /v1/hooks/{id}` (:144) | yes | yes, unless the hook is bound to an owner (404 `hook_not_found`) | 403 | 403 | 403 | class + store |
| 26 | `GET /v1/invites/{id}/principals` (:160) | yes | yes | 403 | 403 | 403 | class |
| 27 | `POST /v1/invites/{id}/disable-joiners` (:161) | yes | yes (skips operators and the caller, as today) | 403 | 403 | 403 | class |
| 28 | `POST /v1/principals/{id}/disable-invitees` (:162) | yes | not owner (the root), peer | 403 | 403 | 403 | class + handler owner check |
| 29 | `PUT /v1/principals/{id}/invite-permission` (:163) | yes | not owner, peer | 403 | 403 | 403 | class + handler owner check |
| 30 | `GET /v1/invites/settings` (:164) | yes | yes | 403 | 403 | 403 | class |
| 31 | `PUT /v1/invites/settings` (:165) | yes | yes (a brake) | 403 | 403 | 403 | class |
| 32 | `POST /v1/principals/{id}/sign-in-links` (:170) | yes (a member target only, as today) | as today: an operator target, owner or peer, is already 400 `ErrSignInLinkTarget` (`store/sign_in_links.go:105`), so the owner check is moot unless #709 changes the target rule, and then it applies; a guest target is refused (ADR-009) | 403 | 403 | 403 | class + handler owner check |
| 33 | `DELETE /v1/sign-in-links/{id}` (:174) | yes | yes; a link made for an owner → 404 (the route's not-found code) | 403 | 403 | 403 | class + store |
| 34 | `GET /v1/reports` (:179) | yes | yes | 403 | 403 | 403 | class |
| 35 | `GET /v1/reports/{id}` (:180) | yes | yes | 403 | 403 | 403 | class |
| 36 | `PUT /v1/reports/{id}` (:181) | yes | yes | 403 | 403 | 403 | class |

### 4.2 Operator behaviour inside `accessAuthenticated` handlers

| Behaviour (file:line) | Owner | Operator | Manager | Member | Guest | Enforcement |
| --- | --- | --- | --- | --- | --- | --- |
| `GET /v1/channels/{channel}/members` as a non-member (`members.go:97`) | yes | yes | no: 404 (a manager is a member of what it manages, and sees nothing else) | 404 | 404 | handler (unchanged) |
| `GET …/nets`: all nets, not only one's own (`nets.go:134`) | yes | yes | no | no | no | handler (unchanged) |
| `GET /v1/invites`, `DELETE /v1/invites/{id}`: everyone's (`invites.go:166`) | yes | yes | own | own | 403 `human_required`-style refusal per ADR-009 (guests do not invite) | handler (unchanged) |
| `DELETE …/voice/share` by someone other than the sharer (`voice_share.go:219`) | yes, member or not | yes, member or not | **yes, in the channel it manages** | 403 | 403 | handler: sharer, or operator, or `manages` on the caller's membership (new check) |
| `GET /v1/sessions`: a session's runtime and host (`sessions.go:401`) | yes | yes | own | own | own | handler (unchanged) |
| `POST /v1/invites`: an open link; `may_invite` in an invite (`store/invites.go:375-440`) | yes | yes | no (personal invites only, with `may_invite`) | no | 403 | store (unchanged) |
| `PUT …/members/{principal_id}` body `can_decide` (`store/members.go:140`) | yes | yes | **no: 403** | — | — | handler + store (new: refused for a non-operator actor) |
| Approvals: request and decide (`approvals_handlers.go:285-297`) | by `can_decide` only | by `can_decide` only | by `can_decide` only | by `can_decide` only | never (ADR-009) | unchanged |

### 4.3 Added by this design and by #696

| Route | Owner | Operator | Manager | Member | Guest | Enforcement |
| --- | --- | --- | --- | --- | --- | --- |
| `PUT /v1/principals/{id}/role` (new, §6) | yes; self-demotion only if another owner can sign in; last-owner guard | **403** | 403 | 403 | 403 | **class `accessOwner`** + store re-check of the caller's mark in the transaction |
| `PUT /v1/channels/{channel}/members/{principal_id}` body `manages` (new field) | yes | yes | 403 | 403 / 404 | as member | handler + store (actor must be an operator) |
| `POST /v1/channels/{channel}/members` by name (new, §3.3) | yes | yes | add a person; refusals 404, limited | 403 / 404 | as member | **class `accessChannelManager`** + handler + store |
| `PUT /v1/channels/{id}/name` (#696) | yes | yes | **yes**, limited (§5.4) | 403 if a member, 404 if not | as member | **class `accessChannelManager`** (channel-rename.md §3.7 said `accessOperator` until this note decided; its isolation row changes from 403 to 404 for a non-member, §10 D) |

## 5. Enforcement and the order of checks

### 5.1 The rule

Every check lives in the server, in one of two places: the **route class** (`guard`), which answers before the handler runs and audits the refusal, or the **handler and store**, where the answer depends on the target or the body. The browser and CLI hide and explain; they enforce nothing (ADR-003; guests-and-accounts §2's last line). Deny is the default: a route added without a class is `accessAuthenticated` (zero value, `auth.go:56`), and a new flag read by nobody grants nothing.

Precedence is additive, from the top: owner ⊇ operator ⊇ manager (in its channel) ⊇ member ⊇ guest. A check for a lower standing passes for every higher one, so no row has "operator: no" where "manager: yes" (the one apparent exception, `manages` being inert on an operator, is additive too: the operator already holds the right).

### 5.2 Two new route classes

Beside the four classes of `auth.go:55-70`:

- **`accessOwner`**: a valid credential whose principal is an operator with the owner mark. `guard` wraps it as it wraps `accessOperator`; a caller without the mark gets 403 `forbidden`, "owner required", and an `access_denied` row. Routes: `PUT /v1/principals/{id}/role`, `GET /v1/audit`, and ADR-009's policy settings when they land.
- **`accessChannelManager`**: a valid credential that is (a) an operator, or (b) a human principal whose membership of the channel the path names carries `manages`. The guard resolves the channel from the path (`{channel}` by name, or `{id}` for the rename route), then: not an operator and not a member → the route's 404 `channel_not_found`, the same bytes as for a channel that never existed; a member without `manages` → 403 `forbidden`, "operator role or channel manager required", audited. An agent caller is refused first, 403 `forbidden` with the `humansOnlyBecause` audit reason, before any lookup, as every agent is on every such route. Routes: the two membership routes, the add-by-name route, the rename route, and #698's channel-scoped moderation routes. The guard may hand the resolved channel to the handler in the context, or the handler may resolve it again; the engineer's call, with the answers identical.

`TestRouteTable`'s `routeExpectations` gains the two classes (`classOwner`, `classManager`); the route-table test of guests-and-accounts §5.3, when it exists, classifies `accessOwner` routes as sensitive by its rule (they grant rights).

### 5.3 Order of checks

For every route in §4, in this order, so that refusals are uniform and a refusal never says more than the one before it would:

1. **Authentication** (`authMiddleware`): no, bad, expired or revoked credential → 401, one answer for all (`writeUnauthenticated`).
2. **Disabled or expired principal**: refused by `ResolveCredentialDetail` inside step 1; a guest past its bound likewise (ADR-009 §4.2).
3. **Kind**: on human-only routes (every route in §4.3 and the membership routes), an agent → 403 before any lookup.
4. **Route class** (`guard`): `accessOperator`/`accessOwner` → 403 for the wrong role or mark; `accessChannelManager` → channel lookup and membership → 404, then the grant → 403. Audited `access_denied`.
5. **Handler, target**: the thing the request names. An unknown principal, credential, link, hook → that route's 404. For a manager, a principal it may not add (outside the manager's channels, hidden, disabled, guest, agent, operator, owner, unknown) → the same 404 `principal_not_found` on both add routes, limited and audited (nothing is confirmed beyond "not an addable person"). On the remove route, the target's membership is read before its role: a non-member is 204; only a current member who is an operator, owner or agent, or holds `can_decide`, is 403 (the manager already sees that member in the list, with `role`, §7).
6. **Handler, body**: a field the caller may not set (`can_decide`, `manages` from a manager; `role: owner` without `owner` rights) → 403; an impossible combination (`owner: true` with `role: member`; a role on an agent) → 400 `invalid_request`.
7. **Store, inside the IMMEDIATE transaction**: every rule re-checked against the rows as they are under the write lock, so that two requests cannot race past a handler check: the actor's role, mark or `manages` (the actor id is passed into the store for every write a non-operator may make); the target's kind, guest state, role and mark; the last-owner guard (§5.5); the guest and agent refusals of §3.4. The store refuses with a typed error, and the handler maps it to the same status the handler check would have given.

Why 404 for a non-member on the manager class rather than 403: the isolation suite's first rule is that a refusal must not tell B that A's channel exists. The two membership routes answer 403 today because only operators reach them; once members can, the answer for a non-member has to be the same as for no channel at all. Their isolation rows change from `operatorOnly` to `channelNotFound` (§10 C). An owner's existence is not a secret of this kind: owners appear in the principal list with their mark, so the owner-protection refusal is an honest 403 on principal-addressed routes. Credential, link and hook ids are different: nothing lists an owner's to an operator, so a probe by id is answered 404, "as one never issued" (the own-credentials rule of `routes.go:120-126`).

### 5.4 Limits for managers

Managers are members with one more right, so where an operator route has no limit, a manager's use of it gets the member's limits:

- Add-by-name refusals: the daily per-caller attempt limit and audit row of §3.3.
- Renames: #696's route gets, for a non-operator caller, the limits `PUT /v1/principals/{id}/name` applies to a self-rename (`renamesPerDay`, `renameAttemptsPerDay`, `principals.go:100-102`), because a 409 `channel_exists` tells a manager that a channel of that name exists somewhere on the server; limited and audited (`channel_rename_refused`, no name in the row) it is the same bounded leak as a person's. channel-rename.md §7 left this open; this is the answer.
- Membership writes: no limit beyond the request-rate limits every route has; each is audited (`member_added`, `member_removed`, actor the manager).

### 5.5 Lock-out rules

The guard moves from "the last operator who can sign in" to **"the last owner who can sign in"**: a principal counts if it is an enabled operator with the owner mark holding a live credential (not revoked, not expired). Since every owner is an operator, the old condition is implied by the new one, and no second guard is needed. `liveOperatorCredentialsTx` (`store/disable.go:19-34`) gains `AND p.owner = 1`; `ErrLastOperator`'s message becomes "this would leave no owner able to sign in"; the wire code stays `last_operator` (409) on every route that sends it today, because the statement is still true (there would be no owner, so no operator, able to sign in) and `SignIns.tsx:20` and the CLI map it. *Choice*; if the owner prefers a new code `last_owner`, it is an additive vocabulary entry on the same routes, and the old code is retired on them in the same PR.

The guard runs, inside the transaction, on exactly these store paths:

| Store path | Reached by | Today | After |
| --- | --- | --- | --- |
| `disablePrincipalTx` (`store/disable.go:140-160`) | `DisablePrincipal` (route 16), `HidePrincipal` (route 18, `store/hide.go:110`), the bulk disables (routes 27, 28, via `disablePrincipals`, `invites.go:495-520`) | last operator | last owner |
| `RevokeAllCredentials` (`store/disable.go:244-263`) | route 21, operator or self | last operator | last owner |
| `RevokeCredential` (`store/credentials.go:402-418`) | route 15, sign-out (`DELETE /v1/credentials/current`), own revoke (`DELETE /v1/credentials/mine/{id}`) | last operator (that credential left out of the count) | last owner, same exclusion |
| `SetRole` (new, §6): demoting an owner to member, or clearing the mark | `PUT /v1/principals/{id}/role` | — | refused when no *other* owner can sign in, so an owner may demote themselves only if another owner who can sign in remains (D5) |
| ADR-009's operator password/TOTP reset and "sign out everywhere" for an owner (#709, #708) | later routes | — | the same guard, by the same helper; those issues must call it (recorded here so they do) |
| ADR-009's operator-issued password-reset links and member device links (guests-and-accounts §6.1, §6.2; #708, #712) | `SetRole` setting the mark | — | not a lock-out rule but the same family as §6's revocation: setting the owner mark also closes every outstanding reset and device link for the target, by one helper, so no operator holds a way to reset an owner's password; #708, #709 and #712 must call it. Sign-in links for the target are closed too, for a tidy trail (an operator target is refused at redemption anyway, `store/sign_in_links.go:207-210`) |

Not guarded by the last-owner rule, on purpose: rename, rotate (a rotated credential is still live), manifests, hooks, and an owner leaving a channel themselves. Removing an owner from a channel or a net is not a lock-out (the owner can sign in and re-add themselves), but a non-owner operator may not do it (§3.1, owner protection). Bulk hide and bulk disable never touch operators, so never owners.

The count is of credentials that would still authenticate today. When ADR-009's operator login lands, "can sign in" also means "has a password and TOTP enrolled, or a live credential"; #709 updates the helper's query and its test, not the rule.

## 6. The role route

`PUT /v1/principals/{id}/role`, class `accessOwner`, humans only. Body:

```go
// pkg/schema/role.go (sketch; the exact Go lands under the schema-change skill)

// RoleChangeRequestV1 is the body of PUT /v1/principals/{id}/role: the
// role the principal is to have, and whether it is to carry the owner mark.
type RoleChangeRequestV1 struct {
    // Role is "operator" or "member".
    Role PrincipalRole `json:"role"`
    // Owner, when present, sets or clears the owner mark. It may be true
    // only with Role "operator". Absent: an existing operator keeps its
    // mark, a member has none.
    Owner *bool `json:"owner,omitempty"`
}

// RoleChangeResponseV1 is the principal as it now is, plus what the change
// left in place, so an owner demoting someone sees what they still hold.
type RoleChangeResponseV1 struct {
    Principal PrincipalAdminV1 `json:"principal"`
    // Manages lists the ids of the channels the principal still manages,
    // in id order; always an array, empty when none.
    Manages []int64 `json:"manages"`
    // InvitesClosed is how many of the principal's invites the demotion
    // closed (§3.1); zero for a promotion.
    InvitesClosed int `json:"invites_closed"`
    // CredentialsRevoked and HooksRevoked are how many of the principal's
    // live credentials and bound hooks the change revoked: every one when
    // the owner mark is set, none otherwise.
    CredentialsRevoked int `json:"credentials_revoked"`
    HooksRevoked       int `json:"hooks_revoked"`
    // LiveCredentials and HooksBound are how many remain after the change,
    // so an owner promoting someone to operator sees what tokens exist.
    LiveCredentials int `json:"live_credentials"`
    HooksBound      int `json:"hooks_bound"`
}
```

*Choice:* the owner mark rides on the role route rather than a route of its own. D1 and D2 name promotion and demotion but not how a second owner is made; without a way, no owner could ever be made after the migration, and D5's "an owner may demote themselves only if another owner remains" could only ever be met by servers that had two operators before the upgrade. Making an owner is the owner's act (D4: an owner grants owner), and one route with one audit action keeps the surface small.

Answers, in the order decided (after §5.3's steps 1–4):

| Case | Answer |
| --- | --- |
| `{id}` unknown, or hidden | 404 `principal_not_found` |
| Target is an agent | 400 `invalid_request`, "an agent has no role" (store-refused too) |
| Target is a guest | 400 `invalid_request`, "a guest holds no role" (ADR-009 item 3; store-refused) |
| `owner: true` with `role: member` | 400 `invalid_request` |
| `role` not one of the two | 400 `invalid_request` |
| Nothing changes (same role, same mark) | 200, nothing written, nothing audited |
| Demoting, or clearing the mark of, the last owner who can sign in (self included) | 409 `last_operator` (§5.5) |
| Target is disabled | allowed: a disabled operator demoted stays disabled; enabling later revives nothing. A disabled owner is not "able to sign in" and does not count for the guard |
| Otherwise | 200 `RoleChangeResponseV1`; one audit row `principal_role_changed`, subject `principal:<id>`, detail `from=member to=operator owner=true manages=… invites_closed=… credentials_revoked=… hooks_revoked=…`; for a demotion, the person's open sockets stay (nothing they can see changes) but their operator-only reads end at the next request |

In the same transaction: a demotion closes the invites and sign-in links §3.1 names, each with its own row as `SetMayInvite(false)` and `disablePrincipalTx` write them. **Setting the owner mark revokes every live credential of the target and every hook bound to the target** (`revokePrincipalCredentialsTx`, `revokePrincipalHooksTx`), with their audit rows, and reports the counts; the promoting owner then issues a fresh credential with route 12, which only an owner may do for an owner. *Why (security review, finding 2):* any operator may issue a credential for a member or a peer and keep the plaintext token (`internal/server/credentials.go:90`), or bind a hook to them, and the role is read per request (`store/resolve.go:54-86`). Without the revocation, a promotion to owner would turn those into owner-grade tokens held by whoever made them, and `credentials` has no `created_by` column to revoke only those. A promotion to plain operator changes no membership, no `can_decide` and no credential, and reports `live_credentials` and `hooks_bound`, so the owner sees what tokens exist and can revoke them. **When the change revoked credentials (`credentials_revoked > 0`), the handler runs after commit the same follow-up as `handleRevokeAllCredentials`**: `hub.DropPrincipalAll`, `events.dropPrincipalAll`, `events.notifyRosters` and `voicePrincipalLostAccess` (`internal/server/disable.go:120-124`, `credentials.go:279-284`), so every socket opened with a revoked credential closes at once (#519). The "open sockets stay" of a demotion below does not apply to setting the mark. The store re-reads the caller's mark under the lock (`SetRole(ctx, actor, actorID, target, role, owner)`); an actor row that is no longer an owner is refused with `ErrNotOwner`, mapped to 403, however the request got past the guard.

Re-authentication: guests-and-accounts §5.3 puts every operator route that grants a right in the recent-authentication class, by rule; this route is in it. Until #709 lands there is no mechanism, and the route is owner-only by class alone; when it lands the route-table classification test requires the proof, with no change here.

## 7. Wire shapes, for the owner's sign-off

All additive: new optional fields, new types, no published field changes meaning, every existing fixture byte-identical (`scripts/schema-compat.sh`). No version bump is expected. Each lands under the schema-change skill with a fixture and a `ts.go` entry.

| Change | Where | Who sees it |
| --- | --- | --- |
| `WhoAmIResponseV1.Owner bool json:"owner,omitempty"` | `pkg/schema/whoami.go` | the caller; true only for an owner (`role` stays `operator`) |
| `ChannelV0.Manages bool json:"manages,omitempty"` | `pkg/schema/channel.go` | in `GET /v1/channels` only, true for a channel the caller manages; omitted elsewhere (`CreateChannelResponse`, the rename response, fixtures unchanged). *Choice* over a new list entry type: one optional field, no new type |
| `ChannelMemberV1.Manages *bool json:"manages,omitempty"` | `pkg/schema/channel_member.go`; also on the events socket's `conch.channel_members.v1` through `memberDocs` | every member of the channel, for human members. *Choice:* unlike `can_decide`, shown to all; a manager's acts are visible anyway, and members need to know whom to ask. Absent for agents (Validate: an agent carries neither flag) |
| `ChannelMemberV1.CanDecide`, existing field: who sees it | `internal/server/members.go` `memberDocs` | today operators, and each member about themselves (#450). **Now also a manager of that channel** (security re-review, finding 2): a manager is refused removing an approver, so refused removes would tell it who the approvers are anyway; showing it is the honest answer and lets the browser hide the control. No wire change. |
| `ChannelMemberV1.Role PrincipalRole json:"role,omitempty"` | same | filled only for a viewer who is an operator or a manager of that channel; omitted for everyone else, so the member list tells a plain member nothing it does not today. The browser uses it to explain "this member is an operator" before trying. *Choice*; could be dropped, and then the browser explains after the 403 |
| `PutChannelMemberRequestV1.Manages *bool json:"manages,omitempty"` | same | operators only; a manager sending it → 403 |
| `AddChannelMemberRequestV1 {Name string}` and its `Validate` (the person-name rule) | `pkg/schema/channel_member.go` | body of `POST /v1/channels/{channel}/members`; answers `ChannelMemberV1` |
| `RoleChangeRequestV1`, `RoleChangeResponseV1` (§6, with the revocation and remaining counts) | `pkg/schema/role.go` (new) | owners |
| `PrincipalAdminV1.Owner bool json:"owner,omitempty"` | `pkg/schema/principal_admin.go` | operators, in the principal list and the hide response |
| Error codes: none new. `forbidden`, `principal_not_found`, `credential_not_found`, `channel_not_found`, `invalid_request`, `last_operator`, `guest_not_allowed` (ADR-009's) cover every refusal above | — | — |
| Audit actions (not wire shapes; listed so the vocabulary stays complete): `principal_role_changed`, `member_manage_granted`, `member_manage_revoked`, `member_add_refused`, `channel_rename_refused` | `internal/server/store` | — |

Not changed: `PrincipalRole`'s two values, `PrincipalV0`, `ChannelMembersV1`'s schema name, any socket's first frame, any MCP tool, any invite shape (no invite carries `manages`, by construction).

## 8. Escalation analysis

Each question tried against the rules above; "no" means every path was checked.

1. **Can an operator make themselves owner?** No. The only write to `principals.owner` is `SetRole`, behind `accessOwner` and a store re-check of the actor's mark under the lock. `POST /v0/principals` writes `member`; invite redemption writes `member` and `owner = 0`; `BootstrapOperator` refuses while any operator exists (`store/roles.go:85`). A credential for an owner (route 12), a sign-in link for an owner (32), a hook as an owner (23), an owner's credential by id (14, 15): all refused. A credential or hook an operator made for someone *before* that person is made owner is revoked by the promotion itself (§6). A test asserts by grep that no SQL in `store/` but `setRoleTx` and the migration updates `owner`.
2. **Can a manager make themselves operator or owner?** No: the role route is owner-only; `manages` and `can_decide` are refused from a manager in the handler and the store; an invite they make carries their channels and voice and nothing else; `may_invite` comes only from an operator (`store/invites.go:625-628` re-reads the maker's role at redemption).
3. **Can a manager remove an operator, an owner or an agent from a channel?** No: 403 in the handler, and `removeChannelMemberTx` re-reads the target's role and kind when the actor is not an operator. A manager can remove a fellow manager (a person; D3 allows it) and themselves.
4. **Does a demoted operator keep a sign-in link or open invite link they made?** No. Main already refuses both at redemption once the maker is no longer an enabled operator (`usableSignInLinkTx`, `store/sign_in_links.go:199-206`; `makerStillAllowed`, `store/invites.go:206-214`). The demotion also closes them explicitly, with personal invites they could make only as an operator, so the lists say so (§3.1, §6). Credentials stay (D5).
5. **Can an invite carry `manages`?** No. No invite shape has the field, `invite_channels` has no column for it, and the joiner's membership rows are inserted with the column's default 0 (and `can_decide = 0`, `store/invites.go:659`). A schema test asserts `CreateInviteRequestV1` has no such field; a store test asserts a joiner manages nothing.
6. **Can a manager re-add someone an operator removed?** Yes, and today an operator can too; the audit trail shows both. A removal that must stick is a ban, which #698 defines (it must survive re-add and re-invite; this note gives it the `accessChannelManager` class to hang on and the operator/manager split for who may ban).
7. **Can two owners demote each other at once?** No: both run `SetRole` in IMMEDIATE transactions; the second sees the first's row and finds no other owner able to sign in → 409. Likewise an owner cannot disable the other owner and then demote themselves.
8. **Can an operator lock out the owner?** No: every path that could (16, 18, 21, 15, 27, 28) is refused for an owner target or skips operators; and the last-owner guard holds even for owners acting on owners.
9. **Can an operator give an owner's rights to someone?** No: nothing an operator can issue (credentials, links, hooks, invites, manifests, membership, `can_decide`, `manages`) carries the mark or the role, and what an operator issued to someone before an owner promotes them is revoked by the promotion (§6).
10. **Can a manager learn about people outside their channels?** Only what §3.3 bounds: that an exact name is an addable person, at a limited rate, audited; the same bound a member has today. Not by stepping through ids: the id add route answers one 404 for everyone outside the manager's channels, and the remove route answers 204 for every non-member, so neither tells a manager anything about a principal it does not already see (finding 1). Member lists stay membership-gated; `GET /v1/principals` stays operator-only; `role` in a member list is filled only for people who may act on it.
11. **Can a guest become anything?** No: the store refuses role, mark and `manages` on a guest; conversion grants nothing (ADR-009 item 4); a converted member is granted `manages` by an operator later, audited, like `can_decide`.
12. **Can an agent become anything?** No: the store refuses role, mark and `manages` on `kind = 'agent'`; the role route and the manager class refuse agent callers before any lookup.
13. **Peer operators.** Two operators can disable, hide or re-credential each other; neither can touch the owner, and the owner can undo both with the trail. Accepted (D2's "keeps today's operator rights"); if it bites in practice, the fix is to protect operators from peers the way owners are, a one-line widening of the same check.
14. **`manages` left on a demoted operator** (§3.2): visible in the demotion response and audit detail; the owner clears it per channel. Not an escalation (it is a right below operator), but a leftover the owner is told about.

## 9. Browser: who can administer, and why an action is unavailable

The browser hides what the caller cannot do and says why; the server is what refuses (ADR-003). It knows what it needs from three fields: `owner` on whoami, `manages` on the channel list, and `manages` plus `role` on the member list (§7). Where the browser has no UI today (principals, credentials, nets, manifests, audit, settings), the CLI is the surface and prints the server's message; those explanations are the server's `message` strings, written to be read.

A settings view ("Who can administer") shows the caller's own standing in one line: "You are the owner." / "You are an operator. Only the owner changes roles or acts on the owner." / "You manage #a and #b." / "You are a member." / "You are a guest; a visit cannot hold any right."

| Action (where) | Shown to | When hidden or refused, the text |
| --- | --- | --- |
| Change a person's role or owner mark (principal list, CLI) | owner | "Only the owner can change roles." |
| Disable, hide, rename, credential, sign-in-link, invite-permission for an **owner** (principal list, CLI) | owner | "This person is the owner; only an owner can change the owner's account." |
| Export the audit trail (CLI) | owner | "Only the owner can export the audit trail." |
| Add a person to this channel (member list) | operators, this channel's managers | "Only operators and this channel's managers add people." |
| Add by name: not found | — | "No one by that name can be added here." (one text for every refused case) |
| Remove a member (member list) | operators; managers for members whose `role` is `member`, `kind` is `human` and `can_decide` is false | manager, target operator: "Operators can only be removed by an operator."; target agent: "Agents are managed by operators."; target an approver: "Approvers are removed by an operator." |
| Make or unmake a manager (member list) | operators | "Only operators choose who manages a channel." |
| Set who decides approvals (member list) | operators | "Only operators choose who decides approvals." |
| Rename this channel (channel list, #696 child b) | operators, this channel's managers | "Only operators and this channel's managers rename it." |
| Stop someone's share (voice strip) | the sharer, operators, this channel's managers | "Only the sharer, an operator or a channel manager can stop a share." |
| Create a channel (#695) | operators | "Only operators create channels." (as #695 has it) |
| Pause or resume invites, hooks, reports, nets (CLI) | operators | "Operator role required." (the server's text today) |
| Anything, as a guest | — | "A guest visit cannot do this; create an account and ask an operator." (ADR-009; the guest's panel already says what a visit can do) |

Rules for the texts: plain words, no role the caller cannot see (a member is never told "this member is an operator"; only operators and managers get `role`), and never a name the caller did not type.

## 10. Follow-up implementation issues

Drafts, not filed; the principal files them after ADR-010 is accepted, in this order. Every server issue is `security` and gets a `security-reviewer` pass (new authorization surface); B2 and the e2e issue are **approval-path** because they change the transaction that writes `can_decide`, and ship with the full request → notify → resolve → audit chain exercised (CLAUDE.md rule 3; Nick merges). Migrations: numbered after #705's (31) and #707's (32) when dispatched; B1 and B2 share one migration if they land together, else two consecutive. Every new route needs a `routeExpectations` entry (`internal/server/auth_test.go`) and an isolation row (`e2e/isolation/matrix.go`); CI fails without either.

**A. Schema: owner mark, manages, role change, add by name** (protocol-designer; `area/schema`; Nick signs off §7 on the issue before dispatch)
- Files: `pkg/schema/whoami.go`, `channel.go`, `channel_member.go`, `principal_admin.go`, `role.go` (new), `testdata/` fixtures (`whoami-response-v1` unchanged plus a new `whoami-response-v1-owner.json`; `role-change-request-v1.json`, `role-change-response-v1.json`, `add-channel-member-request-v1.json`; a members fixture with `manages` and `role`), `ts.go`, regenerated `web/src/schema/*` (`go generate ./pkg/schema`).
- Acceptance: every change in §7, additive; `scripts/schema-compat.sh` passes; `ChannelMembersV1.Validate` refuses `manages` on an agent; `RoleChangeRequestV1.Validate` refuses `owner: true` with `role: member` and an unknown role; a test asserts no invite type has a `manages` field.
- Tests: golden and `Validate` tables per type; `make check`.
- Blocked by: ADR-010 acceptance.

**B1. Store: owner mark, last-owner guard, role change** (server-engineer; `area/server`, `security`)
- Files: `store/store.go` (migration: `ALTER TABLE principals ADD COLUMN owner INTEGER NOT NULL DEFAULT 0 CHECK (owner IN (0, 1) AND (owner = 0 OR role = 'operator'))`, so no write can leave a member marked; `UPDATE principals SET owner = 1 WHERE role = 'operator'`; a Go step that logs, as `reportReservedNames` does, the hooks bound to operators and each operator's live-credential count, for the owner to review after the upgrade, since all of them become owner-grade; no audit row, so the pinned trails of `e2e/p2-hardening`, `e2e/p3-local`, `e2e/dogfood` do not move), `store/resolve.go` (`ResolveCredentialDetail`'s SELECT loads the mark, which `accessOwner` reads; `isOwner` checks role and mark), `store/roles.go` (bootstrap writes `owner = 1`; `SetRole`, `ErrNotOwner`, `ErrRoleNotAllowed`; `principalState` returns the mark), `store/disable.go` (`liveOperatorCredentialsTx` counts owners; message), `store/queries.go` (`Principal.Owner`), `store/invites.go` (the demotion's invite closing reuses `SetMayInvite`'s closing step).
- Acceptance: §5.5's table holds on every listed path; §6's table holds in the store, including the revocation of credentials and hooks when the mark is set; an agent or guest target is refused; the migration marks every operator and nothing else; `BootstrapOperator` makes an owner; the only `UPDATE … owner` statements are the migration and `setRoleTx` (a test greps `store/`).
- Tests: table-driven store tests for promote, demote, mark, self-demote with and without another owner, the race of §8.7 (two transactions), a demotion's invite closing and `manages` listing, the guard on each path with an owner vs a non-owner operator target; migration test on a database with two operators.
- Blocked by: A.

**B2. Store: `manages`, manager-actor membership writes** (server-engineer; `area/server`, `security`, **approval-path**)
- Files: `store/store.go` (migration: `ALTER TABLE channel_members ADD COLUMN manages INTEGER NOT NULL DEFAULT 0 CHECK (manages IN (0, 1))`), `store/members.go` (`PutChannelMember` takes the actor's id and standing; refuses `can_decide` and `manages` from a non-operator actor, any target outside the manager's channels or an operator/owner/agent target from a manager (one error, mapped to 404), removing a member who holds `can_decide` by a manager, removing an owner or changing an owner's `can_decide` by a non-owner operator, `manages` on an agent, guest, disabled or hidden principal; a manager's add inserts `can_decide = 0`; `removeChannelMemberTx` re-reads the target for a manager actor; `SetChannelMemberManages`; `ChannelMember.Manages`; `ManagesChannel(ctx, channelID, principalID)`; `AddChannelMemberByName`), `store/invites.go` (joiner rows: `manages` left at 0; a test says so).
- Acceptance: §3.2 and §4.1 rows 4–5 and §4.3 in the store; `can_decide` behaviour for operators is byte-for-byte what it is today (the approval e2e proves it).
- Tests: table-driven per actor standing × target kind × body; the approval chain e2e (`e2e/dogfood` or the approval e2e) run with a manager-added member who must not be able to decide, and with a manager's attempt to remove an approver who has cast a decision (refused; the approval resolves as before); `go test -race`.
- Blocked by: A; B1 for the owner mark in `Principal`.

**C. Server: the classes, the routes, owner protection, manager rights** (server-engineer; `area/server`, `security`)
- Files: `internal/server/auth.go` (`accessOwner`, `accessChannelManager`, `guard`), `routes.go` (`PUT /v1/principals/{id}/role` → `accessOwner`; `GET /v1/audit` → `accessOwner`; the two membership routes and new `POST /v1/channels/{channel}/members` → `accessChannelManager`), `roles.go` (new handler), `members.go` (body rules, by-name add, its refusal limit and `member_add_refused`), `principals.go`, `disable.go`, `principals_admin.go`, `credentials.go`, `sign_in_links.go`, `invites.go`, `hooks.go` (the owner-target checks of §4.1, one helper `ownerProtected(caller, target)`), `voice_share.go` (manager may stop), `auth.go` whoami (`Owner`), `channels.go` (`Manages` in the list), `members.go` `memberDocs` (`manages`, `role` per viewer), `auth_test.go` (`routeExpectations`), `e2e/isolation/matrix.go` (rows: role route → `operatorOnly`; add-by-name on A's channel → `channelNotFound`; the two membership rows change from `operatorOnly` to `channelNotFound`; **a new row: B's manager adds A's tester by id to B's channel, answered byte-for-byte as a never-issued id**; audit export unchanged as `operatorOnly` since B's members are not owners either), `hooks.go` (the list leaves out owner-bound hooks for a non-owner operator), `roles.go` (after a mark that revoked credentials: the socket and voice drops of §6), `members.go` `memberDocs` (`can_decide` shown to the channel's managers).
- Acceptance: every row of §4 answers as stated for each of owner, operator, manager, member, guest (where guests exist; else a member without grants), agent; §5.3's order holds (a wrong-role caller never triggers a lookup; a non-member never learns a channel exists); the `last_operator` code is unchanged on its routes.
- Tests: **one table-driven unauthorized-access test per route in §4** (`internal/server/administration_test.go`: for each route, each standing, the expected status and code, and that a refusal writes `access_denied` where the class refuses), owner-protection tests per route 3, 4 (`can_decide`), 5, 9, 12–18, 21, 23–25, 28, 29, 33 with an owner target and a non-owner operator caller; the matrix rows; `go run ./e2e/isolation`; the three pinned trails unchanged.
- Blocked by: B1, B2.

**D. Rename for managers** (server-engineer; `area/server`, `security`)
- Files: `routes.go` (`PUT /v1/channels/{id}/name` → `accessChannelManager`), `channels.go` (non-operator limits, `channel_rename_refused`), `e2e/isolation/matrix.go` (its row becomes `channelNotFound`), `channels_test.go`.
- Acceptance: a manager renames its channel; a member of the channel gets 403; a non-member 404; the limits of §5.4; an operator is unlimited.
- Blocked by: #696 child a, C. (If C lands before #696 child a, child a registers the route under the new class directly.)

**E. CLI parity** (cli-engineer; `area/cli`)
- Files: `internal/cli/admin.go` (`admin principal role <id> operator|member [--owner|--no-owner]`, `admin member manage <channel> <id> [--off]`, `admin member add --name <name>`), `command.go` (`whoami` prints `owner`), `admin principal list` shows the mark.
- Acceptance: every new route has a command (rule 4); refusals print the server's message.
- Blocked by: C.

**F. Browser: standing, member management, explanations** (web-engineer; `area/web`)
- Files: `web/src/api.ts`, `state/*` (owner, manages), `ChannelList.tsx` (manages badge), a new `Members.tsx` (add by name and from shared channels, remove, manage, decide, with §9's texts), a settings line "Who can administer", `App.tsx`; tests `members.test.tsx`, `App.test.tsx`; a Playwright spec: operator makes a manager; the manager adds and removes a member, cannot remove the operator, cannot set decide; a member sees no controls.
- Blocked by: A (generated types), C.

**G. Moderation (#698)** builds on `accessChannelManager` and the operator/manager split; its design names which of its actions are manager-level and which operator-level, and how a ban survives re-add (§8.6).

## 11. What a reviewer should push on

- §3.1's two owner-only choices (audit export; ADR-009's policy settings) and the brake rule: each flips a row and nothing else.
- §3.2's choices: no agents for managers; `manages` inert on operators; `can_decide = 0` on a manager's add.
- §3.3's add-by-name route and its leak bound.
- §5.3's 404-for-non-members on the manager class, which changes three isolation rows from 403 to 404.
- §5.5's keeping the `last_operator` code.
- §6's owner mark on the role route.
- §7's `role` on the member list for operators and managers.
