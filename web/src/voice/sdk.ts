// The one file that imports the LiveKit SDK. It adapts the SDK's Room to the
// small Room interface of connection.ts, and quiets the SDK's logging.
//
// The microphone is captured here with echo cancellation, noise suppression and
// auto gain on, muted, published once, and muted again straight after
// publishing, as conch-voice.md section 3 does: the SDK enables a track when it
// publishes it, and again when it republishes after a long interruption, so the
// track is muted after each unless the gate is open. The client counts as
// connected only after that mute. A remote unmute (LiveKit's room API can ask
// for one) is undone the same way while the gate is shut. The mute before
// publishing is so that LiveKit never lists the microphone unmuted at a join
// (issue #501; publishMicrophone).
//
// Leave can come at any point of a connection. Once the adapter is closed,
// every step that resumes after an await stops, and a microphone or screen
// captured or published after the close is stopped, so nothing keeps a device
// open.
//
// Screen share (issue #444). A share is captured with the SDK's own picker
// call and published as the screen-share sources, at 1080p and up to 30 fps
// with simulcast layers of 720p at 15 fps and 360p at 5 fps (decision O6), its
// sound as music. Tracks are subscribed by choice, not automatically: every
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
  ScreenSharePresets,
  Track,
  VideoPreset,
  createLocalAudioTrack,
  getLogger,
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
import type { Room, RoomEvents, ShareSink } from './connection';
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

// What the picker asks for: the picture at up to 1080p and 30 fps, the
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
    resolution: ScreenSharePresets.h1080fps30.resolution,
    contentHint: 'motion',
    systemAudio: 'include',
    selfBrowserSurface: 'exclude',
    surfaceSwitching: 'include',
    suppressLocalAudioPlayback: false,
  };
}

// The share's encodings (decision O6). The bitrates are where it starts;
// the bandwidth measurement of one sharer to 29 viewers (A15) refines them.
export const shareTop = new VideoPreset(1920, 1080, 4_000_000, 30);
export const shareLayers = [new VideoPreset(640, 360, 300_000, 5), new VideoPreset(1280, 720, 1_500_000, 15)];

export function videoPublishOptions(): TrackPublishOptions {
  return {
    source: Track.Source.ScreenShare,
    simulcast: true,
    screenShareEncoding: shareTop.encoding,
    screenShareSimulcastLayers: shareLayers,
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
  private deafened = false;
  private readonly elements = new Map<string, HTMLMediaElement>();
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
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack) => {
        // Nothing is played, or left in the page, after a close.
        if (this.closed) return;
        if (isShare(track.source)) {
          this.shared(track);
          return;
        }
        if (track.kind !== Track.Kind.Audio) return;
        const el = track.attach();
        el.muted = this.deafened;
        this.elements.set(track.sid ?? '', el);
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

  async publishMicrophone(): Promise<void> {
    this.ensureOpen();
    if (this.track !== null) return;
    const room = this.room;
    const track = await createLocalAudioTrack({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    // From here the microphone is open: any failure, or a close that came
    // while a step was pending, stops it before the error goes on.
    try {
      this.ensureOpen();
      this.track = track;
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

  private ensureOpen(): void {
    if (this.closed) throw new ClosedError();
  }

  // listen taps a clone of the microphone for the level meter and the voice
  // detection: the published track is muted while the gate is shut, and a
  // muted track carries silence.
  private listen(track: LocalAudioTrack): void {
    if (this.closed) return;
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

  async setSending(on: boolean): Promise<void> {
    this.wantSending = on;
    const pub = this.publication;
    if (pub === null) return;
    if (on) await pub.unmute();
    else await pub.mute();
  }

  setDeafened(on: boolean): void {
    this.deafened = on;
    for (const el of this.elements.values()) el.muted = on;
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
      const stereo = (track.mediaStreamTrack.getSettings().channelCount ?? 1) >= 2;
      const options = track.source === Track.Source.ScreenShareAudio ? audioPublishOptions(stereo) : videoPublishOptions();
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
    for (const el of this.elements.values()) el.remove();
    this.elements.clear();
    this.attachShare(null);
    this.shareVideo = null;
    this.shareAudio = null;
    const tap = this.tap;
    this.tap = null;
    if (tap !== null) {
      tap.clone.stop();
      await tap.context.close().catch(() => undefined);
    }
    const track = this.track;
    this.track = null;
    this.publication = null;
    // disconnect(true) stops published tracks; these may not have been
    // published yet.
    track?.stop();
    for (const t of this.screen) t.stop();
    this.screen = [];
    try {
      await this.room.disconnect(true);
    } finally {
      redactor.release(this);
    }
  }
}

export function newLiveKitRoom(): Room {
  return new LiveKitAdapter();
}
