// The preload: the only code that runs in the renderer's isolated world.
// Input: none. Output: window.conchDesktop, an EMPTY object. It has no method
// yet: D2-3 adds the named functions of the bridge. It never exposes
// ipcRenderer, an event object or a caller-chosen channel.
//
// The object is frozen here, but contextBridge hands the page a copy, and that
// copy is NOT frozen: the page can add properties to its own window.conchDesktop.
// Nothing may rely on it being frozen; what the page can reach is the functions
// the bridge exposes, and main validates every call.
import { contextBridge } from 'electron';

contextBridge.exposeInMainWorld('conchDesktop', Object.freeze({}));
