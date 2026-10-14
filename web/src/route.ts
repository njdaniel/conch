// Hash routing by hand: #/c/<channel> names the open channel. The hash is per
// tab and survives a reload, and a link to it carries no credential.

export function channelFromHash(hash: string): string | null {
  const match = /^#\/c\/(.+)$/.exec(hash);
  if (match?.[1] === undefined) return null;
  try {
    return decodeURIComponent(match[1]);
  } catch {
    return null;
  }
}

export function hashForChannel(name: string): string {
  return `#/c/${encodeURIComponent(name)}`;
}

// The channel last open in this tab, for a load whose hash names none.
const lastKey = 'conch.web.lastChannel';

export function rememberedChannel(): string | null {
  try {
    return window.sessionStorage.getItem(lastKey);
  } catch {
    return null;
  }
}

export function rememberChannel(name: string): void {
  try {
    window.sessionStorage.setItem(lastKey, name);
  } catch {
    // Not remembered; the first channel is chosen next time.
  }
}
