// A dropped connection must stop its own reconnect before the phone opens
// another (#1972). Pure; the app (livekit.ts, engine.ts) adapts the SDK's
// engine to DroppedEngine and gives the timer.
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
// So a dropped connection is first sealed: from then on the engine refuses to
// start any connection of its own (a restart, the restart's retry on the
// legacy endpoint or in another region, a resume). Then it is closed (with a
// leave, so LiveKit lets the participant go at once), and closed again each
// time it comes back, until no attempt is in flight. Only then is it settled.
// The seal leaves only an attempt already on the wire, which the SDK's own
// timeouts (15 s for the signal join, 15 s for the peer connection) end well
// within `droppedLimit`.
//
// The wait is bounded as a whole: every step (the room's own teardown, each
// leave and close) is raced against the limit, so a step that never returns
// cannot hold the next connection back. At the limit the drop is reported as
// not settled, and a guard carries on closing the engine whenever it comes
// back, up to `lingerLimit`; the controller knows (controller.ts).

export interface DroppedEngine {
  // The engine is closed (the SDK's isClosed).
  readonly closed: boolean;
  // A connection attempt is in flight: the SDK's own reconnect, or the app's
  // first connect (Connection below). An adapter that cannot tell reads true.
  readonly connecting: boolean;
  // Makes the engine refuse every connection attempt that starts from now
  // on. Synchronous, and the first thing done to a dropped engine.
  seal(): void;
  // Tells LiveKit the participant is leaving, if the signal connection is up.
  leave(): Promise<void>;
  close(): Promise<void>;
}

export interface SettleOptions {
  readonly wait: (ms: number) => Promise<void>;
  // Wall-clock milliseconds: timers pause in the background (controller.ts).
  readonly now: () => number;
  // The rest of the room's teardown (the SDK room's disconnect, the audio
  // session): run after the seal, within the same limit.
  readonly teardown?: () => Promise<void>;
  readonly limit?: number;
  readonly linger?: number;
  readonly every?: number;
}

export const droppedLimit = 35_000;
// Counted from the drop, like droppedLimit.
export const lingerLimit = 120_000;
export const droppedEvery = 100;

export interface Retired {
  // Closed with no attempt in flight within droppedLimit.
  readonly settled: boolean;
  // When not settled: the guard still closing the engine whenever it comes
  // back; it resolves once the engine has settled, or at lingerLimit.
  readonly lingering: Promise<void> | null;
}

export const settledRetired: Retired = { settled: true, lingering: null };

// settleDropped seals the engine, runs the teardown, and closes the engine
// again whenever an attempt still in flight reopens it, until it is closed
// with no attempt in flight or the limit is reached. It never throws: a
// failed leave or close is retried on the next pass, and one still pending is
// not started again.
export async function settleDropped(engine: DroppedEngine, o: SettleOptions): Promise<Retired> {
  const start = o.now();
  const limit = start + (o.limit ?? droppedLimit);
  const linger = start + (o.linger ?? lingerLimit);
  const every = o.every ?? droppedEvery;
  engine.seal();

  // within waits for p, or until the deadline, whichever is first.
  const within = async (p: Promise<unknown>, until: number): Promise<void> => {
    const left = until - o.now();
    if (left <= 0) return;
    await Promise.race([p.then(nothing, nothing), o.wait(left)]);
  };
  let closing: Promise<void> | null = null;
  const shut = (): Promise<void> => {
    if (closing === null) {
      closing = (async () => {
        await engine.leave().catch(nothing);
        await engine.close().catch(nothing);
      })().finally(() => {
        closing = null;
      });
    }
    return closing;
  };
  const watch = async (until: number): Promise<boolean> => {
    for (;;) {
      if (!engine.closed) await within(shut(), until);
      if (engine.closed && !engine.connecting) return true;
      const left = until - o.now();
      if (left <= 0) return false;
      await o.wait(Math.min(every, left));
    }
  };

  if (o.teardown !== undefined) await within(o.teardown(), limit);
  if (await watch(limit)) return settledRetired;
  return { settled: false, lingering: watch(linger).then(nothing) };
}

// Connection orders one room's connect against its retirement (#1972): a
// room retired while its audio was being set up never starts the SDK's
// connect, and a retirement waits while that connect is in flight, so a room
// let go can never join afterwards and displace its replacement.
export class Connection {
  private retiring: Promise<Retired> | null = null;
  private inFlight = false;

  get retired(): boolean {
    return this.retiring !== null;
  }

  // connect runs prepare (the audio session), then join (the SDK's connect)
  // unless the connection was retired meanwhile, in which case undo runs
  // instead. True when join ran and the connection is still not retired.
  async connect(prepare: () => Promise<void>, join: () => Promise<void>, undo: () => Promise<void>): Promise<boolean> {
    this.inFlight = true;
    try {
      await prepare();
      if (this.retiring !== null) {
        await undo().catch(nothing);
        return false;
      }
      await join();
      return this.retiring === null;
    } finally {
      this.inFlight = false;
    }
  }

  // retire settles the engine (settleDropped), counting this connect as an
  // attempt in flight. Once only: a second call has the first's answer.
  retire(engine: DroppedEngine, o: SettleOptions): Promise<Retired> {
    if (this.retiring === null) {
      const inFlight = (): boolean => this.inFlight;
      const watched: DroppedEngine = {
        get closed() {
          return engine.closed;
        },
        get connecting() {
          return inFlight() || engine.connecting;
        },
        seal: () => engine.seal(),
        leave: () => engine.leave(),
        close: () => engine.close(),
      };
      this.retiring = settleDropped(watched, o);
    }
    return this.retiring;
  }
}

function nothing(): void {
  return undefined;
}
