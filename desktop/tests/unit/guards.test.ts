import { EventEmitter } from 'node:events';

import type { App, Session, WebContents } from 'electron';
import { describe, expect, it, vi } from 'vitest';

import { foreign, guardContents, guardSession, installGuards } from '../../main/guards.ts';

class FakeSession extends EventEmitter {
  permissionRequest?: (c: unknown, p: string, cb: (ok: boolean) => void, d?: unknown) => void;
  permissionCheck?: (...args: unknown[]) => boolean;
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
  setPermissionCheckHandler(h: FakeSession["permissionCheck"]): void {
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

  it('denies the microphone to the page, whether or not voice is joined: it is the helper’s (issue #1374)', () => {
    for (const details of [{ mediaTypes: ['audio'] }, { mediaTypes: ['audio', 'video'] }, {}]) {
      const cb = vi.fn();
      ses.permissionRequest?.({}, 'media', cb, details);
      expect(cb).toHaveBeenCalledExactlyOnceWith(false);
    }
    expect(ses.permissionCheck?.('media', 'conch-app://ui', { mediaType: 'audio' })).toBe(false);
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

// Q-D4-1 on #1578: the CSP lets wss: and https: as far as main, and this
// filter lets through exactly the live grant's origin while the grant lives.
describe('the request filter and the live companion grant', () => {
  const ORIGIN = 'wss://lk.example.test';
  function filtered(grant: { origin: string | null }) {
    const logs: string[] = [];
    const ses = new FakeSession();
    guardSession(asSession(ses), { log: (l) => logs.push(l), liveOrigin: () => grant.origin });
    const verdict = (url: string): 'passed' | 'cancelled' => {
      const cb = vi.fn();
      ses.beforeRequest?.({ url }, cb);
      return cb.mock.calls[0]?.[0] && (cb.mock.calls[0][0] as { cancel?: boolean }).cancel === true ? 'cancelled' : 'passed';
    };
    return { verdict, logs };
  }

  it('cancels every wss and https origin with no live grant', () => {
    const { verdict, logs } = filtered({ origin: null });
    for (const url of ['wss://lk.example.test/rtc', 'https://lk.example.test/twirp/livekit.RoomService', 'wss://other.example.test/', 'https://example.com/', 'ws://127.0.0.1:7880/rtc', 'http://127.0.0.1/']) {
      expect(verdict(url), url).toBe('cancelled');
    }
    expect(logs).toHaveLength(6);
  });

  it('still lets the app, data: and blob: through with no grant', () => {
    const { verdict } = filtered({ origin: null });
    for (const url of ['conch-app://ui/', 'conch-app://ui/assets/a.js', 'data:image/png;base64,AAAA', 'blob:conch-app://ui/1234']) {
      expect(verdict(url), url).toBe('passed');
    }
  });

  it('lets wss and https of exactly the grant’s host and port through', () => {
    const { verdict } = filtered({ origin: ORIGIN });
    for (const url of [`${ORIGIN}/rtc?access_token=x`, 'wss://lk.example.test:443/rtc', 'https://lk.example.test/rtc/validate', 'https://lk.example.test:443/', 'https://LK.example.test/']) {
      expect(verdict(url), url).toBe('passed');
    }
  });

  it('keeps the app’s own scheme working beside a grant', () => {
    const { verdict } = filtered({ origin: ORIGIN });
    expect(verdict('conch-app://ui/')).toBe('passed');
  });

  it('cancels everything else while the grant lives', () => {
    const { verdict } = filtered({ origin: ORIGIN });
    const refused = [
      // another port, another host, a look-alike, a sub- and a parent domain
      'wss://lk.example.test:8443/rtc',
      'https://lk.example.test:444/',
      'wss://other.example.test/rtc',
      'wss://lk.example.test.evil.test/rtc',
      'wss://evil-lk.example.test/rtc',
      'wss://sub.lk.example.test/rtc',
      'wss://example.test/rtc',
      'wss://lk.example.test@evil.test/rtc',
      'https://lk.example.test.evil.test/',
      // userinfo
      'wss://user@lk.example.test/rtc',
      'wss://user:pass@lk.example.test/rtc',
      'https://user@lk.example.test/',
      // insecure or other schemes, same host
      'ws://lk.example.test/rtc',
      'http://lk.example.test/',
      'ftp://lk.example.test/',
      'file:///etc/passwd',
      'conch-app://lk.example.test/',
      // not a URL
      '',
      'lk.example.test',
      '//lk.example.test/',
    ];
    for (const url of refused) expect(verdict(url), url).toBe('cancelled');
  });

  it('cancels a ws: request from an https grant, and an https request from a wss grant on another port', () => {
    const { verdict } = filtered({ origin: 'https://lk.example.test:7443' });
    expect(verdict('ws://lk.example.test:7443/rtc')).toBe('cancelled');
    expect(verdict('wss://lk.example.test/rtc')).toBe('cancelled');
    expect(verdict('wss://lk.example.test:7443/rtc')).toBe('passed');
  });

  it('never lets a plain-ws or http grant origin pass anything', () => {
    for (const origin of ['ws://127.0.0.1:7880', 'http://127.0.0.1:7880', 'wss://u:p@lk.example.test', 'garbage', '']) {
      const { verdict } = filtered({ origin });
      for (const url of ['ws://127.0.0.1:7880/rtc', 'http://127.0.0.1:7880/', 'wss://lk.example.test/rtc', 'https://lk.example.test/']) {
        expect(verdict(url), `${origin} ${url}`).toBe('cancelled');
      }
    }
  });

  it('reads the grant afresh: after an end, a sign-out or a switch the origin is cancelled again at once', () => {
    const grant: { origin: string | null } = { origin: ORIGIN };
    const { verdict } = filtered(grant);
    expect(verdict(`${ORIGIN}/rtc`)).toBe('passed');
    // end / sign-out / switch: the companion forgets the grant.
    grant.origin = null;
    expect(verdict(`${ORIGIN}/rtc`)).toBe('cancelled');
    expect(verdict('https://lk.example.test/')).toBe('cancelled');
    // a new grant under another identity names another origin.
    grant.origin = 'wss://lk2.example.test';
    expect(verdict(`${ORIGIN}/rtc`)).toBe('cancelled');
    expect(verdict('wss://lk2.example.test/rtc')).toBe('passed');
  });

  it('without a liveOrigin dependency nothing leaves the app’s scheme', () => {
    const ses = new FakeSession();
    guardSession(asSession(ses), { log: () => undefined });
    const cb = vi.fn();
    ses.beforeRequest?.({ url: 'wss://lk.example.test/rtc' }, cb);
    expect(cb).toHaveBeenCalledExactlyOnceWith({ cancel: true });
  });
});
