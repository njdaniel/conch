// The reload notice (issue #1534): shown once when the server's version
// changes during a session, never reloads by itself, quiet otherwise.
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import * as api from './api';
import { UpdateNotice, updateCheckInterval, updateText } from './UpdateNotice';

const health = (version: string) => ({ version, temporary: false });

describe('UpdateNotice', () => {
  let answers: (string | null)[];
  let reload: Mock<() => void>;
  beforeEach(() => {
    vi.useFakeTimers();
    answers = [];
    reload = vi.fn<() => void>();
    vi.spyOn(api, 'serverHealth').mockImplementation(() => {
      const next = answers.shift();
      return Promise.resolve(next === undefined || next === null ? null : health(next));
    });
  });
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  async function tick() {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(updateCheckInterval);
    });
  }

  it('says nothing while the version is the same', async () => {
    answers = ['v1', 'v1', 'v1'];
    render(<UpdateNotice reload={reload} />);
    await act(async () => {});
    await tick();
    await tick();
    expect(screen.queryByText(updateText)).toBeNull();
  });

  it('says nothing when a check fails, and keeps the baseline', async () => {
    answers = ['v1', null, 'v1'];
    render(<UpdateNotice reload={reload} />);
    await act(async () => {});
    await tick();
    await tick();
    expect(screen.queryByText(updateText)).toBeNull();
  });

  it('shows the notice once as a polite status when the version changes, and reloads only on the button', async () => {
    answers = ['v1', 'v2', 'v3'];
    render(<UpdateNotice reload={reload} />);
    await act(async () => {});
    await tick();
    const status = screen.getByRole('status');
    expect(status.textContent).toContain(updateText);
    expect(screen.getAllByText(updateText)).toHaveLength(1);
    await tick();
    expect(screen.getAllByText(updateText)).toHaveLength(1);
    expect(reload).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Reload' }));
    expect(reload).toHaveBeenCalledTimes(1);
  });
});
