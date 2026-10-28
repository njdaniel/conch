// An operator's setup (issue #714, design §5.3): the page that spends a
// setup token made on the server's host (conchd operator bootstrap, setup or
// recover), then sets the operator's password and enrols their authenticator
// app. Nobody is signed in by it: once both are done, the operator signs in
// with their name, the password and a code, and only then is an operator in
// this browser.
//
// The token, the password and the codes stay in their fields, never in the
// page's state, and go only in a request body. The credential the token
// gives lives fifteen minutes, in api.ts's memory alone: a reload drops it,
// and a new token is made on the host. The authenticator's key is shown once,
// in this view only, behind Reveal, and never while this tab shares its
// screen (Reveal.tsx). No QR code is drawn (that would be a new dependency,
// design §5.3): the key is typed into the app.
import { useEffect, useId, useLayoutEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { waitWords } from './Invite';
import { revealLimits, sharingLine, useTabSharing } from './Reveal';
import type { TOTPEnrollmentV1 } from './schema/auth';
import { ServerVersion } from './ServerVersion';
import { NewPassword } from './Settings';
import { passwordProblem } from './CreateAccount';
import { typedCode } from './SignIn';
import { insecureLine, needsHttps } from './transport';

type Step =
  | { readonly step: 'token' }
  | { readonly step: 'password'; readonly name: string; readonly hasPassword: boolean }
  // skippable: whoami says setup is complete without an authenticator
  // (ADR-012: a server that lets an operator sign in with a password alone).
  | { readonly step: 'authenticator'; readonly name: string; readonly skippable: boolean }
  | { readonly step: 'done'; readonly name: string; readonly skipped?: boolean }
  // The setup credential has ended (its fifteen minutes, or the host
  // recovered the operator again): a new token is needed.
  | { readonly step: 'ended' };

type Refusal = { readonly failure: api.ApiFailure; readonly code?: string; readonly retryAfter?: number };

function refusalLine(result: Refusal): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  switch (result.code) {
    case 'setup_token_invalid':
      return 'That setup token does not work. Each works once, for ten minutes: make a new one on the server’s host (conchd operator setup).';
    case 'auth_failed':
      return 'That code is not right. Check that the app’s clock is right, and try the next code it shows.';
    case 'password_unacceptable':
      return 'That password is too common, or has your name in it. Choose another.';
    case 'rate_limited':
      return `Too many tries. Try again in ${waitWords(result.retryAfter)}.`;
    case 'totp_already_enrolled':
      return 'This operator has an authenticator already. To replace it, recover the operator on the server’s host (conchd operator recover).';
    case 'https_required':
      return insecureLine;
    default:
      return 'The server refused. Try again.';
  }
}

// grouped is the key in groups of four, as authenticator apps show them, so
// it can be read and typed without losing one's place.
function grouped(secret: string): string {
  return secret.replace(/(.{4})/g, '$1 ').trim();
}

export function Setup({
  origin,
  link,
  onDone,
  onBack,
}: {
  readonly origin: string;
  // A setup link's token (ADR-011), waiting in a ref for the field: put in
  // it once, and spent only when the person clicks Continue.
  readonly link?: { current: string | null };
  // skipped: the authenticator was left unset.
  readonly onDone: (skipped: boolean) => void;
  readonly onBack: () => void;
}) {
  const id = useId();
  const insecure = needsHttps(origin);
  const sharing = useTabSharing();
  const [step, setStep] = useState<Step>({ step: 'token' });
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  const tokenField = useRef<HTMLInputElement>(null);
  const codeField = useRef<HTMLInputElement>(null);
  const chosen = useRef<HTMLInputElement>(null);
  const again = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  // The authenticator's key, in memory only, shown on Reveal.
  const enrollment = useRef<TOTPEnrollmentV1 | null>(null);
  const [enrolled, setEnrolled] = useState(false);
  const [revealed, setRevealed] = useState(false);
  // Only that the field came filled from a link, never what is in it.
  const [fromLink, setFromLink] = useState(false);

  // A setup link fills the token field and spends nothing: redeem runs on the
  // click alone. The token moves from the ref to the field and is gone from
  // the ref.
  useLayoutEffect(() => {
    const token = link?.current ?? null;
    if (token === null || tokenField.current === null) return;
    tokenField.current.value = token;
    if (link !== undefined) link.current = null;
    setFromLink(true);
  }, [link]);

  useEffect(() => {
    heading.current?.focus();
  }, [step.step]);

  // A share that starts hides the key at once.
  useEffect(() => {
    if (sharing) setRevealed(false);
  }, [sharing]);

  // Leaving forgets the key and the setup credential here.
  useEffect(
    () => () => {
      enrollment.current = null;
      api.endSetup();
    },
    [],
  );

  function ended(result: Refusal): boolean {
    if (result.failure !== 'unauthorized') return false;
    enrollment.current = null;
    setStep({ step: 'ended' });
    return true;
  }

  // next reads what the setup credential says is left to do.
  async function next(): Promise<void> {
    const state = await api.setupState();
    if (!state.ok) {
      if (!ended(state)) setLine(refusalLine(state));
      return;
    }
    if (state.value.auth.totp_enrolled === true) setStep({ step: 'done', name: state.value.name });
    else
      setStep({
        step: 'authenticator',
        name: state.value.name,
        skippable: state.value.auth.setup_complete && state.value.auth.scope === 'setup',
      });
  }

  // skip ends the setup credential with no authenticator (ADR-012 item 5):
  // the operator then signs in with the name and password alone.
  function skip() {
    if (busy || step.step !== 'authenticator') return;
    const name = step.name;
    setBusy(true);
    setLine(null);
    enrollment.current = null;
    setRevealed(false);
    void api.skipAuthenticator().then((result) => {
      setBusy(false);
      if (!result.ok && !ended(result)) {
        setLine(refusalLine(result));
        return;
      }
      if (result.ok) setStep({ step: 'done', name, skipped: true });
    });
  }

  function redeem(event: SyntheticEvent) {
    event.preventDefault();
    const input = tokenField.current;
    if (input === null || busy) return;
    const token = input.value.trim();
    input.value = '';
    if (token === '') {
      input.focus();
      return;
    }
    setBusy(true);
    setLine(null);
    void api.redeemSetupToken(token).then((result) => {
      setBusy(false);
      if (!result.ok) {
        setLine(refusalLine(result));
        tokenField.current?.focus();
        return;
      }
      setStep({ step: 'password', name: result.value.name, hasPassword: result.value.auth.password_set });
    });
  }

  function setPassword(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    const typed = chosen.current?.value ?? '';
    const said = passwordProblem(typed, again.current?.value ?? '');
    if (said !== null) {
      if (again.current !== null) again.current.value = '';
      setLine(said);
      chosen.current?.focus();
      return;
    }
    setBusy(true);
    setLine(null);
    void api.setupPassword(typed).then(async (result) => {
      setBusy(false);
      if (chosen.current !== null) chosen.current.value = '';
      if (again.current !== null) again.current.value = '';
      if (!result.ok) {
        if (!ended(result)) setLine(refusalLine(result));
        return;
      }
      await next();
    });
  }

  function showKey() {
    if (busy || sharing) return;
    if (enrollment.current !== null) {
      setRevealed(true);
      return;
    }
    setBusy(true);
    setLine(null);
    void api.enrollAuthenticator().then((result) => {
      setBusy(false);
      if (!result.ok) {
        if (!ended(result)) setLine(refusalLine(result));
        return;
      }
      enrollment.current = result.value;
      setEnrolled(true);
      setRevealed(true);
    });
  }

  function confirm(event: SyntheticEvent) {
    event.preventDefault();
    const input = codeField.current;
    if (input === null || busy) return;
    const code = typedCode(input.value);
    input.value = '';
    if (code === null) {
      setLine(`Type the ${String(api.codeDigits)} digits the app shows for this server.`);
      input.focus();
      return;
    }
    setBusy(true);
    setLine(null);
    void api.confirmAuthenticator(code).then(async (result) => {
      setBusy(false);
      if (!result.ok) {
        if (!ended(result)) setLine(refusalLine(result));
        codeField.current?.focus();
        return;
      }
      enrollment.current = null;
      setRevealed(false);
      // With the password set, the setup credential has ended: setup is
      // complete.
      const state = await api.setupState();
      if (state.ok && !state.value.auth.password_set) setStep({ step: 'password', name: state.value.name, hasPassword: false });
      else setStep({ step: 'done', name: step.step === 'authenticator' ? step.name : '' });
    });
  }

  const key = enrollment.current;
  const shown = revealed && !sharing ? key : null;

  return (
    <main className="signin setup" aria-labelledby={`${id}-heading`}>
      <h1 id={`${id}-heading`} ref={heading} tabIndex={-1}>
        {step.step === 'done' ? 'Setup is complete' : 'Set up an operator account'}
      </h1>
      <p>
        Server: <code>{origin}</code>
      </p>
      {insecure ? (
        <p className="notice insecure">{insecureLine}</p>
      ) : step.step === 'token' ? (
        <form onSubmit={redeem}>
          <p>
            Paste the setup token that conchd operator bootstrap, setup or recover printed on the server’s host. It works once,
            for ten minutes. Then you choose a password and set up an authenticator app.
          </p>
          {fromLink ? <p className="hint">The setup token from your link is filled in. Nothing has been used yet: click Continue.</p> : null}
          <label htmlFor={`${id}-token`}>Setup token</label>
          <input id={`${id}-token`} ref={tokenField} type="password" autoComplete="off" autoCapitalize="off" autoCorrect="off" spellCheck={false} />
          <button type="submit" aria-busy={busy}>
            {busy ? 'Checking…' : 'Continue'}
          </button>
        </form>
      ) : step.step === 'password' ? (
        <form onSubmit={setPassword}>
          <p>
            Setting up <strong>{step.name}</strong>. This page has fifteen minutes; do not reload it.
          </p>
          {step.hasPassword ? (
            <p className="hint">
              This account has a password. If it is lost, or someone else may know it, choose a new one now: it replaces the old
              one. Otherwise keep it.
            </p>
          ) : null}
          <NewPassword chosen={chosen} again={again} label="Choose a password" />
          <button type="submit" aria-busy={busy}>
            {busy ? 'Setting…' : 'Set password'}
          </button>
          {step.hasPassword ? (
            <button
              type="button"
              onClick={() => {
                setLine(null);
                void next();
              }}
            >
              Keep my password
            </button>
          ) : null}
        </form>
      ) : step.step === 'authenticator' ? (
        <section aria-labelledby={`${id}-app`}>
          <h2 id={`${id}-app`}>Your authenticator app</h2>
          <p>
            Add this server to an authenticator app on a device you keep with you, by typing its key. The key is shown once:
            the server never shows it again.
          </p>
          <p className="hint">{revealLimits}</p>
          {sharing ? (
            <p role="status" className="notice">
              {sharingLine}
            </p>
          ) : null}
          {shown === null ? null : (
            <dl className="totp-key" aria-label="Authenticator key">
              <dt>Key</dt>
              <dd>
                <code>{grouped(shown.secret)}</code>
              </dd>
              <dt>Account</dt>
              <dd>{shown.account}</dd>
              <dt>Issuer</dt>
              <dd>{shown.issuer}</dd>
              <dt>Type</dt>
              <dd>
                Time-based, {shown.algorithm}, {shown.digits} digits, every {shown.period_seconds} seconds
              </dd>
            </dl>
          )}
          <div className="actions">
            {shown === null ? (
              <button type="button" onClick={showKey} disabled={sharing} aria-busy={busy}>
                {enrolled ? 'Reveal the key' : 'Show the key'}
              </button>
            ) : (
              <button
                type="button"
                onClick={() => {
                  setRevealed(false);
                }}
              >
                Hide the key
              </button>
            )}
          </div>
          {enrolled ? (
            <form onSubmit={confirm}>
              <label htmlFor={`${id}-code`}>Code the app shows now</label>
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
              <button type="submit" aria-busy={busy}>
                {busy ? 'Checking…' : 'Finish setup'}
              </button>
            </form>
          ) : null}
          {step.skippable ? (
            <>
              <p className="hint">
                This server lets an operator sign in with a name and password alone. You can skip the authenticator now: you will
                not be asked for a code.
              </p>
              <div className="actions">
                <button type="button" onClick={skip} aria-busy={busy}>
                  Skip
                </button>
              </div>
            </>
          ) : null}
        </section>
      ) : step.step === 'done' ? (
        <>
          {step.skipped === true ? (
            <p>
              {step.name === '' ? 'Your' : `${step.name}’s`} password is set, with no authenticator. Sign in with your name and
              your password.
            </p>
          ) : (
            <p>
              {step.name === '' ? 'Your' : `${step.name}’s`} password and authenticator are set. Sign in with your name, your
              password and a code from the app. Keep the app’s device safe: without it, only the server’s host can let you back in.
            </p>
          )}
          <div className="actions">
            <button
              type="button"
              onClick={() => {
                onDone(step.skipped === true);
              }}
            >
              Go to sign-in
            </button>
          </div>
        </>
      ) : (
        <p role="alert" className="notice">
          The setup has ended: it lasts fifteen minutes, and a reload ends it too. Make a new setup token on the server’s host
          (conchd operator setup), then start again.
        </p>
      )}
      <p role="alert" className="notice">
        {line ?? ''}
      </p>
      {step.step === 'done' ? null : (
        <div className="actions">
          <button type="button" onClick={onBack}>
            Go back
          </button>
        </div>
      )}
      <ServerVersion className="hint" />
    </main>
  );
}
