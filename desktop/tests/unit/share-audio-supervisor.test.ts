// The owner's supervisor with the Linux share-sound switch on and off (issue
// #1854), against the fake conch-voice replaying the v3 transcripts of
// pkg/schema/testdata/voice-control-v3/ over the real endpoint, spawn and
// protocol modules: v3 negotiated when the helper offers it, the commands of a
// share's sound written exactly as the transcript's owner writes them, the
// helper's confirmed state in the view, and v2 (silent) when the switch is off
// or the helper has no v3. No network beyond a Unix socket, no real helper.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { OWNER_VERSIONS_SHARE_AUDIO } from '../../main/voice/protocol.ts';
import { Supervisor, type StartRequest, type VoiceState } from '../../main/voice/supervisor.ts';
import { FAKE_TOKEN, fixture, ownerLines, scratch, until, type Scratch } from './voice-support.ts';

let s: Scratch;
let sup: Supervisor | null;
let states: VoiceState[];

beforeEach(() => {
  s = scratch();
  sup = null;
  states = [];
});
afterEach(async () => {
  await sup?.stop();
  s.cleanup();
});

function supervisor(switchOn: boolean): Supervisor {
  sup = new Supervisor({
    resources: s.resources,
    env: { HOME: s.root, XDG_RUNTIME_DIR: s.run, PATH: process.env['PATH'] ?? '' },
    log: () => undefined,
    timings: { helloTimeoutMs: 5000, ackTimeoutMs: 5000, termAfterMs: 300, killAfterMs: 700 },
    ...(switchOn ? { versions: OWNER_VERSIONS_SHARE_AUDIO } : {}),
  });
  sup.subscribe((st) => states.push(st));
  return sup;
}

const request: StartRequest = {
  profileId: 'p1',
  configDir: '/nonexistent/config/p1',
  token: FAKE_TOKEN,
  server: 'https://conch.example.org',
  channel: 'general',
  principalId: 7,
};

function stdin(): Record<string, unknown>[] {
  return s
    .reported()
    .filter((r) => r['t'] === 'stdin')
    .map((r) => JSON.parse(String(r['line'])) as Record<string, unknown>);
}

const withoutCredential = (lines: Record<string, unknown>[]): Record<string, unknown>[] =>
  lines.map((l) => Object.fromEntries(Object.entries(l).filter(([k]) => k !== 'credential')));

function running(v: Supervisor) {
  const st = v.state;
  return st.status === 'running' ? st.helper : null;
}

describe('with the switch on', () => {
  it('negotiates v3 when the helper offers it, and writes a share’s sound exactly as the transcript’s owner does', async () => {
    const t = 'voice-control-v3/transcript-share-audio.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor(true);
    const started = await v.start(request);
    expect(started.status === 'running' && started.helper.version).toBe(3);
    // Before anything is asked: the sound is off, nothing counted.
    await until(() => running(v)?.phase === 'connected', 5000, 'connected');
    expect(running(v)?.shareAudio).toEqual({ state: 'off', muted: false, sources: 0 });

    expect(await v.setShareAudio({ capturing: true, applicationName: 'Conch' })).toEqual({ ok: true });
    // The ack follows the state that says how it went; later states follow it.
    await until(() => running(v)?.shareAudio?.sources === 1, 5000, 'the count to follow what is playing');
    expect(running(v)?.shareAudio).toEqual({ state: 'on', muted: false, sources: 1 });

    expect(await v.setShareAudioMute(true)).toEqual({ ok: true });
    await until(() => running(v)?.shareAudio?.muted === true, 5000, 'the mute');
    // The microphone's own mute and deafen did not move.
    expect(running(v)).toMatchObject({ muted: false, deafened: false });
    expect(await v.setShareAudioMute(false)).toEqual({ ok: true });
    expect(await v.setShareAudio({ capturing: false })).toEqual({ ok: true });
    await until(() => running(v)?.shareAudio?.state === 'off', 5000, 'off');
    await v.stop();
    expect(v.state).toEqual({ status: 'idle' });

    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
    expect(stdin().every((l) => l['schema'] === 'conch.voice_control.v3')).toBe(true);
    // Never the application scope, and a stop carries no scope and no name.
    expect(stdin().map((l) => l['scope'])).toEqual([undefined, 'system_excluding_call', undefined, undefined, undefined, undefined]);
    expect(stdin().map((l) => l['application_name'])).toEqual([undefined, 'Conch', undefined, undefined, undefined, undefined]);
  });

  it('shows no_sources as on with nothing playing, and the helper’s refusal as unavailable with its reason', async () => {
    const t = 'voice-control-v3/transcript-share-audio-publish-refused.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor(true);
    await v.start(request);
    await until(() => running(v)?.phase === 'connected', 5000, 'connected');
    expect(await v.setShareAudio({ capturing: true, applicationName: 'Conch' })).toEqual({ ok: true });
    // The refusal is in the state, before the ack; the ack is ok.
    await until(() => states.some((st) => st.status === 'running' && st.helper.shareAudio?.problem === 'publish_refused'), 5000, 'the refusal');
    const refused = states.find((st) => st.status === 'running' && st.helper.shareAudio?.problem === 'publish_refused');
    expect(refused?.status === 'running' && refused.helper.shareAudio).toEqual({ state: 'unavailable', problem: 'publish_refused', muted: false, sources: 0 });
    // The same command again, as the transcript's owner does after a pass.
    expect(await v.setShareAudio({ capturing: true, applicationName: 'Conch' })).toEqual({ ok: true });
    await until(() => running(v)?.shareAudio?.state === 'on', 5000, 'on');
    expect(await v.setShareAudio({ capturing: false })).toEqual({ ok: true });
    await v.stop();
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
  });

  it('a v3 helper that offers v1, v2 and v3 to an owner that has v3 gets v3 from the first line (get_state)', async () => {
    const t = 'voice-control-v3/transcript-negotiation-v3-owner.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor(true);
    expect((await v.start(request)).status).toBe('running');
    expect(await v.refresh()).toEqual({ ok: true });
    await v.stop();
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
  });

  it('falls back to v2 against a helper that has no v3: silent, and the commands are unsupported, nothing written', async () => {
    const t = 'voice-control-v2/transcript-negotiation-v2-owner.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor(true);
    const started = await v.start(request);
    expect(started.status === 'running' && started.helper.version).toBe(2);
    expect(running(v)?.shareAudio).toBeUndefined();
    expect(await v.setShareAudio({ capturing: true, applicationName: 'Conch' })).toEqual({ ok: false, error: 'unsupported' });
    expect(await v.setShareAudioMute(true)).toEqual({ ok: false, error: 'unsupported' });
    expect(await v.refresh()).toEqual({ ok: true });
    await v.stop();
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
    expect(stdin().every((l) => l['schema'] === 'conch.voice_control.v2')).toBe(true);
  });

  it('falls back to v1 against a v1 helper', async () => {
    const t = 'voice-control/transcript-join-mute-leave.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor(true);
    const started = await v.start(request);
    expect(started.status === 'running' && started.helper.version).toBe(1);
    expect(await v.setShareAudio({ capturing: false })).toEqual({ ok: false, error: 'unsupported' });
    await v.stop();
    expect(stdin().every((l) => l['schema'] === 'conch.voice_control.v1')).toBe(true);
  });
});

describe('with the switch off (the default)', () => {
  it('offers the versions it always did: a helper that offers v3 is spoken to in v2, silent, and never sent a share-audio command', async () => {
    const t = 'voice-control-v3/transcript-negotiation-v2-owner.json';
    s.helper({ transcript: fixture(t) });
    const v = supervisor(false);
    const started = await v.start(request);
    expect(started.status === 'running' && started.helper.version).toBe(2);
    expect(running(v)?.shareAudio).toBeUndefined();
    expect(await v.setShareAudio({ capturing: true, applicationName: 'Conch' })).toEqual({ ok: false, error: 'unsupported' });
    expect(await v.refresh()).toEqual({ ok: true });
    await v.stop();
    expect(withoutCredential(stdin())).toEqual(withoutCredential(ownerLines(t)));
    expect(stdin().every((l) => l['schema'] === 'conch.voice_control.v2')).toBe(true);
  });

  it('a helper that offers v1 and v3 only (no v2) is not spoken to in v3', async () => {
    s.helper({ transcript: fixture('voice-control/transcript-join-mute-leave.json'), helloVersions: ['conch.voice_control.v1', 'conch.voice_control.v3'] });
    const v = supervisor(false);
    const started = await v.start(request);
    expect(started.status === 'running' && started.helper.version).toBe(1);
  });

  it('a helper that offers only v3 is refused before any start', async () => {
    s.helper({ transcript: fixture('voice-control-v3/transcript-negotiation-v3-owner.json'), helloVersions: ['conch.voice_control.v3'] });
    const v = supervisor(false);
    expect(await v.start(request)).toEqual({ status: 'failed', generation: 1, reason: 'version-mismatch' });
    await until(() => s.reported().some((r) => r['t'] === 'stdin-end'), 5000, 'standard input to close');
    expect(stdin()).toEqual([]);
  });
});
