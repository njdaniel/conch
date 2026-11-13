import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import { readFileSync } from 'node:fs';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { NotificationSettings } from './NotificationSettings';
import { platform } from './platform';
import { fromBase64url, push as browserPush, toBase64url } from './platform/browser/push';
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
    expect(workerSource).not.toMatch(/\bfetch\s*\(|XMLHttpRequest|WebSocket|localStorage|sessionStorage|indexedDB|Authorization|\btoken\b|importScripts/);
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
    expect(browser.serviceWorker.register).toHaveBeenCalledWith(`${import.meta.env.BASE_URL}sw.js`, { scope: import.meta.env.BASE_URL, updateViaCache: 'none' });
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
  answer: (request: Request) => Response;
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

function fakePush(overrides: Partial<PushService> = {}): PushService & { readonly calls: string[] } {
  const calls: string[] = [];
  return {
    calls,
    supported: () => true,
    subscribe: vi.fn((key: string) => {
      calls.push(`subscribe ${key}`);
      return Promise.resolve({ endpoint: 'https://push.example/e', p256dh: 'BAUG', auth: 'Bwg' });
    }),
    subscribed: vi.fn(() => Promise.resolve(true)),
    unsubscribe: vi.fn(() => {
      calls.push('unsubscribe');
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
    expect(renderHook(() => usePush(props({ eligible: false }))).result.current.state.kind).toBe('hidden');
    expect(renderHook(() => usePush(props({ signedIn: false }))).result.current.state.kind).toBe('hidden');
    expect(server.requests).toEqual([]);
  });

  it('is unsupported where the browser has no push, without asking the server', () => {
    platform.services.push = fakePush({ supported: () => false });
    const server = serve();
    expect(renderHook(() => usePush(props())).result.current.state.kind).toBe('unsupported');
    platform.services.push = undefined;
    expect(renderHook(() => usePush(props())).result.current.state.kind).toBe('unsupported');
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
    const hook = renderHook(() => usePush(props()));
    await settled(server, hook, 'not-configured');
  });

  it('is off when configured and nothing is registered, and denied when the browser blocks notifications', async () => {
    platform.services.push = fakePush();
    const server = serve();
    const off = renderHook(() => usePush(props()));
    await settled(server, off, 'off');
    permission = 'denied';
    const denied = renderHook(() => usePush(props()));
    await settled(server, denied, 'denied');
  });

  it('is on when a record, a subscription and a registration the server still holds agree', async () => {
    platform.services.push = fakePush();
    const server = serve();
    api.keepPushRegistration('reg-1');
    server.answer = ((base) => (request: Request) => (request.path === '/v1/push/registrations/mine' ? json({ registrations: [registration()] }) : base(request)))(server.answer);
    const hook = renderHook(() => usePush(props()));
    await settled(server, hook, 'on');
  });

  it('reads a record the server no longer holds as off, and clears it', async () => {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    api.keepPushRegistration('reg-1');
    const hook = renderHook(() => usePush(props()));
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
    const hook = renderHook((p: Props) => usePush(p), { initialProps: props(over) });
    await settled(server, hook, 'off');
    server.requests.length = 0;
    return { fake, server, hook };
  }

  it('asks permission, subscribes with the server’s key, registers as a web subscription of this origin, then mirrors the policy and mutes', async () => {
    permission = 'default';
    const { fake, server, hook } = await ready();
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current.state.kind).toBe('on');
    expect(fake.calls).toEqual(['subscribe AQID']);
    expect(server.requests).toEqual([
      {
        method: 'POST',
        path: '/v1/push/registrations',
        body: { transport: 'webpush', platform: 'web', app: window.location.origin, endpoint: 'https://push.example/e', p256dh: 'BAUG', auth: 'Bwg' },
      },
      { method: 'PUT', path: '/v1/notifications/preferences', body: { policy: 'mentions', muted: [7], preview: false, quiet_when_unreachable: false } },
    ]);
    expect(api.readPushRegistration()).toBe('reg-1');
    expect(window.localStorage.getItem(registrationKey)).toBe('reg-1');
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

  it('drops the subscription when the server refuses the registration, and mirrors nothing', async () => {
    const { fake, server, hook } = await ready();
    server.answer = (request) => (request.method === 'POST' ? json({ code: 'push_endpoint_refused', message: 'no' }, 400) : json({}, 404));
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current).toMatchObject({ state: { kind: 'off' }, failure: 'register' });
    expect(fake.calls).toEqual(['subscribe AQID', 'unsubscribe']);
    expect(server.requests.map((request) => request.method)).toEqual(['POST']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('ends the registration and drops the subscription when the policy cannot be mirrored', async () => {
    const { fake, server, hook } = await ready();
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    await act(async () => {
      await hook.result.current.enable();
    });
    expect(hook.result.current).toMatchObject({ state: { kind: 'off' }, failure: 'preferences' });
    expect(fake.calls).toEqual(['subscribe AQID', 'unsubscribe']);
    expect(server.requests.map((request) => `${request.method} ${request.path}`)).toEqual([
      'POST /v1/push/registrations',
      'PUT /v1/notifications/preferences',
      'DELETE /v1/push/registrations/reg-1',
    ]);
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

  it('says so when a change could not be copied, and sends it again on the next change', async () => {
    const { server, hook } = await ready();
    await act(async () => {
      await hook.result.current.enable();
    });
    const base = server.answer;
    server.answer = (request) => (request.method === 'PUT' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    hook.rerender(props({ policy: 'all' }));
    await waitFor(() => {
      expect(hook.result.current.failure).toBe('mirror');
    });
    server.answer = base;
    hook.rerender(props({ policy: 'off' }));
    await waitFor(() => {
      expect(hook.result.current.failure).toBeNull();
    });
  });
});

describe('turning it off', () => {
  async function on() {
    const fake = fakePush();
    platform.services.push = fake;
    const server = serve();
    const hook = renderHook(() => usePush(props()));
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
    expect(api.readPushRegistration()).toBe('reg-1');
  });
});

describe('signing out', () => {
  function registered() {
    const fake = fakePush();
    platform.services.push = fake;
    api.keepPushRegistration('reg-1');
    const server = serve();
    return { fake, server };
  }

  it('ends the registration and drops the subscription before the credential is revoked', async () => {
    const { fake, server } = registered();
    expect(await api.signOut()).toBe(true);
    expect(server.requests.map((request) => `${request.method} ${request.path}`)).toEqual(['DELETE /v1/push/registrations/reg-1', 'DELETE /v1/credentials/current']);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
  });

  it('signs out all the same when the registration could not be ended', async () => {
    const { fake, server } = registered();
    const base = server.answer;
    server.answer = (request) => (request.path === '/v1/push/registrations/reg-1' ? json({ code: 'internal_error', message: 'no' }, 500) : base(request));
    expect(await api.signOut()).toBe(true);
    expect(fake.calls).toEqual(['unsubscribe']);
    expect(api.readPushRegistration()).toBeNull();
    expect(window.localStorage.getItem(tokenKey)).toBeNull();
  });

  it('asks nothing about push of a browser that never registered', async () => {
    platform.services.push = fakePush();
    const server = serve();
    expect(await api.signOut()).toBe(true);
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
    expect(api.readPushRegistration()).toBe('reg-1');
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
