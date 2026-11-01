// The saved servers: switch, add, rename and remove (issue #945). Each entry
// shows the server's host and, once someone has signed in, the account the
// entry is for. Main does every change; this only asks and shows.
import { useId, useRef, useState, type SyntheticEvent } from 'react';

import { bridge } from '../access';
import type { ProfileView, ProfilesView } from '../bridge';
import { StorageStatus } from './StorageStatus';
import { addRefusalLines, example, noticeLines } from './text';

interface Props {
  readonly view: ProfilesView;
  readonly onView: (view: ProfilesView) => void;
  // Shown when the list is the whole screen, not a panel over the client.
  readonly heading?: boolean;
}

export function ServerList({ view, onView, heading = false }: Props) {
  const [line, setLine] = useState('');
  const [busy, setBusy] = useState(false);
  const [renaming, setRenaming] = useState<string | null>(null);
  const [removing, setRemoving] = useState<string | null>(null);
  const field = useRef<HTMLInputElement>(null);
  const id = useId();

  async function refresh(): Promise<void> {
    const b = bridge();
    if (b !== undefined) onView(await b.profiles());
  }

  async function act(run: () => Promise<unknown>): Promise<void> {
    if (busy) return;
    setBusy(true);
    setLine('');
    try {
      await run();
    } catch {
      setLine('The desktop app could not do that.');
    } finally {
      setBusy(false);
    }
  }

  async function add(event: SyntheticEvent): Promise<void> {
    event.preventDefault();
    const b = bridge();
    const typed = field.current?.value.trim() ?? '';
    if (b === undefined) return;
    if (typed === '') {
      field.current?.focus();
      return;
    }
    await act(async () => {
      const added = await b.profileAdd(typed);
      if (!added.ok) {
        setLine(addRefusalLines[added.refusal]);
        return;
      }
      // Main replaces this page with the new server's. The promise may not
      // settle for the page that asked.
      setLine('Opening…');
      await b.profileSwitch(added.id);
    });
  }

  function rename(profile: ProfileView, event: SyntheticEvent<HTMLFormElement>): void {
    event.preventDefault();
    const b = bridge();
    const value = new FormData(event.currentTarget).get('label');
    if (b === undefined || typeof value !== 'string') return;
    void act(async () => {
      if (await b.profileRename(profile.id, value)) {
        setRenaming(null);
        await refresh();
      } else {
        setLine('That name cannot be used. Use 1 to 64 characters of plain text.');
      }
    });
  }

  function remove(profile: ProfileView): void {
    const b = bridge();
    if (b === undefined) return;
    void act(async () => {
      if (await b.profileRemove(profile.id)) {
        setRemoving(null);
        await refresh();
      } else {
        setLine(`${profile.label} could not be removed. Its saved sign-in could not be deleted; try again.`);
      }
    });
  }

  return (
    <section className="desktop-servers" aria-label="Servers">
      {heading ? <h2>Servers</h2> : null}
      {view.notice === null ? null : (
        <p role="status" className="notice">
          {noticeLines[view.notice]}
        </p>
      )}
      <StorageStatus view={view} onView={onView} />
      {view.profiles.length === 0 ? <p className="hint">No servers are saved yet. Add the address of the Conch server you use.</p> : null}
      <ul className="desktop-server-list">
        {view.profiles.map((profile) => (
          <li key={profile.id} className={profile.active ? 'desktop-server desktop-server-active' : 'desktop-server'}>
            {renaming === profile.id ? (
              <form
                className="desktop-server-rename"
                onSubmit={(event) => {
                  rename(profile, event);
                }}
              >
                <label>
                  Name
                  <input name="label" type="text" defaultValue={profile.label} maxLength={64} autoComplete="off" spellCheck={false} />
                </label>
                <button type="submit" disabled={busy}>
                  Save name
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setRenaming(null);
                  }}
                >
                  Cancel
                </button>
              </form>
            ) : (
              <>
                <span className="desktop-server-name">{profile.label}</span>
                <span className="desktop-server-host">{profile.host}</span>
                <span className="desktop-server-account">{profile.account === null ? 'not signed in yet' : profile.account}</span>
                {profile.active ? <span className="desktop-server-current">current</span> : null}
              </>
            )}
            {removing === profile.id ? (
              <div className="desktop-server-confirm" role="alertdialog" aria-label={`Remove ${profile.label}`}>
                <p>
                  Remove {profile.label}? This deletes its saved sign-in and everything the app stored for it on this computer. Your account on the server is not
                  touched.
                </p>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => {
                    remove(profile);
                  }}
                >
                  Remove {profile.label}
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setRemoving(null);
                  }}
                >
                  Keep
                </button>
              </div>
            ) : renaming === profile.id ? null : (
              <span className="desktop-server-actions">
                {profile.active ? null : (
                  <button
                    type="button"
                    disabled={busy}
                    aria-label={`Switch to ${profile.label}`}
                    onClick={() => {
                      const b = bridge();
                      if (b !== undefined) void act(() => b.profileSwitch(profile.id));
                    }}
                  >
                    Switch
                  </button>
                )}
                <button
                  type="button"
                  aria-label={`Rename ${profile.label}`}
                  onClick={() => {
                    setRenaming(profile.id);
                  }}
                >
                  Rename
                </button>
                <button
                  type="button"
                  aria-label={`Remove ${profile.label}…`}
                  onClick={() => {
                    setRemoving(profile.id);
                  }}
                >
                  Remove
                </button>
              </span>
            )}
          </li>
        ))}
      </ul>
      <p role="alert" className="notice">
        {line}
      </p>
      <form
        className="desktop-server-add"
        onSubmit={(event) => {
          void add(event);
        }}
      >
        <label htmlFor={id}>Server address</label>
        <input id={id} ref={field} type="text" inputMode="url" autoComplete="off" spellCheck={false} placeholder={example} />
        <button type="submit" disabled={busy}>
          Continue
        </button>
      </form>
      <p className="hint">Another account on a server you have already saved is another entry: add the same address again.</p>
    </section>
  );
}
