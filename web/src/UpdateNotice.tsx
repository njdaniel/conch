import { useEffect, useState } from 'react';
import * as api from './api';

export const updateText = 'Conch was updated. Reload to get the new version.';

// How often the server's version is read again (issue #1534), besides when the
// window regains focus. One request every few minutes.
export const updateCheckInterval = 5 * 60_000;

// A non-blocking notice, shown once when the server's build version differs
// from the one this page started with. The version is /healthz's, which needs
// no credential. It never reloads by itself (that would lose a draft or a
// call); the page keeps working until the person presses Reload. A check that
// fails says nothing and leaves the baseline alone.
export function UpdateNotice({ reload = () => { window.location.reload(); } }: { readonly reload?: () => void } = {}) {
  const [updated, setUpdated] = useState(false);
  useEffect(() => {
    let live = true;
    let baseline: string | null = null;
    const check = async () => {
      const h = await api.serverHealth();
      if (!live || h === null || h.version === '') return;
      baseline ??= h.version;
      if (h.version !== baseline) setUpdated(true);
    };
    void check();
    const timer = window.setInterval(() => void check(), updateCheckInterval);
    const visible = () => {
      if (document.visibilityState === 'visible') void check();
    };
    document.addEventListener('visibilitychange', visible);
    return () => {
      live = false;
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', visible);
    };
  }, []);
  if (!updated) return null;
  return (
    <div role="status" className="banner update-notice">
      <p>{updateText}</p>
      <button type="button" onClick={reload}>
        Reload
      </button>
    </div>
  );
}
