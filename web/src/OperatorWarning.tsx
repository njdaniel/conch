import { useEffect, useRef } from 'react';

// ADR-007: an operator credential must not become the browser's default.
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
