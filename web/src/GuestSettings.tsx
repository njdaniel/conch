// Settings → Joining, for an operator (issue #714, design §2): whether guest
// visits are allowed at all, what the server's bare address offers (nothing,
// guest visits, accounts, or both), a pause, and the channels the address
// admits to; the live guest visits; and End all guest visits, which first
// says what it would end (a dry run) and ends them only on a yes.
//
// The server decides every one of these again, in the transaction that
// writes: settings that cannot stand together are refused
// (join_policy_conflict) and said so here. Changing them, and ending visits,
// need a recent proof of the operator's password and code, which the page
// asks for over this form and then sends the same change again (Reauth.tsx):
// what was chosen here is kept meanwhile.
//
// Guest mode opens nothing that invitations close. Allowing guests changes no
// invite: each invite says whom it admits, and every invite made before
// guests, or without a choice, admits accounts only. The bare address admits
// guests only when its own mode says so, and only to the channels chosen
// here. Turning guests off ends no visit already live.
import { useCallback, useContext, useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { guestName } from './guestName';
import { InviteSettings } from './InviteSettings';
import { JoiningSettings } from './joiningSettings';
import { serverLine } from './Invite';
import type { ChannelV0 } from './schema/channel';
import type { EndGuestsResponseV1, GuestAdminV1, JoinSettingsResponseV1, JoinWarning, OpenJoiningMode, PutJoinSettingsRequestV1 } from './schema/guest';

type Line = { readonly text: string; readonly refused: boolean };

const modes: readonly { readonly mode: OpenJoiningMode; readonly label: string; readonly guests: boolean }[] = [
  { mode: 'off', label: 'Nothing: invite-only', guests: false },
  { mode: 'account', label: 'Create an account', guests: false },
  { mode: 'guest', label: 'Join as guest', guests: true },
  { mode: 'both', label: 'Both', guests: true },
];

// warningLine is what the operator is told of a warning the server gives.
export function warningLine(warning: JoinWarning, live: number): string {
  switch (warning) {
    case 'live_guests_remain':
      return `${live === 1 ? '1 guest visit is' : `${String(live)} guest visits are`} still live. Turning guests off does not end them: End all guest visits does.`;
    case 'trusted_proxy_unset':
      return 'conchd has no trusted proxy set, so callers may all share one address, and the guest limit per address binds them together.';
    case 'one_address':
      return 'Every live guest came from one address.';
    case 'reserved_names_in_use':
      return 'An account has a name kept for guests. Guests cannot be allowed until it is renamed.';
    case 'server_name_in_use':
      return 'An account holds the server’s name, or one that looks the same. It is kept; you may rename it.';
  }
}

function refusalOf(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string }, nothing: string): string {
  if (result.failure === 'network') return `Cannot reach the server. ${nothing}`;
  if (result.code === 'join_policy_conflict' || result.code === 'invalid_request') return serverLine(result.message, `The server refused. ${nothing}`);
  if (result.code === 'reauth_required') return `It needs your password first. ${nothing}`;
  return `The server refused. ${nothing}`;
}

type EndAll =
  | { readonly phase: 'idle' }
  | { readonly phase: 'checking' }
  | { readonly phase: 'asked'; readonly found: EndGuestsResponseV1 }
  | { readonly phase: 'ending' };

export function GuestSettings({ channels, onUnauthorized }: { readonly channels: readonly ChannelV0[]; readonly onUnauthorized: () => void }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  // Expanded when the invite panel sends an operator here (issue #909).
  // It takes focus, which scrolls it into view with the switch below it.
  const sentHere = useContext(JoiningSettings)?.count ?? 0;
  const toggle = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (sentHere === 0) return;
    setOpen(true);
    toggle.current?.focus();
  }, [sentHere]);
  const [loaded, setLoaded] = useState<JoinSettingsResponseV1 | null>(null);
  const [loadLine, setLoadLine] = useState<string | null>(null);
  // What is chosen here, not yet saved.
  const [guests, setGuests] = useState(false);
  const [mode, setMode] = useState<OpenJoiningMode>('off');
  const [paused, setPaused] = useState(false);
  const [openChannels, setOpenChannels] = useState<ReadonlySet<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  const [live, setLive] = useState<readonly GuestAdminV1[] | null>(null);
  const [endAll, setEndAll] = useState<EndAll>({ phase: 'idle' });
  const [endLine, setEndLine] = useState<Line | null>(null);

  const take = useCallback((doc: JoinSettingsResponseV1) => {
    setLoaded(doc);
    setGuests(doc.settings.guests_enabled);
    setMode(doc.settings.open_joining);
    setPaused(doc.settings.joining_paused);
    setOpenChannels(new Set(doc.settings.open_channels));
  }, []);

  const readGuests = useCallback(() => {
    void api.listGuests().then((result) => {
      if (result.ok) setLive(result.value);
      else if (result.failure === 'unauthorized') onUnauthorized();
    });
  }, [onUnauthorized]);

  // Read afresh each time the section opens.
  useEffect(() => {
    if (!open) return;
    let live = true;
    setLoadLine(null);
    void api.joinSettings().then((result) => {
      if (!live) return;
      if (result.ok) take(result.value);
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setLoadLine(result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read the joining settings.');
    });
    readGuests();
    return () => {
      live = false;
    };
  }, [open, take, readGuests, onUnauthorized]);

  function save(event: SyntheticEvent) {
    event.preventDefault();
    if (busy || loaded === null) return;
    const was = loaded.settings;
    const chosen = [...openChannels].sort();
    const request: PutJoinSettingsRequestV1 = {
      ...(guests === was.guests_enabled ? {} : { guests_enabled: guests }),
      ...(mode === was.open_joining ? {} : { open_joining: mode }),
      ...(paused === was.joining_paused ? {} : { joining_paused: paused }),
      ...(chosen.join('\n') === [...was.open_channels].sort().join('\n') ? {} : { open_channels: chosen }),
    };
    if (Object.keys(request).length === 0) {
      setLine({ text: 'Nothing has changed.', refused: false });
      return;
    }
    setBusy(true);
    setLine(null);
    void api.putJoinSettings(request).then((result) => {
      setBusy(false);
      if (result.ok) {
        take(result.value);
        setLine({ text: 'Saved.', refused: false });
        readGuests();
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      // What was chosen stays chosen, to change and save again.
      setLine({ text: refusalOf(result, 'Nothing was changed.'), refused: true });
    });
  }

  function dryRun() {
    if (endAll.phase !== 'idle') return;
    setEndAll({ phase: 'checking' });
    setEndLine(null);
    void api.endAllGuests(true).then((result) => {
      if (result.ok) {
        if (result.value.count === 0) {
          setEndAll({ phase: 'idle' });
          setEndLine({ text: 'No guest visit is live: there is nothing to end.', refused: false });
        } else setEndAll({ phase: 'asked', found: result.value });
        return;
      }
      setEndAll({ phase: 'idle' });
      if (result.failure === 'unauthorized') onUnauthorized();
      else setEndLine({ text: refusalOf(result, 'No visit was ended.'), refused: true });
    });
  }

  function confirmEnd() {
    if (endAll.phase !== 'asked') return;
    setEndAll({ phase: 'ending' });
    void api.endAllGuests(false).then((result) => {
      setEndAll({ phase: 'idle' });
      if (result.ok) {
        const n = result.value.count;
        setEndLine({ text: n === 1 ? 'Ended 1 guest visit.' : `Ended ${String(n)} guest visits.`, refused: false });
        readGuests();
        void api.joinSettings().then((again) => {
          if (again.ok) setLoaded(again.value);
        });
        return;
      }
      if (result.failure === 'unauthorized') onUnauthorized();
      else setEndLine({ text: refusalOf(result, 'No visit was ended.'), refused: true });
    });
  }

  const asked = endAll.phase === 'asked' ? endAll.found : null;

  return (
    <div className="guest-settings">
      <button
        ref={toggle}
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-panel`}
        onClick={() => {
          setOpen(!open);
          setLine(null);
          setEndLine(null);
          setEndAll({ phase: 'idle' });
        }}
      >
        Joining and guests
      </button>
      <div id={`${id}-panel`} role="region" aria-label="Joining and guests" hidden={!open} className="guest-settings-panel">
        {loadLine === null ? null : (
          <p role="alert" className="notice">
            {loadLine}
          </p>
        )}
        <InviteSettings open={open} asked={sentHere} onUnauthorized={onUnauthorized} />
        {loaded === null ? (
          loadLine === null ? (
            <p className="hint">Loading…</p>
          ) : null
        ) : (
          <>
            <form aria-label="Joining" onSubmit={save}>
              <label>
                <input
                  type="checkbox"
                  checked={guests}
                  onChange={(event) => {
                    setGuests(event.target.checked);
                    // The address cannot offer guests while guests are off.
                    if (!event.target.checked && (mode === 'guest' || mode === 'both')) setMode(mode === 'both' ? 'account' : 'off');
                  }}
                />{' '}
                Allow guest visits
              </label>
              <p className="hint">
                The master switch, for invites and the address alike. Allowing guests changes no invite: each says whom it
                admits, and an invite admits accounts only unless its maker chose guests. Turning guests off ends no visit already live.
              </p>
              <fieldset>
                <legend>The server’s address offers</legend>
                {modes.map((m) => (
                  <label key={m.mode}>
                    <input
                      type="radio"
                      name={`${id}-mode`}
                      checked={mode === m.mode}
                      disabled={m.guests && !guests}
                      onChange={() => {
                        setMode(m.mode);
                      }}
                    />{' '}
                    {m.label}
                  </label>
                ))}
              </fieldset>
              <p className="hint">Someone with no invite may join only as the address offers, and only to the channels below.</p>
              <fieldset>
                <legend>Channels the address admits to</legend>
                {channels.length === 0 ? <p className="hint">You are in no channels.</p> : null}
                {channels.map((channel) => (
                  <label key={channel.id}>
                    <input
                      type="checkbox"
                      checked={openChannels.has(channel.name)}
                      onChange={(event) => {
                        const next = new Set(openChannels);
                        if (event.target.checked) next.add(channel.name);
                        else next.delete(channel.name);
                        setOpenChannels(next);
                      }}
                    />{' '}
                    #{channel.name}
                  </label>
                ))}
                <p className="hint">Choosing a channel does not show its older private history to guests.</p>
              </fieldset>
              <label>
                <input
                  type="checkbox"
                  checked={paused}
                  onChange={(event) => {
                    setPaused(event.target.checked);
                  }}
                />{' '}
                Pause joining from the address
              </label>
              <button type="submit" aria-busy={busy}>
                {busy ? 'Saving…' : 'Save joining'}
              </button>
              <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
                {line?.text ?? ''}
              </p>
            </form>
            {loaded.warnings.length === 0 ? null : (
              <ul className="join-warnings" aria-label="Warnings">
                {loaded.warnings.map((w) => (
                  <li key={w}>{warningLine(w, loaded.live_guests)}</li>
                ))}
              </ul>
            )}
          </>
        )}
        <h3>Live guest visits</h3>
        {live === null ? (
          <p className="hint">Loading…</p>
        ) : live.length === 0 ? (
          <p className="hint">None.</p>
        ) : (
          <ul className="signin-list" aria-label="Live guest visits">
            {live.map((g) => (
              <li key={g.id}>
                <span>{guestName(g)}</span>
                <span className="hint">
                  {' '}
                  · {g.origin === 'invite' ? 'through an invite' : 'from the address'} · ends by{' '}
                  <time dateTime={g.expires_at}>{new Date(g.expires_at).toLocaleString()}</time>
                </span>
              </li>
            ))}
          </ul>
        )}
        {asked === null ? (
          <div className="actions">
            <button type="button" className="danger" onClick={dryRun} aria-busy={endAll.phase === 'checking'} disabled={endAll.phase !== 'idle'}>
              {endAll.phase === 'ending' ? 'Ending…' : 'End all guest visits'}
            </button>
          </div>
        ) : (
          <div role="group" aria-labelledby={`${id}-end-question`} className="signout-confirm">
            <p id={`${id}-end-question`}>
              End {asked.count === 1 ? '1 guest visit' : `${String(asked.count)} guest visits`}? Yes ends every visit live at that
              moment, which may be more than counted here if guests join meanwhile. Each guest is signed out at once and cannot come
              back as the same guest. Their messages stay.
            </p>
            {asked.guests.length === 0 ? null : (
              <ul className="end-list" aria-label="Visits it would end">
                {asked.guests.map((g) => (
                  <li key={g.id}>{g.handle}</li>
                ))}
              </ul>
            )}
            <div className="actions">
              <button type="button" className="danger" onClick={confirmEnd}>
                Yes, end {asked.count === 1 ? 'it' : 'them'}
              </button>
              <button
                type="button"
                onClick={() => {
                  setEndAll({ phase: 'idle' });
                }}
              >
                Cancel
              </button>
            </div>
          </div>
        )}
        <p role="status" className={endLine?.refused === true ? 'notice' : 'hint'}>
          {endLine?.text ?? ''}
        </p>
      </div>
    </div>
  );
}
