// The world shared by the live desktop tests (issues #1383 and #1583): the
// facts e2e/desktop-voice gives in the environment, the observer's questions
// (what its listener B counted, who LiveKit lists), and Owner, one desktop
// instance (a Supervisor and the VoiceService over it, with the real
// conch-voice). voice-live.test.ts and companion-live.test.ts use it; it is
// not a test and not run by itself.
//
// Bounds stated once. B's stats arrive once a second and each line describes
// the second before it, so what B observes of a change is known only to
// within a line plus the time to get there: SETTLE_MS is that, for a stop, on
// top of the contract's own bound (GATE_SHUT or the release tail). HEAR_MS is
// how long a press may take to be counted by B (the joined helper's key
// thread, the gate, the network, LiveKit, B's decoder and its next stats).
import { execFileSync } from 'node:child_process';
import { chmodSync, closeSync, copyFileSync, openSync, writeFileSync, writeSync } from 'node:fs';
import type { Server, Socket } from 'node:net';
import { join } from 'node:path';

import { expect } from 'vitest';

import type { NativeVoiceView } from '../../../web/src/platform/desktop/bridge.ts';
import { HELPER_NAME, defaultSpawn } from '../../main/voice/spawn.ts';
import { CEILING_ARGS, VoiceService } from '../../main/voice/service.ts';
import { Supervisor } from '../../main/voice/supervisor.ts';
import { scratch, until, type Scratch } from '../unit/voice-support.ts';

// ---------------------------------------------------------------- the world

export function need(name: string): string {
  const v = process.env[name];
  if (v === undefined || v === '') throw new Error(`${name} is not set: this test is the owner side of e2e/desktop-voice; run make desktop-voice-e2e`);
  return v;
}

export const world = {
  voice: need('CONCH_VOICE_BIN'),
  origin: need('CONCH_E2E_ORIGIN'),
  observer: need('CONCH_E2E_OBSERVER'),
  observerToken: need('CONCH_E2E_OBSERVER_TOKEN'),
  channel: need('CONCH_E2E_CHANNEL'),
  a: { id: Number(need('CONCH_E2E_A_ID')), token: need('CONCH_E2E_A_TOKEN'), tone: Number(need('CONCH_E2E_TONE_A')) },
  c: { id: Number(need('CONCH_E2E_C_ID')), token: need('CONCH_E2E_C_TOKEN'), tone: Number(need('CONCH_E2E_TONE_C')) },
};
export type Who = typeof world.a;

export const HEAR_MS = 8_000;
export const SETTLE_MS = 3_000;
// The default release tail (DEFAULT_RELEASE_TAIL_MS of conch-voice-control).
export const RELEASE_TAIL_MS = 100;
// How much of B's stats to read after a stop before judging silence.
export const OBSERVE_MS = 2_000;

export function note(line: string): void {
  console.log(`desktop-voice-e2e: ${line}`);
}

export const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

// -------------------------------------------------------------- the observer

export interface HeardLine {
  at: number;
  frames: number;
  audible: number;
  hz: number;
}
export interface Heard {
  now: number;
  stats_lines: number;
  lines: HeardLine[];
}
export interface Participants {
  rooms: number;
  participants: { identity: string; sources: string[] }[];
}

export async function ask<T>(method: string, path: string): Promise<T> {
  const res = await fetch(world.observer + path, { method, headers: { authorization: `Bearer ${world.observerToken}` } });
  const text = await res.text();
  if (!res.ok) throw new Error(`observer ${method} ${path}: ${String(res.status)} ${text.slice(0, 300)}`);
  return JSON.parse(text) as T;
}

export const heard = (who: Who, since: number): Promise<Heard> => ask<Heard>('GET', `/heard?speaker=p${String(who.id)}&since=${String(since)}`);
export const inRoom = async (): Promise<Participants> => ask<Participants>('GET', '/participants');

// waitHeard returns how long after `since` B first counted who's tone audibly.
export async function waitHeard(who: Who, since: number, within = HEAR_MS): Promise<number> {
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
export async function expectSilentFrom(who: Who, from: number): Promise<void> {
  const wait = from + OBSERVE_MS - Date.now();
  if (wait > 0) await sleep(wait);
  const h = await heard(who, from);
  expect(h.stats_lines, 'B printed stats lines in the window (it is alive)').toBeGreaterThanOrEqual(1);
  expect(h.lines.filter((l) => l.audible > 0), `B counted audio from p${String(who.id)} in a stats line received after the stop`).toEqual([]);
}

// ------------------------------------------------------------ key records

export const EV_SYN = 0;
export const EV_KEY = 1;
export const KEY_F13 = 183;

export function record(kind: number, code: number, value: number): Buffer {
  const b = Buffer.alloc(24);
  b.writeBigUInt64LE(1_700_000_000n, 0);
  b.writeUInt16LE(kind, 16);
  b.writeUInt16LE(code, 18);
  b.writeInt32LE(value, 20);
  return b;
}
export const keyEvent = (value: 0 | 1 | 2): Buffer => Buffer.concat([record(EV_KEY, KEY_F13, value), record(EV_SYN, 0, 0)]);

// ----------------------------------------------------------------- an owner

export interface OwnerOptions {
  readonly name: string;
  readonly who: Who;
  // Replaces the 30 s ceiling through the same extraArgs path.
  readonly ceilingSecs?: number;
}

// Owner is one desktop instance: its own scratch directories (runtime,
// configuration, resources), a key pipe, a Supervisor and the VoiceService
// over it, the way main wires them.
export class Owner {
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
      locked: () => false,
      supervisor: this.sup,
      // No one-shot helper runs in this test (#1378): devices and saves are
      // refused, and teardown has nothing to cancel.
      oneShot: {
        devices: () => Promise.resolve({ ok: false, error: 'unsupported' } as const),
        save: () => Promise.resolve({ ok: false, error: 'unsupported' } as const),
        cancel: () => Promise.resolve(),
      },
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

export function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

export async function onlyOnce(who: Who): Promise<void> {
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

export async function absent(who: Who): Promise<boolean> {
  return !(await inRoom()).participants.some((p) => p.identity === `p${String(who.id)}`);
}

const owners: Owner[] = [];

// closeOwners ends every owner a test made and the second session of A: the
// afterEach of each test file.
export async function closeOwners(): Promise<void> {
  for (const o of owners.splice(0)) await o.close();
  await ask('DELETE', '/second-session').catch(() => undefined);
}
export function owner(o: OwnerOptions): Owner {
  const made = new Owner(o);
  owners.push(made);
  return made;
}


// absentSoon is true once LiveKit no longer lists who; it can take a moment.
export async function absentSoon(who: Who): Promise<boolean> {
  const end = Date.now() + 10_000;
  for (;;) {
    if (await absent(who)) return true;
    if (Date.now() > end) return false;
    await sleep(250);
  }
}


// eventually polls an async check every 200 ms until it returns true, or
// fails with what it was waiting for once `ms` have passed.
export async function eventually(check: () => Promise<boolean>, ms: number, what: string): Promise<void> {
  const end = Date.now() + ms;
  for (;;) {
    if (await check()) return;
    if (Date.now() > end) throw new Error(`timed out after ${String(ms)} ms waiting for ${what}`);
    await sleep(200);
  }
}
