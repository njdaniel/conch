// The one kind of socket main opens for a feed: Node's own WebSocket (undici),
// as measured on the pinned Electron (desktop/README.md, "Live feeds").
// Input: a URL main built, the bearer header and the handlers. Output: a
// handle that closes the socket, and destroys it when its server will not
// finish the close.
//
// Two limits that a WebSocket alone does not give, both from undici's own
// Agent, reached through Node's global dispatcher (no undici package):
//  - the frame-size cap, at the frame's header. The Agent's
//    webSocket.maxPayloadSize makes undici fail the connection as soon as a
//    header says more than MAX_FRAME_BYTES, before the payload is read, where
//    its default would buffer up to 128 MiB of it;
//  - the raw socket. undici's close sends the close frame and waits for the
//    server's, with no deadline of its own. The Agent's connect hook makes the
//    TCP (or TLS) connection here, so the handle can destroy it.
// The hook makes the connection as undici's default connector does, with
// Node's net and tls and their defaults: no CA list and no verification option
// is passed, so the process-wide trust (main/api/trust.ts) and the hostname
// check apply unchanged, and a certificate this machine does not trust fails
// the socket exactly as it fails a fetch.
//
// One Agent per socket: what its connect hook made is that socket's alone.
// When Node's global dispatcher is not undici's Agent the socket is a plain
// WebSocket, without either limit, and main says so once in its log.
import { isIP, connect as netConnect, type Socket } from 'node:net';
import { connect as tlsConnect } from 'node:tls';

import { loopback } from './origin.ts';

export const MAX_FRAME_BYTES = 1024 * 1024;
export const CONNECT_TIMEOUT_MS = 30_000;

export interface SocketHandlers {
  readonly open: () => void;
  readonly frame: (data: unknown) => void;
  readonly close: (code: number, reason: string) => void;
}

export interface SocketHandle {
  // Starts the closing handshake. The close handler runs when it ends.
  close(): void;
  // Ends the connection at once, without the handshake. false when there is
  // no connection this handle can reach (a plain WebSocket, or one whose
  // connection has not been made yet).
  destroy(): boolean;
}

export type Connect = (url: string, headers: Readonly<Record<string, string>>, on: SocketHandlers) => SocketHandle;

type WebSocketInit = Exclude<ConstructorParameters<typeof WebSocket>[1], string | string[] | undefined>;
type Dispatcher = NonNullable<WebSocketInit['dispatcher']>;

// What undici hands its connect hook, and what the hook answers.
interface ConnectOptions {
  readonly hostname: string;
  readonly port: string;
  readonly protocol: string;
  readonly servername?: string | null;
}
type Connector = (options: ConnectOptions, callback: (error: Error | null, socket: Socket | null) => void) => void;

interface AgentOptions {
  readonly webSocket: { readonly maxPayloadSize: number };
  readonly connect: Connector;
}
export type AgentConstructor = new (options: AgentOptions) => Dispatcher;

// agentConstructor is undici's Agent, the class of Node's global dispatcher,
// or null when the dispatcher is not one.
export function agentConstructor(): AgentConstructor | null {
  const dispatcher: unknown = Reflect.get(globalThis, Symbol.for('undici.globalDispatcher.1'));
  if (typeof dispatcher !== 'object' || dispatcher === null) return null;
  const ctor: unknown = dispatcher.constructor;
  return typeof ctor === 'function' && ctor.name === 'Agent' ? (ctor as AgentConstructor) : null;
}

// connector makes the connection undici asks for, and gives it to made: TLS
// for https:, plain TCP for http: to a loopback host only (the address rule of
// ./origin.ts), and nothing at all for anything else. The address was checked
// when it was set; this holds if that check is ever loosened.
export function connector(made: (socket: Socket) => void): Connector {
  return (options, callback) => {
    const secure = options.protocol === 'https:';
    if (!secure && !(options.protocol === 'http:' && loopback(options.hostname))) {
      callback(new Error('a feed connects with TLS, or in plain text to this machine only'), null);
      return;
    }
    const host = options.hostname.replace(/^\[(.*)\]$/u, '$1');
    const port = Number(options.port) || (secure ? 443 : 80);
    const servername = options.servername ?? (isIP(host) === 0 ? host : undefined);
    const socket = secure ? tlsConnect({ host, port, ALPNProtocols: ['http/1.1'], ...(servername === undefined ? {} : { servername }) }) : netConnect({ host, port });
    made(socket);
    socket.setNoDelay(true);
    let settled = false;
    const settle = (error: Error | null): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      callback(error, error === null ? socket : null);
    };
    const timer = setTimeout(() => {
      socket.destroy();
      settle(new Error('the connection timed out'));
    }, CONNECT_TIMEOUT_MS);
    timer.unref();
    socket.once(secure ? 'secureConnect' : 'connect', () => {
      settle(null);
    });
    socket.once('error', (error: Error) => {
      settle(error);
    });
  };
}

// createNodeConnect is the Connect of the app. log is told once if the limits
// above are not available. Node sets its global dispatcher at its first
// request (in the app, a sign-in always comes before a feed), so one that is
// missing is looked for again at the next socket.
export function createNodeConnect(log: (line: string) => void, agent: () => AgentConstructor | null = agentConstructor): Connect {
  let Agent: AgentConstructor | null = null;
  let told = false;
  return (url, headers, on) => {
    Agent ??= agent();
    if (Agent === null && !told) {
      told = true;
      log('conch-desktop: Node’s dispatcher is not undici’s Agent; feeds have no frame-size cap at the header and no forced close');
    }
    let raw: Socket | null = null;
    const dispatcher =
      Agent === null
        ? null
        : new Agent({
            webSocket: { maxPayloadSize: MAX_FRAME_BYTES },
            connect: connector((socket) => {
              raw = socket;
            }),
          });
    const socket = new WebSocket(url, { headers: { ...headers }, ...(dispatcher === null ? {} : { dispatcher }) });
    // Binary frames arrive as an ArrayBuffer and are refused by the feeds.
    socket.binaryType = 'arraybuffer';
    socket.addEventListener('open', () => {
      on.open();
    });
    socket.addEventListener('message', (event) => {
      on.frame(event.data);
    });
    // An error is always followed by a close, which is what is reported.
    socket.addEventListener('close', (event) => {
      void dispatcher?.close().catch(() => undefined);
      on.close(event.code, event.reason);
    });
    return {
      close: () => {
        socket.close();
      },
      destroy: () => {
        const connection: Socket | null = raw;
        if (connection === null) return false;
        connection.destroy();
        void dispatcher?.destroy().catch(() => undefined);
        return true;
      },
    };
  };
}
