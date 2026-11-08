import { useCallback, useEffect, useId, useRef, useState, type KeyboardEvent } from 'react';
import { Avatar } from './Icons';
import { IdentityMarks } from './Identity';
import type { IdentityInput } from './identity';
import type { Entry } from './agents/join';
import type { Person } from './useLiveChannel';

const openedEvent = 'conch-profile-card-open';

type ProfilePerson = (Person | Entry) & IdentityInput;

function present(person: ProfilePerson): person is Entry & IdentityInput {
  return 'state' in person && 'live' in person && typeof person.live === 'number';
}

// ProfileCard opens a read-only, non-modal disclosure from a person's name.
// Every value comes from the opening surface; an Entry alone adds its
// already-held presence. Controlled open state lets a member row move between
// presence groups without closing the card.
export function ProfileCard({ id, person, viewer, name: surfaceName, className = '', open, onOpenChange, onDisappear }: {
  readonly id: number;
  readonly person: ProfilePerson | undefined;
  readonly viewer?: number;
  readonly name?: string;
  readonly className?: string;
  readonly open?: boolean;
  readonly onOpenChange?: (open: boolean) => void;
  // The owning surface names a focus target that outlives a disappearing row.
  readonly onDisappear?: () => void;
}) {
  const [ownOpen, setOwnOpen] = useState(false);
  const shown = open ?? ownOpen;
  const opener = useRef<HTMLButtonElement>(null);
  const card = useRef<HTMLDivElement>(null);
  const closeButton = useRef<HTMLButtonElement>(null);
  const openNow = useRef(shown);
  openNow.current = shown;
  const disappear = useRef(onDisappear);
  disappear.current = onDisappear;
  const hadPerson = useRef(person !== undefined);
  const key = useId();
  const titleId = `${key}-name`;
  const cardId = `${key}-card`;
  const name = surfaceName ?? (person === undefined || person.name.trim() === '' ? `#${String(id)}` : person.name);
  const shownPerson: ProfilePerson = person ?? { name, kind: null };

  const change = useCallback((next: boolean) => {
    if (open === undefined) setOwnOpen(next);
    onOpenChange?.(next);
  }, [open, onOpenChange]);
  const close = useCallback(() => {
    if (!openNow.current) return;
    openNow.current = false;
    change(false);
    opener.current?.focus();
  }, [change]);
  const show = () => {
    if (shown) {
      close();
      return;
    }
    openNow.current = true;
    change(true);
    document.dispatchEvent(new CustomEvent(openedEvent, { detail: key }));
  };

  useEffect(() => {
    if (!shown) return;
    closeButton.current?.focus();
    const outside = (event: Event) => {
      const target = event.target;
      if (target instanceof Node && (card.current?.contains(target) === true || opener.current?.contains(target) === true)) return;
      close();
    };
    const another = (event: Event) => {
      if (event instanceof CustomEvent && event.detail !== key) close();
    };
    document.addEventListener('pointerdown', outside);
    document.addEventListener('mousedown', outside);
    document.addEventListener('touchstart', outside);
    document.addEventListener(openedEvent, another);
    return () => {
      document.removeEventListener('pointerdown', outside);
      document.removeEventListener('mousedown', outside);
      document.removeEventListener('touchstart', outside);
      document.removeEventListener(openedEvent, another);
    };
  }, [shown, close, key]);

  useEffect(() => {
    if (hadPerson.current && person === undefined) close();
    hadPerson.current = person !== undefined;
  }, [person, close]);

  useEffect(() => () => {
    if (openNow.current) disappear.current?.();
  }, []);

  function keyDown(event: KeyboardEvent<HTMLDivElement>) {
    if (event.nativeEvent.isComposing) return;
    if (event.key !== 'Escape' && event.key !== 'Tab') return;
    event.preventDefault();
    event.stopPropagation();
    close();
  }

  return (
    <span className={`profile-card-disclosure ${className}`.trim()}>
      <button
        ref={opener}
        type="button"
        className="profile-card-name"
        aria-haspopup="dialog"
        aria-expanded={shown}
        {...(shown ? { 'aria-controls': cardId } : {})}
        onClick={(event) => {
          // A recognised long press has already opened the row's menu. Its
          // following click must not open a card and take focus from the menu.
          const menu = event.currentTarget.closest('li')?.querySelector('[role="menu"]');
          if (menu !== null && menu !== undefined) return;
          show();
        }}
      >
        {name}
      </button>
      {shown ? (
        <div ref={card} id={cardId} role="dialog" aria-modal="false" aria-labelledby={titleId} className="profile-card" onKeyDown={keyDown}>
          <div className="profile-card-person">
            <Avatar id={id} name={name} />
            <div>
              <strong id={titleId}>{name}</strong>
              {shownPerson.kind === 'human' || shownPerson.kind === 'agent' ? (
                <span className="profile-card-kind">{shownPerson.kind === 'agent' ? 'Agent' : 'Person'}</span>
              ) : null}
            </div>
          </div>
          <div className="profile-card-marks">
            <IdentityMarks {...shownPerson} id={id} viewer={viewer} />
          </div>
          {present(shownPerson) ? (
            <p className="profile-card-presence">
              {shownPerson.state}{shownPerson.live > 1 ? ` (${String(shownPerson.live)} sessions)` : ''}
            </p>
          ) : null}
          <button ref={closeButton} type="button" className="profile-card-close" onClick={close}>Close</button>
        </div>
      ) : null}
    </span>
  );
}
