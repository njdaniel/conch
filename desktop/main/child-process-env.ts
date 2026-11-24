import { debuglog } from 'node:util';

// The Node debug categories whose output carries what main must keep to
// itself: child_process prints each spawn's options, the voice helper's
// environment (CONCH_TOKEN, CONCH_VOICE_ATTACH) included (#2105); stream
// prints a preview of every chunk main reads, helper output and conchd's
// answers included (#2155).
const exposing = ['child_process', 'stream'] as const;

// The refusals are fixed text: they name the variable, never its value.
export const packagedRefusal =
  'Remove NODE_DEBUG from the launch environment and restart Conch. The installed app does not run with Node debug output, which can expose voice credentials and server data.';
export const debugRefusal =
  'Remove NODE_DEBUG from the launch environment and restart Conch. Child process and stream debugging can expose voice credentials and server data.';

// requireSafeChildProcessEnv runs first in main (main.ts), before anything is
// spawned or read. A child env allowlist cannot stop Node from forwarding
// NODE_V8_COVERAGE, so it is removed. Node also takes its NODE_DEBUG snapshot
// before running main's JavaScript, so deleting the variable does not silence
// a category: a packaged build refuses any NODE_DEBUG at all (it has no use
// for Node's debug output), and a development run refuses the categories
// that expose data, leaving the others to developers.
export function requireSafeChildProcessEnv(packaged: boolean): void {
  const asked = process.env.NODE_DEBUG ?? '';
  delete process.env.NODE_V8_COVERAGE;
  delete process.env.NODE_DEBUG;
  if (packaged && asked.trim() !== '') throw new Error(packagedRefusal);
  if (exposing.some((category) => debuglog(category).enabled)) throw new Error(debugRefusal);
}
