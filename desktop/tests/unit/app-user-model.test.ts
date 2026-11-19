// The Windows AppUserModelID (issue #900): set on Windows only, to the
// builder config's appId, and before start() can make a window or a
// notification. Also exercises Windows protocol registration and launch
// delivery (#1944). Written to hold on Windows too (vitest.windows.config.ts).
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

import { afterEach, describe, expect, it, vi } from 'vitest';

import { applyAppUserModelId } from '../../main/app-user-model.ts';
import { acquire, type InstanceApp } from '../../main/instance.ts';
import { Invites, registerInviteProtocol, SWAP_GUARD_MS } from '../../main/invites.ts';

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

describe('Windows invite registration', () => {
  it('registers conch once, leaving an existing registration alone', () => {
    let registered = false;
    const app = {
      isPackaged: true,
      isDefaultProtocolClient: vi.fn(() => registered),
      setAsDefaultProtocolClient: vi.fn(() => (registered = true)),
    };
    expect(registerInviteProtocol(app, 'win32')).toBe(true);
    expect(registerInviteProtocol(app, 'win32')).toBe(true);
    expect(app.setAsDefaultProtocolClient.mock.calls).toEqual([['conch']]);
    expect(app.isDefaultProtocolClient.mock.calls).toEqual([['conch'], ['conch']]);
  });

  it('reports registration failure and leaves other platforms alone', () => {
    const app = { isPackaged: true, isDefaultProtocolClient: vi.fn(() => false), setAsDefaultProtocolClient: vi.fn(() => false) };
    for (const platform of ['linux', 'darwin'] as const) expect(registerInviteProtocol(app, platform)).toBe(true);
    expect(app.isDefaultProtocolClient).not.toHaveBeenCalled();
    expect(app.setAsDefaultProtocolClient).not.toHaveBeenCalled();
    expect(registerInviteProtocol(app, 'win32')).toBe(false);
  });

  it('an unpackaged run leaves the installed handler alone (#1944 review)', () => {
    const app = { isPackaged: false, isDefaultProtocolClient: vi.fn(() => false), setAsDefaultProtocolClient: vi.fn(() => true) };
    expect(registerInviteProtocol(app, 'win32')).toBe(true);
    expect(app.isDefaultProtocolClient).not.toHaveBeenCalled();
    expect(app.setAsDefaultProtocolClient).not.toHaveBeenCalled();
  });

  it('registers in startup only after obtaining the data-root lock', () => {
    const app = code(readFileSync(join(MAIN, 'app.ts'), 'utf8'));
    expect(app.indexOf('registerInviteProtocol(app)')).toBeGreaterThan(app.indexOf('if (!first) return;'));
  });
});

describe('Windows invite launches', () => {
  const exe = 'C:\\Users\\Alice\\AppData\\Local\\Programs\\Conch\\Conch.exe';
  const origin = 'https://chat.example.org';
  const secret = 'CANARY-windows-invite';
  const link = `conch://invite?server=${encodeURIComponent(origin)}&code=${secret}`;
  afterEach(() => vi.useRealTimers());

  // Both arrival paths feed the same router. No Electron runtime, shell,
  // socket or filesystem is needed, so these run in desktop-windows CI.
  function launch(path: 'cold' | 'second', argv: readonly string[]) {
    vi.useFakeTimers();
    const profiles = {
      findByOrigin: () => [],
      view: () => ({ active: 'new-profile' }),
      add: vi.fn(() => Promise.resolve({ ok: true as const, id: 'new-profile' })),
      switchTo: vi.fn(() => Promise.resolve(true)),
    };
    const log = vi.fn();
    const invites = new Invites({ profiles, log, now: Date.now, voiceRunning: () => false });
    if (path === 'cold') invites.arrive(argv);
    else {
      let listener: ((event: unknown, argv: readonly string[]) => void) | undefined;
      const app: InstanceApp = {
        requestSingleInstanceLock: () => true,
        exit: vi.fn(),
        on: (_event, callback) => { listener = callback; },
      };
      acquire(app, vi.fn(), (args) => { invites.arrive(args); });
      listener?.({}, argv);
    }
    return { invites, profiles, log };
  }

  for (const path of ['cold', 'second'] as const) {
    it.each([link, link.replace('//invite?', '//invite/?')])(`${path}: takes the final URL (form %#) without contacting a server until a click`, async (url) => {
      const { invites, profiles, log } = launch(path, [exe, '--original-process-start-time=123', 'C:\\file name.txt', url]);
      const shown = invites.arrived();
      expect(shown).toMatchObject({ ok: true, origin, host: 'chat.example.org', voice: false });
      expect(JSON.stringify(shown)).not.toContain(secret);
      expect(profiles.add).not.toHaveBeenCalled();
      expect(profiles.switchTo).not.toHaveBeenCalled();
      expect(invites.take()).toBeNull();
      if (shown === null || !shown.ok) throw new Error('no invite');
      expect(await invites.accept(shown.handle, { add: true })).toEqual({ ok: false, refusal: 'just-arrived' });
      vi.advanceTimersByTime(SWAP_GUARD_MS);
      expect(await invites.accept(shown.handle, { add: true })).toEqual({ ok: true, switched: true });
      expect(invites.take()).toBe(secret);
      expect(invites.take()).toBeNull();
      expect(JSON.stringify(log.mock.calls)).not.toContain(secret);
    });

    it.each<[string[], string | null]>([
      [[`"${link}"`], null],
      [[`${link}" --inspect=9229`], 'characters'],
      [[`${link}&exec=calc.exe`], 'malformed'],
      [[link.replace(secret, `${secret}%22%20%26%20calc.exe`)], 'code'],
      [[`--url=${link}`], null],
      [[`C:\\downloads\\${link}.url`], null],
      [[`${link}\n`], 'characters'],
      [['conch://not-an-invite', link], 'malformed'],
    ])(`${path}: refuses hostile argv %#`, (args, refusal) => {
      const { invites, profiles, log } = launch(path, [exe, ...args]);
      expect(invites.arrived()).toEqual(refusal === null ? null : { ok: false, refusal });
      expect(invites.arrived()).toBeNull();
      expect(invites.take()).toBeNull();
      expect(profiles.add).not.toHaveBeenCalled();
      expect(profiles.switchTo).not.toHaveBeenCalled();
      expect(JSON.stringify(log.mock.calls)).not.toContain(secret);
    });
  }
});
