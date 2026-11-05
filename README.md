# Conch

**Open-source, self-hosted communications for people and AI agents.**

Conch is being built for groups that need to talk together without everyone talking
over everyone else: gaming squads, international friend groups, and engineering teams.
The goal is dependable push-to-talk, clear control over who hears each conversation,
and AI participants with explicit identities and permissions.

The project started with an agent-native messaging and approval core. It now also has
text nets, a browser client and optional LiveKit voice and screen sharing.

> **Alpha.** The browser, Go CLI/TUI and public APIs are available.
> See the [tester guide](docs/alpha-tester-guide.md) for joining in the browser.

## Why Conch?

Large shared voice channels often force a choice between useful coordination and
small-group conversation. Conch's direction is to support both:

- **Layered conversations:** talk within a squad, crew, or working group while the
  appropriate people also participate in coordination.
- **Clear push-to-talk targets:** see which group each key reaches, independently of
  the messages or roster you are browsing.
- **Community control:** run your own instance and choose which people and agents can
  participate.
- **AI-aware communication:** agents have distinct identities, scoped access, and
  structured approval requests rather than unrestricted access to every conversation.
- **Linux-first development:** treat Linux audio and input behavior as core product
  concerns, with Windows support a product goal so mixed-platform groups can join.

A **net** is a named subset of a channel's members. Nets can overlap: a squad lead can
belong to both a squad net and a coordination net. Members may speak; monitors listen.
An in-channel **whisper** targets specific principals. Text nets and whispers work
today; simultaneous voice listening and per-net PTT are planned.

## Current status

| Area | Available today | Still to build or validate |
| --- | --- | --- |
| Messaging | Channels, typed messages, browser, CLI/TUI, REST/WebSocket APIs | |
| Identity and access | Member links, passwords, roles and permissions, public and private channels, server-enforced agent manifests | |
| Scoped conversations | Text nets and whispers, audience-aware delivery, member/monitor roles | Layered voice, independent PTT keys, ducking, stereo placement |
| Voice | Optional LiveKit control plane, session tokens, presence, transmit reporting, CLI status/watch | Native microphone/playback client and real-world PTT reliability |
| Agents | MCP messaging and approvals, capability checks, AI administrators (an agent an owner grants administration to, gated by approvals), reference auto-reply bot | Persistent run/task views, richer runtime adapters and intervention |
| Captions and translation | Not implemented as end-user features | Optional live captions, translated captions, then spoken translation |
| Clients | Browser and Go CLI/TUI | Windows native integrations, packaging and cross-platform validation |
| Screen sharing | Browser sharing and watching through optional LiveKit | Browser and network validation |

See the [quickstart](#quickstart), [text-net example](#7-nets-and-whispers-the-squad-layout),
and [voice setup](#voice-optional) for the implemented surfaces.

The [private communications alpha](docs/design/private-alpha.md) records the original
plan ([ADR-008](docs/adr/ADR-008-private-communications-alpha.md)). Work is tracked on the
[Conch project board](https://github.com/users/njdaniel/projects/14) (private for now). Found a problem or have an
idea? Use the [bug report or feedback forms](https://github.com/njdaniel/conch/issues/new/choose). Want to
work on it? Start with [CONTRIBUTING.md](CONTRIBUTING.md). Found a security problem? Report it privately, as [SECURITY.md](SECURITY.md) describes.

## One server, one community

Members start in the default channels and see their full history. Public channels
appear under **Browse channels**; members can join them themselves. Private channels
are listed only to their members in that directory. A channel manager or someone
with `manage_channels` adds people to a private channel. Reading messages and files and joining voice still need membership,
including for operators. Members can create public and private channels by default.

The member directory lists the community's members and agents. Owners can narrow
what members see through `see_member_directory`. See
[hiding members](docs/deploy.md#hiding-members-from-each-other) for its limits.

Open the member link you were sent to make an account. The bare server address is
a sign-in page. `open_signup` is off by default on a new server (an upgraded server keeps its old open-joining mode); an owner can turn it on to let
people make accounts there too. An operator needs `change_server_settings` to do so.
Member links are reusable, with an optional use limit. Without an explicit expiry,
a link lasts seven days by default. The setting is
`member_link_default_expiry_seconds`; `0` means no default expiry.

Voice is on for new members. The per-person `voice_allowed` switch can take it away;
role permissions still apply. See [the permissions and voice switches](docs/deploy.md#the-eleven-permissions).
There are **no guests for now**. Guest data is kept; guest visits return with #1004.

Before upgrading an older server, read the
[one-community upgrade notice](CHANGELOG.md#upgrade-notice-one-community-issue-979)
and [member-link changes](CHANGELOG.md#010-alpha11---2026-11-02).
For setup, use the existing [personal or group server guide](docs/deploy.md#running-a-personal-or-a-group-server).

## Client design and P3 planning

The [client experience specification](docs/design/client-experience.md) describes
navigation, voice, nets, agent views and failure states, with
[five visual references](docs/design/mockups/README.md).
[ADR-007](docs/adr/ADR-007-client-interfaces.md) records the UI stack:
Go/Bubble Tea/Lip Gloss TUI, React/TypeScript/Vite browser UI and an Electron desktop
shell sharing that UI, with native Rust voice managed by the client.

Next is [P3 local prototype planning](docs/design/p3-local-prototype.md): one local
server, multiple independently configured TUI clients, a browser and AI participants,
plus optional VM/LAN tests. These documents record the plans. Use the [quickstart](#quickstart) for installation.

## Product direction

The following describes the intended experience. It does not replace the accepted
architecture decisions or the implementation milestones in [ROADMAP.md](ROADMAP.md).
GUI scope, Windows audio/input support, session presets, and caption retention need
their corresponding design decisions before implementation. The desktop framework is
Electron ([ADR-007](docs/adr/ADR-007-client-interfaces.md)); platform validation is
still required.

### Presets for different groups

Start with a useful layout, then rename groups and change roles without constructing
whisper lists from scratch:

| Proposed preset | Example layout |
| --- | --- |
| Friends | Shared conversation with optional private team groups |
| EVE fleet | Squad nets, leadership coordination, fleet orders |
| Foxhole operation | Vehicle crews, infantry groups, logistics, operation coordination |
| Engineering | Team conversation, pair programming, code review, optional AI participants |
| Incident response | Incident lead, investigation groups, explicit priority announcements |

Presets should configure the same communication primitives. A community should not
need a different server implementation for each activity.

### Interfaces that make routing visible

A graphical client and a first-class TUI should make listening and transmission
targets obvious. Browsing a project or conversation should not silently change the
PTT destination. Priority audio should be explicit and configurable, not a privilege
automatically granted by someone's job title.

Reliable PTT requires native press/release handling while another application has
focus. Linux and Windows integrations must be tested independently; choosing a GUI
framework does not establish that global keyboard or mouse-button PTT works.

### Optional captions, translation, and AI participation

The intended caption view identifies the speaker and source net, with optional
original and translated text. Caption access must respect the source audience.
The proposed default is temporary captions, with explicit controls for retention
and clear disclosure of where audio is processed; this policy is not implemented yet.

AI participants should expose what they can hear, what tools they can use, their
current task, and supported stop or intervention controls. Listening permission and
permission to act are separate. Conch's approval records govern actions only when the
executing tool or runtime actually checks and enforces those decisions.

## Architecture at a glance

- **Core:** Go server `conchd`, Go CLI/TUI `conch`, and the optional reference adapter
  `conch-adapter` (formerly `conch-bot`).
- **Dependency-free core:** embedded SQLite in WAL mode; messaging, approvals, and
  audit do not require a separate database, broker, or media service.
- **Optional media:** LiveKit runs as a separate service. `conchd` handles access and
  voice control; LiveKit transports audio. Core messaging and approvals remain usable
  when media is unavailable.
- **Native voice direction:** the Rust workspace under `voice/` scaffolds a separate
  `conch-voice` client, initially targeting Linux/PipeWire as accepted in
  [ADR-006](docs/adr/ADR-006-rust-voice-client.md). The current executable reports its
  version; microphone, playback, and PTT commands are still to come.
- **API parity:** clients use public REST/WebSocket APIs; agents use
  [MCP](https://modelcontextprotocol.io) over the same core.
- **Deployment:** one instance serves one community or organization today. Hosted
  multi-tenancy and large-scale orchestration are not implemented.
- **Optional integrations:** ntfy notifications; optional backup tooling such as
  Litestream. See the ADRs for the integration boundaries.

## Trust and scope

Conch currently uses a **server-trust model, not end-to-end encryption**. The server
operator controls the database. Audience checks determine delivery; they cannot stop
a recipient from repeating what they received.

Audit records make actions inspectable, but they are not a cryptographic guarantee
against an operator modifying the database. Agent manifests restrict Conch access;
they do not by themselves sandbox an agent's external tools.

There is no federation or bespoke WebRTC stack. The browser and CLI/TUI are the human interfaces. The [charter](docs/adr/ADR-000-charter.md) and
[ADRs](docs/adr/) record the accepted implementation constraints; the
[roadmap](ROADMAP.md) retains the existing phase gates until explicitly amended.

## Why "Conch"?

A conch is a signal horn and a symbol of speaking rights. The name reflects the goal:
help people and agents know when to speak, whom they are addressing, and who can hear.

## Quickstart

Every command below has been run against real `bin/conchd`/`bin/conch` builds. Paths and IDs are examples — substitute your own.

### The whole stack in one command

To try the web client with voice and screen share on your own machine, `scripts/dev-stack.sh` does the steps below for you. It needs Go, Docker, and the Node in `web/.nvmrc` for the first build.

```sh
scripts/dev-stack.sh up       # build what is missing; start LiveKit (Docker) and conchd; make an operator and a channel
scripts/dev-stack.sh token    # the operator credential, to paste at http://127.0.0.1:28080/app/
scripts/dev-stack.sh invite   # a link for a second person: open it in a private window
scripts/dev-stack.sh viewers 29 --fake-sharer   # optional load: simulated participants for 10 minutes
scripts/dev-stack.sh stats    # LiveKit's and conchd's CPU and memory, and who is in voice
scripts/dev-stack.sh down     # stop both; down --wipe also deletes the state
```

- It runs the same pinned LiveKit image as `e2e/voice`, limited by default to 2 CPUs and 4 GB, the alpha droplet's starting size, so what you see compares with it. `viewers` runs `scripts/alpha-loadtest.sh` on the other CPUs.
- `conchd` and LiveKit's signalling port listen on `127.0.0.1`. LiveKit's media ports listen on every interface, as described in [Run LiveKit beside `conchd`](#run-livekit-beside-conchd).
- The state (database, operator credential, LiveKit's key) is kept in `~/.cache/conch-dev`, never in the checkout. The script stops only what it started. `--help` lists the ports, CPUs and other settings you can change through the environment.

### 1. Build

```sh
make build   # builds bin/conchd and bin/conch
make check   # fmt, vet, lint, tests, schema-compat, dependency gate — run before opening any PR
```

`make check` lints with the `golangci-lint` version in `.golangci-lint-version` (the one CI runs) and refuses any other; install it with `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(cat .golangci-lint-version)`.

`make check` also checks the Rust voice client under `voice/` ([ADR-006](docs/adr/ADR-006-rust-voice-client.md)), which is not finished and is not needed to run `conchd` or `conch`. That leg needs [rustup](https://rustup.rs), `g++`, `jq`, GLib's, PipeWire's and libclang's headers (`sudo apt install libglib2.0-dev libpipewire-0.3-dev libclang-dev pkg-config`; the last two are for the PipeWire bindings of `dmn-audio`, which `conch-voice` uses for the microphone and speakers) and three things no distribution packages at the right version, which one script fetches into `~/.cache/conch` (about 1.3 GB unpacked, from a 2.1 GB download; no root, nothing installed system-wide):

```sh
scripts/voice-toolchain.sh   # once: clang 22, cargo-deny and LiveKit's prebuilt libwebrtc, each verified against a pinned SHA-256
make rust-check              # fmt, clippy, tests, dependency gate, licences and sources; part of make check
```

Without that toolchain `make check` fails and names the script; it does not skip the Rust leg. `make build` is unaffected: it builds only the Go binaries. If you fetched the toolchain before `conch-voice` used PipeWire, run the script once more: what is linked is now a copy of libwebrtc's archive with one member left out, made beside the download and pinned by its own hash (`scripts/voice-pins.sh` says why).

`make check` also checks the browser client under `web/` ([ADR-007](docs/adr/ADR-007-client-interfaces.md)), React and TypeScript built with Vite. That leg needs exactly the Node version in `web/.nvmrc` (24.21.0, with the npm it bundles; `nvm install` in `web/` picks it up) and `jq`. Packages come only from `web/package-lock.json`, through `npm ci`; no browser is downloaded.

```sh
make web-check   # gates (direct dependencies, exact versions, licences), typecheck, lint, unit tests; part of make check
make web-build   # builds web/dist, for conchd serve --web-dir web/dist (served at /app/)
make build-web-embedded   # web-build, then bin/conchd with the client inside it (-tags conchweb)
```

Without that Node `make check` fails and names the version to install; it does not skip the web leg. `make build` is unaffected, and `conchd` never needs Node: it serves the built `web/dist` as plain files.

### 2. Create the operator and start `conchd`

`conchd` requires authentication by default, so the first step is an operator — the one principal who can administer the instance:

```sh
umask 077   # everything created in this shell — the data directory and the token file — is readable only by you
mkdir -p /tmp/conch-data
bin/conchd bootstrap-operator --data /tmp/conch-data --name owner > /tmp/conch-data/operator.token
bin/conchd serve --data /tmp/conch-data --listen 127.0.0.1:8080
```

- `bootstrap-operator` works offline on the data directory. It prints the operator's token once, alone on stdout, and refuses to run again once an operator exists. The token can administer the whole instance: the `umask 077` above is what keeps the file it is captured to private, so do not skip it, and never write a token to a file other users can read.
- `--data` (or `CONCHD_DATA`) is required — directory for the embedded SQLite database.
- `--listen` (or `CONCHD_LISTEN`) defaults to `:8080`, which is every interface; the example binds to localhost.
- `--auth` (or `CONCHD_AUTH`) is `required` by default. `--auth off` opens every endpoint to anyone who can reach the port and trusts request bodies for identity; it is for local development only, and `conchd` says so at startup.
- `--max-upload-bytes` (or `CONCHD_MAX_UPLOAD_BYTES`) is the largest file an upload may carry (default 10 MiB; `0` disables uploads). See [Files](#files).
- `--web-dir` (or `CONCHD_WEB_DIR`) serves a built web client (a directory holding `index.html`) at `/app/`, on the API's own origin, and redirects `/` to `/app/`. A path under `/app/` that names no file returns `index.html` (client-side routing); a missing path ending in a file extension is a `404`. There are no directory listings, only `GET` and `HEAD`, nothing outside the directory is reachable, and a file or directory whose name starts with a dot is never served. `/app/` needs no credential (the page must load before sign-in); the API it calls still does. Every `/app/` response carries a strict Content-Security-Policy. Unset, `conchd` serves the web client built into the binary, if there is one, and otherwise there is no `/app/`. `conchd` refuses to start if the directory is missing or has no `index.html`. Whatever is in the directory runs as code on `conchd`'s own origin, so only the operator who runs `conchd` should be able to write to it.
- Release binaries have the web client built in, so `/app/` works with no Node and no `--web-dir`; `conchd version` prints `web: embedded` or `web: none`, and the start-up output names the source (`conchd web: embedded`, `dir <path>` or `none`). A plain `go build` or `make build` has none (it needs no Node); `make build-web-embedded` builds one that has, and fails to compile if `internal/web/dist` is empty. `--web-dir` wins over the built-in client. `--no-web` (or `CONCHD_NO_WEB` set to a boolean such as `1` or `true`; any other value is refused at start) turns `/app/` off even when a client is built in, and `/` is then not redirected; it is refused together with `--web-dir`.
- **Signing in at `/app/`.** A member signs in with a name and password, or an issued credential. The page keeps it in the browser's local storage for that origin, so closing the tab, or the whole browser, no longer signs a member out, and every tab of the origin is signed in with it. How long that lasts is the credential's: one from an invite, or issued with `--renew-days N`, expires N days after it was last used (an invite's default is 90), since each day it is used its expiry moves on; a fixed `--expires-at` stays fixed. **Sign out** (in the lobby's Settings) first asks, saying how to return: with a name and password, or a new link from an operator for an account without a password (see [Signing a person back in](#signing-a-person-back-in)). It then revokes that credential on the server and signs out every tab of this browser; if the server cannot be told, the page still forgets the credential and says so, and it then works until it expires or an operator revokes it. **Sign out everywhere** asks the same way, then revokes every credential the person has, so their other browsers and clients are refused on their next request; if the server does not do it, the page stays signed in and says so. An operator's credential is the exception: after the warning it is kept for that one tab only, and closing the tab drops it. The page runs no third-party script (`script-src 'self'`), which is what makes keeping a credential in local storage acceptable: any script on the origin could read it, which is one more reason only the operator who runs `conchd` may write the web directory.
- `--allowed-origins` (or `CONCHD_ALLOWED_ORIGINS`) is a comma-separated list of extra `Origin`s the WebSocket upgrade accepts, such as `http://localhost:5173`. It exists only so a Vite dev server on another port can connect. Each entry is one exact origin (`http` or `https`, a host, an optional port); wildcards are refused at start-up, and so are empty entries. Unset, the same-origin check is unchanged.
- `--session-stale` (or `CONCHD_SESSION_STALE`, default `30s`) and `--session-offline` (or `CONCHD_SESSION_OFFLINE`, default `90s`) are how long a session may go without a heartbeat before it shows as `stale` and then as `offline`; `conchd` refuses to start unless stale is shorter than offline. A session is one live client or runtime of a principal (a TUI, a browser tab, an adapter): `POST /v1/sessions` creates one (any caller with a credential; an agent needs `sessions.manage`; at most 16 online or stale per credential, then `429 too_many_sessions`; offline ones do not count, and reviving one is held to the same bound), `POST /v1/sessions/{id}/heartbeat` keeps it alive and revives a stale or offline one, `DELETE /v1/sessions/{id}` ends it, and `GET /v1/sessions?channel=<name>` lists the sessions of the channel's members, for members only (a non-member, an operator who is not a member and an unknown channel all get the same `404 channel_not_found`). A session belongs to the credential that created it: anyone else, another credential of the same principal and operators included, gets `404 session_not_found`. Revoking, rotating or revoking all of a credential, or disabling a principal, ends its sessions, and so does a credential's expiry (at the next sweep, with cause `credential_expired`). Rows are never deleted. Sessions need authentication: with `--auth off` the routes answer `400 sessions_require_auth`. A roster shows `runtime` and `host` only to an operator and to the session's own principal.

  A run is one piece of work an agent session reports, in one channel and optionally answering one message there. `POST /v1/sessions/{id}/runs` with `{"channel":"<name>","message_id":<id>,"task":"<one line>","link":"https://…"}` starts one in state `queued` (agents only, a human gets `403 agent_required`; on a session of the caller's own credential, otherwise `404 session_not_found`). The agent needs `runs.manage` and, in the channel, the permission posting to the run's audience needs: `post` for a run that answers nothing or a channel-wide message, `post_net` for a net message, `whisper` for a whisper (and `whisper_agent` when another agent is in it). The message must be in that channel and visible to the agent. Every one of those refusals is the same `404 channel_not_found` an unknown channel gets. `POST /v1/runs/{id}/state` with `{"state":…,"outcome":…}` moves it along the fixed lifecycle (`409 invalid_transition` otherwise; a terminal state sets `ended_at`), and `POST /v1/runs/{id}/activity` with `{"kind":…,"summary":…,"outcome":…,"data":…}` adds an entry (`409` once the run has ended). Only the credential that owns the run's session may do either; anyone else, whatever they hold, gets `404 run_not_found`. Each report re-checks the manifest and spends `runs.manage`. Every agent is held to three bounds, whatever its manifest says: a session may hold at most 32 runs that have not ended (the next start is `429 too_many_runs`; ending a run frees a slot, though a run the agent can no longer see, after losing the channel or the net, cannot be ended and keeps its slot until the session ends, so start a new session), a credential at most 512 across all its sessions, live, offline or ended (the 513th is the same `429 too_many_runs`, with a message that says the credential is full), and a run at most 10,000 activity entries (the next is `429 too_much_activity`). Both answers are the same over MCP, and neither is audited per request. A session that has gone `offline` takes no new run, no activity and no state change other than to a terminal state (`409 session_offline`): the agent heartbeats first, which brings it back if the credential is under its 16 live sessions (offline ones are not counted), so reports land only on a credential's at most 16 live sessions. A session that has ended answers the same. Ending a run on an offline session stays allowed. The runs of an offline session stay open and still count against the 512, because a heartbeat can bring the session back. An agent that crashed and restarted recovers by calling `DELETE /v1/sessions/{id}` on its old sessions: their ids are in `GET /v1/sessions?channel=` while they were seen in the last 24 hours, and in the `session_id` of their runs in `GET /v1/runs?channel=` after that; when neither shows them (a run the agent can no longer see), an operator rotating or revoking the credential ends them all. ending a session (by the client, or because its credential was revoked or expired or its principal disabled) fails its open runs in the same transaction, with outcome `session ended` and a `run_ended` audit row each, which frees their slots. `GET /v1/runs/{id}`, `GET /v1/runs/{id}/activity?after=&limit=` and `GET /v1/runs?channel=<name>&session=&after=&limit=` read them. The run list is ascending by id by default (`order=asc`, paged with `after`); `order=desc` lists the newest first, from the newest run with no `before` and below `before=<id>` otherwise, so a client pages back by passing the smallest id it holds (a newest-first page has no `next_after`). `after` goes only with `asc` and `before` only with `desc`; any other mix, both together included, is `400 invalid_request`, as is an `order` that is neither, and an empty `after=`, `before=` or `order=` is as if absent. Either way a run you may not see neither fills a page nor moves the cursor. A run that answers a message is visible to exactly the people who can see that message, and one that answers nothing to the channel's members; its activity goes with it. Operators get no exemption, and a run you may not see answers `404 run_not_found` like one that does not exist. The roster shows every agent session's `status` (`running`, `blocked`, `waiting`, `failed` or `idle`, from its runs) to all its readers, and its `current_run` and `task` only to those who may see that run; `outcome_unknown` is true while a session with a run open is `stale` or `offline`. Nothing changes a run because its session went quiet (stale or offline); a session that ends fails its open runs, as above. `run_started` and `run_ended` are audited; activity is not.

  An intervention is a person asking an agent's run to take a message or to stop. `POST /v1/runs/{id}/interventions` with `{"kind":"message","body":"…"}` (a message needs a `body`; `cancel`, `pause` and `resume` take none) records one in state `requested`. Any human who can see the run may ask (the people who could prompt the agent by posting where it listens); operators get no exemption, and an agent gets `403 human_required`. A run you may not see answers `404 run_not_found`, like one that does not exist. A run that has ended, or whose session has, answers `409 invalid_transition`; if the run's session did not declare the kind the answer is `409 intervention_unsupported` and nothing is stored (the refusal is audited as `intervention_unsupported`); a `body` or `reason` may hold newlines and tabs but no other control character; at most 16 interventions wait (`requested`) on one run, and the next is `429 too_many_interventions`. `GET /v1/runs/{id}/interventions?after=&limit=` and `GET /v1/interventions/{id}` read them, for the same people who can see the run (an agent also needs `messages.read` and `read` in the channel). The agent's adapter polls `GET /v1/sessions/{id}/interventions?state=requested&after=&limit=` (only the credential that owns the session; it needs `runs.manage` and spends it; `state` is optional and `after` is the last id seen) and answers with `POST /v1/interventions/{id}/ack` and `{"state":"acknowledged"|"applied"|"failed","reason":"…"}` (only the credential that owns the run's session, otherwise `404 intervention_not_found`; `requested` becomes any answer, `acknowledged` becomes `applied` or `failed`, anything else is `409 invalid_transition`; `failed` needs a `reason`). An intervention never changes the run: an applied `cancel` is the adapter saying it stopped the work, and the run is `cancelled` only when the adapter also reports that on `POST /v1/runs/{id}/state`. `intervention_requested` (the person, the run and the kind) and `intervention_answered` (the state) are audited, and neither holds the instruction or the reason.

  Reactions (issue #765, `docs/design/message-reactions.md`) are a durable record of (message, person, emoji); counts are always derived from the records. All routes are under `/v2/channels/{channel}/…` and need authentication (`--auth off` answers `400 reactions_require_auth`). `{emoji}` is the percent-encoded emoji (`%F0%9F%91%8D`; `#` is `%23`); the server canonicalizes any qualification of it to the fully-qualified key, and refuses anything that is not exactly one emoji of the pinned Unicode Emoji set with `400 invalid_reaction`. `PUT …/messages/{message_id}/reactions/{emoji}` sets your reaction and `DELETE` takes it back; both are idempotent and answer `200` with the message's whole `conch.reaction_summary.v1` (`reactions: [{emoji, count, me}]`, ordered by the earliest reaction held with each key, and `as_of`). `GET …/messages/{message_id}/reactions` is one summary; `GET …/reactions/{emoji}?after=<principal_id>&limit=<1..100>` lists who reacted (ids only; a guest carries `guest_reactor`) and `next_after` when there may be more; `GET /v2/channels/{channel}/reactions?messages=1,2,3` (at most 100 distinct ids) is one summary for each listed message you can see that has any, and a missing id is an empty summary. A message you cannot see (unknown, in another channel, a whisper or net you are not in, from before a guest's bound) is `404 message_not_found` on every route, byte for byte as a message that does not exist; a non-member gets `channel_not_found`. Setting needs a current *member* seat on a net message's net (`403 reaction_not_permitted` for a monitor or someone taken off the net; taking your own back is always allowed). Limits: 20 kinds per message and 10 reactions per person per message (`409 too_many_reactions`); a person's writes are limited to 60 a minute with a burst of 20, spent by every write that passes the channel gate, refusals included (`429 rate_limited` with `Retry-After` and `retry_after_seconds`); reads are not limited. An agent needs `messages.read` and `read` in the channel (a write answers the whole summary), `messages.post` and, once the message is read, the permission it would need to reply there: `post`, `post_net` or `whisper` (`403 forbidden`, audited), and spends its `messages.post` budget once per write. Reaction rows stay when the reactor leaves a channel, is disabled or hidden, ends a guest visit or becomes a member; a channel manager or operator who can see the message removes one reaction (`DELETE …/reactions/{emoji}/{principal_id}`) or all of one person's (`DELETE …/messages/{message_id}/reactors/{principal_id}`), `200` with the summary, audited as `reaction_moderated`; each removal takes a token of the moderator's own 60-a-minute write bucket and touches one message, so clearing a person's reactions across many messages is one call per message. Changes are audited as `reaction_set` and `reaction_removed` (subject `message:<id>`, detail the channel and emoji, never the body). A reaction posts no message, moves no read mark, notifies no one and reaches no agent; open `GET /v1/events/ws` sockets get a `conch.reaction_summary.v1` frame for each changed message, computed for each subscriber at send time, and nothing is sent on `/v2/ws`; after a reconnect read the batch route for the messages on screen. Agents use the MCP tools `set_reaction`, `remove_reaction`, `read_reactions` and `list_reactors`, which front the same code. Migration 37 adds the `message_reactions` table; operator diagnostics are the audit rows above. Nothing external is required.

  Read state (issue #351) is how a client shows unread counts. `GET /v1/read-state` answers `{"read_state":[…]}`: one `conch.read_state.v1` per channel the caller is a member of, with `last_read_id` (0 when the person never marked it), `unread` and, once marked, `updated_at`. `unread` counts only the messages after the mark that this person may see (channel-wide ones, their nets', whispers they are in: the same rule as the message list, so a hidden message is never counted and never moves anything) and stops at 1000, so a client shows `1000+`. `PUT /v1/channels/{channel}/read-state` with `{"last_read_id":<id>}` moves the mark and answers the new state: it only moves forward (a lower id leaves it and answers the stored state), and an id past the newest message the caller may see is taken as that message. The mark belongs to the person, not to a credential or a session, so a person's clients share it. A non-member and an unknown channel get the same `404 channel_not_found`; both routes are for people (an agent gets `403 human_required`) and need authentication (`--auth off` answers `400 read_state_require_auth`). A person's own posts are not unread to them. Marking is not audited; the events socket sends changed read-state documents after posts and shared marks, and `GET /v1/read-state` resynchronizes on channel switches and reconnects. A mark outlives membership, so leaving and rejoining a channel keeps it.
  Browser notifications (issue #840) are off by default. A member can choose mentions-only or all messages, mute individual channels without hiding unread badges, and explicitly enable native browser notifications. Preferences are local to the account, server and browser; alert content is never persisted. Denied or unsupported native permission leaves the in-app notification center available. Access loss, sign-out and account switches close native alerts and clear their content.
  `GET /v1/events/ws?channel=<name>[&session=<id>]` is how a client follows all of this live, without polling. It takes a bearer or a ticket, and sends one `pkg/schema` document per text frame, told apart by `schema`: a `SessionRosterV1` for the channel on connect and again whenever a session of a member is created, ended, changes state or status, or the channel's membership changes (including the sweeper marking a session `stale` or `offline`; a roster identical to the last one sent is not sent again); a `RunV1` when a run in the channel that you may see is created or changes state; an `AgentActivityV1` when one is appended to such a run; an `InterventionV1` when one on such a run is created or answered; a `ReadStateV1` when your unread count or shared mark changes, rebuilt from your current memberships and sent only when different; and a `MessageV2` for each newly committed message you may read, used by the browser's notification center. Every frame is built for you when it is sent, by what the REST routes use, so you are never sent a document you could not fetch now: a channel member outside a whisper gets no frame about a run that answers it, and its roster shows no `current_run` or `task` for it. The upgrade asks what `GET /v1/sessions?channel=` asks (a non-member and an unknown channel get the same `404 channel_not_found`; an agent needs `messages.read` and `read` in the channel, spent once, as for `/v2/ws`, and asked again without spending before every batch of frames), and `--auth off` answers `400 sessions_require_auth`. With `&session=<id>` the session must be your credential's and not ended, otherwise `404 session_not_found` (an agent also needs `sessions.manage`); the server then refreshes its `last_seen_at` every 10 seconds while the socket is open, as a heartbeat would, so a TUI or a browser tab stays `online` without calling one. If that session is ended while the socket is open (`DELETE /v1/sessions/{id}`, or the sweeper ending an expired credential's), the next refresh, within 10 seconds, closes the socket with 1008 `session ended`, and a reconnect with that `session` is refused `404 session_not_found`; a socket without `&session=` is not affected. Membership or credential loss closes the socket with 1008 (at once for removal from the channel, a disable, a revoke-all, a revoke or rotation of the credential it was opened with, and a new manifest; within the 30 s re-check for an expired credential); a reader too slow to keep up is closed with 1008 `subscriber too slow` and reconnects to get a fresh roster. One socket rebuilds its roster at most every 250 ms: changes in between are sent together, as the roster after them, so a `RunV1` may arrive up to 250 ms before the roster showing its status. There is no replay: after a reconnect, take the roster and read runs and read state over REST.
- `--trusted-proxies` (or `CONCHD_TRUSTED_PROXIES`) is a comma-separated list of CIDRs, such as `127.0.0.1/32,::1/128`, of reverse proxies whose `X-Forwarded-For` `conchd` believes. Default: none, and the header is never read. The client address is used for one thing only, the [invite](#invites) routes' per-address rate limits; see [the proxy section](#optional-tls-and-a-reverse-proxy).
- `--invite-allowance` (or `CONCHD_INVITE_ALLOWANCE`, default `5`) is how many open personal invites one person who may invite can hold at once. Operators have no allowance. See [Invites](#invites).
- `--ntfy-server`/`--ntfy-topic`/`--ntfy-urgent-topic` (or `CONCHD_NTFY_*`) are optional. Set both topics with the server: a new approval and its resolution are pushed to `--ntfy-topic`, an escalation to `--ntfy-urgent-topic`. With a topic left empty the notifications that use it are not sent; `conchd` warns once at start and records each in the audit log as `notify_failed`, not attempted. ntfy is a push-notification integration, not a dependency: `conchd` runs, and approvals still resolve, with no ntfy server reachable — the deployment invariant (ADR-002) requires no other external process for core function.

### 3. Create a channel, people, and an agent

The operator does this with `conch admin`, signed in by the token `bootstrap-operator` printed. `CONCH_TOKEN` carries it for this shell; the commands below use your ordinary credential and the server decides what it may do (a member who tries one is refused).

```sh
umask 077                                                    # a new shell (conchd holds the other one): the token files below must be private
export CONCH_TOKEN=$(cat /tmp/conch-data/operator.token)     # --server defaults to http://127.0.0.1:8080; set CONCH_SERVER otherwise

bin/conch admin channel create ops
# created channel ops (id 1)                  the operator, who created it, is its only member

bin/conch admin principal create --kind human alice
# created human alice (id 2)
bin/conch admin principal create --kind agent deploy-bot
# created agent deploy-bot (id 3)

# a credential for each: the token is printed alone on stdout, once
bin/conch admin credential issue 2 --label "alice laptop" > alice.token
bin/conch admin credential issue 3 --label deploy-bot > deploy-bot.token
# credential 2 does not expire                (on stderr; --expires-at <RFC 3339> sets an expiry, --renew-days <N> one that renews by use)

# channel membership decides who can see a channel at all
bin/conch admin member add ops 2
bin/conch admin member add ops 3
```

`conch admin` is a plain client of the REST API (every command names one route), so everything it does can still be done with `curl`. `--json` prints the server's response document instead of the one-line result. The token files are only as private as the `umask 077` at the top of that block makes them, and it has to be run in this shell, not only in the one that started `conchd`; never write a token anywhere other users can read.

A principal who is not a member of a channel cannot read it, post to it, subscribe to it, or see it listed; to them it does not exist. Operators manage membership but get no exemption. The other `conch admin` commands: `credential list <principal-id>`, `credential revoke <credential-id>`, `principal disable|enable <id>` (switch a principal off entirely, and back on), `principal list` and `principal hide` (who exists, and deleting accounts: see [Cleaning up test accounts](#cleaning-up-test-accounts)), `member remove <channel> <principal-id>`, and `member decide [--off] <channel> <principal-id>` (who may decide the channel's approvals; see Approvals). Rotating a credential (`POST /v1/credentials/{id}/rotate`) is REST only.

A credential issued with `--renew-days N` (REST: `renew_lifetime_seconds`, from 2 to 365 days, not together with `expires_at`) expires N days after it is issued, and a request it makes once a day has passed since its expiry was last set moves the expiry to N days after that request; so it is written at most once a day, and an expired or revoked credential is never brought back. One unused for N days expires. `credential list --json` shows it with `"renews": true`; rotating it keeps it renewable. A person who redeems an invite gets one, with the invite's credential lifetime. Anyone can sign themselves out over REST: `DELETE /v1/credentials/current` revokes the credential the request carries (audited `credential_revoked` with `reason=self`), and `POST /v1/principals/{id}/credentials/revoke-all` is open to the principal itself as well as to operators (sign out everywhere; anyone else is refused `403` and audited `access_denied`). Neither may remove the last operator's last credential (`409 last_operator`). Anyone can also see and end their own sign-ins: `GET /v1/credentials/mine` lists the caller's live credentials (id, label, when made, expiry, whether it renews, and `"current": true` on the one the request carries; never a token), and `DELETE /v1/credentials/mine/{id}` revokes one of them (audited `credential_revoked` with `reason=self`). Another principal's credential id is answered `404 credential_not_found`, exactly as one never issued, and the last operator's last credential is refused as above. A sign-in made through a sign-in link is labelled `sign-in link <id>`, so a person can see that a link was used for their account; the web client lists them under "Your sign-ins".

**Names.** A principal's name is unique on the server by how it reads, not how it is spelt: a name that differs from an existing one only by case ("Alice" and "alice"), by look-alike characters (a Latin "a" and a Cyrillic "а", a capital "I" for an "l" as in "AIice", a zero for an "o" as in "b0b", the ligature "ﬁ" and "fi"), by an invisible joiner or direction mark, or by extra or unusual spaces ("Alice  Smith", a trailing blank) is taken, and creating, bootstrapping, joining through an invite or renaming with it is refused with `409 principal_exists`. Names beginning with `hidden-` are reserved for hidden accounts (see [Cleaning up test accounts](#cleaning-up-test-accounts)), and so is every name that reads the same by the rule above (`HIDDEN-12`, `hidden-l2`, `hidden-12` with a Cyrillic `і`): creating, bootstrapping, joining or renaming with one is refused for everyone, operators included, with `409 principal_exists` and the message `names beginning with hidden- are reserved; choose another`. Such a refusal is not counted as a rename try. A name made only of characters nobody can see (a Hangul filler, say) is refused with `400 invalid_request`. The name is still stored and shown as typed. The comparison uses two keys per name, Unicode case folding and the confusable skeleton of [UTS #39](https://www.unicode.org/reports/tr39/) in both orders, and two names clash when either key matches (the data is pinned in `internal/server/names`). Two names that already clashed before this rule (an older database) are both kept, and neither can be renamed to a name that clashes with the other. `PUT /v1/principals/{id}/name` with `{"name":"…"}` renames. A person may rename themselves up to five times a day, and try up to twenty times a day: a rename refused because the name is taken counts as a try and is audited as `principal_rename_refused` (without the name), so the refusal cannot be used to search the server for names. Past either limit the answer is `429 rate_limited` with `Retry-After`. Only the person's own renames and tries count. An operator may rename anyone, with no limit, and an operator's rename of someone else counts toward nothing. An agent may not rename itself (`403 human_required`; its name is the operator's to set). Each rename is audited as `principal_renamed` with both names, and connected clients get the new name in their next roster. `conch rename <name>` renames you; `conch admin principal rename <id> <name>` renames anyone; the web client has "Change your name" in the lobby.

**Administration.** Who may do what is set by four fixed roles and twelve permissions (ADR-014, ADR-015). The roles are owner, operator and member on the server, and channel manager inside a channel; none can be added or renamed. The permissions are a closed list, each a switch for members and a switch for operators (`create_public_channels`, `create_private_channels`, `make_member_links`, `see_member_directory`, `use_voice`, `share_screen`, `use_camera`, `manage_members`, `act_as_members`, `manage_channels`, `change_server_settings`, `read_audit_log`); owners hold all of them. Two presets set the members' column in one step: **personal server** (members cannot create channels, make member links or see the directory) and **group server** (the defaults). Operators and owners manage all of it in one Settings area in the web client (Server, Roles and permissions, Members, Channels, Reports), and `conch admin permissions show|set|preset` does the same from the command line. [docs/deploy.md](docs/deploy.md#running-a-personal-or-a-group-server) walks through running a personal server and a group server, and says what an operator can still see.

**AI administrators.** An owner can give an agent some of the administrative permissions (`manage_members`, `manage_channels`, `make_member_links`, `change_server_settings`, `read_audit_log`; never `act_as_members`) so an AI client can administer the server over MCP. `conch admin ai-admin create <name> [--channel admin] [--grant <perm,...>] [--require-approval <action,...>] [--ttl 24h] [--run-window 24h] [--quorum n] [--max-pending n] [--credential-days 90] [--client json|claude-code] [--entry-name <entry>]` makes the agent, seats you as a decider in its approval channel (created private if absent), writes its manifest (`permissions` and `admin`, owner only: any other caller is refused and the new agent is disabled again), issues a renewable credential, and prints the MCP client entry on stdout and only that, so a redirect or a pipe gets a clean file; progress and advice go to stderr. The entry is named `conch-<server name>`, and `--client claude-code` prints the one-line `claude mcp add --transport http --scope user ...` instead. The token is shown once and is the agent's whole credential: keep it out of a project `.mcp.json` that people commit (use `--scope user`), and do not run the command under `set -x`, which would echo it (the command refuses when it can see an exported `SHELLOPTS` with `xtrace`; a shell that traces without exporting it is yours to turn off, as for CI). `conch admin ai-admin list` and `show <id>` read what is granted, and `revoke <id>` disables the agent, which revokes its credentials and ends its sessions; deleting the entry on the client revokes nothing. `conch admin manifest put` accepts the same `permissions` and `admin` fields for a manifest written by hand. The whole feature is in [AI administrators](#ai-administrators).

Later steps sign in as someone else, so `unset CONCH_TOKEN` when you are done administering (it overrides a stored login).

**Audit export.** An operator can read the audit trail as JSON Lines: `GET /v1/audit?after=<id>&limit=<n>` (both optional) streams one `conch.audit_event.v1` object per line, in ascending id order, `Content-Type: application/x-ndjson`, up to the last event at the moment the request started. To follow on, pass the last id you received as `after`. The trail names every principal and every refusal, so no one but an operator can read any of it, and each export is itself recorded as an `audit_exported` event (naming the caller and the range) that is not part of that export. `conch audit export [--after N] [--limit N]` writes the same lines to standard output unchanged: `conch audit export > audit.jsonl`. This is the one command that does not rewrite what the server sent: its output is for files and pipes, the server's own bytes.

### 4. Human side: `conch`

Each person signs in once with the token the operator gave them. The token is read from stdin and stored under your user config directory, readable only by you:

```sh
bin/conch login --server http://127.0.0.1:8080 < alice.token
# logged in to http://127.0.0.1:8080 as alice (member)
bin/conch whoami
```

By default the login lives in `$XDG_CONFIG_HOME/conch/credentials.json` (else `~/.config/conch/credentials.json`). `CONCH_CONFIG_DIR` moves it: set it to an absolute directory and that directory, used as it is (no `conch/` is added), holds `credentials.json` and, for `conch-voice`, `voice.toml`. A relative path is refused with one line naming the variable; a value of only white space counts as unset. `conch-voice` reads the same variable, so the two programs always agree on where the login is. To be two people on one machine, give each terminal its own directory. Two TUIs side by side, each signed in as someone else: `conch login` creates a missing directory private (0700); it never changes one that exists, and refuses one that others may read or enter.

```sh
# terminal 1
export CONCH_CONFIG_DIR=$HOME/.config/conch-alice
bin/conch login --server http://127.0.0.1:8080 < alice.token
bin/conch

# terminal 2
export CONCH_CONFIG_DIR=$HOME/.config/conch-bob
bin/conch login --server http://127.0.0.1:8080 < bob.token
bin/conch
```

Neither process reads or writes the other's file. `CONCH_TOKEN` still wins over the file in whichever directory is in use.

Without `--server`, `conch` (the TUI and every subcommand) uses `CONCH_SERVER`, else the server of the stored login when exactly one is stored (and `CONCH_TOKEN` is not set), else `http://127.0.0.1:8080`. With several logins stored it does not guess: if the default does not answer, the error names the stored servers, and `--server <url>` or `CONCH_SERVER=<url>` picks one.

Then `conch` with no arguments launches the TUI (needs a real terminal). It lists the channels you are a member of and shows who you are signed in as. Each channel shows how many messages you have not read after its name (`general 3`; nothing at 0, `999+` at the server's cap of 1000): the TUI reads the counts from `GET /v1/read-state` on start, after a reconnect and when a message arrives in another channel (at most every 2 seconds; between reads it adds one locally), and moves your read mark with `PUT /v1/channels/{channel}/read-state` to the newest message on screen when you open a channel and as messages arrive while you are at the bottom of it, at most once every 2 seconds per channel. Scrolled up, the mark does not move; it moves when the newest message comes back into view (you scroll down to it, or return to the channels view). The mark is yours, shared by all your clients. With `--auth off`, or signed in as an agent, there is no read state, and the TUI shows no counts and says nothing. The TUI must be the terminal's foreground job: started in the background (`conch &`, `timeout` without `--foreground`, a supervisor that does not hand over the terminal) it refuses with one line and a non-zero exit instead of being stopped silently by the kernel — use a plain subcommand there instead.

```sh
bin/conch
```

For scripting, `conch` also has plain subcommands. They act as whoever is logged in:

```sh
bin/conch send ops "deploying release 42"
bin/conch tail ops
bin/conch approvals list
bin/conch approve --reason "looks good" 1
bin/conch reject  --reason "not yet" 1
bin/conch logout
```

`conch search <channel> <words…>` finds messages in one channel (the REST route is `GET /v2/channels/{channel}/messages/search?q=<words>&before=<id>&limit=<n>`). A message matches when it contains every word, in any order, ignoring case and accents; words are whole words, never a query language (`*`, `-`, `OR`, quotes and the like are just text). Results are newest first, in the format `tail` prints, and page back with `before` (`next_before` in the response; the CLI prints the `--before` value to use on stderr). You are shown exactly what listing the channel shows you: a net message or whisper you cannot see is not found, and nothing in the response tells you one exists. No match prints nothing and exits 0. `q` is required and at most 256 bytes; `limit` is as for listing. The browser's Search button in the open channel uses the same route, pages older matches, and opens a result's surrounding history without changing the live conversation or its scroll position.

`GET /v2/channels/{channel}/messages` (and `/v1`) is ascending by id by default (`order=asc`, paged with `after`, `next_after` in the response). `order=desc` lists the newest first: with no `before` it is the newest page, and a client pages back by passing `before=<id>` the smallest id it holds, so opening a channel is one request, `?order=desc&limit=100`. A newest-first page has no `next_after`. `after` goes only with `asc` and `before` only with `desc`; any other mix, both together included, an unknown `order`, and `order=desc` with `thread`, is `400 invalid_request`, and an empty value is no value. The audience rule applies before the limit in both directions, so a message you cannot see neither fills a page nor moves the cursor. `/v0` has no `order`.

#### Threads

`conch send --reply-to <id> <channel> <text>` replies to message `<id>` (REST: `POST /v2/channels/{channel}/messages` with `"thread_root_id": <id>`). Threads are one level: a reply to a reply is refused. **A reply has its root's audience and cannot choose its own**: it reaches only people who can see the message it answers, so a reply to a whisper is a whisper to the same people (whoever of them replies; the author of the whisper sees it), a reply on a net goes to those of the root's recipients who are still on the net (someone added to the net later does not get replies to a message they cannot see, and someone taken off it gets no more of its talk through an old thread), and `--reply-to` cannot be combined with `--net` or `--to` (the API refuses `thread_root_id` together with `audience`: `400 thread_audience_conflict`). A message you cannot see, one that does not exist, one in another channel and a reply all give the same `404 thread_not_found`, byte for byte, so a reply cannot be used to find out that a scoped message exists. `GET /v2/channels/{channel}/messages?thread=<id>` (the CLI's `conch thread <channel> <id>`) returns the root and then its replies, in order, with the usual `after` and `limit`, showing you exactly what the channel listing shows you; a non-numeric or zero `thread` is a `400`. The channel listing and the v2 WebSocket still deliver replies in id order like any message, each with `thread_root_id` set, and `conch tail` marks one `[reply:<id>]`. v0 and v1 clients see a reply to a channel-wide message as an ordinary message and never see a scoped one. Threads are not yet available through MCP, and the TUI does not draw them. In the browser, Reply opens the root and its one-level replies inline; a reply offers Open thread instead of another reply, and its composer shows the inherited audience without a way to change it.

`conch voice status <channel>` shows who is connected to the channel's voice and who is transmitting, then exits; `--watch` keeps following and prints a `--- <time>` line and the new state on every change until you interrupt it. It only reads presence; it never joins voice. Each participant is one line, in principal id order: `<id> <name|-> <talking|quiet>[+camera] <joined-at>` (an RFC 3339 UTC time), for example `3 alice talking 2026-10-09T08:30:00Z`. `+camera` follows the third field of someone whose camera is on (`quiet+camera`, `talking+camera`); lines for people without a camera are as before. The API gives a member only their own name, so other participants show as `-`; a name with spaces or unusual characters is double-quoted and escaped. An empty room prints `nobody is connected to voice in <channel>` and exits 0. Voice not configured, or configured but unreachable, prints one line and exits nonzero (under `--watch`, an unreachable voice server is printed and the watch carries on).

`conch voice camera stop <channel> <person-id>` turns off one person's camera in the channel's voice room (`DELETE /v1/channels/{channel}/voice/camera?principal=<id>`). It is for an owner or an operator holding `manage_channels`; `<person-id>` is the principal id, as in `conch admin principal voice`. There is no self-stop in the CLI: the browser that turned a camera on turns it off. It prints nothing and exits 0 whether or not that person had a camera on (the route never says), and a refusal prints one line and exits nonzero. See [Camera](#camera).

`conch voice where` says which of your channels have people in voice, so you can find where to join: one line per channel, `<channel> <count>`, counting the people in the channel-wide room (a net's room is not counted). Nobody in voice anywhere prints `nobody is in voice in any of your channels` and exits 0; voice not configured or unreachable, or an account without voice, prints one line and exits nonzero. `--json` prints the server's `conch.voice_occupancy.v1` document as it is (channel ids, zero counts included). It reads `GET /v1/voice/occupancy` and never joins.

#### Files

`conch upload <channel> <path>` sends a file to a channel, posts one message to the whole channel that attaches it (`--message <text>`, by default the file's name) and prints the file id; `conch download <file-id>` writes it to its own name in the current directory, or to `-o <path>`. `download` never overwrites an existing file, and a name the server sends that could point outside the current directory is refused (use `-o` then). The REST routes are `POST /v2/channels/{channel}/files?filename=<name>` (the request body is the file; `Content-Type` is recorded as the declared media type; the answer is a `conch.file.v1`), `GET /v2/files/{id}/meta` and `GET /v2/files/{id}`. A message attaches files with the payload `conch.file_attachment.v1`, `{"file_ids":[1,2]}` (at most 10); the post is refused whole, with `invalid_file_attachment`, unless every file is in that channel, was uploaded by the poster and is not attached yet.

- **Who can fetch a file:** its uploader, and exactly the people the message it is attached to is visible to, now, by the same rule as listing and search. A file attached to a whisper or a net message is fetchable by that audience only; a member who leaves the channel can no longer fetch any file in it. Anyone else gets the same `404` as for an id that does not exist. Operators have no exemption. Files are for people: an agent is refused on every file route.
- **Downloads are never rendered:** always `Content-Type: application/octet-stream`, `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff`, whatever media type was declared. The declared type and the file name are information in the metadata; they are never part of a path on the server.
- `--max-upload-bytes` (or `CONCHD_MAX_UPLOAD_BYTES`) is the largest file, 10485760 (10 MiB) by default, enforced while the body is read. `0` turns uploads off (they answer `403 uploads_disabled`; files already stored can still be fetched).
- **Files live on the server's disk, not in the database:** `<data>/files/<first two hex of the sha256>/<sha256>`, written to a temporary file, synced and renamed before the database row is made; identical content is stored once. They are never deleted. Litestream replicates the database but **not** the `files` directory: back that up with the data directory (for example `rsync` or a filesystem snapshot), or a restored database will point at files that are gone.

#### Nets and whispers

A message goes to the whole channel, to a net (a named subset of the channel's members), or to a list of principals (a whisper). With no flag `send` posts channel-wide, so name the scope you mean:

```sh
bin/conch send --net alpha ops "alpha team: hold the deploy"   # to the net named alpha
bin/conch send --to 3,5 ops "quick question"                   # whisper to principals 3 and 5
bin/conch nets list ops                                        # name  id:role id:role ...
bin/conch nets create ops alpha
bin/conch nets add ops alpha 5            # add --monitor to listen only
bin/conch nets remove ops alpha 5
bin/conch nets archive ops alpha
```

`--net` and `--to` cannot be combined. `tail` marks scoped messages: `[net:alpha]` for a net, `[whisper:3,5,7]` for a whisper (the ids listed are everyone who can see it); channel-wide messages have no marker, and a message body that itself starts with `[` is printed as `\[` so that it cannot be mistaken for one. A body is free text and `tail` and `search` print it to your terminal, so every control character in it (escape, bell, backspace, tab, NUL, DEL, the C1 controls), every Unicode bidirectional control and the line and paragraph separators is written as a visible escape (`\x1b` for escape, a backslash, `u` and four hex digits for the others; a real backslash is doubled), never sent to the terminal; ordinary text, emoji included, is unchanged, and two different bodies never print the same line. Every other name or word the plain commands print from the server (net names, approval titles, `whoami`, error messages) is escaped the same way. Whispers are discretion, not secrecy: they are recorded in the audit log, and `send --to` says so on stderr. Net management is an operator action; anyone else gets the server's refusal.

In the TUI the prompt always says where your next message goes: `ops >` for the whole channel, `ops/alpha >` for a net. `/net alpha` sets that target, `/net` alone returns to the channel, and switching channels resets it. `/w 3,5 quick question` whispers once and leaves the target as it was. A message that really starts with `/` is sent as `//text`; any other `/word` is an error, not a message. Scoped messages start with a `[net:alpha]` or `[whisper:3,5]` badge (a whisper lists the other participants); a body that starts with `[` is shown with a leading `\`. The first whisper of a session reminds you that whispers are recorded in the audit log. If the server refuses a send, your text stays in the input and nothing else is sent: the TUI never falls back to the channel.

The TUI shows who and what is connected. While it runs it holds a session of its own (`POST /v1/sessions` with `client: "tui"`, `DELETE`d when it quits, `SIGINT` and `SIGTERM` included) and follows the events socket (`/v1/events/ws?channel=&session=`) of the channel on screen, reopened when you switch channel. The members line under the input reads `online: alice(2) bob · agents: worker-test ● observer-test ○`: people with a live session (a number after the name when one person has several), then agents with a session (`●` running, idle, blocked, waiting or failed; `◐` stale; `○` offline). Members with no session are not listed there. On a terminal of 120x30 or more the people pane on the right replaces it: every member once, with `online`, `stale` or `offline` (and `×2` for two live sessions), the agents with their status and task, and who is in the channel's voice room. `tab` cycles channels, inbox and agents, and `shift+tab` goes the other way. The Agents view lists each agent member of the channel once, with its status as the server derives it (`idle`, `running`, `blocked`, `waiting`, `failed`, `stale`, `offline`; an agent with no session is `offline`), its current task and when it was last heard from; `enter` unfolds its sessions with their runtime and model, and a stale or offline session with a run still open says `outcome unknown`. Names, tasks, runtimes and models are chosen by other people and agents, so they are printed as text with control characters dropped. When the events socket is down or refused, or the TUI's session could not be created (a server with `--auth off`), both views read `unavailable` rather than showing a roster that may be stale, messaging keeps working, and the socket is retried with the same backoff as the message stream (1 s, doubling to 30 s); a roster is never replayed, so a reconnect takes the next one as the state. When the server has ended the TUI's session (a revoke, a rotate, an expired credential) the socket is refused with `404 session_not_found`: the TUI then creates one new session and reopens the socket with it, and if that fails the views stay `unavailable` and the status line says why (for example `members: session ended; sign in again`). The message stream follows `/v2/ws?after=<last id held>`: after a drop it asks for exactly what it missed, with no reload of the channel. Below 80x24 the Agents view is one pane; below 40x10 the TUI asks for a larger terminal.

The TUI's layout and keys are described in [docs/design/tui-polish.md](docs/design/tui-polish.md). In short: from 80x24 a title bar names the server, the view (`[Chat] Inbox Agents Runs`), who you are and the state of the text connection (`● text live`, `◐ text reconnecting`); from 120x30 the people pane is added; below 80x24 the layout is compact. `F1` opens the help everywhere (also `?` where nothing is being typed, and `/help` in the chat view, for a terminal that keeps `F1` for itself as GNOME Terminal does), and `esc` closes it; while it is open no key reaches the view underneath. In the chat view `←`/`→` move the focus between the channel list and the messages, and `↑`/`↓` move whichever has it; typing always goes to the input. Long and multi-line messages wrap, their further lines marked `┊`. `pgup`/`pgdn` scroll the history, `home` goes to the oldest message held and `end` back to the newest. Scrolled up, new messages do not move what you are reading: the message pane's bottom border says `↓ 3 newer · end: latest`. The voice strip under the input shows your own voice when the TUI has joined it (`/voice join`, see [Voice in the TUI](#voice-in-the-tui)): `voice: #squad · mic on · hold the talk key to talk`, `TALKING`, `muted`, `reconnecting (connection lost), retry in 3 s`, or `voice stopped: <why>`. Otherwise it says what the channel's voice presence shows of you, and only that: `voice off · /voice join`, or `you: in #general voice from another client` when `conch-voice` or a browser has you in that channel's room. It cannot see other channels' rooms, and says nothing about you while presence is reconnecting. Then it says who is in the room of the channel on screen; browsing never changes where you speak.

In the Agents view, `↑`/`↓` also walk through an unfolded agent's sessions and each session's ten newest runs (read with one `GET /v1/runs?channel=&session=&order=desc&limit=10` per session when the agent is unfolded). `enter` on a session with a run open, or on a run, opens run detail: the run's id, state, task, link, start and end times and outcome, then its timeline (see below). `esc` goes back to the view it was opened from. `m` types a one-line message to the run (`enter` sends it, `esc` drops it; it is one line, and control characters other than tab are refused) and `x` asks `cancel this run? y/n`; a key is offered only for a kind the session's roster entry declares in `interventions`, and the rest read `(unsupported)` (`pause (unsupported)` and `resume (unsupported)` always: this client does not send them) and do nothing. A request shows as `requested…` and becomes `acknowledged`, `applied` or `failed: <reason>` only when the intervention's frame says so, and the run's state changes only on a run frame or a re-read: an applied cancel leaves the run reading `running` until the agent reports it `cancelled`. A run that has ended shows its outcome and no keys; a stale or offline session's open run says `outcome unknown` beside its state. A run you can no longer see closes the pane with one line. Every field is text written by other people or agents and is printed with control characters dropped. After the events socket reconnects the open run is read again, since nothing is replayed.

The Runs view lists the channel's runs across all agents, newest first (`r` in the Agents view; `a` goes back, `tab` moves on to the channels). One line per run: its state, the agent's name (from the members list; `#<id>` when the list lacks it), its task, when it started, then `for <duration>` for an open run (counted to now, redrawn once a second) or `ended <time>` for one that ended, and its outcome. A narrow terminal drops the outcome, then the end, then the start, never the state, the agent or the task. It reads the newest 50 runs (`GET /v1/runs?channel=&order=desc&limit=50`, no session filter) and the next 50 below the oldest held when the selection reaches the end of what is listed, and a run frame updates or inserts a line at once; after the events socket reconnects the newest page is read again and merged (only that page: an older run that changed while the socket was down keeps its old state until it is opened), and a line at the top of the pane says `live updates paused` while it is down. `f` cycles the filter `all`, `open`, `ended` (open means a state a run can still leave); the status line names it and the number listed. `enter` opens the run in run detail, the same pane the Agents view opens; `esc` there returns to the Runs view, and a run you can no longer see closes the pane with one line, as from the Agents view. No keys here type into the channel's draft.

Run detail's timeline is one stream: each activity entry reads `+12.4s  (+3s)  <kind> <summary> → <outcome>`, the offset from the run's start (`+0.0s`, `+12.4s`, `+3m02s`, `+1h02m03s`) and, in brackets, the gap from the line above when that is a second or more. A person's request to the run and the agent's answer are lines of the same stream, in time order, marked `↳`: `↳ #7 message · asked · <body>` at the time it was asked (`↳ #7 message · requested… · <body>` while no frame has answered it), and `↳ #7 acknowledged`, `↳ #7 applied` or `↳ #7 failed: <reason>` at the time it was answered. The header shows `duration: 5m00s so far` (redrawn once a second) for an open run and `duration: 4m00s` for one that ended. `↑`/`↓`/`pgup`/`pgdn` move the cursor `›` (the bottom line shown) back and forward through the timeline, in pages as before; the newest 1000 entries are kept. An entry that carries `data` is marked `▸`; `enter` on it unfolds the data under it, pretty-printed and cut at 2 KiB with `…` (while it is unfolded, `pgup`/`pgdn` scroll it; moving the cursor folds it), and `enter` again folds it. `enter` on a line without data says `this entry has no data` in the status line. `g` leaves for the channels view at the message the run answers, or says `no message` when the run answers none (`message #N is older than the history held` when it is not among the messages the TUI holds). Everything shown to whom is the server's: the TUI makes no request for this beyond the ones above.

- `CONCH_SERVER` (or `--server`) — conchd URL (default `http://127.0.0.1:8080`).
- `CONCH_TOKEN` — a token to use instead of the stored login, for scripts and CI.
- `CONCH_CHANNELS` — optional comma-separated override for which channels the TUI opens.
- `CONCH_MOUSE` — the TUI takes mouse input (click a channel or approval to select it, click a pane to give it the focus, wheel to scroll messages, click `end: latest` to go back to the newest) unless this is `0`. While it does, the terminal's own text selection needs Shift held (Shift+drag); `CONCH_MOUSE=0` gives it back. Under tmux, `set -g mouse on` is needed for events to reach the TUI. The keyboard does everything the mouse does, and a click never sends, decides or quits.
- `--author` / `CONCH_AUTHOR` are deprecated: identity comes from your login. They still work against a server running `--auth off`.

#### The live stream and reconnecting

`GET /v2/ws?channel=<name>` is the live stream: one JSON `MessageV2` per text frame, for every message posted after you connected. Add `&after=<id>` to be given the history first, gap-free: the server subscribes you, then sends every message you may see with an id greater than `<id>`, in ascending order, then carries on live. Connect with the id of the last message you have, and a message posted while you were reconnecting is not lost; `after=0` replays everything you may see, and without `after` nothing is replayed and the stream is exactly as before. The value must be a non-negative integer or the request is refused with `400 invalid_request` before any upgrade. Only `/v2/ws` has it (`/v1/ws` and `/v0/ws` cannot express an audience and ignore it).

**Deduplicate by `id`.** On one connection each message arrives once: the replay is in id order, and a message posted while it is running is sent in its place, not twice. Live messages are sent as they are broadcast, which is id order except when two are posted at the same instant. Across connections the same message can arrive again, for example when you reconnect with an older id than the newest you showed, so a client treats `id` as the identity of a message and ignores a frame whose id it already has. A replay shows you what `GET /v2/channels/{channel}/messages` shows you, so a net message or whisper outside your audience is not sent, and removal from the channel ends the stream as it does for a live one. If the subscription's buffer of new messages fills while a long replay is still being sent, the connection is closed as too slow (policy violation, reason `subscriber too slow`); reconnect with the newest id you received.

#### Open sockets per credential

A credential may hold at most **32** WebSockets open at once, counted across `/v0/ws`, `/v1/ws`, `/v2/ws`, `/v1/events/ws`, `/v1/voice/ws` and `/v1/voice/occupancy/ws` together. The next upgrade is refused before it is upgraded with `429` and `{"code":"too_many_sockets"}`; close one and the next is let in. The count is per credential, so a second credential of the same principal has its own 32, and a socket opened with a ticket counts against the credential that asked for the ticket. It is held in memory, so a restart forgets it (and every socket with it), and with `--auth off` there is no credential and no cap. The limit is a constant, not a flag.

#### Opening the stream from a browser: tickets

A browser cannot set an `Authorization` header on a WebSocket, and **the bearer token must never be put in a URL**: URLs end up in proxy logs, browser history and `Referer` headers. `conchd` never reads a bearer from a query parameter, under any flag. Instead the client asks for a short-lived, single-use **ticket** with its bearer, then opens the socket with `?ticket=`:

```sh
# with the bearer in the header, as for any other call
curl -s -X POST http://127.0.0.1:8080/v1/ws-tickets -H "Authorization: Bearer $CONCH_TOKEN"
# {"ticket":"conch_wst_...","expires_at":"2026-10-13T12:00:30.000Z"}
```

```js
const { ticket } = await (await fetch("/v1/ws-tickets", { method: "POST", headers: { Authorization: `Bearer ${token}` } })).json();
const ws = new WebSocket(`wss://conch.example/v2/ws?channel=ops&ticket=${ticket}`);
```

- A ticket is good for **30 seconds** and for **one** use. It is spent by the first request that presents it, whether or not that upgrade then succeeds; a second attempt is `401`. Ask for a new one for each connection, including each reconnect.
- It does exactly what the credential that asked for it could do at the moment it is spent, as that principal. If the credential has been revoked or has expired, or its principal disabled, in the meantime, the ticket is refused. A socket opened with a ticket is closed (policy violation, 1008) at once when the credential that asked for the ticket is revoked, like any other, and re-checked every 30 seconds for expiry.
- Only `GET /v2/ws`, `GET /v1/events/ws`, `GET /v1/voice/ws` and `GET /v1/voice/occupancy/ws` accept a ticket (`/v1/ws`, `/v0/ws` and every other route ignore the parameter and want a header), and only when the request carries no `Authorization` header: with a header, the header is used and the ticket is left unspent. Any refusal (missing, used, expired, unknown, or revoked credential) is the same `401`.
- A credential may hold at most 8 unspent tickets; the next request is `429 too_many_tickets`. Tickets live in memory, so a restart forgets them. With `--auth off` there is no credential to bind one to and the route answers `400 tickets_require_auth`.
- A ticket is a secret for its 30 seconds. `conchd` does not log request URLs, and a ticket is never logged or audited. A reverse proxy in front of it may log the URL: that is tolerable for a value that is dead after one use, and is exactly why the bearer must not be there.

### Optional: the reference adapter (`conch-adapter`)

`conch-adapter` is Conch's reference runtime adapter, a generic subprocess
adapter. It watches one channel and answers each new human message by running a
command: by default the local Claude Code CLI, `claude -p`. It also tells the
server it is there. Give it its agent's token and principal ID:

```sh
CONCH_ADAPTER_TOKEN=<the agent's token> \
CONCH_ADAPTER_PRINCIPAL_ID=3 \
CONCH_ADAPTER_CHANNEL=ops \
bin/conch-adapter
```

**Run with permissions skipped.** The default command is
`claude -p {prompt} --model {model} --dangerously-skip-permissions --output-format text`.
The Claude Code subprocess runs with permissions skipped: it can read and write
files and run commands as the user who started the adapter, on the strength of
whatever is in its prompt. That prompt holds the messages it is answering, and
any `message` intervention a person sent to the run (below). Only people who can
see a run can send it an intervention (the server's rule), and the text still
goes straight to that subprocess. Run the adapter as a user, in a directory and
on a host where that is acceptable, and give its agent a manifest for the one
channel it serves.

The subprocess does not receive the agent's Conch credentials: the adapter
removes every `CONCH_ADAPTER_*` and `CONCH_BOT_*` variable and `CONCH_TOKEN` from
its environment, and passes everything else (`HOME`, `PATH`, the runtime's own
login) unchanged. A model asked to print its environment cannot reveal the
token. On Linux the adapter also makes itself not dumpable, so a runtime running
as the same user cannot read the token from `/proc/<adapter pid>/environ`
either; the runtime itself is unaffected. A model can still read files the
adapter's user can read, such as a `conch login` credential under
`~/.config/conch`: run the adapter as a user of its own.

It skips messages already present when it starts and ignores its own replies.
It replies in kind: an answer to a net message goes to that net, an answer to
a whisper goes to the same principals, and an answer to a channel-wide message
is channel-wide. To reply on a net or to a whisper its manifest needs
`post_net` or `whisper` (plus `whisper_agent` for a whisper that includes
another agent). If the server refuses a scoped reply, the adapter logs one line
and posts nothing; it never falls back to a wider audience. It does not start
scoped conversations.

**Presence.** On start it first reads the channel, to skip what is already there, and only then registers its session, so a session that shows online will answer the next message sent; a credential that cannot read the channel fails at that first step (`seed cursor`). It registers a session (client `conch-adapter`, its
runtime, model and host, and the interventions `message` and `cancel`) and
sends a heartbeat every 10 seconds from a goroutine of its own, so a long reply
does not make it look offline. Each reply is one run, in the channel and
answering the newest message it is replying to: `starting`, `running`, then
`completed`, or `failed` with the exit status or `timeout` as the outcome (and
nothing posted). The run's activity is `command_started` (it names the
command's first word only, never the prompt), `command_finished`, and
`reply_posted`. (With the `codex` runtime, below, `command_started` and
`command_finished` come once for each command or tool call Codex reports,
instead of once for the process, and name that command's first word only.)
Sessions belong to an issued credential, so the adapter needs
one: create it with `POST /v1/principals/{id}/credentials`. A static
`--mcp-token` cannot register a session. The agent's manifest needs the
capabilities `sessions.manage` and `runs.manage` besides `messages.read` and
`messages.post`. Registering the session is part of starting: **an existing
`conch-bot` setup needs an issued credential and those two capabilities before
the adapter will start**, and it says so and exits if they are missing. If the
manifest has a rate limit on `runs.manage`, it must allow about seven calls per
reply (`start_run`, three state changes, three activity entries) plus one
`poll_interventions` every 2 seconds while a run is open.

**Interventions.** While a run is open the adapter checks every 2 seconds for
interventions. A `message` is answered `applied` and its text goes into the
prompt of the next replies to the same audience, and only that audience (the
last five are kept). A `cancel` kills the command and everything it started,
reports the run `cancelled` once they are gone, and then answers `applied`.
On Unix a `pause` stops the command and everything it started (`SIGSTOP` to its
process group), answers `applied` and reports the run `waiting` with the outcome
`paused`; a `resume` continues them (`SIGCONT`), reports the run `running` and
answers `applied`. A `pause` of a run already paused, or of one whose command has
not started, and a `resume` of a run not paused, are answered `failed`. A `cancel`
still works on a paused run. Time spent paused does not count against the reply
timeout, and a run paused, over all its pauses, for longer than `CONCH_ADAPTER_MAX_PAUSE` has its
command killed and fails with the outcome `paused longer than <duration>`. Where
there are no process groups the adapter does not declare `pause` and `resume`.
Anything else is answered `failed`. On SIGTERM or SIGINT it stops the
command, reports an open run `cancelled` with the outcome `adapter stopped`,
ends its session and releases the lock file.

**Settings.** Each `CONCH_ADAPTER_*` name falls back to the `CONCH_BOT_*` name
`conch-bot` used (`SERVER`, `TOKEN`, `PRINCIPAL_ID`, `CHANNEL`, `POLL_INTERVAL`,
`MAX_BACKOFF`, `CONTEXT_MESSAGES`, `MODEL`, `REPLY_TIMEOUT`, `LOCK_FILE`), and the
new name wins when both are set. Also:

- `CONCH_ADAPTER_CMD`: the command, split into arguments once as a shell
  would read words, and run without a shell (so `$(...)`, `;` and backticks are
  just characters). An argument that is exactly `{prompt}` or `{model}` is
  replaced by the prompt or the model name, as that one argument. If `{prompt}`
  does not appear, the prompt is written to the command's standard input.
  `CLAUDE_BIN`, if set, replaces the first word of the default command; a `CONCH_ADAPTER_CMD` you set is run as written.
- `CONCH_ADAPTER_RUNTIME` (default `claude-code`): the runtime profile (below) and the label shown for the runtime. A value that names no profile (`claude-code`, `codex`) is refused unless `CONCH_ADAPTER_CMD` is set, in which case it is only a label with plain-text output: a misspelt name never falls back to the Claude default command.
- `CONCH_ADAPTER_WORKDIR`: the directory the command runs in; default the adapter's own. With the default Codex command it is also given to Codex with `-C`.
- `CONCH_ADAPTER_HEARTBEAT` (default `10s`).
- `CONCH_ADAPTER_MAX_PAUSE` (default: the reply timeout): how long a run may be paused, all its pauses
  together, before its command is killed and the run fails.
- `CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE` (default `10`): over it, a reply is
  dropped with a log line. It slows a loop between adapters; it does not end one.
- `CONCH_ADAPTER_REPLY_TO_AGENTS=1` (default off): by default the adapter answers only
  messages written by people. A message from another agent goes into the prompt as
  context but never starts a run, and so does one from an author the adapter cannot
  tell (it learns who is a person from the channel's member list, which its
  credential may read; that is a REST call, `GET /v1/channels/{channel}/members`,
  made once per new author and counted against `messages.read`, so the adapter needs
  the REST routes as well as `/mcp`, and an author it cannot place is named once in
  its log). Set this to `1` and it answers agents too. **Warning:** two
  adapters with this set in one channel answer each other without end and spend the
  quota of both runtimes; the replies-per-minute cap only slows them.
- `CONCH_ADAPTER_FIXTURE=1`: see below.

**Runtime profiles.** `CONCH_ADAPTER_RUNTIME` picks a profile: a default
command, a default model and a reader for the command's output. `claude-code`
is the default described above. `codex` runs
`codex exec --json --model {model} --sandbox read-only --skip-git-repo-check --ephemeral --color never`,
with the prompt on standard input, and reads the JSONL events it prints: each
command or tool call becomes a `command_started` and a `command_finished` entry
(the first word of the command only, and its exit status), the last agent
message is the reply, and every other event is ignored. Lines that are not JSON
are ignored; more than 100 of them fail the run (`codex output not JSONL`).
Fixture markers are never read for this profile, whatever `CONCH_ADAPTER_FIXTURE`
says. `CONCH_ADAPTER_MODEL` is required for `codex` (the adapter will not invent
a model name) and the adapter exits with one line naming it if it is missing.
`CODEX_BIN`, if set, replaces `codex` in the default command only, as
`CLAUDE_BIN` does for Claude Code (both must be a bare program name or an absolute path); a `CONCH_ADAPTER_CMD` you set is run as
written, but is still read as the profile reads.

`ollama` is the local-model profile. It runs `ollama run --nowordwrap {model}` with the
prompt on standard input and reads the printed text as the reply, so a run
reports the process as one `command_started` and `command_finished` pair, as
`claude-code` does (and, as there, `CONCH_ADAPTER_FIXTURE=1` must never be set
with a real model). `CONCH_ADAPTER_MODEL` is
required and nothing is invented (for example `llama3.1:8b`; pull it first with
`ollama pull`). `OLLAMA_BIN`, if set, replaces `ollama` in the default command
and in the probe, under the same rule as `CLAUDE_BIN`. **The probe:** before the
adapter reads the channel or registers a session, it runs `ollama show {model}`
and waits at most 10 seconds for exit status 0. If it does not get it, the
adapter exits with status 1 and one line, and posts nothing: `ollama: not on PATH`,
`ollama: daemon not reachable` (also printed when the daemon does not answer in
10 seconds), or `ollama: model X is not pulled; run: ollama pull X`. The adapter
never switches to another runtime or model; no paid service is reached. A run
whose command later exits non-zero is `failed` with that exit status, as for the
other profiles. The probe is made only for the default command: a
`CONCH_ADAPTER_CMD` you set is run as written, unprobed. The model only writes
text; it has no tools here.

**Cancel.** A cancel kills the command's process group. If Codex's own sandbox helper starts commands in a new session or process group, that kill (and a pause's `SIGSTOP`) may not reach them. The reader also stops reporting after 256 commands in one run and fails the run (`too much activity`).

**Sandbox.** Unlike `claude -p`, Codex has a sandbox, and the default command
uses it read-only: the agent can read files and run commands that do not
write, which is what a reply agent needs. It still runs as the user who started
the adapter, with that user's Codex credentials. To widen it, write the command
yourself, for example `CONCH_ADAPTER_CMD="codex exec --json --model {model} --sandbox workspace-write ..."`;
`--dangerously-bypass-approvals-and-sandbox` is reachable only that way. What
that gives is what the default Claude command gives: anyone who can post where
the agent listens, or send its run a `message`, steers a process that can
change files and run anything as you. Run it only where that is acceptable.

Two adapters on one host need different `CONCH_ADAPTER_LOCK_FILE` values and
nothing else. `conch-bot` is the old name of the program; it still works for
one more release and prints one deprecation line to stderr. It is the same
program compiled into the `conch-bot` binary; it never runs another binary.

**Fixture mode.** For tests and demos the same binary runs a script instead of
a model: `CONCH_ADAPTER_CMD=e2e/p3-local/fixtures/echo.sh` replies with the last
line of the prompt, `slow.sh` waits first, `fail.sh` exits 3, and
`approval.sh` raises an approval first. Only with `CONCH_ADAPTER_FIXTURE=1` does
the adapter read the command's output line by line and act on two markers,
removing them from the reply: `@approval <title>` raises an approval through
`request_approval`, puts the run in `waiting` until it is decided, and goes on;
`@sleep <seconds>` waits. Without that variable these lines are ordinary reply
text and no output can raise an approval. For `approval.sh` the agent's
manifest also needs `approvals.request` and `approvals.await`. **Never set it
for a real model**:
with it on, anything the model prints that starts with `@approval ` asks a
person for a decision in the agent's name.

### 5. Agent side: MCP

Agents connect to `POST /mcp` (streamable HTTP) with `Authorization: Bearer <token>` — the agent's token from step 3. Twenty tools are registered. A request to `/mcp` may be at most 1 MiB, the same as a REST post; a larger one is refused with `413 request_too_large` and does nothing.

- `post_message` — post a message to a channel as the authenticated agent. With an `audience` (`{"kind":"net","net_id":N}` or `{"kind":"principals","principal_ids":[…]}`) it goes only to that net or those principals; the agent's manifest must grant `post_net`, or `whisper` (plus `whisper_agent` to reach another agent).
- `read_channel` — read one paginated page of messages from a channel: channel-wide ones, and scoped ones the agent is a recipient of. A message with an `audience` was not sent to everyone; reply in kind by sending the same `audience` back.
- `request_approval` — raise an approval as the authenticated agent.
- `await_decision` — block until an approval resolves (`timeout_ms`, clamped to a 60s server-side max).
- `check_decision` — read an approval's current state/resolution immediately, without blocking.

An adapter, or an agent's own runtime, reports its presence and its work with eight more (issue #267). Each is the REST route of the same purpose (below) over MCP: it takes and returns the same `pkg/schema` documents, allows and refuses the same requests with the same error codes, and spends the same budget. They need an issued credential, because a session belongs to one; a deprecated static `--mcp-token` gets `sessions_require_auth`. Two differences come from MCP itself. Arguments the tool's input schema refuses (a missing required field, an unknown one, a wrong type) are refused by the MCP library with a plain-text error, before the manifest is consulted, where REST answers `400 invalid_request`. And the library decodes and re-encodes every call's arguments, so `post_activity`'s `data` is stored with its object keys sorted and its numbers as float64 (an integer above 2^53 loses precision), where REST stores the JSON as sent.

- `register_session`, `heartbeat`, `end_session` (`sessions.manage`) — `POST /v1/sessions`, `POST /v1/sessions/{id}/heartbeat`, `DELETE /v1/sessions/{id}`.
- `start_run`, `update_run`, `post_activity` (`runs.manage`) — `POST /v1/sessions/{id}/runs`, `POST /v1/runs/{id}/state`, `POST /v1/runs/{id}/activity`.
- `poll_interventions`, `ack_intervention` (`runs.manage`) — `GET /v1/sessions/{id}/interventions` and `POST /v1/interventions/{id}/ack`.

An AI administrator ([docs/design/ai-administrators.md](docs/design/ai-administrators.md)) administers the server through three more, fronting the admin door's REST routes in the same way. They need `admin.act`, an `admin` block in the manifest naming the approval channel, and the operator permissions the manifest grants; an action the agent holds no permission for is `permission_denied`, and one naming the agent itself or its approval channel is `admin_action_not_allowed`.

- `describe_admin_actions` — `GET /v1/admin/actions`: the catalog as it applies to the agent (route, permission, `held`, `gated`) and its admin settings.
- `submit_admin_action` — `POST /v1/admin/actions`: run one catalog action (`action`, `target` `{kind, id}`, `params`, `reason`). A direct action answers `ran` with the route's `http_status` and `result`; a gated one answers `pending_approval` with an approval the server composes in the agent's approval channel, which a person decides. The agent watches it with `await_decision` or `check_decision` (it needs `approvals.await` or `approvals.check`, `read` in that channel, and a seat there).
- `run_approved_action` — `POST /v1/admin/actions/{approval_id}/run`: run the approved action, once, within the run window; refused with `approval_not_approved`, `approval_already_run`, `approval_run_window_passed` or `target_changed` otherwise.

The `admin.act` budget is spent once per submitted action; the catalog read and the run spend nothing, and a refused action spends nothing. The audit rows the door writes (`admin_action_requested`, `admin_action_ran`, `admin_action_refused`) say `via=mcp`.

**Agents are deny-by-default.** A token only says who the agent is. To do anything in a channel an agent needs both **membership** of the channel (step 3) and a **manifest** granting the capability and the permission there:

```sh
CONCH_TOKEN=$(cat /tmp/conch-data/operator.token) bin/conch admin manifest put 3 <<'EOF'   # the operator's token for this one command; --file <path> reads a file
{
  "display_name": "deploy-bot", "tier": "A",
  "capabilities": ["messages.read","messages.post","approvals.request","approvals.await","approvals.check"],
  "channels": [{"channel_id": 1, "permissions": ["read","post"]}]
}
EOF
```

Capabilities are `messages.read`, `messages.post`, `approvals.request`, `approvals.await`, `approvals.check`, `sessions.manage` (register, heartbeat and end the agent's own sessions), `runs.manage` (report runs and activity, and read and answer interventions on its own runs) and `admin.act` (use the admin door); each is granted separately. The session and run pair are P3 additions, used by the session and run tools and routes. Without membership a channel looks like it does not exist; with membership but no grant the call returns `forbidden`. Every refusal is written to the audit log (a caller repeating the same denial gets a row for each of the first eight in a minute and then only for the 16th, 32nd… with its number, not one per request for ever). A manifest may also cap a capability with `rate_limits`; over the cap the call is refused with `429` and `Retry-After` (REST, and a webhook bound to the agent) or the `rate_limited` error (MCP). The counters are in memory, so a restart starts every window afresh. Details: [docs/design/agent-manifest.md](docs/design/agent-manifest.md).

A raw JSON-RPC example (most agents will instead use an MCP client SDK):

```sh
curl -s -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Authorization: Bearer $(cat deploy-bot.token)" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
        "name":"post_message","arguments":{"channel":"ops","body":"release 42 is built"}}}'
```

Approvals are requested by agents over MCP and decided by humans with `conch approve` / `conch reject`; the decider is always the logged-in person and must be a member of the approval's channel who holds its right to decide. Every member an operator adds holds it, unless added with `conch admin member add --no-decide`. A person who joins through an invite never does. `conch admin member decide [--off] <channel> <principal-id>` grants it or takes it away. A member without it sees the channel's approvals marked view-only, and a decision is refused with `403 decision_not_permitted`.

#### AI administrators

An AI administrator is an ordinary agent (kind `agent`) to which an owner has granted some operator permissions and one place to ask: an approval channel. It administers the server for you, and anything that closes or opens something waits for a person's decision. The design is [docs/design/ai-administrators.md](docs/design/ai-administrators.md).

**Set one up.** Either command makes the agent, its manifest, its approval channel and a credential, and prints the MCP client entry once, alone on stdout (put it in the AI's client settings; do not run it under `set -x`):

```sh
# as an owner, from a machine that can reach the server
bin/conch admin ai-admin create deploy-admin > deploy-admin.mcp.json

# or on the host, offline, over SSH (works on the database directly, audited as `system`)
conchd ai-admin create --data /var/lib/conch --origin https://chat.example.org deploy-admin
```

By default it is granted `manage_members`, `manage_channels` and `make_member_links`, asks in a private channel called `admin` (made if absent; the owner who ran `conch admin ai-admin create` decides there, and `conchd ai-admin create` seats every owner who can sign in, or the principals named with `--decider`), allows each approval 24 hours to be decided and 24 hours to be run, and gets a credential that lasts 90 days and renews by use. `--grant`, `--require-approval`, `--channel`, `--ttl`, `--run-window`, `--quorum`, `--max-pending` and `--credential-days` change that; `conch admin ai-admin create --help` lists them, and `conch admin ai-admin list` and `show <id>` read back what exists. `change_server_settings` and `read_audit_log` are never granted unless you name them. The agent also holds `admin.act`, limited to 60 an hour by default; it does not hold `approvals.request`, because the server raises its approvals itself. In the web console an owner does the same from the members section, "Add an AI administrator"; the agent's requests appear in its approval channel as a card with **Run it** and **Do not run**, drawn from the server's own approvals (never from a message), and a granted agent's card offers **Remove access**.

Agents hold no permission unless an owner grants one, and only an owner can write a grant; nothing changes for existing agents.

**What it can do.** Three MCP tools (see [Agent side: MCP](#5-agent-side-mcp)): `describe_admin_actions` lists the closed catalog of actions it may take, `submit_admin_action` runs one, and `run_approved_action` runs a gated one after a person approved it. The catalog is a fixed list of named actions over existing routes (members, channels, member links, credentials and hooks, and, with the two powerful permissions, join settings, permissions and the audit log). It has no way to change a role, write a manifest, act as another member, create an agent or use the approval routes.

- **Direct actions** run at once: listing, renaming, creating channels, adding a channel member, making a bounded member link, pausing invites.
- **Gated actions** raise an ordinary approval in the agent's approval channel and answer `pending_approval`: disabling, enabling or hiding a member, revoking credentials or hooks, changing a channel's visibility, archiving it, removing a channel member or changing their rights, disabling the people a link or a member brought in, resuming invites, and changing join settings or permissions. An owner can widen this set for an agent with `--require-approval`, never narrow it. The approval names the action and its target and carries the agent's `reason`; that text is the agent's own words, so read it as that, not as the server's.
- **Deciding.** A person who may decide in that channel approves with `conch approve <id> --option run` (or rejects), with the REST decision, or in the web client. An agent can never decide an approval, its own included. Approving runs nothing: the agent then calls `run_approved_action`, once, within the run window. A run before approval, a second run, a run after the window and a run on a target that changed since the approval are each refused (`approval_not_approved`, `approval_already_run`, `approval_run_window_passed`, `target_changed`).
- **Authority is only through the door.** The grant counts when the agent submits an action through `POST /v1/admin/actions` (or the MCP tool). The same agent calling an administrative REST route directly is refused as before. It cannot change its own approval channel's roster or rights.

**Audit.** Every action leaves the rows a person's would, with the agent as actor, plus `admin_action_requested`, `admin_action_ran`, `admin_action_refused`, `admin_grant_changed` and `ai_admin_created` / `ai_admin_revoked`. A gated disable reads `approval_created`, `admin_action_requested`, `decision_cast`, `approval_resolved`, `principal_disabled`, `admin_action_ran`; `go run ./e2e/dogfood` checks exactly that chain.

**Revoke.** `conch admin ai-admin revoke <id>` (or `conchd ai-admin revoke --data <dir> <id>` on the host) disables the agent: its credentials, sessions and sockets end at once and its grant is cleared. To narrow without revoking, write a new manifest with `conch admin manifest put`. Deleting the entry from the AI's client settings revokes nothing; the credential still works until you revoke it.

#### Reporting from outside Conch: a CI job or a remote script

A runtime that cannot be run as a subprocess of `conch-adapter` (a CI job, a hosted agent, a script on another machine) can report its work with plain HTTP. The session, run, activity and intervention routes above take an issued agent credential in the `Authorization` header and are held to the agent's manifest, its rate limits and the audit log, like the MCP tools; each body is limited to 1 MiB and decoded strictly (an unknown field is `400`). No other route, listener or process is involved. The test `TestExternalRuntime*` in `internal/server/external_runtime_test.go` drives this sequence and every refusal below.

**Set-up** (the operator, once per sender). Give the sender a principal of its own and a credential that expires; a shared credential shares its 16 live sessions and its rate budget, and revoking a dedicated one ends only its sessions. Put it in one channel and grant the least:

```sh
# as the operator (CONCH_TOKEN, step 3)
bin/conch admin principal create --kind agent ci-runner          # say it is id 4
bin/conch admin member add ops 4
bin/conch admin credential issue 4 --label "nightly build" --expires-at 2026-12-31T00:00:00Z > ci.token
bin/conch admin manifest put 4 <<'EOF'
{
  "display_name": "ci-runner", "tier": "C",
  "capabilities": ["sessions.manage","runs.manage"],
  "channels": [{"channel_id": 1, "permissions": ["post"]}],
  "rate_limits": [
    {"capability": "sessions.manage", "max": 12, "window_seconds": 60},
    {"capability": "runs.manage", "max": 120, "window_seconds": 60}
  ]
}
EOF
```

A rate limit is opt-in: a capability with no entry is unlimited, so declare both. A heartbeat every 10 seconds spends 6 `sessions.manage` a minute. The sender needs `post` in the channel because a run is shown to the channel's audience, but not `messages.read` or `messages.post`: it cannot read or post messages.

**The sequence** (the sender; `$CONCH_SERVER` is the server's URL, `ci.token` the credential). In a CI job, keep the credential in a masked secret and never run this block under `set -x`, which would print the token into the job log:

```sh
AUTH="Authorization: Bearer $(cat ci.token)"
API=${CONCH_SERVER:-http://127.0.0.1:8080}

# 1. register; keep the id
SESSION=$(curl -sf -X POST "$API/v1/sessions" -H "$AUTH" \
  -d '{"client":"ci-job","runtime":"github-actions","host":"runner-3"}' | jq .id)

# 2. heartbeat in the background for the life of the job
( while sleep 10; do curl -sf -X POST "$API/v1/sessions/$SESSION/heartbeat" -H "$AUTH" >/dev/null; done ) &
HEARTBEAT=$!

# 3. start a run in the channel; keep its id
RUN=$(curl -sf -X POST "$API/v1/sessions/$SESSION/runs" -H "$AUTH" \
  -d '{"channel":"ops","task":"nightly build","link":"https://ci.example.com/jobs/7421"}' | jq .id)

# 4. report states and activity
curl -sf -X POST "$API/v1/runs/$RUN/state" -H "$AUTH" -d '{"state":"starting"}' >/dev/null
curl -sf -X POST "$API/v1/runs/$RUN/state" -H "$AUTH" -d '{"state":"running"}' >/dev/null
curl -sf -X POST "$API/v1/runs/$RUN/activity" -H "$AUTH" \
  -d '{"kind":"command_started","summary":"go test ./..."}' >/dev/null
curl -sf -X POST "$API/v1/runs/$RUN/activity" -H "$AUTH" \
  -d '{"kind":"command_finished","summary":"go test ./...","outcome":"passed","data":{"exit":0}}' >/dev/null

# 5. a terminal state ends the run (activity after that is 409, so post run_ended first)
curl -sf -X POST "$API/v1/runs/$RUN/activity" -H "$AUTH" \
  -d '{"kind":"run_ended","summary":"build finished","outcome":"passed"}' >/dev/null
curl -sf -X POST "$API/v1/runs/$RUN/state" -H "$AUTH" -d '{"state":"completed","outcome":"passed"}' >/dev/null

# 6. stop the heartbeat and end the session
kill $HEARTBEAT 2>/dev/null; wait $HEARTBEAT 2>/dev/null
curl -sf -X DELETE "$API/v1/sessions/$SESSION" -H "$AUTH"
```

A member sees the session, the run and each activity entry live on `GET /v1/events/ws`, and `session_started`, `run_started`, `run_ended` and `session_ended` are in the audit log, without the task. A sender that wants to be told to stop lists the kinds it will answer in `register` (`"interventions":["cancel"]`), polls `GET /v1/sessions/$SESSION/interventions?state=requested` and answers each with `POST /v1/interventions/{id}/ack`; a kind it did not list is refused at once with `409 intervention_unsupported`.

**What is not supported.**

- **No back-dating.** `at` is the server's clock; a sender that replays a backlog gets the time of receipt.
- **No retries without thought.** There is no idempotency key: a retried start creates a second run and a retried activity a second entry. A retried state change after a lost response gets `409 invalid_transition`, which means it is already done. Do not retry a start blindly.
- **A run report is not a heartbeat.** Only a heartbeat (or an open events socket with `session=`) keeps a session alive. A sender that reports but never heartbeats shows `stale` after 30 seconds and `offline` after 90, and its open run reads `outcome_unknown`. Heartbeat from the background, as above.
- **`--auth off` refuses sessions** (`400 sessions_require_auth`), and so does a static `--mcp-token`.

What is refused: a wrong or revoked credential is `401`. Starting a run without `runs.manage`, or in a channel the sender is not in, is `404 channel_not_found` (audited as `access_denied`, with `capability_not_granted` or `not_a_member`); a report on an existing run after `runs.manage` is gone is `403 forbidden`. A body over 1 MiB is `413`; malformed JSON, an unknown field, activity `data` over 8 KiB or an `http://` link is `400`. Over a rate limit is `429` with `Retry-After`, audited as `agent_rate_limited`.

#### When the sender cannot hold a session: the relay

A one-shot script, or a CI job made of separate short commands, has no process that lives as long as its run, so nothing keeps its session alive and nothing remembers the ids. `conch-adapter` has a second mode for that: `CONCH_ADAPTER_MODE=relay`. The relay is an optional process beside the runtime. It registers one session, heartbeats it, and takes the runtime's reports on a Unix socket, which it forwards to `conchd` over REST with the same bodies as above. It runs no command and reads no channel, so its manifest needs `sessions.manage`, `runs.manage` and `post` in its one channel, and neither `messages.read` nor `messages.post`. It is not required for anything: a sender that can run the sequence above does not need it. It does not use MCP, which re-encodes activity `data`. Design and threat model: `docs/design/webhook-adapter.md` sections 7 and 8.

```sh
export CONCH_ADAPTER_MODE=relay CONCH_ADAPTER_SERVER=$API CONCH_ADAPTER_TOKEN=$(cat ci.token) \
       CONCH_ADAPTER_CHANNEL=ops CONCH_ADAPTER_RUNTIME=github-actions
conch-adapter &        # the socket is $XDG_RUNTIME_DIR/conch-adapter-relay.sock
RELAY="curl -sf --unix-socket $XDG_RUNTIME_DIR/conch-adapter-relay.sock"

RUN=$($RELAY -X POST http://relay/v1/runs -d '{"channel":"ops","task":"nightly build"}' | jq .id)
$RELAY -X POST http://relay/v1/runs/$RUN/state -d '{"state":"running"}' >/dev/null
$RELAY -X POST http://relay/v1/runs/$RUN/activity -d '{"kind":"command_finished","summary":"go test ./...","outcome":"passed"}' >/dev/null
$RELAY -X POST http://relay/v1/runs/$RUN/state -d '{"state":"completed","outcome":"passed"}' >/dev/null
kill %1                # SIGTERM ends the session
```

- **The socket.** A Unix socket only, mode 0600, in a directory that must be mode 0700 and owned by the relay's user: anyone who can open it (that user, and root) can report as the relay's principal, and nobody else. The directories above it must not be writable by other users (the default `/run/user/UID` is fine). The relay never listens on TCP. It refuses to start without `XDG_RUNTIME_DIR` or `CONCH_ADAPTER_RELAY_SOCKET` (an absolute path of under 100 bytes), refuses a path that exists and is not a socket, and replaces the socket of a relay that died but not one that is running. Its lock file is `<socket>.lock`. In a CI job every step runs as that user, third-party actions included; they could already read the job's credential.
- **The routes.** `GET /v1/session` (the relay's own session, for a readiness check), `POST /v1/runs`, `POST /v1/runs/{id}/state`, `POST /v1/runs/{id}/activity`, `GET /v1/interventions?state=&after=&limit=` and `POST /v1/interventions/{id}/ack`. They are `conchd`'s routes with the session made implicit, and take and return the same documents. Bodies are decoded strictly at 1 MiB with the same types before anything is forwarded: over the limit is `413 request_too_large`; malformed, an unknown field or a value the type refuses is `400 invalid_request`. `conchd`'s status, error body and `Retry-After` come back unchanged (except that a run report refused `409 session_offline` is retried once after the relay heartbeats its session, and the retry's answer comes back), so a manifest without `runs.manage` or a rate limit looks the same as on the direct routes.
- **What the relay refuses itself,** without asking `conchd` (so it is not in the audit log; the relay logs the code and never a body): a run id it did not create (`404 run_not_found`; two relays on one credential cannot report on each other's runs), an intervention id it did not return from a poll (`404 intervention_not_found`) and a channel other than `CONCH_ADAPTER_CHANNEL` (`404 channel_not_found`).
- **Interventions.** `CONCH_ADAPTER_INTERVENTIONS`, a comma-separated list of kinds, default empty: the relay declares none, so a request for any is refused at once with `409 intervention_unsupported`. For the kinds it declares, the script polls and answers through the socket. A request the script has not answered within `CONCH_ADAPTER_INTERVENTION_TIMEOUT` (default `60s`) is answered `failed` by the relay, with the reason `not taken by <runtime> within 60s`, so that a person who asks gets an answer (a refusal other than 429 ends the attempts, a 429 waits for `Retry-After`, a 5xx is tried again every second); the script's late answer is then `conchd`'s `409 invalid_transition`. The relay looks for waiting requests at each heartbeat even if the script never polls, which costs one `runs.manage` call per heartbeat when kinds are declared. Answering never moves the run: after a cancel, the script reports `cancelled` itself.
- **Stopping.** On SIGTERM or SIGINT the relay ends its session. It does not invent an outcome itself, but ending a session fails its open runs with outcome `session ended`, so report the real terminal state before stopping. A relay that is killed instead leaves its session to go stale and then offline, and the roster shows `outcome_unknown` on its open run.
- **Settings.** `CONCH_ADAPTER_SERVER`, `CONCH_ADAPTER_TOKEN` and `CONCH_ADAPTER_CHANNEL` are required; `CONCH_ADAPTER_RUNTIME` (default `relay`) is a free label here, since no command runs; `CONCH_ADAPTER_MODEL` (default none), `CONCH_ADAPTER_HEARTBEAT` (default `10s`), `CONCH_ADAPTER_RELAY_SOCKET`, `CONCH_ADAPTER_LOCK_FILE`. `CONCH_ADAPTER_PRINCIPAL_ID` and the command settings are not read. Give each external runtime its own principal and credential, as above. A relay needs a `conchd` with authentication on: with `--auth off` it cannot start, because `conchd` refuses its session.
- **A relay that cannot reach `conchd`** answers `502 internal_error` and keeps heartbeating; one that cannot register at start exits with the reason.

### 6. Verify your install

```sh
go run ./e2e/dogfood
```

This drives the full loop live against freshly built binaries, authenticated end to end: it bootstraps an operator, a human signs in with `conch login`, an agent with an issued credential and a manifest posts via MCP and requests approval, ntfy fires, the human resolves it with `conch approve` and a reason, `await_decision` returns the structured outcome, and the audit log shows the whole chain. Along the way it asserts what must be refused — no credential, a decision in someone else's name, a non-member reading, posting or deciding, a second agent reaching outside its grant — then reruns the approval half with ntfy unreachable to prove it still resolves (ADR-002). Exits nonzero on any assertion failure.

#### The P2 hardening gate

```sh
go run ./e2e/p2-hardening
```

This is the wider check, and the one to run before a release. It starts from a database written by a P1 `conchd` (checked in as `e2e/p2-hardening/testdata/p1.sql`), upgrades it with `bootstrap-operator --keep-existing-credentials` and `serve`, and drives freshly built binaries through everything added since, on that upgraded database:

- **Upgrade.** Every P1 message, approval, resolution and audit row is served as P1 stored it. The approvals P1 left open are decided afterwards, one of them by a second decision added to the one P1 recorded. The webhook URL P1 handed out still works. Nothing about old approvals is re-sent.
- **Identity.** Nothing is served without a credential. Two agents hold different manifests and credentials. A rotated or revoked credential stops at once, and the principal and its history stay.
- **Capability enforcement.** What a manifest grants works; everything else is refused with a stable error and one audit row. An agent can never decide an approval. The request, notify, decide, await chain runs with ntfy answering and with ntfy down. An agent at its rate limit slows nobody else, not even an agent with the same limit.
- **What P2 gave people.** The channel list (through REST, the client, and the real TUI on a pseudo-terminal), an approval read by id, audit export, search, files, threads, and the principal-review report posted by a plain command with no scheduler.
- **Operations.** No credential the run ever held is in the audit export or in `conchd`'s log, `conchd` logs no error but the two about the ntfy server the run took down, `e2e/dogfood` passes, and the run leaves no process and no file behind.

It prints one `ok` line per step and ends with `p2-hardening-check: PASS`, or stops at the first failed step with `p2-hardening-check: FAIL: <phase>: <what>` and the end of `conchd`'s log, and exits nonzero. It needs Go, and on Linux bash. Two steps need Linux, the TUI one and the principal-review one; elsewhere each prints a `NOT RUN` line and the last line counts them, and with `CI` set that is a failure. `-skip-dogfood` leaves out the last step for a caller that runs `e2e/dogfood` itself. CI runs it on every pull request and every push to `main`, and the release workflow runs it on every tag.

#### The cross-group isolation check

```sh
go run ./e2e/isolation
```

One server holds several groups in private channels of their own, and this checks that they stay apart. It starts a fresh `conchd` with two groups: A joins through an operator's open link, B through personal invites, one of them made by a B tester. Both chat, reply in threads, whisper, post to a net, upload a file, run an agent and ask for an approval and a report; A also talks in voice and shares a screen. Then:

- **The matrix.** For every route in `internal/server/routes.go`, a B member tries A's channels, people, messages, files, sessions, runs, approvals, invites and reports by name and by id. It runs as an invited tester, as a member the operator made, and (for the agent routes) as B's agent. Each answer must be the documented refusal. No body or header may hold any name, id or piece of text of A's other than what was asked about. A's object and one that never existed must get the same answer. Naming an A member as the author, requester or decider is refused. A route with no row in the matrix fails the run, so a new route cannot skip the check.
- **MCP.** B's agent calls the channel, session, run and intervention tools and the approval reads on A's objects. Each is refused as for a ghost.
- **Sockets.** B holds every socket variant open on both its channels for the whole run. They carry B's own traffic and nothing of A's, checked once A is active and again at the end. Opening one for A's channel is refused before the upgrade.
- **Media.** B cannot get a voice session for A's channel. LiveKit refuses a B token edited to name A's room. A B participant who subscribes to everything sees nothing of A's.
- **Revocation.** An operator disables a tester who holds every socket variant, a LiveKit participant and the share. Then the operator disables everyone who joined through A's open link, and closes the link. The program measures how long each socket and participant takes to go and holds it to a bound: 2 s for a socket; for LiveKit, one voice poller pass plus a room rotation, read from the server's source. Afterwards the revoked stay out: a ticket or join grant they kept no longer works, and a poller pass later LiveKit lists them nowhere. After the link is closed, using it answers `invite_invalid`.

It ends with `isolation-check: PASS` and the measured revocation times. The media half needs Docker, the pinned LiveKit image and the pinned `lk`, the same ones `e2e/voice` uses. Without them it is skipped with one line. With `CI` set, a skip is a failure. CI runs it on every pull request and every push to `main`.

### 7. Nets and whispers: the squad layout

A message goes to the whole channel, to a **net** (a named subset of the channel's members) or, as a **whisper**, to a list of principals. Who can see it is fixed when it is posted: joining a net later shows nothing earlier, and leaving it hides nothing already received. A net's **members** listen and speak; its **monitors** only listen. Nets overlap, which is how a lead hears her squad and the command net at once. The server never delivers a scoped message to anyone outside its audience, on any read path: REST, WebSocket, MCP and the CLI.

This builds the roadmap's layout, two squads of four with their leads on a command net: a channel `squads`, a human `lead`, and agents `a1`..`a4` and `b1`..`b4`. Start from a fresh data directory (step 2) and `CONCH_TOKEN` set to the operator's token as in step 3; the ids below are the ones a fresh instance hands out (the operator is 1, the channel is 1, `lead` is 2, `a1`..`a4` are 3..6, `b1`..`b4` are 7..10).

```sh
umask 077; T=$(mktemp -d)        # a private directory for the tokens; removed at the end

bin/conch admin channel create squads
bin/conch admin principal create --kind human lead
for n in a1 a2 a3 a4 b1 b2 b3 b4; do
  bin/conch admin principal create --kind agent $n
done

# a credential (kept in a private file, never printed) and channel membership for each of the nine
for id in 2 3 4 5 6 7 8 9 10; do
  bin/conch admin credential issue $id --label squads > "$T/$id.token"
  bin/conch admin member add squads $id
done

# agents are deny-by-default: read the channel, and speak on a net they belong to
for id in 3 4 5 6 7 8 9 10; do
  bin/conch admin manifest put $id <<'EOF'
{"display_name": "squad agent", "tier": "A", "capabilities": ["messages.read","messages.post"],
 "channels": [{"channel_id": 1, "permissions": ["read","post_net"]}]}
EOF
done
```

The nets are created empty; members are added one at a time. Net management is an operator action.

```sh
for net in alpha bravo command; do
  bin/conch nets create squads $net
done
for id in 3 4 5 6;  do bin/conch nets add squads alpha $id; done
for id in 7 8 9 10; do bin/conch nets add squads bravo $id; done
bin/conch nets add squads command 3; bin/conch nets add squads command 7     # the two squad leads, a1 and b1
bin/conch nets add squads command 2 --monitor                                # the human lead listens on command and cannot speak there
bin/conch nets list squads                                                   # the nets and their members
unset CONCH_TOKEN                                                            # the next steps are the people and agents, not the operator
```

Now speak. The human uses the CLI, signed in with her token as in step 4:

```sh
bin/conch login --server http://127.0.0.1:8080 < "$T/2.token"
bin/conch tail squads &                                   # scoped messages carry [net:command] or [whisper:...] markers
sleep 1                                                   # tail shows what arrives after it has connected, not what came before
bin/conch send --to 3 squads "a word before you brief the squad"    # a whisper to a1
```

An agent speaks on a net over MCP by sending the net's id as the `audience` (here a1, principal 3, on `alpha`, whose id is 1: the nets were created in the order alpha, bravo, command, so they are 1, 2 and 3). It replies in kind by sending back the `audience` of a message it received:

```sh
curl -s -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Authorization: Bearer $(cat "$T/3.token")" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
        "name":"post_message","arguments":{"channel":"squads","body":"alpha: hold position",
        "audience":{"kind":"net","net_id":1}}}}'
```

Transmitting on a net needs the `post_net` grant in the agent's manifest, whispering needs `whisper` (and `whisper_agent` to whisper to another agent), and a channel-wide post needs `post`. A monitor who tries to speak on a net is refused, and an agent that is not on a net gets the same answer for it as for a net that does not exist.

**Scoped messages are discretion, not secrecy.** There is no end-to-end encryption: the server controls who *receives* a message, not what a recipient repeats, and the operator of the instance holds the database. Every scoped message, whisper or net, is written to the audit log with its audience and the principals it reached (never its text), and `conch send --to` says so each time.

When you are done, `rm -rf "$T"` removes the token files.

The whole layout is also a test. This builds it with real binaries, posts into every audience, and asserts the exact set of message ids each participant sees over every surface there is (MCP `read_channel`; the REST v2, v1 and v0 lists; the v2, v1 and v0 WebSockets; `conch tail`), that every scoped message read back still carries its audience, the net list each participant is shown, the refusals, the audit log, and that nothing scoped reaches the v1 or v0 wire:

```sh
go run ./e2e/nets
```

CI runs it on every pull request, since any server change can open a read path.

### Upgrading an existing instance

- **Authentication is now on by default.** Run `conchd bootstrap-operator` once against your existing data directory before starting the new `conchd`. It revokes credentials and deletes webhook hooks created before the operator existed, and tells you how many. `go run ./e2e/p2-hardening` performs this upgrade on a real P1 database on every pull request (with `--keep-existing-credentials`, to show a P1 webhook URL survives).
- **Channels now have members.** The upgrade makes every existing principal a member of every existing channel, so nothing disappears; channels and principals created afterwards start with no memberships.
- **Names clash by case and look-alike characters.** The upgrade computes the name keys of every existing principal and keeps them all; if existing names already clash (say "Alice" and "alice"), or a name has no character a reader can see, `conchd` logs the groups and the principal ids once, at that start. Both keep working; see Names in step 3.
- **Agents are deny-by-default.** An agent that existed before has no manifest and can do nothing until you write one; `conchd` logs how many such agents there are at startup. The static `--mcp-token token=principal_id` flag still works but is deprecated in favour of issued credentials.

### Known limitations

- Tokens travel in the `Authorization` header in the clear unless you put TLS in front of `conchd`; keep it on localhost or a VPN, or behind a TLS-terminating reverse proxy ([recipe](#optional-tls-and-a-reverse-proxy)).
- `conch login` does not echo a token typed at its prompt. Piping it from a file, as shown, also keeps it out of your clipboard and scrollback.
- A webhook hook URL is a posting credential. List hooks with `GET /v1/hooks` and revoke one with `DELETE /v1/hooks/{id}` (operator only); only a hash of each token is stored. The database file is created owner-only (0600) and an existing looser one is tightened at startup.

### Optional: TLS and a reverse proxy

For a public server on a droplet, with Caddy, LiveKit, systemd, backups and a restore drill, follow [docs/deploy.md](docs/deploy.md); the recipe below is the LAN version.

`conchd` speaks plain HTTP and WebSocket. It has no TLS of its own, and that is the supported core: `conchd` alone, with no other process, is the dependency-free deployment (ADR-002, rule 1 in [CLAUDE.md](CLAUDE.md)). A reverse proxy is **optional**. Nothing in `conchd` needs one and nothing degrades without one. Plain `http://` is fine on localhost and on a trusted LAN with scoped credentials; put the proxy in front when a token would otherwise cross a network you do not control, or when you want the page and the sockets on `https://` and `wss://`.

The proxy terminates TLS and forwards everything, WebSocket upgrades included, to `conchd` on loopback. Three things matter, and each was checked against `conchd` rather than assumed:

- **Keep the `Host` header the client sent.** A browser's WebSocket upgrade carries an `Origin`, and `conchd` accepts it only when its host equals the request's `Host` (the library's same-origin check; `internal/server/origins.go` adds extra origins only for `--allowed-origins`). If the proxy rewrites `Host` to the backend's address, the upgrade is refused with `403`. Caddy keeps `Host` by default. nginx does not (its default is `$proxy_host`), so the block below sets it. With `Host` kept and one public name for the page and the sockets, the origins match and **`--allowed-origins` is not needed**; leave it unset. (`conch`, `conch-adapter` and `conch-voice` send no `Origin`, so they are not subject to this check; the web page is.)
- **Bind `conchd` to loopback** (`--listen 127.0.0.1:8080`) so the proxy is the only way in, and **tell `conchd` the proxy is trusted** with `--trusted-proxies 127.0.0.1/32,::1/128`. Without it every request seems to come from the proxy, so the [invite](#invites) routes' per-address rate limits become one limit shared by everybody. With it, for a request whose connection comes from a trusted address, `conchd` walks `X-Forwarded-For` from the right and takes the first address that is not a trusted proxy as the client; if there is none, or an entry it reaches is not an address, it uses the connection's own address. A connection from anywhere else never has the header read, so a client cannot pick its own address by sending one. `Forwarded` is not read, and nothing but the invite limits uses the address. It is personal data, so it is never written to the audit trail; a log line carries at most its IPv4 /24 or IPv6 /48. Caddy sets `X-Forwarded-For` by default; nginx needs `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;` (in the block below). The password routes (`POST /v2/invites/redeem`, `POST /v1/login`) also need `X-Forwarded-Proto: https` from a trusted proxy, and the proxy must **set** both headers itself, never pass a client's through: `conchd` reads the rightmost `X-Forwarded-Proto` entry and believes it. Caddy does so by default; nginx needs `proxy_set_header X-Forwarded-Proto $scheme;`, which overwrites whatever the client sent. Without it those routes answer `403 https_required`. Trust only proxies you run, each by its own address (a `/32` or `/128`): a trusted address can claim to be any client, and a client inside a trusted range is skipped as if it were a proxy, so it picks its own address.
- **The stream needs no long idle timeout, but the upload needs the body limit raised.** `conchd` pings every open stream (`/v0/ws`, `/v1/ws`, `/v2/ws`, `/v1/events/ws`, `/v1/voice/ws`, `/v1/voice/occupancy/ws`) every 25 s and closes one whose peer does not answer within 10 s, so a proxy's idle timer never runs out on a healthy connection and a dead one is dropped in about 35 s. `/v1/events/ws` and, for a client that asks with `?heartbeat=1`, `/v2/ws` also carry a small `conch.heartbeat.v1` frame at the same rate, which is how the web page (a browser cannot see pings) tells that the connection is alive; it reconnects after 60 s without a frame. `conch tail` and the TUI send their own pings every 20 s and reconnect (the TUI) or exit (`conch tail`) within about 30 s of a connection that went silent. Any proxy read timeout over about 30 s therefore works: nginx's 60 s default is enough. nginx limits a request body to 1 MiB (the block below sets 11m); `conchd` allows uploads of 10 MiB by default (`--max-upload-bytes`). Caddy has neither limit.

Start `conchd` as in step 2, on loopback, optionally with the page:

```sh
bin/conchd serve --data /tmp/conch-data --listen 127.0.0.1:8080 --web-dir web/dist
```

#### Caddy

On a LAN there is no public name for a public CA to certify, so use Caddy's own CA with `tls internal`. `/etc/caddy/Caddyfile`:

```caddyfile
conch.lan {
    tls internal
    reverse_proxy 127.0.0.1:8080
}
```

`reverse_proxy` forwards WebSocket upgrades, keeps `Host` and does not time out an idle stream; nothing else is needed. For a name that is publicly resolvable, drop `tls internal` and Caddy gets a certificate on its own. Caddy's root certificate (for `tls internal`, by default `/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt`; `caddy trust` installs it on the host that runs Caddy) is what every other machine must trust, below.

#### nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name conch.lan;

    ssl_certificate     /etc/ssl/conch/conch.lan.crt;   # your certificate for conch.lan
    ssl_certificate_key /etc/ssl/conch/conch.lan.key;
    ssl_protocols       TLSv1.2 TLSv1.3;            # nginx before 1.23.4 also allows TLSv1 and TLSv1.1 by default

    client_max_body_size 11m;       # above conchd's 10 MiB upload default; raise both together

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $http_host;               # the Host the client sent, port included
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;   # for --trusted-proxies
        proxy_set_header X-Forwarded-Proto $scheme;                    # set, not passed on: the password routes need https
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 120s;                        # conchd pings every 25 s; this only has to be longer than that
    }
}
```

nginx's default access log records each request's URI. For the web page's sockets that includes a single-use ticket valid for 30 seconds (`?ticket=`); the `Authorization` header is not logged. Treat the log as sensitive, or log less.

The certificate and key are yours to make: a certificate from your own CA, or one from a public CA for a public name. `openssl` or `mkcert` will make a private CA; the certificate must list `conch.lan` (or the IP you connect to) in its subject alternative names.

#### Clients and `https://`

Point each client at the `https://` address. Each accepts it, and each takes the name from the URL it was given, so the stored login is keyed by that exact address: `conch login --server https://conch.lan` is a different login from `http://...`, and `conch-voice` reads the same file.

| Client | Where `https://` is accepted | What it does with it |
| --- | --- | --- |
| `conch` | `cli.NewClient` in `internal/cli/client.go` (and `cli.NormalizeServer` in `internal/cli/credentials.go`, which names the stored login) accept `http` or `https` and refuse anything else | Calls go over HTTPS. Each socket function (`Tail`, `Subscribe`, `SubscribeV2After`, `SubscribeEvents`, `SubscribeVoicePresence` in the same file) turns `http` into `ws` and `https` into `wss` |
| `conch-adapter` | `loadConfig` in `internal/adapter/adapter.go` takes `CONCH_ADAPTER_SERVER` as is, with no scheme check; `mcpclient.New` in `internal/mcpclient/client.go` posts to `<server>/mcp` with Go's `net/http`, which speaks HTTPS to an `https://` URL | It reaches `conchd` over MCP only (HTTP POST, no WebSocket), so `https://` is all it needs |
| `conch-voice` | `ServerAddress::parse` in `voice/crates/conch-voice-api/src/server.rs` and `Config::validate` in `voice/crates/conch-voice-control/src/config.rs` accept `http` or `https`; `Client::new` in `voice/crates/conch-voice-api/src/client.rs` builds a `reqwest` client with TLS from `rustls-tls-native-roots` (`voice/crates/conch-voice-api/Cargo.toml`) | REST calls and the presence socket (`presence_stream` in `presence.rs`, made by `reqwest`) go over TLS. It ignores `HTTPS_PROXY` and friends (`no_proxy()`), and it never follows a redirect |

None of the three has a flag for a CA file or for skipping verification. A certificate from a public CA just works. For a private CA (Caddy's `tls internal`, `mkcert`, your own), install its root certificate in the operating system's trust store on every client machine (on Debian and Ubuntu, copy the `.crt` to `/usr/local/share/ca-certificates/` and run `update-ca-certificates`). On Linux, `SSL_CERT_FILE=/path/to/root.crt` points a single process at one instead; that was exercised for `conch` and rests on Go's standard library, which `conch-adapter` shares. `conch-voice` uses the system store through `rustls-native-certs`, and its handling of a private CA was **not** run for this recipe. Without a trusted certificate, `conch` stops with `x509: certificate signed by unknown authority` and sends nothing.

Check it from a client:

```sh
bin/conch login --server https://conch.lan < alice.token
bin/conch whoami --server https://conch.lan
bin/conch tail --server https://conch.lan ops      # a wss:// stream; Ctrl-C to stop
```

A browser opens `https://conch.lan/` and the page reaches `conchd` on `wss://conch.lan`, because it builds the socket address from `window.location` (`web/src/api.ts`) and its Content-Security-Policy allows `connect-src 'self'` (plus the LiveKit origin when voice is configured, see [Voice in the browser](#voice-in-the-browser)).

**Secure context.** A browser treats an `https://` page, or `http://localhost`, as a *secure context*; a plain `http://` page on a LAN address is not one. Text chat does not need a secure context, which is why `http://` on a LAN is enough for it. Capturing a microphone in the page does: the browser voice client (see [Voice in the browser](#voice-in-the-browser)) shows "Voice needs a secure page" and offers no join on a plain `http://` page, so put this proxy, or LiveKit's own TLS, in front of the page. Behind `https://`, LiveKit's signalling address must be `wss://` too: a page served over `https://` may not open a `ws://` socket. See [Run LiveKit beside `conchd`](#run-livekit-beside-conchd) for its signalling port.

#### What was verified, and what was not

With Caddy and nginx absent from the machine that wrote this recipe, the two configurations above were not run. What was run: `conchd` behind a TLS reverse proxy of the same shape (a small Go program using `httputil.ReverseProxy` with a self-signed certificate) with `Host` preserved and with `Host` rewritten to the backend. `conch login`, `whoami`, `admin channel create`, `send` and `tail` worked over `https://` and `wss://` once the certificate was trusted through `SSL_CERT_FILE`, and failed with an unknown-authority error without it. A WebSocket upgrade carrying `Origin: https://<proxy>` got `101` with `Host` preserved and `403` with `Host` rewritten, which is why the nginx block sets it. `conch-adapter` and `conch-voice` were read, not run, over TLS. Treat the Caddy and nginx text as unrun until you have tried it on your host.

## Invites

A member link makes a human account with a name and password. It normally lands
the joiner in the default channels as they are when the link is used, with full
history, voice on and the per-person member-link switch on. Role permissions still
apply. With no default channels, the new member starts in no channel and can use
**Browse channels**. A holder of `manage_channels` may name landing channels instead
through the API or `--landing-channel` in the CLI.

Members can make member links by default. Making one needs `make_member_links`
and, for a member, the `may_invite` switch. Links are reusable until they expire,
reach their use limit or are closed. A link without an explicit expiry uses
`member_link_default_expiry_seconds`: seven days by default, or no expiry if the
setting is `0`. Asking for `never_expires: true` needs `manage_members`.
The maker can see and close their own links and see their joiners. A holder of
`manage_members` can do so for every link.

| Route | What it does |
| --- | --- |
| `POST /v2/invites` | Make a member link (`kind: "member"`); returns its code once. |
| `GET /v2/invites` | List your links, or all links with `manage_members`. |
| `POST /v2/invites/preview` | Preview who made the link, its landing channels and expiry. No credential needed. |
| `POST /v2/invites/redeem` | Use the code with a name and password to make an account. Needs HTTPS or loopback. |
| `GET /v2/invites/{id}/joiners` | List who joined through a link you made, or any link with `manage_members`. |
| `DELETE /v1/invites/{id}` | Close your link, or any link with `manage_members`. Existing accounts stay. |

Use `conch admin invite create --expires 24h --uses 5` to make a link.
The browser's **Invite someone** panel also makes member links. Keep the link
private: anyone holding it can use it. See [member links and the invite brake](docs/deploy.md#member-links-and-the-invite-brake)
for closing links and pausing invites.

Older account-only invites still work until they expire. Their joiners get their
remaining landing channels and the default channels, with full history and voice.
`POST /v1/invites` remains available to holders of `manage_members` for legacy
clients; other callers get `409 invite_kind_retired` and must use v2.
`POST /v1/invites/redeem` answers `409 password_required`; use v2 to make an account.

**Rate limits.** In memory, so a restart resets them. Past one, the answer is `429 rate_limited` with `Retry-After` in whole seconds.

| Limit | Default |
| --- | --- |
| Redeem attempts, per client address | 10 per 10 minutes |
| Preview attempts, per client address | 30 per 10 minutes |
| Successful redemptions, server-wide | 20 per minute |
| Invites made, per principal (operators too) | 20 per hour |

Each is a bucket that refills evenly, and failed attempts count the same as successful ones (the server-wide limit counts only redemptions that succeed). An IPv6 client is counted by its /64. Each limit keeps buckets for at most 65,536 clients at once; past that, a new client shares a bucket with its IPv4 /16 or IPv6 /32 until idle buckets are dropped, so a flood from one network cannot use up everyone else's attempts. Behind a reverse proxy the per-address limits need `--trusted-proxies` (see [the proxy section](#optional-tls-and-a-reverse-proxy)); without it, everyone behind the proxy shares one bucket.

Anyone holding a valid code can learn whether a name is taken, from the `409`, within the redeem limit, and without using the invite up (through v2, five such answers an hour per address, then `429`). A link that leaks lets whoever finds it test names that way, so close a leaked link.

A password is up to 128 characters (normalized to NFC first) and may be short, but not empty: any characters, spaces welcome; it may not be on the bundled list of common passwords, be one short run repeated, or contain the account's name. It is stored only as an Argon2id hash (64 MiB, 3 passes) and is in no log, audit row or error.

### Joining and guests: no guests for now

Guests were removed on 2026-10-30. The community migration ends active visits and
closes invites that admitted guests. Guest records and attribution are kept.
Guests return with #1004. The bare address admits no guests.

`open_signup` is off by default. An owner, or an operator with
`change_server_settings`, can set it through `PUT /v1/join/settings` or
`conch admin join set --open-signup on`. It needs a public default channel.
When on, the address offers account creation into the default channels, with full
history and voice. `GET /v1/join/settings` reads the settings.
The settings stay on v1; `member_link_default_expiry_seconds` sets the default link
lifetime. `conch admin join set --link-expiry 7d` sets it from the CLI.

### Signing in with a password

A member who joined with a password (through `POST /v2/invites/redeem`) signs in again from any device with `POST /v1/login` and `{"name":…,"password":…}`, over HTTPS or from loopback: the answer is `{"next":"signed_in",…}` with a new credential, kept to last as an invite's is. Every failure, whatever its cause (no such name, a wrong password, a disabled or hidden account, an agent, an account with no password yet), is the same `401 auth_failed` after the same work. An operator's right password answers only `{"next":"totp","challenge":…}`, for the second factor, and an operator who has not set both up gets `auth_failed`. Accounts made without a password (through v1 before it stopped making accounts, or by an operator) have none and keep signing in with their credentials and sign-in links. Limits: ten failures per address and five per typed name in ten minutes (then `429` at once, with `Retry-After`, for a name whether anyone has it or not), thirty sign-ins per name an hour, and a bounded queue for the hashing itself. Audited: `login` and `credential_created`; failures as `login_failed`, thinned, with the reason and the name only when it is someone's.

### Administering invites

A holder of `manage_members` sees every invite, closes any of them, and has these routes besides (any operator may read `GET /v1/invites/settings`; changing it needs `manage_members`):

| Route | Body | Success |
| --- | --- | --- |
| `GET /v1/invites/{id}/principals` | | `200`, everyone who joined through it: id, name, when, and whether disabled |
| `POST /v1/invites/{id}/disable-joiners` | `{"dry_run":bool,"close":bool}` | `200`, `{"dry_run":…,"count":N,"principal_ids":[…]}`. Disables every enabled joiner the way `POST /v1/principals/{id}/disable` does: credentials revoked, sockets closed, out of voice. `close` (default `true`) closes the invite first. `dry_run` changes nothing and only counts |
| `POST /v1/principals/{id}/disable-invitees` | `{"dry_run":bool,"depth":"1"\|"all"}` | `200`, the same shape: the people this person invited (`"1"`, the default), or everyone under them (`"all"`). The person themselves is left alone, and plain `disable` never follows the tree |
| `PUT /v1/principals/{id}/invite-permission` | `{"may_invite":bool}` | `204`. Needs `manage_members`. The per-person brake applies to members. Turning it off closes their open member links and personal invites; turning it on does not reopen them |
| `PUT /v1/principals/{id}/voice-permission` | `{"voice_allowed":bool}` | `200` `{principal_id, voice_allowed, changed}`. Needs `manage_members` and a recent proof; an owner target needs an owner; an agent is `409 not_a_person`; a hidden or unknown id is `404 principal_not_found`. Off removes the person (and their companion and viewer, and any share) from every voice room before the answer, with no room rotated; on changes nothing live. Each change is audited `voice_permission_changed` |
| `GET /v1/invites/settings`, `PUT /v1/invites/settings` | `{"paused":bool}` | `200` with the setting. Paused refuses making, previewing and redeeming any invite. It is stored, so it survives a restart |

The two bulk disables never disable an operator or the caller, and the last operator is never disabled by anything. Disabling a person, however it is done, closes every invite they made that could still be used.

`conch admin` covers all of it:

```sh
LINK=$(bin/conch admin invite create --expires 24h --uses 5)   # a member link, alone on stdout; --never-expires for operators
bin/conch admin invite list --state open            # <id> <kind> <state> uses <n>[/<max>] joiners <n> expires <time|never> by <id> other <n> channels <names>
bin/conch admin invite close 3
bin/conch admin invite joiners 1                    # <id> <enabled|disabled> <joined> <name>
bin/conch admin invite disable-joiners 1            # a dry run: how many it would disable
bin/conch admin invite disable-joiners 1 --yes      # disable them and close the link
bin/conch admin invite pause                        # resume, status
bin/conch admin principal invite-permission 5 on    # or off
bin/conch admin principal disable-invitees 5 --all  # a dry run; --yes to do it
```

A link is a member link (ADR-013): it lands its joiner in the default channels unless a holder of `manage_channels` names `--landing-channel`, and `--open`, `--personal`, `--channel`, `--voice`, `--may-invite` and `--join-as` of the old per-channel invite are gone (`--max-uses` is `--uses`). The rest of the community model, from the command line:

```sh
bin/conch channels list [--all]                     # <id> <visibility> <default|-> <joined|-> <members|-> <archived|-> <name>; --all is for operators
bin/conch channels join lounge                      # and leave; create lounge [--private]
bin/conch admin channel settings lounge --public --default on   # --private keeps every member; the answer says whether open sign-up went off
bin/conch admin channel settings lounge --archive               # keeps history and members, refuses anything new; --restore brings it back; a default channel cannot be archived
bin/conch admin principal voice 5 off               # a person's voice switch; asks for your password when the server does
bin/conch admin directory members                   # the community
bin/conch admin join set --open-signup on --link-expiry 7d      # 0 for no default expiry; join status shows both and the default channels
```

`invite create` prints the link and nothing else on standard output; its id and expiry go to standard error. The code appears in no other output.

**When a link leaks.**

1. `conch admin invite pause`: nobody can join through any invite while you work.
2. `conch admin invite joiners <id>`: who came in through it.
3. `conch admin invite disable-joiners <id>` to see how many, then again with `--yes`. Each of them is disabled at once, sockets and voice included, and the link is closed.
4. `conch admin invite list --state open` to check that nothing else needs closing (`conch admin invite close <id>`).
5. `conch admin invite resume`.

Someone who joined through the link and should stay can be enabled again (`conch admin principal enable <id>`) and issued a new credential.

Removing a person and everyone they invited (`conch admin principal disable-invitees <id> --all`) works the same way: pause first. The tree is read once, before anyone is disabled, and each person's own invites close only when they are disabled, so someone who joins through one of them in between is not in the list.

**Audit.** Making, redeeming and closing an invite are audited by the store (`invite_created`, `invite_redeemed`, `principal_created`, `member_added`, `credential_created`, `invite_closed`), attributed to the maker, or, for a redemption, to `invite:<id>`. `invite_closed` says why: `reason=creator` (its maker), `operator` (an operator who did not make it), `creator_disabled` (its maker was disabled) or `permission_revoked` (its maker lost the right to invite). An operator's administration adds `invites_paused`, `invites_resumed`, `invite_permission_changed`, `voice_permission_changed`, and one summary row per bulk disable, `joiners_disabled` (the invite, the count, whether it was closed) or `invitees_disabled` (the person, the depth, the count), beside each person's own `principal_disabled`. A dry run writes nothing. Refusals are audited as `invite_rate_limited` (which limit) and `invite_redeem_refused` (an invalid code), by actor `anonymous` on preview and redeem, thinned like `access_denied`: the first eight of a kind in a minute, then the 16th, 32nd…, each with `count=N`. No row carries the code or the client address. A change to the permissions document that takes `make_member_links` from a maker, or gives it back, suspends or resumes the maker's open personal invites without closing them: one `invite_suspended` or `invite_resumed` row per invite (actor `system`, `reason=permission`); the same change to `act_as_members` writes `sign_in_link_suspended` and `sign_in_link_resumed` for sign-in, reset and enrolment links.

### Signing a person back in

A person who joined with a password can sign in with it again. For an older account without a password, a holder of `act_as_members` can make a **sign-in link** to return to the same account:

```sh
LINK=$(bin/conch admin principal sign-in-link 7)               # the link, alone on stdout: <server>/app/#/signin/conch_sil_...
bin/conch admin principal sign-in-link 7 --expires 2h          # a duration, 2d, or an RFC 3339 time; default 24h, at most 7d
bin/conch admin sign-in-link close 4                            # the link's id, from the line on stderr: it stops working
```

The person opens it in any browser; the page says whose account it opens and signs into it with a new credential, kept to last like an invite's (renewed by use, with the lifetime of the invite they joined through, or 90 days). Their other credentials are left as they are. A link works once, for one person, and only for a member: an agent, an operator, or a disabled or hidden account is refused (`400 invalid_request`, `409 principal_disabled`). It is handled like an invite code: only its hash is stored, the code travels in a `POST` body and the URL fragment only, and it is in no log or audit row.

| Route | Who | Answer | Refusals |
| --- | --- | --- | --- |
| `POST /v1/principals/{id}/sign-in-links` | operators; body `{"expires_at":…}` or `{}` | `201` with the link and its code, `Cache-Control: no-store` | `400 invalid_request`; `404 principal_not_found`; `409 principal_disabled`; `429 rate_limited` (with making invites) |
| `POST /v1/sign-in-links/preview` | anyone; body `{"code":…}` | `200`, `{"name":…}` and nothing else | `404 sign_in_link_invalid`; `429 rate_limited` |
| `POST /v1/sign-in-links/redeem` | anyone; body `{"code":…}` | `201` with the principal, its new credential and the token, `Cache-Control: no-store` | `404 sign_in_link_invalid`; `429 rate_limited` |
| `DELETE /v1/sign-in-links/{id}` | operators | `204`, also for a link already used or closed, which it leaves as it is | `404 sign_in_link_not_found` |

A code that is unknown, malformed, used or expired, made by someone no longer an operator, or for someone no longer a member who may sign in, gets one answer: `404 sign_in_link_invalid`. Preview and redeem share the invite routes' per-address limits, and redeem their server-wide one, so the two kinds of code together get no more tries than one. Pausing invites does not pause sign-in links. Disabling the person, or the operator who made the link, closes it, and enabling them again does not reopen it. Audited: `sign_in_link_created` (by the operator, about the person), `sign_in_link_redeemed` and `credential_created` (by `sign_in_link:<id>`), `sign_in_link_closed` (`reason=operator`, `principal_disabled` or `creator_disabled`), and, thinned as for invites, `sign_in_link_redeem_refused` and `invite_rate_limited`.

**When a link leaks, or the person says it did not work.** The landing page tells someone whose link no longer works to tell an operator if they did not use it themselves. To see whether it was used: `conch audit export` shows `sign_in_link_redeemed` with `sign_in_link=<id> credential=<credential-id>`, and `conch admin credential list <principal-id>` shows that credential, labelled `sign-in link <id>`. If someone else used it, `conch admin credential revoke <credential-id>` ends that sign-in at once, and leaves the person's own. A link not yet used is stopped with `conch admin sign-in-link close <id>`. Then make the person a new one.

### Cleaning up test accounts

No account is ever deleted. Deleting one means disabling and hiding it: the account is disabled as `principal disable` does it (credentials revoked, sockets closed, out of voice, its invites closed), its webhooks are revoked, it leaves every channel and net, so it is in no member list, and it gives up its name, which becomes `hidden-<id>`. The old name is free for anyone at once and stays in the audit trail. The account's messages stay, shown under the new name. Its open approvals are left as they are: one that escalates to a hidden account waits until it expires. Names that read as `hidden-…`, in any case or look-alike spelling, are reserved (see Names above): only a hide gives one. A name like that from before the reservation is kept, and `conchd` names those accounts in its log when it upgrades.

```sh
bin/conch admin principal list                       # <id> <kind> <role> <active|disabled|hidden> creds <n> invite <id|-> <name>
bin/conch admin principal list --state hidden        # or active, disabled; --prefix <p>, --invite <id>, --kind human|agent
bin/conch admin principal hide 12                    # one account, at once
bin/conch admin principal hide --prefix test-        # a dry run: whom it would hide
bin/conch admin principal hide --prefix test- --yes  # hide them
bin/conch admin principal hide --invite 3 --yes      # everyone who joined through invite 3
```

| Route | Body | Success |
| --- | --- | --- |
| `GET /v1/principals` | | `200`, `{"principals":[…],"next_before":…}`, newest first: id, kind, name, role, `created_at`, `disabled_at`, `hidden_at`, `invited_by`, `invite_id` and the count of live credentials. `?state=active\|disabled\|hidden` (default: everyone not hidden), `?kind=`, `?role=`, `?invite_id=`, `?name_prefix=`; paged with `?before=<id>` and `?limit=` (at most 200, default 100) |
| `POST /v1/principals/{id}/hide` | | `200`, `{"principal":{…}}` as it now is. Hiding a hidden account changes nothing. `400` for yourself, `409 last_operator` for the last operator able to sign in |
| `POST /v1/principals/hide` | `{"name_prefix":s,"invite_id":n,"kind":"human"\|"agent","dry_run":bool}` | `200`, `{"dry_run":…,"count":N,"principals":[{"id":…,"name":…}],"remaining":M}`. `dry_run` is required (`400` without it, so a hide is never done by leaving it out). A name prefix (at least 3 characters, matched exactly as typed, case included) or an invite is required; given both, an account must match both. Never selects an operator, an account already hidden, or you. At most 500 a call: `remaining` says how many more match, so run it again |

All three are for operators only (anyone else gets `403 forbidden`, audited as `access_denied`). With authentication off the bulk hide answers `401`, as the bulk disables do. `conch admin principal enable <id>` brings a hidden account back as far as being able to be given a new credential: it keeps its `hidden-<id>` name and gets back no membership and no credential.

**Audit.** A hide writes the disable's rows, a `member_removed` per channel and `net_member_removed` per net, a `hook_revoked` per webhook (`reason=principal hidden`), `principal_renamed` and `principal_hidden` (detail `name="<old name>"`). A bulk hide adds one `principals_bulk_hidden` row with the selector and the count; a dry run writes nothing. Enabling a hidden account writes `principal_unhidden` after `principal_enabled`. Creating an account writes `principal_created` (`kind=<kind>`), by `POST /v0/principals`, `conchd bootstrap-operator` or an invite, and creating a channel `channel_created` (`name="<name>"`).

## Voice (optional)

Voice adds the first optional *server* process to a Conch deployment: a [LiveKit](https://livekit.io) server running beside `conchd`. LiveKit carries the audio. `conchd` decides who may join a channel's voice room, who may speak and who is shown, and records it ([ADR-004](docs/adr/ADR-004-voice-via-livekit.md), [design note](docs/design/voice-control-plane.md)). `conchd` never touches audio. The `conch` TUI joins voice by starting `conch-voice` itself ([Voice in the TUI](#voice-in-the-tui)); the plain CLI only reads who is connected (`conch voice status`, `conch voice where`).

Everything else is unaffected by voice. With no LiveKit settings, messaging, approvals and audit work exactly as before and the voice endpoints answer `voice_not_configured`. With settings but LiveKit stopped or unreachable they work too, and the voice endpoints answer `voice_unavailable`: the session endpoint refuses (503), presence says `available: false`, `conch voice status` prints one line and exits nonzero, and `conchd` writes one `voice_enforcement_unavailable` audit row per outage. `conchd` looks at LiveKit once when it starts but does not wait for it, so it starts and stays up whether or not LiveKit does, and voice recovers by itself when LiveKit returns. While `conchd` cannot reach LiveKit it cannot remove anyone from a room: people who are connected stay connected, and nobody is removed by name until it can reach LiveKit again (a room that must be abandoned is still retired in `conchd`'s own records at once, so no new session names it).

**Who is in voice where.** `GET /v1/voice/occupancy` answers a `conch.voice_occupancy.v1` document: for each channel you are a member of, how many people are in its voice room, as that channel's presence counts them (`{"schema":"conch.voice_occupancy.v1","configured":true,"available":true,"channels":[{"channel_id":7,"in_voice":3},{"channel_id":9,"in_voice":0}]}`). It names nobody; who is there is in the channel's presence. A channel you are not in is absent, never a zero, and operators get no exemption. While voice is not configured or LiveKit is not answering, `channels` is empty. `GET /v1/voice/occupancy/ws` (bearer, or `?ticket=`) sends the same document on connect and again whenever a count changes, and nothing else: not when someone talks or shares. Being removed from a channel takes its row out of the next frame; being added shows within the 30 s re-check. Both routes are for people who hold voice: an agent gets `403 forbidden` and a person without voice `403 voice_not_allowed`, both audited as `access_denied`, and with authentication off they answer `400 voice_requires_auth`. Seeing occupancy never joins voice. The [design note](docs/design/voice-occupancy.md) has the rest.

### Run LiveKit beside `conchd`

This is the single-machine setup, for trying voice. A public LiveKit (external address, TLS on signalling, TURN fallback, a key per instance) is in [docs/deploy.md](docs/deploy.md).

These commands run LiveKit in Docker on the host network, with its signalling port bound to localhost, and continue the quickstart (the `ops` channel, `alice` as principal 2). The static settings are in a file; the key pair is not:

`/tmp/conch-voice` below is for a demonstration on a machine only you use: another local user who creates that directory first controls the settings file LiveKit reads. On a shared machine, put the file in a directory that only you can write.

```sh
umask 077
mkdir -p /tmp/conch-voice
cat > /tmp/conch-voice/livekit.yaml <<'EOF'
port: 7880
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: 7881
  port_range_start: 50000
  port_range_end: 50100
  use_external_ip: false
room:
  auto_create: false
EOF
export CONCHD_LIVEKIT_API_KEY=conch
export CONCHD_LIVEKIT_API_SECRET="$(head -c 32 /dev/urandom | base64 | tr -d '/+=\n')"
export LIVEKIT_KEYS="$CONCHD_LIVEKIT_API_KEY: $CONCHD_LIVEKIT_API_SECRET"
docker run -d --name livekit --network host \
  -e LIVEKIT_KEYS \
  -v /tmp/conch-voice/livekit.yaml:/etc/livekit.yaml:ro \
  livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3 \
  --config /etc/livekit.yaml --node-ip 127.0.0.1
bin/conchd serve --data /tmp/conch-data --listen 127.0.0.1:8080 --livekit-url ws://127.0.0.1:7880
```

- **Automatic room creation must be off** (`room.auto_create: false`). `conchd` creates a channel's room itself, under a random name, before it issues a token. When someone loses the right to be in a room, `conchd` rotates it: the channel gets a new room and the old one is deleted, which disconnects everyone in it and makes every token for it useless, including the ones LiveKit itself sends connected participants and renews. That only holds if a deleted room stays deleted. With automatic creation on, a stale token recreates the room by joining it. `conchd` cannot read LiveKit's configuration, so this is yours to get right.
- **Keys come from the environment.** `CONCHD_LIVEKIT_API_KEY` and `CONCHD_LIVEKIT_API_SECRET` are read only from the environment, never from a flag, so the secret is not in `conchd`'s process list, and `conchd` never logs it. LiveKit must be given the same pair (here through `LIVEKIT_KEYS`, passed to `docker run` by name so that the secret is not on its command line either). Use a long random secret.
- **All or nothing.** With none of the three settings (URL, key, secret), voice is not configured. With some but not all, `conchd` refuses to start and names what is missing.
- `--livekit-url` (or `CONCHD_LIVEKIT_URL`) is the `ws://` or `wss://` address clients are given. `--livekit-api-url` (or `CONCHD_LIVEKIT_API_URL`) is the `http://` or `https://` address `conchd` calls for room control; it defaults to the same address with the scheme swapped.
- **Room prefix and one API key per server.** Every room name is a prefix and 128 random bits. `--livekit-room-prefix` (or `CONCHD_LIVEKIT_ROOM_PREFIX`) sets the prefix for new rooms; the default is `conch-`. It is lower-case letters, digits and `-`, at most 32 characters, and ends with `-`; `conchd` refuses to start otherwise. Rooms already stored keep their names, so changing it migrates nothing. Several `conchd` servers may share one LiveKit, but each needs its own `--livekit-room-prefix`, one that no other server on that LiveKit uses. A server deletes any room of its own prefix and name shape (the prefix and 26 lower-case base32 characters) that its database does not know, so two servers with the same prefix end each other's calls; if the log warns that this happened on two sweeps in a row, look for another server with the same prefix. Rooms of other prefixes are left alone, even a prefix that begins with this server's (`alpha-2-` beside `alpha-`). The prefix keeps the servers' rooms apart by name, but it is not an access control, because a LiveKit API key can act on every room. Give each server instance its own key pair: LiveKit accepts several (`keys:` in its settings, or several pairs in `LIVEKIT_KEYS`), and one key per server limits what a leaked secret can do and lets you revoke one server without touching the rest.
- **Clocks.** A join token lives 15 seconds and LiveKit allows a minute of leeway either way. If `conchd` and LiveKit run on different machines, keep their clocks synchronised.
- **Ports and TLS.** Clients need LiveKit's signalling port (7880 here) and its media ports (TCP 7881 and the UDP range). `bind_addresses` covers the signalling port only: **the media ports listen on every interface of the host** (measured with this configuration: `127.0.0.1:7880`, `*:7881`). Nothing can be done on them without a session negotiated over the signalling port, but if this host is reachable from networks it should not serve, close them in its firewall. Put TLS in front of the signalling port for anything beyond localhost: [docs/deploy.md](docs/deploy.md) does that with Caddy, with the public addresses and a firewall. The example here is a single-host setup.

### Look at it

A member asks `conchd` for a session and is given the LiveKit address, their identity and a short-lived join token for the channel's room. `conch voice status` shows who is connected:

```sh
curl -s -X POST localhost:8080/v1/channels/ops/voice/session -H @alice.header -o /dev/null -w '%{http_code}\n'
# 200
bin/conch voice status ops
# nobody is connected to voice in ops
```

`alice.header` holds the line `Authorization: Bearer <alice's token>` (as in step 3, kept in a private file so the token is not in the process list). If you run `conch voice status` within a second or two of starting `conchd` and LiveKit together, it can say "voice is unavailable" until `conchd` has looked at LiveKit once; run it again. Joining is a client's job: `conch-voice join`, the TUI's `/voice join` ([Voice in the TUI](#voice-in-the-tui)) or the web page; `go run ./e2e/voice` joins a headless participant. Stop LiveKit with `docker rm -f livekit` and the same two commands print `503` (the body says `voice_unavailable`) and "voice is unavailable", the second exiting nonzero. If a `go run ./e2e/voice` is killed before it can clean up, remove what it left with `docker ps -aq --filter label=conch-voice-check | xargs -r docker rm -f`; that removes every voice-check container on the host, so do not run it where another run is in progress.

### Which server `conch-voice` uses

`conch-voice join` takes the server from `--server`, else `CONCH_SERVER`, else `server` in `voice.toml`, else the server of the stored login when exactly one is stored (and `CONCH_TOKEN` is not set: that token is not the stored login, and is never sent to its server), else `http://127.0.0.1:8080`. It reads the same `credentials.json` as `conch` and uses it the same way, so one `conch login` is enough for both. With several logins stored it does not guess: if it ends up on the default and has no login for it, or nothing answers there, the error names the stored servers, and `--server <url>`, `server` in `voice.toml` or `CONCH_SERVER=<url>` picks one.

### Voice in the TUI

In the TUI, `/voice join` joins the voice of the channel on screen (or `/voice join <channel>` another of your channels), and you talk with the talk key `conch-voice` reads, with no second terminal. The TUI starts `conch-voice managed` as its own child for each voice session and stops it when you leave, quit or sign out; if the TUI is killed, `conch-voice` notices that its owner has gone, shuts the microphone and is gone within 3 s. The design is [docs/design/tui-voice.md](docs/design/tui-voice.md).

| Command | Does |
| --- | --- |
| `/voice join [channel]` | Join that channel's voice, or the one on screen. Browsing channels afterwards does not move it; to switch, leave and join. If presence shows you already in that room from another client, it says joining here disconnects it, and joins if you repeat the command within 10 s. For a channel that is not on screen, whose presence the TUI does not read, it always asks for that repeat. |
| `/voice leave` | Leave. |
| `/voice mute on\|off`, `/voice deafen on\|off` | Mute, deafen. The strip says `muting…` until `conch-voice` has done it, then what it did. Undeafening leaves mute as it was. |
| `/voice retry` | Try again after a failure a new attempt could fix (a lost server, another device taking over). Nothing retries by itself. |
| `/voice status` | Everything voice is doing in words, the `conch-voice` version and path, and the last lines of its log. |
| `/voice help` | The commands, the talk key and the note on SSH below. |

- **Setup.** Install `conch-voice` beside `conch` or in `PATH`, or set `CONCH_VOICE_HELPER` to its absolute path; it must come from the same release as `conch`, and it needs Linux with PipeWire. Set the talk key under `[keys]` in `voice.toml` beside your login (`conch-voice devices` lists keyboards, `conch-voice keys` shows key codes). A program that other users could change, or that sits in a directory they could write, is refused and never run.
- **The same person, the same server.** `conch-voice` joins as whoever the TUI is signed in as, on the TUI's server: the stored login by name, or `CONCH_TOKEN` when the TUI's own token came from there. It checks with `whoami` before it opens anything and refuses a login that is someone else's. With no login (a server with `--auth off`), `/voice join` says voice needs a signed-in person. One account has one voice connection per room: joining in a second TUI, or in the browser, disconnects the first, which says so.
- **Text never waits for voice.** A missing, refused or failed `conch-voice` changes the strip and the status line, never the composer, the messages or the people pane.
- **Over SSH**, `conch-voice` runs on the machine where `conch` runs and uses that machine's microphone, speakers and keyboard, not those of the computer you connect from. No audio is carried over SSH.

`go run ./e2e/voice managed` checks this end to end with real TUIs on pseudo-terminals, the `conch-voice` each starts, `conchd` and LiveKit in Docker, tones for microphones and a named pipe for the keyboard; it is not part of `go test ./...` or CI, and it needs Docker and the voice toolchain (`scripts/voice-toolchain.sh`). The check with a real headset and talk key is the maintainer's, by hand: [docs/p3-walkthrough.md](docs/p3-walkthrough.md#p3-15-voice-in-the-tui-opt-in).

### Voice in the browser

The web page at `/app/` joins a channel's voice with the LiveKit JavaScript SDK, in the same room and under the same rules as `conch-voice`: `conchd` issues the session, the join token allows publishing a microphone and nothing else, and the poller checks what the page reports. One identity is allowed per person per room, so the browser and `conch-voice` displace each other: the one that joined later stays, and the other says another device took over.

- **Join and leave** are in the Voice section beside the member list. Mute and deafen are shown. Deafen also mutes everything you hear and shuts your microphone.
- **Two ways to open the microphone.** The page starts in **Open microphone**. It opens when you speak, using a level you set with a meter, and shuts after a short pause. **Open mic**, beside the talk control, switches between the two in one tap. On a phone it is on the voice strip. **Voice options** has the same choice. The choice is remembered per browser. **Push to talk** opens it while you hold the talk key (a backtick by default; the page lets you change it) or the **Hold to talk** button, which also works by touch, and shuts it a release delay after you let go (200 ms unless you set another under **Voice options**; issue #965). Echo cancellation, noise suppression and automatic gain are on in both.
- **The talk key works only while this tab has focus**, and only when focus is not in a text box. Losing focus or switching tab shuts the microphone at once, without the release delay; hold the key again to talk. A key that works while a game has another window in front needs a desktop app; on Linux, `conch-voice` has a global talk key.
- **On a phone, audio continues only while the page is open on screen.**
- **Secure context.** A microphone needs `https://` or `http://localhost` (see [Clients and `https://`](#clients-and-https) above). On any other page the Join button is off and says why.
- **The Content-Security-Policy changes only when voice is configured.** With LiveKit settings, `connect-src` also names the LiveKit address's origin: `wss://host[:port]` and the same host over `https://` (the SDK makes plain requests to it when a connection fails). With none, the policy is what it was. Because the address is written into the policy, `conchd` refuses to start if its host is anything but letters, digits, dots and hyphens (or an IPv4 address) or its port is not digits; an IPv6 literal is refused too, since a policy cannot name one, so give LiveKit a host name. Pages under `/app/` carry `Permissions-Policy: microphone=(self), camera=(self), display-capture=(self)`: the page's own origin may use a microphone and a camera and share a screen (webcam video, ADR-015). Every other answer, the API, files and the metrics listener included, carries `Permissions-Policy: camera=(), microphone=(), display-capture=()`. No `media-src` is needed: playback from a stream is not governed by it (checked in Chromium, where the browser suite saw no policy violation).
- **When it stops.** If the room is deleted or you are removed, the page asks `conchd` again, which either issues a new session or says why not (for example that you are no longer a member). If LiveKit is down the page says voice is unavailable and tries again, 1 s doubling to 30 s; chat is unaffected. The SDK's own reconnect is allowed for 20 s, then the page goes back to `conchd`.
- **Reports.** Every time the microphone opens or closes the page tells `conchd`, one report at a time and in order, so the audit log has a `voice_transmit_started` and `voice_transmit_stopped` pair for each talk. A press waits for the answer to its `started` report before the microphone opens, for at most a second, and the page says *Waiting for the server* meanwhile and *Transmitting* only once audio is sent; past that second audio goes ahead without the answer, so it never depends on `conchd` being reachable. A browser that does not report would get a `voice_transmit_unreported` row for every transmission.
- **No token or room name** is stored, logged, shown or put in the page; the SDK's own log is set to warnings, redacted, and kept in memory (the page makes no console call).
- **Where the join token can still be seen.** When a connection fails, the SDK puts the short-lived (15 s) join token in the `access_token` query of its `/rtc/v1/validate` request and of the signalling URL, so it appears in LiveKit's access log and in the page's resource timing entries.

| Browser | Voice | Talk key |
| --- | --- | --- |
| Chrome or Edge on Windows, macOS, Linux | expected | tab focused |
| Firefox or Safari, desktop | expected | tab focused |
| iPhone or iPad | expected | while open on screen |
| Android, Chrome | expected | while open on screen |

"Expected" means what the browsers document: Chromium was tried here against a real LiveKit with a fake microphone (`make web-e2e`); the other cells have not been tried by hand yet.

### Screen share

Anyone in a channel's voice room may share a screen into it, one share per room at a time; the others in the room watch. The join token never allows it: a member who is connected asks `conchd` with `POST /v1/channels/{channel}/voice/share`, and `conchd` then lets that one connection publish a screen and its sound as well as a microphone (`200`, a `VoiceShareV1`). While someone else shares the answer is `409 voice_share_busy`; without a session, or not connected, it is `409 voice_no_session`; with LiveKit down, `503 voice_unavailable`. `DELETE` on the same path stops it: the sharer may, and so may an operator, whether or not they are a member; anyone else gets `403`. A share also ends by itself when the sharer leaves the room or loses access, when the room is rotated, or when nothing has been shared for a while (15 seconds after the claim if nothing was published, 5 seconds after the share's tracks went). Ending a share takes the permission away again, and LiveKit removes the share's tracks for every viewer at once. Presence marks the sharer with `sharing` (and `sharing_audio` when the share has sound), and every start and stop is in the audit log as `voice_share_started` and `voice_share_stopped` with its reason. A shared track nobody claimed (which LiveKit should never allow) is withdrawn by the next pass and audited as `voice_share_unclaimed`. Claims are held in memory: restarting `conchd` ends every share, and the first pass after it takes the permission back. `conchd` limits no bitrate; the sharer's browser chooses the quality, and LiveKit forwards it. The server half is in the [design note](docs/design/voice-control-plane.md) §12.

**In the browser.** While connected to a channel's voice, the Voice section offers *Share screen*, on every browser that has a screen picker (no iPhone or iPad browser does). Before the picker opens it says what sound the share will carry; afterwards it says whether sound was actually shared. The page connects, claims the share, opens the picker, claims again (the picker may have stayed open longer than `conchd` waits for a first track) and then publishes. The Conch tab itself is left out of the picker's choices, and the page warns that sharing the entire screen or the browser's window would still show it, chat included.

- **Quality.** The picture is captured at the source's own size, up to 3840×2160, and at up to 60 fps; it is never scaled up. It is published as captured, at up to 6 Mb/s when its longer side is at most 1920 pixels, 10 Mb/s up to 2560 and 18 Mb/s above, with two smaller simulcast layers below it, 1080p at 30 fps (4 Mb/s) and 720p at 15 fps (1.5 Mb/s); a layer that would not be smaller than the capture is left out, so a 1080p capture has the 720p layer only. The codec is VP8. A viewer receives only the layer its view needs, and the sharer encodes only the layers someone receives, so the 4K layer costs upload and server bandwidth only while someone watches it full screen on a large display. These are starting figures; the bandwidth measurement of one sharer to 29 viewers refines them. Every share is captured as motion: when bandwidth runs short the frame rate is kept and text may lose sharpness. Sound is sent as music (no voice processing, stereo when the capture has two channels), and the whole system's sound leaves out what the Conch page itself plays, so the room's voices are not sent back to it.
- **Stopping.** *Stop sharing*, the browser's own stop control and leaving voice each ask `conchd` to end the share, but only while presence still shows it as the sharer's: an operator's stop ends whoever holds the share, so a page whose claim lapsed (a picker left open) never sends one. If the server ends it (an operator, lost access, the room, nothing shared for a while), the page stops publishing and says which, from the reason the sharer's own presence carries (`share_ended`). A share does not survive a reconnect through `conchd`: the page says so and the person shares again. A reconnect that the SDK makes by itself claims the share again. A rotation is the exception: when anyone in the room is disabled, hidden or removed, `conchd` renews the room, which ends the share (`voice_share_stopped` with `reason=rotated`), and the sharer's page joins the new room, claims the share again and publishes the same capture without opening the picker (a new `voice_share_started`); viewers keep watching. The claim is `conchd`'s check as always, so a sharer who was the one removed does not get the share back, and a capture already stopped in the browser is not resumed.
- **Watching.** Anyone in the room sees who is sharing, and *Watch* opens the share in the Voice section, with *Full screen*. The picture fits a phone's screen. The share's sound has its own volume and mute, apart from voice; deafen silences it too. Where the browser will not start sound by itself (phones), the view offers *Tap to play sound*. The page subscribes to a share only while it is watched, and the SDK asks LiveKit for the layer the view's size needs, so a small view or a phone receives a smaller layer than a full screen, and a closed view none.
- **Watch only.** Beside *Join voice*, *Watch only* connects without the microphone: nothing is captured, the browser does not ask for it, and presence still lists the person. *Use microphone* captures and publishes it later, muted, as a full join does.

| Browser | Share a screen | Sound shared | Watch |
| --- | --- | --- | --- |
| Chrome or Edge on Windows | yes | a tab's sound, or the whole system's with the entire screen | yes |
| Chrome or Edge on macOS or Linux | yes | a tab's sound only | yes |
| Firefox or Safari, desktop | yes | none | yes |
| iPhone or iPad | no | no | yes |
| Android, Chrome | to check | no | yes |

As for voice, Chromium on Linux was tried here against a real LiveKit with a fake screen and a fake sound track (`make web-e2e`, `web/e2e/12-screen-share.spec.ts`); the other cells are what the browsers document, not yet tried by hand.

### Camera

Anyone in a channel's voice room who holds the `use_camera` permission may turn a camera on in it, and everyone in the room can watch it (webcam video, [ADR-015](docs/adr/ADR-015-webcam-video.md), design note [webcam-video.md](docs/design/webcam-video.md)). **The camera is off for everyone but owners until an owner turns it on**: `use_camera` is off for operators and members by default, needs `use_voice`, and is withheld by the voice switch too ([the permissions](docs/deploy.md#the-twelve-permissions)). It is for one-to-one calls and small groups, up to 9 cameras in a room. The camera and a screen share can be on together. The camera runs in the browser only: `conch-voice`, the TUI and the CLI publish no camera and show none.

**The server half.** The join token allows a microphone and nothing else, as for the share, so a new connection cannot publish a camera until it claims one, whatever the client does. A member who is connected asks `conchd` with `POST /v1/channels/{channel}/voice/camera`, and `conchd` then lets that one connection publish a camera as well as a microphone (`200`, a `VoiceCameraV1`: `principal_id` and `started_at`). Joining, unmuting and sharing never claim one; the camera is off on joining, after a reconnect and after a restart.

- **Refusals.** A person without voice (`use_voice` off, or the voice switch off) gets `403 voice_not_allowed`; one with voice but without `use_camera` gets `403 permission_denied` naming `use_camera`; an agent, `403 forbidden`; without a session in the channel's room, or not connected, `409 voice_no_session`; voice not configured, `503 voice_not_configured`; LiveKit down, `503 voice_unavailable`. A claim refused for lack of `use_camera`, a full room, a stale claim or the rate bound writes no audit row; a refusal of voice itself, or of an agent, is audited as `access_denied`, as it is everywhere.
- **The cap.** A room holds at most 9 cameras. The tenth claim is `409 voice_camera_full`. The cap is a constant in `conchd`, not a setting, and it does not limit how many people are in voice: 30 can listen while 9 show their cameras.
- **Continuing a claim.** A request that carries `?started_at=<the claim's started_at>` continues that claim (a connection replaced by the SDK's own reconnect gets the camera granted again, and nothing is written). A name that is not the caller's current claim is `409 voice_camera_stale`, and the browser then stops its capture: a client that resumes after it left, after an operator's stop, after the camera went idle or after a restart is refused. Only a request that names no claim starts a new one, and only the Camera button sends that. The one exception is a room's renewal: when anyone in the room is disabled, hidden or removed, `conchd` renews the room, which ends every claim (`reason=rotated`), and a request naming such a claim, from the same person in the same channel within 30 seconds, is accepted as a new claim (a new `voice_camera_started`). That is how the browser keeps a camera across a renewal without the person pressing anything.
- **The rate bound.** Claims, continues and adoptions together are bounded per person: a burst of 10, refilled at one every 6 seconds. Past it the answer is `429 voice_camera_rate_limited`, with `Retry-After`. Releases are never bounded.
- **Ending.** `DELETE` on the same path ends the caller's own claim (`?started_at=` names it; `204` when there is none; a name that is no longer current is `409 voice_camera_stale`). A claim also ends by itself when the person leaves the room or loses access (a change to `use_camera`, `use_voice` or the voice switch ends it before the change answers), when the room is renewed, or when nothing is sent: 15 seconds after the claim if no camera track was ever seen, 5 seconds after the last one went. Ending a claim takes the permission away again, and LiveKit removes the camera's track for every viewer. Stopping a camera never drops a screen share, and the reverse.
- **The operator's stop.** `DELETE …/voice/camera?principal=<id>` ends someone else's camera. It needs `manage_channels`, which an owner always holds; the operator need not be a member of the channel. **This is stricter than the share stop**: a channel's manager, who may stop a share, may not stop a camera. A member of the channel without `manage_channels` gets `403 permission_denied`, and anyone else without it gets the channel's uniform `404`, so the route never tells a stranger that the channel exists. The person can turn the camera on again; a lasting ban is the voice switch or the permission. An unknown person answers `204`, as a known one with no camera does; `started_at` together with a `principal` naming someone else is `400 invalid_request`. `conch voice camera stop` does this from the command line.
- **Presence and the audit log.** Presence marks a person with a camera `camera` (the roster shows the word *camera* beside *sharing*), and a person's own presence carries `camera_ended` when their claim ends: `reason` (`self`, `operator`, `left`, `access_lost`, `rotated` or `idle`), `started_at` and `ended_at`, until they claim again. It never says who stopped the camera; the audit log does. Every start and stop is in the audit log as `voice_camera_started` and `voice_camera_stopped` with its reason. A camera track nobody claimed, or one whose kind and declared source disagree (a video track declared as the microphone, an audio track declared as the camera), is withdrawn by the next pass and audited as `voice_camera_unclaimed`; a publisher whose track LiveKit will not remove is removed from the room (`voice_participant_removed`, `camera_unclaimed` or `camera_not_withdrawn`). Claims are held in memory: restarting `conchd` drops every claim, and the first pass after it takes the permission back. The browser says "Your camera was turned off."
- **No limit on quality.** `conchd` limits no bitrate or resolution; the browser chooses, and LiveKit forwards.

**In the browser.** While connected to a channel's voice, a person who holds `use_camera` (and is not in *Watch only* or listening only) sees a *Camera* button beside *Mute* and the share. Someone without it has none; **Voice options** says *Camera is off for you on this server.* There is no keyboard shortcut to turn the camera on: an accidental key press must not show someone's face. Turning it off is the same button.

- **Choosing and testing.** **Voice options** has a camera picker under the microphone's, listing the cameras once the browser has named them (after the camera, or its test, was allowed). The choice is kept by device and by name, and is asked for exactly: if the camera chosen is not there, the picker says *Your chosen camera is not connected.* and nothing falls back to another camera or to the browser's default. **Test camera** shows a preview, labelled *Preview: not sent*; it sends nothing, claims nothing, and stops when the options close.
- **Two indicators, from this page's own camera.** *Camera in use* means the browser has the camera but nothing is sent yet; *Sending video* means it is published. Both come from the local track, never from presence, so a laptop whose camera is not capturing never says *Sending video* because the same person's phone is. The browser's own indicator (the tab, the device light) is never the only one. While it starts, the page says *Starting your camera…* and when it stops, why (for example *An operator turned off your camera.*).
- **The self-view and the call view.** Turning your camera on opens the call view on the stage, the place a watched share is drawn, with your own mirrored tile (*You*; *Hide self-view* hides only that tile). **Someone else's camera never opens it**: while anyone has a camera on, the voice controls show *Show video (n)*, and *Hide video* closes the view. Voice in Conch is per channel and the conversation comes first, so another person's camera must not push the chat away or spend a phone's data. Cameras are received only while the view is open.
- **Up to 9.** While at most 9 people are in voice, every person gets a tile; one without a camera shows their initial, their name and *Camera off*. With more than 9 people, only the cameras get tiles. The grid depends only on the number of tiles and the width, and nothing reorders while someone speaks. The speaking person's tile has a ring and the word *speaking*, and the view does not move its focus to them. A tile with a camera is a button, *Enlarge {name}'s video*: it takes the main area and the rest go in a strip, *Back to grid* (or Escape) returns, and *Full screen* works as the share's does.
- **A camera beside a share.** Both can be on at once, by one person or by two. While you watch the share with the call view open, the share is the main picture and the cameras sit in a strip, on its right from 1100 px wide and below it otherwise; enlarging a camera swaps it with the share.
- **Mute and deafen** leave the camera as it is. *Share screen* never starts it, and *Stop sharing* never stops it.
- **What stops a camera.** The capture is released, not muted, so the device light goes off. It stops on *Camera*, on *Leave voice*, on signing out, and on a change of profile, server or voice channel, before anything is done as the new identity or channel. It stops when the server ends the claim for any reason but a renewal, when LiveKit takes the camera away, when another device of the same person takes this one's place (the person is in voice on one device at a time), when the person is removed or the room deleted, and when the device is lost (unplugged, or taken by another app): then the page says *Camera disconnected. Choose a camera in Voice options.* and never turns on another camera. It never starts again by itself after any of these, including a reconnect through `conchd`: only the button does. Across the SDK's own reconnect, or a renewal of the room, the capture is kept (blanked, then published again under the same claim) and the claim is made again by name.
- **Errors say what to do.** The browser blocking the camera: *The browser blocked the camera. Allow it in this site's settings.* No camera: *No camera found.* Taken by another app: *The camera is in use by another app.* A full call: *This call already has 9 cameras on.* Too many presses: *The camera was turned on too often. Wait a moment, then try again.* None of them touches voice or chat.
- **On a phone.** "A phone" here is a window narrower than 700 px, or a touch-only device however wide it is held. It is read when a capture starts and when the page is hidden, never followed by a later resize.
  - Video appears only in the Voice view. Under Chat and Channels the page draws no camera tile and subscribes to no camera, so a phone in Chat receives no video. A phone's own camera keeps sending there.
  - The voice strip stays one row of four controls. While your camera is captured or sent, its state button says *Camera on*, with the camera glyph, and opens Voice, where the Camera button is.
  - The camera and the camera test stop when the page is hidden (the phone locks, or you switch apps). Showing the page again starts nothing; the person taps *Camera*.
  - The capture asks for the front camera first (ideal, not exact), at a smaller size and rate than a computer (below). A camera chosen in the picker is asked for exactly, and kept.

**Quality.** These figures are the SDK's presets, not measurements. `conchd` does not set them; the browser does. VP8, with adaptive stream and dynacast: a sender stops encoding a layer nobody receives, and a hidden tile asks for none.

| | Computer | Phone |
| --- | --- | --- |
| Capture asks for (ideal) | 1280 × 720 at 30 fps | 640 × 360 at 24 fps |
| Top layer, at most | 1.2 Mbps | 450 kbps |
| Smaller layers | 360p at 450 kbps, 180p at 150 kbps | 180p at 150 kbps |

For scale, from the same presets and not measured: in a one-to-one call each side sends at most about 1.8 Mbps across all its layers and receives one layer; in a grid of 9 cameras each person receives 8 small layers and LiveKit sends roughly 9 × 8 streams. **Nothing here claims what a droplet carries.** The measurement of one-to-one and 9-camera calls, with and without a share, is not done yet; [docs/deploy.md](docs/deploy.md#camera-video-and-bandwidth) says how it will be, and the cap of 9 stays a constant until it has been.

**Who can see your camera, and what Conch does with it.**

- Video goes from your browser to LiveKit and from LiveKit to each person watching, encrypted on each hop (DTLS-SRTP).
- LiveKit can see the media, and so can whoever runs it: the owner of the server.
- It is **not end-to-end encrypted**, and nothing in the page suggests it is.
- **Conch records nothing**: no recording, no egress, no frames kept by `conchd`, and no grant ever allows it. Anyone watching can still record their own screen, and nothing can stop that.
- Everyone in the room can receive the cameras in it, as they receive its voice and a share; watching is not a permission. The room is the boundary. Agents do not use voice, so they see no video.

**Guests.** Guest visits are off for now ([Joining and guests](#joining-and-guests-no-guests-for-now)). The rule for when they return (ADR-015, O15) is that a live guest in voice receives cameras and never claims one; today a guest's claim is refused (`403 voice_not_allowed`).

| Browser | Camera | State |
| --- | --- | --- |
| Chromium on Linux, with a fake camera | send and receive | tried in `make web-e2e` (`web/e2e/44-camera.spec.ts`, `45-camera-groups.spec.ts`, `46-camera-phone.spec.ts`), against a real LiveKit |
| Chrome or Edge, with a real camera, on Windows, macOS, Linux | send and receive | untried |
| Firefox, desktop | send and receive | untried |
| Safari, desktop | send and receive | untried |
| iPhone or iPad, Safari | send and receive | untried |
| Android, Chrome | send and receive | untried |

"Untried" means no person has run it yet: not that it fails. No real camera, no phone, no browser other than Chromium and no deployed server has been tried; the by-hand checks are recorded as tested, unsupported or untried in [docs/reports/TEMPLATE-camera-bandwidth.md](docs/reports/TEMPLATE-camera-bandwidth.md)'s report when they are run.

### Verify voice

```sh
go run ./e2e/voice
```

This proves both halves against real processes. The degraded half needs no LiveKit: `conchd` with no LiveKit settings, and with settings and nothing listening, each followed by the approval dogfood. The live half starts the pinned LiveKit image and removes it when done, and joins a headless participant with `lk` 2.19.0 on the token `conchd` issued. The participant joins and is seen, unmutes and mutes and is seen to, an outsider is refused, removing a member who held a session rotates the room and locks that member out, removing one who never had a session changes nothing, and LiveKit is stopped. Then three real `conch-voice` clients, each with a tone for a microphone, join one channel: each is heard by the other two only while it holds its key, every press is in the audit log as a reported pair, a false report is caught, a removed member's client stops while the others move to the new room, and they reconnect by themselves when LiveKit is restarted. A member claims a screen share and an `lk` participant publishes a screen and its sound, which another subscribes to; the share is stopped and its tracks disappear; publishing those without a claim is refused by LiveKit. It checks that no token, secret or room name reaches its output, `conchd`'s log, the audit log or anything a client wrote, and prints one `voice-check: PASS` line. It needs Docker on Linux and network access for the image and `lk`; without them the live half is skipped with one line and exit status 0. The `conch-voice` part also needs the pinned Rust toolchain (`./scripts/voice-toolchain.sh`, once); without it that part alone is skipped and the last line says so. With `CI` set any skip is a failure.

## Diagnostics

What `conchd` tells an operator about a problem, and how a tester's report of one becomes an issue (private alpha A13, issue #446).

**Request ids.** Every answer carries `X-Request-Id`, 16 hex characters, new for every request; a refusal and a socket upgrade have one too. Every log line written for that request ends with `request_id=<id>`, so a tester who quotes the id (the web page records it for every failed call) leads straight to the lines that matter. An `X-Request-Id` the client sends is ignored, so nobody can choose the id of someone else's lines. Error bodies are unchanged.

**What the log holds.** Each socket (`/v0/ws`, `/v1/ws`, `/v2/ws`, `/v1/events/ws`, `/v1/voice/ws`, `/v1/voice/occupancy/ws`) writes one line when it opens and one when it closes:

```text
2026/10/16 09:00:00 INFO ws: socket opened route="GET /v2/ws" principal=12 credential=31 request_id=3f9a0c1d2b4e5f60
2026/10/16 09:05:12 INFO ws: socket closed route="GET /v2/ws" principal=12 credential=31 duration=5m12.004s reason=keepalive_timeout frames=48 request_id=3f9a0c1d2b4e5f60
```

The reason is one of `client`, `server_shutdown`, `keepalive_timeout`, `revoked` (the credential was revoked or expired, or the principal disabled), `removed` (from the channel, or an agent's manifest no longer lets it read there), `slow_consumer`, `session_ended` and `error`. A client that reconnects in a loop does not fill the log: each credential gets five sockets a minute logged in full, and the rest are counted and written as one `ws: sockets summarised` line by the first socket after the minute.

The log never holds a token, a socket ticket, an invite code, a voice room name, a channel name or message text: routes appear as their patterns (`POST /v2/channels/{channel}/messages`), and people and credentials by id.

**`/healthz`.** `GET /healthz` needs no credential and answers `{"status","version","db","voice"}`. `status` and `db` are `ok`, or `degraded` with a `503` when the database cannot be reached. `voice` is `ok` (LiveKit answered the last time it was asked), `unavailable` (configured, but not answering, or not yet asked since the start) or `not_configured`. A voice outage never makes `status` degraded or the answer `503`: messaging, approvals and audit work without it.

**Metrics.** `conchd` can serve Prometheus metrics (issue #562). They are off by default. To turn them on, give a second, loopback address:

```sh
bin/conchd serve --data ./data --metrics-listen 127.0.0.1:9464   # or CONCHD_METRICS_LISTEN=127.0.0.1:9464
curl -s http://127.0.0.1:9464/metrics | grep '^conch_'
```

They are served on that listener only, at `/metrics` and nothing else. The main listener has no `/metrics` (it answers `404`), whatever the settings. The metrics listener has no authentication: loopback is the boundary, as with LiveKit's own `prometheus_port`. The counts say how many people are online, in voice and sharing, so an address that is not loopback (`:9464`, `0.0.0.0:9464`, a public IP or a host name other than `localhost`) stops `conchd` at start unless `--metrics-allow-remote` (or `CONCHD_METRICS_ALLOW_REMOTE=1`) is given as well, and then `conchd` logs a warning. To scrape from another machine, prefer an SSH tunnel or a Prometheus on the same host.

| Metric | Type | Labels |
| --- | --- | --- |
| `conch_http_requests_total` | counter | `route` (the route's pattern, such as `POST /v1/channels/{channel}/messages`, or `unmatched` for any path no route matches), `code` (`1xx` for an upgraded socket, `2xx`, `3xx`, `4xx`, `5xx`) |
| `conch_http_request_duration_seconds` | histogram | `route`. Sockets are counted above but not timed. |
| `conch_ws_connections` | gauge | `kind`: `hub` (`/v0/ws`, `/v1/ws`, `/v2/ws`), `events`, `voice` (`/v1/voice/ws`, `/v1/voice/occupancy/ws`) |
| `conch_ws_disconnects_total` | counter | `reason`: the connection log's reasons (`client`, `server_shutdown`, `keepalive_timeout`, `revoked`, `removed`, `slow_consumer`, `session_ended`, `error`) |
| `conch_principals_online` | gauge | `kind` (`human`, `agent`): principals with a session that is online, not stale |
| `conch_voice_rooms_active`, `conch_voice_participants`, `conch_voice_shares_active` | gauge | none; as voice presence shows them, so zero while LiveKit is not answering |
| `conch_voice_tokens_issued_total` | counter | `result`: `ok`, or `refused` for any other answer |
| `conch_messages_posted_total` | counter | `kind` of the author (`human`, `agent`), over REST and MCP |
| `conch_auth_failures_total` | counter | `reason`: `missing_credential`, `invalid_credential`, `invalid_ticket` |
| `conch_problem_reports_total` | counter | none |
| `conch_build_info` | gauge, always 1 | `version` |

The Go runtime's and the process's own metrics (`go_*`, `process_*`: heap, goroutines, garbage collection, CPU, open files) come with them. No label names a person, a channel, a room or a path: every value is from a fixed list or the route table, so no request can add a series.

**Reporting a problem.** A tester uses "Report a problem" in the web page's header (also offered when the page cannot reach the server) or `conch report`. Both ask the bug template's questions, show exactly what will be sent, and send it only when asked to. The web page adds its version, the browser, the system, the window's size, its voice state and its last 100 diagnostic events, kept in memory only: sockets opened and closed with their codes, reconnect attempts, failed calls with their status, error code and request id, voice connection states and microphone errors by name. The page empties it whenever its sign-in ends (signing out, out everywhere, in another tab, or a `401`), so a shared browser never reports the last person's events. Nothing in them is a token, a ticket, an invite code, a room or channel name or message text; the server refuses an event of any other shape. `conchd` stores the report with its own version, the reporter, the time, the voice health and the reporter's last 20 request ids and socket closes, from memory. A report is at most 32 KiB, a person may send 10 an hour, and each is audited as `problem_reported` with its id alone.

| Route | Who | Success | Refusals |
| --- | --- | --- | --- |
| `POST /v1/reports` | people (an agent gets `403 human_required`; authentication off, `401`) | `201`, `{"id","created_at"}` | `400 invalid_request`, `413 request_too_large`, `429 rate_limited` |
| `GET /v1/reports` | operators | `200`, newest first, without the reports' text; `?limit=` | |
| `GET /v1/reports/{id}` | operators | `200`, the whole report | `404 problem_report_not_found` |
| `PUT /v1/reports/{id}` | operators | `200`, the report marked filed. Body `{"state":"filed","issue":"<url>"}` | `400 invalid_request`, `404 problem_report_not_found` |

**From report to issue.** `conchd` never calls GitHub: the repository is public, and a report names channels, people and devices. An operator files one from their own machine:

```sh
bin/conch admin reports list                    # <id> <open|filed> <time> principal <id> <client> <version> server <version> <often> [issue]
bin/conch admin reports show 7                  # who sent it, then the body
bin/conch admin reports show 7 --markdown       # the issue body alone
ops/report-to-issue.sh 7
```

`--markdown` writes the bug template's sections (what you did, what you expected, what happened instead, version, device, how often, diagnostics). It names no one. The operator's `GET /v1/reports/{id}` answer carries a `redact` list, every channel, net and principal name on the server (never stored with the report, and in no other answer), and every one of them is replaced by `[channel]` or `[person]` in everything the client wrote, whether or not the operator is in that channel; standard error says how many were taken out. Each of the tester's three answers is in a fenced code block longer than any run of backticks in it, so a mention, an issue link or HTML in it stays text. The browser appears as its family and major version (`Firefox 131`); the stored report keeps the full user agent. `ops/report-to-issue.sh` runs it, opens the result in `$EDITOR` to read and trim, refuses anything that still looks like a Conch credential, warns about anything that looks like a web, email or IP address, asks for a title and a yes, and only then runs `gh issue create --label type/bug --body-file …` and `conch admin reports file <id> <issue-url>`. The issue lands in the Project's Backlog with the bug template's label.

## Contributing

Setting up, the checks to run, and how issues, pull requests and reviews work are in [CONTRIBUTING.md](CONTRIBUTING.md). Security reports go privately, as [SECURITY.md](SECURITY.md) describes.

## License

[AGPL-3.0](LICENSE)
