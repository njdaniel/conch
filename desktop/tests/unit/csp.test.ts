import { describe, expect, it } from 'vitest';

import { CONTENT_SECURITY_POLICY } from '../../main/csp.ts';

describe('CONTENT_SECURITY_POLICY', () => {
  it('is exactly the policy of linux-desktop.md §3 as amended by Q-D4-1 (connect-src wss: https:)', () => {
    expect(CONTENT_SECURITY_POLICY).toBe(
      "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' blob: data:; connect-src wss: https:; object-src 'none'; base-uri 'none'; frame-src 'none'; frame-ancestors 'none'; form-action 'none'; worker-src 'none'",
    );
  });

  it('allows no inline script, eval or remote origin outside connect-src', () => {
    // Q-D4-1: only connect-src names a network scheme, and only wss: and
    // https: (never ws:, http: or a host); main's request filter narrows
    // those to the live grant's origin (guards.test.ts).
    const rest = CONTENT_SECURITY_POLICY.split('; ').filter((d) => !d.startsWith('connect-src '));
    expect(rest.join('; ')).not.toMatch(/unsafe-|\*|https?:|wss?:/);
    expect(CONTENT_SECURITY_POLICY).not.toMatch(/unsafe-|\*|\bws:|\bhttp:/);
    expect(CONTENT_SECURITY_POLICY.split('; ').filter((d) => d.startsWith('connect-src '))).toEqual(['connect-src wss: https:']);
  });
});
