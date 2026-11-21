# Design: local 1:1 direct messages and private voice calls

- **Status:** Proposed with [ADR-017](../adr/ADR-017-local-direct-messages-and-calls.md) (2026-11-19). Accepted when Nick merges the PR that adds both.
- **Issue:** [#2038](https://github.com/njdaniel/conch/issues/2038); epic [#2013](https://github.com/njdaniel/conch/issues/2013); decision on [#1501](https://github.com/njdaniel/conch/issues/1501).
- **Owners:** protocol-designer (fixtures, §2), server-engineer (§3–§9), web-engineer and voice-engineer (clients).

ADR-017 decides. This note says how: the wire sketch the fixtures issues start from, who may do what to whom, the call states, the limits, how fast access ends, what is audited, and how act-as credentials are told apart. Names of routes, types and frames below are a **sketch**: #2016 (DM fixtures) and #2029 (call fixtures) fix them through the `schema-change` skill, and may rename them without reopening this note. The rules are not a sketch: a child that wants to change a rule, a bound or a refusal code's meaning changes this note first, in its own PR.

All line references are to main at the commit this note merges on.

## 1. Scope

In v1: open or resume a conversation with one other person; send plain text and images; edit and remove one's own messages; read marks and unread counts; search within one conversation; content-free push; voice calls with ringing on every signed-in browser and desktop, answer, decline, cancel, hang up, missed calls. Clients: browser and desktop for everything; TUI and CLI for DMs only.

Out of v1 (ADR-017, cut list): camera and screen share in calls; companion identities in call rooms; reactions, threads, mentions, audiences and typed payloads in DMs; inbox-wide search; TUI calls; contact preferences beyond the three in §3.3; native mobile ringing; phones at all until ADR-016 Decision 10 is amended (#2037); "report a DM"; group conversations (#1501).

## 2. Fixtures sketch

### 2.1 Resources

| Resource | Fields (sketch) | Notes |
|---|---|---|
| Conversation | `id`, `peer` (the other principal, as the directory shows it to the caller, or a former-member label), `created_at`, `last_message_at`, `unread`, `hidden` (the caller's own flag) | Never carries the caller's own id as a field to fill in: the caller is implicit. The store keeps `lo`, `hi`. |
| DM message | `id`, `conversation_id`, `author_id`, `body`, `files`, `created_at`, `edited_at`, `removed` | Own id space, scoped by resource type. No `payload`, `audience`, `thread_root_id`, `mentions`. |
| DM read state | `conversation_id`, `last_read_id`, `updated_at` | Same clamp as channels: a mark above the newest visible message is taken as that message (`internal/server/store/read_state.go:71-76`). |
| Call attempt | `id`, `conversation_id`, `caller_id`, `callee_id`, `state`, `reason`, `revision`, `ring_deadline`, `join_deadline`, `created_at`, `ended_at` | The selected endpoints (credential ids) are stored, never sent to the other side. |
| Call session | `room`, `token`, `url`, `expires_at` | Only to a selected endpoint, after acceptance (§4). |

`MessageV2`, `ReadStateV1`, `FileV1` and `NotificationSummaryV1` all require `channel_id > 0` (`pkg/schema/message_v2.go:249`, `pkg/schema/read_state.go:54`, `pkg/schema/file.go:124`, `pkg/schema/notification.go:224`). DMs therefore get new types, and the pending-notifications response needs a version that can carry a DM summary. The `NotificationKind` vocabulary (`pkg/schema/notification.go:161-167`) gains a DM value and a missed-call value.

### 2.2 Routes (sketch)

| Route | Does |
|---|---|
| `POST /v1/dms` `{principal_id}` | Open or resume. Returns the conversation. Creates nothing the other person can see. |
| `GET /v1/dms` | The caller's inbox: conversations with at least one message or call attempt the caller may see, not hidden by the caller. Paged. |
| `GET /v1/dms/{id}` | One conversation. |
| `GET /v1/dms/{id}/messages` | Page of messages, the channel pagination conventions. |
| `POST /v1/dms/{id}/messages` | Send. Refuses `payload`, `audience` and `thread_root_id` with 400 `invalid_request`. |
| `PATCH /v1/dms/{id}/messages/{message_id}` | Edit one's own message. |
| `DELETE /v1/dms/{id}/messages/{message_id}` | Remove one's own message. There is no admin removal. |
| `PUT /v1/dms/{id}/read-state` | Move the caller's mark. |
| `PUT /v1/dms/{id}/hidden` | Hide or unhide in the caller's inbox only. A new message unhides. |
| `GET /v1/dms/{id}/search` | Search this conversation only. |
| `POST /v1/dms/{id}/calls` | Invite. Answers with the attempt (state `ringing`, or `unavailable`). |
| `GET /v1/calls/current` | The caller's non-terminal attempt, if any, for reconnect. |
| `POST /v1/calls/{call}/accept` · `/decline` · `/cancel` · `/hangup` | Transitions, each with `revision`. Idempotent on an attempt already in the target state. |
| `POST /v1/calls/{call}/session` | A join token for the caller's selected endpoint. |
| `PUT /v1/me/dm-preference` | `everyone` · `shared_channel` · `nobody`. |
| `PUT /v1/principals/{id}/dm-permission` | Operator's per-person switch, `manage_members`, re-auth as for `voice-permission` (`internal/server/routes.go:361,465`). |
| Server-wide switch | A field on the server settings route of the class that sets `hide_members`; default on. |
| Per-person events socket | `/v1/events/ws` without `?channel=` (today it is required, `internal/server/events_ws.go:646-650`), or a new route; #2016 chooses. |

DM files: #2017 and #2023 choose between one more branch in `FileVisibleTo` (`internal/server/store/audience.go:695`), so that one function decides for one id space, or a separate route and id space. **Not** two independent checks on one id space. DM files are never readable by agents (`humansOnlyFiles`, `internal/server/authz.go:820`).

### 2.3 Frames on the per-person socket (sketch)

`dm.message`, `dm.message_edited`, `dm.message_removed`, `dm.read` (the caller's own marks, for other devices), `call.state` (attempt id, state, reason, revision), and a `snapshot` frame on open carrying unread counts and the current call attempt. Recipients are computed at fan-out time against current state (§3), as `dropPrincipal` does for channels. A frame never names a principal the receiver may not see.

### 2.4 Refusal codes

| Status, code | When | Says about others |
|---|---|---|
| 404 `principal_not_found` | Open: the target does not exist, has ended, or is hidden from the caller | Nothing: one body for all three |
| 404 `dm_not_found` | Any `{id}` route: no such conversation, or the caller is not one of its pair (whatever the caller's role) | Nothing |
| 404 `call_not_found` | Any `{call}` route: no such attempt, or the caller is neither side | Nothing |
| 403 `contact_unavailable` | Open, send, edit: the target is visible but cannot be contacted (§3.2) | "Can't contact", never why |
| 403 `dms_off` | The caller's own DMs are switched off, or the server's are | About the caller or the server only |
| 403 `own_sign_in_required` | The caller's credential is an act-as credential (§7) | About the caller only |
| 403 `forbidden` | The caller is an agent (the existing people-only refusal, `internal/server/authz.go:859-860`) | Nothing |
| 409 `call_state_changed` | A transition whose `revision` is stale; carries the current state | About this attempt only |
| 409 `call_in_progress` | The caller already has a non-terminal attempt (other than the glare case, §4.2) | About the caller only |
| 429 `rate_limited` | §5, with `Retry-After` | Nothing |

A call invite to an ineligible or busy callee is **not** an error: it answers 200 with an attempt in state `unavailable` (§4).

## 3. Contact and refusal matrix

### 3.1 Order of checks

Each DM and call route checks in this order and stops at the first refusal, so that no later check leaks through an earlier one:

1. authenticated; 2. a person, not an agent; 3. the credential is the caller's own (§7); 4. for writes, the server's and the caller's own switch; 5. the rate limit (§5); 6. pair membership (`dm_not_found`) or target visibility (`principal_not_found`); 7. target eligibility (`contact_unavailable`, or `unavailable` for a call).

The open and invite buckets are spent on refused attempts too, so probing costs tokens.

### 3.2 The matrix

Rows are the state of the target or the pair; columns are the caller's act. "Read" covers history, files, search and read marks. "Live" is what the per-person socket delivers.

| State | Open (new pair) | Open (existing pair) | Send / edit | Remove own | Read | Call invite | Live frames |
|---|---|---|---|---|---|---|---|
| Target does not exist, has ended, or is hidden from the caller (`hide_members`) | 404 `principal_not_found` | 404 `principal_not_found` | n/a | n/a | n/a | n/a | n/a |
| Caller not one of the pair (any role, operator included) | n/a | n/a | 404 `dm_not_found` | 404 | 404 | 404 | none |
| Target is the caller | 400 `invalid_request` | n/a | n/a | n/a | n/a | n/a | n/a |
| Target is an agent | 400 `invalid_request` (agents are listed as agents, so this says nothing new) | n/a | n/a | n/a | n/a | n/a | n/a |
| Target disabled (a person, or an agent) | 404 `principal_not_found`, as `PUT /v1/blocks/{id}` answers (one shared fragment, `listedPrincipalSQL`; a disabled agent is not found either, not "agent") | resumes (read only) | 403 `contact_unavailable` | allowed | allowed; former-member label if identity is no longer visible | `unavailable` | none to the disabled side |
| Block in either direction (#1832) | 403 `contact_unavailable` | resumes (read only) | 403 `contact_unavailable` | allowed | allowed | `unavailable`; a ringing or active call ends (§4) | dropped at fan-out |
| Target's preference `nobody` | 403 `contact_unavailable` | resumes (read only) | 403 `contact_unavailable` | allowed | allowed | `unavailable` | normal |
| Target's preference `shared_channel` and no channel in common | 403 `contact_unavailable` | resumes (read only) | 403 `contact_unavailable` | allowed | allowed | `unavailable` | normal |
| Target's DMs switched off by an operator | 403 `contact_unavailable` | resumes (read only) | 403 `contact_unavailable` | allowed | allowed | `unavailable` | normal |
| Caller's DMs switched off by an operator | 403 `dms_off` | allowed (read only) | 403 `dms_off` | allowed | allowed | 403 `dms_off` | reads only |
| Server-wide switch off | 403 `dms_off` | allowed (read only) | 403 `dms_off` | allowed | allowed | 403 `dms_off` | reads only |
| `hide_members` on; the target is visible to the caller but the caller is not visible to the target | 403 `contact_unavailable` | resumes (read only) | 403 `contact_unavailable` | allowed | allowed; authors keep their names (ADR-017 Decision 11) | `unavailable` | normal |
| Target busy in another private call, or with no signed-in device | allowed | allowed | allowed | allowed | allowed | `unavailable` | normal |
| Caller's credential is act-as (§7) | 403 `own_sign_in_required` on every column, reads included | | | | | | none |
| Caller is an agent | 403 `forbidden`, with the existing agent-denial audit row (route only, no target) | | | | | | none |
| Caller disabled or credential revoked | 401, as everywhere | | | | | | socket closed |

**The same-404 rule.** A guessed principal id that is hidden, nonexistent, ended, or (for a new pair) disabled, gets byte-identical answers and the same timing class (one indexed lookup) as "principal not found", and the same set `PUT /v1/blocks/{id}` answers 404 for, so the two routes cannot be compared to mark an id as a disabled person or agent (the directory never shows who is disabled). A conversation or call id the caller is not part of answers like one that does not exist. No DM or call route is previewable; a structural test beside `TestPreviewableRoutesAreReads` (`internal/server/preview_guard_test.go:15`) pins it.

**The generic `contact_unavailable` rule.** One code and one message for every eligibility reason. It tells a caller that this person cannot be reached now, which a person can usually see anyway; it never says which of block, preference, switch, disabled or visibility it was, and never who blocked whom.

**"Resumes (read only)"** means `POST /v1/dms` returns the existing conversation so the caller can read it; a new pair in the same state gets nothing created.

### 3.3 Contact preference

`everyone` (default), `shared_channel` (both are members of at least one channel, public or private, at the moment of the write), `nobody`. It is the recipient's preference and applies to every write and invite, not only to the first. It is the person's own setting; operators do not see it.

## 4. Calls

### 4.1 Transition table

Every change is a compare-and-swap on `revision`. Terminal states (`unavailable`, `declined`, `cancelled`, `missed`, `failed`, `ended`) are final: no transition leaves them and no attempt reopens. A repeated request for the transition that already happened answers 200 with the attempt (idempotent).

| From | Event | To | Side effects |
|---|---|---|---|
| none | invite; callee eligible (§3) and has no non-terminal attempt | `ringing` | `ring_deadline` = now + 40 s; caller's credential recorded as its endpoint; `call.state` to every callee socket |
| none | invite; callee ineligible, busy or with no signed-in device | `unavailable` | Visible to the caller only. The callee is told nothing and gets no missed-call record. |
| none | invite while the callee is ringing the caller (glare) | (no new attempt) | Returns the existing callee→caller attempt as the caller's incoming call (§4.2) |
| `ringing` | callee's first valid accept (CAS) | `accepted` | Accepting credential recorded as the callee's endpoint; `join_deadline` = now + 30 s; every other callee socket told to stop ringing |
| `ringing` | callee declines | `declined` | |
| `ringing` | caller cancels | `cancelled` | |
| `ringing` | `ring_deadline` passes | `missed` | A missed-call record for the callee alone |
| `accepted` | both selected endpoints seen in the room by the poller | `active` | |
| `accepted` | `join_deadline` passes without both | `failed` (`join_timeout`) | Room deleted |
| `accepted`, `active` | either side hangs up | `ended` (`hangup`) | Room deleted |
| `active` | either side absent from the room longer than the 20 s reconnect grace | `ended` (`gone`) | Room deleted |
| any non-terminal | block either way; either side disabled; a selected credential revoked; DMs or voice switched off for either side or the server | `ended` (`revoked`), or `cancelled` (`revoked`) from `ringing` | Room deleted; no missed-call record; the reason is shown to neither side beyond "call ended" |
| any non-terminal | conchd starts, or a restore is applied | `failed` (`server_restart`) | Every call room swept |

The 40 s, 30 s and 20 s figures are v1 values. #2030 may change them with a recorded reason in this note.

### 4.2 Races

- **One attempt per person.** A principal has at most one non-terminal attempt as caller or callee. A second invite from a person who has one answers 409 `call_in_progress`; an invite to a person who has one answers `unavailable`.
- **Glare** (A invites B while B's invite to A is ringing): A's request returns B's attempt, which A can accept. Neither side sees "busy".
- **Accept on two devices:** the first CAS wins; the second gets 409 `call_state_changed` with state `accepted` and stops ringing.
- **Endpoint identity is per credential.** LiveKit identity is `p<id>` per principal (`internal/server/voice.go:31-33`), and browser tabs share one credential, so conchd can bind the call only to a credential. The join token is issued only to the selected credentials. A second tab on the same credential that joins displaces the first, as in channels. Ringing dismissal needs no per-socket id: the device that sent the accept knows from its own 200, and every other socket sees `accepted` and stops; a socket that did not send the accept never joins by itself.
- **Reconnect:** a client that loses its socket reads `GET /v1/calls/current` and the `snapshot` frame, and follows the server; it never infers state locally.
- **Channel voice:** Answer, when the person is in channel voice, first asks and then leaves channel voice (ADR-017 Decision 6). Browsing never moves voice (`web/src/voice/useVoice.ts:292`, the joined channel versus the one shown). The server does not enforce one room per person; that stays ADR-016 M1-4's future issue.
- **After a terminal state**, no client rejoins the call room, whatever token it still holds (§6).

### 4.3 Rooms

Call rooms get their own tables (`call_rooms`, holders), never `voice_rooms`, whose `channel_id` is `NOT NULL` (`internal/server/store/store.go:275`). Names use the same 128-bit random shape (`internal/server/store/voice.go:224-233`). The poller tracks call rooms without feeding occupancy, which counts only channel-wide rooms (`internal/server/voice_occupancy.go:29-30`). Call rooms join the orphan sweep's known set: `orphanRooms` deletes any room of this instance's name shape that the store does not hold (`internal/server/voice_presence.go:2590-2613`), so a call room outside the store would be deleted within one 30 s sweep. A structural test keeps occupancy and presence from reading call rooms. The code does not go in `internal/server/calls.go`, which is agent session calls (#267).

## 5. Limits

In-memory token buckets per principal, as every limit in conchd is (`internal/server/channel_write_limit.go:10-31`): a refusal is 429 `rate_limited` with `Retry-After`, answered before the store is touched; a restart refills them.

| Bucket | v1 value | Spent on |
|---|---|---|
| New conversations | 20 per day | the first message or invite in a pair that had none, and refused opens of a new pair |
| DM messages | burst 20, then 1 per 2 s | every send, across conversations |
| DM edits | burst 10, then 1 per 6 s; plus the channel cap on revisions per message (409 `message_edit_limit_reached`, `internal/server/message_edit.go:38`) | every edit |
| Call invites | burst 3, then 1 per 20 s | every invite, refused or `unavailable` included |
| Repeat invites to one callee | 3 per 10 minutes per pair | invites that ended `unavailable`, `declined` or `missed` |
| Uploads | the existing upload limits | as for channels |

Message body, file size and file count are the channel limits. #2018 may tune the values with a recorded reason here.

## 6. Revocation bounds

When a block, a disable, a revoked credential or a switch ends a call, the media ends within:

| Condition | Bound | From |
|---|---|---|
| LiveKit reachable | about 4 s for the evict request, plus one poller pass (at most 1.5 s), then the room is deleted | `voiceEvictTimeout` (`internal/server/voice_presence.go:98`), `voicePassGapMax` (`:62`) |
| LiveKit unreachable | the first successful sweep after it returns; retries back off to at most 30 s | `voiceBackoffMax` (`:80`), `voiceSweepInterval` (`:65`) |
| A token already issued | a join token can start a connection for about 75 s (15 s lifetime plus LiveKit's 60 s leeway). With LiveKit's auto-create on, a stale token can recreate a deleted room; that room is retired in the store and deleted by the next sweep (≤30 s), and nobody else is in it | `internal/server/voice.go:25-29`, `:411-422` |

Sockets close and ringing stops at once on disable, through `principalDisabled` (`internal/server/disable.go:133-139`), which the per-person socket must join. A block drops live frames at the next fan-out.

## 7. The act-as rule

`act_as_members` (`pkg/schema/permission.go:60-64`) lets an operator mint a credential for a non-owner (`POST /v1/principals/{id}/credentials`, `internal/server/routes.go:220`) or make a sign-in link for them. Either would read the person's DMs through the ordinary API. DM and call routes refuse, with 403 `own_sign_in_required`, a credential when:

1. `credentials.issued_by` is set and names a principal other than the credential's holder; or
2. it was made by redeeming a sign-in link whose `created_by` is not its holder.

Both facts are already stored: `credentials.issued_by` (migration 42, `internal/server/store/store.go:996`) names the issuing operator or the person themself, and `sign_in_links` records `created_by` and the `credential_id` it made (`internal/server/store/store.go:646-657`). A link a person makes for their own second device (ADR-016 Decision 4) is their own. No new column is needed.

**Residual case.** `issued_by` is NULL for a bootstrap, an invite redemption, a link redemption, and rows from before 2026-10-08 that the backfill found no audit row for (`internal/server/store/issued.go:15-26`). Links are caught by rule 2. Refusing every other NULL would refuse every member still on the credential their invite made, so NULL counts as the person's own; an operator-issued credential older than 2026-10-08 with no audit trail cannot be told apart. The docs say so, and an owner who cares revokes old credentials. (Open point O1 for Nick.)

Hooks never authenticate on DM or call routes. A person refused by this rule signs in themself once (password, or a link they make on another device).

## 8. Audit policy

- **No audit row names the counterparty.** Opening, sending, editing, removing, reading, searching, calling and blocking write no row whose subject or detail names the other person or the conversation. This is the opposite of whispers, whose rows name the recipients (`internal/server/store/audience.go:33-37`), because `GET /v1/audit` is readable by `read_audit_log` holders (`internal/server/routes.go:267`).
- **Audited as settings:** the operator's per-person switch (subject: the person switched; no counterparty) and the server-wide switch. A person's own contact preference is not audited.
- **Agent refusals** write the existing agent-denial row, naming the route, never a target.
- **Aggregate counts only** for operators: conversations, messages, call attempts by terminal state, refusals by code. No principal ids, no pairs. conchd has no metrics surface on main; the first one is its own issue (open point O2).
- **Restore:** a restore rolls a block made after the backup back, as it rolls back memberships and messages (`internal/server/store/restore.go:27-30`). Every credential is ended by the restore anyway, and every non-terminal call ends `failed` (§4.1). The docs say so.

## 9. Retention, backup and restore

**conchd has no retention job** for messages or files: nothing deletes them by age, and even an uploaded file that is never attached persists (`docs/design/browser-image-attachments.md:79-90`). A DM lasts until its author removes it; removal follows the channel author-removal rules (#1828) for both sides. Hiding a conversation is per viewer and deletes nothing. Backups include DM rows and their blobs. A restore never reactivates a call.

## 10. Children of #2013 this note feeds

| Child | Uses |
|---|---|
| #2016 DM fixtures, #2029 call fixtures | §2 |
| #2017 store | §2.1, §4.3; a single visibility function for DM rows with a structural guard beside `TestOnlyAudienceGoReadsMessageRows` (`internal/server/store/audience_test.go:493`) and `TestMessageReadsStayInTheVisibilityFunction` (`internal/server/visibility_guard_test.go:31`) |
| #2018 eligibility | §3, §5, §7; blocked by #1832 (the block relation, not on main) |
| #2025 push and pending | content-free wake (`internal/server/push/doc.go:16`), one helper as `TestNewMessagesReachPushThroughOneHelper` (`internal/server/push_send_test.go:41`) pins; a DM preview obeys the existing per-credential preview switch |
| #2030–#2032 calls | §4, §6 |
| the desktop call path (not yet filed) | `voice_control` v4: v3 addresses a session by `channel` only (`pkg/schema/voice_control_v3.go:198-201`), and the helper joins only through the channel session route (`voice/crates/conch-voice-api/src/client.rs:145`); the desktop renderer never holds the voice grant (`web/src/platform/types.ts:171-173`) |
| the isolation matrix (not yet filed) | §3.2 as its table: actor (pair member, other member, operator, act-as, agent, disabled, stale credential) by path (every route, socket, file, search, pending, push, preview, admin, MCP) |

## 11. Open points for Nick

| # | Point | Recommendation |
|---|---|---|
| O1 | Credentials with a NULL `issued_by` and no sign-in link (§7): count as the person's own | Yes. Refusing them would lock out every member still on their invite credential. |
| O2 | Where the aggregate counts live (§8): no metrics surface exists | A later issue; v1 ships without counts rather than with per-pair rows. |
| O3 | The v1 timings (§4.1) and limits (§5) | As written; the children may tune them with a recorded reason. |
| O4 | ADR-000's D12 row carries its earlier amendments inline (ADR-005, ADR-003). This PR adds only a header note there, as for the other ADRs | Add the inline "*Amended by ADR-017*" to the row on acceptance if Nick prefers the existing convention. |
