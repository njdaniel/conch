import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { waitWords } from './Invite';
import type { ChannelV0, ChannelVisibility } from './schema/channel';

// The rule a name must keep, for a refusal that does not say which part it
// broke. The server's own line is never shown: the page's words are chosen by
// the server's code.
export const nameRule = 'That name cannot be used: it needs a letter you can see, at most 100 characters, and no spaces at either end or control characters.';

// The one answer to every collision. A name is taken whoever's channel it is,
// and the page never says whether the one in the way is a private channel it
// cannot see (design section 5, R2).
export const nameTaken = 'That name is taken';

// How long a daily cap asks the person to wait: the Retry-After, in hours once
// it is long.
function capWait(seconds: number | undefined): string {
  if (seconds === undefined || seconds < 7200) return waitWords(seconds);
  return `${String(Math.ceil(seconds / 3600))} hours`;
}

type Line = { readonly text: string; readonly refused: boolean };

// refusalLine is the plain words for a channel the server did not make.
function refusalLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly retryAfter?: number }, visibility: ChannelVisibility): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  switch (result.code) {
    case 'channel_exists':
      return nameTaken;
    case 'rate_limited':
      return `You have reached the daily limit for making channels. Try again in ${capWait(result.retryAfter)}.`;
    case 'permission_denied':
      return `You do not have permission to make a ${visibility} channel.`;
    case 'forbidden':
      return 'Only members of this community can make channels.';
    case 'invalid_request':
      return nameRule;
    default:
      return 'The server could not create the channel. Try again.';
  }
}

// "New channel" (issues #695, #991): a member makes a public or private channel
// from the channel list (the server decides who may make which), one request per submit, however often the button is pressed. The new
// channel is the server's answer, handed to onCreated; nothing changes in the
// list until the server has said yes. A refusal leaves the name as typed, to
// be corrected. Making a channel only makes it: it does not join its voice.
export function CreateChannel({
  onCreated,
  onUnauthorized,
  asks = 0,
  canPublic = true,
  canPrivate = true,
}: {
  // Counts requests from elsewhere on the page to open the form (issue
  // #1032); each one opens it and puts focus in the name field.
  readonly asks?: number;
  // Which visibilities this person is offered, from what whoami says they
  // hold; the server still decides. At least one is true, or this is not drawn.
  readonly canPublic?: boolean;
  readonly canPrivate?: boolean;
  readonly onCreated: (channel: ChannelV0) => void;
  readonly onUnauthorized: () => void;
}) {
  // Two of these may be on the page at once (the channel list's and the
  // console's), so the ids that tie the label and the form are made, not typed.
  const ids = useId();
  const formId = `${ids}-form`;
  const fieldId = `${ids}-field`;
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState('');
  const [chosen, setChosen] = useState<ChannelVisibility>('public');
  // What is sent is a visibility this person is offered.
  const visibility: ChannelVisibility = chosen === 'private' && !canPrivate ? 'public' : chosen === 'public' && !canPublic ? 'private' : chosen;
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
    void api.createChannel(value, visibility).then((result) => {
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
      setLine({ text: refusalLine(result, visibility), refused: true });
    });
  }

  return (
    <div className="create-channel">
      <button
        ref={toggle}
        type="button"
        aria-expanded={open}
        aria-controls={formId}
        onClick={() => {
          setOpen(!open);
          setLine(null);
        }}
      >
        New channel
      </button>
      <form id={formId} aria-label="New channel" hidden={!open} onSubmit={submit}>
        <label htmlFor={fieldId}>Channel name</label>
        <input
          id={fieldId}
          ref={field}
          type="text"
          autoComplete="off"
          value={value}
          readOnly={busy}
          onChange={(e) => {
            setValue(e.target.value);
          }}
        />
        {canPublic && canPrivate ? (
          <fieldset disabled={busy}>
            <legend>Who can find it</legend>
            <label>
              <input
                type="radio"
                name={`${ids}-visibility`}
                checked={visibility === 'public'}
                onChange={() => {
                  setChosen('public');
                }}
              />
              Public: every member can find and join it
            </label>
            <label>
              <input
                type="radio"
                name={`${ids}-visibility`}
                checked={visibility === 'private'}
                onChange={() => {
                  setChosen('private');
                }}
              />
              Private: only people you add
            </label>
          </fieldset>
        ) : null}
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
