// The voice client: asks conchd for a session before every connection attempt,
// joins the room, owns the transmit gate and the report queue, and follows the
// disconnect table of conch-voice.md section 5. It knows the SDK only through
// the Room interface below (sdk.ts is the one file that imports it), so the
// tests drive it with a stub.
//
// The join token is passed to Room.connect and dropped. It is never stored,
// logged or put in the DOM.
//
// A press reports `started` and waits for conchd's answer before the track
// opens, and a release mutes the track before it reports `stopped` (issue
// #495). conchd's poller compares what it sees of the microphone with the
// reports; the other order let it see a press before its report, and record an
// honest press as unreported. The wait is bounded (reportWait): past it, or if
// the report is still being retried, the track opens anyway, so audio never
// depends on conchd being reachable (conch-voice.md section 6, "Client side").
//
// It also holds the screen share (issue #444): the sharer's state machine
// (share.ts), whether this page watches someone else's share, and the
// elements a watched share plays into.
//
// When the room is renewed (rotated, issue #516) every page reconnects to the
// new one, and a share in progress is resumed by its sharer (share.ts). A page
// watching it keeps watching: the share went with the room, so it waits a
// while for the same person's share to come back instead of letting go.
import type { ShareClaimResult, TransmitAnswer, VoiceSessionResult } from '../api';
import type { VoicePresenceV1, VoiceTransmitState } from '../schema/voice';
import type { Clock } from '../state/feed';
import { afterDisconnect, afterRefusal, retryDelay, type DisconnectCause, type Next } from './disconnect';
import { TransmitGate, type GateMode, type GateOptions } from './gate';
import { occupants, principalOf, sharer } from './presence';
import { ReportQueue, type ReportEvent } from './reports';
import { idleShare, resumeLimit, ScreenShare, type ScreenEnd, type ShareContent, type ShareRoom, type ShareState } from './share';
import { record } from '../diagnostics';

// The names a browser gives the failures of a microphone it was asked for
// (DOMException names). Any other error of a connection is not a device's.
const deviceErrors: ReadonlySet<string> = new Set(['NotAllowedError', 'NotFoundError', 'NotReadableError', 'OverconstrainedError', 'SecurityError', 'AbortError']);

export interface RoomEvents {
  readonly onReconnecting: () => void;
  readonly onReconnected: () => void;
  readonly onDisconnected: (cause: DisconnectCause) => void;
  // Principal ids of the active speakers, by the identity p<id>.
  readonly onSpeakers: (principals: readonly number[]) => void;
  // This page's published share is gone (share.ts).
  readonly onScreenEnded: (cause: ScreenEnd) => void;
  // Which of a watched share's tracks have arrived.
  readonly onShareTracks: (tracks: ShareTracks) => void;
}

export interface ShareTracks {
  readonly video: boolean;
  readonly audio: boolean;
}

// The elements a watched share plays into: its picture, and its sound, which
// has a volume and a mute of its own.
export interface ShareSink {
  readonly video: HTMLVideoElement;
  readonly audio: HTMLAudioElement;
}

// What the client needs of one LiveKit room connection.
export interface Room extends ShareRoom {
  // Resolves once the room is joined and, when publish is set, the
  // microphone track is published and muted. It rejects if any of that fails
  // (a microphone refused included).
  connect: (url: string, token: string, roomName: string, publish: boolean, events: RoomEvents) => Promise<void>;
  // Captures and publishes the microphone, muted, on a connection made
  // without it (watch only).
  publishMicrophone: () => Promise<void>;
  // Subscribes to a share's picture and sound, or lets them go. Microphones
  // are always subscribed.
  setWatching: (on: boolean) => void;
  // Plays a watched share into these elements, or into none.
  attachShare: (sink: ShareSink | null) => void;
  // Opens or closes the track: unmute or mute. Muted again after any republish.
  setSending: (on: boolean) => Promise<void>;
  setDeafened: (on: boolean) => void;
  // The microphone level, 0 to 1.
  level: () => number;
  close: () => Promise<void>;
}

export type Phase = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'unavailable' | 'stopped';

export interface VoiceState {
  readonly phase: Phase;
  readonly channel: string | null;
  // Why the client stopped, or what is wrong while it retries.
  readonly why: string | null;
  readonly mode: GateMode;
  readonly muted: boolean;
  readonly deafened: boolean;
  readonly listenOnly: boolean;
  // Joined to watch: connected without the microphone, until asked for it.
  readonly watchOnly: boolean;
  readonly transmitting: boolean;
  readonly level: number;
  readonly threshold: number;
  readonly speaking: ReadonlySet<number>;
  readonly reportNote: 'retrying' | 'rate_limited' | null;
  readonly share: ShareState;
  // Whether this page watches the room's share, and which of its tracks
  // have arrived.
  readonly watching: boolean;
  readonly shareTracks: ShareTracks;
}

export interface VoiceEnv {
  readonly clock: Clock;
  readonly random: () => number;
  readonly createSession: (channel: string) => Promise<VoiceSessionResult>;
  readonly report: (channel: string, state: VoiceTransmitState, signal: AbortSignal) => Promise<TransmitAnswer>;
  readonly newRoom: () => Room;
  readonly claimShare: (channel: string) => Promise<ShareClaimResult>;
  // Releases the share whose claim started at startedAt (issue #487).
  readonly stopShare: (channel: string, startedAt: string) => Promise<unknown>;
  readonly gate?: GateOptions;
}

// How long the SDK's own reconnect is allowed before the client goes back to
// conchd (conch-voice.md section 5).
export const reconnectLimit = 20_000;
// A connection that lasted this long starts the retry wait over.
export const healthyAfter = 30_000;
// How often the microphone level is read.
export const levelEvery = 50;
// How long a Leave waits for the last report.
const leaveDrain = 1000;
// The longest a press waits for the answer to its `started` report before the
// track opens anyway, and the longest the release of a press waits for the
// track to mute before `stopped` is reported.
export const reportWait = 1000;

export const initialVoice: VoiceState = {
  phase: 'idle',
  channel: null,
  why: null,
  mode: 'ptt',
  muted: false,
  deafened: false,
  listenOnly: false,
  watchOnly: false,
  transmitting: false,
  level: 0,
  threshold: 0.1,
  speaking: new Set(),
  reportNote: null,
  share: idleShare,
  watching: false,
  shareTracks: { video: false, audio: false },
};

const noTracks: ShareTracks = { video: false, audio: false };

export class VoiceClient {
  private readonly env: VoiceEnv;
  private readonly gate: TransmitGate;
  private readonly reports: ReportQueue;
  private state: VoiceState = initialVoice;
  private listeners = new Set<() => void>();
  private room: Room | null = null;
  private channel: string | null = null;
  // Each attempt has a number; answers for an older one are discarded.
  private attempt = 0;
  private failures = 0;
  private retryTimer: unknown = null;
  private reconnectTimer: unknown = null;
  private levelTimer: unknown = null;
  private connectedAt = 0;
  private now: () => number;
  private readonly share: ScreenShare;
  // This person, from the identity of the last session.
  private me: number | null = null;
  private sink: ShareSink | null = null;
  // Who presence last showed sharing, and whose share this page watches.
  // awaiting is the timer of a watched share that went with its room: the
  // watching ends if the share is not back by then.
  private holder: number | null = null;
  private watched: number | null = null;
  private awaiting: unknown = null;
  // The gate's changes, carried out one after another: a press's wait for its
  // answer, then a release's report. talk numbers the run of them a
  // connection makes (a drop abandons it); edge counts the gate's changes, so
  // that a press released while it waited does not open the track.
  private steps: Promise<void> = Promise.resolve();
  private talk = 0;
  private edge = 0;

  constructor(env: VoiceEnv, now: () => number = () => Date.now()) {
    this.env = env;
    this.now = now;
    this.gate = new TransmitGate(env.clock, (open) => {
      this.onGate(open);
    }, env.gate);
    this.share = new ScreenShare(
      { claim: env.claimShare, release: env.stopShare },
      (share) => {
        this.set({ share });
      },
      env.clock,
    );
    this.reports = new ReportQueue({
      send: (state, signal) => {
        const channel = this.channel;
        return channel === null ? Promise.resolve('no_session') : env.report(channel, state, signal);
      },
      clock: env.clock,
      onEvent: (event) => {
        this.onReport(event);
      },
    });
  }

  // useSyncExternalStore's pair.
  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };
  snapshot = (): VoiceState => this.state;

  private set(patch: Partial<VoiceState>): void {
    // Each change of phase goes into the page's diagnostic ring (issue
    // #446): the state alone, never the channel or the reason's words.
    if (patch.phase !== undefined && patch.phase !== this.state.phase) record({ kind: 'voice_state', state: patch.phase });
    this.state = { ...this.state, ...patch };
    for (const l of [...this.listeners]) l();
  }

  // join connects to a channel's voice. Watch only connects without the
  // microphone: nothing is captured or published until useMicrophone.
  join(channel: string, options: { readonly watchOnly?: boolean } = {}): void {
    if (this.state.phase !== 'idle' && this.state.phase !== 'stopped') return;
    this.channel = channel;
    this.failures = 0;
    this.set({ phase: 'connecting', channel, why: null, reportNote: null, watchOnly: options.watchOnly === true, watching: false, shareTracks: noTracks });
    void this.connect();
  }

  // useMicrophone ends watch only: the microphone is captured, published and
  // muted, as on a full join, and the talk controls work from then on.
  async useMicrophone(): Promise<void> {
    const room = this.room;
    if (!this.state.watchOnly || this.state.listenOnly || this.state.phase !== 'connected' || room === null) return;
    const mine = this.attempt;
    this.set({ watchOnly: false });
    try {
      await room.publishMicrophone();
    } catch {
      if (mine !== this.attempt) return;
      this.set({ watchOnly: true, why: 'The microphone could not be used' });
      return;
    }
    if (mine === this.attempt) this.applyBlock();
  }

  // startShare shares a screen into the room this page is connected to.
  startShare(content: ShareContent): void {
    const channel = this.channel;
    const room = this.room;
    if (this.state.phase !== 'connected' || channel === null || room === null) return;
    void this.share.start(channel, room, content);
  }

  stopShare(): void {
    void this.share.stop('self');
  }

  // watch subscribes to the room's share, or lets it go.
  watch(on: boolean): void {
    this.endAwaiting();
    this.watched = on ? this.holder : null;
    this.room?.setWatching(on);
    this.set(on ? { watching: true } : { watching: false, shareTracks: noTracks });
  }

  // attachShare plays a watched share into the view's elements, on this
  // connection and on every one after it.
  attachShare(sink: ShareSink | null): void {
    this.sink = sink;
    this.room?.attachShare(sink);
  }

  // presence is each presence document the voice socket sends: whether this
  // person still holds the share, whether anyone does, and, in this person's
  // own document alone, why the server ended their last share.
  presence(p: VoicePresenceV1): void {
    const holder = sharer(p);
    this.share.presence(holder === null ? 'none' : holder.principal === this.me ? 'me' : 'other', p.share_ended);
    this.holder = holder?.principal ?? null;
    if (this.state.watching) this.follow(holder?.principal ?? null, occupants(p).some((o) => o.principal === this.me));
  }

  // follow keeps a watching page on the share it watches. A share gone while
  // this person is still listed in the room has ended. A share gone with
  // this person (the room was renewed, and everyone left it) is waited for:
  // the same person's share coming back is watched as before, anyone else's
  // is not.
  private follow(holder: number | null, listed: boolean): void {
    if (holder !== null) {
      const waited = this.awaiting !== null;
      this.endAwaiting();
      if (waited && holder !== this.watched) this.watch(false);
      else this.watched = holder;
      return;
    }
    if (this.awaiting !== null) return;
    if (listed || this.watched === null) {
      this.watch(false);
      return;
    }
    this.awaitShare();
  }

  // awaitShare waits for a watched share that went with its room.
  private awaitShare(): void {
    if (this.awaiting !== null) return;
    this.awaiting = this.env.clock.setTimeout(() => {
      this.awaiting = null;
      this.watch(false);
    }, resumeLimit);
  }

  private endAwaiting(): void {
    if (this.awaiting !== null) this.env.clock.clearTimeout(this.awaiting);
    this.awaiting = null;
  }

  // leave shuts the gate, lets the last report go, and disconnects.
  async leave(): Promise<void> {
    const mine = ++this.attempt;
    this.clearTimers();
    this.gate.setBlocked(true);
    // Leaving ends a share: it is stopped and conchd is told, while the
    // connection and the credential are still there.
    const sharing = this.share.stop('left');
    // The release's `stopped` is queued once its mute is done.
    await this.within(this.steps, leaveDrain);
    await this.reports.drain(leaveDrain);
    await sharing;
    if (mine !== this.attempt) return;
    this.talk++;
    this.steps = Promise.resolve();
    this.reports.reset();
    const room = this.room;
    this.room = null;
    this.channel = null;
    this.endAwaiting();
    this.watched = null;
    this.set({ phase: 'idle', channel: null, why: null, transmitting: false, level: 0, speaking: new Set(), reportNote: null, watching: false, shareTracks: noTracks });
    if (room !== null) await room.close().catch(() => undefined);
  }

  setMode(mode: GateMode): void {
    this.gate.setMode(mode);
    this.set({ mode });
  }

  setMuted(muted: boolean): void {
    this.set({ muted });
    this.applyBlock();
  }

  setDeafened(deafened: boolean): void {
    this.room?.setDeafened(deafened);
    this.set({ deafened });
    this.applyBlock();
  }

  setThreshold(threshold: number): void {
    this.gate.setThreshold(threshold);
    this.set({ threshold });
  }

  press(): void {
    this.gate.press();
  }

  release(): void {
    this.gate.release();
  }

  // lost is losing focus or visibility: a release.
  lost(): void {
    this.gate.release();
  }

  dispose(): void {
    void this.leave();
    this.listeners.clear();
  }

  private applyBlock(): void {
    const s = this.state;
    this.gate.setBlocked(s.phase !== 'connected' || s.muted || s.deafened || s.listenOnly || s.watchOnly);
  }

  private clearTimers(): void {
    for (const t of [this.retryTimer, this.reconnectTimer, this.levelTimer]) {
      if (t !== null) this.env.clock.clearTimeout(t);
    }
    this.retryTimer = null;
    this.reconnectTimer = null;
    this.levelTimer = null;
  }

  private onGate(open: boolean): void {
    const talk = this.talk;
    const edge = ++this.edge;
    if (open) {
      this.steps = this.steps.then(() => this.opened(talk, edge));
    } else {
      // The track is muted at once, and `stopped` reported once it is.
      const muted = (this.room?.setSending(false) ?? Promise.resolve()).catch(() => undefined);
      this.steps = this.steps.then(() => this.shut(talk, muted));
    }
    this.set({ transmitting: open });
  }

  // opened reports `started` and opens the track once conchd has answered,
  // or after reportWait, unless the gate has changed since or the connection
  // has gone. A report that fails for good drops the connection first.
  private async opened(talk: number, edge: number): Promise<void> {
    if (talk !== this.talk) return;
    const answered = new Promise<void>((resolve) => {
      this.reports.push('started', resolve);
    });
    await this.within(answered, reportWait);
    const room = this.room;
    if (talk !== this.talk || edge !== this.edge || room === null) return;
    await this.within(room.setSending(true), reportWait);
  }

  private async shut(talk: number, muted: Promise<void>): Promise<void> {
    await this.within(muted, reportWait);
    if (talk === this.talk) this.reports.push('stopped');
  }

  // within resolves when p settles, however it settles, or after ms.
  private within(p: Promise<unknown>, ms: number): Promise<void> {
    const clock = this.env.clock;
    return new Promise((resolve) => {
      const handle = clock.setTimeout(resolve, ms);
      const done = () => {
        clock.clearTimeout(handle);
        resolve();
      };
      void p.then(done, done);
    });
  }

  private onReport(event: ReportEvent): void {
    switch (event) {
      case 'ok':
        if (this.state.reportNote !== null) this.set({ reportNote: null });
        break;
      case 'retrying':
      case 'rate_limited':
        this.set({ reportNote: event });
        break;
      case 'gave_up':
      case 'no_session':
        // Drop the connection, so nothing is delivered late, and ask conchd.
        this.drop({ kind: 'new_session' });
        break;
      case 'unauthorized':
        this.drop({ kind: 'stop', why: 'You are signed out' });
        break;
    }
  }

  // drop ends the current connection and acts on `next`. roomDeleted: LiveKit
  // deleted the room, as conchd does when it renews one.
  private drop(next: Next, roomDeleted = false): void {
    const mine = ++this.attempt;
    this.clearTimers();
    this.gate.setBlocked(true);
    // Nothing the gate asked for on this connection is carried out from here:
    // no track opens, and no `stopped` goes behind the reset.
    this.talk++;
    this.steps = Promise.resolve();
    this.reports.reset();
    // A share does not outlive its connection: the next one starts without it,
    // unless its room was renewed, when the next connection resumes it. A
    // client that stops resumes nothing.
    if (next.kind === 'stop') void this.share.stop('lost');
    else void this.share.lost(roomDeleted);
    // A watched share goes with a deleted room, and is waited for, whether
    // presence has said so yet or not.
    if (roomDeleted && next.kind !== 'stop' && this.state.watching && this.watched !== null) this.awaitShare();
    const room = this.room;
    this.room = null;
    this.set({ speaking: new Set(), level: 0, transmitting: false, shareTracks: noTracks });
    if (room !== null) void room.close().catch(() => undefined);
    this.act(next, mine);
  }

  private act(next: Next, mine: number): void {
    switch (next.kind) {
      case 'stop':
        this.share.abandon();
        this.set({ phase: 'stopped', why: next.why });
        break;
      case 'new_session':
        this.set({ phase: 'connecting', why: null });
        void this.connect(mine);
        break;
      case 'retry': {
        const wait = retryDelay(this.failures++, this.env.random);
        this.set({ phase: 'unavailable', why: 'Voice is unavailable; trying again' });
        this.retryTimer = this.env.clock.setTimeout(() => {
          this.retryTimer = null;
          if (mine !== this.attempt) return;
          this.set({ phase: 'connecting' });
          void this.connect(mine);
        }, wait);
        break;
      }
    }
  }

  private async connect(known?: number): Promise<void> {
    const mine = known ?? ++this.attempt;
    const channel = this.channel;
    if (channel === null) return;
    const session = await this.env.createSession(channel);
    if (mine !== this.attempt) return;
    if (!session.ok) {
      this.act(afterRefusal(session), mine);
      return;
    }
    const grant = session.value.rooms.find((g) => g.audience === undefined);
    if (grant === undefined) {
      this.act({ kind: 'stop', why: 'This channel has no voice room for everyone' }, mine);
      return;
    }
    this.me = principalOf(session.value.identity);
    const room = this.env.newRoom();
    this.room = room;
    const events: RoomEvents = {
      onReconnecting: () => {
        if (mine !== this.attempt) return;
        this.set({ phase: 'reconnecting' });
        this.applyBlock();
        this.reconnectTimer = this.env.clock.setTimeout(() => {
          this.reconnectTimer = null;
          if (mine !== this.attempt) return;
          // The SDK did not recover in time: close it and let conchd decide.
          this.drop({ kind: 'new_session' });
        }, reconnectLimit);
      },
      onReconnected: () => {
        if (mine !== this.attempt) return;
        if (this.reconnectTimer !== null) this.env.clock.clearTimeout(this.reconnectTimer);
        this.reconnectTimer = null;
        this.set({ phase: 'connected' });
        this.applyBlock();
        void this.share.reconnected();
      },
      onDisconnected: (cause) => {
        if (mine !== this.attempt) return;
        this.drop(afterDisconnect(cause), cause === 'room_deleted');
      },
      onSpeakers: (ids) => {
        if (mine !== this.attempt) return;
        this.set({ speaking: new Set(ids) });
      },
      onScreenEnded: (cause) => {
        if (mine !== this.attempt) return;
        this.share.ended(cause);
      },
      onShareTracks: (tracks) => {
        if (mine !== this.attempt) return;
        this.set({ shareTracks: tracks });
      },
    };
    // What a share needs is set before connecting, so that a track that
    // arrives with the connection already finds it.
    room.setWatching(this.state.watching);
    room.attachShare(this.sink);
    try {
      await room.connect(session.value.livekit_url, grant.token, grant.room, grant.can_publish && !this.state.watchOnly, events);
    } catch (err: unknown) {
      // A microphone refused or missing is said by the error's name alone.
      // A DOMException is not an Error everywhere, so its name is read as a
      // property.
      const name = typeof err === 'object' && err !== null && 'name' in err ? err.name : undefined;
      if (typeof name === 'string' && deviceErrors.has(name)) record({ kind: 'device_error', name });
      if (mine !== this.attempt) return;
      void room.close().catch(() => undefined);
      this.room = null;
      this.act({ kind: 'retry' }, mine);
      return;
    }
    if (mine !== this.attempt) {
      void room.close().catch(() => undefined);
      return;
    }
    room.setDeafened(this.state.deafened);
    this.connectedAt = this.now();
    this.set({ phase: 'connected', why: null, listenOnly: !grant.can_publish });
    this.applyBlock();
    this.watchLevel(mine, room);
    this.share.connected(channel, room);
    // A connection that lasts starts the retry wait over.
    this.env.clock.setTimeout(() => {
      if (mine === this.attempt && this.now() - this.connectedAt >= healthyAfter) this.failures = 0;
    }, healthyAfter);
  }

  private watchLevel(mine: number, room: Room): void {
    const tick = () => {
      if (mine !== this.attempt) return;
      const raw = room.level();
      // Quantised, so a meter does not re-render the page at every read.
      const level = Math.round(raw * 20) / 20;
      this.gate.level(raw);
      if (level !== this.state.level) this.set({ level });
      this.levelTimer = this.env.clock.setTimeout(tick, levelEvery);
    };
    this.levelTimer = this.env.clock.setTimeout(tick, levelEvery);
  }
}
