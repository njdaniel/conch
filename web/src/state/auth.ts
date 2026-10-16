// Who is signed in. Pure transitions, so they run without a DOM.
import type { ApiFailure } from '../api';
import type { WhoAmIResponseV1 } from '../schema/principal';

// Why the sign-in screen is showing a line. expired is a credential that
// stopped working after it was accepted (a 401 mid-session, or a stored one
// that no longer checks out). The rest follow a sign-out (issue #439): untold
// is one the server could not be told of, so the credential may still work;
// everywhere is every credential of the person revoked; elsewhere is another
// tab of this browser signing the shared credential out.
export type AuthNotice = 'rejected' | 'network' | 'invalid' | 'expired' | 'untold' | 'everywhere' | 'elsewhere';

export type AuthState =
  | { readonly phase: 'restoring' }
  | { readonly phase: 'signedOut'; readonly notice: AuthNotice | null }
  | { readonly phase: 'checking' }
  | { readonly phase: 'operatorWarning'; readonly principal: WhoAmIResponseV1 }
  | { readonly phase: 'signedIn'; readonly principal: WhoAmIResponseV1 };

export type AuthAction =
  | { readonly type: 'submit' }
  | { readonly type: 'verified'; readonly principal: WhoAmIResponseV1 }
  // held: on restore, an operator credential found kept to last, taken out of
  // storage and held back for the warning (issue #439).
  | { readonly type: 'held'; readonly principal: WhoAmIResponseV1 }
  | { readonly type: 'failed'; readonly failure: ApiFailure }
  | { readonly type: 'continue' }
  | { readonly type: 'signOut'; readonly notice?: AuthNotice | null }
  | { readonly type: 'unauthorized' }
  // The signed-in principal changed its name (issue #441).
  | { readonly type: 'renamed'; readonly name: string };

export function initialAuth(hasStoredToken: boolean): AuthState {
  return hasStoredToken ? { phase: 'restoring' } : { phase: 'signedOut', notice: null };
}

const signedOut = (notice: AuthNotice | null): AuthState => ({ phase: 'signedOut', notice });

export function authReducer(state: AuthState, action: AuthAction): AuthState {
  switch (action.type) {
    case 'submit':
      return state.phase === 'signedOut' ? { phase: 'checking' } : state;
    case 'verified':
      if (state.phase === 'checking') {
        // A new sign-in as an operator is warned about; a stored credential
        // that was already accepted past the warning is not asked again.
        return action.principal.role === 'operator'
          ? { phase: 'operatorWarning', principal: action.principal }
          : { phase: 'signedIn', principal: action.principal };
      }
      return state.phase === 'restoring' ? { phase: 'signedIn', principal: action.principal } : state;
    case 'held':
      return state.phase === 'restoring' ? { phase: 'operatorWarning', principal: action.principal } : state;
    case 'failed':
      if (state.phase === 'checking') {
        return signedOut(action.failure === 'unauthorized' ? 'rejected' : action.failure);
      }
      if (state.phase === 'restoring') {
        return signedOut(action.failure === 'unauthorized' ? 'expired' : action.failure);
      }
      return state;
    case 'continue':
      return state.phase === 'operatorWarning' ? { phase: 'signedIn', principal: state.principal } : state;
    case 'signOut':
      return signedOut(action.notice ?? null);
    case 'unauthorized':
      return state.phase === 'signedIn' || state.phase === 'operatorWarning' ? signedOut('expired') : state;
    case 'renamed':
      return state.phase === 'signedIn' ? { phase: 'signedIn', principal: { ...state.principal, name: action.name } } : state;
  }
}

const lines: Record<AuthNotice, string> = {
  rejected: 'That credential was not accepted',
  network: 'Cannot reach the server',
  invalid: 'The server gave an unexpected reply',
  expired: 'Signed out: the credential is no longer valid',
  untold: 'Signed out in this browser, but the server could not be told: the credential may still work until it expires',
  everywhere: 'Signed out everywhere: every sign-in of yours has ended',
  elsewhere: 'Signed out in another tab',
};

export function noticeText(notice: AuthNotice): string {
  return lines[notice];
}
