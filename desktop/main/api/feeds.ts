// The renderer's live feeds, opened and held by main (issue #943;
// linux-desktop.md §4: "Feed subscribe, unsubscribe: approved feed kind,
// channel, session, cursor; main owns socket handles, rate limits, queue
// bounds and teardown; sends validated events").
//
// Input: a subscription the IPC layer has checked (a number the preload chose,
// a feed kind and its parameters), the identity in force, and the document
// that asked. Output: feed events for that document: open, batches of frames
// each validated with its generated validator, and one close.
//
// What it owns, and the rules it keeps:
//  - the socket. Its URL is the selected origin plus the feed's fixed route
//    and a query main builds from the validated parameters; the bearer is the
//    Authorization header and never part of a URL; no Origin, Cookie or
//    ticket is sent (Node's WebSocket, measured on the pinned Electron:
//    desktop/README.md). conchd reads a ticket only when there is no
//    Authorization header, so none is asked for and none exists to be logged;
//  - every frame: text only, at most MAX_FRAME_BYTES, valid for its feed by
//    the generated validators of web/src/schema. Anything else is dropped and
//    counted, never sent on;
//  - the queue: frames go to the renderer one batch at a time, and the next
//    batch waits until the preload acknowledges the last. Frames that wait
//    are bounded in number and bytes; past either bound the socket is closed
//    and the renderer told 1013, which its feed state machine retries like any
//    other loss (and the messages feed then resumes after the newest id held);
//  - teardown: an unsubscribe, the end of the document that subscribed (a
//    reload, a crash, the window closing) and the end of the identity (sign
//    out, a new sign-in, another server) each close the sockets they own, and
//    the last two do it synchronously, before anything else runs. A socket
//    event that arrives after its subscription ended is dropped here, so a
//    frame from a closed socket never reaches the renderer.
//
// Reconnecting and the heartbeat watchdog are the renderer's own
// (web/src/state/feed.ts), as in the browser; main reports open, close and the
// frames, and the close of a socket that failed before it opened is told as
// 1008 when the credential turns out to have ended (one whoami says so), as
// the browser learns from a ticket refused with 401.
import { parseEventFrame } from '../../../web/src/agents/frames.ts';
import type { FeedKind, FeedRequest } from '../../../web/src/platform/desktop/bridge.ts';
import { operations, type OperationId } from '../../../web/src/platform/operations.ts';
import { heartbeatV1, messageV2, voiceOccupancyV1, voicePresenceV1 } from '../../../web/src/schema/generated.ts';
import type { FeedEvent } from '../../shared/bridge-names.ts';
import type { Client } from './client.ts';
import type { Capture, Session } from './session.ts';

// How many feeds one document may hold at once. The web client holds four at
// most (a channel's messages, events and voice, and the occupancy), and a
// fifth for a moment while it changes channel.
export const MAX_FEEDS = 32;
export const MAX_FRAME_BYTES = 1024 * 1024;
export const MAX_QUEUED_FRAMES = 512;
export const MAX_QUEUED_BYTES = 4 * 1024 * 1024;
const MAX_REASON_LENGTH = 123;

// The close codes main itself reports.
export const CLOSE_SIGNED_OUT = 1008;
export const CLOSE_BEHIND = 1013;
// What a socket that ended without a close frame reports, as a browser's does.
export const CLOSE_ABNORMAL = 1006;

// The socket main opens, as the feeds need it: Node's WebSocket in the app
// (nodeConnect), a fake in the tests.
export interface SocketHandlers {
  readonly open: () => void;
  readonly frame: (data: unknown) => void;
  readonly close: (code: number, reason: string) => void;
}

export interface SocketHandle {
  close(): void;
}

export type Connect = (url: string, headers: Readonly<Record<string, string>>, on: SocketHandlers) => SocketHandle;

// nodeConnect opens a WebSocket with Node's own client (undici). An error is
// always followed by a close, which is what is reported. Binary frames arrive
// as an ArrayBuffer and are refused by the validation.
export const nodeConnect: Connect = (url, headers, on) => {
  const socket = new WebSocket(url, { headers: { ...headers } });
  socket.binaryType = 'arraybuffer';
  socket.addEventListener('open', () => {
    on.open();
  });
  socket.addEventListener('message', (event) => {
    on.frame(event.data);
  });
  socket.addEventListener('close', (event) => {
    on.close(event.code, event.reason);
  });
  return {
    close: () => {
      socket.close();
    },
  };
};

const ROUTES: Readonly<Record<FeedKind, OperationId>> = {
  messages: 'messagesSocket',
  events: 'eventsSocket',
  voice: 'voiceSocket',
  occupancy: 'occupancySocket',
};

// The parameters each feed takes, and no others.
const PARAMETERS: Readonly<Record<FeedKind, readonly string[]>> = {
  messages: ['channel', 'after'],
  events: ['channel', 'session'],
  voice: ['channel'],
  occupancy: [],
};

const CONTROL = /[\u0000-\u001f\u007f-\u009f]/u;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isFeedKind(value: unknown): value is FeedKind {
  return typeof value === 'string' && Object.hasOwn(ROUTES, value);
}

// parseFeedRequest gives the subscription the renderer asked for, or null
// when it is not exactly one: a known kind with exactly its parameters, a
// channel name of 1 to 256 characters with no control character, a cursor
// that is a whole number, a session that is a positive one.
export function parseFeedRequest(value: unknown): FeedRequest | null {
  if (!isRecord(value)) return null;
  const { kind, channel, session, after } = value;
  if (!isFeedKind(kind)) return null;
  const wanted = PARAMETERS[kind];
  const keys = Object.keys(value).filter((key) => key !== 'kind');
  if (keys.length !== wanted.length || !keys.every((key) => wanted.includes(key))) return null;
  if (wanted.includes('channel') && (typeof channel !== 'string' || channel.length === 0 || channel.length > 256 || CONTROL.test(channel))) return null;
  if (wanted.includes('after') && (typeof after !== 'number' || !Number.isSafeInteger(after) || after < 0)) return null;
  if (wanted.includes('session') && (typeof session !== 'number' || !Number.isSafeInteger(session) || session <= 0)) return null;
  return {
    kind,
    ...(typeof channel === 'string' ? { channel } : {}),
    ...(typeof session === 'number' ? { session } : {}),
    ...(typeof after === 'number' ? { after } : {}),
  };
}

// feedUrl is the socket's address: the origin's ws or wss form, the feed's
// route from the operation table, and its query. The messages feed always
// asks for the heartbeat, which the renderer's silence watchdog lives on.
export function feedUrl(origin: string, request: FeedRequest): string {
  const base = origin.replace(/^http/u, 'ws');
  const query = new URLSearchParams();
  if (request.channel !== undefined) query.set('channel', request.channel);
  if (request.kind === 'messages') {
    query.set('after', String(request.after ?? 0));
    query.set('heartbeat', '1');
  }
  if (request.session !== undefined) query.set('session', String(request.session));
  const search = query.toString();
  return `${base}${operations[ROUTES[request.kind]].path}${search === '' ? '' : `?${search}`}`;
}

function parsed(data: string): unknown {
  try {
    return JSON.parse(data) as unknown;
  } catch {
    return undefined;
  }
}

const isHeartbeat = (doc: unknown): boolean => doc !== undefined && heartbeatV1(doc).length === 0;

// validFrame says whether a text frame is one its feed sends: a v2 message or
// the heartbeat on the messages feed; a document the events feed's own parser
// accepts, or the heartbeat, on the events feed; a presence document on the
// voice feed; an occupancy document on the occupancy feed.
export function validFrame(kind: FeedKind, data: string): boolean {
  switch (kind) {
    case 'messages': {
      const doc = parsed(data);
      return doc !== undefined && (messageV2(doc).length === 0 || isHeartbeat(doc));
    }
    case 'events':
      return parseEventFrame(data) !== null || isHeartbeat(parsed(data));
    case 'voice': {
      const doc = parsed(data);
      return doc !== undefined && voicePresenceV1(doc).length === 0;
    }
    case 'occupancy': {
      const doc = parsed(data);
      return doc !== undefined && voiceOccupancyV1(doc).length === 0;
    }
  }
}

// Why a socket was closed, for the log line.
type Ending = 'unsubscribed' | 'document' | 'signed out' | 'server' | 'behind';

interface Subscription {
  readonly key: string;
  readonly id: number;
  readonly document: number;
  readonly kind: FeedKind;
  readonly capture: Capture;
  socket: SocketHandle | null;
  opened: boolean;
  ended: boolean;
  // Frames waiting for the batch in flight to be acknowledged.
  queue: string[];
  queuedBytes: number;
  inFlight: boolean;
  readonly onAbort: () => void;
}

export interface FeedsDeps {
  readonly session: Session;
  // For the one whoami after a socket that failed before it opened.
  readonly client: Pick<Client, 'send'>;
  readonly connect: Connect;
  // Sends an event to the document that subscribed, if it is still the one
  // shown; the app drops it otherwise.
  readonly deliver: (document: number, event: FeedEvent) => void;
  // One line; never a URL, a channel, a frame or a credential.
  readonly log: (line: string) => void;
  readonly maxQueuedFrames?: number;
  readonly maxQueuedBytes?: number;
}

const keyOf = (document: number, id: number): string => `${String(document)}/${String(id)}`;

function reasonOf(reason: unknown): string {
  return typeof reason === 'string' ? reason.slice(0, MAX_REASON_LENGTH) : '';
}

function closeCodeOf(code: unknown): number {
  return typeof code === 'number' && Number.isInteger(code) && code >= 1000 && code <= 4999 ? code : CLOSE_ABNORMAL;
}

export class Feeds {
  readonly #deps: FeedsDeps;
  readonly #subscriptions = new Map<string, Subscription>();
  #invalid = 0;
  #behind = 0;

  constructor(deps: FeedsDeps) {
    this.#deps = deps;
  }

  // How many sockets main holds open (or opening) now.
  get open(): number {
    let n = 0;
    for (const sub of this.#subscriptions.values()) if (sub.socket !== null) n++;
    return n;
  }

  // How many frames have been dropped as invalid since start.
  get invalid(): number {
    return this.#invalid;
  }

  // How many feeds have been closed because the renderer fell behind.
  get behind(): number {
    return this.#behind;
  }

  // subscribe opens the feed request names for document under the number id.
  // false when it is refused: a number already in use, or too many feeds. A
  // subscription made with nobody signed in is accepted and closed at once,
  // 1008, as conchd closes a socket whose credential has ended.
  subscribe(document: number, id: number, request: FeedRequest): boolean {
    const key = keyOf(document, id);
    if (this.#subscriptions.has(key)) return false;
    let held = 0;
    for (const sub of this.#subscriptions.values()) if (sub.document === document) held++;
    if (held >= MAX_FEEDS) return false;

    const { session } = this.#deps;
    const capture = session.capture();
    if (capture.token === null || capture.origin === null || capture.signal.aborted) {
      queueMicrotask(() => {
        this.#deps.deliver(document, { id, type: 'close', code: CLOSE_SIGNED_OUT, reason: 'signed out' });
      });
      return true;
    }

    const sub: Subscription = {
      key,
      id,
      document,
      kind: request.kind,
      capture,
      socket: null,
      opened: false,
      ended: false,
      queue: [],
      queuedBytes: 0,
      inFlight: false,
      onAbort: () => {
        this.#end(sub, 'signed out', { code: CLOSE_SIGNED_OUT, reason: 'signed out' });
      },
    };
    this.#subscriptions.set(key, sub);
    // The end of the identity ends the feed in the same turn: Session aborts
    // this signal before it takes the next credential.
    capture.signal.addEventListener('abort', sub.onAbort, { once: true });

    const live = (): boolean => !sub.ended && this.#subscriptions.get(key) === sub && session.current(capture);
    try {
      sub.socket = this.#deps.connect(feedUrl(capture.origin, request), { Authorization: `Bearer ${capture.token}` }, {
        open: () => {
          if (!live()) return;
          sub.opened = true;
          this.#deps.deliver(document, { id, type: 'open' });
        },
        frame: (data) => {
          if (live()) this.#frame(sub, data);
        },
        close: (code, reason) => {
          if (live()) this.#closed(sub, closeCodeOf(code), reasonOf(reason));
        },
      });
    } catch {
      // An address Node's WebSocket would not take: a socket that never
      // opened.
      queueMicrotask(() => {
        if (live()) this.#closed(sub, CLOSE_ABNORMAL, '');
      });
    }
    this.#deps.log(`conch-desktop: feed ${sub.kind} opening; ${String(this.open)} open`);
    return true;
  }

  // ack is the preload's word that it has handed the last batch to the page.
  // false when there was no batch in flight for that number.
  ack(document: number, id: number): boolean {
    const sub = this.#subscriptions.get(keyOf(document, id));
    if (sub === undefined || !sub.inFlight) return false;
    sub.inFlight = false;
    this.#flush(sub);
    return true;
  }

  // unsubscribe closes the feed. Nothing more is sent for it.
  unsubscribe(document: number, id: number): boolean {
    const sub = this.#subscriptions.get(keyOf(document, id));
    if (sub === undefined) return false;
    this.#end(sub, 'unsubscribed', null);
    return true;
  }

  // endDocument closes every feed of every document but current: the page
  // that subscribed has gone (a reload, a crash, the window closing), so
  // there is no one to tell.
  endDocument(current: number): void {
    for (const sub of [...this.#subscriptions.values()]) {
      if (sub.document !== current) this.#end(sub, 'document', null);
    }
  }

  #frame(sub: Subscription, data: unknown): void {
    if (typeof data !== 'string' || Buffer.byteLength(data, 'utf8') > MAX_FRAME_BYTES || !validFrame(sub.kind, data)) {
      this.#invalid++;
      // The first, then every hundredth: a server that sends nothing else
      // must not fill the log.
      if (this.#invalid === 1 || this.#invalid % 100 === 0) {
        this.#deps.log(`conch-desktop: dropped an invalid ${sub.kind} frame; ${String(this.#invalid)} so far`);
      }
      return;
    }
    sub.queue.push(data);
    sub.queuedBytes += Buffer.byteLength(data, 'utf8');
    if (sub.queue.length > (this.#deps.maxQueuedFrames ?? MAX_QUEUED_FRAMES) || sub.queuedBytes > (this.#deps.maxQueuedBytes ?? MAX_QUEUED_BYTES)) {
      this.#behind++;
      this.#end(sub, 'behind', { code: CLOSE_BEHIND, reason: 'the app fell behind' });
      return;
    }
    this.#flush(sub);
  }

  // flush sends the waiting frames as one batch, unless one is in flight.
  #flush(sub: Subscription): void {
    if (sub.ended || sub.inFlight || sub.queue.length === 0) return;
    const frames = sub.queue;
    sub.queue = [];
    sub.queuedBytes = 0;
    sub.inFlight = true;
    this.#deps.deliver(sub.document, { id: sub.id, type: 'frames', frames });
  }

  // closed is the server's close, or the socket's failure. What it sent before
  // closing is sent on first. A socket that never opened may have been
  // refused for its credential, which a WebSocket does not say: one whoami
  // does, and a 401 there ends the sign-in (the client forgets it), which is
  // told as 1008.
  #closed(sub: Subscription, code: number, reason: string): void {
    const opened = sub.opened;
    const frames = sub.queue;
    this.#remove(sub, 'server');
    if (frames.length > 0) this.#deps.deliver(sub.document, { id: sub.id, type: 'frames', frames });
    if (opened) {
      this.#deps.deliver(sub.document, { id: sub.id, type: 'close', code, reason });
      return;
    }
    void this.#deps.client.send({ op: 'whoami', capture: sub.capture }).then(
      (exchange) => {
        const ended = exchange.kind === 'unauthorized';
        this.#deps.deliver(sub.document, ended ? { id: sub.id, type: 'close', code: CLOSE_SIGNED_OUT, reason: 'signed out' } : { id: sub.id, type: 'close', code, reason });
      },
      () => {
        this.#deps.deliver(sub.document, { id: sub.id, type: 'close', code, reason });
      },
    );
  }

  // end closes the socket main holds and, when told, says so to the page.
  #end(sub: Subscription, why: Ending, tell: { readonly code: number; readonly reason: string } | null): void {
    if (sub.ended) return;
    const socket = sub.socket;
    this.#remove(sub, why);
    socket?.close();
    if (tell !== null) this.#deps.deliver(sub.document, { id: sub.id, type: 'close', ...tell });
  }

  #remove(sub: Subscription, why: Ending): void {
    sub.ended = true;
    sub.socket = null;
    sub.queue = [];
    sub.queuedBytes = 0;
    sub.capture.signal.removeEventListener('abort', sub.onAbort);
    this.#subscriptions.delete(sub.key);
    this.#deps.log(`conch-desktop: feed ${sub.kind} closed (${why}); ${String(this.open)} open`);
  }
}
