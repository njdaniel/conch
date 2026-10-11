# Voice spike report — LiveKit, Rust client, push-to-talk

**Date:** 2026-10-05 · **Author:** Fable (Principal Engineer) · **Decision owner:** Nick
**Issue:** #87 (track phase V0; feeds ADR-004 "Settled by spike") · **Code:** branch `spike/voice-v0`, `spike/voice/` — not for merge

**Status: partial.** Everything that can run unattended on one machine is done. Three things need Nick at the keyboard and are listed under [Not yet done](#not-yet-done).

## Answers

| # | Question | Answer |
|---|---|---|
| 1 | Audience mapping | **One LiveKit room per net.** The one-room, server-driven design does not work. |
| 2 | Go without a LiveKit SDK | **Yes.** Tokens and room control work from the standard library. |
| 3 | Rust SDK fit | **Works**, with one real cost: it needs clang ≥ 21 to build. |
| 4 | Echo cancellation / noise suppression | **Available from Rust and runs.** How well it works is not measured. |
| 5 | Global push-to-talk on Wayland | **`evdev` only.** COSMIC exposes no global-shortcuts portal. Not yet exercised. |

## 1. Audience mapping

Tested with headless clients: `alice` publishes a tone, `bob` is in the audience, `carol` is not and asks for every track she can see (`force=1`). Frames received are counted at 100 per second.

| Setup | bob (audience) | carol (outsider) | Enforced by |
|---|---|---|---|
| **A1** One room; every token `canSubscribe=false`; server calls `UpdateSubscriptions` for bob | 0 frames | 0 frames | — does not work |
| **A2** One room; publisher sets an allow-list of `bob` | 700 frames | 0 frames | the *speaker's* client |
| **A3** One room; no allow-list (control) | — | 680 frames | nothing |
| **B** One room per net; carol holds a token for another net | 700 frames | 0 frames | the token `conchd` signs |

- **A1 fails.** `UpdateSubscriptions` returns `200 {}` but no audio arrives when the token denies subscribing. There is no server API to grant one track to one participant.
- **A2 works against a modified listener**, but the allow-list is set by the speaking client. `conchd` would not be the authority and could not audit it reliably.
- **B is enforced by `conchd` alone.** The room name is inside the signed token, so an outsider has nothing to ask for.

Cost of listening to several nets (one process, 10 s, a talker in every room):

| Rooms | Connect time each | CPU | Memory (RSS) |
|---|---|---|---|
| 1 | 639 ms | 0.58 s (≈6% of one core) | 123 MB |
| 4 | ≈390 ms | 0.70 s (≈7%) | 60 MB |

The memory figures are single runs and clearly noisy; read them as "60–125 MB, not growing with room count". All four rooms delivered full audio (≈1000 frames each).

Live membership changes work from `conchd`: `RemoveParticipant` disconnects a listener mid-stream, and `UpdateParticipant` with `can_subscribe=false` stopped a listener's audio at 357 frames of a longer stream.

Latency, tone switched on → first loud frame at the listener: **38 ms** on loopback, excluding microphone and speaker.

**Recommendation.** Rooms per net, named by `conchd`, with short-lived tokens; channel-wide voice is just another room.

**Whisper — proposed, not tested as a whole.** Give each principal an "inbox" room in which only they can subscribe; a whisper is a publish-only token to the target's inbox, minted and logged by `conchd`. It is built from behaviours verified above (`canSubscribe=false` blocks reception; tokens bound rooms). Connecting takes ~0.4–0.6 s, so the client would keep recent targets connected. The publisher allow-list (A2) is the fallback.

## 2. Go without a LiveKit SDK

`spike/voice/lkctl/main.go` — its `go.mod` has no `require` lines. Against server 1.13.7 it:

- mints join tokens (HS256 JWT: `iss` = API key, `sub` = identity, a `video` grant) that the server accepts;
- calls `ListRooms`, `ListParticipants`, `UpdateSubscriptions`, `RemoveParticipant`, `UpdateParticipant` over the HTTP (Twirp JSON) API, all returning 200.

The whole token function:

```go
func mint(key, secret, identity string, grant map[string]any, ttl time.Duration) string {
	now := time.Now()
	head, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims := map[string]any{"iss": key, "nbf": now.Unix() - 5, "exp": now.Add(ttl).Unix(), "video": grant}
	if identity != "" {
		claims["sub"] = identity
	}
	body, _ := json.Marshal(claims)
	signing := b64(head) + "." + b64(body) // b64 = base64.RawURLEncoding
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil))
}
```

No LiveKit Go dependency is needed for V3.

## 3. Rust SDK fit

- **Publishing and receiving PCM works.** 48 kHz mono, 10 ms frames, through `NativeAudioSource` and `NativeAudioStream`.
- **Microphone → LiveKit works:** ~100 frames/s from the default mic reached a second client, with speech detected.
- **LiveKit → headphones works:** 600 remote frames played through PipeWire.
- **The spike bypassed `dmn-audio`.** It pipes `pw-record` / `pw-play` in raw mode. Integrating `dmn-audio` (after it gains 48 kHz and an output stream) is untested V4 work.

Build and size, on this machine (16 cores):

| Measure | Value |
|---|---|
| Clean release build | ≈41 s for 316 crates (libwebrtc is downloaded prebuilt, not compiled) |
| Binary | 36.7 MB; links only libc, libm, libgcc |
| `target/` after debug + release | 1.6 GB |

**The real cost: the build needs clang ≥ 21.** `webrtc-sys` refuses older compilers because its libwebrtc is built against a recent libc++. Pop!_OS/Ubuntu 24.04 ships clang 18 and offers at most 20. The spike used clang 22.1.8 extracted from the LLVM release tarball (≈610 MB, no root), plus `--gcc-install-dir` so it finds GCC 13's C++ headers. CI and every contributor to `voice/` would need the same.

## 4. Echo cancellation and noise suppression

`livekit::webrtc::native::apm::AudioProcessingModule` exposes echo cancellation, gain control, high-pass filter, and noise suppression. The spike ran it on ~590 captured frames with the playback stream as the echo reference, without error.

Not measured: whether it actually removes echo with open speakers. Until that is tested by ear, plan the MVP as headphones-recommended.

## 5. Global push-to-talk on Wayland (COSMIC)

- **Portal: not available.** This session's `org.freedesktop.portal.Desktop` lists no `GlobalShortcuts` interface (nor `InputCapture`).
- **`evdev`: needs permission.** Keyboards are `root:input 0660`; the user is not in `input`, so opening the device fails with "Permission denied".
- **Consequence to accept knowingly:** read access to `evdev` lets a process see *every* keystroke. A narrower grant (a udev rule or ACL for one device) is possible but still exposes that whole keyboard.

The key-watching code (press and release) is written but has not run.

## Not yet done

Each needs Nick; commands assume `spike/voice/` on `spike/voice-v0`, built per its README.

1. **Hold-to-talk.** Grant read access to one keyboard for this boot, then hold Right Ctrl:
   `sudo setfacl -m u:$USER:r /dev/input/event7` then `./talk.sh nick 127.0.0.1 /dev/input/event7:97`
2. **Two machines on a LAN.** `./server.sh 192.168.1.245` here, `./talk.sh <name> 192.168.1.245` on each. The dev server uses publicly known keys — trusted network only, and stop it afterwards (`docker rm -f conch-lk-spike`).
3. **Echo cancellation by ear.** Two clients, one on open speakers with `APM=1`, then `APM=0`.

Also unverified: publishing into several rooms from one process (only multi-room *listening* was measured), token expiry and refresh behaviour, and anything beyond loopback (NAT, packet loss).

## What was used

| Component | Version |
|---|---|
| LiveKit server | 1.13.7 (`livekit/livekit-server:latest`, Docker, `--dev`) |
| `livekit` crate | 0.9.3 (`libwebrtc` 0.3.50, `webrtc-sys` 0.3.47, WebRTC tag `webrtc-89d790b`) |
| Other crates | `tokio` 1.53.2, `futures`, `anyhow` |
| Rust / Go | rustc 1.96.1 / go 1.25.0 |
| clang | 22.1.8 (LLVM release tarball) |
| PipeWire | 1.6.8 (`pw-record`, `pw-play`) |

Re-run the audience experiments with `spike/voice/exp-audience.sh`; a second full run reproduced A1.

## Effect on the plan

- **ADR-004:** audience mapping is rooms per net; no Go dependency. Both can be written into the ADR.
- **ADR-006:** add the clang ≥ 21 requirement and how CI gets it.
- **V4:** global push-to-talk means an `evdev` permission step in setup (`conch-voice doctor`), with the keystroke-visibility consequence documented.
- **V5:** whisper design (inbox rooms) needs its own small test before it is committed to.
