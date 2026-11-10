// The native voice service (issue #1373) over a fake supervisor, and over the
// real supervisor with the fake helper (tests/fixtures/fake-helper.ts) for what
// reaches a process: argv, environment, teardown, two instances.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { VOICE_CONTROL_STOP_KILL_AFTER_MS } from '../../../web/src/schema/generated.ts';
import { Supervisor, type CommandResult, type StartRequest, type VoiceState } from '../../main/voice/supervisor.ts';
import { CEILING_ARGS, DESKTOP_MISSED_RELEASE_SECS, VoiceService, type VoiceIdentity, type VoiceSupervisor } from '../../main/voice/service.ts';
import type { NativeVoiceDevicesResult, NativeVoiceView, VoiceDeviceKey, VoiceSaveResult } from '../../../web/src/platform/desktop/bridge.ts';
import { FAKE_TOKEN, fixture, scratch, until, type Scratch } from './voice-support.ts';

const V2 = 'voice-control-v2/transcript-negotiation-v2-owner.json';

// A supervisor that does what it is told and records every call.
class FakeSupervisor implements VoiceSupervisor {
  state: VoiceState = { status: 'idle' };
  readonly starts: StartRequest[] = [];
  readonly calls: string[] = [];
  next: VoiceState = { status: 'running', generation: 1, helper: helper() };
  gate: Promise<void> | null = null;
  // Blocks start before it records anything or publishes starting.
  preGate: Promise<void> | null = null;
  entered = 0;
  hangStop = false;
  private listeners = new Set<(s: VoiceState) => void>();
  ack: CommandResult = { ok: true };

  set(s: VoiceState): void {
    this.state = s;
    for (const l of this.listeners) l(s);
  }
  subscribe(l: (s: VoiceState) => void): () => void {
    this.listeners.add(l);
    return () => this.listeners.delete(l);
  }
  async start(req: StartRequest): Promise<VoiceState> {
    this.entered++;
    if (this.preGate !== null) await this.preGate;
    this.starts.push(req);
    this.set({ status: 'starting', generation: 1 });
    if (this.gate !== null) await this.gate;
    this.set(this.next);
    return this.next;
  }
  stop(): Promise<void> {
    this.calls.push('stop');
    if (this.hangStop) return new Promise(() => undefined);
    this.set({ status: 'idle' });
    return Promise.resolve();
  }
  setMute(m: boolean): Promise<CommandResult> {
    this.calls.push(`mute:${String(m)}`);
    return Promise.resolve(this.ack);
  }
  setDeafen(d: boolean): Promise<CommandResult> {
    this.calls.push(`deafen:${String(d)}`);
    return Promise.resolve(this.ack);
  }
  setDevices(): Promise<CommandResult> {
    return Promise.resolve(this.ack);
  }
  setTalkMode(): Promise<CommandResult> {
    return Promise.resolve(this.ack);
  }
}

function helper(over: Partial<Extract<VoiceState, { status: 'running' }>['helper']> = {}): Extract<VoiceState, { status: 'running' }>['helper'] {
  return { version: 2, helperVersion: '1.2.3', phase: 'connected', capture: 'on', playback: 'on', transmitting: false, muted: false, deafened: false, blocked: [], talkMode: 'push_to_talk', ptt: 'ready', ...over };
}

class Identity implements VoiceIdentity {
  readonly profileId = 'p1';
  readonly configDir = '/nonexistent/config/p1';
  readonly principalId = 7;
  live = true;
  signedIn = true;
  reads = 0;
  credential(): { origin: string; token: string } | null {
    this.reads++;
    return this.signedIn ? { origin: 'https://conch.example.org', token: FAKE_TOKEN } : null;
  }
  current(): boolean {
    return this.live;
  }
}

// Records the one-shot runs the service asks for (issue #1378).
class FakeOneShot {
  calls: [string, ...unknown[]][] = [];
  cancels = 0;
  devicesAnswer: NativeVoiceDevicesResult = { ok: true, pipewire: 'ok', synced: true, microphones: [], speakers: [] };
  saveAnswer: VoiceSaveResult = { ok: true };
  devices(configDir: string): Promise<NativeVoiceDevicesResult> {
    this.calls.push(['devices', configDir]);
    return Promise.resolve(this.devicesAnswer);
  }
  save(configDir: string, key: VoiceDeviceKey | 'keys.device' | 'keys.talk', value: string): Promise<VoiceSaveResult> {
    this.calls.push(['save', configDir, key, value]);
    return Promise.resolve(this.saveAnswer);
  }
  cancel(): Promise<void> {
    this.cancels++;
    return Promise.resolve();
  }
}

let oneShot: FakeOneShot;
let fake: FakeSupervisor;
let id: Identity;
let logs: string[];
let views: NativeVoiceView[];
let svc: VoiceService;
beforeEach(() => {
  oneShot = new FakeOneShot();
  fake = new FakeSupervisor();
  id = new Identity();
  logs = [];
  views = [];
  svc = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(id), log: (l) => logs.push(l), stopBoundMs: 200 });
  svc.subscribe((v) => views.push(v));
});
afterEach(() => {
  vi.useRealTimers();
});

const join = (channel = 'general', muted = false, deafened = false) => svc.join({ channel, muted, deafened });

describe('join', () => {
  it('starts one helper with main-owned values, the token read now, and the 30 s ceiling', async () => {
    const v = await join('general', true, false);
    expect(v.status).toBe('running');
    expect(fake.starts).toEqual([
      {
        profileId: 'p1',
        configDir: '/nonexistent/config/p1',
        token: FAKE_TOKEN,
        server: 'https://conch.example.org',
        channel: 'general',
        principalId: 7,
        muted: true,
        deafened: false,
        extraArgs: ['--transmit-ceiling-secs', '30'],
      },
    ]);
    expect(DESKTOP_MISSED_RELEASE_SECS).toBe(30);
    expect(CEILING_ARGS).toEqual(['--transmit-ceiling-secs', '30']);
    expect(id.reads).toBe(1);
  });

  it('a second join while one runs is refused busy and starts nothing', async () => {
    await join();
    const again = await join('other');
    expect(again.problem).toBe('busy');
    expect(again.channel).toBe('general');
    expect(fake.starts.length).toBe(1);
  });

  it('two joins at once start one', async () => {
    let release: () => void = () => undefined;
    fake.gate = new Promise((r) => {
      release = r;
    });
    const a = join();
    const b = await join('other');
    expect(b.problem).toBe('busy');
    release();
    await a;
    expect(fake.starts.length).toBe(1);
  });

  it('signed out: nothing starts, and the view says why', async () => {
    id.signedIn = false;
    const v = await join();
    expect(v).toMatchObject({ status: 'unavailable', problem: 'not-signed-in', channel: null, retryable: false });
    expect(fake.starts).toEqual([]);
  });

  it('no identity at all: nothing starts', async () => {
    svc = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(null), log: () => undefined });
    expect((await svc.join({ channel: 'x', muted: false, deafened: false })).problem).toBe('not-signed-in');
    expect(fake.starts).toEqual([]);
  });

  it('a missing helper is unavailable, not-installed; the channel is not kept', async () => {
    fake.next = { status: 'unavailable', reason: 'not-installed' };
    const v = await join();
    expect(v).toMatchObject({ status: 'unavailable', problem: 'not-installed', channel: null, retryable: false });
  });
});

describe('nothing starts by itself', () => {
  it('launch, sign-in, restore, switch and reconnect events never call start', async () => {
    const other = new Identity();
    // Everything the app does around voice: construct, ask for state, tear down
    // on a switch, look at identities.
    expect(svc.state().status).toBe('idle');
    await svc.teardown();
    id.live = false;
    other.signedIn = false;
    await svc.teardown();
    expect(fake.starts).toEqual([]);
    expect(fake.calls.filter((c) => c !== 'stop')).toEqual([]);
  });

  it('retry is the only other way in, and a fake clock sees no self-retry', async () => {
    vi.useFakeTimers();
    fake.next = { status: 'lost', generation: 1, reason: 'exited' };
    await join();
    await vi.advanceTimersByTimeAsync(10 * 60_000);
    expect(fake.starts.length).toBe(1);
  });
});

describe('the destination', () => {
  it('changes only on join and when the session ends', async () => {
    expect(svc.state().channel).toBeNull();
    await join('general');
    for (let i = 0; i < 20; i++) {
      // Browsing: state is asked for, helper states arrive; no input names a channel.
      fake.set({ status: 'running', generation: 1, helper: helper({ transmitting: i % 2 === 0 }) });
      expect(svc.state().channel).toBe('general');
    }
    expect(views.every((v) => v.channel === 'general')).toBe(true);
    await svc.leave();
    expect(svc.state().channel).toBeNull();
  });
});

describe('state', () => {
  it('maps each supervisor state to one view, with only confirmed helper state', async () => {
    await join();
    const cases: [VoiceState, Partial<NativeVoiceView>][] = [
      [{ status: 'stopping', generation: 1 }, { status: 'stopping', retryable: false }],
      [{ status: 'failed', generation: 1, reason: 'hello-timeout' }, { status: 'failed', problem: 'hello-timeout', retryable: true }],
      [{ status: 'failed', generation: 1, reason: 'version-mismatch' }, { status: 'failed', problem: 'version-mismatch', retryable: false }],
      [{ status: 'failed', generation: 1, reason: 'invalid-start' }, { status: 'failed', retryable: false }],
      [{ status: 'lost', generation: 1, reason: 'write-failed' }, { status: 'lost', retryable: true }],
      [{ status: 'unavailable', reason: 'untrusted' }, { status: 'unavailable', problem: 'untrusted', retryable: false }],
      [{ status: 'unavailable', reason: 'runtime-dir-unset' }, { status: 'unavailable', retryable: false }],
      [{ status: 'ended', generation: 1, helper: helper({ phase: 'failed', reason: 'server_unreachable' }) }, { status: 'ended', problem: 'server_unreachable', retryable: true }],
      [{ status: 'ended', generation: 1, helper: helper({ phase: 'failed', reason: 'identity_mismatch' }) }, { status: 'ended', retryable: false }],
    ];
    for (const [s, want] of cases) {
      fake.set(s);
      expect(svc.state(), JSON.stringify(s)).toMatchObject(want);
    }
    fake.set({ status: 'running', generation: 1, helper: helper({ phase: 'reconnecting', reason: 'connection_lost', retryInMs: 4000, muted: true }) });
    expect(svc.state().helper).toMatchObject({ phase: 'reconnecting', retryInMs: 4000, muted: true });
    expect(svc.state().helper).not.toHaveProperty('helperVersion');
  });

  it('commands resolve with the helper ack and change nothing optimistically', async () => {
    await join();
    fake.ack = { ok: false, error: 'invalid' };
    expect(await svc.setMute(true)).toEqual({ ok: false, error: 'invalid' });
    expect(svc.state().helper?.muted).toBe(false);
    fake.ack = { ok: true };
    expect(await svc.setMute(true)).toEqual({ ok: true });
    // Still what the helper last said, until it says otherwise.
    expect(svc.state().helper?.muted).toBe(false);
  });
});

describe('retry', () => {
  it('reuses the last destination, mute and deafen the person set; refused when not retryable', async () => {
    fake.next = { status: 'failed', generation: 1, reason: 'hello-timeout' };
    await join('ops', false, false);
    // The person muted and deafened meanwhile (acked), so retry carries it.
    fake.next = { status: 'running', generation: 2, helper: helper() };
    fake.state = { status: 'failed', generation: 1, reason: 'hello-timeout' };
    await svc.setMute(true);
    await svc.setDeafen(true);
    const v = await svc.retry();
    expect(v.status).toBe('running');
    expect(fake.starts.length).toBe(2);
    expect(fake.starts[1]).toMatchObject({ channel: 'ops', muted: true, deafened: true, extraArgs: ['--transmit-ceiling-secs', '30'] });
  });

  it('is refused for a failure a retry cannot fix', async () => {
    fake.next = { status: 'failed', generation: 1, reason: 'version-mismatch' };
    await join();
    const v = await svc.retry();
    expect(v.retryable).toBe(false);
    expect(fake.starts.length).toBe(1);
  });

  it('is refused with nothing to retry, and while running', async () => {
    await svc.retry();
    await join();
    await svc.retry();
    expect(fake.starts.length).toBe(1);
  });
});

describe('teardown and the generation fence', () => {
  it('leaves and stops, and resolves within the kill bound with a hung supervisor', async () => {
    vi.useFakeTimers();
    await join();
    fake.hangStop = true;
    let done = false;
    const p = svc.teardown().then(() => {
      done = true;
    });
    await vi.advanceTimersByTimeAsync(199);
    expect(done).toBe(false);
    await vi.advanceTimersByTimeAsync(2);
    await p;
    expect(done).toBe(true);
    expect(logs.some((l) => l.includes('did not stop'))).toBe(true);
    // The default bound is the contract's.
    const dflt = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(id), log: () => undefined });
    let d2 = false;
    void dflt.teardown().then(() => {
      d2 = true;
    });
    await vi.advanceTimersByTimeAsync(VOICE_CONTROL_STOP_KILL_AFTER_MS + 1);
    expect(d2).toBe(true);
  });

  it('a join overtaken by sign-out is stopped and reports nothing for it', async () => {
    let release: () => void = () => undefined;
    fake.gate = new Promise((r) => {
      release = r;
    });
    const p = join();
    await until(() => fake.starts.length === 1, 1000, 'the start');
    id.live = false;
    release();
    const v = await p;
    expect(v).toMatchObject({ status: 'idle', channel: null });
    expect(fake.calls).toContain('stop');
    expect(views.filter((x) => x.status === 'running')).toEqual([]);
    expect(svc.state().channel).toBeNull();
  });

  it('a join overtaken by a teardown is stopped too', async () => {
    let release: () => void = () => undefined;
    fake.gate = new Promise((r) => {
      release = r;
    });
    const p = join();
    await until(() => fake.starts.length === 1, 1000, 'the start');
    await svc.teardown();
    release();
    expect((await p).status).toBe('idle');
    expect(fake.calls.filter((c) => c === 'stop').length).toBe(2);
  });
});

describe('a join still inside Supervisor.start', () => {
  it('teardown waits for it to be stopped, and state() shows nothing of it meanwhile', async () => {
    let release: () => void = () => undefined;
    fake.preGate = new Promise((r) => {
      release = r;
    });
    const p = join('secret-room');
    await until(() => fake.entered === 1, 1000, 'the start');
    expect(fake.starts).toEqual([]);
    let done = false;
    const t = svc.teardown().then(() => {
      done = true;
    });
    // The switch goes on: the identity is overtaken while the start is blocked.
    id.live = false;
    await new Promise((r) => setTimeout(r, 50));
    expect(done).toBe(false);
    expect(svc.state()).toMatchObject({ status: 'idle', channel: null });
    release();
    await t;
    await p;
    // The start went through, was seen to be overtaken, and was stopped before teardown resolved.
    expect(fake.starts.length).toBe(1);
    expect(fake.calls.filter((c) => c === 'stop').length).toBe(2);
    expect(svc.state().status).toBe('idle');
  });

  it('with a start that never returns, teardown still resolves within the bound', async () => {
    vi.useFakeTimers();
    fake.preGate = new Promise(() => undefined);
    void join();
    await vi.advanceTimersByTimeAsync(10);
    let done = false;
    void svc.teardown().then(() => {
      done = true;
    });
    await vi.advanceTimersByTimeAsync(150);
    expect(done).toBe(false);
    await vi.advanceTimersByTimeAsync(100);
    expect(done).toBe(true);
  });
});

describe('secrets', () => {
  it('the token is in no log line, no view and no error (canary)', async () => {
    await join();
    fake.set({ status: 'failed', generation: 1, reason: 'spawn-failed' });
    fake.set({ status: 'lost', generation: 1, reason: 'exited' });
    await svc.retry();
    await svc.setMute(true);
    await svc.teardown();
    const seen = JSON.stringify([logs, views, svc.state()]);
    expect(seen).not.toContain(FAKE_TOKEN);
    expect(seen).not.toMatch(/token|nonce|pid|stderr/i);
  });
});

// The real supervisor and the fake helper process.
describe('with the real supervisor', () => {
  let a: Scratch;
  let b: Scratch;
  let sups: Supervisor[];
  let logs2: string[];
  beforeEach(() => {
    a = scratch();
    b = scratch();
    sups = [];
    logs2 = [];
  });
  afterEach(async () => {
    await Promise.all(sups.map((x) => x.stop()));
    a.cleanup();
    b.cleanup();
  });

  function make(s: Scratch, profile: string, timings = {}): { svc: VoiceService; views: NativeVoiceView[] } {
    const sup = new Supervisor({
      resources: s.resources,
      env: { HOME: s.root, XDG_RUNTIME_DIR: s.run },
      log: (l) => logs2.push(l),
      timings: { helloTimeoutMs: 5000, ackTimeoutMs: 5000, termAfterMs: 300, killAfterMs: 700, ...timings },
    });
    sups.push(sup);
    const identity: VoiceIdentity = { profileId: profile, configDir: s.config, principalId: 7, credential: () => ({ origin: 'https://conch.example.org', token: FAKE_TOKEN }), current: () => true };
    const service = new VoiceService({ supervisor: sup, oneShot, identity: () => Promise.resolve(identity), log: (l) => logs2.push(l) });
    const seen: NativeVoiceView[] = [];
    service.subscribe((v) => seen.push(v));
    return { svc: service, views: seen };
  }

  const argvOf = (s: Scratch): string[] => (s.reported().find((r) => r['t'] === 'argv')?.['argv'] ?? []) as string[];

  it('the ceiling flag is in argv; the token and nonce are not; they are in no view or log', async () => {
    a.helper({ transcript: fixture(V2), stopAt: 6, leakToStderr: true });
    const m = make(a, 'p1');
    expect((await m.svc.join({ channel: 'general', muted: false, deafened: false })).status).toBe('running');
    const argv = argvOf(a);
    expect(argv.slice(-2)).toEqual(['--transmit-ceiling-secs', '30']);
    expect(argv.join(' ')).not.toContain(FAKE_TOKEN);
    const nonce = ((a.reported().find((r) => r['t'] === 'env')?.['env'] ?? {}) as Record<string, string>)['CONCH_VOICE_ATTACH'] ?? '';
    await m.svc.leave();
    const seen = JSON.stringify([m.views, logs2, m.svc.state()]);
    expect(seen).not.toContain(FAKE_TOKEN);
    expect(nonce.length).toBe(43);
    expect(seen).not.toContain(nonce);
  });

  it('a helper offering no version this app speaks: failed, version-mismatch, not retryable', async () => {
    a.helper({ transcript: fixture(V2), helloVersions: ['conch.voice_control.v3'] });
    const m = make(a, 'p1');
    expect(await m.svc.join({ channel: 'general', muted: false, deafened: false })).toMatchObject({ status: 'failed', problem: 'version-mismatch', retryable: false });
  });

  it('a helper that never says hello: hello-timeout, retryable', async () => {
    a.helper({ transcript: fixture(V2), attach: 'none' });
    const m = make(a, 'p1', { helloTimeoutMs: 300 });
    const v = await m.svc.join({ channel: 'general', muted: false, deafened: false });
    expect(v.status).toBe('failed');
    expect(v.retryable).toBe(true);
  });

  it('no helper installed: unavailable, not-installed', async () => {
    const m = make(a, 'p1');
    // scratch() installs no helper until helper() is called.
    expect(await m.svc.join({ channel: 'general', muted: false, deafened: false })).toMatchObject({ status: 'unavailable', problem: 'not-installed' });
  });

  it('a hung helper is killed within the bound by teardown', async () => {
    a.helper({ transcript: fixture('voice-control-pipe/transcript-attach-then-hello.json'), stopAt: 5, then: 'hang' });
    const m = make(a, 'p1');
    const joining = m.svc.join({ channel: 'general', muted: false, deafened: false });
    await joining.catch(() => undefined);
    const t0 = Date.now();
    await m.svc.teardown();
    expect(Date.now() - t0).toBeLessThanOrEqual(VOICE_CONTROL_STOP_KILL_AFTER_MS);
    expect(m.svc.state().status).toBe('idle');
  });

  it('two instances side by side: stopping one leaves the other running', async () => {
    a.helper({ transcript: fixture(V2), stopAt: 6 });
    b.helper({ transcript: fixture(V2), stopAt: 6 });
    const one = make(a, 'p1');
    const two = make(b, 'p2');
    const req = { channel: 'general', muted: false, deafened: false };
    expect((await one.svc.join(req)).status).toBe('running');
    expect((await two.svc.join(req)).status).toBe('running');
    await one.svc.leave();
    expect(one.svc.state().status).toBe('idle');
    expect(two.svc.state().status).toBe('running');
    expect(b.reported().some((r) => r['t'] === 'stdin-end')).toBe(false);
    await two.svc.leave();
  });
});

describe('the one-shot runs (issue #1378)', () => {
  it('lists devices for the profile in force, with its configuration directory and nothing else', async () => {
    expect(await svc.devices()).toEqual({ ok: true, pipewire: 'ok', synced: true, microphones: [], speakers: [] });
    expect(oneShot.calls).toEqual([['devices', '/nonexistent/config/p1']]);
    expect(fake.starts).toEqual([]);
  });

  it('saves a device or the talk mode for the profile in force', async () => {
    expect(await svc.saveDevice({ key: 'audio.input', value: 'Mic' })).toEqual({ ok: true });
    expect(await svc.saveDevice({ key: 'audio.talk_mode', value: 'open' })).toEqual({ ok: true });
    expect(oneShot.calls).toEqual([
      ['save', '/nonexistent/config/p1', 'audio.input', 'Mic'],
      ['save', '/nonexistent/config/p1', 'audio.talk_mode', 'open'],
    ]);
  });

  it('refuses a key or a value that does not fit, without running anything', async () => {
    for (const [key, value] of [
      ['audio.input', 'a"b'],
      ['audio.output', ''],
      ['audio.talk_mode', 'hold'],
      ['keys.talk', 'KEY_F13'],
      ['audio.input', 'a\u2028b'],
    ] as const) {
      expect(await svc.saveDevice({ key: key as VoiceDeviceKey, value }), `${key} ${value}`).toEqual({ ok: false, error: 'invalid' });
    }
    expect(oneShot.calls).toEqual([]);
  });

  it('says not-signed-in with no identity, and runs nothing', async () => {
    const none = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(null), log: () => undefined });
    expect(await none.devices()).toEqual({ ok: false, error: 'not-signed-in' });
    expect(await none.saveDevice({ key: 'audio.input', value: 'Mic' })).toEqual({ ok: false, error: 'not-signed-in' });
    expect(oneShot.calls).toEqual([]);
  });

  it('a teardown cancels the run in progress, and overtakes one still waiting for its identity', async () => {
    await svc.teardown();
    expect(oneShot.cancels).toBe(1);
    let release: (v: Identity) => void = () => undefined;
    const slow = new VoiceService({
      supervisor: fake,
      oneShot,
      identity: () => new Promise<Identity>((resolve) => (release = resolve)),
      log: () => undefined,
    });
    const pending = slow.devices();
    const torn = slow.teardown();
    // The run reaches identity() after the stamp read (issue #1600).
    await new Promise<void>((resolve) => setImmediate(resolve));
    release(id);
    expect(await pending).toEqual({ ok: false, error: 'not-signed-in' });
    await torn;
    expect(oneShot.calls).toEqual([]);
  });

  it('a leave does not cancel a one-shot run', async () => {
    await svc.leave();
    expect(oneShot.cancels).toBe(0);
  });
});

describe('the installed files were replaced (issue #1600)', () => {
  let replaced: { value: boolean };
  beforeEach(() => {
    replaced = { value: false };
    svc = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(id), log: (l) => logs.push(l), stopBoundMs: 200, updated: () => replaced.value });
    views = [];
    svc.subscribe((v) => views.push(v));
  });

  it('before the replacement a join starts as usual', async () => {
    expect((await join()).status).toBe('running');
    expect(fake.starts.length).toBe(1);
  });

  it('refuses a join with the update reason, starts nothing and reads no credential', async () => {
    replaced.value = true;
    const v = await join();
    expect(v).toMatchObject({ status: 'unavailable', problem: 'update-pending', channel: null, retryable: false });
    expect(fake.entered).toBe(0);
    expect(fake.starts).toEqual([]);
    expect(id.reads).toBe(0);
    expect(views.at(-1)).toMatchObject({ problem: 'update-pending' });
    expect(svc.state()).toMatchObject({ problem: 'update-pending' });
    expect(logs.some((l) => l.includes('replaced'))).toBe(true);
  });

  it('refuses a retry the same way and starts nothing', async () => {
    fake.next = { status: 'failed', generation: 1, reason: 'hello-timeout' };
    await join('ops');
    fake.state = { status: 'failed', generation: 1, reason: 'hello-timeout' };
    replaced.value = true;
    const v = await svc.retry();
    expect(v).toMatchObject({ status: 'unavailable', problem: 'update-pending' });
    expect(fake.starts.length).toBe(1);
  });

  it('leaves a running helper alone: no stop, no change of view, and the call can go on', async () => {
    await join();
    replaced.value = true;
    expect(svc.state().status).toBe('running');
    expect(fake.calls).toEqual([]);
    // A second join is busy, as before; the running helper is still running.
    expect((await join('other')).problem).toBe('busy');
    expect(fake.calls).toEqual([]);
    expect(svc.state().status).toBe('running');
    // Mute still works on it.
    expect((await svc.setMute(true)).ok).toBe(true);
  });

  it('leave still works, and a join after it is refused', async () => {
    await join();
    replaced.value = true;
    const left = await svc.leave();
    expect(left.status).toBe('idle');
    expect(fake.calls).toContain('stop');
    expect((await join()).problem).toBe('update-pending');
    expect(fake.starts.length).toBe(1);
  });
});

describe('the stamp changes between focus and Join (issue #1600)', () => {
  it('a Join is refused when the re-read finds the update, with no focus or tick', async () => {
    const stamp = { value: 'old' };
    const running = 'old';
    let known = false;
    svc = new VoiceService({
      supervisor: fake,
      oneShot,
      identity: () => Promise.resolve(id),
      log: (l) => logs.push(l),
      stopBoundMs: 200,
      updated: () => known,
      recheck: () => {
        known = stamp.value !== running;
        return Promise.resolve();
      },
    });
    expect((await svc.join({ channel: 'general', muted: false, deafened: false })).status).toBe('running');
    await svc.leave();
    stamp.value = 'new';
    const v = await svc.join({ channel: 'general', muted: false, deafened: false });
    expect(v).toMatchObject({ status: 'unavailable', problem: 'update-pending' });
    expect(fake.starts.length).toBe(1);
  });

  it('a failed re-read keeps the last answer', async () => {
    svc = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(id), log: (l) => logs.push(l), updated: () => false, recheck: () => Promise.reject(new Error('x')) });
    expect((await svc.join({ channel: 'general', muted: false, deafened: false })).status).toBe('running');
  });
});

describe('one-shot helper runs after an update (issue #1600)', () => {
  beforeEach(() => {
    svc = new VoiceService({ supervisor: fake, oneShot, identity: () => Promise.resolve(id), log: (l) => logs.push(l), updated: () => true });
  });

  it('refuses the device list with update-pending and runs nothing', async () => {
    expect(await svc.devices()).toEqual({ ok: false, error: 'update-pending' });
    expect(oneShot.calls).toEqual([]);
  });

  it('refuses a device save with update-pending and runs nothing', async () => {
    expect(await svc.saveDevice({ key: 'audio.input', value: 'mic' })).toEqual({ ok: false, error: 'update-pending' });
    expect(oneShot.calls).toEqual([]);
  });

  it('still says invalid for a bad save, before anything else', async () => {
    expect(await svc.saveDevice({ key: 'keys.talk' as never, value: 'x' })).toEqual({ ok: false, error: 'invalid' });
  });
});
