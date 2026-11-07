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
// Screen share (issue #444). The sharer chooses the capture's caps: 720p,
// 1080p (the default), or Source up to 4K, at 15, 30 or 60 fps. The picture
// has lower simulcast layers where it is larger, with 720p at up to 30 fps at
// the bottom; its sound is published as music. Tracks are subscribed by
// choice, not automatically: every
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
//
// The camera (ADR-015, design §7). It is captured here alone, with
// createLocalVideoTrack, and only when the person presses the camera button
// or tests the camera: the device chosen exactly, or, with none chosen, the
// browser's own choice; at 1280x720 and 30 fps as an ideal, never exact. It
// is published as a track the page provides, so the SDK never captures a
// camera by itself, not another one when the device goes. It is published
// only once LiveKit has said the claim granted the camera (#525), with
// simulcast layers of 360p and 180p below a top layer of at most 1.2 Mbps.
// Stopping stops the capture at once, so the device light goes off, and then
// unpublishes it. A capture kept for the next connection is blanked (black
// frames) and published nowhere until it is adopted.
//
// Tracks are subscribed by kind and source together (S1): a microphone only
// if it is sound, a camera only if it is a picture, from someone presence
// shows holding a camera claim, and only while the call view is shown; a
// share's picture only if it is a picture, and its sound only if it is
// sound, while the share is watched. A track whose kind disagrees with its
// source is never subscribed, so a picture published as a microphone is never
// shown, and a sound published as a camera is never played.
import {
  AudioPresets,
  ConnectionState,
  DisconnectReason,
  LocalAudioTrack,
  LocalVideoTrack,
  LogLevel,
  LoggerNames,
  PublishTrackError,
  Room as LiveKitRoom,
  RoomEvent,
  Track,
  TrackEvent,
  VideoPreset,
  createLocalAudioTrack,
  createLocalVideoTrack,
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
  type VideoCaptureOptions,
} from 'livekit-client';
import type { DisconnectCause } from './disconnect';
import type { Camera, CameraProfile, KeptCamera } from './camera';
import type { CameraTrack, Microphone, Output, Processing, Room, RoomEvents, ShareSink } from './connection';
import { principalOf } from './presence';
import { LogRing, Redactor } from './redact';
import { defaultShareQuality, type Captured, type KeptScreen, type ShareDirection, type ShareQuality, type ShareSample } from './share';

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

// What this page receives besides microphones: whether it watches the share,
// whether the call view is open (the person's choice), whether the video is
// shown here now (false only in the phone layout outside the Voice view,
// design §8), and whether the track's publisher holds a camera claim, as
// presence says.
export interface Wants {
  readonly watching: boolean;
  readonly viewing: boolean;
  readonly shown: boolean;
  readonly claimed: boolean;
}

// wanted is the subscription choice for a remote track, by its declared
// source and its kind together (S1): a microphone always, if it is sound; a
// camera if it is a picture, from someone holding a claim, while the call
// view is open and shown here; a share's picture and its sound while the share is watched,
// each of its own kind. Anything else, and any track whose kind disagrees
// with its source, never.
export function wanted(source: Track.Source, kind: Track.Kind | undefined, want: Wants): boolean {
  switch (source) {
    case Track.Source.Microphone:
      return kind === Track.Kind.Audio;
    case Track.Source.Camera:
      return kind === Track.Kind.Video && want.viewing && want.shown && want.claimed;
    case Track.Source.ScreenShare:
      return kind === Track.Kind.Video && want.watching;
    case Track.Source.ScreenShareAudio:
      return kind === Track.Kind.Audio && want.watching;
    default:
      return false;
  }
}

// What a track would be wanted for if the page wanted everything: false only
// for a kind its source disagrees with.
const everything: Wants = { watching: true, viewing: true, shown: true, claimed: true };

// ---- the camera

// What a capture of the camera asks for (design §7.5): the chosen device
// exactly, as an object, which the SDK leaves alone (a bare id it tries as
// exact and then again as ideal, which captures some other camera). The
// desktop asks 1280x720 at 30 fps, the phone (O11) 640x360 at 24 fps, each as
// an ideal. With none chosen, the browser chooses, and the person with it in
// its prompt; on a phone it is asked for the front camera first
// (facingMode user, which a browser takes as an ideal, never exact). A
// remembered choice is exact and carries no facingMode.
export const cameraAsk = { width: 1280, height: 720, frameRate: 30 } as const;
export const phoneCameraAsk = { width: 640, height: 360, frameRate: 24 } as const;

export function cameraOptions(choice: string | null, profile: CameraProfile = 'desktop'): VideoCaptureOptions {
  const resolution = { ...(profile === 'phone' ? phoneCameraAsk : cameraAsk) };
  if (choice !== null) return { resolution, deviceId: { exact: choice } };
  return profile === 'phone' ? { resolution, facingMode: 'user' } : { resolution };
}

// How the camera is published (design §7.5). The desktop: a top layer of at
// most 1.2 Mbps, and simulcast layers of 360p at 450 kbps and 180p at
// 150 kbps, VP8 the SDK's default. The phone (O11): a top layer of 360p at
// most 450 kbps, one simulcast layer of 180p at 150 kbps, VP8.
export function cameraPublishOptions(profile: CameraProfile = 'desktop'): TrackPublishOptions {
  if (profile === 'phone') {
    return {
      source: Track.Source.Camera,
      simulcast: true,
      videoCodec: 'vp8',
      videoEncoding: { maxBitrate: 450_000, maxFramerate: phoneCameraAsk.frameRate },
      videoSimulcastLayers: [new VideoPreset(320, 180, 150_000, 15)],
    };
  }
  return {
    source: Track.Source.Camera,
    simulcast: true,
    videoEncoding: { maxBitrate: 1_200_000, maxFramerate: cameraAsk.frameRate },
    videoSimulcastLayers: [new VideoPreset(320, 180, 150_000, 15), new VideoPreset(640, 360, 450_000, 30)],
  };
}

// The profile each capture was made with: its switches and its publish keep
// it, including a capture kept for the next connection.
const captureProfiles = new WeakMap<LocalVideoTrack, CameraProfile>();

// captureCameraTrack captures the camera and wraps it as a track the page
// provides: the SDK restarts nothing it did not capture, so a camera that
// goes is never replaced by another.
async function captureCameraTrack(choice: string | null, profile: CameraProfile): Promise<LocalVideoTrack> {
  const captured = await createLocalVideoTrack(cameraOptions(choice, profile));
  const track = new LocalVideoTrack(captured.mediaStreamTrack, captured.constraints, true);
  track.source = Track.Source.Camera;
  captureProfiles.set(track, profile);
  return track;
}

// listCameras lists the cameras without asking for any: null while the
// browser does not name them (no camera allowed yet).
async function listCameras(): Promise<readonly Camera[] | null> {
  const inputs = await LiveKitRoom.getLocalDevices('videoinput', false);
  if (inputs.some((d) => d.label === '')) return null;
  return inputs.map((d) => ({ id: d.deviceId, label: d.label }));
}

// LiveKit's own number for the camera source in a participant's permission
// (livekit.TrackSource.CAMERA in its protocol), which the SDK does not
// export.
const protoCamera = 1;

// ---- the private camera test (design §7.1)
//
// A capture that belongs to no room: nothing connects, claims, publishes or
// records. The page shows it in a local preview and stops it.

export interface CameraTestCapture {
  // Plays the preview into a video element, or into none.
  show(el: HTMLVideoElement | null): void;
  stop(): void;
}

export async function captureCameraTest(choice: string | null, profile: CameraProfile = 'desktop'): Promise<CameraTestCapture> {
  const captured = await createLocalVideoTrack(cameraOptions(choice, profile));
  let shown: HTMLVideoElement | null = null;
  return {
    show: (el) => {
      if (shown !== null) captured.detach(shown);
      shown = el;
      if (el !== null) captured.attach(el);
    },
    stop: () => {
      if (shown !== null) captured.detach(shown);
      shown = null;
      captured.stop();
    },
  };
}

export const testCameras = listCameras;

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

// ---- the private microphone test (issue #846)
//
// A capture that belongs to no room: nothing here connects, publishes or
// records. The page's test reads the track into a local audio graph and stops
// it. It is here because this file alone reaches for the devices.

// TestCapture is a microphone captured for the test, asked for as a call asks
// for it (the same constraints), and the way to release it.
export interface TestCapture {
  readonly track: MediaStreamTrack;
  stop(): void;
}

export async function captureTest(choice: string | null, processing = true): Promise<TestCapture> {
  const captured = await createLocalAudioTrack(microphoneOptions(choice, processing));
  return {
    track: captured.mediaStreamTrack,
    stop: () => {
      captured.stop();
    },
  };
}

// testMicrophones lists the microphones without capturing any. A browser that
// has not been allowed one lists them without names: then null, as the call's
// list is.
export async function testMicrophones(): Promise<readonly Microphone[] | null> {
  const inputs = await LiveKitRoom.getLocalDevices('audioinput', false);
  if (inputs.some((d) => d.label === '')) return null;
  return inputs.map((d) => ({ id: d.deviceId, label: d.label }));
}

// watchMicrophones calls back when the browser's devices change, until the
// returned function is called, which removes exactly the listener added. It
// listens itself rather than through a room: a room that never connected
// does not remove its own listener when disconnected.
export function watchMicrophones(onChange: () => void): () => void {
  const devices = (navigator as { readonly mediaDevices?: MediaDevices }).mediaDevices;
  if (devices === undefined) return () => undefined;
  devices.addEventListener('devicechange', onChange);
  return () => {
    devices.removeEventListener('devicechange', onChange);
  };
}

// ---- the share's sizes, bitrates and stats

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

const sourceAsk = { width: 3840, height: 2160, frameRate: 60 } as const;

// The SDK turns the resolution into an ideal (a maximum on Safari), never an
// exact size or a minimum, so a smaller source is not scaled up or refused.
function shareAsk(quality: ShareQuality): { readonly width: number; readonly height: number; readonly frameRate: number } {
  if (quality.resolution === '720p') return { width: 1280, height: 720, frameRate: quality.frameRate };
  if (quality.resolution === '1080p') return { width: 1920, height: 1080, frameRate: quality.frameRate };
  return { ...sourceAsk, frameRate: quality.frameRate };
}

const shareTops: readonly { readonly longest: number; readonly maxBitrate: number }[] = [
  { longest: 1920, maxBitrate: 6_000_000 },
  { longest: 2560, maxBitrate: 10_000_000 },
];
const shareTopMost = 18_000_000;

// The lower layers, smallest first. The bottom layer keeps motion smooth on
// a pane-sized view; neither layer is ever faster than the top.
const shareLower: readonly ShareLayer[] = [
  { width: 1280, height: 720, maxBitrate: 2_000_000, maxFramerate: 30 },
  { width: 1920, height: 1080, maxBitrate: 4_000_000, maxFramerate: 30 },
];

const known = (n: number | undefined): n is number => n !== undefined && Number.isFinite(n) && n > 0;

// fitBox scales a picture down to fit, keeping its shape and standing the box
// on end for a portrait capture. It never scales up.
function fitBox(width: number, height: number, boxWidth: number, boxHeight: number): readonly [number, number] {
  const long = Math.max(width, height);
  const short = Math.min(width, height);
  const boxLong = Math.max(boxWidth, boxHeight);
  const boxShort = Math.min(boxWidth, boxHeight);
  if (long <= boxLong && short <= boxShort) return [width, height];
  const [l, s] = long * boxShort >= short * boxLong ? [boxLong, Math.floor((short * boxLong) / long)] : [Math.floor((long * boxShort) / short), boxShort];
  return height > width ? [s, l] : [l, s];
}

// shareEncodings chooses the top layer from the capture and the sharer's
// choice, and the lower layers from the top, smallest first.
export function shareEncodings(captured: CapturedVideo, quality: ShareQuality = defaultShareQuality): { readonly top: ShareLayer; readonly lower: readonly ShareLayer[] } {
  const ask = shareAsk(quality);
  const [capturedWidth, capturedHeight] = known(captured.width) && known(captured.height) ? [captured.width, captured.height] : [ask.width, ask.height];
  const [width, height] = fitBox(capturedWidth, capturedHeight, ask.width, ask.height);
  const fps = known(captured.frameRate) ? Math.min(captured.frameRate, ask.frameRate) : ask.frameRate;
  const long = Math.max(width, height);
  const short = Math.min(width, height);

  let maxBitrate = shareTopMost;
  for (const row of shareTops) {
    if (long <= row.longest) {
      maxBitrate = row.maxBitrate;
      break;
    }
  }

  const lower: ShareLayer[] = [];
  for (const layer of shareLower) {
    if (long <= Math.max(layer.width, layer.height) && short <= Math.min(layer.width, layer.height)) continue;
    const [w, h] = fitBox(width, height, layer.width, layer.height);
    lower.push({ width: w, height: h, maxBitrate: layer.maxBitrate, maxFramerate: Math.min(layer.maxFramerate, fps) });
  }
  return { top: { width, height, maxBitrate, maxFramerate: fps }, lower };
}

// What the picker asks for: the chosen picture caps, unprocessed sound, the
// Conch tab left out, and the shared tab still heard by the sharer. At 15 fps
// detail keeps text sharp; at 30 and 60 motion keeps the frame rate instead.
export function captureOptions(quality: ShareQuality = defaultShareQuality): ScreenShareCaptureOptions {
  return {
    video: true,
    audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
    resolution: shareAsk(quality),
    contentHint: quality.frameRate === 15 ? 'detail' : 'motion',
    systemAudio: 'include',
    selfBrowserSurface: 'exclude',
    surfaceSwitching: 'include',
    suppressLocalAudioPlayback: false,
  };
}

// How the picture is published, from what was captured and the chosen caps.
// The codec is the SDK's default, VP8.
export function videoPublishOptions(captured: CapturedVideo, quality: ShareQuality = defaultShareQuality): TrackPublishOptions {
  const { top, lower } = shareEncodings(captured, quality);
  return {
    source: Track.Source.ScreenShare,
    simulcast: true,
    screenShareEncoding: new VideoPreset(top.width, top.height, top.maxBitrate, top.maxFramerate).encoding,
    screenShareSimulcastLayers: lower.map((l) => new VideoPreset(l.width, l.height, l.maxBitrate, l.maxFramerate)),
  };
}

// The subset of inbound-rtp and outbound-rtp stats the readout uses.
export interface ShareStats {
  readonly id?: string;
  readonly ssrc?: number;
  readonly type: string;
  readonly kind?: string;
  readonly mediaType?: string;
  readonly frameWidth?: number;
  readonly frameHeight?: number;
  readonly framesPerSecond?: number;
  readonly bytesReceived?: number;
  readonly bytesSent?: number;
  readonly timestamp?: number;
}

const count = (n: number | undefined): n is number => n !== undefined && Number.isFinite(n) && n >= 0;

// shareSample reads the received picture, or the largest sent simulcast layer.
// publication names the publication the stats come from, so a sample of a
// replaced publication is never compared with one of the old.
export function shareSample(stats: Iterable<ShareStats>, direction: ShareDirection, publication?: string): ShareSample | null {
  const wanted = direction === 'received' ? 'inbound-rtp' : 'outbound-rtp';
  let found: ShareStats | null = null;
  for (const stat of stats) {
    if (stat.type !== wanted || stat.kind === 'audio' || stat.mediaType === 'audio') continue;
    if (!known(stat.frameWidth) || !known(stat.frameHeight) || !count(stat.framesPerSecond)) continue;
    if (found === null || stat.frameWidth * stat.frameHeight > (found.frameWidth ?? 0) * (found.frameHeight ?? 0)) found = stat;
  }
  if (found === null) return null;
  const bytes = direction === 'received' ? found.bytesReceived : found.bytesSent;
  const stream = found.id !== undefined || found.ssrc !== undefined ? `${found.id ?? ''}/${found.ssrc === undefined ? '' : String(found.ssrc)}` : undefined;
  return {
    width: found.frameWidth ?? 0,
    height: found.frameHeight ?? 0,
    fps: found.framesPerSecond ?? 0,
    ...(stream === undefined ? {} : { stream }),
    ...(publication === undefined ? {} : { publication }),
    ...(count(bytes) ? { bytes } : {}),
    ...(count(found.timestamp) ? { timestamp: found.timestamp } : {}),
  };
}

// shareReadout formats the latest sample. Bitrate needs two samples of the
// same RTP stream of the same publication, and counters that did not go back;
// zero bytes and zero fps are real readings, not missing ones.
export function shareReadout(current: ShareSample | null, previous?: ShareSample | null): string | null {
  if (current === null) return null;
  let text = `${String(current.width)}×${String(current.height)} · ${String(Math.round(current.fps))} fps`;
  if (previous?.bytes === undefined || previous.timestamp === undefined || current.bytes === undefined || current.timestamp === undefined) return text;
  if (previous.stream !== current.stream || previous.publication !== current.publication) return text;
  const elapsed = current.timestamp - previous.timestamp;
  const bytes = current.bytes - previous.bytes;
  if (elapsed <= 0 || bytes < 0) return text;
  text += ` · ${(bytes * 8 / elapsed / 1000).toFixed(1)} Mbps`;
  return text;
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

// A camera's capture between two connections: still capturing, blanked,
// published on neither.
class KeptCam implements KeptCamera {
  readonly track: LocalVideoTrack;
  onEnded: (() => void) | null = null;

  constructor(track: LocalVideoTrack) {
    this.track = track;
    // The track's own event, which follows the capture through a switch of
    // camera (the SDK moves it to each new MediaStreamTrack).
    track.on(TrackEvent.Ended, this.ended);
  }

  private readonly ended = (): void => {
    const ended = this.onEnded;
    this.onEnded = null;
    ended?.();
  };

  live = (): boolean => this.track.mediaStreamTrack.readyState === 'live';

  stop = (): void => {
    this.forget();
    this.track.stop();
  };

  // forget stops listening: the capture is another connection's now.
  forget = (): void => {
    this.onEnded = null;
    this.track.off(TrackEvent.Ended, this.ended);
  };
}

// How many samples the level is read from: 512, about 10 ms at 48 kHz, the
// frame conch-voice's open microphone judges (conch-voice.md section 3).
// It was 1024 until issue #965; with the level read every 10 ms, a longer
// window would let a loud onset show only once it filled more of it.
export const levelWindow = 512;

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
  private readonly buffer = new Uint8Array(levelWindow);
  // The share this page captured, until it is stopped; and whether this
  // adapter is the one taking it down, so its own unpublish is not reported.
  private screen: LocalTrack[] = [];
  private shareQuality: ShareQuality = defaultShareQuality;
  private shareQualityChange: Promise<void> = Promise.resolve();
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
  // The camera this page captured, until it is stopped or kept; its
  // publication once published; whether this adapter is taking it down, so
  // its own unpublish is not reported; and the element of the self-view.
  private camera: LocalVideoTrack | null = null;
  private cameraPub: LocalTrackPublication | null = null;
  private stoppingCamera = false;
  // Numbers each capture of the camera, and each stop or keep: a capture
  // that completes after a newer one began, or after a stop, is stopped at
  // once and never held, so no capture is left running unreferenced.
  private cameraTurn = 0;
  // The switch of camera under way, and the number of the last one asked
  // for (switchCamera).
  private switching: Promise<void> = Promise.resolve();
  private switchTurn = 0;
  // Stops listening for the held camera's end (holdCamera).
  private forgetEnded: () => void = () => undefined;
  // The captures this connection kept for the next one: never its to stop.
  private readonly keptTracks = new WeakSet<LocalVideoTrack>();
  // The tracks that arrived with a kind their source disagrees with (S1),
  // by sid: never subscribed again.
  private readonly refused = new Set<string>();
  private selfView: HTMLVideoElement | null = null;
  // Whether the call view is shown, who presence shows holding a camera
  // claim, the camera tracks received by principal, and the element each
  // plays into.
  private viewing = false;
  private shown = true;
  private claimed: ReadonlySet<number> = new Set();
  private readonly remoteCameras = new Map<number, RemoteTrack>();
  private readonly cameraViews = new Map<number, HTMLVideoElement>();

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
      .on(RoomEvent.TrackPublished, (pub: RemoteTrackPublication, participant: RemoteParticipant) => {
        this.subscribe(pub, participant);
      })
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, pub: RemoteTrackPublication, participant: RemoteParticipant) => {
        // Nothing is played, or left in the page, after a close.
        if (this.closed) return;
        // Subscribed by choice alone, and the choice is made again here, on
        // what arrived: a track whose kind disagrees with its source is let
        // go, never played or shown (S1).
        if (!wanted(track.source, track.kind, this.wantsOf(participant))) {
          // A kind its source disagrees with is never wanted, whatever the
          // page shows: remembered, so no later choice subscribes it again.
          if (!wanted(track.source, track.kind, everything)) this.refused.add(pub.trackSid);
          pub.setSubscribed(false);
          return;
        }
        if (isShare(track.source)) {
          this.shared(track);
          return;
        }
        if (track.source === Track.Source.Camera) {
          this.cameraArrived(track, participant);
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
        if (track.source === Track.Source.Camera) {
          this.cameraGone(track);
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
        // The camera went without this adapter taking it down: LiveKit took
        // the source away (the claim ended, or the pass withdrew it). The SDK
        // also unpublishes every track when the server ends the connection,
        // just before it says the room is disconnected, so the cause is read
        // once that is over: a connection's end is the client's to act on
        // (disconnect.ts cameraAfter), and not the server taking the camera.
        if (pub.source === Track.Source.Camera) {
          if (this.stoppingCamera || this.closed) return;
          const track = pub.track;
          if (track !== undefined && track !== this.camera) return;
          this.cameraPub = null;
          window.setTimeout(() => {
            if (this.closed || room.state === ConnectionState.Disconnected) return;
            if (track !== undefined && track !== this.camera) return;
            events.onCameraEnded('server');
          }, 0);
          return;
        }
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
    analyser.fftSize = levelWindow;
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

  async pickScreen(quality: ShareQuality = defaultShareQuality): Promise<Captured> {
    this.ensureOpen();
    this.shareQuality = quality;
    await this.stopScreen();
    const tracks = await this.room.localParticipant.createScreenTracks(captureOptions(quality));
    if (this.closed) {
      for (const t of tracks) t.stop();
      throw new ClosedError();
    }
    // The size asked for is an ideal, which a browser may exceed: hold the
    // chosen cap on the capture before anything is published from it.
    const video = tracks.find((t): t is LocalVideoTrack => t.source === Track.Source.ScreenShare);
    if (video !== undefined) {
      try {
        await this.constrainShare(video, quality);
        this.ensureOpen();
      } catch (err) {
        for (const t of tracks) t.stop();
        throw err;
      }
    }
    this.holdScreen(tracks);
    return { audio: tracks.some((t) => t.source === Track.Source.ScreenShareAudio) };
  }

  setShareQuality(quality: ShareQuality): Promise<void> {
    this.shareQuality = quality;
    // setParameters must follow the sender's latest parameters. Queue changes
    // made quickly one after another, rather than letting an older one win.
    const change = this.shareQualityChange.then(() => this.applyShareQuality());
    this.shareQualityChange = change.catch(() => undefined);
    return change;
  }

  private async applyShareQuality(): Promise<void> {
    const track = this.screen.find((t): t is LocalVideoTrack => t.source === Track.Source.ScreenShare);
    if (track === undefined) return;
    await this.constrainShare(track, this.shareQuality);
    this.ensureOpen();
    await this.shapeSender(track, this.shareQuality);
  }

  // constrainShare holds the chosen caps on the live capture.
  private async constrainShare(track: LocalVideoTrack, quality: ShareQuality): Promise<void> {
    const capture = track.mediaStreamTrack;
    const ask = shareAsk(quality);
    capture.contentHint = quality.frameRate === 15 ? 'detail' : 'motion';
    await capture.applyConstraints({ width: { max: ask.width }, height: { max: ask.height }, frameRate: { max: ask.frameRate } });
  }

  // shapeSender sets the published encodings by position: the highest carries
  // the chosen top layer against the size actually captured, and those below
  // it the lower layers, smallest first. LiveKit names encodings by how many
  // there are (q; q,h; q,h,f) and fixes that number at publish time, so the
  // names say nothing about which layer one carries. An encoding with no
  // layer of its own takes the top, and no layer is above the top.
  private async shapeSender(track: LocalVideoTrack, quality: ShareQuality): Promise<void> {
    const settings = track.mediaStreamTrack.getSettings();
    track.publishOptions = videoPublishOptions(settings, quality);
    const sender = track.sender;
    if (sender === undefined) return;
    const { top, lower } = shareEncodings(settings, quality);
    const parameters = sender.getParameters();
    const last = parameters.encodings.length - 1;
    // Smallest first: the order LiveKit builds them in.
    parameters.encodings = parameters.encodings.map((encoding, index) => {
      const own = index === last ? top : lower[index];
      const layer = own === undefined ? top : { ...own, maxBitrate: Math.min(own.maxBitrate, top.maxBitrate), maxFramerate: Math.min(own.maxFramerate, top.maxFramerate) };
      const scale = known(settings.width) ? Math.max(1, settings.width / layer.width) : 1;
      return { ...encoding, maxBitrate: layer.maxBitrate, maxFramerate: layer.maxFramerate, scaleResolutionDownBy: scale };
    });
    await sender.setParameters(parameters);
  }

  async readShareStats(direction: ShareDirection): Promise<ShareSample | null> {
    const track = direction === 'sent' ? this.screen.find((t) => t.source === Track.Source.ScreenShare) : this.shareVideo;
    const report = await track?.getRTCStatsReport();
    if (report === undefined) return null;
    const stats: ShareStats[] = [];
    report.forEach((stat) => {
      stats.push(stat as ShareStats);
    });
    return shareSample(stats, direction, track?.sid);
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
      const options = track.source === Track.Source.ScreenShareAudio ? audioPublishOptions((settings.channelCount ?? 1) >= 2) : videoPublishOptions(settings, this.shareQuality);
      await this.publishGranted(track, options, deadline);
      this.ensureOpen();
      // LiveKit builds the top layer from the real capture size: hold the cap
      // on it. A refusal leaves the share as published.
      if (track.source === Track.Source.ScreenShare) await this.shapeSender(track as LocalVideoTrack, this.shareQuality).catch(() => undefined);
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
  private async publishGranted(track: LocalTrack, options: TrackPublishOptions, deadline: number, held: () => boolean = () => this.screen.includes(track)): Promise<LocalTrackPublication> {
    for (;;) {
      // Stopped or kept for another connection while an earlier track was
      // published: not to publish here.
      if (!held()) throw new TakenError();
      // Listened for before publishing: the grant may come while the SDK is
      // getting there.
      const changed = new Promise<void>((resolve) => {
        this.grantWaiters.push(resolve);
      });
      let publication: LocalTrackPublication;
      try {
        publication = await this.room.localParticipant.publishTrack(track, options);
      } catch (err) {
        if (!(err instanceof PublishTrackError) || err.status !== 403) throw err;
        const left = deadline - performance.now();
        if (left <= 0 || !(await within(changed, left))) throw err;
        this.ensureOpen();
        // Stopped or kept for another connection meanwhile: not to publish.
        if (!held()) throw err;
        continue;
      }
      // Taken while the publish was on its way: taken down here. The
      // unpublish that took it does so too if the SDK had the publish as
      // pending by then, but finds nothing if it came before (the SDK
      // registers a publish only after waiting out a reconnect), and the
      // room conchd retired may live on for a while. A 403 publishes
      // nothing, so the retry above has nothing to take down.
      if (!held()) {
        await this.room.localParticipant.unpublishTrack(track, false).catch(() => undefined);
        throw new TakenError();
      }
      return publication;
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

  private wantsOf(participant: Participant | undefined): Wants {
    const principal = participant === undefined ? null : principalOf(participant.identity);
    return { watching: this.watching, viewing: this.viewing, shown: this.shown, claimed: principal !== null && this.claimed.has(principal) };
  }

  private subscribe(pub: RemoteTrackPublication, participant: RemoteParticipant | undefined): void {
    if (this.closed) return;
    const want = !this.refused.has(pub.trackSid) && wanted(pub.source, pub.kind, this.wantsOf(participant));
    // What was asked, not what has arrived: a subscription on its way is not
    // asked for again, nor one refused asked back.
    if (pub.isDesired !== want) pub.setSubscribed(want);
  }

  private subscribeAll(): void {
    this.room.remoteParticipants.forEach((p: RemoteParticipant) => {
      p.trackPublications.forEach((pub) => {
        this.subscribe(pub, p);
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

  // ---- the camera

  async captureCamera(choice: string | null, profile: CameraProfile = 'desktop'): Promise<void> {
    this.ensureOpen();
    // One capture at a time: one held is stopped first.
    await this.stopCamera();
    this.ensureOpen();
    const turn = ++this.cameraTurn;
    const track = await captureCameraTrack(choice, profile);
    if (this.closed) {
      track.stop();
      throw new ClosedError();
    }
    // A newer capture, a stop or a kept capture came while the browser was
    // asked: this one is not the camera.
    if (turn !== this.cameraTurn || this.camera !== null) {
      track.stop();
      throw new TakenError();
    }
    this.holdCamera(track);
  }

  // holdCamera makes a capture this connection's camera, captured here or
  // kept from an earlier connection.
  private holdCamera(track: LocalVideoTrack): void {
    // Never two captures: one held is stopped before another takes its place.
    const before = this.camera;
    if (before !== null && before !== track) {
      before.stop();
      if (this.selfView !== null) before.detach(this.selfView);
    }
    this.camera = track;
    this.cameraPub = null;
    // The track's own event, not its MediaStreamTrack's: a switch of camera
    // replaces the MediaStreamTrack, and the SDK moves its listener to each.
    this.forgetEnded();
    const ended = () => {
      // Kept for another connection since, or stopped by this adapter (a
      // track stopped by script fires no ended event): not this one's.
      if (this.camera !== track || this.stoppingCamera || this.closed) return;
      this.events?.onCameraEnded('device');
    };
    track.on(TrackEvent.Ended, ended);
    this.forgetEnded = () => {
      track.off(TrackEvent.Ended, ended);
      this.forgetEnded = () => undefined;
    };
    if (this.selfView !== null) track.attach(this.selfView);
    this.events?.onCameraChanged();
  }

  // cameraGranted reports whether LiveKit's last word on this connection's
  // permission lets it publish a camera: an empty list of sources is every
  // source.
  private cameraGranted(): boolean {
    const p = this.room.localParticipant.permissions;
    if (p === undefined || !p.canPublish) return false;
    const sources: readonly number[] = p.canPublishSources;
    return sources.length === 0 || sources.includes(protoCamera);
  }

  async awaitCameraGrant(): Promise<void> {
    this.ensureOpen();
    const track = this.camera;
    const deadline = performance.now() + grantWait;
    for (;;) {
      if (track === null || this.camera !== track) throw new TakenError();
      if (this.cameraGranted()) return;
      const changed = new Promise<void>((resolve) => {
        this.grantWaiters.push(resolve);
      });
      const left = deadline - performance.now();
      if (left <= 0 || !(await within(changed, left))) throw new Error('voice: the camera was not granted in time');
      this.ensureOpen();
    }
  }

  async publishCamera(): Promise<void> {
    this.ensureOpen();
    const track = this.camera;
    if (track === null) throw new TakenError();
    const publication = await this.publishGranted(track, cameraPublishOptions(captureProfiles.get(track)), performance.now() + grantWait, () => this.camera === track);
    this.ensureOpen();
    this.cameraPub = publication;
    this.events?.onCameraChanged();
  }

  // switchCamera moves the capture to another camera, exactly, keeping its
  // publication. On the latest switch's failure the capture is stopped, but
  // its wrapper and publication stay held: the caller must switch it again or
  // call stopCamera.
  //
  // Switches run one after another, and only the last one asked for runs: one
  // overtaken while it waited does nothing, so an earlier switch's failure
  // can never stop a capture a later one made.
  async switchCamera(choice: string): Promise<void> {
    this.ensureOpen();
    const turn = ++this.switchTurn;
    const before = this.switching;
    let finished: () => void = () => undefined;
    this.switching = new Promise<void>((resolve) => {
      finished = resolve;
    });
    try {
      await before;
      if (turn !== this.switchTurn) return;
      this.ensureOpen();
      const track = this.camera;
      if (track === null) return;
      // The capture this switch moves: a stop, a keep or a new capture since
      // moves cameraTurn on, and the switch then answers for nothing held.
      const owned = this.cameraTurn;
      const ours = () => owned === this.cameraTurn && this.camera === track;
      // What the switch leaves behind once the track is not this
      // connection's: stopped, unless it was kept for the next connection,
      // which holds it now.
      const letGo = () => {
        if (!this.keptTracks.has(track)) track.stop();
      };
      try {
        await track.restartTrack(cameraOptions(choice, captureProfiles.get(track)));
      } catch (err) {
        // Only the last switch asked for, on the capture this connection
        // still holds, may stop it: one overtaken while the browser was
        // asked leaves it to the later one.
        if (ours()) {
          // Keep the stopped wrapper for a newer choice to restart while the
          // camera is starting; its caller stops and releases if none is left.
          if (turn === this.switchTurn) track.stop();
        } else if (this.camera !== track) letGo();
        throw err;
      }
      if (this.closed || !ours()) {
        if (this.camera !== track) letGo();
        throw new ClosedError();
      }
      this.events?.onCameraChanged();
    } finally {
      finished();
    }
  }

  async stopCamera(): Promise<void> {
    // A capture still being asked for is not to be held once it comes.
    this.cameraTurn++;
    const track = this.camera;
    this.camera = null;
    this.cameraPub = null;
    this.wakeGrant();
    this.forgetEnded();
    if (track === null) return;
    this.stoppingCamera = true;
    // The capture stops first, at once: the device light goes off even if
    // the signalling connection hangs and the unpublish never answers.
    track.stop();
    if (this.selfView !== null) track.detach(this.selfView);
    this.events?.onCameraChanged();
    try {
      await this.room.localParticipant.unpublishTrack(track, true).catch(() => undefined);
    } finally {
      this.stoppingCamera = false;
    }
  }

  keepCamera(): KeptCamera | null {
    this.cameraTurn++;
    const track = this.camera;
    this.camera = null;
    this.cameraPub = null;
    this.wakeGrant();
    this.forgetEnded();
    if (track === null) return null;
    this.keptTracks.add(track);
    // Blanked at once: black frames until the next connection adopts it.
    track.mediaStreamTrack.enabled = false;
    if (this.selfView !== null) track.detach(this.selfView);
    this.stoppingCamera = true;
    try {
      void this.room.localParticipant.unpublishTrack(track, false).catch(() => undefined);
    } finally {
      this.stoppingCamera = false;
    }
    this.events?.onCameraChanged();
    return new KeptCam(track);
  }

  adoptCamera(kept: KeptCamera): void {
    this.ensureOpen();
    this.cameraTurn++;
    if (!(kept instanceof KeptCam)) throw new Error('voice: not a camera this page kept');
    kept.forget();
    kept.track.mediaStreamTrack.enabled = true;
    this.holdCamera(kept.track);
  }

  // cameraTrack says what this page's own camera is, from the local track
  // alone: none, captured and not published, or published.
  cameraTrack(): CameraTrack {
    const track = this.camera;
    if (track === null || track.mediaStreamTrack.readyState !== 'live') return 'none';
    return this.cameraPub === null ? 'captured' : 'published';
  }

  cameraInUse(): string | null {
    const track = this.camera;
    if (track === null || track.mediaStreamTrack.readyState !== 'live') return null;
    return track.mediaStreamTrack.getSettings().deviceId ?? null;
  }

  cameras(): Promise<readonly Camera[] | null> {
    return listCameras();
  }

  attachSelf(el: HTMLVideoElement | null): void {
    const old = this.selfView;
    if (old !== null) this.camera?.detach(old);
    this.selfView = el;
    if (el !== null) this.camera?.attach(el);
  }

  // setViewing subscribes the cameras of the people who hold a claim while
  // the call view is shown, and lets every camera go, explicitly, when it
  // closes.
  setViewing(on: boolean): void {
    this.viewing = on;
    this.subscribeAll();
  }

  // setShown is whether the video is shown here now: false in the phone
  // layout outside the Voice view, where every camera is let go, explicitly,
  // and none is received. The person's own choice (setViewing) is kept, so
  // that the cameras come back with the Voice view and need no tap (O8).
  setShown(on: boolean): void {
    this.shown = on;
    this.subscribeAll();
  }

  // setCameras is who presence shows holding a camera claim: a camera from
  // anyone else is not subscribed, and one subscribed is let go.
  setCameras(claimed: ReadonlySet<number>): void {
    this.claimed = new Set(claimed);
    this.subscribeAll();
  }

  attachCamera(principal: number, el: HTMLVideoElement | null): void {
    const old = this.cameraViews.get(principal);
    const track = this.remoteCameras.get(principal);
    if (old !== undefined) track?.detach(old);
    if (el === null) this.cameraViews.delete(principal);
    else {
      this.cameraViews.set(principal, el);
      track?.attach(el);
    }
  }

  private cameraArrived(track: RemoteTrack, participant: RemoteParticipant): void {
    const principal = principalOf(participant.identity);
    if (principal === null) return;
    const before = this.remoteCameras.get(principal);
    if (before !== undefined && before !== track) before.detach();
    this.remoteCameras.set(principal, track);
    const el = this.cameraViews.get(principal);
    if (el !== undefined) track.attach(el);
    this.events?.onCameraTracks([...this.remoteCameras.keys()]);
  }

  private cameraGone(track: RemoteTrack): void {
    track.detach();
    for (const [principal, t] of this.remoteCameras) {
      if (t === track) this.remoteCameras.delete(principal);
    }
    this.events?.onCameraTracks([...this.remoteCameras.keys()]);
  }

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.wantSending = false;
    // A share's or a camera's publish waiting for a grant stops waiting.
    this.wakeGrant();
    for (const { el } of this.elements.values()) el.remove();
    this.elements.clear();
    this.attachShare(null);
    this.shareVideo = null;
    this.shareAudio = null;
    for (const t of this.remoteCameras.values()) t.detach();
    this.remoteCameras.clear();
    this.cameraViews.clear();
    // The camera stops with the connection, unless it was kept for the next.
    const camera = this.camera;
    this.camera = null;
    this.cameraPub = null;
    camera?.stop();
    this.selfView = null;
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
