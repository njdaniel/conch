# The private communications alpha

Issue [#420](https://github.com/njdaniel/conch/issues/420). The decision is
[ADR-008](../adr/ADR-008-private-communications-alpha.md). This note is the plan: what
the alpha is, what is decided, what is still open with a recommendation, the work in
order, and how it is built, tested and fed back.

**Status:** proposed with ADR-008. Nick's sign-off on the PR that adds both is the
go-ahead to file the implementation issues in §5.

## 1. What the alpha is

**Who.**
- Testers Nick invites, in several groups that are completely separate from each other.
- About 30 people online at once. The total number of accounts is higher: it includes Nick's throwaway test accounts and AI agents.

**What they do.** They chat, talk in voice and share a screen, all in the browser, on one server Nick runs on DigitalOcean.

**Exit scenario.** The alpha is done when this works end to end, on a deployed, versioned build:

1. Nick deploys a build and can see its version in the web client and from the server.
2. He sends an open invite link to one group and personal invites to two people. They join as separate users, from at least Windows, macOS, Linux and one phone.
3. They chat in their group's channel, and see nothing of the other groups.
4. They talk in voice, with mute and deafen, and with push-to-talk while the tab has focus.
5. One shares a screen with sound (in Chrome or Edge); the others watch, phone included.
6. A tester's connection drops (Wi-Fi off, laptop sleep). The client says so, comes back on its own, and loses or repeats no message. Voice and the screen share resume.
7. Nick revokes one tester and closes the open link. The tester is out at once, and the link no longer works.
8. A tester reports a bug from inside the product with the build version and diagnostics. It becomes an issue in the backlog.

**What A14 (#447) measured, 2026-10-17.** Steps 3 and 7 have a check of their own: `go run ./e2e/isolation`, run in CI as the `isolation-check` job. It runs two groups on one `conchd`, with the pinned LiveKit in Docker. A member of one group tries every route of the server against the other group's channels, people, messages, files, sessions, runs, approvals, invites and reports, and its sockets and LiveKit's media too. Every answer is the documented refusal (or, on a route that answers only about the caller, the caller's own data) and says nothing of the other group (the [manual](../manual.md#the-cross-group-isolation-check) lists the checks). Revocation is timed from the moment the operator's request is sent:

| Step | Measured (loopback, five runs) | Bound the check holds |
| --- | --- | --- |
| Disable a tester holding every socket variant (eight), a LiveKit participant and the share | all eight sockets closed within 6.7 ms; LiveKit stopped listing the participant within 9.2 ms | 2 s per socket; 5.5 s for LiveKit |
| Disable everyone from an open link (two joiners, sixteen sockets, two participants) | all sixteen sockets closed within 21 ms; both participants gone within 17.3 ms | the same |
| Close the link | redeeming and previewing it answer `invite_invalid` at once | |

The LiveKit bound is one pass of conchd's voice poller (`voicePassGapMax`, 1.5 s) plus a room rotation (`voiceEvictTimeout`, 4 s). The check reads both from `internal/server/voice_presence.go`, so the bound follows the code. In practice neither waits for the poller: the sockets are dropped and the room rotated inside the operator's request, before it answers, and the poller is the backstop. On the droplet, add the network round trip to these figures. After each step the check also requires the revoked to stay out: a socket ticket they kept is refused (401), a join grant they kept names a room LiveKit no longer has (404, because the room was rotated), and one poller pass later LiveKit lists them in no room.

Message, file and principal ids are global sequences, so a member can infer another group's activity volume from id gaps; accepted for the alpha (see #500).

## 2. Platforms

| | Chat | Voice | Talk key | Share a screen | Share its sound | Watch a share |
| --- | --- | --- | --- | --- | --- | --- |
| Windows, Chrome or Edge | expected | expected | tab focused | expected | tab or whole system | expected |
| macOS, Chrome or Edge | expected | expected | tab focused | expected | tab; whole screen to check | expected |
| Linux, Chrome or Edge | expected | expected | tab focused | expected | tab only | expected |
| Firefox or Safari, any desktop | expected | expected | tab focused | expected | none | expected |
| iPhone or iPad browser | expected | expected | while open on screen | no | no | expected |
| Android, Chrome | expected | expected | while open on screen | to check | no | expected |
| Linux, `conch-voice` | (TUI) | yes | **global** | no | no | no |
| TUI, any OS | yes | no | no | no | no | no |

This table shows what the browsers document today. Every browser cell says "expected" until it has been tried (A10, A11, A16), and "to check" where the documentation is unclear: macOS whole-screen sound in recent Chrome, and screen sharing from Android Chrome. Once tried, it becomes the tester-facing table.

**What A11 (#444) tried, 2026-10-16.** The screen-share columns were tried only where this machine allows: Chromium on Linux, headless, in `make web-e2e` (`web/e2e/12-screen-share.spec.ts`), against the pinned LiveKit with Chromium's fake screen (1920x1080 at 30 fps) and its fake sound track. There, a share was published with its sound, a second browser watched it (frames and sound arriving) having asked for no microphone, a third was told who was sharing, and stopping by the sharer, by the browser's stop control, by leaving and by an operator each closed the view. A phone-wide window received a smaller simulcast layer than a full-screen desktop one. That covers the Linux row's share, share-sound (a captured sound track; a real tab's sound was not tried) and watch cells as far as a fake device can. Everything else is untried, because it needs a device or an OS this machine does not have:

| Cell | Result |
| --- | --- |
| Linux, Chrome: share, sound, watch | tried with fake devices in headless Chromium (above); a real tab's sound untried |
| macOS, Chrome or Edge: whole-screen sound ("to check") | untried: no macOS here |
| Android, Chrome: sharing ("to check") | untried: no Android device here |
| Windows, Chrome or Edge: share, tab and system sound, watch | untried: no Windows here |
| Firefox or Safari: share without sound, watch | untried: only Chromium is installed for the browser suite |
| iPhone or iPad: watch, "tap to play sound" | untried: no iOS device here |
| Android: watch, "tap to play sound" | untried: no Android device here |

These are left for a person with the devices (#474), whose results replace the "expected" and "to check" cells above.

A talk key that works while a game has focus needs a desktop app. That is Electron (ADR-007), after the alpha.

## 3. Decided

**By Nick** (recorded on #420, 2026-10-15):
- **Platforms:** Linux, Windows, macOS, and phones or tablets. The browser is the alpha client everywhere (ADR-008 §1).
- **No global talk key for the alpha.**
- **Screen sharing carries sound** where the browser can.
- **Invites are the way in.** There is no public signup. Testers may invite testers. Operators may put out an **open link**: anyone with it may join until it expires, and a use cap is optional and off by default.
- **About 30 at once**, many more accounts in total, including Nick's throwaway test accounts and AI agents.
- **One server; each group in its own private channels;** one-on-ones are two-member private channels.

**Adopted by this plan** from the recommendations on #420, under Nick's standing "use your recommendations". He can overrule any of them at review:
- **"Deleting" an account means disable plus hide.** The account leaves rosters and member lists, and stays in the audit trail. Operators get bulk cleanup: everyone from one link, or by name prefix.
- **Agents are never created by invites.** Testers bringing their own agents would be a separate invite kind, not planned.
- **Inviting is a per-person permission that only an operator grants** (Nick, 2026-10-16, #457). Nobody who joins through an invite may invite unless an operator grants it, directly or through an operator's invite. Those who hold it have:
  - a small allowance of outstanding personal invites (default 5);
  - single-use personal invites (default 7-day expiry);
  - an operator option to revoke an inviter's invitees too (off by default).
- **A second server stays one deployment step away.** That needs a per-server LiveKit room prefix and a per-instance deploy layout. A client keeps one profile per server (TUI: `CONCH_CONFIG_DIR`; browser: one origin per server), and the onboarding docs say how to switch.
- **No per-group moderators in the alpha.** Moderation stays operator-only.

## 4. Open decisions, with recommendations

Each gets confirmed or changed before its implementation issue is dispatched.

**O1. How long a browser stays signed in.**

Today the credential lives in `sessionStorage`, one tab at a time (D2): closing the tab signs you out. Testers will not paste a token each day.

*Recommend:* keep it in `localStorage` per origin, with:
- a "sign out everywhere" action;
- a credential lifetime set at invite (default 90 days, renewable by using it);
- the same CSP and no third-party scripts, which is what makes local storage acceptable.

The alternative is an HttpOnly session cookie. It needs CSRF protection on every write route, which Conch does not have, so it is the bigger change.

*Decided* (2026-10-16, recorded on #420; built in #439): the recommendation is adopted. A member's browser credential is kept in `localStorage` for the origin; sign out revokes it on the server (`DELETE /v1/credentials/current`) and sign out everywhere revokes all of the person's (`revoke-all`, now open to the principal itself). Invite credentials renew by being used, at most once a day. This amends D2 (#260) for members only: an operator's credential is still kept for one tab, after the warning.

**O2. What a joiner is.**

*Recommend:* a joiner is an ordinary human principal with the channels and grants its invite named. There is no separate guest kind. "Guest" is just a person whose invite was narrow.

**O3. Names.**

Names are unique per server today, and #420 left open whether that needs fixing. Uniqueness is also case-sensitive: today "Alice" and "alice" can both exist.

*Recommend:* fold case and apply Unicode confusable skeletons at redemption and at operator create, and let a person rename themselves. Renaming is new, small, and audited.

**O4. Minimum voice.**

*Recommend:* channel voice only. That means join and leave, mute, deafen, push-to-talk while focused or an open microphone with the browser's own noise suppression and echo cancellation, and who is talking.

Nets in voice (V5) follow the alpha. The alpha's groups are single rooms. Whispers stay text.

**O5. Who may share.**

*Recommend:* anyone in the room may share, one share per room at a time. Whoever is sharing can stop, and an operator can stop it.

**O6. Size of the share.**

*Recommend:*
- 1080p at up to 30 fps for screen content;
- simulcast layers so phones and slow links get a smaller one;
- measure one sharer to 29 viewers on the chosen droplet before the alpha opens.

If the droplet's bandwidth is the limit, the plan is a larger droplet or LiveKit's own egress limits. A second media server would only come after that.

**O7. The deployment.**

*Recommend:* one droplet running:
- `conchd` (systemd, data under `/var/lib/conch/<instance>`);
- Caddy for public TLS on one subdomain per instance;
- LiveKit (systemd or Docker), with UDP media ports open and TCP/TLS fallback through Caddy's certificate.

Backups are SQLite's online backup of the database plus `files/`, each night, to a DigitalOcean Space or other off-droplet storage. A restore is tried before testers join. Litestream stays optional.

Upgrades swap the binary from a tagged release and restart. Rollback is the previous binary, plus the backup if a migration ran. A droplet with 4 GB RAM and 2 vCPU is the starting point, sized after the O6 measurement.

**O8. Builds and versions.**

*Recommend:*
- Every release build carries its version (`-ldflags -X main.version=<tag>`); today every build says `v0.0.0-dev`.
- The web client shows the version, and so do `/healthz` and `whoami`.
- A test build is a tagged pre-release (`v0.x.y-alpha.N`) with release notes and a tester checklist. Promotion and rollback follow the existing release rules.

**O9. Reconnects.**

*Recommend:* server pings on every WebSocket (for example every 25 s), so a dead connection is noticed in seconds, not at a proxy timeout. Browser media reconnects through the LiveKit SDK, and the client shows the state.

## 5. The work, in order

Each item below becomes one issue in a **Private alpha** milestone. They are filed after ADR-008 is signed, except the ones marked *independent*, which do not depend on it and can start now.

**Now: the foundations**

| # | Work | Size | Depends on |
| --- | --- | --- | --- |
| A1 (#425) | Versioned builds: stamp the version, show it in web, `/healthz` and whoami; tagged pre-releases with notes (*independent*) | S–M | |
| A2 (#426) | Feedback workflow: bug and feedback issue templates, priority and type labels, the Project board (§6) (*independent*; the board needs Nick's `gh auth refresh -h github.com -s project`) | S | |
| A3 (#427) | Per-server LiveKit room prefix and a separate API key per instance (*independent*) | S | |
| A4 (#428) | WebSocket keepalive on every socket, server and clients (*independent*) | S | |
| A5 | The deployment: systemd unit per instance, Caddy, LiveKit on a public host, firewall, backups and a restore drill, upgrade and rollback (O7) | M | A1 |
| A6 | Invites on the server: schema, storage, audit, redemption, rate limits (with the proxy's client address), pause switch, operator administration; security review | L | ADR-008 |
| A7 | Lasting browser sign-in (O1) | S | ADR-008 |

**Next: the communications**

| # | Work | Size | Depends on |
| --- | --- | --- | --- |
| A8 | Invites in the web client: the landing page, choosing a name, invite creation for those allowed | M | A6, A7 |
| A9 | Names: case and look-alike uniqueness, rename (O3) | S | A6 |
| A10 | Browser voice: LiveKit JS, CSP and hygiene rules relaxed for it, mute, deafen, focused push-to-talk, talking indicator; the dependency signed off | L | ADR-008, A3 |
| A11 | Screen share: grant sources and server rule, share with sound, a watch view that works on a phone, simulcast (O5, O6); try the "to check" cells of §2 | L | A10 |
| A12 | Disable plus hide, bulk cleanup, operator list of principals, audit rows for principal and channel creation | M | A6 |
| A13 | Diagnostics: connection logs with request ids, LiveKit health, and an in-product "report a problem" that files build and environment (#332) | M | A1, A2 |

**Before testers: the exit**

| # | Work | Size | Depends on |
| --- | --- | --- | --- |
| A14 | Cross-group isolation e2e: one group finds nothing of another over REST, the sockets, search, files, voice, screen share and invites. A revoked tester's open sockets and LiveKit participant are gone at once. | M | A6, A10, A11 |
| A15 | The bandwidth measurement (O6) on the real droplet | S | A5, A11 |
| A16 | The exit scenario (§1) on a deployed build, by Nick with two or more testers, recorded like the P3 exit. It includes a real network drop: the web client catches up on missed messages with none lost or repeated, as P3-09 already checks for a server restart. | S | everything above |

**After the alpha:** a desktop app with a global talk key (Electron); nets in voice (V5); live transcription and captions; web files, search and threads. Translation, richer agent participation, and assemblies and deliberation (P4/P5) are deferred.

The existing voice-track issues stay where they are: #185, #189, #393, #394 and #396 in P8, and #381 and #353 in "P3 extensions". The alpha does not wait for them.

## 6. How it is built, tested and fed back

**The board.** One GitHub Project, [Conch](https://github.com/users/njdaniel/projects/14), is the working source of truth. Its fields:
- **Status:** Backlog → Ready → In progress → Review → Ready to test → Done;
- **Iteration:** one-week iterations, starting 2026-10-19;
- **Priority:** Blocker, High, Normal, Low;
- **Kind:** Bug, Feature, Chore, Research (GitHub reserves the name "Type");
- **Build:** the build an item was tested on.

Its views are: the current iteration by status; the backlog by priority; the "Ready to test" queue with the build each item is in; and progress toward the alpha milestone.

Project items are the repository's issues. A merged implementation closes its issue, and the Project keeps the item in "Ready to test" until Nick or a tester confirms it on a build. If automation would move it to Done on close, a linked acceptance issue holds it instead.

**An iteration.**
1. Pick a goal someone can see, and a few Ready issues.
2. Agents take the highest-priority unblocked Ready issue, through the existing dispatch, review and merge rules.
3. Whenever a coherent slice is ready, cut a tagged pre-release with notes and a short checklist of what to try.
4. Nick or testers record the result on the item.

At the end of the week, unfinished work is finished, split, or returned to the backlog. Nothing carries over silently.

**Feedback.** Testers report from inside the product (A13), or with the bug template:
- what they did, what they expected, what happened;
- the build version, OS and browser;
- whether it happens again.

Reports land in Backlog and are triaged weekly. Anything that stops joining, talking or privacy jumps the queue as a Blocker. Everything else is weighed against the milestone.

**Rules kept.** The existing review, security-review, schema-change, dependency and release rules all apply unchanged. This adds no second process.
