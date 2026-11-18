import type { RemoteTrack } from 'livekit-client';
import { TrackEvent } from 'livekit-client';
import { afterEach, expect, it, vi } from 'vitest';
import { sizePictureInPicture } from './sdk';

afterEach(() => {
  Reflect.deleteProperty(document, 'pictureInPictureElement');
});

it('uses PiP dimensions on enter, resize and background page resize, then restores the stage', () => {
  const video = document.createElement('video');
  Object.defineProperty(video, 'clientWidth', { value: 320 });
  Object.defineProperty(video, 'clientHeight', { value: 180 });
  const pip = Object.assign(new EventTarget(), { width: 960, height: 540 });
  let current: Element | null = video;
  Object.defineProperty(document, 'pictureInPictureElement', { configurable: true, get: () => current });
  const observed: { width: number; height: number }[] = [];
  const listeners = new Set<() => void>();
  // The SDK dispatches synchronously: its publication observes dimensions
  // before the adapter, just as when a subscribed track is attached.
  const track = {
    on: (_event: string, listener: () => void) => { listeners.add(listener); },
    off: (_event: string, listener: () => void) => { listeners.delete(listener); },
    emit: (_event: string, dimensions: { width: number; height: number }) => {
      observed.push(dimensions);
      for (const listener of listeners) listener();
    },
  };
  const changed = vi.fn();
  let stop = sizePictureInPicture(video, track as unknown as RemoteTrack, changed);
  const enter = new Event('enterpictureinpicture');
  Object.defineProperty(enter, 'pictureInPictureWindow', { value: pip });
  video.dispatchEvent(enter);
  expect(observed.at(-1)).toEqual({ width: 960, height: 540 });
  expect(changed).toHaveBeenCalledOnce();
  pip.width = 1280;
  pip.height = 720;
  pip.dispatchEvent(new Event('resize'));
  expect(observed.at(-1)).toEqual({ width: 1280, height: 720 });
  track.emit(TrackEvent.VideoDimensionsChanged, { width: 0, height: 0 });
  expect(observed.at(-1)).toEqual({ width: 1280, height: 720 });
  // A renewed room attaches a new track while the same element stays in PiP.
  stop();
  stop = sizePictureInPicture(video, track as unknown as RemoteTrack, changed);
  expect(observed.at(-1)).toEqual({ width: 1280, height: 720 });
  current = null;
  video.dispatchEvent(new Event('leavepictureinpicture'));
  expect(observed.at(-1)).toEqual({ width: 320, height: 180 });
  const count = observed.length;
  pip.dispatchEvent(new Event('resize'));
  expect(observed).toHaveLength(count);
  stop();
  expect(listeners.size).toBe(0);
});

// The SDK scales an element's size by its pixel density (2 on a screen
// denser than 2), and the PiP size and the size given back on leave are
// scaled the same way, or a dense screen would be sent too small a layer.
it('scales the PiP size and the restored stage size as the SDK does on a dense screen', () => {
  const ratio = Object.getOwnPropertyDescriptor(window, 'devicePixelRatio');
  Object.defineProperty(window, 'devicePixelRatio', { configurable: true, value: 3 });
  try {
    const video = document.createElement('video');
    Object.defineProperty(video, 'clientWidth', { value: 320 });
    Object.defineProperty(video, 'clientHeight', { value: 180 });
    let current: Element | null = video;
    Object.defineProperty(document, 'pictureInPictureElement', { configurable: true, get: () => current });
    const observed: { width: number; height: number }[] = [];
    const track = { on: vi.fn(), off: vi.fn(), emit: (_event: string, dimensions: { width: number; height: number }) => { observed.push(dimensions); } };
    const stop = sizePictureInPicture(video, track as unknown as RemoteTrack, vi.fn());
    const enter = new Event('enterpictureinpicture');
    Object.defineProperty(enter, 'pictureInPictureWindow', { value: Object.assign(new EventTarget(), { width: 480, height: 270 }) });
    video.dispatchEvent(enter);
    expect(observed.at(-1)).toEqual({ width: 960, height: 540 });
    current = null;
    video.dispatchEvent(new Event('leavepictureinpicture'));
    expect(observed.at(-1)).toEqual({ width: 640, height: 360 });
    stop();
  } finally {
    if (ratio === undefined) Reflect.deleteProperty(window, 'devicePixelRatio');
    else Object.defineProperty(window, 'devicePixelRatio', ratio);
  }
});
