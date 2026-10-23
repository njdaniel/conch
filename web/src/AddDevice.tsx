// Add a device (issue #714, design §6.2): a member makes a one-use link that
// signs their own account in on another device, inside "Your sign-ins". The
// server asks for a recent proof of the password first, which the page asks
// for (Reauth.tsx) and then makes the link. The link works once, for ten
// minutes, and at most three are open at a time.
//
// The link is shown once, to the person who made it, and only on Reveal:
// it is kept in memory here (never in the page's state, storage or a log),
// hidden again when this tab starts sharing its screen, and refused while it
// shares. The page cannot know what else may be watching the screen, and
// says so beside it (Reveal.tsx). Copy puts it on the clipboard without
// showing it. Done, or closing the section, forgets it here; the open links
// are listed, without their codes, and Close ends one so it signs nobody in.
// The device that opens it is asked to confirm whose account it signs into,
// and to sign out whoever is signed in there first (SignInLink.tsx).
import { useCallback, useEffect, useId, useRef, useState, type SyntheticEvent } from 'react';
import * as api from './api';
import { waitWords } from './Invite';
import { revealLimits, sharingLine, useTabSharing } from './Reveal';
import { deviceLink } from './route';
import type { SignInLinkV1 } from './schema/signin';
import { insecureLine } from './transport';

type Line = { readonly text: string; readonly refused: boolean };

// makeLine is the plain words for a link the server would not make.
function makeLine(result: { readonly failure: api.ApiFailure; readonly code?: string; readonly retryAfter?: number }): string {
  if (result.failure === 'network') return 'Cannot reach the server. No link was made; try again.';
  switch (result.code) {
    case 'too_many_device_links':
      return 'You already have as many open device links as you may. Close one below first.';
    case 'reauth_required':
      return 'No link was made: it needs your password first.';
    case 'rate_limited':
      return `You have made too many links for now. Try again in ${waitWords(result.retryAfter)}.`;
    case 'guest_not_allowed':
      return 'A guest visit cannot add a device. Create an account first.';
    default:
      return 'The server refused. No link was made.';
  }
}

const clock = (at: string) => new Date(at).toLocaleTimeString();

// insecure is a page served over plain HTTP beyond this machine (#713's
// rule): no link is made there, since making one asks for the password.
export function AddDevice({ origin, insecure, onUnauthorized }: { readonly origin: string; readonly insecure: boolean; readonly onUnauthorized: () => void }) {
  const id = useId();
  const sharing = useTabSharing();
  const [label, setLabel] = useState('');
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<Line | null>(null);
  // The link just made: what the server keeps of it here, and its address,
  // with the code in it, in a ref alone.
  const [made, setMade] = useState<SignInLinkV1 | null>(null);
  const address = useRef<string | null>(null);
  const [revealed, setRevealed] = useState(false);
  const [copied, setCopied] = useState<'no' | 'yes' | 'failed'>('no');
  const [links, setLinks] = useState<readonly SignInLinkV1[] | null>(null);
  const [listLine, setListLine] = useState<string | null>(null);

  const refresh = useCallback(() => {
    void api.listDeviceLinks().then((result) => {
      if (result.ok) {
        setLinks(result.value);
        setListLine(null);
      } else if (result.failure === 'unauthorized') onUnauthorized();
      else setListLine('Could not read your open device links.');
    });
  }, [onUnauthorized]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // A share that starts hides the link at once.
  useEffect(() => {
    if (sharing) setRevealed(false);
  }, [sharing]);

  // Leaving forgets the link here.
  useEffect(
    () => () => {
      address.current = null;
    },
    [],
  );

  function make(event: SyntheticEvent) {
    event.preventDefault();
    if (busy) return;
    setBusy(true);
    setLine(null);
    void api.createDeviceLink(label.trim()).then((result) => {
      setBusy(false);
      if (result.ok) {
        address.current = deviceLink(origin, result.value.linkCode);
        setMade(result.value.link);
        setRevealed(false);
        setCopied('no');
        setLabel('');
        refresh();
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setLine({ text: makeLine(result), refused: true });
    });
  }

  function copy() {
    const link = address.current;
    if (link === null) return;
    try {
      void navigator.clipboard.writeText(link).then(
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

  function done() {
    address.current = null;
    setMade(null);
    setRevealed(false);
    setCopied('no');
    refresh();
  }

  function close(link: SignInLinkV1) {
    void api.closeDeviceLink(link.id).then((result) => {
      if (result.ok || (result.failure !== 'network' && result.code === 'sign_in_link_not_found')) {
        if (made?.id === link.id) done();
        else refresh();
        setListLine(null);
        return;
      }
      if (result.failure === 'unauthorized') onUnauthorized();
      else setListLine(result.failure === 'network' ? 'Cannot reach the server. The link is still open; try again.' : 'Could not close that link.');
    });
  }

  const shown = revealed && !sharing ? address.current : null;

  return (
    <section className="add-device" aria-labelledby={`${id}-heading`}>
      <h3 id={`${id}-heading`}>Add a device</h3>
      <p className="hint">
        Makes a link that signs this account in on another device: open it there. It works once, for ten minutes. You may
        be asked for your password first.
      </p>
      {insecure && made === null ? (
        <p className="notice insecure">{insecureLine}</p>
      ) : made === null ? (
        <form onSubmit={make}>
          <label htmlFor={`${id}-label`}>Name for the device (optional)</label>
          <input
            id={`${id}-label`}
            type="text"
            value={label}
            maxLength={100}
            autoComplete="off"
            onChange={(event) => {
              setLabel(event.target.value);
            }}
          />
          <button type="submit" aria-busy={busy}>
            {busy ? 'Making the link…' : 'Make a device link'}
          </button>
        </form>
      ) : (
        <div role="group" aria-label="New device link" className="device-link">
          <p>
            <strong>Link made.</strong> It signs into your account once, until{' '}
            <time dateTime={made.expires_at}>{clock(made.expires_at)}</time>. It is shown only here, and only now.
          </p>
          <p className="hint">{revealLimits}</p>
          {sharing ? (
            <p role="status" className="notice">
              {sharingLine}
            </p>
          ) : null}
          {shown === null ? null : (
            <input
              type="text"
              readOnly
              value={shown}
              aria-label="Device link"
              onFocus={(event) => {
                event.currentTarget.select();
              }}
            />
          )}
          <div className="actions">
            {revealed && !sharing ? (
              <button
                type="button"
                onClick={() => {
                  setRevealed(false);
                }}
              >
                Hide link
              </button>
            ) : (
              <button
                type="button"
                disabled={sharing}
                onClick={() => {
                  setRevealed(true);
                }}
              >
                Reveal link
              </button>
            )}
            <button type="button" onClick={copy}>
              Copy link
            </button>
            <button type="button" onClick={done}>
              Done
            </button>
          </div>
          <p role="status" className="hint">
            {copied === 'yes' ? 'Copied.' : copied === 'failed' ? 'Could not copy: reveal the link and copy it by hand.' : ''}
          </p>
        </div>
      )}
      <p role="alert" className={line?.refused === true ? 'notice' : 'hint'}>
        {line?.text ?? ''}
      </p>
      <h4>Open device links</h4>
      {listLine === null ? null : <p className="notice">{listLine}</p>}
      {links === null ? (
        <p className="hint">Loading…</p>
      ) : links.length === 0 ? (
        <p className="hint">None open.</p>
      ) : (
        <ul className="signin-list" aria-label="Open device links">
          {links.map((link) => (
            <li key={link.id}>
              <span id={`${id}-link-${String(link.id)}`}>{link.label ?? `device link ${String(link.id)}`}</span>
              <span className="hint">
                {' '}
                · until <time dateTime={link.expires_at}>{clock(link.expires_at)}</time>
              </span>{' '}
              <button
                type="button"
                aria-describedby={`${id}-link-${String(link.id)}`}
                onClick={() => {
                  close(link);
                }}
              >
                Close link
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
