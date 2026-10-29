import { describe, expect, it } from 'vitest';
import { platform } from '.';
import { platform as browser } from './browser';
import type { ConchDesktopBridge } from './desktop/bridge';
import { platform as desktop } from './desktop';

describe('the platform', () => {
  it('is the browser’s in a normal build and under test', () => {
    expect(platform).toBe(browser);
    expect(platform.name).toBe('browser');
    expect(platform.services.browserVoice).toBe(true);
  });

  it('the browser’s credential store keeps a tab’s and a lasting credential apart, and never throws', () => {
    window.sessionStorage.clear();
    window.localStorage.clear();
    const store = platform.credentials;
    expect(store.read()).toBeNull();
    expect(store.write('member', 'lasting')).toBe(true);
    expect(store.write('operator', 'tab')).toBe(true);
    // The tab's own comes first.
    expect(store.read()).toEqual({ token: 'operator', place: 'tab' });
    expect(store.isStored({ token: 'member', place: 'lasting' })).toBe(true);
    // A late drop of a credential that is no longer the stored one does nothing.
    store.drop({ token: 'stale', place: 'lasting' });
    expect(store.isStored({ token: 'member', place: 'lasting' })).toBe(true);
    store.drop({ token: 'member', place: 'lasting' });
    expect(store.isStored({ token: 'member', place: 'lasting' })).toBe(false);
    window.sessionStorage.clear();
  });
});

describe('the desktop stub', () => {
  it('answers every operation as a server that cannot be reached', async () => {
    expect(desktop.name).toBe('desktop');
    const call = { op: 'whoami' } as const;
    await expect(desktop.http.get(call, 't')).rejects.toBeInstanceOf(TypeError);
    await expect(desktop.http.send(call, 't', { keepalive: false })).rejects.toBeInstanceOf(TypeError);
    await expect(desktop.http.open(call)).rejects.toBeInstanceOf(TypeError);
    await expect(desktop.http.health({ op: 'health' })).rejects.toBeInstanceOf(TypeError);
    expect(() => desktop.sockets.open('messagesSocket', {})).toThrow(TypeError);
  });

  it('keeps no credential, offers no notification and no browser voice', () => {
    expect(desktop.credentials.read()).toBeNull();
    expect(desktop.credentials.write('t', 'lasting')).toBe(false);
    expect(desktop.services.notifications.supported()).toBe(false);
    expect(desktop.services.notifications.permission()).toBe('unsupported');
    expect(desktop.services.browserVoice).toBe(false);
  });
});

// The bridge is types only. This list is checked by the compiler against the
// interface: a function added to ConchDesktopBridge and not listed here, or
// listed and not declared, fails tsc.
describe('the desktop bridge type', () => {
  it('declares the functions the desktop main and renderer need', () => {
    const named: Record<keyof ConchDesktopBridge, true> = {
      serverAddress: true,
      setServerAddress: true,
      request: true,
      subscribe: true,
      restore: true,
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
    expect(Object.keys(named)).toHaveLength(15);
  });
});
