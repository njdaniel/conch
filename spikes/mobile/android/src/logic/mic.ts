// The microphone's publish order and the transmit reports around the gate,
// from web/src/voice/sdk.ts (capturePublish, #501) and connection.ts (opened,
// shut) at ed4a68f9, without the SDK. M4-C wires a real Room to these.
import type { Clock } from './clock';
import type { VoiceTransmitState } from './schema/generated';

// Anything that can be muted: a local track, or its publication.
export interface Mutable {
  mute: () => Promise<void>;
}

// The SDK's publishTrack, as the order needs it.
export interface Publisher<P extends Mutable> {
  publish: (track: Mutable) => Promise<P>;
}

// publishMicrophone is the order of #501: the track is muted before it is
// published, so that it is listed muted from the first; the publication is
// muted again after. A republish (a device switch, a new capture) goes through
// here again, in the same order.
export async function publishMicrophone<P extends Mutable>(track: Mutable, publisher: Publisher<P>): Promise<P> {
  await track.mute();
  const publication = await publisher.publish(track);
  await publication.mute();
  return publication;
}

// How long a press waits for the answer to its `started` report before the
// track is unmuted, and how long a shut waits for the mute before `stopped`.
export const reportWait = 1000;

export interface ReportSink {
  // The ReportQueue's push.
  push: (state: VoiceTransmitState, done?: () => void) => void;
}

export interface TransmitterOptions {
  readonly clock: Clock;
  readonly reports: ReportSink;
  // Unmutes (true) or mutes (false) the track and its publication.
  readonly setSending: (sending: boolean) => Promise<void>;
  // Called when audio is being sent (the track is unmuted) and when it stops.
  readonly onTransmitting?: (transmitting: boolean) => void;
}

// Transmitter carries out the gate's changes one after another: `started` is
// answered (or reportWait passes) before the unmute; on a shut the track is
// muted at once and `stopped` is sent after the mute.
export class Transmitter {
  private readonly options: TransmitterOptions;
  private steps: Promise<void> = Promise.resolve();
  private talk = 0;
  private edge = 0;

  constructor(options: TransmitterOptions) {
    this.options = options;
  }

  // reset abandons what the gate asked for on a connection that has gone.
  reset(): void {
    this.talk++;
  }

  // onGate is the gate's onChange.
  onGate(open: boolean): void {
    const talk = this.talk;
    const edge = ++this.edge;
    if (open) {
      this.steps = this.steps.then(() => this.opened(talk, edge));
      return;
    }
    // Muted at once; `stopped` once it is.
    const muted = this.options.setSending(false).catch(() => undefined);
    this.steps = this.steps.then(() => this.shut(talk, muted));
    this.options.onTransmitting?.(false);
  }

  // idle resolves when the steps so far are done.
  idle(): Promise<void> {
    return this.steps;
  }

  private async opened(talk: number, edge: number): Promise<void> {
    if (talk !== this.talk) return;
    const answered = new Promise<void>((resolve) => {
      this.options.reports.push('started', resolve);
    });
    await this.within(answered, reportWait);
    if (talk !== this.talk || edge !== this.edge) return;
    await this.within(
      this.options.setSending(true).then(() => {
        if (talk === this.talk && edge === this.edge) this.options.onTransmitting?.(true);
      }),
      reportWait,
    );
  }

  private async shut(talk: number, muted: Promise<void>): Promise<void> {
    await this.within(muted, reportWait);
    if (talk === this.talk) this.options.reports.push('stopped');
  }

  private within(p: Promise<unknown>, ms: number): Promise<void> {
    const clock = this.options.clock;
    return new Promise((resolve) => {
      const handle = clock.setTimeout(resolve, ms);
      const done = () => {
        clock.clearTimeout(handle);
        resolve();
      };
      void p.then(done, done);
    });
  }
}
