// The input-only helper reuses evdev and Windows Raw Input. It reports only
// presses of the configured mute binding. Main decides what each press does.
import type { MainLog } from '../diagnostics.ts';
import { muteKeyEventV1, type MuteKeyEventV1 } from '../../../web/src/schema/generated.ts';
import { decodeObject, LineSplitter } from './protocol.ts';
import { defaultSpawn, findHelper, oneShotEnv, spawnHelper } from './spawn.ts';

export function watchMuteKey(
  resources: string,
  env: NodeJS.ProcessEnv,
  log: MainLog,
  configDir: string,
  onPress: () => void,
): () => void {
  let stopped = false;
  let closePipe: (() => void) | undefined;
  const stop = (): void => {
    stopped = true;
    closePipe?.();
  };
  const fail = (message: string): void => {
    if (stopped) return;
    stop();
    log(message);
  };
  const invalid = (): void => { fail('voice: the mute key reader sent an invalid event'); };
  void findHelper(resources).then((executable) => {
    if (stopped) return;
    // A private stdin pipe stays open while main owns the listener. If main
    // exits, the helper sees EOF and exits even when there is no key input.
    const child = spawnHelper(defaultSpawn, executable, ['watch-mute-key'], oneShotEnv(env, configDir));
    closePipe = () => { child.stdin.destroy(); };
    child.stdin.on('error', () => { fail('voice: the mute key reader lost its owner pipe'); });
    child.stderr.resume();
    const lines = new LineSplitter();
    child.stdout.on('data', (bytes: Buffer) => {
      if (stopped) return;
      lines.push(bytes, (line) => {
        if (stopped) return;
        // The shared reader rejects invalid UTF-8, duplicate fields and trailing
        // values before the generated validator checks the event's shape.
        if (line === 'too_long' || !decodeObject(line).ok) {
          invalid();
          return;
        }
        const event: unknown = JSON.parse(line.toString('utf8'));
        if (muteKeyEventV1(event).length !== 0 || !(event as MuteKeyEventV1).press) {
          invalid();
          return;
        }
        onPress();
      });
    });
    child.stdout.once('end', () => {
      if (lines.pending) invalid();
    });
    child.on('error', () => { fail('voice: the mute key reader failed'); });
    child.once('close', (code) => {
      if (code !== 0) fail('voice: the mute key reader exited unexpectedly');
      stop();
    });
  }).catch(() => { fail('voice: the mute key reader could not be started'); });
  return stop;
}
