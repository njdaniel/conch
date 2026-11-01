import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import * as api from './api';
import { Composer } from './Composer';
import { guestName } from './guestName';
import { Avatar } from './Icons';
import { ImageAttachment } from './ImageAttachment';
import { loadStored, saveEdit } from './state/draftStore';
import { canEdit, EditedMarker, editEnds, editWindowMs, MessageEditor } from './MessageEdit';
import { ActionsMenu, useOpeners, type MenuItem } from './MessageMenu';
import { approvalPayloadSchema, approvalV1, type ApprovalV1 } from './schema/approval';
import { fileAttachmentPayloadSchema, fileAttachmentV1, type FileAttachmentV1 } from './schema/file';
import type { Audience, MessageV2 } from './schema/message';
import type { Target, TargetAction } from './state/drafts';
import type { ChannelChat, PendingSend } from './state/messages';
import { canLoadOlder, isLater, lastId, oldestId, ordered } from './state/messages';
import type { LiveStatus, Person } from './useLiveChannel';
import { mentions } from './useNotifications';

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

// authorOf names a message's author. A post made during a guest visit is
// named by the attribution stored with it (issue #713, design §4.4), the same
// for ever: after the visit ended, after the guest made an account, and
// whoever is in the members list now. Anyone else is named from the list.
export function authorOf(message: MessageV2, people: ReadonlyMap<number, Person>): string {
  return message.guest_author === undefined ? nameOf(people, message.author_id) : guestName(message.guest_author);
}

function attachmentsOf(message: MessageV2): FileAttachmentV1 | null {
  const payload = message.payload;
  if (payload?.schema !== fileAttachmentPayloadSchema || fileAttachmentV1(payload.data).length > 0) return null;
  return payload.data as FileAttachmentV1;
}

export function Message({
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
  thread,
  actions,
  editor,
  notice,
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
  readonly thread?: ReactNode;
  // The message's actions menu (issue #845): only what the viewer may do
  // here, and none at all draws no menu.
  readonly actions?: readonly MenuItem[];
  // The inline editor, in place of the text, while the viewer edits it.
  readonly editor?: ReactNode;
  // A line about the message, under it: an edit that could not be made.
  readonly notice?: string | null;
}) {
  const approval = approvalOf(message);
  const attachments = attachmentsOf(message);
  const items = editor === undefined ? actions ?? [] : [];
  const [menuOpen, setMenuOpen] = useState(false);
  const more = useRef<HTMLButtonElement>(null);
  const openers = useOpeners(items.length > 0, () => {
    setMenuOpen(true);
  });
  const named = approval === null && editor === undefined && mentions(message.body, people.get(viewer)?.name ?? '');
  const classes = ['message', ...(cont ? ['cont'] : []), ...(named ? ['mentions-you'] : []), ...(items.length > 0 ? ['has-actions'] : []), ...(editor === undefined ? [] : ['editing'])];
  return (
    <li className={classes.join(' ')} data-message-id={message.id} tabIndex={-1} {...openers}>
      {named ? <span className="vh">Mentions you. </span> : null}
      <Line
        time={clock(message.created_at)}
        author={authorOf(message, people)}
        authorId={message.author_id}
        kind={people.get(message.author_id)?.kind ?? null}
        badge={message.audience === undefined ? null : audienceLabel(message.audience, viewer, nets)}
        tag={sent ? 'Sent' : null}
      >
        {editor ?? (approval === null ? (
          <>
            {message.body}
            <EditedMarker message={message} channel={channel} viewer={viewer} onUnauthorized={onUnauthorized} />
          </>
        ) : <ApprovalCard approval={approval} people={people} />)}
        {attachments === null ? null : (
          <span className="attachments">
            {attachments.file_ids.map((fileId) => (
              <ImageAttachment key={fileId} fileId={fileId} channel={channel} viewer={viewer} onUnauthorized={onUnauthorized} />
            ))}
          </span>
        )}
      </Line>
      {notice === undefined || notice === null ? null : <p role="status" className="edit-notice">{notice}</p>}
      {items.length === 0 ? null : (
        <ActionsMenu label={`More actions for message ${String(message.id)}`} items={items} open={menuOpen} onOpenChange={setMenuOpen} button={more} />
      )}
      {onOpen === undefined ? null : <button type="button" className="search-open" aria-label={`Open context for message ${String(message.id)}`} onClick={() => {
        onOpen(message.id);
      }}>Open context</button>}
      {onReply === undefined || message.thread_root_id !== undefined ? null : <button type="button" className="message-action reply-action" aria-label={`Reply to message ${String(message.id)}`} onClick={(event) => {
        onReply(message.id, event.currentTarget);
      }}>Reply</button>}
      {onOpenThread === undefined || message.thread_root_id === undefined ? null : <button type="button" className="message-action" aria-label={`Open thread for message ${String(message.thread_root_id)}`} onClick={(event) => {
        onOpenThread(message.thread_root_id as number, event.currentTarget);
      }}>Open thread</button>}
      {thread}
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
  // A guest who made an account is the same principal: its posts as a guest
  // and as a member are drawn apart.
  if (before.guest_author?.handle !== message.guest_author?.handle) return false;
  if (audienceKey(before.audience) !== audienceKey(message.audience)) return false;
  if (approvalOf(before) !== null || approvalOf(message) !== null) return false;
  return Date.parse(message.created_at) - Date.parse(before.created_at) < sameRun;
}

function ThreadView({ rootId, root, chat, channel, live, people, nets, viewer, draft, onDraft, onClose, onReplySend, onRetrySend, onDismissSend, onUnauthorized }: {
  readonly rootId: number;
  readonly root: MessageV2 | undefined;
  readonly chat: ChannelChat;
  readonly channel: string;
  readonly live: LiveStatus;
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
        closeNow.current();
        if (result.failure === 'unauthorized') unauthorizedNow.current?.();
      } else setStatus('error');
      return;
    }
    setFetched((previous) => append ? [...previous, ...result.value.messages] : result.value.messages);
    setNextAfter(result.value.next_after);
    setStatus('ready');
  }, [channel, rootId]);

  useEffect(() => {
    setFetched([]);
    setNextAfter(undefined);
    void load(0, false);
    return () => {
      request.current++;
    };
  }, [load]);

  const byId = new Map<number, MessageV2>();
  if (root !== undefined) byId.set(root.id, root);
  for (const message of [...fetched, ...ordered(chat)]) {
    if (message.id !== rootId && message.thread_root_id !== rootId) continue;
    // The thread's own read and the channel's copy: the later edit wins.
    const held = byId.get(message.id);
    if (held === undefined || isLater(message, held)) byId.set(message.id, message);
  }
  const messages = [...byId.values()].sort((a, b) => a.id - b.id);
  const rootMessage = byId.get(rootId);
  const me = people.get(viewer) ?? { name: `#${String(viewer)}`, kind: null };
  const pending = chat.pending.filter((send) => send.threadRootId === rootId);
  return (
    <section className="thread-view" aria-label={`Thread around message ${String(rootId)}`}>
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
        {messages.map((message) => <Message key={message.id} message={message} people={people} nets={nets} viewer={viewer} channel={channel} sent={chat.sentIds.has(message.id)} cont={false} onUnauthorized={onUnauthorized} />)}
        {pending.map((send) => <Pending key={`local-${String(send.localId)}`} send={send} me={me} nets={nets} viewer={viewer} onRetry={onRetrySend} onDismiss={onDismissSend} />)}
      </ol>
      {nextAfter === undefined ? null : (
        <button type="button" disabled={status === 'loading'} onClick={() => {
          void load(nextAfter, true);
        }}>Load more replies</button>
      )}
      {rootMessage === undefined ? null : (
        <form className="reply-composer" onSubmit={(event) => {
          event.preventDefault();
          if (live !== 'open' || draft.trim() === '') return;
          onReplySend(draft, rootId, rootMessage.audience);
          onDraft('');
        }}>
          <p className="hint">Replying to {rootMessage.audience === undefined ? `#${channel}` : audienceLabel(rootMessage.audience, viewer, nets)}; the audience cannot be changed.</p>
          <label htmlFor={`reply-${String(rootId)}`}>Reply to message #{rootId}</label>
          <div className="reply-box">
            <textarea id={`reply-${String(rootId)}`} autoFocus rows={2} value={draft} disabled={live !== 'open'} onChange={(event) => {
              onDraft(event.target.value);
            }} onKeyDown={(event) => {
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
  );
}

// The open channel's heading, where focus goes on arriving in the lobby
// (issue #590). It is not the composer: on a phone that would open the
// keyboard over the page.
export const headingId = 'conversation-heading';

// How close to the bottom still counts as being at it, in pixels.
const nearBottom = 24;

// The open channel: its messages in id order, and the box to write in.
export function Conversation({
  channelId,
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
  onReplySend,
  onRetrySend,
  onDismissSend,
  onLoadOlder,
  onReload,
  onBottom,
  onUnauthorized,
  onSearch,
  onEdited,
  historyBounded = false,
}: {
  // The viewer's own membership here has a history bound (history_after), so
  // what was said before they joined is not shown (issue #1034). False when
  // it has none or is not known yet: the page never claims hiding it cannot see.
  readonly historyBounded?: boolean;
  // The channel's id, for keeping an open edit across a reload (issue #1039);
  // without it nothing is kept.
  readonly channelId?: number;
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
  const [openThread, setOpenThread] = useState<number | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  const threadReturn = useRef<{ readonly root: number; readonly anchor: number | null; readonly opener: HTMLElement | null } | null>(null);
  const openFrom = useCallback((id: number, from: HTMLElement) => {
    opener.current = from;
    setOpenThread(id);
  }, []);
  const [replyDrafts, setReplyDrafts] = useState<ReadonlyMap<number, string>>(new Map());
  const openNow = useRef(openThread);
  const chatNow = useRef(chat);
  openNow.current = openThread;
  chatNow.current = chat;
  const closeThread = useCallback(() => {
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
      : list?.querySelector<HTMLElement>(`[data-message-id="${String(back.root)}"] .reply-action`)
        ?? list?.querySelector<HTMLElement>(`[data-message-id="${String(back.root)}"]`)
        ?? list?.querySelector<HTMLElement>(`[data-message-id="${String(back.anchor)}"]`);
    target?.focus();
  }, [openThread]);

  // Editing (issue #845, docs/design/message-editing.md §13.2): one editor at
  // a time, its text kept here; a line under a message an edit could not
  // reach; and the time the window is counted from, moved on when the next
  // of the viewer's messages leaves it, so Edit goes when it can no longer be
  // used.
  const [edit, setEdit] = useState<{ readonly id: number; readonly text: string; readonly next: number | null } | null>(null);
  const [notices, setNotices] = useState<ReadonlyMap<number, string>>(new Map());
  const [now, setNow] = useState(() => Date.now());
  const focusBack = useRef<number | null>(null);
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
  useEffect(() => {
    const id = focusBack.current;
    if (id === null || edit !== null) return;
    focusBack.current = null;
    const item = log.current?.querySelector<HTMLElement>(`[data-message-id="${String(id)}"]`);
    (item?.querySelector<HTMLElement>('.more-actions') ?? item)?.focus();
  }, [edit]);
  const note = useCallback((id: number, line: string | null) => {
    setNotices((previous) => {
      if (line === null && !previous.has(id)) return previous;
      const next = new Map(previous);
      if (line === null) next.delete(id);
      else next.set(id, line);
      return next;
    });
  }, []);
  const startEdit = useCallback((id: number) => {
    const message = chatNow.current.byId.get(id);
    if (message === undefined) return;
    note(id, null);
    setEdit((current) => {
      // Unsaved text elsewhere is dropped only once the person says so.
      const was = current === null ? undefined : chatNow.current.byId.get(current.id);
      if (current !== null && current.id !== id && was !== undefined && current.text !== was.body) return { ...current, next: id };
      return { id, text: message.body, next: null };
    });
  }, [note]);
  const closeEdit = useCallback((id: number) => {
    focusBack.current = id;
    setEdit((current) => (current?.id === id ? null : current));
  }, []);

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
  const editMessage = edit?.id ?? null;
  const editText = edit?.text ?? '';
  useEffect(() => {
    if (!editSettled || channelId === undefined) return;
    saveEdit(viewer, channelId, editMessage === null ? null : { message: editMessage, text: editText });
  }, [editSettled, channelId, viewer, editMessage, editText]);

  const shown = ordered(chat);
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
        {chat.history === 'ready' && shown.length === 0 && chat.pending.length === 0 ? (
          <div className="hint empty-conversation">
            <p>No messages here that you can see yet.</p>
            {historyBounded ? <p>Messages sent before you joined aren&apos;t shown.</p> : null}
            <p>Write a message to start.</p>
          </div>
        ) : null}
        <ol className="messages">
          {shown.map((message, i) => (
            <Message
              key={message.id}
              message={message}
              people={people}
              nets={nets}
              viewer={viewer}
              channel={channel}
              sent={chat.sentIds.has(message.id)}
              cont={continues(shown[i - 1], message)}
              onUnauthorized={onUnauthorized}
              onReply={openFrom}
              onOpenThread={openFrom}
              actions={onEdited !== undefined && canEdit(message, viewer, viewerKind, now) ? [{ key: 'edit', label: 'Edit', onSelect: () => { startEdit(message.id); } }] : []}
              notice={notices.get(message.id) ?? null}
              {...(edit?.id !== message.id || onEdited === undefined ? {} : {
                editor: (
                  <MessageEditor
                    message={message}
                    channel={channel}
                    text={edit.text}
                    onText={(text) => {
                      setEdit((current) => (current?.id === message.id ? { ...current, text } : current));
                    }}
                    onCancel={() => {
                      closeEdit(message.id);
                    }}
                    onSaved={(saved) => {
                      onEdited(saved);
                      closeEdit(message.id);
                    }}
                    onGone={(line) => {
                      note(message.id, line);
                      closeEdit(message.id);
                    }}
                    onUnauthorized={onUnauthorized}
                    discard={edit.next === null ? undefined : {
                      onDiscard: () => {
                        const next = edit.next;
                        const target = next === null ? undefined : chatNow.current.byId.get(next);
                        setEdit(target === undefined ? null : { id: target.id, text: target.body, next: null });
                      },
                      onKeep: () => {
                        setEdit((current) => (current === null ? null : { ...current, next: null }));
                      },
                    }}
                  />
                ),
              })}
              thread={threadAnchor === null || message.id !== threadAnchor ? null : (
                <ThreadView
                  key={openThread}
                  rootId={openThread as number}
                  root={chat.byId.get(openThread as number)}
                  chat={chat}
                  channel={channel}
                  live={live}
                  people={people}
                  nets={nets}
                  viewer={viewer}
                  draft={replyDrafts.get(openThread as number) ?? ''}
                  onDraft={(text) => {
                    setReplyDrafts((previous) => new Map(previous).set(openThread as number, text));
                  }}
                  onClose={closeThread}
                  onReplySend={onReplySend}
                  onRetrySend={onRetrySend}
                  onDismissSend={onDismissSend}
                  onUnauthorized={onUnauthorized}
                />
              )}
            />
          ))}
          {chat.pending.filter((send) => send.threadRootId === undefined).map((send) => (
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
        paused={live === 'reconnecting'}
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
