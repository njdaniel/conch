import { useState, type ReactNode } from 'react';
import { ChannelList } from './ChannelList';
import { ServerVersion } from './ServerVersion';
import type { ChannelsState } from './state/channels';
import { connectionLabel, type ConnectionState } from './state/connection';

// Below this width the right panel starts closed (it collapses first).
const wide = '(min-width: 900px)';

function startsOpen(): boolean {
  return typeof window.matchMedia !== 'function' || window.matchMedia(wide).matches;
}

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
  readonly onOpen: (channel: string) => void;
  readonly onRetry: () => void;
  readonly onSignOut: () => void;
  // Sign out everywhere (issue #439), and why it last failed, if it did. A1
  // added no settings panel, so both live in the header.
  readonly onSignOutEverywhere: () => void;
  readonly signOutProblem: string | null;
}) {
  const [panelOpen, setPanelOpen] = useState(startsOpen);

  return (
    <div className="lobby">
      <header>
        <h1>Conch</h1>
        <div className="signout">
          <button type="button" onClick={onSignOut} aria-describedby="signout-note">
            Sign out
          </button>
          <button type="button" onClick={onSignOutEverywhere} aria-describedby="signout-note">
            Sign out everywhere
          </button>
          <span id="signout-note" className="hint">
            Sign out ends this sign-in, in every tab of this browser. Sign out everywhere ends all of yours, on every device.
          </span>
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
