// The native module's JavaScript side (ConchVoice.swift): the microphone
// permission and the AVAudioSession interruption and route observers, as the NativePort of
// controller.ts, and the event stream the lifecycle bridge reads. The
// microphone permission is asked here, at Join, never at launch (M-P1, M-P2).
import { NativeEventEmitter, NativeModules } from 'react-native';
import type { NativeEvent } from './bridge';
import type { NativePort } from './controller';

interface Raw {
  // 'granted' | 'denied' | 'undetermined' after asking if undetermined.
  requestMicrophone(): Promise<string>;
  startSession(): Promise<string>;
  stopSession(): Promise<void>;
  // The output port types now in use, e.g. ["Speaker"], never a device name.
  routes(): Promise<string[]>;
}

const raw = NativeModules.ConchVoice as Raw & object;

export const nativePort: NativePort = {
  async ensurePermissions() {
    return (await raw.requestMicrophone()) === 'granted' ? 'granted' : 'microphone_denied';
  },
  async startSession() {
    await raw.startSession();
  },
  stopSession: () => raw.stopSession(),
};

export function routes(): Promise<string[]> {
  return raw.routes();
}

// onNativeEvent subscribes to the module's events; it returns the way to stop.
export function onNativeEvent(listener: (event: NativeEvent) => void): () => void {
  const emitter = new NativeEventEmitter(NativeModules.ConchVoice);
  const subscription = emitter.addListener('ConchVoiceEvent', (e: unknown) => {
    const event = e as Partial<NativeEvent> | null;
    if (typeof event?.type !== 'string') return;
    listener({ type: event.type, state: event.state ?? '', detail: event.detail ?? '' });
  });
  return () => {
    subscription.remove();
  };
}
