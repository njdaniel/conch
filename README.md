<p align="center">
  <img src="docs/assets/conch-logo.png" alt="Conch: voice and chat" width="320">
</p>

<p align="center">
  <strong>Open-source, self-hosted communications for people and AI agents.</strong><br>
  Chat, voice and screen sharing with explicit audiences, governed agents and an auditable core.
</p>

<p align="center">
  <a href="https://github.com/njdaniel/conch/actions/workflows/ci.yml"><img src="https://github.com/njdaniel/conch/actions/workflows/ci.yml/badge.svg" alt="CI status"></a>
</p>

<p align="center">
  <a href="#quickstart">Quickstart</a> ·
  <a href="docs/manual.md">Manual</a> ·
  <a href="docs/deploy.md">Deploy</a> ·
  <a href="ROADMAP.md">Roadmap</a> ·
  <a href="CONTRIBUTING.md">Contributing</a> ·
  <a href="SECURITY.md">Security</a>
</p>

## What Conch is

Conch gives small communities and engineering teams one place to talk, coordinate and work with AI agents. You run the server, keep your data, and choose who can read, speak or act.

People and agents share one conversation log, but they are not the same kind of principal. An agent has its own identity, scoped permissions, visible runs and structured approval requests. A person decides.

- **Chat with context.** Channels, threads, files, search, unread state and typed messages, in the browser, the TUI, the CLI, REST and WebSocket.
- **Keep audiences explicit.** Text nets and whispers let a lead coordinate across groups without telling everyone everything.
- **Talk and share.** Browser voice, screen sharing and webcam, and a native Linux/PipeWire `conch-voice` client, through optional LiveKit media.
- **See what agents do.** Sessions, runs, activity and supported message, cancel, pause and resume interventions.
- **Ask before acting.** Agents request approvals over MCP. A person decides with a reason, and the request, the decision and the outcome are audited.
- **Run it yourself.** One Go server with embedded SQLite handles messaging, identity, approvals and audit. It needs no other service.

## Status

Conch is in a **private alpha** (latest tag: `v0.1.0-alpha.15`). The API and the wire shapes can still change between alphas. Read the [CHANGELOG](CHANGELOG.md) before you upgrade.

| Area | Works today | Partial or still being validated | Planned |
| --- | --- | --- | --- |
| Messaging | Channels, threads, files, search, unread state, nets, whispers, channel archive | Wider use by real groups | Richer conversation features as use earns them |
| Browser | Chat, invites, sign-in, people and agent views, settings, diagnostics | Browser and device coverage | UI polish and mobile work ([roadmap](ROADMAP.md)) |
| Voice and sharing | Browser voice (push-to-talk, open mic, watch only), screen sharing with audio where the browser allows it, companion share audio, webcam | Real-device and network reliability | Layered voice nets, captions, translation |
| Native voice | `conch-voice` for Linux/PipeWire, with native push-to-talk; voice from inside the TUI (`/voice join`) | Other desktops | Windows and other platforms |
| Agents | MCP messaging and approvals, sessions, runs, activity, interventions, reference adapters, AI administrators (an agent an owner lets administer the server, with approvals gating the risky actions) | More runtimes | Richer orchestration |
| Deployment | Single-server core, embedded browser client, invites, audit, diagnostics, a deployment runbook | Upgrade and production evidence from the alpha | Hosted multi-tenancy and federation are not planned |

The [roadmap](ROADMAP.md) separates shipped work, the current alpha and later direction. Captions and translation are not current features.

## Architecture

```text
Browser ─────────┐
conch TUI / CLI ─┤
conch-voice ─────┼── REST / WebSockets / MCP ── conchd ── embedded SQLite
Agents (MCP) ────┘                                 │
                                                   └── optional LiveKit
                                                       voice and screen media
```

- **`conchd` is the core.** It owns identity, membership, messages, delivery, sessions, runs, approvals and audit. Messaging and approvals keep working when LiveKit is absent.
- **Clients use public contracts.** The browser and terminal use REST and WebSockets. Agents use MCP, under the same server-enforced permissions. Wire shapes come from `pkg/schema`.
- **Media is optional.** LiveKit carries audio and screen tracks as a separate integration.
- **One server, one community.** There is no hosted multi-tenancy and no federation.

The boundaries are in [ADR-001](docs/adr/ADR-001-mcp-native-architecture.md), [ADR-002](docs/adr/ADR-002-single-binary-sqlite.md) and the other [ADRs](docs/adr/).

## Quickstart

The full local demo starts `conchd`, the browser client and LiveKit, then makes an operator and a channel. It needs [Go](https://go.dev), Docker and the Node version in [`web/.nvmrc`](web/.nvmrc) for the first browser build. `scripts/dev-stack.sh --help` lists the other tools and settings.

```sh
git clone https://github.com/njdaniel/conch.git
cd conch
scripts/dev-stack.sh up
scripts/dev-stack.sh token
```

Open **http://127.0.0.1:28080/app/** and paste the credential that `token` printed. Keep it private. For a second person, run `scripts/dev-stack.sh invite`.

The state lives in `~/.cache/conch-dev`, outside the checkout. To finish:

```sh
scripts/dev-stack.sh down          # stop the demo
scripts/dev-stack.sh down --wipe   # stop it and delete its state
```

To build only the core (Go is enough; Node, Rust, Docker and LiveKit are not needed for messaging and approvals):

```sh
make build   # bin/conchd, bin/conch, bin/conch-adapter, bin/conch-bot
```

Then follow [create the operator and start `conchd`](docs/manual.md#2-create-the-operator-and-start-conchd) in the manual.

## Documentation

| Document | What it covers |
| --- | --- |
| [Manual](docs/manual.md) | Setup step by step, the CLI and TUI, MCP, AI administrators, adapters, nets, invites, TLS, voice, screen sharing, diagnostics |
| [Deployment runbook](docs/deploy.md) | A public server with Caddy, LiveKit, systemd, backups, upgrade and rollback |
| [Alpha tester guide](docs/alpha-tester-guide.md) | Joining and using Conch as an invited tester |
| [Linux desktop app](docs/desktop/linux.md) | Install, verify, update and remove the `.deb`, and what is supported where |
| [Contributing](CONTRIBUTING.md) | Toolchains, checks, and the issue and PR workflow |
| [Roadmap](ROADMAP.md) | Shipped phases, the private alpha, later direction |
| [Client experience](docs/design/client-experience.md) | Interaction design, including voice and failure states |
| [ADRs](docs/adr/) | The accepted architecture and product constraints, starting with the [charter](docs/adr/ADR-000-charter.md) |
| [Changelog](CHANGELOG.md) | What changed in each release, and upgrade notes |

## Development

```sh
make build              # Go binaries
make check              # fmt, vet, lint, tests, schema, dependency, Rust and web gates
go run ./e2e/dogfood    # live request, notify, resolve, audit approval loop
```

`make check` uses pinned tools and does not skip a missing toolchain. [CONTRIBUTING.md](CONTRIBUTING.md) has the setup.

## Trust and security

Conch uses a **server-trust model, not end-to-end encryption**. The operator controls the server and the database. Nets and whispers make the server deliver only to their audience; they cannot stop a recipient repeating what they received.

An agent manifest limits what an agent may do in Conch. It does not sandbox the agent's own tools. Audit records make activity inspectable; they are not a cryptographic guarantee against an operator who edits the database.

Report a vulnerability privately, as [SECURITY.md](SECURITY.md) describes, not in a public issue. For bugs and feedback, use the [issue forms](https://github.com/njdaniel/conch/issues/new/choose). The [manual](docs/manual.md#trust-and-scope) has more on trust and scope.

## Why "Conch"?

A conch is a signal horn and a symbol of speaking rights. The name reflects the goal: help people and agents know when to speak, whom they are addressing, and who can hear.

## Contributing

Setting up, the checks to run, and how issues, pull requests and reviews work are in [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[AGPL-3.0](LICENSE)
