import { describe, expect, it } from 'vitest';

import { CONTENT_SECURITY_POLICY } from '../../main/csp.ts';

describe('CONTENT_SECURITY_POLICY', () => {
  it('is exactly the policy of linux-desktop.md §3 with connect-src none', () => {
    expect(CONTENT_SECURITY_POLICY).toBe(
      "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' blob: data:; connect-src 'none'; object-src 'none'; base-uri 'none'; frame-src 'none'; frame-ancestors 'none'; form-action 'none'; worker-src 'none'",
    );
  });

  it('allows no inline script, eval or remote origin', () => {
    expect(CONTENT_SECURITY_POLICY).not.toMatch(/unsafe-|\*|https?:|wss?:/);
  });
});
