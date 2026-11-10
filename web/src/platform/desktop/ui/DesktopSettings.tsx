// The "Desktop app" section (issue #1587): the app-wide settings main keeps,
// not per account. Main does every change and may refuse it; this only asks
// and shows what main answers. Closing keeps the app running only while a
// working tray exists: the choice is disabled, with the reason, while no tray
// host is on the desktop, and once made it waits for the person to say they
// can see the icon (Q-D5-2, #1589). Launch at login is off by default and is
// the person's own choice.
import { useEffect, useId, useState } from 'react';

import { settingsBridge } from '../access';
import { copyText } from '../clipboard';
import type { DesktopSettingsView, IntegrationsView, SettingsChange } from '../bridge';
import { integrationNames, integrationSentence, integrationsPlainText, integrationsText, integrationStateWords, settingsRefusalLines, settingsText } from './text';

// What works on this desktop (issue #1593): main's held states as a list, and
// a Copy button for a problem report. Shown only when main answers; a row is
// text, never markup.
function Integrations() {
  const [view, setView] = useState<IntegrationsView | null>(null);
  const [copied, setCopied] = useState<'' | 'yes' | 'no'>('');
  const id = useId();

  useEffect(() => {
    let live = true;
    void settingsBridge()
      ?.integrations?.()
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

  if (view === null || view.rows.length === 0) return null;

  async function copy(rows: IntegrationsView): Promise<void> {
    try {
      await copyText(integrationsPlainText(rows));
      setCopied('yes');
    } catch {
      setCopied('no');
    }
  }

  return (
    <section className="desktop-integrations" aria-labelledby={`${id}-h`}>
      <h3 id={`${id}-h`}>{integrationsText.heading}</h3>
      <ul>
        {view.rows.map((row) => (
          <li key={row.id}>
            <strong>{integrationNames[row.id]}</strong>: {integrationStateWords[row.state]}. {integrationSentence(row)}
          </li>
        ))}
      </ul>
      <button
        type="button"
        onClick={() => {
          void copy(view);
        }}
      >
        {integrationsText.copy}
      </button>
      <p role="status">{copied === 'yes' ? integrationsText.copied : copied === 'no' ? integrationsText.copyFailed : ''}</p>
    </section>
  );
}

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
  const asking = view.closeToTray && !view.trayConfirmed;

  return (
    <section className="desktop-settings" aria-labelledby={`${id}-h`}>
      <h2 id={`${id}-h`}>{settingsText.heading}</h2>
      <fieldset disabled={busy}>
        <legend>{settingsText.closeLegend}</legend>
        <label>
          <input
            type="radio"
            name={`${id}-close`}
            checked={!view.closeToTray}
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
            checked={view.closeToTray}
            disabled={!view.trayDetected && !view.closeToTray}
            aria-describedby={view.trayDetected || view.closeToTray ? undefined : `${id}-tray`}
            onChange={() => {
              void change({ closeToTray: true });
            }}
          />{' '}
          {settingsText.closeTray}
        </label>
        {view.trayDetected || view.closeToTray ? null : (
          <p id={`${id}-tray`} className="desktop-settings-note">
            {settingsText.noTray}
          </p>
        )}
        {asking ? (
          <div role="group" aria-label={settingsText.trayAsk} className="desktop-settings-note">
            <p>{settingsText.trayAsk}</p>
            <button
              type="button"
              onClick={() => {
                void change({ trayConfirmed: true });
              }}
            >
              {settingsText.trayYes}
            </button>{' '}
            <button
              type="button"
              onClick={() => {
                void change({ closeToTray: false });
              }}
            >
              {settingsText.trayNo}
            </button>
          </div>
        ) : null}
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
      <Integrations />
    </section>
  );
}
