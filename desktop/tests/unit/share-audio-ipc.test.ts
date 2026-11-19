// The share sound's four IPC functions (issue #1854): refused outright with
// main's switch off, from a subframe, a stale generation or another sender;
// the person's act taken from main's own record (for start, the Share click's
// arm once main granted the capture of its pick, and nothing else; a fresh
// click for choose and for an unmute), never from a word in a payload; and,
// over the real share sound, no live share, a watch that is no share, a
// cancelled pick, the choice none, and the order. The rig is
// share-audio.support.ts; the Ipc and the DisplayCapture are the real ones.
import { describe, expect, it } from 'vitest';

import { DisplayCapture, type CaptureContents } from '../../main/display-capture.ts';
import { DocumentGate } from '../../main/document.ts';
import { Ipc, type ContentsLike, type FrameLike, type InvokeEvent, type ShareSoundApi } from '../../main/ipc.ts';
import { SHARE_SOUND_MS, ShareAllowance } from '../../main/share-allowance.ts';
import { SHARE_SOUND_FUNCTIONS } from '../../shared/bridge-names.ts';
import { DOC, deferred, rig, trace, type Rig } from './share-audio.support.ts';

function frame(token = 'frame-1', over: Partial<FrameLike> = {}): FrameLike {
  return { frameToken: token, parent: null, detached: false, url: 'conch-app://ui/', ...over };
}

interface Harness {
  readonly ipc: Ipc;
  readonly gate: DocumentGate;
  readonly contents: ContentsLike;
  readonly main: FrameLike;
  readonly logs: string[];
  readonly clock: { now: number };
  readonly input: { recent: boolean; taken: number };
  readonly share: ShareAllowance;
  event(over?: Partial<InvokeEvent>): InvokeEvent;
}

// The gate is at generation DOC with its frame committed: the document the
// rig's companion was granted in.
function harness(shareSound: ShareSoundApi | null): Harness {
  const main = frame();
  const contents = { mainFrame: main, isDestroyed: () => false };
  const gate = new DocumentGate();
  for (let n = 0; n < DOC; n++) gate.begin();
  gate.commit(main);
  const logs: string[] = [];
  const clock = { now: 1_000_000 };
  const input = { recent: false, taken: 0 };
  const share = new ShareAllowance({ now: () => clock.now, generation: () => gate.generation });
  const gestures = {
    take: () => {
      input.taken++;
      return input.recent ? ('pointer' as const) : null;
    },
  };
  const none = new Proxy({}, { get: () => () => Promise.resolve(null) });
  const ipc = new Ipc({
    owned: () => contents,
    gate,
    service: none as never,
    feeds: none as never,
    files: none as never,
    gestures,
    profiles: none as never,
    voice: none as never,
    installed: none as never,
    invites: none as never,
    notifications: none as never,
    settings: none as never,
    share,
    ...(shareSound === null ? {} : { shareSound }),
    now: () => clock.now,
    log: (line) => logs.push(line),
  });
  return { ipc, gate, contents, main, logs, clock, input, share, event: (over = {}) => ({ sender: contents, senderFrame: main, ...over }) };
}

// What a fake share sound was asked, in order.
function recorder(): { api: ShareSoundApi; calls: [string, unknown[]][] } {
  const calls: [string, unknown[]][] = [];
  const api: ShareSoundApi = {
    start: (...args) => {
      calls.push(['start', args]);
      return Promise.resolve({ ok: true, share: 1 });
    },
    choose: (...args) => {
      calls.push(['choose', args]);
      return Promise.resolve({ ok: true, share: null });
    },
    mute: (...args) => {
      calls.push(['mute', args]);
      return Promise.resolve({ ok: true });
    },
    end: (...args) => {
      calls.push(['end', args]);
      return Promise.resolve(true);
    },
    shareEnded: (...args) => {
      calls.push(['shareEnded', args]);
      return Promise.resolve(true);
    },
  };
  return { api, calls };
}

const CALLS: [string, unknown[]][] = [
  ['shareSoundStart', []],
  ['shareSoundChoose', [{ choice: 'none' }]],
  ['shareSoundMute', [true]],
  ['shareSoundEnd', [{ share: 1 }]],
  ['shareSoundShareEnded', []],
];

const refused = { ok: false, kind: 'refused' };

// The Share click as main sees it, up to a granted capture: the page's
// shareArm() takes the click, the companion grant takes the grant use, the
// display-media request takes the capture use, and main answering the pick
// with a source grants the capture (display-capture.ts).
async function armedAndGranted(h: Harness): Promise<void> {
  h.input.recent = true;
  expect(await h.ipc.invoke(h.event(), 'shareArm', [])).toEqual({ ok: true, value: true });
  h.input.recent = false;
  expect(h.share.takeGrant(h.contents)).toBe(true);
  expect(h.share.takeCapture(h.contents)).toBe(true);
  h.share.grantCapture(h.contents);
}

describe('the share sound’s IPC functions', () => {
  it('are exactly the five of the bridge', () => {
    expect([...SHARE_SOUND_FUNCTIONS].sort()).toEqual(CALLS.map(([name]) => name).sort());
  });

  it.each(CALLS)('%s is refused with the switch off, before any act is taken, and nothing runs', async (name, args) => {
    const h = harness(null);
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), name, args)).toEqual(refused);
    expect(h.logs).toEqual(['conch-desktop: refused an IPC call (disabled); 1 so far']);
    // The person's act was not spent on a call that cannot run.
    expect(h.input.taken).toBe(0);
  });

  describe.each(CALLS)('%s is refused', (name, args) => {
    const cases: [string, (h: Harness) => InvokeEvent, string][] = [
      ['from a subframe', (h) => h.event({ senderFrame: frame('frame-2', { parent: h.main }) }), 'subframe'],
      ['from another web contents', (h) => h.event({ sender: { isDestroyed: () => false, mainFrame: h.main } }), 'sender'],
      ['from a frame that is not the main frame', (h) => h.event({ senderFrame: frame('other') }), 'frame'],
      ['from a foreign origin', (h) => h.event({ senderFrame: frame('frame-1', { url: 'https://evil.example/' }) }), 'origin'],
      [
        'from a stale document generation',
        (h) => {
          h.gate.begin();
          return h.event();
        },
        'generation',
      ],
    ];

    it.each(cases)('%s', async (_case, make, reason) => {
      const { api, calls } = recorder();
      const h = harness(api);
      h.input.recent = true;
      expect(await h.ipc.invoke(make(h), name, args)).toEqual(refused);
      expect(h.logs).toEqual([`conch-desktop: refused an IPC call (${reason}); 1 so far`]);
      expect(calls).toEqual([]);
    });
  });
});

describe('the person’s act', () => {
  it('start: a fresh click alone is not its act (a click is not a granted capture), and is not spent', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(h.logs.at(-1)).toContain('(gesture)');
    expect(h.input.taken).toBe(0);
    expect(calls).toEqual([]);
  });

  it('start: the Share click’s arm runs it once main granted the capture, for the document that asked', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    await armedAndGranted(h);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual({ ok: true, value: { ok: true, share: 1 } });
    expect(calls).toEqual([['start', [DOC]]]);
  });

  it('start: no click and no arm is refused', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(h.logs.at(-1)).toContain('(gesture)');
    expect(calls).toEqual([]);
  });

  it('start: a payload that says it is a gesture is not one (arguments are refused, and a gesture is main’s own record)', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    for (const args of [[{ gesture: true }], [true], [{ userGesture: true }], [1]]) expect(await h.ipc.invoke(h.event(), 'shareSoundStart', args)).toEqual(refused);
    expect(calls).toEqual([]);
    // Refused by size or shape, never as a click that was missing: the payload is not read for one.
    expect(h.logs.every((l) => /\((shape|size)\)/.test(l))).toBe(true);
  });

  it('start: the Share click’s arm is its act only once main granted the capture of its pick, and only once', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    // The page's shareArm() at the start of the Share click: the click is taken there.
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareArm', [])).toEqual({ ok: true, value: true });
    h.input.recent = false;
    // Armed, but main has not allowed the picker: no sound use yet.
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(h.share.takeGrant(h.contents)).toBe(true);
    // The pick started (the capture use is spent) but main has not answered
    // it with a source: still no sound use (#1854 review F2).
    expect(h.share.takeCapture(h.contents)).toBe(true);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    h.share.grantCapture(h.contents);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual({ ok: true, value: { ok: true, share: 1 } });
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(calls).toEqual([['start', [DOC]]]);
  });

  it('the allowance’s sound use waits for a granted capture, expires, is cleared by every end, and is for one document', () => {
    const h = harness(recorder().api);
    // A pick that was cancelled or refused: the capture use spent, nothing granted.
    h.share.arm(h.contents);
    h.share.takeCapture(h.contents);
    expect(h.share.takeSound(h.contents)).toBe(false);
    // A grant with no pick of this arm records nothing.
    h.share.arm(h.contents);
    h.share.grantCapture(h.contents);
    expect(h.share.takeSound(h.contents)).toBe(false);
    h.share.arm(h.contents);
    h.share.takeCapture(h.contents);
    h.share.grantCapture(h.contents);
    h.clock.now += SHARE_SOUND_MS + 1;
    expect(h.share.takeSound(h.contents)).toBe(false);
    h.share.arm(h.contents);
    h.share.takeCapture(h.contents);
    h.share.grantCapture(h.contents);
    h.share.clear();
    expect(h.share.takeSound(h.contents)).toBe(false);
    h.share.arm(h.contents);
    h.share.takeCapture(h.contents);
    h.share.grantCapture(h.contents);
    h.gate.begin();
    expect(h.share.takeSound(h.contents)).toBe(false);
    // Another window never takes it.
    const h2 = harness(recorder().api);
    h2.share.arm(h2.contents);
    h2.share.takeCapture(h2.contents);
    h2.share.grantCapture(h2.contents);
    expect(h2.share.takeSound({})).toBe(false);
    expect(h2.share.takeSound(null)).toBe(false);
    expect(h2.share.takeSound(h2.contents)).toBe(true);
  });

  it('choose needs a fresh click: the Share click’s arm does not stand for the toggle', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    h.share.arm(h.contents);
    h.share.takeCapture(h.contents);
    h.share.grantCapture(h.contents);
    expect(await h.ipc.invoke(h.event(), 'shareSoundChoose', [{ choice: 'none' }])).toEqual(refused);
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareSoundChoose', [{ choice: 'none' }])).toEqual({ ok: true, value: { ok: true, share: null } });
    expect(calls).toEqual([['choose', ['none', DOC]]]);
  });

  it('a mute and an end only reduce what is shared: no act, and the document is main’s', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    expect(await h.ipc.invoke(h.event(), 'shareSoundMute', [true])).toEqual({ ok: true, value: { ok: true } });
    expect(await h.ipc.invoke(h.event(), 'shareSoundEnd', [{ share: 7 }])).toEqual({ ok: true, value: true });
    expect(calls).toEqual([
      ['mute', [true, DOC]],
      ['end', [7]],
    ]);
  });

  it('the page’s word that its share ended needs no act, takes no argument, and is for the caller’s document', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    expect(await h.ipc.invoke(h.event(), 'shareSoundShareEnded', [])).toEqual({ ok: true, value: true });
    expect(await h.ipc.invoke(h.event(), 'shareSoundShareEnded', [DOC + 1])).toEqual(refused);
    expect(await h.ipc.invoke(h.event(), 'shareSoundShareEnded', [{ document: DOC + 1 }])).toEqual(refused);
    expect(calls).toEqual([['shareEnded', [DOC]]]);
  });

  it('an unmute resumes what is shared, so it needs the person’s fresh act (#1854 review F4)', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    expect(await h.ipc.invoke(h.event(), 'shareSoundMute', [false])).toEqual(refused);
    expect(h.logs.at(-1)).toContain('(gesture)');
    expect(calls).toEqual([]);
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareSoundMute', [false])).toEqual({ ok: true, value: { ok: true } });
    expect(calls).toEqual([['mute', [false, DOC]]]);
  });
});

describe('what a page may say', () => {
  const bad: [string, unknown[]][] = [
    // The scope, the application and every identifier are main's: no key carries them.
    ['shareSoundChoose', []],
    ['shareSoundChoose', ['none']],
    ['shareSoundChoose', [{}]],
    ['shareSoundChoose', [{ choice: 'application' }]],
    ['shareSoundChoose', [{ choice: 'everything' }]],
    ['shareSoundChoose', [{ choice: '' }]],
    ['shareSoundChoose', [{ choice: 'none', scope: 'application' }]],
    ['shareSoundChoose', [{ choice: 'none', application_name: 'Firefox' }]],
    ['shareSoundChoose', [{ choice: 'none', publisher: 'companion' }]],
    ['shareSoundChoose', [{ choice: 'none' }, 'extra']],
    ['shareSoundChoose', [{ scope: 'system_excluding_call' }]],
    ['shareSoundMute', []],
    ['shareSoundMute', ['true']],
    ['shareSoundMute', [1]],
    ['shareSoundMute', [null]],
    ['shareSoundMute', [true, true]],
    ['shareSoundEnd', []],
    ['shareSoundEnd', [1]],
    ['shareSoundEnd', [{}]],
    ['shareSoundEnd', [{ share: 0 }]],
    ['shareSoundEnd', [{ share: -1 }]],
    ['shareSoundEnd', [{ share: 1.5 }]],
    ['shareSoundEnd', [{ share: '1' }]],
    ['shareSoundEnd', [{ share: Number.MAX_SAFE_INTEGER + 2 }]],
    ['shareSoundEnd', [{ share: 1, pid: 4 }]],
    ['shareSoundEnd', [{ share: 1 }, 1]],
    ['shareSoundStart', ['general']],
  ];

  it.each(bad)('%s %j is refused as a shape, and nothing runs', async (name, args) => {
    const { api, calls } = recorder();
    const h = harness(api);
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), name, args)).toEqual(refused);
    expect(h.logs.at(-1)).toMatch(/\((shape|size)\)/);
    expect(calls).toEqual([]);
  });

  it('every choice of the vocabulary is taken', async () => {
    const { api, calls } = recorder();
    const h = harness(api);
    h.input.recent = true;
    for (const choice of ['none', 'system_excluding_call']) expect((await h.ipc.invoke(h.event(), 'shareSoundChoose', [{ choice }])).ok).toBe(true);
    expect(calls.map(([, args]) => args[0])).toEqual(['none', 'system_excluding_call']);
  });
});

// Over the real share sound, with the helper and conchd faked.
describe('through the real share sound', () => {
  function wired(r: Rig): Harness {
    if (r.sound === null) throw new Error('no sound');
    return harness(r.sound);
  }

  it('no live share: refused by the service, nothing sent', async () => {
    const r = rig();
    const h = wired(r);
    await armedAndGranted(h);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual({ ok: true, value: { ok: false, error: 'no-share' } });
    expect(r.events).toEqual([]);
  });

  it('a companion held for watching is no share: a click is refused, and an arm with no capture the share sound knows of sends nothing', async () => {
    const r = rig();
    const h = wired(r);
    await r.watch();
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(r.events).toEqual([]);
    // Even with an arm (a capture granted that the share sound was never told
    // of for this companion), the share sound itself refuses.
    await armedAndGranted(h);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual({ ok: true, value: { ok: false, error: 'no-share' } });
    expect(r.events).toEqual([]);
    expect(r.sup.wire).toEqual([]);
  });

  it('the choice “with sound” while only watching keeps the choice and asks nothing', async () => {
    const r = rig({ choice: 'none' });
    const h = wired(r);
    await r.watch();
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareSoundChoose', [{ choice: 'system_excluding_call' }])).toEqual({ ok: true, value: { ok: true, share: null } });
    expect(r.prefs.choice).toBe('system_excluding_call');
    expect(r.events).toEqual([]);
    expect(r.sup.wire).toEqual([]);
    // And no failure is shown for it: there is no share to have failed.
    expect(r.seen()).toEqual({ choice: 'system_excluding_call' });
  });

  it('the choice none: not offered, no consent, no command', async () => {
    const r = rig({ choice: 'none' });
    const h = wired(r);
    await r.live();
    await armedAndGranted(h);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual({ ok: true, value: { ok: false, error: 'not-offered' } });
    expect(r.events).toEqual([]);
  });

  it('a document that is not the share’s is no live share', async () => {
    const r = rig();
    const h = wired(r);
    await r.live();
    h.gate.begin();
    h.gate.commit(h.main);
    await armedAndGranted(h);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual({ ok: true, value: { ok: false, error: 'no-share' } });
    expect(await h.ipc.invoke(h.event(), 'shareSoundMute', [true])).toEqual({ ok: true, value: { ok: false, error: 'not-running' } });
    expect(r.events).toEqual([]);
  });

  it('a live share, the Share click’s arm: the consent, then the command; then the page’s end: the stop, then the DELETE', async () => {
    const r = rig();
    const h = wired(r);
    await r.live();
    await armedAndGranted(h);
    const started = await h.ipc.invoke(h.event(), 'shareSoundStart', []);
    expect(started).toEqual({ ok: true, value: { ok: true, share: expect.any(Number) as number } });
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on']);
    const share = started.ok ? (started.value as { share: number }).share : 0;
    h.input.recent = false;
    expect(await h.ipc.invoke(h.event(), 'shareSoundEnd', [{ share }])).toEqual({ ok: true, value: true });
    expect(trace(r, 'helper:capture off', 'http:DELETE audio')).toEqual(['helper:capture off', 'http:DELETE audio']);
    // The same end again, or another share's, ends nothing.
    expect(await h.ipc.invoke(h.event(), 'shareSoundEnd', [{ share }])).toEqual({ ok: true, value: false });
    expect(trace(r, 'helper:capture off', 'http:DELETE audio')).toHaveLength(2);
  });
});

// The real DisplayCapture, between the Share click and the share's sound: only
// a pick main answered with a source makes the sound use and the share
// sound's record of a share (#1854 review F2).
describe('through the real display capture', () => {
  function capturing(r: Rig, sources: readonly { id: string; name: string }[] | Promise<readonly { id: string; name: string }[]>): { h: Harness; capture: DisplayCapture; pick: () => Promise<unknown> } {
    if (r.sound === null) throw new Error('no sound');
    const sound = r.sound;
    const h = harness(sound);
    const owned = h.contents as unknown as CaptureContents & { forcefullyCrashRenderer(): void; once(): void; close(): void };
    Object.assign(owned, { forcefullyCrashRenderer: () => undefined, once: () => undefined, close: () => undefined });
    const capture = new DisplayCapture({
      owned: () => owned,
      gate: () => h.gate,
      share: h.share,
      live: () => r.companion.liveOrigin() !== null,
      available: () => 'available',
      getSources: () => Promise.resolve(sources),
      granted: () => {
        sound.captureGranted(h.gate.generation);
      },
      log: (line) => h.logs.push(line),
    });
    const pick = (): Promise<unknown> =>
      new Promise((resolve) => {
        capture.handle({ frame: h.main, videoRequested: true, audioRequested: false }, resolve);
      });
    return { h, capture, pick };
  }

  async function shareClick(h: Harness): Promise<void> {
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareArm', [])).toEqual({ ok: true, value: true });
    h.input.recent = false;
  }

  it('a pick the person cancelled: no sound use, and start is refused with nothing sent', async () => {
    const r = rig();
    await r.watch();
    const { h, pick } = capturing(r, []);
    await shareClick(h);
    expect(await pick()).toBeNull();
    expect(h.logs.some((l) => l.includes('cancelled or denied'))).toBe(true);
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    // A click after it is no act for start either.
    h.input.recent = true;
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(r.events).toEqual([]);
  });

  it.each(['the companion ended', 'the document changed'] as const)('a stale pick (%s during it) records no capture: no sound use, and no share the share sound knows of', async (what) => {
    const r = rig();
    await r.watch();
    const sources = deferred<readonly { id: string; name: string }[]>();
    const { h, pick } = capturing(r, sources.promise);
    await shareClick(h);
    const answered = pick();
    if (what === 'the companion ended') {
      await r.companion.stop();
      // app.ts: every companion end clears the allowance.
      h.share.clear();
    } else {
      // A new document in the window: the pick's frame is no longer its.
      h.gate.begin();
      h.gate.commit(frame('frame-2'));
    }
    sources.resolve([{ id: 'screen:1', name: 'one' }]);
    expect(await answered).toBeNull();
    expect(h.logs.some((l) => l.includes('the grant or the document ended during the pick'))).toBe(true);
    // A later Watch gives a companion again: the stale pick is not its share.
    if (what === 'the companion ended') expect(await r.companion.grant({ channel: 'general' })).toMatchObject({ ok: true });
    r.events.length = 0;
    // Refused at IPC either way: no sound use (and, for the new document, the
    // old frame is not the window's any more).
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    // Nor does the share sound hold a share for a later choice of "on".
    expect(await r.sound?.start(DOC)).toEqual({ ok: false, error: 'no-share' });
    expect(await r.sound?.choose('system_excluding_call', DOC)).toEqual({ ok: true, share: null });
    expect(r.events).toEqual([]);
  });

  it('a pick refused for more than one source: no sound use', async () => {
    const r = rig();
    await r.watch();
    const { h, pick } = capturing(r, [
      { id: 'screen:1', name: 'one' },
      { id: 'screen:2', name: 'two' },
    ]);
    await shareClick(h);
    expect(await pick()).toBeNull();
    expect(await h.ipc.invoke(h.event(), 'shareSoundStart', [])).toEqual(refused);
    expect(r.events).toEqual([]);
  });

  it('a pick answered with a source: the capture is granted, and the share’s sound is asked in order', async () => {
    const r = rig();
    await r.watch();
    const { h, pick } = capturing(r, [{ id: 'screen:1', name: 'one' }]);
    await shareClick(h);
    expect(await pick()).toEqual({ video: { id: 'screen:1', name: 'one' } });
    const started = await h.ipc.invoke(h.event(), 'shareSoundStart', []);
    expect(started).toEqual({ ok: true, value: { ok: true, share: expect.any(Number) as number } });
    expect(r.events).toEqual(['http:PUT audio', 'helper:capture on']);
  });
});
