// The input-only helper reuses evdev and Windows Raw Input. It reports only
// presses of the configured mute binding. Main decides what each press does.
import { defaultSpawn, findHelper, oneShotEnv, spawnHelper } from './spawn.ts';

export function watchMuteKey(
  resources: string,
  env: NodeJS.ProcessEnv,
  log: (line: string) => void,
  configDir: string,
  onPress: () => void,
): () => void {
  let stopped = false;
  let stopChild: (() => void) | undefined;
  void findHelper(resources).then((executable) => {
    if (stopped) return;
    // A private stdin pipe stays open while main owns the listener. If main
    // exits, the helper sees EOF and exits even when there is no key input.
    const child = spawnHelper(defaultSpawn, executable, ['watch-mute-key'], oneShotEnv(env, configDir));
    stopChild = () => { child.stdin.destroy(); };
    child.stdin.on('error', stopChild);
    child.stderr.resume();
    // The private pipe carries one 0x01 byte per press.
    child.stdout.on('data', (bytes: Buffer) => {
      if (stopped) return;
      for (const byte of bytes) {
        if (byte === 1) onPress();
      }
    });
    child.on('error', () => {
      if (!stopped) log('voice: the mute key reader failed');
    });
    child.once('close', (code) => {
      if (!stopped && code !== 0) log('voice: the mute key reader exited unexpectedly');
    });
  }).catch(() => {
    if (!stopped) log('voice: the mute key reader could not be started');
  });
  return () => {
    stopped = true;
    stopChild?.();
  };
}
