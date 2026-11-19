// Main's part of the Linux screen share's sound (issue #1854;
// docs/design/linux-share-audio.md §5.3, §10, §11): the order (consent, then
// command; on every end, command, then DELETE), every end, the stale share, the
// failures and their words, the independent mute, and a switch that is off.
// The real Companion, VoiceService and ShareSound run over a fake helper and a
// fake conchd (share-audio.support.ts); no network, no helper, no Electron.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { Lifecycle } from '../../main/lifecycle.ts';
import { shareTeardown } from '../../main/share-hook.ts';
import {
  CONSENT_RETRY_MS,
  PUBLISH_RETRY_MS,
  STOP_ACK_BOUND_MS,
  linuxShareAudioEnabled,
  registeredApplicationName,
} from '../../main/voice/share-audio.ts';
import { VOICE_CONTROL_CAPTURE_STOP_MS } from '../../../web/src/schema/generated.ts';
import { ANSWER, DOC, STARTED, consent, deferred, expectNeverApplicationScope, expectStopThenWithdraw, ok, refusal, rig, settle, trace, type Rig } from './share-audio.support.ts';

afterEach(() => {
  vi.useRealTimers();
});

// A share with its sound on: joined, a companion, the consent and the
// command, and the log cleared.
async function sounding(r: Rig): Promise<number> {
  await r.live();
  const started = await r.sound?.start(DOC);
  expect(started).toMatchObject({ ok: true });
  expect(trace(r, 'http:PUT audio', 'helper:capture on')).toEqual(['http:PUT audio', 'helper:capture on']);
  r.events.length = 0;
  r.sends.length = 0;
  return started?.ok === true ? started.share : -1;
}

describe('the switch', () => {
  it('is on by default on Linux, off for CONCH_LINUX_SHARE_AUDIO=0 exactly, and never elsewhere (#1952)', () => {
    expect(linuxShareAudioEnabled({}, 'linux')).toBe(true);
    for (const value of [undefined, '', '1', 'true', 'yes', 'off', ' 0', '0 ', '00', 'false']) {
      expect(linuxShareAudioEnabled({ CONCH_LINUX_SHARE_AUDIO: value }, 'linux'), String(value)).toBe(true);
    }
    expect(linuxShareAudioEnabled({ CONCH_LINUX_SHARE_AUDIO: '0' }, 'linux')).toBe(false);
    for (const platform of ['win32', 'darwin'] as const) {
      expect(linuxShareAudioEnabled({}, platform)).toBe(false);
      expect(linuxShareAudioEnabled({ CONCH_LINUX_SHARE_AUDIO: '1' }, platform)).toBe(false);
    }
  });

  it('the application name is the registered one when the wire can carry it, else the product’s', () => {
    expect(registeredApplicationName('Conch')).toBe('Conch');
    expect(registeredApplicationName('Conch Desktop')).toBe('Conch Desktop');
    for (const bad of ['', 'x'.repeat(129), 'Cönch', 'a\nb']) expect(registeredApplicationName(bad)).toBe('Conch');
  });

  it('off: the view says nothing of sound, and no consent or command is ever sent, on any end', async () => {
    const r = rig({ sound: false });
    await r.live();
    expect(r.seen()).toBeUndefined();
    expect('shareSound' in r.voice.state()).toBe(false);
    await r.companion.end('lock');
    await r.voice.leave();
    expect(r.events).toEqual(['http:DELETE companion', 'helper:leave']);
    expect(r.sup.wire).toEqual([]);
  });

  it('off: a stop of the share by the page sends the companion’s removal and nothing of sound', async () => {
    const r = rig({ sound: false });
    await r.live();
    await r.companion.stop();
    expect(r.events).toEqual(['http:DELETE companion']);
  });
});

describe('the order: consent, then command', () => {
  it('records the consent for the person with the one scope, and only then tells the helper, with main’s own name', async () => {
    const r = rig();
    await r.live();
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on']);
    expect(r.sends).toEqual([{ op: 'voiceShareAudio', params: { channel: 'general' }, body: { scope: 'system_excluding_call', publisher: 'person' }, timeoutMs: expect.any(Number) as number }]);
    expect(r.sup.wire).toEqual([{ schema: 'conch.voice_control.v3', kind: 'set_share_audio', id: expect.any(Number) as number, capturing: true, scope: 'system_excluding_call', application_name: 'Conch' }]);
    expectNeverApplicationScope(r);
  });

  it('never sends the command before the consent has answered, however slow it is', async () => {
    const r = rig();
    await r.live();
    const slow = deferred<ReturnType<typeof consent>>();
    r.put.push(slow.promise);
    const started = r.sound?.start(DOC);
    await settle();
    expect(r.events).toEqual(['http:PUT audio']);
    slow.resolve(consent());
    expect(await started).toMatchObject({ ok: true });
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on']);
  });

  it('sends no command without a 200: a refusal, then the one more ask, then words, and the share goes on', async () => {
    const r = rig({ consentRetryMs: 5 });
    await r.live();
    r.put.push(refusal(409, 'voice_no_session'), refusal(409, 'voice_no_session'));
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: false, error: 'failed', share: expect.any(Number) as number });
    expect(r.events).toEqual(['http:PUT audio', 'http:PUT audio']);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'consent-failed' });
    expect(r.companion.current()).not.toBeNull();
    expect(r.sup.wire).toEqual([]);
  });

  it('asks once more after one pass, and a second answer of 200 goes on to the command', async () => {
    const r = rig({ consentRetryMs: 5 });
    await r.live();
    r.put.push(refusal(409, 'voice_no_session'));
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    expect(r.events).toEqual(['http:PUT audio', 'http:PUT audio', 'helper:capture on']);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it('the wait of that one pass is one poller pass (1.5 s) and the end cancels it', async () => {
    expect(CONSENT_RETRY_MS).toBe(1500);
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    r.put.push(refusal(409, 'voice_no_session'));
    const started = r.sound?.start(DOC);
    await vi.advanceTimersByTimeAsync(1499);
    expect(trace(r, 'http:PUT audio')).toHaveLength(1);
    await r.companion.stop();
    expect(await started).toMatchObject({ ok: false });
    await vi.advanceTimersByTimeAsync(5000);
    // The second ask never went; the withdrawal followed the first ask.
    expect(trace(r, 'http:PUT audio')).toHaveLength(1);
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
  });

  it('an older server answers the field with 400: no route, silent, not asked again, no withdrawal owed', async () => {
    const r = rig();
    await r.live();
    r.put.push(refusal(400, 'invalid_request'));
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: false, error: 'failed', share: expect.any(Number) as number });
    expect(r.events).toEqual(['http:PUT audio']);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'old-server' });
    // The toggle stays disabled for the next share, until the identity changes.
    await r.companion.stop();
    expect(trace(r, 'http:DELETE audio')).toEqual([]);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'old-server' });
  });

  it('forgets what it learned of the server when the identity ends', async () => {
    const r = rig();
    await r.live();
    r.put.push(refusal(400, 'invalid_request'));
    await r.sound?.start(DOC);
    await shareTeardown({ dropClient: () => undefined, companion: r.companion, ...(r.sound === null ? {} : { sound: r.sound }) })({ reason: 'switch' });
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it.each([
    ['another scope', { scope: 'application' }],
    ['the companion as publisher', { publisher: 'companion' }],
    ['no publisher (an old answer)', { publisher: undefined }],
  ])('a consent answered with %s is not the one asked for: no command, and it is withdrawn at once', async (_name, over) => {
    const r = rig();
    await r.live();
    r.put.push(consent(STARTED, over));
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: false, error: 'failed', share: expect.any(Number) as number });
    // Withdrawn now, not at the share's end (#1854 review F5).
    expect(r.events).toEqual(['http:PUT audio', 'http:DELETE audio']);
    await r.companion.stop();
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
  });

  it('does not take an answer that is not the document of a consent', async () => {
    const r = rig();
    await r.live();
    r.put.push(ok({ whatever: true }), ok({ whatever: true }));
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: false });
    expect(trace(r, 'helper:capture on')).toEqual([]);
  });

  it('“Share without sound”: with the choice none there is no consent and no command', async () => {
    const r = rig({ choice: 'none' });
    await r.live();
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'not-offered' });
    await r.companion.stop();
    expect(r.events).toEqual(['http:DELETE companion']);
    expect(r.sup.wire).toEqual([]);
  });

  it('a share that is not live, or is of another document, gets nothing', async () => {
    const r = rig();
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    await r.live();
    expect(await r.sound?.start(DOC + 1)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).toEqual([]);
  });

  it('the consent names the share it was recorded for: another share’s start time is refused', async () => {
    const r = rig();
    const token = await sounding(r);
    expect(token).toBeGreaterThan(0);
    // Off, then on again: the server answers with another share's start time.
    await r.sound?.choose('none', DOC);
    r.events.length = 0;
    r.put.push(consent('2026-11-16T13:00:00Z'));
    await r.sound?.choose('system_excluding_call', DOC);
    // The 2xx is not this share's: no command, and it is withdrawn at once,
    // not left recorded until the share ends (#1854 review F5).
    expect(r.events).toEqual(['http:PUT audio', 'http:DELETE audio']);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'consent-failed' });
    await r.companion.stop();
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
  });
});

describe('an older helper', () => {
  it.each([1, 2] as const)('v%i: no route; silent; no consent and no command; the toggle says why before the picker', async (version) => {
    const r = rig({ version });
    await r.live();
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'old-helper' });
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: false, error: 'unavailable', share: expect.any(Number) as number });
    expect(r.events).toEqual([]);
    expect(r.sup.wire).toEqual([]);
    await r.companion.stop();
    expect(r.events).toEqual(['http:DELETE companion']);
  });

  it('a v3 helper says nothing is unavailable before anything was asked', async () => {
    const r = rig();
    await r.live();
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it('the supervisor of a v2 session cannot be asked for the sound at all', async () => {
    const r = rig({ version: 2 });
    await r.live();
    expect(await r.sup.setShareAudio({ capturing: true, applicationName: 'Conch' })).toEqual({ ok: false, error: 'unsupported' });
    expect(r.sup.wire).toEqual([]);
  });
});

describe('the helper’s own words (each share_audio_problem)', () => {
  it('no_pipewire: words, the share keeps running, nothing is retried by main', async () => {
    const r = rig();
    await r.live();
    r.sup.onCapture = () => {
      r.sup.audio = { state: 'unavailable', problem: 'no_pipewire', muted: false, sources: 0 };
      r.sup.emit();
      return { ok: true };
    };
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'no-pipewire' });
    expect(trace(r, 'helper:capture on')).toHaveLength(1);
    expect(r.companion.current()).not.toBeNull();
    // The sound system comes back: the helper says on, and the words go.
    r.sup.audio = { state: 'on', muted: false, sources: 1 };
    r.sup.emit();
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it('no_process_tree: words; the log has the reason and nothing of the machine', async () => {
    const r = rig();
    await r.live();
    r.sup.onCapture = () => {
      r.sup.audio = { state: 'unavailable', problem: 'no_process_tree', muted: false, sources: 0 };
      r.sup.emit();
      return { ok: true };
    };
    await r.sound?.start(DOC);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'no-process-tree' });
    expect(r.logs.some((l) => l.includes('no-process-tree'))).toBe(true);
    expect(r.logs.join('\n')).not.toMatch(/\bpid\b|\/proc|\/|general|TOKEN|Conch/);
  });

  it('unsupported and not_connected: not_connected is silent until connected; unsupported is the generic words', async () => {
    const r = rig();
    await r.live();
    r.sup.onCapture = () => {
      r.sup.audio = { state: 'unavailable', problem: 'not_connected', muted: false, sources: 0 };
      r.sup.emit();
      return { ok: true };
    };
    await r.sound?.start(DOC);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
    r.sup.audio = { state: 'unavailable', problem: 'unsupported', muted: false, sources: 0 };
    r.sup.emit();
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'unavailable' });
  });

  it('no_sources with on: with sound, nothing playing; no words', async () => {
    const r = rig();
    await r.live();
    r.sup.onCapture = () => {
      r.sup.audio = { state: 'on', problem: 'no_sources', muted: false, sources: 0 };
      r.sup.emit();
      return { ok: true };
    };
    await r.sound?.start(DOC);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
    expect(r.voice.state().helper?.shareAudio).toEqual({ state: 'on', problem: 'no_sources', muted: false, sources: 0 });
  });

  it('publish_refused: the command is repeated once after one pass (1.5 s), then the words', async () => {
    expect(PUBLISH_RETRY_MS).toBe(1500);
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    const refused = (): void => {
      r.sup.audio = { state: 'unavailable', problem: 'publish_refused', muted: false, sources: 0 };
      r.sup.emit();
    };
    r.sup.onCapture = () => {
      refused();
      return { ok: true };
    };
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    // Not yet words: a repeat is pending.
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS - 1);
    expect(trace(r, 'helper:capture on')).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(trace(r, 'helper:capture on')).toHaveLength(2);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'publish-refused' });
    // Once: no third command however long it waits.
    await vi.advanceTimersByTimeAsync(30_000);
    expect(trace(r, 'helper:capture on')).toHaveLength(2);
    expect(r.companion.current()).not.toBeNull();
  });

  it('publish_refused that the repeat fixes: on, no words', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    r.sup.onCapture = (call) => {
      if (call === 1) {
        r.sup.audio = { state: 'unavailable', problem: 'publish_refused', muted: false, sources: 0 };
      } else {
        r.sup.audio = { state: 'on', muted: false, sources: 2 };
      }
      r.sup.emit();
      return { ok: true };
    };
    await r.sound?.start(DOC);
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS);
    expect(trace(r, 'helper:capture on')).toHaveLength(2);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it('the end cancels the repeat: no command after it, from the wait', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    r.sup.onCapture = () => {
      r.sup.audio = { state: 'unavailable', problem: 'publish_refused', muted: false, sources: 0 };
      r.sup.emit();
      return { ok: true };
    };
    await r.sound?.start(DOC);
    await r.companion.stop();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(trace(r, 'helper:capture on')).toHaveLength(1);
    expectStopThenWithdraw(r);
  });

  // #1854 review F1: the repeat rests on the consent of the first command; a
  // stop withdraws that consent, so the repeat must never write after it.
  const refusedOnce = (r: Rig): void => {
    r.sup.onCapture = (call) => {
      r.sup.audio = call === 1 ? { state: 'unavailable', problem: 'publish_refused', muted: false, sources: 0 } : { state: 'on', muted: false, sources: 2 };
      r.sup.emit();
      return { ok: true };
    };
  };

  it('turned off during the repeat’s wait: the repeat is cancelled, and no capture follows the withdrawal', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    refusedOnce(r);
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    await vi.advanceTimersByTimeAsync(500);
    expect(await r.sound?.choose('none', DOC)).toEqual({ ok: true, share: null });
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS * 4);
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on', 'helper:capture off', 'http:DELETE audio']);
    expect(r.sup.audio.state).not.toBe('on');
  });

  it('off then on again during the repeat’s wait: the capture comes only after the new consent’s 200', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    refusedOnce(r);
    await r.sound?.start(DOC);
    await vi.advanceTimersByTimeAsync(100);
    await r.sound?.choose('none', DOC);
    r.events.length = 0;
    const slow = deferred<ReturnType<typeof consent>>();
    r.put.push(slow.promise);
    const on = r.sound?.choose('system_excluding_call', DOC);
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS * 2);
    // The new consent is unanswered: nothing captures, the old repeat least of all.
    expect(r.events).toEqual(['http:PUT audio']);
    slow.resolve(consent());
    await vi.advanceTimersByTimeAsync(100);
    expect(await on).toMatchObject({ ok: true });
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on']);
  });

  it('off then on again during the repeat’s wait, the new consent refused: no capture at all', async () => {
    vi.useFakeTimers();
    const r = rig({ consentRetryMs: 10 });
    await r.live();
    refusedOnce(r);
    await r.sound?.start(DOC);
    await vi.advanceTimersByTimeAsync(100);
    await r.sound?.choose('none', DOC);
    r.events.length = 0;
    r.put.push(refusal(409, 'voice_no_session'), refusal(409, 'voice_no_session'));
    const on = r.sound?.choose('system_excluding_call', DOC);
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS * 2);
    expect(await on).toMatchObject({ ok: true });
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'consent-failed' });
  });

  // Probe F of the round-2 review: the wanted check alone. The capture's ack
  // is slow, publish_refused comes before it, and "off" is queued behind the
  // enable, so the stop (and its cancellation of the wait) has not run when
  // the repeat's wait ends.
  it('a slow capture ack with “off” queued behind it: the repeat does not write, and the end is the withdrawal', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    const ack = deferred<{ ok: true }>();
    r.sup.onCapture = (call) => {
      r.sup.audio = { state: 'unavailable', problem: 'publish_refused', muted: false, sources: 0 };
      r.sup.emit();
      return call === 1 ? ack.promise : { ok: true };
    };
    const started = r.sound?.start(DOC);
    await vi.advanceTimersByTimeAsync(100);
    const off = r.sound?.choose('none', DOC);
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS * 2);
    expect(trace(r, 'helper:capture on')).toHaveLength(1);
    ack.resolve({ ok: true });
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS * 4);
    await started;
    await off;
    expect(trace(r, 'helper:capture on')).toHaveLength(1);
    expect(r.events.at(-1)).toBe('http:DELETE audio');
  });

  // The stop's cancellation of the wait, alone: after "off" and "on" again the
  // new command stands (commanded and wanted are both true again), so only the
  // cancelled wait keeps the old repeat from writing a capture of its own.
  it('off then on again with a new consent: the old repeat never writes after the new command', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    refusedOnce(r);
    await r.sound?.start(DOC);
    await vi.advanceTimersByTimeAsync(100);
    await r.sound?.choose('none', DOC);
    await r.sound?.choose('system_excluding_call', DOC);
    expect(trace(r, 'helper:capture on')).toHaveLength(2);
    await vi.advanceTimersByTimeAsync(PUBLISH_RETRY_MS * 4);
    expect(trace(r, 'helper:capture on')).toHaveLength(2);
  });

  it('a command the helper refuses outright: stopped and withdrawn, silent with the generic words', async () => {
    const r = rig();
    await r.live();
    r.sup.onCapture = () => ({ ok: false, error: 'invalid' });
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: false, error: 'failed', share: expect.any(Number) as number });
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'unavailable' });
    expect(trace(r, 'helper:capture off', 'http:DELETE audio')).toEqual(['helper:capture off', 'http:DELETE audio']);
  });

  it('no word names a token, a room, a path, a device or an application', async () => {
    const r = rig();
    await r.live();
    r.sup.onCapture = () => {
      r.sup.audio = { state: 'unavailable', problem: 'no_pipewire', muted: false, sources: 0 };
      r.sup.emit();
      return { ok: true };
    };
    await r.sound?.start(DOC);
    const shown = JSON.stringify(r.voice.state()) + r.logs.join('\n');
    for (const secret of ['TOKEN-SECRET', ANSWER.token, 'conch.example.org', '/dev/', 'Conch"', 'application_name']) expect(shown).not.toContain(secret);
  });
});

describe('every end sends the helper’s stop, then the DELETE', () => {
  // Each end main sees, one test each (linux-share-audio.md §10, main's column).
  const ends: [string, (r: Rig, token: number) => Promise<void>][] = [
    [
      'the person’s Stop',
      async (r) => {
        await r.companion.stop();
      },
    ],
    [
      'the person’s Leave (companion first, then voice, as the IPC does)',
      async (r) => {
        const ended = r.companion.end('leave');
        await r.voice.leave();
        await ended;
      },
    ],
    [
      'lock',
      async (r) => {
        const life = new Lifecycle({ voice: r.voice, companion: r.companion, notifications: { setLocked: () => undefined }, log: () => undefined });
        await life.lockSignal('logind', true);
      },
    ],
    [
      'suspend (the voice leave is written first by the lifecycle: the sound ends before it)',
      async (r) => {
        const life = new Lifecycle({ voice: r.voice, companion: r.companion, notifications: { setLocked: () => undefined }, log: () => undefined });
        await life.suspend();
      },
    ],
    ...(['sign-out', 'switch', 'quit'] as const).map(
      (reason): [string, (r: Rig) => Promise<void>] => [
        `${reason} (the share hook, then voice)`,
        async (r) => {
          await shareTeardown({ dropClient: () => undefined, companion: r.companion, ...(r.sound === null ? {} : { sound: r.sound }) })({ reason });
          await r.voice.teardown();
        },
      ],
    ),
    [
      'a room renewal (the helper reconnects for room_deleted)',
      async (r) => {
        r.sup.phase = 'reconnecting';
        r.sup.reason = 'room_deleted';
        r.sup.emit();
        await settle();
      },
    ],
    [
      'the operator’s stop, or any end seen in presence (the page says its share ended)',
      async (r, token) => {
        expect(await r.sound?.end(token)).toBe(true);
      },
    ],
    [
      'a reload',
      async (r) => {
        await r.companion.end('reload');
      },
    ],
  ];

  it.each(ends)('%s', async (_name, end) => {
    const r = rig();
    const token = await sounding(r);
    await end(r, token);
    await settle();
    expectStopThenWithdraw(r);
    // Where the helper is told to leave too, the stop is written first.
    if (r.events.includes('helper:leave')) expect(r.events.indexOf('helper:capture off')).toBeLessThan(r.events.indexOf('helper:leave'));
    // Once: nothing is sent twice, and the helper is never told to capture again.
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
    expect(r.sound?.view()).toEqual({ choice: 'system_excluding_call' });
    expectNeverApplicationScope(r);
  });

  it('a renderer crash: at once, without waiting for the companion’s removal to finish', async () => {
    const r = rig();
    await sounding(r);
    // The companion's removal never answers.
    r.removal.push(new Promise(() => undefined));
    void r.companion.end('crash');
    await settle();
    expectStopThenWithdraw(r);
    expect(r.events[0]).toBe('helper:capture off');
  });

  it('the helper gone: the DELETE alone, and no command is written to a helper that is not there', async () => {
    const r = rig();
    await sounding(r);
    r.sup.lose();
    await settle();
    expect(trace(r, 'helper:capture off')).toEqual([]);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
    expect(r.sup.wire).toHaveLength(1);
  });

  it('the stop is written before the leave of the helper, on a leave that nothing else ends first', async () => {
    const r = rig();
    await sounding(r);
    await r.voice.leave();
    await settle();
    expect(trace(r, 'helper:capture off', 'helper:leave')).toEqual(['helper:capture off', 'helper:leave']);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
  });

  it('a helper that never answers the stop does not hold the DELETE past the capture-stop bound', async () => {
    expect(STOP_ACK_BOUND_MS).toBe(VOICE_CONTROL_CAPTURE_STOP_MS);
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    await r.sound?.start(DOC);
    r.sup.onStop = () => new Promise(() => undefined);
    const ended = r.companion.stop();
    await vi.advanceTimersByTimeAsync(STOP_ACK_BOUND_MS - 1);
    expect(trace(r, 'http:DELETE audio')).toEqual([]);
    await vi.advanceTimersByTimeAsync(1);
    await ended;
    expectStopThenWithdraw(r);
  });

  it('a busy helper is asked to stop again until it takes it', async () => {
    vi.useFakeTimers();
    const r = rig();
    await r.live();
    await r.sound?.start(DOC);
    let stops = 0;
    r.sup.onStop = () => (++stops < 3 ? { ok: false, error: 'busy' } : { ok: true });
    const ended = r.companion.stop();
    await vi.advanceTimersByTimeAsync(200);
    await ended;
    expect(stops).toBe(3);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
  });

  it('a withdrawal that fails is logged by its kind alone and not retried', async () => {
    const r = rig();
    await sounding(r);
    r.withdraw.push(refusal(500));
    await r.companion.stop();
    await settle();
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
    expect(r.logs.some((l) => l.includes('withdrawal was not confirmed'))).toBe(true);
    expect(r.logs.join('\n')).not.toMatch(/general|TOKEN/);
  });
});

describe('a stale share never stops a newer share’s sound', () => {
  it('a late end of share A (the page’s, then the companion’s) leaves share B alone', async () => {
    const r = rig();
    const tokenA = await sounding(r);
    const refA = r.companion.current();
    expect(refA).not.toBeNull();
    // A ends (the person's Stop); B is a new share by a new companion.
    await r.companion.stop();
    r.events.length = 0;
    expect(await r.companion.grant({ channel: 'general' })).toMatchObject({ ok: true });
    // B's pick was answered with a source: B is a share.
    r.sound?.captureGranted(DOC);
    r.events.length = 0;
    const startedB = await r.sound?.start(DOC);
    expect(startedB).toMatchObject({ ok: true });
    const tokenB = startedB?.ok === true ? startedB.share : -1;
    expect(tokenB).not.toBe(tokenA);
    r.events.length = 0;
    // Late cleanups of A, however they arrive.
    expect(await r.sound?.end(tokenA)).toBe(false);
    if (refA !== null) await r.sound?.ended(refA, 'stopped');
    if (refA !== null) await r.sound?.ended(refA, 'helper');
    await settle();
    expect(r.events).toEqual([]);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
    // B's own end still works, once.
    expect(await r.sound?.end(tokenB)).toBe(true);
    expectStopThenWithdraw(r);
  });

  it('a consent answer that arrives after the stop sends no command, and is withdrawn after it lands', async () => {
    const r = rig();
    await r.live();
    const late = deferred<ReturnType<typeof consent>>();
    r.put.push(late.promise);
    const started = r.sound?.start(DOC);
    await settle();
    expect(r.events).toEqual(['http:PUT audio']);
    const stopped = r.companion.stop();
    await settle();
    // The DELETE waits for the consent in flight: a late PUT would land after it.
    expect(trace(r, 'http:DELETE audio')).toEqual([]);
    late.resolve(consent());
    await stopped;
    expect(await started).toEqual({ ok: false, error: 'no-share' });
    expect(trace(r, 'helper:capture on', 'helper:capture off')).toEqual([]);
    expect(trace(r, 'http:PUT audio', 'http:DELETE audio')).toEqual(['http:PUT audio', 'http:DELETE audio']);
  });

  it('a new share’s consent waits for the older one’s withdrawal to finish', async () => {
    const r = rig();
    await r.live();
    const late = deferred<ReturnType<typeof consent>>();
    r.put.push(late.promise);
    const startedA = r.sound?.start(DOC);
    await settle();
    // A ends while its consent is unanswered (the stop waits for it).
    const stoppedA = r.companion.stop();
    expect(await r.companion.grant({ channel: 'general' })).toMatchObject({ ok: true });
    r.sound?.captureGranted(DOC);
    r.events.length = 0;
    const slow = deferred<ReturnType<typeof ok>>();
    r.withdraw.push(slow.promise);
    const startedB = r.sound?.start(DOC);
    await settle();
    // B has asked nothing while A's PUT is unanswered and A's DELETE is not done.
    expect(r.events).toEqual([]);
    late.resolve(consent());
    await settle();
    expect(r.events).toEqual(['http:DELETE audio']);
    slow.resolve(ok());
    await stoppedA;
    expect(await startedB).toMatchObject({ ok: true });
    expect(await startedA).toMatchObject({ ok: false });
    expect(r.events).toEqual(['http:DELETE audio', 'http:PUT audio', 'helper:capture on']);
  });

  it('a start while the sound is already on for a share that did not end is an end of that one and a fresh consent', async () => {
    const r = rig();
    await sounding(r);
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    expect(trace(r, 'helper:capture off', 'http:DELETE audio', 'http:PUT audio', 'helper:capture on')).toEqual([
      'helper:capture off',
      'http:DELETE audio',
      'http:PUT audio',
      'helper:capture on',
    ]);
  });
});

// #1854 review F2 and F3: a companion grant alone (Watch holds one) is no
// share; only a capture main granted is.
describe('a watch is no share', () => {
  it('start while only watching: no share, nothing asked of conchd or the helper', async () => {
    const r = rig();
    await r.watch();
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).toEqual([]);
    expect(r.sup.wire).toEqual([]);
  });

  it('the choice “with sound” while only watching keeps the choice, asks nothing, and shows no failure', async () => {
    const r = rig({ choice: 'none' });
    await r.watch();
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.prefs.choice).toBe('system_excluding_call');
    expect(r.events).toEqual([]);
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it('a capture granted in another document, or with no companion held, records no share', async () => {
    const r = rig();
    r.sound?.captureGranted(DOC);
    await r.watch();
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    r.sound?.captureGranted(DOC + 1);
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(r.events).toEqual([]);
  });

  it('after the page ends a share that had a sound record (its token), the companion may stay for watching: a choice of “on” then asks nothing', async () => {
    const r = rig({ choice: 'none' });
    await r.live();
    const chosen = await r.sound?.choose('system_excluding_call', DOC);
    expect(chosen).toMatchObject({ ok: true, share: expect.any(Number) as number });
    const token = chosen?.ok === true && chosen.share !== null ? chosen.share : -1;
    expect(await r.sound?.end(token)).toBe(true);
    expect(r.companion.current()).not.toBeNull();
    await r.sound?.choose('none', DOC);
    r.events.length = 0;
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual([]);
  });

  // #1854 review round 2: a share that never had a sound record has no token
  // to end; the page's tokenless word (shareEnded) clears the capture.
  it('a silent share (the choice none): the share ends, the companion stays, and a choice of “on” then asks nothing', async () => {
    const r = rig({ choice: 'none' });
    await r.live();
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'not-offered' });
    expect(await r.sound?.shareEnded(DOC)).toBe(true);
    expect(r.companion.current()).not.toBeNull();
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual([]);
    expect(r.sup.wire).toEqual([]);
  });

  it('a share whose start never reached main (refused at IPC): its end clears the capture all the same', async () => {
    const r = rig({ choice: 'none' });
    await r.live();
    expect(await r.sound?.shareEnded(DOC)).toBe(true);
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual([]);
  });

  it('the box unticked while the start’s consent was in flight (a start with no token): the share’s end withdraws it and clears the capture', async () => {
    const r = rig();
    await r.live();
    const slow = deferred<ReturnType<typeof consent>>();
    r.put.push(slow.promise);
    const started = r.sound?.start(DOC);
    await settle();
    const off = r.sound?.choose('none', DOC);
    slow.resolve(consent());
    expect(await started).toEqual({ ok: false, error: 'no-share' });
    await off;
    // The withdrawal of that consent is owed either way: by the disable, or by the share's end.
    expect(await r.sound?.shareEnded(DOC)).toBe(true);
    await settle();
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(trace(r, 'http:DELETE audio')).toHaveLength(1);
    r.events.length = 0;
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual([]);
  });

  it('the share’s end of another document touches nothing; of this one it ends the sound, the stop then the DELETE', async () => {
    const r = rig();
    await r.live();
    expect(await r.sound?.start(DOC)).toMatchObject({ ok: true });
    r.events.length = 0;
    expect(await r.sound?.shareEnded(DOC + 1)).toBe(false);
    expect(r.events).toEqual([]);
    expect(await r.sound?.shareEnded(DOC)).toBe(true);
    expectStopThenWithdraw(r);
    expect(await r.sound?.shareEnded(DOC)).toBe(false);
  });

  it('a failed start names its record, and the page’s end of it clears the words', async () => {
    const r = rig({ consentRetryMs: 1 });
    await r.live();
    r.put.push(refusal(409, 'voice_no_session'), refusal(409, 'voice_no_session'));
    const started = await r.sound?.start(DOC);
    expect(started).toMatchObject({ ok: false, error: 'failed', share: expect.any(Number) as number });
    expect(r.seen()).toEqual({ choice: 'system_excluding_call', unavailable: 'consent-failed' });
    const token = started?.share ?? -1;
    expect(await r.sound?.end(token)).toBe(true);
    // The companion stays (watching): main's view and the toggle agree on the
    // choice, and no failure of a share that is over is shown.
    expect(r.companion.current()).not.toBeNull();
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });
});

describe('the choice, during a share', () => {
  it('off: the stop, then the DELETE; the share goes on; on again: the consent, then the command', async () => {
    const r = rig();
    await sounding(r);
    expect(await r.sound?.choose('none', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual(['helper:capture off', 'http:DELETE audio']);
    expect(r.prefs.choice).toBe('none');
    expect(r.companion.current()).not.toBeNull();
    r.events.length = 0;
    // On again: the token of the share's sound, for the page to end it with.
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: expect.any(Number) as number });
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on']);
    expect(r.prefs.saves).toEqual(['none', 'system_excluding_call']);
  });

  it('before any share: only the choice is kept; nothing is asked of anyone', async () => {
    const r = rig();
    // Joined, with no companion and no share.
    await r.voice.join({ channel: 'general', muted: false, deafened: false });
    expect(await r.sound?.choose('none', DOC)).toEqual({ ok: true, share: null });
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual([]);
  });

  it('a choice that could not be kept changes nothing', async () => {
    const r = rig();
    await sounding(r);
    r.prefs.failSave = true;
    expect(await r.sound?.choose('none', DOC)).toEqual({ ok: false, error: 'not-saved' });
    expect(r.events).toEqual([]);
  });

  it('turned off while the consent is being asked: no command is sent, and the consent is withdrawn', async () => {
    const r = rig();
    await r.live();
    const slow = deferred<ReturnType<typeof consent>>();
    r.put.push(slow.promise);
    const started = r.sound?.start(DOC);
    await settle();
    const off = r.sound?.choose('none', DOC);
    await settle();
    slow.resolve(consent());
    await off;
    await started;
    expect(trace(r, 'helper:capture on')).toEqual([]);
    expect(trace(r, 'http:PUT audio', 'http:DELETE audio')).toEqual(['http:PUT audio', 'http:DELETE audio']);
  });
});

describe('the share-sound mute is its own', () => {
  it('sends set_share_audio_mute, shows the helper’s confirmed state, and leaves the microphone and deafen alone', async () => {
    const r = rig();
    await sounding(r);
    expect(await r.sound?.mute(true, DOC)).toEqual({ ok: true });
    expect(r.events).toEqual(['helper:share-mute true']);
    const helper = r.voice.state().helper;
    expect(helper?.shareAudio).toMatchObject({ state: 'on', muted: true });
    expect(helper).toMatchObject({ muted: false, deafened: false });
    expect(r.sup.wire.at(-1)).toMatchObject({ kind: 'set_share_audio_mute', muted: true });
    expect(await r.sound?.mute(false, DOC)).toEqual({ ok: true });
    expect(r.voice.state().helper?.shareAudio?.muted).toBe(false);
  });

  it('the microphone mute and deafen do not touch it', async () => {
    const r = rig();
    await sounding(r);
    await r.sound?.mute(true, DOC);
    r.events.length = 0;
    await r.voice.setMute(true);
    await r.voice.setDeafen(true);
    await r.voice.setMute(false);
    expect(r.events).toEqual(['helper:mic-mute true', 'helper:deafen true', 'helper:mic-mute false']);
    expect(r.voice.state().helper?.shareAudio).toMatchObject({ state: 'on', muted: true });
    expect(r.sup.wire.filter((c) => String(c['kind']).startsWith('set_share_audio'))).toHaveLength(2);
  });

  it('has nothing to mute without a sound asked for, or for another document', async () => {
    const r = rig();
    await r.live();
    expect(await r.sound?.mute(true, DOC)).toEqual({ ok: false, error: 'not-running' });
    await r.sound?.start(DOC);
    expect(await r.sound?.mute(true, DOC + 1)).toEqual({ ok: false, error: 'not-running' });
    expect(trace(r, 'helper:share-mute true')).toEqual([]);
  });
});
