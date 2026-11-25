// Setup transitions can recover stranded focus or move it within their own
// controls. An active control elsewhere keeps focus.
export function focusMayMove(setupRoot: HTMLElement | null): boolean {
  const active = document.activeElement;
  return active === null || active === document.body
    || setupRoot?.contains(active) === true || active.closest('[inert]') !== null;
}
