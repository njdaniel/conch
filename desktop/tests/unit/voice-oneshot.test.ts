// The one-shot runs of the voice helper (issue #1378, D3-7 of #771): the
// device list reader against pkg/schema's golden fixtures and conformance
// cases, and the runner against a fake helper run as a Node child through a
// wrapper script (tests/fixtures/fake-oneshot.ts). No network, no audio
// device, no real conch-voice (tests/integration/ has that).
import { spawn as nodeSpawn } from 'node:child_process';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { VOICE_DEVICES_MAX_BYTES, VOICE_DEVICES_MAX_NAME_BYTES, voiceDevicesV1 } from '../../../web/src/schema/generated.ts';
import { parseVoiceDevices } from '../../main/voice/devices.ts';
import { ONE_SHOT_TIMEOUT_MS, OneShot, SAVE_KEYS, isSaveKey, lastLine, saveValueFits, type OneShotDeps } from '../../main/voice/oneshot.ts';
import type { OneShotSpawnFn } from '../../main/voice/spawn.ts';
import type { OneShotScenario } from '../fixtures/fake-oneshot.ts';
import { FAKE_TOKEN, fixture, scratch, scratchBase, type Scratch } from './voice-support.ts';

const FAKE_ONESHOT = resolve(import.meta.dirname, '../fixtures/fake-oneshot.ts');
const DIR = 'voice-devices';

// --------------------------------------------------------------- the reader

interface Case {
  readonly name: string;
  readonly line?: string;
  readonly line_hex?: string;
  readonly line_file?: string;
  readonly want: { readonly result: 'accept' | 'refuse'; readonly error?: string };
}

const bytesOf = (c: Case): Buffer => {
  if (c.line !== undefined) return Buffer.from(c.line, 'utf8');
  if (c.line_hex !== undefined) return Buffer.from(c.line_hex.replaceAll(/\s+/gu, ''), 'hex');
  return readFileSync(fixture(`${DIR}/${c.line_file ?? ''}`));
};

// Go's phrases quote a value this reader does not repeat; the rest is the same.
const unquoted = (s: string): string => s.replaceAll(/"[^"]*"/gu, '').replaceAll(/\s+/gu, ' ');

describe('the device list reader against pkg/schema/testdata/voice-devices/', () => {
  const cases = (JSON.parse(readFileSync(fixture(`${DIR}/cases.json`), 'utf8')) as { cases: Case[] }).cases;

  it('has the cases it is checked against', () => {
    expect(cases.length).toBeGreaterThan(40);
  });

  it.each(cases.map((c) => [c.name, c] as const))('%s', (_name, c) => {
    const got = parseVoiceDevices(bytesOf(c));
    expect(got.ok ? 'accept' : 'refuse').toBe(c.want.result);
    if (!got.ok && c.want.error !== undefined) expect(unquoted(got.detail)).toContain(unquoted(c.want.error));
  });

  const goldens = readdirSync(fixture(DIR)).filter((f) => /^voice-devices-v1.*\.json$/u.test(f));

  it('has every golden fixture', () => {
    expect(goldens).toContain('voice-devices-v1.json');
    expect(goldens.length).toBeGreaterThanOrEqual(7);
  });

  it.each(goldens)('%s matches the generated validator and is accepted by the reader', (file) => {
    const raw = readFileSync(fixture(`${DIR}/${file}`));
    expect(voiceDevicesV1(JSON.parse(raw.toString('utf8')))).toEqual([]);
    expect(parseVoiceDevices(raw).ok).toBe(true);
  });

  it('refuses the sizes the Go test builds: past the most, and more than the most a list holds', () => {
    const one = '{"schema":"conch.voice_devices.v1","pipewire":"ok","synced":true,"microphones":[],"speakers":[],"unshown_microphones":0,"unshown_speakers":0,"config":"none"}';
    expect(parseVoiceDevices(Buffer.from(one + ' '.repeat(VOICE_DEVICES_MAX_BYTES - one.length))).ok).toBe(true);
    expect(parseVoiceDevices(Buffer.from(one + ' '.repeat(VOICE_DEVICES_MAX_BYTES - one.length + 1)))).toMatchObject({ ok: false });
    const dev = (i: number): string => `{"name":"d${String(i)}","description":"","default":false}`;
    const many = (n: number): string => one.replace('"microphones":[]', `"microphones":[${Array.from({ length: n }, (_, i) => dev(i)).join(',')}]`);
    expect(parseVoiceDevices(Buffer.from(many(64))).ok).toBe(true);
    expect(parseVoiceDevices(Buffer.from(many(65)))).toMatchObject({ ok: false });
  });

  it('never repeats the document in a refusal', () => {
    const bad = parseVoiceDevices(Buffer.from('{"schema":"conch.voice_devices.v1","pipewire":"SECRET-WORD","x":1}'));
    expect(bad.ok).toBe(false);
    expect(JSON.stringify(bad)).not.toContain('SECRET-WORD');
  });

  it('carries no field the schema forbids (sensitive.json)', () => {
    const forbidden = (JSON.parse(readFileSync(fixture(`${DIR}/sensitive.json`), 'utf8')) as { forbidden_field_fragments: string[] }).forbidden_field_fragments;
    const doc = readFileSync(fixture(`${DIR}/voice-devices-v1.json`));
    const parsed = parseVoiceDevices(doc);
    if (!parsed.ok) throw new Error('fixture refused');
    const names = new Set<string>();
    const walk = (v: unknown): void => {
      if (Array.isArray(v)) v.forEach(walk);
      else if (typeof v === 'object' && v !== null) {
        for (const [k, x] of Object.entries(v)) {
          names.add(k);
          walk(x);
        }
      }
    };
    walk(parsed.devices);
    for (const name of names) for (const f of forbidden) expect(name.toLowerCase(), `${name} / ${f}`).not.toContain(f);
  });
});

// ----------------------------------------------------------- the save rules

describe('what may be saved', () => {
  it('knows the keys of the issue and no others', () => {
    expect([...SAVE_KEYS]).toEqual(['audio.input', 'audio.output', 'audio.talk_mode', 'keys.device', 'keys.talk']);
    for (const k of ['keys.mute', 'keys.deafen', 'server', 'channel', 'audio', 'toString', '', '--help']) expect(isSaveKey(k), k).toBe(false);
    expect(isSaveKey(5)).toBe(false);
  });

  it.each([
    ['audio.input', 'default', true],
    ['audio.input', 'alsa_input.pci-0000_00_1f.3.analog-stereo', true],
    ['audio.output', '-h', true],
    ['audio.input', 'Mikrofon für Sprache', true],
    ['audio.input', 'x'.repeat(VOICE_DEVICES_MAX_NAME_BYTES), true],
    ['audio.input', 'x'.repeat(VOICE_DEVICES_MAX_NAME_BYTES + 1), false],
    ['audio.input', 'é'.repeat(128), false],
    ['audio.input', '', false],
    ['audio.input', 'a"b', false],
    ['audio.input', 'a\\b', false],
    ['audio.input', 'a\nb', false],
    ['audio.input', 'a\rb', false],
    ['audio.input', 'a\u0000b', false],
    ['audio.input', 'a\u007fb', false],
    ['audio.input', 'a\u0085b', false],
    ['audio.input', 'a\u2028b', false],
    ['audio.input', 'a\u2029b', false],
    ['audio.input', 'a\ud800b', false],
    ['audio.input', 5, false],
    ['audio.input', null, false],
    ['audio.talk_mode', 'open', true],
    ['audio.talk_mode', 'push_to_talk', true],
    ['audio.talk_mode', 'default', false],
    ['audio.talk_mode', 'OPEN', false],
    ['audio.talk_mode', 'hold', false],
    ['audio.talk_mode', 'toString', false],
    ['audio.talk_mode', '', false],
  ] as const)('%s = %j: %s', (key, value, ok) => {
    expect(saveValueFits(key, value)).toBe(ok);
  });

  it('lastLine keeps one printable line', () => {
    expect(lastLine('')).toBe('');
    expect(lastLine('first\nsecond \n\n')).toBe('second');
    expect(lastLine('bad \u001b[31mred\u0000é')).toBe('bad ?[31mred??');
    expect(lastLine('x'.repeat(500))).toHaveLength(160);
  });
});

// --------------------------------------------------------------- the runner

let s: Scratch;
let logs: string[];
let oneshot: OneShot;
let spawned: { command: string; args: readonly string[]; options: Parameters<OneShotSpawnFn>[2] }[];
const children: { pid?: number | undefined }[] = [];

const alive = (pid: number): boolean => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};

// install writes the scenario and a conch-voice wrapper that runs the fake
// with it, in the scratch resources directory.
function install(scenario: Omit<OneShotScenario, 'report'>): void {
  const path = join(s.root, 'oneshot.json');
  writeFileSync(path, JSON.stringify({ ...scenario, report: s.report }));
  const quote = (v: string): string => `'${v.replaceAll("'", `'\\''`)}'`;
  const script = join(s.resources, 'conch-voice');
  writeFileSync(script, `#!/bin/sh\nexec ${quote(process.execPath)} ${quote(FAKE_ONESHOT)} ${quote(path)} "$@"\n`);
  chmodSync(script, 0o755);
}

interface Seen {
  pid: number;
  argv: string[];
  env: Record<string, string>;
  cwd: string;
  stdinIsDevNull: boolean;
}
const seen = (): Seen[] => s.reported() as unknown as Seen[];

const GOOD = fixture(`${DIR}/voice-devices-v1.json`);
const UNAVAILABLE = fixture(`${DIR}/voice-devices-v1-pipewire-unavailable.json`);

function make(over: Partial<OneShotDeps> = {}): OneShot {
  const spawn: OneShotSpawnFn = (command, args, options) => {
    spawned.push({ command, args, options });
    const child = nodeSpawn(command, [...args], options);
    children.push(child);
    return child;
  };
  return new OneShot({
    resources: s.resources,
    env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, CONCH_TOKEN: FAKE_TOKEN, CONCH_VOICE_ATTACH: 'nonce', CONCH_SERVER: 'https://x', LD_PRELOAD: '/evil.so', RUST_LOG: 'trace', LANG: 'C', PATH: '/usr/bin' },
    log: (l) => logs.push(l),
    spawn,
    timeoutMs: 5000,
    drainGraceMs: 100,
    ...over,
  });
}

beforeEach(() => {
  s = scratch();
  logs = [];
  spawned = [];
  oneshot = make();
});
afterEach(async () => {
  await oneshot.cancel();
  for (const c of children.splice(0)) if (c.pid !== undefined && alive(c.pid)) process.kill(c.pid, 'SIGKILL');
  s.cleanup();
});

describe('devices()', () => {
  it('runs `devices --json` with argv only: three descriptors, no shell, not detached, cwd /, no token, no nonce', async () => {
    install({ stdoutFile: GOOD });
    const result = await oneshot.devices(s.config);
    expect(result.ok).toBe(true);
    expect(spawned).toHaveLength(1);
    const run = spawned[0];
    expect(run?.command).toBe(join(s.resources, 'conch-voice'));
    expect(run?.args).toEqual(['devices', '--json']);
    expect(run?.options).toMatchObject({ stdio: ['ignore', 'pipe', 'pipe'], shell: false, detached: false, cwd: '/' });
    const got = seen()[0];
    expect(got?.argv).toEqual(['devices', '--json']);
    expect(got?.cwd).toBe('/');
    expect(got?.stdinIsDevNull).toBe(true);
    // What the helper sees: the allowlisted names and the profile's directory.
    // (Minus what the wrapper's /bin/sh adds for itself.)
    const env = Object.fromEntries(Object.entries(got?.env ?? {}).filter(([k]) => !['PWD', 'OLDPWD', 'SHLVL', '_'].includes(k)));
    expect(env).toEqual({ HOME: s.root, XDG_RUNTIME_DIR: s.run, LANG: 'C', CONCH_CONFIG_DIR: s.config });
    expect(JSON.stringify(got?.env)).not.toContain(FAKE_TOKEN);
    expect(JSON.stringify(spawned[0]?.options.env)).not.toContain(FAKE_TOKEN);
    expect(spawned[0]?.options.env).not.toHaveProperty('CONCH_TOKEN');
    expect(spawned[0]?.options.env).not.toHaveProperty('CONCH_VOICE_ATTACH');
  });

  it('returns the validated document’s microphones, speakers, their defaults and the saved device names only', async () => {
    install({ stdoutFile: GOOD });
    const result = await oneshot.devices(s.config);
    const doc = JSON.parse(readFileSync(GOOD, 'utf8')) as { microphones: unknown[]; speakers: unknown[]; pipewire: string; synced: boolean; configured_input: string };
    expect(result).toEqual({ ok: true, pipewire: doc.pipewire, synced: doc.synced, microphones: doc.microphones, speakers: doc.speakers, configuredInput: doc.configured_input });
    expect(Object.keys(result).sort()).toEqual(['configuredInput', 'microphones', 'ok', 'pipewire', 'speakers', 'synced']);
  });

  it('returns both saved names, listed or not, and none when none is saved', async () => {
    install({ stdoutFile: fixture(`${DIR}/voice-devices-v1-configured-input-absent.json`) });
    const both = await oneshot.devices(s.config);
    expect(both).toMatchObject({ ok: true, configuredInput: 'alsa_input.usb-Generic_USB_Headset-00.mono-fallback', configuredOutput: 'alsa_output.pci-0000_00_1f.3.analog-stereo' });
    install({ stdoutFile: fixture(`${DIR}/voice-devices-v1-config-none.json`) });
    const none = await oneshot.devices(s.config);
    expect(none.ok).toBe(true);
    expect(none).not.toHaveProperty('configuredInput');
    expect(none).not.toHaveProperty('configuredOutput');
  });

  it('passes nothing else of the helper’s document on, however it is named', async () => {
    const doc = JSON.parse(readFileSync(GOOD, 'utf8')) as Record<string, unknown>;
    install({ stdout: JSON.stringify({ ...doc, talk_mode: 'open', token: 'SECRET-TOKEN', path: '/home/x/voice.toml', configured_talk_mode: 'open' }) });
    const result = await oneshot.devices(s.config);
    expect(result.ok).toBe(true);
    expect(Object.keys(result).sort()).toEqual(['configuredInput', 'microphones', 'ok', 'pipewire', 'speakers', 'synced']);
    expect(JSON.stringify(result)).not.toContain('SECRET-TOKEN');
    expect(JSON.stringify(result)).not.toContain('voice.toml');
  });

  it.each([
    ['a control character', 'Mic\u0007x'],
    ['a quotation mark', 'Mic"x'],
    ['the word default', 'default'],
    ['too long a name', 'x'.repeat(VOICE_DEVICES_MAX_NAME_BYTES + 1)],
  ])('refuses a document whose saved name has %s', async (_what, name) => {
    const doc = JSON.parse(readFileSync(GOOD, 'utf8')) as Record<string, unknown>;
    install({ stdout: JSON.stringify({ ...doc, configured_input: name }) });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'bad-output' });
  });

  it('gives the pipewire-unavailable document as it is', async () => {
    install({ stdoutFile: UNAVAILABLE });
    expect(await oneshot.devices(s.config)).toEqual({ ok: true, pipewire: 'unavailable', synced: false, microphones: [], speakers: [] });
  });

  it('fails on output that does not validate, and does not log it', async () => {
    install({ stdout: '{"schema":"conch.voice_devices.v1","pipewire":"SECRET-WORD"}\n' });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'bad-output' });
    expect(logs.join('\n')).not.toContain('SECRET-WORD');
  });

  it('fails on output that is not JSON at all', async () => {
    install({ stdout: 'hello' });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'bad-output' });
  });

  it('accepts a document of exactly the most bytes, and cuts off one byte more and fails', async () => {
    const one = readFileSync(UNAVAILABLE, 'utf8').trim();
    install({ stdout: one + ' '.repeat(VOICE_DEVICES_MAX_BYTES - Buffer.byteLength(one)) });
    expect((await oneshot.devices(s.config)).ok).toBe(true);
    install({ stdout: one + ' '.repeat(VOICE_DEVICES_MAX_BYTES - Buffer.byteLength(one) + 1) });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'too-much-output' });
  });

  it('kills a run that keeps writing past the bound, and reaps it', async () => {
    install({ fill: VOICE_DEVICES_MAX_BYTES * 40, hang: true });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'too-much-output' });
    const pid = seen()[0]?.pid ?? 0;
    expect(alive(pid)).toBe(false);
  });

  it('kills a run past its time with SIGKILL through its ChildProcess, and reaps it', async () => {
    expect(ONE_SHOT_TIMEOUT_MS).toBe(10_000);
    oneshot = make({ timeoutMs: 300 });
    install({ hang: true });
    const t0 = Date.now();
    const result = await oneshot.devices(s.config);
    expect(result).toEqual({ ok: false, error: 'timed-out' });
    expect(Date.now() - t0).toBeLessThan(3000);
    // The pid is the ChildProcess's own, from the spawn seam: the fake helper
    // reports its pid only once Node has started, and a starved machine can
    // take longer than the 300 ms bound to get there (issue #1644), when the
    // report is empty and pid 0 would mean "our own process group".
    const child = children[0] as unknown as { pid?: number; signalCode: string | null; exitCode: number | null };
    expect(child.pid).toBeGreaterThan(0);
    expect(alive(child.pid ?? 0)).toBe(false);
    expect(child.signalCode).toBe('SIGKILL');
  });

  it('the default bound is the issue’s ten seconds', async () => {
    const real = (await import('../../main/voice/oneshot.ts')).ONE_SHOT_TIMEOUT_MS;
    expect(real).toBe(10_000);
  });

  it('does not wait for a grandchild that holds the pipes after the helper has exited', async () => {
    install({ stdout: readFileSync(GOOD, 'utf8'), grandchild: true });
    const sh = join(s.resources, 'conch-voice');
    // The wrapper starts the fake, then a sleeper holding the same pipes.
    const body = readFileSync(sh, 'utf8').replace(/^exec /mu, '').trimEnd();
    writeFileSync(sh, `#!/bin/sh\n${body}\nsleep 30 &\n`);
    const t0 = Date.now();
    expect((await oneshot.devices(s.config)).ok).toBe(true);
    expect(Date.now() - t0).toBeLessThan(4000);
    // Reap the sleeper.
    spawned.length = 0;
  });

  it.each([
    [2, 'unsupported'],
    [1, 'failed'],
    [101, 'failed'],
  ])('exit status %i is %s', async (exitCode, error) => {
    install({ stdoutFile: GOOD, exitCode, stderr: 'conch-voice: nope\n' });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error });
    expect(logs.join('\n')).toContain('conch-voice: nope');
  });

  it('logs the last line of standard error cleaned of control characters, and never the token', async () => {
    install({ exitCode: 1, stderr: `old line\nvoice.toml refused \u001b[31m${'x'.repeat(400)}\n` });
    await oneshot.devices(s.config);
    const log = logs.join('\n');
    expect(log).toContain('voice.toml refused ?[31m');
    expect(log).not.toContain('\u001b');
    expect(log).not.toContain('old line');
    expect(log).not.toContain(FAKE_TOKEN);
    expect(log.length).toBeLessThan(400);
  });

  it('says not-installed with no helper, and runs nothing', async () => {
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'not-installed' });
    expect(spawned).toEqual([]);
  });

  it('says untrusted for a helper others can write, a link, and a helper outside the resources, and runs nothing', async () => {
    install({ stdoutFile: GOOD });
    chmodSync(join(s.resources, 'conch-voice'), 0o775);
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'untrusted' });
    chmodSync(join(s.resources, 'conch-voice'), 0o755);
    rmSync(join(s.resources, 'conch-voice'));
    writeFileSync(join(s.root, 'elsewhere'), '#!/bin/sh\nexit 0\n', { mode: 0o755 });
    const { symlinkSync } = await import('node:fs');
    symlinkSync(join(s.root, 'elsewhere'), join(s.resources, 'conch-voice'));
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'untrusted' });
    expect(spawned).toEqual([]);
  });

  it('refuses a configuration directory that is not absolute, and runs nothing', async () => {
    install({ stdoutFile: GOOD });
    expect(await oneshot.devices('relative/dir')).toEqual({ ok: false, error: 'spawn-failed' });
    expect(spawned).toEqual([]);
  });
});

describe('save()', () => {
  it('runs `config set <key> -- <value>`: the value after `--`, as one argument, and prints nothing to the page', async () => {
    install({});
    expect(await oneshot.save(s.config, 'audio.input', 'alsa_input.usb-Mic_1')).toEqual({ ok: true });
    expect(seen()[0]?.argv).toEqual(['config', 'set', 'audio.input', '--', 'alsa_input.usb-Mic_1']);
    expect(spawned[0]?.options).toMatchObject({ stdio: ['ignore', 'pipe', 'pipe'], shell: false, detached: false, cwd: '/' });
    expect(seen()[0]?.stdinIsDevNull).toBe(true);
    expect(seen()[0]?.env).not.toHaveProperty('CONCH_TOKEN');
    expect(seen()[0]?.env['CONCH_CONFIG_DIR']).toBe(s.config);
  });

  it.each(['-h', '--help', '-', '--', '-x', '--version'])('a name that begins with a dash (%s) comes after `--`, so it cannot be an option', async (name) => {
    install({});
    expect((await oneshot.save(s.config, 'audio.output', name)).ok).toBe(true);
    const argv = seen()[0]?.argv ?? [];
    expect(argv.slice(0, 4)).toEqual(['config', 'set', 'audio.output', '--']);
    expect(argv[4]).toBe(name);
    expect(argv).toHaveLength(5);
  });

  it('passes shell syntax through as one literal argument, run by no shell', async () => {
    install({});
    const name = '$(touch pwned); `id` | cat > /tmp/x && echo \'hi\' *';
    expect((await oneshot.save(s.config, 'audio.input', name)).ok).toBe(true);
    expect(seen()[0]?.argv.at(-1)).toBe(name);
    expect(spawned[0]?.args).toEqual(['config', 'set', 'audio.input', '--', name]);
  });

  it('saves the talk mode, and `default` for a device', async () => {
    install({});
    expect((await oneshot.save(s.config, 'audio.talk_mode', 'open')).ok).toBe(true);
    expect((await oneshot.save(s.config, 'audio.input', 'default')).ok).toBe(true);
    expect(seen().map((r) => r.argv)).toEqual([
      ['config', 'set', 'audio.talk_mode', '--', 'open'],
      ['config', 'set', 'audio.input', '--', 'default'],
    ]);
  });

  it.each([
    ['audio.input', 'a"b'],
    ['audio.input', ''],
    ['audio.input', 'a\nb'],
    ['audio.input', 'x'.repeat(256)],
    ['audio.talk_mode', 'hold'],
    ['keys.talk', 'KEY_F13'],
    ['server', 'http://evil.example'],
  ])('refuses %s = %j before anything runs', async (key, value) => {
    install({});
    expect(await oneshot.save(s.config, key as 'audio.input', value)).toEqual({ ok: false, error: 'invalid' });
    expect(spawned).toEqual([]);
  });

  it.each([
    [2, 'unsupported'],
    [1, 'failed'],
  ])('exit status %i is %s', async (exitCode, error) => {
    install({ exitCode, stderr: 'conch-voice: could not save\n' });
    expect(await oneshot.save(s.config, 'audio.input', 'Mic')).toEqual({ ok: false, error });
  });

  it('kills a save past its time', async () => {
    oneshot = make({ timeoutMs: 300 });
    install({ hang: true });
    expect(await oneshot.save(s.config, 'audio.input', 'Mic')).toEqual({ ok: false, error: 'timed-out' });
    expect(alive(seen()[0]?.pid ?? 0)).toBe(false);
  });
});

describe('one at a time, and cancel', () => {
  it('refuses a second run while one is in progress, whichever kind, and runs it once the first has ended', async () => {
    install({ stdoutFile: GOOD, hang: true });
    const first = oneshot.devices(s.config);
    // Same tick: the first has not even found the helper yet.
    expect(await oneshot.save(s.config, 'audio.input', 'Mic')).toEqual({ ok: false, error: 'busy' });
    expect(await oneshot.devices(s.config)).toEqual({ ok: false, error: 'busy' });
    while (seen().length === 0) await new Promise((r) => setTimeout(r, 10));
    await oneshot.cancel();
    expect(await first).toEqual({ ok: false, error: 'cancelled' });
    install({});
    expect((await oneshot.save(s.config, 'audio.input', 'Mic')).ok).toBe(true);
    expect(seen().filter((r) => r.argv[0] === 'devices')).toHaveLength(1);
  });

  it('cancel kills a running one-shot and resolves only once it is reaped', async () => {
    install({ hang: true });
    const run = oneshot.devices(s.config);
    while (seen().length === 0) await new Promise((r) => setTimeout(r, 10));
    const pid = seen()[0]?.pid ?? 0;
    expect(alive(pid)).toBe(true);
    await oneshot.cancel();
    expect(alive(pid)).toBe(false);
    expect(await run).toEqual({ ok: false, error: 'cancelled' });
  });

  it('cancel before the helper has started stops it from starting', async () => {
    install({ stdoutFile: GOOD });
    const run = oneshot.devices(s.config);
    await oneshot.cancel();
    expect(await run).toEqual({ ok: false, error: 'cancelled' });
    expect(spawned).toEqual([]);
  });

  it('cancel with nothing running does nothing', async () => {
    await oneshot.cancel();
    install({ stdoutFile: GOOD });
    expect((await oneshot.devices(s.config)).ok).toBe(true);
  });

  it('a start that fails leaves it free for the next', async () => {
    const failing = make({
      spawn: () => {
        throw new Error('EAGAIN');
      },
    });
    install({ stdoutFile: GOOD });
    expect(await failing.devices(s.config)).toEqual({ ok: false, error: 'spawn-failed' });
    expect(await failing.devices(s.config)).toEqual({ ok: false, error: 'spawn-failed' });
  });
});

describe('the scratch directory', () => {
  it('is under a directory the helper check accepts', () => {
    expect(s.resources.startsWith(scratchBase())).toBe(true);
    const base = mkdtempSync(join(scratchBase(), 'conch-voice-x-'));
    mkdirSync(join(base, 'a'));
    rmSync(base, { recursive: true, force: true });
  });
});
