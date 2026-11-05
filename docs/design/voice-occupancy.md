# Design: voice occupancy across the server

- **Status:** Accepted, 2026-10-18, by the project owner (issue #596, child a), with the three answers recorded in §7. It reverses two decisions recorded in [browser-ui-polish.md](browser-ui-polish.md) (§2). Implemented: the server half (child b, PR #613) and the web client (child c, issue #596). **Amended 2026-11-03** (issue #1103, approved by the owner: "names in the sidebar"): point 1 of §1 is reversed, and §3.1 and §3.3 carry the change, recorded in §8. The document now also says *who* is in each channel's voice, by principal id, and the browser's list names them.
- **Touches:** [voice-control-plane.md](voice-control-plane.md) §6 (presence, its poller and its socket), [client-experience.md](client-experience.md) (UX-02, UX-04, UX-09, UX-23), [ADR-004](../adr/ADR-004-voice-via-livekit.md) (agents keep no voice), [ADR-005](../adr/ADR-005-nets-and-whispers.md) (voice belongs to a channel; nets are rooms of their own), the [browser UX audit](../reviews/browser-ux-audit.md) (F01, F02).
- **Owner:** protocol-designer (this note, the one new wire shape); server-engineer (child b); web-engineer (child c).
- **Issues:** #596 (this note and the split). Child c also builds on #584 (the strip names its destination) and #585 (a live roster when not joined).

Voice presence is shown only for the channel on screen. A tester told "meet in voice" cannot see where the others are, presses Join voice where they happen to be reading, and ends up alone (audit F01, P1). This note adds one number per channel, how many are in its voice room, shown in the channel list and in the voice strip, and says what it costs. It keeps the model: voice belongs to a channel, there are no voice channels, and nothing outside the strip joins anything.

## 1. What this adds, and what it does not

Adds: one document, `conch.voice_occupancy.v1`, that says for each channel the viewer may see voice in how many people are in its channel-wide room and, since #1103, who they are (`principal_ids`, §3.3); one read route and one socket that serve it; a count beside each channel's name in the browser's list, and under the name the people in its voice, by name, up to five and then "+M more" (§6); and, while not in voice, one line in the strip, "3 in voice in #game-night", with a button that opens that channel.

Does not add: names *on the wire* (the document carries principal ids, as presence does; the browser names them from the channel's members, §6); a join control anywhere but the strip (a name in the list opens the channel, as its button does, and never joins); a speaking, muted or deafened state in the list (speaking stays in the voice bar of the room the viewer joined; muted and deafened wait for #598); any new call to LiveKit; a change to any published type; an MCP tool; a dependency; a TUI view (a follow-up, §3.5).

*As first accepted (2026-10-18) this point read "Does not add: names anywhere but the channel's own presence". The owner reversed that on 2026-11-03 (#1103, §8): a tester told "meet in voice" wants to see who is where, not only how many.*

## 2. What this reverses, and why

#536 recorded two decisions in [browser-ui-polish.md](browser-ui-polish.md): in the §1 lobby table, "Left: 'Voice' rooms with avatars: not drawn as rooms. Voice in Conch is per channel (ADR-005); who is in voice is in the voice strip. No fake rooms"; and in §4, "Nothing outside the voice section mentions voice or offers to join it (the lobby test that checks this is unchanged)."

Both were drawn to stop the browser from looking like a room list where browsing and joining blur (UX-02). The audit showed the cost: with nothing outside the strip saying where voice is, people cannot find each other. UX-04 already allows what is needed ("the lobby shows … permitted voice presence"; "an activity card can offer 'Join voice' for a named channel"). So the rule narrows to what it was protecting:

> Nothing outside the voice strip **joins** voice or offers a join. The channel list may say how many are in a channel's voice; opening the channel is all it does.

§1's row stays true in substance (no rooms are drawn: a count sits on the channel, as the unread count does); its wording changes. §4's sentence becomes the rule above, and the lobby test in `web/src/App.test.tsx` is rewritten to it (§6). On the owner's yes, child c edits those two lines of browser-ui-polish.md to point here.

## 3. The five questions

### 3.1 What is shown

**As accepted (2026-10-18): counts only, of the channel-wide room, as people.** For each channel, the number of distinct principals in the room with no audience, as that channel's presence would list them: the same poller state and exclusions (a stale or unread room shows nobody; nothing while LiveKit is unavailable). Two connections by one person are one person, as in presence. The viewer counts themself.

**Amended (2026-11-03, #1103): the count and the principals behind it.** Each row also carries `principal_ids`, the distinct principals the count counts, ascending: exactly the people that channel's presence lists in its channel-wide room, so a row reveals to its reader nothing presence does not already. Ids, not names, as `VoicePresenceV1` carries them: the wire gains no text, and a name is the browser's to resolve (§6). The poller wakes the occupancy sockets when a room's *people* change, not only its count: one person swapped for another in a single pass is a frame.

Alternatives, as weighed in 2026-10 and again in #1103: *names per channel on the wire* would be a second place names are written and kept current, and text in a broadcast document; ids keep the document to what presence already says. *Transmitting or sharing flags* answer no finding, and a transmit flag changes on every key press, multiplying frames (§4); both can be added later as optional fields. *Counting net rooms* makes one channel's number differ by viewer, since a net is visible to its members and monitors only (`GET /v1/channels/{channel}/nets` is filtered to the caller's nets); a number must mean the same to everyone who sees it. Net rooms are not counted, and their people never named, for anyone; if wanted later they fit as a separate optional list (§3.3), which is how ADR-005's nets are kept in mind.

Cost: nothing in storage; a count and a set the poller already holds. A row grows by about eight bytes a person.

### 3.2 Who sees it

**Recommendation: exactly the callers `voicePresenceChannel` admits, channel by channel.** A human, a member, with `voice_allowed`. The channel set is `ListChannelsForPrincipal`, the read behind `GET /v1/channels`, so the document and the list agree (operators get no exemption on either). A channel the caller is not a member of is absent, never a zero. A caller without voice gets presence's 403 `voice_not_allowed`; an agent 403, audited as on presence; with `--auth off` both routes answer 400 `voice_requires_auth` before anything is read. No refusal names a channel.

Alternatives: *counts for members without voice* (#437): useful social information ("the others are in voice; ask for it"), but it is presence reduced, and would make `voice_allowed` mean less here than on every other voice route. Recommended against; it is the one product call in this note (§7). *Counts for every channel on the server* reveals that a channel exists and is busy to someone who cannot read it: against ADR-003 and the isolation suite's first rule.

Cost: one membership read per socket, cached and refreshed as §3.4 says.

### 3.3 The wire shape

**Recommendation: one new additive document.** A sketch; the exact Go lands in child b under the `schema-change` skill. A compatible change: a new type, no published field touched, no version bump, every existing fixture byte-identical.

```go
// pkg/schema/voice_occupancy.go (sketch)
const VoiceOccupancySchemaV1 = "conch.voice_occupancy.v1"

// VoiceChannelOccupancy is one channel the reader may see voice in.
type VoiceChannelOccupancy struct {
    ChannelID int64 `json:"channel_id"`
    InVoice   int   `json:"in_voice"` // distinct people in the channel-wide room
    // Since #1103 (additive): who InVoice counts, ascending; always written
    // by conchd, [] for nobody; absent only in a document from before it.
    PrincipalIDs []int64 `json:"principal_ids,omitzero"`
}

// VoiceOccupancyV1 is the whole state, never a delta: the body of
// GET /v1/voice/occupancy and every frame of its socket.
type VoiceOccupancyV1 struct {
    Schema     string                  `json:"schema"` // always VoiceOccupancySchemaV1
    Configured bool                    `json:"configured"`
    Available  bool                    `json:"available"`
    Channels   []VoiceChannelOccupancy `json:"channels"`
}
```

```json
{"schema": "conch.voice_occupancy.v1", "configured": true, "available": true,
 "channels": [{"channel_id": 7, "in_voice": 3, "principal_ids": [3, 5, 12]},
              {"channel_id": 9, "in_voice": 0, "principal_ids": []}]}
```

`Validate`, in the manner of presence: the schema name; `available` only when `configured`; `channels` always an array (`[]`, never `null`) and empty unless `available` (a count last seen is not reported while LiveKit cannot be reached, as rooms are not); ascending positive `channel_id`, none twice; `in_voice` never negative; and, where `principal_ids` is stated, exactly `in_voice` positive ids, ascending, none twice. When available, every channel the viewer may see voice in is a row, zeros included: a reader tells "nobody" from "no information", and the isolation check asserts "absent" against "zero" literally. No name, token or room; `voice_test.go`'s reflection check (no field `Token`, `Room` or `RoomName` reachable) extends to this type, and no field of it names anyone in text.

**`principal_ids` and the published fixture (#1103).** The field is additive under the `schema-change` skill: `voice-occupancy-v1.json`, published without it, stays byte-identical, and it is that fixture, with `in_voice: 3` and no ids, that fixes the rule "a row without `principal_ids` is a row from a writer before the field; the count stands alone". conchd never writes such a row: it states the ids on every row, `[]` for nobody, and the server tests and the isolation suite assert that. A reader that ignores the field sees the document as it was; the browser shows the count alone for a row without it.

Fixtures: `voice-occupancy-v1.json`, `-people.json` (#1103), `-unavailable.json`, `-not-configured.json`. `ts.go` gains two rows (`VoiceChannelOccupancy`; `VoiceOccupancyV1`, family `voice-occupancy-v1`, with its schema), so `web/src/schema/` is generated. No new error code; refusals use `voice_requires_auth`, `forbidden`, `voice_not_allowed` and `too_many_sockets` in the existing `Error` body.

Alternatives: *channel names in rows* (the reader holds the id-to-name map; presence carries `channel_id` alone); *non-zero rows only* (smaller, but absence becomes ambiguous); *a field on `ChannelV0`* (couples a listing read once to a count that must stay live). Forward compatibility: a later `transmitting` or `sharing` count, or `nets: [{net_id, in_voice}]` for net members, is an optional field without a version.

### 3.4 The transport

**Recommendation: a read and a socket of its own.**

| Route | Access | Answer |
|---|---|---|
| `GET /v1/voice/occupancy` | authenticated human with `voice_allowed` | the document, once |
| `GET /v1/voice/occupancy/ws` | the same; bearer or ticket as on `/v1/voice/ws` | the document on connect and whenever it changes, nothing otherwise |

The socket follows `voice_ws.go`: refusals before the upgrade are plain HTTP; the subscription registers before the checks finish, so a drop during them closes it; `acceptSocket` counts it against the credential's 32; the credential is re-checked on the 30 s interval; `dropPrincipalAll` (disabled or hidden) and `dropCredential` (revoked) close it at once with the same close codes; each wake-up rebuilds the whole document and sends it only when the bytes differ.

Two things differ from presence. The subscription is per viewer, not per channel: the poller keeps occupancy subscribers in a set of their own and wakes them when a room's *participant set* or availability changes (join, leave, removal, rotation, outage either way), not on a transmit flip, which changes no count. And membership is not a reason to close: the socket holds the viewer's channel set, re-reads it on the 30 s re-check, and re-reads it at once when `voiceMemberRemoved` fires for that viewer, so a channel they are removed from leaves the document in the same breath that closes their presence socket for it. Being added shows on the next re-check at the latest: a gain of information, not a leak.

Alternatives:

- *A frame on the events socket.* Its frames are already several types told apart by `schema`, so mixing is not the objection. It is opened per channel, carries that channel's agent activity under an agent's `messages.read` budget, and agents are refused occupancy. Putting a per-viewer, server-wide document there means every tab's events socket repeats it, a new visibility rule on that socket, and a second path from the poller into the events hub. Presence kept voice on its own socket (voice-control-plane.md §6); so does this.
- *The presence socket with a wider query, or a second document on it.* Its readers, `parsePresence` in the browser and `conch-voice`, expect one type. A second type is a breaking change to a contract the Rust client depends on, dressed as a query parameter.
- *One presence socket per channel.* With three sockets a tab already (§4) and 32 per credential, a person in ten channels could not open a second tab.
- *Polling the read.* Meets "within 3 s" only at a 2 s interval: a membership read and a document per tab every 2 s, for ever, for something that changes a few times an hour. Everywhere else the project sends a whole-state frame on change.

Parity (CLAUDE.md rule 4): the read exists first and the browser is a client of it. `conch voice where`, a one-line table of the document, is the CLI consumer, a small follow-up issue.

### 3.5 Agents and the TUI

Agents keep no voice (ADR-004 defers them; voice-control-plane.md §6 says whether an agent may see who is talking is part of that decision). A count is the same information, smaller: both routes refuse an agent as presence does, and `/mcp` gains no tool, so there is no MCP-only capability (rule 4) and no agent capability before the decision. `conch-voice` is unchanged. The TUI's channel list gets the same mark in a follow-up under tui-polish, not in #596.

## 4. Cost on the server

**The rate at which conchd asks LiveKit does not change.** Passes are driven by rooms in use and sweeps by the 30 s timer (voice-control-plane.md §6); the only requests that wake the loop are a session and a transmit report, through `wakeIfIdle`. An occupancy subscriber consumes what passes already learn, like a presence subscriber: it never wakes the loop and never calls LiveKit. The count is read from the room states a pass has just written, under the lock the pass holds.

**Fan-out.** Each change to a participant set wakes every occupancy subscriber (presence wakes only the channel's). A wake costs one document build under the poller's lock, over tens of rooms, and a byte compare; a transmit flip wakes none of them. A slow socket costs the poller nothing: the wake channel holds one notification and the send never blocks, as now. For one instance with tens of people (ADR-002) this is well under the events socket, which reads SQLite per frame.

**The socket budget.** A tab holds a message socket, an events socket and, from #585, a presence socket whenever a channel is shown: three. This adds a fourth, per tab, not per channel: eight tabs per credential instead of ten, with a TUI and a voice client still fitting. That is the reason the socket is per viewer.

**Store.** One `ListChannelsForPrincipal` per socket open, per re-check and per removal hook. No table, no migration.

## 5. Isolation

The same per-viewer filters as presence: membership (the channel set), `voice_allowed` (the refusal) and audiences (net rooms are never counted; when they exist, the builder counts only rooms whose audience is the whole channel, so the structural rule and the per-viewer rule agree). A channel the caller may not read is absent, not zero.

With `principal_ids` (#1103) the document names people, by id, so two more things hold and are checked. Every id in a row is a member of that row's channel as the reader lists its members (the isolation rows' `checkPeopleAreMembers`): the reader could already see them there, and under hide-members (community-model §13.2 clause 4) sharing a channel is exactly what lets one member know another. The marker scan reads `principal_ids` as principal ids, so an id of A's in anything B is sent fails the run wherever it appears. The hide-members phase adds a voice row: with LiveKit, bob in the voice of the public channel alice is in and zed in the voice of his own; alice's occupancy, read and on her sockets, names bob under the shared channel (as her member list of it does) and has neither zed's channel nor his id; bob's occupancy names nothing of alice.

Checks child b must add. In `e2e/isolation` the coverage check (`routes.go`) fails the run until both routes have a row, so these are not optional:

1. **`matrix.go`:** an `own` row for `GET /v1/voice/occupancy` and one for `GET /v1/voice/occupancy/ws` (bearer, and with a ticket): they succeed for B's prober and hold nothing of A. The marker scan checks every byte, A's channel ids among the numbers; the rows also assert that every `channel_id` is one of B's own. `agentRows` gets both as 403 `forbidden` for B's agent, audited.
2. **`sockets.go` and `controlB`:** B holds an occupancy socket open from before A starts voice to the end of the run; its frames carry only B's channels, and B's counts do not change when A's people join and leave. A positive control, as `controlB` does for presence, has someone of B's join B's room and asserts B's count rises, so the silence means something. Without LiveKit the socket carries one document, `configured: false`.
3. **`revoke.go`:** a revoked tester's occupancy socket closes with the revoked code, and so does a disabled or hidden one (its credential is no longer live, as on the presence socket); a member removed from one of B's channels keeps the socket and loses that row within the bound the presence socket is closed in.
4. **A person without voice:** the world has none (`voice_allowed` defaults to 1; bea and ola both hold it). Add one, or cover it in the handler table: 403 on both routes, and the browser never opens the socket for them (§6).
5. **`web/e2e/14-isolation.spec.ts`, the LiveKit case:** with alice in A's room, B's list shows no voice mark and B's strip no "in voice in" line; `watchTraffic` already records every socket frame, so the new socket is scanned for A's markers without a change to the helper.

Handler and socket tests in `internal/server`, table-driven on the pattern of `voice_test.go` and `voice_presence_test.go`: member with zero and non-zero channels, non-member channel absent, agent, no voice, not configured, unavailable, a room with an audience set not counted, a frame on join and leave and none on a transmit flip, closes on revocation and disablement, drops a channel on removal. Presence visibility is authorization: child b gets a `security-reviewer` pass.

## 6. The web client (child c)

**The marks.** In `ChannelList.tsx`, after the name and beside the unread pill: a `role="img"` span with a speaker glyph (inline SVG, `aria-hidden`) and the number, named "3 in voice", with an id the channel's button adds to its `aria-describedby`, exactly as `UnreadBadge` does. Zero shows nothing. The mark appears whether or not the viewer is in voice; the button's name stays the channel's.

**The names (#1103).** Under a channel's row, when its row states people: a list named "In voice in #game-night", one row a person, in the server's order (ascending id), up to five and then "+M more". The viewer is "You"; anyone else is named as the voice bar's roster names them, or "Member N" until the channel's members have been read. Each row is a button that calls `onOpen` for the channel and nothing else, outside the tab order (the channel's button, which is in it, does the same), so the list stays one tab stop a channel. No speaking indicator, and no muted or deafened state until #598. On a phone each row keeps a 44 px target. A row without `principal_ids` (an older conchd) shows the count alone.

Names come from the client, not the wire: `useOccupantNames.ts` reads `GET /v1/channels/{channel}/members` for a channel with someone in voice the page cannot name, once per channel and again only for someone new there, never while a read for that channel is in flight, and never for the viewer; the people of the channel on screen name whom they can first. It is `useVoiceNames.ts`'s rule, over every channel in the document instead of the one joined. Every channel in the document is one the viewer is a member of, so its members are readable, hide-members included (§5).

**The strip line.** While not in voice, and some channel *other than the one on screen* has people, the Voice region shows "3 in voice in #game-night" and a button **Open #game-night** that calls the existing `onOpen` and nothing else. The channel with most people is named, ties to the lower id; with several, the line ends "and 2 more channels" and the list has the rest. The shown channel's own people are the roster (#585), not this line; while in voice the line is not shown, since the strip then describes the viewer's own connection.

**Nothing outside the strip offers a join.** The lobby assertion in `App.test.tsx` becomes: outside the Voice region, no control has an accessible name matching `/join|voice/i`; and the marks are present as text in the list. Seeing occupancy never joins: `useOccupancy.ts` imports nothing from `sdk.ts` or `connection.ts`, constructs no `VoiceClient`, and its component test asserts `getUserMedia` is never called when a document arrives.

**The socket.** `api.ts` opens `/v1/voice/occupancy/ws` through `connect` as it does presence; the hook holds it for the life of the signed-in page, not per channel. The trap #585 found applies: a refusal before the upgrade reaches the browser as close 1006 and `Feed` retries for ever, so the hook does not open the socket when `WhoAmIResponseV1.voice_allowed` is false. On `configured: false` the one frame says so and the marks stay empty.

## 7. Decisions of the project owner (2026-10-18)

1. **The reversal (§2): yes.** Counts in the channel list, under the narrowed rule "nothing outside the strip joins or offers a join". Children b and c may proceed.
2. **People without voice (§3.2): refused**, as on every other voice route. `voice_allowed` keeps meaning "may see and use voice".
3. **Nets (§3.1): the channel-wide room only, for everyone.** Net rooms, when they arrive, are a separate optional list for the net's own members, not folded into the channel's number.

## 8. Amendment: names in the sidebar (2026-11-03, issue #1103)

The owner's sign-off: "approve 1103, names in the sidebar". The principal's decisions, which this note now carries:

1. **§1 point 1 is reversed.** The channel list names who is in each channel's voice, nested under the channel, not only how many.
2. **The wire change is additive, no version bump.** `VoiceChannelOccupancy` gains `principal_ids` (§3.3): the distinct principals in the channel-wide room, ascending, always exactly `in_voice` of them, which `Validate` checks whenever the field is stated. Ids, not names, as `VoicePresenceV1`: the wire carries no new text, and a row reveals exactly what that channel's presence already does. The published fixture is untouched; a reader that ignores the field sees today's document.
3. **Names are the client's.** The browser resolves them as the voice bar's roster does, from the channel's members (§6). Every channel in the document is one the reader may read, so its members are readable; that held under hide-members too (§5).
4. **No speaking indicator in the list**; speaking stays in the voice bar of the room the viewer joined. **Muted or deafened** wait for #598.
5. **Fold:** up to five names under a channel, then "+M more". Clicking a name, the "+M more" or the channel opens the channel. Nothing in the list joins voice.
6. **Net rooms stay out, for everyone**, people and count alike, as decision 3 of §7 says.
