// The page's diagnostic ring (issue #446): the last ringSize things that
// happened between the page and the server, for "Report a problem". It lives
// in this module's memory only: never in storage, never logged, gone on
// reload. Only api.ts and the voice client record into it.
//
// An event holds what DiagnosticEventV1 allows and nothing else: numbers,
// closed vocabularies and short tokens, checked again here so that a value of
// the wrong shape is dropped rather than kept. A socket is named by what it
// carries, never by its address (which holds a channel name and a ticket); an
// HTTP failure by its status, the server's error code and its request id,
// never by its path or body. So no token, ticket, invite code, room name,
// channel name or message text can be in the ring.
import type { DiagnosticEventV1, DiagnosticSocket, DiagnosticVoiceStateName } from './schema/report';

export const ringSize = 100;

// What a recorder says. Fields are checked by record().
export type Recorded =
  | { readonly kind: 'socket_open'; readonly socket: DiagnosticSocket }
  | { readonly kind: 'socket_close'; readonly socket: DiagnosticSocket; readonly code: number }
  | { readonly kind: 'reconnect'; readonly socket: DiagnosticSocket; readonly attempt: number }
  | { readonly kind: 'http_failure'; readonly status: number; readonly error?: string; readonly requestId?: string | null }
  | { readonly kind: 'voice_state'; readonly state: DiagnosticVoiceStateName }
  | { readonly kind: 'device_error'; readonly name: string };

// The shapes pkg/schema/report.go accepts.
const errorCode = /^[a-z][a-z0-9_]{0,63}$/;
const requestId = /^[0-9a-f]{16}$/;
const errorName = /^[A-Z][A-Za-z]{0,58}Error$/;
const sockets: readonly string[] = ['messages', 'events', 'voice'];
const voiceStates: readonly string[] = ['idle', 'connecting', 'connected', 'reconnecting', 'unavailable', 'stopped'];

let ring: DiagnosticEventV1[] = [];
let voice: DiagnosticVoiceStateName | null = null;

const code = (n: number): number | undefined => (Number.isInteger(n) && n > 0 && n <= 4999 ? n : undefined);

// event is what is kept of r, or null when r cannot be kept as it is.
function event(r: Recorded, at: string): DiagnosticEventV1 | null {
  switch (r.kind) {
    case 'socket_open':
      return sockets.includes(r.socket) ? { at, kind: r.kind, socket: r.socket } : null;
    case 'socket_close': {
      if (!sockets.includes(r.socket)) return null;
      const c = code(r.code);
      return c === undefined ? { at, kind: r.kind, socket: r.socket } : { at, kind: r.kind, socket: r.socket, code: c };
    }
    case 'reconnect':
      return sockets.includes(r.socket) && Number.isInteger(r.attempt) && r.attempt > 0 ? { at, kind: r.kind, socket: r.socket, attempt: r.attempt } : null;
    case 'http_failure': {
      const e: DiagnosticEventV1 = { at, kind: r.kind };
      const c = code(r.status);
      if (c !== undefined) e.code = c;
      if (r.error !== undefined && errorCode.test(r.error)) e.error = r.error;
      if (typeof r.requestId === 'string' && requestId.test(r.requestId)) e.request_id = r.requestId;
      return e;
    }
    case 'voice_state':
      return voiceStates.includes(r.state) ? { at, kind: r.kind, state: r.state } : null;
    case 'device_error':
      return errorName.test(r.name) ? { at, kind: r.kind, name: r.name } : null;
  }
}

// record keeps one event, dropping the oldest past ringSize.
export function record(r: Recorded, now: number = Date.now()): void {
  const e = event(r, new Date(now).toISOString());
  if (e === null) return;
  if (r.kind === 'voice_state') voice = r.state;
  ring.push(e);
  if (ring.length > ringSize) ring = ring.slice(ring.length - ringSize);
}

// recent is a copy of the ring, oldest first.
export function recent(): DiagnosticEventV1[] {
  return ring.map((e) => ({ ...e }));
}

// voiceState is the voice connection's last recorded state, or null when the
// page has not used voice.
export function voiceState(): DiagnosticVoiceStateName | null {
  return voice;
}

// forget empties the ring. Tests use it; the page has no reason to.
export function forget(): void {
  ring = [];
  voice = null;
}
