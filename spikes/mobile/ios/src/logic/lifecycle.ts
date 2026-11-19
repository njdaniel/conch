// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
// The lifecycle: the moments the OS takes the phone away from the person, and
// what each does to the transmit gate and the camera (ADR-016 Decisions 5 and
// 10). Pure; the app (M4-C; M5-C on iOS) feeds it from AppState, the screen-off broadcast
// and audio focus.
//
// Background, the iOS inactive state (Control Center, a call banner), screen
// lock and an audio interruption each shut the gate in both modes with no
// tail and stop the camera, released naming its claim. When the last of them
// ends the blocks lift, and nothing opens: the gate needs a new press or, in
// open mode, quiet and then an onset (the resume latch); the camera needs the
// button. The OS's own "should resume" hint is not a reason to open anything.
import type { CameraClient } from './camera';
import type { TransmitGate } from './gate';

export type LifecycleReason = 'background' | 'inactive' | 'lock' | 'interruption';

export type AppState = 'active' | 'background' | 'inactive';

export class Lifecycle {
  private readonly gate: Pick<TransmitGate, 'lifecycleBlock' | 'lifecycleUnblock'>;
  private readonly camera: Pick<CameraClient, 'lifecycleBlock' | 'lifecycleUnblock'>;
  private readonly active = new Set<LifecycleReason>();
  private pending: Promise<void> = Promise.resolve();

  constructor(gate: Pick<TransmitGate, 'lifecycleBlock' | 'lifecycleUnblock'>, camera: Pick<CameraClient, 'lifecycleBlock' | 'lifecycleUnblock'>) {
    this.gate = gate;
    this.camera = camera;
  }

  get reasons(): readonly LifecycleReason[] {
    return [...this.active];
  }

  get blocked(): boolean {
    return this.active.size > 0;
  }

  // appState is the app's foreground state: active lifts background and
  // inactive; the others set theirs (and clear the other).
  appState(state: AppState): void {
    this.set('background', state === 'background');
    this.set('inactive', state === 'inactive');
  }

  screenLocked(locked: boolean): void {
    this.set('lock', locked);
  }

  // interrupted is an audio interruption (a call, the assistant, another app
  // taking the audio). Its end, whatever the OS says about resuming, only
  // lifts the block: the OS hint is ignored by design.
  interrupted(interrupted: boolean): void {
    this.set('interruption', interrupted);
  }

  // settled resolves once the camera's stop and release, started by the last
  // event, are done.
  settled(): Promise<void> {
    return this.pending;
  }

  private set(reason: LifecycleReason, on: boolean): void {
    const had = this.active.has(reason);
    if (on === had) return;
    if (on) this.active.add(reason);
    else this.active.delete(reason);
    if (on) {
      // The gate first and the camera second, both before anything awaits.
      this.gate.lifecycleBlock();
      this.pending = this.camera.lifecycleBlock();
    } else if (this.active.size === 0) {
      this.gate.lifecycleUnblock();
      this.camera.lifecycleUnblock();
    }
  }
}
