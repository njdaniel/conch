# Contributing to Conch

Thanks for looking. Conch is pre-alpha (see the [README](README.md)), so expect things to move. This file is what you need on day one. The rules themselves live elsewhere, and where this file and they disagree, they win:

- [CLAUDE.md](CLAUDE.md): the build rules. It is written for the AI agents the maintainer works with, but it binds every change, whoever or whatever wrote it.
- [ADR-000](docs/adr/ADR-000-charter.md): the charter. What Conch is, what it will not be, and who decides what.
- The [ADRs](docs/adr/) and the [roadmap](ROADMAP.md): the decisions already made, and the order of the work.

You do not need Claude or any other AI tool to contribute. The files under `.claude/` are the maintainer's tooling; you can read them as the project's standards, but nothing asks you to run them.

## Before you start

1. **Find or file an issue.** Every change starts from one. Use the [issue forms](https://github.com/njdaniel/conch/issues/new/choose): a bug report, or feedback and ideas. Blank issues are turned off.
2. **Check that it is ready.** Work is written up as issues a newcomer can pick up with no other context, using the [task template](.github/ISSUE_TEMPLATE/task.md): Context, Files, Acceptance criteria, Required tests, Blocked by. An issue that has these and is not blocked is ready.
3. **Claim before working.** Read the issue comments, labels, linked PRs and
   Project status. Check other active claims for overlapping files. Agents and
   their dispatchers use `bash ops/issue-claim.sh claim ISSUE --agent SESSION
   --branch BRANCH --files PATHS` with a unique session name; start only after
   success. The command records the claim, sets `agent/in-progress`, and verifies
   the board's In progress status. If another person/agent is working it, ask
   for a handoff instead of duplicating it. Contributors without repository or
   Project write access ask the maintainer to claim on their behalf. For anything
   larger than a small fix, agree the approach on the issue first. Follow the
   [claim lifecycle](docs/design/agent-dispatch.md#issue-claims) for review,
   abandonment and merged work.
4. **Check the settled decisions.** The non-goals in [ADR-000](docs/adr/ADR-000-charter.md) (for example no end-to-end encryption, no federation, no multi-tenancy) and the locked decisions there and in later ADRs are not reopened by a pull request. Reopening one takes an [ADR proposal](.github/ISSUE_TEMPLATE/adr-proposal.md) with new evidence, and the maintainer's sign-off.

## Set up

| You need | For | Notes |
| --- | --- | --- |
| Go, the version in [go.mod](go.mod) | everything | Enough on its own for `make build` and the Go tests. |
| `golangci-lint`, exactly the version in [.golangci-lint-version](.golangci-lint-version) | `make lint`, `make check` | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(cat .golangci-lint-version)`. Any other version is refused. |
| `jq` | the dependency gates | |
| Node, exactly the version in [web/.nvmrc](web/.nvmrc), with the npm it bundles | the browser client under `web/` | `nvm install` in `web/` picks it up. |
| [rustup](https://rustup.rs), `g++`, and `libglib2.0-dev libpipewire-0.3-dev libclang-dev pkg-config` | the Rust voice client under `voice/` | Then run `scripts/voice-toolchain.sh` once: it fetches a pinned clang, cargo-deny and libwebrtc into `~/.cache/conch` (about 2.1 GB to download), with no root. |
| `sqlite3`; `shellcheck` and `systemd-analyze` if you have them | `make deploy-test` | Without the last two that test skips them with a line; CI has them. |
| Docker | running LiveKit for voice | Only for voice: `scripts/dev-stack.sh`, `go run ./e2e/voice` and `go run ./e2e/isolation`. |

Then install the pre-commit hook once:

```sh
make hooks-install   # every commit: gofmt, invisible characters, vet, lint, go test, schema and TypeScript checks, Go dependency gate, Rust leg
```

`make check` does not skip the Rust or web leg when its toolchain is missing: it fails and names what to install. The [README's build section](README.md#1-build) says more about each leg.

## Build and run

```sh
make build                    # bin/conchd, bin/conch, bin/conch-adapter, bin/conch-bot (Go only)
scripts/dev-stack.sh up       # the whole stack on your machine: LiveKit in Docker, conchd, an operator and a channel
scripts/dev-stack.sh token    # the operator credential, to paste at http://127.0.0.1:28080/app/
scripts/dev-stack.sh down     # stop it; down --wipe also deletes its state
```

`scripts/dev-stack.sh --help` lists every command and setting. To do the same by hand, one step at a time, follow the [Quickstart](README.md#quickstart). `make p3-local` starts a disposable server with a small test community and `make p3-reset` removes it ([walkthrough](docs/p3-walkthrough.md)). `make web-build` builds the browser client and `make rust-build` builds `bin/conch-voice`.

## Checks

Run `make check` before you open a pull request. It runs gofmt, the invisible-character check, `go vet`, `golangci-lint`, `go test`, the schema compatibility check, the generated TypeScript check, the Go dependency gate, the deployment-script test, the Rust leg (`make rust-check`) and the web leg (`make web-check`). CI runs the same checks, with `go test -race`, and more:

| If you change | Also run | CI job |
| --- | --- | --- |
| anything | `make check` | `check` |
| `pkg/schema` | follow the [schema-change procedure](.claude/skills/schema-change/SKILL.md); `go generate ./pkg/schema` regenerates the web client's wire types | `check` |
| `web/` | `make web-e2e` for the browser suite (once: `cd web && npx playwright install chromium`) | `web-e2e`, `web-embedded` |
| `voice/` or the voice control plane | `go run ./e2e/voice` (Docker) | `voice-check` |
| routes, audit rows, principals or credentials | `go run ./e2e/p2-hardening`, `go run ./e2e/p3-local`, `go run ./e2e/dogfood` | `p2-hardening-check`, `p3-local-check`, `dogfood-check` |
| nets and whispers | `go run ./e2e/nets` | `nets-check` |
| the reference bot | `go run ./e2e/conch-bot` | `conch-bot-check` |
| access between groups | `go run ./e2e/isolation` (Docker) | `isolation-check` |
| `ops/deploy` | `make deploy-test` | `check` |

These run on every pull request, except `dogfood-check`, which runs on pull requests labelled `approval-path` and on every push to `main`. The jobs are defined in [.github/workflows/ci.yml](.github/workflows/ci.yml).

Code is idiomatic for its language: Go is `golangci-lint` clean with table-driven tests; the Rust and TypeScript rules are in [CLAUDE.md](CLAUDE.md) rule 6. Never skip or delete a failing test without saying so in the pull request.

## Pull requests

- **One issue, one branch, one pull request.** Change only what the issue asks for. If you find something else, file an issue for it.
- **Reference the issue** in the description: `Closes #N`, or `Refs #N` when the pull request does not finish it. A CI check fails without it.
- **Fill in the [template](.github/pull_request_template.md):** what changed, the invariant checklist, and under Tests what you ran and what it printed. Flag anything you could not run, and why.
- **Merging** needs green CI and the maintainer's review. Approval-path changes (the request, notify, resolve, audit chain; the paths are listed in [process-checks.yml](.github/workflows/process-checks.yml)) carry the `approval-path` label and an end-to-end test of the whole chain, and the maintainer merges them personally.
- **Security-sensitive changes** (authentication and authorization, tokens, capability enforcement, webhook ingest, the MCP auth layer) get a security review before they merge.
- **Compatibility.** Every wire shape comes from `pkg/schema`. A published schema version never changes: a breaking change is a new version, with the maintainer's sign-off. Anything the CLI or TUI can do exists in the REST/WebSocket API first ([ADR-001](docs/adr/ADR-001-mcp-native-architecture.md)). `conchd` needs no other process for messaging, approvals or audit ([ADR-002](docs/adr/ADR-002-single-binary-sqlite.md)).
- **New dependencies** need the maintainer's sign-off before you add them, so ask on the issue first. Each also needs a line in its allowlist: [deps-allowlist.txt](deps-allowlist.txt) for Go, [voice/deps-allowlist.txt](voice/deps-allowlist.txt) for Rust, and [web/deps-allowlist.txt](web/deps-allowlist.txt) (at an exact version) for the web client, plus [web/licences-allowlist.txt](web/licences-allowlist.txt) for a new licence. The gates in `make check` fail without it.

What a review blocks on, and what it leaves as a comment, is written down in [.claude/agents/reviewer.md](.claude/agents/reviewer.md); the security review's scope is in [.claude/agents/security-reviewer.md](.claude/agents/security-reviewer.md).

## AI-assisted contributions

They are welcome, on the same terms as any other:

- **You are responsible for what you submit.** Understand every line, run the checks yourself, and report what they actually printed. "The tool said the tests pass" is not evidence.
- **Say what a tool wrote.** Keep any `Co-Authored-By` trailer your tool adds, and name in the description the tool and the parts it wrote. The maintainer's own agent-made changes do the same (the attribution step of [the review gate](docs/design/agent-dispatch.md#the-review-gate)).
- **Same review, same rules.** A pull request opened by a bot account, or from a `codex/*` branch, also needs the `gate:reviewed` label, which a reviewer applies after a full review.

## Reporting bugs

Use the [bug report form](https://github.com/njdaniel/conch/issues/new/choose). Leave out passwords, invite links and credentials (anything starting with `conch_`). If you are testing a running server, "Report a problem" in the web page and `conch report` collect the details for you ([Diagnostics](README.md#diagnostics)).

## Reporting a security vulnerability

Report it privately, through GitHub's **Report a vulnerability** button on the [Security tab](https://github.com/njdaniel/conch/security), not in a public issue, pull request or discussion. [SECURITY.md](SECURITY.md) says what is in scope, which versions are supported and what happens after a report.

## Licence

Conch is licensed under the [GNU Affero General Public License v3.0](LICENSE) ([ADR-000](docs/adr/ADR-000-charter.md), D13). Contributions are accepted under the same licence.
