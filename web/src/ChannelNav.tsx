import { createPortal } from 'react-dom';
import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import * as api from './api';
import { isArchived } from './archived';
import { matchChannels, nextUnread, sidebarOrder, stepChannel } from './channelOrder';
import { Glyph, PrivateMark } from './Icons';
import type { ChannelListingV1, ChannelV0 } from './schema/channel';
import { badgeText } from './useReadState';
import { isTyping } from './voice/keys';

// The quick channel switcher and channel keyboard shortcuts (issue #1492).
//
//   Ctrl+K / Cmd+K         open the switcher (also "Find a channel", always visible)
//   Alt+Down / Alt+Up      next / previous channel in sidebar order
//   Alt+Shift+Down / Up    next / previous channel with unread messages
//
// The shortcuts never fire in a text field or editor, during IME composition,
// or while another dialog is open. Everything here only opens a channel the
// caller already lists (or asks the directory to show a row): it never joins a
// channel, joins voice, or touches the microphone. What it searches is the
// viewer's own channel list as the page holds it, so a channel lost while it is
// open leaves the list, and the public channels to join are read from the
// directory each time it opens and dropped when it closes or the identity
// changes.

const statusMs = 4000;
const directoryPollMs = 3000;

function isMac(): boolean {
  return typeof navigator !== 'undefined' && /mac|iphone|ipad/i.test(navigator.platform);
}

// Whether another dialog (settings, the reauth prompt, the image viewer, a
// picker) is open: the shortcuts leave the page alone then.
function dialogOpen(): boolean {
  // The settings panel is always in the page, hidden until opened.
  return [...document.querySelectorAll('[role="dialog"], [aria-modal="true"], dialog[open]')].some((el) => el.closest('[hidden], [inert]') === null);
}

function composing(event: globalThis.KeyboardEvent): boolean {
  return event.isComposing;
}

// The message composer is the one text field the shortcuts work in: it is where
// people usually are, and none of these keys type anything there. Every other
// field (search, the switcher's input, forms, edit in place) keeps its keys.
function inComposer(target: EventTarget | null): boolean {
  return target instanceof HTMLElement && target.id === 'composer-text';
}

interface Item {
  readonly key: string;
  readonly name: string;
  readonly join: boolean;
  readonly isPrivate: boolean;
  readonly archived: boolean;
  readonly unread: number;
}

export function ChannelNav({
  channels,
  unread,
  selected,
  me,
  refresh = 0,
  onOpen,
  onBrowse,
}: {
  readonly channels: readonly ChannelV0[];
  readonly unread: ReadonlyMap<string, number>;
  readonly selected: string | null;
  // The signed-in principal: when it changes the switcher closes and forgets.
  readonly me: number;
  // Bumped whenever the page learns the directory or its own permissions changed
  // (channels_changed, a reconnect): the Join group is read again.
  readonly refresh?: number;
  readonly onOpen: (name: string) => void;
  // Shows a public channel's row in the directory. Absent for a viewer who has
  // no directory: no Join group is offered then and none is read.
  readonly onBrowse?: (name: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [activeKey, setActiveKey] = useState<string | null>(null);
  const [rows, setRows] = useState<readonly ChannelListingV1[]>([]);
  const [line, setLine] = useState('');
  const opener = useRef<HTMLElement | null>(null);
  const entry = useRef<HTMLButtonElement>(null);
  const field = useRef<HTMLInputElement>(null);
  const seq = useRef(0);
  const timer = useRef<number | null>(null);
  const id = useId();
  const canBrowse = onBrowse !== undefined;
  const latest = useRef({ channels, unread, selected, onOpen, open });
  latest.current = { channels, unread, selected, onOpen, open };

  function show(text: string) {
    setLine(text);
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => {
      setLine('');
      timer.current = null;
    }, statusMs);
  }
  useEffect(
    () => () => {
      if (timer.current !== null) window.clearTimeout(timer.current);
    },
    [],
  );

  function openSwitcher() {
    const at = document.activeElement;
    opener.current = at instanceof HTMLElement && at !== document.body ? at : entry.current;
    setQuery('');
    setActiveKey(null);
    setOpen(true);
  }

  function close() {
    setOpen(false);
    const back = opener.current;
    opener.current = null;
    if (back !== null && back.isConnected) back.focus();
    else entry.current?.focus();
  }

  useEffect(() => {
    if (open) field.current?.focus();
  }, [open]);

  // A different person, or a signed-out page: nothing of the last one stays.
  const lastMe = useRef(me);
  useEffect(() => {
    if (lastMe.current === me) return;
    lastMe.current = me;
    seq.current++;
    setOpen(false);
    setQuery('');
    setActiveKey(null);
    setRows([]);
    setLine('');
  }, [me]);

  // The directory rows, read when it opens, dropped when it closes.
  useEffect(() => {
    if (!open || !canBrowse) {
      seq.current++;
      setRows([]);
      return;
    }
    // The server pushes nothing to a non-member when a public channel turns
    // private or is archived, so while the switcher is open the directory is
    // read again a few seconds after each read finishes, as well as on every
    // refresh signal. A read that answers after a refresh, a close or another
    // person began is dropped; the next poll waits for the last one, so a slow
    // directory still lands.
    let poll = 0;
    let live = true;
    const read = () => {
      const mine = ++seq.current;
      void api.channelDirectory().then((result) => {
        if (mine === seq.current && result.ok) setRows(result.value);
      }).finally(() => {
        if (live && mine === seq.current) poll = window.setTimeout(read, directoryPollMs);
      });
    };
    read();
    return () => {
      live = false;
      window.clearTimeout(poll);
      seq.current++;
    };
  }, [open, canBrowse, refresh, channels]);

  // The shortcuts.
  useEffect(() => {
    function onKey(event: globalThis.KeyboardEvent) {
      if (composing(event) || (isTyping(event.target) && !inComposer(event.target)) || dialogOpen()) return;
      const now = latest.current;
      if ((event.ctrlKey || event.metaKey) && !event.altKey && !event.shiftKey && event.key.toLowerCase() === 'k') {
        event.preventDefault();
        openSwitcher();
        return;
      }
      if (!event.altKey || event.ctrlKey || event.metaKey) return;
      if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
      event.preventDefault();
      const direction = event.key === 'ArrowDown' ? 1 : -1;
      if (event.shiftKey) {
        const to = nextUnread(now.channels, now.unread, now.selected, direction);
        if (to === null) show('No unread channels');
        else now.onOpen(to);
        return;
      }
      const to = stepChannel(
        now.channels.filter((c) => !isArchived(c)),
        now.selected,
        direction,
      );
      if (to !== null) now.onOpen(to);
    }
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('keydown', onKey);
    };
  }, []);

  const mine: Item[] = matchChannels(channels, query).map((c) => ({
    key: `c:${c.name}`,
    name: c.name,
    join: false,
    isPrivate: c.visibility === 'private',
    archived: isArchived(c),
    unread: unread.get(c.name) ?? 0,
  }));
  const q = query.trim().toLowerCase();
  const held = new Set(sidebarOrder(channels).map((c) => c.name));
  const joinable: Item[] = rows
    .filter((r) => !r.joined && r.visibility === 'public' && !held.has(r.name) && r.archived_at === undefined && r.name.toLowerCase().includes(q))
    .map((r) => ({ key: `j:${r.name}`, name: r.name, join: true, isPrivate: false, archived: false, unread: 0 }));
  const items = [...mine, ...joinable];
  // A highlighted row that left the list (a channel revoked while open) falls
  // back to the first.
  const active = items.find((i) => i.key === activeKey) ?? items[0];
  const optionId = (item: Item) => `${id}-${item.key.replace(/[^a-z0-9-]/gi, '_')}`;

  function choose(item: Item) {
    setOpen(false);
    opener.current = null;
    if (item.join) {
      onBrowse?.(item.name);
      return;
    }
    onOpen(item.name);
    // Focus follows the choice, so it is never left on the page's body: to the
    // conversation heading, where the page puts it when a channel opens.
    window.setTimeout(() => {
      document.getElementById('conversation-heading')?.focus();
    }, 0);
  }

  const activeId = active === undefined ? undefined : optionId(active);
  useEffect(() => {
    if (!open || activeId === undefined) return;
    const el = document.getElementById(activeId);
    if (el !== null && typeof el.scrollIntoView === 'function') el.scrollIntoView({ block: 'nearest' });
  }, [open, activeId]);

  function onDialogKey(event: KeyboardEvent) {
    if (event.nativeEvent.isComposing) return;
    const index = active === undefined ? -1 : items.indexOf(active);
    const at = (to: number) => {
      const target = items[(to + items.length) % items.length];
      if (target !== undefined) setActiveKey(target.key);
    };
    switch (event.key) {
      case 'Escape':
        event.preventDefault();
        event.stopPropagation();
        close();
        break;
      case 'ArrowDown':
        event.preventDefault();
        at(index + 1);
        break;
      case 'ArrowUp':
        event.preventDefault();
        at(index - 1);
        break;
      case 'Home':
        event.preventDefault();
        at(0);
        break;
      case 'End':
        event.preventDefault();
        at(items.length - 1);
        break;
      case 'Enter':
        event.preventDefault();
        if (active !== undefined) choose(active);
        break;
      case 'Tab':
        event.preventDefault();
        break;
    }
  }

  const option = (item: Item) => (
    <li
      key={item.key}
      id={optionId(item)}
      role="option"
      aria-selected={item === active}
      className={item === active ? 'switcher-option active' : 'switcher-option'}
      onMouseDown={(event) => {
        event.preventDefault();
      }}
      onClick={() => {
        choose(item);
      }}
    >
      <span className="switcher-name">{item.name}</span>
      {item.isPrivate ? <PrivateMark /> : null}
      {item.archived ? <span className="ag-badge">Archived</span> : null}
      {item.join ? <span className="hint">Opens in the directory</span> : null}
      {badgeText(item.unread) === null ? null : (
        <span className="unread" role="img" aria-label={`${badgeText(item.unread) ?? ''} unread`}>
          {badgeText(item.unread)}
        </span>
      )}
    </li>
  );
  const listId = `${id}-list`;
  const shortcut = isMac() ? '⌘K' : 'Ctrl+K';
  return (
    <div className="channel-nav">
      <button ref={entry} type="button" className="quiet find-channel" aria-keyshortcuts="Control+K Meta+K" onClick={openSwitcher}>
        <Glyph name="channels" />
        <span>Find a channel</span>
        <kbd aria-hidden="true">{shortcut}</kbd>
      </button>
      <p role="status" className="nav-status hint">
        {line}
      </p>
      {open
        ? createPortal(
        <div
          className="switcher-backdrop"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget) close();
          }}
        >
          <div role="dialog" aria-modal="true" aria-label="Find a channel" className="switcher" onKeyDown={onDialogKey}>
            <input
              ref={field}
              type="text"
              role="combobox"
              aria-label="Find a channel"
              aria-expanded="true"
              aria-controls={listId}
              aria-autocomplete="list"
              {...(active === undefined ? {} : { 'aria-activedescendant': optionId(active) })}
              autoComplete="off"
              spellCheck={false}
              placeholder="Channel name"
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                setActiveKey(null);
              }}
            />
            <div id={listId} role="listbox" aria-label="Channels" className="switcher-list">
              {items.length === 0 ? (
                <p className="empty">No channels match.</p>
              ) : (
                <>
                  {mine.length === 0 ? null : (
                    <ul role="group" aria-label="Your channels">
                      {mine.map(option)}
                    </ul>
                  )}
                  {joinable.length === 0 ? null : (
                    <ul role="group" aria-label="Join">
                      {joinable.map(option)}
                    </ul>
                  )}
                </>
              )}
            </div>
            <p className="hint switcher-help">Enter opens. Esc closes. Alt+Up and Alt+Down move between channels; add Shift for unread.</p>
          </div>
        </div>
        ,
            document.body,
          )
        : null}
    </div>
  );
}
