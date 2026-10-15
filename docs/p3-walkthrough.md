# P3 local walkthrough

**What this is.** A walk through the P3 prototype on your own Linux machine: one
disposable `conchd`, two terminal clients as two people, a browser as a third, and
two scripted agents. It follows the thirteen scenarios P3-01 to P3-13 of the
[planning brief](design/p3-local-prototype.md) (section 4), in order. The runner that sets it up
is `e2e/p3-local` (issue #283); the behaviour you look at is specified in
[client experience](design/client-experience.md).

Nothing here needs a VM, a paid API, a microphone or LiveKit. Nothing here touches
your own Conch setup: every participant has a configuration directory of its own
inside the test root, and `~/.config/conch` is never read or written.

## Prerequisites

- Go, the version `go.mod` names.
- The Node that `web/.nvmrc` pins (nvm finds it; `make web-build` says so if it
  is missing). Only needed the first time, and again after `web/src` changes.
- A browser. For the browser to be a person of its own, use a window or profile
  that has no other Conch tab signed in (see P3-04).
- Terminal windows: at least six (the two TUIs, a third for the repeat login, the
  two agents, and one for commands). Make each at least 80 columns by 24 rows.

The same thirteen rows run without you in `go run ./e2e/p3-local` (no `--keep`):
the runner starts its own server, drives two terminal clients on pseudo-terminals
and the scripted agents, checks what each row says should appear, and removes
everything (issue #284). That is what CI runs. P3-13 is not part of it unless you
add `--real-adapter` and have `claude` on your PATH; it then runs one real reply
as you, which this walkthrough's last section describes.

## Start it

Everything is run from the repository root. This builds `conchd`, `conch` and
`conch-adapter` into the test root's `bin/`, builds the web client if
`web/dist` is missing or older than `web/src`, starts `conchd` on a loopback port
it picks itself, creates the community, and prints one command per participant.

```sh
go run ./e2e/p3-local --keep
```

`make p3-local` does the same. The test root is `$CONCH_P3_ROOT`, else
`$XDG_STATE_HOME/conch-p3`, else `~/.local/state/conch-p3`.

The runner prints every command below. In this document `<root>` stands for the
root it prints, `<address>` for the server's `host:port`, and `<id:NAME>`,
`<credential:NAME>` and `<pid:conchd>` for the ids in the list it prints under
"values the walkthrough names". Replace them by what you see. Windows are called
by the participant's name: `tui-a` is the first terminal, `tui-b` the second,
`tui-a2` the third, `agent-a` and `agent-b` the two agents, `browser` the browser.

The community it creates:

| | |
| --- | --- |
| People | `nick-test` (terminals `tui-a` and `tui-a2`, two credentials), `guest-test` (`tui-b`), `browser-test` (the browser) |
| Agents | `worker-test` (`agent-a`): can read, post and whisper in all three channels, ask for approval, keep a session and record runs. `observer-test` (`agent-b`): can read `general` and keep a session, nothing else |
| `general` | everyone |
| `test-private` | `nick-test` and `worker-test` only |
| `nets-demo` | everyone; net `red`: `nick-test` member, `guest-test` monitor; net `blue`: `guest-test` and `browser-test` members |

Each participant's credential is in its own file under `<root>`, mode 0600. The
operator's credential is in `operator.env`, which only the admin commands in P3-04
and P3-10 use.

## P3-01 Start the server, two TUIs, the browser and two agents

1. In `tui-a`, start the first terminal client:

   ```sh
   set -a; . <root>/tui-a.env; set +a; <root>/bin/conch
   ```

2. In `tui-b`, the second:

   ```sh
   set -a; . <root>/tui-b.env; set +a; <root>/bin/conch
   ```

3. In `agent-a` and `agent-b`, the two agents. Each prints nothing at first and
   keeps running; leave them be.

   ```sh
   set -a; . <root>/agent-a.env; set +a; <root>/bin/conch-adapter
   ```

   ```sh
   set -a; . <root>/agent-b.env; set +a; <root>/bin/conch-adapter
   ```

4. In the browser, open the address the runner printed (`http://<address>/app/`),
   then paste the credential from this file into the sign-in field. The page shows
   the server it talks to and one credential field, and no username or password.

   ```sh
   cat <root>/browser.txt
   ```

What should appear. In each client, signed in with its own credential, the
channels you may use: `general` and `nets-demo` for `guest-test` and
`browser-test`, plus `test-private` for `nick-test`. In `general`, the members
line (TUI) or members panel (browser) names the people who are connected and the
agents with a status each: `worker-test` and `observer-test` online, not offline.
These come from the server's real sessions: the ids and names are the ones
`conch whoami` prints, not placeholders. Press `tab` in a TUI to cycle channels,
inbox and agents; in the agents view each agent appears once, and `enter`
expands it to its session (client, runtime `fixture`, model `fixture`).

## P3-02 Send from each human client and from an agent

1. Send a line from `tui-a`, one from `tui-b` and one from the browser, each in
   `general`. Put the word `hello` in each.
2. From `tui-a`, send a message that is for the agent to answer, such as
   `hello, please answer`. `worker-test` replies in the channel within a few seconds,
   repeating the last line it was given. `observer-test` sends nothing: its
   manifest does not let it post.

What should appear. Every window shows each message once, in the same order, with
the author's name. No message is doubled between the optimistic copy and the live
frame. Reload the browser page and re-open the channel in a TUI: the history is
the same. From any terminal you can list it too:

```sh
set -a; . <root>/tui-b.env; set +a; <root>/bin/conch search general hello
```

## P3-03 Browse different channels at the same time

1. In `tui-a`, open `nets-demo`. In `tui-b`, stay in `general`. In the browser,
   open `general`, then start typing a line without sending it.
2. In `tui-a`, type half a line in `nets-demo` and switch to `general` and back.

What should appear. Each window keeps its own selected channel. Opening a channel
in one client moves no other client. The half-typed line in the browser, and the
one in `tui-a`, are still there after switching channels and after roster updates.

## P3-04 One person, two sessions

1. In `tui-a2`, sign in as `nick-test` again with its second credential:

   ```sh
   set -a; . <root>/tui-a2.env; set +a; <root>/bin/conch
   ```

2. Look at the members line in `tui-b` or the browser.
3. Quit `tui-a2` (`ctrl-c`). Start it again, then, from the command window, list
   `nick-test`'s credentials and revoke the one named `tui-a2`:

   ```sh
   set -a; . <root>/operator.env; set +a; <root>/bin/conch admin credential list <id:nick-test>
   ```

   ```sh
   set -a; . <root>/operator.env; set +a; <root>/bin/conch admin credential revoke <credential:tui-a2>
   ```

What should appear. After step 1, `nick-test` is one name with `(2)` beside it
in the other clients, not two people. After step 3's quit, it is `nick-test`
alone again, and `tui-a` is unaffected. After the revoke, `tui-a2` loses access
(its next request is refused and it asks you to sign in) while `tui-a` carries on
sending and receiving. The listing shows `tui-a` active and `tui-a2` revoked.

## P3-05 A restricted channel, nets and whispers

1. As `nick-test` in `tui-a`, post to `test-private`. Then post to the `red` net
   of `nets-demo` (in the TUI, `/net red` then your line; or from the command
   window):

   ```sh
   set -a; . <root>/tui-a.env; set +a; <root>/bin/conch send --net red nets-demo 'hello red'
   ```

2. Whisper to the agent:

   ```sh
   set -a; . <root>/tui-a.env; set +a; <root>/bin/conch send --to <id:worker-test> general 'hello, a whisper'
   ```

3. Follow `nets-demo` as `guest-test` (it monitors `red`; `ctrl-c` ends it), and
   look for the word `hello` in `general`:

   ```sh
   set -a; . <root>/tui-b.env; set +a; <root>/bin/conch tail nets-demo
   ```

   ```sh
   set -a; . <root>/tui-b.env; set +a; <root>/bin/conch search general hello
   ```

4. As `guest-test`, who is not in `test-private`, try to read it, through the
   client and straight through the API:

   ```sh
   set -a; . <root>/tui-b.env; set +a; <root>/bin/conch tail test-private
   ```

   ```sh
   set -a; . <root>/tui-b.env; set +a; curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $CONCH_TOKEN" http://<address>/v2/channels/test-private/messages
   ```

What should appear. `test-private` is not in `guest-test`'s or `browser-test`'s
channel list. The `red` post shows to `nick-test` and, marked as a net message, to
`guest-test` (monitor), and not to `browser-test`, who is only on `blue`. The
whisper shows to its recipients only, marked as a whisper, and appears in the
audit log. The search finds it for those who may see it and for nobody else. The
last two commands are refused: `channel not found` from the client, and a
`404` status from the API. An outsider cannot tell the channel exists.

## P3-06 An agent through idle, running, waiting, completed and failed

1. With the default `agent-a` running, send a line in `general`. While it
   answers, `worker-test` shows as running, then idle again. Open the run in the
   agents view of a TUI (`tab` to agents, `enter` on the agent, `enter` on its
   run) and in the browser's agent detail.
2. Stop `agent-a` (`ctrl-c`) and start it with the approval script. Each run now
   asks for an approval, so it shows as waiting until someone decides:

   ```sh
   set -a; . <root>/agent-a.env; set +a; CONCH_ADAPTER_CMD=<root>/fixtures/approval.sh <root>/bin/conch-adapter
   ```

   Send a line, then decide it from a terminal: the approval is listed here, and
   `conch approve <id>` or `conch reject <id>` resolves it.

   ```sh
   set -a; . <root>/tui-a.env; set +a; <root>/bin/conch approvals list
   ```

3. Stop it and start it with the failing script. A run now ends failed:

   ```sh
   set -a; . <root>/agent-a.env; set +a; CONCH_ADAPTER_CMD=<root>/fixtures/fail.sh <root>/bin/conch-adapter
   ```

What should appear. The TUI and the browser show the same session and run ids,
the same current task, the same state and the same activity lines, in agreement.
A completed run shows its outcome and offers no keys; the failed one shows
`exit 3` and posts no reply. The approval's decision moves the run on.

## P3-07 Stop an agent or its heartbeat

1. Freeze both agents. Their heartbeats stop, and they do not exit:

   ```sh
   pkill -STOP -f <root>/bin/conch-adapter
   ```

2. Watch the members line or panel. Wait for the server's threshold (the agents
   send a heartbeat every 10 seconds; the server marks a session stale after a few
   missed ones and offline after more).
3. Let them go on:

   ```sh
   pkill -CONT -f <root>/bin/conch-adapter
   ```

4. Stop one agent with `ctrl-c` and start it again.

What should appear. The agents go stale, then offline, within the documented
threshold, and come back online after step 3: the same session revives. If a run
was open, the view says `outcome unknown` beside its last reported state; it
does not invent a result. After step 4 the agent has a new session but is still
one `worker-test` (or `observer-test`): no duplicate identity.

## P3-08 Instructions and cancel

1. Stop `agent-a`, start it with the slow script, and send a line in `general`:
   the run now waits 30 seconds before it replies.

   ```sh
   set -a; . <root>/agent-a.env; set +a; CONCH_ADAPTER_CMD=<root>/fixtures/slow.sh <root>/bin/conch-adapter
   ```

2. In a TUI, open the run (agents view, `enter` on the agent, `enter` on the
   run). Press `m`, type an instruction and press enter. Then press `x` and answer
   `y` to cancel the run. In the browser's run detail use the `Message` and
   `Cancel` buttons.
3. Open the run of `observer-test` or look at what the detail offers for a kind
   the agent did not declare, such as pause.

What should appear. The intervention shows `requested…`, then `acknowledged`,
then `applied` (or `failed: <reason>`), from the agent's own reports. The run
changes to cancelled only when the run's own update says so, not when you
press `y`. Kinds the agent does not support are listed as unsupported (TUI:
`pause (unsupported)`; browser: disabled with `Not supported by this agent`) and
do nothing.

## P3-09 Drop a connection, then restart the server

1. Stop the server. A client that is open goes to its reconnecting state:

   ```sh
   kill <pid:conchd>
   ```

2. Type a line in one client while it is down. Do not resend it by hand yet.
3. In a free window, start the server again on the same address and data:

   ```sh
   <root>/bin/conchd serve --listen <address> --web-dir <webdir> --data <root>/data
   ```

What should appear. While the server is down every client says the connection is
lost or stale, retries with a growing delay, and keeps what it already showed
(marked as possibly out of date). After step 3 they reconnect on their own, with
no sign-in again (the credentials are in the database) and the history from before
is still there. A line sent while the server was down is shown as failed or not
confirmed; nothing sends it again for you, so check the conversation before you
retry. No message appears twice.

## P3-10 Remove a member, revoke a credential

1. With `tui-a` open on `test-private`, take `nick-test` out of it:

   ```sh
   set -a; . <root>/operator.env; set +a; <root>/bin/conch admin member remove test-private <id:nick-test>
   ```

2. Try to send or reload in `tui-a`. Then put the membership back:

   ```sh
   set -a; . <root>/operator.env; set +a; <root>/bin/conch admin member add test-private <id:nick-test>
   ```

3. Revoke a credential, as in P3-04, and watch the session that held it.

What should appear. After step 1 the live view of `test-private` ends in `tui-a`
and in any browser tab: the channel leaves the list (browser: `You no longer have
access to #test-private`), and reading, writing and reconnecting are refused. The
other participants are unaffected and keep working. After step 2 `tui-a` can open
the channel again.

## P3-11 No LiveKit, no model service

You have started neither. Check that the core works and that what is optional says
so:

```sh
set -a; . <root>/tui-a.env; set +a; <root>/bin/conch voice status general
```

```sh
set -a; . <root>/tui-a.env; set +a; <root>/bin/conch whoami
```

What should appear. Chat, the two scripted agents, approvals (`conch approvals
list` works, and `agent-a` with the approval script raises one) and the audit log
all work. Voice reports that it is not configured, and the clients show no voice
controls that would work. The agents use fixture scripts: no model service is
involved.

## P3-12 Reset

Stop the agents and TUIs (`ctrl-c`), then:

```sh
go run ./e2e/p3-local --reset
```

`make p3-reset` does the same. It stops the processes the runner started (and any
`conchd`, `conch` or `conch-adapter` that runs from the test root's `bin/`), and
removes the test root. It refuses a directory that does not hold the marker file
`.conch-p3-root`, and it never touches `~/.config/conch` or a `conchd` started
from anywhere else. Start again and look:

```sh
go run ./e2e/p3-local --keep
```

What should appear. The first command ends with `reset: stopped everything` and
the directory is gone (`ls` it to see). The second builds nothing new and gives
a clean community with new ids and new credentials. `~/.config/conch` and any
other Conch you run are as they were.

## P3-13 One real adapter, opt-in

This runs the real Claude Code command as you. Read the warning the runner
prints first. Reset, then start with the flag:

`go run ./e2e/p3-local --reset` then `go run ./e2e/p3-local --keep --real-adapter`

(These two are the only commands in this document that the runner does not
print, which is why they are not in a fenced block: the test that checks every
fenced command against the runner's output would refuse them.)

`agent-a.env` then holds no fixture command and no fixture mode, so
`conch-adapter` runs its default command (`claude -p ... --dangerously-skip-permissions`
with the model `sonnet`), which needs the `claude` CLI installed and signed in.
It can read, change and delete what your account can in the directory you start
it from, and it uses your own quota. Start it only in a directory you would let it
change:

```sh
set -a; . <root>/agent-a.env; set +a; <root>/bin/conch-adapter
```

What should appear. `worker-test` appears with runtime `claude-code` and the
model `sonnet`, and a real task and activity stream when you post in `general`.
If `claude` is missing or not signed in, the run fails with that stated; nothing
falls back to a fixture or to another paid service.

### P3-13 with Codex instead

The same row runs Codex, the other real runtime. Reset, then start with the
runtime named (Codex has no default model, so name one you can use; it is
written into `agent-a.env`, or leave it out and set it when you start the
adapter):

`go run ./e2e/p3-local --reset` then `CONCH_ADAPTER_MODEL=<model> go run ./e2e/p3-local --keep --real-adapter --runtime codex`

(Not fenced, for the reason above.) `agent-a.env` then holds
`CONCH_ADAPTER_RUNTIME=codex` and no fixture, so `conch-adapter` runs its Codex
profile: `codex exec --json --model <model> --sandbox read-only
--skip-git-repo-check --ephemeral --color never`, with the prompt on standard
input. The sandbox is read-only on purpose: Codex can read what your account can
and run commands that do not write, and it uses your own quota. Start the
adapter from the same fenced command as above, in a directory you would let it
read; `CONCH_ADAPTER_WORKDIR` makes the directory something other than the one
you start from, and `CODEX_BIN` names a `codex` that is not on `PATH`. If you
did not set `CONCH_ADAPTER_MODEL` before, set it for this start, or the adapter
exits with one line naming it.

What should appear. `worker-test` appears with runtime `codex` and your model.
When you post in `general`, the run's activity lists each command Codex ran as a
`command_started` and `command_finished` pair, naming the command's first word
only (never its arguments), then `reply_posted`, and the reply is Codex's final
message. If `codex` is missing or not signed in, the run fails with that stated;
nothing falls back to a fixture or to another paid service.

## When you are done

```sh
go run ./e2e/p3-local --reset
```
