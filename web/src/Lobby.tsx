import { useEffect, useRef, useState, type ReactNode } from 'react';
import { ChannelList } from './ChannelList';
import { ServerVersion } from './ServerVersion';
import type { ChannelsState } from './state/channels';
import { connectionLabel, type ConnectionState } from './state/connection';

// Below this width the right panel starts closed (it collapses first).
const wide = '(min-width: 900px)';

function startsOpen(): boolean {
  return typeof window.matchMedia !== 'function' || window.matchMedia(wide).matches;
}

// What signing out costs, said before it is done (issue #493). A person who
// joined by invite has no password: once their sign-in is revoked, only an
// operator can let them back into this account.
export const signOutWarning = 'To come back into this account, you will need a new link from an operator.';
const signOutQuestion = {
  here: `Sign out of this browser? ${signOutWarning}`,
  everywhere: `Sign out on every device, this browser included? ${signOutWarning}`,
} as const;

// The lobby: channels on the left, the open channel in the middle, a
// collapsible panel on the right, a status line at the bottom. The middle is
// the conversation the caller gives it; the panel is the caller's too (who is here and what is running).
export function Lobby({
  origin,
  name,
  channels,
  unread,
  connection,
  conversation,
  panel,
  voice,
  invites,
  account,
  report,
  onOpen,
  onRetry,
  onSignOut,
  onSignOutEverywhere,
  signOutProblem,
}: {
  readonly origin: string;
  readonly name: string;
  readonly channels: ChannelsState;
  readonly unread: ReadonlyMap<string, number>;
  readonly connection: ConnectionState;
  readonly conversation: ReactNode;
  readonly panel: ReactNode;
  // The voice section: kept by the caller, which owns the voice client.
  readonly voice: ReactNode;
  // "Invite someone" (issue #440), or null for a person who may not invite.
  readonly invites: ReactNode;
  // The caller's own account: changing their name (issue #441). There is no
  // settings page yet, so it sits in the header.
  readonly account?: ReactNode;
  // "Report a problem" (issue #446): in the header beside the sign-outs, and
  // again where the page says it cannot reach the server.
  readonly report?: ReactNode;
  readonly onOpen: (channel: string) => void;
  readonly onRetry: () => void;
  readonly onSignOut: () => void;
  // Sign out everywhere (issue #439), and why it last failed, if it did. A1
  // added no settings panel, so both live in the header.
  readonly onSignOutEverywhere: () => void;
  readonly signOutProblem: string | null;
}) {
  const [panelOpen, setPanelOpen] = useState(startsOpen);
  // Which sign-out is waiting for its yes, if one is.
  const [confirming, setConfirming] = useState<'here' | 'everywhere' | null>(null);
  const cancel = useRef<HTMLButtonElement>(null);
  // The question takes focus on its safe answer, so Enter keeps the account.
  useEffect(() => {
    if (confirming !== null) cancel.current?.focus();
  }, [confirming]);

  return (
    <div className="lobby">
      <header>
        <h1>Conch</h1>
        {account}
        <div className="signout">
          {confirming === null ? (
            <>
              <button
                type="button"
                onClick={() => {
                  setConfirming('here');
                }}
                aria-describedby="signout-note"
              >
                Sign out
              </button>
              <button
                type="button"
                onClick={() => {
                  setConfirming('everywhere');
                }}
                aria-describedby="signout-note"
              >
                Sign out everywhere
              </button>
              {report}
              <span id="signout-note" className="hint">
                Sign out ends this sign-in, in every tab of this browser. Sign out everywhere ends all of yours, on every device.
                Either way, you will need a new link from an operator to come back into this account.
              </span>
            </>
          ) : (
            <div role="group" aria-labelledby="signout-question" className="signout-confirm">
              <p id="signout-question">{signOutQuestion[confirming]}</p>
              <div className="actions">
                <button
                  type="button"
                  onClick={() => {
                    const which = confirming;
                    setConfirming(null);
                    if (which === 'here') onSignOut();
                    else onSignOutEverywhere();
                  }}
                >
                  {confirming === 'here' ? 'Yes, sign out' : 'Yes, sign out everywhere'}
                </button>
                <button
                  ref={cancel}
                  type="button"
                  onClick={() => {
                    setConfirming(null);
                  }}
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
          {signOutProblem === null ? null : (
            <p role="alert" className="notice">
              {signOutProblem}
            </p>
          )}
        </div>
      </header>
      <div className="areas">
        <nav aria-label="Channels" className="channels">
          <h2>Channels</h2>
          {channels.loaded ? (
            <ChannelList channels={channels.channels} selected={channels.selected} unread={unread} onOpen={onOpen} />
          ) : connection === 'reconnecting' ? (
            <div>
              <p className="empty">Cannot reach the server</p>
              <button type="button" onClick={onRetry}>
                Try again
              </button>
              {report}
            </div>
          ) : (
            <p className="empty">Loading channels…</p>
          )}
        </nav>
        <section aria-label="Conversation" className="conversation">
          {channels.lost === undefined ? null : (
            <p role="alert">You no longer have access to #{channels.lost}</p>
          )}
          {conversation}
        </section>
        <aside aria-label="Members panel" className="panel">
          {voice}
          {invites}
          <button
            type="button"
            aria-expanded={panelOpen}
            aria-controls="members-body"
            onClick={() => {
              setPanelOpen(!panelOpen);
            }}
          >
            Members
          </button>
          <div id="members-body" hidden={!panelOpen} tabIndex={panelOpen ? 0 : undefined} aria-label="Members list" role="region">
            {panel}
          </div>
        </aside>
      </div>
      <footer role="status" aria-label="Status" className="status">
        <span>{name}</span>
        <span>{origin}</span>
        <span>{connectionLabel(connection)}</span>
        <ServerVersion />
      </footer>
    </div>
  );
}
