// The camera's state machine (ADR-015, design §6.2, §7.2, §11.5) against a
// stub room and a stub conchd: every transition; a stop during each await;
// each error of §7.2 in its words; no other camera when the device goes; the
// one rule (S2), that only the button sends an un-named claim and every
// resume names the claim it continues; stop and release on every end that is
// not resumed (S5); and the adoption of a claim the room's renewal ended. No
// network, no camera.
import { describe, expect, it } from 'vitest';
import type { CameraClaimResult } from '../api';
import type { VoiceCameraEnded } from '../schema/voice';
import {
  CameraClient,
  captureFailure,
  cameraNote,
  failureNote,
  findCamera,
  keepLimit,
  passGap,
  refusalFailure,
  renewalGrace,
  unpublishWait,
  type CameraFailure,
  type CameraRoom,
  type CameraState,
  type KeptCamera,
} from './camera';
import { cameraAfter } from './disconnect';
import { FakeClock, settle } from './fakes';

interface Gate {
  promise: Promise<void>;
  open: () => void;
  fail: (err: unknown) => void;
}

function gate(): Gate {
  let open: () => void = () => undefined;
  let fail: (err: unknown) => void = () => undefined;
  const promise = new Promise<void>((res, rej) => {
    open = res;
    fail = rej;
  });
  return { promise, open, fail };
}

// A room that records each step in the log it shares with conchd's stub, so
// that their order is seen. A step can be held for the test to finish.
class StubCamRoom implements CameraRoom {
  captured = false;
  published = false;
  kept: { live: boolean; stopped: boolean } | null = null;
  captureError: Error | null = null;
  grantError: Error | null = null;
  hold: 'capture' | 'grant' | 'publish' | 'stop' | null = null;
  held: Gate | null = null;
  constructor(readonly log: string[], readonly name = 'room') {}
  private step(what: 'capture' | 'grant' | 'publish' | 'stop'): Promise<void> {
    if (this.hold !== what) return Promise.resolve();
    this.hold = null;
    this.held = gate();
    return this.held.promise;
  }
  captureCamera = async (choice: string | null): Promise<void> => {
    this.log.push(`${this.name} capture ${choice ?? 'browser'}`);
    await this.step('capture');
    if (this.captureError !== null) throw this.captureError;
    this.captured = true;
  };
  awaitCameraGrant = async (): Promise<void> => {
    this.log.push(`${this.name} grant`);
    await this.step('grant');
    if (this.grantError !== null) throw this.grantError;
  };
  publishCamera = async (): Promise<void> => {
    this.log.push(`${this.name} publish`);
    await this.step('publish');
    // As sdk.ts: a capture stopped while its publish was on its way is
    // taken down when it completes, and the publish fails.
    if (!this.captured) throw new Error('taken');
    this.published = true;
  };
  stopCamera = (): Promise<void> => {
    if (this.captured) this.log.push(`${this.name} stop`);
    this.captured = false;
    this.published = false;
    return this.step('stop');
  };
  keepCamera = (): KeptCamera | null => {
    if (!this.captured) return null;
    this.log.push(`${this.name} keep`);
    this.captured = false;
    this.published = false;
    const kept = { live: true, stopped: false };
    this.kept = kept;
    const log = this.log;
    return {
      live: () => kept.live && !kept.stopped,
      onEnded: null,
      stop: () => {
        kept.stopped = true;
        log.push('kept stop');
      },
    };
  };
  adoptCamera = (): void => {
    this.log.push(`${this.name} adopt`);
    this.captured = true;
  };
}

const started = '2026-11-03T10:00:00.000Z';
const adopted = '2026-11-03T10:00:30.000Z';
const granted = (at = started): CameraClaimResult => ({ ok: true, value: { principal_id: 7, started_at: at } });
const refused = (status: number, code: string): CameraClaimResult => ({ ok: false, failure: 'invalid', status, code });

function setup(answers: CameraClaimResult[] = []) {
  const clock = new FakeClock();
  const log: string[] = [];
  const states: CameraState[] = [];
  // Each claim conchd was asked for, and whether it named a claim.
  const claims: (string | undefined)[] = [];
  const client = new CameraClient(
    {
      claim: (channel, startedAt) => {
        claims.push(startedAt);
        log.push(startedAt === undefined ? `POST ${channel}` : `POST ${channel} ?started_at=${startedAt}`);
        return Promise.resolve(answers.shift() ?? granted());
      },
      release: (channel, startedAt) => {
        log.push(`DELETE ${channel} ?started_at=${startedAt}`);
        return Promise.resolve();
      },
    },
    (s) => states.push(s),
    clock,
  );
  const room = new StubCamRoom(log);
  return { clock, log, states, claims, client, room };
}

async function sending(t = setup()) {
  await t.client.start('general', t.room, null);
  await settle();
  expect(t.client.current).toEqual({ kind: 'sending' });
  return t;
}

const ended = (reason: VoiceCameraEnded['reason'], at = started): VoiceCameraEnded => ({ reason, started_at: at, ended_at: '2026-11-03T10:00:20.000Z' });

describe('the camera, turned on and off', () => {
  it('goes through capture, claim, the grant and the publish, and is sending only then', async () => {
    const t = await sending();
    expect(t.states.map((s) => s.kind)).toEqual(['capturing', 'claiming', 'granted-waiting', 'publishing', 'sending']);
    expect(t.log).toEqual(['room capture browser', 'POST general', 'room grant', 'room publish']);
  });

  it('captures the camera chosen, exactly', async () => {
    const t = setup();
    await t.client.start('general', t.room, 'cam-2');
    expect(t.log[0]).toBe('room capture cam-2');
  });

  it('turned off: the capture stops first, then conchd is told, naming the claim', async () => {
    const t = await sending();
    t.log.length = 0;
    await t.client.stop('self');
    await settle();
    expect(t.log).toEqual(['room stop', `DELETE general ?started_at=${started}`]);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'self' });
    expect(t.states.slice(-2).map((s) => s.kind)).toEqual(['stopping', 'off']);
  });

  it('a stop whose unpublish hangs still tells conchd, after unpublishWait, with the capture already stopped', async () => {
    const t = await sending();
    t.log.length = 0;
    t.room.hold = 'stop';
    const stopping = t.client.stop('self');
    await settle();
    expect(t.log).toEqual(['room stop']);
    t.clock.advance(unpublishWait);
    await stopping;
    expect(t.log).toEqual(['room stop', `DELETE general ?started_at=${started}`]);
  });

  it('does nothing when started twice, or stopped while off', async () => {
    const t = await sending();
    await t.client.start('general', t.room, null);
    expect(t.log.filter((l) => l.startsWith('POST'))).toHaveLength(1);
    const u = setup();
    await u.client.stop('self');
    expect(u.log).toEqual([]);
  });
});

describe('a stop during each step', () => {
  it.each([['capture'], ['grant'], ['publish']] as const)('during the %s: nothing is left captured, and a claim granted is released by name', async (step) => {
    const t = setup();
    t.room.hold = step;
    const start = t.client.start('general', t.room, null);
    await settle();
    const stopped = t.client.stop('left');
    t.room.held?.open();
    await stopped;
    await start;
    await settle();
    expect(t.room.captured).toBe(false);
    expect(t.room.published).toBe(false);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'left' });
    const releases = t.log.filter((l) => l.startsWith('DELETE'));
    expect(releases).toEqual(step === 'capture' ? [] : [`DELETE general ?started_at=${started}`]);
    // Nothing is published after the stop.
    const after = t.log.slice(t.log.lastIndexOf('room stop') + 1);
    expect(after).not.toContain('room publish');
  });

  it('during the claim: the claim granted after the stop is released, naming it', async () => {
    const t = setup();
    let answer: (r: CameraClaimResult) => void = () => undefined;
    const client = new CameraClient(
      {
        claim: (channel, at) => {
          t.log.push(at === undefined ? `POST ${channel}` : `POST ${channel} ?started_at=${at}`);
          return new Promise((resolve) => {
            answer = resolve;
          });
        },
        release: (channel, at) => {
          t.log.push(`DELETE ${channel} ?started_at=${at}`);
          return Promise.resolve();
        },
      },
      () => undefined,
      t.clock,
    );
    const start = client.start('general', t.room, null);
    await settle();
    await client.stop('self');
    answer(granted());
    await start;
    await settle();
    expect(t.log).toEqual(['room capture browser', 'POST general', 'room stop', `DELETE general ?started_at=${started}`]);
    expect(t.room.captured).toBe(false);
  });
});

describe('what the person is told (design §7.2)', () => {
  const words: [CameraFailure, string][] = [
    ['blocked', "The browser blocked the camera. Allow it in this site's settings."],
    ['not_found', 'No camera found.'],
    ['in_use', 'The camera is in use by another app.'],
    ['not_connected', 'Your chosen camera is not connected.'],
    ['not_allowed', 'Camera is off for you on this server.'],
    ['full', 'This call already has 9 cameras on.'],
    ['stale', 'Your camera was turned off.'],
    ['failed', 'Could not start the camera.'],
    ['disconnected', 'Camera disconnected. Choose a camera in Voice options.'],
  ];
  it.each(words)('%s says %j', (reason, text) => {
    expect(failureNote(reason)).toBe(text);
    expect(cameraNote({ kind: 'failed', reason })).toBe(text);
  });

  it.each([
    ['NotAllowedError', 'blocked'],
    ['SecurityError', 'blocked'],
    ['NotFoundError', 'not_found'],
    ['NotReadableError', 'in_use'],
    ['OverconstrainedError', 'not_connected'],
    ['AbortError', 'failed'],
  ] as const)('a capture refused with %s is %s, and nothing is claimed', async (name, reason) => {
    expect(captureFailure(new DOMException('no', name))).toBe(reason);
    const t = setup();
    t.room.captureError = new DOMException('no', name);
    await t.client.start('general', t.room, 'cam-1');
    expect(t.client.current).toEqual({ kind: 'failed', reason });
    expect(t.claims).toEqual([]);
  });

  it.each([
    [403, 'permission_denied', 'not_allowed'],
    [409, 'voice_camera_full', 'full'],
    [429, 'voice_camera_rate_limited', 'rate_limited'],
    [500, 'internal', 'failed'],
  ] as const)('a claim refused %i %s is %s: the capture stops, and nothing is published', async (status, code, reason) => {
    expect(refusalFailure({ ok: false, failure: 'invalid', status, code })).toBe(reason);
    const t = setup([refused(status, code)]);
    await t.client.start('general', t.room, null);
    expect(t.client.current).toEqual({ kind: 'failed', reason });
    expect(t.room.captured).toBe(false);
    expect(t.log).not.toContain('room publish');
  });

  it('voice_no_session is asked once more after one pass, then "Could not start the camera"', async () => {
    const t = setup([refused(409, 'voice_no_session'), refused(409, 'voice_no_session')]);
    const start = t.client.start('general', t.room, null);
    await settle();
    expect(t.claims).toEqual([undefined]);
    t.clock.advance(passGap);
    await start;
    expect(t.claims).toEqual([undefined, undefined]);
    expect(t.client.current).toEqual({ kind: 'failed', reason: 'failed' });
    expect(t.room.captured).toBe(false);
  });

  it('voice_no_session then a grant sends video', async () => {
    const t = setup([refused(409, 'voice_no_session')]);
    const start = t.client.start('general', t.room, null);
    await settle();
    t.clock.advance(passGap);
    await start;
    await settle();
    expect(t.client.current).toEqual({ kind: 'sending' });
  });

  it('a grant that never comes stops the capture and releases the claim', async () => {
    const t = setup();
    t.room.grantError = new Error('not granted in time');
    await t.client.start('general', t.room, null);
    await settle();
    expect(t.client.current).toEqual({ kind: 'failed', reason: 'failed' });
    expect(t.log.slice(-2)).toEqual(['room stop', `DELETE general ?started_at=${started}`]);
  });
});

describe('the device lost', () => {
  it('stops, releases and says so, and never captures another camera', async () => {
    const t = await sending();
    t.log.length = 0;
    t.client.ended('device');
    await settle();
    expect(t.log).toEqual(['room stop', `DELETE general ?started_at=${started}`]);
    expect(t.client.current).toEqual({ kind: 'failed', reason: 'disconnected' });
    t.clock.advance(keepLimit);
    await settle();
    expect(t.log.filter((l) => l.includes('capture'))).toEqual([]);
  });
});

describe('the one rule (S2): only the button sends an un-named claim', () => {
  it('the SDK reconnecting names the claim; a stale answer stops and releases, and nothing is claimed again', async () => {
    const t = await sending(setup([granted(), refused(409, 'voice_camera_stale')]));
    t.log.length = 0;
    await t.client.reconnected();
    await settle();
    expect(t.claims).toEqual([undefined, started]);
    expect(t.log).toEqual([`POST general ?started_at=${started}`, 'room stop']);
    expect(t.client.current).toEqual({ kind: 'failed', reason: 'stale' });
    expect(cameraNote(t.client.current)).toBe('Your camera was turned off.');
  });

  it('the SDK reconnecting, answered with the claim, carries on', async () => {
    const t = await sending();
    await t.client.reconnected();
    expect(t.claims).toEqual([undefined, started]);
    expect(t.client.current).toEqual({ kind: 'sending' });
  });

  it('a full reconnect while the capture is held names the claim, and carries on with the same capture', async () => {
    const t = await sending();
    t.client.lost('keep');
    expect(t.client.current).toEqual({ kind: 'resuming' });
    expect(t.room.kept?.stopped).toBe(false);
    const next = new StubCamRoom(t.log, 'next');
    t.client.connected('general', next);
    await settle();
    expect(t.claims).toEqual([undefined, started]);
    expect(t.log.slice(-3)).toEqual(['next adopt', 'next grant', 'next publish']);
    expect(t.client.current).toEqual({ kind: 'sending' });
  });

  it('a full reconnect answered stale stops the capture and says so: never an un-named claim', async () => {
    const t = await sending(setup([granted(), refused(409, 'voice_camera_stale')]));
    t.client.lost('keep');
    t.client.connected('general', new StubCamRoom(t.log, 'next'));
    await settle();
    expect(t.claims).toEqual([undefined, started]);
    expect(t.room.kept?.stopped).toBe(true);
    expect(t.client.current).toEqual({ kind: 'failed', reason: 'stale' });
    expect(t.log).not.toContain('next adopt');
  });

  it('a full reconnect after the capture was stopped never turns it back on', async () => {
    const t = await sending();
    await t.client.stop('self');
    t.client.connected('general', new StubCamRoom(t.log, 'next'));
    await t.client.reconnected();
    await settle();
    expect(t.claims).toEqual([undefined]);
    expect(t.log.filter((l) => l.startsWith('next'))).toEqual([]);
  });

  it('no path but start sends an un-named claim: across a renewal, a reconnect and an SDK resume', async () => {
    const t = await sending(setup([granted(), granted(adopted), granted(adopted), granted(adopted)]));
    t.client.presence(ended('rotated'));
    t.client.connected('general', new StubCamRoom(t.log, 'r2'));
    await settle();
    await t.client.reconnected();
    t.client.lost('keep');
    t.client.connected('general', new StubCamRoom(t.log, 'r3'));
    await settle();
    expect(t.claims[0]).toBeUndefined();
    expect(t.claims.slice(1).every((c) => c !== undefined)).toBe(true);
    expect(t.claims).toEqual([undefined, started, adopted, adopted]);
  });
});

describe('the room renewed (rotation)', () => {
  it('camera_ended rotated keeps the capture, blanked, and the next connection adopts it with a POST naming the ended claim', async () => {
    const t = await sending(setup([granted(), granted(adopted)]));
    t.log.length = 0;
    t.client.presence(ended('rotated'));
    expect(t.client.current).toEqual({ kind: 'resuming' });
    expect(t.log).toEqual(['room keep']);
    expect(cameraNote(t.client.current)).toMatch(/held, and sends nothing/);
    const next = new StubCamRoom(t.log, 'next');
    t.client.connected('general', next);
    await settle();
    expect(t.log).toEqual(['room keep', `POST general ?started_at=${started}`, 'next adopt', 'next grant', 'next publish']);
    expect(t.client.current).toEqual({ kind: 'sending' });
    // The adoption is a new claim: the next release names it.
    await t.client.stop('self');
    expect(t.log.at(-1)).toBe(`DELETE general ?started_at=${adopted}`);
  });

  it.each([
    ['stale', refused(409, 'voice_camera_stale'), 'stale'],
    ['no session', refused(409, 'voice_no_session'), 'failed'],
    ['refused', refused(403, 'permission_denied'), 'failed'],
    ['unreachable', { ok: false, failure: 'network' }, 'failed'],
  ] as const)('an adoption answered %s stops the capture, and claims nothing more', async (_what, answer, reason) => {
    const t = await sending(setup([granted(), answer]));
    t.client.presence(ended('rotated'));
    t.client.connected('general', new StubCamRoom(t.log, 'next'));
    await settle();
    expect(t.room.kept?.stopped).toBe(true);
    expect(t.client.current).toEqual({ kind: 'failed', reason });
    expect(t.claims).toEqual([undefined, started]);
    expect(t.log).not.toContain('next adopt');
  });

  it('a deleted room before presence says why: held for renewalGrace, adopted once presence says rotated', async () => {
    const t = await sending(setup([granted(), granted(adopted)]));
    t.client.lost(cameraAfter('room_deleted'));
    expect(t.client.current).toEqual({ kind: 'resuming' });
    t.client.connected('general', new StubCamRoom(t.log, 'next'));
    await settle();
    // Not claimed until presence says the room was renewed.
    expect(t.claims).toEqual([undefined]);
    t.client.presence(ended('rotated'));
    await settle();
    expect(t.claims).toEqual([undefined, started]);
    expect(t.client.current).toEqual({ kind: 'sending' });
  });

  it('a deleted room that presence never says was renewed is stopped and released after renewalGrace (S5)', async () => {
    const t = await sending();
    t.client.lost(cameraAfter('room_deleted'));
    t.clock.advance(renewalGrace);
    await settle();
    expect(t.room.kept?.stopped).toBe(true);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'lost' });
    expect(t.log.at(-1)).toBe(`DELETE general ?started_at=${started}`);
    expect(t.claims).toEqual([undefined]);
  });

  it('a capture held longer than keepLimit is stopped', async () => {
    const t = await sending();
    t.client.lost('keep');
    t.clock.advance(keepLimit);
    await settle();
    expect(t.room.kept?.stopped).toBe(true);
    expect(t.client.current.kind).toBe('off');
  });
});

describe('every other end stops and releases (S5)', () => {
  it('participant_removed stops the capture and releases the claim at once', async () => {
    const t = await sending();
    t.log.length = 0;
    t.client.lost(cameraAfter('participant_removed'));
    await settle();
    expect(t.log).toEqual(['room stop', `DELETE general ?started_at=${started}`]);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'lost' });
  });

  it('duplicate_identity stops the capture at once, and leaves the claim, now the other device’s, to end idle', async () => {
    const t = await sending();
    t.log.length = 0;
    t.client.lost(cameraAfter('duplicate_identity'));
    await settle();
    expect(t.log).toEqual(['room stop']);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'lost' });
    // And never claims it again.
    t.client.connected('general', new StubCamRoom(t.log, 'next'));
    await t.client.reconnected();
    expect(t.claims).toEqual([undefined]);
  });

  it('duplicate_identity while held between connections stops the kept capture, without a release', async () => {
    const t = await sending();
    t.client.lost('keep');
    t.client.lost(cameraAfter('duplicate_identity'));
    await settle();
    expect(t.room.kept?.stopped).toBe(true);
    expect(t.log.filter((l) => l.startsWith('DELETE'))).toEqual([]);
  });

  it('a refused next session stops a capture held for it', async () => {
    const t = await sending();
    t.client.lost('keep');
    t.client.refused();
    await settle();
    expect(t.room.kept?.stopped).toBe(true);
    expect(t.log.at(-1)).toBe(`DELETE general ?started_at=${started}`);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'lost' });
  });

  it('a camera still starting when the connection ends is stopped, not carried on', async () => {
    const t = setup();
    t.room.hold = 'grant';
    void t.client.start('general', t.room, null);
    await settle();
    t.client.lost('keep');
    await settle();
    expect(t.room.captured).toBe(false);
    expect(t.client.current).toEqual({ kind: 'off', ended: 'lost' });
  });

  it.each([['self'], ['operator'], ['left'], ['access_lost'], ['idle']] as const)('camera_ended %s stops the capture, says why, and claims nothing again', async (reason) => {
    const t = await sending();
    t.client.presence(ended(reason));
    await settle();
    expect(t.room.captured).toBe(false);
    expect(t.client.current).toEqual({ kind: 'off', ended: reason });
    expect(t.claims).toEqual([undefined]);
    // The claim is over: it is not released again.
    expect(t.log.filter((l) => l.startsWith('DELETE'))).toEqual([]);
    t.client.connected('general', new StubCamRoom(t.log, 'next'));
    await settle();
    expect(t.claims).toEqual([undefined]);
  });

  it('camera_ended about an earlier claim changes nothing', async () => {
    const t = await sending();
    t.client.presence(ended('operator', '2026-11-03T09:00:00.000Z'));
    expect(t.client.current).toEqual({ kind: 'sending' });
  });

  it('LiveKit taking the source away stops the capture, releases by name, and says why once presence does', async () => {
    const t = await sending();
    t.client.ended('server');
    await settle();
    expect(t.client.current).toEqual({ kind: 'off', ended: 'server' });
    expect(t.log.slice(-2)).toEqual(['room stop', `DELETE general ?started_at=${started}`]);
    t.client.presence(ended('operator'));
    expect(t.client.current).toEqual({ kind: 'off', ended: 'operator' });
    expect(cameraNote(t.client.current)).toBe('An operator turned off your camera.');
  });

  it('a camera held between connections stops when the client stops (abandon)', async () => {
    const t = await sending();
    t.client.lost('keep');
    t.client.abandon();
    await settle();
    expect(t.room.kept?.stopped).toBe(true);
  });
});

describe('the camera chosen (N7)', () => {
  const desk = { id: 'cam-1', label: 'Desk camera' };
  it('is taken by its id while listed', () => {
    expect(findCamera([desk, { id: 'cam-2', label: 'Laptop camera' }], desk)).toBe(desk);
  });
  it('is found under a new id when exactly one camera carries its name', () => {
    expect(findCamera([{ id: 'new-id', label: 'Desk camera' }, { id: 'cam-2', label: 'Laptop camera' }], desk)).toEqual({ id: 'new-id', label: 'Desk camera' });
  });
  it('is not connected when two cameras carry its name, or none does: never the default, never by place', () => {
    expect(findCamera([{ id: 'a', label: 'Desk camera' }, { id: 'b', label: 'Desk camera' }], desk)).toBeNull();
    expect(findCamera([{ id: 'default', label: 'Default' }, { id: 'cam-2', label: 'Laptop camera' }], desk)).toBeNull();
    expect(findCamera([], desk)).toBeNull();
  });
  it('is asked for as it is while the browser names no camera', () => {
    expect(findCamera(null, desk)).toBe(desk);
  });
});

describe('what a connection end means for the camera (disconnect.ts)', () => {
  it.each([
    ['duplicate_identity', 'displaced'],
    ['participant_removed', 'stop'],
    ['room_deleted', 'renewal'],
    ['server_shutdown', 'keep'],
    ['lost', 'keep'],
  ] as const)('%s: %s', (cause, loss) => {
    expect(cameraAfter(cause)).toBe(loss);
  });
});
