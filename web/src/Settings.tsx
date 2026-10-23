// Settings → Password (issue #714, design §5.2, §6.1): a member changes
// their password with the current one, and an operator with a code from
// their authenticator as well; a member from before passwords sets a first
// one with the enrollment code an operator gave them. The reset landing page
// (SignInLink.tsx) takes its form from here too.
//
// Passwords and codes stay in their fields, never in the page's state, and
// go only in a request body; a field is emptied once what it held is sent.
// No form here is shown on a page that is not served over HTTPS (beyond this
// machine): what it would send could be read on the way.
//
// What the page cannot do, it says: there is no reset a person can do
// themselves. A member's forgotten password comes back through a reset link
// an operator sends after checking who they are; an operator's password and
// authenticator are recovered only on the server's host.
import { useEffect, useId, useRef, useState, type ReactNode, type RefObject, type SyntheticEvent } from 'react';
import * as api from './api';
import { minPassword, passwordProblem } from './CreateAccount';
import { serverLine, waitWords } from './Invite';
import type { WhoAmIResponseV1 } from './schema/principal';
import { typedCode } from './SignIn';
import { insecureLine } from './transport';

// The limits of recovery, said wherever a password is.
export const memberRecoveryLine =
  'Forgot your password? There is no reset you can do yourself: an operator checks that it is you, then sends you a link to choose a new one.';
export const operatorRecoveryLine =
  'An operator’s password and authenticator are recovered only on the server’s host (conchd operator recover). No page can reset them.';

type Line = { readonly text: string; readonly refused: boolean };

// The words for a refusal every password write shares.
function passwordRefusal(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string; readonly retryAfter?: number }, wrong: string): string {
  if (result.failure === 'network') return 'No answer from the server, so the page cannot tell whether the password changed. If the old one stops working, use the new one.';
  switch (result.code) {
    case 'auth_failed':
      return wrong;
    case 'password_unacceptable':
      return 'That password is too common, or has your name in it. Choose another.';
    case 'invalid_request':
      return serverLine(result.message, 'The server did not take that password.');
    case 'rate_limited':
      return `Too many tries. Try again in ${waitWords(result.retryAfter)}.`;
    case 'https_required':
      return insecureLine;
    default:
      return 'The server refused. Your password is as it was.';
  }
}

// NewPassword is the new password, typed twice. The caller reads the two
// fields by their refs when the form is sent.
export function NewPassword({
  chosen,
  again,
  label = 'New password',
}: {
  readonly chosen: RefObject<HTMLInputElement | null>;
  readonly again: RefObject<HTMLInputElement | null>;
  readonly label?: string;
}) {
  const id = useId();
  return (
    <>
      <label htmlFor={`${id}-new`}>{label}</label>
      <p id={`${id}-help`} className="hint">
        At least {minPassword} characters; a few words with spaces between them work well.
      </p>
      <input id={`${id}-new`} ref={chosen} type="password" autoComplete="new-password" aria-describedby={`${id}-help`} />
      <label htmlFor={`${id}-again`}>Type it again</label>
      <input id={`${id}-again`} ref={again} type="password" autoComplete="new-password" />
    </>
  );
}

// emptied clears every field it is given.
function emptied(...fields: readonly RefObject<HTMLInputElement | null>[]): void {
  for (const field of fields) if (field.current !== null) field.current.value = '';
}

export function PasswordSettings({
  who,
  insecure,
  onChanged,
  onUnauthorized,
}: {
  readonly who: WhoAmIResponseV1;
  readonly insecure: boolean;
  // The password is set or changed: whoami says something new.
  readonly onChanged: () => void;
  readonly onUnauthorized: () => void;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  const currentField = useRef<HTMLInputElement>(null);
  const codeField = useRef<HTMLInputElement>(null);
  const proofField = useRef<HTMLInputElement>(null);
  const chosen = useRef<HTMLInputElement>(null);
  const again = useRef<HTMLInputElement>(null);
  const operator = who.role === 'operator';
  const hasPassword = who.auth?.password_set === true;

  // Closing the section forgets whatever was typed in it.
  useEffect(() => {
    if (!open) emptied(currentField, codeField, proofField, chosen, again);
  }, [open]);

  function check(): { readonly chosen: string } | null {
    const said = passwordProblem(chosen.current?.value ?? '', again.current?.value ?? '');
    if (said !== null) {
      emptied(again);
      setLine({ text: said, refused: true });
      chosen.current?.focus();
      return null;
    }
    return { chosen: chosen.current?.value ?? '' };
  }

  function settle(result: api.PasswordResult, done: string, wrong: string) {
    setBusy(false);
    if (result.ok) {
      emptied(currentField, codeField, proofField, chosen, again);
      setLine({ text: done, refused: false });
      onChanged();
      return;
    }
    if (result.failure === 'unauthorized' || result.failure === 'unkept') {
      onUnauthorized();
      return;
    }
    emptied(currentField, codeField);
    setLine({ text: passwordRefusal(result, wrong), refused: true });
  }

  function change(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    const current = currentField.current?.value ?? '';
    if (current === '') {
      setLine({ text: 'Type your current password.', refused: true });
      currentField.current?.focus();
      return;
    }
    let code: string | undefined;
    if (operator) {
      const typed = typedCode(codeField.current?.value ?? '');
      if (typed === null) {
        setLine({ text: `Type the ${String(api.codeDigits)} digits your authenticator app shows.`, refused: true });
        codeField.current?.focus();
        return;
      }
      code = typed;
    }
    const next = check();
    if (next === null) return;
    setBusy(true);
    setLine(null);
    void api.changePassword(current, next.chosen, code).then((result) => {
      settle(
        result,
        'Your password is changed. Every other sign-in of yours has ended; this one goes on.',
        operator ? 'The current password or the code is not right.' : 'The current password is not right.',
      );
    });
  }

  function setFirst(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    const proof = (proofField.current?.value ?? '').trim();
    if (proof === '') {
      setLine({ text: 'Paste the enrollment code an operator gave you.', refused: true });
      proofField.current?.focus();
      return;
    }
    const next = check();
    if (next === null) return;
    setBusy(true);
    setLine(null);
    void api.setPassword(next.chosen, proof).then((result) => {
      if (!result.ok && result.failure !== 'unkept' && result.failure !== 'network' && result.code === 'sign_in_link_invalid') {
        setBusy(false);
        emptied(proofField);
        setLine({ text: 'That enrollment code does not work. Each works once, for a short time: ask an operator for a new one.', refused: true });
        return;
      }
      settle(result, 'Your password is set. From now on you sign in with your name and this password.', 'The server refused.');
    });
  }

  let body: ReactNode;
  if (insecure) {
    body = <p className="notice insecure">{insecureLine}</p>;
  } else if (hasPassword) {
    body = (
      <form aria-label="Change your password" onSubmit={change}>
        <label htmlFor={`${id}-current`}>Current password</label>
        <input id={`${id}-current`} ref={currentField} type="password" autoComplete="current-password" />
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
            />
          </>
        ) : null}
        <NewPassword chosen={chosen} again={again} />
        <p className="hint">Changing it ends every other sign-in of yours, on every device.</p>
        <button type="submit" aria-busy={busy}>
          {busy ? 'Changing…' : 'Change password'}
        </button>
      </form>
    );
  } else if (operator) {
    body = <p className="hint">An operator’s password is set on the server’s host, with a setup token: conchd operator setup.</p>;
  } else {
    body = (
      <form aria-label="Set up a password" onSubmit={setFirst}>
        <p className="hint">
          Your account has no password yet. Ask an operator for an enrollment code, then choose a password: from then on you
          sign in with your name and it.
        </p>
        <label htmlFor={`${id}-proof`}>Enrollment code from an operator</label>
        <input
          id={`${id}-proof`}
          ref={proofField}
          type="password"
          autoComplete="off"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
        />
        <NewPassword chosen={chosen} again={again} label="Choose a password" />
        <button type="submit" aria-busy={busy}>
          {busy ? 'Setting…' : 'Set password'}
        </button>
      </form>
    );
  }

  return (
    <div className="password-settings">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-panel`}
        onClick={() => {
          setOpen(!open);
          setLine(null);
        }}
      >
        Password
      </button>
      <div id={`${id}-panel`} role="region" aria-label="Password" hidden={!open} className="password-panel">
        {body}
        <p className="hint">{operator ? operatorRecoveryLine : memberRecoveryLine}</p>
        <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
          {line?.text ?? ''}
        </p>
      </div>
    </div>
  );
}

// ResetPassword is the form of a reset link's landing page: a new password
// for the account the link names, typed twice. The caller sends it with the
// link's code.
export function ResetPassword({
  name,
  busy,
  problem,
  onSubmit,
}: {
  readonly name: string;
  readonly busy: boolean;
  // The server's refusal of the last try, in words, or null.
  readonly problem: string | null;
  readonly onSubmit: (password: string) => void;
}) {
  const chosen = useRef<HTMLInputElement>(null);
  const again = useRef<HTMLInputElement>(null);
  const [local, setLocal] = useState<string | null>(null);

  // A refused password is typed afresh.
  useEffect(() => {
    if (problem === null) return;
    emptied(chosen, again);
    chosen.current?.focus();
  }, [problem]);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    const typed = chosen.current?.value ?? '';
    const said = passwordProblem(typed, again.current?.value ?? '');
    setLocal(said);
    if (said !== null) {
      emptied(again);
      chosen.current?.focus();
      return;
    }
    onSubmit(typed);
  }

  return (
    <form className="create-account" aria-label="Choose a new password" onSubmit={submit}>
      <p>
        Choose a new password for <strong>{name}</strong>. It replaces the old one, and every sign-in of the account ends:
        then sign in with the new one.
      </p>
      <NewPassword chosen={chosen} again={again} />
      <button type="submit" aria-busy={busy}>
        {busy ? 'Setting…' : 'Set the new password'}
      </button>
      <p role="alert" className="notice">
        {local ?? problem ?? ''}
      </p>
    </form>
  );
}
