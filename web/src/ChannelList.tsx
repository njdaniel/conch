import { useEffect, useRef, type KeyboardEvent, type ReactNode } from 'react';
import { Glyph } from './Icons';
import type { ChannelV0 } from './schema/channel';
import { badgeText } from './useReadState';
import { nobody, type InVoice } from './voice/useOccupancy';

const badgeId = (channel: number) => `unread-${String(channel)}`;
const voiceId = (channel: number) => `in-voice-${String(channel)}`;
const managesId = (channel: number) => `manages-${String(channel)}`;

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

// How many are in a channel's voice (issue #596): a speaker and the number,
// named "3 in voice", beside the button and describing it, as the unread count
// does. It says where people are talking and nothing more: the button opens
// the channel, and only the voice strip joins.
function VoiceMark({ id, count }: { readonly id: number; readonly count: number }) {
  if (count <= 0) return null;
  return (
    <span id={voiceId(id)} className="in-voice" role="img" aria-label={`${String(count)} in voice`}>
      <Glyph name="speaker" />
      {count}
    </span>
  );
}

// The channels as a list of buttons. Tab reaches the list once; the arrow keys
// move between channels (Home and End jump), Enter or Space opens the one
// focused. Under it, for an operator or this channel's manager, the control
// that renames the open channel (issue #728). Managed channels carry a badge.
export function ChannelList({
  channels,
  selected,
  unread,
  occupancy = nobody,
  empty = 'No channels yet: an operator needs to add you to one',
  rename,
  onOpen,
}: {
  readonly channels: readonly ChannelV0[];
  readonly selected: string | null;
  // Each channel's unread count by name; a channel at 0 has no entry.
  readonly unread: ReadonlyMap<string, number>;
  // How many are in each channel's voice, by id; nobody, no entry.
  readonly occupancy?: InVoice;
  // What an empty list says: by default, who can change that. Someone who can
  // make a channel is told less, with the control to do it beside the list.
  readonly empty?: string;
  // Rename the open channel (issue #728), or nothing for a person who may
  // not: shown under the list while a channel is open.
  readonly rename?: ReactNode;
  readonly onOpen: (name: string) => void;
}) {
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  // The one button in the tab order: the open channel, or the first.
  const stop = selected ?? channels[0]?.name ?? null;

  // The open channel is kept in sight, however it was opened: in a long list
  // it may be off screen. jsdom has no scrollIntoView.
  useEffect(() => {
    const el = selected === null ? undefined : buttons.current.get(selected);
    if (el !== undefined && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [selected]);

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
    return <p className="empty">{empty}</p>;
  }
  return (
    <>
      <ul className="channel-list">
        {channels.map((channel, i) => {
          // What describes the button: a manager grant, who is in its voice, then its unread.
          const described = [
            channel.manages === true ? managesId(channel.id) : null,
            (occupancy.get(channel.id) ?? 0) > 0 ? voiceId(channel.id) : null,
            (unread.get(channel.name) ?? 0) > 0 ? badgeId(channel.id) : null,
          ].filter((x) => x !== null);
          return (
            <li key={channel.id}>
              <button
                type="button"
                ref={(el) => {
                  if (el === null) buttons.current.delete(channel.name);
                  else buttons.current.set(channel.name, el);
                }}
                tabIndex={channel.name === stop ? 0 : -1}
                aria-describedby={described.length === 0 ? undefined : described.join(' ')}
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
              {channel.manages === true ? (
                <span id={managesId(channel.id)} className="ag-badge manages">
                  manages
                </span>
              ) : null}
              <VoiceMark id={channel.id} count={occupancy.get(channel.id) ?? 0} />
              <UnreadBadge id={channel.id} count={unread.get(channel.name) ?? 0} />
            </li>
          );
        })}
      </ul>
      {selected === null ? null : rename}
    </>
  );
}
