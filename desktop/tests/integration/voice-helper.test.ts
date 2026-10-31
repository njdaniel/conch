// Integration (issue #948): the supervisor with the real conch-voice, built by
// `make rust-build` (bin/conch-voice) or named by CONCH_VOICE_BIN, against a
// stub conchd on loopback, with no audio device (--mic none --sink count).
// Same machine, no network beyond loopback, no microphone, no LiveKit.
//
// Without the binary it is skipped and says so; with CI set, a missing binary
// fails instead of skipping. Run with `npm run test:integration` (make
// desktop-voice-it). Real-owner bounds measured from Electron and the stdin
// leak across Chromium's children are #922 LE1, not this test.
import { execFileSync } from 'node:child_process';
import type { ChildProcessWithoutNullStreams } from 'node:child_process';
import { copyFileSync, chmodSync, existsSync } from 'node:fs';
import { createServer, type Server } from 'node:http';
import type { AddressInfo } from 'node:net';
import { join, resolve } from 'node:path';

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import { VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_STOP_KILL_AFTER_MS } from '../../../web/src/schema/generated.ts';
import { HELPER_NAME, defaultSpawn } from '../../main/voice/spawn.ts';
import { Supervisor, type VoiceState } from '../../main/voice/supervisor.ts';
import { FAKE_TOKEN, readFixture, scratch, until, type Scratch } from '../unit/voice-support.ts';

const binary = process.env['CONCH_VOICE_BIN'] ?? resolve(import.meta.dirname, '../../../bin/conch-voice');
const have = existsSync(binary);
const ci = process.env['CI'] === 'true' || process.env['CI'] === '1';

if (!have) {
  const why = `the real conch-voice is not at ${binary}: run make rust-build, or set CONCH_VOICE_BIN`;
  if (ci) throw new Error(`voice-helper integration: ${why}`);
  console.warn(`voice-helper integration: SKIPPED: ${why}`);
}

describe.runIf(have)('the supervisor with the real conch-voice', () => {
  const whoami = readFixture<{ id: number }>('whoami-response-v1.json');
  let conchd: Server;
  let origin = '';
  const seen: string[] = [];

  beforeAll(async () => {
    conchd = createServer((req, res) => {
      seen.push(`${req.method ?? ''} ${req.url ?? ''}`);
      req.resume();
      if (req.method === 'GET' && req.url === '/v1/whoami') {
        res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(whoami));
        return;
      }
      // Every voice session is refused for now, so the helper stays
      // reconnecting: no LiveKit is needed.
      res.writeHead(503, { 'content-type': 'application/json' }).end(JSON.stringify(readFixture('error.json')));
    });
    await new Promise<void>((r) => conchd.listen(0, '127.0.0.1', r));
    origin = `http://127.0.0.1:${String((conchd.address() as AddressInfo).port)}`;
    execFileSync(binary, ['--version'], { stdio: 'ignore' });
  });
  afterAll(async () => {
    await new Promise<void>((r) => conchd.close(() => { r(); }));
  });

  let s: Scratch;
  let logs: { at: number; line: string }[];
  let states: VoiceState[];
  let children: ChildProcessWithoutNullStreams[];
  let nonces: string[];
  let sup: Supervisor;
  beforeEach(() => {
    s = scratch();
    copyFileSync(binary, join(s.resources, HELPER_NAME));
    chmodSync(join(s.resources, HELPER_NAME), 0o755);
    logs = [];
    states = [];
    children = [];
    nonces = [];
    sup = new Supervisor({
      resources: s.resources,
      env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, PIPEWIRE_REMOTE: 'conch-voice-test-no-such-socket', RUST_LOG: 'trace' },
      log: (line) => logs.push({ at: Date.now(), line }),
      spawn: (command, args, options) => {
        nonces.push(options.env?.['CONCH_VOICE_ATTACH'] ?? '');
        const child = defaultSpawn(command, args, options);
        children.push(child);
        return child;
      },
    });
    sup.subscribe((st) => states.push(st));
  });
  afterEach(async () => {
    await sup.stop();
    s.cleanup();
  });

  const start = (): Promise<VoiceState> =>
    sup.start({
      profileId: 'it1',
      configDir: s.config,
      token: FAKE_TOKEN,
      server: origin,
      channel: 'general',
      principalId: whoami.id,
      extraArgs: ['--mic', 'none', '--sink', 'count'],
    });

  const reconnecting = (): boolean => sup.state.status === 'running' && sup.state.helper.phase === 'reconnecting';

  it('start, attach, hello, v2, state; then stop: leave, and the helper exits 0 in time', async () => {
    const started = await start();
    expect(started.status, JSON.stringify({ started, logs, diag: sup.diagnostics() })).toBe('running');
    if (started.status === 'running') expect(started.helper.version).toBe(2);
    await until(reconnecting, 15_000, 'reconnecting');
    expect(seen).toContain('GET /v1/whoami');
    const t0 = Date.now();
    await sup.stop();
    const took = Date.now() - t0;
    expect(sup.state).toEqual({ status: 'idle' });
    expect(logs.map((l) => l.line)).toContain('voice: helper 1 exited (code 0)');
    expect(logs.some((l) => /SIGTERM|SIGKILL/.test(l.line))).toBe(false);
    console.log(`voice-helper integration: stop to reap took ${String(took)} ms`);
    expect(took).toBeLessThan(VOICE_CONTROL_STOP_KILL_AFTER_MS);
    // Neither secret is anywhere the owner shows or logs, the helper's own
    // standard error at trace level included.
    const shown = JSON.stringify([logs, states, sup.diagnostics()]);
    expect(nonces[0]?.length).toBe(43);
    expect(shown).not.toContain(nonces[0] ?? '?');
    expect(shown).not.toContain(FAKE_TOKEN);
  });

  it('owner loss: the end of standard input, and the helper stops with owner_lost and exits within the bound', async () => {
    expect((await start()).status).toBe('running');
    await until(reconnecting, 15_000, 'reconnecting');
    const child = children[0];
    expect(child).toBeDefined();
    const t0 = Date.now();
    child?.stdin.destroy();
    await until(() => logs.some((l) => l.line.startsWith('voice: helper 1 exited')), VOICE_CONTROL_OWNER_LOSS_EXIT_MS + 1000, 'the exit');
    const exited = logs.find((l) => l.line.startsWith('voice: helper 1 exited'));
    console.log(`voice-helper integration: owner loss to exit took ${String((exited?.at ?? 0) - t0)} ms`);
    expect((exited?.at ?? Infinity) - t0).toBeLessThan(VOICE_CONTROL_OWNER_LOSS_EXIT_MS);
    expect(exited?.line).toBe('voice: helper 1 exited (code 0)');
    // The helper said why, before it went.
    const last = states.at(-1);
    expect(last?.status === 'ended' && last.helper.reason).toBe('owner_lost');
    expect(existsSync(join(s.run, 'conch', 'it1'))).toBe(true);
  });
});
