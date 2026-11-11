// The sharer's quality choice, its capture and encoding caps, and the live
// readout. jsdom storage, but no network, browser capture or LiveKit server.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { readShareQuality, writeShareQuality } from '../api';
import { defaultShareQuality, type ShareQuality, type ShareSample } from './share';
import { askedText, captureOptions, nextShortfall, noShortfall, shareEncodings, shareReadout, shareSample, shortfallHold, shortfallReason, type ShareStats, type ShortfallState } from './sdk';

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

describe('the sent readout: reasons, loss and the capture', () => {
  const sent = (overrides: Partial<ShareStats> = {}): ShareStats => ({
    type: 'outbound-rtp',
    kind: 'video',
    id: 'OT1',
    ssrc: 7,
    frameWidth: 1920,
    frameHeight: 1080,
    framesPerSecond: 30,
    bytesSent: 1_000_000,
    timestamp: 1000,
    ...overrides,
  });
  const remote = (overrides: Partial<ShareStats> = {}): ShareStats => ({ type: 'remote-inbound-rtp', kind: 'video', localId: 'OT1', ssrc: 7, fractionLost: 0.004, ...overrides });

  it.each([
    { reason: 'none', want: 'none' },
    { reason: 'cpu', want: 'cpu' },
    { reason: 'bandwidth', want: 'bandwidth' },
    { reason: 'other', want: 'other' },
  ])('keeps the encoder reason $reason', ({ reason, want }) => {
    expect(shareSample([sent({ qualityLimitationReason: reason })], 'sent')?.limitation).toBe(want);
  });

  it('leaves an absent or unknown reason out rather than guessing', () => {
    expect(shareSample([sent()], 'sent')).not.toHaveProperty('limitation');
    expect(shareSample([sent({ qualityLimitationReason: 'sunspots' })], 'sent')).not.toHaveProperty('limitation');
  });

  it('reads the sender loss from the remote report of the same stream, and says nothing without one', () => {
    const first = shareSample([sent(), remote()], 'sent');
    expect(first?.lost).toBe(0.004);
    expect(shareReadout(first)).toBe('1920×1080 · 30 fps · 0.4% lost');
    const next = shareSample([sent({ bytesSent: 1_725_000, timestamp: 2000 }), remote({ fractionLost: 0 })], 'sent');
    expect(shareReadout(next, first)).toBe('1920×1080 · 30 fps · 5.8 Mbps · 0.0% lost');
    expect(shareReadout(shareSample([sent()], 'sent'))).toBe('1920×1080 · 30 fps');
    // Another stream's report, an audio report and a nonsense fraction are not this one's.
    expect(shareSample([sent(), remote({ localId: 'OT2', ssrc: 8 }), remote({ kind: 'audio' }), remote({ fractionLost: 7 })], 'sent')).not.toHaveProperty('lost');
  });

  it('reads the viewer loss over the interval between two samples of one stream', () => {
    const inbound = (overrides: Partial<ShareStats> = {}): ShareStats => ({ type: 'inbound-rtp', kind: 'video', id: 'IT1', ssrc: 3, frameWidth: 1280, frameHeight: 720, framesPerSecond: 15, packetsReceived: 1000, packetsLost: 100, ...overrides });
    const first = shareSample([inbound()], 'received');
    // The first sample has no interval, whatever the lifetime counters hold.
    expect(shareReadout(first)).toBe('1280×720 · 15 fps');
    const next = shareSample([inbound({ packetsReceived: 1995, packetsLost: 105 })], 'received');
    expect(shareReadout(next, first)).toBe('1280×720 · 15 fps · 0.5% lost');
    const clean = shareSample([inbound({ packetsReceived: 2995, packetsLost: 105 })], 'received');
    expect(shareReadout(clean, next)).toBe('1280×720 · 15 fps · 0.0% lost');
    // Another stream, counters gone back, no packets in the interval, or counters absent: nothing.
    expect(shareReadout(shareSample([inbound({ id: 'IT2', packetsReceived: 2000 })], 'received'), first)).toBe('1280×720 · 15 fps');
    expect(shareReadout(shareSample([inbound({ packetsReceived: 10 })], 'received'), first)).toBe('1280×720 · 15 fps');
    expect(shareReadout(shareSample([inbound()], 'received'), first)).toBe('1280×720 · 15 fps');
    expect(shareReadout(shareSample([inbound({ packetsReceived: undefined })], 'received'), first)).toBe('1280×720 · 15 fps');
    // A viewer has no encoder reason, whatever the stat holds.
    expect(shareSample([inbound({ qualityLimitationReason: 'cpu' })], 'received')).not.toHaveProperty('limitation');
  });

  it('marks a sent sample whose simulcast layer is not being sent', () => {
    const layer = (overrides: Partial<ShareStats>): ShareStats => ({ type: 'outbound-rtp', kind: 'video', frameWidth: 1280, frameHeight: 720, framesPerSecond: 15, ...overrides });
    expect(shareSample([layer({ id: 'a' }), layer({ id: 'b', frameWidth: 1920, frameHeight: 1080, active: false })], 'sent')?.paused).toBe(true);
    expect(shareSample([layer({ id: 'a', active: true })], 'sent')).not.toHaveProperty('paused');
    expect(shareSample([layer({ id: 'a' })], 'sent')).not.toHaveProperty('paused');
  });

  it('counts a pause only when the layer off is above the one sent', () => {
    const layer = (overrides: Partial<ShareStats>): ShareStats => ({ type: 'outbound-rtp', kind: 'video', frameWidth: 1920, frameHeight: 1080, framesPerSecond: 31, ...overrides });
    const small = (overrides: Partial<ShareStats>): ShareStats => layer({ frameWidth: 1280, frameHeight: 720, framesPerSecond: 15, ...overrides });
    // The 720p layer nobody takes is off, the top is sent: not paused.
    const lower = shareSample([layer({ id: 'f', rid: 'f' }), small({ id: 'h', rid: 'h', active: false })], 'sent');
    expect(lower).toMatchObject({ width: 1920, height: 1080 });
    expect(lower).not.toHaveProperty('paused');
    expect(lower).not.toHaveProperty('layerOff');
    // Ranked by rid when there are no sizes on the off layer.
    expect(shareSample([layer({ id: 'f', rid: 'f' }), { type: 'outbound-rtp', kind: 'video', id: 'q', rid: 'q', active: false }], 'sent')).not.toHaveProperty('paused');
    expect(shareSample([small({ id: 'h', rid: 'h' }), { type: 'outbound-rtp', kind: 'video', id: 'f', rid: 'f', active: false }], 'sent')?.paused).toBe(true);
    // The top off with a stale size: the sent layer is the largest that is sending.
    const stale = shareSample([layer({ id: 'f', rid: 'f', active: false }), small({ id: 'h', rid: 'h' })], 'sent');
    expect(stale).toMatchObject({ width: 1280, height: 720, paused: true });
    // Nothing to rank by: noted, not called a pause.
    const unknown = shareSample([layer({ id: 'x' }), { type: 'outbound-rtp', kind: 'video', id: 'y', active: false }], 'sent');
    expect(unknown).toMatchObject({ layerOff: true });
    expect(unknown).not.toHaveProperty('paused');
  });

  it('keeps only the capture settings the browser gave', () => {
    expect(shareSample([sent()], 'sent', 'TR', { width: 1280, height: 720, fps: 30 })?.captured).toEqual({ width: 1280, height: 720, fps: 30 });
    expect(shareSample([sent()], 'sent', 'TR', { width: 0, height: undefined, fps: undefined })).not.toHaveProperty('captured');
    expect(shareSample([{ ...sent(), type: 'inbound-rtp' }], 'received', 'TR', { width: 1280, height: 720 })).not.toHaveProperty('captured');
  });

  it('says what was asked, labelled as asked', () => {
    expect(askedText({ resolution: '1080p', frameRate: 60 })).toBe('Asked 1080p · 60 fps');
    expect(askedText({ resolution: '720p', frameRate: 15 })).toBe('Asked 720p · 15 fps');
    expect(askedText({ resolution: 'source', frameRate: 30 })).toBe('Asked source size · 30 fps');
  });
});

describe('the shortfall reason', () => {
  const ask: ShareQuality = { resolution: '1080p', frameRate: 60 };
  const sample = (overrides: Partial<ShareSample> = {}): ShareSample => ({ width: 1920, height: 1080, fps: 60, ...overrides });

  it.each([
    { what: 'on target', s: sample(), q: ask, want: null },
    { what: 'a little under the rate is on target', s: sample({ fps: 49 }), q: ask, want: null },
    { what: 'slow, the encoder says CPU', s: sample({ fps: 31, limitation: 'cpu' }), q: ask, want: 'Limited by CPU' },
    { what: 'slow, the encoder says bandwidth', s: sample({ fps: 31, limitation: 'bandwidth' }), q: ask, want: 'Limited by bandwidth' },
    { what: 'slow, the encoder says other', s: sample({ fps: 31, limitation: 'other' }), q: ask, want: "Your browser doesn't say why" },
    { what: 'slow, no limitation reported (Firefox): no reason', s: sample({ fps: 31 }), q: ask, want: null },
    { what: 'no limitation reported, a smaller sent picture with a full-size capture (Firefox top layer shrunk)', s: sample({ width: 1280, height: 720, fps: 15, captured: { width: 1920, height: 1080 } }), q: ask, want: null },
    { what: 'no limitation reported, a capture smaller than asked', s: sample({ width: 1280, height: 720, captured: { width: 1280, height: 720 } }), q: ask, want: "What you're sharing is smaller than 1080p" },
    { what: 'none and slow stays no reason', s: sample({ fps: 15, limitation: 'none' }), q: ask, want: null },
    { what: 'slow but the encoder says none (a still screen)', s: sample({ fps: 2, limitation: 'none' }), q: ask, want: null },
    { what: 'scaled down by bandwidth', s: sample({ width: 1280, height: 720, limitation: 'bandwidth', captured: { width: 1920, height: 1080 } }), q: ask, want: 'Limited by bandwidth' },
    { what: 'a capture smaller than asked', s: sample({ width: 1280, height: 720, captured: { width: 1280, height: 720 } }), q: ask, want: "What you're sharing is smaller than 1080p" },
    { what: 'a capture smaller than asked beats a reason', s: sample({ width: 1280, height: 720, limitation: 'cpu', captured: { width: 1280, height: 720 } }), q: ask, want: "What you're sharing is smaller than 1080p" },
    { what: 'a smaller picture with no capture settings is not blamed on the window', s: sample({ width: 1280, height: 720, limitation: 'cpu' }), q: ask, want: 'Limited by CPU' },
    { what: 'a taller capture fitted to the box is on target', s: sample({ width: 1728, height: 1080, captured: { width: 1920, height: 1200 } }), q: ask, want: null },
    { what: 'the top layer paused (nobody takes it): not a shortfall', s: sample({ width: 1280, height: 720, fps: 15, limitation: 'none', paused: true, captured: { width: 1920, height: 1080 } }), q: ask, want: null },
    { what: 'the top layer paused, whatever the limitation says', s: sample({ width: 1280, height: 720, fps: 15, limitation: 'cpu', paused: true, captured: { width: 1920, height: 1080 } }), q: ask, want: null },
    { what: 'the top layer paused and no reason in the stats', s: sample({ width: 1280, height: 720, fps: 15, paused: true }), q: ask, want: null },
    { what: 'a whole layer short with limitation none and a big capture (paused, unflagged)', s: sample({ width: 1280, height: 720, fps: 15, limitation: 'none', captured: { width: 1920, height: 1080 } }), q: ask, want: null },
    { what: 'a smaller capture still shows while the top layer is paused', s: sample({ width: 1280, height: 720, limitation: 'none', paused: true, captured: { width: 1280, height: 720 } }), q: ask, want: "What you're sharing is smaller than 1080p" },
    { what: 'a lower layer off (nobody takes it), the top slow on CPU', s: sample({ fps: 31, limitation: 'cpu', layerOff: true, captured: { width: 1920, height: 1080 } }), q: ask, want: 'Limited by CPU' },
    { what: 'a lower layer off, the top slow on bandwidth', s: sample({ fps: 31, limitation: 'bandwidth', layerOff: true, captured: { width: 1920, height: 1080 } }), q: ask, want: 'Limited by bandwidth' },
    { what: 'a layer off of unknown rank, the picture smaller than the capture fitted', s: sample({ width: 1280, height: 720, fps: 15, limitation: 'cpu', layerOff: true, captured: { width: 1920, height: 1080 } }), q: ask, want: null },
    { what: '720p asked and delivered', s: sample({ width: 1280, height: 720 }), q: { resolution: '720p', frameRate: 60 }, want: null },
    { what: 'a small window at 720p', s: sample({ width: 800, height: 600, captured: { width: 800, height: 600 } }), q: { resolution: '720p', frameRate: 60 }, want: "What you're sharing is smaller than 720p" },
    { what: 'source asks no size', s: sample({ width: 800, height: 600 }), q: { resolution: 'source', frameRate: 60 }, want: null },
  ] satisfies { what: string; s: ShareSample; q: ShareQuality; want: string | null }[])('$what', ({ s, q, want }) => {
    expect(shortfallReason(s, q)).toBe(want);
  });

  const slow = sample({ fps: 31, limitation: 'cpu' });
  const step = (state: ShortfallState, s: ShareSample | null, now: number) => nextShortfall(state, s, ask, now);

  it('shows the reason only after the shortfall has held, and clears it only after recovery has held', () => {
    let state = noShortfall;
    state = step(state, slow, 0);
    expect(state.reason).toBeNull();
    state = step(state, slow, shortfallHold - 1);
    expect(state.reason).toBeNull();
    state = step(state, slow, shortfallHold);
    expect(state.reason).toBe('Limited by CPU');
    // Back on target: the reason stays until the hold has passed.
    state = step(state, sample(), 6000);
    expect(state.reason).toBe('Limited by CPU');
    state = step(state, sample(), 6000 + shortfallHold - 1);
    expect(state.reason).toBe('Limited by CPU');
    state = step(state, sample(), 6000 + shortfallHold);
    expect(state.reason).toBeNull();
  });

  it('a blip shorter than the hold never shows, and a relapse restarts the hold', () => {
    let state = step(noShortfall, slow, 0);
    state = step(state, sample(), 3000);
    state = step(state, slow, 4000);
    state = step(state, slow, 4000 + shortfallHold - 1);
    expect(state.reason).toBeNull();
    state = step(state, slow, 4000 + shortfallHold);
    expect(state.reason).toBe('Limited by CPU');
    // A relapse while the reason shows keeps it, and cancels the clearing.
    state = step(state, sample(), 10_000);
    state = step(state, slow, 12_000);
    state = step(state, sample(), 14_000);
    expect(state.reason).toBe('Limited by CPU');
  });

  it('follows the latest reason while short, and starts over on no sample or another publication', () => {
    let state = step(noShortfall, { ...slow, publication: 'A' }, 0);
    state = step(state, { ...slow, publication: 'A' }, shortfallHold);
    expect(state.reason).toBe('Limited by CPU');
    state = step(state, { ...slow, publication: 'A', limitation: 'bandwidth' }, 6000);
    expect(state.reason).toBe('Limited by bandwidth');
    expect(step(state, { ...slow, publication: 'B' }, 7000).reason).toBeNull();
    expect(step(state, null, 7000)).toEqual(noShortfall);
  });
});
