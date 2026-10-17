import { createContext } from 'react';

// Where the voice section draws the shared screen it is watching: the top of
// the lobby's centre column (issue #536). The section itself stays in the
// voice strip and draws the view there with a portal, so the view is one
// component instance however the layout changes. Null where there is no
// lobby (a test of the section alone): the view is then drawn in place.
export const StageHost = createContext<HTMLElement | null>(null);
