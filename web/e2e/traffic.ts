// Helpers for A14's search of captured traffic (issue #1069). No browser, no
// network: unit-tested in traffic.test.ts.

// Random secrets and tickets can contain a group's name by chance (a ticket
// ending in "boblB0g" is not bob). They carry no names, so they are blanked
// before a name is searched for. This is the one list of them:
//  - the string value of a JSON key that holds one (ticket, token,
//    livekit_token, secret, and any key ending in one of those words);
//  - any conch_... token form (conch_wst_..., and the like), wherever it is.
const secretKey = /("[A-Za-z0-9_]*(?:ticket|token|secret)"\s*:\s*")(?:[^"\\]|\\.)*(")/gi;
const tokenForm = /conch_[A-Za-z0-9_-]+/g;

// scrubSecrets returns text with random secret values blanked. Names in
// ordinary fields and sentences are left alone.
export function scrubSecrets(text: string): string {
  return text.replace(secretKey, '$1$2').replace(tokenForm, 'conch_');
}

// findLeak returns a failure message with about 40 characters either side of
// the first place marker appears in text once secrets are blanked, or
// undefined when it does not appear.
export function findLeak(text: string, marker: string): string | undefined {
  const scrubbed = scrubSecrets(text);
  const at = scrubbed.indexOf(marker);
  if (at < 0) return undefined;
  const context = scrubbed.slice(Math.max(0, at - 40), at + marker.length + 40).replace(/\s+/g, ' ');
  return `"${marker}" at ...${context}...`;
}
