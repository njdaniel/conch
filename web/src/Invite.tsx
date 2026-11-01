// The landing page of an invite link (issue #440): what the invite grants,
// then how to join with it, then the lobby. Since issue #713 an invite may
// make an account (a name and a password together, through POST
// /v2/invites/redeem). Guests are removed for now (issue #1005), so the page
// offers an account when the server's preview says the invite allows it, and
// nothing else. The
// code is a prop held in memory by App, sent only in request bodies, and
// never rendered: nothing on this page shows it.
import { useEffect, useRef, useState } from 'react';
import * as api from './api';
import { CreateAccount, type AccountProblem } from './CreateAccount';
import type { InvitePreviewV1 } from './schema/invite';
import type { WhoAmIResponseV1 } from './schema/principal';
import { isNameMessage, nameRefusalLine } from './nameRefusal';
import { ServerVersion } from './ServerVersion';
import { noticeText } from './state/auth';
import { fallbackChannel } from './state/channels';
import { insecureLine, needsHttps } from './transport';

// The one answer for a code that is unknown, used, expired or closed. The
// server does not say which, so neither does the page. Someone who joined from
// a personal link and comes back to it signs in with their name and password,
// or, without one, needs a sign-in link (issue #593); the heading says the
// rest, in other words.
export const noLongerWorks = 'Ask the person who sent it for a new one. If you already joined from this link, sign in instead.';
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

// What the page says when the server refuses a password
// because the connection is not HTTPS (design §5.4): what the operator has to
// do, not just that it failed.
export const httpsRequiredLine =
  'The server takes passwords only over HTTPS, and it does not see this connection as HTTPS. Ask the operator to serve Conch over HTTPS. Behind a reverse proxy, conchd must be started with --trusted-proxies naming the proxy, and the proxy must set X-Forwarded-Proto.';

// A refusal of the server's, as the invite, joining and sign-in calls give it.
export type Refusal = { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string; readonly retryAfter?: number };

// refusalLine is the plain words for an invite the server would not preview
// or redeem, and for the refusals every joining route shares.
export function refusalLine(result: Refusal): string {
  if (result.failure === 'network') return 'Cannot reach the server. Check your connection and try again.';
  switch (result.code) {
    case 'invite_invalid':
      return noLongerWorks;
    case 'invites_paused':
      return 'Invites are paused on this server for now. Try the link again later.';
    case 'rate_limited':
      return `Too many tries from your network. Try again in ${waitWords(result.retryAfter)}.`;
    case 'https_required':
      return httpsRequiredLine;
    case 'accounts_require_auth':
      return 'This server runs without sign-in, so it has no accounts.';
    default:
      return 'The server gave an unexpected reply. Try again.';
  }
}

// accountRefusal is the plain words for a refusal of an account by the
// server, whichever route was making it, and the field it is about; null
// when the refusal is not about the account itself.
export function accountRefusal(result: Refusal): AccountProblem | null {
  switch (result.code) {
    case 'principal_exists':
      return { line: 'That name is taken. Choose another.', field: 'name' };
    case 'password_unacceptable':
      return { line: 'That password is too common, a repeated pattern such as aaaa, blank, or has your name in it. Choose another.', field: 'password' };
    case 'invalid_request':
      // A refusal of the name never shows the server's own words: they speak
      // of a "principal name" (issue #1082).
      if (isNameMessage(result.message)) return { line: nameRefusalLine(result.message), field: 'name' };
      return { line: serverLine(result.message, 'The server did not accept that name or password. Check both and try again.'), field: 'name' };
    case 'joining_closed':
      return { line: 'Accounts cannot be made here now.' };
    case 'joining_paused':
      return { line: 'Joining is paused on this server for now. Try again later.' };
    case 'https_required':
      return { line: httpsRequiredLine };
    default:
      return null;
  }
}

// heldLine is said when this browser holds a sign-in that could not be
// checked. Once the server answers, a reload says whose it is and offers to
// sign it out, as for anyone signed in here.
export const heldLine =
  'This browser holds another sign-in that could not be checked. Reload the page when the server can be reached: it will say who is signed in here and offer to sign them out before you join.';

// What an invite lets a new person do now, from its preview: make an account,
// or nothing. Guests are removed for now (issue #1005): whatever the server
// says of guests, this page offers none.
interface Offer {
  readonly account: boolean;
  readonly paused: boolean;
}

function offerOf(preview: InvitePreviewV1): Offer {
  const join = preview.join;
  if (join === undefined) return { account: true, paused: false };
  return { account: join.account, paused: join.paused === true };
}

// channelWords names an invite's channels in a sentence: "#red and #blue".
export function channelWords(channels: readonly string[]): string {
  const named = channels.map((c) => `#${c}`);
  if (named.length <= 1) return named.join('');
  return `${named.slice(0, -1).join(', ')} and ${named[named.length - 1] ?? ''}`;
}

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
  onSignIn,
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
  // Sign in to an existing account instead, which uses nothing of the
  // invite: App says afterwards if that account is not in the invite's
  // channels.
  readonly onSignIn: (channels: readonly string[], inviter: string) => void;
}) {
  const [preview, setPreview] = useState<Preview>({ state: 'loading' });
  const [attempt, setAttempt] = useState(0);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<AccountProblem | null>(null);
  const [untold, setUntold] = useState(false);
  // Redeeming found another sign-in held here after all.
  const [held, setHeld] = useState(false);
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

  const ready = preview.state === 'ready' ? preview.preview : null;
  const offer = ready === null ? null : offerOf(ready);
  // A page on plain HTTP beyond this machine offers no form that would send
  // a password (design §5.4): the server would refuse it,
  // but only after the password had crossed the network.
  const insecure = needsHttps(origin);
  const way = offer === null || insecure ? null : offer.account ? 'account' : null;

  function newPerson() {
    if (busy) return;
    setBusy(true);
    void onNewPerson().then((told) => {
      setBusy(false);
      setUntold(!told);
    });
  }

  // joinedOr finishes a redemption that went through, or says why it did
  // not; refused is what to do with a refusal of the server's that is not
  // about the invite itself.
  function joinedOr(result: api.RedeemResult, refused: (r: api.InviteRefusal) => void) {
    if (ready === null) return;
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
          line: 'You joined, but this browser will not let the page keep your sign-in. Allow this site to keep data, then sign in, or ask for a new invite.',
          gone: true,
        });
        return;
      case 'unauthorized':
      case 'network':
      case 'invalid':
        break;
    }
    if (result.code === 'invite_invalid') setPreview({ state: 'refused', line: noLongerWorks, gone: true });
    else refused(result);
  }

  function createAccount(name: string, password: string) {
    if (busy || ready === null || insecure) return;
    setBusy(true);
    setProblem(null);
    void api.redeemInvite(inviteCode, name, password).then((result) => {
      setBusy(false);
      joinedOr(result, (refusal) => {
        setProblem(
          refusal.code === 'joining_closed'
            ? { line: 'This invite cannot be used to make an account.' }
            : (accountRefusal(refusal) ?? { line: refusalLine(refusal) }),
        );
      });
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
              with nobody signed in, going back leads to the sign-in page. */}
          {preview.gone ? (
            <div className="actions">
              <button type="button" onClick={onBack}>
                {signedInAs === null ? 'Sign in' : 'Back to Conch'}
              </button>
            </div>
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
      {ready === null || offer === null ? null : (
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
                You are signed in here as <strong>{signedInAs}</strong>. This invite is for someone new: it does not add{' '}
                {signedInAs} to {channelWords(ready.channels)}. If {signedInAs} should be there, ask {ready.inviter} to add
                you. Continuing as a new person signs {signedInAs} out of this browser.
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
            <>
              {untold ? <p className="notice">{noticeText('untold')}</p> : null}
              {insecure ? (
                <p className="notice insecure">{insecureLine}</p>
              ) : offer.paused ? (
                <p className="notice">Joining through this invite is paused for now. Try the link again later.</p>
              ) : !offer.account ? (
                <p className="notice">This invite cannot be used to join right now. Ask {ready.inviter} for another way in.</p>
              ) : null}
              {way === 'account' ? (
                <CreateAccount
                  purpose="invite"
                  busy={busy}
                  problem={problem}
                  onSubmit={createAccount}
                  onCancel={() => {
                    if (busy) return;
                    setProblem(null);
                    onBack();
                  }}
                />
              ) : null}
              <p className="hint">
                Already have an account?{' '}
                <button
                  type="button"
                  className="link"
                  onClick={() => {
                    onSignIn(ready.channels, ready.inviter);
                  }}
                >
                  Sign in instead
                </button>
                . Signing in does not use this invite or add you to its channels.
              </p>
            </>
          )}
        </>
      )}
      <ServerVersion className="hint" />
    </main>
  );
}
