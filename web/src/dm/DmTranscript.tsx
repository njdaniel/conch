// The DM transcript: one conversation, by id, as a view of its own. It takes
// everything from props (the API, the scope, the viewer), so the inbox route
// mounts it and a test mounts it alone against fixtures.
//
// Shown: the other person's name (kept for old messages when they and the
// viewer stop sharing a channel: the name is the conversation's, not looked up
// in the directory), history oldest first with "Load earlier", sends with
// Retry, the author's own edit and removal, the removal tombstone (the
// server's copy, so it is the same after a reconnect), read marks, and the
// access the viewer has now. No reactions, threads, mentions or typed
// payloads: a DM has none.
import { useEffect, useLayoutEffect, useRef, useState, type KeyboardEvent, type SyntheticEvent } from 'react';
import { useBlocksContext } from '../blocks';
import { Avatar } from '../Icons';
import { formatSize } from '../fileSize';
import type { DMConversationV1, DMFileV1, DMMessageV1, DMReadStateV1 } from '../schema/generated';
import { dateTimeOf, timeOf } from '../time';
import { isDmOperationServed, type DmApi, type DmFailure } from './api';
import { DmComposer } from './DmComposer';
import { canRetrySend, cannotMessage, noAccess, peerName, type Pending, type PendingSends } from './model';
import { useDmTranscript } from './useDmTranscript';

export interface DmConversationViewProps {
  readonly conversationId: number;
  readonly api: DmApi;
  // Whose ids and drafts these are: an origin and an account. A change of it
  // empties the view.
  readonly scope: string;
  readonly viewer: number;
  readonly draft: string;
  readonly onDraft: (conversation: number, text: string) => void;
  readonly pendingSends?: PendingSends;
  readonly onRead?: (state: DMReadStateV1) => void;
  readonly enabled?: boolean;
  readonly viewing?: boolean;
}

const failureLine = (failure: DmFailure): string => (failure === 'unavailable' ? cannotMessage : failure === 'rate_limited' ? 'You are sending too fast. Wait a moment.' : 'The server refused that. Try again.');

function FileChip({ api, conversation, id }: { readonly api: DmApi; readonly conversation: number; readonly id: number }) {
  const [file, setFile] = useState<DMFileV1 | 'unavailable' | null>(null);
  const [problem, setProblem] = useState('');
  useEffect(() => {
    let current = true;
    setFile(null);
    void api.fileMeta(conversation, id).then((r) => {
      if (current) setFile(r.ok ? r.value : 'unavailable');
    });
    return () => {
      current = false;
    };
  }, [api, conversation, id]);
  if (file === null) return <span className="hint">Loading file…</span>;
  if (file === 'unavailable') return <span className="hint">File unavailable</span>;
  const held = file;
  async function save(button: HTMLButtonElement) {
    setProblem('');
    if (api.saveFile !== undefined) {
      const result = await api.saveFile(conversation, id);
      if (button.isConnected && !result.ok) setProblem('Could not download the file.');
      return;
    }
    // In the browser, download the bytes as the server sent them.
    const result = await api.fileBytes(conversation, id);
    if (!button.isConnected) return;
    if (!result.ok) {
      setProblem('Could not download the file.');
      return;
    }
    const url = URL.createObjectURL(result.value);
    const link = document.createElement('a');
    link.href = url;
    link.download = held.filename;
    link.click();
    URL.revokeObjectURL(url);
  }
  return (
    <span className="dm-file">
      <button type="button" aria-label={`Download ${held.filename}`} onClick={(event) => void save(event.currentTarget)}>
        {held.filename}
      </button>{' '}
      <span className="hint">{formatSize(held.size)}</span>
      {problem === '' ? null : <span role="alert" className="hint"> {problem}</span>}
    </span>
  );
}

function MessageRow({ message, mine, author, api, conversation, canWrite, onEdit, onRemove }: {
  readonly message: DMMessageV1;
  readonly mine: boolean;
  readonly author: string;
  readonly api: DmApi;
  readonly conversation: number;
  readonly canWrite: boolean;
  readonly onEdit: (id: number, body: string) => Promise<DmFailure | null>;
  readonly onRemove: (id: number) => Promise<DmFailure | null>;
}) {
  const [mode, setMode] = useState<'view' | 'edit' | 'remove'>('view');
  const [text, setText] = useState(message.body);
  const [problem, setProblem] = useState('');
  const [busy, setBusy] = useState(false);
  const removed = message.removed === true;
  const canDownload = isDmOperationServed('dmFileMeta') && isDmOperationServed('dmFileBytes');
  const when = timeOf(message.created_at);
  const row = useRef<HTMLLIElement>(null);
  const editButton = useRef<HTMLButtonElement>(null);
  const removeButton = useRef<HTMLButtonElement>(null);
  const editor = useRef<HTMLTextAreaElement>(null);
  const keepButton = useRef<HTMLButtonElement>(null);
  const restore = useRef<'edit' | 'remove' | null>(null);
  const shownMode = removed || (mode === 'edit' && !canWrite) ? 'view' : mode;

  useLayoutEffect(() => {
    if (row.current?.closest('[inert]') !== null) return;
    if (shownMode === 'edit') editor.current?.focus();
    else if (shownMode === 'remove') keepButton.current?.focus();
    else if (restore.current !== null) {
      const target = restore.current === 'edit' ? editButton : removeButton;
      (target.current ?? row.current).focus();
      restore.current = null;
    }
  }, [shownMode]);

  function end() {
    const active = document.activeElement;
    if (active === document.body || row.current?.contains(active) === true) {
      if (mode !== 'view') restore.current = mode;
    }
    setMode('view');
    setProblem('');
  }

  function cancelOnEscape(event: KeyboardEvent) {
    if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
    event.preventDefault();
    end();
  }

  async function save(event: SyntheticEvent) {
    event.preventDefault();
    if (busy || text.trim() === '' || text === message.body) return;
    setBusy(true);
    const failure = await onEdit(message.id, text);
    setBusy(false);
    if (failure === null) end();
    else setProblem(failureLine(failure));
  }
  async function confirm() {
    setBusy(true);
    const failure = await onRemove(message.id);
    setBusy(false);
    if (failure === null) end();
    else setProblem(failureLine(failure));
  }

  return (
    <li ref={row} tabIndex={-1} className={`message dm-message${removed ? ' removed' : ''}${mine ? ' mine' : ''}`} data-message={`dm:${String(conversation)}:${String(message.id)}`}>
      <span className="dm-author">{author}</span> <time dateTime={message.created_at} title={dateTimeOf(message.created_at)}>{when}</time>
      {message.edited_at === undefined || removed ? null : <span className="hint"> (edited)</span>}
      {shownMode === 'edit' ? (
        <form onSubmit={(event) => void save(event)} onKeyDown={cancelOnEscape}>
          <label>
            Edit message
            <textarea
              ref={editor}
              rows={2}
              readOnly={busy}
              value={text}
              onChange={(event) => { setText(event.target.value); }}
            />
          </label>
          <button type="submit" disabled={busy || text.trim() === '' || text === message.body}>Save</button>
          <button type="button" onClick={end}>Cancel</button>
        </form>
      ) : (
        <p className="dm-body">{removed ? <em>{message.body}</em> : message.body}</p>
      )}
      {!removed && canDownload ? (message.file_ids ?? []).map((id) => <FileChip key={id} api={api} conversation={conversation} id={id} />) : null}
      {mine && !removed && shownMode === 'view' ? (
        <span className="dm-actions">
          {canWrite ? <button ref={editButton} type="button" aria-label={`Edit your message from ${when}`} onClick={() => { setText(message.body); setMode('edit'); }}>Edit</button> : null}
          <button ref={removeButton} type="button" aria-label={`Remove your message from ${when}`} onClick={() => { setMode('remove'); }}>Remove</button>
        </span>
      ) : null}
      {shownMode === 'remove' ? (
        <span role="group" aria-label="Remove this message?" onKeyDown={cancelOnEscape}>
          Remove this message?{' '}
          <button type="button" disabled={busy} onClick={() => void confirm()}>Yes, remove</button>
          <button ref={keepButton} type="button" onClick={end}>Keep</button>
        </span>
      ) : null}
      {problem === '' ? null : <p role="alert" className="hint">{problem}</p>}
    </li>
  );
}

function PendingRow({ pending, canWrite, onRetry, onDiscard }: { readonly pending: Pending; readonly canWrite: boolean; readonly onRetry: () => void; readonly onDiscard: () => void }) {
  const line =
    pending.status === 'sending' ? 'Sending…' : pending.failure === 'unavailable' ? cannotMessage : pending.failure === 'dms_off' ? 'Direct messages are off.' : pending.status === 'unconfirmed' ? 'Not confirmed. It may not have been sent.' : 'Not sent.';
  return (
    <li className="message dm-message pending" data-pending={pending.nonce}>
      <p className="dm-body">{pending.body}</p>
      <span role={pending.status === 'sending' ? 'status' : 'alert'} className="hint">{line}</span>
      {pending.status === 'sending' ? null : (
        <>
          {canWrite && canRetrySend(pending) ? <button type="button" onClick={onRetry}>Retry</button> : null}
          <button type="button" onClick={onDiscard}>Discard</button>
        </>
      )}
    </li>
  );
}

// Search results: this conversation only, merged by id across pages.
function Search({ api, conversation, viewer, name, onClose }: {
  readonly api: DmApi;
  readonly conversation: number;
  readonly viewer: number;
  readonly name: string;
  readonly onClose: () => void;
}) {
  const [words, setWords] = useState('');
  const [found, setFound] = useState<{ readonly query: string; readonly messages: readonly DMMessageV1[]; readonly next: number | null } | null>(null);
  const [problem, setProblem] = useState('');
  const turn = useRef(0);
  const input = useRef<HTMLInputElement>(null);
  useLayoutEffect(() => {
    if (input.current?.closest('[inert]') === null) input.current.focus();
  }, []);
  useEffect(() => () => { turn.current++; }, []);

  async function run(query: string, before?: number) {
    const mine = ++turn.current;
    const result = await api.search(conversation, query, before);
    if (mine !== turn.current) return;
    if (!result.ok) {
      setProblem('Search failed. Try again.');
      return;
    }
    setProblem('');
    const next = result.value.next_before ?? null;
    setFound((held) => {
      const base = before === undefined || held === null ? [] : held.messages;
      const seen = new Set(base.map((m) => m.id));
      return { query, messages: [...base, ...result.value.messages.filter((m) => !seen.has(m.id))], next };
    });
  }

  return (
    <section aria-label="Search this conversation" className="dm-search" onKeyDown={(event) => {
      if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
      event.preventDefault();
      onClose();
    }}>
      <form
        role="search"
        onSubmit={(event) => {
          event.preventDefault();
          if (words.trim() !== '') void run(words.trim());
        }}
      >
        <label>
          Search this conversation
          <input ref={input} type="search" value={words} onChange={(event) => { setWords(event.target.value); }} />
        </label>
        <button type="submit" disabled={words.trim() === ''}>Search</button>
        <button type="button" onClick={onClose}>Close search</button>
      </form>
      {problem === '' ? null : <p role="alert" className="hint">{problem}</p>}
      {found === null ? null : found.messages.length === 0 ? (
        <p className="empty">{`Nothing found for “${found.query}”.`}</p>
      ) : (
        <ul aria-label="Search results">
          {found.messages.map((m) => (
            <li key={m.id}>
              <strong>{m.author_id === viewer ? 'You' : name}</strong> <time dateTime={m.created_at}>{dateTimeOf(m.created_at)}</time>
              <p className="dm-body">{m.body}</p>
            </li>
          ))}
        </ul>
      )}
      {found?.next === null || found === null ? null : <button type="button" onClick={() => void run(found.query, found.next ?? undefined)}>More results</button>}
    </section>
  );
}

export function DmTranscript(props: DmConversationViewProps) {
  return <TranscriptView key={`${props.scope}/${String(props.viewer)}/${String(props.conversationId)}`} {...props} />;
}

function TranscriptView({ conversationId, api, scope, viewer, draft, onDraft, onRead, pendingSends, enabled = true, viewing = true }: DmConversationViewProps) {
  const { blocked } = useBlocksContext();
  const t = useDmTranscript(api, scope, conversationId, viewing, enabled, blocked, onRead, pendingSends);
  const { state, canWrite } = t;
  const [searching, setSearching] = useState(false);
  // The text being written is state here (the provider's drafts only keep it
  // across a switch), so typing re-renders.
  const [text, setText] = useState(draft);
  const log = useRef<HTMLDivElement>(null);
  const pageOpener = useRef<HTMLButtonElement | null>(null);
  const searchButton = useRef<HTMLButtonElement>(null);
  const conversation: DMConversationV1 | null = state.conversation;
  const peer = conversation === null ? '' : peerName(conversation);
  const newest = t.messages.at(-1)?.id;
  const newestPending = t.pending.at(-1)?.nonce;

  // A new message at the bottom is brought into view; reading older history is not.
  useEffect(() => {
    const el = log.current;
    if (el !== null && typeof el.scrollTo === 'function') el.scrollTo({ top: el.scrollHeight });
  }, [newest, newestPending, state.status]);

  useLayoutEffect(() => {
    if (state.loadingOlder) return;
    const opener = pageOpener.current;
    pageOpener.current = null;
    if (opener === null || opener.isConnected || document.activeElement !== document.body) return;
    const target = log.current?.querySelector<HTMLElement>('ol > .dm-message') ?? log.current;
    target?.focus();
  }, [state.loadingOlder, state.older]);

  if (state.status === 'gone' || state.status === 'own_sign_in_required') {
    return (
      <section aria-label="Conversation" className="dm-transcript">
        <p role="alert" className="banner">{state.status === 'gone' ? noAccess : 'Direct messages need your own sign-in.'}</p>
      </section>
    );
  }
  if (state.status === 'loading') {
    return (
      <section aria-label="Conversation" className="dm-transcript" aria-busy="true">
        <p className="empty">Loading conversation…</p>
      </section>
    );
  }
  if (state.status === 'failed' || conversation === null) {
    return (
      <section aria-label="Conversation" className="dm-transcript">
        <div className="problem">
          <p className="empty" role="alert">Cannot load this conversation</p>
          <button type="button" onClick={t.reload}>Try again</button>
        </div>
      </section>
    );
  }

  const historyAvailable = state.status === 'ready';
  const canSearch = historyAvailable && isDmOperationServed('searchDmMessages');
  return (
    <section aria-label={`Conversation with ${peer}`} className="dm-transcript">
      <header className="dm-header">
        <Avatar id={conversation.peer_id} name={peer} />
        <h2>{peer}</h2>
        {canSearch ? <button ref={searchButton} type="button" aria-expanded={searching} onClick={() => { setSearching((on) => !on); }}>Search</button> : null}
        {conversation.state === 'archived' ? <span className="hint">Archived</span> : null}
      </header>
      {state.access === 'unavailable' || blocked.has(conversation.peer_id) ? <p role="status" className="banner">{cannotMessage}</p> : null}
      {(!enabled || state.access === 'off') ? <p role="status" className="banner">Direct messages are off.</p> : null}
      {searching && canSearch ? <Search api={api} conversation={conversationId} viewer={viewer} name={peer} onClose={() => {
        setSearching(false);
        searchButton.current?.focus();
      }} /> : null}
      {historyAvailable ? (
      <div ref={log} role="log" aria-label="Messages" aria-live="polite" tabIndex={0} className="dm-log">
        {state.older === null ? null : (
          <button type="button" className="dm-older" aria-disabled={state.loadingOlder} onClick={(event) => {
            if (state.loadingOlder) return;
            pageOpener.current = document.activeElement === event.currentTarget ? event.currentTarget : null;
            void t.loadOlder();
          }}>
            {state.loadingOlder ? 'Loading earlier messages…' : 'Load earlier messages'}
          </button>
        )}
        {state.olderFailed ? <p role="alert" className="hint">Could not load earlier messages.</p> : null}
        {t.messages.length === 0 && t.pending.length === 0 ? <p className="empty">{`No messages yet. Say hello to ${peer}.`}</p> : null}
        <ol aria-label="Messages in order, oldest first">
          {t.messages.map((m) => (
            <MessageRow
              key={m.id}
              message={m}
              mine={m.author_id === viewer}
              author={m.author_id === viewer ? 'You' : peer}
              api={api}
              conversation={conversationId}
              canWrite={canWrite}
              onEdit={t.edit}
              onRemove={t.remove}
            />
          ))}
          {t.pending.map((p) => (
            <PendingRow key={p.nonce} pending={p} canWrite={canWrite} onRetry={() => void t.retry(p.nonce)} onDiscard={() => { t.discard(p.nonce); }} />
          ))}
        </ol>
      </div>
      ) : <p className="empty" role="status">Message history is unavailable on this server.</p>}
      {historyAvailable && canWrite ? (
        <DmComposer
          api={api}
          conversation={conversationId}
          name={peer}
          draft={text}
          onDraft={(next) => {
            setText(next);
            onDraft(conversationId, next);
          }}
          onSend={t.send}
        />
      ) : null}
    </section>
  );
}
