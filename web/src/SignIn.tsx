// The page of someone not signed in: the bare address's front door (issue
// #713, ADR-009, design §3.1, §3.2). It offers what the server's front door
// says the address allows now (Join as guest, Create an account, either or
// neither, with honest words for paused and full), then Sign in with a name
// and a password for a returning member, then the advanced way in: a
// credential an operator issued, for operators and scripts. The server
// decides again whatever is sent; hiding a button is not enforcement.
//
// An operator signs in with a password and then a code from an authenticator
// app (issue #714, design §5.3). A right password alone signs nobody in: the
// page then shows the code's step and nothing of the server, and the step
// ends at the challenge's five minutes. An operator's first setup, from a
// token made on the host, is its own page (Setup.tsx), reached from here.
import { useEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { CreateAccount, type AccountProblem } from './CreateAccount';
import { durationWords, GuestJoin, guestJoinRefusal } from './Guest';
import { accountRefusal, heldLine, refusalLine } from './Invite';
import type { JoinInfoV1 } from './schema/guest';
import type { WhoAmIResponseV1 } from './schema/principal';
import { ServerVersion } from './ServerVersion';
import { noticeText, type AuthNotice } from './state/auth';
import { insecureLine, needsHttps } from './transport';

// Said when a join went through but this browser would not keep its sign-in.
const keepLine = 'You joined, but this browser will not let the page keep your sign-in. Allow this site to keep data, then try again.';

type Info = { readonly state: 'loading' } | { readonly state: 'ready'; readonly info: JoinInfoV1 } | { readonly state: 'unknown' };

// What the page says of a setup token pasted where a credential goes: it is
// not one, and is not sent as one.
export const setupTokenLine = 'That is a setup token, not a credential. Use it under Set up an operator account.';
const setupTokenPrefix = 'conch_setup_';

// The code's step, while an operator's password was right (issue #714).
export interface CodeStep {
  readonly name: string;
  readonly until: string;
  readonly checking: boolean;
  readonly line?: string;
}

export function SignIn({
  origin,
  notice,
  line,
  checking,
  code,
  onSubmit,
  onPassword,
  onCode,
  onCodeOver,
  onForget,
  onJoined,
  onSetup,
}: {
  readonly origin: string;
  readonly notice: AuthNotice | null;
  // The page's own words for a refused password sign-in, in place of a notice.
  readonly line?: string;
  readonly checking: boolean;
  // The code's step, when an operator's password was right.
  readonly code?: CodeStep;
  readonly onSubmit: (credential: string) => void;
  readonly onPassword: (name: string, password: string) => void;
  readonly onCode?: (code: string) => void;
  // The code's step given up, by the person or by its time running out
  // (over): the password comes first again.
  readonly onCodeOver?: (over: boolean) => void;
  // Opens an operator's setup from a host-made token.
  readonly onSetup?: () => void;
  // Forgetting a stored sign-in that could not be checked (issue #469),
  // offered only when signing in was refused because of one.
  readonly onForget: () => void;
  // Joined from the front door, as a guest or with a new account. principal
  // is null when the credential was kept but whoami did not answer.
  readonly onJoined: (principal: WhoAmIResponseV1 | null) => void;
}) {
  const field = useRef<HTMLInputElement>(null);
  const nameField = useRef<HTMLInputElement>(null);
  const passwordField = useRef<HTMLInputElement>(null);
  const [info, setInfo] = useState<Info>({ state: 'loading' });
  const [way, setWay] = useState<'guest' | 'account' | null>(null);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<AccountProblem | null>(null);
  const [guestProblem, setGuestProblem] = useState<string | null>(null);
  const [credentialLine, setCredentialLine] = useState<string | null>(null);
  const insecure = needsHttps(origin);

  useEffect(() => {
    let live = true;
    void api.joinInfo().then((result) => {
      if (!live) return;
      // A server that predates the front door, or none that answers: the
      // page offers sign-in alone, as before.
      setInfo(result.ok ? { state: 'ready', info: result.value } : { state: 'unknown' });
    });
    return () => {
      live = false;
    };
  }, []);

  // After a refusal focus goes back where the next try is typed, so it needs
  // no mouse: the credential field for a refused credential, the password
  // for a refused password.
  useEffect(() => {
    if (notice === 'rejected' || notice === 'held') field.current?.focus();
    else if (line !== undefined) passwordField.current?.focus();
    else if (notice !== null) (nameField.current ?? field.current)?.focus();
  }, [notice, line]);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    const input = field.current;
    if (input === null || checking) return;
    const credential = input.value.trim();
    // The field is emptied at once: the credential is held by the caller, not
    // by the page.
    input.value = '';
    setCredentialLine(null);
    if (credential === '') {
      input.focus();
      return;
    }
    // A host-made setup token signs nobody in (issue #714): it is not sent
    // as a credential, and the page says where it goes.
    if (credential.startsWith(setupTokenPrefix)) {
      setCredentialLine(setupTokenLine);
      input.focus();
      return;
    }
    onSubmit(credential);
  }

  function signIn(event: SyntheticEvent) {
    event.preventDefault();
    const name = nameField.current?.value ?? '';
    const password = passwordField.current;
    if (password === null || checking) return;
    const typed = password.value;
    // The password is emptied at once, as the credential is.
    password.value = '';
    if (name === '' || typed === '') {
      (name === '' ? nameField.current : password)?.focus();
      return;
    }
    onPassword(name, typed);
  }

  function joinAsGuest(label: string) {
    if (busy) return;
    setBusy(true);
    setGuestProblem(null);
    void api.joinAsGuest(label).then((result) => {
      setBusy(false);
      if (result.ok) {
        onJoined(result.value);
        return;
      }
      switch (result.failure) {
        case 'unchecked':
          onJoined(null);
          return;
        case 'held':
          setGuestProblem(heldLine);
          return;
        case 'unkept':
          setGuestProblem(keepLine);
          return;
        default:
          setGuestProblem(guestJoinRefusal(result));
      }
    });
  }

  function createAccount(name: string, password: string) {
    if (busy) return;
    setBusy(true);
    setProblem(null);
    void api.createAccount(name, password).then((result) => {
      setBusy(false);
      if (result.ok) {
        onJoined(result.value);
        return;
      }
      switch (result.failure) {
        case 'unchecked':
          onJoined(null);
          return;
        case 'held':
          setProblem({ line: heldLine });
          return;
        case 'unkept':
          setProblem({ line: keepLine });
          return;
        default:
          setProblem(accountRefusal(result) ?? { line: refusalLine(result) });
      }
    });
  }

  const ready = info.state === 'ready' ? info.info : null;
  const join = ready?.join;
  const passwordLogin = ready === null || ready.password_login;
  const shown = line ?? (notice === null ? '' : noticeText(notice));

  if (code !== undefined) {
    return <CodeSignIn origin={origin} step={code} insecure={insecure} onCode={onCode} onCodeOver={onCodeOver} />;
  }

  return (
    <main className="signin">
      <h1>Conch</h1>
      <p>
        Server: <code>{origin}</code>
        {ready !== null && ready.server_name !== '' ? <span className="server-name"> ({ready.server_name})</span> : null}
      </p>
      <p role="alert" className="notice">
        {shown}
      </p>
      {insecure ? (
        <p className="notice insecure">{insecureLine}</p>
      ) : join === undefined ? null : (
        <section aria-labelledby="join-heading" className="front-door">
          <h2 id="join-heading">Join</h2>
          {join.paused ? (
            <p className="notice">Joining is paused on this server for now. If you have an account, sign in below.</p>
          ) : !join.guest && !join.account ? (
            <p className="hint">
              {join.guests_full
                ? 'No more guests can join right now. Try again later.'
                : 'This server is invite-only. To join, ask someone on it for an invite link.'}
            </p>
          ) : (
            <>
              {join.guests_full ? <p className="hint">Guest visits are full right now; you can still create an account.</p> : null}
              {way === null ? (
                <div className="actions">
                  {join.guest ? (
                    <button
                      type="button"
                      onClick={() => {
                        setWay('guest');
                      }}
                    >
                      Join as guest
                    </button>
                  ) : null}
                  {join.account ? (
                    <button
                      type="button"
                      onClick={() => {
                        setWay('account');
                      }}
                    >
                      Create an account
                    </button>
                  ) : null}
                </div>
              ) : null}
              {way === 'guest' && ready !== null ? (
                <GuestJoin
                  busy={busy}
                  problem={guestProblem}
                  bounds={`after ${durationWords(ready.guest_idle_seconds)} without activity, and after ${durationWords(ready.guest_absolute_seconds)} at most`}
                  onJoin={joinAsGuest}
                  onCancel={() => {
                    if (busy) return;
                    setGuestProblem(null);
                    setWay(null);
                  }}
                />
              ) : null}
              {way === 'account' ? (
                <CreateAccount
                  purpose="join"
                  busy={busy}
                  problem={problem}
                  onSubmit={createAccount}
                  onCancel={() => {
                    if (busy) return;
                    setProblem(null);
                    setWay(null);
                  }}
                />
              ) : null}
            </>
          )}
        </section>
      )}
      {insecure || !passwordLogin ? null : (
        <section aria-labelledby="password-heading">
          <h2 id="password-heading">Sign in</h2>
          <form onSubmit={signIn}>
            <label htmlFor="signin-name">Name</label>
            <input
              id="signin-name"
              ref={nameField}
              type="text"
              autoComplete="username"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
            />
            <label htmlFor="signin-password">Password</label>
            <input id="signin-password" ref={passwordField} type="password" autoComplete="current-password" />
            <button type="submit" aria-busy={checking}>
              {checking ? 'Checking…' : 'Sign in'}
            </button>
            <p className="hint">
              Forgot your password? There is no reset here: ask an operator for a link to choose a new one. Without a
              password yet, ask an operator for a sign-in link.
            </p>
          </form>
        </section>
      )}
      <section aria-labelledby="credential-heading" className="credential-signin">
        <h2 id="credential-heading">Operators and scripts</h2>
        <p className="hint">
          An operator signs in above, with a name, a password and a code from an authenticator app. A credential pasted here
          is a script’s: no password or code is checked, so it is never an operator’s sign-in.
        </p>
        {onSetup === undefined || insecure ? null : (
          <div className="actions">
            <button type="button" onClick={onSetup}>
              Set up an operator account
            </button>
          </div>
        )}
        <form onSubmit={submit}>
          <label htmlFor="credential">Credential</label>
          <p id="credential-help" className="hint">
            Paste the credential an operator issued to you. People sign in with a name and a password above.
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
            {checking ? 'Checking…' : 'Use credential'}
          </button>
          {notice === 'held' && (
            <button type="button" onClick={onForget}>
              Forget the stored sign-in
            </button>
          )}
          <p role="status" className="notice">
            {credentialLine ?? ''}
          </p>
        </form>
      </section>
      <ServerVersion className="hint" />
    </main>
  );
}

// typedCode is a typed code as it is sent: six digits, as an authenticator
// shows them, spaces allowed between; null for anything else.
export function typedCode(typed: string): string | null {
  const digits = typed.replace(/\s+/g, '');
  return new RegExp(`^\\d{${String(api.codeDigits)}}$`).test(digits) ? digits : null;
}

// CodeSignIn is the second step of an operator's sign-in: the code from the
// authenticator app, for the name whose password was right. Nothing else of
// the page is shown, and nobody is signed in until the code is accepted. The
// code is in its field alone, never in the page's state, and the field is
// emptied as it is sent. The step ends by itself when the challenge does.
function CodeSignIn({
  origin,
  step,
  insecure,
  onCode,
  onCodeOver,
}: {
  readonly origin: string;
  readonly step: CodeStep;
  readonly insecure: boolean;
  readonly onCode: ((code: string) => void) | undefined;
  readonly onCodeOver: ((over: boolean) => void) | undefined;
}) {
  const field = useRef<HTMLInputElement>(null);
  const [local, setLocal] = useState<string | null>(null);

  useEffect(() => {
    field.current?.focus();
  }, [step.line]);

  // Out of time, the challenge is of no more use: the password comes first
  // again, before a code is typed in vain.
  useEffect(() => {
    const wait = Math.min(Math.max(0, Date.parse(step.until) - Date.now()), 2 ** 31 - 1);
    const timer = window.setTimeout(() => onCodeOver?.(true), wait);
    return () => {
      window.clearTimeout(timer);
    };
  }, [step.until, onCodeOver]);

  function send(event: SyntheticEvent) {
    event.preventDefault();
    const input = field.current;
    if (input === null || step.checking) return;
    const code = typedCode(input.value);
    input.value = '';
    if (code === null) {
      setLocal(`Type the ${String(api.codeDigits)} digits your authenticator app shows for this server.`);
      input.focus();
      return;
    }
    setLocal(null);
    onCode?.(code);
  }

  const until = new Date(step.until);
  return (
    <main className="signin">
      <h1>Conch</h1>
      <p>
        Server: <code>{origin}</code>
      </p>
      <section aria-labelledby="code-heading">
        <h2 id="code-heading">Sign in: your code</h2>
        {insecure ? (
          <p className="notice insecure">{insecureLine}</p>
        ) : (
          <form onSubmit={send}>
            <p>
              The password for <strong>{step.name}</strong> was right. You are not signed in yet: type the code your
              authenticator app shows now.
            </p>
            <label htmlFor="signin-code">Code from your authenticator app</label>
            <input
              id="signin-code"
              ref={field}
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-describedby="signin-code-help"
            />
            <p id="signin-code-help" className="hint">
              This step ends at <time dateTime={step.until}>{until.toLocaleTimeString()}</time>; after that, sign in with
              your password again. Lost your authenticator? Recovery happens on the server’s host: conchd operator recover.
            </p>
            <button type="submit" aria-busy={step.checking}>
              {step.checking ? 'Checking…' : 'Sign in'}
            </button>
            <button
              type="button"
              onClick={() => {
                onCodeOver?.(false);
              }}
            >
              Start again
            </button>
          </form>
        )}
        <p role="alert" className="notice">
          {local ?? step.line ?? ''}
        </p>
      </section>
      <ServerVersion className="hint" />
    </main>
  );
}
