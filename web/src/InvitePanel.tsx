// "Invite someone" (issue #440): for a person whoami says may invite. A member
// makes personal invites to their own channels. Only an operator also gets an
// open link, a use cap and the right to invite in turn: only an operator grants
// that (issue #457), and the server refuses it to anyone else anyway. The new
// link is the one place the page shows an invite code, once, to its maker.
import { useCallback, useEffect, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { serverLine, waitWords } from './Invite';
import { inviteLink } from './route';
import type { ChannelV0 } from './schema/channel';
import type { CreateInviteRequestV1, InviteKind, InviteV1 } from './schema/invite';
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
  const [kind, setKind] = useState<InviteKind>('personal');
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const [voice, setVoice] = useState(false);
  const [lifetime, setLifetime] = useState(defaultLifetime('personal'));
  const [capped, setCapped] = useState(false);
  const [cap, setCap] = useState('10');
  const [mayInvite, setMayInvite] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  // The link of the invite just made: shown until Done, and never again.
  const [link, setLink] = useState<string | null>(null);
  const [copied, setCopied] = useState<'no' | 'yes' | 'failed'>('no');
  const [invites, setInvites] = useState<readonly InviteV1[] | null>(null);
  const [listProblem, setListProblem] = useState<string | null>(null);

  const meId = me.id;
  const refresh = useCallback(() => {
    void api.listInvites().then((result) => {
      if (result.ok) {
        // An operator's list may hold everyone's invites (issue #438); this
        // panel is for the person's own.
        setInvites(result.value.filter((invite) => invite.created_by === meId));
        setListProblem(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setListProblem('Could not load your invites.');
    });
  }, [meId, onUnauthorized]);

  useEffect(() => {
    if (open) refresh();
  }, [open, refresh]);

  // Closing the panel forgets the link it showed.
  useEffect(() => {
    if (!open) setLink(null);
  }, [open]);

  if (me.may_invite !== true) return null;

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
      voice,
      ...(chosen === undefined || chosen.isDefault === true ? {} : { expires_at: new Date(Date.now() + chosen.seconds * 1000).toISOString() }),
      ...(operator && kind === 'open' && capped ? { max_uses: cappedUses } : {}),
      ...(operator && mayInvite ? { may_invite: true } : {}),
    };
    setBusy(true);
    setProblem(null);
    setLink(null);
    void api.createInvite(request).then((result) => {
      setBusy(false);
      if (result.ok) {
        setLink(inviteLink(origin, result.value.code));
        setCopied('no');
        refresh();
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setProblem(createLine(result));
    });
  }

  function copy() {
    if (link === null) return;
    try {
      void navigator.clipboard.writeText(link).then(
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
        type="button"
        aria-expanded={open}
        aria-controls="invites-body"
        onClick={() => {
          setOpen(!open);
        }}
      >
        Invite someone
      </button>
      <div id="invites-body" hidden={!open}>
        {link === null ? null : (
          <div className="invite-link" role="group" aria-label="New invite link">
            <p>
              <strong>Anyone with this link can join</strong> until it expires or you close it. It is shown only now: copy it
              and send it to the person you are inviting.
            </p>
            <input
              type="text"
              readOnly
              value={link}
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
                  setLink(null);
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
          <label>
            <input
              type="checkbox"
              checked={voice}
              onChange={(event) => {
                setVoice(event.target.checked);
              }}
            />{' '}
            Voice (only if you have it)
          </label>
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
          <button type="submit" disabled={!canCreate} aria-busy={busy}>
            {busy ? 'Making the invite…' : 'Make invite'}
          </button>
          <p role="alert" className="notice">
            {problem ?? ''}
          </p>
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
      </div>
    </section>
  );
}
