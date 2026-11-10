// The packaged smoke test of the renderer's two new allowances and their
// refusals (issue #1580, D4-03 of #772; Q-D4-1 and Q-D4-4 on #1578). Needs a
// display (CI: xvfb-run) and the unpacked build in dist/, like shell.spec.ts.
//
// Under Xvfb there is no companion grant and no screen-cast portal, so what is
// asserted here is the refusals: a LiveKit-shaped request is cancelled by
// main's request filter, and getDisplayMedia rejects with no grant and with no
// gesture. The grant itself and a real portal pick are the manual check of
// #1580 and the full run of #1584.
import dgram from 'node:dgram';
import { networkInterfaces } from 'node:os';

import { chromium, expect, test, type Browser, type Page } from '@playwright/test';

import { launch, type Launched } from './launch.ts';

test.describe('the renderer’s network and capture allowances, with no companion grant', () => {
  let app: Launched;
  let browser: Browser;
  let page: Page;

  test.beforeAll(async () => {
    app = launch({ debugPort: true });
    browser = await chromium.connectOverCDP(await app.cdpUrl());
    const context = browser.contexts()[0];
    if (context === undefined) throw new Error('the app has no browser context');
    page = context.pages()[0] ?? (await context.waitForEvent('page'));
    await page.waitForLoadState('load');
  });

  test.afterAll(async () => {
    await browser.close();
    await app.stop();
  });

  test('cancels a LiveKit-shaped https request and wss connection in main', async () => {
    const outcome = await page.evaluate(`Promise.all([
      fetch('https://lk.example.test/twirp/livekit.RoomService/ListRooms', { method: 'POST' }).then(() => 'sent', () => 'refused'),
      new Promise((resolve) => {
        const ws = new WebSocket('wss://lk.example.test/rtc?access_token=x');
        ws.onopen = () => resolve('open');
        ws.onerror = () => resolve('refused');
      }),
    ])`);
    expect(outcome).toEqual(['refused', 'refused']);
    // The CSP lets wss: and https: as far as main (Q-D4-1); main's filter
    // names the refusal, scheme and host only.
    await expect.poll(() => app.output()).toContain('conch-desktop: blocked a request to https://lk.example.test');
    expect(app.output()).toContain('conch-desktop: blocked a request to wss://lk.example.test');
    expect(app.output()).not.toContain('access_token');
  });

  test('rejects getDisplayMedia with no gesture', async () => {
    const result = await page.evaluate("navigator.mediaDevices.getDisplayMedia({ video: true }).then(() => 'granted', (e) => e.name)");
    expect(result).not.toBe('granted');
  });

  test('rejects getDisplayMedia right after a raw click, which arms nothing, and says why', async () => {
    await page.evaluate(`window.__share = 'pending'; document.addEventListener('mouseup', () => {
      navigator.mediaDevices.getDisplayMedia({ video: true, audio: true }).then(() => { window.__share = 'granted'; }, (e) => { window.__share = e.name; });
    }, { once: true });`);
    await page.mouse.click(5, 5);
    await expect.poll(() => page.evaluate('window.__share')).not.toBe('pending');
    expect(await page.evaluate('window.__share')).not.toBe('granted');
    // The reason is main's, and it is not the picker's: nothing was opened.
    // A click alone no longer arms capture (the share allowance, #1580); only
    // the page's shareArm() does.
    await expect.poll(() => app.output()).toMatch(/display capture refused: no share allowance/);
  });

  // The positive path (S3 of #1621): after a real click the page's shareArm()
  // arms the allowance, so the display-media request gets past the allowance
  // check (display-capture.ts permits) and is refused for the next reason,
  // not for lack of an allowance. That proves the wiring from the bridge to
  // permits; the grant and the portal are absent under Xvfb. It runs after
  // the no-allowance test above and reads only the log lines added since.
  test('arms on a real click, so getDisplayMedia is refused for the grant or the portal, not the allowance', async () => {
    const before = app.output().length;
    await page.evaluate(`window.__armed = 'pending'; window.__share = 'pending'; document.addEventListener('mouseup', () => {
      window.conchDesktop.shareArm().then((armed) => {
        window.__armed = armed;
        return navigator.mediaDevices.getDisplayMedia({ video: true });
      }).then(() => { window.__share = 'granted'; }, (e) => { window.__share = e.name; });
    }, { once: true });`);
    await page.mouse.click(5, 5);
    await expect.poll(() => page.evaluate('window.__share')).not.toBe('pending');
    expect(await page.evaluate('window.__armed')).toBe(true);
    expect(await page.evaluate('window.__share')).not.toBe('granted');
    await expect.poll(() => app.output().slice(before)).toMatch(/display capture refused: (no grant|share unavailable: no screen-cast portal)/);
    expect(app.output().slice(before)).not.toMatch(/display capture refused: no share allowance/);
  });

  test('keeps the microphone and camera denied', async () => {
    const result = await page.evaluate("navigator.mediaDevices.getUserMedia({ audio: true, video: true }).then(() => 'granted', (e) => e.name)");
    expect(result).not.toBe('granted');
  });

  // KNOWN RESIDUAL (M1 on #1621). WebTransport and WebRTC (ICE, STUN, TURN)
  // never reach webRequest.onBeforeRequest, and the CSP's `connect-src wss:
  // https:` permits WebTransport to any host. So, with no grant, the page can
  // still send UDP to a non-loopback address. Each test below states what
  // SHOULD hold once M1 is fixed (nothing arrives) and is marked test.fail:
  // while the residual exists the assertion fails and the test passes; the
  // day M1 is fixed the assertion holds, the test "unexpectedly passes" and
  // Playwright fails it, which is the cue to drop test.fail.
  // A listener on a non-loopback interface address, as the reviewer's probe
  // did: loopback is not what a real exfiltration target looks like.
  const nonLoopback = ((): string | null => {
    for (const list of Object.values(networkInterfaces())) {
      for (const a of list ?? []) if (a.family === 'IPv4' && !a.internal) return a.address;
    }
    return null;
  })();

  async function listen(): Promise<{ port: number; seen: () => number; close: () => void }> {
    const socket = dgram.createSocket('udp4');
    let n = 0;
    socket.on('message', () => {
      n += 1;
    });
    await new Promise<void>((resolve) => socket.bind(0, nonLoopback ?? '0.0.0.0', resolve));
    return { port: socket.address().port, seen: () => n, close: () => socket.close() };
  }

  test('known residual (M1): a WebTransport to a non-loopback address sends nothing', async () => {
    test.fail();
    test.skip(nonLoopback === null, 'no non-loopback IPv4 interface on this machine');
    const udp = await listen();
    const target = `${nonLoopback ?? ''}:${String(udp.port)}`;
    try {
      await page.evaluate(`(async () => {
        if (typeof WebTransport === 'undefined') return 'none';
        const t = new WebTransport('https://${target}/x');
        return Promise.race([t.ready.then(() => 'ready', () => 'rejected'), new Promise((r) => setTimeout(() => r('timeout'), 3000))]);
      })()`);
      await new Promise((r) => setTimeout(r, 500));
      expect(udp.seen()).toBe(0);
    } finally {
      udp.close();
    }
  });

  test('known residual (M1): an RTCPeerConnection with a stun: server sends nothing to a non-loopback address', async () => {
    test.fail();
    test.skip(nonLoopback === null, 'no non-loopback IPv4 interface on this machine');
    const udp = await listen();
    const target = `${nonLoopback ?? ''}:${String(udp.port)}`;
    try {
      await page.evaluate(`(async () => {
        const pc = new RTCPeerConnection({ iceServers: [{ urls: 'stun:${target}' }] });
        pc.createDataChannel('x');
        await pc.setLocalDescription(await pc.createOffer());
        await new Promise((r) => setTimeout(r, 2500));
        pc.close();
        return 'done';
      })()`);
      expect(udp.seen()).toBe(0);
    } finally {
      udp.close();
    }
  });
});
