// The real SDK, not a stub (issue #565): what the browser's picker is asked
// for when a share is picked. Up to 3840x2160 at up to 60 fps, as an ideal on
// Chromium and Firefox and a maximum on Safari, never an exact size or a
// minimum, so the browser captures a source at its own size and never scales
// it up. The picker is a stub that keeps what it was asked and refuses, as a
// person cancelling it would. jsdom, no network, no screen.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { newLiveKitRoom } from './sdk';

const chrome = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36';
const firefox = 'Mozilla/5.0 (X11; Linux x86_64; rv:143.0) Gecko/20100101 Firefox/143.0';
const safari = 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.6 Safari/605.1.15';

// pickedWith opens the picker through the adapter as the given browser and
// returns the video constraints it was asked for.
async function pickedWith(userAgent: string): Promise<unknown> {
  vi.spyOn(navigator, 'userAgent', 'get').mockReturnValue(userAgent);
  const asked: DisplayMediaStreamOptions[] = [];
  Object.defineProperty(navigator, 'mediaDevices', {
    configurable: true,
    value: {
      getDisplayMedia: (options: DisplayMediaStreamOptions) => {
        asked.push(options);
        return Promise.reject(new DOMException('Permission denied', 'NotAllowedError'));
      },
    },
  });
  const room = newLiveKitRoom();
  try {
    await expect(room.pickScreen()).rejects.toThrow('Permission denied');
  } finally {
    await room.close();
  }
  expect(asked).toHaveLength(1);
  return asked[0]?.video;
}

// Every constraint of the picture, flattened to "name.kind" or "name".
function kinds(video: unknown): string[] {
  if (typeof video !== 'object' || video === null) return [];
  const out: string[] = [];
  for (const [name, value] of Object.entries(video) as [string, unknown][]) {
    if (typeof value === 'object' && value !== null) for (const kind of Object.keys(value)) out.push(`${name}.${kind}`);
    else out.push(name);
  }
  return out.sort();
}

afterEach(() => {
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, 'mediaDevices');
});

describe('what the picker is asked for', () => {
  it.each([
    { browser: 'Chromium', userAgent: chrome },
    { browser: 'Firefox', userAgent: firefox },
  ])('on $browser: up to 3840x2160 at up to 60 fps, as ideals', async ({ userAgent }) => {
    const video = await pickedWith(userAgent);
    expect(video).toEqual({ width: { ideal: 3840 }, height: { ideal: 2160 }, frameRate: 60 });
  });

  it('on Safari: up to 3840x2160 at up to 60 fps, as maximums', async () => {
    const video = await pickedWith(safari);
    expect(video).toEqual({ width: { max: 3840 }, height: { max: 2160 }, frameRate: 60 });
  });

  // A bare frame rate is an ideal too. Nothing forces a size or a rate: no
  // exact and no minimum, which would make the browser scale a smaller
  // source up, or refuse it.
  it.each([chrome, firefox, safari])('never an exact size or a minimum (%s)', async (userAgent) => {
    for (const kind of kinds(await pickedWith(userAgent))) expect(kind).not.toMatch(/\.(exact|min)$/);
  });
});
