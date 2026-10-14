// The time "x minutes ago" is counted from, moved on every half minute so the
// words do not go stale on a page that is left open.
import { useEffect, useState } from 'react';

export function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const handle = window.setInterval(() => {
      setNow(Date.now());
    }, 30_000);
    return () => {
      window.clearInterval(handle);
    };
  }, [active]);
  return now;
}
