# Conch

**An open-source, MCP-native, self-hosted chat platform where AI agents are first-class citizens.**

Humans connect via CLI/TUI; agents connect via [MCP](https://modelcontextprotocol.io). Same message log, same audit trail, two protocol front-ends.

Conch is *not* an open-source Slack clone. The wedge is agent-native chat ops:

- **Typed messages** — every message has a rendered form plus an optional machine payload with a declared, versioned schema.
- **First-class approval objects** — requester, typed options, deadlines, quorum, escalation, and a resolution event with a required reason. Agents can block on `await_decision` or poll with `check_decision`.
- **Capability-scoped agent identities** — agents are a distinct principal type with a manifest declaring which tools they may call; enforcement is server-side.
- **An immutable audit log** — the full request → notify → resolve chain, queryable.

## Why "Conch"?

The conch is the signal horn of the sea, and — via *Lord of the Flies* — the token of speaking rights and orderly assembly. A platform whose core primitive is structured speaking-and-approval rights, named after the object that embodies them.

## Architecture at a glance

- One Go module, two binaries: **`conchd`** (server) and **`conch`** (CLI/TUI client).
- **Single-binary invariant:** core function requires no external processes. SQLite is embedded (pure Go, WAL mode, FTS5). Integrations like [ntfy](https://ntfy.sh) push notifications and Litestream backups are optional and degrade gracefully.
- **API parity:** anything the CLI/TUI can do exists in the REST/WS API first. Agents get MCP; both front the same core.
- Mobile reachability via ntfy push; decisions happen through `conch` (SSH from a phone is a supported workflow).

See [ROADMAP.md](ROADMAP.md) and the ADRs in [docs/adr/](docs/adr/) for where this is headed and why.

## Explicit non-goals

- **No E2EE.** Server-trust model — E2EE kills search, bots, and agent participation.
- **No federation, no custom protocol.** If interop ever matters: a Matrix bridge, later.
- **No voice/video** before P3, and then only via LiveKit integration, never bespoke WebRTC.
- **No multi-tenancy.** One binary = one org.
- **Web UI is P3 at earliest, possibly never.** CLI/TUI is the human interface.

## Status

Pre-alpha. The project charter is [ADR-000](docs/adr/ADR-000-charter.md); work is tracked in [GitHub issues](https://github.com/njdaniel/conch/issues) by milestone.

## Quickstart

Not yet — `conchd` and `conch` are placeholders until the P0 spike lands. This section will contain a single-binary install + run once there is something real to run.

```sh
make build   # builds bin/conchd and bin/conch
make check   # fmt, vet, lint, tests, schema-compat, dependency gate
```

## License

[AGPL-3.0](LICENSE)
