// The scripted agent the specs start: worker-test's conch-adapter running a
// fixture script (never a model).
import { closeSync, openSync } from 'node:fs';
import { join } from 'node:path';
import { spawn } from 'node:child_process';
import { roster } from './api';
import { envFile, safePath, type World } from './world';

export interface LaunchedAgent {
  // The session the adapter opened.
  readonly session: number;
  // Stops the adapter the way ctrl-c does, and waits for it to go.
  readonly stop: () => Promise<void>;
}

let launches = 0;

// launchWorker starts worker-test's conch-adapter with the settings of
// agent-a.env, as the walkthrough's printed command does, but with one of the
// fixture scripts and optionally another channel, and waits for the session it
// opens. The command is always a script from the root: never a model, never the
// real claude.
export async function launchWorker(world: World, script: 'slow.sh' | 'hang.sh', options: { channel?: string; sleep: number }): Promise<LaunchedAgent> {
  launches++;
  const channel = options.channel ?? 'general';
  const known = new Set((await roster(world, world.tokens.nick, channel)).map((s) => s.id));
  const settings = Object.fromEntries(envFile(join(world.root, 'agent-a.env')));
  const log = openSync(join(world.root, 'logs', `worker-${String(launches)}.log`), 'a');
  const env: Record<string, string | undefined> = {
    PATH: safePath(),
    HOME: join(world.root, 'home'),
    TMPDIR: join(world.root, 'tmp'),
    ...settings,
    CONCH_ADAPTER_CMD: join(world.root, 'fixtures', script),
    CONCH_ADAPTER_HEARTBEAT: '1s',
    CONCH_ADAPTER_CHANNEL: channel,
    CONCH_ADAPTER_LOCK_FILE: join(world.root, `worker-${String(launches)}.lock`),
    FIXTURE_SLEEP: String(options.sleep),
  };
  const child = spawn(join(world.root, 'bin', 'conch-adapter'), [], { cwd: world.root, env, stdio: ['ignore', log, log] });
  closeSync(log);
  const exited = () => child.exitCode !== null;
  const stop = async () => {
    if (exited()) return;
    const gone = new Promise<void>((done) => {
      child.once('exit', done);
    });
    child.kill('SIGTERM');
    await Promise.race([gone, new Promise((done) => setTimeout(done, 10_000))]);
    if (!exited()) child.kill('SIGKILL');
  };
  try {
    const deadline = Date.now() + 20_000;
    for (;;) {
      if (exited()) throw new Error(`the adapter exited at once; see ${join(world.root, 'logs', `worker-${String(launches)}.log`)}`);
      const mine = (await roster(world, world.tokens.nick, channel)).find((s) => s.name === 'worker-test' && s.client === 'conch-adapter' && !known.has(s.id));
      if (mine !== undefined) return { session: mine.id, stop };
      if (Date.now() > deadline) throw new Error('the adapter opened no session within 20s');
      await new Promise((done) => setTimeout(done, 200));
    }
  } catch (err) {
    await stop();
    throw err;
  }
}
