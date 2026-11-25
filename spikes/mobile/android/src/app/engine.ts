// The SDK's engine as logic/dropped.ts sees it (#1972). Its own file, and
// importing only livekit-client (no React Native), so that a unit test can
// check it against the real SDK: test/engine.test.ts builds a livekit-client
// Room in node and fails if an upgrade changes what is read or replaced here.
//
// Pinned to livekit-client 2.22.3, whose internals it reaches:
//  - `attemptingReconnect`, private there: true while RTCEngine.attemptReconnect
//    runs, a restart's signal join included;
//  - `join` on the engine, and `join` and `reconnect` on its SignalClient: the
//    only ways the engine opens a signal connection. A restart, its retry on
//    the legacy endpoint (join calling itself) and its retry in another region
//    (restartConnection calling itself) all go through `this.join`, a resume
//    through `client.reconnect`. Sealing replaces the three on this engine's
//    instances with a refusal, so none of them can join again.
// If the shape is not the one expected (engineKnown false), the adapter reads
// every attempt as still in flight: the drop then waits out droppedLimit and
// lingers, which is slow but never lets an unknown engine rejoin unseen.
import { ConnectionError, type Room } from 'livekit-client';
import type { DroppedEngine } from '../logic/dropped';

export type RoomEngine = Room['engine'];

// The refusal's status: a 403 is what the SDK reads as a token it may not use
// (ConnectionError.notAllowed), so a restart gives up instead of trying again.
const refusedStatus = 403;
const refusal = 'this connection was dropped by the app (#1972)';

interface Internals {
  readonly attemptingReconnect?: unknown;
  readonly join?: unknown;
  readonly client?: { readonly join?: unknown; readonly reconnect?: unknown; readonly sendLeave?: unknown };
}

// engineKnown says the engine has the internals this adapter was written for.
export function engineKnown(engine: RoomEngine): boolean {
  const e = engine as unknown as Internals;
  return (
    typeof e.attemptingReconnect === 'boolean' &&
    typeof e.join === 'function' &&
    typeof e.client?.join === 'function' &&
    typeof e.client.reconnect === 'function' &&
    typeof e.client.sendLeave === 'function'
  );
}

export function droppedEngine(engine: RoomEngine): DroppedEngine {
  const known = engineKnown(engine);
  return {
    get closed() {
      return engine.isClosed;
    },
    get connecting() {
      if (!known) return true;
      return (engine as unknown as Internals).attemptingReconnect === true;
    },
    seal: () => {
      const refuse = (): Promise<never> => Promise.reject(ConnectionError.notAllowed(refusal, refusedStatus));
      // Each on its own: an unknown shape seals what it can and never throws
      // (the drop goes on, waiting out the limit as above).
      const sealing = [
        () => (engine.join = refuse),
        () => (engine.client.join = refuse),
        () => (engine.client.reconnect = refuse),
      ];
      for (const s of sealing) {
        try {
          s();
        } catch {
          // An engine without that part: nothing to seal there.
        }
      }
    },
    leave: async () => {
      if (!engine.client.isDisconnected) await engine.client.sendLeave();
    },
    close: () => engine.close('dropped by the app'),
  };
}

// goneEngine stands in when the SDK's room has already let its engine go
// (Room.disconnect clears it): nothing is left that could connect.
export const goneEngine: DroppedEngine = {
  closed: true,
  connecting: false,
  seal: () => undefined,
  leave: () => Promise.resolve(),
  close: () => Promise.resolve(),
};
