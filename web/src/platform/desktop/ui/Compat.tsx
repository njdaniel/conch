// What the app says of the server's release (issue #946): the state that
// replaces the client for a server that is not on the tested list, and the
// warnings that sit over it. Main decides; this only shows.
import { bridge } from '../access';
import type { CompatView, ProfilesView } from '../bridge';
import { ServerList } from './ServerList';
import { incompatibleLine, overrideLine, unreadable, untested } from './text';

// blocks: the states in which the client is not shown at all.
export function blocks(compat: CompatView): boolean {
  return compat.state === 'unsupported' || compat.state === 'unreachable';
}

export function Unsupported({ view, onView }: { readonly view: ProfilesView; readonly onView: (view: ProfilesView) => void }) {
  const { compat } = view;
  const current = view.profiles.find((p) => p.id === view.active);
  return (
    <main className="signin desktop-chooser">
      <h1>Conch</h1>
      {compat.override ? <DevelopmentNote compat={compat} /> : null}
      <p role="alert" className="notice">
        {compat.state === 'unreachable' ? unreadable(compat.desktop) : untested(compat.server, compat.desktop)}
      </p>
      <p className="hint">Nothing was signed in, and no sign-in is offered for this server.{current === undefined ? '' : ` (${current.host})`}</p>
      {current === undefined ? null : (
        <button
          type="button"
          onClick={() => {
            // Main asks the server again and replaces this page.
            void bridge()?.profileSwitch(current.id);
          }}
        >
          Check again
        </button>
      )}
      <ServerList view={view} onView={onView} heading />
    </main>
  );
}

// DevelopmentNote is the persistent warning of the development flag, and, for
// a server it let through, the untested version.
export function DevelopmentNote({ compat }: { readonly compat: CompatView }) {
  return (
    <section role="note" aria-label="Development build" className="notice desktop-dev-note">
      <p>{overrideLine}</p>
      {compat.state === 'unlisted' ? <p>{untested(compat.server, compat.desktop)}</p> : null}
    </section>
  );
}

export function Incompatible({ compat }: { readonly compat: CompatView }) {
  return compat.invalid === null ? null : (
    <p role="alert" className="notice desktop-incompatible">
      {incompatibleLine(compat.invalid, compat.server, compat.desktop)}
    </p>
  );
}
