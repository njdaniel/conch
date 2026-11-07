// The sharer's quality choice, its capture and encoding caps, and the live
// readout. jsdom storage, but no network, browser capture or LiveKit server.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readShareQuality, writeShareQuality } from '../api';
import { defaultShareQuality, type ShareQuality } from './share';
import { captureOptions, shareEncodings, shareReadout, shareSample, type ShareStats } from './sdk';

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe('the share quality kept in local preferences', () => {
  it('defaults to 1080p at 30 fps and reads back the last choice', () => {
    expect(readShareQuality()).toEqual(defaultShareQuality);
    expect(defaultShareQuality).toEqual({ resolution: '1080p', frameRate: 30 });
    const chosen: ShareQuality = { resolution: '720p', frameRate: 15 };
    writeShareQuality(chosen);
    expect(readShareQuality()).toEqual(chosen);
  });

  it.each(['not json', 'null', '{"resolution":"4k","frameRate":30}', '{"resolution":"720p","frameRate":24}', '{"resolution":"1080p"}'])('reads an invalid kept value as the default: %s', (kept) => {
    window.localStorage.setItem('conch.web.voice.share-quality.v1', kept);
    expect(readShareQuality()).toEqual(defaultShareQuality);
  });

  it('tolerates refused storage', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('storage is off'); });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('storage is off'); });
    expect(readShareQuality()).toEqual(defaultShareQuality);
    expect(() => { writeShareQuality({ resolution: 'source', frameRate: 60 }); }).not.toThrow();
  });
});

describe('the chosen capture and encodings', () => {
  it.each([
    { quality: { resolution: '720p', frameRate: 15 }, width: 1280, height: 720, hint: 'detail' },
    { quality: { resolution: '1080p', frameRate: 30 }, width: 1920, height: 1080, hint: 'motion' },
    { quality: { resolution: 'source', frameRate: 60 }, width: 3840, height: 2160, hint: 'motion' },
  ] as const)('caps $quality.resolution at $quality.frameRate fps and uses $hint', ({ quality, width, height, hint }) => {
    expect(captureOptions(quality)).toMatchObject({ resolution: { width, height, frameRate: quality.frameRate }, contentHint: hint });
  });

  const bottom = { width: 1280, height: 720, maxBitrate: 2_000_000, maxFramerate: 30 };
  it.each([
    { quality: { resolution: '720p', frameRate: 15 }, top: { width: 1280, height: 720, maxBitrate: 6_000_000, maxFramerate: 15 }, lower: [] },
    { quality: { resolution: '1080p', frameRate: 30 }, top: { width: 1920, height: 1080, maxBitrate: 6_000_000, maxFramerate: 30 }, lower: [bottom] },
    { quality: { resolution: 'source', frameRate: 60 }, top: { width: 3840, height: 2160, maxBitrate: 18_000_000, maxFramerate: 60 }, lower: [bottom, { width: 1920, height: 1080, maxBitrate: 4_000_000, maxFramerate: 30 }] },
  ] as const)('sets the top and bottom layers for $quality.resolution at $quality.frameRate fps', ({ quality, top, lower }) => {
    expect(shareEncodings({ width: 3840, height: 2160, frameRate: 60 }, quality)).toEqual({ top, lower });
  });

  it('does not upscale a smaller capture and never sends a lower layer faster than the top', () => {
    expect(shareEncodings({ width: 1000, height: 700, frameRate: 15 }, { resolution: '1080p', frameRate: 60 })).toEqual({
      top: { width: 1000, height: 700, maxBitrate: 6_000_000, maxFramerate: 15 },
      lower: [],
    });
    expect(shareEncodings({ width: 3840, height: 2160, frameRate: 60 }, { resolution: 'source', frameRate: 15 }).lower).toEqual([
      { ...bottom, maxFramerate: 15 },
      { width: 1920, height: 1080, maxBitrate: 4_000_000, maxFramerate: 15 },
    ]);
  });
});

describe('the stats readout', () => {
  const received = (overrides: Partial<ShareStats> = {}): ShareStats => ({
    type: 'inbound-rtp',
    kind: 'video',
    frameWidth: 1920,
    frameHeight: 1080,
    framesPerSecond: 30,
    bytesReceived: 1_000_000,
    timestamp: 1000,
    ...overrides,
  });

  it('reads resolution and fps at once, then bitrate from bytes over time', () => {
    const first = shareSample([received()], 'received');
    expect(first).toEqual({ width: 1920, height: 1080, fps: 30, bytes: 1_000_000, timestamp: 1000 });
    expect(shareReadout(first)).toBe('1920×1080 · 30 fps');
    const next = shareSample([received({ bytesReceived: 1_475_000, timestamp: 2000 })], 'received');
    expect(shareReadout(next, first)).toBe('1920×1080 · 30 fps · 3.8 Mbps');
  });

  it('shows zero fps and a zero bitrate, rather than treating zero as missing', () => {
    const first = shareSample([received({ framesPerSecond: 0 })], 'received');
    const next = shareSample([received({ framesPerSecond: 0, timestamp: 2000 })], 'received');
    expect(shareReadout(next, first)).toBe('1920×1080 · 0 fps · 0.0 Mbps');
  });

  it.each([
    { what: 'no video', stats: [] },
    { what: 'audio only', stats: [received({ kind: 'audio' })] },
    { what: 'no width', stats: [received({ frameWidth: undefined })] },
    { what: 'zero height', stats: [received({ frameHeight: 0 })] },
    { what: 'no fps', stats: [received({ framesPerSecond: undefined })] },
  ])('shows nothing for $what', ({ stats }) => {
    expect(shareSample(stats, 'received')).toBeNull();
  });

  it('omits bitrate without a usable earlier sample, and ignores audio and other RTP directions', () => {
    const stats = [received({ bytesReceived: undefined }), received({ type: 'outbound-rtp', bytesSent: 9_000_000 }), received({ kind: 'audio' })];
    const sample = shareSample(stats, 'received');
    expect(sample).toEqual({ width: 1920, height: 1080, fps: 30, timestamp: 1000 });
    expect(shareReadout(sample, { width: 1920, height: 1080, fps: 30, bytes: 0, timestamp: 0 })).toBe('1920×1080 · 30 fps');
  });

  it('keeps the stream and publication, and takes bitrate only within one of each', () => {
    const a = shareSample([received({ id: 'RTCInboundRTPVideoStream_1', ssrc: 11 })], 'received', 'TR_a');
    expect(a).toMatchObject({ stream: 'RTCInboundRTPVideoStream_1/11', publication: 'TR_a' });
    const same = shareSample([received({ id: 'RTCInboundRTPVideoStream_1', ssrc: 11, bytesReceived: 1_475_000, timestamp: 2000 })], 'received', 'TR_a');
    expect(shareReadout(same, a)).toBe('1920×1080 · 30 fps · 3.8 Mbps');
    const replaced = shareSample([received({ id: 'RTCInboundRTPVideoStream_2', ssrc: 22, bytesReceived: 1_475_000, timestamp: 2000 })], 'received', 'TR_a');
    expect(shareReadout(replaced, a)).toBe('1920×1080 · 30 fps');
    const renewed = shareSample([received({ id: 'RTCInboundRTPVideoStream_1', ssrc: 11, bytesReceived: 1_475_000, timestamp: 2000 })], 'received', 'TR_b');
    expect(shareReadout(renewed, a)).toBe('1920×1080 · 30 fps');
  });

  it('follows the largest sent layer, so a switch of layer resets the bitrate', () => {
    const layer = (ssrc: number, frameWidth: number, frameHeight: number, bytesSent: number, timestamp: number): ShareStats => ({ type: 'outbound-rtp', kind: 'video', id: `out${String(ssrc)}`, ssrc, frameWidth, frameHeight, framesPerSecond: 30, bytesSent, timestamp });
    const both = (bigBytes: number, timestamp: number, bigActive = true): ShareStats[] => [layer(1, 1280, 720, 500_000, timestamp), ...(bigActive ? [layer(2, 1920, 1080, bigBytes, timestamp)] : [])];
    const first = shareSample(both(1_000_000, 1000), 'sent');
    expect(first).toMatchObject({ width: 1920, height: 1080, stream: 'out2/2' });
    const dropped = shareSample(both(0, 2000, false), 'sent');
    expect(dropped).toMatchObject({ width: 1280, height: 720, stream: 'out1/1' });
    expect(shareReadout(dropped, first)).toBe('1280×720 · 30 fps');
  });

  it('omits bitrate when the counter goes back or time does not advance, and shows a zero reading', () => {
    const first = shareSample([received({ id: 's', ssrc: 1, bytesReceived: 5_000_000 })], 'received');
    const reset = shareSample([received({ id: 's', ssrc: 1, bytesReceived: 100, timestamp: 2000 })], 'received');
    expect(shareReadout(reset, first)).toBe('1920×1080 · 30 fps');
    const still = shareSample([received({ id: 's', ssrc: 1, bytesReceived: 5_000_000, timestamp: 1000 })], 'received');
    expect(shareReadout(still, first)).toBe('1920×1080 · 30 fps');
    const zero = shareSample([received({ id: 's', ssrc: 1, bytesReceived: 5_000_000, timestamp: 2000 })], 'received');
    expect(shareReadout(zero, first)).toBe('1920×1080 · 30 fps · 0.0 Mbps');
  });

  it('shows no readout for a missing sample and no bitrate against a missing previous one', () => {
    expect(shareReadout(null, shareSample([received()], 'received'))).toBeNull();
    expect(shareReadout(shareSample([received()], 'received'), null)).toBe('1920×1080 · 30 fps');
  });
});
