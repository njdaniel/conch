// Editing one's own recent text (issue #845, docs/design/message-editing.md
// §13): which messages offer Edit, the inline editor, the "(edited)" marker
// and the author's Earlier versions. The server is the authority on every
// one of these: the page hiding Edit is a convenience, and each refusal has
// its own line.
import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import * as api from './api';
import { Linkified } from './Linkified';
import { fileAttachmentPayloadSchema } from './schema/file';
import { MAX_MESSAGE_EDITS, MESSAGE_EDIT_WINDOW_SECONDS, type MessageRevisionV1, type MessageV2 } from './schema/message';
import type { PrincipalKind } from './schema/principal';

export const editWindowMs = MESSAGE_EDIT_WINDOW_SECONDS * 1000;

// canEdit says whether the page offers Edit on a message: the viewer's own,
// the viewer a person, no payload or a file attachment (whose files stay as
// they are), and within the window by the page's clock. A pending or failed
// send is not a message yet and is never offered it.
export function canEdit(message: MessageV2, viewer: number, kind: PrincipalKind | null | undefined, now: number): boolean {
  if (message.author_id !== viewer || kind !== 'human') return false;
  if (message.payload !== undefined && message.payload.schema !== fileAttachmentPayloadSchema) return false;
  return now - Date.parse(message.created_at) <= editWindowMs;
}

// editsLeft says whether the page still counts an edit available: fewer than
// MAX_MESSAGE_EDITS known (issue #1031), and no refusal has ended the message.
// A page opened after the edits cannot know them: the server answers then.
export function editsLeft(known: number, final: boolean): boolean {
  return !final && known < MAX_MESSAGE_EDITS;
}

// editEnds is when the window of a message closes, in ms since the epoch.
export function editEnds(message: MessageV2): number {
  return Date.parse(message.created_at) + editWindowMs;
}

function hhmm(d: Date): string {
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

// when is a time as the marker and the versions say it: the local time, and
// the date too when it is not today.
export function when(at: string, now: Date = new Date()): string {
  const d = new Date(at);
  const today = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  return today ? hhmm(d) : `${d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' })} ${hhmm(d)}`;
}

// The marker after an edited message's text. For the author it is a button
// that opens Earlier versions; for every other reader plain text with the
// same name. It is the same live, after a reload, in a thread and in search.
export function EditedMarker({
  message,
  channel,
  viewer,
  onUnauthorized,
}: {
  readonly message: MessageV2;
  readonly channel: string;
  readonly viewer: number;
  readonly onUnauthorized?: (() => void) | undefined;
}) {
  const [open, setOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const at = message.edited_at;
  if (at === undefined) return null;
  const name = `Edited at ${when(at)}`;
  if (message.author_id !== viewer) {
    return (
      <>
        {' '}
        <time className="edited-marker" dateTime={at} title={name} aria-label={name}>
          (edited)
        </time>
      </>
    );
  }
  return (
    <>
      {' '}
      <button
        ref={button}
        type="button"
        className="edited-marker"
        aria-label={name}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => {
          setOpen(true);
        }}
      >
        <time className="edited" dateTime={at} title={name}>
          (edited)
        </time>
      </button>
      {open ? (
        <EarlierVersions
          channel={channel}
          messageId={message.id}
          onUnauthorized={onUnauthorized}
          onClose={() => {
            setOpen(false);
            button.current?.focus();
          }}
        />
      ) : null}
    </>
  );
}

// Earlier versions: fetched when opened, never before; newest first, the
// current one marked. Only the author is ever answered.
export function EarlierVersions({
  channel,
  messageId,
  onClose,
  onUnauthorized,
}: {
  readonly channel: string;
  readonly messageId: number;
  readonly onClose: () => void;
  readonly onUnauthorized?: (() => void) | undefined;
}) {
  const [revisions, setRevisions] = useState<readonly MessageRevisionV1[] | null>(null);
  const [failed, setFailed] = useState(false);
  const [attempt, setAttempt] = useState(0);
  const dialog = useRef<HTMLDivElement>(null);
  const close = useRef<HTMLButtonElement>(null);
  const title = useId();
  const closeNow = useRef(onClose);
  const unauthorizedNow = useRef(onUnauthorized);
  closeNow.current = onClose;
  unauthorizedNow.current = onUnauthorized;

  useEffect(() => {
    close.current?.focus();
  }, []);

  useEffect(() => {
    let live = true;
    setFailed(false);
    void api.messageRevisions(channel, messageId).then((result) => {
      if (!live) return;
      if (result.ok) {
        setRevisions(result.value);
        return;
      }
      if (result.failure === 'unauthorized') {
        closeNow.current();
        unauthorizedNow.current?.();
        return;
      }
      // Not the author's any more, or gone: there is nothing to show.
      if (result.code === 'message_not_found' || result.code === 'channel_not_found') {
        closeNow.current();
        return;
      }
      setFailed(true);
    });
    return () => {
      live = false;
    };
  }, [channel, messageId, attempt]);

  function key(event: KeyboardEvent<HTMLDivElement>) {
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      onClose();
      return;
    }
    if (event.key !== 'Tab') return;
    const controls = dialog.current?.querySelectorAll<HTMLElement>('button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])');
    if (controls === undefined || controls.length === 0) return;
    const first = controls[0];
    const last = controls[controls.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last?.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first?.focus();
    }
  }

  const newestFirst = revisions === null ? [] : [...revisions].reverse();
  const current = revisions === null ? 0 : Math.max(...revisions.map((r) => r.revision));
  return (
    <div className="versions-layer">
      <div className="versions-backdrop" onClick={onClose} />
      <div ref={dialog} className="versions-dialog" role="dialog" aria-modal="true" aria-labelledby={title} onKeyDown={key}>
        <h2 id={title}>Earlier versions</h2>
        <p className="hint">Only you can see earlier versions. They are kept on the server.</p>
        {revisions === null && !failed ? <p role="status">Loading earlier versions…</p> : null}
        {failed ? (
          <div role="alert" className="banner problem">
            <p>Couldn&apos;t load the earlier versions.</p>
            <button
              type="button"
              onClick={() => {
                setAttempt((n) => n + 1);
              }}
            >
              Try again
            </button>
          </div>
        ) : null}
        {revisions === null ? null : (
          <ol className="versions" aria-label="Versions, newest first">
            {newestFirst.map((revision) => (
              <li key={revision.revision} className={revision.revision === current ? 'version current' : 'version'}>
                <div className="version-head">
                  <time dateTime={revision.created_at}>{when(revision.created_at)}</time>
                  {revision.revision === current ? <span className="version-current"> Current</span> : null}
                </div>
                <p className="version-body"><Linkified text={revision.body} /></p>
              </li>
            ))}
          </ol>
        )}
        <div className="actions">
          <button ref={close} type="button" onClick={onClose}>
            Close
          </button>
        </div>
      </div>
    </div>
  );
}

// The line for each refusal of an edit (§13.2), and whether it ends the
// editor (the message is gone from the person) or leaves Save of no use.
type Refusal = { readonly line: string; readonly gone?: true; readonly final?: true; readonly wait?: number };

const limitLine = 'This message has been edited the most times allowed.';

// atLimit: the page knows MAX_MESSAGE_EDITS edits of the message, so a 429
// is the burst spent on a message that cannot be edited again anyway.
function refusalOf(result: Exclude<api.EditResult, { readonly ok: true }>, atLimit: boolean): Refusal {
  if (result.failure !== 'refused') return { line: "Couldn't save. Your text is kept." };
  switch (result.code) {
    case 'message_edit_window_closed':
      return { line: 'This message can no longer be edited: it is more than an hour old.', final: true };
    case 'message_edit_limit_reached':
      return { line: limitLine, final: true };
    case 'message_not_editable':
      return { line: "This message can't be edited.", final: true };
    case 'message_not_found':
    case 'channel_not_found':
      return { line: 'This message is no longer available to you.', gone: true };
    case 'rate_limited': {
      if (atLimit) return { line: limitLine, final: true };
      const wait = Math.max(1, result.retryAfter ?? 1);
      return { line: `Too many edits. Try again in ${String(wait)} ${wait === 1 ? 'second' : 'seconds'}.`, wait };
    }
    default:
      return { line: "Couldn't save. Your text is kept." };
  }
}

// The inline editor: the message's text in a box, focused with the caret at
// the end, Save and Cancel under it. Enter saves, Shift+Enter adds a line, and
// Enter while an input method is composing does nothing (the composer's rule);
// Escape cancels. The person's text is theirs until they save or cancel: a
// live edit of the same message from another tab changes the message under
// it, not the box.
export function MessageEditor({
  message,
  channel,
  text,
  onText,
  onCancel,
  onSaved,
  onGone,
  onFinal,
  atLimit = false,
  onUnauthorized,
  discard,
}: {
  // The message as held now: its body is what Save compares with.
  readonly message: MessageV2;
  readonly channel: string;
  readonly text: string;
  readonly onText: (text: string) => void;
  readonly onCancel: () => void;
  readonly onSaved: (message: MessageV2) => void;
  // The message is no longer the person's to edit: the editor closes and the
  // line is shown under the message.
  readonly onGone: (line: string) => void;
  // A refusal ended the message's edits for good: the page stops offering Edit.
  readonly onFinal?: (() => void) | undefined;
  // Ten edits are known already (issue #1031).
  readonly atLimit?: boolean;
  readonly onUnauthorized?: (() => void) | undefined;
  // Asked when another message's Edit was chosen with unsaved text here.
  readonly discard?: { readonly onDiscard: () => void; readonly onKeep: () => void } | undefined;
}): ReactNode {
  const box = useRef<HTMLTextAreaElement>(null);
  const discardButton = useRef<HTMLButtonElement>(null);
  const [saving, setSaving] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [waiting, setWaiting] = useState(false);
  // atLimit as of now: a save in flight classifies its answer by this, not by
  // the value it started with.
  const limited = useRef(atLimit);
  limited.current = atLimit;
  const hint = useId();
  const error = useId();
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  useEffect(() => {
    const el = box.current;
    if (el === null) return;
    el.focus();
    el.setSelectionRange(el.value.length, el.value.length);
  }, []);

  // A rate refusal showing when the tenth edit becomes known is the limit.
  const finalNow = useRef(onFinal);
  finalNow.current = onFinal;
  useEffect(() => {
    if (!atLimit || refusal?.wait === undefined) return;
    setRefusal({ line: limitLine, final: true });
    finalNow.current?.();
  }, [atLimit, refusal]);

  // A rate limit holds Save back for as long as the server said.
  const wait = refusal?.wait;
  useEffect(() => {
    if (wait === undefined) return;
    setWaiting(true);
    const handle = window.setTimeout(() => {
      setWaiting(false);
    }, wait * 1000);
    return () => {
      window.clearTimeout(handle);
    };
  }, [wait, refusal]);

  // The question takes focus while it is asked; Keep editing gives it back.
  const asking = discard !== undefined;
  const asked = useRef(false);
  useEffect(() => {
    if (asking) discardButton.current?.focus();
    else if (asked.current) box.current?.focus();
    asked.current = asking;
  }, [asking]);

  const canSave = !saving && !waiting && refusal?.final !== true && text.trim() !== '' && text !== message.body;

  async function save() {
    if (!canSave) return;
    setSaving(true);
    const result = await api.editMessage(channel, message.id, text);
    if (!live.current) return;
    setSaving(false);
    if (result.ok) {
      onSaved(result.message);
      return;
    }
    if (result.failure === 'unauthorized') {
      onUnauthorized?.();
      return;
    }
    const next = refusalOf(result, limited.current);
    if (next.gone === true) {
      onGone(next.line);
      return;
    }
    if (next.final === true) onFinal?.();
    setRefusal(next);
  }

  function key(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.nativeEvent.isComposing) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      onCancel();
      return;
    }
    if (event.key !== 'Enter' || event.shiftKey) return;
    event.preventDefault();
    void save();
  }

  return (
    <div className="message-editor">
      <textarea
        ref={box}
        aria-label={`Edit message ${String(message.id)}`}
        aria-describedby={refusal === null ? hint : `${hint} ${error}`}
        rows={Math.min(8, Math.max(2, text.split('\n').length))}
        value={text}
        readOnly={saving}
        onChange={(event) => {
          onText(event.target.value);
        }}
        onKeyDown={key}
      />
      <p id={hint} className="hint">
        Editing. Enter to save, Esc to cancel
      </p>
      {refusal === null ? null : (
        <p id={error} role="alert" className="edit-error">
          {refusal.line}
        </p>
      )}
      {discard === undefined ? null : (
        <div role="alertdialog" aria-label="Discard your edit?" className="edit-discard">
          <p>Discard your edit?</p>
          <button ref={discardButton} type="button" onClick={discard.onDiscard}>
            Discard
          </button>{' '}
          <button type="button" onClick={discard.onKeep}>
            Keep editing
          </button>
        </div>
      )}
      <div className="editor-actions">
        <button
          type="button"
          className="primary"
          disabled={!canSave}
          onClick={() => {
            void save();
          }}
        >
          {saving ? 'Saving…' : 'Save'}
        </button>
        <button type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );
}
