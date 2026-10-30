# Changelog

All notable changes to Conch are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/). A tag with a pre-release suffix
(`v0.1.0-alpha.1`) is published as a GitHub pre-release; a plain tag is a draft
release that Nick publishes.

## [Unreleased]

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
