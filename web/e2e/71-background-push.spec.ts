// "Notify me when Conch is closed" (issue #1826): the browser subscribes
// through the service worker conchd serves at /app/sw.js, registers the
// subscription as a web push registration, and keeps the server's copy of its
// notification policy in step.
//
// What this proves: the build serves a worker that Chromium registers in the
// /app/ scope; turning it on creates exactly one registration (platform web,
// transport webpush, app the origin) and puts the local policy and mutes on
// the server; a later change reaches the server; turning it off, and signing
// out, end the registration and drop the subscription, the sign-out before the
// credential is revoked.
//
// What it does not prove: a real push service. Headless Chromium has none to
// subscribe with, so the Push API's subscribe and getSubscription are replaced
// in the page by a stand-in that makes a real P-256 key pair and a private-range
// endpoint (loopback is never admitted, even so; the address is an IP literal,
// so the registration looks nothing up and nothing is dialled; conchd runs with
// --push-webpush-allow-private in this suite); the registration, the
// preferences and the worker's registration are real. No wake travels, and the
// worker's push handler is not run here: headless Chromium reports
// notifications as denied, so showNotification cannot succeed. The handler is
// covered by src/push.test.tsx, and the sender by the Go tests.
import type { Page } from '@playwright/test';
import { expect, signIn, test } from './fixtures';
import { createCredentialResponseV1 } from '../src/schema/credential';
import { call, ok, principalId } from './world';

async function standIn(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { __push: { subscribed: boolean; unsubscribed: number; endpoint: string } };
    w.__push = { subscribed: false, unsubscribed: 0, endpoint: '' };
    // Headless Chromium reports notifications as denied whatever is granted.
    Object.defineProperty(Notification, 'permission', { configurable: true, get: () => 'granted' });
    let keys: { p256dh: ArrayBuffer; auth: ArrayBuffer } | null = null;
    let key: ArrayBuffer | null = null;
    const subscription = {
      get endpoint() {
        return w.__push.endpoint;
      },
      get options() {
        return { applicationServerKey: key };
      },
      getKey: (name: string) => (keys === null ? null : name === 'p256dh' ? keys.p256dh : keys.auth),
      unsubscribe: () => {
        w.__push.subscribed = false;
        w.__push.unsubscribed++;
        return Promise.resolve(true);
      },
    };
    PushManager.prototype.getSubscription = () => Promise.resolve(w.__push.subscribed ? (subscription as unknown as PushSubscription) : null);
    PushManager.prototype.subscribe = async (options?: PushSubscriptionOptionsInit) => {
      const pair = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits']);
      keys = { p256dh: await crypto.subtle.exportKey('raw', pair.publicKey), auth: crypto.getRandomValues(new Uint8Array(16)).buffer };
      const given = options?.applicationServerKey;
      key = typeof given === 'string' || given === null || given === undefined ? null : ArrayBuffer.isView(given) ? given.buffer : given;
      w.__push.endpoint = `https://10.255.255.1:9/push/${crypto.randomUUID()}`;
      w.__push.subscribed = true;
      return subscription as unknown as PushSubscription;
    };
  });
}

interface Registration {
  readonly id: string;
  readonly transport: string;
  readonly platform: string;
  readonly app: string;
}

const mine = async (world: Parameters<typeof call>[0], token: string): Promise<Registration[]> => {
  const reply = await call(world, token, 'GET', '/v1/push/registrations/mine');
  expect(reply.status).toBe(200);
  return (reply.body as { registrations: Registration[] }).registrations;
};

const preferences = async (world: Parameters<typeof call>[0], token: string): Promise<{ policy: string; muted: number[] }> => {
  const reply = await call(world, token, 'GET', '/v1/notifications/preferences');
  expect(reply.status).toBe(200);
  return reply.body as { policy: string; muted: number[] };
};

async function openSettings(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Settings' }).click();
}

test('background push: subscribe, register, mirror the policy, show one fixed notice, end on off and on sign-out', async ({ meet, world }) => {
  const { page } = await meet();
  await standIn(page);
  // This test signs out, which ends the credential it signed in with, so it
  // uses one of its own (as 26-notifications does).
  const own = await ok(world, world.tokens.operator, 'POST', `/v1/principals/${String(await principalId(world, world.tokens.nick))}/credentials`, createCredentialResponseV1, { label: 'e2e background push' });
  await signIn(page, world, own.token, 'nick-test');

  // The worker is a file of the build, served so that Chromium may use it.
  const served = await page.evaluate(async () => {
    const res = await fetch('/app/sw.js', { cache: 'no-store' });
    return { status: res.status, type: res.headers.get('content-type'), cache: res.headers.get('cache-control') };
  });
  expect(served.status).toBe(200);
  expect(served.type).toMatch(/javascript/);
  expect(served.cache).toBe('no-cache');

  await openSettings(page);
  await page.getByLabel('Message notifications').selectOption('all');
  const turnOn = page.getByRole('button', { name: 'Notify me when Conch is closed' });
  await expect(turnOn).toBeVisible();
  expect(await mine(world, own.token)).toEqual([]);
  await turnOn.click();
  await expect(page.getByRole('button', { name: 'Stop notifying me when Conch is closed' })).toBeVisible();

  // One registration of this browser, and the local policy on the server.
  const registrations = await mine(world, own.token);
  expect(registrations).toHaveLength(1);
  expect(registrations[0]).toMatchObject({ transport: 'webpush', platform: 'web', app: world.origin });
  expect(await preferences(world, own.token)).toMatchObject({ policy: 'all', muted: [] });

  // The worker is registered in the client's scope and active.
  const scope = await page.evaluate(async () => (await navigator.serviceWorker.getRegistration('/app/'))?.scope ?? null);
  expect(scope).toBe(`${world.origin}/app/`);

  // A change of the local policy while push is on reaches the server.
  await page.getByLabel('Message notifications').selectOption('mentions');
  await expect.poll(async () => (await preferences(world, own.token)).policy).toBe('mentions');

  // Turning it off ends the registration and drops the subscription.
  await page.getByRole('button', { name: 'Stop notifying me when Conch is closed' }).click();
  await expect(page.getByRole('button', { name: 'Notify me when Conch is closed' })).toBeVisible();
  expect(await mine(world, own.token)).toEqual([]);
  expect(await page.evaluate(() => (window as unknown as { __push: { subscribed: boolean } }).__push.subscribed)).toBe(false);

  // Turned on again, signing out ends it first and the credential after.
  await page.getByRole('button', { name: 'Notify me when Conch is closed' }).click();
  await expect(page.getByRole('button', { name: 'Stop notifying me when Conch is closed' })).toBeVisible();
  const [registration] = await mine(world, own.token);
  const order: string[] = [];
  page.on('request', (request) => {
    if (request.method() === 'DELETE') order.push(new URL(request.url()).pathname);
  });
  await page.getByRole('button', { name: 'Sign out', exact: true }).click();
  await page.getByRole('button', { name: 'Yes, sign out', exact: true }).click();
  await expect.poll(() => order).toContain('/v1/credentials/current');
  // The page ends its agent session first; of the rest, push comes before the credential.
  expect(order.filter((path) => !path.startsWith('/v1/sessions/'))).toEqual([`/v1/push/registrations/${registration?.id ?? ''}`, '/v1/credentials/current']);
  expect(await page.evaluate(() => (window as unknown as { __push: { subscribed: boolean } }).__push.subscribed)).toBe(false);
});
