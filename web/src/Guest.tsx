// Temporary guests in the browser (issue #713, ADR-009 D2, design §3.3,
// §3.4): the visit's own panel in the lobby, which says who the guest is shown
// as and until when, and offers Create an account and End visit. No form
// starts a visit: guests are removed for now (issue #1005), and a visit
// already live is all this serves. A guest's credential is this tab's alone
// (src/api.ts); nothing here sees it.
import { useEffect, useRef, useState } from 'react';
import type { ConvertResult, InviteRefusal } from './api';
import { CreateAccount, type AccountProblem } from './CreateAccount';
import { guestName } from './guestName';
import { accountRefusal, refusalLine } from './Invite';
import type { GuestV1 } from './schema/guest';
import { insecureLine } from './transport';

// conversionRefusal is the plain words for a Create an account the server
// refused during a visit. The guest is as it was.
function conversionRefusal(result: InviteRefusal | { readonly ok: false; readonly failure: 'unconfirmed' }): AccountProblem {
  if (result.failure === 'unconfirmed') {
    return { line: 'The server did not answer about your account. You are still a guest: try again in a moment.' };
  }
  switch (result.code) {
    case 'conversion_not_allowed':
      return { line: 'Accounts cannot be made from this visit now. An operator can allow it.' };
    case 'joining_paused':
      return { line: 'Joining is paused on this server for now, so accounts cannot be made. Try again later.' };
    case 'invites_paused':
      return { line: 'Invites are paused on this server for now, so this visit cannot become an account. Try again later.' };
    default:
      return accountRefusal(result) ?? { line: refusalLine(result) };
  }
}

type Ending = 'idle' | 'asking' | 'ending' | 'unconfirmed';

// GuestVisit is the visit's panel in the lobby. Nothing in it changes what
// the page shows of the guest until the server has answered: a cancel or a
// refusal leaves the visit as it was.
export function GuestVisit({
  guest,
  insecure = false,
  onConvert,
  onEnd,
  onForget,
}: {
  readonly guest: GuestV1;
  // The page is on plain HTTP beyond this machine (needsHttps): Create an
  // account, which takes a password, is not offered, and the page says why.
  readonly insecure?: boolean;
  // Create an account. The page starts over as the member on success; this
  // panel is told the rest.
  readonly onConvert: (name: string, password: string) => Promise<ConvertResult>;
  // End visit: ended means the server confirmed it, and the page signs out.
  readonly onEnd: () => Promise<'ended' | 'unconfirmed'>;
  // Forget the visit in this tab without the server's word.
  readonly onForget: () => void;
}) {
  const [creating, setCreating] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<AccountProblem | null>(null);
  const [ending, setEnding] = useState<Ending>('idle');
  const cancel = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (ending === 'asking') cancel.current?.focus();
  }, [ending]);

  const canCreate = guest.can_convert && !insecure;

  function convert(name: string, password: string) {
    if (busy || !canCreate) return;
    setBusy(true);
    setProblem(null);
    void onConvert(name, password).then((result) => {
      setBusy(false);
      // A success, a visit that is over or an account not kept: the page
      // leaves this panel behind.
      if (result.ok || result.failure === 'gone' || result.failure === 'unkept') return;
      setProblem(conversionRefusal(result));
    });
  }

  function end() {
    setEnding('ending');
    void onEnd().then((outcome) => {
      if (outcome === 'unconfirmed') setEnding('unconfirmed');
    });
  }

  const expires = new Date(guest.expires_at);
  return (
    <section aria-label="Guest visit" className="guest-visit">
      <p className="guest-who">
        <strong>Temporary guest</strong> · <span className="guest-name">{guestName(guest)}</span>
      </p>
      <p className="hint">
        End visit signs you out permanently. Your messages remain. Create an account to keep access. The visit ends by{' '}
        <time dateTime={guest.expires_at}>{expires.toLocaleString()}</time> at the latest, or sooner after a while without
        activity. Closing the tab does not end it.
      </p>
      {creating && canCreate ? (
        <div className="guest-create">
          <h2>Create an account</h2>
          <p className="hint">
            Your account keeps this visit’s conversations and nothing more. Your messages so far stay shown as {guestName(guest)}.
          </p>
          <CreateAccount
            purpose="convert"
            busy={busy}
            problem={problem}
            onSubmit={convert}
            onCancel={() => {
              if (busy) return;
              setCreating(false);
              setProblem(null);
            }}
          />
        </div>
      ) : ending === 'idle' ? (
        <div className="actions">
          {canCreate ? (
            <button
              type="button"
              onClick={() => {
                setCreating(true);
              }}
            >
              Create an account
            </button>
          ) : null}
          <button
            type="button"
            onClick={() => {
              setEnding('asking');
            }}
          >
            End visit
          </button>
        </div>
      ) : ending === 'asking' || ending === 'ending' ? (
        <div role="group" aria-labelledby="end-visit-question" className="signout-confirm">
          <p id="end-visit-question">End your visit? You cannot come back as this guest. Your messages remain.</p>
          <div className="actions">
            <button type="button" className="danger" onClick={end} disabled={ending === 'ending'} aria-busy={ending === 'ending'}>
              {ending === 'ending' ? 'Ending…' : 'Yes, end visit'}
            </button>
            <button
              ref={cancel}
              type="button"
              disabled={ending === 'ending'}
              onClick={() => {
                setEnding('idle');
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      ) : (
        <div className="guest-unconfirmed">
          <p role="alert" className="notice">
            The server did not confirm that your visit ended, so it may still be on. Try again; it ends on its own by{' '}
            {expires.toLocaleString()} at the latest.
          </p>
          <div className="actions">
            <button type="button" onClick={end}>
              Try again
            </button>
            <button type="button" onClick={onForget}>
              Forget it in this tab
            </button>
          </div>
        </div>
      )}
      {insecure ? (
        <p className="notice insecure">{insecureLine}</p>
      ) : !guest.can_convert && !creating ? (
        <p className="hint">Accounts cannot be made from this visit now.</p>
      ) : null}
    </section>
  );
}
