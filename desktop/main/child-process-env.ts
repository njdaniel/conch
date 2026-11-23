import { debuglog } from 'node:util';

// A child env allowlist cannot stop Node from forwarding NODE_V8_COVERAGE.
// Node also takes its NODE_DEBUG snapshot before running main's JavaScript,
// so deleting it does not silence child_process's spawn option logging.
export function requireSafeChildProcessEnv(): void {
  delete process.env.NODE_V8_COVERAGE;
  delete process.env.NODE_DEBUG;
  if (debuglog('child_process').enabled) {
    throw new Error('Remove NODE_DEBUG from the launch environment and restart Conch. Child process debugging can expose voice credentials.');
  }
}
