// The child-process environment guard (issue #2105) only protects the voice
// helper's credentials if it runs before anything else in main: before the
// scheme, the AppUserModelID and start(), which spawns. The guard's own tests
// (tests/integration/child-process-env.test.ts) import it directly, so this
// file is what fails if main.ts stops calling it, or calls it too late.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

const MAIN = join(import.meta.dirname, '..', '..', 'main');

function code(text: string): string {
  return text.replace(/^\s*\/\/.*$/gm, '');
}

describe('requireSafeChildProcessEnv in main.ts', () => {
  const main = code(readFileSync(join(MAIN, 'main.ts'), 'utf8'));

  it('is called exactly once', () => {
    expect(main.split('requireSafeChildProcessEnv(app.isPackaged)').length - 1).toBe(1);
  });

  it('is the first statement of launch()', () => {
    const body = /function launch\(\)[^{]*\{([\s\S]*?)\n\}/.exec(main)?.[1] ?? '';
    expect(body).not.toBe('');
    const first = body.split('\n').map((l) => l.trim()).find((l) => l !== '');
    expect(first).toBe('requireSafeChildProcessEnv(app.isPackaged);');
  });

  it('runs before registerScheme(), applyAppUserModelId() and start()', () => {
    const at = main.indexOf('requireSafeChildProcessEnv(app.isPackaged)');
    expect(at).toBeGreaterThanOrEqual(0);
    for (const later of ['registerScheme()', 'applyAppUserModelId(app)', 'start()']) {
      expect(main.indexOf(later), later).toBeGreaterThan(at);
    }
  });

  it('launch() is invoked at module scope', () => {
    expect(main).toMatch(/^launch\(\)/m);
  });

  it('main.ts is the entry the main bundle is built from', () => {
    const config = readFileSync(join(import.meta.dirname, '..', '..', 'vite.main.config.ts'), 'utf8');
    expect(config).toMatch(/entry:\s*'main\/main\.ts'/);
  });
});
