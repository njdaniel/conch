import { useEffect, useRef, type SyntheticEvent } from 'react';
import { ServerVersion } from './ServerVersion';
import { noticeText, type AuthNotice } from './state/auth';

// One field for a credential an operator issued. There is no username and no
// password reset: Conch has no password login.
export function SignIn({
  origin,
  notice,
  checking,
  onSubmit,
  onForget,
}: {
  readonly origin: string;
  readonly notice: AuthNotice | null;
  readonly checking: boolean;
  readonly onSubmit: (credential: string) => void;
  // Forgetting a stored sign-in that could not be checked (issue #469),
  // offered only when signing in was refused because of one.
  readonly onForget: () => void;
}) {
  const field = useRef<HTMLInputElement>(null);

  // After a refusal the field gets focus back, so the next try needs no mouse.
  useEffect(() => {
    if (notice !== null) field.current?.focus();
  }, [notice]);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    const input = field.current;
    if (input === null || checking) return;
    const credential = input.value.trim();
    // The field is emptied at once: the credential is held by the caller, not
    // by the page.
    input.value = '';
    if (credential === '') {
      input.focus();
      return;
    }
    onSubmit(credential);
  }

  return (
    <main className="signin">
      <h1>Conch</h1>
      <p>
        Server: <code>{origin}</code>
      </p>
      <form onSubmit={submit}>
        <label htmlFor="credential">Credential</label>
        <p id="credential-help" className="hint">
          Paste the credential an operator issued to you.
        </p>
        <input
          id="credential"
          ref={field}
          type="password"
          autoComplete="off"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          aria-describedby="credential-help"
        />
        <button type="submit" aria-busy={checking}>
          {checking ? 'Checking…' : 'Sign in'}
        </button>
        <p role="alert" className="notice">
          {notice === null ? '' : noticeText(notice)}
        </p>
        {notice === 'held' && (
          <button type="button" onClick={onForget}>
            Forget the stored sign-in
          </button>
        )}
      </form>
      <ServerVersion className="hint" />
    </main>
  );
}
