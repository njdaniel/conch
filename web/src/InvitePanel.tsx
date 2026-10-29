// "Invite someone" (issue #440): for a person whoami says may invite. A member
// makes personal invites to their own channels. Only an operator also gets an
// open link, a use cap and the right to invite in turn: only an operator grants
// that (issue #457), and the server refuses it to anyone else anyway. The new
// link is the one place the page shows an invite code, once, to its maker.
// Voice is offered only to someone who holds it, and a member sees how many
// more personal invites they may make, both as whoami says (issue #468).
//
// Whom an invite admits (issue #714, design §2): an account, as every invite
// always has, a guest visit, or either. The choice is an operator's, offered
// only while the joining settings allow guests; without it an invite admits
// accounts alone, and a member's always does here. Guests are still subject
// to the server's switch when the invite is used.
import { useCallback, useContext, useEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { clock } from './Conversation';
import { Glyph } from './Icons';
import { serverLine, waitWords } from './Invite';
import { JoiningSettings } from './joiningSettings';
import { platform } from './platform';
import { inviteLink } from './route';
import type { ChannelV0 } from './schema/channel';
import type { CreateInviteRequestV1, InviteJoinMode, InviteKind, InviteV1 } from './schema/invite';
import type { WhoAmIResponseV1 } from './schema/principal';

// How long an invite lasts. The marked one is the server's default for the
// kind, sent as no expiry at all so the server applies it. None is the
// server's maximum, so a browser clock a little ahead is not refused.
interface Lifetime {
  readonly label: string;
  readonly seconds: number;
  readonly isDefault?: true;
}

const hour = 60 * 60;
const day = 24 * hour;

const lifetimes: Readonly<Record<InviteKind, readonly Lifetime[]>> = {
  personal: [{ label: '1 day', seconds: day }, { label: '7 days', seconds: 7 * day, isDefault: true }, { label: '14 days', seconds: 14 * day }],
  open: [{ label: '1 hour', seconds: hour }, { label: '24 hours', seconds: day, isDefault: true }, { label: '3 days', seconds: 3 * day }],
};

function defaultLifetime(kind: InviteKind): string {
  return (lifetimes[kind].find((l) => l.isDefault === true) ?? lifetimes[kind][0])?.label ?? '';
}

const createRefusals: Readonly<Record<string, string>> = {
  invite_not_allowed: 'You may not make this kind of invite.',
  invite_grant_exceeds_inviter: 'An invite can give only what you have: channels you are in, and voice only if you have it.',
  invite_allowance_exceeded:
    'You already have as many unused personal invites as you may. Close one, or wait until one is used or expires.',
  invites_paused: 'Invites are paused on this server for now.',
  channel_not_found: 'One of those channels does not exist any more.',
  human_required: 'Only people make invites.',
};

// An operator can lift the pause, so is told where (issue #909).
const pausedForOperator = 'Invites are paused on this server. Resume them in Settings → Joining and guests.';

// createLine is the plain words for an invite the server would not make.
function createLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string; readonly retryAfter?: number }): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === 'rate_limited') return `You have made too many invites for now. Try again in ${waitWords(result.retryAfter)}.`;
  if (result.code === 'invalid_request') return serverLine(result.message, 'The server did not accept the invite.');
  return (result.code === undefined ? undefined : createRefusals[result.code]) ?? 'The server refused the invite.';
}

// stateOf is what has become of an invite, from what the server says of it.
function stateOf(invite: InviteV1, now: number): string {
  if (invite.closed_at !== undefined) return 'Closed';
  if (invite.max_uses !== undefined && invite.uses >= invite.max_uses) return 'Used';
  if (Date.parse(invite.expires_at) <= now) return 'Expired';
  return 'Open';
}

function usesOf(invite: InviteV1): string {
  if (invite.kind === 'personal') return invite.uses > 0 ? 'joined' : 'not used yet';
  return invite.max_uses === undefined ? `${String(invite.uses)} joined` : `${String(invite.uses)} of ${String(invite.max_uses)} joined`;
}

export function InvitePanel({
  origin,
  me,
  channels,
  onUnauthorized,
}: {
  readonly origin: string;
  readonly me: WhoAmIResponseV1;
  readonly channels: readonly ChannelV0[];
  readonly onUnauthorized: () => void;
}) {
  const operator = me.role === 'operator';
  const [open, setOpen] = useState(false);
  const toggle = useRef<HTMLButtonElement>(null);
  const [kind, setKind] = useState<InviteKind>('personal');
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const [voice, setVoice] = useState(false);
  const [lifetime, setLifetime] = useState(defaultLifetime('personal'));
  const [capped, setCapped] = useState(false);
  const [cap, setCap] = useState('10');
  const [mayInvite, setMayInvite] = useState(false);
  const [joinAs, setJoinAs] = useState<InviteJoinMode>('account');
  // Whether the joining settings allow guests, as an operator reads them
  // when the panel opens; false until they say so.
  const [guestsAllowed, setGuestsAllowed] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  // Whether the problem is a pause an operator can lift (issue #909).
  const [resume, setResume] = useState(false);
  const joining = useContext(JoiningSettings);
  // The link of the invite just made, and its kind, which says who it lets
  // in: shown until Done, and never again.
  const [link, setLink] = useState<{ readonly url: string; readonly kind: InviteKind } | null>(null);
  const [copied, setCopied] = useState<'no' | 'yes' | 'failed'>('no');
  const [invites, setInvites] = useState<readonly InviteV1[] | null>(null);
  const [listProblem, setListProblem] = useState<string | null>(null);
  // When the list was last read, so a list left open says how old it is.
  const [readAt, setReadAt] = useState<number | null>(null);
  const makeButton = useRef<HTMLButtonElement>(null);
  // whoami as last re-read here: what is left of the allowance changes with
  // every invite made, closed, used or expired, and only the server counts it.
  const [latest, setLatest] = useState<WhoAmIResponseV1 | null>(null);

  const meId = me.id;
  const refresh = useCallback(() => {
    void api.listInvites().then((result) => {
      if (result.ok) {
        // An operator's list may hold everyone's invites (issue #438); this
        // panel is for the person's own.
        setInvites(result.value.filter((invite) => invite.created_by === meId));
        setReadAt(Date.now());
        setListProblem(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setListProblem('Could not load your invites.');
    });
    // Without an answer the count shown is the last one the server gave.
    void api.whoAmI().then((result) => {
      if (result.ok) setLatest(result.value);
      else if (result.failure === 'unauthorized') onUnauthorized();
    });
  }, [meId, onUnauthorized]);

  useEffect(() => {
    if (open) refresh();
  }, [open, refresh]);

  useEffect(() => {
    if (!open || !operator) return;
    let live = true;
    void api.joinSettings().then((result) => {
      if (!live) return;
      const allowed = result.ok && result.value.settings.guests_enabled;
      setGuestsAllowed(allowed);
      if (!allowed) setJoinAs('account');
    });
    return () => {
      live = false;
    };
  }, [open, operator]);

  // An invite is used elsewhere, by someone else: while the panel is open, the
  // list is read again when the person comes back to the page. No timer polls
  // it (issue #593); Refresh reads it on demand.
  useEffect(() => {
    if (!open) return undefined;
    function onChange() {
      if (document.visibilityState !== 'hidden') refresh();
    }
    document.addEventListener('visibilitychange', onChange);
    return () => {
      document.removeEventListener('visibilitychange', onChange);
    };
  }, [open, refresh]);

  // Closing the panel forgets the link it showed.
  useEffect(() => {
    if (!open) setLink(null);
  }, [open]);

  if (me.may_invite !== true) return null;

  const who = latest ?? me;
  // A server that predates voice_allowed says nothing: offer voice, as
  // before, and let the server refuse it (issue #468).
  const voiceAllowed = who.voice_allowed !== false;
  // Absent for an operator, who has no allowance.
  const remaining = who.invites_remaining;
  const cappedUses = Number(cap);
  const capValid = Number.isInteger(cappedUses) && cappedUses >= 1;
  const canCreate = !busy && picked.size > 0 && !(operator && kind === 'open' && capped && !capValid);

  function create(event: SyntheticEvent) {
    event.preventDefault();
    if (!canCreate) return;
    const chosen = lifetimes[kind].find((l) => l.label === lifetime);
    const request: CreateInviteRequestV1 = {
      kind,
      channels: channels.map((c) => c.name).filter((name) => picked.has(name)),
      voice: voiceAllowed && voice,
      ...(chosen === undefined || chosen.isDefault === true ? {} : { expires_at: new Date(Date.now() + chosen.seconds * 1000).toISOString() }),
      ...(operator && kind === 'open' && capped ? { max_uses: cappedUses } : {}),
      ...(operator && mayInvite ? { may_invite: true } : {}),
      ...(operator && guestsAllowed && joinAs !== 'account' ? { join_as: joinAs } : {}),
    };
    setBusy(true);
    setProblem(null);
    setResume(false);
    setLink(null);
    void api.createInvite(request).then((result) => {
      setBusy(false);
      if (result.ok) {
        setLink({ url: inviteLink(origin, result.value.code), kind: result.value.invite.kind });
        setCopied('no');
        refresh();
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else {
        const paused = operator && result.code === 'invites_paused';
        setProblem(paused ? pausedForOperator : createLine(result));
        setResume(paused);
      }
    });
  }

  function copy() {
    if (link === null) return;
    try {
      void platform.services.copyText(link.url).then(
        () => {
          setCopied('yes');
        },
        () => {
          setCopied('failed');
        },
      );
    } catch {
      // No clipboard here (a page not served over https): select it by hand.
      setCopied('failed');
    }
  }

  function close(id: number) {
    void api.closeInvite(id).then((result) => {
      if (result.ok) refresh();
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setListProblem(result.failure === 'network' ? 'Cannot reach the server. Try again.' : 'Could not close that invite.');
    });
  }

  const now = Date.now();

  return (
    <section className="invites" aria-label="Invites">
      <button
        ref={toggle}
        type="button"
        className="icon-button"
        aria-expanded={open}
        aria-controls="invites-body"
        onClick={() => {
          setOpen(!open);
        }}
      >
        <Glyph name="invite" />
        <span className="label">Invite someone</span>
      </button>
      {/* It opens over the page (issue #536): Escape closes it, as it does
          Settings and Members, and focus goes back to its button. */}
      <div
        id="invites-body"
        hidden={!open}
        onKeyDown={(event) => {
          if (event.key !== 'Escape' || event.defaultPrevented) return;
          event.preventDefault();
          setOpen(false);
          toggle.current?.focus();
        }}
      >
        {link === null ? null : (
          <div className="invite-link" role="group" aria-label="New invite link">
            <p>
              {link.kind === 'personal' ? (
                <>
                  <strong>The first person to open this link joins</strong>, and then it stops working.
                </>
              ) : (
                <>
                  <strong>Anyone with this link can join</strong> until it expires or you close it.
                </>
              )}{' '}
              It is shown only now: copy it and send it to the person you are inviting.
            </p>
            <input
              type="text"
              readOnly
              value={link.url}
              aria-label="Invite link"
              onFocus={(event) => {
                event.currentTarget.select();
              }}
            />
            <div className="actions">
              <button type="button" onClick={copy}>
                Copy link
              </button>
              <button
                type="button"
                onClick={() => {
                  // Done unmounts itself: focus goes to Make invite, inside the
                  // panel, so Escape still closes it. The list is read again,
                  // since the link may have been used while it was shown.
                  makeButton.current?.focus();
                  setLink(null);
                  refresh();
                }}
              >
                Done
              </button>
            </div>
            <p role="status" className="hint">
              {copied === 'yes' ? 'Copied.' : copied === 'failed' ? 'Could not copy: select the link and copy it.' : ''}
            </p>
          </div>
        )}
        <form className="invite-form" onSubmit={create}>
          {operator ? (
            <fieldset>
              <legend>Kind</legend>
              {(['personal', 'open'] as const).map((k) => (
                <label key={k}>
                  <input
                    type="radio"
                    name="invite-kind"
                    checked={kind === k}
                    onChange={() => {
                      setKind(k);
                      setLifetime(defaultLifetime(k));
                    }}
                  />{' '}
                  {k === 'personal' ? 'Personal: one person, once' : 'Open link: anyone with it'}
                </label>
              ))}
            </fieldset>
          ) : null}
          <fieldset>
            <legend>Channels</legend>
            {channels.length === 0 ? <p className="hint">You are in no channels to invite to.</p> : null}
            {channels.map((channel) => (
              <label key={channel.id}>
                <input
                  type="checkbox"
                  checked={picked.has(channel.name)}
                  onChange={(event) => {
                    const next = new Set(picked);
                    if (event.target.checked) next.add(channel.name);
                    else next.delete(channel.name);
                    setPicked(next);
                  }}
                />{' '}
                #{channel.name}
              </label>
            ))}
          </fieldset>
          {voiceAllowed ? (
            <label>
              <input
                type="checkbox"
                checked={voice}
                onChange={(event) => {
                  setVoice(event.target.checked);
                }}
              />{' '}
              Voice
            </label>
          ) : null}
          <label>
            Expires after{' '}
            <select
              value={lifetime}
              onChange={(event) => {
                setLifetime(event.target.value);
              }}
            >
              {lifetimes[kind].map((l) => (
                <option key={l.label} value={l.label}>
                  {l.label}
                </option>
              ))}
            </select>
          </label>
          {operator && kind === 'open' ? (
            <div className="invite-cap">
              <label>
                <input
                  type="checkbox"
                  checked={capped}
                  onChange={(event) => {
                    setCapped(event.target.checked);
                  }}
                />{' '}
                Limit the number of people
              </label>
              {capped ? (
                <label>
                  At most{' '}
                  <input
                    type="number"
                    min={1}
                    step={1}
                    value={cap}
                    onChange={(event) => {
                      setCap(event.target.value);
                    }}
                  />
                </label>
              ) : null}
            </div>
          ) : null}
          {operator ? (
            <label>
              <input
                type="checkbox"
                checked={mayInvite}
                onChange={(event) => {
                  setMayInvite(event.target.checked);
                }}
              />{' '}
              Joiners may invite others
            </label>
          ) : null}
          {operator && guestsAllowed ? (
            <fieldset>
              <legend>Who may join with it</legend>
              {(
                [
                  ['account', 'An account: a name and a password'],
                  ['guest', 'A temporary guest'],
                  ['both', 'Either: the joiner chooses'],
                ] as const
              ).map(([mode, words]) => (
                <label key={mode}>
                  <input
                    type="radio"
                    name="invite-join-as"
                    checked={joinAs === mode}
                    onChange={() => {
                      setJoinAs(mode);
                    }}
                  />{' '}
                  {words}
                </label>
              ))}
              <p className="hint">A guest joins only while guests are allowed, and can become an account only if the invite allows accounts.</p>
            </fieldset>
          ) : null}
          {remaining === undefined ? null : (
            <p className="hint">{remaining === 1 ? '1 invite left' : `${String(remaining)} invites left`}</p>
          )}
          <button ref={makeButton} type="submit" disabled={!canCreate} aria-busy={busy}>
            {busy ? 'Making the invite…' : 'Make invite'}
          </button>
          <p role="alert" className="notice">
            {problem ?? ''}
          </p>
          {resume && joining !== null ? (
            <button type="button" onClick={joining.open}>
              Open Settings → Joining and guests
            </button>
          ) : null}
        </form>
        <h3>Your invites</h3>
        {listProblem === null ? null : <p className="notice">{listProblem}</p>}
        {invites === null ? (
          <p className="hint">Loading…</p>
        ) : invites.length === 0 ? (
          <p className="hint">None yet.</p>
        ) : (
          <ul className="invite-list" aria-label="Your invites">
            {invites.map((invite) => {
              const state = stateOf(invite, now);
              return (
                <li key={invite.id}>
                  <span>{invite.kind === 'open' ? 'Open link' : 'Personal'}</span> <span>{invite.channels.map((c) => `#${c.name}`).join(', ')}</span>{' '}
                  <span className="invite-state">{state}</span> <span>· {usesOf(invite)}</span>{' '}
                  <span>
                    · expires <time dateTime={invite.expires_at}>{new Date(invite.expires_at).toLocaleString()}</time>
                  </span>
                  {state === 'Open' ? (
                    <>
                      {' '}
                      <button
                        type="button"
                        onClick={() => {
                          close(invite.id);
                        }}
                      >
                        Close
                      </button>
                    </>
                  ) : null}
                </li>
              );
            })}
          </ul>
        )}
        <button type="button" onClick={refresh}>
          Refresh
        </button>
        {readAt === null ? null : (
          <>
            {' '}
            <span className="hint">
              Read at <time dateTime={new Date(readAt).toISOString()}>{clock(readAt)}</time>
            </span>
          </>
        )}
      </div>
    </section>
  );
}
