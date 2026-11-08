export const appearanceThemes = ['system', 'light', 'dark', 'high-contrast'] as const;
export const appearanceAccents = ['green', 'blue', 'violet', 'pink', 'orange', 'cyan'] as const;
export const appearanceDensities = ['comfortable', 'compact'] as const;

export type AppearanceTheme = (typeof appearanceThemes)[number];
export type AppearanceAccent = (typeof appearanceAccents)[number];
export type AppearanceDensity = (typeof appearanceDensities)[number];

export interface AppearancePreferences {
  readonly theme: AppearanceTheme;
  readonly accent: AppearanceAccent;
  readonly density: AppearanceDensity;
}

// Dark is the default, as Conch has always looked: a light system setting
// does not change anyone's view until they choose System or Light.
export const defaultAppearance: AppearancePreferences = { theme: 'dark', accent: 'green', density: 'comfortable' };

export function applyAppearance(preferences: AppearancePreferences, root: HTMLElement = document.documentElement): void {
  if (preferences.theme === 'system') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', preferences.theme);
  root.setAttribute('data-accent', preferences.accent);
  root.setAttribute('data-density', preferences.density);
}
