// Which voice strip the platform gets (issue #1376): the native one where the
// platform has a native voice, the browser's everywhere else.
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { platform } from './platform';
import type { NativeVoiceView } from './platform/desktop/bridge';
import type { NativeVoice } from './platform/types';
import * as sdk from './voice/sdk';
import { VoiceStatus } from './VoiceStatus';

const idle: NativeVoiceView = { status: 'idle', generation: 0, channel: null, retryable: false };
const nv: NativeVoice = {
  snapshot: () => idle,
  subscribe: () => () => undefined,
  join: () => Promise.reject(new Error('not in this test')),
  leave: () => Promise.reject(new Error('not in this test')),
  retry: () => Promise.reject(new Error('not in this test')),
  setMute: () => Promise.resolve({ ok: true }),
  setDeafen: () => Promise.resolve({ ok: true }),
  setDevices: () => Promise.resolve({ ok: true }),
  setTalkMode: () => Promise.resolve({ ok: true }),
};

function strip() {
  return (
    <VoiceStatus signedIn channel="general" viewer={1} people={new Map()} voiceAllowed reachable channels={[]} occupancy={new Map()} onUnauthorized={() => undefined} onOpen={() => undefined} />
  );
}

beforeEach(() => {
  vi.spyOn(api, 'openVoiceSocket').mockResolvedValue({ ok: true, value: { close: () => undefined } });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  Reflect.deleteProperty(platform, 'nativeVoice');
});

describe('VoiceStatus', () => {
  it('renders the browser strip on the browser platform', () => {
    render(strip());
    expect(screen.getByText('Microphone off')).toBeTruthy();
  });

  it('renders the native strip, and builds no room, where the platform has a native voice', async () => {
    const room = vi.spyOn(sdk, 'newLiveKitRoom');
    Object.defineProperty(platform, 'nativeVoice', { value: nv, configurable: true });
    render(strip());
    await waitFor(() => { expect(screen.getByText('voice off')).toBeTruthy(); });
    expect(screen.queryByText('Microphone off')).toBeNull();
    expect(room).not.toHaveBeenCalled();
  });
});
