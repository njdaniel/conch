import { useSyncExternalStore } from 'react';
import type { VoiceController, VoiceState } from '../controller';

export function useVoiceState(controller: VoiceController): VoiceState {
  return useSyncExternalStore(controller.subscribe, () => controller.state);
}
