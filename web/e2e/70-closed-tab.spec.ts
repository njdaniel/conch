// A person who closes the tab without pressing Leave (issue #1776), and one
// who leaves while the browser is slow to let go of the audio.
//
// A closed tab never reaches the app's own leave (VoiceClient.leaveVoice,
// LiveKitAdapter.close): useVoice's pagehide only shuts the PTT gate. What
// sends LiveKit its LeaveRequest is livekit-client's own page-leave handler
// (disconnectOnPageLeave), which starts an asynchronous disconnect that a page
// teardown under load could outrun, leaving LiveKit to time the dead
// connection out (about 20 s, vanishedWithin in 67-voice-recovery) instead of
// dropping the person at once. CI saw that once (run 43063362427, p4 listed
// past 20 s). The first two variants prove a closed tab is dropped, by that
// livekit-client path; they do not guard #1777.
//
// The third variant guards #1777 (#1764): LiveKitAdapter.close() starts
// room.disconnect(true) at once and no longer waits for the AudioContext close
// (the untap), which can stall. It presses Leave voice with
// AudioContext.prototype.close never resolving and leaves the pages open;
// LiveKit must list nobody of theirs within leaveWithin. The pages are not
// closed: a closed page would be left by livekit-client's page-leave handler
// even with close() stalled, and the variant would not tell.
//
// In each, two people go together, each page's CPU throttled (CDP
// Emulation.setCPUThrottlingRate) so a handler runs late, and a third stays. For
// each leaving page the CDP session also records the frames its LiveKit signal
// socket sent after the leave began (a LeaveRequest is all a leaving page has
// left to send: media goes over WebRTC); that is an annotation, and LiveKit's
// own list, within leaveWithin of the start (people.ts: far under the 20 s a
// leave that never arrived takes), is the assertion.
//
// What this proves: Chromium, one machine, 6x throttled page CPU, Chromium's
// fake devices. What it does not: a tab killed by the OS or a browser crash
// (that is 67-voice-recovery's vanished page), other engines, a slow network.
import type { CDPSession, Page } from '@playwright/test';
import { expect, test } from './fixtures';
import { enter, expectClean, joinVoice, leave, leaveVoice, leaveWithin, type Someone } from './people';
import { answered } from './livekit-admin';
import { dockerAvailable, emptiesWithin, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

// How hard the closing pages' CPU is throttled: Chromium runs their scripts
// this many times slower. 6 is DevTools' own "low-tier mobile" setting; a
// leave that took one tick idle takes six here, which is what lets a teardown
// outrun it, while the page can still finish joining (the throttle starts
// after the join).
const throttle = 6;

// Counts the frames each page's signal socket sends, read passively from the
// CDP Network domain: nothing is routed or held, so the page's teardown is
// the browser's own (a routeWebSocket passthrough closes its server side
// before a frame it is forwarding is delivered, and lost the leave on a plain
// close).
class Frames {
  readonly sentAt: number[] = [];
  private readonly signal = new Set<string>();

  constructor(cdp: CDPSession) {
    cdp.on('Network.webSocketCreated', (e) => {
      if (/\/rtc/.test(e.url)) this.signal.add(e.requestId);
    });
    cdp.on('Network.webSocketFrameSent', (e) => {
      if (this.signal.has(e.requestId)) this.sentAt.push(Date.now());
    });
  }

  after(t: number): number {
    return this.sentAt.filter((s) => s >= t).length;
  }
}

// Makes AudioContext.close() never settle, before the page's scripts run: the
// slow untap #1777's close() must not wait for.
const stallAudioClose = () => {
  AudioContext.prototype.close = () => new Promise<void>(() => undefined);
};

test.describe('@voice a closed tab with LiveKit', () => {
  let vw: VoiceWorld;
  let stopWorld: (() => void) | undefined;

  test.beforeAll(async ({}, info) => {
    if (!dockerAvailable()) {
      if (process.env['CI'] !== undefined) throw new Error('browser voice needs Docker for the pinned LiveKit image, and there is none');
      info.skip(true, 'Docker is not available; the browser voice suite needs it (it fails, rather than skips, when CI is set)');
      return;
    }
    vw = await startVoiceWorld(loadWorld());
    stopWorld = vw.stop;
  });
  test.afterAll(() => {
    stopWorld?.();
  });

  const variants = [
    { name: 'closed as a person closes it (beforeunload, then pagehide)', how: 'person' },
    { name: 'closed with a plain page.close (no beforeunload)', how: 'plain' },
    { name: 'leaving voice with a stalled AudioContext.close, the pages left open (guards #1777)', how: 'leave' },
  ] as const;

  for (const v of variants) {
    test(`two tabs ${v.name} under load are gone from LiveKit within leaveWithin`, async ({ browser }, info) => {
      test.setTimeout(120_000);
      const people: Someone[] = [];
      const cdps: CDPSession[] = [];
      try {
        const stall = v.how === 'leave' ? { before: async (p: Page) => {
            await p.addInitScript(stallAudioClose);
          } } : {};
        people.push(await enter(browser, vw, vw.alice));
        people.push(await enter(browser, vw, vw.bob, stall));
        people.push(await enter(browser, vw, vw.carol, stall));
        const [alice, bob, carol] = people as [Someone, Someone, Someone];
        const leaving = [bob, carol];
        const frames: Frames[] = [];
        for (const p of leaving) {
          const cdp = await p.context.newCDPSession(p.page);
          cdps.push(cdp);
          // Before the join, so the signal socket's creation is seen.
          await cdp.send('Network.enable');
          frames.push(new Frames(cdp));
        }
        for (const p of people) await joinVoice(p, 'Join voice');
        await expect.poll(() => answered(vw.participants()), { timeout: 15_000 }).toEqual([vw.alice, vw.bob, vw.carol].map((m) => `p${String(m.id)}`).sort());
        for (const cdp of cdps) await cdp.send('Emulation.setCPUThrottlingRate', { rate: throttle });

        // The bound runs from here, the start of the leave, not from when the
        // closes resolve.
        const startedAt = Date.now();
        if (v.how === 'leave') await Promise.all(leaving.map((p) => leaveVoice(p)));
        // The pages of the third variant stay open: closing them would let
        // livekit-client's page-leave handler, still registered while the room
        // is connected, send the leave that close() failed to, and the variant
        // would pass with #1777 reverted (it did).
        if (v.how !== 'leave') await Promise.all(leaving.map((p) => p.page.close({ runBeforeUnload: v.how === 'person' })));
        await expect
          .poll(() => answered(vw.participants()), { timeout: leaveWithin + (Date.now() - startedAt), message: 'LiveKit drops bob and carol at once' })
          .toEqual([`p${String(vw.alice.id)}`]);
        const droppedAfter = Date.now() - startedAt;
        info.annotations.push({ type: 'LiveKit dropped both after', description: `${String(droppedAfter)} ms (bound ${String(leaveWithin)} ms)` });
        for (const [i, p] of leaving.entries()) {
          info.annotations.push({ type: `${p.member.name}'s signal frames sent after the leave began`, description: String(frames[i]?.after(startedAt) ?? 0) });
        }
        expect(droppedAfter, 'total time from the start of the leave').toBeLessThanOrEqual(leaveWithin);
        // The one who stayed saw nothing wrong.
        await expect(alice.page.getByRole('region', { name: 'Voice' })).toContainText(/Connected in #/);
        await expectClean(vw, alice);
      } finally {
        // Throttle off first (a page that outlived a failure), every person
        // closed whatever the others do, and LiveKit empty before the next
        // test, so a ghost of this one cannot pass or fail that one.
        await Promise.allSettled(cdps.map((cdp) => cdp.send('Emulation.setCPUThrottlingRate', { rate: 1 })));
        await Promise.allSettled(people.map((p) => leave(p)));
        await expect.poll(() => answered(vw.participants()), { timeout: emptiesWithin, message: 'LiveKit is empty again' }).toEqual([]);
      }
    });
  }
});
