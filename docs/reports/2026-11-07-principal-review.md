# Principal review: alpha.11 to alpha.16, moderation and AI administrators

**Date:** 2026-11-07 · **Author:** Fable (Principal Engineer) · **Decision owner:** Nick
**Window:** since `2026-11-02-principal-review.md` (which ended at `v0.1.0-alpha.10`): 174 merged PRs, #1142 to #1460, one of them that report (#1173). Six pre-releases, `v0.1.0-alpha.11` (2026-11-02) to `v0.1.0-alpha.16` (2026-11-06), and 17 PRs on `main` since. `main` is at 3d3b65a3, and all 28 CI jobs are green on it, `dogfood-check` and `p2-hardening-check` included.
**Headline:** The core invariants hold:
- The core still needs no external process.
- Every Go, Rust, web, desktop and Android-spike dependency is allowlisted, and the one new set (the Android spike, #1400) carries your recorded sign-off.
- REST and WS send only `pkg/schema` shapes.
- All 20 MCP tools have a REST twin.
- All 22 approval-path PRs shipped or kept a full request → notify → resolve → audit test. There are now 28 in-process chain tests, up from 21.

Four things need attention:
- **Rule 3's merge record** (last week's Decision 2, still open). Of the 22 approval-path PRs, 10 have no written OK from you anywhere on GitHub. See Decisions 3.
- **One small schema-first drift.** The admin door builds four dispatched bodies from anonymous structs (#1461).
- **One latent Go-scope gap.** The new `spikes/` Node tree isn't ignored (#1462).
- **AI administrators shipped in alpha.15 before their S9 dogfood run (#1336),** which the slice plan put "before release". See Decisions 4.

**Method.** As last week, every PR passed the five-gate review when it merged. This review lists them all (appendix) and spends its depth on four parallel read-only audits against `origin/main`:
- the approval path and its merge record;
- dependencies and schema-first;
- API parity;
- the backlog and this week's follow-ups.

I re-checked each finding I relied on myself, by file and line, before writing it here.

## Merged work (174 PRs)

By release. The full one-line list is in the appendix.

- **alpha.11 (12, 11-02):**
  - Open sign-up replaces the joining modes (#987).
  - Redemption into the default channels (#986).
  - The channel directory with self-join and self-leave (#982).
  - MCP thread replies and the two lost message fields (#1171, last week's drift).
  - A role change clears re-auth proofs (#1054).
  - The admin console shell (#1018).
- **alpha.12 (38, 11-03):**
  - The rest of the community model: hide members (#988), the directories in web and TUI (#991, #995, #993), the sign-in-only lobby (#990), CLI parity (#994).
  - The demotion brake on member links (#1165, last week's security defect).
  - The console's Roles, Channels and Members sections (#1019–#1021).
  - Add-by-id with `can_decide` (#1206).
  - Capped list reads in the CLI (#1198, #1203).
  - Open microphone by default (#1244).
  - The personal and group server guide (#1023).
- **alpha.13 (20, 11-04):**
  - Webcam C1–C6: the schema, `use_camera` under the voice brake, the claim, one-to-one, the CLI (#1238–#1243).
  - Open-vocabulary validators (#1051).
  - The community model's docs and its newcomer e2e (#997, #989).
- **alpha.14 (12):**
  - Camera groups and the phone camera (#1267, #1268).
  - "Who you see" and departed authors (#996).
  - The alpha.7 polish batch (#1042).
  - CI moved onto self-hosted runners. alpha.15 moved it back.
- **alpha.15 (43, 11-05):**
  - Channel archive and restore (#844, #1296).
  - Retiring `POST /v1/invites` (#1134).
  - Who issued each credential and hook (#1024).
  - Share audio on consent and native-voice watching (#898, #936, #934).
  - **AI administrators S0–S8** (#1326 design and ADR amendments; #1328–#1335).
  - Refusing server-only approval payloads in messages (#1345).
  - The approval-path label check's blind spots (#1170, last week's process gap).
  - CI on hosted runners, split into parallel jobs.
- **alpha.16 (32, 11-06):**
  - Cameras on by default on new servers (#1361).
  - ADR-016 accepted, and the Android spike's M4-A and M4-B (#1389, #1390).
  - The Linux desktop's native voice, D3-1 to D3-12 (merged, not supported; #754).
  - **Moderation M1–M4:** the contracts, the voice-only kick, channel bans, message removal (#849–#852).
  - Migration 46 (#998).
  - Push-to-talk opens on key-down in both clients (#970).
- **On `main` since alpha.16 (17):**
  - Moderation M5 (the isolation matrix, #853) and M6 (CLI, TUI and browser, #1439, #1440).
  - Reactions in the browser and the CLI/TUI (#766, #767).
  - A voice-only kick closes presence with `voice_removed` (#1409, #1456).
  - ADR-001's two parity exceptions (#1172).
  - The PTT gate's hard cap (#1453) and #965's onset bound (PR #1448).

**Scope and acceptance.** The audits found no PR shipped outside its issue's scope in a way that matters. Points to note:
- **#1424 (migration 46)** said "Held: Nick confirms the release condition before merge". That condition is met: #976's comment requires one tagged release with I2 (alpha.10), and your "merge 1424 when green, and go on alpha.16" is on #420.
- **#766 and #1024 are still open.** Their PRs (#1433, #1325) say "Closes", and they're the only two in the window where that didn't take. Both are on the close list.
- **No labels after alpha.16.** The 17 PRs since alpha.16 (#1433–#1460) carry no area or security labels: the dispatcher stopped applying them on 2026-11-06. The approval-path label check still ran on each, and found nothing. But the security-review and release-notes trails rely on those labels. I'll relabel those 17 and fix the dispatcher; that needs no decision.
- **Copilot review failed on quota on every PR in the window.** All review records are agent comments posted as `njdaniel`.
- **alpha.11's release sign-off isn't quoted** on its CHANGELOG PR (#1187) or on #420. alpha.12 to alpha.16 each quote yours.

## Invariant status

| Rule | Status | Evidence |
|---|---|---|
| 1. Single-binary, dependency-free core | **OK; one latent gap → #1462** | `depgate`, `voice-depgate`, `web-depgate` and `web-licences` pass offline. Each allowlist matches its manifest exactly: Go 9, Rust 13, web 14, desktop 8, Android spike 11. In the window, `go.mod` changed only its `ignore` block (#954, PR #1299). The voice manifest moved `dmn-audio` to a Linux-only target at the same rev (#930). `voice-pins.sh` is unchanged. The Android spike (`spikes/mobile/android/`, outside `make check` by design) carries your "sign off 1400" (2026-11-05) for its 11 npm deps, the build licences and the Gradle classpath. No new required process: the admin door dispatches in-process (`admin_dispatch.go:157-161`), and LiveKit and ntfy stay optional (`cmd/conchd/main.go:283-287`). **Gap:** `go.mod`'s `ignore` and `scripts/go-scope.sh:19` cover `web` and `desktop` only, so a spike dependency shipping `.go` files would re-break `go ./...` (#954's failure). |
| 2. Schema-first | **Drift (low) → #1461** | `schema-ts-check` and `schema-compat` are OK. No version bump in the window: the new names (`conch.admin_action.v1`, `conch.agent_manifest.v1`, `conch.message_removed.v1`) are additive. #1176's guard ties the MCP message and reaction projections to the golden fixtures (`mcp_projection_test.go`). The admin-door MCP tools return `schema.AdminCatalogResponseV1` and `AdminActionResultV1` directly. Web has no wire literals outside `web/src/schema/`; Rust's wire crates check the Go fixtures. **Drift:** `internal/server/admin_actions.go:392, 412, 458, 478` marshal the dispatched bodies from anonymous structs that duplicate `CreatePrincipalRequest`, `RenameChannelRequestV1`, `AddChannelMemberRequestV1` and `PutChannelMemberRequestV1`. The bytes are identical today, but a schema change wouldn't reach the door. |
| 3. Approval-path E2E | **OK** | 22/22 labelled PRs have a full-chain test or kept one green. Every new approval flow has its own: AI admins (`TestAIAdminDecisionNeverCountsFullChain`, `TestAdminActionGatedFullChain`, `TestMCPAdminActionGatedFullChain`, `web/e2e/49-ai-admin.spec.ts`, `e2e/dogfood/aiadmin.go:288`), archive (`TestArchivedChannelApprovalFullChain`, `e2e/dogfood/archived.go:242`), bans (`TestAdminBanChannelMemberFullChain`), message removal (`TestMessageRemovalOfAnApprovalCardChain`), the converted guest (`e2e/isolation/convertedapproval.go`). No unlabelled PR changed approval logic. The M2 kick is a direct, audited action with no approval. #1170's fix works: #1459's on-label canary ran and passed. **Open gap:** the label filter still misses the web approval UI (`web/src/AdminApprovals.tsx`, `AdminActionCard.tsx`) and `e2e/isolation/convertedapproval.go`. #1344 and #1324 matched no guarded path and were labelled by hand. This changes the approval-path check, so it's yours to review (Decisions 6). |
| 3. "Merged by Nick personally" | **Deviation, partly recorded → Decisions 3** | All 22 were merged by the session. GitHub records:<br>- **A written OK from you** for 7: #1149, #1177, #1181 and #1424 (#976's "merge all of #976's children when ready", 11-01); #1211 ("approve 1206" / "merge 1206 when reviewed"); #1297 ("merge 1296 when it's green"); #1363 ("merge it when it's green").<br>- **The 11-01 session authorization** for #1176.<br>- **Implied by a release instruction** for 4: #1344 and #1346 ("prepare the release when S7 and S8 are in"); #1419 and #1425 (the alpha.16 go names M3 and M4).<br>- **Nothing** for 10. #1178's body says "Nick reviews this one", with no record that you did. The other 9: #1257, #1295, #1324, #1337, #1338, #1339, #1343, #1351, #1398. Several of their bodies say "Nick merges".<br>CLAUDE.md:9 and ADR-000:100 are unchanged. |
| 4. API parity | **OK** | 20 MCP tools (`mcp.go`), each with a REST twin (`routes.go`). The agent approval tools are ADR-001's recorded exception (#1454). The admin door is REST first: `GET/POST /v1/admin/actions…` (`routes.go:247-253`) serve humans and agents. Each catalog action dispatches onto an existing route, guarded by `TestAdminCatalogMatchesRouteTable`, as the design records (`ai-administrators.md:9`, §8.4). Every new CLI and TUI command calls public routes only: hook list and revoke, community parity (guarded by `parity_test.go`), moderation, reactions, camera stop, archive, `admin ai-admin`. No client imports `internal/server`. `conchd ai-admin` is an offline host command under the ADR-001 exception. |

Information, not drift:
- `conch admin ai-admin create` composes four routes on the client (`admin_ai_admin.go:282-322`), so a client that dies mid-way can leave a half-made agent. Parity holds. A server-side atomic route would be a design choice, not a fix.
- The live-media and race gates still pending in `e2e/isolation/gaterows.go` point at #1217, which is otherwise done. #1467 moves them.

## Backlog delta

- **Filed (8), the next tranche.** All are executable cold, need no sign-off, Mac or hardware, and are milestoned Private alpha:
  - **#1461:** the admin door dispatches the `pkg/schema` request types (the rule 2 drift). server, security, S.
  - **#1462:** `go-scope` ignores and probes `spikes/` (the rule 1 gap). infra, XS.
  - **#1463:** the CLI's disable and role print what the person issued to others, and `conch admin principal revoke-issued` (#1325's follow-up). cli, security, S.
  - **#1464:** the web console shows the same list, with the owner's Revoke all (#1325's follow-up). web, security, M.
  - **#1465:** the TUI follows `conch.channels_changed.v1` (deferred in `membership-live.md` §2 and #844). cli, M.
  - **#1466:** the TUI marks archived channels and makes them read-only (#844's TUI half). cli, S. Blocked by #1465 (shared `model.go`).
  - **#1467:** live-media isolation rows for `camerasWithheld` and `endCameraIfWithheld`, with the notes moved off #1217 (#1258's pending gates). server, security, M.
  - **#1468:** isolation rows for a non-owner operator banning an owner, and the MCP tombstone read (#1438's "Not covered"). server, security, S.
- **Closed: none.** Closing is blocked for this session; the list is under "Close (Nick)".
- **Re-scope (mine to do next session, no decision needed):**
  - **#1336 (S9)** is a stub: no Files and an empty criterion. Its blocker #1335 is closed. I'll rewrite it from `ai-administrators.md` §10.
  - **Stale blocked-by lines.** These now wait only on a sign-off or a by-hand step: #1457, #1213, #1220 and #1222 (sign-off); #657 (PR #680); #1384 (by hand); #1041 (#970 closed); #185 and #186. The lines on #976, #1011, #964, #965, #973, #770–#775 and #1387 still name merged ADR PRs.
- **Not yet executable cold:**
  - missing Files, Acceptance or Tests: #27, #176, #332, #353, #396, #420, #455, #474, #481, #662, #672, #700, #754, #756, #784–#786, #974;
  - a stub: #1336.
- **Epics ready for a closure audit.** Every child is closed, but each needs its evidence written up first: #976 (community model), #770 (D2), #973 (webcam, if re-scoped to #1285 and C9), and #1326 (after #1336).
- **Open: 125 issues (117 + the 8 filed), 8 PRs.**
  - Issues by milestone: Private alpha 25, P8 voice 6, P3 extensions 2, P7 1, P10+ 1, unmilestoned 90.
  - One PR is over a week old: #680 (17 days, waiting on your by-hand run). Last week's #551, #832 and #88 are resolved.
- **Next tranche, in order.** Lanes run in parallel, and their areas are disjoint:
  1. **#1462 and #1461 first.** They clear this report's drift before alpha.17.
  2. **The cli lane:** #1463, then #1465, then #1466.
  3. **The web lane:** #1464.
  4. **The e2e lane:** #1467 and #1468, one after the other (both touch `e2e/isolation`).
  5. **Already filed and ready:** #843 and #847.

## Close (Nick)

Each was checked against its latest comments and the merged PR.

- **#766** (reactions in the browser): done by PR #1433 (925722f), which says "Closes #766". GitHub didn't close it.
- **#1024** (A12, who issued each credential and hook): done by PR #1325 (fce4c00), "Closes #1024". GitHub didn't close it.
- **#1217** (isolation rows for #1190's six gates): done by PR #1356. Three voice gates got live rows, and the rest name their covering tests. The camera notes that still cite it move to #1467.
- **#821** (refused guest joins take the write lock): superseded. Guest joins are off, so the path is unreachable over HTTP. My 2026-11-05 comment says "Closing as not planned", and the criteria went into #1004. Close as not planned.
- **#854** (moderation M6): split into #1439 and #1440, both merged (PRs #1444 and #1443). The leftover, a voice-kicked browser rejoining, was fixed by #1409 (PR #1458).
- **#964** (optional hide-members): both halves are closed, #988 (PR #1193) and #996 (PR #1281). It was kept open only "as the request".
- **#752** (emoji and reactions epic): #763–#767 are done. #768 (custom emoji) isn't queued and can stand alone.
- **#87** (V0 spike): its spike PR #88 was closed as superseded on 2026-11-05.
- **Carried from last week, waiting on your word:**
  - #626 (UX audit 2, two audits since);
  - #702 (the onboarding epic; every child is closed);
  - #597 (its remaining checks moved to #474 and #970).

## Decisions needed from Nick

1. **#1459: an approval-path merge.** It closes #1435: the batch read returns a timestamped empty summary for every visible message. That's the last ordering gap in reactions (#766, #767), and they're already on `main`.
   - It's reviewed, the security review passed, and `e2e/isolation` and the on-label dogfood canary passed.
   - One red check is stale: the Process-checks `approval-path-label` job ran before the label was added. Re-run it, then merge.
2. **Sign-offs:**
   - **#1457:** add `voice_removed` to `VoiceControlReason`. It's additive, with no version bump. Without it, the desktop shows a voice kick as an internal error.
   - **#1435** is already approved (2026-11-06) and lands with #1459.
   - Still waiting from earlier weeks: #1213 (owner protection on add), #1220 (message size bounds) and #1222 (paging `GET /v1/approvals`).
   - Optional: a one-line ack that #1325's additive `issued_by` fields need no bump, since its body asked.
3. **Rule 3, the merge record (last week's Decision 2, now 22 PRs).**
   - The invariant table lists the 10 approval-path PRs with no recorded OK from you, #1178 among them, which said you'd review it.
   - Either amend CLAUDE.md rule 3 and ADR-000 to "merged by Nick, or by the principal under his recorded authorization", and say where that record lives (I suggest the PR itself, as a quoted line); or tell me to stop merging approval-path PRs until you say so per PR.
   - Either way, I'll add a quote of your OK to each of those 10 PRs if you confirm you gave it.
4. **The MCP connection and the AI-admin dogfood (#1336).**
   - AI admins shipped in alpha.15, but S9, the canary the slice plan put before release, hasn't run on a real server. An AI client disables a test member through the gated path, you approve from the phone, and the audit chain is asserted.
   - The scripted half, `e2e/dogfood/aiadmin.go`, is green in CI.
   - To run S9, run `(umask 077; conch admin ai-admin create fable-admin > ~/.config/conch/alpha-admin.mcp.json)` on alpha and point this session's `conch` MCP entry at it. The entry refuses connections today (ECONNREFUSED).
   - I'll rewrite #1336 to the template first. Should S9 gate alpha.17?
5. **Mac builds:** #1401 (iOS scaffold M5-A, which also needs your sign-off on its one dependency line), then #1403 (M5-B) and #1417 (M5-C), which are stacked on it.
6. **Android dependencies:** #1413 (M4-C) needs your line-by-line sign-off under ADR-016 Decision 8 on `@livekit/react-native` 3.1.0, `@livekit/react-native-webrtc` 150.1.1 and `livekit-client` 2.22.3. #1416 (M4-D) is stacked on it and adds no dependency. Also yours: the approval-path label check's remaining blind spots (rule 3 row). I'll file that issue for your review if you want it.
7. **By-hand checks:**
   - **#680:** TUI voice v2, 17 days old. Run it by hand, or merge with that row moved to a follow-up.
   - **#1355:** the operator setup QR code. Scan it with a real authenticator app; the security review passed.
   - **#965:** a by-ear check on alpha.16. PR #1448 bounds open-mic onset at a p90 of 80 ms, measured with a fake microphone.
   - **#1041:** catch a "transmitting" linger with the trace on alpha. It didn't reproduce in 60 fresh joins.
   - Also by hand, not urgent: #1384 and #1385 (desktop D3 acceptance), #185 (COSMIC) and #1285 (camera on the droplet).
8. **Closes:** the list under "Close (Nick)". The ones you named are #766, #1217, #1024, #821 and #854. This review adds #964, #752 and #87, and carries #626, #702 and #597.
9. **Release readiness (alpha.17).** Not ready yet. Nothing is asked of you until the items below are done.
   - **Content:** 17 PRs since alpha.16: moderation M5 and M6 in every client, reactions in the browser, CLI and TUI, the `voice_removed` close, and the PTT hard cap. `[Unreleased]` in `CHANGELOG.md` is empty, so a CHANGELOG PR comes first.
   - **Precondition 1 (CI green on `main`):** met at 3d3b65a3. All 28 jobs passed, including `dogfood-check` and `p2-hardening-check`.
   - **Precondition 2 (`go run ./e2e/p2-hardening` on the release commit):** run at tag time, not in this review.
   - **Precondition 3 (no unresolved drift):** not yet. #1461 and #1462 are small and go first. The rule 3 record (Decisions 3) needs your answer or your explicit acceptance.
   - **Precondition 4 (your sign-off):** not given.
   - **Couplings:**
     - #1459 should land first, so reactions ship without their ordering gap.
     - #1457 lets the desktop name a voice kick. The desktop isn't a supported client (#754), so this doesn't block.
     - Whether #1336 gates the release is your call (item 4).
     - No epic deploy condition applies. #1011's personal preset doesn't apply to alpha, per your 2026-11-05 "keep alpha as group".

## Appendix: every merged PR, by release

<details><summary>0.1.0-alpha.11 (12 PRs)</summary>

- #1187 docs: CHANGELOG for v0.1.0-alpha.11
- #1184 web: private channels show a lock; operators make a channel public, private or default from its menu (#1183)
- #1181 server: redemption lands joiners in the default channels; legacy invites and v1 projections (#986)
- #1149 channels: the channel directory; members join and leave channels themselves (#982)
- #1176 server: MCP messages carry thread_root_id and guest_author; post_message replies in a thread (#1171)
- #1177 server: open sign-up replaces the joining modes; the bare address admits no guests (#987)
- #1175 web: the admin console shell and its Server section (#1018)
- #1174 store: a role change clears the principal's recent-auth proofs (#1054)
- #1173 docs: principal review 2026-11-02 (P2 to alpha.10)
- #1166 web e2e: the phone voice strip waits for the roster to settle before tapping (#1163)
- #1154 web: the message log is one tab stop, and its controls are named by author and time (#1040)
- #1142 web: no request known to fail after a password change or a lost channel (#1084)

</details>

<details><summary>0.1.0-alpha.12 (38 PRs)</summary>

- #1250 CHANGELOG: v0.1.0-alpha.12
- #1247 web: open microphone by default, remember the choice, one-tap switch (#1244)
- #1249 docs: running a personal or a group server (#1023)
- #1245 voice: the sidebar names who is in each channel's voice (#1103)
- #1229 web: Disable joiners in the console's Invites list (#1228)
- #1197 web: sign-in-only lobby and member links; v1 invites for member managers only (#990)
- #1230 cli: conch admin hook list and revoke (#1228)
- #1231 web: Add to a channel uses the add-if-absent form and drops its pre-read (#1227)
- #1216 web: the community list follows conch.directory_changed.v1 (#993)
- #1225 store: a disabled person is listed as nobody's manager (#1210)
- #1236 web: the late-secret test waits 5 s for its line (#1235)
- #1234 web: Composer draft tests wait for the preview's object URL effect (#1002)
- #1233 web e2e: spec 35 retries a lost phone tap without double-tapping a toggle (#1232)
- #1223 schema: export the message, run and report page maxima (#1221)
- #1224 tui: operators' every-channel directory view (#1199)
- #1219 cli: cap the remaining list reads, and the thread loop stops at the page limit (#1203)
- #1218 e2e/isolation: restricted rows for twelve pending gates, and the hide-members rows (#1190)
- #1193 server: hide members from each other through see_member_directory (#988)
- #1215 web e2e: showView waits for the phone navigation instead of skipping it (#1214)
- #1211 members: add by id with can_decide, and an add never changes an existing member (#1206)
- #1196 tui: channel directory, community member pane, create channel and voice switch (#995)
- #1160 voice: tests kill and reap every conch-voice they spawn, on every path (#1157)
- #1212 server: the leave-sockets test reads each events socket's first roster first (#1207)
- #1209 schema, server, web: managers on the ?scope=all channel listing (#1202)
- #1208 store: the last-member leave rule counts community members only (#1150)
- #1204 cli: cap the response body of every community list read (#1198)
- #1205 web: the admin console's Members section and the permission-aware person menu (#1020)
- #1201 web: the admin console's Channels section (#1021)
- #1152 voice: make the binary, slow-server and leave-while-joining tests load-robust (#1141)
- #1159 e2e: P3-09 waits one message-socket backoff ceiling for the post-restart message (#1156)
- #1200 web: a rename's channel_exists reads "That name is taken", like create (#1195)
- #1194 web: channel directory, members create channels, community member panel (#991)
- #1186 store: demotion closes the member links a member could not hold (#1165)
- #1192 e2e/isolation: every permission gate has a restricted row, and a personal-preset phase (#1017)
- #1191 cli: community-model parity: conch channels, member links, channel settings, voice, joining (#994)
- #1189 server: notifications test waits for the outside-net queue to drain (#1180)
- #1188 web: channelsrefresh tests wait for the socket and the list they assert (#1185)
- #1179 web: the Roles and permissions section: matrix, presets, and what operators issued (#1019)

</details>

<details><summary>0.1.0-alpha.13 (20 PRs)</summary>

- #1277 changelog: v0.1.0-alpha.13
- #1275 web: a failed start-time camera switch hands over to a newer choice (#1266)
- #1273 docs: camera=(self) on /app/, and the twelfth permission
- #1272 server: a share revocation waits for a retiring room's withdrawal (#1265)
- #1270 docs: the community model as shipped (#997)
- #1271 e2e: the community newcomer scenario end to end (#989)
- #1260 roadmap: webcam video moves into the private alpha (ADR-015)
- #1264 web: the camera, one to one (#1242)
- #1259 cli: camera in conch voice status, and conch voice camera stop (#1243)
- #1258 server: the camera claim, enforced by kind and source, revoked live (#1241)
- #1263 web: Leave channel in the channel ⋯ menu (#1095)
- #1262 docs: the browser starts in the open microphone (#1261)
- #1257 store: use_camera under the voice brake; the PUT enforces it (#1240)
- #1246 server: one publish-permission function for every UpdateParticipant, no behaviour change (#1239)
- #1256 web tests: wait for settled work, not 30 ms, in the section tests (#1255)
- #1248 schema: the camera permission, claim and presence (#1238)
- #1252 e2e: the Docker probe retries a call that times out (#1251)
- #1237 web: admin Members and Channels follow the directory frame (#1226)
- #1253 schema-ts: client-tolerant validators for open vocabularies (#1051)
- #1254 web e2e: the mode toggle helper retries until the mode takes (#1244)

</details>

<details><summary>0.1.0-alpha.14 (12 PRs)</summary>

- #1294 changelog: v0.1.0-alpha.14
- #1292 web: the alpha.7 polish batch, F17-F22, F24, F26 (#1042)
- #1293 loadtest: --camera-publishers N, each claiming its camera before lk publishes (#1284)
- #1291 ci: split web-e2e into two shards, each with its own server (#1290)
- #1289 ci: web-e2e, voice-check and isolation-check run on the self-hosted runners (#1288)
- #1287 ci: the check job runs on the self-hosted 4 TB runner for this repo's branches (#1286)
- #1283 server: a hide signals the readers of the hidden person's messages (#1282)
- #1281 web, tui: who you see, and the authors who left (#996)
- #1278 docs: the camera (#1269, Part A)
- #1276 web: the camera on a phone (#1268)
- #1280 web: the 30 s empty-list test's budget exceeds its own wait (#1279)
- #1274 web: camera groups of up to nine, enlarging, the speaker, cameras beside a share (#1267)

</details>

<details><summary>0.1.0-alpha.15 (43 PRs)</summary>

- #1178 ci: approval-path check guards the chain tests, p2-hardening and the audit export; dogfood runs on a late label (#1170)
- #1360 CHANGELOG: v0.1.0-alpha.15
- #1351 issue #1345: messages refuse the approval payloads the server alone writes
- #1346 issue #1335: AI admins S8 — docs, CHANGELOG and the dogfood loop
- #1359 issue #647: Open button affordance in the strip; guard one-space and phone-row fixes
- #1358 issue #633: base the roster test's join and leave waits on the poller's worst case
- #1356 issue #1217: e2e/isolation rows for the voice gates #1190 left pending
- #1357 issue #1352: a slow mirror slows voice-check, never fails it
- #1353 issue #1352: CI caches the voice build's apt packages and frees disk only when short
- #1354 issue #566: conch-voice refuses echo_cancellation = true until it exists
- #1350 issue #1340: Watch only on a fresh page joins plainly when no voice of the person's is in the room
- #1349 issue #1348: remove the committed isolation binary; ignore e2e build outputs
- #1347 issue #886: the CLI build's VCS diagnostics look where go looks
- #1344 issue #1334: AI admins S7 — Add an AI administrator, the agent card, the approval card
- #1343 issue #1331: AI admins S4 — the admin door's MCP tools
- #1342 issue #1332: AI admins S5 — conch admin ai-admin create|list|show|revoke
- #1341 issue #1333: AI admins S6 — conchd ai-admin create|revoke on the host
- #1339 issue #1330: AI admins S3 — the admin door: catalog, dispatcher, gate, one-shot run
- #1338 issue #1329: AI admins S2 — the store holds an agent to its manifest grant
- #1324 e2e/isolation: the member a guest became raises and decides an approval (#860)
- #1337 AI admins S1: schema for the manifest grant, admin actions and their approvals (#1328)
- #1312 web: watch a share beside native voice as p<id>.view (#936)
- #1317 voice: the companion's share audio on the holder's consent (#898)
- #1327 design + ADR amendment: AI administrators over MCP (#1326)
- #1325 Admin A12: record who issued each credential and hook; list them on demotion and disable (#1024)
- #1323 design: profile avatars (#974)
- #1322 design: read-only Preview as member (#1007)
- #1320 design: guest names, a clean chosen name with a Guest badge (#1003)
- #1321 voice-check: judge audible frames over the whole hold (#1316)
- #1299 go: keep go ./... out of desktop/ as well as web/ (#954)
- #1314 conch-voice: dmn-audio is Linux-only; a neutral device table (#930)
- #1313 conch-voice: exclude the session's companion; --share-audio-owner (#934)
- #1311 web: the secret-panel test drives its timer and has its own bound (#1301)
- #1310 desktop: a pasted credential stays in main behind a one-time handle (#1123)
- #1308 ADR-014 decision 6: no link carries a may_invite grant (#1306)
- #1307 web e2e: 34-message-editing waits on state, not timing (#1048)
- #1304 Retire POST /v1/invites for everyone (#1134)
- #1319 CI: everything on GitHub-hosted runners; check split into parallel jobs (#1318)
- #1302 TUI: show the caller's permissions and follow their changes (#1148)
- #1303 CI: docs-only pull requests run a light check (#1300)
- #1297 Approvals and runs in an archived channel: refuse new, let in-flight finish (#1296)
- #1298 docs: admin-roles says may_invite is the person's own brake (#1182)
- #1295 Channels: archive and restore without deleting history (#844)

</details>

<details><summary>0.1.0-alpha.16 (32 PRs)</summary>

- #1432 CHANGELOG: v0.1.0-alpha.16 (holds for Nick's release decision)
- #1424 issue #998: store migration 46 drops the unread open_channels table (I13; holds for Nick's release-condition confirmation)
- #1425 issue #852: moderation M4, message removal (approval-path; stacked on #1419)
- #1419 issue #851: moderation M3, channel entry bans and admission enforcement
- #1431 issue #1421: desktop voice options open on the saved microphone and speakers
- #1430 issue #970: push-to-talk opens on key-down and reports in parallel (both clients)
- #1429 issue #1427: channelsrefresh empty-list test waits for the list's own timer (flake)
- #1428 issue #1046: desktop re-sends a request 401'd for a just-rotated credential
- #1426 issue #1422: desktop password change, set password, guest conversion and re-authentication through main
- #1423 issue #1382: talk-key setup in the desktop voice options (D3-11)
- #1420 issue #1379: microphone, speakers and talk mode in the desktop voice options (D3-8)
- #1408 issue #1376: the desktop voice strip from native state (D3-5)
- #1418 issue #1381: per-device talk-key setup in main (D3-10)
- #1410 issue #1378: one-shot helper runs for devices and config set, and their IPC (D3-7)
- #1414 issue #850: moderation M2, remove a participant from voice only
- #1407 issue #1374: the voice IPC family and preload bridge, with no press or release (D3-3)
- #1415 issue #1412: the channel directory Leave test waits for focus to return
- #1411 issue #1383: the live same-machine desktop voice test (D3-12)
- #1406 issue #1375: the native-voice platform seam, store and state words (D3-4)
- #1405 issue #1373: main's voice service on the #948 supervisor (D3-2)
- #1404 issue #1380: config set and unset for the key device and bindings (D3-9)
- #1402 issue #1390: Android spike gate, lifecycle and camera logic with unit tests (M4-B)
- #1399 issue #1377: bundle the matching conch-voice in the built app (D3-6)
- #1398 issue #849: moderation M1, the signed schema and storage contracts
- #1397 issue #1372: the desktop push-to-talk ceiling in managed mode (D3-1)
- #1371 issue #1366: dev-stack LAN mode for the phone experiments
- #1370 issue #1367: camera rows M-C1 to M-C10 in the mobile capability record
- #1369 ADR-016: the Unchanged line agrees with the camera amendment
- #1365 issue #1106: ADR-016 accepted, with the phone camera in scope
- #832 Require shared issue claims before agent dispatch
- #1363 issue #1361: cameras on by default on new servers; both presets include them
- #1362 issue #545: README front page with the Conch logo; the guide moves to docs/manual.md

</details>

<details><summary>After alpha.16, on `main` (unreleased) (17 PRs)</summary>

- #1460 issue #1456: the CLI and TUI treat voice_removed as out of voice
- #1458 issue #1409: a voice-only removal closes presence with voice_removed
- #1455 issue #1453: the gate hard-caps a push-to-talk press; gate.rs counts one press
- #1452 issue #1450: a member's open menu and question survive a presence change
- #1451 issue #1449: 51-moderation waits for the target to settle online before its menu
- #1454 issue #1172: ADR-001 records two deliberate parity exceptions
- #1448 issue #965: the onset spec holds open mic to an 80 ms p90 bound
- #1443 issue #1440: moderation in the browser (M6b)
- #1444 issue #1439: moderation in the conch CLI and TUI (M6a)
- #1447 issue #646: the report form warns that console output can hold a voice token
- #1446 issue #1445: reports.rs timing tests keep a margin over their frames
- #1442 issue #1441: phone voice strip waits for its state button and bounds actions
- #1438 issue #853: moderation M5, the isolation and visibility matrix on a real server
- #1436 issue #935: e2e/voice proves one playback owner per audio source
- #1433 issue #766: reactions in the browser
- #1437 issue #886: TestGoVCSRoot's fixture cannot see shared temp ancestors
- #1434 issue #767: reactions in the conch CLI and TUI

</details>
