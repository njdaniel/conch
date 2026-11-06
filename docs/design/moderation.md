# Design: channel moderation

- **Status:** Proposed for principal and security review, issue #698. No implementation is authorized by this note. The wire changes in §10 additionally need Nick's sign-off and the `schema-change` procedure before code.
- **Builds on:** [ADR-010](../adr/ADR-010-delegated-administration.md) and [delegated-administration.md](delegated-administration.md), whose `accessChannelManager` class and target rules are now implemented; [ADR-009](../adr/ADR-009-guests-and-open-joining.md) and [guests-and-accounts.md](guests-and-accounts.md) for temporary identity and history bounds.
- **Scope:** four separate operations: voice-only removal, channel membership removal, channel entry ban, and inappropriate-message removal. Account administration, guest lifecycle and problem reporting are inputs to these decisions, not new operations here.
- **Current-code baseline:** `79e9d5c`. Paths and behavior below were checked there, not inferred from the older inventories in the earlier designs.

## 1. Decisions in one page

| Operation | REST contract | Who may act | Lasting effect |
| --- | --- | --- | --- |
| **Remove from voice** | **New** `DELETE /v1/channels/{channel}/voice/participants/{principal_id}` | Owner, operator, or manager of this channel | None. Evicts current media only; the same person may immediately rejoin. |
| **Remove channel membership** | **Reuse** `DELETE /v1/channels/{channel}/members/{principal_id}` | Existing owner/operator/manager rules, unchanged | Removes channel and net membership. It is not a ban; an authorized actor may re-add the principal. |
| **Ban channel entry** | **New** `PUT` and `DELETE /v1/channels/{channel}/bans/{principal_id}`; **new** `GET /v1/channels/{channel}/bans` | Same target limits as membership removal. Only operators may pre-ban a non-member; a manager lifts only their own bans | Atomically records a principal/channel ban and removes current membership. Re-add is refused until unban. Unban does not re-add. |
| **Remove a message** | **New** `DELETE /v2/channels/{channel}/messages/{message_id}` | Owner, operator, or manager of this channel, but only when the caller is also a member who can already see that message | Retains a tombstone and suppresses the original body, payload, search match and attachment access. No undo or hard-erasure claim in this slice. |

The distinctions matter:

- A voice kick is not a membership removal and creates no reconnect restriction.
- A membership removal is not a ban and revokes no credential.
- A ban is principal- and channel-scoped, not a server-wide disable and not reliable identification of a person behind a new account or guest visit.
- Message removal is not account deletion, physical database deletion, report filing, or removal of every copy a recipient may already have made.

No operation adds an MCP administration tool. Browser and CLI/TUI parity comes through these REST routes, as ADR-001 requires.

## 2. What exists now

### 2.1 Authorization and membership

`internal/server/auth.go:59-77,249-346` implements `accessOwner`, `accessOperator` and `accessChannelManager`. The manager class refuses agents before lookup, lets any operator through, gives a non-member the same 404 `channel_not_found` as an unknown channel, and gives a member without `manages` a 403. These checks are meaningful only with authentication required; with `--auth off`, registered handlers run without role guards and audit as `system`, as other administration routes do today.

`DELETE /v1/channels/{channel}/members/{principal_id}` is already `accessChannelManager` (`internal/server/routes.go:55-57`). The store re-reads the actor under an IMMEDIATE write lock (`internal/server/store/members.go:680-768`):

- A non-owner operator may not remove an owner.
- A manager first checks target membership. A non-member target is an idempotent 204 regardless of whether the principal exists.
- A manager may remove only a human, non-operator, non-owner member with `can_decide = 0`; this excludes agents and approvers as well as owners/operators.
- A manager may remove a fellow manager or themselves. A guest is a human member with no durable grant, so a manager may remove an active guest's membership.
- Removal also deletes net seats. Actual changes write `member_removed` and `net_member_removed`; an idempotent no-op writes nothing.

After commit the handler calls `hub.DropPrincipal`, `events.dropPrincipal`, roster notification and `voiceMemberRemoved` (`internal/server/members.go:334-374`). Message sockets close so a later broadcast cannot race through; REST reads and posts subsequently fail membership checks. Credentials and memberships of other channels survive.

### 2.2 Voice

A voice session requires current membership, a human principal, `voice_allowed`, and a live credential (`internal/server/voice.go:35-185,207-227`). LiveKit participant identity is `p<principal_id>`, one identity per principal per room. `livekit.Client.Evict` reports whether `RemoveParticipant` actually disconnected that identity (`internal/server/livekit/client.go:250-265`).

Existing membership removal already closes voice presence sockets, removes the channel from occupancy sockets, rotates a room when an invalid holder requires it, and evicts the principal from other current rooms (`internal/server/voice_presence.go:1942-1967`). Disable, credential revocation and guest termination use related whole-principal hooks. The entitlement poller is the backstop for membership and credential loss.

There is no voice-only moderation route. The existing `voice.evict` helper is not sufficient on its own for one: it deliberately never fails the caller, and a still-entitled member can be re-added by a later pass if LiveKit removal fails. Section 4 therefore requires a result-reporting synchronous path, not merely a new handler around the fire-and-forget helper.

### 2.3 Disable, hide and guest termination

These remain server-wide operator controls, not channel-manager substitutes:

- `POST /v1/principals/{id}/disable` revokes all credentials, closes the principal's invites and sign-in links, advances authentication generation, ends sessions and drops sockets/voice. It does **not** remove ordinary memberships; enabling later restores no credential, but a new credential can use memberships that remain (`internal/server/store/disable.go:133-235`). Disabling an active guest ends the visit permanently.
- `POST /v1/principals/{id}/credentials/revoke-all` similarly ends current access without disabling the principal. New credentials remain possible.
- `POST /v1/principals/{id}/hide` disables, removes every channel/net membership, revokes hooks and renames the principal to `hidden-<id>`; old messages stay under the new name (`internal/server/store/hide.go:71-159`). It refuses a guest because guest attribution is immutable. This is account deletion semantics, not routine channel moderation.
- `POST /v1/guests/{id}/end` and the dry-run-capable `POST /v1/guests/end` end visits, remove their memberships and revoke their credentials (`internal/server/guests.go`, `internal/server/store/guests.go:673-765`). They are operator-only. A manager cannot end a visit server-wide.

A channel removal or ban must not silently invoke any of these broader operations.

### 2.4 Invites and entry

Operators can close any invite with `DELETE /v1/invites/{id}`; its maker can close their own. Invite pause, joiner disable and inviter-tree disable are separate operator controls (`internal/server/invites.go`). These are useful incident brakes but are not implied by a ban.

Current admission behavior bounds what a principal-ID ban can promise:

- `PUT`/`POST` channel membership routes can re-add an existing principal.
- `POST /v2/invites/redeem` creates a **new** principal and inserts its memberships (`internal/server/store/invites.go:673-807`). It does not sign an existing account into an invite.
- `POST /v1/join/account` likewise creates a new principal with fresh history bounds (`internal/server/store/guest_policy.go:594-707`).
- `POST /v1/join/guest` and `POST /v1/invites/redeem-guest` create a new guest principal, handle, credential and history bound. A later visit from the same address and label is intentionally a different identity.

Consequently, “survives re-invite” can mean only that the banned principal cannot be re-added by an operator/manager or by any future existing-account acceptance path. It cannot mean that a newly created account is known to be the same person. The design states this limitation rather than hiding it.

### 2.5 Messages, history and reports

Messages are currently insert-only. There is no message edit/delete route and no removal marker in storage. List and search share `messageVisible`, including audience and guest history-bound predicates (`internal/server/store/audience.go:200-316`). Threads use the same rule. File downloads use message visibility, with a separate uploader path (`internal/server/store/audience.go:479-560`). v0/v1/v2 sockets have different envelope capabilities; scoped messages go only to v2 recipients.

`POST /v1/reports` creates a diagnostic problem report. Its list, detail and filed-state routes are operator-only (`internal/server/reports.go`). Reports are not message-reporting or moderation queues, may contain free text, and must not be exposed to channel managers or automatically filed to the public repository. This design leaves them unchanged and does not retract text copied into one.

The isolation suite already checks cross-group route behavior and guest history across list, search, threads, files, read state, sockets and indirect objects (`e2e/isolation/matrix.go`, `guestrows.go`, `guestrevoke.go`). Moderation tests extend that mechanism rather than relying only on handler unit tests.

## 3. Common rules

### 3.1 Server-side authorization

All new channel routes use `accessChannelManager`; each gets a `TestRouteTable` classification, a recent-auth decision and an isolation row. Ownership is additive: owner and operator pass the class; a manager passes only in a channel they belong to and manage. Plain members and guests have no moderation standing.

The handler and store apply the operation's target rule as well:

- The store re-checks a non-operator actor's current `manages` grant inside every moderation write transaction. A grant revoked after the route guard cannot race the write.
- Existing owner protection and manager target restrictions apply to membership removal and bans. They do not make an owner's voice or messages immune from moderation in a channel. Voice removal has no durable account effect; message removal changes content, not the author's account or approval rights.
- Operators retain no general content-read exemption. In particular, message removal requires normal membership and message visibility even for an owner/operator.
- With authentication off there is no meaningful principal-scoped security boundary; the new handlers follow the existing unconstrained/system-actor behavior. Tests of authorization run with authentication required.

Ban and message routes are reduce-only like membership removal and invite close, so their recent-auth classification is `reauthNone`. Voice removal is also `reauthNone`. This adds no new grant or credential path and does not weaken the existing recent-auth rules.

### 3.2 Confirmation and feedback

Confirmation is a client requirement, not a pretend server guarantee: a direct REST call cannot prove that a person read a dialog. Browser and CLI/TUI flows show the exact target and channel and require an explicit second step before calling the route. Non-interactive CLI use requires an explicit `--yes`; interactive use defaults to cancel.

The confirmation text names the actual consequence:

- **Voice:** “Remove `<name>` from voice in `<channel>` now. They can rejoin; chat access is unchanged.”
- **Membership:** “Remove `<name>` from `<channel>` and its nets. This does not ban them or revoke their account.”
- **Ban:** “Remove `<name>` from `<channel>` and block this principal from being added again. Other channels are unaffected. A new account or guest visit is not identified by this ban.”
- **Message:** “Remove this message for everyone in its audience. Future reads show a tombstone; server backups and copies already received cannot be recalled.” Include the message only from a view the actor can already read.

Success feedback distinguishes “removed”, “already absent/removed” where the API reports only idempotent 204, and “unban does not re-add”. Voice failure says removal was incomplete and offers retry; it never reports a successful kick after a failed LiveKit call. Managers are not given `GET /v1/audit` or server-wide report access to provide feedback.

### 3.3 Audit

Successful state changes and their follow-up effects are attributable to the authenticated actor; guard refusals keep the existing `access_denied` behavior. Audit detail carries IDs and bounded result codes, never message bodies, payloads, search text, invite codes, credentials, room names, free-form moderator reasons or client addresses.

New actions are:

| Action | When | Subject and detail |
| --- | --- | --- |
| `voice_participant_kicked` | Once per authorized voice-removal request, including an absent-participant no-op | Subject `channel:<id>`; `principal=<target> result=completed|failed`. Actual disconnection also keeps the existing `voice_participant_removed` telemetry with `reason=moderator_kick`. |
| `channel_ban_added` | A new ban row commits | Subject `channel:<id>`; `principal=<target>`. Membership/net removals have their existing rows too. |
| `channel_ban_removed` | An existing ban is lifted | Same subject/detail. No membership is restored. |
| `message_removed` | A message changes from visible to removed | Subject `message:<id>`; `channel=<id>`, no content or reason. |

Repeating an existing ban, unbanning a non-existent ban, or removing an already removed message writes no new moderation row. Existing membership and voice no-op rules remain as described above.

## 4. Remove from voice only

### 4.1 Contract and authorization

**New:** `DELETE /v1/channels/{channel}/voice/participants/{principal_id}`, class `accessChannelManager`.

The route is idempotent and targets an identity, not a credential. An owner, operator or this channel's manager may remove any current human participant, including an owner/operator participant. A manager cannot use it in another channel. Agents cannot act and have no voice target. The response reveals only moderator standing, not whether the target was present:

- **204** after every current-room eviction succeeds, and when the target is already absent. With LiveKit unconfigured, 204 is also correct because no media participant can exist.
- **503 `voice_unavailable`** if any required LiveKit eviction cannot be completed. Some rooms may already have been evicted; the client must retry. This is preferable to a false 204.
- Common route-class errors: 401, 403 for a member without the grant, 404 `channel_not_found` for a non-member manager or unknown channel.

The implementation performs a manager-standing re-check before recording the request, then uses a new result-returning voice helper. It must not just call today's fire-and-forget `voice.evict`.

### 4.2 Live effects

After authorization and before the response:

1. `voice.dropPrincipal(channelID, principalID)` closes the target's voice-presence sockets for that channel. Message/events sockets and occupancy sockets stay open because membership is unchanged.
2. Forget the target from current presence and close any open transmit state so other members stop seeing them before LiveKit work.
3. Call LiveKit `Evict` for `p<principal_id>` in every current room of the channel, bounded by `voiceEvictTimeout`. Do not skip future net-room rows in this new helper. Do not rotate the whole room: the target remains entitled and other participants need not be disconnected.
4. Return 204 only if every eviction completed. On failure, log and return 503; because the principal is still entitled, the ordinary entitlement poller is not a removal backstop and presence may legitimately reappear. The audit result is `failed`, not a fabricated removal.

Success is a point-in-time eviction, not a hold: a concurrent or immediate rejoin is allowed and may make the target present again by the time the response arrives. Use a ban when re-entry must be refused.

No credential, voice grant, room-holder authorization, channel membership, screen-share right or message subscription changes. An active share is withdrawn as part of leaving that room, but may be claimed again on rejoin.

### 4.3 Reconnect, guests and history

- The same or an alternate credential of the same principal may immediately call `POST /v1/channels/{channel}/voice/session` again and reconnect sockets. The 15-second join-token path and normal membership/credential checks are unchanged.
- An active guest may likewise rejoin if its visit is still live and has voice. To keep that guest out, use a ban for the channel or an operator's End visit, not repeated kicks.
- Message history, search, files and read state are completely unaffected.

## 5. Remove channel membership

### 5.1 Contract and authorization

**Reuse:** `DELETE /v1/channels/{channel}/members/{principal_id}` with no route or authorization change. Section 2.1's owner/operator/manager target matrix and store re-check are normative. In particular, a manager cannot remove an approver, agent, operator or owner, and a non-owner operator cannot remove an owner.

Confirmation and audit use §3. Existing `member_removed` and `net_member_removed` rows are the authoritative record. There is no new `member_kicked` synonym.

### 5.2 Live effects and return access

The existing post-commit sequence is retained: channel message sockets drop, events sockets drop and rosters update, voice presence/occupancy access and LiveKit participation end through `voiceMemberRemoved`. Other channel subscriptions and credentials stay valid.

- New REST, history, search and voice requests in this channel fail as a non-member, normally 404 `channel_not_found`.
- Alternate credentials of the same principal gain nothing because authorization is membership-based.
- An authorized operator/manager may re-add the principal. A manager remains limited to an addable non-guest human who already shares a channel and cannot add agents, guests, operators or approvers.
- A normal member who is re-added gets the channel's ordinary history, including the absence interval; current code has no general member history watermark. An active guest re-admitted by an operator gets a **fresh** `history_after`, not access to the previous visit's earlier channel history.
- Removing an active guest from one channel does not end the visit or its credential, and does not stop it using other memberships or creating a separate account when policy allows. A new guest visit is a new principal with fresh bounds.
- Other members retain all existing messages; removal changes the audience, not history.

## 6. Ban future channel entry

### 6.1 Contract and stored meaning

New routes, all `accessChannelManager`:

- `PUT /v1/channels/{channel}/bans/{principal_id}` — create the ban if absent and atomically remove current channel/net membership. 204 for a new ban or one the actor may repeat.
- `DELETE /v1/channels/{channel}/bans/{principal_id}` — lift the ban. 204 for an existing ban the actor may lift or an absent ban. It does not restore membership. A manager's absent-ban 204 requires no target lookup, as for membership removal.
- `GET /v1/channels/{channel}/bans` — 200 `ListChannelBansResponseV1`, ordered by principal ID, so an authorized actor can see whom they can unban. The shape in §10 needs Nick's approval.

A ban is a durable `(channel_id, principal_id)` row with `created_by` and `created_at`, no expiry and no free-text reason in this slice. It is not a credential revocation or a server-wide principal ban. Ban and unban write only on an actual state change.

Owner/operator/manager target rules match membership removal:

- Only an owner may ban an owner; a non-owner operator gets 403.
- A manager may ban a current human non-operator/non-owner member with `can_decide = 0`, including an active guest or fellow manager. An approver, agent, operator or owner target gets 403.
- Operators may pre-ban a known non-member principal. A manager may not use PUT to enumerate arbitrary principal IDs: a target that is neither a current eligible member nor already banned by that manager gets the uniform 404 `principal_not_found`.
- A manager may repeat or lift only a ban they themselves created. They cannot repeat, lift or overwrite another manager's or an operator's ban; that gets 403. An operator may lift any ban except where owner protection applies, and an owner may lift any ban. `created_by` in the list makes this distinction visible.
- A manager may ban themselves; they then lose the manager grant with the membership and need an operator to unban/re-add them. Confirmation says so.

### 6.2 Admission and live enforcement

Creating the ban and deleting membership/net seats are one IMMEDIATE transaction, with actor/target checks repeated under the lock. Repeating an existing ban also removes any residual membership/net rows in that transaction; a legacy or restored row cannot survive merely because the ban already existed. Actual cleanup writes the usual `member_removed`/`net_member_removed` rows, and the handler runs §5's socket/LiveKit revocation after commit even when the ban row itself was unchanged. Other channels, credentials and sign-in methods stay valid.

Unban has a strict order in one IMMEDIATE transaction: **delete any residual membership/net rows first, then delete the ban row**. Cleanup is audited and followed by the same live revocation after commit; `channel_ban_removed` is written only if a ban existed. Thus every committed state has either a ban or no membership—never an unbanned residual membership that becomes active again. An absent ban remains a no-op and does not remove an ordinary current membership. Unban only makes a future authorized add possible; it restores no socket, net seat, approval right, manager grant or history access.

Every path that can create or restore a membership checks the shared ban predicate inside its write transaction: `PutChannelMember`, `AddChannelMemberByName`, guest admission through those writes, and any future existing-account invite acceptance or membership restoration. Account/guest invite redemption and bare-address joining currently create new IDs, so §6.4 applies instead of pretending the old ID's ban matches.

The predicate also guards reads, not only writes. `IsChannelMember`, `isChannelMemberTx`, and every direct `channel_members` SELECT/JOIN must mean “member and not banned,” including the raw audience/recipient conditions in `internal/server/store/audience.go`. Review and test the whole set: REST list/post/search, v0/v1/v2 WS admission and history gating, MCP reads, threads/replies, files and their uploader path, read state/runs and other indirect objects, voice presence/session/occupancy and both `voiceStillEntitled` checks. A residual banned membership therefore grants no content or media access even before cleanup runs.

The voice race follows the existing two-check mechanism rather than promising the impossible. If the ban commits before the final entitlement check in `voiceGrantOnce`, that check fails and no token is signed. If it commits after that check, a token may still be signed or returned; the ban's committed post-commit socket drops and normal `voiceMemberRemoved` rotation/`Evict` path terminate its use within the existing revocation bounds. Tests cover both sides and do not claim an absolute no-token-after-request guarantee.

### 6.3 Credentials, conversion and disable/enable

- All current and future credentials of the **same principal** are subject to the channel ban. Password login, credential rotation and a sign-in link do not bypass it.
- Guest-to-account conversion keeps the principal ID, so an existing ban survives conversion and conversion restores no removed membership.
- Disable/enable and hide/enable do not clear a ban. Those routes retain their separate server-wide semantics.
- A different principal, even with the same display name, password, browser or network address, is not covered. Server policy cannot prove whether it is the same person.

### 6.4 Anonymous guests and invite reality

An active guest can be banned from one channel. This removes that visit's membership and blocks re-admission of that guest principal. A manager cannot end the whole visit or re-add the guest later; those remain operator actions.

After End visit, expiry or a new redemption, a guest has a new principal and handle. The old ban does not and must not claim to cover it. The same is true of a newly created member account. No permanent anonymous identification is available in Conch today.

The honest mitigations are the existing coarse controls, chosen explicitly and audited separately:

- close an abused invite (`DELETE /v1/invites/{id}`), which also affects legitimate future users of that link;
- pause invites, pause joining, disable new guests, or end current guest visits as an operator;
- keep the existing per-address guest cap and rate limits as friction, not identity;
- disable an abusive current principal server-wide when channel-scoped action is insufficient.

A ban does not automatically close an invite or disable joiners: those have much larger blast radius and need their own confirmation. This design does **not** add IP bans, browser fingerprints, cookies as identity, device attestation or secret address logs. NAT, shared networks, proxies, address churn and new accounts make such a promise inaccurate, and storing more identifying data has its own privacy cost.

## 7. Remove an inappropriate message

### 7.1 Contract and authorization

**New:** `DELETE /v2/channels/{channel}/messages/{message_id}`, class `accessChannelManager`.

This is a soft removal, addressed by immutable message ID. It returns:

- 204 on the first removal and on an already removed message;
- 404 `message_not_found` for an unknown ID, a message in another channel, or one the caller may not see;
- the usual class-level 401/403/channel 404.

Although operators pass the route class from outside a channel, this handler then applies the normal content checks. Every actor, owner included, must be a current channel member and able to see the target under the existing audience and history-bound rules. An operator who is not a member gets 404 `channel_not_found`; a manager who cannot see a whisper, net message or pre-bound message gets the same 404 `message_not_found` as an unknown ID. No body, payload, recipient list or author metadata is returned to make the decision.

The author may be any principal, including an owner/operator, agent or guest; content moderation is not an account action. A manager can remove only what they can already read, not every message in the channel by ID enumeration.

### 7.2 Stored representation and audit

A migration adds `message_removals` keyed by `message_id`, with channel ID, actor and timestamp. The original `messages` row, author, audience, thread relation and timestamp remain. The removal and `message_removed` audit row commit atomically; the actor is re-checked under the write lock.

The original body and payload remain in the server database and backups in this alpha slice. They are suppressed on every product read surface, not cryptographically or physically erased. There is no unremove route. A later compliance/retention feature would be a separate design and must not be implied by this operation.

### 7.3 History, search, attachments and threads

For every current and future authorized reader:

- v0/v1/v2 list and thread reads retain the message ID and its position, author/guest attribution, audience, thread root and timestamp. Body becomes the fixed schema constant `MessageRemovedBody` (“This message was removed by a moderator.”), payload is omitted, and v2 adds `removed: true`. Older clients that ignore the new field still render the safe tombstone body.
- Search excludes removed messages in the SQL statement, before limit and cursor calculation. The original FTS row may remain; it must never return the removed body or affect page size/`next_before`. Searching an old term finds nothing.
- `GET /v2/files/{id}` and `/meta` refuse a file attached to a removed message with the same 404 `file_not_found` as an invisible file. This includes the uploader: the current uploader bypass must not reopen an attachment whose only attached message was removed. Unattached uploads retain current behavior.
- A removed thread root is a tombstone, not `thread_not_found`, so remaining replies keep a coherent parent. Replies are not cascade-removed and retain their own audiences. Reply creation rules are otherwise unchanged.
- Read cursors and unread counts may count a tombstone as an entry; IDs and pagination stay stable. No count exposes the removed text.
- MCP reads use the same store projection and show a tombstone only when the agent could already read the message. No MCP moderation tool is added.
- Text copied into reports, run activity, other messages, logs, notifications or client exports is independent content and is not recursively rewritten. No design can retract screenshots or a recipient's existing local copy.

### 7.4 Live delivery and reconnect

After commit, the server broadcasts the **tombstone projection**, never the original body or payload:

- Channel-wide messages use v0/v1/v2 message broadcasts with the same ID. v0/v1 receive the fixed body; v2 also receives `removed: true`. The configured `Server.Broadcaster` seam is called too, with exactly the safe v0 tombstone, as it is for an ordinary channel-wide post.
- Scoped messages use v2 only and the original recipient set intersected with currently authorized subscribers. They never call the configured broadcaster, and no v0/v1 frame, events-socket broadcast or global notification reveals that a whisper/net message existed.
- `internal/server/ws.go`'s replay filter is part of the contract, not an implementation detail. It tracks per-ID tombstone outcomes alongside the replay high-water mark: an original emitted by replay followed by a live removal still emits the tombstone once; a tombstone emitted by replay followed by the buffered original emits nothing more, and duplicate tombstones are suppressed. Ordinary buffered originals at or below the high-water mark remain suppressed. The live `historyGate` check applies to these removals too, including removals of older messages in later replay pages and scoped messages, so a tombstone cannot bypass audience or guest bounds.
- Stateful browser/TUI views upsert by ID rather than appending a duplicate. Streaming outputs do not rewrite earlier output; they emit only the safe tombstone they receive and never print the original again.
- No sockets are dropped and LiveKit is untouched. The author and other recipients stay connected.
- A reconnect or alternate credential receives the same tombstone through history, never the original. A guest still gets only messages within its history bound; a new guest visit gets no pre-visit removal frame or content.

Live retraction is best effort until the frame arrives. The durable guarantee is about subsequent server reads, not remote copies already displayed or saved.

## 8. Operation interaction guide

| Desired outcome | Correct action | Do not substitute |
| --- | --- | --- |
| Stop current noise but allow immediate return | Remove from voice | Membership removal or credential revocation |
| Remove chat and voice access but allow authorized re-add | Remove membership | Ban or disable |
| Keep this principal out after re-add attempts | Ban channel entry | Membership removal alone |
| Stop all current sessions of an account | Operator disable or revoke-all | Channel ban |
| Retire an account and its attribution name | Operator hide | Message removal; hide refuses guests |
| End every access held by a temporary guest | Operator End visit | Manager ban, which covers one channel/principal only |
| Suppress one inappropriate post | Remove message | Hide author or delete the whole channel history |
| Stop an abused anonymous entry link | Close invite or pause joining, separately confirmed | Claim that a guest principal ban identifies its next visit |

## 9. Security and isolation acceptance cases

The implementation must prove at least these cases on a real server:

1. A manager of B cannot call any new route on A's channel; the response and ghost-channel response are identical. A plain member of the same channel gets 403, an agent gets the human-only refusal, and no target lookup precedes those checks.
2. A non-owner operator cannot ban an owner. A manager cannot ban/remove an approver, agent, operator or owner. Pending approvals and `can_decide` are unchanged; if implementation touches approval authorization or its write transaction, it becomes `approval-path` work and additionally requires the complete request → notify → resolve → audit test and Nick's merge.
3. Voice removal leaves membership and chat sockets intact, evicts only the target from current channel rooms, permits immediate rejoin, and returns 503 rather than false success when LiveKit eviction fails.
4. Membership removal and ban take effect on every message/events/voice socket and LiveKit within the existing revocation bounds. Alternate credentials do not restore access; other channels remain usable. A ban before the final voice entitlement check prevents signing; a ban after it is handled by the normal revocation path.
5. Every existing-principal add path refuses a ban. Seeded residual membership is invisible through every raw content path, is cleaned with audit/live revocation on repeat ban, and is still absent after unban's cleanup-before-delete ordering. Unban then permits a later explicit add but restores nothing by itself. Conversion preserves the ban.
6. A banned guest's current principal cannot be re-admitted. A subsequent guest visit is demonstrably a new principal with a fresh history bound; the test does not assert that the old ban matches it. Invite closure and End visit remain separate operator controls.
7. A manager cannot remove an invisible whisper/net/pre-bound message or distinguish it from an unknown ID. An operator outside the channel cannot probe its messages through DELETE.
8. Removed body/payload text is absent from v0/v1/v2 history, threads, search, attachments, configured-broadcaster output and live frames for every original audience. Unauthorized recipients receive no frame or metadata. Replay emits original→tombstone once, never tombstone→original, including older/scoped messages. Removed rows do not alter search cursors/counts, and the uploader cannot fetch the removed attachment directly.
9. Cross-group marker scans and ghost comparisons in `e2e/isolation` cover every new route. Existing guest history tests continue to pass, and new cases combine removal with guest bounds and a subsequent visit.
10. Audit contains actor/action/target IDs and outcomes only—never message text, payload, invite code, token, room name or client address. Idempotent repeats follow §3.3.

## 10. Proposed wire and storage changes — Nick's approval required

Nothing in this section is approved merely by appearing here. Before implementation, Nick signs off the shapes and a protocol-designer runs the `schema-change` procedure. New audit action strings and internal SQLite tables are not published schema changes, but their review belongs with the same work.

| Proposal | Compatibility intent | Notes |
| --- | --- | --- |
| `ChannelBanV1 {principal_id, name, created_by, created_at}` and `ListChannelBansResponseV1 {bans}` | New types, additive | Name/handle is for display and confirmation; authorization always uses ID. No reason, role, address or credential data. Existing ban rows remain meaningful after rename/conversion. |
| `MessageV2.Removed bool json:"removed,omitempty"` | Additive optional field; no v3 expected | Existing fixtures must stay byte-identical. Add a removed-message fixture and generated TypeScript. `MessageRemovedBody` is a schema constant. Validation requires removed messages to have that body and no payload. |
| Error code `message_not_found` (404) | Additive vocabulary on `schema.Error` | One response for unknown, wrong-channel and invisible targets. Reuse existing `voice_unavailable`, `channel_not_found`, `principal_not_found`, and `forbidden` elsewhere. |
| SQLite `channel_bans` and `message_removals` tables | New migration, old rows remain readable | No published field changes meaning and no backfill rewrites content. Store the original message row; project a tombstone on read. |

All new REST routes otherwise have empty DELETE/PUT bodies or simple GET responses, so no confirmation-token, reason, expiry, role, address or ban-request shape is introduced. No MCP tool-definition change is needed for administration, but MCP message output must be checked against the additive `MessageV2` field in the same schema PR. `scripts/schema-compat.sh`, schema golden tests and generated consumers are mandatory.

## 11. Candidate follow-up issues for principal review

The follow-ups are filed as **#849 (M1), #850 (M2), #851 (M3), #852 (M4), #853 (M5) and #854 (M6)**. IDs M1–M6 are local ordering labels. Each implementation issue is security-sensitive and gets a `security-reviewer` pass. M1 additionally records Nick's §10 approval before dispatch.

### M1. Schema and storage contracts for moderation

**Context.** Implement only the approved contracts in `docs/design/moderation.md` §10: channel-ban records, message-removal records, ban-list responses and the additive v2 tombstone marker. No moderation route behavior is hidden in this schema PR.

**Files.** `pkg/schema/message_v2.go`, a new moderation/ban schema file, schema errors and fixtures, `pkg/schema/testdata/`, `pkg/schema/ts.go`, generated `web/src/schema/*`, `internal/server/store/store.go` and a focused migration test. Suggested labels: `area/schema`, `security`.

**Acceptance criteria.**

- [ ] Nick's approval of the exact §10 shapes is linked; no unapproved reason, expiry, address or role field appears.
- [ ] `ChannelBanV1`, `ListChannelBansResponseV1`, `MessageV2.removed` and `MessageRemovedBody` validate as specified; all existing fixtures are byte-identical.
- [ ] The migration creates only the approved ban/removal records and leaves existing messages, memberships and guest attribution readable.
- [ ] Generated TypeScript and MCP-facing message definitions are synchronized.

**Required tests.** Schema golden/Validate tables, removed-message validation, migration from the preceding schema version, `go test ./pkg/schema/...`, `./scripts/schema-compat.sh`, and `make check`.

**Blocked by.** #698 design acceptance and Nick's wire-shape sign-off.

### M2. Voice-only participant removal

**Context.** Implement `docs/design/moderation.md` §4. Current membership removal can evict but removes chat access; the existing voice eviction helper cannot report failure and cannot backstop a still-entitled member.

**Files.** `internal/server/routes.go`, a focused voice moderation handler, `voice_presence.go`, `livekit/client.go` only if its result contract needs reuse changes, route/recent-auth tests, `internal/server/administration_test.go`, voice integration tests, `e2e/voice` and `e2e/isolation`. Suggested labels: `area/server`, `security`.

**Acceptance criteria.**

- [ ] `DELETE /v1/channels/{channel}/voice/participants/{principal_id}` implements the exact actor matrix, 204/503 behavior and `voice_participant_kicked` audit result.
- [ ] Presence/transmit ends before LiveKit work; every current channel room is evicted without room-wide rotation; message/events/occupancy access remains.
- [ ] A failed LiveKit eviction never returns success, and immediate same/alternate-credential rejoin works after a successful kick.
- [ ] The target's membership, credentials, voice grant and history are unchanged.

**Required tests.** Actor/target matrix and isolation ghost rows; absent participant idempotency; LiveKit down/partial-room failure; real LiveKit e2e for Evict, presence closure, other participants staying connected and rejoin; audit redaction; `go test -race ./internal/server/...`; `CI=1 go run ./e2e/voice`; and `CI=1 go run ./e2e/isolation`.

**Verification prerequisites and commands.** Run from the repository root on Linux x86_64 with Go from `go.mod`, rustup/cargo (which reads `voice/rust-toolchain.toml`), Docker usable by the current user, host networking and loopback ports, and network access to pull the pinned LiveKit image and `lk` and to fetch the voice toolchain. Install the native packages required by `scripts/voice-env.sh`: `libglib2.0-dev libpipewire-0.3-dev libclang-dev pkg-config g++`. `./scripts/voice-toolchain.sh` installs the pinned clang, cargo-deny and verified libwebrtc into the user cache; sourcing `scripts/voice-env.sh` checks them and the native packages. `CI=1` is mandatory so a missing Docker/image/`lk`/Rust prerequisite fails rather than silently skipping the live half. This mirrors `.github/workflows/ci.yml:454-547`.

```sh
./scripts/voice-toolchain.sh
. ./scripts/voice-env.sh
go test -race ./internal/server/...
CI=1 go run ./e2e/voice
CI=1 go run ./e2e/isolation
```

**Blocked by.** #698. It does not depend on M1 unless implementation chooses to share internal moderation storage; no new voice wire shape is required.

### M3. Channel entry bans and admission enforcement

**Context.** Implement `docs/design/moderation.md` §6 on the M1 storage. Membership removal alone currently permits re-add; principal/channel bans must be checked transactionally at every existing-principal admission path without claiming to identify new accounts or guest visits.

**Files.** `internal/server/routes.go`, new ban handlers, `internal/server/store/members.go`, `internal/server/store/audience.go` and a new ban store file, guest admission and future existing-account admission seams, `internal/server/ws.go`, `internal/server/voice.go`, voice presence/occupancy, route/recent-auth tests, `e2e/voice` and `e2e/isolation`. Suggested labels: `area/server`, `security`.

**Acceptance criteria.**

- [ ] PUT/DELETE ban and GET ban list implement the exact routes, target rules, idempotency, confirmation data and audit actions.
- [ ] Ban plus membership/net removal is atomic and reuses the existing socket/LiveKit follow-up. Repeat ban cleans residual rows; unban cleans them before deleting the ban, with audit/live revocation, and restores nothing.
- [ ] Every raw membership read/write path applies the ban predicate, including REST/WS/MCP, threads, files, indirect objects and voice. A seeded residual membership grants no access before or after unban.
- [ ] Operator and manager adds and guest admission refuse the banned principal. A ban before the final voice entitlement check prevents signing; one after it is terminated by normal socket/LiveKit revocation without an absolute no-token claim.
- [ ] Conversion, alternate credentials and disable/enable do not clear a ban; a new account or guest visit is documented/tested as a different principal, not falsely matched.
- [ ] Approval rights are unchanged and managers cannot ban approvers.

**Required tests.** Table-driven actor × target × current-member cases; every admission and raw membership content path; seeded residual-membership tests before ban, after repeat ban, and after unban, including audit rows and live socket/LiveKit revocation; ban/add and both sides of the ban/final-voice-check race; guest conversion/new-visit cases; cross-group ghost comparisons and audit redaction; `CI=1 go run ./e2e/voice`; and `CI=1 go run ./e2e/isolation`. If the implementation touches approval authorization or the `can_decide` transaction, add `approval-path` and the full request → notify → resolve → audit chain test.

**Verification prerequisites and commands.** From the repository root, use Linux x86_64, Go from `go.mod`, rustup/cargo for `voice/rust-toolchain.toml`, Docker with Linux host networking and free loopback ports, and network access for the pinned LiveKit image, pinned `lk` and voice downloads. Install `libglib2.0-dev libpipewire-0.3-dev libclang-dev pkg-config g++`, then fetch the pinned clang/cargo-deny/libwebrtc with `./scripts/voice-toolchain.sh`; `. ./scripts/voice-env.sh` verifies that complete environment. Keep `CI=1` on both e2e commands so unavailable Docker, network, image, `lk` or Rust tooling cannot produce a skipped-but-green run. These are the voice/isolation prerequisites in `.github/workflows/ci.yml:454-547`.

```sh
./scripts/voice-toolchain.sh
. ./scripts/voice-env.sh
go test -race ./internal/server/...
CI=1 go run ./e2e/voice
CI=1 go run ./e2e/isolation
```

**Blocked by.** M1.

### M4. Audience-safe message removal

**Context.** Implement `docs/design/moderation.md` §7 on M1. Messages are currently immutable, list/search/thread/file reads share visibility predicates, and v0/v1/v2 need safe live tombstones without exposing scoped-message existence.

**Files.** `internal/server/routes.go`, a message moderation handler, `internal/server/messages.go` and its broadcaster tests, `internal/server/ws.go` and its replay tests, `internal/server/store/audience.go` and a removal store file, file visibility, `internal/server/hub/`, MCP read-output checks, route/recent-auth tests and isolation tests. Suggested labels: `area/server`, `security`.

**Acceptance criteria.**

- [ ] DELETE implements normal caller membership plus target visibility even for owners/operators; unknown and invisible targets are indistinguishable.
- [ ] Removal and audit are atomic and idempotent; original body/payload never appear in new responses, frames, configured-broadcaster output or audit.
- [ ] v0/v1/v2 list/thread history and live updates show the specified tombstone only to authorized audiences; search excludes removals before pagination. Channel-wide removal calls the configured broadcaster with the safe v0 tombstone; scoped removal never calls it.
- [ ] The ws replay filter emits original→tombstone once and suppresses tombstone→original/duplicate delivery, including older messages across replay pages and scoped removals under the live history gate.
- [ ] Removed attachments return 404 even to the uploader; thread replies, guest attribution/bounds and unrelated messages remain coherent.
- [ ] Reconnect and alternate credentials cannot recover the original through REST, WS or MCP.

**Required tests.** All message versions and socket types; channel-wide/net/whisper audiences; unauthorized live-frame absence; `ws.go` replay-hook tests for original-then-removal, replayed-tombstone-then-buffered-original, duplicate tombstones, older pages and scoped/history-bound removals; a recording-broadcaster test proving channel-wide output contains only the safe tombstone and scoped output contains nothing; manager history-bound and operator-nonmember probes; search terms/cursors; direct file/meta as recipient and uploader; removed thread root/replies; guest conversion and subsequent-visit history; audit/content redaction; race tests for remove/read/broadcast.

**Blocked by.** M1.

### M5. End-to-end moderation isolation and visibility matrix

**Context.** Add the real-server acceptance cases in `docs/design/moderation.md` §9 to the existing isolation harness. Unit tests alone do not prove that cross-group refusals, ghost objects, live frames and guest history bounds agree.

**Files.** `e2e/isolation/matrix.go`, a focused moderation scenario file, guest history/rejoin scenarios and any shared marker/socket helpers; `e2e/voice` only for coverage not already landed in M2/M3. Suggested labels: `area/server`, `security`.

**Acceptance criteria.**

- [ ] Every new route has a matrix row and ghost comparison, with no A-group markers in bodies or headers.
- [ ] Banned and removed principals stay out across REST, every socket family and LiveKit while retaining unrelated-channel access and valid alternate credentials elsewhere.
- [ ] Seeded residual membership is invisible, repeat-ban cleanup revokes it, and it remains absent after unban; both sides of the ban/final-voice-check race follow §6.2.
- [ ] Message removal is tested for cross-group isolation, scoped-audience non-disclosure, all history/search/file surfaces, replay/live ordering, safe live updates and reconnects.
- [ ] Guest tests prove current-principal bans, fresh identity/history on the next visit, and no pre-visit removal/content leak.

**Required tests.** `CI=1 go run ./e2e/voice`; `CI=1 go run ./e2e/isolation` with authentication and LiveKit enabled and the moderation, guest and revocation phases asserted; and the standard pinned audit-trail comparisons (updated only for the new expected actions, never by dropping assertions).

**Verification prerequisites and commands.** Run from the repository root on Linux with Go from `go.mod`, a working Docker daemon, Linux host networking and available loopback ports. Both suites need network access to pull the exact pinned LiveKit image and `lk`; `e2e/isolation` uses those for its media and LiveKit-revocation phases. Because `e2e/voice` also runs real `conch-voice` clients under `CI=1`, use Linux x86_64 with rustup/cargo, install `libglib2.0-dev libpipewire-0.3-dev libclang-dev pkg-config g++`, and run `./scripts/voice-toolchain.sh` for the pinned clang/cargo-deny/libwebrtc; `. ./scripts/voice-env.sh` verifies that toolchain and `voice/rust-toolchain.toml` is honored. `CI=1` makes every skipped media or Rust scenario a failure. See the exact CI setup at `.github/workflows/ci.yml:454-547`; require the moderation, guest and revocation phase assertions in the isolation output, not merely a zero exit.

```sh
./scripts/voice-toolchain.sh
. ./scripts/voice-env.sh
CI=1 go run ./e2e/voice
CI=1 go run ./e2e/isolation
```

**Blocked by.** M2, M3 and M4.

### M6. CLI/TUI and browser moderation parity

**Context.** Expose the four distinct operations with the confirmation and feedback in `docs/design/moderation.md` §3. The server remains the enforcement point; controls are hidden only as presentation.

**Files.** `internal/cli` administration/client commands and TUI member/voice/timeline state, `web/src/api.ts`, channel member/voice/message views and state, generated schema consumers, unit tests and Playwright specs. Suggested labels: `area/cli`, `area/web` (split into two issues if parallel ownership requires disjoint areas).

**Acceptance criteria.**

- [ ] Each operation has a CLI and browser path using its exact REST route; no MCP-only or client-only moderation mechanism exists.
- [ ] Confirmation text matches the operation's real scope, including rejoin, unban-does-not-re-add, anonymous identity limits and non-erasure of received copies.
- [ ] Voice 503 offers retry without claiming success; ban list/unban and idempotent feedback are clear.
- [ ] Only stateful browser/TUI views upsert v0/v1/v2 live tombstones by ID. Streaming CLI/TUI outputs emit only the safe tombstone received and never replay the original or pretend to rewrite prior output; inaccessible scoped messages create no UI notification.
- [ ] Members/guests without standing see no actionable control, and direct server refusals are displayed accurately.

**Required tests.** CLI command/API and streaming-output tests, browser state/component tests, interactive and `--yes` confirmation tests, and Playwright flows for manager/member voice kick, membership removal, ban/unban, message removal, reconnect and live tombstone updates.

**Verification prerequisites and commands.** Run from the repository root on Linux with Go from `go.mod`, Docker and host networking for the browser-voice spec, and network access for npm, Playwright's Chromium download and the pinned LiveKit image. Install the Node version in `web/.nvmrc` (24.21.0; CI uses `actions/setup-node` with that file), then install the locked web dependencies with `npm ci --no-audit` and the Playwright-pinned Chromium with `npx playwright install chromium`. On a CI image needing Chromium's native libraries, use `npx playwright install --with-deps chromium` as `.github/workflows/ci.yml:395-433` does. `make web-e2e` repeats the lockfile install, checks that Chromium exists, starts and removes its fixture server, and runs the suite; keep `CI=1` so missing Docker cannot silently skip browser voice. M6 itself does not build `conch-voice`; the inherited M2/M3/M5 gates still carry their separate Rust, native-package and pinned-`lk` prerequisites above.

```sh
(cd web && npm ci --no-audit)
(cd web && npx playwright install chromium)
go test ./internal/cli/...
make web-check
CI=1 make web-e2e
```

**Blocked by.** M2, M3, M4 and M5.
