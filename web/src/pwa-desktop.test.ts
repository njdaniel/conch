import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { manifestLink } from '../vite.pwa';

// The hook is called as Vite calls it; its signature is a union, so cast.
type Hook = (html: string) => string;
const run = (mode: string, html: string): string => {
  const hook = manifestLink(mode).transformIndexHtml as Hook;
  return hook(html);
};

describe('the web app manifest link (issue #1970)', () => {
  const page = readFileSync('index.html', 'utf8');

  it('is in the browser build page', () => {
    expect(page).toContain('<link rel="manifest"');
    expect(run('production', page)).toContain('<link rel="manifest"');
  });

  it('is left out of the desktop build page, whose CSP would block it', () => {
    const out = run('desktop', page);
    expect(out).not.toContain('manifest');
    expect(out).toContain('<link rel="icon"');
  });
});
