// "Keep me signed in on this computer" on the desktop sign-in form (issue
// #2077). The choice is main's, for the window in force (ProfilesView.keep);
// beside it, what this computer can do with it: kept in the system keyring,
// or why it cannot be (no keyring unlocked, a keyring that protects nothing,
// a private folder that is not private), with Check again for the keyring
// (the server list's Retry, under its own name) and the help page. It is on by default where storage is protected, and cannot be
// turned on where it is not. An operator's sign-in is never kept, and the
// line says so. No password passes through here, and nothing is stored by the
// page.
import { useEffect, useState } from 'react';

import { bridge } from '../access';
import type { ProfilesView } from '../bridge';
import { keepLine, keepText } from './text';

export function KeepSignIn() {
  const [view, setView] = useState<ProfilesView | null>(null);
  const [busy, setBusy] = useState(false);
  // The person's click, shown at once until main's answer (its view) says
  // what the choice now is.
  const [clicked, setClicked] = useState<boolean | null>(null);

  useEffect(() => {
    let live = true;
    const b = bridge();
    if (b === undefined) return;
    void b.profiles().then(
      (first) => {
        if (live) setView(first);
      },
      () => undefined,
    );
    return () => {
      live = false;
    };
  }, []);

  async function ask(call: () => Promise<ProfilesView>): Promise<void> {
    if (busy) return;
    setBusy(true);
    try {
      setView(await call());
    } catch {
      // The choice and the status stay as main last said.
    } finally {
      setClicked(null);
      setBusy(false);
    }
  }

  if (view === null) {
    return (
      <p className="hint keep-signin" role="status">
        {keepText.checking}
      </p>
    );
  }
  const kept = view.storage.kind === 'protected';
  // Main names the help page (keepHelpFor in desktop/main/profiles/service.ts).
  const help = view.keepHelp;
  return (
    <div className="keep-signin">
      <label>
        <input
          type="checkbox"
          name="keep-signed-in"
          checked={kept && (clicked ?? view.keep)}
          disabled={!kept || busy}
          aria-describedby="keep-signin-status"
          onChange={(event) => {
            const keep = event.currentTarget.checked;
            const b = bridge();
            if (b === undefined || busy) return;
            setClicked(keep);
            void ask(() => b.profileKeep(keep));
          }}
        />
        {keepText.label}
      </label>
      <p id="keep-signin-status" className="hint" role="status" aria-label="Staying signed in">
        {keepLine(view.storage, view.platform)} {keepText.operator}
      </p>
      {kept ? null : (
        <button
          type="button"
          disabled={busy}
          onClick={() => {
            const b = bridge();
            if (b !== undefined) void ask(() => b.storageRetry());
          }}
        >
          {keepText.retry}
        </button>
      )}
      {help === null ? null : (
        <a className="hint" href={help} target="_blank" rel="noopener noreferrer">
          {keepText.help}
        </a>
      )}
    </div>
  );
}
