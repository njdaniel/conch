import { useEffect, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { serverLine } from './Invite';
import type { ChannelV0 } from './schema/channel';

// What the server's refusal means to the person, by its code. A name is taken
// whoever's channel it is, so the one in the way may be one they are not in
// and cannot see.
const refusals: Readonly<Record<string, string>> = {
  channel_exists: 'There is already a channel with that name, perhaps one you are not in. Choose another name.',
  forbidden: 'Only an operator can create channels.',
};

// The rule a name must keep, for a refusal that does not say which part it
// broke.
const nameRule = 'That name cannot be used: it needs a letter you can see, at most 100 characters, and no spaces at either end or control characters.';

type Line = { readonly text: string; readonly refused: boolean };

// refusalLine is the plain words for a channel the server did not make.
function refusalLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly message?: string }): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === 'invalid_request') return serverLine(result.message, nameRule);
  return (result.code === undefined ? undefined : refusals[result.code]) ?? 'The server could not create the channel. Try again.';
}

// "New channel" (issue #695): an operator makes a channel from the channel
// list, one request per submit, however often the button is pressed. The new
// channel is the server's answer, handed to onCreated; nothing changes in the
// list until the server has said yes. A refusal leaves the name as typed, to
// be corrected. Making a channel only makes it: it does not join its voice.
export function CreateChannel({
  onCreated,
  onUnauthorized,
  asks = 0,
}: {
  // Counts requests from elsewhere on the page to open the form (issue
  // #1032); each one opens it and puts focus in the name field.
  readonly asks?: number;
  readonly onCreated: (channel: ChannelV0) => void;
  readonly onUnauthorized: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  // The request in flight, held outside state: two submits in one tick both
  // see the state of the render before them, and the second must send nothing.
  const sending = useRef(false);
  const toggle = useRef<HTMLButtonElement>(null);
  // An answer that comes after the list is gone (signed out) changes nothing.
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);

  const field = useRef<HTMLInputElement>(null);
  const seen = useRef(asks);
  // The form is shown, and the field takes focus, in the render after a
  // request: the effect below runs once the field is no longer hidden.
  const [focusAsk, setFocusAsk] = useState(0);
  useEffect(() => {
    if (asks === seen.current) return;
    seen.current = asks;
    setOpen(true);
    setLine(null);
    setFocusAsk((n) => n + 1);
  }, [asks]);
  useEffect(() => {
    if (focusAsk > 0) field.current?.focus();
  }, [focusAsk]);

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    if (sending.current || value.trim() === '') return;
    sending.current = true;
    setBusy(true);
    setLine(null);
    void api.createChannel(value).then((result) => {
      sending.current = false;
      if (!live.current) return;
      setBusy(false);
      if (result.ok) {
        setOpen(false);
        setValue('');
        setLine({ text: `Created #${result.value.name}`, refused: false });
        onCreated(result.value);
        // The form is hidden with focus in it: focus goes back to its button.
        toggle.current?.focus();
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setLine({ text: refusalLine(result), refused: true });
    });
  }

  return (
    <div className="create-channel">
      <button
        ref={toggle}
        type="button"
        aria-expanded={open}
        aria-controls="create-channel-form"
        onClick={() => {
          setOpen(!open);
          setLine(null);
        }}
      >
        New channel
      </button>
      <form id="create-channel-form" aria-label="New channel" hidden={!open} onSubmit={submit}>
        <label htmlFor="create-channel-field">Channel name</label>
        <input
          id="create-channel-field"
          ref={field}
          type="text"
          autoComplete="off"
          value={value}
          readOnly={busy}
          onChange={(e) => {
            setValue(e.target.value);
          }}
        />
        <button type="submit" disabled={busy || value.trim() === ''}>
          Create channel
        </button>
      </form>
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
    </div>
  );
}
