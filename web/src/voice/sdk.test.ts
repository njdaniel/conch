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
// Each person's volume (issue #661): set on the elements that play their
// microphone, at attach and at every change, 0 included; apart from deafen,
// kept through an output switch, and never on a share's sound.
// No network, no microphone, no screen.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { VoiceSessionResult } from '../api';
import { VoiceClient, companionQualities } from './connection';
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
  // Run inside connect, before it resolves (the SDK's signal events).
  duringConnect: (() => void) | null;
  audioContexts: number;
  // When set, an AudioContext's close() settles only when this does.
  contextClose: Promise<void> | null;
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
  // The encodings LiveKit made for the screen share, by rid, smallest first:
  // it names them by how many there are (q; q,h; q,h,f). And whether the
  // browser leaves the capture as it was when constrained (it may).
  rids: string[];
  ignoreConstraints: boolean;
  // When set, the next picker waits for the test to answer it (issue #1659).
  pickGate: Deferred<undefined> | null;
  // Issue #658: the captures made, what each restart was asked for, when set
  // the next restart waits for the test or fails, the devices the browser
  // lists, what each listing was asked, and the audio contexts made.
  medias: StubMedia[];
  restarts: unknown[];
  restart: Deferred<undefined> | null;
  restartFails: Error | null;
  devices: StubDevice[];
  listed: unknown[][];
  contexts: StubAudio[];
  // Issue #659: the filters the browser keeps on whatever it is asked.
  keptOn: string[];
  // Issue #660: whether the browser can choose an output, each output the
  // SDK was asked to switch to, and when set, a refusal of the next switch.
  outputSelection: boolean;
  outputs: string[];
  outputFails: Error | null;
  // The room's canPlaybackAudio, and what startAudio does: how many times it
  // was called, and the error it rejects with (null: it succeeds).
  canPlayback: boolean;
  startAudios: number;
  startAudioFails: Error | null;
  // ADR-015: what each camera capture was asked for, the captures made, a
  // refusal of the next one, and this connection's permission as LiveKit
  // last told it.
  videoOptions: unknown[];
  videoTracks: StubVideo[];
  videoFails: Error | null;
  // When set, each camera capture waits for the next of these, in order.
  videoHolds: Deferred<undefined>[];
  // When set, each restart of the camera waits for the next of these.
  videoRestartHolds: Deferred<undefined>[];
  permissions: { canPublish: boolean; canPublishSources: number[] } | undefined;
}
interface StubSettings {
  channelCount?: number;
  width?: number;
  height?: number;
  frameRate?: number;
}
interface StubParameters {
  encodings: RTCRtpEncodingParameters[];
}
interface StubSender {
  parameters: StubParameters;
  getParameters: () => StubParameters;
  setParameters: (parameters: StubParameters) => Promise<void>;
}
interface StubScreenTrack {
  source: string;
  stopped: number;
  mediaStreamTrack: { readyState: string; enabled: boolean; contentHint: string; constraints: MediaTrackConstraints[]; getSettings: () => StubSettings; applyConstraints: (constraints: MediaTrackConstraints) => Promise<void>; addEventListener: (type: string, fn: () => void) => void; ended: () => void };
  // What getSettings answers; a test may change it, as a capture can change size.
  settings: StubSettings;
  sender?: StubSender;
  publishOptions?: unknown;
  stop: () => void;
}
// A camera's capture, as the browser gives it and as the page wraps it.
interface StubVideo {
  source: string;
  stopped: number;
  userProvided: boolean;
  attached: unknown[];
  mediaStreamTrack: { readyState: string; enabled: boolean; deviceId: string; getSettings: () => { deviceId: string }; addEventListener: (type: string, fn: () => void) => void; ended: () => void };
  stop: () => void;
  attach: (el: unknown) => unknown;
  detach: (el?: unknown) => unknown[];
  restartTrack: (options: unknown) => Promise<void>;
}
interface StubRemotePub {
  source: string;
  kind: string;
  isSubscribed: boolean;
  isDesired?: boolean;
  trackSid?: string;
  setSubscribed: (on: boolean) => void;
  setVideoQuality?: (quality: number) => void;
  setVideoDimensions?: (dimensions: { width: number; height: number }) => void;
  // The SDK's requestedMaxQuality ?? HIGH: what the publication is capped at.
  videoQuality?: number;
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
    duringConnect: null,
    audioContexts: 0,
    contextClose: null,
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
    rids: ['q', 'h'],
    ignoreConstraints: false,
    pickGate: null,
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
    canPlayback: true,
    startAudios: 0,
    startAudioFails: null,
    videoOptions: [],
    videoTracks: [],
    videoFails: null,
    videoHolds: [],
    videoRestartHolds: [],
    permissions: undefined,
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
        contentHint: 'motion',
        constraints: [],
        getSettings: () => ({ ...track.settings }),
        applyConstraints: (constraints) => {
          track.mediaStreamTrack.constraints.push(constraints);
          const max = (value: ConstrainULong | undefined): number | undefined => typeof value === 'object' && 'max' in value && typeof value.max === 'number' ? value.max : undefined;
          const width = max(constraints.width);
          const height = max(constraints.height);
          const frameRate = max(constraints.frameRate);
          if (!state.ignoreConstraints) track.settings = { ...track.settings, ...(width === undefined ? {} : { width }), ...(height === undefined ? {} : { height }), ...(frameRate === undefined ? {} : { frameRate }) };
          state.calls.push('constrain');
          return Promise.resolve();
        },
        addEventListener: (type, fn) => {
          if (type === 'ended') listeners.push(fn);
        },
        // The browser ends the capture (its own stop control).
        ended: () => {
          track.mediaStreamTrack.readyState = 'ended';
          for (const fn of listeners) fn();
        },
      },
      sender: source === 'screen_share' ? {
        parameters: { encodings: state.rids.map((rid) => ({ rid })) },
        getParameters() {
          return { ...this.parameters, encodings: this.parameters.encodings.map((encoding) => ({ ...encoding })) };
        },
        setParameters(parameters) {
          this.parameters = parameters;
          state.calls.push('parameters');
          return Promise.resolve();
        },
      } : undefined,
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
  // A camera's capture of the device a constraint names exactly, or of the
  // browser's own choice.
  const videoMedia = (options: unknown): StubVideo['mediaStreamTrack'] => {
    const asked: Record<string, unknown> = typeof options === 'object' && options !== null ? { ...options } : {};
    const deviceId = asked['deviceId'];
    const exact = typeof deviceId === 'object' && deviceId !== null && 'exact' in deviceId ? String(deviceId.exact) : 'browser-choice';
    const listeners: (() => void)[] = [];
    const m: StubVideo['mediaStreamTrack'] = {
      readyState: 'live',
      enabled: true,
      deviceId: exact,
      getSettings: () => ({ deviceId: m.deviceId }),
      addEventListener: (type, fn) => {
        if (type === 'ended') listeners.push(fn);
      },
      // The device ends the capture (unplugged, taken by another app).
      ended: () => {
        m.readyState = 'ended';
        for (const fn of listeners) fn();
      },
    };
    return m;
  };
  class LocalVideoTrack implements StubVideo {
    source = 'unknown';
    stopped = 0;
    userProvided: boolean;
    attached: unknown[] = [];
    mediaStreamTrack: StubVideo['mediaStreamTrack'];
    private readonly ends = new Set<() => void>();
    constructor(mediaStreamTrack: StubVideo['mediaStreamTrack'], _constraints: unknown, userProvided = false) {
      this.mediaStreamTrack = mediaStreamTrack;
      this.userProvided = userProvided;
      this.wire(mediaStreamTrack);
      state.videoTracks.push(this);
    }
    // As the SDK's LocalTrack: the MediaStreamTrack's end is told as the
    // track's Ended, from whichever MediaStreamTrack it has now.
    private wire(m: StubVideo['mediaStreamTrack']): void {
      m.addEventListener('ended', () => {
        if (m !== this.mediaStreamTrack) return;
        for (const fn of [...this.ends]) fn();
      });
    }
    on = (event: string, fn: () => void) => {
      if (event === 'ended') this.ends.add(fn);
      return this;
    };
    off = (event: string, fn: () => void) => {
      if (event === 'ended') this.ends.delete(fn);
      return this;
    };
    stop = () => {
      this.stopped++;
      state.calls.push('stop camera track');
      this.mediaStreamTrack.readyState = 'ended';
    };
    attach = (el: unknown) => {
      this.attached.push(el);
      return el;
    };
    detach = (el?: unknown) => {
      this.attached = this.attached.filter((e) => el !== undefined && e !== el);
      return [];
    };
    restartTrack = async (options: unknown) => {
      state.videoOptions.push(options);
      const hold = state.videoRestartHolds.shift();
      if (hold !== undefined) await hold.promise;
      const m = videoMedia(options);
      this.mediaStreamTrack = m;
      this.wire(m);
    };
  }
  class Room {
    // Read with permission requests as asked: the stub never asks for any.
    static getLocalDevices(kind: string, requestPermissions: boolean) {
      state.listed.push([kind, requestPermissions]);
      return Promise.resolve(state.devices.filter((d) => d.kind === kind));
    }
    remoteParticipants = state.remote;
    // The room's own name, as the server says it once connected.
    name = 'sfu-room-name-1581';
    localParticipant = {
      identity: 'p7',
      get permissions() {
        return state.permissions;
      },
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
        const gate = state.pickGate;
        state.pickGate = null;
        return gate === null ? Promise.resolve(tracks) : gate.promise.then(() => tracks);
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
    get canPlaybackAudio() {
      return state.canPlayback;
    }
    startAudio() {
      state.startAudios++;
      if (state.startAudioFails !== null) return Promise.reject(state.startAudioFails);
      // As the SDK's: every element it plays is unmuted first, synchronously.
      for (const el of document.querySelectorAll('audio')) el.muted = false;
      state.canPlayback = true;
      state.handlers.get('apc')?.(true);
      return Promise.resolve();
    }
    connect(_url: string, token: string, options: unknown) {
      state.connectOptions = options;
      state.calls.push('connect');
      if (state.connectLog !== null) state.logger.methodFactory('warn')(`${state.connectLog} ${token}`, { token });
      state.duringConnect?.();
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
      SignalReconnecting: 'src',
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
      SignalConnected: 'sc',
      AudioPlaybackStatusChanged: 'apc',
    },
    TrackEvent: { Ended: 'ended' },
    PublishTrackError,
    DisconnectReason: { ROOM_DELETED: 5, PARTICIPANT_REMOVED: 4, DUPLICATE_IDENTITY: 2, SERVER_SHUTDOWN: 3 },
    ConnectionState: { Disconnected: 'disconnected' },
    LogLevel: { warn: 3 },
    LoggerNames: { Default: 'livekit' },
    Track: {
      Kind: { Audio: 'audio', Video: 'video' },
      Source: { Microphone: 'microphone', Camera: 'camera', ScreenShare: 'screen_share', ScreenShareAudio: 'screen_share_audio', Unknown: 'unknown' },
    },
    VideoQuality: { LOW: 0, HIGH: 2 },
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
    LocalVideoTrack,
    // The SDK's camera capture: the browser's track, which the adapter wraps.
    createLocalVideoTrack: (options: unknown) => {
      state.videoOptions.push(options);
      state.calls.push('capture camera');
      if (state.videoFails !== null) return Promise.reject(state.videoFails);
      const hold = state.videoHolds.shift();
      if (hold !== undefined) return hold.promise.then(() => new LocalVideoTrack(videoMedia(options), options, false));
      return Promise.resolve(new LocalVideoTrack(videoMedia(options), options, false));
    },
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

import { cameraOptions, cameraPublishOptions, captureOptions, grantWait, newLiveKitRoom, principalOf, sdkLog, shareEncodings, wanted, type ShareLayer } from './sdk';
import type { Room, RoomEvents } from './connection';
import { CameraClient } from './camera';
import { Track } from 'livekit-client';

// What the module did to the SDK's loggers when it loaded, before any test.
const atLoad = { levelCalls: [...state.levelCalls], factoryReplaced: state.logger.methodFactory !== state.stubFactory, calls: [...state.calls] };

// An audio context as the browser has one: it runs, may be suspended, says so
// to its listeners, and counts what is done to it (issue #1794). sample is the
// byte every read of the analyser fills the buffer with (128 is silence).
class StubAudio {
  closed = 0;
  resumed = 0;
  state: 'running' | 'suspended' | 'interrupted' | 'closed' = 'running';
  // What resume() does: runs the context, leaves it as it is, or refuses.
  onResume: 'run' | 'stay' | 'refuse' = 'run';
  sample = 128;
  sources = 0;
  disconnected = 0;
  readonly listeners = new Set<() => void>();
  createAnalyser() {
    return { fftSize: 0, getByteTimeDomainData: (b: Uint8Array) => b.fill(this.sample), disconnect: () => { this.disconnected++; } };
  }
  createMediaStreamSource() {
    this.sources++;
    return { connect: () => undefined, disconnect: () => { this.disconnected++; } };
  }
  addEventListener(type: string, l: () => void) {
    if (type === 'statechange') this.listeners.add(l);
  }
  removeEventListener(type: string, l: () => void) {
    if (type === 'statechange') this.listeners.delete(l);
  }
  resume() {
    this.resumed++;
    if (this.onResume === 'refuse') return Promise.reject(new DOMException('no gesture', 'NotAllowedError'));
    if (this.onResume === 'run') this.become('running');
    return Promise.resolve();
  }
  close() {
    this.closed++;
    this.state = 'closed';
    return state.contextClose ?? Promise.resolve();
  }
  // The browser changing the context's state, and saying so.
  become(next: StubAudio['state']) {
    if (this.state === next) return;
    this.state = next;
    for (const l of [...this.listeners]) l();
  }
  asContext(): AudioContext {
    return this as unknown as AudioContext;
  }
}

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
  state.duringConnect = null;
  state.audioContexts = 0;
  state.canPlayback = true;
  state.startAudios = 0;
  state.startAudioFails = null;
  state.contextClose = null;
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
  state.videoOptions.length = 0;
  state.videoTracks.length = 0;
  state.videoFails = null;
  state.videoHolds.length = 0;
  state.videoRestartHolds.length = 0;
  state.permissions = undefined;
  vi.stubGlobal(
    'AudioContext',
    class extends StubAudio {
      constructor() {
        super();
        state.audioContexts++;
        state.contexts.push(this);
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
  onStreamTracks: () => undefined,
  onMicrophoneLost: () => undefined,
  onMeter: () => undefined,
  onPlayback: () => undefined,
  onDevicesChanged: () => undefined,
  onOutputMoved: () => undefined,
  onCameraEnded: () => undefined,
  onCameraChanged: () => undefined,
  onCameraTracks: () => undefined,
};

describe('the SDK adapter', () => {
  // Issue #501: muted before publishing, so that LiveKit lists the
  // microphone muted from the first; and again after, as the second layer.
  it('captures with echo cancellation, noise suppression and auto gain, and mutes before publishing and straight after', async () => {
    await newLiveKitRoom().connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, events);
    expect(state.captureOptions).toEqual({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
  });

  // The tap only feeds the level meter: an audio context the browser refuses
  // must not tear down a join that worked, or the client would retry for ever.
  it('a tap that cannot be made leaves the room connected and the track published, and says there is no meter', async () => {
    vi.stubGlobal(
      'AudioContext',
      function AudioContext() {
        throw new DOMException('too many audio contexts', 'NotSupportedError');
      },
    );
    const meter: boolean[] = [];
    const names: (string | undefined)[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, {
      ...events,
      onMeter: (on: boolean, name?: string) => {
        meter.push(on);
        names.push(name);
      },
    });
    expect(state.calls).toEqual(['connect', 'capture', 'premute', 'publish', 'mute']);
    expect(meter).toEqual([false]);
    // The browser's error is named to the client.
    expect(names).toEqual(['NotSupportedError']);
    // The clone made before the context failed is let go.
    expect(state.medias[0]?.clones[0]?.stopped).toBe(1);
    expect(room.level()).toBe(0);
    await room.setSending(true);
    await room.close();
  });

  // An AudioContext whose source cannot be made: the failure is after the
  // context exists, so the context itself is closed too. fails is read at
  // each construction, so a test can let the first tap work and fail the next.
  function sourceThrows(fails: () => boolean) {
    vi.stubGlobal(
      'AudioContext',
      class extends StubAudio {
        private readonly fail = fails();
        constructor() {
          super();
          state.audioContexts++;
          state.contexts.push(this);
        }
        override createMediaStreamSource(): ReturnType<StubAudio['createMediaStreamSource']> {
          if (this.fail) throw new DOMException('no source', 'InvalidStateError');
          return super.createMediaStreamSource();
        }
      },
    );
  }

  // iOS Safari plays nothing until a tap: the adapter tells the client what the
  // room says after the join and whenever it changes, and the tap goes to the
  // room's startAudio in the same call.
  describe('playback held back until a tap', () => {
    it('says what the room says after the join', async () => {
      state.canPlayback = false;
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onPlayback: (on: boolean) => told.push(on) });
      expect(told).toEqual([false]);
      await room.close();
    });

    it('says true after the join when the browser lets it play', async () => {
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onPlayback: (on: boolean) => told.push(on) });
      expect(told).toEqual([true]);
      await room.close();
    });

    it('says what the room says whenever the event fires', async () => {
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onPlayback: (on: boolean) => told.push(on) });
      state.canPlayback = false;
      state.handlers.get('apc')?.(false);
      state.canPlayback = true;
      state.handlers.get('apc')?.(true);
      expect(told).toEqual([true, false, true]);
      await room.close();
    });

    it('the tap calls startAudio at once, and success is told by the event', async () => {
      state.canPlayback = false;
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onPlayback: (on: boolean) => told.push(on) });
      const done = room.startAudio();
      // Called in the same turn as the tap, before anything was awaited.
      expect(state.startAudios).toBe(1);
      await done;
      expect(told).toEqual([false, true]);
      await room.close();
    });

    it('a refusal rejects with the browser\'s error and tells nothing more', async () => {
      state.canPlayback = false;
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onPlayback: (on: boolean) => told.push(on) });
      state.startAudioFails = new DOMException('blocked', 'NotAllowedError');
      await expect(room.startAudio()).rejects.toMatchObject({ name: 'NotAllowedError' });
      expect(told).toEqual([false]);
      await room.close();
    });

    // The room says canPlaybackAudio the moment any one element plays, while
    // a voice the browser refused earlier stays paused (#1793).
    it('stays false while a voice the browser refused is paused, and says true when it plays', async () => {
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onPlayback: (on: boolean) => told.push(on) });
      const made: HTMLAudioElement[] = [];
      const refused = { source: 'microphone', kind: 'audio', sid: 'r1', attach: () => { const el = document.createElement('audio'); made.push(el); return el; }, detach: () => made.splice(0) };
      state.handlers.get('ts')?.(refused, undefined, { identity: 'p8' });
      const el = made[0];
      expect(el).toBeDefined();
      if (el === undefined) return;
      // jsdom's element is paused; another track played, so the room says true.
      state.canPlayback = true;
      state.handlers.get('apc')?.(true);
      expect(told).toEqual([true, false]);
      let paused = true;
      Object.defineProperty(el, 'paused', { get: () => paused });
      paused = false;
      el.dispatchEvent(new Event('playing'));
      expect(told).toEqual([true, false, true]);
      // The browser lets go of it again (an interruption): the tap is back.
      paused = true;
      el.dispatchEvent(new Event('pause'));
      expect(told).toEqual([true, false, true, false]);
      await room.close();
    });

    it('a voice that left no longer holds playback back', async () => {
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onPlayback: (on: boolean) => told.push(on) });
      const made: HTMLAudioElement[] = [];
      const refused = { source: 'microphone', kind: 'audio', sid: 'r1', attach: () => { const el = document.createElement('audio'); made.push(el); return el; }, detach: () => made.splice(0) };
      state.handlers.get('ts')?.(refused, undefined, { identity: 'p8' });
      state.handlers.get('apc')?.(true);
      state.handlers.get('tu')?.(refused, undefined, { identity: 'p8' });
      expect(told).toEqual([true, false, true]);
      await room.close();
    });

    it('tells nothing after a close', async () => {
      const told: boolean[] = [];
      const room = newLiveKitRoom();
      await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onPlayback: (on: boolean) => told.push(on) });
      await room.close();
      state.handlers.get('apc')?.(false);
      expect(told).toEqual([true]);
    });
  });

  it('a source that cannot be made closes the context and its clone, and says there is no meter', async () => {
    sourceThrows(() => true);
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onMeter: (on: boolean) => meter.push(on) });
    expect(meter).toEqual([false]);
    expect(state.contexts).toHaveLength(1);
    expect(state.contexts[0]?.closed).toBe(1);
    expect(state.medias[0]?.clones[0]?.stopped).toBe(1);
    expect(room.level()).toBe(0);
    await room.close();
  });

  // Switching microphones makes a new tap, so the meter can come back or go.
  it('a tap that failed at the join is made again by a switch, and says the meter is back', async () => {
    let failing = true;
    sourceThrows(() => failing);
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onMeter: (on: boolean) => meter.push(on) });
    expect(meter).toEqual([false]);
    failing = false;
    await room.switchMicrophone('mic-2');
    expect(meter).toEqual([false, true]);
    await room.close();
  });

  it('a tap that works and then fails at a switch says the meter is gone, and lets the old tap go', async () => {
    let failing = false;
    sourceThrows(() => failing);
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, onMeter: (on: boolean) => meter.push(on) });
    expect(meter).toEqual([true]);
    const old = state.medias[0];
    failing = true;
    await room.switchMicrophone('mic-2');
    expect(meter).toEqual([true, false]);
    expect(old?.clones[0]?.stopped).toBe(1);
    expect(state.contexts[0]?.closed).toBe(1);
    // The failed tap's own context and clone are let go as well.
    expect(state.contexts[1]?.closed).toBe(1);
    expect(state.medias[1]?.clones[0]?.stopped).toBe(1);
    expect(room.level()).toBe(0);
    await room.close();
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

  it("holds the room's own name once connected, though the caller gave none (the companion's grant names no room)", async () => {
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    await room.connect('wss://lk.example', 'tok-abcdef', '', false, events);
    state.logger.methodFactory('warn')('joined sfu-room-name-1581 as p7.screen');
    const lines = sdkLog.recent().join('\n');
    expect(lines).toContain('joined');
    expect(lines).not.toContain('sfu-room-name-1581');
  });

  it("holds the room's own name at the signal event, before connect resolves", async () => {
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    const lines: string[] = [];
    state.duringConnect = () => {
      state.handlers.get('sc')?.();
      state.logger.methodFactory('warn')('joined sfu-room-name-1581 as p7.screen');
      lines.push(sdkLog.recent().join('\n'));
    };
    await room.connect('wss://lk.example', 'tok-abcdef', '', false, events);
    expect(lines.join('\n')).toContain('joined');
    expect(lines.join('\n')).not.toContain('sfu-room-name-1581');
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

  // Issue #1764: the leave must not wait for the tap's AudioContext to close;
  // a page closed meanwhile never sent it.
  it('close starts the disconnect at once, even when the tap\'s AudioContext close hangs', async () => {
    const { room } = await connected('mic-1');
    let release: () => void = () => undefined;
    state.contextClose = new Promise<void>((resolve) => {
      release = resolve;
    });
    state.calls.length = 0;
    const closing = room.close();
    await settle();
    expect(state.calls).toContain('disconnect');
    release();
    await closing;
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
  let sids = 0;
  const remotePub = (source: string, kind = source === 'microphone' || source === 'screen_share_audio' ? 'audio' : 'video'): StubRemotePub => {
    const p: StubRemotePub = {
      source,
      kind,
      trackSid: `TR_${String(++sids)}`,
      isSubscribed: false,
      videoQuality: 2,
      setSubscribed: (on) => {
        p.isSubscribed = on;
        state.calls.push(`subscribe ${source} ${String(on)}`);
      },
      // As the SDK: the cap is the publication's, and asking for the same
      // one again sends nothing.
      setVideoQuality: (quality) => {
        if (p.videoQuality === quality) return;
        p.videoQuality = quality;
        state.calls.push(`quality ${source} ${String(quality)}`);
      },
      setVideoDimensions: ({ width, height }) => {
        p.videoQuality = 2;
        state.calls.push(`dimensions ${source} ${String(width)}x${String(height)}`);
      },
    };
    return p;
  };
  const layers = () => state.calls.filter((c) => c.startsWith('quality') || c.startsWith('dimensions'));

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
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', mic], ['b', screen], ['c', sound]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect([mic.isSubscribed, screen.isSubscribed, sound.isSubscribed]).toEqual([true, false, false]);
    room.setWatches(new Set([8]), null);
    expect([mic.isSubscribed, screen.isSubscribed, sound.isSubscribed]).toEqual([true, true, true]);
    // A track published later follows the same choice; a camera never.
    const later = remotePub('microphone');
    const camera = remotePub('camera');
    state.handlers.get('tp')?.(later);
    state.handlers.get('tp')?.(camera);
    expect([later.isSubscribed, camera.isSubscribed]).toEqual([true, false]);
    room.setWatches(new Set(), null);
    expect([mic.isSubscribed, screen.isSubscribed, sound.isSubscribed]).toEqual([true, false, false]);
  });

  it('with two sharers subscribes picture and sound per Watch, leaves the focused one to adaptive stream, holds the other at LOW, and never wants unwatched or own tracks', async () => {
    const eight = remotePub('screen_share');
    const eightSound = remotePub('screen_share_audio');
    const nine = remotePub('screen_share');
    const nineSound = remotePub('screen_share_audio');
    const unwatched = remotePub('screen_share');
    const own = remotePub('screen_share');
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', eight], ['b', eightSound]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.remote.set('p9', { identity: 'p9', trackPublications: new Map([['a', nine], ['b', nineSound]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.remote.set('p10', { identity: 'p10', trackPublications: new Map([['a', unwatched]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.remote.set('p7', { identity: 'p7', trackPublications: new Map([['a', own]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    room.setWatches(new Set([8, 9]), 8);
    expect([eight.isSubscribed, eightSound.isSubscribed, nine.isSubscribed, nineSound.isSubscribed, unwatched.isSubscribed, own.isSubscribed]).toEqual([true, true, true, true, false, false]);
    // 8, focused, is never capped: adaptive stream sizes it to its element.
    expect(layers()).toEqual(['quality screen_share 0']);
    room.setWatches(new Set([8, 9]), 9);
    // 8 is held; 9's cap is lifted by a bound above every layer, so the
    // element's size decides (a phone gets a smaller layer than a stage).
    expect(layers()).toEqual(['quality screen_share 0', 'quality screen_share 0', 'dimensions screen_share 7680x4320']);
    // Asked again, nothing changes.
    room.setWatches(new Set([8, 9]), 9);
    expect(layers()).toHaveLength(3);
  });

  // Issue #1512 (voice-multi-share.md §7.4, §11.6): the desktop's companion
  // under B2 takes the watched streams' tracks by publisher and nothing else:
  // the focused one at the top layer, the others at the lowest; never its own
  // person's stream (the person's p7 or the companion's own p7.screen), never
  // an unwatched one, never a voice or a camera, whoever is in the room.
  it('the companion subscribes the watched streams only, the focused at the top layer and the others at LOW, and never its own tracks, a voice or a camera', async () => {
    const entries: Record<string, StubRemotePub[]> = {
      p8: [remotePub('screen_share'), remotePub('screen_share_audio'), remotePub('microphone'), remotePub('camera')],
      'p9.screen': [remotePub('screen_share'), remotePub('screen_share_audio')],
      p10: [remotePub('screen_share'), remotePub('screen_share_audio'), remotePub('microphone')],
      // This person's own: the person's identity and their own companion's.
      p7: [remotePub('screen_share'), remotePub('screen_share_audio'), remotePub('microphone')],
      'p7.screen': [remotePub('screen_share'), remotePub('screen_share_audio')],
    };
    for (const [identity, pubs] of Object.entries(entries)) {
      state.remote.set(identity, { identity, trackPublications: new Map(pubs.map((p, i) => [String(i), p])) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    }
    const asked = (identity: string): string[] => (entries[identity] ?? []).map((p) => `${p.source}:${String(p.isSubscribed)}`);
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(Object.keys(entries).flatMap(asked).filter((s) => s.endsWith(':true')), 'nothing before a Watch').toEqual([]);
    room.setWatches(new Set([8, 9, 7]), 8);
    expect(asked('p8')).toEqual(['screen_share:true', 'screen_share_audio:true', 'microphone:false', 'camera:false']);
    expect(asked('p9.screen')).toEqual(['screen_share:true', 'screen_share_audio:true']);
    expect(asked('p10')).toEqual(['screen_share:false', 'screen_share_audio:false', 'microphone:false']);
    expect(asked('p7')).toEqual(['screen_share:false', 'screen_share_audio:false', 'microphone:false']);
    expect(asked('p7.screen')).toEqual(['screen_share:false', 'screen_share_audio:false']);
    // 8, focused, is left to adaptive stream; 9 is held to the lowest layer;
    // the sound has no layer.
    expect(layers()).toEqual(['quality screen_share 0']);
    expect(entries.p8?.[0]?.videoQuality).toBe(2);
    expect(entries['p9.screen']?.[0]?.videoQuality).toBe(0);
    // Focus moves: 9 is lifted, 8 is held. Voices and cameras stay unasked.
    room.setWatches(new Set([8, 9]), 9);
    expect(entries.p8?.[0]?.videoQuality).toBe(0);
    expect(entries['p9.screen']?.[0]?.videoQuality).toBe(2);
    expect(asked('p8')).toEqual(['screen_share:true', 'screen_share_audio:true', 'microphone:false', 'camera:false']);
    // A voice published later is not taken either.
    const late = remotePub('microphone');
    state.handlers.get('tp')?.(late, { identity: 'p8' });
    expect(late.isSubscribed).toBe(false);
    // Stop watching 8: its picture and sound are let go; 9's stay.
    room.setWatches(new Set([9]), 9);
    expect(asked('p8').slice(0, 2)).toEqual(['screen_share:false', 'screen_share_audio:false']);
    expect(asked('p9.screen')).toEqual(['screen_share:true', 'screen_share_audio:true']);
  });

  it('a stream held at LOW, let go and watched again focused, has its cap lifted: the cap is the publication\'s', async () => {
    const nine = remotePub('screen_share');
    state.remote.set('p9', { identity: 'p9', trackPublications: new Map([['a', nine]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    room.setWatches(new Set([9]), null);
    expect(layers()).toEqual(['quality screen_share 0']);
    room.setWatches(new Set(), null);
    expect(nine.isSubscribed).toBe(false);
    room.setWatches(new Set([9]), 9);
    expect(layers()).toEqual(['quality screen_share 0', 'dimensions screen_share 7680x4320']);
  });

  it('a signal-only resume is reported as reconnecting and then reconnected', async () => {
    const seen: string[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, {
      ...events,
      onReconnecting: () => seen.push('reconnecting'),
      onReconnected: () => seen.push('reconnected'),
    });
    state.handlers.get('src')?.();
    state.handlers.get('rd')?.();
    expect(seen).toEqual(['reconnecting', 'reconnected']);
  });

  it('after a full reconnect a new publication under the same sid is capped at LOW again', async () => {
    const eight = remotePub('screen_share');
    const nine = remotePub('screen_share');
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', eight]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.remote.set('p9', { identity: 'p9', trackPublications: new Map([['a', nine]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    room.setWatches(new Set([8, 9]), 8);
    expect(layers()).toEqual(['quality screen_share 0']);
    // The SDK made the room anew: 9's publication is a new one, uncapped,
    // under the same sid.
    const again = remotePub('screen_share');
    again.trackSid = nine.trackSid;
    state.remote.set('p9', { identity: 'p9', trackPublications: new Map([['a', again]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.handlers.get('rd')?.();
    expect(again.isSubscribed).toBe(true);
    expect(layers()).toEqual(['quality screen_share 0', 'quality screen_share 0']);
  });

  it("a desktop companion's stream, published as p8.screen, is 8's: watched, held to its layer and played into 8's tile; a viewer's identity is nobody's stream", async () => {
    const told: unknown[] = [];
    const screen = remotePub('screen_share');
    const sound = remotePub('screen_share_audio');
    const viewerShare = remotePub('screen_share');
    state.remote.set('p8.screen', { identity: 'p8.screen', trackPublications: new Map([['a', screen], ['b', sound]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.remote.set('p9.view', { identity: 'p9.view', trackPublications: new Map([['a', viewerShare]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onStreamTracks: (p, t) => told.push([p, t]) });
    expect([screen.isSubscribed, sound.isSubscribed]).toEqual([false, false]);
    room.setWatches(new Set([8, 9]), null);
    expect([screen.isSubscribed, sound.isSubscribed, viewerShare.isSubscribed]).toEqual([true, true, false]);
    expect(layers()).toEqual(['quality screen_share 0']);
    room.setWatches(new Set([8]), 8);
    expect(layers().at(-1)).toBe('dimensions screen_share 7680x4320');
    const attached: string[] = [];
    const track = (source: string, kind: string) => ({
      source,
      kind,
      sid: source,
      attach: (el?: HTMLMediaElement) => {
        attached.push(`${source} ${el?.tagName ?? 'new'}`);
        return el ?? document.createElement('audio');
      },
      detach: () => [],
    });
    const sink = { video: document.createElement('video'), audio: document.createElement('audio') };
    room.attachStream(8, sink);
    const companion = { identity: 'p8.screen' };
    state.handlers.get('ts')?.(track('screen_share', 'video'), screen, companion);
    state.handlers.get('ts')?.(track('screen_share_audio', 'audio'), sound, companion);
    // Played into 8's tile, and never into the page as a voice.
    expect(attached).toEqual(['screen_share VIDEO', 'screen_share_audio AUDIO']);
    expect(document.body.querySelectorAll('audio')).toHaveLength(0);
    expect(told).toEqual([[8, { video: true, audio: false }], [8, { video: true, audio: true }]]);
    expect(screen.isSubscribed).toBe(true);
    // Unwatched, it is let go.
    room.setWatches(new Set(), null);
    expect([screen.isSubscribed, sound.isSubscribed]).toEqual([false, false]);
  });

  it("never watches this person's own stream, from their own identity or their companion, whatever this page connected as", async () => {
    const companionScreen = remotePub('screen_share');
    const otherScreen = remotePub('screen_share');
    state.remote.set('p7.screen', { identity: 'p7.screen', trackPublications: new Map([['a', companionScreen]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', otherScreen]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    room.setWatches(new Set([7, 8]), 7);
    expect([companionScreen.isSubscribed, otherScreen.isSubscribed]).toEqual([false, true]);
  });

  it("plays a share into the view's elements, not into the page, and says what arrived", async () => {
    const told: unknown[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onStreamTracks: (p, t) => told.push([p, t]) });
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
    room.setWatches(new Set([8]), null);
    const sharer = { identity: 'p8' };
    // Arrives before the view is open: kept, and attached once it opens.
    state.handlers.get('ts')?.(video, remotePub('screen_share'), sharer);
    const sink = { video: document.createElement('video'), audio: document.createElement('audio') };
    room.attachStream(8, sink);
    state.handlers.get('ts')?.(audio, remotePub('screen_share_audio'), sharer);
    expect(attached).toEqual(['screen_share VIDEO', 'screen_share_audio AUDIO']);
    expect(document.body.querySelectorAll('audio')).toHaveLength(0);
    expect(told).toEqual([[8, { video: true, audio: false }], [8, { video: true, audio: true }]]);
    state.handlers.get('tu')?.(video);
    expect(told.at(-1)).toEqual([8, { video: false, audio: true }]);
    room.attachStream(8, null);
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

  it("asks the picker for the default 1080p30 with unprocessed sound, tab and system sound offered, this page's own sound and this tab left out", async () => {
    const room = await connected();
    expect(await room.pickScreen()).toEqual({ audio: true });
    expect(state.screenOptions).toEqual({
      video: true,
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
      resolution: { width: 1920, height: 1080, frameRate: 30 },
      contentHint: 'motion',
      systemAudio: 'include',
      selfBrowserSurface: 'exclude',
      surfaceSwitching: 'include',
      suppressLocalAudioPlayback: false,
    });
    // Nothing is published by picking; the chosen caps are held on the capture.
    expect(state.calls).toEqual(['pick', 'constrain']);
  });

  it('uses motion for the default and detail for a 15 fps choice', () => {
    expect(captureOptions()).toEqual({
      video: true,
      audio: { echoCancellation: false, noiseSuppression: false, autoGainControl: false, channelCount: 2, restrictOwnAudio: true },
      resolution: { width: 1920, height: 1080, frameRate: 30 },
      contentHint: 'motion',
      systemAudio: 'include',
      selfBrowserSurface: 'exclude',
      surfaceSwitching: 'include',
      suppressLocalAudioPlayback: false,
    });
    expect(captureOptions({ resolution: '720p', frameRate: 15 }).contentHint).toBe('detail');
  });

  // The picture's encodings follow its track's settings, read after the picker.
  // A 1080p30 capture has a 720p layer and no 1080p one.
  it('publishes a 1080p picture as captured with a 720p layer at 30 fps, and the sound as stereo music', async () => {
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
          screenShareSimulcastLayers: [{ width: 1280, height: 720, encoding: { maxBitrate: 2_000_000, maxFramerate: 30 } }],
        },
      },
      {
        source: 'screen_share_audio',
        options: { source: 'screen_share_audio', audioPreset: { maxBitrate: 128000 }, dtx: false, red: false, forceStereo: true },
      },
    ]);
  });

  it('changes a live capture and its sender encodings without republishing', async () => {
    const room = await connected();
    await room.pickScreen();
    await room.publishScreen();
    const track = state.screenTracks[0];
    if (track === undefined) throw new Error('no screen track');
    const published = [...state.published];
    state.calls.length = 0;
    await room.setShareQuality({ resolution: '720p', frameRate: 15 });
    expect(track.mediaStreamTrack.contentHint).toBe('detail');
    expect(track.mediaStreamTrack.constraints).toEqual([{ width: { max: 1920 }, height: { max: 1080 }, frameRate: { max: 30 } }, { width: { max: 1280 }, height: { max: 720 }, frameRate: { max: 15 } }]);
    expect(track.sender?.parameters.encodings).toEqual([
      { rid: 'q', maxBitrate: 6_000_000, maxFramerate: 15, scaleResolutionDownBy: 1 },
      { rid: 'h', maxBitrate: 6_000_000, maxFramerate: 15, scaleResolutionDownBy: 1 },
    ]);
    expect(state.calls).toEqual(['constrain', 'parameters']);
    expect(state.published).toEqual(published);
  });

  // LiveKit's real layouts: one encoding is q, two are q,h, three are q,h,f.
  const q = (maxBitrate: number, maxFramerate: number, scaleResolutionDownBy: number, rid: string) => ({ rid, maxBitrate, maxFramerate, scaleResolutionDownBy });
  it.each([
    { name: '1 encoding, 720p to Source', rids: ['q'], from: { resolution: '720p', frameRate: 30 }, to: { resolution: 'source', frameRate: 60 }, want: [q(18_000_000, 60, 1, 'q')] },
    { name: '2 encodings, 720p to Source', rids: ['q', 'h'], from: { resolution: '720p', frameRate: 30 }, to: { resolution: 'source', frameRate: 60 }, want: [q(2_000_000, 30, 3, 'q'), q(18_000_000, 60, 1, 'h')] },
    { name: '3 encodings, 720p to Source', rids: ['q', 'h', 'f'], from: { resolution: '720p', frameRate: 30 }, to: { resolution: 'source', frameRate: 60 }, want: [q(2_000_000, 30, 3, 'q'), q(4_000_000, 30, 2, 'h'), q(18_000_000, 60, 1, 'f')] },
    { name: '3 encodings, Source to 720p', rids: ['q', 'h', 'f'], from: { resolution: 'source', frameRate: 60 }, to: { resolution: '720p', frameRate: 30 }, want: [q(6_000_000, 30, 1, 'q'), q(6_000_000, 30, 1, 'h'), q(6_000_000, 30, 1, 'f')] },
    { name: '2 encodings, 1080p to 720p', rids: ['q', 'h'], from: { resolution: '1080p', frameRate: 30 }, to: { resolution: '720p', frameRate: 30 }, want: [q(6_000_000, 30, 1, 'q'), q(6_000_000, 30, 1, 'h')] },
  ] as const)('puts the chosen top on the highest encoding: $name', async ({ rids, from, to, want }) => {
    state.captured = { width: 3840, height: 2160, frameRate: 60 };
    state.rids = [...rids];
    const room = await connected();
    await room.pickScreen(from);
    await room.publishScreen();
    await room.setShareQuality(to);
    const encodings = state.screenTracks[0]?.sender?.parameters.encodings;
    // The capture is 4K until constrained; its size follows the constraint.
    expect(encodings).toEqual(want);
  });

  it('holds the chosen cap at publish when the picker returns a larger capture than asked', async () => {
    state.captured = { width: 3840, height: 2160, frameRate: 60 };
    state.ignoreConstraints = true;
    const room = await connected();
    await room.pickScreen({ resolution: '1080p', frameRate: 30 });
    await room.publishScreen();
    const track = state.screenTracks[0];
    expect(track?.mediaStreamTrack.constraints).toEqual([{ width: { max: 1920 }, height: { max: 1080 }, frameRate: { max: 30 } }]);
    expect(track?.sender?.parameters.encodings).toEqual([
      { rid: 'q', maxBitrate: 2_000_000, maxFramerate: 30, scaleResolutionDownBy: 3 },
      { rid: 'h', maxBitrate: 6_000_000, maxFramerate: 30, scaleResolutionDownBy: 2 },
    ]);
  });

  // Auto (issue #1659): settled once from the capture as it came, then capped
  // as settled; what it settled to is what is declared and published.
  it.each([
    { name: '1920x1080 is Source, capped at 4K', captured: { width: 1920, height: 1080, frameRate: 60 }, want: { resolution: 'source', frameRate: 30 }, cap: { width: { max: 3840 }, height: { max: 2160 }, frameRate: { max: 30 } }, top: 6_000_000 },
    { name: '2560x1440 is 1080p, capped at 1080p', captured: { width: 2560, height: 1440, frameRate: 60 }, want: { resolution: '1080p', frameRate: 30 }, cap: { width: { max: 1920 }, height: { max: 1080 }, frameRate: { max: 30 } }, top: 6_000_000 },
    { name: '4K is 1080p, capped at 1080p', captured: { width: 3840, height: 2160, frameRate: 60 }, want: { resolution: '1080p', frameRate: 30 }, cap: { width: { max: 1920 }, height: { max: 1080 }, frameRate: { max: 30 } }, top: 6_000_000 },
  ])('Auto: $name', async ({ captured, want, cap, top }) => {
    state.captured = captured;
    const room = await connected();
    expect(room.settled({ resolution: 'auto', frameRate: 30 })).toEqual({ resolution: '1080p', frameRate: 30 });
    await room.pickScreen({ resolution: 'auto', frameRate: 30 });
    expect(room.settled({ resolution: 'auto', frameRate: 30 })).toEqual(want);
    await room.publishScreen();
    const track = state.screenTracks[0];
    expect(track?.mediaStreamTrack.constraints).toEqual([cap]);
    expect(state.published[0]?.options).toMatchObject({ screenShareEncoding: { maxBitrate: top, maxFramerate: 30 } });
    expect(track?.mediaStreamTrack.contentHint).toBe('motion');
    // Other choices settle to themselves, and Auto stays as it was settled.
    expect(room.settled({ resolution: '720p', frameRate: 15 })).toEqual({ resolution: '720p', frameRate: 15 });
    await room.setShareQuality({ resolution: 'auto', frameRate: 30 });
    expect(room.settled({ resolution: 'auto', frameRate: 30 })).toEqual(want);
  });

  it('Auto chosen live lifts an earlier cap to read the screen\'s size', async () => {
    state.captured = { width: 2560, height: 1440, frameRate: 60 };
    const old = await connected();
    await old.pickScreen({ resolution: '720p', frameRate: 30 });
    await old.publishScreen();
    await old.setShareQuality({ resolution: 'auto', frameRate: 30 });
    // The 720p cap was lifted to Auto's ask before the size was read.
    expect(old.settled({ resolution: 'auto', frameRate: 30 })).toEqual({ resolution: '1080p', frameRate: 30 });
  });

  it('a kept capture carries what Auto settled to, for the next connection', async () => {
    state.captured = { width: 1920, height: 1080, frameRate: 60 };
    const old = await connected();
    await old.pickScreen({ resolution: 'auto', frameRate: 30 });
    expect(old.settled({ resolution: 'auto', frameRate: 30 })).toEqual({ resolution: 'source', frameRate: 30 });
    const kept = old.keepScreen();
    expect(kept).not.toBeNull();
    const next = await connected();
    if (kept !== null) next.adoptScreen(kept);
    expect(next.settled({ resolution: 'auto', frameRate: 30 })).toEqual({ resolution: 'source', frameRate: 30 });
  });

  // A choice made while the share is being picked, or while a kept capture is
  // resumed (issue #1659): the capture is settled and capped to the choice as it
  // is when it is published, to Auto and from it.
  it.each([
    { name: 'to Auto while the picker is open', from: { resolution: '720p', frameRate: 15 }, to: { resolution: 'auto', frameRate: 30 }, captured: { width: 1920, height: 1080, frameRate: 60 }, settled: { resolution: 'source', frameRate: 30 }, cap: { width: { max: 3840 }, height: { max: 2160 }, frameRate: { max: 30 } } },
    { name: 'to Auto on a larger screen while the picker is open', from: { resolution: '720p', frameRate: 15 }, to: { resolution: 'auto', frameRate: 30 }, captured: { width: 2560, height: 1440, frameRate: 60 }, settled: { resolution: '1080p', frameRate: 30 }, cap: { width: { max: 1920 }, height: { max: 1080 }, frameRate: { max: 30 } } },
    { name: 'from Auto to 720p while the picker is open', from: { resolution: 'auto', frameRate: 30 }, to: { resolution: '720p', frameRate: 15 }, captured: { width: 3840, height: 2160, frameRate: 60 }, settled: { resolution: '720p', frameRate: 15 }, cap: { width: { max: 1280 }, height: { max: 720 }, frameRate: { max: 15 } } },
    { name: 'from Source to 1080p60 while the picker is open', from: { resolution: 'source', frameRate: 30 }, to: { resolution: '1080p', frameRate: 60 }, captured: { width: 3840, height: 2160, frameRate: 60 }, settled: { resolution: '1080p', frameRate: 60 }, cap: { width: { max: 1920 }, height: { max: 1080 }, frameRate: { max: 60 } } },
  ] as const)('picking: $name', async ({ from, to, captured, settled, cap }) => {
    state.captured = captured;
    const room = await connected();
    state.pickGate = deferred();
    const gate = state.pickGate;
    const pick = room.pickScreen(from);
    await Promise.resolve();
    await room.setShareQuality(to);
    gate.resolve(undefined);
    await pick;
    expect(room.settled(to)).toEqual(settled);
    await room.publishScreen();
    const track = state.screenTracks[0];
    expect(track?.mediaStreamTrack.constraints.at(-1)).toEqual(cap);
    expect(state.published[0]?.options).toMatchObject({ screenShareEncoding: { maxFramerate: settled.frameRate } });
  });

  it.each([
    { name: 'to Auto', from: { resolution: '720p', frameRate: 15 }, to: { resolution: 'auto', frameRate: 30 }, settled: { resolution: 'source', frameRate: 30 }, cap: { width: { max: 3840 }, height: { max: 2160 }, frameRate: { max: 30 } } },
    { name: 'from Auto to 720p', from: { resolution: 'auto', frameRate: 30 }, to: { resolution: '720p', frameRate: 15 }, settled: { resolution: '720p', frameRate: 15 }, cap: { width: { max: 1280 }, height: { max: 720 }, frameRate: { max: 15 } } },
  ] as const)('resuming: a kept capture changed $name is settled and capped before it is published', async ({ from, to, settled, cap }) => {
    state.captured = { width: 1920, height: 1080, frameRate: 60 };
    const old = await connected();
    await old.pickScreen(from);
    const kept = old.keepScreen();
    expect(kept).not.toBeNull();
    const next = await connected();
    // The choice is made after the capture was kept, before it is adopted.
    await next.setShareQuality(to);
    if (kept !== null) next.adoptScreen(kept);
    await next.publishScreen();
    expect(next.settled(to)).toEqual(settled);
    expect(state.screenTracks[0]?.mediaStreamTrack.constraints.at(-1)).toEqual(cap);
  });

  it('publishes a Source 4K60 picture at 60 fps and 18 Mb/s with layers of 720p30 and 1080p30', async () => {
    state.captured = { width: 3840, height: 2160, frameRate: 60 };
    const room = await connected();
    await room.pickScreen({ resolution: 'source', frameRate: 60 });
    await room.publishScreen();
    expect(state.published[0]?.options).toEqual({
      source: 'screen_share',
      simulcast: true,
      screenShareEncoding: { maxBitrate: 18_000_000, maxFramerate: 60 },
      screenShareSimulcastLayers: [
        { width: 1280, height: 720, encoding: { maxBitrate: 2_000_000, maxFramerate: 30 } },
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
          screenShareSimulcastLayers: [{ width: 1280, height: 720, encoding: { maxBitrate: 2_000_000, maxFramerate: 30 } }],
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
  it('publishes a kept Source capture at the size it has when the next connection publishes it', async () => {
    const old = await connected();
    await old.pickScreen({ resolution: 'source', frameRate: 60 });
    await old.publishScreen();
    const kept = old.keepScreen();
    await old.close();
    if (kept === null) throw new Error('nothing kept');
    const video = state.screenTracks[0];
    if (video === undefined) throw new Error('no picture captured');
    video.settings = { width: 2560, height: 1440, frameRate: 60 };
    const next = await connected();
    await next.setShareQuality({ resolution: 'source', frameRate: 60 });
    next.adoptScreen(kept);
    state.published.length = 0;
    await next.publishScreen();
    expect(state.published[0]?.options).toEqual({
      source: 'screen_share',
      simulcast: true,
      screenShareEncoding: { maxBitrate: 10_000_000, maxFramerate: 60 },
      screenShareSimulcastLayers: [
        { width: 1280, height: 720, encoding: { maxBitrate: 2_000_000, maxFramerate: 30 } },
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
    expect(state.calls).toEqual(['pick', 'constrain', 'publish screen_share']);
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

// Source keeps the capture's own size and rate up to 4K60. The top layer's
// bitrate follows its longer side; below it are 720p30 at 2 Mbps and 1080p30,
// smallest first, each left out unless it is smaller than the capture.
describe("a share's encodings", () => {
  const sourceEncodings = (captured: { width?: number; height?: number; frameRate?: number }) => shareEncodings(captured, { resolution: 'source', frameRate: 60 });
  const p720: ShareLayer = { width: 1280, height: 720, maxBitrate: 2_000_000, maxFramerate: 30 };
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
    expect(sourceEncodings({ width, height, frameRate: fps })).toEqual({ top: { width, height, maxBitrate: bitrate, maxFramerate: fps }, lower });
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
    expect(sourceEncodings({ width, height, frameRate: 60 })).toEqual({ top: { width, height, maxBitrate: bitrate, maxFramerate: 60 }, lower });
  });

  it('never sends more than 60 fps, nor a lower layer faster than the top', () => {
    expect(sourceEncodings({ width: 3840, height: 2160, frameRate: 120 }).top.maxFramerate).toBe(60);
    expect(sourceEncodings({ width: 3840, height: 2160, frameRate: 24 })).toEqual({
      top: { width: 3840, height: 2160, maxBitrate: 18_000_000, maxFramerate: 24 },
      lower: [{ ...p720, maxFramerate: 24 }, { ...p1080, maxFramerate: 24 }],
    });
    expect(sourceEncodings({ width: 1920, height: 1080, frameRate: 10 }).lower).toEqual([{ ...p720, maxFramerate: 10 }]);
  });

  it('takes a Source capture whose settings do not say its size as 4K, and one without a frame rate as 60 fps', () => {
    const top = { width: 3840, height: 2160, maxBitrate: 18_000_000, maxFramerate: 60 };
    expect(sourceEncodings({})).toEqual({ top, lower: [p720, p1080] });
    expect(sourceEncodings({ width: 0, height: 0, frameRate: Number.NaN })).toEqual({ top, lower: [p720, p1080] });
    expect(sourceEncodings({ width: 3840 })).toEqual({ top, lower: [p720, p1080] });
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
    state.handlers.get('ts')?.(voice('a'), undefined, { identity: 'p8' });
    room.setDeafened(true);
    state.handlers.get('ts')?.(voice('b'), undefined, { identity: 'p9' });
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

describe('the volume of each person', () => {
  // A remote microphone, as the SDK hands it over: each attach makes a new
  // element, and a detach gives back those it made.
  const voice = (sid: string) => {
    const made: HTMLAudioElement[] = [];
    return {
      source: 'microphone',
      kind: 'audio',
      sid,
      attach: () => {
        const el = document.createElement('audio');
        made.push(el);
        return el;
      },
      detach: () => made.splice(0),
    };
  };
  const arrive = (track: ReturnType<typeof voice>, identity: string) => {
    state.handlers.get('ts')?.(track, undefined, { identity });
  };
  const played = () => [...document.body.querySelectorAll('audio')];

  async function connected() {
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    return room;
  }

  it('a volume set before the track arrives is applied when it arrives, 0 included', async () => {
    const room = newLiveKitRoom();
    room.setVolume(8, 0);
    room.setVolume(9, 0.4);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    arrive(voice('a'), 'p8');
    arrive(voice('b'), 'p9');
    arrive(voice('c'), 'p10');
    expect(played().map((el) => el.volume)).toEqual([0, 0.4, 1]);
    await room.close();
  });

  it('0 stays 0 when the track is attached again: the person leaving and coming back', async () => {
    const room = await connected();
    room.setVolume(8, 0);
    const first = voice('a');
    arrive(first, 'p8');
    state.handlers.get('tu')?.(first);
    expect(played()).toHaveLength(0);
    arrive(voice('a2'), 'p8');
    expect(played().map((el) => el.volume)).toEqual([0]);
    await room.close();
  });

  it("a change applies to that person's attached elements alone", async () => {
    const room = await connected();
    arrive(voice('a'), 'p8');
    arrive(voice('b'), 'p9');
    room.setVolume(8, 0.25);
    expect(played().map((el) => el.volume)).toEqual([0.25, 1]);
    room.setVolume(8, 0);
    expect(played().map((el) => el.volume)).toEqual([0, 1]);
    room.setVolume(8, 1);
    expect(played().map((el) => el.volume)).toEqual([1, 1]);
    await room.close();
  });

  it('deafen mutes everyone at every volume, and undeafen leaves each volume as it was', async () => {
    const room = await connected();
    room.setVolume(8, 0);
    room.setVolume(9, 0.4);
    arrive(voice('a'), 'p8');
    arrive(voice('b'), 'p9');
    arrive(voice('c'), 'p10');
    room.setDeafened(true);
    expect(played().map((el) => [el.muted, el.volume])).toEqual([[true, 0], [true, 0.4], [true, 1]]);
    room.setDeafened(false);
    expect(played().map((el) => [el.muted, el.volume])).toEqual([[false, 0], [false, 0.4], [false, 1]]);
    await room.close();
  });

  // The SDK's startAudio unmutes every element it plays; the tap must not
  // undo a deafen.
  it('a tap while deafened leaves every voice muted, and while not deafened plays them', async () => {
    const room = await connected();
    arrive(voice('a'), 'p8');
    arrive(voice('b'), 'p9');
    room.setDeafened(true);
    await room.startAudio();
    expect(played().map((el) => el.muted)).toEqual([true, true]);
    room.setDeafened(false);
    await room.startAudio();
    expect(played().map((el) => el.muted)).toEqual([false, false]);
    await room.close();
  });

  it('the playback event re-mutes the voices while deafened', async () => {
    const room = await connected();
    arrive(voice('a'), 'p8');
    room.setDeafened(true);
    for (const el of played()) el.muted = false;
    state.handlers.get('apc')?.(true);
    expect(played().map((el) => el.muted)).toEqual([true]);
    await room.close();
  });

  it('a volumechange that unmuted a voice while deafened is undone, and left alone otherwise', async () => {
    const room = await connected();
    arrive(voice('a'), 'p8');
    const el = played()[0];
    if (el === undefined) throw new Error('no element');
    room.setDeafened(true);
    el.muted = false;
    el.dispatchEvent(new Event('volumechange'));
    expect(el.muted).toBe(true);
    room.setDeafened(false);
    el.dispatchEvent(new Event('volumechange'));
    expect(el.muted).toBe(false);
    // A closed room's elements no longer hold anything.
    room.setDeafened(true);
    await room.close();
    el.muted = false;
    el.dispatchEvent(new Event('volumechange'));
    expect(el.muted).toBe(false);
  });

  it('holds through a switch of the speakers', async () => {
    const room = await connected();
    room.setVolume(8, 0.4);
    arrive(voice('a'), 'p8');
    await room.switchOutput('out-1');
    arrive(voice('b'), 'p8');
    expect(state.outputs).toEqual(['out-1']);
    expect(played().map((el) => el.volume)).toEqual([0.4, 0.4]);
    await room.close();
  });

  it("never touches a share's sound, which has its own volume", async () => {
    const room = await connected();
    const sink = { video: document.createElement('video'), audio: document.createElement('audio') };
    sink.audio.volume = 0.7;
    room.attachStream(8, sink);
    const sound = { source: 'screen_share_audio', kind: 'audio', sid: 's', attach: (el?: HTMLMediaElement) => el, detach: () => [] };
    room.setVolume(8, 0);
    room.setWatches(new Set([8]), null);
    state.handlers.get('ts')?.(sound, { isSubscribed: true, setSubscribed: () => undefined }, { identity: 'p8' });
    room.setVolume(8, 0.2);
    expect(sink.audio.volume).toBe(0.7);
    expect(played()).toHaveLength(0);
    await room.close();
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

// ---- the camera (ADR-015)

describe('wanted: kind, source and publisher together (S1)', () => {
  const none = { watching: new Set<number>(), focused: null, own: 7, viewing: false, shown: false, claimed: false, voice: true };
  const all = { watching: new Set([8, 9]), focused: 8, own: 7, viewing: true, shown: true, claimed: true, voice: true };
  const { Source, Kind } = Track;
  it.each([
    [Source.Microphone, Kind.Audio, true, true],
    [Source.Microphone, Kind.Video, false, false],
    [Source.Camera, Kind.Video, true, false],
    [Source.Camera, Kind.Audio, false, false],
    [Source.ScreenShare, Kind.Video, true, false],
    [Source.ScreenShare, Kind.Audio, false, false],
    [Source.ScreenShareAudio, Kind.Audio, true, false],
    [Source.ScreenShareAudio, Kind.Video, false, false],
    [Source.Unknown, Kind.Video, false, false],
    [Source.Microphone, undefined, false, false],
  ])('%s as %s: %s while viewing, watching and claimed; %s otherwise', (source, kind, wantedAll, wantedNone) => {
    expect(wanted(source, kind, 8, all)).toBe(wantedAll);
    expect(wanted(source, kind, 8, none)).toBe(wantedNone);
  });

  it('wants both picture and sound of each watched sharer, nothing unwatched, and never this page\'s own stream', () => {
    for (const source of [Source.ScreenShare, Source.ScreenShareAudio]) {
      const kind = source === Source.ScreenShare ? Kind.Video : Kind.Audio;
      expect(wanted(source, kind, 8, all)).toBe(true);
      expect(wanted(source, kind, 9, all)).toBe(true);
      expect(wanted(source, kind, 10, all)).toBe(false);
      expect(wanted(source, kind, 7, all)).toBe(false);
    }
  });

  it('no camera is wanted while the video is not shown here, even with the call view open; microphones and watched shares are unaffected', () => {
    const away = { ...all, shown: false };
    expect(wanted(Source.Camera, Kind.Video, 8, away)).toBe(false);
    expect(wanted(Source.Microphone, Kind.Audio, 8, away)).toBe(true);
    expect(wanted(Source.ScreenShare, Kind.Video, 8, away)).toBe(true);
    expect(wanted(Source.ScreenShareAudio, Kind.Audio, 8, away)).toBe(true);
  });

  it('a camera is wanted only while the call view is shown, and only from someone holding a claim', () => {
    expect(wanted(Source.Camera, Kind.Video, 8, { ...all, watching: new Set() })).toBe(true);
    expect(wanted(Source.Camera, Kind.Video, 8, { ...all, viewing: false })).toBe(false);
    expect(wanted(Source.Camera, Kind.Video, 8, { ...all, claimed: false })).toBe(false);
  });
});

describe('wanted: the screen companion takes no voice (issue #1581)', () => {
  const base = { watching: new Set([8, 9]), focused: 8, own: 7, viewing: true, shown: true, claimed: true };
  const { Source, Kind } = Track;
  // source x kind x publisher x voice. A microphone is wanted only with voice;
  // a share's picture and sound only from a watched publisher other than the
  // person's own; a camera only with voice too (the companion takes none).
  it.each([
    [Source.Microphone, Kind.Audio, 8, true, true],
    [Source.Microphone, Kind.Audio, 8, false, false],
    [Source.Microphone, Kind.Audio, 10, true, true],
    [Source.Microphone, Kind.Audio, 10, false, false],
    [Source.Microphone, Kind.Audio, null, false, false],
    [Source.Microphone, Kind.Video, 8, true, false],
    [Source.Microphone, Kind.Video, 8, false, false],
    [Source.ScreenShare, Kind.Video, 8, true, true],
    [Source.ScreenShare, Kind.Video, 8, false, true],
    [Source.ScreenShare, Kind.Video, 10, false, false],
    [Source.ScreenShare, Kind.Video, 7, false, false],
    [Source.ScreenShareAudio, Kind.Audio, 9, true, true],
    [Source.ScreenShareAudio, Kind.Audio, 9, false, true],
    [Source.ScreenShareAudio, Kind.Audio, 7, false, false],
    [Source.ScreenShareAudio, Kind.Video, 9, false, false],
    [Source.Camera, Kind.Video, 8, true, true],
    // Changed in the security-review fixes (#1622 S4b): the companion
    // subscribes to screen shares only, never to a camera (voice is false for
    // it alone), where this row first said true.
    [Source.Camera, Kind.Video, 8, false, false],
    [Source.Unknown, Kind.Audio, 8, false, false],
  ])('%s as %s from %s with voice %s: %s', (source, kind, principal, voice, expected) => {
    expect(wanted(source, kind, principal, { ...base, voice })).toBe(expected);
  });
});

describe('the screen companion room (issue #1581)', () => {
  const remotePub = (source: string, kind: string): StubRemotePub => {
    const p: StubRemotePub = {
      source,
      kind,
      isSubscribed: false,
      setSubscribed: (on) => {
        p.isSubscribed = on;
      },
      setVideoQuality: () => undefined,
    };
    return p;
  };

  it('subscribes to no microphone, however it arrives, and to a watched share alone', async () => {
    const mic = remotePub('microphone', 'audio');
    const share = remotePub('screen_share', 'video');
    const sound = remotePub('screen_share_audio', 'audio');
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', mic], ['b', share], ['c', sound]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect([mic.isSubscribed, share.isSubscribed, sound.isSubscribed]).toEqual([false, false, false]);
    room.setWatches(new Set([8]), null);
    expect([mic.isSubscribed, share.isSubscribed, sound.isSubscribed]).toEqual([false, true, true]);
    room.setWatches(new Set(), null);
    expect([mic.isSubscribed, share.isSubscribed, sound.isSubscribed]).toEqual([false, false, false]);
  });

  it('a browser room still subscribes to microphones', async () => {
    const mic = remotePub('microphone', 'audio');
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', mic]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    expect(mic.isSubscribed).toBe(true);
  });

  it('asks the picker for no sound at all, keeps only the picture, and publishes it alone', async () => {
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    state.calls.length = 0;
    expect(await room.pickScreen()).toEqual({ audio: false });
    expect(state.screenOptions).toMatchObject({ video: true, audio: false, systemAudio: 'exclude', selfBrowserSurface: 'exclude' });
    // The audio track the stub gave is stopped, never held.
    expect(state.screenTracks.map((t) => t.source)).toEqual(['screen_share', 'screen_share_audio']);
    await room.publishScreen();
    expect(state.published.map((p) => p.source)).toEqual(['screen_share']);
  });

  // Issue #1772: the companion captures and publishes through the same rows as
  // the browser (shareAsk, shareTops, contentHint), so each of the desktop's
  // choices reaches the picker's constraints, the capture's caps and the
  // encodings: the top layer is never larger than asked and its fps is the
  // chosen one. The capture here is a 4K60 screen, larger than any ask.
  it.each([
    { name: '720p30', q: { resolution: '720p', frameRate: 30 }, ask: { width: 1280, height: 720, frameRate: 30 }, top: { maxBitrate: 6_000_000, maxFramerate: 30 } },
    { name: '1080p30', q: { resolution: '1080p', frameRate: 30 }, ask: { width: 1920, height: 1080, frameRate: 30 }, top: { maxBitrate: 6_000_000, maxFramerate: 30 } },
    { name: '1080p60', q: { resolution: '1080p', frameRate: 60 }, ask: { width: 1920, height: 1080, frameRate: 60 }, top: { maxBitrate: 6_000_000, maxFramerate: 60 } },
    { name: 'Source 30', q: { resolution: 'source', frameRate: 30 }, ask: { width: 3840, height: 2160, frameRate: 30 }, top: { maxBitrate: 18_000_000, maxFramerate: 30 } },
    { name: 'Source 60', q: { resolution: 'source', frameRate: 60 }, ask: { width: 3840, height: 2160, frameRate: 60 }, top: { maxBitrate: 18_000_000, maxFramerate: 60 } },
  ] as const)('the companion captures and publishes $name as asked', async ({ q, ask, top }) => {
    expect(companionQualities).toContainEqual(q);
    state.captured = { width: 3840, height: 2160, frameRate: 60 };
    state.ignoreConstraints = false;
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    await room.pickScreen(q);
    expect(state.screenOptions).toMatchObject({ video: true, audio: false, resolution: ask, contentHint: 'motion' });
    await room.publishScreen();
    const track = state.screenTracks.find((t) => t.source === 'screen_share');
    expect(track?.mediaStreamTrack.constraints.at(-1)).toEqual({ width: { max: ask.width }, height: { max: ask.height }, frameRate: { max: ask.frameRate } });
    expect(track?.mediaStreamTrack.contentHint).toBe('motion');
    expect(state.published.map((p) => p.source)).toEqual(['screen_share']);
    expect(state.published[0]?.options).toMatchObject({ screenShareEncoding: top });
    const topEncoding = track?.sender?.parameters.encodings.at(-1);
    expect(topEncoding).toMatchObject({ maxBitrate: top.maxBitrate, maxFramerate: top.maxFramerate });
    // Never larger than asked: the capture is scaled down to the ask.
    expect(track?.mediaStreamTrack.getSettings().width).toBeLessThanOrEqual(ask.width);
    expect(track?.mediaStreamTrack.getSettings().height).toBeLessThanOrEqual(ask.height);
  });

  it('never captures or publishes a microphone', async () => {
    const room = newLiveKitRoom();
    room.setCompanion?.(true);
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    await expect(room.publishMicrophone()).rejects.toThrow('no microphone');
    await expect(room.switchMicrophone(null)).rejects.toThrow('no microphone');
    expect(state.calls).not.toContain('microphone');
  });

  it('captureOptions asks for sound by default and for none when told not to', () => {
    expect(captureOptions()).toMatchObject({ audio: { channelCount: 2 }, systemAudio: 'include' });
    expect(captureOptions(undefined, false)).toMatchObject({ audio: false, systemAudio: 'exclude' });
  });
});

describe('receiving cameras', () => {
  const remotePub = (source: string, kind: string): StubRemotePub => {
    const p: StubRemotePub = {
      source,
      kind,
      isSubscribed: false,
      setSubscribed: (on) => {
        p.isSubscribed = on;
      },
      setVideoQuality: () => undefined,
    };
    return p;
  };

  it('subscribes a camera only while viewing, only from a claim holder, only as video, and lets it go when the view closes', async () => {
    const camera = remotePub('camera', 'video');
    const soundAsCamera = remotePub('camera', 'audio');
    const videoAsMic = remotePub('microphone', 'video');
    const mic = remotePub('microphone', 'audio');
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', camera], ['b', soundAsCamera], ['c', videoAsMic], ['d', mic]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    const subscribed = () => [camera.isSubscribed, soundAsCamera.isSubscribed, videoAsMic.isSubscribed, mic.isSubscribed];
    expect(subscribed()).toEqual([false, false, false, true]);
    room.setViewing(true);
    // Nobody holds a claim yet.
    expect(subscribed()).toEqual([false, false, false, true]);
    room.setCameras(new Set([8]));
    expect(subscribed()).toEqual([true, false, false, true]);
    room.setViewing(false);
    expect(subscribed()).toEqual([false, false, false, true]);
    room.setViewing(true);
    room.setCameras(new Set());
    expect(subscribed()).toEqual([false, false, false, true]);
  });

  it('lets every camera go while the video is not shown here, and keeps the choice to view: the cameras are back with the view, with no other call', async () => {
    const camera = remotePub('camera', 'video');
    const mic = remotePub('microphone', 'audio');
    const share = remotePub('screen_share', 'video');
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', camera], ['b', mic], ['c', share]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    room.setWatches(new Set([8]), null);
    room.setViewing(true);
    room.setCameras(new Set([8]));
    const subscribed = () => [camera.isSubscribed, mic.isSubscribed, share.isSubscribed];
    expect(subscribed()).toEqual([true, true, true]);
    room.setShown(false);
    expect(subscribed()).toEqual([false, true, true]);
    // Presence and the view's own choice change nothing while it is hidden.
    room.setCameras(new Set([8, 9]));
    room.setViewing(true);
    expect(subscribed()).toEqual([false, true, true]);
    room.setShown(true);
    expect(subscribed()).toEqual([true, true, true]);
  });

  it('a camera that arrives while the video is not shown is let go, never played', async () => {
    const told: (readonly number[])[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onCameraTracks: (p) => told.push(p) });
    room.setViewing(true);
    room.setCameras(new Set([8]));
    room.setShown(false);
    const attached: string[] = [];
    const pub = remotePub('camera', 'video');
    pub.isSubscribed = true;
    state.handlers.get('ts')?.(
      { source: 'camera', kind: 'video', sid: 'c', attach: () => attached.push('camera') && undefined, detach: () => [] },
      pub,
      { identity: 'p8' },
    );
    expect(attached).toEqual([]);
    expect(pub.isSubscribed).toBe(false);
    expect(told).toEqual([]);
  });

  it('a track that arrives with a kind its source disagrees with is let go, never played or shown', async () => {
    const told: (readonly number[])[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onCameraTracks: (p) => told.push(p) });
    room.setViewing(true);
    room.setCameras(new Set([8]));
    const attached: string[] = [];
    const track = (source: string, kind: string) => ({
      source,
      kind,
      sid: `${source}-${kind}`,
      attach: (el?: HTMLMediaElement) => {
        attached.push(`${source}/${kind}`);
        return el ?? document.createElement('audio');
      },
      detach: () => [],
    });
    const pubs = [remotePub('microphone', 'video'), remotePub('camera', 'audio')];
    state.handlers.get('ts')?.(track('microphone', 'video'), pubs[0], { identity: 'p8' });
    state.handlers.get('ts')?.(track('camera', 'audio'), pubs[1], { identity: 'p8' });
    expect(attached).toEqual([]);
    expect(document.body.querySelectorAll('audio, video')).toHaveLength(0);
    expect(told).toEqual([]);
    // A camera as video, from a claim holder, is kept and played in its tile.
    const tile = document.createElement('video');
    room.attachCamera(8, tile);
    state.handlers.get('ts')?.(track('camera', 'video'), remotePub('camera', 'video'), { identity: 'p8' });
    expect(attached).toEqual(['camera/video']);
    expect(told).toEqual([[8]]);
    // From someone without a claim: let go.
    const stray = remotePub('camera', 'video');
    stray.isSubscribed = true;
    state.handlers.get('ts')?.(track('camera', 'video'), stray, { identity: 'p9' });
    expect(stray.isSubscribed).toBe(false);
    expect(told).toEqual([[8]]);
  });

  it('a track refused for its kind is never subscribed again, however often presence comes', async () => {
    // Published as a camera picture, it arrives as sound.
    const pub = { ...remotePub('camera', 'video'), trackSid: 'TR_smuggled' };
    pub.setSubscribed = (on) => {
      pub.isSubscribed = on;
      pub.isDesired = on;
    };
    state.remote.set('p8', { identity: 'p8', trackPublications: new Map([['a', pub]]) } as unknown as { trackPublications: Map<string, StubRemotePub> });
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, events);
    room.setViewing(true);
    room.setCameras(new Set([8]));
    expect(pub.isDesired).toBe(true);
    state.handlers.get('ts')?.({ source: 'camera', kind: 'audio', sid: 'x', attach: () => undefined, detach: () => [] }, pub, { identity: 'p8' });
    expect(pub.isDesired).toBe(false);
    room.setCameras(new Set([8]));
    room.setViewing(true);
    expect(pub.isDesired).toBe(false);
  });
});

describe('the camera', () => {
  let ended: string[] = [];
  let changed = 0;
  async function connected(): Promise<Room> {
    ended = [];
    changed = 0;
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, {
      ...events,
      onCameraEnded: (cause) => ended.push(cause),
      onCameraChanged: () => {
        changed++;
      },
    });
    return room;
  }

  it('asks for the camera chosen exactly, at 1280x720 and 30 fps as an ideal, or the browser own choice', () => {
    expect(cameraOptions('cam-1')).toEqual({ resolution: { width: 1280, height: 720, frameRate: 30 }, deviceId: { exact: 'cam-1' } });
    expect(cameraOptions(null)).toEqual({ resolution: { width: 1280, height: 720, frameRate: 30 } });
  });

  // Design §7.5: the profile is chosen as the capture starts. facingMode is
  // present only for a phone with no camera chosen, and exact only with one.
  it.each([
    ['desktop', null, { resolution: { width: 1280, height: 720, frameRate: 30 } }],
    ['desktop', 'cam-1', { resolution: { width: 1280, height: 720, frameRate: 30 }, deviceId: { exact: 'cam-1' } }],
    ['phone', null, { resolution: { width: 640, height: 360, frameRate: 24 }, facingMode: 'user' }],
    ['phone', 'cam-1', { resolution: { width: 640, height: 360, frameRate: 24 }, deviceId: { exact: 'cam-1' } }],
  ] as const)('%s capture with %s chosen asks %j', (profile, choice, expected) => {
    const options = cameraOptions(choice, profile);
    expect(options).toEqual(expected);
    expect('facingMode' in options).toBe(profile === 'phone' && choice === null);
    expect('deviceId' in options).toBe(choice !== null);
  });

  it('publishes a phone camera as VP8, a 450 kbps top layer of 360p and one 180p layer at 150 kbps', () => {
    const options = cameraPublishOptions('phone');
    expect(options).toMatchObject({ source: 'camera', simulcast: true, videoCodec: 'vp8', videoEncoding: { maxBitrate: 450_000, maxFramerate: 24 } });
    expect(options.videoSimulcastLayers?.map((l) => [l.width, l.height, l.encoding.maxBitrate])).toEqual([[320, 180, 150_000]]);
    // The desktop is as it was: no codec of its own, two layers.
    expect(cameraPublishOptions('desktop')).toEqual(cameraPublishOptions());
    expect(cameraPublishOptions().videoCodec).toBeUndefined();
  });

  it('a phone capture keeps its profile for its publish and its switches', async () => {
    const room = await connected();
    await room.captureCamera(null, 'phone');
    expect(state.videoOptions.at(-1)).toEqual({ resolution: { width: 640, height: 360, frameRate: 24 }, facingMode: 'user' });
    state.permissions = { canPublish: true, canPublishSources: [1] };
    await room.publishCamera();
    expect(state.published.at(-1)?.options).toMatchObject({ videoCodec: 'vp8', videoEncoding: { maxBitrate: 450_000 } });
    await room.switchCamera('cam-2');
    expect(state.videoOptions.at(-1)).toEqual({ resolution: { width: 640, height: 360, frameRate: 24 }, deviceId: { exact: 'cam-2' } });
    // A desktop capture after it is the desktop's.
    await room.captureCamera(null);
    expect(state.videoOptions.at(-1)).toEqual({ resolution: { width: 1280, height: 720, frameRate: 30 } });
  });

  it('publishes as the camera, with simulcast layers of 360p and 180p under 1.2 Mbps', () => {
    const options = cameraPublishOptions();
    expect(options).toMatchObject({ source: 'camera', simulcast: true, videoEncoding: { maxBitrate: 1_200_000, maxFramerate: 30 } });
    expect(options.videoSimulcastLayers?.map((l) => [l.width, l.height, l.encoding.maxBitrate])).toEqual([
      [320, 180, 150_000],
      [640, 360, 450_000],
    ]);
  });

  it('captures as a track the page provides, and publishes only once LiveKit has granted the camera', async () => {
    const room = await connected();
    await room.captureCamera('cam-1');
    expect(state.videoTracks.at(-1)?.userProvided).toBe(true);
    expect(state.videoTracks.at(-1)?.source).toBe('camera');
    expect(room.cameraTrack()).toBe('captured');
    state.permissions = { canPublish: true, canPublishSources: [2] };
    let granted = false;
    const waiting = room.awaitCameraGrant().then(() => {
      granted = true;
    });
    await settle();
    expect(granted).toBe(false);
    // LiveKit says the claim granted the camera (TrackSource.CAMERA is 1).
    state.permissions = { canPublish: true, canPublishSources: [2, 1] };
    state.handlers.get('ppc')?.(undefined, state.room?.localParticipant);
    await waiting;
    expect(granted).toBe(true);
    await room.publishCamera();
    expect(state.published.at(-1)?.source).toBe('camera');
    expect(room.cameraTrack()).toBe('published');
    expect(changed).toBeGreaterThan(0);
  });

  it('a grant that does not come in grantWait fails the wait', async () => {
    vi.useFakeTimers();
    try {
      const room = await connected();
      await room.captureCamera(null);
      state.permissions = { canPublish: true, canPublishSources: [2] };
      const waiting = room.awaitCameraGrant().then(
        () => 'granted',
        () => 'refused',
      );
      await vi.advanceTimersByTimeAsync(grantWait + 10);
      expect(await waiting).toBe('refused');
    } finally {
      vi.useRealTimers();
    }
  });

  it('stopping stops the capture at once, before the unpublish answers, and is never reported as the server', async () => {
    const room = await connected();
    await room.captureCamera(null);
    state.permissions = { canPublish: true, canPublishSources: [] };
    await room.awaitCameraGrant();
    await room.publishCamera();
    state.unpublishHangs = true;
    state.calls.length = 0;
    void room.stopCamera();
    expect(state.calls).toEqual(['stop camera track', 'unpublish camera']);
    expect(state.videoTracks.at(-1)?.mediaStreamTrack.readyState).toBe('ended');
    expect(room.cameraTrack()).toBe('none');
    expect(ended).toEqual([]);
  });

  it('a device that ends the capture is told as the device, and nothing is captured again', async () => {
    const room = await connected();
    await room.captureCamera('cam-1');
    state.calls.length = 0;
    state.videoTracks.at(-1)?.mediaStreamTrack.ended();
    expect(ended).toEqual(['device']);
    expect(state.calls).not.toContain('capture camera');
  });

  it('LiveKit taking the published camera away is told as the server, once the event is over', async () => {
    const room = await connected();
    await room.captureCamera(null);
    state.permissions = { canPublish: true, canPublishSources: [] };
    await room.publishCamera();
    state.handlers.get('ltu')?.({ source: 'camera', track: state.videoTracks.at(-1) });
    expect(room.cameraTrack()).toBe('captured');
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(ended).toEqual(['server']);
  });

  it('the unpublish the SDK makes as the server ends the connection is not told as the server: the end is (S5)', async () => {
    const room = await connected();
    await room.captureCamera(null);
    state.permissions = { canPublish: true, canPublishSources: [] };
    await room.publishCamera();
    // As the SDK's handleDisconnect: every track unpublished, then the room
    // disconnected.
    state.handlers.get('ltu')?.({ source: 'camera', track: state.videoTracks.at(-1) });
    if (state.room !== null) (state.room as { state?: string }).state = 'disconnected';
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(ended).toEqual([]);
  });

  it('a capture kept for the next connection is blanked, not stopped, and the next one unblanks it', async () => {
    const room = await connected();
    await room.captureCamera(null);
    const track = state.videoTracks.at(-1);
    const kept = room.keepCamera();
    expect(kept?.live()).toBe(true);
    expect(track?.mediaStreamTrack.enabled).toBe(false);
    expect(track?.stopped).toBe(0);
    expect(room.cameraTrack()).toBe('none');
    const next = await connected();
    if (kept === null) throw new Error('nothing kept');
    next.adoptCamera(kept);
    expect(track?.mediaStreamTrack.enabled).toBe(true);
    expect(next.cameraTrack()).toBe('captured');
  });

  it('a Leave stops the camera, and a capture that completes after it is stopped', async () => {
    const room = await connected();
    await room.captureCamera(null);
    const first = state.videoTracks.at(-1);
    await room.close();
    expect(first?.stopped).toBe(1);
    const late = await connected();
    const capture = late.captureCamera(null);
    void late.close();
    await expect(capture).rejects.toThrow();
    expect(state.videoTracks.at(-1)?.stopped).toBe(1);
  });

  it.each([
    ['in reverse order', [1, 0]],
    ['in order', [0, 1]],
  ] as const)('on, off and on while the first capture is asked for, the captures answered %s: one capture lives, the newer, and the other is stopped (S5)', async (_order, answering) => {
    const room = await connected();
    const holds = [deferred<undefined>(), deferred<undefined>()];
    state.videoHolds.push(...holds);
    const first = room.captureCamera('cam-1').then(
      () => 'held',
      () => 'taken',
    );
    await settle();
    await room.stopCamera();
    const second = room.captureCamera('cam-1').then(
      () => 'held',
      () => 'taken',
    );
    await settle();
    for (const i of answering) {
      holds[i]?.resolve(undefined);
      await settle();
    }
    expect([await first, await second]).toEqual(['taken', 'held']);
    // Each capture is the browser's track and the page's wrapper of it: one
    // device capture each.
    const captures = () => new Set(state.videoTracks.map((t) => t.mediaStreamTrack));
    expect(captures().size).toBe(2);
    expect([...captures()].filter((m) => m.readyState === 'live')).toHaveLength(1);
    expect(room.cameraTrack()).toBe('captured');
    // Stopping it leaves nothing captured.
    await room.stopCamera();
    expect([...captures()].filter((m) => m.readyState === 'live')).toHaveLength(0);
  });

  it('a failed switch after the grant follows a choice changed back and publishes a live track', async () => {
    const room = await connected();
    state.permissions = { canPublish: true, canPublishSources: [2] };
    const choice = { id: 'cam-1' };
    const releases: string[] = [];
    const client = new CameraClient(
      {
        claim: () => Promise.resolve({ ok: true, value: { principal_id: 7, started_at: '2026-11-03T10:00:00.000Z' } }),
        release: (_channel, startedAt) => {
          releases.push(startedAt);
          return Promise.resolve();
        },
      },
      () => undefined,
      new FakeClock(),
      () => choice.id,
    );
    const to2 = deferred<undefined>();
    const to3 = deferred<undefined>();
    const backTo2 = deferred<undefined>();
    state.videoRestartHolds.push(to2, to3, backTo2);
    const starting = client.start('general', room, 'cam-1');
    await settle();
    expect(client.current).toEqual({ kind: 'granted-waiting' });
    choice.id = 'cam-2';
    state.permissions = { canPublish: true, canPublishSources: [] };
    state.handlers.get('ppc')?.(undefined, state.room?.localParticipant);
    await settle();
    expect(state.videoOptions.at(-1)).toMatchObject({ deviceId: { exact: 'cam-2' } });
    to2.resolve(undefined);
    choice.id = 'cam-3';
    await settle();
    expect(state.videoOptions.at(-1)).toMatchObject({ deviceId: { exact: 'cam-3' } });
    choice.id = 'cam-2';
    to3.reject(new DOMException('gone', 'OverconstrainedError'));
    await settle();
    expect(state.videoOptions.at(-1)).toMatchObject({ deviceId: { exact: 'cam-2' } });
    expect(client.current).toEqual({ kind: 'granted-waiting' });
    backTo2.resolve(undefined);
    await starting;
    expect(client.current).toEqual({ kind: 'sending' });
    expect(room.cameraInUse()).toBe('cam-2');
    expect(room.cameraTrack()).toBe('published');
    expect(state.videoTracks.at(-1)?.mediaStreamTrack.readyState).toBe('live');
    expect(state.published.map((p) => p.source)).toEqual(['camera']);
    await client.stop('self');
    expect(room.cameraTrack()).toBe('none');
    expect(state.videoTracks.at(-1)?.mediaStreamTrack.readyState).toBe('ended');
    expect(releases).toEqual(['2026-11-03T10:00:00.000Z']);
  });

  it('a failed chooseCamera while sending unpublishes the camera and releases its claim', async () => {
    state.permissions = { canPublish: true, canPublishSources: [] };
    const session: VoiceSessionResult = {
      ok: true,
      value: { livekit_url: 'wss://lk.example', identity: 'p7', rooms: [{ room: 'r', token: 't', can_publish: true, expires_at: '2026-11-03T10:00:15.000Z' }] },
    };
    const releases: string[] = [];
    const client = new VoiceClient({
      clock: new FakeClock(),
      random: () => 0.5,
      createSession: () => Promise.resolve(session),
      report: () => Promise.resolve('ok'),
      newRoom: newLiveKitRoom,
      claimShare: () => Promise.resolve({ ok: false, failure: 'network' }),
      stopShare: () => Promise.resolve(),
      claimCamera: () => Promise.resolve({ ok: true, value: { principal_id: 7, started_at: '2026-11-03T10:00:00.000Z' } }),
      stopCamera: (_channel, startedAt) => {
        releases.push(startedAt);
        return Promise.resolve();
      },
    });
    client.join('general');
    await settle();
    await settle();
    await client.startCamera();
    await settle();
    expect(client.snapshot()).toMatchObject({ camera: { kind: 'sending' }, cameraTrack: 'published' });
    const failed = deferred<undefined>();
    state.videoRestartHolds.push(failed);
    const choosing = client.chooseCamera({ id: 'cam-2', label: 'Laptop camera' });
    await settle();
    failed.reject(new DOMException('gone', 'OverconstrainedError'));
    await choosing;
    await settle();
    expect(client.snapshot()).toMatchObject({ camera: { kind: 'failed', reason: 'not_connected' }, cameraTrack: 'none' });
    expect(state.published).toEqual([]);
    expect(releases).toEqual(['2026-11-03T10:00:00.000Z']);
    await client.leave();
  });

  it('a switch overtaken by a later one never stops the capture the later one made', async () => {
    const room = await connected();
    await room.captureCamera('cam-a');
    const toB = deferred<undefined>();
    const toC = deferred<undefined>();
    state.videoRestartHolds.push(toB, toC);
    const b = room.switchCamera('cam-b').then(
      () => 'switched',
      () => 'failed',
    );
    // B's restart is under way when C is asked for.
    await settle();
    const c = room.switchCamera('cam-c').then(
      () => 'switched',
      () => 'failed',
    );
    await settle();
    // C's camera is had first; B's fails after it.
    toC.resolve(undefined);
    await settle();
    toB.reject(new DOMException('gone', 'OverconstrainedError'));
    expect([await b, await c]).toEqual(['failed', 'switched']);
    expect(room.cameraTrack()).toBe('captured');
    expect(room.cameraInUse()).toBe('cam-c');
  });

  it('a camera unplugged after a switch is still told as the device', async () => {
    const room = await connected();
    await room.captureCamera('cam-a');
    await room.switchCamera('cam-b');
    expect(room.cameraInUse()).toBe('cam-b');
    const wrapper = state.videoTracks.find((t) => t.userProvided);
    wrapper?.mediaStreamTrack.ended();
    expect(ended).toEqual(['device']);
  });

  it('a switch overtaken by a stop leaves nothing captured, and does not fail the stop', async () => {
    const room = await connected();
    await room.captureCamera('cam-a');
    const toB = deferred<undefined>();
    state.videoRestartHolds.push(toB);
    const b = room.switchCamera('cam-b').then(
      () => 'switched',
      () => 'failed',
    );
    await settle();
    await room.stopCamera();
    toB.resolve(undefined);
    expect(await b).toBe('failed');
    const wrapper = state.videoTracks.find((t) => t.userProvided);
    expect(wrapper?.mediaStreamTrack.readyState).toBe('ended');
    expect(room.cameraTrack()).toBe('none');
  });

  it('a switch overtaken by a keep never stops the capture kept for, and adopted by, the next connection', async () => {
    const room = await connected();
    await room.captureCamera('cam-a');
    const toB = deferred<undefined>();
    state.videoRestartHolds.push(toB);
    const b = room.switchCamera('cam-b').then(
      () => 'switched',
      () => 'failed',
    );
    await settle();
    const kept = room.keepCamera();
    if (kept === null) throw new Error('nothing kept');
    const next = await connected();
    next.adoptCamera(kept);
    toB.reject(new DOMException('gone', 'OverconstrainedError'));
    expect(await b).toBe('failed');
    expect(next.cameraTrack()).toBe('captured');
    const wrapper = state.videoTracks.find((t) => t.userProvided);
    expect(wrapper?.stopped).toBe(0);
  });

  it('a capture that completes after a stop is stopped, never held', async () => {
    const room = await connected();
    const hold = deferred<undefined>();
    state.videoHolds.push(hold);
    const capture = room.captureCamera(null).then(
      () => 'held',
      () => 'taken',
    );
    await settle();
    await room.stopCamera();
    hold.resolve(undefined);
    expect(await capture).toBe('taken');
    expect(state.videoTracks.at(-1)?.mediaStreamTrack.readyState).toBe('ended');
    expect(room.cameraTrack()).toBe('none');
  });

  it('plays the self-view from the local track', async () => {
    const room = await connected();
    const self = document.createElement('video');
    room.attachSelf(self);
    await room.captureCamera(null);
    expect(state.videoTracks.at(-1)?.attached).toEqual([self]);
  });
});

// Issue #1794: the meter listens with the audio context the client made in
// the Join gesture, because iOS leaves one made later suspended; and it
// reports and reads the meter only while that context runs.
describe('the meter\'s audio context', () => {
  const connect = (room: Room, audio: StubAudio | null, on: Partial<RoomEvents> = {}) =>
    room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', true, { ...events, ...on }, audio?.asContext() ?? null);

  it('listens with the context it was given, and makes none of its own', async () => {
    const given = new StubAudio();
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    expect(state.contexts).toHaveLength(0);
    expect(given.sources).toBe(1);
    expect(meter).toEqual([true]);
    // The room never closes what it does not own, and lets go of what it added.
    await room.close();
    expect(given.closed).toBe(0);
    expect(given.listeners.size).toBe(0);
    expect(given.disconnected).toBeGreaterThan(0);
  });

  it('a mic switch taps the same context again, and makes no new one', async () => {
    const given = new StubAudio();
    const room = newLiveKitRoom();
    await connect(room, given, {});
    await room.switchMicrophone('mic-2');
    await room.switchMicrophone('mic-3');
    expect(state.contexts).toHaveLength(0);
    expect(state.audioContexts).toBe(0);
    expect(given.sources).toBe(3);
    expect(given.closed).toBe(0);
    // One listener, the newest tap's.
    expect(given.listeners.size).toBe(1);
    await room.close();
  });

  it('a given context that is already closed is not used: the room makes its own, as before', async () => {
    const given = new StubAudio();
    given.state = 'closed';
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    expect(given.sources).toBe(0);
    expect(state.contexts).toHaveLength(1);
    expect(state.contexts[0]?.sources).toBe(1);
    expect(meter).toEqual([true]);
    await room.close();
    // What the room made, it closes.
    expect(state.contexts[0]?.closed).toBe(1);
    expect(given.closed).toBe(0);
  });

  it('with none given the room makes its own, and if the browser refuses it the meter is gone (#1795)', async () => {
    vi.stubGlobal('AudioContext', function AudioContext() {
      throw new DOMException('refused', 'NotSupportedError');
    });
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, null, { onMeter: (on: boolean) => meter.push(on) });
    expect(meter).toEqual([false]);
    await room.close();
  });

  it('a given context whose source cannot be made is left to the client to close, and says there is no meter', async () => {
    const given = new StubAudio();
    given.createMediaStreamSource = () => {
      throw new DOMException('no source', 'InvalidStateError');
    };
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    expect(meter).toEqual([false]);
    expect(given.closed).toBe(0);
    await room.close();
  });

  it('level() is 0 while the context is suspended or interrupted, whatever the analyser last held', async () => {
    const given = new StubAudio();
    given.sample = 255;
    const room = newLiveKitRoom();
    await connect(room, given, {});
    expect(room.level()).toBeGreaterThan(0.5);
    // The page would resume it; here the browser does not.
    given.onResume = 'stay';
    given.become('suspended');
    expect(room.level()).toBe(0);
    given.become('interrupted');
    expect(room.level()).toBe(0);
    given.become('running');
    expect(room.level()).toBeGreaterThan(0.5);
    await room.close();
  });

  it('a state change tells the client: running is a working meter, anything else is not', async () => {
    const given = new StubAudio();
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    expect(meter).toEqual([true]);
    given.become('suspended');
    given.become('running');
    given.become('interrupted');
    expect(meter).toEqual([true, false, true, false]);
    // After the room is closed nothing is told.
    await room.close();
    given.become('running');
    expect(meter).toEqual([true, false, true, false]);
  });

  it('a switch drops the old tap\'s listener: one state change tells the client once', async () => {
    const given = new StubAudio();
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    await room.switchMicrophone('mic-2');
    meter.length = 0;
    given.onResume = 'stay';
    given.become('suspended');
    expect(meter).toEqual([false]);
    await room.close();
  });

  it('does not say the meter works before the context runs', async () => {
    const given = new StubAudio();
    given.state = 'suspended';
    given.onResume = 'stay';
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    expect(given.resumed).toBe(1);
    await settle();
    // A context that never runs is not a working meter: silently, with no
    // name, the client is told there is none.
    expect(meter.length).toBeGreaterThan(0);
    expect(meter.every((on) => !on)).toBe(true);
    expect(room.level()).toBe(0);
    given.become('running');
    expect(meter.at(-1)).toBe(true);
    await room.close();
  });

  it('a context that stays pending or comes back not running is no meter, and records no name', async () => {
    const given = new StubAudio();
    given.state = 'suspended';
    given.onResume = 'stay';
    const names: (string | undefined)[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (_on: boolean, name?: string) => names.push(name) });
    await settle();
    expect(names.every((n) => n === undefined)).toBe(true);
    await room.close();
  });

  it('a context that the room resumes is said working once it runs, and never before', async () => {
    const given = new StubAudio();
    given.state = 'suspended';
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    await settle();
    // Not working until it runs, then working, and it ends working.
    expect(meter[0]).toBe(false);
    expect(meter.at(-1)).toBe(true);
    await room.close();
  });

  it('a resume the browser refuses says there is no meter', async () => {
    const given = new StubAudio();
    given.state = 'suspended';
    given.onResume = 'refuse';
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await connect(room, given, { onMeter: (on: boolean) => meter.push(on) });
    await settle();
    expect(meter.length).toBeGreaterThan(0);
    expect(meter.every((on) => !on)).toBe(true);
    await room.close();
  });

  // Issue #1794 review: nothing else brings back a context an interruption
  // suspended, so the page resumes it when it is visible.
  describe('after an interruption', () => {
    const visible = (v: 'visible' | 'hidden'): void => {
      Object.defineProperty(document, 'visibilityState', { value: v, configurable: true });
    };
    afterEach(() => {
      Reflect.deleteProperty(document, 'visibilityState');
    });

    it('a context that suspends while the page is visible is resumed', async () => {
      visible('visible');
      const given = new StubAudio();
      const room = newLiveKitRoom();
      await connect(room, given, {});
      given.onResume = 'stay';
      given.become('suspended');
      expect(given.resumed).toBe(1);
      await room.close();
    });

    it('a context that suspends while the page is hidden waits for it to be visible', async () => {
      visible('hidden');
      const given = new StubAudio();
      const room = newLiveKitRoom();
      await connect(room, given, {});
      given.onResume = 'stay';
      given.become('suspended');
      expect(given.resumed).toBe(0);
      visible('visible');
      document.dispatchEvent(new Event('visibilitychange'));
      expect(given.resumed).toBe(1);
      await room.close();
    });

    it('a visible page does not resume a context that runs', async () => {
      visible('visible');
      const given = new StubAudio();
      const room = newLiveKitRoom();
      await connect(room, given, {});
      document.dispatchEvent(new Event('visibilitychange'));
      expect(given.resumed).toBe(0);
      await room.close();
    });

    it('stops listening for the page being visible when the tap goes', async () => {
      visible('visible');
      const given = new StubAudio();
      const room = newLiveKitRoom();
      await connect(room, given, {});
      await room.close();
      given.state = 'suspended';
      document.dispatchEvent(new Event('visibilitychange'));
      expect(given.resumed).toBe(0);
    });
  });

  // Use microphone after Watch only: a click, so its context is the page's.
  it('a microphone published after a watch only join taps the context handed to it', async () => {
    const given = new StubAudio();
    const meter: boolean[] = [];
    const room = newLiveKitRoom();
    await room.connect('wss://lk.example', 'tok-abcdef', 'room-abcdef', false, { ...events, onMeter: (on: boolean) => meter.push(on) });
    expect(given.sources).toBe(0);
    await room.publishMicrophone(given.asContext());
    expect(state.contexts).toHaveLength(0);
    expect(given.sources).toBe(1);
    expect(meter).toEqual([true]);
    await room.close();
    expect(given.closed).toBe(0);
  });

  // The gate: an open microphone that was talking on speech must not stay open
  // on a buffer the suspended analyser kept.
  it('a context that suspends shuts an open microphone that was talking, and a running one lets it open again', async () => {
    const given = new StubAudio();
    given.sample = 255;
    const session: VoiceSessionResult = {
      ok: true,
      value: { livekit_url: 'wss://lk.example', identity: 'p7', rooms: [{ room: 'r', token: 't', can_publish: true, expires_at: '2026-11-13T10:00:15.000Z' }] },
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
    client.join('general', { audio: given.asContext() });
    await settle();
    await settle();
    expect(client.snapshot()).toMatchObject({ phase: 'connected', meter: true });
    client.setMode('open');
    clock.advance(100);
    expect(client.snapshot().talking).toBe(true);
    given.onResume = 'stay';
    given.become('suspended');
    await settle();
    expect(client.snapshot()).toMatchObject({ meter: false, talking: false });
    // Still loud in its buffer, still shut.
    clock.advance(100);
    expect(client.snapshot().talking).toBe(false);
    given.become('running');
    clock.advance(100);
    expect(client.snapshot()).toMatchObject({ meter: true, talking: true });
    await client.leave();
    // Closed once, by the client; the room's close did not.
    expect(given.closed).toBe(1);
  });
});
