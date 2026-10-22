# Design: renaming a channel

- **Status:** Accepted by the principal, 2026-10-21 (issue #696, the design half), including the `from` precondition (§3.2, §3.6) and name keys for channels (§3.5). The published shapes in §5 (two request and response types, one socket document, one error code) still need the project owner's yes before the server issue is dispatched. Nothing here is implemented; no code, dependency or migration lands with this note.
- **Touches:** [private-alpha.md](private-alpha.md) O3 (names) and §3 (one server, each group in its own private channels), [ADR-003](../adr/ADR-003-multi-human-access.md) (membership is the visibility boundary), [ADR-008](../adr/ADR-008-private-communications-alpha.md) §5, the principal rename of #441 (`internal/server/store/rename.go`), #695 (channel creation in the browser, the same list) and #697 (who may rename beyond operators).
- **Owner:** protocol-designer (this note and the shapes); server-engineer (child a, §6.1); web-engineer (child b, §6.2).

An operator can create a channel and nobody can rename one. The channel's name is its address on every route, socket query, browser link and CLI argument, so a rename done naively breaks every held URL, every open socket and every script. Underneath, identity is already stable: every table that refers to a channel does so by `channels.id`, and the voice room's name is random, never derived from the channel's. This note says how a rename works on top of that, in plain terms: one id-addressed route, one new frame for open sockets, a 404 for the old name, the look-alike name rule of #441 applied to channels, a stated answer for two renames at once, operators only, and one audit row.

## 1. What this adds, and what it does not

Adds: `PUT /v1/channels/{id}/name` (operators only), a `RenameChannelRequestV1` and `RenameChannelResponseV1`, a `conch.channel.v1` document that the events socket sends when a channel is renamed under it, the error code `channel_name_stale`, name keys on `channels` (a migration) so that channel names are unique by how they read, the audit action `channel_renamed`, and a rename control in the browser's channel list for operators.

Does not add: channel delete or archive, a description, member-initiated rename (#697), a CLI command (a follow-up, §7), id addressing on any other channel route (§3.1), an alias or redirect for the old name (§3.4), a change to any published type or fixture, an MCP tool (§3.7), a dependency.

## 2. Facts on main, verified at 900b43a

Everything the issue states holds, with these refinements:

- **No rename exists.** `internal/server/routes.go` has `POST /v0/channels` and `GET /v1/channels` and nothing that writes `/v1/channels/{channel}` itself; `internal/server/store/` has no channel rename. `CreateChannelRequest.Validate` (`pkg/schema/channel.go`) is `ValidateDisplayName` with `MaxChannelNameLength` 100 and a non-blank rule, "enforced only on creation".
- **The name is the address.** Every `{channel}` route resolves it by exact spelling (`store.ChannelByName`, `queries.go:198`; `channelForCaller`, `members.go:53`). `/v2/ws`, `/v1/events/ws` and `/v1/voice/ws` take `?channel=<name>`; `GET /v1/sessions` and `GET /v1/runs` take `?channel=<name>`; the browser's route is `#/c/<name>` (`web/src/route.ts`) and its state keys the selected channel, drafts, chat history and unread counts by name; the MCP tools `post_message` and `read_channel` take a channel name, `request_approval` a `channel_id`.
- **Identity underneath is by id.** Every table that refers to a channel does so by `channels (id)`, and none stores a channel name but `channels` itself: `messages` (and their FTS column `fts_channel`, `'c' || channel_id`), `channel_members`, `nets`, `voice_rooms` (a random `room_name`, `store/voice.go`), `invite_channels`, `hooks`, `approvals`, `read_marks`, `files`, `runs`. Agent manifests allow channels by id (`AllowsChannel(channelID, …)`, `internal/server/authz.go`). Every open socket is bound to a channel id once accepted (`hub.SubscribeV2(channelID, …)`, `eventsHub.subscribe(channel.ID, …)`, `voice.subscribe(ch.ID, …)`); a rename does not close or move any of them.
- **The pattern to follow is #441.** `PUT /v1/principals/{id}/name` (`routes.go:31`, `accessOperatorOrSelf`), `store.RenamePrincipal` in one IMMEDIATE transaction: `claimNameTx` (keys from `internal/server/names`, `ErrNameInvalid`, `ErrDuplicate`), `setNameTx` (the UPDATE and the `principal_renamed` row, detail `from=%q to=%q`), same-string is a no-op. Migration 27 added `name_key` and `name_skel` to `principals` with insert and update triggers that require them, and `fillNameKeys` computed them for existing rows and logged clashing groups without renaming anyone. The handler then calls `s.events.notifyRosters()` so open events sockets resend their member lists.
- **The events socket is where new document kinds go.** It sends one `pkg/schema` document per frame, told apart by `schema`, and "its readers already skip a schema they do not know" (`pkg/schema/heartbeat.go`; `web/src/agents/frames.ts` returns null for an unknown schema; the TUI's `dispatchEvent` in `internal/cli/client.go` returns nil). `/v2/ws` carries bare message envelopes, and a frame that is not a message goes only to a client that asked for heartbeats.
- **The browser today.** `useLiveChannel` opens `/v2/ws` for the open channel; `useAgents` opens the events socket only once the browser's own session exists; `useVoice` holds the presence socket of the channel on screen and a `VoiceClient` that keeps the joined channel's *name* for `createVoiceSession`, transmit reports and the share. A `channel_not_found` from a REST call is a plain failure; on a 1008 close the page re-lists channels and, when the name is gone, treats the channel as lost (`onLost`).

## 3. The six questions

### 3.1 The route: id-addressed, and only this one

**Recommendation: `PUT /v1/channels/{id}/name`, beside `PUT /v1/principals/{id}/name`. Every other channel route stays name-addressed.**

Why by id: the rename is the one write whose address is the thing it changes. Addressed by name, a retried or raced request means "rename whatever is now called X", and after a swap that is another channel; addressed by id it always means the same channel. It is also what the issue and #441 already chose. The mux accepts it: `{id}` and `{channel}` sit in the same segment, but `/name` is a literal no other `PUT /v1/channels/{channel}/…` route ends in (`read-state`, `members/{principal_id}`, `nets/…`), so no two patterns overlap.

Why not id addressing more broadly: 22 routes, three sockets, two query parameters, the MCP tools, the TUI and every Playwright spec address channels by name, and nothing in the alpha needs them by id. A client that holds the list from `GET /v1/channels` has both. The one gain, a link that survives a rename, is deliberately not taken (§3.4). If id addressing comes later it comes as new routes under a new prefix, not by changing what `{channel}` means; this route does not get in the way.

Unknown id: 404 `channel_not_found`, the same answer as for an id that never existed, as the isolation matrix requires. Only operators reach the handler, so the answer tells nobody anything.

### 3.2 The request and the response

```go
// pkg/schema/channel.go (sketch; the exact Go lands in child a under the
// schema-change skill)

// RenameChannelRequestV1 is the body of PUT /v1/channels/{id}/name.
type RenameChannelRequestV1 struct {
    // Name is the new name: the rule a name meets on creation
    // (CreateChannelRequest.Validate), checked by Validate.
    Name string `json:"name"`
    // From is the name the caller believes the channel has now. The server
    // refuses the rename (409 channel_name_stale) when it is not, so two
    // operators working from different lists cannot rename past each other.
    // Required; the same string as Name is allowed (a no-op).
    From string `json:"from"`
}

// RenameChannelResponseV1 is the channel as it now is.
type RenameChannelResponseV1 struct {
    Channel ChannelV0 `json:"channel"`
}
```

`Validate` applies the create rule to `Name` and requires `From` to be non-blank; it does not validate `From` further, since it is compared, never stored. Fixtures: `rename-channel-request-v1.json`, `rename-channel-response-v1.json`; both types go in `pkg/schema/ts.go` so the browser gets them generated.

`From` is the one place this differs from the principal rename, on purpose: a person's name is theirs alone and an operator renaming one is cleanup, whereas a channel's name is an address that other operators and scripts hold. The precondition costs one string compare inside the transaction that already holds the row. It mirrors `started_at` on `DELETE /v1/channels/{channel}/voice/share` (409 `voice_share_stale`).

Answers, in the order they are decided:

| Case | Answer |
| --- | --- |
| Caller is not an operator (under AuthRequired) | 403 `forbidden` from the route class, audited `access_denied`, before the body is read |
| Body not JSON, too large | 400 `invalid_request`, 413 `request_too_large` (as every handler) |
| `name` fails the create rule; `from` blank | 400 `invalid_request` |
| No channel with that id | 404 `channel_not_found` |
| The channel is already called `name` (same string) | 200, nothing written, nothing audited; `from` is not checked, so a retry of a rename that already happened is idempotent |
| `from` is not the channel's current name | 409 `channel_name_stale`, nothing written |
| `name` has no character a reader can see (`ErrNameInvalid`) | 400 `invalid_request` |
| Another channel holds either key of `name` (`ErrDuplicate`) | 409 `channel_exists`, nothing written |
| Otherwise | 200 `RenameChannelResponseV1`, one `channel_renamed` row, every events socket of the channel gets `conch.channel.v1` |

Under `--auth off` the route is open like every other and the actor is whatever `auditActor` gives.

### 3.3 Open sockets: one new document on the events socket

**Recommendation: a `conch.channel.v1` document, the channel as it now is, sent on every events socket of the channel when its name changes. No socket is closed. `/v2/ws` and `/v1/voice/ws` send nothing.**

```go
// pkg/schema/channel.go (sketch)

// ChannelSchemaV1 names the document the events socket sends when the
// channel it follows changes: today, when it is renamed.
const ChannelSchemaV1 = "conch.channel.v1"

// ChannelDocV1 is the channel as it now is. Like the roster and the member
// list it is the whole state, not a delta: a reader that misses one and
// later reads GET /v1/channels is in the same place.
type ChannelDocV1 struct {
    Schema    string    `json:"schema"`     // always ChannelSchemaV1
    ChannelID int64     `json:"channel_id"` // as every events document names its channel
    Name      string    `json:"name"`
    CreatedAt Timestamp `json:"created_at"`
}
```

It is sent only on change, never on connect, so the first frame of an events socket is the roster as before and no existing reader sees anything new unless a rename happens. The reader knows which name it opened the socket with, so the document need not carry the old name; a client with a list finds the entry by `channel_id` and replaces its name. The audit row holds both names for people.

Alternatives considered:

- *A `channel_renamed` event with `previous_name`.* A delta. The events socket's rule is whole-state documents (roster, member list, run), and a state document is what a later field (a description, an archived_at) also fits. The name `conch.channel.v1` says what it is, not what happened.
- *The same frame on `/v2/ws`.* It is the socket every chat client always holds, but it carries bare message envelopes by design; the heartbeat had to be opt-in because every older reader takes each frame for a message. One carrier is enough, and the events socket is the one built for documents.
- *Close every socket of the channel with a new close reason and let clients re-list.* One hub call on the server, but it drops three sockets per tab for everyone in the channel at once, the voice strip blinks to "loading", and the browser's 1008 path does a whoami and a list per tab. Reconnect churn for an event that changes one string.
- *Nothing: clients find out at their next 404.* Slow and silent; and the voice client's next session request after a reconnect would be answered `channel_not_found` and stop voice (§4).

What the server does in the handler after a successful rename: `s.events.notifyChannel(channel.ID, eventRef{kind: eventChannel})`, a fourth `eventKind` beside run, activity and intervention, whose `sendRef` reads the channel by id and sends the document. A socket whose channel was renamed twice quickly sends the current state twice; harmless.

The gap: a browser opens its events socket only once it has a session, so for a few seconds after sign-in, and while the server is replacing a session, a tab can miss the frame. Child b closes it the slow way (§4): a `channel_not_found` for the open channel makes the page re-list channels and find its channel by id.

### 3.4 Old links, invites and scripts: the old name is gone

**Recommendation: a request naming the old name gets 404 `channel_not_found`, the same as a name that never existed. No redirect, no alias.**

- *Browser links* `#/c/<old>`: the page lists channels, finds no such name and lands where it lands today when the hash names nothing it can see (the remembered channel, else `#general`, else the first). That is the existing rule, unchanged. A friendlier "that channel was renamed" needs the page to know old names, which the server deliberately does not tell it (below). Tabs that were open move by §3.3.
- *Invites* store channels by id (`invite_channels`) and preview them by joining `channels` (`inviteChannelsTx`), so an invite made before the rename grants the renamed channel and its preview shows the new name. Nothing to do.
- *Scripts and the TUI* that hold the old name get `channel_not_found` on their next call and must be updated; the audit row (`channel_renamed`, `from=… to=…`) is where an operator finds out what the name became. The browser is the alpha's client (ADR-008 §1); a `conch admin channel rename` command is a follow-up (§7).
- *Agents* over MCP address `post_message` and `read_channel` by name and get the same `channel_not_found`. An agent is told the same for a channel it is not a member of, so this reveals nothing new.

Why no redirect or alias: a redirect from an old name to a new one, or an alias that keeps answering for a while, must either answer non-members too (and then anyone can learn that a channel existed and what it is called now, against ADR-003 and the isolation suite's first rule: a refusal must not tell B that A's object exists) or be gated on membership, and then it is a second lookup with a lifetime, a table, a sweep and the question of what happens when a new channel takes the old name. Operators rename channels seldom, and a name that is wrong for a few people for a few seconds is cheaper than any of that.

Consequence to state plainly: the old name is free the moment the rename commits (by key, §3.5), so an operator who renames `ops` to `ops-old` and then creates `ops` has pointed every held `#/c/ops` link at the new channel. That is the operator's intent when they do it; nothing stops it and nothing need.

### 3.5 Name keys: #441's rule applies to channel names

**Recommendation: yes. Channel names become unique by their keys (case-folded, and look-alike skeleton), not by spelling, for creation and rename alike.**

A channel name is an address people read in links and invites, and the look-alike risk is the same as for a person: an invite to `#general` with a capital I for the l is a different channel today. The rule and the code already exist (`names.KeysOf`, `claimNameTx`, `clashTx`); what changes is a migration and two inserts:

- Migration (the next free number): `ALTER TABLE channels ADD COLUMN name_key TEXT`, `name_skel TEXT`, two indexes, the insert and update triggers that require both (as migration 27), and a Go step that fills them for existing channels and logs clashing groups and blank-key names, keeping all as they are. The step writes no audit row, so the pinned audit trails of `e2e/p2-hardening`, `e2e/p3-local` and `e2e/dogfood` do not move.
- `CreateChannelAs` and the test seed `CreateChannel` compute and store the keys; a create whose keys clash is 409 `channel_exists` (today only the exact spelling is). `ChannelByName` keeps matching by exact spelling: uniqueness is by key, lookup is by what was typed, as for principals.
- `claimNameTx` and `clashTx` are generalised to take the table (or a channel twin of each is written; the engineer's call, kept small). `reservedName` (the `hidden-` prefix) is a principal rule and does not apply; nor does ADR-009's `guest-` prefix.

Alternative: *spelling-only uniqueness for channels, keys for principals.* Smaller, but then a rename could put `Ops` beside `ops`, and the rename clash rule would differ from the create clash rule. One rule for every name on the server is easier to state.

Consequence: a server whose existing channels already clash by key keeps them, as #441 did for principals; a rename of either to a name clashing with the other is refused. The log line at migration time says which.

### 3.6 Two renames at once

Every rename runs in one IMMEDIATE transaction (`withImmediateTx`), so SQLite serialises them and the second always sees the first's row. Three cases, each explicit:

- *Two channels renamed to the same name:* the first commits; the second finds the key taken and gets 409 `channel_exists`. Nothing of the second is written.
- *One channel renamed by two operators to different names:* the first commits; the second's `from` no longer matches and it gets 409 `channel_name_stale`. Nothing of the second is written. The operator re-lists, sees the new name, and decides again.
- *One channel renamed by two operators to the same name:* the first commits; the second finds the channel already so called and gets 200 with nothing written (the same-string rule, checked before `from`).

Without `from` the second case would be last-writer-wins with two audit rows, which is explicit too, but it lets an operator working from a stale list undo a colleague's rename without knowing. The required tests in the issue ("concurrent renames: one wins, the other 409") are these three.

### 3.7 Who may rename, and the audit row

**Operators only, now:** the route is `accessOperator`, so a member is refused by the route class with 403 `forbidden` and an `access_denied` row before the body is read or the channel looked up, as every operator route is. Agents are humans' tools and never operators, so no MCP tool exists or is needed; parity holds (the browser calls the same REST route a script would). Who else may rename (a delegated administrator, a channel's creator) is #697's matrix; when it decides, this route changes class or grows a handler check, and nothing else here moves.

**Audit:** one row in the rename's transaction, `channel_renamed`, actor the caller (`principal:<id>`, or what `auditActor` gives under `--auth off`), subject `channel:<id>` (as `channel_created` and the voice rows name a channel), detail `from=%q to=%q`, both quoted as the principal rename quotes them so a name with spaces or `to=` in it reads as one field. No row for a refusal beyond the route class's `access_denied`: a `channel_exists` or `channel_name_stale` tells an operator nothing the channel list does not, so there is nothing to count, unlike a member's `principal_rename_refused`.

## 4. The browser (child b)

What moves on a rename, whether learned from the operator's own response, from `conch.channel.v1`, or the slow way:

- The channel list entry (by id), the selected name, the hash (`#/c/<new>`; replacing the history entry rather than pushing one is the web engineer's call, with `hygiene.test.ts` holding the page to its one `replaceState` site today), the remembered channel, and the keys of chat history, drafts and unread counts (`web/src/state/*`). A new `renamed` action on the channels reducer, and the equivalent where state is keyed by name.
- The sockets stay: `useLiveChannel` and `useAgents` must key their effects so a name change under the same id does not reload history or reopen the socket; the next REST call simply uses the new name.
- Voice is not touched: the LiveKit room is the same and the presence socket is bound by id. But `VoiceClient` keeps the joined channel's *name* for `createVoiceSession` (asked before every connection attempt), transmit reports and the share, so it needs a `renamed(name)` (or a channel by id) or the next reconnect is answered `channel_not_found` and voice stops with "This channel has no voice room". This is the "without leaving voice" criterion.
- The slow way: when any call for the open channel is answered 404 `channel_not_found`, the page re-lists channels and looks for its channel's id; found under a new name, it applies `renamed`; not found, `lost` as today. This covers a tab whose events socket was not open when the frame went out.
- A second tab on the same channel holds its own events socket and moves the same way. A tab opened later on the old hash lands as §3.4 says.
- The control: in the channel list, for an operator only (whoami `role`), a rename beside the open channel that sends `{name, from}`; its refusals map `invalid_request`, `channel_exists`, `channel_name_stale` (with "the channel was renamed to … meanwhile; look again") and a network failure to their own lines, keep the typed name, and never send twice (`ChangeName.tsx` is the pattern). A member sees no control, and the server is what refuses them.

## 5. Published-shape changes for the owner's sign-off

All additive: new types, no published field touched, no version bump, every existing fixture byte-identical (`scripts/schema-compat.sh` holds this).

| Change | Where |
| --- | --- |
| `RenameChannelRequestV1 {name, from}` and its `Validate` | `pkg/schema/channel.go`, fixture `rename-channel-request-v1.json`, `ts.go` |
| `RenameChannelResponseV1 {channel: ChannelV0}` | the same, fixture `rename-channel-response-v1.json` |
| `ChannelSchemaV1 = "conch.channel.v1"`, `ChannelDocV1 {schema, channel_id, name, created_at}` and its `Validate` | the same, fixture `channel-v1.json`, `ts.go` with `Schema:` so the browser's `frames.ts` can name it |
| Error code `channel_name_stale` (409) | `pkg/schema/channel.go` as `ErrorCodeChannelNameStale` |
| Audit action `channel_renamed` | `internal/server/store` (not a wire shape; listed so the audit vocabulary in the alpha exit record stays complete) |
| Channel names unique by key (a server rule, not a shape; `POST /v0/channels` answers 409 for a key clash it did not before) | migration, `CreateChannelAs` |

Not changed: `ChannelV0`, `CreateChannelRequest`, `ListChannelsResponse`, any socket's first frame, any MCP tool, the `SocketCloseReason` vocabulary.

## 6. The split

Two issues, server first; the web one is blocked by the server one's merge (it needs the generated types and a server that sends the frame). Both reference #696; the browser control is not in parallel with #695 (same `ChannelList.tsx`, `api.ts`) and the `pkg/schema/ts.go` edit lands serially with #695 and #704.

### 6.1 Child a: server (server-engineer; `area/schema`, `area/server`, `security`)

Files:

- `pkg/schema/channel.go`, `pkg/schema/testdata/rename-channel-request-v1.json`, `rename-channel-response-v1.json`, `channel-v1.json`, `pkg/schema/ts.go`, regenerated `web/src/schema/channel.ts` (`go generate ./pkg/schema`, never by hand).
- `internal/server/store/store.go` (the migration), `internal/server/store/channels_rename.go` (new: `RenameChannel(ctx, actor, id, from, name, now) (ChannelRenameResult, error)`, `ErrChannelNameStale`, the fill step), `internal/server/store/rename.go` (generalise `claimNameTx`/`clashTx` to a table, or add a channel twin), `internal/server/store/members.go` and `queries.go` (keys on insert).
- `internal/server/channels.go` (`handleRenameChannel`), `internal/server/routes.go` (`rt("PUT /v1/channels/{id}/name", accessOperator, …)`), `internal/server/events_ws.go` (`eventChannel`, `notifyChannel`, `sendRef` case), `e2e/isolation/matrix.go` (an `operator(...)` row: B renames A's channel by id with `From: s.name`, wants 403 `forbidden`).
- Tests: `internal/server/store/channels_rename_test.go` (transaction, same-string no-op, stale `from`, duplicate by spelling, by case, by look-alike, the three races of §3.6, the audit row's actor, subject and quoted detail, the migration's fill on a store with clashing names), `internal/server/channels_test.go` (operator 200; member 403 with `access_denied`; AuthOff 200; 400 for a bad name and a blank `from`; 404; 409 both codes; after a rename the messages, members, nets, invites preview, read marks and voice room of the id are unchanged and the old name is 404 on every route family), `internal/server/events_ws_test.go` (every events socket of the channel gets one valid `conch.channel.v1` with the new name, sockets of other channels get nothing, the socket stays open; `/v2/ws` and `/v1/voice/ws` get nothing and stay open), `pkg/schema` golden and validate tests.
- Checks: `make check`, `go test -race ./...`, `go run ./e2e/isolation`, and the three pinned-trail runs (`e2e/p2-hardening`, `e2e/p3-local`, `e2e/dogfood`), which must not move.
- Review: a `security-reviewer` pass (a new operator route and the isolation row); Nick signs off the shapes in §5 on the issue before dispatch. The approval path is not touched.

### 6.2 Child b: web (web-engineer; `area/web`)

Files:

- `web/src/api.ts` (`renameChannel(id, from, name)`), `web/src/agents/frames.ts` (the `conch.channel.v1` case), `web/src/agents/useAgents.ts` (hand the frame up), `web/src/state/channels.ts` (`renamed`), `web/src/state/messages.ts` and the drafts and read-state keys (re-key by name), `web/src/useLiveChannel.ts` and `useAgents.ts` (effects keyed so a rename does not reopen), `web/src/voice/connection.ts` (`renamed`), `web/src/voice/useVoice.ts`, `web/src/App.tsx` (the slow way on `channel_not_found`; the hash), `web/src/route.ts` (if the hash is replaced rather than pushed), `web/src/ChannelList.tsx` and `web/src/RenameChannel.tsx` (new), `web/src/app.css`.
- Tests: `web/src/ChannelList.test.tsx` and `web/src/renamechannel.test.tsx` (control shown for an operator only; success, 400, 409 `channel_exists`, 409 `channel_name_stale`, network failure; no double send), `web/src/state/channels.test.ts` (`renamed`), `web/src/agents/state.test.ts` (the frame parses; an unknown schema still null), `web/src/voice/connection.test.ts` (a rename while joined keeps the room and uses the new name for the next session request and report), a new `web/e2e/20-channel-rename.spec.ts` (an operator renames from the browser; the page shows the new name and hash without reloading; reload lands on the new name; a second tab open on the channel moves too; a tab in voice stays in voice, its presence unchanged; a third tab opened on the old hash lands on the fallback channel; a member has no control). Async renders found with `await findBy…`, effects flushed before events (brief rules; #465, #477).
- Checks: `make web-check`, `make check`.

## 7. Follow-ups, filed separately

- `conch admin channel rename <old> <new>` in the CLI (resolve the id from the list, send `{from: old, name: new}`), and the TUI's reaction to `channel_not_found` on a channel it is following (today: a plain error).
- A "that channel was renamed" notice for a stale `#/c/<old>` link, if testers hit it; it needs the page to remember ids with names, not the server to remember old names.
- When #697 decides who else may rename, this route's class or handler check, and the attempt limits a non-operator would need (as `renamesPerDay` for a person's own name).
