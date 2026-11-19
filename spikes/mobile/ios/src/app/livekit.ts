// The room, on @livekit/react-native 3.1.0 and livekit-client 2.22.3: the
// RoomPort of controller.ts. This is the only file that imports the SDK's room
// (and logging.ts the SDK's loggers); the logic never sees it.
//
// What it follows from the browser's web/src/voice/sdk.ts:
//  - nothing is subscribed but by choice: wanted() (logic/subscribe.ts) says
//    yes to microphones, and to a camera only from a claim holder while the
//    Voice view is open and shown (camera/policy.ts, M5-D);
//  - the microphone is muted before it is published and the publication again
//    after (logic/mic.ts publishMicrophone, #501), and a mic unmuted by anyone
//    else while the gate is shut is muted again;
//  - a deafened phone plays nothing: remote audio tracks go to volume 0.
// What it does differently from Android: the SDK owns the AVAudioSession's
// category and activation (registerGlobals and AudioSession), and the Swift
// module (ConchVoice.swift) only observes it, so that an interruption and a
// route change reach the lifecycle without the two fighting over the category.
import { AudioSession } from '@livekit/react-native';
import {
  ConnectionState,
  DisconnectReason,
  LocalAudioTrack,
  type LocalTrackPublication,
  type Participant,
  type RemoteTrack,
  type RemoteTrackPublication,
  Room,
  RoomEvent,
  type RemoteVideoTrack,
  Track,
  TrackEvent,
  createLocalAudioTrack,
} from 'livekit-client';
import type { DisconnectCause } from '../logic/disconnect';
import { publishMicrophone, type Mutable } from '../logic/mic';
import type { TrackKind, TrackSource } from '../logic/subscribe';
import { LiveKitCamera } from './camera/capture';
import { noCameras, principalOf, wantTrack, type CameraWants } from './camera/policy';
import type { RoomEvents, RoomPort } from './controller';

// startAudio configures and starts the SDK's audio session: the call category
// and the output (speaker unless a headset or Bluetooth is attached).
export async function startAudio(): Promise<void> {
  await AudioSession.configureAudio({ ios: { defaultOutput: 'speaker' } });
  await AudioSession.startAudioSession();
}

export async function stopAudio(): Promise<void> {
  await AudioSession.stopAudioSession();
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

function sourceOf(source: Track.Source): TrackSource {
  switch (source) {
    case Track.Source.Microphone:
      return 'microphone';
    case Track.Source.Camera:
      return 'camera';
    case Track.Source.ScreenShare:
      return 'screen_share';
    case Track.Source.ScreenShareAudio:
      return 'screen_share_audio';
    default:
      return 'unknown';
  }
}

export interface LiveKitRoomOptions {
  // The local microphone track, when there is one, for the level probe.
  readonly onMicrophone: (track: LocalAudioTrack | null) => void;
  // The cameras received, by principal, whenever they change.
  readonly onCameras: (tracks: ReadonlyMap<number, RemoteVideoTrack>) => void;
}

export class LiveKitRoom implements RoomPort {
  private readonly room = new Room({ adaptiveStream: true, dynacast: true });
  private readonly options: LiveKitRoomOptions;
  private track: LocalAudioTrack | null = null;
  private publication: LocalTrackPublication | null = null;
  private wantSending = false;
  private deafened = false;
  private closed = false;
  private events: RoomEvents | null = null;
  readonly camera: LiveKitCamera;
  private wants: CameraWants = noCameras;
  private readonly cameras = new Map<number, RemoteVideoTrack>();

  constructor(options: LiveKitRoomOptions) {
    this.options = options;
    this.camera = new LiveKitCamera(this.room, () => this.events);
  }

  async connect(url: string, token: string, publish: boolean, events: RoomEvents): Promise<void> {
    this.events = events;
    await startAudio();
    const room = this.room;
    room
      .on(RoomEvent.Reconnecting, events.onReconnecting)
      .on(RoomEvent.Reconnected, () => {
        this.subscribeAll();
        events.onReconnected();
      })
      .on(RoomEvent.Disconnected, (reason?: DisconnectReason) => {
        events.onDisconnected(causeOf(reason));
      })
      .on(RoomEvent.TrackPublished, (pub: RemoteTrackPublication, participant: Participant) => {
        this.choose(pub, participant.identity);
      })
      .on(RoomEvent.TrackSubscribed, (track: RemoteTrack, pub: RemoteTrackPublication, participant: Participant) => {
        if (track.kind === Track.Kind.Audio) this.volume(track);
        else if (pub.source === Track.Source.Camera) this.cameraIn(track, participant.identity);
      })
      .on(RoomEvent.TrackUnsubscribed, (track: RemoteTrack, _pub: RemoteTrackPublication, participant: Participant) => {
        this.cameraOut(track, participant.identity);
      })
      .on(RoomEvent.TrackUnmuted, (pub: { source: Track.Source }, participant: Participant) => {
        // Unmuted by someone else (the room API) while the gate is shut: muted
        // again. An unmute of our own comes with the gate open.
        if (participant !== room.localParticipant || pub.source !== Track.Source.Microphone || this.wantSending) return;
        void this.publication?.mute().catch(() => undefined);
      });
    // Autosubscribe is off: tracks are asked for by wanted().
    await room.connect(url, token, { autoSubscribe: false });
    if (this.closed) return;
    this.subscribeAll();
    if (!publish) return;
    await this.publishMic();
  }

  private async publishMic(): Promise<void> {
    const captured = await createLocalAudioTrack({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    // A track the page provides: the SDK does not restart or recapture it behind
    // the gate's back.
    const track = new LocalAudioTrack(captured.mediaStreamTrack, captured.constraints, true);
    track.source = Track.Source.Microphone;
    try {
      if (this.closed) throw new Error('closed');
      this.track = track;
      track.on(TrackEvent.Ended, () => {
        if (this.closed || track !== this.track) return;
        this.track = null;
        this.publication = null;
        this.options.onMicrophone(null);
        track.stop();
        this.events?.onMicrophoneEnded();
      });
      const asMutable: Mutable = { mute: async () => void (await track.mute()) };
      const published = await publishMicrophone(asMutable, {
        publish: async () => {
          const raw = await this.room.localParticipant.publishTrack(track, { source: Track.Source.Microphone });
          return { raw, mute: async () => void (await raw.mute()) };
        },
      });
      if (this.closed) throw new Error('closed');
      this.publication = published.raw;
      this.options.onMicrophone(track);
    } catch (e) {
      this.track = null;
      this.publication = null;
      track.stop();
      throw e;
    }
  }

  async setSending(sending: boolean): Promise<void> {
    this.wantSending = sending;
    const track = this.track;
    const pub = this.publication;
    if (track === null || pub === null) return;
    if (sending) {
      await track.unmute();
      await pub.unmute();
    } else {
      await Promise.all([pub.mute(), track.mute()]);
    }
  }

  setDeafened(deafened: boolean): void {
    this.deafened = deafened;
    for (const p of this.room.remoteParticipants.values()) {
      for (const pub of p.audioTrackPublications.values()) {
        if (pub.track !== undefined) this.volume(pub.track);
      }
    }
  }

  async disconnect(): Promise<void> {
    this.closed = true;
    this.camera.close();
    this.cameras.clear();
    this.options.onCameras(new Map());
    this.options.onMicrophone(null);
    const track = this.track;
    this.track = null;
    this.publication = null;
    try {
      await this.room.disconnect(true);
    } finally {
      track?.stop();
      await stopAudio().catch(() => undefined);
    }
  }

  private volume(track: RemoteTrack): void {
    if (track.kind !== Track.Kind.Audio) return;
    (track as unknown as { setVolume: (v: number) => void }).setVolume(this.deafened ? 0 : 1);
  }

  private subscribeAll(): void {
    if (this.room.state === ConnectionState.Disconnected) return;
    for (const p of this.room.remoteParticipants.values()) {
      for (const pub of p.trackPublications.values()) this.choose(pub, p.identity);
    }
  }

  // watchCameras sets which cameras to receive and applies it to every track
  // already published: a camera no longer wanted is unsubscribed at once.
  watchCameras(wants: CameraWants): void {
    this.wants = wants;
    this.subscribeAll();
  }

  private cameraIn(track: RemoteTrack, identity: string): void {
    const principal = principalOf(identity);
    // Chosen again on what arrived: a track the policy no longer wants is let go.
    if (principal === null || track.kind !== Track.Kind.Video || !wantTrack('camera', 'video', identity, this.wants)) return;
    this.cameras.set(principal, track as RemoteVideoTrack);
    this.options.onCameras(new Map(this.cameras));
  }

  private cameraOut(track: RemoteTrack, identity: string): void {
    const principal = principalOf(identity);
    if (principal === null || this.cameras.get(principal) !== track) return;
    this.cameras.delete(principal);
    this.options.onCameras(new Map(this.cameras));
  }

  // choose subscribes to a remote track only if wanted: by its declared
  // source, its kind and its publisher together (camera/policy.ts).
  private choose(pub: RemoteTrackPublication, identity: string): void {
    const kind: TrackKind | undefined = pub.kind === Track.Kind.Audio ? 'audio' : pub.kind === Track.Kind.Video ? 'video' : undefined;
    const want = wantTrack(sourceOf(pub.source), kind, identity, this.wants);
    if (want !== pub.isSubscribed) pub.setSubscribed(want);
  }
}
