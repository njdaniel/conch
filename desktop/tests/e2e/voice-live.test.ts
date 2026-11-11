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
import { existsSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { afterEach, describe, expect, it } from 'vitest';

import { VOICE_CONTROL_GATE_SHUT_MS, VOICE_CONTROL_OWNER_LOSS_EXIT_MS, VOICE_CONTROL_STOP_KILL_AFTER_MS } from '../../../web/src/schema/generated.ts';
import { until } from '../unit/voice-support.ts';
import { HEAR_MS, RELEASE_TAIL_MS, SETTLE_MS, absentSoon, alive, closeOwners, heard, note, onlyOnce, owner, sleep, waitHeard, world, inRoom, expectSilentFrom, ask } from './support.ts';

afterEach(closeOwners);

// ------------------------------------------------------------------- tests

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

