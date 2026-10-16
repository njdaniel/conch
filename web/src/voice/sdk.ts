// The one file that imports the LiveKit SDK. It adapts the SDK's Room to the
// small Room interface of connection.ts, and quiets the SDK's logging.
//
// The microphone is captured here with echo cancellation, noise suppression and
// auto gain on, published once, and muted straight after publishing, as
// conch-voice.md section 3 does: the SDK enables a track when it publishes it,
// and again when it republishes after a long interruption, so the track is
// muted after each unless the gate is open. The client counts as connected only
// after that mute. A remote unmute (LiveKit's room API can ask for one) is
// undone the same way while the gate is shut.
//
// Leave can come at any point of a connection. Once the adapter is closed,
// every step that resumes after an await stops, and a microphone captured or
// published after the close is stopped, so nothing keeps the device open.
import {
  DisconnectReason,
  LocalAudioTrack,
  LogLevel,
  LoggerNames,
  Room as LiveKitRoom,
  RoomEvent,
  Track,
  createLocalAudioTrack,
  getLogger,
  type LocalTrackPublication,
  type Participant,
  type RemoteTrack,
  type TrackPublication,
} from 'livekit-client';
import type { DisconnectCause } from './disconnect';
import type { Room, RoomEvents } from './connection';
import { LogRing, Redactor } from './redact';

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

// Thrown by a connection step that resumes after the adapter was closed.
class ClosedError extends Error {
  constructor() {
    super('voice connection closed');
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

// principalOf reads the principal id out of an identity p<id>.
export function principalOf(identity: string): number | null {
  const m = /^p(\d+)$/.exec(identity);
  return m === null || m[1] === undefined ? null : Number(m[1]);
}

class LiveKitAdapter implements Room {
  private readonly room = new LiveKitRoom({ adaptiveStream: false, dynacast: false });
  // Set by close(); every step after an await checks it.
  private closed = false;
  private track: LocalAudioTrack | null = null;
  private publication: LocalTrackPublication | null = null;
  private tap: { readonly context: AudioContext; readonly analyser: AnalyserNode; readonly clone: MediaStreamTrack } | null = null;
  private wantSending = false;
  private deafened = false;
  private readonly elements = new Map<string, HTMLMediaElement>();
  private readonly buffer = new Uint8Array(1024);

  async connect(url: string, token: string, roomName: string, publish: boolean, events: RoomEvents): Promise<void> {
    this.ensureOpen();
    // Held for this adapter alone: its close() releases these and no other
    // connection's.
    redactor.hold(this, token, roomName);
    const room = this.room;
    room
      .on(RoomEvent.Reconnecting, events.onReconnecting)
      .on(RoomEvent.Reconnected, () => {
        events.onReconnected();
      })
      .on(RoomEvent.Disconnected, (reason?: DisconnectReason) => {
        events.onDisconnected(causeOf(reason));
      })
      .on(RoomEvent.ActiveSpeakersChanged, (speakers: Participant[]) => {
        const ids = speakers.map((p) => principalOf(p.identity)).filter((n): n is number => n !== null);
        events.onSpeakers(ids);
      })
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack) => {
        // Nothing is played, or left in the page, after a close.
        if (track.kind !== Track.Kind.Audio || this.closed) return;
        const el = track.attach();
        el.muted = this.deafened;
        this.elements.set(track.sid ?? '', el);
        document.body.appendChild(el);
      })
      .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack) => {
        for (const el of track.detach()) el.remove();
        this.elements.delete(track.sid ?? '');
      })
      .on(RoomEvent.LocalTrackPublished, (pub: LocalTrackPublication) => {
        // Published again by the SDK after a full reconnect: muted again.
        if (pub.source === Track.Source.Microphone && !this.wantSending) void pub.mute().catch(() => undefined);
      })
      .on(RoomEvent.TrackUnmuted, (pub: TrackPublication, participant: Participant) => {
        // Unmuted by someone else (the room API) while the gate is shut: muted
        // again. An unmute of our own comes with the gate open.
        if (participant !== room.localParticipant || pub.source !== Track.Source.Microphone || this.wantSending) return;
        void (pub as LocalTrackPublication).mute().catch(() => undefined);
      });
    await room.connect(url, token, { autoSubscribe: true });
    this.ensureOpen();
    if (!publish) return;
    const track = await createLocalAudioTrack({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    // From here the microphone is open: any failure, or a close that came
    // while a step was pending, stops it before the error goes on.
    try {
      this.ensureOpen();
      this.track = track;
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

  async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    this.wantSending = false;
    for (const el of this.elements.values()) el.remove();
    this.elements.clear();
    const tap = this.tap;
    this.tap = null;
    if (tap !== null) {
      tap.clone.stop();
      await tap.context.close().catch(() => undefined);
    }
    const track = this.track;
    this.track = null;
    this.publication = null;
    // disconnect(true) stops published tracks; this one may not have been
    // published yet.
    track?.stop();
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
