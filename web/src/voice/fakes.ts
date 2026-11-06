// Test doubles for the voice client: a clock the test moves by hand. Imported
// by tests only.
import type { Clock } from '../state/feed';
import type { Room } from './connection';

// The camera half of a room for a test that never turns a camera on: nothing
// is captured, and every capture is refused.
export type CameraHalf = Pick<
  Room,
  | 'captureCamera'
  | 'awaitCameraGrant'
  | 'publishCamera'
  | 'stopCamera'
  | 'keepCamera'
  | 'adoptCamera'
  | 'cameraTrack'
  | 'cameraInUse'
  | 'cameras'
  | 'switchCamera'
  | 'attachSelf'
  | 'setViewing'
  | 'setShown'
  | 'setCameras'
  | 'attachCamera'
>;

export function noCamera(): CameraHalf {
  return {
    captureCamera: () => Promise.reject(new DOMException('no camera in this test', 'NotFoundError')),
    awaitCameraGrant: () => Promise.reject(new Error('no camera in this test')),
    publishCamera: () => Promise.reject(new Error('no camera in this test')),
    stopCamera: () => Promise.resolve(),
    keepCamera: () => null,
    adoptCamera: () => undefined,
    cameraTrack: () => 'none',
    cameraInUse: () => null,
    cameras: () => Promise.resolve(null),
    switchCamera: () => Promise.resolve(),
    attachSelf: () => undefined,
    setViewing: () => undefined,
    setShown: () => undefined,
    setCameras: () => undefined,
    attachCamera: () => undefined,
  };
}

export class FakeClock implements Clock {
  now = 0;
  private next = 1;
  private timers = new Map<number, { at: number; run: () => void }>();

  setTimeout = (run: () => void, ms: number): unknown => {
    const id = this.next++;
    this.timers.set(id, { at: this.now + ms, run });
    return id;
  };

  clearTimeout = (handle: unknown): void => {
    this.timers.delete(handle as number);
  };

  // advance moves time forward, running each timer due on the way in order.
  advance(ms: number): void {
    const end = this.now + ms;
    for (;;) {
      let pick: [number, { at: number; run: () => void }] | null = null;
      for (const entry of this.timers) {
        if (entry[1].at <= end && (pick === null || entry[1].at < pick[1].at)) pick = entry;
      }
      if (pick === null) break;
      this.timers.delete(pick[0]);
      this.now = Math.max(this.now, pick[1].at);
      pick[1].run();
    }
    this.now = end;
  }

  get pending(): number {
    return this.timers.size;
  }
}

// settle lets promise continuations run.
export async function settle(): Promise<void> {
  for (let i = 0; i < 10; i++) await Promise.resolve();
}
