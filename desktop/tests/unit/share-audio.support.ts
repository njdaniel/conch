// A rig for the Linux screen share's sound (issue #1854): the real Companion,
// the real VoiceService and the real ShareSound over a fake helper (which
// writes its commands through the real protocol encoder) and a fake conchd.
// Every effect lands, in order, in one list (`events`), so a test asserts the
// order the design requires: consent before command, command before DELETE.
// No network, no browser, no helper process.
import { expect } from 'vitest';

import type { ShareSoundChoice, ShareSoundView } from '../../../web/src/platform/desktop/bridge.ts';
import type { VoiceCompanionResponseV1 } from '../../../web/src/schema/generated.ts';
import type { Exchange, Send } from '../../main/api/client.ts';
import { Companion, type CompanionClient } from '../../main/voice/companion.ts';
import { SCHEMA_V1, SCHEMA_V2, SCHEMA_V3, command, encodeCommand, type Version, type Want } from '../../main/voice/protocol.ts';
import { VoiceService, type VoiceIdentity, type VoiceServiceDeps, type VoiceSupervisor } from '../../main/voice/service.ts';
import { ShareSound, type ShareSoundDeps } from '../../main/voice/share-audio.ts';
import type { CommandResult, HelperView, ShareAudioView, VoiceState } from '../../main/voice/supervisor.ts';
import { readFixture } from './voice-support.ts';

export const ANSWER = readFixture<VoiceCompanionResponseV1>('voice-companion-response-v1.json');
export const STARTED = '2026-11-16T12:00:00Z';
export const DOC = 5;

export const ok = (body?: unknown): Exchange => ({ kind: 'answer', status: body === undefined ? 204 : 200, ok: true, ...(body === undefined ? {} : { body }) });
export const refusal = (status: number, code?: string): Exchange => ({ kind: 'answer', status, ok: false, ...(code === undefined ? {} : { body: { code, message: 'no' } }) });
export const consent = (started: string = STARTED, over: Record<string, unknown> = {}): Exchange => ok({ scope: 'system_excluding_call', publisher: 'person', started_at: started, ...over });

export function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

// Lets every promise and zero-delay timer that is ready run.
export const settle = (): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, 15);
  });

const OFF: ShareAudioView = { state: 'off', muted: false, sources: 0 };

// The helper, as the supervisor presents it: it writes each command through
// the protocol's own encoder (so a scope or a name that the wire would refuse
// throws here) and says in `events` what it was asked, in order.
export class FakeSupervisor implements VoiceSupervisor {
  state: VoiceState = { status: 'idle' };
  version: 1 | 2 | 3 = 3;
  phase: HelperView['phase'] = 'connected';
  reason: HelperView['reason'];
  audio: ShareAudioView = OFF;
  micMuted = false;
  deafened = false;
  // A helper that is gone takes no command: the supervisor says not-running.
  alive = true;
  // Every command line written, decoded.
  readonly wire: Record<string, unknown>[] = [];
  captures = 0;
  // What the helper does with set_share_audio {capturing: true}: the call's
  // number, and the state it settles into before the ack.
  onCapture: (call: number) => Promise<CommandResult> | CommandResult = () => {
    this.audio = { state: 'on', muted: this.audio.muted, sources: 2 };
    this.emit();
    return { ok: true };
  };
  onStop: () => Promise<CommandResult> | CommandResult = () => {
    this.audio = { ...OFF, muted: this.audio.muted };
    this.emit();
    return { ok: true };
  };
  private nextId = 2;
  private readonly listeners = new Set<(s: VoiceState) => void>();

  constructor(private readonly events: string[]) {}

  subscribe(listener: (s: VoiceState) => void): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  view(): HelperView {
    return {
      version: this.version,
      helperVersion: '0.3.0',
      phase: this.phase,
      ...(this.reason === undefined ? {} : { reason: this.reason }),
      capture: 'on',
      playback: 'on',
      transmitting: false,
      muted: this.micMuted,
      deafened: this.deafened,
      blocked: [],
      talkMode: 'push_to_talk',
      ptt: 'ready',
      ...(this.version === 3 ? { shareAudio: this.audio } : {}),
    };
  }

  private publish(): void {
    for (const l of [...this.listeners]) l(this.state);
  }

  // emit says the helper's state changed.
  emit(): void {
    if (this.state.status === 'running') this.state = { status: 'running', generation: 3, helper: this.view() };
    this.publish();
  }

  // lose is the helper going away: the supervisor's one `lost` state.
  lose(): void {
    this.alive = false;
    this.state = { status: 'lost', generation: 3, reason: 'exited' };
    this.publish();
  }

  start(): Promise<VoiceState> {
    this.state = { status: 'running', generation: 3, helper: this.view() };
    return Promise.resolve(this.state);
  }

  stop(): Promise<void> {
    if (this.alive && this.state.status === 'running') this.events.push('helper:leave');
    this.alive = false;
    this.state = { status: 'idle' };
    this.publish();
    return Promise.resolve();
  }

  setMute(muted: boolean): Promise<CommandResult> {
    this.events.push(`helper:mic-mute ${String(muted)}`);
    this.micMuted = muted;
    this.emit();
    return Promise.resolve({ ok: true });
  }

  setDeafen(deafened: boolean): Promise<CommandResult> {
    this.events.push(`helper:deafen ${String(deafened)}`);
    this.deafened = deafened;
    this.emit();
    return Promise.resolve({ ok: true });
  }

  setDevices(): Promise<CommandResult> {
    return Promise.resolve({ ok: true });
  }

  setTalkMode(): Promise<CommandResult> {
    return Promise.resolve({ ok: true });
  }

  private schema(): Version {
    return this.version === 3 ? SCHEMA_V3 : this.version === 2 ? SCHEMA_V2 : SCHEMA_V1;
  }

  private write(want: Want): boolean {
    const c = command(this.schema(), this.nextId++, want);
    if (c === null) return false;
    this.wire.push(JSON.parse(encodeCommand(c).toString()) as Record<string, unknown>);
    return true;
  }

  setShareAudio(want: { readonly capturing: false } | { readonly capturing: true; readonly applicationName: string }): Promise<CommandResult> {
    if (!this.alive || this.state.status !== 'running') return Promise.resolve({ ok: false, error: 'not-running' });
    const w: Want = want.capturing
      ? { kind: 'set_share_audio', capturing: true, scope: 'system_excluding_call', applicationName: want.applicationName }
      : { kind: 'set_share_audio', capturing: false };
    if (!this.write(w)) return Promise.resolve({ ok: false, error: 'unsupported' });
    this.events.push(want.capturing ? 'helper:capture on' : 'helper:capture off');
    return Promise.resolve(want.capturing ? this.onCapture(++this.captures) : this.onStop());
  }

  setShareAudioMute(muted: boolean): Promise<CommandResult> {
    if (!this.alive || this.state.status !== 'running') return Promise.resolve({ ok: false, error: 'not-running' });
    if (!this.write({ kind: 'set_share_audio_mute', muted })) return Promise.resolve({ ok: false, error: 'unsupported' });
    this.events.push(`helper:share-mute ${String(muted)}`);
    this.audio = { ...this.audio, muted };
    this.emit();
    return Promise.resolve({ ok: true });
  }
}

export interface Rig {
  readonly events: string[];
  readonly logs: string[];
  readonly sup: FakeSupervisor;
  readonly voice: VoiceService;
  readonly companion: Companion;
  readonly sound: ShareSound | null;
  readonly prefs: { choice: ShareSoundChoice; saves: ShareSoundChoice[]; failSave: boolean };
  readonly sends: Send[];
  // What the fake conchd answers next, per route, before its default: the
  // consent answers 200 for the one scope and publisher; every DELETE 204.
  readonly put: (Exchange | Promise<Exchange>)[];
  readonly withdraw: (Exchange | Promise<Exchange>)[];
  readonly removal: (Exchange | Promise<Exchange>)[];
  // Main's own presence reads (issue #1876): each read takes the next
  // answer; with none queued it fails (says nothing).
  readonly presence: (Exchange | Promise<Exchange>)[];
  // A rename of the channel (issue #1880): requests that name `from` get 404
  // channel_not_found, and the channel list answers `list` (none: it fails).
  readonly rename: { from: string | null; list: Exchange | null };
  readonly client: CompanionClient;
  readonly views: ReturnType<VoiceService['state']>[];
  // Joins voice, gets the companion and has main grant the capture of a pick
  // (ShareSound.captureGranted, as display-capture.ts does when the portal
  // answers with a source): a live share, with the log cleared.
  live(): Promise<void>;
  // Joins voice and gets the companion only: a watch, which is no share. The
  // log is cleared.
  watch(): Promise<void>;
  // The share's sound view, as the page sees it.
  seen(): ShareSoundView | undefined;
}

export interface RigOptions {
  readonly sound?: boolean;
  readonly version?: 1 | 2 | 3;
  readonly choice?: ShareSoundChoice;
  readonly consentRetryMs?: number;
  readonly publishRetryMs?: number;
  readonly stopAckBoundMs?: number;
  readonly presencePollMs?: number;
  // The channel_id the fake conchd puts in the grant answer (issue #1883); any
  // value, valid or not. Absent: the answer carries none, as an older server's.
  readonly grantChannelId?: number;
}

export function rig(opts: RigOptions = {}): Rig {
  const events: string[] = [];
  const logs: string[] = [];
  const sends: Send[] = [];
  const put: Rig['put'] = [];
  const withdraw: Rig['withdraw'] = [];
  const removal: Rig['removal'] = [];
  const presence: Rig['presence'] = [];
  const rename: Rig['rename'] = { from: null, list: null };
  const sup = new FakeSupervisor(events);
  sup.version = opts.version ?? 3;
  const prefs: Rig['prefs'] = { choice: opts.choice ?? 'system_excluding_call', saves: [], failSave: false };
  const identity: VoiceIdentity = {
    profileId: 'p1',
    configDir: '/nonexistent/p1',
    principalId: 7,
    credential: () => ({ origin: 'https://conch.example.org', token: 'TOKEN-SECRET' }),
    current: () => true,
  };
  const oneShot = { devices: () => Promise.resolve({ ok: false, error: 'unsupported' }), save: () => Promise.resolve({ ok: true }), cancel: () => Promise.resolve() };
  const voice = new VoiceService({
    supervisor: sup,
    oneShot: oneShot as unknown as VoiceServiceDeps['oneShot'],
    identity: () => Promise.resolve(identity),
    log: (line) => logs.push(line),
    locked: () => false,
    stopBoundMs: 2000,
  });
  const client: CompanionClient = {
    send: (request) => {
      sends.push(request);
      switch (request.op) {
        case 'voiceViewer':
          events.push('http:POST companion');
          return Promise.resolve(ok(opts.grantChannelId === undefined ? ANSWER : { ...ANSWER, channel_id: opts.grantChannelId }));
        case 'voiceViewerEnd':
          if (rename.from !== null && request.params?.['channel'] === rename.from) {
            events.push('http:DELETE companion (old name)');
            return Promise.resolve(refusal(404, 'channel_not_found'));
          }
          events.push('http:DELETE companion');
          return Promise.resolve(removal.shift() ?? ok());
        case 'voiceShareAudio':
          events.push('http:PUT audio');
          return Promise.resolve(put.shift() ?? consent());
        case 'voiceShareAudioEnd':
          if (rename.from !== null && request.params?.['channel'] === rename.from) {
            events.push('http:DELETE audio (old name)');
            return Promise.resolve(refusal(404, 'channel_not_found'));
          }
          events.push('http:DELETE audio');
          return Promise.resolve(withdraw.shift() ?? ok());
        case 'listChannels': {
          events.push('http:GET channels');
          return Promise.resolve(rename.list ?? { kind: 'none' });
        }
        case 'voicePresence': {
          if (rename.from !== null && request.params?.['channel'] === rename.from) {
            events.push('http:GET presence (old name)');
            return Promise.resolve(refusal(404, 'channel_not_found'));
          }
          const next = presence.shift();
          if (next === undefined) return Promise.resolve({ kind: 'none' });
          events.push('http:GET presence');
          return Promise.resolve(next);
        }
        default:
          return Promise.resolve({ kind: 'none' });
      }
    },
  };
  let sound: ShareSound | null = null;
  const companion = new Companion({
    voice,
    locked: () => false,
    client: () => client,
    document: () => DOC,
    tell: () => undefined,
    log: (line) => logs.push(line),
    sleep: () => Promise.resolve(),
    channelId: (share) => sound?.channelId(share) ?? null,
    ...(opts.sound === false ? {} : { soundEnded: (share, reason) => sound?.ended(share, reason) ?? Promise.resolve() }),
  });
  const views: Rig['views'] = [];
  voice.subscribe((view) => views.push(view));
  if (opts.sound !== false) {
    const deps: ShareSoundDeps = {
      voice,
      companion,
      choice: () => prefs.choice,
      setChoice: (choice) => {
        if (prefs.failSave) return Promise.resolve(false);
        prefs.choice = choice;
        prefs.saves.push(choice);
        return Promise.resolve(true);
      },
      applicationName: 'Conch',
      log: (line) => logs.push(line),
      ...(opts.consentRetryMs === undefined ? {} : { consentRetryMs: opts.consentRetryMs }),
      ...(opts.publishRetryMs === undefined ? {} : { publishRetryMs: opts.publishRetryMs }),
      ...(opts.stopAckBoundMs === undefined ? {} : { stopAckBoundMs: opts.stopAckBoundMs }),
      ...(opts.presencePollMs === undefined ? {} : { presencePollMs: opts.presencePollMs }),
    };
    const made = new ShareSound(deps);
    sound = made;
    voice.attachShareSound({ view: () => made.view(), leaving: () => made.leaving() });
  }
  const r: Rig = {
    events,
    logs,
    sup,
    voice,
    companion,
    sound,
    prefs,
    sends,
    put,
    withdraw,
    removal,
    presence,
    rename,
    client,
    views,
    live: async () => {
      await r.watch();
      sound?.captureGranted(DOC);
    },
    watch: async () => {
      await voice.join({ channel: 'general', muted: false, deafened: false });
      expect(await companion.grant({ channel: 'general' })).toMatchObject({ ok: true });
      events.length = 0;
      sends.length = 0;
      logs.length = 0;
      views.length = 0;
    },
    seen: () => voice.state().shareSound,
  };
  return r;
}

// The relative order of what happened: the events that name these, in the
// order they happened.
export function trace(r: Rig, ...names: string[]): string[] {
  return r.events.filter((e) => names.includes(e));
}

// Every end sends the helper's stop and then the withdrawal, once each.
export function expectStopThenWithdraw(r: Rig): void {
  expect(trace(r, 'helper:capture off', 'http:DELETE audio')).toEqual(['helper:capture off', 'http:DELETE audio']);
}

// No command of any share's sound ever named the application scope.
export function expectNeverApplicationScope(r: Rig): void {
  for (const c of r.sup.wire) expect(c['scope']).not.toBe('application');
  for (const s of r.sends) expect(JSON.stringify(s.body ?? {})).not.toContain('application');
}
