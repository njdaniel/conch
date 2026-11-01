import { afterEach, describe, expect, it, vi } from 'vitest';
import { platform } from './index';

afterEach(() => {
  Reflect.deleteProperty(window, 'conchDesktop');
  vi.unstubAllGlobals();
});

const { services } = platform;

describe('the desktop services of attachments, links and the clipboard (#947)', () => {
  it('marks a link for a new window, which main turns into the OS browser, never a window of the app', () => {
    expect(services.externalLink).toEqual({ target: '_blank', rel: 'noopener noreferrer' });
  });

  it('copies with the page’s own clipboard, which main lets through for the app window after a click', async () => {
    const writeText = vi.fn(() => Promise.resolve());
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    await services.copyText('hello');
    expect(writeText).toHaveBeenCalledExactlyOnceWith('hello');
  });

  it('saves a file by asking main for it by number alone', async () => {
    const saveFile = vi.fn(() => Promise.resolve('saved' as const));
    Object.defineProperty(window, 'conchDesktop', { value: { saveFile }, configurable: true });
    expect(await services.saveFile?.(9)).toBe('saved');
    expect(saveFile).toHaveBeenCalledExactlyOnceWith(9);
  });

  it('reports a failure, not a throw, when main refuses or is missing', async () => {
    expect(await services.saveFile?.(9)).toBe('failed');
    const saveFile = vi.fn(() => Promise.reject(new Error('refused')));
    Object.defineProperty(window, 'conchDesktop', { value: { saveFile }, configurable: true });
    expect(await services.saveFile?.(9)).toBe('failed');
  });
});
