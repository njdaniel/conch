# Changelog

All notable changes to Conch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). A tag with a pre-release suffix
(`v0.1.0-alpha.1`) is published as a GitHub pre-release; a plain tag is a draft
release that Nick publishes.

## [Unreleased]

### Added

- Archive and restore a channel without deleting its history (#844, server,
  schema and CLI; the browser follows). `PUT /v1/channels/{id}/settings` takes
  `"archived": true|false` (`manage_channels`, audited as
  `channel_settings_changed`). An archived channel keeps its history, members
  and read marks and refuses anything new with 409 `channel_archived`: posts
  (REST, webhook, MCP), edits, reactions, uploads, voice claims, self-join,
  adding members, and invites naming it. Voice in progress ends when it is
  archived. A default channel cannot be archived (409
  `channel_archive_default`). `conch admin channel settings <channel>
  --archive|--restore`; `conch channels list` gains an `archived` column.
  Additive wire fields (`archived_at`, `archived`) and migration 41
  (`channels.archived_at`); no schema version bump.

## [0.1.0-alpha.14] - 2026-11-04

This is a pre-1.0 release (`v0.1.0-alpha.14`): the API and the wire shapes
can still change between alphas. There is no breaking change in it, and no
wire change: an `alpha.13` client works against it unchanged.

The headline is cameras for groups of up to nine and on phones. It also
brings the client half of hiding members from each other, a round of polish
from the third browser audit, and the camera load-test tool. Upgrade with
`conch-server upgrade v0.1.0-alpha.14` from your workstation, or with
`conch-upgrade.sh` on the droplet (docs/deploy.md step 11).

There is no migration since `alpha.13`, so rolling back to `alpha.13` is a
plain downgrade. **The camera stays off for everyone but owners until an
owner turns it on** (`use_camera`), as in `alpha.13`.

**For operators: the sign-in page folds its operator section** behind
"Operator or script?" (#1042, F24). Open it to paste a credential or to set
up an operator account; it opens by itself when the credential is the only
way in, or after a credential was refused.

### Added

- **Cameras for groups of up to 9, and on a phone** (ADR-015, #973; #1267,
  PR #1274; #1268, PR #1276). Web only; the server and the wire are as in
  `alpha.13`. As before, **camera is off for everyone but owners until turned
  on** (`use_camera`).
  - **The call view holds up to 9 tiles** (it held 4). Every person in voice
    gets a tile while there are 9 or fewer, with their initial and "Camera
    off" when there is no camera; with more, only the cameras get tiles. The
    speaking person's tile has a ring and the word "speaking", and nothing
    reorders. A tile is a button, "Enlarge {name}'s video", with "Back to
    grid" and Full screen, and a camera can sit beside a watched share (the
    share is the main picture and the cameras are a strip, on its right from
    1100 px wide).
  - **The phone rules** (ADR-015 O9, O10, O11). On a narrow window or a
    touch-only device, video is drawn and received only in the Voice view, so
    a phone in Chat receives none; the voice strip stays one row of four, and
    its state button says "Camera on"; the camera and the camera test stop
    when the page is hidden and never restart by themselves; the capture is
    640 x 360 at 24 fps with one smaller layer, asking for the front camera
    first. These are the SDK's presets, not measurements.
- **Who you see, and the authors who left** (#996, PR #1281; the client half
  of hiding members from each other, as ADR-014 narrowed it). When members
  do not hold `see_member_directory`, the member panel says "You see the
  people you share a channel with", in the browser and the TUI's community
  list. Authors who are no longer in the member list are named through
  `GET /v1/channels/{channel}/authors` in the browser's conversation and
  threads and in the TUI history, falling back to `#<id>`; a hidden author
  reads as the server's `hidden-<id>`. The CLI's history lines keep the
  author id, since their format is pinned for scripts.
- **The camera load test** (#1284, PR #1293). `scripts/alpha-loadtest.sh
  --camera-publishers N` (0 to 9) makes each of the first N simulated
  participants claim its camera before it publishes, so a 9-camera
  measurement needs no nine real devices. It refuses to start unless
  members hold `use_camera`, and never changes permissions.

### Changed

- **The third browser audit's polish** (#1042, PR #1292):
  - the phone's Join voice button keeps its words in one column (F17);
  - a touch screen shows no keyboard hints, and the editor says "Save or
    Cancel" there (F18);
  - the invite page's version badge no longer covers its text, and "guest"
    names are "reserved" (F19);
  - the notification hint follows the chosen policy (F20);
  - an unused invite reads Unused, paused ones Paused, with a banner while
    invites are paused; Escape closes the invite panel (F21);
  - the release-delay slider steps 50 ms, and PageUp and PageDown 250 ms
    (F22);
  - the sign-in page folds its operator section (F24, above);
  - every time and date comes from one 24-hour format, an image sent alone
    shows its name once, "Sent" shows only under your newest message,
    Escape closes search, the report review shows a summary, and an address
    naming a channel you cannot see is corrected to the one shown (F26).

### Fixed

- **A hide reaches the readers of the hidden person's messages** (#1282,
  PR #1283). When members are hidden from each other, someone who held a
  hidden person's old messages but shared no channel with them kept seeing
  the old name until a reload. They are now told of the hide, and see
  `hidden-<id>`. The new audience is exactly the people the authors route
  already names that person to.

### Docs

- **The camera is documented** (#1269, Part A; Part B, the by-hand and
  bandwidth checks, is Nick's). README has a Camera section on the pattern
  of Screen share (the routes, errors, cap, idle and rate bound, the
  operator stop that needs `manage_channels` and is stricter than the share
  stop, presence, the audit actions, the browser and phone behaviour, the
  SDK's quality presets, the trust model, guests and a browser table) and
  the CLI's `+camera` and `conch voice camera stop`. `docs/deploy.md` says
  that turning the camera on for members takes two cells, that the voice
  switch withholds it, that both presets leave it off, and what is and is
  not known about bandwidth. The tester guide has a "Your camera" section,
  camera columns in the browser table and lines in the known-limits list.
  `docs/reports/TEMPLATE-camera-bandwidth.md` is the measurement method.
  **No capacity is claimed:** every bitrate is the SDK's preset, and every
  browser but Chromium with a fake camera is untried. The video is
  encrypted on each hop but not end-to-end encrypted, and Conch records
  nothing.

### Tests

- **CI runs its heavy jobs on self-hosted runners** for this repository's
  own branches (#1286, PR #1287; #1288, PR #1289): `check`, `web-e2e`,
  `voice-check` and `isolation-check`, after the hosted runner ran out of
  disk restoring the voice build. A fork's pull request stays on GitHub's
  runners.
- **`web-e2e` runs as two shards**, each with its own server (#1290,
  PR #1291), and its job bound is 30 minutes (PR #1276): about 19 minutes
  became under 10.
- Test-only fixes: the 30-second empty-list test's budget (#1279, PR #1280),
  and the phone camera spec runs in a wide font too, so a font cannot hide a
  clipped strip (PR #1276).

## [0.1.0-alpha.13] - 2026-11-04

This is a pre-1.0 release (`v0.1.0-alpha.13`): the API and the wire shapes
can still change between alphas. There is no breaking change in it.

The headline is webcam video in channel voice rooms (ADR-015): one to one
and small groups, in the browser, with a `use_camera` permission that is off
until an owner turns it on. It also brings a Leave channel item to the
channel menu, an admin console that updates itself, and web clients that
accept permissions and warnings they do not yet know. Upgrade with
`conch-server upgrade v0.1.0-alpha.13` from your workstation, or with
`conch-upgrade.sh` on the droplet (docs/deploy.md step 11).

There is no migration since `alpha.12`, so rolling back to `alpha.12` is a
plain downgrade. The wire changes are additive only, listed after Added, and
an `alpha.12` client ignores them. **Nothing changes for a server that does
not use cameras:** `use_camera` is off for operators and members on upgrade.

### Added

- **Webcam video in channel voice rooms** (ADR-015, #973; #1238, #1239,
  #1240, #1241, #1242, #1243). A person in a channel's voice room can turn
  on a camera, and the others in the room can watch it.
  - **A camera is a permission, `use_camera`, and it is off by default** for
    operators and members. The owner turns it on in Settings, Roles and
    permissions: the operator cell first, then the member cell (a member
    cell cannot be on while the operator cell is off). It needs `use_voice`,
    and the voice switch (`voice_allowed`) also withholds it: there is no
    camera-only switch. Applying a preset leaves the operator cell and turns
    the member cell off (#1240).
  - **One to one and small groups.** A room holds at most nine cameras (the
    tenth claim is refused with 409 `voice_camera_full`), and the browser's
    call view shows up to four tiles, with a "Show video (n)" control, a
    camera picker, a test camera and a mirrored self-view (#1242).
  - **conchd enforces kind and source on every track** (#1241). A video
    track declared as the microphone, an audio track declared as the camera,
    a camera without a claim, and a companion's or viewer's camera are each
    withdrawn and audited once as `voice_camera_unclaimed`; the publisher is
    removed if the track survives. LiveKit itself accepts such tracks, so
    this check is the only thing that stops them. Camera starts and ends are
    audited too (`voice_camera_started`, `voice_camera_stopped`, with the
    reason). The routes are `POST` and `DELETE
    /v1/channels/{channel}/voice/camera`; an operator with `manage_channels`
    stops someone's camera with `DELETE ...?principal=`. A claim that
    publishes nothing for 15 seconds ends as `idle`.
  - **A camera ends before the route answers** when its permission is
    withdrawn: the permissions PUT, the voice switch, a role change, losing
    the channel, disabling a person, revoking a credential and a room
    rotation all end it (#1241). Stopping a camera never drops a screen
    share, and the reverse.
  - **The browser asks for the camera only from the Camera button** (#1242).
    Joining voice, unmuting and sharing the screen never prompt for it, and
    leaving or signing out stops the capture. A denied camera leaves voice
    and chat working.
  - **The CLI** (#1243): `conch voice status` marks a person with a camera
    with `+camera` on the state field (`quiet+camera`, `talking+camera`),
    and `conch voice camera stop <channel> <person>` is the operator stop.
    Lines for people without a camera are unchanged.
  - **A new `Permissions-Policy`** (#1242): `camera=(self)` on `/app/`, and
    deny-all (`camera=(), microphone=(), display-capture=()`) on every other
    answer, the metrics listener included.
- **Leave channel in the channel ⋯ menu** (#1095, PR #1263), for members of
  the channel (not guests). A public channel is left at once; a private one
  asks first, since it cannot be found again. A refusal, such as the last
  member of a private channel, is said under the row.

**Wire changes since `alpha.12`, all additive; no version bump.**
- `use_camera`, the twelfth value of the `Permission` catalog (#1238). Off
  for operators and members, requires `use_voice`, not operator-only. Every
  other default is unchanged.
- `camera` on `VoiceParticipant` (it needs `can_publish`) and `camera_ended`
  on `VoicePresenceV1`, with the closed `VoiceCameraEndReason` vocabulary
  (the same six values as the share's) (#1238).
- `VoiceCameraV1`, the camera route's answer, and its parameters
  `started_at` and `principal` (#1238, #1241).
- Three error codes: `voice_camera_full`, `voice_camera_stale` and
  `voice_camera_rate_limited` (#1238).
- On the TypeScript side, eight open vocabularies are now client-tolerant
  (`Permission`, `PermissionRole`, `PermissionPreset`, `PermissionsWarning`,
  `IssuedKind`, `JoinWarning`, `InviteKindV2`, `DirectoryScope`); the other
  57 stay strict (#1051). No fixture changed.

### Changed

- **Older web clients accept permissions and warnings they do not yet know**
  (#1051, PR #1253). Before, a page that met a new permission (such as
  `use_camera`) in a response refused the whole answer. Now it keeps the
  value: the console shows an unknown preset, warning or issued kind by its
  name, and Revoke is disabled for an unknown issued kind so it is never
  sent to a hook route. Unions, states, roles and the voice protocol stay
  strict.
- **The admin Members and Channels sections update themselves** (#1226,
  PR #1237), when the server says the directory changed. Members loses its
  Refresh button. An open panel and its focus survive the re-read.
- **The roadmap moves webcam video into the private alpha** (PR #1260, with
  Nick's sign-off of 2026-11-03). Docs only.

### Fixed

- **A screen-share revocation waits for a retiring room's withdrawal**
  (#1265, PR #1272). When a share's permission was withdrawn while a room
  rotation was still withdrawing that share, the route could answer before
  the share had ended. It now waits, as a camera's revocation already did
  (#1241). The race predates cameras.
- **A camera start follows a newer camera choice** (#1266, PR #1275). If the
  camera chosen while starting could not be had and the person had already
  chosen another, the start ended instead of moving to the newer choice.
  Now it follows the newer choice, and a choice changed back to a camera it
  had already tried still publishes a live picture.

### Docs

- **The tester guide and README say the browser starts in the open
  microphone** (#1261, PR #1262), name the "Open mic" toggle and say the
  choice is kept. Docs only.
- **The docs describe the community model as it ships** (#997, PR #1270).
  README, `docs/deploy.md` and the tester guide cover member links, the
  sign-in-only front page and `open_signup`, default channels with their
  history, Browse channels, the member directory, voice on by default, and
  no guests for now. The older private-alpha design notes say that ADR-013
  supersedes them.
- **README and `docs/deploy.md` give the camera policy and the twelfth
  permission** (PR #1273, refs #1269): `Permissions-Policy` is
  `camera=(self)` on `/app/`, and `use_camera` is listed with the other
  permissions, off for operators and members.

### Tests

- Test-only and flake fixes, with no change to behaviour: the e2e Docker
  probe retries a call that times out (#1251), the Channels and Members
  section tests wait for settled work instead of 30 ms (#1255), and the
  mode-toggle helper in the phone voice strip spec presses again until the
  mode takes (#1244, PR #1254).
- New coverage for the camera: an approval full-chain test that turns
  `use_camera` on and off while an approval is open (#1240), the pinned
  LiveKit permission requests of every publish-permission write (#1239), and
  `e2e/voice` and `e2e/isolation` rows for the camera (#1241).
- **The community newcomer scenario runs end to end** (#989, PR #1271), in
  a new CI job, `community-check` (`go run ./e2e/community`), on every pull
  request and push to `main`. The owner makes a member link; a newcomer
  redeems it, reads `#general`'s history, finds and joins a channel made
  public, and joins its voice. Her voice is switched off and back on, the
  closed link stops working, and the server admits no guest. It runs
  against a real conchd and the pinned LiveKit, through the API and media
  control, not the browser.

## [0.1.0-alpha.12] - 2026-11-03

This is a pre-1.0 release (`v0.1.0-alpha.12`): the API and the wire shapes
can still change between alphas. **One breaking change: members can no longer
use `POST /v1/invites`** (#990). Read the upgrade notice before upgrading.

This finishes the community model for people who are not operators. A server
can hide its members from each other, and the web app, the TUI and the CLI
all learn the community: a channel directory, a people pane, member links and
joining, an admin console with Roles and permissions, Members and Channels
sections, and a sign-in-only front page. Operators can list and revoke hooks
from the CLI and disable a link's joiners from the web, and adding someone to
a channel by id can never take away a member's right to decide approvals.
Upgrade with `conch-server upgrade v0.1.0-alpha.12` from your workstation, or
with `conch-upgrade.sh` on the droplet (docs/deploy.md step 11).

There is no migration since `alpha.11`, so rolling back to `alpha.11` is a
plain downgrade. The wire changes are additive only, listed after Added, and
an `alpha.11` client ignores them. Behaviour does change: read the upgrade
notice below, and Changed.

**Upgrade notice.**
- **A private server should apply the `personal` preset first.** Hiding
  members (#988) works through the member cell of `see_member_directory`,
  which the `group` defaults leave on. See "Upgrade notice: roles and
  permissions (ADR-014)" at the end of this section.
- **Members can no longer use the v1 invite route.** `POST /v1/invites`
  answers a caller without `manage_members` with 409 `invite_kind_retired`
  (#990). Scripts that make invites as a member must use `POST /v2/invites`
  (`conch admin invite create` already does).

### Added

- **Hide members from each other** (#988, community model I14a). Switching
  the member cell of `see_member_directory` off, through `PUT /v1/permissions`
  or the `personal` preset, makes the directory show a member only the people
  they share a channel with, themselves, and those they invited. Operators
  always see everyone. Adding someone to a channel, and reading a channel's
  authors, answer the same 404 `principal_not_found` for a person you may not
  know as for one who does not exist.
  - `GET /v1/channels/{channel}/authors?ids=` names the authors of messages
    in a channel the caller is in, for up to 100 ids, and only those the
    caller may see.
  - The events socket sends `conch.directory_changed.v1` (a frame with no
    fields) to whoever may know the person who changed: when someone is
    created, renamed, disabled, enabled, hidden or given a new role, and when
    the permission is switched. Frames are coalesced.
- **The web app follows the community** (#991, #993). A channel directory
  lists public channels and your private ones, with a lock, and a member
  joins or leaves from it (leaving a private channel asks first). Members
  create channels as `create_public_channels` and `create_private_channels`
  allow. A people panel lists who you may know. It, the channel's member list
  and the add-people picker refresh on `conch.directory_changed.v1`, so the
  Refresh button is gone.
- **Member links and a sign-in-only front page in the web app** (#990). The
  front page offers Sign in, and "Create an account" only while open sign-up
  is on; otherwise it says "This server is invite-only" and never offers a
  guest visit. Every member has an Invite people panel: make a member link
  with an optional expiry and use limit, see your own links with their uses
  and joiners, and close them. "Never expires" shows only to holders of
  `manage_members`, and the panel says so when an operator has turned your
  invite brake off. A link opens a landing page that previews and redeems it
  through the v2 routes.
- **The web admin console grows three sections.**
  - Roles and permissions (#1019): the permissions matrix, with locked cells
    explained, the presets with what each changes, and the warnings.
  - Channels (#1021), for holders of `manage_channels`: every channel with
    its lock, default mark, member count and who manages it, and a menu to
    add a person, rename, make private or public, make or stop being a
    default channel, grant or remove managing, and remove a person.
  - Members (#1020): the member list and a person menu that offers only the
    actions the viewer may take on that person (nobody but an owner acts on
    an owner, no sign-in link for an operator, owner or agent, and so on).
  - "Disable joiners of link N" on each Invites row that has joiners, for
    holders of `manage_members`: it counts first, asks, then disables (#1228).
- **The CLI learns the community model** (#994, #1228).
  - `conch channels list|join|leave|create` (`list --all` for operators,
    `create --private`).
  - `conch admin invite create|list|joiners|close` work on member links, with
    `--expires`, `--uses`, `--never-expires` and `--credential-days`. The old
    flags (`--open`, `--personal`, `--channel`, `--voice`, `--may-invite`,
    `--join-as`, `--max-uses`) still parse, then fail with a line naming what
    replaces them.
  - `conch admin channel settings <channel> [--public|--private]
    [--default on|off]`, `conch admin principal voice <id> on|off`, and
    `conch admin directory members`.
  - `conch admin join set --open-signup on|off --link-expiry`, and a
    `status` that shows open sign-up, the default channels and
    `no_default_channel`.
  - `conch admin hook list` and `conch admin hook revoke <id>`. The list shows
    each hook's id, state, channel, maker and label, and never a token.
  - Refusals print the CLI's own words by error code, never the server's
    text, and a 429 shows its wait.
- **The TUI learns the community model** (#995, #1199). `ctrl+d` or
  `/channels` opens the channel directory (join, leave, create), `ctrl+p` or
  `/people` the community pane, and `v` on a person's row, for operators,
  switches their voice. Holders of `manage_channels` press `a` in the
  directory for the every-channel view with member counts.
- **Add a person to a channel by id, without ever changing an existing
  member** (#1206). `POST /v1/channels/{channel}/members` now takes `id` as
  well as `name` (exactly one), and an operator may send `can_decide` for a
  new member. Someone already in the channel is left exactly as they are
  and the answer is 200 with their membership, so an add can never take a
  right to decide approvals away, as `PUT .../members/{id}` with
  `can_decide: false` does. A channel manager sending `can_decide` is
  refused with 403, as on the PUT. The web console's "Add to a channel" uses
  it and no longer reads the members first (#1227).
- **The web sidebar names who is in each channel's voice** (#1103). Under
  each channel, up to 5 people in its voice room are named, then "+M more";
  you are "You", and someone the page cannot name is "Member N". A name only
  opens the channel; it never joins voice. Phone rows are 44 px. The ids come
  from `conch.voice_occupancy.v1` (below), and the page names them from the
  channel's member list, so it shows only people the viewer may already know.

**Wire changes since `alpha.11`, all additive.**
- `managers` on the `?scope=all` channel listing (`ChannelListingV1`, #1202):
  the people who manage that channel, sent only to holders of
  `manage_channels` and omitted from the member view.
- `id` and `can_decide` on `AddChannelMemberRequestV1` (#1206); `name` becomes
  optional. The old request is byte-identical.
- `principal_ids` on `VoiceChannelOccupancy` in `conch.voice_occupancy.v1`
  (#1103): the distinct people in the channel-wide room, ascending, `[]` for
  nobody, always sent by conchd. Ids only, no names. A client that does not
  know it ignores it, and the published fixture (no ids) stays valid.
- The route `GET /v1/channels/{channel}/authors` and the
  `conch.directory_changed.v1` events frame (#988); their shapes were
  already in `pkg/schema`.
- None from the CLI and TUI size caps (#1198, #1203) or the exported page
  maxima (#1221).

### Changed

- **The browser joins voice with the open microphone** (#1244), where it
  started in push to talk. The person's choice is kept per browser, and an
  "Open mic" toggle beside the talk control switches it in one tap, on the
  desktop voice bar and on the phone strip.
- **`POST /v1/invites` is for operators who manage members only** (#990).
  A caller without `manage_members` now gets 409 `invite_kind_retired`:
  members make member links with `POST /v2/invites`. An operator holding
  `manage_members` keeps v1 until #1134 retires it.
- **The last person in a private channel can no longer leave while only
  non-members remain** (#1150). An agent, a disabled or hidden person or a
  visiting guest no longer counts as someone left to run it. Public channels
  are unchanged.
- **A disabled person is not listed as a manager** (#1210), in the
  `?scope=all` listing or a channel's members, until they are enabled again.
  It is display only: who may manage is decided as before.
- **Web: a rename refused with `channel_exists` reads "That name is taken"**,
  never hinting that the name belongs to a private channel you cannot see
  (#1195).
- **The web app no longer offers a guest visit or the per-channel invite
  tick**, and no longer calls the old `POST /v0/channels` (#990, #991).

### Security

- **Demoting an operator closes the member links a member could not hold**
  (#1165). It happens in the same transaction, with an `invite_closed` row
  each: every member link when their `may_invite` brake is off, and the
  never-expiring ones when it is on. Before, links made under the role
  survived a demotion with the brake off, and turning the brake on later
  brought back links a member could never make. `invite_closed` for a member
  link now says `link=member`.
- **The CLI and TUI cap the response body of every list read** against a
  hostile server: the community lists (#1198), and the rest, including
  messages, reports, runs, activity, interventions, nets and credentials
  (#1203). The thread loop also stops at the page limit.
- A guard test lists every store function that reads principals and fails on
  one that bypasses the "may know" predicate, so hidden members stay hidden
  as routes are added (#988).

### Docs

- **Docs for running a personal or a group server** (#1023, ADR-014).
  `docs/deploy.md` has a new part, "Running a personal or a group server",
  with the console path first and the `conch admin` command after for the
  roles and the owner mark, the preset (what each changes), member links and
  the invite brake, channel privacy and default channels, hiding members, and
  what an operator can and cannot see. README gains an administration
  paragraph. Docs only: no code, wire or migration change.

### Tests

- Test-only and flake fixes, with no change to behaviour: a channel-refresh
  test race (#1185), a notifications test (#1180), the voice binary,
  slow-server and leave tests (#1141), voice tests that left clients running
  (#1157), the e2e wait for the post-restart message (#1156), a leave-sockets
  test (#1207), the phone navigation and voice strip specs (#1214, #1232), and
  two web unit tests (#1002, #1235).
- The isolation e2e gains a restricted row for every permission gate, a
  personal-preset phase and the hide-members rows (#1017, #1190), and the
  schema exports the message, run and report page maxima that the server and
  the client share (#1221).

### Upgrade notice: roles and permissions (ADR-014)

For anyone who runs a personal server, or moves one across `alpha.8` to
`alpha.10`.

- **The migration changes nothing.** Migration 39 (`alpha.8`) adds the
  `role_permissions` table, empty, so every permission reads as its default
  and behaviour is the same until an owner changes a cell.
- **Apply `personal` once, before members can see each other.** The member
  directory (`alpha.10`, #983) shows every member to every member by default.
  On a personal server, apply the preset (`conch admin permissions preset
  personal`, or the setup card in Settings → Server) before the release with
  the directory reaches it. A server already on `alpha.10` or later that never
  applied it is showing its members to each other now: apply it now. This release (`alpha.12`)
  also draws the directory in the web client (Community
  members, Browse channels), so a personal server upgrading to it must have
  applied the preset first. Browse channels lists every public channel to
  members; the preset doesn't turn that off.
- **Rolling back forgets a preset.** A binary from before `alpha.8` refuses a
  database that has migration 39, so rolling back restores the pre-upgrade
  backup (docs/deploy.md step 11; the rollback script does it), and a preset
  applied since the upgrade goes with it.
- **Owners skip brakes.** A voice or member-link switch stored on an owner
  no longer counts: an owner whose voice or link-making was switched off has
  them again.
- **Open sign-up members can make links.** Members who join through open
  sign-up hold the member-link switch, so, while members hold
  `make_member_links`, they can make member links straight away, and they
  keep that after open sign-up is switched off. Close their links or remove
  the people a link brought in (ADR-013 decision 5).
- **Members who made links see the people who joined through them**
  (#988). With the directory off, a member sees themself, the people in
  their channels and the people who joined through a link they made. That
  is recorded at join and never cleared, so on a server that ran the
  defaults first it stays after link-making is switched off; `conch admin
  principal list` shows "invited by" to find them. The
  server now sends the `default_channel_exposes_members` warning when
  members lack the directory and a default channel exists. Before this
  release it didn't, so check for default channels yourself on a server
  that runs an earlier one.
- **The preset leaves the channel directory on.** There is no permission
  for it: on a personal server a member's **Browse channels** still lists
  every public channel, and any member may join one. Keep a personal
  server's channels private.

## [0.1.0-alpha.11] - 2026-11-02

This finishes the joining side of the one-community model: open sign-up and
member links both make a full member in the default channels, and members
find, join and leave public channels themselves. Private channels now show a
lock, and operators make a channel public or private from its menu. The web
app has an admin console (Server, and Reports), agents can reply in threads
over MCP, and a member's re-authentication no longer carries over a
promotion. Upgrade with `conch-server upgrade v0.1.0-alpha.11` from your
workstation, or with `conch-upgrade.sh` on the droplet (docs/deploy.md
step 11).

There is no migration and no wire change since `alpha.10`, so rolling back to
`alpha.10` is a plain downgrade. Behaviour does change: see Changed.

### Added

- **The channel directory; members join and leave channels themselves**
  (#982, community model I3b).
  - `GET /v1/directory/channels` lists every public channel and the private
    channels the caller is in, each with `joined`, `default`, `visibility`
    and (when joined) `manages`, and no member counts or names.
    `?scope=all` lists every channel with its `members` count, for holders
    of `manage_channels` only. Agents and guests are refused.
  - `POST /v1/channels/{channel}/join` joins a public channel, as a member
    who does not decide its approvals and with its whole history. Joining a
    channel you are in returns your membership unchanged. A private channel
    you are not in answers the same 404 as one that does not exist. Agents
    and guests get 403 `channel_not_joinable`.
  - `POST /v1/channels/{channel}/leave` leaves any channel you are in, and a
    manager's grant goes with it. The last member of a private channel may
    not leave it (409 `channel_last_member`). A decider who leaves takes
    their decisions with them: pending approvals count only the deciders
    who remain.

- **Private channels show a lock, and operators change it from the
  channel's menu** (#1183). A private channel's row shows a lock in place of
  `#`, live as it changes, and screen readers hear "private channel". A
  holder of `manage_channels` gets "Make private" or "Make public" in the
  channel's ⋯ menu, which asks first, and "Make default channel" or "Stop
  being a default channel". A default channel must be public, so each offers
  the other only once that holds, with the reason shown.
- **The web admin console** (#1018). Operators and the owner get a Server
  administration area in Settings: the server's name, a setup card on a new
  server (Personal, Group, or decide later) that shows what a preset changes
  before applying it, the permissions summary and warnings, and the joining
  settings with every member link. Members see one line saying who
  administers the server. It follows permission changes live.
- **Agents reply in threads over MCP** (#1171). `post_message` takes
  `thread_root_id` and follows the REST rules, and `read_channel` and
  `post_message` now carry `thread_root_id` and `guest_author`, which MCP used
  to drop.

### Changed

- **Open sign-up makes a full member, and the bare address never admits a
  guest** (#987, ADR-013). The joining settings stay on
  `GET/PUT /v1/join/settings`, with one switch, `open_signup`, in place of the
  four joining modes.
  - While open sign-up is on, `POST /v1/join/account` makes the member a
    member link makes: the default channels with the whole of their history
    (no history bound), no right to decide approvals, voice on, and the
    right to make member links on. Before, such an account started with a
    history bound in each channel and no right to invite. `joining_paused`
    still pauses it, and turning it on needs at least one default channel.
  - `open_channels` now refuses a private channel with
    `409 channel_settings_conflict`, where it used to make the channel public.
    Make a channel public first with `PUT /v1/channels/{id}/settings`.
  - `join_voice: false` is refused with `409 join_policy_conflict`: every
    member may use voice. `conch admin join set --voice off` gets that answer.
  - `POST /v1/join/guest` always answers `403 guests_disabled`, and
    `GET /v1/join` never offers a guest and always offers voice. Guests, when
    they return, come through guest links (#1004).
  - The settings answer carries `default_channels` and warns
    `no_default_channel` when no channel is a default channel.
- **Redeeming a link makes a full member** (#986, ADR-013).
  `POST /v2/invites/redeem` with a member link lands the joiner in the
  default channels as they are at that moment (none is fine), or in the
  link's landing channels, with the whole history, no right to decide
  approvals, voice on and the right to make member links on.
  - An invite made before the upgrade still lands in its own channels its
    maker still stands behind, and now also in the default channels. One with
    none of its own channels left is still refused.
  - Every joiner has voice and may make member links, whatever the invite
    said: both are switches an operator turns off for one person
    (`PUT /v1/principals/{id}/voice-permission`, `.../invite-permission`).
    Before, an invite could withhold voice, and only an operator's invite
    could pass on the right to invite.
  - For old clients, `GET /v1/invites` and `POST /v1/invites/preview` show a
    member link as an `open` invite into the default channels as they are
    now, with voice, no right to invite passed on, joining as an account, and
    its expiry (or the never-expires sentinel).
  - For an invite made before the upgrade, `GET /v1/invites` still shows the
    voice and right to invite it was made with, and its list and preview show
    only its own channels, not the default channels its joiner now also
    lands in. Redemption follows the rules above, not those fields.
  - Such an invite keeps today's rules for its own channels, including one
    made private since: it still lands its joiner there while its maker stands
    behind it (an operator holding `manage_channels`, or a member still in the
    channel), until it expires or an operator closes it.

- **Web: the message log is one Tab stop** (#1040). Arrow keys move between
  messages and Home and End go to the first and last, and Tab walks only the
  chosen message's controls. Controls are named by author and time ("Reply
  to ada's message, 21:38"), never by message id.

### Fixed

- **No request the page knows will fail** (#1084). Changing your password
  while staying signed in no longer sends a stale `whoami` or failing events
  sockets, and losing a channel while in its voice no longer asks for its
  presence. The console stays clean.

### Security

- Any role change (a promotion, a demotion, an owner-mark change) now clears every recent-auth proof of that person, so a member's password-only re-authentication no longer satisfies an operator route that needs password and code (#1054). Their sessions stand; they re-authenticate on the next sensitive action. The `principal_role_changed` audit row gains `proofs_cleared=N`.

### Docs

- The principal review of 2026-11-02 (P2 to alpha.10) is in
  `docs/reports/2026-11-02-principal-review.md`.
- A test fix, with no change to behaviour: the phone voice strip spec
  (#1163).

## [0.1.0-alpha.10] - 2026-11-02

This is the move to one community (ADR-013): member permissions, members
making channels, member links, the member directory, the desktop app's saved
server profiles, server allowlist and safer links and clipboard, and web
polish (member and channel actions in ⋯ menus, and more). Upgrade with
`conch-server upgrade v0.1.0-alpha.10` from your workstation, or with
`conch-upgrade.sh` on the droplet (docs/deploy.md step 11).

Unlike `alpha.9`, this release has a migration (migration 40, issue #979).
Rolling back is the previous binary plus the pre-upgrade backup, not a plain
downgrade. See the upgrade notice below.

### Upgrade notice: one community (issue #979)

The first start on this version converts the server to the community model
(ADR-013), in one migration that runs before the server takes traffic. Take
the usual backup first (docs/deploy.md does).

- **Channels.** Every channel that was open to the bare address becomes a
  public **default** channel, which new members start in. Every other
  channel stays **private**, with exactly its current members.
- **Making a channel public or private.** An operator holding
  `manage_channels` sets a channel's visibility and its default mark with
  `PUT /v1/channels/{id}/settings` (#981; see Added). Listing a channel among
  the joining settings' open channels still makes it public **and** a
  default channel, and the audit row says `visibility=private->public`.
  Taking it off that list makes it no longer a default but leaves it public;
  the settings route is how to make it private again.
- **Making a channel.** Any member can now make a public or private channel
  with `POST /v1/channels` and is its manager (#981; see Added).
  `POST /v0/channels` stays operator-only and makes a private channel.
- **Members.** Every member, a disabled account included, is added to every
  default channel, with no right to decide approvals. Anyone already in a
  channel keeps the rights they had there.
- **History bounds are cleared.** A member who could see a channel's
  messages only from when they joined now sees its whole history. The old
  bounds are kept in the database (`legacy_history_bounds`) and nothing
  reads them. Until open sign-up's change (#987) ships, someone who joins
  through the bare address afterwards still starts with a bound from when
  they joined.
- **Voice and making invite links are on for every member.** Every member
  can now make member links (`POST /v2/invites`, issue #985), which land the
  newcomer in the default channels and grant nothing else. An operator
  switches the right off for one person with
  `PUT /v1/principals/{id}/invite-permission`, which also closes that
  person's open links.
- **Member links.** Make one with `POST /v2/invites`, and read it with
  `GET /v2/invites`, `POST /v2/invites/preview` and
  `GET /v2/invites/{id}/joiners`. `POST /v1/invites` is unchanged for now: the
  browser and `conch admin invite create` still use it until they move to
  member links (#990, #994), and a later release retires it (#1134). Invites
  made before the upgrade keep working, and are listed, previewed and closed
  as before.
- **`--invite-allowance` now counts member links.** The number of open member
  links and personal invites together that one person may hold (default 5, an
  uncapped link counts as open; operators have no limit).
  A link that names no expiry lasts 7 days, and only an operator can make one
  that never expires.
- **Joining.** The bare address now either makes accounts (open sign-up on,
  where it admitted accounts before) or admits nobody. It no longer admits
  guests. Voice for new accounts is always on.
- **Guests.** Guests were switched off by #1005. The migration ends any guest
  visit still active and closes every invite that could admit a guest, each
  audited. The guest records themselves are kept.
- **Rollback** is the previous binary plus the backup taken before the
  upgrade: the previous binary refuses the converted database.

The server log reports what the migration found and what it changed, and the
audit trail has one `community_migrated` row with the counts.

### Added

- **Members make channels; operators set a channel's visibility and default
  mark** (#981, community model I3a).
  - `POST /v1/channels` (`{name, visibility}`) lets any member make a public
    or private channel, under the `create_public_channels` and
    `create_private_channels` permissions. The creator is its only member
    and its manager, and decides its approvals. Agents and guests are
    refused.
  - Every name collision answers the same 409 `channel_exists`. A member
    without `manage_channels` may make 10 channels and try 30 times a day
    (429 with `Retry-After`), and each refused try is audited
    `channel_create_refused` without the name.
  - `PUT /v1/channels/{id}/settings` (`{visibility?, default?}`) is for
    operators holding `manage_channels`. A default channel must be public
    (409 `channel_settings_conflict`). Turning a channel private keeps its
    members. Unmarking the last default channel while open sign-up is on
    turns sign-up off, and the answer says so (`open_signup_turned_off`).
  - `GET /v1/channels` and the events socket's `conch.channel.v1` carry
    `visibility` and `default`, and a settings change sends the channel
    document. `POST /v0/channels` stays operator-only and makes a private
    channel.

- **Member permissions: a document, `GET`/`PUT /v1/permissions`, and a live
  signal** (#1015, approval-path). What a member or an operator may do is now
  one permissions document, and operator routes ask for the permission they
  need instead of "is an operator".
  - `GET /v1/permissions` returns the document, what the caller may edit,
    warnings, and the credentials issued to others. `PUT /v1/permissions`
    applies patches and the `personal` and `group` presets. It refuses
    operator-column changes from a non-owner, a stale revision, and any cell
    the server doesn't enforce, and it asks for a fresh sign-in.
  - An operator without the needed permission gets `permission_denied`
    naming it, and the refusal is audited. Members and agents get the same
    403 as before. The manager check is now `manage_channels`, and a channel
    the operator isn't in answers 404.
  - `whoami` carries `permissions`, `permissions_revision`, `may_invite` and
    `voice_allowed`. `conch.permissions_changed.v1` tells every signed-in
    human socket when the document changes, and tells one person when their
    role or brake changes.
- **Permissions reach voice, screen share and member links, with live
  effects** (#1016). `use_voice`, `share_screen` and `make_member_links` are
  now enforced, so the `personal` preset applies. This ships with #1015.
  - Withholding `use_voice` closes the voice sockets and removes everyone it
    now withholds, in a room or only watching, before `PUT /v1/permissions`
    answers. It is checked again mid-request (403 `voice_not_allowed`) and on
    every poller pass. No room is rotated.
  - Withholding `share_screen` ends the share at once, and a reclaim after
    losing the right ends the held share.
  - Withholding `make_member_links` refuses new links with
    `invite_not_allowed`, naming the permission. Suspending and resuming a
    link is audited per link.
  - Guests hold no `use_voice` and lose voice. Guests are off in this
    release, so nobody is affected.
- **Set one person's voice permission** (#984).
  `PUT /v1/principals/{id}/voice-permission` (`{voice_allowed}`) is for
  holders of `manage_members`. It removes the person from voice before it
  answers, and a body without `voice_allowed` is refused. Each change is
  audited `voice_permission_changed`, and a repeat is not. The target gets
  `permissions_changed`.
- **A role change that withholds `use_voice` removes the person from voice
  at once** (#1131). `PUT /v1/principals/{id}/role` used to end a withheld
  share but leave the person in the room for up to about 1.5 seconds.
- **The member directory, and managers add any member** (#983).
  `GET /v1/directory/members` lists the community's members and visible
  agents, paged.
  - Everyone sees id, kind, name, role and join time. Holders of
    `manage_members` also see owner, voice and invite flags, and who invited
    them. Guests and agents are refused.
  - With `see_member_directory` a viewer sees everyone. Without it (the
    `personal` preset) a viewer sees themselves and the members of their
    channels.
  - A channel manager can add any member they may know, by id or by name,
    not only people who share a channel. Every other target answers the same
    404 `principal_not_found`. With the directory off, a manager adds only
    channel-mates, so adding can't be used to find out who exists.
- **`conch admin permissions`, and permissions in `whoami`** (#1022).
  `show` prints the matrix, warnings, the preset and which roles you may
  change. `set <permission> --members on|off --operators on|off` checks the
  cell locally before sending anything. `preset personal|group` shows the
  difference and asks first (`--yes` skips the question). A stale revision
  is re-read once and reported, never retried. `conch whoami` prints your
  permissions. A refusal that names a permission now prints it. The TUI part
  is #1148.
- **Desktop app: saved server profiles** (#945). Each server is a profile
  bound to one account on first sign-in. Another account on a bound profile
  is refused, and a second account on the same server is a second profile.
  Switching tears the old profile down (sockets, requests, window and
  partition) and starts the next in a fresh partition, so nothing from
  server A reaches B. Only a member's sign-in is kept. Sign-out and Remove
  forget, delete and revoke it. The profile file and data folder are private
  to you (`0600` and `0700`), and a corrupt file is set aside before
  anything is written. `setServerAddress` is gone, so a page can't re-point a
  profile.
- **Desktop app: sign in only to a listed server release** (#946). Before
  any saved credential is read, the app asks the server's `/healthz` and
  checks the version against `desktop/build/compat.json`, an exact list. An
  unlisted version, or no answer, means no automatic sign-in, and the saved
  credential is left alone for a server the app does support. The screen
  shows both versions with "Check again". A reply that fails validation
  after sign-in is reported as incompatible, not as a network error.
  `--allow-unlisted-server` works only in an unpackaged development build,
  with a visible warning.
- **Desktop app: links, attachments and the clipboard go through the shell**
  (#947). A link opens in your browser only after your own click, tap, Enter
  or Space, and Enter asks first ("Open this link in your browser?", Cancel
  is the default). Links are `http`/`https` only, with no user name, no
  lookalike host and at most 2048 characters, and at most 10 a minute.
  Uploads and downloads use the shell's own client with size limits, and
  Save goes through the native dialog and refuses if the account changes
  meanwhile. Clipboard writes work within 1.5 seconds of your input, and
  reads never do.
- **Web: problem reports for operators** (#1029). Settings has a Reports
  section: the list (newest first), one report with what they did, expected
  and saw, and a closed "Technical details" for diagnostics. Mark filed
  takes an `http(s)` address of 1 to 512 bytes and shows it as a link only
  when the message log would. Only operators see it. There is no wire
  change.
- **Web: a typed `:name:` becomes its emoji** (#1081). Typing the closing
  colon of a known name turns `:tada:` into the emoji. Only typing does it,
  never a paste, a drop, a restored draft or IME composition, and times like
  `12:30:45` and colons inside links are left alone. One Backspace gives the
  text back.
- **Web: a half-typed reply survives a reload** (#1080). It is kept by
  thread root, text only, and forgotten when sent, emptied or closed. It
  always goes to its root's audience, so a kept reply never reaches more
  people.
- **Community wire shapes** (#978, groundwork for the entries above). The
  schema gains channel visibility, the v2 invite family (`member` only), the
  channel and member directories, and the voice permission. The v1 invite
  shapes are byte-identical, and the bump was signed off in ADR-013.

### Changed

- **Web: member actions are in a ⋯ menu on each row** (#1094). People and
  agents get one menu in place of inline buttons: Remove from channel, and
  for an operator on a person, Make manager and Let decide approvals. It
  opens by button, Enter or Space, right-click or long press. A row with
  nothing to offer has no ⋯.
- **Web: channel actions are in a ⋯ menu on each row** (#1135). Invite
  people (for anyone who may invite) opens the invite panel with that
  channel ticked. Rename channel… (for an operator or the channel's manager,
  never a visit) opens the rename form under that row. The rename form that
  sat under the channel list is gone, and a row with nothing to offer has no
  ⋯. It opens the same four ways as the member menu.
- **Web: removing someone from a channel, and pausing invites, ask first**
  (#1083). "Remove from channel" asks "Remove <name> from #<channel>?" with
  focus on Cancel. Ticking "Pause all invites" says no one can make, preview
  or use an invite, links already sent included, until you switch it back.
  Un-pausing doesn't ask.
- **Web: the join form says "name"** (#1082). A refused name on the join,
  invite or guest forms now shows the page's own line for the rule it broke
  and puts the cursor in the name field. "schema: principal name …" no
  longer reaches the person.
- **Web: Edit stops being offered after ten known edits or a final refusal**
  (#1031). A 429 at the limit shows the limit line, not "try again".

### Fixed

- **conch-voice never drops the final state or the closing line** (#1144).
  When the output queue was full, the last managed state and the `Stopped`
  and `Closed` lines could be dropped, so a supervisor or the TUI missed the
  helper's last word. Last lines now have places of their own, and exit is
  still time-bounded.

### Security

- **Voice fails closed after a screen-share grant, and a revocation waits
  only for claims running at the time** (#1089). If the check that the room
  is still live fails after a grant, the share is ended and withdrawn and
  the claim answers 409 `voice_no_session`, where before it answered 200
  with the screen granted. A claim that queues after a revocation starts
  can't lengthen its wait, and is refused.
- **A held screen share is withdrawn when its room is rotated, even if
  LiveKit's DeleteRoom fails** (#1087). Before, a failed DeleteRoom left the
  grant in place until the sweep, up to 30 seconds.

### Docs

- **Mobile design** (#1107, #1106, #1108). A phone interaction spec with an
  accessible flow mockup, a mobile app architecture proposal (ADR-016,
  Proposed, so not a decision yet) with a capability matrix, and a design
  for push delivery and notification privacy. None of it is built.
- **Dependency audit.** RUSTSEC-2026-0429 (zip 0.6.6) is ignored in
  `voice/deny.toml` for the same reason as the three earlier zip advisories:
  the extraction path never runs, because `LK_CUSTOM_WEBRTC` is always set
  (#1167, dropped with #481).
- **Test fixes.** Several flaky tests are fixed, with no change to behaviour
  (#1098, #1104, #958, #1136).

## [0.1.0-alpha.9] - 2026-11-01

These are the core-flow fixes from browser UX audit 3 (first run, the
empty channel, attachments, the phone top bar, a faster first load),
@mentions, reply context and links in messages, and a screen-share
revocation fix. Upgrade with
`conch-server upgrade v0.1.0-alpha.9` from your workstation, or with
`conch-upgrade.sh` on the droplet (docs/deploy.md step 11).

There is no migration and no wire change since `alpha.8`, so rolling back to
`alpha.8` is a plain downgrade.

### Added

- **Attach any file the server accepts** (#1035). The composer's Attach
  button (it was "Attach images") takes any file, by the picker, a paste or
  a drop.
  - A PNG or JPEG still goes as an image, with a preview.
  - Any other file shows in the log as a chip: its name, its size and
    Download.
  - A file that is too large gets one line naming it and the server's limit.
  - Kept files follow the same rule as kept images: a draft that comes back
    is never sent to more people than you chose.
- **A first-run screen for an operator in no channel** (#1032). Instead of
  an empty centre and an endless "Connecting…", it says "You're not in any
  channels yet" and offers three things:
  - **Make a channel**, which opens the form with the cursor in its field;
  - **Invite someone**, which needs a channel first;
  - **Decide how people join**, which opens Settings at Joining.

  On a phone it shows in the Channels view. The members column says "No
  channel open".
- **A loading screen, and a smaller first download** (#1037). conchd gzips
  the web client's own files (HTML, JavaScript, CSS) and nothing else, so
  no API, socket or attachment answer is compressed.
- **@mention completion, and a mark on messages that name you** (#1043).
  Typing `@` and a few letters offers up to 8 of the channel's members
  (never yourself), worked with the arrow keys, Enter, Tab and Escape.
  Enter or Tab inserts `@name `. A message that names you gets a mark at
  its edge, using the same rule that decides a mention notification.
- **A reply shows what it answers** (#1038). Above a reply in the log is
  "Replying to <author>: <the first line of what it answers>", or "Reply
  in a thread" when that message isn't loaded. A message shows "1 reply"
  or "N replies" for the replies loaded in the log, which opens the
  thread. The thread form says "Reply to <author>" and fits a phone
  screen, and Reply and Open thread are at least 44 px square.
- **Web addresses in messages are links** (#1044). Only `http` and
  `https` addresses become links, and they open in a new tab. To make
  lookalike links harder, some addresses stay plain text:
  - an address with a user name or password in it;
  - an address whose site name has letters outside plain ASCII, or is
    written percent-encoded or in its `xn--` form.

  An invisible character never becomes part of a link: inside an address
  it leaves the address as text, and a few end the address where they
  stand.

### Changed

- **An empty conversation no longer claims the channel is empty** (#1034).
  It says "No messages here that you can see yet." and invites a first
  message. Only someone whose history starts when they joined (open
  joining) is also told "Messages sent before you joined aren't shown."

### Fixed

- **On a phone, the connection state stays readable** (#1030).
  "Reconnecting" gets its own row under the top bar's buttons, and nothing
  covers it, down to 320 px wide.
- **Kept images follow their channel across a re-sign-in** (#1067). They
  were kept by channel name, so an image could come back in a different
  channel that had taken the old name while you were signed out.
- **Revoking someone's access waits for a screen share still being
  granted** (#1061). This covers removal from the channel, disabling, and
  revoking a credential or all of them. If one of these lands while that
  person's screen share is being granted, the server answers only after
  the grant has finished and it has asked LiveKit to take it back. Before,
  such a grant could outlive the revocation.
  - If LiveKit fails both to delete the old room and to take the grant back,
    the grant can still remain until the sweep. The follow-ups are #1087 and
    #1089.
  - The same stop path is ready for the permissions page that is coming in
    #1015. This release has no route to change permissions.

### Diagnostics

- **A trace for push-to-talk** (#1041). "Transmitting" lingering after the
  first press couldn't be reproduced: in 60 tries it cleared within 1.7 s.
  To catch it on a live server:
  - in the browser console, run
    `sessionStorage.setItem('conch.web.voice.trace', '1')`;
  - reload the page;
  - after a press that lingers, read `window.conchVoiceTrace`.

  The trace holds no room, token or person. conchd logs the matching steps at
  debug level. #1041 stays open.
- Browser UX audit 4, on `alpha.8`, is in `docs/reviews/browser-ux-audit-4.md`
  (#1078).

## [0.1.0-alpha.8] - 2026-10-31

These are fixes for the biggest problems found by browser UX audit 3
(`docs/reviews/browser-ux-audit-3.md`), drafts that survive a reload, and
the groundwork for admin-configurable permissions. Upgrade with
`conch-server upgrade v0.1.0-alpha.8` from your workstation, or with
`conch-upgrade.sh` on the droplet (docs/deploy.md step 11).

Migration 39 adds the `role_permissions` table (ADR-014). It starts empty,
so every permission reads as its default and nothing anyone can do changes.
A rollback to `alpha.7` restores the pre-upgrade backup: what was written
since the upgrade is lost, and everyone signs in again (docs/deploy.md,
"Rollback with a migration").

### Added

- **Unsent drafts survive a reload** (#1039). What you were typing, who it
  was going to (a whisper's recipients or a net) and a message you were
  editing all come back after a reload or after signing back in.
  - A draft never comes back addressed to more people than you chose. Until
    its recipients are checked it can't be sent, and if they are no longer
    available it stays unsent and says so.
  - Pressing Escape never widens a draft that came back. Only choosing an
    audience yourself does.
  - Drafts are kept for the browser tab only, and signing out clears them
    (except when the network caused it).
- **Emoji shortcodes find common names** (#1036). `:thumbsup:`, `:+1:`,
  `:heart:` and other everyday aliases work, and a search of several words
  ranks the closest match first.

- **Your channel list follows changes live** (#1027). When someone adds you
  to a channel, removes you, or renames a channel you are in, your list
  changes without a reload.
  - This works through a new events-socket document,
    `conch.channels_changed.v1`. It carries no channel names, and only the
    person concerned (or a renamed channel's members) receives it.
  - A page with no channels yet checks again when you come back to it, and
    every 30 seconds.

### Fixed

- **Changing your password no longer signs that page out** (#1000). A
  request still carrying the old sign-in could answer "not signed in" and
  end the session. Now such a request is sent again with the new sign-in,
  for the same person only. A real sign-out still signs you out.
- **On a phone, the voice strip is one row and never covers the message
  box** (#1028). Under Chat and Channels it shows four controls:
  - where you are (tap to open Voice);
  - Hold to talk;
  - Mute;
  - Leave.

  Everything else is on the Voice tab. At 320 px wide, the message list
  grows from 96 px to 272 px.
- **Signing in with a password and code counts as recent proof** (#1033).
  For ten minutes after such a sign-in, an operator is not asked to prove
  it again before a sensitive change.
- **A second whisper to the same people stays a whisper** (#1066). If you
  started a new whisper, or added an image to one, while your previous
  whisper to the same people was still being sent, the composer could fall
  back to the whole channel when the first was accepted, and the next Send
  went to everyone. A finished send now never changes the audience of a
  newer draft.

### Changed

- **Operator powers now come from a permissions table, not the role**
  (#1013, #1014; ADR-014). Today every operator still holds every operator
  permission, so nothing anyone can do changes. Once the admin console
  lands (#1015), an owner can turn individual permissions on or off for
  operators and members. Refusals by a store check now also guard renaming
  a person and revoking their sign-ins.

## [0.1.0-alpha.7] - 2026-10-30

Upgrade with `conch-server upgrade v0.1.0-alpha.7` from your workstation, or
with `conch-upgrade.sh` on the droplet (docs/deploy.md step 11). Migration 38
adds message editing (the `edited_at` column and the revisions table), so a
rollback to `alpha.6` restores the pre-upgrade backup: what was written since
the upgrade is lost, and everyone signs in again (docs/deploy.md, "Rollback
with a migration").

### Added

- **Edit your own message** (#845). Each of your own text messages has a ⋯
  menu (also reached by right-click, a long press, or the keyboard) with
  **Edit**.
  - **Limits:** an edit is allowed for an hour after you send it, up to ten
    times.
  - **Keys:** Enter saves, Shift+Enter adds a line, and Esc cancels.
  - **What others see:** the new text arrives live with an **(edited)**
    marker. Only you can open **Earlier versions**.
  - **Search** finds the new text only.
  - **Nothing else changes:** an edit sends no notification, and the
    message's attachments, audience, reactions and replies stay as they
    were. Approval requests can't be edited.
  - **API:** `PUT /v2/channels/{channel}/messages/{message_id}/body` and
    `GET …/revisions`. Agents can't edit. Owners see a `message_edited` row
    in the audit log, with no text.

### Changed

- **No minimum password length** (#975, #977). A password can be as short
  as you like, but not empty, and at most 128 characters. Every way of
  setting one accepts this: sign-up, invite, change, reset and operator
  setup. The browser and the CLI no longer ask for 15 characters.
  - **Still refused:** passwords that are only spaces, that repeat a short
    pattern (`aaaa`, `abab`), or that contain your name.
  - **Note:** common short passwords such as `password` are accepted.
    Sign-in rate limits (five failures per name per ten minutes) are the
    protection.
- **Guests are turned off for now** (#1005). No route starts a guest visit
  (`403 guests_disabled`). The settings and invites can no longer admit
  guests (`409 join_policy_conflict`). The web client no longer offers guest
  entry. A server upgraded with guests on turns them off at start, audited as
  `system`; a bare address open to both is then open to accounts only. Visits
  already live can still end or become accounts. No guest data is deleted.

## [0.1.0-alpha.6] - 2026-10-30

Upgrade with `conch-server upgrade v0.1.0-alpha.6` from your workstation, or
with `conch-upgrade.sh` on the droplet (docs/deploy.md step 11). Migration 37
adds the reactions table, so a rollback to `alpha.5` restores the
pre-upgrade backup: what was written since the upgrade is lost, and everyone
signs in again (docs/deploy.md, "Rollback with a migration").

### Added

- **The phone layout: Chat, Channels and Voice** (#929). On a phone the web
  client has three tabs along the bottom. **Voice** is where you join and leave
  voice, talk, mute and see who is in the room. A thin bar on the other tabs
  shows you are still connected. Before this release a phone had no way to
  join voice.
- **An emoji picker, and `:shortcode:` completion in the message box** (#763).
  - The picker searches by name.
  - Typing `:fire` offers matches, and Enter or Tab inserts the chosen one.
  - The catalog is Unicode 18.0, vendored and generated (#953).
- **Message reactions on the server** (#765, `docs/design/message-reactions.md`):
  a durable record of (message, person, emoji) with derived counts. REST:
  `PUT` and `DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}`
  (idempotent; answer `conch.reaction_summary.v1`), `GET .../reactions`,
  `GET .../reactions/{emoji}?after=&limit=` (who reacted), the batch
  `GET /v2/channels/{channel}/reactions?messages=1,2,3` (at most 100), and two
  moderation removals for a channel's managers and operators. Four MCP tools
  (`set_reaction`, `remove_reaction`, `read_reactions`, `list_reactors`) over the
  same code; live updates as `conch.reaction_summary.v1` frames on
  `/v1/events/ws`. Reactions follow the message's visibility (a hidden message,
  whisper, net or the part of history before a guest's bound has none to read),
  keep their rows when the reactor loses access, and are audited when they
  change. **Migration 37** adds the `message_reactions` table; as for every
  migration, going back to an earlier binary means restoring the pre-upgrade
  backup. See README.md, "Reactions".
  The browser doesn't show reactions yet (#766).
- **A release delay for push-to-talk** (#965). It's a slider under Voice
  options, from 0 to 2 seconds, kept per tab.
- **Screen-share companions on the server** (#933, #932). A person in a
  voice room can get a second token that publishes only their screen, or
  only watches. The server counts it as the same person in presence and
  occupancy. This is for the desktop app, which is not released yet.
- **Desktop app groundwork** (#940, #941, #942, #944). This is the packaged
  Electron shell, its credential vault and its REST client. It is built and
  tested in CI but not yet shipped.
- **The voice helper's Linux events socket** (`--events-pipe`, #937) and its
  attach line (#893).

### Changed

- **The end of a push-to-talk phrase is no longer cut off** (#965). The browser
  dropped audio it had captured but not yet sent, so the 100 ms release tail
  was heard as about half of that. The default is now 200 ms, which is heard
  as about 150 ms.
- **"Transmitting" now means audio is actually going out** (#965). While a
  press waits for the server's answer, the voice strip says "Waiting for the
  server".
- **Losing focus shuts the push-to-talk microphone at once** (#965), without
  the release delay. A press still waiting for the server's answer never
  opens after focus is lost.
- **Open-mic detection is faster** (#965). It reads the level every 10 ms
  instead of every 50 ms.

## [0.1.0-alpha.5] - 2026-10-29

Upgrade with `conch-upgrade.sh` (docs/deploy.md step 11). Once the droplet
has the `conch-server` scripts, `conch-server upgrade v0.1.0-alpha.5` does it
from your workstation; on a droplet installed from an earlier release, run
`conch-server sync --scripts-ref v0.1.0-alpha.5` first. There is no
migration, so a rollback to `alpha.4` keeps the data as it is.

### Added

- **`conch-server`, to run more than one Conch server on the droplet from
  your workstation** (#861): `new`, `list`, `upgrade`, `rm` and `sync`, in
  `ops/conch-server` (docs/deploy.md, "A second instance").
  - `new <name> [--temporary]` makes the server's DNS records, its own user,
    data directory, LiveKit key, env file and Caddy block on the droplet,
    starts it, and shows the first operator's setup link on your terminal
    only.
  - `rm <name>` takes a final backup, then removes the server and its DNS
    records. It asks you to type the name, and refuses a normal (not
    temporary) server without `--force-normal`.
  - `list` shows every server with its port, mode and health. `upgrade <tag>`
    upgrades them all.
- **Paused invites point an operator at the switch** (#909). When an
  operator's invite is refused because invites are paused, the invite panel
  says the switch is in Settings → Joining and guests, with a button that
  opens that section. A member still sees only "Invites are paused on this
  server for now."

### Changed

- **The managed voice helper treats a failed write to its events channel as
  the owner having gone** (#894), as it already treats the end of its input.
  It stops within the owner-loss bound instead of running on unheard. A
  write that is merely interrupted or would block doesn't count.
- **The voice helper subscribes to the tracks it plays by rule** (#906),
  ready for the desktop app's screen-share participant: never the person's
  own companion, and only microphones when the companion plays shared
  sound. With no companion, which is every session today, it plays every
  remote audio track as before, and it subscribes again after a full
  reconnect.

### Fixed

- **`conch-backup.sh` no longer treats a normal server as temporary** when
  its env file has `CONCHD_MODE=temporary` followed by another `CONCHD_MODE`
  line (#885). systemd uses the last one, so such a server was running as
  normal while its backups stayed local-only. Anything ambiguous now backs
  up as normal, and asks for an off-site destination.
- **`conch-server` reuses one SSH connection across its steps** (#927).
  The droplet's firewall refuses a sixth SSH connection within 30 seconds,
  and `new` opened one per step, so it failed partway through installing the
  scripts. The shared connection closes after 15 idle seconds, so a long
  wait for DNS can mean one more.

### Internal

- The voice helper's audio backend is chosen per platform (#892), ready for
  a Windows backend. There is no change on Linux.
- Three flaky tests were fixed (#632, #689, #920), and the CLI test build
  now explains a failure to read its version-control status (#886). The Linux
  desktop design is recorded (docs/design/linux-desktop.md, #769).

## [0.1.0-alpha.4] - 2026-10-28

Upgrade with `conch-upgrade.sh` (docs/deploy.md step 11). There is no
migration, so a rollback to `alpha.3` keeps the data as it is.

### Added

- **The Conch icon in the browser tab** (#917): the app icon's dark tile, in
  place of a blank tab.

### Changed

- **A first password set without its enrolment code** now answers 403
  `enrollment_proof_required` instead of `reauth_required` (#815). The
  browser points at the enrolment-code field, and `conch password set` asks
  for `--proof`, instead of asking a member with no password to confirm
  their password.
- **`conch-voice` refuses an empty or space-padded `audio.input` or
  `audio.output`** in `voice.toml` as a bad configuration naming the key
  (#691). The device layer trimmed such a name, so a blank one silently
  meant the default device and a padded one (`" mic "`) meant `mic`. Remove
  the padding to keep the same device; for the default, write `default` or
  remove the key.

### Fixed

- **Push-to-talk always ends when the microphone stalls** (#686). If the
  microphone stopped delivering during the release tail, the transmission
  never ended and the status kept saying "transmitting". It is now cut once
  the tail and a further 50 ms have passed with no frame.
- **The voice helper's and the TUI's control-line readers agree on two more
  cases** (#690): both refuse a `\u` escape of an unpaired surrogate as
  malformed, and both refuse an empty `error`, `reason` or `ptt_problem`.

## [0.1.0-alpha.3] - 2026-10-28

A small build so an operator can resume invites from the browser. Upgrade
with `conch-upgrade.sh` (docs/deploy.md step 11). There is no migration, so a
rollback to `alpha.2` keeps the data as it is.

### Added

- **Pause and resume invites in the browser** (#882): a "Pause all invites" switch
  in Settings → Joining, beside the guest-joining pause. It does what
  `conch admin invite pause|resume` does. Restoring a backup pauses both, on
  purpose, and until now invites could only be resumed with the CLI.

### Fixed

- After a cancelled re-authentication, Settings → Joining no longer says it
  needs "a code" from an operator who has no authenticator.

## [0.1.0-alpha.2] - 2026-10-28

The second alpha build. Operators can sign in with a password alone, and
short-lived test servers have their own mode. Upgrade with
`conch-upgrade.sh` (docs/deploy.md step 11). Migration 36 adds the server's
mode record; a rollback to `alpha.1` restores the pre-upgrade backup.

### Added

- **Password-only operator sign-in** (ADR-012). With
  `CONCHD_OPERATOR_TOTP=optional` in the instance's env file, an operator who
  has not enrolled an authenticator signs in, re-authenticates and changes
  their password with the password alone. An operator who enrolled one is
  always asked for its code. The default stays `required`. Switching back to
  `required` signs operators' browser sessions out and logs how many
  operators still have no authenticator.
- **Temporary servers** (ADR-011). `CONCHD_MODE=temporary` makes a
  short-lived server:
  - password-only operators, as above;
  - every user sees "Temporary server: no backups, and it may be removed";
  - backups are local only, with no off-site copy.
- **Setup without an authenticator.** Where the server allows it, the
  browser's setup page offers **Skip** after the password, and `conch setup`
  asks "Set up an authenticator now? [y/N]".
- **Setup links.** `conchd operator setup --link --origin https://<host>`
  prints a link that opens the setup page with the token filled in; nothing
  is spent until you press Continue.
- **Replies in the browser**: one-level threads, with the audience inherited
  from the message replied to.
- **A private microphone test** in the voice panel: a level meter, and your
  own voice played back only if you ask, with nothing sent anywhere.

### Changed

- The browser and `conch` ask an operator for an authenticator code at
  re-authentication and password change only when one is enrolled.

### Fixed

- **Security:** rotating a browser session
  (`POST /v1/credentials/{id}/rotate`) produced a credential that was no
  longer treated as a session, so it skipped recent re-authentication for up
  to its 12-hour expiry. A rotated session now stays a session.
- A password-only operator's browser sign-in is kept for that tab instead of
  being revoked straight after.
- The deployment runbook now matches the first real droplet run.

## [0.1.0-alpha.1] - 2026-10-27

The first tagged build: the private communications alpha (ADR-008), deployed
on one droplet for invited testers. Pre-1.0: wire and storage formats may
change between alpha builds. What testers can expect, and what does not work
yet, is in [docs/alpha-tester-guide.md](docs/alpha-tester-guide.md).

### Added

- **Browser client** at `/app/`, served by `conchd`: channels, one-level
  threads, unread badges kept live, mentions, notifications and per-channel
  mute, search in the current channel, and pasted or attached images with
  private inline previews.
- **Voice and screen share** through an optional LiveKit server: voice
  channels with mute, deafen and push-to-talk, and screen share with sound.
- **Invites and onboarding** (ADR-009): personal invites and time-limited open
  links; temporary guests; password accounts made directly or by converting
  a guest; return sign-in from a new browser; sign out everywhere.
- **Operators** sign in in the browser with a password and an authenticator
  code, re-authenticate for sensitive actions, and recover a lost account
  from the host (`conchd operator recover`).
- **Delegated administration** (ADR-010): a server owner, delegated
  operators, and channel managers who manage their own channel's members and
  name, in the browser and through `conch admin`.
- **Isolation**: each person sees only the channels they belong to; guests
  never see history from before they were admitted, and cannot decide
  approvals.
- **Agents** connect over MCP, as before, beside people in the same channels.
- **Deployment** on one droplet (`docs/deploy.md`): systemd units, Caddy,
  LiveKit with TURN/TLS, nightly encrypted off-site backups, upgrade, rollback
  and a restore drill.
- Every build says which it is. Release builds report their tag, `make build`
  reports `git describe`, and a plain `go build` reports its commit. `conchd
  version`, `conch version`, `GET /healthz`, `GET /v1/whoami` (a new optional
  `server_version` field), `conch whoami` and the web client (sign-in page and
  the lobby footer) show it.
- Tags with a pre-release suffix produce a GitHub pre-release.
