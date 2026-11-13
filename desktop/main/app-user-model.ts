// The Windows AppUserModelID (issue #900; windows-desktop.md §9.2). Input:
// Electron's app and the platform. Output: on Windows, app.setAppUserModelId
// with the app id; on every other platform, nothing.
//
// Windows groups the taskbar button and attributes notifications by this id,
// and the installer's Start menu and desktop shortcuts carry the same one
// (electron-builder's NSIS template sets their AUMI to appId, which is
// APP_ID). main.ts calls this first, before start() makes any window or
// notification; a unit test holds that order.
import { APP_ID } from './autostart.ts';

export interface AppUserModelApp {
  setAppUserModelId(id: string): void;
}

export function applyAppUserModelId(app: AppUserModelApp, platform: NodeJS.Platform = process.platform): void {
  if (platform === 'win32') app.setAppUserModelId(APP_ID);
}
