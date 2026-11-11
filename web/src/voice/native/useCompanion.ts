// The desktop strip's screen companion (issue #1582, D4-05 of #772): the one
// VoiceClient the page builds on the desktop, for sharing the screen and
// watching others' shares (#1581). It is never the person's voice (the helper
// is), so it is created only where the native voice has a companion bridge,
// and it is not joined until the person shares or watches. Its share quality
// is the desktop's (Q-D4-3), kept like the browser's own choice.
import { useEffect, useRef, useSyncExternalStore } from 'react';
import * as api from '../../api';
import type { NativeVoice } from '../../platform/types';
import { VoiceClient, initialVoice, type VoiceState } from '../connection';
import { newLiveKitRoom } from '../sdk';
import { companionEnv } from './companionEnv';

// What the share control and the stream tiles need of the companion; a fake
// satisfies it in a test.
export type CompanionClient = Pick<
  VoiceClient,
  'subscribe' | 'snapshot' | 'startCompanionShare' | 'watchCompanionStream' | 'stopShare' | 'watchStream' | 'focusShare' | 'setShareQuality' | 'presence' | 'attachStream' | 'readShareStats'
>;

const realClock = {
  setTimeout: (run: () => void, ms: number) => window.setTimeout(run, ms),
  clearTimeout: (handle: unknown) => {
    window.clearTimeout(handle as number);
  },
};

const never = (): (() => void) => () => undefined;

export interface Companion {
  readonly client: CompanionClient;
  readonly state: VoiceState;
}

// useCompanion is the companion client of this source and its state, or null
// where the bridge has no companion (then the strip offers no share).
export function useCompanion(source: NativeVoice | undefined): Companion | null {
  const ref = useRef<VoiceClient | null>(null);
  const env = companionEnv(source);
  if (ref.current === null && env !== undefined) {
    ref.current = new VoiceClient({
      clock: realClock,
      random: Math.random,
      createSession: api.createVoiceSession,
      companion: env,
      report: api.reportVoiceTransmit,
      newRoom: newLiveKitRoom,
      claimShare: api.claimVoiceShare,
      stopShare: api.stopVoiceShare,
      shareQuality: api.keptShareQuality() ?? undefined,
    });
  }
  const client = ref.current;
  const state = useSyncExternalStore(client?.subscribe ?? never, client?.snapshot ?? (() => initialVoice));
  const kept = useRef(state.shareQuality);
  useEffect(() => {
    if (kept.current === state.shareQuality) return;
    kept.current = state.shareQuality;
    api.writeShareQuality(state.shareQuality);
  }, [state.shareQuality]);
  useEffect(
    () => () => {
      client?.dispose();
    },
    [client],
  );
  return client === null ? null : { client, state };
}
