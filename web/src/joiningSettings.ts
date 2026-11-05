import { createContext } from 'react';

// A way to open Settings with Joining expanded (issue #909). The
// lobby owns the Settings drawer and provides it; the invite panel asks for it
// when an operator's invite is refused as paused, and GuestSettings expands on
// a new request. Null where there is no lobby: nothing then offers the jump.
// Whether the Settings drawer is open. The administration console reads what
// it shows afresh each time the drawer opens, and not while it is shut. True
// where there is no lobby (a section on its own).
export const SettingsOpen = createContext(true);

export interface JoiningRequest {
  // Counts requests, so each one is seen once by the section.
  readonly count: number;
  readonly open: () => void;
}

export const JoiningSettings = createContext<JoiningRequest | null>(null);

// A way to open the member-link panel from the console's Members section
// (issue #1020, "Add a person"): the lobby closes Settings and the page opens
// the panel. Null where there is no lobby or the person cannot make member
// links: the section then offers no such button.
export const AddPerson = createContext<(() => void) | null>(null);
