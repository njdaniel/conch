# Private alpha exit record

Issue #449: the exit of the private communications alpha (item A16 of #420). The
exit scenario is [`private-alpha.md`](private-alpha.md) §1 and the decision is
[ADR-008](../adr/ADR-008-private-communications-alpha.md). This record follows the
P3 exit record ([`p3-exit-record.md`](p3-exit-record.md)): the run, a row per check
with its evidence, what surprised us, and what remains. Testers follow the
[tester guide](../alpha-tester-guide.md).

**Status: a run sheet, not yet run.** Prepared on 2026-10-16, before the pre-release
is tagged. Every Result and Evidence cell is empty until the run. A row marked
*(lands with #NNN)* checks a feature that is still in an open pull request on that
date. It stays in the sheet, and is run only once that feature is in the tagged build.

> **Whose run this is.** The run that closes the alpha is Nick's own, with at least
> two testers (#449). An agent may prepare the build and run the automated checks.
> Each row's "Run by" says who is meant to run it:
>
> - **Person:** Nick or a tester, by hand, during the run. An agent never fills one of
>   these rows.
> - **Agent:** an agent session may run it. Its Result then says "agent run", as P3's
>   record did for P3-13 and P3-14.
> - **Person, agent may check:** a person makes it happen. An agent may then read the
>   evidence Nick saved (an audit export, `conchd`'s log lines) and fill in the
>   Evidence cell, marked "checked by an agent".
>
> If a row is run differently on the day, its Result says who really ran it.

## Filling it in

This repository is public, and so is this record.

- **No tester is named.** Testers are T1, T2… with the platform they joined from.
  Nobody's handle, email, address or device name appears, and no group's name.
- **People and channels go by id.** Principals are written by id (`principal 14`),
  channels as "group A's channel", or by id. Audit rows are quoted by their id, action
  and detail. Rows whose detail carries a name (`principal_renamed`,
  `principal_hidden`, `channel_created`) are cited by id only.
- **No secret and no address.** No invite code or link, no credential, token or
  ticket, and not the server's address. The server is "the alpha server", with its
  instance name.
- **Screenshots are not committed.** Describe what one showed.
- **Times are UTC**, from Nick's machine.

A step that fails:

- is filed as a bug, `priority/blocker` if it stops joining, talking or privacy (plan
  §6);
- is run again on a fixed build, as a new run below;
- keeps its first run in this record. Nothing is overwritten.

The alpha milestone closes only when every row passes, or Nick accepts a failure in
his own words under [Accepted failures](#accepted-failures).

## The run

| | |
| --- | --- |
| Date | |
| Pre-release tag | |
| Commit | |
| Server | the alpha server, instance: |
| `conchd` version (`/healthz`) | |
| LiveKit | the pinned image, as in [`docs/deploy.md`](../deploy.md) §4: |
| Testers | how many, and T1… with each one's OS, browser and browser version |
| Groups | one joined through the open link; two people through personal invites |
| How it was recorded | a recorded call, or notes; kept outside the repository |

## Before the run

The release skill requires B1–B5 on the tagged commit before the tag.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| B1 | `make check` passes | Agent | the commit, and the last lines of the output | | |
| B2 | `make web-e2e` passes | Agent | the number of tests passed, including `10-voice`, `11-invites`, `12-phone` and the screen-share spec *(lands with #444)* | | |
| B3 | `go run ./e2e/isolation` passes *(lands with #447)* | Agent | its last lines | | |
| B4 | `go run ./e2e/voice` passes | Agent | its last lines | | |
| B5 | The dogfood canary passes (`dogfood-check` skill) | Agent | its last lines | | |
| B6 | Nick tags `v0.x.y-alpha.N` through the release skill. It is a GitHub pre-release, with notes and its CHANGELOG entry | Person | the tag, the commit, and that the release is marked pre-release | | |
| B7 | The deployment is upgraded to the tag: `conch-upgrade.sh <tag> <instance>` ends with `/healthz` answering `ok` and the tag (deploy.md §11) | Person | the tag `/healthz` reported, and the time | | |
| B8 | A backup taken after the upgrade | Person | the archive's name and time | | |
| B9 | The restore drill (deploy.md §9) on that archive passes | Person | the drill's last lines | | |
| B10 | Voice is configured and up: `/healthz` says `voice` `ok` *(the field lands with #446)*; LiveKit's UDP media ports are open | Person | the `voice` value | | |
| B11 | Invites are not paused: `conch admin invite status` | Person | its answer | | |
| B12 | The bandwidth measurement (A15, #448) is recorded and the droplet sized for it | Person | where it is recorded | | |
| B13 | The tester guide matches this build: its version example is right, every *(lands with #NNN)* mark is removed or its section struck, and it still names no group or person and holds no link, code or credential | Agent | the commit of the guide sent to testers | | |

## Rows

### Step 1: the version

> Nick deploys a build and can see its version in the web client and from the server.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 1.1 | `GET /healthz` answers `200` with `status` `ok`, `db` `ok`, `version` the tag, and `voice` `ok` *(`voice` lands with #446)* | Person, agent may check | the JSON answer, quoted (it holds nothing secret) | | |
| 1.2 | `conch whoami` against the server prints `server: <tag>` | Person | the `server:` line only | | |
| 1.3 | `conch version` names the client's build | Person | its output | | |
| 1.4 | The invite landing page shows `Version <tag>` under the form | Person | per tester: seen, and the text | | |
| 1.5 | The lobby's status bar shows `Version <tag>`, on every tester's device | Person | per tester: seen, and the text | | |

### Step 2: invites and joins

> He sends an open invite link to one group and personal invites to two people. They
> join as separate users, from at least Windows, macOS, Linux and one phone.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 2.1 | Nick makes an open link for group A's channels, with voice: `conch admin invite create --open --channel <c> --expires 24h --voice` | Person | the invite id, its expiry, and the `invite_created` audit row's id | | |
| 2.2 | Nick makes a personal invite for each of two people, for channels no other group is in: `conch admin invite create --personal --channel <c> --voice` | Person | the two invite ids, and their `invite_created` rows | | |
| 2.3 | Each tester opens their link. The landing page shows who invited them, the channels, whether voice is included, and the expiry | Person | per tester: seen | | |
| 2.4 | After the page opens, the address bar no longer holds the code, and Back does not bring it back | Person | per tester: seen | | |
| 2.5 | A taken name, or a look-alike of one, is refused with "That name is taken. Choose another.", and the field keeps what was typed | Person | which tester, and what kind of clash was tried (case, look-alike); not the name | | |
| 2.6 | Every tester joins as a separate person: `conch admin invite joiners <id>` for each invite | Person, agent may check | per invite: the joiners' principal ids, all distinct | | |
| 2.7 | Each join is audited: `invite_redeemed`, `principal_created`, a `member_added` per granted channel and `credential_created`, with actor `invite:<id>` | Person, agent may check | per joiner: the rows' ids, from `conch audit export` | | |
| 2.8 | A personal link opened a second time shows "This invite no longer works" | Person | which invite | | |
| 2.9 | A join from **Windows** | Person | the tester, the Windows version, the browser and its version | | |
| 2.10 | A join from **macOS** | Person | the tester, the macOS version, the browser and its version | | |
| 2.11 | A join from **Linux** | Person | the tester, the distribution, the browser and its version | | |
| 2.12 | A join from **a phone** (iPhone, iPad or Android) | Person | the tester, the device kind and system version, the browser | | |
| 2.13 | The sign-in lasts: a tester closes the whole browser, reopens the server's page, and is still signed in without pasting anything | Person | the tester and platform | | |

### Step 3: chat, and nothing of the other groups

> They chat in their group's channel, and see nothing of the other groups.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 3.1 | Group A's members chat in their channel. Each message is seen once, in order, by every member there | Person | the ids of the exchange's first and last messages, from the server's history (`GET /v2/channels/{channel}/messages`); per tester, anything missing or doubled | | |
| 3.2 | Each personal-invite tester chats in their own channel, the same way | Person | as 3.1 | | |
| 3.3 | Each tester's channel list holds exactly the channels their invite granted | Person | per tester: the number listed, and the number granted | | |
| 3.4 | No tester sees anyone of another group: not in the member list, the voice roster, or the "Invited by" of the landing page (only the inviter) | Person | per tester: seen | | |
| 3.5 | A tester who opens another group's channel by its address (`#/c/<name>`) is not shown it | Person | the tester; what the page showed | | |
| 3.6 | The automated check of the same: B3 passed on this commit | Agent | B3's result | | |

### Step 4: voice

> They talk in voice, with mute and deafen, and with push-to-talk while the tab has
> focus.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 4.1 | Each tester chooses Join voice. The browser asks for the microphone, and everyone in voice is listed in the Voice section | Person | per tester: joined, and the prompt seen; the number of lines in `conch voice status <channel>` | | |
| 4.2 | Push to talk with the talk key, while the tab has focus: heard only while it is held. Switching tab while holding it stops the talk | Person | per desktop tester: seen | | |
| 4.3 | Hold to talk, on a phone or touch screen | Person | the tester and device | | |
| 4.4 | Open microphone: with the level set on the meter, it opens on speech and closes after a pause | Person | the tester | | |
| 4.5 | Mute: the muted tester is not heard. Deafen: the deafened tester hears nobody and is not heard. Both show in their panel | Person | the testers; what the panel showed | | |
| 4.6 | Whoever talks is marked "(speaking)" on the others' pages, and `talking` in `conch voice status` | Person | seen; the status line, ids only | | |
| 4.7 | Each talk is a reported `voice_transmit_started` and `voice_transmit_stopped` pair (`source=reported`), and the session has no `voice_transmit_unreported` row. Each tester has a `voice_joined` row | Person, agent may check | for the session's time window, from `conch audit export`: the count of each action, and the id of any `voice_transmit_unreported` row | | |
| 4.8 | On a phone, voice stops when the page leaves the screen and comes back when it returns, as the guide says | Person | the tester and device; what happened | | |

### Step 5: a screen share with sound *(lands with #444)*

> One shares a screen with sound (in Chrome or Edge). The others watch, phone
> included.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 5.1 | A tester in Chrome or Edge shares a tab with its sound. Before the picker, the page says what sound will be shared; afterwards, that sound is being shared | Person | the tester, OS and browser; the two notes as shown | | |
| 5.2 | Audited `voice_share_started`, actor the sharer, detail `audio=true` | Person, agent may check | the row's id and detail | | |
| 5.3 | Watched with picture and sound in Chrome or Edge on a desktop | Person | the viewer and platform | | |
| 5.4 | Watched in Firefox or Safari | Person | the viewer and platform | | |
| 5.5 | Watched on a phone: the picture fits the screen and the sound plays, after "Tap to play sound" where it is offered | Person | the viewer and device; whether the tap was needed | | |
| 5.6 | A tester who chooses Watch only sees the share, and the browser does not ask for the microphone | Person | the tester | | |
| 5.7 | A second tester who tries to share is told who is sharing | Person | the tester; the message | | |
| 5.8 | Stop sharing, or the browser's own stop, ends the share for every viewer. Audited `voice_share_stopped` with `reason=self` | Person, agent may check | what the viewers saw; the row's id | | |
| 5.9 | On Windows, the whole screen shared with the system's sound | Person | tried or untried, and why | | |
| 5.10 | The "to check" cells of plan §2: macOS Chrome sharing the whole screen with sound, and Android Chrome sharing a screen | Person | each: yes, no, or untried and why | | |

### Step 6: a dropped connection

> A tester's connection drops (Wi-Fi off, laptop sleep). The client says so, comes back
> on its own, and loses or repeats no message. Voice and the screen share resume.

Before each drop, the tester whose connection drops is in the group's channel, in
voice, and watching a share *(lands with #444)*. While they are away, the others post
numbered messages ("drop 1", "drop 2"…) and keep talking. The message ids go in the
table after this one.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 6.1 | **Wi-Fi off for about 60 s on a laptop.** The page shows Reconnecting in its status bar, and "This conversation may be out of date" | Person | the tester and platform; the times Wi-Fi went off and on; what the page showed | | |
| 6.2 | It comes back on its own: Connected, without a reload or a click | Person | the seconds from Wi-Fi on to Connected | | |
| 6.3 | No message is lost or repeated | Person, agent may check | the drop's line in the message-id table | | |
| 6.4 | Voice resumes without the tester touching anything, or the record says what they had to do | Person | the Voice section's states, as seen; the time; the tester's `voice_left` and `voice_joined` rows, if any | | |
| 6.5 | The share resumes for the viewer who dropped *(lands with #444)* | Person | what the viewer saw, and what they had to do | | |
| 6.6 | **The sharer's connection drops.** By the design in #444, a reconnect through `conchd` ends the share, and the page says "Your share ended when the voice connection dropped. Share again if you want to." A short drop that the media connection survives by itself keeps the share. *(lands with #444)* | Person | which happened, and how long the drop was | | |
| 6.7 | **A laptop lid closed for a few minutes,** then opened: the checks of 6.1–6.5 again | Person | as 6.1–6.5 | | |
| 6.8 | Server side: `conchd`'s log has a close line for each of the tester's sockets, with `reason=keepalive_timeout` or `client` *(the lines land with #446)* | Person, agent may check | per socket: the route, the principal id, the reason and the duration, nothing else from the line | | |
| 6.9 | The others were unaffected: they kept chatting and talking during the drop | Person | seen | | |

**Message ids across each drop.** The server's side comes from its history
(`GET /v2/channels/{channel}/messages?after=<id>`, as a member). The page's side is
what the tester's page holds after it reconnects: each message's `data-message-id`,
read in the browser's developer tools, or the numbered messages read off the page and
matched to the server's ids.

| Drop | Tester and platform | How | Last id the page held before | Ids posted while down (server) | Ids the page holds after | Missing | Repeated |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 1 | | Wi-Fi off, about 60 s | | | | | |
| 2 | | lid closed, a few minutes | | | | | |

### Step 7: revocation, and closing the open link

> Nick revokes one tester and closes the open link. The tester is out at once, and the
> link no longer works.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 7.1 | While the tester is in the channel and in voice, Nick disables them: `conch admin principal disable <id>` | Person | the principal id, and the time | | |
| 7.2 | The tester's page leaves at once, for the sign-in page with "Signed out: the credential is no longer valid" | Person | the seconds from 7.1 until the page changed | | |
| 7.3 | The tester leaves voice, and the others' voice roster drops them | Person | seen; the time | | |
| 7.4 | Audited: `principal_disabled`; a `credential_revoked` for each credential; `invite_closed` with `reason=creator_disabled` for any invite they had open; and, as they were in voice, `voice_room_rotated` with `reason=principal_disabled` | Person, agent may check | the rows' ids | | |
| 7.5 | Reloading the page leaves the tester signed out. Any other browser or client of theirs is refused too | Person | what was tried | | |
| 7.6 | Nick closes the open link: `conch admin invite close <id>`. Audited `invite_closed` with `reason=creator` | Person, agent may check | the invite id; the row's id | | |
| 7.7 | The open link, opened afterwards in a browser that never used it, shows "This invite no longer works" | Person | the platform; the time | | |
| 7.8 | Everyone else is unaffected: still signed in, chatting and in voice | Person | seen | | |

### Step 8: a report that becomes an issue *(lands with #446)*

> A tester reports a bug from inside the product with the build version and
> diagnostics. It becomes an issue in the backlog.

| Row | Check | Run by | Evidence it needs | Result | Evidence |
| --- | --- | --- | --- | --- | --- |
| 8.1 | A tester chooses Report a problem, answers the questions, reviews exactly what will be sent, and sends it. The page gives the report's number | Person | the tester and platform; the report id | | |
| 8.2 | Audited `problem_reported`, with the report id alone | Person, agent may check | the row's id | | |
| 8.3 | Nick reads it with `conch admin reports show <id>`. It holds the client's version and the server's (both the tag), the browser and system, the page's recent events, and the server's request ids for that person | Person | yes or no for each part, not the content | | |
| 8.4 | `ops/report-to-issue.sh <id>`: Nick reads and trims the body in his editor, and confirms. The body names no channel and no person | Person | the redaction count it printed; that Nick read it | | |
| 8.5 | The issue exists, labelled `type/bug`, in the Project's Backlog, with nothing private of the tester's in it | Person | the issue number; its Project status | | |
| 8.6 | `conch admin reports list` shows the report as filed, with that issue | Person, agent may check | the report's line, without the principal id | | |

## Platforms tried

Plan §2's table, as this run leaves it. Every cell is "yes", "no", or "untried" with
the reason. Once the run is done, this table replaces plan §2's and the tester
guide's section 8.

| | Chat | Voice | Talk key | Share a screen | Share its sound | Watch a share |
| --- | --- | --- | --- | --- | --- | --- |
| Windows, Chrome or Edge | | | | | | |
| macOS, Chrome or Edge | | | | | | |
| Linux, Chrome or Edge | | | | | | |
| Firefox or Safari, any desktop | | | | | | |
| iPhone or iPad browser | | | | | | |
| Android, Chrome | | | | | | |
| Linux, `conch-voice` | | | | | | |
| TUI, any OS | | | | | | |

## Accepted failures

A row that did not pass and that Nick accepts for the alpha's exit, in his own words,
with the date and the issue that tracks it.

None yet.

## What surprised us

Each item fixed, or filed with its issue number.

Nothing yet: the run has not happened.

## What remains

Each item is a filed issue, in the right milestone.

Nothing yet: the run has not happened.

## Runs again

A step run again on a fixed build: the date, the tag, the rows run, and their results.
The first run's rows above stay as they were.

None yet.

## After the run

- [ ] Every row above has a Result and its Evidence, or is under Accepted failures.
- [ ] What surprised us, and What remains, are written. Each remaining item is a filed issue.
- [ ] Plan §2's table is replaced by Platforms tried, and the plan's status is updated.
- [ ] The tester guide's section 8 matches Platforms tried.
- [ ] `ROADMAP.md` says where the alpha stands.
- [ ] The record and the guide have been read for private details before merging.
- [ ] Nick closes the Private alpha milestone.
