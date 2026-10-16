// The SDK adapter against a stub of the SDK: the track is muted straight after
// it is published, after any republish and after a remote unmute, the capture
// asks for the browser's echo cancellation and noise suppression, a Leave at
// any point leaves no microphone open, and the SDK's logs are redacted and
// kept off the console. No network, no microphone.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { VoiceSessionResult } from '../api';
import { VoiceClient } from './connection';
import { FakeClock, settle } from './fakes';

type Sink = (...args: unknown[]) => void;
interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (err: unknown) => void;
}
interface StubTrack {
  stopped: number;
  mediaStreamTrack: { clone: () => { enabled: boolean; stop: () => void } };
  stop: () => void;
}
interface StubPub {
  source: string;
  mute: () => Promise<void>;
  unmute: () => Promise<void>;
}
interface Stub {
  handlers: Map<string, (...args: unknown[]) => void>;
  calls: string[];
  captureOptions: unknown;
  logger: { methodFactory: (method: string) => Sink; setLevel: (...args: unknown[]) => void };
  stubFactory: (method: string) => Sink;
  levelCalls: unknown[][];
  // When set, the next capture or publish waits for the test to settle it.
  capture: Deferred<StubTrack> | null;
  publish: Deferred<StubPub> | null;
  publishFails: boolean;
  tracks: StubTrack[];
  room: { localParticipant: unknown } | null;
  // What the stub SDK logs while connecting, as a real one may.
  connectLog: string | null;
  audioContexts: number;
  // The global names that were there before any module of this file loaded.
  baseline: ReadonlySet<string>;
}
const state = vi.hoisted((): Stub => {
  const stubFactory = () => () => undefined;
  const levelCalls: unknown[][] = [];
  return {
    handlers: new Map(),
    calls: [],
    captureOptions: undefined,
    logger: {
      methodFactory: stubFactory,
      setLevel: (...args: unknown[]) => {
        levelCalls.push(args);
      },
    },
    stubFactory,
    levelCalls,
    capture: null,
    publish: null,
    publishFails: false,
    tracks: [],
    room: null,
    connectLog: null,
    audioContexts: 0,
    baseline: new Set(Object.getOwnPropertyNames(globalThis)),
  };
});

vi.mock('livekit-client', () => {
  const pub = (): StubPub => ({
    source: 'microphone',
    mute: () => (state.calls.push('mute'), Promise.resolve()),
    unmute: () => (state.calls.push('unmute'), Promise.resolve()),
  });
  class Room {
    localParticipant = {
      identity: 'p7',
      publishTrack: () => {
        state.calls.push('publish');
        if (state.publishFails) return Promise.reject(new Error('publish refused'));
        return state.publish === null ? Promise.resolve(pub()) : state.publish.promise;
      },
    };
    constructor() {
      state.room = this;
    }
    on(event: string, fn: (...args: unknown[]) => void) {
      state.handlers.set(event, fn);
      return this;
    }
    connect(_url: string, token: string) {
      state.calls.push('connect');
      if (state.connectLog !== null) state.logger.methodFactory('warn')(`${state.connectLog} ${token}`, { token });
      return Promise.resolve();
    }
    disconnect() {
      state.calls.push('disconnect');
      return Promise.resolve();
    }
  }
  return {
    Room,
    RoomEvent: { Reconnecting: 'rc', Reconnected: 'rd', Disconnected: 'dc', ActiveSpeakersChanged: 'as', TrackSubscribed: 'ts', TrackUnsubscribed: 'tu', LocalTrackPublished: 'ltp', TrackUnmuted: 'tum' },
    DisconnectReason: { ROOM_DELETED: 5, PARTICIPANT_REMOVED: 4, DUPLICATE_IDENTITY: 2, SERVER_SHUTDOWN: 3 },
    LogLevel: { warn: 3 },
    LoggerNames: { Default: 'livekit' },
    Track: { Kind: { Audio: 'audio' }, Source: { Microphone: 'microphone' } },
    LocalAudioTrack: function LocalAudioTrack() {
      return undefined;
    },
    createLocalAudioTrack: (options: unknown) => {
      state.captureOptions = options;
      state.calls.push('capture');
      const track: StubTrack = {
        stopped: 0,
        mediaStreamTrack: { clone: () => ({ enabled: false, stop: () => undefined }) },
        stop: () => {
          track.stopped++;
        },
      };
      state.tracks.push(track);
      if (state.capture === null) return Promise.resolve(track);
      const pending = state.capture;
      return pending.promise.then(() => track);
    },
    getLogger: () => state.logger,
    // The SDK's own setLogLevel persists the level; the adapter must not call it.
    setLogLevel: () => {
      state.calls.push('setLogLevel');
    },
  };
});

import { newLiveKitRoom, principalOf, sdkLog } from './sdk';

// What the module did to the SDK's loggers when it loaded, before any test.
const atLoad = { levelCalls: [...state.levelCalls], factoryReplaced: state.logger.methodFactory !== state.stubFactory, calls: [...state.calls] };

function deferred<T>(): Deferred<T> {
  let resolve: (value: T) => void = () => undefined;
  let reject: (err: unknown) => void = () => undefined;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

beforeEach(() => {
  state.handlers.clear();
  state.calls.length = 0;
  state.capture = null;
  state.publish = null;
  state.publishFails = false;
  state.tracks.length = 0;
  state.room = null;
  state.connectLog = null;
  state.audioContexts = 0;
  vi.stubGlobal(
    'AudioContext',
    class {
      constructor() {
        state.audioContexts++;
      }
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
    },
  );
  vi.stubGlobal('MediaStream', function MediaStream() {
    return undefined;
  });
});

const events = { onReconnecting: () => undefined, onReconnected: () => undefined, onDisconnected: () => undefined, onSpeakers: () => undefined };

describe('the SDK adapter', () => {
  it('captures with echo cancellation, noise suppression and auto gain, and mutes straight after publishing', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    expect(state.captureOptions).toEqual({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    expect(state.calls).toEqual(['connect', 'capture', 'publish', 'mute']);
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

  it('mutes again after a remote unmute while the gate is shut, and only then', async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    const unmuted = state.handlers.get('tum');
    const local = state.room?.localParticipant;
    const pub = { source: 'microphone', mute: () => (state.calls.push('remote-unmute-mute'), Promise.resolve()) };
    // The room API unmutes the microphone; the gate is shut, so it is muted again.
    unmuted?.(pub, local);
    expect(state.calls.filter((c) => c === 'remote-unmute-mute')).toHaveLength(1);
    // Someone else's track, and a track that is not the microphone, are left alone.
    unmuted?.(pub, { identity: 'p8' });
    unmuted?.({ ...pub, source: 'camera' }, local);
    // With the gate open the unmute is ours.
    await room.setSending(true);
    unmuted?.(pub, local);
    expect(state.calls.filter((c) => c === 'remote-unmute-mute')).toHaveLength(1);
    await room.setSending(false);
    unmuted?.(pub, local);
    expect(state.calls.filter((c) => c === 'remote-unmute-mute')).toHaveLength(2);
  });

  it('a listen-only grant publishes nothing', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(state.calls).toEqual(['connect']);
  });

  it('a Leave while the microphone is being captured stops it when it arrives, and publishes nothing', async () => {
    state.capture = deferred();
    const room = newLiveKitRoom();
    const connecting = room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    const outcome = connecting.then(
      () => 'resolved',
      () => 'rejected',
    );
    await settle();
    expect(state.calls).toEqual(['connect', 'capture']);
    await room.close();
    state.capture.resolve(undefined as unknown as StubTrack);
    expect(await outcome).toBe('rejected');
    expect(state.tracks).toHaveLength(1);
    expect(state.tracks[0]?.stopped).toBeGreaterThan(0);
    expect(state.calls).not.toContain('publish');
    expect(state.calls).not.toContain('mute');
    expect(state.audioContexts).toBe(0);
  });

  it('a Leave while the track is being published stops it, and does not mute or tap it', async () => {
    state.publish = deferred();
    const room = newLiveKitRoom();
    const outcome = room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events).then(
      () => 'resolved',
      () => 'rejected',
    );
    await settle();
    expect(state.calls).toEqual(['connect', 'capture', 'publish']);
    await room.close();
    // close() stops the captured track at once, publish or not.
    expect(state.tracks[0]?.stopped).toBeGreaterThan(0);
    state.publish.resolve({ source: 'microphone', mute: () => (state.calls.push('mute'), Promise.resolve()), unmute: () => Promise.resolve() });
    expect(await outcome).toBe('rejected');
    expect(state.calls).not.toContain('mute');
    expect(state.audioContexts).toBe(0);
  });

  it('a failed publish stops the microphone and fails the connection', async () => {
    state.publishFails = true;
    await expect(newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events)).rejects.toThrow('publish refused');
    expect(state.tracks[0]?.stopped).toBe(1);
    expect(state.audioContexts).toBe(0);
  });

  it('a closed adapter does not connect again', async () => {
    const room = newLiveKitRoom();
    await room.close();
    await expect(room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events)).rejects.toThrow();
    expect(state.calls).not.toContain('connect');
  });

  it('keeps the SDK log redacted and in memory', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    const sink = state.logger.methodFactory('warn');
    sink('joined room-abcdef with tok-abcdef', { token: 'tok-abcdef' });
    const lines = sdkLog.recent().join('\n');
    expect(lines).toContain('joined');
    expect(lines).not.toMatch(/room-abcdef|tok-abcdef/);
  });

  it('quiets the SDK when the module loads, at warn, without persisting the level', () => {
    expect(atLoad.factoryReplaced).toBe(true);
    // setLevel(warn, persist=false), and never the SDK's persisting setLogLevel.
    expect(atLoad.levelCalls).toEqual([[3, false]]);
    expect(atLoad.calls).not.toContain('setLogLevel');
    expect(state.calls).not.toContain('setLogLevel');
  });

  it('reads a principal from an identity', () => {
    expect(principalOf('p17')).toBe(17);
    expect(principalOf('x17')).toBeNull();
  });
});

// The whole client over the adapter: the join token and the room name of a
// grant are handed to the SDK and kept nowhere a page or another script can
// read them back. The stub SDK logs both while connecting, as a real one may.
describe('the join token and room name', () => {
  const token = 'eyJhbGciOiJIUzI1NiJ9.join-token-body.signature-part';
  const roomName = 'conch-0123456789abcdef0123456789abcdef';

  function holds(text: string): boolean {
    return text.includes(token) || text.includes(roomName) || text.includes('join-token-body');
  }

  // The globals that were there before any voice code loaded. Some belong to
  // the test runner and answer any property (JSON.stringify would call them),
  // so only globals added since are read as JSON; every string global is read.
  const baseline = state.baseline;

  // Every string a script on the page could find: storage, cookies, the DOM,
  // and the own properties of the global object, as text or JSON.
  function pageText(): string {
    const parts: string[] = [JSON.stringify(sessionStorage), JSON.stringify(localStorage), document.cookie, document.documentElement.outerHTML];
    for (const name of Object.getOwnPropertyNames(globalThis)) {
      const desc = Object.getOwnPropertyDescriptor(globalThis, name);
      if (desc === undefined || !('value' in desc)) continue;
      const v: unknown = desc.value;
      if (typeof v === 'string') parts.push(v);
      else if (!baseline.has(name) && v !== null && typeof v === 'object') {
        try {
          parts.push(JSON.stringify(v));
        } catch {
          // Circular or not serialisable: not where a plain value was written.
        }
      }
    }
    return parts.join('\n');
  }

  it('are kept nowhere: not in the client state, the SDK log ring, storage, cookies, the DOM or a global', async () => {
    state.connectLog = `joining ${roomName} with`;
    const session: VoiceSessionResult = {
      ok: true,
      value: { livekit_url: 'wss://lk.example', identity: 'p7', rooms: [{ room: roomName, token, can_publish: true, expires_at: '2026-10-16T00:00:15.000Z' }] },
    };
    const clock = new FakeClock();
    const client = new VoiceClient({
      clock,
      random: () => 0.5,
      createSession: () => Promise.resolve(session),
      report: () => Promise.resolve('ok'),
      newRoom: newLiveKitRoom,
    });
    client.join('general');
    await settle();
    await settle();
    expect(client.snapshot().phase).toBe('connected');
    const snapshot = client.snapshot();
    expect(holds(JSON.stringify({ ...snapshot, speaking: [...snapshot.speaking] }))).toBe(false);
    const ring = sdkLog.recent().join('\n');
    expect(ring).toContain('joining');
    expect(holds(ring)).toBe(false);
    expect(holds(pageText())).toBe(false);

    await client.leave();
    expect(holds(pageText())).toBe(false);
  });

  it('the check finds a token written to a global or to storage', () => {
    // The check above is only as good as pageText; show that it sees a write.
    const g = globalThis as unknown as Record<string, unknown>;
    g['lastVoiceGrant'] = { token };
    expect(holds(pageText())).toBe(true);
    delete g['lastVoiceGrant'];
    sessionStorage.setItem('voice', roomName);
    expect(holds(pageText())).toBe(true);
    sessionStorage.removeItem('voice');
    expect(holds(pageText())).toBe(false);
  });
});
