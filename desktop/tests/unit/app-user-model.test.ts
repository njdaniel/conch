// The Windows AppUserModelID (issue #900): set on Windows only, to the
// builder config's appId, and before start() can make a window or a
// notification. Written to hold on Windows too (vitest.windows.config.ts).
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

import { describe, expect, it } from 'vitest';

import { applyAppUserModelId } from '../../main/app-user-model.ts';

const MAIN = join(import.meta.dirname, '..', '..', 'main');

function calls(platform: NodeJS.Platform): string[] {
  const ids: string[] = [];
  applyAppUserModelId({ setAppUserModelId: (id) => ids.push(id) }, platform);
  return ids;
}

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
    e.isDirectory() ? sources(join(dir, e.name)) : e.name.endsWith('.ts') ? [join(dir, e.name)] : [],
  );
}

function code(text: string): string {
  return text.replace(/^\s*\/\/.*$/gm, '');
}

describe('applyAppUserModelId', () => {
  it.each<[NodeJS.Platform, string[]]>([
    ['win32', ['chat.conch.desktop']],
    ['linux', []],
    ['darwin', []],
  ])('on %s sets %j', (platform, want) => {
    expect(calls(platform)).toEqual(want);
  });

  it("is the builder config's appId, which the installer's shortcuts carry", () => {
    const config = JSON.parse(readFileSync(join(import.meta.dirname, '../../build/electron-builder.config.json'), 'utf8')) as { appId: string };
    expect(calls('win32')).toEqual([config.appId]);
  });
});

describe('the call order in main.ts', () => {
  const main = code(readFileSync(join(MAIN, 'main.ts'), 'utf8'));

  it('calls it once, at module scope, before start()', () => {
    const at = main.indexOf('applyAppUserModelId(app)');
    const startAt = main.indexOf('start()');
    expect(at).toBeGreaterThanOrEqual(0);
    expect(startAt).toBeGreaterThan(at);
    expect(main.split('applyAppUserModelId(').length - 1).toBe(1);
  });

  it('is the one place in main/ that sets the id', () => {
    const setters = sources(MAIN).filter((p) => /\.setAppUserModelId\(/.test(code(readFileSync(p, 'utf8'))));
    expect(setters.map((p) => p.slice(MAIN.length + 1).replaceAll('\\', '/'))).toEqual(['app-user-model.ts']);
  });
});
