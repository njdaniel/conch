// The lifecycle bridge: the OS's signals as the lifecycle's inputs
// (logic/lifecycle.ts, logic/ios.ts). Two sources feed it:
//  - React Native's AppState: 'active', 'background' and 'inactive' (Control
//    Center, a call banner, the lock gesture), each a lifecycle reason;
//  - the Swift module's events (ConchVoice.swift): AVAudioSession's
//    interruption notification (began / ended) and route-change notification.
// The OS's own "should resume" hint is not an input: an interruption ending
// only lifts the block, and nothing opens (the gate's resume latch and a fresh
// press). The hint is passed through for the receipt and read by no decision.
import type { AppState } from '../logic/lifecycle';
import type { InterruptionEvent, RouteChangeEvent, RouteChangeReason } from '../logic/ios';

// What the native module emits.
export interface NativeEvent {
  readonly type: string;
  readonly state: string;
  readonly detail: string;
}

// What the bridge drives; the VoiceController implements it.
export interface LifecycleSink {
  appState(state: AppState): void;
  interruption(event: InterruptionEvent): void;
  routeChange(event: RouteChangeEvent, route: string): void;
}

// appStateOf maps React Native's AppState string; anything it does not know is
// treated as leaving the foreground, the safe side.
export function appStateOf(state: string): AppState {
  if (state === 'active') return 'active';
  if (state === 'inactive') return 'inactive';
  return 'background';
}

const reasons: readonly RouteChangeReason[] = [
  'unknown',
  'newDeviceAvailable',
  'oldDeviceUnavailable',
  'categoryChange',
  'override',
  'wakeFromSleep',
  'noSuitableRouteForCategory',
  'routeConfigurationChange',
];

// reasonOf: a reason the module sends that the logic does not name is
// 'unknown', which still shuts a held press (the safe side).
export function reasonOf(name: string): RouteChangeReason {
  return reasons.find((r) => r === name) ?? 'unknown';
}

// deliver hands one native event to the sink. It returns a line for the
// diagnostics (the kind and state, never a device name).
export function deliver(sink: LifecycleSink, event: NativeEvent): string {
  switch (event.type) {
    case 'interruption':
      // state: 'began' | 'ended'; detail: 'shouldResume' or '' on an end.
      if (event.state === 'began') sink.interruption({ type: 'began' });
      else if (event.state === 'ended') sink.interruption({ type: 'ended', shouldResume: event.detail === 'shouldResume' });
      break;
    case 'route':
      // state: the reason; detail: the output port types, e.g. "Speaker".
      sink.routeChange({ reason: reasonOf(event.state) }, event.detail);
      break;
  }
  return `native ${event.type}: ${event.state}`;
}
