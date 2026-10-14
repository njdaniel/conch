import { useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { approvalPayloadSchema, approvalV1, type ApprovalV1 } from './schema/approval';
import type { Audience, MessageV2 } from './schema/message';
import type { ChannelChat, PendingSend } from './state/messages';
import { canLoadOlder, lastId, oldestId, ordered } from './state/messages';
import type { LiveStatus, Person } from './useLiveChannel';

// HH:MM in the reader's own time zone.
export function clock(when: string | number): string {
  const d = new Date(when);
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

// The author's name from the members list; #<id> for anyone not on it.
function nameOf(people: ReadonlyMap<number, Person>, id: number): string {
  return people.get(id)?.name ?? `#${String(id)}`;
}

// audienceLabel is the scope of a message that does not go to the whole
// channel, as the TUI shows it. A whisper names how many others hear it.
export function audienceLabel(audience: Audience, viewer: number, nets: ReadonlyMap<number, string>): string {
  if (audience.kind === 'net') return `→ net ${nets.get(audience.net_id) ?? `#${String(audience.net_id)}`}`;
  const others = audience.principal_ids.filter((id) => id !== viewer);
  // Only the viewer: count them rather than say nobody.
  return `→ whisper to ${String(others.length > 0 ? others.length : audience.principal_ids.length)}`;
}

// approvalOf reads the approval a message carries, or null if it has none
// (or one that is not the shape this client knows: then it is a plain message).
function approvalOf(message: MessageV2): ApprovalV1 | null {
  const payload = message.payload;
  if (payload?.schema !== approvalPayloadSchema || approvalV1(payload.data).length > 0) return null;
  return payload.data as ApprovalV1;
}

function ApprovalCard({ approval, people }: { readonly approval: ApprovalV1; readonly people: ReadonlyMap<number, Person> }) {
  return (
    <article className="approval-card" aria-label="Approval request">
      <strong>{approval.title}</strong>
      <div>Requested by {nameOf(people, approval.requester_id)}</div>
      <div>State: {approval.state}</div>
      <div className="hint">Decide in the terminal client: conch approvals</div>
    </article>
  );
}

function Line({
  time,
  author,
  kind,
  badge,
  tag,
  children,
}: {
  readonly time: string;
  readonly author: string;
  readonly kind: Person['kind'];
  readonly badge: string | null;
  readonly tag: string | null;
  readonly children: ReactNode;
}) {
  // The badge comes first, as in the TUI, so a name or a body cannot pass for one.
  return (
    <>
      {badge === null ? null : <span className="audience">{badge} </span>}
      <time>{time}</time> <span className="author">{author}</span>
      {kind === 'agent' ? <span className="kind"> [agent]</span> : null}: <span className="body">{children}</span>
      {tag === null ? null : <span className="state"> {tag}</span>}
    </>
  );
}

function Message({
  message,
  people,
  nets,
  viewer,
  sent,
}: {
  readonly message: MessageV2;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly sent: boolean;
}) {
  const approval = approvalOf(message);
  return (
    <li className="message">
      <Line
        time={clock(message.created_at)}
        author={nameOf(people, message.author_id)}
        kind={people.get(message.author_id)?.kind ?? null}
        badge={message.audience === undefined ? null : audienceLabel(message.audience, viewer, nets)}
        tag={sent ? 'Sent' : null}
      >
        {approval === null ? message.body : <ApprovalCard approval={approval} people={people} />}
      </Line>
    </li>
  );
}

function Pending({
  send,
  me,
  onRetry,
  onDismiss,
}: {
  readonly send: PendingSend;
  readonly me: Person;
  readonly onRetry: (localId: number) => void;
  readonly onDismiss: (localId: number) => void;
}) {
  const tag =
    send.status === 'sending' ? 'Sending'
    : send.status === 'failed' ? 'Failed'
    : 'Not confirmed: check the conversation before retrying';
  return (
    <li className="message pending">
      <Line time={clock(send.at)} author={me.name} kind={me.kind} badge={null} tag={tag}>
        {send.body}
      </Line>
      {send.status === 'sending' ? null : (
        <>
          {' '}
          <button
            type="button"
            onClick={() => {
              onRetry(send.localId);
            }}
          >
            Retry
          </button>{' '}
          <button
            type="button"
            onClick={() => {
              onDismiss(send.localId);
            }}
          >
            Dismiss
          </button>
        </>
      )}
    </li>
  );
}

// How close to the bottom still counts as being at it, in pixels.
const nearBottom = 24;

// The open channel: its messages in id order, and the box to write in.
export function Conversation({
  channel,
  chat,
  live,
  people,
  nets,
  viewer,
  draft,
  onDraft,
  onSend,
  onRetrySend,
  onDismissSend,
  onLoadOlder,
  onReload,
}: {
  readonly channel: string;
  readonly chat: ChannelChat;
  readonly live: LiveStatus;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly draft: string;
  readonly onDraft: (text: string) => void;
  readonly onSend: (body: string) => void;
  readonly onRetrySend: (localId: number) => void;
  readonly onDismissSend: (localId: number) => void;
  readonly onLoadOlder: () => void;
  readonly onReload: () => void;
}) {
  const log = useRef<HTMLDivElement>(null);
  // Whether the reader is at the bottom, so new messages follow them.
  const atBottom = useRef(true);
  // Where the reader was before older messages were drawn above them, and the
  // oldest id then: the anchor is spent when the oldest changes, not on the
  // renders in between (the request starting).
  const anchor = useRef<{ readonly height: number; readonly top: number; readonly oldest: number } | null>(null);
  const seen = useRef('');
  const [unseen, setUnseen] = useState(false);

  const shown = ordered(chat);
  const hasOlder = canLoadOlder(chat);
  const loadingOlder = chat.older === 'loading';
  const me = people.get(viewer) ?? { name: `#${String(viewer)}`, kind: null };
  const newest = `${String(lastId(chat))}/${String(chat.pending.length)}`;

  // Runs after every render, before it is painted: keep the reader where they
  // were when older messages came in, follow new ones when they were at the
  // bottom, and otherwise offer Jump to latest.
  useLayoutEffect(() => {
    const el = log.current;
    if (el === null) return;
    const held = anchor.current;
    if (held !== null && chat.older === 'error') anchor.current = null;
    else if (held !== null && oldestId(chat) !== held.oldest) {
      anchor.current = null;
      el.scrollTop = held.top + (el.scrollHeight - held.height);
      return;
    }
    if (newest === seen.current) return;
    seen.current = newest;
    if (atBottom.current) el.scrollTop = el.scrollHeight;
    else setUnseen(true);
  });

  function showOlder() {
    const el = log.current;
    if (el !== null) anchor.current = { height: el.scrollHeight, top: el.scrollTop, oldest: oldestId(chat) };
    onLoadOlder();
  }

  function jump() {
    const el = log.current;
    if (el !== null) el.scrollTop = el.scrollHeight;
    atBottom.current = true;
    setUnseen(false);
  }

  function submit() {
    if (draft.trim() === '' || live !== 'open') return;
    atBottom.current = true;
    onSend(draft);
  }

  function key(event: KeyboardEvent) {
    // Enter sends, Shift+Enter adds a line; Enter while composing (an input
    // method choosing a character) is not a send.
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    submit();
  }

  return (
    <>
      <h2>{channel}</h2>
      {live === 'reconnecting' ? <p className="stale">This conversation may be out of date</p> : null}
      {chat.history === 'error' ? (
        <div>
          <p>Cannot load the messages</p>
          <button type="button" onClick={onReload}>
            Try again
          </button>
        </div>
      ) : null}
      <div
        ref={log}
        className="log"
        role="log"
        aria-label="Messages"
        tabIndex={0}
        onScroll={(event) => {
          const el = event.currentTarget;
          atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight <= nearBottom;
          if (atBottom.current) setUnseen(false);
          if (el.scrollTop === 0 && hasOlder && !loadingOlder && chat.older !== 'error') showOlder();
        }}
      >
        {chat.trimmed || (!chat.exhausted && !hasOlder && shown.length > 0) ? <p className="hint">older history is not loaded</p> : null}
        {chat.older === 'error' ? <p role="alert">Cannot load older messages</p> : null}
        {hasOlder ? (
          <button type="button" disabled={loadingOlder} onClick={showOlder}>
            Load older messages
          </button>
        ) : null}
        <ol className="messages">
          {shown.map((message) => (
            <Message key={message.id} message={message} people={people} nets={nets} viewer={viewer} sent={chat.sentIds.has(message.id)} />
          ))}
          {chat.pending.map((send) => (
            <Pending key={`local-${String(send.localId)}`} send={send} me={me} onRetry={onRetrySend} onDismiss={onDismissSend} />
          ))}
        </ol>
      </div>
      {unseen ? (
        <button type="button" className="jump" onClick={jump}>
          Jump to latest
        </button>
      ) : null}
      {chat.dropped > 0 ? (
        <p className="hint">
          {String(chat.dropped)} unreadable {chat.dropped === 1 ? 'message was' : 'messages were'} ignored
        </p>
      ) : null}
      <form
        className="composer"
        onSubmit={(event) => {
          event.preventDefault();
          submit();
        }}
      >
        <label htmlFor="composer-text">Message #{channel}</label>
        <textarea
          id="composer-text"
          rows={2}
          value={draft}
          disabled={live !== 'open'}
          onChange={(event) => {
            onDraft(event.target.value);
          }}
          onKeyDown={key}
        />
        <button type="submit" disabled={live !== 'open' || draft.trim() === ''}>
          Send
        </button>
      </form>
    </>
  );
}
