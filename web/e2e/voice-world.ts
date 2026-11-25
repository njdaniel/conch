// A community with voice for the browser suite (issue #442): a LiveKit server
// from the same pinned image e2e/voice runs, in Docker, and a conchd of its own
// configured for it, with three members in one channel. The shared p3-local
// server has no LiveKit and stays as it is; this one lives beside it, in its
// own directory, and is removed by stop().
//
// It uses the conchd binary p3-local built into the shared root, so nothing is
// built twice. Without Docker it reports why; the spec decides whether that
// fails (CI) or skips (a developer's machine).
import { closeSync, mkdirSync, mkdtempSync, openSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Page } from '@playwright/test';
import type { BrowserLogEntry } from './console-lines';
import { listIdentities, listTracks, roomCount, untilAnswered, type NoAnswer, type PublishedTrack } from './livekit-admin';
import { tokensInSignal } from './signal-tokens';
import { repoRoot, safePath, type World } from './world';

// The same image, by version and digest, as e2e/internal/lkit/kit.go (which
// e2e/voice and e2e/isolation run).
const livekitImage = 'livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3';

export function dockerAvailable(): boolean {
  const r = spawnSync('docker', ['version', '--format', '{{.Server.Version}}'], { encoding: 'utf8', timeout: 20_000 });
  return r.status === 0;
}

function docker(args: string[], env?: Record<string, string | undefined>): { ok: boolean; out: string } {
  const r = spawnSync('docker', args, { encoding: 'utf8', timeout: 120_000, ...(env === undefined ? {} : { env }) });
  return { ok: r.status === 0, out: `${r.stdout}${r.stderr}`.trim() };
}

// The room timeouts of the livekit.yaml below, in milliseconds, and the longest
// LiveKit may then take to list nobody once the last connection is gone: the
// two run in turn at worst, plus a stated margin of 10 s for a loaded runner.
const livekitEmptyTimeout = 5_000;
const livekitDepartureTimeout = 5_000;
export const emptiesWithin = livekitEmptyTimeout + livekitDepartureTimeout + 10_000;

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

// The line conchd prints once it holds its port (cmd/conchd/main.go), read as
// ops/deploy/conch-restore.sh and e2e/p3-local read it.
const listeningLine = /^conchd \S+ listening on 127\.0\.0\.1:(\d+) \(data /m;

// conchdUp waits up to ms for server, a conchd started with --listen
// 127.0.0.1:0 and its output going to log, to say which port it holds and then
// to answer /healthz there, and returns its origin. If it does not, the error
// says whether conchd is still running or how it ended, and gives the last of
// its output, with redact applied.
async function conchdUp(server: ChildProcess, log: string, ms: number, redact: (s: string) => string): Promise<string> {
  const until = Date.now() + ms;
  const output = () => {
    try {
      return readFileSync(log, 'utf8');
    } catch {
      return '';
    }
  };
  const ended = () => {
    if (server.exitCode !== null) return `exited with status ${String(server.exitCode)}`;
    if (server.signalCode !== null) return `was killed by ${server.signalCode}`;
    return undefined;
  };
  const failed = (what: string) => {
    const tail = output().trimEnd().split('\n').slice(-20).join('\n');
    const state = ended() ?? `is still running after ${String(ms / 1000)}s`;
    return new Error(`conchd ${what}: it ${state}; the last of its output:\n${tail === '' ? '(none)' : redact(tail)}`);
  };
  let origin: string | undefined;
  while (origin === undefined) {
    const port = listeningLine.exec(output())?.[1];
    if (port !== undefined) origin = `http://127.0.0.1:${port}`;
    else if (ended() !== undefined || Date.now() >= until) throw failed('did not say which port it holds');
    else await new Promise((r) => setTimeout(r, 100));
  }
  while (!(await answers(`${origin}/healthz`))) {
    if (ended() !== undefined || Date.now() >= until) throw failed(`did not answer ${origin}/healthz`);
    await new Promise((r) => setTimeout(r, 200));
  }
  return origin;
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
  // The operator's credential, for a spec that makes a group of its own beside
  // the channel (issue #447: a second group must see nothing of this one).
  readonly operator: string;
  // Every secret of this world, for the check that none shows on a page: the
  // credentials and the LiveKit secret, every join token and room name a page
  // was given (added by watchPage as the sessions are answered), and every
  // token LiveKit itself gave a page over its signalling socket, a refreshed
  // one after a join among them (added by watchPage as each frame arrives,
  // issue #1948).
  readonly secrets: readonly string[];
  // How many join tokens and room names watchPage has added from conchd's
  // answers (the tokens LiveKit gave are not counted).
  readonly sessionSecrets: () => number;
  addSecret: (value: string) => void;
  // Adds a token LiveKit gave a page to secrets, without counting it in
  // sessionSecrets.
  addLiveKitToken: (value: string) => void;
  api: (token: string, method: 'GET' | 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown) => Promise<{ status: number; text: string }>;
  // The audit trail as the operator exports it: one line per event.
  audit: () => Promise<{ actor: string; action: string; subject: string; detail: string }[]>;
  removeFromChannel: (member: Member) => Promise<void>;
  // A member made for one test, in the channel, for a test that disables
  // them (issue #516): a disabled principal's credentials never work again,
  // so the three above are not used for that.
  addMember: (name: string) => Promise<Member>;
  // n more separate principals, each in the channel with a credential of its
  // own (issue #1608), named for the call so that two calls do not collide.
  // Their credentials are added to secrets.
  members: (n: number) => Promise<Member[]>;
  // LiveKit's own list of who is connected, asked of LiveKit directly with the
  // world's API secret (livekit-admin.ts): every participant identity in every
  // room, sorted, a person connected twice listed twice. conchd's identities
  // are "p<principal id>", with ".screen" or ".view" for a companion. Room
  // names are never returned, and an error carries an HTTP status only.
  participants: () => Promise<string[] | NoAnswer>;
  // The tracks one member publishes, of one kind, asked of LiveKit directly
  // and sorted by sid: 'microphone' is MICROPHONE, 'screen' is SCREEN_SHARE and
  // SCREEN_SHARE_AUDIO. A room that ends between the calls counts as empty.
  tracks: (member: Pick<Member, 'id'>, kind: 'microphone' | 'screen') => Promise<PublishedTrack[] | NoAnswer>;
  // How many rooms LiveKit lists; 0 once the last has closed.
  rooms: () => Promise<number | NoAnswer>;
  // Freezes the LiveKit container's processes (docker pause) and thaws them:
  // the server stops answering without closing a socket, as an unreachable
  // host does (issue #1612). Always unpause before stop.
  pauseLiveKit: () => void;
  unpauseLiveKit: () => void;
  // An operator disables a principal: conchd renews every voice room they
  // are in.
  disable: (member: Member) => Promise<void>;
  // An operator stops the channel's screen share (issue #444); the status.
  // An operator's stop of someone's stream: since #1508 it names the person
  // (a bare DELETE is the caller's own stream).
  stopShareAsOperator: (who: Member) => Promise<number>;
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
  empty_timeout: ${String(livekitEmptyTimeout / 1000)}
  departure_timeout: ${String(livekitDepartureTimeout / 1000)}
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
    // conchd picks its own port (issue #803): a port guessed here could be
    // taken already, or held by another server that answers /healthz.
    const logPath = join(dir, 'conchd.log');
    const log = openSync(logPath, 'a');
    server = spawn(
      bin,
      // Relaxed invite limits, as the shared server has (global-setup.ts): every
      // sign-in reads /v1/join from 127.0.0.1, and a spec that signs many people
      // in would otherwise run the per-address preview budget dry (issue #2207).
      ['serve', '--listen', '127.0.0.1:0', '--web-dir', join(repoRoot, 'web', 'dist'), '--data', data, '--livekit-url', livekitOrigin, '--test-relax-invite-limits'],
      { cwd: dir, env: { ...baseEnv, CONCHD_LIVEKIT_API_KEY: key, CONCHD_LIVEKIT_API_SECRET: secret }, stdio: ['ignore', log, log] },
    );
    closeSync(log);
    const origin = await conchdUp(server, logPath, 30_000, (s) => s.replaceAll(secret, '[redacted]').replaceAll(operator, '[redacted]'));

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
    const lk = `http://127.0.0.1:${String(httpPort)}`;
    let fromSessions = 0;
    let made = 0;

    return {
      origin,
      appUrl: `${origin}/app/`,
      livekitOrigin,
      channel,
      alice,
      bob,
      carol,
      operator,
      secrets,
      sessionSecrets: () => fromSessions,
      addSecret: (value) => {
        if (value === '' || secrets.includes(value)) return;
        secrets.push(value);
        fromSessions++;
      },
      addLiveKitToken: (value) => {
        if (value === '' || secrets.includes(value)) return;
        secrets.push(value);
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
      addMember: async (name) => {
        const m = await member(name);
        secrets.push(m.token);
        return m;
      },
      members: async (n) => {
        const batch = ++made;
        const out: Member[] = [];
        for (let i = 1; i <= n; i++) {
          const m = await member(`extra${String(batch)}x${String(i)}`);
          secrets.push(m.token);
          out.push(m);
        }
        return out;
      },
      participants: () => untilAnswered(() => listIdentities(lk, key, secret)),
      tracks: (m, kind) =>
        untilAnswered(() => listTracks(lk, key, secret, `p${String(m.id)}`, (source) => (kind === 'microphone' ? source === 'MICROPHONE' : source.startsWith('SCREEN_SHARE')))),
      rooms: () => untilAnswered(() => roomCount(lk, key, secret)),
      pauseLiveKit: () => {
        const r = docker(['pause', container]);
        if (!r.ok) throw new Error(`docker pause: ${r.out}`);
      },
      unpauseLiveKit: () => {
        const r = docker(['unpause', container]);
        if (!r.ok) throw new Error(`docker unpause: ${r.out}`);
      },
      disable: async (m) => {
        await must(`disable ${m.name}`, operator, 'POST', `/v1/principals/${String(m.id)}/disable`);
      },
      stopShareAsOperator: async (who) => (await api(operator, 'DELETE', `/v1/channels/${channel}/voice/share?principal=${String(who.id)}`)).status,
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

// What a page said and was given, for the checks that no secret shows, and
// what went wrong on it that the person would not see (issue #595).
export interface Watch {
  // Every console message and uncaught error of the page, in order.
  readonly console: readonly string[];
  // Every entry the browser logged itself (DevTools Log.entryAdded), as
  // opposed to the page's own console.* calls (Runtime.consoleAPICalled):
  // what lets console-lines.ts tell Chromium's failed-socket line from a page
  // that logs the same text. Chromium only; empty in Firefox and WebKit, where
  // the allowance that needs it then allows nothing.
  readonly browserLog: readonly BrowserLogEntry[];
  // Every request to conchd the browser counted as failed, as its method,
  // path and error: one aborted or never answered, or one answered and its
  // body let go unread (Chromium's net::ERR_ABORTED after a 204).
  readonly failed: readonly string[];
  // Every room token conchd gave this page, read from its answers and never
  // from the world's secrets: the join token of each room in a voice session
  // answer, and the token of each viewer companion answer, in the order the
  // answers were read (so the last is the newest once settled() has
  // resolved); each is in the world's secrets too.
  readonly conchdTokens: readonly string[];
  // Every token LiveKit gave this page over its signalling sockets (a
  // refreshed one after each join, and one on a room move), in the order the
  // frames arrived; each is in the world's secrets too. A JWT-shaped string
  // only signal-tokens.ts's scan found comes with its signed part (ending in
  // a dot), which is no token.
  readonly livekitTokens: readonly string[];
  // Every Permissions-Policy violation the page was told of by a
  // ReportingObserver, as the feature and where in a script it was asked for.
  readonly policy: readonly string[];
  // Resolves once every voice session answer seen so far has been read and
  // its join tokens and room names added to the world's secrets, every frame
  // LiveKit had sent by then has been read for tokens, and every entry the
  // browser had logged by then is in browserLog.
  settled: () => Promise<void>;
}

// Installed before the page's own scripts: hands each Permissions-Policy
// violation report to the test through the binding watchPage exposes, so a
// report made before a reload is kept.
const reportPolicy = () => {
  const tell = (window as unknown as { __conchPolicyReport?: (line: string) => Promise<void> }).__conchPolicyReport;
  new ReportingObserver(
    (reports) => {
      for (const r of reports) {
        // A PermissionsPolicyViolationReportBody, which TypeScript's DOM types
        // do not name.
        const b = (r.body ?? {}) as { featureId?: string; sourceFile?: string | null; lineNumber?: number | null; columnNumber?: number | null };
        void tell?.(`${b.featureId ?? '?'} at ${b.sourceFile ?? '(no file)'}:${String(b.lineNumber ?? '?')}:${String(b.columnNumber ?? '?')}`);
      }
    },
    { types: ['permissions-policy-violation'], buffered: true },
  ).observe();
};

// watchPage records everything page writes to its console, every request to
// conchd that fails and every Permissions-Policy report, and adds to vw's
// secrets the join token and room name of every voice session (and viewer
// companion) conchd answers it with, and every token LiveKit hands it over a
// signalling socket. Call it before the page loads.
export async function watchPage(page: Page, vw: VoiceWorld): Promise<Watch> {
  const lines: string[] = [];
  const failed: string[] = [];
  const policy: string[] = [];
  const reads: Promise<void>[] = [];
  const conchdTokens: string[] = [];
  const livekitTokens: string[] = [];
  page.on('console', (m) => {
    lines.push(`${m.type()}: ${m.text()}`);
  });
  // The browser's own log, through a DevTools session of the test's (Watch.browserLog).
  // Chromium only: web-e2e runs Chromium, and the Firefox and WebKit projects
  // (the @cross tests) have no CDP and allow no failed-socket line. Playwright
  // delivers this session's events apart from its console events, so a Log
  // entry can arrive after the console line it matches; settled() makes a
  // round trip on the session, which Chromium answers only after every entry
  // it had sent before.
  const browserLog: BrowserLogEntry[] = [];
  let browserLogged = (): Promise<void> => Promise.resolve();
  if (page.context().browser()?.browserType().name() === 'chromium') {
    const cdp = await page.context().newCDPSession(page);
    cdp.on('Log.entryAdded', ({ entry }) => {
      browserLog.push({ source: entry.source, level: entry.level, text: entry.text });
    });
    await cdp.send('Log.enable');
    browserLogged = async () => {
      // A failed round trip (the page closed) leaves browserLog short, which
      // only ever allows fewer lines.
      await cdp.send('Runtime.evaluate', { expression: '0' }).catch(() => undefined);
    };
  }
  page.on('pageerror', (err) => {
    lines.push(`pageerror: ${err.message}`);
  });
  page.on('requestfailed', (req) => {
    const url = new URL(req.url());
    if (url.origin !== vw.origin) return;
    failed.push(`${req.method()} ${url.pathname} ${req.failure()?.errorText ?? ''}`);
  });
  await page.exposeFunction('__conchPolicyReport', (line: string) => {
    policy.push(line);
  });
  await page.addInitScript(reportPolicy);
  page.on('response', (res) => {
    const path = new URL(res.url()).pathname;
    const session = path.endsWith('/voice/session');
    if (!session && !path.endsWith('/voice/companion')) return;
    reads.push(
      res.json().then(
        (body: unknown) => {
          // A session answers with a grant per room; a viewer companion with
          // one grant of its own (VoiceCompanionResponseV1).
          const rooms = session ? (body as { rooms?: unknown }).rooms : [body];
          if (!Array.isArray(rooms)) return;
          for (const g of rooms as ({ token?: unknown; room?: unknown } | null)[]) {
            if (typeof g?.token === 'string') {
              vw.addSecret(g.token);
              conchdTokens.push(g.token);
            }
            if (typeof g?.room === 'string') vw.addSecret(g.room);
          }
        },
        () => undefined,
      ),
    );
  });
  // LiveKit's own tokens (issue #1948): after a join it sends the page a
  // refreshed token, which the SDK reconnects with and conchd never sees.
  // They are read here from the frames the browser received on a socket to
  // LiveKit's origin, as each arrives, and never by having the page log them.
  // The handler runs before Playwright reports anything later on the page, so
  // a token is a secret before the page could show it.
  const livekit = new URL(vw.livekitOrigin).origin;
  page.on('websocket', (ws) => {
    if (new URL(ws.url()).origin !== livekit) return;
    ws.on('framereceived', ({ payload }) => {
      for (const t of tokensInSignal(typeof payload === 'string' ? payload : new Uint8Array(payload))) {
        vw.addLiveKitToken(t);
        if (!livekitTokens.includes(t)) livekitTokens.push(t);
      }
    });
  });
  return {
    console: lines,
    browserLog,
    failed,
    conchdTokens,
    livekitTokens,
    policy,
    settled: async () => {
      await Promise.all(reads);
      // A round trip on Playwright's own session to the page: its answer comes
      // after every frame event the page had by then, so each has been read.
      // A closed page has no more frames to give.
      await page.evaluate(() => 0).catch(() => undefined);
      await browserLogged();
    },
  };
}
