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

`make check` lints with the `golangci-lint` version in `.golangci-lint-version` (the one CI runs) and refuses any other; install it with `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(cat .golangci-lint-version)`.

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
- `--ntfy-server`/`--ntfy-topic`/`--ntfy-urgent-topic` (or `CONCHD_NTFY_*`) are optional. ntfy is a push-notification integration, not a dependency: `conchd` runs, and approvals still resolve, with no ntfy server reachable — the deployment invariant (ADR-002) requires no other external process for core function.

### 3. Create a channel, people, and an agent

There's no admin CLI yet; the operator does this through the REST API with their token:

```sh
OP="Authorization: Bearer $(cat /tmp/conch-data/operator.token)"
J='Content-Type: application/json'

curl -s -X POST localhost:8080/v0/channels   -H "$OP" -H "$J" -d '{"name":"ops"}'
# {"channel":{"id":1,"name":"ops", ...}}       the operator, who created it, is its only member

curl -s -X POST localhost:8080/v0/principals -H "$OP" -H "$J" -d '{"kind":"human","name":"alice"}'
# {"principal":{"id":2,"kind":"human", ...}}
curl -s -X POST localhost:8080/v0/principals -H "$OP" -H "$J" -d '{"kind":"agent","name":"deploy-bot"}'
# {"principal":{"id":3,"kind":"agent", ...}}

# a credential for each: the token is shown once, in this response
curl -s -X POST localhost:8080/v1/principals/2/credentials -H "$OP" -H "$J" -d '{"label":"alice laptop"}'
curl -s -X POST localhost:8080/v1/principals/3/credentials -H "$OP" -H "$J" -d '{"label":"deploy-bot"}'
# {"credential":{"id":2, ...},"token":"conch_..."}

# channel membership decides who can see a channel at all
curl -s -X PUT localhost:8080/v1/channels/ops/members/2 -H "$OP"
curl -s -X PUT localhost:8080/v1/channels/ops/members/3 -H "$OP"
```

On a machine you share, a header given with `-H "..."` is visible in the process list while `curl` runs. Put the header line in a private file and pass `-H @that-file` instead.

A principal who is not a member of a channel cannot read it, post to it, subscribe to it, or see it listed; to them it does not exist. Operators manage membership but get no exemption. Other operator actions: `DELETE /v1/credentials/{id}` revokes one credential, `POST /v1/credentials/{id}/rotate` replaces one, `POST /v1/principals/{id}/disable` switches a principal off entirely (and `/enable` back on), and `DELETE /v1/channels/{channel}/members/{id}` removes a member.

### 4. Human side: `conch`

Each person signs in once with the token the operator gave them. The token is read from stdin and stored under your user config directory, readable only by you:

```sh
bin/conch login --server http://127.0.0.1:8080 < alice.token
# logged in to http://127.0.0.1:8080 as alice (member)
bin/conch whoami
```

Then `conch` with no arguments launches the TUI (needs a real terminal). It lists the channels you are a member of and shows who you are signed in as:

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

`--net` and `--to` cannot be combined. `tail` marks scoped messages: `[net:alpha]` for a net, `[whisper:3,5,7]` for a whisper (the ids listed are everyone who can see it); channel-wide messages have no marker, and a message body that itself starts with `[` is printed as `\[` so that it cannot be mistaken for one. Whispers are discretion, not secrecy: they are recorded in the audit log, and `send --to` says so on stderr. Net management is an operator action; anyone else gets the server's refusal.

In the TUI the prompt always says where your next message goes: `ops >` for the whole channel, `ops/alpha >` for a net. `/net alpha` sets that target, `/net` alone returns to the channel, and switching channels resets it. `/w 3,5 quick question` whispers once and leaves the target as it was. A message that really starts with `/` is sent as `//text`; any other `/word` is an error, not a message. Scoped messages start with a `[net:alpha]` or `[whisper:3,5]` badge (a whisper lists the other participants); a body that starts with `[` is shown with a leading `\`. The first whisper of a session reminds you that whispers are recorded in the audit log. If the server refuses a send, your text stays in the input and nothing else is sent: the TUI never falls back to the channel.

- `CONCH_SERVER` (or `--server`) — conchd URL (default `http://127.0.0.1:8080`).
- `CONCH_TOKEN` — a token to use instead of the stored login, for scripts and CI.
- `CONCH_CHANNELS` — optional comma-separated override for which channels the TUI opens.
- `--author` / `CONCH_AUTHOR` are deprecated: identity comes from your login. They still work against a server running `--auth off`.

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

Agents connect to `POST /mcp` (streamable HTTP) with `Authorization: Bearer <token>` — the agent's token from step 3. Five tools are registered:

- `post_message` — post a message to a channel as the authenticated agent. With an `audience` (`{"kind":"net","net_id":N}` or `{"kind":"principals","principal_ids":[…]}`) it goes only to that net or those principals; the agent's manifest must grant `post_net`, or `whisper` (plus `whisper_agent` to reach another agent).
- `read_channel` — read one paginated page of messages from a channel: channel-wide ones, and scoped ones the agent is a recipient of. A message with an `audience` was not sent to everyone; reply in kind by sending the same `audience` back.
- `request_approval` — raise an approval as the authenticated agent.
- `await_decision` — block until an approval resolves (`timeout_ms`, clamped to a 60s server-side max).
- `check_decision` — read an approval's current state/resolution immediately, without blocking.

**Agents are deny-by-default.** A token only says who the agent is. To do anything in a channel an agent needs both **membership** of the channel (step 3) and a **manifest** granting the capability and the permission there:

```sh
curl -s -X PUT localhost:8080/v1/principals/3/manifest -H "$OP" -H "$J" -d '{
    "display_name": "deploy-bot", "tier": "A",
    "capabilities": ["messages.read","messages.post","approvals.request","approvals.await","approvals.check"],
    "channels": [{"channel_id": 1, "permissions": ["read","post"]}]
  }'
```

Capabilities are `messages.read`, `messages.post`, `approvals.request`, `approvals.await`, `approvals.check`; each is granted separately. Without membership a channel looks like it does not exist; with membership but no grant the call returns `forbidden`. Every refusal is written to the audit log. Details: [docs/design/agent-manifest.md](docs/design/agent-manifest.md).

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

### Upgrading an existing instance

- **Authentication is now on by default.** Run `conchd bootstrap-operator` once against your existing data directory before starting the new `conchd`. It revokes credentials and deletes webhook hooks created before the operator existed, and tells you how many.
- **Channels now have members.** The upgrade makes every existing principal a member of every existing channel, so nothing disappears; channels and principals created afterwards start with no memberships.
- **Agents are deny-by-default.** An agent that existed before has no manifest and can do nothing until you write one; `conchd` logs how many such agents there are at startup. The static `--mcp-token token=principal_id` flag still works but is deprecated in favour of issued credentials.

### Known limitations

- Tokens travel in the `Authorization` header in the clear unless you put TLS in front of `conchd`; keep it on localhost or a VPN, or behind a TLS-terminating reverse proxy.
- `conch login` does not echo a token typed at its prompt. Piping it from a file, as shown, also keeps it out of your clipboard and scrollback.
- A webhook hook URL is a posting credential. List hooks with `GET /v1/hooks` and revoke one with `DELETE /v1/hooks/{id}` (operator only); only a hash of each token is stored. The database file is created owner-only (0600) and an existing looser one is tightened at startup.

## License

[AGPL-3.0](LICENSE)
