# Browser client UX audit 4

**Release:** v0.1.0-alpha.8. **Date:** 2026-10-31. **Status:** independent review, for the project owner's approval; nothing here is accepted until then.
**Reviewer:** an AI agent that did not build the client. It drove the running web client in Chromium through Playwright, with temporary users on a disposable local server.
**References:** the [third audit](browser-ux-audit-3.md) (#1026) and its [screenshots](browser-ux-audit-3/), [admin roles](../design/admin-roles.md) (ADR-014), [community model](../design/community-model.md) (ADR-013), [ADR-007](../adr/ADR-007-client-interfaces.md).

Every finding below comes from the running client, with the steps that show it. Guesses are marked as guesses. No product code was changed, and nothing touched the real alpha server.

## Summary

**All seven audit-3 fixes that shipped in alpha.8 hold up.** F1 (#1027), #1000, #1028, F8 (#1033), F11 (#1036), F14 (#1039) and #1066 are each **fixed**. The one qualification is on F14: a half-typed *reply* is not kept across a reload, which #1039 never promised (new F2).

- **Channel list (F1, #1027).** A person added to, removed from or renamed in a channel sees it in about 0.3 s, in both of their sessions, with the unread count. A non-member's socket and a just-removed member's socket carry nothing about it.
- **Password change (#1000).** Five changes in a row, with a second session open, left the page signed in every time, with "Your password is changed. Every other sign-in of yours has ended; this one goes on."
- **Phone voice strip (#1028).** One row of four controls, 44 px tall (61 px with its frame) at 320 and 390 wide, in voice, with a share on and while watching. Send stays tappable.
- **Drafts (F14).** Text, whisper recipients, a net, an open edit and the Escape rule all survive a reload. The page keeps them through an outage and drops them on sign-out.
- **Whisper send (#1066).** A second whisper to the same people never became a public message, with a slow first send, with an image instead of text, and with the first send failing.

**The permissions change (ADR-014) took nothing away.** Every operator action works for the owner and for a second, non-owner operator, and members are refused with a plain line. There is **no blocker**. Section B has the table.

What is new this time (briefly; the audit was broader and shorter than audit 3):

1. **A run of whispers loses its "whisper to" marker** (F1 below). The first message of a run shows it; the next message under its own header does not, so a whisper reads as a public message. The marker also says "whisper to 1", not who.
2. **A half-typed reply is lost on reload** (F2), where a channel draft is kept.
3. Five small things: a fully typed `:tada:` stays literal, phone Reply buttons are 33 px wide, one join error says "Principal", removing a person from a channel and pausing invites ask for no confirmation, and expected states log red console errors.

**Findings: 0 blocker, 0 major, 2 minor, 5 polish (7 in all).**

| # | Finding | Severity | Viewports | Covered by |
| --- | --- | --- | --- | --- |
| F1 | Whisper marker shows on the first message of a run only, and counts instead of naming | minor | all | none (new) |
| F2 | A half-typed reply is lost on reload | minor | all | none (new; #1039 did not promise it) |
| F3 | A fully typed `:tada:` or `:+1:` is sent as that text | polish | all | none (new; #1036 closed) |
| F4 | Phone: the Reply button is 33 px wide | polish | 320, 390 | none (new) |
| F5 | A join error names a "Principal" | polish | all | none (new) |
| F6 | Removing someone from a channel, and pausing all invites, ask for no confirmation | polish | all | none (new) |
| F7 | Expected states write red errors to the console | polish | all | none (new) |

## What was audited

| | |
| --- | --- |
| Build | Tag `v0.1.0-alpha.8` (`6150e9d`), checked out on branch `audit-alpha8/web-engineer`. conchd built with `go build -ldflags "-X main.version=v0.1.0-alpha.8"`; web client `npm ci` then Vite build with Node 24.21.0 (`web/.nvmrc`). Every page shows "Version v0.1.0-alpha.8". |
| Server | The audit's own `conchd serve` on `127.0.0.1:28777`, a fresh data directory in a scratch folder, `--web-dir web/dist`, `--test-relax-invite-limits`, `CONCHD_TOTP_KEY` set (operator code required, as in production), pointed at a local LiveKit. Stopped and restarted three times for outage tests. |
| LiveKit | `livekit/livekit-server:v1.13.7` (the pinned digest) in Docker on host networking, started the way `web/e2e/voice-world.ts` starts it, on ports of its own. |
| Browser | Playwright's Chromium (headless shell) with `--use-fake-ui-for-media-stream --use-fake-device-for-media-stream --auto-select-desktop-capture-source="Entire screen"`. One browser project at a time. |
| Viewports | Desktop 1440×900. Phones 390×844 and 320×640, both with `isMobile` and `hasTouch`, scale 1. |
| How it was driven | Throwaway scripts under `audit/` in the worktree (not part of any suite, no spec added to `web/e2e/`). One browser context per person; each flow logged console errors and warnings, failed requests and 4xx/5xx answers. API calls were made with the audit's own sign-ins. |
| When | About 21:55 to 22:50 local time on 2026-10-31, plus a ten-minute wait for the stale-proof check. |

**People and channels**, all made through the product:

- **owner**: made with `conchd operator bootstrap`, then the browser's setup page: password (`kqvz`), authenticator key read from "Show the key", code computed by the script. No script credential. Every operator check signs in with password and code.
- **alice** (1440): joined by the owner's personal invite, with voice and "Joiners may invite others". Password `abcd`.
- **bob** (390×844) and **carol** (320×640): joined by **alice's member invites** (personal links), with 4-letter passwords. A new account at 320 (`newbie`) was made by the owner's personal invite.
- **helperbot**: an agent principal, made over the API, with a credential, added to `#general`.
- **Channels**: `#general`, `#random`, `#secret` (owner and alice only; membership is the private boundary in alpha.8), plus `#lounge` (later `#lounge2`) and `#tmpchan` made during the checks. A net `ops` in `#general` (alice and bob).
- **A second operator** (not the owner): carol was promoted, given `conchd operator setup`'s token, and set up through the browser flow, to check what an operator who is not the owner can do. She was demoted afterwards.

**What this audit cannot show:**

- Real phones: iOS Safari, Android Chrome, on-screen keyboards, `getDisplayMedia` being absent, background tabs, orientation changes.
- Real microphones, headsets, echo or speech detection. The fake microphone plays a steady tone.
- A LAN or WAN path, TLS, or the Caddy proxy. Every request was to loopback. Throttling was Chromium's emulation (400 ms latency, about 150 kbit/s down), which does not slow WebSockets.
- Browser notification permission. Headless Chromium denies it, so only the in-page centre was seen.
- Firefox and WebKit.
- The alpha.7 behaviour side by side: Section B compares against alpha.7 by reading the code (`git diff v0.1.0-alpha.7 v0.1.0-alpha.8` changes no route or access class in `internal/server/routes.go`), not by running both servers.

## A. Verification of audit-3 fixes

| Fix | Verdict | Evidence |
| --- | --- | --- |
| **F1, #1027** the channel list follows membership | **fixed** | See below. |
| **#1000** a password change does not sign the page out | **fixed** | 5 of 5 changes in a row, and the restoring sixth, stayed signed in. |
| **#1028** the phone voice strip | **fixed** | One row, 44 px, Send tappable, at 320 and 390. |
| **F8, #1033** no second proof within 10 minutes of a password-and-code sign-in | **fixed** | No prompt, also after a reload. |
| **F11, #1036** `:+1:`, `:thumbsup:`, `:heart:`, `:tada:` and ranking | **fixed** | All four complete first or near it; one cosmetic gap (F3). |
| **F14, #1039** drafts survive a reload | **fixed** (replies are out of its scope; see F2) | Each case below. |
| **#1066** a second whisper to the same people | **fixed** | Three variants stayed whispers. |

### F1, #1027: the channel list follows membership — fixed

Four sessions, all signed in: alice at 1440 twice (two contexts), bob at 390, carol at 320. The owner acted over the API. The page was polled every 100 ms.

| Change | alice (2 sessions) | Result |
| --- | --- | --- |
| Owner creates `#lounge`; alice is not in it | no change for 1.5 s | correct: nothing shown to a non-member |
| Owner adds alice to `#lounge` | both sessions list it in 0.3 s | with an unread count of 1 after the owner's post |
| Owner renames it to `lounge2` | both sessions show the new name in 0.3 s | unread count kept |
| Owner removes alice | both sessions drop it in 0.3 s | |
| Owner adds bob (390) to private `#secret`, then removes him | appears in 0.3 s, gone in 0.3 s | carol (320) saw nothing either time |

[Screenshot: alice's list right after she was added](browser-ux-audit-4/A-f1-added-live-1440.png).

Leaks:

- **A non-member's socket.** carol's page received 4 frames in the whole run (voice occupancy, voice presence and the session roster of `#general`). None mentioned `lounge` or `secret`. The same for bob while `#lounge` changed.
- **A removed member.** alice was viewing `#secret` when she was removed. The page showed a red "You no longer have access to #secret", moved her to `#general`, and the owner's next post in `#secret` reached none of her frames or her page. Her API read of `#secret` answers 404.
- **A whisper.** alice whispered to the owner only; bob's frames, page and history read carried none of it.

### #1000: password change — fixed

Settings → Password, as alice with a second session open. Five changes in a row (and a sixth that put the password back), each with the current password, a new one and a repeat. The page stayed signed in every time and showed "Your password is changed. Every other sign-in of yours has ended; this one goes on." The second session dropped to the sign-in page with "Signed out: the credential is no longer valid". The console does write a few 401s and WebSocket 404s at each change (F7).

### #1028: phone voice strip — fixed

bob (390×844) and carol (320×640) in voice in `#general` with alice at 1440. Measured on the page in three states, under Chat and Channels.

| State | Size | Strip (`.dock` / controls) | Message log | Send reached by a tap at its centre |
| --- | --- | --- | --- | --- |
| in voice | 390 | 61 / 44 px | 476 px | yes |
| in voice | 320 | 61 / 44 px | 272 px | yes |
| alice shares, not watching | 390, 320 | 61 / 44 px | 476, 272 px | yes |
| watching the share | 390 | 61 / 44 px | 242 px | yes |
| watching the share | 320 | 61 / 44 px | 120 px | yes |

The strip holds four controls on one row: "In voice in #general", Hold to talk, Mute, Leave voice. The roster, hint and share notes are in the Voice tab. No horizontal overflow at either size. Screenshots: [320 watching](browser-ux-audit-4/A-1028-strip-320-watching.png), [390 in voice](browser-ux-audit-4/A-1028-strip-390-in-voice.png). At 320 while watching, the log is 120 px: that is #641's bound, not a regression.

### F8, #1033: no second proof within ten minutes — fixed

The owner signed in with password and code in the browser, then at once opened Settings → Joining, ticked "Pause joining from the address" and pressed Save joining: no "Confirm it is you" dialog. After a reload (still inside ten minutes) the same save asked nothing either. `whoami` showed `recent_auth_until` ten minutes after the sign-in, and the session token did not change on reload. The stale case is in Section B.

### F11, #1036: shortcodes — fixed

In the composer, the first suggestion for each typed query (min 2 characters):

| Typed | First suggestions |
| --- | --- |
| `:+1` | 👍 thumbs_up |
| `:thumbsup`, `:thumbs_up` | 👍 thumbs_up |
| `:thumbs` | 👍 thumbs_up, 👎 thumbs_down |
| `:heart` | ❤️ red_heart, 😍, ♥️ heart_suit, 🫶 |
| `:tada` | 🎉 party_popper |
| `:party` | 🥳 partying_face, 🎉 party_popper |
| `:fire`, `:joy`, `:pray`, `:clap`, `:sob`, `:100`, `:ok`, `:wave`, `:rocket` | the obvious emoji first |

In the picker, "thumbs up", "red heart", "party popper", "crying face", "face with tears" and "smiling face" each put the exact name first; `+1` and `tada` find the same emoji. Enter on `:+1` writes 👍. One gap: F3.

### F14, #1039: drafts survive a reload — fixed

Each case was typed, reloaded, and the composer read back.

| Case | Result |
| --- | --- |
| Plain text, one channel; a second draft in another channel; switching back | kept, kept per channel |
| Send, then reload | empty composer |
| **Whisper** to bob and carol | back as a whisper, both ticked, text intact |
| Escape on the restored whisper, twice | the audience did not change; still "Whisper to…" with both ticked |
| **Net whisper** to `ops` | back as `net ops`; Escape did not widen it |
| **Open edit**, edited text unsaved | the editor reopened with the text; Escape closed it |
| **Sign out** | storage holds no draft; the next sign-in (alice, then bob in the same tab) starts empty |
| **Outage** (server stopped for 20 s) | the draft and recipients stayed; the composer said "Sending is paused until the server is back. Your draft is kept."; it was still there, and sendable, once the server returned |
| **A whisper recipient who left** (bob removed from `#general` while alice's draft waited) | on reload the text stayed, nobody was ticked, Send was disabled, and the page said "Who this draft was for is no longer available. Choose who it goes to before sending." Escape and Send changed nothing |
| A net member who left | the net draft is kept as the net; narrower, never wider |

[Screenshot: the recipient who left](browser-ux-audit-4/A-f14-recipient-left-1440.png). Not kept: a half-typed *reply* (F2).

### #1066: a second whisper while the first sends — fixed

With the POST for `#general` held for 5 s in the browser:

1. **Second whisper typed.** alice whispered "A" to bob and pressed Send, then at once picked Whisper to… → bob and typed "B". When A was answered, the composer was still a whisper to bob with "B" in it. B went out with `audience: principals [3]`.
2. **An image instead of typing.** After "C" was sent, she attached `t.png` and picked bob again. The composer stayed a whisper, and the image went out as `principals [3]`.
3. **The first send fails** (the held POST is answered 503). "D" showed Retry; the new draft "E" stayed a whisper to bob; Retry re-sent D to `principals [3]`.

No post in any variant had an audience wider than bob.

## B. Regressions from the permissions change (ADR-014)

The permissions table ships empty. `git diff v0.1.0-alpha.7 v0.1.0-alpha.8 -- internal/server/routes.go` is empty: no route changed its access class. Each action was run as the owner, over the API and (for the first six rows) in the browser, then as a plain member and as a member allowed to invite. alice made invites; bob was refused.

**No blocker: nothing an operator could do before is refused now.**

| Action | Owner / operator | Member refused with |
| --- | --- | --- |
| Make an open or personal invite (browser: Invite someone → Make invite) | works, link shown | "you may not make this kind of invite" (403) |
| Close an invite | works | – |
| Pause and un-pause all invites (browser: Settings → Joining → Pause all invites) | works at once; invites then answer 503 "invites are paused on this server" to everyone, owner too | "operator role required" (403) |
| Joining settings (browser: address offers, channels, Pause joining; Save joining) | works; a change is stored | "owner required" (403) |
| Channel members: add by name, manage, remove (browser: People) | works | "operator role or channel manager required" |
| Rename a channel (browser) | works | – |
| Create a channel, create a net | works | "operator role required" |
| Member list: `GET /v1/principals` | works | "operator role required" |
| Change a role (owner only) | works both ways | "owner required" |
| Rename a person; disable and enable; hide; invite right | works | "operator role or the principal itself required" for rename; "operator role required" otherwise |
| End sign-ins: browser "End this sign-in" (asks "Yes, end it"); revoke-all; sign-in links | works | "operator role or the principal itself required" |
| Reports: a member files, the operator lists, opens and marks one filed (API) | works | list and mark: "operator role required" |
| Audit export | works for the owner | "owner required" |

A second operator who is not the owner (carol, set up through the real flow) could list people, create channels and invites, pause invites, read reports, rename, disable and enable, add and remove channel members and revoke sign-ins. The owner-only routes (saving join settings, the audit export, role changes) answered 403 to her, as to a member.

Stale proof (the owner's session older than ten minutes): see the note at the end of this section.

Other things seen:

- `GET /v1/permissions` answers 404 and `whoami` has no `permissions` field. That is expected: #1015 and #1016 are open.
- The refusals are plain but terse: "operator role required" does not say who to ask. That is the same wording as in alpha.7 (the strings are in the code unchanged).
- Browser parity gap, known: there is no list of people with role, disable and rename in the browser (#1020, #972), and no reports view (#1029).
- A demoted operator keeps the authenticator step at sign-in (carol, demoted, still got a code prompt). This looks deliberate.

**Stale proof, not walked in the browser.** Adding a person to a channel over the API with an owner session that was more than ten minutes old answered 403, where the same call with a fresh sign-in answered 204 (checked twice). That is the server half of "Confirm it is you". The browser half (the dialog, over the form, keeping what was chosen) was set up as a ten-minute wait, but the script that was to do it failed on a selector of its own, so it was not seen here. `web/e2e/23-operator-auth.spec.ts` covers it; it should be checked in the next pass.

## New findings

Severity: **blocker** stops a tester; **major** a tester will regularly go wrong, lose something, or be unable to do a core task; **minor** a real defect or gap with a workaround; **polish** wording, layout or consistency.

### F1 — A run of whispers loses its "whisper to" marker, and the marker counts instead of naming

- **Severity:** minor. **Viewports:** all (seen as the sender at 1440).
- **Steps:**
  1. alice picks Whisper to… → bob and sends "W1", "W2", "W3", one after the other.
  2. Look at the log, as alice and as bob. Reload and look again.
- **What happens:**
  - "W1" has the amber "→ whisper to 1" badge. "W2" and "W3" do not, even where W2 starts a new header because the minute changed. Under a new header with an avatar and no badge, a whisper looks like a message to the channel.
  - It is the same after a reload, so it is not a live-update lag.
  - A whisper that follows a public message does show it (checked with public and whispered posts taking turns, as alice and as bob at 390).
  - The badge says "whisper to 1" to both the sender and the recipient. It does not name anyone, and bob (the recipient) is not told it was to him.
- **Expected:** every message that went to fewer than the whole channel says so, on its own row or under a header that visibly covers it, and names who it went to ("whisper to bob", or "whisper to you" for the recipient). A long run must not scroll the marker out of sight.
- **Screenshot:** [W1 to W3 right after sending](browser-ux-audit-4/F01-whisper-badge-run-1440.png).
- **Suggested fix:** draw the badge on every row whose audience is not the channel, or at least on every row that gets its own header; show names (or "you") from the roster the page already has.
- **Existing issue:** none.

### F2 — A half-typed reply is lost on reload

- **Severity:** minor. **Viewports:** all (seen at 1440).
- **Steps:** alice presses Reply on a message, types "half-typed reply" in the reply box, reloads, and presses Reply on the same message.
- **What happens:** the thread panel is closed after the reload, and the reply box is empty when reopened. The channel draft, a whisper, a net and an open edit are all kept (F14).
- **Expected:** the reply draft is kept the same way, by thread root, or the page says it will not be. #1039 listed only the channel draft and the edit, so this is a gap, not a regression.
- **Suggested fix:** keep `{root message id → text}` in the same `sessionStorage` record, and drop it on send and sign-out as the others are.
- **Existing issue:** none (#1039 is closed).

### F3 — A fully typed `:tada:` or `:+1:` is sent as that text

- **Severity:** polish. **Viewports:** all.
- **Steps:** type `x :tada:` or `:+1:` or `:thumbsup:` in the composer. Enter sends.
- **What happens:** the suggestion list closes when the closing colon is typed and the text stays `:tada:`. The message arrives as the literal characters. The completion itself, before the closing colon, is correct.
- **Expected:** most people finish a shortcode with the colon, as in other chat apps; either swap a known name for its emoji, or keep the list open so Enter picks it.
- **Suggested fix:** in `completionAt`, treat `:name:` of a known alias or shortcode as a pending completion, and replace it when the colon is typed.
- **Existing issue:** none.

### F4 — Phone: the Reply button is 33 px wide

- **Severity:** polish. **Viewports:** 320, 390.
- **Steps:** open `#general` with a few messages on a phone and measure the buttons.
- **What happens:** every "Reply" under a message is 33×44 px. The rest of the phone controls, including the strip, are 44 px each way. The "⋯" menu button next to it is fine.
- **Expected:** at least 44 px each way, as the rest.
- **Existing issue:** none (#1038 covers the reply form's fit, not this button).

### F5 — A join error names a "Principal"

- **Severity:** polish. **Viewports:** all.
- **Steps:** open an invite and choose a name with an invisible character in it (U+200B).
- **What happens:** the form says "Principal name must not contain invisible formatting characters." The hint above the field already says "no invisible characters" and calls it a name.
- **Expected:** "A name cannot contain invisible characters."
- **Existing issue:** none (the other join errors read well: "That name is taken. Choose another.", "That password is too common, a repeated pattern such as aaaa, blank, or has your name in it.").

### F6 — Removing someone from a channel, and pausing all invites, ask for no confirmation

- **Severity:** polish. **Viewports:** all.
- **Steps:** as the owner, in a channel's People list press "Remove bob", which sits next to "Make bob a manager". In Settings → Joining press "Pause all invites".
- **What happens:** both act at once. The removal ends bob's view of the channel at once ; pausing stops every invite on the server, including the ones people were just sent. The invite pause shows no further sign than the switch.
- **Expected:** a short confirm for the removal in a private channel, and for the pause, a line saying what it stops (the form already says that above the switch). "End this sign-in" and "Sign out" already ask.
- **Existing issue:** none (#1042's F21 covers that the invite panel does not show a pause).

### F7 — Expected states write red errors to the console

- **Severity:** polish. **Viewports:** all.
- **What happens** (each is a real, handled state, shown as an error in the console and in any monitoring that listens):
  - Every signed-out load of `/app/` logs "Failed to load resource: 401" for `GET /v1/whoami`.
  - A password change, with the page staying signed in, logs four 401s for `whoami`, three for `GET /v1/channels/general/voice` and three WebSocket handshake 404s for `/v1/events/ws`.
  - Being removed from a channel you are viewing logs a 404 for `GET /v1/channels/<name>/voice`.
  - Starting a screen share logs "Permissions policy violation: camera is not allowed in this document."
  - A stopped server gives about 16 `POST /v1/ws-tickets` `ERR_CONNECTION_REFUSED` in 30 s: one every 2 s, which is a fair back-off, but each is a red error.
- **Expected:** a probe that is expected to fail, and a retry that is expected to refuse, stay quiet; the share request asks only for what it needs.
- **Existing issue:** none.

## Known, still present

Seen again in alpha.8 and unchanged. Open issues track them.

| What was seen | Issue |
| --- | --- |
| The owner has no browser view of problem reports. The API lists and files them (checked), the browser does not | #1029 |
| The phone top bar hides the connection state. (Not re-measured here.) | #1030 |
| The browser attaches PNG and JPEG only: `notes.txt` is refused with "only PNG and JPEG images can be attached." | #1035 |
| A new owner lands on "No channels yet" with "Connecting…" under it | #1032 |
| First load on a throttled connection (150 kbit/s, 400 ms): about 10 s before any text appears | #1037 |
| Two tab stops per message (Reply and More actions), named by message number | #1040 |
| The invite panel does not say that invites are paused; its rows read "Open" for a status and "Open link" for a kind; "Your sign-ins" rows differ only in a time | #1042 (F21, F24) |
| Three "Only operators…" lines show to a member; "Remove owner" on yourself | #972, #1020 |
| Others cannot see that someone is muted or deafened in voice | #598 |
| A person's role, disable and rename have no browser surface | #1020 |

Not re-checked, and not seen to change: #1031 (the Edit limit), #1034, #1038, #1041 (the "transmitting" lag: after release the line read "Not transmitting" within 300 ms on the first try, but only one press was tried), #1043, #1044.

## C. General pass

- **Onboarding.** The owner's setup (token, password, key, code, sign-in with a code) worked as described. A bogus invite link says "This link can't be used. Ask the person who sent it for a new one. If you already joined from this link, sign in instead." A taken name, a repeated-pattern password, a mismatch, a password equal to the name each give a clear line. Ten wrong sign-ins in a row all say "The name or password is not right." and are not locked out (that is a server question, not a page one). A second tab in the same browser opens signed in (a lasting sign-in); drafts are per tab by design.
- **Channels.** Create, rename, add and remove work. The unread count shows on a non-open channel. An access loss while viewing is handled (F1 above).
- **Messaging.** Posting, Enter and Shift+Enter, a blank draft keeps Send disabled, a 2500-character word and hostile text (`<img onerror>`, `<b>`, markdown) render as text with no overflow at 390. An edit saves with Enter, others see "(edited)". Search finds a message and "Open context". An image attaches. Replies and the thread panel work. The emoji picker and completion work (F3).
- **Voice.** Join, Mute, Deafen (it explains "Muted by deafen"), Hold to talk (the line says "Transmitting" while held and "Not transmitting" 300 ms after), Leave, Voice options and the device list all worked, with a roster that followed within a second. A reload leaves voice, as expected. No console error except F7's.
- **Settings.** Every section opened at 320 and 390 with no horizontal overflow, and the invite, notification and members panels did too. Escape closes the Settings, Notifications and Invite panels and returns focus to the button that opened each.
- **Accessibility basics.** Every visible control has an accessible name; landmarks are in place (`main`, `nav` "Channels", `aside`, labelled sections); the page has an `h1`; `lang="en"`; the sign-in page is one `main`. There is no skip link; the first Tab from a fresh load lands on Search and not on the header controls (not scored: #1040 shapes this).
- **Robustness.**
  - **Reload:** drafts, channel and sign-in come back (F14).
  - **A stopped server:** "Reconnecting" shows in about 0.3 s. After the server returned, the page was Connected within about a second at the 30 s mark, and a message sent after the restart arrived. No sign-out.
  - **Two tabs** of one person: both signed in; a second session shows as "(2 sessions)" in People.
  - **A slow network:** see the table of known items. A slow first send did not lose or widen anything (#1066 above).
  - **Console and failed requests:** none from any flow other than those in F7. No `pageerror` anywhere.

## Proposed new issues

Proposals; none was filed.

1. **Web: every message sent to fewer than the channel shows who it went to** (F1). Badge on every such row, with names, "you" for the recipient.
2. **Web: keep a half-typed reply across a reload** (F2). Same store as #1039.
3. **Web: swap a typed `:name:` for its emoji, or keep the list open on the closing colon** (F3).
4. **Web: a 44 px Reply button on phones** (F4). Fold into #1038 if it is still open when this is read.
5. **Web: the join form says "name", not "Principal"** (F5). The server string, or a map at the client.
6. **Web: confirm removing someone from a private channel, and say what pausing invites stops** (F6).
7. **Web: stop logging handled 401, 404 and refused-connection answers as console errors** (F7).

## What works well

- **Live membership** (F1 of audit 3): 0.3 s, in every session of the person, with no signal to anyone else.
- **Drafts that do not widen.** The whisper that came back held, the one whose recipient left, and the one a slow send could have overwritten: none became public by any path tried.
- **The voice strip on a phone**: one tidy row at 320, with the log and Send left alone.
- **Sign-in and join errors** say what to do.
- **The operator path**: password and code, with the proof carried for ten minutes, and "Confirm it is you" over the form when it is needed.
- **Outage handling**: quick "Reconnecting", a paused composer that says its draft is kept, a recovery of about a second, no sign-out.
- **Message bodies** are text: hostile input is shown, never run.
