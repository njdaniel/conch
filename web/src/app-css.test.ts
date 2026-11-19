import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

// Track every parenthesis, including :not() and :is(): a nested :has() is
// forbidden even through another pseudo-class. Ignore comments and strings,
// where those characters do not form selectors.
function hasNestedHas(css: string): boolean {
  const code = css.replace(/\/\*[\s\S]*?\*\/|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'/g, '');
  const stack: boolean[] = [];
  for (const [token] of code.matchAll(/:has\(|[()]/gi)) {
    if (token === ')') stack.pop();
    else {
      const has = token.toLowerCase() === ':has(';
      if (has && stack.includes(true)) return true;
      stack.push(has);
    }
  }
  return false;
}

describe('CSS :has() hygiene (issue #1931)', () => {
  it.each([
    '.centre:has(> .stage:not(:empty):has(.compact)) {}',
    '.centre:has(.stage:is(:has(.compact))) {}',
    '.centre:HAS(.stage\n:has(.compact)) {}',
  ])('rejects nested :has() in %s', (css) => {
    expect(hasNestedHas(css)).toBe(true);
  });

  it.each([
    '.centre:has(> .stage:not(:empty) .compact) {}',
    '.centre:has(.stage):not(:has(.compact)) {}',
    '/* :has(:has()) */ .stage:has([title=":has("]) {}',
  ])('allows unnested :has() in %s', (css) => {
    expect(hasNestedHas(css)).toBe(false);
  });

  it('keeps app.css free of nested :has(), which browsers discard', () => {
    expect(hasNestedHas(readFileSync('src/app.css', 'utf8'))).toBe(false);
  });
});
