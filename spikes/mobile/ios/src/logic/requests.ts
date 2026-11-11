// Copied from spikes/mobile/android at 477c0ce7 (#1390, M4-B, with its review fixes) for the iOS spike (#1394, M5-B).
// The request list: every route the spike may call, and the one place a
// request is built (#1109: Credential and API use). Routes and methods are
// those of web/src/platform/operations.ts at ed4a68f9.
//
// The list has no `.../voice/companion` route (the viewer and companion
// identities are never a phone's way into a room, ADR-016 Decision 3), and the
// camera routes take `started_at` and nothing else: never `participant=`.
// The bearer is sent only as an Authorization header (authorization below),
// never in a URL; a socket carries the single-use ticket from POST
// /v1/ws-tickets in its query and nothing else.
export type Method = 'GET' | 'POST' | 'DELETE';

export interface Route {
  readonly method: Method;
  readonly path: string;
  // The query keys the route accepts. Anything else is refused.
  readonly query: readonly string[];
  readonly socket: boolean;
}

function route(method: Method, path: string, query: readonly string[] = [], socket = false): Route {
  return { method, path, query, socket };
}

export const requests = {
  // Sign-in (M4-C): a password, or the code of a one-use sign-in link. Neither
  // carries a credential in the URL; both answer with the bearer, which the app
  // holds in memory only.
  login: route('POST', '/v1/login'),
  redeemSignInLink: route('POST', '/v1/sign-in-links/redeem'),
  listChannels: route('GET', '/v1/channels'),
  whoami: route('GET', '/v1/whoami'),
  wsTicket: route('POST', '/v1/ws-tickets'),
  voiceSession: route('POST', '/v1/channels/{channel}/voice/session'),
  voicePresence: route('GET', '/v1/channels/{channel}/voice'),
  voiceTransmit: route('POST', '/v1/channels/{channel}/voice/transmit'),
  voiceOccupancy: route('GET', '/v1/voice/occupancy'),
  claimCamera: route('POST', '/v1/channels/{channel}/voice/camera', ['started_at']),
  stopCamera: route('DELETE', '/v1/channels/{channel}/voice/camera', ['started_at']),
  voiceSocket: route('GET', '/v1/voice/ws', ['ticket'], true),
  occupancySocket: route('GET', '/v1/voice/occupancy/ws', ['ticket'], true),
} as const satisfies Record<string, Route>;

export type RequestName = keyof typeof requests;

export interface Built {
  readonly method: Method;
  // A path and query, relative to the server; the app adds the origin.
  readonly url: string;
}

// build makes a request from the list. A path parameter is escaped; a query
// key the route does not name throws, so `participant` cannot be added.
export function build(name: RequestName, params: Readonly<Record<string, string>> = {}, query: Readonly<Record<string, string | undefined>> = {}): Built {
  const r: Route = requests[name];
  const path = r.path.replace(/\{(\w+)\}/g, (_, key: string) => {
    const value = params[key];
    if (value === undefined || value === '') throw new Error(`request ${name}: missing path parameter ${key}`);
    return encodeURIComponent(value);
  });
  const pairs: string[] = [];
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined) continue;
    if (!r.query.includes(key)) throw new Error(`request ${name}: query key ${key} is not allowed`);
    pairs.push(`${encodeURIComponent(key)}=${encodeURIComponent(value)}`);
  }
  return { method: r.method, url: pairs.length === 0 ? path : `${path}?${pairs.join('&')}` };
}

// claimCameraRequest: without startedAt it starts a claim, which only the
// camera button asks for; with it, it names the claim it continues.
export function claimCameraRequest(channel: string, startedAt?: string): Built {
  return build('claimCamera', { channel }, { started_at: startedAt });
}

// stopCameraRequest names the claim it ends.
export function stopCameraRequest(channel: string, startedAt: string): Built {
  return build('stopCamera', { channel }, { started_at: startedAt });
}

// authorization is the only place the bearer becomes a header. It is held in
// memory by the app and never written anywhere.
export function authorization(bearer: string): Readonly<Record<string, string>> {
  return { Authorization: `Bearer ${bearer}` };
}
