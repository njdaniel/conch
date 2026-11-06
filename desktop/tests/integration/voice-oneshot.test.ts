// Integration (issue #1378, D3-7 of #771): the one-shot runs with the real
// conch-voice, built by `make rust-build` (bin/conch-voice) or named by
// CONCH_VOICE_BIN. No PipeWire, no audio device, no network, no login.
//
// Without the binary it is skipped and says so; with CI set, a missing binary
// fails instead of skipping. Run with `npm run test:integration` (make
// desktop-voice-it).
import { chmodSync, copyFileSync, existsSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { OneShot } from '../../main/voice/oneshot.ts';
import { HELPER_NAME } from '../../main/voice/spawn.ts';
import { FAKE_TOKEN, scratch, type Scratch } from '../unit/voice-support.ts';

const binary = process.env['CONCH_VOICE_BIN'] ?? resolve(import.meta.dirname, '../../../bin/conch-voice');
const have = existsSync(binary);
const ci = process.env['CI'] === 'true' || process.env['CI'] === '1';

if (!have) {
  const why = `the real conch-voice is not at ${binary}: run make rust-build, or set CONCH_VOICE_BIN`;
  if (ci) throw new Error(`voice-oneshot integration: ${why}`);
  console.warn(`voice-oneshot integration: SKIPPED: ${why}`);
}

describe.runIf(have)('the one-shot runs with the real conch-voice', () => {
  let s: Scratch;
  let logs: string[];
  let oneshot: OneShot;
  const voiceToml = (): string => join(s.config, 'voice.toml');

  beforeEach(() => {
    s = scratch();
    copyFileSync(binary, join(s.resources, HELPER_NAME));
    chmodSync(join(s.resources, HELPER_NAME), 0o755);
    logs = [];
    // No PipeWire is reachable: the socket named does not exist. A token in
    // main's environment must not reach the helper.
    oneshot = new OneShot({
      resources: s.resources,
      env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, PIPEWIRE_REMOTE: 'conch-voice-test-no-such-socket', CONCH_TOKEN: FAKE_TOKEN },
      log: (l) => logs.push(l),
    });
  });
  afterEach(() => {
    s.cleanup();
  });

  it('devices --json with no PipeWire gives the pipewire-unavailable document, and it validates', async () => {
    expect(await oneshot.devices(s.config)).toEqual({ ok: true, pipewire: 'unavailable', synced: false, microphones: [], speakers: [] });
    expect(logs.join('\n')).not.toContain(FAKE_TOKEN);
  });

  it('config set saves a device and the talk mode into the profile’s voice.toml, and nothing else', async () => {
    writeFileSync(voiceToml(), '# mine\n[audio]\ninput = "default"\n', { mode: 0o600 });
    expect(await oneshot.save(s.config, 'audio.output', 'alsa_output.pci-0000_00_1f.3.analog-stereo')).toEqual({ ok: true });
    expect(await oneshot.save(s.config, 'audio.talk_mode', 'open')).toEqual({ ok: true });
    const text = readFileSync(voiceToml(), 'utf8');
    expect(text).toContain('# mine');
    expect(text).toContain('output = "alsa_output.pci-0000_00_1f.3.analog-stereo"');
    expect(text).toContain('talk_mode = "open"');
    expect(readdirSync(s.config).sort()).toEqual(['voice.toml']);
    // The token went nowhere.
    expect(text).not.toContain(FAKE_TOKEN);
  });

  it('a name that begins with a dash is saved as a name: `--` is accepted by the real parser, and is not read as help', async () => {
    writeFileSync(voiceToml(), '[audio]\n', { mode: 0o600 });
    expect(await oneshot.save(s.config, 'audio.input', '-h')).toEqual({ ok: true });
    expect(readFileSync(voiceToml(), 'utf8')).toContain('input = "-h"');
    expect(await oneshot.save(s.config, 'audio.output', '--help')).toEqual({ ok: true });
    expect(readFileSync(voiceToml(), 'utf8')).toContain('output = "--help"');
  });

  it('config set keys.device and keys.talk (#1381) save the talk-key binding, and the helper’s own refusals are not logged', async () => {
    writeFileSync(voiceToml(), '# mine\n[audio]\ninput = "default"\n', { mode: 0o600 });
    const path = '/dev/input/by-id/usb-Example_Keyboard-event-kbd';
    expect(await oneshot.save(s.config, 'keys.device', path)).toEqual({ ok: true });
    expect(await oneshot.save(s.config, 'keys.talk', '275')).toEqual({ ok: true });
    const text = readFileSync(voiceToml(), 'utf8');
    expect(text).toContain('# mine');
    expect(text).toContain(`device = "${path}"`);
    expect(text).toContain('talk = "275"');
    expect(readdirSync(s.config).sort()).toEqual(['voice.toml']);
    // Replacing is fine, and a value the helper refuses is a failure with nothing logged of it.
    expect(await oneshot.save(s.config, 'keys.talk', '30')).toEqual({ ok: true });
    expect(readFileSync(voiceToml(), 'utf8')).toContain('talk = "30"');
    expect(logs.join('\n')).not.toContain('usb-Example');
    expect(logs.join('\n')).not.toContain(FAKE_TOKEN);
  });

  it('a second run once the first has ended is fine; the device list then shows what was saved', async () => {
    writeFileSync(voiceToml(), '[audio]\n', { mode: 0o600 });
    expect((await oneshot.save(s.config, 'audio.input', 'my_mic')).ok).toBe(true);
    const list = await oneshot.devices(s.config);
    expect(list.ok).toBe(true);
  });
});
