// Create an account (issue #713, ADR-009 D3, design §3.1, §3.3): a permanent
// name and a password, together. The same form makes an account from the
// bare address, through an invite, and from a live guest visit. The caller
// sends it and says how the server answered; this form only checks what can
// be checked without the server (the passwords match, the length), which is
// no substitute for the server's rules. The passwords stay in their fields,
// never in the page's state, and the page sends them only in a request body.
import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';

// The password rules the server holds (pkg/schema ValidatePassword), in
// characters: said beside the field and checked before sending, so a person
// learns of a short one without a round trip.
const minPassword = 15;
const maxPassword = 128;

// Which field a refusal of the server's is about, so focus goes there.
export type ProblemField = 'name' | 'password';

export interface AccountProblem {
  readonly line: string;
  readonly field?: ProblemField;
}

export function CreateAccount({
  purpose,
  busy,
  problem,
  onSubmit,
  onCancel,
}: {
  // join is the bare address, invite an invite's page, convert a guest's.
  readonly purpose: 'join' | 'invite' | 'convert';
  readonly busy: boolean;
  // The server's refusal of the last try, in words, or null.
  readonly problem: AccountProblem | null;
  readonly onSubmit: (name: string, password: string) => void;
  // Leaving the form changes nothing on the server.
  readonly onCancel?: () => void;
}) {
  const id = useId();
  const [name, setName] = useState('');
  const [local, setLocal] = useState<AccountProblem | null>(null);
  const nameField = useRef<HTMLInputElement>(null);
  const password = useRef<HTMLInputElement>(null);
  const again = useRef<HTMLInputElement>(null);
  const shown = local ?? problem;

  // After a refusal, focus goes to the field it is about; a password that
  // was refused is emptied, so the next one is typed afresh.
  useEffect(() => {
    if (problem === null) return;
    if (problem.field === 'password') {
      if (password.current !== null) password.current.value = '';
      if (again.current !== null) again.current.value = '';
      password.current?.focus();
    } else {
      nameField.current?.focus();
    }
  }, [problem]);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    const chosen = password.current?.value ?? '';
    const repeated = again.current?.value ?? '';
    // In code points after NFC, as the server counts (ValidatePassword).
    const length = Array.from(chosen.normalize('NFC')).length;
    let refused: AccountProblem | null = null;
    if (name === '') refused = { line: 'Choose a name.', field: 'name' };
    else if (length < minPassword) refused = { line: `Choose a password of at least ${String(minPassword)} characters.`, field: 'password' };
    else if (length > maxPassword) refused = { line: `Choose a password of at most ${String(maxPassword)} characters.`, field: 'password' };
    else if (chosen !== repeated) refused = { line: 'The two passwords are not the same. Type them again.', field: 'password' };
    setLocal(refused);
    if (refused !== null) {
      if (refused.field === 'password') {
        if (again.current !== null) again.current.value = '';
        password.current?.focus();
      } else {
        nameField.current?.focus();
      }
      return;
    }
    // What was typed is sent as it is: the server decides whether it is a
    // name, and whether the password is good enough.
    onSubmit(name, chosen);
  }

  const submitLabel = purpose === 'convert' ? 'Create my account' : 'Create account';
  return (
    <form className="create-account" onSubmit={submit}>
      <label htmlFor={`${id}-name`}>Your name</label>
      <p id={`${id}-name-help`} className="hint">
        Your permanent name: others see it, and you sign in with it. Up to 100 characters, with no space at the start or end
        and no invisible characters. Names that begin with “guest” are kept for guests. The server says if it is taken.
      </p>
      <input
        id={`${id}-name`}
        ref={nameField}
        type="text"
        value={name}
        autoComplete="username"
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
        aria-describedby={`${id}-name-help ${id}-problem`}
        aria-invalid={shown?.field === 'name'}
        onChange={(event) => {
          setName(event.target.value);
        }}
      />
      <label htmlFor={`${id}-password`}>Choose a password</label>
      <p id={`${id}-password-help`} className="hint">
        At least {minPassword} characters; a few words with spaces between them work well. You cannot reset it yourself: if
        you forget it, an operator can send you a link to choose a new one.
      </p>
      <input
        id={`${id}-password`}
        ref={password}
        type="password"
        autoComplete="new-password"
        aria-describedby={`${id}-password-help ${id}-problem`}
        aria-invalid={shown?.field === 'password'}
      />
      <label htmlFor={`${id}-again`}>Type the password again</label>
      <input id={`${id}-again`} ref={again} type="password" autoComplete="new-password" aria-invalid={shown?.field === 'password'} />
      <button type="submit" aria-busy={busy}>
        {busy ? 'Creating…' : submitLabel}
      </button>
      <p id={`${id}-problem`} role="alert" className="notice">
        {shown?.line ?? ''}
      </p>
      {onCancel === undefined ? null : (
        <button type="button" onClick={onCancel}>
          {purpose === 'convert' ? 'Cancel' : 'Go back'}
        </button>
      )}
    </form>
  );
}
