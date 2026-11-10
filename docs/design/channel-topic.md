# Design: a channel's topic

- **Status:** Accepted by Nick (Tier-H), 2026-11-07, as written, §7 shapes included (recorded on #843). Previously: Proposed by the protocol-designer, 2026-11-07, for the project owner's sign-off (issue [#843](https://github.com/njdaniel/conch/issues/843); browser UX audit #699). Every decision below is a recommendation with its reason, made under the principal's delegation; nothing is left open. The schema change is additive (new optional fields and new types, no published fixture changes), so the `schema-change` skill asks no version bump; the issue asks for Nick's yes before implementation, and §7 is what he says yes to. Nothing here is implemented; no code, dependency or migration lands with this note.
- **Touches:** [channel-rename.md](channel-rename.md) (the route class, the id-addressed write, the events document and the audit row this follows), [membership-live.md](membership-live.md) (`conch.channels_changed.v1`), [community-model.md](community-model.md) §3.2 and §5 (the directory and what a private channel may never reveal), [ai-administrators.md](ai-administrators.md) (the admin door, which every manager route is dispatched through), [browser-ui-polish.md](browser-ui-polish.md) ("Centre: channel header with topic — channels have no topic in the API"), [ADR-010](../adr/ADR-010-delegated-administration.md) decision 3 (what a channel manager does in its channel), [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md) (`manage_channels`), [ADR-003](../adr/ADR-003-multi-human-access.md) (membership is the visibility boundary), [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (REST first; MCP never exceeds it).
- **Owner:** protocol-designer (this note and the shapes); server-engineer (§8.1), web-engineer (§8.2), cli-engineer (§8.3), server-engineer again for the MCP slice (§8.4).
- **Current-code baseline:** origin/main `16cc0114`, inspected 2026-11-07. `ChannelV0` has `id, name, created_at, manages, visibility, default, archived_at`; `ChannelListingV1` and `ChannelDocV1` carry the same channel fields. `PUT /v1/channels/{id}/name` is in the `accessChannelManager` class and is the admin door's `rename_channel` (gate `direct`). The directory query lists a private channel only to its members (`channels_self.go:217`) and to a `?scope=all` holder of `manage_channels`. No MCP tool lists channels; `read_channel` returns a page of messages and `next_after`.

A channel has a name and nothing else a person can read to learn what it is for. Someone who joins `#ops` from the directory, or is added to `#incident-412`, learns the purpose and the rules of the room only by asking or by scrolling. This note adds one short line, the topic, says who sets it, where it travels, and where each client shows it.

## 1. Decisions in one page

| Question | Decision |
| --- | --- |
| Name and count | One field, `topic`. Not `description`, not both (§2.1). |
| Shape | `topic` string on `ChannelV0`, `ChannelListingV1` and `ChannelDocV1`, `omitempty`; absent means no topic, on an old server as on a new one (§2.2). |
| Bound and character rule | At most `MaxChannelTopicLength = 250` characters under `ValidateDisplayName`, the rule a channel name meets, with the empty string allowed (§2.3). One line, no control or invisible characters, no leading or trailing whitespace. |
| Version | Additive; no bump (§2.4). |
| The write | `PUT /v1/channels/{id}/topic`, body `SetChannelTopicRequestV1{topic}`, answer `SetChannelTopicResponseV1{channel}`; `""` clears; same string is a 200 no-op (§3). No `from` precondition (§3.3). |
| Who | The `accessChannelManager` class, exactly as rename: an owner, an operator holding `manage_channels`, or a member holding `manages` in that channel; re-checked in the store's transaction (§4.1). Agents only through the admin door: a new `set_channel_topic` action, gate `direct` (§4.3). |
| Limits | No attempt limit (a topic is an oracle on nothing), but a write-rate bound shared with rename: a burst of 5, then one change per 10 s, per (principal, channel) (§4.2). |
| Archived channel | The write is allowed, as rename and settings are: administration of an archived channel is not "something new" in it (§3.2). |
| Audit | One row, `channel_topic_changed`, subject `channel:<id>`, detail `from=%q to=%q` (§4.4). |
| Where it travels | Everywhere the name travels and nowhere else: `GET /v1/channels`, the directory (public rows to everyone; private rows only to members and `?scope=all`), every response carrying `ChannelV0`, `conch.channel.v1` on change, `conch.channels_changed.v1` to each human member as the re-read bell (§5). Not on invites (§5.4). |
| MCP | `read_channel` output gains `topic` (omitted when empty). No new tool (§5.5). |
| Browser | Shown under the name in the conversation header and under the name in the directory; set from the channel row's ⋯ menu beside Rename (§6.1). |
| TUI | Shown in the chat pane title after the name; not set from the TUI (§6.2). |
| CLI | `conch channels topic <channel> [<text>] [--clear]` reads, sets and clears; `conch channels list --json` carries it; the text listing is unchanged (§6.3). |
| Split | Four issues: schema+server first, then web, CLI/TUI and MCP in parallel (§8). |

## 2. Name and shape

### 2.1 One field, `topic`

**Recommendation: one optional field named `topic`.**

The audit's gap is "a joiner cannot learn a channel's purpose or rules in context". One line in the header answers it, and one line is what every chat product shows there; `topic` is the word IRC, Slack and Discord use for it, and the word browser-ui-polish.md already uses for the header slot. `description` suggests a paragraph, a second place to read and a second editor; two fields double the controls, the audit detail, the tests and the questions ("which one shows in the directory?") for servers whose channels have ten people. Anything longer than a line (house rules, links to runbooks) is a pinned message, which is its own design, or a later additive `description` field; neither is blocked by choosing one field now.

### 2.2 Where the field sits

```go
// pkg/schema/channel.go (sketch; the exact Go lands in §8.1 under the
// schema-change skill)

type ChannelV0 struct {
    // ... as today ...
    // Topic is the channel's one-line topic (issue #843, additive): what the
    // channel is for, set by its managers. Omitted when it has none, and by
    // a server that predates it; a reader takes either as no topic. Held to
    // ValidateDisplayName with MaxChannelTopicLength, so it is safe in a
    // header, a terminal title and an audit line.
    Topic string `json:"topic,omitempty"`
}
```

The same field, with the same comment, on `ChannelListingV1` (so the directory shows it before a join) and on `ChannelDocV1` (so an open page learns a change live). Every response that embeds `ChannelV0` (`ListChannelsResponse`, `CreateChannelResponse(V1)`, `JoinChannelResponseV1`, `RenameChannelResponseV1`, `ChannelSettingsResponseV1`) carries it with no change to those types, through the one `channelFromStore`.

`omitempty` rather than a pointer: the empty string and absence mean the same thing, no topic, and the browser's generated types stay `topic?: string`. Nothing needs to tell "cleared" from "never set"; the audit row says when it changed.

### 2.3 The bound and the character rule

**Recommendation: `MaxChannelTopicLength = 250` characters; `ValidateDisplayName("channel topic", topic, MaxChannelTopicLength)`; the empty string is valid.**

The topic is a label, not prose. It is shown on one row in the browser header, in the TUI's pane title, in `conch channels list --json`, and quoted in an audit detail that ends up in log lines and the audit export, which is exactly the surface `ValidateDisplayName` was written for: valid UTF-8, no C0/C1/DEL, no line or paragraph separators, no strong bidi controls, no invisible format characters, no leading or trailing whitespace, every script's letters and ordinary punctuation allowed. A URL passes. Using the name rule means no new validator and no second character policy to keep in step; the only new constant is the length. Multi-line text is refused deliberately: a header that may wrap to two lines on a phone is still one line of text, and a topic with a newline in it would break the TUI title and the CLI's one-row output. (The message body is bounded differently, by `MaxPostRequestBytes` in `pkg/schema/limits.go`, and rendered as prose; that is the right rule for a message and the wrong one for a label.)

250 characters is two and a half names: enough for a sentence and a link, short enough to show whole in a desktop header and to keep the audit detail readable. No separate byte bound: 250 code points is at most 1000 bytes, well inside every body limit, and names carry none either.

Clearing: `""`. The request has one field, so an absent field and an empty one read the same, and both clear. That is stated, not accidental: there is no other field a client could mean to change.

### 2.4 Additive, no version bump

All of §7 is additive under the `schema-change` skill: a new optional field on three published types, with `omitempty`, so every existing fixture encodes byte-identically (`scripts/schema-compat.sh` proves it); two new request/response types; one new audit action and one new admin action value (vocabulary additions are additive by the rule recorded in ai-administrators.md §6). No field is removed, renamed, retyped or made required, and no published meaning changes. A client that predates the field ignores it (unknown-field tolerance is the protocol's rule); a server that predates it never sends it, and the client shows no topic. Nick's yes is asked because the issue asks it, not because a version moves.

## 3. The write

### 3.1 The route and its shapes

**Recommendation: `PUT /v1/channels/{id}/topic`, beside `PUT /v1/channels/{id}/name`.**

```go
// SetChannelTopicRequestV1 is the body of PUT /v1/channels/{id}/topic.
type SetChannelTopicRequestV1 struct {
    // Topic is the new topic, or "" to clear it. Held to the channel name's
    // character rule with MaxChannelTopicLength, checked by Validate.
    Topic string `json:"topic"`
}

// SetChannelTopicResponseV1 is the channel as it now is.
type SetChannelTopicResponseV1 struct {
    Channel ChannelV0 `json:"channel"`
}
```

Why its own route and not a field on `PUT /v1/channels/{id}/settings`: settings is a `manage_channels` route (`accessPermission`), and a channel manager who is a plain member must be able to set the topic (§4.1). One route cannot be in two classes, and widening settings to managers would hand them visibility and the default mark, which ADR-010 keeps from them. Why by id: it is the pattern of the other two administration writes on a channel (rename, settings), the admin door targets a channel by id (`tChannel`), and a stale name in the path would turn a retry into a write on whatever channel now has that name. Every other channel route stays name-addressed, as channel-rename.md §3.1 decided. The mux accepts it: `/topic` is a literal no other `PUT /v1/channels/{…}/…` route ends in.

### 3.2 Answers, in the order they are decided

| Case | Answer |
| --- | --- |
| Caller is an agent | 403 `forbidden` from the class (`humansOnlyBecause`, "agents cannot administer channels"); agents use the admin door (§4.3) |
| No channel with that id, or the caller is not a member and does not hold `manage_channels` | 404 `channel_not_found`, the same answer, before the body is read (the class) |
| Member without `manages`, operator without `manage_channels` and without `manages` | 403 `forbidden` "operator role or channel manager required", one `access_denied` row (the class) |
| Body not JSON, too large | 400 `invalid_request`, 413 `request_too_large` |
| `topic` fails §2.3 | 400 `invalid_request` naming the rule, never echoing the text |
| The channel already has that topic (same string, `""` included) | 200, nothing written, nothing audited |
| The store's standing re-check fails (`ErrManagerRefused`) | 403 `forbidden`, as rename answers it |
| Otherwise | 200 `SetChannelTopicResponseV1`, one `channel_topic_changed` row, `conch.channel.v1` to every events socket of the channel, `conch.channels_changed.v1` to every human member |

An archived channel takes the write. The archived guard refuses what is "new in the channel" (a post, an edit, a reaction, an upload, voice, a join, a member, an invite); rename and settings are not refused, and a topic is administration of the same kind. Restoring a channel is itself a settings write on an archived channel, so the line is already drawn there. Under `--auth off` the route is open like every other, and the actor is what `auditActor` gives.

### 3.3 No `from` precondition

Rename carries `from` because a name is an address other operators and scripts hold, and a rename past a colleague's rename silently breaks theirs. A topic is held by nobody and addresses nothing; two managers who set it at once get last-writer-wins, two audit rows that show both texts in order, and a header that shows the later one to both, which is what both would expect. The precondition would cost a field, an error code and a stale-state path in three clients for a race that costs nothing when it happens.

## 4. Who may set it, and the audit

### 4.1 The class

**Recommendation: `accessChannelManager`, exactly rename's gate.**

ADR-010 decision 3 lists what a manager does in its channel, and renaming is on the list; a topic is a lighter write than a rename (it changes no address, clashes with nothing) by the same person about the same room, so it belongs to the same standing. ADR-014's refinement applies unchanged: an operator holds manager standing everywhere only while holding `manage_channels`; without it, the operator is a manager where it holds `manages` and a member elsewhere, and the class answers it as such (404 for a channel it is not in, 403 for one it does not manage, never an answer that tells the two apart). An owner always holds `manage_channels`. The store's `SetChannelTopic` re-checks standing inside its IMMEDIATE transaction, as `RenameChannel` does, so a grant revoked between the class check and the write is refused (ADR-010 decision 7).

Who may not: members, guests, agents directly (§4.3), anyone who cannot see the channel.

### 4.2 No attempt limit; a write-rate bound shared with rename

Rename has `renamesPerDay` and `renameAttemptsPerDay` because a name collision is an existence oracle on private channel names (community-model §5), and the limit bounds how fast a manager can probe. A topic collides with nothing and its refusals (400 on the caller's own text, 403 on the caller's own standing) reveal nothing about anyone else, so it has no attempt limit: that limit would have no threat behind it.

A second, different threat remains (issue #1667, from the security review of #1531). Every change writes an audit row and sends `channels_changed` to each human member, whose client re-reads its channel list. No limiter covered the topic route (the invite, sign-in, reaction, report and agent-budget limiters each guard their own routes, and there is no global request limiter), and rename's daily cap is per person across all channels. A manager in a loop, or a buggy script, could fill the audit log and churn every member's client.

The bound: one token bucket per (principal, channel), shared by `PUT /v1/channels/{id}/name` and `PUT /v1/channels/{id}/topic`. A burst of 5, refilled at one change per 10 seconds (the `inviteLimit` bucket of invite_limits.go, 5 per 50 s). Why these numbers: a person setting up a room renames it and writes a topic in a handful of edits, and fixing a typo is one or two more, so 5 at once never meets the bound; after that, one change per 10 s is faster than anyone edits by hand and 360 an hour at most from a loop, against thousands a second without it. Past it the answer is 429 `rate_limited` with `Retry-After` and `retry_after_seconds`, the repository's standard shape. The check runs after the body is read and valid and before the store is touched, so a refusal writes no audit row and sends no `channels_changed`; a malformed request spends nothing, and a same-string write spends a token. The bucket is in memory only, as the other limits are: a restart refills it, the worst case being one extra burst. Another manager, or another channel, has its own bucket.

### 4.3 Agents: the admin door

No agent reaches the route: the class is humans-only, as every channel-administration route is. An agent whose manifest grants `manage_channels` (ai-administrators.md) reaches it the way it reaches rename, through `submit_admin_action`, so the catalog gains one row:

| Action | Route | Permission | Gate | Target | Params |
| --- | --- | --- | --- | --- | --- |
| `set_channel_topic` | `PUT /v1/channels/{id}/topic` | `manage_channels` | `direct` | channel | `{"topic": "..."}` (`""` clears) |

`direct`, as `rename_channel` is: the write is reversible by the same action, narrows nobody's access and reveals nothing, and the catalog gates approval on irreversible or widening actions. `changesTarget: true`, as rename, so a later gated action on the channel is checked against the channel as the decider saw it. `AdminActionSetChannelTopic` is a new value of the `AdminAction` vocabulary; `admin_catalog_test.go` requires the row, and the row must say the class is `accessChannelManager` under `manage_channels`, which it is.

### 4.4 The audit row

One row in the write's transaction: action `channel_topic_changed`, actor `principal:<id>` (or what `auditActor` gives under `--auth off`), subject `channel:<id>`, detail `from=%q to=%q`, both quoted as rename quotes names so a topic with spaces or `to=` in it reads as one field. The detail holds the text: an audit trail that said only "changed" would not let an operator see what a manager put in front of a channel's members, and the character rule (§2.3) is what makes the text safe to put in a log line. A cleared topic reads `to=""`. No row for a same-string write (nothing changed) and none for a refusal beyond the class's `access_denied`, as for rename.

What this exposes: the audit export shows private channels' topics to holders of `read_audit_log`, as it already shows their names in `channel_renamed` rows; admin-roles.md §7.6 records that exposure, and this adds a field to it, not a new reader.

## 5. Where it travels

The rule, stated once: **the topic goes exactly where the name goes, and nowhere the name does not.** No document gains a channel it did not carry, and no caller learns a topic of a channel whose name it could not already read.

### 5.1 `GET /v1/channels` and every `ChannelV0`

The caller's own channels carry `topic` (operators included; under `--auth off`, every channel, as today). `ChannelV0` is the one struct, so create, join, rename and settings responses carry it without a change to their types.

### 5.2 The directory

`ChannelListingV1.topic` on every row the caller may see. A public channel's row goes to every member, so its topic does too; that is the point of the field: a joiner reads what a channel is for before joining. A private channel's row exists only for its members and for a `?scope=all` caller holding `manage_channels` (`channelDirectory`, `channelDirectoryAll`), and the topic rides the row; a non-member gets no row, so there is nothing to leak through. The member view's "no counts and no names on a private channel" rule (community-model §5) is untouched: the topic is on the channel, not about its members. The browser fetches the directory when it is opened and that stays so; a topic changed while the directory is on screen shows at the next open, as a rename does today.

### 5.3 The events socket

`conch.channel.v1` gains `topic`, and the handler sends it on every events socket of the channel after a change (`notifyChannel`, as rename and settings do). It is a whole-state document, so a reader that misses one and later reads `GET /v1/channels` is in the same place; it is sent only on change, never on connect, so no existing reader sees anything new unless a topic changes. The socket is bound to the channel id and its holders are members, so the audience is right by construction.

`conch.channels_changed.v1` goes to every human member (`channelRenamedLists`, the same call rename and settings make): it is content-free, and it means "re-read `GET /v1/channels`". It is sent because the topic is a field of the list, and a client that holds the list but has no events socket on the channel (the TUI; a browser tab on another channel) learns the change that way. One re-read per member per change; topics change seldom.

### 5.4 Not on invites

`InviteChannelV1` carries a channel's name for the invitee's preview and does not gain a topic. An invitee is not yet a member; the preview is what the inviter chose to show, and the invitee reads the topic on landing, from `GET /v1/channels`. Adding it is one additive field later if testers want it; leaving it out now keeps the invite shapes still.

### 5.5 MCP

**Recommendation: `read_channel`'s output gains `topic` (string, omitted when empty); no new tool.**

An agent is told a channel's rules the same way a person is, in context: the first `read_channel` of a session returns the topic beside the page. It is the agent-side answer to the audit finding, and it is additive to the tool's output object (`mcpListMessagesOutput`), which readers of unknown fields ignore. Parity holds (ADR-001; CLAUDE.md rule 4): the agent could read the same field from `GET /v1/channels` with its own credential, so MCP gives nothing REST does not.

No `list_channels` agent tool: none exists today, agents address channels by the names in their manifest, and a listing tool is its own design question (which channels an agent may enumerate) that #843 does not need answered. The admin door's `list_channels` action (`GET /v1/directory/channels?scope=all`) returns `ChannelListingV1` rows and so carries the topic with no change.

## 6. Clients

### 6.1 Browser

The conversation header (`conv-head`) shows the topic as plain text under the `<h2>` name, nothing when there is none. React renders it as a text node; no `dangerouslySetInnerHTML`, no linkification in this slice (a link in a topic is text; making it clickable needs the same URL handling messages have and is a follow-up, not a leak). The directory (`ChannelDirectory.tsx`) shows it under each row's name, where a joiner reads it before Join. Setting: a "Set topic" entry in the channel row's ⋯ menu beside Rename, shown to the same callers (`role === 'operator' || channel.manages`), opening a small form in the pattern of `RenameChannel.tsx`: the current topic prefilled, a 250-character bound in the control, one send, refusals mapped (`invalid_request` to the rule, `forbidden` to "you no longer manage this channel", network failure kept with the typed text), a Clear button sending `""`. Live: the `conch.channel.v1` case in `frames.ts` hands `topic` up and the channels reducer gains a `topic` action (as `renamed`); `conch.channels_changed.v1` re-reads the list as it does today, so the sidebar and header agree after a reload or a missed frame.

### 6.2 TUI

`chatTitle` shows `# name · topic` when a topic is set, truncated to the pane width and passed through `sanitize` (the server refuses what `sanitize` strips, but an older server did not store topics and a newer one might loosen the rule; the TUI defends its own terminal). The topic comes from the `ChannelV0` the TUI already holds for the list and is refreshed on `conch.channels_changed.v1` as the list is. The TUI does not set topics: it has no rename or settings control either, and administration is the plain CLI's.

### 6.3 CLI

`conch channels topic <channel>` prints the topic (one line, `safeLine`, empty output when none); `conch channels topic <channel> <text>` sets it; `conch channels topic <channel> --clear` clears it. It lives in the `channels` group, not `admin`, because a manager who is a plain member may set it. The read resolves the channel by name from `GET /v1/channels` as `join` does, and the write sends `PUT /v1/channels/{id}/topic`; refusals print the server's one line and exit nonzero, as the other `channels` verbs do. `conch channels list --json` carries `topic` on every row since it prints the server's document; the text listing is unchanged, because its last column is the name, which may contain spaces, and scripts take the rest of the line as the name.

## 7. Published-shape changes for the owner's sign-off

All additive: new optional fields with `omitempty`, new types, new vocabulary values; no published field touched; every existing fixture byte-identical.

| Change | Where |
| --- | --- |
| `MaxChannelTopicLength = 250` | `pkg/schema/channel.go`; `pkg/schema/ts.go` constants table so the browser bounds its control from the same number |
| `ChannelV0.Topic string json:"topic,omitempty"` | `pkg/schema/channel.go`; new fixture `channel-v0-topic.json` |
| `ChannelListingV1.Topic`, the same | the same; new fixture `channel-listing-v1-topic.json` |
| `ChannelDocV1.Topic`, the same; `Validate` applies §2.3 when non-empty | the same; new fixture `channel-v1-topic.json` |
| `SetChannelTopicRequestV1{topic}` and its `Validate` (§2.3, empty allowed) | the same; fixtures `set-channel-topic-request-v1.json`, `set-channel-topic-request-v1-clear.json`; `ts.go` |
| `SetChannelTopicResponseV1{channel: ChannelV0}` | the same; fixture `set-channel-topic-response-v1.json`; `ts.go` |
| `AdminActionSetChannelTopic = "set_channel_topic"` | `pkg/schema/admin_action.go` (closed vocabulary, one value added); the catalog fixture that lists actions, if one does |
| Audit action `channel_topic_changed` | `internal/server/store` (not a wire shape; listed so the audit vocabulary stays complete) |
| `read_channel` output: `topic string json:"topic,omitempty"` | `internal/server/mcp.go` (`mcpListMessagesOutput`), synced in §8.4 |

Not changed: `ChannelSettingsRequestV1`, `RenameChannelRequestV1`, `InviteChannelV1`, any socket's first frame, any error code vocabulary (the refusals reuse `invalid_request`, `forbidden`, `channel_not_found`), the `SocketCloseReason` vocabulary.

## 8. The split

Four issues. The first ships the schema and the server together (the skill's "sync consumers in the same PR" rule binds the server's use of the shapes); the other three are blocked by its merge and are disjoint by area, so they dispatch in parallel. Each references #843 and this note. The first gets a `security-reviewer` pass (a new manager route, a new admin-door row, an isolation row). The approval flow itself does not change, but 8.1 edits `internal/server/admin_actions.go` and 8.4 edits `internal/server/mcp.go`. Both paths are in `process-checks.yml`'s approval-path list, so those two PRs carry the `approval-path` label and Nick merges them.

### 8.1 Schema and server: the topic field, `PUT /v1/channels/{id}/topic`, audit, events, admin door

Labels: `area/schema`, `area/server`, `security`.

**Context.** docs/design/channel-topic.md (this note) is the design; §7 lists the shapes, §3 the route and its answers, §4 the class, the admin-door row and the audit row, §5 the documents that carry the field. Nick's sign-off on §7 is recorded on #843 before dispatch. The channel-rename route (`handleRenameChannel`, `store.RenameChannel`, `channelRenamedLists`) is the pattern to copy; the topic has no `from`, no name keys and no attempt limit (§3.3, §4.2). Administration of an archived channel is allowed (§3.2).

**Files.**
- `pkg/schema/channel.go` (field on three types, `MaxChannelTopicLength`, the two new types and `Validate`), `pkg/schema/admin_action.go` (`AdminActionSetChannelTopic`), `pkg/schema/ts.go` (the constant and the two types), `pkg/schema/testdata/` (the six new fixtures of §7), regenerated `web/src/schema/*.ts` (`go generate ./pkg/schema`, never by hand), `pkg/schema/channel_test.go`, `golden_test.go`.
- `internal/server/store/store.go` (migration, next free number: `ALTER TABLE channels ADD COLUMN topic TEXT NOT NULL DEFAULT ''`; no audit row, no fill step), `internal/server/store/queries.go` (`Channel.Topic`, read in every channel SELECT: `ChannelByID`, `ChannelByName`, `ListChannelsForPrincipal`, `channelDirectory`, `channelDirectoryAll`), `internal/server/store/channels_topic.go` (new: `SetChannelTopic(ctx, actor, callerID, id, topic, now) (ChannelTopicResult, error)` in one IMMEDIATE transaction with the manager re-check, same-string no-op, `AuditChannelTopicChanged`).
- `internal/server/channels.go` (`handleSetChannelTopic`; `channelFromStore` and the listing projection carry `Topic`), `internal/server/routes.go` (`rt("PUT /v1/channels/{id}/topic", accessChannelManager, …)`), `internal/server/events_ws.go` (the `conch.channel.v1` document carries `Topic`), `internal/server/admin_actions.go` (the `set_channel_topic` row, `buildSetChannelTopic` taking `{"topic"}` with unknown fields refused), `e2e/isolation/matrix.go` (a `byChannel("PUT /v1/channels/{id}/topic", …)` row: B sets A's channel's topic by id, wants `channel_not_found`).

**Acceptance criteria.**
- [ ] `scripts/schema-compat.sh` passes with every pre-existing fixture byte-identical; the six new fixtures round-trip; `ChannelV0{}` still encodes without a `topic` key.
- [ ] `SetChannelTopicRequestV1.Validate` accepts `""` and a 250-character topic; refuses 251 characters, a newline, a tab, U+202E, U+200B, leading or trailing whitespace and invalid UTF-8, each with an error naming the rule and not the text.
- [ ] An owner, an operator holding `manage_channels`, and a member holding `manages` in the channel each get 200 with the channel carrying the new topic; the same string again is 200 with no new audit row; `""` clears and the response omits `topic`.
- [ ] A member without `manages` gets 403 `forbidden` and one `access_denied` row; a non-member gets 404 `channel_not_found` before the body is read; an agent gets 403 from the class; an operator without `manage_channels` gets 404 in a channel it is not in and 403 in one it is in without `manages`; an unknown id is 404; a bad topic is 400 `invalid_request`; an oversized body is 413.
- [ ] An archived channel takes the write (200).
- [ ] The audit row is `channel_topic_changed`, actor `principal:<id>`, subject `channel:<id>`, detail `from=%q to=%q` with both quoted; a cleared topic reads `to=""`.
- [ ] After a change, every open events socket of the channel receives exactly one valid `conch.channel.v1` carrying the new topic and stays open; sockets of other channels receive nothing; every human member's events socket receives `conch.channels_changed.v1`; `/v2/ws` and `/v1/voice/ws` receive nothing.
- [ ] `GET /v1/channels` carries `topic` on the caller's channels; `GET /v1/directory/channels` carries it on public rows for a non-member and on private rows only for members and `?scope=all`; a non-member's directory response contains neither the private channel's name nor its topic.
- [ ] `describe_admin_actions` lists `set_channel_topic` (`manage_channels`, not gated); an agent holding `manage_channels` runs it through `submit_admin_action` with `{"topic": "…"}` and the channel's topic changes with the agent as audit actor; an agent without the permission is refused; params with an unknown field are refused.
- [ ] Nothing in the pinned audit trails of `e2e/p2-hardening`, `e2e/p3-local` and `e2e/dogfood` moves.

**Required tests.**
- `pkg/schema`: golden tests for the six fixtures; table-driven `Validate` tests for the request and `ChannelDocV1` (the cases above); `ts_test` for the constant.
- `internal/server/store/channels_topic_test.go`: transaction, no-op, clear, manager re-check refused, archived allowed, audit row fields and quoting, the migration on a store with existing channels (topic reads `""`).
- `internal/server/channels_test.go`: the handler table above; after a topic change the channel's id, name, members, messages, read marks and voice room are unchanged.
- `internal/server/events_ws_test.go`: the two documents, their recipients and the untouched sockets.
- `internal/server/channels_directory_test.go` (or where the directory tests live): the three visibility cases.
- `internal/server/admin_door_test.go` / `admin_catalog_test.go`: the new row, its class and permission, the dispatch, the refusals.
- `go run ./e2e/isolation` with the new row; `make check`; `go test -race ./...`; the three pinned-trail runs.

**Blocked by.** Nick's sign-off on §7 of docs/design/channel-topic.md, recorded on #843. Nothing else.

### 8.2 Web: show and set the topic

Labels: `area/web`.

**Context.** channel-topic.md §6.1. The generated types from 8.1 give `topic?: string` on `ChannelV0`, `ChannelListingV1` and `ChannelDocV1` and the `MAX_CHANNEL_TOPIC_LENGTH` constant. `RenameChannel.tsx` and its ⋯ menu wiring in `App.tsx` (`canRename`, `renderRename`) are the pattern; `frames.ts` and the channels reducer already handle `conch.channel.v1` for the name. Topics render as text: no `dangerouslySetInnerHTML`, no inline style, no linkification.

**Files.** `web/src/api.ts` (`setChannelTopic(id, topic)`), `web/src/agents/frames.ts` (hand `topic` up), `web/src/state/channels.ts` (`topic` action; `settings` and `renamed` keep it), `web/src/Conversation.tsx` (the header line), `web/src/ChannelDirectory.tsx` (the row line), `web/src/ChannelList.tsx` and `web/src/App.tsx` (the ⋯ entry, `canSetTopic`, `renderTopic`), `web/src/SetTopic.tsx` (new), `web/src/app.css`; tests beside each; a new `web/e2e/NN-channel-topic.spec.ts`.

**Acceptance criteria.**
- [ ] A channel with a topic shows it under the name in the header; one without shows nothing there; the text `<b>x</b> & y` shows literally as text with no element created.
- [ ] The directory shows each listed channel's topic under its name, for a non-member of a public channel too.
- [ ] The ⋯ menu shows "Set topic" to an operator and to a manager of that channel and to nobody else; the form prefills the current topic, bounds input at `MAX_CHANNEL_TOPIC_LENGTH`, sends once, and on success the header, sidebar and directory show the new topic without a reload; Clear sends `""` and the header line disappears.
- [ ] `invalid_request`, `forbidden` and a network failure each show their own line and keep the typed text; the form never sends twice.
- [ ] A second tab open on the channel shows the new topic on the `conch.channel.v1` frame without a reload; a tab on another channel shows it in the sidebar after the `channels_changed` re-read; a reload shows it.
- [ ] `make web-check` passes; the generated schema files are untouched by hand (`scripts/schema-ts-check.sh`).

**Required tests.** Unit: `Conversation` header with and without a topic and with hostile text (`textContent` equals the string; no child element); `ChannelDirectory` row; `SetTopic` success, clear, 400, 403, network, no double send; `channels` reducer `topic` action and that `renamed`/`settings` preserve it; `frames.ts` parses `topic` and still returns null for an unknown schema. Playwright: a manager sets a topic; a member's second tab updates live; reload; the directory shows it to a non-member of a public channel; a private channel and its topic are absent from a non-member's directory; a plain member has no control. Async renders found with `await findBy…`.

**Blocked by.** 8.1 (merged).

### 8.3 CLI and TUI: read, set, clear and show the topic

Labels: `area/cli`.

**Context.** channel-topic.md §6.2 and §6.3. The `channels` group (`internal/cli/channels.go`: list, join, leave, create) gains a `topic` verb; the TUI's `chatTitle` (`internal/cli/tui/view.go`) gains the topic after the name. The TUI sets nothing. The text output of `conch channels list` does not change.

**Files.** `internal/cli/channels.go` (the verb, its refusal table entry, `channelsOutput.topic`), `internal/cli/client_channels.go` (`SetChannelTopic`), `internal/cli/tui/view.go` (`chatTitle`), `internal/cli/tui/model.go` (the channel's topic from the list it holds), tests beside each.

**Acceptance criteria.**
- [ ] `conch channels topic ops` prints the topic on one line through `safeLine`, or nothing and exit 0 when there is none; an unknown channel exits nonzero with one line.
- [ ] `conch channels topic ops "Incidents and their follow-ups"` sends `PUT /v1/channels/{id}/topic` with the channel's id resolved by name and prints the server's channel line; `--clear` sends `""`; `--clear` with a text argument is a usage error.
- [ ] A 400, 403 or 404 prints the server's one error line and exits nonzero; `--json` prints the response document.
- [ ] `conch channels list` text output is byte-identical to today for a channel with a topic; `conch channels list --json` carries `topic` as the server sent it.
- [ ] The TUI chat pane title reads `# ops · Incidents and their follow-ups` when a topic is set and `# ops` when not; the title is truncated to the pane width and a topic containing a control character or a bidi control is shown with them stripped; the title updates after a `conch.channels_changed.v1` re-read.

**Required tests.** Table-driven tests for the verb (print, set, clear, usage error, each refusal, `--json`) against a fake server; a test that pins the text listing's format; TUI `chatTitle` tests for set, unset, truncation and hostile text; `make check`.

**Blocked by.** 8.1 (merged).

### 8.4 MCP: `read_channel` carries the topic

Labels: `area/server`.

**Context.** channel-topic.md §5.5. Parity (ADR-001, CLAUDE.md rule 4): the field is one an agent can already read from `GET /v1/channels`; this puts it where an agent reads a channel. The output type is `mcpListMessagesOutput` in `internal/server/mcp.go`; the channel is already resolved in `readChannelMCP`'s scope, so no extra read is needed.

**Files.** `internal/server/mcp.go` (`mcpListMessagesOutput.Topic string json:"topic,omitempty"`; `readChannelMCP` returns the channel's topic; the tool description says a `topic`, when present, is the channel's one-line purpose set by its managers), `internal/server/mcp_projection_test.go` and `mcp_test.go`.

**Acceptance criteria.**
- [ ] `read_channel` on a channel with a topic returns `topic` beside `messages` and `next_after`; on a channel without one the key is absent; the tool's output schema lists it as optional.
- [ ] The agent's capability check is unchanged: an agent without `read` in the channel is refused as today and learns no topic.
- [ ] No other tool's input or output changes; `make check` passes.

**Required tests.** `mcp_test.go`: topic present, absent, and after a change by a manager (the next page carries the new one); `mcp_projection_test.go` keeps the projection complete; an agent refused on the channel sees no topic in the error.

**Blocked by.** 8.1 (merged).

## 9. Follow-ups, not in these issues

- A `description` (multi-line, longer) if testers want more than a line; additive, same class, its own note.
- A topic on `InviteChannelV1` previews (§5.4), if invitees ask what they are being invited to.
- Links in a topic made clickable in the browser, with the URL handling messages get.
- A "topic changed" system line in the channel's history, so members who were away see that it moved; that is a message kind, and messages are the message design's to grow.
