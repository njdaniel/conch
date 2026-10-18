// The sharer's state machine against a stub room and a stub conchd: claim,
// busy, start, stop by each cause, and a share the server ended; and what the
// page says about sound for each browser. No SDK, no network, no device.
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ShareClaimResult } from '../api';
import type { VoiceShareEnded, VoiceShareEndReason } from '../schema/voice';
import { FakeClock, settle } from './fakes';
import { browserFeatures, busyNote, endedNote, othersShareNote, ownPageNote, resumeLimit, ScreenShare, sharedNote, shortShareNote, soundNote, type Captured, type KeptScreen, type ShareFeatures, type ShareRoom, type ShareState } from './share';

interface Deferred<T> {
  readonly promise: Promise<T>;
  readonly resolve: (value: T) => void;
  readonly reject: (err: unknown) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve: (value: T) => void = () => undefined;
  let reject: (err: unknown) => void = () => undefined;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const granted: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:00.000Z', audio: true } };
const busy: ShareClaimResult = { ok: false, failure: 'invalid', status: 409, code: 'voice_share_busy' };
const noSession: ShareClaimResult = { ok: false, failure: 'invalid', status: 409, code: 'voice_no_session' };
const unavailable: ShareClaimResult = { ok: false, failure: 'invalid', status: 503, code: 'voice_unavailable' };

function setup(claims: ShareClaimResult[] = []) {
  const calls: string[] = [];
  // The claim each release named, in order (issue #487).
  const released: string[] = [];
  const states: ShareState[] = [];
  const room = {
    pick: null as Deferred<Captured> | null,
    pickFails: false,
    publishFails: false,
    audio: true,
    // Issue #516: what keepScreen hands over, and whether its capture is live.
    kept: null as KeptScreen | null,
    live: true,
    // When set, the next claim waits for the test to answer it.
    claim: null as Deferred<ShareClaimResult> | null,
    // Issue #535: when set, the next publish waits for the test to settle it
    // (a share waiting for LiveKit's grant).
    publish: null as Deferred<undefined> | null,
  };
  const clock = new FakeClock();
  const stub: ShareRoom = {
    pickScreen: () => {
      calls.push('pick');
      if (room.pick !== null) return room.pick.promise;
      return room.pickFails ? Promise.reject(new Error('NotAllowedError')) : Promise.resolve({ audio: room.audio });
    },
    publishScreen: () => {
      calls.push('publish');
      const held = room.publish;
      room.publish = null;
      if (held !== null) return held.promise;
      return room.publishFails ? Promise.reject(new Error('refused')) : Promise.resolve();
    },
    stopScreen: () => {
      calls.push('stop screen');
      return Promise.resolve();
    },
    keepScreen: () => {
      calls.push('keep');
      const kept: KeptScreen = {
        audio: room.audio,
        live: () => room.live,
        onEnded: null,
        stop: () => {
          calls.push('stop kept');
        },
      };
      room.kept = kept;
      return kept;
    },
    adoptScreen: () => {
      calls.push('adopt');
    },
  };
  const share = new ScreenShare(
    {
      claim: (channel) => {
        calls.push(`claim ${channel}`);
        const held = room.claim;
        room.claim = null;
        return held === null ? Promise.resolve(claims.shift() ?? granted) : held.promise;
      },
      release: (channel, startedAt) => {
        calls.push(`release ${channel}`);
        released.push(startedAt);
        return Promise.resolve();
      },
    },
    (state) => {
      states.push(state);
    },
    clock,
  );
  return { share, stub, room, calls, released, states, clock };
}

async function sharing(t = setup()) {
  await t.share.start('general', t.stub);
  expect(t.share.current).toEqual({ kind: 'sharing', audio: true });
  t.calls.length = 0;
  return t;
}

describe('starting a share', () => {
  it('claims, opens the picker, claims again, then publishes', async () => {
    const t = setup();
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual(['claim general', 'pick', 'claim general', 'publish']);
    expect(t.states.map((s) => s.kind)).toEqual(['claiming', 'picking', 'publishing', 'sharing']);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true });
  });

  it('says when no sound was obtained', async () => {
    const t = setup();
    t.room.audio = false;
    await t.share.start('general', t.stub);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: false });
  });

  it('someone else sharing is told before the picker opens, and nothing is released', async () => {
    const t = setup([busy]);
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual(['claim general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'busy' });
  });

  it('a claim refused for another reason says which', async () => {
    for (const [answer, end] of [
      [noSession, 'not_connected'],
      [unavailable, 'unavailable'],
      [{ ok: false, failure: 'network' }, 'unavailable'],
      [{ ok: false, failure: 'invalid', status: 403, code: 'voice_not_allowed' }, 'failed'],
    ] as const) {
      const t = setup([answer]);
      await t.share.start('general', t.stub);
      expect(t.share.current, JSON.stringify(answer)).toEqual({ kind: 'idle', ended: end });
      expect(t.calls).toEqual(['claim general']);
    }
  });

  it('a picker closed without choosing releases the claim', async () => {
    const t = setup();
    t.room.pickFails = true;
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual(['claim general', 'pick', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'cancelled' });
  });

  it('a share taken by someone else while the picker was open stops the capture and releases nothing', async () => {
    const t = setup([granted, busy]);
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual(['claim general', 'pick', 'claim general', 'stop screen']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'busy' });
  });

  it('a second claim that fails otherwise stops the capture and releases the first', async () => {
    const t = setup([granted, unavailable]);
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual(['claim general', 'pick', 'claim general', 'stop screen', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'unavailable' });
  });

  it('a refused publish stops the capture and releases the claim', async () => {
    const t = setup();
    t.room.publishFails = true;
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual(['claim general', 'pick', 'claim general', 'publish', 'stop screen', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'failed' });
  });

  it('does not start twice', async () => {
    const t = await sharing();
    await t.share.start('general', t.stub);
    expect(t.calls).toEqual([]);
  });

  it('a stop while the picker is open releases at once, and stops what is picked when it comes', async () => {
    const t = setup();
    t.room.pick = deferred();
    const started = t.share.start('general', t.stub);
    await settle();
    expect(t.share.current.kind).toBe('picking');
    await t.share.stop('left');
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'left' });
    t.room.pick.resolve({ audio: true });
    await started;
    expect(t.calls).toEqual(['claim general', 'pick', 'stop screen', 'release general', 'stop screen']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'left' });
  });
});

describe('stopping a share', () => {
  it('by the sharer, the browser, leaving or a lost connection: stops publishing and tells conchd', async () => {
    for (const cause of ['self', 'browser', 'left', 'lost'] as const) {
      const t = await sharing();
      await t.share.stop(cause);
      expect(t.calls, cause).toEqual(['stop screen', 'release general']);
      expect(t.share.current).toEqual({ kind: 'idle', ended: cause });
    }
  });

  it("the browser's own stop control, seen by the room", async () => {
    const t = await sharing();
    t.share.ended('browser');
    await settle();
    expect(t.calls).toEqual(['stop screen', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'browser' });
  });

  it('a share the server withdrew stops publishing and does not ask conchd', async () => {
    const t = await sharing();
    t.share.ended('server');
    await settle();
    expect(t.calls).toEqual(['stop screen']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'server' });
  });

  it('presence without the sharer, once it showed them, is the server ending it', async () => {
    const t = await sharing();
    // Before presence has shown the share, its absence is only late news.
    t.share.presence('none');
    expect(t.share.current.kind).toBe('sharing');
    t.share.presence('me');
    t.share.presence('none');
    await settle();
    expect(t.calls).toEqual(['stop screen']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'server' });
  });

  it('presence is not looked at while the picker is open: the claim is made again after it', async () => {
    const t = setup();
    t.room.pick = deferred();
    const started = t.share.start('general', t.stub);
    await settle();
    t.share.presence('me');
    // The first claim lapsed while the person chose.
    t.share.presence('none');
    t.room.pick.resolve({ audio: false });
    await started;
    expect(t.share.current).toEqual({ kind: 'sharing', audio: false });
  });

  it('a reconnect claims again; a share now someone else\'s ends as the server\'s', async () => {
    const t = await sharing(setup([granted, granted, granted, busy]));
    await t.share.reconnected();
    expect(t.calls).toEqual(['claim general']);
    expect(t.share.current.kind).toBe('sharing');
    await t.share.reconnected();
    expect(t.calls).toEqual(['claim general', 'claim general', 'stop screen']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'server' });
  });

  it('a reconnect claim that fails for another reason leaves the share to the server', async () => {
    const t = await sharing(setup([granted, granted, unavailable]));
    await t.share.reconnected();
    expect(t.share.current.kind).toBe('sharing');
  });

  // Security review of #444: conchd's stop ends whoever holds the share when
  // the caller is an operator, so a stop sent once the claim lapsed would end
  // someone else's share as a moderation stop.
  describe("never stops a share that is no longer this person's", () => {
    async function lapsedWhilePicking(then: (t: ReturnType<typeof setup>) => Promise<void>) {
      const t = setup();
      t.room.pick = deferred();
      const started = t.share.start('general', t.stub);
      await settle();
      expect(t.share.current.kind).toBe('picking');
      // The picker stays open past conchd's wait: the claim lapses, and bea
      // claims the room's share.
      t.share.presence('none');
      t.share.presence('other');
      await then(t);
      await started;
      return t;
    }

    it('an operator who closes the picker late asks nothing of conchd', async () => {
      const t = await lapsedWhilePicking(async (t) => {
        t.room.pick?.reject(new Error('NotAllowedError'));
        await settle();
      });
      expect(t.calls).toEqual(['claim general', 'pick']);
      expect(t.share.current).toEqual({ kind: 'idle', ended: 'cancelled' });
    });

    it('an operator who clicks Stop sharing late asks nothing of conchd', async () => {
      const t = await lapsedWhilePicking(async (t) => {
        await t.share.stop('self');
        t.room.pick?.resolve({ audio: true });
      });
      expect(t.calls.filter((c) => c.startsWith('release'))).toEqual([]);
      expect(t.share.current).toEqual({ kind: 'idle', ended: 'self' });
    });

    it('leaving voice late asks nothing of conchd', async () => {
      const t = await lapsedWhilePicking(async (t) => {
        await t.share.stop('left');
        t.room.pick?.resolve({ audio: true });
      });
      expect(t.calls.filter((c) => c.startsWith('release'))).toEqual([]);
    });

    it('a share shown as someone else\'s while sharing is not stopped when this page stops', async () => {
      const t = await sharing();
      t.share.presence('me');
      // Presence moves to another holder (the server ended ours, and bea
      // claimed): this page stops publishing, and asks nothing of conchd.
      t.share.presence('other');
      await settle();
      await t.share.stop('self');
      expect(t.calls.filter((c) => c.startsWith('release'))).toEqual([]);
    });

    it('a share still shown as ours is asked to stop, once', async () => {
      const t = await sharing();
      t.share.presence('me');
      await t.share.stop('self');
      await t.share.stop('self');
      expect(t.calls.filter((c) => c.startsWith('release'))).toEqual(['release general']);
    });

    it('a claim granted after a stop was asked for is let go', async () => {
      const t = setup();
      let answer: (r: ShareClaimResult) => void = () => undefined;
      const share = new ScreenShare(
        {
          claim: () => new Promise<ShareClaimResult>((resolve) => {
            answer = resolve;
          }),
          release: (channel) => {
            t.calls.push(`release ${channel}`);
            return Promise.resolve();
          },
        },
        () => undefined,
        new FakeClock(),
      );
      const started = share.start('general', t.stub);
      await share.stop('self');
      // Nothing was held when the stop was asked for.
      expect(t.calls.filter((c) => c.startsWith('release'))).toEqual([]);
      answer(granted);
      await started;
      expect(t.calls.filter((c) => c.startsWith('release'))).toEqual(['release general']);
    });
  });

  // Issue #487: every release names the claim it means, so conchd refuses one
  // that comes after that claim ended, whatever this page believed.
  describe('names the claim it releases', () => {
    const later: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:00:20.000Z', audio: true } };

    it('a stop names the claim held', async () => {
      const t = await sharing();
      await t.share.stop('self');
      expect(t.released).toEqual(['2026-10-16T00:00:00.000Z']);
    });

    it('a claim that lapsed in the picker and was made anew is released by its new time', async () => {
      const t = await sharing(setup([granted, later]));
      await t.share.stop('self');
      expect(t.released).toEqual(['2026-10-16T00:00:20.000Z']);
    });

    it('a picker closed without choosing names the first claim', async () => {
      const t = setup();
      t.room.pickFails = true;
      await t.share.start('general', t.stub);
      expect(t.released).toEqual(['2026-10-16T00:00:00.000Z']);
    });
  });

  it('a stop when nothing is shared does nothing', async () => {
    const t = setup();
    await t.share.stop('self');
    t.share.ended('server');
    t.share.presence('none');
    expect(t.calls).toEqual([]);
    expect(t.share.current).toEqual({ kind: 'idle', ended: null });
  });
});

// Issue #487: the sharer's own presence says why the server ended the share,
// for the share of the last claim (by its started_at).
describe('why the server ended it', () => {
  const ended = (reason: VoiceShareEndReason, startedAt = '2026-10-16T00:00:00.000Z'): VoiceShareEnded => ({
    reason,
    started_at: startedAt,
    ended_at: '2026-10-16T00:01:00.000Z',
  });

  // 'rotated' is not an end while the capture lasts: the share is resumed
  // (issue #516, below).
  it('presence that ends the share says why, for each reason', async () => {
    for (const [reason, end] of [
      ['operator', 'operator'],
      ['access_lost', 'access_lost'],
      ['idle', 'idle'],
      ['left', 'left'],
      ['self', 'self'],
    ] as const) {
      const t = await sharing();
      t.share.presence('me');
      t.share.presence('none', ended(reason));
      await settle();
      expect(t.share.current, reason).toEqual({ kind: 'idle', ended: end });
      expect(t.calls, reason).toEqual(['stop screen']);
    }
  });

  it('a reason that comes after the room withdrew the share is said then', async () => {
    const t = await sharing();
    t.share.ended('server');
    await settle();
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'server' });
    t.share.presence('none', ended('operator'));
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'operator' });
  });

  it('a reason that came first is used when the room withdraws the share', async () => {
    const t = await sharing();
    // The frame and the withdrawal race; here the frame wins but still shows
    // the share (an older picture), so only the withdrawal ends it.
    t.share.presence('me', ended('idle'));
    expect(t.share.current.kind).toBe('sharing');
    t.share.ended('server');
    await settle();
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'idle' });
  });

  it('a reason about another share is not this one\'s', async () => {
    const t = await sharing();
    t.share.presence('me');
    t.share.presence('none', ended('operator', '2026-10-15T23:00:00.000Z'));
    await settle();
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'server' });
  });

  it('the same moment written another way is the same share', async () => {
    const t = await sharing();
    t.share.presence('me');
    t.share.presence('none', ended('operator', '2026-10-16T02:00:00.000+02:00'));
    await settle();
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'operator' });
  });

  it('the reason a last share ended does not colour the next one', async () => {
    const later: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:02:00.000Z', audio: true } };
    const t = await sharing(setup([granted, granted, later, later]));
    t.share.presence('me');
    t.share.presence('none', ended('operator'));
    await settle();
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'operator' });
    await t.share.start('general', t.stub);
    // The document still carries the old reason while the new share runs.
    t.share.presence('me', ended('operator'));
    t.share.presence('none', ended('operator'));
    await settle();
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'server' });
  });

  it('a share this page stopped itself keeps its own words', async () => {
    const t = await sharing();
    await t.share.stop('left');
    t.share.presence('none', ended('self'));
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'left' });
  });

  it('says each reason in its own words, and never names who stopped it', () => {
    expect(endedNote('operator', null)).toBe('An operator stopped your share.');
    expect(endedNote('left', null)).toBe('You left voice, so your share ended.');
    expect(endedNote('access_lost', null)).toBe('Your share ended: you no longer have voice in this channel.');
    expect(endedNote('rotated', null)).toBe('Your share ended because the voice room was renewed. Share again once you are back in voice.');
    expect(endedNote('idle', null)).toBe('Your share ended because nothing was shared for a while.');
    expect(endedNote('self', null)).toBe('You stopped sharing.');
  });
});

// Issue #516: a share whose room was renewed (rotated) is resumed on the next
// connection, with the same capture and no picker, when conchd grants the
// claim again; and is not otherwise.
describe('resuming a share after the room was renewed', () => {
  const rotated: VoiceShareEnded = { reason: 'rotated', started_at: '2026-10-16T00:00:00.000Z', ended_at: '2026-10-16T00:01:00.000Z' };
  // The share claimed again in the new room is a new share.
  const again: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:01:00.000Z', audio: true } };
  const notAllowed: ShareClaimResult = { ok: false, failure: 'invalid', status: 403, code: 'voice_not_allowed' };

  // renewed: a share under way, then presence saying its room was renewed,
  // then the old connection going.
  async function renewed(claims: ShareClaimResult[] = [granted, granted, again]) {
    const t = await sharing(setup(claims));
    t.share.presence('me');
    t.share.presence('none', rotated);
    await t.share.lost(true);
    return t;
  }

  it('keeps the capture off the old connection, and on the new one claims again and publishes it, with no picker', async () => {
    const t = await renewed();
    expect(t.share.current).toEqual({ kind: 'resuming' });
    // Kept once, when presence said: nothing stopped, nothing released (the
    // old claim ended with the room).
    expect(t.calls).toEqual(['keep']);
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general', 'adopt', 'publish']);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
    expect(t.states.map((s) => s.kind).slice(-3)).toEqual(['resuming', 'publishing', 'sharing']);
    // The resumed share is the new claim's: a stop names it.
    await t.share.stop('self');
    expect(t.released).toEqual(['2026-10-16T00:01:00.000Z']);
  });

  // Security review of #516: conchd says the room was renewed before it
  // deletes it, and a deletion that fails leaves it up to 30 s, with anyone
  // removed from the channel possibly still in it.
  it('stops publishing in the old room as soon as presence says it was renewed, before any disconnect', async () => {
    const t = await sharing(setup([granted, granted, again]));
    t.share.presence('me');
    t.share.presence('none', rotated);
    expect(t.share.current).toEqual({ kind: 'resuming' });
    expect(t.calls).toEqual(['keep']);
    // The old connection still up: nothing is claimed or published on it.
    t.share.presence('none', rotated);
    await settle();
    expect(t.calls).toEqual(['keep']);
    // Its going changes nothing more.
    await t.share.lost(true);
    expect(t.calls).toEqual(['keep']);
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general', 'adopt', 'publish']);
  });

  it('resumes across rooms renewed one after another', async () => {
    const t = await renewed([granted, granted, noSession, again]);
    // The room is renewed again between this connection and its claim.
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general']);
    expect(t.share.current).toEqual({ kind: 'resuming' });
    await t.share.lost(true);
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general', 'claim general', 'adopt', 'publish']);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
  });

  it('rooms renewed one after another still give up at the limit', async () => {
    const t = await renewed([granted, granted, noSession]);
    t.share.connected('general', t.stub);
    await settle();
    t.clock.advance(resumeLimit);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general', 'stop kept']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'rotated' });
  });

  it('a room deleted before presence said why waits for presence to say it was renewed', async () => {
    const t = await sharing(setup([granted, granted, again]));
    await t.share.lost(true);
    expect(t.share.current).toEqual({ kind: 'resuming' });
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep']);
    t.share.presence('none', rotated);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general', 'adopt', 'publish']);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
  });

  it('a share ended for any other reason is not resumed', async () => {
    for (const reason of ['operator', 'access_lost', 'idle', 'left', 'self'] as const) {
      const t = await sharing(setup([granted, granted, again]));
      await t.share.lost(true);
      t.share.connected('general', t.stub);
      t.share.presence('none', { ...rotated, reason });
      await settle();
      expect(t.calls, reason).toEqual(['keep', 'stop kept']);
      expect(t.share.current, reason).toEqual({ kind: 'idle', ended: reason });
    }
  });

  it('a connection lost without the room being deleted ends the share, as before', async () => {
    const t = await sharing();
    await t.share.lost(false);
    expect(t.calls).toEqual(['stop screen', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'lost' });
  });

  it('a capture the browser ended is not resumed', async () => {
    // Ended before the old connection went: the capture was taken off it
    // when presence said, and says so itself.
    const before = await sharing();
    before.share.presence('me');
    before.share.presence('none', rotated);
    before.room.live = false;
    before.room.kept?.onEnded?.();
    await settle();
    expect(before.calls).toEqual(['keep', 'stop kept']);
    expect(before.share.current).toEqual({ kind: 'idle', ended: 'browser' });

    // Ended while kept: the capture says so.
    const kept = await renewed();
    kept.room.live = false;
    kept.room.kept?.onEnded?.();
    await settle();
    expect(kept.calls).toEqual(['keep', 'stop kept']);
    expect(kept.share.current).toEqual({ kind: 'idle', ended: 'browser' });
    kept.share.connected('general', kept.stub);
    await settle();
    expect(kept.calls).toEqual(['keep', 'stop kept']);

    // Found ended when the new connection is up.
    const late = await renewed();
    late.room.live = false;
    late.share.connected('general', late.stub);
    await settle();
    expect(late.calls).toEqual(['keep', 'stop kept']);
    expect(late.share.current).toEqual({ kind: 'idle', ended: 'browser' });
  });

  it('a share stopped while it waited is not resumed', async () => {
    const t = await renewed();
    await t.share.stop('self');
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'stop kept']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'self' });
  });

  it('a claim conchd refuses stops the capture and says why', async () => {
    for (const [answer, end] of [
      [busy, 'busy'],
      [notAllowed, 'failed'],
      [unavailable, 'unavailable'],
    ] as const) {
      const t = await renewed([granted, granted, answer]);
      t.share.connected('general', t.stub);
      await settle();
      expect(t.calls, end).toEqual(['keep', 'claim general', 'stop kept']);
      expect(t.share.current, end).toEqual({ kind: 'idle', ended: end });
    }
  });

  // The sharer was the one removed: conchd gives them no session, so the
  // client stops before any new connection, and nothing is claimed again.
  it('a sharer whose client stops is not resumed, and is told the room was renewed', async () => {
    const t = await renewed();
    t.share.abandon();
    await settle();
    expect(t.calls).toEqual(['keep', 'stop kept']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'rotated' });
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'stop kept']);
  });

  it('stops the capture if the share is not resumed in time', async () => {
    const t = await sharing();
    await t.share.lost(true);
    t.clock.advance(resumeLimit - 1);
    await settle();
    expect(t.share.current).toEqual({ kind: 'resuming' });
    t.clock.advance(1);
    await settle();
    // Presence never said why: the old claim is let go, in case it lasts.
    expect(t.calls).toEqual(['keep', 'stop kept', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'lost' });
  });

  it('a resume cut short by another lost connection is made on the next', async () => {
    const t = await renewed([granted, granted, again]);
    // The first new connection goes while its claim is on its way.
    const claim = deferred<ShareClaimResult>();
    t.room.claim = claim;
    t.share.connected('general', t.stub);
    await settle();
    await t.share.lost(false);
    claim.resolve(again);
    await settle();
    expect(t.share.current).toEqual({ kind: 'resuming' });
    expect(t.calls).toEqual(['keep', 'claim general']);
    // conchd answers a holder who claims again with the share they hold.
    t.share.connected('general', t.stub);
    await settle();
    expect(t.calls).toEqual(['keep', 'claim general', 'claim general', 'adopt', 'publish']);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
  });
});

// Issue #535: a room renewed while the share is being published (waiting for
// LiveKit's grant), whether a first share or one being resumed, is resumed on
// the next connection like a share renewed while published; any other end is
// as before, and one wait covers every renewal until the share is back.
describe('a room renewed while the share is being published', () => {
  const first = '2026-10-16T00:00:00.000Z';
  const again: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:01:00.000Z', audio: true } };
  const third: ShareClaimResult = { ok: true, value: { principal_id: 7, started_at: '2026-10-16T00:02:00.000Z', audio: true } };
  const notAllowed: ShareClaimResult = { ok: false, failure: 'invalid', status: 403, code: 'voice_not_allowed' };
  const ended = (reason: VoiceShareEndReason, startedAt: string): VoiceShareEnded => ({ reason, started_at: startedAt, ended_at: '2026-10-16T00:03:00.000Z' });

  // resumedPublishing: a share renewed once, and claimed again in the new
  // room, where its publish waits for the grant.
  async function resumedPublishing(claims: ShareClaimResult[] = [granted, granted, again, third]) {
    const t = await sharing(setup(claims));
    t.share.presence('me');
    t.share.presence('none', ended('rotated', first));
    await t.share.lost(true);
    const publish = deferred<undefined>();
    t.room.publish = publish;
    t.share.connected('general', t.stub);
    await settle();
    expect(t.share.current).toEqual({ kind: 'publishing' });
    expect(t.calls).toEqual(['keep', 'claim general', 'adopt', 'publish']);
    // The new room's presence shows the share.
    t.share.presence('me');
    t.calls.length = 0;
    // claimed: the claim being published; next: the one claimed to resume it.
    return { ...t, publish, claimed: '2026-10-16T00:01:00.000Z', next: '2026-10-16T00:02:00.000Z' };
  }

  // firstPublishing: a first share, picked and claimed again, whose publish
  // waits for the grant.
  async function firstPublishing(claims: ShareClaimResult[] = [granted, granted, again]) {
    const t = setup(claims);
    const publish = deferred<undefined>();
    t.room.publish = publish;
    void t.share.start('general', t.stub);
    await settle();
    expect(t.share.current).toEqual({ kind: 'publishing' });
    expect(t.calls).toEqual(['claim general', 'pick', 'claim general', 'publish']);
    t.share.presence('me');
    t.calls.length = 0;
    return { ...t, publish, claimed: first, next: '2026-10-16T00:01:00.000Z' };
  }

  const shares = [
    ['a resumed share', resumedPublishing],
    ['a first share', firstPublishing],
  ] as const;

  // The orders the sharer's page can learn of the renewal in: presence
  // saying so while the old room is still up, the room deleted first, or
  // presence saying why while it still shows the share (an older picture),
  // and the publish failing before anything else.
  for (const [what, publishing] of shares) {
    for (const order of ['presence first', 'room deleted first', 'told, then the publish failed'] as const) {
      it(`${what} renewed while it waited for its grant is resumed on the next connection (${order})`, async () => {
        const t = await publishing();
        if (order === 'told, then the publish failed') {
          t.share.presence('me', ended('rotated', t.claimed));
          expect(t.share.current).toEqual({ kind: 'publishing' });
          t.publish.reject(new Error('failed to publish track, insufficient permissions'));
          await settle();
          expect(t.share.current).toEqual({ kind: 'resuming' });
          expect(t.calls).toEqual(['keep']);
        }
        if (order === 'presence first') {
          t.share.presence('none', ended('rotated', t.claimed));
          // Taken off the old connection at once, still capturing: nothing
          // stopped, nothing released (the claim ended with the room).
          expect(t.share.current).toEqual({ kind: 'resuming' });
          expect(t.calls).toEqual(['keep']);
        }
        await t.share.lost(true);
        expect(t.share.current).toEqual({ kind: 'resuming' });
        expect(t.calls).toEqual(['keep']);
        // The publish under way on the old connection fails, as the adapter
        // makes it once the capture is kept: the share still waits to resume.
        t.publish.reject(new Error('voice share taken off this connection'));
        await settle();
        expect(t.share.current).toEqual({ kind: 'resuming' });
        expect(t.calls).toEqual(['keep']);
        t.share.connected('general', t.stub);
        await settle();
        if (order === 'room deleted first') {
          // Only a renewal is resumed: nothing until presence says it was.
          expect(t.calls).toEqual(['keep']);
          t.share.presence('none', ended('rotated', t.claimed));
          await settle();
        }
        expect(t.calls).toEqual(['keep', 'claim general', 'adopt', 'publish']);
        expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
        // The share resumed is the newest claim's.
        await t.share.stop('self');
        expect(t.released).toEqual([t.next]);
      });
    }

    it(`${what} ended any other way while it waited for its grant ends as before`, async () => {
      for (const reason of ['operator', 'access_lost', 'idle', 'left', 'self'] as const) {
        const t = await publishing();
        t.share.presence('none', ended(reason, t.claimed));
        await settle();
        expect(t.calls, reason).toEqual(['stop screen']);
        expect(t.share.current, reason).toEqual({ kind: 'idle', ended: reason });
        t.publish.reject(new Error('refused'));
        await t.share.lost(true);
        t.share.connected('general', t.stub);
        await settle();
        expect(t.calls, reason).toEqual(['stop screen']);
      }

      // The browser's own stop control, and a connection lost without its
      // room being deleted.
      const browser = await publishing();
      browser.share.ended('browser');
      await settle();
      expect(browser.calls).toEqual(['stop screen', 'release general']);
      expect(browser.share.current).toEqual({ kind: 'idle', ended: 'browser' });
      const lost = await publishing();
      await lost.share.lost(false);
      expect(lost.calls).toEqual(['stop screen', 'release general']);
      expect(lost.share.current).toEqual({ kind: 'idle', ended: 'lost' });

      // A publish refused with no renewal said: the share could not start.
      const refused = await publishing();
      refused.publish.reject(new Error('failed to publish track, insufficient permissions'));
      await settle();
      expect(refused.calls.filter((c) => c !== 'stop kept')).toEqual(['stop screen', 'release general']);
      expect(refused.share.current).toEqual({ kind: 'idle', ended: 'failed' });
    });

    it(`${what} renewed while it waited, then refused in the next room or signed out, stops the capture`, async () => {
      for (const [answer, end] of [
        [busy, 'busy'],
        [notAllowed, 'failed'],
      ] as const) {
        const t = await publishing(what === 'a first share' ? [granted, granted, answer] : [granted, granted, again, answer]);
        t.share.presence('none', ended('rotated', t.claimed));
        await t.share.lost(true);
        t.share.connected('general', t.stub);
        await settle();
        expect(t.calls, end).toEqual(['keep', 'claim general', 'stop kept']);
        expect(t.share.current, end).toEqual({ kind: 'idle', ended: end });
      }

      const out = await publishing();
      out.share.presence('none', ended('rotated', out.claimed));
      await out.share.lost(true);
      out.share.abandon();
      await settle();
      out.share.connected('general', out.stub);
      await settle();
      expect(out.calls).toEqual(['keep', 'stop kept']);
      expect(out.share.current).toEqual({ kind: 'idle', ended: 'rotated' });
    });

    // Security review of #516 (F1): a capture is never published after stop
    // or leave.
    it(`${what} renewed while it waited, then stopped or left, is never published again`, async () => {
      for (const cause of ['self', 'left'] as const) {
        const t = await publishing();
        t.share.presence('none', ended('rotated', t.claimed));
        await t.share.stop(cause);
        t.publish.reject(new Error('voice share taken off this connection'));
        await t.share.lost(true);
        t.share.connected('general', t.stub);
        await settle();
        expect(t.calls, cause).toEqual(['keep', 'stop kept']);
        expect(t.share.current, cause).toEqual({ kind: 'idle', ended: cause });
      }
    });
  }

  it('one wait covers both renewals: the second carries on with what is left of the first', async () => {
    const t = await sharing(setup([granted, granted, again, third]));
    t.share.presence('me');
    t.share.presence('none', ended('rotated', first));
    await t.share.lost(true);
    t.clock.advance(30_000);
    t.room.publish = deferred();
    t.share.connected('general', t.stub);
    await settle();
    expect(t.share.current).toEqual({ kind: 'publishing' });
    t.clock.advance(10_000);
    t.share.presence('none', ended('rotated', '2026-10-16T00:01:00.000Z'));
    await t.share.lost(true);
    t.clock.advance(resumeLimit - 40_000 - 1);
    await settle();
    expect(t.share.current).toEqual({ kind: 'resuming' });
    t.clock.advance(1);
    await settle();
    expect(t.calls.slice(-2)).toEqual(['keep', 'stop kept']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'rotated' });
  });

  it('a share whose wait ran out while it was being published ends at the next renewal', async () => {
    const t = await sharing(setup([granted, granted, again, third]));
    t.share.presence('me');
    t.share.presence('none', ended('rotated', first));
    await t.share.lost(true);
    t.clock.advance(30_000);
    t.room.publish = deferred();
    t.share.connected('general', t.stub);
    await settle();
    // The wait runs out while the publish waits for the grant: it carries on.
    t.clock.advance(20_000);
    await settle();
    expect(t.share.current).toEqual({ kind: 'publishing' });
    t.share.presence('none', ended('rotated', '2026-10-16T00:01:00.000Z'));
    await settle();
    expect(t.calls.slice(-2)).toEqual(['keep', 'stop kept']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'rotated' });
  });

  it('a share back in a room has the whole wait again at its next renewal', async () => {
    const t = await sharing(setup([granted, granted, again, third]));
    t.share.presence('me');
    t.share.presence('none', ended('rotated', first));
    await t.share.lost(true);
    t.clock.advance(40_000);
    t.share.connected('general', t.stub);
    await settle();
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
    t.share.presence('me');
    t.share.presence('none', ended('rotated', '2026-10-16T00:01:00.000Z'));
    await t.share.lost(true);
    t.clock.advance(resumeLimit - 1);
    await settle();
    expect(t.share.current).toEqual({ kind: 'resuming' });
    t.share.connected('general', t.stub);
    await settle();
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true, resumed: true });
  });
});

describe('what the page says', () => {
  const browsers: readonly [string, ShareFeatures, RegExp][] = [
    ['Chrome on Windows', { canShare: true, chromium: true, platform: 'windows' }, /tab shares its own sound.*whole system's sound/],
    ['Edge on macOS', { canShare: true, chromium: true, platform: 'mac' }, /only a tab shares its sound.*without sound/],
    ['Chrome on Linux', { canShare: true, chromium: true, platform: 'linux' }, /only a tab shares its sound/],
    ['Chrome on Android', { canShare: true, chromium: true, platform: 'android' }, /No sound: a phone/],
    ['Firefox on Windows', { canShare: true, chromium: false, platform: 'windows' }, /No sound: this browser shares the picture only/],
    ['Safari on macOS', { canShare: true, chromium: false, platform: 'mac' }, /No sound: this browser shares the picture only/],
    ['Safari on iPhone', { canShare: false, chromium: false, platform: 'ios' }, /cannot share a screen.*You can still watch/],
  ];

  it('says what sound a share will carry, per browser, before the picker opens', () => {
    for (const [name, features, text] of browsers) expect(soundNote(features), name).toMatch(text);
  });

  // Issue #600: the strip's one line names the same facts as soundNote and
  // ownPageNote, for each of soundNote's cases: whether sound comes, and that
  // a whole screen shows this page. A browser that cannot share says so in
  // full, as it is one sentence and there is nothing about a picker to add.
  it('says the same in one short line: whether sound comes, and that a whole screen shows this page', () => {
    const sound: Record<string, RegExp> = {
      'Chrome on Windows': /^Sharing: a tab or the entire screen carries sound; /,
      'Edge on macOS': /^Sharing: only a tab carries sound; /,
      'Chrome on Linux': /^Sharing: only a tab carries sound; /,
      'Chrome on Android': /^Sharing: the picture only, no sound; /,
      'Firefox on Windows': /^Sharing: the picture only, no sound; /,
      'Safari on macOS': /^Sharing: the picture only, no sound; /,
      'Safari on iPhone': /^This browser cannot share a screen/,
    };
    for (const [name, features] of browsers) {
      const line = shortShareNote(features);
      expect(line, name).toMatch(sound[name] ?? /no such case/);
      if (features.canShare) {
        expect(line, name).toMatch(/a whole screen also shows this page\.$/);
        expect(line.length, `${name}: ${line}`).toBeLessThanOrEqual(90);
      } else {
        expect(line, name).toBe(soundNote(features));
      }
    }
  });

  it("says in one line who else shares and with what sound, which is why Share screen is off", () => {
    expect(othersShareNote('bob', false)).toBe('bob is sharing a screen.');
    expect(othersShareNote('bob', true)).toBe('bob is sharing a screen, with sound.');
    expect(busyNote('bob', true)).toBe('bob is sharing a screen, with sound. One person shares at a time.');
    expect(busyNote(null, false)).toBe('Someone else is sharing a screen. One person shares at a time.');
  });

  it('warns before the picker opens that a whole screen or this window shows this page, chat included', () => {
    expect(ownPageNote).toBe('Sharing your entire screen or this browser window shows this page too, chat included. Share a tab or another window to keep it out.');
  });

  it('says afterwards whether sound was obtained', () => {
    const chrome: ShareFeatures = { canShare: true, chromium: true, platform: 'linux' };
    const firefox: ShareFeatures = { canShare: true, chromium: false, platform: 'linux' };
    expect(sharedNote(true, chrome)).toBe('You are sharing your screen with its sound.');
    expect(sharedNote(false, chrome)).toMatch(/without sound\. To share sound, stop and pick a tab/);
    expect(sharedNote(false, firefox)).toBe('You are sharing your screen without sound.');
  });

  it('names who is sharing when a claim is busy, and says why every other share ended', () => {
    expect(endedNote('busy', 'alice')).toBe('alice is sharing a screen. One person shares at a time.');
    expect(endedNote('busy', null)).toBe('Someone else is sharing a screen. One person shares at a time.');
    expect(endedNote('server', null)).toMatch(/server ended your share.*operator/);
    for (const end of ['self', 'browser', 'left', 'lost', 'cancelled', 'not_connected', 'unavailable', 'failed'] as const) {
      expect(endedNote(end, null).length, end).toBeGreaterThan(10);
    }
  });
});

describe('reading the browser', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  function as(agent: string, hints: unknown, picker: boolean) {
    vi.stubGlobal('navigator', {
      userAgent: agent,
      userAgentData: hints,
      mediaDevices: picker ? { getDisplayMedia: () => Promise.reject(new Error('not in a test')) } : {},
    });
    return browserFeatures();
  }

  it('finds Chromium and its platform from client hints, and the rest from the user agent', () => {
    expect(as('Mozilla/5.0 (Windows NT 10.0)', { brands: [{ brand: 'Chromium' }, { brand: 'Microsoft Edge' }], platform: 'Windows' }, true)).toEqual({
      canShare: true,
      chromium: true,
      platform: 'windows',
    });
    expect(as('Mozilla/5.0 (X11; Linux x86_64) Gecko/20100101 Firefox/140.0', undefined, true)).toEqual({ canShare: true, chromium: false, platform: 'linux' });
    expect(as('Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)', undefined, false)).toEqual({ canShare: false, chromium: false, platform: 'ios' });
    expect(as('Mozilla/5.0 (Linux; Android 15)', { brands: [{ brand: 'Chromium' }], platform: 'Android' }, true).platform).toBe('android');
  });
});
