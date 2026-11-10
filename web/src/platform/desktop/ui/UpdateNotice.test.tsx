// The update notice (issue #1600): shown from main's state, silent otherwise,
// and a restart only on the click.
import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { DesktopUpdateBridge, InstalledView } from '../bridge';
import { UpdateNotice } from './UpdateNotice';

function install(first: InstalledView): { push: (view: InstalledView) => void; restart: ReturnType<typeof vi.fn> } {
  let listener: (view: InstalledView) => void = () => undefined;
  const restart = vi.fn(() => Promise.resolve());
  const update: DesktopUpdateBridge = {
    updateState: () => Promise.resolve(first),
    updateRestart: restart,
    onUpdateState: (l) => {
      listener = l;
      return () => {
        listener = () => undefined;
      };
    },
  };
  Object.defineProperty(window, 'conchDesktop', { value: update, configurable: true, writable: true });
  return {
    push: (view) => {
      act(() => {
        listener(view);
      });
    },
    restart,
  };
}

afterEach(() => {
  cleanup();
  Reflect.deleteProperty(window, 'conchDesktop');
});

describe('UpdateNotice', () => {
  it('shows nothing while nothing was replaced', async () => {
    install({ updated: false, version: null });
    const { container } = render(<UpdateNotice />);
    await act(async () => {
      await Promise.resolve();
    });
    expect(container.textContent).toBe('');
  });

  it('shows nothing without the update bridge', () => {
    Object.defineProperty(window, 'conchDesktop', { value: {}, configurable: true, writable: true });
    const { container } = render(<UpdateNotice />);
    expect(container.textContent).toBe('');
  });

  it('says Conch was updated to the version when state already says so', async () => {
    install({ updated: true, version: '0.1.0-alpha.18' });
    render(<UpdateNotice />);
    expect(await screen.findByText('Conch was updated to 0.1.0-alpha.18. Restart when you’re ready.')).toBeTruthy();
  });

  it('appears when main sends the state, and shows the version as text', async () => {
    const bridge = install({ updated: false, version: null });
    render(<UpdateNotice />);
    await act(async () => {
      await Promise.resolve();
    });
    bridge.push({ updated: true, version: '<b>1</b>' });
    expect(screen.getByRole('status').textContent).toContain('<b>1</b>');
    expect(screen.getByRole('status').querySelector('b')).toBeNull();
  });

  it('asks main to restart only on the click', async () => {
    const bridge = install({ updated: true, version: '0.1.0-alpha.18' });
    render(<UpdateNotice />);
    const button = await screen.findByRole('button', { name: 'Restart' });
    expect(bridge.restart).not.toHaveBeenCalled();
    fireEvent.click(button);
    expect(bridge.restart).toHaveBeenCalledOnce();
  });
});
