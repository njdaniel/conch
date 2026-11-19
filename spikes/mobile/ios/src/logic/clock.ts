// Copied from spikes/mobile/android at ea00f74f (#1390, M4-B) for the iOS spike (#1394, M5-B).
// The clock every piece of logic here is given, so a test drives time by hand.
// The same shape as web/src/state/feed.ts (copied at ed4a68f9).
export interface Clock {
  setTimeout: (run: () => void, ms: number) => unknown;
  clearTimeout: (handle: unknown) => void;
}
