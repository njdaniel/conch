import { useEffect, useRef, useState, type SyntheticEvent } from 'react';
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

// "Rename" (issue #728): an operator or this channel's manager renames it from the
// channel list, one request per submit, however often the button is pressed.
// It sends the name the list shows now with the new one, so a rename someone
// else made meanwhile is refused rather than undone. Nothing changes on the
// page until the server has said yes; the channel's answer is handed to
// onRenamed. A refusal leaves the name as typed, to be corrected.
export function RenameChannel({
  channel,
  onRenamed,
  onStale,
  onUnauthorized,
}: {
  // The open channel, as the list holds it now.
  readonly channel: Pick<ChannelV0, 'id' | 'name'>;
  readonly onRenamed: (channel: ChannelV0) => void;
  // The server says the channel is no longer called what the list says, or
  // is not found: the list is read again, and this is what it is called now
  // (null when the list no longer holds it, or could not be read).
  readonly onStale: (id: number) => Promise<string | null>;
  readonly onUnauthorized: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState(channel.name);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  // The request in flight, held outside state: two submits in one tick both
  // see the state of the render before them, and the second must send nothing.
  const sending = useRef(false);
  const toggle = useRef<HTMLButtonElement>(null);
  // An answer that comes after the control is gone (another channel opened,
  // signed out) changes nothing here; the list still learns of a rename made.
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
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
        setOpen(false);
        setLine({ text: `Renamed to #${result.value.name}`, refused: false });
        // The form is hidden with focus in it: focus goes back to its button.
        toggle.current?.focus();
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
    <div className="rename-channel">
      <button
        ref={toggle}
        type="button"
        aria-expanded={open}
        aria-controls="rename-channel-form"
        onClick={() => {
          setOpen(!open);
          setValue(channel.name);
          setLine(null);
        }}
      >
        Rename #{channel.name}
      </button>
      <form id="rename-channel-form" aria-label="Rename channel" hidden={!open} onSubmit={submit}>
        <label htmlFor="rename-channel-field">New name</label>
        <input
          id="rename-channel-field"
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
      </form>
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
