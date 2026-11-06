# Browser client UX audit 3

**Release:** v0.1.0-alpha.7. **Date:** 2026-10-30. **Status:** independent review, for the project owner's approval; nothing here is accepted until then.
**Reviewer:** an AI agent that did not build the client. It drove the running web client in Chromium through Playwright, with temporary users on a disposable local server.
**References:** the [first audit](browser-ux-audit.md) (#577), the [second audit](browser-ux-audit-2.md) (#626), [message editing](../design/message-editing.md) (#845), [mobile browser layout](../design/mobile-browser-layout.md) (#929), [community model](../design/community-model.md) (ADR-013), [admin roles](../design/admin-roles.md) (ADR-014), [ADR-007](../adr/ADR-007-client-interfaces.md).

Every finding below comes from the running client, with the steps that show it. Guesses are marked as guesses. No product code was changed, and nothing touched the real alpha server.

## Summary

Alpha.7's new pieces work. **Message editing** works by every route the changelog names: the ⋯ menu, right-click, a long press and the keyboard. The editor opens inline. Enter saves, Esc cancels, and focus returns to ⋯. Others see "(edited)" live, within a second, on a desktop and on two phones. Only the author can open **Earlier versions**. Once a message is an hour old, the page stops offering Edit. **Short passwords** work everywhere they are set: in sign-up, in invites, for the operator and in Settings. Passwords that repeat a pattern or contain the person's name are still refused. **Guests are gone** from the front door: with sign-up open, the bare address offers "Create an account" and nothing else.

Most of the other flows hold up too. Chat, images, paste, the emoji picker, search, notifications and invites all work. Voice joins, push-to-talk, the device picker, mute, deafen and screen share also work. When the server stops and starts again, the client recovers in about 8 s and keeps the draft. No page threw an error.

What will hurt testers:

1. **A person added to a channel does not see it until they reload** (F1). The owner adds alice to `#secret`, and alice's list stays the same for minutes.
2. **Changing your password signs you out of the page you changed it on** (F2). This is #1000; it happened on the first try here.
3. **On a phone in voice, the voice controls take over Chat** (F3). At 320×640 they take 305 of 640 px and cover the Send button. At 390×844 while watching a share, 120 px of chat is left.
4. **The owner has no way in the browser to read problem reports, or to help a person who is locked out** (F4). Reports go to a place only the CLI can read.

**Findings: 0 blocker, 4 major, 12 minor, 10 polish (26 in all).**

| # | Finding | Severity | Viewports | Covered by |
| --- | --- | --- | --- | --- |
| F1 | Someone added to a channel does not see it until they reload | major | all | none (new) |
| F2 | Changing your password signs this page out | major | all | #1000 |
| F3 | Phone, in voice: the voice strip covers Chat (Send hidden at 320×640) | major | 320, 390 | regression of #600/#641's bound; none open |
| F4 | Owner: no browser view of problem reports, and no per-person actions | major | all | #1020 (person actions); reports: none |
| F5 | Phone top bar hides your name and the connection state | minor | 320, 390 | none (new) |
| F6 | Edit stays on offer after the ten-edit limit, and the first refusal blames a rate limit | minor | all | none (new) |
| F7 | A new owner lands on a blank page; Members says "Connecting…" forever | minor | 1440 | none (new) |
| F8 | The operator must re-enter password and code minutes after signing in with both | minor | all | none (new) |
| F9 | A newcomer is told "No messages … yet. Say hello." in a busy channel | minor | all | #986, #991 (community model) |
| F10 | The browser can attach only PNG and JPEG; other files arrive as "not supported" | minor | all | none (new) |
| F11 | `:shortcode:` completion misses common names and ranks oddly | minor | all | none (#763 closed) |
| F12 | First load: 1.3 MB of uncompressed script and a blank page meanwhile | minor | phones | none (new) |
| F13 | Replies lose their context in the log; phone Send reply overflows | minor | all | none (new) |
| F14 | A half-typed message is lost on reload | minor | all | none (new) |
| F15 | Keyboard: two tab stops per message; controls named by message id | minor | 1440 | none (new) |
| F16 | "transmitting" can stay on for 8 s or more after the first push-to-talk | minor | all | none (new) |
| F17 | Phone Voice tab: "Join voice in #general" wraps into two columns | polish | 390 | #647 (spacing only) |
| F18 | Phones are shown keyboard hints (backtick talk key, "Esc to cancel") | polish | 320, 390 | none (new) |
| F19 | Invite page: the Version badge covers text; the name hint still mentions guests | polish | 1440 | none (new) |
| F20 | Notification settings say "off until you choose" after you chose | polish | all | none (new) |
| F21 | Invites: identical rows, unclear "Open", pause not shown, link left on screen, Escape on the toggle | polish | all | none (new) |
| F22 | Release delay slider moves 10 ms per key press over 0–2000 ms | polish | 1440 | none (new) |
| F23 | Members panel: three "Only operators…" lines for members; "Remove owner" on yourself | polish | all | #972, #1020 |
| F24 | The sign-in page shows operator setup to everyone; "Your sign-ins" rows look alike | polish | all | none (new) |
| F25 | Mentions: no completion and no highlight for the person named | polish | all | none (new) |
| F26 | Small things: mixed date formats, a filename shown twice, Esc in search, raw JSON in reports | polish | all | none (new) |

## What was audited

| | |
| --- | --- |
| Build | Tag `v0.1.0-alpha.7` (`d5500a1`), checked out on branch `audit-alpha7/web-engineer`. conchd built with `go build -ldflags "-X main.version=v0.1.0-alpha.7"`; web client `npm ci && npm run build` with Node 24.21.0 (`web/.nvmrc`). Every page shows "Version v0.1.0-alpha.7". |
| Server | The audit's own `conchd serve` on `127.0.0.1:28777`, a fresh data directory in a scratch folder, `--web-dir web/dist`, `--test-relax-invite-limits` (all browsers share one address), `--livekit-room-prefix audit3-`, `CONCHD_TOTP_KEY` set (operator TOTP required, as in production). Stopped and restarted once for the outage test. |
| LiveKit | `livekit/livekit-server:v1.13.7` (the pinned digest), in Docker on host networking, started the way `web/e2e/voice-world.ts` starts it, on ports of its own. |
| Browser | Chromium 149.0.7827.55 headless shell, via `@playwright/test` 1.61.0 (the repository's copy), with `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream`, a 440 Hz tone as the fake microphone, and `--auto-select-desktop-capture-source="Entire screen"`. |
| Viewports | Desktop 1440×900. Phones 390×844 and 320×640, both with `isMobile`, `hasTouch` and scale 2. |
| How it was driven | A throwaway driver (outside the repository's suites) kept one browser context per person. It ran script steps against them, wrote screenshots, and logged console errors, warnings, failed requests and 4xx/5xx answers per flow. |
| When | 21:33–22:40 local time on 2026-10-30. The last half hour was spent waiting out the one-hour edit window. |

**People and channels**, all made through the product:

- **owner**: made with `conchd operator bootstrap`, then the browser's setup page. The password is four letters (`kqvz`). The authenticator codes were computed by the script from the revealed key.
- **alice** (1440): joined by the owner's personal invite, with voice and "Joiners may invite others". 4-letter password.
- **bob** (390×844) and **carol** (320×640): joined by **alice's member invites**, with 4-letter passwords.
- **erin** (320) and **dave** (1440): made accounts at the bare address once the owner turned on "Create an account". Passwords of 2 and 4 characters.
- **helperbot**: an agent principal, made over the API, with a credential, and added to `#general`.
- **Channels**, made by the owner in the browser: `#general`, `#random`, and `#secret` (owner and alice only). Alpha.7 has no public/private flag: membership is the boundary, so `#secret` is private by its members.
- **Messages**: about 40 in `#general` (20 seeded over the API, the rest typed), including one long paragraph and one long URL.

**What this audit cannot show:**

- Real phones: iOS Safari, Android Chrome, on-screen keyboards, `getDisplayMedia` being absent, background tabs.
- Real microphones, headsets, echo, or speech detection. The fake microphone plays a steady tone.
- A LAN or WAN path, TLS, or the Caddy proxy. Every request was to loopback. "Slow network" is Chromium's emulation (2 s latency, about 50 kB/s), which does not throttle WebSockets.
- Browser notification permission. Headless Chromium denies it, so only the in-page notification centre was seen.
- Firefox and WebKit.

## Findings

Severity: **blocker** stops a tester; **major** a tester will regularly go wrong, lose something, or be unable to do a core task; **minor** a real defect or gap with a workaround; **polish** wording, layout or consistency.

### F1 — Someone added to a channel does not see it until they reload

- **Severity:** major. **Viewports:** all (seen at 1440).
- **Steps:**
  1. owner on `#secret` adds alice by name in the members panel. The panel shows "alice online" at once.
  2. alice is signed in at 1440, viewing `#general`.
  3. Watch alice's channel list for 30 s.
  4. owner posts "alice, this is just us two" in `#secret`; wait 3 s.
  5. alice reloads.
- **What happens:**
  - alice's list stays "general | random" for 30 s, and after owner's message too.
  - No unread count and no notification appear.
  - Only a reload shows `#secret`, with its unread count of 1.
  - The events socket has no membership kind (`internal/server/events_ws.go`: run, activity, intervention, channel, message, reactions, message-edited). The page cannot know without re-reading.
- **Expected:** the channel appears in the person's list within a second or two of being added, with its unread count. It goes away the same way when they are removed.
- **Screenshot:** [alice's list 30 s after she was added](browser-ux-audit-3/F01-added-to-secret-not-listed-1440.png).
- **Suggested fix:** a server issue first (parity rule). Send a content-free "your channels changed" signal on the person's events sockets on membership add and remove. The page re-reads `GET /v1/channels` and read-state. #993 deliberately sends nothing for membership changes, so it does not cover this.
- **Existing issue:** none. #993 (directory refresh) excludes membership changes.

### F2 — Changing your password signs this page out

- **Severity:** major. **Viewports:** all (seen at 1440).
- **Steps:** alice: Settings → Password → current `abcd`, new `wxyz` twice → Change password.
- **What happens:** the page drops to the sign-in screen with a red "Signed out: the credential is no longer valid". The form had just said "Changing it ends every other sign-in of yours". The console shows a 401 for `GET /v1/whoami`. It happened on the first and only try. #1000 saw it in 2 of 6 runs.
- **Expected:** stay signed in on this page, with a "Password changed" confirmation.
- **Screenshot:** [after Change password](browser-ux-audit-3/F02-signed-out-after-password-change-1440.png).
- **Suggested fix:** as #1000 says. A 401 for a request sent with a credential that has since been rotated is not a sign-out.
- **Existing issue:** #1000 (open).

### F3 — Phone, in voice: the voice strip covers Chat

- **Severity:** major. **Viewports:** 320×640, 390×844.
- **Steps:**
  1. alice, bob (390) and carol (320) are in voice in `#general`. alice shares her screen.
  2. bob presses Watch, then the Chat tab.
  3. bob presses Stop watching.
  4. carol opens the Chat tab.
- **What happens:** the "compact" strip under Chat shows all of the following:
  - the roster chips;
  - "Connected in #general";
  - "Microphone in use · Not transmitting";
  - six or seven controls (seven while a share is on), in two rows at 320 px;
  - the two-line talk-key hint.

  Measured:

  | Page | Strip | Message log |
  | --- | --- | --- |
  | 390×844, in voice, not watching | 245 px | 275 px |
  | 390×844, watching a share | 177 px + 276 px stage | **120 px** |
  | 320×640, in voice, share on, not watching | 305 px | **96 px** |

  At 320×640 the strip overlaps the composer. The point at the centre of the Send button is the strip (`elementFromPoint`), so Send cannot be tapped; Enter still sends. "Jump to latest" sits over the one message that shows. The phone-layout design (§ "Persistent voice bar") calls the strip compact. The changelog for alpha.6 calls it "a thin bar". #600 bounded it at 200 px at 390×844.
- **Expected:** under Chat and Channels, a one-line bar: where you are, Hold to talk, Mute, Leave, and a way to the Voice tab. The roster, the hint and the share notes stay in the Voice tab. The composer is never covered.
- **Screenshots:** [320, in voice](browser-ux-audit-3/F03-chat-in-voice-320.png), [390, watching](browser-ux-audit-3/F03-chat-watching-390.png), [390, in voice](browser-ux-audit-3/F03-chat-in-voice-390.png).
- **Suggested fix:**
  - Under 700 px on the Chat and Channels tabs, hide the roster, the hint lines and the status pill.
  - Fold the controls into one row of four.
  - Reserve the strip's height in the layout, so it is never drawn over the composer.
  - Add a layout e2e that asserts Send is hittable at 320×640 in voice.
- **Existing issue:** none open. #600 and #641 (closed) set the bound this breaks; #929 (closed) made the tabs.

### F4 — Owner: no browser view of problem reports, and no per-person actions

- **Severity:** major. **Viewports:** all.
- **Steps:**
  1. bob (390): Settings → Report a problem → fill it in → Review report → Send report. He is told "Your report was sent as number 1".
  2. The owner looks for it in the browser.
- **What happens:**
  - The owner's Settings has one server item, **Joining**. The members panel per channel offers Remove, Make manager, and decides approvals. Nothing reads reports: `GET /v1/reports` answers the owner's credential with the report, but no page calls it.
  - Nor can the owner do any of these in the browser: end a person's sign-ins (`…/credentials/revoke-all`), disable a person, or make a reset link for someone who forgot their password. The sign-in page tells that person "ask an operator for a link".
  - All of these are CLI-only today (`conch admin reports`, `conch admin …`).
- **Expected:** an owner who runs the server from a browser can read and close reports and help a locked-out person.
- **Screenshot:** [the owner's Settings](browser-ux-audit-3/F04-owner-settings-1440.png).
- **Suggested fix:** #1020 (admin console Members section) already plans sign-out-everywhere, disable and sign-in/reset links. Add a Reports section to the console: list, read, and mark filed (`GET/PUT /v1/reports`), routes that already exist.
- **Existing issue:** #1020 covers the person actions; nothing covers reports in the browser.

### F5 — Phone top bar hides your name and the connection state

- **Severity:** minor. **Viewports:** 390×844, 320×640.
- **Steps:** sign in as bob at 390, as carol at 320, and as the owner at 390 (four top-bar buttons). Then stop conchd.
- **What happens:**
  - At 390 a member's name shows as "b…".
  - For the owner at 390, and for anyone at 320, the name is gone. The status word is cut by the bell button ("Connect", "Conn").
  - During the outage at 320, "Reconnecting" reads "Reco…g" behind the bell. On a phone the conversation shows no banner, so it is the only outage signal.
- **Expected:** the connection state is always readable. Who you are is one tap away.
- **Screenshots:** [operator at 390](browser-ux-audit-3/F05-operator-topbar-390.png), [outage at 320](browser-ux-audit-3/F05-reconnecting-hidden-320.png), [member at 390](browser-ux-audit-3/F05-name-truncated-390.png).
- **Suggested fix:**
  - Under 700 px, drop the address from the bar and keep the coloured dot with a short word ("Online", "Offline").
  - Move the name into the Settings button's label.
  - Show the stale banner over the conversation on a phone as on a desktop.
- **Existing issue:** none.

### F6 — Edit stays on offer after the ten-edit limit, and the first refusal blames a rate limit

- **Severity:** minor. **Viewports:** all (seen at 1440).
- **Steps:**
  1. alice's message "back" is edited 9 times (over the API, as another tab of hers would) and once in the page.
  2. Right-click it → Edit → type → Enter.
  3. Wait 6 s → Enter.
  4. Cancel → right-click again.
- **What happens:**
  - Edit is offered after ten edits.
  - The first save says "Too many edits. Try again in 5 seconds." The burst of ten tokens is spent.
  - The retry says "This message has been edited the most times allowed."
  - After Cancel, the menu still offers Edit.

  The design (§13.1) says Edit shows only with "fewer than `MAX_MESSAGE_EDITS` edits known". The page counts neither edits seen live nor the refusal.
- **Expected:** the menu stops offering Edit once ten edits are known, and always after a `message_edit_limit_reached` refusal.
- **Screenshot:** [the second refusal](browser-ux-audit-3/F06-edit-limit-1440.png).
- **Suggested fix:**
  - Count `message_edited` frames per message in the reducer.
  - Mark a message final on `message_edit_limit_reached` or `message_edit_window_closed`.
  - On a 429, prefer the limit reason when ten edits are already known.
- **Existing issue:** none (#845 closed).

### F7 — A new owner lands on a blank page; Members says "Connecting…" forever

- **Severity:** minor. **Viewports:** 1440.
- **Steps:** complete operator setup on a fresh server, then sign in.
- **What happens:**
  - The page shows "No channels yet" and a New channel button on the left, an empty dark centre, and "Connecting…" in the members column.
  - "Connecting…" never changes while there is no channel (checked again a minute later).
  - Nothing says what to do first: make a channel, open joining, invite someone.
- **Expected:** a short first-run panel ("Make your first channel", "Invite someone", "Decide how people join") and no endless "Connecting…".
- **Screenshot:** [first landing](browser-ux-audit-3/F07-owner-first-landing-1440.png).
- **Suggested fix:** an empty-server state in the conversation area with those three actions. The members column says "No channel open". #1018's setup card ("How will this server be used?") could live here.
- **Existing issue:** none; #1018 plans a setup card in the admin console, not on the landing.

### F8 — The operator must re-enter password and code minutes after signing in with both

- **Severity:** minor. **Viewports:** all.
- **Steps:** the owner signs in with password and authenticator code. About a minute later, they make their first invite.
- **What happens:** "Confirm it is you … given in the last ten minutes" asks for the password and a code again. It asks again for the first Joining change, 14 minutes later. The sign-in itself does not set the recent proof (`recent_auth_until`).
- **Expected:** a full password-and-code sign-in counts as the proof for its first ten minutes.
- **Screenshot:** [the prompt over the first invite](browser-ux-audit-3/F08-reauth-after-signin-1440.png).
- **Suggested fix:** set `recent_auth_until` on the session credential that `POST /v1/login/totp` issues (a server change). It already binds to the session and is cleared on auth changes.
- **Existing issue:** none.

### F9 — A newcomer is told "No messages … yet. Say hello." in a busy channel

- **Severity:** minor. **Viewports:** all (seen at 320).
- **Steps:** erin makes an account at the bare address (admitted to `#general`, which has about 40 messages).
- **What happens:** "No messages in #general yet. Say hello." The API gives her nothing before she joined. The Joining form says "Choosing a channel does not show its older private history to people who join", so the hiding is by design, but the empty state says something untrue.
- **Expected:** "Messages from before you joined aren't shown." Or the history, as the community model plans.
- **Screenshot:** [erin's first view](browser-ux-audit-3/F09-newcomer-empty-320.png).
- **Suggested fix:** until the community model lands, distinguish "empty" from "nothing visible to you yet" in the empty state.
- **Existing issue:** #986 and #991 (community model: a newcomer "reads its history").

### F10 — The browser can attach only PNG and JPEG; other files arrive as "not supported"

- **Severity:** minor. **Viewports:** all.
- **Steps:**
  1. alice picks `notes.pdf` with Attach images.
  2. carol uploads the same PDF over the API (`POST /v2/channels/general/files`, 201) and posts it.
- **What happens:**
  - alice gets "notes.pdf: only PNG and JPEG images can be attached." GIF and WebP are refused the same way (`accept="image/png,image/jpeg"`).
  - carol's message shows "notes.pdf · This file format is not supported for image preview. · Download". The server and other clients can send a file; the browser can't.
- **Expected:** any file the server takes can be attached. Non-images show as a file chip (name, size, Download), and "not supported" is not shown for an ordinary file.
- **Screenshot:** [the refusal and carol's PDF](browser-ux-audit-3/F10-file-attachment-1440.png).
- **Suggested fix:**
  - Rename the button "Attach", and allow any type within `--max-upload-bytes`.
  - Keep previews for PNG and JPEG (and add GIF and WebP if wanted).
  - Show a file chip for everything else.
- **Existing issue:** none (#762 closed, images only).

### F11 — `:shortcode:` completion misses common names and ranks oddly

- **Severity:** minor. **Viewports:** all.
- **Steps:** type each of these in the composer: `:fir`, `:fire`, `:smile`, `:heart`, `:joy`, `:tada`, `:+1`, `:thumb`, `:ok`.
- **What happens:**
  - `:fir` suggests ":firefighter:" and five skin tones before ":fire_engine:". 🔥 is not among the eight offered.
  - `:smile` offers only ":cat_with_wry_smile:".
  - `:heart` shows no ❤️ in its first five.
  - `:joy` puts 🕹️ ":joystick:" first.
  - `:tada` and `:+1` offer nothing.
  - The catalogue uses CLDR names only; the Slack, GitHub and Discord names people type are missing.
- **Expected:** the common names work (`:+1:`, `:smile:`, `:heart:`, `:tada:`, `:joy:`). An exact or prefix name match ranks first. Skin tones fold under their base emoji.
- **Screenshot:** [`:fir`](browser-ux-audit-3/F11-shortcode-fir-1440.png).
- **Suggested fix:**
  - Add a small alias table (the gemoji or Slack set) to the vendored catalogue's generator.
  - Rank an exact match, then a whole-word prefix, then a substring.
  - Hide skin-tone variants unless typed.
- **Existing issue:** none open (#763 closed).

### F12 — First load: 1.3 MB of uncompressed script and a blank page meanwhile

- **Severity:** minor (major for a phone on a weak connection). **Viewports:** phones.
- **Steps:** a fresh 390×844 context with Chromium's network emulation (2 s latency, about 50 kB/s) opens `/app/`.
- **What happens:**
  - The page is a plain dark rectangle for 30.9 s before the sign-in form appears.
  - `index-*.js` is 1,325,420 bytes, sent without `Content-Encoding` (334 kB gzipped).
  - `index.html`'s `#root` is empty, so nothing shows until the script runs.
  - The deploy `Caddyfile` has no `encode` directive, so production is the same.
- **Expected:** compressed assets, and something on screen at once.
- **Screenshot:** [8 s into the load](browser-ux-audit-3/F12-slow-first-load-390.png).
- **Suggested fix:**
  - Add `encode zstd gzip` to `ops/deploy/Caddyfile`, or have conchd serve precompressed `.gz`/`.br` assets.
  - Put static text ("Loading Conch…") inside `#root` in `index.html`. That is markup, not inline script or style, so the CSP allows it.
  - Consider code-splitting LiveKit and the emoji catalogue (Vite already warns about chunk size).
- **Existing issue:** none.

### F13 — Replies lose their context in the log; phone Send reply overflows

- **Severity:** minor. **Viewports:** all.
- **Steps:** bob (390) taps Reply on alice's "anyone up for game night on friday?" and sends "I am in, bringing snacks". alice watches at 1440.
- **What happens:**
  - alice's log gets "bob: I am in, bringing snacks · Open thread" at the bottom. Nothing says what it answers.
  - The root message shows no "1 reply".
  - On the phone, the thread's Send reply button ends at x 385, past the thread box's edge at 374.
  - Two text boxes are on screen at once: the reply box and the channel composer.
  - The reply box is labelled "Reply to message #6".
- **Expected:** a reply in the log quotes a line of its root ("replying to alice: anyone up for…"). The root shows a reply count. The reply box fits its box and is labelled "Reply to alice".
- **Screenshots:** [desktop log](browser-ux-audit-3/F13-reply-without-context-1440.png), [phone overflow](browser-ux-audit-3/F13-send-reply-overflow-390.png).
- **Suggested fix:**
  - A one-line root excerpt above each reply in the log.
  - A count on the root.
  - Wrap Send reply under the box below 400 px.
  - Name the box by author.
- **Existing issue:** none (#842 closed).

### F14 — A half-typed message is lost on reload

- **Severity:** minor. **Viewports:** all.
- **Steps:**
  1. alice types "a draft I was typing" and reloads.
  2. Separately, she opens Edit on a message, changes it, and reloads.
- **What happens:** both are gone. A draft survives a server outage (good), but not a reload, a crash or a closed tab.
- **Expected:** the draft of each channel survives a reload in this browser.
- **Suggested fix:** keep per-channel drafts in `sessionStorage` (or `localStorage`, keyed by account and channel), cleared on send. Message text is the person's own, so local storage is acceptable.
- **Existing issue:** none.

### F15 — Keyboard: two tab stops per message; controls named by message id

- **Severity:** minor. **Viewports:** 1440.
- **Steps:** alice, with about 30 messages loaded, presses Tab repeatedly from the conversation heading.
- **What happens:**
  - 45 stops through the log (each own message has ⋯ and Reply; others have Reply), then the composer, then Join voice. That is 82 stops within the first 120. The count grows with history.
  - Names are by id: "More actions for message 25", "Reply to message 6", "Open context for message 6", "Edit message 25".
  - The visible label of the reply box is "Reply to message #6".

  Focus is always visible (a 2 px blue outline everywhere, or the composer's ring), and ⋯ becomes visible on focus.
- **Expected:** the log is one tab stop, with arrow keys between messages and Tab into a message's own controls. Names say whose message: "More actions for alice's message, 21:38".
- **Suggested fix:**
  - A roving tabindex in the log (`role="log"` keeps its live region).
  - Build accessible names from author and time.
- **Existing issue:** none.

### F16 — "transmitting" can stay on for 8 s or more after the first push-to-talk

- **Severity:** minor (intermittent). **Viewports:** all.
- **Steps:** two people in voice. One holds Hold to talk (touch, or Space on the focused button) for 1.5 s and lets go. The other watches the roster.
- **What happens:**
  - Normally "transmitting" clears 0.5–0.75 s after release and "speaking" at about 1.0–1.3 s (5 of 7 runs).
  - Twice, on a person's **first** push-to-talk since joining, it stayed on longer: still "transmitting" 3 s after bob's release, and 8 s after alice's. In alice's run "speaking" had cleared at 1.1 s. Both had cleared by the next check, about 30 s later.
  - Not reproduced on demand after a leave and rejoin.
- **Expected:** "transmitting" clears within a second of release every time.
- **Suggested fix:** a guess: the first transmission publishes the microphone track, and the mute that follows races the publish. Log the transmit report and the track mute event with timestamps on the first press, and check them against the poller.
- **Existing issue:** none. #965 (voice clipping) is nearby but different.

### F17 — Phone Voice tab: "Join voice in #general" wraps into two columns

- **Severity:** polish. **Viewports:** 390×844.
- **What happens:** the button's two spans wrap side by side ("Join / voice" beside "in / #general").
- **Screenshot:** [Voice tab](browser-ux-audit-3/F16-join-voice-wrap-390.png).
- **Suggested fix:** stack the two spans, or let them wrap as one line of text.
- **Existing issue:** #647 fixes the double space, not the wrap.

### F18 — Phones are shown keyboard hints

- **Severity:** polish. **Viewports:** 320, 390.
- **What happens:**
  - In voice, the phone shows "Talk key: ` (backtick). It works only while this tab has focus…" under the controls. The same text is in the Chat-tab strip, where it adds two lines to F3.
  - The inline editor says "Editing. Enter to save, Esc to cancel" on a phone, which has no Esc key.
- **Screenshots:** [voice at 320](browser-ux-audit-3/F17-phone-key-hint-320.png), [editor at 390](browser-ux-audit-3/F17-phone-edit-hint-390.png).
- **Suggested fix:** hide the talk-key lines on a coarse pointer, and say "Save or Cancel" on a phone.
- **Existing issue:** none.

### F19 — Invite page: the Version badge covers text; the name hint still mentions guests

- **Severity:** polish. **Viewports:** 1440×900.
- **What happens:**
  - On the invite landing, the Version badge sits over "…add you to its channels." The last word is hidden.
  - The name hint says "Names that begin with “guest” are kept for guests", but guests are off in alpha.7. The same hint is on the bare-address sign-up form.
- **Screenshot:** [invite landing](browser-ux-audit-3/F18-invite-landing-1440.png).
- **Suggested fix:** keep the badge in the card's flow. Drop the guest sentence while guests are off, or say "Names that begin with guest are reserved".
- **Existing issue:** none.

### F20 — Notification settings say "off until you choose" after you chose

- **Severity:** polish. **Viewports:** all.
- **What happens:** with "Mentions only" chosen, the hint below still reads "Notifications are off until you choose a policy." The rest works: a mention in `#random` reached alice's centre as "Mention in #random", and a non-mention did not.
- **Screenshot:** [Settings → Notifications](browser-ux-audit-3/F19-notification-copy-1440.png).
- **Suggested fix:** make the hint follow the choice ("You're alerted when someone writes your name").
- **Existing issue:** none.

### F21 — Invites: identical rows, unclear "Open", pause not shown, link left on screen, Escape on the toggle

- **Severity:** polish. **Viewports:** all.
- **What happens:**
  - Two personal invites to the same channels list as identical rows, "Personal #general, #random **Open** · not used yet". "Open" here means not closed, while "Open link" is a kind.
  - While the owner has paused all invites, alice's panel says nothing until she presses Make invite ("Invites are paused on this server for now."). The list still says "Open · not used yet" for links that cannot be used.
  - "Pause all invites" does not pause bare-address sign-up, which is a separate switch. dave made an account while invites were paused.
  - The owner's "shown only now" link stayed on screen in the open panel, behind Settings, more than 20 minutes after it was made and used.
  - Opening the panel leaves focus on "Invite someone", and Escape there does nothing. Escape closes the panel only once focus is inside it (one Tab). The panel stays open over the members column until clicked away.
- **Screenshot:** [alice's panel while paused](browser-ux-audit-3/F20-invites-paused-1440.png).
- **Suggested fix:**
  - An optional note per invite ("for bob").
  - "Unused" or "Waiting" instead of "Open".
  - A paused banner at the top of the panel.
  - "Pause all invites (sign-up at the address has its own switch below)".
  - Clear the made link when the panel closes or the invite is used.
  - Let Escape on the toggle close the panel, or move focus into it on open.
- **Existing issue:** none.

### F22 — Release delay slider moves 10 ms per key press over 0–2000 ms

- **Severity:** polish. **Viewports:** 1440.
- **What happens:** `step=10` on 0–2000 ms. Five → presses take it from 200 to 250 ms, and reaching 1 s needs 80. The value reads well ("200 ms").
- **Suggested fix:** a step of 50 ms (or 100), with PageUp/PageDown for larger moves.
- **Existing issue:** none.

### F23 — Members panel: three "Only operators…" lines for members; "Remove owner" on yourself

- **Severity:** polish. **Viewports:** all.
- **What happens:**
  - A member's panel opens with three lines: "Only operators and this channel's managers add people. / Only operators choose who manages a channel. / Only operators choose who decides approvals."
  - The owner's panel gives every row three controls, their own row included ("Remove owner", "Make owner a manager").
- **Suggested fix:** one line for members, or none. Per-row actions in a menu, without self-removal for the last owner.
- **Existing issue:** #972 and #1020 (permission-aware member menu).

### F24 — The sign-in page shows operator setup to everyone; "Your sign-ins" rows look alike

- **Severity:** polish. **Viewports:** all.
- **What happens:**
  - Every sign-in page has an "Operators and scripts" section with "Set up an operator account" and a "Credential" paste box. An invited friend has to read past it.
  - "Your sign-ins" lists "password sign-in · since 10/30/2026, 9:49:34 PM" for each browser. You cannot tell the phone from the desktop.
- **Suggested fix:** fold the operator section behind "Operator or script?". Label sign-ins with the browser and platform from the user agent at sign-in, or let people name them.
- **Existing issue:** none.

### F25 — Mentions: no completion and no highlight for the person named

- **Severity:** polish. **Viewports:** all.
- **What happens:**
  - Typing "@bo" offers nothing.
  - bob sees "hey @bob are you there?" as plain text, not marked as being for him.
  - With "Mentions only" on, a mention in another channel does alert (as F20 says).
- **Suggested fix:** complete names from the channel's members, and highlight a message that names the viewer.
- **Existing issue:** none.

### F26 — Small things

- **Severity:** polish.
- **What happens:**
  - Dates and times mix 24-hour and 12-hour formats: "Read at 21:36" beside "expires 11/6/2026, 8:36:08 PM".
  - An image sent with no text gets its filename as the body, so "pasted.png" shows twice.
  - "Sent" stays under every one of your own messages.
  - Esc in the search box clears it, but does not close search.
  - Review report shows the raw JSON to the person sending it.
  - A non-member who opens `#/c/secret` sees `#general`, while the address bar keeps `#/c/secret`. Nothing leaks.
  - URLs in messages are not links.
- **Suggested fix:**
  - One `Intl.DateTimeFormat` for every time on the page.
  - An empty body for an image-only message.
  - Show "Sent" only until the next message.
  - Esc closes search.
  - A readable summary in the report review, with the JSON behind a disclosure.
  - Correct the address to the channel shown.
  - Linkify `http(s)` URLs as text-only anchors with `rel="noopener noreferrer"`.
- **Existing issue:** none.

## Flows walked

| Flow | Viewports | Result |
| --- | --- | --- |
| Owner setup (setup token, 4-letter password, authenticator) and first sign-in | 1440 | Works; the key is hidden until Show the key. First landing: F7. First privileged action: F8 |
| Bare address, sign-up off and on | 1440, 320 | Off: "This server is invite-only." On: "Create an account" only, no guest entry. erin (2-character password) and dave joined. Empty state: F9 |
| Invite → account with a 4-letter password → first landing | 1440, 390, 320 | Works. The landing names the inviter, channels, voice and expiry; the code leaves the address bar at once. It is clear where you are: the heading and the highlighted channel, with focus on the heading. Top bar: F5. Page: F19 |
| Sign out, sign in, wrong password, sign out everywhere | 1440, 390 | Works. "The name or password is not right." keeps the name and focuses the password. Names are matched without case. Sign out everywhere ended a second tab and a phone ("Signed out: the credential is no longer valid") |
| Channel list, switching, unread badges, private channel, member list | all | Badges are live, with the voice count beside them. A non-member never sees `#secret` (also by URL). Adding a member: F1 |
| Posting, multi-line, long text, mentions | 1440, 390, 320 | Shift+Enter makes new lines. Long words and URLs wrap at 320. A 4,000-character draft scrolls inside a three-line box. Mentions: F25 |
| Replies and threads, search | 1440, 390 | Work. Search opens context and returns focus to Search. Replies: F13. Search is per channel (F26) |
| **Editing** | 1440, 390, 320 | ⋯, right-click, long press (500 ms, through Chromium's gesture pipeline) and keyboard (Shift+Tab to ⋯, Enter, Enter) all open the menu. The editor opens inline with the caret at the end. A blank text keeps Save disabled. Others see "(edited)" live. Earlier versions are the author's only, newest first, and focus returns to "(edited)". The window was checked in real time. An editor opened at 21:59 on a message sent at 21:38 was saved at 22:38:52. The answer was "This message can no longer be edited: it is more than an hour old.", the text was kept, and after Cancel the ⋯ was gone from the same long-lived page, and after a reload. With the page clock moved 61 minutes ahead, Edit is hidden too. After dave edited "hello from dave" to "zebra crossing", search found "zebra" and not "hello". Ten-edit limit: F6 |
| Emoji picker and `:shortcode:` | 1440 | The picker opens on its search box; Enter inserts and returns to the composer. Completion: F11 |
| Attachments: upload, paste, a file | 1440 | Upload and paste preview and send; a remove button before sending. Files: F10 |
| Voice: join, leave, push-to-talk (pointer, touch, Space), release delay, open mic, mute, deafen, device picker, indicators | 1440, 390, 320 | All work; three people in one room. Push-to-talk shows on the other side in 0.2–1.2 s. Deafen mutes, and says so. Leave returns focus to Join. F16, F22. Others still cannot see mute (#598, open) |
| Screen share | 1440 (sharer), 390 (watcher) | Works; the watcher has volume, full screen and Stop watching. Share on a phone context worked only because emulated Chromium has `getDisplayMedia`; the code feature-detects it |
| Phone tabs (Chat, Channels, Voice), joining from Voice | 390, 320 | The tabs work. The dot on Voice and "You are in voice" are good. Chat while in voice: F3. Join button: F17 |
| Settings: password (short, mismatch, pattern, name, wrong current), sign out everywhere, notifications | 1440 | Each refusal has its own clear line. Password change: F2. Notifications: F20, F25 |
| Joining (operator), invites (member and operator), pausing | 1440, 390 | Work. A dead link while paused says "Invites are paused on this server for now. Try the link again later." with Try again. F21 |
| Operator: invites, member actions, ending sign-ins, reports | 1440, 390 | Invites and per-channel actions only: F4 |
| Agent principal | 1440 | Shows under "Agents" as offline. It cannot post without a manifest (403 "the agent's manifest does not permit this"), as designed |
| Reload mid-flow | 1440 | The channel is kept (`#/c/random`). The draft and the open editor are lost: F14 |
| Slow network | 390 | F12. After loading, sign-in took 6.4 s; sending showed "Sent" at once |
| Server stopped 20 s, then started | 1440, 390, 320 | "Reconnecting" at once. "This conversation may be out of date". "Sending is paused until the server is back. Your draft is kept." Join voice is disabled with its reason; voice in LiveKit carries on. All four pages were "Connected" 7.9 s after the start, and the draft sent. At 320 the state is hard to read: F5 |
| Two tabs, two devices | 1440, 390 | Both stay in step; sign out everywhere ends both |

## Accessibility basics

- **Keyboard:** every flow above can be done with the keyboard. Focus lands on the conversation heading after sign-in and on Settings' heading when it opens. Escape closes Settings, Members, the emoji picker, Earlier versions, search context and the message menu, and focus returns to the opener each time. The invite panel is the exception (F21). Tab order is long in the log (F15).
- **Focus visibility:** a 2 px `rgb(142,197,255)` outline on every stop. The composer's ring is on its box. No invisible stops.
- **Labels and roles:** the regions and lists are named (Channels, Conversation, Messages, Members list, Voice, Main navigation). Unread and voice counts are `img`s named "1 unread" and "2 in voice". The emoji list is a proper `listbox` with `aria-activedescendant`. Names by message id: F15. axe-core is not a devDependency, so no axe pass was run.
- **Contrast:** a script measured every visible text node against its effective background, on alice's 1440 page, bob's 390, the owner's and the sign-in page. Nothing was under 4.5:1.

## Console and network, per flow

| Flow | What the console showed |
| --- | --- |
| Owner setup | One 401 for `GET /v1/whoami` before sign-in |
| Owner invite, joining | One 403 each: the `reauth_required` that opens "Confirm it is you" (F8) |
| Password | Two 400s (pattern, name), one 401 (wrong current), then a 401 for `GET /v1/whoami` after the change: F2 |
| Sign in, sign out everywhere | One 401 for a wrong password; 401 `whoami` on each page that was signed out |
| Edit limit | One 429 and one 409 for the 11th edit: F6 |
| Paused invites | 503 for `POST /v1/invites` and `POST /v1/invites/preview`. A pause is not an outage; a 409 or 403 would keep it out of the "server error" class in testers' logs |
| Screen share | "Permissions policy violation: camera is not allowed in this document." once per share (kept by decision on #595) |
| Server down | 72 `POST /v1/ws-tickets` connection-refused across four pages in 20 s (the reconnect backoff) |
| Everything else | Nothing: no page errors, no failed requests |

## What works well

- **Editing is complete and careful.** It has four ways in, the editor is inline, focus returns, edits arrive live, and Earlier versions are for the author only. Edit is hidden past the hour, the refusals are honest, and attachments and replies are untouched.
- **Short passwords are clean everywhere.** No mention of 15 characters is left, and each refusal says why in one line.
- **The front door is simpler.** No guest entry, and the bare address offers exactly what the owner chose.
- **The outage behaviour is good.** The state is named, the draft is kept, voice carries on, joining is disabled with its reason, and recovery is quick.
- **Invite landing pages say who invited you, to what, and until when.** The code is gone from the address bar at once. A paused or dead link says so plainly.
- **Contrast and focus rings are consistent** across desktop and phone.
- **Nothing threw.** No uncaught error on any page in about 70 minutes of use by eleven browser contexts.

## Proposed new issues

These cover findings no open issue covers. They are proposals; none was filed.

1. **Server and web: a person's channel list follows membership changes live (F1).** Send a content-free "channels changed" signal on the events socket when a person is added or removed. The page re-reads its channels and read-state.
2. **Web: the phone's voice strip under Chat and Channels is one line and never covers the composer (F3).** Restore #600's bound, add a Send-hittable layout test at 320×640 in voice.
3. **Web: Reports in the admin console (F4).** List, read and mark filed problem reports over the existing `GET/PUT /v1/reports`.
4. **Web: the phone top bar keeps the connection state readable (F5).** No truncated name or hidden "Reconnecting" at 320–390 px; a stale banner on phones.
5. **Web: stop offering Edit after ten known edits or a final refusal (F6).** Count edit frames; prefer the limit reason over a 429.
6. **Web: a first-run state for a new owner (F7).** Make a channel, invite someone, decide joining; no endless "Connecting…".
7. **Server: a password-and-code sign-in counts as the recent proof (F8).** Set `recent_auth_until` on the session that `POST /v1/login/totp` creates.
8. **Web: an empty state that tells "empty" from "history before you joined" (F9),** until the community model makes it moot.
9. **Web: attach any file, with a file chip for non-images (F10).**
10. **Web: shortcode aliases and ranking (F11).** Common Slack/GitHub names, exact-match first, skin tones folded.
11. **Ops and web: compress the client and show something while it loads (F12).** `encode zstd gzip` in the Caddyfile or precompressed assets, a static "Loading…" in `#root`, code-splitting.
12. **Web: replies show their root in the log, and roots show a reply count (F13).** Includes the phone Send reply overflow.
13. **Web: keep each channel's draft across a reload (F14).**
14. **Web: one tab stop for the message log, and names by author and time (F15).**
15. **Voice: find why "transmitting" can linger after the first push-to-talk (F16).**
16. **Web: alpha.7 polish batch (F17–F22, F24–F26).** Join button wrap, phone key hints, invite-page badge and guest copy, notification hint, invite list wording and pause banner, release-delay step, operator section on the sign-in page, sign-in labels, mention completion and highlight, date formats, image-only bodies, "Sent", Esc in search, report review, linkified URLs.
