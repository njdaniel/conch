# Browser client UX audit

**Issue:** #577. **Date:** 2026-10-18. **Status:** independent review, for the project owner's approval; nothing here is accepted until then.
**Reviewer:** an AI agent that did not build the client, driving the running web client in Chromium through Playwright.
**References:** [mockups](../design/mockups/README.md), [client experience](../design/client-experience.md) (UX-01 to UX-23), [browser UI polish](../design/browser-ui-polish.md) (#536), [ADR-007](../adr/ADR-007-client-interfaces.md), [ADR-008](../adr/ADR-008-private-communications-alpha.md), [private alpha](../design/private-alpha.md).

Every finding below is something seen in the running client, with the steps that show it. Guesses are marked as guesses. No product code was changed.

## Summary

The client gets the most important rule right: **signing in, entering a channel and browsing never joins voice or opens the microphone**, and joined voice stays where it was while you read other channels. Leaving voice stops capture. Voice survives a conchd restart; drafts survive it too. Screen sharing works from both sides, also on a phone, and keeps playing while the watcher reads another channel.

What a Discord-familiar tester will trip over is **finding where people are talking**. Voice presence is shown only for the channel on screen, and for someone not in voice it is a single read that never updates, so the strip can show people who left and miss people who arrived. The Join voice button does not name the room it joins. Around that, several voice states give the wrong reason (a permission refusal reads "Voice status could not be read"; a denied microphone reads "Voice unavailable; trying again"), the roster clips at 1280 px, and on a phone joining voice pushes the newest messages out of view.

Findings: **0 × P0, 2 × P1, 9 × P2, 16 × P3** (27 in all).

### Top five, in the recommended order of implementation

| Order | Improvement | Findings | Size |
| --- | --- | --- | --- |
| 1 | **Name the destination and the way back.** "Join voice in #game-night"; make "Connected in #game-night" a control that opens that channel. | F01 (part), F04 | quick |
| 2 | **Keep the voice roster live for people not in voice**, so what the strip shows before joining is true. | F02 | small to medium |
| 3 | **Say the real reason when voice cannot start**: no voice permission, microphone denied, and a status that recovers after a reconnect. | F06, F07, F27 | quick |
| 4 | **Never hide who is in voice**: the roster wraps or says "+N", with a count, at every width. | F03 | quick |
| 5 | **Show voice occupancy server-wide**: each channel in the list shows how many are in its voice room, and the strip says where people are when you are elsewhere. | F01 (rest) | larger, may need an API |

Order rationale: 1, 3 and 4 are small client changes that remove the commonest wrong turns; 2 makes the information the strip already shows trustworthy before 5 spreads it to the channel list. The phone layout (F08) and keyboard push-to-talk (F09) are the next two.

## Environment

| | |
| --- | --- |
| Commit | `3d63ce2` (origin/main, "Merge pull request #575"); the server and the web client both report version `3d63ce2` |
| Build | `make build`; `make web-build` with Node 24.21.0 |
| Server | the audit's own `conchd serve` on `127.0.0.1:28577`, a fresh data directory, `--web-dir web/dist`, LiveKit at `ws://127.0.0.1:27880` with `--livekit-room-prefix audit577-` |
| Browser | Chromium headless shell 149.0.7827.55, driven by `@playwright/test` 1.61.0 (the repository's copy); `livekit-client` 2.22.3 in the bundle |
| OS | Linux 7.0.11 |
| Viewports | 1440×900 and 1280×720 (desktop); 390×844 phone (`isMobile`, `hasTouch`, scale 2); 720×450 as 200% zoom of 1440×900 |
| People at once | two to five browser contexts, one per person |

**Demo data.** Made through the API and `conch admin`, the way `scripts/dev-stack.sh` and `web/e2e/` do: an operator `op` from `conchd bootstrap-operator`; channels `#general`, `#random`, `#game-night` (used for voice), `#book-club` (only op, ana and ben are members: Conch has no public/private flag, every channel is membership-only, so this is the "private" one) and `#announcements` (left empty for the empty state). `ana` (may invite), `ben` and `cy` joined through personal invites redeemed over the API; 33 messages of ordinary chat, one long paragraph and one long URL. During the audit `dee` joined from ana's invite link in a desktop browser, `eve` from an operator's open link on a phone, `kit` from an invite without voice, and `gus` from an invite redeemed over the API.

**What fake devices cover, and what they do not.** Chromium ran with `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream --auto-select-desktop-capture-source="Entire screen"`. The fake microphone is a periodic tone and the fake screen a 1280×720 test pattern; permission prompts are accepted without a dialog. They prove **the interface's states** (capture opened or not, LiveKit participants and tracks, transmit reports, received audio energy and video frames). They prove nothing about real microphones, headsets, echo, noise suppression, speech detection, background-tab behaviour, the browser's real share picker, or other browsers and operating systems. One run without the fake-UI flag and with the microphone permission refused covered the denied state (F27).

**How capture and voice were checked.** An init script wrapped `getUserMedia`/`getDisplayMedia` on each page to record every stream and whether its tracks were live; LiveKit's own room service was asked for the participants and tracks of the audit's rooms (`audit577-` prefix); conchd's `GET /v1/channels/{c}/voice` gave its view of presence; listener-side audio was measured as `inbound-rtp` `totalAudioEnergy`.

**Console and network.** Recorded per flow (console errors and warnings, page errors, failed requests, HTTP 4xx/5xx). Apart from what F25 lists and the expected refusals (404 on a dead invite's preview, 409 on a taken name, 403 for a person without voice, connection refused while conchd was stopped), the flows logged nothing.

## Journeys tested

| Journey | Viewports | Result |
| --- | --- | --- |
| A brand-new person opens an invite link, picks a name (a taken name first), arrives in `#general`, sends a first message | 1440; phone (open link) | Works. The code leaves the address bar at once; nothing joins voice; first message shows Sent and reaches others live. [landing](browser-ux-audit/journey-01-invite-landing-1440.png), [name taken](browser-ux-audit/journey-02-name-taken-1440.png), [first message](browser-ux-audit/journey-03-first-message-1440.png), [phone](browser-ux-audit/journey-04-first-message-phone.png) |
| Used, closed, expired and malformed invite links | 1440; phone | One message for all four, by design (F19). [page](browser-ux-audit/F19-invite-no-longer-works.png) |
| Enter the server, browse channels and members, not in voice | all | No capture, no LiveKit participant, no conchd presence, for every person who signed in or joined. [lobby](browser-ux-audit/baseline-lobby-1440.png), [phone](browser-ux-audit/baseline-lobby-phone.png) |
| Join voice in `#game-night`, then read `#general` and `#book-club` | 1440, 1280, phone | Voice stays in `#game-night`; the strip says "Connected in #game-night". [joined](browser-ux-audit/journey-05-joined-voice-1440.png), [browsing](browser-ux-audit/F04-browsing-while-in-voice-1440.png) |
| Push to talk by pointer, touch and talk key; mute; deafen; open microphone; seen from the other side | 1440, 1280, phone | Transmission follows the gate; others see "transmitting". Gaps: F09, F11, F13, F21 |
| Screen share by one, watched by a desktop and a phone, watcher switches channel, sharer stops | 1440, 1280, phone | Works; the picture keeps playing across the channel switch. [watching](browser-ux-audit/journey-06-watching-share-1440.png), [phone](browser-ux-audit/journey-07-watching-share-phone.png), [sharer](browser-ux-audit/journey-08-sharing-1280.png) |
| Leave voice and stay in the server | 1440 | Capture tracks end, LiveKit drops the participant, the channel on screen stays. [left](browser-ux-audit/journey-09-left-voice-1440.png) |
| conchd stopped for 30 s under three open tabs (one in voice), then started | 1440, 1280, phone | "Reconnecting" within 6 s, stale banner, composer disabled, draft kept; LiveKit voice continues; all "Connected" within 2 s of the restart; the draft sends. [down](browser-ux-audit/F17-server-down-1280.png), [recovered](browser-ux-audit/journey-10-recovered-after-restart-1280.png). Gap: F06 |
| Empty channel; refused actions (share while another shares; voice for a person without it; microphone denied) | 1440 | F14, F07, F27, F16 |
| Keyboard only: sign in, Tab order, arrows in the channel list, Settings and Escape, join/talk/leave | 1440 | Focus ring visible everywhere (2 px `#8ec5ff`, [example](browser-ux-audit/a11y-focus-ring.png)); the channel list is one Tab stop with arrow keys; Settings closes on Escape and returns focus to its button; the phone's Members panel opens over the page with a Close button and no sideways overflow ([phone](browser-ux-audit/a11y-phone-members-panel.png)); Escape in voice does not leave voice. Contrast measured on the page: muted text 7.8:1 or more. Gaps: F09, F20 |
| Sign in by pasting a credential (operator-issued) | 1440 | Works; says where the credential comes from. [page](browser-ux-audit/baseline-signin-1440.png) |
| 200% zoom | 720×450 | Usable: the page scrolls as a document as designed. [in voice](browser-ux-audit/a11y-zoom200-in-voice.png) |

## Findings

Severity: **P0** blocks a tester; **P1** a tester will regularly go wrong or be misled; **P2** a real defect or gap with a workaround; **P3** polish. Kind: defect, confusing flow, visual inconsistency, not yet implemented.

### F01 — Nobody can see where people are talking unless they open that exact channel; Join voice does not name its room

- **Severity / kind:** P1, confusing flow. **Viewports:** all.
- **Steps:** ana opens `#game-night` and presses Join voice. ben is on `#general`.
- **What happens:** ben's strip says "Not in voice · Microphone off · Join voice · Watch only". Nothing in the channel list, the members column or the strip says that anyone is in voice anywhere. Pressing Join voice there joins `#general`'s empty room; the button's name is only "Join voice". After ana herself leaves voice from `#general`, her strip no longer shows the room her friends are still in.
- **Should:** UX-04: "An activity card can offer 'Join voice' for a named channel"; UX-09: "Join names the destination". The issue's Discord benchmark: visible voice-channel join affordances.
- **Impact:** "Meet me in voice" sends each tester to whichever channel they happen to be reading. They end up alone in different rooms.
- **Recommendation:** quick part: label the button "Join voice in #general" (and Watch only likewise). Larger part: show, per channel in the list, a speaker mark and the number in its voice room, and when you are not in voice, a line in the strip such as "3 in voice in #game-night · Go there". Keep Conch's model (voice belongs to a channel, ADR-005); do not add Discord-style separate voice channels.
- **Screenshots:** [ben on #general while ana talks in #game-night](browser-ux-audit/F01-voice-elsewhere-not-shown-1280.png), [after leaving](browser-ux-audit/journey-09-left-voice-1440.png).
- **Existing issues:** none found (searched "voice presence channel list", "join voice"). Related: #536 (decided against drawing voice rooms in the list), #442.

### F02 — For someone not in voice, the roster is a one-time read: it misses arrivals and keeps people who left

- **Severity / kind:** P1, defect. **Viewports:** all.
- **Steps:** ben opens `#game-night` (not in voice). ana joins voice there. Wait 30 s. Then ben switches to `#general` and back. Then ana leaves; wait 10 s.
- **What happens:** for 30 s ben's strip shows nobody in voice. After switching away and back it shows "ana". Ten seconds after ana left, it still shows "ana". (The client reads presence once when the channel is shown and subscribes only while joined: `useVoice.ts`.)
- **Should:** UX-23 "no fake participants"; the issue: "Check microphone and transmission indicators are truthful."
- **Impact:** the only voice information a not-yet-joined tester sees is wrong in both directions.
- **Recommendation:** keep the shown channel's presence live while not joined (the voice socket or the events socket), or poll it. *Guess:* the server's voice socket may already serve a member who has not joined; check before designing.
- **Screenshot:** [30 s after ana joined](browser-ux-audit/F02-stale-roster-30s-after-join.png).
- **Existing issues:** none found.

### F03 — The voice roster clips at 1280 px wide: three of five people are hidden, with no cue

- **Severity / kind:** P2, defect. **Viewports:** 1280×720 (1440×900 fits five).
- **Steps:** five people join voice in `#game-night`; look at the strip at 1280×720.
- **What happens:** the roster is a 207 px box with `overflow-x: auto` holding 362 px of chips: "op", "ana" and half of "You" show; cy and dee are cut off. No scroll bar, count or "+N" is drawn. Already with three people ben's view at 1280 hides cy.
- **Should:** UX-09 "Show listeners"; #536's acceptance: larger rosters covered.
- **Impact:** a tester cannot tell who is listening before speaking.
- **Recommendation:** let the roster wrap onto its own row, or show the first chips plus "+N" that opens the full list; always say "N in voice".
- **Screenshots:** [1280](browser-ux-audit/F03-roster-clipped-1280.png), [1440](browser-ux-audit/F03-roster-1440.png), [three people at 1280](browser-ux-audit/journey-08-sharing-1280.png).
- **Existing issues:** #536 (closed) covered "larger rosters" at 1440; not this width.

### F04 — "Connected in #game-night" is text, not a way back

- **Severity / kind:** P2, confusing flow. **Viewports:** all.
- **Steps:** join voice in `#game-night`, open `#book-club`, look for a way back to the voice channel from the strip.
- **What happens:** the strip has no link or button for the connected channel; you find it again in the channel list (on a phone, a sideways-scrolling row where it may be off screen).
- **Should:** the issue's "persistent connected-voice status" with "a way back" (Discord's voice panel opens its channel).
- **Recommendation:** make the channel name in the strip a button that opens it; on a phone also scroll the channel row to it.
- **Screenshot:** [browsing while in voice](browser-ux-audit/F04-browsing-while-in-voice-1440.png).
- **Existing issues:** none found.

### F05 — People in voice show as "Member 4" when the channel on screen is not one they are in

- **Severity / kind:** P2, defect. **Viewports:** all.
- **Steps:** ana and cy join voice in `#game-night`; ana opens `#book-club` (cy is not a member).
- **What happens:** ana's strip lists "You" and "Member 4"; names come from the members of the channel on screen, not of the voice channel.
- **Recommendation:** resolve roster names from the joined channel's members (or the principals list).
- **Screenshot:** [roster](browser-ux-audit/F05-member-n-in-roster.png).
- **Existing issues:** none found.

### F06 — After leaving voice during an outage, "Voice status could not be read" stays after the server is back

- **Severity / kind:** P2, defect. **Viewports:** 1440.
- **Steps:** join voice in `#game-night`; stop conchd; after 8 s press Leave voice; start conchd; wait 30 s.
- **What happens:** the status says Connected, but the strip still says "Voice status could not be read." and Join voice and Watch only are disabled. Opening another channel and coming back clears it. (When not in voice during the outage, it recovers: checked separately.)
- **Recommendation:** re-read presence when the connection returns, and retry a failed read.
- **Screenshot:** [30 s after restart](browser-ux-audit/F06-voice-status-stuck-after-restart.png).
- **Existing issues:** none found.

### F07 — A person whose invite has no voice is told "Voice status could not be read"

- **Severity / kind:** P2, confusing flow. **Viewports:** 1440.
- **Steps:** redeem an invite made without Voice (`kit`); open `#game-night`.
- **What happens:** the strip shows Join voice and Watch only, both disabled, and "Voice status could not be read." The browser logs 403 for `GET /v1/channels/{c}/voice`.
- **Should:** UX-23 "voice action explains unavailability"; the landing page told this person "Voice: Not included", so the lobby should say the same.
- **Recommendation:** treat 403 as "Your account does not include voice. Ask whoever invited you." and hide the voice buttons or keep them disabled with that reason.
- **Screenshot:** [strip](browser-ux-audit/F07-no-voice-permission.png).
- **Existing issues:** none found; related #440.

### F08 — On a phone, joining voice pushes the newest messages out of view, and the strip takes a third of the screen

- **Severity / kind:** P2, defect / visual. **Viewports:** 390×844.
- **Steps:** on a phone, open `#general` (scrolled to the newest message), press Join voice.
- **What happens:** the message log shrinks from 451 to 252 px and is left 199 px above its end: the newest message is below the fold. In voice the strip is about 300 of 844 px, five lines of it the share-sound note.
- **Should:** UX-07 new messages and the latest stay reachable; UX-22 keep chat readable on small surfaces.
- **Recommendation:** keep the log pinned to its end when its height changes; on narrow screens move the share notes into Voice options (the #536 note lists this as "left for later").
- **Screenshots:** [after join](browser-ux-audit/F08-phone-after-join.png), [in voice](browser-ux-audit/F08-phone-in-voice.png).
- **Existing issues:** #467 (closed) fixed width, not this; #536 §6 "left for later".

### F09 — "Hold to talk" cannot be operated from the keyboard

- **Severity / kind:** P2, defect (accessibility). **Viewports:** 1440.
- **Steps:** join voice with the keyboard (Enter on Join voice moves focus to Hold to talk); hold Space, then hold Enter.
- **What happens:** neither transmits; the button reacts only to pointer events. The talk key (backtick) does work while focus is on another strip button. With focus in the composer the backtick types a character and does not transmit, which the hint ("works only while this tab has focus") does not say.
- **Should:** UX-22 "controls need … keyboard operation".
- **Recommendation:** Space or Enter held on the focused button presses, release releases; the hint adds "and not while typing".
- **Existing issues:** none found.

### F10 — No choice of microphone or speakers, and no volume per person

- **Severity / kind:** P2, not yet implemented. **Viewports:** all.
- **What happens:** Settings has name, sign-ins, sign out, report and version; Voice options has push to talk / open microphone, the talk key and the open-microphone level. The client never lists devices; the browser's default microphone and output are used.
- **Should:** UX-11 "Microphone selection shows the actual device and missing-device state".
- **Impact:** a tester with a headset and a webcam microphone cannot pick one; nobody can turn one loud person down.
- **Screenshot:** [Settings](browser-ux-audit/F10-settings-no-audio-devices.png).
- **Existing issues:** none for the browser (#185/#184 are the native client).

### F11 — Others cannot see who is muted or deafened

- **Severity / kind:** P3, not yet implemented. **Viewports:** all.
- **Steps:** ana mutes, holds to talk; then deafens. Look at ben's strip.
- **What happens:** ana's own strip says "Muted." / "Deafened (you hear no one, and you are muted)."; ben's shows nothing about it. #536 chose this ("Presence says who transmits, not who is muted"); the voice-lounge mockup and Discord both show it.
- **Recommendation:** a later decision; it needs a presence field (a `pkg/schema` change, so the schema-change procedure).
- **Existing issues:** none found.

### F12 — The strip's notes are long and always there; the strip reflows when its status line grows

- **Severity / kind:** P3, visual inconsistency. **Viewports:** 1440, 1280, phone.
- **What happens:** in voice, two or three lines about the talk key and share sound sit under the controls on desktop, five on a phone, whether or not anyone means to share. Deafening lengthens the status text and pushes all the buttons down to a second row, so the controls move under the pointer.
- **Recommendation:** move the share notes behind Share screen (shown before the picker opens) or Voice options; give the status a fixed width or wrap it, not the controls.
- **Screenshots:** [joined](browser-ux-audit/journey-05-joined-voice-1440.png), [deafened](browser-ux-audit/F12-F13-deafened-strip-reflow.png).
- **Existing issues:** #536 §6 lists the phone part as left for later.

### F13 — Deafened says "you are muted", but Mute shows unpressed

- **Severity / kind:** P3, visual inconsistency. **Viewports:** 1440.
- **Steps:** in voice, press Deafen.
- **What happens:** "Deafened (you hear no one, and you are muted)", while the Mute button still reads "Mute" with `aria-pressed=false`.
- **Recommendation:** show Mute as pressed (and disabled, "muted by deafen") while deafened.
- **Screenshot:** [deafened](browser-ux-audit/F12-F13-deafened-strip-reflow.png).

### F14 — Share screen stays enabled while someone else shares; the refusal note outlives the share

- **Severity / kind:** P3, confusing flow. **Viewports:** 1440.
- **Steps:** ben shares; ana presses Share screen; ben stops.
- **What happens:** the press is refused with "ben is sharing. One person shares at a time." (clear). After ben stopped, ana's strip still said "Someone else is sharing. One person shares at a time." (seen while conchd was later stopped).
- **Recommendation:** disable Share screen while another share is live, with that sentence as its reason; clear it when the share ends.
- **Screenshot:** [refused](browser-ux-audit/F14-share-refused.png).
- **Existing issues:** #443, #444 (closed) built the rule.

### F15 — A viewer is not told that the share they were watching ended

- **Severity / kind:** P3, confusing flow. **Viewports:** 1440, phone.
- **Steps:** ana watches ben's share; ben presses Stop sharing.
- **What happens:** the stage disappears; nothing says why (the sharer gets "You stopped sharing.").
- **Should:** UX-16 "Stopping the share returns to the voice lounge"; a one-line note keeps it from looking like a crash.
- **Screenshot:** [after the share ended](browser-ux-audit/F15-share-ended-viewer.png).
- **Existing issues:** #487 (closed) is the sharer's side.

### F16 — An empty channel is a blank pane

- **Severity / kind:** P3, defect against UX-23. **Viewports:** 1440.
- **Steps:** open `#announcements`, which has no messages.
- **What happens:** the conversation area is empty; no text.
- **Should:** UX-23 "Empty channel/voice room: invite a message or offer explicit Join".
- **Recommendation:** "No messages in #announcements yet. Say hello." in the log.
- **Screenshot:** [empty](browser-ux-audit/F16-empty-channel.png).

### F17 — While the server is unreachable the composer is disabled without saying so; Join voice stays enabled

- **Severity / kind:** P3, confusing flow. **Viewports:** 1280, phone.
- **Steps:** with a draft typed, stop conchd and wait.
- **What happens:** "Reconnecting" in the top bar and "This conversation may be out of date" over the log (good), but the textarea keeps full opacity and colours and cannot be typed in, with no reason given; only Send dims. Join voice and Watch only stay enabled while conchd is down. The draft is kept and sends once the server is back (good).
- **Should:** UX-05 "disable new sends until reconnected" (done) and explain it.
- **Recommendation:** a disabled look for the composer and a line "Sending is paused until the server is back"; disable Join voice with the same reason.
- **Screenshots:** [1280](browser-ux-audit/F17-server-down-1280.png), [phone](browser-ux-audit/F17-server-down-phone.png).

### F18 — Invite panel: the personal link is described as open to anyone; the list does not follow use; focus is lost after Done

- **Severity / kind:** P3, defect. **Viewports:** 1440.
- **Steps:** as ana, Invite someone → tick `#general` → Make invite → Done → press Escape. Redeem the invite elsewhere; watch the list.
- **What happens:** (1) the new-link box says "Anyone with this link can join until it expires or you close it" for a personal, single-use invite. (2) After Done, focus is on the page body and Escape no longer closes the panel. (3) Five seconds after the invite was used the list still says "Open · not used yet" with a Close button, until Refresh.
- **Recommendation:** wording by kind ("The first person to open this link joins; it then stops working"); focus to Make invite or the list after Done; refresh the list on open and after a redemption event, or say when it was read.
- **Screenshot:** [new link, masked](browser-ux-audit/F18-invite-link-made-masked.png).
- **Existing issues:** #440 (closed) built the panel; #477 is about its tests.

### F19 — A dead invite page repeats itself and its only exit is a sign-in form a new person cannot use

- **Severity / kind:** P3, confusing flow. **Viewports:** 1440, phone.
- **Steps:** open a used, closed, expired or malformed invite link.
- **What happens:** heading and alert both say "This invite no longer works" (the alert adds "Ask the person who sent it for a new one."). Go back opens the credential-paste sign-in, which a person from an invite never had. One message for every cause is deliberate (no oracle) and should stay.
- **Recommendation:** a different heading ("This link can't be used"); replace Go back with nothing or with "If you already joined from this link, ask for a sign-in link" (#493).
- **Screenshot:** [page](browser-ux-audit/F19-invite-no-longer-works.png).
- **Existing issues:** #493 (closed, sign-in links).

### F20 — Focus is dropped after signing in and after Leave voice

- **Severity / kind:** P3, defect (accessibility). **Viewports:** 1440.
- **Steps:** sign in with the keyboard; later, Enter on Leave voice.
- **What happens:** `document.activeElement` is the body both times; the next Tab starts from the top of the page.
- **Recommendation:** after sign-in, focus the channel heading or the composer; after Leave voice, Join voice.

### F21 — No "speaking" mark for a push-to-talk talker

- **Severity / kind:** P3, defect (to confirm with a real microphone). **Viewports:** 1440.
- **Steps:** ana and ben in voice; ana holds the talk key for 6 s; sample ben's roster every 0.5 s.
- **What happens:** "ana (transmitting)" from 1 s on; "speaking" never appears, although ben received ana's audio (inbound audio energy rose by 0.96 over 3 s while held, 0 while not). With open microphone, "speaking" did appear for ana.
- **Caveat:** the fake microphone is a tone; LiveKit's speaker detection may treat it differently from speech. A person with a microphone should check before this is worked on.
- **Screenshot:** [ben's view](browser-ux-audit/F21-ptt-transmitting-no-speaking.png).

### F22 — Members column: an unexplained "(2)", no online grouping, and agent/run sections for everyone

- **Severity / kind:** P3, visual inconsistency. **Viewports:** 1440, 1280.
- **What happens:** "ana (2) online" when ana has two live sessions (for example a reload, whose old session has not timed out); the number has no label or accessible text. People are listed in join order with offline among online. Every tester sees "Agents · No agents in this channel · Runs · Filter runs [all] · No runs in this channel".
- **Recommendation:** "(2 devices)" or a tooltip; Online then Offline groups with counts (as the lobby mockup's "Online — 6"); hide Agents and Runs when the channel has none.
- **Screenshot:** ["(2)" in the column](browser-ux-audit/F18-invite-link-made-masked.png) (right edge), [lobby](browser-ux-audit/baseline-lobby-1440.png).

### F23 — Phone: the channel row hides channels without a cue, unread pills look like tabs, small targets

- **Severity / kind:** P3, visual inconsistency. **Viewports:** 390×844.
- **What happens:** the row shows two or three channels; the rest are off to the right with no fade or arrow. The unread pill sits apart from its name and reads as a tab of its own. Members and Settings are 41×44 px; the audience select is 27 px tall (WCAG 2.2 AA's 24 px is met; the 44 px guideline is not).
- **Screenshots:** [row](browser-ux-audit/F23-phone-channel-row.png), [lobby](browser-ux-audit/baseline-lobby-phone.png).
- **Existing issues:** #467 (closed).

### F24 — The same person on two devices: "Not in voice" beside "You" in the roster

- **Severity / kind:** P3, confusing flow. **Viewports:** phone.
- **Steps:** cy is in voice on a desktop; cy opens `#game-night` on a phone.
- **What happens:** the phone says "Not in voice" and lists "You" among those in voice.
- **Recommendation:** "You (on another device)". UX-08 already says concurrent voice by one principal needs a policy.
- **Screenshot:** [phone](browser-ux-audit/F24-same-person-two-devices-phone.png).

### F25 — Console noise that will show up in testers' bug reports

- **Severity / kind:** P3, defect. **Viewports:** all.
- **What happens:** every transmit report (`POST …/voice/transmit`) gets its 204 and is then logged by Chromium as failed with `net::ERR_ABORTED` (two per press); starting a share logs "Permissions policy violation: camera is not allowed in this document."; the invite landing page logs a 404 or 409 as a console error for an expected refusal.
- **Impact:** a tester (or the report-a-problem diagnostics) sees red errors on a working client.
- **Recommendation:** find why the transmit request is aborted after its answer (*guess:* the report's AbortController, or an unread body); ask for the screen without touching the camera policy.

### F26 — "Watch only" does not say whether you will hear people

- **Severity / kind:** P3, confusing flow. **Viewports:** all.
- **What happens:** before joining, the second button is "Watch only"; after it, "Watch only: your microphone is off." with "Use microphone". Nothing says whether you hear the room.
- **Recommendation:** "Listen and watch (microphone off)" or a hint under the button.

### F27 — A denied microphone reads "Voice unavailable … trying again"

- **Severity / kind:** P2, defect. **Viewports:** 1440.
- **Steps:** Chromium without the fake-UI flag, the microphone permission refused (`navigator.permissions` reports `denied`); press Join voice.
- **What happens:** "Voice unavailable in #game-night · Not transmitting · Voice is unavailable; trying again", with Hold to talk, Mute and Deafen enabled. Nothing mentions the microphone or the browser's permission.
- **Should:** UX-09 "Distinguish joining, joined, permission denied, unavailable and failed"; UX-23 "Microphone denied/missing: identify problem, offer device/settings action".
- **Recommendation:** detect `NotAllowedError`/`NotFoundError` and say "Your browser blocked the microphone. Allow it in the address bar, or use Watch only"; stop retrying until the person acts.
- **Screenshot:** [strip](browser-ux-audit/F27-mic-denied.png).

## Quick fixes and larger proposals

**Quick fixes** (client only, no API or schema change): F01 (name the destination), F03, F04, F05, F06, F07, F08, F09, F12, F13, F14, F15, F16, F17, F18, F19, F20, F22, F23, F25, F26, F27.

**Larger proposals:**
- **Voice occupancy across the server (F01, F02).** Channel list marks with counts, a "people are in voice elsewhere" line, live presence for non-joined viewers. May need a presence feed that is not per channel; design with ADR-005's nets in mind so a later net view fits.
- **Audio devices and per-person volume (F10).** Device lists, missing-device state, output choice where the browser allows it, local volume per participant.
- **Others' mute and deafen state (F11).** A presence field, so a `pkg/schema` change.
- **Multi-device voice (F24)** waits for the media-session policy UX-08 names.
- **Speaking detection under push to talk (F21)** after a check with a real microphone.

## Acceptance criteria of #577

| Criterion | Result | Evidence |
| --- | --- | --- |
| Reviewer has interacted with the actual running browser UI | **Pass** | Playwright drove `/app/` of a running conchd built at `3d63ce2`, with up to five people at once; every finding has steps and most a screenshot. |
| Desktop and narrow/mobile viewports covered, with screenshots | **Pass** | 1440×900, 1280×720, 390×844 (touch), 720×450; 39 screenshots in `browser-ux-audit/`. |
| Joining a server never implicitly joins voice or starts microphone capture | **Pass** | For ana, ben, cy (sign-in), dee (desktop invite) and eve (phone open link): no `getUserMedia` call, no LiveKit participant, empty conchd presence, strip "Not in voice · Microphone off"; also after browsing three channels. |
| Browsing text channels does not alter the explicitly joined voice channel | **Pass** | While ana read `#general` and `#book-club`, conchd presence kept her in `#game-night`, the strip said "Connected in #game-night", and a watched share kept playing. |
| Joining and leaving voice are discoverable and give clear status and feedback | **Fail (in part)** | Leaving is clear (red Leave voice, capture ends, status returns). Joining is discoverable only for the channel on screen and the button does not name it (F01); the pre-join roster is stale (F02) or clipped (F03); refusals give the wrong reason (F07, F27, F06). |
| At least one complete realistic user journey, including failure and empty states | **Pass** | Invite → name taken → join → first message → voice → share → leave; dead invites, empty channel, conchd stopped and started, refused share, no-voice account, denied microphone. |
| Findings reproducible, severity-ranked and mapped to existing or new issues | **Pass, pending filing** | Steps and severity per finding; existing issues named; new ones written below for the principal to file. |
| No secrets leaked, no production deployment, no unapproved code changes | **Pass** | Own conchd on loopback with its own data; credentials and invite codes held in memory or a 0600 scratch file, never printed; a check found no token or code in any page's DOM or address; the invite-link screenshot is masked; every screenshot was looked at. Only this file and its screenshots were added. |
| Independent review delivered for the owner's approval before UX changes are accepted | **Pending** | This document is the delivery; approval is the owner's. |

## How to reproduce

Build with `make build` and `make web-build`; run `bin/conchd bootstrap-operator --data <dir> --name op`, then `bin/conchd serve --data <dir> --listen 127.0.0.1:<port> --web-dir web/dist --livekit-url ws://127.0.0.1:<lk-port> --livekit-room-prefix <prefix>` with `CONCHD_LIVEKIT_API_KEY` and `CONCHD_LIVEKIT_API_SECRET` set (or `scripts/dev-stack.sh up`). Make channels and people with `conch admin channel create`, `conch admin member add` and invites, as in Demo data. Open `/app/` in Chromium; for a second person use a second profile or a private window. The steps under each finding are written for that setup. The audit's own scripts lived in a scratch directory and are not part of the repository.

## Proposed issues

Not filed: the principal files them. Each was checked for duplicates with `gh issue list --state all --search …` (queries: "voice presence channel list", "join voice", "roster", "microphone device", "push to talk keyboard", "empty channel", "invite list", "voice status", "phone voice", "reconnect voice", "deafen", "member name voice", "focus", "transmit report", "share screen refused", "web layout narrow", "sessions members", "sign out operator link"). None duplicates a finding; related closed issues are named in each.

### 1. Web voice: name the room Join voice joins, and make the connected channel a way back (F01 quick part, F04)

**Context.** The strip's Join voice and Watch only join the channel on screen, but their names do not say which; once in voice, "Connected in #x" is plain text. UX-04/UX-09 in `docs/design/client-experience.md`; audit #577 F01, F04.
**Files.** `web/src/voice/VoicePanel.tsx`, `web/src/Lobby.tsx` (channel selection), `web/src/ChannelList.tsx` (scroll into view on a phone), `web/src/voice/*.test.tsx`, `web/e2e/10-voice.spec.ts`.
**Acceptance criteria.**
- [ ] Not in voice, the buttons read "Join voice in #<channel>" and "Watch only in #<channel>" (visible text or accessible name; on a phone the visible text may be shorter, the accessible name is full).
- [ ] In voice, the connected channel's name in the strip is a button; it opens that channel and, below 700 px, scrolls the channel row to it.
- [ ] Nothing about joining or the voice lifecycle changes.
**Required tests.** Component tests for both names and for the button opening the channel; the voice e2e asserts the accessible names.
**Blocked by.** Nothing.

### 2. Web voice: keep the shown channel's voice roster live when not joined (F02)

**Context.** Not in voice, the strip reads presence once when a channel is shown (`useVoice.ts`); it misses people who join and keeps people who leave until the channel is reopened. UX-23 "no fake participants". Audit #577 F02.
**Files.** `web/src/voice/useVoice.ts`, `web/src/api.ts`; possibly `internal/server/voice_ws.go` if the voice socket does not serve a member who has not joined (check first).
**Acceptance criteria.**
- [ ] With the strip showing a channel and the viewer not in voice, someone joining or leaving that room appears or disappears within 3 s.
- [ ] A failed read is retried and is retried again when the connection comes back (covers F06).
- [ ] No microphone, LiveKit connection or join happens to make this work.
**Required tests.** Unit test of the subscription lifecycle; e2e: two people, one watching the strip while the other joins and leaves.
**Blocked by.** Nothing.

### 3. Web voice: say the real reason voice cannot start (F06, F07, F27)

**Context.** A 403 on voice presence (account without voice) shows "Voice status could not be read."; a denied microphone shows "Voice unavailable … trying again" and keeps retrying; after leaving voice during an outage the "could not be read" state survives the reconnect. UX-09, UX-23. Audit #577 F06, F07, F27.
**Files.** `web/src/voice/useVoice.ts`, `web/src/voice/connection.ts`, `web/src/voice/sdk.ts` (the capture error), `web/src/voice/VoicePanel.tsx`, tests beside them.
**Acceptance criteria.**
- [ ] A person whose grants have no voice sees "Your account does not include voice" and no enabled voice button.
- [ ] A refused or missing microphone says so, names the fix (browser permission, or Watch only), and does not retry by itself.
- [ ] After a reconnect the presence is read again; Join voice is enabled again without changing channel.
**Required tests.** Component tests per state; e2e with the microphone permission refused (Chromium without the fake-UI flag) and with an invite without voice.
**Blocked by.** Nothing.

### 4. Web voice: the roster never hides people (F03)

**Context.** At 1280×720 the roster is a 207 px scroller with no visible cue; with five in voice three are hidden. Audit #577 F03.
**Files.** `web/src/app.css`, `web/src/voice/VoicePanel.tsx`, `web/e2e/16-layout.spec.ts`.
**Acceptance criteria.**
- [ ] At 1280×720, 1440×900, 768×1024 and 390×844 with eight people in voice, each name is visible or a visible "+N" control lists the rest; the strip says "N in voice".
- [ ] The strip's controls do not move when the roster grows.
**Required tests.** Layout e2e with eight fake participants at the four sizes.
**Blocked by.** Nothing.

### 5. Web voice: roster names come from the voice channel, not the channel on screen (F05)

**Context.** Viewing a channel the others are not in, the roster says "Member 4". Audit #577 F05.
**Files.** `web/src/VoiceStatus.tsx`, `web/src/Lobby.tsx` (where `people` comes from), `web/src/voice/VoicePanel.tsx`.
**Acceptance criteria.**
- [ ] In voice in `#a` and viewing `#b`, every person in the roster shows their name.
**Required tests.** Component test; e2e step in the voice spec.
**Blocked by.** Nothing.

### 6. Web: phone layout in voice keeps the newest message in view (F08, phone part of F12)

**Context.** At 390×844, joining voice leaves the log 199 px above its end and the strip takes about 300 px, five lines of which are share notes. UX-07, UX-22; #536 §6 "left for later". Audit #577 F08.
**Files.** `web/src/Conversation.tsx` (scroll anchoring on resize), `web/src/voice/VoicePanel.tsx`, `web/src/app.css`, `web/e2e/12-phone.spec.ts`.
**Acceptance criteria.**
- [ ] A log at its end stays at its end when the strip changes height (join, leave, notes appear).
- [ ] At 390×844 in voice, nobody sharing, the strip is at most 200 px; the share notes appear when the person goes to share, or in Voice options.
**Required tests.** Phone e2e: scrolled to the end, join voice, assert the last message is in view; strip height bound.
**Blocked by.** Nothing.

### 7. Web: keyboard push-to-talk on the button, and focus after sign-in and Leave voice (F09, F20)

**Context.** Hold to talk responds only to pointer events; after sign-in and after Leave voice focus falls to the body. UX-21, UX-22. Audit #577 F09, F20.
**Files.** `web/src/voice/VoicePanel.tsx`, `web/src/App.tsx` (focus after sign-in).
**Acceptance criteria.**
- [ ] Space or Enter held on the focused Hold to talk transmits; key up stops; losing focus stops.
- [ ] The talk-key hint also says it does not work while typing.
- [ ] After sign-in focus is on the channel heading (or the composer); after Leave voice on Join voice.
**Required tests.** Component tests for key down/up and blur; e2e keyboard-only join, talk, leave.
**Blocked by.** Nothing.

### 8. Web voice: strip feedback (F13, F14, F15, F26, desktop part of F12)

**Context.** Deafened says "you are muted" while Mute is unpressed; Share screen stays enabled during another's share and its refusal note outlives it; a viewer is not told a share ended; "Watch only" does not say you will hear; the share notes always take two or three lines and a longer status pushes the controls to a new row. Audit #577.
**Files.** `web/src/voice/VoicePanel.tsx`, `web/src/voice/share.ts`, `web/src/voice/ShareView.tsx`, `web/src/app.css`.
**Acceptance criteria.**
- [ ] While deafened, Mute is shown pressed and disabled.
- [ ] While another person shares, Share screen is disabled with "<name> is sharing. One person shares at a time."; the note goes when the share ends.
- [ ] A viewer whose share ended sees "<name> stopped sharing" for a few seconds.
- [ ] The second join button says the microphone is off and that the room is heard.
- [ ] Status text changes do not move the controls.
**Required tests.** Component tests for each state.
**Blocked by.** Nothing.

### 9. Web: empty channel and offline composer states (F16, F17)

**Context.** An empty channel is blank; while reconnecting the composer is disabled without looking disabled or saying why, and Join voice stays enabled. UX-05, UX-23. Audit #577.
**Files.** `web/src/Conversation.tsx`, `web/src/Composer.tsx`, `web/src/voice/VoicePanel.tsx`, `web/src/app.css`.
**Acceptance criteria.**
- [ ] A channel with no messages says so and invites the first one.
- [ ] While not connected the composer looks disabled and says sending is paused; the draft is kept (as now).
- [ ] Join voice and Watch only are disabled with a reason while the server is unreachable.
**Required tests.** Component tests; extend `web/e2e/09-restart.spec.ts` with the composer state.
**Blocked by.** Nothing.

### 10. Web invites: wording by kind, a list that follows use, focus after Done; the dead-invite page (F18, F19)

**Context.** Audit #577 F18, F19; built in #440, sign-in links in #493.
**Files.** `web/src/InvitePanel.tsx`, `web/src/Invite.tsx`, `web/src/invite.test.tsx`, `web/e2e/11-invites.spec.ts`.
**Acceptance criteria.**
- [ ] A personal invite's box says the first person to use it joins and it then stops working; an open link's says what it says now.
- [ ] After Done, focus is inside the panel and Escape closes it.
- [ ] Opening the panel reads the list again; the list shows when it was read.
- [ ] The dead-invite page's heading and message differ; it does not send a new person to the credential form, and mentions sign-in links for someone who already joined. The single message for every cause stays.
**Required tests.** Component tests; e2e: make, use elsewhere, reopen, see Used.
**Blocked by.** Nothing.

### 11. Web: members column and phone channel row polish (F22, F23, F24)

**Context.** Unlabelled "(2)"; no Online/Offline grouping; Agents and Runs shown when empty; phone channel row without an overflow cue, unread pill apart from its name, 41 px and 27 px targets; "Not in voice" beside "You". Audit #577.
**Files.** `web/src/agents/MembersPanel.tsx`, `web/src/ChannelList.tsx`, `web/src/voice/VoicePanel.tsx`, `web/src/app.css`.
**Acceptance criteria.**
- [ ] The session count reads "(2 devices)" or has an accessible description.
- [ ] People are grouped Online / Offline with counts.
- [ ] Agents and Runs are hidden when the channel has neither.
- [ ] On a phone the channel row shows that more channels exist, and each unread pill is inside its channel's button.
- [ ] Touch targets on the phone are at least 44×44 px.
- [ ] The same person in voice on another device is "You (another device)".
**Required tests.** Component tests; phone e2e for target sizes.
**Blocked by.** Nothing.

### 12. Web voice: quiet the console (F25)

**Context.** Each transmit report is answered 204 and then shown as `net::ERR_ABORTED`; sharing logs a camera Permissions-Policy violation; expected invite refusals log as console errors. Testers' reports will carry these. Audit #577 F25.
**Files.** `web/src/voice/reports.ts`, `web/src/api.ts`, `web/src/voice/share.ts`, `web/src/Invite.tsx`.
**Acceptance criteria.**
- [ ] A press and release in voice logs no failed request.
- [ ] Starting a share logs no Permissions-Policy error.
**Required tests.** An e2e step that fails on console errors and failed requests during join, talk, share and leave.
**Blocked by.** Nothing.

### 13. Web voice: show where people are in voice across the server (F01 larger part) — proposal

**Context.** Only the channel on screen shows voice presence, so testers meet in different rooms. The issue's Discord benchmark asks for visible voice join affordances. Conch's voice belongs to a channel (ADR-005); this keeps that. Needs a design note first: a server-wide presence read or feed (the per-channel `GET /v1/channels/{c}/voice` exists; reading it for every channel does not scale to a live view). Audit #577 F01.
**Files.** A design note under `docs/design/`; then `web/src/ChannelList.tsx`, `web/src/voice/*`; possibly `internal/server/voice_presence.go` and `pkg/schema` (schema-change procedure).
**Acceptance criteria.**
- [ ] Each channel in the list shows whether its voice room has people and how many, kept live, only for channels the viewer is a member of.
- [ ] Not in voice, the strip says where people are ("3 in voice in #game-night") with a control that opens that channel.
- [ ] Seeing this never joins voice, opens a microphone or delivers audio.
**Required tests.** e2e: three people, voice in one channel, the others see it from other channels; isolation check that a non-member sees nothing (`e2e/isolation`).
**Blocked by.** #2 above (live presence), the design note.

### 14. Web voice: microphone and speaker choice, per-person volume — proposal (F10)

**Context.** The browser default devices are always used; UX-11 asks for the actual device and a missing-device state. Audit #577 F10.
**Files.** `web/src/voice/sdk.ts`, `web/src/voice/connection.ts`, `web/src/voice/VoicePanel.tsx` (Voice options).
**Acceptance criteria.**
- [ ] Voice options lists microphones and (where the browser allows) outputs, shows the one in use, and survives a device being unplugged without silently switching.
- [ ] Each person in the roster has a local volume.
**Required tests.** Unit tests with fake device lists; e2e with Chromium's fake devices.
**Blocked by.** Nothing; owner's priority call.

### 15. Voice presence: show others' mute and deafen — proposal (F11)

**Context.** Others see "transmitting" but not muted or deafened; #536 left it out on purpose. Needs a presence field. Audit #577 F11.
**Files.** `pkg/schema` (voice presence; schema-change skill), `internal/server/voice_presence.go`, `web/src/voice/*`, TUI equivalents.
**Acceptance criteria.**
- [ ] A person's mute and deafen show to others in the roster, with words, within 2 s.
**Required tests.** Schema fixtures; server tests; e2e.
**Blocked by.** The owner's decision.

### 16. Check the "speaking" mark under push to talk with a real microphone (F21)

**Context.** With Chromium's fake tone, a push-to-talk talker shows "transmitting" but never "speaking", though audio arrives; open microphone does show "speaking". Audit #577 F21. A by-hand check, then a fix if it reproduces.
**Files.** `web/src/voice/sdk.ts`, `web/src/voice/connection.ts`.
**Acceptance criteria.**
- [ ] With a real microphone, someone holding the talk key and speaking shows "speaking" to the others; result recorded on the issue.
**Required tests.** If a fix is needed, an e2e with a fake audio file of speech.
**Blocked by.** A person with a microphone (#474 is the by-hand browser pass).
