import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { serverLine } from './Invite';
import { MAX_CHANNEL_TOPIC_LENGTH, type ChannelV0 } from './schema/channel';

// The rule a topic must keep, for a refusal that does not say which part it
// broke. It is the server's (pkg/schema ValidateDisplayName): a length limit,
// no space at either end, no control or invisible characters. Unlike a name, a
// topic may be empty and needs no letter.
const topicRule = `That topic cannot be used: it is at most ${String(MAX_CHANNEL_TOPIC_LENGTH)} characters, with no spaces at either end and no control characters.`;

// The topic's length the way the server counts it: in code points.
function topicLength(topic: string): number {
  return Array.from(topic).length;
}

// What the person is told when the topic is over the limit.
const tooLong = `A topic is at most ${String(MAX_CHANNEL_TOPIC_LENGTH)} characters.`;

// refusalLine is the plain words for a topic the server did not set.
function refusalLine(result: Extract<api.InviteResult<ChannelV0>, { ok: false }>): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === 'invalid_request') return serverLine(result.message, topicRule);
  if (result.code === 'forbidden') return 'You no longer manage this channel.';
  if (result.code === api.channelNotFound) return 'That channel is gone, or you can no longer see it.';
  return 'The server could not set the topic. Try again.';
}

// "Set topic" (issue #1504): an operator or this channel's manager sets or
// clears the topic from the channel's ⋯ menu, in place under its row, one
// request per submit however often a button is pressed. Nothing changes on
// the page until the server has said yes; the channel's answer is handed to
// onSet and the form closes. A refusal leaves the topic as typed, to be
// corrected. Text over the limit is never cut: it is counted, said so, and
// not sent.
export function SetTopic({
  channel,
  onSet,
  onUnauthorized,
  onClose,
}: {
  // The channel being changed, as the list holds it now; its topic prefills.
  readonly channel: Pick<ChannelV0, 'id' | 'topic'>;
  // The channel as the server says it now is, once the topic is set or cleared.
  readonly onSet: (channel: ChannelV0) => void;
  // The session has ended: the caller signs in again.
  readonly onUnauthorized: () => void;
  // Escape, Cancel and a topic set close the form; the caller puts focus back
  // on the channel's ⋯.
  readonly onClose: () => void;
}) {
  const [value, setValue] = useState(channel.topic ?? '');
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  // True from a request sent until its answer, so a second press (or Enter)
  // before the state has re-rendered cannot send twice.
  const sending = useRef(false);
  const field = useRef<HTMLInputElement>(null);
  const fieldId = useId();
  // False once the form is gone, so a late answer does not set state on it.
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    field.current?.focus();
    return () => {
      live.current = false;
    };
  }, []);

  function send(topic: string) {
    if (sending.current) return;
    sending.current = true;
    setBusy(true);
    setLine(null);
    void api.setChannelTopic(channel.id, topic).then((result) => {
      if (result.ok) {
        sending.current = false;
        onSet(result.value);
        if (!live.current) return;
        setBusy(false);
        onClose();
        return;
      }
      sending.current = false;
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      if (!live.current) return;
      setBusy(false);
      setLine(refusalLine(result));
    });
  }

  const over = topicLength(value) > MAX_CHANNEL_TOPIC_LENGTH;

  return (
    <div
      className="rename-channel set-topic"
      onKeyDown={(event) => {
        if (event.key !== 'Escape' || event.nativeEvent.isComposing || event.defaultPrevented) return;
        event.preventDefault();
        event.stopPropagation();
        onClose();
      }}
    >
      <form
        aria-label="Set topic"
        onSubmit={(event: SyntheticEvent) => {
          event.preventDefault();
          if (!over) send(value);
        }}
      >
        <label htmlFor={fieldId}>Topic</label>
        <input
          ref={field}
          id={fieldId}
          type="text"
          autoComplete="off"
          value={value}
          readOnly={busy}
          aria-invalid={over ? 'true' : undefined}
          onChange={(event) => {
            setValue(event.target.value);
          }}
        />
        <button type="submit" disabled={busy || over}>
          Set topic
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => {
            send('');
          }}
        >
          Clear
        </button>
        <button type="button" onClick={onClose}>
          Cancel
        </button>
      </form>
      <p role="status" className={line === null && !over ? 'hint' : 'notice'}>
        {over ? tooLong : (line ?? '')}
      </p>
    </div>
  );
}
