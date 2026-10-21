// The one file that imports the LiveKit SDK. It adapts the SDK's Room to the
// small Room interface of connection.ts, and quiets the SDK's logging.
//
// The microphone is captured here with echo cancellation, noise suppression and
// auto gain on, unless the person has turned voice processing off for it
// (issue #659), muted, published once, and muted again straight after
// publishing, as conch-voice.md section 3 does: the SDK enables a track when it
// publishes it, and again when it republishes after a long interruption, so the
// track is muted after each unless the gate is open. The client counts as
// connected only after that mute. A remote unmute (LiveKit's room API can ask
// for one) is undone the same way while the gate is shut. The mute before
// publishing is so that LiveKit never lists the microphone unmuted at a join
// (issue #501; publishMicrophone).
//
// The microphone is the person's choice (issue #658): a device by id,
// captured exactly or not at all, or the system default, which follows the
// computer's sound settings. The capture is published as a track the page
// provides, so the SDK never captures a device by itself: not the first
// device listed after a device change, not the default when a capture ends
// while the gate is open, and not on the next unmute after an end. Every
// change of device is the page's own restart of the track, which keeps the
// publication and its mute. A capture that ends, or a chosen device that is
// no longer listed, is lost: its capture is stopped and the client is told,
// and nothing is captured again until the person chooses.
//
// The speakers are the person's choice too (issue #660), where the browser
// lets a page choose them: every voice, and a watched share's sound, plays
// on the output chosen, or on the system default (the empty id). The SDK
// keeps the choice for tracks and people that come later. When the output
// in use goes, the SDK moves sound to another one by itself, and the client
// is told where; on the system default it is put back on the system default.
//
// Each other person has a volume of their own on this page (issue #661), 0
// to 1, kept by principal. It is set here on the element that plays their
// microphone, when it is attached and at every change: the SDK keeps a volume
// for an element attached later only when it is not 0, so a person turned
// right down would come back at full volume. Deafen is the element's mute,
// which is apart from its volume. A share's sound has a volume of its own,
// in the watch view, and is never touched here.
//
// Leave can come at any point of a connection. Once the adapter is closed,
// every step that resumes after an await stops, and a microphone or screen
// captured or published after the close is stopped, so nothing keeps a device
// open.
//
// Screen share (issue #444). A share is captured with the SDK's own picker
// call, at up to 3840x2160 and 60 fps, and published as the screen-share
// sources: the picture as captured, with simulcast layers of 1080p at 30 fps
// and 720p at 15 fps where the capture is larger (issue #565), its sound as
// music. Tracks are subscribed by choice, not automatically: every
// microphone, and a share's picture and sound only while this page watches
// it. Adaptive stream asks LiveKit for the layer the watch view's size needs
// (none while it is hidden), and dynacast lets a sharer stop encoding a layer
// nobody receives.
//
// A share outlives a renewed room (issue #516): the capture is taken off the
// old connection without being stopped (keepScreen) and published on the next
// (adoptScreen), with the same options. The SDK would stop every local track
// when the server ends a connection (a deleted room does), so it is told not
// to stop a track it unpublishes: this adapter stops every track itself, on
// stopScreen and on close.
//
// A share is published only once LiveKit has told this connection that the
// claim granted its sources (issue #525): conchd's answer to the claim can be
// read first, and the SDK refuses a source its last permission does not name.
//
// A capture kept while its publish is under way (a room renewed mid-publish,
// issue #535) is blanked at once: its tracks are disabled, so the old room is
// sent black frames and silence until the capture is adopted by the next
// connection. A track whose publish was under way is taken down when that
// publish completes, and the share's other track is not published at all.
import {
  AudioPresets,
  DisconnectReason,
  LocalAudioTrack,
  LogLevel,
  LoggerNames,
  PublishTrackError,
  Room as LiveKitRoom,
  RoomEvent,
  Track,
  TrackEvent,
  VideoPreset,
  createLocalAudioTrack,
  getLogger,
  supportsAudioOutputSelection,
  type AudioCaptureOptions,
  type LocalTrack,
  type LocalTrackPublication,
  type Participant,
  type RemoteParticipant,
  type RemoteTrack,
  type RemoteTrackPublication,
  type RoomConnectOptions,
  type RoomOptions,
  type ScreenShareCaptureOptions,
  type TrackPublication,
  type TrackPublishOptions,
} from 'livekit-client';
import type { DisconnectCause } from './disconnect';
import type { Microphone, Output, Processing, Room, RoomEvents, ShareSink } from './connection';
import { principalOf } from './presence';
import { LogRing, Redactor } from './redact';
import type { Captured, KeptScreen } from './share';

export { principalOf };

// One redactor and one ring for the page: the SDK's loggers are global.
export const redactor = new Redactor();
export const sdkLog = new LogRing(redactor);

let logging = false;

// quietSdk sets every SDK logger to warn and sends its records, redacted, to
// the ring instead of the browser log. It runs when this module loads, so the
// SDK says nothing to the console before the first connection either.
//
// The level is not persisted: setLevel is called with persist=false, and the
// SDK's setLogLevel is not called at all, since it persists the level through
// loglevel to localStorage, or to a cookie where that is unavailable.
export function quietSdk(): void {
  if (logging) return;
  logging = true;
  const names = Object.values(LoggerNames) as string[];
  for (const name of names) {
    const logger = getLogger(name);
    logger.methodFactory = (method) => (message: unknown, context?: unknown) => {
      sdkLog.add(method, typeof message === 'string' ? message : '', context);
    };
    logger.setLevel(LogLevel.warn, false);
  }
}

quietSdk();

// How the page joins a room: adaptive stream and dynacast on, nothing
// subscribed but by choice (wanted), and no track stopped by the SDK when it
// unpublishes one, so that a share's capture survives its room.
export const roomOptions: RoomOptions = { adaptiveStream: true, dynacast: true, stopLocalTrackOnUnpublish: false };
export const connectOptions: RoomConnectOptions = { autoSubscribe: false };

// wanted is the subscription choice for a remote track: a microphone always,
// a share's picture and sound only while the share is watched, nothing else.
export function wanted(source: Track.Source, watching: boolean): boolean {
  if (source === Track.Source.Microphone) return true;
  if (source === Track.Source.ScreenShare || source === Track.Source.ScreenShareAudio) return watching;
  return false;
}

// ---- the microphone

// microphoneOptions is what a capture of the microphone asks for: the chosen
// device exactly, as an object, which the SDK leaves alone (a bare id it
// tries as exact and then again as ideal, which captures some other device);
// or, with none chosen, the system default as before. The browser's three
// voice filters are all on or all off (issue #659). A restart replaces every
// constraint with these, so each field is given every time; the SDK gives a
// restart without a device its own default-device preference, so the filters
// are asked for on the system default too.
//
// With processing off, voice isolation is asked off as well: the SDK asks for
// it on at a first capture unless told otherwise, and a filter left on is not
// a microphone sent as it is.
export function microphoneOptions(choice: string | null, processing = true): AudioCaptureOptions {
  const filters: AudioCaptureOptions = processing
    ? { echoCancellation: true, noiseSuppression: true, autoGainControl: true }
    : { echoCancellation: false, noiseSuppression: false, autoGainControl: false, voiceIsolation: false };
  return choice === null ? filters : { deviceId: { exact: choice }, ...filters };
}

// filterOf reads one filter from a capture's settings: on, off, or null where
// the browser does not say. Echo cancellation may be named by its mode
// ("all", "remote-only" or "none") rather than as a flag.
function filterOf(value: unknown): boolean | null {
  if (typeof value === 'boolean') return value;
  if (typeof value === 'string') return value !== 'none';
  return null;
}

// processingOf is what a capture says of its voice processing.
export function processingOf(settings: MediaTrackSettings): Processing {
  return {
    echoCancellation: filterOf(settings.echoCancellation),
    noiseSuppression: filterOf(settings.noiseSuppression),
    autoGainControl: filterOf(settings.autoGainControl),
  };
}

// captureMicrophone captures the microphone and wraps the capture as a track
// the page provides. The SDK manages a track it captured itself: it switches
// it to another device after a device change, restarts it on the default
// device when it ends while unmuted, and captures it again at the next unmute
// after an end. It does none of these to a track the page provides.
export async function captureMicrophone(choice: string | null, processing = true): Promise<LocalAudioTrack> {
  const captured = await createLocalAudioTrack(microphoneOptions(choice, processing));
  const track = new LocalAudioTrack(captured.mediaStreamTrack, captured.constraints, true);
  track.source = Track.Source.Microphone;
  return track;
}

// ---- the share's sizes and bitrates
//
// Every size and bitrate a share uses is here (issue #565, which replaced
// decision O6's 1080p30 with 720p15 and 360p5). They are where it starts:
// the bandwidth report of #448, which measures a 4K60 share too, refines them.

export interface ShareLayer {
  readonly width: number;
  readonly height: number;
  readonly maxBitrate: number;
  readonly maxFramerate: number;
}

// The captured picture's size and frame rate, as its track's settings say.
export interface CapturedVideo {
  readonly width?: number;
  readonly height?: number;
  readonly frameRate?: number;
}

// The picker is asked for up to 3840x2160 at up to 60 fps. The SDK asks the
// browser for that as an ideal (a maximum on Safari), never as an exact size
// or a minimum, so a source is captured at its own size, scaled down to fit
// but never up.
const shareAsk = { width: 3840, height: 2160, frameRate: 60 };

// The top layer is the picture as captured, at the capture's own frame rate
// up to the ask's, with a bitrate chosen by the capture's longer side: the
// first row whose longest it does not exceed, and above them all shareTopMost.
const shareTops: readonly { readonly longest: number; readonly maxBitrate: number }[] = [
  { longest: 1920, maxBitrate: 6_000_000 },
  { longest: 2560, maxBitrate: 10_000_000 },
];
const shareTopMost = 18_000_000;

// The lower layers, at most two (all LiveKit takes besides the top): 1080p,
// which most desktop viewers will take, and 720p for phones and slow links.
// Each is the capture scaled down to fit its box, keeping the capture's
// shape, with the box stood on end for a portrait capture. A layer that
// would not be smaller than the capture is left out: a 1080p capture has a
// 720p layer only, and a 720p one none.
const shareLower: readonly ShareLayer[] = [
  { width: 1280, height: 720, maxBitrate: 1_500_000, maxFramerate: 15 },
  { width: 1920, height: 1080, maxBitrate: 4_000_000, maxFramerate: 30 },
];

// What a capture is taken to be when the browser does not say its size.
const shareUnknown = { width: 1920, height: 1080 };

const known = (n: number | undefined): n is number => n !== undefined && Number.isFinite(n) && n > 0;

// shareEncodings chooses a share's encodings from what was captured: the top
// layer, and the lower layers smallest first.
export function shareEncodings(captured: CapturedVideo): { readonly top: ShareLayer; readonly lower: readonly ShareLayer[] } {
  const { width: w, height: h, frameRate: r } = captured;
  const [width, height] = known(w) && known(h) ? [w, h] : [shareUnknown.width, shareUnknown.height];
  const fps = known(r) ? Math.min(r, shareAsk.frameRate) : shareAsk.frameRate;
  const long = Math.max(width, height);
  const short = Math.min(width, height);
  const portrait = height > width;

  let maxBitrate = shareTopMost;
  for (const row of shareTops) {
    if (long <= row.longest) {
      maxBitrate = row.maxBitrate;
      break;
    }
  }

  const lower: ShareLayer[] = [];
  for (const layer of shareLower) {
    const boxLong = Math.max(layer.width, layer.height);
    const boxShort = Math.min(layer.width, layer.height);
    if (long <= boxLong && short <= boxShort) continue;
    // Scaled by the side that fits the box more tightly; the other follows
    // the capture's shape, rounded down so the layer stays in the box.
    const [l, s] = long * boxShort >= short * boxLong ? [boxLong, Math.floor((short * boxLong) / long)] : [Math.floor((long * boxShort) / short), boxShort];
    lower.push({ width: portrait ? s : l, height: portrait ? l : s, maxBitrate: layer.maxBitrate, maxFramerate: Math.min(layer.maxFramerate, fps) });
  }
  return { top: { width, height, maxBitrate, maxFramerate: fps }, lower };
}

// What the picker asks for: the picture at up to 3840x2160 and 60 fps, the
// sound (a tab's, or the whole system's where the browser offers it) without
// the voice processing a microphone gets, the Conch tab itself left out of
// the choices, and the shared tab still heard by the sharer. The system's
// sound leaves out what this page plays (restrictOwnAudio), so the room's
// voices are not sent back into the room. Every share is captured as motion
// (issue #554): shares are mostly films and games watched together, and motion
// keeps the frame rate when bandwidth runs short, giving up text sharpness
// only then.
export function captureOptions(): ScreenShareCaptureOptions {
  return {
    video: true,
    audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
    resolution: { ...shareAsk },
    contentHint: 'motion',
    systemAudio: 'include',
    selfBrowserSurface: 'exclude',
    surfaceSwitching: 'include',
    suppressLocalAudioPlayback: false,
  };
}

// How the picture is published, from what was captured (shareEncodings).
// The codec is the SDK's default, VP8.
export function videoPublishOptions(captured: CapturedVideo): TrackPublishOptions {
  const { top, lower } = shareEncodings(captured);
  return {
    source: Track.Source.ScreenShare,
    simulcast: true,
    screenShareEncoding: new VideoPreset(top.width, top.height, top.maxBitrate, top.maxFramerate).encoding,
    screenShareSimulcastLayers: lower.map((l) => new VideoPreset(l.width, l.height, l.maxBitrate, l.maxFramerate)),
  };
}

// The share's sound is music, not speech: no discontinuous transmission or
// redundancy, and stereo when the capture has two channels.
export function audioPublishOptions(stereo: boolean): TrackPublishOptions {
  return {
    source: Track.Source.ScreenShareAudio,
    audioPreset: stereo ? AudioPresets.musicHighQualityStereo : AudioPresets.musicHighQuality,
    dtx: false,
    red: false,
    forceStereo: stereo,
  };
}

// How long a share's publish waits for LiveKit to tell this connection that
// the claim granted the screen-share sources (issue #525). Within conchd's
// wait for a share's first track (15 s), past which the claim ends anyway.
export const grantWait = 10_000;

// within reports whether done settles before ms have passed.
function within(done: Promise<void>, ms: number): Promise<boolean> {
  let timer = 0;
  const late = new Promise<boolean>((resolve) => {
    timer = window.setTimeout(() => {
      resolve(false);
    }, ms);
  });
  return Promise.race([done.then(() => true), late]).finally(() => {
    window.clearTimeout(timer);
  });
}

// Thrown by a connection step that resumes after the adapter was closed.
class ClosedError extends Error {
  constructor() {
    super('voice connection closed');
  }
}

// Thrown by a share's publish whose capture was stopped, or kept for another
// connection, while it was under way.
class TakenError extends Error {
  constructor() {
    super('voice share taken off this connection');
  }
}

function causeOf(reason: DisconnectReason | undefined): DisconnectCause {
  switch (reason) {
    case DisconnectReason.ROOM_DELETED:
      return 'room_deleted';
    case DisconnectReason.PARTICIPANT_REMOVED:
      return 'participant_removed';
    case DisconnectReason.DUPLICATE_IDENTITY:
      return 'duplicate_identity';
    case DisconnectReason.SERVER_SHUTDOWN:
      return 'server_shutdown';
    default:
      return 'lost';
  }
}

const isShare = (source: Track.Source) => source === Track.Source.ScreenShare || source === Track.Source.ScreenShareAudio;

// A share's capture between two connections: still capturing, published on
// neither.
class Kept implements KeptScreen {
  readonly tracks: readonly LocalTrack[];
  readonly audio: boolean;
  onEnded: (() => void) | null = null;

  constructor(tracks: readonly LocalTrack[]) {
    this.tracks = tracks;
    this.audio = tracks.some((t) => t.source === Track.Source.ScreenShareAudio);
    const video = tracks.find((t) => t.source === Track.Source.ScreenShare);
    video?.mediaStreamTrack.addEventListener('ended', () => {
      const ended = this.onEnded;
      this.onEnded = null;
      ended?.();
    });
  }

  live = (): boolean => this.tracks.length > 0 && this.tracks.every((t) => t.mediaStreamTrack.readyState === 'live');

  stop = (): void => {
    for (const t of this.tracks) t.stop();
  };
}

class LiveKitAdapter implements Room {
  private readonly room = new LiveKitRoom(roomOptions);
  // Set by close(); every step after an await checks it.
  private closed = false;
  private events: RoomEvents | null = null;
  private track: LocalAudioTrack | null = null;
  private publication: LocalTrackPublication | null = null;
  private tap: { readonly context: AudioContext; readonly analyser: AnalyserNode; readonly clone: MediaStreamTrack } | null = null;
  private wantSending = false;
  // The microphone chosen, by device id, or null for the system default; and
  // whether the capture was lost, when nothing captures until a new choice.
  private choice: string | null = null;
  private lost = false;
  // Whether the browser's voice processing is asked for (issue #659).
  private processing = true;
  // The first capture and publish of the microphone while it is under way,
  // and the switches asked for: one capture at a time (issue #669). A second
  // publish joins the one under way, and a switch waits for it and then
  // moves it, the last switch alone.
  private publishing: Promise<void> | null = null;
  private switches = 0;
  private deafened = false;
  // The output every voice plays on, by id: the empty id is the system
  // default (issue #660).
  private output = '';
  // The elements playing each microphone, by track sid, and whose it is; and
  // the volume of each person not at full volume, by principal (issue #661).
  private readonly elements = new Map<string, { readonly el: HTMLMediaElement; readonly principal: number | null }>();
  private readonly volumes = new Map<number, number>();
  private readonly buffer = new Uint8Array(1024);
  // The share this page captured, until it is stopped; and whether this
  // adapter is the one taking it down, so its own unpublish is not reported.
  private screen: LocalTrack[] = [];
  private stoppingScreen = false;
  // Called at the next change of this connection's own permission, and when
  // the share is stopped, kept or closed: where a share's publish refused for
  // want of the claim's grant waits (publishGranted).
  private grantWaiters: (() => void)[] = [];
  // The browser ended the capture (its own stop control, or the shared tab
  // or window closed).
  private captureEnded = false;
  // The share this page watches: whether it does, where it plays, and the
  // tracks that have arrived.
  private watching = false;
  private sink: ShareSink | null = null;
  private shareVideo: RemoteTrack | null = null;
  private shareAudio: RemoteTrack | null = null;

  async connect(url: string, token: string, roomName: string, publish: boolean, events: RoomEvents): Promise<void> {
    this.ensureOpen();
    this.events = events;
    // Held for this adapter alone: its close() releases these and no other
    // connection's.
    redactor.hold(this, token, roomName);
    const room = this.room;
    room
      .on(RoomEvent.Reconnecting, events.onReconnecting)
      .on(RoomEvent.Reconnected, () => {
        // Subscriptions are asked for again on a connection made anew.
        this.subscribeAll();
        events.onReconnected();
      })
      .on(RoomEvent.Disconnected, (reason?: DisconnectReason) => {
        events.onDisconnected(causeOf(reason));
      })
      .on(RoomEvent.ActiveSpeakersChanged, (speakers: Participant[]) => {
        const ids = speakers.map((p) => principalOf(p.identity)).filter((n): n is number => n !== null);
        events.onSpeakers(ids);
      })
      .on(RoomEvent.TrackPublished, (pub: RemoteTrackPublication) => {
        this.subscribe(pub);
      })
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, _pub: RemoteTrackPublication, participant: RemoteParticipant) => {
        // Nothing is played, or left in the page, after a close.
        if (this.closed) return;
        if (isShare(track.source)) {
          this.shared(track);
          return;
        }
        if (track.kind !== Track.Kind.Audio) return;
        const el = track.attach();
        const principal = principalOf(participant.identity);
        el.muted = this.deafened;
        el.volume = this.volumeOf(principal);
        this.elements.set(track.sid ?? '', { el, principal });
        document.body.appendChild(el);
      })
      .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack) => {
        if (isShare(track.source)) {
          this.unshared(track);
          return;
        }
        for (const el of track.detach()) el.remove();
        this.elements.delete(track.sid ?? '');
      })
      .on(RoomEvent.LocalTrackPublished, (pub: LocalTrackPublication) => {
        // Published again by the SDK after a full reconnect: muted again.
        if (pub.source === Track.Source.Microphone && !this.wantSending) void pub.mute().catch(() => undefined);
      })
      .on(RoomEvent.LocalTrackUnpublished, (pub: LocalTrackPublication) => {
        // The share's picture went without this adapter taking it down: the
        // server withdrew the share, unless the browser's own stop control
        // ended the capture. The SDK unpublishes an ended capture from inside
        // the same ended event that tells this adapter, and may do it first,
        // so the cause is read once that event is over. A track no longer
        // this connection's share (stopped, or kept for the next connection,
        // and taken down once a publish under way completed) is not reported.
        if (pub.source !== Track.Source.ScreenShare || this.stoppingScreen || this.closed) return;
        if (pub.track !== undefined && !this.screen.includes(pub.track)) return;
        window.setTimeout(() => {
          if (!this.captureEnded && !this.closed) events.onScreenEnded('server');
        }, 0);
      })
      .on(RoomEvent.ParticipantPermissionsChanged, (_prev: unknown, participant: Participant) => {
        if (participant === room.localParticipant) this.wakeGrant();
      })
      .on(RoomEvent.MediaDevicesChanged, () => {
        void this.devicesChanged();
      })
      .on(RoomEvent.ActiveDeviceChanged, (kind: MediaDeviceKind, id: string) => {
        // A switch of our own names the output chosen. Any other is the
        // SDK's, after the output in use went (or, on the system default, a
        // change of the devices listed): the system default is chosen again,
        // and any other output the client is told of.
        if (kind !== 'audiooutput' || this.closed || id === this.output) return;
        if (this.output === '') {
          void room.switchActiveDevice('audiooutput', '').catch(() => undefined);
          return;
        }
        this.output = id;
        events.onOutputMoved(id);
      })
      .on(RoomEvent.TrackUnmuted, (pub: TrackPublication, participant: Participant) => {
        // Unmuted by someone else (the room API) while the gate is shut: muted
        // again. An unmute of our own comes with the gate open.
        if (participant !== room.localParticipant || pub.source !== Track.Source.Microphone || this.wantSending) return;
        void (pub as LocalTrackPublication).mute().catch(() => undefined);
      });
    await room.connect(url, token, connectOptions);
    this.ensureOpen();
    // Whoever was already in the room: their tracks are subscribed by choice too.
    this.subscribeAll();
    if (!publish) return;
    await this.publishMicrophone();
  }

  setMicrophone(choice: string | null): void {
    this.choice = choice;
  }

  setProcessing(on: boolean): void {
    this.processing = on;
  }

  async publishMicrophone(): Promise<void> {
    this.ensureOpen();
    // One under way is joined, to its end: its track is set before it is
    // published.
    if (this.publishing === null) {
      if (this.track !== null) return;
      this.publishing = this.capturePublish().finally(() => {
        this.publishing = null;
      });
    }
    await this.publishing;
  }

  private async capturePublish(): Promise<void> {
    const room = this.room;
    const track = await captureMicrophone(this.choice, this.processing);
    // From here the microphone is open: any failure, or a close that came
    // while a step was pending, stops it before the error goes on.
    try {
      this.ensureOpen();
      this.track = track;
      this.lost = false;
      track.on(TrackEvent.Ended, () => {
        this.microphoneLost(track);
      });
      // Muted before it is published, so that LiveKit lists it muted from the
      // first: published unmuted and muted after, it was listed unmuted for
      // about 5 ms of every join (measured on the pinned LiveKit), and a pass
      // of conchd's poller landing there recorded a transmission nobody had
      // made (issue #501). The mute after publishing stays, as the second
      // layer.
      await track.mute();
      this.ensureOpen();
      const publication = await room.localParticipant.publishTrack(track, { source: Track.Source.Microphone });
      this.ensureOpen();
      this.publication = publication;
      await publication.mute();
      this.ensureOpen();
    } catch (err) {
      this.track = null;
      this.publication = null;
      track.stop();
      throw err;
    }
    this.listen(track);
  }

  // switchMicrophone moves the published microphone to another device, or
  // captures the same one again with voice processing as now set (issue #659:
  // the browser ignores a change of the filters on a live capture): the
  // track is restarted on it, which keeps its publication and its mute (the
  // client has shut the gate for the switch). With nothing published yet (the
  // microphone was missing when this connection was made) it is captured and
  // published as at a join. A capture that fails leaves nothing captured: the
  // SDK stops the old capture before it asks for the new one.
  //
  // A switch while the first capture is under way waits for it, and fails
  // with it, rather than capture beside it: a second capture would never be
  // published and would keep its device open until Leave (issue #669). Only
  // the last switch waiting moves the capture, to the choice as it is then.
  async switchMicrophone(choice: string | null): Promise<void> {
    this.ensureOpen();
    this.choice = choice;
    const turn = ++this.switches;
    const publishing = this.publishing;
    if (publishing !== null) {
      await publishing;
      this.ensureOpen();
      if (turn !== this.switches) return;
    }
    const track = this.track;
    if (track === null) {
      await this.publishMicrophone();
      return;
    }
    // The tap's clone keeps the old device open: it goes first.
    await this.untap();
    try {
      await track.restartTrack(microphoneOptions(choice, this.processing));
    } catch (err) {
      this.lost = true;
      track.stop();
      throw err;
    }
    // Closed while the device was being opened: close() stopped the track
    // before the restart had its new capture, so it is stopped again here.
    if (this.closed || track !== this.track) {
      track.stop();
      throw new ClosedError();
    }
    this.lost = false;
    this.listen(track);
  }

  // microphones lists the browser's microphones without asking for any:
  // permission requests off, so reading the list never captures a device.
  // A browser that has not been allowed a microphone lists them without
  // names, and then there is no list to choose from (null).
  async microphones(): Promise<readonly Microphone[] | null> {
    const inputs = await LiveKitRoom.getLocalDevices('audioinput', false);
    if (inputs.some((d) => d.label === '')) return null;
    return inputs.map((d) => ({ id: d.deviceId, label: d.label }));
  }

  // microphoneInUse is the device the capture is on, as the capture itself
  // says. Not the SDK's own reading, which lists the devices with permission
  // requests on.
  microphoneInUse(): string | null {
    const track = this.track;
    if (track === null || this.lost) return null;
    const capture = track.mediaStreamTrack;
    if (capture.readyState !== 'live') return null;
    return capture.getSettings().deviceId ?? null;
  }

  // microphoneProcessing is what the capture says of its voice processing,
  // which may not be what was asked: a browser can keep a filter on.
  microphoneProcessing(): Processing | null {
    const track = this.track;
    if (track === null || this.lost) return null;
    const capture = track.mediaStreamTrack;
    if (capture.readyState !== 'live') return null;
    return processingOf(capture.getSettings());
  }

  // devicesChanged follows the browser's devices. A chosen device no longer
  // listed is lost, whether or not its capture has ended: on Linux the sound
  // server may move a stream whose device has gone to another device rather
  // than end it.
  private async devicesChanged(): Promise<void> {
    const track = this.track;
    const choice = this.choice;
    if (track !== null && choice !== null && !this.lost && !this.closed) {
      const inputs = await LiveKitRoom.getLocalDevices('audioinput', false).catch(() => null);
      if (inputs !== null && !inputs.some((d) => d.deviceId === choice) && choice === this.choice) this.microphoneLost(track);
    }
    if (!this.closed) this.events?.onDevicesChanged();
  }

  // microphoneLost stops a capture that ended or whose device went, once,
  // and tells the client. Nothing captures again until the person chooses.
  private microphoneLost(track: LocalAudioTrack): void {
    if (this.closed || track !== this.track || this.lost) return;
    this.lost = true;
    track.stop();
    void this.untap();
    this.events?.onMicrophoneLost();
  }

  private ensureOpen(): void {
    if (this.closed) throw new ClosedError();
  }

  // listen taps a clone of the microphone for the level meter and the voice
  // detection: the published track is muted while the gate is shut, and a
  // muted track carries silence.
  private listen(track: LocalAudioTrack): void {
    if (this.closed) return;
    // A tap left from an earlier capture is let go: one device open at a time.
    void this.untap();
    const clone = track.mediaStreamTrack.clone();
    // A clone copies the track's enabled flag, and the track was muted just
    // before this: the tap must hear the room whatever the published track does.
    clone.enabled = true;
    const context = new AudioContext();
    const analyser = context.createAnalyser();
    analyser.fftSize = 1024;
    context.createMediaStreamSource(new MediaStream([clone])).connect(analyser);
    this.tap = { context, analyser, clone };
    // The page was interacted with to get here, so the browser lets it run;
    // a context that starts suspended would read silence.
    void context.resume().catch(() => undefined);
  }

  // untap stops the tap's clone and closes its audio context.
  private async untap(): Promise<void> {
    const tap = this.tap;
    this.tap = null;
    if (tap === null) return;
    tap.clone.stop();
    await tap.context.close().catch(() => undefined);
  }

  async setSending(on: boolean): Promise<void> {
    this.wantSending = on;
    const pub = this.publication;
    if (pub === null) return;
    if (on) await pub.unmute();
    else await pub.mute();
  }

  setDeafened(on: boolean): void {
    this.deafened = on;
    for (const { el } of this.elements.values()) el.muted = on;
  }

  // setVolume sets how loud this page plays one person's microphone, 0 to
  // 1: now, and on every element attached for them later.
  setVolume(principal: number, volume: number): void {
    if (volume >= 1) this.volumes.delete(principal);
    else this.volumes.set(principal, volume);
    for (const entry of this.elements.values()) {
      if (entry.principal === principal) entry.el.volume = this.volumeOf(principal);
    }
  }

  private volumeOf(principal: number | null): number {
    return principal === null ? 1 : (this.volumes.get(principal) ?? 1);
  }

  level(): number {
    const tap = this.tap;
    if (tap === null) return 0;
    tap.analyser.getByteTimeDomainData(this.buffer);
    let sum = 0;
    for (const v of this.buffer) {
      const x = (v - 128) / 128;
      sum += x * x;
    }
    // RMS, scaled so ordinary speech reads well above a quiet room.
    return Math.min(1, Math.sqrt(sum / this.buffer.length) * 4);
  }

  // ---- the speakers

  // switchOutput plays every voice, and a watched share's sound, on the
  // output given ('' for the system default): those playing now, and those
  // to come on this connection. Called before connecting, it is where the
  // first track plays. A browser that cannot choose is not asked. A refusal
  // puts sound back where it was, and rejects.
  async switchOutput(id: string): Promise<void> {
    if (!supportsAudioOutputSelection()) return;
    const before = this.output;
    this.output = id;
    try {
      await this.room.switchActiveDevice('audiooutput', id);
    } catch (err) {
      // Some voices may have moved before the refusal: all go back.
      if (this.output === id) {
        this.output = before;
        await this.room.switchActiveDevice('audiooutput', before).catch(() => undefined);
      }
      throw err;
    }
  }

  // outputs lists the browser's outputs without asking for any device:
  // permission requests off, so reading the list never captures one. An
  // output the browser does not name is left out.
  async outputs(): Promise<readonly Output[]> {
    if (!supportsAudioOutputSelection()) return [];
    const outputs = await LiveKitRoom.getLocalDevices('audiooutput', false);
    return outputs.filter((d) => d.label !== '').map((d) => ({ id: d.deviceId, label: d.label }));
  }

  // ---- sharing

  async pickScreen(): Promise<Captured> {
    this.ensureOpen();
    await this.stopScreen();
    const tracks = await this.room.localParticipant.createScreenTracks(captureOptions());
    if (this.closed) {
      for (const t of tracks) t.stop();
      throw new ClosedError();
    }
    this.holdScreen(tracks);
    return { audio: tracks.some((t) => t.source === Track.Source.ScreenShareAudio) };
  }

  // holdScreen makes a capture this connection's share, picked here or kept
  // from an earlier connection.
  private holdScreen(tracks: LocalTrack[]): void {
    this.screen = tracks;
    this.captureEnded = false;
    const video = tracks.find((t) => t.source === Track.Source.ScreenShare);
    if (video === undefined) return;
    video.mediaStreamTrack.addEventListener('ended', () => {
      // Kept for another connection since: not this one's to report.
      if (!this.screen.includes(video)) return;
      // A track stopped by script fires no ended event: this is the browser.
      this.captureEnded = true;
      if (!this.stoppingScreen && !this.closed) this.events?.onScreenEnded('browser');
    });
  }

  keepScreen(): KeptScreen | null {
    const tracks = this.screen;
    this.screen = [];
    this.wakeGrant();
    if (tracks.length === 0) return null;
    // Blanked at once: a publish still under way here completes before it
    // can be taken down, and sends only black frames and silence meanwhile.
    for (const track of tracks) track.mediaStreamTrack.enabled = false;
    // Unpublished, if the room still has them, without being stopped: a
    // connection closed with them published would stop them. This is not
    // the server withdrawing the share, so it is not reported.
    this.stoppingScreen = true;
    try {
      for (const track of tracks) void this.room.localParticipant.unpublishTrack(track, false).catch(() => undefined);
    } finally {
      this.stoppingScreen = false;
    }
    return new Kept(tracks);
  }

  adoptScreen(kept: KeptScreen): void {
    this.ensureOpen();
    if (!(kept instanceof Kept)) throw new Error('voice: not a capture this page kept');
    // Sent again as captured, from this connection's publish on.
    for (const track of kept.tracks) track.mediaStreamTrack.enabled = true;
    this.holdScreen([...kept.tracks]);
  }

  async publishScreen(): Promise<void> {
    this.ensureOpen();
    // One bound for the whole share, on the monotonic clock: a grant comes
    // for every source at once, and a clock set back must not stretch it.
    const deadline = performance.now() + grantWait;
    for (const track of this.screen) {
      // Read at each publish, so a capture adopted from an earlier connection
      // is published at the size it has now.
      const settings = track.mediaStreamTrack.getSettings();
      const options = track.source === Track.Source.ScreenShareAudio ? audioPublishOptions((settings.channelCount ?? 1) >= 2) : videoPublishOptions(settings);
      await this.publishGranted(track, options, deadline);
      this.ensureOpen();
    }
  }

  // publishGranted publishes a share's track once this connection may
  // (issue #525). conchd answers a claim once LiveKit has granted the share's
  // sources, but LiveKit tells this connection on its own socket, which the
  // page can read after conchd's answer. The SDK refuses at once (403) a
  // source that the permission it was last told does not name, so a refusal
  // waits for this connection's permission to change and tries again, until
  // the share's deadline (grantWait). This is the SDK's own check, made
  // early: LiveKit checks every publish against the grant whatever the page
  // does.
  private async publishGranted(track: LocalTrack, options: TrackPublishOptions, deadline: number): Promise<void> {
    for (;;) {
      // Stopped or kept for another connection while an earlier track was
      // published: not to publish here.
      if (!this.screen.includes(track)) throw new TakenError();
      // Listened for before publishing: the grant may come while the SDK is
      // getting there.
      const changed = new Promise<void>((resolve) => {
        this.grantWaiters.push(resolve);
      });
      try {
        await this.room.localParticipant.publishTrack(track, options);
      } catch (err) {
        if (!(err instanceof PublishTrackError) || err.status !== 403) throw err;
        const left = deadline - performance.now();
        if (left <= 0 || !(await within(changed, left))) throw err;
        this.ensureOpen();
        // Stopped or kept for another connection meanwhile: not to publish.
        if (!this.screen.includes(track)) throw err;
        continue;
      }
      // Taken while the publish was on its way: taken down here. The
      // unpublish that took it does so too if the SDK had the publish as
      // pending by then, but finds nothing if it came before (the SDK
      // registers a publish only after waiting out a reconnect), and the
      // room conchd retired may live on for a while. A 403 publishes
      // nothing, so the retry above has nothing to take down.
      if (!this.screen.includes(track)) {
        await this.room.localParticipant.unpublishTrack(track, false).catch(() => undefined);
        throw new TakenError();
      }
      return;
    }
  }

  private wakeGrant(): void {
    const waiters = this.grantWaiters;
    this.grantWaiters = [];
    for (const wake of waiters) wake();
  }

  async stopScreen(): Promise<void> {
    const tracks = this.screen;
    this.screen = [];
    this.wakeGrant();
    if (tracks.length === 0) return;
    this.stoppingScreen = true;
    // The capture stops first, at once: the browser's sharing indicator goes
    // out even if the signalling connection hangs and the unpublish below
    // never answers.
    for (const track of tracks) track.stop();
    try {
      for (const track of tracks) {
        await this.room.localParticipant.unpublishTrack(track, true).catch(() => undefined);
      }
    } finally {
      this.stoppingScreen = false;
    }
  }

  // ---- watching

  setWatching(on: boolean): void {
    this.watching = on;
    this.subscribeAll();
  }

  attachShare(sink: ShareSink | null): void {
    const old = this.sink;
    if (old !== null) {
      this.shareVideo?.detach(old.video);
      this.shareAudio?.detach(old.audio);
    }
    this.sink = sink;
    if (sink !== null) {
      this.shareVideo?.attach(sink.video);
      this.shareAudio?.attach(sink.audio);
    }
  }

  private subscribe(pub: RemoteTrackPublication): void {
    if (this.closed) return;
    const want = wanted(pub.source, this.watching);
    if (pub.isSubscribed !== want) pub.setSubscribed(want);
  }

  private subscribeAll(): void {
    this.room.remoteParticipants.forEach((p: RemoteParticipant) => {
      p.trackPublications.forEach((pub) => {
        this.subscribe(pub);
      });
    });
  }

  private shared(track: RemoteTrack): void {
    if (track.source === Track.Source.ScreenShare) this.shareVideo = track;
    else this.shareAudio = track;
    const sink = this.sink;
    if (sink !== null) track.attach(track.source === Track.Source.ScreenShare ? sink.video : sink.audio);
    this.events?.onShareTracks({ video: this.shareVideo !== null, audio: this.shareAudio !== null });
  }

  private unshared(track: RemoteTrack): void {
    track.detach();
    if (track === this.shareVideo) this.shareVideo = null;
    if (track === this.shareAudio) this.shareAudio = null;
    this.events?.onShareTracks({ video: this.shareVideo !== null, audio: this.shareAudio !== null });
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.wantSending = false;
    // A share's publish waiting for a grant stops waiting.
    this.wakeGrant();
    for (const { el } of this.elements.values()) el.remove();
    this.elements.clear();
    this.attachShare(null);
    this.shareVideo = null;
    this.shareAudio = null;
    const untapped = this.untap();
    const track = this.track;
    this.track = null;
    this.publication = null;
    // disconnect(true) stops published tracks; these may not have been
    // published yet.
    track?.stop();
    for (const t of this.screen) t.stop();
    this.screen = [];
    await untapped;
    try {
      await this.room.disconnect(true);
    } finally {
      redactor.release(this);
    }
  }
}

// canChooseOutput is whether this browser lets a page choose the output it
// plays on (issue #660): not Safari.
export function canChooseOutput(): boolean {
  return supportsAudioOutputSelection();
}

export function newLiveKitRoom(): Room {
  return new LiveKitAdapter();
}
