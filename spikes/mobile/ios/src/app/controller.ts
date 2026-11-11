// The voice controller: the one object that owns a phone's place in a voice
// room, built from the logic in src/logic and from three ports (the API, the
// room, the native bridge) so that a test can run it with fakes. No React and
// no SDK is imported here.
//
// What it decides (ADR-016 Decisions 3 and 5):
//  - Join: the presence check first; if the person's own principal is already
//    in the room the phone says so and waits for the person to confirm, since
//    the join displaces the other device (hand-off by explicit displacement).
//    Then the microphone permission, then the observers (from the press),
//    then the voice session, then the room.
//  - Nothing opens by itself: the gate is blocked until connected, shut while
//    muted, deafened or listen-only, and the lifecycle (background, inactive,
//    interruption, route change) shuts it in both modes. Joining, reconnecting and
//    resuming never open it (the resume latch is in logic/gate.ts).
//  - Leave and sign-out tear the media down: the camera stops, the gate shuts,
//    the room disconnects and the observers stop.
//  - The camera (M5-D, ADR-016 Decision 10): off on joining, turned on only by
//    the person's tap, ended by every lifecycle reason, never turned back on
//    by itself (app/camera/controls.ts).
import { cameraOff, type CameraRoom, type CameraState } from '../logic/camera';
import type { Clock } from '../logic/clock';
import { afterDisconnect, afterRefusal, cameraAfter, retryDelay, type DisconnectCause, type Next } from '../logic/disconnect';
import { TransmitGate, phoneMax, type GateMode } from '../logic/gate';
import { IosAudioEvents, type InterruptionEvent, type RouteChangeEvent } from '../logic/ios';
import { Lifecycle, type AppState } from '../logic/lifecycle';
import { Transmitter } from '../logic/mic';
import { ReportQueue, type ReportEvent } from '../logic/reports';
import type { CameraClaimResult, TransmitAnswer, VoiceSessionResult } from '../logic/api';
import type { VoiceParticipant, VoicePresenceV1, VoiceTransmitState } from '../logic/schema/generated';
import type { Result } from './client';
import { CameraControls } from './camera/controls';
import { claimedIn, noCameras, type CameraWants } from './camera/policy';

export type Phase = 'idle' | 'checking' | 'confirm' | 'joining' | 'connected' | 'reconnecting';

export interface VoiceState {
  readonly phase: Phase;
  readonly channel: string | null;
  // Why the phone is not in voice, in words for the person.
  readonly why: string | null;
  // The hand-off warning, while phase is 'confirm'.
  readonly warning: string | null;
  readonly muted: boolean;
  readonly deafened: boolean;
  readonly mode: GateMode;
  readonly listenOnly: boolean;
  // Audio is being sent: the track is unmuted.
  readonly transmitting: boolean;
  // A press is held, or the open microphone has the gate open.
  readonly gateOpen: boolean;
  // The lifecycle reasons now holding the gate shut.
  readonly blocked: readonly string[];
  readonly participants: readonly VoiceParticipant[];
  readonly note: string | null;
  readonly route: string;
  readonly level: number;
  // The phone's own camera (M5-D): the logic's state, whether the button shows
  // it on, and a word of the camera layer's own (the permission, Listen only).
  readonly camera: CameraState;
  readonly cameraOn: boolean;
  readonly cameraExtra: string | null;
}

// The events a room reports; the same as the browser's RoomEvents, less the
// share ones.
export interface RoomEvents {
  onReconnecting: () => void;
  onReconnected: () => void;
  onDisconnected: (cause: DisconnectCause) => void;
  // The microphone capture ended on its own (permission revoked, device lost).
  onMicrophoneEnded: () => void;
  // This phone's camera is gone: the device ended the capture, or LiveKit took
  // the source away.
  onCameraEnded: (cause: 'device' | 'server') => void;
}

export interface RoomPort {
  // Connects, and when publish is true captures and publishes the microphone
  // muted, in the order of logic/mic.ts publishMicrophone.
  connect(url: string, token: string, publish: boolean, events: RoomEvents): Promise<void>;
  // Unmutes (true) or mutes (false) the track and its publication.
  setSending(sending: boolean): Promise<void>;
  setDeafened(deafened: boolean): void;
  disconnect(): Promise<void>;
  // The camera of this connection (logic/camera.ts CameraRoom). Nothing is
  // captured until the camera button is pressed.
  readonly camera: CameraRoom;
  // Which remote cameras to receive: only from claim holders, only while the
  // Voice view is open and shown (app/camera/policy.ts).
  watchCameras(wants: CameraWants): void;
}

export interface NativePort {
  // The microphone permission (asked at Join, never at launch).
  ensurePermissions(): Promise<'granted' | 'microphone_denied'>;
  // The camera permission: asked at the first tap of the camera button, never
  // at launch or at Join (M-C7).
  ensureCameraPermission(): Promise<boolean>;
  // Starts the AVAudioSession interruption and route observers (the SDK owns
  // the session's category and activation); stops them.
  startSession(): Promise<void>;
  stopSession(): Promise<void>;
}

export interface VoiceApi {
  presence(channel: string): Promise<Result<VoicePresenceV1>>;
  voiceSession(channel: string): Promise<VoiceSessionResult>;
  reportTransmit(channel: string, state: VoiceTransmitState, signal: AbortSignal): Promise<TransmitAnswer>;
  claimCamera(channel: string, startedAt?: string): Promise<CameraClaimResult>;
  releaseCamera(channel: string, startedAt: string): Promise<unknown>;
}

export interface ControllerOptions {
  readonly api: VoiceApi;
  readonly native: NativePort;
  readonly newRoom: () => RoomPort;
  readonly clock: Clock;
  readonly random: () => number;
  // The signed-in principal's id; the presence check compares with it.
  readonly me: () => number;
  readonly log: (message: string) => void;
  // Wall-clock milliseconds [Date.now]. Timers pause in the background, so
  // waits that must not outlive a freeze are measured with this, not ticks.
  readonly now?: () => number;
  // How often presence is read while a channel is open, in ms.
  readonly pollEvery?: number;
  // Registers values the diagnostics must never show (the LiveKit host).
  readonly hold?: (...secrets: string[]) => void;
}

export const pollEvery = 3000;
const away = 'Voice ended while the app was away. Join again when you want to talk.';
// Shown when the app returns and an interruption (a call, Siri) has not
// reported its end. iOS does not always deliver the end to an app that was in
// the background, and there is no call to ask the session; the gate stays shut,
// which is the safe side, and Leave clears it.
const interruptionPending = 'An audio interruption (a call, Siri) has not said it is over, so the microphone stays shut. If it is over, leave voice and join again.';
const heldTooLong = 'The microphone shut after 30 s of one press. Release and press again.';

const initial: VoiceState = {
  phase: 'idle',
  channel: null,
  why: null,
  warning: null,
  muted: false,
  deafened: false,
  mode: 'ptt',
  listenOnly: false,
  transmitting: false,
  gateOpen: false,
  blocked: [],
  participants: [],
  note: null,
  route: '',
  level: 0,
  camera: cameraOff,
  cameraOn: false,
  cameraExtra: null,
};

export class VoiceController {
  private readonly o: ControllerOptions;
  private s: VoiceState = initial;
  private readonly listeners = new Set<() => void>();
  readonly gate: TransmitGate;
  readonly lifecycle: Lifecycle;
  // The AVAudioSession events, as logic/ios.ts reads them; its `delivered`
  // list is what iOS actually sent, for the receipt.
  readonly audio: IosAudioEvents;
  private readonly reports: ReportQueue;
  private readonly transmitter: Transmitter;
  private readonly camera: CameraControls;
  // The Voice view is open; who holds a camera claim (presence).
  private viewing = false;
  private claimed: ReadonlySet<number> = new Set();
  private room: RoomPort | null = null;
  // Numbers each join and each connection: a late answer from an older one
  // does nothing.
  private attempt = 0;
  private retries = 0;
  private everConnected = false;
  private held = false;
  private retryTimer: unknown = null;
  private pollTimer: unknown = null;
  private polling: string | null = null;
  // The channel whose Voice view is on screen, joined or not (watch).
  private viewedChannel: string | null = null;

  constructor(options: ControllerOptions) {
    this.o = options;
    this.transmitter = new Transmitter({
      clock: options.clock,
      reports: { push: (state, done) => this.reports.push(state, done) },
      setSending: (on) => this.room?.setSending(on) ?? Promise.resolve(),
      onTransmitting: (transmitting) => this.set({ transmitting }),
    });
    this.reports = new ReportQueue({
      clock: options.clock,
      send: (state, signal) => {
        const channel = this.s.channel;
        return channel === null ? Promise.resolve('no_session') : options.api.reportTransmit(channel, state, signal);
      },
      onEvent: (event) => this.onReport(event),
    });
    this.gate = new TransmitGate(
      options.clock,
      (open) => {
        // A gate that shut while the press was still held was the bound.
        if (!open && this.held) {
          this.held = false;
          this.set({ note: heldTooLong });
        }
        this.set({ gateOpen: open });
        this.transmitter.onGate(open);
      },
      { max: phoneMax },
    );
    this.camera = new CameraControls(
      {
        api: { claimCamera: (c, at) => options.api.claimCamera(c, at), releaseCamera: (c, at) => options.api.releaseCamera(c, at) },
        permission: () => options.native.ensureCameraPermission(),
        blocked: () => this.lifecycle.blocked,
        clock: options.clock,
        now: options.now ?? Date.now,
      },
      () => {
        this.set({ camera: this.camera.state, cameraOn: this.camera.on, cameraExtra: this.camera.extra });
      },
    );
    this.lifecycle = new Lifecycle(this.gate, this.camera.client);
    this.audio = new IosAudioEvents(this.lifecycle, this.gate);
  }

  // ---- state

  get state(): VoiceState {
    return this.s;
  }

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  private set(patch: Partial<VoiceState>): void {
    this.s = { ...this.s, ...patch };
    for (const l of [...this.listeners]) l();
  }

  // ---- join and leave

  // join starts the presence check. If the person's own principal is already
  // in the room, the phone waits in 'confirm' for confirmJoin.
  async join(channel: string): Promise<void> {
    if (this.s.phase !== 'idle') return;
    const mine = ++this.attempt;
    this.camera.reset();
    this.set({ ...initialFor(this.s), phase: 'checking', channel, why: null, warning: null, note: null });
    const presence = await this.o.api.presence(channel);
    if (mine !== this.attempt) return;
    if (!presence.ok) {
      this.fail(presence.failure === 'network' ? 'Could not reach the server.' : 'Could not read who is in this voice room.');
      return;
    }
    if (!presence.value.configured || !presence.value.available) {
      this.fail('Voice is not available on this server right now.');
      return;
    }
    const me = this.o.me();
    const here = presence.value.rooms.flatMap((r) => r.audience === undefined ? r.participants : []).some((p) => p.principal_id === me);
    if (here) {
      this.set({
        phase: 'confirm',
        warning: 'You are already in this voice room on another device. Joining here takes your place: the other device will stop.',
      });
      return;
    }
    await this.start(channel, mine);
  }

  // confirmJoin is the person agreeing to the displacement.
  async confirmJoin(): Promise<void> {
    const channel = this.s.channel;
    if (this.s.phase !== 'confirm' || channel === null) return;
    await this.start(channel, this.attempt);
  }

  cancelJoin(): void {
    if (this.s.phase !== 'confirm') return;
    this.attempt++;
    this.set({ phase: 'idle', warning: null });
  }

  private async start(channel: string, mine: number): Promise<void> {
    this.set({ phase: 'joining', warning: null });
    const permission = await this.o.native.ensurePermissions();
    if (mine !== this.attempt) return;
    if (permission === 'microphone_denied') {
      this.fail('Voice needs the microphone. Allow it for this app in iOS Settings, then join again.');
      return;
    }
    try {
      // From the Join press.
      await this.o.native.startSession();
    } catch (e) {
      if (mine !== this.attempt) return;
      this.o.log(`audio observers refused: ${errorName(e)}`);
      this.fail(`iOS would not start the audio observers (${errorName(e)}).`);
      return;
    }
    if (mine !== this.attempt) {
      void this.o.native.stopSession();
      return;
    }
    this.retries = 0;
    this.everConnected = false;
    await this.connect(channel, mine);
  }

  private async connect(channel: string, mine: number): Promise<void> {
    const session = await this.o.api.voiceSession(channel);
    if (mine !== this.attempt) return;
    if (!session.ok) {
      this.camera.refused();
      this.act(afterRefusal(session), mine);
      return;
    }
    const grant = session.value.rooms.find((g) => g.audience === undefined);
    if (grant === undefined) {
      this.act({ kind: 'stop', why: 'This channel has no voice room for everyone.' }, mine);
      return;
    }
    // The grant's address is the server's, and never for a log: its host, and
    // host:port, are held by the diagnostics whatever shape they take.
    const target = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]+)/i.exec(session.value.livekit_url)?.[1];
    this.o.hold?.(session.value.livekit_url, ...(target === undefined ? [] : [target, target.replace(/:\d+$/, '')]));
    const room = this.o.newRoom();
    this.room = room;
    this.transmitter.reset();
    const events: RoomEvents = {
      onReconnecting: () => {
        if (mine !== this.attempt) return;
        this.set({ phase: 'reconnecting' });
        this.applyBlock();
        this.updateWants();
      },
      onReconnected: () => {
        if (mine !== this.attempt) return;
        this.set({ phase: 'connected' });
        this.applyBlock();
        this.updateWants();
        // The SDK's own reconnect: the camera's claim is named again.
        void this.camera.reconnected();
      },
      onDisconnected: (cause) => {
        if (mine !== this.attempt || room !== this.room) return;
        this.o.log(`disconnected: ${cause}`);
        // The capture stops, or is held for the next connection, before
        // anything else is decided (disconnect.ts cameraAfter).
        this.camera.lost(cameraAfter(cause));
        this.act(afterDisconnect(cause), mine);
      },
      onCameraEnded: (cause) => {
        if (mine !== this.attempt || room !== this.room) return;
        this.camera.ended(cause);
      },
      onMicrophoneEnded: () => {
        if (mine !== this.attempt) return;
        // Permission revoked or the device lost (M-I3): the gate is blocked, the
        // person is told, and nothing retries.
        this.gate.setBlocked(true);
        this.set({ note: 'The microphone stopped. Check the app’s microphone permission, then leave and join again.', muted: true });
      },
    };
    try {
      await room.connect(session.value.livekit_url, grant.token, grant.can_publish, events);
    } catch (e) {
      if (mine !== this.attempt || room !== this.room) return;
      this.o.log(`connect failed: ${errorName(e)}`);
      this.dropRoom();
      // A first connection that fails is not retried in a loop: voice is
      // unavailable (M-R4). One that had worked is retried by the table.
      this.act(this.everConnected ? { kind: 'retry' } : { kind: 'stop', why: 'Could not connect to voice. Voice may be unavailable on this server.' }, mine);
      return;
    }
    if (mine !== this.attempt || room !== this.room) {
      void room.disconnect();
      return;
    }
    this.everConnected = true;
    this.retries = 0;
    room.setDeafened(this.s.deafened);
    this.set({ phase: 'connected', why: null, listenOnly: !grant.can_publish });
    this.applyBlock();
    this.camera.connected(channel, room.camera);
    this.updateWants();
    this.startPolling(channel);
  }

  // act carries out the disconnect table's answer.
  private act(next: Next, mine: number): void {
    if (mine !== this.attempt) return;
    switch (next.kind) {
      case 'stop':
        void this.leave(next.why, false);
        return;
      case 'new_session': {
        this.dropRoom();
        void this.afterAway(mine, 0);
        return;
      }
      case 'retry': {
        this.dropRoom();
        void this.afterAway(mine, retryDelay(this.retries++, this.o.random));
        return;
      }
    }
  }

  // afterAway decides, after a connection ended: while the app is away the
  // phone leaves voice and says so (M-L7); in the foreground it connects again
  // through conchd, with the gate shut.
  private afterAway(mine: number, delay: number): void {
    const channel = this.s.channel;
    if (channel === null) return;
    if (this.lifecycle.blocked) {
      void this.leave(away, false);
      return;
    }
    this.set({ phase: 'reconnecting' });
    this.retryTimer = this.o.clock.setTimeout(() => {
      this.retryTimer = null;
      if (mine !== this.attempt) return;
      void this.connect(channel, mine);
    }, delay);
  }

  private dropRoom(): void {
    const room = this.room;
    this.room = null;
    this.transmitter.reset();
    this.reports.reset();
    this.gate.setBlocked(true);
    if (room !== null) void room.disconnect().catch(() => undefined);
    this.claimed = new Set();
  }

  // leave is the Leave button. graceful waits (briefly) for the microphone to
  // mute and `stopped` to be reported before the room goes.
  async leave(why: string | null = null, graceful = true): Promise<void> {
    if (this.s.phase === 'idle') return;
    const mine = ++this.attempt;
    this.clearRetry();
    this.held = false;
    this.gate.shut();
    this.gate.setBlocked(true);
    // The capture stops first, then the claim is released while the room and
    // the credential still stand.
    await this.camera.stop('left');
    if (graceful && this.room !== null) {
      await Promise.race([this.transmitter.idle(), this.after(1500)]);
      await this.reports.drain(1500);
    }
    if (mine !== this.attempt) return;
    this.dropRoom();
    // Once more, after the room is gone: a camera tap that was still in
    // flight when Leave began must not turn the camera on afterwards.
    await this.camera.stop('left');
    await this.o.native.stopSession().catch(() => undefined);
    this.clearInterruption();
    this.set({ ...initialFor(this.s), phase: 'idle', channel: this.s.channel, why, warning: null, participants: [], transmitting: false, gateOpen: false });
    // Presence follows the channel on screen again.
    if (this.viewedChannel !== null) this.startPolling(this.viewedChannel);
  }

  // tearDown is the credential gone (a 401): media first, no waiting, and no
  // report is sent with a dead bearer. The app shows signed out after it.
  async tearDown(): Promise<void> {
    this.attempt++;
    this.clearRetry();
    this.stopPolling();
    this.held = false;
    this.gate.shut();
    this.gate.setBlocked(true);
    // The capture stops now (synchronously, inside stop), the room is dropped,
    // and only then is the release awaited: a claim that hangs (up to 10 s)
    // must not keep the room and the observers up.
    const stopped = this.camera.stop('left');
    this.dropRoom();
    await this.o.native.stopSession().catch(() => undefined);
    this.clearInterruption();
    await stopped;
    this.set({ ...initial, camera: this.camera.state, cameraOn: this.camera.on, cameraExtra: this.camera.extra });
  }

  // clearInterruption forgets an interruption after the observers stopped:
  // nothing will deliver its end any more, so it must not outlive the room (a
  // stale reason would hold the next join's gate shut for good).
  private clearInterruption(): void {
    const before = this.lifecycle.reasons;
    this.lifecycle.interrupted(false);
    this.lifecycleChanged(before);
    if (this.s.note === interruptionPending) this.set({ note: null });
  }

  private fail(why: string): void {
    this.attempt++;
    this.set({ phase: 'idle', why, warning: null });
    this.o.log(`join failed: ${why}`);
  }

  private after(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.o.clock.setTimeout(resolve, ms);
    });
  }

  private clearRetry(): void {
    if (this.retryTimer !== null) this.o.clock.clearTimeout(this.retryTimer);
    this.retryTimer = null;
  }

  // ---- presence

  // watch reads the presence of the channel on screen. While the phone is in
  // voice in one channel, presence follows that one, whatever is on screen: a
  // Voice view of another channel neither replaces the joined room's
  // participants nor stops their polling.
  watch(channel: string | null): void {
    const was = this.viewedChannel;
    this.viewedChannel = channel;
    const joined = this.s.phase === 'idle' ? null : this.s.channel;
    // The joined channel's Voice view closing takes the level probe, and with
    // it the open microphone's samples, so the gate must not stay open: it
    // shuts, arms the resume latch, and a held press ends.
    if (channel === null && joined !== null && was === joined) {
      this.held = false;
      this.gate.shutAndArm();
    }
    if (channel === null) {
      if (was !== null && joined !== null && was !== joined) return;
      this.stopPolling();
      return;
    }
    if (joined !== null && channel !== joined) return;
    this.startPolling(channel);
  }

  private startPolling(channel: string): void {
    if (this.polling === channel) return;
    this.stopPolling();
    this.polling = channel;
    const tick = async (): Promise<void> => {
      const result = await this.o.api.presence(channel);
      if (this.polling !== channel) return;
      if (result.ok) {
        const room = result.value.rooms.find((r) => r.audience === undefined);
        this.set({ participants: room?.participants ?? [] });
        this.claimed = claimedIn(room?.participants ?? []);
        this.camera.presence(result.value.camera_ended);
        this.updateWants();
      }
      this.pollTimer = this.o.clock.setTimeout(() => void tick(), this.o.pollEvery ?? pollEvery);
    };
    void tick();
  }

  private stopPolling(): void {
    this.polling = null;
    if (this.pollTimer !== null) this.o.clock.clearTimeout(this.pollTimer);
    this.pollTimer = null;
  }

  // ---- the controls

  setMuted(muted: boolean): void {
    this.set({ muted });
    this.applyBlock();
  }

  setDeafened(deafened: boolean): void {
    this.set({ deafened });
    this.room?.setDeafened(deafened);
    this.applyBlock();
  }

  setMode(mode: GateMode): void {
    this.set({ mode });
    this.gate.setMode(mode);
  }

  press(): void {
    this.held = true;
    this.set({ note: null });
    this.gate.press();
    if (!this.gate.open) this.held = false;
  }

  release(): void {
    this.held = false;
    this.gate.release();
  }

  // cancel is a touch cancel (the system took the touch).
  cancel(): void {
    this.held = false;
    this.gate.cancel();
  }

  // level is the microphone level, for the open microphone.
  level(value: number): void {
    this.gate.level(value);
    this.set({ level: value });
  }

  private applyBlock(): void {
    const s = this.s;
    this.gate.setBlocked(s.phase !== 'connected' || s.muted || s.deafened || s.listenOnly);
  }

  // ---- the lifecycle: the app's bridge feeds these (lifecycle.ts)

  appState(state: AppState): void {
    this.held = false;
    const before = this.lifecycle.reasons;
    this.lifecycle.appState(state);
    this.lifecycleChanged(before);
    // Back in front with an interruption that never said it ended: say so.
    if (state === 'active' && this.s.phase !== 'idle' && this.lifecycle.reasons.includes('interruption')) this.set({ note: interruptionPending });
  }

  // interruption is AVAudioSession's interruption notification (began/ended).
  // The end only lifts the block: shouldResume is recorded and acted on never.
  interruption(event: InterruptionEvent): void {
    this.held = false;
    const before = this.lifecycle.reasons;
    this.audio.interruption(event);
    this.lifecycleChanged(before);
    // The end arrived after all: the note about a missing end is no longer true.
    if (event.type === 'ended' && this.s.note === interruptionPending) this.set({ note: null });
  }

  // lifecycleChanged publishes the reasons now holding, and tells the camera
  // when one has begun (a camera tap waiting for the foreground gives up).
  private lifecycleChanged(before: readonly string[]): void {
    const now = this.lifecycle.reasons;
    if (now.some((r) => !before.includes(r))) this.camera.blockBegan();
    this.set({ blocked: now });
    this.updateWants();
  }

  // ---- the camera

  // cameraTap is the camera button, the only thing that turns it on.
  async cameraTap(): Promise<void> {
    const channel = this.s.channel;
    const room = this.room;
    if (channel === null || room === null || this.s.phase !== 'connected') return;
    await this.camera.tap(channel, room.camera, this.s.listenOnly);
  }

  // setViewing says whether the Voice view is open: cameras are received only
  // while it is.
  setViewing(viewing: boolean): void {
    this.viewing = viewing;
    this.updateWants();
  }

  // updateWants tells the room which cameras to receive. Nothing is received
  // when the view is closed, the app is away, or the phone is not connected.
  private updateWants(): void {
    // Presence is read for the joined channel only (watch): wants built from
    // another channel's claims would name the wrong people.
    const connected = this.s.phase === 'connected' && this.room !== null && this.polling === this.s.channel;
    const wants: CameraWants = connected
      ? { viewing: this.viewing, shown: !this.lifecycle.blocked, claimed: this.claimed }
      : noCameras;
    this.room?.watchCameras(wants);
  }

  // routeChange is AVAudioSession's route-change notification. A held press
  // shuts at once and the person presses again (logic/ios.ts).
  routeChange(event: RouteChangeEvent, route: string): void {
    this.set({ route });
    if (this.gate.open) this.held = false;
    this.audio.routeChange(event);
  }

  private onReport(event: ReportEvent): void {
    switch (event) {
      case 'ok':
      case 'retrying':
        return;
      case 'rate_limited':
        this.set({ note: 'Talking too fast for the server. Wait a moment.' });
        return;
      case 'gave_up':
      case 'no_session':
        // A report that cannot be delivered: the connection is dropped and
        // conchd decides (a late request is worse than none).
        if (this.room !== null) {
          this.camera.lost('keep');
          this.act({ kind: 'new_session' }, this.attempt);
        }
        return;
      case 'unauthorized':
        // The api client has raised the auth signal; the app tears down.
        return;
    }
  }
}

function initialFor(s: VoiceState): VoiceState {
  // What a new join keeps from the last: the person's choices.
  return { ...initial, muted: s.muted, deafened: s.deafened, mode: s.mode, route: s.route, blocked: s.blocked, camera: s.camera, cameraOn: s.cameraOn, cameraExtra: s.cameraExtra };
}

function errorName(e: unknown): string {
  if (e instanceof Error) return e.message === '' ? e.name : e.message;
  return 'unknown';
}
