// The version display: the server's version always, and the desktop app's own
// beside it in the desktop build only (issue #946).
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import * as api from './api';
import { platform } from './platform';
import { ServerVersion } from './ServerVersion';

beforeEach(() => {
  vi.spyOn(api, 'serverHealth').mockResolvedValue({ status: 'ok', version: 'v0.1.0-alpha.9', temporary: false } as never);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  Reflect.deleteProperty(platform.services, 'appVersion');
});

describe('ServerVersion', () => {
  it('in the browser build shows the server’s version alone', async () => {
    render(<ServerVersion className="hint" />);
    await screen.findByText('v0.1.0-alpha.9');
    expect(screen.queryByText(/App/)).toBeNull();
    expect(screen.getByText(/Version/).textContent).toBe('Version v0.1.0-alpha.9');
  });

  it('in the desktop build shows the app’s own version beside the server’s', async () => {
    Object.defineProperty(platform.services, 'appVersion', { value: () => Promise.resolve('1.2.3'), configurable: true, writable: true });
    render(<ServerVersion className="hint" />);
    await screen.findByText('v0.1.0-alpha.9');
    await screen.findByText('1.2.3');
    const line = screen.getByText(/Version/).closest('p');
    expect(line?.textContent).toBe('Version v0.1.0-alpha.9 · App 1.2.3');
  });

  it('shows the app’s version even when the server has not answered', async () => {
    vi.spyOn(api, 'serverHealth').mockResolvedValue(null);
    Object.defineProperty(platform.services, 'appVersion', { value: () => Promise.resolve('1.2.3'), configurable: true, writable: true });
    render(<ServerVersion className="hint" />);
    await screen.findByText('1.2.3');
    expect(screen.queryByText('v0.1.0-alpha.9')).toBeNull();
  });
});
