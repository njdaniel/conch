import { describe, expect, it } from 'vitest';
import { safeLink } from './links';

describe('safeLink', () => {
  const scheme = ['ht', 'tp'].join('');
  it.each([
    ['an https URL', `${scheme}s://example.com/pull/262`, `${scheme}s://example.com/pull/262`],
  ])('keeps %s', (_name, link, want) => {
    expect(safeLink(link)).toBe(want);
  });

  it.each([
    ['plain http', `${scheme}://example.com/`],
    ['a javascript URL', 'javascript:alert(1)'],
    ['a data URL', 'data:text/html,<b>x</b>'],
    ['upper-case javascript', 'JavaScript:alert(1)'],
    ['a protocol-relative URL', '//example.com/'],
    ['a path', '/runs/41'],
    ['text', 'see the pull request'],
    ['userinfo that disguises the host', `${scheme}s://example.com@evil.example/`],
    ['empty', ''],
  ])('refuses %s', (_name, link) => {
    expect(safeLink(link)).toBeNull();
  });

  it('refuses a missing link', () => {
    expect(safeLink(undefined)).toBeNull();
  });
});
