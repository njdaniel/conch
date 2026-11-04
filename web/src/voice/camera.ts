// The camera (ADR-015; design docs/design/webcam-video.md §6.2, §7.1, §7.2):
// the person's own camera, as a state machine the voice client drives, on the
// pattern of the screen share (share.ts).
//
// The order is the share's: connect, capture, claim, wait for LiveKit to tell
// this connection that the claim granted the camera (#525), publish. The join
// token allows the microphone alone, so a camera published before the claim is
// refused by LiveKit, and conchd's poller withdraws one it did not grant.
//
// The one rule (S2). The camera button alone sends an un-named claim (start).
// Every other claim names the claim it continues (?started_at=): the SDK's own
// reconnect (reconnected), a full reconnect through conchd while the capture
// is still held, and the adoption of a claim the room's renewal ended
// (resume). conchd refuses a name it no longer holds with 409
// voice_camera_stale, and the capture then stops: a full reconnect after the
// capture was stopped never turns it back on, because nothing is held to
// name.
//
// Stop and release (S5). Unless the connection is resumed by the SDK, or the
// room was renewed and the capture is adopted by the next one, every end
// stops the capture, so the device light goes off, and only then asks conchd
// to end the claim, naming it: the button, leaving voice (and so signing out,
// a change of server or profile, a change of channel), the server ending the
// claim for any reason but the renewal, LiveKit taking the source away, a
// stale answer, another device taking this one's place, a removal, a deleted
// room that was not a renewal, and a refused next session. The device going
// is the same, and nothing captures again until the person asks: never another
// camera. A capture is released, never muted.
//
// The state is what this page does. Whether the page captures or sends is
// read from the local track (connection.ts), never from presence: presence
// says the person's claim, which outlives a displaced connection for a few
// seconds (S5).
import type { CameraClaimResult } from '../api';
import type { VoiceCameraEnded, VoiceCameraEndReason } from '../schema/voice';
import type { Clock } from '../state/feed';

// A camera the browser lists, or the one chosen: its device id and its name.
export interface Camera {
  readonly id: string;
  readonly label: string;
}

// A capture taken off a connection that ended, still capturing but blanked
// (black frames), for the next connection to publish. sdk.ts implements it.
export interface KeptCamera {
  // Whether the browser still captures.
  readonly live: () => boolean;
  // Called once if the device ends the capture while it is kept.
  onEnded: (() => void) | null;
  // Stops the capture.
  readonly stop: () => void;
}

// What the camera needs of the room connection (sdk.ts implements it).
export interface CameraRoom {
  // Captures the camera, exactly the device given (null: the browser's own
  // choice, when the person has made none), and holds it unpublished. Rejects
  // with the browser's error.
  readonly captureCamera: (choice: string | null) => Promise<void>;
  // Resolves once LiveKit has told this connection that it may publish a
  // camera (#525); rejects when it has not within grantWait, or the capture
  // was stopped or kept meanwhile, or the connection closed.
  readonly awaitCameraGrant: () => Promise<void>;
  // Publishes the capture as the camera, with its simulcast layers.
  readonly publishCamera: () => Promise<void>;
  // Stops the capture at once and takes it off the room. Never rejects.
  readonly stopCamera: () => Promise<void>;
  // Takes the capture off this connection, still capturing but blanked, for
  // the next one; null when there was none.
  readonly keepCamera: () => KeptCamera | null;
  // Gives this connection a capture kept from an earlier one, unblanked:
  // publishCamera publishes it.
  readonly adoptCamera: (kept: KeptCamera) => void;
}

// Why the camera is off: the person's own stop, leaving, the connection
// ending, the server's reasons, or LiveKit taking the source away without one
// (server).
export type CameraEnd = 'self' | 'lost' | 'server' | VoiceCameraEndReason;

// Why a camera could not start or was stopped for a reason the person can act
// on (design §7.2's table).
export type CameraFailure =
  | 'blocked'
  | 'not_found'
  | 'in_use'
  | 'not_connected'
  | 'not_allowed'
  | 'full'
  | 'stale'
  | 'rate_limited'
  | 'disconnected'
  | 'failed';

export type CameraState =
  | { readonly kind: 'off'; readonly ended: CameraEnd | null }
  // The browser was asked; a preview may show.
  | { readonly kind: 'capturing' }
  | { readonly kind: 'claiming' }
  // Claimed: waiting for LiveKit's permission update (#525).
  | { readonly kind: 'granted-waiting' }
  | { readonly kind: 'publishing' }
  | { readonly kind: 'sending' }
  // Between two connections: the capture is held, blanked and published
  // nowhere, until the next connection claims it again by name.
  | { readonly kind: 'resuming' }
  | { readonly kind: 'stopping' }
  | { readonly kind: 'failed'; readonly reason: CameraFailure };

export const cameraOff: CameraState = { kind: 'off', ended: null };

// on reports whether the camera is anything but off: what the button shows
// pressed.
export function cameraOn(state: CameraState): boolean {
  return state.kind !== 'off' && state.kind !== 'failed';
}

export interface CameraApi {
  // Claims the camera. Without startedAt it starts a claim (the button
  // alone); with it, it continues that claim.
  readonly claim: (channel: string, startedAt?: string) => Promise<CameraClaimResult>;
  // Releases the claim whose started_at is startedAt, and no other.
  readonly release: (channel: string, startedAt: string) => Promise<unknown>;
}

// What a connection ending means for the camera (disconnect.ts decides it):
// stop the capture and release the claim; stop the capture and leave the
// claim to the person's other device, which took this one's place
// (displaced: conchd's claim is the person's, and it ends idle unless that
// device sends a camera, design §4.6); keep the capture for the next
// connection, which claims it again by name (a full reconnect through
// conchd); or keep it while the room is renewed, which presence confirms
// (renewal).
export type CameraLoss = 'stop' | 'displaced' | 'keep' | 'renewal';

// How long a claim refused because the connection was not yet recorded waits
// before it is asked once more: one pass of conchd's poller (design §7.2).
export const passGap = 1500;
// How long a capture is kept for the next connection before it is stopped:
// the share's resumeLimit (share.ts).
export const keepLimit = 45_000;
// How long a capture whose room was deleted waits for presence to say the
// room was renewed (camera_ended, reason rotated) before it is stopped. The
// deleted room publishes nothing meanwhile, and the capture is blanked.
export const renewalGrace = 3000;
// How long a stop waits for the room to take the camera down before conchd
// is told: the capture is stopped before the wait, and a hung signalling
// connection does not hold the release back.
export const unpublishWait = 1000;

type Refusal = Extract<CameraClaimResult, { ok: false }>;

// refusalFailure names a refused claim.
export function refusalFailure(r: Refusal): CameraFailure {
  if (r.code === 'permission_denied') return 'not_allowed';
  if (r.code === 'voice_camera_full') return 'full';
  if (r.code === 'voice_camera_stale') return 'stale';
  if (r.code === 'voice_camera_rate_limited') return 'rate_limited';
  return 'failed';
}

// errorName is the name of a thrown error. A DOMException is not an Error
// everywhere, so its name is read as a property.
function errorName(err: unknown): unknown {
  return typeof err === 'object' && err !== null && 'name' in err ? err.name : undefined;
}

// captureFailure names a capture the browser refused (design §7.2).
export function captureFailure(err: unknown): CameraFailure {
  switch (errorName(err)) {
    case 'NotAllowedError':
    case 'SecurityError':
      return 'blocked';
    case 'NotFoundError':
      return 'not_found';
    case 'NotReadableError':
      return 'in_use';
    case 'OverconstrainedError':
      return 'not_connected';
    default:
      return 'failed';
  }
}

// What the person is told of each failure.
export function failureNote(reason: CameraFailure): string {
  switch (reason) {
    case 'blocked':
      return "The browser blocked the camera. Allow it in this site's settings.";
    case 'not_found':
      return 'No camera found.';
    case 'in_use':
      return 'The camera is in use by another app.';
    case 'not_connected':
      return 'Your chosen camera is not connected.';
    case 'not_allowed':
      return cameraOffForYou;
    case 'full':
      return 'This call already has 9 cameras on.';
    case 'stale':
      return 'Your camera was turned off.';
    case 'rate_limited':
      return 'The camera was turned on too often. Wait a moment, then try again.';
    case 'disconnected':
      return 'Camera disconnected. Choose a camera in Voice options.';
    case 'failed':
      return 'Could not start the camera.';
  }
}

// What the person is told of each end, or null where they know already.
export function endNote(end: CameraEnd): string | null {
  switch (end) {
    case 'self':
      return null;
    case 'left':
      return 'Your camera stopped when you left voice.';
    case 'lost':
      return 'Your camera stopped when the voice connection ended. Turn it on again if you want to.';
    case 'operator':
      return 'An operator turned off your camera.';
    case 'access_lost':
      return 'Camera is not allowed for you.';
    case 'idle':
      return 'Your camera stopped: nothing was being sent.';
    case 'rotated':
    case 'server':
      return 'Your camera was turned off.';
  }
}

// cameraNote is what the notes say of the camera's state, or null.
export function cameraNote(state: CameraState): string | null {
  switch (state.kind) {
    case 'off':
      return state.ended === null ? null : endNote(state.ended);
    case 'failed':
      return failureNote(state.reason);
    case 'capturing':
      return 'Starting your camera…';
    case 'claiming':
    case 'granted-waiting':
    case 'publishing':
      return 'Starting your camera… Not sent yet.';
    case 'resuming':
      return 'The voice connection is being renewed. Your camera is held, and sends nothing until it is back.';
    case 'stopping':
      return 'Stopping your camera…';
    case 'sending':
      return null;
  }
}

// What Voice options says to someone the server does not let use a camera.
export const cameraOffForYou = 'Camera is off for you on this server.';
// And to someone in Watch only or Listen only (O14).
export const watchOnlyCamera = 'Watch only publishes nothing: no microphone and no camera.';
export const listenOnlyCamera = 'Listen only publishes nothing: no microphone and no camera.';

// The camera chosen among those listed (N7): its own id while listed, or, if
// that id is no longer listed (a browser may give a device a new id on a
// reload), the one camera listed under its name, if exactly one is. Two
// cameras of that name, or none, are not connected (null): never the default
// device, and never a camera by its place in the list. A list the browser
// does not name (null: no camera allowed yet) cannot be matched, and the
// choice is asked for as it is, exactly.
export function findCamera(list: readonly Camera[] | null, chosen: Camera): Camera | null {
  if (list === null) return chosen;
  if (list.some((c) => c.id === chosen.id)) return chosen;
  const named = list.filter((c) => c.label === chosen.label);
  return named.length === 1 ? (named[0] ?? null) : null;
}

// sameInstant reports whether two wire timestamps are one moment, whatever
// their spelling.
function sameInstant(a: string, b: string): boolean {
  const x = Date.parse(a);
  return !Number.isNaN(x) && x === Date.parse(b);
}

// Every reason the server gives is an end the page can say. A reason added to
// the schema without a word for it fails to compile here.
const toldEnd: (reason: VoiceCameraEndReason) => CameraEnd = (reason) => reason;

export class CameraClient {
  private readonly api: CameraApi;
  private readonly onChange: (state: CameraState) => void;
  private readonly clock: Clock;
  private state: CameraState = cameraOff;
  // Each start, stop and resume has a number; a step that resumes after a
  // newer one began does nothing more.
  private attempt = 0;
  private channel: string | null = null;
  private room: CameraRoom | null = null;
  // The started_at of the last claim granted: what every later claim and the
  // release name. held: the claim is known to be this page's, so a release
  // is owed.
  private claimed: string | null = null;
  private held = false;
  // Why the server ended that claim, once presence said.
  private told: CameraEnd | null = null;
  // Between connections: the capture kept, how it was lost, whether presence
  // has confirmed the renewal, and the timers that give up.
  private kept: KeptCamera | null = null;
  private loss: CameraLoss | null = null;
  private renewal = false;
  private keepTimer: unknown = null;
  private graceTimer: unknown = null;
  private reclaiming = false;

  constructor(api: CameraApi, onChange: (state: CameraState) => void, clock: Clock) {
    this.api = api;
    this.onChange = onChange;
    this.clock = clock;
  }

  get current(): CameraState {
    return this.state;
  }

  // holdsKept reports whether a capture is held between connections, still
  // capturing: the indicator's "Camera in use" then.
  holdsKept(): boolean {
    return this.kept?.live() === true;
  }

  private set(state: CameraState): void {
    this.state = state;
    this.onChange(state);
  }

  private idle(): boolean {
    return this.state.kind === 'off' || this.state.kind === 'failed';
  }

  // start is the camera button: capture, claim (un-named, the one place that
  // sends one), wait for the grant, publish. It does nothing unless the
  // camera is off.
  async start(channel: string, room: CameraRoom, choice: string | null): Promise<void> {
    if (!this.idle()) return;
    const mine = ++this.attempt;
    this.channel = channel;
    this.room = room;
    this.told = null;
    this.set({ kind: 'capturing' });
    try {
      await room.captureCamera(choice);
    } catch (err: unknown) {
      if (mine !== this.attempt) return;
      this.set({ kind: 'failed', reason: captureFailure(err) });
      return;
    }
    if (mine !== this.attempt) {
      // Stopped while the browser was asked: what it gave is let go.
      await room.stopCamera();
      return;
    }
    this.set({ kind: 'claiming' });
    let answer = await this.api.claim(channel);
    if (!answer.ok && answer.code === 'voice_no_session' && mine === this.attempt) {
      // The connection may not be recorded yet: asked once more after one
      // pass, still for the press that started it.
      await this.wait(passGap);
      if (mine === this.attempt) answer = await this.api.claim(this.channel);
    }
    if (answer.ok) this.granted(answer.value.started_at);
    if (mine !== this.attempt) {
      // Stopped while the claim was on its way: a claim granted since is let
      // go, unless the camera was started again.
      if (this.idle()) await this.releaseHeld();
      return;
    }
    if (!answer.ok) {
      await room.stopCamera();
      if (mine === this.attempt) this.set({ kind: 'failed', reason: refusalFailure(answer) });
      return;
    }
    await this.publish(mine, room);
  }

  // publish waits for the grant and publishes the capture the room holds.
  private async publish(mine: number, room: CameraRoom): Promise<void> {
    this.set({ kind: 'granted-waiting' });
    try {
      await room.awaitCameraGrant();
      if (mine !== this.attempt) return;
      this.set({ kind: 'publishing' });
      await room.publishCamera();
    } catch {
      // Taken over meanwhile (a stop, or the connection kept the capture for
      // the next one): not this step's to end.
      if (mine !== this.attempt) return;
      await this.stopAndRelease('failed');
      return;
    }
    if (mine !== this.attempt) return;
    this.endKeeping();
    this.set({ kind: 'sending' });
  }

  // stop ends the camera from this side: the button, leaving voice (and so
  // signing out and every change of identity, server or channel), and every
  // connection end that is not resumed. The capture stops first; then conchd
  // is asked to end the claim, naming it.
  async stop(cause: 'self' | 'left' | 'lost'): Promise<void> {
    if (this.idle() || this.state.kind === 'stopping') return;
    await this.stopAndRelease(cause);
  }

  private async stopAndRelease(end: CameraEnd | CameraFailure): Promise<void> {
    const mine = ++this.attempt;
    const room = this.room;
    this.dropKept();
    this.set({ kind: 'stopping' });
    // Called now, so the track is stopped before anything below waits.
    const down = room?.stopCamera() ?? Promise.resolve();
    await this.bounded(down, unpublishWait);
    await this.releaseHeld();
    if (mine === this.attempt) this.set(isFailure(end) ? { kind: 'failed', reason: end } : { kind: 'off', ended: end });
  }

  // ended is the room saying the capture is gone: the device ended it (lost,
  // unplugged, taken by another app), or LiveKit took the source away.
  ended(cause: 'device' | 'server' | 'not_connected'): void {
    if (this.idle() || this.state.kind === 'stopping') return;
    if (cause === 'device' || cause === 'not_connected') {
      // Never another camera: the person chooses.
      void this.stopAndRelease(cause === 'device' ? 'disconnected' : 'not_connected');
      return;
    }
    // A capture kept for the next connection was taken off the old one on
    // purpose.
    if (this.state.kind === 'resuming') return;
    void this.serverEnded();
  }

  // presence is why this person's last claim ended, from their own presence
  // document, matched to the claim by its started_at. Only the room's
  // renewal is resumed; any other reason stops the capture, and nothing is
  // claimed again.
  presence(ended: VoiceCameraEnded | undefined): void {
    if (ended === undefined || this.claimed === null || !sameInstant(ended.started_at, this.claimed)) return;
    if (this.told !== null) return;
    this.told = toldEnd(ended.reason);
    if (this.state.kind === 'off' && this.state.ended === 'server') {
      this.set({ kind: 'off', ended: this.told });
      return;
    }
    if (this.idle() || this.state.kind === 'stopping') return;
    this.held = false;
    if (this.told === 'rotated' && (this.state.kind === 'sending' || this.state.kind === 'resuming')) {
      this.renewal = true;
      if (this.state.kind === 'sending') this.suspend('renewal');
      else this.endGrace();
      void this.resume();
      return;
    }
    if (this.state.kind === 'resuming') {
      void this.giveUp(this.told);
      return;
    }
    void this.serverEnded();
  }

  // lost is the connection ending (connection.ts), before it is closed, with
  // what disconnect.ts says that means for the camera. A camera being started
  // stops: only one already sending is carried to the next connection.
  lost(loss: CameraLoss): void {
    if (this.idle() || this.state.kind === 'stopping') return;
    // Another device of the person's took this one's place: the capture
    // stops at once, and the claim is not this page's to end any more. A
    // release sent now could end a camera the other device has just turned
    // on, on the same claim.
    if (loss === 'displaced') this.held = false;
    if (this.state.kind === 'resuming') {
      // A resume under way on the connection going stops where it is; the
      // next connection resumes again, unless this end is a stop.
      this.attempt++;
      this.reclaiming = false;
      this.room = null;
      if (loss === 'stop' || loss === 'displaced') void this.giveUp('lost');
      return;
    }
    if (this.state.kind !== 'sending' || loss === 'stop' || loss === 'displaced') {
      void this.stopAndRelease('lost');
      return;
    }
    this.suspend(loss);
  }

  // refused is conchd refusing the next session: the capture stops.
  refused(): void {
    if (this.state.kind === 'resuming') void this.giveUp('lost');
  }

  // connected is a new connection up: a capture kept from the last one is
  // claimed again on it, by name.
  connected(channel: string, room: CameraRoom): void {
    if (this.state.kind !== 'resuming' || this.kept === null) return;
    this.channel = channel;
    this.room = room;
    void this.resume();
  }

  // reconnected is the SDK's own reconnect: the connection made anew needs
  // the source granted to it again, so the claim is named. A stale answer
  // means the server no longer holds it, and the capture stops.
  async reconnected(): Promise<void> {
    const { channel, claimed } = this;
    if (this.state.kind !== 'sending' || channel === null || claimed === null) return;
    const mine = this.attempt;
    const answer = await this.api.claim(channel, claimed);
    if (mine !== this.attempt) return;
    if (answer.ok) {
      this.granted(answer.value.started_at);
      return;
    }
    if (answer.code === 'voice_camera_stale') {
      this.held = false;
      await this.stopAndRelease('stale');
    }
  }

  // renamed is the channel renamed (issue #728): what is asked of conchd from
  // now on names the new name.
  renamed(channel: string): void {
    if (this.channel !== null) this.channel = channel;
  }

  // reset forgets why the last camera ended, at a new join: nothing else
  // changes, and nothing is captured.
  reset(): void {
    if (this.idle() && this.state !== cameraOff) this.set(cameraOff);
  }

  // abandon is the client stopping (signed out, another device took over):
  // a capture held between connections stops.
  abandon(): void {
    if (this.state.kind === 'resuming') void this.giveUp('lost');
  }

  // granted records a claim conchd granted. A new time is a new claim, and
  // what presence said of the last one is not about it.
  private granted(startedAt: string): void {
    this.held = true;
    if (this.claimed !== null && sameInstant(this.claimed, startedAt)) return;
    this.claimed = startedAt;
    this.told = null;
  }

  // releaseHeld asks conchd to end the claim if it is known to be this
  // page's, once, naming it.
  private async releaseHeld(): Promise<void> {
    const { channel, claimed } = this;
    if (!this.held || claimed === null || channel === null) return;
    this.held = false;
    await this.api.release(channel, claimed).catch(() => undefined);
  }

  // suspend keeps the capture for the next connection: taken off this one at
  // once, blanked, and stopped if it is not claimed again in time. A deleted
  // room that presence has not yet said was renewed is given renewalGrace.
  private suspend(loss: CameraLoss): void {
    this.attempt++;
    this.loss = loss;
    this.set({ kind: 'resuming' });
    const room = this.room;
    this.room = null;
    const kept = room?.keepCamera() ?? null;
    this.kept = kept;
    if (kept === null || !kept.live()) {
      void this.giveUp('lost');
      return;
    }
    kept.onEnded = () => {
      if (this.kept === kept) void this.giveUp('disconnected');
    };
    if (this.keepTimer === null) {
      this.keepTimer = this.clock.setTimeout(() => {
        this.keepTimer = null;
        if (this.state.kind === 'resuming') void this.giveUp(this.told ?? 'lost');
      }, keepLimit);
    }
    if (loss === 'renewal' && !this.renewal && this.graceTimer === null) {
      this.graceTimer = this.clock.setTimeout(() => {
        this.graceTimer = null;
        if (this.state.kind === 'resuming' && !this.renewal) void this.giveUp(this.told ?? 'lost');
      }, renewalGrace);
    }
  }

  // resume claims the kept capture again on the new connection, naming the
  // claim, once the capture, the connection and (for a renewal) presence's
  // word are all there. Anything but 200 stops the capture.
  private async resume(): Promise<void> {
    const { kept, room, channel, claimed } = this;
    if (this.state.kind !== 'resuming' || this.reclaiming || kept === null || room === null || channel === null || claimed === null) return;
    if (this.loss === 'renewal' && !this.renewal) return;
    if (!kept.live()) {
      await this.giveUp('disconnected');
      return;
    }
    const mine = ++this.attempt;
    this.reclaiming = true;
    const answer = await this.api.claim(channel, claimed);
    this.reclaiming = false;
    if (answer.ok) this.granted(answer.value.started_at);
    if (mine !== this.attempt) {
      if (this.idle()) await this.releaseHeld();
      return;
    }
    if (!answer.ok) {
      this.held = false;
      await this.giveUp(refusalFailure(answer) === 'stale' ? 'stale' : 'failed');
      return;
    }
    if (!kept.live()) {
      await this.giveUp('disconnected');
      return;
    }
    this.kept = null;
    kept.onEnded = null;
    this.renewal = false;
    this.loss = null;
    try {
      room.adoptCamera(kept);
    } catch {
      kept.stop();
      await this.giveUp('failed');
      return;
    }
    await this.publish(mine, room);
  }

  // giveUp ends a camera held between connections: the capture stops, and a
  // claim still known to be this page's is let go.
  private async giveUp(end: CameraEnd | CameraFailure): Promise<void> {
    await this.stopAndRelease(end);
  }

  // serverEnded is the server, or LiveKit, ending the camera: the capture
  // stops. A claim presence has said is over is not asked about again; one it
  // has not is released, naming it, which conchd refuses if it is over.
  private async serverEnded(): Promise<void> {
    await this.stopAndRelease(this.told ?? 'server');
  }

  private dropKept(): void {
    this.endKeeping();
    const kept = this.kept;
    this.kept = null;
    this.renewal = false;
    this.loss = null;
    this.reclaiming = false;
    if (kept === null) return;
    kept.onEnded = null;
    kept.stop();
  }

  private endKeeping(): void {
    if (this.keepTimer !== null) this.clock.clearTimeout(this.keepTimer);
    this.keepTimer = null;
    this.endGrace();
  }

  private endGrace(): void {
    if (this.graceTimer !== null) this.clock.clearTimeout(this.graceTimer);
    this.graceTimer = null;
  }

  private wait(ms: number): Promise<void> {
    return new Promise((resolve) => {
      this.clock.setTimeout(resolve, ms);
    });
  }

  // bounded resolves when p settles, however it settles, or after ms.
  private bounded(p: Promise<unknown>, ms: number): Promise<void> {
    return new Promise((resolve) => {
      const handle = this.clock.setTimeout(resolve, ms);
      const done = () => {
        this.clock.clearTimeout(handle);
        resolve();
      };
      void p.then(done, done);
    });
  }
}

const failures: ReadonlySet<string> = new Set<CameraFailure>(['blocked', 'not_found', 'in_use', 'not_connected', 'not_allowed', 'full', 'stale', 'rate_limited', 'disconnected', 'failed']);

function isFailure(end: CameraEnd | CameraFailure): end is CameraFailure {
  return failures.has(end);
}
