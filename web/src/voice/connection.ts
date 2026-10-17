// The voice client: asks conchd for a session before every connection attempt,
// joins the room, owns the transmit gate and the report queue, and follows the
// disconnect table of conch-voice.md section 5. It knows the SDK only through
// the Room interface below (sdk.ts is the one file that imports it), so the
// tests drive it with a stub.
//
// The join token is passed to Room.connect and dropped. It is never stored,
// logged or put in the DOM.
import type { TransmitAnswer, VoiceSessionResult } from '../api';
import type { VoiceTransmitState } from '../schema/voice';
import type { Clock } from '../state/feed';
import { afterDisconnect, afterRefusal, retryDelay, type DisconnectCause, type Next } from './disconnect';
import { TransmitGate, type GateMode, type GateOptions } from './gate';
import { ReportQueue, type ReportEvent } from './reports';
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
}

// What the client needs of one LiveKit room connection.
export interface Room {
  // Resolves once the room is joined, the microphone track is published and
  // muted. It rejects if any of that fails (a microphone refused included).
  connect: (url: string, token: string, roomName: string, publish: boolean, events: RoomEvents) => Promise<void>;
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
  readonly transmitting: boolean;
  readonly level: number;
  readonly threshold: number;
  readonly speaking: ReadonlySet<number>;
  readonly reportNote: 'retrying' | 'rate_limited' | null;
}

export interface VoiceEnv {
  readonly clock: Clock;
  readonly random: () => number;
  readonly createSession: (channel: string) => Promise<VoiceSessionResult>;
  readonly report: (channel: string, state: VoiceTransmitState, signal: AbortSignal) => Promise<TransmitAnswer>;
  readonly newRoom: () => Room;
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

export const initialVoice: VoiceState = {
  phase: 'idle',
  channel: null,
  why: null,
  mode: 'ptt',
  muted: false,
  deafened: false,
  listenOnly: false,
  transmitting: false,
  level: 0,
  threshold: 0.1,
  speaking: new Set(),
  reportNote: null,
};

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

  constructor(env: VoiceEnv, now: () => number = () => Date.now()) {
    this.env = env;
    this.now = now;
    this.gate = new TransmitGate(env.clock, (open) => {
      this.onGate(open);
    }, env.gate);
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

  join(channel: string): void {
    if (this.state.phase !== 'idle' && this.state.phase !== 'stopped') return;
    this.channel = channel;
    this.failures = 0;
    this.set({ phase: 'connecting', channel, why: null, reportNote: null });
    void this.connect();
  }

  // leave shuts the gate, lets the last report go, and disconnects.
  async leave(): Promise<void> {
    const mine = ++this.attempt;
    this.clearTimers();
    this.gate.setBlocked(true);
    await this.reports.drain(leaveDrain);
    if (mine !== this.attempt) return;
    this.reports.reset();
    const room = this.room;
    this.room = null;
    this.channel = null;
    this.set({ phase: 'idle', channel: null, why: null, transmitting: false, level: 0, speaking: new Set(), reportNote: null });
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
    this.gate.setBlocked(s.phase !== 'connected' || s.muted || s.deafened || s.listenOnly);
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
    // The track follows the gate at once; the report does not hold it up.
    void this.room?.setSending(open).catch(() => undefined);
    this.reports.push(open ? 'started' : 'stopped');
    this.set({ transmitting: open });
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

  // drop ends the current connection and acts on `next`.
  private drop(next: Next): void {
    const mine = ++this.attempt;
    this.clearTimers();
    this.gate.setBlocked(true);
    this.reports.reset();
    const room = this.room;
    this.room = null;
    this.set({ speaking: new Set(), level: 0, transmitting: false });
    if (room !== null) void room.close().catch(() => undefined);
    this.act(next, mine);
  }

  private act(next: Next, mine: number): void {
    switch (next.kind) {
      case 'stop':
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
      },
      onDisconnected: (cause) => {
        if (mine !== this.attempt) return;
        this.drop(afterDisconnect(cause));
      },
      onSpeakers: (ids) => {
        if (mine !== this.attempt) return;
        this.set({ speaking: new Set(ids) });
      },
    };
    try {
      await room.connect(session.value.livekit_url, grant.token, grant.room, grant.can_publish, events);
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
