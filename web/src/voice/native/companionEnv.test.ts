import { describe, expect, it, vi } from 'vitest';
import type { CompanionEvent, NativeVoiceView } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import { companionEnv } from './companionEnv';

const view = (deafened: boolean): NativeVoiceView => ({
  status: 'running',
  generation: 1,
  channel: 'general',
  retryable: false,
  helper: { version: 2, phase: 'connected', capture: 'open', transmitting: false, muted: false, deafened, blocked: [], ptt: 'off' } as unknown as NonNullable<NativeVoiceView['helper']>,
});

function source(initial: NativeVoiceView) {
  let current = initial;
  const listeners = new Set<() => void>();
  let onEnd: ((event: CompanionEvent) => void) | undefined;
  const companion = {
    grant: vi.fn(() => Promise.resolve({ ok: false, error: 'not-yet' } as const)),
    end: vi.fn(() => Promise.resolve(true)),
    onEnd: vi.fn((l: (event: CompanionEvent) => void) => {
      onEnd = l;
      return () => undefined;
    }),
  };
  const native = {
    snapshot: () => current,
    subscribe: (l: () => void) => {
      listeners.add(l);
      return () => listeners.delete(l);
    },
    companion,
  } as unknown as NativeVoice;
  return { native, companion, set: (v: NativeVoiceView) => { current = v; for (const l of [...listeners]) l(); }, end: (event: CompanionEvent) => onEnd?.(event), listeners };
}

describe('companionEnv', () => {
  it('is absent in the browser and with a native voice that has no companion', () => {
    expect(companionEnv(undefined)).toBeUndefined();
    const bare = { snapshot: () => view(false), subscribe: () => () => undefined } as unknown as NativeVoice;
    expect(companionEnv(bare)).toBeUndefined();
  });

  it('hands the grant, the end and main’s end reason through', async () => {
    const s = source(view(false));
    const env = companionEnv(s.native);
    expect(await env?.grant('general')).toEqual({ ok: false, error: 'not-yet' });
    expect(s.companion.grant).toHaveBeenCalledWith({ channel: 'general' });
    expect(await env?.end()).toBe(true);
    const reasons: string[] = [];
    env?.onEnd((r) => reasons.push(r));
    s.end({ type: 'end', reason: 'lock' });
    expect(reasons).toEqual(['lock']);
  });

  it('reads the helper’s deafen, false while there is no helper, and follows the view', () => {
    const s = source(view(false));
    const env = companionEnv(s.native);
    expect(env?.deafened()).toBe(false);
    const told = vi.fn();
    const off = env?.onDeafened(told);
    s.set(view(true));
    expect(env?.deafened()).toBe(true);
    expect(told).toHaveBeenCalledTimes(1);
    s.set({ status: 'idle', generation: 2, channel: null, retryable: false });
    expect(env?.deafened()).toBe(false);
    off?.();
    expect(s.listeners.size).toBe(0);
  });
});
