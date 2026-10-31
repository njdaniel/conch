import { readFileSync } from 'node:fs';
import { createServer, type IncomingHttpHeaders, type Server } from 'node:http';
import { execFile } from 'node:child_process';
import { createServer as createHttpsServer } from 'node:https';
import { createHash } from 'node:crypto';
import type { AddressInfo, Socket } from 'node:net';
import { join, resolve } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import type { FeedRequest } from '../../../web/src/platform/desktop/bridge.ts';
import type { Exchange, Send } from '../../main/api/client.ts';
import {
  CLOSE_BEHIND,
  CLOSE_DEADLINE_MS,
  CLOSE_SIGNED_OUT,
  feedUrl,
  Feeds,
  MAX_FEEDS,
  MAX_FRAME_BYTES,
  MAX_SOCKETS,
  parseFeedRequest,
  validFrame,
  type Connect,
  type SocketHandlers,
} from '../../main/api/feeds.ts';
import { agentConstructor, createNodeConnect } from '../../main/api/socket.ts';
import { Session } from '../../main/api/session.ts';
import { makePki, type Pki } from './support.ts';
import type { FeedEvent } from '../../shared/bridge-names.ts';

// Frames are the Go golden fixtures, as the server writes them.
const testdata = resolve(import.meta.dirname, '../../../pkg/schema/testdata');
const fixture = (name: string): string => JSON.stringify(JSON.parse(readFileSync(join(testdata, `${name}.json`), 'utf8')) as unknown);
const MESSAGE = fixture('message-v2');
const HEARTBEAT = fixture('heartbeat-v1');
const PRESENCE = fixture('voice-presence-v1');
const OCCUPANCY = fixture('voice-occupancy-v1');
const ROSTER = fixture('session-roster-v1');

const TOKEN = 'conch_feedtest_bearer_canary_0123456789';

// A socket main opened, as a fake: what it was opened with, and the server's
// side of it.
interface FakeSocket {
  readonly url: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly on: SocketHandlers;
  closed: boolean;
  destroyed: boolean;
  // Whether destroy() can reach a connection.
  destroyable: boolean;
}

interface Rig {
  readonly feeds: Feeds;
  readonly session: Session;
  readonly sockets: FakeSocket[];
  readonly events: [number, FeedEvent][];
  readonly logs: string[];
  readonly sent: Send[];
  // What the one whoami after a failed open answers.
  whoami: Exchange;
  // Who heard what, in order: socket closes and the identity's first request.
  readonly order: string[];
  // The close deadlines set, to be run by the test.
  readonly timers: { run: () => void; ms: number; cleared: boolean }[];
}

function rig(limits: { maxQueuedFrames?: number; maxQueuedBytes?: number } = {}): Rig {
  const session = new Session();
  session.setOrigin('http://127.0.0.1:8080');
  session.adopt(TOKEN);
  const sockets: FakeSocket[] = [];
  const events: [number, FeedEvent][] = [];
  const logs: string[] = [];
  const sent: Send[] = [];
  const order: string[] = [];
  const timers: Rig['timers'] = [];
  const r: Rig = {
    timers,
    session,
    sockets,
    events,
    logs,
    sent,
    order,
    whoami: { kind: 'answer', status: 200, ok: true },
    feeds: undefined as unknown as Feeds,
  };
  const connect: Connect = (url, headers, on) => {
    const socket: FakeSocket = { url, headers, on, closed: false, destroyed: false, destroyable: true };
    sockets.push(socket);
    return {
      close: () => {
        socket.closed = true;
        order.push(`closed ${url}`);
      },
      destroy: () => {
        if (!socket.destroyable) return false;
        socket.destroyed = true;
        return true;
      },
    };
  };
  const client = {
    send: (request: Send): Promise<Exchange> => {
      sent.push(request);
      // As the real client: a 401 on a bearer operation forgets the sign-in.
      if (r.whoami.kind === 'unauthorized') session.forget();
      return Promise.resolve(r.whoami);
    },
  };
  const feeds = new Feeds({
    session,
    client,
    connect,
    deliver: (document, event) => {
      events.push([document, event]);
    },
    log: (line) => logs.push(line),
    setTimer: (run, ms) => {
      const timer = { run, ms, cleared: false };
      timers.push(timer);
      return timer;
    },
    clearTimer: (timer) => {
      (timer as Rig['timers'][number]).cleared = true;
    },
    ...limits,
  });
  return Object.assign(r, { feeds });
}

const occupancy: FeedRequest = { kind: 'occupancy' };
const messages: FeedRequest = { kind: 'messages', channel: 'general', after: 4 };

const flush = async (): Promise<void> => {
  for (let i = 0; i < 5; i++) await Promise.resolve();
};

const types = (r: Rig): string[] => r.events.map(([, e]) => (e.type === 'close' ? `close ${String(e.code)}` : e.type === 'frames' ? `frames ${String(e.frames.length)}` : e.type));

describe('what the renderer may ask for', () => {
  it.each<[unknown, FeedRequest | null]>([
    [{ kind: 'messages', channel: 'general', after: 0 }, { kind: 'messages', channel: 'general', after: 0 }],
    [{ kind: 'events', channel: 'general', session: 3 }, { kind: 'events', channel: 'general', session: 3 }],
    [{ kind: 'voice', channel: 'général room' }, { kind: 'voice', channel: 'général room' }],
    [{ kind: 'occupancy' }, { kind: 'occupancy' }],
    [{ kind: 'messages', channel: 'general' }, null],
    [{ kind: 'messages', channel: 'general', after: 1.5 }, null],
    [{ kind: 'messages', channel: 'general', after: 0, session: 1 }, null],
    [{ kind: 'events', channel: 'general', session: -2 }, null],
    [{ kind: 'voice' }, null],
    [{ kind: 'voice', channel: 7 }, null],
    [{ kind: 'voice', channel: 'a\u0000b' }, null],
    [{ kind: 'occupancy', ticket: 'x' }, null],
    [{ kind: 'occupancy', url: 'wss://evil.example/' }, null],
    [{ kind: 'wsTicket' }, null],
    [{ kind: 'constructor' }, null],
    ['occupancy', null],
    [null, null],
  ])('%j is %j', (given, wanted) => {
    expect(parseFeedRequest(given)).toEqual(wanted);
  });

  it('is turned into the feed’s fixed route on the selected origin, with a query main builds', () => {
    expect(feedUrl('http://127.0.0.1:8080', messages)).toBe('ws://127.0.0.1:8080/v2/ws?channel=general&after=4&heartbeat=1');
    expect(feedUrl('https://chat.example', { kind: 'events', channel: 'a b&c=d', session: 9 })).toBe('wss://chat.example/v1/events/ws?channel=a+b%26c%3Dd&session=9');
    expect(feedUrl('https://chat.example', { kind: 'voice', channel: 'general' })).toBe('wss://chat.example/v1/voice/ws?channel=general');
    expect(feedUrl('https://chat.example', occupancy)).toBe('wss://chat.example/v1/voice/occupancy/ws');
  });
});

describe('the socket main opens', () => {
  it('carries the bearer in the Authorization header alone: no ticket, no Origin, nothing in the URL', () => {
    const r = rig();
    expect(r.feeds.subscribe(1, 1, messages)).toBe(true);
    const [socket] = r.sockets;
    expect(socket?.headers).toEqual({ Authorization: `Bearer ${TOKEN}` });
    expect(socket?.url).not.toContain(TOKEN);
    expect(socket?.url).not.toContain('ticket');
    // Nothing asked for a ticket either.
    expect(r.sent).toEqual([]);
    expect(r.feeds.open).toBe(1);
    expect(r.logs.join('\n')).not.toContain(TOKEN);
    expect(r.logs.join('\n')).not.toContain('general');
  });

  it('is not opened for nobody: a subscription with no sign-in is closed 1008 at once', async () => {
    const r = rig();
    r.session.forget();
    expect(r.feeds.subscribe(1, 1, occupancy)).toBe(true);
    await flush();
    expect(r.sockets).toHaveLength(0);
    expect(types(r)).toEqual(['close 1008']);
  });

  it('refuses a number already in use, and more feeds than one document may hold', () => {
    const r = rig();
    expect(r.feeds.subscribe(1, 1, occupancy)).toBe(true);
    expect(r.feeds.subscribe(1, 1, occupancy)).toBe(false);
    for (let id = 2; id <= MAX_FEEDS; id++) expect(r.feeds.subscribe(1, id, occupancy)).toBe(true);
    expect(r.feeds.subscribe(1, MAX_FEEDS + 1, occupancy)).toBe(false);
    // Another document's count is its own.
    expect(r.feeds.subscribe(2, 1, occupancy)).toBe(true);
  });
});

describe('frames', () => {
  it.each<[FeedRequest['kind'], string, boolean]>([
    ['messages', MESSAGE, true],
    ['messages', HEARTBEAT, true],
    ['messages', PRESENCE, false],
    ['messages', ROSTER, false],
    ['events', ROSTER, true],
    ['events', MESSAGE, true],
    ['events', HEARTBEAT, true],
    ['events', OCCUPANCY, false],
    ['voice', PRESENCE, true],
    ['voice', MESSAGE, false],
    ['occupancy', OCCUPANCY, true],
    ['occupancy', HEARTBEAT, false],
    ['messages', 'not json', false],
    ['messages', 'null', false],
    ['events', '{"schema":"conch.unknown.v1"}', false],
  ])('a %s feed takes %s: %s', (kind, frame, valid) => {
    expect(validFrame(kind, frame)).toBe(valid);
  });

  it('sends a valid frame on, and drops and counts what is not one: not text, too large, not its feed’s, not JSON', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, messages);
    const on = r.sockets[0]?.on;
    on?.open();
    on?.frame(new ArrayBuffer(4));
    on?.frame(PRESENCE);
    on?.frame('{');
    on?.frame(`{"x":"${'a'.repeat(MAX_FRAME_BYTES)}"}`);
    expect(r.feeds.invalid).toBe(4);
    on?.frame(MESSAGE);
    expect(r.events).toEqual([
      [1, { id: 1, type: 'open' }],
      [1, { id: 1, type: 'frames', frames: [MESSAGE] }],
    ]);
    // The log says what kind of feed, never what the frame held.
    expect(r.logs.filter((l) => l.includes('dropped'))).toEqual(['conch-desktop: dropped an invalid messages frame; 1 so far']);
  });

  it('sends one batch at a time: what arrives while one is in flight waits for its acknowledgement', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    const on = r.sockets[0]?.on;
    on?.open();
    on?.frame(OCCUPANCY);
    on?.frame(OCCUPANCY);
    on?.frame(OCCUPANCY);
    expect(types(r)).toEqual(['open', 'frames 1']);
    expect(r.feeds.ack(1, 1)).toBe(true);
    expect(types(r)).toEqual(['open', 'frames 1', 'frames 2']);
    // An acknowledgement with nothing in flight, or for another number, does nothing.
    expect(r.feeds.ack(1, 1)).toBe(true);
    expect(r.feeds.ack(1, 1)).toBe(false);
    expect(r.feeds.ack(1, 9)).toBe(false);
    expect(r.feeds.ack(2, 1)).toBe(false);
  });

  it('closes a feed whose page falls behind by frames, tells it 1013 to resubscribe, and drops what follows', () => {
    const r = rig({ maxQueuedFrames: 3 });
    r.feeds.subscribe(1, 1, occupancy);
    const socket = r.sockets[0];
    socket?.on.open();
    for (let i = 0; i < 5; i++) socket?.on.frame(OCCUPANCY);
    // One in flight, three waiting, the fourth over the bound.
    expect(types(r)).toEqual(['open', 'frames 1', `close ${String(CLOSE_BEHIND)}`]);
    expect(socket?.closed).toBe(true);
    expect(r.feeds.open).toBe(0);
    expect(r.feeds.behind).toBe(1);
    socket?.on.frame(OCCUPANCY);
    socket?.on.close(1000, '');
    expect(r.events).toHaveLength(3);
    expect(r.feeds.ack(1, 1)).toBe(false);
  });

  it('closes a feed whose page falls behind by bytes', () => {
    const r = rig({ maxQueuedBytes: MESSAGE.length * 2 });
    r.feeds.subscribe(1, 1, messages);
    const socket = r.sockets[0];
    socket?.on.open();
    for (let i = 0; i < 4; i++) socket?.on.frame(MESSAGE);
    expect(types(r)).toEqual(['open', 'frames 1', `close ${String(CLOSE_BEHIND)}`]);
    expect(socket?.closed).toBe(true);
  });
});

describe('closes', () => {
  it('passes the server’s close on, with its code and reason, after the frames that came before it', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    const on = r.sockets[0]?.on;
    on?.open();
    on?.frame(OCCUPANCY);
    on?.frame(OCCUPANCY);
    on?.close(1008, 'no longer a member of this channel');
    expect(r.events.slice(1)).toEqual([
      [1, { id: 1, type: 'frames', frames: [OCCUPANCY] }],
      [1, { id: 1, type: 'frames', frames: [OCCUPANCY] }],
      [1, { id: 1, type: 'close', code: 1008, reason: 'no longer a member of this channel' }],
    ]);
    expect(r.feeds.open).toBe(0);
    on?.frame(OCCUPANCY);
    expect(r.events).toHaveLength(4);
  });

  it('asks whoami once when a socket fails before it opens, and says 1008 when the sign-in has ended', async () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.whoami = { kind: 'unauthorized' };
    r.sockets[0]?.on.close(1006, '');
    await flush();
    expect(r.sent.map((s) => s.op)).toEqual(['whoami']);
    expect(types(r)).toEqual([`close ${String(CLOSE_SIGNED_OUT)}`]);
  });

  it('reports a socket that failed before it opened as it closed when the sign-in is fine', async () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.sockets[0]?.on.close(1006, '');
    await flush();
    expect(types(r)).toEqual(['close 1006']);
  });

  it('closes on unsubscribe, says nothing more about it, and counts it closed', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.feeds.subscribe(1, 2, occupancy);
    expect(r.feeds.open).toBe(2);
    expect(r.feeds.unsubscribe(1, 1)).toBe(true);
    expect(r.sockets[0]?.closed).toBe(true);
    expect(r.feeds.open).toBe(1);
    r.sockets[0]?.on.open();
    r.sockets[0]?.on.frame(OCCUPANCY);
    r.sockets[0]?.on.close(1000, '');
    expect(r.events).toEqual([]);
    expect(r.feeds.unsubscribe(1, 1)).toBe(false);
    expect(r.logs).toContain('conch-desktop: feed occupancy closed (unsubscribed); 1 open, 1 closing');
  });

  it('closes every feed of a document that ended (a reload, a crash), and tells it nothing', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.feeds.subscribe(1, 2, messages);
    r.sockets[0]?.on.open();
    r.events.length = 0;
    r.feeds.endDocument(2);
    expect(r.sockets.every((s) => s.closed)).toBe(true);
    expect(r.feeds.open).toBe(0);
    r.sockets[0]?.on.frame(OCCUPANCY);
    expect(r.events).toEqual([]);
    // The new document starts with nothing, and its numbers are its own.
    expect(r.feeds.subscribe(2, 1, occupancy)).toBe(true);
    r.feeds.endDocument(2);
    expect(r.feeds.open).toBe(1);
  });
});

describe('sockets main has closed', () => {
  it('are held, and counted as closing, until their close arrives', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.sockets[0]?.on.open();
    r.feeds.unsubscribe(1, 1);
    expect([r.feeds.open, r.feeds.closing]).toEqual([0, 1]);
    r.sockets[0]?.on.close(1000, '');
    expect([r.feeds.open, r.feeds.closing]).toEqual([0, 0]);
    // The deadline is cleared, and the page is told nothing.
    expect(r.timers.map((t) => [t.ms, t.cleared])).toEqual([[CLOSE_DEADLINE_MS, true]]);
    expect(r.events).toEqual([[1, { id: 1, type: 'open' }]]);
    expect(r.logs.at(-1)).toBe('conch-desktop: feed occupancy socket closed; 0 open, 0 closing');
  });

  it('are destroyed when the server does not finish the close by the deadline', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.feeds.unsubscribe(1, 1);
    expect(r.sockets[0]?.closed).toBe(true);
    r.timers[0]?.run();
    expect(r.sockets[0]?.destroyed).toBe(true);
    expect(r.feeds.closing).toBe(0);
    expect(r.logs.at(-1)).toBe(`conch-desktop: feed occupancy socket destroyed: no close within ${String(CLOSE_DEADLINE_MS)} ms; 0 open, 0 closing`);
    // Its close event, when it comes, changes nothing.
    r.sockets[0]?.on.close(1006, '');
    expect(r.feeds.closing).toBe(0);
    expect(r.events).toEqual([]);
  });

  it('stay counted when there is nothing to destroy, until their close arrives', () => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    const socket = r.sockets[0];
    if (socket !== undefined) socket.destroyable = false;
    r.feeds.unsubscribe(1, 1);
    r.timers[0]?.run();
    expect(r.feeds.closing).toBe(1);
    expect(r.logs.at(-1)).toContain('cannot be destroyed; 0 open, 1 closing');
    socket?.on.close(1006, '');
    expect(r.feeds.closing).toBe(0);
  });

  it('count toward the cap: with MAX_SOCKETS held, open or closing, a subscription is refused and logged', () => {
    const r = rig();
    let n = 0;
    for (let document = 1; n < MAX_SOCKETS; document++) {
      for (let id = 1; id <= MAX_FEEDS && n < MAX_SOCKETS; id++, n++) expect(r.feeds.subscribe(document, id, occupancy)).toBe(true);
    }
    // Close half: they are still held while they close.
    r.feeds.endDocument(99);
    expect([r.feeds.open, r.feeds.closing]).toEqual([0, MAX_SOCKETS]);
    expect(r.feeds.subscribe(99, 1, occupancy)).toBe(false);
    expect(r.sockets).toHaveLength(MAX_SOCKETS);
    expect(r.logs.at(-1)).toBe(`conch-desktop: refused a occupancy feed: ${String(MAX_SOCKETS)} sockets held (0 open, ${String(MAX_SOCKETS)} closing), the most there may be`);
    // One finishing its close makes room for one.
    r.sockets[0]?.on.close(1000, '');
    expect(r.feeds.subscribe(99, 1, occupancy)).toBe(true);
    expect(r.feeds.subscribe(99, 2, occupancy)).toBe(false);
  });
});

describe('the identity', () => {
  it.each<[string, (s: Session) => void]>([
    ['sign-out', (s) => {
      s.forget();
    }],
    ['a new sign-in', (s) => {
      s.adopt('conch_another_identity_0123456789');
    }],
    ['another server', (s) => {
      s.setOrigin('https://other.example');
    }],
  ])('%s closes every socket before the next identity can make a request, and a delayed frame never reaches the page', (_name, change) => {
    const r = rig();
    r.feeds.subscribe(1, 1, occupancy);
    r.feeds.subscribe(1, 2, messages);
    for (const s of r.sockets) s.on.open();
    r.events.length = 0;
    change(r.session);
    // The identity's first request is made after the change returns.
    r.order.push('first request');
    expect(r.order).toEqual([`closed ${r.sockets[0]?.url ?? ''}`, `closed ${r.sockets[1]?.url ?? ''}`, 'first request']);
    expect(r.feeds.open).toBe(0);
    // The frame the server had already sent arrives late.
    for (const s of r.sockets) {
      s.on.frame(OCCUPANCY);
      s.on.frame(MESSAGE);
      s.on.close(1000, '');
    }
    expect(r.events.every(([, e]) => e.type === 'close' && e.code === CLOSE_SIGNED_OUT)).toBe(true);
    expect(r.events).toHaveLength(2);
  });
});

// One real socket, Node's own WebSocket against a loopback server: what goes
// on the wire.
describe('Node’s WebSocket, as main opens it', () => {
  let server: Server | null = null;
  const sockets: Socket[] = [];

  afterEach(async () => {
    for (const s of sockets) s.destroy();
    sockets.length = 0;
    await new Promise<void>((done) => {
      if (server === null) done();
      else
        server.close(() => {
          done();
        });
    });
    server = null;
  });

  function listen(onUpgrade: (url: string, headers: IncomingHttpHeaders, socket: Socket) => void): Promise<string> {
    server = createServer((_req, res) => {
      res.writeHead(404).end();
    });
    server.on('upgrade', (req, socket: Socket) => {
      sockets.push(socket);
      onUpgrade(req.url ?? '', req.headers, socket);
    });
    return new Promise((done) => {
      server?.listen(0, '127.0.0.1', () => {
        done(`http://127.0.0.1:${String((server?.address() as AddressInfo).port)}`);
      });
    });
  }

  function accept(headers: IncomingHttpHeaders, socket: Socket): void {
    const key = String(headers['sec-websocket-key']);
    const answer = createHash('sha1').update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest('base64');
    socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${answer}\r\n\r\n`);
  }

  function textFrame(text: string): Buffer {
    const body = Buffer.from(text, 'utf8');
    if (body.length >= 126) throw new Error('short frames only');
    return Buffer.concat([Buffer.from([0x81, body.length]), body]);
  }

  it('sends the bearer as the Authorization header, and no Origin, Cookie or ticket', async () => {
    const seen: { url: string; headers: IncomingHttpHeaders }[] = [];
    const origin = await listen((url, headers, socket) => {
      seen.push({ url, headers });
      accept(headers, socket);
      socket.write(textFrame(HEARTBEAT));
    });
    const got: string[] = [];
    await new Promise<void>((done) => {
      const handle = createNodeConnect(() => undefined)(feedUrl(origin, messages), { Authorization: `Bearer ${TOKEN}` }, {
        open: () => got.push('open'),
        frame: (data: unknown) => {
          got.push(String(data));
          handle.close();
          done();
        },
        close: () => undefined,
      });
    });
    expect(got).toEqual(['open', HEARTBEAT]);
    expect(seen).toHaveLength(1);
    expect(seen[0]?.url).toBe('/v2/ws?channel=general&after=4&heartbeat=1');
    expect(seen[0]?.headers['authorization']).toBe(`Bearer ${TOKEN}`);
    expect(seen[0]?.headers['origin']).toBeUndefined();
    expect(seen[0]?.headers['cookie']).toBeUndefined();
    expect(seen[0]?.url).not.toContain(TOKEN);
  });

  it('does not follow a redirect of the upgrade, so the credential never goes where it points', async () => {
    let elsewhere = 0;
    const target = createServer((_req, res) => {
      elsewhere++;
      res.writeHead(404).end();
    });
    target.on('upgrade', (_req, socket: Socket) => {
      elsewhere++;
      socket.destroy();
    });
    await new Promise<void>((done) => {
      target.listen(0, '127.0.0.1', () => {
        done();
      });
    });
    const away = `http://127.0.0.1:${String((target.address() as AddressInfo).port)}/v2/ws`;
    const origin = await listen((_url, _headers, socket) => {
      socket.end(`HTTP/1.1 302 Found\r\nLocation: ${away}\r\nContent-Length: 0\r\n\r\n`);
    });
    const code = await new Promise<number>((done) => {
      createNodeConnect(() => undefined)(feedUrl(origin, occupancy), { Authorization: `Bearer ${TOKEN}` }, {
        open: () => undefined,
        frame: () => undefined,
        close: (c: number) => {
          done(c);
        },
      });
    });
    await new Promise<void>((done) => {
      target.close(() => {
        done();
      });
    });
    expect(code).toBe(1006);
    expect(elsewhere).toBe(0);
  });

  it('is opened through undici’s own Agent, which this Node has', () => {
    expect(agentConstructor()?.name).toBe('Agent');
  });

  it('refuses a frame larger than the cap at its header, without waiting for its payload', async () => {
    let serverSaw = '';
    const origin = await listen((_url, headers, socket) => {
      accept(headers, socket);
      // The server's half stays open (allowHalfOpen); its end is the client
      // going away.
      socket.resume();
      socket.on('end', () => {
        serverSaw += 'closed';
      });
      // A text frame that says it is 2 MiB long; its payload never comes.
      const header = Buffer.alloc(10);
      header[0] = 0x81;
      header[1] = 127;
      header.writeBigUInt64BE(BigInt(2 * MAX_FRAME_BYTES), 2);
      socket.write(header);
    });
    const got: string[] = [];
    const started = Date.now();
    const code = await new Promise<number>((done) => {
      createNodeConnect(() => undefined)(feedUrl(origin, occupancy), { Authorization: `Bearer ${TOKEN}` }, {
        open: () => got.push('open'),
        frame: () => got.push('frame'),
        close: (c: number) => {
          done(c);
        },
      });
    });
    expect(got).toEqual(['open']);
    expect(code).toBe(1006);
    expect(Date.now() - started).toBeLessThan(2_000);
    await expect.poll(() => serverSaw).toBe('closed');
  });

  it('destroys a socket whose server ignores the close frame, within the deadline, and frees its place', async () => {
    const ends: string[] = [];
    const origin = await listen((_url, headers, socket) => {
      accept(headers, socket);
      // Never answers the close frame, and never closes.
      socket.resume();
      socket.on('end', () => ends.push('server saw the socket end'));
    });
    const session = new Session();
    session.setOrigin(origin);
    session.adopt(TOKEN);
    const logs: string[] = [];
    let deadlines = 0;
    const feeds = new Feeds({
      session,
      client: { send: () => Promise.resolve({ kind: 'answer', status: 200, ok: true }) },
      connect: createNodeConnect(() => undefined),
      deliver: () => undefined,
      log: (line) => logs.push(line),
      // The deadline, shortened for the test.
      setTimer: (run, ms) => {
        expect(ms).toBe(CLOSE_DEADLINE_MS);
        deadlines++;
        return setTimeout(run, 200);
      },
      clearTimer: (timer) => {
        clearTimeout(timer as ReturnType<typeof setTimeout>);
      },
    });
    expect(feeds.subscribe(1, 1, occupancy)).toBe(true);
    await expect.poll(() => feeds.open).toBe(1);
    await expect.poll(() => sockets.length).toBe(1);
    // Let the socket open before closing it.
    await new Promise((r) => setTimeout(r, 100));
    feeds.unsubscribe(1, 1);
    expect([feeds.open, feeds.closing]).toEqual([0, 1]);
    await expect.poll(() => ends, { timeout: 2_000 }).toEqual(['server saw the socket end']);
    expect(feeds.closing).toBe(0);
    expect(deadlines).toBe(1);
    expect(logs.some((l) => l.includes('socket destroyed: no close within'))).toBe(true);
  });

  // The connect hook makes the TLS connection with Node's defaults: the
  // process-wide roots and the hostname check, as a fetch has them. Trust is
  // process-wide, so each case is a fresh process (./socket-child.ts), as the
  // private-CA tests of trust.test.ts are.
  describe('over TLS', () => {
    const child = resolve(import.meta.dirname, 'socket-child.ts');

    async function overTls(env: (pki: Pki) => Record<string, string>): Promise<{ said: string; upgrades: string[] }> {
      const pki = makePki();
      const upgrades: string[] = [];
      const https = createHttpsServer({ key: pki.keyPem, cert: pki.certPem }, (_req, res) => {
        res.writeHead(200).end('{}');
      });
      https.on('upgrade', (req, socket: Socket) => {
        sockets.push(socket);
        upgrades.push(req.url ?? '');
        accept(req.headers, socket);
        socket.write(textFrame(HEARTBEAT));
      });
      await new Promise<void>((done) => {
        https.listen(0, '127.0.0.1', () => {
          done();
        });
      });
      const origin = `https://localhost:${String((https.address() as AddressInfo).port)}`;
      try {
        const said = await new Promise<string>((done) => {
          execFile(process.execPath, [child, origin], { env: { PATH: process.env['PATH'] ?? '', ...env(pki) }, encoding: 'utf8', timeout: 30_000 }, (_error, stdout) => {
            done(stdout.trim());
          });
        });
        return { said, upgrades };
      } finally {
        // An upgraded socket is the test's, not the server's: close() would
        // wait for it.
        for (const socket of sockets) socket.destroy();
        https.closeAllConnections();
        await new Promise<void>((done) => {
          https.close(() => {
            done();
          });
        });
        pki.cleanup();
      }
    }

    it('refuses a certificate this machine does not trust, before any request', async () => {
      expect(await overTls(() => ({}))).toEqual({ said: 'agent close 1006', upgrades: [] });
    });

    it('trusts a private CA installed the documented way, and sends the upgrade there', async () => {
      expect(await overTls((pki) => ({ NODE_EXTRA_CA_CERTS: pki.caPath }))).toEqual({ said: `agent frame ${HEARTBEAT}`, upgrades: ['/v2/ws'] });
    });
  });
});
