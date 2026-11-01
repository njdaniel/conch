// A disposable conchd on loopback for the smoke tests (issue #942): built from
// this checkout, with a throwaway data directory, one operator, one channel and
// two members who have a password. Nothing outside the temporary directory is
// touched and nothing is shared with a real server. With web, it also serves
// the browser build in web/dist at /app/, for a browser user beside the app
// (issue #943); make web-build makes it.
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const repo = resolve(import.meta.dirname, '../../..');

export interface Member {
  readonly name: string;
  readonly password: string;
  readonly token: string;
}

export interface Disposable {
  readonly origin: string;
  readonly operatorToken: string;
  readonly member: Member;
  // A second member, another principal, for the browser beside the app.
  readonly other: Member;
  api(token: string, method: string, path: string, body?: unknown): Promise<{ status: number; body: unknown }>;
  stop(): Promise<void>;
}

function freePort(): Promise<number> {
  return new Promise((resolvePort, reject) => {
    const probe = createServer();
    probe.on('error', reject);
    probe.listen(0, '127.0.0.1', () => {
      const address = probe.address();
      const port = typeof address === 'object' && address !== null ? address.port : 0;
      probe.close(() => {
        resolvePort(port);
      });
    });
  });
}

// The release the app's allowlist names first (desktop/build/compat.json): a
// disposable conchd stamped with it is one the packaged app supports, and the
// same binary stamped with any other version is one it does not (issue #946).
export function listedVersion(): string {
  const doc = JSON.parse(readFileSync(join(repo, 'desktop', 'build', 'compat.json'), 'utf8')) as { servers: string[] };
  const first = doc.servers[0];
  if (first === undefined) throw new Error('desktop/build/compat.json lists no server release');
  return first;
}

export const UNLISTED_VERSION = 'v9.9.9-unlisted';

export const WEB_DIST = join(repo, 'web', 'dist');

export async function startConchd(options: { readonly web?: boolean; readonly version?: string } = {}): Promise<Disposable> {
  if (options.web === true && !existsSync(join(WEB_DIST, 'index.html'))) {
    throw new Error(`${WEB_DIST} has no browser build; run make web-build first`);
  }
  const dir = mkdtempSync(join(tmpdir(), 'conch-desktop-conchd-'));
  const binary = join(dir, 'conchd');
  execFileSync('go', ['build', '-ldflags', `-X main.version=${options.version ?? listedVersion()}`, '-o', binary, './cmd/conchd'], { cwd: repo, stdio: 'inherit' });
  const data = join(dir, 'data');
  const boot = execFileSync(binary, ['bootstrap-operator', '--data', data, '--name', 'smoke-operator', '--server-name', 'smokehost'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
  const operatorToken = boot.split('\n')[0]?.trim() ?? '';
  const port = await freePort();
  const origin = `http://127.0.0.1:${String(port)}`;
  const child: ChildProcess = spawn(binary, ['serve', '--data', data, '--listen', `127.0.0.1:${String(port)}`, '--auth', 'required', ...(options.web === true ? ['--web-dir', WEB_DIST] : ['--no-web']), '--test-relax-invite-limits'], {
    stdio: 'ignore',
    env: { PATH: process.env['PATH'] ?? '' },
  });

  const api: Disposable['api'] = async (token, method, path, body) => {
    const response = await fetch(`${origin}${path}`, {
      method,
      headers: { Authorization: `Bearer ${token}`, ...(body === undefined ? {} : { 'Content-Type': 'application/json' }) },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    const text = await response.text();
    return { status: response.status, body: text === '' ? null : (JSON.parse(text) as unknown) };
  };

  const deadline = Date.now() + 30_000;
  for (;;) {
    try {
      if ((await fetch(`${origin}/healthz`)).ok) break;
    } catch {
      // Not listening yet.
    }
    if (Date.now() > deadline) throw new Error('the disposable conchd did not start');
    await new Promise((r) => setTimeout(r, 100));
  }

  const made = await api(operatorToken, 'POST', '/v0/channels', { name: 'general' });
  if (made.status >= 300) throw new Error(`could not make a channel: ${JSON.stringify(made.body)}`);
  const invite = await api(operatorToken, 'POST', '/v1/invites', { kind: 'open', channels: ['general'], voice: false, max_uses: 5 });
  const code = (invite.body as { code?: string }).code;
  if (invite.status >= 300 || code === undefined) throw new Error(`could not make an invite: ${JSON.stringify(invite.body)}`);
  const enrol = async (name: string, password: string): Promise<Member> => {
    const redeemed = await fetch(`${origin}/v2/invites/redeem`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code, name, password }),
    });
    const joined = (await redeemed.json()) as { token?: string };
    if (!redeemed.ok || joined.token === undefined) throw new Error(`could not make a member: ${JSON.stringify(joined)}`);
    return { name, password, token: joined.token };
  };
  const member = await enrol('ada', 'smoke-password-canary-5521');
  const other = await enrol('grace', 'smoke-password-canary-7713');

  return {
    origin,
    operatorToken,
    member,
    other,
    api,
    stop: async () => {
      child.kill('SIGTERM');
      await new Promise((r) => setTimeout(r, 300));
      child.kill('SIGKILL');
      rmSync(dir, { recursive: true, force: true });
    },
  };
}
