import { describe, expect, it } from 'vitest';

import { refusalLine } from './admin/permissions';
import { DESKTOP_UNAVAILABLE, desktopUnavailableLine } from './appLimit';

describe('an operation the desktop app refused (issue #1768)', () => {
  it('reads as the app’s own limit, not as a network failure or the server’s refusal', () => {
    const line = refusalLine({ failure: 'invalid', code: DESKTOP_UNAVAILABLE }, 'No credential was made.');
    expect(line).toBe(`${desktopUnavailableLine} No credential was made.`);
    expect(line).not.toContain('Cannot reach');
  });
});
