// What works on this desktop (issue #1593, D5-07 of #773; UX-23).
//
// Input: the states the integrations already keep (the settings view's tray
// and launch answers, the notification support check, the lock observation,
// the portal's last answer, the credential vault's classification and the
// voice helper's confirmed state). Output: one row per integration, in a
// fixed order, each a state and a fixed code for why. Owns: nothing. It
// reads; it opens no device, starts no capture, shows no portal dialog and
// runs nothing of its own, and no row carries a path, device name, address,
// token or user name. The words are the page's (ui/text.ts).
import type { DesktopSettingsView, IntegrationRow, IntegrationsView, NativeVoiceView, StorageStatus } from '../../web/src/platform/desktop/bridge.ts';
import type { LockObservation } from './lifecycle.ts';
import type { ShareAvailability } from './portal.ts';

export interface IntegrationsDeps {
  readonly settings: () => Promise<DesktopSettingsView>;
  readonly notificationSupported: () => boolean;
  readonly lockObservation: () => LockObservation;
  // The portal's latest answer, 'unknown' before the first (Portal.last).
  readonly share: () => ShareAvailability | 'unknown';
  readonly storage: () => StorageStatus;
  readonly voice: () => NativeVoiceView;
}

const working = (id: IntegrationRow['id']): IntegrationRow => ({ id, state: 'working' });

function tray(view: DesktopSettingsView): IntegrationRow {
  if (view.trayWorking) return working('tray');
  if (!view.trayDetected) return { id: 'tray', state: 'unavailable', why: 'no-tray-host' };
  // A host is there, but the icon exists only after the person chose it and
  // said they can see it: main cannot tell more than that.
  return { id: 'tray', state: 'unknown', why: view.closeToTray ? 'tray-unconfirmed' : 'tray-off' };
}

function capture(answer: ShareAvailability | 'unknown'): IntegrationRow {
  switch (answer) {
    case 'available':
      return working('capture');
    case 'no-portal':
      return { id: 'capture', state: 'unavailable', why: 'no-portal' };
    case 'not-wayland':
      return { id: 'capture', state: 'unavailable', why: 'not-wayland' };
    case 'unknown':
      return { id: 'capture', state: 'unknown', why: 'portal-unasked' };
  }
}

function storage(status: StorageStatus): IntegrationRow {
  if (status.kind === 'protected') return working('storage');
  if (status.kind === 'unavailable') return { id: 'storage', state: 'unknown', why: 'secret-store-silent' };
  const why = { unavailable: 'secret-store-missing', 'insecure-backend': 'insecure-backend', 'unknown-backend': 'unknown-backend', 'unsafe-root': 'unsafe-root' } as const;
  return { id: 'storage', state: 'unavailable', why: why[status.reason] };
}

function voiceHelper(view: NativeVoiceView): IntegrationRow {
  if (view.status === 'unavailable' && (view.problem === 'not-installed' || view.problem === 'untrusted')) return { id: 'voiceHelper', state: 'unavailable', why: view.problem };
  if (view.status === 'running' || view.status === 'starting') return working('voiceHelper');
  // Not asked yet (or stopped since): nothing is started just to find out.
  return { id: 'voiceHelper', state: 'unknown', why: 'helper-not-started' };
}

function talkKey(view: NativeVoiceView): IntegrationRow {
  const ptt = view.status === 'running' ? view.helper?.ptt : undefined;
  if (ptt === 'ready') return working('talkKey');
  if (ptt === 'not_configured') return { id: 'talkKey', state: 'unavailable', why: 'key-not-configured' };
  if (ptt === 'unavailable' || ptt === 'lost') return { id: 'talkKey', state: 'unavailable', why: 'key-unavailable' };
  return { id: 'talkKey', state: 'unknown', why: 'helper-not-started' };
}

export async function integrationsView(deps: IntegrationsDeps): Promise<IntegrationsView> {
  const settings = await deps.settings();
  const voice = deps.voice();
  const rows: IntegrationRow[] = [
    tray(settings),
    deps.notificationSupported() ? working('notifications') : { id: 'notifications', state: 'unavailable', why: 'notifications-unsupported' },
    deps.lockObservation() === 'available' ? working('lock') : { id: 'lock', state: 'unavailable', why: 'lock-unobserved' },
    capture(deps.share()),
    storage(deps.storage()),
    settings.launchAvailable ? working('launch') : { id: 'launch', state: 'unavailable', why: settings.launchUnavailable === 'unsafe-location' ? 'unsafe-location' : 'unpackaged' },
    // Nothing plays a sound when a share starts or ends on Linux until
    // #1585 and #1586; the row is a fixed answer, not a probe.
    { id: 'shareSound', state: 'unavailable', why: 'share-silent' },
    voiceHelper(voice),
    talkKey(voice),
  ];
  return { rows };
}
