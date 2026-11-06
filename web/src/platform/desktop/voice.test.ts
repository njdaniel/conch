import { describe, expect, it, vi } from 'vitest';
import type { DesktopVoiceBridge, NativeVoiceView } from './bridge';
import { createNativeVoice } from './voice';

const v = (generation: number, over: Partial<NativeVoiceView> = {}): NativeVoiceView => ({ status: 'running', generation, channel: 'general', retryable: false, ...over });

function fake(initial: NativeVoiceView = v(1)) {
  let listener: ((view: NativeVoiceView) => void) | undefined;
  const unsubscribe = vi.fn();
  const b = {
    voiceState: vi.fn(() => Promise.resolve(initial)),
    voiceJoin: vi.fn(() => Promise.resolve(v(5))),
    voiceLeave: vi.fn(() => Promise.resolve(v(8, { status: 'idle', channel: null }))),
    voiceRetry: vi.fn(() => Promise.resolve(v(7))),
    voiceSetMute: vi.fn(() => Promise.resolve({ ok: true } as const)),
    voiceSetDeafen: vi.fn(() => Promise.resolve({ ok: false, error: 'busy' } as const)),
    voiceSetDevices: vi.fn(() => Promise.resolve({ ok: true } as const)),
    voiceSetTalkMode: vi.fn(() => Promise.resolve({ ok: true } as const)),
    onVoiceState: vi.fn((l: (view: NativeVoiceView) => void) => {
      listener = l;
      return unsubscribe;
    }),
  } satisfies DesktopVoiceBridge;
  return { b, unsubscribe, push: (view: NativeVoiceView) => listener?.(view) };
}

const flush = () => new Promise((r) => setTimeout(r, 0));

describe('createNativeVoice', () => {
  it('is unavailable without a bridge, and every call refuses by name', async () => {
    const nv = createNativeVoice(() => undefined);
    expect(nv.snapshot().status).toBe('unavailable');
    expect(await nv.join({ channel: 'general' })).toEqual({ ok: false, error: 'unsupported' });
    expect(await nv.setMute(true)).toEqual({ ok: false, error: 'unsupported' });
  });

  it('starts idle, follows onVoiceState, and keeps the snapshot stable', async () => {
    const f = fake();
    const nv = createNativeVoice(() => f.b);
    expect(nv.snapshot().status).toBe('idle');
    const first = nv.snapshot();
    expect(nv.snapshot()).toBe(first);
    const l = vi.fn();
    const off = nv.subscribe(l);
    await flush();
    expect(nv.snapshot().generation).toBe(1);
    f.push(v(2, { status: 'failed', problem: 'connect_failed' }));
    expect(nv.snapshot().status).toBe('failed');
    expect(l).toHaveBeenCalledTimes(2);
    off();
    expect(f.unsubscribe).toHaveBeenCalledTimes(1);
  });

  it('subscribes to the bridge once for many listeners', () => {
    const f = fake();
    const nv = createNativeVoice(() => f.b);
    const a = nv.subscribe(() => undefined);
    const c = nv.subscribe(() => undefined);
    expect(f.b.onVoiceState).toHaveBeenCalledTimes(1);
    a();
    expect(f.unsubscribe).not.toHaveBeenCalled();
    c();
    expect(f.unsubscribe).toHaveBeenCalledTimes(1);
  });

  it('ignores a view older than the one held', async () => {
    const f = fake(v(1));
    const nv = createNativeVoice(() => f.b);
    nv.subscribe(() => undefined);
    f.push(v(9));
    await flush();
    expect(nv.snapshot().generation).toBe(9);
    f.push(v(3));
    expect(nv.snapshot().generation).toBe(9);
  });

  it('delegates and returns the bridge result', async () => {
    const f = fake();
    const nv = createNativeVoice(() => f.b);
    expect(await nv.join({ channel: 'general' })).toEqual({ ok: true, view: v(5) });
    expect(f.b.voiceJoin).toHaveBeenCalledWith({ channel: 'general', muted: false, deafened: false });
    expect(nv.snapshot().generation).toBe(5);
    expect(await nv.retry()).toEqual({ ok: true, view: v(7) });
    expect((await nv.leave()).ok).toBe(true);
    expect(nv.snapshot().status).toBe('idle');
    expect(await nv.setMute(true)).toEqual({ ok: true });
    expect(f.b.voiceSetMute).toHaveBeenCalledWith(true);
    expect(await nv.setDeafen(true)).toEqual({ ok: false, error: 'busy' });
    expect(await nv.setDevices({ input: 'a' })).toEqual({ ok: true });
    expect(f.b.voiceSetDevices).toHaveBeenCalledWith({ input: 'a' });
    expect(await nv.setTalkMode('open')).toEqual({ ok: true });
    expect(f.b.voiceSetTalkMode).toHaveBeenCalledWith('open');
  });

  it('turns a rejected call into a typed error, never a throw', async () => {
    const f = fake();
    f.b.voiceJoin.mockRejectedValueOnce('boom');
    f.b.voiceSetMute.mockRejectedValueOnce(new Error('x'));
    const nv = createNativeVoice(() => f.b);
    expect(await nv.join({ channel: 'general' })).toEqual({ ok: false, error: 'lost' });
    expect(await nv.setMute(true)).toEqual({ ok: false, error: 'lost' });
  });
});
