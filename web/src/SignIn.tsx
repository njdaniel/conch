// The page of someone not signed in: the bare address's front door (issue
// #713, ADR-009, design §3.1, §3.2). It offers what the server's front door
// says the address allows now (Join as guest, Create an account, either or
// neither, with honest words for paused and full), then Sign in with a name
// and a password for a returning member, then the advanced way in: a
// credential an operator issued, for operators and scripts. The server
// decides again whatever is sent; hiding a button is not enforcement.
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

export function SignIn({
  origin,
  notice,
  line,
  checking,
  onSubmit,
  onPassword,
  onForget,
  onJoined,
}: {
  readonly origin: string;
  readonly notice: AuthNotice | null;
  // The page's own words for a refused password sign-in, in place of a notice.
  readonly line?: string;
  readonly checking: boolean;
  readonly onSubmit: (credential: string) => void;
  readonly onPassword: (name: string, password: string) => void;
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
    if (credential === '') {
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
        </form>
      </section>
      <ServerVersion className="hint" />
    </main>
  );
}
