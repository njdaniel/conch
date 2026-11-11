// The native voice view for a component (issue #1375, D3-4 of #771): the
// platform's native-voice store read with useSyncExternalStore, and the words
// for it. Where the platform has none (the browser) present is false and the
// view is the "not available here" one. Renders nothing itself.
import { useMemo, useSyncExternalStore } from 'react';
import { platform } from '../../platform';
import type { NativeVoiceView } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import { words, type VoiceWords } from './words';

const ABSENT: NativeVoiceView = { status: 'unavailable', generation: 0, channel: null, retryable: false, problem: 'unsupported' };
const NONE = (): void => undefined;
const absentStore = { subscribe: () => NONE, snapshot: () => ABSENT };

export interface NativeVoiceState {
  readonly present: boolean;
  readonly view: NativeVoiceView;
  readonly words: VoiceWords;
  readonly actions: Omit<NativeVoice, 'snapshot' | 'subscribe'> | undefined;
}

export function useNativeVoice(source: NativeVoice | undefined = platform.nativeVoice, binding?: string): NativeVoiceState {
  const store = source ?? absentStore;
  const view = useSyncExternalStore(store.subscribe, store.snapshot);
  const w = useMemo(() => words(view, binding), [view, binding]);
  return { present: source !== undefined, view, words: w, actions: source };
}
