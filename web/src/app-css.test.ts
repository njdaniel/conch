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

// Issue #2189: the web Voice options panel is a popover (position fixed, a
// sheet on a phone). The desktop app's own panel (voice/native) shares the
// .voice-options class and is drawn as before: every rule that positions or
// covers with it is for .popover or .sheet, which only the web panel sets.
// Each selector of a comma list counts on its own: one bare .voice-options
// among them is a rule for the bare class.
// Splits a selector list on its top-level commas only: not those inside
// :is(), :where(), :not() or any other parentheses or brackets.
export function topLevelCommas(list: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let from = 0;
  for (let i = 0; i < list.length; i++) {
    const c = list[i];
    if (c === '(' || c === '[') depth++;
    else if (c === ')' || c === ']') depth--;
    else if (c === ',' && depth === 0) {
      out.push(list.slice(from, i));
      from = i + 1;
    }
  }
  out.push(list.slice(from));
  return out;
}

export function bareFixedSelectors(css: string): string[] {
  const rules = [...css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)].map((m) => ({ selector: (m[1] ?? '').trim(), body: m[2] ?? '' }));
  return rules
    .filter((r) => /position:\s*fixed/.test(r.body))
    .flatMap((r) => topLevelCommas(r.selector))
    .map((s) => s.trim())
    .filter((s) => /\.voice-options\b/.test(s) && !/\.(popover|sheet)\b/.test(s));
}

describe('Voice options overlay rules are for the web panel only (issue #2189)', () => {
  it('no rule that fixes a .voice-options panel applies to the bare class', () => {
    const css = readFileSync('src/app.css', 'utf8');
    expect(/\.voice-options\.popover:not\(\[hidden\]\)\s*\{[^}]*position:\s*fixed/.test(css)).toBe(true);
    expect(bareFixedSelectors(css)).toEqual([]);
  });

  it('catches a bare selector in a comma list', () => {
    expect(bareFixedSelectors('.voice-options, .voice-options.popover { position: fixed; }')).toEqual(['.voice-options']);
    expect(bareFixedSelectors('.voice-options.popover, .voice-options.sheet { position: fixed; }')).toEqual([]);
  });

  it('does not split inside :is(), :where() or :not()', () => {
    expect(bareFixedSelectors('.voice-options:is(.a, .b).popover { position: fixed; }')).toEqual([]);
    expect(bareFixedSelectors('.voice-options:is(.a, .b) { position: fixed; }')).toEqual(['.voice-options:is(.a, .b)']);
    expect(topLevelCommas('.x:is(.a, .b), .y:where(.c, .d)')).toEqual(['.x:is(.a, .b)', ' .y:where(.c, .d)']);
  });
});
