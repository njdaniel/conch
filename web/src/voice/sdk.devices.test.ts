// watchMicrophones listens for the browser's device changes itself, and
// removes exactly that listener (issue #846 review). A fake mediaDevices
// counts listeners; jsdom, no device.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { watchMicrophones } from './sdk';

afterEach(() => {
  Reflect.deleteProperty(navigator, 'mediaDevices');
});

describe('watching the microphones', () => {
  it('adds one listener, and cleanup removes it so a later change calls nothing', () => {
    const target = new EventTarget();
    let listeners = 0;
    const add = target.addEventListener.bind(target);
    const remove = target.removeEventListener.bind(target);
    Object.defineProperty(target, 'addEventListener', {
      value: (...a: Parameters<typeof add>) => {
        listeners++;
        add(...a);
      },
    });
    Object.defineProperty(target, 'removeEventListener', {
      value: (...a: Parameters<typeof remove>) => {
        listeners--;
        remove(...a);
      },
    });
    Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: target });
    const changed = vi.fn();
    const stop = watchMicrophones(changed);
    expect(listeners).toBe(1);
    target.dispatchEvent(new Event('devicechange'));
    expect(changed).toHaveBeenCalledTimes(1);
    stop();
    expect(listeners).toBe(0);
    target.dispatchEvent(new Event('devicechange'));
    expect(changed).toHaveBeenCalledTimes(1);
  });

  it('is a no-op where the browser has no media devices', () => {
    Object.defineProperty(navigator, 'mediaDevices', { configurable: true, value: undefined });
    expect(() => {
      watchMicrophones(() => undefined)();
    }).not.toThrow();
  });
});
