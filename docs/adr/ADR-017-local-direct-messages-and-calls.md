# ADR-017: Local 1:1 direct messages and private voice calls

- **Status:** Proposed. It is accepted when Nick merges the PR that adds it. The product decisions it records were made by Nick on 2026-11-19, in session ("i agree"), on [#2013](https://github.com/njdaniel/conch/issues/2013); this text is still his to accept.
- **Date:** 2026-11-19
- **Decider:** Nick (Tier-H)
- **Proposal:** [#2038](https://github.com/njdaniel/conch/issues/2038), the root of epic [#2013](https://github.com/njdaniel/conch/issues/2013); the decision is the one recorded on [#1501](https://github.com/njdaniel/conch/issues/1501) (2026-11-19), which stays the research issue for group conversations.
- **Design:** [docs/design/direct-messages.md](../design/direct-messages.md): the fixtures sketch, the contact and refusal matrix, the call transition table, the limits, the revocation bounds, the audit policy and the act-as rule.
- **Amends** (accepted ADRs are not edited; each gets a dated header note pointing here, and the old and new text are below, under [Amendments](#amendments)):
  - [ADR-000](ADR-000-charter.md) **D12**: human 1:1 DMs and private voice calls come forward from P7; agent DMs stay at P6.
  - [ADR-003](ADR-003-multi-human-access.md)'s Unchanged line on standalone human DMs.
  - [ADR-004](ADR-004-voice-via-livekit.md)'s room kinds: a third kind, one private room per call attempt.
  - [ADR-005](ADR-005-nets-and-whispers.md)'s rewording of D12.
  - [ADR-008](ADR-008-private-communications-alpha.md) **Decision 5**'s one-on-one sentence, and its Unchanged line on human DMs.
  - [ADR-013](ADR-013-one-community-model.md)'s list of what in ADR-000's locked table does not move.
  - ROADMAP.md **P7** (Nick's edit, in this PR).

  **Not amended in v1:** [ADR-015](ADR-015-webcam-video.md) Decision 1 (camera in the channel-wide voice room only), ADR-008 Decision 3 (screen share into a channel's voice room) and [ADR-016](ADR-016-mobile-app.md) Decision 10 (DMs out of the mobile phase). Calls carry no camera and no screen share, and the phone work in [#2037](https://github.com/njdaniel/conch/issues/2037) is held until a later ADR amends ADR-016 Decision 10. Single tenancy, the deployment invariant, no E2EE and no federation are unchanged.

## Context

ADR-000 D12 (line 78) put human DMs at P7. Five later ADRs repeat it or build on it: ADR-003 (line 23), ADR-005 (line 38), ADR-008 (lines 61 and 78), ADR-013 (line 16) and ADR-016 (line 38); ADR-004 (line 18) knows only channel and net rooms; and ROADMAP.md kept human DMs in P7. The one way to talk to one person on main is a private channel with two members, which someone has to create and add the other person to, and which any operator can enter.

On 2026-11-19 Nick recorded on #1501 that a local DM is a **first-class conversation of exactly two immutable human principals**, not a hidden channel, and epic #2013 adopted it. A principal-engineer review of both issues (on #2013) found that the code supports that choice more strongly than the #1501 comment says:

- **An operator can add themself to any channel.** `accessChannelManager` admits a holder of `manage_channels`, a member or not (`internal/server/auth.go:479-520`); `POST /v1/channels/{channel}/members` (`internal/server/routes.go:84`) then adds them, and ADR-013 Decision 3 gives a new member the full history. A DM kept as a channel would be two requests away from any operator.
- **Channel membership is previewable.** `GET /v1/channels` and `GET /v1/channels/{channel}/members` are marked `asPreviewable` (`internal/server/routes.go:37,80`), so preview-as-member would show an admin a member's DM pairs.
- **About fifty channel-scoped routes** (`internal/server/routes.go:43-216`), the admin action targets (`internal/server/admin_actions.go:159`), MCP `read_channel` and the audit export would each need a DM exception for the life of the feature.

The same review found what the charter has to settle before any schema or code: the existing act-as path (`act_as_members`, `pkg/schema/permission.go:60-64`, on by default for operators) is a sanctioned way to read anyone's messages through the ordinary API; the audit log already names the recipients of a whisper (`internal/server/store/audience.go:33-37`) and is readable by `read_audit_log` holders (`internal/server/routes.go:267`); and conchd has no retention job of any kind, so "subject to the server's retention policy" in #2013 points at nothing. Nick accepted every recommendation of the review's §6 on 2026-11-19 (Decisions, below).

## Decision

1. **A direct message is a conversation of two local human principals, separate from channels.** It has a durable, server-local id and exactly two principal ids, fixed at creation and never changed. SQLite stores the ordered pair once (`CHECK (lo < hi)`, `UNIQUE (lo, hi)`, foreign keys to principals). Open-or-resume is atomic and returns the same id in either direction, including for two concurrent requests. The authenticated caller supplies one side; a name, a device or a Cloud account never supplies identity. A conversation can never gain a third member; a later group design uses its own membership model (#1501).
2. **The pair is the access boundary.** Every read of a conversation, its messages, files, read marks, search and call attempts requires that the caller is one of the two. Operator role, preview-as-member, an agent or MCP capability, an admin action or the audit export grants none of it. The hosting operator still holds the database and its backups; the docs say so (discretion, not secrecy, as ADR-005 says of whispers).
3. **Contact needs eligibility on both sides; reading retained history needs only membership.** A new conversation, a send, an edit and a call invite need: the caller one of the pair (for open: the target a person other than the caller); both principals human, enabled and not removed; the caller's credential live and the caller's own (Decision 7); no personal block in either direction ([#1832](https://github.com/njdaniel/conch/issues/1832)); DMs on for the server and for both people; the recipient's contact preference met; and, on a `hide_members` server (ADR-013 Decision 9), mutual visibility. Reading history needs pair membership and an enabled principal. Every write and every live fan-out re-checks against current state. The refusals are in the design's contact and refusal matrix: a target the caller may not see gets the same 404 as one that does not exist, and a visible target that cannot be contacted gets one generic `contact_unavailable` that never says why.
4. **Contact preferences are three:** everyone, people I share a channel with, nobody. Opening an empty conversation creates nothing the other person can see until a message or a call invite is sent.
5. **Content in v1:** plain-text bodies, image attachments, edit, and removal by the author. No reactions, threads, mentions or audience fields. **DM posts refuse typed payloads**, approval and admin-action cards included, so the approval path is not touched: approvals and hooks stay channel-bound (`approvals.channel_id NOT NULL`, `internal/server/store/store.go:83`). A later change that lets any approval or admin-action card into a DM is an approval-path change under CLAUDE.md rule 3.
6. **Calls are voice only, one fresh LiveKit room per attempt.** Each attempt has its own id, caller, callee, state, revision, deadlines and one selected endpoint per side. A principal has at most one non-terminal private call at a time. conchd issues a room-scoped join token, with the 15 s lifetime of channel voice (`internal/server/voice.go:29`), only to the accepting callee's selected credential and the caller's, after acceptance and the current voice entitlement check. The room is never in occupancy, presence, `GET /v1/channels/{channel}/voice` or any channel listing. No camera, no screen share and no companion identity (`.screen`, `.view`) join a call room in v1. Opening a DM or receiving a ring starts no capture. Answering a call while in channel voice leaves channel voice after an explicit prompt: no person is in two rooms at once through these clients in v1. The transition table is in the design.
7. **DM and call routes refuse act-as credentials.** A credential is refused when `credentials.issued_by` names a principal other than its holder, or when it was made by redeeming a sign-in link that someone other than its holder made. Hooks never reach a DM or call route. The rule and its one residual case are in the design (§7).
8. **No audit row names the counterparty.** Opening, sending, reading, calling and blocking write no audit row whose subject or detail names the other person. The operator's switches (Decision 9) are audited as any setting is, naming only the person switched. What operators get is aggregate counts with no principal ids.
9. **Operators get two content-blind switches:** a per-person "direct messages off" under `manage_members` (the permission of the per-person voice switch, `internal/server/routes.go:361`), and a server-wide one, on by default, set by the same class that sets `hide_members`. Neither shows the operator any content, pair or call. They are the operator's only anti-abuse tool in v1: there is no DM moderation and no "report a DM" (a follow-up; the limitation is documented).
10. **The caller sees one "unavailable"** for a callee who is busy, offline, blocked either way, opted out or switched off, so that ringing cannot be used to probe who is online or in a call.
11. **On a `hide_members` server**, existing messages keep their author's name after the pair stops sharing a channel (as ADR-013's O14 keeps authors named in channel history); new contact still needs mutual visibility.
12. **Lifecycle.** A disabled principal or a revoked credential loses access at once: sockets close, ringing stops and any call ends within the bounds in the design (§6). Conch has no server-wide ban, only per-channel bans (`channel_bans`, `internal/server/store/store.go:1039`); "removed or banned" in #2013 means a disabled principal or a revoked credential here. Reactivating the same principal resumes its history; a new principal with the same name gets none. Hiding a conversation affects only the viewer's inbox. Restore never reactivates a call: every non-terminal attempt ends `failed`. **conchd has no retention job:** a DM lasts until its author removes it, and the docs say so. Guests, if they return, can never use DMs or calls.
13. **Clients.** Browser and desktop get DMs and calls. The desktop answers through the Rust helper, which needs `voice_control` v4 (a channel-or-call destination; `pkg/schema/voice_control_v3.go:198-201` addresses a session by channel name only); that bump is signed at its own PR through the `schema-change` skill. The TUI and CLI get DMs, not calls. Phones are held (#2037). Native mobile ringing is out.
14. **API parity holds** (ADR-001, D6): every DM and call act is a REST/WS route first. No MCP tool is added: DMs are between people, and agents are refused on these routes as on other people-only routes.

### Nick's decisions (2026-11-19, on #2013)

Each is a recommendation of the review's §6, accepted as written.

| # | Question | Answer |
|---|---|---|
| 1 | A new ADR-017 amends D12, ADR-003, ADR-005, ADR-008 Decision 5 and its Unchanged line, ADR-013's locked list and ROADMAP P7 | Yes (this ADR; it also records ADR-004's room kinds) |
| 2 | The 1:1 ADR checkpoint moves out of #1501 into its own issue, the root Blocked-by | Yes (#2038) |
| 3 | Voice-only calls in v1; no camera or screen share, so ADR-015 and ADR-008 Decision 3 stand | Yes (Decision 6) |
| 4 | DM and call routes refuse act-as credentials | Yes (Decision 7) |
| 5 | No audit rows naming the counterparty; aggregate metrics only | Yes (Decision 8) |
| 6 | A per-person and a server-wide "direct messages off" switch, content-blind, the server-wide one on by default | Yes (Decision 9) |
| 7 | One "unavailable" for busy, offline and blocked | Yes (Decision 10) |
| 8 | Answering leaves channel voice after an explicit prompt; no two rooms at once in v1 | Yes (Decision 6) |
| 9 | On a hide-members server, existing DM messages keep their author's name; new contact needs mutual visibility | Yes (Decision 11) |
| 10 | Edit and author removal in v1; no reactions, threads, mentions or typed payloads | Yes (Decision 5) |
| 11 | "Report a DM" in v1 | No: a follow-up, and the limitation is documented (Decision 9) |
| 12 | Desktop calls through the Rust helper in v1, with the `voice_control` v4 bump | Yes; the bump's sign-off comes at its own PR (Decision 13) |
| 13 | Hold #2037 until ADR-016 Decision 10 is amended | Yes |
| 14 | The docs say conchd has no retention job, and DMs last until the author removes them | Yes (Decision 12) |

**Cut from v1:** camera and screen share in calls; companion identities in call rooms; reactions, threads and mentions; inbox-wide search (search is per conversation); TUI calls (TUI DMs stay); contact preferences beyond everyone / people I share a channel with / nobody; native mobile ringing.

### Gates that still apply

This ADR authorises none of the following; each keeps its own gate.

- **Each schema version bump**, signed by Nick at its own PR through the `schema-change` skill. Known now: `MessageV2` (`pkg/schema/message_v2.go:181,249`), `ReadStateV1` (`pkg/schema/read_state.go:38,54`), `FileV1` (`pkg/schema/file.go:103,124`) and `NotificationSummaryV1` with the pending response that carries it (`pkg/schema/notification.go:206,224`) all require `channel_id > 0`, so a DM cannot appear in them without a new type or a new version; `voice_control` goes to v4 (Decision 13). The closed `NotificationKind` vocabulary (`pkg/schema/notification.go:161-167`) gains a value.
- **A `security-reviewer` pass** on every child that touches authorization, LiveKit grants, files, live fan-out, or the push and pending feed.
- **Any new dependency.** None is expected: LiveKit is already in on the server and the clients. A ringtone asset needs a licence check.
- **The approval path** is not touched (Decision 5). If that changes, CLAUDE.md rule 3 applies.

## Amendments

Old text is quoted from main as of this ADR; the new text is how each line reads once this ADR is accepted. The amended files are not rewritten; each carries a dated header note pointing here.

| Where | Old text | New text |
|---|---|---|
| [ADR-000](ADR-000-charter.md) D12 (line 78) | "Scope at MVP: channels + threads. No DMs before P6 (agent DMs) / P7 (human DMs). Single-tenant." | "Scope at MVP: channels + threads. No agent DMs before P6. Local 1:1 DMs and private voice calls between two human principals are permitted ([ADR-017](ADR-017-local-direct-messages-and-calls.md)); group conversations are unscheduled (#1501). Single-tenant." The row's existing ADR-005 and ADR-003 notes stand. |
| [ADR-003](ADR-003-multi-human-access.md) Unchanged (line 23) | "Standalone human-to-human DMs stay at P7. In-channel whispers are ADR-005's subject." | "Local 1:1 human-to-human DMs and private voice calls are permitted by ADR-017; group conversations stay unscheduled. In-channel whispers are ADR-005's subject." |
| [ADR-004](ADR-004-voice-via-livekit.md) Decision (line 18) | "**One LiveKit room per net** (settled by the V0 spike, as this ADR provided; accepted by Nick, 2026-10-05). Channel-wide voice is one more room." | Unchanged, followed by: "A private call (ADR-017) is a third kind: one fresh room per call attempt, outside every channel, never in occupancy or presence, joined only by the two selected endpoints after acceptance." |
| [ADR-005](ADR-005-nets-and-whispers.md) Charter amendments (line 38) | "**D12** becomes: in-channel whispers are permitted with nets; standalone DMs remain at P6 (agents) and P7 (humans)." | "**D12** becomes: in-channel whispers are permitted with nets; standalone agent DMs remain at P6; local 1:1 human DMs are permitted by ADR-017." |
| [ADR-008](ADR-008-private-communications-alpha.md) Decision 5 (line 61) | "Separate groups are separate private channels on one server; a one-on-one is a private channel with two members (D12 holds: no standalone DMs)." | "Separate groups are separate private channels on one server. A one-on-one is a direct message (ADR-017); an existing two-member private channel stays a channel." The rest of Decision 5 (one org; a second server for harder separation) stands. |
| [ADR-008](ADR-008-private-communications-alpha.md) Unchanged (line 78) | "No standalone human DMs before P7." | "No standalone human DMs other than ADR-017's local 1:1 conversations." |
| [ADR-013](ADR-013-one-community-model.md) Amends (line 16) | "Nothing in [ADR-000](ADR-000-charter.md)'s locked table moves: single tenancy (one server is one community), no DMs, no team abstraction above the channel, and the deployment invariant are unchanged." | "Of ADR-000's locked table, single tenancy (one server is one community), no team abstraction above the channel, and the deployment invariant are unchanged. "No DMs" moved on acceptance of ADR-017: local 1:1 DMs and private calls are permitted, with the pair, not channel membership, as their content boundary." |
| ROADMAP.md P7 (the amendment quote and the paragraph on what P7 adds; main before this PR) | "The full small-team GUI, presence preferences, human DMs and OIDC login stay here." / "P7 adds what is left: presence, human DMs, per-principal permissions polish" | Edited in this PR. The ADR-008 note stays as written and is followed by a dated ADR-017 note: local 1:1 human DMs and private calls leave P7 for #2013, agent DMs stay in P6, groups stay with #1501. The paragraph reads "P7 adds what is left: presence, per-principal permissions polish". |

**Design notes** get dated notes, not rewrites: [community-model.md](../design/community-model.md) line 113 ("What stays: single tenancy, no DMs, …"), with the note after its paragraph (line 119), and [nets-and-whispers.md](../design/nets-and-whispers.md) lines 4 and 190 ("D12 (no DMs before P6/P7)"; "DMs and group DMs (P6/P7)"), each with the note on the same line.

**Not amended:**

| Where | Text on main | Why it stands |
|---|---|---|
| [ADR-015](ADR-015-webcam-video.md) Decision 1 (line 47) | camera video "in the channel-wide voice room only" | No camera in calls in v1. |
| [ADR-008](ADR-008-private-communications-alpha.md) Decision 3 (line 44) | "One person shares into a channel's voice room and the others in that room watch it." | No screen share in calls in v1. |
| [ADR-016](ADR-016-mobile-app.md) Decision 10 (line 38) | "Out of scope in this phase … DMs …" | Phones are held (#2037) until a later ADR amends it. ADR-016 Decision 3's cross-room gap (M1-4) is not closed by this ADR either: the clients avoid two rooms at once by prompting (Decision 6); a server-enforced single owner stays the future issue ADR-016 names. |

## Alternatives considered

| Option | Why not |
|---|---|
| **A hidden two-member private channel** | Reuses the most code, but channels carry operator adds (`auth.go:479-520`, `routes.go:84`), previewable membership (`routes.go:37,80`), about fifty channel routes and the admin action targets. Every one would need a DM exception, forever, and one missed exception leaks a conversation. |
| **A whisper or a net** | A whisper fixes its audience at post time inside one channel, and a net belongs to one channel (ADR-005). Neither gives a durable server-wide pair, its own read marks, or a call lifecycle. |
| **Keep D12 and point people at private channels** | That is main today; a person must create a channel and the operator can enter it. #1501's decision and #2013 reject it. |
| **Camera and screen share in calls now** | Would amend ADR-015 Decision 1 and ADR-008 Decision 3 and bring companion identities into call rooms. Cut from v1 (Nick, decision 3). |
| **Audit every DM act with the pair, as whispers are audited** | Hands `read_audit_log` holders the social graph. Rejected (Nick, decision 5). |
| **Allow act-as on DM routes and say "only by audited impersonation"** | An operator who may sign in as anyone could read any DM through the ordinary API. Rejected (Nick, decision 4). |

## Consequences

- **Storage is duplicated on purpose.** Conversation-owned message rows with their own single visibility function and a structural guard beside `TestOnlyAudienceGoReadsMessageRows` (`internal/server/store/audience_test.go:493`) and `TestMessageReadsStayInTheVisibilityFunction` (`internal/server/visibility_guard_test.go:31`); per-pair read marks with the same clamp as `MarkRead` (`internal/server/store/read_state.go:76`); explicit DM file ownership; a per-conversation full-text index. Reused: body validation and limits, pagination conventions, the blob store, and the voice-room machinery. Migrations are additive; channel tables and APIs, two-member channels and old whispers are unchanged.
- **Call rooms need their own tables.** `voice_rooms.channel_id` is `NOT NULL` (`internal/server/store/store.go:275`), the poller tracks only channel-wide rooms (`internal/server/voice_occupancy.go:29-30`), and the orphan sweep deletes any room with this instance's name shape that the store does not hold (`internal/server/voice_presence.go:2590-2613`). Call rooms therefore get their own tables, join the sweep's known set, and are kept out of occupancy and presence by a structural test.
- **Live delivery needs a per-person stream.** `/v1/events/ws` requires `?channel=` (`internal/server/events_ws.go:646-650`); a member in no channel gets no ring today. DM frames and rings need a per-person stream, and revocation (`principalDisabled`, `internal/server/disable.go:133-139`) must end it.
- **Push stays content-free** (`internal/server/push/doc.go:16`), and new DM messages reach push through one helper, as `TestNewMessagesReachPushThroughOneHelper` (`internal/server/push_send_test.go:41`) pins for channels.
- **No DM route is previewable**, by a structural test beside `TestPreviewableRoutesAreReads` (`internal/server/preview_guard_test.go:15`). No admin action targets a conversation.
- **Web cache keys change** from (channel, id) to (kind, conversation, id): channel message 42 and DM message 42 will both exist.
- **The desktop helper work is about as large as the server call work**, and it is in v1.
- **The block relation (#1832) is not on main** and blocks the eligibility child (#2018).
- **Naming:** `internal/server/calls.go` is taken (agent session calls, #267); the call code uses another name.
- **Every implementing child** gets a `security-reviewer` pass where the Gates section says so, and an isolation matrix (actor × path) is its own issue under #2013.
