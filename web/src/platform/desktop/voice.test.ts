import { describe, expect, it, vi } from 'vitest';
import type { CompanionEvent, CompanionGrantResult, DesktopCompanionBridge, DesktopShareSoundBridge, DesktopVoiceBridge, NativeVoiceView } from './bridge';
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
    // The one-shot helper runs (#1378); the store does not use them.
    voiceDevices: vi.fn(() => Promise.resolve({ ok: false, error: 'unsupported' } as const)),
    voiceSaveDevice: vi.fn(() => Promise.resolve({ ok: false, error: 'unsupported' } as const)),
    // Talk-key setup (#1381).
    voiceKeyDevices: vi.fn(() => Promise.resolve({ ok: false, error: 'unsupported' } as const)),
    voiceSaveTalkBinding: vi.fn(() => Promise.resolve({ ok: false, error: 'unsupported' } as const)),
    reportVoiceDiagnostic: vi.fn(() => Promise.resolve()),
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

describe('createNativeVoice companion (issue #1581)', () => {
  const grant: CompanionGrantResult = { ok: true, grant: { livekitUrl: 'wss://lk.example', token: 't', identity: 'p7.screen', expiresAt: '2026-10-16T00:00:15.000Z' } };

  function withCompanion() {
    const f = fake();
    let listener: ((event: CompanionEvent) => void) | undefined;
    const off = vi.fn();
    const c = {
      companionGrant: vi.fn<DesktopCompanionBridge['companionGrant']>(() => Promise.resolve(grant)),
      companionEnd: vi.fn(() => Promise.resolve(true)),
      shareArm: vi.fn(() => Promise.resolve(true)),
      onCompanion: vi.fn((l: (event: CompanionEvent) => void) => {
        listener = l;
        return off;
      }),
    } satisfies DesktopCompanionBridge;
    return { b: { ...f.b, ...c }, c, off, push: (event: CompanionEvent) => listener?.(event) };
  }

  it('asks the bridge for the grant of the channel and gives the answer as it is', async () => {
    const w = withCompanion();
    const nv = createNativeVoice(() => w.b);
    expect(await nv.companion?.grant({ channel: 'general' })).toEqual(grant);
    expect(w.c.companionGrant).toHaveBeenCalledWith({ channel: 'general' });
  });

  it('gives a typed refusal as it comes, and a failed one for a bridge that throws', async () => {
    const w = withCompanion();
    const nv = createNativeVoice(() => w.b);
    w.c.companionGrant.mockResolvedValueOnce({ ok: false, error: 'not-in-voice' });
    expect(await nv.companion?.grant({ channel: 'general' })).toEqual({ ok: false, error: 'not-in-voice' });
    w.c.companionGrant.mockRejectedValueOnce(new Error('ipc gone'));
    expect(await nv.companion?.grant({ channel: 'general' })).toEqual({ ok: false, error: 'failed' });
  });

  it('arms the share allowance through the bridge, false when it throws or refuses', async () => {
    const w = withCompanion();
    const nv = createNativeVoice(() => w.b);
    expect(await nv.companion?.shareArm()).toBe(true);
    expect(w.c.shareArm).toHaveBeenCalledTimes(1);
    w.c.shareArm.mockRejectedValueOnce(new Error('no click'));
    expect(await nv.companion?.shareArm()).toBe(false);
  });

  it('ends the companion through the bridge, false when it throws', async () => {
    const w = withCompanion();
    const nv = createNativeVoice(() => w.b);
    expect(await nv.companion?.end()).toBe(true);
    w.c.companionEnd.mockRejectedValueOnce(new Error('ipc gone'));
    expect(await nv.companion?.end()).toBe(false);
  });

  it('passes main’s end event to the listener until it stops listening', () => {
    const w = withCompanion();
    const nv = createNativeVoice(() => w.b);
    const seen = vi.fn();
    const stop = nv.companion?.onEnd(seen);
    w.push({ type: 'end', reason: 'renewal' });
    expect(seen).toHaveBeenCalledWith({ type: 'end', reason: 'renewal' });
    stop?.();
    expect(w.off).toHaveBeenCalledTimes(1);
  });

  it('refuses every call, and listens to nothing, without the bridge or with a preload that lacks the companion', async () => {
    for (const get of [() => undefined, () => fake().b]) {
      const nv = createNativeVoice(get);
      expect(await nv.companion?.grant({ channel: 'general' })).toEqual({ ok: false, error: 'failed' });
      expect(await nv.companion?.end()).toBe(false);
      expect(await nv.companion?.shareArm()).toBe(false);
      const seen = vi.fn();
      nv.companion?.onEnd(seen)();
      expect(seen).not.toHaveBeenCalled();
    }
  });
});

describe('createNativeVoice: the share’s sound (issue #1854)', () => {
  function withSound() {
    const f = fake();
    const s = {
      shareSoundStart: vi.fn<DesktopShareSoundBridge['shareSoundStart']>(() => Promise.resolve({ ok: true, share: 4 })),
      shareSoundChoose: vi.fn<DesktopShareSoundBridge['shareSoundChoose']>(() => Promise.resolve({ ok: true, share: null })),
      shareSoundMute: vi.fn<DesktopShareSoundBridge['shareSoundMute']>(() => Promise.resolve({ ok: true })),
      shareSoundEnd: vi.fn<DesktopShareSoundBridge['shareSoundEnd']>(() => Promise.resolve(true)),
      shareSoundShareEnded: vi.fn<DesktopShareSoundBridge['shareSoundShareEnded']>(() => Promise.resolve(true)),
    } satisfies DesktopShareSoundBridge;
    return { b: { ...f.b, ...s }, s };
  }

  it('hands each request to the bridge with exactly its argument, and the answer as it is', async () => {
    const w = withSound();
    const nv = createNativeVoice(() => w.b);
    expect(await nv.shareSound?.start()).toEqual({ ok: true, share: 4 });
    expect(w.s.shareSoundStart).toHaveBeenCalledWith();
    expect(await nv.shareSound?.choose('none')).toEqual({ ok: true, share: null });
    expect(w.s.shareSoundChoose).toHaveBeenCalledWith({ choice: 'none' });
    expect(await nv.shareSound?.mute(true)).toEqual({ ok: true });
    expect(w.s.shareSoundMute).toHaveBeenCalledWith(true);
    expect(await nv.shareSound?.end(4)).toBe(true);
    expect(w.s.shareSoundEnd).toHaveBeenCalledWith({ share: 4 });
    expect(await nv.shareSound?.shareEnded()).toBe(true);
    expect(w.s.shareSoundShareEnded).toHaveBeenCalledWith();
  });

  it('never gives the bridge a scope, an application name or an identifier of its own', async () => {
    const w = withSound();
    const nv = createNativeVoice(() => w.b);
    await nv.shareSound?.start();
    await nv.shareSound?.choose('system_excluding_call');
    for (const call of [...w.s.shareSoundStart.mock.calls, ...w.s.shareSoundChoose.mock.calls]) {
      expect(JSON.stringify(call)).not.toMatch(/application|scope|publisher|pid/);
    }
  });

  it('a refusal by main is the call’s own "no": nothing was added, kept, muted or ended', async () => {
    const w = withSound();
    w.s.shareSoundStart.mockRejectedValueOnce(new Error('refused'));
    w.s.shareSoundChoose.mockRejectedValueOnce(new Error('refused'));
    w.s.shareSoundMute.mockRejectedValueOnce(new Error('refused'));
    w.s.shareSoundEnd.mockRejectedValueOnce(new Error('refused'));
    w.s.shareSoundShareEnded.mockRejectedValueOnce(new Error('refused'));
    const nv = createNativeVoice(() => w.b);
    expect(await nv.shareSound?.start()).toEqual({ ok: false, error: 'not-offered' });
    expect(await nv.shareSound?.choose('none')).toEqual({ ok: false, error: 'not-saved' });
    expect(await nv.shareSound?.mute(true)).toEqual({ ok: false, error: 'lost' });
    expect(await nv.shareSound?.end(1)).toBe(false);
    expect(await nv.shareSound?.shareEnded()).toBe(false);
  });

  it('with no bridge, or a preload that lacks the calls, every call says no and reaches nothing', async () => {
    for (const get of [() => undefined, () => fake().b]) {
      const nv = createNativeVoice(get);
      expect(await nv.shareSound?.start()).toEqual({ ok: false, error: 'not-offered' });
      expect(await nv.shareSound?.choose('none')).toEqual({ ok: false, error: 'not-offered' });
      expect(await nv.shareSound?.mute(true)).toEqual({ ok: false, error: 'unsupported' });
      expect(await nv.shareSound?.end(1)).toBe(false);
    }
  });
});
