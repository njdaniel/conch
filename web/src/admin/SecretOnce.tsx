// A secret the page shows once, to the person who just made it (issue #1020):
// a link that signs in as someone, a credential's token, a hook's token. It is
// hidden until Reveal, never shown while this tab shares its screen, and gone
// at Done; nothing here stores it. Add a device (AddDevice.tsx) treats its link
// the same way.
import { useEffect, useId, useState } from 'react';
import { platform } from '../platform';
import { revealLimits, sharingLine, useTabSharing } from '../Reveal';

export function SecretOnce({ label, value, onDone }: { readonly label: string; readonly value: string; readonly onDone: () => void }) {
  const id = useId();
  const sharing = useTabSharing();
  const [revealed, setRevealed] = useState(false);
  const [copied, setCopied] = useState<'no' | 'yes' | 'failed'>('no');

  // A share that starts hides it at once.
  useEffect(() => {
    if (sharing) setRevealed(false);
  }, [sharing]);

  function copy() {
    try {
      void platform.services.copyText(value).then(
        () => {
          setCopied('yes');
        },
        () => {
          setCopied('failed');
        },
      );
    } catch {
      // No clipboard here: reveal it and copy it by hand.
      setCopied('failed');
    }
  }

  const shown = revealed && !sharing;
  return (
    <div className="secret-once" role="group" aria-label={label}>
      <p className="hint">{revealLimits}</p>
      {sharing ? (
        <p role="status" className="notice">
          {sharingLine}
        </p>
      ) : null}
      {shown ? (
        <input
          id={id}
          type="text"
          readOnly
          value={value}
          aria-label={label}
          onFocus={(event) => {
            event.currentTarget.select();
          }}
        />
      ) : null}
      <div className="actions">
        <button
          type="button"
          disabled={sharing}
          aria-pressed={shown}
          onClick={() => {
            setRevealed(!revealed);
          }}
        >
          {shown ? 'Hide' : 'Reveal'}
        </button>
        <button type="button" onClick={copy}>
          Copy
        </button>
        <button type="button" onClick={onDone}>
          Done
        </button>
      </div>
      {copied === 'yes' ? <p role="status">Copied.</p> : null}
      {copied === 'failed' ? (
        <p role="status" className="notice">
          Could not copy. Reveal it and copy it by hand.
        </p>
      ) : null}
    </div>
  );
}
