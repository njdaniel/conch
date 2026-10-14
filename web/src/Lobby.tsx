import { useState } from 'react';
import { ChannelList } from './ChannelList';
import type { ChannelsState } from './state/channels';
import { connectionLabel, type ConnectionState } from './state/connection';

// Below this width the right panel starts closed (it collapses first).
const wide = '(min-width: 900px)';

function startsOpen(): boolean {
  return typeof window.matchMedia !== 'function' || window.matchMedia(wide).matches;
}

// The lobby: channels on the left, the open channel in the middle, a
// collapsible panel on the right, a status line at the bottom. The middle and
// the panel are empty until #281 and #282 fill them.
export function Lobby({
  origin,
  name,
  channels,
  connection,
  onOpen,
  onRetry,
  onSignOut,
}: {
  readonly origin: string;
  readonly name: string;
  readonly channels: ChannelsState;
  readonly connection: ConnectionState;
  readonly onOpen: (channel: string) => void;
  readonly onRetry: () => void;
  readonly onSignOut: () => void;
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
          <span id="signout-note" className="hint">
            Signing out only clears this tab. It does not revoke the credential.
          </span>
        </div>
      </header>
      <div className="areas">
        <nav aria-label="Channels" className="channels">
          <h2>Channels</h2>
          {channels.loaded ? (
            <ChannelList channels={channels.channels} selected={channels.selected} onOpen={onOpen} />
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
        <section aria-label="Conversation" tabIndex={0} className="conversation">
          {channels.selected === null ? null : <h2>{channels.selected}</h2>}
        </section>
        <aside aria-label="Members panel" className="panel">
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
          <div id="members-body" hidden={!panelOpen} tabIndex={panelOpen ? 0 : undefined} aria-label="Members list" role="region" />
        </aside>
      </div>
      <footer role="status" aria-label="Status" className="status">
        <span>{name}</span>
        <span>{origin}</span>
        <span>{connectionLabel(connection)}</span>
      </footer>
    </div>
  );
}
