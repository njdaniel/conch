import { describe, expect, it, vi } from 'vitest';

vi.mock('electron', () => ({ BrowserWindow: vi.fn() }));

const { webPreferences, preloadPath, PARTITION } = await import('../../main/window.ts');

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
