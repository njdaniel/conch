// What the server list says, in words (issue #945). Pure functions of the
// bridge's view; no component state.
import type { IntegrationId, IntegrationRow, IntegrationState, IntegrationsView, IntegrationWhy, InviteAccept, InviteRefusal, ProfileAddResult, ProfileNotice, StorageStatus } from '../bridge';

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

// Why an invite link was not accepted or could not be opened. Never the link
// or its code: the person sees the rule, not what they pasted.
export const inviteRefusalLines: Readonly<Record<InviteRefusal, string>> = {
  type: 'That is not an invite link.',
  length: 'That is not an invite link: it is empty or too long.',
  characters: 'That link has spaces, hidden or look-alike characters in it, so it was not used.',
  scheme: 'An invite link is a secure (https) address. A plain http link is accepted only for this computer (localhost, 127.0.0.1 or [::1]).',
  userinfo: 'That link contains a user name or password, so it was not used.',
  host: 'The server’s address in that link is written with hidden or look-alike characters, so it was not used.',
  path: 'That is not an invite link: it does not end in /app/#/invite/ and a code.',
  query: 'That is not an invite link: it has extra parts after the address.',
  fragment: 'That is not an invite link: it does not end in #/invite/ and a code.',
  code: 'The code in that link is not one Conch makes.',
  malformed: 'That is not an invite link.',
};

export const inviteAcceptLines: Readonly<Record<Extract<InviteAccept, { ok: false }>['refusal'], string>> = {
  expired: 'That invite link was not used in time. Paste it again.',
  'voice-started': 'Voice started after the link was checked, and opening the invite on that server would leave it. Check the link again.',
  'unknown-profile': 'That saved server is no longer there. Paste the link again.',
  unsupported: 'The desktop app could not open that invite.',
  full: addRefusalLines.full,
  unreachable: addRefusalLines.unreachable,
  certificate: addRefusalLines.certificate,
  'not-conch': addRefusalLines['not-conch'],
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

// What the app says of the server's release (issue #946). X is the server's
// version as /healthz named it, or "unknown"; Y is this app's own.
export const untested = (server: string | null, desktop: string): string =>
  `This server’s version (${server ?? 'unknown'}) has not been tested with this app (${desktop}).`;

export const unreadable = (desktop: string): string =>
  `This server’s version could not be read because it did not answer, so it cannot be checked against this app (${desktop}). Nothing was signed in.`;

export const overrideLine = 'Development build: servers that are not on the tested list are allowed (--allow-unlisted-server). A packaged app has no such option.';

export const incompatibleLine = (family: string, server: string | null, desktop: string): string =>
  `This server’s ${family} answers are not what this app (${desktop}) expects, so its version (${server ?? 'unknown'}) is incompatible with it. This is not a network problem; use a server version that has been tested with this app.`;

// The desktop app's own settings page (issue #1587).
export const settingsText = {
  heading: 'Desktop app',
  closeLegend: 'When I close the window',
  closeQuit: 'Quit Conch',
  closeTray: 'Keep running in the tray',
  noTray: 'No tray is available on this desktop',
  trayAsk: 'Can you see the Conch icon in your panel?',
  trayYes: 'Yes, I can see it',
  trayNo: 'No, quit when I close',
  launch: 'Open Conch when I log in',
  launchUnavailable: 'Only an installed copy of Conch can open when you log in.',
  launchUnsafe: 'Launch at login needs the app installed in a place only you or the system can change.',
  failed: 'The desktop app could not change that.',
} as const;

export const settingsRefusalLines: Readonly<Record<'unpackaged' | 'unsafe-location' | 'foreign-file' | 'no-tray' | 'failed', string>> = {
  unpackaged: 'Only an installed copy of Conch can open when you log in.',
  'unsafe-location': 'Launch at login needs the app installed in a place only you or the system can change.',
  'foreign-file': 'Another program already has a startup entry with Conch’s name, so it was left alone.',
  'no-tray': 'No tray is available on this desktop, so closing the window will quit Conch.',
  failed: 'The desktop app could not change that.',
};

// What works on this desktop (issue #1593, UX-23). Each row reads Working, Not
// available here or Unknown, then one sentence on what it means for the person.
// A reason is a fixed code from main; the sentences are written here, and none
// holds a path, device name, address, token or user name.
export const integrationsText = {
  heading: 'What works on this desktop',
  copy: 'Copy this list',
  copied: 'Copied.',
  copyFailed: 'Could not copy. Select the list and copy it by hand.',
  copyTitle: 'Conch desktop: what works here',
} as const;

export const integrationNames: Readonly<Record<IntegrationId, string>> = {
  tray: 'Tray icon',
  notifications: 'Notifications',
  lock: 'Screen lock',
  capture: 'Screen sharing',
  storage: 'Saved sign-ins',
  launch: 'Open at login',
  shareSound: 'Sound when sharing',
  voiceHelper: 'Voice helper',
  talkKey: 'Talk key',
};

export const integrationStateWords: Readonly<Record<IntegrationState, string>> = {
  working: 'Working',
  unavailable: 'Not available here',
  unknown: 'Unknown',
};

const workingLines: Readonly<Record<IntegrationId, string>> = {
  tray: 'Conch can keep running in your panel when you close the window.',
  notifications: 'Conch can show a notification for a new message.',
  lock: 'Conch can tell when your screen locks, so locking mutes you and hides notification text.',
  capture: 'This desktop offers screen sharing, and shows its own picker first.',
  storage: 'Your sign-ins are kept on this computer, locked by your desktop’s password keyring.',
  launch: 'Conch can open when you log in, if you choose that.',
  // Unreachable today: main answers a fixed "unavailable" for this row until
  // #1585 and #1586 make a sound. It stays so the table is total by type and
  // the row reads correctly the day main reports it working.
  shareSound: 'Conch plays a sound when a share starts or ends.',
  voiceHelper: 'The voice helper is installed and running.',
  talkKey: 'Your talk key is set up and ready.',
};

const whyLines: Readonly<Record<IntegrationWhy, string>> = {
  'no-tray-host': 'This desktop shows no tray, so closing the window quits Conch.',
  'tray-off': 'This desktop has a tray, and Conch uses it only if you choose “Keep running in the tray”.',
  'tray-unconfirmed': 'Conch has not been told yet that you can see its icon, so closing the window still quits it.',
  'notifications-unsupported': 'This desktop does not show notifications for Conch, so you will not be told of new messages while the window is out of sight.',
  'lock-checking': 'Conch is still checking whether it can tell when your screen locks.',
  'lock-unobserved': 'Conch can’t tell when your screen locks on this desktop: locking won’t mute you, and notification text stays hidden.',
  'no-portal': 'This desktop has no screen-share service Conch can use, so you can watch shares but not start one.',
  'not-wayland': 'This desktop is not using the newer display system that Conch needs to share a screen, so you can watch shares but not start one here.',
  'portal-unasked': 'Conch has not checked yet whether this desktop can share a screen.',
  'secret-store-missing': 'This computer has no password keyring, so sign-ins are kept only until the app closes and you will sign in again next time.',
  'insecure-backend': 'The password keyring here protects nothing, so sign-ins are kept only until the app closes and you will sign in again next time.',
  'unknown-backend': 'Conch can’t tell that the password keyring here is a safe one, so sign-ins are kept only until the app closes.',
  'unsafe-root': 'The folder where Conch keeps its files is not private to you, so nothing is saved and sign-ins are kept only until the app closes.',
  'secret-store-silent': 'The password keyring did not answer, so Conch can’t tell yet whether sign-ins will be kept.',
  unpackaged: 'Only an installed copy of Conch can open when you log in.',
  'unsafe-location': 'Launch at login needs Conch installed in a place only you or the system can change.',
  'share-silent': 'Conch makes no sound when a share starts or ends on this desktop yet, so look at the window instead.',
  'not-installed': 'The voice helper is not installed, so voice is off and text chat is unaffected.',
  untrusted: 'Conch does not trust the installed voice helper, so voice is off and text chat is unaffected.',
  'helper-not-started': 'The voice helper starts when you join voice, so Conch can’t tell until then.',
  'helper-starting': 'The voice helper is starting, so Conch can’t tell yet.',
  'helper-failed': 'The voice helper stopped because of a problem, so voice is off until you join again.',
  'voice-unavailable': 'Voice can’t start right now, and text chat is unaffected.',
  'key-not-configured': 'No talk key is set up yet, so set one in voice settings, or use the mute button.',
  'key-unsure': 'The voice helper is running but has not said whether your talk key works, so use the mute button for now.',
  'key-unavailable': 'Conch can’t read your talk key right now, so use the mute button.',
};

export function integrationSentence(row: IntegrationRow): string {
  if (row.state === 'working') return workingLines[row.id];
  if (row.why !== undefined) return whyLines[row.why];
  return row.state === 'unknown' ? 'Conch can’t tell yet.' : 'This is not available on this desktop.';
}

// The list as plain text for a problem report: one line per row, the state
// then its sentence.
export function integrationsPlainText(view: IntegrationsView): string {
  return [integrationsText.copyTitle, ...view.rows.map((row) => `${integrationNames[row.id]}: ${integrationStateWords[row.state]}. ${integrationSentence(row)}`)].join('\n');
}

// The update notice (issue #1600). The version is main's, checked to be a
// release version, and is shown as text.
export const restartLabel = 'Restart';

// Said when voice is running: a restart quits the app, and the call with it.
export const restartEndsCall = 'Restarting ends your call.';

export function updatedLine(version: string | null, callActive = false): string {
  const line = version === null ? 'Conch was updated. Restart when you’re ready.' : `Conch was updated to ${version}. Restart when you’re ready.`;
  return callActive ? `${line} ${restartEndsCall}` : line;
}

// The native dialog main shows when a page did not start after an update (the
// page cannot show the notice itself then). The same words as the notice.
export const updateDialogButtons: readonly [string, string] = ['Restart', 'Later'];

export function updateDialogMessage(version: string | null): string {
  return version === null ? 'Conch was updated' : `Conch was updated to ${version}`;
}

export function updateDialogDetail(callActive: boolean): string {
  const detail = 'Restart Conch to finish the update. Your saved servers are kept. Nothing is restarted until you choose.';
  return callActive ? `${detail} ${restartEndsCall}` : detail;
}
