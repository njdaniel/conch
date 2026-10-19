# Browser client UX audit 2

**Issue:** #626. **Date:** 2026-10-19. **Status:** independent review, for the project owner's approval; nothing here is accepted until then.
**Reviewer:** an AI agent that did not build the client, driving the running web client in Chromium through Playwright.
**References:** the [first audit](browser-ux-audit.md) (#577, F01–F27), [browser UI polish](../design/browser-ui-polish.md) (#536), [voice occupancy](../design/voice-occupancy.md) (#596), [client experience](../design/client-experience.md) (UX-01 to UX-23), [private alpha](../design/private-alpha.md), [ADR-007](../adr/ADR-007-client-interfaces.md), [ADR-008](../adr/ADR-008-private-communications-alpha.md).

Every verdict and finding below comes from using the running client, with the steps that show it. Guesses are marked as guesses. No product code was changed.

## Summary

The UX pool did what the first audit asked. Of the 27 findings, **23 are fixed**, **1 is partly fixed** (F08), **2 are not fixed** because they are open proposals waiting on the owner (F10 → #597, F11 → #598), and **1 is closed without a product change by decision** (F21: #599 traced it to Chromium's default fake microphone). Nothing regressed.

The two P1s are gone. A tester who is not in voice now sees where people are talking from any channel: a speaker mark and a count beside the channel's name, and a line in the strip, "1 in voice in #game-night · Open #game-night", within about 1.5 s of someone joining. The roster of the channel on screen is live (arrivals in 0.4 s, departures in 0.6 s). Join voice names its room, and "Connected in #game-night" is a way back. The wrong reasons are gone: no voice in the account, a blocked microphone and a server outage each say what they are, and the strip recovers on its own after a restart.

The rules #577 checked still hold: signing in, entering a channel, browsing and opening a channel from the occupancy line never open the microphone or join voice; browsing keeps voice where it is and a watched share keeps playing; leaving voice stops capture and LiveKit drops the participant.

What a tester will still trip over is mostly **the phone in a busy room**. With four or more people in voice the capped roster stacks its chips in a column, and with a share on the strip reaches 324 of 844 px; a phone watching a share with five in voice is left a 48 px message log, about one line (N01, P2). The rest are P3: share notes that say the wrong thing at the wrong time (N02, N03), controls that look usable when they are not (N07, N09), a joined strip that says "Connected" with an empty roster while conchd reports voice unavailable (N06), and two console lines testers will paste into reports (N04, N05).

New findings: **0 × P0, 0 × P1, 1 × P2, 8 × P3** (9 in all).

## Environment and commit

| | |
| --- | --- |
| Commit | `a5ab25f` (main, "Merge pull request #640", after the stability wave); the server and the web client both report version `a5ab25f` |
| Build | `make build`; `make web-build` with Node 24.21.0 (`npm ci`) |
| Server | the audit's own `conchd serve` on `127.0.0.1:28626`, a fresh data directory, `--web-dir web/dist`, `--test-relax-invite-limits` (loopback only; every person redeems from one address), LiveKit at `ws://127.0.0.1:27880` with `--livekit-room-prefix audit626-` (the brief's prefix; the issue text says `audit2-`) |
| LiveKit | the running local container (`livekit/livekit-server:v1.13.7`), not restarted or reconfigured; the voice-down state used a TCP relay of the audit's own in front of it (see How to reproduce) |
| Browser | Chromium 149.0.7827.55, headless shell, driven by `@playwright/test` 1.61.0 (the repository's copy); `livekit-client` 2.22.3 in the bundle. One check (F27 with a real permission refusal) used the same Chromium in its new headless mode, which honours a denied permission; the headless shell cannot show or refuse a prompt |
| OS | Linux 7.0.11 |
| Viewports | 1440×900 and 1280×720 (desktop); 768×1024 for the roster; 390×844 phone (`isMobile`, `hasTouch`, scale 2); 720×450 as 200% zoom of 1440×900 |
| People at once | two to eight browser contexts, one per person |

**Demo data.** Made through the API and `conch admin`: an operator `op` from `conchd bootstrap-operator`; channels `#general`, `#random`, `#game-night` (used for voice), `#book-club` (members op, ana and ben only: Conch has no public/private flag, so this is the "private" one) and `#announcements` (left empty; cy and op are members). `ana` (may invite), `ben` and `cy` joined through personal invites redeemed over the API, and `fay`, `gus`, `hal` and `ivy` likewise to fill the roster; 30 messages of ordinary chat, one long paragraph and one long URL. During the audit `dee` joined from ana's invite link in a desktop browser, `eve` from an operator's open link on a phone, `kit` from an invite without voice, and `jo` from an invite redeemed over the API (and signed out again).

**What fake devices cover.** Chromium ran with `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream --auto-select-desktop-capture-source="Entire screen"`, and with `--use-file-for-fake-audio-capture` set to a steady 440 Hz tone, as the browser suite does; one push-to-talk run used Chromium's default beeping fake microphone, as the first audit did. The fake screen is a 1280×720 test pattern. They prove **the interface's states** only: nothing here says anything about real microphones, headsets, echo, speech detection, background tabs, the real share picker, or other browsers and operating systems (#474 is the owner's by-hand pass).

**Evidence per flow.** An init script wrapped `getUserMedia` and `getDisplayMedia` on every page and recorded each stream and whether its tracks were live; LiveKit's room service was asked for the participants and tracks of the audit's rooms (`audit626-` prefix only); `GET /v1/channels/{c}/voice` and `GET /v1/voice/occupancy` gave conchd's view. Console errors and warnings, page errors, failed requests and 4xx/5xx answers were recorded per flow. Apart from what N04 and N05 describe, the only lines were expected ones: one "Permissions policy violation: camera" per share start (kept by decision on #595), a 409 on a taken name and a 404 on a dead invite's preview (kept by decision on #595), and connection-refused while conchd was stopped.

## The fix table

Verdicts: **Fixed**, **Partly**, **Not fixed**, **Regressed**, **Won't fix by decision**. Each is judged against what its fixing issue decided, where that differs from the first audit's recommendation.

| Finding | Fixed by | Verdict | Note | Screenshot |
| --- | --- | --- | --- | --- |
| F01 Nobody can see where people are talking; Join voice does not name its room | #584, #596 (PR #613 server, web child c) | Fixed | Buttons are named "Join voice in #game-night" / "Watch only in #game-night" (on a phone only the verbs show). Not in voice, the list shows a speaker mark and "1", and the strip "1 in voice in #game-night · Open #game-night", 1.5 s after ana pressed Join (LiveKit connect included). Open opens the channel and does not join | [1280](browser-ux-audit-2/F01-occupancy-elsewhere-1280.png), [phone](browser-ux-audit-2/F01-occupancy-phone.png), [beside an unread count](browser-ux-audit-2/N-occupancy-beside-unread-1440.png) |
| F02 Not-joined roster is a one-time read | #585 | Fixed | ben on #game-night, not joined: cy appeared 0.4 s after her join and went 0.6 s after her leave, without reopening the channel | — |
| F03 Roster clips at 1280 px | #587 | Fixed | Eight in voice: 8 of 8 chips inside the strip at 1440, 1280 and 768; "8 in voice" heads the list and names it. On a phone, three chips and "+5 more" (`aria-expanded` toggles). The phone layout of that cap is N01 | [1280](browser-ux-audit-2/F03-roster-8-1280.png), [phone](browser-ux-audit-2/F03-phone-plus-more.png), [expanded](browser-ux-audit-2/F03-phone-expanded.png) |
| F04 "Connected in #game-night" is not a way back | #584 | Fixed | On #book-club, "#game-night" in the strip is a button; it opened #game-night (`aria-current`) and voice stayed "Connected in #game-night". On the voice channel itself it is text. On a phone (op, five channels) the open channel's button moved from x −362 (off screen) to 12 | [desktop](browser-ux-audit-2/F04-way-back-1440.png), [phone](browser-ux-audit-2/F23-phone-row-way-back.png) |
| F05 "Member 4" in the roster | #588 | Fixed | ana in voice in #game-night, reading #book-club (cy not a member): roster "You, cy" | [roster](browser-ux-audit-2/F05-names-from-voice-channel-1440.png) |
| F06 "Voice status could not be read" stays after an outage | #585 | Fixed | The audit's path (join, stop conchd, Leave after 8 s, start conchd): Join voice enabled 3.8 s after the start, the same moment the top bar said Connected | [after restart](browser-ux-audit-2/F06-recovered-after-restart-1440.png) |
| F07 No-voice account told "could not be read" | #585, #586 | Fixed | kit (invite without voice; landing said "Voice: Not included"): "Your account does not include voice. Ask whoever invited you.", no voice buttons, no occupancy marks, and the page made no request to any voice route | [strip](browser-ux-audit-2/F07-no-voice-1440.png) |
| F08 Phone: joining voice hides the newest message; strip a third of the screen | #589, #600 | Partly | The log stays at its end through a join (0 px from the end before and after). The dock is 194 px with up to three in voice, as #600 asked; with four or more it is 234 px, and 324 px with a share on (N01) | [in voice](browser-ux-audit-2/phone-in-voice.png), [five and a share](browser-ux-audit-2/N-phone-strip-5-sharing.png) |
| F09 Hold to talk not keyboard-operable | #590 | Fixed | Space or Enter held on the focused button: the other side shows "transmitting" from 0.5 s; release stops within 1 s; Tab away while held stops within 1.5 s. The hint says the key does not work while typing; in the composer the backtick types and does not transmit, as designed | [ana's view](browser-ux-audit-2/F09-keyboard-ptt-seen-by-ana.png) |
| F10 No microphone or speaker choice, no per-person volume | #597 (open proposal) | Not fixed | Voice options has push to talk / open microphone, the talk key and the share notes; no device list. The owner's priority call | [Voice options](browser-ux-audit-2/F10-voice-options-no-devices-1440.png) |
| F11 Others cannot see who is muted or deafened | #598 (open proposal) | Not fixed | ana muted: ben's roster is unchanged. Needs a schema field and the owner's decision | — |
| F12 Long notes; strip reflows | #600, #587 | Fixed | Notes one line (19 px) at 1280 with three in voice, with "More about sharing" at its end. Leave voice did not move between one and eight in voice, or between deafened and not | [joined](browser-ux-audit-2/journey-04-joined-voice-1440.png) |
| F13 Deafened says muted; Mute unpressed | #591 | Fixed | Deafened: "Muted by deafen", `aria-pressed="true"`, disabled; Undeafen restores "Mute" | [deafened](browser-ux-audit-2/F13-deafened-1280.png) |
| F14 Share screen enabled during another's share; stale refusal | #591 | Fixed | While ben shares, ana's Share screen is disabled, described "ben is sharing a screen, with sound. One person shares at a time."; enabled again when ben stops. The refusal can no longer be reached. (The sharer's own note does go stale: N02) | [watching](browser-ux-audit-2/share-watching-1440.png) |
| F15 A viewer is not told the share ended | #591 | Fixed | "ben stopped sharing." 0.2 s after ben stopped, gone at 5.3 s; Stop watching shows nothing | [viewer](browser-ux-audit-2/F15-share-ended-viewer-1440.png) |
| F16 Empty channel is blank | #592 | Fixed | "No messages in #announcements yet. Say hello." (also during an outage, under the stale banner) | [phone](browser-ux-audit-2/F16-empty-channel-phone.png) |
| F17 Composer disabled without saying so; Join voice enabled while down | #592, #585 | Fixed | "Sending is paused until the server is back. Your draft is kept." under a dimmed box, as its description; Join voice and Watch only disabled with "Voice status unavailable: the server cannot be reached."; the draft sent afterwards. The disabled Join voice still looks green (N07) | [1280](browser-ux-audit-2/F17-server-down-1280.png), [phone](browser-ux-audit-2/F17-server-down-phone.png) |
| F18 Invite panel wording, focus, list | #593 | Fixed | Personal box: "The first person to open this link joins, and then it stops working." Done → focus on Make invite; Escape closes, focus on Invite someone. "Read at 01:21"; Refresh showed "Used · joined". The re-read on showing the tab worked on a `visibilitychange` event (headless Chromium cannot hide a tab, so the event was dispatched by hand) | [masked](browser-ux-audit-2/F18-invite-link-made-masked.png) |
| F19 Dead invite page repeats itself; exit to an unusable form | #593 | Fixed | Used, malformed and closed links: heading "This link can't be used", one alert; no button with nobody signed in, "Back to Conch" for a signed-in browser. Expired was not tried | [desktop](browser-ux-audit-2/F19-dead-invite-1440.png), [phone](browser-ux-audit-2/F19-dead-invite-phone.png) |
| F20 Focus dropped after sign-in and Leave voice | #590 | Fixed | After sign-in (and after joining from an invite) focus is on the channel heading; after Enter on Leave voice, on "Join voice in #game-night"; Escape in voice does not leave | — |
| F21 No "speaking" under push to talk | #599 (PR #620) | Won't fix by decision | No product fix: #599 found LiveKit's detector ignores Chromium's default fake beep. Re-checked: with the steady tone, held Space shows "transmitting" and "speaking" from 0.5 s; with the default fake microphone, "transmitting" only. A real microphone is #474's | — |
| F22 Members column "(2)", no grouping, empty agent sections | #594 | Fixed | "ana (2 sessions)" with two tabs open, "Online — 3" / "Offline — 5", and "No agents or runs in this channel." | ["(2 sessions)"](browser-ux-audit-2/F18-invite-link-made-masked.png) (right edge), [groups](browser-ux-audit-2/F01-occupancy-elsewhere-1280.png) |
| F23 Phone channel row: no cue, pill apart, small targets | #601 | Fixed | Fades at the row's edges (`more-before`, `more-after`); each unread count sits inside its channel's pill; Invite someone, Members, Settings 44×44, the audience select 44 px tall | [row](browser-ux-audit-2/F23-phone-row-way-back.png), [lobby](browser-ux-audit-2/baseline-lobby-phone.png) |
| F24 "Not in voice" beside "You" on a second device | #591 | Fixed | gus's second tab: "You (on another device)" and "You are in voice on another device. Joining here moves your voice to this device." | [200% zoom](browser-ux-audit-2/zoom200-roster-other-device.png) |
| F25 Console noise | #595, #622 | Fixed | No failed request through join, push to talk, share, stop sharing and leave, or on sign-out. Kept by decision on #595: one camera Permissions-Policy line per share start, and the 404/409 lines on invite refusals. Sign-out's 404 is new (N04) | — |
| F26 "Watch only" does not say you will hear | #584 | Fixed | "Microphone off. You hear the room and can watch a share." under the buttons, linked as Watch only's description | [lobby](browser-ux-audit-2/baseline-lobby-1440.png) |
| F27 Denied microphone reads "trying again" | #586 | Fixed | A real refusal (Chromium with the permission denied) and #586's `NotAllowedError` script both give "Voice stopped" and "Your browser blocked the microphone. Allow it for this site in the address bar, then join again, or use Watch only."; one session request, none in the next 30–40 s; Watch only connects; a refused Use microphone keeps the connection and says why | [script](browser-ux-audit-2/F27-mic-blocked-1440.png), [real refusal](browser-ux-audit-2/N-mic-denied-1440.png) |

**Counts:** Fixed 23, Partly 1, Not fixed 2, Regressed 0, Won't fix by decision 1.

## The rules #577 checked

| Rule | Result | Evidence |
| --- | --- | --- |
| Signing in, entering a channel and browsing never join voice or open the microphone | **Holds** | ana, ben and cy (sign-in), dee (desktop invite) and kit (no-voice invite) after browsing up to four channels: no `getUserMedia` call, no LiveKit participant, the strip "Not in voice · Microphone off". Pressing "Open #game-night" in the occupancy line opened the channel only (no capture, no participant). |
| Browsing keeps voice where it is | **Holds** | ana in voice in #game-night read #general and #book-club: conchd's presence kept her in #game-night, LiveKit kept the same participant, the strip said "Connected in #game-night". A watcher who opened #general kept receiving the share (frames 84 → 279 in 3 s). |
| Leaving voice stops capture | **Holds** | After Leave voice, every recorded microphone track ended (ana, ben, cy, dee) and LiveKit dropped the participant; after Stop sharing, the screen tracks ended. Leaving during a conchd outage also ended capture. |

## Journeys tested

| Journey | Viewports | Result |
| --- | --- | --- |
| **The full journey:** ana makes a personal invite; dee opens it, tries a taken name, picks "dee", lands in #general, sends a first message; sees "2 in voice in #game-night", opens it, joins voice; ben shares; dee presses Watch; dee leaves | 1440 (dee), 1280 (ben) | Works end to end. The code leaves the address bar at once; the landing page shows inviter, channels, voice, expiry; the taken name is kept with "That name is taken. Choose another."; nothing joins voice until Join. [landing](browser-ux-audit-2/journey-01-invite-landing-1440.png), [taken](browser-ux-audit-2/journey-02-name-taken-1440.png), [first message](browser-ux-audit-2/journey-03-first-message-1440.png), [joined](browser-ux-audit-2/journey-04-joined-voice-1440.png), [share on](browser-ux-audit-2/journey-05-share-on-before-watch-1440.png), [watching](browser-ux-audit-2/journey-06-watching-share-1440.png), [left](browser-ux-audit-2/journey-10-left-voice-1440.png). Gap: N03 |
| **The same on a phone:** eve opens an operator's open link, joins, sends, opens #game-night from the occupancy line, joins voice, watches ben's share | 390×844 | Works; the log stays at its end through the join. With four in voice and a share on, the strip is 324 px: N01. [landing](browser-ux-audit-2/journey-07-invite-landing-phone.png), [first message](browser-ux-audit-2/journey-08-first-message-phone.png), [watching](browser-ux-audit-2/journey-09-watching-share-phone.png) |
| #general as the default channel (#556) | 1440, phone | Every sign-in and every invite landed in #general, although #announcements and #book-club exist. [lobby](browser-ux-audit-2/baseline-lobby-1440.png) |
| Voice occupancy (#596): one person joins, others on other channels; counts beside unread counts; a person without voice | 1440, 1280, phone | Counts and the strip line within 1.5 s; the mark is outlined with a speaker, the unread pill filled, so they read apart; kit sees no counts. On a phone the mark of a channel off to the right is off screen, but the strip line names it. [1280](browser-ux-audit-2/F01-occupancy-elsewhere-1280.png), [phone](browser-ux-audit-2/F01-occupancy-phone.png), [desktop pills](browser-ux-audit-2/N-occupancy-beside-unread-1440.png), [phone pills](browser-ux-audit-2/N-occupancy-beside-unread-phone.png) |
| Screen share (#554): one button, watched by a desktop and a phone, the watcher browses, the sharer stops, then a viewer shares | 1280, 1440, phone | Share screen opens the picker at once (no content choice); the capture's video track has `contentHint` "motion". Watching is a press of Watch (by #444's design). [sharer](browser-ux-audit-2/share-sharer-1280.png), [desktop](browser-ux-audit-2/share-watching-1440.png), [phone](browser-ux-audit-2/share-watching-phone.png), [while browsing](browser-ux-audit-2/share-watching-while-browsing-1440.png). Gaps: N02, N03 |
| The voice strip: roster at 1 to 8 people; mute, deafen; push to talk by pointer, touch, Space, Enter and the talk key; open microphone; seen from the other side | 1440, 1280, 768, phone | As the fix table says. Gap: N09 (Hold to talk while muted). [eight at 1280](browser-ux-audit-2/F03-roster-8-1280.png) |
| Phone layout in voice | 390×844 | Fits in 194 px with up to three in voice; see N01 beyond that. [in voice](browser-ux-audit-2/phone-in-voice.png), [watching with five](browser-ux-audit-2/N-phone-watching-5.png) |
| The invite panel and the dead-invite page | 1440, phone | As F18 and F19 say. [panel](browser-ux-audit-2/F18-invite-link-made-masked.png), [dead](browser-ux-audit-2/F19-dead-invite-phone.png) |
| Server down: conchd stopped for 30 s under three tabs (one in voice, one with a draft, one on an empty channel on a phone), then started | 1440, 1280, phone | "Reconnecting" at once, stale banner, paused composer, joins disabled with the reason; LiveKit voice continued; all recovered within 4 s of the start; the draft sent. [1280](browser-ux-audit-2/F17-server-down-1280.png), [phone](browser-ux-audit-2/F17-server-down-phone.png), [in voice](browser-ux-audit-2/server-down-in-voice-1440.png), [after](browser-ux-audit-2/F06-recovered-after-restart-1440.png) |
| Voice down: LiveKit unreachable for 25 s (through the audit's relay) while ana is in voice and ben and cy are not, then back | 1440, 1280, phone | Not joined: "Voice is unavailable right now; chat still works." with joins disabled, and no occupancy marks; back 7 s after LiveKit returned. Joined: see N06. [joined](browser-ux-audit-2/voice-down-in-voice-1440.png), [not joined](browser-ux-audit-2/voice-down-not-joined-1280.png), [phone](browser-ux-audit-2/voice-down-phone.png), [back](browser-ux-audit-2/voice-back-in-voice-1440.png) |
| Refused and empty states: blocked microphone, no-voice account, empty channel, busy share | 1440, phone | As F07, F14, F16 and F27 say |
| Keyboard: sign-in focus, Tab order, arrows in the channel list, Settings and Escape, join/talk/leave | 1440 | Focus lands on the heading; Tab goes log → audience → composer → members → strip buttons → top bar → channels; ArrowDown moves within the list; Settings opens on its heading and Escape returns focus to Settings |
| 200% zoom | 720×450 | Usable; the page scrolls as a document (790 px tall), the strip at its end. [in voice](browser-ux-audit-2/zoom200-in-voice.png) |

## New findings

Severity: **P0** blocks a tester; **P1** a tester will regularly go wrong or be misled; **P2** a real defect or gap with a workaround; **P3** polish. Kinds: defect, confusing flow, visual inconsistency, not yet built.

### Defects

#### N01 — Phone: the capped roster stacks its chips in a column; with a share on, the strip leaves about one line of chat

- **Severity / kind:** P2, defect (layout). **Viewport:** 390×844.
- **Steps:** five people in voice in `#game-night` (cy on a phone, four on desktops). Measure cy's dock. ben shares; measure again; cy presses Watch; measure the message log.
- **Evidence:** cy's page, measured:

  | State | Dock | Roster block | Chip rows | Message log |
  | --- | --- | --- | --- | --- |
  | 1 to 3 in voice | 194 px | 40 px | 1 | 347 px |
  | 4 or 5 in voice | 234 px | 81 px | 2 | 307 px |
  | 5 in voice, ben sharing | 324 px | 121 px | 3 | 217 px |
  | 5 in voice, watching | 275 px | 121 px | 3 | **48 px** |

  "N in voice" and "+N more" take the row's ends, so the three capped chips wrap into a column of their own. The viewer's own chip is often behind "+N more" (chips are in principal order). While watching, the line "ben is sharing a screen, with sound. One person shares at a time." takes two more lines, though the stage already says who shares.
- **Should:** #600's acceptance (the dock at most 200 px at 390×844; it was written for three in voice); UX-22 "keep chat readable on small surfaces"; #536 §2 (the conversation keeps what its header, a few lines and the composer need while a share is watched).
- **Impact:** the alpha's exit step 5 is "the others watch, phone included". A phone watching a game night with five people can read one message.
- **Recommendation:** on a phone keep the capped roster on one row (count, chips that shrink and ellipsise, "+N" inline), put the viewer first, and while watching collapse the roster to its count; do not repeat the one-sharer rule in the notes while watching.
- **Screenshots:** [five, sharing](browser-ux-audit-2/N-phone-strip-5-sharing.png), [five, watching](browser-ux-audit-2/N-phone-watching-5.png), [eight](browser-ux-audit-2/F03-phone-plus-more.png).
- **Existing issues:** #587 and #600 (closed) set the cap and the 200 px bound; none open.

#### N02 — The sharer's "You stopped sharing." never goes away

- **Severity / kind:** P3, defect. **Viewport:** 1280.
- **Steps:** ben shares, then presses Stop sharing; wait 30 s; then ana shares.
- **Evidence:** 31 s after stopping, ben's notes still end "You stopped sharing."; it is still there after ana's share has started, under "ana is sharing a screen, with sound. One person shares at a time." It is the sharer's twin of F14's stale note.
- **Recommendation:** clear it after a few seconds, as the viewer's "ben stopped sharing." is (5 s), and whenever a share starts.
- **Screenshot:** [31 s after](browser-ux-audit-2/N-sharer-note-stays-1280.png).
- **Existing issues:** #591 (closed) fixed the viewer's side and the busy note.

#### N04 — Signing out logs a 404 for the page's own session

- **Severity / kind:** P3, defect (console). **Viewport:** 1440.
- **Steps:** as jo, Settings → Sign out → Yes, sign out.
- **Evidence:** the console shows "Failed to load resource: the server responded with a status of 404 (Not Found)" for `DELETE /v1/sessions/{id}`: the credential is revoked first, which ends its sessions, and the page then ends its session (`api.endSession`, from `useAgents`). No `ERR_ABORTED` (#622 holds).
- **Impact:** every sign-out leaves a red line in the console, which testers' reports and the report-a-problem diagnostics carry (F25's reason).
- **Recommendation:** end the session before revoking the credential, or skip ending it once the credential is revoked. Chromium logs any 4xx itself; page script cannot hide it.
- **Existing issues:** #622 (closed) and #595 (closed) are the same family.

#### N05 — While LiveKit is unreachable, the console shows LiveKit URLs that carry a token

- **Severity / kind:** P3, defect (a credential in the console). **Viewport:** 1440.
- **Steps:** ana in voice; make LiveKit unreachable for 25 s (How to reproduce).
- **Evidence:** six console errors "WebSocket connection to 'ws://…/rtc/v1?…' failed" and six failed `GET …/rtc/v1/validate?…`; each URL carries the LiveKit token as a query parameter. (Redacted here and in the audit's logs.)
- **Impact:** a tester who pastes the console into a bug report during an outage pastes a LiveKit token. conchd's own join tokens live 15 s (`voiceTokenLifetime`); *guess:* a reconnect after a while uses LiveKit's refresh token, which lives longer. Chromium writes these lines itself, so page script cannot stop them.
- **Recommendation:** find which token the reconnect carries and how long it lives; if it outlives the outage, say in the report-a-problem form not to paste console output, or have conchd ask LiveKit for a shorter refresh lifetime if it allows.
- **Existing issues:** #619 (closed) is the same token in resource timing in the e2e suite; #161 (closed) on LiveKit's refresh tokens.

### Confusing flows

#### N03 — In voice, a share in progress is not offered to watch

- **Severity / kind:** P3, confusing flow. **Viewports:** all.
- **Steps:** dee is in voice in `#game-night`; ben starts a share.
- **Evidence:** dee's strip gains a "Watch" button among eight controls, and the notes say "ben is sharing a screen, with sound. One person shares at a time." The roster shows "ben sharing". Nothing says to press Watch; the second sentence answers a question dee did not ask. Watching on request is #444's design.
- **Impact:** at "movie night" a newcomer sees a strip that talks about who may share, and may not find the picture.
- **Recommendation:** "ben is sharing a screen, with sound. Watch to see it." with Watch next to it; keep the one-sharer rule as Share screen's description only (where #591 put it).
- **Screenshot:** [share on, before Watch](browser-ux-audit-2/journey-05-share-on-before-watch-1440.png).
- **Existing issues:** #444, #591, #600 (closed).

#### N06 — A joined strip says "Connected" with an empty roster while conchd reports voice unavailable

- **Severity / kind:** P3, confusing flow (to confirm with a real outage). **Viewport:** 1440.
- **Steps:** ana in voice in `#game-night`; LiveKit unreachable for conchd and for the page's signalling for 25 s.
- **Evidence:** for 25 s ana's strip says "Connected in #game-night · Microphone in use · Not transmitting", with no roster and no note; ben and cy, not in voice, see "Voice is unavailable right now; chat still works." conchd's presence says `available: false`. LiveKit itself still listed ana. When LiveKit came back, ana's roster returned.
- **Caveat:** the audit cut only the TCP path (signalling and conchd's API); media over UDP continued. In a real LiveKit failure the SDK would likely drop to Reconnecting (guess). What this shows is that the joined strip ignores `available: false`.
- **Recommendation:** while joined and presence says unavailable, say so ("Voice server unreachable; you may be cut off") rather than silently dropping the roster.
- **Screenshots:** [joined](browser-ux-audit-2/voice-down-in-voice-1440.png), [not joined](browser-ux-audit-2/voice-down-not-joined-1280.png).
- **Existing issues:** #585 (closed) built the not-joined states.

#### N09 — Hold to talk stays enabled while muted or deafened, and pressing it does nothing visible

- **Severity / kind:** P3, confusing flow. **Viewport:** 1440.
- **Steps:** in voice, press Mute (or Deafen); hold Hold to talk for 2 s.
- **Evidence:** the button is enabled; nothing is sent (correct: the other side sees nothing, LiveKit keeps the track muted) and the strip keeps "Muted. Not transmitting". The button does not change.
- **Impact:** someone who muted, forgot, and holds to talk gets no cue why nobody hears them, beyond the status line.
- **Recommendation:** while muted or deafened, label it "Muted: unmute to talk" (keep it focusable) or show a brief note on press.
- **Screenshot:** [deafened](browser-ux-audit-2/F13-deafened-1280.png) (Hold to talk enabled).
- **Existing issues:** #591 (closed) handled Mute's own state.

### Visual inconsistencies

#### N07 — A disabled Join voice looks enabled; the "you hear the room" hint stays when voice cannot be joined

- **Severity / kind:** P3, visual inconsistency. **Viewports:** 1280, phone.
- **Steps:** stop conchd, or make LiveKit unreachable; look at a strip not in voice.
- **Evidence:** Join voice keeps its green fill, slightly dimmed, while Watch only greys out clearly; under them "Microphone off. You hear the room and can watch a share." stays beside "Voice status unavailable: the server cannot be reached."
- **Recommendation:** a clearly disabled look for the primary button (as `button:disabled` gives the others); hide the Watch only hint while the joins are disabled.
- **Screenshots:** [1280](browser-ux-audit-2/F17-server-down-1280.png), [phone](browser-ux-audit-2/F17-server-down-phone.png), [voice down](browser-ux-audit-2/voice-down-not-joined-1280.png).

#### N08 — Small polish in the strip and the phone row

- **Severity / kind:** P3, visual inconsistency. **Viewports:** as stated.
- **What happens:** (1) "Open #game-night" in the occupancy line is drawn as plain text, with no border or underline, so it does not read as a control (1280, phone). (2) "Join voice  in #general" shows a double gap before "in" (1440, 1280). (3) On a phone, the open channel scrolled into view at the row's right edge sits under the fade (cy's "#game-night" after the way back).
- **Recommendation:** give the Open button the quiet-button outline or a link underline; remove the extra space; scroll the open channel to `center` or pad the fade.
- **Screenshots:** [1280](browser-ux-audit-2/F01-occupancy-elsewhere-1280.png), [phone row](browser-ux-audit-2/phone-in-voice.png).

### Not yet built

Device choice and per-person volume (F10, #597) and others' mute and deafen (F11, #598) remain open proposals. No new unbuilt feature was found that the alpha's documents promise.

## Top five, in the recommended order of implementation

| Order | Improvement | Findings | Size |
| --- | --- | --- | --- |
| 1 | **Keep the phone strip compact in a busy room and while watching**: one-row capped roster with the viewer first, the roster collapsed while watching, no one-sharer line while watching. | N01 | small to medium (CSS and the roster) |
| 2 | **Make the share notes say the next step**: "Watch to see it" when someone shares; clear "You stopped sharing." | N03, N02 | quick |
| 3 | **Controls that cannot act look and say so**: disabled Join voice, Hold to talk while muted or deafened, the Watch only hint while voice is unavailable. | N07, N09 | quick |
| 4 | **The joined strip is honest during a voice outage**: say that voice is unreachable instead of dropping the roster. | N06 | small |
| 5 | **Keep testers' consoles clean of noise and tokens**: no 404 on sign-out; find out how long a logged LiveKit token lives. | N04, N05 | quick (N04), a check (N05) |

Order rationale: 1 is the only P2 and sits on the alpha's exit scenario (watching on a phone). 2 and 3 are small client changes on the two flows every tester will use, sharing and talking. 4 matters only during an outage but is the UX-23 contract. 5 protects bug reports; N05 is first a question. N08 is polish after these.

## How to reproduce

Build with `make build` and `make web-build` (Node 24.21.0). Run `bin/conchd bootstrap-operator --data <dir> --name op`, then, with `CONCHD_LIVEKIT_API_KEY` and `CONCHD_LIVEKIT_API_SECRET` set, `bin/conchd serve --data <dir> --listen 127.0.0.1:<port> --livekit-url ws://127.0.0.1:<lk-port> --livekit-room-prefix <prefix> --web-dir web/dist --test-relax-invite-limits`. Make channels with `conch admin channel create` and `conch admin member add`, people with invites redeemed over `POST /v1/invites/redeem`, and messages with `POST /v2/channels/{c}/messages`, as in Demo data. Drive `/app/` in Chromium through `@playwright/test` with the fake-media flags above, one browser context per person; the operator's credential is shown its warning page and needs "Continue anyway".

To make voice unavailable without touching LiveKit, start conchd with `--livekit-url` pointing at a local TCP relay to LiveKit's port, and stop the relay: conchd's API calls and the pages' signalling go through it, and LiveKit keeps running. The real microphone refusal (F27) needs Chromium's new headless mode (`channel: 'chromium'`) with the permission denied through the DevTools protocol (`Browser.setPermission`, `microphone`, `denied`); the headless shell answers `getUserMedia` without a fake UI with `NotSupportedError`. The audit's scripts lived in a scratch directory and are not part of the repository.

## What this audit could not check

- Real microphones, headsets, echo, speech detection (F21's real-microphone half), the real share picker, background tabs, Firefox and Safari: #474.
- Hiding a tab in headless Chromium: the invite list's re-read on showing the tab was driven by a dispatched `visibilitychange` event.
- A real LiveKit failure: the relay cut TCP only (N06's caveat).
- An expired invite link (the other three dead causes were tried; #593 gives all four one text).

## Proposed issues

Filed by the principal as #641–#647 on 2026-10-19. Each was checked for duplicates with `gh issue list --state all --search …` (queries: "stopped sharing", "sessions 404 sign out", "DELETE sessions", "token console", "join token", "Watch share", "roster phone", "deafened talk", "phone strip", "share notes", "dock height") and against the open issues, including #474, #597, #598 and #628–#633. None duplicates a finding; related issues are named in each. F10 and F11 need no new issue (#597, #598).

### 1. Web voice: the phone strip stays compact with four or more in voice and while watching (N01) — filed as #641

**Context.** At 390×844 the capped roster (#587) wraps its three chips into a column once four people are in voice: the dock is 234 px with four or five, 324 px with a share on, and while watching the message log is 48 px. #600's 200 px bound was written for three in voice. UX-22; #536 §2; private-alpha exit step 5 (watching on a phone). Audit #626 N01.
**Files.** `web/src/voice/VoicePanel.tsx` (`Roster`, the busy line while watching), `web/src/app.css` (the phone block's `.voice-who`, `.voice-roster.capped`, `.voice-more`), `web/e2e/16-layout.spec.ts` (fake presence through `page.routeWebSocket`, as #587 does), `web/e2e/12-screen-share.spec.ts`.
**Acceptance criteria.**
- [ ] At 390×844 and 360×740 with eight in voice and nobody sharing, the roster block is one row and the dock is at most 200 px.
- [ ] The viewer's own chip is among those shown when the roster is capped.
- [ ] While a share is watched on a phone with eight in voice, the message log is at least 120 px tall and the composer is on screen without scrolling.
- [ ] While watching, the notes do not repeat who shares (the stage title says it).
- [ ] At 700 px and wider nothing changes (#587's checks pass).
**Required tests.** Layout e2e at both phone sizes with eight faked participants, sharing and not; component test for the viewer-first order.
**Blocked by.** Nothing.

### 2. Web voice: share notes that say the next step, and a sharer's note that clears (N02, N03) — filed as #642

**Context.** In voice, while someone shares, the notes say "<name> is sharing a screen, with sound. One person shares at a time." and do not say to press Watch. After stopping, the sharer's "You stopped sharing." stays for good, even after another share starts. Audit #626 N02, N03; built in #444, #591, #600.
**Files.** `web/src/voice/VoicePanel.tsx` (`ShareNotes`, the ended note), `web/src/voice/share.ts` (`othersShareNote`, `busyNote`, `endedNote`), `web/src/voice/VoicePanel.test.tsx`, `web/e2e/12-screen-share.spec.ts`.
**Acceptance criteria.**
- [ ] In voice, not watching, while another shares: the note reads "<name> is sharing a screen[, with sound]. Watch to see it."; Share screen keeps its description with the one-sharer rule.
- [ ] "You stopped sharing." is gone within 8 s, and at once when anyone's share starts.
- [ ] The other ended reasons (`endedNote`) keep their text and timing unless the principal decides otherwise.
**Required tests.** Component tests for both notes with a fake clock; a step in the share e2e.
**Blocked by.** Nothing.

### 3. Web voice: controls that cannot act look and say so (N07, N09) — filed as #643

**Context.** A disabled Join voice keeps its green fill; the Watch only hint stays while joins are disabled; Hold to talk stays enabled while muted or deafened and does nothing visible when held. UX-07 "Disabled controls explain why"; UX-11. Audit #626 N07, N09.
**Files.** `web/src/app.css` (`button.primary:disabled`), `web/src/voice/VoicePanel.tsx` (the talk button's label while muted or deafened; the not-joined hints), `web/src/voice/VoicePanel.test.tsx`.
**Acceptance criteria.**
- [ ] A disabled primary button's computed background differs from the enabled one by at least the amount other disabled buttons differ.
- [ ] While the joins are disabled, the "You hear the room" hint is not shown; the reason is.
- [ ] While muted or deafened, Hold to talk says why it will not send (visible text or accessible description), and pressing it still sends nothing.
**Required tests.** Component tests per state.
**Blocked by.** Nothing.

### 4. Web voice: the joined strip says when voice is unavailable (N06) — filed as #644

**Context.** While joined, if conchd's presence turns `available: false`, the roster disappears and the strip keeps "Connected in #x" with no note; people not in voice are told "Voice is unavailable right now". UX-23 "LiveKit unavailable: voice action explains unavailability". Audit #626 N06 (seen with signalling cut; media continued).
**Files.** `web/src/voice/VoicePanel.tsx` (in-voice notes), `web/src/voice/useVoice.ts` if the snapshot needs exposing, `web/src/voice/VoicePanel.test.tsx`; optionally `web/e2e/10-voice.spec.ts` with `stopLiveKit`.
**Acceptance criteria.**
- [ ] Joined, with presence `available: false`, the strip shows "Voice server unreachable; you may be cut off" (wording for the principal) and no stale roster.
- [ ] When presence is available again the note goes and the roster returns.
- [ ] Nothing about the connection lifecycle changes.
**Required tests.** Component test with a fake presence snapshot; e2e step if the voice world can stop LiveKit while joined.
**Blocked by.** Nothing.

### 5. Web: signing out logs no 404 for the page's own session (N04) — filed as #645

**Context.** Sign out revokes the credential (which ends its sessions) and then sends `DELETE /v1/sessions/{id}`, answered 404, which Chromium logs as a console error. Audit #626 N04; #622 and #595 quieted the rest.
**Files.** `web/src/api.ts` (`signOut`, `endSession`), `web/src/agents/useAgents.ts` (where the session is ended), `web/e2e/18-status-only.spec.ts`.
**Acceptance criteria.**
- [ ] Sign out and Sign out everywhere log no 4xx/5xx and no failed request (an e2e `response` and `requestfailed` watch on conchd's origin).
- [ ] The session still ends on the server (the members column of another person shows it offline as today).
**Required tests.** e2e step in `18-status-only`; unit test of the order.
**Blocked by.** Nothing.

### 6. Voice: find out which LiveKit token a failed reconnect logs, and how long it lives (N05) — filed as #646

**Context.** While LiveKit is unreachable, Chromium logs the SDK's reconnect URLs, with their token parameter, as console errors; testers paste console output into bug reports. conchd's join tokens live 15 s; a reconnect may use LiveKit's refresh token. Audit #626 N05; #619, #161 (closed).
**Files.** `internal/server/voice.go` (token lifetime), `web/src/Report.tsx` (the report form's guidance), a note in `docs/design/voice-control-plane.md`.
**Acceptance criteria.**
- [ ] The issue records which token appears in the logged URL after 1 min and after 10 min of an outage, and its lifetime.
- [ ] If it can outlive the outage by minutes: the report form says not to paste console output, or conchd shortens the refresh lifetime if LiveKit allows it (principal's call).
**Required tests.** None for the finding; a unit test if the report text changes.
**Blocked by.** Nothing.

### 7. Web: polish in the strip and the phone row (N08) — filed as #647

**Context.** "Open #x" in the occupancy line looks like plain text; "Join voice  in #x" has a double gap; on a phone the open channel scrolled to the row's right edge sits under the fade. Audit #626 N08.
**Files.** `web/src/app.css` (`.voice-elsewhere button`, `.voice-controls .dest`, the phone row's fade), `web/src/ChannelList.tsx` (scroll position), `web/src/voice/VoicePanel.tsx`.
**Acceptance criteria.**
- [ ] The Open button has a visible control affordance at every width.
- [ ] One space between "Join voice" and "in #x".
- [ ] At 390×844, the open channel's button after the way back is fully outside the fade.
**Required tests.** Component or layout e2e assertions for the scroll position; visual check by the principal.
**Blocked by.** Nothing.
