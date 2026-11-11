// The clock every piece of logic here is given, so a test drives time by hand.
// The same shape as web/src/state/feed.ts (copied at ed4a68f9).
export interface Clock {
  setTimeout: (run: () => void, ms: number) => unknown;
  clearTimeout: (handle: unknown) => void;
}
