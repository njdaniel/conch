// Copied from spikes/mobile/android at 9515c1f5 (#1392, M4-D, with its review fixes) for the iOS spike (#1396, M5-D).
// The phone's camera on LiveKit's React Native SDK: the CameraRoom of
// logic/camera.ts, the one place the camera is captured, held and published.
// Adapted from web/src/voice/sdk.ts (captureCameraTrack, awaitCameraGrant,
// publishGranted, stopCamera, keepCamera, adoptCamera). Differences: the
// phone profile only; no self view, no camera choice or switch (the front
// camera is asked for first and the person never picks another, so
// switchCamera is refused); no timers but the SDK's.
//
// The camera is published only once LiveKit has told this connection that the
// claim granted it (#525); the join token allows the microphone alone. A
// capture is stopped before anything is awaited (stopCamera), so the camera
// indicator is off and the 1 s lifecycle bar holds even if signalling hangs.
//
// iOS: the front camera is asked for first as on Android. iOS itself also
// interrupts capture in the background; the app does not rely on that, it stops
// the capture on 'inactive' and 'background' before iOS does, and the receipt
// records what iOS did on its own (#1110 point 5).
import {
  LocalVideoTrack,
  type LocalTrackPublication,
  type Participant,
  PublishTrackError,
  type Room,
  RoomEvent,
  Track,
  TrackEvent,
  type TrackPublishOptions,
  type VideoCaptureOptions,
  VideoPreset,
  createLocalVideoTrack,
} from 'livekit-client';
import type { CameraProfile, CameraRoom, KeptCamera } from '../../logic/camera';

// 640x360 at 24 fps, an ideal; with none chosen, the front camera first
// (facingMode user is an ideal, never exact). Design §7.5, O11.
export const phoneCameraAsk = { width: 640, height: 360, frameRate: 24 } as const;

export function cameraOptions(choice: string | null): VideoCaptureOptions {
  const resolution = { ...phoneCameraAsk };
  if (choice !== null) return { resolution, deviceId: { exact: choice } };
  return { resolution, facingMode: 'user' };
}

// A top layer of 360p at most 450 kbps, one simulcast layer of 180p at
// 150 kbps, VP8 (design §7.5, O11).
export function cameraPublishOptions(): TrackPublishOptions {
  return {
    source: Track.Source.Camera,
    simulcast: true,
    videoCodec: 'vp8',
    videoEncoding: { maxBitrate: 450_000, maxFramerate: phoneCameraAsk.frameRate },
    videoSimulcastLayers: [new VideoPreset(320, 180, 150_000, 15)],
  };
}

// How long a publish waits for LiveKit to say the claim granted the camera.
export const grantWait = 10_000;
// LiveKit's own number for the camera source in a participant's permission
// (livekit.TrackSource.CAMERA), which the SDK does not export.
const protoCamera = 1;

class ClosedError extends Error {
  constructor() {
    super('voice connection closed');
  }
}

class TakenError extends Error {
  constructor() {
    super('camera taken off this connection');
  }
}

// A capture between two connections: still capturing, blanked, published
// nowhere. The phone keeps one only across a full reconnect in the
// foreground; a lifecycle event never keeps it.
class Kept implements KeptCamera {
  readonly track: LocalVideoTrack;
  onEnded: (() => void) | null = null;

  constructor(track: LocalVideoTrack) {
    this.track = track;
    track.on(TrackEvent.Ended, this.ended);
  }

  private readonly ended = (): void => {
    const run = this.onEnded;
    this.onEnded = null;
    run?.();
  };

  live = (): boolean => this.track.mediaStreamTrack.readyState === 'live';

  stop = (): void => {
    this.forget();
    this.track.stop();
  };

  forget = (): void => {
    this.onEnded = null;
    this.track.off(TrackEvent.Ended, this.ended);
  };
}

export interface CameraEvents {
  // The camera is gone: the device ended the capture, or LiveKit took the
  // source away.
  readonly onCameraEnded: (cause: 'device' | 'server') => void;
}

export class LiveKitCamera implements CameraRoom {
  private readonly room: Room;
  private readonly events: () => CameraEvents | null;
  private track: LocalVideoTrack | null = null;
  // The tracks this adapter is taking down (stop or keep), each until its own
  // unpublish settles: the SDK's events for them are not the server taking the
  // camera or the device ending it. Per track, so one stop finishing cannot
  // clear another's, and a new capture is never taken for a stopping one.
  private readonly stopping = new Set<object>();
  private closed = false;
  private turn = 0;
  private waiters: (() => void)[] = [];
  private forgetEnded: () => void = () => undefined;
  private readonly kept = new WeakSet<LocalVideoTrack>();

  constructor(room: Room, events: () => CameraEvents | null) {
    this.room = room;
    this.events = events;
    room
      .on(RoomEvent.ParticipantPermissionsChanged, (_prev: unknown, participant: Participant) => {
        if (participant === room.localParticipant) this.wake();
      })
      .on(RoomEvent.LocalTrackUnpublished, (pub: LocalTrackPublication) => {
        // The source went without this adapter taking it down: LiveKit took it
        // away. The SDK also unpublishes every track as a connection ends, just
        // before it says so; the cause is read once that is over.
        if (pub.source !== Track.Source.Camera || this.closed) return;
        const track = pub.track;
        // A publication that names no track, while one is being taken down, is
        // that one.
        if (track === undefined ? this.stopping.size > 0 : this.stopping.has(track)) return;
        if (track !== undefined && track !== this.track) return;
        setTimeout(() => {
          if (this.closed || this.room.state === 'disconnected') return;
          if (track !== undefined && track !== this.track) return;
          this.events()?.onCameraEnded('server');
        }, 0);
      });
  }

  // close marks the connection over: nothing more is captured or published.
  close(): void {
    this.closed = true;
    this.wake();
  }

  private open(): void {
    if (this.closed) throw new ClosedError();
  }

  private wake(): void {
    const waiters = this.waiters;
    this.waiters = [];
    for (const w of waiters) w();
  }

  async captureCamera(choice: string | null, _profile?: CameraProfile): Promise<void> {
    this.open();
    await this.stopCamera();
    this.open();
    const turn = ++this.turn;
    const captured = await createLocalVideoTrack(cameraOptions(choice));
    const track = new LocalVideoTrack(captured.mediaStreamTrack, captured.constraints, true);
    track.source = Track.Source.Camera;
    if (this.closed) {
      track.stop();
      throw new ClosedError();
    }
    // A stop or a newer capture came while the OS was asked.
    if (turn !== this.turn || this.track !== null) {
      track.stop();
      throw new TakenError();
    }
    this.hold(track);
  }

  private hold(track: LocalVideoTrack): void {
    this.track = track;
    this.forgetEnded();
    const ended = (): void => {
      // Stopped or kept by this adapter: a stop by script fires no ended
      // event, and a kept capture is the next connection's.
      if (this.track !== track || this.stopping.has(track) || this.closed) return;
      this.events()?.onCameraEnded('device');
    };
    track.on(TrackEvent.Ended, ended);
    this.forgetEnded = () => {
      track.off(TrackEvent.Ended, ended);
      this.forgetEnded = () => undefined;
    };
  }

  private granted(): boolean {
    const p = this.room.localParticipant.permissions;
    if (p === undefined || !p.canPublish) return false;
    const sources: readonly number[] = p.canPublishSources;
    return sources.length === 0 || sources.includes(protoCamera);
  }

  async awaitCameraGrant(): Promise<void> {
    this.open();
    const track = this.track;
    const deadline = Date.now() + grantWait;
    for (;;) {
      if (track === null || this.track !== track) throw new TakenError();
      if (this.granted()) return;
      const changed = new Promise<void>((resolve) => {
        this.waiters.push(resolve);
      });
      const left = deadline - Date.now();
      if (left <= 0 || !(await within(changed, left))) throw new Error('voice: the camera was not granted in time');
      this.open();
    }
  }

  async publishCamera(): Promise<void> {
    this.open();
    const track = this.track;
    if (track === null) throw new TakenError();
    const deadline = Date.now() + grantWait;
    for (;;) {
      if (this.track !== track) throw new TakenError();
      // Listened for before publishing: the grant may come while the SDK is
      // getting there.
      const changed = new Promise<void>((resolve) => {
        this.waiters.push(resolve);
      });
      try {
        await this.room.localParticipant.publishTrack(track, cameraPublishOptions());
      } catch (err) {
        if (!(err instanceof PublishTrackError) || err.status !== 403) throw err;
        const left = deadline - Date.now();
        if (left <= 0 || !(await within(changed, left))) throw err;
        this.open();
        continue;
      }
      // Taken while the publish was on its way: taken down here.
      if (this.track !== track) {
        await this.room.localParticipant.unpublishTrack(track, false).catch(() => undefined);
        throw new TakenError();
      }
      return;
    }
  }

  async stopCamera(): Promise<void> {
    this.turn++;
    const track = this.track;
    this.track = null;
    this.wake();
    this.forgetEnded();
    if (track === null) return;
    this.stopping.add(track);
    // The capture stops first, at once, synchronously: the device light goes
    // off even if the signalling connection hangs and the unpublish below
    // never answers.
    track.stop();
    // The unpublish is not waited for. In the background React Native pauses
    // its JS timers, so the logic's bounded wait for a hung unpublish
    // (camera.ts unpublishWait) would never end and the claim's release would
    // wait for the app to come back to the foreground (found on the emulator
    // dry run). conchd's release takes the source away from LiveKit in any
    // case; this track stays in `stopping` until its own unpublish settles so the
    // SDK's unpublish is not reported as the server taking the camera.
    void this.room.localParticipant
      .unpublishTrack(track, true)
      .catch(() => undefined)
      .finally(() => {
        this.stopping.delete(track);
      });
  }

  keepCamera(): KeptCamera | null {
    this.turn++;
    const track = this.track;
    this.track = null;
    this.wake();
    this.forgetEnded();
    if (track === null) return null;
    this.kept.add(track);
    // Blanked at once: black frames until the next connection adopts it.
    track.mediaStreamTrack.enabled = false;
    this.stopping.add(track);
    void this.room.localParticipant
      .unpublishTrack(track, false)
      .catch(() => undefined)
      .finally(() => {
        this.stopping.delete(track);
      });
    return new Kept(track);
  }

  adoptCamera(kept: KeptCamera): void {
    this.open();
    this.turn++;
    if (!(kept instanceof Kept)) throw new Error('voice: not a camera this phone kept');
    kept.forget();
    kept.track.mediaStreamTrack.enabled = true;
    this.hold(kept.track);
  }

  cameraInUse(): string | null {
    const track = this.track;
    if (track === null || track.mediaStreamTrack.readyState !== 'live') return null;
    return track.mediaStreamTrack.getSettings().deviceId ?? null;
  }

  // The phone never chooses a camera, so there is nothing to switch to.
  switchCamera(_choice: string): Promise<void> {
    return Promise.reject(new Error('voice: the phone does not switch cameras'));
  }
}

function within(done: Promise<void>, ms: number): Promise<boolean> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const late = new Promise<boolean>((resolve) => {
    timer = setTimeout(() => {
      resolve(false);
    }, ms);
  });
  return Promise.race([done.then(() => true), late]).finally(() => {
    clearTimeout(timer);
  });
}
