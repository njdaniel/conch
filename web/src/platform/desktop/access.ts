// The one place the desktop adapter reads window.conchDesktop, the object the
// preload exposes (./bridge types it). It is absent outside the desktop shell.
import type { ConchDesktopBridge, DesktopInviteBridge, DesktopSettingsBridge } from './bridge';

export function bridge(): ConchDesktopBridge | undefined {
  return window.conchDesktop;
}

// The bridge with its invite family (issue #1591), which a fake need not have.
export function inviteBridge(): (ConchDesktopBridge & Partial<DesktopInviteBridge>) | undefined {
  return window.conchDesktop;
}

// The app's own settings (issue #1587): present when the preload exposes them.
export function settingsBridge(): Partial<DesktopSettingsBridge> | undefined {
  return window.conchDesktop;
}
