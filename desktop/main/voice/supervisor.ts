// The Electron owner's supervisor for conch-voice on Linux (issue #948, D2-9
// of #770; linux-desktop.md §5.3, L-F3). Model B: the helper is the person's
// one voice participant, and main owns it.
//
// Input: a start request from main (the profile, its configuration
// directory, the token, the server, the channel, the principal), and
// set_mute, set_deafen, get_state, set_devices, set_talk_mode and stop.
// Output: one typed state for the UI (VoiceState), every change sent to
// subscribers. #771 builds Join/Leave, mute and devices on this; #945's
// profile switch calls stop() before identity changes.
// Owns: one helper process per instance: its endpoint, spawn, attach,
// hello, version negotiation, start, the commands and their acks, the stop
// escalation and the reap. It never restarts a helper by itself.
//
// The rules it keeps (linux-desktop.md §5, tui-voice.md §5–§8):
//   - authority only on standard input; nothing is ever written on the events
//     connection;
//   - the hello within VoiceControlHelloTimeoutMS of the spawn; the highest
//     version both sides implement, or nothing written at all;
//   - every command acknowledged within VoiceControlAckTimeoutMS, at most
//     VoiceControlMaxInFlight unanswered;
//   - stop is leave, close standard input and the events endpoint, SIGTERM at
//     VoiceControlStopTermAfterMS and SIGKILL at VoiceControlStopKillAfterMS
//     from the start of the stop, through the ChildProcess object only, then
//     wait for its exit; no new helper is spawned until the old one is
//     reaped;
//   - helper loss (the process exits, the events connection ends, or a line
//     breaks the protocol) is one typed state, and only that: the rest of the
//     app goes on;
//   - neither the token nor the nonce is ever logged, in argv, or in a state.
import { randomBytes } from 'node:crypto';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import type { Socket } from 'node:net';

import {
  VOICE_CONTROL_ACK_TIMEOUT_MS,
  VOICE_CONTROL_HELLO_TIMEOUT_MS,
  VOICE_CONTROL_MAX_IN_FLIGHT,
  VOICE_CONTROL_PIPE_NONCE_BYTES,
  VOICE_CONTROL_STOP_KILL_AFTER_MS,
  VOICE_CONTROL_STOP_TERM_AFTER_MS,
  type VoiceControlAckV1,
  type VoiceControlAckV2,
  type VoiceControlBlock,
  type VoiceControlCapture,
  type VoiceControlErrorCode,
  type VoiceControlHelloV1,
  type VoiceControlPhase,
  type VoiceControlPlayback,
  type VoiceControlPTT,
  type VoiceControlPTTProblem,
  type VoiceControlReason,
  type VoiceControlStateV1,
  type VoiceControlStateV2,
  type VoiceControlTalkMode,
} from '../../../web/src/schema/generated.ts';
import { EndpointError, listen, profileDir, runtimeDir, sweep, type AttachFailure, type EndpointDeps, type EndpointRefusal, type Listening } from './endpoint.ts';
import { SCHEMA_V2, LineSplitter, Tracker, command, encodeCommand, finalPhase, negotiate, startCommand, type Command, type Event, type Version, type Want } from './protocol.ts';
import { defaultSpawn, findHelper, HelperError, helperArgs, helperEnv, spawnHelper, type FindDeps, type HelperRefusal, type SpawnFn } from './spawn.ts';

// What the helper says it is doing, as the UI may show it.
export interface HelperView {
  readonly version: 1 | 2;
  readonly helperVersion: string;
  readonly phase: VoiceControlPhase;
  readonly reason?: VoiceControlReason;
  readonly capture: VoiceControlCapture;
  readonly playback?: VoiceControlPlayback;
  readonly transmitting: boolean;
  readonly muted: boolean;
  readonly deafened: boolean;
  readonly blocked: readonly VoiceControlBlock[];
  readonly talkMode?: VoiceControlTalkMode;
  readonly ptt: VoiceControlPTT;
  readonly pttProblem?: VoiceControlPTTProblem;
}

// Why a start did not get as far as a running session.
export type StartFailure = AttachFailure | 'hello-timeout' | 'version-mismatch' | 'invalid-start' | 'start-refused' | 'spawn-failed' | 'busy';

// How a running helper was lost: the process exited, the events connection
// ended, a line broke the protocol, a command went unanswered, or standard
// input could not be written.
export type LossReason = 'exited' | 'events-ended' | 'malformed' | 'ack-timeout' | 'write-failed';

export type VoiceState =
  | { readonly status: 'idle' }
  // Voice cannot start on this machine or in this app: the helper is not
  // installed (text stays usable), not trusted, or the runtime directory or
  // socket refused. Nothing was spawned.
  | { readonly status: 'unavailable'; readonly reason: HelperRefusal | EndpointRefusal }
  | { readonly status: 'starting'; readonly generation: number }
  | { readonly status: 'running'; readonly generation: number; readonly helper: HelperView }
  | { readonly status: 'stopping'; readonly generation: number }
  // The helper reported its final state, as asked or for its own reason.
  | { readonly status: 'ended'; readonly generation: number; readonly helper: HelperView }
  | { readonly status: 'failed'; readonly generation: number; readonly reason: StartFailure }
  | { readonly status: 'lost'; readonly generation: number; readonly reason: LossReason };

export interface StartRequest {
  // The profile's random local id: names conch/<profile-id> in the runtime
  // directory.
  readonly profileId: string;
  // The profile's own configuration directory (CONCH_CONFIG_DIR).
  readonly configDir: string;
  readonly token: string;
  readonly server: string;
  readonly channel: string;
  readonly principalId: number;
  readonly muted?: boolean;
  readonly deafened?: boolean;
  // Further helper options, such as #771's missed-release default. Never a
  // secret; helperArgs refuses one holding the token or the nonce.
  readonly extraArgs?: readonly string[];
}

export interface Timings {
  readonly helloTimeoutMs: number;
  readonly ackTimeoutMs: number;
  readonly termAfterMs: number;
  readonly killAfterMs: number;
}

export const CONTRACT_TIMINGS: Timings = {
  helloTimeoutMs: VOICE_CONTROL_HELLO_TIMEOUT_MS,
  ackTimeoutMs: VOICE_CONTROL_ACK_TIMEOUT_MS,
  termAfterMs: VOICE_CONTROL_STOP_TERM_AFTER_MS,
  killAfterMs: VOICE_CONTROL_STOP_KILL_AFTER_MS,
};

export interface SupervisorDeps {
  // The app's resources directory, where the bundled conch-voice is.
  readonly resources: string;
  // main's environment: XDG_RUNTIME_DIR, and the allowlisted names.
  readonly env: NodeJS.ProcessEnv;
  readonly log: (line: string) => void;
  readonly spawn?: SpawnFn;
  // Test seams.
  readonly nonce?: () => string;
  readonly endpoint?: EndpointDeps;
  readonly find?: FindDeps;
  readonly timings?: Partial<Timings>;
}

export type CommandResult = { readonly ok: true } | { readonly ok: false; readonly error: VoiceControlErrorCode | 'not-running' | 'busy' | 'unsupported' | 'lost' };

interface Waiter {
  readonly resolve: (r: CommandResult) => void;
  readonly timer: NodeJS.Timeout;
}

// The lines of the helper's standard error kept for diagnostics, and their
// length; the token and the nonce are replaced before a line is kept.
const STDERR_LINES = 32;
const STDERR_LINE_BYTES = 512;

// How long the end of the events connection waits for the process's exit
// before it is the loss itself.
const EXIT_GRACE_MS = 100;

class Run {
  readonly tracker = new Tracker();
  readonly splitter = new LineSplitter();
  readonly waiters = new Map<number, Waiter>();
  readonly stderr: StderrTail;
  child: ChildProcessWithoutNullStreams | null = null;
  listening: Listening | null = null;
  socket: Socket | null = null;
  hello: VoiceControlHelloV1 | null = null;
  view: HelperView | null = null;
  version: Version | null = null;
  nextId = 1;
  started = false;
  // ready: the start was acknowledged; from then on states are shown.
  ready = false;
  stopping = false;
  // ownerStopped: stop() was called; it decides the state from then on.
  ownerStopped = false;
  exited = false;
  resyncing = false;
  // Set once: what became of this run, for the state after the reap.
  outcome: VoiceState | null = null;
  stopped: Promise<void> | null = null;
  readonly reaped: Promise<void>;
  markReaped: () => void = () => undefined;
  helloSeen: (hello: VoiceControlHelloV1) => void = () => undefined;
  broken: (reason: LossReason) => void = () => undefined;

  constructor(
    readonly generation: number,
    readonly secrets: readonly string[],
  ) {
    this.stderr = new StderrTail(secrets);
    this.reaped = new Promise((resolve) => {
      this.markReaped = resolve;
    });
  }
}

function viewOf(state: VoiceControlStateV1 | VoiceControlStateV2, helperVersion: string): HelperView {
  const v2 = state.schema === SCHEMA_V2 ? state : null;
  return {
    version: v2 === null ? 1 : 2,
    helperVersion,
    phase: state.phase,
    ...(state.reason === undefined ? {} : { reason: state.reason }),
    capture: state.capture,
    ...(v2 === null ? {} : { playback: v2.playback, talkMode: v2.talk_mode }),
    transmitting: state.transmitting,
    muted: state.muted,
    deafened: state.deafened,
    blocked: [...state.blocked],
    ptt: state.ptt,
    ...(state.ptt_problem === undefined ? {} : { pttProblem: state.ptt_problem }),
  };
}

// printable keeps a stderr line showable: printable ASCII, bounded, with
// every secret replaced.
function printable(line: string, secrets: readonly string[]): string {
  let out = line;
  for (const s of secrets) if (s !== '') out = out.split(s).join('<redacted>');
  out = out.replace(/[^\x20-\x7e]/g, '?');
  return out.length > STDERR_LINE_BYTES ? `${out.slice(0, STDERR_LINE_BYTES)}…` : out;
}

// The longest partial line held while waiting for its line feed.
const STDERR_PARTIAL_MAX = STDERR_LINE_BYTES * 4;
const STDERR_TOO_LONG = '<line too long>';

// StderrTail keeps the last lines of the helper's standard error, redacted,
// printable and bounded. A line is redacted whole or not kept at all: one
// that passes STDERR_PARTIAL_MAX before its line feed is dropped, with a
// fixed marker in its place, and the rest of it is skipped up to its line
// feed (as LineSplitter does). A fragment cut from a line is never kept,
// since a secret straddling the cut would survive redaction in part.
export class StderrTail {
  private partial = '';
  private skipping = false;
  readonly lines: string[] = [];

  constructor(private readonly secrets: readonly string[]) {}

  private keep(line: string): void {
    this.lines.push(line);
    if (this.lines.length > STDERR_LINES) this.lines.shift();
  }

  push(chunk: string): void {
    let rest = chunk;
    while (rest !== '') {
      const lf = rest.indexOf('\n');
      const piece = lf < 0 ? rest : rest.slice(0, lf);
      rest = lf < 0 ? '' : rest.slice(lf + 1);
      if (this.skipping) {
        if (lf >= 0) this.skipping = false;
        continue;
      }
      if (this.partial.length + piece.length > STDERR_PARTIAL_MAX) {
        this.partial = '';
        this.skipping = lf < 0;
        this.keep(STDERR_TOO_LONG);
        continue;
      }
      if (lf < 0) {
        this.partial += piece;
      } else {
        this.keep(printable(this.partial + piece, this.secrets));
        this.partial = '';
      }
    }
  }
}

export class Supervisor {
  private current: Run | null = null;
  private generation = 0;
  private stateValue: VoiceState = { status: 'idle' };
  private readonly listeners = new Set<(s: VoiceState) => void>();
  private readonly timings: Timings;
  private starting = false;

  constructor(private readonly deps: SupervisorDeps) {
    this.timings = { ...CONTRACT_TIMINGS, ...deps.timings };
  }

  get state(): VoiceState {
    return this.stateValue;
  }

  subscribe(listener: (s: VoiceState) => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  // diagnostics is the helper's recent standard error, redacted.
  diagnostics(): readonly string[] {
    return this.current === null ? [] : [...this.current.stderr.lines];
  }

  private set(next: VoiceState): void {
    this.stateValue = next;
    for (const l of this.listeners) {
      try {
        l(next);
      } catch {
        // A listener's failure is its own; the supervisor goes on.
      }
    }
  }

  private log(line: string): void {
    this.deps.log(line);
  }

  // start starts one helper and joins the channel's voice. It resolves with
  // the state it reached: running, or why not. A start while a helper is
  // stopping waits for that helper to be reaped first; a start while one is
  // running or starting is refused (one helper per instance).
  async start(req: StartRequest): Promise<VoiceState> {
    if (this.starting) return { status: 'failed', generation: this.generation, reason: 'busy' };
    const previous = this.current;
    if (previous !== null && !previous.exited) {
      if (!previous.stopping) return { status: 'failed', generation: previous.generation, reason: 'busy' };
    }
    this.starting = true;
    try {
      if (previous !== null) await previous.reaped;
      return await this.launch(req);
    } finally {
      this.starting = false;
    }
  }

  private async launch(req: StartRequest): Promise<VoiceState> {
    const generation = ++this.generation;
    const params = { server: req.server, channel: req.channel, principalId: req.principalId, muted: req.muted ?? false, deafened: req.deafened ?? false };
    // A start the helper would have to refuse is refused here, before
    // anything is spawned.
    try {
      encodeCommand(startCommand(SCHEMA_V2, 1, params));
    } catch {
      this.log('voice: the start is not one the helper could accept');
      return { status: 'failed', generation, reason: 'invalid-start' };
    }
    let executable: string;
    let dir: string;
    try {
      executable = await findHelper(this.deps.resources, this.deps.find);
      const runtime = await runtimeDir(this.deps.env, this.deps.endpoint);
      dir = await profileDir(runtime, req.profileId, this.deps.endpoint);
      await sweep(dir, (l) => {
        this.log(l);
      });
    } catch (err) {
      return this.unavailable(err);
    }
    let listening: Listening;
    try {
      listening = await listen(dir, { ...this.deps.endpoint, log: (l) => { this.log(l); } });
    } catch (err) {
      return this.unavailable(err);
    }
    const nonce = this.deps.nonce?.() ?? randomBytes(VOICE_CONTROL_PIPE_NONCE_BYTES).toString('base64url');
    const run = new Run(generation, [req.token, nonce]);
    run.listening = listening;
    this.current = run;
    this.set({ status: 'starting', generation });

    let child: ChildProcessWithoutNullStreams;
    try {
      const env = helperEnv({ parent: this.deps.env, configDir: req.configDir, token: req.token, nonce });
      const args = helperArgs(listening.path, req.extraArgs ?? [], run.secrets);
      child = spawnHelper(this.deps.spawn ?? defaultSpawn, executable, args, env);
    } catch {
      listening.close();
      run.exited = true;
      run.markReaped();
      return this.finish(run, { status: 'failed', generation, reason: 'spawn-failed' });
    }
    const spawnedAt = Date.now();
    run.child = child;
    this.watchChild(run, child);
    this.log(`voice: helper ${String(generation)} spawned`);

    // Attach: the one connection, its first line within the hello's
    // deadline, the nonce compared in constant time.
    const deadline = spawnedAt + this.timings.helloTimeoutMs;
    let socket: Socket;
    let rest: Buffer;
    try {
      ({ socket, rest } = await listening.attach(nonce, deadline));
    } catch (err) {
      const reason: AttachFailure = err instanceof EndpointError && err.reason.startsWith('attach-') ? (err.reason as AttachFailure) : 'attach-ended';
      if (run.outcome === null) run.outcome = { status: 'failed', generation, reason };
      void this.stopRun(run, false);
      return this.settle(run);
    }
    run.socket = socket;
    this.log(`voice: helper ${String(generation)} attached`);

    const hello = new Promise<VoiceControlHelloV1 | LossReason | 'timeout'>((resolve) => {
      const timer = setTimeout(() => {
        resolve('timeout');
      }, Math.max(0, deadline - Date.now()));
      run.helloSeen = (h) => {
        clearTimeout(timer);
        resolve(h);
      };
      run.broken = (reason) => {
        clearTimeout(timer);
        resolve(reason);
      };
    });
    this.readEvents(run, socket, rest);
    const got = await hello;
    run.helloSeen = () => undefined;
    run.broken = () => undefined;
    if (got === 'timeout') {
      if (run.outcome === null) run.outcome = { status: 'failed', generation, reason: 'hello-timeout' };
      void this.stopRun(run, false);
      return this.settle(run);
    }
    if (typeof got === 'string') return run.ownerStopped ? (run.outcome ?? this.stateValue) : this.settle(run);
    run.hello = got;
    const version = negotiate(got);
    if (version === null) {
      // Nothing is written: the helper, never started, exits on the end of
      // its standard input having opened nothing.
      this.log(`voice: helper ${String(generation)} offers no version this app speaks`);
      if (run.outcome === null) run.outcome = { status: 'failed', generation, reason: 'version-mismatch' };
      void this.stopRun(run, false);
      return this.settle(run);
    }
    run.version = version;
    run.tracker.version = version;
    this.log(`voice: helper ${String(generation)} speaks ${version}`);

    if (run.stopping) return run.outcome ?? this.stateValue;
    const start = startCommand(version, run.nextId++, params);
    run.started = true;
    const acked = await this.exchange(run, start);
    if (!acked.ok) {
      // Stopped meanwhile: the stop says what became of it.
      if (run.ownerStopped) return run.outcome ?? this.stateValue;
      if (run.outcome === null) run.outcome = { status: 'failed', generation, reason: 'start-refused' };
      void this.stopRun(run, true);
      return this.settle(run);
    }
    if (run.outcome !== null) return run.outcome;
    run.ready = true;
    this.publish(run);
    return this.stateValue;
  }

  private unavailable(err: unknown): VoiceState {
    if (err instanceof HelperError || err instanceof EndpointError) {
      const reason = err.reason as HelperRefusal | EndpointRefusal;
      this.log(`voice: unavailable: ${reason}`);
      const next: VoiceState = { status: 'unavailable', reason };
      this.set(next);
      return next;
    }
    this.log('voice: unavailable: the runtime directory could not be prepared');
    const next: VoiceState = { status: 'unavailable', reason: 'profile-dir-unsafe' };
    this.set(next);
    return next;
  }

  // settle shows the run's outcome now, without waiting for the reap.
  private settle(run: Run): VoiceState {
    const outcome = run.outcome ?? { status: 'lost', generation: run.generation, reason: 'exited' };
    return this.finish(run, outcome);
  }

  private finish(run: Run, outcome: VoiceState): VoiceState {
    run.outcome ??= outcome;
    if (this.current === run && !run.ownerStopped) this.set(run.outcome);
    return run.outcome;
  }

  // publish shows the running helper's latest state.
  private publish(run: Run): void {
    if (this.current !== run || run.outcome !== null || run.stopping || !run.ready) return;
    const view = run.view ?? null;
    if (view === null) return;
    if (finalPhase(view.phase)) {
      run.outcome = { status: 'ended', generation: run.generation, helper: view };
      this.set(run.outcome);
      return;
    }
    this.set({ status: 'running', generation: run.generation, helper: view });
  }

  private watchChild(run: Run, child: ChildProcessWithoutNullStreams): void {
    // Standard output is libwebrtc's, never the protocol: drained and
    // dropped. Standard error is kept, redacted and bounded.
    child.stdout.resume();
    child.stderr.setEncoding('utf8');
    child.stderr.on('data', (chunk: string) => {
      run.stderr.push(chunk);
    });
    child.stdin.on('error', () => {
      if (!run.stopping) this.lose(run, 'write-failed');
    });
    child.on('error', () => {
      // A spawn that failed (the file went between the check and the exec)
      // has no pid and no exit event. Any other error (a kill() that failed,
      // say) leaves a live process, which only its exit event reaps.
      if (child.pid !== undefined) return;
      if (run.outcome === null) run.outcome = { status: 'failed', generation: run.generation, reason: 'spawn-failed' };
      if (!run.exited) this.exitedRun(run, null, null);
    });
    child.once('exit', (code, signal) => {
      this.exitedRun(run, code, signal);
    });
  }

  private exitedRun(run: Run, code: number | null, signal: NodeJS.Signals | null): void {
    if (run.exited) return;
    run.exited = true;
    this.log(`voice: helper ${String(run.generation)} exited (${signal === null ? `code ${String(code)}` : signal})`);
    run.listening?.close();
    run.socket?.destroy();
    for (const [, w] of run.waiters) {
      clearTimeout(w.timer);
      w.resolve({ ok: false, error: 'lost' });
    }
    run.waiters.clear();
    if (!run.stopping && run.outcome === null) {
      run.outcome = { status: 'lost', generation: run.generation, reason: 'exited' };
      if (this.current === run) this.set(run.outcome);
    }
    // Whatever was waiting for the hello waits no longer.
    run.broken('exited');
    run.markReaped();
  }

  private readEvents(run: Run, socket: Socket, rest: Buffer): void {
    const onLine = (line: Buffer | 'too_long'): void => {
      if (run.outcome !== null && run.outcome.status !== 'ended') return;
      const r = run.tracker.read(line);
      if (!r.ok) {
        this.log(`voice: helper ${String(run.generation)} wrote a bad event: ${r.detail}`);
        this.lose(run, 'malformed');
        return;
      }
      this.onEvent(run, r.event, r.gap, r.acked);
    };
    socket.on('data', (chunk: Buffer) => {
      run.splitter.push(chunk, onLine);
    });
    // A helper that exits closes its end of the connection too, and the two
    // events race: the end of the connection is the loss only if the process
    // is still there a moment later.
    let endSeen = false;
    const ended = (): void => {
      if (endSeen) return;
      endSeen = true;
      setTimeout(() => {
        if (!run.stopping && !run.exited) this.lose(run, 'events-ended');
      }, EXIT_GRACE_MS);
    };
    socket.once('end', ended);
    socket.once('close', ended);
    socket.on('error', ended);
    if (rest.length > 0) run.splitter.push(rest, onLine);
    socket.resume();
  }

  private onEvent(run: Run, ev: Event, gap: boolean, acked: Command['kind'] | undefined): void {
    switch (ev.kind) {
      case 'hello':
        run.helloSeen(ev.hello);
        return;
      case 'unknown':
        return;
      case 'protocol_error':
        this.log(`voice: helper ${String(run.generation)} reported ${ev.error.code}`);
        return;
      case 'state':
        run.view = viewOf(ev.state, run.hello?.helper_version ?? '');
        if (run.outcome === null) this.publish(run);
        break;
      case 'ack':
        this.answered(run, ev.ack, acked);
        break;
    }
    if (gap && !run.resyncing && run.outcome === null && !run.stopping) {
      run.resyncing = true;
      void this.send({ kind: 'get_state' }, run).finally(() => {
        run.resyncing = false;
      });
    }
  }

  private answered(run: Run, ack: VoiceControlAckV1 | VoiceControlAckV2, kind: Command['kind'] | undefined): void {
    const w = run.waiters.get(ack.id);
    if (w === undefined) return;
    run.waiters.delete(ack.id);
    clearTimeout(w.timer);
    if (!ack.ok) this.log(`voice: helper ${String(run.generation)} refused ${kind ?? 'a command'}: ${ack.error ?? 'refused'}`);
    w.resolve(ack.ok ? { ok: true } : { ok: false, error: ack.error ?? 'invalid' });
  }

  // exchange writes one command and waits for its ack.
  private exchange(run: Run, c: Command): Promise<CommandResult> {
    if (run.exited || run.stopping || run.child === null) return Promise.resolve({ ok: false, error: 'not-running' });
    if (run.tracker.unanswered >= VOICE_CONTROL_MAX_IN_FLIGHT) return Promise.resolve({ ok: false, error: 'busy' });
    let line: Buffer;
    try {
      line = encodeCommand(c);
    } catch {
      return Promise.resolve({ ok: false, error: 'invalid' });
    }
    return new Promise<CommandResult>((resolve) => {
      const timer = setTimeout(() => {
        run.waiters.delete(c.id);
        resolve({ ok: false, error: 'lost' });
        this.log(`voice: helper ${String(run.generation)} did not answer ${c.kind} in time`);
        this.lose(run, 'ack-timeout');
      }, this.timings.ackTimeoutMs);
      run.waiters.set(c.id, { resolve, timer });
      run.tracker.written(c);
      run.child?.stdin.write(line);
    });
  }

  // send writes a command of the running session.
  private send(want: Want, run: Run | null = this.current): Promise<CommandResult> {
    if (run === null || run.version === null || run.outcome !== null || run.stopping || run.exited) {
      return Promise.resolve({ ok: false, error: 'not-running' });
    }
    const c = command(run.version, run.nextId, want);
    if (c === null) return Promise.resolve({ ok: false, error: 'unsupported' });
    run.nextId++;
    return this.exchange(run, c);
  }

  setMute(muted: boolean): Promise<CommandResult> {
    return this.send({ kind: 'set_mute', muted });
  }

  setDeafen(deafened: boolean): Promise<CommandResult> {
    return this.send({ kind: 'set_deafen', deafened });
  }

  refresh(): Promise<CommandResult> {
    return this.send({ kind: 'get_state' });
  }

  // setDevices and setTalkMode are v2's; in a v1 session they are
  // unsupported.
  setDevices(devices: { readonly input?: string; readonly output?: string }): Promise<CommandResult> {
    return this.send({ kind: 'set_devices', ...devices });
  }

  setTalkMode(talkMode: VoiceControlTalkMode): Promise<CommandResult> {
    return this.send({ kind: 'set_talk_mode', talkMode });
  }

  // lose makes a running helper's loss its one state, and stops it.
  private lose(run: Run, reason: LossReason): void {
    if (run.outcome === null && !run.ownerStopped) {
      run.outcome = { status: 'lost', generation: run.generation, reason };
      this.log(`voice: helper ${String(run.generation)} lost: ${reason}`);
      if (this.current === run) this.set(run.outcome);
    }
    run.broken(reason);
    void this.stopRun(run, false);
  }

  // stop ends the current helper, if any, and resolves once it is reaped:
  // within VoiceControlStopKillAfterMS of the call and the reap. It is what
  // a profile switch or sign-out calls before identity changes (#945).
  async stop(): Promise<void> {
    const run = this.current;
    if (run === null) return;
    if (!run.stopping && !run.exited) this.set({ status: 'stopping', generation: run.generation });
    run.ownerStopped = true;
    await this.stopRun(run, true);
    if (this.current === run) this.set({ status: 'idle' });
  }

  private stopRun(run: Run, leave: boolean): Promise<void> {
    if (run.stopped !== null) return run.stopped;
    run.stopping = true;
    run.stopped = (async () => {
      const child = run.child;
      if (child === null || run.exited) {
        run.listening?.close();
        run.socket?.destroy();
        await run.reaped;
        return;
      }
      // leave, unless nothing was started or the session is already over.
      if (leave && run.started && run.version !== null && !finalPhase(run.view?.phase ?? 'starting') && child.stdin.writable) {
        const c = command(run.version, run.nextId++, { kind: 'leave' });
        if (c !== null) {
          try {
            run.tracker.written(c);
            child.stdin.write(encodeCommand(c));
          } catch {
            // The stop goes on whatever the write did.
          }
        }
      }
      child.stdin.end();
      run.listening?.close();
      run.socket?.destroy();
      const term = setTimeout(() => {
        if (!run.exited) {
          this.log(`voice: helper ${String(run.generation)} still running; SIGTERM`);
          child.kill('SIGTERM');
        }
      }, this.timings.termAfterMs);
      const kill = setTimeout(() => {
        if (!run.exited) {
          this.log(`voice: helper ${String(run.generation)} still running; SIGKILL`);
          child.kill('SIGKILL');
        }
      }, this.timings.killAfterMs);
      await run.reaped;
      clearTimeout(term);
      clearTimeout(kill);
      this.log(`voice: helper ${String(run.generation)} reaped`);
    })();
    return run.stopped;
  }
}
