// Copied from spikes/mobile/android at 29d2c234 (#1392, M4-D, with its review fixes) for the iOS spike (#1396, M5-D).
// The cameras received, by principal, as an external store for the grid. The
// map is replaced, never mutated, so a snapshot is stable.
import type { RemoteVideoTrack } from 'livekit-client';

type Tracks = ReadonlyMap<number, RemoteVideoTrack>;

let current: Tracks = new Map();
const listeners = new Set<() => void>();

export const cameraStore = {
  get: (): Tracks => current,
  subscribe: (listener: () => void): (() => void) => {
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  },
  set: (next: Tracks): void => {
    current = next;
    for (const l of [...listeners]) l();
  },
};
