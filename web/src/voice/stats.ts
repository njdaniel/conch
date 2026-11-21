// Local diagnostics for the dock. Browser stats are scoped to a single
// LiveKit sender or receiver. Only display metrics leave the sampler.
import type { ShareDirection, ShareLimitation } from './share';

// The subset of inbound-rtp and outbound-rtp stats the readout uses.
export interface ShareStats {
  readonly id?: string;
  readonly ssrc?: number;
  readonly type: string;
  readonly kind?: string;
  readonly mediaType?: string;
  readonly frameWidth?: number;
  readonly frameHeight?: number;
  readonly framesPerSecond?: number;
  readonly bytesReceived?: number;
  readonly bytesSent?: number;
  readonly timestamp?: number;
  // outbound-rtp: why the encoder holds the picture back, where the engine
  // says (Chromium does; Firefox and Safari report fewer reasons).
  readonly qualityLimitationReason?: string;
  // inbound-rtp: packets received and lost. remote-inbound-rtp: the sender's
  // view of loss, as a fraction of the interval.
  readonly packetsReceived?: number;
  readonly packetsLost?: number;
  readonly fractionLost?: number;
  readonly localId?: string;
  // outbound-rtp: false when this simulcast layer is not being sent (dynacast
  // pauses the layers nobody takes).
  readonly active?: boolean;
  // outbound-rtp: which simulcast layer: q (lowest), h or f (highest).
  readonly rid?: string;
}

export const count = (n: number | undefined): n is number => n !== undefined && Number.isFinite(n) && n >= 0;

// A paused layer or a browser that omits the reason has no current limit to
// report. Keep this interpretation shared with the CPU-limited share notice.
export function shareLimitation(stat: ShareStats): ShareLimitation | undefined {
  if (stat.active === false) return undefined;
  const reason = stat.qualityLimitationReason;
  return reason === 'none' || reason === 'cpu' || reason === 'bandwidth' || reason === 'other' ? reason : undefined;
}

// Pick the largest layer being sent. A paused simulcast layer may still
// report its last size. The existing share readout also requires size and FPS.
export function shareVideo<T extends ShareStats>(stats: Iterable<T>, direction: ShareDirection, requirePicture = false): T | undefined {
  const type = direction === 'received' ? 'inbound-rtp' : 'outbound-rtp';
  const area = (stat: ShareStats): number => (stat.frameWidth ?? 0) * (stat.frameHeight ?? 0);
  let found: T | undefined;
  for (const stat of stats) {
    if (stat.type !== type || stat.kind === 'audio' || stat.mediaType === 'audio') continue;
    if (requirePicture && (!count(stat.frameWidth) || stat.frameWidth === 0 ||
        !count(stat.frameHeight) || stat.frameHeight === 0 || !count(stat.framesPerSecond))) continue;
    const active = stat.active !== false;
    if (found === undefined || (active && found.active === false) ||
        (active === (found.active !== false) && area(stat) > area(found))) found = stat;
  }
  return found;
}

interface DiagnosticStats extends ShareStats {
  readonly codecId?: string;
  readonly mimeType?: string;
  readonly mediaSourceId?: string;
  readonly transportId?: string;
  readonly selectedCandidatePairId?: string;
  readonly remoteId?: string;
  readonly jitter?: number;
  readonly roundTripTime?: number;
  readonly currentRoundTripTime?: number;
  readonly frames?: number;
  readonly framesSent?: number;
  readonly framesReceived?: number;
  readonly framesRendered?: number;
  readonly freezeCount?: number;
}

export type StatsReport = Pick<ReadonlyMap<string, DiagnosticStats>, 'get' | 'values'>;

export interface CallQuality {
  readonly lossPercent: number | undefined;
  readonly jitterMs: number | undefined;
  readonly rttMs: number | undefined;
}

export interface SentShare {
  readonly captureFps: number | undefined;
  readonly sentFps: number | undefined;
  readonly size: string | undefined;
  readonly limitation: ShareLimitation | undefined;
  readonly codec: string | undefined;
}

export interface ReceivedShare {
  readonly receivedFps: number | undefined;
  readonly renderedFps: number | undefined;
  readonly size: string | undefined;
  readonly freezes: number | undefined;
}

function linked(report: StatsReport, id: string | undefined, type: string): DiagnosticStats | undefined {
  const stat = id === undefined ? undefined : report.get(id);
  return stat?.type === type ? stat : undefined;
}

function previousStream(previous: StatsReport | undefined, current: DiagnosticStats): DiagnosticStats | undefined {
  const old = current.id === undefined ? undefined : previous?.get(current.id);
  return old?.ssrc === current.ssrc && old?.type === current.type ? old : undefined;
}

function delta(current: number | undefined, previous: number | undefined): number | undefined {
  return count(current) && count(previous) && current >= previous ? current - previous : undefined;
}

function rate(frames: number | undefined, oldFrames: number | undefined, now: number | undefined, then: number | undefined): number | undefined {
  const elapsed = delta(now, then);
  const increase = delta(frames, oldFrames);
  return elapsed !== undefined && elapsed > 0 && increase !== undefined ? increase * 1000 / elapsed : undefined;
}

export function callQuality(report: StatsReport, previous?: StatsReport): CallQuality | undefined {
  const inbound = [...report.values()].find((stat) => stat.type === 'inbound-rtp' && (stat.kind ?? stat.mediaType) === 'audio');
  if (inbound === undefined) return undefined;
  const old = previousStream(previous, inbound);
  const received = delta(inbound.packetsReceived, old?.packetsReceived);
  const lost = inbound.packetsLost;
  const oldLost = old?.packetsLost;
  // Lost packets can arrive late and reduce the signed loss counter.
  const lostInInterval = lost !== undefined && oldLost !== undefined && Number.isFinite(lost) && Number.isFinite(oldLost) && lost >= oldLost
    ? lost - oldLost
    : undefined;
  const elapsed = delta(inbound.timestamp, old?.timestamp);
  const total = (received ?? 0) + (lostInInterval ?? 0);
  const lossPercent = elapsed !== undefined && elapsed > 0 && received !== undefined && lostInInterval !== undefined && total > 0
    ? lostInInterval / total * 100
    : undefined;
  const remote = linked(report, inbound.remoteId, 'remote-outbound-rtp');
  const transport = linked(report, inbound.transportId, 'transport');
  const pair = linked(report, transport?.selectedCandidatePairId, 'candidate-pair');
  // Receiver track reports may only have ICE RTT to the LiveKit server.
  const rtt = count(remote?.roundTripTime) ? remote.roundTripTime : pair?.currentRoundTripTime;
  return { lossPercent, jitterMs: count(inbound.jitter) ? inbound.jitter * 1000 : undefined, rttMs: count(rtt) ? rtt * 1000 : undefined };
}

function resolution(stat: DiagnosticStats): string | undefined {
  return count(stat.frameWidth) && stat.frameWidth > 0 && count(stat.frameHeight) && stat.frameHeight > 0
    ? `${String(stat.frameWidth)}×${String(stat.frameHeight)}`
    : undefined;
}

export function sentShare(report: StatsReport, previous?: StatsReport): SentShare | undefined {
  const outbound = shareVideo(report.values(), 'sent');
  if (outbound === undefined) return undefined;
  const old = previousStream(previous, outbound);
  const source = linked(report, outbound.mediaSourceId, 'media-source');
  const oldSource = previous === undefined ? undefined : linked(previous, outbound.mediaSourceId, 'media-source');
  const codec = linked(report, outbound.codecId, 'codec')?.mimeType;
  return {
    captureFps: count(source?.framesPerSecond) ? source.framesPerSecond : rate(source?.frames, oldSource?.frames, source?.timestamp, oldSource?.timestamp),
    sentFps: outbound.active === false ? 0 : rate(outbound.framesSent, old?.framesSent, outbound.timestamp, old?.timestamp),
    size: resolution(outbound),
    limitation: shareLimitation(outbound),
    codec: codec?.match(/^video\/([\w.-]+)$/i)?.[1],
  };
}

type Playback = Pick<VideoPlaybackQuality, 'creationTime' | 'totalVideoFrames' | 'droppedVideoFrames'>;
interface PlaybackSource {
  getVideoPlaybackQuality?: () => Playback;
}

export function receivedShare(report: StatsReport, previous?: StatsReport, playback?: Playback, oldPlayback?: Playback): ReceivedShare | undefined {
  const inbound = shareVideo(report.values(), 'received');
  if (inbound === undefined) return undefined;
  const old = previousStream(previous, inbound);
  // Some receivers omit framesRendered. The attached player's presented-frame
  // counter then gives the rate independently of network and decode rates.
  const played = (quality: Playback | undefined) => quality === undefined ? undefined : quality.totalVideoFrames - quality.droppedVideoFrames;
  return {
    receivedFps: rate(inbound.framesReceived, old?.framesReceived, inbound.timestamp, old?.timestamp),
    renderedFps: rate(inbound.framesRendered, old?.framesRendered, inbound.timestamp, old?.timestamp)
      ?? (old === undefined ? undefined : rate(played(playback), played(oldPlayback), playback?.creationTime, oldPlayback?.creationTime)),
    size: resolution(inbound),
    freezes: count(inbound.freezeCount) ? inbound.freezeCount : undefined,
  };
}

interface StatsTrack {
  getRTCStatsReport(): Promise<StatsReport | undefined>;
}

export type DiagnosticsSource =
  | { readonly kind: 'audio'; readonly track: StatsTrack; readonly principal: number }
  | { readonly kind: 'sent'; readonly track: StatsTrack }
  | { readonly kind: 'received'; readonly track: StatsTrack; readonly principal: number; readonly video: PlaybackSource | undefined };

export interface Diagnostics {
  readonly calls: ReadonlyMap<number, CallQuality>;
  readonly sent: SentShare | undefined;
  readonly received: ReadonlyMap<number, ReceivedShare>;
}

interface Reading {
  readonly report: StatsReport;
  readonly video: PlaybackSource | undefined;
  readonly playback: Playback | undefined;
}

type Sample = Reading & (
  | { readonly kind: 'audio'; readonly principal: number; readonly quality: CallQuality | undefined }
  | { readonly kind: 'sent'; readonly quality: SentShare | undefined }
  | { readonly kind: 'received'; readonly principal: number; readonly quality: ReceivedShare | undefined }
);

// After looking up the track, check that it still has the same role in the readout.
function sameRole(source: DiagnosticsSource | Sample, requested: DiagnosticsSource | undefined): boolean {
  return requested?.kind === source.kind
    && (requested.kind === 'sent' || ('principal' in source && requested.principal === source.principal));
}

// Start immediately, then sample every two seconds while open. Publish each
// source as it resolves; a source whose read is still pending is not asked again.
export function startDiagnostics(sources: () => readonly DiagnosticsSource[], publish: (stats: Diagnostics) => void): () => void {
  const latest = new Map<StatsTrack, Sample>();
  let stopped = false;
  // One read in flight per track: a slow getStats skips its own next ticks
  // and still counts when it lands, without holding up any other source.
  const pending = new Set<StatsTrack>();
  const requestedSources = () => new Map(sources().map((source) => [source.track, source]));
  const prune = (requested: ReadonlyMap<StatsTrack, DiagnosticsSource>) => {
    const size = latest.size;
    for (const [track, sample] of latest) {
      if (!sameRole(sample, requested.get(track))) latest.delete(track);
    }
    return latest.size !== size;
  };
  const publishLatest = () => {
    const calls = new Map<number, CallQuality>();
    const received = new Map<number, ReceivedShare>();
    let sent: SentShare | undefined;
    for (const sample of latest.values()) {
      if (sample.kind === 'sent') sent = sample.quality;
      else if (sample.quality !== undefined) {
        if (sample.kind === 'audio') calls.set(sample.principal, sample.quality);
        else received.set(sample.principal, sample.quality);
      }
    }
    publish({ calls, sent, received });
  };
  const read = async (source: DiagnosticsSource) => {
    const { track } = source;
    pending.add(track);
    const report = await track.getRTCStatsReport().catch(() => undefined);
    pending.delete(track);
    if (stopped) return;

    const requested = requestedSources();
    const removed = prune(requested);
    const current = requested.get(track);
    if (current === undefined || !sameRole(source, current)) {
      if (removed) publishLatest();
      return;
    }
    if (report === undefined) {
      latest.delete(track);
    } else {
      const old = latest.get(track);
      const video = current.kind === 'received' ? current.video : undefined;
      const playback = video?.getVideoPlaybackQuality?.();
      const reading = { report, video, playback };
      switch (current.kind) {
        case 'audio':
          latest.set(track, { ...reading, kind: current.kind, principal: current.principal, quality: callQuality(report, old?.report) });
          break;
        case 'sent':
          latest.set(track, { ...reading, kind: current.kind, quality: sentShare(report, old?.report) });
          break;
        case 'received': {
          const quality = receivedShare(report, old?.report, playback, old?.video === video ? old?.playback : undefined);
          latest.set(track, { ...reading, kind: current.kind, principal: current.principal, quality });
          break;
        }
      }
    }
    publishLatest();
  };
  const tick = () => {
    const requested = requestedSources();
    if (prune(requested) || requested.size === 0) publishLatest();
    for (const source of requested.values()) if (!pending.has(source.track)) void read(source);
  };
  tick();
  const timer = setInterval(tick, 2000);
  return () => {
    stopped = true;
    clearInterval(timer);
    latest.clear();
  };
}
