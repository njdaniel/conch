import { createContext } from 'react';

// A way to open Settings with Joining and guests expanded (issue #909). The
// lobby owns the Settings drawer and provides it; the invite panel asks for it
// when an operator's invite is refused as paused, and GuestSettings expands on
// a new request. Null where there is no lobby: nothing then offers the jump.
export interface JoiningRequest {
  // Counts requests, so each one is seen once by the section.
  readonly count: number;
  readonly open: () => void;
}

export const JoiningSettings = createContext<JoiningRequest | null>(null);
