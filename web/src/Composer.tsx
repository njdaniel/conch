import { createContext, useContext, useEffect, useLayoutEffect, useRef, useState, type DragEvent, type KeyboardEvent, type ReactNode } from 'react';
import { EmojiPicker } from './EmojiPicker';
import { completeShortcode, completionAt, insertAt, loadEmoji, type Emoji, type EmojiIndex } from './emoji/emoji';
import { Glyph } from './Icons';
import { formatSize, isPreviewType } from './fileSize';
import { validateImage } from './image';
import type { Audience } from './schema/message';
import { audienceScope, maxDraftAttachments, type DraftAttachment, type NewDraftAttachment } from './state/attachments';
import type { Target, TargetAction } from './state/drafts';
import type { Person } from './useLiveChannel';

// The text the TUI shows after the first whisper.
export const whisperNotice = 'note: whispers are recorded in the audit log';

// What the composer says while the connection is being made again.
export const pausedText = 'Sending is paused until the server is back. Your draft is kept.';
const pausedId = 'composer-paused';

const channelValue = 'channel';
const whisperValue = 'whisper';
const netValue = (id: number) => `net:${String(id)}`;

export interface ComposerAttachmentContextValue {
  readonly attachments: readonly DraftAttachment[];
  readonly add: (files: readonly NewDraftAttachment[]) => void;
  readonly remove: (clientId: number) => void;
  readonly cancel: (clientId: number) => void;
  readonly retry: (clientId: number) => void;
  // The draft's images are about to change (issue #1066): said before any
  // image is checked, so a send already on its way can no longer take this
  // draft for its own.
  readonly editing: () => void;
}

const noAttachments: ComposerAttachmentContextValue = {
  attachments: [],
  add: () => undefined,
  remove: () => undefined,
  cancel: () => undefined,
  retry: () => undefined,
  editing: () => undefined,
};
const ComposerAttachmentContext = createContext<ComposerAttachmentContextValue>(noAttachments);

// App owns the bytes and upload lifecycle. The context keeps Conversation's
// existing public props unchanged while Composer remains its child.
export function ComposerAttachmentProvider({ value, children }: { readonly value: ComposerAttachmentContextValue; readonly children: ReactNode }) {
  return <ComposerAttachmentContext.Provider value={value}>{children}</ComposerAttachmentContext.Provider>;
}

function DraftPreview({ attachment, busy, onRemove, onCancel, onRetry }: {
  readonly attachment: DraftAttachment;
  readonly busy: boolean;
  readonly onRemove: () => void;
  readonly onCancel: () => void;
  readonly onRetry: () => void;
}) {
  const [url, setUrl] = useState<string | null>(null);
  const [broken, setBroken] = useState(false);
  const image = isPreviewType(attachment.mediaType);
  useEffect(() => {
    if (!image) return;
    const made = URL.createObjectURL(attachment.bytes);
    setUrl(made);
    return () => {
      URL.revokeObjectURL(made);
    };
  }, [attachment.bytes, image]);

  return (
    <li className="draft-attachment">
      {!image ? <span className="draft-image-fallback" aria-hidden="true">File</span> : url !== null && !broken ? <img src={url} alt="" onError={() => { setBroken(true); }} /> : <span className="draft-image-fallback" aria-hidden="true">Image</span>}
      <span className="draft-attachment-name">{attachment.filename}</span>
      {image ? null : <span className="hint draft-attachment-size">{formatSize(attachment.size)}</span>}
      {attachment.status === 'uploading' ? (
        <>
          <progress aria-label={`Uploading ${attachment.filename}`} />
          <span className="hint">Uploading…</span>
          <button type="button" onClick={onCancel} aria-label={`Cancel upload of ${attachment.filename}`}>Cancel</button>
        </>
      ) : null}
      {attachment.status === 'failed' || attachment.status === 'cancelled' ? (
        <>
          {attachment.status === 'failed' ? <span role="alert" className="draft-attachment-error">{attachment.error ?? 'Upload failed. Try again.'}</span> : <span className="hint">Upload cancelled.</span>}
          <button type="button" onClick={onRetry} aria-label={`Retry upload of ${attachment.filename}`}>Retry</button>
        </>
      ) : null}
      <button type="button" onClick={onRemove} disabled={busy} aria-label={`Remove ${attachment.filename}`}>Remove</button>
    </li>
  );
}

// The composer: who the message goes to, the box to write in, and Send. The
// audience is the server's to allow; this offers what the viewer can see.
export function Composer({
  channel,
  target,
  nets,
  monitored,
  people,
  viewer,
  draft,
  enabled,
  paused,
  onDraft,
  onTarget,
  onSend,
  atBottom,
}: {
  readonly channel: string;
  readonly target: Target;
  readonly nets: ReadonlyMap<number, string>;
  readonly monitored: ReadonlySet<number>;
  readonly people: ReadonlyMap<number, Person>;
  readonly viewer: number;
  readonly draft: string;
  // Whether the connection is open; the composer is read-only otherwise.
  readonly enabled: boolean;
  // Whether the connection was open and is being made again: the composer
  // says why it is read-only (UX-05, UX-07). Not at the first load.
  readonly paused: boolean;
  readonly onDraft: (text: string) => void;
  readonly onTarget: (action: TargetAction) => void;
  readonly onSend: (body: string, audience: Audience | undefined, attachments?: readonly DraftAttachment[]) => void;
  // Called when a send starts, so the log follows the new message.
  readonly atBottom: () => void;
}) {
  const select = useRef<HTMLSelectElement>(null);
  const picker = useRef<HTMLInputElement>(null);
  const textarea = useRef<HTMLTextAreaElement>(null);
  const emojiButton = useRef<HTMLButtonElement>(null);
  const [emojiOpen, setEmojiOpen] = useState(false);
  const [caret, setCaret] = useState(0);
  const [suggestion, setSuggestion] = useState(0);
  const [dismissed, setDismissed] = useState<string | null>(null);
  const [emojiIndex, setEmojiIndex] = useState<EmojiIndex | null>(null);
  // Where the caret goes once a chosen emoji is in the draft.
  const caretAfter = useRef<number | null>(null);
  // Counted on each insertion, so the caret and focus are put back even when
  // the text did not change (an emoji replaced by itself).
  const [insertions, setInsertions] = useState(0);
  const fileDraft = useContext(ComposerAttachmentContext);
  const attachments = fileDraft.attachments;
  const [inputError, setInputError] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const validationGeneration = useRef(0);
  const whispering = target.whisper !== null;
  const chosen = target.whisper ?? [];

  // Nets the viewer may transmit to. The one chosen stays on the list even if
  // it is not (yet) known, so a send never quietly widens to the channel.
  const choices = [...nets].filter(([id]) => !monitored.has(id) || id === target.net);
  if (target.net !== null && !nets.has(target.net)) choices.push([target.net, `#${String(target.net)}`]);

  const others = [...people].filter(([id]) => id !== viewer).sort((a, b) => a[1].name.localeCompare(b[1].name));
  const names = chosen.map((id) => people.get(id)?.name ?? `#${String(id)}`);
  const netName = target.net === null ? '' : (nets.get(target.net) ?? `#${String(target.net)}`);

  const label = whispering
    ? names.length > 0
      ? `Whisper to ${names.join(', ')}`
      : 'Whisper to…'
    : target.net !== null
      ? `Message net ${netName}`
      : `Message #${channel}`;

  const value = whispering ? whisperValue : target.net !== null ? netValue(target.net) : channelValue;

  function audience(): Audience | undefined {
    if (whispering) return { kind: 'principals', principal_ids: [...chosen] };
    if (target.net !== null) return { kind: 'net', net_id: target.net };
    return undefined;
  }

  const scope = `${channel}/${audienceScope(audience())}`;
  const scopeNow = useRef(scope);
  scopeNow.current = scope;
  useEffect(() => {
    setInputError(null);
    setSending(false);
  }, [scope]);
  useEffect(() => {
    if (attachments.length === 0 || attachments.some((a) => a.status === 'failed' || a.status === 'cancelled')) setSending(false);
  }, [attachments]);
  useEffect(() => () => {
    validationGeneration.current++;
  }, []);

  const uploading = attachments.some((a) => a.status === 'uploading');
  const busy = sending || uploading;
  // The draft may be edited: the box is not read-only. An emoji is an edit, so
  // neither the picker nor the suggestions work (or stay open) when it is.
  const editable = enabled && !busy;
  useEffect(() => {
    if (!editable) setEmojiOpen(false);
  }, [editable]);

  // `:short` at the caret opens the suggestions. The catalog is fetched only
  // once something has been typed that could use it.
  const completion = editable && !emojiOpen ? completionAt(draft, caret) : null;
  const completionKey = completion === null ? null : `${String(completion.start)}:${completion.query}`;
  useEffect(() => {
    if (completionKey === null || emojiIndex !== null) return;
    let live = true;
    loadEmoji().then((index) => { if (live) setEmojiIndex(index); }, () => undefined);
    return () => { live = false; };
  }, [completionKey, emojiIndex]);
  const suggestions: readonly Emoji[] = completion !== null && emojiIndex !== null ? completeShortcode(emojiIndex, completion.query) : [];
  const suggesting = completion !== null && suggestions.length > 0 && dismissed !== completionKey;
  const activeSuggestion = Math.min(suggestion, Math.max(0, suggestions.length - 1));
  const optionId = (i: number) => `composer-emoji-option-${String(i)}`;
  useLayoutEffect(() => {
    const at = caretAfter.current;
    const box = textarea.current;
    if (at === null || box === null) return;
    caretAfter.current = null;
    box.focus();
    box.setSelectionRange(at, at);
    setCaret(at);
  }, [insertions]);

  // Put an emoji where the selection is (or in place of the typed `:short`).
  function insertEmoji(key: string, from: number, to: number): void {
    if (!editable) return;
    const done = insertAt(draft, from, to, key);
    caretAfter.current = done.caret;
    setSuggestion(0);
    onDraft(done.text);
    setInsertions((n) => n + 1);
  }
  function pickEmoji(emoji: Emoji): void {
    const box = textarea.current;
    setEmojiOpen(false);
    insertEmoji(emoji.key, box?.selectionStart ?? draft.length, box?.selectionEnd ?? draft.length);
  }
  function closePicker(): void {
    setEmojiOpen(false);
    emojiButton.current?.focus();
  }

  const canSend = enabled && !busy && (draft.trim() !== '' || attachments.length > 0) && (!whispering || chosen.length > 0);

  function submit() {
    if (!canSend) return;
    atBottom();
    const body = draft.trim() === '' && attachments.length > 0 ? attachments.map((a) => a.filename).join(', ') : draft;
    if (attachments.length > 0) setSending(true);
    onSend(body, audience(), attachments);
  }

  function key(event: KeyboardEvent) {
    // Enter sends, Shift+Enter adds a line; Enter while composing (an input
    // method choosing a character) is not a send.
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    submit();
  }

  function choose(next: string) {
    if (next === channelValue) onTarget({ type: 'channel', channel });
    else if (next === whisperValue) onTarget({ type: 'whisper', channel });
    else {
      const id = Number(next.slice('net:'.length));
      if (Number.isInteger(id)) onTarget({ type: 'net', channel, net: id });
    }
  }

  async function addFiles(incoming: readonly File[]): Promise<void> {
    if (!enabled || busy || incoming.length === 0) return;
    fileDraft.editing();
    const generation = ++validationGeneration.current;
    const frozenScope = scope;
    const slots = Math.max(0, maxDraftAttachments - attachments.length);
    const accepted = incoming.slice(0, slots);
    const countError = incoming.length > slots ? `Only ${String(maxDraftAttachments)} files can be attached to one message.` : null;
    if (accepted.length === 0) {
      setInputError(countError);
      return;
    }
    const checked = await Promise.all(accepted.map(async (file) => ({ file, result: await validateImage(file) })));
    if (generation !== validationGeneration.current || scopeNow.current !== frozenScope) return;
    // A PNG or JPEG that passes the sniff is an image, with a preview. Any
    // other file, and an image that fails the sniff, goes as a plain file.
    const valid: NewDraftAttachment[] = checked.map(({ file, result }) => {
      if (result.ok) {
        return {
          filename: file.name === '' ? `image.${result.value.format === 'png' ? 'png' : 'jpg'}` : file.name,
          mediaType: result.value.format === 'png' ? 'image/png' : 'image/jpeg',
          bytes: file,
        };
      }
      return {
        filename: file.name === '' ? 'file' : file.name,
        mediaType: isPreviewType(file.type) || file.type === '' ? 'application/octet-stream' : file.type,
        bytes: file,
      };
    });
    const errors: string[] = countError === null ? [] : [countError];
    if (valid.length > 0) fileDraft.add(valid);
    setInputError(errors.length === 0 ? null : errors.join(' '));
  }

  function paste(event: React.ClipboardEvent<HTMLTextAreaElement>): void {
    const files = Array.from(event.clipboardData.files);
    if (files.length === 0) return;
    event.preventDefault();
    void addFiles(files);
  }

  function drop(event: DragEvent<HTMLFormElement>): void {
    const files = Array.from(event.dataTransfer.files);
    if (files.length === 0 || !enabled) return;
    event.preventDefault();
    void addFiles(files);
  }

  return (
    <form
      className={value === channelValue ? 'composer' : 'composer narrowed'}
      onDragOver={(event) => {
        if (enabled && event.dataTransfer.types.includes('Files')) event.preventDefault();
      }}
      onDrop={drop}
      onSubmit={(event) => {
        event.preventDefault();
        submit();
      }}
    >
      <div className="audience-row">
        <label htmlFor="composer-to">To:</label>{' '}
        <select
          id="composer-to"
          ref={select}
          value={value}
          disabled={!enabled || busy}
          aria-describedby={paused ? pausedId : undefined}
          onChange={(event) => {
            choose(event.target.value);
          }}
        >
          <option value={channelValue}>#{channel}</option>
          {choices.map(([id, name]) => (
            <option key={id} value={netValue(id)}>
              net {name}
            </option>
          ))}
          <option value={whisperValue}>Whisper to…</option>
        </select>
        <label htmlFor="composer-text" className="composer-label">
          {label}
        </label>
      </div>
      {whispering ? (
        <fieldset
          className="whisper-picker"
          disabled={busy}
          onKeyDown={(event) => {
            if (event.key !== 'Escape') return;
            event.preventDefault();
            event.stopPropagation();
            onTarget({ type: 'closePicker', channel });
            select.current?.focus();
          }}
        >
          <legend>Whisper to</legend>
          {others.length === 0 ? <p className="hint">Nobody else in this channel to whisper to</p> : null}
          {others.map(([id, person]) => (
            <label key={id} className="whisper-person">
              <input
                type="checkbox"
                checked={chosen.includes(id)}
                onChange={() => {
                  onTarget({ type: 'toggle', channel, principal: id });
                }}
              />{' '}
              {person.name}
              {person.kind === 'agent' ? ' [agent]' : ''}
            </label>
          ))}
        </fieldset>
      ) : null}
      {attachments.length > 0 ? (
        <ul className="draft-attachments" aria-label="Files to send" aria-busy={uploading}>
          {attachments.map((attachment) => (
            <DraftPreview
              key={attachment.clientId}
              attachment={attachment}
              busy={busy}
              onRemove={() => { fileDraft.remove(attachment.clientId); }}
              onCancel={() => { fileDraft.cancel(attachment.clientId); }}
              onRetry={() => { fileDraft.retry(attachment.clientId); }}
            />
          ))}
        </ul>
      ) : null}
      {inputError === null ? null : <p role="alert" className="notice">{inputError}</p>}
      {emojiOpen && editable ? <EmojiPicker onPick={pickEmoji} onClose={closePicker} /> : null}
      {suggesting ? (
        <ul id="composer-emoji-list" className="emoji-suggestions" role="listbox" aria-label="Emoji suggestions">
          {suggestions.map((e, i) => (
            <li
              key={e.key}
              id={optionId(i)}
              role="option"
              aria-selected={i === activeSuggestion}
              onMouseDown={(event) => { event.preventDefault(); }}
              onClick={() => { insertEmoji(e.key, completion.start, caret); }}
            >
              <span aria-hidden="true">{e.key}</span> :{e.shortcode}:
            </li>
          ))}
        </ul>
      ) : null}
      <div className={paused ? 'composer-box paused' : 'composer-box'}>
      <input
        ref={picker}
        type="file"
        multiple
        className="image-file-input"
        aria-label="Choose files"
        tabIndex={-1}
        onChange={(event) => {
          const input = event.currentTarget;
          const files = Array.from(input.files ?? []);
          void addFiles(files).finally(() => {
            input.value = '';
          });
        }}
      />
      <button type="button" className="attach-image" aria-label="Attach" disabled={!enabled || busy} onClick={() => { picker.current?.click(); }}>
        <Glyph name="image" />
        <span className="label">Attach</span>
      </button>
      <textarea
        id="composer-text"
        ref={textarea}
        rows={2}
        value={draft}
        disabled={!enabled}
        readOnly={busy}
        aria-describedby={paused ? pausedId : undefined}
        aria-autocomplete="list"
        aria-controls={suggesting ? 'composer-emoji-list' : undefined}
        aria-activedescendant={suggesting ? optionId(activeSuggestion) : undefined}
        onChange={(event) => {
          setCaret(event.target.selectionStart);
          setSuggestion(0);
          onDraft(event.target.value);
        }}
        onSelect={(event) => { setCaret(event.currentTarget.selectionStart); }}
        onPaste={paste}
        onKeyDown={(event) => {
          if (suggesting && !event.nativeEvent.isComposing) {
            const n = suggestions.length;
            if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
              event.preventDefault();
              setSuggestion((activeSuggestion + (event.key === 'ArrowDown' ? 1 : n - 1)) % n);
              return;
            }
            if ((event.key === 'Enter' || event.key === 'Tab') && !event.shiftKey && !event.ctrlKey && !event.altKey && !event.metaKey) {
              event.preventDefault();
              const chosen = suggestions[activeSuggestion];
              if (chosen !== undefined) insertEmoji(chosen.key, completion.start, caret);
              return;
            }
            if (event.key === 'Escape') {
              event.preventDefault();
              setDismissed(completionKey);
              return;
            }
          }
          if (event.key === 'Escape' && whispering) {
            event.preventDefault();
            onTarget({ type: 'closePicker', channel });
            select.current?.focus();
            return;
          }
          key(event);
        }}
      />
      <button
        ref={emojiButton}
        type="button"
        className="attach-image emoji-trigger"
        aria-label="Add emoji"
        aria-haspopup="dialog"
        aria-expanded={emojiOpen && editable}
        disabled={!enabled || busy}
        onClick={() => { setEmojiOpen(!emojiOpen); }}
      >
        <Glyph name="smile" />
        <span className="label">Add emoji</span>
      </button>
      <button type="submit" className="primary send" disabled={!canSend}>
        <Glyph name="send" />
        <span className="label">Send</span>
      </button>
      </div>
      {paused ? (
        <p id={pausedId} className="hint">
          {pausedText}
        </p>
      ) : null}
      {target.refusal === null ? null : <p role="alert" className="notice">{target.refusal}</p>}
      {target.note === null ? null : <p role="status" className="hint">{target.note}</p>}
    </form>
  );
}
