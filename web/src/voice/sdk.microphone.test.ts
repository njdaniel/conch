// The real SDK, not a stub (issue #658): the microphone chosen is captured
// exactly, and the capture is published as a track the page provides, which
// the SDK never captures a device for by itself. Each of the SDK's three ways
// of doing so to a track it captured itself is driven here, against the
// page's track and, to show the check would see it, against the SDK's own:
// a device change that switches to the first device listed, a capture that
// ends while unmuted, and the next unmute after a capture ended. The browser's
// devices are a stub that counts every capture asked for. jsdom, no network,
// no microphone.
import { createLocalAudioTrack, LocalTrackPublication, Room, RoomEvent, Track, type LocalAudioTrack } from 'livekit-client';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureMicrophone } from './sdk';

// A browser's capture of one device.
class FakeCapture extends EventTarget {
  readonly kind = 'audio';
  readonly id: string;
  readonly deviceId: string;
  enabled = true;
  muted = false;
  readyState: MediaStreamTrackState = 'live';

  constructor(deviceId: string) {
    super();
    this.deviceId = deviceId;
    this.id = `capture-${deviceId}-${String(Math.random())}`;
  }

  getSettings(): MediaTrackSettings {
    return { deviceId: this.deviceId };
  }

  getConstraints(): MediaTrackConstraints {
    return {};
  }

  clone(): FakeCapture {
    return new FakeCapture(this.deviceId);
  }

  stop(): void {
    this.readyState = 'ended';
  }

  // The device goes: the browser ends the capture.
  end(): void {
    this.readyState = 'ended';
    this.dispatchEvent(new Event('ended'));
  }
}

interface Browser {
  // What every capture asked for, in order.
  readonly asked: MediaStreamConstraints[];
  inputs: string[];
  readonly devicechange: (() => void)[];
}

let browser: Browser;

// The device a capture's constraints name, if it is there: an exact id that
// is not listed fails, as a browser fails it; anything else is a preference,
// which the browser meets with some device it has.
function deviceFor(audio: MediaStreamConstraints['audio']): string | DOMException {
  const wanted = typeof audio === 'object' ? audio.deviceId : undefined;
  if (typeof wanted === 'object' && !Array.isArray(wanted) && wanted.exact !== undefined) {
    const id = String(wanted.exact);
    return browser.inputs.includes(id) ? id : new DOMException('no such device', 'OverconstrainedError');
  }
  const preferred = typeof wanted === 'string' ? wanted : typeof wanted === 'object' && !Array.isArray(wanted) ? String(wanted.ideal) : 'default';
  return browser.inputs.includes(preferred) ? preferred : (browser.inputs[0] ?? new DOMException('no device', 'NotFoundError'));
}

beforeEach(() => {
  browser = { asked: [], inputs: ['default', 'mic-1', 'mic-2'], devicechange: [] };
  // jsdom has neither: the SDK names both.
  vi.stubGlobal('MediaStreamTrack', FakeCapture);
  vi.stubGlobal(
    'MediaStream',
    class {
      private readonly tracks: FakeCapture[];
      constructor(tracks: FakeCapture[] = []) {
        this.tracks = tracks;
      }
      getTracks() {
        return this.tracks;
      }
    },
  );
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    value: {
      getUserMedia: (constraints: MediaStreamConstraints) => {
        browser.asked.push(constraints);
        const device = deviceFor(constraints.audio);
        if (device instanceof DOMException) return Promise.reject(device);
        return Promise.resolve(new MediaStream([new FakeCapture(device) as unknown as MediaStreamTrack]));
      },
      enumerateDevices: () =>
        Promise.resolve(browser.inputs.map((deviceId) => ({ kind: 'audioinput', deviceId, groupId: deviceId, label: `Microphone ${deviceId}` }))),
      addEventListener: (type: string, fn: () => void) => {
        if (type === 'devicechange') browser.devicechange.push(fn);
      },
      removeEventListener: () => undefined,
    },
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  Reflect.deleteProperty(navigator, 'mediaDevices');
});

const capture = (track: LocalAudioTrack): FakeCapture => track.mediaStreamTrack as unknown as FakeCapture;

describe('a chosen microphone', () => {
  it('is captured exactly: one failed request, not a second that takes another device', async () => {
    await expect(captureMicrophone('unplugged')).rejects.toMatchObject({ name: 'OverconstrainedError' });
    expect(browser.asked).toHaveLength(1);
    expect(browser.asked[0]?.audio).toMatchObject({ deviceId: { exact: 'unplugged' } });
    // A bare id, as the SDK takes it, is tried again as a preference, which
    // captures some other device: the check above would see that.
    browser.asked.length = 0;
    const other = await createLocalAudioTrack({ deviceId: 'unplugged' });
    expect(browser.asked).toHaveLength(2);
    expect(capture(other).deviceId).toBe('default');
    other.stop();
  });

  it('is published as a track the page provides, from the device chosen', async () => {
    const track = await captureMicrophone('mic-1');
    expect(track.isUserProvided).toBe(true);
    expect(track.source).toBe(Track.Source.Microphone);
    expect(capture(track).deviceId).toBe('mic-1');
    expect(browser.asked).toHaveLength(1);
    track.stop();
  });
});

// The three ways the SDK captures a device for a track it captured itself
// (livekit-client 2.22.3): none of them does for the page's track.
describe("the SDK's own captures", () => {
  const theirs = async (): Promise<LocalAudioTrack> => {
    const track = await createLocalAudioTrack({ echoCancellation: true, noiseSuppression: true, autoGainControl: true });
    expect(track.isUserProvided).toBe(false);
    return track;
  };

  it('(c) the next unmute after the capture ended asks for no new capture', async () => {
    const mine = await captureMicrophone('mic-1');
    await mine.mute();
    capture(mine).end();
    const before = browser.asked.length;
    await mine.unmute();
    expect(browser.asked).toHaveLength(before);

    const sdks = await theirs();
    await sdks.mute();
    capture(sdks).end();
    const was = browser.asked.length;
    await sdks.unmute();
    expect(browser.asked).toHaveLength(was + 1);
    mine.stop();
    sdks.stop();
  });

  // The participant's handler for a track that ended, as the SDK calls it
  // for a published track.
  const ended = (room: Room, track: LocalAudioTrack): Promise<void> =>
    (room.localParticipant as unknown as { handleTrackEnded: (t: LocalAudioTrack) => Promise<void> }).handleTrackEnded(track);

  it('(b) a capture that ends while unmuted is muted, and no other device is captured', async () => {
    const room = new Room();
    const mine = await captureMicrophone('mic-1');
    capture(mine).end();
    const before = browser.asked.length;
    await ended(room, mine);
    expect(browser.asked).toHaveLength(before);
    expect(mine.isMuted).toBe(true);

    const sdks = await theirs();
    capture(sdks).end();
    const was = browser.asked.length;
    await ended(room, sdks);
    // The SDK's own track is captured again, from the default device.
    expect(browser.asked).toHaveLength(was + 1);
    expect(browser.asked.at(-1)?.audio).toMatchObject({ deviceId: 'default' });
    mine.stop();
    sdks.stop();
    await room.disconnect();
  });

  // A room with this track published as its microphone, as far as the
  // room's device handling looks.
  function publishedIn(room: Room, track: LocalAudioTrack): void {
    const info = { sid: 'TR_microphone', name: 'microphone', source: 2, mimeType: '', encryption: 0 } as unknown as ConstructorParameters<typeof LocalTrackPublication>[1];
    const pub = new LocalTrackPublication(Track.Kind.Audio, info, track);
    expect(pub.source).toBe(Track.Source.Microphone);
    room.localParticipant.trackPublications.set('TR_microphone', pub);
    room.localParticipant.audioTrackPublications.set('TR_microphone', pub);
  }

  // A device change after which the device in use is no longer first.
  async function deviceChange(room: Room): Promise<void> {
    await Room.getLocalDevices(undefined, false);
    browser.inputs = ['mic-2'];
    const told = new Promise<void>((resolve) => {
      room.once(RoomEvent.MediaDevicesChanged, () => {
        resolve();
      });
    });
    for (const fn of browser.devicechange) fn();
    await told;
  }

  it('(a) a device change does not move the track to the first device listed', async () => {
    const room = new Room();
    const mine = await captureMicrophone(null);
    publishedIn(room, mine);
    const before = browser.asked.length;
    await deviceChange(room);
    expect(browser.asked).toHaveLength(before);
    expect(capture(mine).deviceId).toBe('default');
    mine.stop();
    await room.disconnect();

    browser.inputs = ['default', 'mic-1', 'mic-2'];
    browser.devicechange.length = 0;
    const other = new Room();
    const sdks = await theirs();
    publishedIn(other, sdks);
    const was = browser.asked.length;
    await deviceChange(other);
    expect(browser.asked).toHaveLength(was + 1);
    expect(browser.asked.at(-1)?.audio).toMatchObject({ deviceId: { exact: 'mic-2' } });
    sdks.stop();
    await other.disconnect();
  });
});
