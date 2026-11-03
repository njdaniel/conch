// The Server section's Joining panel, for an operator (issue #714, design §2;
// ADR-014 design §10.2, issue #1018): whether the server's bare address makes
// an account for anyone who asks (open sign-up; a server that predates it
// still reads the older nothing-or-accounts choice), a pause, how long a
// member link lasts when it names no expiry, and the channels the address
// admits to; every invite, with the brake that pauses them all; the live guest
// visits; and End all guest visits, which first says what it would end (a dry
// run) and ends them only on a yes.
//
// Every control keys on whoami (permissions.ts): one the caller cannot use is
// disabled with the reason. The server decides each request again, and a
// refusal is shown by its code and the permission it names.
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
import * as api from '../api';
import { guestName } from '../guestName';
import { InviteSettings } from '../InviteSettings';
import { JoiningSettings } from '../joiningSettings';
import type { ChannelV0 } from '../schema/channel';
import type { EndGuestsResponseV1, GuestAdminV1, JoinSettingsResponseV1, JoinWarning, OpenJoiningMode, PutJoinSettingsRequestV1 } from '../schema/guest';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { InvitesList } from './InvitesList';
import { canDo, reasonFor, refusalLine } from './permissions';

type Line = { readonly text: string; readonly refused: boolean };

// Guests are off for now (issue #1005): the address offers nothing or accounts.
const modes: readonly { readonly mode: OpenJoiningMode; readonly label: string }[] = [
  { mode: 'off', label: 'Nothing: invite-only' },
  { mode: 'account', label: 'Create an account' },
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
    case 'no_default_channel':
      return 'No channel is a default channel, so someone who joins through a link or the address lands in no channel.';
    default:
      // A warning a newer server added (issue #1051): shown by its name.
      return warning;
  }
}

type EndAll =
  | { readonly phase: 'idle' }
  | { readonly phase: 'checking' }
  | { readonly phase: 'asked'; readonly found: EndGuestsResponseV1 }
  | { readonly phase: 'ending' };

const hoursOf = (seconds: number): string => String(seconds / 3600);

export function JoiningPanel({
  who,
  channels,
  onUnauthorized,
}: {
  readonly who: WhoAmIResponseV1;
  readonly channels: readonly ChannelV0[];
  readonly onUnauthorized: () => void;
}) {
  const canChange = canDo(who, 'change_server_settings');
  const canMembers = canDo(who, 'manage_members');
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
  const [mode, setMode] = useState<OpenJoiningMode>('off');
  const [paused, setPaused] = useState(false);
  // Open sign-up and the default link expiry, in hours as typed; only where
  // the server sends the field.
  const [signup, setSignup] = useState(false);
  const [expiry, setExpiry] = useState('');
  const [openChannels, setOpenChannels] = useState<ReadonlySet<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  const [live, setLive] = useState<readonly GuestAdminV1[] | null>(null);
  const [endAll, setEndAll] = useState<EndAll>({ phase: 'idle' });
  const [endLine, setEndLine] = useState<Line | null>(null);

  const take = useCallback((doc: JoinSettingsResponseV1) => {
    setLoaded(doc);
    // A server that still reads a guest mode (an older one) shows as off.
    setMode(doc.settings.open_joining === 'account' ? 'account' : 'off');
    setPaused(doc.settings.joining_paused);
    setSignup(doc.settings.open_signup === true);
    setExpiry(doc.settings.member_link_default_expiry_seconds === undefined ? '' : hoursOf(doc.settings.member_link_default_expiry_seconds));
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
    // Only an edited expiry is checked and sent. One the server holds that is
    // not a whole number of hours (5400 s shows as 1.5) goes back untouched,
    // by not being sent at all.
    const edited = was.member_link_default_expiry_seconds !== undefined && expiry !== hoursOf(was.member_link_default_expiry_seconds);
    const hours = Number(expiry);
    if (edited && (expiry.trim() === '' || !Number.isInteger(hours) || hours < 0)) {
      setLine({ text: 'The default link expiry is a whole number of hours, or 0 for none.', refused: true });
      return;
    }
    const request: PutJoinSettingsRequestV1 = {
      ...(was.open_signup === undefined
        ? mode === (was.open_joining === 'account' ? 'account' : 'off')
          ? {}
          : { open_joining: mode }
        : signup === was.open_signup
          ? {}
          : { open_signup: signup }),
      ...(edited && hours * 3600 !== was.member_link_default_expiry_seconds ? { member_link_default_expiry_seconds: hours * 3600 } : {}),
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
      setLine({ text: refusalLine(result, 'Nothing was changed.'), refused: true });
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
      else setEndLine({ text: refusalLine(result, 'No visit was ended.'), refused: true });
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
      else setEndLine({ text: refusalLine(result, 'No visit was ended.'), refused: true });
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
        Joining
      </button>
      <div id={`${id}-panel`} role="region" aria-label="Joining" hidden={!open} className="guest-settings-panel">
        {loadLine === null ? null : (
          <p role="alert" className="notice">
            {loadLine}
          </p>
        )}
        <InviteSettings open={open} asked={sentHere} onUnauthorized={onUnauthorized} {...(canMembers ? {} : { disabledReason: reasonFor('manage_members') })} />
        <InvitesList who={who} open={open} onUnauthorized={onUnauthorized} />
        {loaded === null ? (
          loadLine === null ? (
            <p className="hint">Loading…</p>
          ) : null
        ) : (
          <>
            <form aria-label="Joining" onSubmit={save}>
              {canChange ? null : <p className="hint">{reasonFor('change_server_settings')}</p>}
              {loaded.settings.open_signup === undefined ? (
                <fieldset disabled={!canChange}>
                  <legend>The server’s address offers</legend>
                  {modes.map((m) => (
                    <label key={m.mode}>
                      <input
                        type="radio"
                        name={`${id}-mode`}
                        checked={mode === m.mode}
                        onChange={() => {
                          setMode(m.mode);
                        }}
                      />{' '}
                      {m.label}
                    </label>
                  ))}
                </fieldset>
              ) : (
                <label>
                  <input
                    type="checkbox"
                    checked={signup}
                    disabled={!canChange}
                    onChange={(event) => {
                      setSignup(event.target.checked);
                    }}
                  />{' '}
                  Open sign-up: anyone who asks at the server’s address can make an account
                </label>
              )}
              <p className="hint">Someone with no invite may join only as the address offers, and only to the channels below.</p>
              {loaded.settings.member_link_default_expiry_seconds === undefined ? null : (
                <>
                  <label htmlFor={`${id}-expiry`}>Default link expiry (hours, 0 for none)</label>
                  <input
                    id={`${id}-expiry`}
                    type="text"
                    inputMode="numeric"
                    value={expiry}
                    disabled={!canChange}
                    onChange={(event) => {
                      setExpiry(event.target.value);
                    }}
                  />
                </>
              )}
              <fieldset disabled={!canChange}>
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
                <p className="hint">Choosing a channel does not show its older private history to people who join.</p>
              </fieldset>
              <label>
                <input
                  type="checkbox"
                  checked={paused}
                  disabled={!canChange}
                  onChange={(event) => {
                    setPaused(event.target.checked);
                  }}
                />{' '}
                Pause joining from the address
              </label>
              <button type="submit" aria-busy={busy} disabled={!canChange}>
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
        {live === null || live.length === 0 ? null : (
          <>
          <h3>Live guest visits</h3>
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
          </>
        )}
        {live === null || live.length === 0 ? null : asked === null ? (
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
