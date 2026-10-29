import { EventEmitter } from 'node:events';

import type { App, Session, WebContents } from 'electron';
import { describe, expect, it, vi } from 'vitest';

import { foreign, guardContents, guardSession, installGuards } from '../../main/guards.ts';

class FakeSession extends EventEmitter {
  permissionRequest?: (c: unknown, p: string, cb: (ok: boolean) => void) => void;
  permissionCheck?: () => boolean;
  devicePermission?: () => boolean;
  displayMedia?: (r: unknown, cb: (s: unknown) => void) => void;
  spellcheck: boolean | undefined;
  beforeRequest?: (d: { url: string }, cb: (r: { cancel?: boolean }) => void) => void;
  webRequest = {
    onBeforeRequest: (h: FakeSession['beforeRequest']): void => {
      this.beforeRequest = h;
    },
  };
  setPermissionRequestHandler(h: FakeSession['permissionRequest']): void {
    this.permissionRequest = h;
  }
  setPermissionCheckHandler(h: () => boolean): void {
    this.permissionCheck = h;
  }
  setDevicePermissionHandler(h: () => boolean): void {
    this.devicePermission = h;
  }
  setDisplayMediaRequestHandler(h: FakeSession['displayMedia']): void {
    this.displayMedia = h;
  }
  setSpellCheckerEnabled(on: boolean): void {
    this.spellcheck = on;
  }
}

class FakeContents extends EventEmitter {
  windowOpen?: (d: unknown) => { action: string };
  setWindowOpenHandler(h: FakeContents['windowOpen']): void {
    this.windowOpen = h;
  }
}

const asSession = (s: FakeSession): Session => s as unknown as Session;
const asContents = (c: FakeContents): WebContents => c as unknown as WebContents;

function navigation(url: string): { url: string; prevented: boolean; preventDefault(): void } {
  return {
    url,
    prevented: false,
    preventDefault() {
      this.prevented = true;
    },
  };
}

describe('guardSession', () => {
  const logs: string[] = [];
  const ses = new FakeSession();
  guardSession(asSession(ses), { log: (l) => logs.push(l) });

  it('denies every permission request and check', () => {
    for (const permission of ['media', 'geolocation', 'notifications', 'clipboard-read', 'display-capture', 'hid']) {
      const cb = vi.fn();
      ses.permissionRequest?.({}, permission, cb);
      expect(cb).toHaveBeenCalledExactlyOnceWith(false);
    }
    expect(ses.permissionCheck?.()).toBe(false);
    expect(ses.devicePermission?.()).toBe(false);
  });

  it('denies display capture', () => {
    const cb = vi.fn();
    ses.displayMedia?.({}, cb);
    expect(cb).toHaveBeenCalledExactlyOnceWith(null);
  });

  it('cancels HID, USB and serial choosers', () => {
    for (const [event, args] of [
      ['select-hid-device', [{}]],
      ['select-usb-device', [{}]],
      ['select-serial-port', [[], {}]],
    ] as const) {
      const preventDefault = vi.fn();
      const cb = vi.fn();
      ses.emit(event, { preventDefault }, ...args, cb);
      expect(preventDefault, event).toHaveBeenCalledOnce();
      expect(cb, event).toHaveBeenCalledOnce();
      expect(cb.mock.calls[0]?.[0] ?? '', event).toBe('');
    }
  });

  it('cancels every download', () => {
    const cancel = vi.fn();
    ses.emit('will-download', {}, { cancel });
    expect(cancel).toHaveBeenCalledOnce();
  });

  it('turns the spell checker off', () => {
    expect(ses.spellcheck).toBe(false);
  });

  it('cancels, and logs without the path, a request that is not to the app', () => {
    const cb = vi.fn();
    ses.beforeRequest?.({ url: 'https://example.com/secret/path?q=1' }, cb);
    expect(cb).toHaveBeenCalledExactlyOnceWith({ cancel: true });
    expect(logs).toEqual(['conch-desktop: blocked a request to https://example.com']);
  });

  it('lets the app, data: and blob: through', () => {
    for (const url of ['conch-app://ui/', 'conch-app://ui/assets/a.js', 'data:image/png;base64,AAAA', 'blob:conch-app://ui/1234']) {
      const cb = vi.fn();
      ses.beforeRequest?.({ url }, cb);
      expect(cb, url).toHaveBeenCalledExactlyOnceWith({});
    }
  });
});

describe('foreign', () => {
  it.each(['https://example.com/', 'http://127.0.0.1/', 'ws://x/', 'file:///etc/passwd', 'conch-app://other/', 'conch-app://ui:1/', 'ftp://x/'])(
    '%s is foreign',
    (url) => {
      expect(foreign(url)).toBe(true);
    },
  );
});

describe('guardContents', () => {
  const contents = new FakeContents();
  guardContents(asContents(contents));

  it.each(['will-navigate', 'will-redirect', 'will-frame-navigate'])('%s to anything but the initial document is prevented', (event) => {
    for (const url of ['https://example.com/', 'conch-app://ui/other', 'conch-app://other/', 'file:///etc/passwd', 'about:blank', 'javascript:alert(1)']) {
      const n = navigation(url);
      contents.emit(event, n);
      expect(n.prevented, `${event} ${url}`).toBe(true);
    }
    const same = navigation('conch-app://ui/');
    contents.emit(event, same);
    expect(same.prevented).toBe(false);
  });

  it('denies every new window', () => {
    expect(contents.windowOpen?.({ url: 'https://example.com/' })).toEqual({ action: 'deny' });
    expect(contents.windowOpen?.({ url: 'conch-app://ui/' })).toEqual({ action: 'deny' });
  });

  it('prevents webviews', () => {
    const preventDefault = vi.fn();
    contents.emit('will-attach-webview', { preventDefault });
    expect(preventDefault).toHaveBeenCalledOnce();
  });

  it('cancels bluetooth choosers', () => {
    const preventDefault = vi.fn();
    const cb = vi.fn();
    contents.emit('select-bluetooth-device', { preventDefault }, [], cb);
    expect(preventDefault).toHaveBeenCalledOnce();
    expect(cb).toHaveBeenCalledExactlyOnceWith('');
  });
});

describe('installGuards', () => {
  it('guards every web contents the app creates', () => {
    const app = new EventEmitter();
    installGuards(app as unknown as App, { log: () => undefined });
    const contents = new FakeContents();
    const ses = new FakeSession();
    app.emit('web-contents-created', {}, Object.assign(contents, { session: ses }));
    expect(contents.windowOpen?.({})).toEqual({ action: 'deny' });
    expect(ses.permissionCheck?.()).toBe(false);
    expect(contents.listenerCount('will-navigate')).toBe(1);
  });
});
