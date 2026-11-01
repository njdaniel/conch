// The strip over the client in the desktop app: the active server's host and
// the account signed in, always (issue #945), and the way to the server list.
// It asks main for the view now and then, because the account is bound when a
// sign-in completes inside the client, which does not tell this strip.
import { useEffect, useState } from 'react';

import { bridge } from '../access';
import type { ProfilesView } from '../bridge';
import { ServerList } from './ServerList';
import { noticeLines } from './text';

const POLL_MS = 2_000;

export function ProfileBar({ initial }: { readonly initial: ProfilesView }) {
  const [view, setView] = useState(initial);
  const [open, setOpen] = useState(false);

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

  return (
    <>
      <header className="desktop-bar" aria-label="Server">
        <span className="desktop-bar-host">{current?.host ?? 'No server'}</span>
        <span className="desktop-bar-account">{current?.account ?? 'not signed in yet'}</span>
        {kept ? null : <span className="desktop-bar-session">sign-in kept until the app closes</span>}
        {view.notice === null ? null : <span className="desktop-bar-notice">{noticeLines[view.notice]}</span>}
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
      {open ? <ServerList view={view} onView={setView} /> : null}
    </>
  );
}
