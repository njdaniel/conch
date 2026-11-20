// The DM composer: text and files, with the rules of the channel composer that
// apply (a message of only whitespace is refused, Enter sends and Shift+Enter
// breaks the line) and none that do not (no mentions, whispers, threads or
// typed payloads: a DM carries text and files alone). A draft is kept per
// conversation by the provider, so switching away and back keeps it.
import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import { formatSize } from '../fileSize';
import type { DMFileV1 } from '../schema/generated';
import type { DmApi } from './api';

export function DmComposer({ api, conversation, name, draft, onDraft, onSend }: {
  readonly api: DmApi;
  readonly conversation: number;
  readonly name: string;
  readonly draft: string;
  readonly onDraft: (text: string) => void;
  // Called with the text and the files attached; the transcript keeps the send
  // pending, with Retry, so the composer clears at once.
  readonly onSend: (body: string, fileIds: readonly number[]) => void;
}) {
  const [attached, setAttached] = useState<readonly DMFileV1[]>([]);
  const [uploading, setUploading] = useState(false);
  const [problem, setProblem] = useState('');
  const picker = useRef<HTMLInputElement>(null);
  const abort = useRef<AbortController | null>(null);

  // Another conversation starts with no files, and an upload still going for
  // the last one is abandoned.
  useEffect(() => {
    setAttached([]);
    setProblem('');
    setUploading(false);
    return () => {
      abort.current?.abort();
      abort.current = null;
    };
  }, [conversation]);

  const canSend = !uploading && (draft.trim() !== '' || attached.length > 0);

  function send() {
    if (!canSend) return;
    const body = draft.trim() === '' ? attached.map((f) => f.filename).join(', ') : draft;
    onSend(body, attached.map((f) => f.id));
    onDraft('');
    setAttached([]);
  }

  function keyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key !== 'Enter' || event.shiftKey || event.nativeEvent.isComposing) return;
    event.preventDefault();
    send();
  }

  async function attach(file: File) {
    abort.current?.abort();
    const controller = new AbortController();
    abort.current = controller;
    setUploading(true);
    setProblem('');
    const result = await api.uploadFile(conversation, file.name, file.type === '' ? 'application/octet-stream' : file.type, file, controller.signal);
    if (controller.signal.aborted) return;
    setUploading(false);
    if (result.ok) setAttached((held) => [...held, result.value]);
    else setProblem(`Could not attach ${file.name}.`);
  }

  return (
    <form
      className="composer dm-composer"
      onSubmit={(event) => {
        event.preventDefault();
        send();
      }}
    >
      {attached.length === 0 ? null : (
        <ul aria-label="Attached files">
          {attached.map((f) => (
            <li key={f.id}>
              {f.filename} <span className="hint">{formatSize(f.size)}</span>{' '}
              <button type="button" aria-label={`Remove ${f.filename}`} onClick={() => { setAttached((held) => held.filter((x) => x.id !== f.id)); }}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      <label>
        {`Message to ${name}`}
        <textarea
          rows={2}
          value={draft}
          onChange={(event) => { onDraft(event.target.value); }}
          onKeyDown={keyDown}
        />
      </label>
      <div className="composer-actions">
        <label className="attach">
          Attach a file
          <input
            ref={picker}
            type="file"
            onChange={(event) => {
              const file = event.target.files?.[0];
              if (picker.current !== null) picker.current.value = '';
              if (file !== undefined) void attach(file);
            }}
          />
        </label>
        <button type="submit" disabled={!canSend}>Send</button>
      </div>
      {uploading ? <p role="status" className="hint">Uploading…</p> : null}
      {problem === '' ? null : <p role="alert" className="hint">{problem}</p>}
    </form>
  );
}
