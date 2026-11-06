// The preload: the only code that runs in the renderer's isolated world.
// Input: none. Output: window.conchDesktop, a frozen object whose keys are
// exactly the functions of web/src/platform/desktop/bridge.ts (bridge.ts here
// builds it). It never exposes ipcRenderer, an event object or a
// caller-chosen channel; it listens on the one feed channel and passes on the
// message alone.
//
// The object is frozen here, but contextBridge hands the page a copy, and that
// copy is NOT frozen: the page can add properties to its own window.conchDesktop.
// Nothing may rely on it being frozen; what the page can reach is the functions
// the bridge exposes, and main checks every call (desktop/main/ipc.ts).
import { contextBridge, ipcRenderer } from 'electron';

import { FEED_EVENTS } from '../shared/bridge-names.ts';
import { createBridge } from './bridge.ts';

contextBridge.exposeInMainWorld(
  'conchDesktop',
  createBridge(
    (channel, ...args) => ipcRenderer.invoke(channel, ...args) as Promise<unknown>,
    (callback) => {
      ipcRenderer.on(FEED_EVENTS, (_event, message: unknown) => {
        callback(message);
      });
    },
  ),
);
