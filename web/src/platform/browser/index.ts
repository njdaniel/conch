// The browser's platform: fetch, WebSocket, web storage and the browser's own
// services, against the origin that served the page.
import type { Platform } from '../types';
import { credentials } from './credentials';
import { http } from './http';
import { services } from './services';
import { sockets } from './sockets';

export const platform: Platform = { name: 'browser', http, sockets, credentials, services };
