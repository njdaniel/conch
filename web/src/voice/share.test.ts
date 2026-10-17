// The sharer's state machine against a stub room and a stub conchd: claim,
// busy, start, stop by each cause, and a share the server ended; and what the
// page says about sound for each browser. No SDK, no network, no device.
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ShareClaimResult } from '../api';
import type { VoiceShareEnded, VoiceShareEndReason } from '../schema/voice';
import { settle } from './fakes';
import { browserFeatures, endedNote, ownPageNote, ScreenShare, sharedNote, soundNote, type Captured, type ShareFeatures, type ShareRoom, type ShareState } from './share';

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
  };
  const stub: ShareRoom = {
    pickScreen: (content) => {
      calls.push(`pick ${content}`);
      if (room.pick !== null) return room.pick.promise;
      return room.pickFails ? Promise.reject(new Error('NotAllowedError')) : Promise.resolve({ audio: room.audio });
    },
    publishScreen: () => {
      calls.push('publish');
      return room.publishFails ? Promise.reject(new Error('refused')) : Promise.resolve();
    },
    stopScreen: () => {
      calls.push('stop screen');
      return Promise.resolve();
    },
  };
  const share = new ScreenShare(
    {
      claim: (channel) => {
        calls.push(`claim ${channel}`);
        return Promise.resolve(claims.shift() ?? granted);
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
  );
  return { share, stub, room, calls, released, states };
}

async function sharing(t = setup()) {
  await t.share.start('general', t.stub, 'detail');
  expect(t.share.current).toEqual({ kind: 'sharing', audio: true });
  t.calls.length = 0;
  return t;
}

describe('starting a share', () => {
  it('claims, opens the picker, claims again, then publishes', async () => {
    const t = setup();
    await t.share.start('general', t.stub, 'motion');
    expect(t.calls).toEqual(['claim general', 'pick motion', 'claim general', 'publish']);
    expect(t.states.map((s) => s.kind)).toEqual(['claiming', 'picking', 'publishing', 'sharing']);
    expect(t.share.current).toEqual({ kind: 'sharing', audio: true });
  });

  it('says when no sound was obtained', async () => {
    const t = setup();
    t.room.audio = false;
    await t.share.start('general', t.stub, 'detail');
    expect(t.share.current).toEqual({ kind: 'sharing', audio: false });
  });

  it('someone else sharing is told before the picker opens, and nothing is released', async () => {
    const t = setup([busy]);
    await t.share.start('general', t.stub, 'detail');
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
      await t.share.start('general', t.stub, 'detail');
      expect(t.share.current, JSON.stringify(answer)).toEqual({ kind: 'idle', ended: end });
      expect(t.calls).toEqual(['claim general']);
    }
  });

  it('a picker closed without choosing releases the claim', async () => {
    const t = setup();
    t.room.pickFails = true;
    await t.share.start('general', t.stub, 'detail');
    expect(t.calls).toEqual(['claim general', 'pick detail', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'cancelled' });
  });

  it('a share taken by someone else while the picker was open stops the capture and releases nothing', async () => {
    const t = setup([granted, busy]);
    await t.share.start('general', t.stub, 'detail');
    expect(t.calls).toEqual(['claim general', 'pick detail', 'claim general', 'stop screen']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'busy' });
  });

  it('a second claim that fails otherwise stops the capture and releases the first', async () => {
    const t = setup([granted, unavailable]);
    await t.share.start('general', t.stub, 'detail');
    expect(t.calls).toEqual(['claim general', 'pick detail', 'claim general', 'stop screen', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'unavailable' });
  });

  it('a refused publish stops the capture and releases the claim', async () => {
    const t = setup();
    t.room.publishFails = true;
    await t.share.start('general', t.stub, 'detail');
    expect(t.calls).toEqual(['claim general', 'pick detail', 'claim general', 'publish', 'stop screen', 'release general']);
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'failed' });
  });

  it('does not start twice', async () => {
    const t = await sharing();
    await t.share.start('general', t.stub, 'detail');
    expect(t.calls).toEqual([]);
  });

  it('a stop while the picker is open releases at once, and stops what is picked when it comes', async () => {
    const t = setup();
    t.room.pick = deferred();
    const started = t.share.start('general', t.stub, 'detail');
    await settle();
    expect(t.share.current.kind).toBe('picking');
    await t.share.stop('left');
    expect(t.share.current).toEqual({ kind: 'idle', ended: 'left' });
    t.room.pick.resolve({ audio: true });
    await started;
    expect(t.calls).toEqual(['claim general', 'pick detail', 'stop screen', 'release general', 'stop screen']);
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
    const started = t.share.start('general', t.stub, 'detail');
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
      const started = t.share.start('general', t.stub, 'detail');
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
      expect(t.calls).toEqual(['claim general', 'pick detail']);
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
      );
      const started = share.start('general', t.stub, 'detail');
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
      await t.share.start('general', t.stub, 'detail');
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

  it('presence that ends the share says why, for each reason', async () => {
    for (const [reason, end] of [
      ['operator', 'operator'],
      ['access_lost', 'access_lost'],
      ['rotated', 'rotated'],
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
    await t.share.start('general', t.stub, 'detail');
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
    expect(endedNote('busy', 'alice')).toBe('alice is sharing. One person shares at a time.');
    expect(endedNote('busy', null)).toMatch(/^Someone else is sharing/);
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
