// A person who closes the tab without pressing Leave (issue #1776).
//
// The page leaves voice by livekit-client's own page-leave handler
// (disconnectOnPageLeave), which starts an asynchronous disconnect: send LiveKit
// a LeaveRequest, then close the socket. Under load a page's teardown could
// outrun it, and LiveKit then lists the person until it times the dead
// connection out (about 20 s, vanishedWithin in 67-voice-recovery) instead of
// at once. CI saw that once (run 43063362427, p4 listed past 20 s), before
// #1764 made LiveKitVoiceRoom.close() start its disconnect at once.
//
// Here two people close their tabs together, never pressing Leave, with each
// page's CPU throttled (CDP Emulation.setCPUThrottlingRate) so the handler runs
// late, and a third stays. The test reads, for each closing page, whether its
// LiveKit signal socket sent a frame after the close began (a LeaveRequest is
// the only thing a page that is leaving has left to send: the audio and video
// go over WebRTC, not the signal socket; recorded as an annotation), and
// LiveKit's own list, which must
// drop exactly those two within leaveWithin (people.ts), far under the 20 s a
// leave that never arrived takes.
//
// What this proves: Chromium, one machine, 6x throttled page CPU, Chromium's
// fake devices. What it does not: a tab killed by the OS or a browser crash
// (that is 67-voice-recovery's vanished page), other engines, a slow network.
import type { Page } from '@playwright/test';
import { expect, test } from './fixtures';
import { enter, joinVoice, leave, leaveWithin, type Someone } from './people';
import { answered } from './livekit-admin';
import { dockerAvailable, startVoiceWorld, type VoiceWorld } from './voice-world';
import { loadWorld } from './world';

// How hard the closing pages' CPU is throttled: Chromium runs their scripts
// this many times slower. 6 is DevTools' own "low-tier mobile" setting; a
// leave that took one tick idle takes six here, which is what lets a teardown
// outrun it, while the page can still finish joining (the throttle starts
// after the join).
const throttle = 6;

// The signal socket of a page, passed through to LiveKit, counting what the
// page sends it and when (the evidence a leave was sent; see its use below).
class Frames {
  readonly sentAt: number[] = [];

  async install(page: Page): Promise<void> {
    await page.routeWebSocket(/\/rtc/, (ws) => {
      const server = ws.connectToServer();
      ws.onMessage((m) => {
        this.sentAt.push(Date.now());
        server.send(m);
      });
      server.onMessage((m) => {
        ws.send(m);
      });
      ws.onClose((code, reason) => {
        void server.close({ code, reason });
      });
      server.onClose((code, reason) => {
        void ws.close({ code, reason });
      });
    });
  }

  after(t: number): number {
    return this.sentAt.filter((s) => s >= t).length;
  }
}

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

  for (const how of [{ name: 'as a person closes it (beforeunload, then pagehide)', runBeforeUnload: true }, { name: 'with a plain page.close (no beforeunload)', runBeforeUnload: false }]) {
    test(`two tabs closed together under load, ${how.name}, are gone from LiveKit within leaveWithin`, async ({ browser }, info) => {
      test.setTimeout(120_000);
      const bobFrames = new Frames();
      const carolFrames = new Frames();
      const people: Someone[] = [];
      try {
        people.push(await enter(browser, vw, vw.alice));
        people.push(await enter(browser, vw, vw.bob, how.runBeforeUnload ? { before: (p) => bobFrames.install(p) } : {}));
        people.push(await enter(browser, vw, vw.carol, how.runBeforeUnload ? { before: (p) => carolFrames.install(p) } : {}));
        const [alice, bob, carol] = people as [Someone, Someone, Someone];
        for (const p of people) await joinVoice(p, 'Join voice');
        await expect.poll(() => answered(vw.participants()), { timeout: 15_000 }).toEqual([vw.alice, vw.bob, vw.carol].map((m) => `p${String(m.id)}`).sort());

        for (const p of [bob, carol]) {
          const cdp = await p.context.newCDPSession(p.page);
          await cdp.send('Emulation.setCPUThrottlingRate', { rate: throttle });
        }

        const closedAt = Date.now();
        await Promise.all([bob, carol].map((p) => p.page.close({ runBeforeUnload: how.runBeforeUnload })));
                await expect
          .poll(() => answered(vw.participants()), { timeout: leaveWithin, message: 'LiveKit drops bob and carol at once' })
          .toEqual([`p${String(vw.alice.id)}`]);
        const droppedAfter = Date.now() - closedAt;
        info.annotations.push({ type: 'LiveKit dropped both after', description: `${String(droppedAfter)} ms (bound ${String(leaveWithin)} ms)` });
        // Evidence, not an assertion: LiveKit's list above is the test. The
        // frame count is recorded where it is read (the person's close), and
        // not for the plain close at all, because a routeWebSocket
        // passthrough can itself lose the leave there: the page's socket
        // closes with the page, and the route closes its server side before
        // the frame it was forwarding is delivered (seen: with the logger
        // LiveKit kept both people past 5 s on a plain close; without it, 0 of
        // 4 runs did).
        if (how.runBeforeUnload) {
          for (const [name, f] of [['bob', bobFrames], ['carol', carolFrames]] as const) {
            info.annotations.push({ type: `${name}'s signal frames sent after the close began`, description: String(f.after(closedAt)) });
          }
        }
        expect(alice.page.isClosed()).toBe(false);
      } finally {
        for (const p of people) await leave(p);
      }
    });
  }
});
