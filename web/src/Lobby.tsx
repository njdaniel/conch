import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { ChannelList } from './ChannelList';
import type { InVoice } from './voice/useOccupancy';
import { headingId } from './Conversation';
import { Avatar, Glyph } from './Icons';
import { ServerVersion } from './ServerVersion';
import { StageHost } from './stage';
import type { ChannelsState } from './state/channels';
import { connectionLabel, type ConnectionState } from './state/connection';

// At this width and wider the members panel is a column beside the
// conversation; below it, a panel drawn over the page (issue #536). The same
// width is in app.css.
const wide = '(min-width: 1100px)';

function isWide(): boolean {
  return typeof window.matchMedia !== 'function' || window.matchMedia(wide).matches;
}

// useWide follows the window across the breakpoint. Only classes and focus
// follow it: nothing is mounted or unmounted, so a call or a share carries on.
function useWide(): boolean {
  const [wideNow, setWide] = useState(isWide);
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const query = window.matchMedia(wide);
    const change = () => {
      setWide(query.matches);
    };
    query.addEventListener('change', change);
    return () => {
      query.removeEventListener('change', change);
    };
  }, []);
  return wideNow;
}

// What signing out costs, said before it is done (issue #493). A person
// with no password (one who joined before passwords, or was made by an
// operator) can only be let back into this account by an operator; one with
// a password signs in again with it (issue #713).
export const signOutWarning = 'To come back into this account, you will need a new link from an operator.';
export const passwordSignOutWarning = 'To come back into this account, sign in again with your name and password.';
function signOutQuestion(which: 'here' | 'everywhere', passwordSet: boolean): string {
  const warning = passwordSet ? passwordSignOutWarning : signOutWarning;
  return which === 'here' ? `Sign out of this browser? ${warning}` : `Sign out on every device, this browser included? ${warning}`;
}

// The lobby: a top bar (who, which server, the connection, Invite, Members,
// Settings), channels on the left, the open channel in the middle, the
// members panel on the right, and the voice strip across the bottom. The
// middle is the conversation the caller gives it; the panel is the caller's
// too (who is here and what is running).
export function Lobby({
  origin,
  name,
  me = 0,
  channels,
  unread,
  occupancy,
  notificationCenter,
  connection,
  conversation,
  panel,
  voice,
  invites,
  newChannel,
  renameChannel,
  account,
  signIns,
  report,
  onOpen,
  onRetry,
  onSignOut,
  onSignOutEverywhere,
  signOutProblem,
  passwordSet = false,
  banner,
  visit,
  admin,
  overlay,
}: {
  readonly origin: string;
  readonly name: string;
  // The signed-in principal's id, for the colour of their initial.
  readonly me?: number;
  readonly channels: ChannelsState;
  readonly unread: ReadonlyMap<string, number>;
  // How many are in each channel's voice (issue #596), shown in the list.
  readonly occupancy?: InVoice;
  readonly notificationCenter?: ReactNode;
  readonly connection: ConnectionState;
  readonly conversation: ReactNode;
  readonly panel: ReactNode;
  // The voice section: kept by the caller, which owns the voice client. It is
  // rendered in one place only, the strip, whatever the window's width.
  readonly voice: ReactNode;
  // "Invite someone" (issue #440), or null for a person who may not invite.
  readonly invites: ReactNode;
  // "New channel" (issue #695), under the channel list, or null for a person
  // who may not make one.
  readonly newChannel?: ReactNode;
  // Rename the open channel (issue #728), under the channel list, or null for
  // a person who may not.
  readonly renameChannel?: ReactNode;
  // The caller's standing and own account settings, in Settings.
  readonly account?: ReactNode;
  // "Your sign-ins" (issue #496), in Settings. Ending this browser's own
  // sign-in is signing out, so the section is handed endThis, which asks the
  // sign-out's question.
  readonly signIns?: (endThis: () => void) => ReactNode;
  // "Report a problem" (issue #446): in Settings beside the sign-outs, and
  // again where the page says it cannot reach the server.
  readonly report?: ReactNode;
  readonly onOpen: (channel: string) => void;
  readonly onRetry: () => void;
  readonly onSignOut: () => void;
  // Sign out everywhere (issue #439), and why it last failed, if it did.
  readonly onSignOutEverywhere: () => void;
  readonly signOutProblem: string | null;
  // Whether the person has a password to sign in again with (whoami's
  // auth.password_set), which changes what signing out is said to cost.
  readonly passwordSet?: boolean;
  // Shown above the conversation: a guest's visit, or what signing in from
  // an invite did not do (issue #713).
  readonly banner?: ReactNode;
  // For a temporary guest, what Settings says in place of the sign-outs:
  // a visit ends with End visit, in the banner.
  readonly visit?: ReactNode;
  // The server's own settings, for an operator (issue #714): joining and
  // guests, in Settings.
  readonly admin?: ReactNode;
  // A question over the whole page, Settings included (issue #714: a recent
  // proof of the password). While it is shown everything else is inert and
  // stays as it was, so what was being typed is kept.
  readonly overlay?: ReactNode;
}) {
  const [panelOpen, setPanelOpen] = useState(isWide);
  const wideNow = useWide();
  // A column left open would cover the page once the window narrows: it
  // closes, and Members opens it again.
  const [wasWide, setWasWide] = useState(wideNow);
  if (wasWide !== wideNow) {
    setWasWide(wideNow);
    if (!wideNow) setPanelOpen(false);
  }
  const [settingsOpen, setSettingsOpen] = useState(false);
  const asking = overlay !== undefined && overlay !== null;
  // Which sign-out is waiting for its yes, if one is.
  const [confirming, setConfirming] = useState<'here' | 'everywhere' | null>(null);
  const [stage, setStage] = useState<HTMLElement | null>(null);
  const cancel = useRef<HTMLButtonElement>(null);
  const membersButton = useRef<HTMLButtonElement>(null);
  const membersBody = useRef<HTMLDivElement>(null);
  const settingsButton = useRef<HTMLButtonElement>(null);
  const settingsHeading = useRef<HTMLHeadingElement>(null);
  // The members panel is drawn over the page below the wide breakpoint, and
  // then behaves as one: focus goes into it, and back to Members after.
  const membersOver = panelOpen && !wideNow;

  // The question takes focus on its safe answer, so Enter keeps the account.
  useEffect(() => {
    if (confirming !== null) cancel.current?.focus();
  }, [confirming]);
  // Focus in the members column when the window narrows would be lost as the
  // column closes: it goes to Members first.
  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const query = window.matchMedia(wide);
    const narrowed = () => {
      if (!query.matches && membersBody.current?.contains(document.activeElement) === true) membersButton.current?.focus();
    };
    query.addEventListener('change', narrowed);
    return () => {
      query.removeEventListener('change', narrowed);
    };
  }, []);
  // Focus goes back to Settings once the panel has closed: until then the
  // button is inert behind it, and cannot take focus.
  const settingsClosed = useRef(false);
  useEffect(() => {
    if (settingsOpen) settingsHeading.current?.focus();
    else if (settingsClosed.current) {
      settingsClosed.current = false;
      settingsButton.current?.focus();
    }
  }, [settingsOpen]);
  useEffect(() => {
    if (membersOver) membersBody.current?.focus();
  }, [membersOver]);
  // Arriving (signing in, or a sign-in restored on reload) leaves focus on
  // the page's body, where the next Tab starts from the top: the first time a
  // conversation is shown, focus goes to its heading (issue #590). Only then:
  // opening another channel later leaves focus where it is.
  const arrived = useRef(false);
  const showing = channels.loaded && conversation !== null && conversation !== undefined;
  useEffect(() => {
    if (!showing || arrived.current) return;
    const heading = document.getElementById(headingId);
    if (heading === null) return;
    arrived.current = true;
    if (document.activeElement === null || document.activeElement === document.body) heading.focus();
  }, [showing]);

  function closeSettings() {
    setSettingsOpen(false);
    setConfirming(null);
    settingsClosed.current = true;
  }
  function closeMembers() {
    setPanelOpen(false);
    membersButton.current?.focus();
  }
  // Escape closes the panel focus is in, and the panel's button gets focus.
  const onEscape = (close: () => void) => (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || event.defaultPrevented) return;
    event.preventDefault();
    close();
  };

  return (
    <div className="lobby">
      {/* While Settings is open the rest of the page is inert: the panel is
          modal, and focus cannot wander behind its backdrop. */}
      <header className="topbar" inert={settingsOpen || asking}>
        <h1 className="brand">Conch</h1>
        <div role="status" aria-label="Status" className="status">
          <Avatar id={me} name={name} />
          <span className="status-who">{name}</span>
          <span className="status-server">{origin}</span>
          <span className={`conn conn-${connection}`}>{connectionLabel(connection)}</span>
        </div>
        <div className="topbar-actions">
          {notificationCenter}
          {invites}
          <button
            ref={membersButton}
            type="button"
            className="icon-button"
            aria-expanded={panelOpen}
            aria-controls="members-body"
            onClick={() => {
              setPanelOpen(!panelOpen);
            }}
          >
            <Glyph name="members" />
            <span className="label">Members</span>
          </button>
          <button
            ref={settingsButton}
            type="button"
            className="icon-button"
            aria-expanded={settingsOpen}
            aria-controls="settings-panel"
            onClick={() => {
              if (settingsOpen) closeSettings();
              else setSettingsOpen(true);
            }}
          >
            <Glyph name="settings" />
            <span className="label">Settings</span>
          </button>
        </div>
      </header>
      <div className={panelOpen ? 'areas' : 'areas side-closed'} inert={settingsOpen || asking}>
        <nav aria-label="Channels" className="channels">
          <h2>Channels</h2>
          {channels.loaded ? (
            <>
              <ChannelList
                channels={channels.channels}
                selected={channels.selected}
                unread={unread}
                occupancy={occupancy}
                empty={newChannel === null || newChannel === undefined ? undefined : 'No channels yet'}
                rename={renameChannel}
                onOpen={onOpen}
              />
              {newChannel}
            </>
          ) : connection === 'reconnecting' ? (
            <div className="problem">
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
        <main className="centre">
          <div ref={setStage} className="stage" />
          <section aria-label="Conversation" className="conversation">
            {banner}
            {channels.lost === undefined ? null : (
              <p role="alert" className="banner">
                You no longer have access to #{channels.lost}
              </p>
            )}
            {conversation}
          </section>
        </main>
        <aside aria-label="Members panel" className="panel">
          <div
            ref={membersBody}
            id="members-body"
            hidden={!panelOpen}
            tabIndex={panelOpen ? 0 : undefined}
            aria-label="Members list"
            role="region"
            onKeyDown={membersOver ? onEscape(closeMembers) : undefined}
          >
            {membersOver ? (
              <button type="button" className="icon-button close" onClick={closeMembers}>
                <Glyph name="close" />
                <span className="label">Close</span>
              </button>
            ) : null}
            {panel}
          </div>
        </aside>
        {membersOver ? <div className="backdrop" onClick={closeMembers} /> : null}
      </div>
      <div className="dock" inert={settingsOpen || asking}>
        <StageHost.Provider value={stage}>{voice}</StageHost.Provider>
      </div>
      {settingsOpen && !asking ? <div className="backdrop" onClick={closeSettings} /> : null}
      <div
        id="settings-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby="settings-heading"
        className="drawer" hidden={!settingsOpen} inert={asking} onKeyDown={onEscape(closeSettings)}>
        <div className="drawer-head">
          <h2 id="settings-heading" ref={settingsHeading} tabIndex={-1}>
            Settings
          </h2>
          <button type="button" className="icon-button close" onClick={closeSettings}>
            <Glyph name="close" />
            <span className="label">Close</span>
          </button>
        </div>
        <section aria-label="Account" className="drawer-section">
          <p className="who">
            <Avatar id={me} name={name} />
            <span>
              Signed in as <strong>{name}</strong>
              <br />
              <span className="hint">{origin}</span>
            </span>
          </p>
          {account}
          {signIns?.(() => {
            setConfirming('here');
          })}
        </section>
        {admin === undefined || admin === null ? null : (
          <section aria-label="Server" className="drawer-section">
            {admin}
          </section>
        )}
        <section aria-label="Sign out" className="drawer-section signout">
          {visit !== undefined ? (
            visit
          ) : confirming === null ? (
            <>
              <div className="actions">
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
              </div>
              <p id="signout-note" className="hint">
                Sign out ends this sign-in, in every tab of this browser. Sign out everywhere ends all of yours, on every device.
                {passwordSet
                  ? ' Either way, you sign in again with your name and password.'
                  : ' Either way, you will need a new link from an operator to come back into this account.'}
              </p>
            </>
          ) : (
            <div role="group" aria-labelledby="signout-question" className="signout-confirm">
              <p id="signout-question">{signOutQuestion(confirming, passwordSet)}</p>
              <div className="actions">
                <button
                  type="button"
                  className="danger"
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
        </section>
        <section aria-label="Help" className="drawer-section">
          {report}
          <ServerVersion className="hint" />
        </section>
      </div>
      {overlay}
    </div>
  );
}
