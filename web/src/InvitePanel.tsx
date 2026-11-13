// "Invite someone" (issues #440, #990, ADR-013): member links. Every member
// whose make-member-links right is on makes one, with an optional expiry and
// use limit; the link lands a newcomer in the server's default channels and
// grants nothing else. No expiry named means the server's default (7 days);
// "never expires" is offered to someone who holds manage_members alone, and the
// server decides again. The panel lists the person's own links with their uses
// and joiners, shows who joined through each, and closes them. The new link is
// the one place the page shows an invite code, once, to its maker.
//
// A member whose right is off is told why and is not offered the form. A
// holder of manage_members also gets "Disable joiners" on a link; a member does
// not (design O2). Every refusal is said in the page's own words by its code:
// the server's message is never shown.
import { useCallback, useContext, useEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { DESKTOP_UNAVAILABLE, desktopUnavailableLine } from './appLimit';
import { canDo } from './admin/permissions';
import { clock } from './Conversation';
import { Glyph } from './Icons';
import { waitWords } from './Invite';
import { JoiningSettings } from './joiningSettings';
import { platform } from './platform';
import { inviteLink } from './route';
import type { CreateInviteRequestV2 } from './schema/invite';
import type { InviteJoinerV1, InviteV2 } from './schema/permission';
import type { WhoAmIResponseV1 } from './schema/principal';
import { dateTimeOf } from './time';

// How long a link lasts. The marked one is the server's default, sent as no
// expiry at all so the server applies it. Never is offered to a holder of
// manage_members only, and is sent as never_expires.
interface Lifetime {
  readonly label: string;
  readonly seconds?: number;
  readonly isDefault?: true;
  readonly never?: true;
}

const day = 24 * 60 * 60;
const neverLabel = 'Never expires';

const lifetimes: readonly Lifetime[] = [
  { label: '1 day', seconds: day },
  { label: '7 days', seconds: 7 * day, isDefault: true },
  { label: '30 days', seconds: 30 * day },
];
const neverLifetime: Lifetime = { label: neverLabel, never: true };

const defaultLabel = '7 days';

// The most people one link may admit (pkg/schema MaxInviteMaxUses).
const maxUses = 1_000_000;

// An operator can lift the pause, so is told where (issue #909). A member is
// told the plain line, once a refusal has shown it: they have no route to read
// the pause state (F21).
const pausedForMember = 'Invites are paused on this server for now.';
const pausedForOperator = 'Invites are paused on this server. Resume them in Settings → Joining.';

export const brakeOffLine = 'You may not make invite links. An operator has turned that off for you.';

// Said by a refusal's code, in the page's words.
const createRefusals: Readonly<Record<string, string>> = {
  invite_not_allowed: brakeOffLine,
  invite_grant_exceeds_inviter: 'A link can land people only in channels you are in.',
  invite_allowance_exceeded: 'You already have as many open links as you may. Close one, or wait until one expires.',
  invites_paused: pausedForMember,
  invite_kind_retired: 'This server no longer makes that kind of invite.',
  channel_not_found: 'One of those channels does not exist any more.',
  human_required: 'Only people make invite links.',
  reauth_required: 'It needs your password first.',
};

type Refused = { readonly failure: api.ApiFailure; readonly code?: string; readonly retryAfter?: number };

// createLine is the plain words for a link the server would not make.
function createLine(result: Refused): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === DESKTOP_UNAVAILABLE) return desktopUnavailableLine;
  if (result.code === 'rate_limited') return `You have made too many links for now. Try again in ${waitWords(result.retryAfter)}.`;
  if (result.code === 'invalid_request') return 'The server did not accept those settings. Check the number of people.';
  return (result.code === undefined ? undefined : createRefusals[result.code]) ?? 'The server refused the link.';
}

// otherLine is the words for a close, a read of joiners or a disabling that
// did not go through.
function otherLine(result: Refused, what: string): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === 'permission_denied' || result.code === 'forbidden') return `The server does not allow you to ${what}.`;
  if (result.code === 'invite_not_found') return 'That link is gone.';
  if (result.code === 'reauth_required') return 'It needs your password first.';
  if (result.code === 'rate_limited') return `Too many tries. Try again in ${waitWords(result.retryAfter)}.`;
  return `Could not ${what}.`;
}

// stateOf is what has become of a link, from what the server says of it.
// A link that can still be used reads Unused until someone has used it, and
// Paused while invites are paused. "Open" is also the name of a kind of link,
// so a link never reads Open until it has been used.
function stateOf(invite: InviteV2, now: number, paused: boolean): string {
  if (invite.closed_at !== undefined) return 'Closed';
  if (invite.max_uses !== undefined && invite.uses >= invite.max_uses) return 'Used up';
  if (invite.expires_at !== undefined && Date.parse(invite.expires_at) <= now) return 'Expired';
  if (paused) return 'Paused';
  return invite.uses === 0 ? 'Unused' : 'Open';
}

// A link no one has closed, used up or let expire: Close still applies.
const closable = (state: string) => state === 'Unused' || state === 'Open' || state === 'Paused';

function usesOf(invite: InviteV2): string {
  return invite.max_uses === undefined ? `${String(invite.uses)} joined` : `${String(invite.uses)} of ${String(invite.max_uses)} joined`;
}

type Joiners =
  | { readonly state: 'loading' }
  | { readonly state: 'shown'; readonly joiners: readonly InviteJoinerV1[] }
  | { readonly state: 'failed'; readonly line: string };

// A question before disabling joiners: how many, from the dry run.
type Asking = { readonly id: number; readonly count: number };

export function InvitePanel({
  origin,
  me,
  request,
  onUnauthorized,
}: {
  readonly origin: string;
  readonly me: WhoAmIResponseV1;
  // Opens the panel from outside (issue #1135: a channel's ⋯ menu, "Invite
  // people"). A member link names no channel, so none is passed. `n` counts
  // the asks, so asking twice opens it twice.
  readonly request?: { readonly n: number } | undefined;
  readonly onUnauthorized: () => void;
}) {
  // GET /v1/invites/settings is role-gated (accessOperator), so reading the
  // pause stays by role; lifting it (PUT) needs manage_members: holdsMembers.
  const operator = me.role === 'operator';
  const [open, setOpen] = useState(false);
  const toggle = useRef<HTMLButtonElement>(null);
  const [lifetime, setLifetime] = useState(defaultLabel);
  const [capped, setCapped] = useState(false);
  const [cap, setCap] = useState('10');
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string | null>(null);
  const joining = useContext(JoiningSettings);
  // The link just made: shown until Done, and never again.
  const [link, setLink] = useState<{ readonly url: string; readonly id: number } | null>(null);
  // Whether invites are paused: read by an operator, and for a member the
  // sign is a refusal as paused, since they have no route to read it.
  const [paused, setPaused] = useState(false);
  // A member learns of a pause only from a refusal, so what was learnt is
  // forgotten when the panel closes: the next opening asks again.
  useEffect(() => {
    if (!open && !operator) setPaused(false);
  }, [open, operator]);
  const [copied, setCopied] = useState<'no' | 'yes' | 'failed'>('no');
  const [invites, setInvites] = useState<readonly InviteV2[] | null>(null);
  const [listProblem, setListProblem] = useState<string | null>(null);
  // When the list was last read, so a list left open says how old it is.
  const [readAt, setReadAt] = useState<number | null>(null);
  const [joiners, setJoiners] = useState<ReadonlyMap<number, Joiners>>(new Map());
  const [asking, setAsking] = useState<Asking | null>(null);
  const [disabled, setDisabled] = useState<string | null>(null);
  const makeButton = useRef<HTMLButtonElement>(null);
  const refreshButton = useRef<HTMLButtonElement>(null);
  // Set at once when a link is asked for, since busy changes only on the next
  // render: a second press before then sends nothing.
  const making = useRef(false);
  // whoami as last re-read here: what is left of the allowance changes with
  // every link made, closed, used or expired, and only the server counts it.
  const [latest, setLatest] = useState<WhoAmIResponseV1 | null>(null);
  // What the page was told and what the server last said must both allow it: a
  // right turned off since is shown off as soon as the panel reads it again,
  // and the form never shows more than the server answered.
  const mayMake = me.may_invite === true && latest?.may_invite !== false;
  // Whoever holds manage_members may ask for a link that never expires and
  // disable the people a link brought in. As for mayMake, what the page was
  // told and what the server last said must both allow it (canDo reads
  // whoami's permissions, and takes an operator of a server that sends none
  // to hold them all).
  const holdsMembers = canDo(me, 'manage_members') && (latest === null || canDo(latest, 'manage_members'));
  // Losing a right closes what it opened: the question before disabling
  // joiners, the link just shown, and a choice of "Never expires".
  useEffect(() => {
    if (!holdsMembers) {
      setAsking(null);
      setLifetime((now) => (now === neverLabel ? defaultLabel : now));
    }
  }, [holdsMembers]);
  useEffect(() => {
    if (!mayMake) {
      setAsking(null);
      setLink(null);
    }
  }, [mayMake]);

  const meId = me.id;
  const refresh = useCallback(() => {
    void api.memberLinks().then((result) => {
      if (result.ok) {
        // A holder of manage_members is answered everyone's links (issue
        // #438); this panel is for the person's own.
        const mine = result.value.filter((invite) => invite.created_by === meId);
        setInvites(mine);
        // The link shown only once is gone as soon as it is seen used.
        setLink((shown) => (shown !== null && mine.some((invite) => invite.id === shown.id && invite.uses > 0) ? null : shown));
        setReadAt(Date.now());
        setListProblem(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else {
        if (result.code === 'invites_paused') setPaused(true);
        setListProblem('Could not load your links.');
      }
    });
    if (operator) {
      void api.inviteSettings().then((result) => {
        if (result.ok) setPaused(result.value.paused);
      });
    }
    // Without an answer the count shown is the last one the server gave.
    void api.whoAmI().then((result) => {
      if (result.ok) setLatest(result.value);
      else if (result.failure === 'unauthorized') onUnauthorized();
    });
  }, [meId, operator, onUnauthorized]);

  useEffect(() => {
    if (open && mayMake) refresh();
  }, [open, mayMake, refresh]);

  // A link is used elsewhere, by someone else: while the panel is open, the
  // list is read again when the person comes back to the page. No timer polls
  // it (issue #593); Refresh reads it on demand.
  useEffect(() => {
    if (!open || !mayMake) return undefined;
    function onChange() {
      if (document.visibilityState !== 'hidden') refresh();
    }
    document.addEventListener('visibilitychange', onChange);
    return () => {
      document.removeEventListener('visibilitychange', onChange);
    };
  }, [open, mayMake, refresh]);

  // Asked for from outside: open, and take focus into the panel once shown.
  const body = useRef<HTMLDivElement>(null);
  // Counts the links asked for and the times the panel was re-aimed or
  // finished with; an answer from an older count is dropped.
  const generation = useRef(0);
  const [focusAsk, setFocusAsk] = useState(0);
  const asked = request?.n ?? 0;
  useEffect(() => {
    if (asked === 0) return;
    setOpen(true);
    // An earlier link is forgotten, and an answer still on its way changes
    // nothing.
    generation.current++;
    making.current = false;
    setBusy(false);
    setLink(null);
    setCopied('no');
    setProblem(null);
    setFocusAsk(asked);
  }, [asked]);
  useEffect(() => {
    if (focusAsk > 0) body.current?.querySelector<HTMLElement>('input, button')?.focus();
  }, [focusAsk]);

  // Closing the panel forgets the link it showed.
  useEffect(() => {
    if (!open) setLink(null);
  }, [open]);

  const who = latest ?? me;
  // Absent for an operator, who has no allowance.
  const remaining = who.invites_remaining;
  const cappedUses = Number(cap);
  const capValid = Number.isInteger(cappedUses) && cappedUses >= 1 && cappedUses <= maxUses;
  const canCreate = !busy && !(capped && !capValid);
  const choices = holdsMembers ? [...lifetimes, neverLifetime] : lifetimes;

  function create(event: SyntheticEvent) {
    event.preventDefault();
    if (!canCreate || making.current) return;
    making.current = true;
    const chosen = choices.find((l) => l.label === lifetime);
    const made: CreateInviteRequestV2 = {
      kind: 'member',
      ...(chosen?.never === true ? { never_expires: true } : {}),
      ...(chosen === undefined || chosen.isDefault === true || chosen.seconds === undefined
        ? {}
        : { expires_at: new Date(Date.now() + chosen.seconds * 1000).toISOString() }),
      ...(capped ? { max_uses: cappedUses } : {}),
    };
    setBusy(true);
    setProblem(null);
    setLink(null);
    const mine = ++generation.current;
    void api.createMemberLink(made).then((result) => {
      // Asked again (or Done) meanwhile: this answer is for what was asked
      // then, and shows nothing.
      making.current = false;
      if (mine !== generation.current) return;
      setBusy(false);
      if (result.ok) {
        setLink({ url: inviteLink(origin, result.value.code), id: result.value.invite.id });
        setCopied('no');
        if (!operator) setPaused(false);
        refresh();
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else {
        // A pause is the banner's to say, not the problem line's.
        if (result.code === 'invites_paused') {
          setPaused(true);
          setProblem(null);
        } else setProblem(createLine(result));
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
    // The button that was pressed goes with the row's state: focus stays on
    // the panel's own Refresh.
    refreshButton.current?.focus();
    void api.closeInvite(id).then((result) => {
      if (result.ok) refresh();
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setListProblem(otherLine(result, 'close that link'));
    });
  }

  function showJoiners(id: number) {
    if (joiners.has(id)) {
      const next = new Map(joiners);
      next.delete(id);
      setJoiners(next);
      return;
    }
    setJoiners(new Map(joiners).set(id, { state: 'loading' }));
    void api.memberLinkJoiners(id).then((result) => {
      if (result.ok) setJoiners((now) => (now.has(id) ? new Map(now).set(id, { state: 'shown', joiners: result.value }) : now));
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setJoiners((now) => (now.has(id) ? new Map(now).set(id, { state: 'failed', line: otherLine(result, 'read who joined') }) : now));
    });
  }

  // Disabling joiners asks first, from a dry run that says how many.
  function askDisable(id: number) {
    if (!holdsMembers) return;
    setDisabled(null);
    void api.disableJoiners(id, true).then((result) => {
      if (result.ok) setAsking({ id, count: result.value.count });
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setListProblem(otherLine(result, 'disable the people who joined'));
    });
  }

  function disable(id: number) {
    setAsking(null);
    if (!holdsMembers) return;
    void api.disableJoiners(id, false).then((result) => {
      if (result.ok) {
        setDisabled(result.value.count === 1 ? 'Disabled 1 person.' : `Disabled ${String(result.value.count)} people.`);
        setJoiners(new Map());
        refresh();
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setListProblem(otherLine(result, 'disable the people who joined'));
    });
  }

  const now = Date.now();
  // Focus goes into the panel when the toggle opens it (a count, as `asked`).
  const closePanel = () => {
    setOpen(false);
    toggle.current?.focus();
  };

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
          if (!open) setFocusAsk((n) => n + 1);
        }}
        onKeyDown={(event) => {
          if (event.key !== 'Escape' || event.defaultPrevented || !open) return;
          event.preventDefault();
          closePanel();
        }}
      >
        <Glyph name="invite" />
        <span className="label">Invite someone</span>
      </button>
      {/* It opens over the page (issue #536): Escape closes it, as it does
          Settings and Members, and focus goes back to its button. */}
      <div
        id="invites-body"
        ref={body}
        hidden={!open}
        onKeyDown={(event) => {
          if (event.key !== 'Escape' || event.defaultPrevented) return;
          event.preventDefault();
          closePanel();
        }}
      >
        {!mayMake ? (
          <p className="notice" role="status">
            {brakeOffLine}
          </p>
        ) : (
          <>
            {paused ? (
              <div className="notice" role="status">
                <p>{holdsMembers ? pausedForOperator : pausedForMember}</p>
                {holdsMembers && joining !== null ? (
                  <button type="button" onClick={joining.open}>
                    Open Settings → Joining
                  </button>
                ) : null}
              </div>
            ) : null}
            {link === null ? null : (
              <div className="invite-link" role="group" aria-label="New invite link">
                <p>
                  <strong>Anyone with this link can join</strong>, until it expires, is used up or you close it. It is shown only now: copy
                  it and send it to the people you are inviting.
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
                      // Done unmounts itself: focus goes to Make link, inside the
                      // panel, so Escape still closes it. The list is read again,
                      // since the link may have been used while it was shown.
                      makeButton.current?.focus();
                      generation.current++;
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
              <p className="hint">A link lands the people who use it in the server&apos;s default channels.</p>
              <label>
                Expires after{' '}
                <select
                  value={lifetime}
                  onChange={(event) => {
                    setLifetime(event.target.value);
                  }}
                >
                  {choices.map((l) => (
                    <option key={l.label} value={l.label}>
                      {l.label}
                    </option>
                  ))}
                </select>
              </label>
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
                      max={maxUses}
                      step={1}
                      value={cap}
                      onChange={(event) => {
                        setCap(event.target.value);
                      }}
                    />
                  </label>
                ) : null}
              </div>
              {remaining === undefined ? null : <p className="hint">{remaining === 1 ? '1 link left' : `${String(remaining)} links left`}</p>}
              <button ref={makeButton} type="submit" disabled={!canCreate} aria-busy={busy}>
                {busy ? 'Making the link…' : 'Make link'}
              </button>
              <p role="alert" className="notice">
                {problem ?? ''}
              </p>
            </form>
            <h3>Your links</h3>
            {listProblem === null ? null : <p className="notice">{listProblem}</p>}
            {disabled === null ? null : (
              <p role="status" className="hint">
                {disabled}
              </p>
            )}
            {invites === null ? (
              <p className="hint">Loading…</p>
            ) : invites.length === 0 ? (
              <p className="hint">None yet.</p>
            ) : (
              <ul className="invite-list" aria-label="Your links">
                {invites.map((invite) => {
                  const state = stateOf(invite, now, paused);
                  const seen = joiners.get(invite.id);
                  return (
                    <li key={invite.id}>
                      <span className="invite-state">{state}</span> <span>· {usesOf(invite)}</span>{' '}
                      <span>
                        ·{' '}
                        {invite.expires_at === undefined ? (
                          'never expires'
                        ) : (
                          <>
                            expires <time dateTime={invite.expires_at}>{dateTimeOf(invite.expires_at)}</time>
                          </>
                        )}
                      </span>
                      <div className="actions">
                        {invite.joiners === 0 ? null : (
                          <button
                            type="button"
                            aria-expanded={seen !== undefined}
                            onClick={() => {
                              showJoiners(invite.id);
                            }}
                          >
                            {seen === undefined ? `Joiners (${String(invite.joiners)})` : 'Hide joiners'}
                          </button>
                        )}
                        {holdsMembers && invite.joiners > 0 ? (
                          <button
                            type="button"
                            onClick={() => {
                              askDisable(invite.id);
                            }}
                          >
                            Disable joiners
                          </button>
                        ) : null}
                        {closable(state) ? (
                          <button
                            type="button"
                            onClick={() => {
                              close(invite.id);
                            }}
                          >
                            Close
                          </button>
                        ) : null}
                      </div>
                      {!holdsMembers || asking === null || asking.id !== invite.id ? null : (
                        <div className="invite-ask" role="group" aria-label="Disable joiners">
                          <p>
                            {asking.count === 0
                              ? 'Nobody who joined through this link can be disabled.'
                              : `This signs out and disables ${asking.count === 1 ? '1 person' : `${String(asking.count)} people`} who joined through this link, and closes the link too.`}
                          </p>
                          <div className="actions">
                            {asking.count === 0 ? null : (
                              <button
                                type="button"
                                onClick={() => {
                                  refreshButton.current?.focus();
                                  disable(invite.id);
                                }}
                              >
                                Disable {asking.count === 1 ? '1 person' : `${String(asking.count)} people`}
                              </button>
                            )}
                            <button
                              type="button"
                              onClick={() => {
                                refreshButton.current?.focus();
                                setAsking(null);
                              }}
                            >
                              Cancel
                            </button>
                          </div>
                        </div>
                      )}
                      {seen === undefined ? null : seen.state === 'loading' ? (
                        <p className="hint">Loading…</p>
                      ) : seen.state === 'failed' ? (
                        <p className="notice">{seen.line}</p>
                      ) : (
                        <ul className="invite-joiners" aria-label="Joiners of this link">
                          {seen.joiners.map((joiner) => (
                            <li key={joiner.id}>
                              {joiner.name}
                              {joiner.disabled ? ' (disabled)' : ''}
                            </li>
                          ))}
                        </ul>
                      )}
                    </li>
                  );
                })}
              </ul>
            )}
            <button ref={refreshButton} type="button" onClick={refresh}>
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
          </>
        )}
      </div>
    </section>
  );
}
