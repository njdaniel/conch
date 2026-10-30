// The one place the desktop adapter reads window.conchDesktop, the object the
// preload exposes (./bridge types it). It is absent outside the desktop shell.
import type { ConchDesktopBridge } from './bridge';

export function bridge(): ConchDesktopBridge | undefined {
  return window.conchDesktop;
}
