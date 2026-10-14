// A run's link is written by an agent. It becomes a link only when it parses
// and its scheme is exactly https; anything else (a javascript: or data: URL,
// plain http, something that is not a URL at all) stays text. The server
// refuses the same, but the page does not rely on it.
export function safeLink(link: string | undefined): string | null {
  if (link === undefined || link === '') return null;
  let url: URL;
  try {
    url = new URL(link);
  } catch {
    return null;
  }
  // Userinfo in front of the host is how a link is made to read as another
  // site's.
  if (url.protocol !== 'https:' || url.username !== '' || url.password !== '') return null;
  return url.href;
}
