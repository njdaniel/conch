// The one window (linux-desktop.md §3): what its web contents may be, and
// nothing about what is shown in it. Input: whether the app is packaged,
// where its files are, and the gate that tracks its documents. Output: a
// BrowserWindow loading conch-app://ui/.
import { join } from 'node:path';

import { BrowserWindow, type WebPreferences } from 'electron';

import { trackDocument, type DocumentGate } from './document.ts';
import { INITIAL_URL } from './scheme.ts';

// An in-memory session: no "persist:" prefix, so nothing the page stores
// outlives the process. The scheme handler and the guards are installed on it.
export const PARTITION = 'conch-ui';

// Each activation of a profile gets a partition of its own, named for its
// generation and never reused: a switch or a sign-out creates new web contents
// in a fresh in-memory session and destroys the old one (issue #945).
export function partitionFor(generation: number): string {
  return `${PARTITION}-${String(generation)}`;
}

// webPreferences is the whole list, pinned by a unit test: a key added here is
// a decision to make in review, not a default to inherit.
export function webPreferences(packaged: boolean, preload: string, partition: string = PARTITION): WebPreferences {
  return {
    nodeIntegration: false,
    contextIsolation: true,
    sandbox: true,
    webSecurity: true,
    webviewTag: false,
    spellcheck: false,
    navigateOnDragDrop: false,
    safeDialogs: true,
    // Electron's defaults for these are already the safe ones; they are written
    // out so that a default changing, or a copy of this object elsewhere, is a
    // diff to review.
    nodeIntegrationInWorker: false,
    nodeIntegrationInSubFrames: false,
    allowRunningInsecureContent: false,
    experimentalFeatures: false,
    plugins: false,
    enableBlinkFeatures: '',
    devTools: !packaged,
    preload,
    partition,
  };
}

export function preloadPath(appPath: string): string {
  return join(appPath, 'out', 'preload', 'index.cjs');
}

export interface WindowOptions {
  readonly partition?: string;
  readonly bounds?: { readonly x: number; readonly y: number; readonly width: number; readonly height: number };
  // Called with the window after it is made and before its first load: the
  // session's handlers are installed here.
  readonly prepare?: (window: BrowserWindow) => void;
}

export function createWindow(packaged: boolean, appPath: string, gate: DocumentGate, options: WindowOptions = {}): BrowserWindow {
  const window = new BrowserWindow({
    width: 1100,
    height: 760,
    ...(options.bounds ?? {}),
    minWidth: 360,
    minHeight: 480,
    title: 'Conch',
    show: false,
    webPreferences: webPreferences(packaged, preloadPath(appPath), options.partition),
  });
  // The document generation begins at each navigation, before the first load.
  trackDocument(window.webContents, gate);
  options.prepare?.(window);
  window.once('ready-to-show', () => {
    window.show();
  });
  void window.loadURL(INITIAL_URL);
  return window;
}
