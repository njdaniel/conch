// The one window (linux-desktop.md §3): what its web contents may be, and
// nothing about what is shown in it. Input: whether the app is packaged and
// where its files are. Output: a BrowserWindow loading conch-app://ui/.
import { join } from 'node:path';

import { BrowserWindow, type WebPreferences } from 'electron';

import { INITIAL_URL } from './scheme.ts';

// An in-memory session: no "persist:" prefix, so nothing the page stores
// outlives the process. The scheme handler and the guards are installed on it.
export const PARTITION = 'conch-ui';

// webPreferences is the whole list, pinned by a unit test: a key added here is
// a decision to make in review, not a default to inherit.
export function webPreferences(packaged: boolean, preload: string): WebPreferences {
  return {
    nodeIntegration: false,
    contextIsolation: true,
    sandbox: true,
    webSecurity: true,
    webviewTag: false,
    spellcheck: false,
    navigateOnDragDrop: false,
    safeDialogs: true,
    devTools: !packaged,
    preload,
    partition: PARTITION,
  };
}

export function preloadPath(appPath: string): string {
  return join(appPath, 'out', 'preload', 'index.cjs');
}

export function createWindow(packaged: boolean, appPath: string): BrowserWindow {
  const window = new BrowserWindow({
    width: 1100,
    height: 760,
    minWidth: 360,
    minHeight: 480,
    title: 'Conch',
    show: false,
    webPreferences: webPreferences(packaged, preloadPath(appPath)),
  });
  window.once('ready-to-show', () => {
    window.show();
  });
  void window.loadURL(INITIAL_URL);
  return window;
}
