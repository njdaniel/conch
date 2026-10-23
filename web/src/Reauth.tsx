// The question a recent-auth refusal asks (issue #714, design §5.3, §6.2):
// an action that needs a recent proof of the password (and an operator's
// code) was answered reauth_required. The page asks here, over whatever the
// person was doing, which stays as it was underneath: the form they filled
// in keeps what they typed, and once the proof stands the same request is
// sent again, once (api.ts authedSend). Cancel leaves the action undone and
// the form as it was.
//
// The password and the code stay in their fields, never in the page's
// state, and go in the proof's body alone. On a page not served over HTTPS
// (beyond this machine) no field is shown: anything answering on such an
// address could ask for the password this way, and it would cross the
// network in clear.
import { useEffect, useId, useRef, useState, type KeyboardEvent, type SyntheticEvent } from 'react';
import * as api from './api';
import { waitWords } from './Invite';
import { typedCode } from './SignIn';
import { insecureLine } from './transport';

export function Reauth({
  operator,
  insecure,
  onDone,
  onUnauthorized,
}: {
  // An operator proves a code with the password.
  readonly operator: boolean;
  readonly insecure: boolean;
  // Whether a proof now stands: true sends the action again, false leaves it.
  readonly onDone: (proved: boolean) => void;
  // The sign-in itself has ended (a 401 that is not a wrong password).
  readonly onUnauthorized: () => void;
}) {
  const id = useId();
  const heading = useRef<HTMLHeadingElement>(null);
  const passwordField = useRef<HTMLInputElement>(null);
  const codeField = useRef<HTMLInputElement>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  // A sign-in that can hold no proof (a script's credential): nothing to
  // type would help.
  const [stuck, setStuck] = useState(false);

  useEffect(() => {
    if (insecure) heading.current?.focus();
    else passwordField.current?.focus();
  }, [insecure]);

  function empty() {
    if (passwordField.current !== null) passwordField.current.value = '';
    if (codeField.current !== null) codeField.current.value = '';
  }

  function send(event: SyntheticEvent) {
    event.preventDefault();
    if (busy || stuck) return;
    const password = passwordField.current?.value ?? '';
    let code: string | undefined;
    if (password === '') {
      setLine('Type your password.');
      passwordField.current?.focus();
      return;
    }
    if (operator) {
      const typed = typedCode(codeField.current?.value ?? '');
      if (typed === null) {
        setLine(`Type the ${String(api.codeDigits)} digits your authenticator app shows.`);
        codeField.current?.focus();
        return;
      }
      code = typed;
    }
    empty();
    setBusy(true);
    setLine(null);
    void api.reauth(password, code).then((result) => {
      setBusy(false);
      if (result.ok) {
        onDone(true);
        return;
      }
      if (result.failure === 'unauthorized') {
        // The sign-in itself has ended; the page goes back to sign-in.
        onDone(false);
        onUnauthorized();
        return;
      }
      if (result.failure === 'network') {
        setLine('Cannot reach the server. Try again.');
      } else if (result.code === 'auth_failed') {
        setLine(operator ? 'The password or the code is not right.' : 'The password is not right.');
      } else if (result.code === 'rate_limited') {
        setLine(`Too many tries. Try again in ${waitWords(result.retryAfter)}.`);
      } else if (result.code === 'password_not_set') {
        setStuck(true);
        setLine('Your account has no password yet, so this cannot be proved. Set one up under Password in Settings first.');
      } else if (result.code === 'forbidden') {
        setStuck(true);
        setLine('This browser is signed in with a pasted credential, which cannot prove a password. Sign out, then sign in with your name and password to do this.');
      } else {
        setLine('The server refused. Try again.');
      }
      passwordField.current?.focus();
    });
  }

  function onKey(event: KeyboardEvent) {
    if (event.key !== 'Escape' || event.defaultPrevented || busy) return;
    event.preventDefault();
    onDone(false);
  }

  return (
    <div className="reauth-layer">
      <div className="backdrop" />
      <div role="dialog" aria-modal="true" aria-labelledby={`${id}-heading`} className="reauth" onKeyDown={onKey}>
        <h2 id={`${id}-heading`} ref={heading} tabIndex={-1}>
          Confirm it is you
        </h2>
        {insecure ? (
          <p className="notice insecure">{insecureLine}</p>
        ) : (
          <form onSubmit={send}>
            <p>
              {operator
                ? 'This action needs your password and a code from your authenticator app, given in the last ten minutes. What you were doing is kept, and goes ahead once you confirm.'
                : 'This action needs your password, given in the last ten minutes. What you were doing is kept, and goes ahead once you confirm.'}
            </p>
            <label htmlFor={`${id}-password`}>Your password</label>
            <input id={`${id}-password`} ref={passwordField} type="password" autoComplete="current-password" disabled={stuck} />
            {operator ? (
              <>
                <label htmlFor={`${id}-code`}>Code from your authenticator app</label>
                <input
                  id={`${id}-code`}
                  ref={codeField}
                  type="text"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  disabled={stuck}
                />
              </>
            ) : null}
            <div className="actions">
              <button type="submit" disabled={stuck} aria-busy={busy}>
                {busy ? 'Checking…' : 'Confirm'}
              </button>
            </div>
          </form>
        )}
        <p role="alert" className="notice">
          {line ?? ''}
        </p>
        <div className="actions">
          <button
            type="button"
            disabled={busy}
            onClick={() => {
              onDone(false);
            }}
          >
            Cancel
          </button>
        </div>
      </div>
    </div>
  );
}
