import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as api from './api';
import { AppearanceSettings } from './AppearanceSettings';
import { applyAppearance, defaultAppearance, type AppearancePreferences } from './appearance';

const origin = 'https://one.example';
const account = 7;
const chosen: AppearancePreferences = { theme: 'dark', accent: 'violet', density: 'compact' };

beforeEach(() => {
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  applyAppearance(defaultAppearance);
  vi.restoreAllMocks();
});

describe('appearance preferences', () => {
  it('reads and writes a per-server, per-account choice', () => {
    api.writeAppearancePreferences(origin, account, chosen);
    expect(api.readAppearancePreferences(origin, account)).toEqual(chosen);
    expect(window.localStorage.getItem(`conch.web.appearance.v1.${origin}.${String(account)}`)).toBe(JSON.stringify(chosen));
    expect(api.readAppearancePreferences(origin, 8)).toEqual(defaultAppearance);
    expect(api.readAppearancePreferences('https://two.example', account)).toEqual(defaultAppearance);
  });

  it.each([
    ['broken JSON', '{broken'],
    ['a non-object', 'true'],
    ['unknown values', JSON.stringify({ theme: 'sepia', accent: '#123456', density: 'tiny' })],
    ['wrong types', JSON.stringify({ theme: 1, accent: false, density: null })],
  ])('falls back safely for %s', (_name, stored) => {
    window.localStorage.setItem(`conch.web.appearance.v1.${origin}.${String(account)}`, stored);
    expect(api.readAppearancePreferences(origin, account)).toEqual(defaultAppearance);
  });

  it('keeps valid fields when another field is corrupt', () => {
    window.localStorage.setItem(
      `conch.web.appearance.v1.${origin}.${String(account)}`,
      JSON.stringify({ theme: 'light', accent: 'not-allowed', density: 'compact' }),
    );
    expect(api.readAppearancePreferences(origin, account)).toEqual({ theme: 'light', accent: 'green', density: 'compact' });
  });

  it('uses defaults and does not throw when storage is blocked', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new DOMException('Blocked', 'SecurityError');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('Blocked', 'SecurityError');
    });
    expect(api.readAppearancePreferences(origin, account)).toEqual(defaultAppearance);
    expect(() => {
      api.writeAppearancePreferences(origin, account, chosen);
    }).not.toThrow();
  });
});

describe('appearance root attributes', () => {
  it('applies explicit presets, accents and density without inline style', () => {
    applyAppearance(chosen);
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    expect(document.documentElement.getAttribute('data-accent')).toBe('violet');
    expect(document.documentElement.getAttribute('data-density')).toBe('compact');
    expect(document.documentElement.hasAttribute('style')).toBe(false);
  });

  it('leaves the theme attribute absent for System', () => {
    applyAppearance(chosen);
    applyAppearance({ theme: 'system', accent: 'green', density: 'comfortable' });
    expect(document.documentElement.hasAttribute('data-theme')).toBe(false);
    expect(document.documentElement.getAttribute('data-accent')).toBe('green');
    expect(document.documentElement.getAttribute('data-density')).toBe('comfortable');
  });
});

describe('Appearance settings controls', () => {
  function Controls() {
    const [value, setValue] = useState<AppearancePreferences>(defaultAppearance);
    return (
      <AppearanceSettings
        value={value}
        onChange={(next) => {
          setValue(next);
          applyAppearance(next);
        }}
      />
    );
  }

  it('offers only the named choices and applies each control', () => {
    render(<Controls />);
    const region = screen.getByRole('region', { name: 'Appearance' });
    const theme = within(region).getByRole('combobox', { name: 'Theme preset' });
    const accent = within(region).getByRole('combobox', { name: 'Accent color' });
    const density = within(region).getByRole('combobox', { name: 'Chat density' });
    expect(within(theme).getAllByRole('option').map((option) => option.textContent)).toEqual(['System', 'Light', 'Dark', 'High contrast']);
    expect(within(accent).getAllByRole('option').map((option) => option.textContent)).toEqual(['Green', 'Blue', 'Violet', 'Pink', 'Orange', 'Cyan']);
    expect(within(density).getAllByRole('option').map((option) => option.textContent)).toEqual(['Comfortable', 'Compact']);

    theme.focus();
    expect(document.activeElement).toBe(theme);
    fireEvent.change(theme, { target: { value: 'high-contrast' } });
    fireEvent.change(accent, { target: { value: 'cyan' } });
    fireEvent.change(density, { target: { value: 'compact' } });
    expect(document.documentElement.getAttribute('data-theme')).toBe('high-contrast');
    expect(document.documentElement.getAttribute('data-accent')).toBe('cyan');
    expect(document.documentElement.getAttribute('data-density')).toBe('compact');
  });
});
