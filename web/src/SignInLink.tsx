// The landing page of a sign-in link (issue #493): whose account the link
// opens, then one button that signs into it. An operator makes the link for
// a person who signed out, so they come back as themselves, not as someone
// new. The code is a prop held in memory by App, sent only in the bodies of
// preview and redeem, and never rendered: nothing on this page shows it.
import { useEffect, useRef, useState } from 'react';
import * as api from './api';
import { waitWords } from './Invite';
import type { SignInLinkPreviewV1 } from './schema/signin';
import type { WhoAmIResponseV1 } from './schema/principal';
import { ServerVersion } from './ServerVersion';
import { noticeText } from './state/auth';

// The one answer for a code that is unknown, used, expired or closed. The
// server does not say which, so neither does the page. Someone else may have
// used it, so the person is asked to say so: the operator can see the use
// and revoke what it gave.
export const linkNoLongerWorks =
  'This sign-in link no longer works. Each link works once, for a short time. Ask an operator for a new one. If you did not use it, tell an operator.';

// Said when this browser holds a sign-in that could not be checked. Once the
// server answers, a reload says whose it is and offers to sign it out.
export const linkHeldLine =
  'This browser holds another sign-in that could not be checked. Reload the page when the server can be reached: it will say who is signed in here and offer to sign them out first.';

type Refusal = { readonly failure: api.ApiFailure; readonly code?: string; readonly retryAfter?: number };

// refusalLine is the plain words for a link the server would not preview or
// use.
function refusalLine(result: Refusal): string {
  if (result.failure === 'network') return 'Cannot reach the server. Check your connection and try again.';
  switch (result.code) {
    case 'sign_in_link_invalid':
      return linkNoLongerWorks;
    case 'rate_limited':
      return `Too many tries from your network. Try again in ${waitWords(result.retryAfter)}.`;
    default:
      return 'The server gave an unexpected reply. Try again.';
  }
}

type Preview =
  | { readonly state: 'loading' }
  | { readonly state: 'ready'; readonly preview: SignInLinkPreviewV1 }
  // gone is the link that no longer works: nothing on the page can help.
  | { readonly state: 'refused'; readonly line: string; readonly gone: boolean };

export function SignInLink({
  origin,
  signInCode,
  signedInAs,
  unchecked,
  onSignOutFirst,
  onBack,
  onSignedIn,
}: {
  readonly origin: string;
  readonly signInCode: string;
  // The name of whoever is signed in on this page, or null.
  readonly signedInAs: string | null;
  // Whether this browser holds another sign-in that could not be checked.
  // Using the link would write over it, so the page does not offer to.
  readonly unchecked: boolean;
  // Signs whoever is signed in out, first, and resolves to whether the server
  // was told (as signOut does).
  readonly onSignOutFirst: () => Promise<boolean>;
  readonly onBack: () => void;
  // The person signed in again. principal is null when the credential was
  // kept but whoami did not answer: App asks again.
  readonly onSignedIn: (principal: WhoAmIResponseV1 | null) => void;
}) {
  const [preview, setPreview] = useState<Preview>({ state: 'loading' });
  const [attempt, setAttempt] = useState(0);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [untold, setUntold] = useState(false);
  // Using the link found another sign-in held here after all.
  const [held, setHeld] = useState(false);
  const heading = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    let live = true;
    void api.previewSignInLink(signInCode).then((result) => {
      if (!live) return;
      if (result.ok) setPreview({ state: 'ready', preview: result.value });
      else setPreview({ state: 'refused', line: refusalLine(result), gone: result.code === 'sign_in_link_invalid' });
    });
    return () => {
      live = false;
    };
  }, [signInCode, attempt]);

  useEffect(() => {
    heading.current?.focus();
  }, [preview.state]);

  const ready = preview.state === 'ready' ? preview.preview : null;

  function signOutFirst() {
    if (busy) return;
    setBusy(true);
    void onSignOutFirst().then((told) => {
      setBusy(false);
      setUntold(!told);
    });
  }

  function signIn() {
    if (busy || ready === null) return;
    setBusy(true);
    setProblem(null);
    void api.redeemSignInLink(signInCode).then((result) => {
      setBusy(false);
      if (result.ok) {
        onSignedIn(result.value);
        return;
      }
      switch (result.failure) {
        case 'held':
          setHeld(true);
          return;
        case 'unchecked':
          onSignedIn(null);
          return;
        case 'unkept':
          setPreview({
            state: 'refused',
            line: 'This browser will not let the page keep your sign-in, so the link was used up for nothing. Allow this site to keep data, then ask an operator for a new link.',
            gone: true,
          });
          return;
        case 'unauthorized':
        case 'network':
        case 'invalid':
          break;
      }
      if (result.code === 'sign_in_link_invalid') setPreview({ state: 'refused', line: linkNoLongerWorks, gone: true });
      else setProblem(refusalLine(result));
    });
  }

  return (
    <main className="signin invite" aria-labelledby="signin-link-heading">
      <h1 id="signin-link-heading" ref={heading} tabIndex={-1}>
        {preview.state === 'refused' && preview.gone ? 'This sign-in link no longer works' : 'Sign in to Conch'}
      </h1>
      <p>
        Server: <code>{origin}</code>
      </p>
      {preview.state === 'loading' ? <p role="status">Checking the link…</p> : null}
      {preview.state === 'refused' ? (
        <>
          <p role="alert" className="notice">
            {preview.line}
          </p>
          <div className="actions">
            {preview.gone ? null : (
              <button
                type="button"
                onClick={() => {
                  setPreview({ state: 'loading' });
                  setAttempt((n) => n + 1);
                }}
              >
                Try again
              </button>
            )}
            <button type="button" onClick={onBack}>
              Go back
            </button>
          </div>
        </>
      ) : null}
      {ready === null ? null : (
        <>
          <p>
            This link signs you back into your account, <strong>{ready.name}</strong>. It works once.
          </p>
          {signedInAs !== null ? (
            <>
              <p>
                You are signed in here as <strong>{signedInAs}</strong>. Using the link signs {signedInAs} out of this browser
                first.
              </p>
              <div className="actions">
                <button type="button" onClick={signOutFirst} aria-busy={busy}>
                  Sign {signedInAs} out
                </button>
                <button type="button" onClick={onBack}>
                  Go back
                </button>
              </div>
            </>
          ) : unchecked || held ? (
            <>
              <p role="alert" className="notice">
                {linkHeldLine}
              </p>
              <div className="actions">
                <button type="button" onClick={onBack}>
                  Go back
                </button>
              </div>
            </>
          ) : (
            <>
              {untold ? <p className="notice">{noticeText('untold')}</p> : null}
              <div className="actions">
                <button type="button" onClick={signIn} aria-busy={busy}>
                  {busy ? 'Signing in…' : `Sign in as ${ready.name}`}
                </button>
                <button type="button" onClick={onBack}>
                  Go back
                </button>
              </div>
              <p role="alert" className="notice">
                {problem ?? ''}
              </p>
            </>
          )}
        </>
      )}
      <ServerVersion className="hint" />
    </main>
  );
}
