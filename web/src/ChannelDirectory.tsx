import { useCallback, useContext, useEffect, useRef, useState } from 'react';
import * as api from './api';
import { PrivateMark } from './Icons';
import { ShowChat } from './phone';
import type { ChannelListingV1, ChannelV0 } from './schema/channel';

// The channel directory (issues #982, #991, design sections 2.6 and 5): every
// public channel and the person's own private ones, each marked joined,
// default, private and managed, with Join and Leave. It shows exactly what
// the server answered and no more: a row is a channel the server listed, no
// row carries a count of people (the server sends none to a member, and this
// reads none), and nothing here implies that a channel the server did not list
// exists. The words of every refusal are the page's own, by the server's code;
// the server's message is never shown.

const lastMember = 'You are the last member of this private channel. Add someone else before you leave.';
const gone = 'That channel is gone, or you can no longer see it.';

function joinLine(failure: api.ApiFailure, code: string | undefined): string {
  if (failure === 'network') return 'Cannot reach the server. Nothing was changed; try again.';
  switch (code) {
    case 'channel_not_joinable':
      return 'You cannot join this channel.';
    case api.channelNotFound:
      return gone;
    default:
      return 'The server could not add you to the channel. Try again.';
  }
}

function leaveLine(failure: api.ApiFailure, code: string | undefined): string {
  if (failure === 'network') return 'Cannot reach the server. Nothing was changed; try again.';
  switch (code) {
    case 'channel_last_member':
      return lastMember;
    case api.channelNotFound:
      return gone;
    default:
      return 'The server could not take you out of the channel. Try again.';
  }
}

type Line = { readonly text: string; readonly refused: boolean };

function Row({
  row,
  onJoin,
  onLeave,
  onOpen,
  onUnauthorized,
}: {
  readonly row: ChannelListingV1;
  readonly onJoin: (name: string) => Promise<api.InviteResult<ChannelV0>>;
  readonly onLeave: (row: ChannelListingV1) => Promise<api.InviteResult<null>>;
  readonly onOpen: (name: string) => void;
  readonly onUnauthorized: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [asking, setAsking] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  const sending = useRef(false);
  const live = useRef(true);
  const more = useRef<HTMLButtonElement>(null);
  const cancel = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useEffect(() => {
    if (asking) cancel.current?.focus();
  }, [asking]);

  const isPrivate = row.visibility === 'private';

  function settle<T>(result: api.InviteResult<T>, words: (failure: api.ApiFailure, code: string | undefined) => string, done: string): void {
    sending.current = false;
    if (!result.ok && result.failure === 'unauthorized') {
      onUnauthorized();
      return;
    }
    if (!live.current) return;
    setBusy(false);
    setLine(result.ok ? { text: done, refused: false } : { text: words(result.failure, result.code), refused: true });
  }

  function join() {
    if (sending.current) return;
    sending.current = true;
    setBusy(true);
    setLine(null);
    void onJoin(row.name).then((result) => {
      settle(result, joinLine, `Joined #${row.name}`);
    });
  }

  function leave() {
    if (sending.current) return;
    sending.current = true;
    setAsking(false);
    setBusy(true);
    setLine(null);
    void onLeave(row).then((result) => {
      settle(result, leaveLine, `Left #${row.name}`);
    });
  }

  return (
    <li className="directory-row">
      {isPrivate ? <PrivateMark /> : null}
      {row.joined ? (
        <button
          type="button"
          className="quiet channel-name"
          onClick={() => {
            onOpen(row.name);
          }}
        >
          {row.name}
        </button>
      ) : (
        <span className="channel-name">{row.name}</span>
      )}
      {row.default === true ? <span className="ag-badge">Default</span> : null}
      {row.joined ? <span className="ag-badge">Joined</span> : null}
      {row.joined && row.manages === true ? <span className="ag-badge">Manager</span> : null}
      {row.joined ? (
        <button
          ref={more}
          type="button"
          disabled={busy}
          aria-label={`Leave ${row.name}`}
          onClick={() => {
            // A private channel cannot be found again once left, so it asks first.
            if (isPrivate) setAsking(true);
            else leave();
          }}
        >
          Leave
        </button>
      ) : (
        <button type="button" disabled={busy} aria-label={`Join ${row.name}`} onClick={join}>
          Join
        </button>
      )}
      {asking ? (
        <div
          role="group"
          aria-label={`Leave ${row.name}?`}
          className="directory-ask"
          onKeyDown={(event) => {
            if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
            event.preventDefault();
            event.stopPropagation();
            setAsking(false);
            more.current?.focus();
          }}
        >
          <p>{`Leave #${row.name}? You will not find it again unless someone adds you back.`}</p>
          <div className="channel-settings-buttons">
            <button type="button" disabled={busy} onClick={leave}>
              Yes, leave
            </button>
            <button
              ref={cancel}
              type="button"
              onClick={() => {
                setAsking(false);
                more.current?.focus();
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      ) : null}
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </li>
  );
}

// "Browse channels": the directory, drawn under the channel list when asked
// for, and read from the server each time it is opened and whenever the
// person's own list changes while it is open. Joining a channel adds it to the
// list and opens it, with its full history (the server bounds none: ADR-013
// O12); leaving takes it out.
export function ChannelDirectory({
  channels,
  onJoined,
  onLeft,
  onOpen,
  onUnauthorized,
}: {
  // The person's own list, which the directory reads again when it changes.
  readonly channels: readonly ChannelV0[];
  readonly onJoined: (channel: ChannelV0) => void;
  readonly onLeft: (name: string) => void;
  readonly onOpen: (name: string) => void;
  readonly onUnauthorized: () => void;
}) {
  const showChat = useContext(ShowChat);
  const [open, setOpen] = useState(false);
  const [rows, setRows] = useState<readonly ChannelListingV1[] | null>(null);
  const [failed, setFailed] = useState(false);
  const seq = useRef(0);
  const live = useRef(true);
  const latest = useRef({ onJoined, onLeft, onUnauthorized });
  latest.current = { onJoined, onLeft, onUnauthorized };
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  const load = useCallback(() => {
    const mine = ++seq.current;
    setFailed(false);
    void api.channelDirectory().then((result) => {
      if (mine !== seq.current || !live.current) return;
      if (result.ok) {
        setRows(result.value);
        return;
      }
      if (result.failure === 'unauthorized') {
        latest.current.onUnauthorized();
        return;
      }
      setFailed(true);
    });
  }, []);

  useEffect(() => {
    if (open) {
      load();
      return;
    }
    // Closed, it holds nothing: what it was told is not kept to be shown again
    // later, when the person may no longer be in a private channel it listed.
    seq.current++;
    setRows(null);
    setFailed(false);
  }, [open, channels, load]);

  async function join(name: string): Promise<api.InviteResult<ChannelV0>> {
    const result = await api.joinChannel(name);
    if (!result.ok) {
      // The directory may be out of date: read it again.
      if (live.current && result.failure === 'invalid') load();
      return result;
    }
    latest.current.onJoined(result.value.channel);
    showChat();
    return { ok: true, value: result.value.channel };
  }

  async function leave(row: ChannelListingV1): Promise<api.InviteResult<null>> {
    const result = await api.leaveChannel(row.name);
    if (result.ok) latest.current.onLeft(row.name);
    else if (live.current && result.failure === 'invalid') load();
    return result;
  }

  return (
    <div className="channel-directory">
      <button
        type="button"
        aria-expanded={open}
        aria-controls="channel-directory-body"
        onClick={() => {
          setOpen(!open);
        }}
      >
        Browse channels
      </button>
      <section id="channel-directory-body" aria-label="Channel directory" hidden={!open}>
        {failed ? (
          <div className="problem">
            <p role="status">Cannot load the channels.</p>
            <button type="button" onClick={load}>
              Try again
            </button>
          </div>
        ) : rows === null ? (
          <p className="empty">Loading channels…</p>
        ) : rows.length === 0 ? (
          <p className="empty">No channels to show.</p>
        ) : (
          <ul aria-label="All channels">
            {rows.map((row) => (
              <Row
                key={row.id}
                row={row}
                onJoin={join}
                onLeave={leave}
                onOpen={(name) => {
                  onOpen(name);
                  showChat();
                }}
                onUnauthorized={onUnauthorized}
              />
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
