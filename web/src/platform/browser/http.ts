// The browser's HttpTransport: fetch against the page's own origin. The
// arguments of every fetch here are exactly those the client has always made,
// built from the operation table; http.test.ts pins them.
import { operations, pathOf, previewHeader, type Call } from '../operations';
import type { HttpTransport } from '../types';

const json = 'application/json';

// plain refuses a call that asks for a preview: only a GET carries the header,
// so a write can never answer as someone else (the server refuses it too).
function plain(call: Call): Call {
  if (call.previewAs !== undefined) throw new Error(`operation ${call.op} cannot be previewed`);
  return call;
}

export const http: HttpTransport = {
  get(call, token) {
    if (call.previewAs !== undefined && operations[call.op].previewable !== true) throw new Error(`operation ${call.op} cannot be previewed`);
    return fetch(pathOf(call), {
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: json,
        ...(call.previewAs === undefined ? {} : { [previewHeader]: String(call.previewAs) }),
      },
      cache: 'no-store',
      credentials: 'omit',
    });
  },

  send(call, token, { body, keepalive, signal }) {
    return fetch(pathOf(plain(call)), {
      method: operations[call.op].method,
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: json,
        ...(body === undefined ? {} : { 'Content-Type': json }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store',
      credentials: 'omit',
      keepalive,
      ...(signal === undefined ? {} : { signal }),
    });
  },

  open(call, body) {
    return fetch(pathOf(plain(call)), {
      method: body === undefined ? 'GET' : 'POST',
      headers: body === undefined ? { Accept: json } : { Accept: json, 'Content-Type': json },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      cache: 'no-store',
      credentials: 'omit',
    });
  },

  health(call) {
    return fetch(pathOf(call), { headers: { Accept: json }, cache: 'no-store', credentials: 'omit' });
  },

  upload(call, token, mediaType, bytes, signal) {
    return fetch(pathOf(plain(call)), {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: json,
        ...(mediaType === '' ? {} : { 'Content-Type': mediaType }),
      },
      body: bytes,
      cache: 'no-store',
      credentials: 'omit',
      signal,
    });
  },

  download(call, token) {
    return fetch(pathOf(plain(call)), {
      headers: { Authorization: `Bearer ${token}` },
      cache: 'no-store',
      credentials: 'omit',
    });
  },
};
