# Conch

**Open-source, self-hosted communications for people and AI agents.**

Conch is being built for groups that need to talk together without everyone talking
over everyone else: gaming squads, international friend groups, and engineering teams.
The goal is dependable push-to-talk, clear control over who hears each conversation,
and AI participants with explicit identities and permissions.

The project started with an agent-native messaging and approval core. It now also has
text nets and an optional LiveKit voice control plane. The full voice client and the
interfaces described below are still in development or design.

> **Pre-alpha.** The working surface today is the Go CLI/TUI and public APIs.
> There is no released graphical client or finished end-user voice client yet.
> Planned features below are product direction, not claims about current availability.

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
| Messaging | Channels, typed messages, CLI/TUI, REST/WebSocket APIs | Richer graphical conversation UI |
| Identity and access | Authentication by default, issued credentials, channel membership, server-enforced agent manifests | Smoother onboarding and administration |
| Scoped conversations | Text nets and whispers, audience-aware delivery, member/monitor roles | Layered voice, independent PTT keys, ducking, stereo placement |
| Voice | Optional LiveKit control plane, session tokens, presence, transmit reporting, CLI status/watch | Native microphone/playback client and real-world PTT reliability |
| Agents | MCP messaging and approvals, capability checks, reference auto-reply bot | Persistent run/task views, richer runtime adapters and intervention |
| Captions and translation | Not implemented as end-user features | Optional live captions, translated captions, then spoken translation |
| Clients | Go CLI/TUI; Rust voice workspace and toolchain scaffold for the accepted Linux/PipeWire design | GUI, Windows native integrations, packaging and cross-platform validation |
| Screen sharing | Not implemented | Sharing through the media integration |

See the [quickstart](#quickstart), [text-net example](#7-nets-and-whispers-the-squad-layout),
and [voice setup](#voice-optional) for the implemented surfaces.

## Client design and P3 planning

The [client experience specification](docs/design/client-experience.md) describes
navigation, voice, nets, agent views and failure states, with
[five visual references](docs/design/mockups/README.md).
[ADR-007](docs/adr/ADR-007-client-interfaces.md) records the UI stack:
Go/Bubble Tea/Lip Gloss TUI, React/TypeScript/Vite browser UI and an Electron desktop
shell sharing that UI, with native Rust voice managed by the client.

Next is [P3 local prototype planning](docs/design/p3-local-prototype.md): one local
server, multiple independently configured TUI clients, a browser and AI participants,
plus optional VM/LAN tests. These documents specify intended work; they are not
installation instructions or claims that the browser/desktop clients already exist.

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
  `conch-bot`.
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

There is no federation or bespoke WebRTC stack. The CLI/TUI remains the implemented
human interface. The [charter](docs/adr/ADR-000-charter.md) and
[ADRs](docs/adr/) record the accepted implementation constraints; the
[roadmap](ROADMAP.md) retains the existing phase gates until explicitly amended.

## Why "Conch"?

A conch is a signal horn and a symbol of speaking rights. The name reflects the goal:
help people and agents know when to speak, whom they are addressing, and who can hear.

## Quickstart

Every command below has been run against real `bin/conchd`/`bin/conch` builds. Paths and IDs are examples — substitute your own.

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

### 2. Create the operator and start `conchd`

`conchd` requires authentication by default, so the first step is an operator — the one principal who can administer the instance:

```sh
umask 077   # everything created in this shell — the data directory and the token file — is readable only by you
mkdir -p /tmp/conch-data
bin/conchd bootstrap-operator --data /tmp/conch-data --name nick > /tmp/conch-data/operator.token
bin/conchd serve --data /tmp/conch-data --listen 127.0.0.1:8080
```

- `bootstrap-operator` works offline on the data directory. It prints the operator's token once, alone on stdout, and refuses to run again once an operator exists. The token can administer the whole instance: the `umask 077` above is what keeps the file it is captured to private, so do not skip it, and never write a token to a file other users can read.
- `--data` (or `CONCHD_DATA`) is required — directory for the embedded SQLite database.
- `--listen` (or `CONCHD_LISTEN`) defaults to `:8080`, which is every interface; the example binds to localhost.
- `--auth` (or `CONCHD_AUTH`) is `required` by default. `--auth off` opens every endpoint to anyone who can reach the port and trusts request bodies for identity; it is for local development only, and `conchd` says so at startup.
- `--max-upload-bytes` (or `CONCHD_MAX_UPLOAD_BYTES`) is the largest file an upload may carry (default 10 MiB; `0` disables uploads). See [Files](#files).
- `--web-dir` (or `CONCHD_WEB_DIR`) serves a built web client (a directory holding `index.html`) at `/app/`, on the API's own origin, and redirects `/` to `/app/`. A path under `/app/` that names no file returns `index.html` (client-side routing); a missing path ending in a file extension is a `404`. There are no directory listings, only `GET` and `HEAD`, nothing outside the directory is reachable, and a file or directory whose name starts with a dot is never served. `/app/` needs no credential (the page must load before sign-in); the API it calls still does. Every `/app/` response carries a strict Content-Security-Policy. Unset, there is no `/app/`. `conchd` refuses to start if the directory is missing or has no `index.html`. The build is not embedded in the binary. Whatever is in the directory runs as code on `conchd`'s own origin, so only the operator who runs `conchd` should be able to write to it.
- `--allowed-origins` (or `CONCHD_ALLOWED_ORIGINS`) is a comma-separated list of extra `Origin`s the WebSocket upgrade accepts, such as `http://localhost:5173`. It exists only so a Vite dev server on another port can connect. Each entry is one exact origin (`http` or `https`, a host, an optional port); wildcards are refused at start-up, and so are empty entries. Unset, the same-origin check is unchanged.
- `--session-stale` (or `CONCHD_SESSION_STALE`, default `30s`) and `--session-offline` (or `CONCHD_SESSION_OFFLINE`, default `90s`) are how long a session may go without a heartbeat before it shows as `stale` and then as `offline`; `conchd` refuses to start unless stale is shorter than offline. A session is one live client or runtime of a principal (a TUI, a browser tab, an adapter): `POST /v1/sessions` creates one (any caller with a credential; an agent needs `sessions.manage`; at most 16 online or stale per credential, then `429 too_many_sessions`; offline ones do not count, and reviving one is held to the same bound), `POST /v1/sessions/{id}/heartbeat` keeps it alive and revives a stale or offline one, `DELETE /v1/sessions/{id}` ends it, and `GET /v1/sessions?channel=<name>` lists the sessions of the channel's members, for members only (a non-member, an operator who is not a member and an unknown channel all get the same `404 channel_not_found`). A session belongs to the credential that created it: anyone else, another credential of the same principal and operators included, gets `404 session_not_found`. Revoking, rotating or revoking all of a credential, or disabling a principal, ends its sessions, and so does a credential's expiry (at the next sweep, with cause `credential_expired`). Rows are never deleted. Sessions need authentication: with `--auth off` the routes answer `400 sessions_require_auth`. A roster shows `runtime` and `host` only to an operator and to the session's own principal.

  A run is one piece of work an agent session reports, in one channel and optionally answering one message there. `POST /v1/sessions/{id}/runs` with `{"channel":"<name>","message_id":<id>,"task":"<one line>","link":"https://…"}` starts one in state `queued` (agents only, a human gets `403 agent_required`; on a session of the caller's own credential, otherwise `404 session_not_found`). The agent needs `runs.manage` and, in the channel, the permission posting to the run's audience needs: `post` for a run that answers nothing or a channel-wide message, `post_net` for a net message, `whisper` for a whisper (and `whisper_agent` when another agent is in it). The message must be in that channel and visible to the agent. Every one of those refusals is the same `404 channel_not_found` an unknown channel gets. `POST /v1/runs/{id}/state` with `{"state":…,"outcome":…}` moves it along the fixed lifecycle (`409 invalid_transition` otherwise; a terminal state sets `ended_at`), and `POST /v1/runs/{id}/activity` with `{"kind":…,"summary":…,"outcome":…,"data":…}` adds an entry (`409` once the run has ended). Only the credential that owns the run's session may do either; anyone else, whatever they hold, gets `404 run_not_found`. Each report re-checks the manifest and spends `runs.manage`. `GET /v1/runs/{id}`, `GET /v1/runs/{id}/activity?after=&limit=` and `GET /v1/runs?channel=<name>&session=&after=&limit=` read them. The run list is ascending by id by default (`order=asc`, paged with `after`); `order=desc` lists the newest first, from the newest run with no `before` and below `before=<id>` otherwise, so a client pages back by passing the smallest id it holds (a newest-first page has no `next_after`). `after` goes only with `asc` and `before` only with `desc`; any other mix, both together included, is `400 invalid_request`, as is an `order` that is neither, and an empty `after=`, `before=` or `order=` is as if absent. Either way a run you may not see neither fills a page nor moves the cursor. A run that answers a message is visible to exactly the people who can see that message, and one that answers nothing to the channel's members; its activity goes with it. Operators get no exemption, and a run you may not see answers `404 run_not_found` like one that does not exist. The roster shows every agent session's `status` (`running`, `blocked`, `waiting`, `failed` or `idle`, from its runs) to all its readers, and its `current_run` and `task` only to those who may see that run; `outcome_unknown` is true while a session with a run open is `stale` or `offline`. Nothing changes a run because its session went quiet or ended. `run_started` and `run_ended` are audited; activity is not.

  An intervention is a person asking an agent's run to take a message or to stop. `POST /v1/runs/{id}/interventions` with `{"kind":"message","body":"…"}` (a message needs a `body`; `cancel`, `pause` and `resume` take none) records one in state `requested`. Any human who can see the run may ask (the people who could prompt the agent by posting where it listens); operators get no exemption, and an agent gets `403 human_required`. A run you may not see answers `404 run_not_found`, like one that does not exist. A run that has ended, or whose session has, answers `409 invalid_transition`; if the run's session did not declare the kind the answer is `409 intervention_unsupported` and nothing is stored (the refusal is audited as `intervention_unsupported`); a `body` or `reason` may hold newlines and tabs but no other control character; at most 16 interventions wait (`requested`) on one run, and the next is `429 too_many_interventions`. `GET /v1/runs/{id}/interventions?after=&limit=` and `GET /v1/interventions/{id}` read them, for the same people who can see the run (an agent also needs `messages.read` and `read` in the channel). The agent's adapter polls `GET /v1/sessions/{id}/interventions?state=requested&after=&limit=` (only the credential that owns the session; it needs `runs.manage` and spends it; `state` is optional and `after` is the last id seen) and answers with `POST /v1/interventions/{id}/ack` and `{"state":"acknowledged"|"applied"|"failed","reason":"…"}` (only the credential that owns the run's session, otherwise `404 intervention_not_found`; `requested` becomes any answer, `acknowledged` becomes `applied` or `failed`, anything else is `409 invalid_transition`; `failed` needs a `reason`). An intervention never changes the run: an applied `cancel` is the adapter saying it stopped the work, and the run is `cancelled` only when the adapter also reports that on `POST /v1/runs/{id}/state`. `intervention_requested` (the person, the run and the kind) and `intervention_answered` (the state) are audited, and neither holds the instruction or the reason.

  `GET /v1/events/ws?channel=<name>[&session=<id>]` is how a client follows all of this live, without polling. It takes a bearer or a ticket, and sends one `pkg/schema` document per text frame, told apart by `schema`: a `SessionRosterV1` for the channel on connect and again whenever a session of a member is created, ended, changes state or status, or the channel's membership changes (including the sweeper marking a session `stale` or `offline`; a roster identical to the last one sent is not sent again); a `RunV1` when a run in the channel that you may see is created or changes state; an `AgentActivityV1` when one is appended to such a run; and an `InterventionV1` when one on such a run is created or answered. Every frame is built for you when it is sent, by what the REST routes use, so you are never sent a document you could not fetch now: a channel member outside a whisper gets no frame about a run that answers it, and its roster shows no `current_run` or `task` for it. The upgrade asks what `GET /v1/sessions?channel=` asks (a non-member and an unknown channel get the same `404 channel_not_found`; an agent needs `messages.read` and `read` in the channel, spent once, as for `/v2/ws`, and asked again without spending before every batch of frames), and `--auth off` answers `400 sessions_require_auth`. With `&session=<id>` the session must be your credential's and not ended, otherwise `404 session_not_found` (an agent also needs `sessions.manage`); the server then refreshes its `last_seen_at` every 10 seconds while the socket is open, as a heartbeat would, so a TUI or a browser tab stays `online` without calling one. If that session is ended while the socket is open (`DELETE /v1/sessions/{id}`, or the sweeper ending an expired credential's), the next refresh, within 10 seconds, closes the socket with 1008 `session ended`, and a reconnect with that `session` is refused `404 session_not_found`; a socket without `&session=` is not affected. Membership or credential loss closes the socket with 1008 (at once for removal from the channel, a disable, a revoke-all and a new manifest, within the 30 s re-check otherwise); a reader too slow to keep up is closed with 1008 `subscriber too slow` and reconnects to get a fresh roster. One socket rebuilds its roster at most every 250 ms: changes in between are sent together, as the roster after them, so a `RunV1` may arrive up to 250 ms before the roster showing its status. There is no replay: after a reconnect, take the roster and read runs over REST.
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
# credential 2 does not expire                (on stderr; --expires-at <RFC 3339> sets an expiry)

# channel membership decides who can see a channel at all
bin/conch admin member add ops 2
bin/conch admin member add ops 3
```

`conch admin` is a plain client of the REST API (every command names one route), so everything it does can still be done with `curl`. `--json` prints the server's response document instead of the one-line result. The token files are only as private as the `umask 077` at the top of that block makes them, and it has to be run in this shell, not only in the one that started `conchd`; never write a token anywhere other users can read.

A principal who is not a member of a channel cannot read it, post to it, subscribe to it, or see it listed; to them it does not exist. Operators manage membership but get no exemption. The other `conch admin` commands: `credential list <principal-id>`, `credential revoke <credential-id>`, `principal disable|enable <id>` (switch a principal off entirely, and back on), and `member remove <channel> <principal-id>`. Rotating a credential (`POST /v1/credentials/{id}/rotate`) is REST only.

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

Then `conch` with no arguments launches the TUI (needs a real terminal). It lists the channels you are a member of and shows who you are signed in as. The TUI must be the terminal's foreground job: started in the background (`conch &`, `timeout` without `--foreground`, a supervisor that does not hand over the terminal) it refuses with one line and a non-zero exit instead of being stopped silently by the kernel — use a plain subcommand there instead.

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

`conch search <channel> <words…>` finds messages in one channel (the REST route is `GET /v2/channels/{channel}/messages/search?q=<words>&before=<id>&limit=<n>`). A message matches when it contains every word, in any order, ignoring case and accents; words are whole words, never a query language (`*`, `-`, `OR`, quotes and the like are just text). Results are newest first, in the format `tail` prints, and page back with `before` (`next_before` in the response; the CLI prints the `--before` value to use on stderr). You are shown exactly what listing the channel shows you: a net message or whisper you cannot see is not found, and nothing in the response tells you one exists. No match prints nothing and exits 0. `q` is required and at most 256 bytes; `limit` is as for listing.

`GET /v2/channels/{channel}/messages` (and `/v1`) is ascending by id by default (`order=asc`, paged with `after`, `next_after` in the response). `order=desc` lists the newest first: with no `before` it is the newest page, and a client pages back by passing `before=<id>` the smallest id it holds, so opening a channel is one request, `?order=desc&limit=100`. A newest-first page has no `next_after`. `after` goes only with `asc` and `before` only with `desc`; any other mix, both together included, an unknown `order`, and `order=desc` with `thread`, is `400 invalid_request`, and an empty value is no value. The audience rule applies before the limit in both directions, so a message you cannot see neither fills a page nor moves the cursor. `/v0` has no `order`.

#### Threads

`conch send --reply-to <id> <channel> <text>` replies to message `<id>` (REST: `POST /v2/channels/{channel}/messages` with `"thread_root_id": <id>`). Threads are one level: a reply to a reply is refused. **A reply has its root's audience and cannot choose its own**: it reaches only people who can see the message it answers, so a reply to a whisper is a whisper to the same people (whoever of them replies; the author of the whisper sees it), a reply on a net goes to those of the root's recipients who are still on the net (someone added to the net later does not get replies to a message they cannot see, and someone taken off it gets no more of its talk through an old thread), and `--reply-to` cannot be combined with `--net` or `--to` (the API refuses `thread_root_id` together with `audience`: `400 thread_audience_conflict`). A message you cannot see, one that does not exist, one in another channel and a reply all give the same `404 thread_not_found`, byte for byte, so a reply cannot be used to find out that a scoped message exists. `GET /v2/channels/{channel}/messages?thread=<id>` (the CLI's `conch thread <channel> <id>`) returns the root and then its replies, in order, with the usual `after` and `limit`, showing you exactly what the channel listing shows you; a non-numeric or zero `thread` is a `400`. The channel listing and the v2 WebSocket still deliver replies in id order like any message, each with `thread_root_id` set, and `conch tail` marks one `[reply:<id>]`. v0 and v1 clients see a reply to a channel-wide message as an ordinary message and never see a scoped one. Threads are not yet available through MCP, and the TUI does not draw them.

`conch voice status <channel>` shows who is connected to the channel's voice and who is transmitting, then exits; `--watch` keeps following and prints a `--- <time>` line and the new state on every change until you interrupt it. It only reads presence; it never joins voice. Each participant is one line, in principal id order: `<id> <name|-> <talking|quiet> <joined-at>` (an RFC 3339 UTC time), for example `3 nick talking 2026-10-09T08:30:00Z`. The API gives a member only their own name, so other participants show as `-`; a name with spaces or unusual characters is double-quoted and escaped. An empty room prints `nobody is connected to voice in <channel>` and exits 0. Voice not configured, or configured but unreachable, prints one line and exits nonzero (under `--watch`, an unreachable voice server is printed and the watch carries on).

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

The TUI shows who and what is connected. While it runs it holds a session of its own (`POST /v1/sessions` with `client: "tui"`, `DELETE`d when it quits, `SIGINT` and `SIGTERM` included) and follows the events socket (`/v1/events/ws?channel=&session=`) of the channel on screen, reopened when you switch channel. The members line above the input reads `online: nick(2) guest · agents: worker-test ● observer-test ○`: people with a live session (a number after the name when one person has several), then agents with a session (`●` running, idle, blocked, waiting or failed; `◐` stale; `○` offline). Members with no session are not listed there. `tab` cycles channels, inbox and agents. The Agents view lists each agent member of the channel once, with its status as the server derives it (`idle`, `running`, `blocked`, `waiting`, `failed`, `stale`, `offline`; an agent with no session is `offline`), its current task and when it was last heard from; `enter` unfolds its sessions with their runtime and model, and a stale or offline session with a run still open says `outcome unknown`. Names, tasks, runtimes and models are chosen by other people and agents, so they are printed as text with control characters dropped. When the events socket is down or refused, or the TUI's session could not be created (a server with `--auth off`), both views read `unavailable` rather than showing a roster that may be stale, messaging keeps working, and the socket is retried with the same backoff as the message stream (1 s, doubling to 30 s); a roster is never replayed, so a reconnect takes the next one as the state. When the server has ended the TUI's session (a revoke, a rotate, an expired credential) the socket is refused with `404 session_not_found`: the TUI then creates one new session and reopens the socket with it, and if that fails the views stay `unavailable` and the status line says why (for example `members: session ended; sign in again`). The message stream follows `/v2/ws?after=<last id held>`: after a drop it asks for exactly what it missed, with no reload of the channel. Below 80x24 the Agents view is one pane; below 40x10 the TUI asks for a larger terminal.

In the Agents view, `↑`/`↓` also walk through an unfolded agent's sessions and each session's ten newest runs (read with one `GET /v1/runs?channel=&session=&order=desc&limit=10` per session when the agent is unfolded). `enter` on a session with a run open, or on a run, opens run detail: the run's id, state, task, link, start and end times and outcome, then its activity (read in pages, then live from the events socket; `↑`/`↓`/`pgup`/`pgdn` scroll it) and the history of interventions asked of it. `esc` goes back. `m` types a one-line message to the run (`enter` sends it, `esc` drops it; it is one line, and control characters other than tab are refused) and `x` asks `cancel this run? y/n`; a key is offered only for a kind the session's roster entry declares in `interventions`, and the rest read `(unsupported)` (`pause (unsupported)` and `resume (unsupported)` always: this client does not send them) and do nothing. A request shows as `requested…` and becomes `acknowledged`, `applied` or `failed: <reason>` only when the intervention's frame says so, and the run's state changes only on a run frame or a re-read: an applied cancel leaves the run reading `running` until the agent reports it `cancelled`. A run that has ended shows its outcome and no keys; a stale or offline session's open run says `outcome unknown` beside its state. A run you can no longer see closes the pane with one line. Every field is text written by other people or agents and is printed with control characters dropped. After the events socket reconnects the open run is read again, since nothing is replayed.

- `CONCH_SERVER` (or `--server`) — conchd URL (default `http://127.0.0.1:8080`).
- `CONCH_TOKEN` — a token to use instead of the stored login, for scripts and CI.
- `CONCH_CHANNELS` — optional comma-separated override for which channels the TUI opens.
- `CONCH_MOUSE` — the TUI takes mouse input (click a channel or approval to select it, wheel to scroll messages) unless this is `0`. While it does, the terminal's own text selection needs Shift held (Shift+drag); `CONCH_MOUSE=0` gives it back. Under tmux, `set -g mouse on` is needed for events to reach the TUI. The keyboard does everything the mouse does, and a click never sends, decides or quits.
- `--author` / `CONCH_AUTHOR` are deprecated: identity comes from your login. They still work against a server running `--auth off`.

#### The live stream and reconnecting

`GET /v2/ws?channel=<name>` is the live stream: one JSON `MessageV2` per text frame, for every message posted after you connected. Add `&after=<id>` to be given the history first, gap-free: the server subscribes you, then sends every message you may see with an id greater than `<id>`, in ascending order, then carries on live. Connect with the id of the last message you have, and a message posted while you were reconnecting is not lost; `after=0` replays everything you may see, and without `after` nothing is replayed and the stream is exactly as before. The value must be a non-negative integer or the request is refused with `400 invalid_request` before any upgrade. Only `/v2/ws` has it (`/v1/ws` and `/v0/ws` cannot express an audience and ignore it).

**Deduplicate by `id`.** On one connection each message arrives once: the replay is in id order, and a message posted while it is running is sent in its place, not twice. Live messages are sent as they are broadcast, which is id order except when two are posted at the same instant. Across connections the same message can arrive again, for example when you reconnect with an older id than the newest you showed, so a client treats `id` as the identity of a message and ignores a frame whose id it already has. A replay shows you what `GET /v2/channels/{channel}/messages` shows you, so a net message or whisper outside your audience is not sent, and removal from the channel ends the stream as it does for a live one. If the subscription's buffer of new messages fills while a long replay is still being sent, the connection is closed as too slow (policy violation, reason `subscriber too slow`); reconnect with the newest id you received.

#### Open sockets per credential

A credential may hold at most **32** WebSockets open at once, counted across `/v0/ws`, `/v1/ws`, `/v2/ws`, `/v1/events/ws` and `/v1/voice/ws` together. The next upgrade is refused before it is upgraded with `429` and `{"code":"too_many_sockets"}`; close one and the next is let in. The count is per credential, so a second credential of the same principal has its own 32, and a socket opened with a ticket counts against the credential that asked for the ticket. It is held in memory, so a restart forgets it (and every socket with it), and with `--auth off` there is no credential and no cap. The limit is a constant, not a flag.

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
- It does exactly what the credential that asked for it could do at the moment it is spent, as that principal. If the credential has been revoked or has expired, or its principal disabled, in the meantime, the ticket is refused. A socket opened with a ticket is re-checked every 30 seconds like any other and closed (policy violation, 1008) when its credential is revoked.
- Only `GET /v2/ws` and `GET /v1/events/ws` accept a ticket (`/v1/ws`, `/v0/ws` and every other route ignore the parameter and want a header), and only when the request carries no `Authorization` header: with a header, the header is used and the ticket is left unspent. Any refusal (missing, used, expired, unknown, or revoked credential) is the same `401`.
- A credential may hold at most 8 unspent tickets; the next request is `429 too_many_tickets`. Tickets live in memory, so a restart forgets them. With `--auth off` there is no credential to bind one to and the route answers `400 tickets_require_auth`.
- A ticket is a secret for its 30 seconds. `conchd` does not log request URLs, and a ticket is never logged or audited. A reverse proxy in front of it may log the URL: that is tolerable for a value that is dead after one use, and is exactly why the bearer must not be there.

### Optional: auto-reply bot

`conch-bot` watches one channel and replies to new human messages using the
local `claude -p` command. Give it its agent's token and principal ID:

```sh
CONCH_BOT_TOKEN=<the agent's token> \
CONCH_BOT_PRINCIPAL_ID=3 \
CONCH_BOT_CHANNEL=ops \
bin/conch-bot
```

It skips messages already present when it starts and ignores its own replies.
It replies in kind: an answer to a net message goes to that net, an answer to
a whisper goes to the same principals, and an answer to a channel-wide message
is channel-wide. To reply on a net or to a whisper its manifest needs
`post_net` or `whisper` (plus `whisper_agent` for a whisper that includes
another agent). If the server refuses a scoped reply, the bot logs one line and
posts nothing; it never falls back to a wider audience. It does not start
scoped conversations.
Optional settings include `CONCH_BOT_SERVER`, `CONCH_BOT_POLL_INTERVAL`,
`CONCH_BOT_MAX_BACKOFF`, `CONCH_BOT_CONTEXT_MESSAGES`, `CONCH_BOT_MODEL`,
`CONCH_BOT_REPLY_TIMEOUT`, `CLAUDE_BIN`, and `CONCH_BOT_LOCK_FILE`. Like any
agent it needs channel membership and a manifest (next section) allowing
`messages.read` and `messages.post`.

### 5. Agent side: MCP

Agents connect to `POST /mcp` (streamable HTTP) with `Authorization: Bearer <token>` — the agent's token from step 3. Thirteen tools are registered. A request to `/mcp` may be at most 1 MiB, the same as a REST post; a larger one is refused with `413 request_too_large` and does nothing.

- `post_message` — post a message to a channel as the authenticated agent. With an `audience` (`{"kind":"net","net_id":N}` or `{"kind":"principals","principal_ids":[…]}`) it goes only to that net or those principals; the agent's manifest must grant `post_net`, or `whisper` (plus `whisper_agent` to reach another agent).
- `read_channel` — read one paginated page of messages from a channel: channel-wide ones, and scoped ones the agent is a recipient of. A message with an `audience` was not sent to everyone; reply in kind by sending the same `audience` back.
- `request_approval` — raise an approval as the authenticated agent.
- `await_decision` — block until an approval resolves (`timeout_ms`, clamped to a 60s server-side max).
- `check_decision` — read an approval's current state/resolution immediately, without blocking.

An adapter, or an agent's own runtime, reports its presence and its work with eight more (issue #267). Each is the REST route of the same purpose (below) over MCP: it takes and returns the same `pkg/schema` documents, allows and refuses the same requests with the same error codes, and spends the same budget. They need an issued credential, because a session belongs to one; a deprecated static `--mcp-token` gets `sessions_require_auth`. Two differences come from MCP itself. Arguments the tool's input schema refuses (a missing required field, an unknown one, a wrong type) are refused by the MCP library with a plain-text error, before the manifest is consulted, where REST answers `400 invalid_request`. And the library decodes and re-encodes every call's arguments, so `post_activity`'s `data` is stored with its object keys sorted and its numbers as float64 (an integer above 2^53 loses precision), where REST stores the JSON as sent.

- `register_session`, `heartbeat`, `end_session` (`sessions.manage`) — `POST /v1/sessions`, `POST /v1/sessions/{id}/heartbeat`, `DELETE /v1/sessions/{id}`.
- `start_run`, `update_run`, `post_activity` (`runs.manage`) — `POST /v1/sessions/{id}/runs`, `POST /v1/runs/{id}/state`, `POST /v1/runs/{id}/activity`.
- `poll_interventions`, `ack_intervention` (`runs.manage`) — `GET /v1/sessions/{id}/interventions` and `POST /v1/interventions/{id}/ack`.

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

Capabilities are `messages.read`, `messages.post`, `approvals.request`, `approvals.await`, `approvals.check`, `sessions.manage` (register, heartbeat and end the agent's own sessions) and `runs.manage` (report runs and activity, and read and answer interventions on its own runs); each is granted separately. The last two are P3 additions, used by the session and run tools and routes. Without membership a channel looks like it does not exist; with membership but no grant the call returns `forbidden`. Every refusal is written to the audit log (a caller repeating the same denial gets a row for each of the first eight in a minute and then only for the 16th, 32nd… with its number, not one per request for ever). A manifest may also cap a capability with `rate_limits`; over the cap the call is refused with `429` and `Retry-After` (REST, and a webhook bound to the agent) or the `rate_limited` error (MCP). The counters are in memory, so a restart starts every window afresh. Details: [docs/design/agent-manifest.md](docs/design/agent-manifest.md).

A raw JSON-RPC example (most agents will instead use an MCP client SDK):

```sh
curl -s -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H "Authorization: Bearer $(cat deploy-bot.token)" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
        "name":"post_message","arguments":{"channel":"ops","body":"release 42 is built"}}}'
```

Approvals are requested by agents over MCP and decided by humans with `conch approve` / `conch reject`; the decider is always the logged-in person and must be a member of the approval's channel.

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
- **Agents are deny-by-default.** An agent that existed before has no manifest and can do nothing until you write one; `conchd` logs how many such agents there are at startup. The static `--mcp-token token=principal_id` flag still works but is deprecated in favour of issued credentials.

### Known limitations

- Tokens travel in the `Authorization` header in the clear unless you put TLS in front of `conchd`; keep it on localhost or a VPN, or behind a TLS-terminating reverse proxy.
- `conch login` does not echo a token typed at its prompt. Piping it from a file, as shown, also keeps it out of your clipboard and scrollback.
- A webhook hook URL is a posting credential. List hooks with `GET /v1/hooks` and revoke one with `DELETE /v1/hooks/{id}` (operator only); only a hash of each token is stored. The database file is created owner-only (0600) and an existing looser one is tightened at startup.

## Voice (optional)

Voice adds the first optional *server* process to a Conch deployment: a [LiveKit](https://livekit.io) server running beside `conchd`. LiveKit carries the audio. `conchd` decides who may join a channel's voice room, who may speak and who is shown, and records it ([ADR-004](docs/adr/ADR-004-voice-via-livekit.md), [design note](docs/design/voice-control-plane.md)). `conchd` never touches audio, and the `conch` CLI does not join voice yet; today it reads who is connected (`conch voice status`).

Everything else is unaffected by voice. With no LiveKit settings, messaging, approvals and audit work exactly as before and the voice endpoints answer `voice_not_configured`. With settings but LiveKit stopped or unreachable they work too, and the voice endpoints answer `voice_unavailable`: the session endpoint refuses (503), presence says `available: false`, `conch voice status` prints one line and exits nonzero, and `conchd` writes one `voice_enforcement_unavailable` audit row per outage. `conchd` looks at LiveKit once when it starts but does not wait for it, so it starts and stays up whether or not LiveKit does, and voice recovers by itself when LiveKit returns. While `conchd` cannot reach LiveKit it cannot remove anyone from a room: people who are connected stay connected, and nobody is removed by name until it can reach LiveKit again (a room that must be abandoned is still retired in `conchd`'s own records at once, so no new session names it).

### Run LiveKit beside `conchd`

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
- **Clocks.** A join token lives 15 seconds and LiveKit allows a minute of leeway either way. If `conchd` and LiveKit run on different machines, keep their clocks synchronised.
- **Ports and TLS.** Clients need LiveKit's signalling port (7880 here) and its media ports (TCP 7881 and the UDP range). `bind_addresses` covers the signalling port only: **the media ports listen on every interface of the host** (measured with this configuration: `127.0.0.1:7880`, `*:7881`). Nothing can be done on them without a session negotiated over the signalling port, but if this host is reachable from networks it should not serve, close them in its firewall. Put TLS in front of the signalling port for anything beyond localhost, and see LiveKit's deployment documentation for public addresses; the example is a single-host setup.

### Look at it

A member asks `conchd` for a session and is given the LiveKit address, their identity and a short-lived join token for the channel's room. `conch voice status` shows who is connected:

```sh
curl -s -X POST localhost:8080/v1/channels/ops/voice/session -H @alice.header -o /dev/null -w '%{http_code}\n'
# 200
bin/conch voice status ops
# nobody is connected to voice in ops
```

`alice.header` holds the line `Authorization: Bearer <alice's token>` (as in step 3, kept in a private file so the token is not in the process list). If you run `conch voice status` within a second or two of starting `conchd` and LiveKit together, it can say "voice is unavailable" until `conchd` has looked at LiveKit once; run it again. Joining is a client's job; until `conch-voice` exists, `go run ./e2e/voice` joins a headless participant. Stop LiveKit with `docker rm -f livekit` and the same two commands print `503` (the body says `voice_unavailable`) and "voice is unavailable", the second exiting nonzero. If a `go run ./e2e/voice` is killed before it can clean up, remove what it left with `docker ps -aq --filter label=conch-voice-check | xargs -r docker rm -f`; that removes every voice-check container on the host, so do not run it where another run is in progress.

### Verify voice

```sh
go run ./e2e/voice
```

This proves both halves against real processes. The degraded half needs no LiveKit: `conchd` with no LiveKit settings, and with settings and nothing listening, each followed by the approval dogfood. The live half starts the pinned LiveKit image and removes it when done, and joins a headless participant with `lk` 2.19.0 on the token `conchd` issued. The participant joins and is seen, unmutes and mutes and is seen to, an outsider is refused, removing a member who held a session rotates the room and locks that member out, removing one who never had a session changes nothing, and LiveKit is stopped. Then three real `conch-voice` clients, each with a tone for a microphone, join one channel: each is heard by the other two only while it holds its key, every press is in the audit log as a reported pair, a false report is caught, a removed member's client stops while the others move to the new room, and they reconnect by themselves when LiveKit is restarted. It checks that no token, secret or room name reaches its output, `conchd`'s log, the audit log or anything a client wrote, and prints one `voice-check: PASS` line. It needs Docker on Linux and network access for the image and `lk`; without them the live half is skipped with one line and exit status 0. The `conch-voice` part also needs the pinned Rust toolchain (`./scripts/voice-toolchain.sh`, once); without it that part alone is skipped and the last line says so. With `CI` set any skip is a failure.

## License

[AGPL-3.0](LICENSE)
