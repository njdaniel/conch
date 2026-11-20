import { afterEach, describe, expect, it, vi } from 'vitest';
import { callQuality, receivedShare, sentShare, startDiagnostics, type Diagnostics, type StatsReport } from './stats';

// Synthetic RTCStatsReport snapshots for replay. Each map is a sender or
// receiver report, with opaque IDs linking entries within that report.
type Stat = NonNullable<ReturnType<StatsReport['get']>>;
const report = (...entries: (Stat & { readonly id: string })[]): StatsReport => new Map(entries.map((stat) => [stat.id, stat]));

const audio = { id: 'audio-in', type: 'inbound-rtp', kind: 'audio', ssrc: 10, transportId: 'sub', remoteId: 'audio-out' };
const audioBefore = report(
  { ...audio, timestamp: 1000, packetsReceived: 500, packetsLost: 0, jitter: 0.004 },
);
const audioAfter = report(
  { ...audio, timestamp: 3000, packetsReceived: 598, packetsLost: 2, jitter: 0.012 },
  { id: 'sub', type: 'transport', selectedCandidatePairId: 'pair-selected' },
  { id: 'pair-old', type: 'candidate-pair', currentRoundTripTime: 0.9 },
  { id: 'pair-selected', type: 'candidate-pair', currentRoundTripTime: 0.064 },
);

const outbound = {
  id: 'video-out', type: 'outbound-rtp', kind: 'video', ssrc: 20, active: true,
  frameWidth: 1280, frameHeight: 720, codecId: 'video-codec', mediaSourceId: 'screen-input',
};
const screen = { id: 'screen-input', type: 'media-source', kind: 'video' };
const sendBefore = report(
  { ...outbound, timestamp: 1000, framesSent: 330 },
  { ...screen, timestamp: 1000, frames: 802 },
);
const sendAfter = (reason?: string) => report(
  { ...outbound, timestamp: 3000, framesSent: 332, qualityLimitationReason: reason },
  { id: 'paused', type: 'outbound-rtp', kind: 'video', ssrc: 21, active: false, frameWidth: 1920, frameHeight: 1080, qualityLimitationReason: 'cpu' },
  { id: 'audio-codec', type: 'codec', mimeType: 'audio/opus' },
  { id: 'video-codec', type: 'codec', mimeType: 'video/VP8' },
  { ...screen, timestamp: 3000, frames: 806, framesPerSecond: 2 },
);

const inbound = { id: 'video-in', type: 'inbound-rtp', kind: 'video', ssrc: 30, frameWidth: 1280, frameHeight: 720 };
const receiveBefore = report(
  { ...inbound, timestamp: 1000, framesReceived: 140, framesRendered: 120, freezeCount: 1 },
);
const receiveAfter = report(
  { ...inbound, timestamp: 3000, framesReceived: 190, framesRendered: 160, freezeCount: 3 },
);

describe('call quality', () => {
  it('reports interval loss, receiver jitter and the RTT from the selected transport', () => {
    expect(callQuality(audioAfter, audioBefore)).toEqual({ lossPercent: 2, jitterMs: 12, rttMs: 64 });
    expect(callQuality(audioAfter)).toEqual({ lossPercent: undefined, jitterMs: 12, rttMs: 64 });
    expect(callQuality(receiveAfter, receiveBefore)).toBeUndefined();
  });

  it('prefers a linked RTCP measurement and leaves missing metrics blank', () => {
    const stats = report(
      { ...audio, timestamp: 3000, packetsReceived: 600, packetsLost: 0, jitter: 0 },
      { id: 'audio-out', type: 'remote-outbound-rtp', roundTripTime: 0.032 },
      ...[...audioAfter.values()].filter((s) => s.id !== audio.id).map((s) => ({ ...s, id: s.id ?? '' })),
    );
    expect(callQuality(stats, audioBefore)).toEqual({ lossPercent: 0, jitterMs: 0, rttMs: 32 });
    expect(callQuality(report(audio))).toEqual({ lossPercent: undefined, jitterMs: undefined, rttMs: undefined });
  });

  it('drops loss baselines when an RTP stream changes or its received counter resets', () => {
    const replaced = report({ ...audio, ssrc: 90, timestamp: 3000, packetsReceived: 600, packetsLost: 2 });
    const reset = report({ ...audio, timestamp: 3000, packetsReceived: 50, packetsLost: 2 });
    expect(callQuality(replaced, audioBefore)?.lossPercent).toBeUndefined();
    expect(callQuality(reset, audioBefore)?.lossPercent).toBeUndefined();
  });
});

describe('screen-share quality', () => {
  it.each(['cpu', 'bandwidth', 'none', 'other', 'unknown', undefined])('reads the active layer and limit reason %s', (reason) => {
    expect(sentShare(sendAfter(reason), sendBefore)).toEqual({
      captureFps: 2,
      sentFps: 1,
      size: '1280×720',
      limitation: reason === 'unknown' ? undefined : reason,
      codec: 'VP8',
    });
  });

  it('can derive capture fps when the source only reports a frame counter', () => {
    const counters = report({ ...screen, timestamp: 3000, frames: 806 }, { ...outbound, timestamp: 3000, framesSent: 330 });
    expect(sentShare(counters, sendBefore)).toEqual({ captureFps: 2, sentFps: 0, size: '1280×720', limitation: undefined, codec: undefined });
  });

  it('reports independent receive and render rates, resolution and cumulative freezes', () => {
    expect(receivedShare(receiveAfter, receiveBefore)).toEqual({ receivedFps: 25, renderedFps: 20, size: '1280×720', freezes: 3 });
    expect(receivedShare(receiveAfter)).toEqual({ receivedFps: undefined, renderedFps: undefined, size: '1280×720', freezes: 3 });
  });

  it('uses the attached video playback counters when WebRTC omits framesRendered', () => {
    const stats = report({ ...inbound, timestamp: 3000, framesReceived: 190 });
    const first = { creationTime: 1000, totalVideoFrames: 120, droppedVideoFrames: 2 };
    const next = { creationTime: 3000, totalVideoFrames: 164, droppedVideoFrames: 6 };
    expect(receivedShare(stats, receiveBefore, next, first)).toEqual({ receivedFps: 25, renderedFps: 20, size: '1280×720', freezes: undefined });
    expect(receivedShare(stats, receiveBefore, next)?.renderedFps).toBeUndefined();
  });
});

describe('sampling', () => {
  afterEach(() => { vi.useRealTimers(); });

  it('polls immediately and every two seconds, with distinct baselines for each track', async () => {
    vi.useFakeTimers();
    const mic = { getRTCStatsReport: vi.fn().mockResolvedValueOnce(audioBefore).mockResolvedValue(audioAfter) };
    const screen = { getRTCStatsReport: vi.fn().mockResolvedValueOnce(sendBefore).mockResolvedValue(sendAfter('cpu')) };
    const watch = { getRTCStatsReport: vi.fn().mockResolvedValueOnce(receiveBefore).mockResolvedValue(receiveAfter) };
    const publish = vi.fn<(value: Diagnostics) => void>();
    const stop = startDiagnostics(() => [
      { kind: 'audio', track: mic, principal: 5 },
      { kind: 'sent', track: screen },
      { kind: 'received', track: watch, principal: 6, video: {} },
    ], publish);

    await vi.advanceTimersByTimeAsync(1999);
    expect(publish).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    const sample = publish.mock.lastCall?.[0];
    expect(sample?.calls.get(5)?.lossPercent).toBe(2);
    expect(sample?.sent?.sentFps).toBe(1);
    expect(sample?.received.get(6)?.renderedFps).toBe(20);
    stop();
    await vi.advanceTimersByTimeAsync(4000);
    expect(mic.getRTCStatsReport).toHaveBeenCalledTimes(2);
    expect(screen.getRTCStatsReport).toHaveBeenCalledTimes(2);
    expect(watch.getRTCStatsReport).toHaveBeenCalledTimes(2);
  });

  it('skips overlapping ticks and discards a read still in flight when closed', async () => {
    vi.useFakeTimers();
    let finish!: (stats: StatsReport) => void;
    const pending = new Promise<StatsReport>((resolve) => { finish = resolve; });
    const track = { getRTCStatsReport: vi.fn(() => pending) };
    const publish = vi.fn();
    const stop = startDiagnostics(() => [{ kind: 'sent', track }], publish);
    await vi.advanceTimersByTimeAsync(4000);
    expect(track.getRTCStatsReport).toHaveBeenCalledTimes(1);
    stop();
    finish(sendAfter('cpu'));
    await vi.advanceTimersByTimeAsync(4000);
    expect(track.getRTCStatsReport).toHaveBeenCalledTimes(1);
    expect(publish).not.toHaveBeenCalled();
  });
});
