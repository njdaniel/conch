import { useEffect, useRef } from 'react';

// ADR-007: an operator credential must not become the browser's default.
// Issue #714 (design §5.3): a pasted operator credential is a script's, and
// never stands in for an operator's sign-in, a password and a code: the
// warning says so, and the page takes nothing it does as one (operatorSession
// in state/auth.ts tells them apart).
export function OperatorWarning({
  name,
  onContinue,
  onSignOut,
}: {
  readonly name: string;
  readonly onContinue: () => void;
  readonly onSignOut: () => void;
}) {
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => {
    heading.current?.focus();
  }, []);

  return (
    <main className="signin" aria-labelledby="operator-heading">
      <h1 id="operator-heading" ref={heading} tabIndex={-1}>
        This is an operator credential
      </h1>
      <p>
        You are signed in as <strong>{name}</strong>, an operator. An operator credential can administer the whole
        server, and a browser is not a safe place to keep one. Use an ordinary member credential in a browser.
      </p>
      <p>
        It is a script’s credential, not an operator’s sign-in: no password or code was checked. An operator signs in on
        the sign-in page with their name, their password and a code from their authenticator app, and their sign-in ends
        after twelve hours, or an hour without use. A script’s credential does not.
      </p>
      <div className="actions">
        <button type="button" onClick={onContinue}>
          Continue anyway
        </button>
        <button type="button" onClick={onSignOut}>
          Sign out
        </button>
      </div>
    </main>
  );
}
