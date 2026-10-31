# Design: webcam video in channel voice rooms

- **Status:** Proposed, 2026-10-31. Nothing here is built. The governing decision is [ADR-015](../adr/ADR-015-webcam-video.md) (Proposed). Until Nick accepts it, camera video stays at P9.
- **Issue:** #973. The implementation issues are in §13.
- **Touches:**
  - [ADR-000](../adr/ADR-000-charter.md), whose non-goal is "no video before P9";
  - [ADR-004](../adr/ADR-004-voice-via-livekit.md), "video … remain at P9" and "LiveKit only moves audio";
  - [ADR-008](../adr/ADR-008-private-communications-alpha.md), the browser client and the screen share;
  - [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md), the permission catalog and the brakes;
  - [ADR-013](../adr/ADR-013-one-community-model.md), only as the source of who is a member.
- **Builds on:**
  - [voice-control-plane.md](voice-control-plane.md): §4 sessions, §6 the poller, §12 the screen share, whose claim this copies, and §13 companions;
  - [admin-roles.md](admin-roles.md): §4 the catalog, §5 brakes, §7.5 live effects;
  - [mobile-browser-layout.md](mobile-browser-layout.md) and #1028's one-row strip;
  - [linux-desktop.md](linux-desktop.md) §6, Model B.
- **Owner:**
  - protocol-designer: schema and this note;
  - server-engineer: the claim and the poller;
  - web-engineer: the browser.
- **Verified against:** `main` at `562cc3e`.

Nick asked on 2026-10-30 for webcam video, "starting with one-on-one conversations and small groups in existing channel voice rooms". This note covers how that works within what exists:

- `conchd` stays the only authority on who may publish what, and LiveKit only moves the media;
- the camera is a third kind of track in the rooms that already carry voice and the screen share;
- the browser comes first, and the desktop app follows later.

## 0. The decisions in one table

| # | Decision | Section |
| --- | --- | --- |
| 1 | A new permission, `use_camera`, which needs `use_voice`. It is **off by default for members and operators**: ADR-014's rule for a new permission is that an upgrade changes nothing. Owners always hold it. The voice switch (`voice_allowed`) also withholds it. There is no camera-only brake. | §3 |
| 2 | The camera is **off on joining and never on unless the person turns it on**. The server enforces this, not just the client: the join token still grants the microphone alone. | §3, §4 |
| 3 | Turning the camera on is a **server claim**, like the screen share but without the one-per-room rule. `POST …/voice/camera` grants the `camera` source to the live connection, and `DELETE` takes it away. The room has a cap of 9 cameras. | §4 |
| 4 | **One function computes every publish permission conchd writes.** It covers the microphone, the screen while the person holds the share, and the camera while they hold a camera claim. Stopping a share must never take a camera down, and stopping a camera must never take a share down. | §4.3 |
| 5 | Each poller pass checks every camera against the claims and against `use_camera`. A camera nobody claimed is withdrawn, or its publisher is evicted. A route that takes the camera away (the permissions PUT, the voice switch, removal) ends the claim before it answers. | §4.5, §4.6 |
| 6 | Presence gains `camera` for each participant. The person's own document gains `camera_ended`, which says why their camera stopped. Occupancy is unchanged. | §5 |
| 7 | **Everything on the wire is additive. No version bump** is needed. | §10 |
| 8 | `Permissions-Policy` becomes `camera=(self)`, in the same PR as the browser's first capture. | §6.4 |
| 9 | The browser has a camera button, a camera picker and a camera test in Voice options, and a self-view. A call view on the stage holds up to 9 tiles. A watched share is the main picture, with the cameras in a strip beside it. | §7 |
| 10 | On a phone, video appears only in the Voice tab and is received only while that tab is shown. The camera stops when the page is hidden. The strip keeps its four controls, and its state button says "Camera on". | §8 |
| 11 | On the desktop (Model B, later), the Electron companion publishes the camera. The Rust helper gets no video. | §9 |

## 1. Scope

### 1.1 What the first delivery does

- In a channel's voice room, a person in voice can turn their camera on and off. Everyone in the room can see it.
- It works in the browser: desktop browsers, and phone browsers where the browser allows capture.
- It is sized and tested for **one-on-one and small groups, up to 9 cameras in a room**.
- The camera and the screen share can be on together. One person can have both.

### 1.2 What it does not do

- **No recording, no egress, no server-side frames.** `conchd` never touches media. No grant ever names `roomRecord`, and nothing starts a LiveKit egress.
- **No agents.** Agents are already refused a voice session (voice-control-plane §1). They get no visual access, and nothing here gives them any (ADR-004, P9).
- **No face detection, blurring or backgrounds.** They would need new dependencies and processing of video on the client. That is a later issue, if ever.
- **No camera from the Rust helper or the TUI.** ADR-006 makes `conch-voice` an audio client.
- **No camera in nets or listen-only grants.** A camera is claimed only in the channel-wide room, as a share is (§4.1).
- **No end-to-end encryption** (ADR-000). §6.3 states the trust model.

### 1.3 Governance

ADR-000's non-goals say "no video or screen sharing before P9". ADR-004 says "video and screen sharing remain at P9" and "LiveKit only moves audio". ADR-008 moved the screen share into the alpha, but not camera video. This feature therefore needs an ADR. **ADR-015 is proposed with this note.** It also records the additions to ADR-014's catalog (§3).

The non-goal is not in ADR-000's locked table (D1–D10), and nothing in that table changes:

- D3: LiveKit stays optional, and messaging works without it;
- D6: every capability is a REST route first;
- D10: agents are still governed by their manifest.

The roadmap's P9 line and its alpha section would also need editing. That is Nick's to do (ADR-000), so this note does not touch `ROADMAP.md`.

## 2. What exists, verified at `562cc3e`

| Fact | Where |
| --- | --- |
| The join token grants `canPublishSources: ["microphone"]` only. A companion gets `["screen_share"]` or `[]`. | `internal/server/livekit/token.go` |
| The screen share is a claim. The person's own permission is replaced through `UpdateParticipant` with `MICROPHONE, SCREEN_SHARE, SCREEN_SHARE_AUDIO`. When the share is withdrawn, it is set back to `MICROPHONE` alone, "exactly the join grant". | `internal/server/voice_share.go:394`, `:689`; voice-control-plane §12 |
| `livekit.Permission` has a bundled `ScreenShare` flag and a per-source form (`SourcePermission`). The per-source form accepts only the two screen-share sources. | `internal/server/livekit/client.go:342–400` |
| Each pass withdraws screen-share sources from anyone who does not hold the room's share, leaving them "their own publish grant". | voice-control-plane §12, Enforcement |
| `Permissions-Policy: microphone=(self), camera=(), display-capture=(self)`. | `internal/server/static.go:29` |
| The browser subscribes by choice. A microphone is always wanted, a share only while it is watched, and "a camera never". | `web/src/voice/sdk.ts` `wanted`; `sdk.test.ts:1016` |
| Room options are `adaptiveStream: true, dynacast: true, stopLocalTrackOnUnpublish: false`. The adapter stops every track itself. | `web/src/voice/sdk.ts:139` |
| No source of ours calls a device API. The SDK does the capture, and `src/voice/sdk.ts` is the one file that imports it. | `web/src/hygiene.test.ts:157` |
| The catalog has eleven permissions. `Default` falls through to `true`, and `OperatorOnly` falls through to `true`. The fixture test requires `len(rows) == len(Permissions)`. | `pkg/schema/permission.go`, `permission_test.go:64` |
| `PresetGroup` must equal the member defaults (`TestPresetMemberPermissions`). | `pkg/schema/permission_test.go:296` |
| A1 (#1012) is merged. A2, A3a, A3b and A4 (#1013–#1016) are open, and so is #984 (the per-person voice route). | GitHub, 2026-10-31 |
| The Playwright project launches Chromium with `--use-fake-device-for-media-stream`, which provides a fake camera as well as a fake microphone. | `web/playwright.config.ts:49` |
| `lk`, as `e2e/voice` pins it, publishes its demo video **as a camera**. The share measurements had to rewrite it to a screen share. | voice-control-plane §12, "as measured" |

## 3. Who may turn their camera on

### 3.1 The permission

Row 12 of ADR-014's catalog:

| # | Permission | What it allows | Member default | Operator default | Owner | Configurable by |
| --- | --- | --- | --- | --- | --- | --- |
| 12 | `use_camera` | Turn on a camera in voice, in channels they are in (needs `use_voice`) | **off** | **off** | always | as row 1: the member cell by the owner, or by an operator with `change_server_settings` who holds it; the operator cell by the owner |

**Why off for both.** ADR-014 decision 2 says "a permission added later must default to the behaviour from before it existed", and the schema says the same (`permission.go:27`). Before this, no one could publish a camera. A default of on would put every member's camera one click away the moment a server upgraded, with no word from its owner. That matters most on a personal server, where people who should not see each other may still share a channel with the owner. Owners hold every permission, so the owner can try the feature at once, and turning it on for members is one cell (O2).

**Seeing is not a permission.** Anyone in the room can receive the cameras in it, as they receive its voice. Watching a share is not gated either. A permission to receive would need a per-subscriber grant LiveKit does not have (linux-desktop §6.1: `canSubscribe` is one flag). The room is the boundary, as ADR-004 decided.

**The dependency.** `use_camera` ⇒ `use_voice`, in the same column. The PUT refuses anything else with 409 `permissions_conflict`, as for `share_screen`. A camera is published by a voice connection, and a person without voice has no connection.

**Presets.** Both presets leave `use_camera` off, so `PresetGroup` still equals the defaults and neither preset's published member list changes. An owner who turns camera on for members on a personal server then has a member column that matches no preset, and the console says so, as it does for any edited column (O3).

### 3.2 The brake

A person holds `use_camera` only if all three hold:

- their role's cell is on;
- they hold `use_voice`;
- `voice_allowed` is not off for them.

**No camera-only brake is added.** ADR-014 decision 4 keeps the two existing brakes and adds none. The voice switch already means "this person may not be in voice", which covers the camera. "Voice yes, camera no for this one person" has no answer short of turning the role cell off. Nick's two servers do not need one (O4). If one is needed later, it is a new column and an amendment to ADR-014.

### 3.3 The operator's switches

- **For everyone:** the role cells. The owner can turn the camera off for every member or every operator at once, and §4.6 makes that take effect live.
- **For one person now:** **stop their camera**. `DELETE …/voice/camera?principal=<id>` is open to anyone holding `manage_channels`, which is the standing that stops a share today (admin-roles §4.1, row 9). The person can turn it on again unless a brake or the cell is also changed. That is the same as a share stop, and it is the honest answer: a stop is moderation of a moment, and a lasting ban is the voice switch. Channel managers get moderation §4's voice kick, not a camera stop (O7).
- **There is no operator mute in Conch,** for the microphone or anything else. The nearest things are this stop, the share stop and the kick. §6.2 lists what each one does to a camera.
- **No server flag.** A deployment-wide "no video" is the operator cell and the member cell, both off, which is the default. An owner still holds the permission. An owner who does not want video does not turn it on (O2).

### 3.4 Off on joining

- **Server.** The join token still names `microphone` only (§4.1), so a new connection cannot publish a camera until it claims one. This holds whatever the client does. The camera is never on at joining, after a full reconnect through `conchd`, or after a restart.
- **Client.** Joining, unmuting and sharing never call the camera. Only the camera button does (§7.1).

## 4. Server

### 4.1 The claim, and why a claim

**Option A, the camera in the join token.** `canPublishSources` would be `["microphone", "camera"]` whenever the person holds `use_camera`. The client publishes when it likes, and the poller sees the track.

**Option B, a claim (recommended).** The join token is unchanged. `POST …/voice/camera` adds `camera` to the live connection through `UpdateParticipant`, and `DELETE` removes it. This is the share's mechanism, without its one-per-room rule.

| | A: in the token | B: a claim |
| --- | --- | --- |
| Off at joining | client honesty: the grant allows a camera from the first frame | **server enforced**: no connection starts with a camera |
| What the server knows | what the poller saw, accurate to one pass (≤ 1.5 s) | its own decision, exact, plus what the poller saw |
| Audit times | observed, `source=observed` | the claim's time, as for a share |
| Operator stop | takes `camera` off the connection, and turning it on again then needs a reconnect, or a route to grant it back (which is B) | `DELETE` by the operator, and the person can claim again |
| The cap of §4.4 | cannot be enforced before publishing | refused at the claim, 409 |
| Join token and `e2e/voice`'s microphone-only assertion | change for everyone with `use_camera` | **unchanged** |
| LiveKit refreshed tokens (§10 finding 9, S4, S5) | carry the camera for ten minutes after it is taken away | the same, and the pass covers it as for a share (§4.5) |
| Cost | none on the happy path | one request and one `UpdateParticipant` (about 2 ms, finding S2) when the camera is turned on. Turning a camera on is not latency-critical, unlike push-to-talk. |

B keeps the server's view honest in the strongest sense: a camera exists only because `conchd` granted it, as with a share. It also reuses a pattern that is built, measured (S1–S7) and covered by `e2e/voice`. Both options need the one permission function of §4.3. A needs fewer routes, but its operator stop pushes it towards B anyway (O5).

### 4.2 The routes

`POST /v1/channels/{channel}/voice/camera`, authenticated, no body. The checks are the share's, in the share's order (voice-control-plane §12), with two more:

| # | Check | Answer when it fails |
| --- | --- | --- |
| 1 | a verified caller | 400 `voice_requires_auth` |
| 2 | a member of the channel (operators included, with no exemption) | 404 `channel_not_found` |
| 3 | a human | 403 `forbidden`, audited `access_denied` |
| 4 | `use_voice` (role cell and `voice_allowed`) | 403 `voice_not_allowed` |
| 5 | `use_camera` | 403 `permission_denied`, `permission: use_camera` (ADR-014 §7.4) |
| 6 | voice configured | 503 `voice_not_configured` |
| 7 | the request's credential is a recorded holder of the channel's current channel-wide room | 409 `voice_no_session` |
| 8 | the caller's connection may publish (`can_publish`), and it is not a net or listen-only grant | 409 `voice_no_session` |
| 9 | the room has fewer than 9 cameras claimed, or the caller already holds one | 409 `voice_camera_full`, naming nobody |
| 10 | the claim is recorded (pending), then `UpdateParticipant` sets the permission of §4.3 | LiveKit `not_found`: 409 `voice_no_session`, nothing recorded. LiveKit down: 503 `voice_unavailable`, nothing recorded. |
| 11 | `voice_camera_started` is written, and only then is the claim no longer pending | 500: the claim is undone and the source withdrawn |

- **The answer** is 200 and a `VoiceCameraV1`: `principal_id` and `started_at`.
- **Asking again while holding a claim** grants the source again and answers with the claim already held. It writes nothing. This is how a connection replaced through a full reconnect gets the camera back.
- **A claim that finds its room retired** once it has been granted is taken back and answered 409 `voice_no_session`, as for a share.
- **Rotation.** A claim is made only in the room step 7 found. If that room is rotated while LiveKit is being asked, the claim does not follow the channel to its new room. The answer is 409, and the client claims again.
- **The order for the client** is the share's: connect, capture, claim, wait for LiveKit's permission update, publish (§7.2).

`DELETE /v1/channels/{channel}/voice/camera[?started_at=…][&principal=<id>]`, authenticated.

- **Without `principal`, or naming the caller:** the caller's own claim. Checks 1, 2 and 6, as for a share stop. `?started_at=` names the claim (#487's rule). A release that names a claim which is no longer the caller's current one is 409 `voice_camera_stale` and changes nothing. A request naming no claim ends whatever the caller holds. The browser always names its claim. No claim: 204, nothing written. Otherwise the claim ends with `reason=self`.
- **With `principal` naming someone else:**
  - The caller must hold `manage_channels`. Owners always do.
  - An operator does not have to be a member, as for the share stop.
  - Anyone else, including an operator without `manage_channels`, gets 403 `permission_denied` (`permission: manage_channels`), audited `access_denied`.
  - A non-member without `manage_channels` gets the uniform 404 first, and learns nothing.
  - No such claim: 204.
  - Otherwise the claim ends with `reason=operator`, and the operator is the row's actor.
- **A malformed or repeated parameter** is 400 `invalid_request`, before the channel is looked at.
- **Ending a claim** takes it out of presence and writes the row, and only then asks LiveKit to withdraw the source. A withdrawal LiveKit refuses is retried by the passes (§4.5). The stop itself is never refused for it.

Claims and releases in one room are made one at a time, and so are the withdrawals a pass makes. The share already serialises this way, and the two use **one lock per room**, so that a share grant and a camera grant never overwrite each other's permission.

**A bound per person.** Each claim costs an audit row, a LiveKit call and a presence frame for the room. Claims are bounded per principal: a burst of 10, refilled at one every 6 seconds. Past it, the answer is 429 `voice_camera_rate_limited`, logged at most once a minute, as `voice_report_rate_limited` is. Releases are never bounded (O12).

### 4.3 One permission function

Every `UpdateParticipant` conchd makes for a person's own connection writes one value, computed from the person's state:

| Person's state | `canPublishSources` written |
| --- | --- |
| listen-only (`can_publish` false) | none, with `canPublish: false` |
| in voice | `microphone` |
| holds the room's share | `microphone, screen_share, screen_share_audio` |
| holds a camera claim | `microphone, camera` |
| both | `microphone, screen_share, screen_share_audio, camera` |

A companion (`p<id>.screen`) gets `screen_share` while the person holds the share, plus `camera` while they hold a camera claim, but only once §9's desktop phase exists. Until then, a companion is never given `camera`.

Today's call sites write fixed values: the share's grant and withdrawal (`voice_share.go:394`, `:689`) and the pass's withdrawal of an unclaimed share. Each of them would take a camera down. All of them move to the one function **before** the camera exists (issue C4a, which changes no behaviour). `livekit.Permission` gains a `Camera` source, and `SourcePermission` is allowed to name it.

The test that pins this: stopping a share leaves the sharer's camera published, and ending a camera leaves the share published. It runs against the real LiveKit too (K3, §12).

### 4.4 The cap

The cap is **9 cameras per room**, the largest call view in §7.4. It is a constant in code, not a setting. Nothing published depends on its value, so it can change without a schema change once §12's measurements say what a droplet carries. A tenth claim is 409 `voice_camera_full`, and the browser says "This call already has 9 cameras on" (O6).

The cap counts claims, so it is exact and needs no pass. It does not limit how many people are in voice: 30 people can listen while 9 show their cameras.

### 4.5 What the poller does with cameras

LiveKit lists each published track with `source` and `muted` (finding S7; K6 confirms this for `"source":"CAMERA"`). For each entitled participant, a pass does the following, with the person's participants folded together as §13 of voice-control-plane does:

- **Holds a claim and has an unmuted camera track:** a live camera. Its last-seen time is updated.
- **Holds a claim and has no unmuted camera track:** the claim ends `idle` 15 seconds after it was made if no camera track has been seen at all, or 5 seconds after the last pass that saw one. These are the share's figures. The browser asks for the browser's permission before it claims (§7.2), so 15 seconds is spent on the SDK and network, not on a prompt. A muted camera does not count as live, so presence never says "camera on" for long about a track that sends nothing.
- **Holds no claim and has `camera` granted, or has a camera track:**
  - The source is withdrawn, through §4.3, so the participant keeps exactly their microphone and their share.
  - An empty source list counts as every source, as it does for the share.
  - A published track writes `voice_camera_unclaimed` once per track; a grant alone writes nothing.
  - If LiveKit will not withdraw the source while the track is still published, the participant is removed: `voice_participant_removed`, `reason=camera_unclaimed`, or `camera_not_withdrawn` for a claim that ended. This is the share's fail-closed rule.
- **Entitlement, on every pass, for every claim holder:** `can(principal, use_camera)`, which covers `use_voice`, the brake and the role cell. Losing it ends the claim with `access_lost`. This is the same read the pass makes for `share_screen` (admin-roles §7.3, SF-4).

The pass reads tracks it already lists, so cameras add no LiveKit call and change no timing in voice-control-plane §6.

### 4.6 Revocation and its bounds

| What happens | Effect on a camera | Bound |
| --- | --- | --- |
| The person turns it off | The client unpublishes and stops the capture, then sends `DELETE`. The claim ends `self`. | at once |
| An operator stops it | The claim ends `operator`, and LiveKit unpublishes it for every viewer. | at once (S3 measured 0.25 ms for a share; K3 measures it for a camera) |
| `use_camera` or `use_voice` turned off for a role (`PUT /v1/permissions`) | The PUT's live-effect hook (admin-roles §7.5) ends every affected claim with `access_lost` and withdraws the source **before the response**. | at once. If LiveKit refuses: within one pass (≤ 1.5 s), then eviction bounded by the removal timeout (4 s). |
| The voice switch turned off for a person (#984's route) | The same, from #984's immediate hook (`voicePrincipalLostAccess`), which also takes them out of voice. | as #984 bounds voice: one pass plus one join-token lifetime |
| Removed from the channel, disabled, credential revoked or expired | The room is rotated (voice-control-plane §5). Every claim ends `rotated`, and deleting the room takes every track with it. | at once by hook; one sweep (30 s) for expiry |
| Kicked from voice (moderation §4) | The connection is gone, and the claim ends `left` on the next pass. | one pass |
| Leaving | The claim ends `left` on the first pass that does not see `p<id>`. | one pass |
| A change made without a route (the store edited by hand) | The pass's entitlement read. | one pass |
| `conchd` restarts | Claims are in memory and are dropped, as shares are. The first pass withdraws `camera` from every connection and audits any track still published as unclaimed. The browser shows "Your camera was turned off" and waits for the person. | first pass after start |
| LiveKit unreachable | No claim can be granted (503). A camera already on stays on, because `conchd` cannot reach LiveKit to stop it. That is the same gap as voice-control-plane §5 for voice. A stop during the outage ends the claim at once in presence and the audit, and the source is withdrawn when LiveKit answers. | until LiveKit answers |

The hooks in rows 3 and 4 are #984's and A4's (#1016). Each gains a line, "and end the person's camera claims", and if C4 lands after them, C4 adds that line itself (§13).

### 4.7 Audit

| Event | When | Detail |
| --- | --- | --- |
| `voice_camera_started` | a claim is granted, before it is answered | actor the person; `channel=<id>` |
| `voice_camera_stopped` | a claim ends, once for every claim that started | actor the person, or the operator for `operator`, or `system`; `channel=<id> principal=<id> reason=self\|operator\|left\|access_lost\|rotated\|idle` |
| `voice_camera_unclaimed` | a pass sees a camera track from someone without a claim | actor that principal; `channel=<id> source=observed` |
| `voice_participant_removed` | eviction under §4.5 | existing row; new reasons `camera_unclaimed`, `camera_not_withdrawn` |
| `access_denied` | a refused stop of someone else's camera; an agent | existing row |

Every claim that started gets exactly one stopped row. Each row is subject `channel:<id>`. As for every voice row, no row carries a room name, a token, a device name or a device id. A refused claim (cap, permission) writes nothing, as `voice_share_busy` writes nothing; `permission_denied` is in the response. The 429 bound is logged, not audited.

### 4.8 Companions and viewers

- `p<id>.view` (a browser tab beside native voice) receives cameras as it receives a share. It publishes nothing, and that is unchanged.
- `p<id>.screen` is never granted `camera` until §9's phase. The pass treats a companion with `camera` as unclaimed.
- Folding (§13 of voice-control-plane): the presence flag `camera` is the person's claim, whichever participant publishes it.

## 5. Presence and events

- **`VoiceParticipant.camera`**, an optional bool, omitted when false. True from the moment a claim is answered until it ends, so it reflects `conchd`'s decision, as `sharing` does. With the idle rule of §4.5, it is never true for more than 15 seconds while nothing is sent.
- **`VoicePresenceV1.camera_ended`**, optional, and only in the person's own document, as `share_ended` is (#487). It holds `reason` (`VoiceCameraEndReason`: `self`, `operator`, `left`, `access_lost`, `rotated`, `idle`), `started_at` and `ended_at`. It comes in the same frame that takes `camera` away. The browser uses it to say why ("An operator turned off your camera", "Camera is not allowed for you", "Your camera stopped: nothing was being sent"). It is kept until the person claims again, and a restart drops it. It never says who stopped the camera; the audit row does.
- **Occupancy (`VoiceOccupancyV1`) is unchanged.** It counts people in voice per channel. A camera count in the channel list would reveal more about channels a person is not looking at, and nobody needs it to decide to join.
- **No new event document.** Presence is a full snapshot on its own socket. A permission change already sends `conch.permissions_changed.v1`, and the browser then reads `whoami` again to show or hide the camera button.
- **Whether the button is shown** comes from `whoami.permissions` (A3b), which must include `use_camera`, together with the room grant's `can_publish`. The claim's answer is the truth. The session response does not change (§10).

`VoicePresenceRoom.Validate` gains three checks:

- `camera` needs `can_publish`;
- at most 9 participants have `camera`;
- `camera_ended` has a known reason, both times, an end no earlier than its start, and voice configured.

No document an older server sent can fail them.

## 6. Privacy and safety

### 6.1 Indicators

- **For the person themself:**
  - Two states, kept apart as UX-11 keeps "Microphone in use" apart from "Transmitting":
    - **Camera in use**, amber: captured, but not yet sent;
    - **Sending video**: claimed and published.
  - The camera button is pressed (`aria-pressed`), and the self-view tile shows them.
  - On a phone the strip's state button says "Camera on" (§8).
  - The browser's own indicator (the tab dot, the device light) is never the only one.
- **For others:**
  - the person's tile, with their name;
  - the word "camera" on their roster chip, beside "sharing";
  - presence `camera`, for clients without video (the CLI's `conch voice status`).

### 6.2 When the camera stops

The capture is released, not merely muted, so the device light goes off.

| Event | What happens |
| --- | --- |
| The camera button, off | Unpublish, stop the track, `DELETE` naming the claim. |
| Leave voice | The same, before the room is left. |
| Sign out, change of profile or server, change of voice channel | The same, **before** anything is done on the new identity, channel or server. A camera is never carried into another identity or channel. |
| The room is renewed (rotation), same channel, same person | As the share does (#516): the capture is blanked (black frames) and kept, then claimed and published on the next connection of the same channel's room. If that claim is refused, the capture is stopped. |
| Reconnect within the SDK's window | The track continues, and the claim survives (§4.5: a connection replaced between passes is not a leaving). |
| A full reconnect through `conchd` after the capture was stopped | Never turns it back on. Only the button does. |
| `camera_ended` arrives, or LiveKit takes the `camera` source away | Stop the track and show the reason. Never claim again automatically. |
| The device is lost (`ended`, unplugged, taken by another app) | Stop, release, `DELETE`, and say "Camera disconnected" with the picker. **Never fall back to another camera.** |
| The page is hidden | Desktop layout: nothing happens. A call goes on while the person reads another tab, and the browser's tab indicator stays. Phone layout: stop, as for the button (§8, O9). |
| Mute, deafen | Nothing happens to the camera (§7.6). |
| An operator's voice kick | The connection ends, and so does the capture. |

### 6.3 Trust model, stated plainly

- Video goes from the browser to LiveKit and from LiveKit to each subscriber, encrypted on each hop (DTLS-SRTP).
- LiveKit can see the media, and so can whoever runs it, which is the owner.
- It is **not end-to-end encrypted**, and the browser must not suggest otherwise.
- **Nothing is recorded by Conch.** A participant can still record their own screen, and nothing can stop that. The user documentation says both (C8).

### 6.4 `Permissions-Policy`

The policy becomes `microphone=(self), camera=(self), display-capture=(self)`:

- only the page's own origin may ask for the camera;
- no frame may;
- every other directive is unchanged.

The change goes in the PR that first captures a camera (C6a), not before it, so a release never allows a power its client does not use. It gets a `security-reviewer` pass. The CSP needs no change, because the share's video already renders under it.

## 7. Browser

### 7.1 Controls

- **The camera button** sits in the voice controls beside Mute and Share. Its label is "Camera", with `aria-pressed`, the pattern of the Mute button.
  - It is shown when the person holds `use_camera` and the room's grant can publish.
  - Otherwise it is absent. A person in voice who has been refused the camera gets "Camera is off for you on this server" in Voice options, rather than a button that fails.
  - **There is no keyboard shortcut for turning the camera on.** An accidental key press must not show someone's face. Turning it off is the button, Tab and Enter.
- **The camera picker** goes in Voice options, under the microphone picker. It lists cameras from the SDK (`getLocalDevices('videoinput')`, inside `sdk.ts`, as the microphone list is) and remembers the choice the way the microphone choice is remembered.
  - The chosen device is asked for **exactly** (`{ exact: id }`, as an object, as `microphoneOptions` does).
  - If the choice is missing, the picker says "Your chosen camera is not connected". It does not take the default.
- **Test camera**, in Voice options beside the microphone test: a local preview labelled "Preview: not sent". This is the preview the issue asks for. It captures, sends nothing and claims nothing, and it stops when Voice options closes.
- **The self-view** is the person's own tile, mirrored (a CSS class, no inline style) with the label "You". "Hide self-view" hides only the tile. The "Sending video" state stays shown.

### 7.2 Lifecycle

`connection.ts` gains a camera state alongside the share's:

- `off`;
- `capturing`: the browser asked, and a preview may show;
- `claiming`;
- `granted-waiting`: wait for LiveKit's permission update, as the share waits (#525);
- `publishing`, then `sending`;
- `stopping`;
- `failed(reason)`.

Pressing the button moves through capture (SDK `createLocalVideoTrack` with §7.5's constraints), claim, grant, publish and sending. Each step that resumes after an `await` checks that the adapter is still open, as the share does. A track captured after a close is stopped.

**Errors become actionable states.** None of them affects voice or chat:

| Browser error | Shown |
| --- | --- |
| `NotAllowedError` | "The browser blocked the camera. Allow it in this site's settings." |
| `NotFoundError` | "No camera found." |
| `NotReadableError` | "The camera is in use by another app." |
| `OverconstrainedError` on an exact id | "Your chosen camera is not connected." |
| 403 `permission_denied` | "Camera is off for you on this server." |
| 409 `voice_camera_full` | "This call already has 9 cameras on." |
| 409 `voice_no_session` | The client retries once after a pass gap, as for a share, then shows "Could not start the camera". |

### 7.3 Receiving

- `wanted(source, …)` gains the camera: a camera track is subscribed **only while the call view is shown**. Adaptive stream then picks each tile's layer from its size, and a hidden tile gets no layer.
- The view is not `display: none` while it is subscribed. Closing the view unsubscribes explicitly rather than relying on adaptive stream to notice.
- The video elements are `muted`, `playsInline` and `autoplay`. A camera has no sound, so autoplay rules never block it, including on iOS Safari.

### 7.4 The call view

The call view is drawn **on the stage**, the element that carries the watched share (browser-ui-polish §2), through the same portal. It uses the stage's height rule: the picture gives way first, and the conversation keeps its header, a few lines and the composer.

- **It opens** when the person turns their own camera on, or presses **Show video**. That button appears in the voice controls while anyone in the room has a camera on, and says how many: "Show video (2)".
- **It closes** on **Hide video** or on leaving.
- It is **not opened for people automatically** when someone else turns a camera on. Voice in Conch is per channel and the conversation is central (browser-ui-polish §1), so someone else's camera must not push the chat away or start using a phone's data (O8).

**Tiles.** While the room has at most 9 people in voice, every person in voice gets a tile. A tile without a camera shows the person's initial, name and "Camera off". With more than 9 people, only the cameras get tiles (at most 9, by the cap), and everyone is on the roster as today.

| Tiles | Desktop grid | Narrow (< 700 px) |
| --- | --- | --- |
| 1 | 1 × 1 | 1 × 1 |
| 2 | 2 × 1 | 1 × 2 |
| 3–4 | 2 × 2 | 2 × 2 |
| 5–6 | 3 × 2 | 2 × 3 |
| 7–9 | 3 × 3 | 3 × 3 |

The grid is a pure function of the tile count and the width (`gridOf`), and it is unit-tested. Tiles keep 16:9, with the video `object-fit: contain`. The order is join order with the person themself last. **Nothing reorders while someone speaks.**

**Active speaker.**

- The speaking person's tile gets a ring and the word "speaking", from the SDK's speaking state, which the roster already uses.
- It is never colour alone.
- The view does not switch focus to whoever speaks. A moving layout is harder to follow, and it would be another thing to get wrong for people using a screen reader.

**Enlarging.**

- A tile is a button: "Enlarge {name}'s video".
- Enter or a click gives that tile the main area and puts the rest in a strip.
- Escape, or "Back to grid", returns.
- Full screen works as the share's does.

**With a screen share.** Both can be on at once, by the same person or by different people. Nothing limits it.

- While the person watches the share and the call view is open, **the share is the main picture** and the cameras sit in a strip: on the right at 1100 px and wider, below the share otherwise.
- Enlarging a camera swaps it with the share, and the share moves into the strip.
- Stop watching removes the share and leaves the grid.
- The share's own controls (volume, mute, full screen) stay with it. The call controls never scroll away.

### 7.5 Bandwidth and quality

| | Desktop | Phone layout (narrow and coarse pointer) |
| --- | --- | --- |
| Capture asks (ideal, not exact) | 1280 × 720, 30 fps | 640 × 360, 24 fps |
| Top layer | 720p, at most 1.2 Mbps | 360p, at most 450 kbps |
| Simulcast layers | 360p at 450 kbps, 180p at 150 kbps | 180p at 150 kbps |
| Codec | VP8, the SDK's default and the widest-supported | VP8 |

- Adaptive stream and dynacast are already on, so a publisher stops encoding a layer nobody receives. A 3 × 3 grid of small tiles receives 180p or 360p.
- **These figures are the SDK's presets, not measurements.** For scale: in a 1:1 call each side sends at most about 1.8 Mbps across all layers and receives one layer. In a 9-camera grid each subscriber receives 8 small layers, and LiveKit sends roughly 9 × 8 streams.
- Issue C8 measures CPU and bandwidth, 1:1 and with 9 cameras, on the droplet, with a share at the same time. Until then the documentation claims no capacity. The server limits neither bitrate nor resolution, as for the share (O6 of private-alpha): these are the client's settings (O11).

**Supported browsers.** Chrome and Edge, Firefox and Safari on desktop, Safari on iPhone, and Chrome on Android are all **untried**, except Chromium with a fake camera in CI. C8 reports each one as tested, unsupported or untried, from what was actually run.

### 7.6 How the controls interact

| Control | Microphone | Camera | Receiving video |
| --- | --- | --- | --- |
| Mute | off | **unchanged** | unchanged |
| Deafen | off (as today: the gate is blocked) | **unchanged** | unchanged: deafen is about sound |
| Watch only | not published | **not available**: "Watch only publishes nothing" | yes |
| Listen only (a grant without publishing) | not published | not available | yes |
| Share screen | unchanged | **unchanged**, and never started by sharing | unchanged |
| Stop sharing | unchanged | **unchanged** (§4.3) | unchanged |

**A camera without a microphone** is voice with the microphone muted. That covers someone who signs rather than speaks. Watch only keeps its meaning, which is to publish nothing (O14).

### 7.7 Accessibility

- Every control is a labelled button that can be reached by keyboard, and every state is in words as well as colour: "Camera in use", "Sending video", "Camera off", "speaking".
- A polite live region announces the person's own camera state, the reason in `camera_ended`, and someone else's camera starting or stopping, but only while the call view is open.
- Each tile's video has a label ("{name}'s camera"), and the name is visible text.
- Focus moves to the enlarged tile and returns to the tile it came from.
- With `prefers-reduced-motion`, the layout changes without animation.
- Every target is 44 × 44 px at least.

## 8. The phone layout

- **Video appears only in the Voice tab.** The call view is drawn on the stage in Voice. Under Chat and Channels the stage holds a watched share only, as today, and no camera tile. Camera tracks are subscribed only while Voice is the current view, so a phone in Chat receives no video.
- **The strip stays one row of four** (#1028): the state button, Hold to talk, Mute, Leave voice.
  - While the person's camera is on, the state button reads "In #general · Camera on", with the camera glyph, and opens Voice, where the camera button is.
  - The Voice tab's dot stays.
  - A fifth control would break #1028's 64 px row at 320 px (O10).
- **The camera stops when the page is hidden** in the phone layout (`visibilitychange` to hidden). Phones handle background capture in different ways:
  - Safari on iPhone mutes the track;
  - Chrome on Android may keep it running;
  - a phone in a pocket should not keep sending.
  
  Turning it back on takes a tap, which is the issue's "no reopening without an explicit action" (O9).
- **The phone's own camera**: the picker lists the front and back cameras by the labels the browser gives them. The first default is the front camera (`facingMode: user`, ideal). A remembered choice is exact.
- **Grid**: the narrow column of §7.4's table. Enlarging fills the stage, and the controls stay in the strip under it.

## 9. Desktop (Model B), a later phase

Under Model B (linux-desktop §6, accepted 2026-10-28), the Rust helper is `p<id>`, which publishes the microphone and plays voice, and the Electron renderer is `p<id>.screen`, which publishes the screen.

**Recommended: the Electron companion publishes the camera.**

- Chromium's camera capture and encoding are what the browser already uses, on every OS. The renderer reuses `web/src/voice/` as it does for the share.
- Under Model A's reasoning (linux-desktop §6.2), video in the helper would need:
  - a video capture stack in Rust (V4L2 or PipeWire camera on Linux, Media Foundation on Windows);
  - new dependencies;
  - a capture process that reopens the `pipewire_stubs.o` question.
  
  ADR-006 makes the helper an audio client.
- A third identity (`p<id>.camera`) would add to the identity grammar, the orphan rules and the folding, all for a participant that the companion already is.

What it needs, all additive:

- `VoiceCompanionRequestV1` gains an optional `camera` bool. An older server's strict decoder refuses it with 400, which the app reads as "this server cannot do camera from the desktop". The response's `publish` is unchanged.
- The companion's token still names `screen_share` alone. A camera claim (§4.2) grants `camera` to the person's companion when the companion asked for it, and to `p<id>` otherwise. §4.3's function decides which, and only one of them publishes the camera.
- The renderer receives cameras under B2 (`canSubscribe: true`, `autoSubscribe: false`), now taking camera tracks as well as screen tracks. As with the screen, this is the client's honesty. The person can see these cameras anyway, so it gives no new access.
- The helper does not change (F17 already subscribes to audio only), and it never subscribes to video.
- Electron's main process allows the `media` permission with video for the companion's renderer only.
- Main stops the renderer's camera on helper loss, leave, sign-out and profile switch, before anything else (linux-desktop §6.4's rule, extended).
- This amends Model B's "screen video only", which Nick accepted. The desktop issue asks him first (O13).

## 10. Schema and wire

All of this is **additive. No version bump** is needed. It goes through the `schema-change` skill, and every existing fixture stays byte-identical.

| Change | Kind | Nick? |
| --- | --- | --- |
| `Permission` gains `PermUseCamera = "use_camera"`, at the end of the catalog, in `Permissions` and in `Vocabularies` | new value of a closed, client-tolerant vocabulary (ADR-014 decision 7) | yes, through ADR-015 (it amends ADR-014's catalog) |
| `Default(use_camera)` = false for both roles, `OperatorOnly` = false, `Requires` = `[use_voice]`, not `Fixed`. **Trap:** `Default` and `OperatorOnly` fall through to `true` today, so the new value needs explicit cases. | behaviour of a new value | as above |
| `permission_test.go`: the fixture test changes from "rows == the catalog" to "rows are a prefix of the catalog, in order, each at most once". A document from a server before `use_camera` lists eleven rows and is still valid. New fixtures `permissions-response-v1-camera.json` and `whoami-response-v1-permissions-camera.json` | test loosened to the documented reading; new fixtures | no (the old fixtures are unchanged) |
| `VoiceCameraV1` `{principal_id, started_at}`; `POST`/`DELETE /v1/channels/{channel}/voice/camera`; the parameter constants for `started_at` and `principal` | new type, routes and constants | no |
| `VoiceParticipant.camera`, optional, omitted when false | additive optional field | no |
| `VoicePresenceV1.camera_ended` (`VoiceCameraEnded`), optional; `VoiceCameraEndReason`, a new closed vocabulary in which a new reason is a new version of presence, as for the share's reasons | additive optional field and new vocabulary | no |
| `voice_camera_full`, `voice_camera_stale`, `voice_camera_rate_limited` | additive error codes in the existing `Error` body | no |
| `VoiceSessionResponseV1`, `VoiceRoomGrant`, `VoiceOccupancyV1`, `VoiceShareV1` | **unchanged** | — |
| Desktop phase: `VoiceCompanionRequestV1.camera`, optional | additive optional field, strictly decoded (older servers refuse it) | no, but Model B's amendment is Nick's (O13) |
| Audit actions `voice_camera_started`, `voice_camera_stopped`, `voice_camera_unclaimed`; removal reasons `camera_unclaimed`, `camera_not_withdrawn` | additive audit vocabulary | no |

The presence rule of voice-control-plane §8 still holds, and `voice_test.go`'s reflection test still covers the new fields: presence carries no token, no room name and no device. No MCP tool is added, because agents are not in voice. Parity (rule 4) holds through REST: the CLI prints `camera` in `conch voice status`, and operators get `conch voice camera stop <channel> <person>` (C5). `go generate ./pkg/schema` emits the TypeScript for the new vocabularies and types.

## 11. Tests

### 11.1 Schema (C2)

- Fixtures for each new shape:
  - `voice-camera-v1.json`;
  - `voice-presence-v1-camera.json`, with two cameras and a share;
  - `voice-presence-v1-camera-ended.json`;
  - the two permission fixtures of §10.
- The `Validate` cases of §5.
- The `Permission` table rows for `use_camera`.
- `schema-compat.sh` green, with every existing fixture byte-identical.
- The generated TypeScript checked against the fixtures.

### 11.2 Server unit (C4a, C4, with the fake LiveKit)

- The claim's checks of §4.2, table-driven, in order, with what each refusal leaves behind (no claim, no LiveKit call, no row).
- §4.3's function over listen-only × share × camera × companion, and for each call site:
  - stopping a share leaves the camera;
  - ending a camera leaves the share;
  - the pass's share withdrawal leaves a camera.
- The cap, including a claim that is already held, and claims racing for the ninth place.
- Idle at 15 s and at 5 s; a muted track counting as idle.
- Unclaimed: withdrawal, the audit row once per track, eviction when LiveKit refuses, and an empty source list.
- The entitlement read on every pass: the role cell off, the brake, `use_voice` off.
- The PUT and #984 hooks ending claims before the response.
- Rotation, restart, LiveKit down, a stale release.
- Operator stop:
  - an owner;
  - an operator with `manage_channels`;
  - a restricted operator (403 `permission_denied`);
  - a manager (403);
  - a non-member (404).
- The rate bound.
- One `started` and one `stopped` row per claim, every reason.
- `camera_ended` only in the person's own document.
- No token, room or device name in any row, log or answer.

### 11.3 Isolation (`e2e/isolation`, C4)

- **Matrix rows:**
  - `POST` and `DELETE …/voice/camera` into A's channel by B's member: the uniform 404;
  - `DELETE` with `?principal=` naming a member of A, by B's operator without `manage_channels`;
  - the restricted-operator prober refused the stop;
  - a member without `use_camera` refused 403 `permission_denied`;
  - the personal-preset phase: the camera is refused.
- **A5's coverage test** (`TestEveryPermissionRouteHasARestrictedRow`) gets a row for `use_camera`.
- **The media half, against the pinned LiveKit:**
  - B bends a token of their own to name A's room, and LiveKit refuses it;
  - B, in their own room, receives no camera of A's;
  - A's member, without a claim, tries to publish a camera directly with the SDK (the "modified client"), and LiveKit refuses the source;
  - A's member, after the role cell is turned off, has the camera withdrawn within the bound.

### 11.4 `e2e/voice`, a new scenario `camera` (C4)

- It runs K1–K7 of §12 on every run.
- A claim; a second person receives the camera track; a stop; an operator stop; the cap; a camera and a share together, each stopped alone.
- Restart drops the claims.
- A refreshed token from during a claim regains the camera after it has ended, and the pass withdraws it and audits it.
- The join grant is still microphone only, and the existing assertion stands.

### 11.5 Web unit (Vitest, C6a, C6b, C7)

- The camera state machine:
  - every transition;
  - a close during each `await`;
  - each error of §7.2 mapped to its message;
  - no fallback when the device is lost;
  - the microphone and the camera kept independent;
  - leave, sign-out, channel change and profile change stopping the capture before the next connection;
  - a renewal adopting the capture;
  - `camera_ended` stopping it without a new claim.
- `wanted` with a camera.
- `gridOf`.
- The phone rule for a hidden page.
- The hygiene test still allows no device API outside `sdk.ts`, the camera included.

### 11.6 Playwright (fake devices, `make web-e2e`, C6a, C6b, C7)

- An explicit start, and that a second browser **receives frames**: its `video.videoWidth > 0`, and `framesDecoded` in `getStats` keeps increasing.
- Joining, unmuting and sharing never start the camera.
- Camera off stops the capture (the local track's `readyState` is `ended`), and so do leave and sign-out.
- **A denied permission** (a context without the fake UI flag, with `--deny-permission-prompts`) leaves voice and chat usable and shows the message.
- A camera and a share together, with the share in the main area and the strip.
- The owner turns `use_camera` off for members, and the peer's tile goes within the bound.
- The phone sizes (320 × 640, 390 × 844): video only in Voice; the strip still one row; the camera stopped on hide.
- Keyboard-only use of every control; the axe checks the suite already runs.

### 11.7 By hand (C8, recorded as tested, unsupported or untried)

- A real camera:
  - Chrome and Firefox on Linux and Windows;
  - Safari and Chrome on macOS;
  - Safari on an iPhone, as viewer and as sender;
  - Chrome on Android, as viewer and as sender.
- Unplugging a camera mid-call.
- The CPU and bandwidth of a 1:1 call and of a 9-camera call on the droplet, with and without a share.

**Approval path.** No approval shape or route changes. However, ADR-014 makes every change to `permittedTx` or `Permission.Fixed` approval-path from A3a on, and C3 changes `permittedTx`'s brake mapping. **C3 is therefore labelled `approval-path`.** It ships the full request → notify → resolve → audit test unchanged, and Nick merges it.

## 12. LiveKit behaviours to verify

These are expected from the share's findings, and C4's `e2e/voice` scenario makes them findings. `lk` publishes its demo video as a camera natively, so no relay has to rewrite the source.

| # | Behaviour | Expected, from |
| --- | --- | --- |
| K1 | The join grant `["microphone"]` refuses a camera | S1 |
| K2 | `UpdateParticipant` adding `CAMERA` to a live connection lets it publish | S2 |
| K3 | Withdrawing `CAMERA` unpublishes the camera at once, and leaves the microphone and a share | S3 |
| K4 | LiveKit's refreshed token carries the updated sources | S4 |
| K5 | A token sent during a claim still grants the camera after it ends | S5 (the pass covers it) |
| K6 | `ListParticipants` lists `"source":"CAMERA"`, type `VIDEO`, with `muted` | S7 |
| K7 | The combined permission (microphone, screen share, screen share audio, camera) is accepted and each source is enforced separately | new |

## 13. Proposed issues, ordered

Each is filed to the issue template once ADR-015 is accepted. Every server slice is `security`.

1. **C1. ADR-015 and this design** (this PR). *Blocked by:* nothing. Nick signs off.
2. **C2. Schema: the camera permission, claim and presence.**
   - **Scope:**
     - everything in §10 except the desktop field;
     - the fixtures of §11.1;
     - `vocabularies.go`, `ts.go` and `go generate`;
     - the fixture test loosened as §10 says.
   - No behaviour. *Blocked by:* ADR-015. A1 (#1012) is done.
3. **C3. Store: `use_camera` in `Permitted` and `permittedTx`.**
   - **Scope:**
     - the role cell, `use_voice`, the brake and the dependency, inside the write transaction;
     - the PUT's dependency check;
     - `enforcedPermissions` lists `use_camera` vacuously until C4's route exists, and then its gate reads `can()`.
   - Table-driven tests over role × brake × `use_voice`.
   - *Labels:* `approval-path`, `security`. *Blocked by:* C2 and A2 (#1013). If A3b (#1015) has landed, it also adds the cell to the PUT's checks; otherwise A3b's own checks take it from the catalog.
4. **C4a. Server: one permission function for every `UpdateParticipant`.** The fixed values at the share's call sites (§4.3) are replaced. `livekit.Permission` and `SourcePermission` learn the camera source. **No behaviour change**, with tests proving it, and it lands before any camera exists. *Blocked by:* nothing. It can run at once.
5. **C4. Server: the camera claim.**
   - **Scope:**
     - §4 and §5: the routes, cap, idle, the pass's enforcement and entitlement read, the hooks, the rate bound, audit and presence;
     - `e2e/voice` `camera` with K1–K7;
     - `e2e/isolation` rows and the media half.
   - *Labels:* `security`. *Blocked by:* C3, C4a. It coordinates with A4 (#1016) and #984: whichever lands second adds "end camera claims" to the other's hook.
6. **C5. CLI: camera in `conch voice status`, and `conch voice camera stop`.** Hostile-server tests. *Blocked by:* C4.
7. **C6a. Web: the camera, one to one.**
   - **Scope:**
     - the lifecycle of §7.2;
     - the button, picker, test camera and self-view;
     - receiving under `wanted`;
     - the call view with up to 4 tiles;
     - the errors;
     - the `Permissions-Policy` change (`internal/server/static.go`, one line, with its test);
     - Playwright: start, receive frames, stop, deny, leave and sign-out.
   - *Labels:* `security` (for the policy). *Blocked by:* C2 for types and mocks; C4 for Playwright; A3b (#1015) for `whoami.permissions`.
8. **C6b. Web: small groups and the share.** The 5–9 grid, enlarging, the active speaker, the strip beside a watched share, the accessibility of §7.7, and the Playwright camera-with-share run. *Blocked by:* C6a.
9. **C7. Web: the phone.** §8, and the phone runs of §11.6. *Blocked by:* C6a, and #1028 merged.
10. **C8. Docs and measurements.**
    - **Scope:**
      - the user documentation, with the camera, the trust model of §6.3 and the browser table;
      - the deployment note on bandwidth;
      - the CHANGELOG line ("camera is off for everyone but owners until turned on");
      - the by-hand checks and the 1:1 and 9-camera measurements of §11.7.
    - *Blocked by:* C6b, C7.
11. **C9. Desktop: the camera from the Electron companion (later).**
    - **Scope:**
      - §9: the companion field and its grant;
      - the renderer's capture and subscription;
      - the main process's permission handler and teardown;
      - `e2e/voice` companion rows.
    - *Blocked by:* C6a; the desktop shell's share (W- and D-series); Nick's word on O13.

**Against ADR-014's A1–A4.**

- **A1 (#1012)** is merged, so C2 can start once ADR-015 is accepted.
- **C3 follows A2 (#1013).**
- **C4 can merge before A3b and A4.** Because `use_camera` defaults off and no PUT exists before A3b, only owners hold it. No release can ignore a cell, because nobody can set one. The cell becomes settable in A3b, and the gate already reads `can()`.
- **For members to use the camera**, A3b (#1015) must be released, so that the owner can turn the cell on. A4 (#1016) must then be in the same release, as ADR-014 already requires, because its role-level hook is what ends camera claims live when `use_voice` goes off.
- **C4a** can run in parallel with all of them.
- **C6a needs A3b** for `whoami.permissions`.

## 14. Open points for Nick

- **O1. Bring webcam video into the alpha, out of P9 (ADR-015).** *Recommend:* yes, browser first, within the scope of §1. Recording and agent visual access stay at P9.
- **O2. Defaults of `use_camera`.** *Recommend:* off for members and for operators, by ADR-014's rule for new permissions. Owners hold it, and one cell turns it on for members. The alternative, on for members, changes every upgraded server without its owner's word.
- **O3. Presets.** *Recommend:* leave both presets as they are, with the camera off. Adding the camera to "personal" would change the meaning of a published preset, and "group" must equal the defaults. A personal server with the camera on then matches no preset, and the console says so.
- **O4. A per-person camera brake.** *Recommend:* none. The voice switch covers the camera (ADR-014 decision 4 adds no brake). It can be added later as a column and an ADR-014 amendment, if a server needs "voice but not camera" for one person.
- **O5. A claim or a token grant.** *Recommend:* the claim (§4.1). With a claim, off-at-joining is enforced by the server, the cap is exact, and the operator stop is symmetric. The join token and its test do not change.
- **O6. The cap.** *Recommend:* 9 per room, a constant, to be revisited after C8's measurements.
- **O7. Who may stop someone else's camera.** *Recommend:* holders of `manage_channels`, as for the share stop. Channel managers get moderation §4's kick, not a camera stop.
- **O8. Showing video.** *Recommend:* explicit: "Show video (n)", or turning on your own camera. The alternative, opening it for everyone in voice whenever a camera is on, takes the conversation's place and a phone's data without being asked.
- **O9. A hidden page.** *Recommend:* on the desktop, keep sending; in the phone layout, stop the camera, and turning it back on takes a tap.
- **O10. The phone strip.** *Recommend:* four controls, with "Camera on" in the state button. A fifth control, "Stop video", is the alternative, at the cost of #1028's 64 px row at 320 px.
- **O11. Quality.** *Recommend:* 720p30 at most on a desktop and 360p on a phone, with simulcast and the SDK's presets (§7.5), adjusted after C8.
- **O12. A rate bound on claims**, with the new code `voice_camera_rate_limited`. *Recommend:* yes: a burst of 10, one every 6 s. Without it a member can fill the audit log and the room's presence frames by toggling.
- **O13. The desktop's camera from the Electron companion.** *Recommend:* yes, in a later phase. It amends Model B's "screen video only", which Nick accepted on 2026-10-28.
- **O14. Watch only and the camera.** *Recommend:* Watch only publishes nothing, the camera included. Someone who wants a camera without speaking joins voice and mutes.

## 15. What a reviewer should push on

- **§4.3.** Every `UpdateParticipant` conchd writes must go through the one function. A missed call site takes someone's camera or share down. A camera call site that misses the share's sources would also strip a share.
- **§4.5.** The unclaimed-camera rule must not evict an honest client on a refreshed token or during a restart (K5), only withdraw from it.
- **§4.6.** The PUT and #984 hooks must end claims before the response, and must never fail the request.
- **§6.2.** No path may carry a capture into another identity, channel or server: profile switching, sign-out and the renewal adoption.
- **§6.4.** The policy is widened only in C6a, and only to `self`.
- **§10.** The `Default` and `OperatorOnly` fall-throughs. A test must fail if `use_camera` is ever on by default, or treated as operator-only.
