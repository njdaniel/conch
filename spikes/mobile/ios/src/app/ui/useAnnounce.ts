// The iOS half of live regions: speaks what announce.ts decides. Android's
// accessibilityLiveRegion stays on the views; iOS ignores it.
import { useEffect, useRef } from 'react';
import { AccessibilityInfo, Platform } from 'react-native';
import { stateAnnouncements, textToAnnounce } from '../announce';
import type { VoiceState } from '../controller';

function say(text: string): void {
  if (Platform.OS === 'ios') AccessibilityInfo.announceForAccessibility(text);
}

// useAnnounceState announces changes of the voice state, never the first render.
export function useAnnounceState(state: VoiceState): void {
  const prev = useRef(state);
  useEffect(() => {
    for (const text of stateAnnouncements(prev.current, state)) say(text);
    prev.current = state;
  }, [state]);
}

// useAnnounceText announces a note when it appears or changes.
export function useAnnounceText(text: string | null): void {
  const prev = useRef<string | null>(null);
  useEffect(() => {
    const spoken = textToAnnounce(prev.current, text);
    prev.current = text;
    if (spoken !== null) say(spoken);
  }, [text]);
}
