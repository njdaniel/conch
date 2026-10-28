// Who is signed in. Pure transitions, so they run without a DOM.
import type { ApiFailure } from '../api';
import { visitOf } from '../guestName';
import type { WhoAmIResponseV1 } from '../schema/principal';

// Why the sign-in screen is showing a line. expired is a credential that
// stopped working after it was accepted (a 401 mid-session, or a stored one
// that no longer checks out). The rest follow a sign-out (issue #439): untold
// is one the server could not be told of, so the credential may still work;
// everywhere is every credential of the person revoked; elsewhere is another
// tab of this browser signing the shared credential out. A guest's visit
// (issue #713): visitEnded is one that ended, by End visit, its expiry or an
// operator; visitUntold is one forgotten here that the server could not be
// told of; convertGone is a Create an account whose answer was lost, after
// which the guest credential no longer works. An operator's sign-in (issue
// #714): sessionEnded is the session a password and a code began, ended by
// its twelve hours, an hour without use or a revoke; codeOver is a sign-in
// whose second step ran out of time or of tries, so the password comes
// first again; setupDone is an operator's setup complete, which signs nobody
// in; setupSkipped is the same with no authenticator (ADR-012).
export type AuthNotice =
  | 'sessionEnded'
  | 'codeOver'
  | 'setupDone'
  | 'setupSkipped'
  | 'rejected'
  | 'network'
  | 'invalid'
  | 'expired'
  | 'untold'
  | 'everywhere'
  | 'elsewhere'
  | 'held'
  | 'visitEnded'
  | 'visitUntold'
  | 'convertGone'
  | 'convertUnkept';

export type AuthState =
  | { readonly phase: 'restoring' }
  // line is the page's own words for a refused password sign-in, shown in
  // place of a notice.
  | { readonly phase: 'signedOut'; readonly notice: AuthNotice | null; readonly line?: string }
  | { readonly phase: 'checking' }
  // An operator's right password, waiting for the code (issue #714): nobody
  // is signed in. name is the name typed, for the page to say whose code it
  // asks for; until is when the challenge runs out; checking is a code sent;
  // line is why the last one was refused.
  | { readonly phase: 'code'; readonly name: string; readonly until: string; readonly checking: boolean; readonly line?: string }
  | { readonly phase: 'operatorWarning'; readonly principal: WhoAmIResponseV1 }
  | { readonly phase: 'signedIn'; readonly principal: WhoAmIResponseV1 };

export type AuthAction =
  | { readonly type: 'submit' }
  | { readonly type: 'verified'; readonly principal: WhoAmIResponseV1; readonly own?: boolean }
  // held: on restore, an operator credential found kept to last, taken out of
  // storage and held back for the warning (issue #439).
  | { readonly type: 'held'; readonly principal: WhoAmIResponseV1 }
  | { readonly type: 'failed'; readonly failure: ApiFailure | 'held' }
  // A password sign-in refused, with the words to show (issue #713).
  | { readonly type: 'refused'; readonly line: string }
  // An operator's password was right and a code is asked for (issue #714),
  // a code was sent, and a code was refused with the words to show.
  | { readonly type: 'challenged'; readonly name: string; readonly until: string }
  | { readonly type: 'codeSent' }
  | { readonly type: 'codeRefused'; readonly line: string }
  // A credential was stored by another tab since this page looked: check it,
  // as on load (issue #469).
  | { readonly type: 'recheck' }
  | { readonly type: 'continue' }
  | { readonly type: 'signOut'; readonly notice?: AuthNotice | null }
  | { readonly type: 'unauthorized' }
  // The signed-in principal changed its name (issue #441).
  | { readonly type: 'renamed'; readonly name: string }
  // whoami read again while signed in (a password set, issue #714): the same
  // principal, as it now is.
  | { readonly type: 'refreshed'; readonly principal: WhoAmIResponseV1 };

export function initialAuth(hasStoredToken: boolean): AuthState {
  return hasStoredToken ? { phase: 'restoring' } : { phase: 'signedOut', notice: null };
}

const signedOut = (notice: AuthNotice | null): AuthState => ({ phase: 'signedOut', notice });

export function authReducer(state: AuthState, action: AuthAction): AuthState {
  switch (action.type) {
    case 'submit':
      return state.phase === 'signedOut' ? { phase: 'checking' } : state;
    case 'verified':
      // The code's step is the operator's own sign-in, a password and a
      // code: no warning, which is for a script's credential (issue #714).
      if (state.phase === 'code' && state.checking) return { phase: 'signedIn', principal: action.principal };
      if (state.phase === 'checking') {
        // A new sign-in as an operator is warned about; a stored credential
        // that was already accepted past the warning is not asked again. An
        // operator's own session, which a password alone gives on a server
        // that allows it (ADR-012, issue #867), is not a script's credential:
        // no warning. A pasted credential is warned about whatever it is:
        // only the warning's Continue stores and pins an operator's.
        return action.principal.role === 'operator' && !(action.own === true && operatorSession(action.principal))
          ? { phase: 'operatorWarning', principal: action.principal }
          : { phase: 'signedIn', principal: action.principal };
      }
      return state.phase === 'restoring' ? { phase: 'signedIn', principal: action.principal } : state;
    case 'held':
      return state.phase === 'restoring' ? { phase: 'operatorWarning', principal: action.principal } : state;
    case 'failed':
      if (state.phase === 'checking' || state.phase === 'code') {
        return signedOut(action.failure === 'unauthorized' ? 'rejected' : action.failure);
      }
      if (state.phase === 'restoring') {
        return signedOut(action.failure === 'unauthorized' ? 'expired' : action.failure);
      }
      return state;
    case 'refused':
      return state.phase === 'checking' || state.phase === 'code' ? { phase: 'signedOut', notice: null, line: action.line } : state;
    case 'challenged':
      return state.phase === 'checking' ? { phase: 'code', name: action.name, until: action.until, checking: false } : state;
    case 'codeSent':
      return state.phase === 'code' && !state.checking ? { phase: 'code', name: state.name, until: state.until, checking: true } : state;
    case 'codeRefused':
      return state.phase === 'code' ? { phase: 'code', name: state.name, until: state.until, checking: false, line: action.line } : state;
    case 'recheck':
      return state.phase === 'checking' || state.phase === 'signedOut' ? { phase: 'restoring' } : state;
    case 'continue':
      return state.phase === 'operatorWarning' ? { phase: 'signedIn', principal: state.principal } : state;
    case 'signOut':
      return signedOut(action.notice ?? null);
    case 'unauthorized':
      // A guest's credential stops working when its visit ends: that is
      // what the person is told.
      if (state.phase === 'signedIn' && visitOf(state.principal) !== null) return signedOut('visitEnded');
      // An operator's session ends by its bounds (issue #714): that is what
      // the operator is told, and how to come back.
      if (state.phase === 'signedIn' && operatorSession(state.principal)) return signedOut('sessionEnded');
      return state.phase === 'signedIn' || state.phase === 'operatorWarning' ? signedOut('expired') : state;
    case 'renamed':
      return state.phase === 'signedIn' ? { phase: 'signedIn', principal: { ...state.principal, name: action.name } } : state;
    case 'refreshed':
      return state.phase === 'signedIn' && state.principal.id === action.principal.id ? { phase: 'signedIn', principal: action.principal } : state;
  }
}

const lines: Record<AuthNotice, string> = {
  sessionEnded:
    'Your operator sign-in has ended. It lasts twelve hours at most, and ends after an hour without use. Sign in again with your password and a code. Anything you were typing is kept for when you are back.',
  codeOver: 'That sign-in took too long, or had too many codes that were not right. Sign in again with your name and password.',
  setupDone: 'Setup is complete. Sign in with your name, your password and a code from your authenticator app.',
  setupSkipped: 'Setup is complete. Sign in with your name and your password.',
  rejected: 'That credential was not accepted',
  network: 'Cannot reach the server',
  invalid: 'The server gave an unexpected reply',
  expired: 'Signed out: the credential is no longer valid',
  untold: 'Signed out in this browser, but the server could not be told: the credential may still work until it expires',
  everywhere: 'Signed out everywhere: every sign-in of yours has ended',
  elsewhere: 'Signed out in another tab',
  held: 'This browser holds another sign-in that could not be checked. Reload when the server can be reached, or forget it here first.',
  visitEnded: 'Your guest visit has ended. Your messages stay in the conversation. To come back, join again as a new guest, or sign in with an account.',
  visitUntold:
    'This tab has forgotten your guest visit, but the server could not be told. The visit ends on its own when it expires; until then its sign-in may still work.',
  convertGone:
    'The server did not confirm your new account, and your guest visit has ended. If the account was made, sign in with its name and password.',
  convertUnkept:
    'Your account was made, but this browser will not let the page keep your sign-in. Allow this site to keep data, then sign in with your new name and password.',
};

export function noticeText(notice: AuthNotice): string {
  return lines[notice];
}

// operatorSession says whether whoami describes an operator signed in with a
// password and a code (issue #714, design §5.3): that session, and only it,
// has an idle bound among an operator's credentials (review F5). A script's
// credential has none, and is never taken for an operator's sign-in.
export function operatorSession(who: WhoAmIResponseV1): boolean {
  return who.role === 'operator' && who.auth?.idle_expires_at !== undefined;
}

// keptAcross says whether the page keeps what was being typed when a sign-in
// ended (issue #714): when the credential stopped working rather than the
// person signing out, so it is there again if they sign back in.
export function keptAcross(notice: AuthNotice | null): boolean {
  return notice === 'expired' || notice === 'sessionEnded';
}
