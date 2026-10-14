// Stops the community and removes its root, then checks that nothing is left:
// no directory, and nothing answering on the server's address. Playwright runs
// this after the last test whether the tests passed or failed.
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { resetRoot } from './global-setup';

async function answers(address: string): Promise<boolean> {
  try {
    await fetch(`${address}/healthz`, { signal: AbortSignal.timeout(2000) });
    return true;
  } catch {
    return false;
  }
}

export default async function globalTeardown(): Promise<void> {
  const root = process.env['CONCH_E2E_ROOT'];
  const parent = process.env['CONCH_E2E_PARENT'];
  if (root === undefined || parent === undefined) return;

  // The address is read before the root goes.
  let address: string | undefined;
  if (existsSync(join(root, 'browser.txt'))) {
    address = /^Address: (http:\/\/\S+?)\/app\/$/m.exec(readFileSync(join(root, 'browser.txt'), 'utf8'))?.[1];
  }
  const reset = resetRoot(root);
  if (!reset.ok) throw new Error(`p3-local --reset failed; ${root} is left for inspection:\n${reset.output}`);
  // The runner removes the root itself; whatever it left is checked for before
  // the parent goes, and the parent is checked after.
  const left: string[] = [];
  if (existsSync(root)) left.push(`${root} is still there after the reset`);
  rmSync(parent, { recursive: true, force: true });
  if (existsSync(parent)) left.push(`${parent} could not be removed`);
  if (address !== undefined && (await answers(address))) left.push(`something still answers on ${address}`);
  if (left.length > 0) throw new Error(`the run left something behind: ${left.join('; ')}`);
}
