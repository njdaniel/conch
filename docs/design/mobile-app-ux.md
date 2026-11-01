# Phone app: interaction specification

**Issue:** #1107 (M2 of #757). **Status:** proposal for Nick's review; design only. Nothing here is built, and no production UI, dependency or schema changes with it.
**Prototype:** [`mockups/mobile-app/`](mockups/mobile-app/README.md), illustrative, opens from disk.
**References:** [phone browser layout](mobile-browser-layout.md) (the approved direction), [client experience](client-experience.md) (UX-01 to UX-23), [community model](community-model.md) and [ADR-013](../adr/ADR-013-one-community-model.md), [ADR-007](../adr/ADR-007-client-interfaces.md), [ADR-008](../adr/ADR-008-private-communications-alpha.md), [ADR-000](../adr/ADR-000-charter.md), `web/src/phone.ts`, and the browser's 390 × 844 specs under `web/e2e/` (`12-phone`, `28-replies`, `35-phone-voice-strip`, `36-member-menu`).

This note turns the approved phone browser direction (Nick, 2026-10-28: one primary panel at a time, bottom navigation **Chat / Channels / Voice**, compact persistent voice controls) into what a native phone app must do, screen by screen. It is **stack-neutral**: it names behaviour, not frameworks. Where a sentence depends on the architecture decision of M1 ([#1106](https://github.com/njdaniel/conch/issues/1106), `docs/design/mobile-app.md`) it says "M1", and where it depends on push design it says "M3" (#1108). Neither document existed when this was written, so none of this is checked against them.

## 0. How to read the evidence

| Label | Meaning |
| --- | --- |
| **Existing** | An operation, schema type or browser behaviour I read in `main` (`7472b65`, 2026-11-01, which includes `ebc66c6`): `web/src/platform/operations.ts`, `web/src/schema/generated.ts`, `web/src/voice/*`, the e2e specs. |
| **Prototype** | Checked by script in the mockup, in headless Chromium on one machine. It is a layout and logic check of a mockup. |
| **Assumption** | My knowledge of iOS and Android platform behaviour, or a design choice, not verified here. Each carries "(assumption)". |
| **Future** | Not implemented anywhere. Named in section 11, each a future issue; none is filed by this PR. |

Nothing in this note was tested on a phone, with a screen reader, or with a real on-screen keyboard. Section 12 lists what that leaves for M4, M5 and the private-alpha records.

## 1. Scope

In: the first usable phone experience of #757: open a member invite, confirm the server, create an account or sign in, browse the channels you are in, read and send text and files, explicitly join voice, talk, switch to another channel, leave, switch server, sign out. Failure and recovery states for each.

Out, as in #757 and #1107: guests (no guest entry point, ADR-013 amendment of 2026-10-30), human DMs, camera or video, administration (managing channels, members, invites, policies), screen broadcasting from the phone, SSO, approval decisions on the phone (they stay in `conch`/ntfy), and global or background hardware talk keys. Viewing a shared screen is #1118's and is not drawn here. An operator can sign in (the browser's password plus code flow is Existing) but the phone app draws no operator screens.

## 2. Rules every screen obeys

These restate accepted behaviour (UX-02, UX-04, UX-05, UX-09 to UX-12, UX-23; browser layout section 4) so the phone cannot drift from it.

| ID | Rule |
| --- | --- |
| **MUX-01** | **Browsing never joins or retargets voice.** Opening a channel, a view, Members, Settings, a notification or a link changes what is displayed. Joining voice is only a tap on a Join control, which names the channel in words. Changing the voice channel means Leave, then Join (see section 7.5). Prototype: opening `#lounge` from Channels shows no strip; opening `#general` while in `#lounge` leaves the strip on `#lounge`. |
| **MUX-02** | **Capture is not transmission.** Two lines, always separate: the microphone ("Microphone in use" or "Microphone off", with the reason) and the sending ("Transmitting" or "Not transmitting"). These are the browser's words (browser layout section 2; UX-11). Never "Mic idle" or "Microphone inactive" while the microphone is open. |
| **MUX-03** | **No automatic unmute, no stale talk.** Mute, deafen, listen only and "joined without microphone" survive an interruption as they were. Talking is a held gesture that ends on release, cancel, loss of focus, lock, call, route change or app switch; the next press starts it fresh. Undeafen restores the earlier explicit mute, never an open microphone. |
| **MUX-04** | **Honest state.** Connection (text) and voice are shown separately. Stale history is marked stale. No fabricated names, counts or presence. An unavailable thing says why. Cached access is never shown as current membership. |
| **MUX-05** | **The server decides.** A hidden control is never the enforcement. Visibility, voice entitlement and the publish grant come from the server; the app displays them. A deleted channel and a channel you may not see read the same (404, section 6.4). |
| **MUX-06** | **No silent work.** No permission is requested at launch. The microphone is requested at the first Join with a microphone. Notifications are requested only from Settings, by a tap. The clipboard is read only when you tap Paste. |
| **MUX-07** | **Nothing sensitive in the open.** An invite code and a credential are handled like a password: not logged, not shown after use, not kept in a screenshot-visible field longer than needed, never in analytics or crash text. Storage is M1's (secure device storage). |

## 3. Screens and navigation

Three **views**, in a bottom bar on a phone held upright and a rail on the leading edge when held on its side: **Chat**, **Channels**, **Voice**, each at least 44 px high and wide, the current one marked (`aria-current` or the platform's "selected"), and a dot plus the words "You are in voice in #channel" on **Voice** while in voice. Everything else is a **page** or a **sheet**, reached from a top bar or an action.

| Screen (route in the prototype) | Kind | Reached from | Leaves to |
| --- | --- | --- | --- |
| Invite link in a browser (`link`) | web page | Tapping an invite link | Confirm (app opens), store, paste |
| Add a server (`paste`) | page | First launch with no server; Servers sheet | Confirm |
| Confirm the server (`confirm`) | page | Link opened in the app; Add a server | Create account; Add a server |
| Create your account (`account`) | page | Confirm | Channels |
| Sign in (`signin`) | page | Launch with a saved, signed-out server; Sign out; revoked | Channels |
| Channels (`channels`) | view | Navigation | Chat |
| Chat (`chat`) | view | Navigation; choosing a channel | thread, sheets |
| Voice (`voice` and states) | view | Navigation; the strip's destination button | Voice options |
| Servers and accounts (`servers`) | sheet | The top bar's servers button; Settings | Switch warning; Add a server |
| Settings (`settings`) | page | The top bar | Voice options; Sign out |
| Channel not available (`gone`) | page | A stale link or notification | Channels |
| Signed out by the server (`revoked`) | page | A refused request | Sign in |

The top bar carries the **screen's title as a heading** (the channel as `#general`, the page's name), the server's name and your account on a second line, and at most two icon buttons (Servers or Members, Settings), each 44 px. The title is where focus lands on arrival (section 9.2). On a view the bar is part of the screen, not an overlay.

**Arrival.** After sign-in the app opens the last accessible channel in Chat, else the first, else a "You're not in any channels yet" state (the browser's first-run screen, #1032, UX-04): it explains membership is needed and offers only what the account can do (Existing: make a channel is operator-only today; the browser's page words are reused). The microphone is off and the Voice view says "Not in voice" even when others are talking.

## 4. The flow: invitation to leave

### 4.1 Steps

| # | Screen | The person | The app | Operation (Existing unless marked) | Failure and recovery |
| --- | --- | --- | --- | --- | --- |
| 1 | Link in a browser | Taps an invite link `https://host/app/#/invite/<code>` | Nothing yet. The page offers **Open in Conch**, **Get the Conch app**, **Copy invite link**, **Continue in this browser** | The link format is the browser's (`web/src/route.ts`: the code is in the fragment, so no server or proxy sees it) | "Nothing opened" is said on the page if the app is absent (prototype). |
| 2a | App installed | **Open in Conch** | The app opens at Confirm with the code in memory only | Hand-off from the page to the app: **Future F1** | If nothing opens, step 1's wording; the manual path is 2c. |
| 2b | App not installed | **Get the Conch app**, then returns | The store page is M1/M6's to decide; this note names no store or distribution | none | After installing, the person copies the link and uses 2c. |
| 2c | Add a server | Pastes the link (or types the server address) | Validates locally, reads the clipboard only on **Paste from clipboard** | none until Continue | Empty, unparsable, or plain HTTP to a non-local host: an inline error beside the field, announced; the field keeps its text. HTTP is refused before any password is typed, as the browser does (`needsHttps`: the server refuses it too, 403 `https_required`). |
| 3 | Confirm | Reads the server, taps **Join this server** | Shows the address (large, never abbreviated), the server's name, who invited you and the channels you start in; says "Only continue if someone you know sent you this link" | `POST /v1/invites/preview` (no auth) gives inviter, channels, voice and expiry; `GET /v1/join` gives `server_name`, whether accounts and password sign-in are allowed. The community slices of #976 may add a v2 preview that names the default channels: recheck at M7 dispatch | Unreachable: "Cannot reach the server. Check your connection and try again." with Try again. A used, expired or closed code reads "This invite no longer works" and offers Sign in or Add a server (the browser's one answer, so a code's state is not an oracle). Paused: "Invites are paused on this server for now." Rate limited: "Try again in N minutes." |
| 4 | Create account | Name, password twice, **Create account** | Creates the account and keeps the credential in secure storage (M1). No guest choice is offered | `POST /v2/invites/redeem` (`code`, `name`, `password`); the credential is in the answer | Name taken, password too short or common, password mismatch: inline errors tied to their fields, focus on the first. The server's word on "too short or too common" is shown, not a rule invented by the app. Offline: the typed name is kept. |
| 4b | Sign in | A returning member: name, password | Same storage | `POST /v1/login`; an operator then sends a code, `POST /v1/login/totp` | One refusal for any wrong name or password ("That name or password is not right."): the browser's no-oracle behaviour. Sign-in link: `POST /v1/sign-in-links/redeem`. |
| 5 | Chat or Channels | Reads, taps channels, writes | Opens the last or first channel in Chat; the Channels view lists the channels the account is in | `GET /v1/whoami`, `GET /v1/channels`, `GET /v1/read-state`, `GET /v2/channels/{c}/messages`, live `GET /v2/ws` after `POST /v1/ws-tickets` | Section 5. |
| 6 | Voice | Opens **Voice**, taps **Join voice in #channel** | Asks the system for the microphone if not yet decided; connects; shows "Connecting to #channel" then "In voice in #channel" | `POST /v1/channels/{c}/voice/session` (the answer says whether you may publish), presence `GET /v1/channels/{c}/voice`, occupancy `GET /v1/voice/occupancy` | Section 7. Joining is never automatic: not after sign-in, not on opening a channel, not on resuming. |
| 7 | Leave | **Leave voice** (in the strip or the Voice view) | Stops capture and playback, ends the session, returns to "Not in voice"; text carries on; focus goes to Join | Client-local: ends the media connection. The server learns by presence leaving (no leave operation exists in `operations.ts`) | Leaving needs no confirmation: it is reversible and always safe. Leaving while offline still stops local media at once. |

**Installed or not (assumption).** I do not rely on the OS opening the app from a link for self-hosted servers. iOS Universal Links need the app to declare each domain it handles, and Android App Links need a verified association with each host; a store app cannot name every operator's domain ahead of time. So the web page of step 1 must offer an explicit **Open in Conch** (a custom URL scheme link), and the paste in 2c is a first-class path, not an afterthought. The page cannot tell whether the app is installed, so it offers every way in. If M1/M4 find that links open the app reliably for some servers, nothing here becomes wrong: 2a just happens by itself.

**What the invite page has to carry (Future F1).** The existing browser page `/app/#/invite/<code>` needs an "Open in Conch" link that carries the server address and the code to the app, and an "I already have the app: copy this link" action. That is a browser change and an agreed link shape, not a server or schema change. It is out of this PR's scope and is F1.

### 4.2 Sign-in persistence and logout

Sign-in lasts as long as the credential does (default 90 days, renewable, community model 2.1). **Sign out on this phone** ends this credential only (`DELETE /v1/credentials/current`, "Sign out here"; UX-08) and says your other devices stay signed in; it asks first, because a sign-out with unsent drafts loses them (the browser warns before sign-out too). Drafts for a signed-out server stay on the phone until the server is removed.

## 5. Chat, servers and the keyboard

### 5.1 Channels view

One row per channel the account is in (`GET /v1/channels` lists the caller's channels), full width, 44 px or taller, name on one line with an ellipsis and the full name in the accessible name, a second line with state, an unread pill when `GET /v1/read-state` says so (capped label `1000+`). A row's accessible name is a sentence: "#general. Public. 2 unread. 3 in voice." Choosing a row opens it in Chat and moves your read mark forward (`PUT /v1/channels/{c}/read-state`; the mark only moves forward, UX-02). If read state cannot be fetched the pill is absent and a line says unread counts are unavailable, not zero (UX-05, Existing: the browser's honest unread-unavailable state).

"Public" and "Private" group headings and the words in each row need the channel `visibility` of the community model (**Future F2**: `GET /v1/channels` gains `visibility`, unimplemented). Until it exists the list is flat and the rows say nothing about visibility. The prototype draws the groups to show the target.

### 5.2 Chat view

Heading, audience line, messages, composer. Everything the browser's conversation does, minus what is out of scope, in the phone's form:

| Part | Behaviour |
| --- | --- |
| Message | Author (a long name ends in an ellipsis; the full name is in the accessible name and a long press or the message menu shows it), an **Agent** text badge for agents (UX-01: text as well as colour), body as text only, never markup. Long unbroken text wraps (`overflow-wrap: anywhere`); a link wraps and is a link by its own control; a link opens in the system browser after showing the real destination (the browser's links work, #1044, treats links as text first). @mentions (#1043) are marked with a visible background and the word "mentioned you" in the accessible name when the viewer is named. |
| Message menu | Each message has one 44 px **More for message from Ada** button (a visible ⋯), not a long-press only, so it works for screen readers and switch users (the browser moved member actions into a ⋯ menu for the same reason, #1094). The sheet offers **Reply**, **Copy text**, and, for your own message, Edit (Existing `PUT /v2/channels/{c}/messages/{id}/body`; not drawn). |
| Reply | Sets "Replying to Ada: ..." above the box with **Cancel reply**; the thread opens as its own page with a Back (the browser's one-level thread, #1038, `thread_root_id`). A reply inherits its root's audience and cannot widen it (UX-07). The thread page is not drawn in the prototype. |
| Attachments | A **+ Attach a file** button opens the system picker (photos, files); any file may be attached (#1086); a draft attachment is a removable chip with progress and Retry; an image shows a preview only after the server's image check (`POST /v2/channels/{c}/files`, `GET /v2/files/{id}/meta`); anything else is a chip with name and size. Camera capture is not offered (out of scope). |
| Composer | Labelled "Message #channel" (visible placeholder and accessible name), with the audience in words under it ("Message #general (public channel)"; "Voice stays in #lounge" while in voice elsewhere). **Return inserts a newline; Send sends** (a hardware keyboard may use Ctrl or Cmd+Return). This is the phone's convention and differs from the browser's Enter-sends; it is **Open question Q6**. Drafts are kept per server and channel and across view switches (UX-06, Existing in the browser). |
| Send states | **Sending**, then settled; **Failed. Not sent.** with Retry and Remove; **Not confirmed: check the conversation before retrying.** when the app cannot tell whether the server got it, with Retry (UX-06: no silent resend of an ambiguous message; Existing browser words). Retry is not guaranteed duplicate-free: `PostMessageRequestV2` has no idempotency key, so if a send commits but its answer is lost, a blind retry makes a second message. The browser therefore reconciles first: for a Not confirmed send it re-reads history and resends only if the message is not found (`web/src/App.tsx`, `retrySend` and `reconcile`), and if the history walk cannot be completed it refuses to retry and says so. Its limits: it matches what history returns, so a message that is committed but not yet visible, or a body identical to an earlier one, can be misjudged; nothing on the server enforces it. The phone does the same and shows the same words. Real idempotency is F7. A failure is announced and does not take focus. |
| New messages | Appearing while the reader is at the end keeps them at the end; a reader scrolled up is not moved; a **Jump to latest** control appears (UX-07). History earlier than the page loads on scroll without losing place. |

### 5.3 The keyboard

When the composer takes focus the OS keyboard opens. Rules, in order of priority (the first never gives way):

1. **The draft box and Send stay on the screen and are never covered by the keyboard, the strip or the navigation.** The screen is resized, not overlaid: the app asks the OS to resize its window for the keyboard (Android `adjustResize` or the edge-to-edge equivalent; iOS keyboard layout guide) (assumption: the exact mechanism is M1's stack's).
2. The bottom navigation hides while the keyboard is open (it would take height for nothing; Chat is already chosen). Prototype.
3. The voice **strip** stays while there is room, in portrait; in landscape or at large text (section 8) it hides, and the composer's audience line says "Voice: in #lounge" in words.
4. The header hides in landscape with the keyboard, and at the largest text sizes; the log keeps at least two lines of message text where the screen allows it.
5. Hold to talk is not usable while typing: dismiss the keyboard to talk. The app never talks into a keyboard session.
6. Sending keeps the keyboard open and focus in the box (the person is probably writing again); a failed send does not steal focus.

Measured in the prototype, log height above the composer with the keyboard open, in CSS px at 100% text: 360 × 740: 239; 390 × 844: 320; landscape 740 × 360: 106; 844 × 390: 160. Those include a 290 px (portrait) or 150 px (landscape) stand-in keyboard, not a real one (Prototype; real keyboards vary from about 260 to 340 px portrait with suggestion bars).

### 5.4 Server and account switching

The **Servers** button in the top bar opens a sheet listing saved servers: a name you may edit (default: the server's name, from `GET /v1/join`, else its address), the address, the account on it, and which is current. A saved server is not a tenant of another (client experience, section 1); each has its own credential, its own channel list, drafts and read state, and its own connection. Rules:

- **One server is shown at a time.** Switching changes what is displayed and which connection is live; it does not carry a channel, a draft or a notification across.
- **One voice call at a time, on one server (assumption, M1's media-ownership policy decides).** Switching away from a server while in voice warns first ("Switch to Book club? You are in voice in #lounge on Camp Conch. Switching leaves that call.", **Leave voice and switch** or **Stay here**). It never moves the call silently and never keeps a call open under the other server's screen.
- **Add a server** goes to step 2c. **Sign out on this phone** and **Remove server from this phone** (deletes its credential and drafts) are in Settings, each confirmed.
- Two accounts on one server are two entries, labelled by account. A person is shown once there (UX-08).
- A stale link or notification for server A while server B is current switches to A first, after a visible "Opening Camp Conch" and the same voice warning; it never opens A's channel inside B's screen.

### 5.5 Connection, offline and failure

Connection and voice are separate (UX-01). Text connection shows one banner under the top bar, announced politely, in these words (UX-05, Existing in the browser):

| State | Banner | What changes |
| --- | --- | --- |
| Connecting | "Connecting to host." | History from cache, if any, marked "may be out of date". |
| Reconnecting | "Reconnecting to host. Messages shown may be out of date." | Sending paused (Send is `aria-disabled` with the reason as its description), draft kept, voice strip says "Reconnecting to #lounge" and Hold to talk is disabled with a reason. |
| Offline | "Offline. What you see may be out of date. Sending is paused; your draft is kept." with Try again | The same. Retry is explicit. There is no hidden outbox: nothing sends later by itself (UX-23, "no silent message queue in baseline"). |
| Signed out | The `revoked` page: "You were signed out. The server ended this sign-in. Voice stopped. Messages you were still typing stay on this phone." | Protected requests and media stop; Sign in again, or choose another server. |
| Server error | The refused action says so in words next to where it happened; the app does not retry in a loop | |

On a reconnect that works the banner goes, the view stays where it was if still accessible, and the missed messages load without a gap (UX-07). Moving between Wi-Fi and cellular is a reconnect; the voice session's survival through it is M1/M4's measured question, and the UI says "Reconnecting to #lounge" while it is not sure.

## 6. Permissions, notifications and inaccessible channels

### 6.1 Microphone

The system dialog appears on the first **Join voice** (not at launch, not on opening Voice). **Allow**: join. **Don't allow** (or already denied): the app **does not join silently**; a dialog says "The microphone is off for Conch. Conch cannot send your voice. You can still listen to #lounge." with **Open system settings**, **Join without microphone** and **Cancel**. After a denial the Voice view keeps a line "The microphone is off in system settings" with Open system settings, not a repeated prompt. Changing the permission while the app runs is noticed on return to the foreground.

### 6.2 Notifications

Never asked at launch. **Settings > Notifications** shows the state in words. Not asked: "Off. Conch works without them: you see new messages when you open it." with **Turn on notifications** (the system dialog). Denied: "Off in system settings. Conch cannot turn them on; it works without them." with Open system settings. Allowed: "On." What a notification shows, how a tap routes, and the server side are M3's (#1108) and #1116/#1117's; this note decides only that no screen depends on push and nothing waits for it. Push is optional (#757): a phone without it uses the app as drawn.

### 6.3 Listen only and joining without a microphone

Two different things, both shown by words in the Voice view:

- **Listen only** is the server's decision: the voice session says you may not publish (Existing: `can_publish` in the session grant; the browser shows "Listen only"). The Voice view says "Listen only: this server gave you no microphone here"; there is no Hold to talk, no Mute; Deafen stays. The app does not request the microphone it would not use.
- **Join without microphone** is your choice (the browser calls it **Watch only**, which means something else on a phone that cannot yet view a share, so this note proposes the plainer name; **Open question Q4**): the microphone is never requested, Hold to talk and Mute are absent, and the line says "Microphone off: you joined without it". To talk, Leave and Join again.

### 6.4 Deleted or inaccessible channels

A stale link, a notification tap, or a row for a channel you can no longer reach opens the **Channel not available** page: "It may have been renamed, or you may no longer have access. Conch cannot tell which, and does not say." One body for deleted, renamed and not-allowed (MUX-05: the server answers 404 `channel_not_found` the same for all). If a channel disappears while open: its live view stops, any voice you had in it ends with a toast "Voice in #x ended: you no longer have access", the composer is replaced by that sentence, and the other channels stay usable. No channel-delete operation exists yet (archive is #844); this state is reached today by being removed from a private channel or a rename.

## 7. Voice

### 7.1 Where voice is, in words

- **Strip** (under Chat and Channels, only while joining or in voice): one row of the destination and the shortest controls. The destination is a button reading "In voice in #lounge" (or "Connecting to #lounge", "Reconnecting to #lounge") that opens the Voice view; its accessible name adds "Open Voice". Then, when you can speak: **Hold to talk**, **Mute** or **Unmute**, **Leave**. When you cannot, only **Leave**. Each control at least 44 × 44 px; the whole strip about 53 px at 100% text (Prototype: 53 px; the browser's strip caps at 64). The strip is in the normal flow above the navigation and below the composer, so the log gives way, never Send.
- **Composer audience line**: "Message #general. Voice stays in #lounge." while the two differ. It is the second place the destination is written.
- **Voice view**: heading "Voice", a card "In voice in #lounge" or "Not in voice", who is in voice (UX-04, only what the API authorizes), the two state lines (MUX-02), then controls. When you are viewing another channel it says so: "You are viewing #general. Voice stays in #lounge until you leave. To talk in #general, leave #lounge first, then join." and offers no Join for #general.
- **Voice tab dot**: shows you are in voice, always paired with the words as the control's description, never colour alone.

### 7.2 The Voice view's controls

| Control | Shown when | Behaviour |
| --- | --- | --- |
| **Join voice in #channel** | Not in voice | Explicit; names the channel on screen. Disabled with a stated reason while offline (the reason is the button's description and is announced). |
| **Join without microphone in #channel** | Not in voice | As 6.3. |
| **Hold to talk** | In voice, may speak, "Hold to talk" mode | **Large**: full width, at least 88 px high (the strip's is 44). Held with a finger; the label does not change while held (a changing label would break a screen reader's focus); the state lines, a high-contrast fill and, with motion allowed, a ring show "Transmitting". Release, cancel, lock, call, app switch or route change ends it. Disabled (focusable, `aria-disabled`, with its reason as the description) when muted ("Hold to talk sends nothing while you are muted. Unmute to talk."), deafened, or reconnecting. |
| **Open microphone** mode | A Voice options choice | Replaces the button with an explanation; **Mute** stops sending. The detection is M4's; not drawn beyond the words. |
| **Mute / Unmute** | In voice, may speak | Mute stops sending; the label changes (the browser's words). The microphone may remain open (an assumption carried from the browser, where capture stays open while joined; the OS shows its microphone indicator as long as it does, **Q3**). |
| **Deafen / Undeafen** | In voice | Stops playback and sending (UX-11). Undeafen restores the earlier mute. |
| **Leave voice** | In voice | Immediate. Destructive style, no confirmation. |
| **Voice options** | Always | Sheet: "How you talk" (Hold to talk or Open microphone), "Hold to talk works by" (**Holding the button** or **Tapping to start and again to stop**), and a line that sound follows the phone's current output (the OS routes earpiece, speaker, headset and Bluetooth; the app shows which and does not claim to control it). |

**Hold to talk without holding.** Holding a control down is hard or impossible with some motor impairments and awkward with a screen reader's gestures. **Tapping to start and again to stop** is offered as a setting, with a visible "Transmitting" state and an automatic stop on interruption (MUX-03) and after a stated idle limit M4 sets; the tap style never leaves transmission on after the app leaves the screen. It is a client-local setting. It is proposed here, not in the browser (**Q5**).

### 7.3 Capture versus transmission: the states

| State | Microphone line | Sending line | Hold to talk |
| --- | --- | --- | --- |
| Not in voice | (none) | (none) | none |
| Joined, may speak, idle | Microphone in use | Not transmitting | enabled |
| Holding | Microphone in use | **Transmitting** | pressed |
| Muted | Microphone in use; "Muted. Nothing is sent." | Not transmitting | disabled, reason |
| Deafened | "Deafened. You hear nothing and send nothing." | Not transmitting | disabled, reason |
| Listen only | "Listen only: this server gave you no microphone here" | (none) | none |
| Joined without microphone | "Microphone off: you joined without it" | (none) | none |
| Reconnecting | as before, plus the banner | Not transmitting | disabled, reason |
| After an interruption | unchanged mute and deafen; toast "Hold to talk stopped because the app left the screen. Press again to talk." | Not transmitting | enabled, fresh press needed |

"Microphone in use" is the truth about the capture and is shown whenever it is open (UX-11), also when muted; it must not be softened to "off".

### 7.4 The foreground rule and what survives (depends on M1, M4, M5)

The product promise in #757 is that the supported voice session survives app switching and screen lock and the person returns, leaves and logs out. How is **M4/M5/M1's** to measure; this note fixes only what the **UI** guarantees whichever way they come out:

- **Hold to talk works only while the app is on screen.** There is no background or global talk key (out of scope). The Voice view says: "Sends only while this app is on screen and stops if it leaves." Prototype: the interruption control ends a held talk and shows the toast.
- **A lock, call, alarm, route change or app switch cancels a held talk and nothing else.** No stuck "Transmitting"; no automatic unmute; no automatic resume of talking.
- **If voice cannot continue in the background**, the strip and the Voice view say "Voice stopped" on return, why, and offer Join; text and Channels carry on. If it can, it comes back as "In voice in #lounge" with mute and deafen as left.
- **Screen lock with a held finger** cannot be assumed to deliver a release event; the cancel path is the safe default (assumption, M4 to test).
- A **Bluetooth or headset** change is a route change: the output indicator updates and a held talk is cancelled.
- **Desktop to phone handoff** (one owner of a given microphone per session, ADR-007): if this account already has the media session on another device, the phone says so in the Join control's description and M1's policy decides whether Join transfers or refuses. Not drawn.

### 7.5 Changing channels while in voice

Leave, then Join (two taps, both explicit). This is the safe reading of MUX-01 and UX-02 and is simple to explain; a one-tap "move my voice to #general" is a smaller step but is the retarget the rule exists to prevent, and a mis-tap would land your voice in a room you were only reading. The browser's behaviour for Join while in another channel was not verified for this note (**Q2**).

## 8. Layout: sizes, landscape, safe areas, large text

The four inspectable sizes (CSS px): **360 × 740** and **390 × 844** portrait, **740 × 360** and **844 × 390** landscape. 360 is the narrow Android baseline; 390 is the common iPhone. The browser's own tests add 320 × 640 and 430 × 932, which this prototype does not draw; M7 should keep them.

| Rule | Spec | Prototype result |
| --- | --- | --- |
| Targets | Every button, field and row at least **44 × 44 px**, at every text size; text wrapping makes it taller, never smaller. The strip's four controls are 4 px apart in the prototype; a larger gap is a build-time choice. | No button or field under 44 × 44 px at 100% text across 18 screens and four sizes (script). |
| Width | Nothing scrolls sideways. Every line wraps or ends in an ellipsis with the full text in the accessible name. | No horizontal overflow at the four sizes at 100% and 200%. |
| Safe areas | The app pads for the status bar, the notch or camera cut-out, the rounded corners and the home indicator or gesture bar: top, bottom, and in landscape both sides. Backgrounds run to the edge; controls sit inside. | The prototype applies 24 and 47 px top, 16 to 34 px bottom, 47 or 48 px side insets and draws them on request. The values are typical, not measured on a device. |
| Landscape | The three views become a **rail on the leading edge** (it gives the log its height back); the Voice view is two columns (state and roster beside talk, mute and leave); the composer and strip stay in the column. With the keyboard open the rail, header and strip hide. | Log height above the composer at 100% text: 134 (740 × 360) and 183 px (844 × 390), 106 and 160 with the keyboard. |
| Send is never covered | In every state the draft box and Send are on the screen and the first thing at their centre. | Script: true at all four sizes, 100% and 200% text, with and without the keyboard (in the document-scroll mode below, by scrolling). |
| Largest text | At 160% and over, the strip collapses to its destination button only (Hold to talk, Mute and Leave are in the Voice view). In landscape the collapse starts at 130%. In **landscape at 160% and over there is no fixed chrome**: header, log, composer and strip are one document that scrolls, as the browser does under 560 px of height (browser layout section 4). With the keyboard up at those sizes the header and strip hide. | Log height at 130% text with the strip: 358 (360 × 740), 482 (390 × 844), 98 (740 × 360), 147 (844 × 390). At 200%: 155 and 264 portrait; landscape scrolls as one document. |

**Why the strip collapses.** At 200% the four controls cannot be 44 px apart in 360 px, so leaving voice from Chat needs one more tap (to the Voice view) than at normal sizes; Mute and Hold to talk are there too. That cost is accepted so Send is never covered and the destination is always written.

**Text scaling.** All text sizes follow the OS setting (iOS Dynamic Type including the accessibility sizes, Android font scale to 200%). Nothing is truncated that carries meaning: a destination wraps to two lines before it is cut; names and channels may end in an ellipsis only where the full text is in the accessible name and reachable (a message's author is in its menu). Icons that carry meaning have text beside them (the navigation labels never disappear, only wrap).

**Reduced motion.** When the OS asks for reduced motion (or the app's setting does) sheets appear without sliding, the talk ring does not pulse, and view switches do not animate; every state is carried by text, so nothing depends on motion. Prototype: the Motion control and the system setting.

## 9. Accessibility

### 9.1 Screen reader names

A label is what the screen reader says: it **starts with the visible text** (so a voice-control user who says "tap Leave" reaches "Leave voice") and adds context after it. States are properties, not words in the name, except where the browser already uses words in the name.

| Control | Visible text | Accessible name and state |
| --- | --- | --- |
| Servers button | the menu icon | "Servers. Now on Camp Conch, conch.example.org, signed in as Sam" |
| Settings | the gear | "Settings" |
| Members | the people icon | "Members of #general" |
| Navigation | Chat, Channels, Voice | "Chat", "Channels", "Voice"; the current is "selected" or `aria-current`; Voice adds the description "You are in voice in #lounge" |
| Channel row | "#general", subline, count | "#general. Public. 2 unread. 3 in voice." |
| Message | author, time, body | A list item read as "Ada. Morning! ..." with "Agent" read after an agent's name; the More button "More for message from Ada" |
| Send | Send | "Send"; when paused, `aria-disabled` and a description "Sending is paused until Conch is connected. Your draft is kept." |
| Strip destination | In voice in #lounge | "In voice in #lounge. Open Voice" |
| Strip Hold to talk | Hold to talk | "Hold to talk, sends to #lounge" |
| Strip Leave | Leave | "Leave voice" |
| Mute | Mute / Unmute | the same words (state is the label, as the browser) |
| Voice view Hold to talk | Hold to talk | "Hold to talk, sends to #lounge"; disabled reason as description |
| Transmission state | Transmitting | A polite live status ("Transmitting." and "Not transmitting.") on change, not repeated every frame |
| Join | Join voice in #general | the same |
| Error | red text | Announced as an alert at once and tied to its field with the field's description, never colour alone |

**Hold to talk for screen reader users.** With VoiceOver or TalkBack on, a double-tap-and-hold (the system gesture to press and hold) must work, and the tap-to-toggle style of 7.2 must be reachable and obvious. M4/M5 test both; this note makes no claim they work.

### 9.2 Focus order and management

Focus follows reading order, top to bottom, leading to trailing, per screen, and is rebuilt by orientation (the rail comes first in landscape because it is first on screen).

| Screen | Order |
| --- | --- |
| Link, paste, confirm | Title, explanation, fields (label then field), primary action, secondary actions |
| Account, sign in | Title, server, each field with its error, Show passwords, primary, secondary |
| Channels | Title, Servers, Settings, banner, each channel row in order, the strip's controls, navigation |
| Chat | Title, Members, Settings, banner, messages in order (each with More), reply context, audience line, Attach, message box, Send, then the strip, then navigation |
| Voice | Title, Servers, Settings, banner, destination card, state lines, Hold to talk, Mute, Deafen, Leave, Voice options, navigation |
| Sheet | Opens with focus on its title; the rest of the app is inert; Tab stays inside; **Escape or a back gesture closes it and returns focus to the control that opened it** |

Focus **moves** only when the screen did: to the new screen's title after navigation (including after choosing a channel), to the first invalid field after a failed submit, to the message box after Send or Reply, to Join after Leave, and into a sheet. A banner, a toast or a state change announces itself and does not take focus. Prototype: each of these was run by script.

### 9.3 Long text and names

Names: one line with an ellipsis in dense lists (Channels, members, roster rows); wrapped in headings, the Voice destination, errors and sheets; the full name is always the accessible name. A server address is never shortened on Confirm. Channel names up to the server's limit wrap or end in an ellipsis in a row and show in full in the heading. Messages wrap fully; a very long unbroken string wraps mid-string; there is no "Show more" in this phase (the browser has none). Attachment names wrap and keep their size. Right-to-left and mixed-direction text follow the OS text direction (assumption, M7 to test).

## 10. What each control needs

**Existing** is an operation in `web/src/platform/operations.ts` on `main`. **Local** is client state with no API. **Future** is named in section 11.

| Control | Needs |
| --- | --- |
| Open invite link / Open in Conch | Future F1 (link and hand-off); the link format is Existing |
| Add a server, validate address | Local; `GET /healthz` or `GET /v1/join` to confirm it is a Conch server (Existing) |
| Confirm the server | `POST /v1/invites/preview`, `GET /v1/join` (Existing) |
| Create account | `POST /v2/invites/redeem` (Existing); the new account is a member, community model 2.1 |
| Sign in; operator code; sign-in link | `POST /v1/login`, `/v1/login/totp`, `/v1/sign-in-links/redeem` (Existing) |
| Device name in "Your sign-ins" | `GET /v1/credentials/mine` is Existing; the login request has no device label field, so the phone cannot name itself: **Future F6 / Q7** |
| Sign out on this phone | `DELETE /v1/credentials/current` (Existing) |
| Servers sheet, switching, labels, removal | Local (profiles and secure storage are M1/M6's) |
| Channels list, unread | `GET /v1/channels`, `GET /v1/read-state`, `PUT /v1/channels/{c}/read-state` (Existing); visibility labels need F2 |
| Messages, send, live | `GET` and `POST /v2/channels/{c}/messages`, `POST /v1/ws-tickets` then `GET /v2/ws` (Existing) |
| Reply, thread | `thread_root_id` on `POST /v2/channels/{c}/messages` (Existing) |
| Attach | `POST /v2/channels/{c}/files`, `GET /v2/files/{id}/meta`, `GET /v2/files/{id}` (Existing) |
| Edit own message | `PUT /v2/channels/{c}/messages/{id}/body` (Existing; not drawn) |
| Members of a channel | `GET /v1/channels/{c}/members` (Existing) |
| Who is in voice | `GET /v1/channels/{c}/voice`, `GET /v1/voice/occupancy` and its socket (Existing) |
| Join voice | `POST /v1/channels/{c}/voice/session` (Existing; its answer carries `can_publish`) |
| Hold to talk, Mute, Deafen, capture | Local to the media engine (M1/M4/M5); the server is told by `POST /v1/channels/{c}/voice/transmit` (Existing; the browser reports transmit) |
| Leave | Local; presence follows the media connection |
| Listen only | The session grant (Existing); an operator's switch is community model 2.5 (Future, #976) |
| Join without microphone | Local |
| Voice options | Local |
| Notifications settings | Local state plus M3's registration (Future) |
| Channel not available, revoked | The 404 `channel_not_found` and a refused credential (Existing) |
| Report a problem | `POST /v1/reports` (Existing; the phone app should offer it as the browser does; not drawn) |
| Push, tap routing | Future (M3, #1116, #1117) |
| Screen-share viewing | Future (#1118); not drawn |

## 11. Future issues this note names (none filed)

| ID | What | Why |
| --- | --- | --- |
| F1 | The web invite page offers **Open in Conch** (a link carrying server and code) and **Copy invite link** for phones; an agreed link shape | Section 4.1: self-hosted domains cannot be pre-registered in a store app (assumption) |
| F2 | `visibility` (and `default`) on `GET /v1/channels` | Public and Private groups; community model 3.2, part of #976 |
| F3 | A directory of public channels to join from the phone | Joining a public channel is community model 2.6; the phone has no Join-a-channel screen until it exists. Not drawn |
| F4 | Thread page and reply form for the phone | Specified in 5.2, not drawn |
| F5 | A notification-denied and no-push test row in each client's acceptance record | M3 and #1119/#1120 |
| F7 | Server-side idempotency for posting a message (a client request key on `PostMessageRequestV2`, replayed answers) | Section 5.2: reconciliation is a client heuristic; a schema and API issue first |
| F6 | A device label on login or credential creation, so "Your sign-ins" can say "Sam's Pixel" | Section 10 |

No wire shape is added or changed by this note; F1 to F7 would each go through `pkg/schema` first if they need one.

## 12. Verified and not verified

**Prototype, scripted, headless Chromium on one machine:** all 18 routes load with no script error and no request outside the page; at the four sizes, at 100% and 200% text, with and without the stand-in keyboard, no button or field is under 44 × 44 px at 100%, nothing overflows horizontally, and Send is reachable; the invite (installed and not), paste validation, create-account validation, offline join refusal, the microphone prompt, denial and listen-only paths, join, hold, interrupt, mute, deafen, undeafen, browsing another channel while in voice, the server-switch warning, a failed send and its retry, offline sending pause, leave, sheet focus and Escape-to-opener run as described. The mockup's behaviour is **labelled illustrative** on the page and in its README; no audio, microphone or network exists in it.

**Not verified, and not claimed:** any real device or OS; VoiceOver, TalkBack, Switch Control, Voice Control or external keyboards; a real on-screen keyboard, notch, rotation or gesture bar; Dynamic Type or Android font scale (the 200% control is CSS scaling); real talk, route or Bluetooth behaviour; background and lock behaviour of voice; link hand-off from a browser to an app on either OS; contrast measured with a tool (the palette is dark and was chosen for contrast, but ratios were not computed); the clipboard-permission wording on iOS. M4, M5 and the alpha records (#1109, #1110, #1119, #1120) own those rows. A browser or emulator pass is not proof of native support (#757).

## 13. Open questions for Nick

| # | Question | My recommendation |
| --- | --- | --- |
| Q1 | The invite hand-off: OK to rely on an explicit **Open in Conch** (custom scheme) and paste, not OS link verification (4.1)? | Yes. Verify at M4/M5 whether a verified link can work for some servers, as a bonus. |
| Q2 | Changing voice channel: Leave then Join (7.5), or a one-tap "move voice here"? | Leave then Join. Check what the browser does today. |
| Q3 | Should Mute close the microphone (the OS indicator goes off, saving battery) or keep it open as the browser does? | Keep as the browser until M4 measures; revisit. |
| Q4 | The name for joining with no microphone: the browser says "Watch only". | "Join without microphone" on the phone; consider the same in the browser later. |
| Q5 | Accept the tap-to-toggle talk style as an accessibility setting (7.2)? It adds a state the browser lacks. | Yes, off by default, and offered whenever a screen reader is on. |
| Q6 | Return on the soft keyboard inserts a newline, Send sends (5.2); the browser's Enter sends. | Newline on a phone. |
| Q7 | A device label on sign-in (F6)? | Yes, small and valuable; schema issue first. |
| Q9 | Add server-side idempotency for message posts (F7) before the phone app ships sending? | Yes: a phone loses connections more often than a desktop, so duplicates would be more common. Schema issue first. |
| Q8 | Strip collapse at 160% (section 8) so Leave is a tap further at large text. Accept? | Yes. |

Stack choice, store or distribution, and anything in M1's and M3's documents are not decided here.
