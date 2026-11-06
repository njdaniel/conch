// One-shot runs of the voice helper (issue #1378, D3-7 of #771;
// linux-desktop.md §7.2, tui-voice.md §11; the Go reference is
// internal/cli/voicehelper/oneshot.go): `conch-voice devices --json` lists the
// microphones and speakers, and `conch-voice config set <key> -- <value>`
// saves a choice, each a short process beside the managed one.
//
// Input: the profile's configuration directory and, for a save, a key and
// a value. Output: the validated device list, or a save's success, or a typed
// failure. Owns: the one child at a time, its timeout, its output bound and
// its reaping.
//
// What it keeps:
//   - the helper is found by the same trust checks as the managed one
//     (findHelper) in the app's own resources directory, and nothing else is
//     tried in its place;
//   - argv only: no shell; the value comes after `--`, so that a name that
//     begins with `-` can never be read as an option;
//   - the environment is the managed helper's allowlist and the profile's
//     CONCH_CONFIG_DIR, with no CONCH_TOKEN and no nonce (neither command
//     needs a login); the working directory is /; standard input is
//     /dev/null; the child is not detached;
//   - standard output is read up to VOICE_DEVICES_MAX_BYTES and no further;
//     standard error is kept as one short, printable line, for main's log
//     alone and never for the page;
//   - a run past ONE_SHOT_TIMEOUT_MS is SIGKILLed and reaped;
//   - one run at a time: a second is refused 'busy', and cancel() (the
//     profile-switch media hook) kills the one running and waits for it.
import { VOICE_DEVICES_MAX_BYTES, voiceControlTalkMode } from '../../../web/src/schema/generated.ts';
import type { NativeVoiceDevicesResult, VoiceBindingKey, VoiceDeviceKey, VoiceOneShotError, VoiceSaveResult } from '../../../web/src/platform/desktop/bridge.ts';
import { parseVoiceDevices } from './devices.ts';
import { BY_ID_DIR, fitForARule } from './keydevices.ts';
import { validDeviceName } from './protocol.ts';
import { defaultOneShotSpawn, findHelper, HelperError, oneShotEnv, oneShotSpawnOptions, type FindDeps, type OneShotChild, type OneShotSpawnFn } from './spawn.ts';

// A run that has not ended by itself in this long is killed.
export const ONE_SHOT_TIMEOUT_MS = 10_000;

// How long, after the process has exited, its pipes may stay open (something
// it started may hold a write end) before they are closed from our side.
const DRAIN_GRACE_MS = 500;

// The most of standard error kept, and shown in the log.
const STDERR_TAIL_BYTES = 1024;
const STDERR_LINE_CHARS = 160;

// The exit status conch-voice gives for a command line it does not take.
const USAGE_EXIT = 2;

// The keys `config set` is given: the page's own choices (the devices and the
// talk mode) and, from the talk-key setup alone, the key device and the talk
// key (issue #1381). The page cannot name the latter: see PAGE_SAVE_KEYS.
export type VoiceSaveKey = VoiceDeviceKey | VoiceBindingKey;
export const PAGE_SAVE_KEYS: readonly VoiceDeviceKey[] = ['audio.input', 'audio.output', 'audio.talk_mode'];
export const SAVE_KEYS: readonly VoiceSaveKey[] = [...PAGE_SAVE_KEYS, 'keys.device', 'keys.talk'];

// The highest key code conch-voice takes for keys.talk.
export const MAX_KEY_CODE = 767;

const NO_SEPARATOR = /[\u2028\u2029]/u;

export const DEFAULT_DEVICE = 'default';

export function isSaveKey(value: unknown): value is VoiceSaveKey {
  return typeof value === 'string' && (SAVE_KEYS as readonly string[]).includes(value);
}

// isPageSaveKey: the keys the page may name in voiceSaveDevice.
export function isPageSaveKey(value: unknown): value is VoiceDeviceKey {
  return typeof value === 'string' && (PAGE_SAVE_KEYS as readonly string[]).includes(value);
}

// keyCodeText is the decimal text of a key code 1..767, or null.
export function keyCodeText(code: unknown): string | null {
  return typeof code === 'number' && Number.isInteger(code) && code >= 1 && code <= MAX_KEY_CODE ? String(code) : null;
}

// saveValueFits says whether value can be given to `config set` for key: for
// a device, `default` or a printable name of 1 to 255 bytes with no quotation
// mark, backslash, control character or line separator (the device list's own
// rule); for the talk mode, a word of its vocabulary; for keys.device, the
// by-id path of a name fit for a udev rule (stricter than the helper, which
// takes any clean path under /dev/input/); for keys.talk, a canonical decimal
// code from 1 to 767 (the helper also takes KEY_* names). It is checked here,
// in the IPC layer and again by the service, before anything is passed.
export function saveValueFits(key: unknown, value: unknown): value is string {
  if (!isSaveKey(key) || typeof value !== 'string') return false;
  if (key === 'audio.talk_mode') return voiceControlTalkMode(value).length === 0;
  if (key === 'keys.talk') return /^[1-9]\d{0,2}$/u.test(value) && keyCodeText(Number(value)) === value;
  if (key === 'keys.device') return value.startsWith(`${BY_ID_DIR}/`) && value.length <= BY_ID_DIR.length + 1 + 200 && fitForARule(value.slice(BY_ID_DIR.length + 1));
  return value === DEFAULT_DEVICE || (validDeviceName(value) && !NO_SEPARATOR.test(value));
}

export interface OneShotDeps {
  // The app's resources directory, where the bundled conch-voice is.
  readonly resources: string;
  // main's environment, read only for the allowlisted names.
  readonly env: NodeJS.ProcessEnv;
  readonly log: (line: string) => void;
  // Test seams.
  readonly spawn?: OneShotSpawnFn;
  readonly find?: FindDeps;
  readonly timeoutMs?: number;
  readonly drainGraceMs?: number;
}

type Ran = { readonly ok: true; readonly stdout: Buffer } | { readonly ok: false; readonly error: VoiceOneShotError };

interface Active {
  cancelled: boolean;
  // Kills the child, if there is one yet.
  abort: () => void;
  // Settles once the run has ended and its child is reaped.
  finished: Promise<void>;
}

// lastLine is the last line of standard error that says something, cut to
// printable ASCII: it is device-supplied or the helper's own text, and goes to
// the log alone.
export function lastLine(tail: string): string {
  const lines = tail.split(/[\r\n]+/u);
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = (lines[i] ?? '').replace(/[^\x20-\x7e]/gu, '?').trim();
    if (line !== '') return line.slice(0, STDERR_LINE_CHARS);
  }
  return '';
}

export class OneShot {
  #active: Active | null = null;

  constructor(private readonly deps: OneShotDeps) {}

  // devices runs `conch-voice devices --json` and reads what it writes.
  async devices(configDir: string): Promise<NativeVoiceDevicesResult> {
    const ran = await this.#run(configDir, ['devices', '--json'], 'devices');
    if (!ran.ok) return ran;
    const parsed = parseVoiceDevices(ran.stdout);
    if (!parsed.ok) {
      this.deps.log(`voice: devices: the list was refused: ${parsed.detail}`);
      return { ok: false, error: 'bad-output' };
    }
    const d = parsed.devices;
    return {
      ok: true,
      pipewire: d.pipewire,
      synced: d.synced,
      microphones: d.microphones.map(({ name, description, default: isDefault }) => ({ name, description, default: isDefault })),
      speakers: d.speakers.map(({ name, description, default: isDefault }) => ({ name, description, default: isDefault })),
    };
  }

  // save runs `conch-voice config set <key> -- <value>`. A key or a value that
  // could not be saved is refused before anything runs.
  async save(configDir: string, key: VoiceSaveKey, value: string): Promise<VoiceSaveResult> {
    if (!saveValueFits(key, value)) return { ok: false, error: 'invalid' };
    // `--` ends the options: a device name may begin with `-` (PipeWire lets any
    // program name a node), and without it clap would read `-h` as help, print
    // it and exit 0, which would look like a save.
    // A key device's path is a device's name: the helper's words are not logged.
    const ran = await this.#run(configDir, ['config', 'set', key, '--', value], 'config set', key === 'keys.device');
    return ran.ok ? { ok: true } : ran;
  }

  // cancel kills the run in progress, if any, and resolves once it is reaped.
  async cancel(): Promise<void> {
    const run = this.#active;
    if (run === null) return;
    run.cancelled = true;
    run.abort();
    await run.finished;
  }

  async #run(configDir: string, args: readonly string[], what: string, quiet = false): Promise<Ran> {
    if (this.#active !== null) return { ok: false, error: 'busy' };
    // Taken before the first await: two calls cannot both pass the check.
    let release: () => void = () => undefined;
    const run: Active = {
      cancelled: false,
      abort: () => undefined,
      finished: new Promise<void>((resolve) => {
        release = resolve;
      }),
    };
    this.#active = run;
    try {
      return await this.#exec(run, configDir, args, what, quiet);
    } finally {
      this.#active = null;
      release();
    }
  }

  async #exec(run: Active, configDir: string, args: readonly string[], what: string, quiet: boolean): Promise<Ran> {
    let executable: string;
    try {
      executable = await findHelper(this.deps.resources, this.deps.find);
    } catch (err) {
      if (err instanceof HelperError) {
        this.deps.log(`voice: ${what}: ${err.message}`);
        return { ok: false, error: err.reason };
      }
      return { ok: false, error: 'untrusted' };
    }
    if (run.cancelled) return { ok: false, error: 'cancelled' };
    let child: OneShotChild;
    try {
      child = (this.deps.spawn ?? defaultOneShotSpawn)(executable, args, oneShotSpawnOptions(oneShotEnv(this.deps.env, configDir)));
    } catch {
      this.deps.log(`voice: ${what}: the helper could not be started`);
      return { ok: false, error: 'spawn-failed' };
    }
    return this.#await(run, child, what, quiet);
  }

  #await(run: Active, child: OneShotChild, what: string, quiet: boolean): Promise<Ran> {
    const timeoutMs = this.deps.timeoutMs ?? ONE_SHOT_TIMEOUT_MS;
    const graceMs = this.deps.drainGraceMs ?? DRAIN_GRACE_MS;
    return new Promise<Ran>((resolve) => {
      const chunks: Buffer[] = [];
      let size = 0;
      let tail = '';
      let stopped: VoiceOneShotError | null = null;
      let exit: { readonly code: number | null } | null = null;
      let settled = false;
      let drain: NodeJS.Timeout | undefined;

      const kill = (): void => {
        try {
          child.kill('SIGKILL');
        } catch {
          // Already gone.
        }
      };
      const stop = (why: VoiceOneShotError): void => {
        stopped ??= why;
        kill();
      };
      const timer = setTimeout(() => {
        stop('timed-out');
      }, timeoutMs);
      run.abort = () => {
        stop('cancelled');
      };
      if (run.cancelled) stop('cancelled');

      const settle = (): void => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        clearTimeout(drain);
        child.stdout.destroy();
        child.stderr.destroy();
        const line = quiet ? '' : lastLine(tail);
        const log = (reason: string): void => {
          this.deps.log(`voice: ${what}: ${reason}${line === '' ? '' : `: ${line}`}`);
        };
        if (stopped !== null) {
          log(stopped);
          resolve({ ok: false, error: stopped });
        } else if (exit?.code === 0) {
          resolve({ ok: true, stdout: Buffer.concat(chunks) });
        } else if (exit?.code === USAGE_EXIT) {
          log('unsupported (exit status 2)');
          resolve({ ok: false, error: 'unsupported' });
        } else {
          log(exit?.code === null || exit === null ? 'failed' : `failed (exit status ${String(exit.code)})`);
          resolve({ ok: false, error: 'failed' });
        }
      };

      child.stdout.on('data', (chunk: Buffer) => {
        if (stopped !== null) return;
        size += chunk.length;
        // One byte more than the bound tells a document at the bound from one
        // past it; the rest is never kept.
        if (size > VOICE_DEVICES_MAX_BYTES) {
          stop('too-much-output');
          return;
        }
        chunks.push(chunk);
      });
      child.stderr.on('data', (chunk: Buffer) => {
        tail = (tail + chunk.toString('latin1')).slice(-STDERR_TAIL_BYTES);
      });
      // Streams may error when destroyed; nothing to report.
      child.stdout.on('error', () => undefined);
      child.stderr.on('error', () => undefined);
      child.once('error', () => {
        // Failed to start (or to be killed): there may be no exit event.
        stopped ??= 'spawn-failed';
        settle();
      });
      child.once('exit', (code) => {
        exit = { code };
        // The pipes end with the process; if something it started holds a
        // write end, they are closed from here after a grace.
        drain = setTimeout(settle, graceMs);
      });
      child.once('close', settle);
    });
  }
}
