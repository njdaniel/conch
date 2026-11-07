# Design: several screen shares at once in a voice room

- **Status:** Proposed by the protocol-designer, 2026-11-07, for the project owner's sign-off (issue [#1483](https://github.com/njdaniel/conch/issues/1483)). It replaces decision **O5** of [private-alpha.md](private-alpha.md) ("anyone in the room may share, one share per room at a time"), which Nick set aside on 2026-11-07: "i cant share screen when she sharing"; offered take-over, several at once, or one at a time, he chose **several at once**. Every decision below is a recommendation with its reason, made under the principal's delegation; nothing is left open but what only Nick can decide, listed in §9. The change needs a **new version of the presence document** (§2), so the `schema-change` skill asks for his yes before any code. Nothing here is implemented; no code, dependency or migration lands with this note.
- **Touches:** [voice-control-plane.md](voice-control-plane.md) §6 (presence), §12 (the share) and §13 (companions, their share audio); [webcam-video.md](webcam-video.md) §4.4 and §5 (the cap that is a server constant, not a `Validate` check) and §7.4 (the call view and the strip); [ADR-015](../adr/ADR-015-webcam-video.md); [linux-desktop.md](linux-desktop.md) §6.4 and [windows-desktop.md](windows-desktop.md) §7 (model B: one companion per person); [mobile-app.md](mobile-app.md) §3.4 and §8 (a phone watches, never shares); [admin-roles.md](admin-roles.md) §7.3 (who stops a share); [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (REST first; no MCP tool is added, agents are not in voice).
- **Owner:** protocol-designer (this note and the shapes); server-engineer (§10.2); web-engineer (§10.3, §10.6); cli-engineer (§10.4); voice-engineer (§10.5).
- **Current-code baseline:** origin/main `59cd50ef`, inspected 2026-11-07. The one-share rule lives in three places. **Schema:** `VoicePresenceRoom.Validate` refuses a room listing two participants `sharing` (`pkg/schema/voice.go:343–348`), and so does its Rust twin `VoicePresenceRoom::check` (`voice/crates/conch-voice-api/src/types.rs:750`); the generated TypeScript validator checks fields only, with no count (`web/src/schema/generated.ts`, `voicePresenceRoom`). **Readers that refuse:** the TUI ends its presence subscription on a document that fails `Validate` (`internal/cli/client.go:829–860`); the helper's presence stream validates every frame (`conch-voice-api/src/presence.rs:294`); `e2e/voice` and `e2e/isolation` validate what they read; the server validates its own snapshot before every frame (`voice_ws.go:96`, `voice.go:269`). The browser reads the first sharer it finds (`web/src/voice/presence.ts:46`, `sharer`) and subscribes to **every** screen-share track while watching (`web/src/voice/sdk.ts:185`, `wanted`). **Server:** a room holds `share *voiceShare` (`voice_presence.go:246`) beside `cameras map[int64]*voiceCamera` (`:271`); `POST …/voice/share` answers 409 `voice_share_busy` when another holds it (`voice_share.go:205`), `PUT …/companion/audio` likewise (`voice_companion_audio.go:41`); a channel manager or an operator stops "the share" with a bare `DELETE` (`voice_share.go:260`, `stopShare` `:971`); every `UpdateParticipant` already goes through one permission function that takes "holds the share" as an input (`voice_permission_fn.go`); the camera's cap is `voiceCameraCap = 9` (`voice_camera.go:55`) with 409 `voice_camera_full`. **Measurement:** the one-sharer-to-29-viewers figure O6 asked for (#448, A15) is not recorded anywhere in `docs/` (alpha-exit-record B12 is blank).

Two people in one room cannot both show their screens: the second is told someone else is sharing. Nick wants both to share. Lifting the server's lock alone would break every published client that validates presence, because the one-share rule is part of the published presence document's contract. This note says how two or more sharers reach clients without breaking the old ones, where the new bound is, and what each client does with several shares.

## 1. Decisions in one page

| Question | Decision |
| --- | --- |
| How two sharers reach clients | A **new presence version, `conch.voice_presence.v2`**: the v1 document without the one-share rule, plus `share_started_at` on a sharing participant. A client asks for it with `?schema=conch.voice_presence.v2` on `GET /v1/channels/{channel}/voice` and on `GET /v1/voice/ws`; absent, it gets v1 exactly as today (§2.1, §2.2). |
| What an older client sees | A valid v1 document. With two or more shares, exactly one participant is `sharing`: the reader's own share if they hold one, else the earliest-started; the others show no share (§2.3). |
| The cap | **4 shares per room**, `voiceShareCap`, a server constant like the camera's; a fifth claim is 409 **`voice_share_full`**, naming nobody (§3). |
| The claim | `POST …/voice/share` grants a share to any entitled caller while the room has fewer than 4 and the caller holds none; a holder asking again is answered with the share held, as today. `voice_share_busy` is never answered again; the constant stays (§4.1). |
| The release | `DELETE …/voice/share[?started_at=…]` ends the caller's own share. A moderator ends another's with **`?principal=<id>`**, the camera's parameter (§4.2). |
| Who stops whose | Self; holders of `manage_channels`; a manager of the channel. Unchanged from today (§4.3). |
| Share audio | Per sharer: `PUT …/companion/audio` consents for the caller's own share and companion. Where it answered `voice_share_busy` it answers 409 `voice_no_session` (§4.4). |
| Companions | One `p<id>.screen` per person, as today; N sharers are N companions. Nothing new (§4.5). |
| Grants and passes | Per principal, as today; "anyone but the holder" becomes "anyone who holds no share". Each access-lost path ends that person's share and no one else's (§4.6). |
| Audit | Unchanged rows, one set per share; a refused claim writes nothing (§4.7). |
| Browser | The watched share is the main picture; the others are tiles in the strip, like cameras, ordered by `share_started_at`; a tile's button swaps it in. The main share's sound plays, no other's; strip tiles ride the lowest layer. On a phone, a switcher without tiles (§6.1). |
| TUI, helper, desktop, phone | Each moves to v2 in its own issue; the text names every sharer; the desktop's companion watch view picks one share; a phone watches one share at a time and never shares (§6.2–§6.5). |
| Bandwidth | Estimates in §7, not measurements: four shares with the strip shown cost a viewer about 1.4× one share; the droplet's new cost is up to four captured top layers in. The measurement O6 asked for is still owed and is an issue (§10.7). |
| Tests | Schema fixtures for v2 and the degraded v1; server unit; `e2e/voice` scenario `multishare` with two concurrent sharers, the cap and the v1 reader; isolation rows for `?principal=`, `?schema=` and one sharer losing access beside another (§8). |
| Split | Seven issues: schema, then server, then the browser, the TUI, the helper and the desktop in parallel, then the measurement (§10). None touches the approval path. |

## 2. Versioning: the crux

### 2.1 Why a version and not an addition

**Recommendation: a new version, `conch.voice_presence.v2`.**

The skill's test is whether a published reader refuses the new document. It does: the Go and Rust `Validate` of v1 both say "a room has one share at a time", and the TUI, the helper and both e2e harnesses run it on every frame. A document with two sharers is therefore not a v1 document, whatever its fields. Removing the check from `VoicePresenceRoom.Validate` would change no fixture and pass `schema-compat.sh`, and it would still be a mutation of a published version in place: every reader built before it would drop the document, and the TUI would close its subscription. That is the breaking change the skill reserves for a bump and Nick's yes.

The camera avoided this on purpose (ADR-015, design §5, N3): it never put a count in `Validate`, so its cap can move without a schema change. The share's rule predates that lesson. v2 adopts it: no count of shares anywhere in the schema; the cap is enforced at the claim and presence reports whatever was granted.

The two other routes considered:

- **A capability the client declares on its socket** (a header or parameter meaning "I accept many sharers") without a new schema name. It is the same negotiation as below with the version hidden: the document a "many" client receives is one a v1 reader refuses, so it is a different document and must say so in its `schema` field, or a reader cannot recognise it without out-of-band context, which is what the field exists to avoid (`VoicePresenceSchemaV1`'s comment). Rejected as a version under another name.
- **The server degrades for everyone and nobody asks**: pick one share to show and never tell anyone about the second. That keeps v1 and defeats the feature: the browser could not show two shares. Rejected; but the degradation is right for clients that have not moved, and §2.3 keeps it for them.

### 2.2 The shapes and the parameter

```go
// pkg/schema/voice.go (sketch; the exact Go lands in §10.1 under the
// schema-change skill)

// VoicePresenceSchemaV2 names the v2 presence document: v1 without the
// one-share rule. Any number of participants may be sharing; how many is the
// server's cap, not the schema's (ADR-015 N3).
const VoicePresenceSchemaV2 = "conch.voice_presence.v2"

// VoicePresenceV2 has the fields of VoicePresenceV1, Schema always
// VoicePresenceSchemaV2, Rooms []VoicePresenceRoomV2. share_ended and
// camera_ended are as in v1: the reader's own, in their document alone.
type VoicePresenceV2 struct { /* as VoicePresenceV1 */ }

// VoicePresenceRoomV2 has the fields of VoicePresenceRoom. Validate checks
// the audience, every participant, and no principal twice; it does not count
// sharers.
type VoicePresenceRoomV2 struct { /* as VoicePresenceRoom */ }

// VoiceParticipant gains one optional field, in v1 and v2 alike:
//   ShareStartedAt *Timestamp `json:"share_started_at,omitempty"`
// present exactly when Sharing: the started_at of the claim, as VoiceShareV1
// gave it the sharer. Validate: share_started_at needs sharing, and sharing
// needs share_started_at.

// VoicePresenceSchemaParam is the optional query parameter of
// GET /v1/channels/{channel}/voice and GET /v1/voice/ws naming the presence
// version the caller reads: VoicePresenceSchemaV1 or VoicePresenceSchemaV2.
// Absent means v1. Any other value is 400 invalid_request, read before the
// channel, so it says nothing about the channel.
const VoicePresenceSchemaParam = "schema"
```

**Why `share_started_at`.** Every client needs one deterministic order for several shares: which tile is first, which share a Watch press opens, and which one a v1 reader is shown (§2.3). Participants are ordered by `principal_id`, which is stable but says nothing about the share; "the share that started first" is what a person expects to stay on the stage when a second starts, and the server already holds it (`voiceShare.startedAt`). It is additive on `VoiceParticipant`, omitted when not sharing, so the published v1 fixtures stay byte-identical and a v1 reader that does not know it skips it. It also gives a reader the share's identity, which the sharer already has for their own (`share_ended.started_at`): a share that stopped and started again has a new value. **Why on the participant and not a new `shares` list:** the share stays the participant's attribute, as the camera is; a second list keyed by principal would say the same thing twice and a reader would have to join them.

**Why a query parameter and not a path.** `/v2/ws` versions the message socket by path, but the presence routes sit under `/v1/channels/{channel}/voice` and `/v1/voice/ws` with `?channel=` and `?ticket=` already; a `/v2/…` pair would be a new route class with its own matrix rows and the same handler behind it. One parameter on the two routes that serve presence, read before the channel, is the smallest explicit choice. The occupancy document is unchanged and takes no parameter. The parameter is `schema` because its value is a schema name, the same string the document carries.

**The TypeScript.** `go generate ./pkg/schema` emits `voicePresenceV2` and `voicePresenceRoomV2` beside the v1 pair, and `share_started_at` on `voiceParticipant`; `ts.go` gains the v2 rows and `voice-presence-v2` fixture family. The Rust twin (`conch-voice-api`) gains the same types by hand, checked against the same fixtures as today.

### 2.3 Exactly what an older client sees

A reader of v1 (no `?schema=`, or `?schema=conch.voice_presence.v1`) gets a document that satisfies v1's `Validate` by construction, built from the same room state:

| Shares in the room | The v1 document |
| --- | --- |
| none, or one | byte-identical to today, except that the sharing participant now also carries `share_started_at`, which an old reader skips. |
| two or more | every participant is listed as today; **exactly one** carries `sharing`, `sharing_audio` and `share_started_at`: **the reader's own share if they hold one, otherwise the earliest-started**. The other sharers are listed with those fields omitted, as if not sharing. `share_ended` and `camera_ended` are the reader's own, unchanged. |

**Why own-first.** The one v1 reader that also shares is a browser tab served before the upgrade and not reloaded (`conchd` serves `/app/`, so the browser is otherwise never older than the server). Its share state machine must see its own `sharing` to behave; showing it the earliest share instead would make it offer "Watch Sam's screen" while it shares. **Why earliest for everyone else.** A v1 viewer watching Sam must not have the picture swapped under them because Val started; the earliest share stays theirs until it ends, and then the next-earliest appears, which is the browser's own rule in §6.1. **What it costs.** A v1 reader is not told about the other shares. In practice that reader is the TUI, whose share text names one person, and the helper, which ignores the sharing flags. No first-party client stays on v1 after §10; v1 remains served and published, and retiring it is a later decision, not this one.

**The server's own check.** `snapshotLocked` takes the version beside the viewer, builds the document the reader asked for, and validates it as it does today; a v1 snapshot can never fail the one-share rule because it never sets `sharing` on two.

## 3. The cap

**Recommendation: `voiceShareCap = 4` shares per room, a constant in code, not a setting and not a schema check. A fifth claim is 409 `voice_share_full`, naming nobody. The browser says "This call already has 4 screens shared."**

- **Why a cap at all.** A share's top layer is as captured, up to 4K at 60 fps (O6 as superseded by #565), several times a camera's 1.2 Mbps; the droplet takes every sharer's top layer in and forwards a layer per subscriber out. Without a bound, a room of thirty could put thirty captured screens on a 2-vCPU droplet. The cap also bounds the strip (§6.1) and the audit traffic a room can produce.
- **Why 4.** Nick's case is two. A cap of 2 recreates the refusal he hit the moment a third person wants to show something. Nine, the camera's figure, is the largest call view; nobody follows nine screens, and nine captured 4K layers in is a cost the droplet has never been measured for. Four lets a small group all share, keeps the worst case within about 1.4× today's egress for a viewer who shows the strip (§7), and can move once §10.7 has measured it, without a schema change (ADR-015 N3).
- **It counts claims**, so it is exact and needs no pass: a holder asking again is not a new claim; a pending claim counts (its row is being written); a share that ended frees its place the moment it ends, before LiveKit is asked to withdraw. It does not limit who is in voice.
- **The refusal writes nothing**: no row, no LiveKit call, as `voice_camera_full` and `voice_share_busy` write nothing today. It is read under the room's claims, after the entitlement re-read, so a revoked caller gets `voice_not_allowed`, not `voice_share_full`.
- **No rate bound is added.** The camera's bound (`voice_camera_rate_limited`) exists because its continue-and-adopt traffic costs a LiveKit call per request under the lock; the share's claim has the same shape today and has no bound, and this note does not add one. If a room's log fills with claims, a manager stops the sharer (§4.3); a bound is a separate issue if measurement shows the need.

## 4. Server

### 4.1 The claim

`POST /v1/channels/{channel}/voice/share` keeps every check of voice-control-plane §12 in its order (caller, `share_screen`, session, connection). Under the room's claims:

- the caller holds a share (`r.shares[caller]`, started or pending): the sources are granted again and the share held is answered, unchanged, nothing written;
- else `len(r.shares) >= voiceShareCap`: 409 `voice_share_full`;
- else the claim is recorded, pending, before LiveKit is asked, as today; the person's `p<id>` is granted the share's sources and their `p<id>.screen`, if listed, the screen; `voice_share_started` is written; 200 `VoiceShareV1`.

`r.share *voiceShare` becomes `r.shares map[int64]*voiceShare` keyed by principal, the camera's shape. One share per person is the invariant (a person is one participant in presence, with one companion), so "asking again while holding it" keeps its meaning. **`voice_share_busy` is no longer answered by anything.** The constant `ErrorCodeVoiceShareBusy` stays in `pkg/schema` with its comment amended ("answered by no conchd from this version; kept for readers"), and the TypeScript keeps emitting it: removing a published constant breaks Go and TypeScript readers at compile time for no gain.

### 4.2 The release

`DELETE /v1/channels/{channel}/voice/share[?started_at=…][&principal=<id>]`, on the camera's pattern (`VoiceCameraPrincipalParam`):

- **no `principal`, or the caller's own id:** the caller's own share. With `started_at`, only if that is the caller's current share, else 409 `voice_share_stale` and nothing changes; the stale rule is now against the caller's share, not "the room's", which is the only reading that exists with several. Without it, the caller's share, or 204 when they hold none.
- **`principal` naming someone else:** the moderator's stop (§4.3); 403 `forbidden`, audited `access_denied`, for anyone else; 204 when that person holds no share in the room, nothing written, naming nobody, the same answer as for an id that never existed (the isolation matrix's rule). `started_at` beside a `principal` that is not the caller is 400 `invalid_request`: a moderator stops a person's share, not a claim.
- malformed or repeated parameters are 400 before the channel is looked at, as today.

**What changes for callers.** Today a moderator's bare `DELETE` ends "whoever shares". With several shares that request is ambiguous, and the only published client that stops a share is the browser's own sharer, which always names its `started_at` (`connection.ts:383`); no CLI command and no operator control sends a bare `DELETE`. So the bare request becomes "my own share" with nothing published depending on the old reading, and a moderator names the person, as they do for a camera. The new constant is `VoiceSharePrincipalParam = "principal"`.

### 4.3 Who may stop whose

**Recommendation: unchanged. The sharer; a holder of `manage_channels`; a manager of the channel (`ManagesChannel`).**

ADR-015 O7 made the camera stricter (operators only) with the argument that "the share is one per room, so a manager must be able to free it for the room". With four places instead of one the argument weakens but does not fall: the places can still all be taken, a shared screen is what the room looks at, and a manager's job in their channel is what the room shows (admin-roles §7.3). Narrowing a published behaviour to match the camera would take a tool from managers to gain symmetry; the asymmetry is stated here and in the camera note already. Managers of the channel and operators also keep the voice kick.

### 4.4 Share audio, per sharer

`PUT /v1/channels/{channel}/voice/companion/audio` records the caller's consent on **the caller's own share** (`r.shares[caller].audioScope`) and grants `screen_share_audio` to the caller's own `p<id>.screen`, under the room's claims, as voice-control-plane §13 says. The one check that changes: "the caller holds the room's share (held by someone else: 409 `voice_share_busy`)" becomes "the caller holds a share (none, or pending: 409 `voice_no_session`)". `DELETE …/companion/audio` is unchanged. Every end of a share ends its own consent and no other's; `voice_share_audio_stopped` precedes that share's `voice_share_stopped`.

Presence `sharing_audio` is per participant, as today: the person's own audio track, or their companion's with their consent recorded. Two sharers with sound are two participants with `sharing_audio`; what a viewer plays is the client's choice (§6.1).

### 4.5 Companions

Nothing changes. A companion is `p<id>.screen`, one per person, refused (409 `voice_companion_busy`) when a second is asked for, folded into the person (§13). N sharers are N companions, each granted its own person's screen on its own person's claim, each withdrawn on its own person's end. The viewer identity `p<id>.view` publishes nothing and receives whatever share it subscribes to.

### 4.6 Grants and the passes

- **Grants** are per participant through the permission function (`voice_permission_fn.go`), whose "holds the share" input is now `r.shares[pid] != nil`. Nothing else in the function changes; the structural test over every `UpdateParticipant` call site holds.
- **Enforcement** on each pass: today "anyone but the holder" with a screen-share source granted or published has it withdrawn; now **anyone who holds no share** does. `judgeShareLocked` loops over `r.shares` for the holder half (left, access_lost, idle, the audio flag) and keeps its `consider` half per participant, keyed as today by `voiceWithdrawKey{pid, role}`. Unclaimed shares are audited and evicted as today.
- **Idle** is per share: 15 s without a first track, 5 s after the last. One sharer's idle never touches another's.
- **Access lost.** `endSharesOf(principalID, channelID, reason)` already takes a principal; `sharesWithheld` (the per-pass `share_screen` read) runs over every holder; `endWithheldShare` on the claim path and the voice switch's hook (#984, `voiceAfterVoiceOff`) end the one person's share with `access_lost`. **No other share in the room is touched**, and the room is not rotated by a voice switch (#984's rule). A removal or disable that rotates the room ends every share `rotated`, as it ends everyone's connection today, and each browser resumes its own in the new room (`share.ts`'s `rotated` path).
- **Restart** drops every claim, as today; the first pass withdraws every grant without a claim.
- **Metrics.** `conch_voice_shares_active` counts started shares across rooms; its meaning ("screen shares under way") is unchanged and it may now exceed the room count.
- **Rooms.** Only a channel-wide room has shares, as today.

### 4.7 Audit

No new action and no changed detail. `voice_share_started` (`channel=<id> audio=<bool>`, actor the sharer), `voice_share_stopped` (`channel=<id> principal=<sharer> reason=<reason>`, actor the sharer or the moderator), `voice_share_unclaimed`, `voice_share_audio_started` and `voice_share_audio_stopped` are written per share exactly as today; the `principal=` in the detail already tells one sharer's rows from another's. A refused claim (`voice_share_full`, like `voice_share_busy` before it) writes nothing. Every share that started gets exactly one stopped row.

## 5. Where it travels

- **Presence**, v1 and v2, on `GET /v1/channels/{channel}/voice` and `GET /v1/voice/ws`, by `?schema=`. No new event document: presence is a full snapshot on its own socket.
- **Occupancy** is unchanged: it counts people, never shares.
- **MCP:** nothing. Agents are not in voice (voice-control-plane §1), and no voice tool exists to keep in step. Parity (rule 4) holds through REST: everything a client does here is a REST call, and the CLI gets the operator's stop (§6.2).
- **`VoiceShareV1`, `VoiceSessionResponseV1`, `VoiceRoomGrant`, the companion types:** unchanged.

## 6. Clients

### 6.1 Browser

**Watching.** The stage keeps its model (webcam-video §7.4): one main picture and a strip. **The watched share is the main picture; every other share is a tile in the strip**, drawn like a camera tile with a screen glyph and "{name}'s screen", ordered by `share_started_at` with the newest last, so nothing reorders when someone starts. A tile is a button, "Watch {name}'s screen": it swaps that share into the main area and the previous one into the strip; cameras keep their place in the strip and their enlarge-and-swap. **Watch** (the existing control, and the roster's "is sharing" line) opens the earliest-started share of someone else's; a sharer never sees their own share as a tile (they have the screen). When the main share ends, the stage moves to the next-earliest live share with the existing "{name} stopped sharing." note, so a viewer is not dropped while screens are live; when none is left, watching ends as today.

**Sound.** The main share's sound plays through the existing volume and mute; **no other share's sound is subscribed to or played**. Two soundtracks at once are unintelligible, the desktop's rule is one playback owner per audio source, and it keeps a viewer's download to one audio stream. Swapping the main swaps the sound.

**Subscription.** `wanted` learns whose share is wanted: `ScreenShare` video for the main share and for each visible strip tile, `ScreenShareAudio` for the main share only, keyed by the publisher's principal (as `claimed` keys cameras today). The SDK's adaptive stream sizes each element's layer, so a strip tile rides the lowest layer and the main the layer its size selects; a hidden strip (the call view closed, the phone layout) subscribes to no tile. A sharer may watch another's share while sharing; nothing prevents it.

**Sharing.** Share screen is enabled whenever this page could share, whoever else is; `busyNote` and `oneSharerNote` ("One person shares at a time.") go; 409 `voice_share_full` says "This call already has 4 screens shared." The sharer's own lifecycle (claim, publish, `share_ended`, the `rotated` resume, the named release) is unchanged.

**Phone layout** (narrow, below the existing 700 px rule): no strip. One share fills the width; a chip "Watching {name}'s screen" opens a list of the live shares to switch; the switch swaps picture and sound together. Tiles would cost data for pictures too small to read (O8's reason for not opening video unasked).

**Presence.** `useVoice` asks for `?schema=conch.voice_presence.v2` and reads `voicePresenceV2`; `sharer(presence)` becomes `sharers(presence)`, ordered by `share_started_at`.

### 6.2 TUI and CLI

The TUI reads v2 and names every sharer in its presence text: "sam is sharing a screen" stays for one; "sam and val are sharing their screens (val with sound)" for several; `conch voice status` lists each sharer with `since`. It has no share control and gets none. For parity with the camera's operator stop, the CLI gains `conch voice share stop <channel> <person>` (`DELETE …/voice/share?principal=<id>`), with the hostile-server tests the camera's command has. A TUI not yet updated keeps reading v1 and names one sharer (§2.3).

### 6.3 The voice helper (`conch-voice`)

`conch-voice-api` gains the v2 types and asks for `?schema=conch.voice_presence.v2` on its presence stream, so the helper never reads the degraded document. It shows no share text and plays share audio by the existing rules (#906: never its own companion's tracks; never other participants' `ScreenshareAudio` when its companion owns share playback), which hold with several sharers unchanged. One small issue.

### 6.4 Desktop (Electron companion, model B)

The server side needs nothing: the desktop's share is its person's claim and its own `p<id>.screen`, one per person. The renderer is the web client, so §6.1 is the desktop's view too, with one desktop-specific rule: under B2 the companion subscribes to **the watched share's** screen tracks and the visible tiles', keyed by principal, never "every screen track", and never its own companion's (#906, unchanged). Main's teardown (stop the share, disconnect the companion on `leave`, lock, sign-out) is unchanged. A desktop may share and watch another's share at once.

### 6.5 Phone viewers

A phone never shares (mobile-app §3.4, §8, unchanged). The PWA on a phone gets the switcher of §6.1. The native phone viewer (#1118, future) inherits the same rule, one share at a time with a picker, and reads v2; mobile-app.md gets a one-line amendment pointing here when this note is accepted.

## 7. Bandwidth

These are estimates from the layers #565 fixed and typical Chrome screen-content bitrates, **not measurements**; the measurement O6 asked for (one sharer to 29 viewers on the droplet, #448) was never recorded, and §10.7 owes both it and the N-sharer figures.

| Stream | Typical | Note |
| --- | --- | --- |
| A share's captured top layer | 2.5–5 Mbps at 1080p30; 8–15 Mbps at 4K60 | the sharer's uplink, and the droplet's ingress once per sharer |
| The 1080p30 layer | ~2.5 Mbps | what a desktop viewer's main picture selects |
| The 720p15 layer (the lowest, #565) | ~0.3–0.6 Mbps | what a strip tile selects |
| Share audio | ~0.1 Mbps | one per viewer, the main's |

- **Egress, 29 viewers, today (1 share):** 29 × ~2.6 ≈ **75 Mbps**.
- **Egress, 29 viewers, 4 shares, strip shown:** 29 × (2.6 + 3 × 0.45) ≈ **115 Mbps**, about 1.4×. A viewer with the strip hidden or on a phone costs what one share costs today.
- **Ingress, 4 shares:** 4 × (top layer + the two lower layers) ≈ **15–65 Mbps**, against ≈ 4–16 today. This is the new cost: four captured screens in, forwarded without transcoding, so it is network and a little CPU, not encoding.
- **Transfer.** 115 Mbps for an hour is ≈ 52 GB against a 4 GB droplet's monthly allowance of 4 TB; a daily two-hour session with four shares is within it.

If the measurement finds the ingress or CPU a problem, the levers in order are: the cap (a constant), the strip's tile layer (a smaller simulcast layer from the sharer, a web change), and O6's own answer, a larger droplet. None is a schema change.

## 8. Tests

### 8.1 Schema (§10.1)

- Fixtures: `voice-presence-v2.json` (two sharers, one with sound, a camera), `voice-presence-v2-share-ended.json`, `voice-presence-v1-degraded.json` (a v1 document built from the two-sharer state: one `sharing`, with `share_started_at`), plus `share_started_at` in a new `voice-presence-v1-sharing-started.json`; every existing fixture byte-identical.
- `Validate` cases: v2 accepts two sharers; v1 still refuses two; `share_started_at` without `sharing` and `sharing` without `share_started_at` fail in both; `sharing_audio` still needs `sharing`.
- `schema-compat.sh` green; the generated TypeScript checked against the fixtures; the Rust types against the same fixtures.
- `voice_test.go`'s reflection test still holds: no token, room name or device anywhere in v2.

### 8.2 Server unit (§10.2, fake LiveKit)

- The claim, table-driven: fourth claim 200, fifth 409 `voice_share_full` with nothing written and no LiveKit call; a holder's repeat answers the held share and does not count; a revoked caller gets `voice_not_allowed` before the cap; after one stop the fifth claim succeeds.
- The release: own, by `started_at` (stale against the caller's share), by `principal` for a manager, an operator and a plain member (403, audited), `started_at` with another's `principal` (400), `principal` naming a non-sharer (204, nothing written).
- Companion audio: consent on the caller's own share only; a second sharer's companion is never granted audio by the first's consent; each end writes its own audio row first.
- The passes: two holders, one leaves (`left` for one, the other untouched); one's `share_screen` cut in the store (`access_lost` for one); idle per share; a non-holder's unclaimed track beside two holders; rotation ends both `rotated` with two rows in order.
- The snapshot: v2 lists both; v1 as viewer A (a sharer) shows A; as a non-sharer shows the earliest; both validate; `?schema=` unknown is 400 before the channel; absent is v1 byte-for-byte with one share.
- `metricCounts` with two started shares and one pending.

### 8.3 `e2e/voice`, a new scenario `multishare` (§10.2)

Against the pinned LiveKit, with `lk` participants as `share.go` does: **sam and val both claim (200, 200) and both publish a screen, val with sound**; una, reading v2, sees two `sharing` participants with their `share_started_at`, and her `lk` is subscribed to both screens; **a v1 reader of the same room sees exactly one sharer, sam, and the document validates**; two more members connect and claim (200 each: the room now holds four shares), and **a fifth member's claim is 409 `voice_share_full`** with no `voice_share_started` row; sam stops with his `started_at`: his tracks are gone for una at once, val's are still listed, the v1 reader now shows val, and the fifth claimant's retry succeeds; **an operator who is not a member stops val's share with `?principal=`** and val's `voice_share_stopped` says `reason=operator`; a late release from sam naming his ended `started_at` is 409 `voice_share_stale`; the `companion` scenario adds a line: two companions, each refused `screen_share_audio` until its own holder's PUT and refused again after it, and one holder's PUT never reaches the other's companion. `sharingIn` becomes `sharersIn` over v2. `share.go`'s existing assertion that a second claim is busy (`share.go:249`) becomes the assertion that it succeeds.

### 8.4 `e2e/isolation` (§10.2)

- **Matrix rows:** `DELETE …/voice/share?principal=<A member>` by B's member, B's manager and B's operator-made member on A's channel: 404, no marker, the same as a ghost id; `GET …/voice?schema=conch.voice_presence.v2` and the socket with it, the same refusals as the v1 rows; `?schema=bogus` 400 before the channel, saying nothing. The coverage check keys on routes, so no route is left without a row.
- **Media half** (`media.go`): B's member and B's manager, in B's own channel, `?principal=<A's sharer id>`: 204, nothing written, no marker. The row at `media.go:787` that waited on `voice_share_busy` for den's previous sharer to be found gone is rewritten: the second claim now succeeds at once, and the row asserts the previous share still ends `left` within the bound.
- **Voice gates** (`voicegates.go`): a two-sharer variant of `sharesWithheldRow`: two members share in one room; the owner flips **one** member's voice switch (`PUT /v1/principals/{id}/voice-permission`, the hook with no rotation); within the bound that member's share ends `access_lost` and their tracks are withdrawn, **and the other's share stays listed, published and in v2 presence**, with no rotation and no row of theirs. With the switch back the member claims again (200: the room has a free place).
- **Revocation** (`revoke.go`): the disabled tester who holds "every socket variant, a LiveKit participant and the share" now holds one of two shares; the other tester's share is listed before and after.

### 8.5 Web (§10.3)

- Vitest: `sharers()` order by `share_started_at`; `wanted` with two sharers (main gets video and audio, a tile video only, a hidden strip nothing, own share never); the stage's swap, the move to the next-earliest when the main ends, the phone switcher; the notes (`voice_share_full`, no busy note); the generated `voicePresenceV2` against the fixtures.
- Playwright (fake devices, `make web-e2e`): two contexts share, a third watches and sees two tiles with the earliest as main, swaps, hears the main's sound only, the first sharer stops and the stage moves; a fourth context is told the call has 4 screens shared when the cap is met (with two more headless sharers from the harness).

## 9. Published-shape changes for the owner's sign-off

| Change | Kind | Nick? |
| --- | --- | --- |
| **`conch.voice_presence.v2`**: `VoicePresenceV2`, `VoicePresenceRoomV2`, with no one-share rule; `VoicePresenceSchemaParam` (`?schema=`) on `GET /v1/channels/{channel}/voice` and `GET /v1/voice/ws`, absent meaning v1 (§2.1, §2.2) | **version bump** | **yes** |
| The v1 degradation under several shares: one `sharing`, the reader's own else the earliest-started (§2.3) | the compatibility story of the bump; within v1's contract | **yes**, with the bump |
| `VoiceParticipant.share_started_at`, optional, present iff `sharing`, in v1 and v2 (§2.2) | additive optional field | no |
| `voice_share_full` (§3) | additive error code | no |
| `VoiceSharePrincipalParam = "principal"` on `DELETE …/voice/share`; a bare moderator `DELETE` now means the caller's own share (§4.2) | additive parameter; changed reading of a request no published client sends | no, noted |
| `voice_share_busy` kept, answered by nothing; `PUT …/companion/audio` answers `voice_no_session` where it answered `voice_share_busy` (§4.1, §4.4) | doc change; changed refusal, no shape change | no, noted |
| `voiceShareCap = 4` (§3) | a server constant, not a shape | **yes, as the product decision replacing O5**, as O6's figures were his |
| Who may stop whose share: unchanged (§4.3) | no change | no |
| Audit actions and details (§4.7); `VoiceShareV1`, the session and companion types; occupancy | unchanged | — |

On acceptance, the same PR that flips this note's status adds to private-alpha.md O5 the line "*Superseded by #1483 (2026-11-07): several shares per room, capped; see voice-multi-share.md*", as O6 carries for #565, amends voice-control-plane §12's opening rule in place with the same pointer, and adds the one-line cross-reference to mobile-app.md §8.

## 10. The split

Seven issues, filed to the task template once Nick has signed §9; each is executable cold with this note as its design. **None touches a path in `.github/workflows/process-checks.yml`'s approval-path list** (`internal/server/mcp*.go`, `internal/server/admin_*`, the approval, decision, resolution and audit-read files): no MCP tool, no admin-door action and no audit reader changes. Each issue says so under its labels.

### 10.1 Schema: `conch.voice_presence.v2`, `share_started_at`, `voice_share_full`, the two parameters

- **Context:** this note §2, §3, §4.2; the `schema-change` skill; Nick's sign-off on §9 recorded on #1483. Breaking: a new version beside v1; v1 never changes. The cap is not a `Validate` check (ADR-015 N3).
- **Files:** `pkg/schema/voice.go`, `pkg/schema/voice_test.go`, `pkg/schema/ts.go`, `pkg/schema/testdata/voice-presence-v2*.json`, `voice-presence-v1-degraded.json`, `voice-presence-v1-sharing-started.json`; `web/src/schema/generated.ts` by `go generate` only.
- **Acceptance criteria:**
  - [ ] `VoicePresenceSchemaV2`, `VoicePresenceV2`, `VoicePresenceRoomV2` exist; `VoicePresenceRoomV2.Validate` accepts two sharing participants; `VoicePresenceRoom.Validate` still refuses them with today's message.
  - [ ] `VoiceParticipant.ShareStartedAt` (`share_started_at`, omitempty) validates as present iff `Sharing`.
  - [ ] `ErrorCodeVoiceShareFull = "voice_share_full"`, `VoiceSharePrincipalParam = "principal"`, `VoicePresenceSchemaParam = "schema"` exist with the comments of §2.2 and §4.2; `ErrorCodeVoiceShareBusy`'s comment says it is answered by nothing from this version.
  - [ ] `scripts/schema-compat.sh` passes with every existing fixture byte-identical; the new fixtures of §8.1 are in `testdata/` and in `ts.go`'s table; `scripts/schema-ts-check.sh` passes.
- **Required tests:** §8.1 in full; `go test ./pkg/schema/...`; `make check`.
- **Blocked by:** Nick's sign-off on §9 (recorded on #1483). Labels: `area/schema`, `voice`. Approval-path: no.

### 10.2 Server: several shares per room, the cap, `?principal=`, `?schema=`, companion audio per sharer, e2e

- **Context:** this note §2.3, §3, §4, §5, §8.2–§8.4. The share becomes a map keyed by principal, as the camera is; the permission function's share input becomes per principal; v1 is built degraded as §2.3 says; `voice_share_busy` is answered nowhere.
- **Files:** `internal/server/voice_share.go`, `voice_companion_audio.go`, `voice_presence.go` (room state, `snapshotLocked` with a version, `metricCounts`), `voice_ws.go`, `voice.go` (`handleVoicePresence`), `voice_permission_fn.go`, `voice_permission.go` (the voice switch's hook), their tests; `e2e/voice/share.go` (`multishare`, `sharersIn`), `e2e/voice/companion.go`; `e2e/isolation/media.go`, `voicegates.go`, `revoke.go`, the matrix table; `docs/design/voice-control-plane.md` §12 and §13 amendments; `docs/design/private-alpha.md` O5 line.
- **Acceptance criteria:**
  - [ ] Two entitled, connected members of one channel both get 200 from `POST …/voice/share`; both are `sharing` in v2 presence with distinct `share_started_at`; the fifth concurrent claim in a room is 409 `voice_share_full` with no row and no LiveKit call; a holder's repeat answers the held share.
  - [ ] `GET …/voice` and `GET /v1/voice/ws` honour `?schema=` (v1 default, v2 on request, 400 `invalid_request` for any other value before the channel is resolved); a v1 document with two shares lists exactly one `sharing` participant, the reader's own else the earliest-started, and validates; with one share it is byte-identical to today but for `share_started_at`.
  - [ ] `DELETE …/voice/share` ends the caller's own share; `?started_at=` is checked against the caller's share (409 `voice_share_stale` otherwise); `?principal=<id>` ends that person's share for a holder of `manage_channels` or a manager of the channel, is 403 `forbidden` (audited `access_denied`) for anyone else, 204 with nothing written when that person holds none, and 400 with `started_at` naming someone else's.
  - [ ] `PUT …/companion/audio` grants `screen_share_audio` to the caller's own companion for the caller's own share and answers 409 `voice_no_session` when the caller holds none; `voice_share_busy` appears in no handler.
  - [ ] Each access-lost path (`share_screen` cut, the voice switch, membership loss, idle, leaving) ends one person's share and no other's, with its rows; rotation ends every share `rotated`.
  - [ ] `conch_voice_shares_active` counts started shares across rooms.
- **Required tests:** §8.2 unit tables; `e2e/voice` `multishare` and the `companion` lines of §8.3; the isolation rows of §8.4 with the coverage check green; `make check`.
- **Blocked by:** §10.1. Labels: `area/server`, `voice`, `security` (LiveKit grants). Approval-path: no (touches no listed path). Security-reviewer pass before merge (grants and who-may-stop).

### 10.3 Web: watch one of several shares, tiles in the strip, the phone switcher

- **Context:** this note §6.1, §8.5; webcam-video §7.4 (the stage and strip this reuses). The browser moves to v2 presence; subscription is keyed by sharer; one share's sound plays.
- **Files:** `web/src/voice/presence.ts` (`sharers`), `useVoice.ts` (`?schema=`), `sdk.ts` (`wanted`, `Wants` keyed by principal), `connection.ts`, `VoicePanel.tsx`, `CallView.tsx`, `ShareView.tsx`, `share.ts` (notes), `web/src/stage.ts`, their tests; `web/e2e/12-screen-share.spec.ts` and a new multishare spec; `web/src/schema/generated.ts` is generated only.
- **Acceptance criteria:**
  - [ ] With two others sharing, Watch shows the earliest-started share as the main picture and the other as a strip tile labelled "{name}'s screen"; the tile's button swaps them; cameras keep their tiles; tiles are ordered by `share_started_at` and never reorder while shares continue.
  - [ ] Only the main share's `ScreenShareAudio` track is subscribed and played; strip tiles subscribe to `ScreenShare` video only; a hidden strip subscribes to no tile; the page's own share is never a tile.
  - [ ] When the main share ends and another is live, the stage moves to the next-earliest with "{name} stopped sharing."; when none is live, watching ends as today.
  - [ ] Share screen is available while others share; 409 `voice_share_full` shows "This call already has 4 screens shared."; `oneSharerNote` and `busyNote` are gone.
  - [ ] Below the narrow breakpoint there is no strip: one share fills the width and a "Watching {name}'s screen" chip switches between live shares, picture and sound together.
  - [ ] `tsc`, `eslint` and `scripts/schema-ts-check.sh` clean; no new dependency.
- **Required tests:** §8.5 Vitest and Playwright.
- **Blocked by:** §10.1 (types and fixtures); §10.2 for Playwright. Labels: `area/web`, `voice`. Approval-path: no.

### 10.4 CLI and TUI: v2 presence, several sharers in the text, `conch voice share stop`

- **Context:** this note §6.2; parity with `conch voice camera stop` (ADR-015 C5).
- **Files:** `internal/cli/client.go` (`VoicePresence`, `SubscribeVoicePresence` on v2), the TUI's presence text, a new `internal/cli/voice_share.go` and tests.
- **Acceptance criteria:**
  - [ ] The TUI reads `?schema=conch.voice_presence.v2` and names every sharer: one as today; two or more as "sam and val are sharing their screens (val with sound)".
  - [ ] `conch voice status` lists each sharer with when they started.
  - [ ] `conch voice share stop <channel> <person>` sends `DELETE …/voice/share?principal=<id>`; exits nonzero with one line on 403, 404 and on a hostile server's malformed answer; 204 prints nothing.
- **Required tests:** table-driven presence text; hostile-server tests as the camera's command has; `golangci-lint` clean.
- **Blocked by:** §10.2. Labels: `area/cli`, `voice`. Approval-path: no.

### 10.5 Voice helper: v2 presence types and stream

- **Context:** this note §6.3. The helper validates every presence frame and would refuse two sharers; it moves to v2 so no first-party client reads the degraded document.
- **Files:** `voice/crates/conch-voice-api/src/types.rs`, `presence.rs`, its fixture tests; `voice/conch-voice/src/presence.rs`.
- **Acceptance criteria:**
  - [ ] `VoicePresenceV2` and `VoicePresenceRoomV2` exist with `check` as §2.2; `VoiceParticipant.share_started_at` is read; the v2 fixtures decode and validate; the v1 types are unchanged and still refuse two sharers.
  - [ ] The presence stream asks for `?schema=conch.voice_presence.v2` and the helper's roster is built from v2.
  - [ ] `cargo fmt`, `cargo clippy -D warnings`, no `unwrap` outside tests, no new dependency.
- **Required tests:** fixture round-trips against `pkg/schema/testdata/voice-presence-v2*.json`; the stream test with a two-sharer frame.
- **Blocked by:** §10.2. Labels: `area/voice`, `voice`. Approval-path: no.

### 10.6 Desktop: the companion watches one of several shares

- **Context:** this note §6.4; linux-desktop §6.4, #906. The renderer is the web client of §10.3; this issue is the desktop-only subscription rule and a check that two desktops share at once.
- **Files:** the companion's subscription filter in the desktop renderer/main (`desktop/main/voice/*`, `web/src/voice/native/*` as #772 lands them), their tests.
- **Acceptance criteria:**
  - [ ] Under B2 the companion subscribes to the watched share's and the visible tiles' screen tracks by publisher, never to its own companion's tracks and never to voice.
  - [ ] Two desktops in one room both share; each helper plays voice once; neither plays its own share's sound; the operator stop by `?principal=` ends one and not the other.
- **Required tests:** the renderer's filter unit tests; an `e2e/voice` companion line if the harness has two companions (else recorded as a by-hand check with the two-desktop steps).
- **Blocked by:** §10.3; #772. Labels: `area/desktop`, `voice`. Approval-path: no.

### 10.7 Docs and measurement: N sharers to 29 viewers on the droplet

- **Context:** this note §3, §7; O6's unrecorded measurement (#448, alpha-exit-record B12).
- **Files:** `docs/design/alpha-exit-record.md` B12, a deployment note, the CHANGELOG line ("several people can share a screen at once, up to four per room"), user documentation.
- **Acceptance criteria:**
  - [ ] Recorded for N = 1, 2 and 4 sharers at 1080p30 and one at 4K60, 29 viewers with the strip shown: droplet ingress and egress, CPU, and the viewer's layer selection; with the figure the cap is confirmed or an issue to change it is filed.
- **Required tests:** none (a measurement, recorded).
- **Blocked by:** §10.3. Labels: `area/docs`, `voice`. Approval-path: no.
