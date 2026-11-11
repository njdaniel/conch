# Testing: the end-to-end suites

Which suite runs where, what it proves and what it does not, and the rules for
the tags, the pull request budget, flakes and artifacts (issue #1613, epic
#1607). The unit tests (`go test`, `cargo test`, Vitest) are not here: `make
check` runs them. Nothing below runs against production or the alpha by
default; every suite starts its own throwaway `conchd` (except `desktop-smoke`,
which serves the packaged files and starts none).

## The suites

| Suite (CI job) | Lane | What it proves | What it does not | Local command | Artifacts |
| --- | --- | --- | --- | --- | --- |
| `web/e2e` in Chromium (`web-e2e`, three shards) | every pull request and push to `main`, with `--grep-invert @heavy` | the browser client against a fixture community: sign-in, messages, admin, browser voice, camera and screen share on Chromium's fake devices, with the pinned LiveKit in Docker | Firefox and WebKit (they run in the heavy lane, below); real microphones, cameras and screens; a LAN or a second machine; real AI agents (scripted ones only) | `make web-e2e` (once: `cd web && npx playwright install chromium`) | traces of failed tests, `web-e2e-test-results-N`, 7 days |
| `web/e2e` heavy lane (`e2e-heavy.yml`) | daily on `main`, and on demand for any branch | the tests tagged `@heavy`: those over the pull request budget, and deliberate fault or blackhole tests | the same limits as the row above | `make web-e2e-heavy` | traces of failed tests, `e2e-heavy-test-results`, 7 days |
| `voice-check` | every pull request | the voice control plane: a real `conchd`, the real `conch` CLI, three `conch-voice` clients and a real LiveKit; the core unaffected with LiveKit down | a human's ears; real audio devices; PipeWire | `go run ./e2e/voice` (Docker) | the log |
| `desktop-voice-check` | every pull request | the Linux desktop shell's voice and screen companion against the web client and LiveKit | a real display or sound card; a second machine (#1584) | `make desktop-voice-e2e` (Docker) | the log |
| `isolation-check` | every pull request | access between groups: what a principal outside a group can and cannot reach, with LiveKit rooms | the browser; more than the scripted principals | `go run ./e2e/isolation` (Docker) | the log |
| `community-check` | every pull request | the community model's newcomer scenario: invite, sign-in, default channel, a private channel (separate principals) | the browser's rendering of it | `go run ./e2e/community` (Docker) | the log |
| `p3-local-check` | every pull request | the P3 fixture community, on loopback and then non-loopback | anything beyond one machine's network stack | `go run ./e2e/p3-local`, then `go run ./e2e/p3-local --non-loopback` | the log |
| `p2-hardening-check` | every pull request | routes, audit rows, credentials and their hardening | the browser | `go run ./e2e/p2-hardening` | the log |
| `nets-check` | every pull request | nets and whispers over the public API | voice media | `go run ./e2e/nets` | the log |
| `dogfood-check` | pull requests labelled `approval-path`, and every push to `main` | the approval chain: an agent over MCP requests, ntfy fires, a human resolves with `conch approve`, the audit chain is asserted | a real AI agent (scripted); a phone's notification | `go run ./e2e/dogfood` | the log |
| `conch-bot-check` | every pull request | the reference bot as a client of the public API | other bots | `go run ./e2e/conch-bot` | the log |
| `desktop-smoke` | every pull request | the packaged desktop app starts under a virtual display and serves the web client | real windowing, the installed `.deb`, other distributions (#1603) | `xvfb-run -a make desktop-smoke` (after `make desktop-build`) | the log |

Docs-only pull requests run none of these (`scripts/ci-docs-only.sh`), with
one exception: a docs-only pull request labelled `approval-path` still runs
`dogfood-check`, which has no `changes` gate. Every suite here also runs on
every push to `main`.

Two limits hold for every row. Principals in these suites are separate
principals or sessions of one principal as each suite says, but all run on one
machine; none crosses a LAN or a VM. Audio and video are fake devices (a tone
file, Chromium's test pattern), so a green run says the media path connects and
flows, not that anyone can hear or see well.

## By-hand coverage that automation does not replace

- #474: the alpha's browser flows in real Chrome and Firefox.
- #1584: desktop share and watch on Pop!_OS with a second machine.
- #1603: Linux desktop release acceptance: mixed clients, two servers, gaming
  push-to-talk, share and watch, every failure path.
- #448 and #1513: screen share to many viewers, and multi-share measurements,
  on the droplet.

## Who may do what: where it is tested

A role by action map of the suites above, in three tables: content,
administration and media (issue #1609, epic #1607). It says which test covers
each cell, so that the next gap is visible. It adds no test.
Each citation names a test that exists at the base of the pull request that
added this section and asserts what the cell says it does.

The roles are the ones the server has (`docs/design/admin-roles.md`,
`internal/server/routes.go`): owner; operator; channel manager (per channel);
member; agent. Three more rows are standings, not roles: a member with a cell
or a brake off, a member removed or banned (from a channel, or disabled), and
a member of another group. The last row is the guest. The permissions document
has twelve permissions, each with a member cell and an operator cell, and a
person's brake (`voice_allowed`, `may_invite`) can take one away. The actions
are the columns. Where a role has no standing in an action, the cell says
`n/a`.

How to read a cell:

- `W` is the browser (`web/e2e`). `G` is Go: `internal/server` tests (`S`) and
  the Go end-to-end suites (`I` is `e2e/isolation`, `Co` is `e2e/community`,
  `Vo` is `e2e/voice`, `Do` is `e2e/dogfood`).
- `+` means the test asserts the action is allowed. `x` means it asserts the
  action is refused. A key with no sign asserts both.
- `none` means no test covers that half of the cell. A cell that is `none` in
  both halves is empty.
- Keys are defined in the lists after the tables.

**Content**

| Role | Read, post | Private channel: see or change | Member links | Member directory |
| --- | --- | --- | --- | --- |
| Owner, and operator with the defaults | W: +W04b; G: +Co, xS17 (an operator outside the channel is refused as for a ghost) | W: +W40, +W42; G: +S11a, xS17 | W: +W11b; G: +S10, +Co | W: none; G: +S15 |
| Operator with a cell off | W: none; G: xS11a | W: xW68a; G: xS11a, xIr | W: xW68b; G: xS11b, xS10, xIr | n/a (fixed on) |
| Channel manager | as member | W: xW24b; G: xS5 | as member | W: none; G: xS8b |
| Member | W: +W02, +W04c, xW04a; G: +Co, +S17 | W: xW04a, +W04b; G: xS17 | W: +W11a; G: +S10 | W: +W47; G: +S15 |
| Member with a cell or brake off | n/a (fixed right) | n/a (fixed right) | W: xW11c; G: xS10, xIp | W: xW47; G: xS15, xIh |
| Removed or banned member | W: xW51a, xW04b; G: xS16, xImod | W: xW51a, xW14b; G: xS16, xImod | n/a | n/a |
| Member of another group | W: xW14a; G: xIm | W: xW14a, xW14c; G: xIm, xImed | W: none; G: xIm | n/a (community-wide by default) |
| Agent | W: none; G: +Do, xS17, xIa | W: none; G: xIa, xS17 | W: none; G: xS10, xIa | W: none; G: xS1 |
| Guest (refused, #1005) | n/a (see below) | n/a | W: xW22; G: xS18, xCo, xS10, xIg | W: none; G: xS19 |

**Administration**

| Role | Channel create, rename, archive | Channel members add, remove | Moderation | Edit permissions document | Act as |
| --- | --- | --- | --- | --- | --- |
| Owner, and operator with the defaults | W: +W20, +W21a, +W48a; G: +S5, +S6, +S7 | W: +W24, +W42, +W04b; G: +S8c | W: +W12b; G: +S9, +Vo | W: +W39a, +W39b; G: +S4 | W: +W39b; G: +S3 |
| Operator with a cell off | W: xW68a; G: xS5, xS6, xIr | W: xW68a; G: xS11c, xIr | W: xW68a; G: xS14, xIr | W: none; G: xS4 | W: xW43b, xW68b; G: xS3, xS11d, xIr |
| Channel manager | W: +W24, xW24b; G: S7, xS5, xS6 | W: +W24, +W51a; G: S8a | W: +W51a, +W51b, +W51c; G: +S9, +Imod | as member | as member |
| Member | W: +W20, xW21b, xW48b; G: +S5, xS5, xS6, xS7 | W: xW24; G: xS8c | W: xW51a, xW51b, xW51c; G: xS9, xImod | W: xW38b; G: xS1, xS2 | W: none; G: xS2, xS3 |
| Member with a cell or brake off | W: xW39c; G: xS5, xIp | n/a | n/a | n/a | n/a |
| Removed or banned member | n/a | W: xW51a; G: xS16, xImod | n/a | n/a | n/a |
| Member of another group | W: none; G: xIm, xIc | W: none; G: xIm, xIc | W: none; G: xIm | W: none; G: xIm | W: none; G: xIm |
| Agent | W: none; G: xS1, xS6, xS7 | W: none; G: xS1, xS9 | W: none; G: xS9, xIa | W: none; G: xS1, xAi | W: none; G: xS3, xAi |
| Guest (refused, #1005) | W: none; G: xS5, xS6, xS7, xIg | n/a | n/a | W: none; G: xIg | W: none; G: xIg |

**Media**

| Role | Voice join | Share screen | Camera |
| --- | --- | --- | --- |
| Owner, and operator with the defaults | W: none; G: +S12 | W: none; G: +S12 | W: none; G: +S14, +Vo |
| Operator with a cell off | W: none; G: xS12 | W: none; G: xS12 | W: none; G: xS14 |
| Channel manager | as member | W: none; G: +S13b | W: none; G: xS14 |
| Member | W: +W10a; G: +Co, +S12 | W: +W12a; G: +S12 | W: +W44; G: +Vo |
| Member with a cell or brake off | W: xW11d; G: Co, xS12, xIv | W: none; G: xS12, xS13a, xIv | W: xW45; G: xS14, xVo |
| Removed or banned member | W: xW10b; G: xVo, xImod, xIrev | W: xW12c; G: xIrev | n/a (ends with voice) |
| Member of another group | W: xW14c; G: xIm, xImed | W: none; G: xIm, xImed | W: none; G: xIm |
| Agent | W: none; G: xS1, xIa | W: none; G: xS1 | W: none; G: xS14, xS1 |
| Guest (refused, #1005) | W: none; G: xS12, xIg | W: none; G: xIg | W: none; G: xS14, xIg |

### Notes on the table

- **Guests.** Guests were removed for now (#1005). No route starts a visit, so
  no live-guest flow is tested and none is claimed. The guest row is the
  refusal of every guest route: `22-guest` (`W22`), `e2e/community` step 6
  (`Co`), and `internal/server` `S18`. The `e2e/isolation` guest rows (`Ig`) run
  against visits that the harness seeds beside the server, so they count only
  as refusal rows. The Go guest rows in `S5` to `S7`, `S10`, `S12` and `S14`
  also seed a guest through the store. The `guest` token in `web/e2e` and
  `e2e/p3-local` is a plain member (`tui-b`), not a guest visit.
- **An operator outside a channel** has no standing in its content. It gets the
  404 of a channel that does not exist (`S17`, `S9`). An operator holding
  `manage_channels` still administers the channel. Without it, the operator
  reads as a member of channels it is not in (`S11a`).
- **"Member with a cell or brake off"** covers `create_public_channels`,
  `create_private_channels`, `make_member_links`, `see_member_directory`,
  `use_voice`, `share_screen` and `use_camera` for members, and the two brakes.
  The other permissions are fixed off for members.
- **"Private channel: see or change"** mixes two things. For an operator,
  owner, member and removed member it is seeing the channel. For a channel
  manager it is the refusal to change its visibility (`S5`).
- **Moderation** is: remove a message, remove or ban from a channel, remove
  from voice only, and stop another person's share or camera. The target of
  moderation is the removed or banned member.
- **`W68b`'s Act as cell** is not `act_as_members` alone. `act_as_members`
  requires `manage_members` in the same column, so the test turns both off in
  one patch; the missing act-as items are `W43b`'s case reached the other way.
- **A cell that says `as member`** has no rule of its own. A channel manager is
  a member everywhere but in its own channel.

### Where the map is empty or thin

The tables have 108 cells. 19 are `n/a` and 5 say `as member`. Of the other 84:

- 43 have a browser test and a Go test.
- 41 have a Go test and no browser test (`W: none`).
- None is empty in both halves.
- None has a browser test and no Go test.

The browser gaps cluster in four places. Each gets a follow-up issue:

- a member with `create_public_channels` or `create_private_channels` off, in
  the browser (the New channel form is not offered);
- a member with `share_screen` off, in the browser (#1611);
- an operator with `act_as_members` off alone seeing only the controls it
  holds (`W43b` has it; `manage_channels` and `manage_members` have `W68a` and
  `W68b`);
- a channel manager not being offered visibility or Archive.

Voice, share and camera for the owner and operator have a Go test and no
browser test of their own. The browser voice specs use plain members.

### Browser keys

| Key | Spec and test | What it asserts |
| --- | --- | --- |
| `W02` | `02-messages`: P3-02 a message sent in the browser appears once in both contexts and once in the history | a member posts, another member reads it once |
| `W04a` | `04-private`: P3-05 browser-test cannot open test-private and nothing about a run there shows in its members panel | a member has no button for a private channel; the API refuses its messages and the run (404) |
| `W04b` | `04-private`: C1027 a channel the owner adds member-1 to shows in their list with its unread count, and goes when they are removed, with no reload | the owner adds and removes a member; the member's list changes live; a removed member sees "You no longer have access" |
| `W04c` | `04-private`: P3-05 the browser composes to a net and to a whisper, and only the audience reads it | a member posts to a net and a whisper; only the audience reads it |
| `W10a` | `10-voice`: a member joins, is heard only while transmitting, shows as talking, and is audited as a reported pair | two members join voice and talk |
| `W10b` | `10-voice`: removing the member disconnects the browser and the page says why | a removed member's voice stops with "not a member" |
| `W11a` | `11-invites`: A8 a joiner makes a link that another browser redeems, sees who joined, then closes one | a member makes, shares and closes a member link |
| `W11b` | `11-invites`: A8 an operator makes a link with its defaults and one that never expires, and closes them | an operator makes links, including one that never expires |
| `W11c` | `11-invites`: A8 a member whose right to make links is off is told why and offered no form | `may_invite` off: no form, and `POST /v2/invites` is 403 |
| `W11d` | `11-invites`: A8 a joiner without voice is told so, and the page asks for no voice route | `voice_allowed` off: the page says so and asks for no voice route |
| `W12a` | `12-screen-share`: a member shares a screen with sound; another watches it without a microphone; a third is told who is sharing; stopping closes the view | a member shares; others watch |
| `W12b` | `12-screen-share`: the browser's own stop control, leaving voice and an operator each end the share for the viewer, and the sharer is told why | an operator stops a member's share; the sharer is told |
| `W12c` | `12-screen-share`: a share carries on when another member is disabled, and ends for good when the sharer is | a disabled sharer's share ends |
| `W14a` | `14-isolation`: A14 a member of one group sees nothing of another, after a full load and after a reconnect | no channel, message or private marker of the other group, at two sizes |
| `W14b` | `14-isolation`: A14 a member removed from the channel, or hidden, leaves the open page of another member without a reload | a removed member leaves the member panel at once |
| `W14c` | `14-isolation`: A14 a member of one group never sees who is in another group's voice room, after a full load and after a reconnect | voice presence is not shared across groups (needs LiveKit) |
| `W20` | `20-create-channel`: C695 an operator makes a channel...; C991 a member is offered the form with a visibility choice and makes a channel, and the operator-only route still refuses a member | an operator and a member each make a channel; the member is 403 on `/v0/channels` |
| `W21a` | `21-channel-rename`: C728 an operator renames the open channel; a second page moves too... | an operator renames |
| `W21b` | `21-channel-rename`: C728 a member is offered no control, and the server refuses the member anyway | no Rename control; the API is 403 |
| `W22` | `22-guest`: the three G6 tests | the bare address and an invite offer no guest entry; every guest route and setting is refused (403 `guests_disabled`, 409 `invite_kind_retired`, 409 `join_policy_conflict`) |
| `W24` | `24-channel-managers`: C793 an operator delegates a channel; its manager adds, removes and renames while members see only their standing | a manager adds, removes and renames; a plain member has no form and no row menu |
| `W24b` | `24-channel-managers`: C1670 a manager who is not an operator is not offered Make private, Make default or Archive, and the server refuses each | the manager's menu is exactly Rename channel…, Set topic, Leave channel, Favorite; `PUT /v1/channels/{id}/settings` for private, default and archived, sent from its page, is 403 `forbidden` |
| `W38b` | `38-admin-console`: A1018 a member sees one line of what they may do and no console | no console, no Joining, no Server (no console is offered; it does not name the permissions document) |
| `W39a` | `39-permissions`: A1019 the owner applies Personal, then Group, from the preset cards | the owner writes the document; the server has the preset |
| `W39b` | `39-permissions`: A1019 the owner turns Act as members off for operators, sees what operators issued, and revokes one | an operator with the cell on issues a credential to another; the owner turns it off and revokes it |
| `W39c` | `39-permissions`: A1668 a member with channel creation off is offered no way to make that channel, and the server refuses a direct call | `create_public_channels` off: no choice of visibility; both off: no New channel anywhere; `POST /v1/channels` from the page is 403 `permission_denied`; the cells are restored and read back |
| `W40` | `40-channel-privacy`: P1183 an operator makes a channel private and public from its menu, and a member sees the lock live | an operator changes visibility; a member sees the lock |
| `W42` | `42-admin-channels`: A1021 on a personal server the owner makes a private channel, adds a person to it, makes them a manager and removes them | owner creates a private channel, adds a person, makes a manager |
| `W43a` | `43-admin-members`: A1020 the owner changes a role from a person's menu, and the server has it | only the owner changes a role |
| `W43b` | `43-admin-members`: A1020 an operator without act_as_members is offered no sign-in link, credential or hook | no act-as item in the menu; an owner is not theirs to act on |
| `W44` | `44-camera`: joining, unmuting and sharing never ask for the camera; the button starts it... | a member starts and stops a camera (claims it by `POST`) |
| `W45` | `45-camera-groups`: the owner turns use_camera off for members: bob's tile on carol's page goes to Camera off, and bob's page ends its track and says why | a member's camera ends within the bound when the cell goes off |
| `W47` | `47-hide-members`: H996 turning the member directory off takes bob from alice's panel and her mention list...; the other test in the file | directory on: bob listed; off: bob gone, the owner and herself remain |
| `W48a` | `48-channel-archive`: A844 an owner archives a channel and restores it; member-1 sees it move live, read-only, and back | owner archives and restores |
| `W48b` | `48-channel-archive`: A844 a member without manage_channels is offered neither Archive nor Restore | no menu item; the API is 403 |
| `W51a` | `51-moderation`: manager confirms membership removal, ban and own unban; a plain member has no controls | manager removes and bans; target loses the channel (404); a plain member has no controls and 403 on voice removal |
| `W51b` | `51-moderation`: message removal replaces live copies by id, survives reconnect and never notifies an inaccessible whisper | manager removes (204); a plain reader has no menu item; a plain channel member who is not the author is 403 |
| `W51c` | `51-moderation`: LiveKit: a manager confirms a voice-only removal with retry, a member cannot, and the target keeps chat access | manager removes from voice; a member is 403 |
| `W68a` | `68-restricted-operator`: R1669 an operator with manage_channels off is offered no rename, archive, visibility change or private-channel view, and the server refuses each | the channel menu is exactly Invite people, Leave channel, Favorite; the person menu in Settings is nine items, none of them Add to a channel…; no Channels section, no Every channel list; from the page, Rename channel, Ban from channel and Remove from channel are 403 `forbidden`, Make private, Make default, Archive and `GET /v1/directory/channels?scope=all` are 403 `permission_denied` naming `manage_channels`, and a private channel it is not in is 404 `channel_not_found` for adding a member and listing members; the cells are restored and read back |
| `W68b` | `68-restricted-operator`: R1669 an operator with manage_members off is offered no removal, disabling or role change, and the server refuses each | `act_as_members` goes with it (it needs `manage_members`); the person menu in Settings is exactly Add to a channel…, so no voice or member-link switch, sign-out, Disable sign-in…, Remove from server… or act-as item; the channel menu and the channel member panel's Remove from voice, Ban from channel and Remove from channel stay (they are `manage_channels`'s); from the page, remove from server, disable, enable, voice and member-link switches, credential list, making a person and the joining settings are 403 `permission_denied` naming `manage_members`, and Change role is 403 `forbidden`; the cells are restored and read back |

### Go keys

| Key | Where | What it asserts |
| --- | --- | --- |
| `S1` | `internal/server/auth_test.go`: `TestRoleMatrix` | every route as operator, member, agent and nobody: the status per route class |
| `S2` | `auth_test.go`: `TestOperatorOnlyRoutesDeny` | a member is 403 on each operator and permission route; audited `access_denied`; nothing minted |
| `S3` | `permissions_test.go`: `TestPermissionGuard` | owner, operator, member, agent, nobody and a restricted operator, on one route per permission class |
| `S4` | `permissions_test.go`: `TestPermissionsPut` | who may write the document: the owner; an operator with and without `change_server_settings`; the operator column is refused |
| `S5` | `channels_v1_test.go`: `TestCreateChannelV1Refusals`, `TestCreateChannelV1Creator`, `TestChannelSettingsRefusals` | create and settings by every caller class; a member creates and manages; create refused naming a withheld cell |
| `S6` | `channel_archive_test.go`: `TestArchivePermissionMatrix` | archive and restore by every caller class; refusals are byte-identical |
| `S7` | `channels_test.go`: `TestRenameChannelManagerStandings` | rename: owner, operator and manager allowed; manager of another channel, member, guest and agent refused |
| `S8a` | `administration_test.go`: `TestManagerAddsAndRemoves`, `TestManagerAddsAnyMember` | a manager adds and removes; it cannot remove an operator |
| `S8b` | `administration_test.go`: `TestManagerAddWithDirectoryOff` | with the directory off a manager adds only people it shares a channel with |
| `S8c` | `members_test.go`: `TestMembershipEndpoints` | a member is 403; an operator adds and removes; a non-member is 404 |
| `S9` | `bans_test.go`: `TestBanRouteActorTargetMatrix`; `voice_moderation_test.go`: `TestVoiceKickActorMatrix`; `message_removal_test.go`: `TestMessageRemovalChecks` | ban, voice removal and message removal by actor: allowed for owner, operator and manager; refused for member, agent, outsider |
| `S10` | `invites_v2_test.go`: `TestCreateMemberLinkRoute`; `permissions_suspension_test.go`: `TestCreateInviteByPermissionAndBrake`; `permissions_test.go`: `TestMemberLinkRefusalsNameThePermission` | member links by caller, by cell and by brake; the refusal names the permission |
| `S11a` | `permissions_restricted_test.go`: `TestRestrictedOperatorReadsAsAMember` | without `manage_channels`, a channel it is not in is the uniform 404 |
| `S11b` | `permissions_restricted_test.go`: `TestRestrictedOperatorInvites` | landing channel and never-expires refused without the cells |
| `S11c` | `permissions_restricted_test.go`: `TestRestrictedOperatorManagerHasManagerStanding` | `can_decide` and `manages` refused without `manage_channels` |
| `S11d` | `permissions_restricted_test.go`: `TestRestrictedOperatorSignInLinks` | sign-in links need `act_as_members` when made and when used |
| `S12` | `voice_permissions_test.go`: `TestVoiceRoutesRefuseByPermission`, `TestVoiceGuestHoldsNoVoice` | voice and share routes by cell and brake; the owner is never refused at the gate (the test asserts not 403, not a successful join); a guest has no voice |
| `S13a` | `voice_permissions_test.go`: `TestPutShareScreenOffEndsAShareAudited` | turning the cell off ends a live share before the PUT answers |
| `S13b` | `administration_test.go`: `TestManagerStopsAShare` | a manager stops another's share; a plain member is 403 |
| `S14` | `voice_camera_test.go`: `TestVoiceCameraClaimRefusals`, `TestVoiceCameraOperatorStop`; `voice_camera_pass_test.go`: `TestVoiceCameraGuest`; `permissions_test.go`: `TestPermissionsPutUseCamera` | camera claim refused for agent, no voice, cell off; only `manage_channels` stops another's camera; a guest is refused |
| `S15` | `hide_members_test.go`: `TestHideMembersDirectory` | with the cell off a member lists only those it may know; operators see everyone |
| `S16` | `bans_test.go`: `TestBannedResidualMembershipGrantsNothing`, `TestBanDropsLiveSockets`; `members_test.go`: `TestWebSocketRemovedMemberIsDropped` | a banned or removed member is refused as a stranger; sockets close |
| `S17` | `members_test.go`: `TestChannelContentLeak` | every content route: a member allowed; a non-member human, agent and operator get the bytes of a missing channel |
| `S19` | `internal/server/directory_test.go`: `TestDirectoryViewers` | an active guest on `GET /v1/directory/members` is 403 `guest_not_allowed`; an ended guest with a replayed token is 401 |
| `S18` | `guests_removed_test.go`: `TestGuestsRemovedNoRouteStartsAVisit`, `TestGuestsRemovedNoGuestEntryPoint` | every guest entry route is 403 `guests_disabled` or refused; no visit exists afterwards |
| `Im` | `e2e/isolation`: the matrix (`matrix.go`, `routes.go`) | each route of `routes.go` has a row (a coverage check fails the run otherwise); the other group's member gets the stated refusal, no marker, and the answer for a missing object |
| `Ia` | `e2e/isolation`: `agents.go` | the other group's agent, over REST and MCP, is refused as for a ghost |
| `Ai` | `e2e/isolation`: `aiadmin.go` | an agent granted every agent-grantable permission is refused on every administrative route |
| `Imod` | `e2e/isolation`: `moderation.go` and the files it names | a removed or banned member is refused every route and socket, timed; message removal by who may do it, and what each socket is sent |
| `Irev` | `e2e/isolation`: `revoke.go` | a disabled person's sockets, LiveKit participant and share end within the bound |
| `Ip` | `e2e/isolation`: `preset.go` | under the personal preset a member's link and channel creation are refused, naming the permission |
| `Ih` | `e2e/isolation`: `hiderows.go` | with the directory cell off, a member sees only who it may know |
| `Ir` | `e2e/isolation`: `restricted.go`, `gaterows.go` | a restricted operator is refused every permission-class route, naming the permission; every gate has a row |
| `Iv` | `e2e/isolation`: `voicegates.go` (needs LiveKit) | voice switch off removes the participant; a withheld share ends on the next pass |
| `Imed` | `e2e/isolation`: `media.go` | the other group cannot get a session for the room or see its tracks |
| `Ic` | `e2e/isolation`: `channelsrows.go` | rename, archive and membership signals reach no one outside the channel |
| `Ig` | `e2e/isolation`: `guestrows.go` | refusal rows for a guest: invites, channel creation, voice, camera, screen share, the permissions document, credentials (seeded visits) |
| `Co` | `e2e/community`: `scenario.go` steps 1 to 6 | owner's link, redemption, public channel, voice off removes within the bound, closed link, no guest |
| `Vo` | `e2e/voice`: `kicked.go`, `banned.go`, `camera.go` | an operator removes from voice and bans; camera claims and stops; a member's claim with the cell off is 403 |
| `Do` | `e2e/dogfood`: `main.go` | an agent over MCP is refused `post_message` with no manifest, then posts once provisioned |

## Tags

A test carries tags in Playwright's `tag` option:
`test('name', { tag: '@heavy' }, async ({ page }) => { ... })`, or in the title
(`test('@smoke name', ...)`, as `63-multi-user-smoke` does); `--grep` matches both. Four exist
(`web/playwright.config.ts` repeats this):

- `@heavy` runs only in the heavy lane. Use it for a test over the pull request
  budget below, or a deliberate fault or blackhole test. The `web-e2e` job runs
  with `--grep-invert @heavy`, so a tagged test is never in a pull request
  shard; `e2e-heavy.yml` runs `--grep @heavy`.
- `@smoke` marks the fast subset for a quick local run: `make web-e2e
  E2E_ARGS="--grep @smoke"`.
- `@cross` marks the subset Firefox and WebKit run (#1658), when
  `CONCH_E2E_BROWSERS` names them. Chromium runs it with the rest, in the pull
  request lane too. Tag a test `@cross` only if it passes on the engines it runs
  on; one that needs a fake microphone also carries `@voice`, which the `webkit`
  project leaves out. An engine that cannot do a thing is not given a skip: the
  test is not tagged, and the table below says why.
- `@voice` marks a test that needs a fake microphone. Firefox gets one from its
  prefs; the `webkit` project leaves `@voice` tests out (see the table below).

Untagged tests stay in the pull request lane. `--grep` is an unanchored regular
expression over titles and tags, so a tag that merely contains `@heavy` (say
`@heavy-gpu`) is caught too.

## Lanes and cost

- **Pull request lane:** `web-e2e`, three shards on `ubuntu-latest`, bounded at
  30 minutes. Budget: a new LiveKit spec file adds at most about 2 minutes to
  its shard, and a shard stays under about 20 minutes. A spec over that is
  `@heavy`.
- **Heavy lane:** `.github/workflows/e2e-heavy.yml` on `ubuntu-latest`, daily
  (GitHub runs schedules on `main` only) and on `workflow_dispatch`. To run it
  on a branch: Actions, e2e-heavy, Run workflow, pick the branch (or `gh
  workflow run e2e-heavy.yml --ref <branch>`). The branch must contain
  `e2e-heavy.yml`, so rebase on `main` first. It has its own concurrency group,
  a 40-minute bound and `contents: read` permission only.
  A run with no `@heavy` test passes (`--pass-with-no-tests`). It does not use the
  self-hosted `HEAVY_RUNNER` path. The repository is public, so hosted minutes
  cost nothing; wall time and queue contention are the cost.
- **Other engines:** the `e2e-cross` job of `e2e-heavy.yml`, one job each for
  Firefox and WebKit (`fail-fast` off, the same 40-minute bound), runs
  `--project=<engine> --grep @cross` with `CONCH_E2E_BROWSERS=<engine>`. The
  Firefox leg first installs PulseAudio and starts a null sink as the default
  output (`pactl load-module module-null-sink`; the step fails if it does not
  start), because a hosted runner has no audio device and Firefox's Web Audio
  analysis of a received track reads silence without one (#1707). It never runs
  in the pull request lane.
- Windows and macOS runners are not in either lane (Q8 of #1607: `conchd` has no
  Windows build in CI, and the pinned LiveKit is a Linux container).

### Firefox and WebKit (#1658)

Without `CONCH_E2E_BROWSERS`, `playwright.config.ts` defines Chromium only and
`make web-e2e` runs the same 369 tests as before. To run the other engines
locally:

```
cd web && npx playwright install firefox webkit     # once; WebKit also needs system libraries
CONCH_E2E_BROWSERS=firefox make web-e2e E2E_ARGS="--project=firefox --grep @cross"
CONCH_E2E_BROWSERS=webkit  make web-e2e E2E_ARGS="--project=webkit --grep @cross"
```

Capabilities, on Playwright 1.61.0's own builds (Firefox 151.0) on Linux,
headless. WebKit here is Playwright's Linux build of the engine, not Safari;
nothing below says anything about Safari, macOS or iOS.

| Capability | Firefox | WebKit |
| --- | --- | --- |
| The client loads, signs in and chats | Yes. `01-sign-in` and `02-messages` (all but the F15 tab-stop count) and the 63 messaging smoke pass | Yes, from CI (e2e-heavy run 42819479322, `e2e-cross (webkit)`): 11 of 12 passed, the 12th (F7) after a one-line filter, below |
| Fake `getUserMedia` audio | Yes, with `firefoxUserPrefs` `media.navigator.streams.fake` and `media.navigator.permission.disabled`: the track is "Default Audio Device", no prompt. Fake video the same way (a camera test passes) | Not established. No launch flag or pref is known; `@voice` tests are left out |
| Fake `getDisplayMedia`, headless | No. The call is never answered (waited 10 s after a click; the same with the `media.getdisplaymedia.*` prefs). No `@cross` test shares a screen. `12-screen-share` also expects tab sound, which the client says Firefox does not give ("No sound: this browser shares the picture only") | Not established |
| WebRTC to the pinned LiveKit on 127.0.0.1 | Yes, with `media.peerconnection.ice.obfuscate_host_addresses` false (the counterpart of Chromium's `WebRtcHideLocalIpsWithMdns`): `63` voice smoke passes (#1703: its one failure in 20 was a teardown wait, not a join: Playwright's plain `page.close()` sends no leave to LiveKit on Firefox, so LiveKit listed the person for about 20 s; `leave` in `people.ts` now closes with `runBeforeUnload`) | Not established |
| A synthetic Web Audio microphone (#1610) heard by the other pages | Yes, given an audio output device. `65-who-hears-whom` passed 20 of 20 on Firefox idle and 20 of 20 under load locally (a PipeWire host), but failed in both first heavy runs (42819479322, 42842835221): a listener's analyser hearing no tone the others hear, `inbound-rtp` bytes (as `63` checks) arriving all the while. Reproduced locally (#1707): with no audio server reachable Firefox fails 65 as CI did, and with a null sink as the default sink it passes; the Firefox leg of the heavy lane now starts a PulseAudio null sink as the default before the tests. Without one, expect -120 dB at every tone | Not established |

How it was established: `npx playwright install firefox webkit` fetched both
builds. Firefox launched and passed the `@cross` set locally. `getDisplayMedia` was
probed on a page of 127.0.0.1 with a click (it needs a user gesture) and never
answered. WebKit does not launch on the development host (missing `libevent-2.1-7t64`,
`libavif16`, `libwoff1`; no root here), so its column comes from CI, where
`--with-deps` installs them: sign-in, messages and the `63` messaging smoke run
there. WebKit's fake microphone, fake display and WebRTC rows stay not
established (`@voice` tests are left out of it). WebKit logs one console error on
every page, "Viewport argument key "interactive-widget" not recognized and
ignored" (the viewport meta in `index.html`); the F7 test drops that exact
message for WebKit only and still fails on any other console error.

Tests that pass on Firefox and are tagged `@cross`: the six of `01-sign-in`,
the five of `02-messages` other than F15's tab-stop count (Firefox's log has one
tab stop more than Chromium's: 8, not 7), and the two of `63-multi-user-smoke`
(the second is also `@voice`), and the one of `65-who-hears-whom` (`@voice`, and
Firefox only with the null audio sink below): 14 tests on Firefox, 12 on WebKit.


## Multi-user specs and helpers

These specs use separate principals, each in its own browser context, and
check what LiveKit itself reports, not only what the pages show (#1608).

| Spec | Lane | What it proves |
|---|---|---|
| `63-multi-user-smoke` | `@smoke`, pull request | three principals send one message each to #general and every page shows each once; three join voice, see each other, and LiveKit's own participant list holds exactly them until they leave |
| `64-five-people` | `@heavy` | five principals join voice and chat; LiveKit lists five, then three, then none |
| `65-who-hears-whom` | pull request, `@cross` | three separate principals in one room, where only the talker is heard, by per-track tone analysis (each person on their own tone), through push to talk, the open microphone with mute, and deafen; nobody hears themselves. Does not prove: real microphones, echo cancellation, speakers, other machines or the desktop helper |
| `66-share-permission-and-pixels` | `@smoke`, pull request | `share_screen` off before a share (refused, 403 `voice_not_allowed`) and during one (ended, `access_lost`), checked in the audit and in LiveKit's own track list; a viewer's picture is not flat and animates; a stopped share started again is a new LiveKit publication. Does not prove: real displays, GPUs, Wayland, Firefox or Safari capture, other machines |
| `67-voice-recovery` | pull request, except one `@heavy` test | voice recovery on one machine: one person's LiveKit signal socket dropped (`routeWebSocket` on `/rtc`) while two others stay, a page offline for 5 s or more (`setOffline`), LiveKit frozen for 8 s (`docker pause`, the `@heavy` test), a page that vanishes without leaving, and a person who leaves and joins again; each checks LiveKit's own list for no ghost and no duplicate. Does not prove: any per-participant media cut (none exists with a same-machine host-network LiveKit; `setOffline` leaves WebRTC UDP alone in Chromium), real network loss, NAT rebinding, Wi-Fi roaming, a mobile background kill, or a TURN path; those are by hand on a real network |

Helpers for multi-user specs (use these; a spec that needs a second copy of
one of them should change the helper). All are in `web/e2e/`:

- `people.ts`
  - `enter(browser, vw, member, options)`: a principal in a browser context of
    its own, signed in, the channel open. `options.context` is the context's own
    options (a phone's screen); `options.before` hooks the page before it
    loads; `options.mode` is `'push to talk'` (the default) or `'unchosen'`.
  - `joinVoice(p, 'Join voice' | 'Watch only')`: joins and waits for the
    connection (and, for Watch only, for an earlier page's presence to clear).
  - `leave(p)` (or `p.close()`): closes the person's pages, then their context.
  - `inbound(page)`: video frames, frame width and audio bytes a page has
    received (`perTrack: true` for each stream).
  - `expectClean(vw, p)`: no secret of the world on the page, in its console,
    storage or timing entries, and no policy violation or failed request.
- `fixtures.ts`: `closeAll(context)`, the way to close a context that has had
  a page. **Every `context.close()` goes through it, except a deliberate
  crash** (the one in `67-voice-recovery`, a tab that dies without leaving, which
  must not be closed page-first or it becomes a clean leave and tests nothing,
  and says so in a comment): it closes the pages first
  and then the context. Closing a context with a page whose WebSocket is busy
  can fail with "file data stream has unexpected number of bytes" or "End of
  central directory record signature not found", because Playwright appends the
  socket's frames live to a trace file and `context.close()` zips that file
  while it is still growing (found in #1660, seen under load). A page closed
  first has closed its sockets, and its `pagehide` ends its voice session.
- `voice-world.ts` (the `vw` of a voice world):
  - `vw.members(n)`: n more principals, each in the channel.
  - `vw.participants()`: LiveKit's own list of who is connected, as identities
    (`p<id>`, with `.screen` or `.view` for a companion), sorted, one entry per
    connection. Specs use `vw.participants()`, not `listIdentities`, which is
    its implementation.
  - `vw.tracks(member, 'microphone' | 'screen')`: the tracks one member
    publishes, from LiveKit, sorted by sid.
  - `vw.rooms()`: how many rooms LiveKit lists; `0` once the last has closed.
  - `emptiesWithin`: how long LiveKit may take to list nobody after the last
    connection goes, for `expect.poll(...).toEqual([])` and
    `expect.poll(() => vw.rooms(), ...).toBe(0)`.
  - `vw.pauseLiveKit()` and `vw.unpauseLiveKit()`.
- `livekit-admin.ts`: `adminToken` (the HS256 admin token) and
  `LiveKitStatusError` (a call's method and HTTP status, never a body), which a
  spec seldom needs; and what a spec does need to read `vw.participants()`,
  `vw.tracks()` and `vw.rooms()`: `noAnswer` / `NoAnswer`, `answered()` and
  `mustAnswer()` (below), plus `untilAnswered` and `LiveKitNoAnswerError`, which
  produce them.
- `people.ts` also exports `keepConnections`, the init script that keeps a page's
  peer connections for `inbound`; `enter` installs it, and a spec that builds its
  own context installs it with `page.addInitScript(keepConnections)`.

Polling LiveKit: a throw inside an `expect.poll` callback fails the poll at
once in Playwright 1.61, it is not retried. So `vw.participants()`,
`vw.tracks()` and `vw.rooms()` do not throw on a timeout or a transport error.
They return `T | NoAnswer`, where `noAnswer` is a symbol and not a list or a
count, so no assertion on a list, a length or a count can be satisfied by it, and
TypeScript makes the caller choose:

- in a poll, `expect.poll(() => answered(vw.participants()))`: `answered` turns
  no answer into `undefined`, which matches nothing, so the poll goes on;
- in a one-off assertion, `expect(await mustAnswer(vw.participants()))`: no
  answer fails the test with "LiveKit gave no answer";
- a helper built on a reading (`videoSids`, `microphones`) returns `T | NoAnswer`
  itself and never filters, maps or counts a `NoAnswer` as if it were a list.

A status error (a wrong key, say) still throws.

## Flake policy

`retries: 0` in both lanes: a retry hides the flake. A failing heavy run
does not notify anyone; it is noticed with `gh run list --workflow
e2e-heavy.yml --limit 7`, which also makes the "twice in 7 runs" rule
checkable, and triaged at the principal's next session. A test that fails
twice in 7 heavy runs with no related change gets an issue. It may be quarantined with
`test.fixme` and a link to that issue only if the pull request description
says so (CLAUDE.md, rule 6): never skip or delete a failing test silently.

## Artifact policy

- On failure each lane uploads `web/test-results` (Playwright traces) with
  7-day retention.
- A trace holds the run's fixture credentials (Bearer headers, the filled
  sign-in value), and Playwright cannot redact it. They are throwaway: the
  world is torn down (server stopped, root removed) before the upload, so a
  downloaded trace opens nothing.
- No `conchd` or LiveKit log is uploaded unless it has first been passed
  through the world's secret list. No secrets are printed to a log.
- Each LiveKit container is labelled with the run (`VOICE_CHECK_RUN`), and an
  `if: always()` step removes only that run's containers.
