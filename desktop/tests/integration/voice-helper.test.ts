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
import type { AddressInfo, Socket } from 'node:net';
import { join, resolve } from 'node:path';

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';

import { VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_STOP_KILL_AFTER_MS } from '../../../web/src/schema/generated.ts';
import type { VoiceSessionResponseV1 } from '../../../web/src/schema/generated.ts';
import { HELPER_NAME, defaultSpawn } from '../../main/voice/spawn.ts';
import { OneShot } from '../../main/voice/oneshot.ts';
import { CEILING_ARGS, VoiceService } from '../../main/voice/service.ts';
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

// Synthetic values, no real credential: a JWT-shaped canary the helper's own
// redaction layers should never let through if an error body echoed it back.
const JWT_CANARY = 'eyJhbGciOiJub25lIn0.eyJzdWIiOiJzeW50aGV0aWMtY2FuYXJ5In0.c3ludGhldGljLXNpZ25hdHVyZS1jYW5hcnk';

// listening binds a stub server to a loopback port and rejects on the server's
// 'error' event, so a bind failure fails the test at once instead of hanging
// until the test timeout.
function listening(server: Server): Promise<void> {
  return new Promise<void>((resolveListen, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      server.off('error', reject);
      resolveListen();
    });
  });
}

// The helper's logger writes its own records at the level asked and everything
// from the SDK only at warn and error (voice/conch-voice/src/logger.rs), so no
// SDK line can show that the helper ran at trace. Its own lines can: the
// presence-socket refusal is written at debug, below the default level, and
// the 503 the stub conchd answers makes the helper write it on every start.
// A run at a lower level than debug does not show it, so its absence fails
// the test. The helper has no trace-only record of its own on this path, so
// this proves "debug or finer", and the --log-level trace argument itself is
// what asks for trace.
const RAN_VERBOSE = /^conch-voice: debug: the presence socket could not be opened/;
const ranVerbose = (lines: readonly string[]): boolean => lines.some((l) => RAN_VERBOSE.test(l));
// RAN_VERBOSE cannot tell debug from trace, so the level asked for is checked
// on the command line itself: the helper was started with --log-level trace.
const askedForTrace = (argv: readonly string[] | undefined): boolean => {
  const i = argv?.lastIndexOf('--log-level') ?? -1;
  return i >= 0 && argv?.[i + 1] === 'trace';
};

// NotLiveKit listens where a session says LiveKit is and answers every HTTP
// request and every upgrade with 404 and a body of synthetic secrets. Like
// NotLiveKit in voice/conch-voice/tests/binary.rs, it keeps what reached it
// and closes every connection it holds.
class NotLiveKit {
  readonly requests: string[] = [];
  private readonly sockets = new Set<Socket>();
  url = '';
  private readonly server: Server;

  constructor(echo: () => string) {
    this.server = createServer((req, res) => {
      this.requests.push(`${req.url ?? ''} ${req.headers.authorization ?? ''}`);
      req.resume();
      const body = Buffer.from(echo());
      res.writeHead(404, { 'content-type': 'text/plain', 'content-length': body.length, connection: 'close' }).end(body);
    });
    this.server.on('upgrade', (req, socket: Socket) => {
      this.requests.push(`${req.url ?? ''} ${req.headers.authorization ?? ''}`);
      const body = Buffer.from(echo());
      socket.on('error', () => undefined);
      socket.end(`HTTP/1.1 404 Not Found\r\nContent-Type: text/plain\r\nContent-Length: ${String(body.length)}\r\nConnection: close\r\n\r\n${body.toString('latin1')}`);
    });
    this.server.on('connection', (socket: Socket) => {
      this.sockets.add(socket);
      socket.on('close', () => this.sockets.delete(socket));
    });
  }

  async listen(): Promise<this> {
    await listening(this.server);
    this.url = `ws://127.0.0.1:${String((this.server.address() as AddressInfo).port)}`;
    return this;
  }

  async close(): Promise<void> {
    for (const s of this.sockets) s.destroy();
    await new Promise<void>((r) => this.server.close(() => { r(); }));
  }
}

// No audio device, and the helper's log at trace. spawn.ts leaves RUST_LOG out
// of the helper's environment by design, so the level is set by the supported
// option, --log-level, which the supervisor passes through as an extra argument.
const HELPER_ARGS = ['--mic', 'none', '--sink', 'count', '--log-level', 'trace'];

describe.runIf(have)('the supervisor with the real conch-voice', () => {
  const whoami = readFixture<{ id: number }>('whoami-response-v1.json');
  const template = readFixture<VoiceSessionResponseV1>('voice-session-response-v1.json');
  const [grant] = template.rooms;
  let conchd: Server;
  let origin = '';
  const seen: string[] = [];
  // Where the stub says LiveKit is; empty refuses every session (the default).
  let livekitUrl = '';

  beforeAll(async () => {
    conchd = createServer((req, res) => {
      seen.push(`${req.method ?? ''} ${req.url ?? ''}`);
      req.resume();
      if (req.method === 'GET' && req.url === '/v1/whoami') {
        res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(whoami));
        return;
      }
      if (livekitUrl !== '' && req.method === 'POST' && req.url === '/v1/channels/general/voice/session') {
        const expires = new Date(Date.now() + 10 * 60_000).toISOString();
        const session: VoiceSessionResponseV1 = { ...template, livekit_url: livekitUrl, rooms: template.rooms.map((r) => ({ ...r, expires_at: expires })) };
        res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify(session));
        return;
      }
      // Unless a test supplies an SDK endpoint, refuse the session so the
      // helper stays reconnecting without contacting LiveKit.
      res.writeHead(503, { 'content-type': 'application/json' }).end(JSON.stringify(readFixture('error.json')));
    });
    await listening(conchd);
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
  // Each spawn's argument list, and the helper's standard error as it wrote it,
  // before the supervisor's own redaction and its 32-line tail: the secret
  // checks read both, so a line the tail evicted, or one the supervisor
  // scrubbed, cannot hide a helper that leaked.
  let argvs: (readonly string[])[];
  let rawStderr: string[];
  let sup: Supervisor;
  let notLiveKit: NotLiveKit | null;
  beforeEach(() => {
    seen.length = 0;
    livekitUrl = '';
    notLiveKit = null;
    s = scratch();
    copyFileSync(binary, join(s.resources, HELPER_NAME));
    chmodSync(join(s.resources, HELPER_NAME), 0o755);
    logs = [];
    states = [];
    children = [];
    nonces = [];
    argvs = [];
    rawStderr = [];
    sup = new Supervisor({
      resources: s.resources,
      env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, PIPEWIRE_REMOTE: 'conch-voice-test-no-such-socket' },
      log: (line) => logs.push({ at: Date.now(), line }),
      spawn: (command, args, options) => {
        nonces.push(options.env?.['CONCH_VOICE_ATTACH'] ?? '');
        argvs.push([...args]);
        const child = defaultSpawn(command, args, options);
        // A second listener only reads; the supervisor's own reader is unaffected.
        child.stderr.on('data', (chunk: Buffer) => rawStderr.push(chunk.toString('utf8')));
        children.push(child);
        return child;
      },
    });
    sup.subscribe((st) => states.push(st));
  });
  afterEach(async () => {
    try {
      await sup.stop();
    } finally {
      try {
        await notLiveKit?.close();
      } finally {
        livekitUrl = '';
        s.cleanup();
      }
    }
  });

  const start = (): Promise<VoiceState> =>
    sup.start({
      profileId: 'it1',
      configDir: s.config,
      token: FAKE_TOKEN,
      server: origin,
      channel: 'general',
      principalId: whoami.id,
      extraArgs: HELPER_ARGS,
    });

  const reconnecting = (): boolean => sup.state.status === 'running' && sup.state.helper.phase === 'reconnecting';

  it('start, attach, hello, v2, state; then stop: leave, and the helper exits 0 in time', async () => {
    const started = await start();
    expect(started.status, JSON.stringify({ started, logs, diag: sup.diagnostics() })).toBe('running');
    if (started.status === 'running') expect(started.helper.version).toBe(2);
    await until(reconnecting, 15_000, 'reconnecting');
    // The helper's standard error is read apart from its events, so the
    // record may arrive a moment after the state does.
    await until(() => ranVerbose(sup.diagnostics()), 15_000, 'the helper\'s debug record');
    expect(seen).toContain('GET /v1/whoami');
    const t0 = Date.now();
    await sup.stop();
    const took = Date.now() - t0;
    expect(sup.state).toEqual({ status: 'idle' });
    expect(logs.map((l) => l.line)).toContain('voice: helper 1 exited (code 0)');
    expect(logs.some((l) => /SIGTERM|SIGKILL/.test(l.line))).toBe(false);
    console.log(`voice-helper integration: stop to reap took ${String(took)} ms`);
    expect(took).toBeLessThan(VOICE_CONTROL_STOP_KILL_AFTER_MS);
    // The record was seen before the stop (see RAN_VERBOSE), so the check
    // below covers the helper's standard error at the level asked, and not
    // only at its default.
    // Neither secret is anywhere the owner shows or logs, the helper's own
    // standard error included (the SDK's records only at warn and error).
    expect(askedForTrace(argvs[0]), JSON.stringify(argvs[0])).toBe(true);
    const shown = JSON.stringify([logs, states, sup.diagnostics(), rawStderr]);
    expect(nonces[0]?.length).toBe(43);
    expect(shown).not.toContain(nonces[0] ?? '?');
    expect(shown).not.toContain(FAKE_TOKEN);
  });

  // D3-2 (#1373), D-9: the service's start carries the ceiling flag, and the
  // real helper accepts it (it refuses an unknown option), so drift in the
  // flag's name fails here.
  it('the service joins with --no-mute-key and --transmit-ceiling-secs 30 and the real helper accepts them', async () => {
    const service = new VoiceService({
      locked: () => false,
      supervisor: sup,
      oneShot: new OneShot({ resources: s.resources, env: { HOME: s.root, XDG_RUNTIME_DIR: s.run }, log: () => undefined }),
      identity: () =>
        Promise.resolve({
          profileId: 'it1',
          configDir: s.config,
          principalId: whoami.id,
          credential: () => ({ origin, token: FAKE_TOKEN }),
          current: () => true,
        }),
      log: (line) => logs.push({ at: Date.now(), line }),
    });
    // The service passes the ceiling; the test adds only the no-audio options.
    const spy = sup.start.bind(sup);
    sup.start = (req) => spy({ ...req, extraArgs: [...(req.extraArgs ?? []), ...HELPER_ARGS] });
    expect(CEILING_ARGS).toEqual(['--transmit-ceiling-secs', '30']);
    const view = await service.join({ channel: 'general', muted: false, deafened: false });
    expect(view.status, JSON.stringify({ view, logs, diag: sup.diagnostics() })).toBe('running');
    expect(view.channel).toBe('general');
    await until(reconnecting, 15_000, 'reconnecting');
    await service.leave();
    expect(service.state().status).toBe('idle');
    expect(logs.map((l) => l.line)).toContain('voice: helper 1 exited (code 0)');
    service.close();
  });

  // #1861: the real SDK is reached (a session is granted, LiveKit is a
  // loopback endpoint that answers 404), the helper's connect error travels
  // by standard error and its state by the events endpoint, and the owner
  // shows the words, redacted and bounded. What the SDK reflects of the
  // body is not assumed: only that nothing synthetic is shown.
  it('a real SDK connection failure shows connect_failed with bounded, redacted detail, and a stop reaps the helper', async () => {
    const room = grant?.room ?? '?';
    const joinToken = grant?.token ?? '?';
    expect(room).not.toBe('?');
    expect(joinToken).not.toBe('?');
    notLiveKit = new NotLiveKit(
      () => `join token ${joinToken} room ${room} login ${FAKE_TOKEN} jwt ${JWT_CANARY} control \u0001\u0007\u001b[31m end`,
    );
    await notLiveKit.listen();
    livekitUrl = notLiveKit.url;

    const started = await start();
    expect(started.status, JSON.stringify({ started, logs, diag: sup.diagnostics() })).toBe('running');

    // The status line and the event travel apart, so the first reconnecting
    // state may have no detail: look for the one that has.
    const failed = (): VoiceState | undefined =>
      states.find((st) => (st.status === 'running' || st.status === 'ended') && st.helper.reason === 'connect_failed' && (st.detail ?? '') !== '');
    await until(() => failed() !== undefined, 30_000, 'connect_failed with a detail');
    const shownState = failed();
    const detail = shownState?.status === 'running' || shownState?.status === 'ended' ? (shownState.detail ?? '') : '';
    const failureDiagnostics = sup.diagnostics();

    // Both ends were reached: conchd's session route and the SDK's endpoint.
    expect(seen).toContain('POST /v1/channels/general/voice/session');
    expect(notLiveKit.requests.length).toBeGreaterThan(0);
    expect(notLiveKit.requests.join('\n')).toContain(joinToken);

    // Actionable (the SDK's own words for the refusal), printable, bounded:
    // 200 printable ASCII characters and at most one ellipsis.
    // The SDK reports the refusal as "signal failure: client error: 404 Not
    // Found"; the words after it are the echoed body and are not asserted.
    expect(detail).toMatch(/signal failure: client error: 404 Not Found/);
    expect(detail).toMatch(/^[\x20-\x7e]+…?$/);
    expect(detail.length).toBeLessThanOrEqual(201);
    // The same words are in the log.
    expect(logs.map((l) => l.line).some((l) => /^voice: helper 1 (reconnecting|ended): connect_failed: /.test(l) && l.endsWith(`connect_failed: ${detail}`))).toBe(true);

    const t0 = Date.now();
    await sup.stop();
    const took = Date.now() - t0;
    expect(sup.state).toEqual({ status: 'idle' });
    expect(logs.map((l) => l.line)).toContain('voice: helper 1 exited (code 0)');
    expect(logs.some((l) => /SIGTERM|SIGKILL/.test(l.line))).toBe(false);
    expect(took).toBeLessThan(VOICE_CONTROL_STOP_KILL_AFTER_MS);

    // The helper ran verbose during the failure, not only afterwards.
    expect(ranVerbose(failureDiagnostics)).toBe(true);
    // The printable and JWT-canary checks bite only when the SDK echoes the
    // refusal body (it does today, in "client error: 404 Not Found - <body>"):
    // if a later SDK drops the body they pass vacuously, which the join-token
    // and room checks share. The body reaching the stub is asserted above.
    expect(askedForTrace(argvs[0]), JSON.stringify(argvs[0])).toBe(true);
    const shown = JSON.stringify([logs, states, failureDiagnostics, sup.diagnostics(), rawStderr]);
    expect(nonces[0]?.length).toBe(43);
    for (const secret of [nonces[0] ?? '?', FAKE_TOKEN, joinToken, room, JWT_CANARY]) {
      expect(shown).not.toContain(secret);
    }
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
