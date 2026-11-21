// Account notices (issue #2042, decision B on #2013): what the server tells a
// member about their own account the next time they sign in with a sign-in of
// their own. Today there is one kind: their password was set, or reset,
// through a link another principal made (an operator with act-as makes
// enrolment and reset links, and whoever opens one chooses the password).
// The audit log records it too; this is the member's side of it.
//
// A notice is shown to the member and no one else: the server lists a
// member's own notices only to a sign-in the member made themselves, and
// answers an empty list to any other, so nothing is shown on an operator's
// credential or link. Dismissing is the member's own, and is a request to the
// server; the notice stays until it succeeds. It carries the name of whoever
// made the link and nothing about anyone else. The text is the page's own,
// the name is shown as text (never as markup).
import { useCallback, useEffect, useState } from 'react';
import * as api from './api';
import { dateTimeOf } from './time';
import type { AccountNoticeV1 } from './schema/signin';

// noticeText is the sentence for a notice. A kind this page does not know is
// shown by its name, as the schema's vocabulary allows.
export function noticeText(notice: AccountNoticeV1): string {
  if (notice.kind === 'password_set_by_link') {
    const by = notice.by_name === '' ? 'another person' : notice.by_name;
    return `Your password was set through a link made by ${by}, at ${dateTimeOf(notice.at)}. If that was not expected, tell the server's operator, and change your password.`;
  }
  return `Notice: ${notice.kind}`;
}

export interface AccountNoticesState {
  readonly notices: readonly AccountNoticeV1[];
  // A problem dismissing, in words, or null.
  readonly line: string | null;
  readonly dismiss: (id: number) => void;
}

// useAccountNotices reads the caller's own undismissed notices once, and
// dismisses one on request. It lives in the page that shows them in two
// places (the lobby's banner and Settings), so dismissing in one clears both.
// It reads only when enabled (a person signed in, not a guest or an agent).
// A failure to read shows nothing: a notice is a courtesy, and a server that
// predates them has none. An unauthorized answer hands over to the page's
// sign-out.
export function useAccountNotices(enabled: boolean, onUnauthorized: () => void): AccountNoticesState {
  const [notices, setNotices] = useState<readonly AccountNoticeV1[]>([]);
  const [line, setLine] = useState<string | null>(null);

  useEffect(() => {
    if (!enabled) return undefined;
    let live = true;
    void api.listAccountNotices().then((result) => {
      if (!live) return;
      if (result.ok) setNotices(result.value);
      else if (result.failure === 'unauthorized') onUnauthorized();
    });
    return () => {
      live = false;
    };
  }, [enabled, onUnauthorized]);

  const dismiss = useCallback(
    (id: number) => {
      void api.dismissAccountNotice(id).then((result) => {
        if (result.ok) {
          setNotices((all) => all.filter((n) => n.id !== id));
          setLine(null);
        } else if (result.failure === 'unauthorized') onUnauthorized();
        else setLine('Could not dismiss that notice. Try again.');
      });
    },
    [onUnauthorized],
  );
  return { notices, line, dismiss };
}

// AccountNotices lists the notices, each with a Dismiss button, and renders
// nothing when there are none.
export function AccountNotices({ state }: { readonly state: AccountNoticesState }) {
  if (state.notices.length === 0) return null;
  return (
    <>
      {state.notices.map((notice) => (
        <div key={notice.id} role="status" className="banner destination account-notice">
          <p>{noticeText(notice)}</p>
          <button
            type="button"
            onClick={() => {
              state.dismiss(notice.id);
            }}
          >
            Dismiss
          </button>
        </div>
      ))}
      {state.line === null ? null : (
        <p className="hint" role="status">
          {state.line}
        </p>
      )}
    </>
  );
}
