// The "Desktop app" section (issue #1587): the app-wide settings main keeps,
// not per account. Main does every change and may refuse it; this only asks
// and shows what main answers. Closing keeps the app running only while a
// working tray exists, so that choice is disabled, with the reason, until
// then. Launch at login is off by default and is the person's own choice.
import { useEffect, useId, useState } from 'react';

import { settingsBridge } from '../access';
import type { DesktopSettingsView, SettingsChange } from '../bridge';
import { settingsRefusalLines, settingsText } from './text';

export function DesktopSettings() {
  const [view, setView] = useState<DesktopSettingsView | null>(null);
  const [line, setLine] = useState('');
  const [busy, setBusy] = useState(false);
  const id = useId();

  useEffect(() => {
    let live = true;
    void settingsBridge()
      ?.settings?.()
      .then(
        (next) => {
          if (live) setView(next);
        },
        () => undefined,
      );
    return () => {
      live = false;
    };
  }, []);

  async function change(next: SettingsChange): Promise<void> {
    const b = settingsBridge();
    if (b?.settingsSet === undefined || busy) return;
    setBusy(true);
    setLine('');
    try {
      const result = await b.settingsSet(next);
      if (result.ok) setView(result.view);
      else setLine(settingsRefusalLines[result.reason]);
    } catch {
      setLine(settingsText.failed);
    } finally {
      setBusy(false);
    }
  }

  if (view === null) return null;
  const keepRunning = view.closeToTray && view.trayWorking;

  return (
    <section className="desktop-settings" aria-labelledby={`${id}-h`}>
      <h2 id={`${id}-h`}>{settingsText.heading}</h2>
      <fieldset disabled={busy}>
        <legend>{settingsText.closeLegend}</legend>
        <label>
          <input
            type="radio"
            name={`${id}-close`}
            checked={!keepRunning}
            onChange={() => {
              void change({ closeToTray: false });
            }}
          />{' '}
          {settingsText.closeQuit}
        </label>
        <label>
          <input
            type="radio"
            name={`${id}-close`}
            checked={keepRunning}
            disabled={!view.trayWorking}
            aria-describedby={view.trayWorking ? undefined : `${id}-tray`}
            onChange={() => {
              void change({ closeToTray: true });
            }}
          />{' '}
          {settingsText.closeTray}
        </label>
        {view.trayWorking ? null : (
          <p id={`${id}-tray`} className="desktop-settings-note">
            {settingsText.noTray}
          </p>
        )}
      </fieldset>
      <label>
        <input
          type="checkbox"
          checked={view.launchAtLogin}
          disabled={busy || (!view.launchAvailable && !view.launchAtLogin)}
          aria-describedby={view.launchAvailable ? undefined : `${id}-launch`}
          onChange={(event) => {
            void change({ launchAtLogin: event.currentTarget.checked });
          }}
        />{' '}
        {settingsText.launch}
      </label>
      {view.launchAvailable ? null : (
        <p id={`${id}-launch`} className="desktop-settings-note">
          {view.launchUnavailable === 'unsafe-location' ? settingsText.launchUnsafe : settingsText.launchUnavailable}
        </p>
      )}
      {line === '' ? null : <p role="alert">{line}</p>}
    </section>
  );
}
