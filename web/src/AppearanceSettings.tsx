import {
  appearanceAccents,
  appearanceDensities,
  appearanceThemes,
  type AppearanceAccent,
  type AppearanceDensity,
  type AppearancePreferences,
  type AppearanceTheme,
} from './appearance';

const themeNames: Record<AppearanceTheme, string> = {
  system: 'System',
  light: 'Light',
  dark: 'Dark',
  'high-contrast': 'High contrast',
};
const accentNames: Record<AppearanceAccent, string> = {
  green: 'Green',
  blue: 'Blue',
  violet: 'Violet',
  pink: 'Pink',
  orange: 'Orange',
  cyan: 'Cyan',
};
const densityNames: Record<AppearanceDensity, string> = { comfortable: 'Comfortable', compact: 'Compact' };

export function AppearanceSettings({ value, onChange }: {
  readonly value: AppearancePreferences;
  readonly onChange: (value: AppearancePreferences) => void;
}) {
  return (
    <section aria-label="Appearance" className="drawer-section appearance-settings">
      <h3>Appearance</h3>
      <label>
        Theme preset
        <select
          value={value.theme}
          onChange={(event) => {
            onChange({ ...value, theme: event.target.value as AppearanceTheme });
          }}
        >
          {appearanceThemes.map((theme) => <option key={theme} value={theme}>{themeNames[theme]}</option>)}
        </select>
      </label>
      <p className="hint">System follows this device’s light or dark preference.</p>
      <label>
        Accent color
        <select
          value={value.accent}
          onChange={(event) => {
            onChange({ ...value, accent: event.target.value as AppearanceAccent });
          }}
        >
          {appearanceAccents.map((accent) => <option key={accent} value={accent}>{accentNames[accent]}</option>)}
        </select>
      </label>
      <label>
        Chat density
        <select
          value={value.density}
          onChange={(event) => {
            onChange({ ...value, density: event.target.value as AppearanceDensity });
          }}
        >
          {appearanceDensities.map((density) => <option key={density} value={density}>{densityNames[density]}</option>)}
        </select>
      </label>
    </section>
  );
}
