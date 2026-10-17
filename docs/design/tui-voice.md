# Design: voice inside the TUI, with a managed `conch-voice` helper

- **Status:** Proposed, 2026-10-17. This is the contract that #540 (Rust) and #541 (Go) implement independently; neither starts until it has been reviewed and merged (issue #539).
- **Issues:** parent #537. This note and the schema: #539. Rust managed mode: #540. Go supervisor: #541. TUI wiring and acceptance: #542.
- **Governed by:** [ADR-007](../adr/ADR-007-client-interfaces.md) ("Client lifecycle": a native client starts, monitors and stops a session-scoped helper over a versioned local control contract), [ADR-006](../adr/ADR-006-rust-voice-client.md), the client design [conch-voice.md](conch-voice.md), and the UX rules [client-experience.md](client-experience.md) (UX-08 to UX-12, UX-23).
- **Wire shapes:** `conch.voice_control.v1`, in [pkg/schema/voice_control.go](../../pkg/schema/voice_control.go). Shared fixtures: [pkg/schema/testdata/voice-control/](../../pkg/schema/testdata/voice-control/) (§16).

A person launches `conch`, types `/voice join`, talks with the key they configured for `conch-voice`, mutes, deafens and leaves, without opening a second terminal. The TUI does this by starting `conch-voice` as its own child process for the length of one voice session, and driving it over two private pipes with a small, versioned protocol. The media engine, the gate, the key watcher and the connection policy are the ones `conch-voice` already has; this note adds a way to own them, not a second implementation.

## 1. What this adds, and what it does not

Adds:

- `conch-voice managed`, a mode of the existing binary that takes its orders from its parent over standard input and reports on file descriptor 3 (§2 to §8). `conch-voice join` and the other commands are unchanged (§12).
- A Go supervisor in `internal/cli` that finds the helper, starts one per voice session, reads its state, sends it commands and stops it within fixed bounds (§10, §14).
- TUI commands and a voice strip that show what the helper confirmed, never what was asked for (§15).

Does not add: a socket, a listener or a daemon; a press or release command (the talk key is read by the helper from the keyboard's event device, as now); any new server endpoint (§17 on API parity); remote audio over SSH; audio on any platform but Linux with PipeWire; a new dependency.

## 2. Processes and pipes

```text
conch (TUI) ─ Supervisor ─ starts ─▶ conch-voice managed      (no shell; own process group; cwd /)
               │  fd 0  pipe, owner writes ─▶  commands, one JSON object per line
               │  fd 3  pipe, owner reads  ◀─  events, one JSON object per line
               │  fd 1  pipe, owner drains ◀─  native noise: read and thrown away
               └  fd 2  pipe, owner drains ◀─  the helper's scrubbed log: a bounded tail kept, never rendered
```

- **Two private, inherited pipes carry the protocol.** The owner creates them and hands the child its ends at start; no other process holds either end (Go's `os/exec` opens every descriptor close-on-exec and passes only `Stdin`, `Stdout`, `Stderr` and `ExtraFiles`). Nothing is bound to a name, a port or a path, so no other process on the machine can reach a helper, and two TUIs cannot reach each other's.
- **Events have a descriptor of their own (decision).** libwebrtc prints a line of its own to standard output on a machine with an NVIDIA GPU, through C's buffered `stdout`, outside any logging (conch-voice.md §5). Buffered native output is flushed in chunks that need not end at a line feed, so on a shared descriptor a native fragment can be glued to the front of a protocol line. A reader that tolerates noise could skip whole foreign lines but could not repair a joined one, and could be shown a line made to look like an event. On fd 3 the helper's protocol writer is the only writer, so every byte there is the protocol. The owner still drains fd 1, without reading it, because a native thread blocked on a full pipe holds C's `stdout` lock and the process then cannot exit (conch-voice.md §8, `lines.rs`).
- **The helper opens fd 3 without `unsafe`.** Rust's standard library takes an inherited descriptor only through `unsafe`, which this repository forbids. On Linux the helper opens `/proc/self/fd/3` for writing, which opens the same pipe again, and checks that what it opened is a FIFO; if fd 3 is missing or is not a pipe it writes one line to standard error and exits 2 before anything else. The inherited descriptor stays open beside the new one; both close when the process ends.
- **stdin carries commands, and is never the talk key.** In managed mode standard input is only the control pipe. The standalone line commands `down`, `up`, `mute`, `deafen` and `quit` do not exist there.
- **Own process group.** The owner starts the helper with `Setpgid`, so the terminal's job-control signals (Ctrl-C, Ctrl-Z, a hang-up of the terminal) do not reach it directly: the owner decides when it stops. The helper has no terminal to read or write: all four descriptors are pipes.
- **Working directory `/`.** Nothing in the helper resolves a path against it; this keeps it from mattering.
- **stderr.** The helper's own log records, scrubbed of secrets and control characters as today (conch-voice.md §8, "Logging"). The owner reads it line by line with the same 4096-byte bound and keeps the last 16 lines, each cut to 200 bytes with control characters removed, for diagnostics (§14). It never interprets them and the TUI never draws them.

## 3. Framing and limits

- One message is one JSON object on one line, UTF-8, ending in a line feed (0x0A). JSON white space around the object is allowed, a carriage return included. A line is read up to its line feed; the bytes past the limit are discarded as they are read, so a reader never holds more than one line's limit.
- Field names are matched exactly, case included. A key given twice makes the line malformed. `null` is accepted for no field. A field the kind does not declare is ignored, so a later writer can add one; neither side ever repeats an ignored field.
- Numbers are integers; an id or seq is from 1 to 2^53-1 so that any JSON reader holds it exactly. Booleans the schema declares are always written, `false` included.
- A partial line at the end of input (no line feed before EOF) is discarded, not acted on.
- The writer's rules are the reader's: `MarshalVoiceControlLine` refuses to write what `ParseVoiceControlCommand` or `ParseVoiceControlEvent` would refuse.

The numbers both sides are held to. The names are the Go constants; the Rust side copies the values.

| Constant | Value | Meaning |
|---|---|---|
| `VoiceControlEventFD` | 3 | The descriptor events are written on. |
| `VoiceControlMaxLineBytes` | 4096 | Longest line, in bytes, without its line feed. |
| `VoiceControlMaxID` | 9007199254740991 | Largest command id or event seq (2^53-1). |
| `VoiceControlMaxInFlight` | 8 | Most commands the owner has written and not seen answered. |
| `VoiceControlEventQueueLines` | 256 | Most events the helper holds for a slow reader; past it an event is dropped and the gap shows in seq. |
| `VoiceControlMaxVersions` | 8 | Most versions a hello offers. |
| `VoiceControlMaxHelperVersionBytes` | 64 | Longest `helper_version`. |
| `VoiceControlMaxServerBytes` | 1024 | Longest `server` in a start. |
| `VoiceControlMaxChannelBytes` | 400 | Longest `channel` in a start (100 characters of up to four bytes). |
| `VoiceControlMaxRetryInMS` | 600000 | Largest `retry_in_ms`. |
| `VoiceControlHelloTimeoutMS` | 5000 | Owner: from starting the process to reading its hello. |
| `VoiceControlStartTimeoutMS` | 10000 | Helper: from writing its hello to reading start. |
| `VoiceControlAckTimeoutMS` | 5000 | Owner: from writing a command (the write included) to reading its ack. |
| `VoiceControlOwnerPollMS` | 250 | Helper: how often it checks that its owner is still its parent. |
| `VoiceControlGateShutMS` | 100 | Helper: from noticing owner loss, or reading leave, to the last microphone frame handed to the track. |
| `VoiceControlCaptureStopMS` | 500 | Helper: from the same moment to the microphone stream closed and playback stopped. |
| `VoiceControlOwnerLossExitMS` | 3000 | Helper: from noticing owner loss to the process having exited. |
| `VoiceControlLeaveExitMS` | 5000 | Helper: from reading leave, standard input still open, to the process having exited. |
| `VoiceControlStopTermAfterMS` | 4000 | Owner: from the start of a stop to SIGTERM, if the helper has not exited. |
| `VoiceControlStopKillAfterMS` | 5000 | Owner: from the start of a stop to SIGKILL, if it has still not exited; then wait and reap. |

## 4. Messages

Every message has `schema` and `kind`. Commands also have `id`; events have `seq`. The Go types are in [voice_control.go](../../pkg/schema/voice_control.go); the golden fixtures show each one (§16).

### Commands (owner to helper, on standard input)

| `kind` | Fields besides `schema`, `kind`, `id` | Does |
|---|---|---|
| `start` | `server` (string), `channel` (string), `principal_id` (integer), `credential` (`stored` or `env`), `muted` (boolean), `deafened` (boolean) | The first command, and the only one before it. Join `channel`'s voice on `server` as `principal_id`, starting muted and deafened as given. |
| `set_mute` | `muted` (boolean) | Set mute to this value. Idempotent: a second `set_mute true` changes nothing. |
| `set_deafen` | `deafened` (boolean) | Set deafen to this value. Idempotent. Undeafening leaves mute as it was (UX-11). |
| `get_state` | none | Write a state snapshot now, whether or not anything changed. |
| `leave` | none | Shut the gate at once (no release tail), leave the room, exit 0. |

- `schema` is the version the owner picked from the hello; in this version always `conch.voice_control.v1`.
- `id` is chosen by the owner, starts at 1 for `start`, and is greater than every id before it.
- `server` is the address as the owner's credential store keys it: `http` or `https`, host in lower case, no user, query or fragment, no trailing slash (`cli.NormalizeServer`). `channel` is the name as the TUI has it from the server: not blank, no control character. Neither is ever echoed back.
- There is no command that presses or releases the talk key, and none that toggles. Typing in the TUI cannot transmit.

### Events (helper to owner, on fd 3)

| `kind` | Fields besides `schema`, `kind`, `seq` | Written |
|---|---|---|
| `hello` | `versions` (array of schema names), `helper_version` (string) | First, before the helper reads anything, opens any device or makes any request. |
| `ack` | `id` (integer), `ok` (boolean), `error` (code, only when `ok` is false) | Once per command that has a usable id, in the order the commands were read. |
| `state` | `phase`, `reason`?, `retry_in_ms`?, `capture`, `transmitting`, `muted`, `deafened`, `blocked`, `ptt`, `ptt_problem`? | On every change, and in answer to `get_state`. A whole snapshot, never a delta. |
| `protocol_error` | `code` | When the helper read a line it cannot act on; then a failed state, then exit 1. |

- `seq` starts at 1 with the hello and goes up by one per event written. A gap means events were dropped for a slow reader (§3).
- The hello's shape is frozen. A helper of any later version writes its hello exactly like this, as `conch.voice_control.v1`, listing every version it speaks; that is how an owner of any version can read it and choose.
- `helper_version` is 1 to 64 bytes of printable ASCII, for a status view.
- Every other string an event carries is a schema name or a closed vocabulary. No event carries free text, an address, a channel name, a principal, a token or a room. Nothing a command or `conchd` said is ever repeated in an event.

## 5. Handshake and ordering

1. The owner starts the process (§10) and writes nothing until it has read a hello.
2. The helper's first event is `hello`, seq 1. If it is anything else, or none arrives within `VoiceControlHelloTimeoutMS`, the owner kills the process: it has been told nothing and can have captured nothing.
3. If the hello does not offer `conch.voice_control.v1`, the owner writes nothing and closes standard input. The helper, never started, exits 0 without writing anything more (`transcript-version-mismatch.json`). The owner reports `helper_incompatible` (§14).
4. Otherwise the owner writes `start` in v1. The helper writes no state before it has accepted a start.
5. The first line the helper reads must be a valid start. If it is not, the helper writes `protocol_error` with the line's own code when that code is one that ends a session (`too_long`, `malformed`, `unknown_schema`, `invalid`), and `not_started` for anything else; then a failed state with reason `protocol_error`, and exits 1, having opened no device (`transcript-bad-first-command.json`). End of input before a start: exit 0, nothing written. No start within `VoiceControlStartTimeoutMS`: failed with `start_timeout`, exit 1.
6. On a valid start the helper writes `state` (phase `starting`) and then `ack ok`. Only then does it read its configuration, check the login, open audio and join (§8.1).
7. **A state a command changed is written before the command's ack.** When the owner reads an `ack ok` for `set_mute true`, the latest state it has read already says `muted: true`, unless something else (the mute key on the keyboard) changed it again since. An ack that changes nothing (a second `set_mute true`) has no state before it.
8. `get_state` is answered by a state and then the ack, always, so the owner can resynchronise after a gap in seq (`transcript-gap-resync.json`).
9. `leave` is answered by a state in phase `stopping` with reason `left`, then the ack; then, when it is done, the final state, and the helper exits 0. A second `leave` while stopping is acknowledged ok. `set_mute` and `set_deafen` once stopping has begun are refused with `stopping`.
10. Acks come in the order commands were read; every command with a usable id gets exactly one. The owner keeps at most `VoiceControlMaxInFlight` unanswered and gives each `VoiceControlAckTimeoutMS`.
11. The last event is a state in phase `stopped` (exit 0) or `failed` (exit 1). Nothing is written after it. The helper gives it up to 250 ms to be written, never more.
12. Seq never repeats or goes back. A reader treats a seq that is not greater than the last as a broken helper.

`transcript-join-mute-leave.json` shows a whole session; the other transcripts show one rule each. `TestVoiceControlTranscripts` checks every transcript against rules 2 to 12, and the Rust side checks its real output against the same rules (§13).

## 6. State

### Phases

```text
starting ─▶ connecting ─▶ connected ◀─▶ reconnecting
    │            │             │              │
    └────────────┴─────────────┴──────────────┴─▶ stopping ─▶ stopped | failed
```

`starting` and `stopping` may go straight to `failed` or `stopped` (a refused login fails in `starting`; a failure need not pass through `stopping`). `connecting` and `reconnecting` follow the helper's own connection policy (conch-voice.md §5); nothing in the protocol asks for a reconnect.

How the helper's existing connection events map (for #540): `Connecting` is `connecting`; `Connected` is `connected`; the SDK's `Reconnecting` is `reconnecting` with no reason; `Waiting { reason, retry_in }` is `reconnecting` with that reason and `retry_in_ms`; `Stopped` (the policy's stop) is `failed` with the reason of §6's table; `Closed` is `stopped`.

### Reasons by phase

| Phase | `reason` | Values |
|---|---|---|
| `starting`, `connecting`, `connected` | absent | |
| `reconnecting` | optional | `room_deleted`, `participant_removed`, `connection_lost`, `reconnect_timed_out`, `connect_failed`, `server_unreachable`, `voice_unavailable`, `no_session`, `server_shutdown` (the names `join --json` uses). With a reason, `retry_in_ms` may say when `conchd` is asked again (0 is at once). |
| `stopping` | required | any reason of `stopped` or of `failed` |
| `stopped` | required | `left` (the owner sent leave), `owner_lost` (§8.3), `signal` (SIGTERM or SIGINT) |
| `failed` | required | see below |

Reasons to fail, and what produces each:

| Reason | Cause |
|---|---|
| `not_signed_in` | No login for the server in the named source, or `conchd` answered 401. |
| `identity_mismatch` | The login is not `principal_id` (§9). |
| `bad_environment` | `credential` is `stored` and `CONCH_TOKEN` is set (§9). |
| `bad_config` | `voice.toml` refused (unknown key, unsafe permissions), or `CONCH_CONFIG_DIR` unusable. |
| `voice_requires_auth` | 400 `voice_requires_auth`. |
| `not_a_person` | 403 for an agent's login. |
| `voice_not_allowed` | 403 `voice_not_allowed` (the invite withheld voice), or whoami says `voice_allowed: false`. |
| `not_a_member` | 404: not a member, no such channel, or membership revoked (seen after a rotation). |
| `voice_not_configured` | 503 `voice_not_configured`. |
| `duplicate_identity` | The same account joined voice elsewhere (LiveKit `DUPLICATE_IDENTITY`). |
| `audio_unavailable` | PipeWire could not be reached, or the speakers could not be opened. A missing microphone is not this: it is `capture: unavailable` and the session goes on. |
| `server_unreachable` | `conchd` could not be reached for the whoami check during `starting`. Once connected, unreachability is a reason to reconnect instead. |
| `start_timeout` | No start within `VoiceControlStartTimeoutMS`. |
| `protocol_error` | After a `protocol_error` event. |
| `internal_error` | Anything else that ended the helper and that it could still report. |

### Fields

- `capture` is the microphone stream, not transmission: `on` while the stream is open with a device (the desktop's microphone indicator is on), `off` while none is open, `unavailable` when it could not be opened or its device went away. It stays `on` while joined with a grant to publish, whether or not anyone is talking (conch-voice.md §3). A listen-only grant never opens it.
- `transmitting` is whether audio is leaving now, release tail included.
- `muted`, `deafened` are the push-to-talk machine's own values, after every source of change (the commands and the mute and deafen keys).
- `blocked` lists every reason a press would do nothing now, in this order, most important first: `not_connected`, `no_publish_grant`, `no_microphone`, `deafened`, `muted`, `key_device_lost`, `max_transmit`. It is always an array.
- `ptt` is whether hold-to-talk can work: `ready` (key device open, talk key bound), `not_configured` (no device or no talk key in `voice.toml`), `unavailable` (configured, never opened since the helper started), `lost` (was open, is not; every press refused until it is open again), or `unknown` (not tried yet: only before the configuration was read). `ptt_problem` says why for `unavailable` and `lost`: `cannot_open`, `refused`, `read_failed`, `ended`, `partial_record`, the `reason` codes of `join --json`'s `key_device` lines.

A state must hold together, and `VoiceControlStateV1.Validate` refuses one that does not:

- `transmitting` only in `connected`, with `capture: on`, `ptt: ready` and nothing in `blocked`.
- `not_connected` in `blocked` in every phase but `connected`.
- `capture: off` in `starting`, `stopped` and `failed`.
- `capture: on` never with `no_microphone`; `capture: unavailable` always with it.
- `muted` exactly when `blocked` has `muted`; `deafened` exactly when it has `deafened`; `ptt: lost` exactly when it has `key_device_lost`.
- `ptt: unknown` never in `connecting`, `connected` or `reconnecting`: the helper tries the key device before it first connects.
- Reasons and `retry_in_ms` as in the table above.

The helper assembles each snapshot after a step of its loop has been applied in full, never between two halves of one change (for example between showing `connected` and telling the push-to-talk machine so).

## 7. Errors

| Code | Where | Means | Helper does |
|---|---|---|---|
| `too_long` | protocol_error | line over 4096 bytes | protocol_error, failed, exit 1 |
| `malformed` | protocol_error | not UTF-8, not one JSON object, a key twice | the same |
| `unknown_schema` | protocol_error | schema missing or not the version in use | the same |
| `invalid` | protocol_error, or ack | a field missing, null or wrong; fatal only when the line has no usable `id` | with an id: ack refused, goes on; without: as above |
| `id_not_increasing` | protocol_error | an id not greater than the one before | protocol_error, failed, exit 1 |
| `not_started` | protocol_error | the first line is not a start | the same |
| `unknown_kind` | ack | a command kind this version does not know | ack refused, goes on |
| `already_started` | ack | a second start | ack refused, goes on |
| `stopping` | ack | set_mute or set_deafen after leaving began | ack refused, goes on |

`VoiceControlErrorCode.Fatal` and `.AckRefusal` are the two columns. The owner writes only valid commands, so any refusal is an owner bug; the supervisor logs it and stops the helper (§14). Refusing instead of failing on an unknown kind lets a later owner probe a helper without ending its session.

**The owner reading events.** An event of a kind it does not know is skipped (it may be a later addition to v1). Anything `ParseVoiceControlEvent` refuses, a first event that is not a hello, a second hello, a seq that is not greater than the last, an ack for an id not next in line, and a write to fd 3 after the final state are a broken helper: the supervisor stops it (§8.5) and reports `bad_event`. A gap in seq is not an error: the owner sends `get_state`.

## 8. Lifecycle and bounds

### 8.1 Start

The helper, in this order: installs its logger; parses its command line; opens fd 3 (exit 2 if it cannot); starts its event writer; writes hello; starts its standard-input reader and its owner check (§8.3); waits for start. Then, after `state starting` and the ack: resolves the configuration directory and reads `voice.toml` for `[keys]` and `[audio]` only (§9); takes the login from the named source; asks `GET /v1/whoami` and compares (§9); connects to PipeWire and opens the speakers; starts the key watcher; and only then runs the existing session loop, which opens the microphone when a session first allows publishing (conch-voice.md §3). **Nothing is captured before a valid start, and nothing before the identity check.**

### 8.2 Leave

On `leave` the helper writes `stopping` (reason `left`), acks, and then: shuts the gate at once with no release tail and mutes the track (within `VoiceControlGateShutMS`); closes the microphone and stops playback (within `VoiceControlCaptureStopMS`); sends the final `stopped` transmit report if a press was open, waiting at most 1 s; closes the room, waiting at most 2 s; writes the final state; exits 0, all within `VoiceControlLeaveExitMS`. Unlike the standalone `quit`, leave has no release tail: the person asked to leave.

### 8.3 Owner loss

The helper's owner is gone when either:

- **its standard input ends** (EOF or a read error). The kernel closes every descriptor of a process that dies, SIGKILL included, so the end of the control pipe is the ordinary sign; or
- **its parent is no longer the process that started it.** At start the helper records its parent's pid (`std::os::unix::process::parent_id`, no `unsafe`) and compares every `VoiceControlOwnerPollMS`. A dead owner's children are re-parented, so this catches a death the pipe misses (a write end leaked into another process). An owner that died before the helper recorded its parent has already closed its end of the pipe, so the first rule catches that one.

Either way the helper writes `stopping` (reason `owner_lost`) and does what leave does, with tighter bounds: gate shut within `VoiceControlGateShutMS`, capture and playback stopped within `VoiceControlCaptureStopMS`, the report and the room each given at most 1 s, and the process gone within `VoiceControlOwnerLossExitMS` of noticing. **This holds with a key device being read**: in managed mode the end of input is never "a release and no more" (§12). A watchdog in the helper ends the process at `VoiceControlOwnerLossExitMS` whatever is still running, by the same means `lines::exit` uses today. If leave was already under way, its reason stands and the tighter bounds apply from the EOF.

A hung owner that is still alive (stopped with SIGSTOP, or deadlocked) is not owner loss: the helper cannot tell it from a busy one, and nothing it does depends on the owner reading. Its events are dropped once 256 wait (§3), the talk key still works only while held, and the owner resynchronises with `get_state` when it reads again. There is no keepalive: one would end a voice session whenever the TUI is suspended or slow, and would add nothing to the guarantees above.

`PR_SET_PDEATHSIG` is not used. The helper cannot set it without `unsafe`, and Go's `SysProcAttr.Pdeathsig` fires when the thread that started the child exits, not the process, which the Go runtime does not let a caller control.

### 8.4 Signals

SIGTERM and SIGINT to the helper do what leave does, with reason `signal` and exit 0 (`transcript-signal.json`). Because of `Setpgid`, the helper gets these only when someone sends them to it: the supervisor's escalation, or a person with `kill`.

### 8.5 Stop, from the owner's side

The supervisor stops a helper (on `/voice leave`, a switch of channel, sign-out, a server or identity change, the TUI's exit, or a broken helper) by:

1. writing `leave`, if the control pipe still works, and then closing standard input straight away (the helper then has both reasons to go, and the owner-loss bounds apply);
2. waiting for the process to exit; at `VoiceControlStopTermAfterMS` from step 1, sending SIGTERM;
3. at `VoiceControlStopKillAfterMS` from step 1, sending SIGKILL;
4. waiting for it (`cmd.Wait`) and so reaping it.

Signals go only to the `*os.Process` this supervisor started, and only before `Wait` has returned, so the pid cannot have been reused. Never `pkill`, never a process name, never a process group of anyone else. **No new helper is started until the old one has been reaped.** So from the start of a stop, a helper that is working has stopped capturing within `VoiceControlCaptureStopMS` and exited within `VoiceControlOwnerLossExitMS`; one that is not is gone by SIGKILL at `VoiceControlStopKillAfterMS`, and the kernel closes its microphone with it.

The TUI's own exit path (a key, SIGINT or SIGTERM through `ctx`) calls `Close` and waits for it, bounded by `VoiceControlStopKillAfterMS` plus one second. If the TUI is killed outright, §8.3 applies.

### 8.6 Failed start-up

| What happens | Owner sees | Reported as (§14) | Capture |
|---|---|---|---|
| no executable found, or one that fails the checks of §10 | nothing started | `helper_not_found`, `helper_untrusted` | none |
| the process cannot be started | `exec` error | `spawn_failed` | none |
| exits before hello (an old binary without `managed` exits 2) | EOF on fd 3, exit status | `helper_incompatible` | none |
| no hello in time | timer | `hello_timeout`, then SIGKILL | none |
| hello without v1 | hello | `helper_incompatible` | none |
| start refused, identity wrong, login missing, audio missing | failed state | that state's reason | none: all before the session loop |
| no ack in time | timer | `ack_timeout`, then §8.5 | stopped by §8.5 |

## 9. Server, profile and credentials

The helper joins exactly the server and account the TUI is using, or fails. It never falls back to another.

**Environment.** The supervisor builds the child's environment from its own, then:

- removes every variable whose name begins with `CONCH_` (`CONCH_TOKEN`, `CONCH_SERVER`, `CONCH_CONFIG_DIR`, `CONCH_CHANNELS`, `CONCH_AUTHOR`, `CONCH_MOUSE`, `CONCH_VOICE_HELPER` and any later one), and `RUST_LOG`, `RUST_BACKTRACE` and `RUST_LIB_BACKTRACE`;
- sets `CONCH_CONFIG_DIR` to the TUI's own configuration directory, as `cli.configDir` resolved it (always absolute), when there is one. When there is none, it sets nothing, and the helper resolves the same none from the same `XDG_CONFIG_HOME` and `HOME` (the rule both programs share, `internal/cli/testdata/config-dir-vectors.json`);
- sets `CONCH_TOKEN` to the TUI's token **only** when the TUI's own credential came from `CONCH_TOKEN`; the start then says `credential: env`.

Nothing else is added, and nothing secret is ever in the command line, which is the executable's path and `managed`.

**The credential.** With `credential: stored`, the helper takes the login `conch login` stored for `server` in `credentials.json` in the configuration directory, with every check it makes today (a regular file, not readable by others); `CONCH_TOKEN` must then be absent from its environment, or it fails with `bad_environment` rather than use a token the owner did not mean. With `credential: env`, it takes `CONCH_TOKEN` and nothing else. Why the token is not sent over the pipe instead (decision): the stored login is the preferred route (the issue says so); `CONCH_TOKEN` is already in the TUI's own environment, visible to the same user in the same way, so passing it on adds no exposure; and a control channel that never carries a secret can be logged, captured in a test and shown in a diagnostic without a second thought. A TUI with no credential at all (a server with authentication off) does not start a helper: voice needs a signed-in person, and the TUI says so.

**The identity check.** Before it asks for a voice session or opens any device, the helper asks `GET /v1/whoami` with the login it found and fails with `identity_mismatch` unless the answer's `id` is `principal_id`. This is what makes "the same account" true: `credentials.json` may have been changed by `conch login` since the TUI started, and two profiles may hold different logins for one server. It also fails early, with `not_a_person` or `voice_not_allowed`, when whoami says the login is an agent or has no voice. (#540 adds a Rust copy of `WhoAmIResponseV1`, round-tripped against the existing `whoami-response-v1*.json` fixtures like the other Rust copies.)

**Server and channel.** The helper uses `start.server` and `start.channel`. In managed mode `CONCH_SERVER` (removed anyway), `--server`, `server` and `channel` in `voice.toml`, the stored-login fallback and the default server are not consulted, and a `voice.toml` that sets `server` or `channel` is not an error: the file is shared with standalone use. `[keys]` and `[audio]` are read as today, so the key device and audio devices a person set up for `conch-voice` are the ones the TUI uses.

**Two TUIs.** Each TUI has its own configuration directory (`CONCH_CONFIG_DIR`, UX-08), its own supervisor and its own helper, started by it and stopped only by it. Neither can see, signal or reconfigure the other's.

## 10. Finding the helper

The supervisor takes the first of these that exists, and stops there:

1. `CONCH_VOICE_HELPER`, if set to more than white space: an absolute path, used exactly. A relative one is refused, naming the variable, and never resolved against the working directory.
2. `conch-voice` in the directory of the running `conch` (`os.Executable`, which Linux reports with links resolved).
3. `conch-voice` found by `exec.LookPath` in `PATH`. Go refuses a result found through an empty or `.` entry of `PATH` (`exec.ErrDot`); that is treated as not found, and so is any result that is not absolute.

The file found (links resolved) must be a regular file, executable, owned by the user or by root, and not writable by group or others; and its directory must not be writable by group or others. A candidate that exists and fails is `helper_untrusted`, and the supervisor does not go on to the next: what runs should be predictable, and a planted file beside `conch` should not quietly hand over to another. The supervisor never runs a shell: `exec.Cmd` with the resolved path as both `Path` and `Args[0]`, and `Args[1]` = `managed`.

A missing or incompatible helper leaves text chat as it was and says what to do: install `conch-voice` beside `conch` or in `PATH`, or point `CONCH_VOICE_HELPER` at it; and that `conch` and `conch-voice` must come from the same release. Whether a release bundles the helper is packaging, decided elsewhere; nothing here assumes it.

## 11. Duplicate identity, reconnection and retry

- **One connection per account per room.** That is the server's rule and stays so (voice-control-plane.md §9): a second connection with the same identity displaces the first. A helper that is displaced fails with `duplicate_identity` and does not try again (conch-voice.md §5). Two TUIs on independent profiles signed in as the same person are two connections of one account: joining in one displaces the other. They are independent processes, not independent voice identities.
- **Joining where the account already is.** Before `/voice join`, the TUI looks at the presence it already reads (#187). If its own principal is in the target room, it does not start the helper; it says that this account is already in voice from another client and that joining here will disconnect it, and starts only if `/voice join` is repeated within 10 s.
- **Reconnection** belongs to the helper's policy (conch-voice.md §5): rotations, lost connections and an unreachable server are `reconnecting` with a reason, and the helper comes back by itself. The supervisor never restarts a helper and never reconnects.
- **Retry** is the person's: after `failed`, or a helper that ended without a final state, the TUI offers `/voice retry` when the reason is one a new attempt could fix (§15). A retry is a new process with a new handshake, for the same channel, with the mute and deafen the person last set. Nothing restarts by itself, so nothing reopens a microphone by itself.

## 12. Standalone `conch-voice` is unchanged

`conch-voice join`, `devices` and `keys` keep their command lines, their `--json` output, their line commands on standard input, and their exit codes. In particular the end of standard input with a key device watched is still "a release and no more" for `join` (conch-voice.md §8); only `managed` treats it as the owner gone. `managed` is a new subcommand beside them, and the `--json` objects of `join` are not this protocol (a case in `cases.json` shows the reader refusing one).

## 13. The helper side (#540)

Build:

- `conch-voice managed`, with no positional arguments. Options: `--log-level` as for `join`; and, hidden from `--help`'s summary and named as test-only, the test microphone and sink (`--mic`, `--sink`) and one test source of key events standing in for the key device (for example the existing record reader on a named pipe). Test options are argv only, never read from the environment or `voice.toml`, and the supervisor never passes them.
- The ordering of §8.1; the rules of §3 and §5; the snapshots of §6 from the existing session loop's state; the error handling of §7; leave, owner loss and signals with the bounds of §3 (`VoiceControlGateShutMS`, `VoiceControlCaptureStopMS`, `VoiceControlOwnerLossExitMS`, `VoiceControlLeaveExitMS`); the configuration and credential rules of §9; a bounded, non-blocking event writer (`VoiceControlEventQueueLines`; the `LineQueue` idea, but dropping without a marker, since seq shows the gap).
- `set_mute` and `set_deafen` turn into the machine's existing toggle only when the state differs, inside the session loop, so a key press at the same moment is applied in order and the snapshot shows the result.
- Rust copies of the shapes, read with the rules of §3 (`wire_object!` with unknown fields ignored, and duplicate keys, nulls and wrong types refused), and written with `serde_json` compactly.

Pass:

- every golden fixture of §16 round-tripped (commands decoded, events encoded), like `types.rs` does for the `voice-*.json` fixtures;
- every `direction: command` case in `cases.json` with the same result and code;
- the rules of §5, checked against the helper's real output in fake-SDK runs driven by the owner side of each transcript (the helper's lines are compared by kind, phase and reason, not byte for byte);
- `sensitive.json`: run with its fake secrets as the login token, join token and room name, and no line written to fd 3 contains any of them;
- tests with the bounds as numbers: owner EOF with the key watcher active (and while transmitting) stops capture and exits within the bounds; a version-mismatch or bad first command exits with no capture; malformed and oversized commands; repeated set-state; standalone `join` unchanged.

## 14. The supervisor side (#541)

A focused package, `internal/cli/voicehelper`, with no TUI code in it. Its public interface (names may be adjusted in review, not the behaviour):

```go
// Profile is the account a helper joins as. Built once per signed-in TUI.
type Profile struct {
	Server      string                        // as cli.NormalizeServer wrote it
	ConfigDir   string                        // the TUI's configuration directory; "" if it has none
	PrincipalID int64                         // from the TUI's own whoami
	Credential  schema.VoiceControlCredential // stored, or env when the TUI's token came from CONCH_TOKEN
	Token       string                        // only with env; never logged, never in Status or errors
}

type Options struct {
	HelperPath string // from CONCH_VOICE_HELPER; "" to search (§10)
	// For tests only: extra helper arguments and shortened timings.
	TestArgs []string
	Timings  Timings
}

func New(p Profile, o Options) *Supervisor

func (s *Supervisor) Join(ctx context.Context, channel string) error // start one helper; returns after the start's ack
func (s *Supervisor) Leave(ctx context.Context) error                 // §8.5; returns once reaped
func (s *Supervisor) Retry(ctx context.Context) error                 // Join again for the last channel, after a failure
func (s *Supervisor) SetMute(ctx context.Context, on bool) error      // returns after the ack
func (s *Supervisor) SetDeafen(ctx context.Context, on bool) error    // returns after the ack
func (s *Supervisor) Refresh(ctx context.Context) error               // get_state
func (s *Supervisor) Status() Status                                  // the latest snapshot
func (s *Supervisor) Updates() <-chan Status                          // capacity 1, latest wins, never blocks the supervisor
func (s *Supervisor) Diagnostics() []string                           // the stderr tail (§2), sanitized
func (s *Supervisor) Close(ctx context.Context) error                 // Leave, and refuse every later Join

var ErrBusy, ErrNotRunning, ErrClosed, ErrNoCredential error
```

`Join` while a helper is running or stopping is `ErrBusy`; commands with no helper are `ErrNotRunning`; anything after `Close` is `ErrClosed`; a `Profile` with no credential is `ErrNoCredential`, before anything is started. `Join` takes at most `VoiceControlHelloTimeoutMS` plus `VoiceControlAckTimeoutMS`; the TUI calls it from a `tea.Cmd`, never from `Update`.

```go
type Status struct {
	Gen           uint64 // which helper process; 0 before the first
	Channel       string // the channel this helper was started for
	Phase         Phase  // idle, then the wire phases; idle again after Leave or Close
	Reason        string // a wire reason (§6) or a supervisor reason (below)
	Retryable     bool
	RetryIn       time.Duration
	Capture       schema.VoiceControlCapture
	Transmitting  bool
	Muted         bool
	Deafened      bool
	Blocked       []schema.VoiceControlBlock
	PTT           schema.VoiceControlPTT
	PTTProblem    schema.VoiceControlPTTProblem
	HelperVersion string
}
```

`Status` is made only from the helper's states and the supervisor's own observations: never from what was asked. Reasons the supervisor adds, which never cross the wire: `helper_not_found`, `helper_untrusted`, `helper_incompatible`, `spawn_failed`, `hello_timeout`, `ack_timeout`, `bad_event` (§7), `helper_exited` (exited without a final state), `helper_crashed` (ended by a signal the supervisor did not send, or exit 101). Each of these is phase `failed`.

How it works: one goroutine reads fd 3 (bounded lines, `schema.ParseVoiceControlEvent`, the reader's checks of §7) and publishes each state; one goroutine writes commands (a write that does not finish within `VoiceControlAckTimeoutMS` is a broken helper); one drains fd 1 and discards it; one drains fd 2 into the bounded tail; and `cmd.Wait` runs once, after which nothing signals the process. It keeps the mute and deafen the person last set, for a retry, and forgets them on `Close`.

Pass: every `direction: event` case in `cases.json`, and the transcripts' owner side, with `schema.ParseVoiceControlEvent` and `schema.MarshalVoiceControlLine` (the Go readers are the specification of the owner's reading). Tests with a fake helper (a Go test binary speaking the protocol): handshake, version mismatch, hello and ack timeouts, oversized, malformed and noisy output on fd 1 and fd 3, ack and state ordering, a gap and its resync, broken pipes, a crash, repeated join and leave, the stop escalation with each bound, reaping, two supervisors that cannot affect each other's process or environment, the environment of §9, and discovery of §10. No microphone, PipeWire or LiveKit; run under `-race`.

## 15. The TUI side (#542)

Commands (checked against the composer's existing `/net`, `/w` and `//`; no collision):

| Command | Does |
|---|---|
| `/voice join [channel]` | Join the named channel's voice, or the channel on screen, named in the reply. Refused while a helper runs (`/voice leave` first). |
| `/voice leave` | Stop the helper (§8.5). |
| `/voice mute on` / `off` | `SetMute`. Shows "muting…" until the ack; the strip then shows the state's `muted`. |
| `/voice deafen on` / `off` | `SetDeafen`, likewise. |
| `/voice retry` | `Retry`, when the last failure is retryable. |
| `/voice status` | The full `Status` in words, the helper's version and path, and the diagnostics tail. |
| `/voice help` | These commands, how to set the talk key (`conch-voice devices`), and the SSH note below. |

The voice strip is drawn from the latest `Status` only, beside the text connection state and never mixed with it: destination channel (which browsing text channels never changes, UX-02), voice connection, capture, transmit, mute and deafen, push-to-talk, and a short reason.

| Status | Strip says |
|---|---|
| idle | voice off · `/voice join` |
| `starting`, `connecting` | voice: joining #channel… |
| `connected`, nothing blocked, `ptt: ready` | voice: #channel · mic on · hold \<key\> to talk |
| `transmitting` | voice: #channel · TALKING |
| `muted` / `deafened` | … · muted / … · deafened (both shown when both) |
| `capture: off` with `no_publish_grant` | … · listen only |
| `capture: unavailable` | … · no microphone |
| `ptt: not_configured` | … · no talk key: set `[keys]` in voice.toml (`conch-voice devices`) |
| `ptt: unavailable` / `lost` | … · talk key unavailable (\<ptt_problem in words\>) / talk key lost: presses refused |
| `reconnecting` | voice: reconnecting (\<reason\>), retry in N s |
| `stopping` | voice: leaving… |
| `failed` | voice stopped: \<reason in words\>, and `/voice retry` when retryable |

Capture and transmission are shown separately: a joined user with the gate shut is "mic on", never "mic off" (UX-11). The roster and speaking marks are #187's, from the TUI's own presence subscription; the helper does not send a roster.

Retryable: `duplicate_identity` (with the warning of §11), `server_unreachable`, `voice_unavailable`, `audio_unavailable`, `start_timeout`, `internal_error`, `protocol_error`, and the supervisor's `hello_timeout`, `ack_timeout`, `bad_event`, `helper_exited`, `helper_crashed`, `spawn_failed`. Not retryable, with what to do instead: `not_signed_in` and `identity_mismatch` (sign in again), `bad_environment` (a bug: report it), `bad_config` (fix `voice.toml`), `voice_requires_auth`, `not_a_person`, `voice_not_allowed`, `not_a_member`, `voice_not_configured` (nothing the person can do here), `helper_not_found`, `helper_untrusted`, `helper_incompatible` (install a matching `conch-voice`).

Text chat never waits on voice: every supervisor call runs in a `tea.Cmd`, and a failed helper leaves the composer, the timeline and the presence roster as they were. Sign-out, a server or identity change, and the TUI's exit call `Close` first.

**Over SSH** the helper runs on the machine where `conch` runs, and uses that machine's microphone, speakers and keyboard, not those of the computer the SSH session comes from. `/voice help` says so; no remote audio is added.

## 16. Fixtures

All in `pkg/schema/testdata/voice-control/`, read in place by the Go tests (`pkg/schema/voice_control_test.go`) and by the Rust helper's tests. Like every fixture there, they are immutable once merged (`scripts/schema-compat.sh`); a change is a new file. They are in a directory of their own so that `voice/crates/conch-voice-api`'s check that every top-level `voice-*.json` fixture has a Rust type does not apply to them before #540 adds those types.

Golden messages, each byte-identical to its Go type's indented encoding:

- Commands: `command-start.json`, `command-start-env.json`, `command-set-mute.json`, `command-set-deafen.json`, `command-get-state.json`, `command-leave.json`.
- Events: `event-hello.json`, `event-ack.json`, `event-ack-refused.json`, `event-protocol-error.json`.
- States: `event-state-starting.json`, `event-state-connected.json`, `event-state-transmitting.json`, `event-state-muted-deafened.json`, `event-state-listen-only.json`, `event-state-no-microphone.json`, `event-state-no-talk-key.json`, `event-state-key-device-lost.json`, `event-state-reconnecting.json`, `event-state-reconnecting-backoff.json`, `event-state-reconnecting-sdk.json`, `event-state-stopping.json`, `event-state-stopped.json`, `event-state-stopped-owner-lost.json`, `event-state-failed-duplicate-identity.json`, `event-state-failed-identity-mismatch.json`.

Conformance, for both readers: `cases.json`, each case a line (as text, as hex bytes, or as a file) and the result wanted: accepted with its kind, refused with its code (and id, when the line had one), or skipped. It covers every code, unknown and later versions, the limits on both sides of 4096 bytes, wrong types, nulls, duplicate keys, case, control characters, invalid UTF-8, the absence of any press command, and each consistency rule of a state. The long lines are `lines/command-at-limit.txt`, `lines/command-too-long.txt`, `lines/event-at-limit.txt` and `lines/event-too-long.txt`.

Ordering, as whole sessions of steps (an owner or helper message, the owner closing standard input or sending a signal, the helper's exit code): `transcript-join-mute-leave.json`, `transcript-owner-lost.json`, `transcript-signal.json`, `transcript-version-mismatch.json`, `transcript-bad-first-command.json`, `transcript-refused-commands.json`, `transcript-identity-mismatch.json`, `transcript-rotation-then-duplicate.json`, `transcript-gap-resync.json`.

Secrets: `sensitive.json` lists the fake secrets the tests use and the fragments no field name may contain. `TestVoiceControlCarriesNoSecrets` checks the Go types and every fixture against it; the Rust tests check the helper's real output against it.

## 17. Security and parity

- **Nothing secret on the channel, by construction**: no field could carry a token (the field lists are pinned by `TestVoiceControlFieldsAreAnExactList`), events carry no free text, and the login travels as it already does (§9). Join tokens and room names never leave the helper, as today.
- **Nothing on the channel can transmit.** Presses come only from the key device the person configured. A shut gate reopens only on a new press, after a rejoin too (UX-10).
- **Nothing from the helper reaches the terminal unchecked.** The owner shows vocabularies mapped to its own words, and the helper's version only after the printable-ASCII check; fd 1 is discarded unread; fd 2 is shown only by `/voice status`, sanitized.
- **Ownership.** One helper per TUI, signalled only through its own `*os.Process` before it is reaped (§8.5). The helper has no way to be reached by any other process.
- **API parity (ADR-000 D6).** Every server action the helper takes is the public API (`/v1/whoami`, the voice session, transmit reports, presence), as `conch-voice` does today. Mute, deafen and the helper's life are local media state, not server capabilities, so there is nothing for the REST/WS API or MCP to gain.
- A security-reviewer pass is required on #540, #541 and #542 (credentials, process ownership and the transmit lifecycle), as #537 says.

## 18. Decisions, and what needs review

Decided here, where the issues left a choice:

1. **Events on a dedicated descriptor** rather than framing that tolerates noise (§2).
2. **The helper speaks first**, and its hello is frozen, so any owner can read any helper's offer (§5).
3. **No secret on the pipe**; the stored login first, `CONCH_TOKEN` passed by environment only when the TUI itself got it that way (§9).
4. **An identity check with whoami before anything is opened** (§9), so a changed `credentials.json` cannot make the helper join as someone else.
5. **Owner loss is EOF or a changed parent**, with no keepalive and no `PDEATHSIG` (§8.3).
6. **Whole snapshots**, written before the ack of the command that caused them, and a seq to show drops (§5, §6).
7. **One process per voice session**: switching channel is a stop and a new start, so no channel switching exists inside a helper.
8. **Leave has no release tail** (§8.2).
9. **Stop at the first discovered candidate** rather than falling through to the next (§10).
10. **Unknown commands are refused, not fatal; malformed lines are fatal** (§7).

Needs review before #540 and #541 start: this note and the new types (#539's acceptance criterion). The shapes are new and additive, so under the schema-change skill they need no version bump; nothing published changes. No new dependency is proposed.
