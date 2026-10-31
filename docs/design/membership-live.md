# Design: a person's channel list follows membership changes live

- **Status:** Proposed by the protocol-designer, 2026-10-31 (issue #1027, the design and schema half). It confirms the issue's recommendation, with the adjustments in §1. The schema change is additive (a new type, no existing fixture changes), so it needs no version sign-off (`schema-change` skill). The server and web halves build on this note in the same PR.
- **Touches:** [community-model.md](community-model.md) §13.6 (`conch.directory_changed.v1`, the pattern this follows), [admin-roles.md](admin-roles.md) and ADR-014 (`see_member_directory`), [channel-rename.md](channel-rename.md) (`conch.channel.v1`), ADR-003 (membership is the visibility boundary), browser UX audit 3, finding F1.
- **Owner:** protocol-designer (this note and the shape); server-engineer and web-engineer (§7).

When someone else adds a person to a channel, or removes them, the person's open page does not change until they reload. The events socket has no frame that says "your channels changed". The read-state frames cannot serve: a newly gained channel with nothing unread sends none, and the zeroed read-state sent on a removal looks the same as "never marked, nothing unread". This note adds one content-free signal, says who gets it and when, and says what the page does with it.

## 1. Decisions

| Question | Decision |
| --- | --- |
| The document | `conch.channels_changed.v1`, `ChannelsChangedV1{schema}`. Its wire form is exactly `{"schema":"conch.channels_changed.v1"}`. It carries no channel id, no name, no principal, no count and no reason. |
| What it means | "Your answer to `GET /v1/channels` may have changed." The client re-reads `GET /v1/channels` and `GET /v1/read-state` and **replaces** what it holds. It never edits its list from the frame. |
| Why content-free | A frame with no content cannot leak a name, through a race or otherwise: the subscriber's goroutine reads nothing to build it. Channel ids were considered and rejected. They would let a client skip a re-read, but a removed member's frame would then name the channel it lost, and a frame racing a later change could name a channel the reader is no longer in. The re-read is one small `GET`, bounded by the coalescing. |
| Who gets it | Only human principals whose own `GET /v1/channels` answer changed (§2), on every one of their open events sockets. Never agents. Never a third party. |
| Rate | Coalesced per socket: the first change arms a timer for the roster floor (`Server.eventsRosterFloor`, default `defaultEventsRosterFloor`, 250 ms), and one frame goes when it fires, whatever arrived meanwhile (§3). |
| Gating | None beyond the socket's own checks (§4). Read-state frames are unchanged. `see_member_directory` never decides it. |

**Adjustments to the issue's proposal.** These are settled here.
- **More triggers.** A rename, a visibility or default change, and an archive also change what `GET /v1/channels` answers, so they send the frame too. A rename sends it today; the others send it once their routes exist (§2). The recipients are then the channel's human members, each of whom can already read the new state.
- **Only effective changes send it.** A `DELETE …/members/{id}` that removed nobody sends nothing. So does a `PUT` that neither added the member nor set `manages`. Otherwise a manager could make any principal's page re-read on demand.
- **Removal from the open channel is driven by the close, not by the frame.** The browser's events socket is opened on the open channel (`?channel=`), so a removal from that channel closes it (1008, `SocketCloseRemoved`) before any frame could arrive on it. The page's existing 1008 path (re-read the list; the channel is gone, so it is lost) already handles this case. The frame covers the person's *other* channels and their other tabs.
- **The line** is the page's existing one, "You no longer have access to #name" (`Lobby.tsx`, `channels.lost`), not new copy.

## 2. Triggers and recipients

The server sends the frame after the change commits, from the handler that made it, through one helper (§7.1). Every trigger is listed here, with the route on `main` at `d5500a1` where one exists.

| Change | Route or hook (`internal/server`) | Recipient(s) |
| --- | --- | --- |
| P added to a channel by id | `handlePutChannelMember` (`members.go`), when the store reports `added` | P |
| P's `manages` set by an operator | `handlePutChannelMember`, when `req.Manages != nil` (the list carries `manages`) | P |
| P added by name | `handleAddChannelMemberByName`, when `added` | P |
| P removed by a manager or an operator | `handleDeleteChannelMember`, when the store reports `removed` | P (on the sockets that stay open) |
| P creates a channel | `handleCreateChannel` (`channels.go`), the creator, for P's other tabs | P |
| A membership through an invite or joining | `handleJoinAccount` and `writeGuestJoined` (`join.go`), `guestConverted` (`conversion.go`), the invite redeem routes (`invites.go`) | P. Today these principals or credentials are new, so no socket of theirs is open and the call does nothing. It is made anyway, so a later route that adds an existing principal is covered. |
| A channel renamed | `handleRenameChannel`, when `res.Changed` | Every human member of the channel, listed from the store after the commit |
| Visibility or default changed (ADR-013, `PUT /v1/channels/{id}/settings`; not on `main`) | that handler, on a change | Every human member of the channel |
| A channel archived (#844; not on `main`) | that handler | Every human member just before the archive, listed in its transaction and sent after the commit |
| Self-join and leave (#991, ADR-013 I3b; not on `main`) | those handlers | P |

**No frame is needed** where every socket of the principal closes anyway: a disable, a role change that drops the sockets, a password recovery, a credential revocation, and a guest visit that ends. The page's reconnect re-reads (§5).

**Out of scope.** These are not changed here.
- Non-members' view of the **channel directory** (`GET /v1/directory/channels`, #991) when a public channel is created, renamed or turned private. That is a different read, and its live refresh, if any, gets its own issue.
- **Agents.** Their memberships reach them over MCP.
- The **`conch` TUI.** It skips a schema it does not know (`dispatchEvent`, `internal/cli/client.go`). If it wants the signal, that is a separate issue under API parity (ADR-001). The frame is on the public events socket, so the API side is already there.

## 3. Coalescing

`eventsSub` gains a `channelsChanged` flag, set under `hub.mu` by the helper and taken by `take`. On a set flag, the subscriber's goroutine arms `channelsLater` for the floor if it is not already armed. When the timer fires, the goroutine writes one frame. A change that arrives while the timer is armed adds nothing.

This is deliberately trailing-edge, unlike the roster's leading-edge floor. Any burst within the floor (an operator adding a person to three channels, or a rename that follows an add) gives exactly one frame. Every frame comes at most one floor after its first change, well inside the two seconds the issue asks for. The frame has no content, so a frame sent late is never wrong, only redundant.

Before the write, `select` on `sub.dropped` first, as the `rosterLater` case does. A socket dropped meanwhile closes with its reason and sends nothing.

## 4. Gating

- **The socket's own checks are unchanged.** `flush` asks `stillMember` and the credential as it does today. A socket the person may no longer hold closes, and the frame is not sent on it.
- **No membership check on the frame.** The frame is addressed to P, about P, and says nothing. No visibility rule decides it, as none decides a heartbeat.
- **Read-state frames are unchanged.** The server keeps sending them as today, including the zeroed `ReadStateV1{channel_id}` after a removal. The client stops treating any read-state frame as list information. It applies a frame only for a channel in its list, and after a re-read it replaces every count with `GET /v1/read-state`. Order does not matter, because the counts come from the re-read.
- **`see_member_directory` (ADR-014) and `hide_members` (#988).** They are independent of this signal. They decide which *people* a viewer may know. This signal says only that the viewer's own channels changed, and it is never sent to a third party, so the directory predicate never applies to it. A membership change that also changes someone's directory answer (with the permission off) sends `conch.directory_changed.v1` to those viewers under #988's targeting, as a separate frame.

### 4.1 How it differs from `conch.directory_changed.v1`

| | `conch.channels_changed.v1` (this note) | `conch.directory_changed.v1` (#993, §13.6) |
| --- | --- | --- |
| Re-read | `GET /v1/channels`, `GET /v1/read-state` | `GET /v1/directory/members` |
| Replaces | the channel list and unread counts | the people cache and autocomplete candidates |
| Caused by | the viewer's own memberships, or a change to a channel they are in | a principal created, renamed, disabled, enabled, hidden or re-roled; a setting toggled; with the setting on, a membership change that alters who knows whom |
| Recipients | the subject (or the channel's members) | every viewer whose directory answer changed |
| On a membership change with the setting off | sent to the subject | not sent |

The two signals never stand in for each other. A client handles each with its own re-read. Neither shares a flag or a timer with the other on `eventsSub`.

## 5. Client behaviour (browser)

1. **Parse.** `frames.ts` recognises `channelsChangedV1Schema` and validates it with the generated `channelsChangedV1`. An unknown field is ignored. Anything else is skipped, as today.
2. **Re-read and replace.** On the frame, the page runs one single-flight `refreshChannels()`. A frame that arrives during a re-read schedules one more re-read after it, not a parallel one. The refresh reads `GET /v1/channels` and replaces the list. The open channel stays open if it is still there. Then it reads `GET /v1/read-state` and replaces the counts. A channel that is new in the list shows its unread count at once.
3. **The open channel is gone.** The page opens the fallback channel (`#general`, else the first, else the empty state) and shows the existing line, "You no longer have access to #name". Nothing of that channel stays on screen: its feed, drafts-in-view, members panel and runs are cleared, as `lost` does today. In practice the 1008 close on the open channel's socket usually gets there first (§1). Both paths end in the same `lost` action, and the second is a no-op.
4. **A hint only.** The list on screen is always the server's last answer. The frame never adds, removes or renames an entry by itself.
5. **Reconnect.** After the events socket reconnects (`open` following `waiting` or `denied`), the page re-reads its list once. A frame missed while the socket was down is then caught up.
6. **No channel at all.** The events socket needs `?channel=`, so a person in no channel has none, and cannot be told of their first add. Until a channel-less events socket exists, the page re-reads `GET /v1/channels` when the window regains focus or becomes visible. While its list is empty, it also re-reads every 30 s. A channel-less events socket is a separate, security-reviewed issue.

## 6. Isolation and leak rows

| # | Row | Expected |
| --- | --- | --- |
| L1 | A is added to channel C, which B is not in; B has an events socket open on a shared channel | B receives no `channels_changed` frame. |
| L2 | A is removed from C; B is a member of C | B receives no `channels_changed` frame. B's member list changes through the existing members frame, as today. |
| L3 | A is added or removed; an operator and an agent each have events sockets open | Neither receives the frame. |
| L4 | A's frame | Its bytes are exactly `{"schema":"conch.channels_changed.v1"}`. |
| L5 | A is removed from C, and its frame arrives | A's `GET /v1/channels` lacks C, and any C route answers 404 `channel_not_found`, the same answer as for a channel that never existed. |
| L6 | A manager sends `DELETE …/members/{B}` for a B who is not in the channel, or a no-op `PUT` | B receives nothing. |
| L7 | C is renamed | Only C's human members receive the frame, and a non-member's sockets stay quiet. |

## 7. Implementation brief

### 7.1 Server (`internal/server`)

- `events_ws.go`:
  - Add `channelsChanged bool` to `eventsSub`, and return it from `take`.
  - Add `func (h *eventsHub) notifyChannelsChanged(principalIDs ...int64)`. It sets the flag and signals every sub with `sub.human` and a matching `principalID`. A zero id matches nothing.
  - In `eventsStream`, add `channelsLater <-chan time.Time`. `flush` arms it for `e.s.eventsRosterFloor` if it is nil. A new `run` case first checks `sub.dropped` (as the `rosterLater` case does), then `stillMember`, then writes `schema.ChannelsChangedV1{Schema: schema.ChannelsChangedSchemaV1}` through `sendDoc`.
  - Extend the file's header comment with the new frame.
- Call sites, each after the commit and next to the existing `notifyChannelRoster`:
  - `members.go`: `handlePutChannelMember`. Keep the `added` result the store returns, which is discarded today, and notify when `added || req.Manages != nil`.
  - `members.go`: `handleAddChannelMemberByName`, when `added`.
  - `members.go`: `handleDeleteChannelMember`. Keep `removed` and notify when it is true, after `s.events.dropPrincipal`, so the dropped socket is already gone.
  - `channels.go`: `handleCreateChannel`, for the creator.
  - `channels.go`: `handleRenameChannel`, when `res.Changed`. List the channel's members from the store and notify the human ones. A failed list read is logged and does not fail the rename.
  - `join.go`: `handleJoinAccount` and `writeGuestJoined`; `conversion.go`: `guestConverted`; the invite redeem handlers in `invites.go`. Each passes the new principal's id.
- Tests: a new `events_ws_channels_changed_test.go`, table-driven, run with `go test -race ./internal/server/...`.
  - For each route (add by id, add by name, set `manages`, delete, create, rename, join account): P's open sockets each get exactly one frame within the floor (the test sets a short floor). Another member's, an operator's, an unrelated human's and an agent's sockets get none.
  - The no-op `DELETE` and the no-op `PUT` send nothing (L6).
  - Three changes within the floor give one frame per socket.
  - The frame's bytes are exactly the wire form.
  - P removed from the channel its socket is on: that socket closes with `SocketCloseRemoved` and carries no frame, and P's socket on another channel gets one.
- `e2e/isolation/`: rows L1–L4 and L7 in `matrix.go`, with the socket helpers in `sockets.go` (`openBSockets` and `checkQuiet` already count B's frames).
- No MCP tool changes: agents never receive the frame, and the API carries everything (ADR-001 parity).

### 7.2 Web (`web/src`)

- `agents/frames.ts`: add a `{ kind: 'channelsChanged' }` frame for `channelsChangedV1Schema`.
- `agents/useAgents.ts`: add an `onChannelsChanged` callback, called on that frame and once on each reconnect (`open` after `waiting` or `denied`).
- `App.tsx`: add a single-flight `refreshChannels()` next to `refind`, which reads `api.listChannels()`. On success it dispatches a new `refreshed` action; on 401 it signs out as today.
- `state/channels.ts`:
  - Add `refreshed { channels }`. It replaces the list.
  - If `selected` is absent from the new list, set `lost` to that name and select the fallback, exactly as `lost` does.
  - Never merge.
- `useReadState.ts`: expose a reload that re-reads `api.listReadState()` and replaces the counts. `refreshChannels` calls it after the list.
- Add focus and visibility re-reads, and the 30 s re-read while the list is empty (§5.6).
- Unit tests, with a mocked socket and fetch:
  - The frame is parsed, and an unknown field is tolerated (`state.test.ts` or `panel.test.tsx`).
  - The list is replaced, and a channel absent from the new answer is dropped (`ChannelList.test.tsx`, `state/channels.test.ts`).
  - A removed open channel is left with the line, and none of its messages stays (`App.test.tsx`).
  - The counts are replaced after a refresh (`readstate.test.tsx`).
  - The list is re-read after a reconnect.
  - Frames that overlap a re-read cause one follow-up re-read, not parallel ones.
- Playwright `e2e/04-private.spec.ts`, at 1440×900 with two contexts:
  - owner adds member-1 to channel-a, and member-1 sees it within 2 s with no reload;
  - owner posts, and the badge shows 1 within 2 s;
  - owner removes member-1, and channel-a goes.
  - The add assertion must fail on `main` first.

## 8. Schema

- `pkg/schema/channels_changed.go`: `ChannelsChangedSchemaV1` and `ChannelsChangedV1` with `Validate`.
- `pkg/schema/testdata/channels-changed-v1.json`: the golden fixture.
- `pkg/schema/ts.go`: a row in `TSTypes` with family `channels-changed-v1`, regenerated into `web/src/schema/generated.ts` and `generated.families.ts`.
- **Compatible.** It is a new type, and no existing type or fixture changes. Readers of the events socket already skip unknown schemas. The server never adds content to v1; a signal that carries content would be a new design, with a security review.
