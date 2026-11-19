import { createContext } from 'react';

// The OS reported in the desktop shell's profile view, for platform-specific copy.
export const DesktopOS = createContext<string | null>(null);
