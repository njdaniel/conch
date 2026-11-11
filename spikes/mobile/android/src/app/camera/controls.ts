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
//    nothing starts and the person taps again, with the permission now held.
//    While the prompt is up, the one 'background' it causes is expected; any
//    other lifecycle reason that begins before the answer (lock, an
//    interruption, a second 'background', or 'stopped') abandons the tap at
//    once, so an Allow tapped minutes later, after a lock or a call, starts
//    nothing. 'stopped' is the native activity stop (#1758): the prompt only
//    pauses the activity, so a stop is the person leaving (Home, Recents),
//    which AppState does not report a second time. After
//    the answer, any reason that begins abandons it too. The wall-clock wait
//    measures from the answer (9515c1f5: the freeze that mattered was after
//    the grant). The prompt's 'background' is expected to reach JavaScript
//    before the answer, since the activity pauses before the dialog can be
//    answered (unverified on a device). If it came only after, the capture may
//    start and the lifecycle stops it at once: it fails shut, and the person
//    taps again.
//    "In time" is wall-clock time: React Native pauses its JS timers in the
//    background, so counting ticks would let a tap granted just before a call
//    start the camera minutes later, on resume, with no new tap. The tap is
//    also abandoned if a lifecycle block begins after the answer, and on
//    stop, loss or leave;
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
  // Wall-clock milliseconds (Date.now); a test freezes or jumps it.
  readonly now: () => number;
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
  // The permission has been answered; the prompt's 'background' has been seen.
  private answered = false;
  private backgroundSeen = false;
  // Counts the stops, losses and leaves; a tap in flight that sees it change
  // is abandoned.
  private cancels = 0;
  // Counts the lifecycle blocks that have begun (blockBegan).
  private blocks = 0;
  // The tap generation a lifecycle block abandoned (N1): if its permission then
  // comes back granted, the person is told to tap again, not left with nothing.
  private blockCancelled = -1;

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
    // The button reads "Turn camera off" while a tap is pending: a tap now
    // cancels it.
    if (this.asking) {
      this.cancelTap();
      return;
    }
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
    this.answered = false;
    this.backgroundSeen = false;
    this.note = null;
    const gen = this.cancels;
    this.onChange();
    let allowed: boolean;
    try {
      allowed = await this.ports.permission();
    } catch {
      allowed = false;
    }
    // Stopped, lost or left while the prompt was up: nothing more happens.
    if (gen !== this.cancels) {
      if (allowed && this.blockCancelled === gen) this.say(tapAgain);
      return;
    }
    if (!allowed) {
      this.asking = false;
      this.client.fail('blocked');
      this.onChange();
      return;
    }
    // The prompt's own leaving of the foreground began before the answer; a
    // block that begins after it is a new reason to stay off.
    this.answered = true;
    const blocks = this.blocks;
    const foreground = await this.waitForeground(gen, blocks);
    if (gen !== this.cancels) return;
    this.asking = false;
    if (!foreground) {
      this.say(tapAgain);
      return;
    }
    await this.client.start(channel, room, null, 'phone');
  }

  // blockBegan is the controller saying a lifecycle reason has begun. After the
  // answer, a tap still waiting for the foreground is abandoned. Before it, the
  // permission prompt's own 'background' (the first, once) is expected; any
  // other reason abandons the tap now.
  blockBegan(reason: string): void {
    this.blocks++;
    if (!this.asking || this.answered) return;
    if (reason === 'background' && !this.backgroundSeen) {
      this.backgroundSeen = true;
      return;
    }
    this.blockCancelled = this.cancels;
    this.cancelTap();
  }

  // abandonTap abandons a tap in flight: the connection is reconnecting, so
  // nothing may start on it.
  abandonTap(): void {
    this.cancelTap();
  }

  // cancelTap abandons a tap in flight (the permission prompt, or the wait
  // for the foreground).
  private cancelTap(): void {
    this.cancels++;
    if (this.asking) {
      this.asking = false;
      this.onChange();
    }
  }

  private say(note: string): void {
    this.note = note;
    this.onChange();
  }

  // waitForeground resolves true once no lifecycle reason holds, within
  // foregroundWait of wall-clock time; false if the time is up (checked first,
  // so a resume after a long freeze of the timers is too late), if a block has
  // begun since the answer, or if the tap was cancelled.
  private waitForeground(gen: number, blocks: number): Promise<boolean> {
    const deadline = this.ports.now() + foregroundWait;
    const done = (): boolean | null => {
      if (gen !== this.cancels || blocks !== this.blocks) return false;
      if (this.ports.now() >= deadline) return false;
      return this.ports.blocked() ? null : true;
    };
    const first = done();
    if (first !== null) return Promise.resolve(first);
    return new Promise((resolve) => {
      const look = (): void => {
        const answer = done();
        if (answer !== null) {
          resolve(answer);
          return;
        }
        this.ports.clock.setTimeout(look, foregroundStep);
      };
      this.ports.clock.setTimeout(look, foregroundStep);
    });
  }

  // ---- what the controller tells the camera (all of camera.ts's, by name)

  stop(cause: 'self' | 'left' | 'lost'): Promise<void> {
    this.cancelTap();
    return this.client.stop(cause);
  }

  ended(cause: 'device' | 'server' | 'not_connected'): void {
    this.client.ended(cause);
  }

  lost(loss: CameraLoss): void {
    this.cancelTap();
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
    this.cancelTap();
    const hadNote = this.note !== null;
    this.note = null;
    this.client.reset();
    // A note cleared while the camera state is unchanged still changes what
    // the screen says (cameraExtra), so the controller is told.
    if (hadNote) this.onChange();
  }
}

export { cameraOff };
