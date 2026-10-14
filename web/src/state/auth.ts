// Who is signed in. Pure transitions, so they run without a DOM.
import type { ApiFailure } from '../api';
import type { WhoAmIResponseV1 } from '../schema/principal';

// Why the sign-in screen is showing a line. expired is a credential that
// stopped working after it was accepted (a 401 mid-session, or a stored one
// that no longer checks out).
export type AuthNotice = 'rejected' | 'network' | 'invalid' | 'expired';

export type AuthState =
  | { readonly phase: 'restoring' }
  | { readonly phase: 'signedOut'; readonly notice: AuthNotice | null }
  | { readonly phase: 'checking' }
  | { readonly phase: 'operatorWarning'; readonly principal: WhoAmIResponseV1 }
  | { readonly phase: 'signedIn'; readonly principal: WhoAmIResponseV1 };

export type AuthAction =
  | { readonly type: 'submit' }
  | { readonly type: 'verified'; readonly principal: WhoAmIResponseV1 }
  | { readonly type: 'failed'; readonly failure: ApiFailure }
  | { readonly type: 'continue' }
  | { readonly type: 'signOut' }
  | { readonly type: 'unauthorized' };

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
      return signedOut(null);
    case 'unauthorized':
      return state.phase === 'signedIn' || state.phase === 'operatorWarning' ? signedOut('expired') : state;
  }
}

const lines: Record<AuthNotice, string> = {
  rejected: 'That credential was not accepted',
  network: 'Cannot reach the server',
  invalid: 'The server gave an unexpected reply',
  expired: 'Signed out: the credential is no longer valid',
};

export function noticeText(notice: AuthNotice): string {
  return lines[notice];
}
