// The isolation properties of issue #945, with two real HTTP servers standing
// in for two conchd and the same pieces the app wires per activation (a
// Session, the Client and Service over it, a gate, feeds), under the real
// switch sequence. What the window shows is a fake that records what main
// sends it.
import { afterEach, describe, expect, it } from 'vitest';

import { Client } from '../../main/api/client.ts';
import { Feeds, type Connect, type SocketHandlers } from '../../main/api/feeds.ts';
import { Service } from '../../main/api/service.ts';
import { Session } from '../../main/api/session.ts';
import { DocumentGate } from '../../main/document.ts';
import { TeardownHooks } from '../../main/profiles/hooks.ts';
import { Switcher, type Activation } from '../../main/profiles/switch.ts';
import type { Profile } from '../../main/profiles/store.ts';
import type { FeedEvent } from '../../shared/bridge-names.ts';
import { sendJson, startHttp, who, type Fake } from './support.ts';

interface Window {
  readonly name: string;
  readonly received: string[];
  destroyed: boolean;
  readonly storage: Map<string, string>;
}

interface Rig {
  readonly activation: Activation;
  readonly service: Service;
  readonly feeds: Feeds;
  readonly window: Window;
  readonly sockets: SocketHandlers[];
  readonly gate: DocumentGate;
}

const servers: Fake[] = [];
afterEach(async () => {
  await Promise.all(servers.splice(0).map((s) => s.close()));
});

function profile(n: string, origin: string): Profile {
  return { id: n.repeat(32), label: n, origin, principal: null };
}

function harness() {
  const rigs: Rig[] = [];
  const windows: Window[] = [];
  const switcher = new Switcher({
    hooks: new TeardownHooks(),
    now: Date.now,
    log: () => undefined,
    open: (spec) => {
      const p = spec.profile;
      const session = new Session();
      const client = new Client({ fetch: globalThis.fetch, session });
      const service = new Service({ client, session, now: Date.now, log: () => undefined });
      if (p !== null) {
        session.setOrigin(p.origin);
        session.adopt(`tok-${p.label}`);
      }
      const window: Window = { name: p?.label ?? 'chooser', received: [], destroyed: false, storage: new Map() };
      const gate = new DocumentGate();
      gate.begin();
      gate.commit({ frameToken: 'f' });
      const sockets: SocketHandlers[] = [];
      const connect: Connect = (_url, _headers, on) => {
        sockets.push(on);
        return { close: () => undefined, destroy: () => undefined, pause: () => undefined, resume: () => undefined } as unknown as ReturnType<Connect>;
      };
      const feeds = new Feeds({
        session,
        client,
        connect,
        deliver: (document, event: FeedEvent) => {
          // As app.ts: only to a live window, and only for the document that subscribed.
          if (!window.destroyed && gate.shows(document)) window.received.push(JSON.stringify(event));
        },
        log: () => undefined,
      });
      gate.onEnd((current) => {
        feeds.endDocument(current);
      });
      const activation: Activation = {
        profileId: p?.id ?? null,
        token: () => session.capture().token,
        whoami: () => Promise.resolve(null),
        signOut: () => service.signOut(),
        endSockets: () => {
          gate.begin();
        },
        endRequests: () => {
          session.forget();
        },
        destroy: () => {
          window.destroyed = true;
          window.storage.clear();
          return Promise.resolve();
        },
      };
      windows.push(window);
      rigs.push({ activation, service, feeds, window, sockets, gate });
      return Promise.resolve(activation);
    },
  });
  return { switcher, rigs, windows };
}

describe('two servers, two accounts', () => {
  it('a delayed response from A after switching to B never reaches B’s window, and A’s request is aborted', async () => {
    let release: () => void = () => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const a = await startHttp((_req, res) => {
      void held.then(() => {
        sendJson(res, 200, { ...who, name: 'ada-from-A' });
      });
    });
    const b = await startHttp((_req, res) => {
      sendJson(res, 200, { ...who, name: 'grace-from-B' });
    });
    servers.push(a, b);
    const h = harness();
    await h.switcher.switchTo(profile('a', a.origin), { reason: 'switch' });
    const pendingA = h.rigs[0]?.service.restore();
    await new Promise((r) => setTimeout(r, 30));
    expect(a.seen).toHaveLength(1);
    await h.switcher.switchTo(profile('b', b.origin), { reason: 'switch' });
    release();
    const lateA = await pendingA;
    // A's answer was dropped in main: it is not an account, and it was not
    // handed to anyone.
    expect(lateA?.ok).toBe(false);
    expect(JSON.stringify(lateA)).not.toContain('ada-from-A');
    // B's service answers B's own server, with B's own credential, and its
    // requests never touch A.
    const mine = await h.rigs[1]?.service.restore();
    expect(mine?.ok === true && mine.who?.name).toBe('grace-from-B');
    expect(b.seen[0]?.headers['authorization']).toBe('Bearer tok-b');
    expect(a.seen.every((s) => s.headers['authorization'] === 'Bearer tok-a')).toBe(true);
    expect(a.seen).toHaveLength(1);
  });

  it('a delayed WebSocket frame from A after switching to B never reaches B’s window', async () => {
    const a = await startHttp(() => undefined);
    const b = await startHttp(() => undefined);
    servers.push(a, b);
    const h = harness();
    await h.switcher.switchTo(profile('a', a.origin), { reason: 'switch' });
    const rigA = h.rigs[0];
    rigA?.feeds.subscribe(rigA.gate.generation, 1, { kind: 'occupancy' });
    expect(rigA?.sockets).toHaveLength(1);
    await h.switcher.switchTo(profile('b', b.origin), { reason: 'switch' });
    const rigB = h.rigs[1];
    // The frame arrives late, on the socket A's main still holds.
    rigA?.sockets[0]?.open();
    rigA?.sockets[0]?.frame('{"type":"voice.occupancy","from":"A"}');
    expect(rigB?.window.received).toEqual([]);
    expect(rigA?.window.received).toEqual([]);
    expect(rigA?.feeds.open).toBe(0);
  });

  it('what one profile’s window stored is gone, and not visible in the next', async () => {
    const h = harness();
    await h.switcher.switchTo(profile('a', 'http://127.0.0.1:1'), { reason: 'switch' });
    h.windows[0]?.storage.set('draft:general', 'A’s unsent words');
    h.windows[0]?.storage.set('pref:theme', 'dark');
    await h.switcher.switchTo(profile('b', 'http://127.0.0.1:2'), { reason: 'switch' });
    expect(h.windows[1]?.storage.size).toBe(0);
    expect(h.windows[0]?.storage.size).toBe(0);
    expect(h.windows[0]?.destroyed).toBe(true);
    expect(h.windows[1]).not.toBe(h.windows[0]);
  });
});
