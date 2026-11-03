// The community kept in step with the server (issue #993, community-model
// §13.6): the events socket says "your directory answer may have changed"
// (conch.directory_changed.v1, no content) and the page reads the directory
// again and replaces what it holds. Frames that land close together make one
// read: the server already sends one frame per floor, so this only absorbs a
// burst that crosses sockets or a reconnect.
import { useCallback, useEffect, useRef, useState } from 'react';

// How long after the first of a burst of frames the read is made.
export const directoryCoalesce = 250;

export function useDirectoryChanged(): { readonly tick: number; readonly signal: () => void } {
  const [tick, setTick] = useState(0);
  const timer = useRef<number | null>(null);
  const signal = useCallback(() => {
    if (timer.current !== null) return;
    timer.current = window.setTimeout(() => {
      timer.current = null;
      setTick((n) => n + 1);
    }, directoryCoalesce);
  }, []);
  useEffect(
    () => () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
      timer.current = null;
    },
    [],
  );
  return { tick, signal };
}
