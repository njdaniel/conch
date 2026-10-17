// The time "x minutes ago" is counted from, moved on every half minute (or on
// every `every` ms) so the words do not go stale on a page that is left open.
import { useEffect, useState } from 'react';

export function useNow(active: boolean, every = 30_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const handle = window.setInterval(() => {
      setNow(Date.now());
    }, every);
    return () => {
      window.clearInterval(handle);
    };
  }, [active, every]);
  return now;
}
