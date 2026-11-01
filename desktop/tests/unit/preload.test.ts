import { describe, expect, it, vi } from 'vitest';

import type { ConchDesktopBridge } from '../../../web/src/platform/desktop/bridge.ts';
import type { FeedHandlers } from '../../../web/src/platform/desktop/bridge.ts';
import { createBridge, type Invoke } from '../../preload/bridge.ts';
import { BRIDGE_FUNCTIONS, PROFILE_FUNCTIONS, FEED_EVENTS, INVOKED_FUNCTIONS } from '../../shared/bridge-names.ts';

const { exposeInMainWorld, invoke, on } = vi.hoisted(() => ({ exposeInMainWorld: vi.fn(), invoke: vi.fn(), on: vi.fn() }));
vi.mock('electron', () => ({ contextBridge: { exposeInMainWorld }, ipcRenderer: { invoke, on } }));

const noListen = (): void => undefined;

// Checked by the compiler against the interface: a function added to the
// bridge and not listed here, or listed and not declared, fails tsc.
const named: Record<keyof ConchDesktopBridge, true> = {
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
};

describe('the preload', () => {
  it('exposes one frozen conchDesktop whose keys are exactly the functions of bridge.ts, and nothing else', async () => {
    await import('../../preload/index.ts');
    expect(exposeInMainWorld).toHaveBeenCalledOnce();
    const [name, api] = exposeInMainWorld.mock.calls[0] as [string, Record<string, unknown>];
    expect(name).toBe('conchDesktop');
    expect(Object.isFrozen(api)).toBe(true);
    expect(Reflect.ownKeys(api).sort()).toEqual(Object.keys(named).sort());
    expect([...BRIDGE_FUNCTIONS, ...PROFILE_FUNCTIONS].sort()).toEqual(Object.keys(named).sort());
    for (const key of Reflect.ownKeys(api)) expect(typeof api[key as string], String(key)).toBe('function');
    for (const forbidden of ['ipcRenderer', 'invoke', 'send', 'on', 'sendSync', 'electron', 'require', 'process']) expect(forbidden in api).toBe(false);
    // It listens on the one feed channel, and passes on the message, not the
    // event.
    expect(on).toHaveBeenCalledOnce();
    expect(on.mock.calls[0]?.[0]).toBe(FEED_EVENTS);
  });

  it('sends each function on its own channel with its arguments and unwraps the answer', async () => {
    invoke.mockResolvedValue({ ok: true, value: { origin: 'https://x.example' } });
    const bridge = createBridge(invoke as Invoke, noListen);
    await expect(bridge.serverAddress()).resolves.toEqual({ origin: 'https://x.example' });
    expect(invoke).toHaveBeenLastCalledWith('conch:serverAddress');
    await bridge.signInWithPassword({ name: 'a', password: 'b' });
    expect(invoke).toHaveBeenLastCalledWith('conch:signInWithPassword', { name: 'a', password: 'b' });
    expect(INVOKED_FUNCTIONS).toHaveLength(28);
  });

  it('rejects with a bare "refused" when main refuses, or answers anything but an envelope', async () => {
    const bridge = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }), noListen);
    await expect(bridge.signOut()).rejects.toThrow(/^refused$/);
    const odd = createBridge(() => Promise.resolve('<script>'), noListen);
    await expect(odd.restore()).rejects.toThrow(/^refused$/);
    const nothing = createBridge(() => Promise.resolve(undefined), noListen);
    await expect(nothing.restore()).rejects.toThrow(/^refused$/);
  });

});

// A preload wired to a fake main: every invoke is recorded and answered ok,
// and send() delivers a feed event as main would.
function wired(answer: (channel: string, args: unknown[]) => unknown = () => true) {
  const calls: [string, unknown[]][] = [];
  let deliver: (message: unknown) => void = () => undefined;
  const bridge = createBridge(
    (channel, ...args) => {
      calls.push([channel, args]);
      return Promise.resolve({ ok: true, value: answer(channel, args) });
    },
    (callback) => {
      deliver = callback;
    },
  );
  const send = (event: unknown): void => {
    deliver(event);
  };
  return { bridge, calls, send };
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
    const rejected = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }), noListen);
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
