// A message's actions menu (issue #845, docs/design/message-editing.md §13.1):
// one menu, opened four ways. A visible "More actions" (⋯) button, Enter or
// Space on it when focused, right-click on the message, and a long press on
// a touch screen. Escape and Tab close it, and Escape puts focus back on the
// button; an outside press closes it where the press was; choosing an item
// closes it and hands over to the item.
//
// It is kept small and free of any one message's business so that the
// permission-aware context menu of issue #972 can adopt it: the caller says
// which items there are (only those the person may use), and an item list
// that is empty draws nothing at all.
import { useEffect, useId, useRef, type KeyboardEvent, type RefObject, type TouchEvent } from 'react';

export interface MenuItem {
  // Unique within the menu.
  readonly key: string;
  readonly label: string;
  readonly onSelect: () => void;
  // Shown, and announced, as unavailable for now (a request is pending);
  // choosing it does nothing and leaves the menu open. Absent means usable.
  readonly disabled?: boolean;
}

// How long a finger rests on a message before its menu opens.
export const longPressMs = 500;

// How far a finger may move during a long press, in CSS pixels, before it is
// taken for a scroll.
const slop = 10;

export function ActionsMenu({
  label,
  items,
  open,
  onOpenChange,
  button,
}: {
  // The button's and the menu's accessible name ("More actions for message 12").
  readonly label: string;
  readonly items: readonly MenuItem[];
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly button: RefObject<HTMLButtonElement | null>;
}) {
  const menu = useRef<HTMLDivElement>(null);
  const id = useId();
  const change = useRef(onOpenChange);
  change.current = onOpenChange;

  // On opening, the first item has focus (the menu button pattern).
  useEffect(() => {
    if (open) menu.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
  }, [open]);

  // A press anywhere but the menu and its button closes it. Focus stays where
  // the press put it.
  useEffect(() => {
    if (!open) return;
    const outside = (event: Event) => {
      const target = event.target;
      if (target instanceof Node && (menu.current?.contains(target) === true || button.current?.contains(target) === true)) return;
      change.current(false);
    };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('mousedown', outside);
    document.addEventListener('touchstart', outside);
    return () => {
      document.removeEventListener('pointerdown', outside);
      document.removeEventListener('mousedown', outside);
      document.removeEventListener('touchstart', outside);
    };
  }, [open, button]);

  if (items.length === 0) return null;

  function key(event: KeyboardEvent<HTMLDivElement>) {
    if (event.nativeEvent.isComposing) return;
    const entries = Array.from(menu.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? []);
    const at = entries.findIndex((entry) => entry === document.activeElement);
    let to: number | null = null;
    switch (event.key) {
      case 'Escape':
        event.preventDefault();
        event.stopPropagation();
        onOpenChange(false);
        button.current?.focus();
        return;
      case 'Tab':
        onOpenChange(false);
        return;
      case 'ArrowDown':
        to = at + 1 >= entries.length ? 0 : at + 1;
        break;
      case 'ArrowUp':
        to = at <= 0 ? entries.length - 1 : at - 1;
        break;
      case 'Home':
        to = 0;
        break;
      case 'End':
        to = entries.length - 1;
        break;
      default:
        return;
    }
    event.preventDefault();
    entries[to]?.focus();
  }

  return (
    <span className="message-menu">
      <button
        ref={button}
        type="button"
        className="more-actions"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        {...(open ? { 'aria-controls': id } : {})}
        onClick={() => {
          onOpenChange(!open);
        }}
      >
        <span aria-hidden="true">⋯</span>
      </button>
      {open ? (
        <div ref={menu} id={id} role="menu" aria-label={label} className="actions-menu" onKeyDown={key}>
          {items.map((item) => (
            <button
              key={item.key}
              type="button"
              role="menuitem"
              tabIndex={-1}
              {...(item.disabled === true ? { 'aria-disabled': true } : {})}
              onClick={() => {
                if (item.disabled === true) return;
                onOpenChange(false);
                item.onSelect();
              }}
            >
              {item.label}
            </button>
          ))}
        </div>
      ) : null}
    </span>
  );
}

// useOpeners gives a message the other two ways in: right-click, with the
// browser's own menu suppressed on that message only (and never over a text
// box, a link or an image, whose own menus people use), and a long press.
// Android opens the context menu on a long press itself; iOS does not, so
// the timer is the way there. Both only ever open.
export function useOpeners(enabled: boolean, open: () => void) {
  const timer = useRef<number | null>(null);
  const start = useRef<{ readonly x: number; readonly y: number } | null>(null);
  useEffect(() => () => {
    if (timer.current !== null) window.clearTimeout(timer.current);
  }, []);
  const cancel = () => {
    if (timer.current !== null) window.clearTimeout(timer.current);
    timer.current = null;
    start.current = null;
  };
  const own = (target: EventTarget) => !(target instanceof Element && target.closest('textarea, input, a, img, [role="menu"], [role="dialog"]') !== null);
  if (!enabled) return {};
  return {
    onContextMenu: (event: { readonly target: EventTarget; preventDefault: () => void }) => {
      if (!own(event.target)) return;
      event.preventDefault();
      cancel();
      open();
    },
    onTouchStart: (event: TouchEvent) => {
      const touch = event.touches[0];
      if (event.touches.length !== 1 || touch === undefined || !own(event.target)) return;
      cancel();
      start.current = { x: touch.clientX, y: touch.clientY };
      timer.current = window.setTimeout(() => {
        timer.current = null;
        start.current = null;
        open();
      }, longPressMs);
    },
    onTouchMove: (event: TouchEvent) => {
      const touch = event.touches[0];
      const from = start.current;
      if (touch === undefined || from === null) return;
      if (Math.abs(touch.clientX - from.x) > slop || Math.abs(touch.clientY - from.y) > slop) cancel();
    },
    onTouchEnd: cancel,
    onTouchCancel: cancel,
  };
}
