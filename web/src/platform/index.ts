// The platform the client runs on. A normal build (vite build, make web-build)
// is the browser's. A desktop build (vite build --mode desktop) aliases this
// module's './selected' import to the desktop adapter in vite.config.ts, so
// the browser bundle never contains desktop code.
import { platform as selected } from '#platform-selected';
import type { Platform } from './types';

export const platform: Platform = selected;

export type { Platform } from './types';
