import { EventEmitter } from 'node:events';
import { BrowserWindow } from 'electron';
import { describe, expect, it, vi } from 'vitest';
import { DocumentGate } from '../../main/document.ts';
import type { SettingsFiles } from '../../main/settings.ts';

vi.mock('electron', () => ({ BrowserWindow: vi.fn() }));

const { createWindow, webPreferences, preloadPath, PARTITION, WindowMaximized } = await import('../../main/window.ts');

describe('webPreferences', () => {
  it('is exactly this object in the packaged build', () => {
    expect(webPreferences(true, '/app/out/preload/index.cjs')).toStrictEqual({
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      webSecurity: true,
      webviewTag: false,
      spellcheck: false,
      navigateOnDragDrop: false,
      safeDialogs: true,
      nodeIntegrationInWorker: false,
      nodeIntegrationInSubFrames: false,
      allowRunningInsecureContent: false,
      experimentalFeatures: false,
      plugins: false,
      enableBlinkFeatures: '',
      devTools: false,
      preload: '/app/out/preload/index.cjs',
      partition: 'conch-ui',
    });
  });

  it('differs only in devTools when not packaged', () => {
    expect(webPreferences(false, '/p')).toStrictEqual({ ...webPreferences(true, '/p'), devTools: true });
  });

  it('uses an in-memory partition', () => {
    expect(PARTITION.startsWith('persist:')).toBe(false);
    expect(PARTITION).not.toBe('');
  });

  it('puts the preload inside the app path', () => {
    expect(preloadPath('/x/resources/app.asar')).toBe('/x/resources/app.asar/out/preload/index.cjs');
  });
});

it('remembers maximize across window creation and persists unmaximize across restart', async () => {
  let disk: string | undefined;
  const files: SettingsFiles = {
    read: () => Promise.resolve(disk === undefined ? { kind: 'absent' } : { kind: 'text', text: disk }),
    write: (text) => {
      disk = text;
      return Promise.resolve();
    },
  };
  const log = vi.fn();
  const open = (maximized: InstanceType<typeof WindowMaximized>) => {
    const window = Object.assign(new EventEmitter(), {
      webContents: new EventEmitter(),
      maximize: vi.fn(() => { window.emit('maximize'); }),
      show: vi.fn(),
      loadURL: vi.fn(() => Promise.resolve()),
    });
    vi.mocked(BrowserWindow).mockImplementationOnce(function () {
      return window as unknown as BrowserWindow;
    });
    createWindow(true, '/app', new DocumentGate(), { maximized });
    window.emit('ready-to-show');
    return window;
  };
  const first = new WindowMaximized(files, log);
  await first.load();
  const initial = open(first);
  expect(initial.maximize).not.toHaveBeenCalled();
  initial.emit('maximize');
  initial.emit('minimize');
  initial.emit('closed');
  expect(open(first).maximize).toHaveBeenCalledOnce();
  await first.flush();

  const restarted = new WindowMaximized(files, log);
  await restarted.load();
  const restored = open(restarted);
  expect(restored.maximize).toHaveBeenCalledOnce();
  restored.emit('unmaximize');
  await restarted.flush();

  const normal = new WindowMaximized(files, log);
  await normal.load();
  expect(open(normal).maximize).not.toHaveBeenCalled();
  expect(log).not.toHaveBeenCalled();
});
