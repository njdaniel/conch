// What the browser suite knows of the community e2e/p3-local --keep set up: read
// from the files that program writes into its root (<participant>.env,
// browser.txt, operator.env, commands.txt, pids.json) and nothing else. Tests
// observe through the API with these credentials and act through the page.
import { existsSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { whoAmIResponseV1 } from '../src/schema/principal';
import type { Validator } from '../src/schema/validate';

// The repository, found from web/e2e.
export const repoRoot = resolve(import.meta.dirname, '..', '..');

// PATH without any directory that holds a program called claude. The real
// binary must never run from this suite (the scripted agents are fixtures), so
// nothing it starts can find one.
export function safePath(): string {
  const dirs = (process.env['PATH'] ?? '').split(':').filter((dir) => dir !== '');
  const kept = dirs.filter((dir) => !existsSync(join(dir, 'claude')));
  const dropped = dirs.filter((dir) => !kept.includes(dir));
  // A directory that holds claude is dropped whole. If that took go or node
  // with it, say so rather than let the runner fail with "go: not found".
  for (const program of ['go', 'node']) {
    if (!kept.some((dir) => existsSync(join(dir, program)))) {
      throw new Error(
        `${program} is not on PATH once the directories that hold claude are dropped (${dropped.join(', ') || 'none dropped'}); ` +
          `the suite never lets claude be found, so put ${program} in a directory of its own`,
      );
    }
  }
  return kept.join(':');
}

export interface World {
  readonly root: string;
  // http://127.0.0.1:<port>
  readonly origin: string;
  readonly appUrl: string;
  readonly tokens: Readonly<Record<'browser' | 'nick' | 'nick2' | 'guest' | 'worker' | 'observer' | 'operator', string>>;
  // The credential id of nick-test's second credential, for revoking it.
  readonly nick2CredentialId: number;
  // The process that is serving, from pids.json, and what it was started with.
  readonly serverPid: () => number;
}

// envFile reads a KEY=value file that the runner wrote for a shell to source.
export function envFile(path: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const line of readFileSync(path, 'utf8').split('\n')) {
    const m = /^([A-Z0-9_]+)=(.*)$/.exec(line);
    if (m?.[1] === undefined || m[2] === undefined) continue;
    const raw = m[2];
    out.set(m[1], raw.startsWith("'") && raw.endsWith("'") ? raw.slice(1, -1).replaceAll("'\\''", "'") : raw);
  }
  return out;
}

function envValue(file: string, key: string): string {
  const value = envFile(file).get(key);
  if (value === undefined || value === '') throw new Error(`${file} has no ${key}`);
  return value;
}

let cached: World | undefined;

export function loadWorld(): World {
  if (cached !== undefined) return cached;
  const root = process.env['CONCH_E2E_ROOT'];
  if (root === undefined) throw new Error('CONCH_E2E_ROOT is not set: the global setup did not run');
  const token = (part: string) => envValue(join(root, `${part}.env`), 'CONCH_TOKEN');

  // browser.txt: "Address: http://host:port/app/", then the credential.
  const browserTxt = readFileSync(join(root, 'browser.txt'), 'utf8').split('\n');
  const address = /^Address: (http:\/\/\S+?)\/app\/$/.exec(browserTxt[0] ?? '')?.[1];
  if (address === undefined) throw new Error('browser.txt has no address line');
  const browser = (browserTxt[2] ?? '').trim();
  if (browser === '') throw new Error('browser.txt has no credential');

  // commands.txt ends with the values the walkthrough names.
  const credential = /^ {2}credential tui-a2 (\d+)$/m.exec(readFileSync(join(root, 'commands.txt'), 'utf8'))?.[1];
  if (credential === undefined) throw new Error('commands.txt does not name the credential of tui-a2');

  cached = {
    root,
    origin: address,
    appUrl: `${address}/app/`,
    tokens: {
      browser,
      nick: token('tui-a'),
      nick2: token('tui-a2'),
      guest: token('tui-b'),
      worker: token('agent-a'),
      observer: token('agent-b'),
      operator: token('operator'),
    },
    nick2CredentialId: Number(credential),
    serverPid: () => {
      const pids: unknown = JSON.parse(readFileSync(join(root, 'pids.json'), 'utf8'));
      if (!Array.isArray(pids)) throw new Error('pids.json is not a list');
      const entry = (pids as { name?: unknown; pid?: unknown }[]).find((p) => p.name === 'conchd');
      if (typeof entry?.pid !== 'number') throw new Error('pids.json does not name conchd');
      return entry.pid;
    },
  };
  return cached;
}

// Every credential in the community, for the check that none leaks.
export function allTokens(world: World): string[] {
  return Object.values(world.tokens);
}

// ---------------------------------------------------------------- the API

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

export interface Reply {
  readonly status: number;
  readonly body: unknown;
}

// call makes one request as the holder of token.
export async function call(world: World, token: string, method: 'GET' | 'POST' | 'DELETE', path: string, body?: unknown): Promise<Reply> {
  const res = await fetch(world.origin + path, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: 'application/json',
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(10_000),
  });
  const text = await res.text();
  let parsed: unknown = null;
  try {
    parsed = text === '' ? null : JSON.parse(text);
  } catch {
    parsed = text;
  }
  return { status: res.status, body: parsed };
}

// ok is a successful call whose body has the shape the web client's schema
// module says it has (those are checked against pkg/schema's golden fixtures).
export async function ok<T>(
  world: World,
  token: string,
  method: 'GET' | 'POST' | 'DELETE',
  path: string,
  check: Validator<T>,
  body?: unknown,
): Promise<T> {
  const reply = await call(world, token, method, path, body);
  if (reply.status < 200 || reply.status > 299) throw new ApiError(reply.status, `${method} ${path}: status ${String(reply.status)}`);
  const problems = check(reply.body);
  if (problems.length > 0) throw new Error(`${method} ${path}: ${problems.join('; ')}`);
  return reply.body as T;
}

export async function principalId(world: World, token: string): Promise<number> {
  return (await ok(world, token, 'GET', '/v1/whoami', whoAmIResponseV1)).id;
}
