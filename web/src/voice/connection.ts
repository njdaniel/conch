// The voice client: asks conchd for a session before every connection attempt,
// joins the room, owns the transmit gate and the report queue, and follows the
// disconnect table of conch-voice.md section 5. It knows the SDK only through
// the Room interface below (sdk.ts is the one file that imports it), so the
// tests drive it with a stub.
//
// The join token is passed to Room.connect and dropped. It is never stored,
// logged or put in the DOM.
//
// A press opens the track at once and reports `started` in parallel (issue
// #970; before it, #495 held the track until conchd had answered, and a round
// trip of speech was lost at the start of every press). A release mutes the
// track before it reports `stopped`. If the report is late, conchd's poller
// may see the microphone first and record an unreported transmission, closed
// `reason=reported` once the report arrives (or `reason=muted`, if the release
// comes first): truthful, and rare (conch-voice.md section 6, "Client side"). Audio never depends on conchd being reachable.
//
// What the page says of a transmission is what is sent (issue #965): it says
// transmitting once the track is unmuted, not when the key goes down, and
// stops saying it when the gate shuts, as the track is muted. Between the two
// the gate is open (talking) while the press waits for its answer: nothing is
// sent yet, and the page says it is waiting. The release delay (the gate's
// tail) is the person's to set, and the open microphone's level is read
// every 10 ms, so that an onset opens the gate sooner.
//
// It also holds the screen share (issue #444): the sharer's state machine
// (share.ts), which of the others' streams this page watches and which one is
// focused (voice-multi-share.md §7.1, #1509), and the elements each watched
// stream plays into.
//
// When the room is renewed (rotated, issue #516) every page reconnects to the
// new one, and a share in progress is resumed by its sharer (share.ts). A page
// watching it keeps watching: the share went with the room, so it waits a
// while for the same person's share to come back instead of letting go.
//
// The microphone is the person's choice (issue #658), handed to every new
// room before it connects. It is kept by device id and by name: a browser
// may give a device a new id on a reload (Chromium does while the site's
// permission is not kept), and a choice whose id is no longer listed is the
// one device listed under its name, if exactly one is. A switch shuts and
// blocks the gate while the device changes, so a press held across it ends
// and nothing is sent until a new press. A microphone lost (its capture
// ended, or the device chosen is no longer listed) is missing: the gate stays
// blocked, and nothing is captured, on this connection or the next, until
// the person chooses one.
//
// The browser's voice processing (echo cancellation, noise suppression and
// automatic gain) is one switch per microphone (issue #659), on unless the
// person turned it off for that device, System default being a device of its
// own. A change in voice captures the same device again, through the same
// switch: the browser ignores a change of the filters on a live capture.
//
// The speakers are the person's choice too (issue #660), where the browser
// lets a page choose: the system default, or an output the browser lists,
// handed to every new room before it connects, so that no voice plays
// elsewhere first. Like the microphone, the choice is kept by id and by
// name. One the browser refuses, or no longer lists, is let go for the
// system default, and the page says so; one the SDK moved away from, its
// output gone, is the output it moved to.
//
// Each other person has a volume on this page alone (issue #661), held here
// until the page is reloaded and never stored: handed to every new room
// before it connects, so that it holds through a reconnect, a renewed room,
// and a Leave and a Join. Nobody else hears a difference.
//
// And the camera (ADR-015): the person's own (camera.ts), the camera chosen,
// kept by id and by name and re-matched by name only to exactly one camera
// (N7), and the call view, which receives the cameras of the room only while
// it is shown. Whether the page captures or sends video is read from the
// local track alone (S5). The microphone and the camera are independent:
// mute, deafen and the share leave the camera as it is.
import type { CameraClaimResult, ShareClaimResult, TransmitAnswer, VoiceSessionResult, VoiceViewerResult } from '../api';
import type { VoicePresenceV2, VoiceTransmitState } from '../schema/voice';
import type { Clock } from '../state/feed';
import { CameraClient, cameraOff, cameraOn, findCamera, type Camera, type CameraLoss, type CameraProfile, type CameraRoom, type CameraState } from './camera';
import { afterDisconnect, afterRefusal, afterViewerRefusal, cameraAfter, retryDelay, type DisconnectCause, type Next } from './disconnect';
import { clampTail, defaultTail, TransmitGate, type GateMode, type GateOptions } from './gate';
import { cameraHolders, occupants, principalOf, sharers } from './presence';
import { ReportQueue, type ReportEvent } from './reports';
import { defaultShareQuality, idleShare, qualityLaterNote, resumeLimit, ScreenShare, type ScreenEnd, type ShareDirection, type ShareQuality, type ShareRoom, type ShareSample, type ShareState } from './share';
import { record } from '../diagnostics';

// The names a browser gives the failures of a microphone it was asked for
// (DOMException names). Any other error of a connection is not a device's.
const deviceErrors: ReadonlySet<string> = new Set(['NotAllowedError', 'NotFoundError', 'NotReadableError', 'OverconstrainedError', 'SecurityError', 'AbortError']);

// deviceReason says what a device error means for the person, by its name
// alone, or null for an error that is worth trying again (an AbortError, or
// one that is not a device's). Retrying a microphone the browser refused, or
// one that is not there, cannot succeed until the person does something, so
// those stop instead (issue #586). The words name no device, room or token.
function deviceReason(name: unknown, watchOnly = false): string | null {
  // Someone already watching only is not told to watch only.
  const or = watchOnly ? '.' : ', or use Watch only.';
  switch (name) {
    case 'NotAllowedError':
    case 'SecurityError':
      return `Your browser blocked the microphone. Allow it for this site in the address bar, then join again${or}`;
    case 'NotFoundError':
    case 'OverconstrainedError':
      return `No microphone was found. Connect one and join again${or}`;
    case 'NotReadableError':
      return `The microphone is in use by another program, or could not be opened. Close it and join again${or}`;
    default:
      return null;
  }
}

// errorName is the name of a thrown error. A DOMException is not an Error
// everywhere, so its name is read as a property.
function errorName(err: unknown): unknown {
  return typeof err === 'object' && err !== null && 'name' in err ? err.name : undefined;
}

// What a join says when the microphone chosen is not there (issue #658). The
// choice is let go, so joining again uses the system default.
export const chosenGone = 'The microphone you chose is not connected. Connect it, or join again to use the system default.';

// What a join says when the device its connection was capturing is gone but
// a newer choice was made meanwhile: the choice is kept (issue #677).
export const earlierGone = 'The microphone this connection was using is not connected. Your newer choice is kept; join again to use it.';

// The names of a capture that failed because the device asked for is not there.
const goneErrors: ReadonlySet<string> = new Set(['NotFoundError', 'OverconstrainedError']);

// A microphone the browser lists, or the one chosen: its device id and its
// name.
export interface Microphone {
  readonly id: string;
  readonly label: string;
}

// An output the browser lists, or the one chosen (issue #660): its device id
// and its name.
export type Output = Microphone;

// outputName names an output: null is the system default.
function outputName(output: Output | null): string {
  return output === null ? 'System default' : output.label;
}

// What the page says when the browser refuses an output, or no longer lists
// it; and when the output in use went and the SDK moved sound to another.
export function outputRefused(choice: Output | null, inUse: Output | null): string {
  return `Could not play sound on ${outputName(choice)}; it stays on ${outputName(inUse)}.`;
}

export function outputMoved(now: Output | null): string {
  return `Your speakers were disconnected; sound now plays on ${outputName(now)}.`;
}

// What a capture says of the browser's voice processing (issue #659): each
// filter on, off, or null where the browser does not say.
export interface Processing {
  readonly echoCancellation: boolean | null;
  readonly noiseSuppression: boolean | null;
  readonly autoGainControl: boolean | null;
}

// Where the voice processing chosen for each microphone is kept: by device
// id, and System default (null) as a device of its own. A device never set
// has it on.
export interface ProcessingStore {
  readonly get: (device: string | null) => boolean;
  readonly set: (device: string | null, on: boolean) => void;
}

// Where the person's choice of microphone mode is kept (issue #1244). A
// browser that has never chosen is in open microphone; a store that cannot be
// read says so by returning null.
export interface ModeStore {
  readonly get: () => GateMode | null;
  readonly set: (mode: GateMode) => void;
}

// memoryStore keeps the choices for as long as the client lasts.
function memoryStore(): ProcessingStore {
  const kept = new Map<string | null, boolean>();
  return {
    get: (device) => kept.get(device) ?? true,
    set: (device, on) => {
      kept.set(device, on);
    },
  };
}

export interface RoomEvents {
  readonly onReconnecting: () => void;
  readonly onReconnected: () => void;
  readonly onDisconnected: (cause: DisconnectCause) => void;
  // Principal ids of the active speakers, by the identity p<id>.
  readonly onSpeakers: (principals: readonly number[]) => void;
  // This page's published share is gone (share.ts).
  readonly onScreenEnded: (cause: ScreenEnd) => void;
  // Which of one publisher's watched stream tracks have arrived (#1509).
  readonly onStreamTracks: (principal: number, tracks: ShareTracks) => void;
  // The microphone was lost: its capture ended, or the device chosen is no
  // longer listed. The room has stopped the capture.
  readonly onMicrophoneLost: () => void;
  // The browser's devices changed: microphones came or went.
  readonly onDevicesChanged: () => void;
  // The output in use went, and the SDK moved every voice to this one.
  readonly onOutputMoved: (id: string) => void;
  // This page's camera is gone: its device ended the capture, or LiveKit
  // took the source away (camera.ts).
  readonly onCameraEnded: (cause: 'device' | 'server') => void;
  // This page's own camera track changed: captured, published or stopped.
  readonly onCameraChanged: () => void;
  // Whose camera tracks have arrived, by principal.
  readonly onCameraTracks: (principals: readonly number[]) => void;
}

// What this page's own camera is, as its local track says (S5): none,
// captured but not sent ("Camera in use"), or published ("Sending video").
export type CameraTrack = 'none' | 'captured' | 'published';

export interface ShareTracks {
  readonly video: boolean;
  readonly audio: boolean;
}

// The elements a watched share plays into: its picture, and its sound, which
// has a volume and a mute of its own.
export interface ShareSink {
  readonly video: HTMLVideoElement;
  readonly audio: HTMLAudioElement;
}

// What the client needs of one LiveKit room connection.
export interface Room extends ShareRoom, CameraRoom {
  // Resolves once the room is joined and, when publish is set, the
  // microphone track is published and muted. It rejects if any of that fails
  // (a microphone refused included).
  connect: (url: string, token: string, roomName: string, publish: boolean, events: RoomEvents) => Promise<void>;
  // Captures and publishes the microphone, muted, on a connection made
  // without it (watch only).
  publishMicrophone: () => Promise<void>;
  // The microphone to capture: a device by id, captured exactly or not at
  // all, or null for the system default. Set before connecting.
  setMicrophone: (choice: string | null) => void;
  // Whether the browser's voice processing is asked for, at every capture
  // from here on: the next connection's, and a switch's.
  setProcessing: (on: boolean) => void;
  // Moves the microphone to another device, or captures the same one again,
  // keeping its publication and its mute; or captures and publishes it if
  // nothing is. It rejects, with nothing captured, if the device cannot be
  // had.
  switchMicrophone: (choice: string | null) => Promise<void>;
  // The microphones the browser lists, read without asking for any device;
  // null when it does not name them.
  microphones: () => Promise<readonly Microphone[] | null>;
  // The device the microphone is captured from, or null while none is.
  microphoneInUse: () => string | null;
  // What the capture says of its voice processing, or null while nothing is
  // captured.
  microphoneProcessing: () => Processing | null;
  // Plays every voice, and a watched share's sound, on this output ('' for
  // the system default), now and on what comes later on this connection;
  // before connecting, from the first. It rejects if the browser refuses
  // it, with sound left where it was.
  switchOutput: (id: string) => Promise<void>;
  // The outputs the browser lists and names, read without asking for any
  // device.
  outputs: () => Promise<readonly Output[]>;
  // Subscribes to each watched publisher's share, picture and sound, with the
  // focused stream at the top layer and every other at the lowest; unwatched
  // streams get nothing (voice-multi-share.md §7.1, §8). Microphones are
  // always subscribed.
  setWatches: (watched: ReadonlySet<number>, focused: number | null) => void;
  // Plays one publisher's watched share into these elements, or into none.
  attachStream: (principal: number, sink: ShareSink | null) => void;
  // Opens or closes the track: unmute or mute. Muted again after any republish.
  setSending: (on: boolean) => Promise<void>;
  setDeafened: (on: boolean) => void;
  // How loud this page plays one person's microphone, by principal, 0 to 1:
  // now and whenever their microphone arrives later on this connection.
  // Deafen silences everyone whatever their volume.
  setVolume: (principal: number, volume: number) => void;
  // The microphone level, 0 to 1.
  level: () => number;
  // The camera (ADR-015): what the local track is, the device it is on, the
  // cameras the browser lists (read without asking for any; null while it
  // does not name them), a move of the capture to another camera, exactly,
  // and where the self-view plays.
  cameraTrack: () => CameraTrack;
  cameraInUse: () => string | null;
  cameras: () => Promise<readonly Camera[] | null>;
  switchCamera: (choice: string) => Promise<void>;
  attachSelf: (el: HTMLVideoElement | null) => void;
  // Receiving cameras: whether the call view is open (the person's choice),
  // whether the video is shown here now (setShown: false in the phone layout
  // outside the Voice view, where no camera is received), who holds a camera
  // claim (presence), and the element each person's camera plays into.
  setViewing: (on: boolean) => void;
  setShown: (on: boolean) => void;
  setCameras: (claimed: ReadonlySet<number>) => void;
  attachCamera: (principal: number, el: HTMLVideoElement | null) => void;
  close: () => Promise<void>;
}

export type Phase = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'unavailable' | 'stopped';

export interface VoiceState {
  readonly phase: Phase;
  readonly channel: string | null;
  // Why the client stopped, or what is wrong while it retries.
  readonly why: string | null;
  readonly mode: GateMode;
  readonly muted: boolean;
  readonly deafened: boolean;
  readonly listenOnly: boolean;
  // Joined to watch: connected without the microphone, until asked for it.
  readonly watchOnly: boolean;
  // Watch only beside the person's own voice in another client (issue #936):
  // connected as the viewer, p<id>.view, which publishes nothing and so
  // displaces nobody.
  readonly viewer: boolean;
  // The gate is open: the key is held (or in its release delay), or the open
  // microphone hears speech. Nothing may be sent yet: see transmitting.
  readonly talking: boolean;
  // Audio is being sent: the gate is open and the track unmuted.
  readonly transmitting: boolean;
  // How long the gate stays open after the key comes up, in ms.
  readonly releaseDelay: number;
  readonly level: number;
  readonly threshold: number;
  readonly speaking: ReadonlySet<number>;
  readonly reportNote: 'retrying' | 'rate_limited' | null;
  readonly share: ShareState;
  // The capture and top-layer caps chosen by the sharer, kept locally.
  readonly shareQuality: ShareQuality;
  // Which shares this page watches by publisher, which one is focused (the
  // stage's, at the top layer), and which of each watched share's tracks have
  // arrived (#1509). focusedShare is always one of watched, or null.
  readonly watched: ReadonlySet<number>;
  readonly focusedShare: number | null;
  readonly streamTracks: ReadonlyMap<number, ShareTracks>;
  // The microphones to choose from, or null while they are not read (watch
  // only, listen only) or the browser does not name them.
  readonly microphones: readonly Microphone[] | null;
  // The microphone chosen; null is the system default.
  readonly microphone: Microphone | null;
  // The device the capture is on, by id, or null while nothing is captured.
  readonly inUse: string | null;
  // Whether voice processing is on for the microphone chosen, and what the
  // capture says of it, or null while nothing is captured.
  readonly processing: boolean;
  readonly reported: Processing | null;
  // The microphone was lost, or a switch failed: nothing is captured and the
  // gate is blocked until the person chooses a microphone.
  readonly missing: boolean;
  // Whether this browser lets the page choose the speakers (issue #660);
  // the outputs it lists, or null while they are not read; the output chosen
  // and in use, null being the system default; and what the page says of a
  // refusal, or of a move by the SDK.
  readonly outputSelection: boolean;
  readonly outputs: readonly Output[] | null;
  readonly output: Output | null;
  readonly outputNote: string | null;
  // The volume of each person this page does not play at full volume, by
  // principal, 0 to 1 (issue #661). Anyone not in it is at 1.
  readonly volumes: ReadonlyMap<number, number>;
  // The person's own camera (camera.ts), and what its local track is: the
  // indicators come from that alone, never from presence (S5).
  readonly camera: CameraState;
  readonly cameraTrack: CameraTrack;
  // The cameras to choose from, or null while they are not read or the
  // browser does not name them; the camera chosen, or null for the
  // browser's own choice; and the device the capture is on.
  readonly cameras: readonly Camera[] | null;
  readonly cameraChoice: Camera | null;
  readonly cameraInUse: string | null;
  // Whether the call view is shown, and whose camera tracks have arrived.
  readonly viewing: boolean;
  readonly cameraTracks: ReadonlySet<number>;
}

export interface VoiceEnv {
  readonly clock: Clock;
  readonly random: () => number;
  readonly createSession: (channel: string) => Promise<VoiceSessionResult>;
  // The viewer grant, and its removal (issue #936). Without them a watch
  // beside another client is refused, never made as the person's own p<id>.
  readonly createViewer?: (channel: string) => Promise<VoiceViewerResult>;
  readonly removeViewer?: (channel: string) => Promise<unknown>;
  readonly report: (channel: string, state: VoiceTransmitState, signal: AbortSignal) => Promise<TransmitAnswer>;
  readonly newRoom: () => Room;
  readonly claimShare: (channel: string, quality?: ShareQuality) => Promise<ShareClaimResult>;
  // Releases the share whose claim started at startedAt (issue #487).
  readonly stopShare: (channel: string, startedAt: string) => Promise<unknown>;
  // The camera's claim, un-named or naming the claim it continues, and its
  // release, naming the claim (ADR-015). Without them the camera is never
  // claimed.
  readonly claimCamera?: (channel: string, startedAt?: string) => Promise<CameraClaimResult>;
  readonly stopCamera?: (channel: string, startedAt: string) => Promise<unknown>;
  // The camera chosen earlier in this tab, if any.
  readonly camera?: Camera | null;
  readonly gate?: GateOptions;
  // A timestamped line for each step of a press and its release, for finding
  // why a roster stayed on "transmitting" (issue #1041). Each line is an event
  // name and nothing else: no room, token or person. Absent unless asked for.
  readonly trace?: (line: string) => void;
  // The microphone chosen earlier in this tab, if any (issue #658).
  readonly microphone?: Microphone | null;
  // The voice processing chosen for each microphone (issue #659); without
  // it, the choices last as long as the client.
  readonly processing?: ProcessingStore;
  // The microphone mode chosen earlier in this browser (issue #1244); without
  // it, the mode is open microphone and lasts as long as the client.
  readonly mode?: ModeStore;
  // The share quality chosen earlier in this browser; default is 1080p30.
  readonly shareQuality?: ShareQuality;
  // Whether the speakers may be chosen, and the output chosen earlier in
  // this tab, if any (issue #660).
  readonly outputSelection?: boolean;
  readonly output?: Output | null;
}

// How long the SDK's own reconnect is allowed before the client goes back to
// conchd (conch-voice.md section 5).
export const reconnectLimit = 20_000;
// A connection that lasted this long starts the retry wait over.
export const healthyAfter = 30_000;
// How often the microphone level is read, for the open microphone's gate, and
// how often the meter is told of it. The gate's read was every 50 ms until
// issue #965: an onset waited up to that long before the gate opened.
export const levelEvery = 10;
export const meterEvery = 50;
// How long a Leave waits for the last report.
const leaveDrain = 1000;
// The longest a press waits for its track to unmute, and the longest the
// release of a press waits for the track to mute before `stopped` is
// reported.
export const reportWait = 1000;

export const initialVoice: VoiceState = {
  phase: 'idle',
  channel: null,
  why: null,
  mode: 'open',
  muted: false,
  deafened: false,
  listenOnly: false,
  watchOnly: false,
  viewer: false,
  talking: false,
  transmitting: false,
  releaseDelay: defaultTail,
  level: 0,
  threshold: 0.1,
  speaking: new Set(),
  reportNote: null,
  share: idleShare,
  shareQuality: defaultShareQuality,
  watched: new Set(),
  focusedShare: null,
  streamTracks: new Map(),
  microphones: null,
  microphone: null,
  inUse: null,
  processing: true,
  reported: null,
  missing: false,
  outputSelection: false,
  outputs: null,
  output: null,
  outputNote: null,
  volumes: new Map(),
  camera: cameraOff,
  cameraTrack: 'none',
  cameras: null,
  cameraChoice: null,
  cameraInUse: null,
  viewing: false,
  cameraTracks: new Set(),
};

const noCameraTracks: ReadonlySet<number> = new Set();

// The camera's states before it is published, when its start applies the
// choice itself.
const beforePublish: ReadonlySet<CameraState['kind']> = new Set(['capturing', 'claiming', 'granted-waiting', 'resuming']);

// Without the camera's routes, every claim is refused.
const noCamera = (): Promise<CameraClaimResult> => Promise.resolve({ ok: false, failure: 'invalid' });

// Nothing captured: no device in use, and nothing said of its processing.
const nothingInUse = { inUse: null, reported: null } as const;

// inUseOn is what a room's capture says: its device and its processing.
function inUseOn(room: Room): Pick<VoiceState, 'inUse' | 'reported'> {
  return { inUse: room.microphoneInUse(), reported: room.microphoneProcessing() };
}

export class VoiceClient {
  private readonly env: VoiceEnv;
  private readonly gate: TransmitGate;
  private readonly reports: ReportQueue;
  private state: VoiceState = initialVoice;
  private listeners = new Set<() => void>();
  private room: Room | null = null;
  private channel: string | null = null;
  // Each attempt has a number; answers for an older one are discarded.
  private attempt = 0;
  private failures = 0;
  private retryTimer: unknown = null;
  private reconnectTimer: unknown = null;
  private levelTimer: unknown = null;
  private connectedAt = 0;
  private now: () => number;
  private readonly share: ScreenShare;
  private readonly camera: CameraClient;
  // This person, from the identity of the last session.
  private me: number | null = null;
  // Watches and their players are keyed by publisher. awaiting holds one
  // resume timer per watched publisher whose stream went with its renewed
  // room: that watch ends if the publisher's stream is not back by then
  // (voice-multi-share.md §7.1).
  private readonly streamSinks = new Map<number, ShareSink>();
  private readonly awaiting = new Map<number, unknown>();
  // Who else holds a camera claim, from presence, and the elements the call
  // view plays cameras into: handed to every new connection.
  private cameraClaims: ReadonlySet<number> = new Set();
  private readonly cameraViews = new Map<number, HTMLVideoElement>();
  private selfView: HTMLVideoElement | null = null;
  // Numbers the camera's operations (on, off, a switch, and every new
  // capture or stop, however it came): a switch's outcome counts only while
  // nothing newer has happened to the camera.
  private cameraOps = 0;
  // Whether the call view opens once the button's capture is given.
  private openOnCapture = false;
  // Whether the video is shown on this page now (showVideo): false in the
  // phone layout outside the Voice view.
  private videoShown = true;
  // Counts the page being hidden on a phone (pageHidden), so that a start
  // still choosing its camera can tell it was hidden meanwhile.
  private hides = 0;
  // The gate's changes, carried out one after another: a press's wait for its
  // answer, then a release's report. talk numbers the run of them a
  // connection makes (a drop abandons it); edge counts the gate's changes, so
  // that a press released while it waited does not open the track.
  private steps: Promise<void> = Promise.resolve();
  private talk = 0;
  private edge = 0;
  // Whether a switch of microphone is under way, and which: the gate is
  // blocked meanwhile, and only the last one's outcome counts.
  private switching = false;
  private switches = 0;
  private readonly processing: ProcessingStore;
  private readonly modes: ModeStore | null;
  // Numbers the changes of output: only the last one's refusal counts.
  private outputTurn = 0;

  constructor(env: VoiceEnv, now: () => number = () => Date.now()) {
    this.env = env;
    this.now = now;
    this.processing = env.processing ?? memoryStore();
    this.modes = env.mode ?? null;
    this.state = {
      ...initialVoice,
      mode: this.modes?.get() ?? initialVoice.mode,
      shareQuality: env.shareQuality ?? defaultShareQuality,
      ...this.chosen(env.microphone ?? null),
      outputSelection: env.outputSelection === true,
      output: env.output ?? null,
      releaseDelay: clampTail(env.gate?.tail ?? defaultTail),
      cameraChoice: env.camera ?? null,
    };
    this.gate = new TransmitGate(env.clock, (open) => {
      this.onGate(open);
    }, env.gate);
    this.gate.setMode(this.state.mode);
    this.share = new ScreenShare(
      { claim: env.claimShare, release: env.stopShare },
      (share) => {
        this.set({ share });
      },
      env.clock,
    );
    this.camera = new CameraClient(
      { claim: env.claimCamera ?? noCamera, release: env.stopCamera ?? (() => Promise.resolve()) },
      (camera) => {
        if (camera.kind === 'capturing' || camera.kind === 'stopping' || camera.kind === 'resuming') this.cameraOps++;
        this.set({ camera, ...this.cameraNow() });
        // The button's capture given: the call view opens. Anything else
        // that ends the start opens nothing.
        if (camera.kind !== 'capturing' && this.openOnCapture) {
          this.openOnCapture = false;
          if (camera.kind === 'claiming') this.viewVideo(true);
        }
      },
      env.clock,
      () => this.state.cameraChoice?.id ?? null,
    );
    this.reports = new ReportQueue({
      send: (state, signal) => {
        const channel = this.channel;
        return channel === null ? Promise.resolve('no_session') : env.report(channel, state, signal);
      },
      clock: env.clock,
      onEvent: (event) => {
        this.onReport(event);
      },
    });
  }

  // useSyncExternalStore's pair.
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };
  snapshot = (): VoiceState => this.state;

  private set(patch: Partial<VoiceState>): void {
    // Each change of phase goes into the page's diagnostic ring (issue
    // #446): the state alone, never the channel or the reason's words.
    if (patch.phase !== undefined && patch.phase !== this.state.phase) record({ kind: 'voice_state', state: patch.phase });
    this.state = { ...this.state, ...patch };
    for (const l of [...this.listeners]) l();
  }

  // join connects to a channel's voice. Watch only connects without the
  // microphone: nothing is captured or published until useMicrophone.
  //
  // beside is for Watch only when the person's own voice is in another client
  // (the helper, the TUI): the page then connects as the viewer and never as
  // the person's own p<id>, which would knock that client out of the room.
  join(channel: string, options: { readonly watchOnly?: boolean; readonly beside?: boolean } = {}): void {
    if (this.state.phase !== 'idle' && this.state.phase !== 'stopped') return;
    this.channel = channel;
    this.failures = 0;
    // The camera is off on joining (design §3.4): only its button turns it
    // on, and why the last one ended is not news here.
    this.camera.reset();
    // Joining is the person's own choice: a microphone missing before is
    // asked for again, as chosen.
    this.set({ phase: 'connecting', channel, why: null, reportNote: null, watchOnly: options.watchOnly === true, viewer: options.watchOnly === true && options.beside === true, watched: new Set(), focusedShare: null, streamTracks: new Map(), missing: false });
    void this.connect();
  }

  // renamed is the channel voice is in renamed (issue #728), from the name
  // it was joined by. Nothing is left or reconnected: the room and the
  // presence socket are the channel's by id. What is asked of conchd from now
  // on (the next session, a transmit report, the share) names the new name.
  renamed(from: string, to: string): void {
    if (this.channel !== from || from === to) return;
    this.channel = to;
    this.share.renamed(to);
    this.camera.renamed(to);
    if (this.state.channel === from) this.set({ channel: to });
  }

  // useMicrophone ends watch only: the microphone is captured, published and
  // muted, as on a full join, and the talk controls work from then on.
  async useMicrophone(): Promise<void> {
    const room = this.room;
    if (!this.state.watchOnly || this.state.viewer || this.state.listenOnly || this.state.phase !== 'connected' || room === null) return;
    const mine = this.attempt;
    this.set({ watchOnly: false });
    try {
      await this.findChosen(room);
      if (mine !== this.attempt) return;
      room.setMicrophone(this.state.microphone?.id ?? null);
      room.setProcessing(this.state.processing);
      await room.publishMicrophone();
    } catch (err: unknown) {
      if (mine !== this.attempt) return;
      // The connection stays: the person keeps watching, told why. A chosen
      // device that is not there is let go, as at a join.
      const name = errorName(err);
      if (this.state.microphone !== null && typeof name === 'string' && goneErrors.has(name)) {
        room.setMicrophone(null);
        this.set({ watchOnly: true, ...this.chosen(null), why: chosenGone });
        return;
      }
      this.set({ watchOnly: true, why: deviceReason(name, true) ?? 'The microphone could not be used' });
      return;
    }
    if (mine !== this.attempt) return;
    this.set(inUseOn(room));
    this.applyBlock();
    void this.readMicrophones(mine, room);
  }

  // chooseMicrophone chooses the microphone: a device listed, or null for the
  // system default. The choice is kept for every later connection. In voice
  // with the microphone, the capture moves to it at once, with the gate shut
  // and blocked until it has: a press held across the switch ends, and
  // nothing is sent until a new one. A switch that fails leaves the
  // microphone missing, with the reason. The device is captured with the
  // voice processing chosen for it. Chosen while connecting, it is captured
  // once connected (connect).
  async chooseMicrophone(choice: Microphone | null): Promise<void> {
    this.set(this.chosen(choice));
    const room = this.room;
    const s = this.state;
    if (room === null || s.watchOnly || s.listenOnly || (s.phase !== 'connected' && s.phase !== 'reconnecting')) return;
    await this.switchTo(room, choice?.id ?? null);
  }

  // setProcessing turns the browser's voice processing on or off for the
  // microphone chosen, and keeps that for the device. In voice with the
  // microphone captured, the same device is captured again at once, as by a
  // switch: the gate is shut and blocked until it has been. A microphone
  // missing is not captured: the choice is used once one is chosen.
  async setProcessing(on: boolean): Promise<void> {
    const chosen = this.state.microphone;
    this.processing.set(chosen?.id ?? null, on);
    this.set({ processing: on });
    const room = this.room;
    const s = this.state;
    if (room === null || s.watchOnly || s.listenOnly || s.missing || (s.phase !== 'connected' && s.phase !== 'reconnecting')) return;
    await this.switchTo(room, chosen?.id ?? null);
  }

  // switchTo captures the microphone again, on the device given and with the
  // voice processing chosen, with the gate shut and blocked until it has.
  // Only the last switch's outcome counts.
  private async switchTo(room: Room, choice: string | null): Promise<void> {
    const mine = this.attempt;
    const turn = ++this.switches;
    this.switching = true;
    this.applyBlock();
    // What the old capture said of its processing is not the new one's.
    this.set({ reported: null });
    room.setProcessing(this.state.processing);
    let failure: unknown = null;
    try {
      await room.switchMicrophone(choice);
    } catch (err: unknown) {
      failure = err;
    }
    if (turn !== this.switches) return;
    this.switching = false;
    if (mine !== this.attempt) return;
    if (failure === null) this.set({ missing: false, why: null, ...inUseOn(room) });
    else this.set({ missing: true, ...nothingInUse, why: deviceReason(errorName(failure)) ?? 'The microphone could not be used' });
    this.applyBlock();
    void this.readMicrophones(mine, room);
  }

  // chosen is a microphone chosen, with the voice processing chosen for it.
  private chosen(microphone: Microphone | null): Pick<VoiceState, 'microphone' | 'processing'> {
    return { microphone, processing: this.processing.get(microphone?.id ?? null) };
  }

  // findChosen finds the microphone chosen among those listed, before it is
  // captured: a choice whose id is no longer listed takes the id of the one
  // device listed under its name. With none, or more than one, it stays as
  // it is, and its capture fails. The list is read without asking for any
  // device, so a browser that does not name its devices yet finds nothing.
  private async findChosen(room: Room): Promise<void> {
    const chosen = this.state.microphone;
    if (chosen === null) return;
    const list = await room.microphones().catch(() => null);
    if (list === null || list.some((m) => m.id === chosen.id) || this.state.microphone !== chosen) return;
    const named = list.filter((m) => m.label === chosen.label);
    const found = named.length === 1 ? named[0] : undefined;
    if (found === undefined) return;
    // Its voice processing goes with it to the new id.
    this.processing.set(found.id, this.processing.get(chosen.id));
    this.set(this.chosen(found));
  }

  // readMicrophones reads the list of microphones and the device in use,
  // while this page uses its microphone: never in watch only or listen only.
  // The outputs are read at the same times, in watch only and listen only
  // too: sound plays there as well.
  private async readMicrophones(mine: number, room: Room): Promise<void> {
    void this.readOutputs(mine, room);
    if (this.state.watchOnly || this.state.listenOnly) return;
    const microphones = await room.microphones().catch(() => null);
    if (mine !== this.attempt) return;
    this.set({ microphones, ...(this.state.missing ? nothingInUse : inUseOn(room)) });
  }

  // chooseOutput chooses the speakers: an output listed, or null for the
  // system default. The choice is kept for every later connection. In voice,
  // every voice moves there at once; one the browser refuses leaves sound
  // where it was, and the page says so.
  async chooseOutput(choice: Output | null): Promise<void> {
    const before = this.state.output;
    const turn = ++this.outputTurn;
    this.set({ output: choice, outputNote: null });
    const room = this.room;
    if (room === null || !this.state.outputSelection) return;
    try {
      await room.switchOutput(choice?.id ?? '');
    } catch {
      if (turn !== this.outputTurn || room !== this.room) return;
      this.set({ output: before, outputNote: outputRefused(choice, before) });
    }
  }

  // findOutput finds the output chosen among those listed: by its id, or,
  // if that is no longer listed, the one output listed under its name. It is
  // null if neither is listed. While the browser names no output it cannot
  // be told, and is taken as it is.
  private findOutput(list: readonly Output[] | null, chosen: Output): Output | null {
    if (list === null || list.length === 0) return chosen;
    const listed = list.find((o) => o.id === chosen.id);
    if (listed !== undefined) return listed;
    const named = list.filter((o) => o.label === chosen.label);
    return named.length === 1 ? (named[0] ?? null) : null;
  }

  // applyOutput hands the output chosen to a new room before it connects,
  // found by its name if its id has changed. One not listed, or refused, is
  // let go: the room plays on the system default, and the page says so.
  private async applyOutput(room: Room): Promise<void> {
    const chosen = this.state.output;
    if (chosen === null) return;
    const found = this.findOutput(await room.outputs().catch(() => null), chosen);
    // Chosen again meanwhile: chooseOutput handed that to this room.
    if (this.state.output !== chosen) return;
    if (found !== null) {
      try {
        await room.switchOutput(found.id);
        if (this.state.output === chosen) this.set({ output: found });
        return;
      } catch {
        // Let go, below.
      }
    }
    if (this.state.output === chosen) this.set({ output: null, outputNote: outputRefused(chosen, null) });
  }

  // readOutputs reads the outputs the browser lists, and finds the output
  // chosen among them again: the browser may name its outputs only once the
  // microphone is allowed, and give them new ids then. One under a new id
  // is played on by that id; one no longer listed is let go.
  private async readOutputs(mine: number, room: Room): Promise<void> {
    if (!this.state.outputSelection) return;
    const outputs = await room.outputs().catch(() => null);
    if (mine !== this.attempt) return;
    this.set({ outputs });
    const chosen = this.state.output;
    if (chosen === null) return;
    const found = this.findOutput(outputs, chosen);
    if (found !== null && found.id === chosen.id) {
      if (found.label !== chosen.label) this.set({ output: found });
      return;
    }
    const turn = ++this.outputTurn;
    this.set(found === null ? { output: null, outputNote: outputRefused(chosen, null) } : { output: found });
    try {
      await room.switchOutput(found?.id ?? '');
    } catch {
      if (turn !== this.outputTurn || mine !== this.attempt || found === null) return;
      this.set({ output: null, outputNote: outputRefused(found, null) });
    }
  }

  // startShare shares a screen into the room this page is connected to.
  startShare(): void {
    const channel = this.channel;
    const room = this.room;
    if (this.state.phase !== 'connected' || channel === null || room === null) return;
    void this.share.start(channel, room, this.state.shareQuality);
  }

  // A quality choice applies to the next picker, and to a live capture and
  // its sender without ending the publication; once the capture has it, conchd
  // is told, for the others' label (share.ts changeQuality). The local choice
  // is kept even if this browser cannot apply it to the track already open,
  // and then conchd is not told: the stream did not change.
  async setShareQuality(quality: ShareQuality): Promise<void> {
    this.set({ shareQuality: quality });
    try {
      await this.room?.setShareQuality(quality);
    } catch {
      if (this.state.shareQuality === quality) this.set({ why: 'The share quality could not be changed on this capture.' });
      return;
    }
    const answer = await this.share.changeQuality(quality);
    if (answer === 'later' && this.state.shareQuality === quality) this.set({ why: qualityLaterNote });
  }

  readShareStats(direction: ShareDirection, principal?: number): Promise<ShareSample | null> {
    return this.room?.readShareStats(direction, principal).catch(() => null) ?? Promise.resolve(null);
  }

  stopShare(): void {
    void this.share.stop('self');
  }

  // ---- the camera

  // startCamera is the camera button, and nothing else: it turns the camera
  // on in the room this page is connected to, with the microphone, and opens
  // the call view. The camera chosen is found again by its name if its id has
  // changed, and only if exactly one camera carries it; one not found is not
  // connected, and no other camera is taken.
  //
  // The profile (phone or desktop, design §7.5) is the capture's, chosen here
  // as it starts and kept for its switches and publish.
  async startCamera(profile: CameraProfile = 'desktop'): Promise<void> {
    const channel = this.channel;
    const room = this.room;
    const s = this.state;
    if (s.phase !== 'connected' || s.watchOnly || s.listenOnly || channel === null || room === null) return;
    if (s.camera.kind !== 'off' && s.camera.kind !== 'failed') return;
    const mine = this.attempt;
    const hides = this.hides;
    const choice = await this.cameraToUse();
    if (mine !== this.attempt || room !== this.room) return;
    // The page was hidden on a phone while the camera was being chosen: the
    // start ends here, with nothing captured.
    if (hides !== this.hides) return;
    if (choice === false) {
      // The camera chosen is not connected: said, through the camera's own
      // state, and nothing is captured.
      this.camera.fail('not_connected');
      return;
    }
    // The call view opens once the browser gave the capture (O8): a camera
    // refused opens nothing.
    this.openOnCapture = true;
    await this.camera.start(channel, room, choice, profile);
    if (mine !== this.attempt || room !== this.room) return;
    void this.readCameras(mine, room);
  }

  // cameraToUse is the camera to capture: the one chosen, found again by its
  // name if its id has changed and exactly one camera carries it (N7); null
  // for none chosen (the browser's own choice); or false for a choice that is
  // not connected, when nothing is to be captured: never another camera.
  async cameraToUse(): Promise<string | null | false> {
    const room = this.room;
    const chosen = this.state.cameraChoice;
    if (chosen === null) return null;
    const list = room === null ? null : await room.cameras().catch(() => null);
    if (this.state.cameraChoice !== chosen) return this.state.cameraChoice?.id ?? null;
    if (list !== null) this.set({ cameras: list });
    const found = findCamera(list, chosen);
    if (found === null) return false;
    if (found.id !== chosen.id) this.set({ cameraChoice: found });
    return found.id;
  }

  // stopCamera is the camera button, off: the capture stops, and conchd is
  // told, naming the claim.
  stopCamera(): Promise<void> {
    this.cameraOps++;
    return this.camera.stop('self');
  }

  // pageHidden is the page going out of sight in the phone layout (the
  // caller decides that: phone.ts, at the moment the event fires): the camera
  // ends as the button ends it, at any step of its start, and the capture is
  // released with a DELETE naming the claim. Nothing restarts it when the
  // page shows again: only the button does. The desktop layout never calls
  // it (O9).
  pageHidden(): void {
    this.hides++;
    if (!cameraOn(this.state.camera)) return;
    void this.stopCamera();
  }

  // chooseCamera chooses the camera, kept for every later capture. While the
  // camera is on, its capture moves to the camera chosen, exactly; a move
  // that fails stops the camera, and no other camera is taken.
  async chooseCamera(choice: Camera): Promise<void> {
    this.set({ cameraChoice: choice });
    // Until the camera is published, its own start moves the capture to the
    // choice, read afresh before the grant and before the publish
    // (camera.ts follow): a second switch here would race it.
    if (beforePublish.has(this.state.camera.kind)) return;
    const room = this.room;
    if (room === null || room.cameraTrack() === 'none') return;
    // A switch answers for the capture it moved: once the camera was turned
    // off, on again, or moved again, its outcome is not the camera's now.
    const turn = ++this.cameraOps;
    try {
      await room.switchCamera(choice.id);
    } catch {
      if (room !== this.room || turn !== this.cameraOps) return;
      // The camera chosen could not be had: stopped and released, and no
      // other camera is taken.
      this.camera.ended('not_connected');
      return;
    }
    if (room !== this.room || turn !== this.cameraOps) return;
    this.set(this.cameraNow());
  }

  // viewVideo shows the call view, or closes it: the room's cameras are
  // received only while it is shown.
  viewVideo(on: boolean): void {
    this.room?.setViewing(on);
    this.set(on ? { viewing: true } : { viewing: false, cameraTracks: noCameraTracks });
  }

  // showVideo is whether the video is shown here now: the phone layout
  // outside the Voice view shows none, and every camera is let go there
  // while the person's choice to view (viewVideo) is kept. The person's own
  // camera is not touched: it keeps sending.
  showVideo(on: boolean): void {
    this.videoShown = on;
    this.room?.setShown(on);
  }

  // attachCamera plays a person's camera into a tile, on this connection and
  // on every one after it; attachSelf plays this page's own.
  attachCamera(principal: number, el: HTMLVideoElement | null): void {
    if (el === null) this.cameraViews.delete(principal);
    else this.cameraViews.set(principal, el);
    this.room?.attachCamera(principal, el);
  }

  attachSelf(el: HTMLVideoElement | null): void {
    this.selfView = el;
    this.room?.attachSelf(el);
  }

  // refreshCameras reads the cameras the browser lists again: after the
  // camera test was allowed, the browser names them.
  refreshCameras(): void {
    const room = this.room;
    if (room !== null) void this.readCameras(this.attempt, room);
  }

  // readCameras reads the cameras the browser lists, and the device in use.
  // The camera chosen is found again by its name when its id is no longer
  // listed and exactly one camera carries it (N7); one not found stays
  // chosen, and the picker says it is not connected.
  private async readCameras(mine: number, room: Room): Promise<void> {
    const cameras = await room.cameras().catch(() => null);
    if (mine !== this.attempt || room !== this.room) return;
    const chosen = this.state.cameraChoice;
    const found = chosen === null || cameras === null ? null : findCamera(cameras, chosen);
    this.set({ cameras, ...(found !== null && found.id !== chosen?.id ? { cameraChoice: found } : {}), ...this.cameraNow() });
  }

  // cameraNow is what the local track says: whether it is captured or
  // published, and on which device. A capture held between connections is
  // captured.
  private cameraNow(): Pick<VoiceState, 'cameraTrack' | 'cameraInUse'> {
    const room = this.room;
    const track = room?.cameraTrack() ?? 'none';
    if (track === 'none' && this.camera.holdsKept()) return { cameraTrack: 'captured', cameraInUse: null };
    return { cameraTrack: track, cameraInUse: room?.cameraInUse() ?? null };
  }

  // watchStream subscribes to one publisher's share, picture and sound, or
  // lets it go, without changing any other watch (voice-multi-share.md §7.1,
  // #1509). This person's own stream is never watched here: they have the
  // screen.
  watchStream(principal: number, on: boolean): void {
    if (on && principal === this.me) return;
    this.endAwaiting(principal);
    const watched = new Set(this.state.watched);
    if (on) watched.add(principal);
    else watched.delete(principal);
    const focusedShare = !on && this.state.focusedShare === principal ? null : this.state.focusedShare;
    this.watches(watched, focusedShare);
  }

  // focusShare selects one watched stream for the stage and the top layer, or
  // clears that choice; an unwatched publisher is never selected (#1509).
  focusShare(principal: number | null): void {
    const focusedShare = principal !== null && this.state.watched.has(principal) ? principal : null;
    if (focusedShare === this.state.focusedShare) return;
    this.watches(this.state.watched, focusedShare);
  }

  // switchStream is the phone's one-stream watch: every other watch ends and
  // the chosen publisher becomes watched and focused, picture and sound
  // together, in one step (voice-multi-share.md §7.1 phone layout).
  switchStream(principal: number): void {
    if (principal === this.me) return;
    for (const other of this.state.watched) if (other !== principal) this.endAwaiting(other);
    this.endAwaiting(principal);
    this.watches(new Set([principal]), principal);
  }

  // watches sets what this page watches and which stream is focused, on the
  // connection and in the state; a stream no longer watched loses its tracks.
  private watches(watched: ReadonlySet<number>, focusedShare: number | null): void {
    const streamTracks = new Map([...this.state.streamTracks].filter(([principal]) => watched.has(principal)));
    this.room?.setWatches(watched, focusedShare);
    this.set({ watched, focusedShare, streamTracks });
  }

  // attachStream plays one watched publisher into its tile, on this connection
  // and every one after it, independently of every other tile (#1509).
  attachStream(principal: number, sink: ShareSink | null): void {
    if (sink === null) this.streamSinks.delete(principal);
    else this.streamSinks.set(principal, sink);
    this.room?.attachStream(principal, sink);
  }

  // presence is each v2 document the voice socket sends: every live sharer,
  // whether this person still holds their own share, and, in this person's own
  // document alone, why the server ended their last share (#1509).
  presence(p: VoicePresenceV2): void {
    const streams = sharers(p);
    const mine = streams.some((s) => s.principal === this.me);
    this.share.presence(mine ? 'me' : streams.length === 0 ? 'none' : 'other', p.share_ended);
    // Why this person's camera claim ended, if it did; and who else holds
    // one, whose cameras alone are received.
    this.camera.presence(p.camera_ended);
    const holders = cameraHolders(p);
    if (this.me !== null) holders.delete(this.me);
    this.cameraClaims = holders;
    this.room?.setCameras(holders);
    this.follow(new Set(streams.map((s) => s.principal)), this.me !== null && occupants(p).some((o) => o.principal === this.me));
  }

  // follow keeps each watch on its publisher's stream, as the old single
  // watch did for the room's one share. A watched stream gone while this
  // person is still listed in the room has ended, now, whether or not its
  // sharer is still listed. One gone with this person (the room was renewed,
  // and everyone left it) is waited for: the same publisher's stream coming
  // back is watched as before. A stream that is back ends the wait for it, and
  // another publisher's never takes its place (voice-multi-share.md §7.1).
  private follow(live: ReadonlySet<number>, listed: boolean): void {
    for (const principal of [...this.state.watched]) {
      if (live.has(principal)) this.endAwaiting(principal);
      else if (this.awaiting.has(principal)) continue;
      else if (listed) this.watchStream(principal, false);
      else this.awaitShare(principal);
    }
  }

  // awaitShare waits for the same publisher's watched share that went with its
  // room; the timer ends that watch alone if it does not return (#1509).
  private awaitShare(principal: number): void {
    if (this.awaiting.has(principal)) return;
    this.awaiting.set(principal, this.env.clock.setTimeout(() => {
      this.awaiting.delete(principal);
      this.watchStream(principal, false);
    }, resumeLimit));
  }

  private endAwaiting(principal?: number): void {
    if (principal === undefined) {
      for (const timer of this.awaiting.values()) this.env.clock.clearTimeout(timer);
      this.awaiting.clear();
      return;
    }
    const timer = this.awaiting.get(principal);
    if (timer !== undefined) this.env.clock.clearTimeout(timer);
    this.awaiting.delete(principal);
  }

  // leave shuts the gate, lets the last report go, and disconnects.
  // removed is conchd saying, on the presence socket, that a moderator put
  // this person out of voice (close 1008 `voice_removed`, issue #1409). They
  // are still a member, so the client stops with why rather than asking for
  // the next session, which conchd would grant: nothing rejoins by itself,
  // and a deliberate join works at once. Out of voice already, nothing to do.
  removed(why: string): void {
    if (this.state.phase === 'idle' || this.state.phase === 'stopped') return;
    this.drop({ kind: 'stop', why }, 'stop');
  }

  async leave(): Promise<void> {
    const mine = ++this.attempt;
    this.clearTimers();
    this.gate.setBlocked(true);
    // Leaving ends the camera first: the capture stops and conchd is told,
    // naming the claim, before anything else. Then the share: it is stopped
    // and conchd is told, while the connection and the credential are still
    // there.
    const camera = this.camera.stop('left');
    const sharing = this.share.stop('left');
    // The release's `stopped` is queued once its mute is done.
    await this.within(this.steps, leaveDrain);
    await this.reports.drain(leaveDrain);
    await sharing;
    await camera;
    if (mine !== this.attempt) return;
    this.talk++;
    this.steps = Promise.resolve();
    this.reports.reset();
    const room = this.room;
    const left = this.channel;
    const viewer = this.state.viewer;
    this.room = null;
    this.channel = null;
    this.endAwaiting();
    this.streamSinks.clear();
    // Nobody's camera is received out of voice; the next join reads presence.
    this.cameraClaims = new Set();
    this.switching = false;
    this.set({
      phase: 'idle',
      channel: null,
      why: null,
      talking: false,
      transmitting: false,
      level: 0,
      speaking: new Set(),
      reportNote: null,
      watched: new Set(),
      focusedShare: null,
      streamTracks: new Map(),
      microphones: null,
      ...nothingInUse,
      missing: false,
      outputs: null,
      outputNote: null,
      viewing: false,
      cameraTracks: noCameraTracks,
      cameras: null,
      cameraTrack: 'none',
      cameraInUse: null,
    });
    if (room !== null) await room.close().catch(() => undefined);
    // The viewer is removed from the room as the page goes; a refusal is not
    // news, since its token lives seconds and the room drops it anyway.
    const remove = this.env.removeViewer;
    if (viewer && left !== null && remove !== undefined) await remove(left).catch(() => undefined);
  }

  setMode(mode: GateMode): void {
    this.gate.setMode(mode);
    this.modes?.set(mode);
    this.set({ mode });
  }

  setMuted(muted: boolean): void {
    this.set({ muted });
    this.applyBlock();
  }

  setDeafened(deafened: boolean): void {
    this.room?.setDeafened(deafened);
    this.set({ deafened });
    this.applyBlock();
  }

  // setVolume sets how loud this page plays one person, 0 to 1, in steps of
  // 5%: on this connection at once, and on every one after it until the page
  // is reloaded.
  setVolume(principal: number, volume: number): void {
    const v = Number.isFinite(volume) ? Math.round(Math.min(1, Math.max(0, volume)) * 20) / 20 : 1;
    const volumes = new Map(this.state.volumes);
    if (v === 1) volumes.delete(principal);
    else volumes.set(principal, v);
    this.room?.setVolume(principal, v);
    this.set({ volumes });
  }

  setThreshold(threshold: number): void {
    this.gate.setThreshold(threshold);
    this.set({ threshold });
  }

  // setReleaseDelay sets how long the gate stays open after the key comes
  // up, from 0 to 2 s: from the next release on.
  setReleaseDelay(ms: number): void {
    this.gate.setTail(ms);
    this.set({ releaseDelay: this.gate.releaseDelay });
  }

  press(): void {
    this.gate.press();
  }

  release(): void {
    this.gate.release();
  }

  // lost is losing focus or visibility. In push to talk it shuts the gate at
  // once, with no release delay (issue #965): the delay can be 2 s, and
  // whoever switched window is not talking to the room any more. A press
  // still waiting for its answer never opens the track. The open microphone
  // goes on, as it does in a tab out of sight.
  lost(): void {
    this.gate.lost();
  }

  dispose(): void {
    void this.leave();
    this.listeners.clear();
  }

  private applyBlock(): void {
    const s = this.state;
    this.gate.setBlocked(s.phase !== 'connected' || s.muted || s.deafened || s.listenOnly || s.watchOnly || s.missing || this.switching);
  }

  private clearTimers(): void {
    for (const t of [this.retryTimer, this.reconnectTimer, this.levelTimer]) {
      if (t !== null) this.env.clock.clearTimeout(t);
    }
    this.retryTimer = null;
    this.reconnectTimer = null;
    this.levelTimer = null;
  }

  // A trace that throws must not change what a press does: it is caught here,
  // and the line is lost.
  private trace(event: string): void {
    try {
      this.env.trace?.(`${String(this.now())} ${event}`);
    } catch {
      // The trace is diagnostic only.
    }
  }

  private onGate(open: boolean): void {
    const talk = this.talk;
    const edge = ++this.edge;
    this.trace(open ? 'gate open' : 'gate shut');
    if (open) {
      this.steps = this.steps.then(() => this.opened(talk, edge));
    } else {
      // The track is muted at once, and `stopped` reported once it is.
      const muted = (this.room?.setSending(false) ?? Promise.resolve()).catch(() => undefined);
      // Observed apart, so shut waits on the mute itself and not one step later.
      void muted.then(() => {
        this.trace('track muted');
      });
      this.steps = this.steps.then(() => this.shut(talk, muted));
    }
    // Transmitting begins once the track is unmuted (opened), and ends here,
    // with the mute.
    this.set(open ? { talking: true } : { talking: false, transmitting: false });
  }

  // opened unmutes the track at once and queues the `started` report to go
  // in parallel (issue #970), unless the gate has changed since or the
  // connection has gone. The report's answer is not waited for: one that fails
  // for good drops the connection, which mutes the track. Once the track is
  // unmuted, if the gate has not changed meanwhile, the page says it is
  // transmitting.
  private async opened(talk: number, edge: number): Promise<void> {
    if (talk !== this.talk) return;
    // Always reported, so that the `stopped` that follows a press is never
    // alone, even for one let go before the track could open.
    this.reports.push('started');
    this.trace('started reported');
    const room = this.room;
    if (edge !== this.edge || room === null) return;
    // Said when the unmute lands, however late: past reportWait the steps go
    // on without it, but the track is open once it does. Not if the gate has
    // shut, or the connection gone, meanwhile.
    await this.within(
      room.setSending(true).then(() => {
        this.trace('track unmuted');
        if (talk === this.talk && edge === this.edge && room === this.room) this.set({ transmitting: true });
      }),
      reportWait,
    );
  }

  private async shut(talk: number, muted: Promise<void>): Promise<void> {
    await this.within(muted, reportWait);
    if (talk === this.talk) {
      this.trace('stopped reported');
      this.reports.push('stopped');
    }
  }

  // within resolves when p settles, however it settles, or after ms.
  private within(p: Promise<unknown>, ms: number): Promise<void> {
    const clock = this.env.clock;
    return new Promise((resolve) => {
      const handle = clock.setTimeout(resolve, ms);
      const done = () => {
        clock.clearTimeout(handle);
        resolve();
      };
      void p.then(done, done);
    });
  }

  private onReport(event: ReportEvent): void {
    switch (event) {
      case 'ok':
        if (this.state.reportNote !== null) this.set({ reportNote: null });
        break;
      case 'retrying':
      case 'rate_limited':
        this.set({ reportNote: event });
        break;
      case 'gave_up':
      case 'no_session':
        // Drop the connection, so nothing is delivered late, and ask conchd.
        // A camera held is claimed again by name on the next connection.
        this.drop({ kind: 'new_session' }, 'keep');
        break;
      case 'unauthorized':
        this.drop({ kind: 'stop', why: 'You are signed out' }, 'stop');
        break;
    }
  }

  // drop ends the current connection and acts on `next`. roomDeleted: LiveKit
  // deleted the room, as conchd does when it renews one. camera is what the
  // end means for the camera (disconnect.ts cameraAfter); a client that stops
  // always stops it.
  private drop(next: Next, camera: CameraLoss, roomDeleted = false): void {
    const mine = ++this.attempt;
    // The camera first, before anything else is done (S5): stopped and
    // released, or taken off this connection for the next.
    this.camera.lost(next.kind === 'stop' && camera !== 'displaced' ? 'stop' : camera);
    this.clearTimers();
    this.gate.setBlocked(true);
    // Nothing the gate asked for on this connection is carried out from here:
    // no track opens, and no `stopped` goes behind the reset.
    this.talk++;
    this.steps = Promise.resolve();
    this.reports.reset();
    // A share does not outlive its connection: the next one starts without it,
    // unless its room was renewed, when the next connection resumes it. A
    // client that stops resumes nothing.
    if (next.kind === 'stop') void this.share.stop('lost');
    else void this.share.lost(roomDeleted);
    // Every watched share goes with a deleted room and is waited for, whether
    // presence has said so yet or not (#1509).
    if (roomDeleted && next.kind !== 'stop') for (const principal of this.state.watched) this.awaitShare(principal);
    const room = this.room;
    this.room = null;
    // A switch under way goes with its connection; its choice is kept.
    this.switching = false;
    this.set({ speaking: new Set(), level: 0, talking: false, transmitting: false, streamTracks: new Map(), cameraTracks: noCameraTracks, ...nothingInUse, ...this.cameraNow() });
    if (room !== null) void room.close().catch(() => undefined);
    this.act(next, mine);
  }

  private act(next: Next, mine: number): void {
    switch (next.kind) {
      case 'stop':
        this.share.abandon();
        this.camera.abandon();
        this.set({ phase: 'stopped', why: next.why, viewing: false, cameraTracks: noCameraTracks });
        break;
      case 'new_session':
        this.set({ phase: 'connecting', why: null });
        void this.connect(mine);
        break;
      case 'retry': {
        const wait = retryDelay(this.failures++, this.env.random);
        this.set({ phase: 'unavailable', why: 'Voice is unavailable; trying again' });
        this.retryTimer = this.env.clock.setTimeout(() => {
          this.retryTimer = null;
          if (mine !== this.attempt) return;
          this.set({ phase: 'connecting' });
          void this.connect(mine);
        }, wait);
        break;
      }
    }
  }

  private async connect(known?: number): Promise<void> {
    const mine = known ?? ++this.attempt;
    const channel = this.channel;
    if (channel === null) return;
    let session = this.state.viewer ? await this.viewerSession(channel) : await this.env.createSession(channel);
    if (mine !== this.attempt) return;
    // Presence of a page that closed without leaving lingers a few seconds and
    // reads as the person's voice in another client (issue #1340). The viewer
    // grant's 409 voice_no_session says no voice of theirs is in the room, so
    // there is nobody to knock out: this is a plain Watch only after all.
    if (this.state.viewer && !session.ok && session.code === 'voice_no_session') {
      this.set({ viewer: false });
      session = await this.env.createSession(channel);
      if (mine !== this.attempt) return;
    }
    if (!session.ok) {
      // conchd refused the next session: a camera held for it stops (S5).
      // An answer that never came is not a refusal, and is retried.
      if (session.failure !== 'network') this.camera.refused();
      this.act(this.state.viewer ? afterViewerRefusal(session) : afterRefusal(session), mine);
      return;
    }
    const grant = session.value.rooms.find((g) => g.audience === undefined);
    if (grant === undefined) {
      this.act({ kind: 'stop', why: 'This channel has no voice room for everyone' }, mine);
      return;
    }
    // The viewer's identity is p<id>.view: the person it stands for is p<id>.
    this.me = principalOf(session.value.identity.replace(/\.view$/, ''));
    const room = this.env.newRoom();
    this.room = room;
    const events: RoomEvents = {
      onReconnecting: () => {
        if (mine !== this.attempt) return;
        this.set({ phase: 'reconnecting' });
        this.applyBlock();
        this.reconnectTimer = this.env.clock.setTimeout(() => {
          this.reconnectTimer = null;
          if (mine !== this.attempt) return;
          // The SDK did not recover in time: close it and let conchd decide.
          this.drop({ kind: 'new_session' }, 'keep');
        }, reconnectLimit);
      },
      onReconnected: () => {
        if (mine !== this.attempt) return;
        if (this.reconnectTimer !== null) this.env.clock.clearTimeout(this.reconnectTimer);
        this.reconnectTimer = null;
        this.set({ phase: 'connected' });
        this.applyBlock();
        void this.share.reconnected();
        void this.camera.reconnected();
      },
      onDisconnected: (cause) => {
        if (mine !== this.attempt) return;
        this.drop(afterDisconnect(cause), cameraAfter(cause), cause === 'room_deleted');
      },
      onSpeakers: (ids) => {
        if (mine !== this.attempt) return;
        this.set({ speaking: new Set(ids) });
      },
      onScreenEnded: (cause) => {
        if (mine !== this.attempt) return;
        this.share.ended(cause);
      },
      // Each stream's arrived tracks are its own tile's: one stream's never
      // stands for another's, and a stream no longer watched has none.
      onStreamTracks: (principal, tracks) => {
        if (mine !== this.attempt) return;
        const streamTracks = new Map(this.state.streamTracks);
        if (this.state.watched.has(principal)) streamTracks.set(principal, tracks);
        else streamTracks.delete(principal);
        this.set({ streamTracks });
      },
      onMicrophoneLost: () => {
        if (mine !== this.attempt) return;
        this.set({ missing: true, ...nothingInUse });
        this.applyBlock();
        void this.readMicrophones(mine, room);
      },
      onDevicesChanged: () => {
        if (mine !== this.attempt) return;
        void this.readMicrophones(mine, room);
        void this.readCameras(mine, room);
      },
      onOutputMoved: (id) => {
        if (mine !== this.attempt) return;
        // Named from the list read before: the output moved to was in it.
        const now = id === '' ? null : (this.state.outputs?.find((o) => o.id === id) ?? { id, label: 'another output' });
        this.outputTurn++;
        this.set({ output: now, outputNote: outputMoved(now) });
      },
      onCameraEnded: (cause) => {
        if (mine !== this.attempt) return;
        this.camera.ended(cause);
        this.set(this.cameraNow());
        void this.readCameras(mine, room);
      },
      onCameraChanged: () => {
        if (mine !== this.attempt) return;
        this.set(this.cameraNow());
      },
      onCameraTracks: (principals) => {
        if (mine !== this.attempt) return;
        this.set({ cameraTracks: new Set(principals) });
      },
    };
    // What a share needs is set before connecting, so that a track that
    // arrives with the connection already finds it.
    room.setWatches(this.state.watched, this.state.focusedShare);
    for (const [principal, sink] of this.streamSinks) room.attachStream(principal, sink);
    // No capture on a new room, so this only records the choice synchronously.
    void room.setShareQuality(this.state.shareQuality).catch(() => undefined);
    // So is what the call view receives, and where it plays.
    room.setCameras(this.cameraClaims);
    room.setViewing(this.state.viewing);
    room.setShown(this.videoShown);
    room.attachSelf(this.selfView);
    for (const [principal, el] of this.cameraViews) room.attachCamera(principal, el);
    // So is each person's volume, so that nobody is heard at full volume
    // first.
    for (const [principal, volume] of this.state.volumes) room.setVolume(principal, volume);
    // So are the speakers chosen, so that no voice plays elsewhere first.
    if (this.state.outputSelection && this.state.output !== null) {
      await this.applyOutput(room);
      if (mine !== this.attempt) return;
    }
    // So is deafen, so that a voice attached while connecting is muted from
    // the first. It is not set again after the connection: a change made
    // meanwhile reaches this room through setDeafened.
    room.setDeafened(this.state.deafened);
    // So is the microphone chosen, found by its name if its id has changed.
    // A microphone missing is not captured at all: the connection is made
    // without it, until the person chooses one.
    const publish = grant.can_publish && !this.state.watchOnly;
    if (publish && !this.state.missing) {
      await this.findChosen(room);
      // Left or dropped meanwhile: whoever did closed this room.
      if (mine !== this.attempt) return;
    }
    const microphone = this.state.microphone?.id ?? null;
    room.setMicrophone(microphone);
    const processing = this.state.processing;
    room.setProcessing(processing);
    try {
      await room.connect(session.value.livekit_url, grant.token, grant.room, publish && !this.state.missing, events);
    } catch (err: unknown) {
      // A microphone refused or missing is said by the error's name alone.
      const name = errorName(err);
      if (typeof name === 'string' && deviceErrors.has(name)) record({ kind: 'device_error', name });
      if (mine !== this.attempt) return;
      // A new attempt number first: closing a room that had joined before the
      // microphone failed fires its Disconnected, which must not act again
      // (it would turn a stop into a retry).
      const next = ++this.attempt;
      void room.close().catch(() => undefined);
      this.room = null;
      // A microphone that cannot be had stops: the next attempt is the
      // person's own Join voice or Watch only. Anything else is retried. The
      // device chosen not being there is said as such, and the choice is let
      // go, so the next Join voice uses the system default. It is the device
      // the room was given that is gone: a newer choice made while
      // connecting is a different device, and is kept (issue #677).
      if (publish && this.state.microphone !== null && typeof name === 'string' && goneErrors.has(name)) {
        if (this.state.microphone.id !== microphone) {
          this.act({ kind: 'stop', why: earlierGone }, next);
          return;
        }
        this.set(this.chosen(null));
        this.act({ kind: 'stop', why: chosenGone }, next);
        return;
      }
      const why = deviceReason(name);
      this.act(why === null ? { kind: 'retry' } : { kind: 'stop', why }, next);
      return;
    }
    if (mine !== this.attempt) {
      void room.close().catch(() => undefined);
      return;
    }
    this.connectedAt = this.now();
    this.set({ phase: 'connected', why: null, listenOnly: !grant.can_publish, ...inUseOn(room) });
    this.applyBlock();
    // The microphone chosen, or its voice processing, changed while
    // connecting: the capture may have been made as it was, so it is made
    // again as it is now (issue #669 for the device).
    const now = this.state.microphone?.id ?? null;
    if (publish && !this.state.missing && (now !== microphone || this.state.processing !== processing)) void this.switchTo(room, now);
    void this.readMicrophones(mine, room);
    void this.readCameras(mine, room);
    this.watchLevel(mine, room);
    this.share.connected(channel, room);
    this.camera.connected(channel, room);
    // A connection that lasts starts the retry wait over.
    this.env.clock.setTimeout(() => {
      if (mine === this.attempt && this.now() - this.connectedAt >= healthyAfter) this.failures = 0;
    }, healthyAfter);
  }

  // viewerSession asks for the viewer grant and shapes it as a session, so
  // that connecting is one path: the same room, a token that publishes
  // nothing. The page never asks for the person's own session on this path.
  private async viewerSession(channel: string): Promise<VoiceSessionResult> {
    const ask = this.env.createViewer;
    if (ask === undefined) return { ok: false, failure: 'invalid' };
    const viewer = await ask(channel);
    if (!viewer.ok) return viewer;
    const v = viewer.value;
    return { ok: true, value: { livekit_url: v.livekit_url, identity: v.identity, rooms: [{ room: v.room, token: v.token, can_publish: false, expires_at: v.expires_at }] } };
  }

  private watchLevel(mine: number, room: Room): void {
    let reads = 0;
    const tick = () => {
      if (mine !== this.attempt) return;
      const raw = room.level();
      this.gate.level(raw);
      // Quantised, and told every meterEvery ms, so a meter does not
      // re-render the page at every read.
      if (++reads * levelEvery >= meterEvery) {
        reads = 0;
        const level = Math.round(raw * 20) / 20;
        if (level !== this.state.level) this.set({ level });
      }
      this.levelTimer = this.env.clock.setTimeout(tick, levelEvery);
    };
    this.levelTimer = this.env.clock.setTimeout(tick, levelEvery);
  }
}
