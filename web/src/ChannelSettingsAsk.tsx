import { useEffect, useRef, useState } from 'react';
import * as api from './api';
import type { ChannelSettingsRequestV1, ChannelV0, ChannelVisibility } from './schema/channel';

// One change to a channel's settings, as the ⋯ menu asks for it (issue #1183).
export type SettingsChange =
  | { readonly kind: 'visibility'; readonly to: ChannelVisibility }
  | { readonly kind: 'default'; readonly to: boolean };

// The words of a refusal are the page's own, by the server's code; the
// server's message is never shown (it may carry a channel's name or anything a
// proxy wrote).
export function refusalLine(failure: api.ApiFailure, code: string | undefined, change: SettingsChange): string {
  if (failure === 'network') return 'Cannot reach the server. Nothing was changed; try again.';
  switch (code) {
    case 'channel_settings_conflict':
      return change.kind === 'default' && change.to
        ? 'A private channel cannot be a default channel. Make it public first.'
        : 'A default channel must be public. Stop it being a default channel first.';
    case 'permission_denied':
      return 'You need permission to manage channels to change this.';
    case 'reauth_required':
      return 'Nothing was changed: it needs your password first.';
    case api.channelNotFound:
      return 'That channel is gone, or you can no longer see it.';
    default:
      return 'The server could not change the channel. Nothing was changed.';
  }
}

export const openSignupOffLine = 'Open sign-up is now off: no channel is a default channel.';

// What the person is asked before a change that takes effect for everyone.
// Making a channel a default, or not, is not asked: it adds and removes nobody.
export function askLine(name: string, to: ChannelVisibility): string {
  return to === 'private'
    ? `Make #${name} private? Only its current members will see it.`
    : `Make #${name} public? Every member will be able to find and join it.`;
}

type Line = { readonly text: string; readonly refused: boolean };

// The confirmation under a channel's row, and the request it sends: one per
// confirm, however often the button is pressed. Focus starts on Cancel. A
// default mark is sent at once, since nothing is asked, and this shows how it
// went. Nothing changes on the page until the server has said yes; its answer
// is handed to onApplied. A refusal leaves this open to be read and closed.
export function ChannelSettingsAsk({
  channel,
  change,
  onApplied,
  onUnauthorized,
  onClose,
}: {
  readonly channel: ChannelV0;
  readonly change: SettingsChange;
  readonly onApplied: (channel: ChannelV0) => void;
  readonly onUnauthorized: () => void;
  // Escape, Cancel and Close; the caller puts focus back on the row's ⋯.
  readonly onClose: () => void;
}) {
  const asks = change.kind === 'visibility';
  const [busy, setBusy] = useState(!asks);
  const [line, setLine] = useState<Line | null>(null);
  const [done, setDone] = useState(false);
  const sending = useRef(false);
  const live = useRef(true);
  const cancel = useRef<HTMLButtonElement>(null);
  // The request this component was made for, which later renders never alter.
  const frozen = useRef(change);
  const id = channel.id;

  function send() {
    if (sending.current) return;
    sending.current = true;
    setBusy(true);
    setLine(null);
    const c = frozen.current;
    const request: ChannelSettingsRequestV1 = c.kind === 'visibility' ? { visibility: c.to } : { default: c.to };
    void api.channelSettings(id, request).then((result) => {
      if (result.ok) {
        onApplied(result.value.channel);
        if (!live.current) return;
        if (result.value.open_signup_turned_off === true) {
          setBusy(false);
          setDone(true);
          setLine({ text: openSignupOffLine, refused: false });
          return;
        }
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
      setDone(true);
      setLine({ text: refusalLine(result.failure, result.code, c), refused: true });
    });
  }

  useEffect(() => {
    live.current = true;
    cancel.current?.focus();
    if (!asks) send();
    return () => {
      live.current = false;
    };
    // Sent once, on mounting: later renders of the row change nothing here.
  }, []);

  const shown = frozen.current;
  const asking = shown.kind === 'visibility' && !done;
  return (
    <div
      className="channel-settings-ask"
      role="group"
      aria-label={`Change settings of ${channel.name}`}
      onKeyDown={(event) => {
        if (event.key !== 'Escape' || event.nativeEvent.isComposing || event.defaultPrevented) return;
        event.preventDefault();
        event.stopPropagation();
        onClose();
      }}
    >
      {shown.kind === 'visibility' && asking ? <p>{askLine(channel.name, shown.to)}</p> : null}
      <div className="channel-settings-buttons">
        {shown.kind === 'visibility' && asking ? (
          <button type="button" disabled={busy} onClick={send}>
            {shown.to === 'private' ? 'Make private' : 'Make public'}
          </button>
        ) : null}
        <button type="button" ref={cancel} onClick={onClose}>
          {asking ? 'Cancel' : 'Close'}
        </button>
      </div>
      <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
        {busy && line === null ? 'Saving…' : (line?.text ?? '')}
      </p>
    </div>
  );
}
