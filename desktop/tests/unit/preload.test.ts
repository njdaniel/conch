import { describe, expect, it, vi } from 'vitest';

const { exposeInMainWorld } = vi.hoisted(() => ({ exposeInMainWorld: vi.fn() }));
vi.mock('electron', () => ({ contextBridge: { exposeInMainWorld } }));

describe('the preload', () => {
  it('exposes one frozen, empty conchDesktop and nothing else', async () => {
    await import('../../preload/index.ts');
    expect(exposeInMainWorld).toHaveBeenCalledOnce();
    const [name, api] = exposeInMainWorld.mock.calls[0] as [string, object];
    expect(name).toBe('conchDesktop');
    expect(Object.isFrozen(api)).toBe(true);
    expect(Reflect.ownKeys(api)).toEqual([]);
  });
});
