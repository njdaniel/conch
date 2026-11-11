// "Add from an invite link" (issue #1591). The person pastes an invite link;
// main parses it and answers the server it names, and the saved servers for
// it. Nothing is contacted, added, switched or signed in until a click on one
// of the choices below. The pasted text is cleared from the field as soon as
// main has it, and the invite's code is never in this page: only a handle.
//
// A conch:// link (issue #1592) reaches the same choice: main checks it as it
// checks a pasted one, and this page is handed the same answer, a handle and
// the server, never the link or the code. It asks main for it when it mounts
// and when main says one arrived (or, when ProfileBar holds the asking, is
// handed the answer). Nothing happens until the person's click.
import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';

import { inviteBridge } from '../access';
import type { InviteCheck } from '../bridge';
import { takeInvite } from '../invite';
import { inviteAcceptLines, inviteRefusalLines } from './text';

type Checked = Extract<InviteCheck, { ok: true }>;

// How long the choices stay disabled after a newer link replaces the one on
// show (issue #1592, security review S1): a click aimed at the old server must
// not land on the new one. Main enforces the same interval on the handle
// itself (SWAP_GUARD_MS in desktop/main/invites.ts), so this is the words and
// the disabled buttons, not the guard.
export const SWAP_GUARD_MS = 1_000;

interface Props {
  // A link that reached the app, already checked by main, handed down by the
  // strip that asks for it (./ProfileBar). Undefined: this page asks itself.
  readonly arrival?: Checked | null;
  // Told once the arrival has been taken up, so it is not taken up twice.
  readonly onArrivalTaken?: () => void;
  // Whether the active profile's client is on the page. On the "unsupported
  // release" page it is not: an invite for the active profile would be handed
  // to the address with nothing to read it, and the code would stay there.
  readonly clientMounted: boolean;
}

export function InviteLink({ clientMounted, arrival, onArrivalTaken }: Props) {
  const [checked, setChecked] = useState<Checked | null>(null);
  const [line, setLine] = useState('');
  const [busy, setBusy] = useState(false);
  // True for SWAP_GUARD_MS after a different link replaced the one on show.
  const [swapped, setSwapped] = useState(false);
  const swapTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const field = useRef<HTMLInputElement>(null);
  const id = useId();
  const available = inviteBridge()?.inviteCheck !== undefined;
  // The handle on show, so a newer link can spend it.
  const shown = useRef<string | null>(null);

  // adopt shows a checked link from main in place of any on show; the one
  // replaced is cancelled, so its code is not kept.
  function adopt(next: Checked): void {
    const b = inviteBridge();
    const old = shown.current;
    shown.current = next.handle;
    if (old !== null && old !== next.handle && b?.inviteCancel !== undefined) void b.inviteCancel(old).catch(() => undefined);
    setLine('');
    setChecked(next);
    if (swapTimer.current !== null) clearTimeout(swapTimer.current);
    swapTimer.current = null;
    if (old !== null && old !== next.handle) {
      // A different link replaced the one on show: hold the choices still for
      // the interval, so a click meant for the old one cannot act on this one.
      setSwapped(true);
      swapTimer.current = setTimeout(() => {
        swapTimer.current = null;
        setSwapped(false);
      }, SWAP_GUARD_MS);
    } else {
      setSwapped(false);
    }
  }

  // No timer outlives the page.
  useEffect(
    () => () => {
      if (swapTimer.current !== null) clearTimeout(swapTimer.current);
    },
    [],
  );

  // Asking for itself: once at mount (a cold start's link waited for this
  // page) and again whenever main says one arrived.
  const own = arrival === undefined;
  useEffect(() => {
    if (!own) return;
    const b = inviteBridge();
    if (b?.inviteArrived === undefined) return;
    let live = true;
    const ask = (): void => {
      void b.inviteArrived?.().then(
        (answer) => {
          if (live && answer?.ok === true) adopt(answer);
        },
        () => undefined,
      );
    };
    ask();
    const stop = b.onInviteArrived?.(ask);
    return () => {
      live = false;
      stop?.();
    };
    // adopt only reads a ref and sets state, so it is not a dependency.
  }, [own]);

  // Handed down: take it up once.
  useEffect(() => {
    if (arrival === undefined || arrival === null) return;
    adopt(arrival);
    onArrivalTaken?.();
  }, [arrival]);

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
      if (answer.ok) adopt(answer);
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
      if (!answer.ok && answer.refusal === 'just-arrived') {
        // Main did not spend the handle: the choice stays, and the person
        // chooses again once they have read the server.
        setLine(inviteAcceptLines['just-arrived']);
        return;
      }
      shown.current = null;
      setChecked(null);
      if (!answer.ok) {
        setLine(inviteAcceptLines[answer.refusal]);
      } else if (answer.switched) {
        // Main replaces this page with the profile's, which takes the code.
        // The promise may not settle for the page that asked.
        setLine('Opening…');
      } else if (!clientMounted) {
        // Not reachable from the buttons below; kept so that a code is never
        // written to the address when no client will read it.
        setLine(inviteAcceptLines.unsupported);
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
    shown.current = null;
    setChecked(null);
    setSwapped(false);
    setLine('');
    if (b?.inviteCancel !== undefined && handle !== undefined) void b.inviteCancel(handle).catch(() => undefined);
  }

  if (!available) return null;
  const choices = checked === null ? [] : checked.profiles.filter((p) => clientMounted || !p.active);
  const hidden = checked === null ? 0 : checked.profiles.length - choices.length;
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
          {swapped ? <p role="status">A new invite link arrived</p> : null}
          {checked.voice ? <p role="status">Voice is running. Opening the invite on a different server leaves voice.</p> : null}
          {hidden > 0 ? <p>This server’s release is not supported by this app, so the invite cannot be opened there. Open it as a new server entry instead.</p> : null}
          {checked.profiles.length === 0 ? (
            <p>No saved server is for this address. Adding it saves a new server entry, then opens the invite there.</p>
          ) : choices.length === 0 ? null : (
            <ul>
              {choices.map((profile) => (
                <li key={profile.id}>
                  <button
                    type="button"
                    disabled={busy || swapped}
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
            disabled={busy || swapped}
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
      {/* Only while there is something to say: an empty alert would be a second one on the address page. */}
      {line === '' ? null : (
        <p role="alert" className="notice">
          {line}
        </p>
      )}
    </section>
  );
}
