// One activation of a profile, in Electron (issue #945): its own identity in
// main (a Session, the Client and Service over it, its feeds), its own
// document gate, and its own window in a fresh in-memory partition. Nothing
// is shared with the activation before it or after it, so nothing of one
// profile's can reach another's: a switch makes all of this anew and destroys
// the old.
//
// Input: the app's fixed environment and the profile to open with the
// credential main found for it. Output: an Activation for the switch sequence,
// and a Live that the one IPC registry reads its current parts from.
import type { BrowserWindow } from 'electron';

import { FEED_EVENTS } from '../../shared/bridge-names.ts';
import { whoAmIResponseV1 } from '../../../web/src/schema/principal.ts';
import { Client } from '../api/client.ts';
import { Feeds, type Connect } from '../api/feeds.ts';
import { Service } from '../api/service.ts';
import { Session } from '../api/session.ts';
import { DocumentGate } from '../document.ts';
import { guardSession } from '../guards.ts';
import type { ContentsLike } from '../ipc.ts';
import { handle, SCHEME, type Manifest } from '../scheme.ts';
import { createWindow, partitionFor } from '../window.ts';
import type { ProbeResult, StoredCheck } from './service.ts';
import type { Account, Activation, OpenSpec } from './switch.ts';

export interface ActivationEnv {
  readonly packaged: boolean;
  readonly appPath: string;
  readonly manifest: Manifest;
  readonly readUi: (path: string) => Promise<Uint8Array>;
  readonly connect: Connect;
  readonly now: () => number;
  readonly log: (line: string) => void;
}

// What the one IPC registry calls into: always the newest activation's parts.
// The parts of a finished activation stay until the next opens, and answer
// nothing (their window is gone, so every caller is refused).
export class Live {
  service: Service | null = null;
  feeds: Feeds | null = null;
  gate: DocumentGate = new DocumentGate();
  window: BrowserWindow | null = null;
  // True while an activation's window is being destroyed on purpose, so that
  // the last window closing is not the app ending.
  destroying = false;

  owned(): ContentsLike | null {
    return this.window === null || this.window.isDestroyed() ? null : this.window.webContents;
  }
}

type Bounds = { x: number; y: number; width: number; height: number };

export function activationFactory(env: ActivationEnv, live: Live): (spec: OpenSpec & { readonly token: string | null }) => Promise<Activation> {
  let bounds: Bounds | undefined;
  return (spec) => {
    const { profile, token } = spec;
    const identity = new Session();
    const client = new Client({ fetch: globalThis.fetch, session: identity });
    const service = new Service({ client, session: identity, now: env.now, log: env.log });
    if (profile !== null) {
      identity.setOrigin(profile.origin);
      if (token !== null) identity.adopt(token);
    }
    const gate = new DocumentGate();
    let window: BrowserWindow | null = null;
    const owned = (): ContentsLike | null => (window === null || window.isDestroyed() ? null : window.webContents);
    const feeds = new Feeds({
      session: identity,
      client,
      connect: env.connect,
      deliver: (document, event) => {
        const contents = owned();
        if (contents !== null && gate.shows(document)) window?.webContents.send(FEED_EVENTS, event);
      },
      log: env.log,
    });
    gate.onEnd((current) => {
      feeds.endDocument(current);
    });
    window = createWindow(env.packaged, env.appPath, gate, {
      partition: partitionFor(spec.generation),
      ...(bounds === undefined ? {} : { bounds }),
      prepare: (made) => {
        const ses = made.webContents.session;
        guardSession(ses, { log: env.log });
        ses.protocol.handle(SCHEME, (request) => handle(request, { manifest: env.manifest, readFile: (path) => env.readUi(path), log: env.log }));
      },
    });
    live.service = service;
    live.feeds = feeds;
    live.gate = gate;
    live.window = window;

    const activation: Activation = {
      profileId: profile?.id ?? null,
      token: () => identity.capture().token,
      async whoami(): Promise<Account | null> {
        const outcome = await service.restore();
        if (!outcome.ok || outcome.who === null || whoAmIResponseV1(outcome.who).length > 0) return null;
        const who = outcome.who;
        return { id: who.id, name: who.name, lasting: who.role === 'member' && who.guest === undefined, guest: who.guest !== undefined };
      },
      signOut: () => service.signOut(),
      endSockets: () => {
        gate.begin();
      },
      endRequests: () => {
        identity.forget();
      },
      async destroy() {
        const w = window;
        if (w.isDestroyed()) return;
        const ses = w.webContents.session;
        bounds = w.isMaximized() || w.isFullScreen() ? bounds : w.getBounds();
        live.destroying = true;
        try {
          await new Promise<void>((done) => {
            w.once('closed', () => {
              done();
            });
            w.destroy();
          });
        } finally {
          live.destroying = false;
        }
        // The partition is in memory; what it held is dropped now rather than
        // when the process ends.
        await ses.clearStorageData().catch(() => undefined);
        await ses.clearCache().catch(() => undefined);
        await ses.clearAuthCache().catch(() => undefined);
        await ses.closeAllConnections().catch(() => undefined);
      },
    };
    return Promise.resolve(activation);
  };
}

// probe asks an address whether it is a Conch server: /healthz, with no
// credential, through the same client (so the same trust and redirect rules).
export function prober(log: (line: string) => void): (origin: string) => Promise<ProbeResult> {
  return async (origin) => {
    const session = new Session();
    session.setOrigin(origin);
    const client = new Client({ fetch: globalThis.fetch, session });
    const exchange = await client.send({ op: 'health' });
    session.forget();
    if (exchange.kind === 'none') {
      if (exchange.reason === 'certificate') return 'certificate';
      if (exchange.reason === 'invalid' || exchange.reason === 'oversize' || exchange.reason === 'redirect') return 'not-conch';
      log('conch-desktop: an address did not answer');
      return 'unreachable';
    }
    return exchange.kind === 'answer' && exchange.status === 200 ? 'ok' : 'not-conch';
  };
}

// revoker tells a server that a credential held for a profile that is not
// active is over: a session of its own, the same client, the same revocation as
// sign-out. It never raises.
export function revoker(log: (line: string) => void): (origin: string, token: string) => Promise<'ended' | 'unconfirmed' | 'unsent'> {
  return async (origin, token) => {
    const session = new Session();
    const client = new Client({ fetch: globalThis.fetch, session });
    const service = new Service({ client, session, now: Date.now, log });
    session.setOrigin(origin);
    session.adopt(token);
    return service.signOut().catch(() => 'unsent' as const);
  };
}

// verifier asks a server who a stored credential is, in a session of its own
// and before any window holds it (issue #945: a restored credential is checked
// against the principal its profile is bound to). It never raises.
export function verifier(log: (line: string) => void): (origin: string, token: string) => Promise<StoredCheck> {
  return async (origin, token) => {
    const session = new Session();
    const client = new Client({ fetch: globalThis.fetch, session });
    const service = new Service({ client, session, now: Date.now, log });
    session.setOrigin(origin);
    session.adopt(token);
    try {
      const outcome = await service.restore();
      if (outcome.ok && outcome.who !== null && whoAmIResponseV1(outcome.who).length === 0) {
        const who = outcome.who;
        return { kind: 'account', account: { id: who.id, name: who.name, lasting: who.role === 'member' && who.guest === undefined, guest: who.guest !== undefined } };
      }
      return !outcome.ok && outcome.kind === 'unauthorized' ? { kind: 'rejected' } : { kind: 'unreachable' };
    } catch {
      return { kind: 'unreachable' };
    } finally {
      session.forget();
    }
  };
}
