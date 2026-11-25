// Whether a modal of the lobby (Settings, re-authentication, the voice options
// sheet) is up, for what is drawn outside the lobby and still on the page: the
// desktop shell's strip and update notice, the browser's Reload notice. The
// lobby is the one owner: it derives the state and writes it here, and
// those elements read it to be inert, so the keyboard cannot reach them behind
// a modal. Nothing else writes it.
import { createContext, useContext, useEffect, useSyncExternalStore } from 'react';

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

// A modal the lobby does not draw itself, drawn over the page (into the body,
// say): the quick switcher's dialog. It says it is open with useReportOverlay,
// and the lobby, which counts them, tells the voice options sheet something is
// over it (VoiceSheet's behind), as it does for Settings and re-authentication.
// The one place a new body-level modal says so: one that does not is the one
// the sheet will take focus from.
export const OverlayReport = createContext<(open: boolean) => void>(() => undefined);

// Says, for as long as open is true, that a modal is over the page.
export function useReportOverlay(open: boolean): void {
  const report = useContext(OverlayReport);
  useEffect(() => {
    if (!open) return;
    report(true);
    return () => {
      report(false);
    };
  }, [open, report]);
}

// Said by the lobby to what is drawn inside the Members panel: called before an
// action there shows something in the centre of the page (Open run), so that,
// where the panel is a modal drawer over the page, it closes first and the
// target, not the drawer, gets the page. A side column, which is not modal,
// is left as it is.
export const LeaveDrawer = createContext<() => void>(() => undefined);

// Said by the lobby to the invite panel in its top bar: the Members drawer is
// drawn over the page. The invite panel, which is painted over the page too,
// closes then rather than stay up over the drawer, inert.
export const DrawerOver = createContext(false);
