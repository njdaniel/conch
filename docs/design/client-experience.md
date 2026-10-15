# Conch client experience

**Status:** Design specification for planning, 2026-10-13; not a feature inventory.
**Architecture:** [ADR-007](../adr/ADR-007-client-interfaces.md) (#259).
**Immediate target:** [P3 local prototype](p3-local-prototype.md).

## 1. Scope and language

Requirements below are proposed behavior unless already governed by an accepted
ADR. IDs are stable planning/acceptance references. P3 implements the subset in
the planning brief; later sections describe the intended product, not extra P3 gates.
An image illustrates layout; written behavior and accepted ADRs take precedence
over generated labels, icons and shortcut hints.

| Term | Meaning in the experience |
| --- | --- |
| Server | A self-hosted Conch instance/community. A saved server entry is not a tenant on a shared server. |
| Principal | A durable human or agent identity. |
| Session | One active client/runtime connection associated with a principal. Multiple sessions are not multiple people. |
| Channel | Persistent conversation with server-enforced membership. |
| Net | An overlapping subset of a channel's members, with member/monitor roles; not a nested independent channel. |
| Voice room | Presentation of a channel's voice participation; later scoped nets obey ADR-005. No new unrelated room entity is implied by the mockups. |
| Selected conversation | Text channel/net/thread currently being browsed. |
| Transmit target | Explicit audience receiving PTT audio, independent of selected text conversation. |

## 2. Shared layout and navigation

**UX-01 — Persistent identity and state.** Always identify the active server and
signed-in principal. Show server connectivity separately from voice connectivity.
An online dot alone must not imply microphone capture or voice participation.

Graphical clients use a left channel/net navigator, central conversation or media
area, and a collapsible member/activity panel. The bottom voice strip persists
while browsing. The TUI uses equivalent panels with visible focus, selection,
keyboard hints and an always-visible status line. UI state is local to each
session; opening a channel on one client does not navigate another client.

**UX-02 — Browsing does not route audio.** Selecting a channel, net, member or
thread changes the displayed content only. Changing the voice channel or transmit
target requires its own action. A speaking indicator names the actual source net.

Navigation labels include unread counts only when backed by a defined read-state
model. That model now exists (issue #351): `GET /v1/read-state` gives each channel
the caller is in a `conch.read_state.v1` document with the person's read mark and an
`unread` count of the messages after it that this person may see (capped at 1000, so
a label shows `1000+`). The mark is the person's, shared by all their clients, and
moves forward only (`PUT /v1/channels/{channel}/read-state`). A client that cannot
reach the API shows the honest unread-unavailable state, not an invented count.
Showing the counts in the TUI and the browser is later work; agents have no read state.
Member lists show principals once with expandable sessions; distinguish humans
from agents using a text badge as well as visual styling.

## 3. Connection, login and the lobby

**UX-03 — Connect.** Entry: launch a client without an active connection, choose a
saved server, or follow a supported invite. Show server address and identity before
connection. P3 uses the existing issued-credential model; do not draw a functioning
password/OIDC sign-in until those flows exist. A failed login preserves the address,
does not reveal credentials, and offers retry. Logout clears the local session's
credential state and terminates its media helper. Credential revocation is distinct.

**UX-04 — Arrive outside voice.** Successful login opens the last accessible text
channel, otherwise the first accessible channel or a 'No channels yet' state.
The microphone is inactive. Show 'Not in voice' even when others are talking.
Never autojoin a room from a selected chat, invite preview, or remembered layout.

The lobby shows accessible text channels, permitted voice presence, central chat,
and online members. If names/presence are not authorized or supplied by the API,
show permitted identifiers or an unavailable state, not fabricated names.
An activity card can offer 'Join voice' for a named channel. No captions or audio
from that room are delivered just because its roster is visible.

**UX-05 — Reconnect.** Distinguish Connecting, Connected, Reconnecting and Signed
out. Keep already-authorized visible history while offline, mark it stale, and
disable new sends until reconnected in the initial prototype. Restore the current
view when still accessible. Do not silently resend an ambiguous failed message.
Permission loss clears/restricts affected views promptly; cached access must not
be mistaken for current membership.

## 4. Chat interaction

**UX-06 — Compose and deliver.** Composer names its audience: channel, net or
whisper. Enter sends; graphical Shift+Enter adds a newline. The TUI documents a
working multiline binding for supported terminals rather than assuming modified
Enter is portable. Preserve drafts when switching conversations within a session.
Show Sending, Sent or Failed states without duplicating a message when the server
acknowledgment and live event both arrive. Failure offers explicit retry and a
warning if delivery is uncertain; server IDs determine identity.

**UX-07 — History and selection.** Opening a channel loads an ordered page, then
subscribes to live messages without a gap. Loading earlier history preserves scroll
position. New messages do not pull a reader away from older history; offer a jump
to latest. Reply views inherit the root message's audience; users cannot widen it.
Search and attachments use the same visibility rules as ordinary messages.

Replies, file controls, search, mentions and notifications enter client scope only
when implemented against the public API. Disabled controls explain why; screenshots
must not imply support. Desktop/browser notification preferences are per client and
do not change message authorization. No private message content in a notification
after the relevant access is lost. Human DMs remain future scope.

## 5. Multiple sessions and participants

**UX-08 — Session isolation.** Two TUI instances can use independent configuration
and credential locations without overwriting each other. Browser profiles support
different users; tabs in one profile may share authentication and must not be
claimed as different identities. A principal with two clients still appears once,
with two sessions when the session API supports it. Closing one leaves the other
online. Personal navigation, drafts and connection faults do not propagate.

Independent issued credentials are used for revocation tests. 'Sign out here'
does not mean 'revoke all credentials'. A later account-management action must
explicitly name its broader effect. Concurrent voice by the same principal needs
a media-session policy before support is promised; P3's repeat-login baseline is text.

## 6. Voice and microphone controls (parallel voice track)

**UX-09 — Join and leave.** Join names the destination and requests device access
when needed. Distinguish joining, joined, permission denied, unavailable and failed.
Show listeners, speaker activity and own microphone state. Leave stops local capture,
playback and transmission for the session and returns to the lobby state while text
continues. Browser device permission never substitutes for explicit room joining.

**UX-10 — PTT.** Hold the configured key/button to transmit; release ends it. The
control displays binding and audience, with visible idle/talking/muted/error states.
Native global PTT must work while another application is focused and handle missing
devices, lost releases and reconnects according to the native voice design. Rejoin
requires a fresh press before sending. The browser must advertise focused-window
limitations and close its gate on lost focus; it cannot promise desktop global PTT.

**UX-11 — Mute/deafen.** Mute prevents transmission; deafen stops playback and also
prevents transmission. Undeafening restores the prior explicit mute preference,
but never resumes a held/stale transmission automatically. Microphone selection
shows the actual device and missing-device state; no silently transmitting from an
unexpected fallback. PTT protects transmission, while device-open/capture state is
shown separately and must not be described as 'microphone inactive' if it is open.

**UX-12 — Helper failure.** A native helper crash stops voice, marks it unavailable,
and offers restart/rejoin. Text stays usable. Clients own and clean up only their
own helper; one client quitting cannot interrupt another profile's session.

## 7. Multiple nets and ducking (V5)

**UX-13 — Explicit listening and speaking.** List joined/listened-to nets, speaking
permissions, PTT bindings and the current target separately. A monitor can listen
but not transmit. A net selection in the chat navigator never changes a PTT binding.
Whisper controls show the exact recipients before transmission. Restrict delivery
on the server, not by muting unauthorized tracks in the interface.

**UX-14 — Ducking.** When an explicitly prioritized net speaks, reduce playback
volume of lower-priority nets, then restore it after activity ends. Controls name
the priority order, attenuation and recovery behavior; offer Off and a local preview.
It changes what this listener hears, not sender volume, membership or permissions.
Priority is not automatically granted by a job title. Equal-priority and overlapping
talkers need a deterministic rule; V5 planning must settle exact gain/timing defaults.
Keep stereo placement and per-net volume separate controls; avoid color-only routing.

## 8. Screen sharing and movie night (later media scope)

**UX-15 — Share.** Explicitly select a screen/window and separately choose whether
to include supported system audio. Preview the selection before publishing. Show
an unmistakable sharing indicator and Stop sharing control; source closure or lost
permission stops the share and reports why. Keep microphone and stream-audio mute
independent. OS/browser limitations appear before starting, not as silent failures.

**UX-16 — Watch.** A share becomes the central stage with participant strip and
optional captions. Expand/fullscreen and stream volume affect only the local view.
Stopping the share returns to the voice lounge, preserving the voice connection.
Text chat remains reachable. The TUI shows share metadata and an explicit action to
open a graphical viewer; it does not pretend to render a video stage in character cells.

## 9. Captions and translation (future scope)

**UX-17 — Caption controls.** Separate transcription, translated captions and
spoken translation. Identify speaker, source channel/net, source language and time;
visually distinguish original and translated text. Let the reader choose target
language and reveal originals. Loading, partial, unavailable and failed translations
remain recognizable; never display fabricated transcript text on failure.

Processing location and access must be visible before audio is sent for processing.
Caption audiences never exceed the audio audience. Temporary captions and retained
transcripts are different modes; retention/disclosure policy requires a decision
before implementation. The mockup's 'Not saved' is a proposed mode, not a claim
about the server audit log or external providers. Turning off local display must not
be confused with disabling server-side transcription for everyone.

## 10. AI participants, activity and approvals

**UX-18 — Agent identity.** Show an Agent badge, durable display name, runtime/model
when reported, and session-level online/idle/running/blocked/waiting/failed/offline
state. Heartbeat expiry indicates loss of contact, not proof a remote task stopped.
Keep human online presence separate from an agent's run status.

**UX-19 — Inspect and intervene.** Select an agent to inspect permitted sessions,
current task/run and timestamped activity. Runs have stable IDs and explicit outcome
states. Display runtime/host metadata only to authorized viewers and avoid secrets
or raw process environments. An agent menu lists Message, Cancel, Pause and Resume
only as supported by the adapter. Show requested/acknowledged/failed intervention;
never call a run cancelled just because a button was pressed.

**UX-20 — Approvals.** Existing approval semantics stay authoritative. Show requester,
requested action, options, deadline and current resolution; preserve the required
reason and server authorization. Agents cannot impersonate humans or self-approve.
P3 may link to the existing TUI approval workflow rather than creating browser write
controls. Listening access does not grant tool execution; adapters must enforce
decisions at the action boundary. No implicit unlimited agent-to-agent reply loop.

## 11. Keyboard, accessibility and platform behavior

**UX-21 — Focus.** Tab/Shift+Tab cycles available panes/controls; arrows navigate
lists; Enter activates the focused item or sends in the composer; Escape dismisses
an overlay without leaving voice. Keep navigation keys out of text editing except
where explicitly documented. Illustrative F2/F5/F10 shortcuts need collision checks
in real terminals and browsers; browser-reserved keys need an alternative binding.
Disconnect must not be triggered by ordinary typing. PTT bindings are configured
separately from UI shortcuts and never enable transmission outside voice.

**UX-22 — Small surfaces.** Collapse the right panel before shrinking chat beyond
readability; on small terminals use switchable panes. Keep audience and voice state
visible. Provide a meaningful terminal-too-small state. Use text labels in addition
to colors, visible keyboard focus, readable contrast and scalable graphical text.
Browser/desktop controls need semantic accessible names and keyboard operation.

| Capability | TUI | Browser | Desktop |
| --- | --- | --- | --- |
| Chat, roster, agents | Native terminal views | Shared React views | Same React views |
| Media | Native helper when supported | Browser media/permissions | Native adapter + media integration |
| Global PTT | Native input, not terminal key events alone | No general cross-app guarantee | Native input, validated per OS |
| Screen video | Metadata/open viewer | Graphical stage | Graphical stage |
| Installation | Terminal client; helper bundled for media | URL, no native install for text | One installation/launcher |

## 12. Empty/error state contract

**UX-23 — Honest recovery.** All clients distinguish these cases:

| State | UI and recovery |
| --- | --- |
| No channels | Explain membership is needed; no automatic unauthorized channel creation. |
| Empty channel/voice room | Invite a message or offer explicit Join; no fake participants. |
| Server unreachable | Retry/address controls, stale-state label; no silent message queue in baseline. |
| Session revoked | Stop protected requests/media; return to sign-in with drafts handled locally. |
| Channel/net access removed | Remove live access and stop affected media; other authorized views remain usable. |
| LiveKit unavailable | Text/agents remain available; voice action explains unavailability. |
| Microphone denied/missing | Identify problem, offer device/settings action; do not transmit. |
| Agent heartbeat stale | Mark session stale/offline and run outcome unknown as appropriate. |
| Adapter cannot pause/cancel | Explain unsupported action rather than offer a false success. |
| Captions/share unsupported | Explain platform or service limitation; preserve text/voice as applicable. |

## 13. Screen inventory and remaining design decisions

See the [five current mockups](mockups/README.md): graphical movie night, graphical
voice lounge, TUI chat/voice/captions, graphical lobby and TUI lobby. Next concepts
are graphical multiple nets, TUI multiple nets, and a one-to-one multilingual view.
The last does not by itself authorize implementing human DMs ahead of the roadmap.

P3 planning must settle endpoint/session schemas, presence visibility, browser auth,
configuration/profile isolation, reconnect cursor behavior, and a minimal agent
runtime adapter. Later planning settles exact shortcut maps, multi-session voice
ownership, media process boundaries, caption retention, ducking defaults and desktop
platform support. These are named decisions, not permission to let individual
implementation tasks invent conflicting behavior.
