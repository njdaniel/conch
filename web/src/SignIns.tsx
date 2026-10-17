import { useEffect, useId, useRef, useState } from 'react';
import * as api from './api';
import type { OwnCredentialV1 } from './schema/credential';

// "Your sign-ins" (issue #496): every browser or app signed in as this person,
// each with "End this sign-in". One made through a sign-in link an operator
// sent is labelled "sign-in link <id>" by the server, which is how the person
// learns a link was used for their account. Ending this browser's own is
// signing out, with the sign-out's own question (onEndThis); ending another
// asks here first.

type Loaded =
  | { readonly kind: 'loading' }
  | { readonly kind: 'ready'; readonly list: readonly OwnCredentialV1[] }
  | { readonly kind: 'failed'; readonly line: string };

// endLine says why another sign-in was not ended.
function endLine(result: { readonly failure: api.ApiFailure; readonly code?: string }): string {
  if (result.failure === 'network') return 'Cannot reach the server. The sign-in was not ended; try again.';
  if (result.code === 'last_operator') return 'That is the last sign-in of the only operator who can sign in, so it cannot end.';
  return 'The server refused. The sign-in was not ended.';
}

export function YourSignIns({ onEndThis, onUnauthorized }: { readonly onEndThis: () => void; readonly onUnauthorized: () => void }) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [loaded, setLoaded] = useState<Loaded>({ kind: 'loading' });
  // The other sign-in waiting for its yes, by id, if one is.
  const [confirming, setConfirming] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [line, setLine] = useState<{ readonly text: string; readonly refused: boolean } | null>(null);
  const cancel = useRef<HTMLButtonElement>(null);

  // Read afresh each time the section opens: a sign-in may have started or
  // ended elsewhere since.
  useEffect(() => {
    if (!open) return;
    let live = true;
    setLoaded({ kind: 'loading' });
    void api.listSignIns().then((result) => {
      if (!live) return;
      if (result.ok) setLoaded({ kind: 'ready', list: result.value });
      else if (result.failure === 'unauthorized') onUnauthorized();
      else setLoaded({ kind: 'failed', line: result.failure === 'network' ? 'Cannot reach the server.' : 'Could not read your sign-ins.' });
    });
    return () => {
      live = false;
    };
  }, [open, onUnauthorized]);

  // The question takes focus on its safe answer, so Enter keeps the sign-in.
  useEffect(() => {
    if (confirming !== null) cancel.current?.focus();
  }, [confirming]);

  function end(signIn: OwnCredentialV1) {
    if (busy) return;
    setBusy(true);
    setLine(null);
    void api.endSignIn(signIn.id).then((result) => {
      setBusy(false);
      setConfirming(null);
      if (result.ok || result.code === 'credential_not_found') {
        // Gone either way: ended now, or no longer one of the caller's.
        setLoaded((was) => (was.kind === 'ready' ? { kind: 'ready', list: was.list.filter((c) => c.id !== signIn.id) } : was));
        setLine({ text: result.ok ? `Ended the sign-in “${signIn.label}”.` : `The sign-in “${signIn.label}” had already ended.`, refused: false });
        return;
      }
      if (result.failure === 'unauthorized') {
        onUnauthorized();
        return;
      }
      setLine({ text: endLine(result), refused: true });
    });
  }

  return (
    <div className="signins">
      <button
        type="button"
        aria-expanded={open}
        aria-controls={`${id}-panel`}
        onClick={() => {
          setOpen(!open);
          setConfirming(null);
          setLine(null);
        }}
      >
        Your sign-ins
      </button>
      <div id={`${id}-panel`} role="region" aria-label="Your sign-ins" className="signins-panel" hidden={!open}>
        <p className="hint">
          Each is a browser or app signed in as you. One labelled “sign-in link” began when a sign-in link from an operator was used: if that was not you, end
          it and tell an operator.
        </p>
        {loaded.kind === 'loading' ? (
          <p className="empty">Loading your sign-ins…</p>
        ) : loaded.kind === 'failed' ? (
          <p role="alert" className="notice">
            {loaded.line}
          </p>
        ) : (
          <ul aria-label="Sign-ins" className="signin-list">
            {loaded.list.map((c) => (
              <li key={c.id}>
                <span id={`${id}-label-${String(c.id)}`}>{c.label}</span>
                {c.current === true ? <strong className="signin-here"> This browser</strong> : null}
                <span className="hint">
                  {' '}
                  · since <time dateTime={c.created_at}>{new Date(c.created_at).toLocaleString()}</time>
                </span>{' '}
                {confirming === c.id ? (
                  <div role="group" aria-labelledby={`${id}-question`} className="signout-confirm">
                    <p id={`${id}-question`}>End the sign-in “{c.label}”? Whatever browser or app uses it will be signed out.</p>
                    <div className="actions">
                      <button
                        type="button"
                        disabled={busy}
                        onClick={() => {
                          end(c);
                        }}
                      >
                        Yes, end it
                      </button>
                      <button
                        ref={cancel}
                        type="button"
                        disabled={busy}
                        onClick={() => {
                          setConfirming(null);
                        }}
                      >
                        Cancel
                      </button>
                    </div>
                  </div>
                ) : (
                  <button
                    type="button"
                    aria-describedby={`${id}-label-${String(c.id)}`}
                    disabled={busy}
                    onClick={() => {
                      setLine(null);
                      if (c.current === true) {
                        setConfirming(null);
                        onEndThis();
                      } else setConfirming(c.id);
                    }}
                  >
                    End this sign-in
                  </button>
                )}
              </li>
            ))}
          </ul>
        )}
        <p role="status" className={line?.refused === true ? 'notice' : 'hint'}>
          {line?.text ?? ''}
        </p>
      </div>
    </div>
  );
}
