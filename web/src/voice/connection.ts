// The voice client: asks conchd for a session before every connection attempt,
// joins the room, owns the transmit gate and the report queue, and follows the
// disconnect table of conch-voice.md section 5. It knows the SDK only through
// the Room interface below (sdk.ts is the one file that imports it), so the
// tests drive it with a stub.
//
// The join token is passed to Room.connect and dropped. It is never stored,
// logged or put in the DOM.
//
// A press reports `started` and waits for conchd's answer before the track
// opens, and a release mutes the track before it reports `stopped` (issue
// #495). conchd's poller compares what it sees of the microphone with the
// reports; the other order let it see a press before its report, and record an
// honest press as unreported. The wait is bounded (reportWait): past it, or if
// the report is still being retried, the track opens anyway, so audio never
// depends on conchd being reachable (conch-voice.md section 6, "Client side").
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
// (share.ts), whether this page watches someone else's share, and the
// elements a watched share plays into.
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
import type { ShareClaimResult, TransmitAnswer, VoiceSessionResult } from '../api';
import type { VoicePresenceV1, VoiceTransmitState } from '../schema/voice';
import type { Clock } from '../state/feed';
import { afterDisconnect, afterRefusal, retryDelay, type DisconnectCause, type Next } from './disconnect';
import { clampTail, defaultTail, TransmitGate, type GateMode, type GateOptions } from './gate';
import { occupants, principalOf, sharer } from './presence';
import { ReportQueue, type ReportEvent } from './reports';
import { idleShare, resumeLimit, ScreenShare, type ScreenEnd, type ShareRoom, type ShareState } from './share';
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
  // Which of a watched share's tracks have arrived.
  readonly onShareTracks: (tracks: ShareTracks) => void;
  // The microphone was lost: its capture ended, or the device chosen is no
  // longer listed. The room has stopped the capture.
  readonly onMicrophoneLost: () => void;
  // The browser's devices changed: microphones came or went.
  readonly onDevicesChanged: () => void;
  // The output in use went, and the SDK moved every voice to this one.
  readonly onOutputMoved: (id: string) => void;
}

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
export interface Room extends ShareRoom {
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
  // Subscribes to a share's picture and sound, or lets them go. Microphones
  // are always subscribed.
  setWatching: (on: boolean) => void;
  // Plays a watched share into these elements, or into none.
  attachShare: (sink: ShareSink | null) => void;
  // Opens or closes the track: unmute or mute. Muted again after any republish.
  setSending: (on: boolean) => Promise<void>;
  setDeafened: (on: boolean) => void;
  // How loud this page plays one person's microphone, by principal, 0 to 1:
  // now and whenever their microphone arrives later on this connection.
  // Deafen silences everyone whatever their volume.
  setVolume: (principal: number, volume: number) => void;
  // The microphone level, 0 to 1.
  level: () => number;
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
  // Whether this page watches the room's share, and which of its tracks
  // have arrived.
  readonly watching: boolean;
  readonly shareTracks: ShareTracks;
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
}

export interface VoiceEnv {
  readonly clock: Clock;
  readonly random: () => number;
  readonly createSession: (channel: string) => Promise<VoiceSessionResult>;
  readonly report: (channel: string, state: VoiceTransmitState, signal: AbortSignal) => Promise<TransmitAnswer>;
  readonly newRoom: () => Room;
  readonly claimShare: (channel: string) => Promise<ShareClaimResult>;
  // Releases the share whose claim started at startedAt (issue #487).
  readonly stopShare: (channel: string, startedAt: string) => Promise<unknown>;
  readonly gate?: GateOptions;
  // The microphone chosen earlier in this tab, if any (issue #658).
  readonly microphone?: Microphone | null;
  // The voice processing chosen for each microphone (issue #659); without
  // it, the choices last as long as the client.
  readonly processing?: ProcessingStore;
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
// The longest a press waits for the answer to its `started` report before the
// track opens anyway, and the longest the release of a press waits for the
// track to mute before `stopped` is reported.
export const reportWait = 1000;

export const initialVoice: VoiceState = {
  phase: 'idle',
  channel: null,
  why: null,
  mode: 'ptt',
  muted: false,
  deafened: false,
  listenOnly: false,
  watchOnly: false,
  talking: false,
  transmitting: false,
  releaseDelay: defaultTail,
  level: 0,
  threshold: 0.1,
  speaking: new Set(),
  reportNote: null,
  share: idleShare,
  watching: false,
  shareTracks: { video: false, audio: false },
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
};

const noTracks: ShareTracks = { video: false, audio: false };

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
  // This person, from the identity of the last session.
  private me: number | null = null;
  private sink: ShareSink | null = null;
  // Who presence last showed sharing, and whose share this page watches.
  // awaiting is the timer of a watched share that went with its room: the
  // watching ends if the share is not back by then.
  private holder: number | null = null;
  private watched: number | null = null;
  private awaiting: unknown = null;
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
  // Numbers the changes of output: only the last one's refusal counts.
  private outputTurn = 0;

  constructor(env: VoiceEnv, now: () => number = () => Date.now()) {
    this.env = env;
    this.now = now;
    this.processing = env.processing ?? memoryStore();
    this.state = {
      ...initialVoice,
      ...this.chosen(env.microphone ?? null),
      outputSelection: env.outputSelection === true,
      output: env.output ?? null,
      releaseDelay: clampTail(env.gate?.tail ?? defaultTail),
    };
    this.gate = new TransmitGate(env.clock, (open) => {
      this.onGate(open);
    }, env.gate);
    this.share = new ScreenShare(
      { claim: env.claimShare, release: env.stopShare },
      (share) => {
        this.set({ share });
      },
      env.clock,
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
  join(channel: string, options: { readonly watchOnly?: boolean } = {}): void {
    if (this.state.phase !== 'idle' && this.state.phase !== 'stopped') return;
    this.channel = channel;
    this.failures = 0;
    // Joining is the person's own choice: a microphone missing before is
    // asked for again, as chosen.
    this.set({ phase: 'connecting', channel, why: null, reportNote: null, watchOnly: options.watchOnly === true, watching: false, shareTracks: noTracks, missing: false });
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
    if (this.state.channel === from) this.set({ channel: to });
  }

  // useMicrophone ends watch only: the microphone is captured, published and
  // muted, as on a full join, and the talk controls work from then on.
  async useMicrophone(): Promise<void> {
    const room = this.room;
    if (!this.state.watchOnly || this.state.listenOnly || this.state.phase !== 'connected' || room === null) return;
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
    void this.share.start(channel, room);
  }

  stopShare(): void {
    void this.share.stop('self');
  }

  // watch subscribes to the room's share, or lets it go.
  watch(on: boolean): void {
    this.endAwaiting();
    this.watched = on ? this.holder : null;
    this.room?.setWatching(on);
    this.set(on ? { watching: true } : { watching: false, shareTracks: noTracks });
  }

  // attachShare plays a watched share into the view's elements, on this
  // connection and on every one after it.
  attachShare(sink: ShareSink | null): void {
    this.sink = sink;
    this.room?.attachShare(sink);
  }

  // presence is each presence document the voice socket sends: whether this
  // person still holds the share, whether anyone does, and, in this person's
  // own document alone, why the server ended their last share.
  presence(p: VoicePresenceV1): void {
    const holder = sharer(p);
    this.share.presence(holder === null ? 'none' : holder.principal === this.me ? 'me' : 'other', p.share_ended);
    this.holder = holder?.principal ?? null;
    if (this.state.watching) this.follow(holder?.principal ?? null, occupants(p).some((o) => o.principal === this.me));
  }

  // follow keeps a watching page on the share it watches. A share gone while
  // this person is still listed in the room has ended. A share gone with
  // this person (the room was renewed, and everyone left it) is waited for:
  // the same person's share coming back is watched as before, anyone else's
  // is not.
  private follow(holder: number | null, listed: boolean): void {
    if (holder !== null) {
      const waited = this.awaiting !== null;
      this.endAwaiting();
      if (waited && holder !== this.watched) this.watch(false);
      else this.watched = holder;
      return;
    }
    if (this.awaiting !== null) return;
    if (listed || this.watched === null) {
      this.watch(false);
      return;
    }
    this.awaitShare();
  }

  // awaitShare waits for a watched share that went with its room.
  private awaitShare(): void {
    if (this.awaiting !== null) return;
    this.awaiting = this.env.clock.setTimeout(() => {
      this.awaiting = null;
      this.watch(false);
    }, resumeLimit);
  }

  private endAwaiting(): void {
    if (this.awaiting !== null) this.env.clock.clearTimeout(this.awaiting);
    this.awaiting = null;
  }

  // leave shuts the gate, lets the last report go, and disconnects.
  async leave(): Promise<void> {
    const mine = ++this.attempt;
    this.clearTimers();
    this.gate.setBlocked(true);
    // Leaving ends a share: it is stopped and conchd is told, while the
    // connection and the credential are still there.
    const sharing = this.share.stop('left');
    // The release's `stopped` is queued once its mute is done.
    await this.within(this.steps, leaveDrain);
    await this.reports.drain(leaveDrain);
    await sharing;
    if (mine !== this.attempt) return;
    this.talk++;
    this.steps = Promise.resolve();
    this.reports.reset();
    const room = this.room;
    this.room = null;
    this.channel = null;
    this.endAwaiting();
    this.watched = null;
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
      watching: false,
      shareTracks: noTracks,
      microphones: null,
      ...nothingInUse,
      missing: false,
      outputs: null,
      outputNote: null,
    });
    if (room !== null) await room.close().catch(() => undefined);
  }

  setMode(mode: GateMode): void {
    this.gate.setMode(mode);
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

  private onGate(open: boolean): void {
    const talk = this.talk;
    const edge = ++this.edge;
    if (open) {
      this.steps = this.steps.then(() => this.opened(talk, edge));
    } else {
      // The track is muted at once, and `stopped` reported once it is.
      const muted = (this.room?.setSending(false) ?? Promise.resolve()).catch(() => undefined);
      this.steps = this.steps.then(() => this.shut(talk, muted));
    }
    // Transmitting begins once the track is unmuted (opened), and ends here,
    // with the mute.
    this.set(open ? { talking: true } : { talking: false, transmitting: false });
  }

  // opened reports `started` and opens the track once conchd has answered,
  // or after reportWait, unless the gate has changed since or the connection
  // has gone. A report that fails for good drops the connection first. Once
  // the track is unmuted, if the gate has not changed meanwhile, the page
  // says it is transmitting.
  private async opened(talk: number, edge: number): Promise<void> {
    if (talk !== this.talk) return;
    const answered = new Promise<void>((resolve) => {
      this.reports.push('started', resolve);
    });
    await this.within(answered, reportWait);
    const room = this.room;
    if (talk !== this.talk || edge !== this.edge || room === null) return;
    // Said when the unmute lands, however late: past reportWait the steps go
    // on without it, but the track is open once it does. Not if the gate has
    // shut, or the connection gone, meanwhile.
    await this.within(
      room.setSending(true).then(() => {
        if (talk === this.talk && edge === this.edge && room === this.room) this.set({ transmitting: true });
      }),
      reportWait,
    );
  }

  private async shut(talk: number, muted: Promise<void>): Promise<void> {
    await this.within(muted, reportWait);
    if (talk === this.talk) this.reports.push('stopped');
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
        this.drop({ kind: 'new_session' });
        break;
      case 'unauthorized':
        this.drop({ kind: 'stop', why: 'You are signed out' });
        break;
    }
  }

  // drop ends the current connection and acts on `next`. roomDeleted: LiveKit
  // deleted the room, as conchd does when it renews one.
  private drop(next: Next, roomDeleted = false): void {
    const mine = ++this.attempt;
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
    // A watched share goes with a deleted room, and is waited for, whether
    // presence has said so yet or not.
    if (roomDeleted && next.kind !== 'stop' && this.state.watching && this.watched !== null) this.awaitShare();
    const room = this.room;
    this.room = null;
    // A switch under way goes with its connection; its choice is kept.
    this.switching = false;
    this.set({ speaking: new Set(), level: 0, talking: false, transmitting: false, shareTracks: noTracks, ...nothingInUse });
    if (room !== null) void room.close().catch(() => undefined);
    this.act(next, mine);
  }

  private act(next: Next, mine: number): void {
    switch (next.kind) {
      case 'stop':
        this.share.abandon();
        this.set({ phase: 'stopped', why: next.why });
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
    const session = await this.env.createSession(channel);
    if (mine !== this.attempt) return;
    if (!session.ok) {
      this.act(afterRefusal(session), mine);
      return;
    }
    const grant = session.value.rooms.find((g) => g.audience === undefined);
    if (grant === undefined) {
      this.act({ kind: 'stop', why: 'This channel has no voice room for everyone' }, mine);
      return;
    }
    this.me = principalOf(session.value.identity);
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
          this.drop({ kind: 'new_session' });
        }, reconnectLimit);
      },
      onReconnected: () => {
        if (mine !== this.attempt) return;
        if (this.reconnectTimer !== null) this.env.clock.clearTimeout(this.reconnectTimer);
        this.reconnectTimer = null;
        this.set({ phase: 'connected' });
        this.applyBlock();
        void this.share.reconnected();
      },
      onDisconnected: (cause) => {
        if (mine !== this.attempt) return;
        this.drop(afterDisconnect(cause), cause === 'room_deleted');
      },
      onSpeakers: (ids) => {
        if (mine !== this.attempt) return;
        this.set({ speaking: new Set(ids) });
      },
      onScreenEnded: (cause) => {
        if (mine !== this.attempt) return;
        this.share.ended(cause);
      },
      onShareTracks: (tracks) => {
        if (mine !== this.attempt) return;
        this.set({ shareTracks: tracks });
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
      },
      onOutputMoved: (id) => {
        if (mine !== this.attempt) return;
        // Named from the list read before: the output moved to was in it.
        const now = id === '' ? null : (this.state.outputs?.find((o) => o.id === id) ?? { id, label: 'another output' });
        this.outputTurn++;
        this.set({ output: now, outputNote: outputMoved(now) });
      },
    };
    // What a share needs is set before connecting, so that a track that
    // arrives with the connection already finds it.
    room.setWatching(this.state.watching);
    room.attachShare(this.sink);
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
    this.watchLevel(mine, room);
    this.share.connected(channel, room);
    // A connection that lasts starts the retry wait over.
    this.env.clock.setTimeout(() => {
      if (mine === this.attempt && this.now() - this.connectedAt >= healthyAfter) this.failures = 0;
    }, healthyAfter);
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
