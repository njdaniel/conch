// The one registry of IPC handlers, and the check every invocation passes
// before anything else (linux-desktop.md §4: "On every invocation and
// subscription, main checks event.sender against the owned webContents,
// senderFrame against its live main frame, the exact local origin and the
// expected document generation, plus schema, size and rate limits").
//
// Input: an invoke event from Electron, a function name and its arguments.
// Output: an Envelope: the function's value, or one opaque `refused`, which is
// counted and logged by reason (never by content) and executes nothing. The
// order is fixed: caller, name, rate, size and shape of the arguments, then
// the function. Arguments are plain data checked here; the renderer cannot
// name a channel, a URL, a header or a method.
import type { IpcMain } from 'electron';

import type { BridgeRequest, FeedRequest } from '../../web/src/platform/desktop/bridge.ts';
import { operations } from '../../web/src/platform/operations.ts';
import { channelOf, INVOKED_FUNCTIONS, type Envelope, type InvokedFunction } from '../shared/bridge-names.ts';
import { parseFeedRequest, type Feeds } from './api/feeds.ts';
import { targetOf, isOperationId } from './api/target.ts';
import { invocable, type Service, type SignInStep } from './api/service.ts';
import type { DocumentGate } from './document.ts';
import { HOST, SCHEME } from './scheme.ts';

export interface FrameLike {
  readonly frameToken: string;
  readonly parent: unknown;
  readonly detached: boolean;
  readonly url: string;
}

export interface ContentsLike {
  isDestroyed(): boolean;
  readonly mainFrame: FrameLike;
}

export interface InvokeEvent {
  readonly sender: ContentsLike;
  readonly senderFrame: FrameLike | null;
}

export type Refusal = 'sender' | 'destroyed' | 'frame' | 'subframe' | 'origin' | 'generation' | 'name' | 'rate' | 'size' | 'shape' | 'error';

export interface IpcDeps {
  // The one web contents that may call main.
  readonly owned: () => ContentsLike | null;
  readonly gate: DocumentGate;
  readonly service: Service;
  readonly feeds: Pick<Feeds, 'subscribe' | 'ack' | 'unsubscribe'>;
  readonly now: () => number;
  readonly log: (line: string) => void;
}

const ORIGIN_PREFIX = `${SCHEME}://${HOST}/`;

// caller says why an invocation is not from the one document of the one
// window, or null when it is.
export function checkCaller(event: InvokeEvent, owned: ContentsLike | null, gate: DocumentGate): Refusal | null {
  if (owned === null || event.sender !== owned) return 'sender';
  if (event.sender.isDestroyed()) return 'destroyed';
  const frame = event.senderFrame;
  if (frame === null || frame.detached) return 'frame';
  if (frame.parent !== null) return 'subframe';
  if (frame !== event.sender.mainFrame && frame.frameToken !== event.sender.mainFrame.frameToken) return 'frame';
  if (!frame.url.startsWith(ORIGIN_PREFIX)) return 'origin';
  if (!gate.current(frame)) return 'generation';
  return null;
}

// A fixed window of one minute per function.
class RateLimit {
  #start = 0;
  #count = 0;
  readonly #limit: number;

  constructor(limit: number) {
    this.#limit = limit;
  }

  allow(now: number): boolean {
    if (now - this.#start >= 60_000) {
      this.#start = now;
      this.#count = 0;
    }
    this.#count++;
    return this.#count <= this.#limit;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// plain says whether a value is JSON-shaped data: objects and arrays built from
// Object and Array, strings, finite numbers, booleans and null, nested at most
// sixteen deep, with at most ten thousand nodes. Anything else (a function, a
// Map, a typed array, a symbol, undefined inside a structure) is not data.
export function plain(value: unknown): boolean {
  let nodes = 0;
  const walk = (v: unknown, depth: number): boolean => {
    if (++nodes > 10_000 || depth > 16) return false;
    if (v === null || typeof v === 'string' || typeof v === 'boolean') return true;
    if (typeof v === 'number') return Number.isFinite(v);
    if (Array.isArray(v)) return v.every((item) => walk(item, depth + 1));
    if (typeof v !== 'object') return false;
    const proto: unknown = Object.getPrototypeOf(v);
    if (proto !== Object.prototype && proto !== null) return false;
    return Object.values(v).every((item) => walk(item, depth + 1));
  };
  return walk(value, 0);
}

interface Spec {
  readonly limit: number;
  readonly maxBytes: number;
  // The argument list as the function takes it, or null when it is not a
  // valid call.
  readonly parse: (args: readonly unknown[]) => readonly unknown[] | null;
  readonly run: (to: Handlers, args: readonly unknown[]) => unknown;
}

// What a function runs against: the service, the feeds, and the document
// generation of the caller (already checked to be the current one).
interface Handlers {
  readonly service: Service;
  readonly feeds: IpcDeps['feeds'];
  readonly document: number;
}

// A feed's number, which the preload chose: a positive whole number.
const isFeedId = (value: unknown): value is number => typeof value === 'number' && Number.isSafeInteger(value) && value > 0 && value <= 2 ** 31;

const feedId = (args: readonly unknown[]): readonly unknown[] | null => (args.length === 1 && isFeedId(args[0]) ? args : null);

const none = (args: readonly unknown[]): readonly unknown[] | null => (args.length === 0 ? args : null);

// signIn is the spec of a function that takes one request of the operation's
// request type and sends that operation.
function signIn(step: SignInStep): Spec {
  return {
    limit: 30,
    maxBytes: 16 * 1024,
    parse: (args) => {
      const [body] = args;
      return args.length === 1 && isRecord(body) && operations[step].request?.(body).length === 0 ? [body] : null;
    },
    run: ({ service }, [body]) => service.signIn(step, body as Record<string, unknown>),
  };
}

// request parses a BridgeRequest: its keys, the operation, whether the
// renderer may invoke it, its path values and query (as a target), and its
// body against the operation's request validator.
export function parseRequest(value: unknown): BridgeRequest | null {
  if (!isRecord(value)) return null;
  const allowed = new Set(['operation', 'params', 'query', 'body', 'keepalive']);
  if (!Object.keys(value).every((key) => allowed.has(key))) return null;
  const { operation, params, query, body, keepalive } = value;
  if (typeof operation !== 'string' || !isOperationId(operation) || !invocable(operation)) return null;
  if (params !== undefined && (!isRecord(params) || Object.keys(params).length > 8)) return null;
  if (query !== undefined && typeof query !== 'string') return null;
  if (keepalive !== undefined && typeof keepalive !== 'boolean') return null;
  if (targetOf(operation, params, query) === null) return null;
  const check = operations[operation].request;
  if (check === undefined ? body !== undefined : check(body).length > 0) return null;
  return {
    operation,
    ...(params === undefined ? {} : { params: params as Record<string, string | number> }),
    ...(query === undefined ? {} : { query }),
    ...(body === undefined ? {} : { body }),
    ...(keepalive === undefined ? {} : { keepalive }),
  };
}

export const SPECS: Readonly<Record<InvokedFunction, Spec>> = {
  serverAddress: { limit: 120, maxBytes: 0, parse: none, run: ({ service }) => service.serverAddress() },
  setServerAddress: {
    limit: 30,
    maxBytes: 4 * 1024,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' ? args : null),
    run: ({ service }, [origin]) => service.setServerAddress(origin as string),
  },
  request: {
    limit: 1200,
    maxBytes: 1024 * 1024,
    parse: (args) => {
      const parsed = args.length === 1 ? parseRequest(args[0]) : null;
      return parsed === null ? null : [parsed];
    },
    run: ({ service }, [request]) => service.request(request as BridgeRequest),
  },
  restore: { limit: 120, maxBytes: 0, parse: none, run: ({ service }) => service.restore() },
  signInWithCredential: {
    limit: 30,
    maxBytes: 8 * 1024,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' && /^\S{1,4096}$/u.test(args[0]) ? args : null),
    run: ({ service }, [credential]) => service.signInWithCredential(credential as string),
  },
  keepCredential: {
    limit: 30,
    maxBytes: 8 * 1024,
    parse: (args) => (args.length === 1 && typeof args[0] === 'string' && /^\S{1,4096}$/u.test(args[0]) ? args : null),
    run: ({ service }, [credential]) => service.keepCredential(credential as string),
  },
  signInWithPassword: signIn('login'),
  signInWithCode: signIn('loginTotp'),
  redeemInvite: signIn('redeemInvite'),
  redeemInviteAsGuest: signIn('redeemInviteGuest'),
  redeemSignInLink: signIn('redeemSignInLink'),
  redeemSetupToken: signIn('setupRedeem'),
  resetPassword: signIn('resetPassword'),
  joinAsGuest: signIn('joinGuest'),
  createAccount: signIn('joinAccount'),
  signOut: { limit: 120, maxBytes: 0, parse: none, run: ({ service }) => service.signOut() },
  // The feeds (issue #943). A reconnecting feed subscribes at most once a
  // second, and an acknowledgement follows each batch of frames; past these
  // a batch waits, and a feed that waits too long is closed and resubscribes.
  feedSubscribe: {
    limit: 600,
    maxBytes: 1024,
    parse: (args) => {
      const [id, request] = args;
      const feed = args.length === 2 && isFeedId(id) ? parseFeedRequest(request) : null;
      return feed === null ? null : [id, feed];
    },
    run: ({ feeds, document }, [id, request]) => feeds.subscribe(document, id as number, request as FeedRequest),
  },
  feedAck: { limit: 60_000, maxBytes: 32, parse: feedId, run: ({ feeds, document }, [id]) => feeds.ack(document, id as number) },
  feedUnsubscribe: { limit: 600, maxBytes: 32, parse: feedId, run: ({ feeds, document }, [id]) => feeds.unsubscribe(document, id as number) },
};

export class Ipc {
  readonly #deps: IpcDeps;
  readonly #limits = new Map<InvokedFunction, RateLimit>(INVOKED_FUNCTIONS.map((name) => [name, new RateLimit(SPECS[name].limit)]));
  #refusals = 0;

  constructor(deps: IpcDeps) {
    this.#deps = deps;
  }

  // How many invocations have been refused since start.
  get refusals(): number {
    return this.#refusals;
  }

  #refuse(reason: Refusal): Envelope {
    this.#refusals++;
    this.#deps.log(`conch-desktop: refused an IPC call (${reason}); ${String(this.#refusals)} so far`);
    return { ok: false, kind: 'refused' };
  }

  async invoke(event: InvokeEvent, name: unknown, args: readonly unknown[]): Promise<Envelope> {
    const why = checkCaller(event, this.#deps.owned(), this.#deps.gate);
    if (why !== null) return this.#refuse(why);
    if (typeof name !== 'string' || !(INVOKED_FUNCTIONS as readonly string[]).includes(name)) return this.#refuse('name');
    const fn = name as InvokedFunction;
    const spec = SPECS[fn];
    if (this.#limits.get(fn)?.allow(this.#deps.now()) !== true) return this.#refuse('rate');
    if (!plain(args)) return this.#refuse('shape');
    if (JSON.stringify(args).length > spec.maxBytes + 2) return this.#refuse('size');
    const parsed = spec.parse(args);
    if (parsed === null) return this.#refuse('shape');
    try {
      const to: Handlers = { service: this.#deps.service, feeds: this.#deps.feeds, document: this.#deps.gate.generation };
      return { ok: true, value: await spec.run(to, parsed) };
    } catch {
      // A bug in a function is not the renderer's to learn about.
      return this.#refuse('error');
    }
  }

  // register installs one handler per invoked function and nothing else.
  register(ipcMain: Pick<IpcMain, 'handle'>): void {
    for (const name of INVOKED_FUNCTIONS) {
      ipcMain.handle(channelOf(name), (event, ...args: unknown[]) => this.invoke(event, name, args));
    }
  }
}
