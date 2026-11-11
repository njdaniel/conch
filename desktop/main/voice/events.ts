// Sends the helper's confirmed voice state to the page (issue #1374, D3-3 of
// #771). Input: each NativeVoiceView the VoiceService reports. Output: one
// message on VOICE_EVENTS to the owned web contents, and only while its
// current document has committed; to no other window, and to no document that
// has been left. The view carries no token, path or pid (service.ts).
import type { NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import { VOICE_EVENTS } from '../../shared/bridge-names.ts';
import type { DocumentGate } from '../document.ts';

export interface VoiceTarget {
  // The owned web contents' send, or null when there is no live window.
  readonly contents: () => { send(channel: string, view: NativeVoiceView): void } | null;
  readonly gate: () => DocumentGate;
}

export function deliverVoice(target: VoiceTarget, view: NativeVoiceView): void {
  const contents = target.contents();
  const gate = target.gate();
  if (contents === null || !gate.shows(gate.generation)) return;
  contents.send(VOICE_EVENTS, view);
}
