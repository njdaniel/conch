# Phone browser layout: the mockup mapped to the application

**Issue:** #929. **Status:** implemented; the by-hand checks on real phones are not done (section 6).
**References:** the concept, kept as [`mockups/phone-browser-layout.html`](mockups/phone-browser-layout.html);
[browser UI polish](browser-ui-polish.md) (the layout this revises, section 2);
[client experience](client-experience.md); [ADR-007](../adr/ADR-007-client-interfaces.md).

Nick approved the direction of the concept on 2026-10-28: one primary panel at a
time, a bottom navigation of **Chat / Channels / Voice**, compact persistent
voice controls, and audio and account settings from the top bar. This note says
which parts of the concept are built, on what, and which are only the concept's
sample content. The change is presentation: no API client, generated schema,
reducer, or voice state machine was touched, and the one room/media owner
(`VoiceStatus` / `VoicePanel`, in the lobby's dock) stays mounted once, whatever
the view or the window's width.

The concept is a stand-alone page with sample people and messages, local
interactions and no audio. It is a visual and interaction reference, not code to
copy and not evidence that a phone browser supports anything.

## 1. The rule

Below 700 CSS px (the existing `max-width: 699px` breakpoint) the lobby shows
one of three views and a bottom navigation (`nav`, "Main navigation") whose
three buttons are the views. At 700 px and wider nothing changes: the
navigation is not rendered and the channels, the conversation, the members
column and the strip are on screen as before.

Every view's content is **always mounted**; the views differ in what is drawn.
The channel list and the conversation lie in the same grid cell and the hidden
one is `visibility: hidden`, so it keeps its size and its place: the log's
scroll position, the composer's draft (held in the reducer in any case), the
stage's video element and the voice client all carry on across a switch. Nothing
is unmounted, so nothing can join, leave or retarget voice by being switched
away.

## 2. Concept to component

| Concept element | Built as | Notes |
| --- | --- | --- |
| Header: server button, avatar | The existing top bar (`Lobby`): who, the address, the connection word, Invite, Members, Settings | Kept. Conch has no server display name or picture (API), so the address stands in; the account and audio settings are the existing **Settings** panel, reached from the top bar as the concept's avatar is. |
| Bottom navigation: Chat, Channels, Voice | `Lobby` phone navigation: three buttons, `aria-current="page"` on the current one, 44 px high at least, safe-area padding | Shown only below 700 px. A dot on **Voice**, and the words "You are in voice" as its description, while this page is in voice: `VoicePanel` reports whether it is in voice (joined, connecting, reconnecting or unavailable) through the `VoiceLive` context, and the lobby uses that only to draw (never to start or stop anything). |
| **Chat**: channel heading, messages, composer | The existing `Conversation` and `Composer` | Unchanged components. Heading, audience line, per-channel drafts, Sending / Failed / Not confirmed with Retry, Jump to latest, and the attachment button (which exists: #25-attachments) are as on main. The concept's "+" is the real **Add image** button, not a menu. |
| Chat: members count button | The top bar's **Members** (a panel over the page, as at 700 to 1099 px) | The concept's Members page is not a view: the existing panel (who is here, agents and runs) is kept. |
| **Channels**: Text list, unread | The existing `ChannelList`, full width, one channel a row, with its unread pill, "manages" badge and in-voice count | Replaces the sideways-scrolling row of #601 (the revision recorded in browser-ui-polish section 2). Choosing a channel opens it and returns to Chat. Rename and New channel stay under the list. |
| Channels: **Voice** group (Lounge) | Not built as rooms | Voice in Conch is per channel (ADR-005). A channel's row shows how many are in its voice; the concept's separate voice rooms, their "Active voice connection" dot and subtitles are illustrative. No new entity or API field. |
| Channels: row subtitles ("Chat with everyone") | Not built | Channels have no description in the API. |
| Channels: Members row, "8 online" | Not built | See Members above. |
| **Voice**: heading, "Connected · 3 people", tiles | The existing `VoicePanel`, expanded to the page: its state line ("Connected in #voice-room", or "Not in voice"), who is in voice (all of them, with names), the controls with their words | The panel in the lobby's dock, given the room. No tile grid (browser-ui-polish section 1). |
| Voice: Push to talk / Open mic segments | The existing **Voice options** (mode, talk key, level), reached from the Voice view | Kept where it is; one pair of controls only. |
| Voice: large Hold to talk | The existing **Hold to talk** button, larger in the Voice view | One button. The compact strip and the Voice view are the same element, so there are never two push-to-talk controls. Pointer release, cancel and blur still let go (unchanged handlers). |
| Voice: Join Lounge | Existing **Join voice in #channel** / **Watch only in #channel**, explicit, with the channel named in words in this view | Opening a channel or a view never joins. |
| Voice: "Mic idle" | Not used | The production words are the existing ones: **Microphone in use** (captured) and **Transmitting** / **Not transmitting**, Muted, Deafened, Listen only, Watch only (UX-11). Capture is not transmission. |
| Voice: "Sam's screen, tap to watch" | The existing **Watch** button (and the screen-share notes) | The concept's separate Watch page is the existing stage: see below. |
| Voice: Audio settings | The existing **Voice options** (microphone, output where the browser allows it, per-person volume, processing, microphone test) and Settings | The concept's "Managed by your phone" output and "Preview microphone test" are sample content; the real controls are detected by capability. |
| Persistent voice bar: destination, status, Mute, Leave, talk | The existing strip, compact, under Chat and Channels **while in, connecting or reconnecting** (a stopped or unavailable voice says why in the Voice view): the state with the actual channel (a button back to it when another is on screen), **Hold to talk**, Mute, Deafen, Share / Stop sharing, Watch, Voice options, **Leave voice** | Hidden under Chat and Channels while not in voice (the concept's bar shows only when connected); Join is in the Voice view. Stop sharing and Leave voice keep their words. |
| Watch page: preview, share audio, back | The existing `ShareView` on the stage, which is above the conversation under Chat and above the strip in Voice | Mute share, Share volume, Full screen, Stop watching as on main. Not a view of its own: the way back to chat is the navigation. |
| Settings page | The existing Settings panel over the page | Modal, as before; account, sign-ins, sign-out, report. |
| Sample content: Campfire, Sam, Alex, Maya, messages, "8 online", mic meter, "Preview" notes | Not copied | Illustrative. |
| Lucide icon runtime | Not used | Icons are the existing inline SVG `Glyph`s. |
| `light-dark()` palette | Not used | The existing dark tokens (browser-ui-polish section 3). |

## 3. What the views show

| View | Shown | In the strip (dock) |
| --- | --- | --- |
| Chat | The stage (a watched share), then the conversation: heading, log, composer | Compact, only while in voice |
| Channels | The channel list (and rename / New channel) | Compact, only while in voice |
| Voice | The stage (a watched share), then the voice panel filling the rest, scrolling inside itself | Expanded: everyone in voice, labelled controls, notes and Voice options |

The conversation is not drawn in Voice but stays laid out (hidden, behind), so
returning finds the log where it was and the draft in the box.

## 4. Behaviour kept and how

- **Browsing never joins.** Choosing a channel, a view, Members or Settings calls
  only what it called before. The Join and Watch only buttons are the only way
  in (`VoicePanel`, unchanged). The test in `App.test.tsx` (line 310) that no control
  outside the voice section is named for joining holds.
- **Truthful destination.** The strip's text names the voice channel, as a button
  to it when another is on screen; in the Voice view the channel Join would use
  is written out ("Join voice in #general").
- **Focus.** After choosing a channel in Channels, focus goes to the
  conversation's heading (the page has moved under the finger). The navigation
  buttons keep focus when pressed. Settings and Members return focus as before.
- **Keyboard and browser chrome.** `index.html`'s viewport asks for
  `interactive-widget=resizes-content` and `viewport-fit=cover`; the lobby is
  `100dvh`, follows the visual viewport (`window.visualViewport`) where a browser
  does not resize the layout viewport for the keyboard, and pads for the safe
  areas. The composer and the strip are in the same column, so a shorter
  viewport shortens the log, not the controls.
- **Short windows** (under 560 px tall, which includes a phone turned on its
  side at 844 px, a tablet layout) keep the existing rule: the page scrolls as a
  document.
- **No horizontal overflow**: every row is `minmax(0, 1fr)`, long names wrap or
  end in an ellipsis; tested at 320 × 640, 360 × 740, 390 × 844 and 430 × 932.

## 5. Overlap with #647

#647 (open) polishes the voice strip and the channel row. This change
**replaces** the channel row on a phone, so the row's polish is moot there; the
strip's compact styles from #600 and #641 are kept as they were for the
compact strip. Nothing of #647's is duplicated here; whoever lands second
rebases `app.css` and `ChannelList.tsx`.

## 6. Verified and not verified

Verified: Vitest component tests; Playwright (Chromium, emulated phone sizes,
touch) in `12-phone`, `16-layout`, `10-voice` and `12-screen-share`; before and
after captures in [`mobile-browser-layout/`](mobile-browser-layout/), made by
`web/e2e/shots/phone.shots.ts`.

**Not verified** (by hand, on real devices, still to do): Android Chrome and iOS
Safari with a real on-screen keyboard (the emulator's resize is not the keyboard),
real microphone permissions and denial, rotation, Bluetooth and speaker routes,
what a locked or backgrounded phone does to a call (not promised: the page says
the talk key and hold-to-talk work only while the page is active), and iOS's
`visualViewport` behaviour.
