// The SDK's logs go to the diagnostics ring, redacted, and nowhere else: not
// to the console (adapted from quietSdk in web/src/voice/sdk.ts).
import { LoggerNames, LogLevel, getLogger } from 'livekit-client';
import type { Diagnostics } from '../logic/diagnostics';

let installed = false;

export function quietSdk(diag: Diagnostics): void {
  if (installed) return;
  installed = true;
  for (const name of Object.values(LoggerNames) as string[]) {
    const logger = getLogger(name);
    logger.methodFactory = (method) => (message: unknown, context?: unknown) => {
      diag.add(method, typeof message === 'string' ? message : '', context);
    };
    // Not setLogLevel(): it persists the level.
    logger.setLevel(LogLevel.warn, false);
  }
}
