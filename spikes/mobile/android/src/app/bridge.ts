// The lifecycle bridge: the OS's signals as the lifecycle's inputs
// (logic/lifecycle.ts). Three sources feed it:
//  - React Native's AppState: 'active', 'background', and 'inactive' (which
//    Android does not send, but the logic handles for iOS);
//  - the native module's "lock" events (screen off / the person got past the
//    keyguard);
//  - its "focus" events (audio focus lost or gained: a call, the assistant,
//    another app), "route" events (an audio device came or went) and the
//    "service" event task_removed (the app was swiped away).
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
  // permanent is a loss the OS will never send a gain for (AUDIOFOCUS_LOSS).
  interrupted(interrupted: boolean, permanent?: boolean): void;
  routeChanged(route: string): void;
  // The task was swiped away: voice ends (VoiceService.onTaskRemoved).
  taskRemoved(): void;
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
      // 'lost' is permanent: Android never sends a gain after it, so the
      // controller asks for the focus again when the app is next active.
      if (event.state === 'lost') sink.interrupted(true, true);
      else if (event.state === 'lost_transient') sink.interrupted(true);
      else if (event.state === 'gained') sink.interrupted(false);
      break;
    case 'route':
      sink.routeChanged(event.detail);
      break;
    case 'service':
      if (event.state === 'task_removed') sink.taskRemoved();
      break;
  }
  return `native ${event.type}: ${event.state}`;
}
