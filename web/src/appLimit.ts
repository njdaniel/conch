// What the page says when the desktop app's main process refused an operation
// for it (issue #1768): the server was never asked, so it is not a network
// failure and not the server's refusal. The transport answers it as a 501 with
// this code (the desktop transport); a line function of a page that shows
// the failure of such an operation says this line for it.
export const DESKTOP_UNAVAILABLE = 'desktop_unavailable';

export const desktopUnavailableLine = 'The desktop app can’t do this yet.';

// The desktop bounds the bytes it reads and passes through IPC. A server
// can enforce a lower limit when it receives an upload.
export const DESKTOP_UPLOAD_MAX_BYTES = 20 * 1024 * 1024;
