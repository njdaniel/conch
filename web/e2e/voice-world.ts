// A community with voice for the browser suite (issue #442): a LiveKit server
// from the same pinned image e2e/voice runs, in Docker, and a conchd of its own
// configured for it, with three members in one channel. The shared p3-local
// server has no LiveKit and stays as it is; this one lives beside it, in its
// own directory, and is removed by stop().
//
// It uses the conchd binary p3-local built into the shared root, so nothing is
// built twice. Without Docker it reports why; the spec decides whether that
// fails (CI) or skips (a developer's machine).
import { closeSync, mkdirSync, mkdtempSync, openSync, rmSync, writeFileSync } from 'node:fs';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
import { repoRoot, safePath, type World } from './world';

// The same image, by version and digest, as e2e/voice/harness.go.
const livekitImage = 'livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3';

export function dockerAvailable(): boolean {
  const r = spawnSync('docker', ['version', '--format', '{{.Server.Version}}'], { encoding: 'utf8', timeout: 20_000 });
  return r.status === 0;
}

function docker(args: string[], env?: Record<string, string | undefined>): { ok: boolean; out: string } {
  const r = spawnSync('docker', args, { encoding: 'utf8', timeout: 120_000, ...(env === undefined ? {} : { env }) });
  return { ok: r.status === 0, out: `${r.stdout}${r.stderr}`.trim() };
}

const random = (n: number) => Math.floor(Math.random() * n);
const hex = (n: number) => Array.from({ length: n }, () => random(16).toString(16)).join('');

async function answers(url: string): Promise<boolean> {
  try {
    return (await fetch(url, { signal: AbortSignal.timeout(2000) })).ok;
  } catch {
    return false;
  }
}

async function waitFor(what: string, ms: number, check: () => Promise<boolean>): Promise<void> {
  const until = Date.now() + ms;
  while (Date.now() < until) {
    if (await check()) return;
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`timed out waiting for ${what}`);
}

export interface Member {
  readonly id: number;
  readonly name: string;
  readonly token: string;
}

export interface VoiceWorld {
  readonly origin: string;
  readonly appUrl: string;
  readonly livekitOrigin: string;
  readonly channel: string;
  readonly alice: Member;
  readonly bob: Member;
  // A third member, for the screen share (issue #444): one shares, one
  // watches, and this one is told who is sharing.
  readonly carol: Member;
  // Every secret of this world, for the check that none shows on a page: the
  // credentials and the LiveKit secret, and every join token and room name a
  // page was given (added by watchPage as the sessions are answered).
  readonly secrets: readonly string[];
  // How many join tokens and room names watchPage has added.
  readonly sessionSecrets: () => number;
  addSecret: (value: string) => void;
  api: (token: string, method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown) => Promise<{ status: number; text: string }>;
  // The audit trail as the operator exports it: one line per event.
  audit: () => Promise<{ actor: string; action: string; subject: string; detail: string }[]>;
  removeFromChannel: (member: Member) => Promise<void>;
  // An operator stops the channel's screen share (issue #444); the status.
  stopShareAsOperator: () => Promise<number>;
  stopLiveKit: () => void;
  startLiveKit: () => Promise<void>;
  stop: () => void;
}

export async function startVoiceWorld(world: World): Promise<VoiceWorld> {
  const dir = mkdtempSync(join(tmpdir(), 'conch-web-voice-'));
  const container = `conch-web-voice-${hex(6)}`;
  const label = `conch-web-voice=${process.env['VOICE_CHECK_RUN'] ?? 'local'}`;
  const key = 'webvoicekey';
  const secret = `web-voice-NOT-FOR-PRODUCTION-${hex(24)}`;
  let server: ChildProcess | undefined;

  const stop = () => {
    docker(['rm', '-f', container]);
    if (server?.pid !== undefined && server.exitCode === null) server.kill('SIGTERM');
    rmSync(dir, { recursive: true, force: true });
  };

  try {
    // LiveKit, on ports of its own, bound to localhost. Ports are drawn at
    // random and tried up to three times, as e2e/voice does.
    let httpPort = 0;
    for (let attempt = 1; ; attempt++) {
      httpPort = 21000 + random(4000);
      const tcpPort = httpPort + 4000;
      const base = 40000 + random(150) * 100;
      const config = `port: ${String(httpPort)}
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: ${String(tcpPort)}
  port_range_start: ${String(base)}
  port_range_end: ${String(base + 99)}
  use_external_ip: false
room:
  auto_create: false
  empty_timeout: 5
  departure_timeout: 5
`;
      mkdirSync(join(dir, 'livekit'), { recursive: true });
      writeFileSync(join(dir, 'livekit', 'livekit.yaml'), config);
      docker(['rm', '-f', container]);
      const run = docker(
        ['run', '-d', '--name', container, '--label', label, '--network', 'host', '-e', 'LIVEKIT_KEYS', '-v', `${join(dir, 'livekit', 'livekit.yaml')}:/etc/livekit.yaml:ro`, livekitImage, '--config', '/etc/livekit.yaml', '--node-ip', '127.0.0.1'],
        { ...process.env, LIVEKIT_KEYS: `${key}: ${secret}` },
      );
      if (!run.ok) throw new Error(`docker run: ${run.out.replaceAll(secret, '[redacted]')}`);
      try {
        await waitFor('LiveKit', 60_000, () => answers(`http://127.0.0.1:${String(httpPort)}`));
        break;
      } catch (err) {
        if (attempt >= 3) throw err;
      }
    }
    const livekitOrigin = `ws://127.0.0.1:${String(httpPort)}`;

    // conchd, with its own data and home, configured for that LiveKit.
    const bin = join(world.root, 'bin', 'conchd');
    const data = join(dir, 'data');
    const home = join(dir, 'home');
    mkdirSync(data, { recursive: true });
    mkdirSync(home, { recursive: true });
    const baseEnv: Record<string, string | undefined> = { PATH: safePath(), HOME: home, XDG_CONFIG_HOME: join(home, 'config'), XDG_DATA_HOME: join(home, 'data') };
    const boot = spawnSync(bin, ['bootstrap-operator', '--data', data, '--name', 'web-voice-operator'], { cwd: repoRoot, env: baseEnv, encoding: 'utf8', timeout: 60_000 });
    const operator = boot.stdout.trim();
    if (boot.status !== 0 || operator === '') throw new Error(`bootstrap-operator failed: ${boot.stderr}`);
    const port = 31000 + random(4000);
    const origin = `http://127.0.0.1:${String(port)}`;
    const log = openSync(join(dir, 'conchd.log'), 'a');
    server = spawn(
      bin,
      ['serve', '--listen', `127.0.0.1:${String(port)}`, '--web-dir', join(repoRoot, 'web', 'dist'), '--data', data, '--livekit-url', livekitOrigin],
      { cwd: dir, env: { ...baseEnv, CONCHD_LIVEKIT_API_KEY: key, CONCHD_LIVEKIT_API_SECRET: secret }, stdio: ['ignore', log, log] },
    );
    closeSync(log);
    await waitFor('conchd', 30_000, () => answers(`${origin}/healthz`));

    const api: VoiceWorld['api'] = async (token, method, path, body) => {
      const res = await fetch(origin + path, {
        method,
        headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(10_000),
      });
      return { status: res.status, text: await res.text() };
    };
    const must = async (what: string, token: string, method: 'POST' | 'PUT', path: string, body?: unknown): Promise<unknown> => {
      const r = await api(token, method, path, body);
      if (r.status < 200 || r.status > 299) throw new Error(`${what}: status ${String(r.status)}`);
      return r.text === '' ? null : (JSON.parse(r.text) as unknown);
    };
    const channel = 'voice-room';
    await must('create channel', operator, 'POST', '/v0/channels', { name: channel });
    const member = async (name: string): Promise<Member> => {
      const made = (await must(`create ${name}`, operator, 'POST', '/v0/principals', { kind: 'human', name })) as { principal: { id: number } };
      const cred = (await must(`credential for ${name}`, operator, 'POST', `/v1/principals/${String(made.principal.id)}/credentials`, { label: name })) as { token: string };
      await must(`add ${name}`, operator, 'PUT', `/v1/channels/${channel}/members/${String(made.principal.id)}`);
      return { id: made.principal.id, name, token: cred.token };
    };
    const alice = await member('alice');
    const bob = await member('bob');
    const carol = await member('carol');
    const secrets = [operator, alice.token, bob.token, carol.token, secret];
    let fromSessions = 0;

    return {
      origin,
      appUrl: `${origin}/app/`,
      livekitOrigin,
      channel,
      alice,
      bob,
      carol,
      secrets,
      sessionSecrets: () => fromSessions,
      addSecret: (value) => {
        if (value === '' || secrets.includes(value)) return;
        secrets.push(value);
        fromSessions++;
      },
      api,
      audit: async () => {
        const r = await api(operator, 'GET', '/v1/audit');
        return r.text
          .split('\n')
          .filter((l) => l !== '')
          .map((l) => JSON.parse(l) as { actor: string; action: string; subject: string; detail: string });
      },
      removeFromChannel: async (m) => {
        const r = await api(operator, 'DELETE' as const, `/v1/channels/${channel}/members/${String(m.id)}`);
        if (r.status < 200 || r.status > 299) throw new Error(`remove ${m.name}: status ${String(r.status)}`);
      },
      stopShareAsOperator: async () => (await api(operator, 'DELETE', `/v1/channels/${channel}/voice/share`)).status,
      stopLiveKit: () => {
        docker(['stop', '-t', '1', container]);
      },
      startLiveKit: async () => {
        const r = docker(['start', container]);
        if (!r.ok) throw new Error(`docker start: ${r.out}`);
        await waitFor('LiveKit', 60_000, () => answers(`http://127.0.0.1:${String(httpPort)}`));
      },
      stop,
    };
  } catch (err) {
    stop();
    throw err;
  }
}

// What a page said and was given, for the checks that no secret shows.
export interface Watch {
  // Every console message and uncaught error of the page, in order.
  readonly console: readonly string[];
  // Resolves once every voice session answer seen so far has been read and
  // its join tokens and room names added to the world's secrets.
  settled: () => Promise<void>;
}

// watchPage records everything page writes to its console, and adds the join
// token and room name of every voice session conchd answers it with to vw's
// secrets. Call it before the page loads.
export function watchPage(page: Page, vw: VoiceWorld): Watch {
  const lines: string[] = [];
  const reads: Promise<void>[] = [];
  page.on('console', (m) => {
    lines.push(`${m.type()}: ${m.text()}`);
  });
  page.on('pageerror', (err) => {
    lines.push(`pageerror: ${err.message}`);
  });
  page.on('response', (res) => {
    if (!new URL(res.url()).pathname.endsWith('/voice/session')) return;
    reads.push(
      res.json().then(
        (body: unknown) => {
          const rooms = (body as { rooms?: unknown }).rooms;
          if (!Array.isArray(rooms)) return;
          for (const g of rooms as { token?: unknown; room?: unknown }[]) {
            if (typeof g.token === 'string') vw.addSecret(g.token);
            if (typeof g.room === 'string') vw.addSecret(g.room);
          }
        },
        () => undefined,
      ),
    );
  });
  return {
    console: lines,
    settled: async () => {
      await Promise.all(reads);
    },
  };
}
