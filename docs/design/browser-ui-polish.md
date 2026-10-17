# Browser UI polish: the mockups, mapped to the alpha

**Issue:** #536. **Status:** implemented; awaiting the owner's review of the running build.
**References:** [mockups](mockups/README.md), [client experience](client-experience.md) (UX-01 to UX-23), [ADR-007](../adr/ADR-007-client-interfaces.md), [private alpha](private-alpha.md).

The browser client worked but looked like a form: a light page, a row of
unstyled buttons and a paragraph across the header, a log in a grey box, and
every voice control stacked in the right column. This note compares that with
the three graphical references, says what the alpha takes from each and what
it leaves out, and records the tokens the stylesheet now uses. It changes
presentation only: no API, schema, reducer or voice lifecycle changed
(`share.ts`, `sdk.ts`, `connection.ts` and `useVoice.ts` are untouched), and
no dependency was added.

## 1. The references against the alpha

The images are generated concepts (see the [index](mockups/README.md)). Their
names, languages, captions, "Not saved" line and "Hold Mouse 4" hint are
illustrative, and none of them is reproduced.

### Server lobby (`server-lobby.png`)

| Element in the image | Before | Now | Why |
| --- | --- | --- | --- |
| Top bar: logo, server, "Server connected" | `Conch` heading; name, address and connection in a footer | Top bar: Conch, then the status: the person, the server address, and Connected / Connecting / Reconnecting as a word with a dot | UX-01. Conch has no server display name in the API, so the address stands in. On a phone the address is in Settings. |
| "Invite friends" in the top bar | "Invite someone" in the right column | "Invite someone" in the top bar; its form opens below it, and Escape closes it with focus back on the button | Same panel and behaviour (#440, #457, #468), only moved. Shown only to someone who may invite. |
| Settings gear | Five account buttons and a paragraph in the header | One **Settings** button; a panel holds who you are, Change your name, Your sign-ins, Sign out and Sign out everywhere (with their note and question), Report a problem and the server version | The secondary options leave the header. Nothing is new, nothing removed. |
| Left: server card, channels with `#`, active row | Plain buttons, bold when open | Channels with `#`, the open one marked by a bar, a fill and `aria-current`; unread counts as pills | No server card: the API has no server name or picture. |
| Left: "Voice" rooms with avatars | None | Not drawn as rooms. Voice in Conch is per channel (ADR-005); who is in voice is in the voice strip | No fake rooms. |
| Centre: channel header with topic | `<h2>` | `# name` header | Channels have no topic in the API. |
| Banner "Anna, Lukas and Sofia are in … Join voice" | "Join voice" in the right column | The voice strip lists who is in voice beside Join voice and Watch only | Kept inside the voice section: browsing must never look like joining (UX-02). |
| Messages: avatar, name, time, body | `15:24 bob: text` | Initial, name, an Agent pill, time on a header line; body below; a run of messages by one author within five minutes shows the header once | The text `HH:MM name [agent]: body` is still there for screen readers and the tests; repeated headers are hidden on screen only. |
| Composer with + and send | "To:" select, a label, a textarea, a wide button | One box: the audience select and the audience label ("Message #x", "Whisper to …", "Message net …", in amber when narrower than the channel) above the text, and Send | No "+": there are no attachments. |
| Right: "Online — 6" with presence dots | Plain list | People with initials, a dot and the word (online / stale / offline); agents with an Agent badge and status; runs below | No language lines: Conch has no language field. |
| Bottom strip: you, Not in voice, Microphone inactive | Voice in the right column | A full-width voice strip at the bottom: "Not in voice", "Microphone off", who is in voice, Join voice / Watch only | "Microphone off", not "inactive", and only while nothing is captured (UX-11). Your identity is in the top bar. |

### Voice lounge (`voice-lounge.png`)

| Element | Now | Why |
| --- | --- | --- |
| Large participant tiles, speaking ring and waveform | Roster chips in the strip: initial, name, and "transmitting", "speaking", "sharing" as words; a ring around the initial while speaking | A tile grid would take the conversation's place; the alpha keeps text central. No waveform: the client knows who speaks, not each person's level. |
| Per-tile microphone icons | Not shown | Presence says who transmits, not who is muted. |
| Live captions, translation picker, "Not saved" | Not shown | Out of scope (UX-17). |
| "Hold Mouse 4 to talk" | **Hold to talk**, and under it "Talk key: ` (backtick). It works only while this tab has focus." | UX-10: the browser cannot promise global PTT. On a touch screen the key line is hidden; Voice options still says it. |
| Deafen, Share screen, Captions, Leave | Mute, Deafen, Share screen / Stop sharing, Watch, **Voice options**, **Leave voice** (the one red control) | Only what exists. |
| — | "Microphone in use" (amber) while connected or reconnecting with the microphone, apart from "Transmitting" / "Not transmitting" | Capture is not transmission (UX-11). |
| — | **Voice options**: push to talk or open microphone, the talk key, the open-microphone level, what a share is for, and the talk-key and phone notes | The less used choices, one click away. |

### Movie night (`movie-night.png`)

| Element | Now | Why |
| --- | --- | --- |
| Large stage, "X is sharing", stream volume, full screen | The watch view on the stage at the top of the centre column: who is sharing (and whether with sound), the picture, Mute share, Share volume, Full screen, Stop watching | Same controls as #444. The picture is sized to leave the conversation a header, a few messages and the composer. |
| "1080p" badge | Not shown | The client does not know the layer it receives. |
| Compact participant strip | The roster chips | — |
| Chat beside the stage | The conversation below the stage; the members column where it was | The image's right column is captions, a later feature. |
| Stop sharing in the control bar | Stop sharing, with a glyph of its own (a screen with a cross), replaces Share screen in the strip while you share; it and Leave voice keep their words at every width | Never hidden under options or reduced to an icon. What a share will carry and that a whole screen shows this page stay written under the strip before sharing, as #444 decided; while someone else shares they wait, since no picker can open then (the claim is refused first). |

## 2. Layout

| Width | Layout |
| --- | --- |
| 1100 px and wider | Top bar; channels (15rem), the centre, members (17rem; Members folds it away); the voice strip across the bottom. Each column scrolls on its own; the page does not. |
| 700 to 1099 px | Channels and the centre. Members opens a panel over the right side (UX-22: the right panel goes first). |
| Below 700 px | One column. The channels are one row under the top bar that scrolls sideways within itself, so the open channel stays in sight and one tap opens another. The top bar's and the strip's buttons show icons, their names hidden on screen but still read out; Send likewise. Stop sharing and Leave voice keep their words. Voice options, open, scroll inside the strip (at most 40% of the height). While a share is watched, the channel heading is read out only (the row above names the channel) and the picture shrinks first, so the composer stays on screen at 390×844 and 360×740. |
| Under 560 px tall (200% zoom of 1440×900 is 720×450) | The page scrolls as a document instead of fitting every column to the window; the conversation keeps a height of its own, so it scrolls and loads older messages as before, and the strip is at the end of the page. |

Settings and, below 1100 px, Members are panels over the page with a dimmed
backdrop. Opening one moves focus into it (the Settings heading; the members
list); Escape, its **Close** button or a click on the backdrop closes it and
puts focus back on the button that opened it. Settings is modal
(`aria-modal="true"`): while it is open the rest of the page is `inert`, so
focus cannot go behind it. Members is not modal. A members column left open
closes when the window narrows, rather than cover the page, and focus that was
in it goes to Members.

**Media continuity.** The voice section is rendered once, in the strip, and
never moves: panels, the members column and the window's width change classes
only. The watch view is drawn on the stage with a React portal from that same
section (`web/src/stage.ts`), so it is one component instance through every
layout change, and its `<video>` is attached to the room once. If the stage and
the conversation do not both fit (a phone, watching), the centre column scrolls;
nothing is drawn under the strip.

The navigation is not a drawer on a phone, though the issue suggested
collapsible navigation: a drawer would hide which channel is open and add a tap
to every switch. The panels that do open over the page (Members, Settings) are
the ones the focus and Escape tests cover.

## 3. Tokens

All in `web/src/app.css`, on `:root`. One dark theme, with `color-scheme: dark`
so form controls and scroll bars match.

| Token | Value | Use |
| --- | --- | --- |
| `--bg` | `#111416` | Page |
| `--surface` | `#171b1e` | Columns, panels, the strip |
| `--raised` | `#1f2428` | Buttons, inputs, chips, message hover |
| `--hover` | `#272d32` | Hover |
| `--border` | `#323a40` | Dividers, panel edges |
| `--border-strong` | `#6f7b83` | Edges of buttons and inputs |
| `--text` | `#e8ece9` | Text |
| `--muted` | `#a7b0ab` | Times, hints, unopened channels |
| `--accent` / `--accent-hover` | `#6fd69b` / `#8ee3b1` | Connected, the open channel, speaking, primary buttons |
| `--accent-bg` / `--on-accent` | `#183324` / `#0d1912` | Fill behind accent text; text on an accent button |
| `--danger` / `--danger-bg` | `#ff8a80` / `#3a1c1c` | Leave, refusals, Failed |
| `--warn` | `#e8c06a` | Reconnecting, stale, Not confirmed, Microphone in use, a narrowed audience |
| `--focus` | `#8ec5ff` | Focus outline (2 px, offset 2 px) |
| `--hue-0` … `--hue-5` | six muted hues | Initials, chosen by principal id; white letter |
| `--stage-reserve` | 26rem (16rem on a phone) | Height the watch view leaves for its controls and the conversation |

Type: the system UI font (the CSP allows no web font, and none is needed),
15 px base, 1.45 line height; 0.8125 rem for times and hints; weight 600 for
names and headings. Spacing: `--s1` 0.25 rem to `--s6` 2 rem. Radii: 6 px for
controls, 10 px for panels, round for pills and initials.

Components: one button style with `primary`, `quiet` and `danger` variants and
a pressed state; one input style; pills for unread counts, states and badges;
chips for people in voice; one panel style for Settings, Members over the page
and the invite form.

### Contrast

Checked with a script that reads the tokens out of `app.css` and computes the
WCAG 2 ratio of each text/background pair the stylesheet uses. Normal text, the
lowest pairs: muted on hover 6.26, white on the darkest initial 5.92, danger on
danger-bg 6.75, accent on accent-bg 7.65, on-accent on accent 10.10; text on any
surface 11.45 or more. Non-text: the focus ring 8.63 or more, control edges
(`--border-strong`) 3.61 or more. State never relies on colour alone: every dot
or ring has a word beside it.

## 4. Behaviour kept

- Browsing never joins voice or moves its destination: the strip names the
  channel it is connected to, which may differ from the one on screen.
- Nothing opens the microphone by itself; capture and transmission are said
  apart; the talk key's limit is said beside Hold to talk.
- Drafts, the scroll position, Jump to latest (now a pill over the log),
  Sending / Failed / Not confirmed with Retry and Dismiss, audience badges,
  sign-out questions, revocation and every error line are the same components.
- Nothing outside the voice section mentions voice or offers to join it (the
  lobby test that checks this is unchanged).
- No inline style, no external font or script; icons are inline SVG with
  `aria-hidden`, and each icon button keeps its words as hidden text. The
  initials are CSS (`attr(data-initial)`), so they are in no text read out.
- `prefers-reduced-motion: reduce` turns off the panels' slide and the speaking
  ring's pulse.

## 5. Screenshots

The same seeded state (the browser suite's voice world: alice, bob and carol in
`#voice-room`, the same seven messages), before and after, at 1440×900 and
390×844 (the phone at twice the pixels), in [`browser-ui-polish/`](browser-ui-polish/):

| State | Desktop | Phone |
| --- | --- | --- |
| Sign-in | [before](browser-ui-polish/before-signin-desktop.png), [after](browser-ui-polish/after-signin-desktop.png) | [before](browser-ui-polish/before-signin-phone.png), [after](browser-ui-polish/after-signin-phone.png) |
| Invite | [before](browser-ui-polish/before-invite-desktop.png), [after](browser-ui-polish/after-invite-desktop.png) | [before](browser-ui-polish/before-invite-phone.png), [after](browser-ui-polish/after-invite-phone.png) |
| Lobby and chat (bob and carol in voice) | [before](browser-ui-polish/before-lobby-desktop.png), [after](browser-ui-polish/after-lobby-desktop.png) | [before](browser-ui-polish/before-lobby-phone.png), [after](browser-ui-polish/after-lobby-phone.png) |
| Settings (before: the header, with Your sign-ins open) | [before](browser-ui-polish/before-settings-desktop.png), [after](browser-ui-polish/after-settings-desktop.png) | [before](browser-ui-polish/before-settings-phone.png), [after](browser-ui-polish/after-settings-phone.png) |
| In voice | [before](browser-ui-polish/before-voice-desktop.png), [after](browser-ui-polish/after-voice-desktop.png) | [before](browser-ui-polish/before-voice-phone.png), [after](browser-ui-polish/after-voice-phone.png) |
| Watching carol's share | [before](browser-ui-polish/before-share-desktop.png), [after](browser-ui-polish/after-share-desktop.png) | [before](browser-ui-polish/before-share-phone.png), [after](browser-ui-polish/after-share-phone.png) |
| Cannot reach the server | [before](browser-ui-polish/before-error-desktop.png), [after](browser-ui-polish/after-error-desktop.png) | [before](browser-ui-polish/before-error-phone.png), [after](browser-ui-polish/after-error-phone.png) |

They are made by `web/e2e/shots/ui.shots.ts`, which nothing in CI runs. It needs
Docker (for LiveKit), Go and the Node in `web/.nvmrc`:

```sh
cd web && CONCH_SHOTS_DIR=../docs/design/browser-ui-polish CONCH_SHOTS_TAG=after \
  npx playwright test -c e2e/shots/shots.config.ts
```

To try the build by hand: `scripts/dev-stack.sh up`, then open the address it
prints, and `scripts/dev-stack.sh invite` for a second person.

## 6. Left for later

- A voice lounge tile grid and a per-person level (needs each participant's
  level from the SDK, not just who speaks).
- Captions, translation, language labels (UX-17).
- A server name and picture (needs an API field).
- Multiple nets in the strip (the index says the mockup is still missing).
- Making the share notes shorter on a narrow window: on a phone browser that can
  share, while nobody shares, they take four lines of the strip.
