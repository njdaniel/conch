import { useRef, type KeyboardEvent } from 'react';
import { Glyph } from './Icons';
import type { Audience } from './schema/message';
import type { Target, TargetAction } from './state/drafts';
import type { Person } from './useLiveChannel';

// The text the TUI shows after the first whisper.
export const whisperNotice = 'note: whispers are recorded in the audit log';

const channelValue = 'channel';
const whisperValue = 'whisper';
const netValue = (id: number) => `net:${String(id)}`;

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
  readonly onDraft: (text: string) => void;
  readonly onTarget: (action: TargetAction) => void;
  readonly onSend: (body: string, audience: Audience | undefined) => void;
  // Called when a send starts, so the log follows the new message.
  readonly atBottom: () => void;
}) {
  const select = useRef<HTMLSelectElement>(null);
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
  const canSend = enabled && draft.trim() !== '' && (!whispering || chosen.length > 0);

  function audience(): Audience | undefined {
    if (whispering) return { kind: 'principals', principal_ids: [...chosen] };
    if (target.net !== null) return { kind: 'net', net_id: target.net };
    return undefined;
  }

  function submit() {
    if (!canSend) return;
    atBottom();
    onSend(draft, audience());
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

  return (
    <form
      className={value === channelValue ? 'composer' : 'composer narrowed'}
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
          disabled={!enabled}
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
      <div className="composer-box">
      <textarea
        id="composer-text"
        rows={2}
        value={draft}
        disabled={!enabled}
        onChange={(event) => {
          onDraft(event.target.value);
        }}
        onKeyDown={(event) => {
          if (event.key === 'Escape' && whispering) {
            event.preventDefault();
            onTarget({ type: 'closePicker', channel });
            select.current?.focus();
            return;
          }
          key(event);
        }}
      />
      <button type="submit" className="primary send" disabled={!canSend}>
        <Glyph name="send" />
        <span className="label">Send</span>
      </button>
      </div>
      {target.refusal === null ? null : <p role="alert" className="notice">{target.refusal}</p>}
      {target.note === null ? null : <p role="status" className="hint">{target.note}</p>}
    </form>
  );
}
