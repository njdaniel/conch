// Shown when the saved-servers file cannot be read (issue #945). Nothing has
// been changed; the one action sets that file aside and starts an empty list.
import { useState } from 'react';

import { bridge } from '../access';
import type { ProfilesView } from '../bridge';

export function Recovery({ onView }: { readonly onView: (view: ProfilesView) => void }) {
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);

  async function recover(): Promise<void> {
    const b = bridge();
    if (b === undefined || busy) return;
    setBusy(true);
    setFailed(false);
    try {
      onView(await b.profilesRecover());
    } catch {
      setFailed(true);
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="signin">
      <h1>Conch</h1>
      <h2>The list of servers could not be read</h2>
      <p className="hint">
        The file that holds your saved servers is damaged or from a newer version of the app. Nothing has been changed or deleted. You can start an empty
        list: the damaged file is kept beside it, renamed, and you add your servers again and sign in again.
      </p>
      <p role="alert" className="notice">
        {failed ? 'The desktop app could not start a new list.' : ''}
      </p>
      <button
        type="button"
        disabled={busy}
        onClick={() => {
          void recover();
        }}
      >
        Start an empty list
      </button>
    </main>
  );
}
