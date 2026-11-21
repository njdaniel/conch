import { BlockedRow, isCollapsedFor, useBlocksContext } from './blocks';
import { platform } from './platform';
import { hashForMessage } from './route';
import { ModerationConfirm, useMayModerate, useMayRemoveOwn, useTombstones } from './Moderation';
import { Fragment, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import * as api from './api';
import { archivedLine } from './archived';
import { ReactionBar, ReactionScope, useReactionPicker } from './Reactions';
import { Composer } from './Composer';
import { guestName } from './guestName';
import { Avatar, PrivateMark } from './Icons';
import { IdentityMarks } from './Identity';
import { ProfileCard } from './ProfileCard';
import type { IdentityKey } from './identityMarks';
import { Linkified, previewText } from './Linkified';
import { displayBody, useKnownFileNames } from './spoilerFiles';
import { ImageAttachment } from './ImageAttachment';
import { loadStored, saveEdit, saveReplies } from './state/draftStore';
import { canEdit, EditedMarker, editEnds, editsLeft, editWindowMs, MessageEditor } from './MessageEdit';
import { ActionsMenu, useOpeners, type MenuItem } from './MessageMenu';
import { AdminApprovals } from './AdminApprovals';
import { approvalPayloadSchema, approvalV1, type ApprovalV1 } from './schema/approval';
import type { ChannelVisibility } from './schema/channel';
import { fileAttachmentPayloadSchema, fileAttachmentV1, type FileAttachmentV1 } from './schema/file';
import { MAX_MESSAGE_EDITS, type Audience, type MessageV2 } from './schema/message';
import type { Target, TargetAction } from './state/drafts';
import type { ChannelChat, PendingSend } from './state/messages';
import { canLoadOlder, editsKnown, isLater, lastId, oldestId, ordered } from './state/messages';
import type { LiveStatus, Person } from './useLiveChannel';
import { mentions } from './useNotifications';
import { useAuthorNames } from './useAuthorNames';
import { dateOf, dateTimeOf, sameLocalDay, timeOf } from './time';
import { fromServer } from './schema/validate';

// The time of day, in the reader's own locale and time zone (time.ts).
export function clock(when: string | number): string {
  return timeOf(when);
}

// newestSent: the id of the viewer's newest message among those shown, when it
// is one sent from this page and confirmed; null otherwise. "Sent" shows under
// that one only, not under every message (F26).
export function newestSent(messages: readonly MessageV2[], viewer: number, sentIds: ReadonlySet<number>): number | null {
  let newest: MessageV2 | null = null;
  for (const message of messages) {
    if (message.author_id === viewer && (newest === null || message.id > newest.id)) newest = message;
  }
  return newest !== null && sentIds.has(newest.id) ? newest.id : null;
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
  if (payload?.schema !== approvalPayloadSchema || fromServer(approvalV1)(payload.data).length > 0) return null;
  return payload.data as ApprovalV1;
}

// ApprovalCard draws an approval a message carries. Anyone in the channel can
// post such a message with any payload, so it is never a place to decide:
// the deciding card, AI administrators' included, is drawn only from the
// server's own approval objects (AdminApprovals, GET /v1/approvals), never
// from a message (security review of #1344).
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

const authorMarks: readonly IdentityKey[] = ['agent', 'manager', 'operator'];

function Line({
  at,
  author,
  authorId,
  person,
  viewer,
  badge,
  tag,
  tone,
  interactive = true,
  onCardDisappear,
  children,
}: {
  readonly at: string;
  readonly author: string;
  readonly authorId: number;
  readonly person: Person | undefined;
  readonly viewer: number;
  readonly badge: string | null;
  readonly tag: string | null;
  readonly tone?: 'failed' | 'unconfirmed';
  readonly interactive?: boolean;
  readonly onCardDisappear?: () => void;
  readonly children: ReactNode;
}) {
  // The badge comes first, as in the TUI, so a name or a body cannot pass for
  // one. On screen the header reads name, identity marks, time, audience.
  return (
    <>
      <Avatar id={authorId} name={author} />
      <div className="msg-head">
        {badge === null ? null : <span className="audience">{badge} </span>}
        <time dateTime={at} title={dateTimeOf(at)}>{clock(at)}</time> {interactive ? <ProfileCard id={authorId} person={person} viewer={viewer} name={author} className="author" onDisappear={onCardDisappear} /> : <span className="author">{author}</span>}
        <IdentityMarks kind={person?.kind} role={person?.role} manages={person?.manages} only={authorMarks} textForm />
        <span className="vh">: </span>
      </div>
      <div className="body">{children}</div>
      {tag === null ? null : <span className={tone === undefined ? 'state' : `state ${tone}`}> {tag}</span>}
    </>
  );
}

// authorOf names a message's author. A post made during a guest visit is
// named by the attribution stored with it (issue #713, design §4.4), the same
// for ever: after the visit ended, after the guest made an account, and
// whoever is in the members list now. Anyone else is named from the list.
export function authorOf(message: MessageV2, people: ReadonlyMap<number, Person>): string {
  return message.guest_author === undefined ? nameOf(people, message.author_id) : guestName(message.guest_author);
}

function attachmentsOf(message: MessageV2): FileAttachmentV1 | null {
  const payload = message.payload;
  if (payload?.schema !== fileAttachmentPayloadSchema || fromServer(fileAttachmentV1)(payload.data).length > 0) return null;
  return payload.data as FileAttachmentV1;
}

// messageName says whose message it is and when, for the names of its
// controls: "member-1's message, 21:38". Ids stay out of accessible names.
export function messageName(message: MessageV2, people: ReadonlyMap<number, Person>): string {
  return `${authorOf(message, people)}'s message, ${clock(message.created_at)}`;
}

// Message is one message of a log, a thread or search results. A message by
// someone the viewer blocks (issue #1832) is one collapsed row, "Blocked
// message", whose Show reveals it in this view only: the state lives in this
// row and is never stored, so another list, or this one read again, collapses
// it again. Approval and admin-action cards and tombstones never collapse
// (isBlockExempt).
export function Message(props: Parameters<typeof ShownMessage>[0]) {
  const { blocked } = useBlocksContext();
  const [revealed, setRevealed] = useState(false);
  if (!revealed && isCollapsedFor(blocked, props.message)) {
    return (
      <li className="message blocked" data-message-id={props.message.id} tabIndex={props.stop === true ? 0 : -1}>
        <BlockedRow onShow={() => { setRevealed(true); }} />
      </li>
    );
  }
  return <ShownMessage {...props} />;
}

function ShownMessage({
  message,
  people,
  nets,
  viewer,
  channel,
  sent,
  cont,
  onUnauthorized,
  onOpen,
  onReply,
  onOpenThread,
  replies,
  context,
  thread,
  actions,
  editor,
  notice,
  stop = false,
  reactionNavigation = false,
  highlighted = false,
}: {
  readonly message: MessageV2;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly channel: string;
  readonly sent: boolean;
  // Drawn under the author's message before it, without the header again.
  readonly cont: boolean;
  readonly onUnauthorized?: () => void;
  readonly onOpen?: (id: number) => void;
  readonly onReply?: (id: number, opener: HTMLElement) => void;
  readonly onOpenThread?: (id: number, opener: HTMLElement) => void;
  // How many replies the page holds to this message, when it is a root.
  readonly replies?: number;
  // What a reply answers, drawn above it in the log: the root as held, or
  // null when the page does not hold it. Undefined draws nothing.
  readonly context?: ReplyContext | null;
  readonly thread?: ReactNode;
  // Additional actions in the message's menu (issue #845): only what the
  // viewer may do here. Non-removed messages always offer copying (#1833).
  readonly actions?: readonly MenuItem[];
  // The inline editor, in place of the text, while the viewer edits it.
  readonly editor?: ReactNode;
  // A line about the message, under it: an edit that could not be made.
  readonly notice?: string | null;
  // This message is the log's one tab stop (issue #1040); the rest are -1.
  readonly stop?: boolean;
  readonly reactionNavigation?: boolean;
  readonly highlighted?: boolean;
}) {
  const row = useRef<HTMLLIElement>(null);
  useLayoutEffect(() => {
    const node = row.current;
    return () => { if (node?.contains(document.activeElement) === true) node.focus(); };
  }, [message.removed]);
  const mayModerate = useMayModerate();
  const mayRemoveOwn = useMayRemoveOwn() && message.author_id === viewer && !/^conch\.(approval|admin_action)(\.|$)/.test(message.payload?.schema ?? '');
  const mayRemove = mayModerate || mayRemoveOwn;
  const [removing, setRemoving] = useState(false);
  useEffect(() => { if (message.removed === true || !mayRemove) setRemoving(false); }, [message.removed, mayRemove]);
  const more = useRef<HTMLButtonElement>(null);
  const { addReaction, picker, hasReactionControls } = useReactionPicker(message.id, message.removed !== true, more);
  const who = messageName(message, people);
  const approval = message.removed === true ? null : approvalOf(message);
  const attachments = message.removed === true ? null : attachmentsOf(message);
  // An image sent with no text has its filename as the body, and the figure
  // shows that name already: the name shows once. Display only; the post is
  // unchanged (F26).
  const [fileNames, setFileNames] = useState<ReadonlyMap<number, string>>(new Map());
  const nameOnly = attachments !== null && attachments.file_ids.every((id) => fileNames.has(id)) && message.body === attachments.file_ids.map((id) => fileNames.get(id)).join(', ');
  const [copyNotice, setCopyNotice] = useState('');
  const [manualCopy, setManualCopy] = useState<string | null>(null);
  const copyField = useRef<HTMLTextAreaElement>(null);
  const copyRequest = useRef(0);
  useLayoutEffect(() => {
    if (message.removed === true) {
      setManualCopy(null);
      setCopyNotice('');
    }
    // A clipboard result cannot restore content after removal or unmount.
    return () => { copyRequest.current++; };
  }, [message.removed]);
  useEffect(() => {
    if (manualCopy !== null) { copyField.current?.focus(); copyField.current?.select(); }
  }, [manualCopy]);
  useEffect(() => {
    if (!highlighted) return;
    row.current?.scrollIntoView({ block: 'center', behavior: 'instant' });
    row.current?.focus({ preventScroll: true });
  }, [highlighted]);
  function copy(kind: 'link' | 'text') {
    if (message.removed === true) return;
    const request = ++copyRequest.current;
    const value = kind === 'link' ? `${platform.services.origin()}/app/${hashForMessage(channel, message.id)}` : message.body;
    setManualCopy(null);
    more.current?.focus();
    void Promise.resolve().then(() => platform.services.copyText(value)).then(() => {
      if (request !== copyRequest.current) return;
      setCopyNotice(kind === 'link' ? 'Link copied.' : 'Text copied.');
    }).catch(() => {
      if (request !== copyRequest.current) return;
      setCopyNotice('Clipboard unavailable. Copy the selected text.');
      setManualCopy(value);
    });
  }
  const items: readonly MenuItem[] = message.removed === true ? [] : [
    ...(editor === undefined ? actions ?? [] : []),
    ...(onReply === undefined || message.thread_root_id !== undefined ? [] : [{
      key: 'reply',
      label: 'Reply',
      onSelect: () => {
        const button = more.current;
        if (button === null) return;
        onReply(message.id, button);
        const composer = row.current?.querySelector<HTMLTextAreaElement>(':scope > .thread-view .reply-composer textarea:enabled');
        (composer ?? button).focus();
      },
    }]),
    ...(addReaction === undefined ? [] : [addReaction]),
    ...(mayRemove ? [{ key: 'remove-message', label: mayRemoveOwn ? 'Remove' : 'Remove message', onSelect: () => { setRemoving(true); } }] : []),
    { key: 'copy-link', label: 'Copy link', onSelect: () => { copy('link'); } },
    { key: 'copy-text', label: 'Copy text', onSelect: () => { copy('text'); } },
  ];
  const [menuOpen, setMenuOpen] = useState(false);
  const openers = useOpeners(items.length > 0, () => {
    setMenuOpen(true);
  }, { row: 'li[data-message-id], .thread-view' });
  const named = approval === null && editor === undefined && mentions(message.body, people.get(viewer)?.name ?? '');
  const classes = ['message', ...(highlighted ? ['message-linked'] : []), ...(cont ? ['cont'] : []), ...(named ? ['mentions-you'] : []), ...(items.length > 0 ? ['has-actions'] : []), ...(editor === undefined ? [] : ['editing'])];
  return (
    <li ref={row} className={classes.join(' ')} data-message-id={message.id} tabIndex={stop ? 0 : -1} aria-description={hasReactionControls && reactionNavigation ? "Use Right Arrow for reaction controls; Left and Right Arrow move between them, Escape returns to the message." : undefined} {...(context === undefined ? {} : { 'aria-describedby': `reply-context-${String(message.id)}` })} {...openers}>
      {named ? <span className="vh">Mentions you. </span> : null}
      {context === undefined ? null : (
        <p className="reply-context" id={`reply-context-${String(message.id)}`}>
          {context === null ? 'Reply in a thread' : `Replying to ${context.author}: ${context.excerpt}`}
        </p>
      )}
      <Line
        at={message.created_at}
        author={authorOf(message, people)}
        authorId={message.author_id}
        person={people.get(message.author_id)}
        viewer={viewer}
        interactive={message.removed !== true && !cont}
        onCardDisappear={() => { row.current?.focus(); }}
        badge={message.audience === undefined ? null : audienceLabel(message.audience, viewer, nets)}
        tag={sent ? 'Sent' : null}
      >
        {(message.removed === true ? null : editor) ?? (approval === null ? (
          <>
            {nameOnly ? null : <Linkified text={displayBody(message)} />}
            <EditedMarker message={message} channel={channel} viewer={viewer} onUnauthorized={onUnauthorized} />
          </>
        ) : <ApprovalCard approval={approval} people={people} />)}
        {attachments === null ? null : (
          <span className="attachments">
            {attachments.file_ids.map((fileId) => (
              <ImageAttachment key={fileId} fileId={fileId} channel={channel} viewer={viewer} onUnauthorized={onUnauthorized} onFilename={(name) => { setFileNames((known) => (known.get(fileId) === name ? known : new Map(known).set(fileId, name))); }} />
            ))}
          </span>
        )}
      </Line>
      {message.removed === true || copyNotice === '' ? null : <p role="status">{copyNotice}</p>}
      {message.removed === true || manualCopy === null ? null : <label>Copy manually<textarea ref={copyField} readOnly value={manualCopy} onFocus={(event) => { event.currentTarget.select(); }} /></label>}
      {notice === undefined || notice === null ? null : <p role="status" className="edit-notice">{notice}</p>}
      {items.length === 0 ? null : (
        <ActionsMenu label={`More actions for ${who}`} items={items} open={menuOpen} onOpenChange={setMenuOpen} button={more} />
      )}
      {onOpen === undefined ? null : <button type="button" className="search-open" aria-label={`Open context for ${who}`} onClick={() => {
        onOpen(message.id);
      }}>Open context</button>}
      <ReactionBar enabled={message.removed !== true} logNavigation={reactionNavigation} id={message.id} channel={channel} label={who} people={people} onUnauthorized={onUnauthorized} picker={picker}>
        {onOpenThread === undefined || message.thread_root_id === undefined ? null : <button type="button" className="message-action" aria-label={`Open thread for ${who}`} onClick={(event) => {
          onOpenThread(message.thread_root_id as number, event.currentTarget);
        }}>Open thread</button>}
        {replies === undefined || replies === 0 || onOpenThread === undefined ? null : <button type="button" className="message-action reply-count" onClick={(event) => {
          onOpenThread(message.id, event.currentTarget);
        }}>{replies === 1 ? '1 reply' : `${String(replies)} replies`}</button>}
      </ReactionBar>
      {removing && mayRemove ? <ModerationConfirm op={mayRemoveOwn ? "removeOwnMessage" : "removeMessage"} id={message.id} name={who} channel={channel} onUnauthorized={onUnauthorized} onClose={() => { setRemoving(false); more.current?.focus(); }} /> : null}
      {thread}
    </li>
  );
}

// ReplyContext is what a reply in the log quotes of its root.
export interface ReplyContext {
  readonly author: string;
  readonly excerpt: string;
}

const excerptLength = 80;

// excerptOf is the first line of a body, cut to about excerptLength characters.
// It is drawn as text: markup in it shows as the characters it is.
export function excerptOf(body: string): string {
  const line = (previewText(body).split('\n').find((part) => part.trim() !== '') ?? '').trim();
  const chars = Array.from(line);
  return chars.length <= excerptLength ? line : `${chars.slice(0, excerptLength).join('').trimEnd()}…`;
}

// replyContext quotes the root from the copy this page holds, which the
// server sent to this viewer only if they may read it; no copy, no quote. An
// image-only root is quoted by its shown names, the stored ones matched when
// they are known (see attachmentNames).
function replyContext(root: MessageV2 | undefined, people: ReadonlyMap<number, Person>): ReplyContext | null {
  return root === undefined ? null : { author: authorOf(root, people), excerpt: excerptOf(displayBody(root)) };
}

function Pending({
  send,
  after,
  me,
  nets,
  viewer,
  onRetry,
  onDismiss,
}: {
  readonly send: PendingSend;
  readonly after: string | number | undefined;
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
    <>
      {sameLocalDay(after, send.at) ? null : <DayDivider at={send.at} />}
      <li className="message pending">
        <Line
          at={new Date(send.at).toISOString()}
          author={me.name}
          authorId={viewer}
          person={me}
          viewer={viewer}
          badge={send.audience === undefined ? null : audienceLabel(send.audience, viewer, nets)}
          tag={tag}
          {...(send.status === 'sending' ? {} : { tone: send.status })}
        >
          <Linkified text={send.body} />
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
    </>
  );
}

function DayDivider({ at }: { readonly at: string | number }) {
  const date = dateOf(at);
  return (
    <li role="presentation">
      <div className="message-day" role="separator" aria-label={date}>{date}</div>
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
  // A guest who made an account is the same principal: its posts as a guest
  // and as a member are drawn apart.
  if (before.guest_author?.handle !== message.guest_author?.handle) return false;
  if (audienceKey(before.audience) !== audienceKey(message.audience)) return false;
  if (approvalOf(before) !== null || approvalOf(message) !== null) return false;
  return Date.parse(message.created_at) - Date.parse(before.created_at) < sameRun;
}

function ThreadView({ highlightedId, rootId, root, chat, channel, live, archived, people: known, nets, viewer, draft, onDraft, onClose, onReplySend, onRetrySend, onDismissSend, onUnauthorized, onEditLast, editorFor, notices }: {
  readonly highlightedId: number | null;
  readonly notices: ReadonlyMap<number, string>;
  readonly onEditLast: (messages: readonly MessageV2[], composer: HTMLTextAreaElement) => boolean;
  readonly editorFor: (message: MessageV2, onSaved: (saved: MessageV2) => void) => ReactNode;
  readonly rootId: number;
  readonly root: MessageV2 | undefined;
  readonly chat: ChannelChat;
  readonly channel: string;
  readonly live: LiveStatus;
  // The channel is archived (issue #844): the thread reads, and takes no reply.
  readonly archived: boolean;
  readonly people: ReadonlyMap<number, Person>;
  readonly nets: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly draft: string;
  readonly onDraft: (text: string) => void;
  readonly onClose: () => void;
  readonly onReplySend: (body: string, root: number, audience: Audience | undefined) => void;
  readonly onRetrySend: (localId: number) => void;
  readonly onDismissSend: (localId: number) => void;
  readonly onUnauthorized?: () => void;
}) {
  const { removed, safe } = useTombstones();
  const composing = useRef(false);
  const previousLive = useRef(live);
  useEffect(() => {
    const reconnect = previousLive.current !== 'open' && live === 'open';
    previousLive.current = live;
    if (live !== 'open') setFetched((old) => old.length === 0 ? old : []);
    if (reconnect) void load(0, false);
  });
  const [fetched, setFetched] = useState<readonly MessageV2[]>([]);
  const [nextAfter, setNextAfter] = useState<number | undefined>();
  const [status, setStatus] = useState<'loading' | 'ready' | 'error'>('loading');
  const request = useRef(0);
  const closeNow = useRef(onClose);
  const unauthorizedNow = useRef(onUnauthorized);
  closeNow.current = onClose;
  unauthorizedNow.current = onUnauthorized;
  const load = useCallback(async (after: number, append: boolean) => {
    const current = ++request.current;
    setStatus('loading');
    const result = await api.loadThread(channel, rootId, after);
    if (current !== request.current) return;
    if (!result.ok) {
      if (result.failure === 'unauthorized' || result.code === 'thread_not_found') {
        // Closing may await confirmation of unsaved text in this thread.
        setStatus('ready');
        closeNow.current();
        if (result.failure === 'unauthorized') unauthorizedNow.current?.();
      } else setStatus('error');
      return;
    }
    setFetched((previous) => append ? [...previous, ...result.value.messages.map(safe)] : result.value.messages.map(safe));
    setNextAfter(result.value.next_after);
    setStatus('ready');
  }, [channel, rootId, safe]);

  useEffect(() => {
    setFetched([]);
    setNextAfter(undefined);
    void load(0, false);
    return () => {
      request.current++;
    };
  }, [load]);

  useEffect(() => { setFetched((old) => old.map((m) => removed.get(m.id) ?? m)); }, [removed]);
  const byId = new Map<number, MessageV2>();
  if (root !== undefined) byId.set(root.id, root);
  for (const message of [...fetched, ...ordered(chat)]) {
    if (message.id !== rootId && message.thread_root_id !== rootId) continue;
    // The thread's own read and the channel's copy: the later edit wins.
    const held = byId.get(message.id);
    if (held === undefined || isLater(message, held)) byId.set(message.id, safe(message));
  }
  const messages = [...byId.values()].sort((a, b) => a.id - b.id);
  const sentMark = newestSent(messages, viewer, chat.sentIds);
  const threadAuthors = useMemo(() => [...new Set(messages.filter((m) => m.guest_author === undefined).map((m) => m.author_id))], [messages]);
  const people = useAuthorNames(channel, threadAuthors, known, onUnauthorized);
  const rootMessage = byId.get(rootId);
  const me = people.get(viewer) ?? { name: `#${String(viewer)}`, kind: null };
  const pending = chat.pending.filter((send) => send.threadRootId === rootId);
  return (
    <ReactionScope key={`${String(viewer)}/${channel}/${String(rootId)}`} channel={channel} channelId={messages[0]?.channel_id ?? 0} ids={messages.filter((m) => m.removed !== true).map((m) => m.id)} onUnauthorized={onUnauthorized}>
    <section className="thread-view" aria-label={rootMessage === undefined ? 'Thread' : `Thread around ${messageName(rootMessage, people)}`}>
      <div className="thread-head">
        <h3>Thread</h3>
        <button type="button" onClick={onClose}>Close thread</button>
      </div>
      {status === 'loading' && fetched.length === 0 ? <p role="status">Loading thread…</p> : null}
      {status === 'error' ? (
        <div role="alert" className="banner problem">
          <p>Cannot load this thread.</p>
          <button type="button" onClick={() => {
            void load(nextAfter ?? 0, nextAfter !== undefined);
          }}>Try again</button>
        </div>
      ) : null}
      <ol className="messages" aria-label="Root and replies">
        {messages.map((message, i) => (
          <Fragment key={message.id}>
            {sameLocalDay(messages[i - 1]?.created_at, message.created_at) ? null : <DayDivider at={message.created_at} />}
            <Message highlighted={message.id === highlightedId} message={message} people={people} nets={nets} viewer={viewer} channel={channel} sent={message.id === sentMark} cont={false} onUnauthorized={onUnauthorized} notice={notices.get(message.id) ?? null} editor={editorFor(message, (saved) => { setFetched((previous) => previous.map((held) => held.id === saved.id ? saved : held)); })} />
          </Fragment>
        ))}
        {pending.map((send, i) => <Pending key={`local-${String(send.localId)}`} send={send} after={pending[i - 1]?.at ?? messages.at(-1)?.created_at} me={me} nets={nets} viewer={viewer} onRetry={onRetrySend} onDismiss={onDismissSend} />)}
      </ol>
      {nextAfter === undefined ? null : (
        <button type="button" disabled={status === 'loading'} onClick={() => {
          void load(nextAfter, true);
        }}>Load more replies</button>
      )}
      {rootMessage === undefined || rootMessage.removed === true ? null : archived ? (
        <p role="status" className="hint archived-note">{archivedLine}</p>
      ) : (
        <form className="reply-composer" onSubmit={(event) => {
          event.preventDefault();
          if (live !== 'open' || draft.trim() === '') return;
          onReplySend(draft, rootId, rootMessage.audience);
          onDraft('');
        }}>
          <p className="hint">Replying to {rootMessage.audience === undefined ? `#${channel}` : audienceLabel(rootMessage.audience, viewer, nets)}; the audience cannot be changed.</p>
          <label htmlFor={`reply-${String(rootId)}`}>Reply to {authorOf(rootMessage, people)}</label>
          <div className="reply-box">
            <textarea id={`reply-${String(rootId)}`} autoFocus={highlightedId === null} rows={2} value={draft} disabled={live !== 'open'} onChange={(event) => {
              onDraft(event.target.value);
            }} onCompositionStart={() => { composing.current = true; }} onCompositionEnd={() => { composing.current = false; }} onKeyDown={(event) => {
              if (event.key === 'ArrowUp' && draft === '' && !composing.current && !event.nativeEvent.isComposing && !event.shiftKey && !event.ctrlKey && !event.altKey && !event.metaKey && onEditLast(messages, event.currentTarget)) {
                event.preventDefault();
                return;
              }
              if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
              event.preventDefault();
              if (live !== 'open' || draft.trim() === '') return;
              onReplySend(draft, rootId, rootMessage.audience);
              onDraft('');
            }} />
            <button type="submit" disabled={live !== 'open' || draft.trim() === ''}>Send reply</button>
          </div>
          {live === 'reconnecting' ? <p className="hint">Reconnecting; replies cannot be sent.</p> : null}
        </form>
      )}
    </section>
    </ReactionScope>
  );
}

// The open channel's heading, where focus goes on arriving in the lobby
// (issue #590). It is not the composer: on a phone that would open the
// keyboard over the page.
export const headingId = 'conversation-heading';

// The controls inside a message that Tab would stop at.
const tabbable = 'button, a[href], textarea, input, select';

// syncStops makes the log one tab stop (issue #1040). The message that is the
// stop keeps its controls reachable by Tab; every other message's controls are
// taken out of the order, and given back (only those taken) when it becomes the
// stop. Controls React itself keeps at -1, such as menu items, are not touched.
function syncStops(list: HTMLElement, stop: number | null) {
  for (const item of Array.from(list.children)) {
    if (!(item instanceof HTMLElement) || item.dataset.messageId === undefined) continue;
    const here = item.dataset.messageId === String(stop);
    if (here) {
      for (const el of Array.from(item.querySelectorAll<HTMLElement>('[data-roved]'))) {
        el.removeAttribute('tabindex');
        el.removeAttribute('data-roved');
      }
    } else {
      for (const el of Array.from(item.querySelectorAll<HTMLElement>(tabbable))) {
        if (el.tabIndex < 0) continue;
        el.setAttribute('tabindex', '-1');
        el.setAttribute('data-roved', '');
      }
    }
  }
}

// How close to the bottom still counts as being at it, in pixels.
const nearBottom = 24;

// The message at the top of the log's view and its offset below the log's
// top. Only the log's own rows count: an expanded thread repeats ids inside
// its root's row. Rows are in document order, so a binary search finds it.
function topOfView(log: HTMLElement): { readonly id: string; readonly offset: number } | null {
  const rows = log.querySelectorAll<HTMLElement>(':scope > .messages > li[data-message-id]');
  const top = log.getBoundingClientRect().top;
  let lo = 0;
  let hi = rows.length - 1;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if ((rows[mid]?.getBoundingClientRect().bottom ?? 0) > top) hi = mid;
    else lo = mid + 1;
  }
  const row = rows[lo];
  if (row === undefined) return null;
  return { id: row.dataset.messageId ?? '', offset: row.getBoundingClientRect().top - top };
}

// The open channel: its messages in id order, and the box to write in.
export function Conversation({
  highlightedId = null,
  channelId,
  channel,
  topic,
  chat,
  live,
  people: members,
  nets,
  viewer,
  draft,
  target,
  monitored,
  onDraft,
  onTarget,
  onSend,
  onReplySend,
  onRetrySend,
  onDismissSend,
  onLoadOlder,
  onReload,
  onBottom,
  onUnauthorized,
  onSearch,
  onEdited,
  onEditFinal,
  historyBounded = false,
  visibility,
  archived = false,
  density = '',
  openingMark = null,
}: {
  readonly highlightedId?: number | null;
  // The read mark captured when this channel opened, held for this visit.
  readonly openingMark?: number | null;
  // The channel is archived (issue #844): it opens read-only. History, search
  // and threads read as before; the composer is replaced by one line.
  readonly archived?: boolean;
  // The chat density in force (issue #1497); a change of it keeps the reader's place.
  readonly density?: string;
  // The viewer's own membership here has a history bound (history_after), so
  // what was said before they joined is not shown (issue #1034). False when
  // it has none or is not known yet: the page never claims hiding it cannot see.
  readonly historyBounded?: boolean;
  // The channel's visibility (issue #991): a private one has a lock before its
  // name. Drawn from this alone, never from how many are in the channel.
  readonly visibility?: ChannelVisibility;
  // The channel's id, for keeping an open edit across a reload (issue #1039);
  // without it nothing is kept.
  readonly channelId?: number;
  readonly channel: string;
  readonly topic?: string;
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
  readonly onReplySend: (body: string, root: number, audience: Audience | undefined) => void;
  readonly onRetrySend: (localId: number) => void;
  readonly onDismissSend: (localId: number) => void;
  readonly onLoadOlder: () => void;
  readonly onReload: () => void;
  // Told whether the reader is at the newest message, whenever that changes.
  readonly onBottom: (atBottom: boolean) => void;
  readonly onUnauthorized?: () => void;
  readonly onSearch?: () => void;
  // An edit of the viewer's own message saved (issue #845): the server's copy,
  // for the page to hold. Without it nothing here offers Edit.
  readonly onEdited?: (message: MessageV2) => void;
  // A refusal ended a message's edits for good (issue #1031).
  readonly onEditFinal?: (id: number) => void;
}) {
  // Whoever wrote here and is not in the member list is named by the
  // channel's authors route (design §13.5); the composer's mention candidates
  // stay the member list alone.
  const authorIds = useMemo(() => [...new Set([...chat.byId.values()].filter((m) => m.guest_author === undefined).map((m) => m.author_id))], [chat.byId]);
  const people = useAuthorNames(channel, authorIds, members, onUnauthorized);
  // A file name recorded later (an attachment's metadata loads) redraws the
  // reply excerpts that quote it.
  useKnownFileNames();
  const log = useRef<HTMLDivElement>(null);
  // Whether the reader is at the bottom, so new messages follow them.
  const atBottom = useRef(true);
  // Where the reader was before older messages were drawn above them, and the
  // oldest id then: the anchor is spent when the oldest changes, not on the
  // renders in between (the request starting).
  const anchor = useRef<{ readonly height: number; readonly top: number; readonly oldest: number } | null>(null);
  const seen = useRef('');
  // The message at the top of the view and how far below the log's top it
  // sits, kept as the reader scrolls, so a change of chat density (which
  // changes every message's height) can put that message back.
  const view = useRef<{ readonly id: string; readonly offset: number } | null>(null);
  const shownDensity = useRef(density);
  // The log's height as the observer below last handled it; null before it
  // has, or without one.
  const sized = useRef<number | null>(null);
  const [unseen, setUnseen] = useState(false);
  const [openThread, setOpenThread] = useState<number | null>(null);
  useEffect(() => {
    const message = highlightedId === null ? undefined : chat.byId.get(highlightedId);
    if (message?.thread_root_id !== undefined) setOpenThread(message.thread_root_id);
  }, [highlightedId, chat.byId]);
  const opener = useRef<HTMLElement | null>(null);
  const threadReturn = useRef<{ readonly root: number; readonly anchor: number | null; readonly opener: HTMLElement | null } | null>(null);
  const openFrom = useCallback((id: number, from: HTMLElement) => {
    opener.current = from;
    setOpenThread(id);
  }, []);
  // The replies half-typed in this channel come back from the tab's kept
  // drafts (issue #1080): text by thread root, nothing else.
  const [replyDrafts, setReplyDrafts] = useState<ReadonlyMap<number, string>>(() => {
    const kept = new Map<number, string>();
    if (channelId === undefined) return kept;
    for (const [root, text] of Object.entries(loadStored(viewer).replies[String(channelId)] ?? {})) kept.set(Number(root), text);
    return kept;
  });
  const [repliesSettled, setRepliesSettled] = useState(replyDrafts.size === 0);
  const openNow = useRef(openThread);
  const chatNow = useRef(chat);
  openNow.current = openThread;
  chatNow.current = chat;
  const finishCloseThread = useCallback(() => {
    const open = openNow.current;
    if (open !== null) {
      const loaded = ordered(chatNow.current);
      const anchor = loaded.some((message) => message.id === open) ? open : loaded.find((message) => message.thread_root_id === open)?.id ?? null;
      threadReturn.current = { root: open, anchor, opener: opener.current };
      setReplyDrafts((previous) => {
        const next = new Map(previous);
        next.delete(open);
        return next;
      });
    }
    setOpenThread(null);
  }, []);
  useEffect(() => {
    if (openThread !== null || threadReturn.current === null) return;
    const back = threadReturn.current;
    threadReturn.current = null;
    const list = log.current;
    const target = back.opener?.isConnected === true ? back.opener
      : list?.querySelector<HTMLElement>(`[data-message-id="${String(back.root)}"] > .message-menu > .more-actions`)
        ?? list?.querySelector<HTMLElement>(`[data-message-id="${String(back.root)}"]`)
        ?? list?.querySelector<HTMLElement>(`[data-message-id="${String(back.anchor)}"]`);
    target?.focus();
  }, [openThread]);

  // Editing (issue #845, docs/design/message-editing.md §13.2): one editor at
  // a time, its text kept here; a line under a message an edit could not
  // reach; and the time the window is counted from, moved on when the next
  // of the viewer's messages leaves it, so Edit goes when it can no longer be
  // used.
  type EditOrigin = { readonly composer: HTMLTextAreaElement; readonly thread: number | null };
  type EditRequest = { readonly id: number; readonly origin: EditOrigin | undefined };
  const [edit, setEdit] = useState<{ readonly id: number; readonly text: string; readonly next: EditRequest | 'close-thread' | null } | null>(null);
  const [editFocus, setEditFocus] = useState(0);
  const [notices, setNotices] = useState<ReadonlyMap<number, string>>(new Map());
  const [now, setNow] = useState(() => Date.now());
  const focusBack = useRef<number | null>(null);
  const editOrigins = useRef(new Map<number, EditOrigin>());
  const editThread = edit === null ? null : (editOrigins.current.get(edit.id)?.thread ?? null);
  // Thread history can include messages outside the channel's loaded page.
  const threadEditMessages = useRef(new Map<number, MessageV2>());
  const viewerKind = people.get(viewer)?.kind ?? null;
  useEffect(() => {
    let next = Infinity;
    for (const message of chat.byId.values()) {
      if (message.author_id !== viewer) continue;
      const ends = editEnds(message);
      if (ends >= now && ends < next) next = ends;
    }
    if (next === Infinity) return;
    const handle = window.setTimeout(() => {
      setNow(Date.now());
    }, Math.min(Math.max(0, next - Date.now()) + 1, 2 ** 31 - 1));
    return () => {
      window.clearTimeout(handle);
    };
  }, [chat.byId, viewer, now]);
  // After the editor closes, focus goes back to the message's ⋯ button, or to
  // the message when it no longer offers one (its window closed meanwhile).
  // An Up-arrow edit returns to the composer that opened it instead.
  useEffect(() => {
    const id = focusBack.current;
    if (id === null || edit !== null) return;
    focusBack.current = null;
    const composer = editOrigins.current.get(id)?.composer;
    editOrigins.current.delete(id);
    if (composer?.isConnected === true) {
      composer.focus();
      return;
    }
    const item = log.current?.querySelector<HTMLElement>(`[data-message-id="${String(id)}"]`);
    (item?.querySelector<HTMLElement>('.more-actions') ?? item)?.focus();
  }, [edit]);
  // Whether the page still offers Edit on a message beyond who and when
  // (issue #1031): not at ten known edits, nor after a final refusal.
  const editable = (message: MessageV2, at = now) => canEdit(message, viewer, viewerKind, at) && editsLeft(editsKnown(chat, message.id), chat.final.has(message.id));
  const note = useCallback((id: number, line: string | null) => {
    setNotices((previous) => {
      if (line === null && !previous.has(id)) return previous;
      const next = new Map(previous);
      if (line === null) next.delete(id);
      else next.set(id, line);
      return next;
    });
  }, []);
  const heldForEdit = useCallback((id: number) => {
    const held = chatNow.current.byId.get(id);
    const fetched = threadEditMessages.current.get(id);
    return fetched !== undefined && (held === undefined || isLater(fetched, held)) ? fetched : held;
  }, []);
  const acceptEdit = (request: EditRequest) => {
    const message = heldForEdit(request.id);
    if (edit !== null) editOrigins.current.delete(edit.id);
    if (message === undefined) {
      setEdit(null);
      return;
    }
    if (request.origin === undefined) editOrigins.current.delete(request.id);
    else editOrigins.current.set(request.id, request.origin);
    note(request.id, null);
    setEdit({ id: request.id, text: message.body, next: null });
  };
  const startEdit = (id: number, origin?: EditOrigin) => {
    if (edit?.id === id) {
      // Re-entering the same edit preserves its text and surface.
      setEditFocus((previous) => previous + 1);
      return;
    }
    if (heldForEdit(id) === undefined) return;
    const request = { id, origin };
    const was = edit === null ? undefined : heldForEdit(edit.id);
    if (edit !== null && was !== undefined && edit.text !== was.body) {
      setEdit({ ...edit, next: request });
      return;
    }
    acceptEdit(request);
  };
  const closeThread = () => {
    if (edit !== null && editThread === openNow.current) {
      const was = heldForEdit(edit.id);
      if (was !== undefined && edit.text !== was.body) {
        setEdit({ ...edit, next: 'close-thread' });
        return;
      }
      editOrigins.current.delete(edit.id);
      setEdit(null);
    }
    finishCloseThread();
  };
  const closeEdit = useCallback((id: number) => {
    focusBack.current = id;
    setEdit((current) => (current?.id === id ? null : current));
  }, []);

  // Choose the newest own message first, then check eligibility: an expired,
  // removed or exhausted newest message must never expose an older one.
  const editLast = (messages: readonly MessageV2[], composer: HTMLTextAreaElement, thread: number | null = null): boolean => {
    if (onEdited === undefined || archived || live !== 'open') return false;
    let newest: MessageV2 | undefined;
    for (const message of messages) {
      if (message.author_id === viewer && (newest === undefined || message.id > newest.id)) newest = message;
    }
    if (newest === undefined || !editable(newest, Date.now())) return false;
    if (thread !== null) threadEditMessages.current.set(newest.id, newest);
    startEdit(newest.id, { composer, thread });
    return true;
  };
  const editorFor = (message: MessageV2, onSaved?: (saved: MessageV2) => void): ReactNode => {
    if (edit?.id !== message.id || onEdited === undefined) return undefined;
    return (
      <MessageEditor
        message={message}
        focusRequest={editFocus}
        channel={channel}
        label={`Edit ${messageName(message, people)}`}
        text={edit.text}
        onText={(text) => {
          setEdit((current) => (current?.id === message.id ? { ...current, text } : current));
        }}
        onCancel={() => {
          closeEdit(message.id);
        }}
        onSaved={(saved) => {
          if (threadEditMessages.current.has(saved.id)) threadEditMessages.current.set(saved.id, saved);
          onSaved?.(saved);
          onEdited(saved);
          closeEdit(message.id);
        }}
        onGone={(line) => {
          note(message.id, line);
          closeEdit(message.id);
        }}
        onUnauthorized={onUnauthorized}
        atLimit={editsKnown(chat, message.id) >= MAX_MESSAGE_EDITS}
        onFinal={() => { onEditFinal?.(message.id); }}
        discard={edit.next === null ? undefined : {
          onDiscard: () => {
            const next = edit.next;
            if (next === 'close-thread') {
              editOrigins.current.delete(edit.id);
              setEdit(null);
              finishCloseThread();
            } else if (next !== null) acceptEdit(next);
          },
          onKeep: () => {
            setEdit((current) => (current === null ? null : { ...current, next: null }));
          },
        }}
      />
    );
  };

  // An edit left open by a reload (issue #1039) reopens with its text once
  // the history and the members are here, if the message is still the
  // viewer's to edit; otherwise it is dropped. Nothing is written meanwhile.
  const savedEdit = useRef(channelId === undefined ? null : (loadStored(viewer).edits[String(channelId)] ?? null));
  const [editSettled, setEditSettled] = useState(savedEdit.current === null);
  useEffect(() => {
    const saved = savedEdit.current;
    if (saved === null || chat.history !== 'ready' || viewerKind === null) return;
    const message = chat.byId.get(saved.message);
    if (message === undefined) {
      // Not held is not gone: the message may be below the page read. While
      // the oldest held message is still inside the edit window, and older
      // ones exist, ask for them and look again; the saved text is kept.
      let oldest: MessageV2 | undefined;
      for (const held of chat.byId.values()) if (oldest === undefined || held.id < oldest.id) oldest = held;
      if (oldest !== undefined && saved.message < oldest.id && !chat.exhausted && Date.now() - Date.parse(oldest.created_at) <= editWindowMs) {
        if (chat.older === 'idle') onLoadOlder();
        return;
      }
    }
    savedEdit.current = null;
    if (message !== undefined && onEdited !== undefined && canEdit(message, viewer, viewerKind, Date.now())) {
      setEdit({ id: message.id, text: saved.text, next: null });
    }
    setEditSettled(true);
  }, [chat.history, chat.byId, chat.older, chat.exhausted, viewerKind, viewer, onEdited, onLoadOlder]);
  useEffect(() => {
    if (edit !== null && chat.byId.get(edit.id)?.removed === true) {
      focusBack.current = edit.id;
      setEdit(null);
    }
  }, [chat.byId, edit]);
  const editMessage = edit?.id ?? null;
  const editText = edit?.text ?? '';
  useEffect(() => {
    if (!editSettled || channelId === undefined) return;
    saveEdit(viewer, channelId, editMessage === null ? null : { message: editMessage, text: editText });
  }, [editSettled, channelId, viewer, editMessage, editText]);

  // Once the history is here, a kept reply whose root is not held (or is not
  // a root) is dropped without a word, and the thread of the newest root that
  // still holds one reopens, as an open edit does. Nothing is written before.
  useEffect(() => {
    if (repliesSettled || chat.history !== 'ready') return;
    const kept = new Map<number, string>();
    for (const [root, text] of replyDrafts) {
      const held = chat.byId.get(root);
      if (held !== undefined && held.thread_root_id === undefined && text !== '') kept.set(root, text);
    }
    setReplyDrafts(kept);
    if (kept.size > 0 && openNow.current === null) setOpenThread(Math.max(...kept.keys()));
    setRepliesSettled(true);
  }, [repliesSettled, chat.history, chat.byId, replyDrafts]);
  useEffect(() => {
    if (!repliesSettled || channelId === undefined) return;
    saveReplies(viewer, channelId, replyDrafts);
  }, [repliesSettled, channelId, viewer, replyDrafts]);

  const shown = ordered(chat);
  const pending = chat.pending.filter((send) => send.threadRootId === undefined);
  // Expanded threads share this channel timeline's mark; they deliberately
  // have no second divider or independent unread position.
  // The reducer retains the opening head across view remounts and live arrivals.
  // A fresh history load starts a new visit, even if a cached page rendered first.
  const [unreadBoundary, setUnreadBoundary] = useState<{ readonly head: number; readonly target?: number | null } | null>(null);
  if (unreadBoundary !== null && chat.history === 'loading') setUnreadBoundary(null);
  if (unreadBoundary === null && chat.history === 'ready') {
    setUnreadBoundary({ head: chat.openingHead ?? lastId(chat) });
  }
  // A held page beginning after the mark cannot prove which message was first.
  // Wait until paging crosses it (or exhausts visible history) before placing.
  if (unreadBoundary !== null && unreadBoundary.target === undefined && openingMark !== null
    && ((chat.byId.size > 0 && oldestId(chat) <= openingMark + 1) || chat.exhausted)) {
    const first = shown.find((m) => m.id > openingMark && m.id <= unreadBoundary.head && m.author_id !== viewer && m.removed !== true);
    setUnreadBoundary({ ...unreadBoundary, target: first?.id ?? null });
  }
  const unreadId = unreadBoundary?.target ?? null;
  const unreadHeld = unreadId !== null && chat.byId.has(unreadId);
  const hasUnread = openingMark !== null && unreadBoundary !== null && openingMark < unreadBoundary.head && unreadBoundary.target !== null;
  const divider = useRef<HTMLDivElement>(null);
  const [unreadVisible, setUnreadVisible] = useState(false);
  const [jumpUnread, setJumpUnread] = useState(false);
  const [unreadProblem, setUnreadProblem] = useState('');
  const sentMark = newestSent(shown, viewer, chat.sentIds);
  // The log's roving tab stop (issue #1040): the message last focused, else the
  // newest. A message arriving moves the stop, never focus.
  const list = useRef<HTMLOListElement>(null);
  const [lastFocused, setLastFocused] = useState<number | null>(null);
  const stop = lastFocused !== null && shown.some((message) => message.id === lastFocused) ? lastFocused : shown.at(-1)?.id ?? null;
  const stopNow = useRef(stop);
  stopNow.current = stop;
  // After every render, before paint: new controls (an editor, a thread) join
  // the order only in the stop message.
  useLayoutEffect(() => {
    if (list.current !== null) syncStops(list.current, stop);
  });
  // Controls that mount later on their own (an image's button once it loads).
  useEffect(() => {
    const el = list.current;
    if (el === null || typeof MutationObserver !== 'function') return;
    const watch = new MutationObserver(() => {
      syncStops(el, stopNow.current);
    });
    watch.observe(el, { childList: true, subtree: true });
    return () => {
      watch.disconnect();
    };
  }, []);
  // The top-level message an element is in, if any (not a thread's own).
  function itemOf(target: EventTarget | null): HTMLElement | null {
    let el = target instanceof Element ? target : null;
    while (el !== null && el.parentElement !== list.current) el = el.parentElement;
    return el instanceof HTMLElement && el.dataset.messageId !== undefined ? el : null;
  }
  function onItemKey(event: KeyboardEvent<HTMLOListElement>) {
    if (event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || event.nativeEvent.isComposing) return;
    // Only on the message itself: a text box or button inside keeps its keys.
    const item = event.target instanceof HTMLElement && event.target.parentElement === list.current ? itemOf(event.target) : null;
    if (item === null || list.current === null) return;
    const all = Array.from(list.current.children).filter((el): el is HTMLElement => el instanceof HTMLElement && el.dataset.messageId !== undefined);
    const at = all.indexOf(item);
    let to: number;
    switch (event.key) {
      case 'ArrowRight': {
        const reaction = item.querySelector<HTMLButtonElement>(':scope > .reaction-controls [data-reaction-control]:not(:disabled):not([aria-disabled="true"])');
        if (reaction !== null) { event.preventDefault(); reaction.focus(); }
        return;
      }
      case 'ArrowUp':
        to = Math.max(0, at - 1);
        break;
      case 'ArrowDown':
        to = Math.min(all.length - 1, at + 1);
        break;
      case 'Home':
        to = 0;
        break;
      case 'End':
        to = all.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    all[to]?.focus();
  }
  const replyCounts = useMemo(() => {
    const counts = new Map<number, number>();
    for (const held of chat.byId.values()) {
      if (held.thread_root_id !== undefined) counts.set(held.thread_root_id, (counts.get(held.thread_root_id) ?? 0) + 1);
    }
    return counts;
  }, [chat.byId]);
  const threadAnchor = openThread === null ? null : shown.some((message) => message.id === openThread) ? openThread : shown.find((message) => message.thread_root_id === openThread)?.id ?? null;
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

  // A change of chat density makes every message taller or shorter without
  // changing the newest one or the log: a reader at the bottom stays there,
  // and one who has scrolled up keeps the message they were reading in view.
  useLayoutEffect(() => {
    if (shownDensity.current === density) return;
    shownDensity.current = density;
    const el = log.current;
    if (el === null) return;
    if (atBottom.current) {
      el.scrollTop = el.scrollHeight;
      return;
    }
    const held = view.current;
    const row = held === null ? null : el.querySelector<HTMLElement>(`:scope > .messages > li[data-message-id="${held.id}"]`);
    if (held !== null && row !== null) el.scrollTop += row.getBoundingClientRect().top - el.getBoundingClientRect().top - held.offset;
  }, [density]);

  // The New messages divider is usually placed after the log has settled at
  // the newest message: the opening read mark arrives after the history, or
  // paging crosses it. The divider, and the full header its first unread
  // message then shows, add height above the view without changing the newest
  // message, so the effect above does not follow it. A reader at the bottom
  // stays there; one who has scrolled up keeps the message they were reading
  // where it was. Only Jump to first unread scrolls to the divider.
  const placedUnread = useRef<number | null>(null);
  useLayoutEffect(() => {
    if (placedUnread.current === unreadId) return;
    placedUnread.current = unreadId;
    const el = log.current;
    if (el === null) return;
    if (atBottom.current) {
      el.scrollTop = el.scrollHeight;
      return;
    }
    const held = view.current;
    const row = held === null ? null : el.querySelector<HTMLElement>(`:scope > .messages > li[data-message-id="${held.id}"]`);
    if (held !== null && row !== null) el.scrollTop += row.getBoundingClientRect().top - el.getBoundingClientRect().top - held.offset;
  }, [unreadId]);

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

  // Visibility is relative to the scrolling log, including after resizing on
  // a phone or opening a thread. The jump remains available for unloaded rows.
  useEffect(() => {
    const root = log.current;
    const bar = divider.current;
    setUnreadVisible(false);
    if (root === null || bar === null || typeof IntersectionObserver !== 'function') return;
    const observer = new IntersectionObserver(([entry]) => {
      setUnreadVisible(entry?.isIntersecting ?? false);
    }, { root });
    observer.observe(bar);
    return () => { observer.disconnect(); };
  }, [unreadId, unreadHeld]);

  useEffect(() => {
    if (!jumpUnread || loadingOlder || chat.history !== 'ready') return;
    if (unreadId !== null && chat.byId.has(unreadId)) {
      const row = list.current?.querySelector<HTMLElement>(`:scope > [data-message-id="${String(unreadId)}"]`);
      if (row !== null && row !== undefined) {
        atBottom.current = false;
        onBottom(false);
        row.scrollIntoView({ block: 'center' });
        row.focus({ preventScroll: true });
      }
      setJumpUnread(false);
    } else if (unreadBoundary?.target === null) {
      setUnreadProblem('The first unread message is no longer available.');
      setJumpUnread(false);
    } else if (chat.older === 'error') {
      setUnreadProblem('Cannot load older messages. Try jumping again.');
      setJumpUnread(false);
    } else if (hasOlder) {
      showOlder();
    } else {
      setUnreadProblem(chat.exhausted ? 'The first unread message is no longer available.' : 'Cannot reach the first unread message: the history limit has been reached.');
      setJumpUnread(false);
    }
  });

  function jump() {
    const el = log.current;
    if (el !== null) el.scrollTop = el.scrollHeight;
    atBottom.current = true;
    setUnseen(false);
    onBottom(true);
  }

  return (
    <ReactionScope key={`${String(viewer)}/${String(channelId ?? channel)}`} channel={channel} channelId={channelId ?? shown[0]?.channel_id ?? 0} ids={shown.filter((m) => m.removed !== true).map((m) => m.id)} onUnauthorized={onUnauthorized}>
      <header className="conv-head">
        {hasUnread && !unreadVisible ? (
          <button type="button" className="jump-unread primary" disabled={jumpUnread} onClick={() => { setUnreadProblem(''); setJumpUnread(true); }}>
            {jumpUnread ? 'Loading first unread…' : 'Jump to first unread'}
          </button>
        ) : null}
        <span className="hash" aria-hidden="true">
          #
        </span>
        {visibility === 'private' ? <PrivateMark /> : null}
        <div className="channel-heading">
          <h2 id={headingId} tabIndex={-1}>
            {channel}
          </h2>
          {topic === undefined || topic === '' ? null : <p className="channel-topic">{topic}</p>}
        </div>
        {onSearch === undefined ? null : <button id="channel-search-open" type="button" onClick={onSearch}>Search</button>}
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
      <AdminApprovals channelId={channelId} viewer={viewer} names={(id) => nameOf(members, id)} onUnauthorized={onUnauthorized} />
      <div className="log-wrap">
      <div
        ref={log}
        className="log"
        role="log"
        aria-label="Messages"
        {...(shown.length === 0 ? { tabIndex: 0 } : {})}
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
          view.current = topOfView(el);
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
        {chat.history === 'ready' && shown.length === 0 && chat.pending.length === 0 ? (
          <div className="hint empty-conversation">
            <p>No messages here that you can see yet.</p>
            {historyBounded ? <p>Messages sent before you joined aren&apos;t shown.</p> : null}
            <p>Write a message to start.</p>
          </div>
        ) : null}
        <ol
          ref={list}
          className="messages"
          onKeyDown={onItemKey}
          onFocus={(event) => {
            const item = itemOf(event.target);
            if (item !== null) setLastFocused(Number(item.dataset.messageId));
          }}
        >
          {shown.map((message, i) => {
            const before = shown[i - 1];
            const startsDay = !sameLocalDay(before?.created_at, message.created_at);
            return <Fragment key={message.id}>
              {startsDay ? <DayDivider at={message.created_at} /> : null}
              {message.id === unreadId ? (
                <li className="new-messages-row" role="presentation">
                  <div ref={divider} className="new-messages" role="separator" aria-label="New messages">New messages</div>
                </li>
              ) : null}
              <Message
                highlighted={message.id === highlightedId && message.thread_root_id === undefined}
                stop={message.id === stop}
                reactionNavigation
                replies={replyCounts.get(message.id) ?? 0}
                {...(message.thread_root_id === undefined ? {} : { context: replyContext(chat.byId.get(message.thread_root_id), people) })}
                message={message}
                people={people}
                nets={nets}
                viewer={viewer}
                channel={channel}
                sent={message.id === sentMark}
                cont={!startsDay && message.id !== unreadId && continues(before, message)}
                onUnauthorized={onUnauthorized}
                onReply={openFrom}
                onOpenThread={openFrom}
                actions={onEdited !== undefined && editable(message) ? [{ key: 'edit', label: 'Edit', onSelect: () => { startEdit(message.id); } }] : []}
                notice={notices.get(message.id) ?? null}
                editor={editThread === null ? editorFor(message) : undefined}
                thread={threadAnchor === null || message.id !== threadAnchor ? null : (
                  <ThreadView
                    highlightedId={highlightedId}
                    key={openThread}
                    rootId={openThread as number}
                    root={chat.byId.get(openThread as number)}
                    chat={chat}
                    channel={channel}
                    live={live}
                    archived={archived}
                    people={people}
                    nets={nets}
                    viewer={viewer}
                    draft={replyDrafts.get(openThread as number) ?? ''}
                    onDraft={(text) => {
                      setReplyDrafts((previous) => new Map(previous).set(openThread as number, text));
                    }}
                    onEditLast={(messages, composer) => editLast(messages, composer, openThread)}
                    editorFor={(message, onSaved) => editThread === openThread ? editorFor(message, onSaved) : undefined}
                    notices={notices}
                    onClose={closeThread}
                    onReplySend={onReplySend}
                    onRetrySend={onRetrySend}
                    onDismissSend={onDismissSend}
                    onUnauthorized={onUnauthorized}
                  />
                )}
              />
            </Fragment>;
          })}
          {pending.map((send, i) => (
            <Pending key={`local-${String(send.localId)}`} send={send} after={pending[i - 1]?.at ?? shown.at(-1)?.created_at} me={me} nets={nets} viewer={viewer} onRetry={onRetrySend} onDismiss={onDismissSend} />
          ))}
        </ol>
      </div>
      {unreadProblem === '' ? null : (
        <div className="unread-problem">
          <p role="status">{unreadProblem}</p>
          <button type="button" onClick={() => { setUnreadProblem(''); }}>Dismiss unread notice</button>
        </div>
      )}
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
      {archived ? (
        <p role="status" className="hint archived-note">{archivedLine}</p>
      ) : (
      <Composer
        channel={channel}
        target={target}
        nets={nets}
        monitored={monitored}
        people={members}
        viewer={viewer}
        draft={draft}
        enabled={live === 'open'}
        paused={live === 'reconnecting'}
        onDraft={onDraft}
        onTarget={onTarget}
        onSend={onSend}
        onEditLast={(composer) => editLast(shown, composer)}
        atBottom={() => {
          atBottom.current = true;
          onBottom(true);
        }}
      />
      )}
    </ReactionScope>
  );
}
