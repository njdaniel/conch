import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from 'react';
import { loadEmoji, searchEmoji, type Emoji, type EmojiIndex } from './emoji/emoji';

// The grid is this many columns wide at every width (the CSS says the same),
// so Up and Down move by a row without measuring anything.
export const pickerColumns = 8;

// The emoji picker (issue #763): a labelled dialog beside the box, not over the
// page. A search box, the categories, and a grid of buttons; one button is in
// the tab order and the arrow keys move between the rest. Escape closes it and
// the caller returns focus; choosing an emoji hands back its key.
export function EmojiPicker({ onPick, onClose }: { readonly onPick: (emoji: Emoji) => void; readonly onClose: () => void }) {
  const [index, setIndex] = useState<EmojiIndex | null>(null);
  const [failed, setFailed] = useState(false);
  const [query, setQuery] = useState('');
  const [group, setGroup] = useState(0);
  const [active, setActive] = useState(0);
  const search = useRef<HTMLInputElement>(null);
  const grid = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let live = true;
    loadEmoji().then(
      (loaded) => {
        if (live) setIndex(loaded);
      },
      () => {
        if (live) setFailed(true);
      },
    );
    return () => {
      live = false;
    };
  }, []);
  useEffect(() => {
    search.current?.focus();
  }, []);

  const searching = query.trim() !== '';
  const shown: readonly Emoji[] = useMemo(() => {
    if (index === null) return [];
    return searching ? searchEmoji(index, query) : (index.groups[group]?.browse ?? []);
  }, [index, query, searching, group]);

  function cells(): HTMLElement[] {
    return Array.from(grid.current?.querySelectorAll<HTMLElement>('[data-emoji]') ?? []);
  }
  function focusCell(i: number): void {
    const last = shown.length - 1;
    if (last < 0) return;
    const at = Math.max(0, Math.min(i, last));
    setActive(at);
    cells()[at]?.focus();
  }

  function gridKey(event: KeyboardEvent, i: number): void {
    let to: number | null = null;
    switch (event.key) {
      case 'ArrowRight': to = i + 1; break;
      case 'ArrowLeft': to = i - 1; break;
      case 'ArrowDown': to = i + pickerColumns; break;
      case 'ArrowUp':
        if (i < pickerColumns) {
          event.preventDefault();
          search.current?.focus();
          return;
        }
        to = i - pickerColumns;
        break;
      case 'Home': to = 0; break;
      case 'End': to = shown.length - 1; break;
      default: return;
    }
    event.preventDefault();
    focusCell(to);
  }

  function dialogKey(event: KeyboardEvent): void {
    if (event.key !== 'Escape') return;
    event.preventDefault();
    event.stopPropagation();
    onClose();
  }

  const heading = searching ? 'Search results' : (index?.groups[group]?.name ?? 'Emoji');

  return (
    <div className="emoji-picker" role="dialog" aria-label="Emoji" onKeyDown={dialogKey}>
      <div className="emoji-controls">
        <input
          ref={search}
          type="search"
          aria-label="Search emoji"
          placeholder="Search emoji"
          autoComplete="off"
          value={query}
          onChange={(event) => {
            setQuery(event.target.value);
            setActive(0);
          }}
          onKeyDown={(event) => {
            if (event.key === 'ArrowDown' && shown.length > 0) {
              event.preventDefault();
              focusCell(active);
            }
          }}
        />
        <select
          aria-label="Category"
          value={group}
          disabled={searching || index === null}
          onChange={(event) => {
            setGroup(Number(event.target.value));
            setActive(0);
          }}
        >
          {(index?.groups ?? []).map((g, i) => (
            <option key={g.name} value={i}>
              {g.name}
            </option>
          ))}
        </select>
        <button type="button" className="emoji-close" aria-label="Close emoji picker" onClick={onClose}>
          Close
        </button>
      </div>
      {failed ? <p role="alert" className="notice">The emoji list could not be loaded.</p> : null}
      {index === null && !failed ? <p role="status" className="hint">Loading emoji…</p> : null}
      {index !== null && shown.length === 0 ? <p role="status" className="hint">{searching ? 'No emoji match.' : 'Nothing here.'}</p> : null}
      {shown.length > 0 ? (
        <div ref={grid} className="emoji-grid" role="group" aria-label={heading}>
          {shown.map((e, i) => (
            <button
              key={e.key}
              type="button"
              className="emoji-cell"
              data-emoji=""
              tabIndex={i === Math.min(active, shown.length - 1) ? 0 : -1}
              aria-label={e.name}
              title={`${e.name} :${e.shortcode}:`}
              onKeyDown={(event) => { gridKey(event, i); }}
              onFocus={() => { setActive(i); }}
              onClick={() => { onPick(e); }}
            >
              {e.key}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
