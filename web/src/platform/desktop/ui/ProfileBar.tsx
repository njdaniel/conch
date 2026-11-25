// The strip over the client in the desktop app: the active server's host and
// the account signed in, always (issue #945), and the way to the server list.
// It asks main for the view now and then, because the account is bound when a
// sign-in completes inside the client, which does not tell this strip.
import { useEffect, useState } from 'react';

import { usePageModal } from '../../../pageModal';
import { bridge, inviteBridge } from '../access';
import type { InviteCheck, ProfilesView } from '../bridge';
import { DevelopmentNote, Incompatible } from './Compat';
import { DesktopSettings } from './DesktopSettings';
import { ServerList } from './ServerList';
import { noticeLines } from './text';

const POLL_MS = 2_000;

export function ProfileBar({ initial }: { readonly initial: ProfilesView }) {
  const [view, setView] = useState(initial);
  const [open, setOpen] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  // A conch:// link that reached the app (issue #1592), as main checked it.
  // It opens the server panel, where the invite choice or refusal is shown.
  // Nothing is contacted or switched until the person's click there.
  const [arrival, setArrival] = useState<InviteCheck | null>(null);

  // Asked for once at mount (a cold start's link waited for this page) and
  // whenever main says one arrived.
  useEffect(() => {
    const b = inviteBridge();
    if (b?.inviteArrived === undefined) return;
    let live = true;
    const ask = (): void => {
      void b.inviteArrived?.().then(
        (answer) => {
          if (!live || answer === null) return;
          setArrival(answer);
          setOpen(true);
        },
        () => undefined,
      );
    };
    ask();
    const stop = b.onInviteArrived?.(ask);
    return () => {
      live = false;
      stop?.();
    };
  }, []);

  useEffect(() => {
    const b = bridge();
    if (b === undefined) return;
    let live = true;
    const timer = setInterval(() => {
      void b.profiles().then(
        (next) => {
          if (live) setView(next);
        },
        () => undefined,
      );
    }, POLL_MS);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, []);

  const current = view.profiles.find((p) => p.id === view.active);
  const kept = view.storage.kind === 'protected';

  // Behind a modal of the lobby the strip, its notes and its panels are inert
  // (pageModal). The strip is a direct child of the page (the layout's rules
  // select it so), so it carries the attribute itself; the rest is in a
  // wrapper that draws nothing of its own.
  const modal = usePageModal();
  return (
    <>
      <header className="desktop-bar" aria-label="Server" inert={modal}>
        <span className="desktop-bar-host">{current?.host ?? 'No server'}</span>
        <span className="desktop-bar-account">{current?.account ?? 'not signed in yet'}</span>
        {kept || view.notice === 'operator-window-only' ? null : <span className="desktop-bar-session">sign-in kept until the app closes</span>}
        {/* The open server list already shows the operator note; say it once. */}
        {view.notice === null || (open && view.notice === 'operator-window-only') ? null : <span className="desktop-bar-notice">{noticeLines[view.notice]}</span>}
        <button
          type="button"
          className="desktop-bar-settings"
          aria-expanded={settingsOpen}
          onClick={() => {
            setSettingsOpen(!settingsOpen);
          }}
        >
          Desktop app
        </button>
        <button
          type="button"
          aria-expanded={open}
          onClick={() => {
            setOpen(!open);
          }}
        >
          Servers
        </button>
      </header>
      <div className="page-scope" inert={modal}>
      {view.compat.override ? <DevelopmentNote compat={view.compat} /> : null}
      <Incompatible compat={view.compat} />
      {settingsOpen ? <DesktopSettings /> : null}
      {open ? (
        <ServerList
          view={view}
          onView={setView}
          arrival={arrival}
          onArrivalTaken={() => {
            setArrival(null);
          }}
        />
      ) : null}
      </div>
    </>
  );
}
