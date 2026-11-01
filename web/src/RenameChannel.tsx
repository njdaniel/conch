import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { serverLine } from './Invite';
import type { ChannelV0 } from './schema/channel';

// What the server's refusal means to the person, by its code. A name is
// taken whoever's channel it is, so the one in the way may be one they are
// not in and cannot see.
const refusals: Readonly<Record<string, string>> = {
  channel_exists: 'There is already a channel with that name, or one that reads the same, perhaps one you are not in. Choose another name.',
  forbidden: "Only operators and this channel's managers rename it.",
  channel_not_found: 'That channel is gone, or you can no longer see it.',
};

// The rule a name must keep, for a refusal that does not say which part it
// broke.
const nameRule = 'That name cannot be used: it needs a letter you can see, at most 100 characters, and no spaces at either end or control characters.';

// staleLine is what the page says when someone else renamed the channel
// first: what it is called now, when the list read again says.
export function staleLine(now: string | null): string {
  return now === null
    ? 'The channel was renamed meanwhile; look again.'
    : `The channel was renamed to #${now} meanwhile; look again.`;
}

type Line = { readonly text: string; readonly refused: boolean };

type Refusal = { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string };

// refusalLine is the plain words for a rename the server did not make, but
// for a stale name, which needs the list read again first.
function refusalLine(result: Refusal): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === 'invalid_request') return serverLine(result.message, nameRule);
  return (result.code === undefined ? undefined : refusals[result.code]) ?? 'The server could not rename the channel. Try again.';
}

// "Rename channel…" (issues #728, #1135): an operator or this channel's manager
// renames it from the channel's ⋯ menu, in place under its row, one request per submit, however often the button is pressed.
// It sends the name the list shows now with the new one, so a rename someone
// else made meanwhile is refused rather than undone. Nothing changes on the
// page until the server has said yes; the channel's answer is handed to
// onRenamed. A refusal leaves the name as typed, to be corrected.
export function RenameChannel({
  channel,
  onRenamed,
  onStale,
  onUnauthorized,
  onClose,
}: {
  // The channel being renamed, as the list holds it now.
  readonly channel: Pick<ChannelV0, 'id' | 'name'>;
  // Escape, Cancel and a rename made close the form; the caller puts focus
  // back on the channel's ⋯ (issue #1135).
  readonly onClose: () => void;
  readonly onRenamed: (channel: ChannelV0) => void;
  // The server says the channel is no longer called what the list says, or
  // is not found: the list is read again, and this is what it is called now
  // (null when the list no longer holds it, or could not be read).
  readonly onStale: (id: number) => Promise<string | null>;
  readonly onUnauthorized: () => void;
}) {
  const [value, setValue] = useState(channel.name);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  // The request in flight, held outside state: two submits in one tick both
  // see the state of the render before them, and the second must send nothing.
  const sending = useRef(false);
  const field = useRef<HTMLInputElement>(null);
  // Several rows may each hold a form at once: each names its own field.
  const fieldId = useId();
  // An answer that comes after the control is gone (another channel opened,
  // signed out) changes nothing here; the list still learns of a rename made.
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    field.current?.focus();
    return () => {
      live.current = false;
    };
  }, []);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    if (sending.current || value.trim() === '') return;
    if (value === channel.name) {
      setLine({ text: 'That is already the channel’s name.', refused: true });
      return;
    }
    sending.current = true;
    setBusy(true);
    setLine(null);
    const id = channel.id;
    void api.renameChannel(id, channel.name, value).then(async (result) => {
      if (result.ok) {
        sending.current = false;
        onRenamed(result.value);
        if (!live.current) return;
        setBusy(false);
        onClose();
        return;
      }
      if (result.failure === 'unauthorized') {
        sending.current = false;
        onUnauthorized();
        return;
      }
      // Renamed by someone else first, or not found by that id: the list is
      // read again, and the page moves to what the channel is called now.
      const stale = result.code === 'channel_name_stale';
      const now = stale || result.code === api.channelNotFound ? await onStale(id) : null;
      sending.current = false;
      if (!live.current) return;
      setBusy(false);
      setLine({ text: stale ? staleLine(now) : refusalLine(result), refused: true });
    });
  }

  return (
    <div
      className="rename-channel"
      onKeyDown={(event) => {
        if (event.key !== 'Escape' || event.nativeEvent.isComposing || event.defaultPrevented) return;
        event.preventDefault();
        event.stopPropagation();
        onClose();
      }}
    >
      <form aria-label="Rename channel" onSubmit={submit}>
        <label htmlFor={fieldId}>New name</label>
        <input
          ref={field}
          id={fieldId}
          type="text"
          autoComplete="off"
          value={value}
          readOnly={busy}
          onChange={(e) => {
            setValue(e.target.value);
          }}
        />
        <button type="submit" disabled={busy || value.trim() === ''}>
          Rename channel
        </button>
        <button type="button" onClick={onClose}>
          Cancel
        </button>
      </form>
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
