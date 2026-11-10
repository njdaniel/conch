// What works on this desktop (issue #1593): each row's states, against fakes
// of the held answers. Nothing is probed, so nothing here runs a program.
import { describe, expect, it } from 'vitest';

import type { DesktopSettingsView, IntegrationId, IntegrationRow, NativeVoiceView, StorageStatus } from '../../../web/src/platform/desktop/bridge.ts';
import { integrationsView, type IntegrationsDeps } from '../../main/integrations.ts';

const settings = (over: Partial<DesktopSettingsView> = {}): DesktopSettingsView => ({ closeToTray: false, launchAtLogin: false, trayWorking: false, trayDetected: true, trayConfirmed: false, launchAvailable: true, ...over });
const voice = (over: Partial<NativeVoiceView> = {}): NativeVoiceView => ({ status: 'idle', generation: 0, channel: null, retryable: false, ...over });
const helper = (ptt: 'unknown' | 'ready' | 'not_configured' | 'unavailable' | 'lost'): NativeVoiceView =>
  voice({
    status: 'running',
    helper: { version: 2, phase: 'connected', capture: 'ready', transmitting: false, muted: false, deafened: false, blocked: [], ptt } as unknown as NativeVoiceView['helper'],
  });

const base: IntegrationsDeps = {
  settings: () => Promise.resolve(settings()),
  notificationSupported: () => true,
  lockObservation: () => 'available',
  share: () => 'available',
  storage: () => ({ kind: 'protected' }),
  voice: () => voice(),
};

async function row(id: IntegrationId, over: Partial<IntegrationsDeps>): Promise<IntegrationRow> {
  const view = await integrationsView({ ...base, ...over });
  const found = view.rows.find((r) => r.id === id);
  if (found === undefined) throw new Error(`no row ${id}`);
  return found;
}

const storageOf = (s: StorageStatus): Partial<IntegrationsDeps> => ({ storage: () => s });

describe('integrationsView', () => {
  it('lists every integration once, in a fixed order', async () => {
    const view = await integrationsView(base);
    expect(view.rows.map((r) => r.id)).toEqual(['tray', 'notifications', 'lock', 'capture', 'storage', 'launch', 'shareSound', 'voiceHelper', 'talkKey']);
  });

  it.each<[string, IntegrationId, Partial<IntegrationsDeps>, IntegrationRow]>([
    ['tray working', 'tray', { settings: () => Promise.resolve(settings({ trayWorking: true, closeToTray: true, trayConfirmed: true })) }, { id: 'tray', state: 'working' }],
    ['tray: no host', 'tray', { settings: () => Promise.resolve(settings({ trayDetected: false })) }, { id: 'tray', state: 'unavailable', why: 'no-tray-host' }],
    ['tray: host, not chosen', 'tray', {}, { id: 'tray', state: 'unknown', why: 'tray-off' }],
    ['tray: chosen, not confirmed', 'tray', { settings: () => Promise.resolve(settings({ closeToTray: true })) }, { id: 'tray', state: 'unknown', why: 'tray-unconfirmed' }],
    ['notifications supported', 'notifications', {}, { id: 'notifications', state: 'working' }],
    ['notifications unsupported', 'notifications', { notificationSupported: () => false }, { id: 'notifications', state: 'unavailable', why: 'notifications-unsupported' }],
    ['lock observed', 'lock', {}, { id: 'lock', state: 'working' }],
    ['lock not observed', 'lock', { lockObservation: () => 'unavailable' }, { id: 'lock', state: 'unavailable', why: 'lock-unobserved' }],
    ['capture available', 'capture', {}, { id: 'capture', state: 'working' }],
    ['capture: no portal', 'capture', { share: () => 'no-portal' }, { id: 'capture', state: 'unavailable', why: 'no-portal' }],
    ['capture: not Wayland', 'capture', { share: () => 'not-wayland' }, { id: 'capture', state: 'unavailable', why: 'not-wayland' }],
    ['capture: not asked yet', 'capture', { share: () => 'unknown' }, { id: 'capture', state: 'unknown', why: 'portal-unasked' }],
    ['storage protected', 'storage', {}, { id: 'storage', state: 'working' }],
    ['storage: no secret store', 'storage', storageOf({ kind: 'session-only', reason: 'unavailable' }), { id: 'storage', state: 'unavailable', why: 'secret-store-missing' }],
    ['storage: insecure', 'storage', storageOf({ kind: 'session-only', reason: 'insecure-backend' }), { id: 'storage', state: 'unavailable', why: 'insecure-backend' }],
    ['storage: unknown backend', 'storage', storageOf({ kind: 'session-only', reason: 'unknown-backend' }), { id: 'storage', state: 'unavailable', why: 'unknown-backend' }],
    ['storage: unsafe root', 'storage', storageOf({ kind: 'session-only', reason: 'unsafe-root' }), { id: 'storage', state: 'unavailable', why: 'unsafe-root' }],
    ['storage: store silent', 'storage', storageOf({ kind: 'unavailable' }), { id: 'storage', state: 'unknown', why: 'secret-store-silent' }],
    ['launch available', 'launch', {}, { id: 'launch', state: 'working' }],
    ['launch: unpackaged', 'launch', { settings: () => Promise.resolve(settings({ launchAvailable: false, launchUnavailable: 'unpackaged' })) }, { id: 'launch', state: 'unavailable', why: 'unpackaged' }],
    ['launch: unsafe location', 'launch', { settings: () => Promise.resolve(settings({ launchAvailable: false, launchUnavailable: 'unsafe-location' })) }, { id: 'launch', state: 'unavailable', why: 'unsafe-location' }],
    ['share sound is silent', 'shareSound', {}, { id: 'shareSound', state: 'unavailable', why: 'share-silent' }],
    ['helper running', 'voiceHelper', { voice: () => helper('ready') }, { id: 'voiceHelper', state: 'working' }],
    ['helper starting', 'voiceHelper', { voice: () => voice({ status: 'starting' }) }, { id: 'voiceHelper', state: 'working' }],
    ['helper not installed', 'voiceHelper', { voice: () => voice({ status: 'unavailable', problem: 'not-installed' }) }, { id: 'voiceHelper', state: 'unavailable', why: 'not-installed' }],
    ['helper untrusted', 'voiceHelper', { voice: () => voice({ status: 'unavailable', problem: 'untrusted' }) }, { id: 'voiceHelper', state: 'unavailable', why: 'untrusted' }],
    ['helper not started', 'voiceHelper', {}, { id: 'voiceHelper', state: 'unknown', why: 'helper-not-started' }],
    ['helper unavailable for another reason', 'voiceHelper', { voice: () => voice({ status: 'unavailable', problem: 'not-signed-in' }) }, { id: 'voiceHelper', state: 'unknown', why: 'helper-not-started' }],
    ['talk key ready', 'talkKey', { voice: () => helper('ready') }, { id: 'talkKey', state: 'working' }],
    ['talk key not configured', 'talkKey', { voice: () => helper('not_configured') }, { id: 'talkKey', state: 'unavailable', why: 'key-not-configured' }],
    ['talk key unavailable', 'talkKey', { voice: () => helper('unavailable') }, { id: 'talkKey', state: 'unavailable', why: 'key-unavailable' }],
    ['talk key lost', 'talkKey', { voice: () => helper('lost') }, { id: 'talkKey', state: 'unavailable', why: 'key-unavailable' }],
    ['talk key, helper not running', 'talkKey', {}, { id: 'talkKey', state: 'unknown', why: 'helper-not-started' }],
    ['talk key, helper unsure', 'talkKey', { voice: () => helper('unknown') }, { id: 'talkKey', state: 'unknown', why: 'helper-not-started' }],
  ])('%s', async (_name, id, over, want) => {
    expect(await row(id, over)).toEqual(want);
  });

  it('carries no path, device name, address, token or user name in any row', async () => {
    const leaky = voice({ status: 'unavailable', problem: '/usr/bin/conch-voice token abc', channel: 'general' });
    const view = await integrationsView({ ...base, voice: () => leaky });
    const text = JSON.stringify(view);
    expect(text).not.toMatch(/\/usr|token|abc|general|https?:/);
    for (const r of view.rows) expect(Object.keys(r).every((k) => k === 'id' || k === 'state' || k === 'why')).toBe(true);
  });
});
