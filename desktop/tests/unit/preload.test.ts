import { describe, expect, it, vi } from 'vitest';

import type { ConchDesktopBridge } from '../../../web/src/platform/desktop/bridge.ts';
import { createBridge } from '../../preload/bridge.ts';
import { BRIDGE_FUNCTIONS, INVOKED_FUNCTIONS } from '../../shared/bridge-names.ts';

const { exposeInMainWorld, invoke } = vi.hoisted(() => ({ exposeInMainWorld: vi.fn(), invoke: vi.fn() }));
vi.mock('electron', () => ({ contextBridge: { exposeInMainWorld }, ipcRenderer: { invoke } }));

// Checked by the compiler against the interface: a function added to the
// bridge and not listed here, or listed and not declared, fails tsc.
const named: Record<keyof ConchDesktopBridge, true> = {
  serverAddress: true,
  setServerAddress: true,
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
};

describe('the preload', () => {
  it('exposes one frozen conchDesktop whose keys are exactly the functions of bridge.ts, and nothing else', async () => {
    await import('../../preload/index.ts');
    expect(exposeInMainWorld).toHaveBeenCalledOnce();
    const [name, api] = exposeInMainWorld.mock.calls[0] as [string, Record<string, unknown>];
    expect(name).toBe('conchDesktop');
    expect(Object.isFrozen(api)).toBe(true);
    expect(Reflect.ownKeys(api).sort()).toEqual(Object.keys(named).sort());
    expect([...BRIDGE_FUNCTIONS].sort()).toEqual(Object.keys(named).sort());
    for (const key of Reflect.ownKeys(api)) expect(typeof api[key as string], String(key)).toBe('function');
    for (const forbidden of ['ipcRenderer', 'invoke', 'send', 'on', 'sendSync', 'electron', 'require', 'process']) expect(forbidden in api).toBe(false);
  });

  it('sends each function on its own channel with its arguments and unwraps the answer', async () => {
    invoke.mockResolvedValue({ ok: true, value: { origin: 'https://x.example' } });
    const bridge = createBridge(invoke as (channel: string, ...args: unknown[]) => Promise<unknown>);
    await expect(bridge.serverAddress()).resolves.toEqual({ origin: 'https://x.example' });
    expect(invoke).toHaveBeenLastCalledWith('conch:serverAddress');
    await bridge.signInWithPassword({ name: 'a', password: 'b' });
    expect(invoke).toHaveBeenLastCalledWith('conch:signInWithPassword', { name: 'a', password: 'b' });
    expect(INVOKED_FUNCTIONS).toHaveLength(16);
  });

  it('rejects with a bare "refused" when main refuses, or answers anything but an envelope', async () => {
    const bridge = createBridge(() => Promise.resolve({ ok: false, kind: 'refused' }));
    await expect(bridge.signOut()).rejects.toThrow(/^refused$/);
    const odd = createBridge(() => Promise.resolve('<script>'));
    await expect(odd.restore()).rejects.toThrow(/^refused$/);
    const nothing = createBridge(() => Promise.resolve(undefined));
    await expect(nothing.restore()).rejects.toThrow(/^refused$/);
  });

  it('closes a subscription at once until live feeds exist, and an unsubscribe stops that', async () => {
    const bridge = createBridge(() => Promise.reject(new Error('no channel for subscribe')));
    const onClose = vi.fn();
    bridge.subscribe({ kind: 'messages' }, { onOpen: vi.fn(), onFrame: vi.fn(), onClose });
    await Promise.resolve();
    expect(onClose).toHaveBeenCalledOnce();
    const quiet = vi.fn();
    bridge.subscribe({ kind: 'events' }, { onOpen: vi.fn(), onFrame: vi.fn(), onClose: quiet })();
    await Promise.resolve();
    expect(quiet).not.toHaveBeenCalled();
  });
});
