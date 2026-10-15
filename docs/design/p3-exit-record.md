# P3 first-prototype exit record

Issue #286. The exit of the first P3 prototype (the local multi-client slice of ADR-007),
kept apart from completion of P3 itself. The rows are those of the brief's test table,
[`p3-local-prototype.md`](p3-local-prototype.md) §4, and the walkthrough is
[`docs/p3-walkthrough.md`](../p3-walkthrough.md).

> **Whose run this is.** #286 asks for Nick's own run of the walkthrough, by hand. On
> 2026-10-14 Nick said "skip waiting on me, use your recommendations, continue working on
> p3". So this record is an **agent run** on Nick's machine: the principal session ran
> every row from a fresh worktree of `main`, through the runner the walkthrough uses, with
> real binaries, the terminal clients on pseudo-terminals, the web client in Chromium, and
> one real Claude Code reply. It is not a by-hand run, and it is not labelled as one. Nick's
> own run is optional; if he makes one, it goes in the last section and takes precedence
> over anything here.

## The run

| | |
| --- | --- |
| Date | 2026-10-14 |
| Commit | `ef72661d6e895da72f5d2f3bd1ce3a315e722fd3` (`main`, a fresh `git worktree add --detach origin/main`) |
| OS | Pop!_OS 24.04 LTS, Linux 7.0.11 x86_64 |
| Terminal | the TUIs on pseudo-terminals driven by `e2e/p3-local` (80×24); `TERM=xterm-256color` |
| Browser | Chromium revision 1228, driven by Playwright 1.61.0 (`make web-e2e`) |
| Go | go1.25.0 linux/amd64 |
| Node | v24.21.0 (`web/.nvmrc`) |
| Claude Code (P3-13) | 2.1.299 |
| Commands | `go run ./e2e/p3-local`, `make web-e2e`, `CONCH_P3_ROWS=P3-13 go run ./e2e/p3-local --real-adapter` |

## Rows

Every row passed. "Suite" is `go run ./e2e/p3-local` (46 s for the whole run). "Browser" is
`make web-e2e` (8 tests, 20 s). The suite runs the rows in the order 01–05, 09, 10, 06–08, 11,
13, because P3-10's last check waits for the server's 30 s credential recheck.

| Row | What | Result | Note |
| --- | --- | --- | --- |
| P3-01 | Two TUIs, the browser and two scripted agents connect with separate credentials; the views show the real principals and sessions | **pass** | Suite (0.3 s): five participants, five credentials, one roster, the same for all five viewers. Browser: sign-in by pasting the credential, the fixture channels listed; a wrong credential shows the failure line and leaves no token in the page. |
| P3-02 | A message from each human client and the permitted agent is seen once, in order, by each authorized peer; history agrees after reload | **pass** | Suite (1.8 s): typed in each TUI, with worker-test's replies; every history and every live socket hold the same ids in the same order. Browser: once in two contexts and once in the API, also after a reload. |
| P3-03 | Different channels at once: each client keeps its own selection and draft | **pass** | Suite (0.3 s). |
| P3-04 | One person in two clients: two sessions; ending one leaves the other; revoking one credential affects only its scope | **pass** | Suite (0.2 s): a heartbeat or end on another credential's session is refused 404. Browser (10.5 s): revoking the browser's credential signs it out and the terminal session stays online. The 10.5 s is the events socket's 10 s keep-alive tick. |
| P3-05 | Restricted channel, net and whisper: insiders see it in history, live and search; outsiders cannot fetch or subscribe | **pass** | Suite (1.2 s): REST, `/v2/ws` live and `?after=` replay, `/v1/events/ws` and search, and the run answering a whisper, all hidden from outsiders. Browser: `test-private` cannot be opened, and nothing of a run there shows in the panel. |
| P3-06 | A scripted agent through idle, running, waiting, completed and failed: TUI and browser agree with the API on ids, task, outcome and activity | **pass** | Suite (2.9 s) and browser. With `approval.sh` the `running` state is too brief to see, so it is asserted through the ordered activity. |
| P3-07 | An agent stopped: stale, then offline within the threshold; no invented outcome; reconnection keeps one identity | **pass** | Suite (14.4 s), with `--session-stale 2s --session-offline 5s`: SIGSTOP gives stale, then offline with `outcome_unknown`, and the run stays `running`. SIGCONT brings back the same session id. |
| P3-08 | Instruction and cancel acknowledged and enforced; unsupported operations absent or disabled with a reason | **pass** | Suite (6.0 s): message and cancel end `applied`, and the command's own process is gone after the cancel. An agent asking is refused 403 `human_required`; pause is refused 409 `intervention_unsupported`. Browser: Cancel through its confirmation; Pause is absent. |
| P3-09 | Client connection dropped, then the server restarted: stale status, bounded retries, history restored, no silent resend | **pass** | Suite (7.1 s): the TUI's retries came in waves of 8 at +1.0 s and +3.0 s; it recovered on its own with each message once; the line typed while down was not sent. Browser: Reconnecting and "out of date", then recovery with each message once. |
| P3-10 | Membership removed and a credential revoked: live access ends, read/write/reconnect refused, others unaffected | **pass** | Suite (0.1 s, plus the deferred check): guest's sockets closed 1008 at once; browser-test's sockets closed 1008 at the 30 s recheck; a ticket cannot be spent twice. |
| P3-11 | No LiveKit and no model: chat, scripted agents, approvals and audit work; optional features say unavailable | **pass** | Suite (1.3 s): voice says not configured; an approval raised by `approval.sh` is decided in the TUI inbox; the audit trail is in order. |
| P3-12 | Reset: only the run's own processes and data removed; personal credentials and other Conch instances untouched | **pass** | Setup checks first; "nothing left behind: the root is gone, every process has exited and the port is closed" last. The runner's own tests check that `~/.config/conch` is untouched (a sentinel), and that reset refuses a foreign or symlinked root. |
| P3-13 | One real adapter, opted in: recorded separately below | **pass** | |

Every request the suite made went to a route in the public table (47 requests, 66 routes).

## P3-13: the real adapter

`CONCH_P3_ROWS=P3-13 go run ./e2e/p3-local --real-adapter`, with `claude` on PATH. It ran
once, as Nick's user, in a directory of the runner's root, which was removed afterwards.

| | |
| --- | --- |
| Runtime | `claude-code` (Claude Code 2.1.299) |
| Model | `sonnet` |
| Outcome | `replied`; the run ended `completed` (5.7 s) |
| Note | The runner printed its warning first and never started the real command in its setup checks. The run, its activity and the reply went through the same contract as the scripted agents. |

## P3-14: three runtimes at once (the success criterion)

The roadmap's P3 success criterion, by walkthrough row P3-14. On 2026-10-15 Nick chose
to have this run done as an **agent run**, as P3-13 was ("Agent run it"); it is not a
by-hand run. The principal session ran it once on Nick's machine, from a fresh
`git worktree add --detach origin/main`:

`go run ./e2e/p3-local --reset` then `CONCH_P3_MODEL_CLAUDE=sonnet CONCH_P3_MODEL_CODEX=gpt-6.1-sol CONCH_P3_MODEL_OLLAMA=llama3.1:8b go run ./e2e/p3-local --keep --real-adapters claude,codex,ollama`

The three printed `conch-adapter` commands were then started, each in an empty directory
of its own. Afterwards the run was reset, which removed its root and stopped `conchd`. A
check of every process whose working directory was one of those three found none left.

| | |
| --- | --- |
| Date | 2026-10-15, 13:25 UTC |
| Commit | `4cd8faaef3ec5a8e64c07cd4415971c693dee953` (`main`) |
| Claude Code | 2.1.300, model `sonnet` (principal `real-claude`) |
| Codex | codex-cli 0.162.1, model `gpt-6.1-sol`, Nick's configured default (principal `real-codex`) |
| Ollama | 0.32.0, model `llama3.1:8b` (principal `real-ollama`) |

**What appeared.**
- **Roster:** the operator's `GET /v1/sessions?channel=general` showed three `conch-adapter` sessions, all online:
  - `claude-code` / `sonnet`;
  - `codex` / `gpt-6.1-sol`;
  - `ollama` / `llama3.1:8b`.
- **One message, three replies:** `nick-test` posted one message asking each agent to say which runtime and model it is. Each agent started a run answering it and replied:
  - **Claude Code:** "Claude Code (Claude Agent SDK runtime), running the Sonnet 5.5 model (claude-sonnet-5-5)."
  - **Codex:** "I'm running in the Codex runtime with the GPT-6 model."
  - **The local model:** "I am running PyTorch and MobileNet." The 8B model's answer is wrong, but the contract carried it like the others.
- **Activity per run:**
  - Claude Code and Ollama: `command_started`, `command_finished` (exit 0) and `reply_posted`.
  - Codex: `reply_posted` only, because it ran no command.
- **The instruction to one agent:** a `message` intervention to Codex's open run was `applied` 1.1 s after it was sent, and the run's activity shows `intervention_received`. By design the body goes into that agent's *next* prompt, not the reply it is already writing (`internal/adapter/run.go`). The adapters were stopped before Codex's next reply finished, so the intervention's effect on a reply was not observed.
- **On stop:** stopping the adapters cancelled their open runs with outcome `adapter stopped`.

**What went wrong.**
- **#410: the agents answered each other.** After the three replies, each agent answered the others' replies, and so on: 21 messages and 22 runs in about 30 s, until the adapters were stopped. An adapter answers every message not written by itself. With two or more adapters in one channel this loop has no end, and it spends the operator's quota.
- **#411: escapes in a reply.** One Ollama reply was posted with raw terminal control sequences in it (`\x1b[13D\x1b[K`), from `ollama run`'s word wrap.

**Where this leaves the success criterion.** Claude Code, Codex and a local model ran at the same time against one server. Each showed in the roster with its runtime and model, each run's activity could be read, and an instruction to one was delivered and acknowledged. That is the criterion, met in an agent run. It is not yet comfortable in practice: until #410 is fixed, three agents in one channel talk to each other without end. The observation was through the API and the CLI, not on the TUI's screen; the TUI shows the same data (P3-06).

## What surprised us

Found while building and testing the prototype. Each item is fixed, or filed:

- `--reset` through a symlinked parent directory found none of the run's processes, removed the data from under a running `conchd`, and reported success. Fixed in #339.
- The web client's re-read of a channel merged the new page into stale held messages, which left a gap that nothing on screen showed. Fixed in #356, with race tests.
- The suite's first cancel check could not fail, because the fixture exited at once. It now uses `hang.sh` and a mutation proves it (#341).
- `TestRunWithoutKeep` failed once in CI ("agent-a did not reply within 30s") under `go test -race`: #357.
- The `internal/server` tests take about 6 of go test's 10 minutes in CI: #340.

## What remains of P3

Updated 2026-10-15. Every follow-on filed at the first exit has merged:
- adapters and controls: #343 (Codex), #344 (local model), #345 and #346 (pause and resume);
- the web client: #347 (embedded in the binary), #348 (TypeScript bindings), #350 (Runs view), #352 (composing to nets and whispers);
- the TUI: #349 (Runs view);
- read state: #351, #372, #373;
- the webhook adapter: #359, with #386–#388 and its hardening #392, #402, #403, #405, #406;
- the VM/LAN children: #379, #380, #382.

What is open:

| Issue | Item | Who or what it waits on |
| --- | --- | --- |
| #410 | `conch-adapter` answers other agents, so agents loop in a shared channel | In progress |
| #411 | Terminal escapes in text replies (Ollama) | In progress |
| #381 | The guest session by hand (VM/LAN record), which keeps tracker #353 open | Nick: it needs a real second machine |
| #396 | The voice session by hand | Nick, after #393 and #394; non-blocking for P3 (decided on #354) |
| #393, #394 | Voice rows in the automated suite, which keep tracker #354 open | Gated on #189 (the V4 exit) |

## Nick's own run (optional)

Not made. #286 closed on the agent run above, by Nick's instruction. If Nick runs the walkthrough by hand, record here the date, the commit,
the terminal and browser, one line per row, and anything that surprised him. A row that
fails gets its own issue.
