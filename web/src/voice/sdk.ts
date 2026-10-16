// The one file that imports the LiveKit SDK. It adapts the SDK's Room to the
// small Room interface of connection.ts, and quiets the SDK's logging.
//
// The microphone is captured here with echo cancellation, noise suppression and
// auto gain on, published once, and muted straight after publishing, as
// conch-voice.md section 3 does: the SDK enables a track when it publishes it,
// and again when it republishes after a long interruption, so the track is
// muted after each unless the gate is open. The client counts as connected only
// after that mute.
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
  setLogLevel,
  type LocalTrackPublication,
  type Participant,
  type RemoteTrack,
} from 'livekit-client';
import type { DisconnectCause } from './disconnect';
import type { Room, RoomEvents } from './connection';
import { LogRing, Redactor } from './redact';

// One redactor and one ring for the page: the SDK's loggers are global.
export const redactor = new Redactor();
export const sdkLog = new LogRing(redactor);

let logging = false;

// quietSdk sets every SDK logger to warn and sends its records, redacted, to
// the ring instead of the browser log. The level is not persisted.
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
  setLogLevel(LogLevel.warn);
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
  private publication: LocalTrackPublication | null = null;
  private tap: { readonly context: AudioContext; readonly analyser: AnalyserNode; readonly clone: MediaStreamTrack } | null = null;
  private wantSending = false;
  private deafened = false;
  private readonly elements = new Map<string, HTMLMediaElement>();
  private readonly buffer = new Uint8Array(1024);

  async connect(url: string, token: string, roomName: string, publish: boolean, events: RoomEvents): Promise<void> {
    quietSdk();
    redactor.hold(token, roomName);
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
        if (track.kind !== Track.Kind.Audio) return;
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
        if (pub.source === Track.Source.Microphone && !this.wantSending) void pub.mute();
      });
    await room.connect(url, token, { autoSubscribe: true });
    if (!publish) return;
    const track = await createLocalAudioTrack({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    this.publication = await room.localParticipant.publishTrack(track, { source: Track.Source.Microphone });
    await this.publication.mute();
    this.listen(track);
  }

  // listen taps a clone of the microphone for the level meter and the voice
  // detection: the published track is muted while the gate is shut, and a
  // muted track carries silence.
  private listen(track: LocalAudioTrack): void {
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
    this.wantSending = false;
    for (const el of this.elements.values()) el.remove();
    this.elements.clear();
    const tap = this.tap;
    this.tap = null;
    if (tap !== null) {
      tap.clone.stop();
      await tap.context.close().catch(() => undefined);
    }
    await this.room.disconnect(true);
    redactor.clear();
  }
}

export function newLiveKitRoom(): Room {
  return new LiveKitAdapter();
}
