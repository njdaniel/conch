// Whether a modal of the lobby (Settings, re-authentication, the voice options
// sheet) is up, for what is drawn outside the lobby and still on the page: the
// desktop shell's strip and update notice, the browser's Reload notice. The
// lobby is the one owner: it derives the state and writes it here, and
// those elements read it to be inert, so the keyboard cannot reach them behind
// a modal. Nothing else writes it.
import { useSyncExternalStore } from 'react';

let modal = false;
const listeners = new Set<() => void>();

// Said by the lobby, the only writer. A page without a lobby has no modal.
export function setPageModal(open: boolean): void {
  if (modal === open) return;
  modal = open;
  for (const l of listeners) l();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

// True while a modal of the lobby is up.
export function usePageModal(): boolean {
  return useSyncExternalStore(subscribe, () => modal, () => false);
}
