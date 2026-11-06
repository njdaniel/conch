import { describe, expect, it } from 'vitest';

import type { Feeds } from '../../main/api/feeds.ts';
import type { Files } from '../../main/files.ts';
import type { Service } from '../../main/api/service.ts';
import { DocumentGate } from '../../main/document.ts';
import { checkCaller, Ipc, parseRequest, plain, type ProfilesApi, type VoiceApi, type ContentsLike, type FrameLike, type InvokeEvent } from '../../main/ipc.ts';
import { INVOKED_FUNCTIONS, VOICE_EVENTS, VOICE_FUNCTIONS } from '../../shared/bridge-names.ts';
import { deliverVoice } from '../../main/voice/events.ts';
import type { NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';

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
  // Whether main's compatibility verdict lets a sign-in happen (issue #946).
  readonly permit: { value: boolean };
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
  const permit = { value: true };
  const profiles = new Proxy({} as ProfilesApi, { get: (_t, name: string) => (name === 'signInPermitted' ? () => permit.value : record(`profiles.${name}`)) });
  const logs: string[] = [];
  const clock = { now: 1_000_000 };
  const input = { recent: false };
  const files = new Proxy({} as Files, { get: (_t, name: string) => record(`files.${name}`) });
  const voice = new Proxy({} as VoiceApi, { get: (_t, name: string) => record(`voice.${name}`) });
  const gestures = { take: () => (input.recent ? ('pointer' as const) : null) };
  const ipc = new Ipc({ owned: () => contents, gate, service, feeds, files, gestures, profiles, voice, now: () => clock.now, log: (line) => logs.push(line) });
  return { ipc, gate, contents, main, calls, logs, clock, permit, input, event: (over = {}) => ({ sender: contents, senderFrame: main, ...over }) };
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
    const ipc = new Ipc({
      owned: () => null,
      gate: f.gate,
      service: {} as Service,
      feeds: {} as Feeds,
      files: {} as Files,
      gestures: { take: () => null },
      profiles: {} as ProfilesApi,
      voice: {} as VoiceApi,
      now: () => 0,
      log: () => undefined,
    });
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
    ['profileAdd', []],
    ['profileAdd', [5]],
    ['profileAdd', ['a', 'b']],
    ['signInWithCredential', ['has space']],
    ['signInWithCredential', ['']],
    ['signInWithCredential', ['x'.repeat(5000)]],
    ['keepCredential', ['has space']],
    ['keepCredential', ['conch_pasted_credential']],
    ['keepCredential', ['paste-xyz']],
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
    for (const operation of ['login', 'loginTotp', 'redeemInvite', 'resetPassword', 'setupRedeem', 'wsTicket', 'voiceSession', 'createMemberLink', 'totpEnroll', 'reauth', 'changePassword', 'messagesSocket', 'eventsSocket', 'logout']) {
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

  it.each([
    ['signInWithPassword', [{ name: 'ada', password: 'pw' }]],
    ['signInWithCode', [{ challenge: 'c', code: '123456' }]],
    ['redeemInvite', [{ code: 'abc', name: 'ada', password: 'pw' }]],
    ['joinAsGuest', [{ label: 'ada' }]],
    ['createAccount', [{ name: 'ada', password: 'pw' }]],
    ['signInWithCredential', ['conch_pasted']],
    ['keepCredential', ['paste-00000000000000000000000000000000']],
  ] as const)('does not carry %s when the server is not one the app may sign in to (issue #946)', async (name, args) => {
    const f = fixture();
    f.permit.value = false;
    const outcome = await f.ipc.invoke(f.event(), name, args);
    if (name === 'keepCredential') expect(outcome).toEqual({ ok: true, value: false });
    else expect(outcome).toEqual({ ok: true, value: { ok: false, kind: 'network', reason: 'incompatible' } });
    expect(f.calls).toEqual([]);
  });

  it.each([
    ['an invite preview', { operation: 'previewInvite', body: { code: 'secret-invite-code' } }],
    ['a sign-in link preview', { operation: 'previewSignInLink', body: { code: 'secret-link-code' } }],
    ['a channel list', { operation: 'listChannels' }],
  ] as const)('sends no request but the health check when the server is not one the app may sign in to: %s (issue #946)', async (_name, request) => {
    const f = fixture();
    f.permit.value = false;
    expect(await f.ipc.invoke(f.event(), 'request', [request])).toEqual({ ok: true, value: { kind: 'none', reason: 'incompatible' } });
    expect(f.calls).toEqual([]);
    expect(await f.ipc.invoke(f.event(), 'request', [{ operation: 'health' }])).toEqual({ ok: true, value: { ran: 'request' } });
    expect(f.calls).toEqual([['request', [{ operation: 'health' }]]]);
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
      expect(await f.ipc.invoke(f.event(), 'profileAdd', [arg]), typeof arg).toEqual({ ok: false, kind: 'refused' });
    }
    expect(plain({ a: [1, 'b', null, true] })).toBe(true);
    expect(plain(Object.create(null))).toBe(true);
    expect(plain(new (class X { value = 1; })())).toBe(false);
  });

  it('refuses an argument over its byte cap', async () => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event(), 'profileAdd', ['a'.repeat(5000)])).toEqual({ ok: false, kind: 'refused' });
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
      service: {} as Service,
      feeds: {} as Feeds,
      files: {} as Files,
      gestures: { take: () => null },
      profiles: { add: () => { throw new Error('boom secret-arg'); } } as unknown as ProfilesApi,
      voice: {} as VoiceApi,
      now: () => 0,
      log: (line) => f.logs.push(line),
    });
    expect(await ipc.invoke(f.event(), 'profileAdd', ['secret-arg'])).toEqual({ ok: false, kind: 'refused' });
    expect(f.logs.join('\n')).not.toContain('secret-arg');
  });
});

describe('the registry', () => {
  it('registers one handler per invoked function and no other channel', () => {
    const f = fixture();
    const channels: string[] = [];
    f.ipc.register({ handle: (channel) => void channels.push(channel) });
    expect(channels.sort()).toEqual(INVOKED_FUNCTIONS.map((n) => `conch:${n}`).sort());
    expect(channels).toHaveLength(INVOKED_FUNCTIONS.length);
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

describe('the voice family (issue #1374)', () => {
  const join = { channel: 'general', muted: false, deafened: false };

  it('has no function whose name says press, release, transmit, gate or ptt', () => {
    for (const name of [...VOICE_FUNCTIONS, ...INVOKED_FUNCTIONS]) expect(name).not.toMatch(/press|release|transmit|gate|ptt/i);
  });

  it('passes the parsed arguments to the service and gives its answer back', async () => {
    const f = fixture();
    f.input.recent = true;
    await f.ipc.invoke(f.event(), 'voiceState', []);
    await f.ipc.invoke(f.event(), 'voiceJoin', [join]);
    await f.ipc.invoke(f.event(), 'voiceLeave', []);
    await f.ipc.invoke(f.event(), 'voiceRetry', []);
    await f.ipc.invoke(f.event(), 'voiceSetMute', [true]);
    await f.ipc.invoke(f.event(), 'voiceSetDeafen', [false]);
    await f.ipc.invoke(f.event(), 'voiceSetDevices', [{ output: 'Speakers' }]);
    await f.ipc.invoke(f.event(), 'voiceSetDevices', [{ input: 'Mic', output: 'Speakers' }]);
    await f.ipc.invoke(f.event(), 'voiceSetTalkMode', ['open']);
    await f.ipc.invoke(f.event(), 'voiceSetTalkMode', ['push_to_talk']);
    expect(f.calls).toEqual([
      ['voice.state', []],
      ['voice.join', [join]],
      ['voice.leave', []],
      ['voice.retry', []],
      ['voice.setMute', [true]],
      ['voice.setDeafen', [false]],
      ['voice.setDevices', [{ output: 'Speakers' }]],
      ['voice.setDevices', [{ input: 'Mic', output: 'Speakers' }]],
      ['voice.setTalkMode', ['open']],
      ['voice.setTalkMode', ['push_to_talk']],
    ]);
    expect(f.ipc.refusals).toBe(0);
  });

  it.each([
    ['voiceState', [1]],
    ['voiceLeave', ['x']],
    ['voiceRetry', [{}]],
    ['voiceJoin', []],
    ['voiceJoin', ['general']],
    ['voiceJoin', [null]],
    ['voiceJoin', [[]]],
    ['voiceJoin', [{ channel: 'general', muted: false }]],
    ['voiceJoin', [{ muted: false, deafened: false }]],
    ['voiceJoin', [{ ...join, extra: 1 }]],
    ['voiceJoin', [{ ...join, token: 't' }]],
    ['voiceJoin', [{ ...join, helperPath: '/bin/sh' }]],
    ['voiceJoin', [{ ...join, channel: '' }]],
    ['voiceJoin', [{ ...join, channel: 5 }]],
    ['voiceJoin', [{ ...join, channel: 'a\nb' }]],
    ['voiceJoin', [{ ...join, channel: 'a\u0000b' }]],
    ['voiceJoin', [{ ...join, channel: 'x'.repeat(401) }]],
    ['voiceJoin', [{ ...join, channel: '\u00e9'.repeat(201) }]],
    ['voiceJoin', [{ ...join, muted: 'no' }]],
    ['voiceJoin', [{ ...join, deafened: 0 }]],
    ['voiceJoin', [join, 'extra']],
    ['voiceSetMute', []],
    ['voiceSetMute', ['true']],
    ['voiceSetMute', [1]],
    ['voiceSetMute', [{ muted: true }]],
    ['voiceSetMute', [true, false]],
    ['voiceSetDeafen', [null]],
    ['voiceSetDeafen', ['false']],
    ['voiceSetDevices', []],
    ['voiceSetDevices', [{}]],
    ['voiceSetDevices', ['Mic']],
    ['voiceSetDevices', [{ input: '' }]],
    ['voiceSetDevices', [{ input: 5 }]],
    ['voiceSetDevices', [{ input: 'a\nb' }]],
    ['voiceSetDevices', [{ input: 'x'.repeat(256) }]],
    ['voiceSetDevices', [{ output: 'x'.repeat(256) }]],
    ['voiceSetDevices', [{ input: 'Mic', path: '/dev/snd' }]],
    ['voiceSetDevices', [{ other: 'Mic' }]],
    ['voiceSetDevices', [{ input: 'Mic' }, 'extra']],
    ['voiceSetTalkMode', []],
    ['voiceSetTalkMode', ['hold']],
    ['voiceSetTalkMode', ['press']],
    ['voiceSetTalkMode', ['OPEN']],
    ['voiceSetTalkMode', [{ mode: 'open' }]],
    ['voiceSetTalkMode', ['open', 'extra']],
    ['voiceSetTalkMode', ['toString']],
  ])('refuses a bad argument before the service is called: %s %j', async (name, args) => {
    const f = fixture();
    f.input.recent = true;
    expect(await f.ipc.invoke(f.event(), name, args)).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it('accepts a channel of exactly the byte cap and refuses one byte over', async () => {
    const f = fixture();
    f.input.recent = true;
    expect((await f.ipc.invoke(f.event(), 'voiceJoin', [{ ...join, channel: 'x'.repeat(400) }])).ok).toBe(true);
    expect((await f.ipc.invoke(f.event(), 'voiceJoin', [{ ...join, channel: 'x'.repeat(401) }])).ok).toBe(false);
    // Two bytes a character: 200 fit, 201 do not.
    expect((await f.ipc.invoke(f.event(), 'voiceJoin', [{ ...join, channel: '\u00e9'.repeat(200) }])).ok).toBe(true);
    expect((await f.ipc.invoke(f.event(), 'voiceJoin', [{ ...join, channel: '\u00e9'.repeat(201) }])).ok).toBe(false);
  });

  it('refuses an oversized or non-plain argument before it is parsed', async () => {
    const f = fixture();
    f.input.recent = true;
    expect(await f.ipc.invoke(f.event(), 'voiceJoin', [{ ...join, channel: 'x'.repeat(5000) }])).toEqual({ ok: false, kind: 'refused' });
    expect(f.logs.at(-1)).toContain('(size)');
    expect(await f.ipc.invoke(f.event(), 'voiceSetMute', [new Boolean(true)])).toEqual({ ok: false, kind: 'refused' });
    expect(await f.ipc.invoke(f.event(), 'voiceJoin', [Object.assign(Object.create({ inherited: 1 }) as object, join)])).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it.each([
    ['voiceJoin', [{ channel: 'general', muted: false, deafened: false }]],
    ['voiceRetry', []],
    ['voiceSetTalkMode', ['open']],
  ] as const)('refuses %s without a recent gesture and executes nothing', async (name, args) => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event(), name, args)).toEqual({ ok: false, kind: 'refused' });
    expect(f.logs.at(-1)).toContain('(gesture)');
    expect(f.calls).toEqual([]);
    f.input.recent = true;
    expect((await f.ipc.invoke(f.event(), name, args)).ok).toBe(true);
    expect(f.calls).toHaveLength(1);
  });

  it('needs no gesture to leave, mute, deafen, set devices or choose push-to-talk, nor to read the state', async () => {
    const f = fixture();
    for (const [name, args] of [
      ['voiceState', []],
      ['voiceLeave', []],
      ['voiceSetMute', [true]],
      ['voiceSetDeafen', [true]],
      ['voiceSetDevices', [{ input: 'Mic' }]],
      ['voiceSetTalkMode', ['push_to_talk']],
    ] as const) {
      expect((await f.ipc.invoke(f.event(), name, args)).ok, name).toBe(true);
    }
    expect(f.ipc.refusals).toBe(0);
  });

  it('does not ask for a gesture of an open-mic request that is refused for its shape', async () => {
    const f = fixture();
    const asked: number[] = [];
    const ipc = new Ipc({
      owned: () => f.contents, gate: f.gate, service: {} as Service, feeds: {} as Feeds, files: {} as Files, profiles: {} as ProfilesApi, voice: {} as VoiceApi,
      gestures: { take: (_c, within) => { asked.push(within); return 'pointer'; } }, now: () => 0, log: () => undefined,
    });
    await ipc.invoke(f.event(), 'voiceSetTalkMode', ['bogus']);
    expect(asked).toEqual([]);
  });

  const callerCases: [string, (f: Fixture) => InvokeEvent][] = [
    ['a subframe', (f) => f.event({ senderFrame: frame('frame-2', { parent: f.main }) })],
    ['another window', (f) => f.event({ sender: { isDestroyed: () => false, mainFrame: f.main } })],
    ['a destroyed sender', (f) => { f.contents.destroyed = true; return f.event(); }],
    ['a stale document generation', (f) => { f.gate.begin(); return f.event(); }],
    ['a foreign origin', (f) => f.event({ senderFrame: frame('frame-1', { url: 'https://evil.example/' }) })],
  ];

  it.each(callerCases)('refuses every voice function from %s and calls the service for none', async (_name, make) => {
    const f = fixture();
    f.input.recent = true;
    const event = make(f);
    const calls: [string, unknown[]][] = [
      ['voiceState', []], ['voiceJoin', [join]], ['voiceLeave', []], ['voiceRetry', []], ['voiceSetMute', [true]],
      ['voiceSetDeafen', [true]], ['voiceSetDevices', [{ input: 'Mic' }]], ['voiceSetTalkMode', ['open']],
    ];
    expect(calls.map(([name]) => name).sort()).toEqual([...VOICE_FUNCTIONS].sort());
    for (const [name, args] of calls) expect(await f.ipc.invoke(event, name, args), name).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it('says only "refused" when the service throws, and logs no argument', async () => {
    const f = fixture();
    f.input.recent = true;
    const ipc = new Ipc({
      owned: () => f.contents, gate: f.gate, service: {} as Service, feeds: {} as Feeds, files: {} as Files, profiles: {} as ProfilesApi,
      voice: { join: () => { throw new Error('boom secret-channel'); } } as unknown as VoiceApi,
      gestures: { take: () => 'pointer' }, now: () => 0, log: (line) => f.logs.push(line),
    });
    expect(await ipc.invoke(f.event(), 'voiceJoin', [{ ...join, channel: 'secret-channel' }])).toEqual({ ok: false, kind: 'refused' });
    expect(f.logs.join('\n')).not.toContain('secret-channel');
  });

  it.each([
    ['voiceState', [], 600],
    ['voiceJoin', [join], 20],
    ['voiceLeave', [], 60],
    ['voiceRetry', [], 20],
    ['voiceSetMute', [true], 120],
    ['voiceSetDeafen', [true], 120],
    ['voiceSetDevices', [{ input: 'Mic' }], 30],
    ['voiceSetTalkMode', ['open'], 30],
  ] as const)('limits %s to its own budget a minute', async (name, args, limit) => {
    const f = fixture();
    f.input.recent = true;
    let accepted = 0;
    for (let i = 0; i < limit + 5; i++) if ((await f.ipc.invoke(f.event(), name, args)).ok) accepted++;
    expect(accepted).toBe(limit);
    expect(f.logs.at(-1)).toContain('(rate)');
    f.clock.now += 60_001;
    expect((await f.ipc.invoke(f.event(), name, args)).ok).toBe(true);
  });
});

describe('voice events (issue #1374)', () => {
  const view: NativeVoiceView = { status: 'idle', generation: 1, channel: null, retryable: false };

  function target(committed: boolean, present = true) {
    const sent: [string, unknown][] = [];
    const gate = new DocumentGate();
    gate.begin();
    if (committed) gate.commit({ frameToken: 'f' });
    return { sent, gate, target: { contents: () => (present ? { send: (c: string, v: NativeVoiceView) => void sent.push([c, v]) } : null), gate: () => gate } };
  }

  it('sends the view on the voice channel to the owned contents while its document is committed', () => {
    const t = target(true);
    deliverVoice(t.target, view);
    expect(t.sent).toEqual([[VOICE_EVENTS, view]]);
  });

  it('sends nothing to a document that has begun but not committed, or after it ends, or with no window', () => {
    const uncommitted = target(false);
    deliverVoice(uncommitted.target, view);
    const ended = target(true);
    ended.gate.begin();
    deliverVoice(ended.target, view);
    const gone = target(true, false);
    deliverVoice(gone.target, view);
    expect([uncommitted.sent, ended.sent, gone.sent]).toEqual([[], [], []]);
  });
});
