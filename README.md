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

Every command below has been run against real `bin/conchd`/`bin/conch` builds. Paths and IDs are examples — substitute your own.

### 1. Build

```sh
make build   # builds bin/conchd and bin/conch
make check   # fmt, vet, lint, tests, schema-compat, dependency gate — run before opening any PR
```

### 2. Start `conchd`

```sh
mkdir -p /tmp/conch-data
bin/conchd serve --data /tmp/conch-data --listen :8080
```

- `--data` (or `CONCHD_DATA`) is required — directory for the embedded SQLite database.
- `--listen` (or `CONCHD_LISTEN`) defaults to `:8080`.
- `--mcp-token token=principal_id` (or `CONCHD_MCP_TOKENS`, comma-separated) maps MCP bearer tokens to agent principal IDs — see step 3, since the ID doesn't exist until you bootstrap it.
- `--ntfy-server`/`--ntfy-topic`/`--ntfy-urgent-topic` (or `CONCHD_NTFY_*`) are optional. ntfy is a push-notification integration, not a dependency: `conchd` runs, and approvals still resolve, with no ntfy server reachable — the single-binary invariant (ADR-002) requires no other external process for core function.

### 3. Bootstrap a channel and principals

There's no admin CLI yet — a channel and its principals (human and agent) are created directly via the `/v0` REST API:

```sh
curl -s -X POST localhost:8080/v0/channels \
  -H 'Content-Type: application/json' -d '{"name":"ops"}'
# {"channel":{"id":1,"name":"ops", ...}}

curl -s -X POST localhost:8080/v0/principals \
  -H 'Content-Type: application/json' -d '{"kind":"agent","name":"deploy-bot"}'
# {"principal":{"id":1,"kind":"agent", ...}}

curl -s -X POST localhost:8080/v0/principals \
  -H 'Content-Type: application/json' -d '{"kind":"human","name":"nick"}'
# {"principal":{"id":2,"kind":"human", ...}}
```

The agent principal's returned `id` is what `--mcp-token` must reference. Restart `conchd` (same `--data` dir, so nothing is lost) with the mapping filled in:

```sh
bin/conchd serve --data /tmp/conch-data --listen :8080 --mcp-token mytoken=1
```

### 4. Human side: `conch`

`conch` with no arguments launches the TUI (needs a real terminal):

```sh
CONCH_SERVER=http://127.0.0.1:8080 CONCH_AUTHOR=2 CONCH_CHANNELS=ops bin/conch
```

- `CONCH_SERVER` — conchd URL (default `http://127.0.0.1:8080`).
- `CONCH_AUTHOR` — principal ID used as the message author.
- `CONCH_CHANNELS` — comma-separated channels the TUI opens.

For scripting, `conch` also has plain subcommands:

```sh
bin/conch send --author 2 ops "deploying release 42"
bin/conch tail ops
bin/conch approvals list
bin/conch approve --author 2 --reason "looks good" 1
bin/conch reject  --author 2 --reason "not yet" 1
```

`--server`/`CONCH_SERVER` works the same way on every subcommand.

### Optional: auto-reply bot

`conch-bot` watches one channel and replies to new human messages using the
local `claude -p` command. Give it an MCP token mapped to its own agent
principal and that principal's ID:

```sh
CONCH_BOT_TOKEN=mytoken \
CONCH_BOT_PRINCIPAL_ID=1 \
CONCH_BOT_CHANNEL=ops \
bin/conch-bot
```

It skips messages already present when it starts and ignores its own replies.
Optional settings include `CONCH_BOT_SERVER`, `CONCH_BOT_POLL_INTERVAL`,
`CONCH_BOT_MAX_BACKOFF`, `CONCH_BOT_CONTEXT_MESSAGES`, `CONCH_BOT_MODEL`,
`CONCH_BOT_REPLY_TIMEOUT`, `CLAUDE_BIN`, and `CONCH_BOT_LOCK_FILE`.

### 5. Agent side: MCP

Agents connect to `POST /mcp` (streamable HTTP) with `Authorization: Bearer <token>` — the token from step 3. Five tools are registered:

- `post_message` — post a message to a channel as the authenticated agent.
- `read_channel` — read one paginated page of messages from a channel.
- `request_approval` — raise an approval as the authenticated agent.
- `await_decision` — block until an approval resolves (`timeout_ms`, clamped to a 60s server-side max).
- `check_decision` — read an approval's current state/resolution immediately, without blocking.

A raw JSON-RPC example (most agents will instead use an MCP client SDK):

```sh
curl -s -X POST localhost:8080/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Authorization: Bearer mytoken' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
        "protocolVersion":"2025-06-18",
        "clientInfo":{"name":"my-agent","version":"1"},
        "capabilities":{}}}'
# note the returned Mcp-Session-Id response header — pass it on every subsequent call
```

### 6. Verify your install

```sh
go run ./e2e/dogfood
```

This drives the full loop live against freshly built binaries — agent posts via MCP, requests approval, ntfy fires, a human resolves via `conch approve` with a reason, `await_decision` returns the structured outcome, the audit log shows the whole chain — then reruns the approval half with ntfy unreachable to prove it still resolves (ADR-002). Exits nonzero on any assertion failure.

### Known limitations

- No REST/TUI authentication beyond MCP bearer tokens today. Don't expose `conchd` past localhost/VPN without your own reverse-proxy auth in front of it.
- No per-agent capability enforcement yet — any valid MCP token can call any registered tool.

## License

[AGPL-3.0](LICENSE)
