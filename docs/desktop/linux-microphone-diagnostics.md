# Linux microphone diagnostics

Use this procedure when the Linux desktop shows **Transmitting** and a second
person hears silence. It covers [#2137](https://github.com/njdaniel/conch/issues/2137)
and the single-client control for [#2076](https://github.com/njdaniel/conch/issues/2076).
The physical Linux-to-phone reproduction is pending.

## What the indicators measure

| Stage | Evidence on current main |
| --- | --- |
| Selected device and capture | **Voice options** shows the selected microphone and the helper's capture status. `AudioSystem::microphone` opens the configured PipeWire source or the requested default. The helper rejects a stand-in for a missing named microphone, even if PipeWire connects it. The graph check below shows the visible stream and links. |
| Frames and processing | `PipeWireMic::read` reads 10 ms frames from the capture ring. The transmit task processes them with echo cancellation and noise suppression when configured, then runs the open-mic detector and gate. **Microphone is on** means the helper accepts capture availability. There is no managed UI measurement of frame counts or pre- and post-processing energy on this build. |
| Talk mode, mute and gate | The native strip and tray take **Transmitting** from the helper's session state. It describes an active local transmission state, including its configured release tail. A press in PTT or a fresh sound onset in open mode can start it. The status has no sample, packet or remote playback measurement. |
| Publication and transport | The helper publishes a LiveKit microphone track, initially mutes it, and unmutes it for an allowed transmission. The gate hands allowed frames to the SDK source. Desktop diagnostics can show failures to start or publish, but a connected session and an unmuted publication can still carry silence. This build exposes no native sender byte counter in the UI. |
| Subscription and playback | In the browser, **Voice diagnostics** reads statistics for subscribed microphone tracks. Packet loss, jitter and server RTT describe the transport. Compare inbound audio packets, decoded energy and what the listener hears to identify a receiver problem. |

Code paths are [PipeWire capture](../../voice/conch-voice/src/audio_linux.rs),
[session](../../voice/conch-voice/src/session.rs),
[transmit](../../voice/conch-voice/src/transmit.rs),
[LiveKit](../../voice/conch-voice/src/livekit.rs) and the
[native voice panel](../../web/src/voice/native/NativeVoicePanel.tsx).
The v4 schema from [#2085](https://github.com/njdaniel/conch/issues/2085)
defines mic levels, gate events and call quality. Current Electron offers
v2/v1 by default and v3/v2/v1 with Linux share sound enabled. The managed
helper does not generate the v4 mic and quality diagnostics here. Coordinate
runtime diagnostics with [#2080](https://github.com/njdaniel/conch/issues/2080).

## Run the microphone test

1. Start a trusted desktop build in the affected session. Record the build
   revision and desktop, helper, Electron, server, PipeWire and Android
   Chrome versions. Record the desktop session and app backend separately
   (for example, COSMIC Wayland with XWayland). In the desktop settings,
   open **Diagnostics → Save diagnostics…** while still joined. From the
   preview, transcribe only the version fields and the protocol in
   `voice: helper … speaks …`. The bundled helper's version is available
   from `resources/conch-voice --version` in the app's install directory.
   Use aliases for the profile, server, channel and device.
2. Use a test channel with just the desktop sender and an independent
   Android phone browser listener. Leave voice on any browser on the Linux
   computer and stop all shares. Both people use headphones. On the phone,
   join the channel, leave the microphone muted, enable playback with
   **Tap to hear the channel** if prompted and check media volume, deafen,
   participant volume and the selected output route. Keep Conch foreground.
3. Note the desktop **Microphone** selection as default or named, the
   displayed capture state, mute, deafen and talk-key readiness. In
   **Push to talk**, run three 8-second intervals: quiet and released,
   speaking while held, then speaking after a 3-second release pause.
   Repeat the held interval with the desktop microphone muted. Record the
   local indicator and the listener's result for each interval. If the key
   is unavailable, record the displayed reason and mark PTT untested.
   Leave permissions as they are.
4. Under **Talk mode**, explicitly choose **Open microphone**, confirm,
   unmute the desktop and wait for at least 3 seconds of quiet. Speak for
   8 seconds, then wait for the gate to close during quiet. Repeat with the
   desktop microphone muted. Use the same microphone and phone settings.
   A mode change, unmute or device switch needs a fresh onset after quiet.
5. If needed, repeat the failing interval after leaving and rejoining,
   then after choosing another available microphone. Change one variable at
   a time and record whether the selection persists. For a receiver control,
   leave desktop voice before joining from the Linux browser and repeat the
   speech interval with the same phone. Record that as a separate client run.
6. Restore the original microphone, talk mode and mute state. Leave voice
   on both clients. Record audible speech during authorized transmit and
   silence after its tail, on mute and on leave. If duplicate-client echo
   needs a separate test, use the matrix in #2076 after the single-client
   runs, keeping desktop and browser playback paths distinct in the notes.

### Check PipeWire routing

On the same login session as the desktop app, use the managed helper's
exact PID. `pgrep -x conch-voice` lists the candidates locally. If there
is more than one, identify the child of the test desktop instance. Set
`helper_pid` locally to that number. Run the collector while joined, once
idle and once during a speaking interval:

```sh
python3 scripts/linux-mic-capture-status.py "$helper_pid"
```

Run it as the desktop user with Python 3 and `pw-dump` available. It takes
one read-only snapshot with a 3-second, 4 MiB limit. It counts connections
matching the owning PipeWire client's
`pipewire.sec.pid`, their audio input streams and active links, and which
links come from an audio source or the current default, when known. A link
count is a graph count and can include multiple ports on the same source.
The script does not read microphone samples or print device names, IDs,
PIDs, paths or raw graph data. No capture or route settings are changed.

- Zero helper clients means the PID is absent or not attributable in this
  snapshot. Check that the helper is still running on the same PipeWire
  connection before interpreting other zeros.
- A known helper with no running input or source link helps narrow the
  issue to capture, routing or eligibility. Check the selected microphone,
  publish grant and any missing-device indication. A link to the default
  can also be a PipeWire stand-in the helper refuses.
- A running input and active source link establish graph activity. The
  samples may still be silent, stalled in the helper, or lost downstream.

Use the system input meter during the same speech as an independent check
of a microphone signal. The system meter's selected input must match the
one under test. Record whether it moves. It cannot measure what the helper
reads after buffering or passes after processing.

### Check received audio

On the phone, open **Voice diagnostics** and note whether the remote
participant has transport figures. If Chrome exposes
`chrome://webrtc-internals`, open it in a separate tab before the run and
return to the Conch tab for each speech interval. Read the audio
`inbound-rtp` row for the test sender afterward. With only one remote
participant and shares off, identify the microphone receiver locally and
transcribe only numeric deltas:

- `packetsReceived` and `bytesReceived` for inbound traffic.
- `totalSamplesReceived`, `concealedSamples` and
  `totalSamplesDuration` for decoded delivery and concealment.
- `totalAudioEnergy` and `totalSamplesDuration` for received level.
  Compare the ratio of their changes during the quiet and speech windows.
  A missing sample, a reset or a zero duration gives no level result.

If Chrome cannot show the track stats, record that as unavailable and
keep the audible result. Use the same stream in each interval and begin
new samples after a reconnect or track replacement. Do not download the
WebRTC dump, enable its recording options or include screenshots of its
tables. Use the [WebRTC stats definitions](https://www.w3.org/TR/webrtc-stats/)
to interpret fields the phone exposes.

| Observation | Next point to measure |
| --- | --- |
| Local status never starts, system meter moves, and open mode works | Check talk-key readiness, mute/deafen/lock state and the active talk mode. |
| Local status starts, input is missing or the matching system meter stays flat | Check capture availability, selected/default routing and the physical microphone. |
| Local status starts, a source is active, yet there is no receiver track or no packet increase | Distinguish native publication and outbound RTP from subscription or room transport. The current desktop UI cannot place this boundary by itself. |
| Receiver packets and duration advance, but energy does not rise with speech | Check raw vs processed helper levels, the gated feed and concealment. Packets can carry digital silence. |
| Receiver energy tracks speech, but the listener hears silence | Check the browser playback gesture, per-person volume, phone media output and another known audible sender. |

For a follow-up after a reproducing run, compare aggregate raw/processed
frame counts and levels with the number of frames passed after the gate,
then the microphone publication state and track-scoped outbound RTP
deltas. Gather those through the existing diagnostics work. Correlate both
ends using elapsed time and transcript-free result notes.

Keep one row per phase with elapsed time, mode, capture state, gate state,
graph counts, receiver deltas and heard yes/no. Mark missing measurements
as unavailable. Keep only those rows and the version facts. No audio,
names, key codes, tokens, server addresses, channel names, raw logs or
WebRTC exports are needed in an issue report.

## Separate 20-minute screen share check

[#2115](https://github.com/njdaniel/conch/pull/2115) switches the desktop
companion to separate send and receive peer connections at join. This
separates the send section's VP8 bitrate parameters from the receive
sections in the publisher BUNDLE. [#2063](https://github.com/njdaniel/conch/pull/2063)
adds the CPU limitation and stall diagnostics. Neither change has a
recorded 20-minute physical acceptance run in [#2055](https://github.com/njdaniel/conch/issues/2055).

1. Use a trusted build containing both changes, with native Wayland screen
   capture on the COSMIC machine and Android Chrome as the viewer. Keep
   Linux share sound off. Share a test window with visible continuous
   motion and an elapsed clock. Start from the build's default quality and
   record its resolution, rate and mode.
2. Watch continuously on the phone for 20 minutes, keeping it foreground
   and awake. Open **Voice diagnostics** on both ends. At start, minutes
   5/10/15/20 and any symptom, check whether the desktop preview and phone
   motion advance. Record desktop capture/sent FPS, codec and limitation,
   plus phone received/rendered FPS and playback freezes if available.
   Record the phase and elapsed time of any negotiation, stall, CPU or
   reconnect evidence. Record only the outcome or fixed warning category
   from the diagnostics preview.
3. Record 20 minutes without a freeze, or the observed failure and the
   measured boundary. Falling capture FPS points toward capture. Good
   capture next to falling send FPS and a CPU limitation points toward
   encoder adaptation. Continuing received frames with stalled rendering
   points toward viewer playback. A gap with no evidence stays unresolved.
   Stop the share and leave voice.

## Share sound feedback remains a separate gate

[#2010](https://github.com/njdaniel/conch/pull/2010) records the verified
source design for [#1999](https://github.com/njdaniel/conch/issues/1999).
The existing synthetic PipeWire test passed when it could exclude the
helper's direct playback. A monitor relay presented as an ordinary,
detached playback stream let the synthetic 1,250 Hz signal through. The
actual leaking stream from the physical COSMIC session is still unknown.

Keep Linux share sound off by default. The selected-source implementation
needs trusted picker-to-app identity, positive isolation from call audio,
a reviewed `dmn-audio` revision, and a separately signed chosen-source
schema change. Server consent and withdrawal need an expected-share
precondition with their own review. Re-enabling the default also needs the
physical [L-S6 and per-scope checks](../design/linux-share-audio.md#197-by-hand-checks-l-s6a)
and Nick's approval. The microphone and video checks above do not exercise
that share audio route.
