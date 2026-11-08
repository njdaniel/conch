import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';

import { Client } from '../../main/api/client.ts';
import { Service } from '../../main/api/service.ts';
import { Session } from '../../main/api/session.ts';
import type { ActivationSpec } from '../../main/profiles/service.ts';
import type { Activation } from '../../main/profiles/switch.ts';
import { previewResponseV1 } from '../../../web/src/schema/preview.ts';
import { fromServer } from '../../../web/src/schema/validate.ts';
import { sendJson, startHttp, type Fake } from './support.ts';

import { TeardownHooks } from '../../main/profiles/hooks.ts';
import { Switcher } from '../../main/profiles/switch.ts';
import type { Profile } from '../../main/profiles/store.ts';
import { fakeFactory, ORIGIN_A, ORIGIN_B } from './profiles-support.ts';

const profile = (n: string, origin: string): Profile => ({ id: n.repeat(32), label: n, origin, principal: null });

function make(boundMs = 50) {
  const events: { name: string; at: number }[] = [];
  let clock = 0;
  const now = () => ++clock;
  const log: string[] = [];
  const factory = fakeFactory(log);
  const hooks = new TeardownHooks();
  const logs: string[] = [];
  const switcher = new Switcher({
    hooks,
    now,
    log: (line) => logs.push(line),
    boundMs,
    open: (spec) => factory.create({ ...spec, token: null, onInvalid: () => undefined }),
  });
  return { switcher, hooks, factory, events, now, logs, log };
}

describe('the switch sequence', () => {
  it('runs media hooks, sockets, requests, renderer, helper hooks and only then activates, each after the last, with times to prove it', async () => {
    const t = make();
    const a = profile('a', ORIGIN_A);
    const b = profile('b', ORIGIN_B);
    await t.switcher.switchTo(a, { reason: 'switch' });
    const stamp = (name: string) => () => {
      t.events.push({ name, at: t.now() });
    };
    t.hooks.media('voice', stamp('media:voice'));
    t.hooks.media('share', async () => {
      await new Promise((r) => setTimeout(r, 5));
      stamp('media:share')();
    });
    t.hooks.helper('helper', stamp('helper'));
    const report = await t.switcher.switchTo(b, { reason: 'switch' });
    expect(report.trace.map((e) => e.step)).toEqual(['media', 'media', 'sockets', 'requests', 'renderer', 'helper', 'activate']);
    const times = report.trace.map((e) => e.at);
    expect([...times].sort((x, y) => x - y)).toEqual(times);
    expect(new Set(times).size).toBe(times.length);
    expect(t.events.map((e) => e.name)).toEqual(['media:voice', 'media:share', 'helper']);
    // The old renderer is gone before the helper is released, and the next
    // profile opens after both.
    expect(t.log.slice(-4)).toEqual(['0:sockets', '0:requests', '0:renderer', '1:open bbbb']);
    expect(t.events[2]?.at).toBeGreaterThan(times[4] ?? Infinity);
    expect(t.events[2]?.at).toBeLessThan(times[6] ?? 0);
  });

  it('gives a fresh activation to every switch, none shared', async () => {
    const t = make();
    await t.switcher.switchTo(profile('a', ORIGIN_A), { reason: 'switch' });
    await t.switcher.switchTo(profile('b', ORIGIN_B), { reason: 'switch' });
    expect(t.factory.made).toHaveLength(2);
    expect(t.factory.made[0]).not.toBe(t.factory.made[1]);
    expect(t.factory.made[0]?.done).toEqual(['sockets', 'requests', 'renderer']);
    expect(t.factory.made[1]?.done).toEqual([]);
  });

  it('reports a hook that hangs past its bound, and still completes the switch without the old identity', async () => {
    const t = make(20);
    await t.switcher.switchTo(profile('a', ORIGIN_A), { reason: 'switch' });
    t.hooks.media('stuck-share', () => new Promise<void>(() => undefined));
    t.hooks.helper('throws', () => {
      throw new Error('secret-detail');
    });
    const report = await t.switcher.switchTo(profile('b', ORIGIN_B), { reason: 'switch' });
    expect(report.hung).toEqual(['stuck-share', 'throws']);
    expect(report.trace.find((e) => e.name === 'stuck-share')?.outcome).toBe('hung');
    expect(report.trace.find((e) => e.name === 'throws')?.outcome).toBe('failed');
    // The old activation was ended anyway, and the next one is in force.
    expect(t.factory.made[0]?.done).toEqual(['sockets', 'requests', 'renderer']);
    expect(t.switcher.current?.profileId).toBe('b'.repeat(32));
    expect(t.logs.join('\n')).toContain('"stuck-share" did not finish in time');
    expect(t.logs.join('\n')).not.toContain('secret-detail');
  });

  it('runs one switch at a time, and tells an older completion it has been superseded', async () => {
    const t = make();
    await t.switcher.switchTo(profile('a', ORIGIN_A), { reason: 'switch' });
    const g = t.switcher.generation;
    const first = t.switcher.switchTo(profile('b', ORIGIN_B), { reason: 'switch' });
    const second = t.switcher.switchTo(null, { reason: 'switch' });
    expect(t.switcher.isCurrent(g)).toBe(false);
    await Promise.all([first, second]);
    expect(t.factory.made.map((a) => a.profileId)).toEqual(['a'.repeat(32), 'b'.repeat(32), null]);
    expect(t.factory.made[1]?.done).toEqual(['sockets', 'requests', 'renderer']);
  });

  it('close runs the teardown, with the quit reason, and opens nothing', async () => {
    const t = make();
    const reasons: string[] = [];
    t.hooks.media('voice', ({ reason }) => {
      reasons.push(reason);
    });
    await t.switcher.switchTo(profile('a', ORIGIN_A), { reason: 'switch' });
    reasons.length = 0;
    await t.switcher.close({ reason: 'quit' });
    expect(reasons).toEqual(['quit']);
    expect(t.factory.made).toHaveLength(1);
    expect(t.switcher.current).toBeNull();
  });

  it('a hook that unregisters is not run again', async () => {
    const t = make();
    let runs = 0;
    const off = t.hooks.media('voice', () => {
      runs++;
    });
    await t.switcher.switchTo(profile('a', ORIGIN_A), { reason: 'switch' });
    await t.switcher.switchTo(profile('b', ORIGIN_B), { reason: 'switch' });
    off();
    await t.switcher.switchTo(profile('a', ORIGIN_A), { reason: 'switch' });
    expect(runs).toBe(2);
  });
});

// The golden PreviewResponseV1: a complete answer the client would accept, so
// only the cancellation can make the held read come back as stale.
const previewBody: unknown = JSON.parse(readFileSync(resolve(import.meta.dirname, '../../../pkg/schema/testdata/preview-response-v1.json'), 'utf8'));

describe('a profile switch ends a preview (issue #1540)', () => {
  it('the held response is a valid PreviewResponseV1', () => {
    expect(fromServer(previewResponseV1)(previewBody)).toEqual([]);
  });

  const servers: Fake[] = [];
  afterEach(async () => {
    await Promise.all(servers.splice(0).map((f) => f.close()));
  });

  it('cancels a delayed preview read, disposes the renderer, and sends the next profile\u2019s first request without Conch-Preview-As', async () => {
    // Server A holds the preview read open until the test lets it go.
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const a = await startHttp((_req, res) => {
      void held.then(() => {
        if (!res.destroyed) sendJson(res, 200, previewBody);
      });
    });
    const b = await startHttp((_req, res) => {
      sendJson(res, 200, { channels: [] });
    });
    servers.push(a, b);

    // Real Session, Client and Service per activation, over real sockets; the
    // window is the one fake, a record of whether it was disposed.
    const live: { service: Service; session: Session; disposed: boolean }[] = [];
    const create = (spec: ActivationSpec): Promise<Activation> => {
      const session = new Session();
      const service = new Service({ client: new Client({ fetch: globalThis.fetch, session }), session, now: Date.now, log: () => undefined });
      if (spec.profile !== null) {
        session.setOrigin(spec.profile.origin);
        session.adopt('bearer-for-' + spec.profile.label);
      }
      const entry = { service, session, disposed: false };
      live.push(entry);
      return Promise.resolve({
        profileId: spec.profile?.id ?? null,
        token: () => session.capture().token,
        voiceCredential: () => null,
        voiceCurrent: () => false,
        whoami: () => Promise.resolve(null),
        signOut: () => Promise.resolve('unsent' as const),
        endSockets: () => undefined,
        endRequests: () => {
          session.forget();
        },
        destroy: () => {
          entry.disposed = true;
          return Promise.resolve();
        },
      });
    };
    const switcher = new Switcher({ hooks: new TeardownHooks(), now: Date.now, log: () => undefined, boundMs: 50, open: (spec) => create({ ...spec, token: null, onInvalid: () => undefined }) });

    await switcher.switchTo(profile('a', a.origin), { reason: 'switch' });
    const old = live[0];
    if (old === undefined) throw new Error('no first activation');
    // The preview read is in flight when the person switches.
    const preview = old.service.request({ operation: 'preview', previewAs: 7 });
    await new Promise((r) => setTimeout(r, 50));
    expect(a.seen).toHaveLength(1);
    expect(a.seen[0]?.headers['conch-preview-as']).toBe('7');

    await switcher.switchTo(profile('b', b.origin), { reason: 'switch' });
    release();

    // The old read was cancelled, its late answer never reaches the page.
    expect(await preview).toEqual({ kind: 'none', reason: 'stale' });
    // The renderer that held the preview is gone.
    expect(old.disposed).toBe(true);
    // The next profile's first request carries no preview.
    const next = live[1];
    if (next === undefined) throw new Error('no second activation');
    await next.service.request({ operation: 'listChannels' });
    expect(b.seen).toHaveLength(1);
    expect(b.seen[0]?.headers['conch-preview-as']).toBeUndefined();
    expect(b.seen[0]?.headers['authorization']).toBe('Bearer bearer-for-b');
  });
});
