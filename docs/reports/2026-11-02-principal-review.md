# Principal review: P2 to private alpha, through v0.1.0-alpha.10

**Date:** 2026-11-02 · **Author:** Fable (Principal Engineer) · **Decision owner:** Nick
**Window:** since `2026-07-21-principal-review.md`: 477 merged PRs, ten pre-releases (`v0.1.0-alpha.1`, 2026-10-27, to `v0.1.0-alpha.10`, today). `main` is at 04a48d5.
**Headline:** The invariants hold. The core still needs no external process; every Go, Rust, web and desktop dependency is allowlisted; REST and WS send only `pkg/schema` shapes; all 42 approval-path PRs shipped or kept a full request → notify → resolve → audit test. Three things need work:
- MCP silently drops two message fields (#1171).
- The approval-path label check has blind spots (#1170).
- Since 2026-11-01, approval-path PRs have been merged by this session under your written authorization, not by you personally (rule 3). That needs a decision.

Separately, alpha.10 reached alpha before the `personal` preset was applied, which #1011 required. See Decisions 1.

**Method.** 477 PRs are too many to re-read line by line, and every one passed the five-gate review when it merged. This review lists them all (appendix) and spends its depth on four parallel read-only audits run against `origin/main`:
- approval-path PRs and the label check;
- single-binary and schema-first;
- API parity;
- the backlog.

Each audit's evidence is cited by file and line below.

## Merged work (477 PRs)

By release. The full one-line list is in the appendix.

- **To alpha.1 (353 PRs, 2026-07-21 to 10-27):**
  - P2 hardening closed with its exit gate (`e2e/p2-hardening`, #250).
  - P3 extensions and the V4 voice track: `conch-voice` in Rust, push-to-talk, devices, LiveKit, `conch.voice_control.v2`.
  - The web client (ADR-007) through private-alpha onboarding (ADR-009): invites, guests, password accounts (Argon2id), sign-in links, operator password + TOTP.
  - Delegated admin (owners, managers).
  - Attachments, search, mentions and notifications.
  - The first droplet deployment, with a restored backup made safe before it is served (#783).
- **alpha.2 – alpha.5 (35):**
  - Temporary servers and optional operator TOTP (ADR-011, ADR-012).
  - One-level replies and the private microphone test in the browser, and pausing invites.
  - `conch-server new/rm/list/upgrade` for the droplet (#888).
  - Voice robustness: per-platform audio backends, one playback owner, stalls ending a transmission.
  - The Windows W1 and Linux D1 desktop designs.
- **alpha.6 – alpha.8 (43):**
  - Reactions (#765) with the emoji picker and `:shortcode:`.
  - The desktop shell's first slices (D2-1 to D2-5, D2-9).
  - The phone browser layout and the voice screen companion.
  - Message editing (#845) and drafts kept across a reload.
  - Guests switched off for now (#1005), and short passwords allowed.
  - ADR-013 amended, ADR-014 proposed and its A1–A3a slices (#1012, #1013, #1014), ADR-015 proposed.
  - Browser UX audit 3.
- **alpha.9 (13):** core-flow fixes from audit 3, @mentions, reply context, links, a screen-share revocation fix.
- **alpha.10 (33):**
  - The one-community migration (#979) and member links (#985).
  - Permissions on routes, voice, share and invites (#1015, #1016, #984, #1131), the CLI's `admin permissions` (#1022).
  - The member directory (#983) and member channel creation (#981).
  - Desktop profiles, server allowlist, links and clipboard (#945, #946, #947).
  - The channel and member ⋯ menus (#1094, #1135), and voice fail-closed fixes (#1089, #1087, #1144).

**Scope and acceptance.** The audits found no PR that shipped outside its issue's scope in a way that matters. Two known deviations are recorded on their PRs: #985 kept `POST /v1/invites` open to members and moved that refusal to #990, and #479 deleted one test as a deliberate rule change. One shipped defect was found after merge: #1165, a demoted operator's member links survive the brake. It was filed today and should be in the next tag.

## Invariant status

| Rule | Status | Evidence |
|---|---|---|
| 1. Single-binary, dependency-free core | **OK** | `depgate`, `voice-depgate`, `web-depgate` and the desktop gate pass, and each allowlist matches its manifest exactly (Go 9, Rust 13, web 14, desktop 8). `conchd` starts and serves messaging, approvals and audit with no LiveKit, ntfy or Litestream: LiveKit is all-or-nothing (`livekit/config.go:134`), ntfy unset gives a nil notifier (`server.go:303`), and tests prove each absence (`TestUnconfiguredNtfyRecordsNoNotifications`, `TestVoicePresenceNotConfigured`, …). Minor: the gates check manifest → allowlist only, so an orphaned allowlist line wouldn't fail. There are none today. |
| 2. Schema-first | **Drift → #1171** | REST and WS are clean: all ~100 `writeJSON` sites send `schema.*` values, and no `map[string]any` reaches an encoder. Web types are generated (`schema-ts-check` OK). `voice/` round-trips the Go golden fixtures. **Drift:** `mcp.go:99-114, 247-257` projects `MessageV2` through marshal/unmarshal into `mcpMessageOutput`, silently dropping `thread_root_id` and `guest_author`, with nothing to stop the next field going the same way. Check (not drift): ~30 MCP tool structs live in `internal/server` by ADR-001's go-sdk exception and match today, but nothing ties them to `pkg/schema`. #1171 adds a fixture guard. |
| 3. Approval-path E2E | **OK, with process gaps → #1170** | 42/42 labelled PRs have a full-chain test or kept one green. 21 in-process full-chain tests pass on `main`; `e2e/dogfood` asserts the audit rows (`main.go:841`, `:959`, `:1161`); `e2e/p2-hardening` asserts four chains (`access.go:713-881`). No unlabelled PR changed approval logic. **Gaps:** the label check's filter misses the chain tests, `e2e/p2-hardening`, the audit export and the `can_decide` write paths; and `dogfood-check` doesn't run when the label is added after the last push (#72, #1010; both were caught by the push-to-main run). |
| 3. "Merged by Nick personally" | **Deviation, authorized → Decisions 2** | Since 2026-11-01 this session merges approval-path PRs (#1124, #1130, #1143 in alpha.10) under your written authorization ("merge prs when ready", approval-path included). GitHub shows every merge as `njdaniel`, so the record lives in the session's merge log, not in GitHub. |
| 4. API parity | **OK, two exceptions to record → #1172; one gap → #1171** | All 17 MCP tools have a REST route, and most share the handler code (`calls.go`, `reactions.go`). The CLI, TUI, web and desktop call only public routes: no client imports `internal/server`, and every web operation (`platform/operations.ts`, 78 entries) matches a route. **Exceptions:** agent approvals are MCP-only by design (`routes.go:312-315` `humansOnly`), and `conchd`'s offline operator commands have no route. ADR-001 records neither. **Gap:** MCP can't reply in a thread (#1171). |

Information, not drift:
- The `/v0` create routes are still used by the CLI and web.
- Five `Client` methods in `internal/cli/client.go` have no callers.
- Large REST-only areas (reactions, channel settings, directory, member channel creation) have no client yet. That's allowed, and the console issues (#1018–#1021) cover most of them.

## Backlog delta

- **Filed (6):**
  - #1161 (test-relaxed invite limits), #1163 (e2e 35 flake), #1165 (demoted operator's links: security, alpha.10 defect), #1167 (RUSTSEC-2026-0429): filed while cutting alpha.10.
  - #1170 (approval-path label check), #1171 (MCP fields and thread reply), #1172 (ADR-001 exceptions): from this review.
- **Closed (5):** #992 (superseded by #1018, #1020, #1021), #972 (duplicate of #1020), #838 (same as #1068, fix fe68fd6 on main), #962 (ADR-013 accepted, built in #976), #769 (D1 signed 2026-10-28).
- **Re-scoped:** #1095, #996 and #997 now read their #992 blocker as #1018, #1020 and #1021.
- **Open: 161 issues, 11 PRs.**
  - By milestone: Private alpha 46, P8 voice 6, P3 extensions 2, unmilestoned 105.
  - Three PRs are over a week old: #551 (README and logo, conflicts; the README still says "pre-alpha, no graphical client"), #680 (TUI voice v2, waiting on your by-hand run) and #832 / #88 (drafts; see Decisions).
- **Unblocked by merges, ready to dispatch:** #987, #982 (PR #1149), #986, #1017, #1018, #1148, #1165, #1054, #970, #930, #898, #934, #936, #849, #850, #766–#768, #186, #474.
- **Next tranche, in order:**
  1. **Server lane A, one after another** (they share `matrix.go`, `routes.go` and `store/invites.go`): **#987** (open sign-up; approval-path) → **#982** (PR #1149, rebase) → **#986** (redemption into defaults; approval-path) → **#1165**.
  2. **Lane B:** #1054 (a re-auth proof survives promotion; security, after #1165 in `roles.go`).
  3. **Lane C:** #1018 (web console shell), which gates #1019–#1021 and #1023.
  4. **Then:** #1017; #990 after #986; #991, #988 and #1150 after #982; #994 and #995; #989.
- **Not yet executable cold:** #27, #332, #353, #396, #420, #455, #474, #481, #578, #672, #700, #754, #756, #784–#786, #974 (missing Files, Acceptance or Tests); #1103 (no Blocked-by); #1024 (open shape question for protocol-designer).
- **Release couplings still open:**
  - **#982 must not ship before #987.** On `main`, `PUT /v1/join/settings` with a private channel in `open_channels` silently makes it public, and only #987's refusal closes that. #1149 is held out of the merge queue until #987 merges.
  - **#990** must ship the member refusal on `POST /v1/invites` with the web panel's move to v2.
  - **#1134** retires v1 invites only after #990 and #994.
  - **#1051** must land before any permission beyond the initial eleven.

## Decisions needed from Nick

1. **The `personal` preset on alpha (now).** #1011 requires your personal server to have `personal` applied before a release with the member directory reaches it, and alpha has been on alpha.10 with the `group` defaults since 19:08Z today. That was my miss: I didn't check epic conditions before upgrading, and a note now says to. Under `personal`, members keep voice and screen share but lose the directory, channel creation and member links. If alpha is your personal server, run `conch admin permissions preset personal --server https://alpha.gankbears.com` (owner sign-in). If it's a group server, say so and I'll mark the condition as not applying.
2. **Rule 3, approval-path merges.** CLAUDE.md says you merge approval-path PRs personally. Since 2026-11-01 you've authorized this session to merge them when they're reviewed and green, and it has (#1124, #1130, #1143). Either amend rule 3 to "merged by Nick, or by the principal under his recorded authorization", with where that authorization is recorded, or withdraw the authorization so approval-path PRs wait for you again. A new session asks once either way.
3. **ADR-001 exceptions (#1172).** Record agent approvals as MCP-only (recommended: `humansOnly` keeps an agent credential off the human approval routes), or ask for agent REST approval routes. Also record `conchd`'s offline operator commands as host-only.
4. **Old PRs.**
   - #832 (draft, 8 days; changes CLAUDE.md, AGENTS.md and ci.yml): finish or close.
   - #88 (draft spike report, 28 days): merge as partial, or close in favour of #185 and #189.
   - #680 (TUI voice v2): your by-hand run, or merge with that row moved to a follow-up.
5. **Issues waiting on your word:**
   - #626 (UX audit 2 approval, two audits since): close?
   - #702 (onboarding epic): every child is closed once #718 folds into #449. Close it?
   - #597 and #965: confirm the remaining manual checks moved to #474 and #970, so both can close.
6. **Release readiness (alpha.11).** Not ready, and nothing is asked of you yet:
   - #1165 (security) and #987 are the first two items to land.
   - The four preconditions are checked fresh before any tag: CI green on `main`, `e2e/p2-hardening`, this report (no unresolved drift once #1171 and #1170 merge), and your sign-off.

## Appendix: every merged PR, by release

<details><summary>0.1.0-alpha.1 (353 PRs)</summary>

- #856 issue #839: update unread badges live
- #827 schema: owner mark, channel managers, role change, and add by name (#787)
- #823 issue #813: check Go VCS stamps outside linked worktrees
- #826 issue #825: bump tokio to 1.53.3 in voice/Cargo.lock for RUSTSEC-2026-0386
- #822 issue #800: bound join pressure in the settings race
- #820 issue #814: password change and reauth answer unauthenticated for a dead credential
- #819 issue #714: the browser's operator sign-in, setup, recent-auth and account controls
- #818 issue #717: the real-server isolation check covers guests and the sign-in gates
- #817 issue #812: guest activity is recorded often enough at the minimum idle bound
- #816 issue #802: the channel-rename spec waits for each page's message socket
- #811 issue #804: the rename and create-channel tests wait for the address bar
- #810 issue #803: startVoiceWorld lets conchd choose its port and says why it failed
- #808 issue #800: TestOpenChannelsRaceJoins races until it has proved something
- #807 issue #794: v2 invite redeem checks the password against the server name, and the name before hashing
- #806 issue #798: refuse reserved-namespace server names, and reserve the name offline
- #805 issue #713: the browser's front door, guest visits, accounts and return sign-in
- #801 issue #715: CLI and TUI parity for guests and password sign-in
- #799 issue #760: the server's name is a reserved permanent name
- #795 issue #711: accounts from the bare address, and live guest conversion
- #797 issue #781: the screen-share late-signal route passes LiveKit's close on in order
- #796 issue #782: the voice sweep deletes orphaned rooms; a restored start retires every room
- #783 issue #716: a restored backup is made safe before it is served
- #759 issue #728: rename a channel in the browser
- #761 issue #710: guest entry policy for the bare address and invites
- #750 issue #727: operators rename a channel on the server
- #726 issue #697: delegated administration design and ADR-010 (Proposed)
- #743 issue #740: await_decision withdrawal test waits for the first poll
- #730 issue #679: ntfy topic chain test gives the decision a 5 s window
- #734 issue #733: ntfy transport-error tests dial port 0, not a freed port
- #749 issue #712: members make device links for their own account
- #721 issue #628: unreachable-ntfy subtest dials port 0, not a freed port
- #747 issue #735: conch-voice tests hold their refused address
- #751 issue #709: operator password + TOTP sign-in, setup scope and recovery
- #745 issue #708: password enrollment, change, reset and the recent-auth proof
- #737 issue #707: Argon2id passwords and member sign-in
- #748 issue #706: guests refused member actions; no guest decides an approval
- #736 issue #719: guest history bound on every read surface
- #732 issue #705: temporary guest identities, expiry and attribution
- #746 issue #644: the joined strip says when voice is unavailable
- #744 issue #683: a deafened page is deafened before a new room connects
- #741 issue #645: sign-out waits for its session's end before revoking
- #742 issue #643: controls that cannot act look and say so
- #739 issue #677: a capture failure while connecting keeps a newer mic choice
- #738 issue #642: share notes say the next step, and "You stopped sharing." clears
- #731 issue #641: phone voice strip stays one row with eight in voice
- #729 issue #704: onboarding contracts for guests, passwords and sign-in
- #724 issue #629: tx timing tests calibrate on a median and retry, bounded
- #725 issue #695: operators create channels in the browser
- #723 issue #675: external runtime test waits for each frame it asserts
- #722 issue #696: design note for renaming a channel
- #701 ADR-009: temporary guests, password accounts and return login (#694)
- #693 dispatch: write down when a struggling worker is escalated (#678)
- #692 issue #656: conch-voice managed speaks conch.voice_control.v2
- #688 issue #655: conch-voice switches the microphone and speakers inside a running session
- #687 issue #654: conch-voice open microphone: the microphone's own sound opens the gate
- #685 issue #661: web voice, turn one person down on this page only
- #684 issue #631: no frame reaches the track after it is muted
- #682 issue #660: web voice, choose the speakers voices play on
- #681 issue #669: web voice, one microphone capture at a time
- #676 issue #665: scrub control and bidi characters from conch-voice's usage errors
- #674 issue #673: bump rustls-webpki to 0.103.16 for RUSTSEC-2026-0382
- #668 issue #653: conch.voice_control.v2 (spec, owner sign-off)
- #671 issue #659: web voice, a voice processing switch for a clean microphone
- #667 issue #658: web voice, choose the microphone, never fall back by itself
- #670 issue #652: TUI voice lists, chooses and saves the microphone and speakers
- #666 issue #650: conch-voice devices --json
- #664 issue #651: conch-voice config set saves the devices to voice.toml
- #663 issue #649: schema conch.voice_devices.v1, the device list
- #648 issue #626: second browser UX audit, after the UX pool and the stability wave
- #640 issue #619: expectClean lets LiveKit's join token be only in its validate URL
- #639 issue #609: a test-only conchd flag relaxes the per-address invite limits
- #638 issue #579: the owner-loss test accepts a final state the helper says it lost
- #637 issue #622: read every 204 to its end in the shared request helper
- #635 issue #576: TestRelayRunsItsClocks sweeps until the relay holds the intervention
- #636 issue #621: conch-voice-api tests hold their "nothing listening" port
- #634 issue #610: a stale voice room shown again re-notifies presence
- #630 issue #611: conch voice where, voice occupancy for the CLI
- #627 issue #573: deploy-test binds port 0, so parallel runs never cross
- #625 web: voice occupancy, seen from any channel (#596 c)
- #623 web: quiet the console: read status-only voice answers to the end (#595)
- #624 web voice: the share notes take one line; the phone strip fits in 200 px (#600)
- #620 e2e: push to talk shows speaking as the open microphone does (#599)
- #617 web: an empty channel says so; the composer says when sending is paused (#592)
- #618 web voice: the strip's controls and notes say what is true (#591)
- #616 web voice: keyboard push-to-talk; focus after sign-in and Leave voice (#590)
- #613 conchd: voice occupancy, the server half (#596 b)
- #615 web voice: roster names come from the voice channel (#588)
- #614 web voice: the roster never hides people; the strip's controls stay put (#587)
- #602 docs: voice occupancy design note, accepted (#596 a)
- #612 web: the phone's channel row shows there is more, keeps counts with their channels (#601)
- #608 web voice: a live roster when not joined, and an honest state when it cannot be read (#585)
- #606 web voice: a refused or missing microphone says so and stops retrying (#586)
- #604 web: invite panel wording by kind, a list that follows use, the dead-invite page (#593)
- #605 web: a message log at its end stays at its end when its height changes (#589)
- #607 web voice: Join voice names its room; the connected channel is a way back (#584)
- #603 web: members column labels counts, groups people, folds empty agent sections (#594)
- #583 docs: browser UX audit of the alpha web client (#577)
- #582 e2e: refused voice, a LiveKit restart and an outage through the TUI (#559)
- #581 conch-voice: three tests bounded by what they check, not by load (#572)
- #580 conch-voice: never drop an ack for a slow owner (#571)
- #575 issue #565: share up to 4K at 60 fps, the top layer as captured
- #574 issue #562: Prometheus /metrics for conchd on a separate loopback listener
- #569 issue #560: e2e/isolation also looks for A's LiveKit SIDs in B's lk log
- #568 issue #558: /voice join right after /voice leave is not warned about
- #570 issue #555: the slow-owner managed test reads before it asks
- #564 web: open #general by default (#556)
- #563 tui: open #general by default (#557)
- #561 web: drop the share content choice; every share is motion (#554)
- #553 e2e/isolation: stop flagging a random request id or room name (#544)
- #552 Voice inside the TUI: /voice commands wired to the managed helper
- #548 Browser UI: polish the alpha client to the mockups
- #550 conch-voice managed mode: the helper side of the TUI voice contract
- #549 Go supervisor for the managed conch-voice helper
- #547 TUI: layout, navigation and chat polish
- #546 The managed conch-voice control contract (conch.voice_control.v1)
- #534 Add SECURITY.md now that private vulnerability reporting is on
- #543 voice (web): a room renewed while a share is publishing resumes the share
- #533 A resumed share waits for LiveKit's own grant before publishing
- #290 Add technical-planner agent for issue refinement
- #531 conch-voice uses the only stored login's server
- #532 ntfy transport-error test: a timeout a loaded machine cannot reach
- #526 Revoking one credential closes its sockets at once
- #530 events: read the member baseline before the first roster goes out
- #529 CONTRIBUTING.md for outside contributors
- #524 conch uses the only stored login's server
- #521 A screen share carries on when the voice room is renewed
- #520 Members who leave or are hidden drop out of open pages
- #518 scripts/dev-stack.sh: the whole stack locally in one command
- #514 A reconnect no longer dodges the 3 s limit on excused passes (#509)
- #513 The Report a problem panel stays on screen on a phone (#511)
- #512 A person sees their own sign-ins and can end any one of them (#496)
- #510 Two more ways voice audited an untrue voice_transmit_unreported (#501)
- #507 conch-voice reports a press before it unmutes, and mutes before it reports the release (#502)
- #506 Store tests open a migrated copy instead of migrating each database (#498)
- #505 Tell the sharer why their share ended, and let a release name its claim (#487)
- #504 Cross-group isolation and revocation, end to end (#447)
- #503 The browser reports a press before it unmutes, and mutes before it reports the release (#495)
- #499 An honest sign-out, and an operator's one-time link back into the same account (#493)
- #497 internal/server tests under -race take about a quarter of the time (#472)
- #494 Load-test script and bandwidth report template (preparation for #448)
- #492 Alpha tester guide and exit run sheet (preparation for #449)
- #489 Screen share in the browser: share with sound, watch on a phone (#444)
- #488 Diagnostics: request ids, connection logs, voice health and report a problem (#446)
- #486 whoami says whether the caller holds voice and how many invites are left (#468)
- #491 Your own message no longer shows twice while its POST answer is in flight (#490)
- #479 Approvals: count only decisions by members who hold the right now (#453)
- #485 Sign-in will not overwrite a stored sign-in it could not check (#469)
- #484 The lobby fits a phone (#467)
- #483 Hide accounts, bulk cleanup, an operator principal list, creation audit rows (#445)
- #475 Screen share on the server: a claimed share, one per room (#443)
- #478 Invite-panel tests wait for the channel checkboxes (#477)
- #482 Ignore the zip 0.6.6 advisories we never execute (#480)
- #470 Invites in the web client: the landing page, joining, and Invite someone (#440)
- #464 Names that look alike clash, and rename (#441)
- #473 CI: give go test -race 25 minutes per package (#471)
- #463 Operator administration of invites, the pause switch and conch admin invite (#438)
- #466 Let the storage listener attach before the other-tab test fires (#465)
- #462 A browser sign-in that lasts, sign out and sign out everywhere (#439)
- #461 Invites on the REST API: create, preview, redeem, rate limits and the client address (#437)
- #456 Voice in the browser with the LiveKit JS SDK (#442)
- #460 The long-channel cap test no longer takes nineteen pages (#459)
- #458 Invite joiners cannot invite unless an operator grants it (#457)
- #452 The deployment kit: systemd, Caddy, LiveKit, backups and a restore drill (#435)
- #451 Approvals: only members trusted to decide may decide (#450)
- #454 Invites: schema, storage and store logic (#436)
- #434 Link the Conch board, the alpha plan and the report forms (#426)
- #431 Every build says which it is (#425)
- #429 ADR-008: the private communications alpha, its plan, and the roadmap (#420)
- #433 WebSocket keepalive, so a dead connection is noticed (#428)
- #432 A per-server LiveKit room prefix (#427)
- #430 Bug and feedback issue forms for testers (#426)
- #424 P3 is done; its optional extensions move out (#423)
- #422 P3 status: #410 and #411 are merged (#421)
- #415 conch-adapter: answer people, not other agents, by default (#410)
- #419 mcpclient: do not follow redirects with the agent's bearer (#418)
- #417 Record the P3-14 agent run; bring P3's remaining work up to date (#412)
- #416 conch-adapter: strip terminal control sequences from text replies (#411)
- #414 store: pin the credential clock in TestEndingASessionEndsItsRuns (#413)
- #409 Push run frames for runs that end with their session (#405)
- #408 conch-adapter relay: heartbeat and retry once on session_offline (#406)
- #407 Bound a credential's open runs; ending a session ends its runs (#402)
- #404 conch-adapter: heartbeat and retry once on session_offline (#403)
- #383 README: TLS and a reverse proxy, documented and optional (#380)
- #401 An offline session takes no new runs or activity (#392)
- #397 Web: the browser says voice is not available (#395)
- #400 The automated P3 suite on a non-loopback address (#382)
- #398 conch-adapter relay mode (#387)
- #399 Pause and resume controls in the TUI and the web client (#346)
- #390 e2e/p3-local on the LAN: --advertise (#379)
- #391 Bound open runs per session and activity entries per run (#388)
- #377 conch-adapter: pause and resume (#345)
- #389 Reporting from an external runtime over the REST contract (#386)
- #384 Design note: a generic webhook/event adapter (#359)
- #385 Web: unread counts in the channel list (#373)
- #374 TUI: unread counts in the channel list (#372)
- #363 conchd embeds the web client (build tag conchweb) (#347)
- #378 conch-adapter: local-model (Ollama) profile, and three runtimes at once (#344)
- #376 Web e2e: the net-and-whisper test waits for the confirmed message (#375)
- #371 TypeScript wire types generated from pkg/schema (#348)
- #370 Web: Runs view and a run's timeline (#350)
- #369 p3-local: P3-09 and P3-04 hold under heavy load (#364)
- #368 conch-adapter: the runtime subprocess no longer gets the agent's Conch token (#361)
- #367 Read state: a per-person read mark and unread counts (#351)
- #362 conch-adapter seeds its cursor before registering a session (#357)
- #366 TUI: Runs view and a run's timeline (#349)
- #365 Web: composing to nets and whispers (#352)
- #360 conch-adapter: Codex runtime profile (#343)
- #358 P3 E3: first-prototype exit record and roadmap (#286)
- #342 P3 E2b: the browser suite (#285)
- #341 P3 E2a: the automated P3 suite (#284)
- #339 issue #283: e2e/p3-local, the runner and the walkthrough
- #356 Web: open a channel with its newest page; older pages with before= (#334, web half)
- #338 issue #282: the web client's members and agents panel, run detail and interventions
- #335 issue #281: chat in the web client
- #333 issue #280: the web client's sign-in and lobby
- #300 issue #279: the web client's toolchain, scaffold and gates
- #355 TUI: read a channel's newest page in one request (#337)
- #336 issue #334: the newest page of a channel's messages (order=desc, before=)
- #330 issue #275: conch-adapter, a generic subprocess adapter
- #328 issue #294: the lock-out check waits on SQLite's busy timeout, not 2 s
- #326 issue #235: message search costs grow with the channel, not the database
- #327 issue #293: a forced shut is never shown beside "transmitting"
- #325 issue #322: the ntfy transport test does not ask the machine's resolver
- #306 issue #267: MCP tools for sessions, runs and interventions
- #324 issue #323: rustls 0.23.46 for RUSTSEC-2026-0346..0349
- #321 issue #320: close the events socket when the session it keeps alive ends
- #318 issue #317: GET /v1/runs order=desc, paged back with before=
- #319 issue #312: the TUI recreates its own session when the server ended it
- #316 issue #278: TUI run detail and interventions
- #315 issue #309: at most 32 open WebSockets per credential
- #311 issue #277: the TUI's members line and Agents view
- #314 issue #313: a roster read cut short by the client leaving is not an error
- #310 issue #301: the sweeper ends sessions of an expired credential
- #308 issue #269: the events socket, /v1/events/ws
- #307 issue #303: conch-voice help names the configuration directory rule
- #305 issue #287: interventions on a run
- #304 issue #266: runs and activity, visible only to those who can see what they answer
- #302 issue #273: CONCH_CONFIG_DIR, for conch and conch-voice alike
- #298 issue #265: sessions, heartbeat and expiry
- #299 issue #270: WebSocket tickets, so a browser can open a socket
- #297 issue #276: the TUI shows author names and times
- #292 issue #272: conch admin, the operator's setup commands
- #295 issue #268: /v2/ws?after=<id> subscribes, replays, then streams
- #296 issue #264: schema for sessions, runs, activity and interventions
- #291 issue #271: serve a built web client at /app/ with --web-dir
- #288 issue #274: the members list carries each member's name and kind
- #261 docs: client UI architecture, detailed UX and P3 local prototype
- #262 issue #258: the sound card's clock paces the transmit and mix tasks
- #257 issue #184: the real microphone and speakers through PipeWire, with hot-plug
- #256 docs: preserve GUI and TUI design mockups
- #254 issue #253: the roadmap says P2 Hardening is done
- #252 issue #251: ADR-006 amended, dmn-audio comes from its own public repository
- #250 issue #81: the P2 exit gate, one command over an upgraded P1 database
- #248 issue #240: an agent's post with a file attachment is refused as invalid, not as a server error
- #239 issue #237: an MCP request is held to the size a REST post may be
- #249 issue #245: the unreachable LiveKit address is reserved, not merely free
- #247 issue #25: one level of replies, each reaching only people who can see its root
- #246 issue #244: no invisible characters in tracked files, and a check that keeps it so
- #243 issue #238: a webhook body is measured once, as it was sent
- #242 issue #234: the plain CLI no longer puts server text on the terminal raw
- #241 issue #23: files can be uploaded, attached to messages, and fetched by exactly who can see the message
- #236 issue #22: messages can be searched within a channel, behind the same audience boundary as listing
- #233 issue #231: clients read any message a server can send
- #232 issue #26: the principal review runs on a schedule and posts its summary into Conch
- #230 issue #21: the audit trail can be exported as JSON Lines, by operators, over REST and the CLI
- #229 issue #228: a rate-limited error carries its retry delay as a number
- #227 issue #80: an agent's manifest rate limits are enforced, per principal and capability
- #226 issue #83: the TUI takes the mouse: a click selects, the wheel scrolls message history
- #225 Land V4 on main: conch-voice join, the talk key, and the three-client e2e (#183, #185, #188)
- #224 cli: refuse unsafe server hosts and preserve segment escaping under %2F (fixes #208)
- #221 issue #205: the approval views always fit the terminal
- #223 docs: refresh README product direction and current status
- #199 issue #182: conch-voice-control: configuration, key decoding, push-to-talk, the connection policy
- #207 issue #178: measure the LiveKit Rust SDK behaviours the client relies on, and correct the design
- #201 issue #180: conch-voice-api: the stored login, the voice REST calls, the presence socket, the wire types
- #198 issue #181: conch-voice-audio: the transmit gate, jitter buffers, the mixer, test sources and sinks
- #193 issue #177: the voice/ Cargo workspace, its pinned toolchain, gates and CI
- #219 issue #65: GET /v1/approvals/{id} reads one approval in any state
- #212 issue #170: a notification whose ntfy topic is empty is recorded as not attempted
- #200 cli/tui: sanitize user name in status bar view
- #195 issue #194: TestRehydrateRearmsTimers no longer depends on who escalates
- #220 issue #204: the server refuses names that can rewrite a log line or a terminal
- #217 issue #214: the TUI refuses a background start instead of sitting stopped
- #216 issue #141: creating a webhook hook writes a hook_created audit event
- #215 issue #192: the TUI stops on SIGINT and SIGTERM sent as signals
- #213 issue #210: the removal hooks' store reads have a deadline of their own
- #206 issue #135: transmit reports from the client, audited, and checked by the poller
- #203 issue #202: place the cancellations in TestNotificationMatchesCommitUnderRandomDeadlines
- #197 issue #179: the voice transmit report
- #190 issue #175: design note for the conch-voice client
- #191 issue #187: the TUI shows who is in the channel's voice and who is talking
- #171 issue #129: V3 exit, headless join end to end (e2e/voice)
- #168 issue #121: V2 exit, the squad layout end to end (e2e/nets)
- #174 issue #173: conchd does not wait for LiveKit at startup; it does look
- #172 issue #167: prune retired voice room rows
- #166 issue #161: rotate a voice room when a token holder loses entitlement
- #159 issue #127: voice presence, enforcement and audit
- #151 issue #126: voice sessions — rooms named by conchd, 15-second join tokens
- #152 issue #120: conch-bot replies in kind
- #160 issue #128: conch voice status
- #150 issue #119: TUI — transmit target, /net, /w, scope badges
- #147 issue #118: CLI — conch send --net/--to, conch nets, scoped tail
- #163 issue #149: audit an agent's net-level post refusals
- #148 issue #117: MCP — audience on post_message and read_channel
- #146 issue #116: scoped messages — audience on post, one visibility function, v2 REST and WebSocket
- #165 issue #164: fit the notification body in one ntfy message; keep the topic out of errors
- #162 issue #157: deliver approval notifications whatever the title contains
- #169 issue #158: every committed approval transition gets a notification record
- #156 issue #155: notify and audit after an approval commits, whether or not the caller stayed
- #154 issue #153: always end a store transaction, whatever happens to the request
- #144 issue #143: one golangci-lint version for CI and local, no network-dependent config check
- #145 issue #115: nets — storage, membership, and REST management
- #142 issue #124: schema — voice session and presence shapes
- #140 issue #104: revocable webhook hooks, hashed tokens, owner-only database file
- #108 issue #105: fix flaky TestRehydrateRearmsTimers (one generous wait budget)
- #131 issue #114: schema — message envelope v2 with audience, nets, manifest grants
- #138 issue #125: LiveKit client and configuration from the standard library
- #111 issue #92: V1 exit — approvals bound to the caller, dogfood under auth, authentication on by default
- #110 issue #79: enforce agent manifests and channel membership (MCP, and agent credentials on REST/WebSocket)
- #102 issue #97: MCP endpoint authenticates with stored credentials; dogfood with an issued credential
- #139 issue #137: plain conch commands no longer query the terminal at start-up
- #136 issue #134: conch login reads the token without echo at a terminal
- #130 issue #123: design note — voice control plane
- #133 issue #132: TUI status belongs to a mode
- #112 issue #100: TUI resubscribes after a dropped subscription; stale backfill leaves the status alone
- #109 issue #101: disable a principal, revoke-all, and sockets that follow their credential
- #106 issue #91: conch login — CLI/TUI authenticate with a stored credential
- #107 issue #90: channel membership — store, REST surface, enforcement on REST and WebSocket
- #103 issue #89: authenticate REST and WebSocket — roles, operator bootstrap, whoami
- #99 issue #78: credential store, administration endpoints, resolver
- #98 issue #57: GET /v1/channels and TUI channel list from the API
- #96 issue #77: agent capability manifest — schema, persistence, REST
- #95 issue #94: mark ADR-003..006 Accepted; fold voice-spike findings into ADR-004/006
- #86 issue #85: propose nets-and-voice track — ADR-003..006 + roadmap amendment
- #74 Adopt long-horizon roadmap (P3-P10+)

Squash-merged, or merged through `v4-integration`:

- #70 issue #69: P2 process refresh — external-automation gate, CI process checks, release/security/issue-writer
- #72 Issue #71: add Conch auto-reply bot
- #824 issue #809: settle phone log scroll and keep slow PTT reports inside reportWait
- #828 Onboarding G9b: the approval chain with guests and converted members (#720)
- #829 issue #788: add store owner mark, last-owner guard, and SetRole
- #830 feat(store): add channel managers and manager-actor membership writes (#789)
- #833 Delegated admin C: owner and manager HTTP enforcement
- #834 Delegated admin D: manager channel renames with limits
- #835 Delegated admin E: CLI roles, owners, managers and add-by-name
- #836 Delegated admin F: browser member management and standing
- #837 Alpha: private image attachments and inline previews
- #848 Audit: everyday browser usability and follow-ups
- #855 Design: alpha moderation controls
- #857 Web: notifications, mentions and channel mutes (#840)
- #858 Web: search the current channel (#841)
- #209 issue #183: conch-voice join: connect, talk and listen without hardware
- #211 issue #188: three conch-voice clients end to end in e2e/voice, and in CI
- #218 issue #185: conch-voice: the talk key from the keyboard's event device, mute and deafen, the status display, and setup

</details>

<details><summary>0.1.0-alpha.2 (12 PRs)</summary>

- #881 release: CHANGELOG for v0.1.0-alpha.2
- #880 ops: local-only backups on a temporary instance; temporary-server docs (#869)
- #879 cli: temporary servers and optional operator TOTP (#868)
- #878 web: Skip, temporary banner, setup links, code only when enrolled (#867)
- #877 server: temporary servers and optional operator TOTP (#866)
- #876 ADR-012: the operator's authenticator as its own setting
- #874 web: private microphone test before joining voice (#846)
- #873 web: one-level replies in the browser (#842)
- #872 schema: server_mode on /healthz and whoami (#865)
- #871 Runbook: corrections from the first real droplet run
- #864 ADR-011: temporary servers (proposed)
- #859 release: CHANGELOG for v0.1.0-alpha.1

</details>

<details><summary>0.1.0-alpha.3 (2 PRs)</summary>

- #884 CHANGELOG: v0.1.0-alpha.3
- #883 web: pause and resume invites in Settings → Joining (#882)

</details>

<details><summary>0.1.0-alpha.4 (8 PRs)</summary>

- #921 CHANGELOG: v0.1.0-alpha.4
- #919 voice control: both readers refuse a lone surrogate escape; empty words aligned (#690)
- #912 voice: refuse an empty or space-padded audio.input/output in voice.toml (#691)
- #910 voice: in push-to-talk, a microphone that stalls in the release tail ends the transmission (#686)
- #914 auth: enrollment_proof_required for a first password without its proof (#815)
- #918 web: the Conch favicon in the browser tab (#917)
- #907 test: systemd-analyze verify reads no host drop-ins (#870)
- #889 docs: Windows desktop W1 design and capability matrix, desk research (#775)

</details>

<details><summary>0.1.0-alpha.5 (13 PRs)</summary>

- #939 CHANGELOG: v0.1.0-alpha.5
- #931 voice: choose the audio backend per platform (#892)
- #928 conch-server: reuse one SSH connection per run (#927)
- #926 e2e isolation: accept either 1008 reason on an expired guest's events socket (#920)
- #925 voice: one playback owner per audio source (#906)
- #924 voice: a failed events write is owner loss (#894)
- #923 web: tell an operator where to resume paused invites (#909)
- #908 ops: conch-backup.sh reads CONCHD_MODE by the conservative rule (#885)
- #888 ops: conch-server new/rm/list/upgrade for servers on the alpha droplet (#861)
- #915 docs: Linux desktop D1 design, reconciled with Windows W1 (#769)
- #916 e2e: P3-07 proves stale-before-offline from the events feed (#632)
- #911 voice test: tolerate a broken pipe writing to a helper that already exited (#689)
- #913 test: diagnose the cmd/conch test build's VCS-status failure (#886)

</details>

<details><summary>0.1.0-alpha.6 (16 PRs)</summary>

- #963 design: one-community model and ADR-013 (proposed) (#962)
- #980 CHANGELOG: 0.1.0-alpha.6
- #967 reactions on the server (store, REST, events, MCP) (#765)
- #971 voice (web): the push-to-talk tail is heard, the indicator follows real audio (#965)
- #968 desktop: REST through main, trusted bridge, session-only sign-in (#942, D2-3)
- #969 voice: screen companion and viewer grants, POST/DELETE /v1/channels/{channel}/voice/companion (#933)
- #966 the browser emoji picker and :shortcode: completion (#763)
- #961 web: the platform-adapter seam (#941, D2-2)
- #960 desktop: the credential vault (#944, D2-5)
- #959 schema: emoji catalog, generator and reaction keys (#953)
- #952 design: message reactions contract (#764)
- #955 web: the phone browser layout, Chat / Channels / Voice (#929)
- #956 voice: --events-pipe, the Linux events socket (#937)
- #951 desktop: the packaged local Electron shell (#940, D2-1)
- #949 voice: the poller folds companion and viewer identities into the person (#932)
- #950 schema: conch.voice_control_pipe.v1, the helper's attach line (#893)

</details>

<details><summary>0.1.0-alpha.7 (7 PRs)</summary>

- #1025 CHANGELOG: 0.1.0-alpha.7
- #1008 design: ADR-014 fixed roles with configurable permissions, and the admin console (proposed)
- #1010 Messages: edit your own recent text (#845)
- #1009 Remove guests for now: no new guests or guest entry points, data kept (#1005)
- #1006 ADR-013 amended: no guests for now (#962, #1005)
- #1001 clients: allow short passwords, drop the 15-character text (#977)
- #999 auth: remove the 15-character password minimum (#975)

</details>

<details><summary>0.1.0-alpha.8 (20 PRs)</summary>

- #1073 issue #1066: a finished send never widens a newer draft's audience
- #1072 release: widen the alpha.8 changelog to current main
- #1062 issue #1039: keep unsent drafts, their audience and open edits across a reload
- #1071 issue #1068: the login PTY test waits for echo to go off instead of racing it
- #1064 issue #1014: actor authority reads permissions, not role (ADR-014 A3a)
- #1070 issue #1069: blank random tickets and tokens before the isolation check looks for names
- #1060 issue #957: measure the open mic's hang on an injected clock in scripted tests
- #1065 issue #1063: allow channels_changed before ops2's member list in TestEventsChannelRenamed
- #1059 design: webcam video in voice rooms, ADR-015 (proposed) (#973)
- #1056 desktop: live feeds through main (WebSockets) (#943, D2-4)
- #1058 store: role_permissions, permittedTx and PutPermissions (ADR-014 A2) (#1013)
- #1057 desktop: Electron owner supervisor for conch-voice on Linux (#948, D2-9)
- #1053 web: :shortcode: aliases and ranking (#1036)
- #1055 auth: a password-and-code sign-in counts as recent proof (#1033)
- #1050 CHANGELOG: 0.1.0-alpha.8
- #1052 schema: roles and permissions (ADR-014 A1, additive) (#1012)
- #1026 docs: browser UX audit 3 (v0.1.0-alpha.7)
- #1049 Channel list follows membership changes live (#1027)
- #1047 web: the phone voice strip is one row and never covers the composer (#1028)
- #1045 web: a 401 for a replaced credential never signs the page out (#1000)

</details>

<details><summary>0.1.0-alpha.9 (13 PRs)</summary>

- #1096 release: add @mentions, reply context and links to the alpha.9 changelog
- #1091 web: show what a reply answers, count a root's replies, fit the phone thread form (#1038)
- #1092 web: http(s) URLs in message bodies become safe links (#1044)
- #1090 web: @mention completion, and a mark on messages that name you (#1043)
- #1093 release: CHANGELOG for v0.1.0-alpha.9
- #1085 web: first-run panel for a new owner; an empty state that tells the truth (#1032, #1034)
- #1088 voice: trace a press and its release, and bound the first press's clear (#1041)
- #1086 web: attach any file the server accepts, shown as a file chip (#1035)
- #1077 voice: re-read share_screen under the room's claims, end shares on revocation (#1061)
- #1078 docs: browser UX audit 4 (v0.1.0-alpha.8)
- #1074 web: the phone top bar keeps the connection state readable (#1030)
- #1075 server, web: gzip the web client and show a loading screen (#1037)
- #1076 web: kept images follow their channel by id across re-sign-in (#1067)

</details>

<details><summary>0.1.0-alpha.10 (33 PRs)</summary>

- #1169 release: CHANGELOG for v0.1.0-alpha.10
- #1139 web: channel actions in a ⋯ menu per row: Invite people, Rename (#1135)
- #1153 server: a role change that withholds use_voice removes the person from voice at once (#1131)
- #1164 server: the test-relaxed invite limits raise the per-person create limit too (#1161)
- #1162 server: member links (v2 create, list, preview, joiners) and the may_invite brake (#985)
- #1168 voice: ignore RUSTSEC-2026-0429 (zip 0.6.6), as the other zip advisories are (#1167)
- #1151 voice: never drop the final state or the closing line; robust stall and owner tests (#1144)
- #1155 cli: conch admin permissions, and permissions in whoami (#1022)
- #1145 server: per-person voice permission route with immediate effect (#984)
- #1147 server: the member directory, and managers add any member (#983)
- #1158 test: TestStopEscalation/normal asserts escalation, not wall-clock time (#1136)
- #1143 server: members create channels; operator channel settings (#981)
- #1137 desktop: sign in only to a server release on the app's allowlist (#946)
- #1146 web: stop offering Edit after ten known edits or a final refusal (#1031)
- #1127 desktop: saved server profiles, isolation and switching (D2-6, #945)
- #1138 server, tests: fix the events-socket, server-choice and deploy-test flakes (#1104, #958)
- #1140 voice: time the open-mode hang by frames in tests, and count receive frames per tick (#1098)
- #1133 voice: withdraw a held screen share when its room is rotated and DeleteRoom fails (#1087)
- #1132 server: member permissions on voice, screen share and invites, and their live effects (#1016)
- #1130 store: migration 40 converts a server to one community (#979)
- #1124 server: permission route class, GET/PUT /v1/permissions, whoami and the live signal (#1015)
- #1129 desktop: external links, attachments and the clipboard through the shell (D2-8, #947)
- #1128 docs: mobile push delivery and notification privacy design (M3, #1108)
- #1126 docs: mobile app architecture proposal, ADR-016 (Proposed) and capability matrix (M1, #1106)
- #1125 docs: phone interaction spec and an accessible flow mockup (M2, #1107)
- #1102 web: the join form says "name", not "Principal", in its name refusals (#1082)
- #1122 web: confirm removing someone from a channel, and say what pausing invites stops (#1083)
- #1121 web: keep a half-typed reply across a reload, by thread root (#1080)
- #1105 web: a typed :name: becomes its emoji on the closing colon (#1081)
- #1101 schema: community model I1 — channel visibility, invites v2, the directories, voice permission (#978)
- #1100 web: problem reports for operators — list, read and mark filed (#1029)
- #1097 voice: fail closed on the post-grant liveness read; revocations wait only for claims running at the time (#1089)
- #1099 web: member actions in a ⋯ menu per row instead of inline buttons (#1094)

</details>
