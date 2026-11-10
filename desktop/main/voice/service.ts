// Main's one owner of voice (issue #1373, D3-2 of #771; linux-desktop.md §0
// D1-2, §4, §4.1, §5, §9; tui-voice.md §11, §15). Input: the person's intents
// (join, leave, retry, mute, deafen, devices, talk mode) and the identity of
// the activation in force. Output: one NativeVoiceView, every change sent to
// subscribers, drawn from the #948 supervisor's state and main's own memory
// of the destination, and nothing the renderer says.
// Owns: the destination channel (set only by join, UX-02), the mute and
// deafen the person last set (for retry), and the one call into
// Supervisor.start. It starts nothing by itself: only join and retry start a
// helper (UX-09), and nothing retries on its own.
//
// What it keeps:
//   - the bearer is read from the activation at join time only, handed to
//     Supervisor.start and held nowhere here; the renderer never sees it;
//   - the 30 s missed-release ceiling (D3-1) is passed on every start, and so
//     is --share-audio-owner companion (#1581): the renderer's screen
//     companion plays watched shares' sound, never the helper;
//   - a join that a sign-out, switch or 401 overtakes is stopped and
//     reported nowhere;
//   - teardown leaves and stops the helper within VoiceControlStopKillAfterMS;
//   - the logind watcher of #773 calls setMute and leave (decision D-1).
import { VOICE_CONTROL_STOP_KILL_AFTER_MS, type VoiceControlTalkMode } from '../../../web/src/schema/generated.ts';
import type {
  NativeVoiceDevicesResult,
  NativeVoiceHelper,
  NativeVoiceStatus,
  NativeVoiceView,
  VoiceCommandResult,
  VoiceDeviceKey,
  VoiceKeyDevicesResult,
  VoiceSaveResult,
  VoiceTalkBindingResult,
} from '../../../web/src/platform/desktop/bridge.ts';
import { devicePath, KEY_DEVICE_DISCLOSURE, listKeyDevices, type KeyDeviceDeps } from './keydevices.ts';
import { isPageSaveKey, keyCodeText, saveValueFits, type OneShot } from './oneshot.ts';
import type { CommandResult, HelperView, StartRequest, VoiceState } from './supervisor.ts';

// The longest a push-to-talk transmission may last without a release
// (decision D-3: not each open-mic opening). Nick set 30 s on 2026-10-28.
export const DESKTOP_MISSED_RELEASE_SECS = 30;

// The helper options every start carries (D3-1 / #1372).
export const CEILING_ARGS: readonly string[] = ['--transmit-ceiling-secs', String(DESKTOP_MISSED_RELEASE_SECS)];

// Who plays other participants' share audio (#934, #1581): the renderer's
// companion, p<id>.screen, does. Without this flag the helper would play it as
// well, and a watched share would be heard twice. The helper learns the
// companion's identity from its own session response; main passes nothing else.
export const SHARE_AUDIO_ARGS: readonly string[] = ['--share-audio-owner', 'companion'];

// Every option a start carries, the ceiling last.
export const HELPER_ARGS: readonly string[] = [...SHARE_AUDIO_ARGS, ...CEILING_ARGS];

// The part of the Supervisor this service uses; a test stands in for it.
export interface VoiceSupervisor {
  readonly state: VoiceState;
  subscribe(listener: (s: VoiceState) => void): () => void;
  start(req: StartRequest): Promise<VoiceState>;
  stop(): Promise<void>;
  setMute(muted: boolean): Promise<CommandResult>;
  setDeafen(deafened: boolean): Promise<CommandResult>;
  setDevices(devices: { readonly input?: string; readonly output?: string }): Promise<CommandResult>;
  setTalkMode(mode: VoiceControlTalkMode): Promise<CommandResult>;
}

// The activation in force, as voice needs it. Everything but the credential
// is main's own: never a renderer value.
export interface VoiceIdentity {
  // Names conch/<profile-id> in the runtime directory.
  readonly profileId: string;
  // The profile's own directory (CONCH_CONFIG_DIR): voice.toml and key
  // bindings never cross accounts.
  readonly configDir: string;
  // The verified principal the profile is bound to.
  readonly principalId: number;
  // The credential and the canonical origin, read at join time only.
  credential(): { readonly origin: string; readonly token: string } | null;
  // Whether this identity is still the one in force.
  current(): boolean;
}

export interface VoiceServiceDeps {
  readonly supervisor: VoiceSupervisor;
  // The helper's one-shot runs: the device list and a save (issue #1378).
  readonly oneShot: Pick<OneShot, 'devices' | 'save' | 'cancel'>;
  // The identity in force, or null (no profile, signed out, no private data
  // directory).
  readonly identity: () => Promise<VoiceIdentity | null>;
  readonly log: (line: string) => void;
  // Test seams for the key-device listing (issue #1381).
  readonly keyDevices?: KeyDeviceDeps;
  // How long teardown waits for the stop; the contract's kill bound.
  readonly stopBoundMs?: number;
}

export interface JoinRequest {
  readonly channel: string;
  readonly muted: boolean;
  readonly deafened: boolean;
}

// The supervisor's failures and losses a new attempt could fix (tui-voice.md
// §15): everything not here, and every EndpointRefusal, is not retryable.
// attach-* are the hello timeout's kin; events-ended is a lost helper.
const RETRYABLE_PROBLEMS: ReadonlySet<string> = new Set([
  'hello-timeout',
  'ack-timeout',
  'malformed',
  'exited',
  'events-ended',
  'write-failed',
  'spawn-failed',
  'busy',
  'attach-timeout',
  'attach-refused',
  'attach-too-long',
  'attach-second-connection',
  'attach-ended',
]);

// The reasons a helper that ended on its own gives which a new attempt could
// fix (§15's list, and the connection reasons a person would just retry).
const RETRYABLE_REASONS: ReadonlySet<string> = new Set([
  'duplicate_identity',
  'server_unreachable',
  'voice_unavailable',
  'audio_unavailable',
  'start_timeout',
  'internal_error',
  'protocol_error',
  'connect_failed',
  'connection_lost',
  'reconnect_timed_out',
]);

const NOT_SIGNED_IN = 'not-signed-in';

function helperOf(h: HelperView): NativeVoiceHelper {
  return {
    version: h.version,
    phase: h.phase,
    ...(h.reason === undefined ? {} : { reason: h.reason }),
    ...(h.retryInMs === undefined ? {} : { retryInMs: h.retryInMs }),
    capture: h.capture,
    ...(h.playback === undefined ? {} : { playback: h.playback }),
    transmitting: h.transmitting,
    muted: h.muted,
    deafened: h.deafened,
    blocked: [...h.blocked],
    ...(h.talkMode === undefined ? {} : { talkMode: h.talkMode }),
    ptt: h.ptt,
    ...(h.pttProblem === undefined ? {} : { pttProblem: h.pttProblem }),
  };
}

export class VoiceService {
  private channel: string | null = null;
  private last: JoinRequest | null = null;
  private lastGeneration = 0;
  private joining = false;
  // The join in progress, so that a leave can wait for it to be stopped.
  private inflight: Promise<NativeVoiceView> | null = null;
  // Counts leaves and teardowns: a join begun before one is overtaken by it.
  private epoch = 0;
  // Counts teardowns alone: a one-shot begun before one is overtaken by it
  // (a leave of the channel does not touch the person's device list).
  private teardowns = 0;
  // The identity the current join began under; its current() fences what the
  // supervisor reports.
  private active: VoiceIdentity | null = null;
  // Set when the service itself decides what the view is (no identity).
  private override: NativeVoiceView | null = null;
  private readonly listeners = new Set<(view: NativeVoiceView) => void>();
  private readonly bound: number;
  private unsubscribe: (() => void) | null = null;
  // The names of the last key-device listing, by the opaque id it gave each:
  // an id means nothing in any other listing. Replaced by every listing and
  // dropped at teardown.
  private listing = new Map<string, string>();
  private bindingSave = false;

  constructor(private readonly deps: VoiceServiceDeps) {
    this.bound = deps.stopBoundMs ?? VOICE_CONTROL_STOP_KILL_AFTER_MS;
    this.unsubscribe = deps.supervisor.subscribe((s) => {
      this.onSupervisor(s);
    });
  }

  // close detaches from the supervisor; the service reports nothing after.
  close(): void {
    this.unsubscribe?.();
    this.unsubscribe = null;
    this.listeners.clear();
  }

  // state is the view now.
  state(): NativeVoiceView {
    // A join that sign-out, a switch or a 401 has overtaken shows nothing:
    // not its channel, not its helper.
    if (this.active !== null && !this.active.current()) return { status: 'idle', generation: this.lastGeneration, channel: null, retryable: false };
    return this.override ?? this.view(this.deps.supervisor.state);
  }

  subscribe(listener: (view: NativeVoiceView) => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  private onSupervisor(s: VoiceState): void {
    // A join that sign-out, a switch or a 401 has overtaken reports nothing.
    if (this.active !== null && !this.active.current()) return;
    this.override = null;
    this.emit(this.view(s));
  }

  private emit(view: NativeVoiceView): void {
    for (const l of [...this.listeners]) {
      try {
        l(view);
      } catch {
        // A listener's failure is its own.
      }
    }
  }

  private view(s: VoiceState): NativeVoiceView {
    if ('generation' in s) this.lastGeneration = s.generation;
    const status: NativeVoiceStatus = s.status;
    switch (s.status) {
      case 'idle':
        return { status, generation: this.lastGeneration, channel: null, retryable: false };
      case 'unavailable':
        return { status, generation: this.lastGeneration, channel: null, problem: s.reason, retryable: false };
      case 'starting':
      case 'stopping':
        return { status, generation: s.generation, channel: this.channel, retryable: false };
      case 'running':
        return { status, generation: s.generation, channel: this.channel, retryable: false, helper: helperOf(s.helper) };
      case 'ended': {
        const reason = s.helper.reason;
        const retryable = reason !== undefined && RETRYABLE_REASONS.has(reason);
        return { status, generation: s.generation, channel: this.channel, ...(reason === undefined ? {} : { problem: reason }), retryable, helper: helperOf(s.helper) };
      }
      case 'failed':
      case 'lost':
        return { status, generation: s.generation, channel: this.channel, problem: s.reason, retryable: RETRYABLE_PROBLEMS.has(s.reason) };
    }
  }

  private running(): boolean {
    const st = this.deps.supervisor.state.status;
    return this.joining || st === 'starting' || st === 'running' || st === 'stopping';
  }

  // join starts voice in a channel. The destination is set here and nowhere
  // else. A join while a helper runs is refused busy and starts nothing.
  async join(req: JoinRequest): Promise<NativeVoiceView> {
    return this.begin(req);
  }

  // retry is a new join of the last destination, with the mute and deafen the
  // person last set, offered only for a retryable failure.
  async retry(): Promise<NativeVoiceView> {
    const now = this.state();
    if (this.last === null || !now.retryable || this.running()) {
      this.deps.log('voice: retry refused');
      return now;
    }
    return this.begin(this.last);
  }

  private async begin(req: JoinRequest): Promise<NativeVoiceView> {
    if (this.running()) {
      this.deps.log('voice: join refused: busy');
      return { ...this.state(), problem: 'busy' };
    }
    const p = this.run(req);
    this.inflight = p;
    try {
      return await p;
    } finally {
      if (this.inflight === p) this.inflight = null;
    }
  }

  private async run(req: JoinRequest): Promise<NativeVoiceView> {
    this.joining = true;
    const epoch = this.epoch;
    try {
      const identity = await this.deps.identity();
      const credential = identity?.credential() ?? null;
      if (identity === null || credential === null || epoch !== this.epoch) return this.noIdentity();
      this.active = identity;
      this.override = null;
      this.channel = req.channel;
      this.last = { channel: req.channel, muted: req.muted, deafened: req.deafened };
      let result: VoiceState;
      try {
        // The token is read here, handed on, and not kept.
        result = await this.deps.supervisor.start({
          profileId: identity.profileId,
          configDir: identity.configDir,
          token: credential.token,
          server: credential.origin,
          channel: req.channel,
          principalId: identity.principalId,
          muted: req.muted,
          deafened: req.deafened,
          extraArgs: HELPER_ARGS,
        });
      } catch {
        this.deps.log('voice: the start failed unexpectedly');
        result = { status: 'failed', generation: this.lastGeneration, reason: 'spawn-failed' };
      }
      if (!identity.current() || epoch !== this.epoch) {
        // Overtaken: stop that helper and report nothing for it.
        await this.stopBounded();
        this.channel = null;
        this.active = null;
        return { status: 'idle', generation: this.lastGeneration, channel: null, retryable: false };
      }
      this.active = null;
      if (result.status === 'unavailable' || result.status === 'idle') this.channel = null;
      return this.view(result);
    } finally {
      this.joining = false;
    }
  }

  private noIdentity(): NativeVoiceView {
    const view: NativeVoiceView = { status: 'unavailable', generation: this.lastGeneration, channel: null, problem: NOT_SIGNED_IN, retryable: false };
    this.override = view;
    this.emit(view);
    return view;
  }

  // leave ends voice: leave, close, reap. Safe with nothing running. #773's
  // logind watcher calls it too.
  async leave(): Promise<NativeVoiceView> {
    this.epoch++;
    await this.stopBounded(true);
    this.channel = null;
    this.override = null;
    // Nothing left to retry: a later lost or ended state must not rejoin the
    // channel left (security review of #1374).
    this.last = null;
    return this.state();
  }

  // teardown is the media hook of a switch, sign-out or quit: leave and
  // stop, resolving within the contract's kill bound.
  teardown(): Promise<void> {
    // A one-shot run belongs to the profile it began under: it is killed and
    // reaped with the helper, and one still waiting for its identity is
    // overtaken (issue #1378).
    this.teardowns++;
    this.listing = new Map();
    return Promise.all([this.leave(), this.deps.oneShot.cancel()]).then(() => undefined);
  }

  // devices lists the microphones and speakers (`conch-voice devices --json`),
  // for the profile in force. It starts no voice and reads no login.
  async devices(): Promise<NativeVoiceDevicesResult> {
    const begun = this.teardowns;
    const identity = await this.deps.identity();
    if (identity === null || begun !== this.teardowns) return { ok: false, error: NOT_SIGNED_IN };
    return this.deps.oneShot.devices(identity.configDir);
  }

  // saveDevice runs `config set` for the profile in force. The key and value
  // are checked again here, whatever the caller.
  async saveDevice(request: { readonly key: VoiceDeviceKey; readonly value: string }): Promise<VoiceSaveResult> {
    // The key device and the talk key are saved by saveTalkBinding alone.
    if (!isPageSaveKey(request.key) || !saveValueFits(request.key, request.value)) return { ok: false, error: 'invalid' };
    const begun = this.teardowns;
    const identity = await this.deps.identity();
    if (identity === null || begun !== this.teardowns) return { ok: false, error: NOT_SIGNED_IN };
    return this.deps.oneShot.save(identity.configDir, request.key, request.value);
  }

  // keyDevices lists the devices a talk key could be bound on, by name from
  // /dev/input/by-id, opening none (issue #1381). It needs no login and
  // starts no helper. The ids it gives are good for the next saveTalkBinding
  // until the next listing.
  async keyDevices(): Promise<VoiceKeyDevicesResult> {
    const begun = this.teardowns;
    try {
      const { devices } = await listKeyDevices({ uid: process.getuid?.(), ...this.deps.keyDevices });
      if (begun !== this.teardowns) return { ok: false, error: 'cancelled' };
      this.listing = new Map(devices.map((d) => [d.id, d.name]));
      return { ok: true, devices, disclosure: KEY_DEVICE_DISCLOSURE };
    } catch {
      // No device name or path in the log.
      this.deps.log('voice: the key device list failed');
      return { ok: false, error: 'failed' };
    }
  }

  // saveTalkBinding saves keys.device (the by-id path of a device of the
  // current listing) and then keys.talk (a key code 1..767) for the profile in
  // force, and says which of the two did not save. The id and the code are
  // checked here again, whatever the caller.
  async saveTalkBinding(request: { readonly deviceId: string; readonly code: number }): Promise<VoiceTalkBindingResult> {
    const name = typeof request.deviceId === 'string' ? this.listing.get(request.deviceId) : undefined;
    const code = keyCodeText(request.code);
    if (name === undefined || code === null) return { ok: false, error: 'invalid' };
    const path = devicePath(name);
    if (!saveValueFits('keys.device', path)) return { ok: false, error: 'invalid' };
    if (this.bindingSave) return { ok: false, error: 'busy' };
    this.bindingSave = true;
    try {
      const begun = this.teardowns;
      const identity = await this.deps.identity();
      if (identity === null || begun !== this.teardowns) return { ok: false, error: NOT_SIGNED_IN };
      const device = await this.deps.oneShot.save(identity.configDir, 'keys.device', path);
      if (!device.ok) return { ok: false, error: device.error, failed: 'device' };
      const talk = await this.deps.oneShot.save(identity.configDir, 'keys.talk', code);
      return talk.ok ? { ok: true } : { ok: false, error: talk.error, failed: 'talk' };
    } finally {
      this.bindingSave = false;
    }
  }

  // waitForJoin: also wait for a join still inside Supervisor.start (before it
  // has a process for stop() to reach) to notice it was overtaken and stop
  // what it started, so a teardown never resolves with a helper about to
  // spawn. The same bound holds.
  private async stopBounded(waitForJoin = false): Promise<void> {
    let timer: NodeJS.Timeout | undefined;
    const late = new Promise<void>((resolve) => {
      timer = setTimeout(() => {
        this.deps.log('voice: the helper did not stop within its bound');
        resolve();
      }, this.bound);
    });
    try {
      const stopped = (async () => {
        await this.deps.supervisor.stop().catch(() => undefined);
        if (waitForJoin && this.inflight !== null) await this.inflight.catch(() => undefined);
      })();
      await Promise.race([stopped, late]);
    } finally {
      clearTimeout(timer);
    }
  }

  // The commands resolve with the helper's own ack, never optimistically; the
  // view changes when the helper's state says so.
  async setMute(muted: boolean): Promise<VoiceCommandResult> {
    const r = await this.deps.supervisor.setMute(muted);
    if (r.ok && this.last !== null) this.last = { ...this.last, muted };
    return r;
  }

  async setDeafen(deafened: boolean): Promise<VoiceCommandResult> {
    const r = await this.deps.supervisor.setDeafen(deafened);
    if (r.ok && this.last !== null) this.last = { ...this.last, deafened };
    return r;
  }

  setDevices(devices: { readonly input?: string; readonly output?: string }): Promise<VoiceCommandResult> {
    return this.deps.supervisor.setDevices(devices);
  }

  setTalkMode(mode: VoiceControlTalkMode): Promise<VoiceCommandResult> {
    return this.deps.supervisor.setTalkMode(mode);
  }
}
