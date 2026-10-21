// The SDK adapter against a stub of the SDK: the track is muted before it is
// published and straight after, after any republish and after a remote unmute, the capture
// asks for the browser's echo cancellation and noise suppression, a Leave at
// any point leaves no microphone open, and the SDK's logs are redacted and
// kept off the console. The screen share (issue #444): what the picker is
// asked for, the simulcast layers and the sound published, how a share's end
// is told apart, and the watch view's subscription choices. The share's
// encodings follow what was captured (issue #565). The microphone chosen
// (issue #658): captured exactly, published as a track the page provides,
// moved to another device by a restart of that track, and lost when its
// capture ends or its device is no longer listed. Voice processing (issue
// #659): all three filters on or off at every capture, a change made by a
// restart and never on the live capture, and reported as the capture says.
// The speakers chosen (issue #660): listed without asking for any device,
// switched by the SDK's own switch for outputs, '' being the system default,
// handed to a new room before it connects, and followed when the SDK moves
// sound by itself; not asked at all where the browser cannot choose.
// No network, no microphone, no screen.
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
// One capture of a device, as the browser gives it: the device it is on, and
// the clones the tap made of it.
interface StubMedia {
  deviceId: string;
  readyState: string;
  stopped: number;
  clones: { enabled: boolean; stopped: number; stop: () => void }[];
  clone: () => { enabled: boolean; stopped: number; stop: () => void };
  stop: () => void;
  // Issue #659: the voice processing the capture says it has.
  filters: StubFilters;
  getSettings: () => { deviceId: string } & StubFilters;
}
interface StubFilters {
  echoCancellation?: boolean;
  noiseSuppression?: boolean;
  autoGainControl?: boolean;
}
// The track the adapter publishes: the page's own (user-provided) wrapper of
// a capture, as LocalAudioTrack is.
interface StubTrack {
  stopped: number;
  userProvided: boolean;
  source: string;
  constraints: unknown;
  mediaStreamTrack: StubMedia;
  stop: () => void;
  mute: () => Promise<void>;
  restartTrack: (options: unknown) => Promise<void>;
  on: (event: string, fn: () => void) => StubTrack;
  // The browser ends the capture (the device unplugged).
  ended: () => void;
}
interface StubDevice {
  kind: string;
  deviceId: string;
  label: string;
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
  // When set, the next capture, mute of the captured track, or publish waits
  // for the test to settle it.
  capture: Deferred<undefined> | null;
  premute: Deferred<undefined> | null;
  publish: Deferred<StubPub> | null;
  publishFails: boolean;
  tracks: StubTrack[];
  room: { localParticipant: unknown } | null;
  // What the stub SDK logs while connecting, as a real one may.
  connectLog: string | null;
  audioContexts: number;
  // The global names that were there before any module of this file loaded.
  baseline: ReadonlySet<string>;
  // Issue #444: what the room was made and joined with, the share's tracks
  // as captured, and what was published with which options.
  roomOptions: unknown;
  connectOptions: unknown;
  screenOptions: unknown;
  screenTracks: StubScreenTrack[];
  published: { source: string; options: unknown }[];
  remote: Map<string, { trackPublications: Map<string, StubRemotePub> }>;
  // When set, an unpublish never answers (a hung signalling connection).
  unpublishHangs: boolean;
  // Issue #525: whether LiveKit has told this connection that the share's
  // sources are granted. Without it the SDK refuses a share's publish at
  // once, as the real one does.
  shareGranted: boolean;
  // Issue #535: when set, the next share publish the SDK accepts waits for
  // the test to settle it, pending as the SDK has it.
  sharePublish: Deferred<undefined> | null;
  // When set, the next share publish waits before the SDK has it as pending
  // (the SDK waits out a reconnect first): an unpublish meanwhile finds
  // nothing.
  shareGate: Deferred<undefined> | null;
  // The share publishes the SDK has as pending, by track, and the track of
  // each entry of published.
  pending: Map<unknown, Promise<unknown>>;
  owners: Map<object, unknown>;
  // Issue #565: the size and frame rate the next picture captured says it
  // has (its track's settings); 1080p at 30 fps unless a test says otherwise.
  captured: { width?: number; height?: number; frameRate?: number };
  // Issue #658: the captures made, what each restart was asked for, when set
  // the next restart waits for the test or fails, the devices the browser
  // lists, what each listing was asked, and the audio contexts made.
  medias: StubMedia[];
  restarts: unknown[];
  restart: Deferred<undefined> | null;
  restartFails: Error | null;
  devices: StubDevice[];
  listed: unknown[][];
  contexts: { closed: number }[];
  // Issue #659: the filters the browser keeps on whatever it is asked.
  keptOn: string[];
  // Issue #660: whether the browser can choose an output, each output the
  // SDK was asked to switch to, and when set, a refusal of the next switch.
  outputSelection: boolean;
  outputs: string[];
  outputFails: Error | null;
}
interface StubSettings {
  channelCount?: number;
  width?: number;
  height?: number;
  frameRate?: number;
}
interface StubScreenTrack {
  source: string;
  stopped: number;
  mediaStreamTrack: { readyState: string; enabled: boolean; getSettings: () => StubSettings; addEventListener: (type: string, fn: () => void) => void; ended: () => void };
  // What getSettings answers; a test may change it, as a capture can change size.
  settings: StubSettings;
  stop: () => void;
}
interface StubRemotePub {
  source: string;
  isSubscribed: boolean;
  setSubscribed: (on: boolean) => void;
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
    premute: null,
    publish: null,
    publishFails: false,
    tracks: [],
    room: null,
    connectLog: null,
    audioContexts: 0,
    baseline: new Set(Object.getOwnPropertyNames(globalThis)),
    roomOptions: undefined,
    connectOptions: undefined,
    screenOptions: undefined,
    screenTracks: [],
    published: [],
    remote: new Map(),
    unpublishHangs: false,
    shareGranted: true,
    sharePublish: null,
    shareGate: null,
    pending: new Map(),
    owners: new Map(),
    captured: { width: 1920, height: 1080, frameRate: 30 },
    medias: [],
    restarts: [],
    restart: null,
    restartFails: null,
    devices: [],
    listed: [],
    contexts: [],
    keptOn: [],
    outputSelection: true,
    outputs: [],
    outputFails: null,
  };
});

vi.mock('livekit-client', () => {
  class PublishTrackError extends Error {
    status: number;
    constructor(message: string, status: number) {
      super(message);
      this.status = status;
    }
  }
  const pub = (): StubPub => ({
    source: 'microphone',
    mute: () => (state.calls.push('mute'), Promise.resolve()),
    unmute: () => (state.calls.push('unmute'), Promise.resolve()),
  });
  const screenTrack = (source: string): StubScreenTrack => {
    const listeners: (() => void)[] = [];
    const track: StubScreenTrack = {
      source,
      stopped: 0,
      settings: source === 'screen_share' ? { ...state.captured } : { channelCount: 2 },
      mediaStreamTrack: {
        readyState: 'live',
        enabled: true,
        getSettings: () => ({ ...track.settings }),
        addEventListener: (type, fn) => {
          if (type === 'ended') listeners.push(fn);
        },
        // The browser ends the capture (its own stop control).
        ended: () => {
          track.mediaStreamTrack.readyState = 'ended';
          for (const fn of listeners) fn();
        },
      },
      stop: () => {
        track.stopped++;
        track.mediaStreamTrack.readyState = 'ended';
      },
    };
    return track;
  };
  // A capture of the device a constraint names exactly, or of the default.
  // It has the voice processing asked for, but for any filter the browser
  // keeps on (state.keptOn).
  const media = (options: unknown): StubMedia => {
    const asked: Record<string, unknown> = typeof options === 'object' && options !== null ? { ...options } : {};
    const deviceId = asked['deviceId'];
    const exact = typeof deviceId === 'object' && deviceId !== null && 'exact' in deviceId ? String(deviceId.exact) : 'default';
    const filters: StubFilters = {};
    for (const name of ['echoCancellation', 'noiseSuppression', 'autoGainControl'] as const) {
      const value = asked[name];
      if (typeof value === 'boolean') filters[name] = value || state.keptOn.includes(name);
    }
    const m: StubMedia = {
      deviceId: exact,
      filters,
      readyState: 'live',
      stopped: 0,
      clones: [],
      clone: () => {
        const c = {
          enabled: false,
          stopped: 0,
          stop: () => {
            c.stopped++;
          },
        };
        m.clones.push(c);
        return c;
      },
      stop: () => {
        m.stopped++;
        m.readyState = 'ended';
      },
      getSettings: () => ({ deviceId: m.deviceId, ...m.filters }),
    };
    state.medias.push(m);
    return m;
  };
  class LocalAudioTrack implements StubTrack {
    stopped = 0;
    userProvided: boolean;
    source = 'unknown';
    constraints: unknown;
    mediaStreamTrack: StubMedia;
    private readonly ends: (() => void)[] = [];
    constructor(mediaStreamTrack: StubMedia, constraints: unknown, userProvided = true) {
      this.mediaStreamTrack = mediaStreamTrack;
      this.constraints = constraints;
      this.userProvided = userProvided;
      state.tracks.push(this);
    }
    stop = () => {
      this.stopped++;
      this.mediaStreamTrack.stop();
    };
    // The captured track's own mute, before it is published.
    mute = () => {
      state.calls.push('premute');
      return state.premute === null ? Promise.resolve() : state.premute.promise;
    };
    // As the SDK's: the old capture is stopped before the new one is asked
    // for, and a failed one leaves none.
    restartTrack = async (options: unknown) => {
      state.calls.push('restart');
      state.restarts.push(options);
      this.mediaStreamTrack.stop();
      const held = state.restart;
      state.restart = null;
      if (held !== null) await held.promise;
      if (state.restartFails !== null) throw state.restartFails;
      this.mediaStreamTrack = media(options);
    };
    // The browser's own change of a live capture's filters, which Chromium
    // ignores (issue #659): never to be called.
    applyConstraints = () => {
      state.calls.push('applyConstraints');
      return Promise.resolve();
    };
    on = (event: string, fn: () => void) => {
      if (event === 'ended') this.ends.push(fn);
      return this;
    };
    ended = () => {
      this.mediaStreamTrack.readyState = 'ended';
      for (const fn of this.ends) fn();
    };
  }
  class Room {
    // Read with permission requests as asked: the stub never asks for any.
    static getLocalDevices(kind: string, requestPermissions: boolean) {
      state.listed.push([kind, requestPermissions]);
      return Promise.resolve(state.devices.filter((d) => d.kind === kind));
    }
    remoteParticipants = state.remote;
    localParticipant = {
      identity: 'p7',
      publishTrack: (track: { source?: string }, options?: { source?: string }) => {
        if (options?.source !== undefined && options.source !== 'microphone') {
          if (!state.shareGranted) {
            state.calls.push(`refused ${options.source}`);
            return Promise.reject(new PublishTrackError('failed to publish track, insufficient permissions', 403));
          }
          state.calls.push(`publish ${options.source}`);
          const source = options.source;
          const gate = state.shareGate;
          state.shareGate = null;
          const held = state.sharePublish;
          state.sharePublish = null;
          const finish = () => {
            state.pending.delete(track);
            const entry = { source, options };
            state.published.push(entry);
            state.owners.set(entry, track);
            return { source: track.source };
          };
          // Had as pending, as by the SDK, only once past the gate; one not
          // held completes at once.
          const pend = () => {
            if (held === null) return Promise.resolve(finish());
            const done = held.promise.then(finish);
            state.pending.set(track, done);
            return done;
          };
          return gate === null ? pend() : gate.promise.then(pend);
        }
        state.calls.push('publish');
        if (state.publishFails) return Promise.reject(new Error('publish refused'));
        return state.publish === null ? Promise.resolve(pub()) : state.publish.promise;
      },
      createScreenTracks: (options: unknown) => {
        state.calls.push('pick');
        state.screenOptions = options;
        const tracks = [screenTrack('screen_share'), screenTrack('screen_share_audio')];
        state.screenTracks.push(...tracks);
        return Promise.resolve(tracks);
      },
      unpublishTrack: (track: StubScreenTrack, stop: boolean) => {
        state.calls.push(`unpublish ${track.source}`);
        if (state.unpublishHangs) return new Promise(() => undefined);
        // As the SDK: a publish it has as pending is waited for, a track
        // with no publication is left as it is, and a publication taken
        // down is told to the room at once.
        const take = (): undefined => {
          const i = state.published.findIndex((e) => state.owners.get(e) === track);
          if (i < 0) return undefined;
          state.published.splice(i, 1);
          if (stop) track.stop();
          state.handlers.get('ltu')?.({ source: track.source, track });
          return undefined;
        };
        const pending = state.pending.get(track);
        if (pending !== undefined) return pending.then(take, take);
        take();
        return Promise.resolve(undefined);
      },
    };
    constructor(options: unknown) {
      state.roomOptions = options;
      state.room = this;
    }
    on(event: string, fn: (...args: unknown[]) => void) {
      state.handlers.set(event, fn);
      return this;
    }
    connect(_url: string, token: string, options: unknown) {
      state.connectOptions = options;
      state.calls.push('connect');
      if (state.connectLog !== null) state.logger.methodFactory('warn')(`${state.connectLog} ${token}`, { token });
      return Promise.resolve();
    }
    disconnect() {
      state.calls.push('disconnect');
      return Promise.resolve();
    }
    // The SDK's own switch, which leaves a muted track on its old device
    // until the next unmute: never to be called for the microphone.
    // For the speakers (issue #660) it is the one switch: it moves every
    // voice, and tells the room which output is now in use, as the SDK does.
    switchActiveDevice(kind: string, id: string) {
      state.calls.push(`switchActiveDevice ${kind}`);
      if (kind === 'audiooutput') {
        state.outputs.push(id);
        const refusal = state.outputFails;
        state.outputFails = null;
        if (refusal !== null) return Promise.reject(refusal);
        state.handlers.get('adc')?.(kind, id);
      }
      return Promise.resolve(true);
    }
  }
  return {
    Room,
    RoomEvent: {
      Reconnecting: 'rc',
      Reconnected: 'rd',
      Disconnected: 'dc',
      ActiveSpeakersChanged: 'as',
      TrackPublished: 'tp',
      TrackSubscribed: 'ts',
      TrackUnsubscribed: 'tu',
      LocalTrackPublished: 'ltp',
      LocalTrackUnpublished: 'ltu',
      TrackUnmuted: 'tum',
      ParticipantPermissionsChanged: 'ppc',
      MediaDevicesChanged: 'mdc',
      ActiveDeviceChanged: 'adc',
    },
    TrackEvent: { Ended: 'ended' },
    PublishTrackError,
    DisconnectReason: { ROOM_DELETED: 5, PARTICIPANT_REMOVED: 4, DUPLICATE_IDENTITY: 2, SERVER_SHUTDOWN: 3 },
    LogLevel: { warn: 3 },
    LoggerNames: { Default: 'livekit' },
    Track: {
      Kind: { Audio: 'audio', Video: 'video' },
      Source: { Microphone: 'microphone', Camera: 'camera', ScreenShare: 'screen_share', ScreenShareAudio: 'screen_share_audio' },
    },
    VideoPreset: class {
      width: number;
      height: number;
      encoding: { maxBitrate: number; maxFramerate?: number };
      constructor(width: number, height: number, maxBitrate: number, maxFramerate?: number) {
        this.width = width;
        this.height = height;
        this.encoding = maxFramerate === undefined ? { maxBitrate } : { maxBitrate, maxFramerate };
      }
    },
    AudioPresets: { musicHighQuality: { maxBitrate: 96000 }, musicHighQualityStereo: { maxBitrate: 128000 } },
    LocalAudioTrack,
    // The SDK's capture: the browser's track and the constraints it was
    // captured with, which the adapter wraps as its own.
    createLocalAudioTrack: (options: unknown) => {
      state.captureOptions = options;
      state.calls.push('capture');
      const captured = () => ({ mediaStreamTrack: media(options), constraints: options });
      if (state.capture === null) return Promise.resolve(captured());
      const pending = state.capture;
      return pending.promise.then(captured);
    },
    getLogger: () => state.logger,
    supportsAudioOutputSelection: () => state.outputSelection,
    // The SDK's own setLogLevel persists the level; the adapter must not call it.
    setLogLevel: () => {
      state.calls.push('setLogLevel');
    },
  };
});

import { captureOptions, grantWait, newLiveKitRoom, principalOf, sdkLog, shareEncodings, type ShareLayer } from './sdk';

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
  state.premute = null;
  state.publish = null;
  state.publishFails = false;
  state.tracks.length = 0;
  state.room = null;
  state.connectLog = null;
  state.audioContexts = 0;
  state.roomOptions = undefined;
  state.connectOptions = undefined;
  state.screenOptions = undefined;
  state.screenTracks.length = 0;
  state.published.length = 0;
  state.remote.clear();
  state.unpublishHangs = false;
  state.shareGranted = true;
  state.sharePublish = null;
  state.shareGate = null;
  state.pending.clear();
  state.owners.clear();
  state.captured = { width: 1920, height: 1080, frameRate: 30 };
  state.medias.length = 0;
  state.restarts.length = 0;
  state.restart = null;
  state.restartFails = null;
  state.devices = [];
  state.listed.length = 0;
  state.contexts.length = 0;
  state.keptOn = [];
  state.outputSelection = true;
  state.outputs.length = 0;
  state.outputFails = null;
  vi.stubGlobal(
    'AudioContext',
    class {
      private readonly made = { closed: 0 };
      constructor() {
        state.audioContexts++;
        state.contexts.push(this.made);
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
        this.made.closed++;
        return Promise.resolve();
      }
    },
  );
  vi.stubGlobal('MediaStream', function MediaStream() {
    return undefined;
  });
});

const events = {
  onReconnecting: () => undefined,
  onReconnected: () => undefined,
  onDisconnected: () => undefined,
  onSpeakers: () => undefined,
  onScreenEnded: () => undefined,
  onShareTracks: () => undefined,
  onMicrophoneLost: () => undefined,
  onDevicesChanged: () => undefined,
  onOutputMoved: () => undefined,
};

describe('the SDK adapter', () => {
  // Issue #501: muted before publishing, so that LiveKit lists the
  // microphone muted from the first; and again after, as the second layer.
  it('captures with echo cancellation, noise suppression and auto gain, and mutes before publishing and straight after', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    expect(state.captureOptions).toEqual({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
  });

  it('a Leave while the captured track is being muted stops it, and publishes nothing', async () => {
    state.premute = deferred();
    const room = newLiveKitRoom();
    const outcome = room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events).then(
      () => 'resolved',
      () => 'rejected',
    );
    await settle();
    expect(state.calls).toEqual(['connect', 'capture', 'premute']);
    await room.close();
    state.premute.resolve(undefined);
    expect(await outcome).toBe('rejected');
    expect(state.tracks[0]?.stopped).toBeGreaterThan(0);
    expect(state.calls).not.toContain('publish');
    expect(state.audioContexts).toBe(0);
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
    state.capture.resolve(undefined);
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
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish']);
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

// Issue #444: watch only, the share and the watch view, through the adapter.
describe('watch only', () => {
  it('captures nothing on connecting, and publishes the microphone muted when asked', async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(state.calls).toEqual(['connect']);
    await room.publishMicrophone();
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
    // Once is enough.
    await room.publishMicrophone();
    expect(state.calls.filter((c) => c === 'capture')).toHaveLength(1);
  });
});

// Issue #658: the microphone chosen. Captured exactly or not at all,
// published as the page's own track, moved by restarting that track, and lost
// (stopped, and the client told once) when its capture ends or its device is
// no longer listed.
describe('the microphone chosen', () => {
  const processing = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };
  const inputs = (...ids: string[]): StubDevice[] => ids.map((id) => ({ kind: 'audioinput', deviceId: id, label: `Microphone ${id}` }));

  async function connected(choice: string | null, told: { lost: number; changed: number } = { lost: 0, changed: 0 }, filters = true) {
    const room = newLiveKitRoom();
    room.setMicrophone(choice);
    room.setProcessing(filters);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, {
      ...events,
      onMicrophoneLost: () => {
        told.lost++;
      },
      onDevicesChanged: () => {
        told.changed++;
      },
    });
    const track = state.tracks[0];
    if (track === undefined) throw new Error('no microphone captured');
    return { room, track, told };
  }

  it('captures a chosen device exactly, as an object, with voice processing on; and the system default as before', async () => {
    await connected('mic-1');
    expect(state.captureOptions).toEqual({ deviceId: { exact: 'mic-1' }, ...processing });
    state.tracks.length = 0;
    await connected(null);
    expect(state.captureOptions).toEqual(processing);
  });

  it('publishes the capture as a track the page provides, on the device it was captured from', async () => {
    const { track } = await connected('mic-1');
    expect(track.userProvided).toBe(true);
    expect(track.source).toBe('microphone');
    expect(track.mediaStreamTrack).toBe(state.medias[0]);
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
  });

  it('lists the microphones with permission requests off, and none when the browser does not name them', async () => {
    const { room } = await connected(null);
    state.devices = [...inputs('default', 'mic-1'), { kind: 'audiooutput', deviceId: 'out', label: 'Speakers' }];
    expect(await room.microphones()).toEqual([
      { id: 'default', label: 'Microphone default' },
      { id: 'mic-1', label: 'Microphone mic-1' },
    ]);
    expect(state.listed).toEqual([['audioinput', false]]);
    state.devices = [{ kind: 'audioinput', deviceId: 'mic-1', label: '' }];
    expect(await room.microphones()).toBeNull();
    // Reading the list captured nothing.
    expect(state.calls.filter((c) => c === 'capture')).toHaveLength(1);
  });

  it('reads the device in use from the capture itself', async () => {
    const { room, track } = await connected('mic-1');
    expect(room.microphoneInUse()).toBe('mic-1');
    track.mediaStreamTrack.deviceId = 'other';
    expect(room.microphoneInUse()).toBe('other');
    // Nothing captured, nothing in use.
    track.stop();
    expect(room.microphoneInUse()).toBeNull();
  });

  it("switches by restarting the published track with every field, never by the SDK's switchActiveDevice, and never republishes", async () => {
    const { room, track } = await connected(null);
    state.calls.length = 0;
    await room.switchMicrophone('mic-2');
    expect(state.restarts).toEqual([{ deviceId: { exact: 'mic-2' }, ...processing }]);
    expect(room.microphoneInUse()).toBe('mic-2');
    // Back to the system default: no device named.
    await room.switchMicrophone(null);
    expect(state.restarts.at(-1)).toEqual(processing);
    expect(room.microphoneInUse()).toBe('default');
    expect(state.calls).toEqual(['restart', 'restart']);
    expect(state.tracks).toEqual([track]);
  });

  it('after a switch the tap hears the new device: the old clone is stopped and its audio context closed', async () => {
    const { room } = await connected('mic-1');
    const old = state.medias[0];
    expect(old?.clones).toHaveLength(1);
    expect(state.contexts).toHaveLength(1);
    await room.switchMicrophone('mic-2');
    expect(old?.clones[0]?.stopped).toBe(1);
    expect(state.contexts[0]?.closed).toBe(1);
    // One tap again, of the new capture.
    expect(state.contexts).toHaveLength(2);
    expect(state.contexts[1]?.closed).toBe(0);
    expect(state.medias[1]?.clones).toHaveLength(1);
    expect(state.medias[1]?.clones[0]?.enabled).toBe(true);
  });

  it('a switch that fails leaves nothing captured and says so', async () => {
    const { room, track } = await connected('mic-1');
    state.restartFails = new DOMException('gone', 'OverconstrainedError');
    await expect(room.switchMicrophone('mic-2')).rejects.toThrow('gone');
    expect(track.stopped).toBeGreaterThan(0);
    expect(state.medias.every((m) => m.readyState === 'ended')).toBe(true);
    expect(room.microphoneInUse()).toBeNull();
    expect(state.contexts.every((c) => c.closed > 0)).toBe(true);
  });

  it('a close during a switch leaves no capture open', async () => {
    const { room } = await connected('mic-1');
    const held = deferred<undefined>();
    state.restart = held;
    const switching = room.switchMicrophone('mic-2').then(
      () => 'switched',
      () => 'rejected',
    );
    await settle();
    await room.close();
    held.resolve(undefined);
    expect(await switching).toBe('rejected');
    expect(state.medias.map((m) => m.readyState)).toEqual(['ended', 'ended']);
    // No tap was made of the new capture.
    expect(state.contexts).toHaveLength(1);
    expect(state.contexts[0]?.closed).toBe(1);
  });

  it('switching with nothing published captures and publishes, muted, as a join does', async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    await room.switchMicrophone('mic-2');
    expect(state.captureOptions).toEqual({ deviceId: { exact: 'mic-2' }, ...processing });
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
  });

  it('a capture that ends is lost once: stopped, untapped, and the client told', async () => {
    const { room, track, told } = await connected('mic-1');
    track.ended();
    track.ended();
    expect(told.lost).toBe(1);
    expect(track.stopped).toBe(1);
    expect(state.medias[0]?.clones[0]?.stopped).toBe(1);
    expect(room.microphoneInUse()).toBeNull();
    // Nothing captured since.
    expect(state.calls.filter((c) => c === 'capture' || c === 'restart')).toEqual(['capture']);
  });

  it('a device change whose list lacks the chosen device is lost once; one that lists it, or a system default, is not', async () => {
    state.devices = inputs('default', 'mic-1');
    const listed = await connected('mic-1');
    state.handlers.get('mdc')?.();
    await settle();
    expect(listed.told).toEqual({ lost: 0, changed: 1 });
    expect(listed.track.stopped).toBe(0);
    state.devices = inputs('default');
    state.handlers.get('mdc')?.();
    await settle();
    state.handlers.get('mdc')?.();
    await settle();
    expect(listed.told).toEqual({ lost: 1, changed: 3 });
    expect(listed.track.stopped).toBe(1);
    // Every listing was made with permission requests off.
    expect(state.listed.every(([, ask]) => ask === false)).toBe(true);

    // The system default follows the computer: nothing chosen is missing.
    state.tracks.length = 0;
    state.devices = inputs('default', 'mic-1');
    const following = await connected(null);
    state.devices = [];
    state.handlers.get('mdc')?.();
    await settle();
    expect(following.told).toEqual({ lost: 0, changed: 1 });
    expect(following.track.stopped).toBe(0);
    expect(state.calls).not.toContain('switchActiveDevice audioinput');
  });

  it('chosen again after it was lost, the track is restarted on the device', async () => {
    const { room, track } = await connected('mic-1');
    track.ended();
    await room.switchMicrophone('mic-1');
    expect(state.restarts).toEqual([{ deviceId: { exact: 'mic-1' }, ...processing }]);
    expect(room.microphoneInUse()).toBe('mic-1');
  });

  // Issue #659: the browser's voice processing, all on or all off. Voice
  // isolation, which the SDK asks on at a first capture unless told, is
  // asked off with the rest.
  const unprocessed = { echoCancellation: false, noiseSuppression: false, autoGainControl: false, voiceIsolation: false };

  it('captures with the voice processing given: all three on, or all three off', async () => {
    await connected('mic-1', undefined, false);
    expect(state.captureOptions).toEqual({ deviceId: { exact: 'mic-1' }, ...unprocessed });
    state.tracks.length = 0;
    await connected(null, undefined, false);
    expect(state.captureOptions).toEqual(unprocessed);
    state.tracks.length = 0;
    await connected('mic-1', undefined, true);
    expect(state.captureOptions).toEqual({ deviceId: { exact: 'mic-1' }, ...processing });
  });

  it('a change restarts the track with the device and all three filters, and never changes the live capture', async () => {
    const { room, track } = await connected('mic-1');
    state.calls.length = 0;
    room.setProcessing(false);
    await room.switchMicrophone('mic-1');
    expect(state.restarts).toEqual([{ deviceId: { exact: 'mic-1' }, ...unprocessed }]);
    room.setProcessing(true);
    await room.switchMicrophone('mic-1');
    expect(state.restarts.at(-1)).toEqual({ deviceId: { exact: 'mic-1' }, ...processing });
    // System default: the filters are given with no device named.
    room.setProcessing(false);
    await room.switchMicrophone(null);
    expect(state.restarts.at(-1)).toEqual(unprocessed);
    expect(state.calls).toEqual(['restart', 'restart', 'restart']);
    expect(state.calls).not.toContain('applyConstraints');
    // The same track, published once: no republish.
    expect(state.tracks).toEqual([track]);
  });

  it('reports the filters as the capture says them, not as they were asked', async () => {
    const { room, track } = await connected('mic-1');
    expect(room.microphoneProcessing()).toEqual({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    state.keptOn = ['noiseSuppression'];
    room.setProcessing(false);
    await room.switchMicrophone('mic-1');
    expect(room.microphoneProcessing()).toEqual({ echoCancellation: false, noiseSuppression: true, autoGainControl: false });
    // A browser that does not say is not taken to have done either.
    track.mediaStreamTrack.filters = {};
    expect(room.microphoneProcessing()).toEqual({ echoCancellation: null, noiseSuppression: null, autoGainControl: null });
    // Nothing captured, nothing said.
    track.stop();
    expect(room.microphoneProcessing()).toBeNull();
  });

  it('after a change the tap hears the new capture: the old clone is stopped and a new one tapped', async () => {
    const { room } = await connected('mic-1');
    const old = state.medias[0];
    room.setProcessing(false);
    await room.switchMicrophone('mic-1');
    expect(old?.clones[0]?.stopped).toBe(1);
    expect(state.contexts[0]?.closed).toBe(1);
    expect(state.contexts).toHaveLength(2);
    expect(state.medias[1]?.filters).toEqual({ echoCancellation: false, noiseSuppression: false, autoGainControl: false });
    expect(state.medias[1]?.clones).toHaveLength(1);
    expect(state.medias[1]?.clones[0]?.enabled).toBe(true);
  });
});

describe('the watch view', () => {
  const remotePub = (source: string): StubRemotePub => {
    const p: StubRemotePub = {
      source,
      isSubscribed: false,
      setSubscribed: (on) => {
        p.isSubscribed = on;
        state.calls.push(`subscribe ${source} ${String(on)}`);
      },
    };
    return p;
  };

  // The SDK stops no track it unpublishes, so that a share's capture
  // survives its room (issue #516); the adapter stops every track itself.
  it('joins with adaptive stream and dynacast on, and subscribes to nothing automatically', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(state.roomOptions).toEqual({ adaptiveStream: true, dynacast: true, stopLocalTrackOnUnpublish: false });
    expect(state.connectOptions).toEqual({ autoSubscribe: false });
  });

  it('subscribes every microphone, and a share only while it is watched', async () => {
    // Someone already in the room, with a microphone and a share.
    const mic = remotePub('microphone');
    const screen = remotePub('screen_share');
    const sound = remotePub('screen_share_audio');
    state.remote.set('p8', { trackPublications: new Map([['a', mic], ['b', screen], ['c', sound]]) });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect([mic.isSubscribed, screen.isSubscribed, sound.isSubscribed]).toEqual([true, false, false]);
    room.setWatching(true);
    expect([mic.isSubscribed, screen.isSubscribed, sound.isSubscribed]).toEqual([true, true, true]);
    // A track published later follows the same choice; a camera never.
    const later = remotePub('microphone');
    const camera = remotePub('camera');
    state.handlers.get('tp')?.(later);
    state.handlers.get('tp')?.(camera);
    expect([later.isSubscribed, camera.isSubscribed]).toEqual([true, false]);
    room.setWatching(false);
    expect([mic.isSubscribed, screen.isSubscribed, sound.isSubscribed]).toEqual([true, false, false]);
  });

  it("plays a share into the view's elements, not into the page, and says what arrived", async () => {
    const told: unknown[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onShareTracks: (t) => told.push(t) });
    const attached: string[] = [];
    const remote = (source: string, kind: string) => ({
      source,
      kind,
      sid: source,
      attach: (el?: HTMLMediaElement) => {
        attached.push(`${source} ${el?.tagName ?? 'new'}`);
        return el ?? document.createElement('audio');
      },
      detach: (el?: HTMLMediaElement) => {
        attached.push(`detach ${source} ${el?.tagName ?? 'all'}`);
        return [];
      },
    });
    const video = remote('screen_share', 'video');
    const audio = remote('screen_share_audio', 'audio');
    // Arrives before the view is open: kept, and attached once it opens.
    state.handlers.get('ts')?.(video);
    const sink = { video: document.createElement('video'), audio: document.createElement('audio') };
    room.attachShare(sink);
    state.handlers.get('ts')?.(audio);
    expect(attached).toEqual(['screen_share VIDEO', 'screen_share_audio AUDIO']);
    expect(document.body.querySelectorAll('audio')).toHaveLength(0);
    expect(told).toEqual([{ video: true, audio: false }, { video: true, audio: true }]);
    state.handlers.get('tu')?.(video);
    expect(told.at(-1)).toEqual({ video: false, audio: true });
    room.attachShare(null);
    expect(attached.at(-1)).toBe('detach screen_share_audio AUDIO');
  });
});

describe('sharing a screen', () => {
  async function connected(onScreenEnded: (cause: string) => void = () => undefined) {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onScreenEnded });
    state.calls.length = 0;
    return room;
  }

  it("asks the picker for up to 3840x2160 at up to 60 fps with unprocessed sound, tab and system sound offered, this page's own sound and this tab left out", async () => {
    const room = await connected();
    expect(await room.pickScreen()).toEqual({ audio: true });
    expect(state.screenOptions).toEqual({
      video: true,
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
      resolution: { width: 3840, height: 2160, frameRate: 60 },
      contentHint: 'motion',
      systemAudio: 'include',
      selfBrowserSurface: 'exclude',
      surfaceSwitching: 'include',
      suppressLocalAudioPlayback: false,
    });
    // Nothing is published by picking.
    expect(state.calls).toEqual(['pick']);
  });

  // Issue #554: there is no choice of what a share is for; every share keeps
  // motion, and the rest of the picker's options are as they were.
  it('captures every share as motion', () => {
    expect(captureOptions()).toEqual({
      video: true,
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
      resolution: { width: 3840, height: 2160, frameRate: 60 },
      contentHint: 'motion',
      systemAudio: 'include',
      selfBrowserSurface: 'exclude',
      surfaceSwitching: 'include',
      suppressLocalAudioPlayback: false,
    });
  });

  // Issue #565: the picture's encodings follow its track's settings, read
  // after the picker. A 1080p30 capture has a 720p layer and no 1080p one.
  it('publishes a 1080p picture as captured with a 720p layer at 15 fps, and the sound as stereo music', async () => {
    const room = await connected();
    await room.pickScreen();
    await room.publishScreen();
    expect(state.published).toEqual([
      {
        source: 'screen_share',
        options: {
          source: 'screen_share',
          simulcast: true,
          screenShareEncoding: { maxBitrate: 6_000_000, maxFramerate: 30 },
          screenShareSimulcastLayers: [{ width: 1280, height: 720, encoding: { maxBitrate: 1_500_000, maxFramerate: 15 } }],
        },
      },
      {
        source: 'screen_share_audio',
        options: { source: 'screen_share_audio', audioPreset: { maxBitrate: 128000 }, dtx: false, red: false, forceStereo: true },
      },
    ]);
  });

  it('publishes a 4K60 picture at 60 fps and 18 Mb/s with layers of 720p at 15 fps and 1080p at 30 fps', async () => {
    state.captured = { width: 3840, height: 2160, frameRate: 60 };
    const room = await connected();
    await room.pickScreen();
    await room.publishScreen();
    expect(state.published[0]?.options).toEqual({
      source: 'screen_share',
      simulcast: true,
      screenShareEncoding: { maxBitrate: 18_000_000, maxFramerate: 60 },
      screenShareSimulcastLayers: [
        { width: 1280, height: 720, encoding: { maxBitrate: 1_500_000, maxFramerate: 15 } },
        { width: 1920, height: 1080, encoding: { maxBitrate: 4_000_000, maxFramerate: 30 } },
      ],
    });
  });

  it("tells the browser's stop control from the server's withdrawal, and its own stop from either", async () => {
    vi.useFakeTimers();
    try {
      const ends: string[] = [];
      const room = await connected((cause) => ends.push(cause));
      await room.pickScreen();
      await room.publishScreen();
      // Its own stop: unpublished and stopped, and not reported.
      await room.stopScreen();
      vi.runAllTimers();
      expect(ends).toEqual([]);
      expect(state.screenTracks.map((t) => t.stopped > 0)).toEqual([true, true]);

      // The browser ends the capture; the SDK unpublishes it from inside the
      // same event, possibly first.
      await room.pickScreen();
      await room.publishScreen();
      const video = state.screenTracks[2];
      state.handlers.get('ltu')?.({ source: 'screen_share', track: video });
      video?.mediaStreamTrack.ended();
      vi.runAllTimers();
      expect(ends).toEqual(['browser']);

      // The server withdraws the share: the SDK unpublishes it, the capture
      // still live.
      await room.stopScreen();
      await room.pickScreen();
      await room.publishScreen();
      state.handlers.get('ltu')?.({ source: 'screen_share', track: state.screenTracks[4] });
      vi.runAllTimers();
      expect(ends).toEqual(['browser', 'server']);
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops the capture before unpublishing, so a hung connection leaves no screen captured', async () => {
    const room = await connected();
    await room.pickScreen();
    await room.publishScreen();
    state.unpublishHangs = true;
    let done = false;
    void room.stopScreen().then(() => {
      done = true;
    });
    await settle();
    // The unpublish has not answered, and the capture is stopped all the same.
    expect(done).toBe(false);
    expect(state.calls.filter((c) => c.startsWith('unpublish'))).toEqual(['unpublish screen_share']);
    expect(state.screenTracks.map((t) => t.stopped > 0)).toEqual([true, true]);
  });

  it('a Leave stops a share picked but not published', async () => {
    const room = await connected();
    await room.pickScreen();
    await room.close();
    expect(state.screenTracks.map((t) => t.stopped > 0)).toEqual([true, true]);
  });

  // Issue #516: a renewed room. The capture is taken off the old connection
  // still running, and published on the next with the same options.
  it('keeps a capture across connections: unpublished without being stopped or reported, and published on the next as picked', async () => {
    vi.useFakeTimers();
    try {
      const ends: string[] = [];
      const old = await connected((cause) => ends.push(cause));
      await old.pickScreen();
      await old.publishScreen();
      const kept = old.keepScreen();
      expect(kept?.audio).toBe(true);
      expect(kept?.live()).toBe(true);
      expect(state.calls.filter((c) => c.startsWith('unpublish'))).toEqual(['unpublish screen_share', 'unpublish screen_share_audio']);
      await old.close();
      vi.runAllTimers();
      expect(ends).toEqual([]);
      expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);
      // Kept: stopScreen on the old connection has nothing to stop.
      await old.stopScreen();
      expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);

      const next = await connected((cause) => ends.push(`next ${cause}`));
      if (kept === null) throw new Error('nothing kept');
      next.adoptScreen(kept);
      state.published.length = 0;
      await next.publishScreen();
      expect(state.published.map((p) => p.options)).toEqual([
        {
          source: 'screen_share',
          simulcast: true,
          screenShareEncoding: { maxBitrate: 6_000_000, maxFramerate: 30 },
          screenShareSimulcastLayers: [{ width: 1280, height: 720, encoding: { maxBitrate: 1_500_000, maxFramerate: 15 } }],
        },
        { source: 'screen_share_audio', audioPreset: { maxBitrate: 128000 }, dtx: false, red: false, forceStereo: true },
      ]);
      // The browser's stop control now is the new connection's to report.
      state.screenTracks[0]?.mediaStreamTrack.ended();
      vi.runAllTimers();
      expect(ends).toEqual(['next browser']);
    } finally {
      vi.useRealTimers();
    }
  });

  // Issue #565: the size is read at each publish, not kept from the picker,
  // so a capture whose source changed size (the browser lets the sharer
  // switch what is shared) is published on the next connection as it is now.
  it('publishes a kept capture at the size it has when the next connection publishes it', async () => {
    const old = await connected();
    await old.pickScreen();
    await old.publishScreen();
    const kept = old.keepScreen();
    await old.close();
    if (kept === null) throw new Error('nothing kept');
    const video = state.screenTracks[0];
    if (video === undefined) throw new Error('no picture captured');
    video.settings = { width: 2560, height: 1440, frameRate: 60 };
    const next = await connected();
    next.adoptScreen(kept);
    state.published.length = 0;
    await next.publishScreen();
    expect(state.published[0]?.options).toEqual({
      source: 'screen_share',
      simulcast: true,
      screenShareEncoding: { maxBitrate: 10_000_000, maxFramerate: 60 },
      screenShareSimulcastLayers: [
        { width: 1280, height: 720, encoding: { maxBitrate: 1_500_000, maxFramerate: 15 } },
        { width: 1920, height: 1080, encoding: { maxBitrate: 4_000_000, maxFramerate: 30 } },
      ],
    });
  });

  it('a kept capture says when the browser ended it, and stops when asked', async () => {
    const room = await connected();
    await room.pickScreen();
    await room.publishScreen();
    const kept = room.keepScreen();
    await room.close();
    if (kept === null) throw new Error('nothing kept');
    let told = 0;
    kept.onEnded = () => {
      told++;
    };
    state.screenTracks[0]?.mediaStreamTrack.ended();
    expect(told).toBe(1);
    expect(kept.live()).toBe(false);
    kept.stop();
    expect(state.screenTracks.map((t) => t.stopped)).toEqual([1, 1]);
  });

  it('keeps nothing when nothing was picked, and adopts only a capture it kept', async () => {
    const room = await connected();
    expect(room.keepScreen()).toBeNull();
    expect(() => {
      room.adoptScreen({ audio: false, live: () => true, onEnded: null, stop: () => undefined });
    }).toThrow();
  });

  // Issue #525. conchd answers a claim once LiveKit has granted the share's
  // sources, and LiveKit tells the connection on a socket of its own, which
  // can be read after conchd's answer. A resume publishes straight after its
  // claim, with no picker between: on a slow machine the SDK, still holding
  // the join grant, refused the publish and the share ended as failed.
  async function grantedLate() {
    state.shareGranted = true;
    const old = await connected();
    await old.pickScreen();
    await old.publishScreen();
    const kept = old.keepScreen();
    await old.close();
    if (kept === null) throw new Error('nothing kept');
    const next = await connected();
    next.adoptScreen(kept);
    state.shareGranted = false;
    state.published.length = 0;
    const outcome = { value: 'pending' };
    void next.publishScreen().then(
      () => {
        outcome.value = 'published';
      },
      (err: unknown) => {
        outcome.value = err instanceof Error ? err.message : String(err);
      },
    );
    await settle();
    return { next, outcome };
  }
  const grant = () => {
    state.shareGranted = true;
    state.handlers.get('ppc')?.(undefined, state.room?.localParticipant);
  };

  it("publishes a resumed share once LiveKit has told this connection of the claim's grant, not before", async () => {
    vi.useFakeTimers();
    try {
      const { outcome } = await grantedLate();
      expect(outcome.value).toBe('pending');
      expect(state.published).toEqual([]);
      // Another participant's permission changing is not the grant.
      state.handlers.get('ppc')?.(undefined, { identity: 'p8' });
      await settle();
      expect(outcome.value).toBe('pending');
      vi.advanceTimersByTime(grantWait - 1);
      grant();
      await settle();
      expect(outcome.value).toBe('published');
      expect(state.published.map((p) => p.source)).toEqual(['screen_share', 'screen_share_audio']);
      expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);
    } finally {
      vi.useRealTimers();
    }
  });

  it('a grant that never comes fails the publish in time, and a Leave ends the wait at once', async () => {
    vi.useFakeTimers();
    try {
      const late = await grantedLate();
      vi.advanceTimersByTime(grantWait - 1);
      await settle();
      expect(late.outcome.value).toBe('pending');
      vi.advanceTimersByTime(1);
      await settle();
      expect(late.outcome.value).toBe('failed to publish track, insufficient permissions');
      expect(state.published).toEqual([]);

      const left = await grantedLate();
      await left.next.close();
      await settle();
      expect(left.outcome.value).toBe('voice connection closed');
      expect(state.published).toEqual([]);

      // Stopped while it waited: the wait ends, and a grant after it
      // publishes nothing.
      const stopped = await grantedLate();
      await stopped.next.stopScreen();
      await settle();
      expect(stopped.outcome.value).toBe('failed to publish track, insufficient permissions');
      grant();
      await settle();
      expect(state.published).toEqual([]);
    } finally {
      vi.useRealTimers();
    }
  });

  // Issue #535: the room renewed again while a share waited for its grant.
  // The capture is kept for the next connection: the wait ends, nothing is
  // stopped, and nothing is published on this one, even once the grant comes.
  it('a capture kept while it waited for the grant is not stopped, and a grant after it publishes nothing', async () => {
    vi.useFakeTimers();
    try {
      const { next, outcome } = await grantedLate();
      const kept = next.keepScreen();
      await settle();
      expect(outcome.value).toBe('failed to publish track, insufficient permissions');
      grant();
      await settle();
      expect(state.published).toEqual([]);
      expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);
      expect(kept?.live()).toBe(true);
      await next.close();
      expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);

      // Published on the connection after.
      const third = await connected();
      if (kept === null) throw new Error('nothing kept');
      third.adoptScreen(kept);
      await third.publishScreen();
      expect(state.published.map((p) => p.source)).toEqual(['screen_share', 'screen_share_audio']);
    } finally {
      vi.useRealTimers();
    }
  });

  // Issue #535: a capture kept while its picture's publish was under way. It
  // is blanked at once, its publish is taken down when it completes, its
  // sound is not published, nothing is reported as the server's end, and it
  // is sent as captured again once the next connection adopts it.
  async function keptMidPublish(hold: 'pending' | 'not yet pending') {
    const ends: string[] = [];
    const room = await connected((cause) => ends.push(cause));
    await room.pickScreen();
    const held = deferred<undefined>();
    if (hold === 'pending') state.sharePublish = held;
    else state.shareGate = held;
    let outcome = 'pending';
    void room.publishScreen().then(
      () => {
        outcome = 'published';
      },
      (err: unknown) => {
        outcome = err instanceof Error ? err.message : String(err);
      },
    );
    await settle();
    expect(state.calls).toEqual(['pick', 'publish screen_share']);
    const kept = room.keepScreen();
    expect(state.screenTracks.map((t) => t.mediaStreamTrack.enabled), 'blanked at once').toEqual([false, false]);
    held.resolve(undefined);
    await settle();
    vi.runAllTimers();
    expect(outcome).toBe('voice share taken off this connection');
    expect(state.published, 'nothing left published on the old connection').toEqual([]);
    expect(state.calls.filter((c) => c.startsWith('publish'))).toEqual(['publish screen_share']);
    // Asked of the SDK twice: by the keep, and by the publish once it was done.
    expect(state.calls.filter((c) => c === 'unpublish screen_share').length).toBe(2);
    expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);
    expect(ends).toEqual([]);
    expect(kept?.live()).toBe(true);
    await room.close();
    expect(state.screenTracks.map((t) => t.stopped)).toEqual([0, 0]);

    const next = await connected();
    if (kept === null) throw new Error('nothing kept');
    next.adoptScreen(kept);
    expect(state.screenTracks.map((t) => t.mediaStreamTrack.enabled), 'sent as captured again').toEqual([true, true]);
    await next.publishScreen();
    expect(state.published.map((p) => p.source)).toEqual(['screen_share', 'screen_share_audio']);
  }

  it('a capture kept while the SDK had its publish pending is blanked, taken down when it completes, and sent again on the next connection', async () => {
    vi.useFakeTimers();
    try {
      await keptMidPublish('pending');
    } finally {
      vi.useRealTimers();
    }
  });

  // The unpublish that kept it finds no publication then, and does nothing.
  it('a capture kept before the SDK had its publish as pending is taken down by the publish itself', async () => {
    vi.useFakeTimers();
    try {
      await keptMidPublish('not yet pending');
    } finally {
      vi.useRealTimers();
    }
  });

  it('a first share is sent as captured, and a stop is not a blanking', async () => {
    const room = await connected();
    await room.pickScreen();
    await room.publishScreen();
    expect(state.screenTracks.map((t) => t.mediaStreamTrack.enabled)).toEqual([true, true]);
    await room.stopScreen();
    expect(state.screenTracks.map((t) => t.mediaStreamTrack.enabled)).toEqual([true, true]);
  });
});

// Issue #565: a share's encodings follow what was captured. The top layer is
// the capture at its own frame rate up to 60 fps, its bitrate by the longer
// side (6, 10 and 18 Mb/s up to 1920, up to 2560 and beyond); below it 720p
// at 15 fps and 1080p at 30 fps, smallest first, each left out unless it is
// smaller than the capture.
describe("a share's encodings", () => {
  const p720: ShareLayer = { width: 1280, height: 720, maxBitrate: 1_500_000, maxFramerate: 15 };
  const p1080: ShareLayer = { width: 1920, height: 1080, maxBitrate: 4_000_000, maxFramerate: 30 };

  it.each<{ readonly width: number; readonly height: number; readonly fps: number; readonly bitrate: number; readonly lower: readonly ShareLayer[] }>([
    { width: 1280, height: 720, fps: 30, bitrate: 6_000_000, lower: [] },
    { width: 1280, height: 720, fps: 60, bitrate: 6_000_000, lower: [] },
    { width: 1920, height: 1080, fps: 30, bitrate: 6_000_000, lower: [p720] },
    { width: 1920, height: 1080, fps: 60, bitrate: 6_000_000, lower: [p720] },
    { width: 2560, height: 1440, fps: 30, bitrate: 10_000_000, lower: [p720, p1080] },
    { width: 2560, height: 1440, fps: 60, bitrate: 10_000_000, lower: [p720, p1080] },
    { width: 3840, height: 2160, fps: 30, bitrate: 18_000_000, lower: [p720, p1080] },
    { width: 3840, height: 2160, fps: 60, bitrate: 18_000_000, lower: [p720, p1080] },
  ])('a $width x $height capture at $fps fps', ({ width, height, fps, bitrate, lower }) => {
    expect(shareEncodings({ width, height, frameRate: fps })).toEqual({ top: { width, height, maxBitrate: bitrate, maxFramerate: fps }, lower });
  });

  it.each<{ readonly what: string; readonly width: number; readonly height: number; readonly bitrate: number; readonly lower: readonly ShareLayer[] }>([
    // A portrait capture: the table keys on the longer side, and the layers
    // keep its shape.
    { what: 'a portrait 1080p', width: 1080, height: 1920, bitrate: 6_000_000, lower: [{ ...p720, width: 720, height: 1280 }] },
    { what: 'a portrait 1440p', width: 1440, height: 2560, bitrate: 10_000_000, lower: [{ ...p720, width: 720, height: 1280 }, { ...p1080, width: 1080, height: 1920 }] },
    // Wider than 16:9: each layer fits its box by the long side.
    { what: 'an ultrawide', width: 3440, height: 1440, bitrate: 18_000_000, lower: [{ ...p720, width: 1280, height: 535 }, { ...p1080, width: 1920, height: 803 }] },
    // Narrower than 16:9: by the short side.
    { what: 'a 4:3', width: 2048, height: 1536, bitrate: 10_000_000, lower: [{ ...p720, width: 960, height: 720 }, { ...p1080, width: 1440, height: 1080 }] },
    // Larger than a box on one side only: still a smaller layer.
    { what: 'a window just wider than 720p', width: 1300, height: 700, bitrate: 6_000_000, lower: [{ ...p720, width: 1280, height: 689 }] },
    { what: 'a 1920-wide strip', width: 1920, height: 400, bitrate: 6_000_000, lower: [{ ...p720, width: 1280, height: 266 }] },
    // Inside both boxes: no layer would be smaller, so none.
    { what: 'a small window', width: 1000, height: 700, bitrate: 6_000_000, lower: [] },
  ])('$what capture ($width x $height at 60 fps)', ({ width, height, bitrate, lower }) => {
    expect(shareEncodings({ width, height, frameRate: 60 })).toEqual({ top: { width, height, maxBitrate: bitrate, maxFramerate: 60 }, lower });
  });

  it('never sends more than 60 fps, nor a lower layer faster than the top', () => {
    expect(shareEncodings({ width: 3840, height: 2160, frameRate: 120 }).top.maxFramerate).toBe(60);
    expect(shareEncodings({ width: 3840, height: 2160, frameRate: 24 })).toEqual({
      top: { width: 3840, height: 2160, maxBitrate: 18_000_000, maxFramerate: 24 },
      lower: [p720, { ...p1080, maxFramerate: 24 }],
    });
    expect(shareEncodings({ width: 1920, height: 1080, frameRate: 10 }).lower).toEqual([{ ...p720, maxFramerate: 10 }]);
  });

  it('takes a capture whose settings do not say its size as 1080p, and one without a frame rate as 60 fps', () => {
    const top = { width: 1920, height: 1080, maxBitrate: 6_000_000, maxFramerate: 60 };
    expect(shareEncodings({})).toEqual({ top, lower: [p720] });
    expect(shareEncodings({ width: 0, height: 0, frameRate: Number.NaN })).toEqual({ top, lower: [p720] });
    expect(shareEncodings({ width: 3840 })).toEqual({ top, lower: [p720] });
  });
});

// The whole client over the adapter: the join token and the room name of a
// grant are handed to the SDK and kept nowhere a page or another script can
// read them back. The stub SDK logs both while connecting, as a real one may.
describe('the speakers chosen', () => {
  const outputs = [
    { kind: 'audiooutput', deviceId: 'default', label: 'Default - Headset' },
    { kind: 'audiooutput', deviceId: 'out-1', label: 'Desk speakers' },
    { kind: 'audiooutput', deviceId: 'out-2', label: '' },
    { kind: 'audioinput', deviceId: 'mic-1', label: 'Headset microphone' },
  ];
  // A remote microphone, as the SDK hands it over.
  const voice = (sid: string) => ({ source: 'microphone', kind: 'audio', sid, attach: () => document.createElement('audio'), detach: () => [] });

  it('lists the named outputs with permission requests off', async () => {
    state.devices = outputs;
    expect(await newLiveKitRoom().outputs()).toEqual([
      { id: 'default', label: 'Default - Headset' },
      { id: 'out-1', label: 'Desk speakers' },
    ]);
    expect(state.listed).toEqual([['audiooutput', false]]);
  });

  it("switches with the SDK's switch for outputs, and System default is the empty id", async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    await room.switchOutput('out-1');
    await room.switchOutput('');
    expect(state.outputs).toEqual(['out-1', '']);
    expect(state.calls).not.toContain('switchActiveDevice audioinput');
  });

  it('a refusal puts every voice back on the output in use, and rejects', async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    await room.switchOutput('out-1');
    state.outputFails = new DOMException('no such output', 'NotFoundError');
    await expect(room.switchOutput('out-2')).rejects.toMatchObject({ name: 'NotFoundError' });
    expect(state.outputs).toEqual(['out-1', 'out-2', 'out-1']);
  });

  it('the output kept in the tab is handed to a new room before it connects, so it is where the first voice plays', async () => {
    state.devices = outputs;
    const session: VoiceSessionResult = {
      ok: true,
      value: { livekit_url: 'wss://lk.example', identity: 'p7', rooms: [{ room: 'r', token: 't', can_publish: true, expires_at: '2026-10-16T00:00:15.000Z' }] },
    };
    const client = new VoiceClient({
      clock: new FakeClock(),
      random: () => 0.5,
      createSession: () => Promise.resolve(session),
      report: () => Promise.resolve('ok'),
      newRoom: newLiveKitRoom,
      claimShare: () => Promise.resolve({ ok: false, failure: 'network' }),
      stopShare: () => Promise.resolve(),
      outputSelection: true,
      output: { id: 'out-1', label: 'Desk speakers' },
    });
    client.join('general');
    await settle();
    await settle();
    expect(client.snapshot()).toMatchObject({ phase: 'connected', output: { id: 'out-1', label: 'Desk speakers' }, outputNote: null });
    expect(state.outputs).toEqual(['out-1']);
    expect(state.calls.indexOf('switchActiveDevice audiooutput')).toBeLessThan(state.calls.indexOf('connect'));
    await client.leave();
  });

  it("tells the client where the SDK moved sound when the output in use went, and not of the page's own switch", async () => {
    const moved: string[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onOutputMoved: (id) => moved.push(id) });
    await room.switchOutput('out-1');
    expect(moved).toEqual([]);
    // The SDK's own move, after Desk speakers went; and a change of another kind.
    state.handlers.get('adc')?.('audiooutput', 'default');
    state.handlers.get('adc')?.('audioinput', 'mic-1');
    expect(moved).toEqual(['default']);
  });

  it('on the system default, a move by the SDK is undone: sound stays on the system default', async () => {
    const moved: string[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onOutputMoved: (id) => moved.push(id) });
    state.handlers.get('adc')?.('audiooutput', 'out-1');
    await settle();
    expect(moved).toEqual([]);
    expect(state.outputs).toEqual(['']);
  });

  it('deafen still silences every voice, whatever the output', async () => {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    await room.switchOutput('out-1');
    state.handlers.get('ts')?.(voice('a'));
    room.setDeafened(true);
    state.handlers.get('ts')?.(voice('b'));
    const played = [...document.body.querySelectorAll('audio')];
    expect(played).toHaveLength(2);
    expect(played.every((el) => el.muted)).toBe(true);
    await room.close();
  });

  it('where the browser cannot choose an output, nothing is asked of the SDK', async () => {
    state.outputSelection = false;
    state.devices = outputs;
    const room = newLiveKitRoom();
    await room.switchOutput('out-1');
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(await room.outputs()).toEqual([]);
    await room.switchOutput('');
    expect(state.outputs).toEqual([]);
    expect(state.listed).toEqual([]);
  });
});

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
      claimShare: () => Promise.resolve({ ok: false, failure: 'network' }),
      stopShare: () => Promise.resolve(),
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

// Issue #669: one capture of the microphone at a time. A switch, or a change
// of voice processing, made while the first capture is under way waits for
// it and then moves it; it never captures beside it.
describe('a switch while the first capture is under way', () => {
  const processing = { echoCancellation: true, noiseSuppression: true, autoGainControl: true };
  const unprocessed = { echoCancellation: false, noiseSuppression: false, autoGainControl: false, voiceIsolation: false };
  const live = () => state.medias.filter((m) => m.readyState === 'live');

  // A connection made without the microphone, whose first capture is then
  // asked for and held.
  async function capturing(choice: string | null) {
    const room = newLiveKitRoom();
    room.setMicrophone(choice);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    const held = deferred<undefined>();
    state.capture = held;
    const publishing = room.publishMicrophone();
    await settle();
    expect(state.calls).toEqual(['connect', 'capture']);
    return { room, held, publishing };
  }

  it('waits for it, then moves it: one capture live, on the new choice, published once, the earlier one stopped', async () => {
    const { room, held, publishing } = await capturing('mic-1');
    const switching = room.switchMicrophone('mic-2');
    await settle();
    // Nothing else is captured while the first is under way.
    expect(state.calls).toEqual(['connect', 'capture']);
    held.resolve(undefined);
    await publishing;
    await switching;
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute', 'restart']);
    expect(state.restarts).toEqual([{ deviceId: { exact: 'mic-2' }, ...processing }]);
    expect(state.medias.map((m) => [m.deviceId, m.readyState])).toEqual([
      ['mic-1', 'ended'],
      ['mic-2', 'live'],
    ]);
    expect(state.tracks).toHaveLength(1);
    expect(room.microphoneInUse()).toBe('mic-2');
    // One tap, of the capture that is live.
    expect(state.contexts.filter((c) => c.closed === 0)).toHaveLength(1);
    expect(state.medias[1]?.clones).toHaveLength(1);
  });

  it('a switch and a change of processing both made meanwhile: one move, to the latest of each', async () => {
    const { room, held, publishing } = await capturing(null);
    const first = room.switchMicrophone('mic-2');
    room.setProcessing(false);
    const second = room.switchMicrophone('mic-2');
    held.resolve(undefined);
    await publishing;
    await first;
    await second;
    expect(state.calls.filter((c) => c === 'capture')).toHaveLength(1);
    expect(state.restarts).toEqual([{ deviceId: { exact: 'mic-2' }, ...unprocessed }]);
    expect(live()).toHaveLength(1);
    expect(room.microphoneInUse()).toBe('mic-2');
    expect(room.microphoneProcessing()).toEqual({ echoCancellation: false, noiseSuppression: false, autoGainControl: false });
  });

  it('a second publish joins the one under way', async () => {
    const { room, held, publishing } = await capturing('mic-1');
    const again = room.publishMicrophone();
    held.resolve(undefined);
    await publishing;
    await again;
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
    expect(live()).toHaveLength(1);
  });

  it('a first capture that fails fails the switch waiting for it, and nothing is captured', async () => {
    const { room, held, publishing } = await capturing('mic-1');
    const switching = room.switchMicrophone('mic-2');
    held.reject(new DOMException('gone', 'OverconstrainedError'));
    await expect(publishing).rejects.toThrow('gone');
    await expect(switching).rejects.toThrow('gone');
    expect(state.calls).toEqual(['connect', 'capture']);
    expect(live()).toHaveLength(0);
    expect(room.microphoneInUse()).toBeNull();
  });

  it('a close while both wait leaves no capture open', async () => {
    const { room, held, publishing } = await capturing('mic-1');
    const switching = room.switchMicrophone('mic-2');
    await room.close();
    held.resolve(undefined);
    await expect(publishing).rejects.toThrow();
    await expect(switching).rejects.toThrow();
    expect(state.calls).not.toContain('restart');
    expect(state.calls).not.toContain('publish');
    expect(live()).toHaveLength(0);
    expect(state.contexts).toHaveLength(0);
  });
});
