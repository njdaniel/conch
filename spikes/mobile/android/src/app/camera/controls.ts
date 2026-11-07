// The phone's camera, as the controller drives it (#1109 M4-D; ADR-016
// Decision 10; docs/design/webcam-video.md §3.4, §4.2, §6.2, §7.2 to §7.4).
//
// This is the thin layer over logic/camera.ts's CameraClient, which owns the
// order (capture, claim, wait for the grant, publish), the named continue,
// stale and ended handling, and the stop. No React and no SDK is imported
// here, so a test runs it with fakes.
//
// What it adds, and nothing more:
//  - the button: a tap turns the camera on, or off. The CAMERA permission is
//    asked at the first tap and never at launch or at Join (M-C7). Android
//    shows the prompt in front of the activity, which the lifecycle sees as
//    leaving the foreground; so after a grant the capture starts once the
//    foreground is back (waitForeground), and if it does not come in time
//    nothing starts and the person taps again, with the permission now held;
//  - the camera is the phone's: front-facing first (capture.ts), 640x360;
//  - nothing turns the camera back on by itself: no path here calls start
//    but the tap (M-C6);
//  - the lifecycle (logic/lifecycle.ts) calls client.lifecycleBlock directly.
import type { CameraClaimResult } from '../../logic/api';
import { CameraClient, cameraOff, cameraOn, listenOnlyCamera, type CameraLoss, type CameraRoom, type CameraState } from '../../logic/camera';
import type { Clock } from '../../logic/clock';
import type { VoiceCameraEnded } from '../../logic/schema/generated';

export interface CameraApiPort {
  claimCamera(channel: string, startedAt?: string): Promise<CameraClaimResult>;
  releaseCamera(channel: string, startedAt: string): Promise<unknown>;
}

export interface CameraPorts {
  readonly api: CameraApiPort;
  // Asks for the CAMERA permission (an Android prompt the first time).
  readonly permission: () => Promise<boolean>;
  // Whether a lifecycle reason now holds the camera off.
  readonly blocked: () => boolean;
  readonly clock: Clock;
}

// How long the tap waits, after the permission prompt closes, for the app to
// be back in the foreground before it gives up and asks for another tap.
export const foregroundWait = 2000;
const foregroundStep = 100;

export const tapAgain = 'The camera is allowed. Tap the camera button again to turn it on.';

export class CameraControls {
  readonly client: CameraClient;
  private readonly ports: CameraPorts;
  private readonly onChange: () => void;
  private note: string | null = null;
  private asking = false;

  constructor(ports: CameraPorts, onChange: () => void) {
    this.ports = ports;
    this.onChange = onChange;
    this.client = new CameraClient(
      {
        claim: (channel, startedAt) => ports.api.claimCamera(channel, startedAt),
        release: (channel, startedAt) => ports.api.releaseCamera(channel, startedAt),
      },
      () => {
        this.note = null;
        this.onChange();
      },
      ports.clock,
    );
  }

  get state(): CameraState {
    return this.client.current;
  }

  get on(): boolean {
    return cameraOn(this.client.current) || this.asking;
  }

  // The words for the person: an extra note of this layer, else the state's.
  get extra(): string | null {
    return this.note;
  }

  // tap is the camera button. Off: ask for the permission if it is not held,
  // then start. On: stop. It is the only way the camera starts.
  async tap(channel: string, room: CameraRoom, listenOnly: boolean): Promise<void> {
    if (this.asking) return;
    if (cameraOn(this.client.current)) {
      await this.client.stop('self');
      return;
    }
    if (listenOnly) {
      this.say(listenOnlyCamera);
      return;
    }
    if (this.ports.blocked()) return;
    this.asking = true;
    this.note = null;
    this.onChange();
    let allowed: boolean;
    try {
      allowed = await this.ports.permission();
    } catch {
      allowed = false;
    }
    if (!allowed) {
      this.asking = false;
      this.client.fail('blocked');
      this.onChange();
      return;
    }
    const foreground = await this.waitForeground();
    this.asking = false;
    if (!foreground) {
      this.say(tapAgain);
      return;
    }
    await this.client.start(channel, room, null, 'phone');
  }

  private say(note: string): void {
    this.note = note;
    this.onChange();
  }

  // waitForeground resolves true once no lifecycle reason holds, within
  // foregroundWait.
  private waitForeground(): Promise<boolean> {
    if (!this.ports.blocked()) return Promise.resolve(true);
    return new Promise((resolve) => {
      let waited = 0;
      const look = (): void => {
        if (!this.ports.blocked()) {
          resolve(true);
          return;
        }
        waited += foregroundStep;
        if (waited >= foregroundWait) {
          resolve(false);
          return;
        }
        this.ports.clock.setTimeout(look, foregroundStep);
      };
      this.ports.clock.setTimeout(look, foregroundStep);
    });
  }

  // ---- what the controller tells the camera (all of camera.ts's, by name)

  stop(cause: 'self' | 'left' | 'lost'): Promise<void> {
    return this.client.stop(cause);
  }

  ended(cause: 'device' | 'server' | 'not_connected'): void {
    this.client.ended(cause);
  }

  lost(loss: CameraLoss): void {
    this.client.lost(loss);
  }

  reconnected(): Promise<void> {
    return this.client.reconnected();
  }

  connected(channel: string, room: CameraRoom): void {
    this.client.connected(channel, room);
  }

  presence(ended: VoiceCameraEnded | undefined): void {
    this.client.presence(ended);
  }

  refused(): void {
    this.client.refused();
  }

  // reset forgets why the last camera ended, at a new join.
  reset(): void {
    this.note = null;
    this.client.reset();
  }
}

export { cameraOff };
