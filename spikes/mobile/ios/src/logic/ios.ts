// The iOS audio events as the spike's logic sees them (#1394, M5-B; ADR-016
// Decisions 5 and 10, design section 4.1). Pure; the app (M5-C) feeds it from
// AVAudioSession's interruption and route-change notifications and from
// AppState, and nothing here imports React Native.
//
//  - AppState 'inactive' (Control Center, a call banner) is a lifecycle reason
//    of its own: the gate shuts in both modes with no tail and the camera
//    claim ends, exactly as for background.
//  - An interruption ends with a "should resume" option set or not. The hint
//    is read and thrown away: the end of an interruption only lifts its
//    block, and nothing opens (the gate needs a new press or, in open mode,
//    quiet and an onset; the camera needs the button).
//  - A route change while a press is held (a headset unplugged, the
//    Bluetooth device gone) shuts the gate at once, with no tail, and arms
//    the resume latch, so the held press does not carry on into the new
//    route: a new press opens. A route change settles no lifecycle reason and
//    leaves the camera alone (it carries no microphone).
import type { Lifecycle } from './lifecycle';
import type { TransmitGate } from './gate';

// AVAudioSession.InterruptionType.
export type InterruptionType = 'began' | 'ended';

export interface InterruptionEvent {
  readonly type: InterruptionType;
  // AVAudioSession.InterruptionOptions.shouldResume on an 'ended' event. It is
  // recorded for the receipt and acted on never.
  readonly shouldResume?: boolean;
}

// AVAudioSession.RouteChangeReason, the cases the spike names.
export type RouteChangeReason =
  | 'unknown'
  | 'newDeviceAvailable'
  | 'oldDeviceUnavailable'
  | 'categoryChange'
  | 'override'
  | 'wakeFromSleep'
  | 'noSuitableRouteForCategory'
  | 'routeConfigurationChange';

export interface RouteChangeEvent {
  readonly reason: RouteChangeReason;
}

export class IosAudioEvents {
  private readonly lifecycle: Pick<Lifecycle, 'interrupted' | 'blocked'>;
  private readonly gate: Pick<TransmitGate, 'lifecycleBlock' | 'lifecycleUnblock'>;
  // What iOS delivered, for the receipt (#1110: it records what iOS actually
  // delivered).
  readonly delivered: string[] = [];

  constructor(lifecycle: Pick<Lifecycle, 'interrupted' | 'blocked'>, gate: Pick<TransmitGate, 'lifecycleBlock' | 'lifecycleUnblock'>) {
    this.lifecycle = lifecycle;
    this.gate = gate;
  }

  interruption(event: InterruptionEvent): void {
    this.delivered.push(event.type === 'ended' ? `interruption ended shouldResume=${event.shouldResume === true}` : 'interruption began');
    // shouldResume is not read for any decision.
    this.lifecycle.interrupted(event.type === 'began');
  }

  routeChange(event: RouteChangeEvent): void {
    this.delivered.push(`route change ${event.reason}`);
    // A lifecycle block already shuts the gate and must not be lifted by this.
    if (this.lifecycle.blocked) return;
    // Block then unblock: shut at once, no tail, latch armed, nothing opened.
    this.gate.lifecycleBlock();
    this.gate.lifecycleUnblock();
  }
}
