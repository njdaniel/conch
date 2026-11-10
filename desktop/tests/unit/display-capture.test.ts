import { EventEmitter } from 'node:events';

import type { Session } from 'electron';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { CAPTURE_SOURCES, captureVerdict, CRASH_GRACE_MS, DisplayCapture, displayPermission, type CaptureFrame, type CaptureSource } from '../../main/display-capture.ts';
import { CAPTURE_GESTURE_MS, Gestures } from '../../main/gestures.ts';
import { guardSession } from '../../main/guards.ts';
import { GDBUS_PATHS, PORTAL_ARGS, Portal, sourceTypes, underWayland, type Env, type PortalDeps } from '../../main/portal.ts';

class FakeContents extends EventEmitter {
  mainFrame = { frameToken: 'main-1' };
  crashed = 0;
  closed = 0;
  destroyed = false;
  isDestroyed(): boolean {
    return this.destroyed;
  }
  forcefullyCrashRenderer(): void {
    this.crashed += 1;
  }
  close(): void {
    this.closed += 1;
  }
}

const screen: CaptureSource = { id: 'screen:0:0', name: 'Screen 1' };
const other: CaptureSource = { id: 'window:7:0', name: 'Window' };

interface World {
  clock: { now: number };
  win: FakeContents;
  stranger: FakeContents;
  live: { on: boolean };
  portal: { answer: 'available' | 'no-portal' | 'not-wayland' | 'unknown' };
  current: { token: string };
  sources: { list: CaptureSource[] | Error; calls: number; release?: () => void };
  logs: string[];
  capture: DisplayCapture;
  click: (c: FakeContents) => void;
  frame: (over?: Partial<CaptureFrame>) => CaptureFrame;
  permission: (contents: unknown, details?: Record<string, unknown>) => { granted: boolean; handled: boolean };
}

function world(): World {
  const clock = { now: 10_000 };
  const gestures = new Gestures(() => clock.now);
  const win = new FakeContents();
  const stranger = new FakeContents();
  gestures.attach(win);
  gestures.attach(stranger);
  const live = { on: true };
  const portal: World['portal'] = { answer: 'available' };
  const current = { token: 'main-1' };
  const sources: World['sources'] = { list: [screen], calls: 0 };
  const logs: string[] = [];
  const capture = new DisplayCapture({
    owned: () => win,
    gate: () => ({ current: (f) => f.frameToken === current.token }),
    gestures,
    live: () => live.on,
    available: () => portal.answer,
    getSources: (options) => {
      expect(options).toEqual(CAPTURE_SOURCES);
      sources.calls += 1;
      return sources.list instanceof Error ? Promise.reject(sources.list) : Promise.resolve(sources.list);
    },
    log: (l) => logs.push(l),
  });
  const permission: World['permission'] = (contents, details = { isMainFrame: true, requestingUrl: 'conch-app://ui/', mediaTypes: [] }) => {
    if (!displayPermission('media', details)) return { granted: false, handled: false };
    if (!capture.permits(contents as object, details)) return { granted: false, handled: true };
    let granted = false;
    capture.allow(contents as FakeContents, (g) => {
      granted = g;
    });
    return { granted, handled: true };
  };
  return {
    clock,
    win,
    stranger,
    live,
    portal,
    current,
    sources,
    logs,
    capture,
    click: (c) => c.emit('input-event', {}, { type: 'mouseUp' }),
    frame: (over = {}) => ({ frameToken: 'main-1', url: 'conch-app://ui/', parent: null, detached: false, ...over }),
    permission,
  };
}

// handle runs a display-media request and returns what the callback got.
async function answer(w: World, over: Partial<CaptureFrame> | null, request: { video?: boolean; audio?: boolean } = {}): Promise<unknown> {
  return new Promise((resolve) => {
    w.capture.handle(
      { frame: over === null ? null : w.frame(over), videoRequested: request.video ?? true, audioRequested: request.audio ?? false },
      (s) => {
        resolve(s);
      },
    );
  });
}

describe('displayPermission', () => {
  it('is the media permission with an empty media types list, and nothing else', () => {
    expect(displayPermission('media', { mediaTypes: [] })).toBe(true);
    for (const [permission, details] of [
      ['media', { mediaTypes: ['audio'] }],
      ['media', { mediaTypes: ['video'] }],
      ['media', { mediaTypes: ['audio', 'video'] }],
      ['media', {}],
      ['media', undefined],
      ['display-capture', { mediaTypes: [] }],
      ['clipboard-sanitized-write', { mediaTypes: [] }],
    ] as const) {
      expect(displayPermission(permission, details), `${permission} ${JSON.stringify(details)}`).toBe(false);
    }
  });
});

describe('display capture is granted', () => {
  it('to the owned main frame right after a click, with a live grant and a portal: one source, video only', async () => {
    const w = world();
    w.click(w.win);
    expect(w.permission(w.win)).toEqual({ granted: true, handled: true });
    // The permission uses up nothing; the display-media request takes the act.
    expect(await answer(w, {})).toEqual({ video: screen });
    expect(w.sources.calls).toBe(1);
  });

  it('asks the permission and the request together, as Electron does, inside the callback', async () => {
    const w = world();
    w.click(w.win);
    let result: unknown;
    w.capture.permits(w.win, { isMainFrame: true, requestingUrl: 'conch-app://ui/', mediaTypes: [] });
    w.capture.allow(w.win, () => {
      w.capture.handle({ frame: w.frame(), videoRequested: true, audioRequested: false }, (s) => {
        result = s;
      });
    });
    await vi.waitFor(() => {
      expect(result).toEqual({ video: screen });
    });
    expect(w.win.crashed).toBe(0);
  });

  it('never with audio: a request for audio gets the video member only, and the ask is logged', async () => {
    const w = world();
    w.click(w.win);
    const got = await answer(w, {}, { audio: true });
    expect(got).toEqual({ video: screen });
    expect(Object.keys(got as object)).toEqual(['video']);
    expect(w.logs.join('\n')).toContain('audio was asked for and is not given');
  });
});

describe('display capture is answered with no stream when', () => {
  const cases: { name: string; arrange: (w: World) => void; frame?: Partial<CaptureFrame> | null; reason: RegExp }[] = [
    { name: 'there was no gesture', arrange: () => undefined, reason: /no gesture/ },
    {
      name: 'the gesture is older than the capture window',
      arrange: (w) => {
        w.click(w.win);
        w.clock.now += CAPTURE_GESTURE_MS + 1;
      },
      reason: /no gesture/,
    },
    {
      name: 'the click was in another window',
      arrange: (w) => {
        w.click(w.stranger);
      },
      reason: /no gesture/,
    },
    {
      name: 'the frame is a subframe',
      arrange: (w) => {
        w.click(w.win);
      },
      frame: { parent: {} },
      reason: /a subframe/,
    },
    {
      name: 'the frame is another window’s',
      arrange: (w) => {
        w.click(w.win);
      },
      frame: { frameToken: 'main-2' },
      reason: /not the window/,
    },
    {
      name: 'the frame is a stale document generation',
      arrange: (w) => {
        w.click(w.win);
        w.current.token = 'main-2';
      },
      reason: /a stale document/,
    },
    {
      name: 'the frame is not at the app’s origin',
      arrange: (w) => {
        w.click(w.win);
      },
      frame: { url: 'https://example.com/' },
      reason: /another origin/,
    },
    {
      name: 'the frame is detached',
      arrange: (w) => {
        w.click(w.win);
      },
      frame: { detached: true },
      reason: /not the window/,
    },
    {
      name: 'there is no frame',
      arrange: (w) => {
        w.click(w.win);
      },
      frame: null,
      reason: /not the window/,
    },
    {
      name: 'there is no live companion grant',
      arrange: (w) => {
        w.click(w.win);
        w.live.on = false;
      },
      reason: /no grant/,
    },
    {
      name: 'the portal is unavailable',
      arrange: (w) => {
        w.click(w.win);
        w.portal.answer = 'no-portal';
      },
      reason: /no screen-cast portal/,
    },
    {
      name: 'the portal has not been asked yet',
      arrange: (w) => {
        w.click(w.win);
        w.portal.answer = 'unknown';
      },
      reason: /no screen-cast portal/,
    },
    {
      name: 'the picker returns no source (cancelled or denied)',
      arrange: (w) => {
        w.click(w.win);
        w.sources.list = [];
      },
      reason: /cancelled or denied/,
    },
    {
      name: 'the picker rejects (cancelled)',
      arrange: (w) => {
        w.click(w.win);
        w.sources.list = new Error('cancelled');
      },
      reason: /cancelled or denied/,
    },
    {
      name: 'there are two sources, never the first',
      arrange: (w) => {
        w.click(w.win);
        w.sources.list = [screen, other];
      },
      reason: /more than one source/,
    },
  ];

  it.each(cases)('$name', async ({ arrange, frame, reason }) => {
    const w = world();
    arrange(w);
    expect(await answer(w, frame === undefined ? {} : frame)).toBeNull();
    expect(w.logs.join('\n')).toMatch(reason);
    // Refused for a reason before the picker: no source list was even asked.
    if (!/cancelled|more than one/.test(reason.source)) expect(w.sources.calls).toBe(0);
  });

  it('video is not asked for', async () => {
    const w = world();
    w.click(w.win);
    expect(await answer(w, {}, { video: false })).toBeNull();
    expect(w.sources.calls).toBe(0);
  });

  it('the act is used up by the request, granted or not: a second request needs a new click', async () => {
    const w = world();
    w.click(w.win);
    expect(await answer(w, {})).toEqual({ video: screen });
    expect(await answer(w, {})).toBeNull();
    w.sources.list = [];
    w.click(w.win);
    expect(await answer(w, {})).toBeNull();
    expect(await answer(w, {})).toBeNull();
  });

  it('the grant ends while the picker is open', async () => {
    const w = world();
    w.click(w.win);
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const gestures = new Gestures(() => w.clock.now);
    gestures.attach(w.win);
    const capture = new DisplayCapture({
      owned: () => w.win,
      gate: () => ({ current: () => true }),
      gestures,
      live: () => w.live.on,
      available: () => 'available',
      getSources: async () => {
        await held;
        return [screen];
      },
      log: (l) => w.logs.push(l),
    });
    w.click(w.win);
    const got = new Promise<unknown>((resolve) => {
      capture.handle({ frame: w.frame(), videoRequested: true, audioRequested: false }, resolve);
    });
    w.live.on = false;
    release();
    expect(await got).toBeNull();
    expect(w.logs.join('\n')).toMatch(/ended during the pick/);
  });

  it('a second pick while one is open is refused', async () => {
    const w = world();
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const gestures = new Gestures(() => w.clock.now);
    gestures.attach(w.win);
    const capture = new DisplayCapture({
      owned: () => w.win,
      gate: () => ({ current: () => true }),
      gestures,
      live: () => true,
      available: () => 'available',
      getSources: async () => {
        await held;
        return [screen];
      },
      log: (l) => w.logs.push(l),
    });
    const ask = (): Promise<unknown> =>
      new Promise((resolve) => {
        w.click(w.win);
        capture.handle({ frame: w.frame(), videoRequested: true, audioRequested: false }, resolve);
      });
    const first = ask();
    expect(await ask()).toBeNull();
    release();
    expect(await first).toEqual({ video: screen });
  });
});

describe('the display-capture permission', () => {
  const cases: { name: string; arrange: (w: World) => void; contents?: 'stranger'; details?: Record<string, unknown> }[] = [
    { name: 'no gesture', arrange: () => undefined },
    { name: 'another window', arrange: (w) => {
        w.click(w.stranger);
      }, contents: 'stranger' },
    {
      name: 'a subframe',
      arrange: (w) => {
        w.click(w.win);
      },
      details: { isMainFrame: false, requestingUrl: 'conch-app://ui/', mediaTypes: [] },
    },
    {
      name: 'another origin',
      arrange: (w) => {
        w.click(w.win);
      },
      details: { isMainFrame: true, requestingUrl: 'https://example.com/', mediaTypes: [] },
    },
    {
      name: 'no live grant',
      arrange: (w) => {
        w.click(w.win);
        w.live.on = false;
      },
    },
    {
      name: 'no portal',
      arrange: (w) => {
        w.click(w.win);
        w.portal.answer = 'no-portal';
      },
    },
  ];

  it.each(cases)('is denied: $name', ({ arrange, contents, details }) => {
    const w = world();
    arrange(w);
    expect(w.permission(contents === 'stranger' ? w.stranger : w.win, details)).toEqual({ granted: false, handled: true });
    expect(w.win.crashed).toBe(0);
  });

  it('given and then not followed by a display-media request (the legacy desktop getUserMedia) ends the renderer', () => {
    const w = world();
    w.click(w.win);
    expect(w.permission(w.win)).toEqual({ granted: true, handled: true });
    expect(w.win.crashed).toBe(1);
    expect(w.logs.join('\n')).toMatch(/without a picker/);
  });

  describe('when the crash does not take (S5 of #1621)', () => {
    const legacy = (): World => {
      vi.useFakeTimers();
      const w = world();
      w.click(w.win);
      w.permission(w.win);
      return w;
    };
    afterEach(() => {
      vi.useRealTimers();
    });

    it('closes the page if the renderer is still there after the grace', () => {
      const w = legacy();
      expect(w.win.crashed).toBe(1);
      vi.advanceTimersByTime(CRASH_GRACE_MS - 1);
      expect(w.win.closed).toBe(0);
      vi.advanceTimersByTime(1);
      expect(w.win.closed).toBe(1);
      expect(w.logs.join('\n')).toMatch(/outlived the crash/);
    });

    it('leaves the page alone when the renderer went', () => {
      const w = legacy();
      w.win.emit('render-process-gone');
      vi.advanceTimersByTime(CRASH_GRACE_MS * 2);
      expect(w.win.closed).toBe(0);
    });

    it('leaves the page alone when the contents were destroyed meanwhile', () => {
      const w = legacy();
      w.win.destroyed = true;
      vi.advanceTimersByTime(CRASH_GRACE_MS * 2);
      expect(w.win.closed).toBe(0);
    });
  });

  it('given and followed by the request does not end the renderer', async () => {
    const w = world();
    w.click(w.win);
    let result: unknown;
    w.capture.allow(w.win, () => {
      w.capture.handle({ frame: w.frame(), videoRequested: true, audioRequested: false }, (s) => {
        result = s;
      });
    });
    expect(w.win.crashed).toBe(0);
    await vi.waitFor(() => {
      expect(result).toEqual({ video: screen });
    });
  });
});

// The session handlers, with the capture rules wired in (guards.ts).
describe('guardSession with display capture', () => {
  class FakeSession extends EventEmitter {
    request?: (c: unknown, p: string, cb: (ok: boolean) => void, d: unknown) => void;
    check?: (...a: unknown[]) => boolean;
    media?: (r: unknown, cb: (s: unknown) => void) => void;
    webRequest = { onBeforeRequest: (): void => undefined };
    setPermissionRequestHandler(h: FakeSession['request']): void {
      this.request = h;
    }
    setPermissionCheckHandler(h: FakeSession['check']): void {
      this.check = h;
    }
    setDevicePermissionHandler(): void {}
    setDisplayMediaRequestHandler(h: FakeSession['media']): void {
      this.media = h;
    }
    setSpellCheckerEnabled(): void {}
  }

  function wired() {
    const w = world();
    const ses = new FakeSession();
    guardSession(ses as unknown as Session, { log: () => undefined, capture: w.capture, owned: () => w.win });
    const here = { isMainFrame: true, requestingUrl: 'conch-app://ui/' };
    const permission = (contents: unknown, name: string, details: unknown): boolean => {
      const cb = vi.fn();
      ses.request?.(contents, name, cb, details);
      return cb.mock.calls[0]?.[0] as boolean;
    };
    return { w, ses, here, permission };
  }

  it('grants the display permission and the one source, as video only', async () => {
    const { w, ses, here } = wired();
    w.click(w.win);
    let got: unknown;
    const cb = vi.fn();
    ses.request?.(w.win, 'media', (ok) => {
      cb(ok);
      ses.media?.({ frame: w.frame(), videoRequested: true, audioRequested: true }, (s) => {
        got = s;
      });
    }, { ...here, mediaTypes: [] });
    expect(cb).toHaveBeenCalledExactlyOnceWith(true);
    await vi.waitFor(() => {
      expect(got).toEqual({ video: screen });
    });
    expect(Object.keys(got as object)).toEqual(['video']);
  });

  it('keeps the microphone and camera denied while a grant is live and the person has just clicked', () => {
    const { w, here, permission } = wired();
    w.click(w.win);
    for (const mediaTypes of [['audio'], ['video'], ['audio', 'video']]) {
      expect(permission(w.win, 'media', { ...here, mediaTypes }), mediaTypes.join()).toBe(false);
    }
    expect(permission(w.win, 'media', here)).toBe(false);
    expect(permission(w.win, 'display-capture', { ...here, mediaTypes: [] })).toBe(false);
  });

  it('answers no permission check yes, even for media', () => {
    const { w, ses } = wired();
    w.click(w.win);
    expect(ses.check?.(w.win, 'media', 'conch-app://ui', { mediaType: 'audio' })).toBe(false);
    expect(ses.check?.(w.win, 'media', 'conch-app://ui', { mediaType: 'unknown' })).toBe(false);
    expect(ses.check?.(w.win, 'display-capture', 'conch-app://ui', {})).toBe(false);
  });

  it('answers a display-media request with no stream when the grant is gone', () => {
    const { w, ses } = wired();
    w.click(w.win);
    w.live.on = false;
    const cb = vi.fn();
    ses.media?.({ frame: w.frame(), videoRequested: true, audioRequested: false }, cb);
    expect(cb).toHaveBeenCalledExactlyOnceWith(null);
  });
});

// ------------------------------------------------------------------- portal

describe('sourceTypes', () => {
  it('reads gdbus’s printing of a uint32 and nothing else', () => {
    expect(sourceTypes('(<uint32 3>,)\n')).toBe(3);
    expect(sourceTypes('(<uint32 1>,)')).toBe(1);
    for (const bad of ['', '(<uint32 >,)', '(<int32 3>,)', "(<uint32 3>,)\n(<uint32 3>,)", 'Error: GDBus.Error:org.freedesktop.DBus.Error.ServiceUnknown', '(<uint32 99999999999>,)']) {
      expect(sourceTypes(bad), bad).toBeNull();
    }
  });
});

describe('underWayland', () => {
  it('is Chromium’s test: a wayland session type and a Wayland display', () => {
    expect(underWayland({ XDG_SESSION_TYPE: 'wayland', WAYLAND_DISPLAY: 'wayland-0' })).toBe(true);
    expect(underWayland({ XDG_SESSION_TYPE: 'wayland' })).toBe(false);
    expect(underWayland({ XDG_SESSION_TYPE: 'x11', WAYLAND_DISPLAY: 'wayland-0' })).toBe(false);
    expect(underWayland({})).toBe(false);
  });
});

describe('Portal', () => {
  const wayland: Env = { XDG_SESSION_TYPE: 'wayland', WAYLAND_DISPLAY: 'wayland-0', DBUS_SESSION_BUS_ADDRESS: 'unix:path=/run/user/1000/bus', XDG_RUNTIME_DIR: '/run/user/1000', SECRET_TOKEN: 'x', HOME: '/home/a' };

  function portal(over: Partial<PortalDeps> & { env?: Env } = {}) {
    const logs: string[] = [];
    const runs: { file: string; args: readonly string[]; env: Readonly<Record<string, string>> }[] = [];
    const p = new Portal({
      platform: 'linux',
      env: wayland,
      run: (file, args, env) => {
        runs.push({ file, args, env });
        return Promise.resolve({ code: 0, stdout: '(<uint32 3>,)\n' });
      },
      executable: (path) => path === GDBUS_PATHS[0],
      log: (l) => logs.push(l),
      ...over,
    });
    return { p, logs, runs };
  }

  it('is unknown until asked, and available when the portal offers a monitor or window source', async () => {
    const { p, runs } = portal();
    expect(p.last()).toBe('unknown');
    expect(await p.probe()).toBe('available');
    expect(p.last()).toBe('available');
    expect(runs).toHaveLength(1);
  });

  it('runs one fixed read-only call, by an absolute path, with a minimal environment', async () => {
    const { p, runs } = portal();
    await p.probe();
    expect(runs[0]?.file).toBe(GDBUS_PATHS[0]);
    expect(runs[0]?.args).toEqual(PORTAL_ARGS);
    expect(runs[0]?.args).toContain('org.freedesktop.DBus.Properties.Get');
    expect(runs[0]?.env).toEqual({ LC_ALL: 'C', DBUS_SESSION_BUS_ADDRESS: 'unix:path=/run/user/1000/bus', XDG_RUNTIME_DIR: '/run/user/1000' });
  });

  const unavailable: { name: string; over: Partial<PortalDeps> & { env?: Env }; expected: string }[] = [
    { name: 'not Linux', over: { platform: 'darwin' }, expected: 'no-portal' },
    { name: 'X11, where Chromium would list every source with no picker', over: { env: { XDG_SESSION_TYPE: 'x11', DISPLAY: ':0' } }, expected: 'not-wayland' },
    { name: 'no gdbus', over: { executable: () => false }, expected: 'no-portal' },
    { name: 'the call fails', over: { run: () => Promise.resolve({ code: 1, stdout: '' }) }, expected: 'no-portal' },
    { name: 'the call cannot be run', over: { run: () => Promise.resolve(null) }, expected: 'no-portal' },
    { name: 'the call throws', over: { run: () => Promise.reject(new Error('boom')) }, expected: 'no-portal' },
    { name: 'the answer is not a uint32', over: { run: () => Promise.resolve({ code: 0, stdout: 'hello' }) }, expected: 'no-portal' },
    { name: 'the portal offers no monitor or window source', over: { run: () => Promise.resolve({ code: 0, stdout: '(<uint32 4>,)\n' }) }, expected: 'no-portal' },
  ];

  it.each(unavailable)('is unavailable when $name', async ({ over, expected }) => {
    const { p, logs } = portal(over);
    expect(await p.probe()).toBe(expected);
    expect(p.last()).toBe(expected);
    expect(logs.join('\n')).toContain('share unavailable: no screen-cast portal');
  });

  it('asks again with every probe: a portal started since is found, and a change is one log line', async () => {
    let out = 'nope';
    const { p, logs } = portal({ run: () => Promise.resolve({ code: 0, stdout: out }) });
    expect(await p.probe()).toBe('no-portal');
    expect(await p.probe()).toBe('no-portal');
    out = '(<uint32 2>,)\n';
    expect(await p.probe()).toBe('available');
    expect(logs).toHaveLength(2);
  });
});

describe('captureVerdict (S4 of #1621)', () => {
  const run = (packaged: boolean, switches: Record<string, string>): { verdict: string; logs: string[] } => {
    const logs: string[] = [];
    return { verdict: captureVerdict(packaged, (n) => switches[n] ?? '', (l) => logs.push(l)), logs };
  };

  it.each([
    'WebRTCPipeWireCapturer',
    'Other,WebRTCPipeWireCapturer',
    'WebRTCPipeWireCapturer<Trial',
    'WebRTCPipeWireCapturer:k/v',
    ' Other , WebRTCPipeWireCapturer ',
  ])('refuses a packaged launch with --disable-features=%s', (value) => {
    const { verdict, logs } = run(true, { 'disable-features': value });
    expect(verdict).toBe('refused');
    expect(logs).toHaveLength(1);
  });

  it('refuses --enable-features=-WebRTCPipeWireCapturer, which turns it off too', () => {
    expect(run(true, { 'enable-features': 'A,-WebRTCPipeWireCapturer' }).verdict).toBe('refused');
  });

  it.each<Record<string, string>>([{}, { 'disable-features': 'Other' }, { 'disable-features': 'WebRTCPipeWireCapturerX' }, { 'enable-features': 'WebRTCPipeWireCapturer' }])('lets %j through', (switches) => {
    expect(run(true, switches)).toEqual({ verdict: 'ok', logs: [] });
  });

  it('leaves an unpackaged app alone', () => {
    expect(run(false, { 'disable-features': 'WebRTCPipeWireCapturer' }).verdict).toBe('ok');
  });
});
