// "Add from an invite link" (issue #1591). The person pastes an invite link;
// main parses it and answers the server it names, and the saved servers for
// it. Nothing is contacted, added, switched or signed in until a click on one
// of the choices below. The pasted text is cleared from the field as soon as
// main has it, and the invite's code is never in this page: only a handle.
import { useId, useRef, useState, type SyntheticEvent } from 'react';

import { inviteBridge } from '../access';
import type { InviteCheck } from '../bridge';
import { takeInvite } from '../invite';
import { inviteAcceptLines, inviteRefusalLines } from './text';

type Checked = Extract<InviteCheck, { ok: true }>;

export function InviteLink() {
  const [checked, setChecked] = useState<Checked | null>(null);
  const [line, setLine] = useState('');
  const [busy, setBusy] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  const id = useId();
  const available = inviteBridge()?.inviteCheck !== undefined;

  async function check(event: SyntheticEvent): Promise<void> {
    event.preventDefault();
    const b = inviteBridge();
    const pasted = field.current?.value ?? '';
    if (b?.inviteCheck === undefined || busy) return;
    if (pasted.trim() === '') {
      field.current?.focus();
      return;
    }
    setBusy(true);
    setLine('');
    try {
      const answer = await b.inviteCheck(pasted);
      // Main has it; the field no longer holds the link.
      if (field.current !== null) field.current.value = '';
      if (answer.ok) setChecked(answer);
      else setLine(inviteRefusalLines[answer.refusal]);
    } catch {
      setLine('The desktop app could not read that link.');
    } finally {
      setBusy(false);
    }
  }

  async function open(target: { readonly profile: string } | { readonly add: true }): Promise<void> {
    const b = inviteBridge();
    if (b?.inviteAccept === undefined || checked === null || busy) return;
    setBusy(true);
    setLine('');
    try {
      const answer = await b.inviteAccept(checked.handle, target);
      setChecked(null);
      if (!answer.ok) {
        setLine(inviteAcceptLines[answer.refusal]);
      } else if (answer.switched) {
        // Main replaces this page with the profile's, which takes the code.
        // The promise may not settle for the page that asked.
        setLine('Opening…');
      } else if (!(await takeInvite())) {
        setLine(inviteAcceptLines.expired);
      }
    } catch {
      setLine('The desktop app could not open that invite.');
    } finally {
      setBusy(false);
    }
  }

  function cancel(): void {
    const b = inviteBridge();
    const handle = checked?.handle;
    setChecked(null);
    setLine('');
    if (b?.inviteCancel !== undefined && handle !== undefined) void b.inviteCancel(handle).catch(() => undefined);
  }

  if (!available) return null;
  return (
    <section className="desktop-invite" aria-label="Add from an invite link">
      <h3>Add from an invite link</h3>
      {checked === null ? (
        <form
          className="desktop-invite-form"
          onSubmit={(event) => {
            void check(event);
          }}
        >
          <label htmlFor={id}>Invite link</label>
          <input id={id} ref={field} type="text" autoComplete="off" spellCheck={false} />
          <button type="submit" disabled={busy}>
            Check link
          </button>
          <p className="hint">Paste the link you were sent. The app shows which server it is for; nothing is contacted until you choose.</p>
        </form>
      ) : (
        <div className="desktop-invite-choice" role="group" aria-label="Open this invite">
          <p>
            This invite is for <strong>{checked.origin}</strong>.
          </p>
          {checked.voice ? <p role="status">Voice is running. Opening the invite on a different server leaves voice.</p> : null}
          {checked.profiles.length === 0 ? (
            <p>No saved server is for this address. Adding it saves a new server entry, then opens the invite there.</p>
          ) : (
            <ul>
              {checked.profiles.map((profile) => (
                <li key={profile.id}>
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => {
                      void open({ profile: profile.id });
                    }}
                  >
                    Open with {profile.label} ({profile.account === null ? 'not signed in yet' : `signed in as ${profile.account}`})
                  </button>
                </li>
              ))}
            </ul>
          )}
          <button
            type="button"
            disabled={busy}
            onClick={() => {
              void open({ add: true });
            }}
          >
            {checked.profiles.length === 0 ? 'Add this server and open the invite' : 'Open as a new server entry'}
          </button>
          <button type="button" onClick={cancel}>
            Cancel
          </button>
        </div>
      )}
      <p role="alert" className="notice">
        {line}
      </p>
    </section>
  );
}
