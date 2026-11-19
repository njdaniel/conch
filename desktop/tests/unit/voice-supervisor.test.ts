// The Electron owner's supervisor (issue #948) against a fake conch-voice: a
// Node child (tests/fixtures/fake-helper.ts) replaying the transcripts of
// pkg/schema/testdata/voice-control*/ over the real endpoint, spawn and
// protocol modules. No network beyond a Unix socket in a private directory,
// no real helper, no Electron.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { chmodSync } from 'node:fs';
import { join } from 'node:path';

import type { ChildProcessWithoutNullStreams } from 'node:child_process';

import { PASSED_THROUGH, defaultSpawn } from '../../main/voice/spawn.ts';
import { StderrTail, Supervisor, detailFrom, type StartRequest, type Timings, type VoiceState } from '../../main/voice/supervisor.ts';
import { FAKE_TOKEN, fixture, ownerLines, readFixture, scratch, until, type Scratch } from './voice-support.ts';

let s: Scratch;
let logs: { at: number; line: string }[];
let states: VoiceState[];
let sup: Supervisor | null;

beforeEach(() => {
  s = scratch();
  logs = [];
  states = [];
  sup = null;
});
afterEach(async () => {
  await sup?.stop();
  s.cleanup();
});

const ROGUE = {
  CONCH_SERVER: 'https://elsewhere.example',
  CONCH_VOICE_HELPER: '/tmp/other-helper',
  RUST_LOG: 'trace',
  RUST_BACKTRACE: '1',
  LD_PRELOAD: '/nonexistent/conch-test-preload.so',
  // If this reached the fake helper's Node it would not start at all.
  NODE_OPTIONS: '--require /nonexistent/conch-test-require.js',
};

function supervisor(timings: Partial<Timings> = {}): Supervisor {
  sup = new Supervisor({
    resources: s.resources,
    env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, PATH: process.env['PATH'] ?? '', ...ROGUE },
    log: (line) => logs.push({ at: Date.now(), line }),
    timings: { helloTimeoutMs: 5000, ackTimeoutMs: 5000, termAfterMs: 300, killAfterMs: 700, ...timings },
  });
  sup.subscribe((st) => states.push(st));
  return sup;
}

const request: StartRequest = {
  profileId: 'p1',
  configDir: '/nonexistent/config/p1',
  token: FAKE_TOKEN,
  server: 'https://conch.example.org',
  channel: 'general',
  principalId: 7,
};

function stdin(): Record<string, unknown>[] {
  return s
    .reported()
    .filter((r) => r['t'] === 'stdin')
    .map((r) => JSON.parse(String(r['line'])) as Record<string, unknown>);
}

// sameCommands compares what the owner wrote with a transcript's owner side:
// the desktop always says credential env, the transcripts mostly stored.
function withoutCredential(lines: Record<string, unknown>[]): Record<string, unknown>[] {
  return lines.map((l) => Object.fromEntries(Object.entries(l).filter(([k]) => k !== 'credential')));
}

function reported(t: string): Record<string, unknown>[] {
  return s.reported().filter((r) => r['t'] === t);
}

function spawned(): number {
  return reported('argv').length;
}

describe('a session', () => {
  it('v1: join, mute twice, deafen and undeafen, a snapshot, leave; exactly the transcript', async () => {
    const t = 'voice-control/transcript-join-mute-leave.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor();
    const started = await v.start(request);
    expect(started.status).toBe('running');
    if (started.status === 'running') expect(started.helper.version).toBe(1);
    expect(await v.setMute(true)).toEqual({ ok: true });
    expect(await v.setMute(true)).toEqual({ ok: true });
    expect(await v.setDeafen(true)).toEqual({ ok: true });
    expect(await v.setDeafen(false)).toEqual({ ok: true });
    expect(await v.refresh()).toEqual({ ok: true });
    expect(await v.setTalkMode('open')).toEqual({ ok: false, error: 'unsupported' });
    await v.stop();
    expect(v.state).toEqual({ status: 'idle' });
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
    expect(stdin()[0]?.['credential']).toBe('env');
    expect(reported('stdin-end').length).toBe(1);
    // Main never wrote on the events connection.
    expect(reported('events-bytes')).toEqual([]);
  });

  it('v2 is chosen when both sides implement it', async () => {
    const t = 'voice-control-v2/transcript-negotiation-v2-owner.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor();
    const started = await v.start(request);
    expect(started.status === 'running' && started.helper.version).toBe(2);
    if (started.status === 'running') expect(started.helper.talkMode).toBe('push_to_talk');
    expect(await v.refresh()).toEqual({ ok: true });
    await v.stop();
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
    expect(stdin().every((l) => l['schema'] === 'conch.voice_control.v2')).toBe(true);
  });

  it('a helper offering only a version this owner does not implement is refused before any start', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), helloVersions: ['conch.voice_control.v3'] });
    const v = supervisor();
    expect(await v.start(request)).toEqual({ status: 'failed', generation: 1, reason: 'version-mismatch' });
    await until(() => reported('stdin-end').length === 1, 5000, 'standard input to close');
    expect(stdin()).toEqual([]);
  });

  it('the owner asks for a snapshot after a gap in seq', async () => {
    const t = 'voice-control/transcript-gap-resync.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor();
    expect((await v.start(request)).status).toBe('running');
    await until(() => stdin().length === 2, 5000, 'the get_state');
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
  });

  it('a helper that fails by itself (identity mismatch) ends with its reason and is not restarted', async () => {
    s.helper({ transcript: fixture('voice-control/transcript-identity-mismatch.json') });
    const v = supervisor();
    await v.start(request);
    await until(() => v.state.status === 'ended', 5000, 'the end');
    const st = v.state;
    expect(st.status === 'ended' && st.helper.phase).toBe('failed');
    expect(st.status === 'ended' && st.helper.reason).toBe('identity_mismatch');
    await until(() => logs.some((l) => l.line.includes('exited')), 5000, 'the exit');
    await new Promise((r) => setTimeout(r, 300));
    expect(spawned()).toBe(1);
    expect(v.state.status).toBe('ended');
  });

  it('a second start while one runs is refused: one helper per instance', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 });
    const v = supervisor();
    expect((await v.start(request)).status).toBe('running');
    expect((await v.start(request)).status).toBe('failed');
    expect(spawned()).toBe(1);
  });
});

describe('the spawn', () => {
  it('the environment is exactly the allowlist, and the token is never in argv', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 });
    const v = supervisor();
    expect((await v.start(request)).status).toBe('running');
    const env = reported('env')[0]?.['env'] as Record<string, string>;
    for (const name of Object.keys(ROGUE)) expect(env, name).not.toHaveProperty(name);
    expect(env).not.toHaveProperty('PATH');
    // /bin/sh, which runs the fake's wrapper script, sets PWD itself.
    const shell = ['PWD', 'SHLVL', '_', 'OLDPWD'];
    for (const name of Object.keys(env)) expect([...PASSED_THROUGH, ...shell, 'CONCH_CONFIG_DIR', 'CONCH_TOKEN', 'CONCH_VOICE_ATTACH'], name).toContain(name);
    expect(env['CONCH_TOKEN']).toBe(FAKE_TOKEN);
    expect(env['CONCH_CONFIG_DIR']).toBe(request.configDir);
    expect(env['XDG_RUNTIME_DIR']).toBe(s.run);
    const argv = reported('argv')[0]?.['argv'] as string[];
    expect(argv.slice(0, 2)).toEqual(['managed', '--events-pipe']);
    expect(argv[2]?.startsWith(`${s.run}/conch/p1/`)).toBe(true);
    expect(argv.join(' ')).not.toContain(FAKE_TOKEN);
    expect(argv.join(' ')).not.toContain(env['CONCH_VOICE_ATTACH'] ?? '?');
  });

  it('the token and the nonce are in no log, no state and no diagnostic (canary)', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6, leakToStderr: true });
    const v = supervisor();
    expect((await v.start(request)).status).toBe('running');
    await until(() => v.diagnostics().some((l) => l.includes('token=')), 5000, 'the stderr line');
    const nonce = (reported('env')[0]?.['env'] as Record<string, string>)['CONCH_VOICE_ATTACH'] ?? '';
    expect(nonce.length).toBe(43);
    await v.stop();
    const seen = [JSON.stringify(states), JSON.stringify(logs), JSON.stringify(v.diagnostics())].join('\n');
    expect(v.diagnostics().join('\n')).toContain('token=<redacted> nonce=<redacted>');
    expect(seen).not.toContain(FAKE_TOKEN);
    expect(seen).not.toContain(nonce);
  });

  it('a start the helper would refuse is refused before anything is spawned', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json') });
    const v = supervisor();
    expect(await v.start({ ...request, server: 'https://Conch.Example.org/' })).toEqual({ status: 'failed', generation: 1, reason: 'invalid-start' });
    expect(spawned()).toBe(0);
  });

  it('a stop during the start ends it, idle, with nothing left running', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 });
    const v = supervisor();
    const starting = v.start(request);
    await until(() => v.state.status === 'starting', 5000, 'starting');
    await v.stop();
    await starting;
    expect(v.state).toEqual({ status: 'idle' });
    expect(logs.some((l) => l.line === 'voice: helper 1 reaped')).toBe(true);
  });

  it('a missing helper is "not installed" and voice is simply unavailable', async () => {
    const v = supervisor();
    expect(await v.start(request)).toEqual({ status: 'unavailable', reason: 'not-installed' });
    expect(v.state).toEqual({ status: 'unavailable', reason: 'not-installed' });
    await v.stop();
  });

  it('an unusable runtime directory is unavailable with its reason, and nothing is spawned', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json') });
    sup = new Supervisor({ resources: s.resources, env: {}, log: () => undefined });
    expect(await sup.start(request)).toEqual({ status: 'unavailable', reason: 'runtime-dir-unset' });
    expect(spawned()).toBe(0);
  });
});

describe('the attach through the supervisor', () => {
  it('a wrong nonce fails the start and stops the helper', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), attach: 'wrong' });
    const v = supervisor();
    expect(await v.start(request)).toEqual({ status: 'failed', generation: 1, reason: 'attach-refused' });
    await until(() => logs.some((l) => l.line.includes('helper 1 exited')), 5000, 'the exit');
    expect(stdin()).toEqual([]);
  });

  it('no attach within the deadline fails the start', async () => {
    s.helper({ attach: 'none' });
    const v = supervisor({ helloTimeoutMs: 400 });
    expect(await v.start(request)).toEqual({ status: 'failed', generation: 1, reason: 'attach-timeout' });
  });

  it('a second connection fails the start', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), attach: 'twice' });
    const v = supervisor();
    expect(await v.start(request)).toEqual({ status: 'failed', generation: 1, reason: 'attach-second-connection' });
  });
});

describe('the stop', () => {
  it('leave, close both channels, SIGTERM at termAfter, SIGKILL at killAfter, reap; a second start waits for the reap', async () => {
    // The pipe transcript up to the start's ack, then a helper that ignores
    // the end of its input, the events connection and SIGTERM.
    s.helper({ transcript: fixture('voice-control-pipe/transcript-attach-then-hello.json'), stopAt: 5, then: 'hang' });
    const v = supervisor();
    expect((await v.start(request)).status).toBe('running');
    const t0 = Date.now();
    const stopping = v.stop();
    // A second start, now: it must not spawn until the first is reaped.
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 });
    const second = v.start(request);
    await stopping;
    const at = (needle: string): number => logs.find((l) => l.line.includes(needle))?.at ?? -1;
    expect(at('SIGTERM') - t0).toBeGreaterThanOrEqual(280);
    expect(at('SIGTERM') - t0).toBeLessThan(700);
    expect(at('SIGKILL') - t0).toBeGreaterThanOrEqual(680);
    expect(reported('sigterm').length).toBe(1);
    expect((await second).status).toBe('running');
    // The second helper was spawned only after the first was reaped.
    expect(logs.map((l) => l.line).filter((l) => /helper [12] (exited|spawned|reaped)/.test(l))).toEqual([
      'voice: helper 1 spawned',
      'voice: helper 1 exited (SIGKILL)',
      'voice: helper 1 reaped',
      'voice: helper 2 spawned',
    ]);
    // The stop wrote leave, then closed standard input.
    const first = s.reported().filter((r) => r['pid'] === reported('argv')[0]?.['pid']);
    const leave = first.find((r) => r['t'] === 'stdin' && String(r['line']).includes('"leave"'));
    expect(leave).toBeDefined();
    expect(first.some((r) => r['t'] === 'stdin-end')).toBe(true);
  });

  it('stop with nothing running is a no-op', async () => {
    await supervisor().stop();
    expect(states).toEqual([]);
  });
});

describe('helper loss is one state, and nothing restarts the helper', () => {
  const upToRunning = { transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 } as const;
  const malformed = readFixture<{ cases: { direction: string; line?: string; want: { code?: string } }[] }>('voice-control/cases.json').cases.find(
    (c) => c.direction === 'event' && c.want.code === 'malformed' && c.line !== undefined,
  )?.line;

  it.each([
    ['the process exits', { ...upToRunning, then: 'exit' as const, exitCode: 3 }, 'exited'],
    ['the events connection ends', { ...upToRunning, then: 'close-events' as const }, 'events-ended'],
    ['a malformed line arrives', { ...upToRunning, then: 'line' as const, line: malformed ?? '{' }, 'malformed'],
  ])('%s', async (_name, scenario, reason) => {
    s.helper(scenario);
    const v = supervisor();
    // The loss may come in the same read as the start's ack.
    expect(['running', 'lost']).toContain((await v.start(request)).status);
    await until(() => v.state.status === 'lost', 5000, 'the loss');
    expect(v.state).toEqual({ status: 'lost', generation: 1, reason });
    await until(() => logs.some((l) => l.line.includes('helper 1 exited')), 5000, 'the exit');
    await new Promise((r) => setTimeout(r, 300));
    expect(spawned()).toBe(1);
    expect(v.state.status).toBe('lost');
    // The supervisor is still usable: an explicit start works.
    s.helper(upToRunning);
    expect((await v.start(request)).status).toBe('running');
  });

  it('a command while lost is refused, not thrown', async () => {
    s.helper({ ...upToRunning, then: 'exit', exitCode: 3 });
    const v = supervisor();
    await v.start(request);
    await until(() => v.state.status === 'lost', 5000, 'the loss');
    expect(await v.setMute(true)).toEqual({ ok: false, error: 'not-running' });
  });
});

describe('a process error', () => {
  it("an 'error' from a live helper (a failed kill, say) does not mark it reaped", async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6 });
    const children: ChildProcessWithoutNullStreams[] = [];
    sup = new Supervisor({
      resources: s.resources,
      env: { HOME: s.root, XDG_RUNTIME_DIR: s.run },
      log: (line) => logs.push({ at: Date.now(), line }),
      spawn: (command, args, options) => {
        const child = defaultSpawn(command, args, options);
        children.push(child);
        return child;
      },
    });
    expect((await sup.start(request)).status).toBe('running');
    children[0]?.emit('error', new Error('kill failed'));
    await new Promise((r) => setTimeout(r, 200));
    expect(sup.state.status).toBe('running');
    expect(logs.some((l) => l.line.includes('exited'))).toBe(false);
    await sup.stop();
    expect(logs.some((l) => l.line === 'voice: helper 1 reaped')).toBe(true);
  });
});

describe("the helper's standard error", () => {
  // Every 8-character piece of the secret: none may survive, not even a tail.
  function pieces(secret: string): string[] {
    const out: string[] = [];
    for (let i = 0; i + 8 <= secret.length; i++) out.push(secret.slice(i, i + 8));
    return out;
  }

  it.each([0, 1, 20, FAKE_TOKEN.length - 1, FAKE_TOKEN.length])(
    'a token across the line cap (the cap %i characters into it) never appears, not even in part',
    (into) => {
      const tail = new StderrTail([FAKE_TOKEN]);
      const head = `error: auth failed token=${FAKE_TOKEN}`;
      const tokenAt = head.length - FAKE_TOKEN.length;
      // A partial line just long enough that keeping its last 2048
      // characters would cut `into` characters into the token.
      const length = tokenAt + into + 2048;
      tail.push(head + 'x'.repeat(length - head.length));
      tail.push('\nnext line\n');
      const kept = tail.lines.join('\n');
      for (const p of pieces(FAKE_TOKEN)) expect(kept).not.toContain(p);
      expect(tail.lines).toEqual(['<line too long>', 'next line']);
    },
  );

  it('a token split across chunks of one ordinary line is still redacted', () => {
    const tail = new StderrTail([FAKE_TOKEN]);
    tail.push(`token=${FAKE_TOKEN.slice(0, 10)}`);
    tail.push(`${FAKE_TOKEN.slice(10)} done\n`);
    expect(tail.lines).toEqual(['token=<redacted> done']);
  });
});

describe('a failure says why (issue #1850)', () => {
  const JWT = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyLTcifQ.c2lnbmF0dXJlLWJ5dGVzLWhlcmU';

  it('an untrusted refusal logs the HelperError message, naming the folder that failed, and the state carries only the closed cause', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json') });
    chmodSync(s.root, 0o775);
    const v = supervisor();
    const got = await v.start(request);
    expect(got).toEqual({ status: 'unavailable', reason: 'untrusted', cause: 'folder-writable' });
    expect(logs.map((l) => l.line)).toContain(`voice: unavailable: untrusted: ${s.root} may be changed by other users`);
    expect(JSON.stringify(got)).not.toContain(s.root);
    expect(spawned()).toBe(0);
  });

  it('an untrusted refusal for another cause names the file and has no cause', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json') });
    chmodSync(join(s.resources, 'conch-voice'), 0o644);
    const v = supervisor();
    expect(await v.start(request)).toEqual({ status: 'unavailable', reason: 'untrusted' });
    expect(logs.some((l) => l.line === 'voice: unavailable: untrusted: the voice helper is not executable')).toBe(true);
  });

  // connect_failed is a reason to reconnect (the helper stays running);
  // server_unreachable ends the session.
  it.each([
    ['connect_failed', 'voice-control/transcript-rotation-then-duplicate.json', 7, 'running'],
    ['server_unreachable', 'voice-control/transcript-identity-mismatch.json', undefined, 'ended'],
  ] as const)('the detail of %s is logged and carried, redacted of a JWT, a token= value and the nonce', async (reason, transcript, stopAt, status) => {
    s.helper({
      transcript: fixture(transcript),
      ...(stopAt === undefined ? {} : { stopAt }),
      failReason: reason,
      stderrLines: [
        'unrelated line',
        `not connected: ${reason.replace(/_/g, ' ')} (could not reach wss://lk.example/rtc?access_token=${JWT}&x=1 with ${JWT} for {nonce} and token={token}); asking conchd for a new session`,
      ],
    });
    const v = supervisor();
    await v.start(request);
    await until(() => v.state.status === status && ('helper' in v.state && v.state.helper.reason === reason), 5000, 'the state');
    const st = v.state;
    const detail = st.status === 'ended' || st.status === 'running' ? st.detail : undefined;
    expect(detail).toBe('could not reach wss://lk.example/rtc?access_token=<redacted>&x=1 with <redacted> for <redacted> and token=<redacted>');
    const nonce = (reported('env')[0]?.['env'] as Record<string, string>)['CONCH_VOICE_ATTACH'] ?? '';
    const phase = status === 'ended' ? 'ended' : 'reconnecting';
    expect(logs.map((l) => l.line)).toContain(`voice: helper 1 ${phase}: ${reason}: ${detail ?? ''}`);
    if (status === 'ended') {
      // The stderr tail is logged too, redacted the same way.
      await until(() => logs.filter((l) => l.line.startsWith('voice: helper 1 stderr: ')).length === 2, 5000, 'the stderr tail');
    }
    const seen = [JSON.stringify(states), JSON.stringify(logs)].join('\n');
    for (const secret of [FAKE_TOKEN, nonce, JWT, 'eyJhbGci']) expect(seen).not.toContain(secret);
  });

  it('a connection reason the helper gave no words for is logged as such, and carries no detail', async () => {
    s.helper({ transcript: fixture('voice-control/transcript-rotation-then-duplicate.json'), stopAt: 7, failReason: 'connect_failed', stderrLines: ['an unrelated line'] });
    const v = supervisor();
    await v.start(request);
    await until(() => v.state.status === 'running' && v.state.helper.reason === 'connect_failed', 5000, 'the state');
    expect(v.state.status === 'running' && v.state.detail).toBeUndefined();
    expect(logs.map((l) => l.line)).toContain('voice: helper 1 reconnecting: connect_failed: no detail from the helper');
  });

  it('a helper that exits unexpectedly has its redacted stderr tail logged with the prefix', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6, then: 'exit', exitCode: 3, stderrLines: ['boom token={token} {nonce}'] });
    const v = supervisor();
    await v.start(request);
    await until(() => logs.some((l) => l.line.startsWith('voice: helper 1 stderr: ')), 5000, 'the stderr log');
    expect(logs.map((l) => l.line)).toContain('voice: helper 1 stderr: boom token=<redacted> <redacted>');
    expect(JSON.stringify(logs)).not.toContain(FAKE_TOKEN);
  });

  it('a helper that was asked to stop logs no stderr tail', async () => {
    s.helper({ transcript: fixture('voice-control-v2/transcript-negotiation-v2-owner.json'), stopAt: 6, stderrLines: ['chatter'] });
    const v = supervisor();
    await v.start(request);
    await v.stop();
    expect(logs.some((l) => l.line.includes('stderr:'))).toBe(false);
  });
});

describe('the redaction of the stderr tail and the detail (issue #1850)', () => {
  const JWT = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyLTcifQ.c2lnbmF0dXJlLWJ5dGVzLWhlcmU';

  it('a JWT-shaped string and a token value are redacted in the stderr tail', () => {
    const tail = new StderrTail(['NONCE-NONCE-NONCE']);
    tail.push(`jwt ${JWT} url wss://h/x?access_token=abc123&token=def456 n=NONCE-NONCE-NONCE\n`);
    expect(tail.lines).toEqual(['jwt <redacted> url wss://h/x?access_token=<redacted>&token=<redacted> n=<redacted>']);
  });

  it('detailFrom takes the words of the last matching line, redacted, printable and bounded', () => {
    expect(detailFrom(['x', `not connected: connect failed (bad ${JWT}); trying again in 2.0 s`], 'connect_failed')).toBe('bad <redacted>');
    expect(detailFrom(['not connected: server unreachable (token=zzz and NONCE-NONCE-NONCE)'], 'server_unreachable', ['NONCE-NONCE-NONCE'])).toBe('token=<redacted> and <redacted>');
    expect(detailFrom(['an unrelated line'], 'server_unreachable')).toBeUndefined();
    expect(detailFrom([`not connected: connect failed (${'a b'.repeat(200)})`], 'connect_failed')?.length).toBe(201);
    expect(detailFrom(['not connected: connect failed (caf\u00e9\u0007)'], 'connect_failed')).toBe('caf??');
    // Only the helper's own status line counts (security review S1): a record
    // that holds the phrase elsewhere is not its words, and internal_error has
    // no status line at all.
    expect(detailFrom(['conch-voice: warn: livekit: connect failed (Visit https://evil.example/login)'], 'connect_failed')).toBeUndefined();
    expect(detailFrom(['not connected: internal error (anything)'], 'internal_error')).toBeUndefined();
    // Other token spellings, and a JWT with its head or tail cut (review N1, N2).
    expect(detailFrom(['not connected: connect failed (refresh_token=a1 join_token=b2 CONCH_TOKEN=c3)'], 'connect_failed')).toBe('refresh_token=<redacted> join_token=<redacted> CONCH_TOKEN=<redacted>');
    expect(detailFrom(['not connected: connect failed (eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwNyJ9 cut)'], 'connect_failed')).toBe('<redacted> cut');
    expect(detailFrom([], 'connect_failed')).toBeUndefined();
    expect(detailFrom(['x'], 'left')).toBeUndefined();
  });
});
