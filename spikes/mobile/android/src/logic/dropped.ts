// A dropped connection must stop its own reconnect before the phone opens
// another (#1972). Pure; the app (livekit.ts) adapts the SDK's engine to
// DroppedEngine and gives the timer.
//
// Why: in livekit-client 2.22.3 a full reconnect ("restart") that is under way
// when the connection is closed carries on. Its signal join, once answered,
// marks the closed engine open again, and the engine is then connected on the
// old token with no Room listening to it. Seen after 40 s in the background
// with the camera on: the SDK restarted, a leave from the server arrived during
// the restart, the Room reported a disconnect and the app rejoined with a
// fresh session, then the orphaned engine rejoined too and displaced it
// (duplicate_identity). The app said "Not in voice" while LiveKit and
// `conch voice status` still had the person.
//
// So a dropped connection is closed (with a leave, so LiveKit lets the
// participant go at once) and closed again each time it comes back, until no
// reconnect of its own is in flight. Only then is it settled. The wait is
// bounded: the SDK's own timeouts (15 s for the signal join, 15 s for the
// peer connection) end any attempt well within `droppedLimit`.

export interface DroppedEngine {
  // The engine is closed (the SDK's isClosed).
  readonly closed: boolean;
  // A reconnect attempt of the SDK's own is running.
  readonly reconnecting: boolean;
  // Tells LiveKit the participant is leaving, if the signal connection is up.
  leave(): Promise<void>;
  close(): Promise<void>;
}

export interface SettleOptions {
  readonly wait: (ms: number) => Promise<void>;
  // Wall-clock milliseconds: timers pause in the background (controller.ts).
  readonly now: () => number;
  readonly limit?: number;
  readonly every?: number;
}

export const droppedLimit = 35_000;
export const droppedEvery = 100;

export type Settled = 'settled' | 'timed_out';

// settleDropped closes the engine, and closes it again whenever an attempt
// still in flight reopens it, until it is closed with no attempt in flight.
// It never throws: a failed leave or close is retried on the next pass.
export async function settleDropped(engine: DroppedEngine, o: SettleOptions): Promise<Settled> {
  const limit = o.limit ?? droppedLimit;
  const every = o.every ?? droppedEvery;
  const start = o.now();
  for (;;) {
    if (!engine.closed) {
      await engine.leave().catch(() => undefined);
      await engine.close().catch(() => undefined);
    }
    if (engine.closed && !engine.reconnecting) return 'settled';
    if (o.now() - start >= limit) return 'timed_out';
    await o.wait(every);
  }
}
