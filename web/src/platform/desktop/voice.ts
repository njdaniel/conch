// The native-voice store of the desktop renderer (issue #1375, D3-4 of #771):
// a useSyncExternalStore source over the voice half of window.conchDesktop
// (DesktopVoiceBridge, ./bridge). It shows the last confirmed view main sent
// and delegates every action to the bridge. It never constructs a VoiceClient,
// opens a room or asks for the microphone: the helper owns all of that.
import type { VoiceControlTalkMode } from '../../schema/generated';
import type { NativeVoice, NativeVoiceOutcome } from '../types';
import { bridge } from './access';
import type { DesktopVoiceBridge, NativeVoiceDevicesResult, NativeVoiceView, Unsubscribe, VoiceCommandResult, VoiceSaveResult } from './bridge';

const IDLE: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const UNAVAILABLE: NativeVoiceView = { status: 'unavailable', generation: 0, channel: null, retryable: false, problem: 'unsupported' };

const METHODS = ['voiceState', 'voiceJoin', 'voiceLeave', 'voiceRetry', 'voiceSetMute', 'voiceSetDeafen', 'voiceSetDevices', 'voiceSetTalkMode', 'onVoiceState'] as const;

// The voice half of the bridge, or undefined when the preload does not offer
// all of it.
function voiceBridge(): DesktopVoiceBridge | undefined {
  const whole = bridge();
  if (whole === undefined) return undefined;
  const b: Partial<DesktopVoiceBridge> = whole as object;
  for (const m of METHODS) if (typeof b[m] !== 'function') return undefined;
  return b as DesktopVoiceBridge;
}

const lost = { ok: false, error: 'lost' } as const;
const unsupported = { ok: false, error: 'unsupported' } as const;
const failed = { ok: false, error: 'failed' } as const;

// createNativeVoice builds a store over the voice bridge that get returns.
export function createNativeVoice(get: () => DesktopVoiceBridge | undefined = voiceBridge): NativeVoice {
  let view: NativeVoiceView = get() === undefined ? UNAVAILABLE : IDLE;
  const listeners = new Set<() => void>();
  let off: Unsubscribe | undefined;

  // Takes a view unless it is older than the one held.
  const accept = (next: NativeVoiceView): void => {
    if (next.generation < view.generation) return;
    view = next;
    for (const l of [...listeners]) l();
  };

  const start = (): void => {
    const b = get();
    if (b === undefined) return;
    try {
      off = b.onVoiceState(accept);
      b.voiceState().then(accept, () => undefined);
    } catch {
      // The bridge is gone; the view stays as it was.
    }
  };

  const stop = (): void => {
    try {
      off?.();
    } catch {
      // Nothing to release.
    }
    off = undefined;
  };

  const viewCall = async (call: (b: DesktopVoiceBridge) => Promise<NativeVoiceView>): Promise<NativeVoiceOutcome> => {
    const b = get();
    if (b === undefined) return unsupported;
    try {
      const next = await call(b);
      accept(next);
      return { ok: true, view: next };
    } catch {
      return lost;
    }
  };

  const command = async (call: (b: DesktopVoiceBridge) => Promise<VoiceCommandResult>): Promise<VoiceCommandResult> => {
    const b = get();
    if (b === undefined) return unsupported;
    try {
      return await call(b);
    } catch {
      return lost;
    }
  };

  // The one-shot runs answer with their own refusal words.
  const oneShot = async <R extends NativeVoiceDevicesResult | VoiceSaveResult>(call: (b: DesktopVoiceBridge) => Promise<R>, fail: R): Promise<R> => {
    const b = get();
    if (b === undefined) return fail;
    try {
      return await call(b);
    } catch {
      return fail;
    }
  };

  return {
    snapshot: () => view,
    subscribe(listener) {
      listeners.add(listener);
      if (listeners.size === 1) start();
      return () => {
        listeners.delete(listener);
        if (listeners.size === 0) stop();
      };
    },
    join: (r) => viewCall((b) => b.voiceJoin({ channel: r.channel, muted: r.muted ?? false, deafened: r.deafened ?? false })),
    leave: () => viewCall((b) => b.voiceLeave()),
    retry: () => viewCall((b) => b.voiceRetry()),
    setMute: (muted: boolean) => command((b) => b.voiceSetMute(muted)),
    setDeafen: (deafened: boolean) => command((b) => b.voiceSetDeafen(deafened)),
    setDevices: (devices) => command((b) => b.voiceSetDevices(devices)),
    setTalkMode: (mode: VoiceControlTalkMode) => command((b) => b.voiceSetTalkMode(mode)),
    devices: () => oneShot((b) => b.voiceDevices(), { ok: false, error: 'unsupported' }),
    saveDevice: (request) => oneShot((b) => b.voiceSaveDevice(request), { ok: false, error: 'failed' }),
    keyDevices: async () => {
      const b = get();
      if (b === undefined || typeof b.voiceKeyDevices !== 'function') return unsupported;
      try {
        return await b.voiceKeyDevices();
      } catch {
        return failed;
      }
    },
    saveTalkBinding: async (request) => {
      const b = get();
      if (b === undefined || typeof b.voiceSaveTalkBinding !== 'function') return unsupported;
      try {
        return await b.voiceSaveTalkBinding(request);
      } catch {
        return failed;
      }
    },
  };
}

export const nativeVoice: NativeVoice = createNativeVoice();
