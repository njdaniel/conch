// Starts the community the suite runs against: e2e/p3-local --keep, with a
// temporary root of its own. The runner builds conchd, conch and conch-adapter,
// starts conchd on a free loopback port, provisions the fixture community and
// writes the credentials; --relax-invite-limits keeps the specs, which all come
// from 127.0.0.1, from sharing one invite budget (issue #609);
// world.ts reads them from there. The web client is
// built here first and handed to the runner, so that the runner never runs
// `npm ci` under the Playwright that is running.
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { loadWorld, repoRoot, safePath } from './world';

export const webDir = resolve(repoRoot, 'web');

// runnerEnv is the environment of the Go runner: the caller's, with the root it
// works in and the built client it serves, and with no claude on PATH.
export function runnerEnv(root: string): Record<string, string | undefined> {
  return { ...process.env, PATH: safePath(), CONCH_P3_ROOT: root, CONCH_P3_WEB_DIR: join(webDir, 'dist') };
}

// resetRoot stops everything the runner started under root and removes it.
export function resetRoot(root: string): { ok: boolean; output: string } {
  const reset = spawnSync('go', ['run', './e2e/p3-local', '--reset'], { cwd: repoRoot, env: runnerEnv(root), encoding: 'utf8', timeout: 120_000 });
  return { ok: reset.status === 0, output: `${reset.stdout}${reset.stderr}`.trim() };
}

function must(what: string, result: ReturnType<typeof spawnSync>): void {
  if (result.status !== 0) {
    throw new Error(`${what} failed (${String(result.status)}${result.error === undefined ? '' : `, ${result.error.message}`}):\n${result.stdout}\n${result.stderr}`);
  }
}

export default function globalSetup(): void {
  // The runner makes the root itself and refuses one that exists, so the
  // temporary directory holds the root as a child.
  const parent = mkdtempSync(join(tmpdir(), 'conch-web-e2e-'));
  const root = join(parent, 'p3');
  // Set before anything can fail, so that the teardown finds what to remove;
  // the workers inherit both.
  process.env['CONCH_E2E_PARENT'] = parent;
  process.env['CONCH_E2E_ROOT'] = root;
  try {
    must('npm run build', spawnSync('npm', ['run', 'build'], { cwd: webDir, env: { ...process.env, PATH: safePath() }, encoding: 'utf8', timeout: 300_000 }));
    must('go run ./e2e/p3-local --keep --relax-invite-limits', spawnSync('go', ['run', './e2e/p3-local', '--keep', '--relax-invite-limits'], { cwd: repoRoot, env: runnerEnv(root), encoding: 'utf8', timeout: 600_000 }));
    loadWorld();
  } catch (err) {
    // Playwright does not run the teardown of a setup that threw, so this
    // removes what it made: the root through the runner, then the parent. If
    // the reset fails the parent stays, as in the teardown, and the error says so.
    const reset = resetRoot(root);
    if (!reset.ok) {
      throw new Error(`${String(err)}\n\np3-local --reset also failed; ${parent} is left for inspection:\n${reset.output}`);
    }
    rmSync(parent, { recursive: true, force: true });
    throw err;
  }
}
