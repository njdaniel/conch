# Design: pinned messages and channel resources

- **Status:** **Accepted by Nick, 2026-11-13** ("approve 1525"). His sign-off covers the five items of §12.2: the §10 shapes, additive with no version bump; the migration (47 here; main has since used 47 to 49 for the channel topic and push, so the implementation takes the next free number); the `pin` channel permission and the policy that agents may pin only under an explicit grant; the bounded exposure of §5.3; and 11.3's MCP slice being approval-path, which Nick merges. #1495 and the §11 split are unblocked.
- **History:** Proposed by the protocol-designer, 2026-11-07, for the project owner's sign-off (issue [#1494](https://github.com/njdaniel/conch/issues/1494), child of epic #1491; it gates #1495). Every decision below is a recommendation with its reason, made under the principal's delegation; nothing is left open except what §12 names for Nick. The schema change is additive (new types, one new vocabulary value, new error codes, no published fixture changes), so the `schema-change` skill asks no version bump; the issue asks for Nick's yes before implementation, and §10 is what he says yes to. Nothing here is implemented; no code, dependency or migration lands with this note.
- **Touches:** [nets-and-whispers.md](nets-and-whispers.md) and [ADR-005](../adr/ADR-005-nets-and-whispers.md) (audiences; visibility fixed at post time; one store predicate), [message-reactions.md](message-reactions.md) (the closest precedent: a per-message object read under the message's visibility, its routes, frames, agent rule and isolation rows), [moderation.md](moderation.md) §7 and §10.3 (tombstones, `message_not_found`, the member-who-can-see rule for moderators), [message-editing.md](message-editing.md) (`edited_at`, the events frame pattern), [membership-live.md](membership-live.md) (`conch.channels_changed.v1`, the content-free signal), [channel-topic.md](channel-topic.md) (the shape of this note; a topic is the one-line answer, a pin is the long-form one), [community-model.md](community-model.md) §5 (what a non-member may never learn), [admin-roles.md](admin-roles.md) and [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md) (`manages`, `manage_channels`, owners), [ADR-010](../adr/ADR-010-delegated-administration.md) decision 7 (re-check in the store), [agent-manifest.md](agent-manifest.md) §4 and §6 (channel permissions and how the vocabulary grows), [ai-administrators.md](ai-administrators.md) (why there is no admin-door row), [browser-image-attachments.md](browser-image-attachments.md) (the authorized file routes), [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (REST first; MCP never exceeds it), [ADR-000](../adr/ADR-000-charter.md) D10 (agents hold only what a manifest grants).
- **Owner:** protocol-designer (this note and the shapes); server-engineer (§11.1), web-engineer (§11.2), server-engineer again for the MCP slice (§11.3), cli-engineer (§11.4).
- **Current-code baseline:** origin/main `93453e8b`, inspected 2026-11-07. No pin feature, route, table, frame or client control exists in `pkg/schema`, `internal`, `web/src` or `docs/design`. The newest envelope is `conch.message.v2` (`MessageV2`: `id, channel_id, author_id, created_at, body, payload, audience, thread_root_id, guest_author, edited_at, removed`). Attachments are the payload `conch.file_attachment.v1 {file_ids}`; files are served by `GET /v2/files/{id}` and `/meta` to the uploader and to whoever can see the attached message, `humansOnlyFiles`, and have no expiry. The one visibility condition is `messageVisible` in `internal/server/store/audience.go`, the only file that may name `messages` in SQL (`TestOnlyAudienceGoReadsMessageRows`, `TestMessageReadsStayInTheVisibilityFunction`); the reactions added `reactionMemberCond` (the reader is a live, enabled member now) and `notRemovedCond`, composed as `reactionVisibleCond` and `reactionMessageCond`, and the single-message probe `visibleMessageTx`; `messageColumns` projects a removed message as its tombstone in the statement; the history watermark is `channel_members.history_after`. Standing for moderators is `memberActorStandingTx` (`store/members.go`). Message routes are `accessAuthenticated` and gate on `channelForCaller`; moderation is `accessChannelManager` plus the member-who-can-see rule. The events socket is per channel (`?channel=`), and a reference (`eventRef`) is turned into a document by each subscriber's goroutine with that subscriber's reader in `sendRef`; `notifyReactions` goes through `notifyChannel` to the sockets bound to the channel, humans and agents. Per-principal human buckets are `inviteLimit`/`bucketSet` (`reactionLimiter`, `messageEditLimiter`). Migrations run to 46; the next is 47.

A channel's important things, a D&D handout, a game-night operation plan, the house rules, an AI agent's summary of last week, live in the timeline and sink. A member who wants them scrolls or searches. This note lets a message be pinned so the channel has a short list of what matters, says who may pin, and makes sure that a pin on a whisper, a net message or a message from before someone joined is never a way for anyone to see what they could not see already.

## 1. Decisions in one page

| Question | Decision |
| --- | --- |
| What a pin is | A reference to one immutable message id in its channel, `message_pins (message_id PK, channel_id, pinned_by, pinned_at)`. Not a copy; no body, title, preview or file id is ever stored on the pin (§2.1). |
| Resources | No second object. A "channel resource" is a pinned message that carries a link or a file attachment; the panel filters, nothing new is stored (§2.2). |
| Order and bound | Newest pin first, by `pinned_at` then id; no manual reorder in this slice. At most `MaxPinsPerChannel = 50` per channel (§2.3). |
| Routes | `PUT /v2/channels/{channel}/messages/{message_id}/pin` (200 `PinMessageResponseV1`), `DELETE …/pin` (204), `GET /v2/channels/{channel}/pins` (200 `ListPinsResponseV1`); class `accessAuthenticated`, gate `channelForCaller`, as the reaction and edit routes are (§3). |
| Idempotency | Pinning a pinned message is 200 with the pin as it is, nothing written, nothing audited; unpinning an unpinned one is 204, likewise (§3.2). |
| Who may pin and unpin | The message's author, a member holding `manages` in the channel, an operator holding `manage_channels`, or an owner, **each only as a current member who can see the message** (moderation §7.1's rule). Members otherwise: 403 `pin_not_permitted`. Agents: only with the new channel permission `pin` under `messages.post` (§4). No new role, no admin-door row. |
| Visibility | A pin is visible exactly when its message is, to this reader, now: every read, the limit check's answer, the live signal and the panel go through `messageVisible` plus the member-and-enabled clause with the caller's reader, in the statement (§5). A pin on a message the caller cannot see is `message_not_found`, byte-identical to an unknown id; in the list it is absent. Nothing counts, ids or previews what the caller may not read. |
| Removed message | The removal transaction deletes the pin and sends the signal; a tombstone never occupies a slot (§6.1). |
| Edited message | The pin stands; the list shows the current body with `edited_at` (§6.2). |
| Archived channel | Pin refused 409 `channel_archived`; unpin and list allowed (§6.3). |
| Attachments | Rendered from `GET /v2/files/{id}` with the viewer's own credential, as in the feed; a 404 is the existing "Attachment unavailable" fallback. No public URL, no stored preview (§6.4). |
| Live | `conch.pins_changed.v1`, wire form exactly `{"schema":"conch.pins_changed.v1"}`, on the channel's events sockets, **only to a subscriber who can see the message at send time**, coalesced; the client re-reads `GET …/pins` (§7). |
| MCP | `list_pins` (`messages.read`), `pin_message` and `unpin_message` (`messages.post` and `pin`): plain mirrors of the three routes, which already accept an agent credential under the same grant (§8). |
| Audit | `message_pinned`, `message_unpinned`, subject `message:<id>`, detail `channel=<id>` (`reason=removed` when the removal did it); only on change (§4.5). |
| Limits | Humans: a keyed bucket, 10 pin writes a minute per principal; agents spend `messages.post`. The 51st pin is 409 `too_many_pins` (§4.6). |
| Auth off | Reads as any channel read; writes 400 `pins_require_auth` (§4.7). |
| Browser | A Pins panel in the centre, the search panel's pattern, on every width; Pin and Unpin in the message menu; a small "Pinned" marker in the feed; a "Files and links" filter (§9.1). |
| TUI and CLI | `conch pins list|add|remove`; a TUI pins view as a follow-up (§9.2). |
| Version | Additive; no bump (§10). Migration 47 adds one table (§10.3). |
| Split | Four issues: schema+server first, then web, MCP (approval-path) and CLI in parallel (§11). |

## 2. The pin

### 2.1 A reference, not a copy

**Recommendation: a pin is a row `(message_id, channel_id, pinned_by, pinned_at)` referencing the immutable message id.**

A copy (title, excerpt, file ids) would be a second place the message's content lives, with its own visibility rule, its own edit and removal paths, and a way for a moderated body or a revoked whisper to survive in "pin metadata". A reference has none of that: whatever the message is now, under whatever visibility, is what the pin shows, through the one projection every other read uses. The issue's "no copy of sensitive content into public pin metadata" is met by construction: the row holds three ids and a time. `pinned_by` is a principal id, resolved by the client as an author id is (reactions §6.4); it is omitted on the wire when the pin was written with authentication off, as `ChannelBanV1.created_by` is.

### 2.2 Resources are pinned messages

**Recommendation: no separate resource object.**

The epic's "channel resources" (handouts, plans, rules, summaries) are the things members already post: text with links, or a message with a file attachment. A resource object (a titled link or a file registered to the channel) would need its own storage, visibility, moderation, editing, audit, panel and tests, for content a message already carries with all of those. The panel offers a "Files and links" view that filters the pin list client-side to messages with a `conch.file_attachment.v1` payload or a linkified URL in the body. If testers later want a title on a resource, that is the message's first line; if they want curation beyond fifty pins, that is a later design, not blocked by this one.

### 2.3 Order and bound

**Recommendation: newest pin first, by `pinned_at` descending then `message_id` descending; `MaxPinsPerChannel = 50`; no reorder.**

Fifty is what Discord and Stoat give a channel, enough for a campaign's handouts and a group's rules, and small enough that the list is one page with no cursor. Manual order would need positions, a reorder route and a concurrency rule for a list that the panel can sort by pin time or post time client-side; it is an additive follow-up (a `position` field) if a group asks. The bound is checked inside the pin's transaction against every pin in the channel, visible to the caller or not (§5.3 says what that reveals).

## 3. The routes

### 3.1 Contract

| Route | Class | Body | Answer |
| --- | --- | --- | --- |
| `PUT /v2/channels/{channel}/messages/{message_id}/pin` | `accessAuthenticated` | none | 200 `PinMessageResponseV1{pin}` |
| `DELETE /v2/channels/{channel}/messages/{message_id}/pin` | `accessAuthenticated` | none | 204 |
| `GET /v2/channels/{channel}/pins` | `accessAuthenticated` | — | 200 `ListPinsResponseV1{channel_id, pins}` |

Channel by name and message by id, beside `…/messages/{message_id}/body` and `…/reactions`, so the client that holds a message has what it needs to pin it. Each handler first runs the channel gate (`channelForCaller`: the channel exists and the caller is a current member, else 404 `channel_not_found`; an agent needs the manifest capability and channel permission of §4.3, `CapabilityMessagesPost` + `ChannelPermissionPin` for the writes with the read grant checked as `agentReactionReadGate` does, `CapabilityMessagesRead` + `ChannelPermissionRead` for the list, else 403 `forbidden`, audited), then the store does the rest in one IMMEDIATE transaction (`withImmediateTx`: the reader check, `channelNotArchivedTx` for the pin, the probe, standing, the write, the read-back, in the `reactionWrite` shape). An operator who is not a member gets `channel_not_found` like any non-member: the class is not the manager class, because an author who is a plain member must reach the route, and because an operator pins only what it can read as a member (§4.1).

```go
// pkg/schema/pin.go (sketch; the exact Go lands in §11.1 under the
// schema-change skill)

// PinV1 is one pinned message as the caller may see it. The message is
// the current projection for this reader: edited body, tombstone never
// (a removed message is unpinned), audience as posted.
type PinV1 struct {
    MessageID int64     `json:"message_id"`
    PinnedBy  int64     `json:"pinned_by,omitempty"` // 0 when written with auth off
    PinnedAt  Timestamp `json:"pinned_at"`
    Message   MessageV2 `json:"message"`
}

// ListPinsResponseV1 is the body of GET /v2/channels/{channel}/pins:
// every pin whose message the caller may see, newest pin first. Always
// an array. There is no count of pins the caller may not see.
type ListPinsResponseV1 struct {
    ChannelID int64   `json:"channel_id"`
    Pins      []PinV1 `json:"pins"`
}

// PinMessageResponseV1 is the body of a 200 from PUT …/pin.
type PinMessageResponseV1 struct {
    Pin PinV1 `json:"pin"`
}

// PinsChangedV1 is the events-socket frame: the caller's answer to
// GET …/pins for this socket's channel may have changed. It carries
// nothing else, by design (§7).
type PinsChangedV1 struct {
    Schema string `json:"schema"` // PinsChangedSchemaV1 = "conch.pins_changed.v1"
}

const MaxPinsPerChannel = 50
```

Why the message is embedded: the panel needs author, time, body, audience badge and attachments for up to fifty messages, and there is no single-message read route today. Embedding runs the one projection (`messageColumns`) once per row in the one statement, so the list cannot disagree with the feed about what a message says or whether the caller may see it. `message_id` is repeated beside `message.id` so a client can key the list without opening the envelope; `channel_id` is on the list, not on each pin.

### 3.2 Answers, in the order they are decided

| Case | PUT | DELETE | GET |
| --- | --- | --- | --- |
| Not a member (any role, operators included); channel unknown | 404 `channel_not_found` (the gate) | same | same |
| Agent without the grant of §4.3 | 403 `forbidden`, audited, no budget spent | same | same (`messages.read` and `read`) |
| Authentication off | 400 `pins_require_auth` | same | 200, the auth-off reader's list (§4.7) |
| `message_id` not a positive integer | 400 `invalid_request` | same | — |
| Message unknown, in another channel, invisible to the caller (audience, history bound), or removed | 404 `message_not_found`, byte-identical | 204, nothing written (§3.3) | absent from the list |
| Channel archived | 409 `channel_archived` | allowed | allowed |
| Caller is not the author and holds no standing (§4.1) | 403 `pin_not_permitted` | same | — |
| Caller's standing was revoked between the gate and the transaction | 403 `pin_not_permitted` (re-check, ADR-010 D7) | same | — |
| Already pinned / not pinned | 200, the pin as it is, nothing written, no audit | 204, nothing written, no audit | — |
| Channel holds `MaxPinsPerChannel` pins | 409 `too_many_pins`, body names the bound and no id | — | — |
| Human over the bucket (§4.6) | 429 `rate_limited`, `Retry-After` | same | — |
| Otherwise | 200, one `message_pinned` row, the signal (§7) | 204, one `message_unpinned` row, the signal | 200 |

Visibility is decided before standing, so a manager who cannot see a whisper gets the same `message_not_found` as for an unknown id, and a member who can see a message but may not pin it gets `pin_not_permitted`, which discloses nothing (they can see it). A pin on a tombstone is `message_not_found` because that is what every other write on a removed message answers as built (moderation §10.3: a reaction, an edit, revisions); the client never offers Pin on a tombstone.

### 3.3 Why DELETE of an invisible pin is 204

The `DELETE` answers 204 whether or not a row was removed, and it removes a row only when the caller may see the message and holds standing. For a caller who may not see the message, the answer is 204 with nothing done, the same as for an id that was never pinned: a 404 here would tell a prober which ids exist and are visible to someone, and a 403 would tell them the id is pinned. A manager who can see the message and holds standing removes the row; a plain member who can see it gets `pin_not_permitted`, which is safe for the reason above. The isolation rows (§13) pin this: B's `DELETE` on A's pinned whisper is 204, the row stays, no frame, no audit.

## 4. Who, and the audit

### 4.1 Standing

**Recommendation: author, channel manager (`manages`), operator holding `manage_channels`, owner; each as a current member who can see the message.**

The issue asks for author, manager, operator and owner from existing authority, and that is the set. The reasons for each:

- **The author** may pin their own message. The player posts their character sheet and pins it; the agent posts its summary and pins it (§4.3). Letting a member pin anyone's message would make pins a free-for-all in a fifty-slot list; letting nobody but managers pin would make the GM pin every handout for every player. Author-only for plain members is the middle, and a manager can unpin anything.
- **A channel manager** curates the channel (ADR-010 decision 3; the moderation design assigns per-channel content control there).
- **An operator holding `manage_channels`, and an owner,** have manager standing in every channel (ADR-014), but **only as a member who can see the message**: moderation §7.1's rule, which keeps an operator from pinning, by id, a message in a private channel it is not in, or a whisper it did not receive. The route class answers a non-member operator `channel_not_found`, and the store's standing check (`memberActorStandingTx`'s form) finds the message with `visibleMessageTx`'s rule, so there is no path to a pin on something the actor cannot read.
- **Unpin** takes the same set. A pinner who later loses standing (a manager demoted, an author who left and returned as a plain member of a message they did not write) cannot unpin; a manager can. No "the pinner may always unpin" clause: it would need a fourth standing rule for a case the manager already covers.
- **Members otherwise, guests (when they return), disabled or hidden principals:** `pin_not_permitted` or the gate's refusal. Guests are visitors (ADR-009); pinning is curation of a community's channel, so a guest never pins, as a guest never manages; what they *see* is §5.

Standing is checked in the handler for the early answer and again inside the store's IMMEDIATE transaction (ADR-010 decision 7), as every manager write does, so a grant revoked between the two is refused.

### 4.2 Not a permission, not an admin-door action

No new entry in ADR-014's catalog: pinning is a fixed right of the author and the manager (admin-roles §4.2 lists moderation and the manager's rights as fixed, by choice), and the catalog grows only for rights an owner would want to switch per role. If a server wants members unable to pin their own messages, that is a later additive permission with a default that reproduces this behaviour (ADR-014 decision 2's rule for a permission added later). No admin-door row either: the door targets `principal, channel, credential, hook, invite, report`, not a message, and `remove_message` was deferred from the door for exactly the reason that applies here, that the acting agent must be a member who can see the message (moderation §10.1 Q4). An agent pins through §4.3, as itself.

### 4.3 Agents: an explicit grant

**Recommendation: a new channel permission `pin`; an agent pins and unpins only where its manifest grants `pin`, holds `messages.post`, and holds `read`; it lists pins with `messages.read` and `read`.**

The issue's first sentence names AI summaries as pinned content, so agents must be able to pin, and ADR-000 D10 says they do nothing a manifest did not grant. `pin` follows `post_net` and `whisper`: a channel permission that refines what an agent may do to a channel under the `messages.post` capability (agent-manifest §4: capabilities say what kind, grants say where), added by the §6 procedure (constant, `Valid()`, `ChannelPermissions()`, `Vocabularies`, a new fixture `agent-manifest-v1-pin.json`, the §4 table). It is independent: `pin` does not imply `post`, and `post` does not imply `pin`; an agent that may post but not pin gets `forbidden`, audited, as for any missing grant. The read grant is required because every pin answer embeds the message (the reactions amendment of 2026-10-29, for the same reason). Within the grant, the agent is held to §4.1 as a member: it pins messages it can see, and as a non-author it has no standing unless `manages` is set on its membership row, which nothing today sets for an agent; so in practice an agent pins its own messages, which is the use case. The grant is per channel and off unless written, and `PUT /v0/principals/{id}/manifest` is the owner's and operators' route as today.

Why not reuse `post` for pinning: an agent granted `post` for conversation would silently gain curation of the pin list, and an operator reading a manifest could not tell. Why not a new capability `pins.manage`: the capability vocabulary is the MCP tool map's unit and the rate-limit unit; a pin is a write into the channel's shared state, which is what `messages.post` already budgets, and adding a capability for three tools would make every manifest that wants an agent to pin carry two new words instead of one.

### 4.4 Humans through the browser, the CLI and MCP alike

No surface may do what another cannot (ADR-001; CLAUDE.md rule 4). The three routes are the API; the browser (§9.1), the CLI (§9.2) and the MCP tools (§8) are clients of them with the same refusals.

### 4.5 The audit row

Two actions, written in the write's transaction **only when a row changed**: `message_pinned` and `message_unpinned`, actor `principal:<id>` (or what `auditActor` gives), subject `message:<id>`, detail `channel=<id>`. When the removal transaction unpins (§6.1) the row is `message_unpinned` with actor the moderator and detail `channel=<id> reason=removed`. Never a body, never a filename, never a free-text reason (moderation §3.3). No row for a no-op, a read, or a refusal beyond the existing `access_denied` and agent-denial rows. The rows appear in `GET /v1/audit` for holders of `read_audit_log` as every other does.

### 4.6 Limits

**Recommendation: `MaxPinsPerChannel = 50` (§2.3); for humans a keyed bucket of 10 pin or unpin writes a minute per principal; for agents the `messages.post` budget, spent at the gate.**

The bucket follows the edit limiter (`messageEditLimiter`, an `inviteLimit` of 10 a minute per principal in a `bucketSet`, wired in `server.go` beside `reactionLimiter`), tighter than reactions because pins are rarer than reactions and because the bound's refusal is the one place a pin answers with something about rows the caller cannot see (§5.3): ten attempts a minute bounds how fast anyone can probe it, as the rename limiter bounds the name oracle. A refused write (`message_not_found`, `pin_not_permitted`, `too_many_pins`) takes a token too; a `channel_not_found` before the gate passes takes none. The bucket is per principal, not per channel, so a member cannot widen it by spreading across channels.

### 4.7 Authentication off

Writes are 400 `pins_require_auth`: there is no actor to bind as `pinned_by` or to check as author or manager, which is the reason the reaction and edit routes refuse (`reactions_require_auth`, `message_edit_requires_auth`). The list read is allowed: it is a projection of the channel's messages under the same reader `GET …/messages` uses with authentication off, and refusing it would make the panel fail on a developer's local server for no gain in safety. A pin written before `--auth off` reads with `pinned_by` as stored.

### 4.8 Permission matrix

"Sees" means the pin is in the caller's list and the caller gets the frame; "pins" and "unpins" mean the write succeeds. Every cell assumes the caller passed the channel gate as a current member; a non-member of any role is `channel_not_found` on everything and is not in the table. Guest cells describe guests when they return (#1004).

| Caller | Channel-wide message | Net message | Whisper | Message before the caller's `history_after` | Archived channel | Removed message | Caller's access revoked afterwards |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Author (plain member) | sees, pins, unpins | sees, pins, unpins (a monitor or a member since taken off the net sees and pins what it received) | sees, pins, unpins | — (an author sees their own) | sees; pin `channel_archived`; unpins | `message_not_found`; the pin is already gone | nothing further: `channel_not_found`, socket closed |
| Plain member, not the author | sees; `pin_not_permitted` on both | sees if a recipient at post time, else absent and `message_not_found` / 204 | same as net | absent; `message_not_found` / 204 | sees; `channel_archived`; `pin_not_permitted` | `message_not_found` | same |
| Channel manager (`manages`) | sees, pins, unpins | sees, pins, unpins if a recipient; else absent, `message_not_found`, 204 nothing | same as net | absent; `message_not_found` / 204 | sees; pin `channel_archived`; unpins | `message_not_found`; removal already unpinned | same; standing re-checked in the transaction |
| Operator holding `manage_channels`, or owner, **as a member** | as a manager | as a manager (no exemption from the audience) | as a manager | as a manager | as a manager | as a manager | same |
| Operator without `manage_channels`, member without `manages` | as a plain member | as a plain member | as a plain member | as a plain member | as a plain member | as a plain member | same |
| Guest (active visit) | sees after the bound; `pin_not_permitted` | sees if a recipient and after the bound | same | absent | sees; `channel_archived`; `pin_not_permitted` | `message_not_found` | visit ended: refused at the door |
| Agent with `read` (and `messages.read`) | sees in `GET`/`list_pins` | sees if a recipient | same | absent | sees | — | `channel_not_found` |
| Agent with `messages.post`, `read`, `pin` | pins and unpins its own; `pin_not_permitted` on another's | same, if a recipient | same | — | pin `channel_archived`; unpins its own | `message_not_found` | grant removed: `forbidden`, audited |
| Agent without `pin` (any other grant) | `forbidden`, audited, no budget | same | same | same | same | same | — |
| Disabled principal, revoked or expired credential, ended guest | refused by authentication before any route | | | | | | |
| Authentication off | `GET` 200 with the auth-off reader's list; writes `pins_require_auth` | | | | | | |

## 5. Visibility: the pin is the message's

### 5.1 One predicate, in the statement

Every statement that reads or writes `message_pins` joins `messages m` under the reactions' condition with the caller's `Reader` (`readerFor(r, apiV2)`): `reactionMessageCond`, which is `messageVisible(true)` (the audience rule from the stored recipient rows, the principals list, the history bound `history_after`; a live, enabled membership for a scoped message) **and** `reactionMemberCond` (the reader is, now, a live member of `m.channel_id` and an enabled principal) **and** `notRemovedCond`. The membership clause is not redundant: `messageVisible` checks membership only for a scoped message, and a channel-wide pin would otherwise be readable by a reader removed from the channel between the gate and the statement, or by a subscriber whose signal reference was flushed after a removal committed and before the drop closed the socket (reactions §6.1 found both). The join lives in `audience.go`, as `TestOnlyAudienceGoReadsMessageRows` requires; `store/pins.go` holds only the statements over `message_pins` alone. The write's probe is `visibleMessageTx`'s form with `notRemovedCond` added; the author rule is `m.author_id = ?` in the same statement, as `ownMessageSQL` does for edits, so a non-author and an invisible message are told apart only after the row is found.

The predicate is evaluated **on every read**, never cached and never written down: a pin row carries no "visible to" list. So:

- a member taken off a net still sees the pin of a net message posted while they were on it (visibility is fixed at post time, ADR-005), and sees no pin of a later one;
- a member removed from the channel sees none of its pins and gets `channel_not_found` on every route, as for a channel that never existed;
- a guest (when guests return) sees pins of messages after their `history_after` and none before, and a pin on a reply whose root is before it follows the existing thread rule;
- a reader whose credential was revoked or whose principal was disabled is refused at the door, and an in-flight statement finds no row under the enabled clause;
- an operator who is not a member learns nothing by id: `channel_not_found` from the gate, before any pin statement runs.

### 5.2 The list, the panel and the marker

`GET …/pins` returns the pins whose messages the caller may see, and only those, with no count, cursor or placeholder for the rest. The browser's panel shows that list and nothing else; the feed's "Pinned" marker is derived from the ids in that list, so a message the viewer can see is marked pinned only if the viewer can see the pin, which is the same condition. Nothing on the page says "3 pins you cannot see".

### 5.3 What the bound reveals, and why it is accepted

When a channel holds fifty pins and a caller who can see thirty tries a fifty-first, the answer is `too_many_pins`. That tells them twenty pins exist on messages they cannot see: a count, not an id, a body, an author or an audience. It is of the kind community-model §5 already accepts (id gaps, #500; `other_channels` counts in admin-roles §7.6), bounded by the bucket (§4.6) and the limit being reachable only by a caller who already holds standing in the channel. The alternatives were worse: a per-visibility bound (fifty channel-wide plus unbounded scoped) would make the bound meaningless for the panel; silently dropping the oldest pin would delete someone else's pin without a trace; refusing to pin scoped messages at all would refuse the GM pinning a handout to their table. Recorded in the threat model (§12) and the isolation matrix (§13, row P7) as a documented exposure.

### 5.4 Scoped messages may be pinned

A whisper or a net message can be pinned by anyone in §4.1's set who can see it. The pin shows to exactly the message's audience, under §5.1, so it widens nothing. One rule ("a pin inherits its message's audience") is easier to hold, test and explain than two, and refusing scoped pins would need a different answer for a scoped message than for a channel-wide one, which is one more thing a reader could learn.

## 6. Lifecycle of the source

### 6.1 Removed by a moderator

**Recommendation: `store.RemoveMessage`'s transaction deletes the `message_pins` row for the message, writes `message_unpinned` with `reason=removed`, reports it on `MessageRemoval` (a new `Unpinned bool`), and `handleRemoveMessage` sends the pins signal after the commit beside `notifyMessageRemoved`.**

A tombstone in a pin list is noise that occupies one of fifty slots, and leaving it to a later unpin would leave invisible occupied slots. Removal is permanent (the `message_removals` triggers abort updates and deletes), so there is no restore that would want the pin back. The deletion is inside the removal's transaction so the two cannot disagree; a pin that commits after the removal is impossible, because the pin statement finds no visible non-removed row (`message_not_found`). Sockets that could see the message get `conch.message_removed.v1` as today and `conch.pins_changed.v1` from this; the browser drops the entry on either.

### 6.2 Edited by its author

Nothing changes on the pin. The list projects the current body with `edited_at`, as the feed does. The browser applies the `conch.message_edited.v1` frame it already receives on the channel's events socket to its pin list as well as the feed, so the panel follows an edit live with no new frame and no re-read.

### 6.3 Archived channel

**Recommendation: pin is refused 409 `channel_archived` (`channelNotArchivedTx` in the transaction); unpin and the list are allowed.**

The archive guard refuses what is new in the channel (a post, an edit, a reaction, an upload), and a pin changes what the channel presents to its members, which is content, not administration: the topic design allowed topic writes on an archived channel because a topic is a label managers administer, while a pin is a member's act on a message. Unpin is reduce-only and allowed, as member removal, bans and message removal are in an archived channel (moderation §10.1 Q1). Reads are reads. Restoring the channel leaves the pins as they were.

### 6.4 Attachments and files

**Recommendation: the panel renders a pinned message's attachments exactly as the feed does, through `ImageAttachment` against `GET /v2/files/{id}/meta` and `GET /v2/files/{id}` with the viewer's bearer token; nothing about a file is stored on the pin, and no file is ever public.**

The file routes already answer `file_not_found` to anyone who cannot see the attached message, uploader included once the message is removed (moderation §7.3), and `humansOnlyFiles` keeps agents off them. A 404 renders the existing "Attachment unavailable" fallback, which is what "avoid orphaned attachment previews" needs: there is no preview to orphan, because the pin never held one. Files have no expiry today (`FileV1` carries none); if one is added later, the same 404 path covers it, and this note needs no change. The CLI's `conch download` is the parity path for a pinned file.

### 6.5 The pinner leaves, is disabled, hidden or ended

The pin stays, as a message stays when its author leaves and a reaction stays when its reactor does (reactions §11.1). `pinned_by` keeps the id; the client renders it as it renders an author it cannot resolve (`hidden-<id>`'s style). A manager unpins it if it should go. Deleting pins when their pinner loses access would change what everyone sees for a reason unrelated to the message.

### 6.6 Channel visibility and membership changes

A channel made private, a member added or removed, a net roster edit: none touches a pin row. The next read computes with the new state (§5.1), the removed member's sockets are dropped (`dropPrincipal`) so no signal reaches them, and the newly added member's first list shows what they may see, which for a net or whisper pinned before they arrived is nothing.

## 7. Live: `conch.pins_changed.v1`

**Recommendation: a content-free frame, `{"schema":"conch.pins_changed.v1"}`, sent on the channel's events sockets to each subscriber who can see the pinned or unpinned message at send time, coalesced per socket; the client re-reads `GET …/pins`.**

Content-free, as `conch.channels_changed.v1` is (membership-live §1): a frame with no content cannot leak a body, an id or an author, through a race or otherwise. Per-subscriber gating, as a reaction frame is (reactions §6.3): the handler calls `notifyPinsChanged(channelID, messageID)`, which goes through `notifyChannel` to the sockets bound to that channel (humans and agents, as `notifyReactions` does) with an `eventRef{kind: eventPins, id, channelID}`, and in `sendRef` each subscriber's goroutine runs the §5.1 probe with its own reader (`ListVisibleMessages(ctx, ref.channelID, reader, ref.id-1, 1)` and the member check, the idiom the edited and removed frames use) before writing the frame; a subscriber who cannot see the message gets nothing, so a member outside a whisper does not even learn that a pin changed. For the removal case (§6.1) the message is already removed when the reference is flushed, so the probe is the one the `conch.message_removed.v1` frame uses: the subscriber could see the message, tombstone included. The events socket is bound to one channel (`?channel=`), so the frame needs no channel id; if a channel-less events socket is built later (membership-live §5.6), an optional `channel_id` is an additive field. Coalescing is trailing-edge on the roster floor (`eventsRosterFloor`), as `channels_changed`: a burst of pins gives one frame per socket, and a late frame is only redundant. Agents' events sockets receive it under the `messages.read` budget spent at upgrade, as every events frame is.

The `/v2/ws` message socket, `/v1/ws`, `/v0/ws`, `Server.Broadcaster`, notifications and read state are untouched: a pin is not a message, notifies no one, and moves no read mark.

Reconnect: after the events socket reopens, the browser re-reads the list once, as it re-reads the channel list (membership-live §5.5), so a frame missed while down is caught up. Channel switch: the list is read for the newly open channel (single-flight, the `useChannelRefresh` pattern), which also seeds the feed marker.

## 8. MCP

**Recommendation: three tools, `list_pins`, `pin_message`, `unpin_message`, each a plain mirror of its route; no tool does anything the route does not.**

| Tool | Capability (`mcpToolCapabilities`) | Input | Output | Route |
| --- | --- | --- | --- | --- |
| `list_pins` | `messages.read` | `channel` (name) | `{channel_id, pins: [{message_id, pinned_by, pinned_at, message: mcpMessageOutput}]}` | `GET /v2/channels/{channel}/pins` |
| `pin_message` | `messages.post` | `channel`, `message_id` | `{pin}` as above | `PUT …/pin` |
| `unpin_message` | `messages.post` | `channel`, `message_id` | `{}` | `DELETE …/pin` |

The channel permission `pin` (and `read`) is checked by the same handler code the REST route runs for an agent credential; the tool adds nothing (ADR-001 D6: parity holds because the REST route exists first and an agent with its own token gets the same answer there). Refusals are the route's, as `mcpToolError` renders them. The embedded message is projected through `mcpMessageOutput`, which `mcp_projection_test.go` keeps complete against `MessageV2`. No tool is added for a human-only act, and none for the admin door (§4.2). ADR-001's minimum set is a floor, not a ceiling; the reaction tools set the precedent for mirrors of per-message routes.

## 9. Clients

### 9.1 Browser

**Where.** A pin button in the conversation header (`conv-head`) beside Search, `id="channel-pins-open"`, label "Pinned messages". It opens `PinsPanel`, rendered the way `SearchPanel` is: in the centre, `key={channelId}`, with the conversation body hidden underneath, on every width, so the phone layout (the chat view's full width) needs nothing new and no third column appears. A members-style overlay was considered and rejected: a pinned handout is read, not glanced at, and a 22rem overlay would truncate it; the centre already has the message renderer.

**What it shows.** The list from `GET …/pins`, newest pin first, each entry drawn with the exported `Message` component (same author, time, audience badge, `Linkified` body, `ImageAttachment`, reactions read-only), under a one-line head "Pinned by <name> · <time>", with two controls: **Open context** (the search panel's `openContext(id)` view, loading the page around the id, "Back to pins"), and **Unpin**, shown only when the viewer may unpin (§4.1, computed as `useMayModerate` computes `here`, or the viewer is the author), with an inline confirm in the `ModerationConfirm` pattern. A segmented filter at the top, **All** and **Files and links**, filters client-side (§2.2). Empty state: "Nothing is pinned. Pin a message from its menu." A tombstone never appears (§6.1); a message that fails to render is skipped, never shown as a placeholder.

**The menu.** `Message`'s items gain **Pin** when the viewer may pin and the message is not pinned, and **Unpin** when it is pinned and the viewer may unpin; neither on a tombstone, and neither when the channel is archived except Unpin. The item calls `api.pinMessage(channel, id)` or `api.unpinMessage`; refusals map to one line each (`pin_not_permitted`: "You can't pin this message"; `too_many_pins`: "This channel has 50 pins. Unpin one first."; `channel_archived`: "This channel is archived"; `message_not_found`: "This message is gone"), network failure keeps the menu state. No optimistic insert: the list is re-read on the frame, and the response's `pin` is applied at once for the actor's own tab.

**The marker.** A small "Pinned" label on a feed message whose id is in the held pin list, text and an icon, no colour alone. Derived state, never stored.

**State and frames.** `frames.ts` gains `{kind: 'pinsChanged'}` for `pinsChangedV1Schema`; `useAgents` gets `onPinsChanged`; a single-flight `refreshPins(channelId)` in the `useChannelRefresh` pattern replaces the held list (never merges), runs on channel open, on the frame, on reconnect, and discards an answer whose channel is no longer open. `messageEdited` and `messageRemoved` frames update or drop the matching pin entry. `state/pins.ts` holds `{channelId, pins, loading, failed}` per channel. A 404 that is **not** a `schema.Error` on the first `GET …/pins` (an older server: the mux's text 404) hides the header button and the menu items; a `schema.Error` never does (reactions §12's rule).

**Accessibility and motion.** The panel is `role="region"` `aria-label="Pinned messages"`; its head has a Close button; Escape closes it and focus returns to the header button, as Search does. Entries are a list (`role="list"`), controls are buttons with visible labels, targets at least 44px on coarse pointers. No animation is added (the centre swap has none), so reduced motion needs no rule. Colours use the existing tokens; the epic's high-contrast preset is #1497's, and the panel inherits it by using tokens only. No `dangerouslySetInnerHTML`, no inline style, no `any`.

### 9.2 TUI and CLI

**CLI.** `conch pins list <channel>` prints one line per pin, `pinned_at message_id author_id body` through `safeLine`, newest first; `--json` prints the response document. `conch pins add <channel> <message-id>` and `conch pins remove <channel> <message-id>` send the PUT and the DELETE and print the server's one line on refusal, exit nonzero. In the `pins` group, not `admin`, because an author who is a plain member may pin. `conch download` fetches a pinned file as any other.

**TUI.** A pins view entered from the messages pane, listing the channel's pins with the same message block the history uses and Enter to centre the feed on the message (`centreOn`); Esc back. The TUI has no secondary panels, only modes (`modeDirectory`, `modeCommunity`), so this is a mode; Ctrl+P is taken by the community view, so the cli-engineer picks the key. The TUI does not pin or unpin in this slice; the CLI verbs do. Both are the follow-up issue §11.4; the API side is complete without them (ADR-001).

## 10. Published-shape changes for the owner's sign-off

All additive: new types, one new vocabulary value, new error codes, one new constant, one new events frame; no published field touched; every existing fixture byte-identical (`scripts/schema-compat.sh`).

| Change | Where |
| --- | --- |
| `MaxPinsPerChannel = 50` | `pkg/schema/pin.go`; `ts.go` `TSConstants` as `MAX_PINS_PER_CHANNEL` |
| `PinV1 {message_id, pinned_by omitempty, pinned_at, message: MessageV2}` with `Validate` (ids positive, `pinned_at` set, message valid and not removed) | `pin.go`; fixtures `pin-v1.json`, `pin-v1-whisper.json`, `pin-v1-attachment.json`, `pin-v1-no-pinner.json`; `ts.go` `TSTypes` |
| `ListPinsResponseV1 {channel_id, pins}`; `pins` never null, strictly descending by `pinned_at` then `message_id` | `pin.go`; fixtures `list-pins-response-v1.json`, `list-pins-response-v1-empty.json`; `ts.go` |
| `PinMessageResponseV1 {pin}` | `pin.go`; fixture `pin-message-response-v1.json`; `ts.go` |
| `PinsChangedSchemaV1 = "conch.pins_changed.v1"`, `PinsChangedV1 {schema}` with `Validate` | `pin.go`; fixture `pins-changed-v1.json`; `ts.go` family `pins-changed-v1` |
| Error codes `pin_not_permitted` (403), `too_many_pins` (409), `pins_require_auth` (400) | `pin.go`; fixture `error-too-many-pins.json`; `vocabularies.go` if error codes are listed there |
| `ChannelPermissionPin ChannelPermission = "pin"` | `agent_manifest.go` (`Valid()`, `ChannelPermissions()`), `vocabularies.go`; fixture `agent-manifest-v1-pin.json`; agent-manifest.md §4 row |
| `mcpToolCapabilities`: `list_pins → messages.read`, `pin_message → messages.post`, `unpin_message → messages.post` | `agent_manifest.go`, in the MCP PR (§11.3), with the tools |
| Audit actions `message_pinned`, `message_unpinned` | `internal/server/store` (not a wire shape; listed so the vocabulary stays complete) |

Not changed: `MessageV2` (no `pinned` field: the client derives the marker from the list, and the hot read path does not join pins), `ListMessagesResponseV2`, `ChannelV0`, `ChannelDocV1`, any socket's first frame, `AdminAction`, `Permission`, `SocketCloseReason`.

### 10.1 Compatibility

- **Old browser or TUI against a new server:** sees no pins and loses nothing. Message routes and sockets are unchanged. The events socket sends a `schema` it does not know, and both clients skip unknown schemas (`frames.ts` returns null; `dispatchEvent` skips).
- **New client against an old server:** `GET …/pins` is the mux's text 404, not a `schema.Error`; the client hides the controls (§9.1). Over MCP the three tools are absent from `tools/list`, and an agent does not call a tool it was not offered.
- **A manifest with `pin` read by an old server:** `Valid()` rejects the unknown value, so a `PUT` of such a manifest to an old server is 400; a stored manifest is never read by an older binary than wrote it (§10.3).

### 10.2 Rollback

The migration rule applies unchanged: a binary from before migration 47 refuses a database at or past it ("newer than this binary"), so rolling the binary back means restoring the pre-upgrade backup, as for every migration, and the release notes say so. No runtime switch in this slice; a `server_settings` row `pins_enabled` is an additive follow-up if an operator asks.

### 10.3 Migration 47

```sql
CREATE TABLE message_pins (
  message_id INTEGER PRIMARY KEY REFERENCES messages(id),
  channel_id INTEGER NOT NULL REFERENCES channels(id),
  pinned_by  INTEGER REFERENCES principals(id),   -- NULL under auth off
  pinned_at  INTEGER NOT NULL
);
CREATE INDEX message_pins_channel ON message_pins (channel_id, pinned_at DESC, message_id DESC);
```

One table, one index, no backfill, no audit row; a pre-pin database opens and runs on the new binary at once. The `PRIMARY KEY` on `message_id` is the "one pin per message" rule and the idempotency anchor (`INSERT … ON CONFLICT DO NOTHING`, then read). `channel_id` is denormalized so the limit count and the list index need no join before the visibility join. No `position` column (§2.3). The issue asks that no migration be approved automatically: this one is in §12 for Nick.

## 11. The split

Four issues, not filed here. The first ships schema and server together (the skill's "sync consumers in the same PR" rule); the other three are blocked by its merge and disjoint by area, so they dispatch in parallel. Each references #1494 and #1495 and this note. 11.1 gets a `security-reviewer` pass (new authenticated write surface, new visibility statements, new isolation rows). 11.3 edits `internal/server/mcp.go`, which is on `process-checks.yml`'s approval-path list, so it carries the `approval-path` label and Nick merges it. 11.1 touches none of the listed paths (`pkg/schema/agent_manifest.go`, `store/audience.go`, `message_removal.go` are not on the list) and is not approval-path; the three pinned audit trails must still not move.

### 11.1 Schema and server: pin shapes, `message_pins`, the three routes, visibility, events, removal hook, isolation rows

Labels: `area/schema`, `area/server`, `security`.

**Context.** docs/design/pinned-messages.md (this note) is the design; §10 lists the shapes (sign-off recorded on #1494 before dispatch), §3 the routes and their answers in order, §4 standing, audit, limits and auth-off, §5 the visibility statement (two conditions, in `audience.go`), §6 removal, edit, archive, §7 the frame, §13 the test matrix. The reaction routes (`handlePutReaction`, `store/reactions.go`, the `audience.go` helpers that join `message_reactions` to `messages`, `notifyReactions`, `e2e/isolation/reactionrows.go`) are the pattern to copy; the message-removal transaction (`store.RemoveMessage`) gains the pin deletion. Agents reach the routes with their credential under `messages.post` + `pin` (+ `read`); the MCP tools are 11.3. `ChannelPermission` gains `pin` here, with its fixture and the agent-manifest.md §4 row, so the REST authorization exists before any tool (ADR-001).

**Files.**
- `pkg/schema/pin.go` (new: §10 types, constant, error codes, `Validate`s), `pkg/schema/pin_test.go`, `pkg/schema/agent_manifest.go` (`ChannelPermissionPin`), `pkg/schema/vocabularies.go`, `pkg/schema/ts.go` (`TSTypes` rows, `TSConstants`), `pkg/schema/testdata/` (the fixtures of §10), regenerated `web/src/schema/*.ts` (`go generate ./pkg/schema`, never by hand), `golden_test.go`, `ts_test.go`, `agent_manifest_test.go`; `docs/design/agent-manifest.md` §4 (the `pin` row).
- `internal/server/store/store.go` (migration 47, §10.3), `internal/server/store/pins.go` (new: `PinMessage(ctx, actor, callerID, channelID, messageID, now) (Pin, bool, error)`, `UnpinMessage(...) (bool, error)`, `ListPins(ctx, reader, channelID) ([]Pin, error)`, audit constants; statements over `message_pins` alone), `internal/server/store/audience.go` (the helpers that join pins to `messages` under `messageVisible` + member-and-enabled: the probe, the list projection through `messageColumns`, the limit count; the pin deletion in `RemoveMessage`'s transaction), `internal/server/store/pins_test.go`, `internal/server/store/message_removal_test.go` (the cascade).
- `internal/server/pins.go` (new: the three handlers, `channelForCaller` gate, the agent grant check `pin` via the manifest helpers, the human bucket), `internal/server/routes.go` (three `rt(...)` rows, `accessAuthenticated`), `internal/server/events_ws.go` (`notifyPinsChanged(channelID, messageID)` as an `eventRef`; the per-subscriber probe; the trailing-edge coalescing; the header comment), `internal/server/message_removal.go` (call `notifyPinsChanged` after a removal that unpinned), `internal/server/pins_test.go`, `internal/server/events_ws_pins_test.go`.
- `e2e/isolation/pinrows.go` (new, beside `reactionrows.go`: `pinMatrixRows` added to `matrixRows()`, `pinAgentRows` added to `agentRows()`, `pinHistoryRows` with `pPreRoot`; the rows of §13 marked "isolation"), `e2e/isolation/routes.go` (coverage), `e2e/isolation/world.go` (`activityA` pins A's root, whisper and net message so B's rows have targets).

**Acceptance criteria.**
- [ ] `scripts/schema-compat.sh` passes with every pre-existing fixture byte-identical; the new fixtures round-trip; `scripts/schema-ts-check.sh` passes; `vocabularies_test` passes with `pin`.
- [ ] `PinV1.Validate` refuses a removed message, a zero `message_id`, an unset `pinned_at`; `ListPinsResponseV1.Validate` refuses null `pins`, a non-descending order and a duplicate id.
- [ ] Author (plain member), manager, operator holding `manage_channels` as a member, owner as a member: `PUT …/pin` is 200 with the pin and the current message; a repeat is 200, no row, no audit; `DELETE` is 204, then 204 again with nothing written; `GET …/pins` lists newest pin first with `pinned_by` set.
- [ ] A plain member who is not the author: 403 `pin_not_permitted`; a non-member and a non-member operator: 404 `channel_not_found` before any pin statement; an agent without `pin` (or without `read`): 403 `forbidden`, audited, no budget spent; an agent with `messages.post`, `read` and `pin` pins its own channel-wide message and is `pin_not_permitted` on another's.
- [ ] A whisper's non-recipient, a non-net member, a reader whose history bound is after the message, and an unknown id: `PUT` is 404 `message_not_found`, byte-identical across the four; `DELETE` is 204 with the row intact; the pin is absent from `GET`.
- [ ] The 51st pin in a channel is 409 `too_many_pins` with no id in the body; the count includes pins on messages the caller cannot see; an unpin frees the slot.
- [ ] Human writes beyond 10 a minute are 429 `rate_limited` with `Retry-After`; refused writes take a token; `channel_not_found` does not.
- [ ] Archived channel: `PUT` 409 `channel_archived`; `DELETE` 204 and the row goes; `GET` 200.
- [ ] A message removed by a moderator loses its pin in the same transaction with a `message_unpinned reason=removed` row; a `PUT …/pin` on the tombstone is `message_not_found`; the slot is free.
- [ ] An edit leaves the pin; `GET …/pins` shows the new body and `edited_at`.
- [ ] Auth off: `PUT` and `DELETE` 400 `pins_require_auth`; `GET` 200; a pin stored with `pinned_by` NULL encodes without the key.
- [ ] After a pin or unpin, every events socket of the channel whose reader can see the message gets exactly one `conch.pins_changed.v1`, bytes exactly `{"schema":"conch.pins_changed.v1"}`, within the floor; a non-recipient member's socket, another channel's sockets, `/v2/ws`, `/v1/ws`, `/v0/ws` and `Server.Broadcaster` get nothing; three changes within the floor give one frame per socket; a reference flushed to a just-removed member's socket after the removal committed sends nothing.
- [ ] Audit rows exactly as §4.5: `message_pinned`, `message_unpinned`, subject `message:<id>`, detail `channel=<id>` and `channel=<id> reason=removed`; none for no-ops, reads or refusals beyond `access_denied`; no body text anywhere in the audit.
- [ ] 50 concurrent `PUT`s of one message: one row, identical answers, one audit row; concurrent `PUT` and `DELETE`: the last commit wins and both answers were true at their time; 60 concurrent `PUT`s of distinct messages into an empty channel: exactly 50 rows, 10 `too_many_pins`.
- [ ] Migration 46→47 on a populated database opens and serves; a 46 binary refuses a 47 database with the existing "newer than this binary" error.
- [ ] `go run ./e2e/isolation` passes with the new rows; nothing in the pinned audit trails of `e2e/p2-hardening`, `e2e/p3-local` and `e2e/dogfood` moves.

**Required tests.**
- `pkg/schema`: golden tests for every new fixture; table-driven `Validate` tests; `ts_test` for the constant and families; `agent_manifest_test` for `pin` in `Valid()`, `ChannelPermissions()` and the fixture.
- `internal/server/store/pins_test.go`: transaction, idempotency, standing re-check refused, author rule, limit under concurrency (`-race`), archived refusal and unpin, audit rows, auth-off NULL pinner, migration test; `message_removal_test.go`: the cascade and its audit row.
- `internal/server/pins_test.go`: the handler table above (every row of §3.2), the bucket, the agent grant, the byte-identical 404s; `internal/server/events_ws_pins_test.go`: the recipients, the bytes, the coalescing, the untouched sockets, the flush-after-removal race.
- `e2e/isolation`: rows P1–P9 of §13; `routes.go` coverage.
- `make check`; `go test -race ./...`; the three pinned-trail runs.

**Blocked by.** Nick's sign-off on §10 and §10.3 of docs/design/pinned-messages.md, recorded on #1494. Nothing else.

### 11.2 Web: the Pins panel, the menu items, the marker

Labels: `area/web`.

**Context.** pinned-messages.md §9.1 is the contract; §5.2 says the page shows only the caller's list and derives the marker from it; §10.1 says how an old server is detected. The generated types from 11.1 give `PinV1`, `ListPinsResponseV1`, `PinMessageResponseV1`, `PinsChangedV1` and `MAX_PINS_PER_CHANNEL`. `SearchPanel.tsx` (centre placement, `openContext`, result rendering with the exported `Message`), `MessageMenu.tsx` and the item list in `Conversation.tsx` `Message`, `useMayModerate` in `Moderation.tsx`, `useChannelRefresh.ts` (single-flight) and `frames.ts` are the patterns. No `dangerouslySetInnerHTML`, no inline style, no `any`; wire types only from `web/src/schema/`.

**Files.** `web/src/api.ts` (`listPins`, `pinMessage`, `unpinMessage`; the non-`schema.Error` 404 detection), `web/src/agents/frames.ts` (`pinsChanged`), `web/src/agents/useAgents.ts` (`onPinsChanged`), `web/src/state/pins.ts` (new reducer), `web/src/usePins.ts` (new: single-flight refresh on open, frame, reconnect; applies `messageEdited`/`messageRemoved`), `web/src/PinsPanel.tsx` (new), `web/src/Conversation.tsx` (header button, menu items, marker; `openContext` factored to be shared with Search), `web/src/App.tsx` (`pinsOpen`, wiring), `web/src/app.css` (tokens only); tests beside each; `web/e2e/52-pins.spec.ts`.

**Acceptance criteria.**
- [ ] The header shows "Pinned messages"; opening it shows the channel's pins newest first with author, time, body, audience badge and attachments rendered by the same components as the feed; Escape closes it and focus returns to the button; the conversation body is hidden while it is open and restored after.
- [ ] A manager sees Pin on another's message and Unpin on a pinned one; the author sees Pin on their own; a plain member sees neither on another's message; nobody sees either on a tombstone; in an archived channel only Unpin shows.
- [ ] Pinning from the menu shows the entry in the panel and the "Pinned" marker in the feed without a reload; a second tab on the channel updates on the frame; a tab on a whisper's non-recipient shows nothing and gets no frame; reload shows the same.
- [ ] `pin_not_permitted`, `too_many_pins`, `channel_archived`, `message_not_found` and a network failure each show their own line; the menu never sends twice.
- [ ] A removed pinned message leaves the panel on the `messageRemoved` frame; an edited one shows the new body on the `messageEdited` frame.
- [ ] "Files and links" shows only pins with an attachment payload or a linkified URL; an attachment the viewer cannot fetch shows the existing "Attachment unavailable" fallback and no broken image.
- [ ] Against a server whose `GET …/pins` is a non-`schema.Error` 404, the button and the items are absent and nothing errors; a `schema.Error` 404 shows its line and hides nothing.
- [ ] At 390×844 the panel fills the chat view, controls are at least 44px, the bottom nav still works; `make web-check` passes; the generated schema files are untouched by hand.

**Required tests.** Unit: `state/pins` reducer (replace, drop on removed, update on edited, reset on channel change); `usePins` single-flight and discard-on-channel-change; `PinsPanel` with a whisper pin (badge), an attachment pin (fallback on 404), the filter, empty state, Unpin confirm, each refusal line; `frames.ts` parses `pinsChanged` and still returns null for an unknown schema; the menu item visibility table (author, manager, operator, member, tombstone, archived); the marker. Playwright `52-pins.spec.ts` with two contexts: a manager pins, a member's tab shows the panel entry and marker within 2 s; a whisper pinned by its sender is absent from a non-recipient's panel; unpin; the author pins their own; a plain member has no Pin on another's; phone viewport.

**Blocked by.** 11.1 (merged).

### 11.3 MCP: `list_pins`, `pin_message`, `unpin_message`

Labels: `area/server`, `area/schema`, `approval-path`.

**Context.** pinned-messages.md §8. Parity (ADR-001, CLAUDE.md rule 4): the routes exist and already accept an agent credential under `messages.post` + `pin` (+ `read`) from 11.1; the tools are mirrors. Register through `addAgentTool`; the input and output types sit beside `mcpReactionInput`; the embedded message goes through `mcpMessageOutput`. `internal/server/mcp.go` is on the approval-path list, so this PR carries the label and Nick merges it; the approval flow itself does not change, and the chain tests must still pass.

**Files.** `internal/server/mcp.go` (three registrations and descriptions: a pin is visible only to the message's audience; an agent pins what it can see and, as a non-manager, only its own), `internal/server/pins.go` or `mcp_pins.go` (the tool bodies calling the handler core), `pkg/schema/agent_manifest.go` (`mcpToolCapabilities` three rows), `agent_manifest_test.go`, `internal/server/mcp_test.go`, `mcp_projection_test.go`, `mcp_audience_test.go`.

**Acceptance criteria.**
- [ ] `tools/list` shows the three tools to an agent holding `messages.read`/`messages.post` and not otherwise; `MCPToolCapability` maps them as §8.
- [ ] `pin_message` by an agent with `pin`, `read` and `messages.post` on its own channel-wide message answers the pin; on another's message `pin_not_permitted`; without `pin` `forbidden`, audited, no budget; on a whisper it received, the pin answers and `list_pins` from a non-recipient agent omits it.
- [ ] `list_pins` returns exactly what `GET …/pins` returns for the same agent, message projection included; `unpin_message` answers `{}` and the row is gone; every refusal code equals the REST route's for the same input (a parity table).
- [ ] The approval chain tests and `e2e/dogfood` are unchanged and green; `make check` passes.

**Required tests.** `mcp_test.go`: the table above over both transports; `mcp_projection_test.go` stays complete; `mcp_audience_test.go`: a scoped pin over MCP; the full request → notify → resolve → audit chain test runs green (rule 3, because the file is on the list).

**Blocked by.** 11.1 (merged).

### 11.4 CLI and TUI: `conch pins`, a pins view

Labels: `area/cli`.

**Context.** pinned-messages.md §9.2. The `nets`/`channels` groups (`internal/cli/command.go`) are the pattern for a plural group with `list|add|remove`; `thread.go` `messageLine` is the line format; the TUI's modes (`modeDirectory`, `modeCommunity`) are the pattern for a view, and `centreOn` jumps the history. Ctrl+P is taken.

**Files.** `internal/cli/pins.go` (new), `internal/cli/command.go` (the group), `internal/cli/client_pins.go` (the three calls), `internal/cli/tui/model.go`, `view.go`, `pins.go` (new mode), tests beside each.

**Acceptance criteria.**
- [ ] `conch pins list ops` prints one `safeLine` per pin, newest pin first, `pinned_at message_id author_id body`; `--json` prints the response document; an unknown channel exits nonzero with one line.
- [ ] `conch pins add ops 42` and `conch pins remove ops 42` send the routes and print the server's line on refusal, exit nonzero; `remove` of an unpinned id exits 0.
- [ ] The TUI pins view lists the channel's pins with the history's message block, Enter centres the feed on the message, Esc returns; a whisper pin the viewer cannot see is absent; the view re-reads on `conch.pins_changed.v1`; a key not already bound opens it, documented in `help`.

**Required tests.** Table-driven tests for the verbs against a fake server (list, add, remove, each refusal, `--json`); TUI view tests for list, jump, empty and hostile text through `sanitize`; `make check`.

**Blocked by.** 11.1 (merged).

## 12. Threat model, and what Nick signs off

### 12.1 Threats and answers

| # | Threat | Answer |
| --- | --- | --- |
| T1 | Enumerate hidden messages by pinning ids | `PUT` on an invisible, other-channel or unknown id is one `message_not_found`, byte-identical; `DELETE` is 204 for all three; the list omits; the bucket bounds attempts (§3.2, §3.3, §4.6) |
| T2 | Learn that hidden pins exist through the bound | Accepted, bounded: a count only, to a caller with standing, under the bucket (§5.3) |
| T3 | Learn of a scoped pin through the live frame | The frame is content-free and sent only to subscribers who can see the message at send time (§7) |
| T4 | Widen a message's audience through the panel, the marker or a cache | Every read is the §5.1 statement with the caller's reader; the client holds only its own list and replaces it on every signal; nothing is stored about who may see a pin (§5) |
| T5 | Reach a file through a pin | Files are fetched by the existing authorized routes with the viewer's token; nothing is stored or published about a file on the pin (§6.4) |
| T6 | An agent pins without being told it may | `pin` is an explicit, per-channel, off-by-default manifest grant under `messages.post`; no admin-door row; no implicit standing (§4.3) |
| T7 | Grief a channel by filling the pins | Author-only for plain members, 50 per channel, 10 writes a minute, managers unpin, every change audited with the actor (§4) |
| T8 | A former recipient, former member, disabled or revoked principal keeps seeing pins | The predicate is evaluated on every read with the current state and the member-and-enabled clause; sockets are dropped; nothing is cached server-side (§5.1, §6.6) |
| T9 | A removed message lingers as a pinned tombstone or an "unavailable" preview | Unpinned in the removal transaction; no preview exists (§6.1, §6.4) |
| T10 | An operator outside a private channel pins or lists by id | `channel_not_found` from the gate, before any pin statement (§3.1, §4.1) |
| T11 | Old clients misread a new frame or new codes | Unknown events schemas are skipped by every client; new error codes are additive on `schema.Error`; the old-server 404 rule hides controls (§10.1) |
| T12 | Audit or logs carry content | Detail is ids only; the rows exist only on change (§4.5) |

### 12.2 For Nick

These are the only items this note leaves to him; everything else is decided above with its reason.

1. **The §10 shapes**, additive under the `schema-change` skill (no version bump), as the issue's acceptance criterion asks ("accepted by the owner before implementation").
2. **Migration 47** (§10.3), since the issue says no migration is approved automatically.
3. **The `pin` channel permission** (§4.3): a vocabulary addition under ADR-000 D10, and the policy that agents may pin at all, with an explicit grant and no implicit standing.
4. **The accepted bounded exposure** of §5.3 (the limit's count), recorded here and in the isolation matrix.
5. **The approval-path merge** of 11.3, which edits `internal/server/mcp.go`.

Nothing here conflicts with a locked decision in ADR-000: single tenancy, manifest-and-capability agent identity, API parity and the deployment invariant stand; `conchd` needs no new process, dependency or service.

## 13. Test matrix

Normative for 11.1 unless a row names another issue. "Isolation" means a row in `e2e/isolation` with the three checks: the stated refusal, no marker of A in the body, and an answer byte-identical to the same request against a ghost (an unknown channel or an unknown message id, as the row says).

| # | Scenario | Expected | Where |
| --- | --- | --- | --- |
| S1 | Every §10 fixture; `PinV1` with a removed message; null `pins`; out-of-order list | round-trip; refused; refused; refused | schema |
| W1 | Author pins, pins again, unpins, unpins again | 200 / 200 same pin no row / 204 / 204 nothing; two audit rows | store + handler |
| W2 | 50 concurrent `PUT`s of one message by two sessions of one manager | one row, identical answers, one audit row | store, `-race` |
| W3 | 60 concurrent `PUT`s of distinct messages into an empty channel | exactly 50 rows; 10 `too_many_pins`; no id in any refusal body | store, `-race` |
| W4 | `PUT` racing `DELETE` of the same message | last commit wins; both answers true at their time; frames converge | store |
| W5 | Human exceeds 10 writes a minute; refused writes count; `channel_not_found` does not | 429 with `Retry-After`; tokens as §4.6 | handler |
| W6 | Standing revoked (manager demoted, operator loses `manage_channels`) between the gate and the transaction | 403 `pin_not_permitted`; nothing written | store, with the test hook |
| P1 | Member of another group, and an operator who is not a member, on A's channel: `PUT`, `DELETE`, `GET` | 404 `channel_not_found`; ghost channel | isolation |
| P2 | Member of A's channel names A's message id through B's channel | `PUT` 404 `message_not_found`; `DELETE` 204, row intact; absent from B's list; ghost id | isolation |
| P3 | A's whisper, pinned by A: recipient and non-recipient member of the same channel | recipient lists it and gets the frame; non-recipient: absent, `PUT` 404 `message_not_found`, `DELETE` 204 row intact, no frame | isolation + events |
| P4 | A's net message pinned: net member, a member taken off the net since the post, a monitor, a non-net member | first three see the pin (visibility fixed at post time); non-net member: absent, 404, no frame | isolation |
| P5 | Guest whose history bound is after A's pinned message; a reply pinned whose root is before the bound (when guests return) | absent; absent | guest rows |
| P6 | Manager who cannot see A's whisper tries to pin and unpin it by id | 404 `message_not_found`, 204 nothing; ghost id | isolation |
| P7 | Channel at 50 pins, 20 on whispers B cannot see; B (a manager) pins a 51st | 409 `too_many_pins` with no id (the §5.3 documented exposure) | isolation, documented row |
| P8 | Agent with `read` only, `post` only, `post`+`read` without `pin`: `PUT`, `DELETE`, `GET` | `forbidden` audited for the writes without `pin`; `GET` needs `read`; no budget on refusal | agent rows |
| P9 | Agent with `messages.post`, `read`, `pin`: own channel-wide message; another's; a whisper it received; a whisper it did not | 200; `pin_not_permitted`; 200 (and a non-recipient agent's list omits); `message_not_found` | agent rows |
| L1 | Removal of a pinned message (channel-wide; whisper) | row gone in the same transaction; `message_unpinned reason=removed`; `pins_changed` and `message_removed` to those who could see; nothing to others; `PUT` on the tombstone `message_not_found` | store + events |
| L2 | Edit of a pinned message | pin unchanged; `GET` shows new body and `edited_at`; no `pins_changed` frame | store + handler |
| L3 | Archived channel: pin, unpin, list, then restore | 409 `channel_archived`; 204; 200; pins as before | handler |
| L4 | Pinner leaves, is disabled, hidden; pinned message's author leaves | pin stays; `pinned_by` unchanged; list still answers for members who can see | store |
| L5 | Member removed from the channel after pins exist | `channel_not_found` on all three; socket closed; no frame | handler + events |
| L6 | Pinned message with an attachment: a viewer who can see it, one who cannot, after removal | file routes answer as today (200 / `file_not_found` / `file_not_found` for the uploader too); the panel shows the fallback (11.2) | files tests + web |
| E1 | Frame recipients: a subscriber who can see the message, one who cannot, an agent subscriber with `read`, the actor's other socket, a disabled subscriber not yet closed | frame / nothing / frame / frame / nothing; bytes exactly the wire form | events |
| E2 | Three pins within the floor; a slow consumer | one frame per socket; closed at `eventsQueueMax`, re-read restores | events |
| E3 | No frame on `/v2/ws`, `/v1/ws`, `/v0/ws`; no `Broadcaster` call; no read-state change; no notification | asserted with the recording seams | handler |
| E4 | Reconnect after restart | client re-reads; panel equals server list (11.2 Playwright, 11.4 TUI) | web, TUI |
| A1 | Auth off: writes, reads, a pin stored with NULL pinner | 400 `pins_require_auth`; 200; `pinned_by` key absent | handler + store |
| C1 | Published fixtures byte-identical; TS generated and checked | `schema-compat.sh`, `schema-ts-check.sh` green | CI |
| C2 | Old browser build receives `conch.pins_changed.v1` | ignored, no error (11.2 test with the frame injected) | web |
| C3 | New client against a pre-pin server; a `schema.Error` 404 from a current server | controls hidden on the text 404; shown and the line reported on the `schema.Error`; tools absent from `tools/list` | web, MCP |
| C4 | Migration 46→47 on a populated database; 47 opened by a 46 binary | opens and serves; refused "newer than this binary" | store |
| AU1 | Audit for W1, L1; none for no-ops, reads, refusals; `access_denied` for agents | as §4.5; no body text anywhere | store |
| M1 | Every REST row above over MCP (11.3) | same codes and documents; parity table | MCP tests |
| D1 | The three pinned audit trails (`e2e/p2-hardening`, `e2e/p3-local`, `e2e/dogfood`) | unchanged | CI |

## 14. Choices made

| Option | Why not |
| --- | --- |
| A pinned copy (title, excerpt, file ids) | A second home for content with its own visibility, moderation and edit paths; the exact thing "no copy into pin metadata" forbids (§2.1). |
| A `pinned` field on `MessageV2` | Joins pins on the hot read path for every message in every list, frame and replay, to save the client a 50-row read it needs anyway for the panel (§10). |
| A separate resources object | Everything a pinned message already has, built again (§2.2). |
| Manual order | Positions, a reorder route and a concurrency rule for a list of fifty the client can sort (§2.3). |
| Anyone pins anything | Fifty slots become a free-for-all; managers would spend their time unpinning (§4.1). |
| Only managers pin | The GM pins every player's sheet; the agent cannot pin its own summary (§4.1). |
| A `pin_messages` permission in ADR-014's catalog | Pinning is a fixed right of author and manager, like reacting and moderating; a per-role switch is additive later if a server wants it (§4.2). |
| Agents pin through the admin door | The door has no message target and the acting agent must see the message, which is why `remove_message` was deferred too (§4.2). |
| A new capability `pins.manage` | A pin is a write into the channel that `messages.post` already budgets; one new word in the manifest, not two (§4.3). |
| Tombstones stay pinned | Noise in a fifty-slot list, an invisible occupied slot, and a preview to orphan (§6.1). |
| A frame with the message id | Would name a whisper's id to a non-recipient unless gated anyway; content-free plus per-subscriber gating costs one probe and leaks nothing (§7). |
| A frame to every subscriber of the channel, ungated | Tells a non-recipient that a pin changed when their list did not (§7). |
| Pins in a right-hand overlay | Truncates a handout; the centre already has the renderer and works on phones (§9.1). |
| `DELETE` of an invisible pin is 404 | A prober would learn which ids are visible to someone (§3.3). |

## 15. Follow-ups, not in these issues

- Manual pin order (`position`, a reorder route), if a group asks.
- A per-role `pin_messages` permission (ADR-014's rule for a later permission: default on for members, reproducing this design).
- A "pinned" system line in the history when a message is pinned, which is a message kind and the message design's to grow.
- A `pins_enabled` server setting, if an operator wants pins off without a rollback.
- A channel-less events socket's optional `channel_id` on `conch.pins_changed.v1`.
- TUI pin and unpin controls, once the TUI has any moderation controls.
