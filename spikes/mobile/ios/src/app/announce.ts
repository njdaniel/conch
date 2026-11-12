// What the app says aloud to VoiceOver, decided from a change of state (#1395).
// React Native's accessibilityLiveRegion is Android-only, so on iOS the
// controller's changes are announced with AccessibilityInfo.announceForAccessibility
// (ui/useAnnounce.ts). Pure, so a test runs it: it names changes only, never
// the level meter, and says nothing when nothing changed. Notes (and why and the warning) are
// announced by the Note component through textToAnnounce.
import type { VoiceState } from './controller';

type Said = Pick<VoiceState, 'phase' | 'listenOnly' | 'gateOpen' | 'mode'>;

function phaseWords(s: Said): string | null {
  switch (s.phase) {
    case 'connected':
      return s.listenOnly ? 'In voice, listening only' : 'In voice';
    case 'reconnecting':
      return 'Reconnecting';
    case 'confirm':
      return 'Waiting for you to confirm';
    case 'joining':
      return 'Joining';
    case 'checking':
      return null;
    case 'idle':
      return 'Not in voice';
  }
}

// stateAnnouncements is what to say when the state goes from prev to next.
export function stateAnnouncements(prev: Said, next: Said): string[] {
  const out: string[] = [];
  if (prev.phase !== next.phase || prev.listenOnly !== next.listenOnly) {
    const words = phaseWords(next);
    if (words !== null) out.push(words);
  }
  // why, warning and the notes are shown by Note, which announces its own text.
  // The gate is spoken only while connected, and in push to talk: the open
  // microphone's status line is a Note and says it itself.
  if (prev.gateOpen !== next.gateOpen && next.phase === 'connected' && next.mode === 'ptt') {
    out.push(next.gateOpen ? 'Microphone open' : 'Microphone closed');
  }
  return out;
}

// textToAnnounce is the text of a note that has just appeared or changed.
export function textToAnnounce(prev: string | null, next: string | null): string | null {
  return next !== null && next !== '' && next !== prev ? next : null;
}
