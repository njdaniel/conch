// The SDK adapter against a stub of the SDK: the track is muted straight after
// it is published and after any republish, the capture asks for the browser's
// echo cancellation and noise suppression, and the SDK's logs are redacted and
// kept off the console. No network, no microphone.
import { beforeEach, describe, expect, it, vi } from 'vitest';

type Sink = (...args: unknown[]) => void;
interface Stub {
  handlers: Map<string, (...args: unknown[]) => void>;
  calls: string[];
  captureOptions: unknown;
  logger: { methodFactory: (method: string) => Sink; setLevel: () => void };
}
const state = vi.hoisted(
  (): Stub => ({
    handlers: new Map(),
    calls: [],
    captureOptions: undefined,
    logger: { methodFactory: () => () => undefined, setLevel: () => undefined },
  }),
);

vi.mock('livekit-client', () => {
  class Room {
    localParticipant = {
      publishTrack: () => {
        state.calls.push('publish');
        return Promise.resolve({ source: 'microphone', mute: () => (state.calls.push('mute'), Promise.resolve()), unmute: () => (state.calls.push('unmute'), Promise.resolve()) });
      },
    };
    on(event: string, fn: (...args: unknown[]) => void) {
      state.handlers.set(event, fn);
      return this;
    }
    connect() {
      state.calls.push('connect');
      return Promise.resolve();
    }
    disconnect() {
      return Promise.resolve();
    }
  }
  return {
    Room,
    RoomEvent: { Reconnecting: 'rc', Reconnected: 'rd', Disconnected: 'dc', ActiveSpeakersChanged: 'as', TrackSubscribed: 'ts', TrackUnsubscribed: 'tu', LocalTrackPublished: 'ltp' },
    DisconnectReason: { ROOM_DELETED: 5, PARTICIPANT_REMOVED: 4, DUPLICATE_IDENTITY: 2, SERVER_SHUTDOWN: 3 },
    LogLevel: { warn: 3 },
    LoggerNames: { Default: 'livekit' },
    Track: { Kind: { Audio: 'audio' }, Source: { Microphone: 'microphone' } },
    LocalAudioTrack: function LocalAudioTrack() {
      return undefined;
    },
    createLocalAudioTrack: (options: unknown) => {
      state.captureOptions = options;
      return Promise.resolve({ mediaStreamTrack: { clone: () => ({ enabled: false, stop: () => undefined }) } });
    },
    getLogger: () => state.logger,
    setLogLevel: () => undefined,
  };
});

import { newLiveKitRoom, principalOf, sdkLog } from './sdk';

beforeEach(() => {
  state.handlers.clear();
  state.calls.length = 0;
  vi.stubGlobal('AudioContext', class {
    createAnalyser() {
      return { fftSize: 0, getByteTimeDomainData: () => undefined };
    }
    createMediaStreamSource() {
      return { connect: () => undefined };
    }
    resume() {
      return Promise.resolve();
    }
    close() {
      return Promise.resolve();
    }
  });
  vi.stubGlobal('MediaStream', function MediaStream() {
    return undefined;
  });
});

const events = { onReconnecting: () => undefined, onReconnected: () => undefined, onDisconnected: () => undefined, onSpeakers: () => undefined };

describe('the SDK adapter', () => {
  it('captures with echo cancellation, noise suppression and auto gain, and mutes straight after publishing', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    expect(state.captureOptions).toEqual({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    expect(state.calls).toEqual(['connect', 'publish', 'mute']);
  });

  it('mutes again when the SDK republishes, unless the gate is open', async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    const republish = state.handlers.get('ltp');
    const pub = { source: 'microphone', mute: () => (state.calls.push('republish-mute'), Promise.resolve()) };
    republish?.(pub);
    await room.setSending(true);
    republish?.(pub);
    expect(state.calls.filter((c) => c === 'republish-mute')).toHaveLength(1);
  });

  it('a listen-only grant publishes nothing', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(state.calls).toEqual(['connect']);
  });

  it('keeps the SDK log redacted and in memory', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    const sink = state.logger.methodFactory('warn');
    sink('joined room-abcdef with tok-abcdef', { token: 'tok-abcdef' });
    const lines = sdkLog.recent().join('\n');
    expect(lines).toContain('joined');
    expect(lines).not.toMatch(/room-abcdef|tok-abcdef/);
  });

  it('reads a principal from an identity', () => {
    expect(principalOf('p17')).toBe(17);
    expect(principalOf('x17')).toBeNull();
  });
});
