// Chromium honours --remote-debugging-port and --remote-debugging-pipe in a
// packaged app, and they hand whoever connects full control of every page.
// The app never turns them on. This gate makes a launch that did so explicit:
// it prints one line, and refuses to start unless CONCH_DESKTOP_ALLOW_CDP=1 is
// in the environment (the smoke test sets it; nothing else should).
//
// It catches launcher mistakes (a wrapper script, a stale desktop entry) and
// keeps the choice visible. It is not a defence against a same-user attacker,
// who can set the variable too. Chromium may open the port before this runs,
// so a refused launch can have had it open for a moment.
export const REMOTE_DEBUGGING_SWITCHES = ['remote-debugging-port', 'remote-debugging-pipe'] as const;

export const ON_LINE = 'conch-desktop: remote debugging is ON';

export type DebuggingVerdict = 'off' | 'allowed' | 'refused';

export function debuggingVerdict(
  packaged: boolean,
  hasSwitch: (name: string) => boolean,
  env: Readonly<Record<string, string | undefined>>,
  log: (line: string) => void,
): DebuggingVerdict {
  if (!packaged || !REMOTE_DEBUGGING_SWITCHES.some(hasSwitch)) return 'off';
  log(ON_LINE);
  if (env['CONCH_DESKTOP_ALLOW_CDP'] === '1') return 'allowed';
  log('conch-desktop: refusing to start with remote debugging; set CONCH_DESKTOP_ALLOW_CDP=1 if you meant it');
  return 'refused';
}
