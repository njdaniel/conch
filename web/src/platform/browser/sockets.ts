// The browser's SocketTransport: a WebSocket on the page's own origin (so
// connect-src 'self' holds). The address carries the single-use ticket, so it
// is handed to the WebSocket and to nothing else: never to location, history
// or the DOM.
import { operations } from '../operations';
import type { SocketTransport } from '../types';

export const sockets: SocketTransport = {
  ticket: true,
  open(id, query) {
    const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    return new WebSocket(`${scheme}//${window.location.host}${operations[id].path}?${new URLSearchParams(query).toString()}`);
  },
};
