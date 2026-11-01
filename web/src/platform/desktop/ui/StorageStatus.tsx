// Whether sign-ins are kept across restarts, and a Retry that re-checks.
import { useState } from 'react';

import { bridge } from '../access';
import type { ProfilesView } from '../bridge';
import { storageLine } from './text';

export function StorageStatus({ view, onView }: { readonly view: ProfilesView; readonly onView: (view: ProfilesView) => void }) {
  const [busy, setBusy] = useState(false);
  const kept = view.storage.kind === 'protected';

  async function retry(): Promise<void> {
    const b = bridge();
    if (b === undefined || busy) return;
    setBusy(true);
    try {
      onView(await b.storageRetry());
    } catch {
      // The status stays as it was.
    } finally {
      setBusy(false);
    }
  }

  return (
    <p className="desktop-storage" role="status" aria-label="Sign-in storage">
      <span>{storageLine(view.storage, view.platform)}</span>
      {kept ? null : (
        <button
          type="button"
          disabled={busy}
          onClick={() => {
            void retry();
          }}
        >
          Retry
        </button>
      )}
    </p>
  );
}
