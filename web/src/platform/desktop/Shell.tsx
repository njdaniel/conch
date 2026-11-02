// The desktop shell's gate in front of the client. The desktop window has no
// server of its own to be served from, so before the existing sign-in views can
// run the person says which server to talk to. Main keeps the saved servers
// (profiles) and the one that is active; this window shows the active one's
// client under a strip with its host and account (./ui/ProfileBar), or, with
// none active, the list of servers to switch to or add (./ui/ServerList).
//
// A switch or a sign-out replaces this whole page: main creates a new window
// in a new in-memory session and destroys this one, so nothing of one server's
// reaches the next.
//
// A server whose release is not on the tested list (or that did not answer
// /healthz) gets no client and no sign-in, only the reason (./ui/Compat; issue
// #946); main refuses the sign-in too.
//
// Once a profile is active, if main already holds a sign-in for it (the page
// was reloaded, or the vault gave it back) the stand-in credential is restored
// before the client starts, so that the client's own restore finds it.
import { useEffect, useState, type ReactNode } from 'react';
import { signedIn } from './credentials';
import { bridge } from './access';
import type { ProfilesView } from './bridge';
import { setServerOrigin } from './state';
import { blocks, DevelopmentNote, Unsupported } from './ui/Compat';
import { ProfileBar } from './ui/ProfileBar';
import { Recovery } from './ui/Recovery';
import { ServerList } from './ui/ServerList';

type Phase = 'loading' | 'recovery' | 'list' | 'blocked' | 'ready' | 'missing';

export function DesktopShell({ children }: { readonly children: ReactNode }) {
  const [phase, setPhase] = useState<Phase>('loading');
  const [view, setView] = useState<ProfilesView | null>(null);

  // enter makes the client's stand-in credential if main holds a sign-in.
  async function enter(origin: string): Promise<void> {
    setServerOrigin(origin);
    const b = bridge();
    const restored = b === undefined ? null : await b.restore().catch(() => null);
    if (restored?.ok === true && restored.who !== null) signedIn(restored.who.role === 'member' && restored.who.guest === undefined);
    setPhase('ready');
  }

  async function show(next: ProfilesView): Promise<void> {
    setView(next);
    if (next.recovery) {
      setPhase('recovery');
      return;
    }
    const active = next.profiles.find((p) => p.id === next.active);
    if (active === undefined) setPhase('list');
    else if (blocks(next.compat)) setPhase('blocked');
    else await enter(active.origin);
  }

  useEffect(() => {
    let live = true;
    const b = bridge();
    if (b === undefined) {
      setPhase('missing');
      return;
    }
    void b
      .profiles()
      .then(async (first) => {
        if (live) await show(first);
      })
      .catch(() => {
        if (live) setPhase('missing');
      });
    return () => {
      live = false;
    };
    // show only uses setters and the bridge; it is meant to run once.
  }, []);

  if (phase === 'ready' && view !== null) {
    return (
      <>
        <ProfileBar initial={view} />
        {children}
      </>
    );
  }
  if (phase === 'loading') {
    return (
      <main className="signin">
        <h1>Conch</h1>
        <p role="status">Starting…</p>
      </main>
    );
  }
  if (phase === 'missing' || view === null) {
    return (
      <main className="signin">
        <h1>Conch</h1>
        <p role="alert" className="notice">
          The desktop app’s connection to its main process is missing, so there is nothing to sign in to. Restart the app.
        </p>
      </main>
    );
  }
  if (phase === 'blocked') return <Unsupported view={view} onView={setView} />;
  if (phase === 'recovery') return <Recovery onView={(next) => void show(next)} />;
  return (
    <main className="signin desktop-chooser">
      <h1>Conch</h1>
      {view.compat.override ? <DevelopmentNote compat={view.compat} /> : null}
      <h2>Server address</h2>
      <p className="hint">The address of the Conch server you use. Your sign-in is kept by the app; whether it lasts after the app closes is said below.</p>
      <ServerList view={view} onView={setView} />
    </main>
  );
}
