import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { Glyph } from './Icons';
import { ActionsMenu, useOpeners, type MenuItem } from './MessageMenu';
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

// One channel's row (issue #1135): its button, which opens the channel, its
// badges, and, when the viewer may do anything to the channel, one ⋯ menu
// beside them (never inside the button). The menu is the message log's
// (MessageMenu.tsx), opened by its button, Enter or Space on it, a right-click
// on the row or a long press. A row with no items draws no ⋯ and suppresses no
// native menu. New items are one more push onto `items`; the rename form, when
// chosen, sits in the row under the button and hands focus back to the ⋯.
function ChannelRow({
  channel,
  index,
  selected,
  stop,
  unread,
  voiced,
  mayRename,
  renderRename,
  onInvite,
  register,
  onMove,
  onOpen,
}: {
  readonly channel: ChannelV0;
  readonly index: number;
  readonly selected: string | null;
  readonly stop: string | null;
  readonly unread: number;
  readonly voiced: number;
  readonly mayRename: boolean;
  readonly renderRename: ((channel: ChannelV0, close: () => void) => ReactNode) | undefined;
  readonly onInvite: ((channel: ChannelV0) => void) | undefined;
  readonly register: (name: string, el: HTMLButtonElement | null) => void;
  readonly onMove: (event: KeyboardEvent, from: number) => void;
  readonly onOpen: (name: string) => void;
}) {
  const [menuOpen, setMenuOpen] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const more = useRef<HTMLButtonElement>(null);
  const main = useRef<HTMLButtonElement | null>(null);
  // The grant to rename was revoked while the form was open: it goes, and
  // focus, which was in it, goes to the row's button.
  useEffect(() => {
    if (mayRename || !renaming) return;
    setRenaming(false);
    main.current?.focus();
  }, [mayRename, renaming]);
  // What describes the button: a manager grant, who is in its voice, then its unread.
  const described = [channel.manages === true ? managesId(channel.id) : null, voiced > 0 ? voiceId(channel.id) : null, unread > 0 ? badgeId(channel.id) : null].filter(
    (x) => x !== null,
  );

  const items: MenuItem[] = [];
  if (onInvite !== undefined) {
    items.push({
      key: 'invite',
      label: 'Invite people',
      onSelect: () => {
        onInvite(channel);
      },
    });
  }
  if (mayRename && renderRename !== undefined) {
    items.push({
      key: 'rename',
      label: 'Rename channel…',
      onSelect: () => {
        setRenaming(true);
      },
    });
  }
  const openers = useOpeners(items.length > 0, () => {
    setMenuOpen(true);
  });

  return (
    <li className={items.length > 0 ? 'has-actions' : undefined} {...openers}>
      <button
        type="button"
        ref={(el) => {
          main.current = el;
          register(channel.name, el);
        }}
        tabIndex={channel.name === stop ? 0 : -1}
        aria-describedby={described.length === 0 ? undefined : described.join(' ')}
        aria-current={channel.name === selected ? 'true' : undefined}
        onClick={() => {
          onOpen(channel.name);
        }}
        onKeyDown={(event) => {
          onMove(event, index);
        }}
      >
        {channel.name}
      </button>
      {channel.manages === true ? (
        <span id={managesId(channel.id)} className="ag-badge manages">
          manages
        </span>
      ) : null}
      <VoiceMark id={channel.id} count={voiced} />
      <UnreadBadge id={channel.id} count={unread} />
      {items.length === 0 ? null : (
        <ActionsMenu label={`Actions for ${channel.name}`} items={items} open={menuOpen} onOpenChange={setMenuOpen} button={more} />
      )}
      {renaming && mayRename && renderRename !== undefined
        ? renderRename(channel, () => {
            setRenaming(false);
            // The ⋯ may be gone (the grant was revoked meanwhile): the row's button then.
            (more.current ?? main.current)?.focus();
          })
        : null}
    </li>
  );
}

// The channels as a list of buttons. Tab reaches the list once; the arrow keys
// move between channels (Home and End jump), Enter or Space opens the one
// focused. A row may carry a ⋯ menu of what the viewer can do to that channel
// (issue #1135): Invite people when `onInvite` is given, Rename channel… when
// `canRename` says so for it. Managed channels carry a badge.
export function ChannelList({
  channels,
  selected,
  unread,
  occupancy = nobody,
  empty = 'No channels yet: an operator needs to add you to one',
  canRename,
  renderRename,
  onInvite,
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
  // Whether this viewer may rename that channel (an operator, or its manager);
  // absent, nobody. The server still decides.
  readonly canRename?: (channel: ChannelV0) => boolean;
  // The rename form for a channel, shown in place under its row; `close`
  // hides it and returns focus to the row's ⋯.
  readonly renderRename?: (channel: ChannelV0, close: () => void) => ReactNode;
  // Opens the invite panel with that channel ticked; absent for a viewer who
  // may not invite, who is then offered no Invite people.
  readonly onInvite?: (channel: ChannelV0) => void;
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

  function register(name: string, el: HTMLButtonElement | null) {
    if (el === null) buttons.current.delete(name);
    else buttons.current.set(name, el);
  }

  if (channels.length === 0) {
    return <p className="empty">{empty}</p>;
  }
  return (
    <ul className="channel-list">
      {channels.map((channel, i) => (
        <ChannelRow
          key={channel.id}
          channel={channel}
          index={i}
          selected={selected}
          stop={stop}
          unread={unread.get(channel.name) ?? 0}
          voiced={occupancy.get(channel.id) ?? 0}
          mayRename={canRename?.(channel) === true}
          renderRename={renderRename}
          onInvite={onInvite}
          register={register}
          onMove={move}
          onOpen={onOpen}
        />
      ))}
    </ul>
  );
}
