import { afterEach, describe, expect, it, vi } from 'vitest';
import { platform } from './index';
import type { NotificationClick } from './bridge';

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

describe('desktop notifications (issue #1590)', () => {
  it('uses the OS support state for support, permission and request', async () => {
    const notificationSupported = vi.fn(() => true);
    Object.defineProperty(window, 'conchDesktop', { value: { notificationSupported }, configurable: true });
    expect(services.notifications.supported()).toBe(true);
    expect(services.notifications.permission()).toBe('granted');
    await expect(services.notifications.request()).resolves.toBe('granted');
    expect(notificationSupported).toHaveBeenCalledOnce();
    Object.defineProperty(window, 'conchDesktop', { value: { notificationSupported: () => false }, configurable: true });
    expect(services.notifications.supported()).toBe(false);
    expect(services.notifications.permission()).toBe('unsupported');
    await expect(services.notifications.request()).resolves.toBe('unsupported');
  });

  it('shows exactly the fixed fields, routes a click by tag, and closes by tag', async () => {
    const notify = vi.fn(() => Promise.resolve(true));
    const notifyClose = vi.fn(() => Promise.resolve(true));
    let deliver: (event: NotificationClick) => void = () => undefined;
    Object.defineProperty(window, 'conchDesktop', {
      value: {
        notificationSupported: () => true,
        notify,
        notifyClose,
        onNotification: (listener: (event: NotificationClick) => void) => {
          deliver = listener;
          return () => undefined;
        },
      },
      configurable: true,
    });
    const notification = services.notifications.show('Mention in #general', 'snippet', 'tag-1', 'general');
    const onclick = vi.fn();
    notification.onclick = onclick;
    deliver({ type: 'click', tag: 'tag-1' });
    expect(onclick).toHaveBeenCalledOnce();
    expect(onclick.mock.calls[0]?.[0]).toBeInstanceOf(Event);
    notification.close();
    expect(notify).toHaveBeenCalledExactlyOnceWith({ tag: 'tag-1', title: 'Mention in #general', body: 'snippet', channel: 'general' });
    expect(notifyClose).toHaveBeenCalledExactlyOnceWith({ tag: 'tag-1' });
    deliver({ type: 'click', tag: 'tag-1' });
    expect(onclick).toHaveBeenCalledOnce();
    await Promise.resolve();
  });

  it('fails closed when the bridge or its support answer is absent', async () => {
    expect(services.notifications.permission()).toBe('unsupported');
    expect(() => services.notifications.show('title', 'body', 'missing', 'general')).toThrow(TypeError);
    Object.defineProperty(window, 'conchDesktop', { value: { notificationSupported: () => { throw new Error('refused'); } }, configurable: true });
    expect(services.notifications.permission()).toBe('unsupported');
    await expect(services.notifications.request()).resolves.toBe('unsupported');
  });
});
