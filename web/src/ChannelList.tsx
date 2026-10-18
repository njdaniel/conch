import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import type { ChannelV0 } from './schema/channel';
import { badgeText } from './useReadState';

const badgeId = (channel: number) => `unread-${String(channel)}`;

// The count after a channel's name, with a name of its own ("3 unread") that the
// channel's button is described by. It sits beside the button, not in it, so the
// button keeps its name, the channel's.
function UnreadBadge({ id, count }: { readonly id: number; readonly count: number }) {
  const text = badgeText(count);
  if (text === null) return null;
  return (
    <span id={badgeId(id)} className="unread" role="img" aria-label={`${text} unread`}>
      {text}
    </span>
  );
}

// The channels as a list of buttons. Tab reaches the list once; the arrow keys
// move between channels (Home and End jump), Enter or Space opens the one
// focused.
export function ChannelList({
  channels,
  selected,
  unread,
  onOpen,
}: {
  readonly channels: readonly ChannelV0[];
  readonly selected: string | null;
  // Each channel's unread count by name; a channel at 0 has no entry.
  readonly unread: ReadonlyMap<string, number>;
  readonly onOpen: (name: string) => void;
}) {
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  // The one button in the tab order: the open channel, or the first.
  const stop = selected ?? channels[0]?.name ?? null;

  // The open channel is kept in sight, however it was opened: on a phone the
  // list is one row that scrolls sideways, and it may be off screen. jsdom
  // has no scrollIntoView.
  useEffect(() => {
    const el = selected === null ? undefined : buttons.current.get(selected);
    if (el !== undefined && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [selected]);

  // On a phone the row says there are more channels than it shows (issue
  // #601): more-before while some lie off its left edge, more-after while
  // some lie off its right. The stylesheet draws the fade. Measured on
  // scroll, on resize, and whenever the channels or their counts change
  // what the row holds. A pixel's slack keeps a fractional width from
  // leaving a fade at the end.
  const list = useRef<HTMLUListElement>(null);
  const [more, setMore] = useState('');
  useEffect(() => {
    const el = list.current;
    if (el === null) return;
    const measure = () => {
      const before = el.scrollLeft > 1;
      const after = el.scrollLeft + el.clientWidth < el.scrollWidth - 1;
      setMore(`${before ? ' more-before' : ''}${after ? ' more-after' : ''}`);
    };
    measure();
    el.addEventListener('scroll', measure, { passive: true });
    window.addEventListener('resize', measure);
    return () => {
      el.removeEventListener('scroll', measure);
      window.removeEventListener('resize', measure);
    };
  }, [channels, unread]);

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
    <ul className={`channel-list${more}`} ref={list}>
      {channels.map((channel, i) => (
        <li key={channel.id}>
          <button
            type="button"
            ref={(el) => {
              if (el === null) buttons.current.delete(channel.name);
              else buttons.current.set(channel.name, el);
            }}
            tabIndex={channel.name === stop ? 0 : -1}
            aria-describedby={(unread.get(channel.name) ?? 0) > 0 ? badgeId(channel.id) : undefined}
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
          <UnreadBadge id={channel.id} count={unread.get(channel.name) ?? 0} />
        </li>
      ))}
    </ul>
  );
}
