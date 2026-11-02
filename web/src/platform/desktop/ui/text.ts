// What the server list says, in words (issue #945). Pure functions of the
// bridge's view; no component state.
import type { ProfileAddResult, ProfileNotice, StorageStatus } from '../bridge';

// Written in two parts: the page's source holds no absolute URL.
export const example = ['https:', 'chat.example.org'].join('//');

export const addRefusalLines: Readonly<Record<Extract<ProfileAddResult, { ok: false }>['refusal'], string>> = {
  malformed: `That is not a web address. Write it like ${example}`,
  scheme: 'Use a secure (https) address. A plain http address is accepted only for this computer (localhost, 127.0.0.1 or [::1]).',
  userinfo: 'Leave the user name and password out of the address.',
  path: 'Give the server’s address alone: no path, query or #fragment after the host.',
  unreachable: 'That server could not be reached. Check the address and the connection.',
  unsupported: 'That address could not be saved.',
  certificate:
    'This computer does not trust that server’s certificate, so nothing was sent. If the server uses a private certificate authority, install it in the system’s certificate store (see desktop/README.md), then try again.',
  'not-conch': 'That address answered, but not as a Conch server.',
  full: 'There are as many saved servers as the app keeps. Remove one first.',
};

export function storageLine(storage: StorageStatus, platform: string): string {
  switch (storage.kind) {
    case 'protected':
      return platform === 'win32'
        ? 'Sign-ins are kept on this computer, sealed by Windows (DPAPI). That protects them from other users of this computer, not from other apps running as you.'
        : 'Sign-ins are kept on this computer, sealed by the system’s secret store.';
    case 'unavailable':
      return 'The system’s secret store did not answer, so sign-ins are kept only until the app closes. Retry to check again.';
    case 'session-only':
      switch (storage.reason) {
        case 'unavailable':
          return 'This computer has no secret store available, so sign-ins are kept only until the app closes. You will sign in again next time.';
        case 'insecure-backend':
          return 'The system’s secret store here protects nothing (it uses a built-in password), so sign-ins are kept only until the app closes. You will sign in again next time.';
        case 'unknown-backend':
          return 'The app cannot tell that this computer’s secret store is a safe one, so sign-ins are kept only until the app closes. You will sign in again next time.';
        case 'unsafe-root':
          return 'The data directory is not private to you, so nothing is saved and sign-ins are kept only until the app closes. Fix the directory’s permissions, then Retry.';
      }
  }
}

export const noticeLines: Readonly<Record<ProfileNotice, string>> = {
  'sign-in-again': 'The saved sign-in for this server could not be used. Sign in again.',
  'could-not-verify':
    'The saved sign-in for this server could not be checked because the server did not answer, so it was not used. Switch to the server again when it can be reached.',
  'account-mismatch':
    'That sign-in is for a different account than the one this saved server belongs to, so it was not kept. To use another account, add the server again as a new entry.',
  'revocation-unconfirmed': 'Signed out on this computer. The server did not confirm that it ended that sign-in.',
  'revocation-unsent': 'Signed out on this computer. The server could not be reached, so the app cannot prove the sign-in was ended there; it was forgotten here.',
  'teardown-slow': 'Something that was using the old sign-in did not stop in time. It was left behind, and the new server was started without it.',
};
