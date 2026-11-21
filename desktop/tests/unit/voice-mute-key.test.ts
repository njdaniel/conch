import { EventEmitter } from 'node:events';
import { readFileSync } from 'node:fs';
import { PassThrough } from 'node:stream';
import { expect, it, vi } from 'vitest';

import { watchMuteKey } from '../../main/voice/mute-key.ts';
import { spawnHelper } from '../../main/voice/spawn.ts';

vi.mock('../../main/voice/spawn.ts', async (load) => ({
  ...await load<typeof import('../../main/voice/spawn.ts')>(),
  findHelper: vi.fn(() => Promise.resolve('/fake/conch-voice')),
  spawnHelper: vi.fn(),
}));

const pressLine = readFileSync(new URL('../../../pkg/schema/testdata/mute-key-event-v1.json', import.meta.url), 'utf8');

async function start() {
  const child = Object.assign(new EventEmitter(), {
    stdout: new PassThrough(),
    stdin: new PassThrough(),
    stderr: new PassThrough(),
  });
  vi.mocked(spawnHelper).mockReturnValueOnce(child as unknown as ReturnType<typeof spawnHelper>);
  const log = vi.fn();
  const press = vi.fn();
  const stop = watchMuteKey('/fake', {}, log, '/fake/profile', press);
  await Promise.resolve();
  return { child, log, press, stop };
}

it('handles fragmented lines and multiple presses in one chunk', async () => {
  const { child, press, log, stop } = await start();
  child.stdout.write(pressLine.slice(0, 8));
  expect(press).not.toHaveBeenCalled();
  child.stdout.write(pressLine.slice(8) + pressLine);
  expect(press).toHaveBeenCalledTimes(2);
  expect(log).not.toHaveBeenCalled();
  stop();
  expect(child.stdin.destroyed).toBe(true);
});

it.each([
  'bad JSON\n',
  pressLine.replace('v1', 'v2'),
  pressLine.replace('true', 'false'),
  pressLine.replace('true', 'true,"press":true'),
  pressLine.replace('true', 'true,"code":30'),
  '\n',
])('closes and logs once on invalid watcher output', async (invalid) => {
  const { child, press, log } = await start();
  child.stdout.write(invalid + pressLine);
  child.emit('close', 1);
  expect(child.stdin.destroyed).toBe(true);
  expect(press).not.toHaveBeenCalled();
  expect(log).toHaveBeenCalledOnce();
});

it('refuses an unterminated event at EOF', async () => {
  const { child, press, log } = await start();
  child.stdout.end(pressLine.trimEnd());
  await vi.waitFor(() => { expect(log).toHaveBeenCalledOnce(); });
  expect(child.stdin.destroyed).toBe(true);
  expect(press).not.toHaveBeenCalled();
});
