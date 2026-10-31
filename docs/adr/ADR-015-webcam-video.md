# ADR-015: Webcam video in channel voice rooms

- **Status:** Proposed. It is accepted when Nick merges the PR that adds it.
- **Date:** 2026-10-31
- **Decider:** Nick (Tier-H)
- **Proposal:** [#973](https://github.com/njdaniel/conch/issues/973)
- **Design:** [docs/design/webcam-video.md](../design/webcam-video.md)
- **Amends:**
  - [ADR-000](ADR-000-charter.md)'s non-goal "no video or screen sharing before P9". ADR-008 has already moved the screen share. This moves camera video in a channel's voice room. The rest of P9 (recording, agent visual participation) stays at P9.
  - [ADR-004](ADR-004-voice-via-livekit.md):
    - "video and screen sharing remain at P9" (the camera moves, as above);
    - "LiveKit only moves audio": it moves audio, the screen share (ADR-008) and camera video;
    - `conchd` remains the only authority on who may publish each of them.
  - [ADR-014](ADR-014-fixed-roles-configurable-permissions.md):
    - **Decision 2:** the closed catalog gains a twelfth permission, `use_camera`, which needs `use_voice`. It is off for members and for operators. "Eleven" becomes "twelve", and the dependency list gains `use_camera` ⇒ `use_voice`.
    - **Decision 4:** `voice_allowed` withholds `use_voice`, `share_screen` **and `use_camera`**. No brake is added.
    - **Decision 5:** both presets leave `use_camera` off, so "group server" still equals the defaults.
    - **Decision 7:** the wire additions are additive, as listed below.
  - The roadmap: the P9 line and the alpha section. That edit is Nick's.

  Accepted ADRs are not edited; this one records what changes. Several things are unchanged:
  - [ADR-006](ADR-006-rust-voice-client.md): `conch-voice` stays an audio client;
  - [ADR-008](ADR-008-private-communications-alpha.md)'s decisions;
  - [ADR-013](ADR-013-one-community-model.md).

  Nothing in ADR-000's locked table moves:
  - D3: the deployment invariant, with LiveKit optional;
  - D6: API parity;
  - D10: agent identity by manifest and capabilities.

## Context

On 2026-10-30 Nick asked for webcam video calls in Conch, "starting with one-on-one conversations and small groups in existing channel voice rooms" (#973).

Today:

- conchd grants a person's voice connection the microphone alone;
- the screen share is a separate claim (ADR-008, voice-control-plane §12);
- the browser's `Permissions-Policy` forbids the camera.

ADR-000 and ADR-004 put video at P9. ADR-008 moved only the screen share into the alpha.

ADR-014 governs who may do what through a closed catalog of permissions. Its rule for adding one is that its default must reproduce what the server did before it existed.

## Decision

1. **Camera video moves into the alpha, browser first**, in the channel-wide voice room only:
   - up to 9 cameras per room, which is the size the design is built and tested for;
   - with the screen share allowed at the same time;
   - no recording, no egress, no agents, no processing of video on the server or for detection;
   - no end-to-end encryption, a server-trust model that the documentation states plainly.
2. **A new permission, `use_camera`**, which needs `use_voice`:
   - it is off for members and for operators, by ADR-014's rule for new permissions;
   - owners always hold it;
   - the voice switch withholds it, and no camera-only brake is added.
3. **The camera is off on joining, and exists only while conchd holds a claim for it.** What the server enforces is exactly this, no more: the join token grants the microphone alone; `camera` is granted only to a claim; a request that continues a claim must name it (`?started_at=`), and a named request never starts a new claim, except the adoption of a claim that the room's rotation ended, within a short window. Whether an un-named request was the person's own choice is the client's honesty, and the browser sends one from the camera button alone.
4. **Turning the camera on is a server claim**, made through `POST /v1/channels/{channel}/voice/camera[?started_at=…]` and ended through `DELETE`:
   - The claim adds the `camera` source to the participant the caller is, with the mechanism the screen share uses.
   - A name that is not the caller's current claim is refused, 409 `voice_camera_stale`, and the client stops its capture.
   - There is no one-per-room rule, but a room holds at most 9 claims.
   - The claimer can stop it, and so can a holder of `manage_channels`; the channel's manager, who may stop a share, may not.
   - The poller judges every track by its kind and its declared source together, withdraws any camera nobody claimed, and evicts a publisher that keeps it.
   - Claims are held in memory, and a restart drops them.
5. **Every publish permission conchd writes is computed by one function** from the person's current claims, under the room's claims lock, at every `UpdateParticipant` call site, with a structural test that there is no other. Stopping a share never removes a camera, and stopping a camera never removes a share.
6. **Revocation is live.** A route that takes the camera away ends the claim before it answers, through the same lock a claim is made under, and a claim in flight re-reads its entitlement under that lock, so no claim is granted after a revocation has answered. Those routes are the permissions PUT, the voice switch, membership loss and the other rotation triggers. Every pass re-reads `use_camera` for each claim holder.
7. **The browser's `Permissions-Policy` becomes `camera=(self)`**. The change lands in the PR that first captures a camera, with a security review.
8. **On a phone, video is shown and received only in the Voice tab.** The camera stops when the page is hidden.
9. **On the desktop (Model B, a later phase), the Electron companion publishes the camera.** The Rust helper gets no video. The camera is granted only to the participant the claiming client is, bound to the companion's own bearer, and Electron's permission handler grants video to one armed press at a time; conchd's `Permissions-Policy` does not reach `conch-app://` and is not relied on. This amends Model B's "screen video only", and the desktop issue asks Nick first.
10. **The wire is additive; no version bump.** The additions are:
    - the permission value;
    - `VoiceCameraV1` and its routes;
    - `VoiceParticipant.camera`;
    - `VoicePresenceV1.camera_ended`, with the new closed vocabulary `VoiceCameraEndReason`;
    - three error codes;
    - audit actions.

    `VoiceSessionResponseV1` and occupancy are unchanged. A1's fixture test is loosened so that a document from a server before `use_camera`, with eleven rows, stays valid. No published fixture changes.

## Alternatives considered

| Option | Why not |
| --- | --- |
| The camera in the join token whenever the person holds `use_camera` | Off-at-joining would be the client's honesty, not the server's. The cap cannot be enforced before publishing. An operator stop leaves the person unable to turn the camera on again without reconnecting. It also changes every join token, and the e2e assertion that pins it. |
| `use_camera` on by default for members | It breaks ADR-014's rule for new permissions. Every upgraded server would offer cameras without its owner's word, which matters most on a personal server. |
| A per-person camera brake | ADR-014 adds no brake. The voice switch covers the case that exists. |
| Video from the Rust helper on the desktop | It needs a video capture stack and new dependencies in Rust, on every OS. ADR-006 makes the helper an audio client. Model B already has a Chromium participant that can do it. |
| Wait for P9 | Nick asked for it now. The share's claim, measurements and tests make it a bounded addition to what the alpha already runs. |

## Consequences

- **New security surface:**
  - the claim routes;
  - the operator stop;
  - the poller's camera enforcement;
  - the live hooks;
  - the permission function that replaces the share's fixed permissions;
  - the `Permissions-Policy` change.

  Every server PR, and the web PR that changes the policy, gets a `security-reviewer` pass.
- **Approval path.** The store change that adds `use_camera` to `permittedTx` is approval-path under ADR-014's rule. It ships the full approval end-to-end test unchanged, and Nick merges it.
- **Ordering.**
  - The schema follows A1, which is merged, and the store change follows A2 (#1013).
  - The server can merge before A3b and A4, because only owners hold the camera until A3b's PUT can turn the cell on.
  - Members can use the camera only once A3b (#1015) and A4 (#1016) are released together, as ADR-014 already requires.
- **Bandwidth sizes the deployment again.** The design's figures are the SDK's presets, not measurements. The 1:1 and 9-camera figures are measured on the droplet before anything about capacity is documented.
- **The hygiene and media rules of the browser hold.** No source but `src/voice/sdk.ts` reaches for a device, and the camera is captured through the SDK. No new npm dependency is needed.
- **Guests.** A live guest in voice receives cameras, as they receive voice and the share, and can never claim one. The design's O15 records why.
- **A gap this uncovers in the share.** The share's claim reads the caller's entitlement before it takes the room's lock, the race decision 6 closes for the camera. It is filed as its own issue and fixed in the shared permission function, not here.
- **Open points O1–O15** are in the design §14, each with a recommendation. Accepting this ADR as written takes the recommended reading of each.
