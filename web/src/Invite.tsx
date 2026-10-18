// The landing page of an invite link (issue #440): what the invite grants,
// then a name, then the lobby. The code is a prop held in memory by App, sent
// only in the bodies of preview and redeem, and never rendered: nothing on
// this page shows it.
import { useEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import type { InvitePreviewV1 } from './schema/invite';
import type { WhoAmIResponseV1 } from './schema/principal';
import { ServerVersion } from './ServerVersion';
import { noticeText } from './state/auth';
import { fallbackChannel } from './state/channels';

// The one answer for a code that is unknown, used, expired or closed. The
// server does not say which, so neither does the page. Someone who joined from
// a personal link and comes back to it needs a sign-in link, not an invite
// (issue #593); the heading says the rest, in other words.
export const noLongerWorks = 'Ask the person who sent it for a new one. If you already joined from this link, ask them for a sign-in link instead.';
export const goneHeading = "This link can't be used";

// waitWords says how long Retry-After asks the page to wait, in words.
export function waitWords(seconds: number | undefined): string {
  if (seconds === undefined) return 'a few minutes';
  if (seconds < 60) return seconds === 1 ? '1 second' : `${String(seconds)} seconds`;
  const minutes = Math.ceil(seconds / 60);
  return minutes === 1 ? '1 minute' : `${String(minutes)} minutes`;
}

// serverLine is the server's own line for a request it could not take, as a
// sentence: its schema messages name the rule broken.
export function serverLine(message: string | undefined, fallback: string): string {
  const line = (message ?? '').replace(/^schema:\s*/, '');
  if (line === '') return fallback;
  return `${line.charAt(0).toUpperCase()}${line.slice(1)}${/[.!?]$/.test(line) ? '' : '.'}`;
}

type Refusal = { readonly failure: api.ApiFailure; readonly code?: string; readonly retryAfter?: number };

// refusalLine is the plain words for an invite the server would not preview
// or redeem.
function refusalLine(result: Refusal): string {
  if (result.failure === 'network') return 'Cannot reach the server. Check your connection and try again.';
  switch (result.code) {
    case 'invite_invalid':
      return noLongerWorks;
    case 'invites_paused':
      return 'Invites are paused on this server for now. Try the link again later.';
    case 'rate_limited':
      return `Too many tries from your network. Try again in ${waitWords(result.retryAfter)}.`;
    default:
      return 'The server gave an unexpected reply. Try again.';
  }
}

// heldLine is said when this browser holds a sign-in that could not be
// checked. Once the server answers, a reload says whose it is and offers to
// sign it out, as for anyone signed in here.
export const heldLine =
  'This browser holds another sign-in that could not be checked. Reload the page when the server can be reached: it will say who is signed in here and offer to sign them out before you join.';

type Preview =
  | { readonly state: 'loading' }
  | { readonly state: 'ready'; readonly preview: InvitePreviewV1 }
  // gone is the invite that no longer works: nothing on the page can help,
  // and only someone signed in here has somewhere to go back to.
  | { readonly state: 'refused'; readonly line: string; readonly gone: boolean };

export function Invite({
  origin,
  inviteCode,
  signedInAs,
  unchecked,
  onNewPerson,
  onBack,
  onJoined,
}: {
  readonly origin: string;
  readonly inviteCode: string;
  // The name of whoever is signed in on this page, or null.
  readonly signedInAs: string | null;
  // Whether this browser holds another sign-in that could not be checked (the
  // server did not answer when the page loaded). Redeeming would write over
  // it, so the page does not offer to.
  readonly unchecked: boolean;
  // Signs whoever is signed in out, first, and resolves to whether the server
  // was told (as signOut does).
  readonly onNewPerson: () => Promise<boolean>;
  readonly onBack: () => void;
  // The joiner, and the channel they land in: #general when the invite grants
  // it, else the first channel it grants. principal is null when the account
  // was made but whoami did not answer: App asks again.
  readonly onJoined: (principal: WhoAmIResponseV1 | null, channel: string | undefined) => void;
}) {
  const [preview, setPreview] = useState<Preview>({ state: 'loading' });
  const [attempt, setAttempt] = useState(0);
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const [untold, setUntold] = useState(false);
  // Redeeming found another sign-in held here after all.
  const [held, setHeld] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  const heading = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    let live = true;
    void api.previewInvite(inviteCode).then((result) => {
      if (!live) return;
      if (result.ok) setPreview({ state: 'ready', preview: result.value });
      else setPreview({ state: 'refused', line: refusalLine(result), gone: result.code === 'invite_invalid' });
    });
    return () => {
      live = false;
    };
  }, [inviteCode, attempt]);

  useEffect(() => {
    heading.current?.focus();
  }, [preview.state]);

  // After a refusal the field keeps what was typed and gets focus back.
  useEffect(() => {
    if (problem !== null) field.current?.focus();
  }, [problem]);

  const ready = preview.state === 'ready' ? preview.preview : null;

  function newPerson() {
    if (busy) return;
    setBusy(true);
    void onNewPerson().then((told) => {
      setBusy(false);
      setUntold(!told);
    });
  }

  function join(event: SyntheticEvent) {
    event.preventDefault();
    if (busy || ready === null) return;
    setBusy(true);
    setProblem(null);
    // What was typed is sent as it is: the server decides whether it is a
    // name, and the page shows its answer.
    void api.redeemInvite(inviteCode, name).then((result) => {
      setBusy(false);
      if (result.ok) {
        onJoined(result.value, fallbackChannel(ready.channels));
        return;
      }
      switch (result.failure) {
        case 'held':
          setHeld(true);
          return;
        case 'unchecked':
          onJoined(null, fallbackChannel(ready.channels));
          return;
        case 'unkept':
          setPreview({
            state: 'refused',
            line: 'Your account was made, but this browser will not let the page keep your sign-in. Allow this site to keep data, then ask for a new invite.',
            gone: true,
          });
          return;
        case 'unauthorized':
        case 'network':
        case 'invalid':
          break;
      }
      if (result.code === 'invite_invalid') {
        setPreview({ state: 'refused', line: noLongerWorks, gone: true });
      } else if (result.code === 'principal_exists') {
        setProblem('That name is taken. Choose another.');
      } else if (result.code === 'invalid_request') {
        setProblem(serverLine(result.message, 'The server did not accept that name. Choose another.'));
      } else {
        setProblem(refusalLine(result));
      }
    });
  }

  return (
    <main className="signin invite" aria-labelledby="invite-heading">
      <h1 id="invite-heading" ref={heading} tabIndex={-1}>
        {preview.state === 'refused' && preview.gone ? goneHeading : 'You are invited to Conch'}
      </h1>
      <p>
        Server: <code>{origin}</code>
      </p>
      {preview.state === 'loading' ? <p role="status">Checking the invite…</p> : null}
      {preview.state === 'refused' ? (
        <>
          <p role="alert" className="notice">
            {preview.line}
          </p>
          {/* A person who came by an invite never had a credential to type:
              with nobody signed in, going back would lead only to that form. */}
          {preview.gone ? (
            signedInAs === null ? null : (
              <div className="actions">
                <button type="button" onClick={onBack}>
                  Back to Conch
                </button>
              </div>
            )
          ) : (
            <div className="actions">
              <button
                type="button"
                onClick={() => {
                  setPreview({ state: 'loading' });
                  setAttempt((n) => n + 1);
                }}
              >
                Try again
              </button>
              <button type="button" onClick={onBack}>
                Go back
              </button>
            </div>
          )}
        </>
      ) : null}
      {ready === null ? null : (
        <>
          <dl className="invite-preview" aria-label="What the invite gives">
            <dt>Invited by</dt>
            <dd>{ready.inviter}</dd>
            <dt>Channels</dt>
            <dd>
              <ul className="invite-channels">
                {ready.channels.map((channel) => (
                  <li key={channel}>#{channel}</li>
                ))}
              </ul>
            </dd>
            <dt>Voice</dt>
            <dd>{ready.voice ? 'Included' : 'Not included'}</dd>
            <dt>Expires</dt>
            <dd>
              <time dateTime={ready.expires_at}>{new Date(ready.expires_at).toLocaleString()}</time>
            </dd>
          </dl>
          {signedInAs !== null ? (
            <>
              <p>
                You are signed in here as <strong>{signedInAs}</strong>. This invite makes a new account: continuing as a new
                person signs {signedInAs} out of this browser.
              </p>
              <div className="actions">
                <button type="button" onClick={newPerson} aria-busy={busy}>
                  Continue as a new person
                </button>
                <button type="button" onClick={onBack}>
                  Go back
                </button>
              </div>
            </>
          ) : unchecked || held ? (
            <>
              <p role="alert" className="notice">
                {heldLine}
              </p>
              <div className="actions">
                <button type="button" onClick={onBack}>
                  Go back
                </button>
              </div>
            </>
          ) : (
            <form onSubmit={join}>
              {untold ? <p className="notice">{noticeText('untold')}</p> : null}
              <label htmlFor="invite-name">Your name</label>
              <p id="invite-name-help" className="hint">
                How others see you. Up to 100 characters, with no space at the start or end and no invisible characters. The
                server checks it, and says if it is taken.
              </p>
              <input
                id="invite-name"
                ref={field}
                type="text"
                value={name}
                autoComplete="nickname"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                aria-describedby="invite-name-help invite-problem"
                aria-invalid={problem !== null}
                onChange={(event) => {
                  setName(event.target.value);
                }}
              />
              <button type="submit" aria-busy={busy}>
                {busy ? 'Joining…' : 'Join'}
              </button>
              <p id="invite-problem" role="alert" className="notice">
                {problem ?? ''}
              </p>
              <button type="button" onClick={onBack}>
                Go back
              </button>
            </form>
          )}
        </>
      )}
      <ServerVersion className="hint" />
    </main>
  );
}
