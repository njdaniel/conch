// The hook's talk key: changing the key while the old one is held lets go of
// it, since the old key's keyup is no longer listened for. No SDK, no network.
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

vi.mock('../api', () => ({
  createVoiceSession: () => new Promise(() => undefined),
  reportVoiceTransmit: () => Promise.resolve('ok'),
  getVoicePresence: () => new Promise(() => undefined),
  openVoiceSocket: () => new Promise(() => undefined),
}));
vi.mock('./sdk', () => ({ newLiveKitRoom: () => ({}) }));

import { defaultTalkKey } from './keys';
import { useVoice } from './useVoice';

describe('the talk key', () => {
  it('is let go when it changes while held', () => {
    const { result } = renderHook(() => useVoice({ signedIn: true, channel: null, onUnauthorized: () => undefined }));
    const client = result.current.client;
    const press = vi.spyOn(client, 'press');
    const release = vi.spyOn(client, 'release');
    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { code: defaultTalkKey }));
    });
    expect(press).toHaveBeenCalledTimes(1);
    expect(release).not.toHaveBeenCalled();
    act(() => {
      result.current.setTalkKey('KeyV');
    });
    expect(release).toHaveBeenCalled();
    // The old key's keyup is no longer the talk key's; the new key works.
    release.mockClear();
    act(() => {
      window.dispatchEvent(new KeyboardEvent('keydown', { code: 'KeyV' }));
      window.dispatchEvent(new KeyboardEvent('keyup', { code: 'KeyV' }));
    });
    expect(press).toHaveBeenCalledTimes(2);
    expect(release).toHaveBeenCalledTimes(1);
  });
});
