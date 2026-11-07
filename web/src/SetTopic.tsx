import { useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { serverLine } from './Invite';
import { MAX_CHANNEL_TOPIC_LENGTH, type ChannelV0 } from './schema/channel';

const topicRule = `That topic cannot be used: it needs a letter you can see, at most ${String(MAX_CHANNEL_TOPIC_LENGTH)} characters, and no spaces at either end or control characters.`;

function refusalLine(result: Extract<api.InviteResult<ChannelV0>, { ok: false }>): string {
  if (result.failure === 'network') return 'Cannot reach the server. Try again.';
  if (result.code === 'invalid_request') return serverLine(result.message, topicRule);
  if (result.code === 'forbidden') return 'You no longer manage this channel.';
  if (result.code === api.channelNotFound) return 'That channel is gone, or you can no longer see it.';
  return 'The server could not set the topic. Try again.';
}

export function SetTopic({
  channel,
  onSet,
  onUnauthorized,
  onClose,
}: {
  readonly channel: Pick<ChannelV0, 'id' | 'topic'>;
  readonly onSet: (channel: ChannelV0) => void;
  readonly onUnauthorized: () => void;
  readonly onClose: () => void;
}) {
  const [value, setValue] = useState(channel.topic ?? '');
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<string | null>(null);
  const sending = useRef(false);
  const field = useRef<HTMLInputElement>(null);
  const fieldId = useId();
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
          send(value);
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
          onChange={(event) => {
            setValue(Array.from(event.target.value).slice(0, MAX_CHANNEL_TOPIC_LENGTH).join(''));
          }}
        />
        <button type="submit" disabled={busy}>
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
      <p role="status" className={line === null ? 'hint' : 'notice'}>
        {line ?? ''}
      </p>
    </div>
  );
}
