// The Server section's Invites (ADR-014 design §10.2, issue #1018): every
// member link the caller may see, who made it, how many have joined through
// it and who, and a way to close it. A holder of manage_members sees every
// link; anyone else sees their own. Without manage_channels a link names only
// the channels the caller is in, and the server counts the rest: it is said
// here as "and 2 other channels", never guessed at.
//
// Disable joiners is for a holder of manage_members. It asks first: a dry run
// says how many people it would disable, the question names that number, and
// one send follows a yes (which also closes the link). The question closes if
// the permission goes.
//
// Closing is the maker's or a holder of manage_members's. The page disables
// it for anyone else, with the reason; the server decides again and its
// refusal is shown by its code and permission.
import { useCallback, useEffect, useRef, useState } from 'react';
import * as api from '../api';
import type { InviteJoinerV1, InviteV2 } from '../schema/permission';
import type { WhoAmIResponseV1 } from '../schema/principal';
import { canDo, reasonFor, refusalLine } from './permissions';

type Line = { readonly text: string; readonly refused: boolean };

function when(at: string) {
  return <time dateTime={at}>{new Date(at).toLocaleString()}</time>;
}

// channelsLine names a link's channels, and counts those it leaves out.
export function channelsLine(invite: InviteV2): string {
  const named = (invite.channels ?? []).map((c) => `#${c.name}`);
  const other = invite.other_channels ?? 0;
  const others = other === 0 ? '' : `${String(other)} other ${other === 1 ? 'channel' : 'channels'}`;
  if (named.length === 0) return others === '' ? 'No channel' : `Only ${others}`;
  return others === '' ? named.join(', ') : `${named.join(', ')} and ${others}`;
}

export function InvitesList({ who, open, onUnauthorized }: { readonly who: WhoAmIResponseV1; readonly open: boolean; readonly onUnauthorized: () => void }) {
  const [links, setLinks] = useState<readonly InviteV2[] | null>(null);
  const [loadLine, setLoadLine] = useState<string | null>(null);
  const [line, setLine] = useState<Line | null>(null);
  const [joiners, setJoiners] = useState<ReadonlyMap<number, readonly InviteJoinerV1[]>>(new Map());
  const [closing, setClosing] = useState<number | null>(null);
  const everyone = canDo(who, 'manage_members');
  const [asking, setAsking] = useState<{ readonly id: number; readonly count: number } | null>(null);
  const [sending, setSending] = useState(false);
  const heading = useRef<HTMLHeadingElement>(null);
  const askButtons = useRef(new Map<number, HTMLButtonElement>());

  // Losing manage_members closes the question, and nothing more is sent.
  useEffect(() => {
    if (!everyone) setAsking(null);
  }, [everyone]);

  const read = useCallback(() => {
    void api.memberLinks().then((result) => {
      if (result.ok) {
        setLinks(result.value);
        setLoadLine(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setLoadLine(result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read the invites.');
    });
  }, [onUnauthorized]);

  // Read afresh each time the panel opens.
  useEffect(() => {
    if (open) read();
  }, [open, read]);

  function showJoiners(invite: InviteV2) {
    if (joiners.has(invite.id)) {
      const next = new Map(joiners);
      next.delete(invite.id);
      setJoiners(next);
      return;
    }
    void api.memberLinkJoiners(invite.id).then((result) => {
      if (result.ok) setJoiners((was) => new Map(was).set(invite.id, result.value));
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setLine({ text: refusalLine(result, 'The joiners were not read.'), refused: true });
    });
  }

  function close(invite: InviteV2) {
    if (closing !== null) return;
    setClosing(invite.id);
    setLine(null);
    void api.closeInvite(invite.id).then((result) => {
      setClosing(null);
      if (result.ok) {
        setLine({ text: `Link #${String(invite.id)} is closed.`, refused: false });
        read();
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setLine({ text: refusalLine(result, 'The link was not closed.'), refused: true });
    });
  }

  function askDisable(invite: InviteV2) {
    if (!everyone || sending) return;
    setLine(null);
    void api.disableJoiners(invite.id, true).then((result) => {
      if (result.ok) setAsking({ id: invite.id, count: result.value.count });
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setLine({ text: refusalLine(result, 'No one was disabled.'), refused: true });
    });
  }

  function disable(invite: InviteV2) {
    if (!everyone || sending) return;
    setAsking(null);
    setSending(true);
    heading.current?.focus();
    void api.disableJoiners(invite.id, false).then((result) => {
      setSending(false);
      if (result.ok) {
        setLine({ text: result.value.count === 1 ? 'Disabled 1 person.' : `Disabled ${String(result.value.count)} people.`, refused: false });
        setJoiners(new Map());
        read();
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setLine({ text: refusalLine(result, 'No one was disabled.'), refused: true });
    });
  }

  function cancelAsk(id: number) {
    askButtons.current.get(id)?.focus();
    setAsking(null);
  }

  return (
    <div className="invites-list">
      <h3 ref={heading} tabIndex={-1}>
        Invites
      </h3>
      {loadLine === null ? null : (
        <p role="alert" className="notice">
          {loadLine}
        </p>
      )}
      {links === null ? (
        loadLine === null ? (
          <p className="hint">Loading…</p>
        ) : null
      ) : links.length === 0 ? (
        <p className="hint">{everyone ? 'No member links have been made.' : 'You have made no member links.'}</p>
      ) : (
        <ul className="signin-list" aria-label="Member links">
          {links.map((invite) => {
            const mine = invite.created_by === who.id;
            const closeable = invite.closed_at === undefined && (everyone || mine);
            const seen = joiners.get(invite.id);
            return (
              <li key={invite.id}>
                <span>
                  Link #{invite.id} · made by {invite.created_by_name}
                </span>
                <span className="hint">
                  {' '}
                  · {invite.max_uses === undefined ? `${String(invite.uses)} uses` : `${String(invite.uses)} of ${String(invite.max_uses)} uses`} ·{' '}
                  {invite.joiners === 1 ? '1 joiner' : `${String(invite.joiners)} joiners`} ·{' '}
                  {invite.closed_at !== undefined ? (
                    <>closed {when(invite.closed_at)}</>
                  ) : invite.expires_at === undefined ? (
                    'never expires'
                  ) : (
                    <>expires {when(invite.expires_at)}</>
                  )}
                </span>
                <span className="hint">
                  {' '}
                  · {channelsLine(invite)}
                </span>
                <span className="actions">
                  {invite.joiners === 0 ? null : (
                    <button type="button" aria-expanded={seen !== undefined} onClick={() => { showJoiners(invite); }}>
                      {seen === undefined ? `Show joiners of link ${String(invite.id)}` : `Hide joiners of link ${String(invite.id)}`}
                    </button>
                  )}
                  {everyone && invite.joiners > 0 ? (
                    <button
                      type="button"
                      disabled={sending}
                      ref={(el) => {
                        if (el === null) askButtons.current.delete(invite.id);
                        else askButtons.current.set(invite.id, el);
                      }}
                      onClick={() => {
                        askDisable(invite);
                      }}
                    >
                      Disable joiners of link {invite.id}
                    </button>
                  ) : null}
                  {invite.closed_at !== undefined ? null : (
                    <button
                      type="button"
                      disabled={!closeable || closing !== null}
                      aria-busy={closing === invite.id}
                      aria-describedby={closeable ? undefined : `invite-${String(invite.id)}-why`}
                      onClick={() => {
                        close(invite);
                      }}
                    >
                      Close link {invite.id}
                    </button>
                  )}
                </span>
                {closeable || invite.closed_at !== undefined ? null : (
                  <span id={`invite-${String(invite.id)}-why`} className="hint">
                    {reasonFor('manage_members')}
                  </span>
                )}
                {!everyone || asking === null || asking.id !== invite.id ? null : (
                  <div role="group" aria-label={`Disable joiners of link ${String(invite.id)}`}>
                    <p>
                      {asking.count === 0
                        ? 'Nobody who joined through this link can be disabled.'
                        : `This signs out and disables ${asking.count === 1 ? '1 person' : `${String(asking.count)} people`} who joined through this link, and closes the link too.`}
                    </p>
                    <span className="actions">
                      {asking.count === 0 ? null : (
                        <button
                          type="button"
                          autoFocus
                          onClick={() => {
                            disable(invite);
                          }}
                        >
                          Disable {asking.count === 1 ? '1 person' : `${String(asking.count)} people`}
                        </button>
                      )}
                      <button
                        type="button"
                        autoFocus={asking.count === 0}
                        onClick={() => {
                          cancelAsk(invite.id);
                        }}
                      >
                        Cancel
                      </button>
                    </span>
                  </div>
                )}
                {seen === undefined ? null : (
                  <ul aria-label={`Joiners of link ${String(invite.id)}`}>
                    {seen.map((j) => (
                      <li key={j.id}>
                        {j.name}
                        {j.disabled ? ' (disabled)' : ''}
                      </li>
                    ))}
                  </ul>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
