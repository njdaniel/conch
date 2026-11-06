// The owner side of the live same-machine voice test (issue #1383, D3-12 of
// #771). e2e/desktop-voice (Go) starts conchd and LiveKit, makes the
// principals and a listener B that counts what it hears, and runs this with
// its facts in the environment; run it with `make desktop-voice-e2e`.
//
// Here the desktop's own VoiceService and Supervisor drive the real
// conch-voice into that LiveKit. Everything is synthetic: the microphone is
// `--mic tone:<hz>`, the key is a named pipe of hand-built input_event
// records (`--test-key-device`), and what B hears is read from the harness's
// observer (B's `--sink count` stats lines) and LiveKit's own admin API. No
// microphone, PipeWire, keyboard or Electron; one machine.
//
// Bounds stated once. B's stats arrive once a second and each line describes
// the second before it, so what B observes of a change is known only to
// within a line plus the time to get there: SETTLE_MS is that, for a stop, on
// top of the contract's own bound (GATE_SHUT or the release tail). HEAR_MS is
// how long a press may take to be counted by B (the joined helper's key
// thread, the gate, the network, LiveKit, B's decoder and its next stats).
import { execFileSync } from 'node:child_process';
import { chmodSync, closeSync, copyFileSync, existsSync, openSync, readdirSync, statSync, writeFileSync, writeSync } from 'node:fs';
import type { Server, Socket } from 'node:net';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { VOICE_CONTROL_GATE_SHUT_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_STOP_KILL_AFTER_MS } from '../../../web/src/schema/generated.ts';
import type { NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import { HELPER_NAME, defaultSpawn } from '../../main/voice/spawn.ts';
import { CEILING_ARGS, VoiceService } from '../../main/voice/service.ts';
import { Supervisor } from '../../main/voice/supervisor.ts';
import { scratch, until, type Scratch } from '../unit/voice-support.ts';

// ---------------------------------------------------------------- the world

function need(name: string): string {
  const v = process.env[name];
  if (v === undefined || v === '') throw new Error(`${name} is not set: this test is the owner side of e2e/desktop-voice; run make desktop-voice-e2e`);
  return v;
}

const world = {
  voice: need('CONCH_VOICE_BIN'),
  origin: need('CONCH_E2E_ORIGIN'),
  observer: need('CONCH_E2E_OBSERVER'),
  observerToken: need('CONCH_E2E_OBSERVER_TOKEN'),
  channel: need('CONCH_E2E_CHANNEL'),
  a: { id: Number(need('CONCH_E2E_A_ID')), token: need('CONCH_E2E_A_TOKEN'), tone: Number(need('CONCH_E2E_TONE_A')) },
  c: { id: Number(need('CONCH_E2E_C_ID')), token: need('CONCH_E2E_C_TOKEN'), tone: Number(need('CONCH_E2E_TONE_C')) },
};
type Who = typeof world.a;

const HEAR_MS = 8_000;
const SETTLE_MS = 3_000;
// The default release tail (DEFAULT_RELEASE_TAIL_MS of conch-voice-control).
const RELEASE_TAIL_MS = 100;
// How much of B's stats to read after a stop before judging silence.
const OBSERVE_MS = 2_000;

function note(line: string): void {
  console.log(`desktop-voice-e2e: ${line}`);
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

// -------------------------------------------------------------- the observer

interface HeardLine {
  at: number;
  frames: number;
  audible: number;
  hz: number;
}
interface Heard {
  now: number;
  stats_lines: number;
  lines: HeardLine[];
}
interface Participants {
  rooms: number;
  participants: { identity: string; sources: string[] }[];
}

async function ask<T>(method: string, path: string): Promise<T> {
  const res = await fetch(world.observer + path, { method, headers: { authorization: `Bearer ${world.observerToken}` } });
  const text = await res.text();
  if (!res.ok) throw new Error(`observer ${method} ${path}: ${String(res.status)} ${text.slice(0, 300)}`);
  return JSON.parse(text) as T;
}

const heard = (who: Who, since: number): Promise<Heard> => ask<Heard>('GET', `/heard?speaker=p${String(who.id)}&since=${String(since)}`);
const inRoom = async (): Promise<Participants> => ask<Participants>('GET', '/participants');

// waitHeard returns how long after `since` B first counted who's tone audibly.
async function waitHeard(who: Who, since: number, within = HEAR_MS): Promise<number> {
  const end = Date.now() + within;
  for (;;) {
    const h = await heard(who, since);
    const line = h.lines.find((l) => l.audible > 0 && l.hz === who.tone);
    if (line !== undefined) return line.at - since;
    if (Date.now() > end) throw new Error(`B did not count ${String(who.tone)} Hz from p${String(who.id)} within ${String(within)} ms: ${JSON.stringify(h)}`);
    await sleep(200);
  }
}

// expectSilentFrom checks B, alive, counted nothing audible of who in any
// stats line received after `from`, once it has watched until `from` plus
// OBSERVE_MS.
async function expectSilentFrom(who: Who, from: number): Promise<void> {
  const wait = from + OBSERVE_MS - Date.now();
  if (wait > 0) await sleep(wait);
  const h = await heard(who, from);
  expect(h.stats_lines, 'B printed stats lines in the window (it is alive)').toBeGreaterThanOrEqual(1);
  expect(h.lines.filter((l) => l.audible > 0), `B counted audio from p${String(who.id)} in a stats line received after the stop`).toEqual([]);
}

// ------------------------------------------------------------ key records

const EV_SYN = 0;
const EV_KEY = 1;
const KEY_F13 = 183;

function record(kind: number, code: number, value: number): Buffer {
  const b = Buffer.alloc(24);
  b.writeBigUInt64LE(1_700_000_000n, 0);
  b.writeUInt16LE(kind, 16);
  b.writeUInt16LE(code, 18);
  b.writeInt32LE(value, 20);
  return b;
}
const keyEvent = (value: 0 | 1 | 2): Buffer => Buffer.concat([record(EV_KEY, KEY_F13, value), record(EV_SYN, 0, 0)]);

// ----------------------------------------------------------------- an owner

interface OwnerOptions {
  readonly name: string;
  readonly who: Who;
  // Replaces the 30 s ceiling through the same extraArgs path.
  readonly ceilingSecs?: number;
}

// Owner is one desktop instance: its own scratch directories (runtime,
// configuration, resources), a key pipe, a Supervisor and the VoiceService
// over it, the way main wires them.
class Owner {
  readonly s: Scratch;
  readonly logs: { at: number; line: string }[] = [];
  readonly children: ReturnType<typeof defaultSpawn>[] = [];
  readonly sockets: Socket[] = [];
  readonly views: NativeVoiceView[] = [];
  readonly service: VoiceService;
  readonly sup: Supervisor;
  readonly profileId: string;
  readonly fifo: string;
  fd: number | null = null;
  private repeater: NodeJS.Timeout | null = null;

  constructor(readonly o: OwnerOptions) {
    this.s = scratch();
    this.profileId = o.name;
    copyFileSync(world.voice, join(this.s.resources, HELPER_NAME));
    chmodSync(join(this.s.resources, HELPER_NAME), 0o755);
    writeFileSync(join(this.s.config, 'voice.toml'), '[keys]\ntalk = "KEY_F13"\n', { mode: 0o600 });
    this.fifo = join(this.s.root, 'keyboard');
    execFileSync('mkfifo', ['-m', '600', this.fifo]);
    this.openKeys();
    this.sup = new Supervisor({
      resources: this.s.resources,
      env: { HOME: this.s.root, XDG_RUNTIME_DIR: this.s.run, PIPEWIRE_REMOTE: 'conch-voice-test-no-such-socket' },
      log: (line) => this.logs.push({ at: Date.now(), line }),
      spawn: (command, args, options) => {
        const child = defaultSpawn(command, args, options);
        this.children.push(child);
        return child;
      },
      // The events connection's server side, so a test can close it.
      endpoint: {
        listen: (server: Server, path: string) =>
          new Promise<void>((resolve, reject) => {
            server.on('connection', (socket: Socket) => this.sockets.push(socket));
            server.once('error', reject);
            server.listen({ path, readableAll: false, writableAll: false }, () => {
              server.off('error', reject);
              resolve();
            });
          }),
      },
    });
    // The service passes the 30 s ceiling; the test replaces it when it
    // wants a short one, and adds only the synthetic microphone, sink and key.
    const start = this.sup.start.bind(this.sup);
    this.sup.start = (req) => {
      const kept: string[] = [];
      const given = req.extraArgs ?? [];
      for (let i = 0; i < given.length; i++) {
        const arg = given[i] ?? '';
        if (arg === CEILING_ARGS[0] && o.ceilingSecs !== undefined) {
          i++;
          continue;
        }
        kept.push(arg);
      }
      const ceiling = o.ceilingSecs === undefined ? [] : [CEILING_ARGS[0] ?? '', String(o.ceilingSecs)];
      return start({ ...req, extraArgs: [...kept, ...ceiling, '--mic', `tone:${String(o.who.tone)}`, '--sink', 'count', '--test-key-device', this.fifo] });
    };
    this.service = new VoiceService({
      supervisor: this.sup,
      identity: () =>
        Promise.resolve({
          profileId: this.profileId,
          configDir: this.s.config,
          principalId: o.who.id,
          credential: () => ({ origin: world.origin, token: o.who.token }),
          current: () => true,
        }),
      log: (line) => this.logs.push({ at: Date.now(), line }),
    });
    this.service.subscribe((v) => this.views.push(v));
  }

  // The key pipe held open for read and write, as the managed check does: the
  // helper's open of it never waits, and closing it is the device going away.
  openKeys(): void {
    this.fd = openSync(this.fifo, 'r+');
  }

  closeKeys(): void {
    if (this.fd !== null) closeSync(this.fd);
    this.fd = null;
  }

  private write(value: 0 | 1 | 2): void {
    if (this.fd === null) throw new Error('the key pipe is closed');
    writeSync(this.fd, keyEvent(value));
  }

  press(): number {
    const at = Date.now();
    this.write(1);
    return at;
  }

  release(): number {
    this.stopRepeat();
    const at = Date.now();
    this.write(0);
    return at;
  }

  // repeat writes the held key's repeat records every 33 ms, as a keyboard does.
  repeat(): void {
    this.repeater ??= setInterval(() => {
      try {
        this.write(2);
      } catch {
        this.stopRepeat();
      }
    }, 33);
  }

  stopRepeat(): void {
    if (this.repeater !== null) clearInterval(this.repeater);
    this.repeater = null;
  }

  get view(): NativeVoiceView {
    return this.service.state();
  }

  get helper(): NonNullable<NativeVoiceView['helper']> {
    const h = this.view.helper;
    if (h === undefined) throw new Error(`no helper in the view: ${JSON.stringify(this.view)}`);
    return h;
  }

  async join(): Promise<NativeVoiceView> {
    const view = await this.service.join({ channel: world.channel, muted: false, deafened: false });
    expect(view.status, JSON.stringify({ view, logs: this.logs.map((l) => l.line), diag: this.sup.diagnostics() })).toBe('running');
    await until(() => this.view.helper?.phase === 'connected' && this.view.helper.ptt === 'ready', 45_000, `${this.o.name} connected with its key ready`);
    return view;
  }

  pid(): number {
    const child = this.children[0];
    if (child?.pid === undefined) throw new Error('no helper was spawned');
    return child.pid;
  }

  async close(): Promise<void> {
    this.stopRepeat();
    try {
      await this.service.leave();
    } catch {
      // The test is over; the scratch goes next.
    }
    this.service.close();
    try {
      this.closeKeys();
    } catch {
      // Already closed.
    }
    this.s.cleanup();
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

async function onlyOnce(who: Who): Promise<void> {
  // The track is published a moment after the connection is up.
  let ps = await inRoom();
  const mineOf = (p: Participants): Participants['participants'] => p.participants.filter((x) => x.identity === `p${String(who.id)}`);
  const end = Date.now() + 10_000;
  while ((mineOf(ps)[0]?.sources.length ?? 0) === 0 && Date.now() < end) {
    await sleep(250);
    ps = await inRoom();
  }
  expect(ps.rooms, 'one room in LiveKit').toBe(1);
  const mine = mineOf(ps);
  expect(mine, `exactly one participant for p${String(who.id)}`).toHaveLength(1);
  expect(mine[0]?.sources.length, 'publishing one track').toBe(1);
  expect(mine[0]?.sources[0], 'and it is the microphone').toMatch(/^(MICROPHONE|2)$/);
}

async function absent(who: Who): Promise<boolean> {
  return !(await inRoom()).participants.some((p) => p.identity === `p${String(who.id)}`);
}

const owners: Owner[] = [];
function owner(o: OwnerOptions): Owner {
  const made = new Owner(o);
  owners.push(made);
  return made;
}

afterEach(async () => {
  for (const o of owners.splice(0)) await o.close();
  await ask('DELETE', '/second-session').catch(() => undefined);
});

// ------------------------------------------------------------------- tests

describe('the desktop voice owner with the real conch-voice, LiveKit and a listener', () => {
  it('joins; a press is counted by B, a release stops it', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    const t0 = Date.now();
    await a.join();
    note(`join to connected took ${String(Date.now() - t0)} ms`);
    expect(a.view.helper?.phase).toBe('connected');
    await onlyOnce(world.a);

    // Nothing is heard before a press.
    const before = Date.now();
    await sleep(1_500);
    expect((await heard(world.a, before)).lines.filter((l) => l.audible > 0)).toEqual([]);

    const pressed = a.press();
    await until(() => a.view.helper?.transmitting === true, 5_000, 'transmitting after the press');
    expect(a.helper.blocked).toEqual([]);
    const took = await waitHeard(world.a, pressed);
    note(`press to counted by B took ${String(took)} ms (bound ${String(HEAR_MS)} ms)`);
    // B counted the tone, and A's own helper counted none of its own.
    const released = a.release();
    await until(() => a.view.helper?.transmitting === false, 5_000, 'not transmitting after the release');
    await expectSilentFrom(world.a, released + RELEASE_TAIL_MS + SETTLE_MS);
    note(`release to silent at B within ${String(RELEASE_TAIL_MS + SETTLE_MS)} ms (release tail ${String(RELEASE_TAIL_MS)} + margin ${String(SETTLE_MS)})`);
    await onlyOnce(world.a);
  });

  it('the ceiling: a held key with repeats ends at the ceiling, repeats do not reopen, release then press does', async () => {
    const a = owner({ name: 'desk-a', who: world.a, ceilingSecs: 2 });
    await a.join();
    const pressed = a.press();
    a.repeat();
    await until(() => a.view.helper?.transmitting === true, 5_000, 'transmitting');
    await waitHeard(world.a, pressed);
    await until(() => a.view.helper?.blocked.includes('max_transmit') === true, 8_000, 'blocked by max_transmit');
    const at = Date.now() - pressed;
    note(`press with repeats ended at the ceiling after ${String(at)} ms (ceiling 2000 ms)`);
    expect(at).toBeGreaterThanOrEqual(1_800);
    expect(at).toBeLessThan(5_000);
    expect(a.helper.transmitting).toBe(false);
    const stopped = Date.now();
    // The repeats go on; they do not reopen the gate.
    await sleep(2_500);
    expect(a.helper.blocked).toContain('max_transmit');
    expect(a.helper.transmitting).toBe(false);
    await expectSilentFrom(world.a, stopped + RELEASE_TAIL_MS + SETTLE_MS - 500);

    a.release();
    await until(() => a.view.helper?.blocked.includes('max_transmit') === false, 5_000, 'the block lifted by the release');
    const again = a.press();
    await until(() => a.view.helper?.transmitting === true, 5_000, 'transmitting after release then press');
    await waitHeard(world.a, again);
    a.release();
  });

  it('owner loss: the events connection closed ends the gate and the helper within the bounds', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    await a.join();
    const pid = a.pid();
    const pressed = a.press();
    await waitHeard(world.a, pressed);
    expect(a.sockets.length).toBeGreaterThan(0);
    const lost = Date.now();
    for (const s of a.sockets) s.destroy();
    await until(() => a.logs.some((l) => l.line.startsWith('voice: helper 1 exited')), VOICE_CONTROL_OWNER_LOSS_EXIT_MS + 1_000, 'the helper to exit');
    const exited = a.logs.find((l) => l.line.startsWith('voice: helper 1 exited'));
    const took = (exited?.at ?? Infinity) - lost;
    note(`events connection closed to helper exit took ${String(took)} ms (bound ${String(VOICE_CONTROL_OWNER_LOSS_EXIT_MS)} ms)`);
    expect(took).toBeLessThan(VOICE_CONTROL_OWNER_LOSS_EXIT_MS);
    expect(alive(pid)).toBe(false);
    await expectSilentFrom(world.a, lost + VOICE_CONTROL_GATE_SHUT_MS + SETTLE_MS);
    note(`gate shut as B observed it within ${String(VOICE_CONTROL_GATE_SHUT_MS)} ms + margin ${String(SETTLE_MS)} ms (stats are once a second)`);
    expect(await absentSoon(world.a)).toBe(true);
  });

  it('owner loss: the end of standard input ends the gate and the helper within the bounds', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    await a.join();
    const pid = a.pid();
    const pressed = a.press();
    await waitHeard(world.a, pressed);
    const child = a.children[0];
    expect(child).toBeDefined();
    const lost = Date.now();
    child?.stdin.destroy();
    await until(() => a.logs.some((l) => l.line.startsWith('voice: helper 1 exited')), VOICE_CONTROL_OWNER_LOSS_EXIT_MS + 1_000, 'the helper to exit');
    const exited = a.logs.find((l) => l.line.startsWith('voice: helper 1 exited'));
    const took = (exited?.at ?? Infinity) - lost;
    note(`end of standard input to helper exit took ${String(took)} ms (bound ${String(VOICE_CONTROL_OWNER_LOSS_EXIT_MS)} ms)`);
    expect(took).toBeLessThan(VOICE_CONTROL_OWNER_LOSS_EXIT_MS);
    expect(alive(pid)).toBe(false);
    await expectSilentFrom(world.a, lost + VOICE_CONTROL_GATE_SHUT_MS + SETTLE_MS);
    expect(await absentSoon(world.a)).toBe(true);
  });

  it('leave, and the switch hook (teardown), stop and reap within the kill bound', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    for (const how of ['leave', 'teardown'] as const) {
      a.logs.length = 0;
      a.children.length = 0;
      await a.join();
      await onlyOnce(world.a);
      const pid = a.pid();
      const t0 = Date.now();
      if (how === 'leave') await a.service.leave();
      else await a.service.teardown();
      const took = Date.now() - t0;
      note(`${how} to reaped took ${String(took)} ms (bound ${String(VOICE_CONTROL_STOP_KILL_AFTER_MS)} ms)`);
      expect(took).toBeLessThan(VOICE_CONTROL_STOP_KILL_AFTER_MS);
      expect(a.service.state().status).toBe('idle');
      expect(alive(pid), 'the helper is reaped').toBe(false);
      expect(a.logs.some((l) => /^voice: helper \d+ exited \(code 0\)$/.test(l.line)), 'the helper exited 0').toBe(true);
      expect(a.logs.some((l) => /SIGTERM|SIGKILL/.test(l.line))).toBe(false);
      expect(await absentSoon(world.a), `A is out of the room after ${how}`).toBe(true);
    }
  });

  it('key device loss: the gate shuts, ptt is lost; after it returns only a fresh press transmits', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    await a.join();
    const pressed = a.press();
    await waitHeard(world.a, pressed);
    await until(() => a.view.helper?.transmitting === true, 5_000, 'transmitting');

    const lost = Date.now();
    a.closeKeys();
    await until(() => a.view.helper?.ptt === 'lost', 5_000, 'ptt lost');
    expect(a.helper.transmitting).toBe(false);
    expect(a.helper.blocked).toContain('key_device_lost');
    note(`closing the key pipe to ptt lost took ${String(Date.now() - lost)} ms`);
    await expectSilentFrom(world.a, lost + VOICE_CONTROL_GATE_SHUT_MS + SETTLE_MS);

    // The key was still "held" when the device went; it comes back with no
    // release record, and nothing is replayed from before.
    a.openKeys();
    await until(() => a.view.helper?.ptt === 'ready', 20_000, 'ptt ready again');
    expect(a.helper.blocked).not.toContain('key_device_lost');
    const back = Date.now();
    await sleep(2_500);
    expect(a.helper.transmitting, 'stale input is not replayed').toBe(false);
    await expectSilentFrom(world.a, back + 500);

    a.release();
    const fresh = a.press();
    await until(() => a.view.helper?.transmitting === true, 5_000, 'transmitting after a fresh press');
    await waitHeard(world.a, fresh);
    a.release();
  });

  it('duplicate identity: a second session of A displaces the desktop helper, which is not retried by itself', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    await a.join();
    await onlyOnce(world.a);
    await ask('POST', '/second-session');
    await until(() => a.view.status === 'ended', 30_000, 'the displaced helper to end');
    const v = a.view;
    expect(v.helper?.phase).toBe('failed');
    expect(v.helper?.reason).toBe('duplicate_identity');
    expect(v.problem).toBe('duplicate_identity');
    expect(v.retryable).toBe(true);
    const spawned = a.children.length;
    await sleep(4_000);
    expect(a.children.length, 'nothing started a new helper by itself').toBe(spawned);
    expect(a.view.status).toBe('ended');
    // The second session is the one participant of A now.
    const mine = (await inRoom()).participants.filter((p) => p.identity === `p${String(world.a.id)}`);
    expect(mine).toHaveLength(1);
    await ask('DELETE', '/second-session');
  });

  it('two owners: A and C with separate runtime directories; stopping one leaves the other connected and untouched', async () => {
    const a = owner({ name: 'desk-a', who: world.a });
    const c = owner({ name: 'desk-c', who: world.c });
    expect(a.s.run).not.toBe(c.s.run);
    await Promise.all([a.join(), c.join()]);
    await onlyOnce(world.a);
    await onlyOnce(world.c);
    expect(a.pid()).not.toBe(c.pid());

    const dir = join(c.s.run, 'conch', c.profileId);
    expect(existsSync(dir)).toBe(true);
    const listing = (): string => JSON.stringify([readdirSync(dir).sort(), statSync(dir).ino, statSync(dir).mode]);
    const before = listing();
    const aDir = join(a.s.run, 'conch', a.profileId);
    expect(existsSync(aDir)).toBe(true);

    const t0 = Date.now();
    await a.service.leave();
    note(`stopping A took ${String(Date.now() - t0)} ms`);
    expect(alive(a.pid())).toBe(false);
    expect(alive(c.pid())).toBe(true);
    expect(c.view.status).toBe('running');
    expect(c.view.helper?.phase).toBe('connected');
    expect(listing(), "C's runtime directory is untouched").toBe(before);
    expect(await absentSoon(world.a)).toBe(true);
    await onlyOnce(world.c);

    // C still talks, and B counts C's tone and not A's.
    const pressed = c.press();
    await until(() => c.view.helper?.transmitting === true, 5_000, 'C transmitting');
    await waitHeard(world.c, pressed);
    expect((await heard(world.a, pressed)).lines.filter((l) => l.audible > 0)).toEqual([]);
    c.release();
  });
});

// absentSoon is true once LiveKit no longer lists who; it can take a moment.
async function absentSoon(who: Who): Promise<boolean> {
  const end = Date.now() + 10_000;
  for (;;) {
    if (await absent(who)) return true;
    if (Date.now() > end) return false;
    await sleep(250);
  }
}
