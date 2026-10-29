// The preload: the only code that runs in the renderer's isolated world.
// Input: none. Output: window.conchDesktop, a frozen EMPTY object. It has no
// method yet: D2-3 adds the named functions of the bridge. It never exposes
// ipcRenderer, an event object or a caller-chosen channel.
import { contextBridge } from 'electron';

contextBridge.exposeInMainWorld('conchDesktop', Object.freeze({}));
