import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { appearanceAccents, type AppearanceAccent, type AppearanceTheme } from './appearance';

const css = readFileSync('src/app.css', 'utf8');
type Tokens = Record<string, string>;
type ExplicitTheme = Exclude<AppearanceTheme, 'system'>;

function tokens(theme: ExplicitTheme, accent: AppearanceAccent): Tokens {
  const resolved: Tokens = {};
  const rules = css.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g);
  const allowed = new Set([
    ':root',
    `:root[data-theme='${theme}']`,
    `:root[data-accent='${accent}']`,
    `:root[data-theme='${theme}'][data-accent='${accent}']`,
  ]);
  for (const rule of rules) {
    const selector = rule[1]?.trim() ?? '';
    if (selector.includes('@media')) continue;
    if (!selector.split(',').some((part) => allowed.has(part.trim()))) continue;
    for (const declaration of rule[2]?.matchAll(/--([\w-]+):\s*(#[\da-fA-F]{6})\s*;/g) ?? []) {
      if (declaration[1] !== undefined && declaration[2] !== undefined) resolved[declaration[1]] = declaration[2].toLowerCase();
    }
  }
  return resolved;
}

function luminance(hex: string): number {
  const channels = [1, 3, 5].map((at) => Number.parseInt(hex.slice(at, at + 2), 16) / 255);
  const linear = channels.map((channel) => (channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4));
  return (linear[0] ?? 0) * 0.2126 + (linear[1] ?? 0) * 0.7152 + (linear[2] ?? 0) * 0.0722;
}

export function contrast(foreground: string, background: string): number {
  const light = Math.max(luminance(foreground), luminance(background));
  const dark = Math.min(luminance(foreground), luminance(background));
  return (light + 0.05) / (dark + 0.05);
}

function pair(tokens: Tokens, foreground: string, background: string): [string, string] {
  const fg = tokens[foreground];
  const bg = tokens[background];
  expect(fg, `--${foreground} is defined`).toMatch(/^#[\da-f]{6}$/);
  expect(bg, `--${background} is defined`).toMatch(/^#[\da-f]{6}$/);
  return [fg ?? '', bg ?? ''];
}

const surfaces = ['bg', 'surface', 'raised', 'hover'] as const;
const themes: readonly ExplicitTheme[] = ['light', 'dark', 'high-contrast'];

describe('appearance token contrast', () => {
  it.each(themes)('keeps shared text and state tokens legible in %s', (theme) => {
    for (const accent of appearanceAccents) {
      const palette = tokens(theme, accent);
      const minimum = theme === 'high-contrast' ? 7 : 4.5;
      for (const surface of surfaces) {
        for (const foreground of ['text', 'muted', 'accent', 'danger', 'warn']) {
          const [fg, bg] = pair(palette, foreground, surface);
          expect(contrast(fg, bg), `${theme}/${accent}: --${foreground} on --${surface}`).toBeGreaterThanOrEqual(minimum);
        }
        const [focus, bg] = pair(palette, 'focus', surface);
        expect(contrast(focus, bg), `${theme}/${accent}: focus on ${surface}`).toBeGreaterThanOrEqual(3);
      }
      for (const [foreground, background] of [
        ['accent', 'accent-bg'],
        ['on-accent', 'accent'],
        ['on-accent', 'accent-hover'],
        ['danger', 'danger-bg'],
      ] as const) {
        const [fg, bg] = pair(palette, foreground, background);
        expect(contrast(fg, bg), `${theme}/${accent}: --${foreground} on --${background}`).toBeGreaterThanOrEqual(minimum);
      }
      for (const hue of ['hue-0', 'hue-1', 'hue-2', 'hue-3', 'hue-4', 'hue-5']) {
        const [fg, bg] = pair(palette, 'avatar-text', hue);
        expect(contrast(fg, bg), `${theme}/${accent}: avatar on --${hue}`).toBeGreaterThanOrEqual(minimum);
      }
    }
  });

  it('defines a distinct accent value in every explicit preset', () => {
    for (const accent of appearanceAccents) {
      const values = themes.map((theme) => tokens(theme, accent)['accent']);
      expect(new Set(values).size, accent).toBe(themes.length);
    }
  });
});
