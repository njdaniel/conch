import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import type { SetStateAction } from 'react';
import { afterEach, expect, it, vi } from 'vitest';
import { VoiceDiagnostics } from './VoiceDiagnostics';
import { startDiagnostics, type StatsReport } from './stats';

const stateUpdates = vi.hoisted(() => vi.fn());
vi.mock('react', async (importOriginal) => {
  const real = await importOriginal<typeof import('react')>();
  return {
    ...real,
    useState: <T,>(initial?: T | (() => T)) => {
      const [value, setValue] = real.useState<T | undefined>(initial);
      if (initial !== undefined) return [value, setValue] as const;
      return [value, (next: SetStateAction<T | undefined>) => {
        stateUpdates(next);
        setValue(next);
      }] as const;
    },
  };
});

vi.mock('./stats', async (importOriginal) => {
  const real = await importOriginal<typeof import('./stats')>();
  return { ...real, startDiagnostics: vi.fn(real.startDiagnostics) };
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.mocked(startDiagnostics).mockReset();
  stateUpdates.mockClear();
});

it.each(['sampler stops', 'callback already queued'] as const)('guards state when the readout unmounts with a pending read (%s)', async (scenario) => {
  vi.useFakeTimers();
  let resolve!: (report: StatsReport) => void;
  const promise = new Promise<StatsReport>((finish) => { resolve = finish; });
  const track = { getRTCStatsReport: vi.fn(() => promise) };
  const delivered = vi.fn();
  const stop = vi.fn();
  if (scenario === 'callback already queued') {
    // Model a late delivery even after cleanup, so the test can detect an
    // attempted state update that React would silently ignore after unmount.
    vi.mocked(startDiagnostics).mockImplementationOnce((_sources, publish) => {
      void track.getRTCStatsReport().then(() => {
        delivered();
        publish({ calls: new Map(), sent: undefined, received: new Map() });
      });
      return stop;
    });
  }
  const { unmount } = render(<VoiceDiagnostics client={{ diagnosticsSources: () => [{ kind: 'sent', track }] }} sharing watched={[]} />);
  fireEvent.click(screen.getByRole('button', { name: 'Voice diagnostics' }));
  expect(track.getRTCStatsReport).toHaveBeenCalledTimes(1);
  unmount();
  await act(async () => {
    resolve(new Map([['screen', { type: 'outbound-rtp', kind: 'video', frameWidth: 1280, frameHeight: 720 }]]));
    await vi.advanceTimersByTimeAsync(4000);
  });

  expect(track.getRTCStatsReport).toHaveBeenCalledTimes(1);
  if (scenario === 'callback already queued') {
    expect(stop).toHaveBeenCalledOnce();
    expect(delivered).toHaveBeenCalledOnce();
  }
  expect(stateUpdates).not.toHaveBeenCalled();
});
