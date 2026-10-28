// Settings → Joining, for an operator (issue #882): one switch that pauses
// and resumes every invite, as `conch admin invite pause|resume` does
// (the invite settings route). A paused server lets no one make, preview or
// use an invite. Restoring a backup pauses them on purpose.
//
// The switch shows only what the server last said, never what was clicked,
// and nothing until it has said: a change is sent, and the page then shows
// the answer. Every read and change is numbered, and only the newest one's
// answer is shown. A read wanted while a change is in flight waits for it,
// since it could see the server from before the change. Only a refusal the
// server named (an error code) proves a change was not made: any other
// failure (no answer, or an answer cut off or unreadable) may have followed
// a change that was made, so the switch goes back to unknown and is read
// again.
//
// The change needs a recent proof of the operator's password (and code, if
// enrolled), which authedSend asks for over this page (Reauth.tsx) and then
// sends the same change again; a cancel leaves the switch as it was.
import { useCallback, useEffect, useId, useRef, useState } from 'react';
import * as api from './api';
import { serverLine } from './Invite';

type Line = { readonly text: string; readonly refused: boolean };

function refusalLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string }): string {
  if (result.code === 'reauth_required') return 'It needs your password first. Invites were not changed.';
  if (result.code === 'invalid_request') return serverLine(result.message, 'The server refused. Invites were not changed.');
  return 'The server refused. Invites were not changed.';
}

export function InviteSettings({ open, onUnauthorized }: { readonly open: boolean; readonly onUnauthorized: () => void }) {
  const id = useId();
  // What the server last reported; null while it is not known.
  const [paused, setPaused] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  // The number of the newest read or change; an older one's answer is dropped.
  const latest = useRef(0);
  // A change in flight, and whether a read waits for it to answer.
  const writing = useRef(false);
  const readAfterWrite = useRef(false);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const read = useCallback(
    (after: Line | null) => {
      const n = ++latest.current;
      void api.inviteSettings().then((result) => {
        if (!mounted.current || n !== latest.current) return;
        if (result.ok) {
          setPaused(result.value.paused);
          setLine(after);
        } else if (result.failure === 'unauthorized') onUnauthorized();
        else {
          setPaused(null);
          setLine({ text: result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read whether invites are paused.', refused: true });
        }
      });
    },
    [onUnauthorized],
  );

  // Read afresh each time the section opens; closing drops any read in flight.
  useEffect(() => {
    if (!open) return;
    setPaused(null);
    setLine(null);
    if (writing.current) readAfterWrite.current = true;
    else read(null);
    return () => {
      latest.current++;
    };
  }, [open, read]);

  function change(want: boolean) {
    if (busy || paused === null) return;
    const n = ++latest.current;
    writing.current = true;
    setBusy(true);
    setLine(null);
    void api.putInviteSettings({ paused: want }).then((result) => {
      writing.current = false;
      if (!mounted.current) return;
      setBusy(false);
      if (readAfterWrite.current) {
        // The section was opened again meanwhile: read what the change left.
        readAfterWrite.current = false;
        read(null);
        return;
      }
      if (n !== latest.current) return;
      if (result.ok) {
        setPaused(result.value.paused);
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      if (result.code === undefined) {
        // No refusal the server named: the change may have been made. Ask
        // the server, not the click.
        setPaused(null);
        read({
          text:
            result.failure === 'network'
              ? 'The server could not be reached, so it is not known whether invites changed. This is what it holds now.'
              : 'The server’s answer could not be read, so it is not known whether invites changed. This is what it holds now.',
          refused: true,
        });
        return;
      }
      // Refused, with the server's code: the switch keeps what it last reported.
      setLine({ text: refusalLine(result), refused: true });
    });
  }

  return (
    <div className="invite-settings">
      {paused === null ? (
        <p className="hint">{line === null ? 'Loading whether invites are paused…' : 'Whether invites are paused is not known.'}</p>
      ) : (
        <>
          <label>
            <input
              type="checkbox"
              checked={paused}
              disabled={busy}
              aria-describedby={`${id}-state`}
              onChange={(event) => {
                change(event.target.checked);
              }}
            />{' '}
            Pause all invites
          </label>
          <p id={`${id}-state`} className="hint">
            {paused
              ? 'Invites are paused: no one can make, preview or use an invite.'
              : 'Invites are on. Pausing stops every invite: making, previewing and using one.'}
          </p>
        </>
      )}
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
