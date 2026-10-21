# Design: voice inside the TUI, with a managed `conch-voice` helper

- **Status:** Proposed, 2026-10-17. This is the contract that #540 (Rust) and #541 (Go) implement independently; neither starts until it has been reviewed and merged (issue #539).
- **Issues:** parent #537. This note and the schema: #539. Rust managed mode: #540. Go supervisor: #541. TUI wiring and acceptance: #542.
- **Governed by:** [ADR-007](../adr/ADR-007-client-interfaces.md) ("Client lifecycle": a native client starts, monitors and stops a session-scoped helper over a versioned local control contract), [ADR-006](../adr/ADR-006-rust-voice-client.md), the client design [conch-voice.md](conch-voice.md), and the UX rules [client-experience.md](client-experience.md) (UX-08 to UX-12, UX-23).
- **Wire shapes:** `conch.voice_control.v1`, in [pkg/schema/voice_control.go](../../pkg/schema/voice_control.go). Shared fixtures: [pkg/schema/testdata/voice-control/](../../pkg/schema/testdata/voice-control/) (§16). Version 2, `conch.voice_control.v2` (live device switching and the open microphone, issue #653), is §20: [pkg/schema/voice_control_v2.go](../../pkg/schema/voice_control_v2.go) and [pkg/schema/testdata/voice-control-v2/](../../pkg/schema/testdata/voice-control-v2/). §1 to §19 describe v1 and are unchanged by it; where v2 differs, §20 says so.

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
| `VoiceControlEventQueueLines` | 256 | Most events the helper holds for a slow reader; past it a state or protocol_error is dropped and the gap shows in seq. Acks, and the state that answers `get_state`, are not dropped: they wait behind the others in places of their own, `2 × VoiceControlMaxInFlight` of them (§5, rules 8 and 10). |
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

- `schema` is the version the owner picked from the hello; in this version always `conch.voice_control.v1` (for v2, §20.2).
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
7. **A state a command changed is written before the command's ack.** When the owner reads an `ack ok` for `set_mute true`, the latest state it has read already says `muted: true`, unless something else (the mute key on the keyboard) changed it again since. An ack that changes nothing (a second `set_mute true`) has no state before it. If that state was dropped for a slow reader (§3), the ack still comes, after a gap in seq that says so.
8. `get_state` is answered by a state and then the ack, always, whatever the helper's queue holds, so the owner can resynchronise after a gap in seq (`transcript-gap-resync.json`).
9. `leave` is answered by a state in phase `stopping` with reason `left`, then the ack; then, when it is done, the final state, and the helper exits 0. A second `leave` while stopping is acknowledged ok. `set_mute` and `set_deafen` once stopping has begun are refused with `stopping`.
10. Acks come in the order commands were read; every command with a usable id gets exactly one, however slowly the owner reads. The owner keeps at most `VoiceControlMaxInFlight` unanswered and gives each `VoiceControlAckTimeoutMS`.
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

The candidate is resolved one link at a time (`os.Readlink`, at most 40 links), never with a single `EvalSymlinks` that checks only where it ends. The candidate, and each link target in turn, must sit in a directory that, with every parent up to `/`, is owned by the user or by root and not writable by group or others (as OpenSSH's `safe_path`): otherwise another user who can write one of those directories could plant a link there, to a program however trusted, and choose what runs. The file it finally names must be a regular file, executable, owned by the user or by root, and not writable by group or others. A candidate that exists and fails is `helper_untrusted`, and the supervisor does not go on to the next: what runs should be predictable, and a planted file beside `conch` should not quietly hand over to another. The supervisor never runs a shell: `exec.Cmd` with the resolved path (no link left in it) as both `Path` and `Args[0]`, and `Args[1]` = `managed`.

A missing or incompatible helper leaves text chat as it was and says what to do: install `conch-voice` beside `conch` or in `PATH`, or point `CONCH_VOICE_HELPER` at it; and that `conch` and `conch-voice` must come from the same release. Whether a release bundles the helper is packaging, decided elsewhere; nothing here assumes it.

## 11. Duplicate identity, reconnection and retry

- **One connection per account per room.** That is the server's rule and stays so (voice-control-plane.md §9): a second connection with the same identity displaces the first. A helper that is displaced fails with `duplicate_identity` and does not try again (conch-voice.md §5). Two TUIs on independent profiles signed in as the same person are two connections of one account: joining in one displaces the other. They are independent processes, not independent voice identities.
- **Joining where the account already is.** Before `/voice join`, the TUI looks at the presence it already reads (#187). If its own principal is in the target room, it does not start the helper; it says that this account is already in voice from another client and that joining here will disconnect it, and starts only if `/voice join` is repeated within 10 s.
- **Its own session just left is not another client** (#558). Presence is read on the server's sweep, so it can go on listing this account for a while after `/voice leave`. When the person leaves, the TUI notes the `joined_at` presence then gives this account in the channel-wide room of the channel its helper is in, if that channel is on screen and the helper is not failed (a failed one may have been displaced, and the entry is then the other client's). The server gives each connection its own `joined_at`, so an entry of this account with a noted join time is that session and is neither warned about nor shown as "from another client"; any other entry is another client, as before. Nothing is noted when presence did not show the session, or showed it only in another channel: then the warning stands. The next join clears the note.
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
- **Nothing on the channel can transmit.** The gate is opened in push-to-talk mode by a press of the key device the person configured, and in open mode (v2, §20) by the microphone's own sound crossing the configured level; no command, timer, reconnection or cleared condition opens it in either mode. `set_talk_mode` only chooses which of the two may open it, and mute and deafen shut it in both. A shut gate reopens only on a new press, or in open mode on a fresh onset, after a rejoin too (UX-10).
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

## 19. Acceptance record (#542)

What #542 built is §15: the `/voice` commands, the strip and the lifecycle, in `internal/cli/tui/voicectl.go`, driving the supervisor of §14. Where §15 leaves a word open: the strip says "hold the talk key to talk", not the key's name, because the key is bound in `voice.toml`, which only the helper reads; `/voice join <channel>` takes only a channel of the TUI's own list; the §11 check reads the presence of the channel on screen, so for that channel it warns only when presence shows this account in its voice, and for a channel named that is not on screen, whose presence it does not read, it always asks for the same repeat within 10 s ("this account may already be in #x's voice from another client"); and a server or identity change can happen only at start-up or through a `whoami` that names someone else, so it is the TUI's exit, a refused login (`whoami` or the message socket answering 401) and such a `whoami` that call `Close`. The exit waits, within the same bound, for a helper already being stopped by one of the other two, and no helper is started for a new account until the previous account's has been closed. `Join` and `Retry` wait, within their context, for a helper that has already ended (its final state read or its process gone) to be reaped, instead of answering that voice is busy.

| Check | How | Status |
| --- | --- | --- |
| Commands, strip, refusals, text unaffected, nothing called in `Update` | `internal/cli/tui/voicectl_test.go`, with a stand-in supervisor | automated, in `go test ./...` |
| The supervisor against a fake helper: two owners, kill and restart, stop bounds | `internal/cli/voicehelper` | automated, in `go test ./...` |
| Real TUIs, their own `conch-voice`, `conchd` and LiveKit, tones and a counting sink: join, typing, talk key, mute, deafen, a second owner, the owner killed while transmitting, leave, quit while transmitting, one account in two TUIs, retry, removal, voice refused (no voice in the invite; a stored login that is another account's), LiveKit restarted, and a 40 s outage waited out (#559) | `go run ./e2e/voice managed` (Docker and the voice toolchain; not in CI) | automated, opt-in |
| A real headset and talk key on the maintainer's Pop!_OS machine: press and release while typing and while another window has the focus, key device loss, quitting and `kill -9` while transmitting, the SSH note | [P3-15 in the walkthrough](../p3-walkthrough.md#p3-15-voice-in-the-tui-opt-in) | **pending**: not done; the synthetic runs are not this acceptance |

## 20. Version 2: switching devices live, and an open microphone (#653)

- **Status:** Proposed, 2026-10-19, for the project owner's sign-off (issue #653, parent #662). It is the contract that #654 (the open-microphone engine), #655 (live device switching), #656 (the helper speaking v2) and #657 (the TUI using v2) implement; none starts until it is merged.
- **Decisions recorded on #653 and #662 before this was written:** the open microphone is level-triggered (not always on while unmuted); sound from a virtual microphone is the person's own transmission, under their login, with no capability or server change; the owner signs off this specification by reviewing its PR and merges it himself.
- **Wire shapes:** `conch.voice_control.v2`, in [pkg/schema/voice_control_v2.go](../../pkg/schema/voice_control_v2.go). Shared fixtures: [pkg/schema/testdata/voice-control-v2/](../../pkg/schema/testdata/voice-control-v2/) (§20.8). The ADR is unchanged: ADR-004 makes push-to-talk the default transmit mode, and this adds an opt-in second mode beside it.

v2 lets the owner do two things a v1 session cannot: change the microphone or the speakers without leaving voice (`set_devices`), and choose an **open microphone** (`set_talk_mode`), where the microphone's own sound opens the transmit gate so that a virtual microphone fed by another program (a text-to-speech program, for example) is heard without anyone holding the talk key. Everything else is v1: the processes and pipes (§2), the framing and limits (§3), the handshake and ordering rules (§5), the phases, reasons and blocks (§6), the errors (§7), the lifecycle bounds (§8), credentials (§9) and discovery (§10) hold unchanged, and every number of §3's table applies.

### 20.1 Why a version, and why one

Facts on `main` before this section:

- **A v1 state cannot say "open microphone".** `VoiceControlStateV1.Validate` refuses `transmitting` unless `ptt` is `ready`, and the owner treats a state that fails `Validate` as a broken helper (`bad_event`, §7). A helper transmitting on sound with no talk key would be stopped, or would have to misreport itself.
- **A command kind cannot be added to v1 safely.** A v1 helper answers an unknown kind with `unknown_kind`, and the supervisor treats any refusal as an owner bug and stops the helper (`helper_incompatible`, §14). A new TUI talking to an older helper would lose its voice session on the first `set_devices`.
- **v1 is published and stays byte-identical** (`scripts/schema-compat.sh`). The protocol's own mechanism for this is the hello (§5): its shape is frozen, it lists every version the helper speaks, and the owner picks one.
- **One version for both changes.** v2 and a v3 later would mean two sign-offs, two fixture sets and two negotiation paths for two features the same people need at the same time. If live switching were not wanted, dropping `set_devices` from v2 would still leave a v2 for the open microphone; it is wanted, so both are here.

### 20.2 Negotiation

1. The hello is unchanged: a v2 helper writes `{"schema": "conch.voice_control.v1", "kind": "hello", "seq": 1, "versions": ["conch.voice_control.v1", "conch.voice_control.v2"], ...}` (`event-hello-v1-and-v2.json`). Any owner reads it with the v1 reader.
2. **The owner picks the version by the schema of its start.** A v2 owner writes `start` in v2 (`transcript-negotiation-v2-owner.json`); a v1 owner writes it in v1 and the whole session is v1, as today (`transcript-negotiation-v1-owner.json`). A new owner talking to a helper that offers only v1 uses v1 and falls back to #652's rejoin for a device change; a v1 session is always push-to-talk, even when `voice.toml` says `open` (§20.6).
3. **The version is fixed for the session.** The helper reads its first line with the reader of the version that line names, if the hello offered it, and from then on with that reader alone; the owner reads with the reader of the version it chose. A line of the other version is `unknown_schema`, which ends the session as it does for a version nobody speaks (`transcript-version-fixed.json`). There is no hello in v2: a v2 reader given one refuses it as `invalid`, since it has already read the real one.
4. In Go, v2 lines are read with `ParseVoiceControlCommandV2` and `ParseVoiceControlEventV2`, which apply the framing rules of §3 exactly as the v1 readers do and are the specification of the Rust helper's and the Go supervisor's reading. `MarshalVoiceControlLine` writes both versions. `VoiceControlCommandKind.KnownIn(version)` says which kinds a version has: `set_devices` and `set_talk_mode` are `unknown_kind` to a v1 reader, as they were before v2 existed.

### 20.3 Messages

Commands (owner to helper), in `conch.voice_control.v2`:

| `kind` | Fields besides `schema`, `kind`, `id` | Does |
|---|---|---|
| `start` | as v1 | The first command. The talk mode is **not** carried: the helper reads it from `voice.toml` as it reads `[audio]` today, and the state reports what is in force, so the owner shows what is true rather than what was asked. |
| `set_mute`, `set_deafen`, `get_state`, `leave` | as v1 | As v1. |
| `set_devices` | `input`? (string), `output`? (string) | Use this microphone and these speakers from now on (§20.5). At least one of the two is present. Each is a PipeWire `node.name` under the device list's rule (#649: 1 to `VoiceDevicesMaxNameBytes` = 255 bytes of UTF-8, no control character, no `"` or `\`, so that it can be written into `voice.toml` between quotes as it stands), or `default` for PipeWire's default, followed as it changes. A side left out is left alone; a side present and empty is `invalid`, so that no reader can take an empty name for "leave it". |
| `set_talk_mode` | `talk_mode` (`push_to_talk` or `open`) | Choose what may open the gate (§20.6). The field is named as the state's field, as `set_mute`'s `muted` and `set_deafen`'s `deafened` are; the issue's draft said `mode`. |

Both new commands are **desired states**, like `set_mute`: sent twice, the second changes nothing, has no state before its ack, and is acknowledged `ok`. Neither writes `voice.toml`: the owner saves the choice itself with `conch-voice config set` (#651) and sends the command for the running session, so one program writes the file and the helper never holds a path. Refusals: a bad device name or a mode that does not exist is `invalid`; after leaving began, both are `stopping`, as `set_mute` is (`transcript-refused-commands.json`). The owner only ever writes valid commands, so §7's rule stands: any refusal is an owner bug and the supervisor stops the helper.

Events (helper to owner), in `conch.voice_control.v2`: `ack` and `protocol_error` as v1; `state` with two fields more, `playback` and `talk_mode` (§20.4). **The helper names no device in any event.** The contract forbids free text from the helper (§4), and the owner knows what it asked for; whether it is there is said by `capture` and `playback`.

### 20.4 State

```text
state: phase, reason?, retry_in_ms?, capture, playback, transmitting, muted, deafened, blocked, talk_mode, ptt, ptt_problem?
```

- `playback` is the speakers' stream, as `capture` is the microphone's: `on` while the stream is open with a device, `unavailable` when the speakers named are not there or went away, `off` when no stream is open. Speakers that are named and absent are `unavailable`, and never replaced by the default (`audio.rs` decides the same for the microphone at start-up: the stand-in's audio is discarded); playback resumes when they appear. Deafened is not `off`: the stream stays open and plays silence, as muted keeps `capture: on`. Transmitting does not need speakers, so `playback: unavailable` blocks nothing.
- `talk_mode` is the mode in force: `push_to_talk` or `open`. It is `push_to_talk` in `starting`, before the configuration is read (as `ptt` is `unknown` then), and that is also the mode that sends less. It becomes `open` only once `voice.toml` or `set_talk_mode` said so.
- `blocked` is every reason a press, or in open mode an onset, would do nothing now, in v1's order. In open mode `key_device_lost` is never among them: the talk key is ignored, so nothing held can be lost. `max_transmit` in open mode means one opening reached `max_transmit_secs` and stands until quiet (§20.6).
- `ptt` and `ptt_problem` are reported in open mode too: the key device is still read for the mute and deafen keys. Transmitting does not depend on them there.

A v2 state must hold together, and `VoiceControlStateV2.Validate` refuses one that does not. The rules are v1's (§6), changed in three places and with two added:

- `transmitting` needs `ptt: ready` only in `push_to_talk`. In open mode it needs `connected`, `capture: on` and nothing in `blocked`, whatever `ptt` says (`event-state-open-transmitting.json`, `event-state-open-key-device-lost.json`).
- `key_device_lost` is in `blocked` exactly when `ptt` is `lost` in `push_to_talk`, and never in open mode.
- `playback` is `off` in `starting`, `stopped` and `failed`; `on` or `unavailable` in `connecting`, `connected` and `reconnecting` (the speakers are opened in `starting`, before connecting, §8.1, and absent ones are `unavailable`, so past `starting` there is nothing for `off` to mean); anything in `stopping`, which closes them.
- `talk_mode` is `push_to_talk` in `starting`.
- Everything else as §6: `not_connected` in every phase but `connected`, `capture` and `no_microphone`, `muted` and `deafened` with their blocks, `ptt: unknown` only before connecting, reasons and `retry_in_ms` by phase.

### 20.5 Switching devices

- **What a name may point at.** `input` names a PipeWire node of class `Audio/Source` (a microphone, real or virtual) and `output` one of class `Audio/Sink`. A name of the other class, or of no node, is `unavailable`, exactly as an absent device is: the helper never targets a sink's monitor as the microphone. (dmn-audio's capture stream pointed at a sink name would capture that sink's monitor, and in open mode that would re-broadcast everything the machine plays, the room's other speakers included.) `default` is PipeWire's default of the right class.
- **The microphone, during a transmission:** the gate shuts at once, with no release tail, as it does for mute; the transmission ends and is reported `stopped`; the state that shows `transmitting: false` is written before the ack. A fresh press, or in open mode a fresh onset, is needed afterwards (UX-10: a shut gate reopens only on a new press). In open mode the onset detector starts sounding on the new device (§20.6), so the switch itself cannot open the gate: `open_hang_ms` of quiet and then an onset are needed, however loud the new microphone is (`transcript-open-mode-entered-loud.json`). Nothing captured from the old device after the command, and nothing from the new one before a new opening, is sent (`transcript-microphone-switch-while-transmitting.json`).
- **The speakers:** the gate is not touched; a transmission under way goes on. When nothing the state shows changes, the ack has no state before it.
- **Before the microphone is first opened** (listen-only, or not yet connected): the name is kept and used when it opens; the ack is `ok` with no state before it.
- **A missing device after a switch:** the ack is `ok`, the session now targets that device. The state shows `capture: unavailable` with `no_microphone` (or `playback: unavailable`) until the device appears, and then capture (or playback) resumes. **There is no fallback to another device** (UX-11; `audio.rs` decides the same at start-up). (`transcript-device-missing-after-switch.json`.)
- **The same name as the one in force** changes nothing: the gate is not touched and the ack has no state before it. `default` is PipeWire's default, followed as it changes, as today when `voice.toml` names nothing.
- Whether `capture` passes through `unavailable` while the stream is moved from one device to another is the helper's (#655), and a reader shows what it is told; the transcript shows the plain case, where it stays `on`. What is not the helper's to choose (#655): the class rule above (a source for `input`, a sink for `output`, never a sink's monitor as the microphone, nothing of another class or of no node used in a name's place), and in open mode the detector starting sounding on the new device.

### 20.6 The open microphone

**Level-triggered, not always on while unmuted** (decided on #653; open question 1 of #662). A program's virtual microphone is digital silence between utterances, so any level separates speech from silence cleanly. "Always on" would show the person talking the whole time in every roster, make the server record one endless transmission, send the room's noise between utterances, and end every session at `max_transmit_secs`. The browser's open mode (`web/src/voice/gate.ts`) is level-triggered for the same reasons, with a threshold and a 400 ms hang; this follows it.

The rule, restated for both modes (the header rule of `conch-voice-control/src/ptt.rs`, "the gate is opened by a press and by nothing else", and conch-voice.md §3): **in push-to-talk mode the gate is opened by a press; in open mode by the microphone's own sound crossing the level; no command, timer, reconnection or cleared condition opens it in either mode. `set_talk_mode` only chooses which of the two may open it, and mute and deafen shut it in both.**

How open mode works, in the helper:

- **The detector** runs on each captured 10 ms frame before the gate, only in open mode, and hands no frame onward: the only path to the track is still the gate. It has two states, *quiet* and *sounding*. It becomes sounding when one frame's RMS level is at or above `open_threshold_db` (dBFS: 20·log10 of the frame's RMS over its 480 samples in [−1, 1], so a full-scale sine is −3 dBFS). It becomes quiet after `open_hang_ms` with no frame at or above the level. **A stretch with no frames counts as quiet**: a virtual source that stops delivering must not hold the gate open, so the transmit task's pace timer feeds the detector the time that passed.
- **The detector starts sounding.** On entering open mode (from `voice.toml` at start, or by `set_talk_mode open`), after a microphone switch in open mode, and whenever a block clears, the detector is put in the sounding state, whatever the microphone holds. So the first opening always follows `open_hang_ms` of quiet and then an onset, and no command can be the thing that opened the gate: a `set_talk_mode open` sent while the microphone is loud (a program already speaking, a noisy room) transmits nothing until the sound stops and starts again (`transcript-open-mode-entered-loud.json`). Were the detector to start quiet, the first loud frame after the ack, 10 ms later, would be an onset, and the command would have opened the gate. #654's detector tests must cover this: a detector created in open mode fed loud frames from its first frame never reports an onset until `open_hang_ms` of frames below the level (or of no frames) has passed; the same after a mode change, a microphone switch and each block clearing; and the quiet that counts is quiet observed after that moment, not before it.
- **An onset** is the detector going from quiet to sounding. An onset while nothing blocks opens the gate; the gate then unmutes the track once `conchd` has answered the `started` report, or after at most 1 s, as for a press (conch-voice.md §6). **The onset frame, and what is captured before the gate has opened, is not sent:** nothing captured while the gate was shut is sent, which conch-voice.md §3 keeps. The cost is the first 10 ms and the report wait, as it is at the start of a press. An onset during the release tail of the previous opening waits for the gate to report shut and then opens it as a new transmission, as a press during the tail does (ptt.rs: "the gate is never told to open while it is still shutting"), so two openings' reports cannot run together.
- **Quiet shuts the gate** (the detector going to quiet, `open_hang_ms` after the last frame at or above the level); the release tail then applies, as after a release. The `stopped` report follows the gate shutting, as today.
- **Every v1 block applies**, and a block shuts the gate at once with no tail, as in v1: mute, deafen, not connected, no publish grant, no microphone, the limit. **Once a block clears, a fresh onset is needed**: the detector must go quiet and then sounding again after the clearing. This is the open-mode form of "a held key must be pressed again" (ptt.rs), so unmuting during speech does not send the rest of it, and reconnecting during speech does not resume it. (The browser's gate differs here: it reopens on the next loud frame. The native client fails closed.)
- **`max_transmit_secs` limits each opening** (default 120, at most 600). When it is reached the gate shuts with no tail, `max_transmit` stands until `open_hang_ms` of quiet, and the next onset opens the gate again (`transcript-open-mode.json`).
- **The talk key does nothing in open mode.** A press or a release is ignored, so a key held during a mode switch cannot hold the gate. The mute and deafen keys work as before. A lost key device is `ptt: lost` in the state and blocks nothing (§20.4).
- **A mode switch shuts the gate** at once, with no release tail, in both directions: the person changed what transmission means, and the browser's `setMode` does the same. What was transmitting is reported `stopped`, and a fresh press or a fresh onset is needed (`transcript-mode-switch-while-transmitting.json`).
- **Open mode is never automatic.** It is in force only when `voice.toml` says `talk_mode = "open"` or the owner sent `set_talk_mode open`. It is never a fallback when no key device is configured or the key device cannot be read (#537: no implicit toggle-to-talk or open-mic workaround): `ptt: not_configured` in push-to-talk mode still means nothing can transmit.
- **A v1 session always runs push-to-talk**, even when `voice.toml` says `open`: it sends less, never more. The helper writes one line to its log, which `/voice status` shows (§15). Standalone `conch-voice join` honours `talk_mode` (#654).
- **Transmit reports.** Each opening is one transmission, with `started` and `stopped` reports and the existing 1 s report wait (conch-voice.md §6); many short openings are many report pairs, as in the browser's open mode today. The server's bound is unchanged (`voiceReportBurst` 30, refilled at `voiceReportPerSecond` 4 a second). At the defaults an opening lasts at least one frame plus `open_hang_ms` plus `release_tail_ms`, 10 + 400 + 100 = 510 ms, two reports, so under 3.93 reports a second: below the refill, and the 429 cannot be reached however the sound comes. The arithmetic assumes the rule above: an onset during the tail waits for the gate to shut and begins a new opening after it, so the tail is part of every opening's length; an engine that let an onset cut the tail short would lose the 100 ms and the margin with it. With `open_hang_ms` at its minimum of 100 ms and the default tail, an opening can last 210 ms, 9.5 reports a second, and the burst of 30 lasts about 5.5 s of such chatter; with the tail set to 0 and the default hang, 4.88 a second drain the burst in about 34 s. A `stopped` answered 429 is handled as today: the status line says so, audio does not wait for it, and the poller records what it sees. The TUI's user notes should say that a short hang can hit the bound.
- **Echo.** Echo cancellation does not work yet (#186 open; #566: `echo_cancellation = true` is accepted and does nothing). Open mode on speakers sends the other people's voices back to them. Clients warn and do not refuse (#657 has the words). A virtual microphone fed by a program, not by the room, does not pick the speakers up.

**Configuration** (`voice.toml`, `[audio]`, read by the helper as `[audio]` is read today, §9; conch-voice.md §8): `talk_mode` (`"push_to_talk"` or `"open"`, default `push_to_talk`); `open_threshold_db` (integer dBFS, default −40, from −80 to −10); `open_hang_ms` (default 400, from 100 to 2000). A value outside its range is a refused configuration (`bad_config`), as an unknown key is. The threshold and hang are hand-edited; `conch-voice config set` (#651) takes `audio.talk_mode` so that the TUI can save the mode (#657). In managed mode the helper reads these keys and never writes them.

### 20.7 Security and parity

- **The transmit rule changed and was restated** (§17, conch-voice.md §3): nothing on the channel opens the gate in either mode. A security-reviewer pass on this section and the types is required (#653), on the rule change and on the two new owner-to-helper text fields.
- **`input` and `output` are owner-to-helper text**, like `server` and `channel`: bounded (255 bytes), one line, no control character, never echoed by the helper in any event, and never logged by it unscrubbed. They name a PipeWire node and reach nothing but PipeWire's node lookup; a name that matches nothing is `unavailable`, not an error and not a fallback. The field lists are pinned by `TestVoiceControlV2FieldsAreAnExactList`, and `TestVoiceControlV2CarriesNoSecrets` holds every event string to a vocabulary.
- **No secret and no path**: v2 adds no field that could hold a token, a room or a path; the helper never learns where `voice.toml` is written from the owner, because it never writes it.
- **Sound from a virtual microphone is the person's own transmission**, reported and audited under their login, as any press is (decided on #662). It is a device choice, not agents in voice (P8, `publish:audio`), and needs no capability or server change.
- **API parity (ADR-000 D6):** devices and the talk mode are local media state, like mute and deafen (§17). The server sees the same session, presence and transmit reports as before; nothing is added to the REST/WS API or to MCP.

### 20.8 Fixtures

All in `pkg/schema/testdata/voice-control-v2/`, read in place by `pkg/schema/voice_control_v2_test.go` and, from #656, by the Rust helper's tests. Immutable once merged, like v1's (§16).

Golden messages, each byte-identical to its Go type's indented encoding:

- Commands: `command-start.json`, `command-set-mute.json`, `command-set-deafen.json`, `command-get-state.json`, `command-leave.json`, `command-set-devices.json` (both sides), `command-set-devices-input.json` (the microphone only), `command-set-devices-default.json` (the speakers back to `default`), `command-set-talk-mode.json`.
- Events: `event-hello-v1-and-v2.json` (a v1 hello offering both; read with the v1 reader), `event-ack.json`, `event-ack-refused.json`, `event-protocol-error.json`.
- States: `event-state-starting.json`, `event-state-connected.json`, `event-state-transmitting.json`, `event-state-no-speakers.json`, `event-state-no-microphone.json`, `event-state-open-listening.json`, `event-state-open-transmitting.json`, `event-state-open-muted.json`, `event-state-open-key-device-lost.json`, `event-state-open-max-transmit.json`, `event-state-stopped.json`.

Conformance, for both v2 readers: `cases.json`, in the form of v1's, covering the two new commands and their refusals (neither side, an empty name, a null, a name too long or with a quotation mark, a backslash, an escape sequence or a line feed, a mode that does not exist, the field under another name), that no command presses and none opens the gate, the state's two new fields and every changed consistency rule, a v1 line read as v2 and a v2 line as v1, a hello in v2, and one case for each framing code. The long lines are `lines/command-at-limit.txt`, `lines/command-too-long.txt`, `lines/event-at-limit.txt` and `lines/event-too-long.txt`.

Transcripts, checked by `TestVoiceControlV2Transcripts` against §5's rules and v2's own (the hello offers both, the start fixes the version, a microphone or mode switch during a transmission ends it before the ack, the mode set is in the state before the ack): `transcript-negotiation-v2-owner.json`, `transcript-negotiation-v1-owner.json`, `transcript-microphone-switch-while-transmitting.json`, `transcript-device-missing-after-switch.json`, `transcript-open-mode.json` (an onset, quiet, mute while open and the fresh onset after, the limit reached then quiet), `transcript-mode-switch-while-transmitting.json` (both directions, with the key device lost while open), `transcript-open-mode-entered-loud.json` (`set_talk_mode open` and then a microphone switch while the microphone is loud: nothing transmits until quiet and then an onset), `transcript-refused-commands.json`, `transcript-version-fixed.json`.

Secrets: `sensitive.json`, as v1's.

### 20.9 Decisions

Decided here, where the issue left a choice or asked for one to be recorded (continuing §18's numbering):

11. **One version for both features** (§20.1).
12. **The talk mode is not carried in `start`**; the helper reads it from `voice.toml` and the state reports it, so the owner shows what is true (§20.3).
13. **`set_devices` and `set_talk_mode` are desired states that write nothing**; the owner saves with `config set` (§20.3).
14. **The helper names no device in any event** (§20.3).
15. **A side of `set_devices` left out is left alone; present and empty is `invalid`** (§20.3).
16. **The command's field is `talk_mode`, as the state's**, not `mode` (§20.3).
17. **`playback` is never `off` past `starting`**, and anything in `stopping` (§20.4).
18. **`talk_mode` is `push_to_talk` in `starting`** (§20.4).
19. **Level-triggered**, with a two-state detector, an onset as the transition to sounding, and no frames counting as quiet (§20.6).
20. **The onset frame is not sent** (§20.6).
21. **A fresh onset after a block clears, and the detector starts sounding** on entering open mode, after a microphone switch in open mode and whenever a block clears, so that the first opening always follows `open_hang_ms` of quiet and no command, switch or cleared condition is what opened the gate. Stricter than the browser's gate (§20.6).
22. **A mode switch shuts the gate with no tail, in both directions** (§20.6).
23. **`max_transmit` in open mode stands until quiet** (§20.6).
24. **A v1 session is push-to-talk whatever `voice.toml` says**, with one log line (§20.6).
25. **The version is fixed by the start; a hello in v2 does not exist** and is `invalid` to a v2 reader (§20.2).
26. **`VoiceControlCommandKind` is one vocabulary across versions**, with `KnownIn(version)` saying which kinds a version has, so that the v1 reader still answers `set_devices` with `unknown_kind` (§20.2).

Needs the project owner's sign-off, recorded on #653 before the first commit: (a) the version `conch.voice_control.v2`; (b) the restated transmit rule in conch-voice.md §3 and §17 above; (c) level-triggered rather than always-on. (c) is recorded there already; (a) and (b) were part of the plan on #662 that the owner approved on 2026-10-19 and are his to confirm on this PR.

