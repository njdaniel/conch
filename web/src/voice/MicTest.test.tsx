// The private microphone test (issue #846), driven by fakes for the capture,
// the device list and the audio context: no browser, no device, no network.
// What matters is cleanup: every way the test ends releases the microphone.
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Microphone } from './connection';
import { deniedNote, headphonesNote, levelOf, lostNote, MicTest, noMicrophoneNote, type MicTestDeps } from './MicTest';
import type { TestCapture } from './sdk';

class FakeTrack extends EventTarget {
  stopped = false;
  getSettings() {
    return {};
  }
}

class FakeGain {
  gain = { value: 0 };
  connect() {
    return this;
  }
}

class FakeContext {
  closed = false;
  destination = {};
  sources: { disconnected: boolean }[] = [];
  gains: FakeGain[] = [];
  createMediaStreamSource() {
    const node = { disconnected: false, connect: () => node, disconnect: () => { node.disconnected = true; } };
    this.sources.push(node);
    return node;
  }
  createAnalyser() {
    return {
      fftSize: 0,
      connect: () => undefined,
      getFloatTimeDomainData: (a: Float32Array) => {
        a.fill(0.2);
      },
    };
  }
  createGain() {
    const g = new FakeGain();
    this.gains.push(g);
    return g;
  }
  resume() {
    return Promise.resolve();
  }
  close() {
    this.closed = true;
    return Promise.resolve();
  }
}

interface Harness {
  deps: MicTestDeps;
  asked: (string | null)[];
  captures: { track: FakeTrack; capture: TestCapture }[];
  contexts: FakeContext[];
  devices: { list: readonly Microphone[] | null };
  watchers: Set<() => void>;
  failWith: { name: string } | null;
}

const two: readonly Microphone[] = [
  { id: 'a', label: 'Mic A' },
  { id: 'b', label: 'Mic B' },
];

function harness(): Harness {
  const h: Harness = {
    asked: [],
    captures: [],
    contexts: [],
    devices: { list: two },
    watchers: new Set(),
    failWith: null,
    deps: undefined as unknown as MicTestDeps,
  };
  h.deps = {
    capture: (choice) => {
      h.asked.push(choice);
      if (h.failWith !== null) {
        const err = new Error('refused');
        err.name = h.failWith.name;
        return Promise.reject(err);
      }
      const track = new FakeTrack();
      const capture = {
        track: track as unknown as MediaStreamTrack,
        stop: () => {
          track.stopped = true;
        },
      };
      h.captures.push({ track, capture });
      return Promise.resolve(capture);
    },
    microphones: () => Promise.resolve(h.devices.list),
    watch: (cb) => {
      h.watchers.add(cb);
      return () => {
        h.watchers.delete(cb);
      };
    },
    audioContext: () => {
      const c = new FakeContext();
      h.contexts.push(c);
      return c as unknown as AudioContext;
    },
  };
  return h;
}

// The component is rendered once with stable deps, as the panel does.
async function start(h: Harness) {
  render(<MicTest deps={h.deps} />);
  fireEvent.click(screen.getByRole('button', { name: 'Test microphone' }));
  await act(async () => {
    await vi.advanceTimersByTimeAsync(10);
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal(
    'MediaStream',
    class {
      constructor(readonly tracks: unknown[]) {}
    },
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function released(h: Harness, n = h.captures.length) {
  expect(h.captures).toHaveLength(n);
  for (const c of h.captures) expect(c.track.stopped).toBe(true);
  for (const c of h.contexts) expect(c.closed).toBe(true);
  expect(h.watchers.size).toBe(0);
}

describe('starting and stopping', () => {
  it('shows a moving meter and is private, then Stop releases everything', async () => {
    const h = harness();
    await start(h);
    expect(screen.getByRole('button', { name: 'Stop test' })).toBeTruthy();
    expect(screen.getByText(/nothing is sent or recorded/i)).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100);
    });
    const meter = screen.getByLabelText<HTMLMeterElement>('Microphone test level');
    expect(meter.value).toBeGreaterThan(0);
    expect(h.captures[0]?.track.stopped).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Stop test' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    released(h, 1);
    expect(h.contexts[0]?.sources[0]?.disconnected).toBe(true);
    expect(screen.getByRole('button', { name: 'Test microphone' })).toBeTruthy();
  });

  it('releases the microphone when unmounted, as navigating away does', async () => {
    const h = harness();
    await start(h);
    cleanup();
    released(h, 1);
  });

  it('releases a microphone the browser opens after Stop was pressed', async () => {
    const h = harness();
    let finish: (c: TestCapture) => void = () => undefined;
    const stopped: boolean[] = [];
    h.deps = {
      ...h.deps,
      capture: () =>
        new Promise<TestCapture>((resolve) => {
          finish = resolve;
        }),
    };
    render(<MicTest deps={h.deps} />);
    fireEvent.click(screen.getByRole('button', { name: 'Test microphone' }));
    fireEvent.click(screen.getByRole('button', { name: 'Stop test' }));
    await act(async () => {
      finish({ track: new FakeTrack() as unknown as MediaStreamTrack, stop: () => stopped.push(true) });
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(stopped).toEqual([true]);
  });
});

describe('loopback', () => {
  it('is silent by default, plays back when asked with a headphones hint, and silent again', async () => {
    const h = harness();
    await start(h);
    const gain = h.contexts[0]?.gains[0];
    expect(gain?.gain.value).toBe(0);
    expect(screen.queryByText(headphonesNote)).toBeNull();
    fireEvent.click(screen.getByRole('checkbox', { name: /hear myself/i }));
    expect(gain?.gain.value).toBe(1);
    expect(screen.getByText(headphonesNote)).toBeTruthy();
    // Switching it does not restart the capture.
    expect(h.captures).toHaveLength(1);
    fireEvent.click(screen.getByRole('checkbox', { name: /hear myself/i }));
    expect(gain?.gain.value).toBe(0);
  });
});

describe('changing the microphone', () => {
  it('follows the choice to the new device and releases the old one', async () => {
    const h = harness();
    await start(h);
    const select = screen.getByRole('combobox', { name: 'Microphone' });
    expect(screen.getByRole('option', { name: 'Mic B' })).toBeTruthy();
    fireEvent.change(select, { target: { value: 'b' } });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(h.asked).toEqual([null, 'b']);
    expect(h.captures[0]?.track.stopped).toBe(true);
    expect(h.contexts[0]?.closed).toBe(true);
    expect(h.captures[1]?.track.stopped).toBe(false);
    expect(h.contexts[1]?.closed).toBe(false);
    expect(h.watchers.size).toBe(1);
  });
});

describe('when it cannot start or the microphone goes', () => {
  it.each([
    { name: 'NotAllowedError', note: deniedNote },
    { name: 'NotFoundError', note: noMicrophoneNote },
  ])('says $name and cleans up', async ({ name, note }) => {
    const h = harness();
    h.failWith = { name };
    await start(h);
    expect(screen.getByRole('status').textContent).toBe(note);
    expect(screen.getByRole('button', { name: 'Test microphone' })).toBeTruthy();
    expect(h.captures).toHaveLength(0);
    for (const c of h.contexts) expect(c.closed).toBe(true);
    expect(h.watchers.size).toBe(0);
  });

  it('says a failure of any other kind, and can be started again', async () => {
    const h = harness();
    h.failWith = { name: 'AbortError' };
    await start(h);
    expect(screen.getByRole('status').textContent).toMatch(/could not be opened/);
    h.failWith = null;
    fireEvent.click(screen.getByRole('button', { name: 'Test microphone' }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(screen.getByRole('button', { name: 'Stop test' })).toBeTruthy();
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('stops and says so when the capture ends', async () => {
    const h = harness();
    await start(h);
    await act(async () => {
      h.captures[0]?.track.dispatchEvent(new Event('ended'));
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(screen.getByRole('status').textContent).toBe(lostNote);
    released(h, 1);
    expect(screen.getByRole('button', { name: 'Test microphone' })).toBeTruthy();
  });

  it('stops and says so when the chosen device disappears from the list', async () => {
    const h = harness();
    await start(h);
    fireEvent.change(screen.getByRole('combobox', { name: 'Microphone' }), { target: { value: 'b' } });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10);
    });
    h.devices.list = [{ id: 'a', label: 'Mic A' }];
    await act(async () => {
      for (const cb of h.watchers) cb();
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(screen.getByRole('status').textContent).toBe(lostNote);
    released(h, 2);
    // The device that went is not chosen any more.
    expect(screen.getByRole<HTMLSelectElement>('combobox', { name: 'Microphone' }).value).toBe('');
    expect(screen.queryByRole('option', { name: 'Mic B' })).toBeNull();
  });

  it('keeps going when another device is plugged in', async () => {
    const h = harness();
    await start(h);
    h.devices.list = [...two, { id: 'c', label: 'Mic C' }];
    await act(async () => {
      for (const cb of h.watchers) cb();
      await vi.advanceTimersByTimeAsync(10);
    });
    expect(screen.getByRole('option', { name: 'Mic C' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Stop test' })).toBeTruthy();
    expect(h.captures[0]?.track.stopped).toBe(false);
  });
});

describe('levelOf', () => {
  it('is 0 for silence or nothing, and at most 1', () => {
    expect(levelOf(new Float32Array(0))).toBe(0);
    expect(levelOf(new Float32Array(16))).toBe(0);
    expect(levelOf(new Float32Array(16).fill(1))).toBe(1);
  });
});
