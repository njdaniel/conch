// Settings → Joining, for an operator (issue #882): one switch that pauses
// and resumes every invite, as `conch admin invite pause|resume` does
// (the invite settings route). A paused server lets no one make, preview or
// use an invite. Restoring a backup pauses them on purpose.
//
// The switch shows what the server last said, never what was clicked: a
// change is sent, and the page then shows the answer. The change needs a
// recent proof of the operator's password (and code, if enrolled), which
// authedSend asks for over this page (Reauth.tsx) and then sends the same
// change again; a cancel leaves the switch as it was.
import { useEffect, useId, useState } from 'react';
import * as api from './api';
import { serverLine } from './Invite';

type Line = { readonly text: string; readonly refused: boolean };

function refusalLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string }): string {
  if (result.failure === 'network') return 'Cannot reach the server. Invites were not changed.';
  if (result.code === 'reauth_required') return 'It needs your password first. Invites were not changed.';
  if (result.code === 'invalid_request') return serverLine(result.message, 'The server refused. Invites were not changed.');
  return 'The server refused. Invites were not changed.';
}

export function InviteSettings({ open, onUnauthorized }: { readonly open: boolean; readonly onUnauthorized: () => void }) {
  const id = useId();
  // What the server last reported; null until it has.
  const [paused, setPaused] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);

  // Read afresh each time the section opens.
  useEffect(() => {
    if (!open) return;
    let current = true;
    void api.inviteSettings().then((result) => {
      if (!current) return;
      if (result.ok) {
        setPaused(result.value.paused);
        setLine(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setLine({ text: result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read whether invites are paused.', refused: true });
    });
    return () => {
      current = false;
    };
  }, [open, onUnauthorized]);

  function change(want: boolean) {
    if (busy || paused === null) return;
    setBusy(true);
    setLine(null);
    void api.putInviteSettings({ paused: want }).then((result) => {
      setBusy(false);
      if (result.ok) {
        setPaused(result.value.paused);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      // The switch keeps what the server last reported.
      setLine({ text: refusalLine(result), refused: true });
    });
  }

  return (
    <div className="invite-settings">
      <label>
        <input
          type="checkbox"
          checked={paused === true}
          disabled={paused === null || busy}
          aria-describedby={`${id}-state`}
          onChange={(event) => {
            change(event.target.checked);
          }}
        />{' '}
        Pause all invites
      </label>
      <p id={`${id}-state`} className="hint">
        {paused === null
          ? 'Loading…'
          : paused
            ? 'Invites are paused: no one can make, preview or use an invite.'
            : 'Invites are on. Pausing stops every invite: making, previewing and using one.'}
      </p>
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
