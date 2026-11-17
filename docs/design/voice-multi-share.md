# Design: several screen shares at once in a voice room

- **Status:** Accepted by Nick (Tier-H), 2026-11-07, as written: the `conch.voice_presence.v2` bump, the v1 rule and `voiceShareCap = 6` (recorded on #1483). **Amended 2026-11-07 under #1507 (PR #1526), within the accepted decisions:** the published v1 shapes gain **no** fields, not even optional ones, because the browser's generated validators are closed (`web/src/schema/validate.ts`, `object()` refuses a key the type lacks), so a v1 document carrying `share_started_at` would be dropped by every open tab from before it. The two participant fields live on a v2 participant (`VoiceParticipantV2`), the quality echo on a v2 claim (`VoiceShareV2`) sent only to a client that declared one, and the v1 degradation of §2.3 is a plain v1 document. §2.2, §2.3, §3, §9.1, §10 and §11.1 say so; the bump, the v1 rule and the cap are as signed. Previously: Proposed by the protocol-designer, 2026-11-07, for the project owner's sign-off (issue [#1483](https://github.com/njdaniel/conch/issues/1483)). It replaces decision **O5** of [private-alpha.md](private-alpha.md) ("anyone in the room may share, one share per room at a time"), which Nick set aside on 2026-11-07: "i cant share screen when she sharing"; offered take-over, several at once, or one at a time, he chose **several at once**. **Revised the same day on his direction: screen sharing "like Discord"**: every live stream is a tile in the call grid beside the cameras, nothing is received until a viewer presses Watch, several streams can be watched at once with each one's sound and controls in its tile, the sharer picks the quality (#1488) and viewers see it on the tile, and the grid shows everyone by default (#1487). Every decision below is a recommendation with its reason, made under the principal's delegation; nothing is left open but what only Nick can decide, listed in §10. The change needs a **new version of the presence document** (§2), so the `schema-change` skill asks for his yes before any code. Nothing here is implemented; no code, dependency or migration lands with this note.
- **Touches:** [voice-control-plane.md](voice-control-plane.md) §6 (presence), §12 (the share) and §13 (companions, their share audio); [webcam-video.md](webcam-video.md) §4.4 and §5 (the cap that is a server constant, not a `Validate` check), §7.4 (the call view this puts stream tiles into) and O8 (nothing plays unasked); [ADR-015](../adr/ADR-015-webcam-video.md); issue #1487 (the grid shows everyone; "Hide non-video participants") and issue #1488 (the sharer's quality picker for today's single share); [linux-desktop.md](linux-desktop.md) §6.4 and [windows-desktop.md](windows-desktop.md) §7 (model B: one companion per person; one playback owner per audio source); [mobile-app.md](mobile-app.md) §3.4 and §8 (a phone watches, never shares); [admin-roles.md](admin-roles.md) §7.3 (who stops a share); [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (REST first; no MCP tool is added, agents are not in voice).
- **Owner:** protocol-designer (this note and the shapes); server-engineer (§11.2); web-engineer (§11.3, §11.6); cli-engineer (§11.4); voice-engineer (§11.5).
- **Current-code baseline:** origin/main `59cd50ef`, inspected 2026-11-07. The one-share rule lives in three places. **Schema:** `VoicePresenceRoom.Validate` refuses a room listing two participants `sharing` (`pkg/schema/voice.go:343–348`), and so does its Rust twin `VoicePresenceRoom::check` (`voice/crates/conch-voice-api/src/types.rs:750`); the generated TypeScript validator checks fields only, with no count (`web/src/schema/generated.ts`, `voicePresenceRoom`). **Readers that refuse:** the TUI ends its presence subscription on a document that fails `Validate` (`internal/cli/client.go:829–860`); the helper's presence stream validates every frame (`conch-voice-api/src/presence.rs:294`); `e2e/voice` and `e2e/isolation` validate what they read; the server validates its own snapshot before every frame (`voice_ws.go:96`, `voice.go:269`). The browser reads the first sharer it finds (`web/src/voice/presence.ts:46`, `sharer`), subscribes to **every** screen-share track while watching (`web/src/voice/sdk.ts:185`, `wanted`), and publishes the capture as its top layer with a bitrate by its longest side (6 Mbps to 1920, 10 to 2560, 18 above) over lower layers of 1080p30 at 4 Mbps and 720p15 at 1.5 Mbps (`sdk.ts:401–470`, #565). **Server:** a room holds `share *voiceShare` (`voice_presence.go:246`) beside `cameras map[int64]*voiceCamera` (`:271`); `POST …/voice/share` takes no body and answers 409 `voice_share_busy` when another holds it (`voice_share.go:205`), `PUT …/companion/audio` likewise (`voice_companion_audio.go:41`); a channel manager or an operator stops "the share" with a bare `DELETE` (`voice_share.go:260`, `stopShare` `:971`); every `UpdateParticipant` already goes through one permission function that takes "holds the share" as an input (`voice_permission_fn.go`); the camera's cap is `voiceCameraCap = 9` (`voice_camera.go:55`) with 409 `voice_camera_full`. **Measurement:** the one-sharer-to-29-viewers figure O6 asked for (#448, A15) is not recorded anywhere in `docs/` (alpha-exit-record B12 is blank).

Two people in one room cannot both show their screens: the second is told someone else is sharing. Nick wants both to share, and wants it to feel like Discord's Go Live: streams as tiles in the call grid, watched on demand, several at once, each with its own sound, with the sharer choosing the quality and the viewer seeing it before watching. Lifting the server's lock alone would break every published client that validates presence, because the one-share rule is part of the published presence document's contract. This note says how two or more sharers reach clients without breaking the old ones, where the new bound is, how the quality travels, and what each client does with several streams.

## 1. Decisions in one page

| Question | Decision |
| --- | --- |
| How two sharers reach clients | A **new presence version, `conch.voice_presence.v2`**: the v1 document without the one-share rule. A client asks for it with `?schema=conch.voice_presence.v2` on `GET /v1/channels/{channel}/voice` and on `GET /v1/voice/ws`; absent, it gets v1 exactly as today (§2.1, §2.2). |
| What an older client sees | A valid v1 document. With two or more shares, exactly one participant is `sharing`: the reader's own share if they hold one, else the earliest-started; the others show no share (§2.3). |
| What a sharing participant carries | `share_started_at` (the claim's time) and `share_quality` (the sharer's declared resolution and frame rate, when declared), both fields of the v2 participant, `VoiceParticipantV2`, and of it alone: the published v1 participant is closed to readers and gains nothing (§2.2, §3.2). |
| The quality | The sharer's choice, #1488's picker: resolution `720p`, `1080p` or `source` and frame rate 15, 30 or 60, sent as the optional body of `POST …/voice/share` (`VoiceShareRequestV1`), on the claim and again to change it while live, and echoed on the claim's answer as `VoiceShareV2` then and only then; v2 presence carries it so a viewer sees "1080p · 60 fps" before watching. A declared label, not enforcement (§3). |
| The cap | **6 streams per room**, `voiceShareCap`, a server constant like the camera's; a seventh claim is 409 **`voice_share_full`**, naming nobody (§4). |
| The claim | `POST …/voice/share` grants a stream to any entitled caller while the room has fewer than 6 and the caller holds none; a holder asking again is answered with the stream held and may change its quality. `voice_share_busy` is never answered again; the constant stays (§5.1). |
| The release | `DELETE …/voice/share[?started_at=…]` ends the caller's own stream. A moderator ends another's with **`?principal=<id>`**, the camera's parameter (§5.2). |
| Who stops whose | Self; holders of `manage_channels`; a manager of the channel. Unchanged (§5.3). |
| Share audio (companion) | Per sharer: `PUT …/companion/audio` consents for the caller's own stream and companion. Where it answered `voice_share_busy` it answers 409 `voice_no_session` (§5.4). |
| Companions, grants, passes, audit | Per principal, as today; "anyone but the holder" becomes "anyone who holds no stream"; each access-lost path ends that person's stream and no one else's; unchanged rows, one set per stream, the started row's detail gaining `quality=` when declared (§5.5–§5.6). |
| Browser: the grid | Every live stream is a **tile in the call grid** (#1487's grid, which shows everyone), beside camera tiles; a stream tile counts as video. It shows the sharer, LIVE, the quality label and **Watch**; nothing is received until Watch (O8). Several streams may be watched at once, each playing in its tile with its own volume and mute, sound on from the first frame. Clicking a watched tile **focuses** it: it takes the stage and the rest go to the strip. **Stop watching** per tile. On a phone: one focused stream and a switcher (§7.1). |
| Layers | The focused stream takes the top layer; a watched, unfocused tile is held to the lowest layer (720p30 once #1488 lands; 720p15 before); an unwatched tile receives nothing (§7.1, §8). |
| TUI, helper, desktop, phone | Each moves to v2 in its own issue; the text names every sharer with their quality; the desktop's companion plays every watched stream's sound and the helper none; a phone watches one focused stream at a time and never shares (§7.2–§7.5). |
| Bandwidth | Estimates in §8 from #565's layers, not measurements: egress is paid per watched tile and dominated by focused streams at the top layer (6 Mbps at 1080p), today's cost per viewer already; the droplet's new cost is up to six streams' top layers in. The measurement O6 asked for is still owed and is an issue (§11.7). |
| Tests | Schema fixtures for v2, the degraded v1 and the quality; server unit; `e2e/voice` scenario `multishare` with two concurrent sharers, the cap, the quality change and the v1 reader; isolation rows for the body, `?principal=`, `?schema=` and one sharer losing access beside another (§9). |
| Split | Seven issues: schema, then server, then the browser (after #1487 and #1488), the TUI, the helper and the desktop in parallel, then the measurement (§11). None touches the approval path. |

## 2. Versioning: the crux

### 2.1 Why a version and not an addition

**Recommendation: a new version, `conch.voice_presence.v2`.**

The skill's test is whether a published reader refuses the new document. It does: the Go and Rust `Validate` of v1 both say "a room has one share at a time", and the TUI, the helper and both e2e harnesses run it on every frame. A document with two sharers is therefore not a v1 document, whatever its fields. Removing the check from `VoicePresenceRoom.Validate` would change no fixture and pass `schema-compat.sh`, and it would still be a mutation of a published version in place: every reader built before it would drop the document, and the TUI would close its subscription. That is the breaking change the skill reserves for a bump and Nick's yes.

The camera avoided this on purpose (ADR-015, design §5, N3): it never put a count in `Validate`, so its cap can move without a schema change. The share's rule predates that lesson. v2 adopts it: no count of streams anywhere in the schema; the cap is enforced at the claim and presence reports whatever was granted.

The two other routes considered:

- **A capability the client declares on its socket** (a header or parameter meaning "I accept many sharers") without a new schema name. It is the same negotiation as below with the version hidden: the document a "many" client receives is one a v1 reader refuses, so it is a different document and must say so in its `schema` field, or a reader cannot recognise it without out-of-band context, which is what the field exists to avoid (`VoicePresenceSchemaV1`'s comment). Rejected as a version under another name.
- **The server degrades for everyone and nobody asks**: pick one share to show and never tell anyone about the second. That keeps v1 and defeats the feature: the browser could not show two tiles. Rejected; but the degradation is right for clients that have not moved, and §2.3 keeps it for them.

The Discord-style revision changes none of this: the quality (§3) is additive, and the grid, the watching and the sound are client behaviour over the same document.

### 2.2 The shapes and the parameter

```go
// pkg/schema/voice.go (sketch; the exact Go lands in §11.1 under the
// schema-change skill)

// VoicePresenceSchemaV2 names the v2 presence document: v1 without the
// one-share rule. Any number of participants may be sharing; how many is the
// server's cap, not the schema's (ADR-015 N3).
const VoicePresenceSchemaV2 = "conch.voice_presence.v2"

// VoicePresenceV2 has the fields of VoicePresenceV1, Schema always
// VoicePresenceSchemaV2, Rooms []VoicePresenceRoomV2. share_ended and
// camera_ended are as in v1: the reader's own, in their document alone.
type VoicePresenceV2 struct { /* as VoicePresenceV1 */ }

// VoicePresenceRoomV2 has the shape of VoicePresenceRoom with
// Participants []VoiceParticipantV2. Validate checks the audience, every
// participant, and no principal twice; it does not count sharers.
type VoicePresenceRoomV2 struct { /* as VoicePresenceRoom, v2 participants */ }

// VoiceParticipantV2 has the fields of VoiceParticipant and two more:
//   ShareStartedAt *Timestamp         `json:"share_started_at,omitempty"`
//   ShareQuality   *VoiceShareQuality `json:"share_quality,omitempty"`
// share_started_at is present exactly when Sharing: the started_at of the
// claim, as VoiceShareV1 or VoiceShareV2 gave it the sharer. share_quality is
// present only when Sharing and the sharer declared one (§3.2). Validate:
// either needs sharing; sharing needs share_started_at. A v2 participant who
// is not sharing encodes to the same bytes as a v1 participant.
//
// VoiceParticipant (v1) is unchanged: a published shape gains no field (see
// below).

// VoicePresenceSchemaParam is the optional query parameter of
// GET /v1/channels/{channel}/voice and GET /v1/voice/ws naming the presence
// version the caller reads: VoicePresenceSchemaV1 or VoicePresenceSchemaV2.
// Absent means v1. Any other value is 400 invalid_request, read before the
// channel, so it says nothing about the channel.
const VoicePresenceSchemaParam = "schema"
```

**Why `share_started_at`.** Every client needs one deterministic order for several streams: the tiles' order in the grid, and which stream a v1 reader is shown (§2.3). Participants are ordered by `principal_id`, which is stable but says nothing about the stream; "the stream that started first" is what a person expects to stay where it is when a second starts, and the server already holds it (`voiceShare.startedAt`). It is a field of the v2 participant, omitted when not sharing, so a v2 participant who is not sharing is byte for byte a v1 one. It also gives a reader the stream's identity, which the sharer already has for their own (`share_ended.started_at`): a stream that stopped and started again has a new value. **Why on the participant and not a new `shares` list:** the stream stays the participant's attribute, as the camera is; a second list keyed by principal would say the same thing twice and a reader would have to join them.

**Why not two optional fields on the v1 participant.** The first draft of this note added them to `VoiceParticipant` "in v1 and v2 alike", reasoning that an old reader skips a field it does not know. That is true of the Go and Rust readers and false of the browser's: the generated validators (`web/src/schema/validate.ts`, `object()`) refuse a key the TypeScript type does not have, by design, so that a type behind Go fails loudly. A v1 frame carrying `share_started_at` would therefore be refused by every browser tab served before the change and not reloaded (`parsePresence` returns null and the frame is lost), which is exactly the tab §2.3 is written for, and the review of #1507 (PR #1526) caught it with the published validators run against the proposed fixtures. The rule that follows, and that `pkg/schema/voice_v1_frozen_test.go` now enforces: **a published shape gains no field, optional or not; a new field is a new version.** So `VoiceParticipantV2`, `VoiceShareV2`, and a v1 document that is exactly what today's server sends.

**Why a query parameter and not a path.** `/v2/ws` versions the message socket by path, but the presence routes sit under `/v1/channels/{channel}/voice` and `/v1/voice/ws` with `?channel=` and `?ticket=` already; a `/v2/…` pair would be a new route class with its own matrix rows and the same handler behind it. One parameter on the two routes that serve presence, read before the channel, is the smallest explicit choice. The occupancy document is unchanged and takes no parameter. The parameter is `schema` because its value is a schema name, the same string the document carries.

**The TypeScript.** `go generate ./pkg/schema` emits `voicePresenceV2`, `voicePresenceRoomV2` and `voiceParticipantV2` beside the v1 three, `voiceShareV2` and `voiceShareRequestV1`, and the quality types of §3.2 (`VoiceShareResolution` strict); the v1 validators are byte for byte what they were; `ts.go` gains the v2 rows and the `voice-presence-v2`, `voice-share-request-v1` and `voice-share-v2` fixture families. The Rust twin (`conch-voice-api`) gains the same types by hand (§11.5), checked against the same fixtures as today.

### 2.3 Exactly what an older client sees

A reader of v1 (no `?schema=`, or `?schema=conch.voice_presence.v1`) gets a document that satisfies v1's `Validate` by construction, built from the same room state:

| Streams in the room | The v1 document |
| --- | --- |
| none, or one | byte-identical to today. The start time and the quality are not in it: a v1 reader's validators are closed to them (§2.2). |
| two or more | every participant is listed as today, with v1's fields and no more; **exactly one** carries `sharing` and `sharing_audio`: **the reader's own stream if they hold one, otherwise the earliest-started**. The other sharers are listed as if not sharing. `share_ended` and `camera_ended` are the reader's own, unchanged. The fixture is `voice-presence-v1-degraded.json`, the v1 twin of `voice-presence-v2.json`. |

**Why own-first.** The one v1 reader that also shares is a browser tab served before the upgrade and not reloaded (`conchd` serves `/app/`, so the browser is otherwise never older than the server). Its share state machine must see its own `sharing` to behave; showing it the earliest stream instead would make it offer "Watch Sam's screen" while it shares. **Why earliest for everyone else.** A v1 viewer watching Sam must not have the picture swapped under them because Val started; the earliest stream stays theirs until it ends, and then the next-earliest appears. **What it costs.** A v1 reader is not told about the other streams. In practice that reader is the TUI, whose share text names one person, and the helper, which ignores the sharing flags. No first-party client stays on v1 after §11; v1 remains served and published, and retiring it is a later decision, not this one.

**The server's own check.** `snapshotLocked` takes the version beside the viewer, builds the document the reader asked for, and validates it as it does today; a v1 snapshot can never fail the one-share rule because it never sets `sharing` on two.

## 3. The quality: the sharer's choice, carried in presence

### 3.1 Per stream

**Recommendation: the sharer's quality is a declaration the client sends `conchd` with the claim and whenever it changes, and presence repeats it to viewers. It is a label, not enforcement.**

#1488 gives today's single share Discord's dialog: resolution (720p, 1080p, Source) and frame rate (15, 30, 60), chosen before starting and changeable while live. Per stream that is already the right unit: the choice is the sharer's publish parameters for their own track (the top layer's size, frame rate and bitrate row; the lower layers follow #565's rule beneath it), and nothing on the server or in another stream depends on it. A change while live is a client act on its own publication (the SDK republishes the encodings); no claim, no grant and no LiveKit call by `conchd` is needed for it.

**Should presence carry it?** Yes. A viewer decides whether to press Watch on a tile that says "1080p · 60 fps" or "Source · 30 fps", as Discord's does, and on a phone or a slow link the label is the reason to leave a stream unwatched. `conchd` cannot observe it: LiveKit's listing reports a track's size, not its frame rate or the sharer's intent, and a listing lags a pass. So the sharer tells `conchd`, and presence carries what was declared. The server does not police bitrate or resolution (O6: the publisher chooses), and the label says so by being the declaration.

**How it travels.** `POST /v1/channels/{channel}/voice/share` takes an **optional body**, `VoiceShareRequestV1 {quality?}`, decoded strictly as the companion routes are (an unknown field, trailing content, a resolution or frame rate outside the vocabulary: 400 `invalid_request`; over 1 KiB: 413; an empty body: no quality, so a client from before #1488 is unchanged). On a first claim it is recorded with the claim; **a holder asking again with a body changes the quality** and presence is notified, with no row and the idempotent re-grant the repeat already makes; a holder asking again without a body keeps what was declared. **Bounded (#1546):** a repeat that changes the quality is limited per principal to three in any three seconds; the first change is never refused, a repeat that leaves the quality alone is not a change, and over the bound the answer is 429 `rate_limited` with `Retry-After`, the quality unchanged. **The answer:** a claim whose body declared a quality is answered **`VoiceShareV2`**, which is `VoiceShareV1` plus the `quality` as recorded, then and only then; a claim with no body, or a body that declares nothing, is answered `VoiceShareV1` exactly as today, so a client from before the quality never sees a field its closed validator would refuse, and a client that sent a quality knows to read v2 (§11.2 implements the choice; §11.3 reads it). The body is read after the caller's checks, as the companion route reads its body after step 5, so a non-member learns nothing whatever they send.

### 3.2 The shapes

```go
// VoiceShareResolution is the sharer's declared resolution: the picker's
// vocabulary (#1488). Closed and case-sensitive; a value outside it fails
// validation. "source" is the capture's own size.
type VoiceShareResolution string
const (
    VoiceShareResolution720p   VoiceShareResolution = "720p"
    VoiceShareResolution1080p  VoiceShareResolution = "1080p"
    VoiceShareResolutionSource VoiceShareResolution = "source"
)

// VoiceShareQuality is what the sharer declared: resolution and frame rate.
// FrameRate is one of 15, 30, 60; anything else fails validation.
type VoiceShareQuality struct {
    Resolution VoiceShareResolution `json:"resolution"`
    FrameRate  int                  `json:"frame_rate"`
}

// VoiceShareRequestV1 is the optional body of POST …/voice/share.
type VoiceShareRequestV1 struct {
    Quality *VoiceShareQuality `json:"quality,omitempty"`
}

// VoiceShareV2 is the answer to a claim that declared a quality: VoiceShareV1
// and the quality as recorded, always present. VoiceShareV1 is unchanged.
type VoiceShareV2 struct {
    PrincipalID int64             `json:"principal_id"`
    StartedAt   Timestamp         `json:"started_at"`
    Audio       bool              `json:"audio"`
    Quality     VoiceShareQuality `json:"quality"`
}
```

A new resolution or frame rate is a new value of a closed vocabulary, which for presence means a new version, as a new share-end reason does; the picker's three-by-three is what Discord has offered for years, and `source` already covers anything larger. Audit: `voice_share_started`'s detail gains `quality=<resolution>/<fps>` when declared; a change writes no row (it is a publish setting, not a security event; the audit says what was shared and by whom, not how sharp).

## 4. The cap

**Recommendation: `voiceShareCap = 6` streams per room, a constant in code, not a setting and not a schema check. A seventh claim is 409 `voice_share_full`, naming nobody. The browser says "This call already has 6 streams live."**

- **Why a cap at all, when Discord has none.** Discord's media servers are a fleet; ours is one droplet (rule 1), and a stream costs the droplet its top layer in whether or not anyone watches it. Without a bound, a room of thirty could put thirty captured screens on a 2-vCPU droplet. The cap also bounds the grid.
- **Why 6 and not 4.** The first draft chose 4 with a strip-and-thumbnails view, where every viewer paid for every stream. Under Discord-style watching a viewer pays only for the tiles they press Watch on (§7.1), so the cap no longer protects viewers; it protects the droplet's ingress and the grid. Six streams at the picker's largest choice (Source at 4K60, the 18 Mbps row) are about 140 Mbps in with their lower layers, the same order as the egress the alpha was sized against (one 1080p share to 29 viewers, about 180 Mbps, §8); at #1488's likely default of 1080p30 they are about 70 Mbps. Nine, the camera's figure, would be about 210 Mbps in at the top row and leave no grid for cameras: stream tiles and camera tiles share the 3 × 3 grid of §7.4, and six streams beside a few cameras fills it. Two or four would refuse the fifth friend in a ten-person room, which is the refusal Nick set aside.
- **It counts claims**, so it is exact and needs no pass: a holder asking again is not a new claim; a pending claim counts (its row is being written); a stream that ended frees its place the moment it ends, before LiveKit is asked to withdraw. It does not limit who is in voice.
- **The refusal writes nothing**: no row, no LiveKit call, as `voice_camera_full` and `voice_share_busy` write nothing today. It is read under the room's claims, after the entitlement re-read, so a revoked caller gets `voice_not_allowed`, not `voice_share_full`.
- **It can move** once §11.7 has measured, without a schema change (ADR-015 N3).
- **No rate bound is added.** The camera's bound exists because its continue-and-adopt traffic costs a LiveKit call per request under the lock; the share's claim has the same shape today and has no bound, and a quality change (§3.1) adds a presence frame, as a claim and a release do today. If a room's log or its presence fills with changes, a manager stops the sharer (§5.3); a bound is a separate issue if measurement shows the need.

## 5. Server

### 5.1 The claim

`POST /v1/channels/{channel}/voice/share` keeps every check of voice-control-plane §12 in its order (caller, `share_screen`, session, connection), then reads the body (§3.1). Under the room's claims:

- the caller holds a stream (`r.shares[caller]`, started or pending): the sources are granted again and the stream held is answered, as today; with a body, its quality is recorded and presence notified; nothing written;
- else `len(r.shares) >= voiceShareCap`: 409 `voice_share_full`;
- else the claim is recorded, pending, with its quality, before LiveKit is asked, as today; the person's `p<id>` is granted the share's sources and their `p<id>.screen`, if listed, the screen; `voice_share_started` is written; 200 `VoiceShareV1`.

`r.share *voiceShare` becomes `r.shares map[int64]*voiceShare` keyed by principal, the camera's shape, with `quality` on each. One stream per person is the invariant (a person is one participant in presence, with one companion), so "asking again while holding it" keeps its meaning. **`voice_share_busy` is no longer answered by anything.** The constant `ErrorCodeVoiceShareBusy` stays in `pkg/schema` with its comment amended ("answered by no conchd from this version; kept for readers"), and the TypeScript keeps emitting it: removing a published constant breaks Go and TypeScript readers at compile time for no gain.

### 5.2 The release

`DELETE /v1/channels/{channel}/voice/share[?started_at=…][&principal=<id>]`, on the camera's pattern (`VoiceCameraPrincipalParam`):

- **no `principal`, or the caller's own id:** the caller's own stream. With `started_at`, only if that is the caller's current stream, else 409 `voice_share_stale` and nothing changes; the stale rule is now against the caller's stream, not "the room's", which is the only reading that exists with several. Without it, the caller's stream, or 204 when they hold none.
- **`principal` naming someone else:** the moderator's stop (§5.3); 403 `forbidden`, audited `access_denied`, for anyone else; 204 when that person holds no stream in the room, nothing written, naming nobody, the same answer as for an id that never existed (the isolation matrix's rule). `started_at` beside a `principal` that is not the caller is 400 `invalid_request`: a moderator stops a person's stream, not a claim.
- malformed or repeated parameters are 400 before the channel is looked at, as today.

**What changes for callers.** Today a moderator's bare `DELETE` ends "whoever shares". With several streams that request is ambiguous, and the only published client that stops a share is the browser's own sharer, which always names its `started_at` (`connection.ts:383`); no CLI command and no operator control sends a bare `DELETE`. So the bare request becomes "my own stream" with nothing published depending on the old reading, and a moderator names the person, as they do for a camera. The new constant is `VoiceSharePrincipalParam = "principal"`.

### 5.3 Who may stop whose

**Recommendation: unchanged. The sharer; a holder of `manage_channels`; a manager of the channel (`ManagesChannel`).** **Amended under #1546 (2026-11-07): the holder of `manage_channels` must be a human.** An agent that holds the permission is checked as a plain member and gets the same 403 `forbidden`; the camera stop refuses it likewise. Agents do not moderate voice.

ADR-015 O7 made the camera stricter (operators only) with the argument that "the share is one per room, so a manager must be able to free it for the room". With six places instead of one the argument weakens but does not fall: the places can still all be taken, a stream is what the room looks at, and a manager's job in their channel is what the room shows (admin-roles §7.3). Narrowing a published behaviour to match the camera would take a tool from managers to gain symmetry; the asymmetry is stated here and in the camera note already. Managers of the channel and operators also keep the voice kick.

### 5.4 Share audio, per sharer

`PUT /v1/channels/{channel}/voice/companion/audio` records the caller's consent on **the caller's own stream** (`r.shares[caller].audioScope`) and grants `screen_share_audio` to the caller's own `p<id>.screen`, under the room's claims, as voice-control-plane §13 says. The one check that changes: "the caller holds the room's share (held by someone else: 409 `voice_share_busy`)" becomes "the caller holds a stream (none, or pending: 409 `voice_no_session`)". `DELETE …/companion/audio` is unchanged. Every end of a stream ends its own consent and no other's; `voice_share_audio_stopped` precedes that stream's `voice_share_stopped`.

Presence `sharing_audio` is per participant, as today: the person's own audio track, or their companion's with their consent recorded. Two sharers with sound are two participants with `sharing_audio`; each viewer hears those they watch (§7.1).

### 5.5 Companions, grants and the passes

- **Companions:** nothing changes. A companion is `p<id>.screen`, one per person, refused (409 `voice_companion_busy`) when a second is asked for, folded into the person (§13). N sharers are N companions, each granted its own person's screen on its own person's claim, each withdrawn on its own person's end. The viewer identity `p<id>.view` publishes nothing and receives whatever streams it subscribes to.
- **Grants** are per participant through the permission function (`voice_permission_fn.go`), whose "holds the share" input is now `r.shares[pid] != nil`. Nothing else in the function changes; the structural test over every `UpdateParticipant` call site holds.
- **Enforcement** on each pass: today "anyone but the holder" with a screen-share source granted or published has it withdrawn; now **anyone who holds no stream** does. `judgeShareLocked` loops over `r.shares` for the holder half (left, access_lost, idle, the audio flag) and keeps its `consider` half per participant, keyed as today by `voiceWithdrawKey{pid, role}`. Unclaimed shares are audited and evicted as today.
- **Idle** is per stream: 15 s without a first track, 5 s after the last. One sharer's idle never touches another's.
- **Access lost.** `endSharesOf(principalID, channelID, reason)` already takes a principal; `sharesWithheld` (the per-pass `share_screen` read) runs over every holder; `endWithheldShare` on the claim path and the voice switch's hook (#984, `voiceAfterVoiceOff`) end the one person's stream with `access_lost`. **No other stream in the room is touched**, and the room is not rotated by a voice switch (#984's rule). A removal or disable that rotates the room ends every stream `rotated`, as it ends everyone's connection today, and each browser resumes its own in the new room (`share.ts`'s `rotated` path), declaring its quality again.
- **Restart** drops every claim, as today; the first pass withdraws every grant without a claim.
- **Metrics.** `conch_voice_shares_active` counts started streams across rooms; its meaning ("screen shares under way") is unchanged and it may now exceed the room count.
- **Rooms.** Only a channel-wide room has streams, as today.

### 5.6 Audit

No new action. `voice_share_started` (`channel=<id> audio=<bool>`, actor the sharer) gains `quality=<resolution>/<fps>` when declared; `voice_share_stopped` (`channel=<id> principal=<sharer> reason=<reason>`, actor the sharer or the moderator), `voice_share_unclaimed`, `voice_share_audio_started` and `voice_share_audio_stopped` are written per stream exactly as today; the `principal=` in the detail already tells one sharer's rows from another's. A refused claim (`voice_share_full`, like `voice_share_busy` before it) writes nothing; a quality change writes nothing (§3.2). Every stream that started gets exactly one stopped row.

## 6. Where it travels

- **Presence**, v1 and v2, on `GET /v1/channels/{channel}/voice` and `GET /v1/voice/ws`, by `?schema=`; the quality and the start time on the sharing participant. No new event document: presence is a full snapshot on its own socket.
- **Occupancy** is unchanged: it counts people, never streams.
- **MCP:** nothing. Agents are not in voice (voice-control-plane §1), and no voice tool exists to keep in step. Parity (rule 4) holds through REST: everything a client does here is a REST call, and the CLI gets the operator's stop (§7.2).
- **`VoiceSessionResponseV1`, `VoiceRoomGrant`, the companion types:** unchanged. `VoiceShareV1` gains the optional `quality` echo.

## 7. Clients

### 7.1 Browser

**The grid.** #1487 makes the call grid show everyone in voice by default, camera-off people as avatar tiles, with a "Hide non-video participants" toggle. This note puts **every live stream into that grid as a tile of its own**, beside the sharer's camera or avatar tile, in `share_started_at` order among the stream tiles (newest last; nothing reorders while streams continue). **A stream tile counts as video**: the toggle hides avatars, never a stream. The tile shows the sharer's name, a LIVE badge, the quality label from `share_quality` ("1080p · 60 fps"; "Source · 30 fps"; nothing when the sharer declared none), and a **Watch** button. A sharer's own stream is a tile too, labelled "You · LIVE" with Stop sharing in it and no Watch (they have the screen).

**Watching.** **Nothing is received until Watch** (O8's rule, and Discord's): an unwatched tile holds a still card, no subscription. Watch subscribes to that stream's video and sound and plays both in the tile at once; the click is the gesture browsers want, so there is no unmute step (the existing tap-to-start fallback stays for a phone that still blocks it). **Several streams may be watched at once**, each in its tile, each with **its own volume and mute** on the tile, apart from voice; the sharer's `sharing_audio` tells the tile whether there is sound to control. **Stop watching** on a tile unsubscribes it and returns the card. **Clicking a watched tile focuses it**: it takes the stage as the camera's enlarge does (webcam §7.4), and every other tile, streams and cameras alike, goes to the strip; clicking another watched tile focuses that one; Escape or "Back to grid" returns. Full screen works on the focused stream as it does today. A watched stream that ends leaves the grid with the existing "{name} stopped sharing." note; if it was focused, the view returns to the grid.

**Layers.** The focused stream is subscribed at the **top layer** (the SDK's adaptive stream at stage size selects it). A **watched, unfocused** tile is **held to the lowest layer**, 720p30 (#1488 raises it from 720p15) (`setVideoQuality(LOW)` on its publication), whatever the tile's size on a large monitor, because a grid tile is a preview and the top layer at 6 Mbps in several tiles is what makes "watch three streams" expensive. An **unwatched** tile subscribes to nothing. `wanted` learns which publishers are watched and which is focused, keyed by principal as `claimed` keys cameras today; the sound of a watched stream is wanted whether or not it is focused.

**Sharing.** Share screen is available whenever this page could share, whoever else is; the start goes through #1488's picker and sends the choice as the claim's body; changing it while live republishes the encodings and re-POSTs the body (§3.1). `busyNote` and `oneSharerNote` ("One person shares at a time.") go; 409 `voice_share_full` says "This call already has 6 streams live." The sharer's own lifecycle (claim, publish, `share_ended`, the `rotated` resume, the named release) is unchanged. A sharer may watch others' streams while live.

**The chooser and Auto (amendment 2026-11-11, #1659, within the accepted decisions; Nick's answers to Q9 to Q11 are on #1614).** Beside Share screen the strip has a "Share quality" menu button (`aria-haspopup`) that opens the picker's choices, the current one checked; Share screen alone still shares at once at the last choice, and Voice options' two selects stay, on the same state. Resolution gains **Auto**, a **local preference only**: the wire vocabulary of §3.2 is closed and unchanged, so a claim and a viewer's tile always carry a concrete value. The claim comes before the capture, so Auto declares the provisional `1080p/30` on the first claim and, once the capture is known, the resolved value on the repeat claim that already follows the picker (a holder's repeat, §3.1; the first change is never refused under #1546). The rule: a capture whose longest side is 1920 px or less is shared as it is (`source`), a larger one at `1080p`, always at 30 fps, with `contentHint` by the existing frame-rate rule. Source is labelled "Source (up to 4K)", which is what `shareAsk` already asks for; no 1440p or 2160p choice is added (they wait for a new presence version, #1661). Each choice shows "about N Mbps up (estimate)", from the top-layer rows and lower layers of `sdk.ts` (the code's rows; §8's table is prose and older), labelled an estimate because §8 is not a measurement (#1513); Source at 60 fps shows a one-line caution that many computers and browsers cannot encode it, and the sharer's measured "Sending" line says what was sent (#1660). The desktop companion never offers Auto and reads a stored `auto` as its own default, 720p30; it offers the same other rows (720p30, 1080p30, 1080p60, Source at 30 and 60) and the same Sending line (Q-D4-3, lifted 2026-11-11, #1772; linux-desktop.md).

**Phone layout** (narrow, below the existing 700 px rule): the grid is #1487's phone form; stream tiles show the label and Watch; **one focused stream at a time**, filling the width with its volume and mute, and a "Watching {name}'s screen" switcher chip listing the live streams: switching stops the previous stream and watches the next, picture and sound together. A phone never has two streams playing, for data and for the screen.

**Presence.** `useVoice` asks for `?schema=conch.voice_presence.v2` and reads `voicePresenceV2`; `sharer(presence)` becomes `sharers(presence)`, ordered by `share_started_at`, each with its quality.

### 7.2 TUI and CLI

The TUI reads v2 and names every sharer in its presence text: "sam is sharing a screen (1080p · 60 fps)" for one; "sam and val are sharing their screens (val with sound)" for several, with each quality in `conch voice status` beside `since`. It has no share control and gets none. For parity with the camera's operator stop, the CLI gains `conch voice share stop <channel> <person>` (`DELETE …/voice/share?principal=<id>`), with the hostile-server tests the camera's command has. A TUI not yet updated keeps reading v1 and names one sharer (§2.3).

### 7.3 The voice helper (`conch-voice`)

`conch-voice-api` gains the v2 types and the two participant fields, and asks for `?schema=conch.voice_presence.v2` on its presence stream, so the helper never reads the degraded document. It shows no share text and plays share audio by the existing rules (#906: never its own companion's tracks; never other participants' `ScreenshareAudio` when its companion owns share playback), and on the Linux desktop publishes the person's share sound ([linux-share-audio.md](linux-share-audio.md); amended 2026-11-13, #1585, and that note's own issues). The playback rules hold with several sharers unchanged: with a companion, every watched stream's sound is the companion's to play. One small issue.

### 7.4 Desktop (Electron companion, model B)

The server side needs nothing: the desktop's stream is its person's claim and its own `p<id>.screen`, one per person. The renderer is the web client, so §7.1 is the desktop's view too, with one desktop-specific rule: under B2 the companion subscribes to **the watched streams'** tracks by publisher, the focused one at the top layer and the others at the lowest, never "every screen track", and never its own companion's (#906, unchanged); it is the one playback owner of every watched stream's sound, each with its tile's volume and mute, and the helper plays none. Main's teardown (stop the stream, disconnect the companion on `leave`, lock, sign-out) is unchanged. A desktop may stream and watch others at once.

### 7.5 Phone viewers

A phone never shares (mobile-app §3.4, §8, unchanged). The PWA on a phone gets §7.1's phone layout: one focused stream and the switcher. The native phone viewer (#1118, future) inherits the same rule and reads v2; mobile-app.md gets a one-line amendment pointing here when this note is accepted.

## 8. Bandwidth and layers

These are estimates from the layers #565 fixed in `sdk.ts` and the picker's choices, **not measurements**; the measurement O6 asked for (one sharer to 29 viewers on the droplet, #448) was never recorded, and §11.7 owes both it and the N-stream figures. The picker's default is #1488's; this note assumes 1080p at 30 fps, and the figures move with it.

| Stream layer | Bitrate (#565) | Who takes it |
| --- | --- | --- |
| Top layer, 720p (picker) | about 2.5 Mbps | a focused viewer of a 720p stream |
| Top layer, 1080p (picker) | up to 6 Mbps | a focused viewer of a 1080p stream |
| Top layer, Source (picker) | 6, 10 or 18 Mbps by the capture's longest side (to 1920, to 2560, above) | a focused viewer of a Source stream |
| Lower layer 1080p30 | 4 Mbps | a focused phone or a small stage |
| Lowest layer 720p30 (#1488; 720p15 at 1.5 Mbps before it) | 2 Mbps | every watched, unfocused tile |
| Share audio (music preset, stereo) | about 0.1–0.25 Mbps | every watched stream, focused or not |
| Unwatched | 0 | — |

- **A sharer's uplink and the droplet's ingress, per stream:** top layer plus the lower layers that are smaller than it: about 11.5 Mbps at 1080p (6 + 4 + 1.5), 4 Mbps at 720p (2.5 + 1.5), up to 23.5 at Source 4K (18 + 4 + 1.5). **Six streams in:** about 70 Mbps at the assumed default, about 140 at the picker's largest. This is the cost the cap bounds and the new cost of the feature; it is forwarded without transcoding, so it is network and a little CPU, not encoding.
- **Egress, today's single share, 29 viewers, each on the full stage:** 29 × about 6.2 ≈ **180 Mbps** at 1080p (the first draft of this note said 75 by taking the 1080p30 lower layer; the stage takes the top layer). This is the figure the alpha was to be sized against and never measured.
- **Egress, Discord-style, 29 viewers, six streams live, each viewer watching one focused and two unfocused:** 29 × (6.2 + 2 × 1.7) ≈ **280 Mbps**; each viewer watching one focused only: the 180 of today. Viewers who watch nothing cost nothing: egress is pay-per-Watch, and the room's worst case is a product of the viewers' choices, not of the cap.
- **Transfer.** 280 Mbps for an hour is about 125 GB against a 4 GB droplet's monthly allowance of 4 TB: roughly thirty such hours a month. A two-hour evening with three friends streaming to six viewers is a few GB.

The levers, in order, if the measurement finds a problem: the picker's default (720p halves the focused cost; the sharer's choice is the strongest lever and the one Discord uses), the unfocused layer (already the lowest; a smaller simulcast layer from the sharer is a web change), the cap (a constant), and O6's own answer, a larger droplet. None is a schema change.

## 9. Tests

### 9.1 Schema (§11.1)

- Fixtures: `voice-presence-v2.json` (two sharers with `share_started_at`, one with `share_quality` and sound, one undeclared, a camera), `voice-presence-v2-share-ended.json`, `voice-presence-v1-degraded.json` (the v1 document built from the same two-sharer state: one `sharing`, v1's fields and no more), `voice-share-request-v1.json`, `voice-share-request-v1-empty.json` (`{}`), `voice-share-v2.json`; every existing fixture byte-identical; **no v1 fixture carries a field the published v1 type lacks.**
- **Compatibility test** (`voice_v1_frozen_test.go`): frozen copies of the v1 presence, share and camera types as main published them at `93453e8b`, never edited, decode every fixture of the `voice-presence-v1`, `voice-share-v1` and `voice-camera-v1` families with unknown fields refused and encode them back byte for byte. It matches families as the web test does (`<family>.json` and `<family>-*.json`), so a fixture added later to a v1 family is checked without a change. It stands for the browser's closed validators (§2.2), whose refusal of an unknown key is the property it holds; it failed on the first draft's three v1 fixtures before the rework and passes after.
- `Validate` cases: v2 accepts two sharers (and ten: no cap); v1 still refuses two; on `VoiceParticipantV2`, `share_started_at` or `share_quality` without `sharing` fails and `sharing` without `share_started_at` fails; `sharing_audio` still needs `sharing` in both; a v2 participant who is not sharing encodes as the v1 participant does; `VoiceShareQuality` refuses a resolution outside the three and a frame rate outside 15, 30, 60; `VoiceShareV2` requires its quality; `VoiceShareRequestV1` decodes strictly (unknown field, trailing content, `null`) and accepts an empty body; the degraded v1 room validates and carries no `share_` key.
- `schema-compat.sh` green; the generated TypeScript (`VoiceShareResolution` strict) checked against the fixtures; the Rust types against the same fixtures.
- `voice_test.go`'s reflection tests still hold: no token, room name or device anywhere in v2, the v2 claim, the claim's body or the quality; the v1 field list is exactly what it was.

### 9.2 Server unit (§11.2, fake LiveKit)

- The claim, table-driven: sixth claim 200, seventh 409 `voice_share_full` with nothing written and no LiveKit call; a holder's repeat answers the held stream and does not count; a holder's repeat with a body changes the quality, notifies presence and writes no row; a first claim with a body records it and its started row says `quality=`; a malformed body is 400 after the caller's checks and before the lock; a revoked caller gets `voice_not_allowed` before the cap; after one stop the seventh claim succeeds.
- The release: own, by `started_at` (stale against the caller's stream), by `principal` for a manager, an operator and a plain member (403, audited), `started_at` with another's `principal` (400), `principal` naming a non-sharer (204, nothing written).
- Companion audio: consent on the caller's own stream only; a second sharer's companion is never granted audio by the first's consent; each end writes its own audio row first.
- The passes: two holders, one leaves (`left` for one, the other untouched); one's `share_screen` cut in the store (`access_lost` for one); idle per stream; a non-holder's unclaimed track beside two holders; rotation ends both `rotated` with two rows in order.
- The snapshot: v2 lists both with their fields; v1 as viewer A (a sharer) shows A; as a non-sharer shows the earliest; both validate; `?schema=` unknown is 400 before the channel; absent is v1 byte-for-byte with one undeclared stream.
- `metricCounts` with two started streams and one pending.

### 9.3 `e2e/voice`, a new scenario `multishare` (§11.2)

Against the pinned LiveKit, with `lk` participants as `share.go` does: **sam and val both claim (200, 200), sam with a body of 1080p/60 and val with none, and both publish a screen, val with sound**; una, reading v2, sees two `sharing` participants with their `share_started_at`, sam's `share_quality` and none on val, and her `lk` is subscribed to both screens; **a v1 reader of the same room sees exactly one sharer, sam, and the document validates**; sam re-POSTs 720p/30 and una's next frame says so with no new row; four more members connect and claim (200 each: the room now holds six), and **a seventh member's claim is 409 `voice_share_full`** with no `voice_share_started` row; sam stops with his `started_at`: his tracks are gone for una at once, val's are still listed, the v1 reader now shows val, and the seventh claimant's retry succeeds; **an operator who is not a member stops val's stream with `?principal=`** and val's `voice_share_stopped` says `reason=operator`; a late release from sam naming his ended `started_at` is 409 `voice_share_stale`; the `companion` scenario adds a line: two companions, each refused `screen_share_audio` until its own holder's PUT and refused again after it, and one holder's PUT never reaches the other's companion. `sharingIn` becomes `sharersIn` over v2; sam's claim with a body is answered `VoiceShareV2` and val's without one `VoiceShareV1`. `share.go`'s existing assertion that a second claim is busy (`share.go:249`) becomes the assertion that it succeeds.

### 9.4 `e2e/isolation` (§11.2)

- **Matrix rows:** `POST …/voice/share` with a body, by B on A's channel: 404 before the body is read, whatever the body; `DELETE …/voice/share?principal=<A member>` by B's member, B's manager and B's operator-made member on A's channel: 404, no marker, the same as a ghost id; `GET …/voice?schema=conch.voice_presence.v2` and the socket with it, the same refusals as the v1 rows; `?schema=bogus` 400 before the channel, saying nothing. The coverage check keys on routes, so no route is left without a row.
- **Media half** (`media.go`): B's member and B's manager, in B's own channel, `?principal=<A's sharer id>`: 204, nothing written, no marker. The row at `media.go:787` that waited on `voice_share_busy` for den's previous sharer to be found gone is rewritten: the second claim now succeeds at once, and the row asserts the previous stream still ends `left` within the bound.
- **Voice gates** (`voicegates.go`): a two-sharer variant of `sharesWithheldRow`: two members stream in one room; the owner flips **one** member's voice switch (`PUT /v1/principals/{id}/voice-permission`, the hook with no rotation); within the bound that member's stream ends `access_lost` and their tracks are withdrawn, **and the other's stream stays listed, published and in v2 presence**, with no rotation and no row of theirs. With the switch back the member claims again (200: the room has a free place).
- **Revocation** (`revoke.go`): the disabled tester who holds "every socket variant, a LiveKit participant and the share" now holds one of two streams; the other tester's stream is listed before and after.

### 9.5 Web (§11.3)

- Vitest: `sharers()` order by `share_started_at` and its quality label (declared, undeclared); `wanted` with two sharers (a watched tile gets video and audio, the focused one at the top layer and the unfocused held to LOW, an unwatched tile nothing, own stream never); the grid with stream tiles among cameras and avatars, the toggle keeping streams; focus and back; a watched stream ending; per-tile volume and mute independent of each other and of voice; the phone switcher stopping the previous stream; the notes (`voice_share_full`, no busy note); the claim body sent from the picker and on change; the generated `voicePresenceV2` and `voiceShareRequestV1` against the fixtures.
- Playwright (fake devices, `make web-e2e`): two contexts stream (one picks 720p/15, the label shows on the other's tile), a third sees two tiles with Watch and receives nothing until it presses one, watches both and hears both with separate mutes, focuses one and the other goes to the strip, stops watching one, the first sharer stops and the tile leaves; with four more headless sharers from the harness a seventh context is told the call has 6 streams live.

## 10. Published-shape changes for the owner's sign-off

| Change | Kind | Nick? |
| --- | --- | --- |
| **`conch.voice_presence.v2`**: `VoicePresenceV2`, `VoicePresenceRoomV2`, with no one-share rule; `VoicePresenceSchemaParam` (`?schema=`) on `GET /v1/channels/{channel}/voice` and `GET /v1/voice/ws`, absent meaning v1 (§2.1, §2.2) | **version bump** | **yes** |
| The v1 degradation under several streams: one `sharing`, the reader's own else the earliest-started (§2.3) | the compatibility story of the bump; within v1's contract | **yes**, with the bump |
| `VoiceParticipantV2`, the v2 participant: v1's fields and `share_started_at` (present iff `sharing`) and `share_quality` (present only when `sharing` and declared); `VoiceShareQuality`, `VoiceShareResolution` (`720p`, `1080p`, `source`), frame rate 15/30/60 (§2.2, §3.2). `VoiceParticipant` (v1) unchanged. | part of the v2 version; new closed vocabulary (a new value is a new presence version) | no (within the bump) |
| `VoiceShareRequestV1 {quality?}` as the optional, strictly decoded body of `POST …/voice/share`; a holder's repeat with a body changes the quality; `VoiceShareV2`, the answer to a claim that declared one, `VoiceShareV1` unchanged and still the answer to every other claim (§3.1, §3.2) | new type, additive body on a route that had none, new response type chosen by the request | no |
| Published v1 shapes gain no field, optional or not (§2.2, "Why not two optional fields"); `voice_v1_frozen_test.go` holds them | the rule the amendment of 2026-11-07 adds; no shape change | no, noted |
| `voice_share_full` (§4) | additive error code | no |
| `VoiceSharePrincipalParam = "principal"` on `DELETE …/voice/share`; a bare moderator `DELETE` now means the caller's own stream (§5.2) | additive parameter; changed reading of a request no published client sends | no, noted |
| `voice_share_busy` kept, answered by nothing; `PUT …/companion/audio` answers `voice_no_session` where it answered `voice_share_busy` (§5.1, §5.4) | doc change; changed refusal, no shape change | no, noted |
| `voice_share_started` detail gains `quality=` when declared (§5.6) | additive audit detail | no |
| `voiceShareCap = 6` (§4) | a server constant, not a shape | **yes, as the product decision replacing O5**, as O6's figures were his |
| Who may stop whose stream: unchanged (§5.3) | no change | no |
| `VoiceSessionResponseV1`, `VoiceRoomGrant`, the companion types; occupancy; the other audit actions | unchanged | — |

On acceptance, the same PR that flips this note's status adds to private-alpha.md O5 the line "*Superseded by #1483 (2026-11-07): several streams per room, capped, watched on demand; see voice-multi-share.md*", as O6 carries for #565, amends voice-control-plane §12's opening rule in place with the same pointer, adds the one-line cross-reference to mobile-app.md §8, and notes in webcam-video §7.4 that stream tiles share the grid.

## 11. The split

Seven issues, filed to the task template once Nick has signed §10; each is executable cold with this note as its design. **None touches a path in `.github/workflows/process-checks.yml`'s approval-path list** (`internal/server/mcp*.go`, `internal/server/admin_*`, the approval, decision, resolution and audit-read files): no MCP tool, no admin-door action and no audit reader changes. Each issue says so under its labels. #1487 (the grid) and #1488 (the picker) are filed already and are not re-stated here; §11.3 depends on both.

### 11.1 Schema: `conch.voice_presence.v2` with its participant, the quality types and the v2 claim, `voice_share_full`, the two parameters

- **Context:** this note §2, §3.2, §4, §5.2; the `schema-change` skill; Nick's sign-off on §10 recorded on #1483. Breaking: a new version beside v1; v1 never changes, and gains no field (the amendment of 2026-11-07, §2.2). The cap is not a `Validate` check (ADR-015 N3). The quality is a declared label with a closed vocabulary.
- **Files:** `pkg/schema/voice.go`, `pkg/schema/voice_test.go`, `pkg/schema/voice_v1_frozen_test.go`, `pkg/schema/ts.go`, `pkg/schema/vocabularies.go`, `pkg/schema/testdata/voice-presence-v2*.json`, `voice-presence-v1-degraded.json`, `voice-share-request-v1*.json`, `voice-share-v2.json`; `web/src/schema/generated.ts` by `go generate` only.
- **Acceptance criteria:**
  - [ ] `VoicePresenceSchemaV2`, `VoicePresenceV2`, `VoicePresenceRoomV2`, `VoiceParticipantV2` exist; `VoicePresenceRoomV2.Validate` accepts two sharing participants; `VoicePresenceRoom.Validate` still refuses them with today's message; `VoiceParticipant`, `VoicePresenceRoom`, `VoicePresenceV1` and `VoiceShareV1` are byte for byte what main had.
  - [ ] `VoiceParticipantV2.ShareStartedAt` and `ShareQuality` (omitempty) validate as §2.2 says; `VoiceShareQuality.Validate` refuses a resolution outside `720p`, `1080p`, `source` and a frame rate outside 15, 30, 60.
  - [ ] `VoiceShareRequestV1` with `DecodeVoiceShareRequestV1` (strict, as the companion's decoder; an empty body is a zero request); `VoiceShareV2` with its required `Quality`.
  - [ ] `voice_v1_frozen_test.go` passes: every v1 fixture reads into the frozen v1 types with unknown fields refused.
  - [ ] `ErrorCodeVoiceShareFull = "voice_share_full"`, `VoiceSharePrincipalParam = "principal"`, `VoicePresenceSchemaParam = "schema"` exist with the comments of §2.2 and §5.2; `ErrorCodeVoiceShareBusy`'s comment says it is answered by nothing from this version.
  - [ ] `scripts/schema-compat.sh` passes with every existing fixture byte-identical; the new fixtures of §9.1 are in `testdata/` and in `ts.go`'s table; `VoiceShareResolution` is `TSStrict`; `scripts/schema-ts-check.sh` passes.
- **Required tests:** §9.1 in full; `go test ./pkg/schema/...`; `make check`.
- **Blocked by:** Nick's sign-off on §10 (recorded on #1483). Labels: `area/schema`, `voice`. Approval-path: no.

### 11.2 Server: several streams per room, the cap, the quality body, `?principal=`, `?schema=`, companion audio per sharer, e2e

- **Context:** this note §2.3, §3.1, §4, §5, §6, §9.2–§9.4. The share becomes a map keyed by principal, as the camera is, each with its declared quality; the permission function's share input becomes per principal; v1 is built degraded as §2.3 says; `voice_share_busy` is answered nowhere.
- **Files:** `internal/server/voice_share.go`, `voice_companion_audio.go`, `voice_presence.go` (room state, `snapshotLocked` with a version, `metricCounts`), `voice_ws.go`, `voice.go` (`handleVoicePresence`), `voice_permission_fn.go`, `voice_permission.go` (the voice switch's hook), their tests; `e2e/voice/share.go` (`multishare`, `sharersIn`), `e2e/voice/companion.go`; `e2e/isolation/media.go`, `voicegates.go`, `revoke.go`, the matrix table; `docs/design/voice-control-plane.md` §12 and §13 amendments; `docs/design/private-alpha.md` O5 line.
- **Acceptance criteria:**
  - [ ] Two entitled, connected members of one channel both get 200 from `POST …/voice/share`; both are `sharing` in v2 presence with distinct `share_started_at`; a declared quality appears as `share_quality` and in the started row's detail; a holder's repeat with a body changes it (a presence frame, no row); the seventh concurrent claim in a room is 409 `voice_share_full` with no row and no LiveKit call; a malformed body is 400 after the caller's checks.
  - [ ] `GET …/voice` and `GET /v1/voice/ws` honour `?schema=` (v1 default, v2 on request, 400 `invalid_request` for any other value before the channel is resolved); a v1 document with two streams lists exactly one `sharing` participant, the reader's own else the earliest-started, and validates; with one undeclared stream it is byte-identical to today but for `share_started_at`.
  - [ ] `DELETE …/voice/share` ends the caller's own stream; `?started_at=` is checked against the caller's stream (409 `voice_share_stale` otherwise); `?principal=<id>` ends that person's stream for a holder of `manage_channels` or a manager of the channel, is 403 `forbidden` (audited `access_denied`) for anyone else, 204 with nothing written when that person holds none, and 400 with `started_at` naming someone else's.
  - [ ] `PUT …/companion/audio` grants `screen_share_audio` to the caller's own companion for the caller's own stream and answers 409 `voice_no_session` when the caller holds none; `voice_share_busy` appears in no handler.
  - [ ] Each access-lost path (`share_screen` cut, the voice switch, membership loss, idle, leaving) ends one person's stream and no other's, with its rows; rotation ends every stream `rotated`.
  - [ ] `conch_voice_shares_active` counts started streams across rooms.
- **Required tests:** §9.2 unit tables; `e2e/voice` `multishare` and the `companion` lines of §9.3; the isolation rows of §9.4 with the coverage check green; `make check`.
- **Blocked by:** §11.1. Labels: `area/server`, `voice`, `security` (LiveKit grants). Approval-path: no (touches no listed path). Security-reviewer pass before merge (grants, who-may-stop, the new body).

### 11.3 Web: stream tiles in the grid, Watch, focus, per-stream sound, the quality on the wire

- **Context:** this note §3.1, §7.1, §8, §9.5; webcam-video §7.4 (the stage and strip this reuses); #1487 (the grid shows everyone; the toggle) and #1488 (the picker, for one share). The browser moves to v2 presence; subscription is keyed by sharer and by focus; every watched stream's sound plays with its own control.
- **Files:** `web/src/voice/presence.ts` (`sharers`), `useVoice.ts` (`?schema=`), `sdk.ts` (`wanted`, `Wants` keyed by principal and focus; `setVideoQuality` on unfocused watched publications), `connection.ts` (the claim body from the picker, re-POST on change), `VoicePanel.tsx`, `CallView.tsx` (stream tiles), `ShareView.tsx` (a tile's player with volume and mute), `share.ts` (notes), `web/src/stage.ts`, their tests; `web/e2e/12-screen-share.spec.ts` and a new multishare spec; `web/src/schema/generated.ts` is generated only.
- **Acceptance criteria:**
  - [ ] Every live stream is a tile in the call grid beside camera and avatar tiles, ordered among streams by `share_started_at`, showing the sharer, LIVE, the quality label when declared, and Watch; "Hide non-video participants" never hides a stream tile; the page's own stream is a "You · LIVE" tile with Stop sharing.
  - [ ] No track of a stream is subscribed until its Watch is pressed; Watch subscribes video and sound and plays both at once in the tile with its own volume and mute; several tiles may be watched together; Stop watching unsubscribes one.
  - [ ] Clicking a watched tile focuses it on the stage with everything else in the strip; the focused stream is at the top layer and every other watched stream is held to the lowest layer; Escape or Back to grid returns; a watched stream that ends leaves with "{name} stopped sharing." and a focused one returns the view to the grid.
  - [ ] The picker's choice is sent as the claim's body, and a change while live republishes the encodings and re-POSTs; 409 `voice_share_full` shows "This call already has 6 streams live."; `oneSharerNote` and `busyNote` are gone.
  - [ ] Below the narrow breakpoint one focused stream plays at a time and a "Watching {name}'s screen" chip switches between live streams, picture and sound together.
  - [ ] `tsc`, `eslint` and `scripts/schema-ts-check.sh` clean; no new dependency.
- **Required tests:** §9.5 Vitest and Playwright.
- **Blocked by:** §11.1 (types and fixtures); §11.2 for Playwright; #1487 (the grid); #1488 (the picker). Labels: `area/web`, `voice`. Approval-path: no.

### 11.4 CLI and TUI: v2 presence, several sharers and their quality in the text, `conch voice share stop`

- **Context:** this note §7.2; parity with `conch voice camera stop` (ADR-015 C5).
- **Files:** `internal/cli/client.go` (`VoicePresence`, `SubscribeVoicePresence` on v2), the TUI's presence text, a new `internal/cli/voice_share.go` and tests.
- **Acceptance criteria:**
  - [ ] The TUI reads `?schema=conch.voice_presence.v2` and names every sharer: one as today with the quality when declared; two or more as "sam and val are sharing their screens (val with sound)".
  - [ ] `conch voice status` lists each sharer with when they started and the declared quality.
  - [ ] `conch voice share stop <channel> <person>` sends `DELETE …/voice/share?principal=<id>`; exits nonzero with one line on 403, 404 and on a hostile server's malformed answer; 204 prints nothing.
- **Required tests:** table-driven presence text; hostile-server tests as the camera's command has; `golangci-lint` clean.
- **Blocked by:** §11.2. Labels: `area/cli`, `voice`. Approval-path: no.

### 11.5 Voice helper: v2 presence types and stream

- **Context:** this note §7.3. The helper validates every presence frame and would refuse two sharers; it moves to v2 so no first-party client reads the degraded document.
- **Files:** `voice/crates/conch-voice-api/src/types.rs`, `presence.rs`, its fixture tests; `voice/conch-voice/src/presence.rs`.
- **Acceptance criteria:**
  - [ ] `VoicePresenceV2` and `VoicePresenceRoomV2` exist with `check` as §2.2; `share_started_at` and `share_quality` are read; the v2 fixtures decode and validate; the v1 types are unchanged and still refuse two sharers.
  - [ ] The presence stream asks for `?schema=conch.voice_presence.v2` and the helper's roster is built from v2.
  - [ ] `cargo fmt`, `cargo clippy -D warnings`, no `unwrap` outside tests, no new dependency.
- **Required tests:** fixture round-trips against `pkg/schema/testdata/voice-presence-v2*.json`; the stream test with a two-sharer frame.
- **Blocked by:** §11.2. Labels: `area/voice`, `voice`. Approval-path: no.

### 11.6 Desktop: the companion watches several streams and plays their sound

- **Context:** this note §7.4; linux-desktop §6.4, #906. The renderer is the web client of §11.3; this issue is the desktop-only subscription and playback rule and a check that two desktops stream at once.
- **Files:** the companion's subscription filter in the desktop renderer/main (`desktop/main/voice/*`, `web/src/voice/native/*` as #772 lands them), their tests.
- **Acceptance criteria:**
  - [ ] Under B2 the companion subscribes to the watched streams' tracks by publisher, the focused one at the top layer and the others at the lowest, never to its own companion's tracks and never to voice; it plays every watched stream's sound with its tile's controls, and the helper plays none.
  - [ ] Two desktops in one room both stream; each helper plays voice once; neither plays its own stream's sound; the operator stop by `?principal=` ends one and not the other.
- **Required tests:** the renderer's filter unit tests; an `e2e/voice` companion line if the harness has two companions (else recorded as a by-hand check with the two-desktop steps).
- **Blocked by:** §11.3; #772. Labels: `area/desktop`, `voice`. Approval-path: no.

### 11.7 Docs and measurement: N streams, watch patterns and the picker on the droplet

- **Context:** this note §4, §8; O6's unrecorded measurement (#448, alpha-exit-record B12).
- **Files:** `docs/design/alpha-exit-record.md` B12, a deployment note, the CHANGELOG line ("several people can stream a screen at once, up to six per room, watched on demand"), user documentation.
- **Acceptance criteria:**
  - [ ] Recorded for 1, 2 and 6 streams at the picker's default and one at Source 4K60, with 29 viewers in three patterns (watching nothing; one focused each; one focused and two unfocused each): droplet ingress and egress, CPU, and the layer each viewer received; with the figures the cap and the picker's default are confirmed or an issue to change them is filed.
- **Required tests:** none (a measurement, recorded).
- **Blocked by:** §11.3. Labels: `area/docs`, `voice`. Approval-path: no.
