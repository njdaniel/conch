import { EventEmitter } from 'node:events';

import type { Session, WebContents } from 'electron';
import { describe, expect, it, vi } from 'vitest';

import { Gestures } from '../../main/gestures.ts';
import { clipboardWriteAllowed, guardContents, guardSession } from '../../main/guards.ts';

type Request = (c: unknown, p: string, cb: (ok: boolean) => void, d: unknown) => void;
type Check = (c: unknown, p: string, origin: string, d: unknown) => boolean;

class FakeSession extends EventEmitter {
  request?: Request;
  check?: Check;
  webRequest = { onBeforeRequest: (): void => undefined };
  setPermissionRequestHandler(h: Request): void {
    this.request = h;
  }
  setPermissionCheckHandler(h: Check): void {
    this.check = h;
  }
  setDevicePermissionHandler(): void {}
  setDisplayMediaRequestHandler(): void {}
  setSpellCheckerEnabled(): void {}
}

class FakeContents extends EventEmitter {
  open?: (d: { url: string; postBody?: unknown }) => { action: string };
  setWindowOpenHandler(h: FakeContents['open']): void {
    this.open = h;
  }
}

function setup() {
  const clock = { now: 1_000 };
  const gestures = new Gestures(() => clock.now);
  const win = new FakeContents();
  const stranger = new FakeContents();
  gestures.attach(win);
  gestures.attach(stranger);
  const ses = new FakeSession();
  const deps = { log: () => undefined, gestures, owned: () => win };
  guardSession(ses as unknown as Session, deps);
  const click = (c: FakeContents) => c.emit('input-event', {}, { type: 'mouseUp' });
  const here = { isMainFrame: true, requestingUrl: 'conch-app://ui/' };
  const ask = (contents: unknown, permission: string, details: unknown = here): boolean[] => {
    const cb = vi.fn();
    ses.request?.(contents, permission, cb, details);
    return [cb.mock.calls[0]?.[0] as boolean, ses.check?.(contents, permission, 'conch-app://ui', details) ?? false];
  };
  return { clock, win, stranger, click, ask, deps, ses };
}

describe('the clipboard permission', () => {
  it('grants clipboard-sanitized-write to the owned main frame, right after a click', () => {
    const t = setup();
    t.click(t.win);
    expect(t.ask(t.win, 'clipboard-sanitized-write')).toEqual([true, true]);
  });

  it('refuses it with no gesture, and once the gesture is old', () => {
    const t = setup();
    expect(t.ask(t.win, 'clipboard-sanitized-write')).toEqual([false, false]);
    t.click(t.win);
    t.clock.now += 5_001;
    expect(t.ask(t.win, 'clipboard-sanitized-write')).toEqual([false, false]);
  });

  it('refuses it to any other web contents, a subframe, or another origin, gesture or not', () => {
    const t = setup();
    t.click(t.win);
    t.click(t.stranger);
    expect(t.ask(t.stranger, 'clipboard-sanitized-write')).toEqual([false, false]);
    expect(t.ask(t.win, 'clipboard-sanitized-write', { isMainFrame: false, requestingUrl: 'conch-app://ui/' })).toEqual([false, false]);
    expect(t.ask(t.win, 'clipboard-sanitized-write', { isMainFrame: true, requestingUrl: 'https://example.com/' })).toEqual([false, false]);
    expect(t.ask(t.win, 'clipboard-sanitized-write', { isMainFrame: true, requestingUrl: 'conch-app://ui.evil/' })).toEqual([false, false]);
    expect(t.ask(null, 'clipboard-sanitized-write')).toEqual([false, false]);
  });

  it('never grants clipboard-read, or anything else, however recent the click', () => {
    const t = setup();
    t.click(t.win);
    for (const permission of ['clipboard-read', 'deprecated-sync-clipboard-read', 'media', 'notifications', 'geolocation', 'openExternal', 'fileSystem', 'unknown']) {
      expect(t.ask(t.win, permission), permission).toEqual([false, false]);
    }
  });

  it('grants nothing when the guard was given no gesture source', () => {
    expect(clipboardWriteAllowed({ log: () => undefined }, {}, 'clipboard-sanitized-write', { isMainFrame: true, requestingUrl: 'conch-app://ui/' })).toBe(false);
  });
});

describe('a window the page opens', () => {
  it('is always denied, and a link goes to the OS browser only through Links', () => {
    const contents = new FakeContents();
    const fromWindow = vi.fn().mockReturnValue('opened');
    guardContents(contents as unknown as WebContents, { fromWindow });
    expect(contents.open?.({ url: 'https://example.com/' })).toEqual({ action: 'deny' });
    expect(fromWindow).toHaveBeenCalledExactlyOnceWith(contents, 'https://example.com/', false);
    expect(contents.open?.({ url: 'https://example.com/', postBody: { data: [] } })).toEqual({ action: 'deny' });
    expect(fromWindow).toHaveBeenLastCalledWith(contents, 'https://example.com/', true);
  });

  it('is denied with no Links at all', () => {
    const contents = new FakeContents();
    guardContents(contents as unknown as WebContents);
    expect(contents.open?.({ url: 'https://example.com/' })).toEqual({ action: 'deny' });
  });
});
