import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { NotificationSettings } from './NotificationSettings';
import { platform } from './platform';
import { fromBase64url, push as browserPush, toBase64url, workerScope } from './platform/browser/push';
import type { PushService } from './platform/types';
import { usePush, type PushFailure, type PushState } from './usePush';

// Background push (issue #1826): the worker's handlers, the browser's
// subscription, the settings state machine and the sequence that registers a
// subscription and mirrors the policy, with a rollback for each step.

const workerSource = readFileSync('public/sw.js', 'utf8');

type Handler = (event: unknown) => void;

// loadWorker runs public/sw.js against a stand-in for the worker's global and
// hands back what it registered.
function loadWorker(scope = 'https://conch.example/app/') {
  const handlers = new Map<string, Handler>();
  const shown: { title: string; options: NotificationOptions }[] = [];
  const opened: string[] = [];
  const focused: string[] = [];
  const windows: { url: string; focus: () => Promise<void> }[] = [];
  const self = {
    addEventListener: (type: string, handler: Handler) => handlers.set(type, handler),
    skipWaiting: vi.fn(() => Promise.resolve()),
    clients: {
      claim: vi.fn(() => Promise.resolve()),
      matchAll: vi.fn(() => Promise.resolve(windows)),
      openWindow: vi.fn((url: string) => {
        opened.push(url);
        return Promise.resolve(null);
      }),
    },
    registration: {
      scope,
      showNotification: vi.fn((title: string, options: NotificationOptions) => {
        shown.push({ title, options });
        return Promise.resolve();
      }),
    },
  };
  // eslint-disable-next-line @typescript-eslint/no-implied-eval -- the worker is a script, not a module: it is run against its own global
  (new Function('self', workerSource) as (global: unknown) => void)(self);
  const run = async (type: string, event: Record<string, unknown> = {}) => {
    const waits: Promise<unknown>[] = [];
    handlers.get(type)?.({ ...event, waitUntil: (p: Promise<unknown>) => waits.push(p) });
    await Promise.all(waits);
  };
  return { self, run, shown, opened, focused, windows, handlers };
}

describe('the service worker', () => {
  it('handles install, activate, push and a click, and nothing else', () => {
    const worker = loadWorker();
    expect([...worker.handlers.keys()].sort()).toEqual(['activate', 'install', 'notificationclick', 'push']);
  });

  it('shows exactly one fixed notification for a push, whatever the push carries', async () => {
    const worker = loadWorker();
    await worker.run('push', { data: { text: () => 'SECRET message text from #private', json: () => ({ channel: 'private' }) } });
    await worker.run('push');
    expect(worker.shown).toHaveLength(2);
    for (const notification of worker.shown) {
      expect(notification.title).toBe('New activity in Conch');
      expect(notification.options).toEqual({ body: 'Open Conch to see what you missed.', tag: 'conch-wake', renotify: true });
    }
    expect(JSON.stringify(worker.shown)).not.toMatch(/SECRET|private/);
  });

  it('shows one notification even when the push has no payload at all', async () => {
    const worker = loadWorker();
    await worker.run('push', { data: null });
    expect(worker.shown).toHaveLength(1);
  });

  it('takes over at once', async () => {
    const worker = loadWorker();
    await worker.run('install');
    await worker.run('activate');
    expect(worker.self.skipWaiting).toHaveBeenCalled();
    expect(worker.self.clients.claim).toHaveBeenCalled();
  });

  it('closes the notification and focuses an open client of its scope', async () => {
    const worker = loadWorker();
    const focus = vi.fn(() => Promise.resolve());
    worker.windows.push({ url: 'https://conch.example/other', focus: vi.fn(() => Promise.resolve()) }, { url: 'https://conch.example/app/#general', focus });
    const close = vi.fn();
    await worker.run('notificationclick', { notification: { close } });
    expect(close).toHaveBeenCalled();
    expect(focus).toHaveBeenCalledTimes(1);
    expect(worker.opened).toEqual([]);
  });

  it('opens the client when no window of its scope is open', async () => {
    const worker = loadWorker();
    worker.windows.push({ url: 'https://conch.example/elsewhere', focus: vi.fn(() => Promise.resolve()) });
    await worker.run('notificationclick', { notification: { close: vi.fn() } });
    expect(worker.opened).toEqual(['https://conch.example/app/']);
  });

  it('holds no credential and makes no request', () => {
    expect(workerSource).not.toMatch(/\bfetch\s*\(|XMLHttpRequest|WebSocket|localStorage|sessionStorage|indexedDB|Authorization|\btoken\b|importScripts|event\.data|\bcaches\b|BroadcastChannel|postMessage|MessageChannel/);
    // A scan that cannot see these would pass a worker that read the push.
    for (const sample of ['event.data.text()', 'caches.open(x)', 'new BroadcastChannel(x)', 'client.postMessage(x)']) {
      expect(sample).toMatch(/event\.data|\bcaches\b|BroadcastChannel|postMessage/);
    }
  });
});

describe('the browser’s subscription', () => {
  it('encodes keys as base64url without padding, and back', () => {
    const bytes = new Uint8Array([251, 255, 190, 0, 1]);
    const text = toBase64url(bytes.buffer);
    expect(text).toBe('-_--AAE');
    expect([...fromBase64url(text)]).toEqual([...bytes]);
    expect([...fromBase64url('-_--AAE=')]).toEqual([...bytes]);
    expect(toBase64url(null)).toBe('');
  });

  function fakeBrowser(held: { endpoint: string; key: Uint8Array | null } | null = null) {
    const keyOf = (bytes: number[]) => new Uint8Array(bytes).buffer;
    const state = { subscription: held, unsubscribed: 0 };
    const make = (endpoint: string, key: Uint8Array | null) => ({
      endpoint,
      options: { applicationServerKey: key === null ? null : key.buffer },
      getKey: (name: string) => (name === 'p256dh' ? keyOf([4, 5, 6]) : keyOf([7, 8])),
      unsubscribe: vi.fn(() => {
        state.subscription = null;
        state.unsubscribed++;
        return Promise.resolve(true);
      }),
    });
    const pushManager = {
      getSubscription: vi.fn(() => Promise.resolve(state.subscription === null ? null : make(state.subscription.endpoint, state.subscription.key))),
      subscribe: vi.fn((options: { userVisibleOnly: boolean; applicationServerKey: Uint8Array }) => {
        state.subscription = { endpoint: 'https://push.example/new', key: options.applicationServerKey };
        return Promise.resolve(make('https://push.example/new', options.applicationServerKey));
      }),
    };
    const serviceWorker = {
      register: vi.fn(() => Promise.resolve({})),
      ready: Promise.resolve({ pushManager }),
      getRegistration: vi.fn(() => Promise.resolve({ pushManager })),
    };
    vi.stubGlobal('navigator', { serviceWorker });
    vi.stubGlobal('PushManager', function PushManager() {
      return undefined;
    });
    vi.stubGlobal('Notification', function Notification() {
      return undefined;
    });
    return { state, pushManager, serviceWorker };
  }

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('registers the worker in the client’s scope (/app/ in a build) and subscribes with the server’s key, userVisibleOnly', async () => {
    const browser = fakeBrowser();
    const target = await browserPush.subscribe('AQID');
    // The scope is vite.config's base; push.ts writes it out (see there).
    expect(readFileSync('vite.config.ts', 'utf8')).toContain(`base: '${workerScope}'`);
    expect(browser.serviceWorker.register).toHaveBeenCalledWith('/app/sw.js', { scope: '/app/', updateViaCache: 'none' });
    expect(browser.pushManager.subscribe).toHaveBeenCalledWith({ userVisibleOnly: true, applicationServerKey: new Uint8Array([1, 2, 3]) });
    expect(target).toEqual({ endpoint: 'https://push.example/new', p256dh: 'BAUG', auth: 'Bwg' });
  });

  it('keeps a subscription made with the same key and replaces one made with another', async () => {
    const same = fakeBrowser({ endpoint: 'https://push.example/old', key: new Uint8Array([1, 2, 3]) });
    expect((await browserPush.subscribe('AQID')).endpoint).toBe('https://push.example/old');
    expect(same.pushManager.subscribe).not.toHaveBeenCalled();
    const other = fakeBrowser({ endpoint: 'https://push.example/old', key: new Uint8Array([9, 9, 9]) });
    expect((await browserPush.subscribe('AQID')).endpoint).toBe('https://push.example/new');
    expect(other.state.unsubscribed).toBe(1);
  });

  it('says whether it holds a subscription, and drops it', async () => {
    const browser = fakeBrowser({ endpoint: 'https://push.example/old', key: null });
    expect(await browserPush.subscribed()).toBe(true);
    await browserPush.unsubscribe();
    expect(browser.state.unsubscribed).toBe(1);
    expect(await browserPush.subscribed()).toBe(false);
  });

  it('is unsupported without service workers, push or notifications', () => {
    vi.stubGlobal('navigator', {});
    expect(browserPush.supported()).toBe(false);
    fakeBrowser();
    expect(browserPush.supported()).toBe(true);
    vi.stubGlobal('PushManager', undefined);
    expect(browserPush.supported()).toBe(false);
  });
});

// A conchd that records every request, answering from a table.
interface Request {
  readonly method: string;
  readonly path: string;
  readonly body: unknown;
}

const tokenKey = 'conch.web.token';
const registrationKey = `conch.web.push.v1.${window.location.origin}`;
const record = { id: 'reg-1', account: 2 };
const member = { id: 2, kind: 'human', name: 'ada', role: 'member' };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
const registration = (id = 'reg-1') => ({
  id,
  transport: 'webpush',
  platform: 'web',
  app: window.location.origin,
  target_fingerprint: '0'.repeat(16),
  created_at: '2026-11-13T00:00:00Z',
});

interface Server {
  readonly requests: Request[];
  answer: (request: Request) => Response | Promise<Response>;
}

function serve(): Server {
  const server: Server = {
    requests: [],
    answer: (request) => {
      switch (`${request.method} ${request.path}`) {
        case 'GET /v1/whoami':
          return json(member);
        case 'GET /v1/push/config':
          return json({ transports: ['webpush'], vapid_public_key: 'AQID' });
        case 'GET /v1/push/registrations/mine':
          return json({ registrations: [] });
        case 'POST /v1/push/registrations':
          return json(registration(), 201);
        case 'GET /v1/notifications/preferences':
        case 'PUT /v1/notifications/preferences':
          return json({ policy: 'off', muted: [], preview: false, quiet_when_unreachable: false });
        case 'DELETE /v1/push/registrations/reg-1':
        case 'DELETE /v1/credentials/current':
          return new Response(null, { status: 204 });
        default:
          return json({ code: 'not_found', message: 'no' }, 404);
      }
    },
  };
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
      const body: unknown = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      const request = { method: init?.method ?? 'GET', path, body };
      server.requests.push(request);
      return Promise.resolve(server.answer(request));
    }),
  );
  return server;
}

function fakePush(overrides: Partial<PushService> = {}, held = false): PushService & { readonly calls: string[] } {
  const calls: string[] = [];
  let subscribed = held;
  return {
    calls,
    supported: () => true,
    subscribe: vi.fn((key: string) => {
      calls.push(`subscribe ${key}`);
      subscribed = true;
      return Promise.resolve({ endpoint: 'https://push.example/e', p256dh: 'BAUG', auth: 'Bwg' });
    }),
    subscribed: vi.fn(() => Promise.resolve(subscribed)),
    unsubscribe: vi.fn(() => {
      calls.push('unsubscribe');
      subscribed = false;
      return Promise.resolve();
    }),
    ...overrides,
  };
}

let permission: NotificationPermission = 'granted';
let answer: NotificationPermission = 'granted';
const original = platform.services.push;

beforeEach(async () => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  permission = 'granted';
  answer = 'granted';
  const requestPermission = vi.fn(() => {
    permission = answer;
    return Promise.resolve(answer);
  });
  const fakeNotification = Object.defineProperty(Object.assign(function Notification() {
    return undefined;
  }, { requestPermission }), 'permission', { get: () => permission });
  vi.stubGlobal('Notification', fakeNotification);
  window.localStorage.setItem(tokenKey, 'conch_member_secret');
  serve();
  expect((await api.restore()).ok).toBe(true);
});

afterEach(() => {
  cleanup();
  platform.services.push = original;
  vi.unstubAllGlobals();
});

interface Props {
  readonly signedIn: boolean;
  readonly eligible: boolean;
  readonly account: number;
  readonly policy: 'off' | 'mentions' | 'all';
  readonly muted: ReadonlySet<number>;
}
// usePushStored is usePush as the page runs it: useNotifications writes the
// local preferences to storage before usePush's effects run, and the pump sends
// what is stored.
const usePushStored = (p: Props) => {
  api.writeNotificationPreferences(window.location.origin, p.account, { policy: p.policy, muted: [...p.muted], native: false });
  return usePush(p);
};
const props = (over: Partial<Props> = {}): Props => ({ signedIn: true, eligible: true, account: 2, policy: 'mentions', muted: new Set([7]), ...over });

async function settled(server: Server, hook: { result: { current: ReturnType<typeof usePush> } }, kind: PushState['kind']) {
  await waitFor(() => {
    expect(hook.result.current.state.kind).toBe(kind);
  });
  return server;
}

describe('the settings state', () => {
  it('offers nothing to a guest or an agent, or before sign-in', () => {
    platform.services.push = fakePush();
    const server = serve();
    expect(renderHook(() => usePushStored(props({ eligible: false }))).result.current.state.kind).toBe('hidden');
    expect(renderHook(() => usePushStored(props({ signedIn: false }))).result.current.state.kind).toBe('hidden');
    expect(server.requests).toEqual([]);
  });

  it('is unsupported where the browser has no push, without asking the server', () => {
    platform.services.push = fakePush({ supported: () => false });
    const server = serve();
    expect(renderHook(() => usePushStored(props())).result.current.state.kind).toBe('unsupported');
    platform.services.push = undefined;
    expect(renderHook(() => usePushStored(props())).result.current.state.kind).toBe('unsupported');
    expect(server.requests).toEqual([]);
  });

  it.each([
    { name: 'no webpush transport', config: () => json({ transports: [] }) },
    { name: 'no key', config: () => json({ transports: ['webpush'] }) },
    { name: 'a refusal', config: () => json({ code: 'internal_error', message: 'no' }, 500) },
  ])('is not configured with $name', async ({ config }) => {
    platform.services.push = fakePush();
    const server = serve();
    server.answer = (request) => (request.path === '/v1/push/config' ? config() : json({}, 404));
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'not-configured');
  });

  it('is off when configured and nothing is registered, and denied when the browser blocks notifications', async () => {
    platform.services.push = fakePush();
    const server = serve();
    const off = renderHook(() => usePushStored(props()));
    await settled(server, off, 'off');
    permission = 'denied';
    const denied = renderHook(() => usePushStored(props()));
    await settled(server, denied, 'denied');
  });

  it('is on when a record, a subscription and a registration the server still holds agree', async () => {
    platform.services.push = fakePush({}, true);
    const server = serve();
    api.keepPushRegistration(record);
    server.answer = ((base) => (request: Request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : base(request)))(server.answer);
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'on');
  });

  it('reads a record the server no longer holds as off, and clears it', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    api.keepPushRegistration(record);
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    expect(api.readPushRegistration()).toBeNull();
    expect(fake.calls).toEqual(['unsubscribe']);
  });
});

describe('turning it on', () => {
  async function ready(over: Partial<Props> = {}) {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook((p: Props) => usePushStored(p), { initialProps: props(over) });
    await settled(server, hook, 'off');
    server.requests.length = 0;
    return { fake, server, hook };
  }

  it('asks permission, subscribes with the server’s key, mirrors the policy and mutes, then registers as a web subscription of this origin', async () => {
    permission = 'default';
    const { fake, server, hook } = await ready();
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current.state.kind).toBe('on');
    expect(fake.calls).toEqual(['subscribe AQID']);
    // The policy first: preferences with no registration wake nothing.
    expect(server.requests).toEqual([
      { method: 'PUT', path: '/v1/notifications/preferences', body: { policy: 'mentions', muted: [7], preview: false, quiet_when_unreachable: false } },
      {
        method: 'POST',
        path: '/v1/push/registrations',
        body: { transport: 'webpush', platform: 'web', app: window.location.origin, endpoint: 'https://push.example/e', p256dh: 'BAUG', auth: 'Bwg' },
      },
    ]);
    expect(api.readPushRegistration()).toEqual(record);
    expect(window.localStorage.getItem(registrationKey)).toBe(JSON.stringify(record));
  });

  it('leaves nothing registered when permission is refused, and says which step', async () => {
    permission = 'default';
    answer = 'denied';
    const { fake, server, hook } = await ready();
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current.state.kind).toBe('denied');
    expect(hook.result.current.failure).toBe('permission');
    expect(fake.calls).toEqual([]);
    expect(server.requests).toEqual([]);
  });

  it('leaves nothing registered when the browser will not subscribe', async () => {
    const { fake, server, hook } = await ready();
    platform.services.push = { ...fake, subscribe: vi.fn(() => Promise.reject(new Error('refused'))) };
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current).toMatchObject({ state: { kind: 'off' }, failure: 'subscribe' });
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(server.requests).toEqual([]);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('drops the subscription when the server refuses the registration', async () => {
    const { fake, server, hook } = await ready();
    const base = server.answer;
    server.answer = (request) => (request.method === 'POST' ? json({ code: 'push_endpoint_refused', message: 'no' }, 400) : base(request));
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current).toMatchObject({ state: { kind: 'off' }, failure: 'register' });
    expect(fake.calls).toEqual(['subscribe AQID', 'unsubscribe']);
    expect(server.requests.map((request) => request.method)).toEqual(['PUT', 'POST']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('registers nothing and drops the subscription when the policy cannot be mirrored', async () => {
    const { fake, server, hook } = await ready();
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current).toMatchObject({ state: { kind: 'off' }, failure: 'preferences' });
    expect(fake.calls).toEqual(['subscribe AQID', 'unsubscribe']);
    expect(server.requests.map((request) => `${request.method} ${request.path}`)).toEqual(['PUT /v1/notifications/preferences']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('copies a later change of policy or mutes to the server while on, and only then', async () => {
    const { server, hook } = await ready({ policy: 'off', muted: new Set() });
    hook.rerender(props({ policy: 'all', muted: new Set() }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(server.requests).toEqual([]);
    await act(async () => {
      await hook.result.current.enable();
    });
    server.requests.length = 0;
    hook.rerender(props({ policy: 'all', muted: new Set([9, 7]) }));
    await waitFor(() => {
      expect(server.requests).toEqual([{ method: 'PUT', path: '/v1/notifications/preferences', body: { policy: 'all', muted: [9, 7], preview: false, quiet_when_unreachable: false } }]);
    });
    // The same values again send nothing more.
    hook.rerender(props({ policy: 'all', muted: new Set([9, 7]) }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(server.requests).toHaveLength(1);
  });
});

describe('turning it off', () => {
  async function on() {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    await act(async () => {
      await hook.result.current.enable();
    });
    fake.calls.length = 0;
    server.requests.length = 0;
    return { fake, server, hook };
  }

  it('ends the registration and drops the subscription', async () => {
    const { fake, server, hook } = await on();
    await act(async () => {
      await hook.result.current.disable();
    });
    expect(hook.result.current.state.kind).toBe('off');
    expect(server.requests).toEqual([{ method: 'DELETE', path: '/v1/push/registrations/reg-1', body: undefined }]);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('counts a registration the server already ended as ended', async () => {
    const { fake, server, hook } = await on();
    server.answer = () => json({ code: 'not_found', message: 'no' }, 404);
    await act(async () => {
      await hook.result.current.disable();
    });
    expect(hook.result.current.state.kind).toBe('off');
    expect(fake.calls).toEqual(['unsubscribe']);
  });

  it('stays on and says so when the server could not be told', async () => {
    const { fake, server, hook } = await on();
    server.answer = () => json({ code: 'internal_error', message: 'no' }, 500);
    await act(async () => {
      await hook.result.current.disable();
    });
    expect(hook.result.current).toMatchObject({ state: { kind: 'on' }, failure: 'end' });
    expect(fake.calls).toEqual([]);
    expect(api.readPushRegistration()).toEqual(record);
  });
});

describe('signing out', () => {
  function registered() {
    const fake = fakePush();
    platform.services.push = fake;
    api.keepPushRegistration(record);
    const server = serve();
    return { fake, server };
  }

  it('ends the registration and drops the subscription before the credential is revoked', async () => {
    const { fake, server } = registered();
    expect(await api.signOut(2)).toBe(true);
    expect(server.requests.map((request) => `${request.method} ${request.path}`)).toEqual(['DELETE /v1/push/registrations/reg-1', 'DELETE /v1/credentials/current']);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('signs out all the same when the registration could not be ended', async () => {
    const { fake, server } = registered();
    const base = server.answer;
    server.answer = (request) => (request.path === '/v1/push/registrations/reg-1' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    expect(await api.signOut(2)).toBe(true);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(window.localStorage.getItem(tokenKey)).toBeNull();
  });

  it('asks nothing about push of a browser that never registered', async () => {
    platform.services.push = fakePush();
    const server = serve();
    expect(await api.signOut(2)).toBe(true);
    expect(server.requests.map((request) => request.path)).toEqual(['/v1/credentials/current']);
  });

  it('drops the subscription, with no request of its own, once every credential is revoked', async () => {
    const { fake, server } = registered();
    server.answer = () => json({ revoked: 2 });
    expect(await api.signOutEverywhere(2)).toEqual({ ok: true, value: 2 });
    expect(server.requests.map((request) => request.path)).toEqual(['/v1/principals/2/credentials/revoke-all']);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('keeps push when revoking everything fails', async () => {
    const { fake, server } = registered();
    server.answer = () => json({ code: 'internal_error', message: 'no' }, 500);
    expect((await api.signOutEverywhere(2)).ok).toBe(false);
    expect(fake.calls).toEqual([]);
    expect(api.readPushRegistration()).toEqual(record);
  });
});

describe('the settings section', () => {
  const settings = (state: PushState, failure: PushFailure | null = null) => {
    const onEnable = vi.fn();
    const onDisable = vi.fn();
    render(
      <NotificationSettings policy="mentions" native={false} permission="default" supported onPolicy={vi.fn()} onEnable={vi.fn()} push={{ state, failure, onEnable, onDisable }} />,
    );
    return { onEnable, onDisable };
  };

  it.each([
    { kind: 'hidden' as const },
    { kind: 'checking' as const },
  ])('shows nothing while $kind', (state) => {
    settings(state);
    expect(screen.queryByText('When Conch is closed')).toBeNull();
  });

  it.each([
    { state: { kind: 'unsupported' as const }, line: /not available in this browser/ },
    { state: { kind: 'not-configured' as const }, line: /not turned on at this server/ },
    { state: { kind: 'denied' as const }, line: /blocked for this site/ },
  ])('explains in one line why it is not on offer: $state.kind', ({ state, line }) => {
    settings(state);
    expect(screen.getByText(line)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /when Conch is closed/ })).toBeNull();
  });

  it('turns it on and off with a button, and names the step that failed', () => {
    const off = settings({ kind: 'off' }, 'register');
    fireEvent.click(screen.getByRole('button', { name: 'Notify me when Conch is closed' }));
    expect(off.onEnable).toHaveBeenCalled();
    expect(screen.getByRole('alert').textContent).toMatch(/did not accept this browser/);
    cleanup();
    const on = settings({ kind: 'on' });
    fireEvent.click(screen.getByRole('button', { name: 'Stop notifying me when Conch is closed' }));
    expect(on.onDisable).toHaveBeenCalled();
  });
});

// The review of #1846: wakes must not reach someone who opted out or signed
// out, so a copy that did not take is sent again, every tab keeps it in step,
// and a record is only ever its own account's.
const prefs = (policy: string, muted: number[]) => json({ policy, muted, preview: false, quiet_when_unreachable: false });
const refusedWrite = (retryAfter?: string) =>
  new Response(JSON.stringify({ code: 'rate_limited', message: 'slow down' }), { status: 429, headers: retryAfter === undefined ? {} : { 'Retry-After': retryAfter } });
const puts = (server: Server) => server.requests.filter((request) => request.method === 'PUT' && request.path === '/v1/notifications/preferences');

async function turnedOn(over: Partial<Props> = {}) {
  const fake = fakePush();
  platform.services.push = fake;
  const server = serve();
  const hook = renderHook((p: Props) => usePushStored(p), { initialProps: props(over) });
  await settled(server, hook, 'off');
  await act(async () => {
    await hook.result.current.enable();
  });
  fake.calls.length = 0;
  server.requests.length = 0;
  return { fake, server, hook };
}

describe('a copy of the policy that did not take', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('is sent again after the server’s Retry-After, and not before', async () => {
    const { server, hook } = await turnedOn();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? refusedWrite('30') : base(request));
    hook.rerender(props({ policy: 'off' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(hook.result.current.failure).toBe('mirror');
    expect(puts(server)).toHaveLength(1);
    server.answer = base;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(29_000);
    });
    expect(puts(server)).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(puts(server)).toHaveLength(2);
    expect(puts(server)[1]?.body).toMatchObject({ policy: 'off' });
    expect(hook.result.current.failure).toBeNull();
  });

  it('is sent again after a growing wait when no Retry-After was given, and the latest is what is sent', async () => {
    const { server, hook } = await turnedOn();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    hook.rerender(props({ policy: 'off' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    hook.rerender(props({ policy: 'all' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const before = puts(server).length;
    // The first wait is 1 s and the next 2 s: both have passed.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(puts(server).length).toBeGreaterThan(before);
    server.answer = base;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5000);
    });
    expect(puts(server).at(-1)?.body).toMatchObject({ policy: 'all' });
    expect(hook.result.current.failure).toBeNull();
  });

  it.each([
    { name: 'the browser comes back online', wake: () => window.dispatchEvent(new Event('online')) },
    { name: 'the tab is shown', wake: () => document.dispatchEvent(new Event('visibilitychange')) },
  ])('is sent again at once when $name, if the server gave no Retry-After, without waiting out the backoff', async ({ wake }) => {
    const { server, hook } = await turnedOn();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    hook.rerender(props({ policy: 'off' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(hook.result.current.failure).toBe('mirror');
    server.answer = base;
    await act(async () => {
      wake();
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(hook.result.current.failure).toBeNull();
    expect(puts(server).at(-1)?.body).toMatchObject({ policy: 'off' });
  });

  it('is not sent again by a wake before the server’s Retry-After', async () => {
    const { server, hook } = await turnedOn();
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? refusedWrite('60') : base(request));
    hook.rerender(props({ policy: 'off' }));
    await waitFor(() => {
      expect(hook.result.current.failure).toBe('mirror');
    });
    const sent = puts(server).length;
    await act(async () => {
      window.dispatchEvent(new Event('online'));
      await Promise.resolve();
    });
    expect(puts(server)).toHaveLength(sent);
  });

  it('is not written at load when the server’s copy already agrees, and is when it does not', async () => {
    platform.services.push = fakePush({}, true);
    const server = serve();
    api.keepPushRegistration(record);
    const base = server.answer;
    server.answer = (request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : base(request));
    const agree = renderHook(() => usePushStored(props()));
    await settled(server, agree, 'on');
    // Muted 7 and the policy are the server's own: the read matched, so no write.
    server.answer = (request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : request.method === 'GET' && request.path === '/v1/notifications/preferences' ? prefs('mentions', [7]) : base(request));
    server.requests.length = 0;
    agree.unmount();
    const same = renderHook(() => usePushStored(props()));
    await settled(server, same, 'on');
    await act(async () => {
      await Promise.resolve();
    });
    expect(server.requests.some((request) => request.path === '/v1/notifications/preferences' && request.method === 'GET')).toBe(true);
    expect(puts(server)).toEqual([]);
    same.unmount();
    server.answer = (request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : request.method === 'GET' && request.path === '/v1/notifications/preferences' ? prefs('all', []) : base(request));
    server.requests.length = 0;
    const differ = renderHook(() => usePushStored(props()));
    await settled(server, differ, 'on');
    await waitFor(() => {
      expect(puts(server)).toHaveLength(1);
    });
  });
});

describe('every tab of the origin', () => {
  it('copies a change to the server when another tab turned push on, though this tab reads off', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook((p: Props) => usePushStored(p), { initialProps: props({ policy: 'all', muted: new Set() }) });
    await settled(server, hook, 'off');
    server.requests.length = 0;
    // The other tab registers: its record is the origin's.
    api.keepPushRegistration(record);
    hook.rerender(props({ policy: 'mentions', muted: new Set() }));
    await waitFor(() => {
      expect(puts(server)).toHaveLength(1);
    });
    expect(puts(server)[0]?.body).toMatchObject({ policy: 'mentions' });
  });

  it('reads the state again when another tab turns push on or off', async () => {
    const fake = fakePush({}, true);
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    const base = server.answer;
    server.answer = (request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : base(request));
    api.keepPushRegistration(record);
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: registrationKey, storageArea: window.localStorage }));
    });
    await settled(server, hook, 'on');
    api.keepPushRegistration(null);
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: registrationKey, storageArea: window.localStorage }));
    });
    await settled(server, hook, 'off');
  });

  it('ignores a change of another key', async () => {
    platform.services.push = fakePush();
    const server = serve();
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    server.requests.length = 0;
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: 'conch.web.other', storageArea: window.localStorage }));
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(server.requests).toEqual([]);
  });

  it('reads the state again after a password change rotated the credential, and clears the registration that ended with it', async () => {
    let tell: ((event: api.ReplacingEvent) => void) | undefined;
    vi.spyOn(api, 'onCredentialReplacing').mockImplementation((listener) => {
      tell = listener;
      return () => undefined;
    });
    const { fake, server, hook } = await turnedOn();
    expect(hook.result.current.state.kind).toBe('on');
    // The server ended the old credential's registration with it.
    const base = server.answer;
    server.answer = (request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [] }) : base(request));
    act(() => {
      tell?.({ phase: 'end', id: 1, rotated: true, late: false });
    });
    await settled(server, hook, 'off');
    expect(api.readPushRegistration()).toBeNull();
    expect(fake.calls).toEqual(['unsubscribe']);
  });
});

describe('a sign-in that ends with the tab', () => {
  it('is not offered background push, and nothing about push is asked of the server', async () => {
    window.localStorage.removeItem(tokenKey);
    window.sessionStorage.setItem(tokenKey, 'conch_operator_secret');
    const server = serve();
    expect((await api.restore()).ok).toBe(true);
    platform.services.push = fakePush();
    server.requests.length = 0;
    const hook = renderHook(() => usePushStored(props()));
    await waitFor(() => {
      expect(hook.result.current.state.kind).toBe('session-only');
    });
    expect(server.requests).toEqual([]);
  });

  it('says why in one line', () => {
    render(<NotificationSettings policy="off" native={false} permission="default" supported onPolicy={vi.fn()} onEnable={vi.fn()} push={{ state: { kind: 'session-only' }, failure: null, onEnable: vi.fn(), onDisable: vi.fn() }} />);
    expect(screen.getByText(/need a sign-in that lasts/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: /when Conch is closed/ })).toBeNull();
  });
});

describe('a record that is another account’s', () => {
  const theirs = { id: 'reg-9', account: 9 };

  it('is dropped on load under a lasting credential, locally: no request is made, and this account reads off', async () => {
    const fake = fakePush({}, true);
    platform.services.push = fake;
    const server = serve();
    api.keepPushRegistration(theirs);
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    expect(api.readPushRegistration()).toBeNull();
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(server.requests.some((request) => request.method === 'DELETE')).toBe(false);
  });

  it('is not ended by turning off, and is dropped, with no request for it, by a sign-out', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    api.keepPushRegistration(theirs);
    expect(await api.endPush(2)).toBe(true);
    expect(fake.calls).toEqual([]);
    expect(api.readPushRegistration()).toEqual(theirs);
    expect(await api.signOut(2)).toBe(true);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(server.requests.map((request) => request.path)).toEqual(['/v1/credentials/current']);
  });

  it('is dropped locally by a sign-out that does not know its account, with no request for it', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    api.keepPushRegistration(record);
    expect(await api.signOut()).toBe(true);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(server.requests.map((request) => request.path)).toEqual(['/v1/credentials/current']);
  });

  it('is replaced, not shared, when this account turns push on: the subscription it holds is dropped first', async () => {
    const fake = fakePush({}, true);
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    // Another tab of this origin signed in as someone else since the load.
    api.keepPushRegistration(theirs);
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(fake.calls).toEqual(['unsubscribe', 'subscribe AQID']);
    expect(api.readPushRegistration()).toEqual(record);
  });

  it('keeps the subscription of its own record when it turns push on again', async () => {
    const fake = fakePush({}, true);
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook(() => usePushStored(props()));
    await settled(server, hook, 'off');
    api.keepPushRegistration(record);
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(fake.calls).toEqual(['subscribe AQID']);
  });
});

describe('the push request of a sign-out', () => {
  it('is bounded: a server that never answers it does not hold the sign-out, and the subscription still goes', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    api.keepPushRegistration(record);
    const requests: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
        requests.push(`${init?.method ?? 'GET'} ${path}`);
        if (path === '/v1/push/registrations/reg-1') {
          return new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener('abort', () => {
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(new Response(null, { status: 204 }));
      }),
    );
    expect(await api.signOut(2)).toBe(true);
    expect(requests).toEqual(['DELETE /v1/push/registrations/reg-1', 'DELETE /v1/credentials/current']);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(window.localStorage.getItem(tokenKey)).toBeNull();
  }, 10_000);
});

describe('signing out of a sign-in that ends with the tab', () => {
  it('leaves the member’s record and subscription alone', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    api.keepPushRegistration(record);
    window.localStorage.removeItem(tokenKey);
    window.sessionStorage.setItem(tokenKey, 'conch_operator_secret');
    const server = serve();
    expect((await api.restore()).ok).toBe(true);
    server.requests.length = 0;
    expect(await api.signOut(2)).toBe(true);
    expect(fake.calls).toEqual([]);
    expect(api.readPushRegistration()).toEqual(record);
    expect(server.requests.map((request) => request.path)).toEqual(['/v1/credentials/current']);
  });
});

describe('forgetting a sign-in that could not be checked', () => {
  it('drops the subscription and the record with the credential, with no push request', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    // A page load whose server did not answer: the credential is stored, unchecked.
    api.keepPushRegistration(record);
    const calls: string[] = [];
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
        calls.push(`${init?.method ?? 'GET'} ${path}`);
        return path === '/v1/whoami' ? Promise.reject(new TypeError('Failed to fetch')) : Promise.reject(new TypeError('Failed to fetch'));
      }),
    );
    expect((await api.restore()).ok).toBe(false);
    expect(await api.forgetStored()).toBe(false);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(calls.some((call) => call.includes('/v1/push/'))).toBe(false);
    expect(window.localStorage.getItem(tokenKey)).toBeNull();
  });

  it('leaves the member’s subscription when the credential it forgets is a tab’s', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    api.keepPushRegistration(record);
    window.localStorage.removeItem(tokenKey);
    window.sessionStorage.setItem(tokenKey, 'conch_operator_secret');
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))));
    expect((await api.restore()).ok).toBe(false);
    await api.forgetStored();
    expect(fake.calls).toEqual([]);
    expect(api.readPushRegistration()).toEqual(record);
  });
});

describe('more mutes than the server can hold', () => {
  it('keeps the newest, and the settings say the oldest are cut', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    const many = new Set(Array.from({ length: 300 }, (_, i) => i + 1));
    const hook = renderHook((p: Props) => usePushStored(p), { initialProps: props({ muted: many }) });
    await settled(server, hook, 'off');
    server.requests.length = 0;
    await act(async () => {
      await hook.result.current.enable();
    });
    const body = puts(server)[0]?.body as { muted: number[] };
    expect(body.muted).toHaveLength(256);
    expect(body.muted[0]).toBe(45);
    expect(body.muted.at(-1)).toBe(300);
    expect(hook.result.current.mutesCut).toBe(true);
    expect(api.serverMutes([1, 2, 3])).toEqual([1, 2, 3]);
    render(<NotificationSettings policy="all" native={false} permission="default" supported onPolicy={vi.fn()} onEnable={vi.fn()} push={{ state: { kind: 'on' }, failure: null, mutesCut: true, onEnable: vi.fn(), onDisable: vi.fn() }} />);
    expect(screen.getByText(/newest 256 muted channels/)).toBeTruthy();
  });
});

describe('what the pump sends', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  // Two tabs. The first changed the policy to off and mirrored it. The second
  // was loaded before that and still holds 'all' in memory: it must send what
  // is stored (off), never its own memory, or an opted-out person is woken.
  it('is the stored choice, not the tab’s older memory of it', async () => {
    platform.services.push = fakePush({}, true);
    const server = serve();
    api.keepPushRegistration(record);
    api.writeNotificationPreferences(window.location.origin, 2, { policy: 'off', muted: [], native: false });
    const base = server.answer;
    server.answer = (request) =>
      request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : request.method === 'GET' && request.path === '/v1/notifications/preferences' ? prefs('off', []) : base(request);
    // The raw hook: this tab's memory says 'all' and it wrote nothing to storage.
    const stale = renderHook(() => usePush(props({ policy: 'all', muted: new Set() })));
    await settled(server, stale, 'on');
    await act(async () => {
      await Promise.resolve();
    });
    expect(puts(server)).toEqual([]);
    // And when the server's copy is behind, what is sent is the stored choice.
    server.answer = (request) =>
      request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : request.method === 'GET' && request.path === '/v1/notifications/preferences' ? prefs('all', []) : base(request);
    stale.unmount();
    const behind = renderHook(() => usePush(props({ policy: 'all', muted: new Set() })));
    await settled(server, behind, 'on');
    await waitFor(() => {
      expect(puts(server)).toHaveLength(1);
    });
    expect(puts(server)[0]?.body).toMatchObject({ policy: 'off', muted: [] });
  });

  it('sends the stored mutes on enable, too', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook(() => usePush(props({ policy: 'all', muted: new Set([1]) })));
    await settled(server, hook, 'off');
    api.writeNotificationPreferences(window.location.origin, 2, { policy: 'mentions', muted: [4, 5], native: false });
    server.requests.length = 0;
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(puts(server)[0]?.body).toMatchObject({ policy: 'mentions', muted: [4, 5] });
  });

  it('is not sent under a credential that ends with the tab: the registration it owns is ended instead (a member made an operator)', async () => {
    // restore moved the credential out of the lasting place; the record is still the account's.
    window.localStorage.removeItem(tokenKey);
    window.sessionStorage.setItem(tokenKey, 'conch_operator_secret');
    const server = serve();
    expect((await api.restore()).ok).toBe(true);
    const fake = fakePush({}, true);
    platform.services.push = fake;
    api.keepPushRegistration(record);
    server.requests.length = 0;
    const hook = renderHook((p: Props) => usePushStored(p), { initialProps: props() });
    await waitFor(() => {
      expect(hook.result.current.state.kind).toBe('session-only');
    });
    await waitFor(() => {
      expect(api.readPushRegistration()).toBeNull();
    });
    hook.rerender(props({ policy: 'off' }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(server.requests).toEqual([{ method: 'DELETE', path: '/v1/push/registrations/reg-1', body: undefined }]);
    expect(fake.calls).toEqual(['unsubscribe']);
  });

  it('leaves another account’s record alone when the sign-in is a tab’s', async () => {
    window.localStorage.removeItem(tokenKey);
    window.sessionStorage.setItem(tokenKey, 'conch_operator_secret');
    const server = serve();
    expect((await api.restore()).ok).toBe(true);
    const fake = fakePush({}, true);
    platform.services.push = fake;
    api.keepPushRegistration({ id: 'reg-9', account: 9 });
    server.requests.length = 0;
    const hook = renderHook(() => usePushStored(props()));
    await waitFor(() => {
      expect(hook.result.current.state.kind).toBe('session-only');
    });
    expect(server.requests).toEqual([]);
    expect(api.readPushRegistration()).toEqual({ id: 'reg-9', account: 9 });
  });

  it('schedules nothing for a send that was in flight when the page went away', async () => {
    const { server, hook } = await turnedOn();
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    let answerLate: (response: Response) => void = () => undefined;
    const base = server.answer;
    server.answer = (request) =>
      request.method === 'PUT'
        ? new Promise<Response>((resolve) => {
            answerLate = resolve;
          })
        : base(request);
    hook.rerender(props({ policy: 'off' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(puts(server)).toHaveLength(1);
    hook.unmount();
    answerLate(json({ code: 'internal_error', message: 'no' }, 500));
    await vi.advanceTimersByTimeAsync(0);
    // No resend is waiting, and none is sent later.
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(300_000);
    expect(puts(server)).toHaveLength(1);
  });
});

describe('the bound on the sign-out push request, where AbortSignal.timeout is missing', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('is a controller and a timer', async () => {
    vi.stubGlobal('AbortSignal', Object.assign(function AbortSignal() {
      return undefined;
    }, { timeout: undefined }));
    const fake = fakePush();
    platform.services.push = fake;
    api.keepPushRegistration(record);
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
        const path = typeof input === 'string' ? input : input instanceof URL ? input.pathname : input.url;
        if (path === '/v1/push/registrations/reg-1') {
          return new Promise<Response>((_, reject) => {
            init?.signal?.addEventListener('abort', () => {
              reject(new DOMException('aborted', 'AbortError'));
            });
          });
        }
        return Promise.resolve(new Response(null, { status: 204 }));
      }),
    );
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const out = api.signOut(2);
    await vi.advanceTimersByTimeAsync(3100);
    expect(await out).toBe(true);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
  });
});

describe('a member made an operator', () => {
  it('has the subscription and record dropped, locally, where restore moves the credential out of the lasting place', async () => {
    const fake = fakePush({}, true);
    platform.services.push = fake;
    api.keepPushRegistration(record);
    const server = serve();
    server.answer = (request) => (request.path === '/v1/whoami' ? json({ id: 2, kind: 'human', name: 'ada', role: 'operator' }) : json({}, 404));
    const restored = await api.restore();
    expect(restored).toMatchObject({ ok: true, value: { warn: true } });
    expect(window.localStorage.getItem(tokenKey)).toBeNull();
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(server.requests.map((request) => request.path)).toEqual(['/v1/whoami']);
  });

  it('has them dropped for a visit moved to the tab as well', async () => {
    const fake = fakePush({}, true);
    platform.services.push = fake;
    api.keepPushRegistration(record);
    const server = serve();
    server.answer = (request) =>
      request.path === '/v1/whoami' ? json({ id: 2, kind: 'human', name: 'ada', role: 'member', guest: { state: 'active', origin: 'address', handle: 'g', tag: 'a7k4', started_at: '2026-10-21T10:00:00.000Z', expires_at: '2099-10-22T12:00:00.000Z', idle_expires_at: '2099-10-21T14:00:00.000Z', can_convert: true } }) : json({}, 404);
    await api.restore();
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
  });
});

describe('a record dropped while another is written', () => {
  it('is not cleared if a registration was made in the gap', async () => {
    let finish: () => void = () => undefined;
    const fake = fakePush({
      unsubscribe: vi.fn(
        () =>
          new Promise<void>((resolve) => {
            finish = resolve;
          }),
      ),
    }, true);
    platform.services.push = fake;
    api.keepPushRegistration(record);
    const dropping = api.forgetPush(2);
    api.keepPushRegistration({ id: 'reg-2', account: 2 });
    finish();
    await dropping;
    expect(api.readPushRegistration()).toEqual({ id: 'reg-2', account: 2 });
  });
});

describe('a tab that did not make the change', () => {
  it('reads the server’s copy before writing what another tab stored, and writes nothing when it agrees', async () => {
    platform.services.push = fakePush({}, true);
    const server = serve();
    api.keepPushRegistration(record);
    api.writeNotificationPreferences(window.location.origin, 2, { policy: 'mentions', muted: [7], native: false });
    const base = server.answer;
    server.answer = (request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : base(request));
    // This tab's memory says mentions and it sent that.
    const hook = renderHook(() => usePush(props({ policy: 'mentions', muted: new Set([7]) })));
    await settled(server, hook, 'on');
    await waitFor(() => {
      expect(puts(server)).toHaveLength(1);
    });
    server.requests.length = 0;
    // Another tab chose all, kept it in storage and sent it.
    api.writeNotificationPreferences(window.location.origin, 2, { policy: 'all', muted: [7], native: false });
    server.answer = (request) =>
      request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : request.method === 'GET' && request.path === '/v1/notifications/preferences' ? prefs('all', [7]) : base(request);
    act(() => {
      window.dispatchEvent(new StorageEvent('storage', { key: registrationKey, storageArea: window.localStorage }));
    });
    await waitFor(() => {
      expect(server.requests.some((request) => request.method === 'GET' && request.path === '/v1/notifications/preferences')).toBe(true);
    });
    await act(async () => {
      await Promise.resolve();
    });
    expect(puts(server)).toEqual([]);
  });
});

describe('the drop of another account’s record at load', () => {
  it('is waited for before the state is read, so a registration made meanwhile is not mistaken for it', async () => {
    let finish: () => void = () => undefined;
    const fake = fakePush({
      unsubscribe: vi.fn(
        () =>
          new Promise<void>((resolve) => {
            finish = resolve;
          }),
      ),
    }, true);
    platform.services.push = fake;
    const server = serve();
    api.keepPushRegistration({ id: 'reg-9', account: 9 });
    const hook = renderHook(() => usePushStored(props()));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50));
    });
    expect(hook.result.current.state.kind).toBe('checking');
    expect(server.requests.some((request) => request.path === '/v1/push/config')).toBe(false);
    finish();
    await settled(server, hook, 'off');
  });
});
