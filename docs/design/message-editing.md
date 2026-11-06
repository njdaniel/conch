# Design: edit your own recent text

- **Status:** Proposed for principal and security review, issue [#845](https://github.com/njdaniel/conch/issues/845). The policy decisions in §1 were made by the principal under Nick's delegation on 2026-10-30; this note turns them into contracts. No server or web behaviour is authorized by this note alone. The wire slice in §11 is in this PR, is additive only, and needs no version bump.
- **Builds on:** [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (REST first; MCP and clients front the same core), [ADR-005](../adr/ADR-005-nets-and-whispers.md) and `store/audience.go` (the one visibility condition), [ADR-009](../adr/ADR-009-guests-and-open-joining.md) and [guests-and-accounts.md](guests-and-accounts.md) (history bound, immutable guest attribution), [message-reactions.md](message-reactions.md) (the nearest precedent: a write on an existing message, its refusals, its live frame and its isolation rows).
- **Coordinates with:** [moderation.md](moderation.md) (#698, closed; implementation M1 #849 and M4 #852, both open). Moderation **removes** someone's message; this note lets an author **change** their own. Neither duplicates the other; §10 says how they compose.
- **Current-code baseline:** origin/main `53aa569`, inspected 2026-10-30. Messages are insert-only: no `UPDATE messages` or `DELETE FROM messages` exists anywhere in `internal/server`, and migrations 15 and 22 say so in their comments. The last migration is the reaction table (37, `store/store.go`).

## 1. Decisions in one page

| Question | Answer |
| --- | --- |
| Who may edit | Only the message's author, only a person (an agent gets `403 human_required`), only while a current member who can see the message. |
| What may be edited | The text (`body`) of a message with no payload, or whose payload is a file attachment (`conch.file_attachment.v1`), whose attachments then stay exactly as they were. Every other payload (approval requests, typed signals, unknown payloads) is `409 message_not_editable`. |
| How long | One hour from `created_at` (`schema.MessageEditWindowSeconds`, the server's clock), then `409 message_edit_window_closed`. At most ten edits per message (`schema.MaxMessageEdits`), then `409 message_edit_limit_reached`. |
| Route | `PUT /v2/channels/{channel}/messages/{message_id}/body` with `EditMessageRequestV1 {body}`, answering `200 EditMessageResponseV1 {message}`. |
| History | Every version is kept, append-only, in `message_revisions`. The author reads them with `GET /v2/channels/{channel}/messages/{message_id}/revisions`. Everyone else sees only `edited_at` on the message: the "(edited)" marker and its time. |
| Audit | One `message_edited` row per edit that changed the text: subject `message:<id>`, detail `channel=<id> revision=<n>`. Never any text. |
| Search | The full-text index holds the current text only. An update trigger replaces the old entry; old words no longer find the message. |
| Live | `/v2/ws` sends the edited `MessageV2` again (same id, with `edited_at`) to the same recipients the post went to. The events socket sends a new `conch.message_edited.v1` frame to human members who can read it. Reload shows the same state. |
| Unchanged by an edit | Id, channel, author, `created_at`, audience, net and whisper scope, recipients, thread links, guest attribution, the payload and so attachment ids and bytes, approval payloads, reactions, read marks and unread counts. |
| Notifications and mentions | An edit notifies no one. An `@name` added by an edit does not notify; one removed does not retract a notification already shown. |
| MCP | No tool. Agents cannot edit, so a tool would only ever refuse. `read_channel` output gains the optional `edited_at`, so agents can tell an edited message. |
| CLI | `conch edit <channel> <message-id> <text>` is cheap and is proposed as a follow-up (§14.3), not part of #845's server or web PRs. |
| Version bump | None. `MessageV2.edited_at` is a new optional field; the other shapes are new types (§11). |

## 2. What exists now

| Area | Where | What editing uses or must change |
| --- | --- | --- |
| Message rows | `messages` (migration 1 and later `ALTER`s: `payload_schema`, `payload_json`, `audience_kind`, `net_id`, `thread_root_id`, generated `fts_channel`) | Gains `edited_at`. Everything but `body` and `edited_at` becomes write-protected by a trigger (§8). |
| One visibility condition | `store/audience.go` (`messageVisible`, `visibilityArgs`, the history-bound helpers) | The edit and revision statements embed it, plus the explicit current-member-and-enabled clause the reaction statements use. `TestOnlyAudienceGoReadsMessageRows` (`store/audience_test.go:493`) is textual: any statement naming `FROM messages` or `JOIN messages` lives in `audience.go`. The new statements live there too, and the test's forbidden list gains `message_revisions`. |
| Search | `messages_fts`, external-content FTS5 over `(body, fts_channel)`, kept in step by the insert trigger `messages_fts_insert` (migration 22); `SearchVisibleMessages` in `audience.go` | Needs an update trigger. An external-content table holds no text, so the old entry must be deleted with the **old** values (`'delete'` command) before the new one is inserted (§8). |
| Post bounds | `messages.go`: `decodeJSONBody` (`MaxBytesReader` at `schema.MaxPostRequestBytes`, unknown fields refused), `strings.TrimSpace(body) == ""` → `400 invalid_request` | The edit route uses the same two bounds and the same answers. There is no separate character limit on a message; the frame bound `MaxMessageFrameBytes` is derived from the request bound, so an edited message fits any socket a post fits. |
| Message socket | `ws.go` `streamWSV2`; `hub.BroadcastMessageV2(msg, recipients)`; the replay filter `msg.ID <= replayed` | The edit frame goes through the same broadcast and the same `gateFrame`. The replay filter must not drop it (§7.1). |
| Events socket | `events_ws.go`: `eventMessage` sends a `MessageV2` to human members other than the author, and `useNotifications.ts` notifies on it (and matches `@name` client-side) | Edits get their own `eventRef` kind and their own frame schema, so no client mistakes an edit for a new message (§7.2). |
| Clients | Web `state/messages.ts` and the TUI keep messages in a map by id; `conch tail` (`command.go:527`) streams `/v2/ws` | Re-sending a `MessageV2` with the same id is an upsert for the web and the TUI today. |
| Agent runtime | `internal/adapter` polls `read_channel` with an `after` cursor | An edit keeps its id, so a polling agent is never re-triggered by it. |
| Human write limits | `reactions.go` `reactionLimiter` over `bucketSet` | The edit bucket copies it (§9). |
| Isolation harness | `e2e/isolation/matrix.go`, `reactionrows.go`, the `routes.go` coverage check | Both new routes need rows (§12). |

## 3. Policy

### 3.1 Who

The author, as the store binds it: `messages.author_id` equals the caller's principal. No request names a principal. The caller must be:

- authenticated (with `--auth off` there is no author to match: `400 message_edit_requires_auth`);
- a person (`kind = human`); an agent is refused with `403 human_required` **before** the channel is looked up, so the answer is the same for every channel and message, real or not;
- a current, enabled member of the channel, as for any channel route (`404 channel_not_found` otherwise, identical to an unknown channel);
- able to see the message now, under the same audience and history-bound rules a list read applies.

Owners, operators and channel managers get no exemption: none of them may edit another person's message. Correcting someone else's content is moderation's remove operation, not an edit.

A guest (where guests are enabled) is a person and may edit their own message during the visit. The stored guest attribution is immutable (`message_guest_attribution_no_update`) and is not touched. A converted guest keeps the principal id and so keeps the right to edit within the window.

### 3.2 Window

`now − created_at ≤ MessageEditWindowSeconds` (3600 s), on the server's clock, both in milliseconds as stored. Exactly one hour is still inside. The window is a protocol constant in `pkg/schema` and exported to TypeScript, but only so the page can stop offering the action; the server alone decides.

### 3.3 Edit limit

A message may be edited `MaxMessageEdits` (10) times, so holds at most 11 revisions. With the one-hour window and the rate limit this bounds the stored text to eleven request-sized bodies per message, and keeps the revision list unpaginated. `MaxMessageEdits` is also the bound `ListMessageRevisionsResponseV1` validates against; a later lower write limit must leave the published constant alone (the `MaxReactionKindsOnWire` precedent).

### 3.4 What counts as a text message

| The message | Editable? |
| --- | --- |
| No payload | Yes. |
| Payload `conch.file_attachment.v1` | Yes, the body only. The payload (and so `file_ids`), the `files.message_id` links and the bytes on disk are never touched. |
| Any other payload: `conch.approval.v1`, `leviathan.trade_signal.v1`, a hook's payload, an unknown schema | No: `409 message_not_editable`. The rendered body of a typed message is the human form of its payload (ADR-000 D8); changing one without the other would make them disagree. |
| A reply, or a thread root | Yes. `thread_root_id` is unchanged, and so is every reply's link to the root. |
| A whisper or a net message | Yes, with its audience and recipients unchanged. On a net, the author must still hold a **member** seat (not monitor, not removed from the net): the reaction write rule (`audience.go`'s net seat check). Otherwise `409 message_not_editable`. |
| A message moderation removed (once M4 lands) | No: `409 message_not_editable`. |

### 3.5 Unchanged body

A request whose body is byte-identical to the current text changes nothing: `200` with the message as it is, no revision, no audit row, no frame. This check comes before the window and limit checks, so a retried `PUT` whose first attempt succeeded but whose answer was lost still succeeds after the window closes. It reveals nothing: only the author gets this far.

## 4. Edit route

### 4.1 Contract

```
PUT /v2/channels/{channel}/messages/{message_id}/body
Content-Type: application/json

{"body": "hello, conch (fixed a typo)"}
```

```
200 OK
{"message": {"schema": "conch.message.v2", "id": 42, "channel_id": 7, "author_id": 3,
             "created_at": "2026-07-13T12:34:56.789Z", "body": "hello, conch (fixed a typo)",
             "edited_at": "2026-07-13T12:40:02.105Z"}}
```

`PUT` on the `body` sub-resource, because the request replaces that one field and nothing else, and because every other single-field change in the API is a `PUT` of a sub-resource (`PUT /v1/channels/{id}/name`, `PUT /v1/principals/{id}/name`). There is no `PATCH` route anywhere, and the web operation table has no `PATCH` method. Access class `accessAuthenticated`; the handler applies the rest.

The request is `EditMessageRequestV1`. It is decoded with unknown fields refused, so a request that names `payload`, `audience`, `thread_root_id` or `file_ids` is `400 invalid_request`, not a silent narrowing. The response is `EditMessageResponseV1`: the message as the caller now reads it, the same projection a list read returns.

### 4.2 Order of checks

The order is part of the security design. Nothing before step 7 reads the message.

1. Authentication off → `400 message_edit_requires_auth`.
2. Caller is not a person → `403 human_required`. Before any lookup.
3. `channelForCaller`: unknown channel or not a member → `404 channel_not_found`.
4. Take one token of the caller's edit bucket, whatever follows. Empty bucket → `429 rate_limited` with `retry_after_seconds`. Nothing refunds a token, so probing ids costs what editing costs.
5. `message_id` not a positive integer → `400 invalid_request`.
6. Body: over `MaxPostRequestBytes`, not one JSON object, unknown fields, or empty / white space only → `400 invalid_request`, with the post route's messages.
7. The store, in one immediate transaction:
   1. re-check current, enabled membership (`ErrNotChannelMember` → `404 channel_not_found`);
   2. select the message by `id`, `channel_id`, `author_id = caller` and the visibility condition for the caller → no row: `404 message_not_found`. Unknown id, another channel's id, someone else's message, a whisper the caller is not in, a net message they cannot read, and a message before their history bound are **one** answer;
   3. removed, payload other than a file attachment, or a net message without a member seat → `409 message_not_editable`;
   4. body equal to the current body → commit nothing, answer `200` (§3.5);
   5. outside the window → `409 message_edit_window_closed`;
   6. already edited `MaxMessageEdits` times → `409 message_edit_limit_reached`;
   7. write: revisions, the row, the index (by trigger), the audit row (§8).
8. After commit: broadcast (§7). Answer `200`.

### 4.3 Refusals

| Status | Code | When | New? |
| --- | --- | --- | --- |
| 400 | `message_edit_requires_auth` | conchd runs with `--auth off` | New |
| 400 | `invalid_request` | bad id; body empty, white space, too large, not JSON, unknown field | Existing (post's answers) |
| 403 | `human_required` | caller is an agent | Existing |
| 404 | `channel_not_found` | unknown channel, non-member, operator outside the channel | Existing |
| 404 | `message_not_found` | unknown id, other channel, not the author, cannot see it | Existing (reactions) |
| 409 | `message_not_editable` | typed payload, removed, net seat lost | New |
| 409 | `message_edit_window_closed` | more than an hour old | New |
| 409 | `message_edit_limit_reached` | edited ten times already | New |
| 429 | `rate_limited` | edit bucket empty | Existing |

`message_not_found` for a non-author who **can** see the message is deliberate. A separate "not yours" answer would be harmless for a channel-wide message but would need its own branch for scoped ones, and one answer for every message the caller may not edit, seen or not, cannot leak which ids are scoped away. The `409`s are reachable only by the author, who can already read the message.

## 5. Revision route

```
GET /v2/channels/{channel}/messages/{message_id}/revisions
200 OK
{"channel_id": 7, "message_id": 42, "revisions": [
  {"revision": 1, "body": "helo, conch",                 "created_at": "2026-07-13T12:34:56.789Z"},
  {"revision": 2, "body": "hello, conch",                "created_at": "2026-07-13T12:38:11.000Z"},
  {"revision": 3, "body": "hello, conch (fixed a typo)", "created_at": "2026-07-13T12:40:02.105Z"}]}
```

- Revision 1 is the text as posted, at the message's `created_at`; revision *n*+1 is the text the *n*th edit set, at that edit's `edited_at`. The last is the current body. Oldest first, no pagination (at most 11).
- A message never edited answers its one revision, from the message row. This is not a second way to read a message: the author can already read it.
- The same gates as the edit, steps 1–3 and 5, then the same author-and-visibility select: an agent `403 human_required`; a non-member `404 channel_not_found`; anyone but the author, or an author who can no longer see it, `404 message_not_found`. A removed message (M4) answers `404 message_not_found` too: removal suppresses the text on every read surface, earlier versions included.
- No window: the author may read their history after the hour. No token is spent (a read, bounded, of one's own text).
- Operators and owners get no exemption; the audit row is their record (§8.3).

## 6. Read surfaces

### 6.1 History, threads and search (v2)

Every v2 read of the message (`GET /v2/channels/{channel}/messages`, thread reads, `GET …/messages/search`, the post-reply root lookup) returns the current body and `edited_at`. Nothing else changes: position, id, author, audience, guest attribution, payload.

### 6.2 Search

The index holds the current text only (§8.2). A search for a word only in an earlier version finds nothing; a word only in the new text finds the message. Search ordering and `next_before` are by message id, which an edit does not change, so a page boundary never moves because of an edit.

### 6.3 v0 and v1

`MessageV0` and `MessageV1` have no `edited_at` and get none: v0 and v1 history returns the current body, unmarked. No first-party client reads v0 or v1 any more (the web, the TUI and `conch tail` all use v2); adding the field there is possible later as an additive change, but is not worth widening two legacy envelopes for now. v0 and v1 sockets get no edit frame (§7.1).

### 6.4 MCP

`read_channel` returns `ListMessagesResponseV2`, so its output gains the optional `edited_at` with no code change; the generated output schema follows the Go type. `post_message`'s output is unchanged in practice (a new message has no `edited_at`). No tool is added and no capability changes in `mcpToolCapabilities`.

## 7. Live delivery

### 7.1 Message socket (`/v2/ws`)

After commit the server broadcasts the edited `MessageV2`, the same projection the response carries, through `hub.BroadcastMessageV2` with the recipient list the post had: none for a channel-wide message, the stored `message_recipients` for a scoped one. So the edit reaches exactly the subscribers the original reached, and each frame still passes the socket's `gateFrame` (history bound), so a guest whose bound is after the message gets nothing.

- v0 and v1 subscriptions get no frame, and the configured `Server.Broadcaster` seam is **not** called: it carries `MessageV0` and its consumers would take a second copy as a new post.
- The replay filter in `streamWSV2` drops a buffered frame whose id the replay already sent. It must drop only **originals**: `if replay != nil && msg.ID <= replayed && msg.EditedAt == nil`. An edit frame for an earlier id passes, through the gate as usual. A duplicate (the replay already read the edited text) is harmless under the client rule below.
- **The client rule** (web, TUI, and the contract for third-party socket readers): a copy of a message replaces the one held only if its `edited_at` is the same or later, absent counting as earliest. `edited_at` strictly increases per message (§8.1), so this is exact. It covers a history page read before an edit but answered after the frame, two edit frames delivered out of order, and replay overlap. Once M4 lands, a removed copy is never replaced by one that is not removed.
- A reader that acts on new messages (a bot) must treat a frame with `edited_at` as an update of a message already sent, not a new one. The first-party agent runtime polls `read_channel` by id cursor and never sees an edit as new.

### 7.2 Events socket (`/v1/events/ws`)

A new `eventRef` kind, `eventMessageEdited` (`id` the message, `channelID` its channel), is pushed to every **human** subscriber that holds a membership of the channel, the author's own other pages included; agents are not sent it, as they are not sent `eventMessage`. References for one message coalesce in the queue. When the subscriber's goroutine sends, it reads the message with its own reader at that moment, as `eventMessage` does (a page of one after `id − 1`), and sends

```json
{"schema": "conch.message_edited.v1", "message": { …MessageV2 with edited_at… }}
```

or nothing, if the reader may not see it or is no longer a member. It never calls `writeReadState`: an edit is not a new message and changes no count.

It is a frame of its own, not a second `MessageV2`, because on this socket a `MessageV2` means "new message" and the web notifies on it, `@name` matching included. Older clients skip the unknown schema (`parseEventFrame` returns null; `cli.dispatchEvent` ignores it). A client holding the message (a notification's preview, an open view) updates it in place under the client rule and does not notify.

### 7.3 What an edit does not do

No notification (browser or ntfy), no `@name` processing on the server or the client, no read-mark move, no unread change, no `Broadcaster` call, no agent trigger, no change to reactions or their frames. Added mentions do not notify; removed ones do not retract a shown notification. Rationale: an edit that pinged would be a way to re-notify a channel at will, around the post rate limit, and a "silent" correction is what people expect.

## 8. Storage

### 8.1 Migration

The next free number at merge time. On this baseline that is **38** (37 is the reaction table). M1 (#849), #981 and #964/#988 may also take numbers; whichever merges later renumbers, as the reaction migration did.

```sql
-- 38: Message edits (issue #845, docs/design/message-editing.md §8). The
-- first migration under which a messages row changes: body and edited_at
-- only, and only by EditOwnMessage. A trigger refuses an UPDATE that names any
-- other column. message_revisions keeps every version of an edited message's
-- text, append-only: revision 1 is the text as posted, written by the first
-- edit, so a message never edited has no rows. messages_fts is external
-- content: its entry is replaced by deleting it with the old values and
-- inserting the new, so old words stop matching. Nothing is backfilled.
ALTER TABLE messages ADD COLUMN edited_at INTEGER;

CREATE TABLE message_revisions (
	message_id INTEGER NOT NULL REFERENCES messages (id),
	revision   INTEGER NOT NULL CHECK (revision >= 1),
	body       TEXT    NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (message_id, revision)
);

CREATE TRIGGER message_revisions_no_update BEFORE UPDATE ON message_revisions
BEGIN SELECT RAISE(ABORT, 'message revisions are append-only'); END;
CREATE TRIGGER message_revisions_no_delete BEFORE DELETE ON message_revisions
BEGIN SELECT RAISE(ABORT, 'message revisions are append-only'); END;

CREATE TRIGGER messages_only_body_changes
BEFORE UPDATE OF id, channel_id, author_id, created_at, payload_schema, payload_json,
                 audience_kind, net_id, thread_root_id ON messages
BEGIN SELECT RAISE(ABORT, 'only a message body and edited_at may change'); END;

CREATE TRIGGER messages_edited_at_forward BEFORE UPDATE OF edited_at ON messages
WHEN NEW.edited_at IS NULL OR NEW.edited_at <= COALESCE(OLD.edited_at, OLD.created_at)
BEGIN SELECT RAISE(ABORT, 'edited_at must move forward'); END;

CREATE TRIGGER messages_fts_update AFTER UPDATE OF body ON messages
BEGIN
	INSERT INTO messages_fts (messages_fts, rowid, body, fts_channel)
		VALUES ('delete', OLD.id, OLD.body, OLD.fts_channel);
	INSERT INTO messages_fts (rowid, body, fts_channel)
		VALUES (NEW.id, NEW.body, NEW.fts_channel);
END;
```

Notes for the implementer:

- `message_revisions` is an ordinary rowid table, not `WITHOUT ROWID`: a body may be up to a request in size, and `WITHOUT ROWID` suits small rows.
- The `'delete'` command must be given exactly the values that were indexed, or the external-content index is silently corrupted. The trigger has them in `OLD`; nothing else may update `messages.body`. `fts_channel` is a virtual generated column and is available in `OLD`/`NEW`, as the insert trigger already relies on. A test runs FTS5's `integrity-check` after edits.
- `edited_at` is `max(now_ms, COALESCE(edited_at, created_at) + 1)`, so it is strictly after `created_at` (which `MessageV2.Validate` requires) and strictly increasing per message, even for two edits in one millisecond or a clock that stepped back. The `messages_edited_at_forward` trigger holds the store to it.
- Migration 15's and 22's comments ("messages are never updated") stay as history; this migration's comment supersedes them.

### 8.2 The write

`EditOwnMessage` lives in `audience.go` (it selects from `messages`). In the immediate transaction of §4.2 step 7, after the checks:

1. `n` = the highest revision, or 0. If `n = 0`, insert revision 1 with the current body and `created_at`, and set `n = 1`.
2. Refuse if `n − 1 ≥ MaxMessageEdits`.
3. Insert revision `n + 1` with the new body at `edited_at`.
4. `UPDATE messages SET body = ?, edited_at = ? WHERE id = ?` (the FTS trigger runs).
5. Append the audit row (§8.3).
6. Read back the recipients (`message_recipients`, scoped messages only) for the broadcast, and the message through the normal projection.

The revision read (`OwnMessageRevisions`) is in `audience.go` too, under the same author-and-visibility select. `TestOnlyAudienceGoReadsMessageRows` gains `FROM message_revisions` and `JOIN message_revisions` in its forbidden list: earlier text is as sensitive as the message.

### 8.3 Audit

| Action | Actor | Subject | Detail |
| --- | --- | --- | --- |
| `message_edited` | `principal:<id>` | `message:<id>` | `channel=<id> revision=<n>` |

Written in the edit's transaction, only when the text changed. The revision is the one the edit created (2 for the first edit). No text, no length, no diff. Refusals and no-ops write nothing. The rows appear in `GET /v1/audit` (owner-only today) with no new filter. Volume is bounded by the rate limit and `MaxMessageEdits`.

### 8.4 Backups and retention

Revisions are in the database and so in backups and Litestream replicas, like messages. There is no route to delete a revision, and the triggers refuse it. A future erasure or retention feature would have to drop the triggers in its own migration and design; this note does not promise one. The author-facing wording says "earlier versions are kept" (§13.3).

## 9. Rate limits

Humans: a per-principal bucket `message_edits_per_principal`, burst 10, one token every 6 s (10 a minute sustained), in memory like the reaction bucket (`reactionLimiter` over `bucketSet`, shared overflow key). Spent at §4.2 step 4 on every request that passes the channel gate, refusals included; only a `429` spends none. The revision read spends nothing. Agents never reach a bucket: they are refused at step 2. Together with the window and the edit limit, one person can store at most eleven versions of any message and ten edits a minute.

## 10. Composition with moderation (#698, M1 #849, M4 #852)

- **Removal wins.** A removed message cannot be edited (`409 message_not_editable`), and its revisions read is `404 message_not_found`. Edit and removal both run in immediate transactions, so one of them commits first; an edit that loses sees the removal row. M4's store check and this one read the same `message_removals` table; whichever PR lands second adds the check to the other's path.
- **Tombstones carry no `edited_at`.** Recommendation to M4: the tombstone projection omits `edited_at`, as it omits the payload, so the fixed tombstone body is never marked as an author's edit.
- **Replay ordering.** M4's per-id tombstone tracking in `ws.go` also suppresses an edit frame for an id already tombstoned on that socket, and the client rule says a removed copy is never replaced by one that is not.
- **`message_not_found`** is already defined (`pkg/schema/reaction.go`); M1 reuses it.
- **Search** under M4 excludes removed messages in SQL; the FTS row holds the last text, which M4 never returns. No interaction.
- Moderators do not edit other people's messages. If a manager wants a message changed, the operation is removal; the author may then post a corrected one.

## 11. Wire changes (this PR)

All additive. Existing fixtures are byte-identical (`scripts/schema-compat.sh` against `origin/main` passes).

| Change | File | Notes |
| --- | --- | --- |
| `MessageV2.EditedAt *Timestamp json:"edited_at,omitempty"` | `message_v2.go` | Optional. `Validate`: when present, strictly after `created_at`. `MessageV1FromV2` drops it. |
| `EditMessageRequestV1 {body}` | `message_edit.go` | `Validate`: not empty or white space. |
| `EditMessageResponseV1 {message}` | `message_edit.go` | |
| `MessageRevisionV1 {revision, body, created_at}` | `message_edit.go` | |
| `ListMessageRevisionsResponseV1 {channel_id, message_id, revisions}` | `message_edit.go` | 1 to `MaxMessageEdits + 1`, numbered from 1 in order, times strictly increasing. |
| `MessageEditedV1 {schema: "conch.message_edited.v1", message}` | `message_edit.go` | Events-socket frame; the message must carry `edited_at`. |
| `MessageEditWindowSeconds = 3600`, `MaxMessageEdits = 10` | `message_edit.go`, `ts.go` `TSConstants` | Exported to TypeScript as `MESSAGE_EDIT_WINDOW_SECONDS`, `MAX_MESSAGE_EDITS`. |
| Error codes `message_not_editable`, `message_edit_window_closed`, `message_edit_limit_reached`, `message_edit_requires_auth` | `message_edit.go` | Additive on `schema.Error`. |

Fixtures: `message-v2-edited.json`, `message-v2-edited-attachment.json`, `edit-message-request-v1.json`, `edit-message-response-v1.json`, `edit-message-response-v1-whisper.json`, `message-revision-v1.json`, `list-message-revisions-response-v1.json`, `list-message-revisions-response-v1-unedited.json`, `message-edited-v1.json`, `error-message-edit-window-closed.json`. TypeScript regenerated by `go generate ./pkg/schema`; `scripts/schema-ts-check.sh` passes. The hand-written re-exports in `web/src/schema/message.ts` belong to the web PR.

No SQLite change ships here: the migration is the server PR's.

## 12. Isolation and visibility rows

New file `e2e/isolation/editrows.go`, beside `reactionrows.go`, covering both routes (the `routes.go` coverage check fails without them). Each row asserts the status and code, that the body holds no marker of A, that the answer is byte-identical to the same request against a ghost (an unknown channel or an unknown message id, as the row says), and, for the edit, that nothing changed: A's message, its revisions, `messages_fts` matches and the audit log are as before, and no socket got a frame.

| Caller (against A's message) | Edit `PUT …/body` | Revisions `GET …/revisions` | Identical to |
| --- | --- | --- | --- |
| Member of another group, not in A's channel | `404 channel_not_found` | `404 channel_not_found` | ghost channel |
| Operator who is not a member of A's channel | `404 channel_not_found` | `404 channel_not_found` | ghost channel |
| Member of A's channel, not the author, channel-wide message they can read | `404 message_not_found` | `404 message_not_found` | ghost message id, and the next row |
| Member of A's channel, not a recipient of A's whisper | `404 message_not_found` | `404 message_not_found` | ghost message id, and the row above |
| Member not on A's net, for A's net message | `404 message_not_found` | `404 message_not_found` | ghost message id |
| Guest whose history bound is after A's message | `404 message_not_found` | `404 message_not_found` | ghost message id |
| Agent member with a full manifest, any target | `403 human_required` | `403 human_required` | the agent against a ghost channel and a ghost id |
| Channel manager or owner, not the author | `404 message_not_found` | `404 message_not_found` | ghost message id |

The principal's "identical refusals" reads, row by row, as: each class of caller gets one answer that is the same for a real target and a ghost, and the non-author and the whisper non-recipient get the very same bytes. The non-member's answer is the channel gate's, as on every channel route; the agent's is decided before any lookup, so it carries no information about the target.

Live rows: after A edits a whisper, a net message and a channel-wide message, the non-recipient's `/v2/ws` and events sockets, B's sockets and the bounded guest's sockets receive nothing for it; the recipients receive exactly one update each.

## 13. Web

### 13.1 Where the action is

On each of the viewer's own messages that can be edited (author is the viewer, the viewer is a person, no payload or a file attachment, not removed, not a pending or failed send, within `MESSAGE_EDIT_WINDOW_SECONDS` of `created_at` by the page's clock, fewer than `MAX_MESSAGE_EDITS` edits known): an **Edit** item in a message actions menu. One menu, opened four ways, as #972 specifies for member rows: a visible "More actions" (⋯) button in the message's action row, shown on hover and on keyboard focus within the message on a pointer device; right-click on the message (the native menu suppressed only there); long-press on touch; and Enter/Space on the focused ⋯ button. Escape, an outside click, navigation and choosing an item close it, and focus returns to the button. If #972's menu component has landed, reuse it; if not, build the message menu so #972 can adopt it, and do not block on it. Reply stays a direct button, as now. The server is the authority: the page hiding Edit is a convenience, and a `409` is handled (§13.2).

Shortcut: with the composer empty and focused, Up arrow edits the viewer's newest editable message in the open conversation (the convention people know). Optional in the first web PR.

### 13.2 Inline editing

Edit replaces the message text with a textarea holding the current body, focused, caret at the end, with **Save** and **Cancel** buttons under it and a line "Editing. Enter to save, Esc to cancel". Attachments, an audience badge and the thread controls stay drawn and are not editable.

- Enter saves, Shift+Enter adds a line, and Enter during IME composition does nothing: the composer's rule (`Composer.tsx:254`). Esc cancels and restores; focus returns to the message's ⋯ button.
- Save is disabled while the text is empty or white space, or equal to the current body. While saving the textarea is read-only and Save reads "Saving…".
- Success: the response replaces the held message (client rule), the editor closes.
- Errors, shown inline under the editor, the text kept:
  - `message_edit_window_closed`: "This message can no longer be edited: it is more than an hour old." Editor closes on Cancel.
  - `message_edit_limit_reached`: "This message has been edited the most times allowed."
  - `message_not_editable`: "This message can't be edited."
  - `message_not_found` or `channel_not_found`: "This message is no longer available to you." Editor closes.
  - `rate_limited`: "Too many edits. Try again in N seconds." Save re-enabled after N.
  - network failure: "Couldn't save. Your text is kept." Save retries; a retry of a `PUT` is safe (§3.5).
- One editor at a time per conversation. Opening another, switching channel, or losing the socket's membership closes it; unsaved text is dropped only after "Discard your edit?" when it differs from the current body.
- A live edit of the same message from the author's other tab while editing updates the message underneath; the editor keeps the person's text, and Save sends it.

### 13.3 The marker and earlier versions

A message with `edited_at` shows "(edited)" after its text, muted, as a `<time datetime="…">` whose accessible name is "Edited at 12:40" (the local time; the date too if not today). For every reader but the author it is plain text with that title. For the author it is a button that opens **Earlier versions**: a dialog that fetches `GET …/revisions` on open (never prefetched), lists the versions newest first with their times, marks the current one, and says "Only you can see earlier versions. They are kept on the server." Errors there are shown in the dialog; `message_not_found` closes it. The marker appears identically live, after reload, in threads and in search results.

### 13.4 State

- `state/messages.ts`: `received`, `historyLoaded`, `olderLoaded` and `sent` apply the client rule (§7.1) through the existing map-by-id merge; a stale copy never replaces a newer one. An edit frame for a message not held is dropped: it is not a new message and must not be appended.
- The events socket: `agents/frames.ts` gains a `messageEdited` kind validated with the generated `messageEditedV1`; `useNotifications.ts` updates a held alert's body in place and never notifies or counts unread for it.
- Wire types only from `web/src/schema/` (re-export the generated types in `message.ts`); no `any`, no inline style.

### 13.5 Phone (below 700 px)

There is no hover: the ⋯ button is always shown on the viewer's own editable messages, at least 44 px square, and long-press opens the same menu. The editor is full width with Save and Cancel as 44 px buttons; Enter follows the composer's phone behaviour. Earlier versions opens as a full-width sheet. Tested at 320 × 640 and 390 × 844, with no horizontal scroll.

## 14. Implementation split

Two PRs under #845 (or child issues the principal files), server first. Both carry `security` review. Neither touches the approval path (approval messages are refused by the payload check; no approval code changes). If either does touch it, it becomes `approval-path` and needs the full request → notify → resolve → audit test and Nick's merge.

### 14.1 PR 1: server (server-engineer)

**Scope.** Migration (§8.1), `EditOwnMessage` and `OwnMessageRevisions` in `store/audience.go`, the forbidden-list addition, the two routes in `routes.go` and a new `internal/server/message_edit.go` (check order §4.2, refusals §4.3), the edit bucket (§9), the hub broadcast and replay-filter change (§7.1), the events kind and frame (§7.2), the audit row (§8.3), isolation rows (§12). No MCP tool. No web change.

**Tests.**
- Store: migration from the previous version with existing messages readable; `messages_only_body_changes` refuses an update of each protected column; `message_revisions` refuses update and delete; `edited_at` strictly increasing under a stepped-back clock and two edits in one millisecond; revision 1 written on first edit only; limit at exactly ten edits; window boundary at exactly 3600 s and 3600 s + 1 ms with an injected clock; no-op writes nothing; FTS: old word gone, new word found, other messages unaffected, `INSERT INTO messages_fts(messages_fts) VALUES('integrity-check')` passes after many edits; `TestOnlyAudienceGoReadsMessageRows` still passes.
- Handler table: every row of §4.3 in the §4.2 order (an agent on a ghost channel gets `human_required`; a non-member with a bad body gets `channel_not_found`; a token is spent on a `404`); unknown fields refused; body at and over `MaxPostRequestBytes`; auth off.
- Immutability: a file-attachment message keeps `payload` byte-identical, `files.message_id` unchanged, and `GET /v2/files/{id}` still serves the same bytes to every recipient; an approval message is `409` and its approval is untouched; reactions, thread replies and the thread root link unchanged; audience, `message_recipients` and guest attribution unchanged; read marks and unread counts unchanged.
- Live: `/v2/ws` frame to exactly the original recipients (channel-wide, net, whisper), none to v0/v1, no `Broadcaster` call (recording broadcaster); `ws.go` replay with an edit committed during the replay delivers the edit, and with an edit before the replay delivers no stale copy; history-gated guest gets nothing; events frame to human members incl. the author's other socket, none to agents or non-recipients, no read-state document, coalesced for a burst.
- Audit: one `message_edited` row per changing edit with the right revision, none for refusals or no-ops, no text in any row.
- MCP: `read_channel` returns `edited_at`; the tool list is unchanged.
- `go test -race ./internal/server/...`, `CI=1 go run ./e2e/isolation`, `make check`.

### 14.2 PR 2: web (web-engineer)

**Scope.** §13: the message actions menu with Edit, inline editor, marker, Earlier versions, state rules, events-frame handling, phone layout. `web/src/schema/message.ts` re-exports. Blocked by PR 1 for the Playwright tests.

**Tests.** Reducer: a stale history page after an edit frame keeps the edit; two frames out of order; an edit frame for an unheld message is dropped; the events frame never notifies or counts. Component: Edit shown only on the viewer's own eligible messages (not others', not approval cards, not after the window, not on pending sends); Enter saves, Shift+Enter newline, IME Enter ignored, Esc cancels and restores focus; each refusal's message; the marker's accessible name; the author's marker opens versions, another reader's does not; phone layout. Playwright with two browsers on a real server: edit, live update in the other page, reload shows the same, search finds the new word and not the old, the other reader cannot open versions, a whisper non-recipient sees nothing, attachments still render after an edit. `make web-check`, `make check`.

### 14.3 Follow-up (not #845): terminal

A separate issue for cli-engineer: `conch edit <channel> <message-id> <text>` (or `-` for standard input) over the PUT route, `conch search` and `conch thread` output showing "(edited)", the TUI rendering the marker (it already upserts by id), and `conch tail` printing an edit frame as an edit of message *n*, not as a new message. TUI inline editing is out of scope until asked for.

## 15. Questions for review

1. **Non-author answer.** This note gives a member who can see someone else's message `404 message_not_found`, the same as an invisible one (§4.3). The alternative, `403 forbidden` for a visible message, is friendlier to a client bug but needs a per-message visibility branch to stay safe for scoped messages. Recommendation: keep the 404.
2. **v1 marker.** v0/v1 readers get the edited text unmarked (§6.3). No first-party client is affected. Adding `edited_at` to `MessageV1` would be additive if a v1 consumer appears.
3. **Edit limit of ten.** Chosen to bound storage and keep the revision list unpaginated; it can be lowered without a wire change, not raised above `MaxMessageEdits` without one.
