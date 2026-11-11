// The share quality chooser beside Share screen (issue #1659): a menu button
// that opens a small menu of the same choices as Voice options' selects, the
// current one checked. Share screen alone still shares at once at the last
// choice; this is for choosing at the moment of sharing, Auto included.
//
// Keyboard, as a menu button: Enter, Space or Down on the button opens the
// menu at the checked resolution; Up and Down move among the choices, Home
// and End go to the first and last; Enter or Space chooses (the menu stays,
// for a frame rate next); Escape closes it and returns focus to the button;
// Tab leaves it.
import { useEffect, useRef, useState, type KeyboardEvent as KeyEvent } from 'react';
import { shareEstimateMbps } from './sdk';
import { autoFrameRate, type ShareFrameRate, type ShareQuality, type ShareResolution } from './share';

export const shareResolutions: readonly ShareResolution[] = ['auto', '720p', '1080p', 'source'];
export const shareFrameRates: readonly ShareFrameRate[] = [15, 30, 60];

// What each resolution is called. Source is the capture's own size, and
// shareAsk asks for up to 3840 by 2160 of it.
export const resolutionWords: Readonly<Record<ShareResolution, string>> = {
  auto: 'Auto',
  '720p': '720p',
  '1080p': '1080p',
  source: 'Source (up to 4K)',
};

// What Auto does, in a line (Q10 on #1614).
export const autoNote = 'Auto shares a screen up to 1920 px across as it is, and a larger one at 1080p, at 30 fps.';

// The estimate beside a resolution: the sharer's uplink, labelled as an
// estimate, because it comes from the layers' bitrate rows and not from a
// measurement (#1513).
export function estimateWords(quality: ShareQuality): string {
  return `about ${String(shareEstimateMbps(quality))} Mbps up (estimate)`;
}

// The caution for the one choice most computers and browsers cannot encode.
export const sourceSixtyNote = "Source at 60 fps is 4K at 60 on a large screen. Many computers and browsers can't encode that and will send less; the Sending line shows what was actually sent.";

// cautionFor is the one-line note a choice earns, or null.
export function cautionFor(quality: ShareQuality): string | null {
  return quality.resolution === 'source' && quality.frameRate === 60 ? sourceSixtyNote : null;
}

// The estimate is per resolution: the frame rate does not change the bitrate
// rows the figure comes from.
function estimateFor(resolution: ShareResolution, frameRate: ShareFrameRate): string {
  return estimateWords({ resolution, frameRate: resolution === 'auto' ? autoFrameRate : frameRate });
}

export function ShareQualityMenu({ quality, onChoose, disabled }: {
  readonly quality: ShareQuality;
  readonly onChoose: (next: ShareQuality) => void;
  readonly disabled?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const root = useRef<HTMLDivElement>(null);
  // Focus moves into the menu only when it was opened from the keyboard or
  // the button, never on a click outside.
  const [focusFirst, setFocusFirst] = useState<'checked' | null>(null);
  const caution = cautionFor(quality);
  const auto = quality.resolution === 'auto';

  const items = (): HTMLButtonElement[] => Array.from(menu.current?.querySelectorAll<HTMLButtonElement>('button[role^="menuitem"]:not(:disabled)') ?? []);

  useEffect(() => {
    if (!open || focusFirst === null) return;
    const all = items();
    (all.find((b) => b.getAttribute('aria-checked') === 'true') ?? all[0])?.focus();
    setFocusFirst(null);
  }, [open, focusFirst]);

  // A click or focus outside closes the menu without moving focus.
  useEffect(() => {
    if (!open) return;
    const away = (e: Event) => {
      if (e.target instanceof Node && root.current?.contains(e.target) !== true) setOpen(false);
    };
    document.addEventListener('mousedown', away);
    document.addEventListener('focusin', away);
    return () => {
      document.removeEventListener('mousedown', away);
      document.removeEventListener('focusin', away);
    };
  }, [open]);

  const close = (returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) button.current?.focus();
  };

  const onButtonKey = (e: KeyEvent<HTMLButtonElement>) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      setOpen(true);
      setFocusFirst('checked');
    }
  };

  const onMenuKey = (e: KeyEvent<HTMLDivElement>) => {
    const all = items();
    const at = all.indexOf(document.activeElement as HTMLButtonElement);
    const go = (i: number) => {
      e.preventDefault();
      all[(i + all.length) % all.length]?.focus();
    };
    switch (e.key) {
      case 'ArrowDown': go(at + 1); break;
      case 'ArrowUp': go(at <= 0 ? all.length - 1 : at - 1); break;
      case 'Home': go(0); break;
      case 'End': go(all.length - 1); break;
      case 'Escape':
        e.preventDefault();
        e.stopPropagation();
        close(true);
        break;
      case 'Tab': close(false); break;
    }
  };

  return (
    <div className="share-chooser" ref={root}>
      <button
        ref={button}
        type="button"
        className="share-chooser-button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls="share-chooser-menu"
        aria-label="Share quality"
        disabled={disabled}
        onClick={() => {
          setOpen(!open);
          setFocusFirst(open ? null : 'checked');
        }}
        onKeyDown={onButtonKey}
      >
        <span aria-hidden="true">▾</span>
      </button>
      <div id="share-chooser-menu" ref={menu} className="share-chooser-menu" role="menu" aria-label="Share quality" hidden={!open} onKeyDown={onMenuKey}>
        <div role="group" aria-label="Resolution">
          {shareResolutions.map((resolution) => (
            <button
              key={resolution}
              type="button"
              role="menuitemradio"
              aria-checked={quality.resolution === resolution}
              tabIndex={-1}
              onClick={() => { onChoose({ resolution, frameRate: resolution === 'auto' ? autoFrameRate : quality.frameRate }); }}
            >
              <span className="item-text">{resolutionWords[resolution]}</span>
              <span className="item-hint">{estimateFor(resolution, quality.frameRate)}</span>
            </button>
          ))}
        </div>
        <div role="group" aria-label="Frame rate">
          {shareFrameRates.map((frameRate) => (
            <button
              key={frameRate}
              type="button"
              role="menuitemradio"
              aria-checked={quality.frameRate === frameRate}
              disabled={auto}
              tabIndex={-1}
              onClick={() => { onChoose({ resolution: quality.resolution, frameRate }); }}
            >
              <span className="item-text">{String(frameRate)} fps</span>
            </button>
          ))}
        </div>
        {auto ? <p className="hint">{autoNote}</p> : null}
        {caution === null ? null : <p className="hint" role="note">{caution}</p>}
      </div>
    </div>
  );
}
