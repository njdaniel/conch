import { describe, expect, it, vi } from 'vitest';

import type { CompanionEvent, ConchDesktopBridge, DesktopCompanionBridge, DesktopInviteBridge, DesktopSettingsBridge, DesktopUpdateBridge, DesktopVoiceBridge, NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import type { FeedHandlers } from '../../../web/src/platform/desktop/bridge.ts';
import { createBridge, type Invoke } from '../../preload/bridge.ts';
import { BRIDGE_FUNCTIONS, COMPANION_EVENTS, COMPANION_FUNCTIONS, INVITE_FUNCTIONS, NOTIFY_FUNCTIONS, PROFILE_FUNCTIONS, VOICE_FUNCTIONS, SETTINGS_FUNCTIONS, UPDATE_FUNCTIONS, FEED_EVENTS, NOTIFICATION_EVENTS, UPDATE_EVENTS, VOICE_EVENTS, INVOKED_FUNCTIONS } from '../../shared/bridge-names.ts';

const { exposeInMainWorld, invoke, on, sendSync } = vi.hoisted(() => ({ exposeInMainWorld: vi.fn(), invoke: vi.fn(), on: vi.fn(), sendSync: vi.fn() }));
vi.mock('electron', () => ({ contextBridge: { exposeInMainWorld }, ipcRenderer: { invoke, on, sendSync } }));

const noListen = (): void => undefined;

// Checked by the compiler against the interface: a function added to the
// bridge and not listed here, or listed and not declared, fails tsc.
const named: Record<keyof ConchDesktopBridge | keyof DesktopVoiceBridge | keyof DesktopInviteBridge | keyof DesktopSettingsBridge | keyof DesktopCompanionBridge | keyof DesktopUpdateBridge, true> = {
  serverAddress: true,
  request: true,
  subscribe: true,
  restore: true,
  signInWithCredential: true,
  keepCredential: true,
  signInWithPassword: true,
  signInWithCode: true,
  redeemInvite: true,
  redeemInviteAsGuest: true,
  redeemSignInLink: true,
  redeemSetupToken: true,
  resetPassword: true,
  joinAsGuest: true,
  createAccount: true,
  signOut: true,
  replaceCredential: true,
  profiles: true,
  profileAdd: true,
  profileRename: true,
  profileRemove: true,
  profileSwitch: true,
  storageRetry: true,
  profilesRecover: true,
  uploadFile: true,
  downloadFile: true,
  saveFile: true,
  notify: true,
  notifyClose: true,
  notificationSupported: true,
  onNotification: true,
  voiceState: true,
  voiceJoin: true,
  voiceLeave: true,
  voiceRetry: true,
  voiceSetMute: true,
  voiceSetDeafen: true,
  voiceSetDevices: true,
  voiceSetTalkMode: true,
  voiceDevices: true,
  voiceSaveDevice: true,
  voiceKeyDevices: true,
  voiceSaveTalkBinding: true,
  inviteCheck: true,
  inviteAccept: true,
  inviteCancel: true,
  inviteTake: true,
  onVoiceState: true,
  settings: true,
  settingsSet: true,
  companionGrant: true,
  companionEnd: true,
  onCompanion: true,
  updateState: true,
  updateRestart: true,
  onUpdateState: true,
};

describe('the preload', () => {
  it('exposes one frozen conchDesktop whose keys are exactly the functions of bridge.ts, and nothing else', async () => {
    await import('../../preload/index.ts');
    expect(exposeInMainWorld).toHaveBeenCalledOnce();
    const [name, api] = exposeInMainWorld.mock.calls[0] as [string, Record<string, unknown>];
    expect(name).toBe('conchDesktop');
    expect(Object.isFrozen(api)).toBe(true);
    expect(Reflect.ownKeys(api).sort()).toEqual(Object.keys(named).sort());
    expect([...BRIDGE_FUNCTIONS, ...PROFILE_FUNCTIONS, ...VOICE_FUNCTIONS, ...INVITE_FUNCTIONS, ...NOTIFY_FUNCTIONS, ...SETTINGS_FUNCTIONS, 'notificationSupported', 'onNotification', 'onVoiceState', ...COMPANION_FUNCTIONS, 'onCompanion', ...UPDATE_FUNCTIONS, 'onUpdateState'].sort()).toEqual(Object.keys(named).sort());
    for (const key of Reflect.ownKeys(api)) expect(typeof api[key as string], String(key)).toBe('function');
    for (const forbidden of ['ipcRenderer', 'invoke', 'send', 'on', 'sendSync', 'electron', 'require', 'process']) expect(forbidden in api).toBe(false);
    // It listens on the feed, voice, companion, notification and update
    // channels, and passes on the message, not the event.
    expect((on.mock.calls as unknown[][]).map((call) => call[0])).toEqual([FEED_EVENTS, VOICE_EVENTS, COMPANION_EVENTS, NOTIFICATION_EVENTS, UPDATE_EVENTS]);
    // No exposed or invoked name says press, release, transmit, gate or ptt.
    for (const name of [...Reflect.ownKeys(api).map(String), ...INVOKED_FUNCTIONS]) expect(name).not.toMatch(/press|release|transmit|gate|ptt/i);
  });

  it('sends each function on its own channel with its arguments and unwraps the answer', async () => {
    invoke.mockResolvedValue({ ok: true, value: { origin: 'https://x.example' } });
    const bridge = createBridge(invoke as Invoke, noListen, noListen, noListen);
    await expect(bridge.serverAddress()).resolves.toEqual({ origin: 'https://x.example' });
    expect(invoke).toHaveBeenLastCalledWith('conch:serverAddress');
    await bridge.signInWithPassword({ name: 'a', password: 'b' });
    expect(invoke).toHaveBeenLastCalledWith('conch:signInWithPassword', { name: 'a', password: 'b' });
    await bridge.replaceCredential({ operation: 'changePassword', body: { current: 'a', password: 'b' } });
    expect(invoke).toHaveBeenLastCalledWith('conch:replaceCredential', { operation: 'changePassword', body: { current: 'a', password: 'b' } });
    await bridge.notify({ tag: 't', title: 'title', body: 'body', channel: 'general' });
    expect(invoke).toHaveBeenLastCalledWith('conch:notify', { tag: 't', title: 'title', body: 'body', channel: 'general' });
    await bridge.notifyClose({ tag: 't' });
    expect(invoke).toHaveBeenLastCalledWith('conch:notifyClose', { tag: 't' });
    await bridge.settingsSet({ launchAtLogin: true });
    expect(invoke).toHaveBeenLastCalledWith('conch:settingsSet', { launchAtLogin: true });
    expect(INVOKED_FUNCTIONS).toHaveLength(53);
  });

  it('rejects with a bare "refused" when main refuses, or answers anything but an envelope', async () => {
    const bridge = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }), noListen, noListen, noListen);
    await expect(bridge.signOut()).rejects.toThrow(/^refused$/);
    const odd = createBridge(() => Promise.resolve('<script>'), noListen, noListen, noListen);
    await expect(odd.restore()).rejects.toThrow(/^refused$/);
    const nothing = createBridge(() => Promise.resolve(undefined), noListen, noListen, noListen);
    await expect(nothing.restore()).rejects.toThrow(/^refused$/);
  });

  it('reads notification support only from a valid synchronous envelope', () => {
    const bridge = createBridge(() => Promise.resolve({ ok: true, value: true }), noListen, noListen, noListen, noListen, () => ({ ok: true, value: true }));
    expect(bridge.notificationSupported()).toBe(true);
    const odd = createBridge(() => Promise.resolve({ ok: true, value: true }), noListen, noListen, noListen, noListen, () => ({ ok: true, value: 'yes' }));
    expect(() => odd.notificationSupported()).toThrow(/^refused$/);
  });

});

// A preload wired to a fake main: every invoke is recorded and answered ok,
// and send() delivers a feed event as main would.
function wired(answer: (channel: string, args: unknown[]) => unknown = () => true) {
  const calls: [string, unknown[]][] = [];
  let deliver: (message: unknown) => void = () => undefined;
  let deliverVoice: (message: unknown) => void = () => undefined;
  let deliverCompanion: (message: unknown) => void = () => undefined;
  let deliverNotification: (message: unknown) => void = () => undefined;
  let deliverUpdate: (message: unknown) => void = () => undefined;
  const bridge = createBridge(
    (channel, ...args) => {
      calls.push([channel, args]);
      return Promise.resolve({ ok: true, value: answer(channel, args) });
    },
    (callback) => {
      deliver = callback;
    },
    (callback) => {
      deliverVoice = callback;
    },
    (callback) => {
      deliverCompanion = callback;
    },
    (callback) => {
      deliverNotification = callback;
    },
    undefined,
    (callback) => {
      deliverUpdate = callback;
    },
  );
  const send = (event: unknown): void => {
    deliver(event);
  };
  const sendVoice = (message: unknown): void => {
    deliverVoice(message);
  };
  const sendCompanion = (message: unknown): void => {
    deliverCompanion(message);
  };
  const sendNotification = (message: unknown): void => {
    deliverNotification(message);
  };
  const sendUpdate = (message: unknown): void => {
    deliverUpdate(message);
  };
  return { bridge, calls, send, sendVoice, sendCompanion, sendNotification, sendUpdate };
}

function handlers(): FeedHandlers & { seen: string[] } {
  const seen: string[] = [];
  return {
    seen,
    onOpen: () => seen.push('open'),
    onFrame: (frame) => seen.push(`frame ${frame}`),
    onClose: (code, reason) => seen.push(`close ${String(code)} ${reason}`),
  };
}

describe('the preload’s feeds', () => {
  it('numbers each subscription itself and asks main to open it with the request as given', async () => {
    const { bridge, calls } = wired();
    bridge.subscribe({ kind: 'messages', channel: 'general', after: 4 }, handlers());
    bridge.subscribe({ kind: 'occupancy' }, handlers());
    await Promise.resolve();
    expect(calls).toEqual([
      ['conch:feedSubscribe', [1, { kind: 'messages', channel: 'general', after: 4 }]],
      ['conch:feedSubscribe', [2, { kind: 'occupancy' }]],
    ]);
  });

  it('hands each frame of a batch to the page in order, then acknowledges the batch', () => {
    const { bridge, calls, send } = wired();
    const h = handlers();
    bridge.subscribe({ kind: 'occupancy' }, h);
    send({ id: 1, type: 'open' });
    send({ id: 1, type: 'frames', frames: ['a', 'b'] });
    expect(h.seen).toEqual(['open', 'frame a', 'frame b']);
    expect(calls.at(-1)).toEqual(['conch:feedAck', [1]]);
    send({ id: 1, type: 'close', code: 1006, reason: '' });
    expect(h.seen.at(-1)).toBe('close 1006 ');
    // A close is the last word: nothing after it reaches the page.
    send({ id: 1, type: 'frames', frames: ['late'] });
    expect(h.seen).toHaveLength(4);
  });

  it('drops what main sent for a feed the page has unsubscribed, and tells main once', () => {
    const { bridge, calls, send } = wired();
    const h = handlers();
    const stop = bridge.subscribe({ kind: 'occupancy' }, h);
    send({ id: 1, type: 'open' });
    stop();
    stop();
    send({ id: 1, type: 'frames', frames: ['late'] });
    send({ id: 1, type: 'close', code: 1000, reason: '' });
    expect(h.seen).toEqual(['open']);
    expect(calls.filter(([channel]) => channel === 'conch:feedUnsubscribe')).toEqual([['conch:feedUnsubscribe', [1]]]);
    // No acknowledgement for a batch it did not take.
    expect(calls.some(([channel]) => channel === 'conch:feedAck')).toBe(false);
  });

  it('stops a batch where the page unsubscribes in the middle of it, and does not acknowledge it', () => {
    const { bridge, calls, send } = wired();
    const seen: string[] = [];
    const stop = bridge.subscribe({ kind: 'occupancy' }, {
      onOpen: () => undefined,
      onFrame: (frame) => {
        seen.push(frame);
        stop();
      },
      onClose: () => undefined,
    });
    send({ id: 1, type: 'frames', frames: ['a', 'b'] });
    expect(seen).toEqual(['a']);
    expect(calls.some(([channel]) => channel === 'conch:feedAck')).toBe(false);
  });

  it('closes a subscription main refuses with 1013, which the page retries', async () => {
    const { bridge } = wired(() => false);
    const h = handlers();
    bridge.subscribe({ kind: 'occupancy' }, h);
    await Promise.resolve();
    await Promise.resolve();
    expect(h.seen).toEqual(['close 1013 refused']);
    const rejected = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }), noListen, noListen, noListen);
    const r = handlers();
    rejected.subscribe({ kind: 'occupancy' }, r);
    await Promise.resolve();
    await Promise.resolve();
    expect(r.seen).toEqual(['close 1013 refused']);
  });

  it('ignores a message that is not a feed event, or is for no feed it holds', () => {
    const { bridge, send } = wired();
    const h = handlers();
    bridge.subscribe({ kind: 'occupancy' }, h);
    for (const odd of [null, 'open', { id: '1', type: 'open' }, { id: 1, type: 'frames', frames: [1] }, { id: 1, type: 'close', code: '1000' }, { id: 1, type: 'other' }, { id: 2, type: 'open' }]) send(odd);
    expect(h.seen).toEqual([]);
  });
});

describe('the preload’s notifications (issue #1590)', () => {
  it('delivers exact click tags and drops malformed or extra-field events', () => {
    const { bridge, sendNotification } = wired();
    const seen: string[] = [];
    const stop = bridge.onNotification((event) => seen.push(event.tag));
    for (const odd of [null, 'click', {}, { type: 'open', tag: 't' }, { type: 'click', tag: 5 }, { type: 'click', tag: 't', body: 'secret' }]) sendNotification(odd);
    expect(seen).toEqual([]);
    sendNotification({ type: 'click', tag: 'one' });
    stop();
    sendNotification({ type: 'click', tag: 'two' });
    expect(seen).toEqual(['one']);
  });
});

describe('the preload’s voice family', () => {
  const view: NativeVoiceView = { status: 'idle', generation: 1, channel: null, retryable: false };
  const running: NativeVoiceView = {
    status: 'running',
    generation: 2,
    channel: 'general',
    retryable: false,
    helper: { version: 2, phase: 'connected', capture: 'on', transmitting: false, muted: false, deafened: false, blocked: [], talkMode: 'push_to_talk', ptt: 'ready' },
  };

  it('sends each voice function on its own channel with the argument it was given', async () => {
    const { bridge, calls } = wired();
    await bridge.voiceState();
    await bridge.voiceJoin({ channel: 'general', muted: true, deafened: false });
    await bridge.voiceLeave();
    await bridge.voiceRetry();
    await bridge.voiceSetMute(true);
    await bridge.voiceSetDeafen(false);
    await bridge.voiceSetDevices({ input: 'Mic' });
    await bridge.voiceSetTalkMode('open');
    await bridge.voiceDevices();
    await bridge.voiceSaveDevice({ key: 'audio.input', value: 'Mic' });
    await bridge.voiceKeyDevices();
    await bridge.voiceSaveTalkBinding({ deviceId: 'abc', code: 275 });
    expect(calls).toEqual([
      ['conch:voiceState', []],
      ['conch:voiceJoin', [{ channel: 'general', muted: true, deafened: false }]],
      ['conch:voiceLeave', []],
      ['conch:voiceRetry', []],
      ['conch:voiceSetMute', [true]],
      ['conch:voiceSetDeafen', [false]],
      ['conch:voiceSetDevices', [{ input: 'Mic' }]],
      ['conch:voiceSetTalkMode', ['open']],
      ['conch:voiceDevices', []],
      ['conch:voiceSaveDevice', [{ key: 'audio.input', value: 'Mic' }]],
      ['conch:voiceKeyDevices', []],
      ['conch:voiceSaveTalkBinding', [{ deviceId: 'abc', code: 275 }]],
    ]);
  });

  it('rejects with a bare "refused" when main refuses a voice call', async () => {
    const bridge = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }), noListen, noListen, noListen);
    await expect(bridge.voiceJoin({ channel: 'a', muted: false, deafened: false })).rejects.toThrow(/^refused$/);
  });

  it('hands a well-formed view to each listener, and the view alone', () => {
    const { bridge, sendVoice } = wired();
    const a: NativeVoiceView[] = [];
    const b: NativeVoiceView[] = [];
    bridge.onVoiceState((v) => a.push(v));
    const stopB = bridge.onVoiceState((v) => b.push(v));
    sendVoice(view);
    sendVoice(running);
    stopB();
    sendVoice(view);
    expect(a).toEqual([view, running, view]);
    expect(b).toEqual([view, running]);
  });

  it('passes the lock-mute marker only as true', () => {
    const { bridge, sendVoice } = wired();
    const seen: NativeVoiceView[] = [];
    bridge.onVoiceState((v) => seen.push(v));
    const marked = { ...running, mutedByLock: true };
    sendVoice(marked);
    for (const odd of [{ ...running, mutedByLock: false }, { ...running, mutedByLock: 'yes' }, { ...running, mutedByLock: 1 }]) sendVoice(odd);
    expect(seen).toEqual([marked]);
  });

  it('drops a message that is not exactly a NativeVoiceView', () => {
    const { bridge, sendVoice } = wired();
    const seen: unknown[] = [];
    bridge.onVoiceState((v) => seen.push(v));
    const helper = running.helper;
    for (const odd of [
      null, 'idle', 5, [], {}, { ...view, status: 'transmitting' }, { ...view, status: 5 }, { ...view, generation: '1' }, { ...view, generation: -1 },
      { ...view, channel: 5 }, { ...view, channel: undefined }, { ...view, retryable: 'no' }, { ...view, problem: 5 }, { ...view, token: 'secret' },
      { ...view, helperPath: '/x' }, { ...running, helper: 'on' }, { ...running, helper: { ...helper, phase: 'nope' } },
      { ...running, helper: { ...helper, transmitting: 'yes' } }, { ...running, helper: { ...helper, blocked: ['nope'] } },
      { ...running, helper: { ...helper, blocked: 'none' } }, { ...running, helper: { ...helper, version: 3 } },
      { ...running, helper: { ...helper, pid: 4 } }, { ...running, helper: { ...helper, ptt: 'press' } }, { ...running, helper: { ...helper, talkMode: 'hold' } },
    ]) sendVoice(odd);
    expect(seen).toEqual([]);
  });

  it('keeps delivering after a listener throws, and the error still reaches the console', async () => {
    const { bridge, sendVoice } = wired();
    const seen: NativeVoiceView[] = [];
    bridge.onVoiceState(() => {
      throw new Error('listener');
    });
    bridge.onVoiceState((v) => seen.push(v));
    const caught = new Promise<unknown>((resolve) => process.once('uncaughtException', resolve));
    sendVoice(view);
    expect(seen).toEqual([view]);
    expect(await caught).toEqual(new Error('listener'));
  });
});

describe('the update family (issue #1600)', () => {
  it('invokes the state and the restart on their own channels', async () => {
    const w = wired(() => ({ updated: false, version: null }));
    await expect(w.bridge.updateState()).resolves.toEqual({ updated: false, version: null });
    await w.bridge.updateRestart();
    expect(w.calls).toEqual([['conch:updateState', []], ['conch:updateRestart', []]]);
  });

  it('hands the page each exact InstalledView and drops anything else', () => {
    const w = wired();
    const seen: unknown[] = [];
    const off = w.bridge.onUpdateState((v) => seen.push(v));
    w.sendUpdate({ updated: true, version: '0.1.0-alpha.18' });
    w.sendUpdate({ updated: false, version: null });
    for (const bad of [null, 'x', {}, { updated: true }, { updated: 'yes', version: null }, { updated: true, version: 5 }, { updated: true, version: 'x'.repeat(65) }, { updated: true, version: null, extra: 1 }]) w.sendUpdate(bad);
    expect(seen).toEqual([{ updated: true, version: '0.1.0-alpha.18' }, { updated: false, version: null }]);
    off();
    w.sendUpdate({ updated: true, version: '1.0.0' });
    expect(seen).toHaveLength(2);
  });
});

describe('the preload’s companion (issue #1579)', () => {
  it('sends the grant request and the end on their own channels, with the argument given', async () => {
    const { bridge, calls } = wired((channel) => (channel === 'conch:companionEnd' ? true : { ok: false, error: 'not-in-voice' }));
    await expect(bridge.companionGrant({ channel: 'general' })).resolves.toEqual({ ok: false, error: 'not-in-voice' });
    await expect(bridge.companionEnd()).resolves.toBe(true);
    expect(calls).toEqual([
      ['conch:companionGrant', [{ channel: 'general' }]],
      ['conch:companionEnd', []],
    ]);
  });

  it('rejects with a bare "refused" when main refuses a companion call (a subframe, another window, a stale document, no gesture, extra keys)', async () => {
    const bridge = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }), noListen, noListen, noListen);
    await expect(bridge.companionGrant({ channel: 'general' })).rejects.toThrow(/^refused$/);
    await expect(bridge.companionEnd()).rejects.toThrow(/^refused$/);
  });

  it('hands each end to each listener, and the event alone', () => {
    const { bridge, sendCompanion } = wired();
    const a: CompanionEvent[] = [];
    const b: CompanionEvent[] = [];
    bridge.onCompanion((e) => a.push(e));
    const stopB = bridge.onCompanion((e) => b.push(e));
    const leave: CompanionEvent = { type: 'end', reason: 'leave' };
    const renewal: CompanionEvent = { type: 'end', reason: 'renewal' };
    sendCompanion(leave);
    stopB();
    sendCompanion(renewal);
    expect(a).toEqual([leave, renewal]);
    expect(b).toEqual([leave]);
  });

  it('drops a message that is not exactly a CompanionEvent', () => {
    const { bridge, sendCompanion } = wired();
    const seen: unknown[] = [];
    bridge.onCompanion((e) => seen.push(e));
    for (const odd of [
      null, 'end', 5, [], {}, { type: 'end' }, { type: 'start', reason: 'leave' }, { type: 'end', reason: 'bored' }, { type: 'end', reason: 5 },
      { type: 'end', reason: 'toString' }, { type: 'end', reason: 'leave', token: 'secret' }, { type: 'end', reason: 'leave', livekitUrl: 'wss://x' },
      [{ type: 'end', reason: 'leave' }],
    ]) sendCompanion(odd);
    expect(seen).toEqual([]);
  });

  it('keeps delivering after a listener throws, and the error still reaches the console', async () => {
    const { bridge, sendCompanion } = wired();
    const seen: CompanionEvent[] = [];
    bridge.onCompanion(() => {
      throw new Error('companion listener');
    });
    bridge.onCompanion((e) => seen.push(e));
    const caught = new Promise<unknown>((resolve) => process.once('uncaughtException', resolve));
    sendCompanion({ type: 'end', reason: 'crash' });
    expect(seen).toEqual([{ type: 'end', reason: 'crash' }]);
    expect(await caught).toEqual(new Error('companion listener'));
  });
});
