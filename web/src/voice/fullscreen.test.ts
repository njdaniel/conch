// Both the phone's shared screen and the enlarged camera use this path.
// No browser or device: the container and orientation APIs are controlled.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fullscreenVideo } from './fullscreen';

afterEach(() => { vi.unstubAllGlobals(); });

function player(width = 1920, height = 1080) {
  const container = document.createElement('div');
  const video = document.createElement('video');
  container.append(video);
  Object.defineProperties(video, {
    videoWidth: { value: width },
    videoHeight: { value: height },
  });
  const page = Object.assign(new EventTarget(), {
    fullscreenEnabled: true,
    fullscreenElement: null as Element | null,
  });
  vi.stubGlobal('document', page);
  const lock = vi.fn<(_: 'landscape') => Promise<void>>().mockResolvedValue(undefined);
  const unlock = vi.fn();
  vi.stubGlobal('screen', { orientation: { lock, unlock } });
  const request = vi.fn(() => {
    page.fullscreenElement = container;
    page.dispatchEvent(new Event('fullscreenchange'));
    return Promise.resolve();
  });
  container.requestFullscreen = request;
  const exit = () => {
    page.fullscreenElement = null;
    page.dispatchEvent(new Event('fullscreenchange'));
  };
  return { container, video, page, lock, unlock, request, exit };
}

describe('full-screen video', () => {
  it('requests the container first and waits for it before locking landscape', async () => {
    const p = player();
    let entered: () => void = () => undefined;
    p.request.mockImplementation(() => new Promise<void>((resolve) => {
      entered = () => { p.page.fullscreenElement = p.container; resolve(); };
    }));
    const pending = fullscreenVideo(p.container, p.video);
    expect(p.request).toHaveBeenCalledExactlyOnceWith();
    expect(p.request.mock.contexts[0]).toBe(p.container);
    expect(p.lock).not.toHaveBeenCalled();
    entered();
    await pending;
    expect(p.lock).toHaveBeenCalledExactlyOnceWith('landscape');
    p.exit();
  });

  it('ignores a rejected orientation lock and unlocks on exit', async () => {
    const p = player();
    p.lock.mockRejectedValue(new Error('not supported'));
    await expect(fullscreenVideo(p.container, p.video)).resolves.toBeUndefined();
    expect(p.lock).toHaveBeenCalledWith('landscape');
    expect(p.unlock).not.toHaveBeenCalled();
    p.exit();
    expect(p.unlock).toHaveBeenCalledTimes(1);
    // The exit listener is removed, so later full-screen sessions own theirs.
    p.exit();
    expect(p.unlock).toHaveBeenCalledTimes(1);
  });

  it.each([[720, 1280], [1000, 1000], [0, 0]])('does not rotate a %i by %i video', async (width, height) => {
    const p = player(width, height);
    await fullscreenVideo(p.container, p.video);
    expect(p.request).toHaveBeenCalledTimes(1);
    expect(p.lock).not.toHaveBeenCalled();
    p.exit();
    expect(p.unlock).toHaveBeenCalledTimes(1);
  });

  it.each([undefined, {}, { lock: () => { throw new Error('lock refused'); }, unlock: () => { throw new Error('unlock refused'); } }, { unlock: () => Promise.reject(new Error('unlock refused')) }])('tolerates missing or refusing orientation APIs: %j', async (orientation) => {
    const p = player();
    vi.stubGlobal('screen', { orientation });
    await expect(fullscreenVideo(p.container, p.video)).resolves.toBeUndefined();
    expect(p.exit).not.toThrow();
    // Let a rejected asynchronous unlock settle too.
    await Promise.resolve();
  });

  it('does not lock after leaving while the full-screen request settles', async () => {
    const p = player();
    const pending = fullscreenVideo(p.container, p.video);
    p.exit();
    await pending;
    expect(p.lock).not.toHaveBeenCalled();
    expect(p.unlock).toHaveBeenCalledTimes(1);
  });

  it('ignores a rejected full-screen request and removes its exit listener', async () => {
    const p = player();
    p.request.mockRejectedValue(new Error('full screen refused'));
    await expect(fullscreenVideo(p.container, p.video)).resolves.toBeUndefined();
    p.exit();
    expect(p.lock).not.toHaveBeenCalled();
    expect(p.unlock).not.toHaveBeenCalled();
  });

  it('uses the iPhone video fallback when full screen is not enabled', async () => {
    const p = player();
    p.page.fullscreenEnabled = false;
    const enter = vi.fn();
    Object.defineProperty(p.video, 'webkitEnterFullscreen', { value: enter });
    await fullscreenVideo(p.container, p.video);
    expect(enter).toHaveBeenCalledExactlyOnceWith();
    expect(enter.mock.contexts[0]).toBe(p.video);
    expect(p.request).not.toHaveBeenCalled();
    expect(p.lock).not.toHaveBeenCalled();
    p.exit();
    expect(p.unlock).not.toHaveBeenCalled();
  });
});
