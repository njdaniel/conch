import { describe, expect, it } from 'vitest';

import type { Feeds } from '../../main/api/feeds.ts';
import type { Files } from '../../main/files.ts';
import type { Service } from '../../main/api/service.ts';
import { DocumentGate } from '../../main/document.ts';
import { checkCaller, Ipc, parseRequest, plain, type ContentsLike, type FrameLike, type InvokeEvent } from '../../main/ipc.ts';
import { INVOKED_FUNCTIONS } from '../../shared/bridge-names.ts';

function frame(token = 'frame-1', over: Partial<FrameLike> = {}): FrameLike {
  return { frameToken: token, parent: null, detached: false, url: 'conch-app://ui/', ...over };
}

interface Fixture {
  readonly ipc: Ipc;
  readonly gate: DocumentGate;
  readonly contents: ContentsLike & { destroyed: boolean };
  readonly main: FrameLike;
  readonly calls: [string, unknown][];
  readonly logs: string[];
  readonly clock: { now: number };
  readonly input: { recent: boolean };
  event(over?: Partial<InvokeEvent>): InvokeEvent;
}

function fixture(): Fixture {
  const main = frame();
  const contents = { destroyed: false, mainFrame: main, isDestroyed() { return this.destroyed; } };
  const gate = new DocumentGate();
  gate.begin();
  gate.commit(main);
  const calls: [string, unknown][] = [];
  const record = (name: string) => (...args: unknown[]) => {
    calls.push([name, args]);
    return Promise.resolve({ ran: name });
  };
  const service = new Proxy({} as Service, { get: (_t, name: string) => record(name) });
  const feeds = new Proxy({} as Feeds, { get: (_t, name: string) => record(`feeds.${name}`) });
  const logs: string[] = [];
  const clock = { now: 1_000_000 };
  const input = { recent: false };
  const files = new Proxy({} as Files, { get: (_t, name: string) => record(`files.${name}`) });
  const gestures = { take: () => (input.recent ? ('pointer' as const) : null) };
  const ipc = new Ipc({ owned: () => contents, gate, service, feeds, files, gestures, now: () => clock.now, log: (line) => logs.push(line) });
  return { ipc, gate, contents, main, calls, logs, clock, input, event: (over = {}) => ({ sender: contents, senderFrame: main, ...over }) };
}

describe('every invocation is checked before anything else', () => {
  it('runs a call from the owned contents’ live main frame at the app’s own origin', async () => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event(), 'serverAddress', [])).toEqual({ ok: true, value: { ran: 'serverAddress' } });
    expect(f.ipc.refusals).toBe(0);
  });

  const cases: [string, (f: Fixture) => InvokeEvent, string][] = [
    ['another web contents', (f) => f.event({ sender: { isDestroyed: () => false, mainFrame: f.main } }), 'sender'],
    ['a subframe', (f) => f.event({ senderFrame: frame('frame-2', { parent: f.main }) }), 'subframe'],
    ['a frame that is not the main frame', (f) => f.event({ senderFrame: frame('other') }), 'frame'],
    ['no frame', (f) => f.event({ senderFrame: null }), 'frame'],
    ['a detached frame', (f) => f.event({ senderFrame: frame('frame-1', { detached: true }) }), 'frame'],
    ['a foreign origin', (f) => f.event({ senderFrame: frame('frame-1', { url: 'https://evil.example/' }) }), 'origin'],
    ['a lookalike origin', (f) => f.event({ senderFrame: frame('frame-1', { url: 'conch-app://ui.evil/' }) }), 'origin'],
    ['a destroyed sender', (f) => { f.contents.destroyed = true; return f.event(); }, 'destroyed'],
    ['a stale document generation', (f) => { f.gate.begin(); return f.event(); }, 'generation'],
  ];

  it.each(cases)('refuses %s, counts it, and executes nothing', async (_name, make, reason) => {
    const f = fixture();
    const event = make(f);
    expect(await f.ipc.invoke(event, 'signOut', [])).toEqual({ ok: false, kind: 'refused' });
    expect(f.ipc.refusals).toBe(1);
    expect(f.calls).toEqual([]);
    expect(f.logs).toEqual([`conch-desktop: refused an IPC call (${reason}); 1 so far`]);
  });

  it('refuses everything when the window is gone', async () => {
    const f = fixture();
    const ipc = new Ipc({ owned: () => null, gate: f.gate, service: {} as Service, feeds: {} as Feeds, files: {} as Files, gestures: { take: () => null }, now: () => 0, log: () => undefined });
    expect(await ipc.invoke(f.event(), 'serverAddress', [])).toEqual({ ok: false, kind: 'refused' });
  });

  it('accepts the new document only after it commits', async () => {
    const f = fixture();
    f.gate.begin();
    expect((await f.ipc.invoke(f.event(), 'serverAddress', [])).ok).toBe(false);
    f.gate.commit(f.main);
    expect((await f.ipc.invoke(f.event(), 'serverAddress', [])).ok).toBe(true);
  });

  it('checks the caller before the name, the arguments and the rate', async () => {
    const f = fixture();
    const event = f.event({ senderFrame: null });
    await f.ipc.invoke(event, 'no-such-function', [() => 1]);
    expect(f.logs[0]).toContain('(frame)');
  });
});

describe('arguments', () => {
  it('refuses a name that is not a bridge function', async () => {
    const f = fixture();
    for (const name of ['constructor', '__proto__', 'subscribe', 'login', 5, undefined]) {
      expect(await f.ipc.invoke(f.event(), name, [])).toEqual({ ok: false, kind: 'refused' });
    }
    expect(f.calls).toEqual([]);
  });

  it.each([
    ['serverAddress', [1]],
    ['signOut', ['x']],
    ['setServerAddress', []],
    ['setServerAddress', [5]],
    ['setServerAddress', ['a', 'b']],
    ['signInWithCredential', ['has space']],
    ['signInWithCredential', ['']],
    ['signInWithCredential', ['x'.repeat(5000)]],
    ['keepCredential', ['has space']],
    ['keepCredential', ['']],
    ['keepCredential', [7]],
    ['signInWithPassword', [{ name: 'a' }]],
    ['signInWithPassword', [{ name: 'a', password: 1 }]],
    ['signInWithPassword', ['a']],
    ['signInWithPassword', [null]],
    ['signInWithPassword', [[]]],
    ['signInWithCode', [{ challenge: 'c' }]],
    ['redeemInvite', [{ code: 'c' }]],
    ['request', []],
    ['request', [null]],
    ['request', [{}]],
    ['request', [{ operation: 'nope' }]],
    ['request', [{ operation: 'toString' }]],
    ['request', [{ operation: 'whoami', extra: 1 }]],
    ['request', [{ operation: 'whoami', body: {} }]],
    ['request', [{ operation: 'whoami', params: { x: 'y' } }]],
    ['request', [{ operation: 'listMessages' }]],
    ['request', [{ operation: 'listMessages', params: { channel: 'a/b' } }]],
    ['request', [{ operation: 'listMessages', params: { channel: '..' } }]],
    ['request', [{ operation: 'listMessages', params: { channel: 'a%2fb' } }]],
    ['request', [{ operation: 'listMessages', params: { channel: 'a' }, query: 5 }]],
    ['request', [{ operation: 'listMessages', params: { channel: 'a' }, query: '?x=1' }]],
    ['request', [{ operation: 'postMessage', params: { channel: 'a' } }]],
    ['request', [{ operation: 'postMessage', params: { channel: 'a' }, body: { wrong: true } }]],
    ['request', [{ operation: 'whoami', keepalive: 'yes' }]],
    ['request', [{ operation: 'whoami' }, 'extra']],
    ['request', [{ operation: 'whoami', toString: () => 'x' }]],
    ['feedSubscribe', [1]],
    ['feedSubscribe', [{ kind: 'occupancy' }]],
    ['feedSubscribe', [0, { kind: 'occupancy' }]],
    ['feedSubscribe', [-1, { kind: 'occupancy' }]],
    ['feedSubscribe', [1.5, { kind: 'occupancy' }]],
    ['feedSubscribe', ['1', { kind: 'occupancy' }]],
    ['feedSubscribe', [1, { kind: 'occupancy' }, 'extra']],
    ['feedSubscribe', [1, { kind: 'tickets' }]],
    ['feedSubscribe', [1, { kind: 'occupancy', channel: 'general' }]],
    ['feedSubscribe', [1, { kind: 'messages', channel: 'general' }]],
    ['feedSubscribe', [1, { kind: 'messages', channel: 'general', after: -1 }]],
    ['feedSubscribe', [1, { kind: 'messages', channel: 'general', after: 0, url: 'wss://evil.example/' }]],
    ['feedSubscribe', [1, { kind: 'events', channel: 'general' }]],
    ['feedSubscribe', [1, { kind: 'events', channel: 'general', session: 0 }]],
    ['feedSubscribe', [1, { kind: 'voice', channel: '' }]],
    ['feedSubscribe', [1, { kind: 'voice', channel: 'a\nb' }]],
    ['feedSubscribe', [1, { kind: 'voice', channel: 'x'.repeat(300) }]],
    ['feedAck', []],
    ['feedAck', ['1']],
    ['feedUnsubscribe', [0]],
    ['feedUnsubscribe', [1, 2]],
  ])('refuses a bad shape: %s %j', async (name, args) => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event(), name, args)).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it('refuses every operation the renderer may not invoke, and a socket, and logout', async () => {
    const f = fixture();
    for (const operation of ['login', 'loginTotp', 'redeemInvite', 'resetPassword', 'setupRedeem', 'wsTicket', 'voiceSession', 'createInvite', 'totpEnroll', 'reauth', 'changePassword', 'messagesSocket', 'eventsSocket', 'logout']) {
      expect(await f.ipc.invoke(f.event(), 'request', [{ operation, params: operation === 'voiceSession' ? { channel: 'a' } : undefined }]), operation).toEqual({ ok: false, kind: 'refused' });
    }
    expect(f.calls).toEqual([]);
    expect(f.ipc.refusals).toBe(14);
  });

  it('passes a good request to the service, and the sign-in family to signIn with the operation fixed by the function', async () => {
    const f = fixture();
    await f.ipc.invoke(f.event(), 'request', [{ operation: 'listMessages', params: { channel: 'general' }, query: 'limit=5' }]);
    await f.ipc.invoke(f.event(), 'signInWithPassword', [{ name: 'ada', password: 'pw' }]);
    await f.ipc.invoke(f.event(), 'createAccount', [{ name: 'ada', password: 'pw' }]);
    expect(f.calls).toEqual([
      ['request', [{ operation: 'listMessages', params: { channel: 'general' }, query: 'limit=5' }]],
      ['signIn', ['login', { name: 'ada', password: 'pw' }]],
      ['signIn', ['joinAccount', { name: 'ada', password: 'pw' }]],
    ]);
  });

  it('passes a feed call to the feeds with the caller’s document generation, and the parameters it parsed', async () => {
    const f = fixture();
    const document = f.gate.generation;
    await f.ipc.invoke(f.event(), 'feedSubscribe', [3, { kind: 'messages', channel: 'general', after: 41 }]);
    await f.ipc.invoke(f.event(), 'feedSubscribe', [4, { kind: 'events', channel: 'general', session: 9 }]);
    await f.ipc.invoke(f.event(), 'feedSubscribe', [5, { kind: 'occupancy' }]);
    await f.ipc.invoke(f.event(), 'feedAck', [3]);
    await f.ipc.invoke(f.event(), 'feedUnsubscribe', [3]);
    expect(f.calls).toEqual([
      ['feeds.subscribe', [document, 3, { kind: 'messages', channel: 'general', after: 41 }]],
      ['feeds.subscribe', [document, 4, { kind: 'events', channel: 'general', session: 9 }]],
      ['feeds.subscribe', [document, 5, { kind: 'occupancy' }]],
      ['feeds.ack', [document, 3]],
      ['feeds.unsubscribe', [document, 3]],
    ]);
  });

  it('checks a feed call’s caller like any other: a stale document subscribes nothing', async () => {
    const f = fixture();
    f.gate.begin();
    expect(await f.ipc.invoke(f.event(), 'feedSubscribe', [1, { kind: 'occupancy' }])).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it('refuses arguments that are not plain data: functions, class instances, cycles-by-depth, huge arrays', async () => {
    const f = fixture();
    const deep: Record<string, unknown> = {};
    let cursor = deep;
    for (let i = 0; i < 40; i++) {
      const next: Record<string, unknown> = {};
      cursor['n'] = next;
      cursor = next;
    }
    for (const arg of [() => 1, new Map(), new Date(), new Uint8Array(2), Symbol('s'), 10n, Number.NaN, deep, new Array<number>(20_000).fill(1)]) {
      expect(await f.ipc.invoke(f.event(), 'setServerAddress', [arg]), typeof arg).toEqual({ ok: false, kind: 'refused' });
    }
    expect(plain({ a: [1, 'b', null, true] })).toBe(true);
    expect(plain(Object.create(null))).toBe(true);
    expect(plain(new (class X { value = 1; })())).toBe(false);
  });

  it('refuses an argument over its byte cap', async () => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event(), 'setServerAddress', ['a'.repeat(5000)])).toEqual({ ok: false, kind: 'refused' });
    expect(f.logs.at(-1)).toContain('(size)');
    expect(await f.ipc.invoke(f.event(), 'signInWithPassword', [{ name: 'a', password: 'p'.repeat(20_000) }])).toEqual({ ok: false, kind: 'refused' });
  });

  it('limits each function’s rate per minute, and starts again in the next', async () => {
    const f = fixture();
    let accepted = 0;
    for (let i = 0; i < 40; i++) if ((await f.ipc.invoke(f.event(), 'signInWithPassword', [{ name: 'a', password: 'b' }])).ok) accepted++;
    expect(accepted).toBe(30);
    expect(f.logs.at(-1)).toContain('(rate)');
    // Another function has its own budget.
    expect((await f.ipc.invoke(f.event(), 'serverAddress', [])).ok).toBe(true);
    f.clock.now += 60_001;
    expect((await f.ipc.invoke(f.event(), 'signInWithPassword', [{ name: 'a', password: 'b' }])).ok).toBe(true);
  });

  it('says only "refused" when a function throws, and logs no argument', async () => {
    const f = fixture();
    const ipc = new Ipc({
      owned: () => f.contents,
      gate: f.gate,
      service: { setServerAddress: () => { throw new Error('boom secret-arg'); } } as unknown as Service,
      feeds: {} as Feeds,
      files: {} as Files,
      gestures: { take: () => null },
      now: () => 0,
      log: (line) => f.logs.push(line),
    });
    expect(await ipc.invoke(f.event(), 'setServerAddress', ['secret-arg'])).toEqual({ ok: false, kind: 'refused' });
    expect(f.logs.join('\n')).not.toContain('secret-arg');
  });
});

describe('the registry', () => {
  it('registers one handler per invoked function and no other channel', () => {
    const f = fixture();
    const channels: string[] = [];
    f.ipc.register({ handle: (channel) => void channels.push(channel) });
    expect(channels.sort()).toEqual(INVOKED_FUNCTIONS.map((n) => `conch:${n}`).sort());
    expect(channels).toHaveLength(22);
  });

  it('parses a request the way the table says', () => {
    expect(parseRequest({ operation: 'whoami' })).toEqual({ operation: 'whoami' });
    expect(parseRequest({ operation: 'login', body: {} })).toBeNull();
  });
});

describe('the caller check alone', () => {
  it('is null for the one document', () => {
    const f = fixture();
    expect(checkCaller(f.event(), f.contents, f.gate)).toBeNull();
  });
});
