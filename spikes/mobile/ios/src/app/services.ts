// The app's singletons, built once: the diagnostics, the API client (which
// holds the bearer in memory), the voice controller, and the wiring of the
// lifecycle bridge to the OS. Nothing here persists.
import { AppState } from 'react-native';
import { Diagnostics } from '../logic/diagnostics';
import { ApiClient } from './client';
import { appStateOf, deliver } from './bridge';
import { VoiceController } from './controller';
import { LiveKitRoom } from './livekit';
import { quietSdk } from './logging';
import { cameraStore } from './camera/store';
import { nativePort, onNativeEvent, routes } from './native';
import type { LocalAudioTrack } from 'livekit-client';
import type { AuthSignal, AuthStep } from '../logic/auth';

export const diagnostics = new Diagnostics(200);
quietSdk(diagnostics);

// The signed-in principal, read by the presence check. In memory only.
const who = { id: -1, name: '' };

// The microphone track, for the open-microphone level probe.
let mic: LocalAudioTrack | null = null;
const micListeners = new Set<() => void>();
export const micStore = {
  get: (): LocalAudioTrack | null => mic,
  subscribe: (listener: () => void): (() => void) => {
    micListeners.add(listener);
    return () => {
      micListeners.delete(listener);
    };
  },
};

// What the app does when the credential is refused (logic/auth.ts): media is
// torn down, then signed out is shown. A 1008 revalidates instead.
let onSignedOut: () => void = () => undefined;
export function whenSignedOut(listener: () => void): void {
  onSignedOut = listener;
}

async function onSignal(signal: AuthSignal, steps: readonly AuthStep[]): Promise<void> {
  diagnostics.add('info', `auth signal ${signal}`);
  for (const step of steps) {
    if (step === 'tear_down_media') await controller.tearDown();
    else if (step === 'show_signed_out') {
      who.id = -1;
      onSignedOut();
    } else {
      const again = await client.whoami();
      if (!again.ok && again.failure === 'unauthorized') return;
    }
  }
}

export const client = new ApiClient({
  fetch: (url, init) => fetch(url, init),
  onSignal: (signal, steps) => {
    void onSignal(signal, steps);
  },
  note: (message) => {
    diagnostics.add('http', message);
  },
});

export const clock = {
  setTimeout: (run: () => void, ms: number): unknown => setTimeout(run, ms),
  clearTimeout: (handle: unknown): void => {
    clearTimeout(handle as ReturnType<typeof setTimeout>);
  },
};

export const controller = new VoiceController({
  api: client,
  native: nativePort,
  newRoom: () =>
    new LiveKitRoom({
      onMicrophone: (track) => {
        mic = track;
        for (const l of [...micListeners]) l();
      },
      onCameras: (tracks) => {
        cameraStore.set(tracks);
      },
    }),
  clock,
  random: Math.random,
  me: () => who.id,
  log: (message) => {
    diagnostics.add('voice', message);
  },
  hold: (...secrets) => {
    diagnostics.hold(...secrets);
  },
});

// signedIn records the principal and tells the diagnostics what must never
// appear in an export.
export function signedIn(id: number, name: string): void {
  who.id = id;
  who.name = name;
  const bearer = client.secret;
  if (bearer !== null) diagnostics.hold(bearer);
  if (client.server !== '') diagnostics.hold(client.server, client.server.replace(/^https?:\/\//, ''));
}

export function principal(): { id: number; name: string } {
  return { id: who.id, name: who.name };
}

// signOut: media first, then the bearer (M-K3).
export async function signOut(): Promise<void> {
  await controller.tearDown();
  client.signOut();
  who.id = -1;
}

// The lifecycle bridge (bridge.ts): AppState, and the native interruption and
// route events. Installed once; the app never stops listening. There is no lock
// event on iOS: locking the screen is AppState 'inactive' then 'background'.
let bridged = false;
export function bridgeLifecycle(): void {
  if (bridged) return;
  bridged = true;
  controller.appState(appStateOf(AppState.currentState ?? 'active'));
  AppState.addEventListener('change', (state) => {
    diagnostics.add('lifecycle', `app ${state}`);
    controller.appState(appStateOf(state));
  });
  onNativeEvent((event) => {
    // The route's port types are read in the bridge's own call, not stored.
    diagnostics.add('lifecycle', deliver(controller, event));
  });
  void routes().then((kinds) => {
    diagnostics.add('lifecycle', `routes ${kinds.join(', ')}`);
  });
}
