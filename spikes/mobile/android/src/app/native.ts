// The native module's JavaScript side (ConchVoiceModule.kt): the foreground
// service, audio focus and the routes, as the NativePort of controller.ts, and
// the event stream the lifecycle bridge reads. Permissions are asked here, at
// Join, never at launch (M-P1, M-P2).
import { NativeEventEmitter, NativeModules, PermissionsAndroid } from 'react-native';
import type { NativeEvent } from './bridge';
import type { NativePort } from './controller';

interface Raw {
  startService(): Promise<string>;
  stopService(): Promise<void>;
  serviceRunning(): Promise<boolean>;
  requestFocus(): Promise<boolean>;
  abandonFocus(): Promise<void>;
  routes(): Promise<string[]>;
  isLocked(): Promise<boolean>;
}

const raw = NativeModules.ConchVoice as Raw & object;

export const nativePort: NativePort = {
  async ensurePermissions() {
    const mic = await PermissionsAndroid.request(PermissionsAndroid.PERMISSIONS.RECORD_AUDIO);
    if (mic !== PermissionsAndroid.RESULTS.GRANTED) return 'microphone_denied';
    // The service's notification. Refused, the app still works: the service
    // runs without a visible notification and the OS may say so.
    await PermissionsAndroid.request(PermissionsAndroid.PERMISSIONS.POST_NOTIFICATIONS).catch(() => undefined);
    return 'granted';
  },
  async ensureCameraPermission() {
    const camera = await PermissionsAndroid.request(PermissionsAndroid.PERMISSIONS.CAMERA);
    return camera === PermissionsAndroid.RESULTS.GRANTED;
  },
  async startService() {
    await raw.startService();
  },
  stopService: () => raw.stopService(),
  requestFocus: () => raw.requestFocus(),
  abandonFocus: () => raw.abandonFocus(),
};

export function serviceRunning(): Promise<boolean> {
  return raw.serviceRunning();
}

export function isLocked(): Promise<boolean> {
  return raw.isLocked();
}

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
