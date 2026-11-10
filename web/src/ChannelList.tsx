import { Fragment, useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import * as api from './api';
import { isArchived } from './archived';
import { Glyph } from './Icons';
import { ActionsMenu, useOpeners, type MenuItem } from './MessageMenu';
import { leaveLine } from './leaveChannel';
import type { SettingsChange } from './ChannelSettingsAsk';
import { MoveToGroup, type ArrangementControls } from './ChannelGroups';
import { emptyArrangement, expandGroup, layoutOf, moveToGroup, moveWithin, toggleCollapsed, toggleFavorite, type Group } from './arrangement';
import type { ChannelV0 } from './schema/channel';
import { badgeText } from './useReadState';
import { nobody, type InVoice, type Voices } from './voice/useOccupancy';

// How many people a channel lists in voice before folding the rest into
// "+M more" (issue #1103).
export const shownInVoice = 5;

const noNames: ReadonlyMap<number, string> = new Map();

const badgeId = (channel: number) => `unread-${String(channel)}`;
const voiceId = (channel: number) => `in-voice-${String(channel)}`;
const privateId = (channel: number) => `private-${String(channel)}`;
const topicId = (channel: number) => `topic-${String(channel)}`;
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

// voiceName is how someone in a channel's voice is named in the list: the
// viewer is their own name (names has it, from the signed-in identity), or
// "You" until that is known; anyone else by the name the page has for them,
// or, until the channel's members have been read, by their id.
export function voiceName(id: number, viewer: number, names: ReadonlyMap<number, string>): string {
  if (id === viewer) {
    const own = names.get(id);
    return own === undefined || own === '' ? 'You' : own;
  }
  return names.get(id) ?? `Member ${String(id)}`;
}

// Who is in a channel's voice (issue #1103), nested under its row: a list
// named for the channel, one row a person, up to shownInVoice of them and
// then "+M more". Each row opens the channel, as the channel's own button
// does, and nothing more: nothing in the list joins voice. The rows are
// outside the tab order, since the channel's button, which is in it, does
// what they do; they are read as text under the channel. No speaking,
// muted or deafened state: that is the voice bar's, for the room joined.
function VoicePeople({
  channel,
  people,
  names,
  viewer,
  onOpen,
}: {
  readonly channel: ChannelV0;
  readonly people: readonly number[];
  readonly names: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly onOpen: (name: string) => void;
}) {
  if (people.length === 0) return null;
  const shown = people.slice(0, shownInVoice);
  const more = people.length - shown.length;
  const open = () => {
    onOpen(channel.name);
  };
  return (
    <ul className="in-voice-people" aria-label={`In voice in #${channel.name}`}>
      {shown.map((id) => (
        <li key={id}>
          <button type="button" tabIndex={-1} onClick={open}>
            {voiceName(id, viewer, names)}
          </button>
        </li>
      ))}
      {more > 0 ? (
        <li className="more">
          <button type="button" tabIndex={-1} onClick={open}>
            +{more} more
          </button>
        </li>
      ) : null}
    </ul>
  );
}

// What a row needs to offer the person's arrangement (issue #1493).
interface RowArrange {
  readonly favorite: boolean;
  // In favorites or in a group: it can be moved up and down there.
  readonly section: boolean;
  readonly canUp: boolean;
  readonly canDown: boolean;
  readonly groups: readonly Group[];
  readonly groupId: string | null;
  readonly onFavorite: () => void;
  readonly onMove: (direction: -1 | 1) => void;
  readonly onMoveToGroup: (groupId: string | null) => void;
}

// One channel's row (issues #1135, #1095): its button, which opens the channel,
// its badges, and, when the viewer may do anything to the channel, one ⋯ menu
// beside them (never inside the button). The menu is the message log's
// (MessageMenu.tsx), opened by its button, Enter or Space on it, a right-click
// on the row or a long press. A row with no items draws no ⋯ and suppresses no
// native menu. Forms and answers sit in the row under the button and hand focus
// back to the ⋯.
function ChannelRow({
  channel,
  index,
  selected,
  stop,
  unread,
  voiced,
  voiceNames,
  viewer,
  mayRename,
  renderRename,
  maySetTopic,
  renderTopic,
  mayManage,
  renderSettings,
  onInvite,
  onLeft,
  onUnauthorized,
  register,
  onMove,
  onOpen,
  arrange,
}: {
  readonly channel: ChannelV0;
  readonly index: number;
  readonly selected: string | null;
  readonly stop: string | null;
  readonly unread: number;
  readonly voiced: Voices | undefined;
  readonly voiceNames: ReadonlyMap<number, string>;
  readonly viewer: number;
  readonly mayRename: boolean;
  readonly renderRename: ((channel: ChannelV0, close: () => void) => ReactNode) | undefined;
  readonly maySetTopic: boolean;
  readonly renderTopic: ((channel: ChannelV0, close: () => void) => ReactNode) | undefined;
  readonly mayManage: boolean;
  readonly renderSettings: ((channel: ChannelV0, change: SettingsChange, close: () => void) => ReactNode) | undefined;
  readonly onInvite: ((channel: ChannelV0) => void) | undefined;
  readonly onLeft: ((name: string) => void) | undefined;
  readonly onUnauthorized: (() => void) | undefined;
  readonly register: (name: string, el: HTMLButtonElement | null) => void;
  readonly onMove: (event: KeyboardEvent, from: number) => void;
  readonly onOpen: (name: string) => void;
  // The person's own arrangement (issue #1493); absent, the row offers none.
  readonly arrange: RowArrange | undefined;
}) {
  const [moving, setMoving] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [settingTopic, setSettingTopic] = useState(false);
  // The settings change being confirmed or sent (issue #1183), in place under the row.
  const [change, setChange] = useState<SettingsChange | null>(null);
  const [askingLeave, setAskingLeave] = useState(false);
  const [leaveBusy, setLeaveBusy] = useState(false);
  const [leaveRefusal, setLeaveRefusal] = useState<string | null>(null);
  const leaveSending = useRef(false);
  const cancelLeave = useRef<HTMLButtonElement>(null);
  const live = useRef(true);
  const row = useRef<HTMLLIElement>(null);
  const isPrivate = channel.visibility === 'private';
  const isDefault = channel.default === true;
  const archived = isArchived(channel);
  const more = useRef<HTMLButtonElement>(null);
  const main = useRef<HTMLButtonElement | null>(null);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useEffect(() => {
    if (askingLeave) cancelLeave.current?.focus();
  }, [askingLeave]);
  // The grant to rename was revoked while the form was open: it goes, and
  // focus, which was in it, goes to the row's button.
  useEffect(() => {
    if (mayRename || !renaming) return;
    setRenaming(false);
    main.current?.focus();
  }, [mayRename, renaming]);
  useEffect(() => {
    if (maySetTopic || !settingTopic) return;
    setSettingTopic(false);
    main.current?.focus();
  }, [maySetTopic, settingTopic]);
  // The grant to manage channels went while a change was open: it goes,
  // unsent, and focus, if it was in it, goes to the row's button.
  useEffect(() => {
    if (change === null || mayManage) return;
    // The focused control was in the confirmation, which is gone with this
    // commit, so focus is on the body now; elsewhere on the page it stays.
    const lost = document.activeElement === document.body || row.current?.contains(document.activeElement) === true;
    setChange(null);
    if (lost) main.current?.focus();
  }, [mayManage, change]);
  // What describes the button: private, a manager grant, who is in its voice, then its unread.
  const inVoice = voiced?.count ?? 0;
  const hasTopic = channel.topic !== undefined && channel.topic !== '';
  const described = [hasTopic ? topicId(channel.id) : null, isPrivate ? privateId(channel.id) : null, channel.manages === true ? managesId(channel.id) : null, inVoice > 0 ? voiceId(channel.id) : null, unread > 0 ? badgeId(channel.id) : null].filter(
    (x) => x !== null,
  );

  function closeLeave() {
    setAskingLeave(false);
    more.current?.focus();
  }

  function leave() {
    if (onLeft === undefined || leaveSending.current) return;
    leaveSending.current = true;
    setAskingLeave(false);
    setLeaveBusy(true);
    setLeaveRefusal(null);
    void api.leaveChannel(channel.name).then((result) => {
      leaveSending.current = false;
      if (result.ok) {
        onLeft(channel.name);
        return;
      }
      if (result.failure === 'unauthorized') {
        if (live.current) setLeaveBusy(false);
        onUnauthorized?.();
        return;
      }
      if (!live.current) return;
      setLeaveBusy(false);
      setLeaveRefusal(leaveLine(result.failure, result.code));
      more.current?.focus();
    });
  }

  const items: MenuItem[] = [];
  // An archived channel takes no new member, so it offers no invite.
  if (onInvite !== undefined && !archived) {
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
        setAskingLeave(false);
        setLeaveRefusal(null);
        setChange(null);
        setSettingTopic(false);
        setRenaming(true);
      },
    });
  }
  if (maySetTopic && renderTopic !== undefined) {
    items.push({
      key: 'topic',
      label: 'Set topic',
      onSelect: () => {
        setAskingLeave(false);
        setLeaveRefusal(null);
        setChange(null);
        setRenaming(false);
        setSettingTopic(true);
      },
    });
  }
  if (onLeft !== undefined) {
    items.push({
      key: 'leave',
      label: 'Leave channel',
      disabled: leaveBusy,
      onSelect: () => {
        setRenaming(false);
        setChange(null);
        setLeaveRefusal(null);
        // A private channel cannot be found again once left, so it asks first.
        if (isPrivate) setAskingLeave(true);
        else leave();
      },
    });
  }
  if (mayManage && renderSettings !== undefined && archived) {
    items.push({
      key: 'restore',
      label: 'Restore channel',
      onSelect: () => {
        setAskingLeave(false);
        setLeaveRefusal(null);
        setRenaming(false);
        setChange({ kind: 'archived', to: false });
      },
    });
  }
  if (mayManage && renderSettings !== undefined && !archived) {
    items.push({
      key: 'visibility',
      label: isPrivate ? 'Make public' : 'Make private',
      ...(!isPrivate && isDefault ? { disabled: true, reason: 'A default channel must be public. Stop it being a default channel first.' } : {}),
      onSelect: () => {
        setAskingLeave(false);
        setLeaveRefusal(null);
        setRenaming(false);
        setChange({ kind: 'visibility', to: isPrivate ? 'public' : 'private' });
      },
    });
    items.push({
      key: 'default',
      label: isDefault ? 'Stop being a default channel' : 'Make default channel',
      ...(!isDefault && isPrivate ? { disabled: true, reason: 'A default channel must be public. Make it public first.' } : {}),
      onSelect: () => {
        setAskingLeave(false);
        setLeaveRefusal(null);
        setRenaming(false);
        setChange({ kind: 'default', to: !isDefault });
      },
    });
    items.push({
      key: 'archive',
      label: 'Archive channel…',
      ...(isDefault ? { disabled: true, reason: 'Stop it being a default channel first.' } : {}),
      onSelect: () => {
        setAskingLeave(false);
        setLeaveRefusal(null);
        setRenaming(false);
        setChange({ kind: 'archived', to: true });
      },
    });
  }
  // The arrangement is last: the menu's first item stays what it was (issue #1493).
  if (arrange !== undefined && !archived) {
    items.push({
      key: 'favorite',
      label: arrange.favorite ? 'Unfavorite' : 'Favorite',
      onSelect: () => {
        arrange.onFavorite();
      },
    });
    if (arrange.section) {
      items.push({
        key: 'move-up',
        label: 'Move up',
        ...(arrange.canUp ? {} : { disabled: true, reason: 'Already first.' }),
        onSelect: () => {
          arrange.onMove(-1);
        },
      });
      items.push({
        key: 'move-down',
        label: 'Move down',
        ...(arrange.canDown ? {} : { disabled: true, reason: 'Already last.' }),
        onSelect: () => {
          arrange.onMove(1);
        },
      });
    }
    if (arrange.groups.length > 0) {
      items.push({
        key: 'move-group',
        label: 'Move to group…',
        onSelect: () => {
          setRenaming(false);
          setChange(null);
          setAskingLeave(false);
          setMoving(true);
        },
      });
    }
  }
  const openers = useOpeners(items.length > 0, () => {
    setMenuOpen(true);
  });

  return (
    <li ref={row} className={[items.length > 0 ? 'has-actions' : '', isPrivate ? 'private' : '', archived ? 'archived' : ''].filter((c) => c !== '').join(' ') || undefined} {...openers}>
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
        title={hasTopic ? `${channel.name} — ${channel.topic ?? ''}` : channel.name}
      >
        {isPrivate ? <Glyph name="lock" /> : null}
        <span className="channel-name">{channel.name}</span>
      </button>
      {hasTopic ? (
        <span id={topicId(channel.id)} hidden>
          {channel.topic}
        </span>
      ) : null}
      {isPrivate ? (
        <span id={privateId(channel.id)} hidden>
          private channel
        </span>
      ) : null}
      {channel.manages === true ? (
        <span id={managesId(channel.id)} className="ag-badge manages">
          manages
        </span>
      ) : null}
      <VoiceMark id={channel.id} count={inVoice} />
      <UnreadBadge id={channel.id} count={unread} />
      {items.length === 0 ? null : (
        <ActionsMenu label={`Actions for ${channel.name}`} items={items} open={menuOpen} onOpenChange={setMenuOpen} button={more} />
      )}
      {voiced?.people === null || voiced?.people === undefined ? null : (
        <VoicePeople channel={channel} people={voiced.people} names={voiceNames} viewer={viewer} onOpen={onOpen} />
      )}
      {renaming && mayRename && renderRename !== undefined
        ? renderRename(channel, () => {
            setRenaming(false);
            // The ⋯ may be gone (the grant was revoked meanwhile): the row's button then.
            (more.current ?? main.current)?.focus();
          })
        : null}
      {settingTopic && maySetTopic && renderTopic !== undefined
        ? renderTopic(channel, () => {
            setSettingTopic(false);
            (more.current ?? main.current)?.focus();
          })
        : null}
      {moving && arrange !== undefined ? (
        <MoveToGroup
          channel={channel.name}
          favorite={arrange.favorite}
          groups={arrange.groups}
          current={arrange.groupId}
          onMove={(groupId) => {
            setMoving(false);
            arrange.onMoveToGroup(groupId);
          }}
          onClose={() => {
            setMoving(false);
            (more.current ?? main.current)?.focus();
          }}
        />
      ) : null}
      {change !== null && mayManage && renderSettings !== undefined
        ? // Keyed on the change: another menu item remounts the confirmation, so
          // what the row holds and what is shown never disagree.
          (
            <Fragment key={`${change.kind}:${String(change.to)}`}>
              {renderSettings(channel, change, () => {
                setChange(null);
                (more.current ?? main.current)?.focus();
              })}
            </Fragment>
          )
        : null}
      {askingLeave ? (
        <div
          className="channel-leave-ask"
          role="group"
          aria-label={`Leave ${channel.name}?`}
          onKeyDown={(event) => {
            if (event.key !== 'Escape' || event.nativeEvent.isComposing) return;
            event.preventDefault();
            event.stopPropagation();
            closeLeave();
          }}
        >
          <p>{`Leave #${channel.name}? You will not find it again unless someone adds you back.`}</p>
          <div className="channel-settings-buttons">
            <button type="button" disabled={leaveBusy} onClick={leave}>
              Yes, leave
            </button>
            <button ref={cancelLeave} type="button" onClick={closeLeave}>
              Cancel
            </button>
          </div>
        </div>
      ) : null}
      {leaveBusy || leaveRefusal !== null ? (
        <p role="status" className={leaveRefusal === null ? 'channel-leave-line hint' : 'channel-leave-line notice'}>
          {leaveRefusal ?? 'Leaving…'}
        </p>
      ) : null}
    </li>
  );
}

// The channels as a list of buttons. Tab reaches the list once; the arrow keys
// move between channels (Home and End jump), Enter or Space opens the one
// focused. A row may carry a ⋯ menu of what the viewer can do to that channel
// (issues #1135, #1095): Invite people when `onInvite` is given, Rename channel…
// and Set topic when allowed, and Leave channel when `onLeft` is given.
// Managed channels carry a badge.
export function ChannelList({
  channels,
  selected,
  unread,
  occupancy = nobody,
  voiceNames = noNames,
  viewer = 0,
  empty = 'No channels yet: an operator needs to add you to one',
  canRename,
  renderRename,
  canSetTopic,
  renderTopic,
  canManage = false,
  renderSettings,
  onInvite,
  onLeft,
  onUnauthorized,
  onOpen,
  arrangement,
}: {
  readonly channels: readonly ChannelV0[];
  readonly selected: string | null;
  // Each channel's unread count by name; a channel at 0 has no entry.
  readonly unread: ReadonlyMap<string, number>;
  // Who is in each channel's voice, by id; nobody, no entry.
  readonly occupancy?: InVoice;
  // The names the page has for people, by principal id (issue #1103): the
  // people listed under a channel are named from it, "Member N" otherwise.
  readonly voiceNames?: ReadonlyMap<number, string>;
  // The viewer's principal id: in a list they are "You".
  readonly viewer?: number;
  // What an empty list says: by default, who can change that. Someone who can
  // make a channel is told less, with the control to do it beside the list.
  readonly empty?: string;
  // Whether this viewer may rename that channel (an operator, or its manager);
  // absent, nobody. The server still decides.
  readonly canRename?: (channel: ChannelV0) => boolean;
  // The rename form for a channel, shown in place under its row; `close`
  // hides it and returns focus to the row's ⋯.
  readonly renderRename?: (channel: ChannelV0, close: () => void) => ReactNode;
  // Whether this viewer may set that channel's topic (an operator, or its
  // manager); absent, nobody. The server still decides.
  readonly canSetTopic?: (channel: ChannelV0) => boolean;
  // The set-topic form for a channel, shown in place under its row; `close`
  // hides it and returns focus to the row's ⋯.
  readonly renderTopic?: (channel: ChannelV0, close: () => void) => ReactNode;
  // Whether this viewer holds manage_channels (whoami says so; the server still
  // decides). Only then does a row's ⋯ offer to make its channel public or
  // private, or a default, and `renderSettings` draws the confirmation or the
  // answer in place under the row (issue #1183).
  readonly canManage?: boolean;
  readonly renderSettings?: (channel: ChannelV0, change: SettingsChange, close: () => void) => ReactNode;
  // Opens the invite panel with that channel ticked; absent for a viewer who
  // may not invite, who is then offered no Invite people.
  readonly onInvite?: (channel: ChannelV0) => void;
  // Every listed channel is one the viewer belongs to. onLeft removes it after
  // the server accepts Leave; absent, the row offers no Leave channel.
  readonly onLeft?: (name: string) => void;
  readonly onUnauthorized?: () => void;
  readonly onOpen: (name: string) => void;
  // The person's favorites, groups and order (issue #1493), kept in this
  // browser. Absent, the list is drawn as the server gave it.
  readonly arrangement?: ArrangementControls;
}) {
  const buttons = useRef(new Map<string, HTMLButtonElement>());
  const list = useRef<HTMLUListElement>(null);
  // Archived channels (issue #844) are a collapsed group at the bottom. The
  // open one, if it is archived, keeps the group open so it is in sight.
  const live = channels.filter((channel) => !isArchived(channel));
  const archived = channels.filter(isArchived);
  const selectedArchived = archived.some((channel) => channel.name === selected);
  const [expanded, setExpanded] = useState(false);
  useEffect(() => {
    if (selectedArchived) setExpanded(true);
  }, [selectedArchived]);
  const arranged = arrangement?.arrangement ?? emptyArrangement;
  const layout = layoutOf(arranged, live);
  const sections = layout.favorites.length > 0 || layout.groups.length > 0;
  // What is drawn, in order: a folded group shows only the open channel, so
  // the open one stays in sight.
  const drawn = (group: { readonly group: Group; readonly channels: readonly ChannelV0[] }) => (group.group.collapsed ? group.channels.filter((c) => c.name === selected) : group.channels);
  const visible = [...layout.favorites, ...layout.groups.flatMap(drawn), ...layout.rest];
  const shown = expanded ? [...visible, ...archived] : visible;
  const indexOf = new Map(shown.map((c, i) => [c.name, i]));
  const sectionId = useId();
  const [note, setNote] = useState('');
  // The channel a line names: the line goes the moment it is not held.
  const [noteChannel, setNoteChannel] = useState<number | null>(null);
  const heldNote = noteChannel === null || channels.some((c) => c.id === noteChannel) ? note : '';
  const [noted, setNoted] = useState(false);
  const noteTimer = useRef<number | null>(null);
  function announce(text: string, channel: number | null = null) {
    setNote(text);
    setNoteChannel(channel);
    setNoted(true);
    // The line is only for the moment it is read; it must not outlive a
    // channel that is revoked meanwhile.
    if (noteTimer.current !== null) window.clearTimeout(noteTimer.current);
    noteTimer.current = window.setTimeout(() => {
      setNote('');
      noteTimer.current = null;
    }, 4000);
  }
  useEffect(
    () => () => {
      if (noteTimer.current !== null) window.clearTimeout(noteTimer.current);
    },
    [],
  );
  // The one button in the tab order: the open channel, or the first.
  const stop = selected !== null && shown.some((channel) => channel.name === selected) ? selected : (shown[0]?.name ?? null);

  // The open channel is kept in sight, however it was opened: in a long list
  // it may be off screen. jsdom has no scrollIntoView.
  useEffect(() => {
    const el = selected === null ? undefined : buttons.current.get(selected);
    if (el !== undefined && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [selected]);

  function move(event: KeyboardEvent, from: number) {
    const last = shown.length - 1;
    const to =
      event.key === 'ArrowDown' ? Math.min(from + 1, last)
      : event.key === 'ArrowUp' ? Math.max(from - 1, 0)
      : event.key === 'Home' ? 0
      : event.key === 'End' ? last
      : null;
    if (to === null) return;
    event.preventDefault();
    const target = shown[to];
    if (target !== undefined) buttons.current.get(target.name)?.focus();
  }

  function register(name: string, el: HTMLButtonElement | null) {
    if (el === null) buttons.current.delete(name);
    else buttons.current.set(name, el);
  }

  function left(name: string) {
    if (onLeft === undefined) return;
    const at = shown.findIndex((channel) => channel.name === name);
    onLeft(name);
    // The menu item and then its row go. Focus the neighbouring row, or the
    // list when the person left its last channel, rather than losing it to body.
    const next = shown[at + 1] ?? shown[at - 1];
    const target = next === undefined ? undefined : buttons.current.get(next.name);
    (target ?? list.current)?.focus();
  }

  if (channels.length === 0) {
    return <p className="empty">{empty}</p>;
  }
  const rowArrange = (channel: ChannelV0): RowArrange | undefined => {
    if (arrangement === undefined) return undefined;
    const { edit, arrangement: a } = arrangement;
    const favorite = a.favorites.includes(channel.id);
    const group = a.groups.find((g) => g.channels.includes(channel.id));
    const section = favorite || group !== undefined;
    return {
      favorite,
      section,
      canUp: section && moveWithin(a, live, channel.id, -1) !== a,
      canDown: section && moveWithin(a, live, channel.id, 1) !== a,
      groups: a.groups,
      groupId: group?.id ?? null,
      onFavorite: () => {
        edit((x) => toggleFavorite(x, channel.id));
        announce(favorite ? `${channel.name} removed from favorites` : `${channel.name} added to favorites`, channel.id);
        refocus(channel.name);
      },
      onMove: (direction) => {
        edit((x) => moveWithin(x, live, channel.id, direction));
        const where = favorite ? 'favorites' : (group?.name ?? '');
        const list = favorite ? layout.favorites : (layout.groups.find((g) => g.group.id === group?.id)?.channels ?? []);
        const from = list.findIndex((c) => c.id === channel.id);
        announce(`${channel.name} moved ${direction === -1 ? 'up' : 'down'} in ${where}, position ${String(from + direction + 1)} of ${String(list.length)}`, channel.id);
        refocus(channel.name);
      },
      onMoveToGroup: (groupId) => {
        // A folded group the channel goes into is opened, so it is in sight and
        // keeps focus.
        edit((x) => (groupId === null ? moveToGroup(x, channel.id, null) : expandGroup(moveToGroup(x, channel.id, groupId), groupId)));
        announce(groupId === null ? `${channel.name} taken out of its group` : `${channel.name} moved to ${a.groups.find((g) => g.id === groupId)?.name ?? 'the group'}`, channel.id);
        refocus(channel.name);
      },
    };
  };
  // After an arrangement change the row may have moved to another list and
  // been drawn again: focus goes to its button, not to the page.
  function refocus(name: string) {
    window.setTimeout(() => {
      buttons.current.get(name)?.focus();
    }, 0);
  }
  const row = (channel: ChannelV0) => (
    <ChannelRow
      key={channel.id}
      channel={channel}
      index={indexOf.get(channel.name) ?? 0}
      selected={selected}
      stop={stop}
      unread={unread.get(channel.name) ?? 0}
      voiced={occupancy.get(channel.id)}
      voiceNames={voiceNames}
      viewer={viewer}
      mayRename={canRename?.(channel) === true}
      renderRename={renderRename}
      maySetTopic={canSetTopic?.(channel) === true}
      renderTopic={renderTopic}
      mayManage={canManage}
      renderSettings={renderSettings}
      onInvite={onInvite}
      onLeft={onLeft === undefined ? undefined : left}
      onUnauthorized={onUnauthorized}
      register={register}
      onMove={move}
      onOpen={onOpen}
      arrange={rowArrange(channel)}
    />
  );
  return (
    <>
      {layout.favorites.length === 0 ? null : (
        <div className="channel-group" role="group" aria-label="Favorites">
          <h3 className="channel-group-head">Favorites</h3>
          <ul className="channel-list">{layout.favorites.map(row)}</ul>
        </div>
      )}
      {layout.groups.map((entry) => {
        const { group } = entry;
        const body = `${sectionId}-${group.id}`;
        return (
          <div key={group.id} className="channel-group" role="group" aria-label={group.name}>
            <h3 className="channel-group-head">
              <button
                type="button"
                aria-expanded={!group.collapsed}
                aria-controls={body}
                onClick={() => {
                  arrangement?.edit((a) => toggleCollapsed(a, group.id));
                }}
              >
                <span className="channel-group-caret" aria-hidden="true">
                  {group.collapsed ? '▸' : '▾'}
                </span>
                <span className="channel-group-name">{group.name}</span>
                <span className="channel-group-count"> ({entry.channels.length})</span>
              </button>
            </h3>
            <div id={body}>
              {drawn(entry).length > 0 ? (
                <ul className="channel-list">{drawn(entry).map(row)}</ul>
              ) : group.collapsed ? null : (
                <p className="hint">Empty. Use a channel's ⋯ menu, Move to group…, to add one.</p>
              )}
            </div>
          </div>
        );
      })}
      {layout.rest.length === 0 && sections ? null : (
        <>
          {sections ? <h3 className="channel-group-head">Channels</h3> : null}
          <ul ref={list} className="channel-list" tabIndex={-1}>
            {layout.rest.map(row)}
          </ul>
        </>
      )}
      {archived.length === 0 ? null : (
        <div className="archived-channels">
          <button
            type="button"
            className="archived-toggle"
            aria-expanded={expanded}
            aria-controls="archived-channel-list"
            onClick={() => {
              setExpanded(!expanded);
            }}
          >
            Archived ({archived.length})
          </button>
          {expanded ? (
            <ul id="archived-channel-list" className="channel-list" aria-label="Archived channels">
              {archived.map(row)}
            </ul>
          ) : null}
        </div>
      )}
      {noted ? (
        <p role="status" className="vh">
          {heldNote}
        </p>
      ) : null}
    </>
  );
}
