// The lifecycle bridge: the OS's signals as the lifecycle's inputs
// (logic/lifecycle.ts). Three sources feed it:
//  - React Native's AppState: 'active', 'background', and 'inactive' (which
//    Android does not send, but the logic handles for iOS);
//  - the native module's "lock" events (screen off / the person got past the
//    keyguard);
//  - its "focus" events (audio focus lost or gained: a call, the assistant,
//    another app) and "route" events (an audio device came or went).
// The OS's own "should resume" hint is not an input: a focus gain only lifts
// the block, and nothing opens (the gate's resume latch and a fresh press).
import type { AppState } from '../logic/lifecycle';

// What the native module emits (ConchVoiceModule.kt).
export interface NativeEvent {
  readonly type: string;
  readonly state: string;
  readonly detail: string;
}

// What the bridge drives; the VoiceController implements it.
export interface LifecycleSink {
  appState(state: AppState): void;
  screenLocked(locked: boolean): void;
  interrupted(interrupted: boolean): void;
  routeChanged(route: string): void;
}

// appStateOf maps React Native's AppState string; anything it does not know is
// treated as leaving the foreground, the safe side.
export function appStateOf(state: string): AppState {
  if (state === 'active') return 'active';
  if (state === 'inactive') return 'inactive';
  return 'background';
}

// deliver hands one native event to the sink. It returns a line for the
// diagnostics (the kind and state, never a device name).
export function deliver(sink: LifecycleSink, event: NativeEvent): string {
  switch (event.type) {
    case 'lock':
      sink.screenLocked(event.state === 'locked');
      break;
    case 'focus':
      // 'duck' is a notification sound: not an interruption.
      if (event.state === 'lost' || event.state === 'lost_transient') sink.interrupted(true);
      else if (event.state === 'gained') sink.interrupted(false);
      break;
    case 'route':
      sink.routeChanged(event.detail);
      break;
  }
  return `native ${event.type}: ${event.state}`;
}
