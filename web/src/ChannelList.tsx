import { useRef, type KeyboardEvent } from 'react';
import type { ChannelV0 } from './schema/channel';

// The channels as a list of buttons. Tab reaches the list once; the arrow keys
// move between channels (Home and End jump), Enter or Space opens the one
// focused.
export function ChannelList({
  channels,
  selected,
  onOpen,
}: {
  readonly channels: readonly ChannelV0[];
  readonly selected: string | null;
  readonly onOpen: (name: string) => void;
}) {
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  // The one button in the tab order: the open channel, or the first.
  const stop = selected ?? channels[0]?.name ?? null;

  function move(event: KeyboardEvent, from: number) {
    const last = channels.length - 1;
    const to =
      event.key === 'ArrowDown' ? Math.min(from + 1, last)
      : event.key === 'ArrowUp' ? Math.max(from - 1, 0)
      : event.key === 'Home' ? 0
      : event.key === 'End' ? last
      : null;
    if (to === null) return;
    event.preventDefault();
    const target = channels[to];
    if (target !== undefined) buttons.current.get(target.name)?.focus();
  }

  if (channels.length === 0) {
    return <p className="empty">No channels yet: an operator needs to add you to one</p>;
  }
  return (
    <ul className="channel-list">
      {channels.map((channel, i) => (
        <li key={channel.id}>
          <button
            type="button"
            ref={(el) => {
              if (el === null) buttons.current.delete(channel.name);
              else buttons.current.set(channel.name, el);
            }}
            tabIndex={channel.name === stop ? 0 : -1}
            aria-current={channel.name === selected ? 'true' : undefined}
            onClick={() => {
              onOpen(channel.name);
            }}
            onKeyDown={(event) => {
              move(event, i);
            }}
          >
            {channel.name}
          </button>
        </li>
      ))}
    </ul>
  );
}
