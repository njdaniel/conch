// The desktop page's one clipboard write: plain text, with the page's own API.
// Main permits it for the owned window right after the person's own click or
// key (issue #947). It lives here, apart from index.ts, so that the desktop
// screens can copy without importing the adapter that imports them.
export function copyText(text: string): Promise<void> {
  return navigator.clipboard.writeText(text);
}
