import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { Composer } from './Composer';
import { Avatar } from './Icons';
import { approvalPayloadSchema, approvalV1, type ApprovalV1 } from './schema/approval';
import type { Audience, MessageV2 } from './schema/message';
import type { Target, TargetAction } from './state/drafts';
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

// How long after an author's message their next one is drawn under it,
// without their name and time again (both stay in the text read out).
const sameRun = 5 * 60 * 1000;

function Line({
  time,
  author,
  authorId,
  kind,
  badge,
  tag,
  tone,
  children,
}: {
  readonly time: string;
  readonly author: string;
  readonly authorId: number;
  readonly kind: Person['kind'];
  readonly badge: string | null;
  readonly tag: string | null;
  readonly tone?: 'failed' | 'unconfirmed';
  readonly children: ReactNode;
}) {
  // The badge comes first, as in the TUI, so a name or a body cannot pass for
  // one. On screen the header reads name, agent, time, audience (app.css).
  return (
    <>
      <Avatar id={authorId} name={author} />
      <div className="msg-head">
        {badge === null ? null : <span className="audience">{badge} </span>}
        <time>{time}</time> <span className="author">{author}</span>
        {kind === 'agent' ? (
          <span className="kind">
            <span className="vh"> [</span>agent<span className="vh">]</span>
          </span>
        ) : null}
        <span className="vh">: </span>
      </div>
      <span className="body">{children}</span>
      {tag === null ? null : <span className={tone === undefined ? 'state' : `state ${tone}`}> {tag}</span>}
    </>
  );
}

function Message({
  message,
  people,
  nets,
  viewer,
  sent,
  cont,
}: {
  readonly message: MessageV2;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly sent: boolean;
  // Drawn under the author's message before it, without the header again.
  readonly cont: boolean;
}) {
  const approval = approvalOf(message);
  return (
    <li className={cont ? 'message cont' : 'message'} data-message-id={message.id} tabIndex={-1}>
      <Line
        time={clock(message.created_at)}
        author={nameOf(people, message.author_id)}
        authorId={message.author_id}
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
  nets,
  viewer,
  onRetry,
  onDismiss,
}: {
  readonly send: PendingSend;
  readonly me: Person;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly onRetry: (localId: number) => void;
  readonly onDismiss: (localId: number) => void;
}) {
  const tag =
    send.status === 'sending' ? 'Sending'
    : send.status === 'failed' ? 'Failed'
    : 'Not confirmed: check the conversation before retrying';
  return (
    <li className="message pending">
      <Line
        time={clock(send.at)}
        author={me.name}
        authorId={viewer}
        kind={me.kind}
        badge={send.audience === undefined ? null : audienceLabel(send.audience, viewer, nets)}
        tag={tag}
        {...(send.status === 'sending' ? {} : { tone: send.status })}
      >
        {send.body}
      </Line>
      {send.status === 'sending' ? null : (
        <div className="msg-actions">
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
        </div>
      )}
    </li>
  );
}

function audienceKey(audience: Audience | undefined): string {
  if (audience === undefined) return '';
  return audience.kind === 'net' ? `net ${String(audience.net_id)}` : `to ${audience.principal_ids.join(',')}`;
}

// continues says whether a message is drawn as part of the one before it: the
// same author, the same audience, soon after, and neither an approval.
function continues(before: MessageV2 | undefined, message: MessageV2): boolean {
  if (before === undefined || before.author_id !== message.author_id) return false;
  if (audienceKey(before.audience) !== audienceKey(message.audience)) return false;
  if (approvalOf(before) !== null || approvalOf(message) !== null) return false;
  return Date.parse(message.created_at) - Date.parse(before.created_at) < sameRun;
}

// The open channel's heading, where focus goes on arriving in the lobby
// (issue #590). It is not the composer: on a phone that would open the
// keyboard over the page.
export const headingId = 'conversation-heading';

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
  target,
  monitored,
  onDraft,
  onTarget,
  onSend,
  onRetrySend,
  onDismissSend,
  onLoadOlder,
  onReload,
  onBottom,
}: {
  readonly channel: string;
  readonly chat: ChannelChat;
  readonly live: LiveStatus;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly draft: string;
  readonly target: Target;
  readonly monitored: ReadonlySet<number>;
  readonly onDraft: (text: string) => void;
  readonly onTarget: (action: TargetAction) => void;
  readonly onSend: (body: string, audience: Audience | undefined) => void;
  readonly onRetrySend: (localId: number) => void;
  readonly onDismissSend: (localId: number) => void;
  readonly onLoadOlder: () => void;
  readonly onReload: () => void;
  // Told whether the reader is at the newest message, whenever that changes.
  readonly onBottom: (atBottom: boolean) => void;
}) {
  const log = useRef<HTMLDivElement>(null);
  // Whether the reader is at the bottom, so new messages follow them.
  const atBottom = useRef(true);
  // Where the reader was before older messages were drawn above them, and the
  // oldest id then: the anchor is spent when the oldest changes, not on the
  // renders in between (the request starting).
  const anchor = useRef<{ readonly height: number; readonly top: number; readonly oldest: number } | null>(null);
  const seen = useRef('');
  // The log's height as the observer below last handled it; null before it
  // has, or without one.
  const sized = useRef<number | null>(null);
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

  // Anything that changes the log's own height (the whisper picker, the voice
  // strip, a share's stage) keeps a reader at the newest message there; one
  // who has scrolled up is left where they are. Browsers without a
  // ResizeObserver, and jsdom, keep the behaviour above alone.
  useEffect(() => {
    const el = log.current;
    if (el === null || typeof ResizeObserver !== 'function') return;
    sized.current = null;
    const watch = new ResizeObserver(() => {
      if (el.clientHeight === sized.current) return;
      sized.current = el.clientHeight;
      if (atBottom.current) el.scrollTop = el.scrollHeight;
    });
    watch.observe(el);
    return () => {
      watch.disconnect();
      sized.current = null;
    };
  }, []);

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
    onBottom(true);
  }

  return (
    <>
      <header className="conv-head">
        <span className="hash" aria-hidden="true">
          #
        </span>
        <h2 id={headingId} tabIndex={-1}>
          {channel}
        </h2>
      </header>
      {live === 'reconnecting' ? <p className="stale banner">This conversation may be out of date</p> : null}
      {chat.history === 'error' ? (
        <div className="banner problem">
          <p>Cannot load the messages</p>
          <button type="button" onClick={onReload}>
            Try again
          </button>
        </div>
      ) : null}
      <div className="log-wrap">
      <div
        ref={log}
        className="log"
        role="log"
        aria-label="Messages"
        tabIndex={0}
        onScroll={(event) => {
          const el = event.currentTarget;
          // A scroll at a height the observer has not handled yet is the
          // browser's, not the reader's: Chrome's scroll anchoring puts back
          // a position it once clamped when the log grew (the voice strip
          // getting shorter, say) as soon as the log gets shorter again, which
          // leaves a reader at the newest message more than nearBottom above
          // it. Whether they were at the bottom is kept for the observer.
          if (sized.current === null || el.clientHeight === sized.current) {
            atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight <= nearBottom;
            onBottom(atBottom.current);
            if (atBottom.current) setUnseen(false);
          }
          if (el.scrollTop === 0 && hasOlder && !loadingOlder && chat.older !== 'error') showOlder();
        }}
      >
        {chat.trimmed || (!chat.exhausted && !hasOlder && shown.length > 0) ? <p className="hint">older history is not loaded</p> : null}
        {chat.older === 'error' ? <p role="alert">Cannot load older messages</p> : null}
        {hasOlder ? (
          <button type="button" className="quiet older" disabled={loadingOlder} onClick={showOlder}>
            Load older messages
          </button>
        ) : null}
        <ol className="messages">
          {shown.map((message, i) => (
            <Message
              key={message.id}
              message={message}
              people={people}
              nets={nets}
              viewer={viewer}
              sent={chat.sentIds.has(message.id)}
              cont={continues(shown[i - 1], message)}
            />
          ))}
          {chat.pending.map((send) => (
            <Pending key={`local-${String(send.localId)}`} send={send} me={me} nets={nets} viewer={viewer} onRetry={onRetrySend} onDismiss={onDismissSend} />
          ))}
        </ol>
      </div>
      {unseen ? (
        <button type="button" className="jump primary" onClick={jump}>
          Jump to latest
        </button>
      ) : null}
      </div>
      {chat.dropped > 0 ? (
        <p className="hint">
          {String(chat.dropped)} unreadable {chat.dropped === 1 ? 'message was' : 'messages were'} ignored
        </p>
      ) : null}
      <Composer
        channel={channel}
        target={target}
        nets={nets}
        monitored={monitored}
        people={people}
        viewer={viewer}
        draft={draft}
        enabled={live === 'open'}
        onDraft={onDraft}
        onTarget={onTarget}
        onSend={onSend}
        atBottom={() => {
          atBottom.current = true;
          onBottom(true);
        }}
      />
    </>
  );
}
